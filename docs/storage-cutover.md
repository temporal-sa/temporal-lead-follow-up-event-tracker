# Storage cut-over runbook

Events, links, and participants are moving from Temporal workflow histories to a
DynamoDB table that lives outside the catalog. Afterwards the workflows still own
every change, but the table is the permanent record: it survives a Temporal
namespace reset and the end of retention.

The move takes three releases, and no data is lost:

| Phase | What ships | What changes for users |
|---|---|---|
| 0. Infrastructure | `deploy/event-leads-store.tf` in the platform repo | Nothing |
| A. Copy | The backfill, `WRITES_PAUSED`, and a worker IAM role | Ended events can't be deleted until phase B |
| Freeze | `WRITES_PAUSED=true`, then the old workflows are terminated | Every change returns "Changes are paused for maintenance" for the window |
| B. Switch | Workflows write to the table; reads come from it; a reconciler restarts open events | None, apart from the participant table ordering by first response |

Retained events leave Temporal when their retention ends, so finish phase A and
run the backfill as soon as possible. Repeat the backfill until the freeze.

## Before you start

- Temporal CLI access to the project's namespace (`tmprl-dem-cld-event-leads`), and its API key.
- A checkout of this repository, to run `tracker backfill`. The command only starts
  a workflow; the deployed worker does the copying, so you need no AWS access.
- Commands below use this helper. Fill in the address and key:

  ```sh
  export TEMPORAL_ADDRESS=<namespace gRPC endpoint> TEMPORAL_NAMESPACE=tmprl-dem-cld-event-leads
  export TEMPORAL_API_KEY=<key> TEMPORAL_TASK_QUEUE=event-leads-unversioned
  go build -o bin/tracker ./cmd/tracker
  ```

## Phase 0: infrastructure

1. Copy `deploy/event-leads-store.tf` into the platform repository's `infra/`.
2. Run `tofu plan` and confirm it only adds the table and the two
   `tmprl-dem-cld-event-leads-{web,worker}` roles and their policies. Then apply it.
3. Check the outputs: `event_leads_table` is `tmprl-dem-cld-event-leads`, and the role
   ARNs match the `awsIamRoleArn` values in `deploy/demo-project.yaml`.

## Phase A: copy

1. Merge and publish PR A (`deploy/publish.sh` copies `deploy/demo-project.yaml`).
2. When the rollout is healthy, run the backfill:

   ```sh
   bin/tracker backfill
   ```

   It prints a report and exits non-zero if any event failed or doesn't match:

   ```json
   {"imported": 12, "unchanged": 0, "verified": 0, "noted": [ ... ]}
   ```

   - `imported` events were written; `unchanged` ones already matched their revision.
   - `noted` lists events that copied but needed reconciling, such as a participant
     found in two shards during a relocation or an alias another event now owns.
     Read them; none of them lose data.
   - `problems` lists events that failed or don't match. Fix the cause (usually a
     worker that wasn't reachable) and run it again. An event that has left retention
     can't be copied: check whether it matters before continuing.
3. Run it again whenever you like; each run copies only events that changed.

## Freeze

Pick a time when no event is open (no event's last day is today or later in UTC):
an attendee who submits during the freeze sees the error and has to submit again.

1. Set `WRITES_PAUSED` to `"true"` on both components in `deploy/demo-project.yaml`
   and publish. Wait for the rollout to finish.
2. Confirm that only the new pods poll the queue. Every poller listed should have
   started after the rollout:

   ```sh
   temporal task-queue describe --task-queue event-leads-unversioned
   ```

3. Check that changes are refused. This should fail with `type: Paused`:

   ```sh
   temporal workflow update execute -w event/<any running event> --name banner -i '"check"'
   ```

4. Terminate the event, shard, and alias workflows. Flight passes keep running:

   ```sh
   temporal workflow terminate --reason "storage cut-over" \
     --query "WorkflowType IN ('EventWorkflow','ParticipantShardWorkflow','EventAliasWorkflow') AND ExecutionStatus='Running'"
   ```

5. Run the final copy and the check. Queries still work on terminated workflows,
   and the check must report no problems:

   ```sh
   bin/tracker backfill
   bin/tracker backfill --verify-only
   ```

6. Wait 30 minutes so any flight passes issued before the freeze expire.

## Phase B: switch

Publish PR B with `WRITES_PAUSED` back at `"false"`. Its worker uses a new task queue,
so it never runs the terminated histories, and its reconciler restarts every open
event from the table. Then check the directory, an event's participants and CSV
against the final `--verify-only` run, and submit a test response.

## Rehearse locally

The same steps work against `make preview`. The rehearsal for PR A created events,
submissions, a renamed link, an internal-only response, and a soft-deleted event;
confirmed that verify-only flags a change made after the copy; restarted with
`WRITES_PAUSED=true`; terminated with the query above; and verified all events
from the terminated histories. Point the CLI at the preview with
`--address 127.0.0.1:7234`. If your Temporal CLI profile enables TLS or a login,
add `TEMPORAL_CONFIG_FILE=/dev/null` in front of each command.
