package fulfillment

import (
	"context"
	"errors"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/duynhlab/order-service/internal/core/domain"
	"github.com/duynhlab/order-service/internal/saga"
)

type fakeStarter struct {
	err      error
	gotOpts  client.StartWorkflowOptions
	gotInput []any
	gotCtx   context.Context
}

func (f *fakeStarter) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	f.gotCtx = ctx
	f.gotOpts = options
	f.gotInput = args
	return nil, f.err
}

func order() *domain.Order {
	return &domain.Order{
		ID: "42", UserID: "7", Total: 6498,
		Items: []domain.OrderItem{
			{ProductID: "1", Quantity: 2, Price: 2999},
			{ProductID: "9", Quantity: 1, Price: 500},
		},
	}
}

func TestStart_MapsOrderIntoSagaInputWithDedupID(t *testing.T) {
	st := &fakeStarter{}

	err := Start(context.Background(), st, "order-fulfillment", order(), "tok_visa_ok",
		Options{ReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			StockParticipant: saga.ParticipantInventory})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.gotOpts.ID != saga.WorkflowID("42") || st.gotOpts.TaskQueue != "order-fulfillment" {
		t.Errorf("opts = %+v, want order-fulfillment-42 on order-fulfillment", st.gotOpts)
	}
	if st.gotOpts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
		t.Errorf("reuse policy = %v, want the caller's policy passed through", st.gotOpts.WorkflowIDReusePolicy)
	}
	input, ok := st.gotInput[0].(saga.OrderFulfillmentInput)
	if !ok {
		t.Fatalf("input type = %T", st.gotInput[0])
	}
	if input.OrderID != "42" || input.UserID != "7" || input.Total != 6498 || input.PaymentMethod != "tok_visa_ok" {
		t.Errorf("input = %+v, want the order mapped verbatim", input)
	}
	if len(input.Items) != 2 || input.Items[0] != (saga.ReserveItem{ProductID: "1", Quantity: 2}) {
		t.Errorf("items = %+v, want product/quantity pairs only", input.Items)
	}
	if deadline, ok := st.gotCtx.Deadline(); !ok || deadline.IsZero() {
		t.Error("start context must carry the detached 5s deadline")
	}
}

// Zero Options must mean REJECT_DUPLICATE, not the server default.
//
// This test previously asserted the opposite, and that assertion was pinning a
// money bug. With the start outbox there are two starters for one workflow id —
// the inline path and the dispatcher — and ALLOW_DUPLICATE (the server default)
// permits a NEW run once the previous one has CLOSED. A slow inline start that
// lands after the dispatcher's saga already completed would begin a second
// saga: a second AuthorizePayment and a second CapturePayment. The race is
// reachable, not theoretical: the outbox row is due immediately, the dispatcher
// polls every 5s, and the inline start is bounded at 5s.
//
// Omission has to be the safe choice, so the default lives here rather than in
// each caller's memory.
func TestStart_ZeroOptionsRejectDuplicates(t *testing.T) {
	st := &fakeStarter{}
	if err := Start(context.Background(), st, "q", order(), "", Options{StockParticipant: saga.ParticipantInventory}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.gotOpts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE {
		t.Errorf("policy = %v, want REJECT_DUPLICATE — the server default would allow a second charge-bearing run",
			st.gotOpts.WorkflowIDReusePolicy)
	}
}

// An explicit policy still wins, so a caller that genuinely wants duplicates can
// ask for them.
func TestStart_ExplicitReusePolicyWins(t *testing.T) {
	st := &fakeStarter{}
	err := Start(context.Background(), st, "q", order(), "",
		Options{ReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			StockParticipant: saga.ParticipantInventory})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.gotOpts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE {
		t.Errorf("policy = %v, want the explicit ALLOW_DUPLICATE", st.gotOpts.WorkflowIDReusePolicy)
	}
}

func TestStart_AlreadyStartedMapsToSentinelKeepingDetail(t *testing.T) {
	underlying := &serviceerror.WorkflowExecutionAlreadyStarted{Message: "workflow execution already started"}
	st := &fakeStarter{err: underlying}

	err := Start(context.Background(), st, "q", order(), "", Options{StockParticipant: saga.ParticipantInventory})
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("err = %v, want ErrAlreadyStarted", err)
	}
	var se *serviceerror.WorkflowExecutionAlreadyStarted
	if !errors.As(err, &se) {
		t.Error("underlying serviceerror lost — web logs need the detail")
	}
	if !strings.Contains(err.Error(), "already started") {
		t.Errorf("err text %q lost the detail", err)
	}
}

