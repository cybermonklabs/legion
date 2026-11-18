/**
 * The watchdog over a heartbeat that stopped (R4).
 *
 * A session registers with the listener at its start and at every heartbeat tick, and a
 * registration the listener accepts is what keeps the session live there: Envoy's session bucket
 * forgets a session about 5 minutes after its last one. The heartbeat is armed once per process
 * (`ensureHeartbeat` in `../extensions/envoy.ts`), so a heartbeat timer that stops leaves the
 * process running and unregistered for as long as it lives, and every message published to its
 * role goes to a session Envoy no longer lists.
 *
 * This decides when to restart that heartbeat. It acts only where the heartbeat itself cannot:
 *
 *  - While registrations are accepted, it does nothing.
 *  - While the heartbeat ticks, it does nothing, however long the listener refuses: a failed tick
 *    already buys the one extra call its interval allows (`armRetry`), and a second caller would
 *    make every session of a fleet ask again at once through one listener outage.
 *  - With no accepted registration and no tick for a whole window, the timer has stopped: the
 *    heartbeat is armed again and registers at once. Its try is the one extra attempt that
 *    interval allows.
 *
 * The window is 5 minutes, Envoy's live window; a session's heartbeat gaps measured in production
 * are 137 s at the 99th percentile and 1,148 s at most. A shorter trigger would restart heartbeats
 * that are only slow.
 *
 * Its retries back off to at most a minute, so no session asks the listener faster than that. A
 * registration the listener accepts rests it until that registration is a window old, so a session
 * whose host timers are gone for good still registers, about every 5 minutes, at the edge of
 * Envoy's live window, rather than once. A tick of the heartbeat it restarted ends the retries: the
 * session's own cadence is back.
 */

/** How long a session may run with no accepted registration and no heartbeat tick before its
 *  heartbeat is armed again: Envoy's live window, after which its session bucket forgets it. */
export const REGISTRATION_WATCHDOG_MS = 300_000;

/** The longest a retry of this watchdog waits before it tries again. */
export const WATCHDOG_MAX_BACKOFF_MS = 60_000;

/** How often the watchdog reads its window: a tenth of it, and 30 s at most, so a 5-minute
 *  window is read ten times inside it and a window a test shortens is read inside that. */
export function watchdogCheckMs(windowMs: number): number {
  return Math.max(1, Math.min(Math.floor(windowMs / 10), 30_000));
}

/** What `RegistrationWatchdog` asks of the extension around it. */
export interface RegistrationWatchdogDeps {
  readonly now: () => number;
  /** The window above, read afresh so a session's own setting decides it. */
  readonly windowMs: () => number;
  /** Whether the live session's last accepted registration is younger than `ms`
   *  (`RegistrationState.registrationFresherThan`). */
  readonly registrationFresherThan: (ms: number) => boolean;
  /** When the heartbeat's tick last ran; undefined while no heartbeat is armed. */
  readonly lastTickAt: () => number | undefined;
  /** Arms the heartbeat again and registers at once, keeping the same Envoy session. */
  readonly restart: () => void;
}

export class RegistrationWatchdog {
  readonly #deps: RegistrationWatchdogDeps;
  /** When this watchdog last restarted the heartbeat; undefined while it never has. */
  #actedAt: number | undefined;
  /** How long the next retry waits after the one before it. */
  #backoffMs = 0;

  constructor(deps: RegistrationWatchdogDeps) {
    this.#deps = deps;
  }

  /** One reading of the window; called on the watchdog's own timer. */
  check(): void {
    const { now, windowMs, registrationFresherThan, lastTickAt, restart } = this.#deps;
    const tickedAt = lastTickAt();
    // No heartbeat is armed yet: the session's start arms one, and a session that reached no
    // listener at all is told so by its start rather than by this.
    if (tickedAt === undefined) return;
    const window = windowMs();
    if (registrationFresherThan(window)) {
      this.#rest();
      return;
    }
    const at = now();
    // A tick since this watchdog last acted, inside the window: the heartbeat runs, and its own
    // retry is the one extra attempt the interval allows.
    if (at - tickedAt < window && tickedAt >= (this.#actedAt ?? 0)) {
      this.#rest();
      return;
    }
    if (this.#actedAt !== undefined && at - this.#actedAt < this.#backoffMs) return;
    this.#actedAt = at;
    this.#backoffMs =
      this.#backoffMs === 0
        ? watchdogCheckMs(window)
        : Math.min(this.#backoffMs * 2, WATCHDOG_MAX_BACKOFF_MS);
    restart();
  }

  /** Back to the start: the next end this session meets gets the first delay again. */
  #rest(): void {
    this.#actedAt = undefined;
    this.#backoffMs = 0;
  }
}
