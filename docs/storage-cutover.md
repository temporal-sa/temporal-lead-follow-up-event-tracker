# Storage cut-over runbook

Events, links, and participants are moving from Temporal workflow histories to a
DynamoDB table that lives outside the catalog. Afterwards the workflows still own
every change, but the table is the permanent record: it survives a Temporal
namespace reset and the end of retention.

| Phase | What ships | Status |
|---|---|---|
| 0. Infrastructure | `deploy/create-store.sh` creates the table and the worker and web IAM roles | Done |
| A. Copy | The backfill, `WRITES_PAUSED`, and the worker's IAM role | Done; the backfill copied and verified both events |
| Freeze | `WRITES_PAUSED=true`, then the old workflows are terminated and copied one last time | To do |
| B. Switch | Workflows write to the table; reads come from it; a reconciler restarts open events | To do |

Two things about the platform shape the order below:

- The live manifest is `projects/demo/event-leads.yaml` in
  `temporal-sa/tmprl-demo-cloud-registry`. `deploy/demo-project.yaml` here is a copy;
  change both.
- The platform rebuilds and rolls out as soon as anything merges to this repo's `main`,
  with whatever manifest it has. So settings a release needs must land before the
  release merges. PR A's worker crash-looped for about 15 minutes because its
  `EVENT_LEADS_TABLE` arrived after the code.

## Before you start

- `kubectl --context blue` access to the platform cluster. Find the current rollout's
  namespace and worker with:

  ```sh
  kubectl --context blue get ns | grep event-leads
  kubectl --context blue -n <namespace> get deploy
  ```

- Temporal CLI access to the `tmprl-dem-cld-event-leads` namespace in Temporal Cloud,
  for the terminate step.
- Pick a time when no responses are expected: during the freeze, an attendee who
  submits sees "Changes are paused for maintenance" and has to submit again.

## 1. Freeze

1. In the platform repo, set `WRITES_PAUSED` to `"true"` on both components. Merge,
   and wait until the rollout has replaced both pods (`kubectl get pods` shows new ages).
2. Check that changes are refused: submitting a form shows "Changes are paused for
   maintenance. Please retry in a few minutes.", and reads still work.

## 2. Terminate and copy

1. Terminate the event, shard, and alias workflows:

   ```sh
   temporal workflow terminate --namespace tmprl-dem-cld-event-leads.<account> --reason "storage cut-over" \
     --query "WorkflowType IN ('EventWorkflow','ParticipantShardWorkflow','EventAliasWorkflow') AND ExecutionStatus='Running'"
   ```

2. Copy one last time, then check. Queries still work on terminated workflows; the
   check must report no problems:

   ```sh
   kubectl --context blue -n <namespace> exec deploy/<worker> -- /tracker backfill
   kubectl --context blue -n <namespace> exec deploy/<worker> -- /tracker backfill --verify-only
   ```

## 3. Switch

1. Once 30 minutes have passed since the freeze, change the platform manifest to
   match `deploy/demo-project.yaml` from PR B, but keep `WRITES_PAUSED` at `"true"` on
   both components. That moves both components and the Nexus endpoint to the
   `event-leads-store` task queue, and gives web its read-only role and the table.
   Merge, and wait for the rollout. The code is still PR A's, which ignores the
   new web settings and has nothing to run on the new queue.
2. Merge PR B. The platform rebuilds and rolls it out, still paused. Check:
   - The worker logs `worker started` with `taskQueue=event-leads-store`.
   - `event-reconciler` and a running `event/<id>` for every open event are in the
     Temporal UI within a minute.
   - The directory, each event's participants, and its CSV match the table.
3. Set `WRITES_PAUSED` to `"false"` on both components in the platform manifest and
   merge. After the rollout, submit a test response and delete it with its event, or
   use a test event.

If step 3.2 goes wrong, revert PR B on `main`. The platform rolls PR A back out on the
new queue. PR A's pages read through workflow Queries, and the terminated workflows
have no worker on the old queue to answer them, so pages can't load event data until
PR B is fixed and merged. Nothing in the table changes, and nothing is lost.

## Rehearse locally

The whole sequence has been rehearsed with two local builds: PR A from `main` and
PR B from its branch. Both used one Temporal dev server and DynamoDB Local, with
different task queues. The steps:

1. With PR A, create events, submissions, a resubmission, a renamed link, an
   internal-only response, and a soft-deleted event. Run the backfill.
2. Restart PR A with `WRITES_PAUSED=true`. Check that a submission gets 503.
3. Terminate with the query above (scoped to the rehearsal's task queue). Run the
   backfill and `--verify-only`.
4. Start PR B, still paused, on the new queue. The reconciler starts the open events,
   but not the deleted one. Reads, links, and counts all come from the table.
5. Restart PR B unpaused and check:
   - The CSV is identical to the one exported before the freeze.
   - A new submission through the renamed link works.
   - A pre-freeze request ID is still acknowledged, and reusing it with new details is refused.
   - The internal-only event refuses a second response.
   - An edit saves.
   - Creating an event with a taken name gets the `-2` suffix.

Point the Temporal CLI at a local server with `--address 127.0.0.1:<port>`. If your
CLI profile enables TLS or a login, add `TEMPORAL_CONFIG_FILE=/dev/null` in front of
each command.
