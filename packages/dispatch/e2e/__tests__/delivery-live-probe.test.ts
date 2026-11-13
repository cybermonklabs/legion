import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  type FakeDispatchServer,
  startFakeDispatch,
} from "@legion/envoy-client/test/fake-dispatch";
import type { ServerWebSocket } from "bun";

import after from "../../../contracts/fixtures/live/after.json";
import delta from "../../../contracts/fixtures/live/delta.json";
import error from "../../../contracts/fixtures/live/error.json";
import heartbeat from "../../../contracts/fixtures/live/heartbeat.json";
import snapshot from "../../../contracts/fixtures/live/snapshot.json";
import { REQUEST_DEADLINE_MS } from "../../web/src/api/client";
import { RECONNECT_SPREAD_MS } from "../../web/src/api/live-socket";
import type {
  DeliveryLiveDelta,
  DeliveryLiveSnapshot,
  DeliveryLiveView,
  DeliveryTimelineWithMeasures,
  LiveClientFrame,
} from "../../web/src/api/types";
import type {
  ProbeCloseLine,
  ProbeCompareLine,
  ProbeHeld,
  ProbeLine,
  ProbeOpenLine,
} from "../delivery-live-probe";
import { rowLimit, runBounded } from "./bounded-subprocess";

// The probe runs as an agent runs it, a process of its own, against a stand-in for Dispatch that
// replays the frames Dispatch's own diff writes (packages/contracts/fixtures/live): a
// timeline snapshot, the delta of a deploy, a merge and two pull requests leaving the window, a
// heartbeat, an error, and the snapshot a page subscribing after the delta gets.

const held = snapshot as unknown as DeliveryLiveSnapshot;
const change = delta as unknown as DeliveryLiveDelta;
const next = after as unknown as DeliveryLiveSnapshot;
const heldBody = held.body as DeliveryTimelineWithMeasures;
const nextBody = next.body as DeliveryTimelineWithMeasures;

const TOKEN = "probe-token";
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const probe = fileURLToPath(new URL("../delivery-live-probe.ts", import.meta.url));
const view: DeliveryLiveView = {
  kind: "timeline",
  query: "repo=acme%2Fwidgets",
  window: { preset: 28 },
};

interface Script {
  /** Answers the probe's `count`th subscribe (from 1) on `socket`, for its subscription `sub`. */
  readonly onSubscribe: (socket: ServerWebSocket<undefined>, sub: string, count: number) => void;
  /** Each GET route's body, by path. */
  readonly routes?: Readonly<Record<string, unknown>>;
}

interface Run {
  readonly lines: readonly ProbeLine[];
  readonly stderr: readonly string[];
  /** Sends the probe `signal`, as a Ctrl-C does. */
  kill(signal: NodeJS.Signals): void;
  /** Resolves once `ready` holds; rejects if the probe exits first. */
  until(ready: () => boolean): Promise<void>;
  /** The exit code, once both output streams are read to their end. */
  exited(): Promise<number>;
}

const servers: FakeDispatchServer<LiveClientFrame>[] = [];
const kills: (() => void)[] = [];
const scratchDirs: string[] = [];
/** Woken whenever something a test waits on happens: a line from the probe, a frame the probe
 *  sent, or the probe's output ending. Each test waits on that event, never on a guessed time. */
const waiters = new Set<() => void>();
const wake = () => {
  for (const waiter of [...waiters]) waiter();
};

