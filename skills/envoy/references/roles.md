# How a role claim lives

Read this when a role you held is gone after a resume, fork or restart, or when `envoy_role_set` is refused.

- A claim belongs to the session, not the process. `omp --resume` claims again the role its transcript records, also after the reaper removed the old session's row.
- `/fork`, `/branch`, `/handoff` and any other switch that carries the transcript move the previous session's role to the new id. `/new` and `/resume` install an unrelated transcript and carry no role.
- A holder past the live window reads `unresponsive` and keeps its role. It is not dead: start no replacement for that session and take nothing from it.
  Only the role-claim reaper releases a claim, once the holder is `gone`.
- An automatic reclaim is a soft claim and never steals. The listener grants it when the role is unheld, when its holder is not `live`, or when the holder is the session this one continues (a fork's parent) and either no other client instance runs it live or one of the two carries no client instance id.
  Any other `live` holder keeps the role: the listener refuses, names that holder, and the resumed session logs a warning and holds nothing.
- A refused session keeps running and stays registered, without the role, and does not claim again at each heartbeat.
  The Oh My Pi plugin takes the role at its next session event once the holder is no longer live; the Claude Code channel takes it at an explicit claim. `envoy_role_get` names the holder now.
- Only an explicit `envoy_role_set` is last-claim-wins. Running it again for a role this session holds is harmless.
- `envoy_role_set` to another role is refused while another plugin holds subscriptions that depend on your current role; the refusal names them.
