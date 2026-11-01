# Runbook: Event Lead Tracker

This runbook is for presenting the demo. The [README](README.md) explains what it is and how it's built.

## What it shows

A Temporal employee creates an event, puts its QR code on a booth screen, and attendees scan it and leave their details. The employee watches responses arrive, ends the event, and exports a CSV. Each event is a Workflow that owns it: every change goes through the Workflow, which writes it to a DynamoDB table before saying it's saved. The pages read from that table.

The demo makes three problems visible:

- **A database that many requests write to needs locking, retries, and a way to stop two writers clobbering each other.** Here one Workflow per event is the only writer, so changes happen one at a time, and the table outlives Temporal itself: if the Namespace is lost, the Worker restarts every open event from the table. Beats 1, 2 and 7 show it.
- **Phones at a booth retry, resubmit, and submit twice.** Each submission is an Update that the event handles one at a time, deduplicated by email and by request ID, so a retry never creates a second row or overwrites newer details. Beat 3 shows it.
- **An event runs on a schedule.** Submissions close at midnight UTC after the last day, responses stay exportable, and the Workflows finish seven days later, on a durable timer rather than a cron job. Beats 5 and 6 show it.

## Before you start

- **On tmprl-demo.cloud:** open https://events.tmprl-demo.cloud/admin and sign in with your `temporal.io` account through the catalog. The attendee form and QR image are public, except that an **Internal only** event asks attendees to sign in the same way; everything under `/admin` needs that login.
  - The hosted demo holds real event leads from real events. Create a test event for the walkthrough, use a test email, and delete the event afterwards (see [Reset and tear down](#reset-and-tear-down)). Anyone with access to the project's Temporal Cloud Namespace can read submissions in Workflow histories.
  - To show the Workflows, you need access to the project's Temporal Cloud Namespace. The catalog creates it; this repo doesn't name it.
- **To run it locally:** install Go 1.26 or later, the [Temporal CLI](https://docs.temporal.io/cli), and Docker (for DynamoDB Local), and clone the repo. No accounts or keys are needed.

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
5. Say: "That event is now a running Workflow, and it's the only thing that can change it. It stored the event in DynamoDB before this page showed it, and it holds a timer for when the event finishes."
6. For proof, open the Temporal UI and find `event/<id>` (the ID is in the page URL), type `EventWorkflow`, **Running**. Its history starts with the `create` Update and the `CreateEvent` Activity, and has a timer for seven days after the close date.

### 2. An attendee submits the form

1. Click **Display QR code ↗** to show the full-screen QR display, then either scan it with a phone (hosted) or click **Open attendee form ↗** on the event page (local).
2. Fill in the form with a test email and tap **Request a follow-up →**. The button shows "Saving your details…", then **You're all set.** with the event's confirmation message.
3. Back on the event page, click **Refresh** next to **Participants**. The row appears and **Responses** reads 1.
4. Say: "The form sent an Update to the event's Workflow. The Workflow checked whether this person or this exact request had been seen before, wrote the response to the table, and only then told the phone it was saved."
5. For proof, open `event/<id>` in the Temporal UI. The history shows a `submit` Update (`WorkflowExecutionUpdateAccepted`, `WorkflowExecutionUpdateCompleted`) and the `Lookup` and `SaveSubmission` Activities.

### 3. The same person submits again

1. In the attendee form, reload the page, enter the same email in different capitals (for example `Ada@Example.com` after `ada@example.com`) and a different **Title / role**, and submit.
2. Click **Refresh** on the event page. **Responses** still reads 1, and the row shows the new title. Hover the **Last submitted · UTC** cell to see the first submission time.
3. Say: "One person, one row. Emails are normalized and the newest details win. And if a phone on bad Wi-Fi resends the same request, the Workflow recognizes it and doesn't apply it twice."
4. For proof, the event history shows a second `submit` Update and a second `SaveSubmission`, which replaced the same email's row.

### 4. Change the QR banner while the event runs

1. Under **QR DISPLAY**, change **Banner message** and click **Save message**. The page says "QR display message saved."
2. Reload the QR display tab. The new banner shows.
3. Say: "Changing a setting is one more Update to the same Workflow, which stores it. Nothing else writes to the event, so nothing can overwrite it."

Skip this beat when time is short.

Optionally, change the event too:

1. Under **EVENT DETAILS**, change the name, the welcome message, or the last day, and click **Save details**. A changed date shows "Details saved. Submissions now close …".
2. Under **EVENT LINK**, type a new link and click **Save link**. The page says "Link saved: …" and lists the earlier link as still working.
3. Open the old link and the new one. Both show the attendee form with the new name.
4. Say: "The old link is still reserved in the table and points at this event, so printed QR codes keep working, and no other event can take it."
5. For proof, the event's history shows `details` and `slug` Updates, and a `SaveSlug` Activity that reserved the new link.
6. Under **LINK PREVIEW**, set a title and description and click **Save preview**. The page says "Link preview saved." Paste the event link into Slack to see it unfurl. Slack caches previews, so paste a link it hasn't seen yet, such as a newly changed one. To check without Slack, run `curl -s https://events.tmprl-demo.cloud/<link> | grep og:`.

### 5. Export the responses

1. Click **Export CSV ↓**. The page says "Your CSV export is ready." and the browser downloads `<event-name>-leads.csv`.
2. Open it: one row per participant, a column per question with the question ID in the header (`Email [email]`), plus first and last submission times in UTC.
3. Say: "The export reads every participant from the table, and checks the event didn't change halfway through. If it did, it retries rather than handing you a half-written file."

### 6. End the event

1. Click **End event**. In **End this event?**, click **End event**.
2. The page says "Event ended. New responses are now closed." The badge changes to **ended** and the **End event** button disappears. **Export CSV ↓** still works.
3. Reload the attendee form. It shows **Submissions have closed**.
4. Say: "Ending stops new responses straight away. Without this click, the event closes itself at midnight UTC after its last day. Either way the Workflow keeps running for seven more days so you can still edit the event, and then it completes on its own. The responses stay exportable after that, because they're in the table."
5. For proof, `event/<id>` is still **Running**. Its history shows an `end` Update, and the seven-day timer from beat 1 is still pending.

### 7. Lose Temporal, and the events come back (local)

1. Press `Ctrl+C` in the `make preview` terminal. Delete `.local/preview/temporal.db`, the dev server's data, and keep `.local/preview/dynamodb`. Run `make preview` again.
2. Open http://localhost:8080/admin. The events, their responses, and their links are all there.
3. In the Temporal UI, `event-reconciler` is running, and every open event has a new `event/<id>` run that started with a `ClaimEvent` Activity. Submit the form again: it works.
4. Say: "I just threw away the whole Temporal server, as if the Namespace were deleted. The table kept every event, and when the Worker started, it restarted a Workflow for each open one from the table."

On tmprl-demo.cloud you can't reset the Namespace yourself; skip this beat or describe it.

## Show it in code

All links point at commit [`c350cd2`](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/commit/c350cd24a09ba1bab7dd38db03b3740ab4946fda). Open a file locally with `code -g <path>:<line>`.

### One Workflow per event, as its only writer

**When to use it:** after beat 1, or when someone asks where the data is stored.

**Where it is:** [`internal/web/gateway.go` lines 52–75](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/web/gateway.go#L52-L75) (`code -g internal/web/gateway.go:52`) and [`internal/tracker/workflows.go` lines 428–443](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/workflows.go#L428-L443) (`code -g internal/tracker/workflows.go:428`)

```go
start := g.client.NewWithStartWorkflowOperation(client.StartWorkflowOptions{
	ID: tracker.EventID(event.ID), TaskQueue: g.taskQueue,
	WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
	WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
}, tracker.EventType, tracker.EventInput{ID: event.ID})
handle, err := g.client.UpdateWithStartWorkflow(ctx, client.UpdateWithStartWorkflowOptions{
	StartWorkflowOperation: start,
	UpdateOptions: client.UpdateWorkflowOptions{
		UpdateName: "create", UpdateID: "create-" + hex.EncodeToString(token[:]), Args: []any{event},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	},
})
```

```go
if input.Hydrate {
	err := workflow.ExecuteActivity(activityCtx, "ClaimEvent", input.ID).Get(ctx, &state)
	// ...
}
```

**What you need to understand:**

- This is the entity Workflow pattern: one Workflow per event, with the Workflow ID `event/<id>`. Every change to the event is an Update to it, and nothing else writes to the event's items in DynamoDB. The web component's IAM role can only read the table.
- Creating an event is one call that starts the Workflow and sends it the `create` Update. The Update runs the `CreateEvent` Activity, which stores the event and reserves its link in a single DynamoDB transaction. A taken link fails the Update, and the API tries the next suffix (`/kubecon-2026-2`).
- Every other run starts with `Hydrate`: the `ClaimEvent` Activity loads the event from the table. That's how Continue-As-New, deleting a completed event, and recovery after a lost Namespace all get their state.
- The pages read the table directly ([`internal/store/read.go`](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/store/read.go)): the directory is a DynamoDB index, not a Visibility search, so a new event shows up at once.
- The trade-off is a table to run. It lives outside the catalog ([`deploy/create-store.sh`](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/deploy/create-store.sh)), so deleting or recreating the demo leaves it alone.

**Docs:** [Entity Workflow pattern](https://docs.temporal.io/design-patterns/entity-workflow), [Update-with-Start](https://docs.temporal.io/develop/go/workflows/message-passing#update-with-start)

### Accept each submission as an Update, one at a time

**When to use it:** after beat 2 or 3, or when someone asks what happens when two hundred people scan at once.

**Where it is:** [`internal/tracker/workflows.go` lines 154–193](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/workflows.go#L154-L193) (`code -g internal/tracker/workflows.go:154`)

```go
digest := input.Digest()
var existing Existing
if err := workflow.ExecuteActivity(activityCtx, "Lookup", LookupInput{EventID: state.Event.ID, Email: input.Email, RequestID: input.RequestID}).Get(ctx, &existing); err != nil {
	return "", err
}
// A retried request is acknowledged without writing again, even after the event closes.
if existing.Receipt != nil {
	if existing.Receipt.Digest != digest {
		return "", temporal.NewApplicationError("request ID was already used for different details", "Invalid")
	}
	return "saved", nil
}
// ...
if err := persist(ctx, "SaveSubmission", SubmissionWrite{State: write, Lead: lead, Receipt: Receipt{ID: input.RequestID, Digest: digest}}); err != nil {
	return "", err
}
```

**What you need to understand:**

- A submission is an Update, so the phone waits until the Workflow has stored the participant and returns "saved". A Signal couldn't tell the phone whether it worked.
- The handler runs Activities, so several submissions could interleave while one waits. `workflow.NewMutex` makes them take turns. That's what keeps the count and the email deduplication correct under a burst, without a lock in the database.
- The limit a burst reaches first is Temporal's: one Workflow can have at most 10 Updates in flight. Beyond that the server answers `ResourceExhausted`, and the attendee sees "Temporal is temporarily unavailable. Please retry." and can submit again.
- The form creates one request ID per attempt and reuses it when you resubmit the same details after an error ([`internal/web/static/app.js` lines 184–185](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/web/static/app.js#L184-L185)). The Update ID includes it, and the table keeps a receipt for every request ID, so a retry is acknowledged without a second write, whenever it arrives.

**Docs:** [Go: Update handlers and validators](https://docs.temporal.io/develop/go/workflows/message-passing#updates), [Exactly-once message processing](https://docs.temporal.io/handling-messages#exactly-once-message-processing)

### Store each change before acknowledging it, and fence out older runs

**When to use it:** after the previous entry, when someone asks what happens if a write times out and the Activity retries, or if two Workflow runs write at once.

**Where it is:** [`internal/tracker/workflows.go` lines 91–110](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/workflows.go#L91-L110) (`code -g internal/tracker/workflows.go:91`) and [`internal/store/write.go` lines 15–20](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/store/write.go#L15-L20) (`code -g internal/store/write.go:15`)

```go
// persist runs a store write. If another run has taken the event over, this
// run stops writing and ends once its handlers finish.
persist := func(ctx workflow.Context, activity string, input any) error {
	err := workflow.ExecuteActivity(activityCtx, activity, input).Get(ctx, nil)
	var app *temporal.ApplicationError
	if errors.As(err, &app) && app.Type() == "Fenced" {
		abandoned = true
	}
	return err
}
```

```go
const fenced = "#epoch = :epoch AND #revision <= :revision"
```

**What you need to understand:**

- Every change runs as an Activity that writes to DynamoDB, and the handler only changes its own copy once the write succeeds. The Activity retries with no limit on attempts, backing off to at most 10 seconds between them.
- Each write is conditioned on the run's epoch and the event's revision. Within a run, revisions only grow, and a retry repeats the same revision, so a retry is accepted and an older write is refused.
- Each run that takes an event over (`ClaimEvent`) increments the epoch. An Activity left over from an earlier run, such as one still in flight when that run was terminated, can no longer write. If a run finds its own writes refused, it stops.
- A submission writes the participant, its receipt, and the event's new count in one DynamoDB transaction ([`write.go` lines 152–186](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/store/write.go#L152-L186)).

### Close, complete, and refresh the event on its own timer

**When to use it:** after beat 6, or when someone asks what runs the schedule.

**Where it is:** [`internal/tracker/workflows.go` lines 445–482](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/workflows.go#L445-L482) (`code -g internal/tracker/workflows.go:445`)

```go
for ready && !abandoned {
	remaining := state.Event.CompletesAt.Sub(workflow.Now(ctx))
	if remaining <= 0 {
		break
	}
	rescheduled = false
	woke, err := workflow.AwaitWithTimeout(ctx, remaining, func() bool {
		return rescheduled || abandoned || workflow.GetInfo(ctx).GetContinueAsNewSuggested()
	})
	// ...
	if workflow.GetInfo(ctx).GetContinueAsNewSuggested() && !abandoned {
		// ...
		return Event{}, workflow.NewContinueAsNewError(ctx, EventWorkflow, EventInput{ID: state.Event.ID, Hydrate: true})
	}
}
```

**What you need to understand:**

- One `AwaitWithTimeout` does three jobs. Its timeout is the completion time, seven days after submissions close ([`model.go` line 203](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/model.go#L203)). A changed end date wakes it to start a new timer, and so does Temporal suggesting Continue-As-New.
- There is no timer for closing submissions. The `submit` handler compares the time with the close time when each submission arrives, and the API works out **open**, **ended**, or **archived** when it reads the event ([`model.go` line 108](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/model.go#L108)).
- Before continuing as new or completing, the Workflow sets a flag that makes the Update validators reject new requests, then waits for `AllHandlersFinished`, so no submission is cut off halfway. The next run loads the event from the table, so nothing has to be carried across.
- Completing doesn't delete anything. The event and its responses stay in the table; completion only takes it off the list of open events.

**Docs:** [Continue-As-New](https://docs.temporal.io/workflow-execution/continue-as-new), [Go: Continue-As-New](https://docs.temporal.io/develop/go/workflows/continue-as-new)

### Bring every open event back after losing the Namespace

**When to use it:** after beat 7, or when someone asks what happens if Temporal loses its data.

**Where it is:** [`internal/tracker/activities.go` lines 77–103](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/activities.go#L77-L103) (`code -g internal/tracker/activities.go:77`) and [`internal/tracker/reconcile.go` lines 20–35](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/reconcile.go#L20-L35)

```go
if _, err := a.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
	ID: EventID(event.ID), TaskQueue: a.TaskQueue,
	// A running run is the one we want. A run that was terminated (the
	// storage cut-over) or failed is replaced; one that completed is not.
	WorkflowIDConflictPolicy: enums.WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING,
	WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
}, EventType, EventInput{ID: event.ID, Hydrate: true}); err != nil {
```

**What you need to understand:**

- `ReconcileWorkflow` runs every five minutes. It reads the table's index of open events (not deleted, not completed) and starts a Workflow for each one that doesn't have a running one. Each Worker makes sure it's running when it starts ([`cmd/tracker/main.go` lines 93–99](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/cmd/tracker/main.go#L93-L99)).
- After a Namespace reset, nothing is running and nothing is in Temporal, so the first pass restarts every open event. While it does, reads keep working, and a change sent to a missing Workflow gets "Event is starting. Please retry shortly."
- The ID policies make a pass harmless to repeat: it never starts a second run beside a running one, and never revives an event that completed on its own.

### Export a consistent snapshot

**When to use it:** after beat 5, or when someone asks how the export avoids missing or duplicating a row during a burst.

**Where it is:** [`internal/web/leads.go` lines 55–64](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/web/leads.go#L55-L64) (`code -g internal/web/leads.go:55`)

```go
func (s *server) unchanged(ctx context.Context, event tracker.Event) error {
	confirmed, err := s.visibleEvent(ctx, event.ID)
	if err != nil {
		return err
	}
	if confirmed.Revision != event.Revision || confirmed.Count != event.Count {
		return errChanged
	}
	return nil
}
```

**What you need to understand:**

- The export reads the event, pages through its participants in the table in first-submitted order, then reads the event again. Every change raises the revision, so if a submission landed in between, the export tries again, up to three times ([`leads.go` lines 104–109](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/web/leads.go#L104-L109)).
- After three changed attempts it fails with "participants changed; reload the list or retry the export" rather than producing a partial file.
- The **Participants** table uses the same check page by page.

### What's demo-only

When someone asks what they'd change for their own system, these are the parts to leave out:

- **Game flight codes** belong to a separate game demo and are turned off on the attendee form for now. The Nexus `submit_participant` operation still starts a `FlightPassWorkflow` (`flight-pass/<event-id>/<code>`) that lives 30 minutes so the game can claim the attendee's name once ([`internal/tracker/flight.go` lines 51–93](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/b7fe94dd5e76861921976a94de52b00223bf2b09/internal/tracker/flight.go#L51-L93)). The game reaches this app through the Nexus service in [`internal/tracker/nexus.go`](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/internal/tracker/nexus.go#L33-L43). Lead capture doesn't depend on either.
- **Employee login is the tmprl-demo.cloud catalog's SSO.** Locally, `DEV_AUTH_EMAIL` stands in for it and only works on a loopback address ([`cmd/tracker/main.go` lines 107–113](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/cmd/tracker/main.go#L107-L113)). A customer would use their own identity provider.
- **Optional Worker Versioning.** Setting `TEMPORAL_DEPLOYMENT_NAME` and `TEMPORAL_WORKER_BUILD_ID` makes the Worker pinned ([`cmd/tracker/main.go` lines 74–80](https://github.com/temporal-sa/temporal-lead-follow-up-event-tracker/blob/c350cd24a09ba1bab7dd38db03b3740ab4946fda/cmd/tracker/main.go#L74-L80)). The hosted demo leaves it off.

## Reset and tear down

- **Between runs:** on the event page, click **Delete event**, then **Delete event** in the dialog. The event disappears from **Your events**, and its form and QR code stop opening. A running event Workflow records the deletion and keeps running until its completion time, seven days after its last day, so on tmprl-demo.cloud give test events today's date as the last day.
- **To start locally from nothing:** stop `make preview`, delete `.local/preview/`, and start it again. That removes the dev server's data and the local table, including every event.
- **To stop locally:** press `Ctrl+C` in the `make preview` terminal. It stops the app and the dev server it started; the data stays in `.local/preview/`.
- **What keeps running:** each event Workflow waits until seven days after the last day, then completes on its own. While it waits, nothing runs and nothing needs stopping. The events and responses stay in DynamoDB after that; locally, delete `.local/preview/dynamodb` to remove them.

## When something goes wrong

| What you see | Likely cause | Fix |
|---|---|---|
| `make preview` says `Port 8080 is already in use` | Another app holds the port | Set `PREVIEW_PORT`, for example `PREVIEW_PORT=8090 make preview`, and open that port |
| **Sign in to manage events.** on `/admin` | No employee session: locally you're not using `make preview` or `make dev`; on tmprl-demo.cloud you haven't signed in | Use `make preview` locally; on the hosted demo click **Employee sign in** and use your `temporal.io` account |
| **Sign in to respond.** on an attendee form | The event is **Internal only** (set at creation or under **Edit questions**), and the visitor has no `temporal.io` session | Click **Sign in with Temporal**; the catalog returns to the form. Locally, `make preview` is always signed in, and its identity has no name, so the Name question stays editable |
| **You've already responded** on an internal-only form | That employee's response is saved, and internal-only events take one per person | Nothing to fix. To show the form again locally, delete the test event and create another |
| A phone can't open the QR code locally | The QR code points at `http://localhost:8080` | Use **Open attendee form ↗** or **Copy form link** in a browser on the same machine |
| "end date has already passed in UTC" when creating | The last day is before today in UTC | Pick today's UTC date or later |
| "Temporal is temporarily unavailable. Please retry." on the form or control panel | The Worker isn't running, so the event Workflow can't handle Updates | Start the Worker (it's part of `make preview` and `make dev`); the attendee can submit again with the same details |
| "Event is starting. Please retry shortly." | The event is in the table but its Workflow isn't running, for example just after a Namespace reset | Wait for the reconciler's next pass (at most five minutes after the Worker starts), then retry |
| "Event is refreshing. Please retry shortly." | The event Workflow is about to Continue-As-New | Submit again |
| "Changes are paused for maintenance. Please retry in a few minutes." | `WRITES_PAUSED` is on for the storage cut-over ([docs/storage-cutover.md](docs/storage-cutover.md)) | Wait for the cut-over to finish; reading and exporting still work |
| "submissions have closed, so the end date can no longer change" | The event ended, by date or with **End event** | Click **Duplicate event** and pick a new last day |
| "that event link is already in use" | Another event has that link, now or earlier (links are never released) | Pick a different link |
| "an event can have at most 10 additional links" | The event has used all 10 extra links | Switch back to one of the links listed under **EVENT LINK** |
| Export fails with "participants changed; reload the list or retry the export" | Submissions kept arriving during three export attempts | Click **Export CSV ↓** again |

## Keeping this runbook current

The code links point at a fixed commit. When a linked file changes, update its link, line numbers, and snippet together. Before an event, list the linked files that have changed:

```sh
grep -oE 'blob/[0-9a-f]{40}/[^#)]+' RUNBOOK.md | sort -u | while IFS=/ read -r _ sha file; do
  git diff --quiet "$sha" HEAD -- "$file" || echo "changed since ${sha:0:7}: $file"
done
```
