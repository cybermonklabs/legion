# Waiting, and a grant you already hold

Read this when a run exits 75, and before you end a grant with `agent-secrets grant revoke`.

- Exit 75: the request still waits for a person or the model judge when `--wait` runs out.
  The message names who decides it, where, and the `agent-secrets status <request_id>` that reads it.
  A status poll that did not reach the broker (a reset, a timeout, a 502 or 503) does not end the wait: stderr says once that the command keeps waiting, and polling goes on.
  If the wait runs out while polls still fail, the exit is 75 too, and the message names the last failure.
  `agent-secrets status <request_id>` says whether the request was decided, and running the same command again (the same names and `--ttl`, from the same session) collects a grant it already has, asking no one again.
- `agent-secrets grant revoke <grant_id>` ends a grant given to this session's broker identity, from inside the session, before its lifetime runs out.
  The broker checks the enrollment, not the session: a grant of another enrollment is refused `was given to another broker identity (enrollment)`, and every session of a person who ran `agent-secrets login` on this machine signs as that sign-in's one enrollment, so each of them can end the others' grants.
