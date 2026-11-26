/**
 * `bun e2e/acceptance/page-load.ts --issue <KEY> [--chromium <path>]`, from `packages/dispatch`
 * (or `bun packages/dispatch/e2e/acceptance/page-load.ts ...` from the repository's root), with
 * `DISPATCH_URL` naming a running Dispatch and `DISPATCH_TOKEN_FILE` a file that holds an agent
 * bearer: the production script of R5. It times five loads of the new issue page's Conversation of
 * the issue on that Dispatch, at 1440 by 900 px with no slowing, each in a new browser context so
 * that its cache is empty, and holds each bound to the limits of Verification v4's `c-production`.
 * It reads each bound with the load rows' own instruments (`e2e/load-instruments.ts`) and makes,
 * after each load has ended, the acts at 1440 px that Row 5 of `e2e/issue-page-load.e2e.ts` makes.
 *
 * **The browser.** Playwright's Chromium, headless, from `--chromium <path>`, else Playwright's own
 * build where it is installed, else `/usr/local/bin/chromium`, the issue pod's; with the pod's
 * memory flags, one renderer process and no back-forward cache or site isolation.
 *
 * **What the page may do.** Every request of a load goes through one route (`requestPolicy`). A
 * request that is not a GET is failed before it leaves the browser, and fails the run: a page
 * whose write the script blocked is not the page a reader gets. `GET /auth/whoami` of the Dispatch
 * host is answered in the browser for a reader's login, `probe@example`, in the shape of the
 * contract's `AuthenticatedUser`, with the event log's head as it stands just before the load
 * (`readEventHead`), so the page's feed resumes from it as a reader's does; and the new page is
 * turned on for that login before the app runs (`withIssuePage`). So the script uses no person's
 * credential. Every other GET to the Dispatch host goes on with `Authorization: Bearer <the
 * token>`, and a GET to any other host goes on as the page made it, with no token.
 *
 * **The sockets.** A route sees no WebSocket's handshake, so every socket of a load goes through
 * one WebSocket route (`socketPolicy`). Playwright's own way to the server from such a route
 * (`WebSocketRoute.connectToServer`) opens the socket from the page, with no header the script can
 * set, so a socket to the Dispatch host is opened by the script itself, with `Authorization:
 * Bearer <the token>` on its handshake, as an agent opens `GET /ws/live`
 * (`e2e/delivery-live-probe.ts`), and the route carries each frame between it and the page. The
 * page's socket opens once Dispatch has accepted the handshake, and closes as Dispatch closes it.
 * A socket to any other host is opened from the page as it made it, with no token. Each frame the
 * page sends is recorded, and one that is neither a `subscribe` nor an `unsubscribe` of the live
 * socket (`web/src/api/live-socket.ts`) goes nowhere and fails the run, since another frame could
 * be a write. A handshake Dispatch refuses fails the run too: the page then reconnects all load
 * long, and is not the page a reader gets.
 *
 * The token is read once, as `e2e/acceptance/lib.sh`'s `read_bearer` reads it, and is held in no
 * environment variable; no line the script prints holds it (`printedLines`).
 *
 * **What it prints**, on stdout, once the five loads are done, one fact a line, in this order:
 *
 * - `loadavg-before <the contents of /proc/loadavg>`, read before the first load, since the roles
 *   of an issue pod share its processor;
 * - `request <outcome> <method> <url> count=<n> loads=<the loads>` for each request the page made,
 *   by its outcome, method and address (`sent-with-token`, `sent-unchanged`, `answered-in-browser`
 *   or `failed-in-browser`), then `requests=<n> failed-in-browser=<n>`;
 * - `socket <outcome> <url> count=<n> loads=<the loads>` for each socket the page opened
 *   (`sent-with-token`, `sent-unchanged`, or `refused` with Dispatch's reason after it), then
 *   `socket-frames sent=<n> refused=<n>`, the frames the page sent and those the rule refused;
 * - `load <n> error: <message>` for a load that could not be read to its end, and
 *   `load <n> first-screen rows with no Element Timing entry: <rows>`, each where it happened;
 * - one reading line for each bound, as `readingLine` prints it
 *   (`<bound> rate=1 readings=<the five loads'> limit=<l> fourth=<v> <pass or fail>`): the paint
 *   of the first screen's rows, each act's three bounds, the longest task after the first drawing
 *   of the view row, and the layout shift; after the paint, the largest contentful paint as a line
 *   of its readings alone (`<name> rate=1 readings=<the five loads'>`), since `c-production` holds
 *   it to no limit;
 * - `page-shape.conversation rate=1 load=<n> rows=<r> elements=<e> bytes=<b>` for each load, the
 *   shape of what it drew as it ended, read as Row 1 of `c-load-1440` reads it;
 * - `converted=<true|false>`, the issue tree's `converted` as `GET /api/v1/issues/<KEY>/tree`
 *   answers it;
 * - `loadavg-after <the contents of /proc/loadavg>`, read after the last load;
 * - last, `page-load: pass`, or `page-load: fail: <the bound, request or socket>: <the readings>`,
 *   each failure joined to the next by `; `.
 *
 * It exits 0 on a pass, 1 on a fail, and 2 when it cannot run: no `--issue`, no `DISPATCH_URL`, a
 * token file that is missing, unreadable or empty, a tree read that does not answer, or no browser.
 * It says why on stderr, and it checks each of these but the browser's own start before it starts
 * one.
 */
