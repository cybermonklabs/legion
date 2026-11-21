# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role through a daemon-minted role token, at your session's first turn or at a bound (30 s by default), whichever comes first (`packages/pi-shared/src/legion-claim-core.ts`).
  Never call `envoy_role_set` or subscribe to your role topic yourself.
- Never build a role token from an issue number or title: only the daemon maps tokens to issues.
- A Legion claim is last-claim-wins. It survives parking and worker re-creation, and stays through the issue's linger after close (`linger_hours`).
- `no_holder`, `holder_lapsed` and `delivery_failed` on a Legion role are the daemon's liveness signals, and the daemon acts on them: it revives or recreates the worker and delivers again, or relaunches the root process, which gets a `catch-up` notice of its tree and reads each issue's handoffs on its Dispatch issue (`dispatch handoff-read`).
  Never add a second subscriber, retry by hand, or forward these payloads to an architect.
  - `no_holder`: no session holds the claim. A Legion role whose agent process exits is `gone` at once, so its successor takes the role without waiting out the live window.
  - `holder_lapsed`: the holder has not registered within the live window. The daemon's own notices send `keep: false`, so the listener refuses such a notice `404` and forwards nothing.
    The error names the holder, its `holder_state` and its `last_seen`, and says `claim_released: false`. The holder keeps the role, nothing replaces it, and the daemon holds its notice and publishes it again.
    A publish of yours to such a role is kept: `202` with `kept_reason: holder_lapsed` and `kept_until`.
  - `delivery_failed`: the stream stored no forward for the holder: the holder lookup failed, no live holder was left to forward to, the publish errored, or the stream did not store the forward within the send's window.
    Every forward goes into the stream, so a holder that is live but not reading is forwarded to like any other, and its own consumer reads the message when it answers again; nothing is published live, and a holder cannot have seen a message the stream never stored.
    It releases no claim.
  - No `receipt_timeout`: the listener waits for no receipt, so no signal here says a holder was slow. A role send answered `200` says that the stream stored the forward for the holder.
- A role message forwarded to you is kept on your inbox in the stream, which your own durable consumer reads, so your session gets it once, also after the pane restarts or reconnects.
- A role message to a released Legion role is kept for one hour and wakes the role. The listener reports the wake at the keep.
  Its sweep reports the wake again every five minutes while the message has at least 20 minutes of its hour left and no live session holds the role.
  The woken session gets it as an Envoy card with `kept_at`, which can come before the daemon's task.
- A role send the listener refuses `404` also raises an exception on `notifications.envoy.exceptions.notifications.role.<role>`, naming `unclaimed` or `holder_lapsed`.
  The listener refuses only a publish it keeps nothing for: one sent with `keep: false`, as the daemon's own notices are, or one whose keep failed.
  No consumer acts on it, and the daemon's own refused message retries by itself until the role is held or its architect cannot hold it again: never re-send a refused message by hand because of one.
