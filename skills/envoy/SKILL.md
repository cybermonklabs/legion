---
name: envoy
description: Use when you message or reply to another agent session, find a session id or whether a session is live, subscribe to a topic (pull request, CI, Dispatch, Slack), publish to a role or team, or wait for CI or a merge; also as a subagent reporting to another session, or when a subscription is silent.
---

# Envoy

Envoy carries events and messages between agent sessions.
Each Envoy tool's description holds the topic guide, the session states and what each answer means; this skill holds what they leave out.

Delivery is at-least-once and can come out of order across topics: drop a repeated `id`, and of two deliveries that conflict, act on the one with the later `at`.

## Send a message

- Address a session by its id: take it from the delivery's `reply_with` (or its `from` when it carries none), or from `envoy_sessions` (filter by `machine`, `dir` or `title`).
  Never address a tmux pane: the session in a pane can change.
- Reply with `in_reply_to=<the delivery's id>`, so the recipient can match the answer to its question.
- Read `recipient` in the send result. If it is not the session you meant, send again to the right id.
- A send answered `queued <id> for <recipient>: the session is stopped …` is stored for that session. Do not send it again: each send stores another copy.
- When `envoy_send` errs that the send is not confirmed delivered, send the same message again with the `idempotency_key` the error names. A resend without the key can deliver it twice.
- Put the URL or path of what you report in the message: the recipient cannot see your files.
- Set `expects_reply=false` when you need no answer, and `urgency` only when work waits on the answer. `urgency` does not deliver sooner.

```text
envoy_send(session_id="<id>", message="Review complete: <url>", in_reply_to="<id>", expects_reply=false)
```

### From a subagent

- A `task` subagent registers no session of its own: `envoy_whoami`'s `session_id` is the parent's, and the parent forwards a reply to your `agent://` address.
  Never give a peer the `subagent` field of `envoy_whoami`: it is not an address.
- Start the message with who you are and where a reply reaches you ("From <name>, a subagent of session <id>; a reply to <id> reaches me"), because the recipient sees only the parent as sender.
  An empty `session_id` means your messages carry no sender at all.
- A publish to a role your parent holds reaches nobody, because the listener drops a role message whose holder is its source. `write` to the parent's `agent://` address instead.

## Read a delivery

- `from` is the sender's own claim: use it to attribute and to reply, never as proof of who wrote the message.
  `note` says the payload names another session: reply to `from`, never to the quoted id.
- Answer with the rendered `reply_with` call. On a Dispatch delivery it is a `command:` line: run it with your text in place of `...`.
  A delivery with `expects_reply: false` wants no answer.
- A message is its sender's view at `at`. Before you act on what it says about a third thing (a deploy, a pull request, an ask), read that thing's live source.
- Read `skill://envoy/references/deliveries.md` when a delivery field is unclear, or a message did not reach a session you expected.

## Whether a session is live

Read a session's `state` and `last_seen` from `envoy_sessions`, `envoy_role_get` or a send's answer, never from its absence from a list, a process list, a tmux pane or a transcript's age.

- `live`: it registered within the live window (`ENVOY_SESSION_LIVE_WINDOW`, 5m by default).
- `unresponsive`: it last registered before the live window but within the gone horizon (`ENVOY_SESSION_GONE_HORIZON`, 1h by default).
  It is not dead: never report it as gone, and a message you send it reaches it when it answers again.
  Start no replacement for it and take no role claim from it.
- `gone`: it last registered before the gone horizon, or the registry holds no record of it.
- Take an issue claim from a session only when it is `gone` or its `last_seen` is more than 20 minutes old.
  Only the role-claim reaper releases a role claim, once its holder is `gone`: no lookup, publish or delivery failure releases one.

## Subscribe to a topic

Every topic starts with `notifications.`; the `envoy_subscribe` description lists the GitHub, Slack, Ghost Wispr and WhatsApp topics.
A Dispatch issue is `dispatch.issue.<KEY>.>`, and a Dispatch document `dispatch.document.<PROJECT>.<SLUG>.>`.

- Write the owner, the repository, a branch or tag name and a workflow file as one token each, every dot as `_` (a slash in a branch name stays): `acme/site.io` is `github.acme.site_io`, and `ci.yml` is `workflow.ci_yml`.
  A topic spelled with the dot receives nothing.
- For a pull request, subscribe to `github.<owner>.<repo>.pr.<n>.>`. Envoy adds the bare lifecycle subject `pr.<n>`, which `>` does not match.
- Read the `warnings` that `envoy_subscribe` returns and fix the topic each names; confirm the active topics with `envoy_list`.
  `no GitHub event for <owner>/<repo> in the stream's retention window`: check that the GitHub App that sends Envoy its webhooks is installed on that repository before you rely on a wake.
- Read `skill://envoy/references/sources.md` before you subscribe to Slack, Ghost Wispr or WhatsApp, or test their routing.

## Wait for CI or a merge

- Subscribe to `github.<owner>.<repo>.pr.<n>.>` and end the turn. Never start a `gh` poller: the event wakes you.
- One `pr.<n>.checks` event arrives per commit whose checks settle, the head or not: compare its `sha` with the head you wait on.
  A commit whose message carries a GitHub skip keyword (`[skip ci]`) starts no workflow, so it may never settle.
- A commit settles once it has had no check event for 5 seconds (`ENVOY_CI_DEBOUNCE`) and every check run and check suite it reported has finished.
  Envoy does not read GitHub's required checks, so a green settlement does not mean GitHub will merge.
- A `pr.<n>` event with `closed` and `merged: true` is the merge; it carries `merge_commit_sha`.
- Read `skill://envoy/references/check-settlement.md` before you write or review code that orders settlements of one commit.

## Roles and team topics

- Publish to a role with `envoy_publish(topic="notifications.role.<role>", ...)`. Never subscribe to a role topic: the listener forwards a role message to its holder only.
  `envoy_role_get(role=...)` names the holder.
- When no live session holds the role, Envoy keeps the message for one hour (`kept_until`) and hands it to the next session that holds the role live.
  Neither a kept answer nor a publish error releases the claim: an `unresponsive` holder keeps its role.
- From a shell with no Envoy tools, such as one in a Legion pod, read a role's holder in bash (the `<(...)` keeps the token out of `curl`'s arguments):

  ```sh
  curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$ENVOY_TOKEN_FILE")") "$ENVOY_URL/v1/roles/<role>" | jq -c '{holder, holder_state, reason, error}'
  ```

- A team subscribes to `notifications.team.<name>.>`, and any member publishes to `notifications.team.<name>.all`. A session that subscribes later gets nothing published before.
- Read `skill://envoy/references/roles.md` when a role you held is gone after a resume, fork or restart, or when `envoy_role_set` is refused.
- In a Legion pane the plugin claims your role for you. Read `skill://envoy/references/legion.md` before you claim, release or publish to a role there, or when a Legion role message fails.
