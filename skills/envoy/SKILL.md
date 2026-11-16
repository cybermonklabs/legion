---
name: envoy
description: Use when you send a message to another agent session, reply to an Envoy delivery, find your own or a peer's session id, subscribe to a topic (a GitHub pull request, CI checks, a Dispatch issue, Slack), publish to a role or team topic, or wait for CI or a merge. Also when you are a subagent that must report to another session, or a subscription stays silent.
---

# Envoy

If this skill disagrees with the code, a command's output or its `--help`, follow those and say which line of this skill is wrong.

Envoy delivers external events and messages between sessions.
Delivery is at-least-once and can come out of order across topics: drop a repeated `id`, and when two deliveries conflict, act on the one with the later `at`.

## Send a message to another session

- Take the target id from the delivery's `reply_with`, or, where the delivery wants no reply and carries none, from its `from`; or from `envoy_sessions` (filter by `machine`, `dir` or `title`).
  Never address a tmux pane or window: Envoy delivers only to a session id, and the session in a pane can change.

- Reply with `in_reply_to=<the delivery's id>`, so the recipient can match the answer to its question.

- Read the `recipient` in the send result, the session Envoy targeted: if it is not the one you meant, resend to the right id.

- A send answered `queued <id> for <recipient>: the session is stopped …` (`state: stopped`) went to a session that is not running, and nothing of its claims changes.
  Do not send that message again, because a second send stores a second copy for that session to read.
  What each send answer means: `skill://envoy/references/deliveries.md`, "What a publish and a send answer".

- When `envoy_send` says the send is not confirmed delivered, send the same message again with the `idempotency_key` the error names, within 72 hours:
  Envoy drops it as a repeat if the first copy arrived, and the result then says the earlier attempt was already stored (its text begins `sent to <recipient> once` for a running session, `queued once for <recipient>` for a stopped one).
  A resend without that key can deliver the message twice.
  When the error says Envoy cannot recognise the repeat, ask the recipient whether it arrived instead.

- Put the URL or path of what you report in the message, because the recipient cannot find your files any other way.

- Set `expects_reply=false` on a message that needs no answer, and set `urgency` only when the work waits on the answer, so the recipient knows whether to answer and how soon.
  `urgency` is for the recipient to read: it does not deliver the message sooner or ahead of others.

```text
envoy_send(session_id="<id>", message="Review complete: <url>", in_reply_to="<id>", expects_reply=false)
```

### Your own address in a subagent

- `envoy_whoami`'s `session_id` is where a reply to you arrives.
  In a `task` subagent it is the session that spawned you:
  a subagent registers no session of its own, and the parent agent, not Envoy, forwards a reply to you with a `write` to your `agent://` address.

- Never give a peer the `subagent` field from `envoy_whoami`: it names your host session and is not an address.

- Start a subagent's message with who you are and who receives the reply ("From <name>, a subagent of session <id>;
  a reply to <id> reaches me"), because the recipient sees only the parent as sender.

- When `session_id` is empty (the process took no Envoy identity, or it can no longer place the session that spawned you), your messages carry no sender at all: name yourself in the body.

- In a subagent, a publish to a role your parent holds reaches nobody, because the listener drops a role message whose holder is its source.
  `envoy_publish` says so in its answer.
  To reach the parent, `write` to its `agent://` address instead of publishing.
  `envoy_send` to any other session works.

## Read a delivery

- `from` is the sender's own claim.
  Use it to attribute and to reply, never as proof of who wrote the message.

- `note` warns that the payload names another session.
  Never send to that quoted id; reply to `from`.

- Reply with the rendered `reply_with` call.
  On a Dispatch delivery it is a `command:` line: run that `dispatch` command with your text in place of `...`.
  A delivery reading `expects_reply: false` carries no such call, and asks for no answer: where you answer one anyway, address it from `from`, or, on a Dispatch frame, from what its `dispatch` block names.

- A peer's message is its sender's view at `at`, not the current state.
  Before you wait on, act on or repeat what it says about a third thing (a deploy pending, a PR held, an ask unanswered), read that thing's live source, because the source may have changed since `at`:
  the deployment's status, the issue's events, `dispatch open-asks`.

- Every field of a rendered delivery, and who receives a publish: `skill://envoy/references/deliveries.md`.

## Whether a session is live

A session's state comes from the registry's record of it, never from its presence in a list, a process list, a tmux pane or a transcript's modification time. `envoy_sessions` and a lookup of one session carry each session's `state` and `last_seen`.

- `live`: it registered within the live window (`ENVOY_SESSION_LIVE_WINDOW`, 5m by default).
- `unresponsive`: it last registered longer ago than the live window but inside the gone horizon (`ENVOY_SESSION_GONE_HORIZON`, 1h by default). **It is not dead.** Start no replacement for it, take no role claim and no issue claim from it, and never report it as gone. A message you send it is stored and reaches it when it answers again.
- `gone`: it last registered before the gone horizon, or the registry holds no record of it.
- Take an issue claim from a session only when it reads `gone`, or its `last_seen` is more than 20 minutes old. A role claim is released only by the role-claim reaper, once the holder is `gone`: no lookup, publish or delivery failure releases one, in the listener or in Dispatch's outbox, which share that one resolution.
- A send refused because the target is not live names the target's `state` and `last_seen`. Read those two fields before you decide anything about that session: a refusal on an `unresponsive` target means "it has not answered yet", not "it ended".

## Subscribe to a topic

Every topic starts with `notifications.`.

