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
//   - An ORPHANED hold from Release-before-Reserve: a compensation ran before
//     its Reserve landed, so Release found no row and returned success, and the
//     Reserve then created a hold nothing was watching. inventory-service
//     delegates this seam here explicitly — see the comment on its Release
//     repository method, which names "the order-domain reconciler (RFC-0021
//     P3-5)" as the owner. A failed order with a RESERVED hold is exactly what
//     this scans for, so the seam is covered by the same branch.
//
// It never repairs while the workflow is still open. That is the load-bearing
// guard, not the settle delay: the order row records the saga's LAST DURABLE
// WRITE, which is not the same as its intent. ConfirmOrder can commit
// status=confirmed and then lose its ack — the workflow sees a failure, takes
// the compensation branch, and starts releasing — while the database says
// confirmed. A reconciler trusting the row would issue Commit into that,
// consuming units for an order that is being refunded, and then report the very
// breach it caused. Inventory's row lock does not help: it serializes
// transitions, it does not adjudicate between a commit and a release that
// disagree about direction.
//
// So the question asked first is "is anyone still working on this order?", and
// only a closed workflow makes the order row the final word.
//
// It works entirely through the inventory API and Temporal's describe — no
// cross-database reads.
//
// It reads a reservation's STATUS and acts on that alone, which is sound because
// inventory applies every allocation line AND flips the status inside one
// transaction: RESERVED means no line moved, COMMITTED means all of them did.
// There is no partial state to reconcile, and a multi-warehouse reservation is no
// different from a single-line one.
//
// Racing the saga's own retry is safe, and not merely by convention: inventory's
// transition helper takes `SELECT ... FOR UPDATE` on the reservation header
// before deciding, which SERIALIZES concurrent transitions, and then
// short-circuits on the current status — a second Commit of a COMMITTED
// reservation returns success without touching balances, and likewise for
// Release. The movement ledger insert is keyed by a deterministic command id, so
// even that cannot double-write. Verified in
// inventory-service/internal/core/repository/reservations.go, not assumed from
// the proto comment.
package reconcile

import (
	"context"
	"errors"
	"sync"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/saga"
	"github.com/duynhlab/pkg/grpcx"
	inventoryv1 "github.com/duynhlab/pkg/proto/inventory/v1"
)

// Describer reports the state of an order's fulfillment workflow. Narrow on
// purpose: the reconciler only ever asks whether someone is still working on the
// order. *client.Client satisfies it.
type Describer interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

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

	// DefaultBatch caps one pass. A candidate costs up to three RPCs — one
	// DescribeWorkflowExecution against Temporal, one GetReservation, and possibly
	// one Commit/Release against inventory — so this bounds the burst against BOTH
	// services.
	//
	// Starvation by permanent breaches is handled in the QUERY, not here: an
	// unrepairable breach keeps its row unsettled forever, so an oldest-first scan
	// would park DefaultBatch of them at the head of every pass and never reach
	// newer, repairable work. ListForReconcile therefore sorts orders with a
	// recorded breach code LAST. A full batch is still reported — see Pass.
	DefaultBatch = 200

	// perCandidateBudget bounds the RPCs for one candidate so a slow inventory
	// cannot stall the whole pass.
	perCandidateBudget = 10 * time.Second

	// markBudget bounds the bookkeeping write that follows a candidate. It gets
	// its own budget from the PARENT context on purpose: perCandidateBudget is
	// already spent (and its context cancelled) by the time the outcome is known,
	// and reusing it would make every settle silently fail with a cancelled
	// context — an order would be repaired over and over, forever.
	markBudget = 5 * time.Second

	// DefaultPassBudget bounds a whole pass. Passes cannot overlap — Run calls Pass
	// synchronously and a Ticker drops ticks — so this is not about concurrency.
	// It bounds two other things: how STALE the candidate list may get before it is
	// re-read (batch × perCandidateBudget is over half an hour against a slow
	// inventory, by which point the outcomes being repaired are ancient), and how
	// long shutdown can be waiting for a pass to end. A truncated pass is fine: the
	// next one continues where this stopped, because settled orders leave the scan.
	//
	// It is deliberately LONGER than DefaultInterval — a pass that needs more than
	// a minute should finish rather than be chopped every minute — which means a
	// budget-hitting pass is immediately followed by the next tick.
	DefaultPassBudget = 5 * time.Minute
)

