# Event Lead Tracker

A small mobile-friendly event follow-up form. Temporal employees manage events,
display QR codes, view participants, and export CSV. Each event is a Temporal
Workflow that owns it and is the only writer to its permanent record in DynamoDB.
The web server and the Nexus service read that record; they never write it. It
outlives the Temporal namespace: after a reset, the worker restarts every open
event from it. There is no server session store.

## Run locally

Requirements: Go 1.26 or newer, the Temporal CLI, and Docker (for DynamoDB Local).

For a complete local preview, including a persistent local Temporal server,
worker, and employee control panel:

```sh
make preview
```

Open http://localhost:8080/admin. The preview uses its own Temporal server on
port 7234 (UI on 8234) and DynamoDB Local in Docker on port 8235, ignores
inherited Cloud credentials, and stores both under `.local/preview/`. Stop it
with Ctrl+C. To change ports, set `PREVIEW_PORT`, `PREVIEW_TEMPORAL_PORT`,
`PREVIEW_TEMPORAL_UI_PORT`, and `PREVIEW_DYNAMODB_PORT`.

To present the demo, follow [RUNBOOK.md](RUNBOOK.md): the walkthrough beat by beat, and where in the code each part happens.

To run against an existing local Temporal server instead:

In one terminal, start a persistent local Temporal server:

```sh
mkdir -p .local
temporal server start-dev --db-filename .local/temporal.db
```

In another terminal:

```sh
docker run --rm -p 8000:8000 amazon/dynamodb-local:2.5.4 -jar DynamoDBLocal.jar -sharedDb -inMemory
make dev
```

Open http://localhost:8080/admin. `make dev` runs the web server and worker together
and enables a local employee identity. `DEV_AUTH_EMAIL` is permitted only with a
loopback listener and public URL. Unset it to test anonymous visitors. Real
catalog SSO requires a deployed hostname under `.tmprl-demo.cloud`; its session
cookie is not sent to localhost.

```sh
make test
make vet
make build
```

The store tests need DynamoDB Local. The integration test runs the app through
its HTTP API against a real Temporal server and DynamoDB Local, including a
simulated namespace reset:

```sh
docker run --rm -p 8765:8000 amazon/dynamodb-local:2.5.4 -jar DynamoDBLocal.jar -inMemory -sharedDb
DYNAMODB_INTEGRATION_ENDPOINT=http://localhost:8765 go test ./internal/store
TEMPORAL_INTEGRATION_ADDRESS=localhost:7233 DYNAMODB_INTEGRATION_ENDPOINT=http://localhost:8765 go test ./internal/web -run TestIntegration -v
```

The binary supports `serve`, `worker`, and `dev`. `serve` is the default. In
deployment, run `serve` and `worker` as separate components from the same image.

## Event behavior

Create an event with a name, optional description, and required end date in UTC.
The event's link is its normalized name: "KubeCon 2026" opens at `/kubecon-2026`.
If another event, or one of its links, already uses that slug, the next free
suffix is used (`/kubecon-2026-2`). Slugs are at most 39 characters so the QR
code keeps its capacity; names without ASCII letters or digits, or that match
an app path such as `admin`, get a suffix or a random ID. Events created before
slugs keep their random IDs, and `/events/{id}` links keep working.
An end date of October 7 closes submissions at October 8, 00:00:00 UTC. The event
workflow completes seven days later, October 15, 00:00:00 UTC.
Ending manually closes submissions immediately without shortening that scheduled
completion deadline. Events cannot reopen. New workflow runs use a single timer
for completion; submission updates enforce the cutoff when they arrive, and the
API derives the displayed status from the current time.

Edit the name, subheading, welcome message, and end date under **Event details** on the event
page. The subheading is the small line above the event name on the attendee form;
it starts as "Let's stay in touch", and clearing it shows none. Events created
before it was configurable keep showing the default until someone changes it.
The name, subheading, and welcome message can change until the event completes. The end
date can only change while submissions are open; moving it moves the close time
and the seven-day completion with it, earlier or later. Once an event has ended,
by date or with **End event**, its end date is fixed. To run it again, use
**Duplicate event**, which opens the create form with the name, subheading, welcome message,
questions, QR banner, and link preview filled in and the end date left blank.

