import { type Browser, expect, test } from "@playwright/test";

import { createArtifactComment, editArtifact } from "./api";
import { blockId } from "./editor";
import { inboxList, inboxPane, selectedRow, selectRow } from "./inbox-pane";
import {
  ANCHORED_LINE,
  APPROVAL_FOOT,
  addRowAbove,
  blockOffset,
  CHANGED_STEP,
  commitEventElsewhere,
  DOCUMENT_FOOT,
  documentTextRead,
  FAILING_NOTE,
  focusAfterStaleness,
  headingOffset,
  holdThreadReplies,
  INSERTED_HEADING,
  inboxAt,
  LATE_REPLY,
  LATE_REPLY_FIRST_LINE,
  moveColumnBelow,
  moveColumnTo,
  offsetInColumn,
  PLAIN_BLOCK,
  QUESTION,
  QUESTION_BLOCK,
  type QuestionRun,
  RECOVERY_NOTE,
  restingColumn,
  SECOND_BLOCK,
  SECOND_HEADING,
  SECOND_NOTE,
  SECOND_QUESTION,
  SECOND_THREAD_ROOT,
  SECTION_HEADING,
  sectionBarTop,
  seedApprovalPlace,
  seedApprovalWithMarks,
  seedQuestionColumn,
  seedQuestions,
  seedTwoThreads,
  session,
  THREAD_REPLY,
  waitOutPlacement,
  watchLiveEvents,
  watchLiveGap,
  watchWhoAmI,
} from "./inbox-pane-scroll";
import { holdReadMarks } from "./inbox-threads";
import { holdMermaidWorkers } from "./mermaid-helpers";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The Inbox reading column keeps the reader's place (INFRA-2512).
 *
 * Two families of row say the column is the reader's: an approval request whose position stands
 * through every drive that reads the signed-in person again, and a question or thread whose
 * column stays where the reader put it while the document is written, the list grows, the live
 * socket drops and the document's own read fails and recovers. Seven opening rows beside them
 * say where the column opens - six with their target far down a long document, one at the
 * start of it after the reader was left far down the same document - so a column that is
 * never written fails them.
 *
 * Everything seeded here is made up: this repository is public, so nothing names a real person,
 * host or project. The seed and the readings are in `inbox-pane-scroll.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

// ---------------------------------------------------------------------------
// The approval request the reader reads down.
// ---------------------------------------------------------------------------

// SLOW(>=2s): four drives, each a committed event, a socket gap or a refocus the page must read.
test("the approval pane keeps the place the reader scrolled to", async ({ browser }) => {
  const seed = await seedApprovalPlace();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const events = watchLiveEvents(page);
    const reads = watchWhoAmI(page);
    const gap = await watchLiveGap(page);
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.approvalAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
    await expect(pane.getByRole("region", { name: "Document version 1" })).toBeVisible();
    await expect(pane).toContainText(APPROVAL_FOOT);

    // The reader reads down the document. The pane sends an approval request's column to its
    // start once, when the version's text has been read, so this move comes after that write.
    await moveColumnTo(page, 900);
    const place = (await restingColumn(page)).top;
    expect(place).toBeGreaterThan(400);

    // Each drive commits an event first, so the answer any later read of the signed-in person
    // gets carries a later event head and is a new object. Without that the drives below would
    // read the same object again and say nothing.

    // One: the live socket closed, and resumed above the log's head. The page cannot replay
    // what it missed, so it refreshes every read it holds, the signed-in person's among them.
    await commitEventElsewhere(seed.otherIssueKey, "The ledger keeps a clock of its own.");
    const beforeGap = reads();
    await gap.open();
    await expect.poll(() => gap.starts().slice(0, 2)).toEqual([true, false]);
    await expect.poll(reads).toBeGreaterThan(beforeGap);
    expect((await restingColumn(page)).top).toBe(place);

    // Two: an event committed on another issue, which the page's feed carries to it.
    const beforeEvent = events();
    await commitEventElsewhere(seed.otherIssueKey, "The mirror gives the lease back at 02:00.");
    await expect.poll(events).toBeGreaterThan(beforeEvent);
    expect((await restingColumn(page)).top).toBe(place);

    // Three: a row added above the open one, which redraws the list beside the column.
    const added = await addRowAbove();
    await expect(inboxList(page).locator("[data-inbox-row]").first()).toHaveAttribute(
      "data-inbox-row",
      added
    );
    expect((await restingColumn(page)).top).toBe(place);

    // Four: the window taken out of focus and given it again past the staleness, which reads
    // the signed-in person again (`staleTime` 30 s, `refetchOnWindowFocus` on).
    await commitEventElsewhere(seed.otherIssueKey, "The replay budget is three attempts.");
    const beforeFocus = reads();
    await focusAfterStaleness(page);
    await expect.poll(reads).toBeGreaterThan(beforeFocus);
    expect((await restingColumn(page)).top).toBe(place);

    // The document is still drawn, and it is still the version the column opened.
    await expect(pane.getByRole("region", { name: "Document version 1" })).toBeVisible();
    await expect(pane).toContainText(APPROVAL_FOOT);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// The column the reader moved, in its three shapes.
// ---------------------------------------------------------------------------

/**
 * One run of the question rows: the reader opens the item, waits the placement's bound out,
 * moves the column below the opened block, and reads the block standing at the top of it after
 * each drive. The block is what the row reads, and not the column's number, because text drawn
 * above the reader moves that number by the browser's own scroll anchoring.
 */
async function keepsTheColumn(browser: Browser, run: QuestionRun): Promise<void> {
  const seed = await seedQuestionColumn(run);
  const { threadId } = seed;
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    // The read marks the thread run would otherwise write are held, so the thread goes on
    // saying what the reader has not read (`inbox-threads.ts`).
    await holdReadMarks(page);
    const events = watchLiveEvents(page);
    const reads = watchWhoAmI(page);
    const gap = await watchLiveGap(page);
    const text = documentTextRead(page, seed.artifactId);
    // The thread run serves the thread's replies only once the reader has moved the column.
    const replies = run === "thread" ? await holdThreadReplies(page, seed.artifactId) : undefined;
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.item));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.item);
    await expect(pane.getByText(DOCUMENT_FOOT)).toHaveCount(1);
    await waitOutPlacement(page);

    // The reader moves the column by writing the scroller's position, which is how a test
    // drives a scroll the pane's four reader actions do not carry. They stop below the opened
    // block, so every write a drive makes above them is one the browser's own scroll anchoring
    // has to absorb.
    await moveColumnBelow(page, seed.blockId, 80);
    let standing = (await restingColumn(page)).block;
    expect(standing).not.toBeNull();
    expect(standing).not.toBe(seed.blockId);

    if (replies !== undefined) {
      // The thread is drawn under its block only now, after the reader moved the column. The
      // card it draws there is text above the reader, which moves what stands under them; what
      // this row decides is that the pane wrote the column nothing, which the thread's own
      // first unread message says by standing nowhere near the top of the column. A thread
      // that opened at it, as it did before this change, would have put it exactly there.
      await replies.release();
      await expect(pane.getByText(THREAD_REPLY)).toHaveCount(1);
      const opened = await offsetInColumn(page, "[data-inbox-thread-first-unread]");
      expect(Math.abs(opened)).toBeGreaterThan(40);
      standing = (await restingColumn(page)).block;
      expect(standing).not.toBeNull();
    }

    // One: another session writes the document, above the block the column was opened at.
    const barBefore = run === "heading" ? await sectionBarTop(page) : 0;
    await editArtifact(
      seed.artifactId,
      {
        ops: [{ before: seed.insertedBefore, markdown: seed.insertedMarkdown, op: "insert" }],
        summary: "A block above the opened one",
      },
      session
    );
    await expect(pane.locator("[data-block-id]", { hasText: seed.insertedText })).toHaveCount(1);
    expect((await restingColumn(page)).block).toBe(standing);

    if (run === "heading") {
      // The bar is drawn for the life of the column, bound or none: the heading the edit put
      // between the section and the question moves where the bar starts.
      await expect.poll(() => sectionBarTop(page)).toBeGreaterThan(barBefore);
    }
    if (run === "plain") {
      // A question before any heading has no section, so the pane draws no bar at all.
      await expect(pane.getByTestId("pane-section-bar")).toHaveCount(0);
    }
    if (run === "thread") {
      // The section's own block is read for the life of the column too: Open page now names
      // the heading the edit put immediately above the anchored block.
      const heading = await blockId(seed.artifactId, `### ${INSERTED_HEADING}`);
      await expect(pane.getByRole("link", { name: "Open page" })).toHaveAttribute(
        "href",
        new RegExp(`#b-${heading}$`)
      );
    }

    // Two: a row added above the open one in the Inbox list.
    const added = await addRowAbove();
    await expect(inboxList(page).locator("[data-inbox-row]").first()).toHaveAttribute(
      "data-inbox-row",
      added
    );
    expect((await restingColumn(page)).block).toBe(standing);

    // Three: an event committed on another issue.
    const beforeEvent = events();
    await commitEventElsewhere(seed.otherIssueKey, "The ledger keeps a clock of its own.");
    await expect.poll(events).toBeGreaterThan(beforeEvent);
    expect((await restingColumn(page)).block).toBe(standing);

    // Four: the live socket closed, and resumed above the log's head, so the page refreshes
    // every read it holds.
    const beforeGap = reads();
    await gap.open();
    await expect.poll(() => gap.starts().slice(0, 2)).toEqual([true, false]);
    await expect.poll(reads).toBeGreaterThan(beforeGap);
    expect((await restingColumn(page)).block).toBe(standing);

    // Five: the window taken out of focus and given it again past the staleness.
    await commitEventElsewhere(seed.otherIssueKey, "The replay budget is three attempts.");
    const beforeFocus = reads();
    await focusAfterStaleness(page);
    await expect.poll(reads).toBeGreaterThan(beforeFocus);
    expect((await restingColumn(page)).block).toBe(standing);

    // Six, which is read differently: the read of the document fails with a status the page
    // does not retry (`isRetryableQueryError` refuses 409 DOCUMENT_UNLOADABLE) and then
    // succeeds again. The pane draws its card while that read fails, so neither the block nor
    // the position survives this drive.
    await text.refuse();
    const beforeFailing = events();
    await editArtifact(
      seed.artifactId,
      { ops: [{ after: "end", markdown: `${FAILING_NOTE}\n`, op: "insert" }], summary: "A note" },
      session
    );
    await expect.poll(events).toBeGreaterThan(beforeFailing);
    await expect(pane.getByText(DOCUMENT_FOOT)).toHaveCount(0);
    if (run === "thread") {
      await expect(pane.locator("[data-inbox-pane-fallback]")).toBeVisible();
    } else {
      await expect(pane.getByTestId(`ask-${seed.item}`)).toBeVisible();
    }

    // The route served again re-reads nothing on its own: the page holds its reads for 30
    // seconds. Another session's edit is what asks for the text again, since its
    // `artifact.version` event invalidates `["artifact", id]` and the text stands under that
    // key's own prefix.
    await text.serve();
    await editArtifact(
      seed.artifactId,
      {
        ops: [{ after: "end", markdown: `${RECOVERY_NOTE}\n`, op: "insert" }],
        summary: "A note that reads again",
      },
      session
    );
    await expect(pane.getByText(DOCUMENT_FOOT)).toHaveCount(1);
    await expect(pane.locator("[data-block-id]", { hasText: RECOVERY_NOTE })).toHaveCount(1);

    // What the row reads after the recovery: the reader can scroll again, another session can
    // write the document, and the column does not move. A second placement, armed by the load
    // state flipping back, would send it to the opened block's section or to the start.
    await moveColumnBelow(page, seed.blockId, 80);
    standing = (await restingColumn(page)).block;
    expect(standing).not.toBe(seed.blockId);
    await editArtifact(
      seed.artifactId,
      {
        ops: [{ after: "end", markdown: `${SECOND_NOTE}\n`, op: "insert" }],
        summary: "One more note",
      },
      session
    );
    await expect(pane.locator("[data-block-id]", { hasText: SECOND_NOTE })).toHaveCount(1);
    expect((await restingColumn(page)).block).toBe(standing);

    if (threadId !== undefined) {
      // A reply nobody has read lands while the reader is far below the thread, so the rule
      // that marks a thread read on sight cannot have reached it. It is delivered before the
      // fall-back is drawn, because the fall-back's opening scroll runs once, when the thread
      // is drawn.
      await createArtifactComment(
        seed.artifactId,
        { body: LATE_REPLY, reply_to: threadId },
        session
      );
      await expect(pane.getByText(LATE_REPLY_FIRST_LINE)).toHaveCount(1);

      // Then the block the thread is anchored to goes. The pane falls back to the thread's own
      // card, which opens at its first unread message under Oldest first, the order the pane
      // keeps by default. That opening is the fall-back's, not the placement's.
      await editArtifact(
        seed.artifactId,
        { ops: [{ find: ANCHORED_LINE, op: "delete" }], summary: "The anchored line goes" },
        session
      );
      const firstUnread = pane.locator("[data-inbox-thread-first-unread]");
      await expect(pane.locator("[data-inbox-pane-fallback]")).toBeVisible();
      await expect(firstUnread).toBeVisible();
      await expect(firstUnread).toContainText(THREAD_REPLY);
      await expect
        .poll(() => offsetInColumn(page, "[data-inbox-thread-first-unread]"))
        .toBeLessThanOrEqual(8);
      expect(await offsetInColumn(page, "[data-inbox-thread-first-unread]")).toBeGreaterThan(-8);
    }
  } finally {
    await alice.close();
  }
}

