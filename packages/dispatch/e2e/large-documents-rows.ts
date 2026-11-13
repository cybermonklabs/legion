// What the rows of the eight `large-documents-*.e2e.ts` files share: the seed every one of them
// writes, the readings the page leaves on `window` for them, the three surfaces a block of the
// document can stand on, the other person who writes into the document while a reader's
// editor is built (`theOtherPersonsPen`), and the probe a build is read through at the end of
// this file (`whatTheBuildServes`). A helper here is one more than one of those files needs; a
// helper one file needs stays in that file.
//
// They were one file until the `iphone` half of it cost a browser shard 610 s of a step that is
// given 540 (dispatch://INFRA-341, run 42794865122's `shard (4)`), and Playwright hands a
// file's rows to one shard, so a file larger than the budget fits in none of them. Three of
// the eight are read from the wall clock, and each of those is one project's whole selection,
// because a wall-clock project selects by file (`clockSpecNames`, e2e/configurations.ts):
// `large-documents-measured.e2e.ts` (`iphone-measure-alone`: the two measurements R11's
// targets are read from), `large-documents-build-serves.e2e.ts` (`iphone-alone`: what a build
// serves the rest of the page while it runs) and `large-documents-desktop-build.e2e.ts`
// (`chromium-alone`: the longest main-thread task of a build at a desktop width). The five the
// parallel projects run are `large-documents-long-build.e2e.ts` (what a build of the larger
// document costs the page, with no number read from the clock),
// `large-documents-build.e2e.ts` (what stands on the reader's screen while
// the editor is built), `large-documents-reader.e2e.ts` (what a reader does with a document
// whose editor is still being built, and the rest of acceptance A7),
// `large-documents-first-screen.e2e.ts` (the draw the server sends, and the reads and the
// chrome the page holds until the editor's first batch) and
// `large-documents-fold.e2e.ts` (where that chrome leaves the server's first block).
//
// The documents themselves are `large-documents.ts`, and what a page announces about its own
// draw and its build is `large-documents-page.ts`.
import { HocuspocusProvider, HocuspocusProviderWebsocket } from "@hocuspocus/provider";
import { expect, type Page } from "@playwright/test";
import * as Y from "yjs";

import { FRAGMENT_NAME } from "../web/src/features/doc/roots";
import {
  baseUrl,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifactBlocks,
  getBlockSchema,
  sessionCookie,
} from "./api";
import { theMomentThePageReached } from "./large-documents-page";
import { sql } from "./psql";
import { resetDatabase } from "./seed";
/** What one animation frame of the build held: how many of the document's top-level blocks
 *  stood on the screen across the three surfaces that draw them, how many of those the editor
 *  had built, and any block id two surfaces held at once. `framesOfTheBlocks` records one of
 *  these per frame and the count row reads them. */
export interface BlockFrame {
  readonly blocks: number;
  readonly built: number;
  readonly twice: readonly string[];
}

/** What one task of the page drew: how many top-level blocks the editor gained in it, and how
 *  many decision cards were mounted into the shells of their blocks. A `MutationObserver`
 *  delivers every record of one task in one callback, at that task's own microtask checkpoint,
 *  so one of these is one task's work and nothing else's. `drawsOfEachTask` records them and
 *  the decision-card row of `large-documents-build.e2e.ts` reads them. */
export interface DrawnInATask {
  /** When the task's changes were read, counted from the start of navigation. */
  readonly at: number;
  readonly blocks: number;
  readonly cards: number;
}

/** What one task the page was given while the editor was being built cost it: when it was
 *  given, on the page's own clock, and how long it then waited before it ran. The timer row of
 *  `large-documents-build-serves.e2e.ts` and the desktop guard of
 *  `large-documents-desktop-build.e2e.ts` read these - one per timer
 *  `whatTheBuildServes` hands over, and one per word another person wrote, which the timer row
 *  builds from the stamp the writer's page took as it sent the word and the stamp the
 *  reader's page took as it served the frame carrying it. */
export interface ServedLate {
  /** `performance.now()` when the task was given: when `setTimeout` was called for a timer,
   *  and the moment the other person's page sent the word, counted from the reader's page's
   *  time origin, for one of their words. */
  readonly handedAt: number;
  /** How many top-level blocks the editor held when it was handed over, for a timer the probe
   *  below hands over; `undefined` for one of their words, which arrives when it arrives. */
  readonly builtThen?: number;
  /** How long it waited from then until it ran. */
  readonly waitedMs: number;
}

/** When the reader's page served the frame carrying one of another person's words: the wall
 *  clock it served it on, which `large-documents-build-serves.e2e.ts`'s timer row prints, and
 *  the turn of the page's own loop it was served in, which is the whole of what that row
 *  reads. A turn is a
 *  number the page hands itself, one per task (`whatTheBuildServes` there): a word whose turn
 *  is the swap's own or an earlier one was served by the build, and one whose turn is later
 *  was served by a page that had already taken a fresh turn of work ahead of it. */
export interface ServedWord {
  readonly atMs: number;
  readonly turn: number;
}

