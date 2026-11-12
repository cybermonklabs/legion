// The measurement set-up for a large document on a phone, as dispatch://INFRA-341/spec words it:
// the browser suite's `iphone` project with 4x CPU throttling, reading five numbers from the
// page itself. The first is the time from the start of navigation until the editor's first
// visible block - a block of the editor's own root while that root carries
// `data-write-path="live"`, which is the first block a reader can type in and have the edit
// reach the Yjs document. The second is the time until the editor is open, which is its last
// batch in and its Yjs binding live. The third is the longest main-thread task, through a
// `PerformanceObserver` for `longtask` entries, from the start of navigation until the editor
// has opened and the page has then scrolled to the end of the document one viewport height
// every 100 ms.
//
// The last two are the draws a reader has in front of all that, neither of them read against a
// target: the time until any block of the document was on the screen, whoever drew it, which
// is the server's own blocks where it drew them into the page it sent (`first painted`, about
// a second into the load on this pod); and the time until the page's own code had a block of
// the document on the screen, in the preview it renders inside the document surface, which is
// about seven seconds later. The second of those is what this rig read as the editor's block
// until INFRA-341: its selector fell back to the `Document editor` section's name, and that
// section is the whole document surface, the preview included. Neither draw takes an edit.
//
// Neither draw can be the first number now: the spec's Design says a server or preview block is
// not the block R11 means, and `firstBlockSelector` below rules both out element by element,
// rather than taking whichever block is on the screen first. What the measurement timed says so
// itself - the row reads back where that block stood - so a rig that drifted back onto the
// preview fails rather than printing its draw as the editor's.
//
// The window waits for the editor because the targets are about the whole build. The editor
// shows its first batch's blocks within a second of the sync and builds the rest behind them, a
// batch at a time (`packages/proof-editor/src/progressive-build.ts`), and the document's own
// draw gives up each of those blocks as they land (`web/src/features/doc/DocumentPreview.tsx`),
// so the page holds the whole document throughout and the scroll at the end of the window walks
// the whole of it. A window that closed at the editor's first block would close with most of the
// build still to come (dispatch://INFRA-341, R11).
//
// Every number is the page's own `performance.now()`, which counts from the navigation that
// created the document, so no Node-side round trip is in any of them.
// `large-documents-measured.e2e.ts` prints all five and asserts two of them against R11's
// targets - the editor's first visible block and the longest main-thread task - and the line
// it prints names which two those are.
import { type Page, test } from "@playwright/test";

declare global {
  interface Window {
    /** What the page says the moment it records one of the two numbers the run waits for, so
     *  the run never has to ask for them (`listenForThePage`). Installed before the page's own
     *  scripts; `undefined` for a page no measurement is listening to. */
    __largeDocumentReached?: (reached: Reached) => void;
    /** What the editor's first visible block was timed as: `performance.now()` when a block of
     *  an editor root carrying `data-write-path="live"` was first inside the viewport, which is
     *  the milliseconds from the start of navigation, and what that block itself said about
     *  where it was drawn. Undefined until there was one. */
    __largeDocumentFirstBlock?: {
      at: number;
      editable: boolean;
      inLiveEditor: boolean;
      inPreview: boolean;
    };
    /** `performance.now()` when the editor was first open: its last batch in and its Yjs
     *  binding live; undefined until it was. */
    __largeDocumentEditorAt?: number;
    /** The longest `longtask` entry's duration, in milliseconds, since the page began. */
    __largeDocumentLongestTask?: number;
    /** Every `longtask` entry since the page began, by when it started and how long it ran.
     *  The longest number above says how bad the worst freeze is; this says how many there
     *  were and where each fell, which is what a report of the tasks still over the target
     *  reads (dispatch://INFRA-341, R11). A browser's `longtask` entry is one of at least
     *  50 ms, which is the threshold the entry type itself is defined by. */
    __largeDocumentLongTasks?: { at: number; ms: number }[];
    /** Whether the `longtask` observer above is watching this page: true once `observe` has
     *  returned, and absent where the browser took no such observer. A row whose claim is that
     *  no task was over its target reads this, since an observer that never attached records
     *  no task at all and would meet that claim with nothing. */
    __largeDocumentLongTasksWatched?: boolean;
    /** `performance.now()` when the reader first had any block of the document on the screen -
     *  the server's draw, or the page's own - which is read against no target. */
    __largeDocumentPaintedAt?: number;
    /** `performance.now()` when the page's own code first had a block of the document on the
     *  screen: a block inside the document surface it renders, which is the preview's. Read
     *  against no target, and the number this rig used to read as the editor's. */
    __largeDocumentPageDrawnAt?: number;
    /** `performance.now()` when the editor's first batch landed, which is the task its root
     *  was given `data-write-path="live"` in. Undefined until one has (`watchTheReads`). */
    __largeDocumentFirstBatchAt?: number;
    /** When each part of the chrome the page defers was first drawn, by the name
     *  `watchTheChrome` was given for it; `performance.now()` at that drawing's own task. */
    __chromeDrawn?: Record<string, number>;
    /** Every `/api/v1` request the page has made, by when it went out and the path it went
     *  out on, which is how a row reads the order of the page's reads (`watchTheReads`). */
    __largeDocumentReads?: { at: number; path: string }[];
    /** When each step of the document's own opening happened, on the page's own clock: the
     *  chain that begins once the issue page has rendered and `ProofDocument` admits
     *  (dispatch://INFRA-341, R11). Every field is `performance.now()` at that step, and each
     *  is undefined until the step happened. Read against no target, and printed beside the
     *  numbers that are, so a run says where the editor's half of the load spent its seconds
     *  rather than only how long it took. */
    __documentOpening?: {
      socketCreatedMs?: number;
      socketOpenedMs?: number;
      syncStep1Ms?: number;
      stateFrameMs?: number;
      editorRootMs?: number;
    };
    /** What `watchTheBuildsWindow` recorded of the build: `performance.now()` at the editor's
     *  first batch and at the naming of the finished editor, each undefined until it happened. */
    __theBuildsWindow?: { firstBatchMs: number | undefined; namedMs: number | undefined };
    /** What the page says the moment its first batch lands, so a row that acts inside the
     *  build is told rather than asking (`watchTheBuildsWindow`). */
    __theFirstBatchLanded?: (at: number) => void;
  }
}

