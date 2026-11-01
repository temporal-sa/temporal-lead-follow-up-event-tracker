package store

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
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

func testSnapshot(id string, participants int) tracker.Snapshot {
	created := time.Date(2026, 9, 1, 9, 30, 0, 123456789, time.UTC)
	ended := created.Add(48 * time.Hour)
	subheading := ""
	minimum := 0.0
	form := tracker.DefaultForm()
	form.Sections[0].Fields = append(form.Sections[0].Fields, tracker.Field{ID: "interest", Label: "Interest", Type: "checkboxes", Options: []string{"A", "B"}, Minimum: &minimum})
	snapshot := tracker.Snapshot{
		Event: tracker.Event{
			ID: id, Name: "Summit", Description: "Say hello", EndDate: "2099-10-07", CreatedBy: "e@temporal.io", CreatedAt: created,
			ClosesAt: time.Date(2099, 10, 8, 0, 0, 0, 0, time.UTC), CompletesAt: time.Date(2099, 10, 15, 0, 0, 0, 0, time.UTC),
			EndedAt: &ended, Count: participants, Revision: 7, Form: &form, QRBanner: "Booth 4", Slug: "renamed", Aliases: []string{"renamed"},
			Preview: &tracker.LinkPreview{Title: "Hi"}, Subheading: &subheading,
		},
		Sequence: int64(participants),
		Receipts: []tracker.Receipt{{ID: "request-1", Digest: "digest-1"}},
		Links:    []string{id, "renamed"},
	}
	for i := range participants {
		snapshot.Participants = append(snapshot.Participants, tracker.Lead{
			Name: "Person", Role: "Engineer", Email: fmt.Sprintf("p%03d@example.com", i), Sequence: int64(i + 1), RequestID: fmt.Sprintf("request-%d", i),
			FirstSubmittedAt: created.Add(time.Duration(i) * time.Minute), LastSubmittedAt: created.Add(time.Hour),
			// An empty answer and a JSON array, as checkbox grids store them, must survive.
			Answers: []tracker.Answer{{FieldID: "interest", Values: []string{"A", ""}}, {FieldID: "grid", Values: []string{`["x","y"]`}}},
		})
	}
	return snapshot
}

func TestImportRoundTripsAnEvent(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	// More participants than one batch write holds.
	want := testSnapshot("summit", 60)
	result, err := table.Import(ctx, want)
	if err != nil || result.Unchanged || len(result.LinkConflicts) > 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, err := table.Load(ctx, "summit")
	if err != nil {
		t.Fatal(err)
	}
	if differences := tracker.DiffSnapshots(want, got); len(differences) > 0 {
		t.Fatalf("round trip differs: %v", differences)
	}
	if got.Event.Subheading == nil || *got.Event.Subheading != "" {
		t.Fatalf("a blank subheading must stay blank, not become the default: %v", got.Event.Subheading)
	}
	item := rawEvent(t, table, "summit")
	if item.GSI2PK != openPK || item.GSI1SK != "2026-09-01T09:30:00.123456789Z#summit" {
		t.Fatalf("index keys = %+v", item)
	}
}

func TestImportSkipsAnUnchangedRevision(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	first := testSnapshot("summit", 1)
	if _, err := table.Import(ctx, first); err != nil {
		t.Fatal(err)
	}
	same := testSnapshot("summit", 2)
	result, err := table.Import(ctx, same)
	if err != nil || !result.Unchanged {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, err := table.Load(ctx, "summit")
	if err != nil || len(got.Participants) != 1 {
		t.Fatalf("an unchanged revision was rewritten: %d participants, %v", len(got.Participants), err)
	}
	newer := testSnapshot("summit", 2)
	newer.Event.Revision++
	if result, err := table.Import(ctx, newer); err != nil || result.Unchanged {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, err = table.Load(ctx, "summit")
	if err != nil {
		t.Fatal(err)
	}
	if differences := tracker.DiffSnapshots(newer, got); len(differences) > 0 {
		t.Fatalf("newer revision differs: %v", differences)
	}
}

func TestImportNeverReplacesANewerEvent(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	newer := testSnapshot("summit", 1)
	newer.Event.Revision = 9
	if _, err := table.Import(ctx, newer); err != nil {
		t.Fatal(err)
	}
	older := testSnapshot("summit", 0)
	older.Event.Revision = 8
	if _, err := table.Import(ctx, older); err != ErrArchiveNewer {
		t.Fatalf("err=%v", err)
	}
	// After the cut-over a workflow run claims the event, even at the same revision.
	if _, err := table.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &table.name, Key: key(eventPK("summit"), metaSK),
		UpdateExpression:          aws.String("SET epoch = :one"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":one": &types.AttributeValueMemberN{Value: "1"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := table.Import(ctx, newer); err != ErrArchiveNewer {
		t.Fatalf("claimed event: err=%v", err)
	}
	got, err := table.Load(ctx, "summit")
	if err != nil || len(got.Participants) != 1 || got.Event.Revision != 9 {
		t.Fatalf("the newer event changed: %+v %v", got.Event, err)
	}
}

func TestImportReportsALinkAnotherEventOwns(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	if _, err := table.Import(ctx, testSnapshot("first", 0)); err != nil {
		t.Fatal(err)
	}
	second := testSnapshot("second", 0)
	result, err := table.Import(ctx, second)
	if err != nil || !slices.Equal(result.LinkConflicts, []string{"renamed"}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, err := table.Load(ctx, "second")
	if err != nil || !slices.Equal(got.Links, []string{"second"}) {
		t.Fatalf("links=%v err=%v", got.Links, err)
	}
	first, err := table.Load(ctx, "first")
	if err != nil || !slices.Equal(first.Links, []string{"first", "renamed"}) {
		t.Fatalf("the owner lost its link: %v %v", first.Links, err)
	}
}

func TestDeletedAndCompletedEventsNeedNoWorkflow(t *testing.T) {
	table := testTable(t)
	ctx := context.Background()
	deleted := testSnapshot("deleted", 0)
	now := time.Now()
	deleted.Event.DeletedAt = &now
	completed := testSnapshot("completed", 0)
	completed.Event.CompletesAt = now.Add(-time.Hour)
	for _, snapshot := range []tracker.Snapshot{deleted, completed} {
		if _, err := table.Import(ctx, snapshot); err != nil {
			t.Fatal(err)
		}
		if item := rawEvent(t, table, snapshot.Event.ID); item.GSI2PK != "" {
			t.Errorf("%s is listed as open", snapshot.Event.ID)
		}
	}
	if _, err := table.Load(ctx, "missing"); err != tracker.ErrNotArchived {
		t.Fatalf("err=%v", err)
	}
}

func rawEvent(t *testing.T, table *Table, id string) eventItem {
	t.Helper()
	output, err := table.client.GetItem(context.Background(), &dynamodb.GetItemInput{TableName: &table.name, Key: key(eventPK(id), metaSK), ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	var item eventItem
	if err := unmarshal(output.Item, &item); err != nil {
		t.Fatal(err)
	}
	return item
}
