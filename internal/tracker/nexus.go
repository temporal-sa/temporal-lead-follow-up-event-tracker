package tracker

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/nexus-rpc/sdk-go/nexus"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

const (
	NexusServiceName = "event-leads"

	OperationListEvents        = "list_events"
	OperationGetEvent          = "get_event"
	OperationSubmitParticipant = "submit_participant"
	OperationLookupParticipant = "lookup_participant"
	OperationClaimFlightPass   = "claim_flight_pass"
)

type Service struct {
	Client    client.Client
	TaskQueue string
}

func (s Service) Register(w worker.Worker) {
	svc := nexus.NewService(NexusServiceName)
	svc.MustRegister(
		nexus.NewSyncOperation(OperationListEvents, s.listEvents),
		nexus.NewSyncOperation(OperationGetEvent, s.getEvent),
		nexus.NewSyncOperation(OperationSubmitParticipant, s.submitParticipant),
		nexus.NewSyncOperation(OperationLookupParticipant, s.lookupParticipant),
		nexus.NewSyncOperation(OperationClaimFlightPass, s.claimFlightPass),
	)
	w.RegisterNexusService(svc)
}

type ListEventsInput struct {
	Cursor string `json:"cursor,omitempty"`
}

type PublicEvent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	EndDate     string    `json:"endDate"`
	Status      string    `json:"status"`
	ClosesAt    time.Time `json:"closesAt"`
}

