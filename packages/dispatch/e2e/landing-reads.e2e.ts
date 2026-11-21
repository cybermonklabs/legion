/**
 * The dashboard's landing waterfall, measured on four cold pages, one cold issue tab, and the
 * issue page and the board of a reader who turned the new issue page on (INFRA-1109, R3).
 *
 * Why this row exists. Opening a page of Dispatch before dispatch://INFRA-1109 cost a chain of
 * dependent round trips before anything the reader asked for was drawn: `AuthGate`
 * (`web/src/app.tsx`) held every read until `/auth/whoami` answered; the shell's reads and the
 * lazy chunk of the page then started together; the page's own first reads waited for that chunk;
 * the reads that named what those answered waited for them; and a last set waited again. A person
 * paid that chain on every page they opened, and so did every browser row of this suite, which is
 * why it cost twice - once as
 * a slow page and once as runner minutes. Nothing in the suite measured it, so nothing stopped
 * it growing. This row is that measurement. How long the chain is on the day you read this is in
 * the run's own output and not in this comment: the row prints every landing's whole waterfall,
 * passing or failing, so the work that flattens the chain has a red run to turn green and a
 * later change cannot quietly deepen it again.
 *
 * What a READ is. A GET whose path begins `/api/` or `/auth/`, and nothing else. A write (POST,
 * PUT, PATCH, DELETE) is no read, because it is not what a landing waits for; the document, the
 * scripts, the styles, the fonts and the pictures are no reads either, because they are the
 * build's cost and not the server's answer about this page. The live stream is not a read and
 * cannot be counted here even by accident: it is a WebSocket upgrade to `/ws/live`
 * (`web/src/api/live-socket.ts`, `liveSocketUrl`), which is no GET of those two prefixes, and
 * Playwright reports it as a websocket rather than as a request.
 *
 * What a WAVE is. R3's words are "a group of reads of which none starts after the first response
 * of the next group arrives". Read over the recorded times that is one grouping and only one:
 * reads that were all in flight together - every one of them started before any of them was
 * answered - are one wave, and the next wave opens at the first read that starts once the
 * current wave's earliest response has come back, because that read could have been waiting for
 * it. `wavesOf` below is that rule and nothing else, written as a pure function over the
 * (start, response) pairs so that a reader can check it against the printed waterfall by hand.
 * A page drawn from one wave asks for everything at once; a page drawn from five asks four
 * questions whose answers it already had the means to ask for.
 *
 * What the row drives. Seven cold landings - an issue page, the board, the inbox, a document
 * page, one tab of the issue page other than the Spec, and the issue page and the board again for
 * a reader whose stored issue-page choice is `outline`, the new issue page - each in a browser
 * context of its own, so no query cache, no HTTP cache and no warmed chunk of an earlier landing
 * can answer a read this one should have sent. Each landing is measured from its own navigation
 * until that page's OWN content is on the screen. "On the screen" has no observable of its own,
 * so each page names the locator the suite already waits for when it lands there, and each
 * comment beside a locator names the row it was taken from, so that the two cannot drift apart:
 * a locator that stopped meaning "this page is drawn" would otherwise leave this row measuring a
 * window that ends somewhere else.
 *
 * What the row holds besides the waves. Each page's own route chunk is asked for before the
 * landing read is sent. The entry starts it beside that read, before React renders anything
 * (`web/src/app.tsx`, `preloadRouteChunk`, called from `web/src/main.tsx`), and React's first
 * render is what sends the read; without that start, `AuthGate` draws its skeleton until the
 * landing read answers and the router asks for the chunk only then, so the page's own reads wait
 * on a chunk as well as on the landing. Start against start, and not against the read's answer:
 * on a server that answers the landing read in a few dozen milliseconds, a chunk asked for after
 * some round trip of its own lands after that answer whatever the entry does, and the row would
 * then hold the entry to a race. A chunk is no read, so the waves alone would not see that start
 * go. A landing on an issue tab other than the Spec has its tab's chunk started there too
 * (`preloadRouteChunk`'s tab arm, through `web/src/features/issue/tab-chunks.ts`), so the tab
 * landing's route chunk is that tab's own: without the start, the tab's panel asks for its chunk
 * only once the page has rendered, after the landing read has answered. A landing that draws a
 * document - the issue page and the document page - has the document's chunks started there as
 * well (`loadDocumentChunks`: the editor's library and the Yjs transport, each list named in
 * `web/src/features/doc/document-chunks.ts`), so a chunk of each list - the editor's library and
 * the transport - is asked for before the landing read is sent too; without that start, each
 * comes down only once the document mounts.
 *
 * A reader who turned the new issue page on draws the whole shell, on every page, through a chunk
 * of its own (`web/src/features/issue-page/frame-chunk.ts`): the frame shell, which holds the new
 * page's providers, its state, its fallback and its icon rail. The entry starts that chunk beside
 * the landing read on every address once any stored choice reads `outline` (`preloadRouteChunk`'s
 * first statement), so it is the route chunk of that reader's two landings: without the start,
 * the shell asks for it only once the landing read has answered (`ChosenShell`), and draws no
 * shell until it is in. The board is the second of them because the start is made on every
 * address: a start made in the issue arm alone would pass the issue landing and fail the board's.
 * R3's bound is the bound of the issue page every reader gets, and the new page is a preview a
 * reader turns on in Settings (dispatch://INFRA-1196), so the two landings of that reader are
 * held to the shell's one wave and to the start of their chunk, and their reads and waves are
 * printed beside the default landings' rather than bounded.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */
import { once } from "node:events";

import { setLiveSessions } from "./agents";
import { createIssue, createProject, createProjectDocument } from "./api";
import { documentEditor } from "./editor";
import { inboxPane } from "./inbox-pane";
import { seedCommentThread, THREAD_SESSION } from "./inbox-threads";
import { withIssuePage } from "./issue-page-option";
import { EDITOR_LIBRARY_CHUNK } from "./shell";
import {
  type Browser,
  type BrowserContext,
  expect,
  type Locator,
  type Page,
  type Request,
  test,
} from "./test";
import { asUser } from "./users";

/** R3's bound on one issue landing: at most this many reads, in at most `WAVE_BOUND` waves. */
const ISSUE_READ_BOUND = 12;
const WAVE_BOUND = 2;

/** R3's bound on every one of the landings: the reads the shell sends before the page's own
 *  data can start are one wave, never a chain of them. */
const SHELL_WAVE_BOUND = 1;

/** How long a read that was still in flight when the page's content appeared is given to come
 *  back, so the printed waterfall says when it was answered rather than leaving a gap. It changes
 *  no wave: every read of the window started before the window closed, so a response that arrives
 *  after that can have caused none of them. */
const ANSWER_SETTLE_MS = 2_000;

/** The landings, in the order this row drives them. The assertion names the same seven, so a
 *  landing dropped from here fails the row rather than quietly shrinking what "every page"
 *  covers. */
const PAGES = [
  "issue page",
  "board",
  "inbox",
  "document page",
  "issue tab",
  "issue page of an outline reader",
  "board of an outline reader",
] as const;

/**
 * One read of one landing: a GET under `/api/` or `/auth/`, with the time it started and the time
 * its response arrived, both in milliseconds from the navigation that began the landing.
 *
 * The two times are `performance.now()` readings taken as Playwright hands over the request and
 * the response, and they are fractions rather than whole milliseconds on purpose. Two events of
 * one landing can fall in the same millisecond - a cached answer, or a read the browser sends the
 * instant one lands - and read at whole-millisecond resolution a response and a request that did
 * not wait for it compare equal, which the grouping below would read as the start of a new wave.
 * The clock is monotonic and the events arrive in the order the browser raised them, so the
 * fractions keep that order and a wave is cut only where the browser really did wait.
 *
 * `respondedAt` is the one field written after the fact, by the response listener; it stands at
 * `Number.POSITIVE_INFINITY` while no response has come back, which is the honest reading for the
 * grouping - a read that was never answered inside the window started nothing inside it.
 */
