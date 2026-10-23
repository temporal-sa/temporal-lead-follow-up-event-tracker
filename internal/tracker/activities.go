package tracker

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/client"
)

type Activities struct{ Client client.Client }

func (a *Activities) LocateLead(ctx context.Context, input LocateInput) (LocatedLead, error) {
	id, lead, err := FindLead(ctx, a.Client, input.Shards, input.Email)
	if err != nil || lead == nil {
		return LocatedLead{}, err
	}
	return LocatedLead{ShardID: id, Found: true, RequestID: lead.RequestID, FirstSubmittedAt: lead.FirstSubmittedAt, Digest: lead.Digest()}, nil
}

// FindLead returns the shard holding email and the participant's full record, or a nil record.
func FindLead(ctx context.Context, c client.Client, shards []string, email string) (string, *Lead, error) {
	for _, id := range shards {
		value, err := c.QueryWorkflow(ctx, id, "", "find", email)
		if err != nil {
			return "", nil, err
		}
		var lead *Lead
		if err := value.Get(&lead); err != nil {
			return "", nil, err
		}
		if lead != nil {
			return id, lead, nil
		}
	}
	return "", nil, nil
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