import { existsSync, readFileSync } from "node:fs";
import { parseArgs } from "node:util";
import { type Browser, chromium, type Page } from "@playwright/test";

import type { AuthenticatedUser, LiveClientFrame } from "../../web/src/api/types";
import { presetPath } from "../../web/src/features/issue-page/issue-address";
import {
  ACT_MS,
  afterLoadOrder,
  boundWord,
  FIRST_SCREEN_MS,
  HEADER_SELECTORS,
  LAYOUT_SHIFT,
  LOAD_ACTS,
  LOAD_ROW_SELECTOR,
  LOADS,
  makeSteps,
  VIEW_ROW_SELECTOR,
} from "../issue-page-load";
import { withIssuePage } from "../issue-page-option";
import {
  type ActReading,
  actBounds,
  actReading,
  type BoundReading,
  boundHolds,
  firstScreenOf,
  firstScreenPaint,
  installInstruments,
  type LoadRequests,
  largestContentfulPaint,
  layoutShift,
  longestTaskAfterViewRow,
  pageShape,
  type Reading,
  readingLine,
  recordOf,
  waitForLoadEnd,
  watchLoadRequests,
} from "../load-instruments";

// ---------------------------------------------------------------------------------------------
// The limits, the page and the reader.
// ---------------------------------------------------------------------------------------------

/** The factor between a machine of an issue pod's size and CI's `chromium-load` leg, 1.5 as R5
 *  says until `c-factor` measures it; the production check's limits are R1's and R2's times it. */
const FACTOR = 1.5;

/** The limit on the first screen's paint: 3,750 ms. */
export const FIRST_SCREEN_LIMIT_MS = FIRST_SCREEN_MS * FACTOR;

/** The limit on each act's result, on each part of an act that needs a read, and on the longest
 *  task after the first drawing of the view row: 300 ms. */
export const ACT_LIMIT_MS = ACT_MS * FACTOR;

/** The processor's slowing: none, so the readings are the pod's own processor's. */
const RATE = 1;

/** The viewport of every load, `chromium-load`'s. */
const VIEWPORT = { height: 900, width: 1440 } as const;

/** The acts after each load, in the order Row 5 makes them at 1440 px. */
export const AFTER_LOAD_ACTS = afterLoadOrder(LOAD_ACTS[1440]);

/** The reader the page is signed in as. The login names no person. */
export const READER_LOGIN = "probe@example";

/** The identity read the page waits on before any other, which the route answers in the browser. */
const WHOAMI_PATH = "/auth/whoami";

/** The path of the live socket, on the Dispatch host. */
const LIVE_SOCKET_PATH = "/ws/live";

/** How long Dispatch may take to start the events subscription that reads its event head. */
const EVENT_HEAD_MS = 30_000;

/** The close code a page reads for a handshake that failed, which the route gives the page's
 *  socket where Dispatch refused the script's. */
const ABNORMAL_CLOSE = 1006;

/** The issue pod's Chromium, for a machine where Playwright's own is not installed. */
const POD_CHROMIUM = "/usr/local/bin/chromium";

/** The issue pod's memory flags: one renderer process, and neither the back-forward cache nor the
 *  site isolation that would start more. */
const POD_MEMORY_FLAGS = [
  "--renderer-process-limit=1",
  "--disable-features=BackForwardCache,IsolateOrigins,site-per-process",
];

/** How long the tree read may take before the script cannot run. */
const TREE_READ_MS = 120_000;

/** What a printed line shows where the record held the token. */
const TOKEN_MASK = "<the token>";

const USAGE = [
  "usage: bun e2e/acceptance/page-load.ts --issue <KEY> [--chromium <path>]",
  "  DISPATCH_URL          the Dispatch whose issue page is loaded, e.g. http://<host>",
  "  DISPATCH_TOKEN_FILE   a file that holds the agent bearer each GET to that host carries",
].join("\n");

// ---------------------------------------------------------------------------------------------
// What the page may send.
// ---------------------------------------------------------------------------------------------

/** What the route did with one request: failed it in the browser (not a GET), answered it there
 *  (the identity read), or let it go on to the Dispatch host with the token or to another host as
 *  the page made it. */
export type RequestOutcome =
  | "answered-in-browser"
  | "failed-in-browser"
  | "sent-unchanged"
  | "sent-with-token";

/** One request the route was handed. */
export interface RoutedRequest {
  readonly method: string;
  readonly outcome: RequestOutcome;
  readonly url: string;
}