// Repair actions — bounded metric labels.
const (
	ActionCommitted = "committed" // a confirmed order's reservation was committed
	ActionReleased  = "released"  // a failed order's reservation was released
	ActionBreach    = "breach"    // inconsistent in a way a repair cannot fix
	ActionFailed    = "failed"    // the repair itself failed; the next pass retries
	ActionDeferred  = "deferred"  // the saga is still running; not ours to touch
	// ActionUnreadable: the order's state could not be READ (inventory or Temporal
	// unreachable). Counted rather than only logged, because otherwise a
	// permanently unreadable order produces 1,440 Warn lines a day and nothing an
	// operator can alert on — the same noise pattern the once-per-order reporting
	// below exists to remove.
	ActionUnreadable = "unreadable"
)

// Breach reasons — bounded tokens persisted in reconcile_breach_code so the
// TABLE says WHY, not just THAT. Log retention is shorter than an unresolved
// breach's life, so a single log line is not a durable record.
const (
	BreachReservationMissing = "RESERVATION_MISSING" // confirmed inventory-path order with no reservation
	BreachStockConsumed      = "STOCK_CONSUMED"      // failed order whose stock is COMMITTED
	BreachStockReturned      = "STOCK_RETURNED"      // confirmed order whose stock went back
	BreachForeignReservation = "FOREIGN_RESERVATION" // the reservation belongs to another order
	BreachUnknownStatus      = "UNKNOWN_RES_STATUS"  // a reservation status this build does not know
	BreachNonTerminalOrder   = "NON_TERMINAL_ORDER"  // the scan and the repair logic disagree
)

// reasonOrderFailed is the release reason recorded in inventory's movement
// ledger. Part of the same bounded server-side vocabulary as the saga's
// SAGA_* codes.
const reasonOrderFailed = "RECONCILER_ORDER_FAILED"

// Reconciler scans terminal orders and repairs reservations that disagree.
type Reconciler struct {
	store     domain.ReconcileStore
	inventory inventoryv1.InventoryServiceClient
	workflows Describer
	log       *zap.Logger

	interval    time.Duration
	window      time.Duration
	settleDelay time.Duration
	batch       int
	passBudget  time.Duration

	// stop asks Run to finish the current pass and return WITHOUT cancelling it.
	// Separate from context cancellation on purpose: cancelling aborts an in-flight
	// Commit/Release, which reports a failed repair from a process that is merely
	// shutting down.
	stop     chan struct{}
	stopOnce sync.Once
}

// New builds a reconciler with the package defaults.
func New(store domain.ReconcileStore, inventory inventoryv1.InventoryServiceClient,
	workflows Describer, log *zap.Logger) *Reconciler {
	return &Reconciler{
		store:       store,
		inventory:   inventory,
		workflows:   workflows,
		log:         log,
		interval:    DefaultInterval,
		window:      DefaultWindow,
		settleDelay: DefaultSettleDelay,
		batch:       DefaultBatch,
		passBudget:  DefaultPassBudget,
		stop:        make(chan struct{}),
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
		case <-r.stop:
			r.log.Info("inventory reconciler stopped after finishing its pass")
			return
		case <-ticker.C:
			if err := r.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
				r.log.Error("reconciler pass failed", zap.Error(err))
			}
		}
	}
}

// Stop asks Run to return once the pass it is in has finished. Idempotent, and
// safe to call from any goroutine. It does NOT cancel work in flight — the caller
// cancels the context afterwards as a backstop.
func (r *Reconciler) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
}