interface Read {
  readonly pathname: string;
  readonly search: string;
  readonly startedAt: number;
  respondedAt: number;
}

/** What one landing cost: its reads in start order, and those reads grouped into waves. */
interface Measurement {
  readonly landing: Landing;
  readonly reads: readonly Read[];
  readonly waves: readonly (readonly Read[])[];
  /** When the page's own route chunk was first asked for, in milliseconds from the navigation,
   *  and the path it was asked at, so the report names the chunk's file; undefined when it never
   *  was before the landing ended. */
  readonly routeChunk: { readonly at: number; readonly path: string } | undefined;
  /** When each of the landing's document chunks (`Landing.documentChunks`) was first asked for,
   *  as `routeChunk.at` is read; a chunk with no entry was never asked for before the landing
   *  ended, and a landing that draws no document has none. */
  readonly documentChunkAt: ReadonlyMap<DocumentChunk, number>;
}

/** One chunk of a live document that the entry starts beside the landing read: what the report
 *  calls it, and the pattern of the path it is asked for at. */
interface DocumentChunk {
  readonly name: string;
  readonly pattern: RegExp;
}

/** One page this row lands on cold. */
interface Landing {
  /** What the report calls it; one of `PAGES`. */
  readonly name: (typeof PAGES)[number];
  /** The address the cold navigation opens. */
  readonly address: string;
  /** This page's own content, which is what the measurement window ends at. */
  readonly content: (page: Page) => Locator;
  /** Whether `read` is this page's own data, as against the shell's. The reads before the first
   *  such read are the ones the shell makes before the page can even ask for what it shows. */
  readonly ownData: (read: Read) => boolean;
  /** The page's own route chunk, which the entry starts beside the landing read
   *  (`web/src/app.tsx`, `preloadRouteChunk`). For a reader who turned the new issue page on it
   *  is the frame shell's chunk, which that reader's shell is drawn through on every page. */
  readonly routeChunk: RegExp;
  /** Prepares the landing's own context before its navigation: the two landings of a reader who
   *  turned the new issue page on store that choice here (`withIssuePage`), since each landing
   *  is a context of its own. */
  readonly prepare?: (context: BrowserContext) => Promise<void>;
  /** For a landing that draws a live document, the document's chunks the entry starts beside the
   *  landing read (`web/src/app.tsx`, `loadDocumentChunks`, over the two lists
   *  `web/src/features/doc/document-chunks.ts` names): one chunk of each list. Each is a chunk
   *  whose name stands whether or not the start is there: the libraries those lists import are
   *  chunks of their own either way. The start asks for both lists in one call, but a list
   *  dropped from that call leaves the other's chunks asked in time, so a chunk asked in time
   *  shows only that its own list was started, and the row holds the entry to a chunk of each:
   *  the editor's library, which the editor cannot draw a block without, and Yjs, which the
   *  transport cannot open a document without. */
  readonly documentChunks?: readonly DocumentChunk[];
  /** R3's bound on the whole landing, where it sets one. It sets one for the issue page alone. */
  readonly bounds?: { readonly reads: number; readonly waves: number };
}

/** What the seed left behind for the seven landings to open. */
interface World {
  readonly issue: string;
  /** The slug of the project document the document landing opens. It is a document of the
   *  project and not of the issue, because `/issues/:key/*` is the issue page's own route: an
   *  issue artifact drawn there is the issue page again with another document in it, while
   *  `/projects/:key/documents/:slug` is `DocumentPage`'s own chunk and its own read chain. */
  readonly document: string;
}

