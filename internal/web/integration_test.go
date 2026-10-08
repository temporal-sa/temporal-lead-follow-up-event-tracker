package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func TestIntegrationExportAfterWorkflowCompletion(t *testing.T) {
	address := os.Getenv("TEMPORAL_INTEGRATION_ADDRESS")
	if address == "" {
		t.Skip("set TEMPORAL_INTEGRATION_ADDRESS to run against a local Temporal dev server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := fmt.Sprintf("export-retention-%d", time.Now().UnixNano())
	newWorker := func() worker.Worker {
		w := worker.New(c, id, worker.Options{})
		w.RegisterWorkflow(tracker.EventWorkflow)
		w.RegisterWorkflow(tracker.ParticipantShardWorkflow)
		w.RegisterActivity(&tracker.Activities{Client: c})
		return w
	}
	w := newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { w.Stop() }()
	now := time.Now().UTC()
	event := tracker.Event{ID: id, Name: "Retained responses", EndDate: now.Format("2006-01-02"), ClosesAt: now.Add(4 * time.Second), CompletesAt: now.Add(6 * time.Second), Shards: []string{}}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: tracker.EventID(id), TaskQueue: id}, tracker.EventWorkflow, tracker.EventState{Event: event})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CancelWorkflow(context.Background(), tracker.EventID(id), "")
	defer c.CancelWorkflow(context.Background(), tracker.ShardID(id, 0), "")
	h := testHandler(t, NewTemporalGateway(c, id))
	response := request(t, h, "POST", "/api/events/"+id+"/participants", `{"name":"Pat","role":"Engineer","email":"pat@example.com","reason":"Follow up","requestId":"retained-export-request"}`, "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("submit: %d %s", response.Code, response.Body.String())
	}
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(time.Until(event.ClosesAt) + 50*time.Millisecond):
	}
	// An idle workflow has no timer at the submission cutoff; HTTP status still closes.
	response = request(t, h, "GET", "/api/events/"+id, "", "", "")
	var public tracker.Event
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &public) != nil || public.Status != "ended" {
		t.Fatalf("status after cutoff: %d %s", response.Code, response.Body.String())
	}
	response = request(t, h, "POST", "/api/events/"+id+"/participants", `{"name":"Late","role":"Engineer","email":"late@example.com","requestId":"late-retained-export-request"}`, "", "")
	if response.Code != http.StatusGone {
		t.Fatalf("late submission: %d %s", response.Code, response.Body.String())
	}
	var completed tracker.Event
	if err := run.Get(ctx, &completed); err != nil {
		t.Fatal(err)
	}
	for _, shard := range completed.Shards {
		if err := c.GetWorkflow(ctx, shard, "").Get(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Rebuild queries from retained histories, rather than the worker's cache.
	w.Stop()
	w = newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	response = request(t, h, "GET", "/api/admin/events/"+id+"/participants", "", validSession, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pat@example.com") {
		t.Fatalf("retained participants: %d %s", response.Code, response.Body.String())
	}
	response = request(t, h, "GET", "/api/admin/events/"+id+"/export.csv", "", validSession, "")
	rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if response.Code != http.StatusOK || err != nil || len(rows) != 2 || len(rows[1]) != 9 || rows[1][5] != "pat@example.com" {
		t.Fatalf("retained export: %d %s error=%v", response.Code, response.Body.String(), err)
	}
	iterator := c.GetWorkflowHistory(ctx, tracker.EventID(id), "", false, enums.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	timers := 0
	for iterator.HasNext() {
		event, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.EventType == enums.EVENT_TYPE_TIMER_STARTED {
			timers++
		}
	}
	if timers != 1 {
		t.Fatalf("expected one completion timer, got %d", timers)
	}
}

func TestIntegrationCustomFormDedupeBannerAndExportAfterWorkerRestart(t *testing.T) {
	address := os.Getenv("TEMPORAL_INTEGRATION_ADDRESS")
	if address == "" {
		t.Skip("set TEMPORAL_INTEGRATION_ADDRESS to run against a local Temporal dev server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := client.DialContext(ctx, client.Options{HostPort: address, Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	queue := fmt.Sprintf("custom-form-%d", time.Now().UnixNano())
	newWorker := func() worker.Worker {
		w := worker.New(c, queue, worker.Options{})
		w.RegisterWorkflow(tracker.EventWorkflow)
		w.RegisterWorkflow(tracker.ParticipantShardWorkflow)
		w.RegisterActivity(&tracker.Activities{Client: c})
		return w
	}
	w := newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { w.Stop() }()
	h := testHandler(t, NewTemporalGateway(c, queue))
	body, err := json.Marshal(tracker.CreateEvent{
		Name: "Custom event", EndDate: time.Now().UTC().Add(24 * time.Hour).Format("2006-01-02"),
		Form: customAPIForm(), QRBanner: "Meet the team.",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, h, "POST", "/api/admin/events", string(body), validSession, "https://leads.tmprl-demo.cloud")
	var event tracker.Event
	if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &event) != nil {
		t.Fatalf("create custom event: %d %s", response.Code, response.Body.String())
	}
	defer c.CancelWorkflow(context.Background(), tracker.EventID(event.ID), "")
	defer c.CancelWorkflow(context.Background(), tracker.ShardID(event.ID, 0), "")
	for i, topic := range []string{"Workflows", "Platform"} {
		body, err = json.Marshal(tracker.Submission{RequestID: fmt.Sprintf("custom-integration-%02d", i), Answers: []tracker.Answer{
			{FieldID: "email", Values: []string{" PAT@EXAMPLE.COM "}},
			{FieldID: "topic", Values: []string{topic}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		response = request(t, h, "POST", "/api/events/"+event.ID+"/participants", string(body), "", "")
		if response.Code != http.StatusOK {
			t.Fatalf("custom submit: %d %s", response.Code, response.Body.String())
		}
	}
	response = request(t, h, "POST", "/api/admin/events/"+event.ID+"/banner", `{"banner":"See you at the next conversation."}`, validSession, "https://leads.tmprl-demo.cloud")
	if response.Code != http.StatusOK {
		t.Fatalf("banner update: %d %s", response.Code, response.Body.String())
	}
	w.Stop()
	w = newWorker()
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	response = request(t, h, "GET", "/api/admin/events/"+event.ID, "", validSession, "")
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &event) != nil || event.Count != 1 || event.QRBanner != "See you at the next conversation." {
		t.Fatalf("replayed event: %d %s", response.Code, response.Body.String())
	}
	response = request(t, h, "GET", "/api/admin/events/"+event.ID+"/export.csv", "", validSession, "")
	rows, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if response.Code != http.StatusOK || err != nil || len(rows) != 2 || len(rows[1]) != 8 || rows[1][3] != "pat@example.com" || rows[1][4] != "" || rows[1][5] != "Platform" {
		t.Fatalf("custom export after restart: %d %s error=%v", response.Code, response.Body.String(), err)
	}
	response = request(t, h, "GET", "/admin/events/"+event.ID+"/qr", "", validSession, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "See you at the next conversation.") {
		t.Fatalf("replayed QR banner: %d %s", response.Code, response.Body.String())
	}
}
