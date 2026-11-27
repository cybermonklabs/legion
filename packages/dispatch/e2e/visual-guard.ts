/** The visual review's guard on its shared corpus. Every row of `e2e/visual.e2e.ts` draws pages
 *  of one corpus that the host seeded once (`e2e/visual-seed.ts`), so a row whose page writes to
 *  it changes what every later row draws, and a baseline then holds another row's write. The
 *  guard makes that a failure of the row that wrote, by name, and keeps the write from reaching
 *  the corpus at all:
 *
 *  - Every request a row's page sends to `/api/` with a write method (`DELETE`, `PATCH`, `POST`,
 *    `PUT`; `WRITE_METHODS` in `web/src/api/client.ts`) is aborted and recorded, except a read mark
 *    exactly as the seed already wrote it before any row (`isSeededReadMark`), which moves nothing.
 *  - Every frame a row's page sends on a document socket (`/ws/doc/`) that would change the
 *    document - a Yjs update, or a sync reply that carries a change of the page's own - is
 *    dropped and recorded, read against the server's document as the page was sent it. An update
 *    that changes only the attributes the editor derives on every load (a heading's id, an
 *    ordered list item's label) is dropped and counted, not named: no rendering carries those, and
 *    the next load derives them again. The page's own reads of the document, its awareness and
 *    its authentication still pass.
 *  - Every frame a row's page sends on the live socket (`/ws/live`) that is no subscribe or
 *    unsubscribe is dropped and recorded, and a subscribe of a view kind the list below does not
 *    name is dropped and recorded too.
 *
 *  The row then asserts that list is empty (`e2e/visual.e2e.ts`), so it fails with every write it
 *  made, by name. A guard that let a write through and only reported it would leave the corpus
 *  changed for every row after, which is the failure this module exists to prevent, so a write is
 *  stopped first and reported second.
 *
 *  A write the server makes while it answers is beyond what a page can stop. The row holds every
 *  API read and every live view its page made to the list the Go test
 *  TestTheVisualShotReadsWriteOnlyTheirCaches sends (`unlistedShotReads`, `unlistedShotViews`),
 *  and that test holds what the server writes while it answers them.
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
import type { SeededReadMarks } from "./review-routes";

/** The methods that write (`WRITE_METHODS` in `web/src/api/client.ts`). */
const WRITE_METHODS: Record<string, true> = { DELETE: true, PATCH: true, POST: true, PUT: true };

/** The two read marks a page of the shot set writes as it draws: the broadcast page's read of its
 *  replies (`useMarkShownRepliesRead`), by session, and the Spec tab's read of the spec
 *  (`useSpecMarks`), by document. */
const AGENT_READ_MARK = /^\/api\/v1\/me\/agents\/([^/]+)\/state$/u;
const DOCUMENT_READ_MARK = /^\/api\/v1\/me\/documents\/([^/]+)\/read$/u;

/** Hocuspocus's message type for a Yjs sync message, and y-protocols' sync subtypes. */
const SYNC_MESSAGE = 0;
const SYNC_STEP_2 = 1;
const SYNC_UPDATE = 2;

/** The writes the guard stopped on one row's page, in the order the page sent them; how many
 *  updates it dropped that changed only what the editor derives; the path of each API read the
 *  page made, each once, which `unlistedShotReads` holds to the list; and the kind of each live
 *  view it subscribed, each once, which `unlistedShotViews` holds to the list. */
export interface CorpusWrites {
  readonly stopped: string[];
  derived: number;
  readonly reads: Set<string>;
  readonly views: Set<string>;
}

/** `value`'s fields when it is a plain JSON object, else undefined. */
function plainObject(value: unknown): Readonly<Record<string, unknown>> | undefined {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return undefined;
  return Object.fromEntries(Object.entries(value));
}

/** `value` when it is an array of strings, else undefined. */
function stringList(value: unknown): readonly string[] | undefined {
  return Array.isArray(value) && value.every((item) => typeof item === "string")
    ? value
    : undefined;
}