afterEach(() => {
  waiters.clear();
  for (const kill of kills.splice(0)) kill();
  for (const server of servers.splice(0)) server.stop();
  for (const dir of scratchDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

function scratch(): string {
  const dir = mkdtempSync(join(tmpdir(), "delivery-live-probe-"));
  scratchDirs.push(dir);
  return dir;
}

/** `fixture` as Dispatch sends it to the subscription `sub`. */
const forSub = (fixture: object, sub: string) => JSON.stringify({ ...fixture, sub });

/** A Dispatch the probe watches, on a loopback port of its own: the probe's bearer, the routes
 *  `--compare` reads, and `onSubscribe` answering the `count`th subscribe (from 1). */
function probeDispatch(script: Script): FakeDispatchServer<LiveClientFrame> {
  const dispatch = startFakeDispatch<LiveClientFrame>({
    live: (socket, frame, nth) => {
      if (frame.type === "subscribe") script.onSubscribe(socket, frame.sub, nth);
    },
    onFrame: wake,
    routes: script.routes,
    token: TOKEN,
  });
  servers.push(dispatch);
  return dispatch;
}

async function readLines(
  stream: ReadableStream<Uint8Array>,
  onLine: (line: string) => void
): Promise<void> {
  const reader = stream.getReader();
  const decoder = new TextDecoder();
  let pending = "";
  for (let chunk = await reader.read(); !chunk.done; chunk = await reader.read()) {
    pending += decoder.decode(chunk.value, { stream: true });
    const complete = pending.split("\n");
    pending = complete.pop() ?? "";
    for (const line of complete) if (line !== "") onLine(line);
  }
  if (pending !== "") onLine(pending);
}

function runProbe(args: readonly string[]): Run {
  const child = Bun.spawn([process.execPath, probe, ...args], {
    cwd: dispatchPackage,
    stderr: "pipe",
    stdin: "ignore",
    stdout: "pipe",
  });
  kills.push(() => child.kill("SIGKILL"));
  const lines: ProbeLine[] = [];
  const stderr: string[] = [];
  let finished = false;
  const reading = Promise.all([
    // The probe writes one JSON object per line; the assertions read each as the line it names.
    readLines(child.stdout, (line) => {
      lines.push(JSON.parse(line) as ProbeLine);
      wake();
    }),
    readLines(child.stderr, (line) => stderr.push(line)),
  ]).then(() => {
    finished = true;
    wake();
  });
  return {
    async exited() {
      await reading;
      return child.exited;
    },
    kill: (signal) => child.kill(signal),
    lines,
    stderr,
    until(ready) {
      const { promise, reject, resolve } = Promise.withResolvers<void>();
      const check = () => {
        if (ready()) {
          waiters.delete(check);
          resolve();
        } else if (finished) {
          waiters.delete(check);
          reject(new Error(`the probe ended first: ${JSON.stringify(lines)} ${stderr.join("\n")}`));
        }
      };
      waiters.add(check);
      check();
      return promise;
    },
  };
}

/** The probe as the nine rows that wait for its exit alone start it, six written out and three
 *  from the loop over the flags that set a timer: a bounded child, read whole once it has ended.
 *  They read the exit status and the stderr, which is what the run itself answers with, so this
 *  adds nothing to it. No row drives this one, so its bound stops nothing a row is still using. */
function runProbeToEnd(args: readonly string[]) {
  return runBounded({
    argv: [process.execPath, probe, ...args],
    bound: "short",
    cwd: dispatchPackage,
    from: import.meta.url,
  });
}

/** The `open` lines of the probe's whole stdout. */
function opensOf(stdout: string): ProbeOpenLine[] {
  return stdout
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as ProbeLine)
    .filter((line): line is ProbeOpenLine => line.type === "open");
}

function args(dispatch: { readonly url: string }, token = TOKEN, minutes = "1"): string[] {
  const tokenFile = join(scratch(), "token");
  writeFileSync(tokenFile, `${token}\n`, { mode: 0o600 });
  return [
    "--url",
    dispatch.url,
    "--token-file",
    tokenFile,
    "--view",
    JSON.stringify(view),
    "--minutes",
    minutes,
  ];
}

/** Each key of `record`'s maps, counted. */
const sizes = (record: Readonly<Record<string, object>>) =>
  Object.fromEntries(
    Object.entries(record).map(([key, value]) => [key, Object.keys(value).length])
  );
/** Each key of `record`'s maps, named. */
const names = (record: Readonly<Record<string, object>>) =>
  Object.fromEntries(Object.entries(record).map(([key, value]) => [key, Object.keys(value)]));
const ids = (items: readonly { readonly id: string | number }[]) => items.map((item) => item.id);
const comparisons = (lines: readonly ProbeLine[]) =>
  lines.filter((line): line is ProbeCompareLine => line.type === "compare");
const nothing = {
  changed_at: null,
  counts: null,
  maps: null,
  prs: null,
  runs: null,
  seq: null,
  session_groups: null,
  set: null,
};

test(
  "the probe writes one line per frame and holds what the page would after them",
  async () => {
    const sent: string[] = [];
    const dispatch = probeDispatch({
      onSubscribe(socket, sub) {
        for (const text of [
          forSub(snapshot, sub),
          forSub(delta, sub),
          JSON.stringify(heartbeat),
          forSub(error, sub),
        ]) {
          sent.push(text);
          socket.send(text);
        }
      },
      routes: {
        "/api/v1/delivery/measures": nextBody.measures,
        "/api/v1/delivery/timeline": nextBody,
      },
    });
    const bodyFile = join(scratch(), "body.json");
    const run = runProbe([...args(dispatch), "--compare", "--body-file", bodyFile]);
    await run.until(() => run.lines.some((line) => line.type === "error"));
    run.kill("SIGINT");
    expect(await run.exited()).toBe(0);

    expect(dispatch.upgrades).toEqual([`Bearer ${TOKEN}`]);
    expect(dispatch.received).toEqual([{ sub: "probe", type: "subscribe", view }]);
    const bytes = sent.map((text) => Buffer.byteLength(text));
    const prsChange = change.items.prs;
    const runsChange = change.items.runs;
    if (prsChange === undefined || runsChange === undefined) {
      throw new Error("delta.json changes the pull requests and the runs");
    }
    expect(
      run.lines.filter((line) => line.type !== "compare").map(({ at: _at, ...line }) => line)
    ).toEqual([
      { resume: null, type: "open" },
      {
        ...nothing,
        bytes: bytes[0],
        changed_at: held.changed_at,
        counts: {
          color_counts: sizes(heldBody.color_counts ?? {}),
          facet_counts: sizes(heldBody.facet_counts ?? {}),
        },
        maps: {
          components: Object.keys(heldBody.components ?? {}).length,
          issue_titles: Object.keys(heldBody.issue_titles ?? {}).length,
          session_pr_counts: Object.keys(heldBody.session_pr_counts ?? {}).length,
          session_titles: Object.keys(heldBody.session_titles ?? {}).length,
        },
        prs: (heldBody.prs ?? []).length,
        runs: (heldBody.runs ?? []).length,
        seq: held.seq,
        session_groups: (heldBody.session_groups ?? []).length,
        type: "snapshot",
      },
      {
        ...nothing,
        bytes: bytes[1],
        changed_at: change.changed_at,
        counts: {
          color_counts: names(change.counts.color_counts ?? {}),
          facet_counts: names(change.counts.facet_counts ?? {}),
        },
        maps: names(change.maps),
        prs: { remove: prsChange.remove, upsert: ids(prsChange.upsert) },
        runs: { remove: runsChange.remove, upsert: ids(runsChange.upsert) },
        seq: change.seq,
        set: Object.keys(change.set),
        type: "delta",
      },
      { ...nothing, bytes: bytes[2], type: "heartbeat" },
      {
        ...nothing,
        bytes: bytes[3],
        code: "DELIVERY_NOT_CONFIGURED",
        message: error.message,
        type: "error",
      },
    ]);
    // The delta names the deploy's run, the merge and the two pull requests that left.
    expect(prsChange.remove).toEqual(["acme/widgets#1", "acme/widgets#2"]);
    expect(ids(runsChange.upsert)).toEqual([42]);

    // What the probe holds is the snapshot a page subscribing afterwards gets. A delta never
    // carries freshness, which has its own frame, so it is the one field the two may differ in.
    const applied = (await Bun.file(bodyFile).json()) as ProbeHeld;
    const { freshness: _applied, ...appliedBody } = applied.body as DeliveryTimelineWithMeasures;
    const { freshness: _next, ...expectedBody } = nextBody;
    expect(appliedBody).toEqual(expectedBody);
    expect(applied.revs).toEqual(next.revs);
    expect(applied.seq).toBe(change.seq);
    expect(applied.changed_at).toBe(change.changed_at);

    // --compare reads both routes for the window the delta moved to, with the view's own query.
    expect(
      dispatch.reads.map((url) => [
        url.pathname,
        url.searchParams.get("from"),
        url.searchParams.get("to"),
        url.searchParams.get("repo"),
      ])
    ).toEqual([
      ["/api/v1/delivery/timeline", nextBody.window.from, nextBody.window.to, "acme/widgets"],
      ["/api/v1/delivery/measures", nextBody.window.from, nextBody.window.to, "acme/widgets"],
    ]);
    const same = { equal: true, extra: [], missing: [] };
    const allEqual = { differ: [], equal: true };
    expect(
      comparisons(run.lines).map(({ at: _at, route, ...line }) => ({
        ...line,
        route: route.split("?")[0],
      }))
    ).toEqual([
      {
        equal: true,
        measures: allEqual,
        prs: same,
        route: "/api/v1/delivery/timeline",
        runs: same,
        status: 200,
        type: "compare",
      },
      {
        equal: true,
        measures: allEqual,
        prs: null,
        route: "/api/v1/delivery/measures",
        runs: null,
        status: 200,
        type: "compare",
      },
    ]);
  },
  {
    // This row drives the probe: it waits for the error line, sends SIGINT, and then reads the
    // body file the probe wrote. A bound at the child would kill a process this row still uses.
    timeout: 30_000,
  }
);

test(
  "--compare names each pull request, run and measure a GET answer differs in, and exits 1",
  async () => {
    const [firstPR] = nextBody.prs ?? [];
    const [firstRun] = nextBody.runs ?? [];
    if (firstPR === undefined || firstRun === undefined) {
      throw new Error("after.json holds a pull request and a run");
    }
    const dispatch = probeDispatch({
      onSubscribe(socket, sub) {
        socket.send(forSub(snapshot, sub));
        socket.send(forSub(delta, sub));
      },
      routes: {
        "/api/v1/delivery/measures": nextBody.measures,
        "/api/v1/delivery/timeline": {
          ...nextBody,
          measures: {
            ...nextBody.measures,
            measures: { ...nextBody.measures.measures, rework_share: 0.99 },
          },
          prs: (nextBody.prs ?? []).slice(1),
          runs: [...(nextBody.runs ?? []), { ...firstRun, id: 99 }],
        },
      },
    });
    const run = runProbe([...args(dispatch), "--compare"]);
    await run.until(() => run.lines.some((line) => line.type === "delta"));
    run.kill("SIGINT");
    expect(await run.exited()).toBe(1);

    expect(
      comparisons(run.lines).map(({ equal, measures, prs, runs }) => ({
        equal,
        measures,
        prs,
        runs,
      }))
    ).toEqual([
      {
        equal: false,
        measures: { differ: ["measures.rework_share"], equal: false },
        prs: { equal: false, extra: [firstPR.id], missing: [] },
        runs: { equal: false, extra: [], missing: ["99"] },
      },
      { equal: true, measures: { differ: [], equal: true }, prs: null, runs: null },
    ]);
  },
  {
    // This row drives the probe through --compare: it waits for the delta line and then sends
    // SIGINT. A bound at the child would stop a process this row is still reading from.
    timeout: 30_000,
  }
);

// Dispatch's rotation (4001) is the one close the probe answers by opening again at once; its
// restart (1012) and any other close wait a draw of the first attempt's spread or longer. The
// close line names the delay the probe chose, so the row reads that choice from the probe's own
// output, and the second subscribe is the fact that the reconnect happened.
for (const [code, reason, what] of [[4001, "max age", "rotation"]] as const) {
  test(
    `after a ${what} close the probe reconnects at once and resumes with the change and revisions it holds`,
    async () => {
      const dispatch = probeDispatch({
        onSubscribe(socket, sub, count) {
          if (count > 1) return;
          socket.send(forSub(snapshot, sub));
          socket.send(forSub(delta, sub));
          socket.close(code, reason);
        },
      });
      const run = runProbe(args(dispatch));
      await run.until(() => dispatch.received.length === 2);
      run.kill("SIGINT");
      expect(await run.exited()).toBe(0);

      expect(dispatch.received).toEqual([
        { sub: "probe", type: "subscribe", view },
        { resume: { revs: next.revs, seq: change.seq }, sub: "probe", type: "subscribe", view },
      ]);
      expect(
        run.lines.map((line) => {
          if (line.type === "open") return `open ${line.resume}`;
          if (line.type === "close") return `close ${line.code} retry_ms ${line.retry_ms}`;
          return line.type;
        })
      ).toEqual([
        "open null",
        "snapshot",
        "delta",
        `close ${code} retry_ms 0`,
        `open ${change.seq}`,
      ]);
    },
    {
      // This row drives the probe across a rotation close: it waits for the second subscribe
      // and then sends SIGINT. A bound at the child would kill a process this row still uses.
      timeout: 30_000,
    }
  );
}

test(
  "after a restart close the probe reconnects within the spread and resumes with the change and revisions it holds",
  async () => {
    const dispatch = probeDispatch({
      onSubscribe(socket, sub, count) {
        if (count > 1) return;
        socket.send(forSub(snapshot, sub));
        socket.send(forSub(delta, sub));
        socket.close(1012, "service restart");
      },
    });
    const run = runProbe(args(dispatch));
    await run.until(() => dispatch.received.length === 2);
    run.kill("SIGINT");
    expect(await run.exited()).toBe(0);

    expect(dispatch.received).toEqual([
      { sub: "probe", type: "subscribe", view },
      { resume: { revs: next.revs, seq: change.seq }, sub: "probe", type: "subscribe", view },
    ]);
    expect(
      run.lines.map((line) => {
        if (line.type === "open") return `open ${line.resume}`;
        if (line.type === "close") return `close ${line.code}`;
        return line.type;
      })
    ).toEqual(["open null", "snapshot", "delta", "close 1012", `open ${change.seq}`]);
    // The page's first attempt after a restart close waits a draw of up to
    // `RECONNECT_SPREAD_MS`; the close line names the draw the probe made before it dialled.
    const retry = run.lines.find((line): line is ProbeCloseLine => line.type === "close")?.retry_ms;
    expect(retry).toBeNumber();
    expect(retry).toBeGreaterThanOrEqual(0);
    expect(retry).toBeLessThanOrEqual(RECONNECT_SPREAD_MS);
  },
  {
    // This row drives the probe across a restart close: it waits for the second subscribe,
    // sends SIGINT, and reads the close line. A bound would stop a process it still uses.
    timeout: 30_000,
  }
);

test(
  "a refused upgrade ends the probe with exit 1, naming the refusal",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    const run = await runProbeToEnd(args(dispatch, "not-the-token"));
    expect(run.code).toBe(1);
    expect(dispatch.upgrades).toEqual(["Bearer not-the-token"]);
    expect(run.stderr).toContain("Dispatch refused the connection to ws://127.0.0.1:");
  },
  rowLimit("short")
);

test(
  "a refused view ends the watch with exit 1, naming the code",
  async () => {
    const dispatch = probeDispatch({
      onSubscribe(socket, sub) {
        socket.send(
          JSON.stringify({
            code: "INVALID_QUERY",
            message: "window is wider than 84 days",
            sub,
            type: "error",
          })
        );
      },
    });
    const run = await runProbeToEnd(args(dispatch));
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(
      "Dispatch refused the view: INVALID_QUERY: window is wider than 84 days"
    );
  },
  rowLimit("short")
);

test(
  "the watch ends on its own after --minutes",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    // 0.01 minutes is 600 ms of the probe's own clock, in another process, which no fake timer
    // here reaches. A reading taken here once the process has ended counts its exit and the drain
    // of its pipes as watch, so the row below reads the watch's length from the probe's own
    // `open` and `end` lines. This row holds that the watch ends by itself, cleanly, on one open.
    const run = await runProbeToEnd(args(dispatch, TOKEN, "0.01"));
    expect(run.code).toBe(0);
    expect(opensOf(run.stdout)).toHaveLength(1);
    expect(dispatch.received).toEqual([{ sub: "probe", type: "subscribe", view }]);
  },
  rowLimit("short")
);