/** One request of the run, by the load (1 to 5) that made it. */
export interface LoggedRequest extends RoutedRequest {
  readonly load: number;
}

/** What the route reads and does of Playwright's `Route`, which a test can stand in for. */
export interface PolicyRoute {
  abort(errorCode?: string): Promise<void>;
  continue(options?: { readonly headers?: Readonly<Record<string, string>> }): Promise<void>;
  fulfill(options: {
    readonly body: string;
    readonly contentType: string;
    readonly status: number;
  }): Promise<void>;
  request(): { headers(): Record<string, string>; method(): string; url(): string };
}

/** `url` parsed, null where it is no URL. */
function urlOf(url: string): URL | null {
  try {
    return new URL(url);
  } catch {
    return null;
  }
}

/** What the route does with a request of `method` to `url`, the Dispatch host being `origin`. */
function requestOutcome(method: string, url: string, origin: string): RequestOutcome {
  if (method !== "GET") return "failed-in-browser";
  const address = urlOf(url);
  if (address?.origin !== origin) return "sent-unchanged";
  return address.pathname === WHOAMI_PATH ? "answered-in-browser" : "sent-with-token";
}

/** The route of every request of a load, at every address: each request is told to `routed`, and
 *  then failed, answered or let go on as `requestOutcome` says. The identity read is answered for
 *  `READER_LOGIN` with `eventHead`, the event log's head as `readEventHead` read it just before
 *  the load, so the page's feed resumes from it as a reader's resumes from the head its own sign-in
 *  check read. A route also turns the browser's HTTP cache off, so a load reads nothing that a
 *  request before it fetched. */
export function requestPolicy(
  origin: string,
  token: string,
  eventHead: string,
  routed: (request: RoutedRequest) => void
): (route: PolicyRoute) => Promise<void> {
  const authorization = `Bearer ${token}`;
  const answer = JSON.stringify({
    event_head: eventHead,
    kind: "user",
    login: READER_LOGIN,
  } satisfies AuthenticatedUser);
  return async (route) => {
    const request = route.request();
    const method = request.method();
    const url = request.url();
    const outcome = requestOutcome(method, url, origin);
    routed({ method, outcome, url });
    switch (outcome) {
      case "failed-in-browser":
        return await route.abort("blockedbyclient");
      case "answered-in-browser":
        return await route.fulfill({ body: answer, contentType: "application/json", status: 200 });
      case "sent-with-token":
        return await route.continue({ headers: { ...request.headers(), authorization } });
      case "sent-unchanged":
        return await route.continue();
    }
  };
}

/** Whether the page may send `payload` on a socket: a text frame that is the live socket's
 *  `subscribe` (a `sub` and a `view` of some `kind`) or `unsubscribe` (a `sub`). */
export function socketFrameAllowed(payload: string | Buffer): boolean {
  if (typeof payload !== "string") return false;
  let frame: unknown;
  try {
    frame = JSON.parse(payload);
  } catch {
    return false;
  }
  if (typeof frame !== "object" || frame === null || !("sub" in frame) || !("type" in frame)) {
    return false;
  }
  if (typeof frame.sub !== "string") return false;
  if (frame.type === "unsubscribe") return true;
  return (
    frame.type === "subscribe" &&
    "view" in frame &&
    typeof frame.view === "object" &&
    frame.view !== null &&
    "kind" in frame.view &&
    typeof frame.view.kind === "string"
  );
}

/** What the socket route did with one socket the page opened: opened it to the Dispatch host
 *  itself with the token, had Dispatch refuse that handshake (`reason` says how), or let the page
 *  open it to another host as it made it. */
export type SocketOutcome = "refused" | "sent-unchanged" | "sent-with-token";

/** One socket the socket route was handed. */
export interface RoutedSocket {
  readonly outcome: SocketOutcome;
  /** Why Dispatch refused the handshake; null where it did not. */
  readonly reason: string | null;
  readonly url: string;
}

/** One socket of the run, by the load that opened it. */
export interface LoggedSocket extends RoutedSocket {
  readonly load: number;
}

/** One frame the page sent on a socket: whether the rule let it go on, and its text, a binary
 *  frame by its length. */
export interface RoutedFrame {
  readonly allowed: boolean;
  readonly text: string;
  readonly url: string;
}

/** One frame of the run, by the load that sent it. */
export interface SentFrame extends RoutedFrame {
  readonly load: number;
}

/** What the socket route reads and does of Playwright's `WebSocketRoute`, the page's side of one
 *  socket, which a test can stand in for. */
export interface PolicySocket {
  close(options?: { readonly code?: number; readonly reason?: string }): Promise<void>;
  connectToServer(): { send(message: string | Buffer): void };
  onClose(handler: (code: number | undefined, reason: string | undefined) => void): void;
  onMessage(handler: (message: string | Buffer) => void): void;
  send(message: string | Buffer): void;
  url(): string;
}

/** A socket the script opened to the Dispatch host. */
export interface ServerSocket {
  close(code?: number, reason?: string): void;
  send(message: string | Buffer): void;
}

