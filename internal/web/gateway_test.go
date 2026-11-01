package web

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	commonpb "go.temporal.io/api/common/v1"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
)

func TestTemporalDirectoryQueriesOnlyRunningAndCompletedEvents(t *testing.T) {
	c := &mocks.Client{}
	statuses := []enums.WorkflowExecutionStatus{
		enums.WORKFLOW_EXECUTION_STATUS_RUNNING,
		enums.WORKFLOW_EXECUTION_STATUS_COMPLETED,
		enums.WORKFLOW_EXECUTION_STATUS_TERMINATED,
		enums.WORKFLOW_EXECUTION_STATUS_CANCELED,
		enums.WORKFLOW_EXECUTION_STATUS_FAILED,
		enums.WORKFLOW_EXECUTION_STATUS_TIMED_OUT,
		enums.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW,
	}
	response := &workflowservice.ListWorkflowExecutionsResponse{NextPageToken: []byte("next")}
	for _, status := range statuses {
		id := status.String()
		response.Executions = append(response.Executions, &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: tracker.EventID(id)}, Status: status})
		if status == enums.WORKFLOW_EXECUTION_STATUS_RUNNING || status == enums.WORKFLOW_EXECUTION_STATUS_COMPLETED {
			value := &mocks.Value{}
			value.On("Get", mock.Anything).Run(func(args mock.Arguments) {
				*args[0].(*tracker.Event) = tracker.Event{ID: id, Name: id, Count: 1}
			}).Return(nil).Once()
			c.On("QueryWorkflow", mock.Anything, tracker.EventID(id), "", "event").Return(value, nil).Once()
		}
	}
	c.On("ListWorkflow", mock.Anything, mock.Anything).Return(response, nil).Once()
	events, cursor, err := NewTemporalGateway(c, "events", "default").List(context.Background(), "")
	if err != nil || len(events) != 2 || cursor != base64.RawURLEncoding.EncodeToString(response.NextPageToken) {
		t.Fatalf("directory: events=%v cursor=%q error=%v", events, cursor, err)
	}
	c.AssertExpectations(t)
}

func TestListOmitsDeletedEvents(t *testing.T) {
	c := &mocks.Client{}
	response := &workflowservice.ListWorkflowExecutionsResponse{}
	for _, id := range []string{"visible", "deleted"} {
		response.Executions = append(response.Executions, &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: tracker.EventID(id)}, Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING})
		value := &mocks.Value{}
		eventID := id
		value.On("Get", mock.Anything).Run(func(args mock.Arguments) {
			event := tracker.Event{ID: eventID, Name: eventID}
			if eventID == "deleted" {
				now := time.Now()
				event.DeletedAt = &now
			}
			*args[0].(*tracker.Event) = event
		}).Return(nil).Once()
		c.On("QueryWorkflow", mock.Anything, tracker.EventID(id), "", "event").Return(value, nil).Once()
	}
	c.On("ListWorkflow", mock.Anything, mock.Anything).Return(response, nil).Once()
	events, _, err := NewTemporalGateway(c, "events", "default").List(context.Background(), "")
	if err != nil || len(events) != 1 || events[0].ID != "visible" {
		t.Fatalf("directory: %#v %v", events, err)
	}
	c.AssertExpectations(t)
}

func TestListQueriesEventsConcurrentlyInPageOrder(t *testing.T) {
	c := &mocks.Client{}
	ids := []string{"first", "second", "third", "fourth"}
	response := &workflowservice.ListWorkflowExecutionsResponse{}
	// Every query waits until all of them have started, so one-at-a-time queries time out.
	var started sync.WaitGroup
	started.Add(len(ids))
	for _, id := range ids {
		response.Executions = append(response.Executions, &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: tracker.EventID(id)}, Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING})
		value := &mocks.Value{}
		value.On("Get", mock.Anything).Run(func(args mock.Arguments) {
			*args[0].(*tracker.Event) = tracker.Event{ID: id, Name: id}
		}).Return(nil).Once()
		c.On("QueryWorkflow", mock.Anything, tracker.EventID(id), "", "event").Run(func(mock.Arguments) {
			started.Done()
			all := make(chan struct{})
			go func() { started.Wait(); close(all) }()
			select {
			case <-all:
			case <-time.After(2 * time.Second):
				t.Error("event queries ran one at a time")
			}
		}).Return(value, nil).Once()
	}
	c.On("ListWorkflow", mock.Anything, mock.Anything).Return(response, nil).Once()
	events, _, err := NewTemporalGateway(c, "events", "default").List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, event := range events {
		got = append(got, event.ID)
	}
	if !reflect.DeepEqual(got, ids) {
		t.Fatalf("events %v, want %v", got, ids)
	}
	c.AssertExpectations(t)
}

