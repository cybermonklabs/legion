import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import net from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import type { LegionRole } from "@legion/contracts";
import {
  createLegionDaemonClient,
  type DaemonRestartRetry,
  type LegionDaemonClient,
} from "@legion/pi-shared/daemon-client";
import { daemonWaitMs, forgetDaemonWaitsForTests } from "@legion/pi-shared/daemon-wait";
import { paneHandoffReport } from "@legion/pi-shared/handoff-record";
import type { PiApi, RegisteredTool, SessionContext } from "@legion/pi-shared/pi-types";
import { DEFAULT_STALL_LIMITS } from "@legion/pi-shared/stall-limits";
import { callOverdue, type StallCall } from "@legion/pi-shared/stall-watchdog";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { z } from "zod";
import { createLegionTool } from "./tools";

/** The contract the build under test speaks, which its client states on every request. */
const CONTRACT = 60;

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): string {
  return readFileSync(
    path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name),
    "utf8"
  );
}

const pi = {
  zod: z,
  sendMessage: () => undefined,
  appendEntry: () => undefined,
  getActiveTools: () => [],
  setActiveTools: async () => undefined,
  on: () => undefined,
  registerTool: () => undefined,
  registerCommand: () => undefined,
  registerMessageRenderer: () => undefined,
} as unknown as PiApi;

const context: SessionContext = {
  cwd: "/workspace",
  hasUI: true,
  setInterval: () => undefined,
  setTimeout: () => undefined,
  sessionManager: {
    getSessionId: () => "ses_208",
    getSessionFile: () => "/sessions/208.jsonl",
    ensureOnDisk: async () => undefined,
    getEntries: () => [],
  },
  ui: { notify: () => undefined },
};

const COMMIT = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
const HANDOFF_ID = "0f6d2c1e-7a3b-4e59-9c8d-2b1a6e4f3d70";
/** The claim's registration secret: random, as the daemon's is, since the pane holds it. */
const CLAIM_SECRET = fixtureSecret("claim");
/** The daemon's HANDOFF_NOT_NEW answer to the implementer naming its previous phase's record. */
const NOT_NEW = `the implementer named handoff record ${HANDOFF_ID} for its previous phase of LEGION-208; write this phase's handoff with \`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>\` before completing`;
const stops: Array<() => void> = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** The golden fixture each route the tool calls is answered from. */
const FIXTURE_OF: Readonly<Record<string, string>> = {
  "/legion/v1/state": "state.json",
  "/legion/v1/issues/LEGION-208/record": "issue-record.json",
  "/legion/v1/grants": "grant.json",
  "/legion/v1/handoff/complete": "handoff-complete.json",
  "/legion/v1/signoff": "signoff.json",
  "/legion/v1/gates/register": "gate-register.json",
  "/legion/v1/claims/relaunch": "relaunch.json",
};

/** The fixture `route` is answered with, or a 404 for a route the tool never calls. */
function fixtureAnswer(route: string): Response {
  const fixture = FIXTURE_OF[route];
  if (fixture === undefined) return new Response("no route", { status: 404 });
  return new Response(daemonFixture(fixture), { headers: { "Content-Type": "application/json" } });
}

/** The daemon after its restart: it answers each route from the golden fixtures, or `refuse`'s
 * status and body for the route it names, and records the path of every request it answered. */
function liveDaemon(refuse?: { path: string; status: number; body: object }) {
  const answered: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const route = new URL(request.url).pathname;
      answered.push(route);
      if (refuse !== undefined && route === refuse.path) {
        return Response.json(refuse.body, { status: refuse.status });
      }
      return fixtureAnswer(route);
    },
  });
  stops.push(() => server.stop(true));
  return { url: `http://127.0.0.1:${server.port}`, answered };
}

/** A daemon that takes the first request of `route` and does not answer it until the case calls
 * `release`, as a daemon that accepted a connection and stopped serving holds it, and then
 * answers it from the fixtures; every other request it answers at once. `answered` records the
 * path of every request as it arrives, and `held` resolves with the held request once it has
 * arrived, whose `signal` says whether the client ended it. */
