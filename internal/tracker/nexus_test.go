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
	c := &mocks.Client{}
	value := &mocks.Value{}
	value.On("Get", mock.Anything).Run(func(args mock.Arguments) {
		*args[0].(*Event) = Event{ID: "summit", Status: "open", InternalOnly: true}
	}).Return(nil)
	c.On("QueryWorkflow", mock.Anything, EventID("summit"), "", "event").Return(value, nil)
	input := SubmitParticipantInput{EventID: "summit", Submission: Submission{Name: "Zoe", Role: "SA", Email: "zoe@temporal.io", RequestID: "01234567-89ab-cdef"}}
	_, err := Service{Client: c}.submitParticipant(context.Background(), input, nexus.StartOperationOptions{})
	var handlerError *nexus.HandlerError
	if !errors.As(err, &handlerError) || handlerError.Type != nexus.HandlerErrorTypeBadRequest {
		t.Fatalf("err = %v", err)
	}
	// Only the event query ran: no Update was sent.
	c.AssertExpectations(t)
	c.AssertNotCalled(t, "UpdateWorkflow", mock.Anything, mock.Anything)
}
