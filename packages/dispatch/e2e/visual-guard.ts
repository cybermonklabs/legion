/** The visual review's guard on its shared corpus. Every row of `e2e/visual.e2e.ts` draws pages
 *  of one corpus that the host seeded once (`e2e/visual-seed.ts`), so a row whose page writes to
 *  it changes what every later row draws, and a baseline then holds another row's write. The
 *  guard makes that a failure of the row that wrote, by name, and keeps the write from reaching
 *  the corpus at all:
 *
 *  - Every request a row's page sends to `/api/` with a write method (`DELETE`, `PATCH`, `POST`,
 *    `PUT`; `WRITE_METHODS` in `web/src/api/client.ts`) is aborted and recorded, except the read
 *    marks the seed already wrote before any row (`SEEDED_READ_MARKS`), which change nothing.
 *  - Every frame a row's page sends on a document socket (`/ws/doc/`) that would change the
 *    document - a Yjs update, or a sync reply that carries structs of the page's own - is
 *    dropped and recorded. The page's own reads of the document, its awareness and its
 *    authentication still pass.
 *
 *  The row then asserts that list is empty (`e2e/visual.e2e.ts`), so it fails with every write it
 *  made, by name. A guard that let a write through and only reported it would leave the corpus
 *  changed for every row after, which is the failure this module exists to prevent, so a write is
 *  stopped first and reported second.
 *
 *  It replaces the reverse-order second run the visual job made: that run caught a write only when
 *  some later row's shot happened to draw it, and could not say a row passes alone; this names the
 *  row that wrote, whatever any other row draws.
 *
 *  It imports nothing that reaches `./psql`, as `e2e/visual.e2e.ts` requires of what it imports. */

import type { Page } from "@playwright/test";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";

/** The methods that write (`WRITE_METHODS` in `web/src/api/client.ts`). */
const WRITE_METHODS: Record<string, true> = { DELETE: true, PATCH: true, POST: true, PUT: true };

/** The writes a page of the shot set makes as it draws, which `markWhatTheShotPagesRead`
 *  (`e2e/review-seed.ts`) writes before any row: the broadcast page's read of its replies
 *  (`useMarkShownRepliesRead`), and the Spec tab's read of the spec (`useSpecMarks`). Each is a
 *  mark that only moves forward to where the seed already put it, so the page's own request is a
 *  repeat. A request to either route with any other field is no repeat, and is refused. */
const SEEDED_READ_MARKS: readonly { readonly path: RegExp; readonly fields: readonly string[] }[] =
  [
    { fields: ["read_replies"], path: /^\/api\/v1\/me\/agents\/[^/]+\/state$/u },
    { fields: ["version"], path: /^\/api\/v1\/me\/documents\/[^/]+\/read$/u },
  ];

/** Hocuspocus's message type for a Yjs sync message, and y-protocols' sync subtypes. */
const SYNC_MESSAGE = 0;
const SYNC_STEP_2 = 1;
const SYNC_UPDATE = 2;

/** The writes the guard stopped on one row's page, in the order the page sent them, and the API
 *  routes the page read, each once: the server answers those itself, so a write it made while
 *  answering one is outside what the guard stops, and this list is what a reader checks for it. */
export interface CorpusWrites {
  readonly stopped: string[];
  readonly reads: Set<string>;
}

/** Whether `request` is a seeded read mark: a `PUT` to one of `SEEDED_READ_MARKS`' routes whose
 *  JSON body holds that route's field and no other. */
export function isSeededReadMark(method: string, path: string, body: unknown): boolean {
  if (method !== "PUT" || body === null || typeof body !== "object" || Array.isArray(body)) {
    return false;
  }
  const fields = Object.keys(body);
  return SEEDED_READ_MARKS.some(
    (mark) =>
      mark.path.test(path) &&
      fields.length > 0 &&
      fields.every((field) => mark.fields.includes(field))
  );
}

/** What a frame a page sent on a document socket would change, or `undefined` for one that
 *  changes nothing: a Yjs update (sync subtype 2), which is how an edit travels once the document
 *  has synced, or a sync reply (subtype 1) that carries structs of the page's own. A sync reply
 *  a page sends with no change of its own holds no struct, only the delete set every Yjs v1
 *  update carries whole (`encodeStateAsUpdate` writes the document's whole delete set whatever
 *  state vector it is given), and that deletes nothing the server has not deleted already; the
 *  update's first number is how many clients' structs it holds. A frame is
 *  `<document name><message type><payload>` (Hocuspocus's framing, as
 *  `e2e/acceptance/socket-scope.ts` writes it). A frame that does not parse is reported too,
 *  since the guard cannot say it changes nothing. */
export function documentWrite(frame: Uint8Array): string | undefined {
  try {
    const decoder = createDecoder(frame);
    const document = readVarString(decoder);
    if (readVarUint(decoder) !== SYNC_MESSAGE) return undefined;
    const subtype = readVarUint(decoder);
    if (subtype === SYNC_UPDATE) {
      return `a Yjs update to document ${document} (${readVarUint8Array(decoder).length} bytes)`;
    }
    if (subtype === SYNC_STEP_2) {
      const clients = readVarUint(createDecoder(readVarUint8Array(decoder)));
      return clients === 0
        ? undefined
        : `a sync reply to document ${document} that carries structs of ${clients} client(s)`;
    }
    return undefined;
  } catch (error) {
    return `a document frame the guard could not read (${frame.length} bytes): ${String(error)}`;
  }
}

/** Installs the guard on `page`, before its first navigation: Playwright routes only what a page
 *  sends after the route is in place. */
export async function guardSharedCorpus(page: Page): Promise<CorpusWrites> {
  const writes: CorpusWrites = { reads: new Set(), stopped: [] };
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    if (WRITE_METHODS[request.method()] !== true) {
      writes.reads.add(`${request.method()} ${new URL(request.url()).pathname}`);
      return route.fallback();
    }
    const path = new URL(request.url()).pathname;
    let body: unknown;
    try {
      body = request.postDataJSON();
    } catch {
      body = undefined;
    }
    if (isSeededReadMark(request.method(), path, body)) return route.fallback();
    const sent = request.postData();
    writes.stopped.push(`${request.method()} ${path}${sent ? ` ${sent.slice(0, 200)}` : ""}`);
    await route.abort("blockedbyclient");
  });
  await page.routeWebSocket(/\/ws\/doc\//u, (route) => {
    const server = route.connectToServer();
    route.onMessage((message) => {
      const frame = typeof message === "string" ? new TextEncoder().encode(message) : message;
      const write = documentWrite(new Uint8Array(frame));
      if (write !== undefined) {
        writes.stopped.push(write);
        return;
      }
      server.send(message);
    });
    server.onMessage((message) => route.send(message));
  });
  return writes;
}

/** What a row says when its page wrote, beside the list of writes the guard stopped. */
export const CORPUS_WRITE_MESSAGE =
  "the writes this row's page made to the corpus every visual row shares, which the guard " +
  "stopped: a page of the shot set must draw without writing, and a read mark it writes as it " +
  "draws belongs in the seed (markWhatTheShotPagesRead, e2e/review-seed.ts) and in " +
  "SEEDED_READ_MARKS (e2e/visual-guard.ts)";