/** Opens a socket to `url` whose handshake carries `headers`, and hands each message it receives
 *  and its close to `peer`. Answers once Dispatch accepted the handshake, and fails with why it
 *  did not. */
export type DialSocket = (
  url: string,
  headers: Readonly<Record<string, string>>,
  peer: {
    close(code: number, reason: string): void;
    message(message: string | Buffer): void;
  }
) => Promise<ServerSocket>;

// Bun's WebSocket client accepts request headers; the repository's DOM WebSocket type does not.
const BunWebSocket = WebSocket as unknown as new (
  url: string,
  options: { readonly headers: Readonly<Record<string, string>> }
) => WebSocket;

/** `DialSocket` with Bun's WebSocket client, as `e2e/delivery-live-probe.ts` opens `GET /ws/live`
 *  with an agent's bearer. Once the socket has closed, a close asked of it does nothing: the
 *  page's close of a socket the server closed comes back here with the server's code, which Bun's
 *  `close` would refuse. */
const dialSocket: DialSocket = (url, headers, peer) => {
  const accepted = Promise.withResolvers<ServerSocket>();
  const socket = new BunWebSocket(url, { headers });
  socket.binaryType = "arraybuffer";
  let opened = false;
  let ended = false;
  socket.addEventListener("open", () => {
    opened = true;
    accepted.resolve({
      close: (code, reason) => {
        if (!ended) socket.close(code, reason);
      },
      send: (message) => socket.send(message),
    });
  });
  socket.addEventListener("message", (event) => {
    peer.message(typeof event.data === "string" ? event.data : Buffer.from(event.data));
  });
  socket.addEventListener("error", () => {
    if (!opened) accepted.reject(new Error(`the handshake of ${url} failed`));
  });
  socket.addEventListener("close", (event) => {
    ended = true;
    if (opened) {
      peer.close(event.code, event.reason);
    } else {
      accepted.reject(
        new Error(`the handshake of ${url} ended with ${event.code} ${event.reason}`)
      );
    }
  });
  return accepted.promise;
};

/** The socket route of every socket of a load (`context.routeWebSocket`): each socket is told to
 *  `recorded`, and so is each frame the page sends on it, which goes on only where
 *  `socketFrameAllowed` lets it. A socket to the Dispatch host is opened by `dial`, with
 *  `Authorization: Bearer <token>` on its handshake, and the route carries the frames between it
 *  and the page; Playwright opens the page's side once this route has answered, so the page's
 *  socket opens when Dispatch has accepted the handshake, as it would, and where Dispatch refuses
 *  it, the page's socket closes as a failed handshake closes. A socket to any other host is
 *  opened from the page (`connectToServer`), with the page's own headers and no token. */
export function socketPolicy(
  origin: string,
  token: string,
  recorded: {
    frame(frame: RoutedFrame): void;
    socket(socket: RoutedSocket): void;
  },
  dial: DialSocket = dialSocket
): (socket: PolicySocket) => Promise<void> {
  const dispatch = origin.replace(/^http/, "ws");
  const headers = { authorization: `Bearer ${token}` };
  return async (socket) => {
    const url = socket.url();
    const passes = (message: string | Buffer) => {
      const allowed = socketFrameAllowed(message);
      const text =
        typeof message === "string" ? message : `<a binary frame of ${message.length} bytes>`;
      recorded.frame({ allowed, text, url });
      return allowed;
    };
    if (urlOf(url)?.origin !== dispatch) {
      recorded.socket({ outcome: "sent-unchanged", reason: null, url });
      const server = socket.connectToServer();
      socket.onMessage((message) => {
        if (passes(message)) server.send(message);
      });
      return;
    }
    // The page sends nothing before its side opens, which is after this route answers; it may
    // close its side before then, and the server's is closed as soon as it opens.
    let server: ServerSocket | null = null;
    let closed = false;
    socket.onMessage((message) => {
      if (passes(message)) server?.send(message);
    });
    socket.onClose((code, reason) => {
      closed = true;
      server?.close(code, reason);
    });
    try {
      server = await dial(url, headers, {
        close: (code, reason) => void socket.close({ code, reason }),
        message: (message) => socket.send(message),
      });
    } catch (error) {
      recorded.socket({ outcome: "refused", reason: messageOf(error), url });
      await socket.close({ code: ABNORMAL_CLOSE, reason: "Dispatch refused the handshake" });
      return;
    }
    recorded.socket({ outcome: "sent-with-token", reason: null, url });
    if (closed) server.close();
  };
}

/** The event log's head now: the `since` of the `events_start` with which Dispatch starts an
 *  events subscription that names no resume, on a socket of the script's own to the Dispatch host,
 *  closed once it has answered. */
