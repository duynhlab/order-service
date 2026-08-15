package v1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/duynhlab/order-service/internal/core/domain"
	logicv1 "github.com/duynhlab/order-service/internal/logic/v1"
	"github.com/duynhlab/pkg/authmw"
)

// The operator resolve command (ADR-051). The writer itself is covered against
// a real Postgres in the repository's integration tests — what these tests own
// is the transport contract: who the actor is, which refusals map to which
// status and code, and that a replay is visibly not a second transition.

const testOperatorSub = "d0e00000-0000-4000-8000-000000000001"

// captureWriter records the command it was handed and returns a scripted
// outcome, so a test can assert on what the handler BUILT — the actor in
// particular, which must come from the token and nowhere else.
type captureWriter struct {
	got      domain.StatusCommand
	calls    int
	replayed bool
	err      error
}

func (w *captureWriter) ApplyStatusCommand(_ context.Context, cmd domain.StatusCommand) (bool, error) {
	w.got = cmd
	w.calls++
	return w.replayed, w.err
}

type stubHistory struct {
	rows []domain.StatusHistoryEntry
	err  error
}

func (s *stubHistory) ListStatusHistory(_ context.Context, _ string) ([]domain.StatusHistoryEntry, error) {
	return s.rows, s.err
}

// operatorEngine mounts the protected group with the resolve dependencies. The
// role gate is the real middleware; only the JWT verification is faked, and
// `subject` empty models a chain that authenticated without a subject.
func operatorEngine(t *testing.T, repo domain.OrderRepository, w domain.OrderStatusWriter,
	hist domain.StatusHistoryReader, subject string, roles ...string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewOrderHandler(logicv1.NewOrderService(repo, nil, &stubOutbox{}, nil, noopProjection{}, nil, nil),
		nil, nil, "", nil, nil, nil, nil, w, hist)
	h.mountProtected(r,
		func(c *gin.Context) {
			if subject != "" {
				c.Set(authmw.CtxUserID, subject)
			}
			c.Set(authmw.CtxRoles, roles)
			c.Next()
		},
		authmw.MiddlewareRequireRole(backofficeRole))
	return r
}

func parkedRepo() *unscopedRepo {
	return &unscopedRepo{byID: map[string]*domain.Order{
		"6": {ID: "6", UserID: "u-1", Status: "manual_review", Total: 25798, Version: 7},
	}}
}

func resolveBody(t *testing.T, target, reason, note string, version int64) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"target": target, "reason": reason, "note": note, "version": version,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return bytes.NewReader(b)
}

func postResolve(t *testing.T, r *gin.Engine, body *bytes.Reader) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/order/v1/protected/orders/6/resolve", body)
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func TestResolveRoleGate(t *testing.T) {
	writer := &captureWriter{}
	r := operatorEngine(t, parkedRepo(), writer, nil, testOperatorSub, "customer")
	w := postResolve(t, r, resolveBody(t, "cancelled", "REFUNDED_MANUALLY", "checked", 7))
	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	if writer.calls != 0 {
		t.Error("the role gate let a command through")
	}
}

func TestResolveApplies(t *testing.T) {
	writer := &captureWriter{}
	r := operatorEngine(t, parkedRepo(), writer, nil, testOperatorSub, backofficeRole)

	w := postResolve(t, r, resolveBody(t, "cancelled", "REFUNDED_MANUALLY", "refund confirmed in the provider console", 7))
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	var got resolveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Applied {
		t.Error("applied = false on a fresh command")
	}
	if got.Order == nil || got.Order.ID != "6" {
		t.Errorf("settled order not returned: %+v", got.Order)
	}

	cmd := writer.got
	if cmd.To != domain.OrderStatusCancelled {
		t.Errorf("target = %q, want cancelled", cmd.To)
	}
	if cmd.Reason != domain.ReasonRefundedManually {
		t.Errorf("reason = %q, want REFUNDED_MANUALLY", cmd.Reason)
	}
	if cmd.ActorType != domain.ActorOperator {
		t.Errorf("actor type = %q, want OPERATOR", cmd.ActorType)
	}
	if cmd.Note == "" {
		t.Error("note not carried onto the command")
	}
	// The version the operator read has to be in the command id, or a retry
	// would mint a second command instead of replaying.
	if want := "resolve:6:v7:cancelled"; cmd.CommandID != want {
		t.Errorf("command id = %q, want %q", cmd.CommandID, want)
	}
	// And it has to travel as a precondition too. The command id alone only
	// namespaces retries; without this the writer would apply a stale decision
	// whenever the FSM happened to allow the target from wherever the order had
	// moved to.
	if cmd.ExpectedVersion == nil || *cmd.ExpectedVersion != 7 {
		t.Errorf("expected version = %v, want 7", cmd.ExpectedVersion)
	}
}