/** The build's own window on the page's clock, as `whatTheBuildServes` sees it: the frame its
 *  first batch was found in, the frame the finished editor was named in, the moment the page
 *  was free again after the swap that ends a build - the adoption, the reveal and the margin's
 *  first placement - which the page says by serving a task it was handed in that frame, and
 *  the end of the turn it said that in, which the page says by serving a task it handed over
 *  from inside that turn. A reader's build is over at the third of these, not the second.
 *
 *  `swapTurn` is the turn that third stamp was written in, and it is the only one of these the
 *  liveness reading uses. The three clock stamps are printed - where the swap's turn ended,
 *  how far past the window that was, and the margin - and held to nothing. */
export interface TheBuildRan {
  firstBatchMs?: number;
  openedMs?: number;
  swappedMs?: number;
  swapTurn?: number;
  freeTurnEndedMs?: number;
}

/** Where the editor was when another person's word first showed up in its first block: how
 *  many blocks it held, and whether its build was over by then. */
interface TheirWordLanded {
  readonly blocks: number;
  readonly open: boolean;
}

/** What the page recorded of the answer it pressed while the editor was being built: the press
 *  the page received, how much of the document the editor held then, and whether the build was
 *  still running. */
interface TheAnswerPressed {
  /** The click event's own `timeStamp`, which the browser sets when it makes the event. */
  readonly at: number;
  /** How many top-level blocks the editor held when the press arrived. */
  readonly built: number;
  /** Whether the build was still running then: the finished editor had not been named. */
  readonly building: boolean;
}

/** What the page recorded for one of the edits a reader makes while the editor is being built
 *  (`ReaderEdit` in `large-documents-reader.e2e.ts`): what it did, how many blocks the editor
 *  held when it did it - which is what says the build was still running - and how many it held
 *  afterwards, which says what the edit did to the document's shape. */
export interface EditMade {
  readonly after: number;
  readonly built: number;
  readonly what: string;
}

declare global {
  interface Window {
    /** How many of the document's blocks the editor's root held at each animation frame, which
     *  `framesOfTheBuild` records and the two progressive-build rows read. */
    __blocksBuilt?: number[];
    /** What each animation frame of the build held, which `framesOfTheBlocks` records and the
     *  count row reads. */
    __blockFrames?: BlockFrame[];
    /** What each animation frame of one reader's build held while another person wrote into
     *  the same document: how many blocks her editor held, whether it was open, and the wall
     *  clock the frame ran at, which is the clock the other page's writes are stamped on
     *  (`framesOfHerBuild` in `large-documents-reader.e2e.ts`). */
    __herBuildFrames?: { at: number; built: number; open: boolean }[];
    /** What the row that answers a decision inside a build recorded: the press the page made
     *  in the task the build's first batch landed in, `performance.now()` when the request
     *  carrying the answer left the page, and why the page pressed nothing where the card it
     *  was given was no longer there to press. */
    __answerPressed?: TheAnswerPressed;
    __answerSentMs?: number;
    __answerRefused?: string;
    /** What each task of the build drew, which `drawsOfEachTask` records and the decision-card
     *  row reads. */
    __drawnInTasks?: DrawnInATask[];
    /** What the page made of the edits `readerEditsTheBuild` gave it, and the refusal where
     *  the editor would not take one. */
    __readerEdits?: EditMade[];
    __readerEditsRefused?: string;
    /** What the no-pixels row records about the words its reader is looking at: how far they
     *  ever moved on the screen, and whether the page above them ever changed height at all,
     *  which is what makes the row's answer mean anything. `started` is false where the row
     *  never found its heading. */
    __readersPlace?: { grew: boolean; moved: number; started: boolean };
    /** What the chrome's no-pixels row records about the document's own first block: where
     *  its top stood on the reader's screen when the row began, how far it ever moved from
     *  there, and whether the page above the reader ever changed height at all, which is what
     *  makes the row's answer mean anything. */
    __firstBlockPlace?: { grew: boolean; moved: number; started: number };
    /** What `whatTheBuildServes` recorded: how long each timer the page handed the browser
     *  waited to be run, the turn of the page's own loop each word another person wrote was
     *  served in - with `Date.now()` as it served the frame carrying it, which is printed and
     *  nothing more - and the build's own window, which says which of them the build was
     *  given. The same wall clock is stamped on the other side by the writer himself, as the
     *  frame carrying each word leaves him (`theOtherPersonsPen` below), which is the half of
     *  a word's wait no reading inside this page can take. */
    __servedTimers?: ServedLate[];
    __theirWordsServed?: Record<string, ServedWord>;
    __theBuildRan?: TheBuildRan;
    /** How far the editor's build has got, as `whatTheBuildServes` says it: its own top-level
     *  block count each time that count crosses the next step the liveness row of
     *  `large-documents-build-serves.e2e.ts` writes the other person's words by, and again -
     *  with `itsStop` - when it reaches the blocks that row stops his writing at or when the
     *  editor was named without ever reaching them. Said in the task the page read the count
     *  in. An announcement and not a reading the run takes, for the reason `watchTheDocument`
     *  in `large-documents-page.ts` gives - a page building the rest of a 406,000-character
     *  document answers nothing the run asks it until that build is over, which is the whole
     *  of what the row is arranging its window against. */
    __theBuildReached?: (built: number, itsStop: boolean) => void;
    /** Where the editor was when another person's word reached its first block
     *  (`whenTheirWordLands`). */
    __theirWord?: TheirWordLanded;
  }
}