function holdingDaemon(route: string) {
  const answered: string[] = [];
  const arrived = Promise.withResolvers<Request>();
  const released = Promise.withResolvers<void>();
  let holding = true;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    // A held request stays open until the case releases it: Bun's own 10-second idle limit
    // would otherwise close it for the client.
    idleTimeout: 0,
    fetch: async (request) => {
      const path = new URL(request.url).pathname;
      answered.push(path);
      if (holding && path === route) {
        holding = false;
        arrived.resolve(request);
        await released.promise;
      }
      return fixtureAnswer(path);
    },
  });
  stops.push(() => {
    released.resolve();
    server.stop(true);
  });
  return {
    url: `http://127.0.0.1:${server.port}`,
    answered,
    held: arrived.promise,
    release: () => released.resolve(),
  };
}

/** The real daemon client over a fetch that reads each answer whole before handing it on and
 * keeps every request it sent. `quiet` resolves once every request sent has settled and no step
 * that followed an answer sent another: everything such a step runs here is in memory, so one
 * turn of the event loop after the last answer is all it needs to send its next request. */
function trackedClient(url: string) {
  const sent: Promise<unknown>[] = [];
  const fetchFn = ((input: string | URL | Request, init?: RequestInit) => {
    const whole = fetch(input, init).then(
      async (response) =>
        new Response(await response.text(), {
          status: response.status,
          headers: response.headers,
        })
    );
    sent.push(whole.catch(() => undefined));
    return whole;
  }) as typeof fetch;
  const quiet = async () => {
    for (let seen = -1; seen !== sent.length; ) {
      seen = sent.length;
      await Promise.all(sent);
      await nextTurn();
    }
  };
  return {
    client: createLegionDaemonClient(url, { pluginContract: CONTRACT, fetch: fetchFn }),
    quiet,
  };
}

/** One turn of the event loop: every promise job already queued has run when it resolves. */
function nextTurn(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  setImmediate(resolve);
  return promise;
}

/** Resolves once the daemon has seen the client end `request` (its own `signal`, which Bun aborts
 * when the client closes the connection before the answer). A client that never ends it leaves
 * this pending, and the case fails on its own timeout. */
function endedByClient(request: Request): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (request.signal.aborted) resolve();
  request.signal.addEventListener("abort", () => resolve(), { once: true });
  return promise;
}

/** A port nothing listens on: a connection to it is refused, as it is while a daemon restarts. */
function refusingOrigin(): string {
  const server = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response("") });
  const origin = `http://127.0.0.1:${server.port}`;
  server.stop(true);
  return origin;
}

/** A listener that accepts each connection and closes it once the request arrives, before any
 * response: the daemon going down mid-request. */
async function closingOrigin(): Promise<string> {
  const server = net.createServer((socket) => {
    socket.on("data", () => socket.destroy());
  });
  const listening = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", listening.resolve);
  await listening.promise;
  stops.push(() => server.close());
  const address = server.address();
  if (address === null || typeof address === "string") throw new Error("no TCP address");
  return `http://127.0.0.1:${address.port}`;
}

/** The real daemon client over a fetch that sends its first `outage` requests to `down` (each
 * failing there with the real fetch error) and every later one to the live daemon. */
function restartingClient(live: string, down: string, outage: number) {
  let sent = 0;
  const fetchFn = ((input: string | URL | Request, init?: RequestInit) => {
    sent += 1;
    const url = String(input);
    return fetch(sent <= outage ? url.replace(live, down) : url, init);
  }) as typeof fetch;
  return createLegionDaemonClient(live, { pluginContract: CONTRACT, fetch: fetchFn });
}

/** A virtual clock: each wait is recorded and moves time on at once, and `pass` moves it on
 * while a request is open. */
function virtualClock() {
  let time = 0;
  const waits: number[] = [];
  return {
    waits,
    now: () => time,
    sleep: async (ms: number) => {
      waits.push(ms);
      time += ms;
    },
    pass: (ms: number) => {
      time += ms;
    },
  };
}

/** An attempt limit that never runs out, so every attempt waits for the daemon's own answer. */
const noLimit = () => Promise.withResolvers<void>().promise;

