# Runbook: Event Lead Tracker

This runbook is for presenting the demo. The [README](README.md) explains what it is and how it's built.

## What it shows

A Temporal employee creates an event, puts its QR code on a booth screen, and attendees scan it and leave their details. The employee watches responses arrive, ends the event, and exports a CSV. There is no database: each event is a Workflow, and its participants live in child Workflows.

The demo makes three problems visible:

- **Collecting leads usually means running a form service and a database.** Here the event, its settings, and every response are Workflow state, read back with Queries. Beats 1, 2 and 7 show it.
- **Phones at a booth retry, resubmit, and submit twice.** Each submission is an Update that the event handles one at a time, deduplicated by email and by request ID, so a retry never creates a second row or overwrites newer details. Beat 3 shows it.
- **An event runs on a schedule.** Submissions close at midnight UTC after the last day, responses stay exportable, and the Workflows finish seven days later, on a durable timer rather than a cron job. Beats 5 and 6 show it.

## Before you start

- **On tmprl-demo.cloud:** open https://event-leads.tmprl-demo.cloud/admin and sign in with your `temporal.io` account through the catalog. The attendee form and QR image are public; everything under `/admin` needs that login.
  - The hosted demo holds real event leads from real events. Create a test event for the walkthrough, use a test email, and delete the event afterwards (see [Reset and tear down](#reset-and-tear-down)). Anyone with access to the project's Temporal Cloud Namespace can read submissions in Workflow histories.
  - To show the Workflows, you need access to the project's Temporal Cloud Namespace. The catalog creates it; this repo doesn't name it.
- **To run it locally:** install Go 1.26 or later and the [Temporal CLI](https://docs.temporal.io/cli), and clone the repo. No accounts or keys are needed.

## Start it

Locally, from the repository root:

```sh
make preview
```

It builds the app, starts its own Temporal dev server on port 7234 with data in `.local/preview/`, and runs the web server and worker with a local employee identity (`developer@temporal.io`). It ignores any Temporal Cloud variables in your shell. When it prints `Local preview: http://localhost:8080/admin`, open that URL. The Temporal UI is at http://localhost:8234.

A healthy start shows **Employee control panel**, **Your events**, `developer@temporal.io` in the header, and either your earlier events or **Start with an event.** If port 8080 is taken, set `PREVIEW_PORT` (and `PREVIEW_TEMPORAL_PORT`, `PREVIEW_TEMPORAL_UI_PORT` for the other two).

The QR code encodes the app's public URL, which locally is `http://localhost:8080`, so a phone can't open it. Locally, open the attendee form in a second browser window with **Open attendee form ↗** instead. On tmprl-demo.cloud, scan it with your phone.

## The walkthrough

### 1. Create an event, and its Workflow starts

1. On **Your events**, click **Create event +**.
2. Fill in **Event name** (for example `Runbook demo`) and **Last day for responses · UTC** (today or later; a past date is rejected). The help text under the date says when submissions close.
3. The form builder starts from the **Contact template**: Name, Title / role, Email, and an optional follow-up question. You can add, reorder, and branch questions; **Preview form ↗** shows the attendee view. Leave it as is for the walkthrough. Optionally set **Banner message** under **Your QR display**.
4. Click **Create event**. The page opens the event with the badge **open** and **Responses** 0.
5. Say: "That event is now a running Workflow. Its name, its form, its close date, and every response it collects are Workflow state. There's no table behind this page."
6. For proof, open the Temporal UI and find `event/<id>` (the ID is in the page URL), type `EventWorkflow`, **Running**. Its history has a timer for seven days after the close date.

### 2. An attendee submits the form

1. Click **Display QR code ↗** to show the full-screen QR display, then either scan it with a phone (hosted) or click **Open attendee form ↗** on the event page (local).
2. Fill in the form with a test email and tap **Request a follow-up →**. The button shows "Saving your details…", then **You're all set.** with the event's confirmation message.
3. Back on the event page, click **Refresh** next to **Participants**. The row appears and **Responses** reads 1.
4. Say: "The form sent an Update to the event's Workflow. The Workflow found where this email belongs, wrote it to a participant Workflow, and only then told the phone it was saved."
5. For proof, open `event/<id>` in the Temporal UI. The history shows a `submit` Update (`WorkflowExecutionUpdateAccepted`, `WorkflowExecutionUpdateCompleted`), the `LocateLead` and `WriteLead` Activities, and, for the first response, a child Workflow `event/<id>/participants/0` of type `ParticipantShardWorkflow`. That child holds the participant records.

### 3. The same person submits again

1. In the attendee form, reload the page, enter the same email in different capitals (for example `Ada@Example.com` after `ada@example.com`) and a different **Title / role**, and submit.
2. Click **Refresh** on the event page. **Responses** still reads 1, and the row shows the new title. Hover the **Last submitted · UTC** cell to see the first submission time.
3. Say: "One person, one row. Emails are normalized and the newest details win. And if a phone on bad Wi-Fi resends the same request, the Workflow recognizes it and doesn't apply it twice."
4. For proof, the event history shows a second `submit` Update, and the shard history shows a second `write` Update for the same email.

### 4. Change the QR banner while the event runs

1. Under **QR DISPLAY**, change **Banner message** and click **Save message**. The page says "QR display message saved."
2. Reload the QR display tab. The new banner shows.
3. Say: "Changing a setting is one more Update to the same Workflow. There's no admin database to keep in sync."

Skip this beat when time is short.

### 5. Export the responses

1. Click **Export CSV ↓**. The page says "Your CSV export is ready." and the browser downloads `<event-name>-leads.csv`.
2. Open it: one row per participant, a column per question with the question ID in the header (`Email [email]`), plus first and last submission times in UTC.
3. Say: "The export reads every participant Workflow with Queries, and checks the event didn't change halfway through. If it did, it retries rather than handing you a half-written file."

### 6. End the event

1. Click **End event**. In **End this event?**, click **End event**.
2. The page says "Event ended. New responses are now closed." The badge changes to **ended** and the **End event** button disappears. **Export CSV ↓** still works.
3. Reload the attendee form. It shows **Submissions have closed**.
4. Say: "Ending stops new responses straight away. Without this click, the event closes itself at midnight UTC after its last day. Either way the Workflow keeps running for seven more days so you can still export, and then it completes on its own."
5. For proof, `event/<id>` is still **Running**. Its history shows an `end` Update, and the seven-day timer from beat 1 is still pending.

### 7. Restart everything, and the data is still there (local)

1. Press `Ctrl+C` in the `make preview` terminal, then run `make preview` again.
2. Open http://localhost:8080/admin. The event, its responses, and its ended status are all there.
3. Say: "I just stopped the app and the Worker. The app keeps nothing of its own; the events and responses came back from Temporal."

On tmprl-demo.cloud you can't restart the components yourself; skip this beat or describe it.

## Show it in code

All links point at commit [`48b1570`](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/commit/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848). Open a file locally with `code -g <path>:<line>`.

### Keep each event's state in its own Workflow

**When to use it:** after beat 1, or when someone asks where the data is stored.

**Where it is:** [`internal/web/gateway.go` lines 50–56](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/web/gateway.go#L50-L56) (`code -g internal/web/gateway.go:50`) and [`internal/tracker/workflows.go` lines 11–21](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L11-L21) (`code -g internal/tracker/workflows.go:11`)

```go
func (g *temporalGateway) Create(ctx context.Context, event tracker.Event) error {
	_, err := g.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: tracker.EventID(event.ID), TaskQueue: g.taskQueue,
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, tracker.EventType, tracker.EventState{Event: event})
	return err
}
```

```go
func EventWorkflow(ctx workflow.Context, state EventState) (Event, error) {
	// ...
	if err := workflow.SetQueryHandler(ctx, "event", func() (Event, error) { return state.Event.At(workflow.Now(ctx)), nil }); err != nil {
		return Event{}, err
	}
```

**What you need to understand:**

- This is the entity Workflow pattern: one Workflow per event, with the Workflow ID `event/<id>`. Its input is the whole event state, and the state lives in the Workflow's variables from then on.
- Every page in the control panel reads the event with the `event` Query ([`gateway.go` lines 91–108](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/web/gateway.go#L91-L108)). A Query is read-only and isn't recorded in the history. It needs a running Worker to answer, which is why the control panel fails to load when the Worker is down.
- The event list is a Visibility search, `WorkflowType = 'EventWorkflow' AND ExecutionStatus IN ('Running', 'Completed')` ([`gateway.go` lines 58–68](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/web/gateway.go#L58-L68)). Visibility is eventually consistent, so a new event can take a moment to appear in the list; its direct link works straight away.
- The customer doesn't run a database, a schema, or a backup job for this. The trade-offs: the data is readable by anyone with access to the Namespace, and it lasts as long as the Namespace's retention period after the Workflows complete.

**Docs:** [Entity Workflow pattern](https://docs.temporal.io/design-patterns/entity-workflow), [Go: Queries](https://docs.temporal.io/develop/go/workflows/message-passing#queries)

### Accept each submission as an Update, one at a time

**When to use it:** after beat 2 or 3, or when someone asks what happens when two hundred people scan at once.

**Where it is:** [`internal/tracker/workflows.go` lines 38–62](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L38-L62) (`code -g internal/tracker/workflows.go:38`), sent from [`internal/web/gateway.go` lines 110–120](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/web/gateway.go#L110-L120)

```go
if err := workflow.SetUpdateHandlerWithOptions(ctx, "submit", func(ctx workflow.Context, input Submission) (string, error) {
	if err := mutex.Lock(ctx); err != nil {
		return "", err
	}
	defer mutex.Unlock()
	// ...
	input, err := input.NormalizeForEvent(state.Event)
	// ...
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
```

```go
func (g *temporalGateway) Submit(ctx context.Context, id string, submission tracker.Submission) error {
	handle, err := g.client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: tracker.EventID(id), UpdateID: submission.RequestID + "-" + submission.Digest(),
		UpdateName: "submit", Args: []any{submission}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
```

**What you need to understand:**

- A submission is an Update, so the phone waits until the Workflow has written the participant and returns "saved". A Signal couldn't tell the phone whether it worked.
- The handler runs Activities, so several submissions could interleave while one waits. `workflow.NewMutex` makes them take turns. That is what keeps the count and the email deduplication correct under a burst.
- The form creates one request ID per attempt and reuses it when you resubmit the same details after an error ([`internal/web/static/app.js` lines 95–97](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/web/static/app.js#L95-L97)). The server uses it in the Update ID, and Temporal deduplicates Updates with the same ID within one Workflow run.
- That deduplication doesn't survive Continue-As-New, so the Workflow also keeps the last 1,024 request IDs in its state (`Receipts`) and carries them into each new run. Past that window, a replayed request counts as a new submission, and the email still keeps it to one participant.
- The validator on [line 139](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L139) rejects submissions after the event is deleted, and while the Workflow is about to Continue-As-New (the attendee sees "Event is refreshing. Please retry shortly.") or complete. A rejected Update isn't written to the history.

**Docs:** [Go: Update handlers and validators](https://docs.temporal.io/develop/go/workflows/message-passing#updates), [Exactly-once message processing](https://docs.temporal.io/handling-messages#exactly-once-message-processing)

### Spread participants across child Workflows

**When to use it:** when someone asks how many attendees one event can hold, or whether one Workflow gets too big.

**Where it is:** [`internal/tracker/workflows.go` lines 87–106](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L87-L106) (`code -g internal/tracker/workflows.go:87`), with limits in [`internal/tracker/model.go` lines 16–25](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/model.go#L16-L25)

```go
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
```

```go
const (
	EventType         = "EventWorkflow"
	ShardType         = "ParticipantShardWorkflow"
	DefaultTaskQueue  = "event-leads"
	MaxParticipants   = 5000
	MaxShardBytes     = 1024 * 1024
	HistoryOperations = 500
	RecentRequests    = 1024
	PageSize          = 100
)
```

**What you need to understand:**

- The event Workflow keeps counts and a list of shard IDs. The participant records live in `ParticipantShardWorkflow` children, `event/<id>/participants/0`, `/1`, and so on.
- A shard holds up to 5,000 unique emails or about 1 MiB of state, whichever comes first. When a write reports the shard is full, the event starts the next shard and writes there ([lines 107–128](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L107-L128)). The limit exists because Workflow state is carried through Continue-As-New as a payload, and payloads have a size limit.
- `PARENT_CLOSE_POLICY_ABANDON` keeps the shards running when the event Workflow continues as new or completes. At the very end the event Signals each shard `finish` ([lines 224–228](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L224-L228)).
- A walkthrough event never gets past shard 0. The sharding is there so a large event doesn't need a different design.

**Docs:** [Child Workflows](https://docs.temporal.io/child-workflows), [Parent Close Policy](https://docs.temporal.io/parent-close-policy)

### Make a retried write harmless

**When to use it:** after the previous entry, when someone asks what happens if a write times out and the Activity retries.

**Where it is:** [`internal/tracker/activities.go` lines 29–41](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/activities.go#L29-L41) (`code -g internal/tracker/activities.go:29`) and [`internal/tracker/model.go` lines 219–223](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/model.go#L219-L223)

```go
func (a *Activities) WriteLead(ctx context.Context, command WriteCommand) (WriteResult, error) {
	// The same ID is reused for every Activity attempt, including across shard runs.
	handle, err := a.Client.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID: command.ShardID, UpdateID: fmt.Sprintf("write-%d-%t", command.Lead.Sequence, command.Delete), UpdateName: "write",
		Args: []any{command}, WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	// ...
}
```

```go
// Apply performs no I/O and is also used to exercise the actual capacity boundary.
func (s *ShardState) Apply(c WriteCommand) (WriteResult, error) {
	if c.Lead.Sequence <= s.LastSequence {
		return WriteResult{}, nil
	}
```

**What you need to understand:**

- The event writes to a shard through an Activity that sends the shard an Update. The Activity retries with no limit on attempts, starting 1 second after a failure and backing off to at most 10 seconds between attempts ([`workflows.go` lines 15–18](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L15-L18)).
- Every write carries a sequence number that the event Workflow increments. The Update ID is built from it, so a retry within the same shard run is deduplicated by Temporal.
- The shard also remembers the highest sequence it has applied. A late retry that arrives after a newer write, or after the shard has continued as new, is ignored. That's what stops an old attempt overwriting newer details.

### Close, complete, and refresh the event on its own timer

**When to use it:** after beat 6, or when someone asks what runs the schedule.

**Where it is:** [`internal/tracker/workflows.go` lines 198–230](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/workflows.go#L198-L230) (`code -g internal/tracker/workflows.go:198`)

```go
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
```

**What you need to understand:**

- One `AwaitWithTimeout` does two jobs. Its timeout is the completion time, seven days after submissions close ([`model.go` line 95](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/model.go#L95)). Its condition wakes the Workflow early to Continue-As-New after 500 writes, or when Temporal suggests it because the history is getting large.
- There is no timer for closing submissions. The `submit` handler compares the time with the close time when each submission arrives, and the API works out **open**, **ended**, or **archived** when it reads the event ([`model.go` lines 47–56](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/model.go#L47-L56)).
- Before continuing as new or completing, the Workflow sets a flag that makes the Update validators reject new requests, then waits for `AllHandlersFinished`, so no submission is cut off halfway.
- Completing doesn't delete anything. Queries and exports keep working until the Namespace's retention period removes the closed Workflows.
- `GetVersion` keeps event histories recorded before a code change replaying correctly. The hosted Worker is unversioned, so this repo patches changes to Workflow code; the README explains why.

**Docs:** [Continue-As-New](https://docs.temporal.io/workflow-execution/continue-as-new), [Go: Continue-As-New](https://docs.temporal.io/develop/go/workflows/continue-as-new), [Go: Patching with GetVersion](https://docs.temporal.io/develop/go/workflows/versioning#patching-with-getversion)

### Export a consistent snapshot without a database

**When to use it:** after beat 5, or when someone asks how the export avoids missing or duplicating a row during a burst.

**Where it is:** [`internal/web/leads.go` lines 194–200](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/web/leads.go#L194-L200) (`code -g internal/web/leads.go:194`)

```go
confirmed, err := s.visibleEvent(ctx, id)
if err != nil {
	return nil, err
}
if confirmed.Busy || confirmed.Revision != event.Revision || confirmed.Count != event.Count || count != event.Count {
	return nil, errChanged
}
```

**What you need to understand:**

- The export queries the event, pages through each shard with the `page` Query, then queries the event again. If a submission landed in between, the revision has changed, and the export tries again, up to three times ([lines 120–125](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/web/leads.go#L120-L125)).
- After three changed attempts it fails with "participants changed; reload the list or retry the export" rather than producing a partial file.
- The **Participants** table uses the same check page by page.

### What's demo-only

When someone asks what they'd change for their own system, these are the parts to leave out:

- **Game flight codes** belong to a separate game demo and are turned off on the attendee form for now. The Nexus `submit_participant` operation still starts a `FlightPassWorkflow` (`flight-pass/<event-id>/<code>`) that lives 30 minutes so the game can claim the attendee's name once ([`internal/tracker/flight.go` lines 51–93](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/flight.go#L51-L93)). The game reaches this app through the Nexus service in [`internal/tracker/nexus.go`](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/internal/tracker/nexus.go#L34-L44). Lead capture doesn't depend on either.
- **Employee login is the tmprl-demo.cloud catalog's SSO.** Locally, `DEV_AUTH_EMAIL` stands in for it and only works on a loopback address ([`cmd/tracker/main.go` lines 79–85](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/cmd/tracker/main.go#L79-L85)). A customer would use their own identity provider.
- **Optional Worker Versioning.** Setting `TEMPORAL_DEPLOYMENT_NAME` and `TEMPORAL_WORKER_BUILD_ID` makes the Worker pinned ([`cmd/tracker/main.go` lines 54–61](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/48b15701e0d6e8058d2e1f19b2ef64f9b0ea0848/cmd/tracker/main.go#L54-L61)). The hosted demo leaves it off.

## Reset and tear down

- **Between runs:** on the event page, click **Delete event**, then **Delete event** in the dialog. The event disappears from **Your events**, and its form and QR code stop opening. A running event Workflow records the deletion and keeps running until its completion time, seven days after its last day, so on tmprl-demo.cloud give test events today's date as the last day.
- **To start locally from nothing:** stop `make preview`, delete `.local/preview/`, and start it again. That removes the dev server's data, including every event.
- **To stop locally:** press `Ctrl+C` in the `make preview` terminal. It stops the app and the dev server it started; the data stays in `.local/preview/`.
- **What keeps running:** each event Workflow and its shards wait until seven days after the last day, then complete on their own. While they wait, nothing runs and nothing needs stopping.

## When something goes wrong

| What you see | Likely cause | Fix |
|---|---|---|
| `make preview` says `Port 8080 is already in use` | Another app holds the port | Set `PREVIEW_PORT`, for example `PREVIEW_PORT=8090 make preview`, and open that port |
| **Sign in to manage events.** on `/admin` | No employee session: locally you're not using `make preview` or `make dev`; on tmprl-demo.cloud you haven't signed in | Use `make preview` locally; on the hosted demo click **Employee sign in** and use your `temporal.io` account |
| A phone can't open the QR code locally | The QR code points at `http://localhost:8080` | Use **Open attendee form ↗** or **Copy form link** in a browser on the same machine |
| "end date has already passed in UTC" when creating | The last day is before today in UTC | Pick today's UTC date or later |
| "Temporal is temporarily unavailable. Please retry." on the form or control panel | The Worker isn't running, so the event Workflow can't answer Queries or Updates | Start the Worker (it's part of `make preview` and `make dev`); the attendee can submit again with the same details |
| A new event isn't in **Your events** yet | Visibility is eventually consistent | Wait a moment and reload, or use the event's direct link |
| "Event is refreshing. Please retry shortly." | The event Workflow is about to Continue-As-New | Submit again |
| Export fails with "participants changed; reload the list or retry the export" | Submissions kept arriving during three export attempts | Click **Export CSV ↓** again |

## Keeping this runbook current

The code links point at a fixed commit. When a linked file changes, update its link, line numbers, and snippet together. Before an event, list the linked files that have changed:

```sh
grep -oE 'blob/[0-9a-f]{40}/[^#)]+' RUNBOOK.md | sort -u | while IFS=/ read -r _ sha file; do
  git diff --quiet "$sha" HEAD -- "$file" || echo "changed since ${sha:0:7}: $file"
done
```