export const agent = {
  actor: { id: "e2e-large-documents-agent", kind: "session" as const },
  as: "agent" as const,
};

/** How long `everythingOwedFinished` waits for the server to owe nothing.
 *
 *  It runs in every leg a large-documents row runs in: inside `seedDocument`, so in the
 *  shards (540 s, one retry) and in each `-alone` leg (`iphone-measure-alone`'s 180 s, and 240 s
 *  for `iphone-alone`, `iphone-long-build-alone` and `chromium-alone`;
 *  .github/workflows/dispatch-browser.yaml), and after a write in
 *  `large-documents-long-build.e2e.ts` and `large-documents-reader.e2e.ts`. On this pod, one
 *  worker on two CPUs (dispatch://INFRA-341), it took 1,013 to 1,036 ms after seeding the larger
 *  document, 508 to 529 ms after seeding the smaller one, and 6 to 10 ms after the writes those
 *  two files make during a build. 30 s is about thirty times the slowest.
 *
 *  A settlement that never empties then fails the row 30 s past where its wait began, and
 *  every leg has that room at its worst: the last seeding of `iphone-long-build-alone` begins
 *  about 155 s in (7 s of step and its first two rows' 147.9 s), and fails by 185 s of 240; the
 *  wait after the second row's write comes 10 + 42.3 + 30 s into that row, with 124.4 s left to
 *  it, and fails by 112.3 s; `iphone-measure-alone`'s second row seeds after the first row's
 *  48.6 s and fails by 86 s of 180; `iphone-alone`'s second row by 47 s of 240; and
 *  `chromium-alone`'s desktop row, after the 116 s of that leg's other rows, by 146 s. */
const owedWorkFinishesWithinMs = 30_000;

/**
 * Resolves once the server owes nothing this seed left it: no document settlement queued or
 * running, and no document owed a block index. A document of this size defers its block index
 * (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`), and that work costs the same machine
 * the browser runs on, so a measurement taken while it runs measures the reconciliation too.
 * The wait runs against the one database this run holds (`sql`), and its callers are this
 * file's own `seedDocument`, `large-documents-long-build.e2e.ts` and
 * `large-documents-reader.e2e.ts`. It fails by name past `owedWorkFinishesWithinMs`.
 */
export async function everythingOwedFinished(what: string): Promise<void> {
  await expect
    .poll(
      () =>
        sql(
          "select (select count(*) from doc_settlements_pending) || ' settlements, ' || " +
            "(select count(*) from document_block_versions where version is null) || ' block indexes'"
        ),
      {
        intervals: [500],
        message: `${what}: the write's own work never finished`,
        timeout: owedWorkFinishesWithinMs,
      }
    )
    .toBe("0 settlements, 0 block indexes");
}

/**
 * Writes `content` as an issue document and answers the issue it belongs to and the path a
 * reader opens it at.
 *
 * The issue's title and the document's file name are both short, and both rows use names of the
 * same length. At 390 px the document's first block begins 650 px down a 664 px viewport, and
 * every line either of them wraps to raises the header by about 30 px: a two-line title or file
 * name puts the first block under the fold, where it never comes into the viewport without a
 * scroll and there is no first number to read. Which document a row measures is in the row's
 * own title, not in what it seeds.
 */
export async function seedDocument(
  name: string,
  content: string
): Promise<{ artifactId: string; issueKey: string; path: string }> {
  await resetDatabase();
  await createProject({ key: "BIG", name: "Big documents" });
  const issue = await createIssue({ project: "BIG", title: "A document" }, agent);
  const uploaded = await createIssueArtifact(issue.key, { content, name }, agent);
  await everythingOwedFinished(name);
  return {
    artifactId: uploaded.artifact.id,
    issueKey: issue.key,
    path: `/issues/${issue.key}/artifacts/${uploaded.artifact.slug}`,
  };
}

/** How many of the document's blocks the editor's root holds, at whatever depth. The view wears
 *  its `Document editor` name only once the build is over, and its root is hidden until then,
 *  so neither `documentEditor` nor a visibility check reads a build in progress. */
export function blocksBuilt(page: Page): Promise<number> {
  return page.locator(".proof-editor [data-block-id]").count();
}

/**
 * Records, every animation frame until the editor is named, how many of the document's blocks
 * the editor's root holds, and answers what was recorded.
 *
 * The reading is the page's own rather than a poll from here, because a poll reads the build
 * from outside it: a `page.evaluate` is one task among the page's own, and the count it comes
 * back with is whatever the batches had reached by the time that task ran, never the frame a
 * reader was shown. A frame is the reading a reader gets, and a frame that holds part of a
 * document is the proof this row wants, since a build that froze the page would have drawn no
 * frame at all. Install before the navigation; read once the editor is open.
 */
export async function framesOfTheBuild(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const built: number[] = [];
    window.__blocksBuilt = built;
    const look = () => {
      const root = document.querySelector(".proof-editor");
      if (root !== null) {
        built.push(root.querySelectorAll("[data-block-id]").length);
        // The last frame worth reading is the first one the finished editor is named in: after
        // it every frame would pay for a scan of the whole document and read the same number.
        if (root.querySelector('[aria-label="Document editor"]') !== null) return;
      }
      requestAnimationFrame(look);
    };
    requestAnimationFrame(look);
  });
}

