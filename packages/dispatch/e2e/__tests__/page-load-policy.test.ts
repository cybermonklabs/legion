/** The rules of the production script, `e2e/acceptance/page-load.ts`, that need no browser to
 *  read: what it does with each request the page makes (a GET to the Dispatch host goes on with the
 *  token, a GET to another host goes on unchanged, any other method is failed in the browser and
 *  counted, and the identity read is answered in the browser), what it does with each socket the
 *  page opens (the handshake to the Dispatch host carries the token, one to another host does not)
 *  and which frames the page may send on it, the read of the event head, the verdict over five
 *  loads at the limits of `c-production`, the lines it prints, none of which holds the token, the
 *  bound on each wait of a load, at which a load stops and the run goes on, and what it needs
 *  before it starts a browser. The script itself drives a browser against a running Dispatch,
 *  which these cases do not start: a bounded load's page is a stand-in whose waits end at once or
 *  never. */
import { expect, test } from "bun:test";

import type { LiveClientFrame } from "../../web/src/api/types";
import {
  ACT_LIMIT_MS,
  AFTER_LOAD_ACTS,
  type AnsweredRead,
  boundedLoad,
  type CommentsRead,
  commentsReads,
  type DialSocket,
  type EndReading,
  FIRST_SCREEN_LIMIT_MS,
  type FirstContent,
  failures,
  LOAD_BOUNDS,
  type LoadBound,
  type LoadBounds,
  type LoadReading,
  type LoggedRequest,
  type PageLoadRecord,
  type PageSteps,
  type PolicyRoute,
  type PolicySocket,
  printedLines,
  READER_LOGIN,
  type RoutedFrame,
  type RoutedRequest,
  type RoutedSocket,
  readEventHead,
  readSettings,
  recordLines,
  requestPolicy,
  socketFrameAllowed,
  socketPolicy,
} from "../acceptance/page-load";
import { boundWord, LAYOUT_SHIFT } from "../issue-page-load";
import { type ActReading, actReading, type FrameEntry } from "../load-instruments";
import whoami from "./fixtures/page-load/whoami.json";

/** The Dispatch host a run loads, and the token it sends there. */
const DISPATCH = "http://dispatch.example";
const TOKEN = "e2e-token-of-the-policy-test";

/** The page's live socket on the Dispatch host. */
const LIVE = "ws://dispatch.example/ws/live";

/** A frame the rule lets go on, and one it refuses. */
const SUBSCRIBE = '{"type":"subscribe","sub":"s1","view":{"kind":"events"}}';
const PUBLISH = '{"type":"publish","sub":"s1","body":"a write"}';

/** What the policy did with one request, with what it passed. */
type Done =
  | { readonly call: "abort"; readonly options: string | undefined }
  | { readonly call: "continue"; readonly options: Parameters<PolicyRoute["continue"]>[0] }
  | { readonly call: "fulfill"; readonly options: Parameters<PolicyRoute["fulfill"]>[0] };

/** One request as Playwright hands it to the policy, and each thing the policy did with it. */
function routeOf(method: string, url: string): { readonly done: Done[]; route: PolicyRoute } {
  const done: Done[] = [];
  return {
    done,
    route: {
      abort: async (errorCode) => {
        done.push({ call: "abort", options: errorCode });
      },
      continue: async (options) => {
        done.push({ call: "continue", options });
      },
      fulfill: async (options) => {
        done.push({ call: "fulfill", options });
      },
      request: () => ({
        headers: () => ({ accept: "application/json" }),
        method: () => method,
        url: () => url,
      }),
    },
  };
}

/** The request policy of one run, its event head 41, and every request it was handed. */
function policyOf(): {
  readonly policy: (route: PolicyRoute) => Promise<void>;
  readonly routed: RoutedRequest[];
} {
  const routed: RoutedRequest[] = [];
  return {
    policy: requestPolicy(DISPATCH, TOKEN, "41", (request) => routed.push(request)),
    routed,
  };
}

/** The page's side of one socket as Playwright hands it to the socket route: what the route sent
 *  the page, how it closed it, and what went to a server Playwright opened from the page; and the
 *  page's own frames and close, which a case sends through `page`. */
function pageSocketOf(url: string) {
  const page: {
    close?: (code: number | undefined, reason: string | undefined) => void;
    message?: (message: string | Buffer) => void;
  } = {};
  const seen = {
    closed: [] as { readonly code?: number; readonly reason?: string }[],
    connected: false,
    toPage: [] as (string | Buffer)[],
    toServer: [] as (string | Buffer)[],
  };
  const socket: PolicySocket = {
    close: async (options) => {
      seen.closed.push(options ?? {});
    },
    connectToServer: () => {
      seen.connected = true;
      return {
        send: (message) => {
          seen.toServer.push(message);
        },
      };
    },
    onClose: (handler) => {
      page.close = handler;
    },
    onMessage: (handler) => {
      page.message = handler;
    },
    send: (message) => {
      seen.toPage.push(message);
    },
    url: () => url,
  };
  return { page, seen, socket };
}

/** A dial that records each handshake it is asked for, and either refuses it with `refusal` or
 *  answers a server socket that records what it is sent and how it is closed; `server.peer` is
 *  what the route gave it to hand the server's messages and close to. */