function toolFor(
  role: LegionRole,
  client: LegionDaemonClient,
  clock: Pick<DaemonRestartRetry, "attemptLimit"> & {
    readonly now: () => number;
    readonly sleep: (ms: number) => Promise<void>;
  },
  completed: string[] = [],
  tree = role === "architect" ? "LEGION-207" : "LEGION-208"
) {
  return createLegionTool({
    pi,
    daemon: () => client,
    session: () => ({
      kind: role === "architect" ? "architect" : "phase-worker",
      role,
      sessionID: "ses_208",
      tree,
      issue: "LEGION-208",
      secret: CLAIM_SECRET,
      claimToken: `legion-omp-legion-208-${role}`,
      generation: 1,
    }),
    controllerGrant: async () => {
      throw new Error("no controller grant expected");
    },
    onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
    onUpdateAnswered: () => undefined,
    handoffReport: async () => ({ handoffId: HANDOFF_ID, handoffVersion: 1, commit: COMMIT }),
    branchHead: async () => "",
    resolveDocument: async () => {
      throw new Error("no document lookup expected");
    },
    restartClock: {
      now: clock.now,
      sleep: clock.sleep,
      ...(clock.attemptLimit === undefined ? {} : { attemptLimit: clock.attemptLimit }),
    },
  });
}

/** Runs one tool call, collecting each partial result the tool sends the agent. `toolCallId` is
 * the id the host gives the call, which the tool records its wait for the daemon under. */
async function run(tool: RegisteredTool, parameters: Record<string, unknown>, toolCallId = "") {
  const updates: string[] = [];
  const onUpdate = (partial: { content: Array<{ text: string }> }) => {
    updates.push(partial.content.map((block) => block.text).join(""));
  };
  const result = await tool.execute(toolCallId, parameters, undefined, onUpdate, context);
  return { result, updates };
}

test("handoff_complete waits out a daemon that refuses connections while it restarts, and tells the agent once", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const completed: string[] = [];
  const tool = toolFor(
    "implementer",
    restartingClient(live.url, refusingOrigin(), 4),
    clock,
    completed
  );

  const { result, updates } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.isError).toBeUndefined();
  expect(result.details).toEqual(JSON.parse(daemonFixture("handoff-complete.json")));
  expect(completed).toEqual(["ses_208"]);
  expect(live.answered).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
  expect(clock.waits).toEqual([500, 1000, 2000, 4000]);
  expect(updates).toHaveLength(1);
  expect(updates[0]).toContain("The Legion daemon is unreachable, most likely restarting");
  expect(updates[0]).toContain("handoff_complete is being retried for up to 60 minutes");
});

// The worker's own stall watch must not end the turn whose `legion` call is waiting the daemon
// out, and must still reach any other call: so the wait is recorded under this call's own id
// (`@legion/pi-shared/daemon-wait`), which is the id the host hands `execute` and the one the
// watch keys the call by (`tool_call`'s `toolCallId`).
test("handoff_complete's wait for the daemon is recorded under the tool call that waits", async () => {
  forgetDaemonWaitsForTests();
  const live = liveDaemon();
  const clock = virtualClock();
  const tool = toolFor("implementer", restartingClient(live.url, refusingOrigin(), 4), clock);

  const { result } = await run(tool, { op: "handoff_complete", summary: "Done." }, "toolu_legion");

  expect(result.isError).toBeUndefined();
  expect(clock.waits).toEqual([500, 1000, 2000, 4000]);
  expect(daemonWaitMs("toolu_legion", 0, clock.now())).toBe(7_500);
  expect(daemonWaitMs("toolu_other", 0, clock.now())).toBe(0);
});

