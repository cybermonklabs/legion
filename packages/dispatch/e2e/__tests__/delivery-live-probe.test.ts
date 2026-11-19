import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
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
import { RECONNECT_SPREAD_MS, reconnectDelayMs } from "../../web/src/api/live-socket";
import type {
  DeliveryLiveDelta,
  DeliveryLiveSnapshot,
  DeliveryLiveView,
  DeliveryTimelineWithMeasures,
  LiveClientFrame,
} from "../../web/src/api/types";
import {
  MAX_TIMER_DELAY_MS,
  type ProbeCloseLine,
  type ProbeCompareLine,
  type ProbeEndLine,
  type ProbeHeld,
  type ProbeLine,
  type ProbeOpenLine,
  reconnectWaitMs,
} from "../delivery-live-probe";
import {
  beforeDeadline,
  bounds,
  named,
  pastDeadline,
  rowLimit,
  runBounded,
  watch,
  whether,
} from "./bounded-subprocess";

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
  /** What a failure calls the probe: this file, its command with its arguments, and its
   *  environment, as `named` writes a bounded child. */
  readonly named: string;
  readonly lines: readonly ProbeLine[];
  readonly stderr: readonly string[];
  /** Answers whether the probe has exited and both its output streams are read to their end. */
  readonly ended: () => boolean;
  /** Sends the probe `signal`, as a Ctrl-C does. */
  kill(signal: NodeJS.Signals): void;
  /** The exit code, once both output streams are read to their end. */
  exited(): Promise<number>;
}

const servers: FakeDispatchServer<LiveClientFrame>[] = [];
const kills: (() => void)[] = [];
const scratchDirs: string[] = [];