// SLOW(>=2s): the 3 s bound waited out, then six drives and a read that fails and recovers.
test("the question pane leaves the column where the reader put it, under a heading", async ({
  browser,
}) => {
  await keepsTheColumn(browser, "heading");
});

// SLOW(>=2s): the 3 s bound waited out, then six drives and a read that fails and recovers.
test("the question pane leaves the column where the reader put it, with no heading above the block", async ({
  browser,
}) => {
  await keepsTheColumn(browser, "plain");
});

// SLOW(>=2s): the 3 s bound waited out, then six drives, a failed read and the late reply.
test("the question pane leaves the column where the reader put it, for an anchored thread", async ({
  browser,
}) => {
  await keepsTheColumn(browser, "thread");
});

// ---------------------------------------------------------------------------
// Where the column opens.
// ---------------------------------------------------------------------------

// SLOW(>=2s): a long document seeded and drawn before the placement can be polled for.
test("a question far down its document opens with its section at the top of the column", async ({
  browser,
}) => {
  const seed = await seedQuestions();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.firstAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.firstAskId);
    await expect(pane.locator(`[data-dispatch-ask-block="${QUESTION_BLOCK}"]`)).toContainText(
      QUESTION
    );

    // The editor draws its blocks after the socket syncs, so the placement is polled for.
    await expect.poll(() => headingOffset(page, SECTION_HEADING)).toBeLessThanOrEqual(8);
    expect(await headingOffset(page, SECTION_HEADING)).toBeGreaterThan(-8);
    // The section stands far down the document, so a column that was never written fails this.
    expect((await restingColumn(page)).top).toBeGreaterThan(200);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): that long document drawn, then placed again at the second question's section.
