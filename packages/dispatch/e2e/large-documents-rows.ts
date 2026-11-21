// What the rows of the eleven `large-documents-*.e2e.ts` files share: the seed every one of them
// writes, the readings the page leaves on `window` for them, the three surfaces a block of the
// document can stand on, the other person who writes into the document while a reader's
// editor is built (`theOtherPersonsPen`), the bounds a wait for the page's draw is held to
// (`aDrawWithinMs`, `aLongDrawWithinMs`), and the two selectors an init script finds the
// editor being built and the finished editor by (`liveEditorRoot`, `namedEditor`). A helper
// here is one more than one of those files needs; a helper one file needs stays in that file.
//
// They were one file until the `iphone` half of it cost a browser shard 610 s of a step that is
// given 540 (dispatch://INFRA-341, run 42794865122's `shard (4)`), and Playwright hands a
// file's rows to one shard, so a file larger than the budget fits in none of them. Four of
// the eleven are read from the wall clock, and each of those runs in one project and no other,
// because a wall-clock project selects by file (`clockSpecNames`, e2e/configurations.ts):
// `large-documents-measured.e2e.ts` (`iphone-measure-alone`: the two measurements R11's
// targets are read from), `large-documents-build-serves.e2e.ts` and
// `large-documents-build-serves.the-decision.e2e.ts` (`iphone-alone`: what a build serves the
// rest of the page while it runs, the page's own clock and a decision answered inside it) and
// `large-documents-desktop-build.e2e.ts` (`chromium-alone`: the longest main-thread task of a
// build at a desktop width). The seven the parallel projects run are
// `large-documents-long-build.e2e.ts` (what a build of the larger document costs the page, with
// no number read from the clock; its phone half is `iphone-long-build-alone`'s, a leg of its
// own), `large-documents-build.e2e.ts` (what stands on the reader's screen while the editor is
// built), `large-documents-reader.e2e.ts` (what a reader does with a document whose editor is
// still being built, and the rest of acceptance A7),
// `large-documents-first-screen.e2e.ts` (the draw the server sends, and the reads the page holds
// until the editor's first batch), `large-documents-first-screen.the-chrome.e2e.ts` (the chrome
// the page holds until that batch), `large-documents-first-screen.the-keyboard.e2e.ts` (a reader
// with no pointer reaching for that chrome) and `large-documents-fold.e2e.ts` (where that chrome
// leaves the server's first block).
//
// This list is the one place the family's files are named with what each holds. The other
// files point here rather than copy it, so a file added or split is written in here and in no
// other header.
//
// The documents themselves are `large-documents.ts`, what a page announces about its own draw
// and its build is `large-documents-page.ts`, the steps a page takes on its way to an open
// editor, which `large-documents-long-build.e2e.ts` waits on (`watchThePageMove`,
// `whileThePageMoves`), are `large-documents-page-moves.ts`, which stood at the end of
// `large-documents-page.ts` until that file passed 1,000 lines, and the probe two of the
// wall-clock rows read a build through (`whatTheBuildServes`) is
// `large-documents-build-probe.ts`, which stood at the end of this file until the two together
// passed 1,000 lines.
import { HocuspocusProvider, HocuspocusProviderWebsocket } from "@hocuspocus/provider";
import { expect, type Page } from "@playwright/test";
import * as Y from "yjs";

import { FRAGMENT_NAME } from "../web/src/features/doc/roots";
import { compiledBlockSchema, compiledSchemaCreatable } from "../web/src/features/doc/schema";
import {
  baseUrl,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifactBlocks,
  sessionCookie,
} from "./api";
import { theMomentThePageReached } from "./large-documents-page";
import { sql } from "./psql";
import { resetDatabase } from "./seed";