afterEach(() => {
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

/** The probe as the seven rows that drive it start it: a process of its own, which the row reads
 *  line by line and signals itself, so it is no bounded child, since a bound would kill a process
 *  the row still uses. Each of those rows waits for the line it needs through `lineBy` and for the
 *  probe's exit through `exitAfter`, and each of those waits ends at the short bound, failing with
 *  the name of what it waited for. `afterEach` kills the probe after every row. */
function runProbe(args: readonly string[]): Run {
  const argv = [process.execPath, probe, ...args];
  const child = Bun.spawn(argv, {
    cwd: dispatchPackage,
    stderr: "pipe",
    stdin: "ignore",
    stdout: "pipe",
  });
  kills.push(() => child.kill("SIGKILL"));
  const lines: ProbeLine[] = [];
  const stderr: string[] = [];
  const exit = Promise.all([
    // The probe writes one JSON object per line; the assertions read each as the line it names.
    readLines(child.stdout, (line) => lines.push(JSON.parse(line) as ProbeLine)),
    readLines(child.stderr, (line) => stderr.push(line)),
  ]).then(() => child.exited);
  return {
    ended: watch(exit),
    exited: () => exit,
    kill: (signal) => child.kill(signal),
    lines,
    named: named({ argv, from: import.meta.url }),
    stderr,
  };
}

/** What the probe wrote so far, as a failure quotes it: its lines as JSON, then its stderr. */
function wroteSoFar(run: Run): string {
  return `${JSON.stringify(run.lines)} ${run.stderr.join("\n")}`;
}

/** Waits, as `whether` does, until `holds` answers true for `run`. Where the probe ends first, or
 *  the short bound passes first, it fails naming `what`, the line the row waits for, beside the
 *  lines the probe wrote so far. */
async function lineBy(run: Run, what: string, holds: () => boolean): Promise<void> {
  if (await whether(holds, run.ended)) return;
  const wrote = wroteSoFar(run);
  if (run.ended()) {
    throw new Error(
      `the probe ended first, with exit ${await run.exited()}, before its ${what}: ${wrote}`
    );
  }
  throw new Error(
    `waited the short bound of ${bounds.short} ms for the probe's ${what}, and it never came: ` +
      wrote
  );
}

/** Sends the probe `signal`, as a Ctrl-C does, and answers its exit code once it has exited and
 *  both its output streams have ended. It waits for that as `whether` does, and where the short
 *  bound passes first it fails naming the probe, its argv, the signal it was sent and the bound,
 *  beside the lines the probe wrote so far. */
async function exitAfter(run: Run, signal: NodeJS.Signals): Promise<number> {
  run.kill(signal);
  if (!(await whether(run.ended))) {
    throw new Error(
      `the probe ${run.named} did not exit inside the short bound of ${bounds.short} ms after ` +
        `${signal}: ${wroteSoFar(run)}`
    );
  }
  return run.exited();
}

/** The probe as the eleven rows that wait for its exit alone start it, eight written out and three
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

/** Every line of the probe's whole stdout, each the JSON object it writes on one line. */
function linesOf(stdout: string): ProbeLine[] {
  return stdout
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as ProbeLine);
}

/** The `open` lines of the probe's whole stdout. */
function opensOf(stdout: string): ProbeOpenLine[] {
  return linesOf(stdout).filter((line): line is ProbeOpenLine => line.type === "open");
}

/** The `end` lines of the probe's whole stdout. */
function endsOf(stdout: string): ProbeEndLine[] {
  return linesOf(stdout).filter((line): line is ProbeEndLine => line.type === "end");
}

/** An hour: longer than any row of this file lasts, so the watch of a probe a row drives ends only
 *  on the SIGINT the row sends, and a probe that ignores it fails the row's wait for its exit by
 *  name at the short bound rather than ending its watch on its own at `--minutes`. */
const WATCH_MINUTES = "60";

function args(
  dispatch: { readonly url: string },
  token = TOKEN,
  minutes = WATCH_MINUTES
): string[] {
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
const closes = (lines: readonly ProbeLine[]) =>
  lines.filter((line): line is ProbeCloseLine => line.type === "close");
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
    await lineBy(run, "error line", () => run.lines.some((line) => line.type === "error"));
    expect(await exitAfter(run, "SIGINT")).toBe(0);

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
  // The wait for the error line, then the wait for the exit after SIGINT.
  rowLimit("short", "short")
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
    await lineBy(run, "delta line", () => run.lines.some((line) => line.type === "delta"));
    expect(await exitAfter(run, "SIGINT")).toBe(1);

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
  // The wait for the delta line, then the wait for the exit after SIGINT.
  rowLimit("short", "short")
);

/** The probe's `MAX_TIMER_DELAY_MS`, the longest wait a timer keeps, 2^31-1 ms or about 24.8
 *  days, which is the largest spread the probe takes: far above the rotation row's wait for its
 *  second subscribe, which ends at the short bound, so a reopen on a timer drawn from it reaches
 *  that subscribe inside the bound on at most about one draw in 35 800 (60 s over 24.8 days). */
const NO_TIMER_SPREAD_MS = MAX_TIMER_DELAY_MS;

// Once the first connection has opened, Dispatch's rotation (4001) is the one close the probe
// answers by opening again at once: its restart (1012) waits within the first attempt's spread,
// and any other close waits on the backoff. Two closes fall outside that: the close of a first
// connection that never opened, which ends the watch with no wait, and the probe's own
// replacement of a silent socket, which opens again at once.
//
// Two checks tell a rotation answered at once from one answered on the reconnect timer. The close
// line's `wait_ms`: the probe's timer path (`lost` in `delivery-live-probe.ts`) writes the wait it
// computed and the rotation branch writes null, so a probe that took the timer and reports it fails
// the line assertion whenever it reopens inside the short bound. And the spread,
// `NO_TIMER_SPREAD_MS`: a probe that writes null but still reopens on a timer drawn from it fails
// at the short bound, naming the second subscribe, on all but at most about one draw in 35 800.
// That rate is that probe's alone: one that reports the wait it drew fails on every draw, at the
// bound or on its line. A probe that is right arms no reconnect timer, so the spread costs it
// nothing, and the second subscribe is the fact that the reconnect happened.
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
      const run = runProbe([
        ...args(dispatch),
        "--reconnect-spread-ms",
        String(NO_TIMER_SPREAD_MS),
      ]);
      await lineBy(run, "second subscribe", () => dispatch.received.length === 2);
      expect(await exitAfter(run, "SIGINT")).toBe(0);

      expect(dispatch.received).toEqual([
        { sub: "probe", type: "subscribe", view },
        { resume: { revs: next.revs, seq: change.seq }, sub: "probe", type: "subscribe", view },
      ]);
      expect(
        run.lines.map((line) => {
          if (line.type === "open") return `open ${line.resume}`;
          if (line.type === "close") return `close ${line.code} wait ${line.wait_ms}`;
          return line.type;
        })
      ).toEqual([
        "open null",
        "snapshot",
        "delta",
        `close ${code} wait null`,
        `open ${change.seq}`,
      ]);
    },
    // The wait for the second subscribe, then the wait for the exit after SIGINT.
    rowLimit("short", "short")
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
    // A 50 ms spread, pinned by the option, so the wait is the probe's own draw from [0, 50) ms
    // and this row stays quick. The close line reports the wait the probe computed, so the row
    // holds that wait inside the spread by the probe's own report and not by a clock: a probe
    // that answered the restart at once, which writes null, or on a later attempt's backoff, at
    // least a second, fails it on every run. The backoff's own shape is held by its unit row ("the
    // first reconnect is spread over [0, 2 s); later ones back off within [d/2, d), capped at
    // 30 s", web/src/api/live-socket.test.ts), and the resume's content and order by the
    // assertions here.
    const run = runProbe([...args(dispatch), "--reconnect-spread-ms", "50"]);
    await lineBy(run, "second subscribe", () => dispatch.received.length === 2);
    expect(await exitAfter(run, "SIGINT")).toBe(0);

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
    expect(closes(run.lines)[0].wait_ms).toBeWithin(0, 50);
  },
  // The wait for the second subscribe, then the wait for the exit after SIGINT.
  rowLimit("short", "short")
);

