package tracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

// Run against an isolated dev server: TEMPORAL_INTEGRATION_ADDRESS=localhost:7233 go test ./internal/tracker -run TestIntegration -v.
func TestIntegrationShardingRecoveryAndReplay(t *testing.T) {
	address := os.Getenv("TEMPORAL_INTEGRATION_ADDRESS")
	if address == "" {
		t.Skip("set TEMPORAL_INTEGRATION_ADDRESS to run against a local Temporal dev server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := fmt.Sprintf("integration-%d", time.Now().UnixNano())
	newWorker := func() worker.Worker {
		w := worker.New(c, id, worker.Options{})
		w.RegisterWorkflow(EventWorkflow)
		w.RegisterWorkflow(ParticipantShardWorkflow)
		w.RegisterActivity(&Activities{Client: c})
		return w
	}
	w := newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { w.Stop() }()
	initial := ShardID(id, 0)
	leads := make([]Lead, MaxParticipants)
	for i := range leads {
		leads[i] = Lead{Email: fmt.Sprintf("p%d@example.com", i), Name: "Original", Sequence: int64(i + 1), FirstSubmittedAt: time.Now().UTC()}
	}
	_, err = c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: initial, TaskQueue: id}, ParticipantShardWorkflow, ShardState{EventID: id, Leads: leads, LastSequence: MaxParticipants})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CancelWorkflow(context.Background(), initial, "")
	now := time.Now().UTC()
	event := Event{ID: id, Name: "Integration", EndDate: now.Format("2006-01-02"), CreatedAt: now, ClosesAt: now.Add(10 * time.Second), CompletesAt: now.Add(15 * time.Second), Count: MaxParticipants, Shards: []string{initial}}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: EventID(id), TaskQueue: id}, EventWorkflow, EventState{Event: event, Sequence: MaxParticipants})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CancelWorkflow(context.Background(), EventID(id), "")
	submit := func(input Submission) {
		h, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{WorkflowID: EventID(id), UpdateID: input.RequestID, UpdateName: "submit", Args: []any{input}, WaitForStage: client.WorkflowUpdateStageCompleted})
		if err != nil {
			t.Fatal(err)
		}
		var result string
		if err := h.Get(ctx, &result); err != nil {
			t.Fatal(err)
		}
	}
	submit(Submission{Name: "New", Role: "Engineer", Email: "new@example.com", RequestID: "12345678-new-participant"})
	submit(Submission{Name: "Updated", Role: "Engineer", Email: " P1@EXAMPLE.COM ", RequestID: "12345678-repeat-participant"})
	// Kill the worker and force the same code to reconstruct state from history.
	w.Stop()
	w = newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	query, err := c.QueryWorkflow(ctx, EventID(id), "", "event")
	if err != nil {
		t.Fatal(err)
	}
	var current Event
	if err := query.Get(&current); err != nil {
		t.Fatal(err)
	}
	if current.Count != MaxParticipants+1 || len(current.Shards) < 2 {
		t.Fatalf("event=%+v", current)
	}
	for _, shard := range current.Shards {
		defer c.CancelWorkflow(context.Background(), shard, "")
	}
	var completed Event
	if err := run.Get(ctx, &completed); err != nil {
		t.Fatal(err)
	}
	count := 0
	foundUpdated := false
	for _, shard := range completed.Shards {
		for offset := 0; ; {
			q, err := c.QueryWorkflow(ctx, shard, "", "page", offset, PageSize)
			if err != nil {
				t.Fatal(err)
			}
			var page ShardPage
			if err := q.Get(&page); err != nil {
				t.Fatal(err)
			}
			count += len(page.Leads)
			for _, lead := range page.Leads {
				if lead.Email == "p1@example.com" {
					if lead.Name != "Updated" || !lead.FirstSubmittedAt.Equal(leads[1].FirstSubmittedAt) {
						t.Fatalf("dedupe fields/timestamp changed incorrectly: %+v", lead)
					}
					foundUpdated = true
				}
			}
			offset += len(page.Leads)
			if offset == page.Total {
				break
			}
		}
	}
	if count != completed.Count || !foundUpdated {
		t.Fatalf("retained participants=%d expected=%d", count, completed.Count)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	for _, workflowID := range append([]string{EventID(id)}, completed.Shards...) {
		iterator := c.GetWorkflowHistory(ctx, workflowID, "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		history := &history.History{}
		for iterator.HasNext() {
			e, err := iterator.Next()
			if err != nil {
				t.Fatal(err)
			}
			history.Events = append(history.Events, e)
		}
		if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
			t.Fatalf("replay %s: %v", workflowID, err)
		}
	}
}