// Pass runs one scan. Exported so a test — and an operator, via a one-shot —
// can drive exactly one deterministically.
func (r *Reconciler) Pass(ctx context.Context) error {
	// The bookkeeping context is derived from the UN-budgeted parent. Deriving it
	// from the pass budget would cap it: a repair that lands in the last second of
	// a pass would get a near-dead context, fail to settle, and be re-repaired (and
	// re-logged) next pass — turning the deadline into a repeating-work generator.
	parent := ctx

	ctx, cancelPass := context.WithTimeout(ctx, r.passBudget)
	defer cancelPass()

	candidates, err := r.store.ListForReconcile(ctx, r.settleDelay, r.window, r.batch)
	if err != nil {
		return err
	}

	// A full batch means the scan was truncated. Unlike before, this is now a
	// real signal rather than the steady state: the scan returns only UNSETTLED
	// orders, so on a healthy platform it is nearly empty and a full batch means
	// something is genuinely wrong at volume.
	//
	// Counted as well as logged — a log line cannot be alerted on, and truncation
	// is the one condition under which the backlog gauge is a floor rather than a
	// count.
	if len(candidates) == r.batch {
		recordTruncated(ctx)
		r.log.Warn("reconciler pass hit its batch cap; more unsettled orders remain in the window",
			zap.Int("batch", r.batch))
	}

	for i, c := range candidates {
		if ctx.Err() != nil {
			// The pass budget ran out. Stopping is not a failure: unsettled orders
			// stay in the scan, so the next tick resumes with them.
			r.log.Warn("reconciler pass ran out of budget; the next pass continues",
				zap.Int("examined", i), zap.Int("candidates", len(candidates)))
			return nil
		}

		candCtx, cancel := context.WithTimeout(ctx, perCandidateBudget)
		action, breach, settled := r.reconcileOne(candCtx, c)
		cancel()

		markCtx, cancelMark := context.WithTimeout(parent, markBudget)
		switch {
		case settled:
			if err := r.store.MarkReconciled(markCtx, c.OrderID); err != nil {
				// Not settling the row is safe: the next pass re-examines it, and
				// every repair is idempotent. It only costs a repeated check.
				r.log.Warn("could not mark an order reconciled; the next pass re-checks it",
					zap.String("order_id", c.OrderID), zap.Error(err))
			}
		case action == ActionBreach && c.BreachCode == "":
			// Left UNSETTLED on purpose — it is still inconsistent, so it belongs
			// in the backlog until a human resolves it. Recording the REASON (not
			// merely that there was one) means the table answers "what is wrong with
			// this order" after the log line has aged out, the next pass can stay
			// quiet about it, and the scan can put fresh work ahead of it.
			if err := r.store.MarkReconcileBreach(markCtx, c.OrderID, breach); err != nil {
				r.log.Warn("could not record a reconcile breach",
					zap.String("order_id", c.OrderID), zap.String("breach", breach), zap.Error(err))
			}
		}
		cancelMark()

		// Report an action once per ORDER, not once per pass. A single stuck order
		// otherwise contributes 1,440 counter increments and 1,440 error lines a
		// day, which makes a permanent breach indistinguishable from a stream of
		// fresh saga failures.
		if action != "" && (action != ActionBreach || c.BreachCode == "") {
			recordRepair(ctx, action)
		}
	}
	return nil
}

