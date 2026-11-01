package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// The backfill copies every event the namespace still holds into the archive,
// then checks the archive against what it copied. It runs before the cut-over,
// as often as needed, and once more after the old workflows are terminated.

const BackfillWorkflowID = "event-leads-backfill"

type BackfillInput struct {
	// VerifyOnly compares the archive with the workflows without writing.
	VerifyOnly bool           `json:"verifyOnly,omitempty"`
	PageToken  []byte         `json:"pageToken,omitempty"`
	Report     BackfillReport `json:"report"`
}

type BackfillReport struct {
	Imported  int `json:"imported"`
	Unchanged int `json:"unchanged"`
	Verified  int `json:"verified"`
	// Problems are events that failed to copy or don't match the archive.
	Problems []BackfillEventResult `json:"problems,omitempty"`
	// Noted are events that copied and matched, with something worth a look.
	Noted []BackfillEventResult `json:"noted,omitempty"`
}

type BackfillEventInput struct {
	ID         string `json:"id"`
	VerifyOnly bool   `json:"verifyOnly,omitempty"`
}

const (
	OutcomeImported  = "imported"
	OutcomeUnchanged = "unchanged"
	OutcomeVerified  = "verified"
	OutcomeMismatch  = "mismatch"
	OutcomeFailed    = "failed"
)

type BackfillEventResult struct {
	ID      string   `json:"id"`
	Outcome string   `json:"outcome"`
	Notes   []string `json:"notes,omitempty"`
}

type EventWorkflowPage struct {
	IDs  []string `json:"ids"`
	Next []byte   `json:"next,omitempty"`
}

func BackfillWorkflow(ctx workflow.Context, input BackfillInput) (BackfillReport, error) {
	listCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 5},
	})
	// An event that still fails after a few attempts is reported, not retried forever.
	eventCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	})
	var page EventWorkflowPage
	if err := workflow.ExecuteActivity(listCtx, "ListEventWorkflows", input.PageToken).Get(ctx, &page); err != nil {
		return input.Report, err
	}
	report := input.Report
	for _, id := range page.IDs {
		var result BackfillEventResult
		err := workflow.ExecuteActivity(eventCtx, "BackfillEvent", BackfillEventInput{ID: id, VerifyOnly: input.VerifyOnly}).Get(ctx, &result)
		if err != nil {
			result = BackfillEventResult{ID: id, Outcome: OutcomeFailed, Notes: []string{err.Error()}}
		}
		switch result.Outcome {
		case OutcomeImported:
			report.Imported++
		case OutcomeUnchanged:
			report.Unchanged++
		case OutcomeVerified:
			report.Verified++
		default:
			report.Problems = append(report.Problems, result)
			continue
		}
		if len(result.Notes) > 0 {
			report.Noted = append(report.Noted, result)
		}
	}
	if len(page.Next) > 0 {
		return report, workflow.NewContinueAsNewError(ctx, BackfillWorkflow, BackfillInput{VerifyOnly: input.VerifyOnly, PageToken: page.Next, Report: report})
	}
	return report, nil
}

// ListEventWorkflows returns one page of event IDs in every status. Earlier runs
// of a continued event are skipped; queries reach its latest run.
func (a *Activities) ListEventWorkflows(ctx context.Context, token []byte) (EventWorkflowPage, error) {
	result, err := a.Client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		PageSize: 100, NextPageToken: token,
		Query: "WorkflowType = '" + EventType + "' AND ExecutionStatus != 'ContinuedAsNew'",
	})
	if err != nil {
		return EventWorkflowPage{}, err
	}
	page := EventWorkflowPage{IDs: []string{}, Next: result.NextPageToken}
	for _, execution := range result.Executions {
		id := strings.TrimPrefix(execution.GetExecution().GetWorkflowId(), "event/")
		if !slices.Contains(page.IDs, id) {
			page.IDs = append(page.IDs, id)
		}
	}
	return page, nil
}

func (a *Activities) BackfillEvent(ctx context.Context, input BackfillEventInput) (BackfillEventResult, error) {
	if a.Archive == nil {
		return BackfillEventResult{}, temporal.NewNonRetryableApplicationError("the worker has no archive configured", "Invalid", nil)
	}
	source, notes, err := ExportEvent(ctx, a.Client, input.ID)
	if err != nil {
		return BackfillEventResult{}, err
	}
	result := BackfillEventResult{ID: input.ID, Outcome: OutcomeVerified, Notes: notes}
	if !input.VerifyOnly {
		imported, err := a.Archive.Import(ctx, source)
		if err != nil {
			return BackfillEventResult{}, err
		}
		result.Outcome = OutcomeImported
		if imported.Unchanged {
			result.Outcome = OutcomeUnchanged
		}
		for _, slug := range imported.LinkConflicts {
			result.Notes = append(result.Notes, fmt.Sprintf("link %q already belongs to another archived event; not copied", slug))
			source.Links = slices.DeleteFunc(source.Links, func(link string) bool { return link == slug })
		}
	}
	archived, err := a.Archive.Load(ctx, input.ID)
	if errors.Is(err, ErrNotArchived) {
		result.Outcome = OutcomeMismatch
		result.Notes = append(result.Notes, "not in the archive")
		return result, nil
	}
	if err != nil {
		return BackfillEventResult{}, err
	}
	if differences := DiffSnapshots(source, archived); len(differences) > 0 {
		result.Outcome = OutcomeMismatch
		result.Notes = append(result.Notes, differences...)
	}
	return result, nil
}

