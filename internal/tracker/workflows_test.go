package tracker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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

func TestEventCompletionTimerRejectsLateSubmissions(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Second))
	var timers []time.Duration
	env.SetOnTimerScheduledListener(func(_ string, duration time.Duration) { timers = append(timers, duration) })
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
	if len(timers) != 1 || timers[0] != 7*24*time.Hour+time.Second {
		t.Fatalf("expected only the completion timer, got %v", timers)
	}
}

func TestManualEndKeepsScheduledCompletion(t *testing.T) {
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
		t.Fatal("manual end shortened the scheduled completion deadline")
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
		for i := 1; i < 5; i++ {
			env.UpdateWorkflow("write", fmt.Sprint(i), &testsuite.TestUpdateCallback{OnComplete: func(_ any, err error) {
				if err != nil {
					t.Error(err)
				}
			}}, WriteCommand{Lead: Lead{Email: "p@example.com", Sequence: int64(i)}})
		}
	}, time.Second)
	// Continue-As-New becomes due in the same task that delivers finish, as when the
	// server suggests it on the task that also carries the signal.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflowSkippingWorkflowTask("finish", nil)
		env.UpdateWorkflow("write", "final", &testsuite.TestUpdateCallback{OnAccept: func() { env.SetContinueAsNewSuggested(true) }}, WriteCommand{Lead: Lead{Email: "p@example.com", Sequence: 5}})
	}, 2*time.Second)
	env.ExecuteWorkflow(ParticipantShardWorkflow, ShardState{EventID: "audit", Leads: []Lead{}})
	var continuation *workflow.ContinueAsNewError
	if errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal("finish signal was lost to Continue-As-New")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
}

func TestCustomSubmissionRetryIncludesAnswersAndPreservesDedupe(t *testing.T) {
	// "recorded" is the full-record result in existing histories; "slim" is what LocateLead returns now.
	for _, tc := range []struct {
		format  string
		changed bool
	}{{"recorded", false}, {"recorded", true}, {"slim", false}, {"slim", true}} {
		changed := tc.changed
		t.Run(fmt.Sprintf("%s/changed=%t", tc.format, changed), func(t *testing.T) {
			suite := testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			event := testEvent()
			form := DefaultForm()
			form.Sections[0].Fields = append(form.Sections[0].Fields, Field{ID: "topic", Label: "Topic", Type: "short_text"})
			event.Form = &form
			event.Count = 1
			event.Shards = []string{"original"}
			env.SetStartTime(event.ClosesAt.Add(-time.Hour))
			env.RegisterActivity(&Activities{})
			input := Submission{Name: "Pat", Role: "Engineer", Email: "p@example.com", RequestID: "12345678-1234-1234"}
			normalized, _ := input.NormalizeForEvent(event)
			normalized.Answers = append(normalized.Answers, Answer{FieldID: "topic", Values: []string{"Temporal"}})
			prior := Lead{Name: normalized.Name, Role: normalized.Role, Email: normalized.Email, RequestID: normalized.RequestID, Answers: normalized.Answers}
			located := LocatedLead{ShardID: "original", Lead: &prior}
			if tc.format == "slim" {
				located = LocatedLead{ShardID: "original", Found: true, RequestID: prior.RequestID, FirstSubmittedAt: prior.FirstSubmittedAt, Digest: prior.Digest()}
			}
			env.OnActivity("LocateLead", mock.Anything, mock.Anything).Return(located, nil)
			env.OnSignalExternalWorkflow(mock.Anything, "original", "", "finish", mock.Anything).Return(nil)
			if changed {
				normalized.Answers = append([]Answer(nil), normalized.Answers...)
				normalized.Answers[len(normalized.Answers)-1].Values = []string{"Cloud"}
			}
			checked := false
			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflow("submit", normalized.RequestID, &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
					if (err != nil) != changed {
						t.Errorf("changed=%t error=%v", changed, err)
					}
					checked = true
				}}, normalized)
			}, time.Second)
			env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result Event
			_ = env.GetWorkflowResult(&result)
			if !checked || result.Count != 1 || result.Revision != 0 {
				t.Fatal("retry mutated participant state")
			}
			env.AssertExpectations(t)
		})
	}
}