function dialOf(refusal: string | null = null) {
  const dialed: { readonly headers: Readonly<Record<string, string>>; readonly url: string }[] = [];
  const server = {
    closed: [] as (number | undefined)[],
    peer: null as Parameters<DialSocket>[2] | null,
    sent: [] as (string | Buffer)[],
  };
  const dial: DialSocket = async (url, headers, peer) => {
    dialed.push({ headers, url });
    if (refusal !== null) throw new Error(refusal);
    server.peer = peer;
    return {
      close: (code) => {
        server.closed.push(code);
      },
      send: (message) => {
        server.sent.push(message);
      },
    };
  };
  return { dial, dialed, server };
}

/** The socket policy of one run over `dial`, and every socket and frame it recorded. */
function socketsOf(dial: DialSocket) {
  const frames: RoutedFrame[] = [];
  const sockets: RoutedSocket[] = [];
  const policy = socketPolicy(
    DISPATCH,
    TOKEN,
    { frame: (frame) => frames.push(frame), socket: (socket) => sockets.push(socket) },
    dial
  );
  return { frames, policy, sockets };
}

/** A frame of the page, stamped as shown at `shown`. */
function frame(shown: number): FrameEntry {
  return {
    at: shown - 9,
    foot: 120,
    height: 900,
    rows: true,
    shown,
    time: shown - 10,
    typeface: 0,
    viewRow: true,
  };
}

/** An act whose result needed no read, drawn `shown` ms after its event. */
function heldAct(shown: number): ActReading {
  return { act: "j", loading: shown, read: null, shown };
}

/** One load whose every reading stands at its limit of `c-production`, each act read as `act`,
 *  its first rows drawn 800 ms after the navigation, and no read of the comments list made. */
function loadAt(act: ActReading): LoadReading {
  return {
    acts: AFTER_LOAD_ACTS.map(() => act),
    commentsReads: [],
    firstDrawn: { after: 800, drew: "rows" },
    firstScreen: FIRST_SCREEN_LIMIT_MS,
    largestContentfulPaint: FIRST_SCREEN_LIMIT_MS,
    layoutShift: LAYOUT_SHIFT,
    longestTask: ACT_LIMIT_MS,
    shape: { bytes: 48_000, elements: 3_200, rows: 60 },
    stopped: null,
    unpainted: [],
  };
}

/** `load` with its first act read as `act`. */
function firstAct(load: LoadReading, act: ActReading): LoadReading {
  return { ...load, acts: load.acts.map((each, at) => (at === 0 ? act : each)) };
}

/** The read of an act that needed one. */
function readOf(act: ActReading): NonNullable<ActReading["read"]> {
  if (act.read === null) throw new Error(`${act.act} needed no read`);
  return act.read;
}

/** A run of `loads` whose page sent a GET with the token and the identity read, and opened its
 *  live socket with the token and sent one subscribe on it. */
function recordOf(loads: readonly LoadReading[], changes: Partial<PageLoadRecord> = {}) {
  return {
    converted: true,
    frames: [{ allowed: true, load: 1, text: SUBSCRIBE, url: LIVE }],
    loadavgAfter: "0.90 0.80 0.70 2/300 4000",
    loadavgBefore: "0.50 0.40 0.30 1/300 3990",
    loads,
    requests: [
      { load: 1, method: "GET", outcome: "sent-with-token", url: `${DISPATCH}/issues/ACME-1` },
      { load: 1, method: "GET", outcome: "answered-in-browser", url: `${DISPATCH}/auth/whoami` },
    ],
    sockets: [{ load: 1, outcome: "sent-with-token", reason: null, url: LIVE }],
    ...changes,
  } satisfies PageLoadRecord;
}

test("a GET to the Dispatch host goes on with the token, and a GET to any other host leaves with no Authorization header", async () => {
  const { policy, routed } = policyOf();
  const own = routeOf("GET", `${DISPATCH}/api/v1/issues/ACME-1/tree?view=conversation`);
  await policy(own.route);
  expect(own.done).toEqual([
    {
      call: "continue",
      options: { headers: { accept: "application/json", authorization: `Bearer ${TOKEN}` } },
    },
  ]);

  // Another host, and the same name on another port or scheme, is not the Dispatch host: the
  // request goes on as the page made it, and the page put no Authorization header on it.
  for (const url of [
    "https://fonts.example/face.woff2",
    "http://dispatch.example:8080/api/v1/issues/ACME-1",
    "https://dispatch.example/api/v1/issues/ACME-1",
  ]) {
    const other = routeOf("GET", url);
    await policy(other.route);
    expect(other.done).toEqual([{ call: "continue", options: undefined }]);
    expect(Object.keys(other.route.request().headers())).not.toContain("authorization");
  }
  expect(routed.map((request) => request.outcome)).toEqual([
    "sent-with-token",
    "sent-unchanged",
    "sent-unchanged",
    "sent-unchanged",
  ]);
});