export async function readEventHead(
  origin: string,
  token: string,
  dial: DialSocket = dialSocket
): Promise<string> {
  const head = Promise.withResolvers<string>();
  const timer = setTimeout(
    () => head.reject(new Error(`Dispatch started no events feed within ${EVENT_HEAD_MS} ms`)),
    EVENT_HEAD_MS
  );
  const opened = dial(
    `${origin.replace(/^http/, "ws")}${LIVE_SOCKET_PATH}`,
    { authorization: `Bearer ${token}` },
    {
      close: (code, reason) =>
        head.reject(
          new Error(`Dispatch closed the socket before the feed started: ${code} ${reason}`)
        ),
      message: (message) => {
        let frame: unknown;
        try {
          frame = typeof message === "string" ? JSON.parse(message) : null;
        } catch {
          head.reject(new Error(`Dispatch sent a frame that is no JSON: ${message}`));
          return;
        }
        if (
          typeof frame === "object" &&
          frame !== null &&
          "type" in frame &&
          frame.type === "events_start" &&
          "since" in frame &&
          typeof frame.since === "string"
        ) {
          head.resolve(frame.since);
        }
      },
    }
  );
  const subscribe: LiveClientFrame = { sub: "head", type: "subscribe", view: { kind: "events" } };
  void opened.then(
    (server) => server.send(JSON.stringify(subscribe)),
    (error: unknown) => head.reject(error)
  );
  try {
    return await head.promise;
  } finally {
    clearTimeout(timer);
    // The head is read, or its read has failed: the socket is closed once it has opened, and one
    // that never opens has nothing to close.
    void opened.then(
      (server) => server.close(1000),
      () => undefined
    );
  }
}

// ---------------------------------------------------------------------------------------------
// The readings and the verdict.
// ---------------------------------------------------------------------------------------------

/** The shape of what a load drew as it ended (`pageShape`). */
export interface PageShape {
  readonly bytes: number;
  readonly elements: number;
  readonly rows: number;
}

/** What one load read. A reading is null where the load gave none: a first screen with a row that
 *  has no Element Timing entry, or a load that failed before it read it (`error`). */
export interface LoadReading {
  /** Each act of `AFTER_LOAD_ACTS`, in its order. */
  readonly acts: readonly ActReading[];
  readonly error: string | null;
  readonly firstScreen: Reading;
  readonly largestContentfulPaint: Reading;
  readonly layoutShift: Reading;
  /** After the first drawing of the view row, over the load and the acts after it. */
  readonly longestTask: Reading;
  readonly shape: PageShape | null;
  /** The first screen's rows that have no Element Timing entry. */
  readonly unpainted: readonly string[];
}

/** What a run read, as it prints it. */
export interface PageLoadRecord {
  readonly converted: boolean;
  readonly frames: readonly SentFrame[];
  readonly loadavgAfter: string;
  readonly loadavgBefore: string;
  readonly loads: readonly LoadReading[];
  readonly requests: readonly LoggedRequest[];
  readonly sockets: readonly LoggedSocket[];
}

/** An act the load did not read. */
const UNREAD_ACT: ActReading = { act: null, loading: null, read: null, shown: null };

/** A load that read nothing. */
const UNREAD_LOAD: LoadReading = {
  acts: AFTER_LOAD_ACTS.map(() => UNREAD_ACT),
  error: null,
  firstScreen: null,
  largestContentfulPaint: null,
  layoutShift: null,
  longestTask: null,
  shape: null,
  unpainted: [],
};

/** The bounds of `c-production` over the five loads, in the order the record prints them, the
 *  first screen's paint first, each named as `c-load-1440` names it: the first screen's paint
 *  within 3,750 ms; each act's three bounds (`actBounds`) within 300 ms, which leave a read's own
 *  time, the server's, out; the longest task after the first drawing of the view row within
 *  300 ms; and the layout shift at most 0.1. */
function productionBounds(loads: readonly LoadReading[]): BoundReading[] {
  const bound = (name: string, limit: number, read: (load: LoadReading) => Reading) => ({
    bound: name,
    limit,
    rate: RATE,
    readings: loads.map(read),
  });
  return [
    bound("first-screen.conversation.paint-ms", FIRST_SCREEN_LIMIT_MS, (load) => load.firstScreen),
    ...AFTER_LOAD_ACTS.flatMap((act, at) =>
      actBounds(
        `after-load.${boundWord(act.name)}`,
        RATE,
        ACT_LIMIT_MS,
        loads.map((load) => load.acts[at] ?? UNREAD_ACT)
      )
    ),
    bound("longest-task-ms", ACT_LIMIT_MS, (load) => load.longestTask),
    bound("first-screen.conversation.layout-shift", LAYOUT_SHIFT, (load) => load.layoutShift),
  ];
}

/** The record's line of the largest contentful paint, which `c-production` holds to no limit:
 *  the first three words of the line `readingLine` prints, its name, rate and five readings,
 *  printed as every bound's are. */
function largestContentfulPaintLine(loads: readonly LoadReading[]): string {
  return readingLine({
    bound: "first-screen.conversation.largest-contentful-paint-ms",
    limit: Number.POSITIVE_INFINITY,
    rate: RATE,
    readings: loads.map((load) => load.largestContentfulPaint),
  })
    .split(" ")
    .slice(0, 3)
    .join(" ");
}

