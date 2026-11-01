package tracker

import (
	"context"
	"errors"
	"fmt"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

type Activities struct {
	Client    client.Client
	Store     Store
	TaskQueue string
}

type SlugWrite struct {
	State EventState `json:"state"`
	Slug  string     `json:"slug"`
}

type LookupInput struct {
	EventID   string `json:"eventId"`
	Email     string `json:"email"`
	RequestID string `json:"requestId"`
}

type SubmissionWrite struct {
	State   EventState `json:"state"`
	Lead    Lead       `json:"lead"`
	Receipt Receipt    `json:"receipt"`
}

// storeError turns the store's definite answers into errors a retry can't change.
func storeError(err error) error {
	switch {
	case errors.Is(err, ErrTaken):
		return temporal.NewNonRetryableApplicationError(err.Error(), "Taken", nil)
	case errors.Is(err, ErrFenced):
		return temporal.NewNonRetryableApplicationError(err.Error(), "Fenced", nil)
	case errors.Is(err, ErrNotStored):
		return temporal.NewNonRetryableApplicationError(err.Error(), "Missing", nil)
	}
	return err
}

func (a *Activities) CreateEvent(ctx context.Context, state EventState) error {
	return storeError(a.Store.Create(ctx, state))
}

func (a *Activities) ClaimEvent(ctx context.Context, id string) (EventState, error) {
	state, err := a.Store.Claim(ctx, id)
	return state, storeError(err)
}

func (a *Activities) SaveEvent(ctx context.Context, state EventState) error {
	return storeError(a.Store.SaveEvent(ctx, state))
}

func (a *Activities) SaveSlug(ctx context.Context, write SlugWrite) error {
	return storeError(a.Store.SaveSlug(ctx, write.State, write.Slug))
}

func (a *Activities) Lookup(ctx context.Context, input LookupInput) (Existing, error) {
	return a.Store.Lookup(ctx, input.EventID, input.Email, input.RequestID)
}

func (a *Activities) SaveSubmission(ctx context.Context, write SubmissionWrite) error {
	return storeError(a.Store.SaveSubmission(ctx, write.State, write.Lead, write.Receipt))
}

// StartOpenEvents starts a workflow for every stored event that should have one
// and doesn't, such as after a namespace reset. It returns how many it checked.
func (a *Activities) StartOpenEvents(ctx context.Context) (int, error) {
	checked := 0
	var failures []error
	for cursor := ""; ; {
		events, next, err := a.Store.OpenEvents(ctx, cursor)
		if err != nil {
			return checked, err
		}
		for _, event := range events {
			if _, err := a.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
				ID: EventID(event.ID), TaskQueue: a.TaskQueue,
				// A running run is the one we want. A run that was terminated (the
				// storage cut-over) or failed is replaced; one that completed is not.
				WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
				WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
			}, EventType, EventInput{ID: event.ID, Hydrate: true}); err != nil {
				failures = append(failures, fmt.Errorf("start %s: %w", event.ID, err))
			}
			checked++
			activity.RecordHeartbeat(ctx, checked)
		}
		if next == "" {
			return checked, errors.Join(failures...)
		}
		cursor = next
	}
}
