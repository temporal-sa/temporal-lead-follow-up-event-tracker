# Event Lead Tracker

A small mobile-friendly event follow-up form. Temporal employees manage events,
display QR codes, view participants, and export CSV. All durable application state
lives in Temporal: there is no database, disk-backed application store, or server
session store.

## Run locally

Requirements: Go 1.26 or newer and the Temporal CLI.

For a complete local preview, including a persistent local Temporal server,
worker, and employee control panel:

```sh
make preview
```

Open http://localhost:8080/admin. The preview uses its own Temporal server on
port 7234 (UI on 8234), ignores inherited Cloud credentials, and stores Temporal
data under `.local/preview/`. Stop it with Ctrl+C. To change ports, set
`PREVIEW_PORT`, `PREVIEW_TEMPORAL_PORT`, and `PREVIEW_TEMPORAL_UI_PORT`.

To run against an existing local Temporal server instead:

In one terminal, start a persistent local Temporal server:

```sh
mkdir -p .local
temporal server start-dev --db-filename .local/temporal.db
```

In another terminal:

```sh
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

The optional server-backed integration tests exercise worker restart, actual
Continue-As-New, retained queries, sharding, and history replay. Against an
isolated local dev server, run:

```sh
TEMPORAL_INTEGRATION_ADDRESS=localhost:7233 go test ./internal/tracker -run TestIntegration -v
```

The binary supports `serve`, `worker`, and `dev`. `serve` is the default. In
deployment, run `serve` and `worker` as separate components from the same image.

## Event behavior

Create an event with a name, optional description, and required end date in UTC.
An end date of October 7 closes submissions at October 8, 00:00:00 UTC. The event
and participant workflows complete seven days later, October 15, 00:00:00 UTC.
Ending manually closes submissions immediately without shortening that scheduled
completion deadline. Events cannot reopen. New workflow runs use a single timer
for completion; submission updates enforce the cutoff when they arrive, and the
API derives the displayed status from the current time.

New events start with a form template containing name, title / role, email, and
an optional follow-up reason. Customize the questions when creating an event:
edit labels and help text, add or remove questions, reorder them, and mark them
required or optional. A required email question stays in the first section so
every response has a deduplication key.

Question types include short text, paragraphs, email, phone, URL, number, multiple
choice, dropdown, checkboxes, linear scale, rating, date, time, and single-choice
or checkbox grids. Organize questions into sections. Multiple-choice and dropdown
answers can route to a later section or submit immediately; sections also have
a default next step. Required questions in skipped sections do not block submission.
The server validates the selected path and discards answers from skipped sections.
Form definitions stay fixed after event creation so captured responses and export
columns retain their meaning. Existing events keep their original form behavior.

Set a QR-page banner during event creation or update it from the event control
panel while the event workflow is active. The fullscreen display includes Temporal
branding, the event name, banner, and a QR code linking to the public form. It
adapts to a phone, desktop, or presentation screen.

Emails are trimmed and lowercased; the newest submission replaces the
existing participant's details within that event. The same person may join
different events. Employees can view and export during the event, after submissions
close, and after workflows complete, while their histories remain in Temporal
retention. The seven-day completion timer never disables viewing or export.
Exports include all configured questions in form order, with stable question IDs
in column headers; grids have a column per row. Checkbox answers use JSON arrays
inside CSV cells to preserve individual selections. Exports fail explicitly if
any participant shard is unavailable.

No automatic emails, contacted/pending status, or external CSV storage are
included. A future CSV-storage Activity can be added before final completion;
that change requires replay-compatible versioning for existing workflows.

## Workflow design

`EventWorkflow` owns metadata, UTC timers, counts, and shard references. It
serializes participant submissions and awaits confirmed shard writes before
acknowledging success. `ParticipantShardWorkflow` children hold participant
records, up to 5,000 unique emails or a 1 MiB serialized state budget. New shards
are created automatically; a growing repeat submission may relocate to a new
shard. Activity writes carry monotonically increasing sequence numbers so old
attempts cannot overwrite newer data. Reads and exports verify the coordinator's
revision and busy flag to avoid incomplete or duplicate data during relocation.
The coordinator carries receipts for the most recent 1,024 accepted submissions
across Continue-As-New, so a retried older HTTP request in that window cannot
replace newer details. Beyond that window a replayed request is treated as a new
submission; the email still deduplicates to one participant.

Both workflow types Continue-As-New after 500 write operations or when Temporal
recommends it. Participant children have `ABANDON` parent-close policy and stable
IDs; they remain alive when the coordinator continues. At final completion, the
coordinator signals each shard to finish after its handlers drain. Temporal
Visibility supplies the employee event directory, so newly created events may
take a moment to appear there; direct links work immediately. The directory
includes running and successfully completed events. Terminated, canceled, failed,
timed-out, and continued-as-new runs are excluded. Workflow queries time out
after five seconds, and a directory request is limited to ten seconds.

The 5,000 limit counts unique participants, not repeat submissions. Text-heavy
events shard earlier to stay below Temporal payload limits. Participant and event
data are stored in workflow histories; anyone with Temporal namespace access can
inspect them. Deploy this in a namespace appropriate for lead data.

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

Employee authentication sends only the catalog's `temporal_demo_auth` cookie to
the configured catalog verifier, which validates the session. The app requires
its `204` response with a subject and an exact `temporal.io` email domain; the
catalog's bootstrap identity is not accepted. No signing key is shared with this
app. Identity headers supplied by visitors and participant-entered emails cannot
grant admin access. Verification runs on every protected request with a
three-second timeout and no redirect following. A verifier outage blocks admin
access with `503`; public forms and submissions do not call the verifier. Admin
mutations require same-origin JSON requests. Public event URLs always show the
form, even if the visitor is an authenticated employee.

For a different catalog deployment, set both `AUTH_BASE_URL` and `AUTH_VERIFY_URL`
to its login origin and verifier endpoint.

`GET /healthz` checks the HTTP process. `GET /readyz` checks its Temporal connection.
The worker logs to stdout and shuts down gracefully on SIGINT/SIGTERM.

## tmprl-demo.cloud deployment

[deploy/demo-project.yaml](deploy/demo-project.yaml) is a sample platform manifest.
It exposes the web component publicly (`temporalAuthRequired: false`) so visitors
can submit without employee login; the application protects admin pages and APIs.
The platform injects Temporal credentials. The demo uses an unversioned worker
on the stable `event-leads-unversioned` task queue. Catalog rollouts replace the
runtime namespace, so pinned workers would strand events and retained queries
when their old namespace is removed. The fresh task queue also avoids the previous
queue's versioned routing configuration.
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
SSO requires the deployed hostname to be under `.tmprl-demo.cloud`. Completed
workflow queries require a compatible worker, even during retention. Keep changes
replay-compatible with Temporal patches and recorded-history replay checks.
The optional `TEMPORAL_REPLAY_HISTORY` test can replay a downloaded history file.
Previously pinned workflows still require their original worker deployment;
changing the task queue does not migrate those executions.

This repository prepares deployment artifacts; it does not change cluster-gitops
or deploy infrastructure automatically.
