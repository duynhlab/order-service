package config

import (
	"testing"
)

// clearEnv unsets every env var Load reads, so a test starts from a known state.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"SERVICE_NAME", "PORT", "VERSION", "ENV",
		"TRACING_ENABLED", "OTEL_COLLECTOR_ENDPOINT", "OTEL_SAMPLE_RATE",
		"PROFILING_ENABLED", "PYROSCOPE_ENDPOINT",
		"LOG_LEVEL", "LOG_FORMAT",
		"METRICS_ENABLED", "METRICS_PATH",
		"DB_HOST", "DB_PORT", "DB_NAME", "DB_USER", "DB_PASSWORD", "DB_SSLMODE",
		"DB_POOL_MAX_CONNECTIONS", "DB_POOL_MODE", "DB_POOLER_TYPE",
		"SHUTDOWN_TIMEOUT", "READINESS_DRAIN_DELAY",
		"SHIPPING_GRPC_ADDR", "CART_SERVICE_URL",
		"NOTIFICATION_GRPC_ADDR", "PRODUCT_GRPC_ADDR",
		"TEMPORAL_HOSTPORT", "TEMPORAL_NAMESPACE", "TASK_QUEUE",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	c := Load()
	if c.Service.Port != "8080" {
		t.Errorf("Port = %q, want 8080", c.Service.Port)
	}
	if c.Service.Name != defaultServiceName {
		t.Errorf("Name = %q, want %q", c.Service.Name, defaultServiceName)
	}
	if !c.Tracing.Enabled {
		t.Error("Tracing.Enabled = false, want true")
	}
	if c.Tracing.SampleRate != 0.1 {
		t.Errorf("SampleRate = %v, want 0.1", c.Tracing.SampleRate)
	}
	if c.ShutdownTimeout != 10 {
		t.Errorf("ShutdownTimeout = %d, want 10", c.ShutdownTimeout)
	}
	if c.ReadinessDrainDelay != 5 {
		t.Errorf("ReadinessDrainDelay = %d, want 5", c.ReadinessDrainDelay)
	}
	if c.Temporal.TaskQueue != "order-fulfillment" {
		t.Errorf("Temporal.TaskQueue = %q, want order-fulfillment", c.Temporal.TaskQueue)
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("SERVICE_NAME", "order")
	t.Setenv("PORT", "9000")
	t.Setenv("ENV", "production")
	t.Setenv("TRACING_ENABLED", "false")
	t.Setenv("OTEL_SAMPLE_RATE", "0.5")
	t.Setenv("METRICS_ENABLED", "no")
	t.Setenv("SHUTDOWN_TIMEOUT", "20s")
	t.Setenv("READINESS_DRAIN_DELAY", "999s") // over max(30) -> default 5
	c := Load()
	if c.Service.Name != "order" || c.Service.Port != "9000" || c.Service.Env != "production" {
		t.Errorf("service = %+v", c.Service)
	}
	if c.Tracing.Enabled {
		t.Error("Tracing.Enabled = true, want false")
	}
	if c.Tracing.SampleRate != 0.5 {
		t.Errorf("tracing = %+v", c.Tracing)
	}
	if c.Metrics.Enabled {
		t.Error("Metrics.Enabled = true, want false")
	}
	if c.ShutdownTimeout != 20 {
		t.Errorf("ShutdownTimeout = %d, want 20", c.ShutdownTimeout)
	}
	if c.ReadinessDrainDelay != 5 {
		t.Errorf("ReadinessDrainDelay = %d, want 5 (over-max falls back)", c.ReadinessDrainDelay)
	}
}

