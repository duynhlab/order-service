package saga

import (
	"context"
	"errors"
	"fmt"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"time"

	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// Inventory-participant activities (RFC-0021 phase 3, ADR-030). These are NEW
// activity names — only the v1 workflow branch calls them (branch selection per
// the ADR-030 versioning mechanism); the Product-participant activities above
// are never repointed, so in-flight histories keep the exact old call graph.
//
// Idempotency rides the inventory.v1 contract: reservation_id = order ID, the
// server computes the canonical request hash itself (an empty client hash is
// accepted), replays return the original result, and Release/Commit are
// replay-idempotent per the reservation FSM (ADR-028).

// Release reason codes passed to inventory.v1/Release — bounded tokens
// (^[A-Za-z0-9_.-]{0,64}$ server-side); the workflow passes the failing step's
// code so the reservation ledger records WHY the hold was returned.
//
// A distinct type, not a bare string: the server-side regex is the last line of
// defence, and the obvious next change — threading a failing step's error text
// into the ledger for debuggability — would otherwise compile silently and put
// free-form text into the movement ledger.
type ReleaseReason string

const (
	ReleaseReasonShipmentFailed ReleaseReason = "SAGA_SHIPMENT_FAILED"
	ReleaseReasonCaptureFailed  ReleaseReason = "SAGA_CAPTURE_FAILED"
	ReleaseReasonConfirmFailed  ReleaseReason = "SAGA_CONFIRM_FAILED"
	// ReleaseReasonReserveFailed covers an AMBIGUOUS reserve: the activity
	// exhausted its retries without a definite "nothing was taken" verdict, so
	// the reservation may exist server-side (a committed reserve whose response
	// was lost). v1 reservations never auto-expire — expires_at is
	// observability-only and there is no reaper — so not releasing would hold
	// that stock forever against an order that failed.
	ReleaseReasonReserveFailed ReleaseReason = "SAGA_RESERVE_FAILED"
)

// classifyInventoryErr converts an inventory.v1 error into the saga's retry
// vocabulary. Business rejections (INSUFFICIENT_STOCK, IDEMPOTENCY_CONFLICT,
// INVALID_TRANSITION, ... — grpcx's businessReasons map) become non-retryable
// ApplicationErrors whose TYPE is the reason token, so workflow tests, metrics,
// and Temporal UI can discriminate without parsing messages. Everything else
// stays retryable and MUST be paired with a bounded RetryPolicy by the caller
// (the workflow's per-class ActivityOptions).
func classifyInventoryErr(op, orderID string, err error) error {
	if err == nil {
		return nil
	}
	// A canceled RPC is a shutdown/rollout race (worker stopping mid-call),
	// never a business verdict — it must stay retryable or a routine pod
	// restart would permanently fail the mandatory-forward CommitInventory.
	// (grpcx's code fallback doesn't retry Canceled; the payment path handles
	// this the same way via its rejection whitelist's default branch.)
	if status.Code(err) == codes.Canceled {
		return fmt.Errorf("%s for order %s: %w", op, orderID, err)
	}
	if !grpcx.Retryable(err) {
		reason := grpcx.Reason(err)
		if reason == "" {
			// No ErrorInfo on the wire (version skew / non-platform error):
			// fall back to the gRPC code so the type stays bounded.
			reason = status.Code(err).String()
		}
		return temporal.NewNonRetryableApplicationError(op+" rejected", reason, err)
	}
	return fmt.Errorf("%s for order %s: %w", op, orderID, err)
}

// ensureInventoryClient guards against a nil client the same way the payment
// path does (defense-in-depth despite the unconditional worker dial): fail
// fast with a typed retryable error instead of panicking the activity.
func (a *Activities) ensureInventoryClient() error {
	if a.Inventory == nil {
		return errors.New("inventory client not configured")
	}
	return nil
}

func toReservationItems(items []ReserveItem) []*inventoryv1.ReservationItem {
	out := make([]*inventoryv1.ReservationItem, 0, len(items))
	for _, it := range items {
		out = append(out, &inventoryv1.ReservationItem{
			SkuId:    it.ProductID, // sku_id = product_id (RFC-0021 initial identity)
			Quantity: int64(it.Quantity),
		})
	}
	return out
}

// ReserveInventory places an all-or-nothing hold on stock in inventory-service
// (v1-branch step 1; idempotent by reservation_id = order ID). Emits the same
// order_stock_reservation_total metric the product path used before RFC-0021 P4, so the
// saga dashboards keep working across the migration.
func (a *Activities) ReserveInventory(ctx context.Context, orderID string, items []ReserveItem) error {
	if err := a.ensureInventoryClient(); err != nil {
		return err
	}
	_, err := a.Inventory.Reserve(ctx, &inventoryv1.ReserveRequest{
		ReservationId: orderID,
		OrderId:       orderID,
		Items:         toReservationItems(items),
		// DestinationRegion empty: single default warehouse (ADR-028).
		// RequestHash empty: the server computes the canonical hash itself.
	})
	if err != nil {
		// Metric result mirrors the payment path's vocabulary: insufficient
		// (the expected business outcome) / rejected (other terminal business
		// rejections — deterministic, fire once) / error (transient, re-driven
		// per retry attempt). Keeps the transient-error alert signal clean.
		classified := classifyInventoryErr("reserve inventory", orderID, err)
		switch {
		case grpcx.Reason(err) == grpcx.ReasonInsufficientStock:
			recordStockReservation(ctx, resultInsufficient)
		case isNonRetryableApp(classified):
			recordStockReservation(ctx, resultRejected)
		default:
			recordStockReservation(ctx, resultError)
		}
		return classified
	}
	recordStockReservation(ctx, resultReserved)
	return nil
}

// isNonRetryableApp reports whether classifyInventoryErr produced a terminal
// business rejection (used only to pick the metric bucket).
func isNonRetryableApp(err error) bool {
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.NonRetryable()
}

// ReleaseInventory returns the order's hold (compensation for
// ReserveInventory). Releasing an already-released reservation is a server-side
// no-op success; releasing a COMMITTED one is INVALID_TRANSITION — a
// non-retryable invariant breach (a pre-pivot release raced a commit), never a
// retry storm.
func (a *Activities) ReleaseInventory(ctx context.Context, orderID string, reason ReleaseReason) error {
	if err := a.ensureInventoryClient(); err != nil {
		return err
	}
	if _, err := a.Inventory.Release(ctx, &inventoryv1.ReleaseRequest{
		ReservationId: orderID,
		Reason:        string(reason),
	}); err != nil {
		return classifyInventoryErr("release inventory", orderID, err)
	}
	return nil
}

// CommitInventory converts the order's reservation into a sale — the post-pivot
// MANDATORY FORWARD step (ADR-027/028): a confirmed order must converge to
// COMMITTED, so the v1 workflow branch retries this without an attempt bound.
// Committing a COMMITTED reservation returns it unchanged (replay-idempotent);
// INVALID_TRANSITION (released) or a missing reservation after a confirmed
// order is an invariant breach, surfaced non-retryably so the alert +
// reconciler own it instead of an infinite retry masking it.
func (a *Activities) CommitInventory(ctx context.Context, orderID string) error {
	// Liveness for the mandatory-forward step: the Commit RPC blocks, so a
	// ticker goroutine heartbeats while it runs (and through the GameDay pause
	// below). Paired with commitActivityOptions' 10s HeartbeatTimeout: a live
	// worker — even a slow one — keeps the attempt alive; a killed worker
	// stops heartbeating and the server re-issues within ~10s instead of
	// waiting out the 30s StartToClose. The SDK throttles actual sends, so
	// the 3s tick costs nothing.
	hbDone := make(chan struct{})
	defer close(hbDone)
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbDone:
				return
			case <-t.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()

	if err := a.ensureInventoryClient(); err != nil {
		return err
	}
	if _, err := a.Inventory.Commit(ctx, &inventoryv1.CommitRequest{
		ReservationId: orderID,
	}); err != nil {
		return classifyInventoryErr("commit inventory", orderID, err)
	}
	// GameDay fault hook: the commit is now durable server-side but Temporal
	// has not recorded the activity result. Killing the worker inside this
	// window is the G2b interleaving; on replay Commit runs again and must be
	// a no-op (committing COMMITTED returns it unchanged). ctx-aware so a
	// worker shutdown does not hang on the pause itself.
	if a.CommitPause > 0 {
		select {
		case <-time.After(a.CommitPause):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