/** `loads`, in words. */
function inLoads(loads: readonly number[]): string {
  return `${loads.length === 1 ? "load" : "loads"} ${loads.join(", ")}`;
}

/** `items` grouped by `key`, in the order each key first came, with the loads each came in. */
function grouped<T extends { readonly load: number }>(
  items: readonly T[],
  key: (item: T) => string
): { readonly count: number; readonly first: T; readonly loads: number[] }[] {
  const groups = new Map<string, { count: number; first: T; loads: number[] }>();
  for (const item of items) {
    const group = groups.get(key(item)) ?? { count: 0, first: item, loads: [] };
    group.count += 1;
    if (!group.loads.includes(item.load)) group.loads.push(item.load);
    groups.set(key(item), group);
  }
  return [...groups.values()];
}

/** Each request the page made, by its outcome, method and address. */
function requestGroups(requests: readonly LoggedRequest[]) {
  return grouped(requests, (request) => `${request.outcome} ${request.method} ${request.url}`);
}

/** Each socket the page opened, by its outcome, address and refusal. */
function socketGroups(sockets: readonly LoggedSocket[]) {
  return grouped(sockets, (socket) => `${socket.outcome} ${socket.url} ${socket.reason}`);
}

/** What fails the run, each in the words the last line names it with: each request failed in the
 *  browser, each socket whose handshake Dispatch refused, each frame the socket rule refused, and
 *  each bound whose fourth smallest reading is over its limit, with its readings. */
export function failures(record: PageLoadRecord): string[] {
  const requests = requestGroups(
    record.requests.filter((request) => request.outcome === "failed-in-browser")
  ).map(
    ({ first, loads }) => `${first.method} ${first.url}: failed in the browser in ${inLoads(loads)}`
  );
  const sockets = socketGroups(record.sockets.filter((socket) => socket.outcome === "refused")).map(
    ({ first, loads }) => `socket ${first.url}: refused in ${inLoads(loads)}: ${first.reason}`
  );
  const frames = grouped(
    record.frames.filter((frame) => !frame.allowed),
    (frame) => `${frame.url} ${frame.text}`
  ).map(({ first, loads }) => `socket frame on ${first.url} in ${inLoads(loads)}: ${first.text}`);
  const bounds = productionBounds(record.loads)
    .filter((reading) => !boundHolds(reading))
    .map((reading) => {
      // The reading line less its bound's name and its verdict: the rate, readings, limit and
      // fourth smallest.
      const readings = readingLine(reading).slice(reading.bound.length + 1, -" fail".length);
      return `${reading.bound}: ${readings}`;
    });
  return [...requests, ...sockets, ...frames, ...bounds];
}

/** The record's lines, in the order the header of this file lists them, the verdict last. */
export function recordLines(record: PageLoadRecord): string[] {
  const failed = failures(record);
  const notes = record.loads.flatMap((load, at) => [
    ...(load.error === null ? [] : [`load ${at + 1} error: ${load.error}`]),
    ...(load.unpainted.length === 0
      ? []
      : [
          `load ${at + 1} first-screen rows with no Element Timing entry: ${load.unpainted.join(", ")}`,
        ]),
  ]);
  const [paint, ...bounds] = productionBounds(record.loads).map(readingLine);
  return [
    `loadavg-before ${record.loadavgBefore}`,
    ...requestGroups(record.requests).map(
      ({ count, first, loads }) =>
        `request ${first.outcome} ${first.method} ${first.url} count=${count} loads=${loads.join(",")}`
    ),
    `requests=${record.requests.length} failed-in-browser=${
      record.requests.filter((request) => request.outcome === "failed-in-browser").length
    }`,
    ...socketGroups(record.sockets).map(
      ({ count, first, loads }) =>
        `socket ${first.outcome} ${first.url} count=${count} loads=${loads.join(",")}` +
        (first.reason === null ? "" : `: ${first.reason}`)
    ),
    `socket-frames sent=${record.frames.length} refused=${
      record.frames.filter((frame) => !frame.allowed).length
    }`,
    ...notes,
    paint,
    largestContentfulPaintLine(record.loads),
    ...bounds,
    ...record.loads.map(
      ({ shape }, at) =>
        `page-shape.conversation rate=${RATE} load=${at + 1} ` +
        (shape === null
          ? "none"
          : `rows=${shape.rows} elements=${shape.elements} bytes=${shape.bytes}`)
    ),
    `converted=${record.converted}`,
    `loadavg-after ${record.loadavgAfter}`,
    failed.length === 0 ? "page-load: pass" : `page-load: fail: ${failed.join("; ")}`,
  ];
}

/** The lines the script prints: the record's, with the token masked wherever the page or the
 *  browser put it in an address, a frame or an error. */
export function printedLines(record: PageLoadRecord, token: string): string[] {
  return recordLines(record).map((line) => line.replaceAll(token, TOKEN_MASK));
}