/** Whether a request is a read mark exactly as `seeded` holds it, and so a repeat that moves
 *  nothing: a `PUT` of one session's state whose body is `read_replies` alone, every id of it one
 *  the seed marked read for that session; or a `PUT` of one document's read whose body is
 *  `version` alone, the version the seed wrote. Any other field, reply or version is a write the
 *  seed never made: a lower version moves the read back (`putDocumentRead`), and an id the seed
 *  did not mark marks one more reply read. */
export function isSeededReadMark(
  method: string,
  path: string,
  body: unknown,
  seeded: SeededReadMarks
): boolean {
  const fields = plainObject(body);
  if (method !== "PUT" || fields === undefined) return false;
  const keys = Object.keys(fields);
  const agent = AGENT_READ_MARK.exec(path);
  if (agent !== null) {
    const session = decodeURIComponent(agent[1] ?? "");
    // `Object.hasOwn`, not indexing alone: the seed's maps inherit `Object.prototype`, so a path
    // naming `constructor` would read a function.
    const marked = Object.hasOwn(seeded.agents, session) ? seeded.agents[session] : undefined;
    const replies = stringList(fields.read_replies);
    return (
      marked !== undefined &&
      keys.length === 1 &&
      replies !== undefined &&
      replies.length > 0 &&
      replies.every((id) => marked.includes(id))
    );
  }
  const document = DOCUMENT_READ_MARK.exec(path);
  if (document !== null) {
    const id = decodeURIComponent(document[1] ?? "");
    const version = Object.hasOwn(seeded.documents, id) ? seeded.documents[id] : undefined;
    return version !== undefined && keys.length === 1 && fields.version === version;
  }
  return false;
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
 *  the stamps this guard dropped as derived. Each Yjs update (sync subtype 2) and sync reply
 *  (subtype 1) is applied to a copy of the shadow and judged by what that changed:
 *
 *  - nothing, a reply with no struct: the reply a page with no change of its own sends, whose
 *    delete set (every Yjs v1 update carries its document's whole one) deletes nothing the server
 *    has not. It passes.
 *  - nothing else: a repeat, of what the server sent or of a stamp the guard dropped. It is dropped
 *    as `"derived"`, since forwarding a dropped stamp's repeat would write that stamp.
 *  - only attributes the editor derives on every load (a heading's id, an ordered list item's
 *    label; `changesOnlyDerived`, web/src/features/doc/derived-attributes.ts): the server settles
 *    those without a version, no rendering carries them, and the next load derives them again. It
 *    is dropped as `"derived"`, not named.
 *  - anything else, a deletion included, or a change the shadow cannot place: a write.
 *
 *  A frame that does not parse is a write too, since the guard cannot say it changes nothing. */
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
    if (judged && changed === 0) {
      return subtype === SYNC_STEP_2 && clients === 0 ? undefined : "derived";
    }
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
          : `a sync reply to document ${document} that changes it (${payload.length} bytes, structs of ${clients} client(s))`,
    };
  } catch (error) {
    return {
      write: `a document frame the guard could not read (${frame.length} bytes): ${String(error)}`,
    };
  }
}

/** The list the Go test TestTheVisualShotReadsWriteOnlyTheirCaches (packages/envoy/internal/
 *  dispatch/api/visual_shot_reads_test.go) sends: the routes of the API reads the shot pages may
 *  make, as the route table writes them; the kinds of the live views they may subscribe; and the
 *  sign-in routes they may read (the routes package's `/auth/` routes, which no API route table
 *  holds). That test holds every write the server makes while it answers them to the tables it may
 *  write. A page cannot stop such a write, so a read or a view the list does not name is one
 *  nothing checked. */
