package tracker

import (
	"time"

	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func EventWorkflow(ctx workflow.Context, state EventState) (Event, error) {
	operations := 0
	rotating, retiring := false, false
	mutex := workflow.NewMutex(ctx)
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: 10 * time.Second},
	})
	if err := workflow.SetQueryHandler(ctx, "event", func() (Event, error) { return state.Event.At(workflow.Now(ctx)), nil }); err != nil {
		return Event{}, err
	}
	validator := func() error {
		if rotating {
			return temporal.NewApplicationError("event is refreshing; retry shortly", "Refreshing")
		}
		if retiring {
			return temporal.NewApplicationError("event is completing; submissions are closed", "Closed")
		}
		return nil
	}
	// Deleted events stay in history, but updates other than delete itself are rejected.
	accepting := func() error {
		if state.Event.DeletedAt != nil {
			return temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		return validator()
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "submit", func(ctx workflow.Context, input Submission) (string, error) {
		if err := mutex.Lock(ctx); err != nil {
			return "", err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return "", temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		input, err := input.NormalizeForEvent(state.Event)
		if err != nil {
			return "", temporal.NewApplicationError(err.Error(), "Invalid")
		}
		digest := input.Digest()
		// Temporal Update IDs are per run; carry a bounded retry window across runs.
		for _, receipt := range state.Receipts {
			if receipt.ID == input.RequestID {
				if receipt.Digest != digest {
					return "", temporal.NewApplicationError("request ID was already used for different details", "Invalid")
				}
				return "saved", nil
			}
		}
		if state.Event.At(workflow.Now(ctx)).Status != "open" {
			return "", temporal.NewApplicationError("this event is no longer accepting submissions", "Closed")
		}
		state.Event.Busy = true
		defer func() { state.Event.Busy = false }()
		var existing LocatedLead
		if err := workflow.ExecuteActivity(activityCtx, "LocateLead", LocateInput{Shards: state.Event.Shards, Email: input.Email}).Get(ctx, &existing); err != nil {
			return "", err
		}
		if existing.Lead != nil && existing.Lead.RequestID == input.RequestID {
			changed := existing.Lead.Name != input.Name || existing.Lead.Role != input.Role || existing.Lead.Reason != input.Reason
			if state.Event.Form != nil {
				prior := Submission{Name: existing.Lead.Name, Role: existing.Lead.Role, Email: existing.Lead.Email, Reason: existing.Lead.Reason, Answers: existing.Lead.Answers}
				changed = prior.Digest() != digest
			}
			if changed {
				return "", temporal.NewApplicationError("request ID was already used for different details", "Invalid")
			}
			return "saved", nil
		}
		state.Sequence++
		state.Event.Revision++
		now := workflow.Now(ctx)
		lead := Lead{Name: input.Name, Role: input.Role, Email: input.Email, Reason: input.Reason, Answers: input.Answers, FirstSubmittedAt: now, LastSubmittedAt: now, Sequence: state.Sequence, RequestID: input.RequestID}
		if existing.Lead != nil {
			lead.FirstSubmittedAt = existing.Lead.FirstSubmittedAt
		}
		target := existing.ShardID
		if target == "" && len(state.Event.Shards) > 0 {
			target = state.Event.Shards[len(state.Event.Shards)-1]
		}
		createShard := func() (string, error) {
			id := ShardID(state.Event.ID, len(state.Event.Shards))
			childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{WorkflowID: id, ParentClosePolicy: enums.PARENT_CLOSE_POLICY_ABANDON})
			child := workflow.ExecuteChildWorkflow(childCtx, ParticipantShardWorkflow, ShardState{EventID: state.Event.ID, Leads: []Lead{}})
			if err := child.GetChildWorkflowExecution().Get(ctx, nil); err != nil {
				return "", err
			}
			state.Event.Shards = append(state.Event.Shards, id)
			return id, nil
		}
		if target == "" {
			target, err = createShard()
			if err != nil {
				return "", err
			}
		}
		var result WriteResult
		if err := workflow.ExecuteActivity(activityCtx, "WriteLead", WriteCommand{ShardID: target, Lead: lead}).Get(ctx, &result); err != nil {
			return "", err
		}
		if result.Full {
			// The original remains readable until its replacement is durably confirmed.
			target, err = createShard()
			if err != nil {
				return "", err
			}
			if err := workflow.ExecuteActivity(activityCtx, "WriteLead", WriteCommand{ShardID: target, Lead: lead}).Get(ctx, &result); err != nil {
				return "", err
			}
			if result.Full {
				return "", temporal.NewApplicationError("participant exceeds shard capacity", "Invalid")
			}
			if existing.ShardID != "" {
				if err := workflow.ExecuteActivity(activityCtx, "WriteLead", WriteCommand{ShardID: existing.ShardID, Lead: lead, Delete: true}).Get(ctx, &result); err != nil {
					return "", err
				}
			}
		}
		if existing.Lead == nil {
			state.Event.Count++
		}
		state.Receipts = append(state.Receipts, Receipt{ID: input.RequestID, Digest: digest})
		if len(state.Receipts) > RecentRequests {
			state.Receipts = state.Receipts[len(state.Receipts)-RecentRequests:]
		}
		operations++
		state.Event.Revision++
		return "saved", nil
	}, workflow.UpdateHandlerOptions{Validator: func(Submission) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "end", func(ctx workflow.Context) (Event, error) {
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		if state.Event.EndedAt == nil && workflow.Now(ctx).Before(state.Event.ClosesAt) {
			now := workflow.Now(ctx)
			state.Event.EndedAt = &now
			state.Event.Revision++
			operations++
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: accepting}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "banner", func(ctx workflow.Context, input string) (Event, error) {
		banner, err := NormalizeBanner(input)
		if err != nil {
			return Event{}, temporal.NewApplicationError(err.Error(), "Invalid")
		}
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt != nil {
			return Event{}, temporal.NewApplicationError("this event is no longer available", "Closed")
		}
		if state.Event.QRBanner != banner {
			state.Event.QRBanner = banner
			state.Event.Revision++
			operations++
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: func(string) error { return accepting() }}); err != nil {
		return Event{}, err
	}
	// Registering an additional handler emits no commands and preserves existing histories.
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "delete", func(ctx workflow.Context) (Event, error) {
		if err := mutex.Lock(ctx); err != nil {
			return Event{}, err
		}
		defer mutex.Unlock()
		if state.Event.DeletedAt == nil {
			now := workflow.Now(ctx)
			state.Event.DeletedAt = &now
			state.Event.Revision++
			operations++
		}
		return state.Event.At(workflow.Now(ctx)), nil
	}, workflow.UpdateHandlerOptions{Validator: validator}); err != nil {
		return Event{}, err
	}
	// Preserve the extra timer only when replaying histories from before this change.
	timerVersion := workflow.GetVersion(ctx, "event-completion-timer-only", workflow.DefaultVersion, 1)
	if timerVersion == workflow.DefaultVersion && workflow.Now(ctx).Before(state.Event.ClosesAt) {
		workflow.Go(ctx, func(ctx workflow.Context) { _ = workflow.Sleep(ctx, state.Event.ClosesAt.Sub(workflow.Now(ctx))) })
	}
	// Completion starts Temporal retention; queries and exports remain available.
	remaining := state.Event.CompletesAt.Sub(workflow.Now(ctx))
	if remaining > 0 {
		refresh, err := workflow.AwaitWithTimeout(ctx, remaining, func() bool {
			return operations >= HistoryOperations || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
		})
		if err != nil {
			return Event{}, err
		}
		if refresh {
			rotating = true
			if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
				return Event{}, err
			}
			return Event{}, workflow.NewContinueAsNewError(ctx, EventWorkflow, state)
		}
	}
	retiring = true
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return Event{}, err
	}
	for _, id := range state.Event.Shards {
		if err := workflow.SignalExternalWorkflow(ctx, id, "", "finish", nil).Get(ctx, nil); err != nil {
			return Event{}, err
		}
	}
	return state.Event.At(workflow.Now(ctx)), nil
}

