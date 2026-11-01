package tracker

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func TestPausedEventRejectsEveryChangeButKeepsAnswering(t *testing.T) {
	writesPaused.Store(true)
	defer writesPaused.Store(false)
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	updates := map[string][]any{
		"submit": {Submission{Name: "Pat", Role: "Engineer", Email: "p@example.com", RequestID: "12345678-1234-1234"}},
		"end":    nil, "delete": nil, "banner": {"Booth 4"},
		"preview": {LinkPreview{Title: "Hi"}}, "form": {DefaultForm()}, "internalOnly": {DefaultForm()},
		"details": {EventDetails{Name: "Renamed", EndDate: event.EndDate}}, "slug": {"renamed"},
	}
	rejected := map[string]bool{}
	env.RegisterDelayedCallback(func() {
		for name, args := range updates {
			env.UpdateWorkflow(name, name, &testsuite.TestUpdateCallback{
				OnAccept: func() { t.Errorf("%s was accepted while paused", name) },
				OnReject: func(err error) {
					var app *temporal.ApplicationError
					if !errors.As(err, &app) || app.Type() != "Paused" {
						t.Errorf("%s rejected with %v", name, err)
					}
					rejected[name] = true
				},
				OnComplete: func(any, error) {},
			}, args...)
		}
		value, err := env.QueryWorkflow("event")
		if err != nil {
			t.Fatal(err)
		}
		var answered Event
		if err := value.Get(&answered); err != nil || answered.ID != event.ID {
			t.Errorf("event query: %+v %v", answered, err)
		}
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, EventState{Event: event})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if len(rejected) != len(updates) {
		t.Fatalf("rejected %v of %d updates", rejected, len(updates))
	}
}

func TestStateQueryIncludesSequenceAndReceipts(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	event := testEvent()
	env.SetStartTime(event.ClosesAt.Add(-time.Hour))
	want := EventState{Event: event, Sequence: 12, Receipts: []Receipt{{ID: "request", Digest: "digest"}}}
	var got EventState
	env.RegisterDelayedCallback(func() {
		value, err := env.QueryWorkflow("state")
		if err != nil {
			t.Fatal(err)
		}
		if err := value.Get(&got); err != nil {
			t.Fatal(err)
		}
	}, time.Second)
	env.ExecuteWorkflow(EventWorkflow, want)
	if got.Sequence != 12 || !reflect.DeepEqual(got.Receipts, want.Receipts) || got.Event.ID != event.ID {
		t.Fatalf("state = %+v", got)
	}
}

func TestDiffSnapshots(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	base := Snapshot{
		Event:        Event{ID: "e", Name: "E", Count: 2, Revision: 4},
		Sequence:     2,
		Receipts:     []Receipt{{ID: "a", Digest: "1"}, {ID: "b", Digest: "2"}},
		Participants: []Lead{{Email: "a@x.io", Sequence: 1, FirstSubmittedAt: at}, {Email: "b@x.io", Sequence: 2, FirstSubmittedAt: at}},
		Links:        []string{"e", "renamed"},
	}
	clone := func(s Snapshot) Snapshot {
		s.Receipts, s.Participants, s.Links = slices.Clone(s.Receipts), slices.Clone(s.Participants), slices.Clone(s.Links)
		return s
	}
	reordered := clone(base)
	slices.Reverse(reordered.Receipts)
	slices.Reverse(reordered.Participants)
	slices.Reverse(reordered.Links)
	// What only a workflow holds, and receipts older than its window, aren't differences.
	reordered.Event.Status, reordered.Event.Busy, reordered.Event.Shards = "open", true, []string{"shard"}
	reordered.Receipts = append(reordered.Receipts, Receipt{ID: "older", Digest: "0"})
	if differences := DiffSnapshots(base, reordered); len(differences) > 0 {
		t.Fatalf("equivalent snapshots differ: %v", differences)
	}
	changed := clone(base)
	changed.Event.Revision = 5
	changed.Receipts = changed.Receipts[:1]
	changed.Participants[0].Name = "Changed"
	changed.Links = changed.Links[:1]
	want := []string{"event differs", "receipts differ", "participants differs", "links differs"}
	if differences := DiffSnapshots(base, changed); !reflect.DeepEqual(differences, want) {
		t.Fatalf("differences = %v, want %v", differences, want)
	}
	missing := clone(base)
	missing.Participants = missing.Participants[:1]
	if differences := DiffSnapshots(base, missing); len(differences) != 1 || differences[0] != "participants: want 2, archive has 1" {
		t.Fatalf("differences = %v", differences)
	}
}

func TestBackfillWorkflowReportsEachEventAndContinuesPerPage(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})
	env.OnActivity("ListEventWorkflows", mock.Anything, []byte("page-2")).Return(EventWorkflowPage{IDs: []string{"copied", "same", "noted", "off", "broken"}, Next: []byte("page-3")}, nil)
	env.OnActivity("BackfillEvent", mock.Anything, mock.Anything).Return(func(_ context.Context, input BackfillEventInput) (BackfillEventResult, error) {
		switch input.ID {
		case "copied":
			return BackfillEventResult{ID: input.ID, Outcome: OutcomeImported}, nil
		case "same":
			return BackfillEventResult{ID: input.ID, Outcome: OutcomeUnchanged}, nil
		case "noted":
			return BackfillEventResult{ID: input.ID, Outcome: OutcomeImported, Notes: []string{"count was 3; 2 participants found"}}, nil
		case "off":
			return BackfillEventResult{ID: input.ID, Outcome: OutcomeMismatch, Notes: []string{"participants differs"}}, nil
		}
		return BackfillEventResult{}, temporal.NewNonRetryableApplicationError("shard gone", "Missing", nil)
	})
	prior := BackfillReport{Imported: 10}
	env.ExecuteWorkflow(BackfillWorkflow, BackfillInput{PageToken: []byte("page-2"), Report: prior})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) {
		t.Fatalf("expected Continue-As-New: %v", env.GetWorkflowError())
	}
	var next BackfillInput
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &next); err != nil {
		t.Fatal(err)
	}
	report := next.Report
	if string(next.PageToken) != "page-3" || report.Imported != 12 || report.Unchanged != 1 || len(report.Noted) != 1 || report.Noted[0].ID != "noted" {
		t.Fatalf("next = %+v", next)
	}
	if len(report.Problems) != 2 || report.Problems[0].Outcome != OutcomeMismatch || report.Problems[1].ID != "broken" || report.Problems[1].Outcome != OutcomeFailed {
		t.Fatalf("problems = %+v", report.Problems)
	}
}