// The actor is the token subject. A body that names someone else must not be
// able to sign their name to an operator decision.
func TestResolveIgnoresBodySuppliedActor(t *testing.T) {
	writer := &captureWriter{}
	r := operatorEngine(t, parkedRepo(), writer, nil, testOperatorSub, backofficeRole)

	body := bytes.NewReader([]byte(`{"target":"failed","version":7,"reason":"WRITTEN_OFF",
		"note":"accepted the loss","actor_sub":"somebody-else","actor_id":"somebody-else","operator":"somebody-else"}`))
	if w := postResolve(t, r, body); w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", w.Code, w.Body.String())
	}
	if writer.got.ActorID != testOperatorSub {
		t.Errorf("actor = %q, want the token subject %q", writer.got.ActorID, testOperatorSub)
	}
}

func TestResolveReplayIs200(t *testing.T) {
	writer := &captureWriter{replayed: true}
	r := operatorEngine(t, parkedRepo(), writer, nil, testOperatorSub, backofficeRole)

	w := postResolve(t, r, resolveBody(t, "cancelled", "REFUNDED_MANUALLY", "checked", 7))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 on replay, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"applied":false`) {
		t.Errorf("replay must say applied:false, got %s", w.Body.String())
	}
}

func TestResolveErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		writerErr  error
		wantStatus int
		wantCode   string
	}{
		{"stale version", domain.ErrConcurrencyConflict, http.StatusConflict, "VERSION_CONFLICT"},
		{"already terminal", domain.ErrInvalidTransition, http.StatusConflict, "INVALID_TRANSITION"},
		{"same command, other outcome", domain.ErrIdempotencyConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
		{"no such order", domain.ErrNotFound, http.StatusNotFound, "NOT_FOUND"},
		{"anything else", errors.New("boom"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := operatorEngine(t, parkedRepo(), &captureWriter{err: tc.writerErr}, nil, testOperatorSub, backofficeRole)
			w := postResolve(t, r, resolveBody(t, "cancelled", "REFUNDED_MANUALLY", "checked", 7))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			var env struct{ Code string }
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
		})
	}
}

// Everything the domain refuses before a transaction opens: the writer must
// never be called for these.
func TestResolveRefusedBeforeTheWrite(t *testing.T) {
	cases := []struct {
		name                 string
		target, reason, note string
		version              int64
		wantStatus           int
		wantCode             string
	}{
		{"target outside the resolve set", "pending", "WRITTEN_OFF", "note", 7, http.StatusConflict, "INVALID_TRANSITION"},
		{"target that is not a status", "banana", "WRITTEN_OFF", "note", 7, http.StatusConflict, "INVALID_TRANSITION"},
		{"a reason from another command", "failed", "CUSTOMER_REQUEST", "note", 7, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"an invented reason", "failed", "LOOKS_FINE", "note", 7, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"no note", "failed", "WRITTEN_OFF", "", 7, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"no version", "failed", "WRITTEN_OFF", "note", 0, http.StatusBadRequest, "VALIDATION_ERROR"},
		{"a note the size of a stack trace", "failed", "WRITTEN_OFF", strings.Repeat("x", 513), 7, http.StatusBadRequest, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writer := &captureWriter{}
			r := operatorEngine(t, parkedRepo(), writer, nil, testOperatorSub, backofficeRole)
			w := postResolve(t, r, resolveBody(t, tc.target, tc.reason, tc.note, tc.version))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.wantStatus, w.Body.String())
			}
			var env struct{ Code string }
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
			if writer.calls != 0 {
				t.Error("a refused command still reached the writer")
			}
		})
	}
}

// A verified request with no subject is a misassembled chain, and a privileged
// write has to fail closed rather than record an anonymous actor.
func TestResolveWithoutSubjectFailsClosed(t *testing.T) {
	writer := &captureWriter{}
	r := operatorEngine(t, parkedRepo(), writer, nil, "", backofficeRole)
	w := postResolve(t, r, resolveBody(t, "cancelled", "REFUNDED_MANUALLY", "checked", 7))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", w.Code)
	}
	if writer.calls != 0 {
		t.Error("a subjectless request reached the writer")
	}
}

// A committed transition stays committed even if the re-read for the response
// fails: the command reports success, with the order omitted.
func TestResolveSurvivesAFailedReread(t *testing.T) {
	repo := parkedRepo()
	repo.getErr = context.DeadlineExceeded
	r := operatorEngine(t, repo, &captureWriter{}, nil, testOperatorSub, backofficeRole)
	w := postResolve(t, r, resolveBody(t, "cancelled", "REFUNDED_MANUALLY", "checked", 7))
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201 — the write landed, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"applied":true`) {
		t.Errorf("want applied:true, got %s", w.Body.String())
	}
}