func ParticipantShardWorkflow(ctx workflow.Context, state ShardState) (int, error) {
	operations := 0
	finish := workflow.GetSignalChannel(ctx, "finish")
	rotating := false
	if err := workflow.SetQueryHandler(ctx, "find", func(email string) (*Lead, error) { return state.Find(email), nil }); err != nil {
		return 0, err
	}
	if err := workflow.SetQueryHandler(ctx, "page", func(offset, limit int) (ShardPage, error) {
		if offset < 0 {
			offset = 0
		}
		if offset > len(state.Leads) {
			offset = len(state.Leads)
		}
		if limit < 1 || limit > PageSize {
			limit = PageSize
		}
		end := min(offset+limit, len(state.Leads))
		return ShardPage{Leads: state.Leads[offset:end], Total: len(state.Leads)}, nil
	}); err != nil {
		return 0, err
	}
	if err := workflow.SetUpdateHandlerWithOptions(ctx, "write", func(ctx workflow.Context, command WriteCommand) (WriteResult, error) {
		result, err := state.Apply(command)
		if !result.Full && err == nil {
			operations++
		}
		return result, err
	}, workflow.UpdateHandlerOptions{Validator: func(WriteCommand) error {
		if rotating {
			return temporal.NewApplicationError("shard is refreshing", "Refreshing")
		}
		if finish.Len() > 0 {
			return temporal.NewApplicationError("shard has finished", "Closed")
		}
		return nil
	}}); err != nil {
		return 0, err
	}
	if err := workflow.Await(ctx, func() bool {
		return finish.Len() > 0 || operations >= HistoryOperations || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
	}); err != nil {
		return 0, err
	}
	rotating = true
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return 0, err
	}
	// Signals are not covered by AllHandlersFinished; drain finish before continuing.
	if finish.ReceiveAsync(nil) {
		return len(state.Leads), nil
	}
	return 0, workflow.NewContinueAsNewError(ctx, ParticipantShardWorkflow, state)
}