/**
 * Groups `reads` into waves, which is R3's definition and nothing more.
 *
 * Walking the reads in start order, a wave collects reads while each of them started before the
 * earliest response the wave has seen so far; the first read that starts at or after that
 * response opens the next wave, since it is the first one that could have been sent because of an
 * answer. The earliest response of a wave only ever falls as the wave grows, and a read is only
 * ever added while it starts before the earliest response so far, so every read of a wave did
 * start before the wave's own first response - which is the property R3 states.
 *
 * It is pure, and it is the only place the definition lives, so a reader can take the waterfall
 * this row prints and redo the grouping with a pencil.
 */
function wavesOf(reads: readonly Read[]): Read[][] {
  const ordered = [...reads].sort((left, right) => left.startedAt - right.startedAt);
  const waves: Read[][] = [];
  let wave: Read[] = [];
  let firstAnswer = Number.POSITIVE_INFINITY;
  for (const read of ordered) {
    if (wave.length > 0 && read.startedAt >= firstAnswer) {
      waves.push(wave);
      wave = [];
      firstAnswer = Number.POSITIVE_INFINITY;
    }
    wave.push(read);
    firstAnswer = Math.min(firstAnswer, read.respondedAt);
  }
  if (wave.length > 0) waves.push(wave);
  return waves;
}

/**
 * How many waves the shell spends before this page's own data can start: the waves that are over
 * and done before the wave the page's first own-data read stands in.
 *
 * A read of the shell's that went out in the same wave as the page's own data held nothing up -
 * the page could ask for what it shows at that moment, and did - so it is not a read made
 * "before that page's own data can start" and is not counted. Counting it would also make the
 * bound unreachable: a landing that asks for everything it needs in one wave after the viewer is
 * known would still be read as two, and whether it passed would turn on which read of that wave
 * the browser happened to send first.
 *
 * `undefined` where the landing read no data of its own at all, which is no measurement: it is
 * reported as a fault of its own rather than passing as zero.
 */
function shellWavesBeforeOwnData({ landing, reads, waves }: Measurement): number | undefined {
  const own = reads.find((read) => landing.ownData(read));
  if (own === undefined) return undefined;
  return waves.findIndex((wave) => wave.includes(own));
}

/** Collects every read `page` sends from now on, stamped against `from` (a `performance.now()`
 *  reading taken just before the navigation), and for each read a promise that settles once it
 *  has ended: its response has arrived, or its request has failed. */
function recordReads(
  page: Page,
  from: number
): { readonly ended: ReadonlyMap<Read, Promise<void>>; readonly reads: Read[] } {
  const reads: Read[] = [];
  const ended = new Map<Read, Promise<void>>();
  const pending = new Map<Request, { readonly end: () => void; readonly read: Read }>();
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() !== "GET") return;
    if (!url.pathname.startsWith("/api/") && !url.pathname.startsWith("/auth/")) return;
    const read: Read = {
      pathname: url.pathname,
      respondedAt: Number.POSITIVE_INFINITY,
      search: url.search,
      startedAt: performance.now() - from,
    };
    const end = Promise.withResolvers<void>();
    reads.push(read);
    ended.set(read, end.promise);
    pending.set(request, { end: end.resolve, read });
  });
  page.on("response", (response) => {
    const out = pending.get(response.request());
    if (out === undefined) return;
    out.read.respondedAt = performance.now() - from;
    pending.delete(response.request());
    out.end();
  });
  // A read whose request fails before any response is never answered: it keeps no response
  // time, and nothing is left to wait for.
  page.on("requestfailed", (request) => {
    const out = pending.get(request);
    if (out === undefined) return;
    pending.delete(request);
    out.end();
  });
  return { ended, reads };
}

/** A time as the waterfall prints it: tenths of a millisecond, right-aligned, so a reader reads
 *  the column as a column and can still see which of two reads in one millisecond came first. */
