package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
)

// These tests need DynamoDB Local, for example:
//
//	docker run --rm -p 8765:8000 amazon/dynamodb-local:2.5.4 -jar DynamoDBLocal.jar -inMemory -sharedDb
//	DYNAMODB_INTEGRATION_ENDPOINT=http://localhost:8765 go test ./internal/store
func testTable(t *testing.T) *Table {
	t.Helper()
	endpoint := os.Getenv("DYNAMODB_INTEGRATION_ENDPOINT")
	if endpoint == "" {
		t.Skip("DYNAMODB_INTEGRATION_ENDPOINT is not set")
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "local")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "local")
	t.Setenv("AWS_REGION", "us-west-1")
	ctx := context.Background()
	table, err := Open(ctx, Config{Table: fmt.Sprintf("event-leads-test-%d", time.Now().UnixNano()), Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if err := table.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	// Creating it twice is fine; local development does this on every start.
	if err := table.EnsureTable(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = table.client.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table.name})
	})
	return table
}

func testEvent(id string, created time.Time) tracker.Event {
	subheading := ""
	minimum := 0.0
	form := tracker.DefaultForm()
	form.Sections[0].Fields = append(form.Sections[0].Fields, tracker.Field{ID: "interest", Label: "Interest", Type: "checkboxes", Options: []string{"A", "B"}, Minimum: &minimum})
	return tracker.Event{
		ID: id, Name: "Summit", Description: "Say hello", EndDate: "2099-10-07", CreatedBy: "e@temporal.io", CreatedAt: created,
		ClosesAt: time.Date(2099, 10, 8, 0, 0, 0, 0, time.UTC), CompletesAt: time.Date(2099, 10, 15, 0, 0, 0, 0, time.UTC),
		Form: &form, QRBanner: "Booth 4", Preview: &tracker.LinkPreview{Title: "Hi"}, Subheading: &subheading,
	}
}

func created(t *testing.T, table *Table, id string, at time.Time) tracker.EventState {
	t.Helper()
	state := tracker.EventState{Event: testEvent(id, at), Epoch: 1}
	if err := table.Create(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	return state
}

var createdAt = time.Date(2026, 9, 1, 9, 30, 0, 123456789, time.UTC)

func TestCreateReservesTheIDAndRoundTripsTheEvent(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	state := created(t, table, "summit", createdAt)
	// An Activity retry of the same create succeeds.
	if err := table.Create(ctx, state); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, err := table.Resolve(ctx, "summit")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Summit" || got.Form == nil || len(got.Form.Sections[0].Fields) != len(state.Event.Form.Sections[0].Fields) || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("event = %+v", got)
	}
	if got.Subheading == nil || *got.Subheading != "" {
		t.Fatalf("a blank subheading must stay blank, not become the default: %v", got.Subheading)
	}
	// Another event with the same ID, such as one created after the first completed, is refused.
	other := tracker.EventState{Event: testEvent("summit", createdAt.Add(time.Hour)), Epoch: 1}
	if err := table.Create(ctx, other); !errors.Is(err, tracker.ErrTaken) {
		t.Fatalf("err = %v", err)
	}
	if _, err := table.Resolve(ctx, "missing"); !errors.Is(err, tracker.ErrNotStored) {
		t.Fatalf("err = %v", err)
	}
}

