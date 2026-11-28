// The rows on the registration a session's heartbeat sends: the refresh that goes on while a
// regain hook, a pass, a role move or a whole stall does not settle, and the tmux place each
// registration carries. They have a file of their own, which holds the rows of this subject and
// stays under 1,000 lines. Their `describe` is "envoy OMP extension", the one `envoy.test.ts`
// uses, so a row's full name is the same in every file. Both files share one harness
// (`../test/extension-harness`).
import { describe, expect, jest, setSystemTime, test } from "bun:test";
import type { ExecFn } from "@legion/envoy-client/dispatch-cwd";
import type { TmuxPlace, TmuxPlaceReading } from "@legion/envoy-client/tmux-place";
import { onEnvoyRoleRegained } from "@legion/pi-shared/role-claim-bridge";
import { until } from "@legion/test-fixtures/wait";
import { logger } from "@oh-my-pi/pi-utils";
import type { EnvoyModule } from "../test/envoy-module";
import {
  createPi,
  realReadTmuxPlace,
  sessionContext,
  settle,
  tmuxPlaceState,
  useExtensionHarness,
  withEnvoyTimings,
} from "../test/extension-harness";
import { response, responseWithRegistration, standInFetch } from "../test/listener-stand-in";
import type { SessionContext } from "../test/stand-in-host";

useExtensionHarness();