function stamp(ms: number): string {
  return Number.isFinite(ms) ? `${ms.toFixed(1).padStart(7)} ms` : "     no answer";
}

/** One landing's whole waterfall: a heading, then every read in start order with its wave, then
 *  when the page's route chunk was asked for, and each of the document's chunks where the page
 *  draws a document. */
function waterfallOf({ documentChunkAt, landing, reads, routeChunk, waves }: Measurement): string {
  const lines = [
    `${landing.name} (${landing.address}): ${reads.length} reads in ${waves.length} wave(s)`,
  ];
  for (const [index, wave] of waves.entries()) {
    for (const read of wave) {
      lines.push(
        `  wave ${index + 1}  started ${stamp(read.startedAt)}  answered ${stamp(
          read.respondedAt
        )}  GET ${read.pathname}${read.search}`
      );
    }
  }
  const routeChunkAsked =
    routeChunk === undefined ? "    never" : `${stamp(routeChunk.at)}  ${routeChunk.path}`;
  lines.push(`  route chunk  asked ${routeChunkAsked}`);
  for (const chunk of landing.documentChunks ?? []) {
    const at = documentChunkAt.get(chunk);
    lines.push(`  ${chunk.name} chunk  asked ${at === undefined ? "    never" : stamp(at)}`);
  }
  return lines.join("\n");
}

/**
 * Drives one cold landing and measures it.
 *
 * The context is this landing's own: a context signed in afresh holds no query cache, no HTTP
 * cache and no parsed chunk of a page an earlier landing opened, which is what makes the landing
 * cold. The window runs from the navigation until the page's own content is visible; a read that
 * started inside it is counted whether or not its answer had come back by then.
 */
async function land(browser: Browser, landing: Landing): Promise<Measurement> {
  const context = await asUser(browser, "alice");
  try {
    await landing.prepare?.(context);
    const tab = await context.newPage();
    const from = performance.now();
    const { ended, reads } = recordReads(tab, from);
    let routeChunk: Measurement["routeChunk"];
    const documentChunkAt = new Map<DocumentChunk, number>();
    tab.on("request", (request) => {
      const { pathname } = new URL(request.url());
      if (routeChunk === undefined && landing.routeChunk.test(pathname)) {
        routeChunk = { at: performance.now() - from, path: pathname };
      }
      for (const chunk of landing.documentChunks ?? []) {
        if (!documentChunkAt.has(chunk) && chunk.pattern.test(pathname)) {
          documentChunkAt.set(chunk, performance.now() - from);
        }
      }
    });
    await tab.goto(landing.address);
    await expect(landing.content(tab), `${landing.name} never drew its own content`).toBeVisible();
    const until = performance.now() - from;
    // The reads of the window are these objects and no others from here on: a read that starts
    // after the window closed is not one of them, and a response still to come is written into
    // the object this already holds.
    const measured = reads
      .filter((read) => read.startedAt <= until)
      .sort((left, right) => left.startedAt - right.startedAt);
    // Each read still out is waited on through its own end - its response, or its failure - and
    // for no longer than the settle window above: nothing on the page changes when one of them
    // answers, so there is no state to make a web-first assertion against, and the request's own
    // events are what says it is over.
    await Promise.race([
      Promise.all(measured.map((read) => ended.get(read))),
      once(AbortSignal.timeout(ANSWER_SETTLE_MS), "abort"),
    ]);
    return {
      documentChunkAt,
      landing,
      reads: measured,
      routeChunk,
      waves: wavesOf(measured),
    };
  } finally {
    await context.close();
  }
}

/** The world the seven landings open: one project with a document of its own, one issue with a
 *  spec and no plan, and one comment thread, so the inbox has a thread card to draw. */
async function seedWorld(): Promise<World> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The landing this row measures.\n",
    title: "Measure the landing waterfall",
  });
  const rollout = await createProjectDocument("CORE", {
    content: "# Rollout\n\nHow the landing is cut to two waves.\n",
    name: "rollout.md",
  });
  await seedCommentThread(
    issue.key,
    "How many waves does a cold landing take?",
    "Whatever the run prints: this row measures it rather than guessing."
  );
  return { document: rollout.artifact.slug, issue: issue.key };
}