func TestBannerUpdateAndSchemaSurviveContinueAsNew(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	form := DefaultForm()
	event.Form = &form
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	checked := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("banner", "banner", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if err != nil {
				t.Error(err)
			}
			result := value.(Event)
			if result.QRBanner != "Meet our team" || result.Revision != 1 {
				t.Errorf("banner=%+v", result)
			}
			checked = true
			env.SetContinueAsNewSuggested(true)
		}}, "  Meet our team  ")
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal(env.GetWorkflowError())
	}
	var state EventState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	if !checked || state.Event.QRBanner != "Meet our team" || state.Event.Revision != 1 || !reflect.DeepEqual(state.Event.Form, &form) {
		t.Fatalf("schema/banner lost: %+v", state.Event)
	}
}

func TestPreviewUpdateSurvivesContinueAsNewAndClears(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	steps := []struct {
		input    LinkPreview
		want     *LinkPreview
		revision int64
	}{
		{LinkPreview{Title: "  Booth 7  ", Description: " Say hi "}, &LinkPreview{Title: "Booth 7", Description: "Say hi"}, 1},
		{LinkPreview{Title: "Booth 7", Description: "Say hi"}, &LinkPreview{Title: "Booth 7", Description: "Say hi"}, 1},
		{LinkPreview{}, nil, 2},
		{LinkPreview{Title: "Booth 9"}, &LinkPreview{Title: "Booth 9"}, 3},
	}
	checked := 0
	for i, step := range steps {
		env.RegisterDelayedCallback(func() {
			env.UpdateWorkflow("preview", fmt.Sprintf("preview-%d", i), &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
				if err != nil {
					t.Error(err)
					return
				}
				result := value.(Event)
				if !reflect.DeepEqual(result.Preview, step.want) || result.Revision != step.revision {
					t.Errorf("step %d: preview=%+v revision=%d", i, result.Preview, result.Revision)
				}
				checked++
				if checked == len(steps) {
					env.SetContinueAsNewSuggested(true)
				}
			}}, step.input)
		}, time.Duration(i+1)*time.Second)
	}
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("preview", "too-long", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			var application *temporal.ApplicationError
			if !errors.As(err, &application) || application.Type() != "Invalid" {
				t.Errorf("long preview: %v", err)
			}
		}}, LinkPreview{Title: strings.Repeat("x", 121)})
	}, 500*time.Millisecond)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal(env.GetWorkflowError())
	}
	var state EventState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	if checked != len(steps) || !reflect.DeepEqual(state.Event.Preview, &LinkPreview{Title: "Booth 9"}) || state.Event.Revision != 3 {
		t.Fatalf("preview lost: %+v", state.Event)
	}
}

func TestShardContinueAsNewPreservesCustomAnswers(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.SetContinueAsNewSuggested(true)
	lead := Lead{Email: "p@example.com", Sequence: 2, Answers: []Answer{{FieldID: "topics", Values: []string{"Go", "Cloud"}}}}
	env.ExecuteWorkflow(ParticipantShardWorkflow, ShardState{EventID: "test", Leads: []Lead{lead}, LastSequence: 2})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal(env.GetWorkflowError())
	}
	var state ShardState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Leads) != 1 || !reflect.DeepEqual(state.Leads[0].Answers, lead.Answers) {
		t.Fatalf("custom answers lost: %+v", state)
	}
}

func TestDeleteKeepsTheEventOutOfLaterSubmissions(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	deleted := false
	rejected := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("delete", "delete-event", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if err != nil {
				t.Error(err)
				return
			}
			result := value.(Event)
			if result.DeletedAt == nil || result.Revision != 1 {
				t.Errorf("delete=%+v", result)
			}
			deleted = true
		}})
	}, time.Second)
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("submit", "after-delete", &testsuite.TestUpdateCallback{
			OnReject: func(err error) { rejected = closedUpdate(t, err) },
			OnComplete: func(_ any, err error) {
				if err == nil {
					t.Error("deleted event accepted a submission")
					return
				}
				rejected = closedUpdate(t, err)
			},
		}, Submission{Name: "Pat", Role: "Engineer", Email: "p@example.com", RequestID: "12345678-1234-1234"})
	}, 2*time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result Event
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if !deleted || !rejected || result.DeletedAt == nil || result.Count != 0 {
		t.Fatalf("deleted=%t rejected=%t result=%+v", deleted, rejected, result)
	}
}

