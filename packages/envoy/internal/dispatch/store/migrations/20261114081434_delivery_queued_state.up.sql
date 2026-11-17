-- 20261114081434_delivery_queued_state.up.sql
-- INFRA-626: a fourth delivery attempt state, `queued`. The Envoy listener now takes a message for
-- a session it knows and that is not running, stores it, and answers `state: "stopped"`
-- (cmd/listener/api.go, sendHandler); the session reads it from its own consumer when it runs
-- again, within the 72 hours the stream keeps it. Dispatch records such an attempt `queued` rather
-- than `failed`, so a person's direct message, mention or broadcast to a stopped but known session
-- waits for that session instead of being thrown away.
--
-- Every statement widens what the schema allows and narrows nothing, which is what a rolling
-- update requires (packages/envoy/cmd/dispatch/README.md, "Database migrations"): the release
-- before this one writes only 'pending', 'sent' and 'failed' and never names the new column, all
-- of which this schema still takes, so the old build and the new build run against it at once.
--
-- The two state checks, 0039's on comment_deliveries and 0046's on message_deliveries, gain
-- 'queued'. Each is dropped without `cascade` and added back `not valid`, which records the check
-- for every row written from now on without reading a stored one: no stored row can fail a check
-- wider than the one it was written under, so the read a validation makes would find nothing.
--
-- message_deliveries gains `connected_only`: null, or 'connected_only' where the request that
-- stored the message asked for a session that is running now - the live view's model and thinking
-- switch alone asks it, on the agents route (api/messages.go, createAgentMessageInput.ConnectedOnly).
-- The flag is attempt 1's, which every later attempt inherits: a Retry or a mode change opens its
-- attempt with the first attempt's value (api/message_delivery.go, insertMessageDeliveryAttempt),
-- so a refused switch is refused again and nothing is stored for it. It is persisted because a
-- later send of an attempt is made by someone else - the delivery sweep, or a repeat under the
-- request's key - which never saw the request. The switch rule itself is no row's: every send of a
-- person's own Send or Aside (not one an agent wrote for them) on no issue, no broadcast's copy,
-- whose body is one /model or /effort line asks for a running session, judged from the message and
-- the mode that attempt is sent in
-- (api/messages.go, isSessionSwitch; api/message_delivery.go, deliverMessageResuming), so a switch
-- whose rows carry no flag is still never stored for a stopped session to apply at an unknown
-- later time, and every other message, a BTW or an issue message among them, is queued. A nullable
-- text column with no default is written into the catalogue alone and rewrites no row; null is
-- every stored row's own answer, since no earlier release sent a request that asked. Its check is
-- added `not valid` for the same reason as the state checks: no stored row holds a value.
-- comment_deliveries gains nothing: a comment's mention is never a switch.
--
-- Every statement is one of the census's catalog-only forms (pgmigrate's catalogOnlyAlterTable:
-- drop constraint without cascade, add check not valid, add nullable text column without a
-- default), so it is exempt from the table size limit at any size; each takes ACCESS EXCLUSIVE on
-- its table for its catalogue write alone, under the runner's lock_timeout. No row is refused or
-- rewritten, so the census answers 0. Rolling back past this migration is an image rollback alone
-- while no row carries 'queued'; once one does, the checks have to be widened again by hand
-- before an earlier set is restored.
--
-- The index the session's answers read over these states is the next migration's
-- (20261114081435), built concurrently, and the one it replaces is dropped after it
-- (20261114081436).
alter table comment_deliveries
  drop constraint comment_deliveries_state_check,
  add constraint comment_deliveries_state_check
    check (state in ('pending', 'sent', 'queued', 'failed')) not valid;

alter table message_deliveries
  drop constraint message_deliveries_state_check,
  add constraint message_deliveries_state_check
    check (state in ('pending', 'sent', 'queued', 'failed')) not valid,
  add column connected_only text,
  add constraint message_deliveries_connected_only_check
    check (connected_only = 'connected_only') not valid;