test("handoff_complete retries a connection the daemon closed before answering, and passes on the repeat's refusal marked as possibly following an applied attempt", async () => {
  // The first attempt's three requests are closed unanswered, so the completion may have landed;
  // the daemon answers the repeat ALREADY_RECORDED, which it also answers when that attempt was
  // refused, so the tool claims no success and leaves the phase open.
  const detail = `the implementer completion of phase implementing (round 0) naming handoff record ${HANDOFF_ID} (implement version 1) was already received; this call changed nothing`;
  const live = liveDaemon({
    path: "/legion/v1/handoff/complete",
    status: 409,
    body: { error: detail, code: "HANDOFF_ALREADY_RECORDED" },
  });
  const clock = virtualClock();
  const completed: string[] = [];
  const tool = toolFor(
    "implementer",
    restartingClient(live.url, await closingOrigin(), 3),
    clock,
    completed
  );

  const { result, updates } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.isError).toBe(true);
  expect(result.content[0]).toEqual({
    type: "text",
    text: `POST /legion/v1/handoff/complete failed with 409 HANDOFF_ALREADY_RECORDED: ${detail} (an earlier attempt of handoff_complete lost its connection before the daemon answered, and may have been applied: read_record of your own issue says whether it landed)`,
  });
  expect(completed).toEqual([]);
  expect(live.answered.filter((route) => route === "/legion/v1/handoff/complete")).toHaveLength(1);
  expect(updates).toHaveLength(1);
  expect(clock.waits).toEqual([500, 1000, 2000]);
});

test("a 409 after refused connections only is passed on unmarked, since a refused request was never sent", async () => {
  const detail = NOT_NEW;
  const live = liveDaemon({
    path: "/legion/v1/handoff/complete",
    status: 409,
    body: { error: detail, code: "HANDOFF_NOT_NEW" },
  });
  const clock = virtualClock();
  const tool = toolFor("implementer", restartingClient(live.url, refusingOrigin(), 2), clock);

  const { result } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.content[0]).toEqual({
    type: "text",
    text: `POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_NEW: ${detail}`,
  });
  expect(live.answered.filter((route) => route === "/legion/v1/handoff/complete")).toHaveLength(1);
  expect(clock.waits).toEqual([500, 1000]);
});

test("the daemon's own 502 or 503, which carries its code, is its answer and is not retried", async () => {
  const live = liveDaemon({
    path: "/legion/v1/signoff",
    status: 503,
    body: { error: "the daemon could not reach GitHub", code: "GITHUB_READ_FAILED" },
  });
  const clock = virtualClock();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 0), clock);

  const { result, updates } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result.isError).toBe(true);
  expect(live.answered).toEqual(["/legion/v1/grants", "/legion/v1/signoff"]);
  expect(clock.waits).toEqual([]);
  expect(updates).toEqual([]);
});

test("register_gate waits out a restarting daemon", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const client = restartingClient(live.url, refusingOrigin(), 2);
  const tool = toolFor("architect", client, clock, [], "LEGION-208");

  const { result, updates } = await run(tool, {
    op: "register_gate",
    issue: "LEGION-208",
    artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
    version: 1,
  });

  expect(result).toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
  expect(live.answered).toEqual(["/legion/v1/grants", "/legion/v1/gates/register"]);
  expect(clock.waits).toEqual([500, 1000]);
  expect(updates).toHaveLength(1);
});

test("an aborted call stops waiting and sends nothing more, even with the daemon back up", async () => {
  // Three attempts are refused; the daemon is back for the fourth, and the agent aborts during
  // the third wait, so the cancelled sign_off must never reach it.
  const live = liveDaemon();
  const clock = virtualClock();
  const abort = new AbortController();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 3), {
    now: clock.now,
    sleep: async (ms) => {
      await clock.sleep(ms);
      if (clock.waits.length === 3) abort.abort();
    },
  });

  const result = await tool.execute(
    "",
    { op: "sign_off", issue: "LEGION-208" },
    abort.signal,
    undefined,
    context
  );

  expect(result.isError).toBe(true);
  const [block] = result.content;
  expect(block?.type === "text" ? block.text : "").toBe(
    "sign_off: the Legion daemon was unreachable (ECONNREFUSED: Unable to connect. Is the computer able to access the url?) and the call was aborted; call it again once the daemon is back"
  );
  expect(clock.waits).toEqual([500, 1000, 2000]);
  expect(live.answered).toEqual([]);
});

/** The three writes that wait out a restarting daemon, each with the request its attempt sends
 * first, which the case's daemon holds, and every request the daemon should see: the held one,
 * then the whole of the next attempt. */
