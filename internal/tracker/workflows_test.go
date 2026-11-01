package tracker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/mocks"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
)

func testEvent() Event {
	return Event{
		ID: "test", Name: "Test", EndDate: "2026-10-07", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		ClosesAt: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), CompletesAt: time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC),
		Form: func() *Form { f := DefaultForm(); return &f }(),
	}
}

// harness runs an event workflow against an in-memory store, sending Updates at
// one-second steps of workflow time.
type harness struct {
	t     *testing.T
	env   *testsuite.TestWorkflowEnvironment
	store *memStore
	step  time.Duration
	sent  int
}

func newHarness(t *testing.T) *harness {
	suite := testsuite.WorkflowTestSuite{}
	h := &harness{t: t, env: suite.NewTestWorkflowEnvironment(), store: newMemStore()}
	h.env.RegisterActivity(&Activities{Store: h.store})
	h.env.SetStartTime(testEvent().ClosesAt.Add(-time.Hour))
	return h
}

// stored puts an event in the store, as if an earlier run had created it.
func (h *harness) stored(event Event) {
	h.store.events[event.ID] = EventState{Event: event, Epoch: 1}
	h.store.links[event.ID] = event.ID
}

// then runs fn one step after the previous one.
func (h *harness) then(fn func()) {
	h.step += time.Second
	h.env.RegisterDelayedCallback(fn, h.step)
}

type outcome struct {
	done  bool
	value any
	err   error
}

func (h *harness) update(name string, args ...any) *outcome {
	h.sent++
	result := &outcome{}
	h.env.UpdateWorkflow(name, fmt.Sprintf("%s-%d", name, h.sent), &testsuite.TestUpdateCallback{
		OnAccept:   func() {},
		OnReject:   func(err error) { result.done, result.err = true, err },
		OnComplete: func(value any, err error) { result.done, result.value, result.err = true, value, err },
	}, args...)
	return result
}

func (h *harness) run(input EventInput) {
	h.env.ExecuteWorkflow(EventWorkflow, input)
	if err := h.env.GetWorkflowError(); err != nil {
		h.t.Fatal(err)
	}
}

func errorType(err error) string {
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		return app.Type()
	}
	return ""
}

func submission(email, request string) Submission {
	return Submission{Name: "Pat", Role: "Engineer", Email: email, RequestID: request}
}