func TestClaimFencesOutTheEarlierRun(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	first := created(t, table, "summit", createdAt)
	second, err := table.Claim(ctx, "summit")
	if err != nil || second.Epoch != 2 || second.Event.Name != "Summit" {
		t.Fatalf("claim = %+v, %v", second, err)
	}
	stale := first
	stale.Event.Name, stale.Event.Revision = "Stale", 1
	if err := table.SaveEvent(ctx, stale); !errors.Is(err, tracker.ErrFenced) {
		t.Fatalf("a fenced run wrote: %v", err)
	}
	next := second
	next.Event.Name, next.Event.Revision = "Renamed", 1
	if err := table.SaveEvent(ctx, next); err != nil {
		t.Fatal(err)
	}
	// The same write again is an Activity retry; an older revision is not.
	if err := table.SaveEvent(ctx, next); err != nil {
		t.Fatalf("retry: %v", err)
	}
	older := next
	older.Event.Revision = 0
	if err := table.SaveEvent(ctx, older); !errors.Is(err, tracker.ErrFenced) {
		t.Fatalf("an older revision wrote: %v", err)
	}
	if got, _ := table.Event(ctx, "summit"); got.Name != "Renamed" {
		t.Fatalf("name = %q", got.Name)
	}
	if _, err := table.Claim(ctx, "missing"); !errors.Is(err, tracker.ErrNotStored) {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveSlugReservesTheLink(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	summit := created(t, table, "summit", createdAt)
	created(t, table, "taken", createdAt)
	summit.Event.Slug, summit.Event.Aliases, summit.Event.Revision = "renamed", []string{"renamed"}, 1
	if err := table.SaveSlug(ctx, summit, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := table.SaveSlug(ctx, summit, "renamed"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, slug := range []string{"summit", "renamed"} {
		if got, err := table.Resolve(ctx, slug); err != nil || got.ID != "summit" || got.Slug != "renamed" {
			t.Fatalf("%s resolves to %+v, %v", slug, got, err)
		}
	}
	summit.Event.Revision = 2
	if err := table.SaveSlug(ctx, summit, "taken"); !errors.Is(err, tracker.ErrTaken) {
		t.Fatalf("err = %v", err)
	}
	stale := summit
	stale.Epoch, stale.Event.Revision = 0, 3
	if err := table.SaveSlug(ctx, stale, "fresh"); !errors.Is(err, tracker.ErrFenced) {
		t.Fatalf("err = %v", err)
	}
	if _, err := table.Resolve(ctx, "fresh"); !errors.Is(err, tracker.ErrNotStored) {
		t.Fatalf("a fenced slug write reserved its link: %v", err)
	}
}

func submission(i int, at time.Time) tracker.Lead {
	return tracker.Lead{
		Name: "Person", Role: "Engineer", Email: fmt.Sprintf("p%03d@example.com", i), Sequence: int64(i + 1), RequestID: fmt.Sprintf("request-%d", i),
		FirstSubmittedAt: at, LastSubmittedAt: at,
		// An empty answer and a JSON array, as checkbox grids store them, must survive.
		Answers: []tracker.Answer{{FieldID: "interest", Values: []string{"A", ""}}, {FieldID: "grid", Values: []string{`["x","y"]`}}},
	}
}

func TestSubmissionsPageInFirstSubmittedOrder(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	state := created(t, table, "summit", createdAt)
	// Written newest first, so the order comes from the index, not the writes.
	const total = 7
	for i := total - 1; i >= 0; i-- {
		lead := submission(i, createdAt.Add(time.Duration(i)*time.Minute))
		state.Sequence++
		lead.Sequence = state.Sequence
		state.Event.Count++
		state.Event.Revision++
		receipt := tracker.Receipt{ID: lead.RequestID, Digest: lead.Digest()}
		if err := table.SaveSubmission(ctx, state, lead, receipt); err != nil {
			t.Fatal(err)
		}
		if err := table.SaveSubmission(ctx, state, lead, receipt); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	event, err := table.Event(ctx, "summit")
	if err != nil || event.Count != total || event.Revision != total || event.Form == nil {
		t.Fatalf("event = %+v, %v", event, err)
	}
	var emails []string
	for cursor, pages := "", 0; ; pages++ {
		leads, next, err := table.Participants(ctx, "summit", cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, lead := range leads {
			emails = append(emails, lead.Email)
		}
		if next == "" {
			break
		}
		if pages > total {
			t.Fatal("paging didn't end")
		}
		cursor = next
	}
	want := []string{"p000@example.com", "p001@example.com", "p002@example.com", "p003@example.com", "p004@example.com", "p005@example.com", "p006@example.com"}
	if !slices.Equal(emails, want) {
		t.Fatalf("emails = %v", emails)
	}
	lead, err := table.Participant(ctx, "summit", "p003@example.com")
	if err != nil || lead == nil || lead.Answers[0].Values[1] != "" || lead.Answers[1].Values[0] != `["x","y"]` {
		t.Fatalf("lead = %+v, %v", lead, err)
	}
	existing, err := table.Lookup(ctx, "summit", "p003@example.com", "request-3")
	if err != nil || !existing.Found || existing.Receipt == nil || existing.Receipt.Digest != lead.Digest() || existing.Digest != lead.Digest() {
		t.Fatalf("existing = %+v, %v", existing, err)
	}
	if missing, err := table.Lookup(ctx, "summit", "nobody@example.com", "request-x"); err != nil || missing.Found || missing.Receipt != nil {
		t.Fatalf("missing = %+v, %v", missing, err)
	}
	// An older sequence for the same person is refused, as is a fenced run.
	stale := submission(3, createdAt)
	stale.Sequence, stale.Name = 1, "Stale"
	state.Event.Revision++
	if err := table.SaveSubmission(ctx, state, stale, tracker.Receipt{ID: "request-stale", Digest: "d"}); !errors.Is(err, tracker.ErrFenced) {
		t.Fatalf("err = %v", err)
	}
	fenced := state
	fenced.Epoch = 9
	if err := table.SaveSubmission(ctx, fenced, submission(8, createdAt), tracker.Receipt{ID: "request-8", Digest: "d"}); !errors.Is(err, tracker.ErrFenced) {
		t.Fatalf("err = %v", err)
	}
	if lead, _ := table.Participant(ctx, "summit", "p008@example.com"); lead != nil {
		t.Fatal("a fenced submission was stored")
	}
}

func TestDirectoryAndOpenEvents(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	for i := range 30 {
		created(t, table, fmt.Sprintf("event-%02d", i), createdAt.Add(time.Duration(i)*time.Hour))
	}
	var ids []string
	for cursor := ""; ; {
		events, next, err := table.Events(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			ids = append(ids, event.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(ids) != 30 || ids[0] != "event-29" || ids[29] != "event-00" {
		t.Fatalf("directory = %v", ids)
	}
	// Deleted and completed events need no workflow.
	deleted, err := table.Claim(ctx, "event-01")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	deleted.Event.DeletedAt, deleted.Event.Revision = &now, 1
	if err := table.SaveEvent(ctx, deleted); err != nil {
		t.Fatal(err)
	}
	completed, err := table.Claim(ctx, "event-02")
	if err != nil {
		t.Fatal(err)
	}
	completed.Event.CompletesAt, completed.Event.Revision = now.Add(-time.Hour), 1
	if err := table.SaveEvent(ctx, completed); err != nil {
		t.Fatal(err)
	}
	open, next, err := table.OpenEvents(ctx, "")
	if err != nil || next != "" || len(open) != 28 {
		t.Fatalf("open = %d events, next %q, %v", len(open), next, err)
	}
	for _, event := range open {
		if event.ID == "event-01" || event.ID == "event-02" {
			t.Fatalf("%s is listed as open", event.ID)
		}
	}
}

func TestCursorsBelongToTheirQuery(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	for i := range 30 {
		created(t, table, fmt.Sprintf("event-%02d", i), createdAt.Add(time.Duration(i)*time.Hour))
	}
	_, directory, err := table.Events(ctx, "")
	if err != nil || directory == "" {
		t.Fatalf("next = %q, %v", directory, err)
	}
	for _, cursor := range []string{"not base64!", "e30", directory + "x"} {
		if _, _, err := table.Events(ctx, cursor); !errors.Is(err, tracker.ErrInvalidCursor) {
			t.Errorf("cursor %q: %v", cursor, err)
		}
	}
	if _, _, err := table.Participants(ctx, "event-01", directory, 10); !errors.Is(err, tracker.ErrInvalidCursor) {
		t.Fatalf("a directory cursor read participants: %v", err)
	}
	if _, _, err := table.OpenEvents(ctx, directory); !errors.Is(err, tracker.ErrInvalidCursor) {
		t.Fatalf("a directory cursor read open events: %v", err)
	}
}

func TestHealth(t *testing.T) {
	table := testTable(t)
	if err := table.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	missing := &Table{client: table.client, name: aws.ToString(aws.String("missing-table"))}
	if missing.Health(context.Background()) == nil {
		t.Fatal("a missing table is healthy")
	}
}