// RED TEST of the testing phase, dispatch://INFRA-3082, 2026-11-13. `--minutes` is the length of
// the watch from its socket's first open, so the probe's own clock must say when the watch ended:
// a reading the test takes after the process has exited and its pipes have drained counts that
// time as watch. Measured in this issue's pod on 2026-11-13, a probe that stopped 540 ms after its
// open passed the row above 4 of 5 times under six busy loops. The probe writes an `end` line
// when its watch stops, and that line's time comes no sooner than `--minutes` after its open.
test(
  "the probe's own end line comes no sooner than --minutes after its open",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    const run = await runProbeToEnd(args(dispatch, TOKEN, "0.01"));
    expect(run.code).toBe(0);
    const written = run.stdout
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as { readonly at: string; readonly type: string });
    const opens = written.filter((line) => line.type === "open");
    const ends = written.filter((line) => line.type === "end");
    expect(opens).toHaveLength(1);
    expect(ends, "the probe wrote no end line when its watch stopped").toHaveLength(1);
    expect(
      Date.parse(ends[0].at) - Date.parse(opens[0].at),
      "the probe's own clock says its watch ended before 600 ms had passed since its open"
    ).toBeGreaterThanOrEqual(600);
  },
  rowLimit("short")
);

/** A Dispatch on a loopback port of its own that holds each `/ws/live` upgrade for `holdMs`,
 *  counted from the moment the probe asks for it, as a loaded pod can answer late, and then takes
 *  it. It records the moment it let each upgrade go, and every frame the probe sent, parsed. */
