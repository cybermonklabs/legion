import { postmortem } from "@oh-my-pi/pi-utils";

/**
 * The longest an exit waits for the host's stop to end (`exitAfterHostStop`). It copies the grace
 * the worker shim gives Oh My Pi between its SIGTERM and its SIGKILL, the Go constant
 * `shim.DefaultGrace` (`packages/daemon/internal/shim/shim.go`), whose comment names this one:
 * a change of either is made to both. The shim gives the agent this grace whatever its own
 * `--stop-grace` is: `legion worker-shim` sets `Grace: shim.DefaultGrace`. Nothing in the pane's
 * environment carries it, so it is copied here. pi-envoy's stop ends well inside it: its release,
 * its DELETE and its drain race `SHUTDOWN_DEADLINE_MS`, one second
 * (`packages/pi-envoy/extensions/envoy.ts`).
 */
export const HOST_STOP_GRACE_MS = 5_000;

/** How a failed claim ends the process (`exitAfterHostStop`). */
export interface HostStopExit {
  /** Ends the process with the code it is given. */
  readonly exitProcess: (code: number) => never;
  /** The host's stop once it has begun: a promise that settles when the stop ends. Undefined
   * while no stop has begun. `ompHostStop` in Oh My Pi; a test passes its own. */
  readonly hostStop: () => Promise<void> | undefined;
}

/**
 * The stop of this Oh My Pi process once it has begun: a promise that settles when the stop ends.
 * Undefined while no stop has begun.
 *
 * Oh My Pi runs its exit callbacks (`postmortem` of `@oh-my-pi/pi-utils`, the host's module) on a
 * SIGTERM, a SIGINT, a SIGHUP, a quit and a fatal error. It waits for them before the process
 * exits. pi-envoy's stop is one of them (`pi-envoy-session-stop`). In RPC mode, which a Legion
 * pane runs, a SIGTERM emits no `session_shutdown`, so that callback is the session's stop, with
 * its role release and its DELETE.
 *
 * `postmortem` itself tells whether that stop has begun, so neither plugin reads anything new
 * from the other. An exit-only callback registered after an exit's pass has begun runs at once.
 * One registered before any pass waits for one, and a keep-alive cleanup runs none
 * (`postmortem.register`). So a probe registered here and cancelled at once has run only when the
 * process's stop has begun. A cleanup asked for then starts no pass of its own: it answers the
 * promise of the pass that runs, which settles when every callback has settled or at Oh My Pi's
 * own deadline (`postmortem.cleanup`). After that pass it answers at once. It is asked for only
 * after the probe ran: asked for before, it would run every callback itself.
 */
export function ompHostStop(): Promise<void> | undefined {
  let begun = false;
  const cancel = postmortem.register(
    "pi-legion-host-stop-probe",
    () => {
      begun = true;
    },
    { exitOnly: true }
  );
  cancel();
  return begun ? postmortem.cleanup() : undefined;
}

/**
 * Ends the process with `code`, as a failed claim does (R16 of dispatch://INFRA-3589). When the
 * host's stop has begun (`hostStop`), the exit waits for that stop to end, at most
 * `HOST_STOP_GRACE_MS`. An exit in the middle of the stop would cut pi-envoy's role release and
 * its DELETE short, and the session's role claims would stay held until the listener's reaper
 * finds the holder gone. When no stop has begun, it exits at once, inside this call: an exit that
 * throws (a test's stand-in) throws to the caller.
 *
 * In Oh My Pi the host ends the process itself when its stop has ended (143 after a SIGTERM). So
 * this exit runs after the wait only when the process still lives then, or at the bound.
 */
export function exitAfterHostStop(host: HostStopExit, code: number): Promise<void> {
  const stop = host.hostStop();
  if (stop === undefined) return host.exitProcess(code);
  const bound = Promise.withResolvers<void>();
  const timer = setTimeout(bound.resolve, HOST_STOP_GRACE_MS);
  return Promise.race([stop, bound.promise]).then(() => {
    clearTimeout(timer);
    host.exitProcess(code);
  });
}
