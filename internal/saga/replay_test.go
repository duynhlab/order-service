package saga

import (
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/worker"
)

// TestReplayRecordedHistories replays every recorded workflow history in
// testdata/ against the CURRENT OrderFulfillmentWorkflow code (RFC-0021 P3
// determinism gate). A failure here means the workflow change is
// history-incompatible: an in-flight workflow started under the recorded code
// would break at the next worker deploy. Fix the change (workflow.GetVersion
// branch) — never delete a history to make this pass.
//
// Histories are REAL executions exported from local-stack Temporal; the
// recording procedure lives in testdata/README.md.
func TestReplayRecordedHistories(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "history_*.json"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no recorded histories found in testdata/ — the determinism gate would be vacuous")
	}

	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(OrderFulfillmentWorkflow)
			if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, f); err != nil {
				t.Errorf("history %s does not replay against current workflow code: %v", f, err)
			}
		})
	}
}