function heldDispatch(holdMs: number) {
  const released: number[] = [];
  const received: LiveClientFrame[] = [];
  const server = Bun.serve({
    async fetch(request, srv) {
      if (new URL(request.url).pathname !== "/ws/live") {
        return new Response("not found", { status: 404 });
      }
      // Real time, since the probe is a process of its own whose clock no fake timer here
      // reaches; the row reads the order of this release and the probe's open, not this length.
      await Bun.sleep(holdMs);
      released.push(Date.now());
      return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(_socket, raw) {
        received.push(JSON.parse(String(raw)) as LiveClientFrame);
      },
    },
  });
  kills.push(() => server.stop(true));
  return { received, released, url: `http://127.0.0.1:${server.port}` };
}

// `--minutes` counts from the socket's first open, not from the dial, so a socket that opens late
// still gets its whole watch. The Dispatch here holds the upgrade for 2.5 s from the moment the
// probe asks for it, so the open comes at least 2.5 s after the dial, well past the 600 ms of
// `--minutes 0.01`, however fast the pod runs the probe. The row reads from the probe's own
// `open` line that the open came after the hold was let go, and proves the watch still
// subscribes, and still ends on its own with exit 0.
test(
  "the watch subscribes and ends on its own when its socket opens late",
  async () => {
    const dispatch = heldDispatch(2_500);
    const run = await runProbeToEnd(args(dispatch, TOKEN, "0.01"));
    expect(run.code).toBe(0);
    const opens = opensOf(run.stdout);
    expect(dispatch.released).toHaveLength(1);
    expect(opens).toHaveLength(1);
    expect(Date.parse(opens[0].at)).toBeGreaterThanOrEqual(dispatch.released[0]);
    expect(dispatch.received).toEqual([{ sub: "probe", type: "subscribe", view }]);
  },
  rowLimit("short")
);