// The two steps that decide a reconnect's wait, which this row holds on every run: the
// `--reconnect-spread-ms` the probe parsed reaching `reconnectWaitMs`, and a restart close setting
// the attempt counter back to 0. A spread of 0 makes the first attempt's wait exactly 0 ms, so a
// close line here reports 0 only while both hold. A probe that handed `reconnectWaitMs` the page's
// own `RECONNECT_SPREAD_MS` instead draws from [0, 2 s), and a probe that left the counter alone
// waits the attempt-1 backoff, at least a second; neither draws 0, which is why 0 is the value that
// tells the three apart. It takes two closes: the first lands on attempt 0 whatever the restart
// does with the counter, so the second is the counter's own, and the server sends no frame, since
// a frame resets that counter too.
test(
  "each close line reports the wait, which --reconnect-spread-ms 0 makes zero after a restart",
  async () => {
    const dispatch = probeDispatch({
      onSubscribe(socket, _sub, count) {
        if (count <= 2) socket.close(1012, "service restart");
      },
    });
    const run = runProbe([...args(dispatch), "--reconnect-spread-ms", "0"]);
    await lineBy(run, "second close line", () => closes(run.lines).length === 2);
    expect(await exitAfter(run, "SIGINT")).toBe(0);

    expect(closes(run.lines).map((line) => ({ code: line.code, wait_ms: line.wait_ms }))).toEqual([
      { code: 1012, wait_ms: 0 },
      { code: 1012, wait_ms: 0 },
    ]);
  },
  // The wait for the second close line, then the wait for the exit after SIGINT.
  rowLimit("short", "short")
);

test(
  "a refused upgrade ends the probe with exit 1, naming the refusal",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    const run = await runProbeToEnd(args(dispatch, "not-the-token"));
    expect(run.code).toBe(1);
    expect(dispatch.upgrades).toEqual(["Bearer not-the-token"]);
    expect(run.stderr).toContain("Dispatch refused the connection to ws://127.0.0.1:");
    expect(
      endsOf(run.stdout),
      "the probe wrote an end line when Dispatch refused its connection"
    ).toEqual([]);
    // A first connection Dispatch refuses writes one line, its close line, and no open line; that
    // line's wait_ms is null because the close ends the watch rather than starting a wait.
    expect(linesOf(run.stdout).map(({ at: _at, ...line }) => line)).toEqual([
      { code: 1006, reason: "", type: "close", wait_ms: null },
    ]);
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
    expect(
      endsOf(run.stdout),
      "the probe wrote an end line when Dispatch refused its view"
    ).toEqual([]);
  },
  rowLimit("short")
);

