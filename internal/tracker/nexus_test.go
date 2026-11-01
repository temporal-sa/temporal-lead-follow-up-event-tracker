package tracker

import (
	"context"
	"errors"
	"testing"

	"github.com/nexus-rpc/sdk-go/nexus"
	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/mocks"
)

func TestNexusSubmitRejectsInternalOnlyEvents(t *testing.T) {
	store := newMemStore()
	event := testEvent()
	event.ID, event.InternalOnly = "summit", true
	event.ClosesAt, event.CompletesAt = event.ClosesAt.AddDate(100, 0, 0), event.CompletesAt.AddDate(100, 0, 0)
	store.events["summit"], store.links["summit"] = EventState{Event: event}, "summit"
	c := &mocks.Client{}
	input := SubmitParticipantInput{EventID: "summit", Submission: Submission{Name: "Zoe", Role: "SA", Email: "zoe@temporal.io", RequestID: "01234567-89ab-cdef"}}
	_, err := Service{Client: c, Store: store}.submitParticipant(context.Background(), input, nexus.StartOperationOptions{})
	var handlerError *nexus.HandlerError
	if !errors.As(err, &handlerError) || handlerError.Type != nexus.HandlerErrorTypeBadRequest {
		t.Fatalf("err = %v", err)
	}
	c.AssertNotCalled(t, "UpdateWorkflow", mock.Anything, mock.Anything)
}

func TestNexusReadsComeFromTheStore(t *testing.T) {
	store := newMemStore()
	open, ended := testEvent(), testEvent()
	open.ID, ended.ID = "open", "ended"
	open.ClosesAt, open.CompletesAt = open.ClosesAt.AddDate(100, 0, 0), open.CompletesAt.AddDate(100, 0, 0)
	ended.CompletesAt = ended.CompletesAt.AddDate(100, 0, 0)
	for _, event := range []Event{open, ended} {
		store.events[event.ID], store.links[event.ID] = EventState{Event: event}, event.ID
	}
	store.links["renamed"] = "open"
	store.participants["open"] = map[string]Lead{"p@example.com": {Name: "Pat", Role: "SA", Email: "p@example.com"}}
	service := Service{Store: store}
	ctx := context.Background()
	list, err := service.listEvents(ctx, ListEventsInput{}, nexus.StartOperationOptions{})
	if err != nil || len(list.Events) != 1 || list.Events[0].ID != "open" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got, err := service.getEvent(ctx, GetEventInput{EventID: "renamed"}, nexus.StartOperationOptions{})
	if err != nil || got.ID != "open" || got.Status != "open" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	found, err := service.lookupParticipant(ctx, LookupParticipantInput{EventID: "open", Email: " P@Example.com "}, nexus.StartOperationOptions{})
	if err != nil || !found.Found || found.Name != "Pat" {
		t.Fatalf("lookup = %+v, %v", found, err)
	}
	_, err = service.getEvent(ctx, GetEventInput{EventID: "missing"}, nexus.StartOperationOptions{})
	var handlerError *nexus.HandlerError
	if !errors.As(err, &handlerError) || handlerError.Type != nexus.HandlerErrorTypeNotFound {
		t.Fatalf("missing: %v", err)
	}
}
