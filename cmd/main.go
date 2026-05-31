package main

import (
	"context"
	"errors"
	"net/http"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/duynhne/order-service/config"
	database "github.com/duynhne/order-service/internal/core"
	"github.com/duynhne/order-service/internal/core/repository"
	logicv1 "github.com/duynhne/order-service/internal/logic/v1"
	v1 "github.com/duynhne/order-service/internal/web/v1"
	"github.com/duynhne/order-service/middleware"
	"github.com/duynhne/pkg/grpcx"
)

func main() {
	cfg := config.Load()
	if err := cfg.Validate(); err != nil {
		panic("Configuration validation failed: " + err.Error())
	}

	logger, err := middleware.NewLogger()
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer func() { _ = logger.Sync() }()

	logger.Info("Service starting",
		zap.String("service", cfg.Service.Name),
		zap.String("version", cfg.Service.Version),
		zap.String("env", cfg.Service.Env),
		zap.String("port", cfg.Service.Port),
	)

	tp := initTracing(cfg, logger)

	initProfiling(cfg, logger)

	pool, err := database.Connect(context.Background())
	if err != nil {
		logger.Error("Failed to connect to database", zap.Error(err))
		return
	}
	defer pool.Close()
	logger.Info("Database connection pool established")

	orderRepo := repository.NewPostgresOrderRepository(pool)
	txManager := repository.NewPostgresTransactionManager(pool)
	orderService := logicv1.NewOrderService(orderRepo, txManager)
	v1.SetOrderService(orderService)

	authClient := middleware.NewAuthClient(cfg.AuthServiceURL)
	logger.Info("Auth client initialized", zap.String("auth_service_url", cfg.AuthServiceURL))

	// Shipping client: gRPC when SHIPPING_GRPC_ADDR is set (Phase 1 pilot),
	// otherwise the REST client. Both return identical aggregated responses,
	// so this is a one-env-var rollback to REST.
	if cfg.ShippingGRPCAddr != "" {
		conn, dialErr := grpcx.Dial(cfg.ShippingGRPCAddr)
		if dialErr != nil {
			logger.Error("Failed to dial shipping gRPC", zap.String("addr", cfg.ShippingGRPCAddr), zap.Error(dialErr))
			return
		}
		defer func() { _ = conn.Close() }()
		v1.SetShippingClient(v1.NewShippingGRPCClient(conn))
		logger.Info("Shipping client: gRPC", zap.String("addr", cfg.ShippingGRPCAddr))
	} else {
		v1.SetShippingClient(v1.NewShippingClient(cfg.ShippingServiceURL))
		logger.Info("Shipping client: REST", zap.String("url", cfg.ShippingServiceURL))
	}

	cartClient := v1.NewCartClient(cfg.CartServiceURL)
	v1.SetCartClient(cartClient)

	var isShuttingDown atomic.Bool
	srv := setupServer(cfg, logger, authClient, &isShuttingDown)
	runGracefulShutdown(cfg, srv, tp, pool, logger, &isShuttingDown)
}

func initTracing(cfg *config.Config, logger *zap.Logger) interface{ Shutdown(context.Context) error } {
	if !cfg.Tracing.Enabled {
		logger.Info("Tracing disabled (TRACING_ENABLED=false)")
		return nil
	}
	tp, err := middleware.InitTracing(cfg)
	if err != nil {
		logger.Warn("Failed to initialize tracing", zap.Error(err))
		return nil
	}
	logger.Info("Tracing initialized",
		zap.String("endpoint", cfg.Tracing.Endpoint),
		zap.Float64("sample_rate", cfg.Tracing.SampleRate),
	)
	return tp
}

func initProfiling(cfg *config.Config, logger *zap.Logger) {
	if !cfg.Profiling.Enabled {
		logger.Info("Profiling disabled (PROFILING_ENABLED=false)")
		return
	}
	if err := middleware.InitProfiling(); err != nil {
		logger.Warn("Failed to initialize profiling", zap.Error(err))
		return
	}
	logger.Info("Profiling initialized", zap.String("endpoint", cfg.Profiling.Endpoint))
}

func setupServer(cfg *config.Config, logger *zap.Logger, authClient *middleware.AuthClient, isShuttingDown *atomic.Bool) *http.Server {
	r := gin.Default()

	r.Use(middleware.TracingMiddleware())
	r.Use(middleware.LoggingMiddleware(logger))
	r.Use(middleware.PrometheusMiddleware())

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})
	r.GET("/ready", func(c *gin.Context) {
		if isShuttingDown.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "shutting_down"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Order v1 routes — all private (JWT required). Variant A edge naming.
	privateOrders := r.Group("/order/v1/private")
	privateOrders.Use(middleware.AuthMiddleware(authClient, logger, cfg.AuthAllowUnauthenticatedFallback))
	{
		privateOrders.GET("/orders", v1.ListOrders)
		privateOrders.GET("/orders/:id", v1.GetOrder)
		privateOrders.GET("/orders/:id/details", v1.GetOrderDetails)
		privateOrders.POST("/orders", v1.CreateOrder)
	}

	return &http.Server{
		Addr:              ":" + cfg.Service.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

func runGracefulShutdown(
	cfg *config.Config,
	srv *http.Server,
	tp interface{ Shutdown(context.Context) error },
	pool interface{ Close() },
	logger *zap.Logger,
	isShuttingDown *atomic.Bool,
) {
	go func() {
		logger.Info("Starting order service", zap.String("port", cfg.Service.Port))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("Failed to start server", zap.Error(err))
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	<-ctx.Done()
	logger.Info("Shutdown signal received")

	isShuttingDown.Store(true)
	drainDelay := cfg.GetReadinessDrainDelayDuration()
	if drainDelay > 0 {
		logger.Info("Readiness drain delay started", zap.Duration("delay", drainDelay))
		time.Sleep(drainDelay)
	}

	shutdownTimeout := cfg.GetShutdownTimeoutDuration()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	logger.Info("Shutting down server...", zap.Duration("timeout", shutdownTimeout))

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", zap.Error(err))
	} else {
		logger.Info("HTTP server shutdown complete")
	}

	pool.Close()
	logger.Info("Database pool closed")

	if tp != nil {
		if err := tp.Shutdown(shutdownCtx); err != nil {
			logger.Error("Tracer shutdown error", zap.Error(err))
		} else {
			logger.Info("Tracer shutdown complete")
		}
	}

	middleware.StopProfiling()
	logger.Info("Graceful shutdown complete")
}