/**
 * What the page announces as it happens, and why it announces anything at all.
 *
 * Every number here is the page's own, and the run used to read each of them with
 * `page.evaluate`. A `page.evaluate` is a task on the page's main thread, and the editor's
 * build used to own that thread from its first batch to its last: it handed the thread back
 * through `scheduler.yield()`, whose continuations the browser serves before any posted task,
 * so for the whole of a build the run heard nothing it asked for. Measured on this pod,
 * `iphone` at 4x CPU throttling, with the socket held until the reader's own draw of the
 * 406,319-character document was complete: two readings came back in the three seconds after
 * the sync, and the next one 209 s later, when the build ended. The build hands the thread
 * back as a timer now (`endOfTask` in `@legion/proof-editor`'s `progressive-build.ts`), so a
 * reading the run takes is answered between batches.
 *
 * The announcements stay, because what they time is not what a reading can time. Each number
 * here is the moment the page reached something, and a poll can only say that the page had
 * reached it by the time the poll ran: between the batches of a build on a throttled phone
 * that is hundreds of milliseconds, against targets of three and five seconds. The page calls
 * `__largeDocumentReached` in the task it writes each number in, which reaches the run
 * through the binding, and the run waits for the call rather than asking again and again for
 * a number the page already has.
 */
interface Reached {
  /** The editor's first visible block, exactly as the page recorded it. */
  readonly firstBlock?: NonNullable<Window["__largeDocumentFirstBlock"]>;
  /** `performance.now()` when the editor was first open. */
  readonly editorOpenedMs?: number;
}

/** The two announcements one measurement waits for. Each settles once, with what the page
 *  recorded; neither is ever rejected, so a wait of its own names what never came. */
interface Announcements {
  readonly editorOpened: Promise<number>;
  readonly firstBlock: Promise<NonNullable<Window["__largeDocumentFirstBlock"]>>;
}

/** Installs the binding the page announces through, before any script of the page runs. */
async function listenForThePage(page: Page): Promise<Announcements> {
  const firstBlock = Promise.withResolvers<NonNullable<Window["__largeDocumentFirstBlock"]>>();
  const editorOpened = Promise.withResolvers<number>();
  await page.exposeFunction("__largeDocumentReached", (reached: Reached) => {
    if (reached.firstBlock !== undefined) {
      firstBlock.resolve(reached.firstBlock);
    }
    if (reached.editorOpenedMs !== undefined) {
      editorOpened.resolve(reached.editorOpenedMs);
    }
  });
  return { editorOpened: editorOpened.promise, firstBlock: firstBlock.promise };
}

/** Waits for one of the page's announcements, and fails with `missed` where it never comes.
 *  The wait is the one the caller named, and what it is waiting for is a message the page
 *  sends rather than a reading the run takes, so the build cannot starve it. */
async function theMomentThePageReached<T>(
  announced: Promise<T>,
  timeoutMs: number,
  missed: string
): Promise<T> {
  const expired = Promise.withResolvers<never>();
  const timer = setTimeout(
    () => expired.reject(new Error(`${missed} (waited ${timeoutMs} ms)`)),
    timeoutMs
  );
  try {
    return await Promise.race([announced, expired.promise]);
  } finally {
    clearTimeout(timer);
  }
}

/** What the renderer's CPU is throttled by: the rate the set-up names
 *  (`Emulation.setCPUThrottlingRate`). */
export const cpuThrottlingRate = 4;

/** The longest main-thread task either document may cost a reader ("Targets for large
 *  documents", dispatch://INFRA-341/spec): a task over half a second is a freeze a person
 *  sees. The measuring rows read it as their target, and the line this module prints names
 *  every task past it. */
export const longTaskTargetMs = 500;

/** How long the scroll waits between steps; each step moves one viewport height. */
export const scrollStepMs = 100;

/** How often the page looks for the editor's box, which is the end of its build and of the
 *  window. A timer rather than a frame loop, for the reason `watchTheDocument` gives. */
export const editorPollMs = 200;

/** The scroll stops at the end of the document or after this many steps, whichever comes first,
 *  so a document that keeps growing under the scroll still ends the measurement. */
export const scrollStepsMax = 120;

/** The editor's first visible block, which is the number the targets are read against: the
 *  first `[data-block-id]` element (`BLOCK_ID_ATTRIBUTE` in
 *  `web/src/features/doc/scroll-block.ts`) inside an editor root that says its blocks take
 *  edits reaching the Yjs document.
 *
 *  `data-write-path="live"` is written on the editor's root in the task its first batch lands
 *  in, which is the task that root is shown in - it is `display: none` before it - and the
 *  task from which an edit in those blocks is written through to the document
 *  (`packages/proof-editor/src/progressive-build.ts`). Nothing else on the page can wear it:
 *  `.proof-editor` is the class `constructProofEditor` puts on the root it builds into, and that
 *  build is the only writer of the attribute, which it removes again when the editor is
 *  destroyed (`packages/proof-editor/src/lib.ts`). So neither the blocks the server drew into
 *  the page (`#first-screen`) nor the page's own draw of the document's text (`Document text`,
 *  a sibling of the editor's root inside the `Document editor` section,
 *  `web/src/features/doc/DocumentPreview.tsx`) can be read as this block. The fallback that
 *  once stood here - the `Document editor` section's own name - could: that section is the
 *  whole document surface, the draw the reader meets first included, so the measurement timed
 *  the preview's block and read it against R11's target.
 *
 *  A build whose editor never carries the attribute draws no block this times, and the wait
 *  for it fails as the miss it is rather than quietly timing another surface's draw. */
const liveEditorSelector = '.proof-editor[data-write-path="live"]';
const firstBlockSelector = `${liveEditorSelector} [data-block-id]`;

/** The editor's root, whether or not its first batch has landed in it: `constructProofEditor`
 *  puts this class on the element it builds into, so the root is in the page from the moment
 *  the editor is constructed - while the socket is still syncing - and before anything of the
 *  document is drawn in it (`packages/proof-editor/src/lib.ts`). The opening timeline reads it
 *  as "the editor was created"; `liveEditorSelector` above is the later moment its first batch
 *  lands, which is the binding's. */
