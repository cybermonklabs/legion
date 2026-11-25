# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role through a daemon-minted claim token, at your session's first turn or at a bound (30 s by default), whichever comes first (`packages/pi-shared/src/legion-claim-core.ts`).
  Never call `envoy_role_set` or subscribe to your role topic yourself.
- Never build a token from an issue number or title: a token spelled from a role reaches the agent named for that role, which need not be the one working the step of that role you mean.
  Where your issue ran a plan when you were started, your `Legion addressing` line names the token of the agent of each of its steps, and `dispatch steps --issue <KEY>` gives each run's `agent`.
- A Legion claim is last-claim-wins. It survives parking and worker re-creation, and stays through the issue's linger after close (`linger_hours`).
- `no_holder`, `holder_lapsed` and `delivery_failed` on a Legion role are the daemon's liveness signals, and the daemon acts on them: it revives or recreates the worker and delivers again.
  Never add a second subscriber, retry by hand, or forward these payloads to an architect.
  - `no_holder`: no session holds the claim. A Legion role whose agent process exits is `gone` at once, so its successor takes the role without waiting out the live window.
  - `holder_lapsed`: the holder has not registered within the live window. It keeps the role (`claim_released: false`), nothing replaces it, and the daemon holds its notice and publishes it again.
    A publish of yours to such a role is kept: `202` with `kept_reason: holder_lapsed` and `kept_until`.
  - `delivery_failed`: the stream stored no forward for the holder. It releases no claim.
- A role message to a released Legion role is kept for one hour and wakes the role. The woken session gets it as an Envoy card with `kept_at`, which can come before the daemon's task.
- A role send the listener refuses `404` also raises an exception on `notifications.envoy.exceptions.notifications.role.<role>`, naming `unclaimed` or `holder_lapsed`.
  No consumer acts on it, and the daemon decides by itself whether to send its own refused message again: never re-send a refused message by hand because of one.