Change an event's link under **Event link**. The new slug follows the same rules as
creation. Every earlier link, including the original, keeps opening the same event,
so printed QR codes and open browser tabs keep working; new QR codes use the
current link. An event can hold up to 10 links besides its original. Each link is
reserved in the store, so no other event can take it, and it's never released,
even after the event is deleted. Renaming an event does not change its link.

New events start with a form template containing name, title / role, email, and
an optional follow-up reason. Customize the questions when creating an event:
edit labels and help text, add or remove questions, reorder them, and mark them
required or optional. A required email question stays in the first section so
every response has a deduplication key. Edit the questions later with **Edit
questions** on the event page, until the event completes. The form also holds
the final button's text (default "Request a follow-up", or something like
"Submit survey") and the confirmation message attendees see after they submit;
without one, they see a default thank-you.

To run the same event again, click **Export form** on the event page. It
downloads a JSON file with the questions, sections, button text, confirmation
message, subheading, welcome message, QR banner, and link preview. In a new event, click **Import form**
to load that file, then choose the end date. The server validates the imported
form like any other.

Question types include short text, paragraphs, email, phone, URL, number, multiple
choice, dropdown, checkboxes, linear scale, rating, date, time, and single-choice
or checkbox grids. A multiple-choice question can offer an "Other" option with a
free-text answer, which is stored and exported as the text the participant typed. Organize questions into sections. Multiple-choice and dropdown
answers can route to a later section or submit immediately; sections also have
a default next step. Required questions in skipped sections do not block submission.
The server validates the selected path and discards answers from skipped sections.
Editing a form applies to new responses immediately. Responses already collected
keep their stored answers, but the participant table and CSV export follow the
current form, so answers to a removed question no longer appear there. A form
opened before an edit can still be submitted: answers to removed questions are
dropped, and newly required questions must be answered.

Check **Internal only** when creating an event for a Temporal-only audience, or
turn it on later under **Edit questions** on the event page. It can't be turned
off again. **Duplicate event**, export, and import carry it. Its form then
requires the same employee sign-in as the control panel: a visitor without one
sees **Sign in to respond.** and a catalog login that returns to the event's
current link. Name and Email become fixed questions, marked **Auto-filled**: they
stay required in the first section, and their wording, type, and required setting
can't be edited, nor can they be deleted. Turning the flag on puts a required
short-answer Name question back at the top if the form had none, or had one of
another type. The server rejects an internal-only form without those questions,
both when the flag is set and on every later edit. Attendees see both answers
filled from the catalog's verified identity, read-only, and the server replaces
whatever was submitted for them, so a response is always recorded under the
signed-in employee. When the catalog has no name for the employee, they type it.
An internal-only event takes one response per employee: one who has already
responded sees **You've already responded** with the time of their response
instead of the form, and the event's Workflow rejects a second response, so a
form left open in another tab can't replace the first. That includes a response
collected under the same email before the event became internal-only; those keep
their details. The Nexus `submit_participant` operation rejects internal-only
events, since it carries no sign-in. The link preview and QR image stay public.

Set a QR-page banner during event creation or update it from the event control
panel while the event workflow is active. The fullscreen display includes Temporal
branding, the event name, banner, and a QR code linking to the public form. It
adapts to a phone, desktop, or presentation screen.

Set how an event's link unfurls in Slack and other apps under **Link preview**,
during creation or on the event page until the event completes. The server
renders the title and description as Open Graph and Twitter tags into the page at
`/{slug}` and `/events/{id}`, with `og:url` set to the event's current link. A
blank title uses the event name; a blank description uses the welcome message, or
a generic line when there is none. Every event uses the same Temporal image,
`static/link-preview.png`. Other pages, and links to missing or deleted
events, keep the generic tags. Apps cache previews, so a link already posted can
keep showing its old preview for a while. Export, import, and **Duplicate event**
carry the preview.