const WAITING_WRITES = [
  {
    op: "handoff_complete",
    role: "implementer",
    tree: "LEGION-208",
    parameters: { op: "handoff_complete", summary: "Done." },
    first: "/legion/v1/issues/LEGION-208/record",
    sent: [
      "/legion/v1/issues/LEGION-208/record",
      "/legion/v1/issues/LEGION-208/record",
      "/legion/v1/grants",
      "/legion/v1/handoff/complete",
    ],
  },
  {
    op: "sign_off",
    role: "architect",
    tree: "LEGION-207",
    parameters: { op: "sign_off", issue: "LEGION-208" },
    first: "/legion/v1/grants",
    sent: ["/legion/v1/grants", "/legion/v1/grants", "/legion/v1/signoff"],
  },
  {
    op: "register_gate",
    role: "architect",
    tree: "LEGION-208",
    parameters: {
      op: "register_gate",
      issue: "LEGION-208",
      artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
      version: 1,
    },
    first: "/legion/v1/grants",
    sent: ["/legion/v1/grants", "/legion/v1/grants", "/legion/v1/gates/register"],
  },
] as const;

// The attempt the wrapper gives up on at its five-minute limit must end at the daemon and go no
// further: its first request is ended, so it can neither mint a grant nor post anything when that
// request's answer comes late. The next attempt is then the only one that writes.
for (const write of WAITING_WRITES) {
  test(`${write.op}: an attempt given up at its limit is ended at the daemon and mints and posts nothing, though its first request answers later`, async () => {
    const daemon = holdingDaemon(write.first);
    const { client, quiet } = trackedClient(daemon.url);
    const clock = virtualClock();
    let attempts = 0;
    const tool = toolFor(
      write.role,
      client,
      {
        ...clock,
        // The first attempt's limit runs out once its first request is open at the daemon.
        attemptLimit: () => {
          attempts += 1;
          return attempts === 1 ? daemon.held.then(() => undefined) : noLimit();
        },
      },
      [],
      write.tree
    );

    const { result } = await run(tool, write.parameters);
    expect(result.isError).toBeUndefined();

    // The daemon answers the abandoned request at last, and whatever that attempt would do next
    // has had every chance to run.
    daemon.release();
    await quiet();
    expect(daemon.answered).toEqual([...write.sent]);
    await endedByClient(await daemon.held);
    expect(clock.waits).toEqual([500]);
  }, 30_000);
}

// A daemon that accepts the connection and does not answer is a daemon that is not serving, from
// the request's first moment (R1: the outage charges the worker no action of its stall watch). So a
// stall watch whose bound is below the five-minute attempt limit must not find the waiting call
// overdue while its first attempt is still open, and must still find a call that is not waiting
// overdue.
for (const write of WAITING_WRITES) {
  test(`${write.op}: a first attempt the daemon takes and does not answer charges the call's stall clock nothing`, async () => {
    forgetDaemonWaitsForTests();
    const daemon = holdingDaemon(write.first);
    const clock = virtualClock();
    const tool = toolFor(
      write.role,
      createLegionDaemonClient(daemon.url, { pluginContract: CONTRACT }),
      { ...clock, attemptLimit: noLimit },
      [],
      write.tree
    );
    const call: StallCall = {
      toolCallId: "toolu_legion",
      toolName: "legion",
      input: write.parameters,
      startedAt: clock.now(),
      deadlineMs: undefined,
      step: 0,
      steppedAt: undefined,
      told: undefined,
    };
    const limits = { ...DEFAULT_STALL_LIMITS, callBoundMs: 2 * 60_000 };

    const running = run(tool, write.parameters, "toolu_legion");
    await daemon.held;
    clock.pass(4 * 60_000);

    const waited = daemonWaitMs("toolu_legion", 0, clock.now());
    expect(waited).toBe(4 * 60_000);
    expect(callOverdue(call, clock.now(), limits, waited)).toBeUndefined();
    // The same four minutes on a call that is not waiting for the daemon are past the bound.
    const bash = { ...call, toolCallId: "toolu_bash", toolName: "bash" };
    expect(
      callOverdue(bash, clock.now(), limits, daemonWaitMs("toolu_bash", 0, clock.now()))?.rule
    ).toEqual({ kind: "bound", boundMs: 2 * 60_000 });

    daemon.release();
    expect((await running).result.isError).toBeUndefined();
  }, 30_000);
}