test("a second question of the document the column shows opens at its own section", async ({
  browser,
}) => {
  const seed = await seedQuestions();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.firstAskId));
    await expect(pane.getByText(DOCUMENT_FOOT)).toHaveCount(1);
    await expect.poll(() => headingOffset(page, SECTION_HEADING)).toBeLessThanOrEqual(8);

    // The column already shows this document, so nothing of it is read or drawn again: only
    // the placement sends it to the second question's own section.
    await selectRow(page, seed.secondAskId);
    await expect(pane.locator(`[data-dispatch-ask-block="${SECOND_BLOCK}"]`)).toContainText(
      SECOND_QUESTION
    );
    await expect.poll(() => headingOffset(page, SECOND_HEADING)).toBeLessThanOrEqual(8);
    expect(await headingOffset(page, SECOND_HEADING)).toBeGreaterThan(-8);
    expect((await restingColumn(page)).top).toBeGreaterThan(200);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): that long document drawn, then placed again at a question with no section.
test("a second question with no heading above it opens at the start of the column", async ({
  browser,
}) => {
  const seed = await seedQuestions();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.firstAskId));
    await expect(pane.getByText(DOCUMENT_FOOT)).toHaveCount(1);
    await expect.poll(() => headingOffset(page, SECTION_HEADING)).toBeLessThanOrEqual(8);
    expect((await restingColumn(page)).top).toBeGreaterThan(200);

    // A block before any heading has no section, so the column opens at the document's start
    // rather than staying where the question before it left the one scroller.
    await selectRow(page, seed.plainAskId);
    await expect(pane.locator(`[data-dispatch-ask-block="${PLAIN_BLOCK}"]`)).toBeVisible();
    await expect.poll(async () => (await restingColumn(page)).top).toBe(0);
    await expect(pane.getByTestId("pane-section-bar")).toHaveCount(0);
    expect(await blockOffset(page, PLAIN_BLOCK)).toBeLessThan(900);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the 3 s bound waited out between the two threads of one drawn document.
