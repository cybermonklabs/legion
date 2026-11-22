# Envoy

- Compare a topic through `contracts.TopicForms`, never as one string.
  An issue's events, an agent's messages and a role's lane each have an old form and a project form (`CanonicalTopic`, `LegacyTwin`), and readers take both until the old form is refused.

- In Dispatch, never call the listener while a transaction or a pooled connection is held.
  Resolve first and lock after (`resolveThenLock`), or send between a claim transaction and a settle transaction, as the delivery senders do.

- Dispatch and the secrets broker (`cipherd`) roll: while a release deploys, the previous build serves the same database beside the new one, and for Dispatch the same NATS and the clients of either build.
  So a migration that drops, renames or narrows ships a release after the one that stops reading the old shape, a route is removed a release after the one that stops calling it, and state a later request needs lives in Postgres or a signed cookie, never in one process's memory.
  The section "Two builds at once" of `packages/envoy/cmd/dispatch/README.md` holds the rule for each surface the two builds share.
  CI's `dispatch-two-builds` job runs this tree beside the previous release only over the surfaces it drives, so a passing run does not prove a change safe.

- A message reaches a session only through the session's own durable consumer on `ENVOY_NOTIFICATIONS` (`bus.SessionConsumers`), which its listener relays.
  Add no core NATS subscription to a session's subject and no fallback path: a session would get nothing published while it was away.

Where this file and the code disagree, the code is right: follow the code and correct this file.