/** The chunks the two landings that draw a document hold the entry to starting beside their
 *  landing read (`Landing.documentChunks` says why these two): the editor's library
 *  (`EDITOR_LIBRARY_CHUNK`) and Yjs. */
const DOCUMENT_CHUNKS: readonly DocumentChunk[] = [
  { name: "editor library", pattern: EDITOR_LIBRARY_CHUNK },
  { name: "transport", pattern: /\/assets\/yjs-[\w-]{8}\.js$/u },
];

/** The new issue page's frame shell (`web/src/features/issue-page/frame-shell.tsx`, loaded
 *  through `frame-chunk.ts`), the route chunk of both landings of a reader who turned that page
 *  on (`Landing.routeChunk`). */
const FRAME_SHELL_CHUNK = /\/assets\/frame-shell-[\w-]{8}\.js$/u;

/** The landing route, which answers one page's whole cold read: the viewer, the shell, and the
 *  part of the answer the page named by `?page=` draws (`web/src/api/queries.ts`). */
const LANDING_PATH = "/api/v1/landing";

/** Whether `read` is the landing read that carries `page`'s own data.
 *
 *  A page's own data is whatever answers what that page shows, whichever route answered it. It
 *  used to be one per-resource read the page gated on; the landing carries those same rows now,
 *  so a landing asked for this page is that page's own data as much as the read it stands in
 *  for. Each `ownData` below accepts either, so this row measures the waves before a page can
 *  draw whether its rows came from the one route or from the many.
 */
function landingFor(read: Read, page: "issue" | "board" | "inbox" | "document"): boolean {
  return read.pathname === LANDING_PATH && new URLSearchParams(read.search).get("page") === page;
}