test("a request that is not a GET is failed before it leaves the browser, counted, and fails the run by its name", async () => {
  const { policy, routed } = policyOf();
  for (const method of ["POST", "PUT", "PATCH", "DELETE", "HEAD"]) {
    const write = routeOf(method, `${DISPATCH}/api/v1/whoami`);
    await policy(write.route);
    expect(write.done).toEqual([{ call: "abort", options: "blockedbyclient" }]);
  }
  // Another host's write is failed too: the rule is the method's, whatever the host.
  const elsewhere = routeOf("POST", "https://collector.example/events");
  await policy(elsewhere.route);
  expect(elsewhere.done).toEqual([{ call: "abort", options: "blockedbyclient" }]);
  expect(routed.every((request) => request.outcome === "failed-in-browser")).toBe(true);

  const loads = Array.from({ length: 5 }, () => loadAt(heldAct(ACT_LIMIT_MS)));
  const post: LoggedRequest = {
    load: 3,
    method: "POST",
    outcome: "failed-in-browser",
    url: `${DISPATCH}/api/v1/whoami`,
  };
  const record = recordOf(loads, { requests: [...recordOf(loads).requests, post] });
  const lines = recordLines(record);
  expect(lines).toContain(
    `request failed-in-browser POST ${DISPATCH}/api/v1/whoami count=1 loads=3`
  );
  expect(lines).toContain("requests=3 failed-in-browser=1");
  expect(failures(record)).toEqual([
    `POST ${DISPATCH}/api/v1/whoami: failed in the browser in load 3`,
  ]);
  expect(lines.at(-1)).toBe(
    `page-load: fail: POST ${DISPATCH}/api/v1/whoami: failed in the browser in load 3`
  );
});

test("the identity read is answered in the browser with the keys of AuthenticatedUser, the reader's login and the event head read before the load", async () => {
  const { policy, routed } = policyOf();
  const identity = routeOf("GET", `${DISPATCH}/auth/whoami`);
  await policy(identity.route);
  const [answered, ...rest] = identity.done;
  if (answered?.call !== "fulfill") throw new Error("the identity read was not answered");
  expect(rest).toEqual([]);
  expect({ contentType: answered.options.contentType, status: answered.options.status }).toEqual({
    contentType: "application/json",
    status: 200,
  });
  const body: unknown = JSON.parse(answered.options.body);
  expect(Object.keys(body ?? {}).sort()).toEqual(Object.keys(whoami).sort());
  expect(body).toEqual({ ...whoami, event_head: "41" });
  expect(READER_LOGIN).toBe(whoami.login);
  expect(routed).toEqual([
    { method: "GET", outcome: "answered-in-browser", url: `${DISPATCH}/auth/whoami` },
  ]);

  // The same path on another host is no identity read of this Dispatch.
  const elsewhere = routeOf("GET", "https://other.example/auth/whoami");
  await policy(elsewhere.route);
  expect(elsewhere.done).toEqual([{ call: "continue", options: undefined }]);
});

test("the event head is the since of the events_start Dispatch answers on a socket whose handshake carries the token", async () => {
  const { dial, dialed, server } = dialOf();
  const head = readEventHead(DISPATCH, TOKEN, dial);
  // A frame before the feed starts is no answer.
  server.peer?.message('{"type":"heartbeat"}');
  server.peer?.message(
    '{"type":"events_start","sub":"head","since":"1234","replay_complete":false}'
  );
  expect(await head).toBe("1234");
  expect(dialed).toEqual([{ headers: { authorization: `Bearer ${TOKEN}` }, url: LIVE }]);
  const [subscribe] = server.sent;
  expect(typeof subscribe === "string" ? JSON.parse(subscribe) : subscribe).toEqual({
    sub: "head",
    type: "subscribe",
    view: { kind: "events" },
  });
  expect(server.closed).toEqual([1000]);

  // A handshake Dispatch refuses leaves no head to read.
  const refusing = dialOf("the handshake of ws://dispatch.example/ws/live ended with 1002 ");
  await expect(readEventHead(DISPATCH, TOKEN, refusing.dial)).rejects.toThrow(/ended with 1002/);
});

test("a socket's handshake to the Dispatch host carries the token, one to another host carries none, and only a subscribe or an unsubscribe reaches the server", async () => {
  const { dial, dialed, server } = dialOf();
  const { frames, policy, sockets } = socketsOf(dial);
  const live = pageSocketOf(LIVE);
  await policy(live.socket);
  expect(dialed).toEqual([{ headers: { authorization: `Bearer ${TOKEN}` }, url: LIVE }]);
  expect(live.seen.connected).toBe(false);
  live.page.message?.(SUBSCRIBE);
  live.page.message?.(PUBLISH);
  live.page.message?.('{"type":"unsubscribe","sub":"s1"}');
  expect(server.sent).toEqual([SUBSCRIBE, '{"type":"unsubscribe","sub":"s1"}']);
  // What Dispatch sends reaches the page, and its close closes the page's socket with its code.
  server.peer?.message('{"type":"heartbeat"}');
  expect(live.seen.toPage).toEqual(['{"type":"heartbeat"}']);
  server.peer?.close(4001, "max age");
  expect(live.seen.closed).toEqual([{ code: 4001, reason: "max age" }]);
  // The page's own close closes the server's socket.
  live.page.close?.(1000, "idle");
  expect(server.closed).toEqual([1000]);

  // A socket to another host, or to the same name over another scheme, is the page's own, opened
  // from the page with no token, and the rule still holds its frames.
  for (const url of ["wss://other.example/socket", "wss://dispatch.example/ws/live"]) {
    const other = pageSocketOf(url);
    await policy(other.socket);
    expect(other.seen.connected).toBe(true);
    other.page.message?.(SUBSCRIBE);
    other.page.message?.(PUBLISH);
    expect(other.seen.toServer).toEqual([SUBSCRIBE]);
  }
  expect(dialed).toHaveLength(1);
  expect(sockets).toEqual([
    { outcome: "sent-with-token", reason: null, url: LIVE },
    { outcome: "sent-unchanged", reason: null, url: "wss://other.example/socket" },
    { outcome: "sent-unchanged", reason: null, url: "wss://dispatch.example/ws/live" },
  ]);
  expect(frames.map((sent) => sent.allowed)).toEqual([true, false, true, true, false, true, false]);
});

