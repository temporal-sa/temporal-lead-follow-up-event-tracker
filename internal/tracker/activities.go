package tracker

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/client"
)

type Activities struct{ Client client.Client }

func (a *Activities) LocateLead(ctx context.Context, input LocateInput) (LocatedLead, error) {
	for _, id := range input.Shards {
		value, err := a.Client.QueryWorkflow(ctx, id, "", "find", input.Email)
		if err != nil {
			return LocatedLead{}, err
		}
		var lead *Lead
		if err := value.Get(&lead); err != nil {
			return LocatedLead{}, err
		}
		if lead != nil {
			return LocatedLead{ShardID: id, Lead: lead}, nil
		}
	}
	return LocatedLead{}, nil
}

func (a *Activities) WriteLead(ctx context.Context, command WriteCommand) (WriteResult, error) {
	// The same ID is reused for every Activity attempt, including across shard runs.
	handle, err := a.Client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: command.ShardID, UpdateID: fmt.Sprintf("write-%d-%t", command.Lead.Sequence, command.Delete), UpdateName: "write",
		Args: []any{command}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return WriteResult{}, err
	}
	var result WriteResult
	err = handle.Get(ctx, &result)
	return result, err
}