// reconcileOne inspects one order's reservation and repairs it if needed. It
// returns the action taken (empty when the pair was already consistent) and
// whether the order is now consistent.
func (r *Reconciler) reconcileOne(ctx context.Context, c domain.ReconcileCandidate) (string, string, bool) {
	// Asked FIRST, for EVERY branch — not just the repairable one.
	//
	// The order row records the saga's last durable write, not its intent (see the
	// package doc), and that cuts both ways. A confirmed order whose stock reads
	// RELEASED looks like a terminal breach, but it is the NORMAL mid-flight state
	// of the lost-ConfirmOrder-ack scenario: the compensation has already released
	// and failOrder has not landed yet. Declaring a breach there would file a hard
	// error against a saga that makes the pair consistent seconds later — and,
	// because the thing that broke ConfirmOrder is usually the database, the breach
	// write would fail too, so it would be re-logged every minute during exactly
	// the incident this reporting was built to be trustworthy in.
	//
	// Settling has the mirror problem: nothing ever resets reconciled_at, so an
	// order settled while its saga was still running is invisible forever, whatever
	// happens to it next.
	//
	// A running saga owns its own reservation, full stop. The cost is one Describe
	// per candidate — paid once per order, because a settled order leaves the scan.
	open, err := r.sagaStillRunning(ctx, c.OrderID)
	if err != nil {
		if c.BreachCode == "" {
			r.log.Warn("reconciler could not determine whether the saga is still running; deferring",
				zap.String("order_id", c.OrderID), zap.Error(err))
		}
		return ActionUnreadable, "", false
	}
	if open {
		return ActionDeferred, "", false
	}

	resp, err := r.inventory.GetReservation(ctx, &inventoryv1.GetReservationRequest{
		ReservationId: c.OrderID,
	})
	if err != nil {
		if status.Code(err) == codes.NotFound || grpcx.Reason(err) == grpcx.ReasonNotFound {
			return r.judgeMissingReservation(c)
		}
		if c.BreachCode == "" {
			r.log.Warn("reconciler could not read a reservation; the next pass retries",
				zap.String("order_id", c.OrderID), zap.Error(err))
		}
		return ActionUnreadable, "", false
	}

	// Verify the reservation is the one this order owns before acting on it. The
	// id scheme (reservation_id == order id) is a convention, and the field is
	// already on the wire, so checking it costs nothing and removes a whole class
	// of "moved a stranger's stock" from the design.
	if got := resp.GetReservation().GetOrderId(); got != "" && got != c.OrderID {
		if c.BreachCode == "" {
			r.log.Error("reservation belongs to a different order; refusing to touch it",
				zap.String("order_id", c.OrderID), zap.String("reservation_order_id", got))
		}
		return ActionBreach, BreachForeignReservation, false
	}

	reservationStatus := resp.GetReservation().GetStatus()
	r.reportParticipantDisagreement(ctx, c, resp.GetReservation())
	switch reservationStatus {
	case inventoryv1.ReservationStatus_RESERVATION_STATUS_RESERVED:
		return r.repairReserved(ctx, c)

	case inventoryv1.ReservationStatus_RESERVATION_STATUS_COMMITTED:
		// Consistent for a confirmed order. For a FAILED order it is an invariant
		// breach: stock was consumed for an order that did not happen. A release
		// cannot fix it (COMMITTED is terminal), so this is reported, not
		// repaired.
		if c.Status == statusFailed {
			if c.BreachCode == "" {
				r.log.Error("failed order has COMMITTED stock; inventory consumed units for an order that did not happen",
					zap.String("order_id", c.OrderID))
			}
			return ActionBreach, BreachStockConsumed, false
		}
		return "", "", true

	case inventoryv1.ReservationStatus_RESERVATION_STATUS_RELEASED,
		inventoryv1.ReservationStatus_RESERVATION_STATUS_EXPIRED:
		// EXPIRED is grouped with RELEASED because both mean the units went back,
		// and the repair (or the breach) is the same either way. Nothing produces
		// it today — v1 reservations have no expiry sweeper, which is precisely why
		// a stranded RESERVED hold needs this reconciler at all — so it is handled
		// for the version that adds one rather than being reachable now. If that
		// version also starts expiring holds out from under running sagas, this
		// case stops being purely a breach and needs revisiting.
		//
		// Consistent for a failed order. For a CONFIRMED one the stock went back
		// while the order stands — terminal, so it is reported. The workflow-open
		// guard above is what makes this safe to call a breach: mid-compensation
		// this pair is normal, and it is only final once nobody is working on it.
		if c.Status == statusConfirmed {
			if c.BreachCode == "" {
				// Deliberately does not claim the customer was charged: in the
				// stale-status variant the compensation refunded them and only
				// failOrder did not land, so asserting a charge would send on-call
				// hunting for one that does not exist.
				r.log.Error("order is confirmed but its stock went back; the order row and inventory disagree terminally",
					zap.String("order_id", c.OrderID), zap.String("reservation_status", reservationStatus.String()))
			}
			return ActionBreach, BreachStockReturned, false
		}
		return "", "", true

	case inventoryv1.ReservationStatus_RESERVATION_STATUS_UNSPECIFIED:
		// A server this build does not understand. Not something to guess at.
		if c.BreachCode == "" {
			r.log.Error("unrecognised reservation status; leaving it for a human",
				zap.String("order_id", c.OrderID))
		}
		return ActionBreach, BreachUnknownStatus, false
	}

	// Reachable the moment inventory adds a ReservationStatus value: proto enums
	// are open, so an unknown number lands here rather than in any case above.
	// Logged as loudly as UNSPECIFIED — a silent breach with nothing to explain it
	// is the worst possible way to learn the two services have drifted.
	if c.BreachCode == "" {
		r.log.Error("reservation status is outside every value this build knows; leaving it for a human",
			zap.String("order_id", c.OrderID), zap.Int32("reservation_status", int32(reservationStatus)))
	}
	return ActionBreach, BreachUnknownStatus, false
}