const SHOT_READS_FILE = new URL(
  "../../envoy/internal/dispatch/api/testdata/visual-shot-reads.json",
  import.meta.url
);
const shotListFile = plainObject(JSON.parse(readFileSync(SHOT_READS_FILE, "utf8")));
const shotList = {
  auth: stringList(shotListFile?.auth),
  live: stringList(shotListFile?.live),
  reads: stringList(shotListFile?.reads),
};
if (shotList.reads === undefined || shotList.live === undefined || shotList.auth === undefined) {
  throw new Error(
    `${SHOT_READS_FILE.pathname} holds no list of reads, of live views and of sign-in reads: ` +
      "every visual row's reads would be held to nothing"
  );
}
const listedReads: readonly string[] = shotList.reads;
const listedViews: readonly string[] = shotList.live;
const listedAuthReads: readonly string[] = shotList.auth;

/** The sign-in routes' prefix: the routes package mounts them beside the API (`/auth/whoami`,
 *  `/auth/logout`), and the OpenAPI document names none of them. */
const AUTH_PREFIX = "/auth/";

/** The route table's GET routes, as the committed OpenAPI document names them. Two Go tests hold
 *  that document to the table: TestOpenAPIDocumentHasOneOperationPerMountedRoute and
 *  TestTheCommittedOpenAPIDocumentIsWhatTheEmitterWrites (packages/envoy/internal/dispatch/api/
 *  openapi_test.go), so a route the server mounts is a route here. */
const OPENAPI_FILE = new URL("../../contracts/schemas/dispatch-openapi.json", import.meta.url);
const getRoutes: readonly (readonly string[])[] = Object.entries(
  plainObject(plainObject(JSON.parse(readFileSync(OPENAPI_FILE, "utf8")))?.paths) ?? {}
)
  .filter(([, item]) => plainObject(item)?.get !== undefined)
  .map(([route]) => route.split("/"));
if (getRoutes.length === 0) {
  throw new Error(
    `${OPENAPI_FILE.pathname} names no GET route: every read would be one of no route`
  );
}

const isWildcard = (segment: string): boolean => segment.startsWith("{") && segment.endsWith("}");

/** The GET route the server serves `path` by, or undefined for none: of the routes whose
 *  segments match the path's, a literal matching its own segment and a `{name}` wildcard any one,
 *  the most specific, read segment by segment with a literal over a wildcard - the precedence Go's
 *  ServeMux gives the route table, where `/api/v1/asks/open` serves that path and
 *  `/api/v1/asks/{id}` every other. */
export function routeOf(path: string): string | undefined {
  const segments = path.split("/");
  let best: readonly string[] | undefined;
  for (const route of getRoutes) {
    if (route.length !== segments.length) continue;
    if (!route.every((part, at) => isWildcard(part) || part === segments[at])) continue;
    if (best === undefined) {
      best = route;
      continue;
    }
    const chosen = best;
    const differs = route.findIndex(
      (part, at) => isWildcard(part) !== isWildcard(chosen[at] ?? "")
    );
    if (differs >= 0 && !isWildcard(route[differs] ?? "")) best = route;
  }
  return best?.join("/");
}

/** Each path of `reads` the list does not name the route of, sorted, as `<path> (<route>)`, or
 *  `<path> (no route)` for a path no GET route of the table serves: that one is no read the Go test
 *  could send either. A sign-in path is listed when the list's sign-in reads name it, and is
 *  `<path> (sign-in read)` when they do not. */
export function unlistedShotReads(reads: ReadonlySet<string>): string[] {
  const unlisted: string[] = [];
  for (const path of reads) {
    if (path.startsWith(AUTH_PREFIX)) {
      if (!listedAuthReads.includes(path)) unlisted.push(`${path} (sign-in read)`);
      continue;
    }
    const route = routeOf(path);
    if (route === undefined) unlisted.push(`${path} (no route)`);
    else if (!listedReads.includes(route)) unlisted.push(`${path} (${route})`);
  }
  return unlisted.sort();
}

