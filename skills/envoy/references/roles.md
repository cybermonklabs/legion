# How a role claim lives

Read this when a role you held is gone after a resume, fork or restart, when `envoy_role_set` is refused, or when Envoy keeps a role publish because no live session holds the role.

- A claim belongs to the session, not the process. `omp --resume` claims again the role its transcript records, also after the reaper removed the old session's row (the reaper removes a row only once that session is `gone`: past the gone horizon, `ENVOY_SESSION_GONE_HORIZON`, 1h by default).
- `/fork`, `/branch`, `/handoff` and any other switch that carries the transcript move the previous session's role to the new id. `/new` and `/resume` install an unrelated transcript and carry no role.
- A holder past the live window (`ENVOY_SESSION_LIVE_WINDOW`, 5m by default) reads `unresponsive` and keeps its role.
  Envoy keeps a role publish to it for one hour and answers that it kept it, with `kept_reason: holder_lapsed`, the holder, its `holder_state`, its `last_seen` and `kept_until`, the time that hour ends.
  The next session to hold the role live in that hour gets the message: the holder when it answers again, or a session that claims the role.
  If no session takes it by `kept_until`, Envoy drops it, and when your publish named your session, sends your session a message that names it, unless it dropped it more than an hour late.
  An unresponsive holder is not dead: start no replacement for that session and take nothing from it.
  Only the role-claim reaper releases a claim, once the holder is `gone`.
- A client that stops cleanly deregisters its own session and sends its client instance id, and the listener removes the registration only when the registration carries the same id or none, so a departing client never removes a registration another process made under the same session id.
- A role message the listener forwarded to your session while you held the role is kept on your inbox in the stream, which your own durable consumer reads, so your session gets it once after a resume or a reconnect within 72 hours, while your session still holds the role. Once the role moved, the message goes to the role's holder through its kept lane.
- An automatic reclaim is a soft claim and never steals. The listener grants it when the role is unheld, when its holder is not `live` (`gone`, or `unresponsive` past the live window, which is how a relaunched Legion role recovers it), or when the holder is the session this one continues (a fork's parent) and that predecessor is this client instance's: the predecessor is not `live`, or its live registration carries this instance's own id, or one of the two sides carries no instance id.
  A client instance is one Oh My Pi Envoy extension instance, or one Claude Code channel-server process; each mints its id when it starts.
  Any other `live` holder keeps the role: the listener refuses, names that holder, and the resumed session logs a warning and holds nothing.
- A refused session keeps running and stays registered, without the role, and does not claim again at each heartbeat.
  The Oh My Pi plugin takes the role at its next session event once the holder is no longer live; the Claude Code channel takes it at an explicit claim. `envoy_role_get` names the holder now.
- Only an explicit `envoy_role_set` is last-claim-wins. Running it again for a role this session holds is harmless.
- `envoy_role_set` to another role is refused while another plugin holds subscriptions that depend on your current role; the refusal names them.
