// The rows on the registration a session's heartbeat sends: the refresh that goes on while a
// regain hook, a pass, a role move or a whole stall does not settle, and the tmux place each
// registration carries. They have a file of their own, which holds the rows of this subject and
// stays under 1,000 lines. Their `describe` is "envoy OMP extension", the one `envoy.test.ts`
// uses, so a row's full name is the same in every file. Both files share one harness
// (`../test/extension-harness`).
import { describe, expect, jest } from "bun:test";
import type { ExecFn } from "@legion/envoy-client/dispatch-cwd";
import type { TmuxPlace, TmuxPlaceReading } from "@legion/envoy-client/tmux-place";
import { onEnvoyRoleRegained } from "@legion/pi-shared/role-claim-bridge";
import { settled } from "@legion/test-fixtures/wait";
import type { EnvoyModule } from "../test/envoy-module";
import {
  createPi,
  heartbeatTicks,
  onFakeTimers,
  realReadTmuxPlace,
  rowLogSink,
  rowSessionShutdown,
  sessionContext,
  settle,
  tmuxPlaceState,
  UNRACED_PASS_DEADLINE_MS,
  useExtensionHarness,
  withEnvoyTimings,
} from "../test/extension-harness";
import {
  response,
  responseWithRegistration,
  rowCheck,
  stillOn,
  test,
} from "../test/listener-stand-in";
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
    globalThis.fetch = async (input, init) => {
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
    };
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
    await new Promise((resolve) => setTimeout(resolve, 10));
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
    // An hour, past the command's guard, so no deadline ends a pass (`UNRACED_PASS_DEADLINE_MS`).
    // The wedged pass then holds the queue until the row answers it, and only the tick's own
    // refresh can move the count meanwhile. At the 5 s deadline the row had before, the wedged
    // pass was abandoned on the real clock, and an ordinary pass behind it moved the count, so
    // the row passed with that refresh removed.
    await withEnvoyTimings(
      { heartbeatMs: 25, passDeadlineMs: UNRACED_PASS_DEADLINE_MS },
      async () => {
        let wedgePass = false;
        const wedged = Promise.withResolvers<void>();
        const answered = Promise.withResolvers<void>();
        const intervals: (() => void)[] = [];
        const { accept, registrations, refreshed } = heartbeatTicks(intervals);
        globalThis.fetch = async (input, init) => {
          if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
            // The call the pass awaits, as a socket that is accepted and never answered leaves it:
            // nothing resolves it, and the request's abort signal goes unread.
            if (wedgePass) {
              wedgePass = false;
              wedged.resolve();
              await answered.promise;
            } else {
              accept();
            }
          }
          return responseWithRegistration(input, init, {});
        };
        const { default: envoyExtension }: EnvoyModule = await import(
          "./envoy.ts?heartbeat-wedged-pass"
        );
        const fixture = createPi();
        const context: SessionContext = {
          ...sessionContext("ses_wedged_pass"),
          setInterval: (callback) => intervals.push(callback),
        };
        await envoyExtension(fixture.pi);
        await fixture.handlers.get("session_start")?.({}, context);
        expect(registrations()).toBe(1);

        wedgePass = true;
        intervals[0]?.();
        // The pass is inside that call, so its own registration never reached the listener.
        await settled(
          "the tick's pass to reach the registration call it wedges in",
          wedged.promise
        );
        expect(registrations()).toBe(1);

        // A refresh interval with nothing accepted: the tick posts the registration itself.
        const second = await refreshed();
        expect(second).toBeGreaterThan(1);

        // And the ticks after it do too: the refresh resumes, it does not recover once.
        expect(await refreshed()).toBeGreaterThan(second);

        // Answer at last, so the pass settles and leaves no deadline timer behind.
        answered.resolve();
        for (let step = 0; step < 20; step += 1) await new Promise((r) => setImmediate(r));
      }
    );
    // What can hang is a refresh that never comes (`refreshed`), which a tick with no refresh of
    // its own leaves for good, since the wedged pass outlasts the row.
  });

  test("a role move that never settles still lets the next tick refresh the registration", async () => {
    // What a wedged envoy_role_set or a wedged Legion claim leaves behind: the role-move queue is
    // held, and the heartbeat's pass awaits its own move inside it. Without the move's deadline
    // and the tick's own refresh, the session's registry record lapses while the pane runs on,
    // and the listener reaps the very role claim the move was about.
    // The ticks are hand-driven. The tick that starts the pass is awaited until the pass's
    // registration reaches the listener, and then until its move is parked at the listener. The
    // refresh after it is decided on the registration's age on the real clock, so a tick inside
    // the 25 ms interval posts nothing: the row ticks again until the refresh shows at the
    // listener (`heartbeatTicks`), rather than reading the count after a sleep a loaded machine
    // outruns.
    // An hour, past the command's guard, so no deadline ends the pass or its move
    // (`UNRACED_PASS_DEADLINE_MS`). The move then stays parked until the row answers it, and only
    // the tick's own refresh can move the count meanwhile. At the 5 s deadline the row had
    // before, a loaded host could reach that deadline. The move was then abandoned, and an
    // ordinary pass behind it could move the count in place of the tick.
    await withEnvoyTimings(
      { heartbeatMs: 25, passDeadlineMs: UNRACED_PASS_DEADLINE_MS },
      async () => {
        const role = "legion-controller";
        let wedgeRoleRead = false;
        const parked = Promise.withResolvers<void>();
        const answered = Promise.withResolvers<void>();
        const intervals: (() => void)[] = [];
        const ticks = heartbeatTicks(intervals);
        globalThis.fetch = async (input, init) => {
          const url = new URL(input.toString());
          if (url.pathname === `/v1/roles/${role}`) {
            if (wedgeRoleRead) {
              // The move's read of its role, as a listener that never answers leaves it.
              wedgeRoleRead = false;
              parked.resolve();
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
          if (url.pathname === "/v1/interests/subscribe") ticks.accept();
          return responseWithRegistration(input, init, {});
        };
        const { default: envoyExtension }: EnvoyModule = await import(
          "./envoy.ts?heartbeat-wedged-move"
        );
        const fixture = createPi();
        const context: SessionContext = {
          ...sessionContext("ses_wedged_move"),
          setInterval: (callback) => intervals.push(callback),
        };
        await envoyExtension(fixture.pi);
        await fixture.handlers.get("session_start")?.({}, context);
        await fixture.tools.find((tool) => tool.name === "envoy_role_set")?.execute("", { role });
        expect(ticks.registrations()).toBe(1);

        // This tick's pass registers, then parks in the role move it awaits.
        wedgeRoleRead = true;
        expect(await ticks.registered()).toBe(2);
        await settled("the tick's role move to park in its read of the role", parked.promise);
        expect(ticks.registrations()).toBe(2);

        // The move is still in flight, and a tick refreshes the registration regardless.
        expect(await ticks.refreshed()).toBeGreaterThan(2);

        // Answer at last, so the move and its pass settle and leave no deadline timer behind.
        answered.resolve();
        for (let step = 0; step < 20; step += 1) await new Promise((r) => setImmediate(r));
      }
    );
    // What can hang is a refresh that never comes (`heartbeatTicks`), which a tick with no refresh
    // of its own leaves for good, since the parked move outlasts the row.
  });

  test("a whole stall says so once in the pane and once in the log, and a healthy tick says nothing", async () => {
    // A stall lasts as long as the wedged call does, and every tick inside it meets the same
    // symptom. One line per episode is what a registry outage gets; a line per tick would bury
    // the pane and the log under a notice the agent can do nothing about.
    // The ticks are hand-driven. A tick no pass is running at is awaited until its pass's
    // registration reaches the listener. Inside the stall, the refresh is decided on the
    // registration's age on the real clock, so a tick inside the 25 ms interval posts nothing:
    // the row ticks again until the refresh shows at the listener (`heartbeatTicks`). So each
    // refresh the row counts is a tick that met the stall, which a count read after a sleep a
    // loaded machine outruns could not show.
    // An hour, past the command's guard, so no deadline ends the wedged pass
    // (`UNRACED_PASS_DEADLINE_MS`): the stall lasts until the row answers it. At the 5 s deadline
    // the row had before, a loaded host could reach that deadline. The episode then ended early,
    // and an ordinary pass could refresh the registration in place of the tick.
    await withEnvoyTimings(
      { heartbeatMs: 25, passDeadlineMs: UNRACED_PASS_DEADLINE_MS },
      async () => {
        let wedgePass = false;
        const wedged = Promise.withResolvers<void>();
        const answered = Promise.withResolvers<void>();
        const intervals: (() => void)[] = [];
        const ticks = heartbeatTicks(intervals);
        globalThis.fetch = async (input, init) => {
          if (new URL(input.toString()).pathname === "/v1/interests/subscribe") {
            // The call the pass awaits, as a socket that is accepted and never answered leaves it.
            if (wedgePass) {
              wedgePass = false;
              wedged.resolve();
              await answered.promise;
            } else {
              ticks.accept();
            }
          }
          return responseWithRegistration(input, init, {});
        };
        const { default: envoyExtension }: EnvoyModule = await import(
          "./envoy.ts?heartbeat-stall-said-once"
        );
        const warnings: string[] = [];
        const stopSink = rowLogSink((entry) => {
          if (entry.level === "warn") warnings.push(entry.message);
        });
        try {
          const fixture = createPi();
          const notifications: string[] = [];
          const context: SessionContext = {
            ...sessionContext("ses_stall_once"),
            setInterval: (callback) => intervals.push(callback),
            ui: { ...sessionContext().ui, notify: (message) => notifications.push(message) },
          };
          await envoyExtension(fixture.pi);
          await fixture.handlers.get("session_start")?.({}, context);
          expect(ticks.registrations()).toBe(1);

          // A healthy tick before any stall is silent.
          expect(await ticks.registered()).toBe(2);
          expect(notifications).toEqual([]);
          // Its pass finishes before the wedge, so the tick that wedges starts a pass of its own.
          // What is left of the pass once the listener has its registration is promise steps of
          // the stand-ins, which wait on no clock, so the turns of `settle` finish it.
          await settle();

          wedgePass = true;
          intervals[0]?.();
          // The pass is inside that call, so its registration never reached the listener, and no
          // tick has met the stall yet.
          await settled(
            "the tick's pass to reach the registration call it wedges in",
            wedged.promise
          );
          expect(ticks.registrations()).toBe(2);
          expect(notifications).toEqual([]);

          // Three refreshes inside the one stall, each posted by a tick on its own. A tick says
          // the stall before it posts its refresh, so the notice is there once the refresh is.
          for (let round = 0; round < 3; round += 1) await ticks.refreshed();
          expect(notifications.filter((message) => message.includes("has stalled"))).toHaveLength(
            1
          );
          expect(notifications).toHaveLength(1);
          expect(
            warnings.filter(
              (message) =>
                message === "envoy: reconcile pass stalled; registration refreshed on its own"
            )
          ).toHaveLength(1);

          // The pass settles: the episode is over, and the tick after it is silent again. What is
          // left of the pass once its call answers waits on no clock either (`settle`), so the
          // tick after it starts a pass of its own, which `settle` lets finish too.
          answered.resolve();
          await settle();
          await ticks.registered();
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
      }
    );
    // What can hang is a registration that never reaches the listener (`heartbeatTicks`). That is
    // a refresh that never comes inside the stall, which lasts past the row, or a pass that never
    // registers. A pass that never reaches the wedged call hangs too (`wedged`).
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
     * beside the heartbeat tick that sends the next one and the session's shutdown, which stops
     * its place cache's refresh timer. Each case shuts its session down in a `finally`, and the
     * file's `afterEach` shuts down a session its row left running, a row its guard abandoned
     * included (`rowShutdown`): whichever comes first starts the shutdown, and the other answers
     * that same run.
     */
    async function bootPlacedSession(query: string, sessionID: string) {
      // The row the session is booted in: its heartbeat checks it first, so a body its guard
      // abandoned ticks nothing inside the row after it.
      const ownRow = rowCheck();
      const places: (TmuxPlace | undefined)[] = [];
      let registered: (() => void) | undefined;
      globalThis.fetch = async (input, init) => {
        const url = new URL(input.toString());
        if (url.pathname === "/v1/interests/subscribe" && init?.body !== undefined) {
          places.push(JSON.parse(init.body.toString()).tmux_place);
          registered?.();
        }
        return responseWithRegistration(input, init, {});
      };
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
      const shutDown = rowSessionShutdown(fixture, context);
      await fixture.handlers.get("session_start")?.({}, context);
      return {
        places,
        /** Ticks every interval the session registered, and waits for the registration it sends. */
        heartbeat: async () => {
          stillOn(ownRow, "ticking the placed session's heartbeat");
          const sent = Promise.withResolvers<void>();
          const before = places.length;
          registered = () => {
            if (places.length > before) sent.resolve();
          };
          for (const tick of heartbeats) tick();
          await sent.promise;
        },
        /** Shuts the session down, which stops its place cache's timer (`rowShutdown`). */
        shutDown,
      };
    }

    /** The lifetime these cases run the place cache at: the one a session runs at where nothing
     *  names another. The cache's own refresh timer reads tmux too, once a lifetime from the
     *  session's first registration until the session shuts down, which each case does at its
     *  end. What each case counts is the readings its registrations start, so a case that ran
     *  longer than the lifetime would count that timer's reading as well. */
    const PLACE_LIFETIME_MS = 30_000;

    /** Runs `body` with the place cache at `PLACE_LIFETIME_MS` and on the test's clock
     *  (`onFakeTimers`), for a case that crosses the lifetime. The cache measures the lifetime on
     *  `Date.now` and refreshes on a `setInterval` of its own (`createTmuxPlaceCache`), and the
     *  test's clock holds both, so the lifetime passes only when the case moves the clock
     *  (`heartbeatPastTheLifetime`), whatever the host's speed: on the real clock a slow host could
     *  pass it mid-case, and the refresh would add a reading. The clock is on before the session's
     *  first registration, which arms that timer. Nothing the case waits for needs the clock to
     *  move: the readings, the registrations and the shutdown are in-process here. */
    function atPlaceLifetime(body: () => Promise<void>): Promise<void> {
      return withEnvoyTimings({ tmuxPlaceCacheMs: PLACE_LIFETIME_MS }, () => onFakeTimers(body));
    }

    /** The next registration a session sends once its cached reading has aged out, crossed on
     *  the clock the cache reads (`Date.now`), which the test's clock holds (`atPlaceLifetime`):
     *  `jest.setSystemTime` moves it past the lifetime and runs no timer, and moves it back once
     *  the registration has gone, so each call crosses the same instant and the cache's refresh
     *  timer never runs. A real wait would cost every run the lifetime. The pass the previous tick
     *  started is let settle first, since a tick that finds one still holding the queue refreshes
     *  nothing while the registration it would refresh is fresh. */
    async function heartbeatPastTheLifetime(session: { heartbeat: () => Promise<void> }) {
      const ownRow = rowCheck();
      await settle();
      stillOn(ownRow, "moving the system time");
      const before = Date.now();
      jest.setSystemTime(before + PLACE_LIFETIME_MS + 1_000);
      try {
        await session.heartbeat();
      } finally {
        // Once the row has ended, the file's hooks have put the clock back for the row after it.
        if (ownRow()) jest.setSystemTime(before);
      }
    }

    test("two registrations while one reading is away read tmux once", async () => {
      await atPlaceLifetime(async () => {
        tmuxPlaceState.reading = () => ({ kind: "place", place: PLACE });
        const session = await bootPlacedSession("tmux-place-one-flight", "ses_tmux_one_flight");
        try {
          // A reading that has not answered yet: the registration past the lifetime starts it,
          // and the one after that finds it still away. Two readings at once would have the
          // slower of them overwrite the newer answer with its older one.
          const reading = Promise.withResolvers<TmuxPlaceReading>();
          tmuxPlaceState.reading = () => reading.promise;
          await heartbeatPastTheLifetime(session);
          await heartbeatPastTheLifetime(session);

          // Two registrations, and the one reading they share, which neither waited for: both
          // carry the place the cache held while it was away.
          expect(tmuxPlaceState.calls).toHaveLength(2);
          expect(session.places).toEqual([undefined, PLACE, PLACE]);
          reading.resolve({ kind: "place", place: MOVED });
        } finally {
          await session.shutDown();
        }
      });
      // What can hang is the boot, a heartbeat's registration or the shutdown, should one wait on
      // a timer, which the test's clock never runs here; the command's guard ends it.
    });

    test("the first registration waits on no reading, and the next carries its place", async () => {
      tmuxPlaceState.reading = () => ({ kind: "place", place: PLACE });
      const session = await bootPlacedSession("tmux-place-first", "ses_tmux_first");
      try {
        // The first registration starts the reading and goes out without it: a cold reading is a
        // tmux call of up to a second, and a registration that waited one out could reach the
        // listener behind its own session's shutdown (dispatch://INFRA-392). The reading answers
        // between registrations, and the next one carries it.
        expect(session.places).toEqual([undefined]);
        expect(tmuxPlaceState.calls).toHaveLength(1);
        await session.heartbeat();

        expect(session.places).toEqual([undefined, PLACE]);
        expect(tmuxPlaceState.calls).toHaveLength(1);
      } finally {
        await session.shutDown();
      }
    });

    test("reads tmux once for every registration inside the cache lifetime", async () => {
      let next: TmuxPlaceReading = { kind: "place", place: PLACE };
      tmuxPlaceState.reading = () => next;
      const session = await bootPlacedSession("tmux-place-cached", "ses_tmux_cached");
      try {
        // A pane that moved while the reading is still fresh is not read again, and not reported.
        next = { kind: "place", place: MOVED };
        await session.heartbeat();

        expect(tmuxPlaceState.calls).toHaveLength(1);
        expect(session.places).toEqual([undefined, PLACE]);
      } finally {
        await session.shutDown();
      }
    });

    test("reads tmux again for a registration past the cache lifetime", async () => {
      await atPlaceLifetime(async () => {
        let next: TmuxPlaceReading = { kind: "place", place: PLACE };
        tmuxPlaceState.reading = () => next;
        const session = await bootPlacedSession("tmux-place-expired", "ses_tmux_expired");
        try {
          next = { kind: "place", place: MOVED };
          // The registration past the lifetime starts the reading of the moved pane; the one
          // after it is the first to carry it, and reads nothing itself — both heartbeats cross
          // the same instant, so the reading in between is still fresh.
          await heartbeatPastTheLifetime(session);
          await heartbeatPastTheLifetime(session);

          expect(tmuxPlaceState.calls).toHaveLength(2);
          expect(session.places).toEqual([undefined, PLACE, MOVED]);
        } finally {
          await session.shutDown();
        }
      });
      // What can hang is the boot, a heartbeat's registration or the shutdown, should one wait on
      // a timer, which the test's clock never runs here; the command's guard ends it.
    });

    test("a refused reading registers no place and clears the one it held", async () => {
      await atPlaceLifetime(async () => {
        let next: TmuxPlaceReading = { kind: "place", place: PLACE };
        tmuxPlaceState.reading = () => next;
        const session = await bootPlacedSession("tmux-place-refused", "ses_tmux_refused");
        try {
          // The session left its pane: the listener's entry must lose the place, not keep a
          // stale one.
          next = { kind: "refused" };
          await heartbeatPastTheLifetime(session);
          await heartbeatPastTheLifetime(session);

          expect(session.places).toEqual([undefined, PLACE, undefined]);
        } finally {
          await session.shutDown();
        }
      });
      // What can hang is the boot, a heartbeat's registration or the shutdown, should one wait on
      // a timer, which the test's clock never runs here; the command's guard ends it.
    });

    test("a timed-out reading registers the last place read", async () => {
      await atPlaceLifetime(async () => {
        let next: TmuxPlaceReading = { kind: "place", place: PLACE };
        tmuxPlaceState.reading = () => next;
        const session = await bootPlacedSession("tmux-place-timeout", "ses_tmux_timeout");
        try {
          // A server too loaded to answer in time must not take a live session off the layout.
          next = { kind: "timedOut" };
          await heartbeatPastTheLifetime(session);
          await heartbeatPastTheLifetime(session);

          expect(tmuxPlaceState.calls).toHaveLength(2);
          expect(session.places).toEqual([undefined, PLACE, PLACE]);
        } finally {
          await session.shutDown();
        }
      });
      // What can hang is the boot, a heartbeat's registration or the shutdown, should one wait on
      // a timer, which the test's clock never runs here; the command's guard ends it.
    });

    test("a reading that throws still registers the session, without a place", async () => {
      tmuxPlaceState.reading = () => {
        throw new Error("the tmux reader broke");
      };
      const session = await bootPlacedSession("tmux-place-throws", "ses_tmux_throws");
      try {
        await session.heartbeat();

        expect(session.places).toEqual([undefined, undefined]);
      } finally {
        await session.shutDown();
      }
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
      try {
        expect(tmuxPlaceState.calls).toHaveLength(1);
        expect(tmuxPlaceState.calls[0]?.env.TMUX).toBeUndefined();
        expect(tmuxPlaceState.calls[0]?.env.TMUX_PANE).toBeUndefined();
        expect(calls).toEqual([]);
        expect(session.places).toEqual([undefined]);
      } finally {
        await session.shutDown();
      }
    });
  });
});