Emails are trimmed and lowercased; the newest submission replaces the
existing participant's details within that event. The same person may join
different events. Employees can view and export at any time: during the event,
after submissions close, and after the workflow completes, because responses are
kept in the store. The participant table and the export list participants in the
order they first responded. Exports include all configured questions in form
order, with stable question IDs in column headers; grids have a column per row.
Checkbox answers use JSON arrays inside CSV cells to preserve individual
selections.

**Delete event** hides the event, its form, and its QR code, at any stage. Its
responses stay in the store, so a deletion can be undone by hand, and its links
stay reserved. A deleted event that hasn't completed keeps its workflow until its
completion time, rejecting every change.

No automatic emails or contacted/pending status are included.

## Workflow design

`EventWorkflow` owns one event: its details, its form, its timer, and the order
of every change to it. The store (`internal/store`, one DynamoDB table) holds the
event, its participants, a receipt for every accepted submission, and its links.
The workflow is the store's only writer, through Activities, and stores each
change before acknowledging it. The web server reads the store and sends every
change as an Update; its IAM role can only read the table.

Creating an event is an Update-with-Start: the API starts `event/{slug}` and sends
it the `create` Update in one call. The Update's Activity stores the event and
reserves its slug as a link in one transaction; a taken slug fails it, and the API
tries the next suffix. Every other run starts by claiming the stored event: it
loads the event and increments its epoch. Every store write is conditioned on the
writer's epoch and the event's revision, so a retried write repeats harmlessly and
a write from an earlier run (left over after Continue-As-New, a terminate, or a
reset) is refused. A run whose write is refused stops.

Submissions are serialized by a workflow mutex. Each one looks up the person's
row and the request ID's receipt, then writes the participant, the receipt, and
the event's new count in one transaction. A retried request is acknowledged from
its receipt at any time, even after the event closes; a request ID reused with
different details is refused. An internal-only event takes one response per
employee.

Changing the end date wakes the completion wait, which starts a new timer.
`EventWorkflow` continues as new when Temporal suggests it; the next run claims
the event again, so nothing is carried across. At completion it saves once more
to take the event off the store's list of open events.

`ReconcileWorkflow` keeps a workflow running for every open event, that is, every
event that isn't deleted and hasn't completed. Each worker starts it when it
boots, and it checks every five minutes. After a namespace reset, its first pass
restarts every open event from the store; until then, reads work and changes
answer "Event is starting. Please retry shortly." It never starts a second run
beside a running one, and never restarts an event that completed.

Making an existing event internal-only is the `internalOnly` Update. It carries the
form along with the flag, so the event never requires sign-in without the Name and
Email questions the sign-in answers. The server's `POST /api/admin/events/{id}/form`
sends it instead of the `form` Update when the request sets `internalOnly` on a
public event.

Participant and event data are in the store and in workflow histories (Activity
inputs carry the details being written); anyone with access to either can read
them. Deploy this in an account and namespace appropriate for lead data.

## Configuration

| Variable | Default / purpose |
| --- | --- |
| `LISTEN_ADDRESS` | `127.0.0.1:8080`; Docker uses `0.0.0.0:8080` |
| `PUBLIC_URL` | `http://localhost:8080`; trusted canonical origin for QR and origin checks |
| `TEMPORAL_ADDRESS` | `localhost:7233` |
| `TEMPORAL_NAMESPACE` | `default` |
| `TEMPORAL_TASK_QUEUE` | `event-leads` |
| `TEMPORAL_API_KEY` | Optional Cloud API key; enables TLS |
| `TEMPORAL_TLS` | `true` enables TLS without an API key |
| `TEMPORAL_CLIENT_CERT`, `TEMPORAL_CLIENT_KEY` | Optional mTLS certificate/key file paths |
| `TEMPORAL_CA_CERT` | Optional custom CA certificate path |
| `AUTH_BASE_URL` | `https://catalog.tmprl-demo.cloud`; employee login origin |
| `AUTH_VERIFY_URL` | `https://catalog.tmprl-demo.cloud/_auth/verify`; trusted catalog verifier endpoint |
| `DEV_AUTH_EMAIL` | Explicit local-only employee identity |
| `TEMPORAL_DEPLOYMENT_NAME`, `TEMPORAL_WORKER_BUILD_ID` | Set together to enable pinned Worker Deployment Versioning |
| `EVENT_LEADS_TABLE` | Required: the DynamoDB table that permanently stores events |
| `DYNAMODB_ENDPOINT` | DynamoDB Local URL; when set, the app creates the table on start. Unset uses AWS |
| `AWS_REGION` | Region of the table; credentials come from the AWS default chain (IRSA in deployment) |
| `WRITES_PAUSED` | `false`; `true` rejects every change, for maintenance such as the storage cut-over ([docs/storage-cutover.md](docs/storage-cutover.md)) |