test("a socket the page closes before Dispatch accepts it is closed as it opens, and a refused handshake closes the page's socket and fails the run", async () => {
  const accepted = Promise.withResolvers<void>();
  const closed: (number | undefined)[] = [];
  const slow = socketsOf(async () => {
    await accepted.promise;
    return { close: (code) => closed.push(code), send: () => undefined };
  });
  const early = pageSocketOf(LIVE);
  const routed = slow.policy(early.socket);
  early.page.close?.(1000, "gone");
  accepted.resolve();
  await routed;
  expect(closed).toEqual([undefined]);

  const refusal = "the handshake of ws://dispatch.example/ws/live ended with 1002 ";
  const refusing = socketsOf(dialOf(refusal).dial);
  const refused = pageSocketOf(LIVE);
  await refusing.policy(refused.socket);
  expect(refused.seen.closed).toEqual([{ code: 1006, reason: "Dispatch refused the handshake" }]);
  expect(refusing.sockets).toEqual([{ outcome: "refused", reason: refusal, url: LIVE }]);

  const loads = Array.from({ length: 5 }, () => loadAt(heldAct(ACT_LIMIT_MS)));
  const record = recordOf(loads, {
    sockets: refusing.sockets.map((socket) => ({ ...socket, load: 2 })),
  });
  expect(recordLines(record)).toContain(`socket refused ${LIVE} count=1 loads=2: ${refusal}`);
  expect(failures(record)).toEqual([`socket ${LIVE}: refused in load 2: ${refusal}`]);
});

test("a subscribe and an unsubscribe pass the socket rule, and any other frame the page sends fails the run", () => {
  const sent: readonly LiveClientFrame[] = [
    { sub: "s1", type: "subscribe", view: { kind: "events" } },
    { resume: { since: "41" }, sub: "s2", type: "subscribe", view: { kind: "events" } },
    {
      sub: "s3",
      type: "subscribe",
      view: { kind: "agent", session_id: "00000000-0000-4000-8000-000000000001" },
    },
    { sub: "s1", type: "unsubscribe" },
  ];
  for (const frame of sent) expect(socketFrameAllowed(JSON.stringify(frame))).toBe(true);
  for (const frame of [
    PUBLISH,
    '{"type":"subscribe","view":{"kind":"events"}}',
    '{"type":"subscribe","sub":"s1"}',
    '{"type":"unsubscribe"}',
    "[]",
    "null",
    "not a frame",
    // A binary frame is no frame of the live socket, whatever its bytes read as.
    Buffer.from(JSON.stringify(sent[0])),
  ]) {
    expect(socketFrameAllowed(frame)).toBe(false);
  }

  const loads = Array.from({ length: 5 }, () => loadAt(heldAct(ACT_LIMIT_MS)));
  const refused = { allowed: false, load: 2, text: PUBLISH, url: LIVE };
  const record = recordOf(loads, { frames: [...recordOf(loads).frames, refused] });
  expect(recordLines(record)).toContain(`socket sent-with-token ${LIVE} count=1 loads=1`);
  expect(recordLines(record)).toContain("socket-frames sent=2 refused=1");
  expect(failures(record)).toEqual([`socket frame on ${LIVE} in load 2: ${PUBLISH}`]);
});

