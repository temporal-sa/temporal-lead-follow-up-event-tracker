package tracker

import (
	"context"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/temporal"
)

func TestCreateEventTriesTheNextSlugWhileTaken(t *testing.T) {
	var tried []string
	create := func(_ context.Context, event Event) error {
		tried = append(tried, event.ID)
		if len(tried) == 1 {
			return serviceerror.NewWorkflowExecutionAlreadyStarted("running", "", "")
		}
		if len(tried) == 2 {
			return temporal.NewApplicationError("taken", "Taken")
		}
		return nil
	}
	event, err := CreateWithSlug(context.Background(), Event{Name: "KubeCon 2026"}, create, nil)
	if err != nil || event.ID != "kubecon-2026-3" || len(tried) != 3 {
		t.Fatalf("event = %q, tried %v, err %v", event.ID, tried, err)
	}
}

func TestCreateEventWithARequestIDReturnsTheEventAnEarlierAttemptMade(t *testing.T) {
	store := newMemStore()
	store.events["kubecon-2026"] = EventState{Event: Event{ID: "kubecon-2026", CreateRequestID: "someone-else"}}
	store.links["kubecon-2026"] = "kubecon-2026"
	store.events["kubecon-2026-2"] = EventState{Event: Event{ID: "kubecon-2026-2", CreateRequestID: "booth-1"}}
	store.links["kubecon-2026-2"] = "kubecon-2026-2"
	create := func(_ context.Context, event Event) error {
		return temporal.NewApplicationError("taken", "Taken")
	}
	owned := func(ctx context.Context, slug string) (bool, error) { return CreatedBy(ctx, store, slug, "booth-1") }
	event, err := CreateWithSlug(context.Background(), Event{Name: "KubeCon 2026", CreateRequestID: "booth-1"}, create, owned)
	if err != nil || event.ID != "kubecon-2026-2" {
		t.Fatalf("event = %q, err %v", event.ID, err)
	}
}