func TestCreateStoresTheEventAndRunsToCompletion(t *testing.T) {
	h := newHarness(t)
	var created *outcome
	h.then(func() { created = h.update("create", testEvent()) })
	h.run(EventInput{ID: "test"})
	if created == nil || created.err != nil || created.value.(Event).Status != "open" {
		t.Fatalf("create = %+v", created)
	}
	stored := h.store.stored("test")
	if stored.Epoch != 1 || stored.Event.Name != "Test" || h.store.links["test"] != "test" {
		t.Fatalf("stored = %+v", stored)
	}
	var result Event
	if err := h.env.GetWorkflowResult(&result); err != nil || result.Status != "archived" {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestCreateOfATakenIDEndsTheRunWithoutWriting(t *testing.T) {
	h := newHarness(t)
	other := testEvent()
	other.CreatedAt, other.Name = other.CreatedAt.Add(-time.Hour), "Other"
	h.stored(other)
	var created *outcome
	h.then(func() { created = h.update("create", testEvent()) })
	h.run(EventInput{ID: "test"})
	if created == nil || errorType(created.err) != "Taken" {
		t.Fatalf("create = %+v", created)
	}
	if h.store.stored("test").Event.Name != "Other" {
		t.Fatal("a taken create replaced the other event")
	}
}

func TestARunWithoutACreateEnds(t *testing.T) {
	h := newHarness(t)
	h.run(EventInput{ID: "test"})
	if len(h.store.events) != 0 {
		t.Fatal("a run without a create stored something")
	}
}

func TestHydrateClaimsTheStoredEvent(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	var answered Event
	h.then(func() {
		value, err := h.env.QueryWorkflow("event")
		if err != nil || value.Get(&answered) != nil {
			t.Fatal(err)
		}
	})
	h.run(EventInput{ID: "test", Hydrate: true})
	if answered.Name != "Test" || answered.Status != "open" || h.store.stored("test").Epoch != 2 {
		t.Fatalf("answered %+v, epoch %d", answered, h.store.stored("test").Epoch)
	}
	// An event that isn't stored has nothing to run.
	missing := newHarness(t)
	missing.run(EventInput{ID: "missing", Hydrate: true})
}

func TestSubmissionsAreStoredAndRetriesAcknowledged(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	var first, changed, retry, reused, late, lateRetry *outcome
	h.then(func() { first = h.update("submit", submission("p@example.com", "request-000000001")) })
	h.then(func() {
		next := submission("p@example.com", "request-000000002")
		next.Role = "Manager"
		changed = h.update("submit", next)
	})
	h.then(func() { retry = h.update("submit", submission("p@example.com", "request-000000001")) })
	h.then(func() {
		other := submission("p@example.com", "request-000000001")
		other.Name = "Someone else"
		reused = h.update("submit", other)
	})
	// After the event closes, a new response is refused but a retry is still acknowledged.
	h.env.RegisterDelayedCallback(func() {
		late = h.update("submit", submission("late@example.com", "request-000000003"))
		lateRetry = h.update("submit", submission("p@example.com", "request-000000001"))
	}, 2*time.Hour)
	h.run(EventInput{ID: "test", Hydrate: true})
	for name, o := range map[string]*outcome{"first": first, "changed": changed, "retry": retry, "lateRetry": lateRetry} {
		if o == nil || o.err != nil || o.value != "saved" {
			t.Errorf("%s = %+v", name, o)
		}
	}
	if errorType(reused.err) != "Invalid" || errorType(late.err) != "Closed" {
		t.Fatalf("reused = %v, late = %v", reused.err, late.err)
	}
	stored := h.store.stored("test")
	lead := h.store.participants["test"]["p@example.com"]
	if stored.Event.Count != 1 || stored.Sequence != 2 || lead.Role != "Manager" || lead.Sequence != 2 || !lead.FirstSubmittedAt.Before(lead.LastSubmittedAt) {
		t.Fatalf("stored = %+v, lead = %+v", stored, lead)
	}
	if len(h.store.receipts["test"]) != 2 {
		t.Fatalf("receipts = %v", h.store.receipts["test"])
	}
}

func TestInternalOnlyEventTakesOneResponsePerEmployee(t *testing.T) {
	h := newHarness(t)
	event := testEvent()
	event.InternalOnly = true
	h.stored(event)
	var first, second *outcome
	h.then(func() { first = h.update("submit", submission("zoe@temporal.io", "request-000000001")) })
	h.then(func() { second = h.update("submit", submission("zoe@temporal.io", "request-000000002")) })
	h.run(EventInput{ID: "test", Hydrate: true})
	if first.err != nil || errorType(second.err) != "AlreadyResponded" || h.store.stored("test").Event.Count != 1 {
		t.Fatalf("first = %v, second = %v", first.err, second.err)
	}
}

func TestEditsAreStoredBeforeTheyAreAcknowledged(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	revisions := map[string]int64{}
	edit := func(name string, args ...any) {
		h.then(func() {
			result := h.update(name, args...)
			h.env.RegisterDelayedCallback(func() {
				if result.err != nil {
					t.Errorf("%s: %v", name, result.err)
				}
				revisions[name] = h.store.stored("test").Event.Revision
			}, 0)
		})
	}
	subheading := ""
	form := DefaultForm()
	form.SubmitLabel = "Send"
	edit("banner", "Booth 4")
	edit("banner", "Booth 4")
	edit("preview", LinkPreview{Title: "Hi"})
	edit("form", form)
	edit("details", EventDetails{Name: "Renamed", EndDate: "2026-10-07", Subheading: &subheading})
	edit("slug", "renamed")
	edit("end")
	h.run(EventInput{ID: "test", Hydrate: true})
	stored := h.store.stored("test").Event
	if stored.QRBanner != "Booth 4" || stored.Preview == nil || stored.Form.SubmitLabel != "Send" || stored.Name != "Renamed" ||
		stored.Subheading == nil || *stored.Subheading != "" || stored.Slug != "renamed" || stored.EndedAt == nil {
		t.Fatalf("stored = %+v", stored)
	}
	// Six changes; the repeated banner was not one.
	if stored.Revision != 6 || h.store.links["renamed"] != "test" {
		t.Fatalf("revision = %d, links = %v", stored.Revision, h.store.links)
	}
}

func TestDetailsMoveTheCompletionTimer(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	var timers []time.Duration
	h.env.SetOnTimerScheduledListener(func(_ string, d time.Duration) { timers = append(timers, d) })
	var moved, closedMove *outcome
	h.then(func() { moved = h.update("details", EventDetails{Name: "Test", EndDate: "2026-10-09"}) })
	h.env.RegisterDelayedCallback(func() {
		closedMove = h.update("details", EventDetails{Name: "Test", EndDate: "2026-10-20"})
	}, 3*24*time.Hour)
	h.run(EventInput{ID: "test", Hydrate: true})
	if moved.err != nil || errorType(closedMove.err) != "Closed" {
		t.Fatalf("moved = %v, closedMove = %v", moved.err, closedMove.err)
	}
	var result Event
	if err := h.env.GetWorkflowResult(&result); err != nil || !result.CompletesAt.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if len(timers) != 2 {
		t.Fatalf("timers = %v", timers)
	}
}

func TestSlugRefusesATakenLinkAndStopsAtMaxAliases(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	h.store.links["taken"] = "other"
	var taken *outcome
	h.then(func() { taken = h.update("slug", "taken") })
	results := make([]*outcome, MaxAliases+1)
	for i := range results {
		h.then(func() { results[i] = h.update("slug", fmt.Sprintf("link-%d", i)) })
	}
	h.run(EventInput{ID: "test", Hydrate: true})
	if errorType(taken.err) != "Invalid" || h.store.links["taken"] != "other" {
		t.Fatalf("taken = %v", taken.err)
	}
	for i, result := range results[:MaxAliases] {
		if result.err != nil {
			t.Fatalf("link %d: %v", i, result.err)
		}
	}
	if errorType(results[MaxAliases].err) != "Invalid" || len(h.store.stored("test").Event.Aliases) != MaxAliases {
		t.Fatalf("eleventh = %v", results[MaxAliases].err)
	}
}

func TestDeleteStopsLaterChanges(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	var deleted, after *outcome
	h.then(func() { deleted = h.update("delete") })
	h.then(func() { after = h.update("submit", submission("p@example.com", "request-000000001")) })
	h.run(EventInput{ID: "test", Hydrate: true})
	if deleted.err != nil || h.store.stored("test").Event.DeletedAt == nil || errorType(after.err) != "Closed" {
		t.Fatalf("deleted = %v, after = %v", deleted.err, after.err)
	}
}

// A completed event is deleted by a new run that loads it; the delete arrives
// with the start, as Update-with-Start delivers it.
func TestDeletingACompletedEventRunsBriefly(t *testing.T) {
	h := newHarness(t)
	event := testEvent()
	event.CompletesAt = event.ClosesAt.Add(-2 * time.Hour)
	h.stored(event)
	var deleted *outcome
	h.env.RegisterDelayedCallback(func() { deleted = h.update("delete") }, 0)
	h.run(EventInput{ID: "test", Hydrate: true})
	if deleted == nil || deleted.err != nil || h.store.stored("test").Event.DeletedAt == nil {
		t.Fatalf("deleted = %+v", deleted)
	}
}

func TestARunThatLosesTheEventStopsWriting(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	var fenced *outcome
	h.then(func() {
		// Another run takes the event over.
		if _, err := h.store.Claim(context.Background(), "test"); err != nil {
			t.Fatal(err)
		}
		fenced = h.update("banner", "From the old run")
	})
	h.run(EventInput{ID: "test", Hydrate: true})
	if errorType(fenced.err) != "Fenced" || h.store.stored("test").Event.QRBanner != "" {
		t.Fatalf("fenced = %v, stored = %+v", fenced.err, h.store.stored("test").Event)
	}
	// The run ended at once instead of waiting for completion.
	if h.env.Now().After(testEvent().ClosesAt) {
		t.Fatalf("the fenced run kept running until %v", h.env.Now())
	}
}

func TestContinueAsNewReloadsFromTheStore(t *testing.T) {
	h := newHarness(t)
	h.stored(testEvent())
	h.env.SetContinueAsNewSuggested(true)
	h.env.ExecuteWorkflow(EventWorkflow, EventInput{ID: "test", Hydrate: true})
	var continuation *workflow.ContinueAsNewError
	if !errors.As(h.env.GetWorkflowError(), &continuation) {
		t.Fatalf("expected Continue-As-New: %v", h.env.GetWorkflowError())
	}
	var next EventInput
	if err := converter.GetDefaultDataConverter().FromPayloads(continuation.Input, &next); err != nil {
		t.Fatal(err)
	}
	if next != (EventInput{ID: "test", Hydrate: true}) {
		t.Fatalf("next = %+v", next)
	}
}

func TestPausedEventRejectsEveryChangeButKeepsAnswering(t *testing.T) {
	writesPaused.Store(true)
	defer writesPaused.Store(false)
	h := newHarness(t)
	h.stored(testEvent())
	updates := map[string][]any{
		"submit": {submission("p@example.com", "request-000000001")},
		"end":    nil, "delete": nil, "banner": {"Booth 4"},
		"preview": {LinkPreview{Title: "Hi"}}, "form": {DefaultForm()}, "internalOnly": {DefaultForm()},
		"details": {EventDetails{Name: "Renamed", EndDate: "2026-10-07"}}, "slug": {"renamed"},
	}
	results := map[string]*outcome{}
	h.then(func() {
		for name, args := range updates {
			results[name] = h.update(name, args...)
		}
	})
	h.run(EventInput{ID: "test", Hydrate: true})
	for name, result := range results {
		if errorType(result.err) != "Paused" {
			t.Errorf("%s: %v", name, result.err)
		}
	}
	if h.store.stored("test").Event.Revision != 0 {
		t.Fatal("a paused change was stored")
	}
}

func TestStartOpenEventsStartsEachOpenEvent(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	open, deleted, completed := testEvent(), testEvent(), testEvent()
	deleted.ID, deleted.DeletedAt = "deleted", &now
	completed.ID, completed.CompletesAt = "completed", now.Add(-time.Hour)
	for _, event := range []Event{open, deleted, completed} {
		store.events[event.ID] = EventState{Event: event}
	}
	c := &mocks.Client{}
	c.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(options client.StartWorkflowOptions) bool {
		return options.ID == "event/test" && options.TaskQueue == "events" &&
			options.WorkflowIDConflictPolicy == enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING &&
			options.WorkflowIDReusePolicy == enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
	}), EventType, EventInput{ID: "test", Hydrate: true}).Return(&mocks.WorkflowRun{}, nil).Once()
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(&Activities{Client: c, Store: store, TaskQueue: "events"})
	value, err := env.ExecuteActivity("StartOpenEvents")
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	if err := value.Get(&checked); err != nil || checked != 1 {
		t.Fatalf("checked = %d, %v", checked, err)
	}
	c.AssertExpectations(t)
}

func TestReconcilerKeepsGoingAndContinues(t *testing.T) {
	suite := testsuite.WorkflowTestSuite{}
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{})
	passes := 0
	env.OnActivity("StartOpenEvents", mock.Anything).Return(func(context.Context) (int, error) {
		passes++
		if passes == 2 {
			// A failed pass is logged; the next one tries again.
			return 0, temporal.NewNonRetryableApplicationError("store unavailable", "Unavailable", nil)
		}
		return 1, nil
	})
	env.ExecuteWorkflow(ReconcileWorkflow)
	var continuation *workflow.ContinueAsNewError
	if !errors.As(env.GetWorkflowError(), &continuation) || passes != reconcilePasses {
		t.Fatalf("passes = %d, err = %v", passes, env.GetWorkflowError())
	}
}