func validConfig() *Config {
	c := &Config{}
	c.Service.Name = "order"
	c.Service.Port = "8080"
	c.Service.Env = "production"
	c.Tracing.Enabled = true
	c.Tracing.Endpoint = "otel:4318"
	c.Tracing.SampleRate = 0.1
	c.Tracing.ServiceName = "order"
	c.Profiling.Enabled = true
	c.Profiling.Endpoint = "pyro:4040"
	c.Profiling.ServiceName = "order"
	c.Logging.Level = "info"
	c.Logging.Format = "json"
	return c
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"missing service name", func(c *Config) { c.Service.Name = "" }, true},
		{"default service name", func(c *Config) { c.Service.Name = defaultServiceName }, true},
		{"empty port", func(c *Config) { c.Service.Port = "" }, true},
		{"non-numeric port", func(c *Config) { c.Service.Port = "abc" }, true},
		{"bad env", func(c *Config) { c.Service.Env = "qa" }, true},
		{"tracing endpoint empty", func(c *Config) { c.Tracing.Endpoint = "" }, true},
		{"sample rate too high", func(c *Config) { c.Tracing.SampleRate = 2 }, true},
		{"tracing disabled skips checks", func(c *Config) { c.Tracing.Enabled = false; c.Tracing.Endpoint = "" }, false},
		{"profiling endpoint empty", func(c *Config) { c.Profiling.Endpoint = "" }, true},
		{"bad log level", func(c *Config) { c.Logging.Level = "trace" }, true},
		{"bad log format", func(c *Config) { c.Logging.Format = "xml" }, true},
		{"db host set, name missing", func(c *Config) { c.Database.Host = "h"; c.Database.User = "u"; c.Database.Password = "p" }, true},
		{"db host set, complete", func(c *Config) {
			c.Database.Host = "h"
			c.Database.Name = "n"
			c.Database.User = "u"
			c.Database.Password = "p"
			c.Database.Port = "5432"
		}, false},
		{"db bad port", func(c *Config) {
			c.Database.Host = "h"
			c.Database.Name = "n"
			c.Database.User = "u"
			c.Database.Password = "p"
			c.Database.Port = "abc"
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildDSN(t *testing.T) {
	db := &DatabaseConfig{Host: "localhost", Port: "5432", Name: "order", User: "order", Password: "secret", SSLMode: "disable"}
	got := db.BuildDSN()
	want := "postgresql://order:secret@localhost:5432/order?sslmode=disable"
	if got != want {
		t.Errorf("BuildDSN() = %q, want %q", got, want)
	}
}

func TestEnvHelpers(t *testing.T) {
	clearEnv(t)
	if getEnv("PORT", "x") != "x" {
		t.Error("getEnv default failed")
	}
	t.Setenv("PORT", "1")
	if getEnv("PORT", "x") != "1" {
		t.Error("getEnv value failed")
	}
	t.Setenv("TRACING_ENABLED", "yes")
	if !getEnvBool("TRACING_ENABLED", false) {
		t.Error("getEnvBool yes failed")
	}
	if getEnvInt("DB_POOL_MAX_CONNECTIONS", 7) != 7 {
		t.Error("getEnvInt default failed")
	}
	t.Setenv("DB_POOL_MAX_CONNECTIONS", "bad")
	if getEnvInt("DB_POOL_MAX_CONNECTIONS", 7) != 7 {
		t.Error("getEnvInt bad-value fallback failed")
	}
	t.Setenv("OTEL_SAMPLE_RATE", "bad")
	if getEnvFloat("OTEL_SAMPLE_RATE", 0.2) != 0.2 {
		t.Error("getEnvFloat bad-value fallback failed")
	}
	// getEnvDurationSeconds / ...WithMax are generic helpers. Exercise them with
	// VARIED keys and defaults (not the production constants) so every branch is
	// covered and both params are genuinely tested — which also keeps unparam
	// quiet (it flags params that always receive the same value).
	if getEnvDurationSeconds("DUR_UNSET", 10) != 10 {
		t.Error("getEnvDurationSeconds: unset should return default")
	}
	t.Setenv("DUR_VALID", "20s")
	if getEnvDurationSeconds("DUR_VALID", 7) != 20 {
		t.Error("getEnvDurationSeconds: valid value should parse to seconds")
	}
	t.Setenv("DUR_BAD", "bad")
	if getEnvDurationSeconds("DUR_BAD", 5) != 5 {
		t.Error("getEnvDurationSeconds: invalid value should return default")
	}
	t.Setenv("DUR_OVERMAX", "999s") // > 60s cap
	if getEnvDurationSeconds("DUR_OVERMAX", 3) != 3 {
		t.Error("getEnvDurationSeconds: over-max should return default")
	}
	// ...WithMax: varied keys/defaults/max so all three params are exercised.
	if getEnvDurationSecondsWithMax("DURMAX_UNSET", 5, 30) != 5 {
		t.Error("getEnvDurationSecondsWithMax: unset should return default")
	}
	t.Setenv("DURMAX_VALID", "12s")
	if getEnvDurationSecondsWithMax("DURMAX_VALID", 4, 20) != 12 {
		t.Error("getEnvDurationSecondsWithMax: valid value should parse")
	}
	t.Setenv("DURMAX_OVER", "25s") // > max 15 -> default
	if getEnvDurationSecondsWithMax("DURMAX_OVER", 6, 15) != 6 {
		t.Error("getEnvDurationSecondsWithMax: over-max should return default")
	}
}

func TestEnvironmentPredicatesAndDurations(t *testing.T) {
	c := &Config{}
	c.Service.Env = "dev"
	if !c.IsDevelopment() || c.IsProduction() {
		t.Error("dev predicates wrong")
	}
	c.Service.Env = "prod"
	if c.IsDevelopment() || !c.IsProduction() {
		t.Error("prod predicates wrong")
	}
	c.ShutdownTimeout = 15
	if c.GetShutdownTimeoutDuration().Seconds() != 15 {
		t.Error("GetShutdownTimeoutDuration wrong")
	}
	c.ReadinessDrainDelay = 5
	if c.GetReadinessDrainDelayDuration().Seconds() != 5 {
		t.Error("GetReadinessDrainDelayDuration wrong")
	}
}