/** Each surface a top-level block of the document can stand on the screen in, in the order the
 *  page holds them: the blocks the server drew into the page it sent
 *  (`web/src/features/doc/first-screen.ts`), the page's own draw of the document's text, and
 *  the editor, whose root stands before that draw while the build fills it from the top. */
export const serverDrawnBlocks = "#first-screen > * > [data-block-id]";
export const drawnBlocks = '[aria-label="Document text"] > [data-block-id]';
export const builtBlocks = '.proof-editor [contenteditable="true"] > [data-block-id]';

/**
 * How many top-level blocks the server's index of `artifactId` holds: every block whose range
 * no other block's range contains. That index is the server's own parse of the document, so the
 * count is the document's, not this module's reading of the generator that spelled it, and it is
 * the number of blocks each draw of the document draws.
 */
export async function topLevelBlocks(artifactId: string): Promise<number> {
  const blocks = await getArtifactBlocks(artifactId);
  return blocks.filter(
    (block) =>
      !blocks.some(
        (other) =>
          other.id !== block.id &&
          other.from <= block.from &&
          other.to >= block.to &&
          (other.from < block.from || other.to > block.to)
      )
  ).length;
}

/** A document of one short section, so these rows seed in a moment: what they read is the
 *  order the page does things in, which no length of document changes. */
export const shortDocument = "# The lease ledger\n\nThe paragraph a reader meets first.\n";

/** How long the other person is given to be admitted to the document's room
 *  (`theOtherPersonsPen`): authenticated, and his copy synced. That is a round trip and one
 *  state frame, which took 278 ms for the larger document on this pod, one worker at a time
 *  (dispatch://INFRA-341); 30 s is a hundred times that, and short of each leg's budget whatever
 *  row he writes for. */
const admissionTimeoutMs = 30_000;

/** The other person, writing into the document from this process. */
export interface TheOtherPersonsPen {
  /** Leaves the document's room and drops his copy of it. */
  close(): void;
  /** When each word left, by the word: `Date.now()` as the frame carrying it went out on his
   *  socket, which is the same machine's clock her page stamps its own serving on. Only the
   *  words of the pattern the pen was given (`theirWords`), and empty where it was given none. */
  readonly sent: Readonly<Record<string, number>>;
  /** Writes one word into the document, as one insert and so one frame on her socket. */
  write(word: string): void;
}

/** What a pen is made of, for the one row that reads them: the probe of
 *  `large-documents-build.e2e.ts` holds these over a room that refuses him and reads that each
 *  is gone. Nothing else is given them - a row that writes uses `write` and `close`. */
export interface ThePenIsMadeOf {
  readonly doc: Y.Doc;
  readonly provider: HocuspocusProvider;
  readonly socket: HocuspocusProviderWebsocket;
}

/**
 * The other person as a Node client of the document's own websocket: he joins her room, writes
 * a word when her build asks for one, and stamps each word as the frame carrying it leaves
 * him.
 *
 * **Why he is not a second page.** He was one until this head, and what that measured was the
 * harness. His page held the same document hers does - 406,000 characters in the rows that
 * seed the larger one - so one of his words cost a layout of all of it, 1.2 s on this pod and
 * 232 ms on CI's own `iphone-alone` runner, and that cost, never her build, is what capped the
 * words a window could carry: CI's leg read one word against a floor of two and failed on its
 * own premise (run 43265368727). His speed was an artifact of the instrument and no fact about
 * her page, so the instrument is what moved. Everything her page does, the sent-time
 * population, the turn-exact reading and the floor are what they were, and a writer whose word
 * is an insert and a frame answers every step her build crosses, which puts more arriving work
 * inside that build than a browser peer could: a page that starves its socket fails harder.
 *
 * It is this module's, rather than one file's, because three rows write as the other person
 * now - the timer row of `large-documents-build-serves.e2e.ts`, the edit row of
 * `large-documents-long-build.e2e.ts` and the edit row of
 * `large-documents-build.e2e.ts` - and a second browser page of the same document is the
 * expensive instrument for each of them. (`large-documents-reader.e2e.ts`'s two people need
 * the editor's own keymaps, Enter and Backspace, and keep their page.)
 *
 * **How he joins.** The stack her page connects with (`web/src/features/doc/connection.ts`): a
 * `HocuspocusProvider` over a `Y.Doc` of his own, on `/ws/doc/<artifactId>`, carrying the
 * `schema_version` the server itself answers (`getBlockSchema`) as that parameter and as its
 * token, since a client presenting any other version is admitted read-only (`schemaReadOnly`,
 * `docs/socket_admission.go`) and a writer who cannot write would leave a row reading a window
 * no word ever reached. The room takes a signed-in person's cookie and no bearer at all
 * (INFRA-264), so he carries bob's, in the header Node's own `WebSocket` takes where the
 * browser's takes sub-protocols - which is the whole of the polyfill below. He is ready once
 * the room has authenticated him and his copy has synced, which is the admission
 * `connection.ts` itself waits for, so he holds the document before her page loads.
 *
 * **What a refused admission leaves.** Nothing. A room that admits him read-only - a closed
 * issue, a schema version that is not the server's - or that refuses him outright is the one
 * path on which he never returns a `close` for his caller to call, so the three things he is
 * made of are destroyed here before the refusal is rethrown. Left alive, his provider would go
 * on reconnecting to that room beside every row that follows, on a socket and a `Y.Doc` nobody
 * holds a name for.
 *
 * **How he writes a word.** One insert at the end of the document's first block, where the rows
 * used to put his caret (`placeCaret(bobPage, "after", title)`): one Yjs transaction, so one
 * update, so one frame on her socket carrying that word's bytes and nothing else's.
 *
 * **How a row knows when each word was sent.** The frame is stamped as it goes out, in the
 * `send` his socket makes, which is where his page stamped it too. This is the half of a
 * socket message's wait the reader's own page cannot hold: the frame reaches the server and
 * comes back down her socket in a millisecond or two on one machine, so the stamp is her
 * frame arriving to within that, and the wait read against it carries the relay with it -
 * the direction that cannot flatter her page.
 *
 * **How long he is given to be admitted.** `admissionTimeoutMs`. An admission that neither
 * comes nor is refused - a room that never authenticates him, a copy that never syncs - fails
 * there by name, through the same path a refusal takes, so his provider, its socket and his
 * copy are dropped then and not held until the row's own timeout with no `close` in anyone's
 * hand.
 *
 * `theirWords` is the pattern his words are stamped by, for the one row that reads when each
 * left (`sent`); a row that only writes passes none and stamps nothing. `made` is handed what
 * he is made of as he is made, for the probe row that reads what a refusal leaves.
 */
