// Package reconcile repairs orders whose inventory reservation did not reach the
// state their terminal status implies (RFC-0021 P3).
//
// The saga is durable, so this is not a substitute for it. It exists for the
// narrow set of outcomes Temporal cannot fix by itself:
//
//   - CommitInventory gave up after its elapsed bound, so a confirmed order's
//     stock is still merely RESERVED — reserved forever, because v1 reservations
//     never expire on their own.
//   - A run was terminated or timed out between the pivot and the commit, so no
//     workflow will resume it.
//   - A pre-pivot compensation exhausted its retries, leaving a failed order
//     holding stock.
//
// It works entirely through the inventory API — no cross-database reads — and
// every repair it issues is idempotent, so a repair racing the saga's own retry
// is a no-op rather than a double movement.
package reconcile

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// Defaults.
const (
	// DefaultInterval is how often a pass runs. A minute is far below the time a
	// human would take to notice, and far above the cost of the scan.
	DefaultInterval = time.Minute

	// DefaultWindow bounds how far back a pass looks. Anything older than this
	// has been wrong long enough that it is an incident with a human on it, not
	// something to keep re-attempting on a timer.
	DefaultWindow = 24 * time.Hour

	// DefaultSettleDelay keeps the pass away from orders that JUST reached a
	// terminal status. Without it the reconciler would race the saga's own
	// commit — harmless, because the repairs are idempotent, but it would report
	// repairs for work the saga was already doing and make the backlog gauge
	// meaningless.
	DefaultSettleDelay = 5 * time.Minute

	// DefaultBatch caps one pass. Each candidate costs one GetReservation, so
	// this bounds the RPC burst against inventory.
	DefaultBatch = 200

	// perCandidateBudget bounds the RPCs for one candidate so a slow inventory
	// cannot stall the whole pass.
	perCandidateBudget = 10 * time.Second
)

// Repair actions — bounded metric labels.
const (
	ActionCommitted = "committed" // a confirmed order's reservation was committed
	ActionReleased  = "released"  // a failed order's reservation was released
	ActionBreach    = "breach"    // inconsistent in a way a repair cannot fix
	ActionFailed    = "failed"    // the repair itself failed; the next pass retries
)

// reasonOrderFailed is the release reason recorded in inventory's movement
// ledger. Part of the same bounded server-side vocabulary as the saga's
// SAGA_* codes.
const reasonOrderFailed = "RECONCILER_ORDER_FAILED"

// Reconciler scans terminal orders and repairs reservations that disagree.
type Reconciler struct {
	orders    domain.OrderReconcileLister
	inventory inventoryv1.InventoryServiceClient
	log       *zap.Logger

	interval    time.Duration
	window      time.Duration
	settleDelay time.Duration
	batch       int

	// backlog holds the number of inconsistencies the LAST pass could not
	// resolve. An observable gauge reads it; see RegisterGauge.
	backlog atomic.Int64

	now func() time.Time
}

// New builds a reconciler with the package defaults.
func New(orders domain.OrderReconcileLister, inventory inventoryv1.InventoryServiceClient, log *zap.Logger) *Reconciler {
	return &Reconciler{
		orders:      orders,
		inventory:   inventory,
		log:         log,
		interval:    DefaultInterval,
		window:      DefaultWindow,
		settleDelay: DefaultSettleDelay,
		batch:       DefaultBatch,
	}
}

// Run passes until ctx is cancelled. A failing pass is logged and retried on the
// next tick: stopping the only thing that repairs stranded stock is worse than a
// noisy log.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	r.log.Info("inventory reconciler running",
		zap.Duration("interval", r.interval), zap.Duration("window", r.window))

	for {
		select {
		case <-ctx.Done():
			r.log.Info("inventory reconciler stopped")
			return
		case <-ticker.C:
			if err := r.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.log.Error("reconciler pass failed", zap.Error(err))
			}
		}
	}
}

// Pass runs one scan. Exported so a test — and an operator, via a one-shot —
// can drive exactly one deterministically.
func (r *Reconciler) Pass(ctx context.Context) error {
	to := r.timeNow().Add(-r.settleDelay)
	from := to.Add(-r.window)

	candidates, err := r.orders.ListForReconcile(ctx, from, to, r.batch)
	if err != nil {
		return err
	}

	var unresolved int64
	for _, c := range candidates {
		candCtx, cancel := context.WithTimeout(ctx, perCandidateBudget)
		action, resolved := r.reconcileOne(candCtx, c)
		cancel()
		if action != "" {
			recordRepair(ctx, action)
		}
		if !resolved {
			unresolved++
		}
	}
	// Whatever this pass could not fix is the backlog. Set rather than added, so
	// a repaired backlog goes back to zero instead of decaying.
	r.backlog.Store(unresolved)
	return nil
}

