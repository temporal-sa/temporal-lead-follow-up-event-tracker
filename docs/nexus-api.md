# Nexus API

Other apps reach the tracker through the `event-leads` Nexus service. They can
create an event, read it, and submit a response. The service is generic: it
knows nothing about who calls it. Which projects may call it is configuration,
`allowedCallerProjects` on the `event-leads-participants` endpoint in
`deploy/demo-project.yaml`.

All operations are synchronous, so each answers within Nexus's 10-second limit.

## Operations

| Operation | Input | Output |
| --- | --- | --- |
| `create_event` | `requestId`, `name`, `description`, `endDate`, `form`, `subheading`, `qrBanner`, `preview`, `createdBy` | `PublicEvent` |
| `get_event` | `eventId` (the ID or any of its links) | `PublicEvent` |
| `submit_participant` | `eventId`, `submission` | `status` |
| `lookup_participant` | `eventId`, `email` | `found`, `name`, `role`, `email` |

`PublicEvent` is `id`, `link`, `name`, `description`, `endDate`, `status`, and
`closesAt`.

### `create_event`

This takes the same fields as the admin create form and goes through the same
`NewEvent` validation. The tracker adds no defaults of its own beyond those the
admin form gets. A missing `form` is `DefaultForm()`, as in the admin form. The
caller decides the questions and the end date.

`createdBy` is recorded as the event's creator. Nexus carries no sign-in, so the
tracker takes it as given. The endpoint's caller allowlist is the trust
boundary.

**Retries.** The slug loop that the admin handler runs today (`kubecon-2026`,
then `-2`, …) moves into a function both handlers call. On its own, a retried
Nexus call would run the loop again and create a second event. So the event
stores the caller's `requestId` as `CreateRequestID`. When a slug is taken, the
loop resolves that slug, and if it holds an event with the same
`CreateRequestID`, it returns that event and stops. A retry walks the same slug
sequence, so it finds its own event. The admin handler passes no request ID and
behaves as it does now.

### `submit_participant`

This is unchanged except that the result is `status` only. The tracker already
keeps one participant per email per event, and a request ID it has seen before
with the same details is acknowledged from its receipt without a write. A caller
that wants "join many times, one response" sends a request ID derived from the
details.

Internal-only events still reject Nexus submissions, since the call carries no
sign-in. If someone adds a required question in the editor, submissions without
it are rejected as `BadRequest` with the tracker's message.

## Removed: flight passes

`FlightPassWorkflow`, `claim_flight_pass`, `FlightCode` in the
`submit_participant` result, and `internal/tracker/flight.go` existed only for one
game. They were removed, along with the RUNBOOK's "Game flight codes" entry and the
flight-pass steps in `docs/storage-cutover.md`. `list_events` was removed too; nothing
calls it once the game tracks its own events.

The Worker is unversioned. Before deploying, wait until no `FlightPassWorkflow`
is running (each lives 30 minutes) or terminate them. A workflow whose type is
no longer registered would fail its tasks.

`CreateRequestID` is a new field on the stored event. Running `EventWorkflow`
histories decode it as empty, so they replay unchanged.
