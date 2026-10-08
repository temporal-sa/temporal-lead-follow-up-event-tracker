package tracker

import (
	"os"
	"testing"

	"go.temporal.io/sdk/worker"
)

func TestReplayLegacyEventHistory(t *testing.T) {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/legacy-event-history.json"); err != nil {
		t.Fatal(err)
	}
}

func TestReplayCurrentEventHistory(t *testing.T) {
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, "testdata/current-event-history.json"); err != nil {
		t.Fatal(err)
	}
}

func TestReplayRecordedHistory(t *testing.T) {
	path := os.Getenv("TEMPORAL_REPLAY_HISTORY")
	if path == "" {
		t.Skip("set TEMPORAL_REPLAY_HISTORY to replay a downloaded workflow history")
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, path); err != nil {
		t.Fatal(err)
	}
}