test("the verdict holds each limit and leaves the server's time out of a read", () => {
  // c-production's limits: 2,500 ms and 200 ms times the factor 1.5, and 0.1.
  expect([FIRST_SCREEN_LIMIT_MS, ACT_LIMIT_MS, LAYOUT_SHIFT]).toEqual([3_750, 300, 0.1]);

  // A press whose view needed a read that the server took 3.7 s to answer: the page showed it
  // took the act 300 ms after the event, sent the read 300 ms after it, and drew the rows 300 ms
  // after the answer. The 4.3 s from the event to the rows is no reading; the three parts are.
  const pressed = actReading(
    {
      acts: [
        {
          event: { timeStamp: 1_000, type: "pointerdown" },
          loading: 1,
          measured: true,
          name: "a press on Spec",
          result: 2,
        },
      ],
      frames: [frame(990), frame(1_300), frame(5_300)],
      resource: [
        {
          duration: 3_700,
          initiatorType: "fetch",
          name: `${DISPATCH}/api/v1/issues/ACME-1/tree?view=spec`,
          requestStart: 1_300,
          responseEnd: 5_000,
          responseStart: 4_999,
          startTime: 1_300,
        },
      ],
    },
    0
  );
  expect(pressed).toEqual({
    act: "a press on Spec",
    loading: 300,
    read: { drawn: 300, sent: 300, url: "/api/v1/issues/ACME-1/tree?view=spec" },
    shown: 4_300,
  });
  const atLimits = Array.from({ length: 5 }, () => loadAt(pressed));
  expect(failures(recordOf(atLimits))).toEqual([]);
  expect(recordLines(recordOf(atLimits)).at(-1)).toBe("page-load: pass");
  // An act that needed no read is held to its result's frame, at the same limit.
  const held = Array.from({ length: 5 }, () => loadAt(heldAct(ACT_LIMIT_MS)));
  expect(failures(recordOf(held))).toEqual([]);
  expect(failures(recordOf(held.map((load) => firstAct(load, heldAct(301)))))).toEqual([
    expect.stringMatching(/^after-load\.[a-z-]+\.result-ms: rate=1 readings=301,/),
  ]);

  const act = boundWord(AFTER_LOAD_ACTS[0]?.name ?? "");
  const over: readonly (readonly [string, (load: LoadReading) => LoadReading])[] = [
    [
      "first-screen.conversation.paint-ms",
      (load) => ({ ...load, firstScreen: FIRST_SCREEN_LIMIT_MS + 1 }),
    ],
    [`after-load.${act}.result-ms`, (load) => firstAct(load, { ...pressed, loading: 301 })],
    [
      `after-load.${act}.read-sent-ms`,
      (load) => firstAct(load, { ...pressed, read: { ...readOf(pressed), sent: 301 } }),
    ],
    [
      `after-load.${act}.read-drawn-ms`,
      (load) => firstAct(load, { ...pressed, read: { ...readOf(pressed), drawn: 301 } }),
    ],
    ["longest-task-ms", (load) => ({ ...load, longestTask: ACT_LIMIT_MS + 1 })],
    ["first-screen.conversation.layout-shift", (load) => ({ ...load, layoutShift: 0.11 })],
  ];
  const base = loadAt(pressed);
  for (const [bound, raise] of over) {
    // One load in five may read over a limit: the fourth smallest still meets it.
    expect(failures(recordOf([raise(base), base, base, base, base]))).toEqual([]);
    // Two may not, and the last line names the bound with its readings.
    const record = recordOf([raise(base), base, base, raise(base), base]);
    expect(failures(record)).toEqual([expect.stringMatching(new RegExp(`^${bound}: rate=1 `))]);
    expect(recordLines(record).at(-1)).toMatch(new RegExp(`^page-load: fail: ${bound}: rate=1 `));
  }

  // The largest contentful paint is no bound of c-production: the record prints its five readings
  // after the first screen's paint, with no limit and no verdict, and a load of any reading fails
  // nothing.
  const slowPaint = atLimits.map((load) => ({ ...load, largestContentfulPaint: 9_000 }));
  const lines = recordLines(recordOf(slowPaint));
  const paint = lines.findIndex((line) => line.startsWith("first-screen.conversation.paint-ms "));
  expect(lines[paint + 1]).toBe(
    "first-screen.conversation.largest-contentful-paint-ms rate=1 readings=9000,9000,9000,9000,9000"
  );
  expect(failures(recordOf(slowPaint))).toEqual([]);

  // A load that gave no reading counts as over every limit, so two such loads fail the run.
  const unread = { ...base, firstScreen: null, unpainted: ["comment c-1"] };
  expect(failures(recordOf([unread, unread, base, base, base]))).toEqual([
    "first-screen.conversation.paint-ms: rate=1 readings=none,none,3750,3750,3750 limit=3750 fourth=none",
  ]);
});

test("no line the script prints holds the token", () => {
  const loads = Array.from({ length: 5 }, (_, at) => ({
    ...loadAt(heldAct(ACT_LIMIT_MS)),
    stopped: at === 0 ? `load-end-wait: a read of ?token=${TOKEN} failed` : null,
  }));
  const record = recordOf(loads, {
    frames: [{ allowed: false, load: 1, text: `{"token":"${TOKEN}"}`, url: LIVE }],
    requests: [
      {
        load: 1,
        method: "GET",
        outcome: "sent-unchanged",
        url: `https://elsewhere.example/?t=${TOKEN}`,
      },
      { load: 2, method: "POST", outcome: "failed-in-browser", url: `${DISPATCH}/x/${TOKEN}` },
    ],
  });
  // The record holds the token in an address, a frame and a stop, so the printer has it to hide.
  expect(recordLines(record).filter((line) => line.includes(TOKEN))).toHaveLength(4);
  const printed = printedLines(record, TOKEN);
  expect(printed.filter((line) => line.includes(TOKEN))).toEqual([]);
  expect(printed).toContain(
    "request sent-unchanged GET https://elsewhere.example/?t=<the token> count=1 loads=1"
  );
  expect(printed).toContain("load 1 stopped: load-end-wait: a read of ?token=<the token> failed");
  expect(printed.at(-1)?.startsWith("page-load: fail: ")).toBe(true);
});

/** `LOAD_BOUNDS` with each of `keys`' bounds at 1 ms, which a wait that never settles reaches, and
 *  which a step that answers in the same turn of the event loop, as every fake step below does,
 *  beats. */
function boundsWith(...keys: (keyof LoadBounds)[]): LoadBounds {
  const bounds: Record<keyof LoadBounds, LoadBound> = { ...LOAD_BOUNDS };
  for (const key of keys) bounds[key] = { ...LOAD_BOUNDS[key], ms: 1 };
  return bounds;
}

/** A wait the page never ends. */
function pending<T>(): Promise<T> {
  return Promise.withResolvers<T>().promise;
}

/** A wall clock that answers `times` in turn. */
function clockOf(...times: number[]): () => number {
  return () => times.shift() ?? Number.NaN;
}

