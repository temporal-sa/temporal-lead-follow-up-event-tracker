package web

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/store"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// TestIntegration runs the app against a real Temporal server and DynamoDB
// Local, through its HTTP API:
//
//	temporal server start-dev --port 7299 --headless
//	docker run --rm -p 8765:8000 amazon/dynamodb-local:2.5.4 -jar DynamoDBLocal.jar -inMemory -sharedDb
//	TEMPORAL_INTEGRATION_ADDRESS=localhost:7299 DYNAMODB_INTEGRATION_ENDPOINT=http://localhost:8765 go test ./internal/web -run TestIntegration -v
func TestIntegration(t *testing.T) {
	address, endpoint := os.Getenv("TEMPORAL_INTEGRATION_ADDRESS"), os.Getenv("DYNAMODB_INTEGRATION_ENDPOINT")
	if address == "" || endpoint == "" {
		t.Skip("set TEMPORAL_INTEGRATION_ADDRESS and DYNAMODB_INTEGRATION_ENDPOINT")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "local")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local")
	t.Setenv("AWS_REGION", "us-west-1")
	ctx := context.Background()
	var random [4]byte
	_, _ = rand.Read(random[:])
	run := hex.EncodeToString(random[:])
	events, err := store.Open(ctx, store.Config{Table: "event-leads-integration-" + run, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if err := events.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := client.Dial(client.Options{HostPort: address})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	queue := "event-leads-integration-" + run
	activities := &tracker.Activities{Client: c, Store: events, TaskQueue: queue}
	w := worker.New(c, queue, worker.Options{})
	w.RegisterWorkflow(tracker.EventWorkflow)
	w.RegisterWorkflow(tracker.ReconcileWorkflow)
	w.RegisterActivity(activities)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	h := testHandler(t, NewGateway(events, c, queue))
	origin := "https://leads.tmprl-demo.cloud"
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		response := request(t, h, method, path, body, validSession, origin)
		if response.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, response.Code, want, response.Body.String())
		}
		return response.Body.Bytes()
	}
	decode := func(data []byte, into any) {
		t.Helper()
		if err := json.Unmarshal(data, into); err != nil {
			t.Fatal(err)
		}
	}
	endDate := time.Now().UTC().AddDate(0, 0, 3).Format("2006-01-02")
	name := "Integration " + run
	var first, second tracker.Event
	decode(call("POST", "/api/admin/events", fmt.Sprintf(`{"name":%q,"endDate":%q}`, name, endDate), http.StatusCreated), &first)
	// The same name gets the next free link.
	decode(call("POST", "/api/admin/events", fmt.Sprintf(`{"name":%q,"endDate":%q}`, name, endDate), http.StatusCreated), &second)
	if first.ID != "integration-"+run || second.ID != first.ID+"-2" {
		t.Fatalf("ids = %q, %q", first.ID, second.ID)
	}

	submit := func(slug, email, request, role string) {
		t.Helper()
		call("POST", "/api/events/"+slug+"/participants", fmt.Sprintf(`{"name":"Pat","role":%q,"email":%q,"requestId":%q}`, role, email, request), http.StatusOK)
	}
	submit(first.ID, "a@example.com", "request-0000000001", "Engineer")
	submit(first.ID, "b@example.com", "request-0000000002", "Engineer")
	submit(first.ID, "a@example.com", "request-0000000003", "Manager")
	// A retried request is acknowledged without a second write.
	submit(first.ID, "a@example.com", "request-0000000003", "Manager")

	var renamed tracker.Event
	decode(call("POST", "/api/admin/events/"+first.ID+"/slug", `{"slug":"renamed `+run+`"}`, http.StatusOK), &renamed)
	call("POST", "/api/admin/events/"+second.ID+"/slug", `{"slug":"renamed `+run+`"}`, http.StatusBadRequest)
	submit(renamed.Slug, "c@example.com", "request-0000000004", "SA")

	checkParticipants := func() {
		t.Helper()
		var page struct {
			Leads []tracker.Lead `json:"leads"`
		}
		decode(call("GET", "/api/admin/events/"+first.ID+"/participants", "", http.StatusOK), &page)
		if len(page.Leads) != 3 || page.Leads[0].Email != "a@example.com" || page.Leads[0].Role != "Manager" || page.Leads[2].Email != "c@example.com" {
			t.Fatalf("participants = %+v", page.Leads)
		}
		rows, err := csv.NewReader(strings.NewReader(string(call("GET", "/api/admin/events/"+first.ID+"/export.csv", "", http.StatusOK)))).ReadAll()
		if err != nil || len(rows) != 4 {
			t.Fatalf("csv = %v, %v", rows, err)
		}
	}
	checkParticipants()
	var shown tracker.Event
	decode(call("GET", "/api/admin/events/renamed-"+run, "", http.StatusOK), &shown)
	if shown.ID != first.ID || shown.Count != 3 || shown.Status != "open" {
		t.Fatalf("event = %+v", shown)
	}

	// Lose the workflows, as a namespace reset would. Reads still work, and the
	// reconciler brings the events back from the store.
	for _, id := range []string{first.ID, second.ID} {
		if err := c.TerminateWorkflow(ctx, tracker.EventID(id), "", "simulated reset"); err != nil {
			t.Fatal(err)
		}
	}
	checkParticipants()
	reconciler, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: tracker.ReconcilerWorkflowID + "-" + run, TaskQueue: queue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	}, tracker.ReconcileWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.TerminateWorkflow(ctx, reconciler.GetID(), "", "test finished") }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		described, err := c.DescribeWorkflowExecution(ctx, tracker.EventID(first.ID), "")
		if err == nil && described.WorkflowExecutionInfo.Status == enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the reconciler didn't restart the event: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	submit(first.ID, "d@example.com", "request-0000000005", "PM")
	// An acknowledged request stays acknowledged across the reset.
	submit(first.ID, "a@example.com", "request-0000000003", "Manager")
	decode(call("GET", "/api/admin/events/"+first.ID, "", http.StatusOK), &shown)
	if shown.Count != 4 || shown.Slug != "renamed-"+run {
		t.Fatalf("after reset = %+v", shown)
	}

	call("POST", "/api/admin/events/"+second.ID+"/delete", `{}`, http.StatusOK)
	call("GET", "/api/admin/events/"+second.ID, "", http.StatusNotFound)
	call("GET", "/api/events/missing-"+run, "", http.StatusNotFound)
	var directory struct {
		Events []tracker.Event `json:"events"`
	}
	decode(call("GET", "/api/admin/events", "", http.StatusOK), &directory)
	if len(directory.Events) != 1 || directory.Events[0].ID != first.ID {
		t.Fatalf("directory = %+v", directory.Events)
	}
	for _, id := range []string{first.ID, second.ID} {
		_ = c.TerminateWorkflow(ctx, tracker.EventID(id), "", "test finished")
	}
}