// reconcileOne inspects one order's reservation and repairs it if needed. It
// returns the action taken (empty when the pair was already consistent) and
// whether the order is now consistent.
func (r *Reconciler) reconcileOne(ctx context.Context, c domain.ReconcileCandidate) (string, bool) {
	resp, err := r.inventory.GetReservation(ctx, &inventoryv1.GetReservationRequest{
		ReservationId: c.OrderID,
	})
	if err != nil {
		// NOT_FOUND is the normal answer for a product-path order: it never had
		// an inventory reservation, so there is nothing to reconcile. Treating it
		// as an error would make every pre-cutover order look inconsistent.
		if status.Code(err) == codes.NotFound || grpcx.Reason(err) == grpcx.ReasonNotFound {
			return "", true
		}
		r.log.Warn("reconciler could not read a reservation; the next pass retries",
			zap.String("order_id", c.OrderID), zap.Error(err))
		return "", false
	}

	reservationStatus := resp.GetReservation().GetStatus()
	switch reservationStatus {
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED:
		return r.repairReserved(ctx, c)

	case inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED:
		// Consistent for a confirmed order. For a FAILED order it is an invariant
		// breach: stock was consumed for an order that did not happen. A release
		// cannot fix it (COMMITTED is terminal), so this is reported, not
		// repaired.
		if c.Status == statusFailed {
			r.log.Error("failed order has COMMITTED stock; inventory consumed units for an order that did not happen",
				zap.String("order_id", c.OrderID))
			return ActionBreach, false
		}
		return "", true

	case inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED,
		inventoryv1.ReservationStatus_RESERVATION_STATUS_EXPIRED:
		// Consistent for a failed order. For a CONFIRMED one the stock went back
		// while the customer was charged — again terminal, so it is reported.
		if c.Status == statusConfirmed {
			r.log.Error("confirmed order has released stock; the customer was charged for units that went back",
				zap.String("order_id", c.OrderID), zap.String("reservation_status", reservationStatus.String()))
			return ActionBreach, false
		}
		return "", true

	case inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED:
		// A server this build does not understand. Not something to guess at.
		r.log.Error("unrecognised reservation status; leaving it for a human",
			zap.String("order_id", c.OrderID))
		return ActionBreach, false
	}

	// Unreachable while the switch above is exhaustive.
	return ActionBreach, false
}

// repairReserved drives a still-RESERVED reservation to the state the order's
// terminal status requires.
func (r *Reconciler) repairReserved(ctx context.Context, c domain.ReconcileCandidate) (string, bool) {
	switch c.Status {
	case statusConfirmed:
		// The saga's own CommitInventory did not settle. Commit is idempotent, so
		// racing a late saga retry is a no-op.
		if _, err := r.inventory.Commit(ctx, &inventoryv1.CommitRequest{ReservationId: c.OrderID}); err != nil {
			r.log.Error("reconciler could not commit a confirmed order's reservation",
				zap.String("order_id", c.OrderID), zap.Error(err))
			return ActionFailed, false
		}
		r.log.Info("reconciler committed a confirmed order's reservation",
			zap.String("order_id", c.OrderID))
		return ActionCommitted, true

	case statusFailed:
		// Stock held against an order that will never ship.
		if _, err := r.inventory.Release(ctx, &inventoryv1.ReleaseRequest{
			ReservationId: c.OrderID,
			Reason:        reasonOrderFailed,
		}); err != nil {
			r.log.Error("reconciler could not release a failed order's reservation",
				zap.String("order_id", c.OrderID), zap.Error(err))
			return ActionFailed, false
		}
		r.log.Info("reconciler released a failed order's reservation",
			zap.String("order_id", c.OrderID))
		return ActionReleased, true
	}

	// The lister only returns terminal statuses, so this means the query and this
	// switch have drifted apart.
	r.log.Error("reconciler asked to repair a non-terminal order; the candidate query and the repair logic disagree",
		zap.String("order_id", c.OrderID), zap.String("order_status", c.Status))
	return ActionBreach, false
}

// Order statuses this package acts on. Kept local so the package does not import
// the saga to read two strings.
const (
	statusConfirmed = "confirmed"
	statusFailed    = "failed"
)

func (r *Reconciler) timeNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}
