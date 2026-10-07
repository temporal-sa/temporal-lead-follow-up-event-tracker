package tracker

import (
	"encoding/json"
	"fmt"
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

func TestShardCapacityAndRetry(t *testing.T) {
	state := ShardState{EventID: "id", Leads: make([]Lead, MaxParticipants)}
	for i := range state.Leads {
		state.Leads[i] = Lead{Email: fmt.Sprintf("%d@x.co", i)}
	}
	result, err := state.Apply(WriteCommand{Lead: Lead{Email: "extra@x.co", Sequence: 1}})
	if err != nil || !result.Full || len(state.Leads) != MaxParticipants {
		t.Fatal("participant 5,001 must not be added")
	}
	state = ShardState{EventID: "id", Leads: []Lead{}}
	newer := WriteCommand{Lead: Lead{Email: "p@example.com", Name: "New", Sequence: 2}}
	if _, err := state.Apply(newer); err != nil {
		t.Fatal(err)
	}
	_, _ = state.Apply(newer)
	_, _ = state.Apply(WriteCommand{Lead: Lead{Email: "p@example.com", Name: "Old", Sequence: 1}})
	if len(state.Leads) != 1 || state.Leads[0].Name != "New" {
		t.Fatal("retry duplicated or overwrote newer lead")
	}
	_, _ = state.Apply(WriteCommand{Lead: Lead{Email: "p@example.com", Sequence: 3}, Delete: true})
	_, _ = state.Apply(newer)
	if len(state.Leads) != 0 {
		t.Fatal("late attempt resurrected relocated lead")
	}
}

func TestShardByteCapacityAndGrowingLead(t *testing.T) {
	state := ShardState{EventID: "id", Leads: []Lead{}}
	for i := 1; ; i++ {
		result, err := state.Apply(WriteCommand{Lead: Lead{Email: fmt.Sprintf("%d@x.co", i), Reason: strings.Repeat("x", 1000), Sequence: int64(i)}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Full {
			break
		}
		if i > MaxParticipants {
			t.Fatal("byte guard never triggered")
		}
	}
	data, _ := json.Marshal(state)
	if len(data) > MaxShardBytes || len(state.Leads) >= MaxParticipants {
		t.Fatal("byte guard must shard before count limit for long text")
	}
	state.Leads[0].Reason = ""
	// Fill the released space, then grow that first participant beyond the budget.
	seq := state.LastSequence + 1
	for i := 0; ; i++ {
		r, _ := state.Apply(WriteCommand{Lead: Lead{Email: fmt.Sprintf("fill%d@x.co", i), Reason: strings.Repeat("z", 100), Sequence: seq}})
		if r.Full {
			break
		}
		seq++
	}
	lead := state.Leads[0]
	lead.Reason = strings.Repeat("y", 1000)
	lead.Sequence = seq + 1
	r, _ := state.Apply(WriteCommand{Lead: lead})
	if !r.Full || state.Leads[0].Reason != "" {
		t.Fatal("growing record must leave original intact until relocated")
	}
}