export async function theOtherPersonsPen(
  artifactId: string,
  { made, theirWords }: { made?: (parts: ThePenIsMadeOf) => void; theirWords?: string } = {}
): Promise<TheOtherPersonsPen> {
  const cookie = await sessionCookie("bob");
  const { version } = await getBlockSchema();
  const sent: Record<string, number> = {};
  const theirs = theirWords === undefined ? undefined : new RegExp(theirWords, "g");
  const read = new TextDecoder();
  const his = new Y.Doc();
  const socket = new HocuspocusProviderWebsocket({
    WebSocketPolyfill: class extends WebSocket {
      constructor(url: string | URL) {
        // Node's WebSocket takes its options where the browser's takes sub-protocols, and the
        // document's room admits a signed-in person's cookie and nothing else.
        super(url, {
          headers: { Cookie: `${cookie.name}=${cookie.value}` },
        } as unknown as string[]);
      }

      override send(data: Parameters<WebSocket["send"]>[0]): void {
        if (theirs !== undefined && (data instanceof ArrayBuffer || ArrayBuffer.isView(data))) {
          const sentAt = Date.now();
          for (const [word] of read.decode(data).matchAll(theirs)) {
            sent[word] ??= sentAt;
          }
        }
        super.send(data);
      }
    },
    connect: false,
    parameters: { schema_version: version },
    url: `${baseUrl.replace(/^http/, "ws")}/ws/doc/${encodeURIComponent(artifactId)}`,
  });
  const admitted = Promise.withResolvers<void>();
  let provider: HocuspocusProvider;
  // Authenticated and synced, whichever comes last, which is the admission the page's own
  // connection announces a document on: the room queues its SyncStep2 before it reads the
  // socket's token, so a writer that has both holds the document the reader is about to build.
  const admit = () => {
    if (provider.isAuthenticated && provider.isSynced) {
      admitted.resolve();
    }
  };
  provider = new HocuspocusProvider({
    document: his,
    name: artifactId,
    onAuthenticated: () => {
      // The scope the room admitted him on, as the page reads it (`isSchemaReadOnly`): a
      // read-only admission is a closed issue or a schema version that is not the server's,
      // and either leaves a window no word of his could reach.
      if (provider.authorizedScope === "readonly") {
        admitted.reject(
          new Error(
            `the document's room admitted this writer read-only at schema version ${version}`
          )
        );
        return;
      }
      admit();
    },
    onAuthenticationFailed: ({ reason }) => {
      admitted.reject(new Error(`the document's room refused this writer: ${reason}`));
    },
    onSynced: admit,
    // His socket is his own, as each document's is the page's: destroying him closes it rather
    // than leaving its connection checker and its reconnect behind.
    preserveConnection: false,
    token: String(version),
    websocketProvider: socket,
  });
  made?.({ doc: his, provider, socket });
  const dropHim = () => {
    provider.destroy();
    socket.destroy();
    his.destroy();
  };
  void socket.connect();
  try {
    await theMomentThePageReached(
      admitted.promise,
      admissionTimeoutMs,
      "the document's room neither admitted this writer nor refused him: it never " +
        "authenticated him, or his copy never synced"
    );
  } catch (refused) {
    // He is the caller's to close only once he is admitted, so this is the one path where
    // nobody else can: a provider left here reconnects to that room for the rest of the worker's
    // life, holding its socket and his copy of the document beside every row that follows.
    dropHim();
    throw refused;
  }
  return {
    close: dropHim,
    sent,
    write: (word: string) => {
      const block = his.getXmlFragment(FRAGMENT_NAME).get(0);
      const text = block instanceof Y.XmlElement ? block.get(0) : undefined;
      if (!(text instanceof Y.XmlText)) {
        throw new Error("the document's first block is not text for the other person to write");
      }
      text.insert(text.length, ` ${word}`);
    },
  };
}