func TestCaseViewCarriesVersionAndHistory(t *testing.T) {
	hist := &stubHistory{rows: []domain.StatusHistoryEntry{
		{FromStatus: "manual_review", ToStatus: "cancelled", ReasonCode: "REFUNDED_MANUALLY",
			ActorType: "OPERATOR", ActorID: testOperatorSub, Note: "refund confirmed", CommandID: "resolve:6:v7:cancelled"},
	}}
	r := operatorEngine(t, parkedRepo(), &captureWriter{}, hist, testOperatorSub, backofficeRole)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders/6", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}

	var got struct {
		ID            string `json:"id"`
		UserID        string `json:"user_id"`
		Version       int64  `json:"version"`
		StatusHistory []struct {
			ToStatus  string `json:"to_status"`
			ActorType string `json:"actor_type"`
			ActorID   string `json:"actor_id"`
		} `json:"status_history"`
		Degraded []string `json:"degraded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The order's own fields stay where they were before this route grew.
	if got.ID != "6" || got.UserID != "u-1" {
		t.Errorf("the order fields moved: %+v", got)
	}
	if got.Version != 7 {
		t.Errorf("version = %d, want 7 — the resolve command needs it", got.Version)
	}
	if len(got.StatusHistory) != 1 || got.StatusHistory[0].ActorType != "OPERATOR" ||
		got.StatusHistory[0].ActorID != testOperatorSub {
		t.Errorf("audit trail not rendered: %+v", got.StatusHistory)
	}
	if len(got.Degraded) != 0 {
		t.Errorf("nothing failed, so degraded must be empty: %v", got.Degraded)
	}
}

// An unreadable history degrades its own block. An empty list must never be
// mistaken for "nothing ever happened to this order".
func TestCaseViewDegradesOnHistoryFailure(t *testing.T) {
	hist := &stubHistory{err: context.DeadlineExceeded}
	r := operatorEngine(t, parkedRepo(), &captureWriter{}, hist, testOperatorSub, backofficeRole)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/order/v1/protected/orders/6", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("the case view must still answer, got %d", w.Code)
	}
	var got struct {
		StatusHistory []domain.StatusHistoryEntry `json:"status_history"`
		Degraded      []string                    `json:"degraded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.StatusHistory) != 0 {
		t.Errorf("want an empty history, got %+v", got.StatusHistory)
	}
	if fmt.Sprint(got.Degraded) != "[status_history]" {
		t.Errorf("degraded = %v, want [status_history]", got.Degraded)
	}
}