func TestBackfillWorkflowVerifyOnlyFinishesOnTheLastPage(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})
	env.OnActivity("ListEventWorkflows", mock.Anything, mock.Anything).Return(EventWorkflowPage{IDs: []string{"one"}}, nil)
	env.OnActivity("BackfillEvent", mock.Anything, BackfillEventInput{ID: "one", VerifyOnly: true}).Return(BackfillEventResult{ID: "one", Outcome: OutcomeVerified}, nil).Once()
	env.ExecuteWorkflow(BackfillWorkflow, BackfillInput{VerifyOnly: true})
	var report BackfillReport
	if err := env.GetWorkflowResult(&report); err != nil {
		t.Fatal(err)
	}
	if report.Verified != 1 || len(report.Problems) != 0 {
		t.Fatalf("report = %+v", report)
	}
}

// queryAnswers makes a query on workflowID return value, stored through the result pointer.
func queryAnswers[T any](c *mocks.Client, workflowID, name string, value T, args ...any) {
	result := &mocks.Value{}
	result.On("Get", mock.Anything).Run(func(call mock.Arguments) { *call[0].(*T) = value }).Return(nil)
	c.On("QueryWorkflow", append([]any{mock.Anything, workflowID, "", name}, args...)...).Return(result, nil)
}

func TestExportEventReconcilesShardsCountAndAliases(t *testing.T) {
	c := &mocks.Client{}
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	event := Event{
		ID: "summit", Name: "Summit", Count: 3, Revision: 9, Busy: true, Status: "open",
		Shards: []string{ShardID("summit", 0), ShardID("summit", 1)}, Slug: "kept", Aliases: []string{"kept", "gone", "taken"},
	}
	receipts := []Receipt{{ID: "r1", Digest: "d1"}}
	queryAnswers(c, EventID("summit"), "state", EventState{Event: event, Sequence: 3, Receipts: receipts})
	moved := Lead{Email: "moved@x.io", Sequence: 1, FirstSubmittedAt: at}
	queryAnswers(c, ShardID("summit", 0), "page", ShardPage{Leads: []Lead{{Email: "a@x.io", Sequence: 2, FirstSubmittedAt: at}, moved}, Total: 2}, 0, PageSize)
	// The relocated copy has the newer sequence.
	relocated := moved
	relocated.Sequence, relocated.Name = 3, "Moved"
	queryAnswers(c, ShardID("summit", 1), "page", ShardPage{Leads: []Lead{relocated}, Total: 1}, 0, PageSize)
	queryAnswers(c, EventID("kept"), "event", Event{ID: "summit"})
	c.On("QueryWorkflow", mock.Anything, EventID("gone"), "", "event").Return(nil, serviceerror.NewNotFound("gone"))
	queryAnswers(c, EventID("taken"), "event", Event{ID: "other"})
	snapshot, notes, err := ExportEvent(context.Background(), c, "summit")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Event.Count != 2 || snapshot.Event.Busy || snapshot.Event.Shards != nil || snapshot.Event.Status != "" || snapshot.Sequence != 3 || !reflect.DeepEqual(snapshot.Receipts, receipts) {
		t.Fatalf("event = %+v", snapshot)
	}
	if !reflect.DeepEqual(snapshot.Event.Aliases, event.Aliases) {
		t.Fatalf("the event keeps every alias it recorded: %v", snapshot.Event.Aliases)
	}
	byEmail := map[string]Lead{}
	for _, lead := range snapshot.Participants {
		byEmail[lead.Email] = lead
	}
	if len(byEmail) != 2 || byEmail["moved@x.io"].Name != "Moved" {
		t.Fatalf("participants = %+v", snapshot.Participants)
	}
	if want := []string{"summit", "kept", "gone"}; !reflect.DeepEqual(snapshot.Links, want) {
		t.Fatalf("links = %v, want %v", snapshot.Links, want)
	}
	if len(notes) != 5 {
		t.Fatalf("notes = %q", notes)
	}
}

func TestExportEventFailsWhenAShardIsUnreadable(t *testing.T) {
	c := &mocks.Client{}
	queryAnswers(c, EventID("summit"), "state", EventState{Event: Event{ID: "summit", Shards: []string{ShardID("summit", 0)}}})
	c.On("QueryWorkflow", mock.Anything, ShardID("summit", 0), "", "page", 0, PageSize).Return(nil, serviceerror.NewNotFound("expired"))
	if _, _, err := ExportEvent(context.Background(), c, "summit"); err == nil {
		t.Fatal("an unreadable shard must fail the export, not drop its participants")
	}
}