function landingsOf(world: World): Landing[] {
  const board: Landing = {
    address: "/projects/CORE/issues",
    // The board's own content is the first row of the issues table. The locator is
    // `e2e/issues-table.e2e.ts`'s, which lands on `/projects/CORE/issues` and reads
    // `page.getByTestId("issues-table-scroll").locator("[data-issue-row]")`.
    content: (page) => page.getByTestId("issues-table-scroll").locator("[data-issue-row]").first(),
    name: "board",
    routeChunk: /\/assets\/ProjectPage-[\w-]{8}\.js$/u,
    ownData: (read) =>
      (read.pathname === "/api/v1/issues" &&
        new URLSearchParams(read.search).get("project") === "CORE") ||
      landingFor(read, "board"),
  };
  // A reader who turned the new issue page on: each landing is a context of its own, so each
  // stores that choice, and the frame shell is the route chunk of every page that reader opens.
  const outlineReader: Pick<Landing, "prepare" | "routeChunk"> = {
    prepare: (context) => withIssuePage(context, "alice"),
    routeChunk: FRAME_SHELL_CHUNK,
  };
  // The issue's own data, which the issue page, its tab and the outline reader's issue page
  // each read.
  const issueData = (read: Read): boolean =>
    read.pathname === `/api/v1/issues/${world.issue}` || landingFor(read, "issue");
  return [
    {
      address: `/issues/${world.issue}`,
      // The issue page's own content is the first block of its primary document: the Spec tab is
      // what `/issues/<key>` opens on. The locator is `e2e/prefix-links.e2e.ts`'s, which waits for
      // `documentEditor(page).locator('[data-block-id="…"]')` when it lands on an issue's spec.
      content: (page) => documentEditor(page).locator("[data-block-id]").first(),
      bounds: { reads: ISSUE_READ_BOUND, waves: WAVE_BOUND },
      name: "issue page",
      routeChunk: /\/assets\/IssuePage-[\w-]{8}\.js$/u,
      documentChunks: DOCUMENT_CHUNKS,
      ownData: issueData,
    },
    board,
    {
      address: "/",
      // The inbox's own content is the first thread card. The locator is
      // `e2e/inbox-threads.e2e.ts`'s, which lands on `/` and reads
      // `inboxPane(page).locator("[data-inbox-thread-body]")` at the reading column's width - the
      // width every project that runs this row has, and where the Inbox selects its first row as
      // it opens, so the card is drawn by the landing itself and by no click of this row's.
      content: (page) => inboxPane(page).locator("[data-inbox-thread-body]").first(),
      name: "inbox",
      routeChunk: /\/assets\/inbox-chunk-[\w-]{8}\.js$/u,
      ownData: (read) => read.pathname === "/api/v1/inbox" || landingFor(read, "inbox"),
    },
    {
      address: `/projects/CORE/documents/${world.document}`,
      // The project's own document page, which is `DocumentPage`'s route and chunk
      // (`web/src/app.tsx`); an issue artifact at `/issues/<key>/artifacts/<slug>` is the issue
      // page's route again and would measure that page twice. The address is
      // `e2e/document.e2e.ts`'s, which lands on `/projects/CORE/documents/<slug>` and waits for
      // `documentEditor(page)`; the block locator inside it is `e2e/prefix-links.e2e.ts`'s, as
      // on the issue page above.
      content: (page) => documentEditor(page).locator("[data-block-id]").first(),
      name: "document page",
      routeChunk: /\/assets\/DocumentPage-[\w-]{8}\.js$/u,
      documentChunks: DOCUMENT_CHUNKS,
      // The read this page gates on: `DocumentPage` holds everything else until
      // `["artifact-ref", "CORE/<slug>"]` answers with the document's id
      // (`web/src/features/document/DocumentPage.tsx`, `web/src/api/useProjectArtifact.ts`), and
      // the subscribers, text, blocks and asks reads all name that id.
      ownData: (read) =>
        read.pathname === `/api/v1/projects/CORE/artifacts/${world.document}` ||
        landingFor(read, "document"),
    },
    {
      address: `/issues/${world.issue}/plan`,
      // One tab of the issue page other than the Spec, whose panel is a chunk of its own
      // (`web/src/features/issue/tab-chunks.ts`). The issue has no plan document, so the Plan
      // tab draws its empty state from what the landing answered and sends no read of its own;
      // what this landing holds is that the entry starts the tab's chunk beside the landing read
      // (`web/src/app.tsx`, `preloadRouteChunk`'s tab arm). The locator is the rolling-deploy
      // walk's (`e2e/two-builds.multiprocess.e2e.ts`), which opens `/issues/<key>/plan` and waits
      // for `#issue-plan-panel`'s `plan-empty` or `plan-tab`.
      content: (page) => page.locator("#issue-plan-panel").getByTestId("plan-empty"),
      name: "issue tab",
      routeChunk: /\/assets\/PlanTab-[\w-]{8}\.js$/u,
      ownData: issueData,
    },
    {
      address: `/issues/${world.issue}`,
      // The first landing's address, for a reader whose stored choice is the new issue page:
      // the new page draws there, in the frame shell. Its own content is the page's column,
      // which the new page draws once its read of the issue has answered. The locator is
      // `issueRead`'s (`e2e/issue-page.ts`), which every row of `e2e/issue-page-d.e2e.ts` waits
      // for once it has landed on the new page.
      content: (page) => page.getByTestId("issue-column"),
      name: "issue page of an outline reader",
      ...outlineReader,
      ownData: issueData,
    },
    {
      // The board for the same reader: no issue's address, so the frame shell draws today's
      // shell inside the new page's providers, and the board draws as it does for every reader,
      // from its own content and data above. What this landing adds is the frame shell's start
      // on an address that is no issue's.
      ...board,
      ...outlineReader,
      name: "board of an outline reader",
    },
  ];
}

