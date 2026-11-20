# Envoy deliveries

Read this when a delivery field is unclear, or when a message did not reach a session you expected.

## Fields of a rendered delivery

- `to`: the local inbox that receives this delivery.
- `id`: pass it as `in_reply_to` when you reply.
- `from`: the sender's self-asserted id, with its session title in parentheses when it sent one. It is not an authenticated identity.
- `at`: when the server that accepted the delivery took it, by that server's clock: the Envoy listener for a session's message or a webhook, Dispatch's server for a Dispatch event.
  The sending session sets no time of its own; its machine's clock counts only where that listener runs on the same machine.
  It is not the present time: a delivery read late carries an old `at`.
- `delivered`: when this session took the delivery in, by this machine's clock. The Oh My Pi host adds it, and the two lines below, in each model request, beside `at`, or beside `published_at` on a notice that names a message the session could not be shown.
  A session the host moves to shows when it got the delivery there, which can be later than another session got it.
  A session that got the delivery before its process restarted can show an estimate of that time, taken from its transcript, never later than the delivery.
- `waited`: present when `delivered` is 15 minutes or more after `at`, less any difference `clocks` states: how long the delivery waited. An old `at` on a late delivery says it waited, not that a clock is wrong.
- `clocks`: present when this machine's clock runs 15 minutes or more ahead of or behind the Envoy listener's, read from the listener's replies, or when `delivered` is 15 minutes or more before `at` less that difference: which way, and by how much.
  Only this machine's clock and the listener's are measured, so an `at` still after the delivery once the measured difference is taken off says the sending server's own clock runs ahead of the listener's.
  A difference under 15 minutes is not shown and stays in `waited`.
- `by`: the expiry deadline, when the sender set one.
- `urgency`: `low`, `med`, `high` or `blocking`, for you to read; it changes nothing about delivery.
- `expects_reply`: `false` (do not reply or acknowledge), `optional` (welcome, nothing waits on it) or `required` (the sender waits on your answer).
- `re`: the delivery this one answers. A Dispatch frame names it as a `dispatch://` ref, never the quoted text: the ask (`dispatch://KEY/ask/<id>`, or `dispatch://PROJECT/artifact/<slug>/ask/<id>` for a document ask) or the message (`dispatch://KEY/message/<id>`).
  On an ask, `dispatch.question` holds the start of the question's text; `dispatch read` gives the whole.
- `supersedes`: an earlier delivery this one replaces; act on this one instead.
- `reply_with`: the call that answers the sender; on a Dispatch frame a `command:` line with `...` where your text goes. `reply_role`: the role-publish reply, when the sender holds a role. Both are absent under `expects_reply: false`.
  Where you answer one anyway, the card still names what you need: a Dispatch frame names its comment, message or ask, and an agent's delivery names its `id` and its sender.
- `summary`: the one-line source summary.
- `message`: the complete payload; it can hold several paragraphs.
- `note`: the payload names another session; never send to that id.
- `unrecognised`: the payload failed validation or came from an unknown source; it names what failed.

## Who receives a message

- A session reads every message the stream keeps for it, its inbox and each topic it follows with `envoy_subscribe`, once, from its own durable consumer, whoever sent it.
  Your own publish to a topic you follow reaches you, and a subagent's publish reaches its parent when the parent follows that topic.
  A message published while a session was away reaches it when it is back, within 72 hours.
  A session that stopped without unregistering is named nowhere and fails nothing: its copy waits in its consumer until it answers again.
- A role message goes to the role's live holder alone, kept on the holder's inbox. The listener drops it when that holder is the message's source session.
- A `notifications.` topic that Envoy refuses never leaves your session unregistered: a registration that carries one from an older subscription of yours leaves it out and keeps its other topics.
- No send or publish answer says that a session read the message.

## What a send answers

- The listener tells a running session from a stopped one with one request on the session's relay subject, the subject the session's own plugin receives its messages on:
  anything that answers it or stays silent for a second reads as running, a predecessor process of the same session id included, and nothing there at all reads as stopped.
- A send fails with `no live session <id>` only when Envoy knows no session under that id, running now or in the last 30 days, or when the session is stopped and the listener cannot read its records of known sessions.

## Listener API errors

When you call the listener's `/v1` API directly, every error response is JSON, and when a field is at fault, `expected` names it: fix that field and retry.