func closedUpdate(t *testing.T, err error) bool {
	t.Helper()
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != "Closed" {
		t.Errorf("update error=%v", err)
		return false
	}
	return true
}

func TestFormUpdateReplacesQuestionsAndSurvivesContinueAsNew(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	edited := DefaultForm()
	edited.Sections[0].Fields = append(edited.Sections[0].Fields, Field{ID: "topic", Label: " Topic ", Type: "multiple_choice", Options: []string{"Go"}, AllowOther: true})
	checked := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("form", "invalid", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			if err == nil {
				t.Error("accepted a form without an email question")
			}
		}}, Form{Sections: []Section{{ID: "a", Fields: []Field{{ID: "name", Label: "Name", Type: "short_text"}}}}})
		env.UpdateWorkflow("form", "valid", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if err != nil {
				t.Error(err)
				return
			}
			result := value.(Event)
			if result.Form == nil || len(result.Form.Sections[0].Fields) != 5 || result.Form.Sections[0].Fields[4].Label != "Topic" || result.Revision != 1 {
				t.Errorf("form=%+v", result)
			}
			checked = true
			env.SetContinueAsNewSuggested(true)
		}}, edited)
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal(env.GetWorkflowError())
	}
	var state EventState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	if !checked || state.Event.Form == nil || !state.Event.Form.Sections[0].Fields[4].AllowOther {
		t.Fatalf("edited form lost: %+v", state.Event)
	}
}

func TestInternalOnlyUpdateKeepsIdentityQuestionsAndSurvivesContinueAsNew(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	withoutName := Form{Sections: []Section{{ID: "a", Fields: []Field{{ID: "email", Label: "Email", Type: "email", Required: true}}}}}
	expectInvalid := func(name string) *testsuite.TestUpdateCallback {
		return &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			var application *temporal.ApplicationError
			if !errors.As(err, &application) || application.Type() != "Invalid" {
				t.Errorf("%s: err = %v", name, err)
			}
		}}
	}
	checked := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("internalOnly", "without-name", expectInvalid("internalOnly without a name question"), withoutName)
		env.UpdateWorkflow("internalOnly", "valid", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if result, _ := value.(Event); err != nil || !result.InternalOnly || result.Revision != 1 {
				t.Errorf("internalOnly = %+v, %v", value, err)
			}
		}}, DefaultForm())
		// Once internal-only, a form edit can't drop the name question the sign-in answers.
		env.UpdateWorkflow("form", "drop-name", expectInvalid("form edit without a name question"), withoutName)
		env.UpdateWorkflow("form", "keep-name", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if result, _ := value.(Event); err != nil || !result.InternalOnly {
				t.Errorf("form = %+v, %v", value, err)
			}
			checked = true
			env.SetContinueAsNewSuggested(true)
		}}, DefaultForm())
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal(env.GetWorkflowError())
	}
	var state EventState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	if !checked || !state.Event.InternalOnly {
		t.Fatalf("internal-only lost: %+v", state.Event)
	}
}

func TestDetailsUpdateReschedulesCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, endDate string
		closes        time.Time
	}{
		{"extend", "2026-10-10", time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)},
		{"shorten", "2026-10-06", time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite := testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			event := testEvent()
			event.Shards = []string{"original"}
			env.SetStartTime(event.ClosesAt.Add(-48 * time.Hour))
			var timers []time.Duration
			env.SetOnTimerScheduledListener(func(_ string, duration time.Duration) { timers = append(timers, duration) })
			env.OnSignalExternalWorkflow(mock.Anything, "original", "", "finish", mock.Anything).Return(nil).Once()
			saved := false
			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflow("details", "details", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
					if err != nil {
						t.Error(err)
						return
					}
					e := value.(Event)
					if e.EndDate != tc.endDate || !e.ClosesAt.Equal(tc.closes) || !e.CompletesAt.Equal(tc.closes.AddDate(0, 0, 7)) || e.Status != "open" || e.Revision != 1 {
						t.Errorf("details=%+v", e)
					}
					saved = true
				}}, EventDetails{Name: "Test", EndDate: tc.endDate})
			}, time.Hour)
			// Past the old close time, an extended event still takes submissions.
			checked := tc.name != "extend"
			if !checked {
				env.RegisterDelayedCallback(func() {
					value, err := env.QueryWorkflow("event")
					if err != nil {
						t.Fatal(err)
					}
					var e Event
					_ = value.Get(&e)
					if e.Status != "open" {
						t.Errorf("extended event status=%s", e.Status)
					}
					checked = true
				}, 49*time.Hour)
			}
			env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			if !saved || !checked {
				t.Fatalf("saved=%t checked=%t", saved, checked)
			}
			if want := tc.closes.AddDate(0, 0, 7); !env.Now().Equal(want) {
				t.Fatalf("completed at %v, want %v", env.Now(), want)
			}
			if len(timers) != 2 {
				t.Fatalf("timers=%v", timers)
			}
			env.AssertExpectations(t)
		})
	}
}

func TestDetailsUpdateChangesTextWithoutANewTimer(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	event.Description = "Old welcome"
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	var timers []time.Duration
	env.SetOnTimerScheduledListener(func(_ string, duration time.Duration) { timers = append(timers, duration) })
	revisions := []int64{}
	env.RegisterDelayedCallback(func() {
		for _, details := range []EventDetails{
			{Name: "  Renamed  ", Description: "  New welcome  ", EndDate: event.EndDate},
			{Name: "Renamed", Description: "New welcome", EndDate: event.EndDate},
		} {
			env.UpdateWorkflow("details", "", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
				if err != nil {
					t.Error(err)
					return
				}
				e := value.(Event)
				if e.Name != "Renamed" || e.Description != "New welcome" || !e.CompletesAt.Equal(event.CompletesAt) {
					t.Errorf("details=%+v", e)
				}
				revisions = append(revisions, e.Revision)
			}}, details)
		}
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(revisions, []int64{1, 1}) {
		t.Fatalf("an unchanged save should not bump the revision: %v", revisions)
	}
	if len(timers) != 1 || !env.Now().Equal(event.CompletesAt) {
		t.Fatalf("timers=%v completed=%v", timers, env.Now())
	}
}

func TestDetailsUpdateRejectsInvalidAndClosedChanges(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   string // "", "end", or "delete"
		details EventDetails
		want    string // ApplicationError type, or "" for success
	}{
		{"past end date", "", EventDetails{Name: "Test", EndDate: "2026-10-01"}, "Invalid"},
		{"blank name", "", EventDetails{Name: "  ", EndDate: "2026-10-07"}, "Invalid"},
		{"bad date", "", EventDetails{Name: "Test", EndDate: "10/07/2026"}, "Invalid"},
		{"new date after end", "end", EventDetails{Name: "Test", EndDate: "2026-10-09"}, "Closed"},
		{"rename after end", "end", EventDetails{Name: "Renamed", EndDate: "2026-10-07"}, ""},
		{"rename after delete", "delete", EventDetails{Name: "Renamed", EndDate: "2026-10-07"}, "Closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			suite := testsuite.WorkflowTestSuite{}
			env := suite.NewTestWorkflowEnvironment()
			event := testEvent()
			env.SetStartTime(event.ClosesAt.Add(-24 * time.Hour))
			if tc.setup != "" {
				env.RegisterDelayedCallback(func() {
					env.UpdateWorkflow(tc.setup, tc.setup, &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
						if err != nil {
							t.Error(err)
						}
					}})
				}, time.Second)
			}
			checked := false
			check := func(err error) {
				var app *temporal.ApplicationError
				if tc.want == "" && err != nil || tc.want != "" && (!errors.As(err, &app) || app.Type() != tc.want) {
					t.Errorf("error=%v, want %q", err, tc.want)
				}
				checked = true
			}
			env.RegisterDelayedCallback(func() {
				env.UpdateWorkflow("details", "details", &testsuite.TestUpdateCallback{OnReject: check, OnComplete: func(_ any, err error) { check(err) }}, tc.details)
			}, 2*time.Second)
			env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var result Event
			_ = env.GetWorkflowResult(&result)
			if !checked || !result.CompletesAt.Equal(event.CompletesAt) {
				t.Fatalf("checked=%t result=%+v", checked, result)
			}
		})
	}
}

