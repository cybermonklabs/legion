// What the rows of the four `large-documents-*.e2e.ts` files share: the seed every one of them
// writes, the readings the page leaves on `window` for them, and the three surfaces a block of
// the document can stand on. A helper here is one more than one of those files needs; a helper
// one file needs stays in that file.
//
// They were one file until the `iphone` half of it cost a browser shard 610 s of a step that is
// given 540 (dispatch://INFRA-341, run 42794865122's `shard (4)`), and Playwright hands a
// file's rows to one shard, so a file larger than the budget fits in none of them. The four are
// `large-documents-measured.e2e.ts` (the numbers R11's targets are read from, and the build
// they are read over), `large-documents-build.e2e.ts` (what stands on the reader's screen while
// the editor is built), `large-documents-reader.e2e.ts` (what a reader does with a document
// whose editor is still being built, and the rest of acceptance A7) and
// `large-documents-first-screen.e2e.ts` (the draw the server sends, and the reads and the
// chrome the page holds until the editor's first batch).
//
// The documents themselves are `large-documents.ts`, and what a page announces about its own
// draw and its build is `large-documents-page.ts`.
import { expect, type Page } from "@playwright/test";

import { createIssue, createIssueArtifact, createProject, getArtifactBlocks } from "./api";
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
 *  given, on the page's own clock, and how long it then waited before it ran. The two rows of
 *  `large-documents-measured.e2e.ts` that read a build's own window read these - one per timer
 *  `whatTheBuildServes` hands over, and one per word another person wrote, which that row
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

/** The build's own window on the page's clock, as `whatTheBuildServes` sees it: the frame its
 *  first batch was found in, the frame the finished editor was named in, and the moment the
 *  page was free again after the swap that ends a build - the adoption, the reveal and the
 *  margin's first placement - which the page says by serving a task it was handed in that
 *  frame. A reader's build is over at the third of these, not the second. */
export interface TheBuildRan {
  firstBatchMs?: number;
  openedMs?: number;
  swappedMs?: number;
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
    /** What the row that answers a decision inside a build recorded: the press the page made
     *  in a frame of the build, and `performance.now()` when the request carrying the answer
     *  left the page. */
    __answerPressed?: TheAnswerPressed;
    __answerSentMs?: number;
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
     *  waited to be run, `Date.now()` as the page served the socket frame carrying each word
     *  another person wrote, and the build's own window, which says which of them the build
     *  was given. `__theirWordsSent` is the same stamp in the page that wrote them, taken as
     *  the frame left it (`whenTheirWordsAreSent`). */
    __servedTimers?: ServedLate[];
    __theirWordsServed?: Record<string, number>;
    __theirWordsSent?: Record<string, number>;
    __theBuildRan?: TheBuildRan;
    /** Where the editor was when another person's word reached its first block
     *  (`whenTheirWordLands`). */
    __theirWord?: TheirWordLanded;
  }
}

export const agent = {
  actor: { id: "e2e-large-documents-agent", kind: "session" as const },
  as: "agent" as const,
};

/**
 * Resolves once the server owes nothing this seed left it: no document settlement queued or
 * running, and no document owed a block index. A document of this size defers its block index
 * (`deferBlockIndexBlocks`, `docs/reference_excerpts.go`), and that work costs the same machine
 * the browser runs on, so a measurement taken while it runs measures the reconciliation too.
 * The wait runs against the one database this run holds (`sql`), and its callers are this
 * file's own `seedDocument`, `large-documents-measured.e2e.ts` and
 * `large-documents-reader.e2e.ts`.
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
        timeout: 180_000,
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