test("a second thread anchored to the same block opens at that block's section", async ({
  browser,
}) => {
  const seed = await seedTwoThreads();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await holdReadMarks(page);
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.firstItem));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.firstItem);
    await expect(pane.getByText(DOCUMENT_FOOT)).toHaveCount(1);
    await expect.poll(() => headingOffset(page, SECTION_HEADING)).toBeLessThanOrEqual(8);

    // The reader reads on, far past the block both threads are anchored to.
    await waitOutPlacement(page);
    await moveColumnBelow(page, seed.blockId, 600);
    expect((await restingColumn(page)).block).not.toBe(seed.blockId);

    // The second thread is a second item, so it arms a placement of its own although it names
    // the block the first one named.
    await selectRow(page, seed.secondItem);
    await expect(pane.getByText(SECOND_THREAD_ROOT)).toHaveCount(1);
    await expect.poll(() => headingOffset(page, SECTION_HEADING)).toBeLessThanOrEqual(8);
    expect(await headingOffset(page, SECTION_HEADING)).toBeGreaterThan(-8);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): two versions seeded and the marked one drawn before the step is read.
test("Next change moves the column to the next changed block", async ({ browser }) => {
  const seed = await seedApprovalWithMarks();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.approvalAskId));
    await expect(pane.getByRole("region", { name: "Document version 2" })).toBeVisible();
    await expect(pane.locator(".dispatch-change-block")).toHaveCount(seed.changedBlocks);
    // An approval request opens at the start of its column.
    await expect.poll(async () => (await restingColumn(page)).top).toBe(0);

    // The first marked block stands far down the document, so the step is a real move.
    await pane.locator("header").getByRole("button", { exact: true, name: "Next change" }).click();
    await expect.poll(() => offsetInColumn(page, ".dispatch-change-block")).toBeLessThanOrEqual(8);
    expect(await offsetInColumn(page, ".dispatch-change-block")).toBeGreaterThan(-8);
    expect((await restingColumn(page)).top).toBeGreaterThan(200);
    await expect(pane.locator(".dispatch-change-block").first()).toContainText(CHANGED_STEP);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the diagram is held undrawn, so the row waits the placement's 3 s limit out.
