package tracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// MaxSlugAttempts is how many name-based slugs a create tries before falling
// back to a random ID.
const MaxSlugAttempts = 50

// Creator starts an event's workflow and stores the event. It reports a taken
// slug as Taken or WorkflowExecutionAlreadyStarted.
type Creator func(context.Context, Event) error

// CreateWithSlug picks the event's ID from its name and creates it, trying the
// next suffix while a slug is taken. When the event carries a CreateRequestID,
// a retried create finds the event the earlier attempt made at one of those
// slugs and returns it, instead of making a second one.
func CreateWithSlug(ctx context.Context, event Event, create Creator, owned func(context.Context, string) (bool, error)) (Event, error) {
	for attempt := 0; ; attempt++ {
		event.ID = Slug(event.Name, attempt)
		if event.ID == "" || attempt >= MaxSlugAttempts {
			id, err := randomID()
			if err != nil {
				return Event{}, err
			}
			event.ID = id
		}
		err := create(ctx, event)
		if err == nil || !taken(err) || attempt >= MaxSlugAttempts {
			return event, err
		}
		if event.CreateRequestID == "" {
			continue
		}
		mine, err := owned(ctx, event.ID)
		if err != nil {
			return Event{}, err
		}
		if mine {
			return event, nil
		}
	}
}

func taken(err error) bool {
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	var app *temporal.ApplicationError
	return errors.As(err, &started) || (errors.As(err, &app) && app.Type() == "Taken")
}

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// StartEvent starts the event's workflow with its create Update in one call
// (Update-with-Start). The store reserves the ID; a taken one fails with a
// "Taken" error, and an ID whose workflow is still running fails to start.
func StartEvent(ctx context.Context, c client.Client, taskQueue string, event Event) error {
	// A fixed Update ID would match the running event's own create and report
	// success instead of the conflict.
	token, err := randomID()
	if err != nil {
		return err
	}
	start := c.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
		ID: EventID(event.ID), TaskQueue: taskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, EventType, EventInput{ID: event.ID})
	handle, err := c.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: start,
		UpdateOptions: client.UpdateWorkflowOptions{
			UpdateName: "create", UpdateID: "create-" + token, Args: []any{event},
			WaitForStage: client.WorkflowUpdateStageCompleted,
		},
	})
	if err != nil {
		return err
	}
	return handle.Get(ctx, nil)
}

// CreatedBy reports whether the event stored at slug was created for requestID.
// A slug whose workflow is running but not yet stored may be an earlier attempt
// still saving, so it waits briefly for the store before answering no.
func CreatedBy(ctx context.Context, store Reader, slug, requestID string) (bool, error) {
	for wait := 0; ; wait++ {
		stored, err := store.Resolve(ctx, slug)
		if err == nil {
			return stored.CreateRequestID == requestID, nil
		}
		if !errors.Is(err, ErrNotStored) {
			return false, err
		}
		if wait == 5 {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}
