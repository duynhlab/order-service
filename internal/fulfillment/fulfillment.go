// Package fulfillment is the single place the order-fulfillment saga is
// started from. Both transports delegate here (web keeps its own logging
// semantics; gRPC adds idempotent-kickoff semantics) so the load-bearing
// details — the saga input mapping, the detached 5-second start context, the
// workflow-id dedup scheme — cannot drift between them (RFC-0015 P2,
// homelab ADR-018).
package fulfillment

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/saga"
)

// Starter starts a Temporal workflow. *client.Client satisfies it; kept as an
// interface so both transports stay testable with a fake.
type Starter interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow any, args ...any) (client.WorkflowRun, error)
}

// ErrAlreadyStarted reports that a fulfillment workflow for this order id
// already exists (open — any reuse policy; or closed within the namespace
// retention when RejectDuplicate is used). Callers decide what that means:
// the gRPC adapter treats it as success (the saga already happened), the web
// handler logs it like any other start failure (pre-P2 behavior preserved).
var ErrAlreadyStarted = errors.New("fulfillment workflow already started")

// startTimeout bounds the workflow start call, detached from the caller's
// request context so a client disconnect cannot cancel the start.
const startTimeout = 5 * time.Second

// Options tunes the start semantics per transport.
type Options struct {
	// ReusePolicy: zero (UNSPECIFIED) now means REJECT_DUPLICATE, not the
	// server default.
	//
	// This changed with the start outbox (RFC-0021 P3). Before it there was one
	// starter per order, so the server default (ALLOW_DUPLICATE) was harmless.
	// Now the inline path AND the dispatcher can both start the same workflow id,
	// and ALLOW_DUPLICATE permits a NEW run once the previous one has closed — so
	// a slow inline start that lands after the dispatcher's saga already
	// completed would begin a second saga: a second AuthorizePayment and a second
	// CapturePayment. Real money, from a race that is entirely reachable (the
	// outbox row is due immediately, the dispatcher polls every 5s, and the
	// inline start has a 5s timeout).
	//
	// Defaulting here rather than asking each caller to remember is deliberate:
	// omission must be safe. A caller that genuinely wants duplicates has to say
	// so explicitly.
	//
	// Rejecting is always correct for this workflow id: it is derived from the
	// order id, and one order never legitimately needs two sagas.
	// Semantics: https://docs.temporal.io/workflow-execution/workflowid-runid
	ReusePolicy enumspb.WorkflowIdReusePolicy

	// StockParticipant is the branch this saga's stock runs on, stamped into the
	// workflow input. Since RFC-0021 P4 the only servable value is
	// saga.ParticipantInventory; Start REFUSES anything else, including the zero
	// value. It used to be the opposite — empty meant "the pre-migration product
	// path", a safe default for a caller not yet wired up — and that reading is
	// now the worst outcome available, because an empty participant reaches a
	// workflow that panics its task forever.
	StockParticipant saga.Participant
}

// ErrParticipantUnservable means the order's recorded participant is a branch this
// build no longer has. It is a START-time refusal: better a start that visibly
// fails than a saga that runs and stalls where nothing is watching.
var ErrParticipantUnservable = errors.New("stock participant cannot be served by this build")

// ParticipantSource says WHY ParticipantFor answered as it did, so a caller can
// react to a value it could not use without re-parsing the string it just handed
// over.
type ParticipantSource int

const (
	// SourceRecorded: the order's own record named a branch this build knows.
	SourceRecorded ParticipantSource = iota
	// SourceAbsent: nothing was recorded for this order — the product path by
	// definition, not an invitation to guess (see ParticipantFor).
	SourceAbsent
	// SourceUnrecognised: something was recorded that this build cannot use.
	SourceUnrecognised
)