// SLOW(>=2s): seven cold landings, each in a context of its own - a fresh Chromium context, the
// cold parse of the dashboard bundle it cannot cache, and the landing's own reads, seven times
// over. The cost is the measurement: a warm context would answer reads from its query cache and
// count waves that a person opening the page never pays.
// No `@pr` tag: the pull-request lane's real-stack budget is full. It runs on the full lane at the
// merge queue.
test("a cold landing draws each page from at most two waves of reads", async ({ browser }) => {
  const world = await seedWorld();

  const measured: Measurement[] = [];
  for (const landing of landingsOf(world)) {
    measured.push(await land(browser, landing));
  }

  const waterfall = measured.map(waterfallOf).join("\n\n");
  console.log(`LANDING-READS\n${waterfall}`);

  // The count of landings driven is asserted first, and by name: "on each of the seven landings"
  // is true of no landings at all, so a landing that was dropped or never ran has to fail the row
  // rather than make the bounds below vacuous.
  expect(
    measured.map(({ landing }) => landing.name),
    "the row drives seven cold landings"
  ).toEqual([...PAGES]);

  const faults: string[] = [];
  for (const measurement of measured) {
    const { landing, reads, waves } = measurement;
    if (landing.bounds !== undefined) {
      if (reads.length > landing.bounds.reads) {
        faults.push(
          `the ${landing.name} sent ${reads.length} reads, over the ${landing.bounds.reads} ` +
            "R3 allows"
        );
      }
      if (waves.length > landing.bounds.waves) {
        faults.push(
          `the ${landing.name} took ${waves.length} waves, over the ${landing.bounds.waves} ` +
            "R3 allows"
        );
      }
    }
    const shell = shellWavesBeforeOwnData(measurement);
    if (shell === undefined) {
      faults.push(`the ${landing.name} read no data of its own, so its shell waves cannot be read`);
    } else if (shell > SHELL_WAVE_BOUND) {
      faults.push(
        `the ${landing.name} spent ${shell} waves on shell reads before its own data could ` +
          `start, over the ${SHELL_WAVE_BOUND} R3 allows`
      );
    }
    // The landing read is `AuthGate`'s `whoAmI` (`web/src/api/queries.ts`), which every cold
    // landing sends whatever its page.
    const landingRead = reads.find((read) => read.pathname === LANDING_PATH);
    if (landingRead === undefined) {
      faults.push(`the ${landing.name} sent no landing read to time its route chunk against`);
    } else if (measurement.routeChunk === undefined) {
      faults.push(`the ${landing.name} never asked for its route chunk`);
    } else if (measurement.routeChunk.at >= landingRead.startedAt) {
      faults.push(
        `the ${landing.name} asked for its route chunk ${measurement.routeChunk.path} at ` +
          `${stamp(measurement.routeChunk.at)}, after the landing read was sent at ` +
          `${stamp(landingRead.startedAt)}: the entry no longer starts it beside that read ` +
          "(web/src/app.tsx, preloadRouteChunk)"
      );
    }
    if (landingRead !== undefined) {
      for (const chunk of landing.documentChunks ?? []) {
        const at = measurement.documentChunkAt.get(chunk);
        if (at === undefined) {
          faults.push(`the ${landing.name} never asked for its document's ${chunk.name} chunk`);
        } else if (at >= landingRead.startedAt) {
          faults.push(
            `the ${landing.name} asked for its document's ${chunk.name} chunk at ${stamp(at)}, ` +
              `after the landing read was sent at ${stamp(landingRead.startedAt)}: the entry no ` +
              "longer starts the document's chunks beside that read (web/src/app.tsx, " +
              "preloadRouteChunk, loadDocumentChunks)"
          );
        }
      }
    }
  }

  expect(
    faults.join("\n"),
    `the landing waterfall of each page, in start order:\n\n${waterfall}`
  ).toBe("");
});