// The other half of counting `--minutes` from the first open: a watch whose socket never opens
// still ends. The server here accepts the connection and never answers the upgrade, so the open
// that arms `--minutes` never comes, and the silence check, which acts only after a first open,
// never redials. The row proves that `--open-within 1` ends that watch on its own with exit 1, as
// a refused connection ends it, and not as a clean watch.
test(
  "a watch whose socket never opens ends on its own, and not as a clean watch",
  async () => {
    const silent = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      // Accepts every connection and never answers it: an upgrade that hangs.
      socket: { data() {}, open() {} },
    });
    try {
      const argv = args({ url: `http://127.0.0.1:${silent.port}` }, TOKEN, "0.01");
      argv.push("--open-within", "1");
      const run = await runProbeToEnd(argv);
      expect(run.code).toBe(1);
    } finally {
      silent.stop(true);
    }
  },
  rowLimit("short")
);

test(
  "a missing flag is a usage error, exit 2",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    const run = await runProbeToEnd(args(dispatch).slice(0, 4));
    expect(run.code).toBe(2);
    expect(run.stderr).toContain("--minutes is required");
    expect(dispatch.upgrades).toEqual([]);
  },
  rowLimit("short")
);

/** A Dispatch on a loopback port of its own that sends each subscribe the fixture snapshot and
 *  answers each GET with `answer`, and the paths read. Bun.serve closes a connection silent for
 *  10 s unless told not to, which would answer a read with a reset; the fake Dispatch keeps that
 *  default, so the rows that hold a read open use this one. */
