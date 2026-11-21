import { describe, expect, test } from "bun:test";
import {
  REGISTRATION_WATCHDOG_MS,
  RegistrationWatchdog,
  WATCHDOG_MAX_BACKOFF_MS,
  watchdogCheckMs,
} from "./registration-watchdog";

/** A watchdog over a clock and a heartbeat the case drives by hand. */
function watchdog(options: { readonly windowMs?: number } = {}) {
  const windowMs = options.windowMs ?? REGISTRATION_WATCHDOG_MS;
  const state = {
    now: 0,
    /** When the heartbeat last ticked; undefined while none is armed. */
    tickedAt: undefined as number | undefined,
    /** When the last registration this session made was accepted. */
    registeredAt: undefined as number | undefined,
    restarts: [] as number[],
  };
  const dog = new RegistrationWatchdog({
    now: () => state.now,
    windowMs: () => windowMs,
    registrationFresherThan: (ms) =>
      state.registeredAt !== undefined && state.now - state.registeredAt < ms,
    lastTickAt: () => state.tickedAt,
    restart: () => {
      state.restarts.push(state.now);
    },
  });
  return {
    state,
    /** Moves the clock on and runs every check the watchdog's own timer would run. */
    run: (ms: number) => {
      const step = watchdogCheckMs(windowMs);
      for (let moved = 0; moved < ms; moved += step) {
        state.now += step;
        dog.check();
      }
    },
    check: () => dog.check(),
  };
}

describe("the watchdog over a heartbeat that stopped", () => {
  test("acts after the window with no accepted registration and no tick", () => {
    const { state, run } = watchdog();
    state.tickedAt = 0;
    state.registeredAt = 0;

    run(REGISTRATION_WATCHDOG_MS - watchdogCheckMs(REGISTRATION_WATCHDOG_MS));
    expect(state.restarts).toEqual([]);

    run(watchdogCheckMs(REGISTRATION_WATCHDOG_MS));
    expect(state.restarts).toHaveLength(1);
  });

  test("a heartbeat that still ticks is left alone, however long the listener refuses", () => {
    const { state, run } = watchdog();
    state.tickedAt = 0;
    state.registeredAt = 0;
    // The listener accepts nothing from here on, and the heartbeat ticks through it: its own one
    // extra call per interval is the retry, and the watchdog adds none.
    for (let elapsed = 0; elapsed < 30 * 60_000; elapsed += 90_000) {
      run(90_000);
      state.tickedAt = state.now;
    }
    expect(state.restarts).toEqual([]);
  });

  test("a session whose registrations are accepted is left alone, ticks or not", () => {
    const { state, run } = watchdog();
    state.tickedAt = 0;
    state.registeredAt = 0;
    for (let elapsed = 0; elapsed < 30 * 60_000; elapsed += 60_000) {
      run(60_000);
      state.registeredAt = state.now;
    }
    expect(state.restarts).toEqual([]);
  });

  test("a heartbeat that was never armed is left alone", () => {
    const { state, run } = watchdog();
    run(30 * 60_000);
    expect(state.restarts).toEqual([]);
  });

  test("its retries back off to at most a minute between tries", () => {
    const { state, run } = watchdog();
    state.tickedAt = 0;
    state.registeredAt = 0;
    // Nothing recovers: no registration is accepted and no restarted heartbeat ever ticks.
    run(REGISTRATION_WATCHDOG_MS + 20 * 60_000);

    expect(state.restarts.length).toBeGreaterThan(5);
    const gaps = state.restarts.slice(1).map((at, index) => at - (state.restarts[index] ?? 0));
    expect(Math.max(...gaps)).toBeLessThanOrEqual(WATCHDOG_MAX_BACKOFF_MS);
    // The ladder reaches the cap rather than staying at the first delay for ever.
    expect(Math.max(...gaps)).toBe(WATCHDOG_MAX_BACKOFF_MS);
    expect(Math.min(...gaps)).toBeGreaterThan(0);
  });

  test("a tick of the heartbeat it restarted ends its retries", () => {
    const { state, run } = watchdog();
    state.tickedAt = 0;
    state.registeredAt = 0;
    run(REGISTRATION_WATCHDOG_MS);
    expect(state.restarts).toHaveLength(1);

    // The restarted heartbeat ticks from here on: the session's own cadence is back, and its
    // failed ticks buy the one extra call the interval allows, however long the listener refuses.
    for (let elapsed = 0; elapsed < 10 * 60_000; elapsed += 90_000) {
      state.tickedAt = state.now;
      run(90_000);
    }
    expect(state.restarts).toHaveLength(1);
  });

  test("a stopped heartbeat under a short window acts inside it", () => {
    const windowMs = 200;
    const { state, run } = watchdog({ windowMs });
    state.tickedAt = 0;
    state.registeredAt = 0;
    run(windowMs);
    expect(state.restarts).toHaveLength(1);
    expect(state.restarts[0]).toBeLessThanOrEqual(windowMs + watchdogCheckMs(windowMs));
  });
});