// ExportEvent reads an event, its participants, and its links from its workflows.
// Notes describe anything the export had to reconcile.
func ExportEvent(ctx context.Context, c client.Client, id string) (Snapshot, []string, error) {
	var notes []string
	var state EventState
	if err := query(ctx, c, EventID(id), "state", &state); err != nil {
		return Snapshot{}, nil, fmt.Errorf("read event %s: %w", id, err)
	}
	if state.Event.Busy {
		notes = append(notes, "a submission was in progress")
	}
	// A relocation briefly leaves a participant in two shards; the newest write wins.
	participants := map[string]Lead{}
	for _, shard := range state.Event.Shards {
		for offset := 0; ; {
			var page ShardPage
			if err := query(ctx, c, shard, "page", &page, offset, PageSize); err != nil {
				return Snapshot{}, nil, fmt.Errorf("read %s: %w", shard, err)
			}
			for _, lead := range page.Leads {
				if existing, ok := participants[lead.Email]; ok {
					notes = append(notes, fmt.Sprintf("participant %s is in more than one shard; kept the newest", lead.Email))
					if existing.Sequence >= lead.Sequence {
						continue
					}
				}
				participants[lead.Email] = lead
			}
			offset += len(page.Leads)
			if len(page.Leads) == 0 || offset >= page.Total {
				break
			}
		}
	}
	snapshot := Snapshot{Event: state.Event, Sequence: state.Sequence, Receipts: state.Receipts, Links: []string{id}}
	for _, lead := range participants {
		snapshot.Participants = append(snapshot.Participants, lead)
	}
	if snapshot.Event.Count != len(participants) {
		notes = append(notes, fmt.Sprintf("count was %d; %d participants found", snapshot.Event.Count, len(participants)))
		snapshot.Event.Count = len(participants)
	}
	for _, slug := range state.Event.Aliases {
		var alias Event
		err := query(ctx, c, EventID(slug), "event", &alias)
		var missing *serviceerror.NotFound
		switch {
		case errors.As(err, &missing):
			notes = append(notes, fmt.Sprintf("alias %q is no longer retained; copied anyway", slug))
		case err != nil:
			return Snapshot{}, nil, fmt.Errorf("read alias %s: %w", slug, err)
		case alias.ID != id:
			notes = append(notes, fmt.Sprintf("alias %q now routes to %q; not copied", slug, alias.ID))
			continue
		}
		snapshot.Links = append(snapshot.Links, slug)
	}
	snapshot.Event.Status, snapshot.Event.Busy, snapshot.Event.Shards = "", false, nil
	return snapshot, notes, nil
}

func query(ctx context.Context, c client.Client, workflowID, name string, result any, args ...any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	value, err := c.QueryWorkflow(ctx, workflowID, "", name, args...)
	if err != nil {
		return err
	}
	return value.Get(result)
}

// DiffSnapshots names each part of an event that differs between the two.
// Order doesn't matter for participants, receipts, or links.
func DiffSnapshots(want, got Snapshot) []string {
	var differences []string
	compare := func(part string, a, b any) {
		left, _ := json.Marshal(a)
		right, _ := json.Marshal(b)
		if string(left) != string(right) {
			differences = append(differences, part+" differs")
		}
	}
	want, got = want.canonical(), got.canonical()
	compare("event", want.Event, got.Event)
	compare("sequence", want.Sequence, got.Sequence)
	// The archive keeps every receipt; the workflow keeps only the most recent.
	stored := map[string]string{}
	for _, receipt := range got.Receipts {
		stored[receipt.ID] = receipt.Digest
	}
	for _, receipt := range want.Receipts {
		if digest, ok := stored[receipt.ID]; !ok || digest != receipt.Digest {
			differences = append(differences, "receipts differ")
			break
		}
	}
	if len(want.Participants) != len(got.Participants) {
		differences = append(differences, fmt.Sprintf("participants: want %d, archive has %d", len(want.Participants), len(got.Participants)))
	} else {
		compare("participants", want.Participants, got.Participants)
	}
	compare("links", want.Links, got.Links)
	return differences
}

func (s Snapshot) canonical() Snapshot {
	s.Event.Status, s.Event.Busy, s.Event.Shards = "", false, nil
	s.Receipts = slices.SortedFunc(slices.Values(s.Receipts), func(a, b Receipt) int { return strings.Compare(a.ID, b.ID) })
	s.Participants = slices.SortedFunc(slices.Values(s.Participants), func(a, b Lead) int { return strings.Compare(a.Email, b.Email) })
	s.Links = slices.Sorted(slices.Values(s.Links))
	return s
}