func TestListFailsWhenAnEventQueryFails(t *testing.T) {
	c := &mocks.Client{}
	response := &workflowservice.ListWorkflowExecutionsResponse{}
	for _, id := range []string{"visible", "unavailable"} {
		response.Executions = append(response.Executions, &workflowpb.WorkflowExecutionInfo{Execution: &commonpb.WorkflowExecution{WorkflowId: tracker.EventID(id)}, Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING})
	}
	queryReturns(c, "event/visible", tracker.Event{ID: "visible"})
	unavailable := serviceerror.NewUnavailable("worker unavailable")
	c.On("QueryWorkflow", mock.Anything, "event/unavailable", "", "event").Return(nil, unavailable).Once()
	c.On("ListWorkflow", mock.Anything, mock.Anything).Return(response, nil).Once()
	events, _, err := NewTemporalGateway(c, "events", "default").List(context.Background(), "")
	if !errors.Is(err, unavailable) || events != nil {
		t.Fatalf("events=%v err=%v", events, err)
	}
}

// queryReturns makes the event query on workflowID answer with event.
func queryReturns(c *mocks.Client, workflowID string, event tracker.Event) {
	value := &mocks.Value{}
	value.On("Get", mock.Anything).Run(func(args mock.Arguments) { *args[0].(*tracker.Event) = event }).Return(nil).Once()
	c.On("QueryWorkflow", mock.Anything, workflowID, "", "event").Return(value, nil).Once()
}

func TestDeleteRunningEventRecordsTheUpdate(t *testing.T) {
	c := &mocks.Client{}
	queryReturns(c, "event/test", tracker.Event{ID: "test"})
	c.On("DescribeWorkflowExecution", mock.Anything, "event/test", "").Return(&workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING},
	}, nil).Once()
	handle := &mocks.WorkflowUpdateHandle{}
	now := time.Now().UTC()
	handle.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		*args[1].(*tracker.Event) = tracker.Event{ID: "test", DeletedAt: &now}
	}).Return(nil).Once()
	c.On("UpdateWorkflow", mock.Anything, mock.MatchedBy(func(input client.UpdateWorkflowOptions) bool {
		return input.WorkflowID == "event/test" && input.UpdateName == "delete" && input.UpdateID == "delete-event" && input.WaitForStage == client.WorkflowUpdateStageCompleted
	})).Return(handle, nil).Once()
	if err := NewTemporalGateway(c, "events", "default").Delete(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	c.AssertExpectations(t)
}

// Until the storage cut-over, a closed event's histories are the archive's source,
// so deleting one is refused rather than removing them.
func TestDeleteClosedEventIsRefusedWithoutRemovingIt(t *testing.T) {
	c := &mocks.Client{}
	queryReturns(c, "event/test", tracker.Event{ID: "test", Aliases: []string{"renamed"}})
	c.On("DescribeWorkflowExecution", mock.Anything, "event/test", "").Return(&workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_COMPLETED},
	}, nil).Once()
	if err := NewTemporalGateway(c, "events", "leads").Delete(context.Background(), "test"); !errors.Is(err, errClosedDelete) {
		t.Fatalf("err=%v", err)
	}
	c.AssertNotCalled(t, "WorkflowService")
	c.AssertExpectations(t)
}

func TestDeleteThroughAliasRecordsTheUpdateOnTheEvent(t *testing.T) {
	c := &mocks.Client{}
	queryReturns(c, "event/renamed", tracker.Event{ID: "test"})
	queryReturns(c, "event/test", tracker.Event{ID: "test", Slug: "renamed", Aliases: []string{"renamed"}})
	c.On("DescribeWorkflowExecution", mock.Anything, "event/test", "").Return(&workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: enums.WORKFLOW_EXECUTION_STATUS_RUNNING},
	}, nil).Once()
	handle := &mocks.WorkflowUpdateHandle{}
	handle.On("Get", mock.Anything, mock.Anything).Return(nil).Once()
	c.On("UpdateWorkflow", mock.Anything, mock.MatchedBy(func(input client.UpdateWorkflowOptions) bool {
		return input.WorkflowID == "event/test" && input.UpdateName == "delete"
	})).Return(handle, nil).Once()
	if err := NewTemporalGateway(c, "events", "default").Delete(context.Background(), "renamed"); err != nil {
		t.Fatal(err)
	}
	c.AssertExpectations(t)
}