func TestDetailsUpdateSurvivesContinueAsNew(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("details", "details", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			if err != nil {
				t.Error(err)
			}
			env.SetContinueAsNewSuggested(true)
		}}, EventDetails{Name: "Renamed", Description: "Hi", EndDate: "2026-10-12"})
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatal(env.GetWorkflowError())
	}
	var state EventState
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &state); err != nil {
		t.Fatal(err)
	}
	e := state.Event
	if e.Name != "Renamed" || e.Description != "Hi" || e.EndDate != "2026-10-12" || !e.CompletesAt.Equal(time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("details lost: %+v", e)
	}
}

func TestSlugUpdateReservesAliasesAndFinishesThem(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	// The real alias runs as a child; the test environment fails completion if the
	// finish signal cannot reach a running alias.
	env.RegisterWorkflow(EventAliasWorkflow)
	var started []string
	env.SetOnChildWorkflowStartedListener(func(info *workflow.Info, _ workflow.Context, args converter.EncodedValues) {
		var state AliasState
		if err := args.Get(&state); err != nil || state.EventID != "test" {
			t.Errorf("alias state=%+v err=%v", state, err)
		}
		started = append(started, info.WorkflowExecution.ID)
	})
	var results []Event
	update := func(slug string, wantErr bool) {
		env.UpdateWorkflow("slug", "", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(value any, err error) {
			if (err != nil) != wantErr {
				t.Errorf("slug %q error=%v", slug, err)
			}
			if err == nil {
				results = append(results, value.(Event))
			}
		}}, slug)
	}
	env.RegisterDelayedCallback(func() {
		update("Renamed!", false) // reserves "renamed"
		update("renamed", false)  // already current
		update("test", false)     // back to the original, no new alias
		update("renamed", false)  // an alias it already holds
		update("admin", true)     // reserved path
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	links := []string{}
	for _, e := range results {
		links = append(links, e.Link())
	}
	if !reflect.DeepEqual(links, []string{"renamed", "renamed", "test", "renamed"}) || !reflect.DeepEqual(started, []string{"event/renamed"}) {
		t.Fatalf("links=%v started=%v", links, started)
	}
	var result Event
	_ = env.GetWorkflowResult(&result)
	if !reflect.DeepEqual(result.Aliases, []string{"renamed"}) || result.Revision != 3 {
		t.Fatalf("result=%+v", result)
	}
}

func TestSlugUpdateStopsAtMaxAliases(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	for i := range MaxAliases {
		event.Aliases = append(event.Aliases, fmt.Sprintf("alias-%d", i))
	}
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	env.RegisterWorkflow(EventAliasWorkflow)
	env.OnSignalExternalWorkflow(mock.Anything, mock.Anything, "", "finish", mock.Anything).Return(nil).Times(MaxAliases)
	checked := false
	env.RegisterDelayedCallback(func() {
		env.UpdateWorkflow("slug", "", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			var app *temporal.ApplicationError
			if !errors.As(err, &app) || app.Type() != "Invalid" {
				t.Errorf("error=%v", err)
			}
			checked = true
		}}, "one-too-many")
		env.UpdateWorkflow("slug", "", &testsuite.TestUpdateCallback{OnReject: func(err error) { t.Error(err) }, OnComplete: func(_ any, err error) {
			if err != nil {
				t.Errorf("switching to a held alias failed: %v", err)
			}
		}}, "alias-3")
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result Event
	_ = env.GetWorkflowResult(&result)
	if !checked || len(result.Aliases) != MaxAliases || result.Link() != "alias-3" {
		t.Fatalf("result=%+v", result)
	}
	env.AssertExpectations(t)
}
