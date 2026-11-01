package tracker

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// memStore is an in-memory Store with the DynamoDB store's rules: a taken ID or
// link is refused, writes from an earlier epoch or revision are refused, and a
// retry of the same write succeeds. The DynamoDB store has its own tests in
// internal/store.
type memStore struct {
	mu           sync.Mutex
	events       map[string]EventState
	links        map[string]string
	participants map[string]map[string]Lead
	receipts     map[string]map[string]Receipt
	// now decides which events are open; it defaults to the wall clock.
	now func() time.Time
}

func newMemStore() *memStore {
	return &memStore{events: map[string]EventState{}, links: map[string]string{}, participants: map[string]map[string]Lead{}, receipts: map[string]map[string]Receipt{}}
}

func (m *memStore) fenced(state EventState) bool {
	current, ok := m.events[state.Event.ID]
	return !ok || current.Epoch != state.Epoch || current.Event.Revision > state.Event.Revision
}

func (m *memStore) Create(_ context.Context, state EventState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := state.Event.ID
	if current, ok := m.events[id]; ok && !(current.Event.CreatedAt.Equal(state.Event.CreatedAt) && current.Epoch == state.Epoch) {
		return ErrTaken
	}
	if owner, ok := m.links[id]; ok && owner != id {
		return ErrTaken
	}
	m.events[id], m.links[id] = state, id
	return nil
}

func (m *memStore) Claim(_ context.Context, id string) (EventState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.events[id]
	if !ok {
		return EventState{}, ErrNotStored
	}
	state.Epoch++
	m.events[id] = state
	return state, nil
}

func (m *memStore) SaveEvent(_ context.Context, state EventState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fenced(state) {
		return ErrFenced
	}
	m.events[state.Event.ID] = state
	return nil
}

func (m *memStore) SaveSlug(_ context.Context, state EventState, slug string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if owner, ok := m.links[slug]; ok && owner != state.Event.ID {
		return ErrTaken
	}
	if m.fenced(state) {
		return ErrFenced
	}
	m.links[slug], m.events[state.Event.ID] = state.Event.ID, state
	return nil
}

func (m *memStore) Lookup(_ context.Context, eventID, email, requestID string) (Existing, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var existing Existing
	if lead, ok := m.participants[eventID][email]; ok {
		existing = Existing{Found: true, RequestID: lead.RequestID, FirstSubmittedAt: lead.FirstSubmittedAt, Digest: lead.Digest()}
	}
	if receipt, ok := m.receipts[eventID][requestID]; ok {
		existing.Receipt = &receipt
	}
	return existing, nil
}

func (m *memStore) SaveSubmission(_ context.Context, state EventState, lead Lead, receipt Receipt) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := state.Event.ID
	if m.fenced(state) {
		return ErrFenced
	}
	if current, ok := m.participants[id][lead.Email]; ok && current.Sequence > lead.Sequence {
		return ErrFenced
	}
	if current, ok := m.receipts[id][receipt.ID]; ok && current.Digest != receipt.Digest {
		return ErrFenced
	}
	if m.participants[id] == nil {
		m.participants[id], m.receipts[id] = map[string]Lead{}, map[string]Receipt{}
	}
	stored := m.events[id]
	stored.Event.Count, stored.Event.Revision, stored.Sequence = state.Event.Count, state.Event.Revision, state.Sequence
	m.events[id], m.participants[id][lead.Email], m.receipts[id][receipt.ID] = stored, lead, receipt
	return nil
}

func (m *memStore) Event(_ context.Context, id string) (Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.events[id]
	if !ok {
		return Event{}, ErrNotStored
	}
	return state.Event, nil
}

func (m *memStore) Resolve(ctx context.Context, slug string) (Event, error) {
	m.mu.Lock()
	id, ok := m.links[slug]
	m.mu.Unlock()
	if !ok {
		return Event{}, ErrNotStored
	}
	return m.Event(ctx, id)
}

func (m *memStore) Events(context.Context, string) ([]Event, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	events := make([]Event, 0, len(m.events))
	for _, state := range m.events {
		events = append(events, state.Event)
	}
	slices.SortFunc(events, func(a, b Event) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return events, "", nil
}

func (m *memStore) OpenEvents(ctx context.Context, cursor string) ([]Event, string, error) {
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	all, _, _ := m.Events(ctx, cursor)
	open := slices.DeleteFunc(all, func(e Event) bool { return e.DeletedAt != nil || !e.CompletesAt.After(now) })
	return open, "", nil
}

func (m *memStore) Participants(_ context.Context, eventID, _ string, _ int) ([]Lead, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	leads := make([]Lead, 0, len(m.participants[eventID]))
	for _, lead := range m.participants[eventID] {
		leads = append(leads, lead)
	}
	slices.SortFunc(leads, func(a, b Lead) int {
		if c := a.FirstSubmittedAt.Compare(b.FirstSubmittedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Email, b.Email)
	})
	return leads, "", nil
}

func (m *memStore) Participant(_ context.Context, eventID, email string) (*Lead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lead, ok := m.participants[eventID][email]; ok {
		return &lead, nil
	}
	return nil, nil
}

func (m *memStore) Health(context.Context) error { return nil }

func (m *memStore) stored(id string) EventState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events[id]
}

var _ Store = (*memStore)(nil)