const editorRootSelector = ".proof-editor";

/** Where a block a reader has before the editor exists stands: the element the server put the
 *  document's opening blocks in (`web/src/features/doc/first-screen.ts`), and the page's own
 *  draw of the document's text, which adopts them and then draws the rest
 *  (`web/src/features/doc/DocumentPreview.tsx`). Neither takes an edit. The measurement reads
 *  this off the block it timed, so a run says which surface drew that block rather than leaving
 *  it to be read off the selector. */
const previewSelector = '#first-screen, [aria-label="Document text"]';

/** The page's own draw of the document: a block inside the `Document editor` section, which is
 *  the whole document surface the page renders - the preview it draws the text in, the editor's
 *  root, and the cards beside them (`web/src/features/doc/ProofDocument.tsx`). Until the editor
 *  builds, the only blocks in it are the preview's, so this is the preview's first block and
 *  the page's own first draw of the document in one number.
 *
 *  It is scoped by the section rather than by the preview's own `Document text` name because
 *  the server writes that same name into the page it sends, around the blocks the preview
 *  adopts (`internal/dispatch/pmdoc/first_screen.go`, `first-screen.test.tsx`): a selector on
 *  the name alone reads the server's draw, which `first painted` already times.
 *
 *  This is the selector the measurement read the editor's first block by until INFRA-341, which
 *  is the whole of that defect: a preview block, drawn before the editor exists and taking no
 *  edit, read against R11's target for the block a reader can type in. It is recorded here, and
 *  asserted nowhere, which is the only honest place for it: `watchTheDocument`'s loop stamps
 *  this number before it looks for the editor's own block in each pass, and this selector
 *  matches a block of the editor as readily as one of the preview, so the editor's block is
 *  later than this one however the measurement drifts. A row held the two to that order until
 *  INFRA-341 and could not have failed on either. What says the timed block was the editor's is
 *  the block's own reading of where it stood (`DocumentMeasurement`'s `firstBlock`). */
const pageDrawnSelector = '[aria-label="Document editor"] [data-block-id]';

/** The first block the reader had on the screen at all, whoever drew it: the blocks the server
 *  put in the page it sent, and the page's own draw of the document's text. Recorded as
 *  `first painted`, and read against no target. */
const firstPaintedSelector =
  '#first-screen [data-block-id], [aria-label="Document text"] [data-block-id]';

/** The editor, open: its last batch is in and its Yjs binding is live. The host names the
 *  textbox once the binding has answered its handle, which is a task after the binding
 *  connected (`web/src/features/doc/editor.ts`'s `editorAttributes`), so an editor wearing this
 *  name is an editor whose build is over. Never the `Document editor` section around it: that
 *  section is the whole document surface, the draw the reader reads first included, and it has
 *  a box from the page's first commit. */
const editorSelector = '[role="textbox"][aria-label="Document editor"]';

/**
 * The steps of the document's own opening, each `performance.now()` on the page's own clock,
 * and each `undefined` where the step never happened.
 *
 * This is the chain R11 is about: the socket, its SyncStep1 and the sync's apply begin once
 * the issue page has rendered and `ProofDocument` admits, which on a throttled phone is about
 * six seconds into the load. The chunks that chain needs are asked for from the address
 * before any page renders, and the socket is not (`web/src/features/doc/open-document-early.ts`
 * records what opening it there was measured to cost the reader). Three of the five steps are
 * read from the page's own WebSocket - the recorder below wraps it before any script of the
 * page runs - and the other two from the browser's resource timing and the DOM.
 */
export interface DocumentOpening {
  /** When the document's text was asked for: the read the schema admission check is, which
   *  nothing connects before (`web/index.html`, `open-document-early.ts`). */
  readonly textAskedMs: number | undefined;
  /** When that read answered, which is the earliest anything may connect. */
  readonly textAnsweredMs: number | undefined;
  /** When the document's first WebSocket was created, which is after the transport chunk
   *  (hocuspocus and yjs) has downloaded and evaluated. */
  readonly socketCreatedMs: number | undefined;
  /** When that socket opened. */
  readonly socketOpenedMs: number | undefined;
  /** When the page wrote its first binary frame to it, which is the provider's SyncStep1. */
  readonly syncStep1Ms: number | undefined;
  /** When the server's first binary frame arrived on it, which carries the document's state. */
  readonly stateFrameMs: number | undefined;
  /** When the editor's root was first in the page, which is `constructEditor` building it
   *  while the socket syncs: a number before or inside the state frame's apply, where it used
   *  to be the sync's answer (dispatch://INFRA-341, R11). */
  readonly editorRootMs: number | undefined;
}

export interface DocumentMeasurement {
  /** Milliseconds from the start of navigation until the editor's first visible block was in
   *  the viewport: a block of an editor root carrying `data-write-path="live"`, which is the
   *  first block a reader can type in. */
  readonly firstBlockMs: number;
  /** What the block `firstBlockMs` timed was, read off that element in the frame it was timed
   *  in: whether it stood inside an editor root carrying `data-write-path="live"`, whether it
   *  stood inside an editable host, and whether it stood in either draw the reader has before
   *  the editor exists. The measuring rows assert all three, so a rig that had timed the
   *  preview's block would fail rather than print that draw as the editor's. */
  readonly firstBlock: {
    readonly editable: boolean;
    readonly inLiveEditor: boolean;
    readonly inPreview: boolean;
  };
  /** Milliseconds from the start of navigation until the editor was open: its last batch in
   *  and its binding live. `undefined` where it never opened inside the wait the caller gave
   *  it. */
  readonly editorOpenedMs: number | undefined;
  /** Milliseconds from the start of navigation until the reader had any block of the document
   *  on the screen, whoever drew it. `undefined` where nothing was ever drawn. */
  readonly firstPaintedMs: number | undefined;
  /** Milliseconds from the start of navigation until the page's own code had a block of the
   *  document on the screen, in the preview inside the surface it renders. `undefined` where
   *  it never did. */
  readonly pageDrawnMs: number | undefined;
  /** The longest main-thread task, in milliseconds, over the whole measurement. */
  readonly longestTaskMs: number;
  /** Every main-thread task the browser recorded a `longtask` entry for, which is one of at
   *  least 50 ms, in the order they ran: when each started, counted from the start of
   *  navigation, and how long it held the thread. What the longest number alone cannot say is
   *  how many freezes a reader met and where. */
  readonly longTasks: readonly { readonly at: number; readonly ms: number }[];
  /** Whether the `longtask` observer was attached to the page at all. The two numbers above
   *  are a reading of nothing where it was not, so the rows that hold a build to a longest
   *  task read this beside them. */
  readonly longTasksWatched: boolean;
  /** How many viewport heights the scroll moved before it reached the end of the document or its
   *  cap, which says the page scrolled at all. */
  readonly scrollSteps: number;
  /** Milliseconds from the start of navigation until the window closed, which is what the
   *  longest task is the longest of. */
  readonly windowClosedMs: number;
  /** When each step of the document's own opening happened, on the page's own clock: the text
   *  read the admission check is, the socket, its first sync frame out and the state frame
   *  back, and the editor's root in the page. Asserted nowhere; printed, because what a
   *  measured first block is made of is the chain that drew it. A step that never happened is
   *  `undefined`. */
  readonly opening: DocumentOpening;
}

