package tracker

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// writesPaused rejects every event change while the storage cut-over runs. Only
// Update validators read it. The SDK doesn't run validators on replay and a
// rejected Update is never recorded, so histories replay the same either way.
var writesPaused atomic.Bool

// PauseWrites makes this worker's events reject changes until it restarts.
func PauseWrites() { writesPaused.Store(true) }

func EventWorkflow(ctx workflow.Context, state EventState) (Event, error) {
	rotating, retiring, rescheduled := false, false, false
	mutex := workflow.NewMutex(ctx)
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second},
	})
	if err := workflow.SetQueryHandler(ctx, "event", func() (Event, error) { return state.Event.At(workflow.Now(ctx)), nil }); err != nil {
		return Event{}, err
	}
	// The backfill reads the sequence and receipts too. Registering an additional
	// handler emits no commands and preserves existing histories.
	if err := workflow.SetQueryHandler(ctx, "state", func() (EventState, error) { return state, nil }); err != nil {
		return Event{}, err
	}
	validator := func() error {
		if writesPaused.Load() {
			return temporal.NewApplicationError("changes are paused for maintenance; retry in a few minutes", "Paused")
		}
		if rotating {
			return temporal.NewApplicationError("event is refreshing; retry shortly", "Refreshing")
		}
		if retiring {
			return temporal.NewApplicationError("event is completing; submissions are closed", "Closed")
		}
		return nil
	}
	// Deleted events stay in history, but updates other than delete itself are rejected.
	accepting := func() error {
		if state.Event.DeletedAt != nil {
			return temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		return validator()
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "submit", func(ctx workflow.Context, input Submission) (string, error) {
		if err := mutex.Lock(ctx); err != nil {
			return "", err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return "", temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		input, err := input.NormalizeForEvent(state.Event)
		if err != nil {
			return "", temporal.NewApplicationError(err.Error(), "Invalid")
		}
		digest := input.Digest()
		// Temporal Update IDs are per run; carry a bounded retry window across runs.
		for _, receipt := range state.Receipts {
			if receipt.ID == input.RequestID {
				if receipt.Digest != digest {
					return "", temporal.NewApplicationError("request ID was already used for different details", "Invalid")
				}
				return "saved", nil
			}
		}
		if state.Event.At(workflow.Now(ctx)).Status != "open" {
			return "", temporal.NewApplicationError("this event is no longer accepting submissions", "Closed")
		}
		state.Event.Busy = true
		defer func() { state.Event.Busy = false }()
		var existing LocatedLead
		if err := workflow.ExecuteActivity(activityCtx, "LocateLead", LocateInput{Shards: state.Event.Shards, Email: input.Email}).Get(ctx, &existing); err != nil {
			return "", err
		}
		existing = existing.Resolved()
		if existing.Found && existing.RequestID == input.RequestID {
			if existing.Digest != digest {
				return "", temporal.NewApplicationError("request ID was already used for different details", "Invalid")
			}
			return "saved", nil
		}
		// An internal-only event takes one response per signed-in employee. A retry of
		// the response itself was acknowledged above.
		if existing.Found && state.Event.InternalOnly {
			return "", temporal.NewApplicationError("you have already responded to this event", "AlreadyResponded")
		}
		state.Sequence++
		state.Event.Revision++
		now := workflow.Now(ctx)
		lead := Lead{Name: input.Name, Role: input.Role, Email: input.Email, Reason: input.Reason, Answers: input.Answers, FirstSubmittedAt: now, LastSubmittedAt: now, Sequence: state.Sequence, RequestID: input.RequestID}
		if existing.Found {
			lead.FirstSubmittedAt = existing.FirstSubmittedAt
		}
		target := existing.ShardID
		if target == "" && len(state.Event.Shards) > 0 {
			target = state.Event.Shards[len(state.Event.Shards)-1]
		}
		createShard := func() (string, error) {
			id := ShardID(state.Event.ID, len(state.Event.Shards))
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: id, ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON})
			child := workflow.ExecuteChildWorkflow(childCtx, ParticipantShardWorkflow, ShardState{EventID: state.Event.ID, Leads: []Lead{}})
			if err := child.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
				return "", err
			}
			state.Event.Shards = append(state.Event.Shards, id)
			return id, nil
		}
		if target == "" {
			target, err = createShard()
			if err != nil {
				return "", err
			}
		}
		var result WriteResult
		if err := workflow.ExecuteActivity(activityCtx, "WriteLead", WriteCommand{ShardID: target, Lead: lead}).Get(ctx, &result); err != nil {
			return "", err
		}
		if result.Full {
			// The original remains readable until its replacement is durably confirmed.
			target, err = createShard()
			if err != nil {
				return "", err
			}
			if err := workflow.ExecuteActivity(activityCtx, "WriteLead", WriteCommand{ShardID: target, Lead: lead}).Get(ctx, &result); err != nil {
				return "", err
			}
			if result.Full {
				return "", temporal.NewApplicationError("participant exceeds shard capacity", "Invalid")
			}
			if existing.ShardID != "" {
				if err := workflow.ExecuteActivity(activityCtx, "WriteLead", WriteCommand{ShardID: existing.ShardID, Lead: lead, Delete: true}).Get(ctx, &result); err != nil {
					return "", err
				}
			}
		}
		if !existing.Found {
			state.Event.Count++
		}
		state.Receipts = append(state.Receipts, Receipt{ID: input.RequestID, Digest: digest})
		if len(state.Receipts) > RecentRequests {
			state.Receipts = state.Receipts[len(state.Receipts)-RecentRequests:]
		}
		state.Event.Revision++
		return "saved", nil
	}, workflow.UpdateHandlerOptions{Validator: func(Submission) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "end", func(ctx workflow.Context) (Event, error) {
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		if state.Event.EndedAt == nil && workflow.Now(ctx).Before(state.Event.ClosesAt) {
			now := workflow.Now(ctx)
			state.Event.EndedAt = &now
			state.Event.Revision++
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: accepting}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "banner", func(ctx workflow.Context, input string) (Event, error) {
		banner, err := NormalizeBanner(input)
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		if state.Event.QRBanner != banner {
			state.Event.QRBanner = banner
			state.Event.Revision++
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(string) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "preview", func(ctx workflow.Context, input LinkPreview) (Event, error) {
		preview, err := input.Normalize()
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		current := LinkPreview{}
		if state.Event.Preview != nil {
			current = *state.Event.Preview
		}
		if current != preview {
			state.Event.Preview = nil
			if preview != (LinkPreview{}) {
				state.Event.Preview = &preview
			}
			state.Event.Revision++
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(LinkPreview) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	// Existing responses keep their answers; exports and the participant table follow the new form.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "form", func(ctx workflow.Context, input Form) (Event, error) {
		form, err := input.Normalize()
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		if state.Event.InternalOnly {
			if err := form.RequireIdentityFields(); err != nil {
				return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
			}
		}
		state.Event.Form = &form
		state.Event.Revision++
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(Form) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	// The form arrives with the flag so the event never requires sign-in without the
	// name and email questions the sign-in answers. There is no way back to public.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "internalOnly", func(ctx workflow.Context, input Form) (Event, error) {
		form, err := input.Normalize()
		if err == nil {
			err = form.RequireIdentityFields()
		}
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		state.Event.Form = &form
		state.Event.InternalOnly = true
		state.Event.Revision++
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(Form) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	// The end date can only move while submissions are open, so an ended event never reopens.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "details", func(ctx workflow.Context, input EventDetails) (Event, error) {
		details, closes, err := input.Normalize()
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		now := workflow.Now(ctx)
		changed := false
		if details.EndDate != state.Event.EndDate {
			if state.Event.At(now).Status != "open" {
				return Event{}, temporal.NewApplicationError("submissions have closed, so the end date can no longer change", "Closed")
			}
			if !closes.After(now) {
				return Event{}, temporal.NewApplicationError(errEndDatePassed.Error(), "Invalid")
			}
			state.Event.EndDate, state.Event.ClosesAt, state.Event.CompletesAt = details.EndDate, closes, closes.AddDate(0, 0, 7)
			rescheduled = true
			changed = true
		}
		if details.Name != state.Event.Name || details.Description != state.Event.Description {
			state.Event.Name, state.Event.Description = details.Name, details.Description
			changed = true
		}
		if details.Subheading != nil && *details.Subheading != state.Event.EffectiveSubheading() {
			state.Event.Subheading = details.Subheading
			changed = true
		}
		if changed {
			state.Event.Revision++
		}
		return state.Event.At(now), nil
	}, workflow.UpdateHandlerOptions{Validator: func(EventDetails) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	// A new slug is reserved by starting an alias workflow with that slug's event ID, so
	// event creation's duplicate check treats it like any other taken slug.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "slug", func(ctx workflow.Context, input string) (Event, error) {
		slug, err := NormalizeSlug(input)
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		if slug == state.Event.Link() {
			return state.Event.At(workflow.Now(ctx)), nil
		}
		if slug != state.Event.ID && !slices.Contains(state.Event.Aliases, slug) {
			if len(state.Event.Aliases) >= MaxAliases {
				return Event{}, temporal.NewApplicationError(fmt.Sprintf("an event can have at most %d additional links", MaxAliases), "Invalid")
			}
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
				WorkflowID: EventID(slug), ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON,
				WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			})
			child := workflow.ExecuteChildWorkflow(childCtx, EventAliasWorkflow, AliasState{EventID: state.Event.ID})
			if err := child.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
				var taken *temporal.ChildWorkflowExecutionAlreadyStartedError
				if errors.As(err, &taken) {
					return Event{}, temporal.NewApplicationError("that event link is already in use", "Invalid")
				}
				return Event{}, err
			}
			state.Event.Aliases = append(state.Event.Aliases, slug)
		}
		state.Event.Slug = slug
		state.Event.Revision++
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(string) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "delete", func(ctx workflow.Context) (Event, error) {
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt == nil {
			now := workflow.Now(ctx)
			state.Event.DeletedAt = &now
			state.Event.Revision++
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: validator}); err != nil {
		return Event{}, err
	}
	// Completion starts Temporal retention; queries and exports remain available.
	// A run whose end date never changes starts exactly one completion timer, as
	// before rescheduling existed, so recorded histories replay unchanged.
	for {
		remaining := state.Event.CompletesAt.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			break
		}
		rescheduled = false
		woke, err := workflow.AwaitWithTimeout(ctx, remaining, func() bool {
			return rescheduled || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
		})
		if err != nil {
			return Event{}, err
		}
		if !woke {
			break
		}
		if workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			rotating = true
			if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
				return Event{}, err
			}
			return Event{}, workflow.NewContinueAsNewError(ctx, EventWorkflow, state)
		}
	}
	retiring = true
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return Event{}, err
	}
	for _, id := range state.Event.Shards {
		if err := workflow.SignalExternalWorkflow(ctx, id, "", "finish", nil).Get(ctx, nil); err != nil {
			return Event{}, err
		}
	}
	// Aliases complete with the event, so every slug leaves retention together.
	for _, slug := range state.Event.Aliases {
		if err := workflow.SignalExternalWorkflow(ctx, EventID(slug), "", "finish", nil).Get(ctx, nil); err != nil {
			return Event{}, err
		}
	}
	return state.Event.At(workflow.Now(ctx)), nil
}

