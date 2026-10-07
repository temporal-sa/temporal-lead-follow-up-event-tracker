package tracker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func testEvent() Event {
	return Event{ID: "test", Name: "Test", EndDate: "2026-10-07", ClosesAt: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), CompletesAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC), Shards: []string{}}
}

func TestEventTimersRejectLateSubmissions(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Second))
	checked := false
	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow("event")
		if err != nil {
			t.Fatal(err)
		}
		var e Event
		if err := value.Get(&e); err != nil {
			t.Fatal(err)
		}
		if e.Status != "ended" {
			t.Errorf("status=%s", e.Status)
		}
		env.UpdateWorkflow("submit", "late", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			var app *temporal.ApplicationError
			if !errors.As(err, &app) || app.Type() != "Closed" {
				t.Errorf("late update error=%v", err)
			}
			checked = true
		}}, Submission{Name: "Pat", Role: "Engineer", Email: "p@example.com", RequestID: "12345678-1234-1234"})
	}, 2*time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("late update was not tested")
	}
	var result Event
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "archived" {
		t.Fatal("event did not archive")
	}
	if !env.Now().Equal(event.CompletesAt) {
		t.Fatalf("completion time=%v", env.Now())
	}
}

func TestManualEndKeepsScheduledGrace(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-24 * time.Hour))
	ended := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("end", "end", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if err != nil {
				t.Error(err)
			}
			e := value.(Event)
			if e.Status != "ended" || !e.CompletesAt.Equal(event.CompletesAt) {
				t.Errorf("unexpected event: %+v", e)
			}
			ended = true
		}})
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !ended || !env.Now().Equal(event.CompletesAt) {
		t.Fatal("manual end shortened export window")
	}
}

func TestCoordinatorRoutesDuplicateToOriginalShard(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	event.Shards = []string{"original", "newest"}
	event.Count = 5001
	env.SetStartTime(event.ClosesAt.Add(-24 * time.Hour))
	env.RegisterActivity(&Activities{})
	first := env.Now().Add(-time.Hour)
	env.OnActivity("LocateLead", mock.Anything, mock.Anything).Return(LocatedLead{ShardID: "original", Lead: &Lead{Email: "p@example.com", FirstSubmittedAt: first}}, nil)
	env.OnActivity("WriteLead", mock.Anything, mock.Anything).Return(func(_ context.Context, c WriteCommand) (WriteResult, error) {
		if c.ShardID != "original" || c.Lead.Reason != "Latest" || !c.Lead.FirstSubmittedAt.Equal(first) {
			t.Errorf("bad routing: %+v", c)
		}
		return WriteResult{}, nil
	})
	env.OnSignalExternalWorkflow(mock.Anything, "original", "", "finish", mock.Anything).Return(nil)
	env.OnSignalExternalWorkflow(mock.Anything, "newest", "", "finish", mock.Anything).Return(nil)
	saved := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("submit", "repeat", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			if err != nil {
				t.Error(err)
			}
			saved = true
		}}, Submission{Name: "Pat", Role: "Engineer", Email: " P@EXAMPLE.COM ", Reason: "Latest", RequestID: "12345678-1234-1234"})
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result Event
	_ = env.GetWorkflowResult(&result)
	if !saved || result.Count != 5001 {
		t.Fatal("duplicate changed participant count")
	}
	env.AssertExpectations(t)
}

func TestShardWritesAndFinish(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	lead := Lead{Email: "p@example.com", Name: "First", Sequence: 1}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("write", "first", &testsuite.TestUpdateCallback{OnComplete: func(_ any, err error) {
			if err != nil {
				t.Error(err)
			}
		}}, WriteCommand{Lead: lead})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		lead.Name = "Latest"
		lead.Sequence = 2
		env.UpdateWorkflow("write", "second", &testsuite.TestUpdateCallback{OnComplete: func(_ any, err error) {
			if err != nil {
				t.Error(err)
			}
		}}, WriteCommand{Lead: lead})
	}, 2*time.Second)
	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow("find", lead.Email)
		if err != nil {
			t.Fatal(err)
		}
		var result Lead
		_ = value.Get(&result)
		if result.Name != "Latest" {
			t.Errorf("lead=%+v", result)
		}
		env.SignalWorkflow("finish", nil)
	}, 3*time.Second)
	env.ExecuteWorkflow(ParticipantShardWorkflow, ShardState{EventID: "test", Leads: []Lead{}})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var count int
	_ = env.GetWorkflowResult(&count)
	if count != 1 {
		t.Fatalf("count=%d", count)
	}
}

func TestContinueAsNewPreservesCoordinatorState(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	event.Count = 5001
	event.Shards = []string{"a", "b"}
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	env.SetContinueAsNewSuggested(true)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event, Sequence: 20, Receipts: []Receipt{{ID: "prior-request", Digest: "digest"}}})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatalf("expected Continue-As-New: %v", env.GetWorkflowError())
	}
	var state EventState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	if state.Sequence != 20 || state.Event.Count != 5001 || len(state.Event.Shards) != 2 || len(state.Receipts) != 1 {
		t.Fatalf("lost state: %+v", state)
	}
}

