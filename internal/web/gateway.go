package web

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// Gateway keeps HTTP concerns outside the durable event workflows.
type Gateway interface {
	Create(context.Context, tracker.Event) error
	List(context.Context, string) ([]tracker.Event, string, error)
	Event(context.Context, string) (tracker.Event, error)
	Submit(context.Context, string, tracker.Submission) error
	End(context.Context, string) (tracker.Event, error)
	Page(context.Context, string, int, int) (tracker.ShardPage, error)
	Health(context.Context) error
}

type temporalGateway struct {
	client    client.Client
	taskQueue string
}

func NewTemporalGateway(c client.Client, taskQueue string) Gateway {
	if taskQueue == "" {
		taskQueue = tracker.DefaultTaskQueue
	}
	return &temporalGateway{client: c, taskQueue: taskQueue}
}

func (g *temporalGateway) Create(ctx context.Context, event tracker.Event) error {
	_, err := g.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: tracker.EventID(event.ID), TaskQueue: g.taskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, tracker.EventType, tracker.EventState{Event: event})
	return err
}

func (g *temporalGateway) List(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	token, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, "", errInvalidCursor
	}
	result, err := g.client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		PageSize: 25, NextPageToken: token,
		Query: "WorkflowType = 'EventWorkflow' AND ExecutionStatus != 'ContinuedAsNew'",
	})
	if err != nil {
		return nil, "", err
	}
	events := make([]tracker.Event, 0, len(result.Executions))
	for _, execution := range result.Executions {
		id := strings.TrimPrefix(execution.GetExecution().GetWorkflowId(), "event/")
		event, err := g.Event(ctx, id)
		var missing *serviceerror.NotFound
		if errors.As(err, &missing) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		events = append(events, event)
	}
	return events, base64.RawURLEncoding.EncodeToString(result.NextPageToken), nil
}

func (g *temporalGateway) Event(ctx context.Context, id string) (tracker.Event, error) {
	var event tracker.Event
	result, err := g.client.QueryWorkflow(ctx, tracker.EventID(id), "", "event")
	if err != nil {
		return event, err
	}
	if err := result.Get(&event); err != nil {
		return event, err
	}
	// Queries do not advance workflow time, so derive display status at request time.
	return event.At(time.Now()), nil
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

func (g *temporalGateway) Page(ctx context.Context, shardID string, offset, limit int) (tracker.ShardPage, error) {
	var page tracker.ShardPage
	result, err := g.client.QueryWorkflow(ctx, shardID, "", "page", offset, limit)
	if err != nil {
		return page, err
	}
	err = result.Get(&page)
	return page, err
}

func (g *temporalGateway) Health(ctx context.Context) error {
	_, err := g.client.CheckHealth(ctx, nil)
	return err
}

var (
	errInvalidCursor = errors.New("invalid pagination cursor")
	errChanged       = errors.New("participants changed; reload the list or retry the export")
	errExpired       = errors.New("event participant data is no longer available in Temporal retention")
)