// reportParticipantDisagreement notes an order that holds a reservation while its
// row does not say inventory-path.
//
// The repair still happens (the caller carries on): the hold is real, inventory's
// v1 reservations never expire, and refusing to act on one would strand stock over
// a disagreement about bookkeeping. But it must not happen SILENTLY. These two
// records only get out of step one way — a saga start that resolved its branch
// from a process flag instead of from the order — and this is the only place left
// that sees the evidence. Reported once per order, like every other finding here.
// Reported for EVERY reservation status, not only the repairable one: a confirmed
// order whose hold is already COMMITTED is as much a wrong record as one still
// RESERVED. So this states the fact — the row does not account for a reservation
// that exists — and deliberately does not claim a repair, which for most statuses
// does not happen.
//
// Nothing is claimed unless a reservation actually came back. An empty response
// reads as an UNSPECIFIED status, which is already reported as its own breach, and
// saying "holds a reservation" about it would be a statement this code cannot
// support.
//
// TWO LIMITS, both deliberate:
//
// Suppressed once a breach code is on the row, which is this package's
// once-per-order mechanism — but that only covers outcomes that RECORD one. A
// repair that keeps failing records nothing and settles nothing, so it re-reports
// every pass, exactly as repairs_total{action="failed"} already does; the store's
// own KNOWN LIMIT comment describes the same gap and why closing it needs a
// last-attempt column.
//
// One-way: zero here does NOT mean "no skew". A skewed order that failed BEFORE
// reserving, or a confirmed one whose reserve was lost, leaves no reservation to
// find — the first is routine, the second surfaces as BreachReservationMissing.
// This sees only the half where the row survives.
func (r *Reconciler) reportParticipantDisagreement(ctx context.Context,
	c domain.ReconcileCandidate, reservation *inventoryv1.Reservation) {
	if c.Participant == participantInventory || reservation == nil || c.BreachCode != "" {
		return
	}
	r.log.Error("order holds an inventory reservation its own row does not account for",
		zap.String("order_id", c.OrderID), zap.String("row_participant", c.Participant),
		zap.String("reservation_status", reservation.GetStatus().String()))
	recordParticipantDisagreement(ctx, c.Participant)
}

// judgeMissingReservation decides what a NOT_FOUND means for this order.
//
// Normal for a product-path order: it never had an inventory reservation.
//
// Also normal for a FAILED inventory-path order. The saga authorizes payment
// BEFORE it reserves (saga/workflow.go), so a declined card fails the order
// without any reservation existing; an out-of-stock rejection rolls back
// inventory's transaction, so the header row never commits either. Both are
// routine, and reading them as breaches would poison the backlog gauge with every
// decline and every sold-out item. Inventory never deletes reservation rows, so a
// missing one cannot mean "it was there and went away".
//
// NOT normal for a CONFIRMED inventory-path order: the saga reserves before the
// pivot, so a confirmed order must have a reservation. Its absence means the
// Reserve write was lost or the row was restored away, and CommitInventory's own
// doc delegates exactly that breach here.
func (r *Reconciler) judgeMissingReservation(c domain.ReconcileCandidate) (string, string, bool) {
	if c.Participant != participantInventory || c.Status != statusConfirmed {
		return "", "", true
	}
	if c.BreachCode == "" {
		r.log.Error("confirmed inventory-path order has NO reservation; the reserve appears to have been lost",
			zap.String("order_id", c.OrderID), zap.String("order_status", c.Status))
	}
	return ActionBreach, BreachReservationMissing, false
}

