package tracker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/nexus-rpc/sdk-go/nexus"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
)

const (
	NexusServiceName = "event-leads"

	OperationCreateEvent       = "create_event"
	OperationGetEvent          = "get_event"
	OperationSubmitParticipant = "submit_participant"
	OperationLookupParticipant = "lookup_participant"
)

type Service struct {
	Client    client.Client
	TaskQueue string
	Store     Reader
}

func (s Service) Register(w worker.Worker) {
	svc := nexus.NewService(NexusServiceName)
	svc.MustRegister(
		nexus.NewSyncOperation(OperationCreateEvent, s.createEvent),
		nexus.NewSyncOperation(OperationGetEvent, s.getEvent),
		nexus.NewSyncOperation(OperationSubmitParticipant, s.submitParticipant),
		nexus.NewSyncOperation(OperationLookupParticipant, s.lookupParticipant),
	)
	w.RegisterNexusService(svc)
}

type PublicEvent struct {
	ID          string    `json:"id"`
	Link        string    `json:"link"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	EndDate     string    `json:"endDate"`
	Status      string    `json:"status"`
	ClosesAt    time.Time `json:"closesAt"`
}

// CreateEventInput is the admin create form, plus who asked and the caller's
// ID for this create. A retry with the same RequestID returns the same event.
type CreateEventInput struct {
	CreateEvent
	RequestID string `json:"requestId"`
	CreatedBy string `json:"createdBy"`
}

type GetEventInput struct {
	EventID string `json:"eventId"`
}

type SubmitParticipantInput struct {
	EventID    string     `json:"eventId"`
	Submission Submission `json:"submission"`
}

type SubmitParticipantResult struct {
	Status string `json:"status"`
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

// createEvent takes the caller's details as given; the tracker adds no
// defaults beyond what the admin form gets. Nexus carries no sign-in, so the
// endpoint's caller allowlist decides who may create.
func (s Service) createEvent(ctx context.Context, input CreateEventInput, _ nexus.StartOperationOptions) (PublicEvent, error) {
	requestID := strings.TrimSpace(input.RequestID)
	if requestID == "" || len(requestID) > 128 {
		return PublicEvent{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "request id is required")
	}
	if writesPaused.Load() {
		return PublicEvent{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeUnavailable, "changes are paused for maintenance; retry in a few minutes")
	}
	event, err := NewEvent("pending", strings.TrimSpace(input.CreatedBy), input.CreateEvent, time.Now())
	if err != nil {
		return PublicEvent{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "%s", err.Error())
	}
	event.CreateRequestID = requestID
	create := func(ctx context.Context, event Event) error { return StartEvent(ctx, s.Client, s.queue(), event) }
	owned := func(ctx context.Context, slug string) (bool, error) { return CreatedBy(ctx, s.Store, slug, requestID) }
	event, err = CreateWithSlug(ctx, event, create, owned)
	if err != nil {
		return PublicEvent{}, nexusError(err)
	}
	return publicEvent(event.At(time.Now())), nil
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
	// Nexus callers carry no employee sign-in to answer an internal-only form with.
	if event.InternalOnly {
		return SubmitParticipantResult{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "this event only takes responses from signed-in Temporal employees")
	}
	submission, err := input.Submission.WithoutStaleAnswers(event).NormalizeForEvent(event)
	if err != nil {
		return SubmitParticipantResult{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "%s", err.Error())
	}
	if event.Status == "archived" {
		return SubmitParticipantResult{}, nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "this event is no longer accepting submissions")
	}
	handle, err := s.Client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: EventID(event.ID), UpdateID: submission.RequestID + "-" + submission.Digest(),
		UpdateName: "submit", Args: []any{submission}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return SubmitParticipantResult{}, nexusError(err)
	}
	var status string
	if err := handle.Get(ctx, &status); err != nil {
		return SubmitParticipantResult{}, nexusError(err)
	}
	return SubmitParticipantResult{Status: status}, nil
}

func (s Service) lookupParticipant(ctx context.Context, input LookupParticipantInput, _ nexus.StartOperationOptions) (LookupParticipantResult, error) {
	event, err := s.event(ctx, input.EventID)
	if err != nil {
		return LookupParticipantResult{}, nexusError(err)
	}
	email := strings.ToLower(strings.TrimSpace(input.Email))
	lead, err := s.Store.Participant(ctx, event.ID, email)
	if err != nil {
		return LookupParticipantResult{}, nexusError(err)
	}
	if lead == nil {
		return LookupParticipantResult{Found: false}, nil
	}
	return LookupParticipantResult{Found: true, Name: lead.Name, Role: lead.Role, Email: lead.Email}, nil
}

// event resolves any of an event's links, with its status as of now.
func (s Service) event(ctx context.Context, slug string) (Event, error) {
	event, err := s.Store.Resolve(ctx, slug)
	if err != nil {
		return Event{}, err
	}
	if event.DeletedAt != nil {
		return Event{}, ErrNotStored
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
	if errors.Is(err, ErrNotStored) || errors.As(err, &missing) {
		return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeNotFound, "not found")
	}
	var app *temporal.ApplicationError
	if errors.As(err, &app) {
		switch app.Type() {
		case "Invalid", "BadInput", "Closed":
			return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeBadRequest, "%s", app.Message())
		case "AlreadyResponded", "Taken":
			return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeConflict, "%s", app.Message())
		case "Paused":
			return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeUnavailable, "%s", app.Message())
		}
	}
	return nexus.NewHandlerErrorf(nexus.HandlerErrorTypeInternal, "event tracker request failed")
}

func publicEvent(event Event) PublicEvent {
	return PublicEvent{ID: event.ID, Link: event.Link(), Name: event.Name, Description: event.Description, EndDate: event.EndDate, Status: event.Status, ClosesAt: event.ClosesAt}
}