type AliasState struct {
	EventID string `json:"eventId"`
}

// EventAliasWorkflow reserves an extra slug for an event. It answers the event
// query with only its event's ID; LoadEvent follows that to the event itself.
func EventAliasWorkflow(ctx workflow.Context, state AliasState) error {
	if err := workflow.SetQueryHandler(ctx, "event", func() (Event, error) { return Event{ID: state.EventID}, nil }); err != nil {
		return err
	}
	workflow.GetSignalChannel(ctx, "finish").Receive(ctx, nil)
	return nil
}

func ParticipantShardWorkflow(ctx workflow.Context, state ShardState) (int, error) {
	finish := workflow.GetSignalChannel(ctx, "finish")
	rotating := false
	if err := workflow.SetQueryHandler(ctx, "find", func(email string) (*Lead, error) { return state.Find(email), nil }); err != nil {
		return 0, err
	}
	if err := workflow.SetQueryHandler(ctx, "page", func(offset, limit int) (ShardPage, error) {
		if offset < 0 {
			offset = 0
		}
		if offset > len(state.Leads) {
			offset = len(state.Leads)
		}
		if limit < 1 || limit > PageSize {
			limit = PageSize
		}
		end := min(offset+limit, len(state.Leads))
		return ShardPage{Leads: state.Leads[offset:end], Total: len(state.Leads)}, nil
	}); err != nil {
		return 0, err
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "write", func(ctx workflow.Context, command WriteCommand) (WriteResult, error) {
		result, err := state.Apply(command)
		if !result.Full && err == nil {
		}
		return result, err
	}, workflow.UpdateHandlerOptions{Validator: func(WriteCommand) error {
		if rotating {
			return temporal.NewApplicationError("shard is refreshing", "Refreshing")
		}
		if finish.Len() > 0 {
			return temporal.NewApplicationError("shard has finished", "Closed")
		}
		return nil
	}}); err != nil {
		return 0, err
	}
	if err := workflow.Await(ctx, func() bool {
		return finish.Len() > 0 || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
	}); err != nil {
		return 0, err
	}
	rotating = true
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return 0, err
	}
	// Signals are not covered by AllHandlersFinished; drain finish before continuing.
	if finish.ReceiveAsync(nil) {
		return len(state.Leads), nil
	}
	return 0, workflow.NewContinueAsNewError(ctx, ParticipantShardWorkflow, state)
}