// The probe the wall-clock rows read a build through, and the selectors it carries. It is here
// rather than in one spec file because three files read it: the timer row and the decision row
// of `large-documents-build-serves.e2e.ts`, which `iphone-alone` runs, the desktop guard of
// `large-documents-desktop-build.e2e.ts`, which `chromium-alone` runs, and the edit row of
// `large-documents-long-build.e2e.ts`, which takes the two selectors alone. What the probe
// writes is the three window readings declared above - `__servedTimers`, `__theBuildRan` and
// `__theirWordsServed` - so the writer stands beside the declarations of what it writes.

/** The editor a reader already has while the rest of its blocks are built, as a selector an
 *  init script can carry (`editorBeingBuilt` is the same root as a locator). */
export const liveEditorRoot = '.proof-editor[data-write-path="live"]';

/** The finished editor's own name, which the host writes once the binding has answered
 *  its handle - a task after the binding connected - so an editor wearing it is one whose
 *  build is over (`web/src/features/doc/editor.ts`). */
export const namedEditor = '[role="textbox"][aria-label="Document editor"]';

/** How often the probe below hands the browser a timer. Often enough that a build of a few
 *  seconds is covered by a dozen of them, rarely enough that the probe is not itself the thing
 *  keeping the page busy. */
export const timerEveryMs = 250;

/** How large a frame the reader's page reads for the words another person writes into the
 *  document (`theirWords` below). The document's own state frame is hundreds of kilobytes and
 *  carries none of them; a frame of one write is a few hundred bytes. */
export const theirWordsInFramesUnderBytes = 16_384;

/** What the probe below answers the row that arranges its own window: her page's own word
 *  about how far its build has got, said at each step of it, which is what that row writes
 *  and stops by. */
export interface TheBuildServed {
  /** The editor's own top-level block count as her page last said it, and `undefined` before
   *  it has said anything: what the row reads to know how many of his words her build has
   *  asked for so far. */
  readonly built: () => number | undefined;
  /** Her next word about it, which is how the writer waits: for a step of her build rather
   *  than for a span of anyone's clock. Settles with the count she says; fails, naming what
   *  never came, where she says nothing more inside `timeoutMs`. */
  readonly nextStep: (timeoutMs: number) => Promise<number>;
  /** The count she said when her build reached the blocks the caller named - that many or
   *  more - or the count her editor was named holding, where the build never reached them,
   *  which the row fails on rather than waiting for a word that will never come. Settles
   *  once, and only where the caller named a stop; fails, naming what never came, where
   *  neither has happened inside `timeoutMs`. */
  readonly reachedTheStop: (timeoutMs: number) => Promise<number>;
  /** That same count as soon as she has said it, and `undefined` until then: the writing loop
   *  stops between two of its own keystrokes rather than waiting on a promise, so the word it
   *  is in the middle of is the last one it sends. */
  readonly stopped: () => number | undefined;
}

/**
 * Records what the page's own posted tasks cost it while the editor is being built: every
 * timer the page hands the browser, the build's own window, and - where `theirWords` names a
 * pattern - the turn of its own loop the page served the socket frame carrying each word
 * another person wrote in, so the row can read whether the build was still what served it.
 *
 * **A turn is the page's own number for the task it is in.** The page keeps a counter and
 * hands it out lazily: the first thing recorded in a task takes `++turns` and queues a
 * microtask that drops it again, so everything recorded in that task is given the same
 * number and the next task takes a fresh one. That is exact rather than close, because the
 * checkpoint that drops the number is the one the browser runs when the stack empties at the
 * end of a callback, which no task spans: two records share a number only if one task served
 * them both, and a larger number means a later task, whatever the machine's speed. Nothing
 * the page already publishes can say that - `performance.now()` and the `longtask` entries
 * are a clock, an animation frame is several tasks wide, and a `MessageChannel` message or a
 * posted task has no identity to carry - so the counter is what the swap's own write and each
 * served word are stamped with, and the row's liveness reading is those two numbers and
 * nothing else.
 *
 * A timer is handed over from an animation frame, because a frame is the one thing that does
 * run through a starved build and so the one place a probe can be handed over from.
 *
 * A word is read out of the frame's own bytes, which is what pairs it with the moment the
 * other person sent it (`theOtherPersonsPen`): the two wall-clock stamps are the one
 * machine's, so the difference is what the reader's thread made that edit wait, which the row
 * prints and holds to nothing. Nothing of the frame's arrival is read here, because no clock
 * inside the page holds it - a `MessageEvent` is stamped in the task that dispatches it, and
 * the browser's own `Network.webSocketFrameReceived` is stamped in this renderer as Blink
 * takes the frame off the pipe, which a thread the build owns delays as surely as it delays
 * the event. Both read zero behind a page held for two seconds, measured
 * (dispatch://INFRA-341).
 *
 * A frame larger than `theirWordsInFramesUnderBytes` is left unread: the document's own state
 * frame is hundreds of kilobytes and carries no word of theirs, and decoding it would be this
 * probe holding the thread it is measuring. Only the first document socket is recorded. The
 * socket is wrapped as a `construct` proxy rather than a subclass for the reason
 * `large-documents-page.ts` gives: the page's transport subclasses `globalThis.WebSocket` too.
 *
 * Where `stepBlocks` names a count, the page says how far its editor's build has got each
 * time its own top-level block count crosses the next multiple of it, in the task it read
 * that count in; and where `stopAfterBlocks` names one, it says so again the moment the count
 * reaches that, and with the count it ended on where the editor was named without ever
 * reaching it. Those are the announcements the row writes the other person's words by and
 * stops his writing by, and they are announcements rather than readings for the reason
 * `watchTheDocument` gives: a run that asked this page how far its build had got would be
 * answered when the build was over.
 *
 * Install before the navigation; read once the editor has opened.
 */
