package saga

import (
	"log/slog"

	"go.temporal.io/sdk/workflow"

	"github.com/duynhlab/pkg/logger/slogx"
	"github.com/duynhlab/pkg/temporalx"

	"github.com/duynhlab/order-service/internal/core/domain"
)

// Catalog events decided in workflow code (RFC-0031 § Event catalog). They go
// through temporalx.WorkflowEvent — the SDK's replay-aware logger — so a
// replayed history never writes them twice. Activity-side events use the
// facade's Event instead (see applyOrderCommandOnce).

// compensationDone records one finished compensation step on its metric and
// writes order.compensation.completed, with error.type when it failed.
func compensationDone(ctx workflow.Context, orderID, step string, err error) {
	result := compResult(err)
	recordCompensation(ctx, step, result)
	attrs := []slog.Attr{
		slog.String("order.id", orderID),
		slog.String("compensation.step", step),
		slog.String("outcome", result),
	}
	level := slog.LevelInfo
	if err != nil {
		level = slog.LevelWarn
		attrs = append(attrs, slog.String("error.type", slogx.ErrorType(err)))
	}
	temporalx.WorkflowEvent(ctx, level, "order.compensation.completed", "compensation step finished", attrs...)
}

// sagaFailed writes order.failed once the terminal fail command has landed;
// outcome is failed or compensated (the path the saga took).
func sagaFailed(ctx workflow.Context, orderID string, reason domain.ReasonCode, outcome string) {
	temporalx.WorkflowEvent(ctx, slog.LevelWarn, "order.failed", "order failed",
		slog.String("order.id", orderID), slog.String("reason", string(reason)), slog.String("outcome", outcome))
}

// retryExhausted writes order.retry.exhausted for a bounded retry in workflow
// code that gave up. The attempt count is not visible to workflow code (the
// activity error does not carry it), so the attribute is left out here; the
// dispatchers, which own their counters, include it.
func retryExhausted(ctx workflow.Context, orderID, operation string, err error) {
	temporalx.WorkflowEvent(ctx, slog.LevelError, "order.retry.exhausted", "retry exhausted",
		slog.String("order.id", orderID), slog.String("operation", operation),
		slog.String("error.type", slogx.ErrorType(err)))
}