// What a row of the shards waits for the page to draw is bounded, so a page that never draws it
// fails the row by name inside the leg it runs in, rather than at that leg's step budget, which
// names no row and keeps no report. The two bounds below are shared: `aDrawWithinMs` is the
// bound six files share - `large-documents-first-screen.e2e.ts` and its two siblings
// `large-documents-first-screen.the-chrome.e2e.ts` and
// `large-documents-first-screen.the-keyboard.e2e.ts`, `large-documents-reader.e2e.ts`,
// `large-documents-build.e2e.ts` and `large-documents-fold.e2e.ts` - and `aLongDrawWithinMs`
// the one `large-documents-first-screen.e2e.ts` and its chrome sibling share. A bound only one
// file's waits need is that file's, beside what those waits cost.
//
// The leg. These rows run in the shards, `chromium` and `iphone` four workers wide, under the
// browser step's 540 s with one retry (.github/workflows/dispatch-browser.yaml). What a leg
// leaves of those 540 s moves with every merge, so it is read from the step times that job
// prints rather than written here. A row that hangs runs twice, so it adds twice its failing
// time, less what it costs when it passes, to the worker it runs on. And the fault the bounds
// are for - an editor that never opens - fails the rows of all six files at once, in whichever
// leg holds them: what a bound has to fit is the sum of that over every row of these files one
// leg holds, shared over its four workers, and not one file's rows. That sum is not measured:
// no file of this family gives it, so this is a rule the bounds are written to and not a figure
// shown to fit the step. Four of the six files write what their own rows add when they fail,
// beside their own bounds - `large-documents-build.e2e.ts`, `large-documents-reader.e2e.ts`,
// `large-documents-first-screen.e2e.ts` and `large-documents-fold.e2e.ts` - and the chrome and
// keyboard files write nothing of it; each of those figures is one file's, not the sum.
//
// What the waits cost, timed on this pod, one worker on two CPUs (dispatch://INFRA-341), the
// slower of `chromium` and `iphone`. Bounded by `aDrawWithinMs`, the slowest of each file:
// the reader's drawn card 6.72 s (`large-documents-reader.e2e.ts`); 2.67 s across the three
// first-screen files, whose waits - the draw's first block, the editor's first block, the held
// boxes, the Subscribers control, the headers, the reads and the parts the page held behind its
// first batch - took 0.03 s at the least; the document's header 2.38 s from the navigation and
// the editor's text 0.02 s after it (`large-documents-fold.e2e.ts`); and the draw's first block
// 2.3 s in the viewport and 0.85 s attached (`large-documents-build.e2e.ts`). Bounded by
// `aLongDrawWithinMs`: the editor open over the larger document 12.98 s from the navigation
// (`large-documents-first-screen.e2e.ts`), and the chrome on a page whose socket never answers
// 12.63 s, which waits out `LONGEST_WAIT_FOR_THE_DOCUMENT_MS`
// (`large-documents-first-screen.the-chrome.e2e.ts`). So 20 s is about three times the slowest
// of the first kind and 40 s three times the slowest of the second.

/** How long a row waits for something the page draws in its first seconds. */
export const aDrawWithinMs = 20_000;

/** How long a row waits for the editor over the larger document, or for the chrome a page whose
 *  socket never answers draws once its own deadline has passed. */
export const aLongDrawWithinMs = 40_000;

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
 * `HocuspocusProvider` over a `Y.Doc` of his own, on `/ws/doc/<artifactId>`, presenting what her
 * page presents, from the same manifest her build compiled in (`compiledBlockSchema` and
 * `compiledSchemaCreatable`, `web/src/features/doc/schema.ts`): `schema_version` (V) and
 * `schema_creatable` (R) as parameters, and V alone as his token, as `connection.ts` sends it. He
 * is another reader of the same build, so he reads no schema from the server, as her page reads
 * none; numbers the live servers do not admit get a read-only admission
 * (`packages/envoy/cmd/dispatch/README.md`, "Two builds at once"), and a writer who cannot write
 * would leave a row reading a window no word ever reached. The room takes a signed-in person's
 * cookie and no bearer at all (INFRA-264), so he carries bob's, in the header Node's own
 * `WebSocket` takes where the browser's takes sub-protocols - which is the whole of the polyfill
 * below. He is ready once the room has authenticated him and his copy has synced, which is the
 * admission `connection.ts` itself waits for, so he holds the document before her page loads.
 *
 * **What a refused admission leaves.** Nothing. A room that admits him read-only - a closed
 * issue, schema numbers the live servers do not admit - or that refuses him outright is the one
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
  const version = compiledBlockSchema.version;
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
    parameters: { schema_creatable: compiledSchemaCreatable, schema_version: version },
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
      // read-only admission is a closed issue or schema numbers the live servers do not admit,
      // and either leaves a window no word of his could reach.
      if (provider.authorizedScope === "readonly") {
        admitted.reject(
          new Error(
            `the document's room admitted this writer read-only at schema version ${version}, creatable ${compiledSchemaCreatable}`
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

// The two selectors an init script finds the editor's build by. They are here rather than in
// one file because three carry them: `large-documents-build-probe.ts`, whose probe the timer row
// of `large-documents-build-serves.e2e.ts` and the desktop guard of
// `large-documents-desktop-build.e2e.ts` read a build through, and the decision row of
// `large-documents-build-serves.the-decision.e2e.ts` and the edit row of
// `large-documents-long-build.e2e.ts`, which take the two selectors alone.

/** The editor a reader already has while the rest of its blocks are built, as a selector an
 *  init script can carry (`editorBeingBuilt` is the same root as a locator). */
export const liveEditorRoot = '.proof-editor[data-write-path="live"]';

/** The finished editor's own name, which the host writes once the binding has answered
 *  its handle - a task after the binding connected - so an editor wearing it is one whose
 *  build is over (`web/src/features/doc/editor.ts`). */
export const namedEditor = '[role="textbox"][aria-label="Document editor"]';
