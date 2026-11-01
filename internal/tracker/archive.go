package tracker

import (
	"context"
	"errors"
)

// Archive is the permanent record of events and participants. It outlives the
// Temporal namespace; workflows are its only writers, through Activities.
type Archive interface {
	// Import copies an event exported from its workflows. It is idempotent, and it
	// skips an event whose stored revision already matches.
	Import(context.Context, Snapshot) (ImportResult, error)
	// Load returns everything stored for an event, or ErrNotArchived.
	Load(context.Context, string) (Snapshot, error)
}

// Snapshot is one event and everything that belongs to it.
type Snapshot struct {
	Event        Event     `json:"event"`
	Sequence     int64     `json:"sequence"`
	Receipts     []Receipt `json:"receipts"`
	Participants []Lead    `json:"participants"`
	// Links are the slugs that route to the event: its ID and the aliases it still owns.
	Links []string `json:"links"`
}

type ImportResult struct {
	Unchanged bool `json:"unchanged,omitempty"`
	// LinkConflicts are slugs another archived event already owns.
	LinkConflicts []string `json:"linkConflicts,omitempty"`
}

var ErrNotArchived = errors.New("event is not in the archive")