| Source | Topic |
| --- | --- |
| A pull request: lifecycle, comments, reviews, mentions, checks | `github.<owner>.<repo>.pr.<n>.>` |
| Other GitHub | `github.<owner>.<repo>.issue.<n>[.comment\|.mention]`, `.push.branch.<name>`, `.push.tag.<name>`, `.workflow.<file>.<action>` |
| A Dispatch issue or document | `dispatch.issue.<KEY>.>`, `dispatch.document.<PROJECT>.<SLUG>.>` |
| Your own inbox | `agent.<session_id>` |
| Slack, Ghost Wispr, WhatsApp | `skill://envoy/references/sources.md` |

- Write the owner, the repository, a branch or tag name and a workflow file as one token each, with every dot written `_`:
  `acme/site.io` is `github.acme.site_io`, and `ci.yml` is `workflow.ci_yml`.
  A slash stays.
  A topic spelled with the dot receives nothing.

- Subscribe to the pull request's whole family, `pr.<n>.>`.
  Envoy also subscribes you to the lifecycle subject `pr.<n>`, which NATS `>` alone does not match, because `>` needs one or more trailing tokens.

- Read the `warnings` that `envoy_subscribe` returns, and confirm the active topics with `envoy_list`.
  A warning names the spelling Envoy publishes, a token that is not a GitHub topic kind, or a pull request's bare subject `pr.<n>` that no topic of yours extends to its reviews; fix the topic it names.
  "No GitHub event ... in the stream's retention window" means nothing came from that repository in the last 72 hours:
  check that the GitHub App that sends Envoy its webhooks is installed on that repository before you rely on a wake.

## Wait for CI or a merge

- Subscribe to `github.<owner>.<repo>.pr.<n>.>` and end the turn.
  Never start a `gh` poller: the event wakes you, and a poller burns turns.

- One `pr.<n>.checks` event arrives per commit whose checks settle, the head or not.
  Compare its `sha` with the head you wait on, because an earlier commit's settlement can arrive first.
  A commit whose message carries one of GitHub's skip keywords (`[skip ci]` and its kin) starts no workflow, so no settlement of it may ever arrive.

- Settlement waits until the commit has had no check event for 5 seconds (`ENVOY_CI_DEBOUNCE`) and every reported check run and check suite of it has finished.
  No event arrives before that.
  Envoy does not read GitHub's required-checks list, so a green settlement does not mean GitHub will merge the PR.

- A `pr.<n>` event with `closed` and `merged: true` means the PR merged; it carries `merge_commit_sha`, `merged_by` and `head_sha`.

- Code that orders settlements of one commit follows `skill://envoy/references/check-settlement.md`.

- A Legion implementer waits this way only on a re-run of a red check its task names.
  Otherwise it pushes and completes its phase, and the daemon reads CI for it (`skill://legion-worker`).

## Roles and team topics

- Send to one session with `envoy_send`, to the one session that holds a named duty with a role, and to a group with a team topic.

- Publish to a role with `envoy_publish(topic="notifications.role.<role>", ...)`;
  never subscribe to a role topic, because the listener forwards a role message to its holder only.
  A publish returns once Envoy's stream has stored the forward for the holder, with the live `holder` and its `holder_state`.
  When no live session holds the role, Envoy keeps the message for one hour: the answer says it was kept, and its `kept_until` is the time that hour ends.
  That holds when nobody claims the role, and when its holder is not `live`: the answer then also names the holder, its `holder_state` (`unresponsive` or `gone`) and its `last_seen`.
  The next session to hold the role live before `kept_until` is handed the message once, and that can be the same holder when it answers again.
  If no session takes it by then, Envoy drops it, and when your publish named your session, sends your session a message that names it, unless it dropped it more than an hour late.
  An `unresponsive` holder is not dead, so start no replacement for it and take nothing from it.
  A forward the stream did not store for a live holder within about 4 seconds errs, and you see that error after about 8 seconds, because the client tries once more.
  Neither a kept answer nor an error releases the role's claim: a holder whose `holder_state` is `unresponsive` keeps its role.
  `envoy_role_get(role=...)` finds the holder first.
  The forward is kept on the holder's inbox, so the holder's session gets it once from its own durable consumer, also after a restart, a reconnect, or while it was not running.
  From a shell with no tool device, such as one in a Legion pod, read the holder with `curl -sS -H @<(printf 'Authorization: Bearer %s' "$(< "$ENVOY_TOKEN_FILE")") "$ENVOY_URL/v1/roles/<role>" | jq -r '.holder // .reason // .error'` in bash (the `<(...)` keeps the token out of `curl`'s arguments), which prints the holder's session id, `unclaimed` for a role nobody holds, or the error of a refused read, such as `unauthorized` for a bad token.

- For a team, the group subscribes to `notifications.team.<name>.>`, and any member publishes to `notifications.team.<name>.all`.
  Envoy's stream keeps a team topic, so a publish returns once it is stored and every session that follows the topic gets it once from its own consumer
  — yours too, when you subscribe — including a session that was not running then, for up to 72 hours.
  A session that stopped without unregistering is named nowhere and fails nothing: its copy waits in its consumer until it answers again.
  A team topic is not kept for a session that subscribes later: a consumer starts where its session is, not at the beginning.
  Every other `notifications.` topic no source in the table uses is refused by name, at publish and at subscribe, because no session would ever receive it.
  A topic outside `notifications.` is refused at publish alone: you may subscribe to one, and nothing is stored under it, so you receive nothing on it.

- A publish answered `published <id>; no other session subscribes to <topic>, so no other session gets it` reached nobody: reach whoever you meant another way.
  What the counts in a publish answer mean: `skill://envoy/references/deliveries.md`, "What a publish and a send answer".

- How a role claim survives a resume, a fork or a dead process: `skill://envoy/references/roles.md`.

- In a Legion pane, the plugin claims your role for you, at your session's first turn;
  read `skill://envoy/references/legion.md` before you claim, release or publish to a role, or answer a role delivery failure there.