describe("envoy OMP extension", () => {
  test("a regain hook that never settles does not block the next heartbeat tick's registration", async () => {
    // The hook is legion.ts's daemon round-trip (`claims/ready`, retried on an outage or a
    // transport failure), which this side cannot bound. The heartbeat's healing latch must
    // release once registration and the claim are settled, or a stuck hook would stop
    // re-registration and let the session's registry entry lapse.
    const role = "legion-controller";
    let listenerHoldsClaim = true;
    let registrations = 0;
    const thirdRegistration = Promise.withResolvers<void>();
    globalThis.fetch = standInFetch(async (input, init) => {
      const url = new URL(input.toString());
      if (url.pathname === `/v1/roles/${role}`) {
        if (!listenerHoldsClaim) {
          return Response.json({ error: `no holder for role ${role}` }, { status: 404 });
        }
        return response({ role, holder: "ses_hung_hook", last_seen: 1 });
      }
      if (url.pathname === "/v1/roles/set") {
        listenerHoldsClaim = true;
        return response({
          session_id: "ses_hung_hook",
          machine_id: "test",
          dir: "/tmp/envoy-omp-test",
          topics: [`notifications.role.${role}`],
        });
      }
      if (url.pathname === "/v1/interests/subscribe") {
        registrations += 1;
        if (registrations === 3) thirdRegistration.resolve();
      }
      return responseWithRegistration(input, init, {});
    });
    const { default: envoyExtension }: EnvoyModule = await import("./envoy.ts?heartbeat-hung-hook");
    let hookCalls = 0;
    onEnvoyRoleRegained(async () => {
      hookCalls += 1;
      await Promise.withResolvers<void>().promise;
    });
    const fixture = createPi();
    const intervals: (() => void)[] = [];
    const notifications: string[] = [];
    const context: SessionContext = {
      ...sessionContext("ses_hung_hook"),
      setInterval: (callback) => intervals.push(callback),
      ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
    };
    await envoyExtension(fixture.pi);
    await fixture.handlers.get("session_start")?.({}, context);
    await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
    expect(registrations).toBe(1);

    listenerHoldsClaim = false;
    intervals[0]?.();
    await until(
      "the regain hook and the tick's registration",
      () => hookCalls === 1 && registrations === 2
    );
    expect(hookCalls).toBe(1);
    expect(registrations).toBe(2);

    // The hook is still pending. The next tick must still register.
    intervals[0]?.();
    await thirdRegistration.promise;
    expect(registrations).toBe(3);
    expect(hookCalls).toBe(1);
    expect(notifications).toEqual([]);
  });

  test("a pass stuck on a listener call that never answers still lets later ticks refresh the registration", async () => {
    // The registration refresh is the last step of a pass. A listener socket that neither answers
    // nor errors used to hold every later pass behind it for the life of the process: the pane's
    // registry record then lapsed while it ran on, and every other agent was told there was no
    // live session for it. The refresh has to keep going, tick after tick, not recover once.
    // The ticks are hand-driven, and the age the refresh is decided on is read from the real
    // clock (`Date.now() - registeredAt`), so a tick inside that interval posts nothing: the row
    // waits on the registration reaching the listener, and ticks again until it does, rather than
    // reading the count after a sleep a loaded machine outruns.
    await withEnvoyTimings({ heartbeatMs: 25, passDeadlineMs: 5000 }, async () => {
      let wedgePass = false;
      const wedged = Promise.withResolvers<void>();
      const answered = Promise.withResolvers<void>();
      let registrations = 0;
      globalThis.fetch = standInFetch(async (input, init) => {
        if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
          // The call the pass awaits, as a socket that is accepted and never answered leaves it:
          // nothing resolves it, and the request's abort signal goes unread.
          if (wedgePass) {
            wedgePass = false;
            wedged.resolve();
            await answered.promise;
          } else registrations += 1;
        }
        return responseWithRegistration(input, init, {});
      });
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?heartbeat-wedged-pass"
      );
      const fixture = createPi();
      const intervals: (() => void)[] = [];
      const context: SessionContext = {
        ...sessionContext("ses_wedged_pass"),
        setInterval: (callback) => intervals.push(callback),
      };
      /**
       * Ticks until the listener has accepted one more registration, and answers with the count it
       * reached. A tick that lands inside the refresh interval posts nothing, so the row ticks
       * again rather than deciding on one tick, and a refresh is awaited where it shows — at the
       * listener.
       */
      const refreshed = (): Promise<number> => {
        const seen = registrations;
        return until("the listener to accept one more registration", () => {
          if (registrations > seen) return registrations;
          intervals[0]?.();
          return undefined;
        });
      };
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);
      expect(registrations).toBe(1);

      wedgePass = true;
      intervals[0]?.();
      // The pass is inside that call, so its own registration never reached the listener.
      await wedged.promise;
      expect(registrations).toBe(1);

      // A refresh interval with nothing accepted: the tick posts the registration itself.
      const second = await refreshed();
      expect(second).toBeGreaterThan(1);

      // And the ticks after it do too: the refresh resumes, it does not recover once.
      expect(await refreshed()).toBeGreaterThan(second);

      // Answer at last, so the pass settles and leaves no deadline timer behind.
      answered.resolve();
      for (let step = 0; step < 20; step += 1) await new Promise((r) => setImmediate(r));
    });
  });

  test("a role move that never settles still lets the next tick refresh the registration", async () => {
    // What a wedged envoy_role_set or a wedged Legion claim leaves behind: the role-move queue is
    // held, and the heartbeat's pass awaits its own move inside it. Without the move's deadline
    // and the tick's own refresh, the session's registry record lapses while the pane runs on,
    // and the listener reaps the very role claim the move was about.
    // Real waits between hand-driven ticks: the refresh is decided on a real-clock age.
    await withEnvoyTimings({ heartbeatMs: 25, passDeadlineMs: 5000 }, async () => {
      const role = "legion-controller";
      let wedgeRoleRead = false;
      const answered = Promise.withResolvers<void>();
      let registrations = 0;
      globalThis.fetch = standInFetch(async (input, init) => {
        const url = new URL(input.toString());
        if (url.pathname === `/v1/roles/${role}`) {
          if (wedgeRoleRead) {
            wedgeRoleRead = false;
            await answered.promise;
          }
          return response({ role, holder: "ses_wedged_move", last_seen: 1 });
        }
        if (url.pathname === "/v1/roles/set") {
          return response({
            session_id: "ses_wedged_move",
            machine_id: "test",
            dir: "/tmp/envoy-omp-test",
            topics: [`notifications.role.${role}`],
          });
        }
        if (url.pathname === "/v1/interests/subscribe") registrations += 1;
        return responseWithRegistration(input, init, {});
      });
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?heartbeat-wedged-move"
      );
      const fixture = createPi();
      const intervals: (() => void)[] = [];
      const context: SessionContext = {
        ...sessionContext("ses_wedged_move"),
        setInterval: (callback) => intervals.push(callback),
      };
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);
      await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
      expect(registrations).toBe(1);

      // This tick registers, then parks in the role move its pass awaits.
      wedgeRoleRead = true;
      intervals[0]?.();
      await until(
        "the tick's registration, before its pass parks in the role move",
        () => registrations === 2
      );
      expect(registrations).toBe(2);

      // The move is still in flight, and a tick past the refresh interval refreshes the
      // registration regardless.
      await until("a tick to refresh the registration", () => {
        if (registrations > 2) return true;
        intervals[0]?.();
        return false;
      });
      expect(registrations).toBe(3);

      answered.resolve();
      for (let step = 0; step < 20; step += 1) await new Promise((r) => setImmediate(r));
    });
  });

  test("a whole stall says so once in the pane and once in the log, and a healthy tick says nothing", async () => {
    // A stall lasts as long as the wedged call does, and every tick inside it meets the same
    // symptom. One line per episode is what a registry outage gets; a line per tick would bury
    // the pane and the log under a notice the agent can do nothing about.
    // The refresh is decided on a real-clock age, so a stall's tick posts once that age passes the
    // interval: the row ticks until each refresh reaches the listener.
    await withEnvoyTimings({ heartbeatMs: 25, passDeadlineMs: 5000 }, async () => {
      let wedgePass = false;
      let registrations = 0;
      const answered = Promise.withResolvers<void>();
      globalThis.fetch = standInFetch(async (input, init) => {
        if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
          registrations += 1;
          if (wedgePass) {
            wedgePass = false;
            await answered.promise;
          }
        }
        return responseWithRegistration(input, init, {});
      });
      const { default: envoyExtension }: EnvoyModule = await import(
        "./envoy.ts?heartbeat-stall-said-once"
      );
      const warnings: string[] = [];
      const stopSink = logger.registerLogSink((entry) => {
        if (entry.level === "warn") warnings.push(entry.message);
      });
      try {
        const fixture = createPi();
        const intervals: (() => void)[] = [];
        const notifications: string[] = [];
        const context: SessionContext = {
          ...sessionContext("ses_stall_once"),
          setInterval: (callback) => intervals.push(callback),
          ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
        };
        await envoyExtension(fixture.pi);
        await fixture.handlers.get("session_start")?.({}, context);

        // A healthy tick before any stall is silent.
        intervals[0]?.();
        await until("the healthy tick's registration", () => registrations === 2);
        await settle();
        expect(notifications).toEqual([]);

        wedgePass = true;
        intervals[0]?.();
        await until("the registration the stalled pass parks in", () => registrations === 3);
        // Three refreshes of the one stall, each a tick past the refresh interval posting the
        // registration on its own.
        for (let refresh = 1; refresh <= 3; refresh += 1) {
          await until(`the stall's refresh ${refresh}`, () => {
            if (registrations >= 3 + refresh) return true;
            intervals[0]?.();
            return false;
          });
        }
        expect(notifications.filter((message) => message.includes("has stalled"))).toHaveLength(1);
        expect(notifications).toHaveLength(1);
        expect(
          warnings.filter(
            (message) =>
              message === "envoy: reconcile pass stalled; registration refreshed on its own"
          )
        ).toHaveLength(1);

        // The pass settles: the episode is over, and the tick after it is silent again.
        answered.resolve();
        for (let step = 0; step < 20; step += 1) await new Promise((r) => setImmediate(r));
        const beforeHealthy = registrations;
        intervals[0]?.();
        await until(
          "the healthy tick's registration after the stall",
          () => registrations > beforeHealthy
        );
        await settle();
        expect(notifications).toHaveLength(1);
        expect(
          warnings.filter(
            (message) =>
              message === "envoy: reconcile pass stalled; registration refreshed on its own"
          )
        ).toHaveLength(1);
      } finally {
        stopSink();
      }
    });
  });

  describe("the tmux place a registration carries", () => {
    const PLACE: TmuxPlace = {
      socket: "/tmp/tmux-1000/default",
      server_pid: "4242",
      session_name: "legion",
      window_id: "@3",
      window_index: 2,
      window_name: "infra-883",
      pane_id: "%7",
      pane_index: 1,
      layout: "a1b2,80x24,0,0,7",
      zoomed: false,
    };
    /** The same session after someone moved its pane to a window of its own. */
    const MOVED: TmuxPlace = { ...PLACE, window_id: "@9", window_index: 5, window_name: "moved" };

    /**
     * Boots one session against a listener that records the `tmux_place` of every registration it
     * answers, and hands back those places — `undefined` for a registration that carried none —
     * beside the heartbeat tick that sends the next one.
     */
    async function bootPlacedSession(query: string, sessionID: string) {
      const places: (TmuxPlace | undefined)[] = [];
      let registered: (() => void) | undefined;
      globalThis.fetch = standInFetch(async (input, init) => {
        const url = new URL(input.toString());
        if (url.pathname === "/v1/interests/subscribe" && init?.body !== undefined) {
          places.push(JSON.parse(String(init?.body)).tmux_place);
          registered?.();
        }
        return responseWithRegistration(input, init, {});
      });
      const { default: envoyExtension }: EnvoyModule = await import(`./envoy.ts?${query}`);
      const fixture = createPi();
      const heartbeats: (() => void)[] = [];
      const context: SessionContext = {
        ...sessionContext(sessionID),
        setInterval: (callback) => {
          heartbeats.push(callback);
        },
      };
      await envoyExtension(fixture.pi);
      await fixture.handlers.get("session_start")?.({}, context);
      return {
        places,
        /** The moment the session booted: the first registration's reading is stamped then. */
        bootedAt: Date.now(),
        /** Ticks every interval the session registered, and waits for the registration it sends. */
        heartbeat: async () => {
          const sent = Promise.withResolvers<void>();
          const before = places.length;
          registered = () => {
            if (places.length > before) sent.resolve();
          };
          for (const tick of heartbeats) tick();
          await sent.promise;
        },
      };
    }

    /** The lifetime these cases run the place cache at: the one a session runs at where nothing
     *  names another. What each case counts is the readings its registrations start.
     *
     *  The cache also reads tmux on a refresh timer of its own, armed by the session's first
     *  registration at the lifetime: on the real clock that timer read tmux once more whenever a
     *  slow run took a lifetime, and a case counted three readings where it starts two
     *  (dispatch://INFRA-49/comment/1e3be641-e6e4-4620-a415-c751bfd1c4db). So every case that
     *  crosses the lifetime runs on Bun's fake clock (`jest.useFakeTimers`, which the file's
     *  `afterEach` takes back), where that timer stands still unless the case advances it, which
     *  none does. */
    const PLACE_LIFETIME_MS = 30_000;

    /** The next registration a session sends once its cached reading has aged out. The case moves
     *  the clock the cache reads (`Date.now`, held on the fake clock) past the lifetime, which
     *  fires no timer, rather than waiting it out. Every crossing lands on the same instant, one
     *  lifetime past the boot, so a reading one crossing starts is still fresh at the next. The
     *  pass the previous tick started is let settle first, since a tick that finds one still
     *  holding the queue refreshes nothing while the registration it would refresh is fresh. */
    async function heartbeatPastTheLifetime(session: {
      bootedAt: number;
      heartbeat: () => Promise<void>;
    }) {
      await settle();
      setSystemTime(new Date(session.bootedAt + PLACE_LIFETIME_MS + 1_000));
      await session.heartbeat();
    }

    test("two registrations while one reading is away read tmux once", async () => {
      await withEnvoyTimings({ tmuxPlaceCacheMs: PLACE_LIFETIME_MS }, async () => {
        jest.useFakeTimers();
        tmuxPlaceState.reading = () => ({ kind: "place", place: PLACE });
        const session = await bootPlacedSession("tmux-place-one-flight", "ses_tmux_one_flight");
        // A reading that has not answered yet: the registration past the lifetime starts it, and
        // the one after that finds it still away. Two readings at once would have the slower of
        // them overwrite the newer answer with its older one.
        const reading = Promise.withResolvers<TmuxPlaceReading>();
        tmuxPlaceState.reading = () => reading.promise;
        await heartbeatPastTheLifetime(session);
        await heartbeatPastTheLifetime(session);

        // Two registrations, and the one reading they share, which neither waited for: both
        // carry the place the cache held while it was away.
        expect(tmuxPlaceState.calls).toHaveLength(2);
        expect(session.places).toEqual([undefined, PLACE, PLACE]);
        reading.resolve({ kind: "place", place: MOVED });
      });
    });

    test("the first registration waits on no reading, and the next carries its place", async () => {
      tmuxPlaceState.reading = () => ({ kind: "place", place: PLACE });
      const session = await bootPlacedSession("tmux-place-first", "ses_tmux_first");

      // The first registration starts the reading and goes out without it: a cold reading is a
      // tmux call of up to a second, and a registration that waited one out could reach the
      // listener behind its own session's shutdown (dispatch://INFRA-392). The reading answers
      // between registrations, and the next one carries it.
      expect(session.places).toEqual([undefined]);
      expect(tmuxPlaceState.calls).toHaveLength(1);
      await session.heartbeat();

      expect(session.places).toEqual([undefined, PLACE]);
      expect(tmuxPlaceState.calls).toHaveLength(1);
    });

    test("reads tmux once for every registration inside the cache lifetime", async () => {
      let next: TmuxPlaceReading = { kind: "place", place: PLACE };
      tmuxPlaceState.reading = () => next;
      const session = await bootPlacedSession("tmux-place-cached", "ses_tmux_cached");
      // A pane that moved while the reading is still fresh is not read again, and not reported.
      next = { kind: "place", place: MOVED };
      await session.heartbeat();

      expect(tmuxPlaceState.calls).toHaveLength(1);
      expect(session.places).toEqual([undefined, PLACE]);
    });

    test("reads tmux again for a registration past the cache lifetime", async () => {
      await withEnvoyTimings({ tmuxPlaceCacheMs: PLACE_LIFETIME_MS }, async () => {
        jest.useFakeTimers();
        let next: TmuxPlaceReading = { kind: "place", place: PLACE };
        tmuxPlaceState.reading = () => next;
        const session = await bootPlacedSession("tmux-place-expired", "ses_tmux_expired");
        next = { kind: "place", place: MOVED };
        // The registration past the lifetime starts the reading of the moved pane; the one after
        // it is the first to carry it, and reads nothing itself — both heartbeats cross the same
        // instant, so the reading in between is still fresh.
        await heartbeatPastTheLifetime(session);
        await heartbeatPastTheLifetime(session);

        expect(tmuxPlaceState.calls).toHaveLength(2);
        expect(session.places).toEqual([undefined, PLACE, MOVED]);
      });
    });

    test("a refused reading registers no place and clears the one it held", async () => {
      await withEnvoyTimings({ tmuxPlaceCacheMs: PLACE_LIFETIME_MS }, async () => {
        jest.useFakeTimers();
        let next: TmuxPlaceReading = { kind: "place", place: PLACE };
        tmuxPlaceState.reading = () => next;
        const session = await bootPlacedSession("tmux-place-refused", "ses_tmux_refused");
        // The session left its pane: the listener's entry must lose the place, not keep a stale one.
        next = { kind: "refused" };
        await heartbeatPastTheLifetime(session);
        await heartbeatPastTheLifetime(session);

        expect(tmuxPlaceState.calls).toHaveLength(2);
        expect(session.places).toEqual([undefined, PLACE, undefined]);
      });
    });

    test("a timed-out reading registers the last place read", async () => {
      await withEnvoyTimings({ tmuxPlaceCacheMs: PLACE_LIFETIME_MS }, async () => {
        jest.useFakeTimers();
        let next: TmuxPlaceReading = { kind: "place", place: PLACE };
        tmuxPlaceState.reading = () => next;
        const session = await bootPlacedSession("tmux-place-timeout", "ses_tmux_timeout");
        // A server too loaded to answer in time must not take a live session off the layout.
        next = { kind: "timedOut" };
        await heartbeatPastTheLifetime(session);
        await heartbeatPastTheLifetime(session);

        expect(tmuxPlaceState.calls).toHaveLength(2);
        expect(session.places).toEqual([undefined, PLACE, PLACE]);
      });
    });

    test("a reading that throws still registers the session, without a place", async () => {
      tmuxPlaceState.reading = () => {
        throw new Error("the tmux reader broke");
      };
      const session = await bootPlacedSession("tmux-place-throws", "ses_tmux_throws");
      await session.heartbeat();

      expect(session.places).toEqual([undefined, undefined]);
    });

    test("a session with neither TMUX nor TMUX_PANE runs no tmux call at all", async () => {
      const calls: string[] = [];
      const exec: ExecFn = async (file, args) => {
        calls.push([file, ...args].join(" "));
        return { stdout: "" };
      };
      // The real reader, over an exec that records: what decides there is nothing to ask is the
      // reader itself, and every session in a Kubernetes pod is this one.
      tmuxPlaceState.reading = (env) => realReadTmuxPlace(env, exec);
      const session = await bootPlacedSession("tmux-place-outside", "ses_tmux_outside");

      expect(tmuxPlaceState.calls).toHaveLength(1);
      expect(tmuxPlaceState.calls[0]?.env.TMUX).toBeUndefined();
      expect(tmuxPlaceState.calls[0]?.env.TMUX_PANE).toBeUndefined();
      expect(calls).toEqual([]);
      expect(session.places).toEqual([undefined]);
    });
  });
});
