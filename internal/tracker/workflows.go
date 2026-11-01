package tracker

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// EventInput starts an event workflow. A run either loads a stored event
// (Hydrate) or waits for the create Update that stores a new one.
type EventInput struct {
	ID      string `json:"id"`
	Hydrate bool   `json:"hydrate,omitempty"`
}

// createTimeout bounds how long a run started for a create Update waits for it.
// Update-with-Start delivers the Update with the start, so this only matters
// when the Update was rejected.
const createTimeout = time.Minute

// writesPaused rejects every event change during maintenance. Only Update
// validators read it; the SDK doesn't run validators on replay and a rejected
// Update is never recorded, so histories replay the same either way.
var writesPaused atomic.Bool

// PauseWrites makes this worker's events reject changes until it restarts.
func PauseWrites() { writesPaused.Store(true) }

// EventWorkflow owns one event. The store holds the event and its participants;
// this workflow is their only writer. It serializes changes, stores each one
// before acknowledging it, and keeps the event's timer.
func EventWorkflow(ctx workflow.Context, input EventInput) (Event, error) {
	var state EventState
	// ready means state holds the stored event. abandoned means this run will
	// never hold it: its create failed, or another run took the event over.
	ready, abandoned := false, false
	rotating, retiring, rescheduled := false, false, false
	mutex := workflow.NewMutex(ctx)
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second},
	})
	closed := func() error { return temporal.NewApplicationError("this event is no longer available", "Closed") }
	if err := workflow.SetQueryHandler(ctx, "event", func() (Event, error) {
		if !ready {
			return Event{ID: input.ID}, nil
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}); err != nil {
		return Event{}, err
	}
	validator := func() error {
		if writesPaused.Load() {
			return temporal.NewApplicationError("changes are paused for maintenance; retry in a few minutes", "Paused")
		}
		if rotating {
			return temporal.NewApplicationError("event is refreshing; retry shortly", "Refreshing")
		}
		if retiring || abandoned {
			return temporal.NewApplicationError("event is completing; submissions are closed", "Closed")
		}
		return nil
	}
	// Deleted events keep their workflow until completion, but only delete itself is accepted.
	accepting := func() error {
		if ready && state.Event.DeletedAt != nil {
			return closed()
		}
		return validator()
	}
	// begin waits until this run holds the event, then takes the lock that keeps
	// changes in order. When it returns nil, the caller must unlock.
	begin := func(ctx workflow.Context) error {
		if err := workflow.Await(ctx, func() bool { return ready || abandoned }); err != nil {
			return err
		}
		if err := mutex.Lock(ctx); err != nil {
			return err
		}
		if abandoned {
			mutex.Unlock()
			return closed()
		}
		return nil
	}
	// persist runs a store write. If another run has taken the event over, this
	// run stops writing and ends once its handlers finish.
	persist := func(ctx workflow.Context, activity string, input any) error {
		err := workflow.ExecuteActivity(activityCtx, activity, input).Get(ctx, nil)
		var app *temporal.ApplicationError
		if errors.As(err, &app) && app.Type() == "Fenced" {
			abandoned = true
		}
		return err
	}
	// save stores next as the event's new revision and adopts it once stored.
	save := func(ctx workflow.Context, next Event) error {
		next.Revision++
		write := EventState{Event: next, Sequence: state.Sequence, Epoch: state.Epoch}
		if err := persist(ctx, "SaveEvent", write); err != nil {
			return err
		}
		state = write
		return nil
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, "create", func(ctx workflow.Context, event Event) (Event, error) {
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		created := EventState{Event: event, Epoch: 1}
		created.Event.Status = ""
		err := workflow.ExecuteActivity(activityCtx, "CreateEvent", created).Get(ctx, nil)
		if err != nil {
			abandoned = true
			var app *temporal.ApplicationError
			if errors.As(err, &app) && app.Type() == "Taken" {
				return Event{}, temporal.NewApplicationError(app.Message(), "Taken")
			}
			return Event{}, err
		}
		state, ready = created, true
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(event Event) error {
		if err := validator(); err != nil {
			return err
		}
		if input.Hydrate || ready || event.ID != input.ID {
			return temporal.NewApplicationError("this run doesn't create that event", "Invalid")
		}
		return nil
	}}); err != nil {
		return Event{}, err
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, "submit", func(ctx workflow.Context, input Submission) (string, error) {
		if err := begin(ctx); err != nil {
			return "", err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return "", closed()
		}
		input, err := input.NormalizeForEvent(state.Event)
		if err != nil {
			return "", temporal.NewApplicationError(err.Error(), "Invalid")
		}
		digest := input.Digest()
		var existing Existing
		if err := workflow.ExecuteActivity(activityCtx, "Lookup", LookupInput{EventID: state.Event.ID, Email: input.Email, RequestID: input.RequestID}).Get(ctx, &existing); err != nil {
			return "", err
		}
		// A retried request is acknowledged without writing again, even after the event closes.
		if existing.Receipt != nil {
			if existing.Receipt.Digest != digest {
				return "", temporal.NewApplicationError("request ID was already used for different details", "Invalid")
			}
			return "saved", nil
		}
		if state.Event.At(workflow.Now(ctx)).Status != "open" {
			return "", temporal.NewApplicationError("this event is no longer accepting submissions", "Closed")
		}
		// Responses copied from workflow histories may have no receipt.
		if existing.Found && existing.RequestID == input.RequestID {
			if existing.Digest != digest {
				return "", temporal.NewApplicationError("request ID was already used for different details", "Invalid")
			}
			return "saved", nil
		}
		// An internal-only event takes one response per signed-in employee.
		if existing.Found && state.Event.InternalOnly {
			return "", temporal.NewApplicationError("you have already responded to this event", "AlreadyResponded")
		}
		now := workflow.Now(ctx)
		write := EventState{Event: state.Event, Sequence: state.Sequence + 1, Epoch: state.Epoch}
		write.Event.Revision++
		lead := Lead{Name: input.Name, Role: input.Role, Email: input.Email, Reason: input.Reason, Answers: input.Answers, FirstSubmittedAt: now, LastSubmittedAt: now, Sequence: write.Sequence, RequestID: input.RequestID}
		if existing.Found {
			lead.FirstSubmittedAt = existing.FirstSubmittedAt
		} else {
			write.Event.Count++
		}
		if err := persist(ctx, "SaveSubmission", SubmissionWrite{State: write, Lead: lead, Receipt: Receipt{ID: input.RequestID, Digest: digest}}); err != nil {
			return "", err
		}
		state = write
		return "saved", nil
	}, workflow.UpdateHandlerOptions{Validator: func(Submission) error { return accepting() }}); err != nil {
		return Event{}, err
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, "end", func(ctx workflow.Context) (Event, error) {
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, closed()
		}
		now := workflow.Now(ctx)
		if state.Event.EndedAt == nil && now.Before(state.Event.ClosesAt) {
			next := state.Event
			next.EndedAt = &now
			if err := save(ctx, next); err != nil {
				return Event{}, err
			}
		}
		return state.Event.At(now), nil
	}, workflow.UpdateHandlerOptions{Validator: accepting}); err != nil {
		return Event{}, err
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, "banner", func(ctx workflow.Context, input string) (Event, error) {
		banner, err := NormalizeBanner(input)
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, closed()
		}
		if state.Event.QRBanner != banner {
			next := state.Event
			next.QRBanner = banner
			if err := save(ctx, next); err != nil {
				return Event{}, err
			}
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(string) error { return accepting() }}); err != nil {
		return Event{}, err
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, "preview", func(ctx workflow.Context, input LinkPreview) (Event, error) {
		preview, err := input.Normalize()
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, closed()
		}
		current := LinkPreview{}
		if state.Event.Preview != nil {
			current = *state.Event.Preview
		}
		if current != preview {
			next := state.Event
			next.Preview = nil
			if preview != (LinkPreview{}) {
				next.Preview = &preview
			}
			if err := save(ctx, next); err != nil {
				return Event{}, err
			}
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(LinkPreview) error { return accepting() }}); err != nil {
		return Event{}, err
	}

	// Existing responses keep their answers; exports and the participant table follow the new form.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "form", func(ctx workflow.Context, input Form) (Event, error) {
		form, err := input.Normalize()
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, closed()
		}
		if state.Event.InternalOnly {
			if err := form.RequireIdentityFields(); err != nil {
				return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
			}
		}
		next := state.Event
		next.Form = &form
		if err := save(ctx, next); err != nil {
			return Event{}, err
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(Form) error { return accepting() }}); err != nil {
		return Event{}, err
	}

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
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, closed()
		}
		next := state.Event
		next.Form, next.InternalOnly = &form, true
		if err := save(ctx, next); err != nil {
			return Event{}, err
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(Form) error { return accepting() }}); err != nil {
		return Event{}, err
	}

	// The end date can only move while submissions are open, so an ended event never reopens.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "details", func(ctx workflow.Context, input EventDetails) (Event, error) {
		details, closes, err := input.Normalize()
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, closed()
		}
		now := workflow.Now(ctx)
		next := state.Event
		if details.EndDate != next.EndDate {
			if next.At(now).Status != "open" {
				return Event{}, temporal.NewApplicationError("submissions have closed, so the end date can no longer change", "Closed")
			}
			if !closes.After(now) {
				return Event{}, temporal.NewApplicationError(errEndDatePassed.Error(), "Invalid")
			}
			next.EndDate, next.ClosesAt, next.CompletesAt = details.EndDate, closes, closes.AddDate(0, 0, 7)
		}
		next.Name, next.Description = details.Name, details.Description
		if details.Subheading != nil && *details.Subheading != next.EffectiveSubheading() {
			next.Subheading = details.Subheading
		}
		changed := next.EndDate != state.Event.EndDate || next.Name != state.Event.Name ||
			next.Description != state.Event.Description || next.Subheading != state.Event.Subheading
		if changed {
			moved := next.CompletesAt != state.Event.CompletesAt
			if err := save(ctx, next); err != nil {
				return Event{}, err
			}
			rescheduled = rescheduled || moved
		}
		return state.Event.At(now), nil
	}, workflow.UpdateHandlerOptions{Validator: func(EventDetails) error { return accepting() }}); err != nil {
		return Event{}, err
	}

	// A new slug is reserved in the store, so no other event can take it.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "slug", func(ctx workflow.Context, input string) (Event, error) {
		slug, err := NormalizeSlug(input)
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, closed()
		}
		if slug == state.Event.Link() {
			return state.Event.At(workflow.Now(ctx)), nil
		}
		next := state.Event
		next.Slug = slug
		if slug != next.ID && !slices.Contains(next.Aliases, slug) {
			if len(next.Aliases) >= MaxAliases {
				return Event{}, temporal.NewApplicationError(fmt.Sprintf("an event can have at most %d additional links", MaxAliases), "Invalid")
			}
			next.Aliases = append(slices.Clone(next.Aliases), slug)
		}
		next.Revision++
		write := EventState{Event: next, Sequence: state.Sequence, Epoch: state.Epoch}
		if err := persist(ctx, "SaveSlug", SlugWrite{State: write, Slug: slug}); err != nil {
			var app *temporal.ApplicationError
			if errors.As(err, &app) && app.Type() == "Taken" {
				return Event{}, temporal.NewApplicationError("that event link is already in use", "Invalid")
			}
			return Event{}, err
		}
		state = write
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(string) error { return accepting() }}); err != nil {
		return Event{}, err
	}

	if err := workflow.SetUpdateHandlerWithOptions(ctx, "delete", func(ctx workflow.Context) (Event, error) {
		if err := begin(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt == nil {
			now := workflow.Now(ctx)
			next := state.Event
			next.DeletedAt = &now
			if err := save(ctx, next); err != nil {
				return Event{}, err
			}
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: validator}); err != nil {
		return Event{}, err
	}

	if input.Hydrate {
		err := workflow.ExecuteActivity(activityCtx, "ClaimEvent", input.ID).Get(ctx, &state)
		var app *temporal.ApplicationError
		if errors.As(err, &app) && app.Type() == "Missing" {
			abandoned = true
		} else if err != nil {
			return Event{}, err
		} else {
			ready = true
		}
	} else if _, err := workflow.AwaitWithTimeout(ctx, createTimeout, func() bool { return ready || abandoned }); err != nil {
		return Event{}, err
	} else if !ready {
		abandoned = true
	}

	// The timer runs until completion. A changed end date wakes it to start a new one.
	for ready && !abandoned {
		remaining := state.Event.CompletesAt.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			break
		}
		rescheduled = false
		woke, err := workflow.AwaitWithTimeout(ctx, remaining, func() bool {
			return rescheduled || abandoned || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
		})
		if err != nil {
			return Event{}, err
		}
		if !woke {
			break
		}
		if workflow.GetInfo(ctx).GetContinueAsNewSuggested() && !abandoned {
			rotating = true
			if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
				return Event{}, err
			}
			// The next run claims the event again, which also fences out anything this run left behind.
			return Event{}, workflow.NewContinueAsNewError(ctx, EventWorkflow, EventInput{ID: state.Event.ID, Hydrate: true})
		}
	}
	retiring = true
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return Event{}, err
	}
	if !ready || abandoned {
		return Event{}, nil
	}
	// Saving at completion takes the event off the store's list of open events.
	// The revision is unchanged, so readers see no change.
	if err := persist(ctx, "SaveEvent", state); err != nil {
		return Event{}, err
	}
	return state.Event.At(workflow.Now(ctx)), nil
}
