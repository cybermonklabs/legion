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
 *    dropped and recorded, read against the server's document as the page was sent it. An update
 *    that changes only the attributes the editor derives on every load (a heading's id, an
 *    ordered list item's label) is dropped and counted, not named: no rendering carries those, and
 *    the next load derives them again. The page's own reads of the document, its awareness and
 *    its authentication still pass.
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

import { readFileSync } from "node:fs";

import type { Page } from "@playwright/test";
import { createDecoder, readVarString, readVarUint, readVarUint8Array } from "lib0/decoding";
import * as Y from "yjs";

import { changesOnlyDerived } from "../web/src/features/doc/derived-attributes";

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

/** The writes the guard stopped on one row's page, in the order the page sent them; how many
 *  updates it dropped that changed only what the editor derives; and the path of each API read the
 *  page made, each once, which `unlistedShotReads` holds to the routes the server's own check
 *  sends. */
export interface CorpusWrites {
  readonly stopped: string[];
  derived: number;
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

/** The sync message a document frame carries - its document, its y-protocols subtype and its
 *  update or state vector - or `undefined` for a frame of another message type (awareness,
 *  authentication). A frame is `<document name><message type><payload>` (Hocuspocus's framing, as
 *  `e2e/acceptance/socket-scope.ts` writes it). */
function syncMessage(
  frame: Uint8Array
): { document: string; subtype: number; payload: Uint8Array } | undefined {
  const decoder = createDecoder(frame);
  const document = readVarString(decoder);
  if (readVarUint(decoder) !== SYNC_MESSAGE) return undefined;
  const subtype = readVarUint(decoder);
  return { document, payload: readVarUint8Array(decoder), subtype };
}

/** Applies to `shadow` the document state a frame the server sent carries: its sync reply and
 *  each update after it. The guard reads a page's frame against this copy, which holds what the
 *  server holds as far as the page has been told. */
export function followServer(frame: Uint8Array, shadow: Y.Doc): void {
  try {
    const message = syncMessage(frame);
    if (message?.subtype === SYNC_STEP_2 || message?.subtype === SYNC_UPDATE) {
      Y.applyUpdate(shadow, message.payload);
    }
  } catch {
    // A frame the guard cannot read leaves the copy as it was; a page frame read against it is
    // then judged without that state, which can only make the guard stop more.
  }
}

/** What a frame a page sent on a document socket would change, or `undefined` for one that
 *  changes nothing, read against `shadow`: the server's document as `followServer` keeps it, and
 *  the stamps this guard dropped as derived. A Yjs update (sync subtype 2) or a sync reply
 *  (subtype 1) that carries structs of the page's own is a write, unless all it changes is
 *  attributes the editor derives on every load (a heading's id, an ordered list item's label;
 *  `changesOnlyDerived`, web/src/features/doc/derived-attributes.ts): the server settles those
 *  without a version, no rendering carries them, and the next load derives them again, so they are
 *  dropped and counted as `"derived"`, not named. A frame whose structs the shadow already holds
 *  all of is a repeat - of what the server sent, or of a stamp the guard dropped - and is dropped
 *  as `"derived"` too. A sync reply with no change of its own holds no struct, only the delete set
 *  every Yjs v1 update carries whole (`encodeStateAsUpdate` writes the whole delete set whatever
 *  state vector it is given), which deletes nothing the server has not; the update's first number
 *  is how many clients' structs it holds, and such a reply passes. An update that leans on a struct
 *  the shadow lacks cannot be judged, and is a write; so is a frame that does not parse. */
export function documentWrite(
  frame: Uint8Array,
  shadow: Y.Doc
): { write: string } | "derived" | undefined {
  try {
    const message = syncMessage(frame);
    if (message === undefined) return undefined;
    const { document, payload, subtype } = message;
    if (subtype !== SYNC_UPDATE && subtype !== SYNC_STEP_2) return undefined;
    const clients = readVarUint(createDecoder(payload));
    if (subtype === SYNC_STEP_2 && clients === 0) return undefined;
    const scratch = new Y.Doc();
    Y.applyUpdate(scratch, Y.encodeStateAsUpdate(shadow));
    let changed = 0;
    let derived = false;
    scratch.on("afterTransaction", (transaction: Y.Transaction) => {
      changed = transaction.changed.size;
      derived = changesOnlyDerived(transaction);
    });
    Y.applyUpdate(scratch, payload);
    const judged = scratch.store.pendingStructs === null && scratch.store.pendingDs === null;
    if (judged && changed === 0) return "derived";
    if (judged && derived) {
      // The page holds the stamp though the server never will, and the page's next update
      // follows it; the shadow takes it too, so that update reads against what the page has.
      Y.applyUpdate(shadow, payload);
      return "derived";
    }
    return {
      write:
        subtype === SYNC_UPDATE
          ? `a Yjs update to document ${document} (${payload.length} bytes)`
          : `a sync reply to document ${document} that carries structs of ${clients} client(s)`,
    };
  } catch (error) {
    return {
      write: `a document frame the guard could not read (${frame.length} bytes): ${String(error)}`,
    };
  }
}

/** Installs the guard on `page`, before its first navigation: Playwright routes only what a page
 *  sends after the route is in place. */
export async function guardSharedCorpus(page: Page): Promise<CorpusWrites> {
  const writes: CorpusWrites = { derived: 0, reads: new Set(), stopped: [] };
  await page.route("**/api/**", async (route) => {
    const request = route.request();
    if (WRITE_METHODS[request.method()] !== true) {
      writes.reads.add(new URL(request.url()).pathname);
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
    const shadow = new Y.Doc();
    const bytes = (message: string | Buffer) =>
      new Uint8Array(typeof message === "string" ? new TextEncoder().encode(message) : message);
    route.onMessage((message) => {
      const judged = documentWrite(bytes(message), shadow);
      if (judged === "derived") {
        writes.derived += 1;
        return;
      }
      if (judged !== undefined) {
        writes.stopped.push(judged.write);
        return;
      }
      server.send(message);
    });
    server.onMessage((message) => {
      followServer(bytes(message), shadow);
      route.send(message);
    });
  });
  return writes;
}

/** What a row says when its page wrote, beside the list of writes the guard stopped. */
export const CORPUS_WRITE_MESSAGE =
  "the writes this row's page made to the corpus every visual row shares, which the guard " +
  "stopped: a page of the shot set must draw without writing, and a read mark it writes as it " +
  "draws belongs in the seed (markWhatTheShotPagesRead, e2e/review-seed.ts) and in " +
  "SEEDED_READ_MARKS (e2e/visual-guard.ts)";

/** The routes the visual review's pages may read, as the route table writes them: the list the Go
 *  test TestTheVisualShotReadsWriteOnlyTheirCaches (packages/envoy/internal/dispatch/api/
 *  visual_shot_reads_test.go) sends each of, holding every write the server makes while it answers
 *  to the caches it may fill. A page cannot stop such a write, so a read of a route the list does
 *  not name is a read nothing checked. */
const SHOT_READS_FILE = new URL(
  "../../envoy/internal/dispatch/api/testdata/visual-shot-reads.json",
  import.meta.url
);

/** Each listed route as a pattern a path matches whole, a `{name}` wildcard one path segment. */
const listedShotReads: readonly RegExp[] = (
  JSON.parse(readFileSync(SHOT_READS_FILE, "utf8")) as { readonly reads: readonly string[] }
).reads.map(
  (route) =>
    new RegExp(
      `^${route.replace(/[.*+?^$()|[\]\\]/gu, "\\$&").replace(/\{[a-z_]+\}/gu, "[^/]+")}$`,
      "u"
    )
);

/** The paths of `reads` that no listed route matches, sorted. */
export function unlistedShotReads(reads: ReadonlySet<string>): string[] {
  return [...reads].filter((path) => !listedShotReads.some((route) => route.test(path))).sort();
}

/** What a row says when its page read a route the list does not name, beside those reads. */
export const UNLISTED_READ_MESSAGE =
  "the routes this row's page read that packages/envoy/internal/dispatch/api/testdata/" +
  "visual-shot-reads.json does not name: the Go test that holds the server's writes during these " +
  "reads to the caches they may fill (TestTheVisualShotReadsWriteOnlyTheirCaches) sends only the " +
  "routes it names, so add each one there, as the route table writes it, and a path for it to " +
  "shotReadPaths";