func TestStart_OtherErrorsPassThrough(t *testing.T) {
	boom := errors.New("dial temporal:7233: connection refused")
	st := &fakeStarter{err: boom}

	if err := Start(context.Background(), st, "q", order(), "", Options{StockParticipant: saga.ParticipantInventory}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the raw start failure", err)
	}
}

// The participant is stamped into the workflow input at start and nowhere else.
// This is the seam that makes a flag revert affect only NEW sagas: the worker
// reads the participant from the input it was started with, so an in-flight saga
// cannot change which service holds its stock halfway through.
func TestStart_StampsStockParticipantIntoInput(t *testing.T) {
	st := &fakeStarter{}

	err := Start(context.Background(), st, "order-fulfillment", order(), "tok_visa_ok",
		Options{StockParticipant: saga.ParticipantInventory})
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}

	in, ok := st.gotInput[0].(saga.OrderFulfillmentInput)
	if !ok {
		t.Fatalf("workflow arg is %T, want saga.OrderFulfillmentInput", st.gotInput[0])
	}
	if in.StockParticipant != saga.ParticipantInventory {
		t.Errorf("StockParticipant = %q, want %q", in.StockParticipant, saga.ParticipantInventory)
	}
}

// Zero Options used to be the SAFE default: no participant meant the
// pre-migration product path, so a caller not yet wired up got the old behaviour.
// Since RFC-0021 P4 that reading is the worst outcome available — an empty
// participant reaches a workflow that panics its task forever — so the zero value
// is refused here, in the one place every start goes through, and no workflow is
// created for it.
func TestStart_RefusesAnUnservableParticipant(t *testing.T) {
	for _, participant := range []saga.Participant{"", saga.ParticipantProduct, "warehouse-9"} {
		t.Run("participant="+string(participant), func(t *testing.T) {
			st := &fakeStarter{}

			err := Start(context.Background(), st, "order-fulfillment", order(), "",
				Options{StockParticipant: participant})
			if !errors.Is(err, ErrParticipantUnservable) {
				t.Errorf("Start() = %v, want ErrParticipantUnservable", err)
			}
			if len(st.gotInput) != 0 {
				t.Errorf("a workflow was started with %+v; want none", st.gotInput)
			}
		})
	}
}

// One resolution rule, shared by every start path. What was recorded for the
// order wins, and since RFC-0021 P4 only `inventory` can be SERVED: everything
// else resolves to what it has always meant and is then REFUSED, rather than
// being quietly answered with the one branch that is left.
func TestParticipantFor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		recorded   string
		want       saga.Participant
		wantSource ParticipantSource
		wantErr    bool
	}{
		{
			name:       "recorded inventory is the one servable answer",
			recorded:   "inventory",
			want:       saga.ParticipantInventory,
			wantSource: SourceRecorded,
		},
		{
			// Answering inventory here would reserve real stock for an order whose
			// stock is held at product-service, and orphan the hold there.
			name:       "recorded product still means product, and is refused",
			recorded:   "product",
			want:       saga.ParticipantProduct,
			wantSource: SourceRecorded,
			wantErr:    true,
		},
		{
			// Every order created before RFC-0021 P3 carries no value, and those
			// really did hold their stock at product-service.
			name:       "nothing recorded means product, and is refused",
			recorded:   "",
			want:       saga.ParticipantProduct,
			wantSource: SourceAbsent,
			wantErr:    true,
		},
		{
			// The enum grows on the inventory side, so an unknown token is more
			// likely a branch this build has never heard of than a known one.
			name:       "a token this build does not know resolves to nothing",
			recorded:   "warehouse",
			want:       "",
			wantSource: SourceUnrecognised,
			wantErr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, source, err := ParticipantFor(context.Background(), tc.recorded)
			if got != tc.want || source != tc.wantSource {
				t.Errorf("ParticipantFor(%q) = (%q, %v), want (%q, %v)",
					tc.recorded, got, source, tc.want, tc.wantSource)
			}
			if tc.wantErr && !errors.Is(err, ErrParticipantUnservable) {
				t.Errorf("error = %v, want ErrParticipantUnservable", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error %v", err)
			}
		})
	}
}

// The label values are the query surface for the cutover dashboards; a rename is
// a silent break of every one of them.
func TestParticipantSource_LabelValues(t *testing.T) {
	for source, want := range map[ParticipantSource]string{
		SourceRecorded:     "recorded",
		SourceAbsent:       "absent",
		SourceUnrecognised: "unrecognised",
	} {
		if got := source.String(); got != want {
			t.Errorf("source %d = %q, want %q", source, got, want)
		}
	}
	// A source this build does not name must not silently become a real label and
	// blend into the cutover counters.
	if got := ParticipantSource(99).String(); got != "invalid" {
		t.Errorf("unknown source = %q, want %q", got, "invalid")
	}
}