test("Skip to the answer pressed while the placement runs leaves the column at the answer", async ({
  browser,
}) => {
  const seed = await seedQuestions();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    // The diagram above the question is held undrawn, so the placement's quiet never settles
    // and the press below lands while the placement is still writing the column.
    await holdMermaidWorkers(page);
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.firstAskId));
    await expect(pane.locator(`[data-dispatch-ask-block="${QUESTION_BLOCK}"]`)).toContainText(
      QUESTION
    );
    await pane.getByRole("button", { name: "Skip to the answer" }).click();

    // Read once the bound has passed rather than polled for: a placement still holding the
    // column would have had every drawing pass until then to put it back at the section.
    await waitOutPlacement(page);
    expect(await blockOffset(page, QUESTION_BLOCK)).toBeLessThanOrEqual(8);
    expect(await blockOffset(page, QUESTION_BLOCK)).toBeGreaterThan(-8);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the 3 s bound waited out before the held replies are served and read.
test("a thread whose replies are served after the placement opens at its anchored section", async ({
  browser,
}) => {
  const seed = await seedQuestionColumn("thread");
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.setViewportSize({ height: 900, width: 1440 });
    await holdReadMarks(page);
    const replies = await holdThreadReplies(page, seed.artifactId);
    const pane = inboxPane(page);

    await page.goto(inboxAt(seed.item));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", seed.item);
    await expect(pane.getByText(DOCUMENT_FOOT)).toHaveCount(1);
    await expect.poll(() => headingOffset(page, SECTION_HEADING)).toBeLessThanOrEqual(8);
    await waitOutPlacement(page);

    // The thread is drawn after the placement has been let go, and it is drawn under the
    // anchored block, below the reader, so nothing it draws moves them. The column is still
    // where the placement put it, and the thread's own first unread message is not at its top.
    await replies.release();
    await expect(pane.getByText(THREAD_REPLY)).toHaveCount(1);
    expect(await headingOffset(page, SECTION_HEADING)).toBeLessThanOrEqual(8);
    expect(await headingOffset(page, SECTION_HEADING)).toBeGreaterThan(-8);
    expect(await offsetInColumn(page, "[data-inbox-thread-first-unread]")).toBeGreaterThan(40);
  } finally {
    await alice.close();
  }
});
