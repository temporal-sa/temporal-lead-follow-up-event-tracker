package web

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
	commonpb "go.temporal.io/api/common/v1"
	enums "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
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
	events, cursor, err := NewTemporalGateway(c, "events").List(context.Background(), "")
	if err != nil || len(events) != 2 || cursor != base64.RawURLEncoding.EncodeToString(response.NextPageToken) {
		t.Fatalf("directory: events=%v cursor=%q error=%v", events, cursor, err)
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
				_, err := NewTemporalGateway(c, "events").Event(context.Background(), "test")
				if err != context.DeadlineExceeded {
					t.Fatal(err)
				}
			} else {
				c.On("QueryWorkflow", bounded, "shard", "", query, 0, 100).Return(nil, context.DeadlineExceeded).Once()
				_, err := NewTemporalGateway(c, "events").Page(context.Background(), "shard", 0, 100)
				if err != context.DeadlineExceeded {
					t.Fatal(err)
				}
			}
			c.AssertExpectations(t)
		})
	}
}