// ---------------------------------------------------------------------------------------------
// The run.
// ---------------------------------------------------------------------------------------------

/** What a run needs before it starts a browser. */
export interface RunSettings {
  readonly chromium: string | undefined;
  readonly issue: string;
  /** The Dispatch host's origin, which `DISPATCH_URL` names. */
  readonly origin: string;
  readonly token: string;
}

function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** The run's settings from its arguments and environment, or why it cannot run. The token is
 *  `DISPATCH_TOKEN_FILE`'s contents trimmed of surrounding whitespace, and a file that cannot be
 *  read or holds only whitespace is refused, as `read_bearer` of `e2e/acceptance/lib.sh` refuses
 *  it. */
export function readSettings(
  argv: readonly string[],
  env: Readonly<Record<string, string | undefined>>,
  read: (path: string) => string = (path) => readFileSync(path, "utf8")
): RunSettings | { readonly reason: string } {
  let values: { readonly chromium?: string; readonly issue?: string };
  try {
    values = parseArgs({
      args: [...argv],
      options: { chromium: { type: "string" }, issue: { type: "string" } },
      strict: true,
    }).values;
  } catch (error) {
    return { reason: `${messageOf(error)}\n${USAGE}` };
  }
  if (values.issue === undefined || values.issue === "") {
    return { reason: `name the issue to load with --issue <KEY>\n${USAGE}` };
  }
  const address = env.DISPATCH_URL;
  if (address === undefined || address === "") {
    return { reason: `set DISPATCH_URL to the Dispatch to load\n${USAGE}` };
  }
  const dispatch = urlOf(address);
  if (dispatch === null || (dispatch.protocol !== "http:" && dispatch.protocol !== "https:")) {
    return { reason: `DISPATCH_URL is no http or https URL: ${address}` };
  }
  const file = env.DISPATCH_TOKEN_FILE;
  if (file === undefined || file === "") {
    return { reason: `set DISPATCH_TOKEN_FILE to a file that holds the agent bearer\n${USAGE}` };
  }
  let token: string;
  try {
    token = read(file).trim();
  } catch {
    return { reason: `DISPATCH_TOKEN_FILE names ${file}, which cannot be read` };
  }
  if (token === "") return { reason: "the agent bearer in DISPATCH_TOKEN_FILE is empty" };
  return { chromium: values.chromium, issue: values.issue, origin: dispatch.origin, token };
}

/** The issue tree's `converted`, as `GET /api/v1/issues/<KEY>/tree` answers it to the token, or
 *  why it could not be read. */
async function treeConverted(
  settings: RunSettings
): Promise<boolean | { readonly reason: string }> {
  const path = `/api/v1/issues/${encodeURIComponent(settings.issue)}/tree`;
  try {
    const response = await fetch(new URL(path, settings.origin), {
      headers: { authorization: `Bearer ${settings.token}` },
      // A redirect would carry the token to wherever it points.
      redirect: "error",
      signal: AbortSignal.timeout(TREE_READ_MS),
    });
    if (!response.ok) {
      return { reason: `GET ${path} answered ${response.status}: ${await response.text()}` };
    }
    const tree: unknown = await response.json();
    return typeof tree === "object" &&
      tree !== null &&
      "converted" in tree &&
      typeof tree.converted === "boolean"
      ? tree.converted
      : { reason: `GET ${path} answered no converted of true or false` };
  } catch (error) {
    return { reason: `GET ${path} on ${settings.origin} failed: ${messageOf(error)}` };
  }
}

/** The browser a run starts: `--chromium`'s, else Playwright's own build where it is installed,
 *  else the issue pod's. */
function chromiumPath(given: string | undefined): string {
  if (given !== undefined) return given;
  const own = chromium.executablePath();
  return existsSync(own) ? own : POD_CHROMIUM;
}

/** `/proc/loadavg`, or why it could not be read. */
function loadavg(): string {
  try {
    return readFileSync("/proc/loadavg", "utf8").trim();
  } catch (error) {
    return `unread: ${messageOf(error)}`;
  }
}

/** Makes every act of `AFTER_LOAD_ACTS` once on the page, as Row 5 makes them: each once the page
 *  has gone quiet again, from where the act before left it. Answers the index each act's measured
 *  step was armed under. */
async function makeActs(page: Page, requests: LoadRequests): Promise<number[]> {
  const settle = () => waitForLoadEnd(page, requests);
  const measured: number[] = [];
  let next = 0;
  for (const act of AFTER_LOAD_ACTS) {
    const made = await makeSteps(page, next, act.steps, "now", settle);
    measured.push(made.last);
    next = (await makeSteps(page, made.next, act.after, "now")).next;
  }
  await waitForLoadEnd(page, requests);
  return measured;
}

/** Reads one load of `page` from its navigation's commit: as it ends, its first screen's paint,
 *  its largest contentful paint, its layout shift and the shape of what it drew, as Row 1 of
 *  `c-load-1440` reads them; then the acts after it, and the longest task over both. A load that
 *  fails keeps what it read before the failure, and names the failure. */
