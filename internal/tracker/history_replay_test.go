package tracker

import (
	"os"
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/worker"
)

func newReplayer() worker.WorkflowReplayer {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	replayer.RegisterWorkflow(EventAliasWorkflow)
	return replayer
}

func TestReplayCurrentEventHistory(t *testing.T) {
	if err := newReplayer().ReplayWorkflowHistoryFromJSONFile(nil, "testdata/current-event-history.json"); err != nil {
		t.Fatal(err)
	}
}

// TEMPORAL_REPLAY_HISTORY holds one or more downloaded history files, separated like PATH.
func TestReplayRecordedHistory(t *testing.T) {
	paths := filepath.SplitList(os.Getenv("TEMPORAL_REPLAY_HISTORY"))
	if len(paths) == 0 {
		t.Skip("set TEMPORAL_REPLAY_HISTORY to replay downloaded workflow histories")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if err := newReplayer().ReplayWorkflowHistoryFromJSONFile(nil, path); err != nil {
				t.Fatal(err)
			}
		})
	}
}