/** A reviewer's `legion` tool whose `handoff_complete` reads its review record from the pane's
 * Dispatch (`paneHandoffReport`, as the entry wires it), over a Dispatch that holds the first
 * read until the case calls `release` and a daemon that answers every route at once. `first`
 * resolves with the held read once it has arrived, whose `signal` says whether the client ended
 * it; `reads` records every read Dispatch was sent. */
async function reviewerOverHeldDispatch(attemptLimit: DaemonRestartRetry["attemptLimit"]) {
  const root = await mkdtemp(path.join(tmpdir(), "legion-report-read-"));
  stops.push(() => void rm(root, { recursive: true, force: true }));
  const reads: Request[] = [];
  const first = Promise.withResolvers<Request>();
  const released = Promise.withResolvers<void>();
  const dispatch = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    // The held read stays open until the case releases it: Bun's own 10-second idle limit would
    // otherwise close it for the client.
    idleTimeout: 0,
    fetch: async (request) => {
      reads.push(request);
      if (reads.length === 1) {
        first.resolve(request);
        await released.promise;
      }
      return Response.json({
        id: HANDOFF_ID,
        version: 1,
        issue_key: "LEGION-208",
        phase: "review",
        author: { kind: "agent", session_id: "ses_208" },
        created_at: "2026-11-16T09:00:00Z",
        body: JSON.stringify({ schemaVersion: 1, phase: "review", head: COMMIT }),
      });
    },
  });
  stops.push(() => {
    released.resolve();
    dispatch.stop(true);
  });
  const live = liveDaemon();
  const clock = virtualClock();
  const paneEnv = {
    LEGION_WORKSPACE: root,
    LEGION_ISSUE: "LEGION-208",
    DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
    DISPATCH_TOKEN: fixtureSecret("pane"),
  };
  const tool = createLegionTool({
    pi,
    daemon: () => createLegionDaemonClient(live.url, { pluginContract: CONTRACT }),
    session: () => ({
      kind: "phase-worker",
      role: "reviewer",
      sessionID: "ses_208",
      tree: "LEGION-208",
      issue: "LEGION-208",
      secret: CLAIM_SECRET,
      claimToken: "legion-omp-legion-208-reviewer",
      generation: 1,
    }),
    controllerGrant: async () => {
      throw new Error("no controller grant expected");
    },
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: paneHandoffReport(
      { path: path.join(root, "envoy.json"), error: null },
      paneEnv,
      root,
      "ses_208"
    ),
    branchHead: async () => "",
    resolveDocument: async () => {
      throw new Error("no document lookup expected");
    },
    restartClock: {
      now: clock.now,
      sleep: clock.sleep,
      ...(attemptLimit === undefined ? {} : { attemptLimit }),
    },
  });
  return { tool, live, reads, first: first.promise, release: () => released.resolve(), clock };
}

// The report a completion finds in the pane is no request to the daemon: a reviewer's
// `handoff_complete` reads its review record from the pane's Dispatch, and a read that is still
// open when the attempt's limit runs out is the pane's slow answer, not a daemon that stopped
// serving. So the read is left to finish, the agent is not told the daemon is restarting, nothing
// is read or sent again, and none of the read's time is recorded as a wait for the daemon.
test("a handoff_complete whose report read waits on Dispatch past the attempt's limit is no daemon outage: the read finishes and the call completes once", async () => {
  forgetDaemonWaitsForTests();
  let limits = 0;
  const held = await reviewerOverHeldDispatch(() => {
    limits += 1;
    // The first limit armed runs out once the report read is open at Dispatch.
    return limits === 1 ? held.first.then(() => undefined) : noLimit();
  });

  const running = run(held.tool, { op: "handoff_complete", summary: "Reviewed." }, "toolu_legion");
  await held.first;
  // Six minutes of a slow read, and a turn of the event loop for the limit to act in.
  held.clock.pass(6 * 60_000);
  await nextTurn();
  held.release();
  const { result, updates } = await running;

  expect(result.isError).toBeUndefined();
  expect(updates).toEqual([]);
  expect(held.live.answered).toEqual([
    "/legion/v1/issues/LEGION-208/record",
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ]);
  expect(held.reads).toHaveLength(1);
  expect(held.clock.waits).toEqual([]);
  expect(daemonWaitMs("toolu_legion", 0, held.clock.now())).toBe(0);
}, 30_000);