export async function whatTheBuildServes(
  page: Page,
  {
    stepBlocks = 0,
    stopAfterBlocks = 0,
    theirWords,
  }: { stepBlocks?: number; stopAfterBlocks?: number; theirWords?: string } = {}
): Promise<TheBuildServed> {
  const reached = Promise.withResolvers<number>();
  let built: number | undefined;
  let stopped: number | undefined;
  let step = Promise.withResolvers<number>();
  await page.exposeFunction("__theBuildReached", (said: number, itsStop: boolean) => {
    built = said;
    if (itsStop) {
      stopped ??= said;
      reached.resolve(said);
    }
    step.resolve(said);
    step = Promise.withResolvers<number>();
  });
  await page.addInitScript(
    ({
      every,
      live,
      named,
      perStep,
      stop,
      under,
      words,
    }: {
      every: number;
      live: string;
      named: string;
      perStep: number;
      stop: number;
      under: number;
      words: string | undefined;
    }) => {
      const timers: ServedLate[] = [];
      const servedWords: Record<string, ServedWord> = {};
      const ran: TheBuildRan = {};
      window.__servedTimers = timers;
      window.__theirWordsServed = servedWords;
      window.__theBuildRan = ran;
      /** The turn of the page's own loop this probe is recording in, handed out lazily: the
       *  first record of a task takes the next number and queues the microtask that drops it,
       *  which the browser runs when the stack empties at the end of that task, so every
       *  record of one task carries one number and the next task's records carry a larger
       *  one. This is the whole of the row's liveness reading and it holds no clock. */
      let turns = 0;
      let held: number | undefined;
      const thisTurn = (): number => {
        if (held === undefined) {
          held = ++turns;
          queueMicrotask(() => {
            held = undefined;
          });
        }
        return held;
      };
      let recording = false;
      const theirs = words === undefined ? undefined : new RegExp(words, "g");
      const read = new TextDecoder();
      window.WebSocket = new Proxy(window.WebSocket, {
        construct(made, args: [string | URL, (string | string[])?], asked) {
          const socket = Reflect.construct(made, args, asked) as WebSocket;
          if (recording || theirs === undefined || !String(args[0]).includes("/ws/doc/")) {
            return socket;
          }
          recording = true;
          socket.addEventListener("message", (event) => {
            const servedAt = Date.now();
            const data: unknown = event.data;
            if (!(data instanceof ArrayBuffer) || data.byteLength > under) return;
            const servedIn = thisTurn();
            for (const [word] of read.decode(data).matchAll(theirs)) {
              servedWords[word] ??= { atMs: servedAt, turn: servedIn };
            }
          });
          return socket;
        },
      });
      let handedLast = Number.NEGATIVE_INFINITY;
      /** Whether a timer the probe handed over has still to run. One at a time, because what
       *  this row is about is how long the page waits for a timer, not how deep a queue of
       *  them grows: a frame of this page costs hundreds of milliseconds, so a probe handing
       *  one over every frame outruns the browser and measures its own backlog. A reader's
       *  page posts one and waits for it - React's scheduler, and the one-card-per-task timer
       *  of `useAskCardsDrawnInTasks` - which is what this does too. */
      let owed = false;
      /** The next count of its own blocks the page has still to say it has reached, and
       *  whether it has said the stop. The page says one thing per step rather than one per
       *  frame: what the row writes by is how far this build has got, and a word per step is
       *  what makes the number of them the build's own rather than the machine's. */
      let nextStep = perStep;
      let saidTheStop = false;
      const look = () => {
        const now = performance.now();
        const root = document.querySelector(live);
        if (ran.firstBatchMs === undefined && root !== null) {
          ran.firstBatchMs = now;
        }
        // How far the build has got, read as the top-level child count of the editable host
        // rather than a query over the whole document, so the probe costs the page nothing:
        // the count is a property of a node the browser already holds, and reading it forces
        // no layout. Read once a frame, because the stop below is read from it as well as the
        // timer the probe hands over.
        const built = root?.querySelector('[contenteditable="true"]')?.childElementCount ?? 0;
        if (!owed && now - handedLast >= every) {
          const handedAt = now;
          handedLast = now;
          owed = true;
          setTimeout(() => {
            owed = false;
            timers.push({ builtThen: built, handedAt, waitedMs: performance.now() - handedAt });
          }, 0);
        }
        // How far this build has got, said in the task this read the count in so the run is
        // told rather than asking: once per step of its own blocks, and once more when it
        // reaches the blocks the row stops the other person's writing at.
        if (perStep > 0 && built >= nextStep) {
          while (built >= nextStep) nextStep += perStep;
          const itsStop = stop > 0 && built >= stop;
          saidTheStop ||= itsStop;
          window.__theBuildReached?.(built, itsStop);
        } else if (!saidTheStop && stop > 0 && built >= stop) {
          saidTheStop = true;
          window.__theBuildReached?.(built, true);
        }
        if (document.querySelector(named) !== null) {
          ran.openedMs = now;
          // And the swap that ends the build runs in the tasks after this frame: the binding
          // adopting the document, the editor being revealed, and the margin placing its cards
          // over it for the first time. The page says when that is over rather than leaving it
          // to be inferred from a run of `longtask` entries: a task handed over now runs after
          // the work already queued, so the task it is served in is the page's own word that
          // it is free again - and that task's own turn is the last one the build can still be
          // what served something in. The page stamps the turn there, which is the number the
          // row reads, and the clock beside it, which the row only prints.
          //
          // The end of that turn is stamped too, from a task handed over inside it, and it is
          // printed and nothing more: it is served once the page has got through everything it
          // was already holding, which on this pod is 1.5 to 21 s of the backlog a build
          // leaves - a measurement of what the page carries out of a build, and no part of any
          // reading. The reading is `swapTurn` against the turn each word was served in.
          requestAnimationFrame(() => {
            setTimeout(() => {
              ran.swapTurn = thisTurn();
              ran.swappedMs = performance.now();
              setTimeout(() => {
                ran.freeTurnEndedMs = performance.now();
              }, 0);
            }, 0);
          });
          // And where the editor was named without the build ever reaching that count, the
          // page says so with the count it ended on: the run's wait ends in the row's own
          // failure, which names what the build did, rather than in a timeout.
          if (!saidTheStop && stop > 0) {
            saidTheStop = true;
            window.__theBuildReached?.(built, true);
          }
          return;
        }
        requestAnimationFrame(look);
      };
      requestAnimationFrame(look);
    },
    {
      every: timerEveryMs,
      live: liveEditorRoot,
      named: namedEditor,
      perStep: stepBlocks,
      stop: stopAfterBlocks,
      under: theirWordsInFramesUnderBytes,
      words: theirWords,
    }
  );
  return {
    built: () => built,
    nextStep: (timeoutMs) =>
      theMomentThePageReached(
        step.promise,
        timeoutMs,
        "her build said nothing more about how far it had got: no further step of it came"
      ),
    reachedTheStop: (timeoutMs) =>
      theMomentThePageReached(
        reached.promise,
        timeoutMs,
        "her build never reached the blocks the row stops him at, and her editor was never " +
          "named holding fewer"
      ),
    stopped: () => stopped,
  };
}