/**
 * Throttles `page`'s renderer through a CDP session of its own
 * (`page.context().newCDPSession(page)`, as `e2e/touch.ts` opens one). The session stays attached
 * for the page's life: detaching it would drop the override the measurement is taken under.
 * Chromium only, which is the engine the `iphone` project runs.
 *
 * The rate is the set-up's own unless the caller names another. A row that times nothing may:
 * what a heavier rate buys is a longer build, since a batch takes the blocks it can convert
 * inside its budget (`BATCH_BUDGET_MS` in `packages/proof-editor/src/progressive-build.ts`)
 * and a slower renderer converts fewer of them. A row that reads a clock may not: every number
 * R11 is held to is read under the rate the spec names.
 *
 * Set it for the build the row reads, not for the page's way to that build. It takes effect
 * from the call rather than from the navigation, and a row that holds the document's socket
 * (`documentTransport`'s `holding`) already has the two apart: no batch of the editor runs
 * until `release()`, so the page's boot, its own draw of the document and whatever the row
 * arranges over that draw are setup no reading is taken of, and a rate set before the
 * navigation charges every one of them what the build is charged. Measured on this pod, over
 * the whole of `large-documents-reader.e2e.ts`: 23.0 s of the two-people row's 102 s went on
 * that setup before the window it reads opened, against 58.3 s of build inside it. A row with
 * no hold has no such moment - its build begins with the sync its load asks for - and sets the
 * rate before the navigation, as it must.
 */
export async function throttleTheCpu(page: Page, rate = cpuThrottlingRate): Promise<void> {
  const session = await page.context().newCDPSession(page);
  await session.send("Emulation.setCPUThrottlingRate", { rate });
}

/**
 * Installs the recorders before the page's own scripts: the `longtask` observer, which takes
 * the entries buffered before it registered as well, a frame loop that records
 * `performance.now()` the first time the editor's first visible block has a box inside the
 * viewport - with what that block says about where it was drawn - the first time any draw of
 * the document has one and the first time the page's own draw of the text has one, and a timer
 * that records when the editor first has a box of its own under its name. Call it before the
 * navigation it measures.
 *
 * The three blocks are recorded independently: the two draws are almost always on the screen
 * first, and the loop goes on looking for the editor's own past both of them, which is the
 * number the targets are read against.
 *
 * The named editor's box is what says the build is over and the binding is live: the host names
 * the textbox only once the binding has answered, a task after the binding connected
 * (`web/src/features/doc/editor.ts`), so an editor with that name and a box is an editor whose
 * whole document is in it.
 *
 * The blocks are read every frame, because one of them is the number the targets are read
 * against and a frame is as often as the page can change. The editor's own box is read on a
 * timer instead: it is read for most of a build that can last ten seconds, each read is a
 * layout the page did not ask for, and a frame loop running through the build would both pay
 * that layout sixty times a second and keep the page rendering while the batches want the
 * thread. What it costs is a window that closes up to `editorPollMs` late, which is no target.
 *
 * It answers the two announcements the page makes as it writes those numbers, which is how the
 * run learns them (`listenForThePage`).
 */