function slowDispatch(answer: (url: URL) => Response | Promise<Response>) {
  const reads: string[] = [];
  const server = Bun.serve({
    fetch(request, srv) {
      const url = new URL(request.url);
      if (url.pathname === "/ws/live") {
        return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
      }
      reads.push(url.pathname);
      return answer(url);
    },
    hostname: "127.0.0.1",
    idleTimeout: 0,
    port: 0,
    websocket: {
      message(socket, raw) {
        // The probe's own frame, a subscribe.
        const frame = JSON.parse(String(raw)) as LiveClientFrame;
        if (frame.type === "subscribe") socket.send(forSub(snapshot, frame.sub));
      },
    },
  });
  kills.push(() => server.stop(true));
  return { reads, url: `http://127.0.0.1:${server.port}` };
}

/** The probe's argv for a watch of `url` that compares once it ends, each read given
 *  `readWithin` seconds of silence. */
function compareArgs(url: string, readWithin: string): string[] {
  return [...args({ url }), "--compare", "--read-within", readWithin];
}

// A `--compare` read that receives nothing for `--read-within` fails the probe, naming its route,
// where without that bound the probe waits on the read for good.
test(
  "a --compare read that receives nothing fails the probe with exit 1, naming the route",
  async () => {
    // Takes every GET and never answers it.
    const dispatch = slowDispatch(() => new Promise<Response>(() => {}));
    const run = runProbe(compareArgs(dispatch.url, "1"));
    await run.until(() => run.lines.some((line) => line.type === "snapshot"));
    run.kill("SIGINT");
    expect(await run.exited()).toBe(1);
    expect(dispatch.reads).toEqual(["/api/v1/delivery/timeline"]);
    const stderr = run.stderr.join("\n");
    expect(stderr).toContain("the read of /api/v1/delivery/timeline?");
    expect(stderr).toContain("received nothing for 1 s (--read-within)");
  },
  {
    // This row drives the probe: it waits for the snapshot line and sends SIGINT, and the read
    // runs after that. A bound at the child would kill a process this row still uses.
    timeout: 30_000,
  }
);

