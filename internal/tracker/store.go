package tracker

import (
	"context"
	"errors"
)

// Reader reads the permanent record of events and participants. Events come
// back as stored; callers derive their status with At.
type Reader interface {
	// Event returns the event with this ID, or ErrNotStored.
	Event(ctx context.Context, id string) (Event, error)
	// Resolve returns the event any of its links routes to, or ErrNotStored.
	Resolve(ctx context.Context, slug string) (Event, error)
	// Events pages through every event, newest first.
	Events(ctx context.Context, cursor string) ([]Event, string, error)
	// OpenEvents pages through events that aren't deleted and haven't completed,
	// soonest completion first.
	OpenEvents(ctx context.Context, cursor string) ([]Event, string, error)
	// Participants pages through an event's participants in first-submitted order.
	Participants(ctx context.Context, eventID, cursor string, limit int) ([]Lead, string, error)
	// Participant returns the participant with this email, or nil.
	Participant(ctx context.Context, eventID, email string) (*Lead, error)
	Health(ctx context.Context) error
}

// Store is the permanent record of events and participants. It outlives the
// Temporal namespace. Event workflows are its only writers, through Activities;
// every write is fenced by the writing run's epoch.
type Store interface {
	Reader
	// Create stores a new event and reserves its ID as a link, or returns ErrTaken.
	Create(ctx context.Context, state EventState) error
	// Claim makes the caller the event's only writer and returns its state with
	// the new epoch, or ErrNotStored.
	Claim(ctx context.Context, id string) (EventState, error)
	// SaveEvent stores the event's details.
	SaveEvent(ctx context.Context, state EventState) error
	// SaveSlug reserves slug for the event and stores its details, or returns ErrTaken.
	SaveSlug(ctx context.Context, state EventState, slug string) error
	// Lookup returns what a submission needs about an email and a request ID.
	Lookup(ctx context.Context, eventID, email, requestID string) (Existing, error)
	// SaveSubmission stores a participant, the request's receipt, and the event's new count.
	SaveSubmission(ctx context.Context, state EventState, lead Lead, receipt Receipt) error
}

var (
	ErrNotStored     = errors.New("event not found")
	ErrTaken         = errors.New("that event link is already in use")
	ErrFenced        = errors.New("another workflow run now owns this event")
	ErrInvalidCursor = errors.New("invalid pagination cursor")
)
