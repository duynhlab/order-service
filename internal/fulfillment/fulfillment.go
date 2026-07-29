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

	// StockParticipant is the validated ORDER_STOCK_PARTICIPANT value (RFC-0021
	// P3). Both transports pass the same configured value — it is a platform
	// routing decision, not a transport one — and Start stamps it into the
	// workflow input. Empty (the zero Options) keeps the pre-migration product
	// path, so a caller that has not been wired up yet gets the old behavior
	// rather than an unconfigured one.
	StockParticipant saga.Participant
}

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
// NOTHING RECORDED means the PRODUCT path, and deliberately not the flag. Every
// order that can have an empty value predates the participant column, so product
// is what it actually ran — and it is what the two other readers of that column
// already mean by empty: saga.OrderFulfillmentInput.participant maps "" to
// product, and reconcile.judgeMissingReservation treats an empty row as a
// product-path order with no reservation to find. Answering the flag instead
// would, after the cutover, reserve real stock in inventory for an order the
// reconciler will never probe — an invisible hold, which is the exact failure
// this resolution exists to prevent.
//
// A value the saga would not recognise falls back to the flag rather than being
// passed on: the workflow panics on an unknown participant (deliberately — it must
// not guess which service holds the stock), and a stalled workflow is a heavier
// consequence than the flag plus a caller that reports SourceUnrecognised. Nothing
// writes such a value; the reachable path is a hand-edited row.
// Every resolution is counted here rather than by the callers, because a caller
// that forgets makes the decision unobservable exactly where it matters — the
// inline start handles almost every order, and the transport with no logger is the
// one that would go silent.
func ParticipantFor(ctx context.Context, recorded string, fallback saga.Participant) (saga.Participant, ParticipantSource) {
	participant, source := resolveParticipant(recorded, fallback)
	recordStartParticipant(ctx, string(participant), source)
	return participant, source
}

func resolveParticipant(recorded string, fallback saga.Participant) (saga.Participant, ParticipantSource) {
	switch saga.Participant(recorded) {
	case saga.ParticipantProduct:
		return saga.ParticipantProduct, SourceRecorded
	case saga.ParticipantInventory:
		return saga.ParticipantInventory, SourceRecorded
	case "":
		return saga.ParticipantProduct, SourceAbsent
	}
	return fallback, SourceUnrecognised
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