// ParticipantFor resolves which service owns an order's stock writes. It is the
// platform's one resolution rule, shared by every start path.
//
// The RECORDED value wins over the caller's flag, because the two are not
// interchangeable: the flag is a property of the process answering right now, the
// record is a property of the order. For an order that already exists they can
// disagree — during the cutover's rolling restart, replicas run different flags —
// and the saga must follow the order. Substituting the flag there runs one branch
// while the row names the other, and the reconciler judges by the row: in one
// direction that files a false lost-reserve breach on every confirmed order, in
// the other it swallows a real one.
//
// NOTHING RECORDED still means the PRODUCT path, and that reading does NOT change
// now that the product branch is gone (RFC-0021 P4). Every order that can have an
// empty value predates the participant column, so product is what it actually ran,
// and the other readers of the column agree: saga.OrderFulfillmentInput.participant
// maps "" to product, and reconcile.judgeMissingReservation treats an empty row as
// a product-path order with no reservation to find. Answering inventory instead
// would release stock inventory never reserved and leave the product hold orphaned
// — the invisible hold this resolution exists to prevent.
//
// What changed is the CONSEQUENCE, not the reading. A product answer can no longer
// be SERVED, so this function refuses it with ErrParticipantUnservable and the
// start never happens. That refusal is here rather than only in the workflow on
// purpose: a workflow started with an unservable participant panics its task, and
// a panicking task is NOT loud enough — the gRPC start still answers success, the
// dispatcher sees a RUNNING execution and closes its outbox row, and the
// reconciler only scans terminal orders. The order would sit `pending` with
// nothing pointing at it. A refused START is visible: the caller gets an error and
// the outbox row fails, which is already alerted.
//
// A value the saga would not recognise resolves to NOTHING, and is refused like
// any other unservable participant. It used to fall back to the caller's flag,
// which after RFC-0021 P4 would mean silently reserving real stock at inventory
// for an order whose row says something else — and the reconciler skips rows whose
// participant is not inventory, so that hold would never be judged. An invisible
// hold, arrived at from the other direction. Refusing is the only answer that does
// not invent one. Nothing writes such a value; the reachable path is a
// hand-edited row.
// Every resolution is counted here rather than by the callers, because a caller
// that forgets makes the decision unobservable exactly where it matters — the
// inline start handles almost every order, and the transport with no logger is the
// one that would go silent.
func ParticipantFor(ctx context.Context, recorded string) (saga.Participant, ParticipantSource, error) {
	participant, source := resolveParticipant(recorded)
	servable := participant == saga.ParticipantInventory
	recordStartParticipant(ctx, string(participant), source, servable)
	if !servable {
		return participant, source, fmt.Errorf("%w: recorded %q resolves to %q",
			ErrParticipantUnservable, recorded, participant)
	}
	return participant, source, nil
}

func resolveParticipant(recorded string) (saga.Participant, ParticipantSource) {
	switch saga.Participant(recorded) {
	case saga.ParticipantProduct:
		return saga.ParticipantProduct, SourceRecorded
	case saga.ParticipantInventory:
		return saga.ParticipantInventory, SourceRecorded
	case "":
		return saga.ParticipantProduct, SourceAbsent
	}
	return "", SourceUnrecognised
}

// Start kicks off OrderFulfillmentWorkflow for a committed order. It builds
// the saga input exactly as the web handler always has: no bearer token (the
// saga's cart-clear uses the tokenless internal route), paymentMethod carried
// separately from the order (it is never persisted). Returns
// ErrAlreadyStarted when a workflow with this order's id already exists;
// any other error is the raw start failure. t must be non-nil.
func Start(ctx context.Context, t Starter, taskQueue string, order *domain.Order, paymentMethod string, opts Options) error {
	items := make([]saga.ReserveItem, len(order.Items))
	for i, it := range order.Items {
		items[i] = saga.ReserveItem{ProductID: it.ProductID, Quantity: it.Quantity}
	}
	// The guard lives HERE as well as at the callers, because this is the single
	// place a saga is started from. A caller-side check protects the callers that
	// exist today; this one protects the next one — a "retry my fulfillment"
	// endpoint, a backfill tool, a test promoted into a tool — which would
	// otherwise get an accepted workflow that panics its task forever, with
	// nothing refusing it and nothing watching. The callers keep their own check
	// only because they map the refusal to a transport answer.
	if opts.StockParticipant != saga.ParticipantInventory {
		return fmt.Errorf("%w: %q", ErrParticipantUnservable, opts.StockParticipant)
	}

	input := saga.OrderFulfillmentInput{
		OrderID:       order.ID,
		UserID:        order.UserID,
		Total:         order.Total,
		Items:         items,
		PaymentMethod: paymentMethod,
		// Stamped once, here. The worker reads the participant from the input,
		// never from the flag, so this value is pinned for the saga's lifetime.
		StockParticipant: opts.StockParticipant,
	}

	reusePolicy := opts.ReusePolicy
	if reusePolicy == enumspb.WORKFLOW_ID_REUSE_POLICY_UNSPECIFIED {
		reusePolicy = enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE
	}

	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startTimeout)
	defer cancel()
	_, err := t.ExecuteWorkflow(startCtx, client.StartWorkflowOptions{
		ID:                    saga.WorkflowID(order.ID),
		TaskQueue:             taskQueue,
		WorkflowIDReusePolicy: reusePolicy,
		// Without this the SDK SWALLOWS a rejected duplicate: it converts
		// serviceerror.WorkflowExecutionAlreadyStarted into a handle for the
		// existing run and returns a nil error
		// (sdk@v1.45.0/internal/internal_workflow_client.go:2144-2151, documented
		// at internal/client.go:93). Every caller here needs to know the
		// difference — the outbox dispatcher would otherwise treat a REFUSED
		// start as a successful one, close the durable row, and strand the order
		// `pending` with no workflow; and the gRPC adapter's idempotent-kickoff
		// branch on ErrAlreadyStarted could never fire at all.
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, saga.OrderFulfillmentWorkflow, input)
	if err != nil {
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &already) {
			// Wrap (not replace): errors.Is(ErrAlreadyStarted) still matches
			// and web's log keeps the run-id detail for incident forensics.
			return fmt.Errorf("%w: %w", ErrAlreadyStarted, err)
		}
		return err
	}
	return nil
}