/** The tasks of `served` the browser was given while the build ran, which are the ones the
 *  build could have starved. A task handed over inside the window and run after it is one of
 *  them, which is why they are read by when each was handed over. */
export function handedToTheBuild(served: readonly ServedLate[], ran: TheBuildRan): ServedLate[] {
  const from = ran.firstBatchMs ?? Number.POSITIVE_INFINITY;
  const to = ran.openedMs ?? Number.POSITIVE_INFINITY;
  return served.filter((task) => task.handedAt >= from && task.handedAt <= to);
}

/** How far the build had got at the first and at the last of the tasks the page was handed
 *  inside a window: the fewest and the most top-level blocks the editor's own host held at any
 *  of them. `[0, 0]` where it was handed none. */
export function builtAcross(served: readonly ServedLate[]): [number, number] {
  const built = served.map((task) => task.builtThen ?? 0);
  return built.length === 0 ? [0, 0] : [Math.min(...built), Math.max(...built)];
}

/**
 * Fails unless the timers the page was handed inside a window show the document being built
 * across it: two of them at least, and the editor holding more top-level blocks at the last of
 * them than at the first, which is the build's own batches arriving while they were served.
 *
 * This is the one proof this suite has that a window a row measures held a build, and the
 * three rows that read such a window use it: the timer row of
 * `large-documents-build-serves.e2e.ts`, its decision row's own window and the desktop guard
 * of `large-documents-desktop-build.e2e.ts`. It is a reading of the document rather than of
 * the runner:
 * the same batches carry the same blocks into the editor whatever the machine's speed, so the
 * rise is there on a loaded runner and on an idle one. A window the document was not built in -
 * one that ended before the first batch, or opened after the editor did - holds either no timer
 * at all or timers that all read the same count, and fails here.
 */
export function theWindowHeldABuild(timers: readonly ServedLate[]): void {
  const [least, most] = builtAcross(timers);
  expect(
    timers.length,
    "the page served fewer than two of its own timers inside the window, so nothing here " +
      "shows it held a build at all"
  ).toBeGreaterThanOrEqual(2);
  expect(
    most - least,
    `the editor held ${least} top-level blocks when the page served its first timer of the ` +
      `window and ${most} when it served its last: the window did not cover the document ` +
      "being built"
  ).toBeGreaterThan(0);
}
