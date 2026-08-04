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
// gen1/ holds histories recorded against the ≤v1.9.x workflow (pre-P5 status
// commands). gen2/ was recorded from the P5 code. gen3/ is recorded from the
// RFC-0021 P4 code, which deleted the product stock branch.
//
// Each split is justified the same way, and it is NOT a way to make a failure go
// away. Under pinned worker versioning no execution recorded on an older
// generation is ever claimed by a newer build, so cross-generation replay asserts
// nothing about production. What forced gen3 was measured, not assumed: of the six
// gen2 histories, exactly the four carrying `participant=product` stop replaying,
// because this build refuses that participant rather than running the inventory
// branch for stock held at product-service. The two that do not carry it still
// replay green.
//
// gen1 and gen2 stay in-tree as the gate for maintenance builds of their own
// versions — gen2 is the last generation that can serve a product history at all.
//
// The skip below covers only the window between a generation opening and its
// corpus being recorded from local-stack; ORDER_RELEASE_GATE turns it into a
// failure so a tag cannot be cut inside that window. gen3 is recorded, so it does
// not fire today — it stays for the next generation.
func TestReplayRecordedHistories(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "gen3", "history_*.json"))
	if err != nil {
		t.Fatalf("glob testdata/gen3: %v", err)
	}
	if len(files) == 0 {
		// Mechanical half of the tag gate: the release procedure runs the
		// suite with ORDER_RELEASE_GATE=1, which turns this skip into a
		// failure — a tag cannot be cut with the corpus still missing.
		if os.Getenv("ORDER_RELEASE_GATE") != "" {
			t.Fatal("gen-3 corpus missing and ORDER_RELEASE_GATE is set — record testdata/gen3 before tagging (see testdata/README.md)")
		}
		t.Skip("gen-3 corpus not recorded yet — MUST exist before the next worker build is tagged (see testdata/README.md)")
	}

	replayFiles(t, files)
}

// gen2CarriedForward is the subset of the previous generation this build must
// STILL replay: the histories that carry no product participant.
//
// It exists because "the corpus moved to gen3" would otherwise leave the change
// that most needs a history gate with no history gate at all — gen3 cannot be
// recorded until the build runs somewhere, so between this commit and the
// recording PR the glob above matches nothing and plain `go test` asserts nothing.
// These two files are the measurement that justified the split, kept as an
// assertion instead of a claim in a commit message: an inventory-path saga and a
// cancellation episode replay green, and they must keep doing so.
//
// The list is explicit, not a glob. The other four gen2 histories carry
// `participant=product` and legitimately stop replaying here, so a glob would
// either fail on them or need an exclusion nobody would notice going stale.
var gen2CarriedForward = []string{
	"history_happy_inventory.json",
	"history_cancellation_happy.json",
}

func TestReplayCarriedForwardHistories(t *testing.T) {
	files := make([]string, 0, len(gen2CarriedForward))
	for _, name := range gen2CarriedForward {
		f := filepath.Join("testdata", "gen2", name)
		if _, err := os.Stat(f); err != nil {
			t.Fatalf("carried-forward history %s is missing: %v", f, err)
		}
		files = append(files, f)
	}
	replayFiles(t, files)
}

func replayFiles(t *testing.T, files []string) {
	t.Helper()
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