export async function watchTheDocument(page: Page): Promise<Announcements> {
  const announcements = await listenForThePage(page);
  await page.addInitScript(
    ({
      drawn: drawnSelector,
      editor: editorSelector,
      first: firstSelector,
      live: liveSelector,
      painted: paintedSelector,
      pollMs,
      preview: previewRoots,
      root: rootSelector,
    }: {
      drawn: string;
      editor: string;
      first: string;
      live: string;
      painted: string;
      pollMs: number;
      preview: string;
      root: string;
    }) => {
      // The document's own socket, timed from inside the page. The page's transport subclasses
      // `globalThis.WebSocket` (`web/src/features/doc/connection.ts`) and a row can have put
      // Playwright's own WebSocket mock there first (`documentTransport` in `e2e/editor.ts`),
      // so this stands in front of the constructor as a `construct` proxy rather than as a
      // subclass of its own: `Reflect.construct` builds whatever is already there, with the
      // caller's `new.target`, so the instance is the one that class or mock would have made
      // and the transport's own `send` override keeps its place on the prototype chain. The
      // recorder's `send` sits on the instance in front of both and calls through to them.
      // Only the first document socket is recorded; a reconnect's is another socket and no
      // part of the opening this times.
      const opening: NonNullable<Window["__documentOpening"]> = {};
      window.__documentOpening = opening;
      window.WebSocket = new Proxy(window.WebSocket, {
        construct(made, args: [string | URL, (string | string[])?], asked) {
          const socket = Reflect.construct(made, args, asked) as WebSocket;
          if (!String(args[0]).includes("/ws/doc/") || opening.socketCreatedMs !== undefined) {
            return socket;
          }
          opening.socketCreatedMs = performance.now();
          socket.addEventListener("open", () => {
            opening.socketOpenedMs ??= performance.now();
          });
          socket.addEventListener("message", (event) => {
            if (typeof event.data !== "string") {
              opening.stateFrameMs ??= performance.now();
            }
          });
          const sent = Object.getPrototypeOf(socket).send as WebSocket["send"];
          socket.send = (data) => {
            if (typeof data !== "string") {
              opening.syncStep1Ms ??= performance.now();
            }
            sent.call(socket, data);
          };
          return socket;
        },
      });
      window.__largeDocumentLongestTask = 0;
      window.__largeDocumentLongTasks = [];
      new PerformanceObserver((entries) => {
        for (const entry of entries.getEntries()) {
          window.__largeDocumentLongestTask = Math.max(
            window.__largeDocumentLongestTask ?? 0,
            entry.duration
          );
          window.__largeDocumentLongTasks?.push({ at: entry.startTime, ms: entry.duration });
        }
      }).observe({ buffered: true, type: "longtask" });
      // Written after `observe` has returned, so it is the page's own word that the entries
      // below were being collected rather than this side's assumption that they were.
      window.__largeDocumentLongTasksWatched = true;
      const inTheViewport = (selector: string): Element | null => {
        const element = document.querySelector(selector);
        if (element === null) {
          return null;
        }
        const box = element.getBoundingClientRect();
        const seen =
          box.width > 0 && box.height > 0 && box.top < window.innerHeight && box.bottom > 0;
        return seen ? element : null;
      };
      const look = () => {
        if (opening.editorRootMs === undefined && document.querySelector(rootSelector) !== null) {
          // The editor's root is in the page from `constructEditor`, which builds it while the
          // socket syncs and before its binding has drawn a block, so this is when the editor
          // was constructed rather than when the reader could read it.
          opening.editorRootMs = performance.now();
        }
        if (
          window.__largeDocumentPaintedAt === undefined &&
          inTheViewport(paintedSelector) !== null
        ) {
          window.__largeDocumentPaintedAt = performance.now();
        }
        if (
          window.__largeDocumentPageDrawnAt === undefined &&
          inTheViewport(drawnSelector) !== null
        ) {
          window.__largeDocumentPageDrawnAt = performance.now();
        }
        const block = inTheViewport(firstSelector);
        if (block !== null) {
          // Read off the block itself rather than off the selector that found it, so the run
          // says which surface drew what it timed and the rows can hold it to the editor's.
          window.__largeDocumentFirstBlock = {
            at: performance.now(),
            editable: block.closest('[contenteditable="true"]') !== null,
            inLiveEditor: block.closest(liveSelector) !== null,
            inPreview: block.closest(previewRoots) !== null,
          };
          // Said in the task it was recorded in, so the number the run is given is the one
          // the page wrote rather than the one a later reading found (`listenForThePage`).
          window.__largeDocumentReached?.({ firstBlock: window.__largeDocumentFirstBlock });
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
      const lookForTheEditor = () => {
        const box = document.querySelector(editorSelector)?.getBoundingClientRect();
        if (box !== undefined && box.width > 0 && box.height > 0) {
          window.__largeDocumentEditorAt = performance.now();
          window.__largeDocumentReached?.({ editorOpenedMs: window.__largeDocumentEditorAt });
          return;
        }
        setTimeout(lookForTheEditor, pollMs);
      };
      setTimeout(lookForTheEditor, pollMs);
    },
    {
      drawn: pageDrawnSelector,
      editor: editorSelector,
      first: firstBlockSelector,
      live: liveEditorSelector,
      painted: firstPaintedSelector,
      pollMs: editorPollMs,
      preview: previewSelector,
      root: editorRootSelector,
    }
  );
  return announcements;
}

/**
 * What the page asked Dispatch for and when, and when the editor's first batch landed: the two
 * facts the rows that read the order of the page's reads are built on (dispatch://INFRA-341,
 * R11, "the issue page's non-document panels are drawn after the editor's first batch").
 *
 * Both come from the page itself. Each read is a `resource` entry of the browser's own
 * timing, taken through a `PerformanceObserver` rather than off the buffer, which holds 250
 * entries by default and which a document page fills; `startTime` is when the request went
 * out, which is the number the order is read on. The first batch is the task the editor's root
 * is given `data-write-path="live"` in (`@legion/proof-editor`'s `progressive-build.ts`), read
 * through a `MutationObserver`, whose callback runs at that task's own microtask checkpoint.
 * It runs before the commit that draws the chrome and starts the reads the page held, because
 * the batch's own blocks go into the editor before that attribute is set and queue this
 * observer there, while the page lets those reads go only in the callback the batch answers
 * with, which the build calls after both (`web/src/features/doc/ProofDocument.tsx`'s
 * `onBatch`): React's own microtask is therefore queued behind this one, and a read made at
 * the batch is never timed as one made before it.
 */
export interface PageRead {
  /** `performance.now()` when the request went out, counted from the start of navigation. */
  readonly at: number;
  /** The path it went out on, under `/api/v1`, with its query. */
  readonly path: string;
}

export interface ReadsOfThePage {
  /** `performance.now()` when the editor's first batch landed; undefined where none has. */
  readonly firstBatchMs: number | undefined;
  /** Every `/api/v1` request the page has made, in the order the browser timed them. */
  readonly reads: readonly PageRead[];
}

/** Installs those two recorders before the page's own scripts. Call it before the navigation
 *  whose reads the row is about; `readTheReads` reads what they recorded. */
export async function watchTheReads(page: Page): Promise<void> {
  await page.addInitScript(
    ({ live }: { live: string }) => {
      window.__largeDocumentReads = [];
      new PerformanceObserver((entries) => {
        for (const entry of entries.getEntries()) {
          const url = new URL(entry.name, window.location.origin);
          if (!url.pathname.startsWith("/api/v1/")) {
            continue;
          }
          window.__largeDocumentReads?.push({
            at: entry.startTime,
            path: `${url.pathname.slice("/api/v1".length)}${url.search}`,
          });
        }
      }).observe({ buffered: true, type: "resource" });
      const noteTheFirstBatch = () => {
        if (
          window.__largeDocumentFirstBatchAt === undefined &&
          document.querySelector(live) !== null
        ) {
          window.__largeDocumentFirstBatchAt = performance.now();
        }
      };
      new MutationObserver(noteTheFirstBatch).observe(document, {
        attributeFilter: ["data-write-path"],
        attributes: true,
        childList: true,
        subtree: true,
      });
    },
    { live: liveEditorSelector }
  );
}

/** What the two recorders above have so far. */
export async function readTheReads(page: Page): Promise<ReadsOfThePage> {
  return await page.evaluate(() => ({
    firstBatchMs: window.__largeDocumentFirstBatchAt,
    reads: window.__largeDocumentReads ?? [],
  }));
}

/** The two ends of a build, on the page's own clock: when its first batch landed and when the
 *  finished editor was named. A row that has to act inside a build acts between them. */
export interface TheBuildsWindow {
  /** `performance.now()` when the editor's root was first given `data-write-path="live"`,
   *  which is the task the first batch's blocks landed in; undefined until one has. */
  readonly firstBatchMs: number | undefined;
  /** `performance.now()` when the finished editor was first named, which is the end of the
   *  build (`namedEditorSelector`); undefined while it is still running. */
  readonly namedMs: number | undefined;
}

/** What `watchTheBuildsWindow` answers: a wait for the build's own start, and a reading of the
 *  window as the page has it. */
export interface TheBuildWatched {
  /** Resolves with `performance.now()` at the editor's first batch, which the page announces
   *  in the task that batch landed in. A wait rather than a poll, for the reason
   *  `theMomentThePageReached` gives: between the batches of a build a poll can only say the
   *  page had reached it by the time the poll ran, and a row that has to act inside the build
   *  has the whole of that lateness taken off its window. Fails where no batch lands inside
   *  `timeoutMs`, naming what never came. */
  readonly firstBatch: (timeoutMs: number) => Promise<number>;
  /** Both ends as the page has them now: read once the row has acted, so the window it acted
   *  in is printed rather than asserted. */
  readonly window: () => Promise<TheBuildsWindow>;
}

/**
 * Watches the build of `page`'s editor: the first batch, which the page announces as it lands,
 * and the naming of the finished editor, which it records.
 *
 * Both are read through one `MutationObserver` over the whole document, whose callback the
 * browser runs at the microtask checkpoint of the task that made the change, so each moment is
 * the page's own task rather than the next reading's. The first batch is the editor's root
 * taking `data-write-path="live"` (`live()` in `packages/proof-editor/src/lib.ts`, in the
 * batch's own task) and the naming is the host writing the editor's name onto the view's DOM
 * once the binding has answered its handle (`web/src/features/doc/editor.ts`), which is three
 * tasks past the last batch.
 *
 * Install before the navigation whose build the row acts inside.
 */
export async function watchTheBuildsWindow(page: Page): Promise<TheBuildWatched> {
  const landed = Promise.withResolvers<number>();
  await page.exposeFunction("__theFirstBatchLanded", (at: number) => landed.resolve(at));
  await page.addInitScript(
    ({ live, named }: { live: string; named: string }) => {
      const window_ = { firstBatchMs: undefined, namedMs: undefined } as {
        firstBatchMs: number | undefined;
        namedMs: number | undefined;
      };
      window.__theBuildsWindow = window_;
      const look = () => {
        if (window_.firstBatchMs === undefined && document.querySelector(live) !== null) {
          window_.firstBatchMs = performance.now();
          // Said in the task the batch landed in, so the run is told rather than asking.
          window.__theFirstBatchLanded?.(window_.firstBatchMs);
        }
        if (window_.namedMs === undefined && document.querySelector(named) !== null) {
          window_.namedMs = performance.now();
        }
      };
      // The document itself, because this script runs before the page has an element to watch.
      new MutationObserver(look).observe(document, {
        attributeFilter: ["aria-label", "data-write-path"],
        attributes: true,
        childList: true,
        subtree: true,
      });
    },
    { live: liveEditorSelector, named: editorSelector }
  );
  return {
    firstBatch: (timeoutMs) =>
      theMomentThePageReached(
        landed.promise,
        timeoutMs,
        "the editor's first batch never landed, so the page never began the build this row acts inside"
      ),
    window: async () =>
      await page.evaluate(() => ({
        firstBatchMs: window.__theBuildsWindow?.firstBatchMs,
        namedMs: window.__theBuildsWindow?.namedMs,
      })),
  };
}

/**
 * When each part of the chrome the page defers first stood in the page, on the page's own
 * clock: the shell's own parts and the issue page's header, tab strip and artifact chrome,
 * which are drawn after the editor's first batch (dispatch://INFRA-341 R11,
 * `web/src/features/doc/AfterTheFirstBatch.tsx`).
 *
 * Each part is named by what the reader sees - its own element in the page - never by a class
 * the build renames, and each is recorded in the `MutationObserver` callback the browser runs
 * at the drawing task's own microtask checkpoint, so a part drawn in the batch's own task is
 * never timed later than it.
 */
export async function watchTheChrome(
  page: Page,
  parts: Readonly<Record<string, string>>
): Promise<void> {
  await page.addInitScript((watched: Readonly<Record<string, string>>) => {
    const drawn: Record<string, number> = {};
    window.__chromeDrawn = drawn;
    const look = () => {
      for (const [name, selector] of Object.entries(watched)) {
        if (drawn[name] === undefined && document.querySelector(selector) !== null) {
          drawn[name] = performance.now();
        }
      }
    };
    new MutationObserver(look).observe(document, {
      attributes: true,
      childList: true,
      subtree: true,
    });
    look();
  }, parts);
}

/** What `watchTheChrome` has recorded so far: the parts that have been drawn, by name. */
export async function readTheChrome(page: Page): Promise<Record<string, number>> {
  return await page.evaluate(() => window.__chromeDrawn ?? {});
}

/**
 * Reads the opening timeline off `page`: what the socket recorder above wrote, and the two
 * numbers the browser's own resource timing holds - when the document's text was asked for and
 * when it answered, which is the schema admission check nothing connects before
 * (`packages/dispatch/AGENTS.md`). Both text routes are read, since a Spec tab's address names
 * no slug and its text is read by the id the issue answers with.
 */
export async function readTheOpening(page: Page): Promise<DocumentOpening> {
  return await page.evaluate(() => {
    const text = /^\/api\/v1\/(issues\/[^/]+\/artifacts\/[^/]+|artifacts\/[^/]+)\/text$/;
    const read = (performance.getEntriesByType("resource") as PerformanceResourceTiming[]).find(
      (resource) => text.test(new URL(resource.name).pathname)
    );
    const opening = window.__documentOpening ?? {};
    return {
      editorRootMs: opening.editorRootMs,
      socketCreatedMs: opening.socketCreatedMs,
      socketOpenedMs: opening.socketOpenedMs,
      stateFrameMs: opening.stateFrameMs,
      syncStep1Ms: opening.syncStep1Ms,
      textAnsweredMs: read?.responseEnd,
      textAskedMs: read?.startTime,
    };
  });
}

/** What a caller gives the measurement beyond the page and the path: how long each of its two
 *  waits may last. Each is a wait for something that never came, so a failure reads as the miss
 *  it is rather than as a hang; a build that draws at all is read against the target. */
export interface MeasurementWaits {
  /** How long to wait for the editor to open, which is the end of its build. */
  readonly buildTimeoutMs: number;
  /** How long to wait for the document's first block to come into the viewport. */
  readonly firstBlockTimeoutMs: number;
}

/**
 * Opens `path` and measures it: the navigation starts the page's clock, the editor's first
 * block ends the first number, the editor opening ends the second, and the scroll to the end of
 * the document then closes the window the third is read over; the fourth, the reader's first
 * block from any draw, is read off the page at the end. The navigation waits for the response
 * alone (`commit`), so a document whose `load` lands long after its first block does not hold
 * the measurement up.
 *
 * The scroll waits for the editor rather than racing it, so the window covers the whole build
 * and then a scroll through the whole document (the module comment above).
 *
 * Neither wait reads the page. Both are announcements the page makes in the task it records
 * the number in, so each is the moment the page reached it rather than the moment a reading
 * found it, which between the batches of a throttled build is hundreds of milliseconds apart
 * (`listenForThePage`). What is read below is read once the editor has opened, which is when
 * the page has its thread back.
 */
export async function measureDocument(
  page: Page,
  path: string,
  { buildTimeoutMs, firstBlockTimeoutMs }: MeasurementWaits
): Promise<DocumentMeasurement> {
  await throttleTheCpu(page);
  const reached = await watchTheDocument(page);
  await page.goto(path, { waitUntil: "commit" });

  const firstBlock = await theMomentThePageReached(
    reached.firstBlock,
    firstBlockTimeoutMs,
    "the editor's first visible block never came into the viewport: either no editor " +
      'root ever carried `data-write-path="live"` with a block in it, or a taller page ' +
      "header has pushed that block under the fold, where no scroll of this measurement " +
      "reaches it"
  );

  const editorOpenedMs = await theMomentThePageReached(
    reached.editorOpened,
    buildTimeoutMs,
    "the editor never opened, so there was no build to measure and no document to scroll: " +
      "the page is still the document's own draw of its first blocks"
  );

  // The loop runs in the page, so its steps are the page's own 100 ms apart rather than a round
  // trip's, and a step the throttled main thread runs late delays the next step rather than
  // piling up behind it.
  const scrollSteps = await page.evaluate(
    ({ cap, stepMs }) => {
      const scrolled = Promise.withResolvers<number>();
      let steps = 0;
      const step = () => {
        const root = document.scrollingElement;
        if (root === null) {
          scrolled.resolve(steps);
          return;
        }
        if (steps >= cap || root.scrollTop + root.clientHeight >= root.scrollHeight - 1) {
          scrolled.resolve(steps);
          return;
        }
        steps += 1;
        root.scrollTop += root.clientHeight;
        setTimeout(step, stepMs);
      };
      setTimeout(step, stepMs);
      return scrolled.promise;
    },
    { cap: scrollStepsMax, stepMs: scrollStepMs }
  );

  const longestTaskMs = await page.evaluate(() => window.__largeDocumentLongestTask);
  const longTasks = await page.evaluate(() => window.__largeDocumentLongTasks);
  const longTasksWatched = await page.evaluate(
    () => window.__largeDocumentLongTasksWatched === true
  );
  const windowClosedMs = await page.evaluate(() => performance.now());
  const firstPaintedMs = await page.evaluate(() => window.__largeDocumentPaintedAt);
  const pageDrawnMs = await page.evaluate(() => window.__largeDocumentPageDrawnAt);
  const opening = await readTheOpening(page);
  if (longestTaskMs === undefined || longTasks === undefined) {
    throw new Error("the page lost its recorders: an init script never ran");
  }
  return {
    editorOpenedMs,
    firstBlock: {
      editable: firstBlock.editable,
      inLiveEditor: firstBlock.inLiveEditor,
      inPreview: firstBlock.inPreview,
    },
    firstBlockMs: firstBlock.at,
    firstPaintedMs,
    pageDrawnMs,
    longestTaskMs,
    longTasks,
    longTasksWatched,
    scrollSteps,
    windowClosedMs,
    opening,
  };
}

/** How many readings a measuring row takes of one document, and what its targets are then read
 *  against: the median of them. The protocol is the spec's own - "Each number is the median of
 *  3 runs" (dispatch://INFRA-341/spec, the Design paragraph R11 binds its targets to) - and
 *  what asks for it is this file's own spread: the same two rows at one head read 4,824 ms and
 *  8,311 ms on a first attempt and 3,838 ms and 5,105 ms on the retry
 *  (`e2e/playwright.config.ts`), so one reading held to a target is a target thrown dice for.
 *  An odd count, so the median is a reading the row really took rather than a mean of two that
 *  happened to neighbour it.
 *
 *  Each reading is a page of its own. `measureDocument` times one navigation, and a second
 *  `goto` in the same page would be timed against a browser that already holds the document's
 *  text, its chunks and its blocks - a faster number that is not the reader's first visit. */
export const measuredReadings = 3;

/** What the line below says about itself, for a reader who has not read this file: it is quoted
 *  in a pull request body and in a Dispatch check result, where the numbers have to tell
 *  themselves apart and say which of them a run is held to. */
const measurementLegend =
  "The first editable block is the editor's own first visible block, the first block a reader " +
  'can type in (its root carries `data-write-path="live"`); the median of that number over a ' +
  "row's readings, and the median of the longest main-thread task, are the two read against " +
  "R11's targets. No reading is held to a target on its own - the median line below the " +
  "readings is the one the row asserts - and the only thing the rows assert of a reading is " +
  "that the page recorded a main-thread task at all, since a longest task read off nothing " +
  "meets any target. " +
  "First painted is the first block of the document on the screen from any draw, " +
  "which is the server's own where it drew the document's opening blocks into the page it " +
  "sent. The page's own draw is the first block the app itself put on the screen, in the " +
  "read-only preview inside the document surface it renders: that one is the number this rig " +
  "read as the editor's before INFRA-341 corrected it. Neither draw takes an edit. The " +
  "opening is the document's own chain on the same clock, step by step: the text read the " +
  "schema admission check is, the socket, its SyncStep1, the state frame back, and the " +
  "editor's root in the page.";

/** The opening timeline as one phrase of the line above: each step and when it happened, in
 *  the order they happen, with the steps that never happened named as such. */
function openingLine(opening: DocumentOpening): string {
  const steps: [string, number | undefined][] = [
    ["text asked", opening.textAskedMs],
    ["text answered", opening.textAnsweredMs],
    ["socket created", opening.socketCreatedMs],
    ["socket opened", opening.socketOpenedMs],
    ["SyncStep1 out", opening.syncStep1Ms],
    ["state frame in", opening.stateFrameMs],
    ["editor created", opening.editorRootMs],
  ];
  return steps
    .map(([step, at]) => `${step} ${at === undefined ? "never" : `${Math.round(at)} ms`}`)
    .join(" / ");
}

/**
 * Puts one reading where the run's reader finds it: on the row's annotations, which the report
 * keeps with the result, and on stdout, which `--reporter line` prints as the row runs. Nothing
 * asserts the numbers on this line; `firstBlockTargetMs` is printed beside the first of them so
 * a reader sees what the row's median of these readings will be held to, and `reading` says
 * which of the row's readings this is.
 */
export function printMeasurement(
  what: string,
  measured: DocumentMeasurement,
  { firstBlockTargetMs, reading }: { firstBlockTargetMs: number; reading: number }
): void {
  const over = measured.longTasks.filter(({ ms }) => ms > longTaskTargetMs);
  const line =
    `${what}, reading ${reading} of ${measuredReadings}: ` +
    `first editable block (this reading not asserted, the median's target ` +
    `${firstBlockTargetMs} ms) ${Math.round(measured.firstBlockMs)} ms, ` +
    `longest main-thread task (this reading not asserted, the median's target ` +
    `${longTaskTargetMs} ms) ${Math.round(measured.longestTaskMs)} ms, ` +
    `first painted (not asserted) ${measured.firstPaintedMs === undefined ? "never" : `${Math.round(measured.firstPaintedMs)} ms`}, ` +
    `page's own draw (not asserted) ${measured.pageDrawnMs === undefined ? "never" : `${Math.round(measured.pageDrawnMs)} ms`}, ` +
    `editor open (not asserted) ${measured.editorOpenedMs === undefined ? "never" : `${Math.round(measured.editorOpenedMs)} ms`}, ` +
    `opening (not asserted) ${openingLine(measured.opening)}, ` +
    `${measured.longTasks.length} main-thread task(s) over 50 ms and ${over.length} over ` +
    `${longTaskTargetMs} ms` +
    (over.length === 0
      ? ""
      : ` (${over.map(({ at, ms }) => `${Math.round(ms)} ms at ${Math.round(at)} ms`).join("; ")})`) +
    `, over ${measured.scrollSteps} scroll steps in a ${Math.round(measured.windowClosedMs)} ms window. ` +
    measurementLegend;
  test.info().annotations.push({ description: line, type: "R11 measurement" });
  console.log(`R11 measurement · ${line}`);
}

/** The two numbers a measuring row holds to its targets, each the median of its readings. */
export interface MeasuredMedian {
  /** The median first editable block, in milliseconds from the start of navigation. */
  readonly firstBlockMs: number;
  /** The median longest main-thread task of a reading's window, in milliseconds. */
  readonly longestTaskMs: number;
}

/** The middle of an odd number of readings. */
function middleOf(numbers: readonly number[]): number {
  const sorted = [...numbers].sort((first, second) => first - second);
  return sorted[(sorted.length - 1) / 2] ?? Number.NaN;
}

/**
 * The median of the readings a row took, printed beside every one of them and answered to the
 * row, which holds that median to the targets and the readings themselves to nothing. The
 * readings are printed by `printMeasurement` as each is taken, so this line is read under them
 * and says what the spread beneath it came to.
 */
export function printTheMedian(
  what: string,
  readings: readonly DocumentMeasurement[],
  firstBlockTargetMs: number
): MeasuredMedian {
  const firstBlocks = readings.map(({ firstBlockMs }) => firstBlockMs);
  const longestTasks = readings.map(({ longestTaskMs }) => longestTaskMs);
  const median = {
    firstBlockMs: middleOf(firstBlocks),
    longestTaskMs: middleOf(longestTasks),
  };
  const spread = (numbers: readonly number[]) =>
    numbers.map((ms) => `${Math.round(ms)}`).join(", ");
  const line =
    `${what}: first editable block (asserted, target ${firstBlockTargetMs} ms) ` +
    `${Math.round(median.firstBlockMs)} ms, the median of ${spread(firstBlocks)} ms; ` +
    `longest main-thread task (asserted, target ${longTaskTargetMs} ms) ` +
    `${Math.round(median.longestTaskMs)} ms, the median of ${spread(longestTasks)} ms. ` +
    "Each reading is one navigation of a page of its own, printed in full above and held to " +
    "nothing: what a reader of this line is owed is the spread the median came out of, which " +
    "a single number hides.";
  test.info().annotations.push({ description: line, type: "R11 median" });
  console.log(`R11 median of ${readings.length} readings · ${line}`);
  return median;
}