async function readLoad(page: Page, requests: LoadRequests): Promise<LoadReading> {
  let reading = UNREAD_LOAD;
  try {
    const end = await waitForLoadEnd(page, requests);
    const screen = firstScreenPaint(await firstScreenOf(page));
    const shape = await pageShape(page);
    const atEnd = await recordOf(page);
    reading = {
      ...reading,
      firstScreen: screen.unpainted.length > 0 ? null : screen.reading,
      largestContentfulPaint: largestContentfulPaint(atEnd.lcp, end),
      layoutShift: layoutShift(atEnd.cls),
      longestTask: longestTaskAfterViewRow(atEnd),
      shape,
      unpainted: screen.unpainted,
    };
    const measured = await makeActs(page, requests);
    const last = await recordOf(page);
    reading = {
      ...reading,
      acts: measured.map((index) => actReading(last, index)),
      longestTask: longestTaskAfterViewRow(last),
    };
  } catch (error) {
    reading = { ...reading, error: messageOf(error) };
  }
  return reading;
}

/** What a run's loads record, each item by the load that made it. */
interface Sent {
  readonly frames: SentFrame[];
  readonly requests: LoggedRequest[];
  readonly sockets: LoggedSocket[];
}

/** Load `load` of five: a new context, so its cache is empty, with the request route, answering
 *  the identity read with the event log's head as it stands just before the load, the socket
 *  route, the reader's choice of the new page and the instruments installed before the
 *  navigation. */
async function measuredLoad(
  browser: Browser,
  settings: RunSettings,
  load: number,
  sent: Sent
): Promise<LoadReading> {
  const context = await browser.newContext({ viewport: VIEWPORT });
  try {
    await withIssuePage(context, READER_LOGIN);
    const head = await readEventHead(settings.origin, settings.token);
    await context.route(
      "**/*",
      requestPolicy(settings.origin, settings.token, head, (request) =>
        sent.requests.push({ ...request, load })
      )
    );
    await context.routeWebSocket(
      () => true,
      socketPolicy(settings.origin, settings.token, {
        frame: (frame) => sent.frames.push({ ...frame, load }),
        socket: (socket) => sent.sockets.push({ ...socket, load }),
      })
    );
    const page = await context.newPage();
    await installInstruments(page, {
      boxes: "none",
      header: HEADER_SELECTORS,
      named: null,
      rows: LOAD_ROW_SELECTOR,
      viewRows: VIEW_ROW_SELECTOR,
    });
    const requests = watchLoadRequests(page);
    const address = new URL(presetPath(settings.issue, "conversation"), settings.origin);
    await page.goto(address.href, { waitUntil: "commit" });
    return await readLoad(page, requests);
  } catch (error) {
    return { ...UNREAD_LOAD, error: messageOf(error) };
  } finally {
    await context.close();
  }
}

/** The run: its exit status, 0 on a pass, 1 on a fail, 2 where it cannot run. */
async function main(): Promise<number> {
  const settings = readSettings(Bun.argv.slice(2), process.env);
  if ("reason" in settings) {
    console.error(settings.reason);
    return 2;
  }
  // As `read_bearer` does, both of the bearer's names leave the environment, so the browser the
  // script starts inherits neither.
  delete process.env.DISPATCH_TOKEN;
  delete process.env.DISPATCH_TOKEN_FILE;
  const say = (text: string) => console.error(text.replaceAll(settings.token, TOKEN_MASK));
  const converted = await treeConverted(settings);
  if (typeof converted !== "boolean") {
    say(converted.reason);
    return 2;
  }
  const executable = chromiumPath(settings.chromium);
  if (!existsSync(executable)) {
    say(`no browser at ${executable}: name one with --chromium <path>`);
    return 2;
  }
  const loadavgBefore = loadavg();
  let browser: Browser;
  try {
    browser = await chromium.launch({
      args: POD_MEMORY_FLAGS,
      executablePath: executable,
      headless: true,
    });
  } catch (error) {
    say(`the browser at ${executable} did not start: ${messageOf(error)}`);
    return 2;
  }
  const sent: Sent = { frames: [], requests: [], sockets: [] };
  const loads: LoadReading[] = [];
  try {
    for (let load = 1; load <= LOADS; load += 1) {
      loads.push(await measuredLoad(browser, settings, load, sent));
    }
  } catch (error) {
    say(`the browser could not open load ${loads.length + 1}: ${messageOf(error)}`);
    return 2;
  } finally {
    await browser.close();
  }
  const record: PageLoadRecord = {
    converted,
    frames: sent.frames,
    loadavgAfter: loadavg(),
    loadavgBefore,
    loads,
    requests: sent.requests,
    sockets: sent.sockets,
  };
  for (const line of printedLines(record, settings.token)) console.log(line);
  return failures(record).length === 0 ? 0 : 1;
}

if (import.meta.main) process.exitCode = await main();
