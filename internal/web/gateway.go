package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	commonpb "go.temporal.io/api/common/v1"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"golang.org/x/sync/errgroup"
)

// Gateway keeps HTTP concerns outside the durable event workflows.
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
	Page(context.Context, string, int, int) (tracker.ShardPage, error)
	FindLead(context.Context, []string, string) (*tracker.Lead, error)
	Health(context.Context) error
}

type temporalGateway struct {
	client    client.Client
	taskQueue string
	namespace string
}

func NewTemporalGateway(c client.Client, taskQueue, namespace string) Gateway {
	if taskQueue == "" {
		taskQueue = tracker.DefaultTaskQueue
	}
	if namespace == "" {
		namespace = "default"
	}
	return &temporalGateway{client: c, taskQueue: taskQueue, namespace: namespace}
}

func (g *temporalGateway) Create(ctx context.Context, event tracker.Event) error {
	_, err := g.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: tracker.EventID(event.ID), TaskQueue: g.taskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		// Without this, a taken slug returns the existing event's run instead of an error.
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, tracker.EventType, tracker.EventState{Event: event})
	return err
}

func (g *temporalGateway) List(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, "", errInvalidCursor
	}
	result, err := g.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		PageSize: 25, NextPageToken: token,
		Query: "WorkflowType = 'EventWorkflow' AND ExecutionStatus IN ('Running', 'Completed')",
	})
	if err != nil {
		return nil, "", err
	}
	// Each event is a separate query round trip, so the page runs them together.
	// The page size bounds how many run at once.
	found := make([]*tracker.Event, len(result.Executions))
	group, groupCtx := errgroup.WithContext(ctx)
	for i, execution := range result.Executions {
		if execution.Status != enums.WORKFLOW_EXECUTION_STATUS_RUNNING && execution.Status != enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
			continue
		}
		id := strings.TrimPrefix(execution.GetExecution().GetWorkflowId(), "event/")
		group.Go(func() error {
			event, err := g.Event(groupCtx, id)
			var missing *serviceerror.NotFound
			if errors.As(err, &missing) || errors.Is(err, errDeleted) {
				return nil
			}
			if err != nil {
				return err
			}
			found[i] = &event
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, "", err
	}
	events := make([]tracker.Event, 0, len(found))
	for _, event := range found {
		if event != nil {
			events = append(events, *event)
		}
	}
	return events, base64.RawURLEncoding.EncodeToString(result.NextPageToken), nil
}

func (g *temporalGateway) Event(ctx context.Context, id string) (tracker.Event, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Queries do not advance workflow time, so LoadEvent derives display status at request time.
	event, err := tracker.LoadEvent(ctx, g.client, id)
	if err != nil {
		return event, err
	}
	if event.DeletedAt != nil {
		return tracker.Event{}, errDeleted
	}
	return event, nil
}

func (g *temporalGateway) Submit(ctx context.Context, id string, submission tracker.Submission) error {
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

func (g *temporalGateway) Delete(ctx context.Context, id string) error {
	// Resolve an alias first, and keep deleted events: deleting twice is not an error.
	event, err := tracker.LoadEvent(ctx, g.client, id)
	if err != nil {
		return err
	}
	described, err := g.client.DescribeWorkflowExecution(ctx, tracker.EventID(event.ID), "")
	if err != nil {
		return err
	}
	// A running event can record the deletion. A closed one cannot accept an update,
	// and the directory includes completed workflows, so remove that execution.
	if described.GetWorkflowExecutionInfo().GetStatus() == enums.WORKFLOW_EXECUTION_STATUS_RUNNING {
		handle, err := g.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
			WorkflowID: tracker.EventID(event.ID), UpdateID: "delete-event", UpdateName: "delete",
			WaitForStage: client.WorkflowUpdateStageCompleted,
		})
		if err != nil {
			return err
		}
		return handle.Get(ctx, &event)
	}
	// Aliases go first so a failed attempt can be retried through the event's own slug.
	for _, slug := range append(event.Aliases, event.ID) {
		_, err = g.client.WorkflowService().DeleteWorkflowExecution(ctx, &workflowservice.DeleteWorkflowExecutionRequest{
			Namespace:         g.namespace,
			WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: tracker.EventID(slug)},
		})
		var missing *serviceerror.NotFound
		if err != nil && !errors.As(err, &missing) {
			return err
		}
	}
	return nil
}

func (g *temporalGateway) End(ctx context.Context, id string) (tracker.Event, error) {
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

func (g *temporalGateway) Banner(ctx context.Context, id, banner string) (tracker.Event, error) {
	return g.edit(ctx, id, "banner", banner)
}

func (g *temporalGateway) Preview(ctx context.Context, id string, preview tracker.LinkPreview) (tracker.Event, error) {
	return g.edit(ctx, id, "preview", preview)
}

func (g *temporalGateway) Form(ctx context.Context, id string, form tracker.Form) (tracker.Event, error) {
	return g.edit(ctx, id, "form", form)
}

func (g *temporalGateway) InternalOnly(ctx context.Context, id string, form tracker.Form) (tracker.Event, error) {
	return g.edit(ctx, id, "internalOnly", form)
}

func (g *temporalGateway) Details(ctx context.Context, id string, details tracker.EventDetails) (tracker.Event, error) {
	return g.edit(ctx, id, "details", details)
}

func (g *temporalGateway) Slug(ctx context.Context, id, slug string) (tracker.Event, error) {
	return g.edit(ctx, id, "slug", slug)
}

// edit runs an employee's change as an Update. Each request gets a fresh Update
// ID, so changing a value back to an earlier one is a new change.
func (g *temporalGateway) edit(ctx context.Context, id, update string, value any) (tracker.Event, error) {
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

func (g *temporalGateway) Page(ctx context.Context, shardID string, offset, limit int) (tracker.ShardPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var page tracker.ShardPage
	result, err := g.client.QueryWorkflow(ctx, shardID, "", "page", offset, limit)
	if err != nil {
		return page, err
	}
	err = result.Get(&page)
	return page, err
}

// FindLead returns the participant with email in any of the shards, or nil.
func (g *temporalGateway) FindLead(ctx context.Context, shards []string, email string) (*tracker.Lead, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, lead, err := tracker.FindLead(ctx, g.client, shards, email)
	return lead, err
}

func (g *temporalGateway) Health(ctx context.Context) error {
	_, err := g.client.CheckHealth(ctx, nil)
	return err
}

var (
	errInvalidCursor = errors.New("invalid pagination cursor")
	errChanged       = errors.New("participants changed; reload the list or retry the export")
	errExpired       = errors.New("event participant data is no longer available in Temporal retention")
	errDeleted       = errors.New("this event was deleted")
)