func TestIntegrationContinueAsNewAndReplay(t *testing.T) {
	address := os.Getenv("TEMPORAL_INTEGRATION_ADDRESS")
	if address == "" {
		t.Skip("set TEMPORAL_INTEGRATION_ADDRESS to run against a local Temporal dev server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := fmt.Sprintf("continuation-%d", time.Now().UnixNano())
	w := worker.New(c, id, worker.Options{})
	w.RegisterWorkflow(EventWorkflow)
	w.RegisterWorkflow(ParticipantShardWorkflow)
	w.RegisterActivity(&Activities{Client: c})
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	now := time.Now().UTC()
	event := Event{ID: id, Name: "Continuation", ClosesAt: now.Add(time.Hour), CompletesAt: now.Add(8 * 24 * time.Hour), Shards: []string{}}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: EventID(id), TaskQueue: id}, EventWorkflow, EventState{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CancelWorkflow(context.Background(), EventID(id), "")
	var initialShardRun string
	// Both Workflows continue as new only when Temporal suggests it, so submit until they have.
	var parentRun, shardRun string
	continued := func() bool {
		parent, err := c.DescribeWorkflowExecution(ctx, EventID(id), "")
		if err != nil {
			t.Fatal(err)
		}
		shard, err := c.DescribeWorkflowExecution(ctx, ShardID(id, 0), "")
		if err != nil {
			t.Fatal(err)
		}
		parentRun, shardRun = parent.WorkflowExecutionInfo.Execution.RunId, shard.WorkflowExecutionInfo.Execution.RunId
		return parentRun != run.GetRunID() && shardRun != initialShardRun
	}
	submitted := 0
	for i := 0; i < 3000 && (i%50 != 0 || i == 0 || !continued()); i++ {
		input := Submission{Name: fmt.Sprintf("Person %d", i), Role: "Engineer", Email: fmt.Sprintf("p%d@example.com", i), RequestID: fmt.Sprintf("continuation-request-%d", i)}
		// A refreshing run can reject admission; retry the same request ID on the new run.
		for {
			h, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{WorkflowID: EventID(id), UpdateID: input.RequestID, UpdateName: "submit", Args: []any{input}, WaitForStage: client.WorkflowUpdateStageCompleted})
			if err == nil {
				var status string
				err = h.Get(ctx, &status)
			}
			if err == nil {
				break
			}
			var app *temporal.ApplicationError
			if !errors.As(err, &app) || app.Type() != "Refreshing" {
				t.Fatal(err)
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		if i == 0 {
			description, err := c.DescribeWorkflowExecution(ctx, ShardID(id, 0), "")
			if err != nil {
				t.Fatal(err)
			}
			initialShardRun = description.WorkflowExecutionInfo.Execution.RunId
			defer c.CancelWorkflow(context.Background(), ShardID(id, 0), "")
		}
		submitted++
	}
	if !continued() {
		t.Fatalf("parent and shard must both Continue-As-New within %d submissions", submitted)
	}
	t.Logf("coordinator and shard both continued as new after %d submissions", submitted)
	query, err := c.QueryWorkflow(ctx, EventID(id), "", "event")
	if err != nil {
		t.Fatal(err)
	}
	var current Event
	if err := query.Get(&current); err != nil {
		t.Fatal(err)
	}
	if current.Count != submitted || len(current.Shards) != 1 {
		t.Fatalf("state lost after continuation: %+v", current)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	for _, execution := range [][2]string{{EventID(id), run.GetRunID()}, {EventID(id), parentRun}, {ShardID(id, 0), initialShardRun}, {ShardID(id, 0), shardRun}} {
		iterator := c.GetWorkflowHistory(ctx, execution[0], execution[1], false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		history := &history.History{}
		for iterator.HasNext() {
			e, err := iterator.Next()
			if err != nil {
				t.Fatal(err)
			}
			history.Events = append(history.Events, e)
		}
		if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
			t.Fatalf("replay %v: %v", execution, err)
		}
	}
}

func TestIntegrationSlugAliasesReserveRouteAndReplay(t *testing.T) {
	address := os.Getenv("TEMPORAL_INTEGRATION_ADDRESS")
	if address == "" {
		t.Skip("set TEMPORAL_INTEGRATION_ADDRESS to run against a local Temporal dev server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := fmt.Sprintf("it%d", time.Now().UnixNano())
	other, alias := id+"-b", id+"-renamed"
	newWorker := func() worker.Worker {
		w := worker.New(c, id, worker.Options{})
		w.RegisterWorkflow(EventWorkflow)
		w.RegisterWorkflow(ParticipantShardWorkflow)
		w.RegisterWorkflow(EventAliasWorkflow)
		return w
	}
	w := newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { w.Stop() }()
	now := time.Now().UTC()
	start := func(slug string, closes, completes time.Duration) client.WorkflowRun {
		event := Event{ID: slug, Name: "Integration", EndDate: now.Format("2006-01-02"), ClosesAt: now.Add(closes), CompletesAt: now.Add(completes), Shards: []string{}}
		run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: EventID(slug), TaskQueue: id, WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}, EventWorkflow, EventState{Event: event})
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	run := start(id, 10*time.Second, 15*time.Second)
	defer c.CancelWorkflow(context.Background(), EventID(id), "")
	start(other, time.Hour, 2*time.Hour)
	defer c.CancelWorkflow(context.Background(), EventID(other), "")
	defer c.CancelWorkflow(context.Background(), EventID(alias), "")
	update := func(slug, name string, arg any) (Event, error) {
		h, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{WorkflowID: EventID(slug), UpdateName: name, Args: []any{arg}, WaitForStage: client.WorkflowUpdateStageCompleted})
		if err != nil {
			return Event{}, err
		}
		var event Event
		err = h.Get(ctx, &event)
		return event, err
	}
	renamed, err := update(id, "slug", alias)
	if err != nil || renamed.Link() != alias || len(renamed.Aliases) != 1 {
		t.Fatalf("rename: %+v %v", renamed, err)
	}
	// Neither the new alias nor the first event's original slug can be taken.
	for _, taken := range []string{alias, id} {
		_, err := update(other, "slug", taken)
		var app *temporal.ApplicationError
		if !errors.As(err, &app) || app.Type() != "Invalid" {
			t.Fatalf("took %q: %v", taken, err)
		}
	}
	if _, err := update(id, "details", EventDetails{Name: "Renamed integration", EndDate: now.Format("2006-01-02")}); err != nil {
		t.Fatal(err)
	}
	// Replace the worker so both workflows rebuild their state from history.
	w.Stop()
	w = newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{id, alias} {
		event, err := LoadEvent(ctx, c, slug)
		if err != nil || event.ID != id || event.Name != "Renamed integration" || event.Link() != alias {
			t.Fatalf("load %q: %+v %v", slug, event, err)
		}
	}
	if err := run.Get(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.GetWorkflow(ctx, EventID(alias), "").Get(ctx, nil); err != nil {
		t.Fatalf("alias did not finish with its event: %v", err)
	}
	// While retained, the completed alias still routes and still reserves its slug.
	if event, err := LoadEvent(ctx, c, alias); err != nil || event.ID != id || event.Status != "archived" {
		t.Fatalf("retained alias: %+v %v", event, err)
	}
	_, err = c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: EventID(alias), TaskQueue: id, WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, WorkflowExecutionErrorWhenAlreadyStarted: true}, EventWorkflow, EventState{Event: Event{ID: alias}})
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if !errors.As(err, &started) {
		t.Fatalf("retained alias slug was reusable: %v", err)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	replayer.RegisterWorkflow(EventAliasWorkflow)
	for _, workflowID := range []string{EventID(id), EventID(alias), EventID(other)} {
		iterator := c.GetWorkflowHistory(ctx, workflowID, "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		history := &history.History{}
		for iterator.HasNext() {
			e, err := iterator.Next()
			if err != nil {
				t.Fatal(err)
			}
			history.Events = append(history.Events, e)
		}
		if err := replayer.ReplayWorkflowHistory(nil, history); err != nil {
			t.Fatalf("replay %s: %v", workflowID, err)
		}
	}
}
