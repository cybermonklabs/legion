# Envoy in a Legion pane

Read this in a Legion pane before you touch a role claim, or when a Legion role message fails.

- The Legion plugin claims your assigned role through a daemon-minted role token, at your session's first turn or at a bound (30 s by default), whichever comes first (`packages/pi-shared/src/legion-claim-core.ts`).
  Never call `envoy_role_set` or subscribe to your role topic yourself.
- Never build a role token from an issue number or title: only the daemon maps tokens to issues.
- A Legion claim is last-claim-wins. It survives parking and worker re-creation, and stays through the issue's linger after close (`linger_hours`).
- `no_holder`, `holder_lapsed` and `delivery_failed` on a Legion role are the daemon's liveness signals, and the daemon acts on them: it revives or recreates the worker and delivers again.
  Never add a second subscriber, retry by hand, or forward these payloads to an architect.
  - `no_holder`: no session holds the claim. A Legion role whose agent process exits is `gone` at once, so its successor takes the role without waiting out the live window.
  - `holder_lapsed`: the holder has not registered within the live window. It keeps the role (`claim_released: false`), and the daemon holds its notice and publishes it again.
    A publish of yours to such a role is kept: `202` with `kept_reason: holder_lapsed` and `kept_until`.
  - `delivery_failed`: the stream stored no forward for the holder. It releases no claim.
- A claim whose session has not registered with Envoy for more than 20 minutes is unreachable.
  The daemon marks it, and `read_state` and `read_record` show it with the time of that last registration.
  Your project's controller relaunches it on that same session with the `legion` tool's `relaunch`; the daemon relaunches the controller's own claim only under `controller: daemon` in `legion.yaml`.
  Nothing replaces a session whose last registration is 20 minutes old or less, and a relaunch replaces no session: your workspace and the task you owe stay, and the resumed session is told that its turn was cut.
- A role message to a released Legion role is kept for one hour and wakes the role. The woken session gets it as an Envoy card with `kept_at`, which can come before the daemon's task.
- A role send the listener refuses `404` also raises an exception on `notifications.envoy.exceptions.notifications.role.<role>`, naming `unclaimed` or `holder_lapsed`.
  No consumer acts on it, and the daemon's own refused message retries by itself: never re-send a refused message by hand because of one.