/** What a fake page reads as its load ends. */
const END: EndReading = {
  firstScreen: 1_200,
  largestContentfulPaint: 1_300,
  layoutShift: 0.01,
  longestTask: 80,
  shape: { bytes: 48_000, elements: 3_200, rows: 60 },
  unpainted: [],
};

/** The read of the comments list a fake page made. */
const COMMENTS: CommentsRead = {
  decoded: 5_300_000,
  encoded: 610_000,
  path: "/api/v1/issues/ACME-1/comments",
};

/** A page whose every wait ends at once, its first rows drawn 812 ms after the navigation, its
 *  load ended at 2,000 ms and its comments list read once, but those `changes` replaces; and
 *  each wait it was asked for. */
function pageOf(changes: Partial<PageSteps> = {}) {
  const calls: string[] = [];
  const steps: PageSteps = {
    firstContent: async () => {
      calls.push("first content");
      return { at: 812, kind: "rows", shown: 830 };
    },
    loadEnd: async () => {
      calls.push("load end");
      return 2_000;
    },
    makeAct: async (act, next) => {
      calls.push(act.name);
      return { last: next, next: next + 1 };
    },
    readActs: async (measured) => {
      calls.push(`read acts ${measured.join(",")}`);
      return { acts: measured.map(() => heldAct(120)), longestTask: 90 };
    },
    readComments: async () => {
      calls.push("read comments");
      return [COMMENTS];
    },
    readEnd: async (end) => {
      calls.push(`read end ${end}`);
      return END;
    },
    ...changes,
  };
  return { calls, open: async () => steps };
}

/** Every reading of a load that read none. */
function unreadOf(reading: LoadReading) {
  return {
    acts: reading.acts.filter((act) => act.shown !== null),
    readings: [
      reading.firstScreen,
      reading.largestContentfulPaint,
      reading.layoutShift,
      reading.longestTask,
      reading.shape,
    ],
  };
}

/** The line a load prints for the comments read of `COMMENTS`. */
const COMMENTS_LINE = `comments-read path=${COMMENTS.path} encoded=610000 decoded=5300000`;

test("a load read to its end makes each wait once, in order, and stops at no bound", async () => {
  expect(LOAD_BOUNDS.firstScreen).toEqual({ ms: 180_000, name: "first-screen-wait" });
  const { calls, open } = pageOf();
  const reading = await boundedLoad(open);
  expect(calls).toEqual([
    "first content",
    "load end",
    "read end 2000",
    "read comments",
    ...AFTER_LOAD_ACTS.map((act) => act.name),
    "load end",
    `read acts ${AFTER_LOAD_ACTS.map((_, at) => at).join(",")}`,
  ]);
  expect(reading).toEqual({
    ...END,
    acts: AFTER_LOAD_ACTS.map(() => heldAct(120)),
    commentsReads: [COMMENTS],
    firstDrawn: { after: 812, drew: "rows" },
    longestTask: 90,
    stopped: null,
  });
});

test("a load that draws neither a row of the view nor its empty line within its bound stops there, says so before its readings, reads none, and the run goes on", async () => {
  const drawn = Promise.withResolvers<FirstContent>();
  const { calls, open } = pageOf({ firstContent: () => drawn.promise });
  const stopped = await boundedLoad(open, boundsWith("firstScreen"), clockOf(1_000, 181_000));
  expect(stopped.firstDrawn).toEqual({ after: 180_000, drew: "no" });
  const why =
    "first-screen-wait: the wait for a row of the view or its empty line did not end within 1 ms";
  expect(stopped.stopped).toBe(why);
  expect(unreadOf(stopped)).toEqual({ acts: [], readings: [null, null, null, null, null] });
  // No wait after the first drawing was asked for, but the read of what the load read of the
  // comments list, which a load that drew nothing reads too.
  expect(calls).toEqual(["read comments"]);
  expect(stopped.commentsReads).toEqual([COMMENTS]);
  // The page's wait fails once the context closes under it, after its load has stopped: nothing
  // reads that failure, and it raises nothing.
  drawn.reject(new Error("Target page, context or browser has been closed"));

  const base = loadAt(heldAct(ACT_LIMIT_MS));
  const record = recordOf([stopped, stopped, base, base, base]);
  const lines = recordLines(record);
  const paint = lines.findIndex((line) => line.startsWith("first-screen.conversation.paint-ms "));
  const said = [
    "load 1 drew-first-screen=no after=180000",
    `load 1 ${COMMENTS_LINE}`,
    `load 1 stopped: ${why}`,
    "load 2 drew-first-screen=no after=180000",
    "load 3 drew-first-screen=rows after=800",
  ].map((line) => lines.indexOf(line));
  expect(said.every((at) => at !== -1 && at < paint)).toBe(true);
  expect(lines).toContain("page-shape.conversation rate=1 load=1 none");
  // Two loads that drew nothing fail the first screen's bound, as two loads that gave no reading
  // fail any; one may, as one load in five may read over any limit.
  expect(failures(record)).toContain(
    "first-screen.conversation.paint-ms: rate=1 readings=none,none,3750,3750,3750 limit=3750 fourth=none"
  );
  expect(lines.at(-1)?.startsWith("page-load: fail: first-screen.conversation.paint-ms: ")).toBe(
    true
  );
  expect(failures(recordOf([stopped, base, base, base, base]))).toEqual([]);
});

