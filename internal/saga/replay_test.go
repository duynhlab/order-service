package saga

import (
	"os"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/worker"
)

// TestReplayRecordedHistories replays every recorded workflow history of the
// CURRENT generation against the current OrderFulfillmentWorkflow code (the
// determinism gate). A failure here means the workflow change is
// history-incompatible: an in-flight workflow started under the recorded code
// would break at the next worker deploy. Fix the change — never delete a
// history to make this pass.
//
// Generations (ADR-030): the corpus is split by worker deployment version.
// testdata/gen1/ holds the histories recorded against the ≤v1.9.x workflow
// (pre-P5 status commands); the P5 saga rewrite genuinely diverges from them
// — a Complete activity in the happy tail, reason-carrying FailOrder — and
// under pinned worker versioning no execution recorded on gen-1 code is ever
// claimed by a gen-2 build, so cross-generation replay asserts nothing about
// production. gen-1 stays in-tree as the gate for any v1.9.x maintenance
// build. testdata/gen2/ is recorded from the P5 code via the procedure in
// testdata/README.md.
//
// The skip below is a TEMPORARY gate-weakening, tolerated only between the
// saga rewrite landing and the gen-2 corpus being recorded from local-stack.
// Cutting the v1.10.0 tag with this skip still firing is forbidden — the
// recording PR restores fail-if-empty by existing.
func TestReplayRecordedHistories(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "gen2", "history_*.json"))
	if err != nil {
		t.Fatalf("glob testdata/gen2: %v", err)
	}
	if len(files) == 0 {
		// Mechanical half of the tag gate: the release procedure runs the
		// suite with ORDER_RELEASE_GATE=1, which turns this skip into a
		// failure — a tag cannot be cut with the corpus still missing.
		if os.Getenv("ORDER_RELEASE_GATE") != "" {
			t.Fatal("gen-2 corpus missing and ORDER_RELEASE_GATE is set — record testdata/gen2 before tagging (see testdata/README.md)")
		}
		t.Skip("gen-2 corpus not recorded yet — MUST exist before the v1.10.0 worker build is tagged (see testdata/README.md)")
	}

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			// Both workflow types: the replayer resolves each history by its
			// recorded type name, so one registration set serves the corpus.
			replayer.RegisterWorkflow(OrderFulfillmentWorkflow)
			replayer.RegisterWorkflow(CancellationWorkflow)
			if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, f); err != nil {
				t.Errorf("history %s does not replay against current workflow code: %v", f, err)
			}
		})
	}
}
