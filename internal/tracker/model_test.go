package tracker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUTCClosingAndCompletion(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.FixedZone("Eastern", -4*3600))
	e, err := NewEvent("id", "employee@temporal.io", CreateEvent{Name: "Conference", EndDate: "2026-10-07"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if e.ClosesAt.Format(time.RFC3339) != "2026-10-08T00:00:00Z" || e.CompletesAt.Format(time.RFC3339) != "2026-10-15T00:00:00Z" {
		t.Fatalf("unexpected deadlines: %+v", e)
	}
	if e.At(e.ClosesAt.Add(-time.Nanosecond)).Status != "open" || e.At(e.ClosesAt).Status != "ended" || e.At(e.CompletesAt).Status != "archived" {
		t.Fatal("incorrect boundary statuses")
	}
	e.EndedAt = &now
	if e.At(now).Status != "ended" || e.CompletesAt.Format(time.RFC3339) != "2026-10-15T00:00:00Z" {
		t.Fatal("manual end changed scheduled completion")
	}
}

func TestSubmissionNormalization(t *testing.T) {
	s, err := (Submission{Name: " Pat ", Role: " Engineer ", Email: " PAT@EXAMPLE.COM ", Reason: " Curious ", RequestID: "12345678-1234-1234"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if s.Email != "pat@example.com" || s.Name != "Pat" || s.Role != "Engineer" || s.Reason != "Curious" {
		t.Fatalf("not normalized: %+v", s)
	}
	for _, email := range []string{"Pat <pat@example.com>", "bad", "a@", ""} {
		s.Email = email
		if _, err := s.Normalize(); err == nil {
			t.Errorf("accepted %q", email)
		}
	}
}

func TestNewEventKeepsOnlyANonEmptyPreview(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)
	for _, input := range []*LinkPreview{nil, {}, {Title: "  ", Description: " \n "}} {
		event, err := NewEvent("id", "employee@temporal.io", CreateEvent{Name: "Conference", EndDate: "2026-10-07", Preview: input}, now)
		if err != nil || event.Preview != nil {
			t.Fatalf("empty preview %+v stored as %+v (%v)", input, event.Preview, err)
		}
	}
	event, err := NewEvent("id", "employee@temporal.io", CreateEvent{Name: "Conference", EndDate: "2026-10-07", Preview: &LinkPreview{Title: "  Booth 7 "}}, now)
	if err != nil || event.Preview == nil || *event.Preview != (LinkPreview{Title: "Booth 7"}) {
		t.Fatalf("preview=%+v err=%v", event.Preview, err)
	}
	if _, err := NewEvent("id", "employee@temporal.io", CreateEvent{Name: "Conference", EndDate: "2026-10-07", Preview: &LinkPreview{Description: strings.Repeat("é", 301)}}, now); err == nil {
		t.Fatal("long preview description accepted")
	}
	data, err := json.Marshal(Event{ID: "id"})
	if err != nil || strings.Contains(string(data), "preview") {
		t.Fatalf("event without a preview serializes one: %s", data)
	}
}
