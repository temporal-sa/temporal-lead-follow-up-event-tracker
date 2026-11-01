package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// Gateway keeps HTTP concerns outside the durable event workflows. Reads come
// from the store; every change is an Update to the event's workflow, which
// stores it.
type Gateway interface {
	Create(context.Context, tracker.Event) error
	List(context.Context, string) ([]tracker.Event, string, error)
	Event(context.Context, string) (tracker.Event, error)
	Submit(context.Context, string, tracker.Submission) error
	End(context.Context, string) (tracker.Event, error)
	Delete(context.Context, string) error
	Banner(context.Context, string, string) (tracker.Event, error)
	Preview(context.Context, string, tracker.LinkPreview) (tracker.Event, error)
	Form(context.Context, string, tracker.Form) (tracker.Event, error)
	InternalOnly(context.Context, string, tracker.Form) (tracker.Event, error)
	Details(context.Context, string, tracker.EventDetails) (tracker.Event, error)
	Slug(context.Context, string, string) (tracker.Event, error)
	Participants(ctx context.Context, eventID, cursor string, limit int) ([]tracker.Lead, string, error)
	Participant(ctx context.Context, eventID, email string) (*tracker.Lead, error)
	Health(context.Context) error
}

type gateway struct {
	store     tracker.Reader
	client    client.Client
	taskQueue string
}

func NewGateway(store tracker.Reader, c client.Client, taskQueue string) Gateway {
	if taskQueue == "" {
		taskQueue = tracker.DefaultTaskQueue
	}
	return &gateway{store: store, client: c, taskQueue: taskQueue}
}

// Create starts the event's workflow with its create Update in one call. The
// store reserves the ID; a taken one fails with a "Taken" error, and an ID whose
// workflow is still running fails with WorkflowExecutionAlreadyStarted.
func (g *gateway) Create(ctx context.Context, event tracker.Event) error {
	// A fixed Update ID would match the running event's own create and report
	// success instead of the conflict.
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	start := g.client.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
		ID: tracker.EventID(event.ID), TaskQueue: g.taskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, tracker.EventType, tracker.EventInput{ID: event.ID})
	handle, err := g.client.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: start,
		UpdateOptions: client.UpdateWorkflowOptions{
			UpdateName: "create", UpdateID: "create-" + hex.EncodeToString(token[:]), Args: []any{event},
			WaitForStage: client.WorkflowUpdateStageCompleted,
		},
	})
	if err != nil {
		return err
	}
	return handle.Get(ctx, nil)
}

func (g *gateway) List(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	events, next, err := g.store.Events(ctx, cursor)
	if errors.Is(err, tracker.ErrInvalidCursor) {
		return nil, "", errInvalidCursor
	}
	now := time.Now()
	for i := range events {
		events[i] = events[i].At(now)
	}
	return events, next, err
}

// Event resolves any of an event's links, with its status as of now.
func (g *gateway) Event(ctx context.Context, slug string) (tracker.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	event, err := g.store.Resolve(ctx, slug)
	if err != nil {
		return tracker.Event{}, err
	}
	if event.DeletedAt != nil {
		return tracker.Event{}, errDeleted
	}
	return event.At(time.Now()), nil
}

func (g *gateway) Submit(ctx context.Context, id string, submission tracker.Submission) error {
	handle, err := g.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: tracker.EventID(id), UpdateID: submission.RequestID + "-" + submission.Digest(),
		UpdateName: "submit", Args: []any{submission}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return err
	}
	var status string
	return handle.Get(ctx, &status)
}

// Delete records the deletion through the event's workflow. A completed event
// gets a new run that loads it, records the deletion, and completes again.
func (g *gateway) Delete(ctx context.Context, slug string) error {
	event, err := g.store.Resolve(ctx, slug)
	if err != nil || event.DeletedAt != nil {
		return err
	}
	start := g.client.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
		ID: tracker.EventID(event.ID), TaskQueue: g.taskQueue,
		WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
		WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, tracker.EventType, tracker.EventInput{ID: event.ID, Hydrate: true})
	handle, err := g.client.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
		StartWorkflowOperation: start,
		UpdateOptions: client.UpdateWorkflowOptions{
			UpdateName: "delete", UpdateID: "delete-event", WaitForStage: client.WorkflowUpdateStageCompleted,
		},
	})
	if err != nil {
		return err
	}
	return handle.Get(ctx, nil)
}

func (g *gateway) End(ctx context.Context, id string) (tracker.Event, error) {
	var event tracker.Event
	handle, err := g.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: tracker.EventID(id), UpdateID: "end-event", UpdateName: "end",
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return event, err
	}
	err = handle.Get(ctx, &event)
	return event, err
}

func (g *gateway) Banner(ctx context.Context, id, banner string) (tracker.Event, error) {
	return g.edit(ctx, id, "banner", banner)
}

func (g *gateway) Preview(ctx context.Context, id string, preview tracker.LinkPreview) (tracker.Event, error) {
	return g.edit(ctx, id, "preview", preview)
}

func (g *gateway) Form(ctx context.Context, id string, form tracker.Form) (tracker.Event, error) {
	return g.edit(ctx, id, "form", form)
}

func (g *gateway) InternalOnly(ctx context.Context, id string, form tracker.Form) (tracker.Event, error) {
	return g.edit(ctx, id, "internalOnly", form)
}

func (g *gateway) Details(ctx context.Context, id string, details tracker.EventDetails) (tracker.Event, error) {
	return g.edit(ctx, id, "details", details)
}

func (g *gateway) Slug(ctx context.Context, id, slug string) (tracker.Event, error) {
	return g.edit(ctx, id, "slug", slug)
}

// edit runs an employee's change as an Update. Each request gets a fresh Update
// ID, so changing a value back to an earlier one is a new change.
func (g *gateway) edit(ctx context.Context, id, update string, value any) (tracker.Event, error) {
	var event tracker.Event
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return event, err
	}
	handle, err := g.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: tracker.EventID(id), UpdateID: update + "-" + hex.EncodeToString(token[:]), UpdateName: update,
		Args: []any{value}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return event, err
	}
	err = handle.Get(ctx, &event)
	return event, err
}

func (g *gateway) Participants(ctx context.Context, eventID, cursor string, limit int) ([]tracker.Lead, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	leads, next, err := g.store.Participants(ctx, eventID, cursor, limit)
	if errors.Is(err, tracker.ErrInvalidCursor) {
		return nil, "", errInvalidCursor
	}
	return leads, next, err
}

func (g *gateway) Participant(ctx context.Context, eventID, email string) (*tracker.Lead, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return g.store.Participant(ctx, eventID, email)
}

func (g *gateway) Health(ctx context.Context) error {
	if _, err := g.client.CheckHealth(ctx, nil); err != nil {
		return err
	}
	return g.store.Health(ctx)
}

var (
	errInvalidCursor = errors.New("invalid pagination cursor")
	errChanged       = errors.New("participants changed; reload the list or retry the export")
	errDeleted       = errors.New("this event was deleted")
	errPaused        = errors.New("Changes are paused for maintenance. Please retry in a few minutes.")
)