test("a load that drew Conversation's empty line drew its first screen: its paint is the line's, the acts on a row are not made, and every reading of a row reads none", async () => {
  const made: string[] = [];
  const { open } = pageOf({
    firstContent: async () => ({ at: 1_700, kind: "empty-line", shown: 1_716 }),
    makeAct: async (act, next) => {
      made.push(act.name);
      return { last: next, next: next + 1 };
    },
    // The page drew no row, so the first screen holds none and Element Timing paints none.
    readEnd: async () => ({ ...END, firstScreen: null }),
  });
  const line = await boundedLoad(open);
  expect(line.firstDrawn).toEqual({ after: 1_700, drew: "empty-line" });
  expect([line.firstScreen, line.stopped]).toEqual([1_716, null]);
  // The acts on a row are the fold, the unfold, `j` and `k`, which a page with no row has nothing
  // to make on; every other act is made, from where the act before it left the page.
  const onRows = AFTER_LOAD_ACTS.filter((act) => act.when === "rows");
  expect(onRows.map((act) => act.kind).sort()).toEqual(["fold", "next", "previous", "unfold"]);
  expect(made).toEqual(AFTER_LOAD_ACTS.filter((act) => act.when !== "rows").map((act) => act.name));
  expect(line.acts.map((act) => act.shown)).toEqual(
    AFTER_LOAD_ACTS.map((act) => (act.when === "rows" ? null : 120))
  );

  const base = loadAt(heldAct(ACT_LIMIT_MS));
  const lines = recordLines(recordOf([line, base, base, base, base]));
  expect(lines).toContain("load 1 drew-first-screen=empty-line after=1700");
  expect(lines).toContainEqual(
    expect.stringMatching(/^first-screen\.conversation\.paint-ms rate=1 readings=1716,3750,/)
  );
  // A load that drew the empty line is no freeze: five of them hold the first screen's bound, and
  // the acts on a row read none five times, as a row's readings do on a page that drew none.
  const all = failures(recordOf([line, line, line, line, line]));
  expect(all.some((failure) => failure.startsWith("first-screen."))).toBe(false);
  const fold = onRows[0];
  if (fold === undefined) throw new Error("no act after a load at 1440 px acts on a row");
  expect(all).toContain(
    `after-load.${boundWord(fold.name)}.result-ms: rate=1 readings=none,none,none,none,none ` +
      "limit=300 fourth=none"
  );
});

test("each read of the comments list is sized by Resource Timing, or by its answer where Resource Timing has none, and is printed by its path with its query cut", async () => {
  const url = `${DISPATCH}/api/v1/issues/ACME-1/comments`;
  const asked: string[] = [];
  const answer = (address: string, decoded: number, encoded: number): AnsweredRead => ({
    sizes: async () => {
      asked.push(address);
      return { decoded, encoded };
    },
    url: address,
  });
  const reads = await commentsReads(
    [
      { decodedBodySize: 5_300_000, encodedBodySize: 610_000, name: url },
      // The same address read again, its entry holding no body: its answer sizes it.
      { decodedBodySize: 0, encodedBodySize: 0, name: url },
    ],
    [
      answer(url, 1, 1),
      answer(url, 5_299_000, 609_000),
      // A read Resource Timing kept no entry of.
      answer(`${url}?artifact=spec`, 42_000, 9_000),
    ]
  );
  expect(reads).toEqual([
    COMMENTS,
    { ...COMMENTS, decoded: 5_299_000, encoded: 609_000 },
    { ...COMMENTS, decoded: 42_000, encoded: 9_000 },
  ]);
  // Only the answers Resource Timing could not size were read.
  expect(asked).toEqual([url, `${url}?artifact=spec`]);
  expect(await commentsReads([], [])).toEqual([]);

  const base = loadAt(heldAct(ACT_LIMIT_MS));
  const lines = recordLines(
    recordOf([
      { ...base, commentsReads: [COMMENTS] },
      { ...base, commentsReads: [] },
      { ...base, commentsReads: null },
      base,
      base,
    ])
  );
  expect(lines).toContain(`load 1 ${COMMENTS_LINE}`);
  expect(lines).toContain("load 2 comments-read none");
  expect(lines).toContain("load 3 comments-read unread");
});

test("a load that never ends, or whose end the page never lets the script read, stops at that bound and reads none", async () => {
  const endless = pageOf({ loadEnd: () => pending() });
  const unended = await boundedLoad(endless.open, boundsWith("loadEnd"));
  expect(endless.calls).toEqual(["first content", "read comments"]);
  expect(unended.firstDrawn).toEqual({ after: 812, drew: "rows" });
  expect(unended.stopped).toBe(
    "load-end-wait: the wait for the load's end did not end within 1 ms"
  );
  expect(unreadOf(unended)).toEqual({ acts: [], readings: [null, null, null, null, null] });

  const unread = pageOf({ readEnd: () => pending() });
  const unanswered = await boundedLoad(unread.open, boundsWith("read"));
  expect(unread.calls).toEqual(["first content", "load end", "read comments"]);
  expect(unanswered.stopped).toBe(
    "page-read-wait: the read of the load's end did not end within 1 ms"
  );
  expect(unreadOf(unanswered).readings).toEqual([null, null, null, null, null]);

  // A wait that fails on its own, within its bound, stops the load as well, named by its bound.
  const failing = pageOf({
    loadEnd: async () => {
      throw new Error("the load never had 1000 ms with no request in flight and no long task");
    },
  });
  expect((await boundedLoad(failing.open)).stopped).toBe(
    "load-end-wait: the wait for the load's end failed: the load never had 1000 ms with no " +
      "request in flight and no long task"
  );

  // A read of the comments reads that does not end leaves them unread, and names its bound.
  const silent = pageOf({ readComments: () => pending() });
  const uncounted = await boundedLoad(silent.open, boundsWith("read"));
  expect(uncounted.commentsReads).toBeNull();
  expect(uncounted.stopped).toBe(
    "page-read-wait: the read of the comments reads did not end within 1 ms"
  );
});