/** The live view kinds of `views` the list does not name, sorted. */
export function unlistedShotViews(views: ReadonlySet<string>): string[] {
  return [...views].filter((kind) => !listedViews.includes(kind)).sort();
}

/** What a frame a page sent on the live socket does: `pass`es for an unsubscribe and for a
 *  subscribe of a listed view kind, whose kind it names; a subscribe of a kind the list does not
 *  name is `unlisted`, and anything else - another type, a frame that is no JSON object, a binary
 *  one - is a `write` the guard cannot place. The socket takes nothing but subscribe and
 *  unsubscribe (`liveSocketFrame`, packages/envoy/internal/dispatch/api/live_socket.go). */
export function liveFrame(
  frame: string | Buffer
):
  | { readonly pass: true; readonly view?: string }
  | { readonly unlisted: string }
  | { readonly write: string } {
  if (typeof frame !== "string") {
    return { write: `a binary live socket frame (${frame.length} bytes)` };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(frame);
  } catch {
    parsed = undefined;
  }
  const head = plainObject(parsed);
  const kind = plainObject(head?.view)?.kind;
  if (head?.type === "unsubscribe") return { pass: true };
  if (head?.type === "subscribe" && typeof kind === "string") {
    return listedViews.includes(kind) ? { pass: true, view: kind } : { unlisted: kind };
  }
  return {
    write: `a live socket frame that is no subscribe or unsubscribe: ${frame.slice(0, 200)}`,
  };
}

/** Installs the guard on `page`, before its first navigation: Playwright routes only what a page
 *  sends after the route is in place. `seeded` is the read marks the seed wrote, which a page may
 *  write again exactly as they stand. */
export async function guardSharedCorpus(
  page: Page,
  seeded: SeededReadMarks
): Promise<CorpusWrites> {
  const writes: CorpusWrites = { derived: 0, reads: new Set(), stopped: [], views: new Set() };
  // The API and the sign-in routes beside it (`/auth/whoami`, `/auth/logout`): a read of either is
  // recorded for `unlistedShotReads`, and a write to either is stopped, a sign-in route having no
  // read mark the seed wrote.
  await page.route(/^[a-z]+:\/\/[^/]+\/(api|auth)\//u, async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (WRITE_METHODS[request.method()] !== true) {
      writes.reads.add(path);
      return route.fallback();
    }
    let body: unknown;
    try {
      body = request.postDataJSON();
    } catch {
      body = undefined;
    }
    if (isSeededReadMark(request.method(), path, body, seeded)) return route.fallback();
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
  await page.routeWebSocket(/\/ws\/live(\?|$)/u, (route) => {
    const server = route.connectToServer();
    route.onMessage((message) => {
      const judged = liveFrame(message);
      if ("write" in judged) {
        writes.stopped.push(judged.write);
        return;
      }
      if ("unlisted" in judged) {
        writes.views.add(judged.unlisted);
        return;
      }
      if (judged.view !== undefined) writes.views.add(judged.view);
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
  "draws belongs in the seed (markWhatTheShotPagesRead, e2e/review-seed.ts), which the guard " +
  "lets the page write again exactly as the seed wrote it (isSeededReadMark, e2e/visual-guard.ts)";

/** What a row says when its page read a route, or subscribed a live view, the list does not
 *  name, beside those reads or views. */
export const UNLISTED_READ_MESSAGE =
  "the reads and live views of this row's page that packages/envoy/internal/dispatch/api/" +
  "testdata/visual-shot-reads.json does not name: the Go test that holds the server's writes " +
  "while it answers them (TestTheVisualShotReadsWriteOnlyTheirCaches) sends only what that list " +
  "names, so add each route there as the route table writes it, with a path for it in " +
  "shotReadPaths; each view kind, with a view of it in shotLiveViews; or each sign-in path, with " +
  "what it runs in shotAuthReads. A path of no route is a read the server does not serve";
