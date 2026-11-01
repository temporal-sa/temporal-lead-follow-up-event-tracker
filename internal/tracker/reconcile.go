package tracker

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

const (
	ReconcilerWorkflowID = "event-reconciler"
	reconcileInterval    = 5 * time.Minute
	// Each pass adds a handful of events to history; continuing bounds it.
	reconcilePasses = 100
)

// ReconcileWorkflow keeps a workflow running for every open event in the store.
// After a namespace reset it brings every open event back within one pass; the
// worker starts it when it boots.
func ReconcileWorkflow(ctx workflow.Context) error {
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		HeartbeatTimeout:    time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumInterval: time.Minute},
	})
	for pass := 0; pass < reconcilePasses; pass++ {
		if err := workflow.ExecuteActivity(activityCtx, "StartOpenEvents").Get(ctx, nil); err != nil {
			workflow.GetLogger(ctx).Error("reconcile pass failed", "error", err)
		}
		if err := workflow.Sleep(ctx, reconcileInterval); err != nil {
			return err
		}
	}
	return workflow.NewContinueAsNewError(ctx, ReconcileWorkflow)
}