test("an act that does not end stops the acts at its bound, the acts made before it are read, and those after it read none", async () => {
  const stuck = AFTER_LOAD_ACTS[2];
  if (stuck === undefined) throw new Error("the acts after a load at 1440 px are fewer than three");
  const made: string[] = [];
  const { calls, open } = pageOf({
    makeAct: (act, next) => {
      made.push(act.name);
      return act === stuck ? pending() : Promise.resolve({ last: next, next: next + 1 });
    },
  });
  const reading = await boundedLoad(open, boundsWith("act"));
  expect(made).toEqual(AFTER_LOAD_ACTS.slice(0, 3).map((act) => act.name));
  expect(calls).toEqual([
    "first content",
    "load end",
    "read end 2000",
    "read comments",
    "read acts 0,1",
  ]);
  expect(reading.stopped).toBe(`act-wait: ${stuck.name} did not end within 1 ms`);
  expect(reading.acts.map((act) => act.shown)).toEqual(
    AFTER_LOAD_ACTS.map((_, at) => (at < 2 ? 120 : null))
  );
  expect([reading.firstScreen, reading.longestTask]).toEqual([1_200, 90]);

  // Where the read of the acts stops too, the record names both bounds.
  const twice = pageOf({ makeAct: () => pending(), readActs: () => pending() });
  expect((await boundedLoad(twice.open, boundsWith("act", "read"))).stopped).toBe(
    `act-wait: ${AFTER_LOAD_ACTS[0]?.name} did not end within 1 ms; then page-read-wait: the ` +
      "read of the acts did not end within 1 ms"
  );
});

test("a load whose setup does not end stops before its navigation, and says it drew nothing", async () => {
  const reading = await boundedLoad(() => pending(), boundsWith("setup"));
  expect([reading.firstDrawn, reading.commentsReads]).toEqual([null, null]);
  expect(reading.stopped).toBe("setup-wait: the load's setup did not end within 1 ms");
  const base = loadAt(heldAct(ACT_LIMIT_MS));
  const lines = recordLines(recordOf([reading, base, base, base, base]));
  expect(lines).toContain("load 1 drew-first-screen=no after=none");
  expect(lines).toContain("load 1 comments-read unread");
});

test("the script cannot run without DISPATCH_URL, an issue or a readable token file, and says why", () => {
  const env = { DISPATCH_TOKEN_FILE: "/run/token", DISPATCH_URL: `${DISPATCH}/` };
  const read = (path: string) => {
    if (path === "/run/token") return `  ${TOKEN}\n`;
    throw new Error(`ENOENT: no such file or directory, open '${path}'`);
  };
  // The bearer is the file's contents trimmed of surrounding whitespace, as lib.sh reads it.
  expect(readSettings(["--issue", "ACME-1"], env, read)).toEqual({
    chromium: undefined,
    issue: "ACME-1",
    origin: DISPATCH,
    token: TOKEN,
  });
  expect(
    readSettings(["--issue", "ACME-1", "--chromium", "/opt/chromium/chrome"], env, read)
  ).toMatchObject({ chromium: "/opt/chromium/chrome" });

  const reasons = [
    readSettings(["--issue", "ACME-1"], { ...env, DISPATCH_URL: undefined }, read),
    readSettings(["--issue", "ACME-1"], { ...env, DISPATCH_URL: "not a url" }, read),
    readSettings(["--issue", "ACME-1"], { ...env, DISPATCH_URL: "ws://dispatch.example" }, read),
    readSettings(["--issue", "ACME-1"], { ...env, DISPATCH_TOKEN_FILE: undefined }, read),
    readSettings(["--issue", "ACME-1"], { ...env, DISPATCH_TOKEN_FILE: "/run/absent" }, read),
    readSettings(["--issue", "ACME-1"], env, () => " \n"),
    readSettings([], env, read),
    readSettings(["--issue", "ACME-1", "--slow"], env, read),
  ].map((settings) => ("reason" in settings ? settings.reason.split("\n")[0] : settings));
  expect(reasons).toEqual([
    "set DISPATCH_URL to the Dispatch to load",
    "DISPATCH_URL is no http or https URL: not a url",
    "DISPATCH_URL is no http or https URL: ws://dispatch.example",
    "set DISPATCH_TOKEN_FILE to a file that holds the agent bearer",
    "DISPATCH_TOKEN_FILE names /run/absent, which cannot be read",
    "the agent bearer in DISPATCH_TOKEN_FILE is empty",
    "name the issue to load with --issue <KEY>",
    expect.stringContaining("--slow"),
  ]);
});
