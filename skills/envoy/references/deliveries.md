# Envoy deliveries

Read this when a delivery field is unclear, or when a message did not reach a session you expected.

## Fields of a rendered delivery

- `id`: pass it as `in_reply_to` when you reply.
- `from`: the sender's self-asserted id, with its session title in parentheses when it sent one. It is not an authenticated identity.
- `at`: when the server that accepted the delivery took it, by that server's clock: the Envoy listener for a session's message or a webhook, Dispatch's server for a Dispatch event. A delivery read late carries an old `at`.
- `delivered`: when this session took the delivery in, by this machine's clock. The Oh My Pi host adds it, and the two lines below, in each model request.
- `waited`: present when `delivered` is 15 minutes or more after `at`, less any difference `clocks` states: how long the delivery waited. An old `at` on a late delivery says it waited, not that a clock is wrong.
- `clocks`: present when this machine's clock runs 15 minutes or more ahead of or behind the Envoy listener's: which way, and by how much.
- `urgency`: `low`, `med`, `high` or `blocking`, for you to read.
  Under Oh My Pi, a `low` card of the Legion daemon, or of a Dispatch event other than an ask's or a message or mention to you, starts no turn: you read it at your next turn.
  No other card's urgency changes its delivery.
- `expects_reply`: `false` (do not reply or acknowledge), `optional` (welcome, nothing waits on it) or `required` (the sender waits on your answer).
- `re`: the delivery this one answers. On a Dispatch frame it names the ask or the message, and `dispatch.question` holds the start of an ask's text; `dispatch read` gives the whole.
- `supersedes`: an earlier delivery this one replaces; act on this one instead.
- `reply_with`: on a Dispatch frame, the call that answers, a `command:` line with `...` where your text goes; absent under `expects_reply: false`.
  A card from a session carries none: answer the session at the start of its `from`, or the role its `role` (or `roles`) names.
- `note`: the payload names another session; never send to that id.
- `unrecognised`: the payload failed validation or came from an unknown source; it names what failed.

## Who receives a message

- A session reads every message the stream keeps for it, its inbox and each topic it follows with `envoy_subscribe`, once, from its own durable consumer, whoever sent it.
  Your own publish to a topic you follow reaches you, and a subagent's publish reaches its parent when the parent follows that topic.
  A message published while a session was away reaches it when it is back, within 72 hours.
- A role message goes to the role's live holder alone, kept on the holder's inbox. The listener drops it when that holder is the message's source session.
- No send or publish answer says that a session read the message.
- A listener `/v1` API error is JSON; when a field is at fault, `expected` names it.