func TestCoordinatorCreatesShardAfterCapacityAndRelocates(t *testing.T) {
	for _, relocating := range []bool{false, true} {
		t.Run(fmt.Sprintf("relocating=%t", relocating), func(t *testing.T) {
			suite := testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			event := testEvent()
			event.Count = MaxParticipants
			event.Shards = []string{"original"}
			env.SetStartTime(event.ClosesAt.Add(-time.Hour))
			env.RegisterWorkflow(ParticipantShardWorkflow)
			env.RegisterActivity(&Activities{})
			located := LocatedLead{}
			if relocating {
				located = LocatedLead{ShardID: "original", Lead: &Lead{Email: "p@example.com", FirstSubmittedAt: env.Now().Add(-time.Hour)}}
			}
			env.OnActivity("LocateLead", mock.Anything, mock.Anything).Return(located, nil)
			writes := 0
			env.OnActivity("WriteLead", mock.Anything, mock.Anything).Return(func(_ context.Context, command WriteCommand) (WriteResult, error) {
				writes++
				if writes == 1 {
					if command.ShardID != "original" {
						t.Errorf("first write=%+v", command)
					}
					return WriteResult{Full: true}, nil
				}
				if writes == 2 && (command.ShardID != ShardID(event.ID, 1) || command.Delete) {
					t.Errorf("new shard write=%+v", command)
				}
				if writes == 3 && (command.ShardID != "original" || !command.Delete) {
					t.Errorf("delete after confirmed write=%+v", command)
				}
				return WriteResult{}, nil
			})
			env.OnSignalExternalWorkflow(mock.Anything, "original", "", "finish", mock.Anything).Return(nil)
			saved := false
			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflow("submit", "shard", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
					if err != nil {
						t.Error(err)
					}
					saved = true
				}}, Submission{Name: "Pat", Role: "Engineer", Email: "p@example.com", RequestID: "12345678-1234-1234"})
			}, time.Second)
			env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result Event
			_ = env.GetWorkflowResult(&result)
			expectedCount, expectedWrites := MaxParticipants+1, 2
			if relocating {
				expectedCount, expectedWrites = MaxParticipants, 3
			}
			if !saved || len(result.Shards) != 2 || result.Count != expectedCount || writes != expectedWrites {
				t.Fatalf("event=%+v writes=%d saved=%t", result, writes, saved)
			}
			env.AssertExpectations(t)
		})
	}
}

func TestShardContinueAsNewKeepsRetryHighWaterMark(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.SetContinueAsNewSuggested(true)
	lead := Lead{Email: "p@example.com", Name: "Pat", Sequence: 42}
	env.ExecuteWorkflow(ParticipantShardWorkflow, ShardState{EventID: "test", Leads: []Lead{lead}, LastSequence: 42})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal(env.GetWorkflowError())
	}
	var state ShardState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	if state.LastSequence != 42 || len(state.Leads) != 1 || state.Leads[0].Name != "Pat" {
		t.Fatalf("state=%+v", state)
	}
}

func TestCoordinatorRecognizesOlderRequestAfterContinuation(t *testing.T) {
	for _, tc := range []struct{ changed, closed bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		changed := tc.changed
		t.Run(fmt.Sprintf("changed=%t closed=%t", changed, tc.closed), func(t *testing.T) {
			suite := testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			event := testEvent()
			event.Count = 1
			start := event.ClosesAt.Add(-time.Hour)
			if tc.closed {
				start = event.ClosesAt.Add(time.Hour)
			}
			env.SetStartTime(start)
			input := Submission{Name: "Older", Role: "Engineer", Email: "p@example.com", RequestID: "12345678-older-request"}
			saved := input.Digest()
			if changed {
				input.Name = "Different"
			}
			checked := false
			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflow("submit", input.RequestID, &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
					if (err != nil) != changed {
						t.Errorf("changed=%t error=%v", changed, err)
					}
					checked = true
				}}, input)
			}, time.Second)
			// This is the state a new run receives, after a newer request was accepted.
			env.ExecuteWorkflow(EventWorkflow, EventState{Event: event, Sequence: 2, Receipts: []Receipt{{ID: input.RequestID, Digest: saved}, {ID: "12345678-newer-request", Digest: "newer"}}})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result Event
			_ = env.GetWorkflowResult(&result)
			if !checked || result.Count != 1 || result.Revision != 0 {
				t.Fatal("old retry mutated event state")
			}
		})
	}
}

func TestShardFinishesAtContinueAsNewBoundary(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterDelayedCallback(func() {
		for i := 1; i < HistoryOperations; i++ {
			env.UpdateWorkflow("write", fmt.Sprint(i), &testsuite.TestUpdateCallback{OnComplete: func(_ any, err error) {
				if err != nil {
					t.Error(err)
				}
			}}, WriteCommand{Lead: Lead{Email: "p@example.com", Sequence: int64(i)}})
		}
		env.SignalWorkflowSkippingWorkflowTask("finish", nil)
		env.UpdateWorkflow("write", "final", &testsuite.TestUpdateCallback{}, WriteCommand{Lead: Lead{Email: "p@example.com", Sequence: HistoryOperations}})
	}, time.Second)
	env.ExecuteWorkflow(ParticipantShardWorkflow, ShardState{EventID: "audit", Leads: []Lead{}})
	var continuation *workflow.ContinueAsNewError
	if errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal("finish signal was lost to Continue-As-New")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
}
