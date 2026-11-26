/** The rules of the production script, `e2e/acceptance/page-load.ts`, that need no browser to
 *  read: what it does with each request the page makes (a GET to the Dispatch host goes on with the
 *  token, a GET to another host goes on unchanged, any other method is failed in the browser and
 *  counted, and the identity read is answered in the browser), what it does with each socket the
 *  page opens (the handshake to the Dispatch host carries the token, one to another host does not)
 *  and which frames the page may send on it, the read of the event head, the verdict over five
 *  loads at the limits of `c-production`, the lines it prints, none of which holds the token, and
 *  what it needs before it starts a browser. The script itself drives a browser against a running
 *  Dispatch, which these cases do not start. */
import { expect, test } from "bun:test";

import type { LiveClientFrame } from "../../web/src/api/types";
import {
  ACT_LIMIT_MS,
  AFTER_LOAD_ACTS,
  type DialSocket,
  FIRST_SCREEN_LIMIT_MS,
  failures,
  type LoadReading,
  type LoggedRequest,
  type PageLoadRecord,
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
    viewRow: true,
  };
}

/** An act whose result needed no read, drawn `shown` ms after its event. */
function heldAct(shown: number): ActReading {
  return { act: "j", loading: shown, read: null, shown };
}

/** One load whose every reading stands at its limit of `c-production`, each act read as `act`. */
function loadAt(act: ActReading): LoadReading {
  return {
    acts: AFTER_LOAD_ACTS.map(() => act),
    error: null,
    firstScreen: FIRST_SCREEN_LIMIT_MS,
    largestContentfulPaint: FIRST_SCREEN_LIMIT_MS,
    layoutShift: LAYOUT_SHIFT,
    longestTask: ACT_LIMIT_MS,
    shape: { bytes: 48_000, elements: 3_200, rows: 60 },
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
    error: at === 0 ? `the load failed: a read of ?token=${TOKEN} timed out` : null,
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
  // The record holds the token in an address, a frame and an error, so the printer has it to hide.
  expect(recordLines(record).filter((line) => line.includes(TOKEN))).toHaveLength(4);
  const printed = printedLines(record, TOKEN);
  expect(printed.filter((line) => line.includes(TOKEN))).toEqual([]);
  expect(printed).toContain(
    "request sent-unchanged GET https://elsewhere.example/?t=<the token> count=1 loads=1"
  );
  expect(printed).toContain(
    "load 1 error: the load failed: a read of ?token=<the token> timed out"
  );
  expect(printed.at(-1)?.startsWith("page-load: fail: ")).toBe(true);
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
