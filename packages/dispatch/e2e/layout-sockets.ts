/** The two sockets a layout row's page draws from beside its HTTP answers, recorded and replayed
 *  for the stubbed layout rows (`e2e/layout-rule.stub.e2e.ts`): `/ws/doc/<artifact>`, the live
 *  document a spec, a plan, a Verification document or an artifact is drawn from, and `/ws/live`,
 *  over which an agent's turns reach its live view. A HAR records no socket and
 *  `page.routeWebSocket` has no replay mode (`e2e/stub/live-frames.ts`), so this writes what the
 *  server sent beside the row's HAR, as `<row>.sockets.json`, and answers from it.
 *
 *  - `/ws/doc`: the Hocuspocus and Yjs frames of the first connection to each document, in the
 *    order they crossed. On replay each frame the page sends is answered with the server's frames
 *    that followed the page's frame of the same position in the recording. The frames name the
 *    document by the artifact id the scene replays, and the page's own frames (its state vector,
 *    its awareness) change no frame the server answered with that the page reads.
 *  - `/ws/live`: each subscription's frames, keyed by its view. A subscription carries an id the
 *    page picks (`sub`), so a replayed frame takes the id of the page's own subscription to that
 *    view, and a heartbeat is answered as the server answers it.
 *
 *  Registered on the page before its first load and after the stub fixture's own `/ws/live`
 *  answer, which it replaces for these rows: Playwright reads the last route registered first
 *  (`routeWebSocket` unshifts its handler, playwright-core 1.64.1).
 *
 *  On replay it also answers one write the recording does not hold: the reader's read mark on an
 *  issue (`PUT /api/v1/me/issues/{key}/state`). A replayed issue page sends it and the HAR holds
 *  no answer, so the stub aborted it. A read mark is a kept write
 *  (`features/writes/write-storage.ts`): the page sent it again on each load and each retry - six
 *  times in one replayed `issue-conversation` row - and each is a request under `/api/v1/me/`,
 *  which the settle wait counts as moving the Inbox number (`e2e/review-routes.ts`). One of the
 *  six, sent as the row moved to its next load, was never answered, and the wait timed out on it.
 *  Answered here as the server answers it, the mark lands once and nothing is sent again. */
import { readFileSync, writeFileSync } from "node:fs";
import type { Page, TestInfo, WebSocketRoute } from "@playwright/test";

import { answerTo } from "./stub/live-frames";
import { recordingPath } from "./stub/recordings";
import { stubRecording } from "./stub/selection";

/** One frame, as it crossed: text as text, binary as base64. */
interface Frame {
  readonly binary: boolean;
  readonly data: string;
}

interface DocConnection {
  readonly frames: readonly ({ readonly from: "page" | "server" } & Frame)[];
}

interface Recorded {
  /** The first connection to each document, by its path (`/ws/doc/<artifact id>`). */
  readonly doc: Record<string, DocConnection>;
  /** Every live frame the server sent for a subscription, by the view the page subscribed to. */
  readonly live: Record<string, readonly string[]>;
}

const DOC = /\/ws\/doc\//;
const LIVE = /\/ws\/live/;

function frameOf(message: string | Buffer): Frame {
  return typeof message === "string"
    ? { binary: false, data: message }
    : { binary: true, data: message.toString("base64") };
}

function messageOf(frame: Frame): string | Buffer {
  return frame.binary ? Buffer.from(frame.data, "base64") : frame.data;
}

interface LiveFrame {
  type?: string;
  sub?: string;
  view?: unknown;
}

/** The key a subscription's frames are kept under: its view, which the page asks again for on
 *  every load and in both modes. */
function viewKey(view: unknown): string {
  return JSON.stringify(view ?? null);
}

/** Records the document and live sockets the page opens against the harness, and returns the
 *  write that keeps them beside the row's HAR. */
