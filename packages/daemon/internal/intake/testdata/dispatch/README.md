# Dispatch envelopes of the approvals signal's inputs

Each file here is the Envoy envelope Dispatch's outbox publishes for one event, and the tests of package intake decode it as the daemon's consumer does.

## `project-default-assignee.json`

The one settings event the daemon hears, `settings.project_default_assignee.updated`.
Its event is the project's real event of 2026-11-15 (id 563443, seq 4225), read from Dispatch's event log (`GET /api/v1/events/log`) and neutralized for a public repository, as `packages/daemon/internal/replay/testdata/signals/dispatch/settings-events.json` holds it.
The envelope is built as Dispatch's outbox builds one (`envelope` in `packages/envoy/internal/dispatch/outbox/publisher.go`): the subject is `notifications.dispatch.project.<project>.<type>`, the payload is the event with `quiet` beside its members, and `issued_at` is the event's `created_at` in epoch milliseconds.

## The ask and issue envelopes

`ask-opened.json`, `ask-answered.json`, `ask-resolved.json`, `ask-asked-of.json`, `ask-resolved-declined.json`, `comment-ask-agent.json` and `comment-ask-human.json` are built by hand from Dispatch's own types (`model.AskEventPayload`, and `model.CommentEventPayload` with `ask_state` and `ask_waiting_on`).
`issue-created.json`, `issue-updated.json` and `issue-closed.json` were captured from a running Dispatch, and carry the issue's `assignee`.

## `../dispatch-events/acme-29-signal-inputs.json`

Three real events of one issue as Dispatch's event log serves them (`GET /api/v1/issues/<KEY>/events`), the shape `DecodeDispatchEvent` reads: an `issue.updated` that names the issue's `assignee` (seq 2), an `ask.opened` whose `asked_of` is null (seq 23) and an `ask.answered` of an approval request (seq 40).
They are three of the events of `packages/daemon/internal/replay/testdata/captured/events/`, whose `CAPTURED.md` gives how they were read and neutralized, kept to the members the decoder reads.