// sagaStillRunning reports whether the order's fulfillment workflow is open.
//
// A workflow that no longer exists counts as closed: past the namespace
// retention there is nobody left to compensate, so the order row is the only
// evidence available and acting on it is the best that can be done.
func (r *Reconciler) sagaStillRunning(ctx context.Context, orderID string) (bool, error) {
	resp, err := r.workflows.DescribeWorkflowExecution(ctx, saga.WorkflowID(orderID), "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false, nil
		}
		return false, err
	}

	switch resp.GetWorkflowExecutionInfo().GetStatus() {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
		enumspb.WORKFLOW_EXECUTION_STATUS_PAUSED,
		enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return true, nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enumspb.WORKFLOW_EXECUTION_STATUS_FAILED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
		enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return false, nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_UNSPECIFIED:
		// Cannot tell. Treating "unknown" as closed would let the reconciler
		// write into a live saga, so it counts as still running.
		return true, nil
	}
	return true, nil
}

// repairReserved drives a still-RESERVED reservation to the state the order's
// terminal status requires.
func (r *Reconciler) repairReserved(ctx context.Context, c domain.ReconcileCandidate) (string, string, bool) {
	switch c.Status {
	case statusConfirmed:
		// The saga's own CommitInventory did not settle. Commit is serialized by
		// inventory's row lock and short-circuits on COMMITTED, so racing a late
		// saga retry costs an RPC, not a second decrement.
		if _, err := r.inventory.Commit(ctx, &inventoryv1.CommitRequest{ReservationId: c.OrderID}); err != nil {
			r.log.Error("reconciler could not commit a confirmed order's reservation",
				zap.String("order_id", c.OrderID), zap.Error(err))
			return ActionFailed, "", false
		}
		r.log.Info("reconciler committed a confirmed order's reservation",
			zap.String("order_id", c.OrderID))
		return ActionCommitted, "", true

	case statusFailed:
		// Stock held against an order that will never ship.
		if _, err := r.inventory.Release(ctx, &inventoryv1.ReleaseRequest{
			ReservationId: c.OrderID,
			Reason:        reasonOrderFailed,
		}); err != nil {
			r.log.Error("reconciler could not release a failed order's reservation",
				zap.String("order_id", c.OrderID), zap.Error(err))
			return ActionFailed, "", false
		}
		r.log.Info("reconciler released a failed order's reservation",
			zap.String("order_id", c.OrderID))
		return ActionReleased, "", true
	}

	// The lister only returns terminal statuses, so this means the query and this
	// switch have drifted apart.
	r.log.Error("reconciler asked to repair a non-terminal order; the candidate query and the repair logic disagree",
		zap.String("order_id", c.OrderID), zap.String("order_status", c.Status))
	return ActionBreach, BreachNonTerminalOrder, false
}

// Order statuses this package acts on. Kept local so the package does not import
// the saga to read two strings.
const (
	statusConfirmed = "confirmed"
	statusFailed    = "failed"
)

// Stock participants as recorded on the outbox row. Kept local so this package
// does not import the saga to read two strings.
//
// Only participantInventory means "expect a reservation". Everything else —
// participantProduct, and an empty column, which every reader of it treats as the
// product path — means the order should have none, which is why finding one is
// worth reporting.
// Taken from the saga rather than re-spelled: this package already imports it for
// WorkflowID, so there is no cost, and a rename there must break the build here
// instead of silently reclassifying every order.
const (
	participantInventory = string(saga.ParticipantInventory)
	participantProduct   = string(saga.ParticipantProduct)
)