// A body that arrives in 16 pieces, 0.25 s apart, is read whole and compared. The read gets the
// page's own deadline, `REQUEST_DEADLINE_MS`, so no stall of the pod between two pieces turns
// the verdict. That the deadline starts over on the headers and on each piece, so a body longer
// than the deadline is read whole, is `silence-read.test.ts`'s, on Bun's fake timers.
test(
  "a --compare read whose body keeps arriving is read whole, however long it takes",
  async () => {
    const text = JSON.stringify(heldBody);
    const count = 16;
    const pieces = Array.from({ length: count }, (_, index) =>
      text.slice((index * text.length) / count, ((index + 1) * text.length) / count)
    );
    const dispatch = slowDispatch((url) => {
      if (url.pathname === "/api/v1/delivery/measures") return Response.json(heldBody.measures);
      let sent = 0;
      const encoder = new TextEncoder();
      const body = new ReadableStream<Uint8Array>({
        async pull(controller) {
          // A real gap between pieces, so the body reaches the probe, a process of its own, as
          // pieces; no verdict here reads how long the gap took.
          if (sent > 0) await Bun.sleep(250);
          controller.enqueue(encoder.encode(pieces[sent] ?? ""));
          sent += 1;
          if (sent === pieces.length) controller.close();
        },
      });
      return new Response(body, { headers: { "content-type": "application/json" } });
    });
    const run = runProbe(compareArgs(dispatch.url, String(REQUEST_DEADLINE_MS / 1000)));
    await run.until(() => run.lines.some((line) => line.type === "snapshot"));
    run.kill("SIGINT");
    expect(await run.exited(), run.stderr.join("\n")).toBe(0);
    expect(dispatch.reads).toEqual(["/api/v1/delivery/timeline", "/api/v1/delivery/measures"]);
    expect(comparisons(run.lines).map((line) => [line.route.split("?")[0], line.equal])).toEqual([
      ["/api/v1/delivery/timeline", true],
      ["/api/v1/delivery/measures", true],
    ]);
  },
  {
    // This row drives the probe: it waits for the snapshot line and sends SIGINT, and the read
    // runs after that. The 60 s, two `REQUEST_DEADLINE_MS`, covers the wait for the snapshot,
    // which the Dispatch here sends at the subscribe, and a read of about 4 s of pieces, in
    // which any silence fails the probe by name at 30 s, half this limit.
    timeout: 2 * REQUEST_DEADLINE_MS,
  }
);

// A delay longer than 2^31-1 ms does not fit `setTimeout`'s 32-bit field, and bun sets it to
// 1 ms, which would end a watch at once. Each flag that sets a timer refuses a value that long
// with a usage error that names the largest value it takes.
for (const [flag, value, refusal] of [
  ["--minutes", "40000", "--minutes is at most 35791.394116 minutes"],
  ["--open-within", "3000000", "--open-within is at most 2147483.647 seconds"],
  ["--read-within", "3000000", "--read-within is at most 2147483.647 seconds"],
] as const) {
  test(
    `${flag} longer than a timer keeps is a usage error, exit 2`,
    async () => {
      const dispatch = probeDispatch({ onSubscribe() {} });
      const argv =
        flag === "--minutes" ? args(dispatch, TOKEN, value) : [...args(dispatch), flag, value];
      const run = await runProbeToEnd(argv);
      expect(run.code).toBe(2);
      expect(run.stderr).toContain(refusal);
      expect(dispatch.upgrades).toEqual([]);
    },
    rowLimit("short")
  );
}