Employee authentication sends only the catalog's `temporal_demo_auth` cookie to
the configured catalog verifier, which validates the session. The app requires
its `204` response with a subject and an exact `temporal.io` email domain; the
catalog's bootstrap identity is not accepted. No signing key is shared with this
app. Identity headers supplied by visitors and participant-entered emails cannot
grant admin access. Verification runs on every protected request with a
three-second timeout and no redirect following. A verifier outage blocks admin
access with `503`; public forms and submissions do not call the verifier, except
on internal-only events, where an outage blocks the form the same way. The
employee's name comes from the verifier's optional `X-Temporal-Auth-Name`
header, decoded as Latin-1 when it is not UTF-8, as the catalog sends it. Admin
mutations, and submissions to internal-only events, require same-origin JSON requests. Public event URLs always show the
form, even if the visitor is an authenticated employee.

For a different catalog deployment, set both `AUTH_BASE_URL` and `AUTH_VERIFY_URL`
to its login origin and verifier endpoint.

`GET /healthz` checks the HTTP process. `GET /readyz` checks its Temporal connection and the table.
The worker logs to stdout and shuts down gracefully on SIGINT/SIGTERM.

## tmprl-demo.cloud deployment

[deploy/demo-project.yaml](deploy/demo-project.yaml) is a sample platform manifest.
It exposes the web component publicly (`temporalAuthRequired: false`) so visitors
can submit without employee login; the application protects admin pages and APIs.
The platform injects Temporal credentials. The demo uses an unversioned worker
on the `event-leads-store` task queue, which the Nexus endpoint also targets.
Catalog rollouts replace the project's Kubernetes namespace; the Temporal
namespace is per project and is removed when the catalog entry is finalized, its
region changes, or its auth mode changes. The event table lives outside the
catalog, so neither touches it: [deploy/create-store.sh](deploy/create-store.sh)
creates it and the components' IAM roles in the demo AWS account, and the
manifest names the roles in `awsIamRoleArn`. Run the script again when a platform
cluster is added or replaced, so the roles trust its OIDC provider.
The sample points `AUTH_VERIFY_URL` at the catalog's internal Kubernetes service.
Employee login still uses the public catalog URL. No project auth secret is needed.

The current DemoProject schema allows multiple path routes on one hostname, but
all routes inherit one `spec.ingress.temporalAuthRequired` value. It does not
support multiple ingress definitions or per-route auth overrides. This app uses
public ingress and verifies employee access inside its admin handlers.

Before applying the manifest:

1. Publish this source repository at its configured `temporal-sa` GitHub URL, or
   adjust the sample source settings.
2. Ensure the web component can reach the catalog service configured in
   `AUTH_VERIFY_URL` and that the catalog's employee SSO is configured.
3. Apply the sample through cluster-gitops-config's normal project flow, and verify
   the serving web and worker images and their readiness.

The app does not create OAuth clients or issue its own employee sessions. Shared
SSO requires the deployed hostname to be under `.tmprl-demo.cloud`. The worker is
unversioned, so new worker code replays the histories of running events. Keep
changes replay-compatible, and check them with the recorded-history replay tests.
The optional `TEMPORAL_REPLAY_HISTORY` test replays downloaded history files; list
several separated by `:`, for example
`TEMPORAL_REPLAY_HISTORY=$(ls ~/Downloads/*_events.json | paste -sd: -) go test ./internal/tracker -run TestReplay -v`.

This repository prepares deployment artifacts; it does not change cluster-gitops
or deploy infrastructure automatically.