type ListEventsResult struct {
	Events     []PublicEvent `json:"events"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type GetEventInput struct {
	EventID string `json:"eventId"`
}

type SubmitParticipantInput struct {
	EventID    string     `json:"eventId"`
	Submission Submission `json:"submission"`
}

type SubmitParticipantResult struct {
	Status     string `json:"status"`
	FlightCode string `json:"flightCode"`
}

type LookupParticipantInput struct {
	EventID string `json:"eventId"`
	Email   string `json:"email"`
}

type LookupParticipantResult struct {
	Found bool   `json:"found"`
	Name  string `json:"name,omitempty"`
	Role  string `json:"role,omitempty"`
	Email string `json:"email,omitempty"`
}

type ClaimFlightPassInput struct {
	EventID string `json:"eventId"`
	Code    string `json:"code"`
}

func (s Service) listEvents(ctx context.Context, input ListEventsInput, _ nexus.StartOperationOptions) (ListEventsResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token, err := base64.RawURLEncoding.DecodeString(input.Cursor)
	if err != nil {
		return ListEventsResult{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "invalid cursor")
	}
	result, err := s.Client.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		PageSize: 25, NextPageToken: token,
		Query: "WorkflowType = 'EventWorkflow' AND ExecutionStatus = 'Running'",
	})
	if err != nil {
		return ListEventsResult{}, nexusError(err)
	}
	events := make([]PublicEvent, 0, len(result.Executions))
	for _, execution := range result.Executions {
		id := strings.TrimPrefix(execution.GetExecution().GetWorkflowId(), "event/")
		event, err := s.event(ctx, id)
		var missing *serviceerror.NotFound
		if errors.As(err, &missing) {
			continue
		}
		if err != nil {
			return ListEventsResult{}, nexusError(err)
		}
		if event.Status != "open" {
			continue
		}
		events = append(events, publicEvent(event))
	}
	next := ""
	if len(result.NextPageToken) > 0 {
		next = base64.RawURLEncoding.EncodeToString(result.NextPageToken)
	}
	return ListEventsResult{Events: events, NextCursor: next}, nil
}

func (s Service) getEvent(ctx context.Context, input GetEventInput, _ nexus.StartOperationOptions) (PublicEvent, error) {
	event, err := s.event(ctx, input.EventID)
	if err != nil {
		return PublicEvent{}, nexusError(err)
	}
	return publicEvent(event), nil
}

func (s Service) submitParticipant(ctx context.Context, input SubmitParticipantInput, _ nexus.StartOperationOptions) (SubmitParticipantResult, error) {
	event, err := s.event(ctx, input.EventID)
	if err != nil {
		return SubmitParticipantResult{}, nexusError(err)
	}
	submission, err := input.Submission.NormalizeForEvent(event)
	if err != nil {
		return SubmitParticipantResult{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "%s", err.Error())
	}
	if event.Status == "archived" {
		return SubmitParticipantResult{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "this event is no longer accepting submissions")
	}
	handle, err := s.Client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: EventID(input.EventID), UpdateID: submission.RequestID + "-" + submission.Digest(),
		UpdateName: "submit", Args: []any{submission}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return SubmitParticipantResult{}, nexusError(err)
	}
	var status string
	if err := handle.Get(ctx, &status); err != nil {
		return SubmitParticipantResult{}, nexusError(err)
	}
	code, err := EnsureFlightPass(ctx, s.Client, s.queue(), input.EventID, submission)
	if err != nil {
		return SubmitParticipantResult{}, nexusError(err)
	}
	return SubmitParticipantResult{Status: status, FlightCode: code}, nil
}

func (s Service) lookupParticipant(ctx context.Context, input LookupParticipantInput, _ nexus.StartOperationOptions) (LookupParticipantResult, error) {
	event, err := s.event(ctx, input.EventID)
	if err != nil {
		return LookupParticipantResult{}, nexusError(err)
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	located, err := (&Activities{Client: s.Client}).LocateLead(ctx, LocateInput{Shards: event.Shards, Email: email})
	if err != nil {
		return LookupParticipantResult{}, nexusError(err)
	}
	if located.Lead == nil {
		return LookupParticipantResult{Found: false}, nil
	}
	return LookupParticipantResult{Found: true, Name: located.Lead.Name, Role: located.Lead.Role, Email: located.Lead.Email}, nil
}

func (s Service) claimFlightPass(ctx context.Context, input ClaimFlightPassInput, _ nexus.StartOperationOptions) (FlightPassProfile, error) {
	code, err := NormalizeFlightCode(input.Code)
	if err != nil {
		return FlightPassProfile{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "%s", err.Error())
	}
	if strings.TrimSpace(input.EventID) == "" {
		return FlightPassProfile{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "event id is required")
	}
	return ClaimFlightPass(ctx, s.Client, input.EventID, code)
}

func (s Service) event(ctx context.Context, id string) (Event, error) {
	var event Event
	result, err := s.Client.QueryWorkflow(ctx, EventID(id), "", "event")
	if err != nil {
		return event, err
	}
	if err := result.Get(&event); err != nil {
		return event, err
	}
	return event.At(time.Now()), nil
}

func (s Service) queue() string {
	if s.TaskQueue != "" {
		return s.TaskQueue
	}
	return DefaultTaskQueue
}

func nexusError(err error) error {
	if err == nil {
		return nil
	}
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeNotFound, "not found")
	}
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		switch app.Type() {
		case "Invalid", "BadInput", "Closed":
			return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "%s", app.Message())
		case "Claimed":
			return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeConflict, "%s", app.Message())
		case "Expired":
			return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeNotFound, "%s", app.Message())
		}
	}
	return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "event tracker request failed")
}

func publicEvent(event Event) PublicEvent {
	return PublicEvent{ID: event.ID, Name: event.Name, Description: event.Description, EndDate: event.EndDate, Status: event.Status, ClosesAt: event.ClosesAt}
}

func EnsureFlightPass(ctx context.Context, c client.Client, taskQueue, eventID string, submission Submission) (string, error) {
	var last error
	for salt := 0; salt < 32; salt++ {
		code := FlightCode(eventID, submission.RequestID, salt)
		_, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: FlightPassID(eventID, code), TaskQueue: taskQueue,
			WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		}, FlightPassWorkflow, FlightPassInput{
			EventID: eventID, Code: code, Name: submission.Name, Role: submission.Role, Email: submission.Email,
			RequestID: submission.RequestID, ExpiresAt: time.Now().Add(FlightPassLifetime),
		})
		if err == nil {
			return code, nil
		}
		var already *serviceerror.WorkflowExecutionAlreadyStarted
		if !errors.As(err, &already) {
			return "", err
		}
		owner, qerr := flightPassOwner(ctx, c, eventID, code)
		if qerr == nil && owner == submission.RequestID {
			return code, nil
		}
		last = err
	}
	if last == nil {
		last = errors.New("could not issue a flight code")
	}
	return "", last
}

func flightPassOwner(ctx context.Context, c client.Client, eventID, code string) (string, error) {
	value, err := c.QueryWorkflow(ctx, FlightPassID(eventID, code), "", "request")
	if err != nil {
		return "", err
	}
	var requestID string
	err = value.Get(&requestID)
	return requestID, err
}

func ClaimFlightPass(ctx context.Context, c client.Client, eventID, code string) (FlightPassProfile, error) {
	handle, err := c.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: FlightPassID(eventID, code), UpdateID: "claim",
		UpdateName: "claim", WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return FlightPassProfile{}, nexusError(err)
	}
	var profile FlightPassProfile
	if err := handle.Get(ctx, &profile); err != nil {
		return FlightPassProfile{}, nexusError(err)
	}
	return profile, nil
}
