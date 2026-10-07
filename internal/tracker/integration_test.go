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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
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
	for i := 0; i <= HistoryOperations; i++ {
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
	}
	parent, err := c.DescribeWorkflowExecution(ctx, EventID(id), "")
	if err != nil {
		t.Fatal(err)
	}
	shard, err := c.DescribeWorkflowExecution(ctx, ShardID(id, 0), "")
	if err != nil {
		t.Fatal(err)
	}
	if parent.WorkflowExecutionInfo.Execution.RunId == run.GetRunID() || shard.WorkflowExecutionInfo.Execution.RunId == initialShardRun {
		t.Fatal("parent and shard must both Continue-As-New")
	}
	query, err := c.QueryWorkflow(ctx, EventID(id), "", "event")
	if err != nil {
		t.Fatal(err)
	}
	var current Event
	if err := query.Get(&current); err != nil {
		t.Fatal(err)
	}
	if current.Count != HistoryOperations+1 || len(current.Shards) != 1 {
		t.Fatalf("state lost after continuation: %+v", current)
	}
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflow(EventWorkflow)
	replayer.RegisterWorkflow(ParticipantShardWorkflow)
	for _, execution := range [][2]string{{EventID(id), run.GetRunID()}, {EventID(id), parent.WorkflowExecutionInfo.Execution.RunId}, {ShardID(id, 0), initialShardRun}, {ShardID(id, 0), shard.WorkflowExecutionInfo.Execution.RunId}} {
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