// `--minutes` counts from the first open, and the row reads the probe's own `open` and `end` lines.
test(
  "the probe's own end line comes no sooner than --minutes after its open",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    const run = await runProbeToEnd(args(dispatch, TOKEN, "0.01"));
    expect(run.code).toBe(0);
    const opens = opensOf(run.stdout);
    const ends = endsOf(run.stdout);
    expect(
      opens,
      "the probe wrote no open line, or more than one, for its one connection"
    ).toHaveLength(1);
    expect(ends, "the probe wrote no end line when its watch stopped").toHaveLength(1);
    expect(
      Date.parse(ends[0].at) - Date.parse(opens[0].at),
      "the probe's own clock says its watch ended before 600 ms had passed since its open"
    ).toBeGreaterThanOrEqual(600);
    expect(dispatch.received).toEqual([{ sub: "probe", type: "subscribe", view }]);
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
// `open` line that the open came after the hold was let go, and from its `end` line that the
// watch ran at least 600 ms past that open; and it proves the watch still subscribes, and still
// ends on its own with exit 0.
test(
  "the watch subscribes and ends on its own when its socket opens late",
  async () => {
    const dispatch = heldDispatch(2_500);
    const run = await runProbeToEnd(args(dispatch, TOKEN, "0.01"));
    expect(run.code).toBe(0);
    const opens = opensOf(run.stdout);
    const ends = endsOf(run.stdout);
    expect(dispatch.released, "the held Dispatch let no upgrade go, or more than one").toHaveLength(
      1
    );
    expect(
      opens,
      "the probe wrote no open line, or more than one, for its one connection"
    ).toHaveLength(1);
    expect(Date.parse(opens[0].at)).toBeGreaterThanOrEqual(dispatch.released[0]);
    expect(ends, "the probe wrote no end line when its late-opening watch stopped").toHaveLength(1);
    expect(
      Date.parse(ends[0].at) - Date.parse(opens[0].at),
      "the probe's own clock says its late-opening watch ended before 600 ms had passed since its open"
    ).toBeGreaterThanOrEqual(600);
    expect(dispatch.received).toEqual([{ sub: "probe", type: "subscribe", view }]);
  },
  rowLimit("short")
);

// The other half of counting `--minutes` from the first open: a watch whose socket never opens
// still ends. The server here accepts the connection and never answers the upgrade, so the open
// that arms `--minutes` never comes, and the silence check, which acts only after a first open,
// never redials. The row proves that `--open-within 1` ends that watch on its own with exit 1, as
// a refused connection ends it, and not as a clean watch. Its stderr must name the flag's 1 s, so
// a probe that misreads the value fails here, as does an exit 1 for any other reason, a crash at
// load included. The server notes when it accepted the connection and when that connection
// closed, and the row holds the time between to at least half the flag's 1 s and under
// `heldUnderMs`. That time is read on the row's own silent server, from its accept of the
// probe's connection to that connection's close. The lower bound stands at half the timer rather
// than at the timer itself because the time between is the timer give or take the scheduling of
// the probe's dial and of this process's accept and close handlers, so load can shorten it as
// well as lengthen it: in 30 runs of this row alone under nine busy loops, three per CPU, on a
// pod of 3 CPUs on 2026-11-15, the lowest measured 952.0 ms. The upper bound follows the
// module's rule, three times the largest reading of its kind under load, in whichever shape gave
// it, rounded up to the next 100 ms. Under nine busy loops, three per CPU, on a pod of 3 CPUs on
// 2026-11-15, a correct 1 s timer's accept-to-close reached 1 123.0 ms in those 30 runs of this
// row alone, and 1 001.6-1 048.7 ms in 5 runs of the whole lane,
// `bun test --parallel --timeout 60000 e2e/__tests__`. The bound stands on the larger, the
// row-alone 1 123.0 ms: three times it is 3 369.0 ms, so the bound is 3 400 ms.
// So the row catches a timer set well short, one set several times too long and one left at the
// 45 s default; it does not catch one set between about 0.5 s and about 3.4 s, such as 2 s for
// 1 s, which held 1 987-2 029 ms.
const heldUnderMs = 3_400;

test(
  "a watch whose socket never opens ends on its own, and not as a clean watch",
  async () => {
    const accepted: number[] = [];
    const closed = Promise.withResolvers<number>();
    const silent = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      // Accepts every connection and never answers it: an upgrade that hangs.
      socket: {
        close() {
          closed.resolve(performance.now());
        },
        data() {},
        open() {
          accepted.push(performance.now());
        },
      },
    });
    try {
      const openWithinMs = 1_000;
      const argv = args({ url: `http://127.0.0.1:${silent.port}` }, TOKEN, "0.01");
      argv.push("--open-within", `${openWithinMs / 1_000}`);
      const run = await runProbeToEnd(argv);
      expect(run.code).toBe(1);
      expect(
        run.stderr,
        "the probe's stderr does not say the socket missed the 1 s that --open-within gave it"
      ).toContain("did not open within 1 s (--open-within)");
      expect(
        endsOf(run.stdout),
        "the probe wrote an end line when its socket never opened"
      ).toEqual([]);
      expect(
        accepted,
        "the silent server accepted no connection from the probe, or more than one"
      ).toHaveLength(1);
      // The probe has exited, so its connection is closed, and the close is on its way here; a
      // close that never comes fails by name rather than on the row's own clock.
      const close = await beforeDeadline(closed.promise, performance.now() + heldUnderMs);
      if (close === pastDeadline) {
        throw new Error(
          `the silent server saw no close of the probe's connection within ${heldUnderMs} ms of ` +
            "the probe's exit"
        );
      }
      const held = close - accepted[0];
      expect(
        held,
        `the probe gave up on its connection before ${openWithinMs / 2} ms, half the ` +
          `${openWithinMs} ms that --open-within gave it`
      ).toBeGreaterThanOrEqual(openWithinMs / 2);
      expect(
        held,
        `the probe held its connection for ${heldUnderMs} ms or longer, more than three times the ` +
          `largest hold, 1 123.0 ms, that a correct ${openWithinMs} ms timer showed under load, in ` +
          `30 runs of this row alone, against the ${openWithinMs} ms that --open-within gave it`
      ).toBeLessThan(heldUnderMs);
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

// --url may carry a user and a password. One that is no http or https URL with a host is a usage
// error naming its length and the cause the one function found, and the socket of one that
// carries a user part is named by --url's fixed phrase (`urlForText`) when Dispatch refuses it.
// The password is made for this run.
test(
  "a --url that is no http or https URL with a host is named by its length, and one with a user part without it",
  async () => {
    const password = crypto.randomUUID();
    const dispatch = probeDispatch({ onSubscribe() {} });
    const unreadable = `made-up-user:${password}@127.0.0.1:1`;
    const usage = await runProbeToEnd(
      args(dispatch).map((arg) => (arg === dispatch.url ? unreadable : arg))
    );
    expect({ code: usage.code, holds: usage.stderr.includes(password) }).toEqual({
      code: 2,
      holds: false,
    });
    expect(usage.stderr).toContain(
      `--url must be Dispatch's http or https address: its value is ${unreadable.length} characters long`
    );
    const withUser = dispatch.url.replace("http://", `http://made-up-user:${password}@`);
    const refused = await runProbeToEnd(
      args(dispatch, "not-the-token").map((arg) => (arg === dispatch.url ? withUser : arg))
    );
    expect({ code: refused.code, holds: refused.stderr.includes(password) }).toEqual({
      code: 1,
      holds: false,
    });
    expect(refused.stderr).toContain(`Dispatch refused the connection to --url ${URL_NOT_PRINTED}`);
  },
  // The usage error's probe, then the refused one's, one after the other.
  rowLimit("short", "short")
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

// A `--compare` read that receives nothing for `--read-within` fails the probe, naming its route
// and the time it ran out of, where without that bound the probe waits on the read for good. The
// read starts once SIGINT ends the watch, so the probe exits by itself after SIGINT only where the
// read gives up, and its stderr names the route and the 1 s the read was given. A probe whose read
// never gives up fails the row's wait for its exit by name, at the short bound. The timer's own
// value against `--read-within` is `silence-read.test.ts`'s, on Bun's fake clock.
test(
  "a --compare read that receives nothing ends the probe by itself after SIGINT with exit 1, naming the route",
  async () => {
    // Takes every GET and never answers it.
    const dispatch = slowDispatch(() => new Promise<Response>(() => {}));
    const run = runProbe(compareArgs(dispatch.url, "1"));
    await lineBy(run, "snapshot line", () => run.lines.some((line) => line.type === "snapshot"));
    expect(await exitAfter(run, "SIGINT")).toBe(1);
    expect(dispatch.reads).toEqual(["/api/v1/delivery/timeline"]);
    const stderr = run.stderr.join("\n");
    expect(stderr).toContain("the read of /api/v1/delivery/timeline?");
    expect(stderr).toContain("received nothing for 1 s (--read-within)");
  },
  // The wait for the snapshot line, then the wait for the exit after SIGINT.
  rowLimit("short", "short")
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
    await lineBy(run, "snapshot line", () => run.lines.some((line) => line.type === "snapshot"));
    expect(await exitAfter(run, "SIGINT"), run.stderr.join("\n")).toBe(0);
    expect(dispatch.reads).toEqual(["/api/v1/delivery/timeline", "/api/v1/delivery/measures"]);
    expect(comparisons(run.lines).map((line) => [line.route.split("?")[0], line.equal])).toEqual([
      ["/api/v1/delivery/timeline", true],
      ["/api/v1/delivery/measures", true],
    ]);
  },
  // The wait for the snapshot line, then the wait for the exit after SIGINT, which covers the
  // read: about 4 s of pieces, in which any silence fails the probe by name at its 30 s
  // `--read-within`, inside the short bound.
  rowLimit("short", "short")
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

test(
  "a --reconnect-spread-ms no timer can hold is a usage error, exit 2",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    // A wait of more than 2^31-1 ms does not fit a timer: the runtime clamps it to 1 ms, so a
    // spread whose draws pass that bound makes the probe reopen at once, the opposite of what the
    // option says. The probe refuses such a value rather than honouring it silently as its
    // opposite.
    const run = await runProbeToEnd([
      ...args(dispatch, TOKEN, "0.01"),
      "--reconnect-spread-ms",
      String(2 ** 31),
    ]);
    expect(run.code).toBe(2);
    expect(run.stderr).toContain("--reconnect-spread-ms");
    expect(dispatch.upgrades).toEqual([]);
  },
  rowLimit("short")
);

test(
  "a blank --reconnect-spread-ms is a usage error, exit 2",
  async () => {
    const dispatch = probeDispatch({ onSubscribe() {} });
    // `Number("  ")` is 0, and finite: without the guard's trim a blank value reads as a zero
    // spread, which means reopen at once — the option silently honoured as something the caller
    // never asked for.
    const run = await runProbeToEnd([
      ...args(dispatch, TOKEN, "0.01"),
      "--reconnect-spread-ms",
      "  ",
    ]);
    expect(run.code).toBe(2);
    expect(run.stderr).toContain("--reconnect-spread-ms");
    expect(dispatch.upgrades).toEqual([]);
  },
  rowLimit("short")
);

// What the probe's own `reconnectWaitMs` promises for whatever spread it is handed and for a later
// attempt; the close-line row above observes the parsed option reaching it, at a spread of 0.
// This row calls the helper itself. It catches both ways the scaling goes wrong: a draw missing
// the division by the page's own window, which makes the option mean something other than the
// milliseconds it names, and a scaled draw reaching an attempt it must not, which would bend a
// later attempt's backoff and jitter away from the page's. It drives the helper against the page's
// own function and spawns nothing, so it takes no limit.
test("--reconnect-spread-ms is the window the first attempt's wait is drawn from", () => {
  const DAY_MS = 24 * 3_600_000;
  // The first attempt waits the draw's share of the spread, whatever the spread is.
  for (const spreadMs of [RECONNECT_SPREAD_MS, DAY_MS, 5]) {
    for (const draw of [0, 0.25, 0.75]) {
      expect(reconnectWaitMs(0, spreadMs, () => draw)).toBe(draw * spreadMs);
    }
  }
  // At the page's own spread the probe waits what the page waits for the same draw.
  expect(reconnectWaitMs(0, RECONNECT_SPREAD_MS, () => 0.25)).toBe(reconnectDelayMs(0, () => 0.25));
  // A later attempt is the page's backoff and jitter, which the option never touches: two
  // spreads orders of magnitude apart leave the same draw's wait the page's own.
  for (const attempt of [1, 5]) {
    for (const draw of [0, 0.75]) {
      const page = reconnectDelayMs(attempt, () => draw);
      expect(reconnectWaitMs(attempt, DAY_MS, () => draw)).toBe(page);
      expect(reconnectWaitMs(attempt, 5, () => draw)).toBe(page);
    }
  }
});