// The agent's abort still reaches the report read: it ends the read at Dispatch, the call
// answers at once, and no grant is minted and nothing posted.
test("an abort while a handoff_complete's report read waits on Dispatch ends that read at Dispatch and posts nothing", async () => {
  const held = await reviewerOverHeldDispatch(noLimit);
  const abort = new AbortController();

  const running = held.tool.execute(
    "",
    { op: "handoff_complete", summary: "Reviewed." },
    abort.signal,
    undefined,
    context
  );
  await held.first;
  abort.abort();
  const result = await running;

  expect(result.isError).toBe(true);
  await endedByClient(await held.first);
  held.release();
  await nextTurn();
  expect(held.live.answered).toEqual(["/legion/v1/issues/LEGION-208/record"]);
  expect(held.reads).toHaveLength(1);
}, 30_000);

// The agent's abort ends the attempt that is waiting on the daemon, not only the waits between
// attempts: the open request is ended at the daemon, the call answers at once rather than at the
// attempt's five-minute limit (which never runs out here), and nothing that attempt would do next
// is sent when the daemon answers late.
test("an abort while an attempt waits on the daemon ends that attempt at the daemon, answers at once, and sends nothing more", async () => {
  const daemon = holdingDaemon("/legion/v1/issues/LEGION-208/record");
  const { client, quiet } = trackedClient(daemon.url);
  const clock = virtualClock();
  const tool = toolFor("implementer", client, { ...clock, attemptLimit: noLimit });
  const abort = new AbortController();

  const running = tool.execute(
    "",
    { op: "handoff_complete", summary: "Done." },
    abort.signal,
    undefined,
    context
  );
  await daemon.held;
  abort.abort();
  const result = await running;

  expect(result.isError).toBe(true);
  const [block] = result.content;
  expect(block?.type === "text" ? block.text : "").toBe(
    "handoff_complete: the call was aborted while an attempt waited for the daemon's answer; that attempt may have been applied, so read the record before calling again"
  );
  // The open request is ended at the daemon while the daemon still holds it, before it answers.
  await endedByClient(await daemon.held);
  daemon.release();
  await quiet();
  expect(daemon.answered).toEqual(["/legion/v1/issues/LEGION-208/record"]);
  expect(clock.waits).toEqual([]);
}, 30_000);

test("a 409 from the daemon is its answer and is never retried", async () => {
  const live = liveDaemon({
    path: "/legion/v1/handoff/complete",
    status: 409,
    body: { error: NOT_NEW, code: "HANDOFF_NOT_NEW" },
  });
  const clock = virtualClock();
  const completed: string[] = [];
  const tool = toolFor(
    "implementer",
    restartingClient(live.url, refusingOrigin(), 0),
    clock,
    completed
  );

  const { result, updates } = await run(tool, { op: "handoff_complete", summary: "Done." });

  expect(result.isError).toBe(true);
  expect(result.content).toEqual([
    {
      type: "text",
      text: `POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_NEW: ${NOT_NEW}`,
    },
  ]);
  expect(live.answered.filter((route) => route === "/legion/v1/handoff/complete")).toHaveLength(1);
  expect(clock.waits).toEqual([]);
  expect(updates).toEqual([]);
  expect(completed).toEqual([]);
});