func TestEventFollowsAnAliasOnce(t *testing.T) {
	c := &mocks.Client{}
	queryReturns(c, "event/renamed", tracker.Event{ID: "test"})
	queryReturns(c, "event/test", tracker.Event{ID: "test", Name: "Conference", Slug: "renamed", ClosesAt: time.Now().Add(time.Hour), CompletesAt: time.Now().Add(2 * time.Hour)})
	event, err := NewTemporalGateway(c, "events", "default").Event(context.Background(), "renamed")
	if err != nil || event.ID != "test" || event.Name != "Conference" || event.Status != "open" {
		t.Fatalf("event=%+v err=%v", event, err)
	}
	c.AssertExpectations(t)

	// An alias whose event slug now belongs to another alias must not show that other event.
	c = &mocks.Client{}
	queryReturns(c, "event/stale", tracker.Event{ID: "gone"})
	queryReturns(c, "event/gone", tracker.Event{ID: "someone-else"})
	_, err = NewTemporalGateway(c, "events", "default").Event(context.Background(), "stale")
	var missing *serviceerror.NotFound
	if !errors.As(err, &missing) {
		t.Fatalf("stale alias error=%v", err)
	}
	c.AssertExpectations(t)
}

func TestBannerUpdateIDsAllowReturningToPreviousMessage(t *testing.T) {
	c := &mocks.Client{}
	seen := make(map[string]bool)
	for _, banner := range []string{"First", "Second", "First"} {
		handle := &mocks.WorkflowUpdateHandle{}
		handle.On("Get", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
			*args[1].(*tracker.Event) = tracker.Event{ID: "test", QRBanner: banner}
		}).Return(nil).Once()
		options := mock.MatchedBy(func(input client.UpdateWorkflowOptions) bool {
			return input.WorkflowID == "event/test" && input.UpdateName == "banner" && input.WaitForStage == client.WorkflowUpdateStageCompleted && len(input.Args) == 1 && input.Args[0] == banner && input.UpdateID != ""
		})
		c.On("UpdateWorkflow", mock.Anything, options).Run(func(args mock.Arguments) {
			id := args[1].(client.UpdateWorkflowOptions).UpdateID
			if seen[id] {
				t.Errorf("banner update reused ID %q", id)
			}
			seen[id] = true
		}).Return(handle, nil).Once()
	}
	g := NewTemporalGateway(c, "events", "default")
	for _, banner := range []string{"First", "Second", "First"} {
		event, err := g.Banner(context.Background(), "test", banner)
		if err != nil || event.QRBanner != banner {
			t.Fatalf("banner update: %#v %v", event, err)
		}
	}
	c.AssertExpectations(t)
}

func TestTemporalQueriesHaveBoundedDeadlines(t *testing.T) {
	for _, query := range []string{"event", "page"} {
		t.Run(query, func(t *testing.T) {
			c := &mocks.Client{}
			bounded := mock.MatchedBy(func(ctx context.Context) bool {
				deadline, ok := ctx.Deadline()
				return ok && time.Until(deadline) > 0 && time.Until(deadline) <= 5*time.Second
			})
			if query == "event" {
				c.On("QueryWorkflow", bounded, "event/test", "", query).Return(nil, context.DeadlineExceeded).Once()
				_, err := NewTemporalGateway(c, "events", "default").Event(context.Background(), "test")
				if err != context.DeadlineExceeded {
					t.Fatal(err)
				}
			} else {
				c.On("QueryWorkflow", bounded, "shard", "", query, 0, 100).Return(nil, context.DeadlineExceeded).Once()
				_, err := NewTemporalGateway(c, "events", "default").Page(context.Background(), "shard", 0, 100)
				if err != context.DeadlineExceeded {
					t.Fatal(err)
				}
			}
			c.AssertExpectations(t)
		})
	}
}