async function record(page: Page, testInfo: TestInfo): Promise<() => void> {
  const recorded: { doc: Record<string, DocConnection>; live: Record<string, string[]> } = {
    doc: {},
    live: {},
  };
  await page.routeWebSocket(DOC, (socket: WebSocketRoute) => {
    const path = new URL(socket.url()).pathname;
    const frames: ({ from: "page" | "server" } & Frame)[] = [];
    if (recorded.doc[path] === undefined) recorded.doc[path] = { frames };
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      frames.push({ from: "page", ...frameOf(message) });
      server.send(message);
    });
    server.onMessage((message) => {
      frames.push({ from: "server", ...frameOf(message) });
      socket.send(message);
    });
  });
  await page.routeWebSocket(LIVE, (socket: WebSocketRoute) => {
    const views = new Map<string, string>();
    const server = socket.connectToServer();
    socket.onMessage((message) => {
      if (typeof message === "string") {
        const sent = JSON.parse(message) as LiveFrame;
        if (sent.type === "subscribe" && sent.sub !== undefined) {
          views.set(sent.sub, viewKey(sent.view));
        }
      }
      server.send(message);
    });
    server.onMessage((message) => {
      if (typeof message === "string") {
        const answered = JSON.parse(message) as LiveFrame;
        const view = answered.sub === undefined ? undefined : views.get(answered.sub);
        if (view !== undefined) {
          recorded.live[view] ??= [];
          recorded.live[view].push(message);
        }
      }
      socket.send(message);
    });
  });
  return () => {
    writeFileSync(recordingPath(testInfo, ".sockets.json"), `${JSON.stringify(recorded)}\n`);
  };
}

/** Answers the page's document and live sockets from the row's recorded frames. */
async function replay(page: Page, testInfo: TestInfo): Promise<void> {
  const recorded = JSON.parse(
    readFileSync(recordingPath(testInfo, ".sockets.json"), "utf8")
  ) as Recorded;
  await page.routeWebSocket(DOC, (socket: WebSocketRoute) => {
    const connection = recorded.doc[new URL(socket.url()).pathname];
    if (connection === undefined) return;
    // The server's frames, in groups: the frames that followed the page's n-th frame are sent
    // when the page sends its n-th frame here, and those before its first are sent at once.
    const groups: Frame[][] = [[]];
    for (const frame of connection.frames) {
      if (frame.from === "page") groups.push([]);
      else groups[groups.length - 1]?.push(frame);
    }
    let sent = 0;
    for (const frame of groups[0] ?? []) socket.send(messageOf(frame));
    socket.onMessage(() => {
      sent += 1;
      for (const frame of groups[sent] ?? []) socket.send(messageOf(frame));
    });
  });
  await page.routeWebSocket(LIVE, (socket: WebSocketRoute) => {
    socket.onMessage((message) => {
      if (typeof message !== "string") return;
      const sent = JSON.parse(message) as LiveFrame;
      if (sent.type === "heartbeat") {
        socket.send(JSON.stringify(answerTo({ type: "heartbeat" })));
        return;
      }
      if (sent.type !== "subscribe" || sent.sub === undefined) return;
      for (const frame of recorded.live[viewKey(sent.view)] ?? []) {
        socket.send(JSON.stringify({ ...(JSON.parse(frame) as LiveFrame), sub: sent.sub }));
      }
    });
  });
}

/** Routes the page's document and live sockets for one stubbed layout row: recorded against the
 *  harness in a recording run, answered from the recording otherwise. Returns what the row calls
 *  after its last read, which writes the recording in a recording run and does nothing else. */
export async function routeLayoutSockets(page: Page, testInfo: TestInfo): Promise<() => void> {
  if (stubRecording()) return await record(page, testInfo);
  await replay(page, testInfo);
  await page.route(/\/api\/v1\/me\/issues\/[^/]+\/state$/, async (route) => {
    if (route.request().method() !== "PUT") {
      await route.fallback();
      return;
    }
    // The server's `UserIssueState`, the fields the mark wrote over what a new reader holds.
    const wrote = (route.request().postDataJSON() ?? {}) as Record<string, unknown>;
    await route.fulfill({
      contentType: "application/json",
      json: { dismissed: [], last_read_seq: 0, pinned: false, seq: 0, ...wrote },
    });
  });
  return () => undefined;
}