// A relaunch is not among the writes that wait out a restart. Its request reached a daemon that
// closed the connection before answering, so the claim may already be stopped and started again,
// and a repeat would be a second relaunch inside 6 hours, which the daemon refuses as a person's
// case. So the call fails at once, naming the closed connection, though the daemon is back.
test("relaunch is posted once: a connection closed before the answer fails the call, with no wait and no repeat", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const client = restartingClient(live.url, await closingOrigin(), 1);
  const tool = createLegionTool({
    pi,
    daemon: () => client,
    session: () => ({ kind: "controller", sessionID: "ses_controller" }),
    controllerGrant: async () => ({
      grantId: "controller-grant-1",
      expiresAt: "2099-01-01T00:00:00Z",
    }),
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: async () => {
      throw new Error("no handoff report expected");
    },
    branchHead: async () => "",
    resolveDocument: async () => {
      throw new Error("no document lookup expected");
    },
    restartClock: { now: clock.now, sleep: clock.sleep },
  });

  const { result, updates } = await run(tool, {
    op: "relaunch",
    claim: "legion-omp-legion-208-implementer",
  });

  expect(result.isError).toBe(true);
  expect(result.content).toEqual([
    { type: "text", text: expect.stringContaining("ECONNRESET: The socket connection was closed") },
  ]);
  expect(live.answered).toEqual([]);
  expect(clock.waits).toEqual([]);
  expect(updates).toEqual([]);
});

test("sign_off waits out a restarting daemon with a fresh grant on each attempt, and a 409 is not retried", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 2), clock);

  const { result, updates } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result).toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
  expect(live.answered).toEqual(["/legion/v1/grants", "/legion/v1/signoff"]);
  expect(clock.waits).toEqual([500, 1000]);
  expect(updates).toHaveLength(1);
  expect(updates[0]).toContain("sign_off is being retried");

  const refusing = liveDaemon({
    path: "/legion/v1/signoff",
    status: 409,
    body: {
      error: "LEGION-208 is in phase reviewing, not accepting; this sign-off changed nothing",
      code: "SIGNOFF_OUTSIDE_ACCEPTING",
    },
  });
  const quiet = virtualClock();
  const refused = await run(
    toolFor("architect", restartingClient(refusing.url, refusingOrigin(), 0), quiet),
    { op: "sign_off", issue: "LEGION-208" }
  );
  expect(refused.result.isError).toBe(true);
  expect(refused.result.content[0]).toMatchObject({
    text: expect.stringContaining("409 SIGNOFF_OUTSIDE_ACCEPTING"),
  });
  expect(refusing.answered.filter((route) => route === "/legion/v1/signoff")).toHaveLength(1);
  expect(quiet.waits).toEqual([]);
});

test("a 503 from a proxy in front of the daemon is retried like a refused connection", async () => {
  let unavailable = 2;
  const answered: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const route = new URL(request.url).pathname;
      answered.push(route);
      if (unavailable > 0) {
        unavailable -= 1;
        return new Response("<html>503 Service Unavailable</html>", { status: 503 });
      }
      return new Response(
        daemonFixture(route === "/legion/v1/grants" ? "grant.json" : "signoff.json")
      );
    },
  });
  stops.push(() => server.stop(true));
  const clock = virtualClock();
  const tool = toolFor(
    "architect",
    createLegionDaemonClient(`http://127.0.0.1:${server.port}`, { pluginContract: CONTRACT }),
    clock
  );

  const { result } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result.isError).toBeUndefined();
  expect(answered).toEqual([
    "/legion/v1/grants",
    "/legion/v1/grants",
    "/legion/v1/grants",
    "/legion/v1/signoff",
  ]);
  expect(clock.waits).toEqual([500, 1000]);
});

test("a daemon that stays down past the sixty-minute budget fails the call with the reason", async () => {
  const live = liveDaemon();
  const clock = virtualClock();
  const tool = toolFor("architect", restartingClient(live.url, refusingOrigin(), 1_000), clock);

  const { result, updates } = await run(tool, { op: "sign_off", issue: "LEGION-208" });

  expect(result.isError).toBe(true);
  const [block] = result.content;
  expect(block?.type === "text" ? block.text : "").toBe(
    "sign_off: the Legion daemon was unreachable (ECONNREFUSED: Unable to connect. Is the computer able to access the url?) and stayed so for 60 minutes; call it again once the daemon is back"
  );
  expect(live.answered).toEqual([]);
  expect(clock.waits.reduce((sum, wait) => sum + wait, 0)).toBe(60 * 60_000);
  expect(Math.max(...clock.waits)).toBe(30_000);
  expect(updates).toHaveLength(1);
});
