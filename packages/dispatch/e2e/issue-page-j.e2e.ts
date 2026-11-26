/** the Spec view reads the server's stored decisions, ties and results */
import type { Locator, Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { firstLineOf } from "../web/src/features/issue-page/issue-tree";
import {
  CONTROL_LABELS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  PRESET_ORDER,
  SPEC_WORDS,
  STATE_LINES,
  sentenceCase,
  VERIFICATION_WORDS,
  wordsIn,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import {
  createAsk,
  editArtifact,
  getArtifact,
  getAsk,
  getIssueCoverage,
  getIssueDecisions,
  listComments,
  listIssueAsks,
} from "./api";
import { nextFrames } from "./frames";
import {
  seedNoChildren,
  seedNoSummary,
  seedUnbrokenWords,
  seedWholeIssue,
  UNBROKEN_PATH,
} from "./issue-layout-seed";
import {
  CITATION_BLOCK_ID,
  OPEN_QUESTION_BLOCK_ID,
  requirementBlockId,
  SUMMARY_SENTENCES,
} from "./issue-layout-seed-text";
import {
  contentsPane,
  gotoIssue,
  issuePage,
  outlineControl,
  outlineSheet,
  phase,
  presetCount,
} from "./issue-page";
import {
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
  expectOpeningLanded,
  focusRow,
  focusThePage,
  longWords,
  nodeRow,
  nodeRows,
  PHONE,
  rowGroups,
  rowMore,
  rowOf,
  rowWords,
  tree,
} from "./issue-page-g";
import {
  caretInNode,
  focusedNodeId,
  gapKinds,
  idsOf,
  onScreen,
  openPreset,
  rowGaps,
} from "./issue-page-i";
import {
  becauseLines,
  contentsEntries,
  contentsLineFaults,
  decidedByCells,
  focusBranchControl,
  ownWords,
  rowAuthor,
  seedChildWithSpec,
  specRuns,
  statementBlock,
  threeThings,
} from "./issue-page-j";
import { withIssuePage } from "./issue-page-option";
import { IPHONE_13 } from "./issue-page-p";
import { TYPEFACE, typefaceAddress, typefaceStatuses } from "./issue-page-typeface";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertPageFits } from "./unclipped";
import { asUser } from "./users";

/**
 * The Spec view's rows of the new issue page: the issue's three sentences, the Contents list and
 * the Spec view, read on the real stack, because they read the server's stored decisions, asks,
 * coverage and results. Each row turns the new page on for alice with `withIssuePage` through
 * the page's `phase`, seeds what it reads with the seeds of `e2e/issue-layout-seed.ts`, and
 * reads the page through `e2e/issue-page.ts`, `e2e/issue-page-g.ts`, `e2e/issue-page-i.ts`, the
 * typeface's `e2e/issue-page-typeface.ts` and this view's own `e2e/issue-page-j.ts`.
 *
 * The rows come from the checkpoint's `e2e/issue-layout.e2e.ts`, each under the title its
 * check names in the view's `verification.md` (c-rows).
 */

// c-three-things, as the architect reads R8, with the Contents pane beside each filter and Graph:
// the three sentences stand each after its label, once in each view; from 1024 px up in the
// Contents pane, beside Conversation, each filter and Graph, so the Spec view draws no first
// group of them; below 1024 px above the view's first row, in Spec as the view's first group,
// and never in the Outline sheet, which draws the list alone; and the two rows "How it gets
// done" and "What you will be shown" are drawn nowhere. Partial on today's data:
// who wrote the sentences and when they were last read back wait on a read that holds them, so
// the head draws neither and this test never asserts an author.
// SLOW(>=2s): two seeded issues and fourteen loads of the page, each read for its head and the
// page's own draw.
test("the three sentences stand with their labels once in each view, in the Contents pane from 1024 px up and above the view's first row below it @phone", async ({
  browser,
}) => {
  // Fourteen loads of the page, each read for its head and the page's own draw.
  test.setTimeout(180_000);
  const labels = [
    CONTROL_LABELS.in_short_what,
    CONTROL_LABELS.in_short_why,
    CONTROL_LABELS.in_short_done,
  ];
  const spoken = async (head: Locator): Promise<string[]> =>
    (await head.locator("li [data-author-text]").allInnerTexts()).map((sentence) =>
      sentence.replace(/\s+/g, " ").trim()
    );
  const labelled = async (head: Locator): Promise<string[]> =>
    (await head.locator("li [data-page-chrome]").allInnerTexts()).map((label) => label.trim());
  /** The head `head` draws: the spec's own three sentences, each after its label, and neither
   *  of the two rows. The sentences are the summary node's words, which the batched reference
   *  read names last of the page's reads, so the reading waits for them. */
  const expectTheThree = async (head: Locator, where: string): Promise<void> => {
    await expect(head, `the three sentences are not drawn ${where}`).toHaveCount(1);
    await expect
      .poll(() => spoken(head), {
        message: `the spec's own three sentences are not drawn ${where}`,
      })
      .toEqual([...SUMMARY_SENTENCES]);
    expect(await labelled(head), `the three sentences carry no labels ${where}`).toEqual(labels);
    const words = (await head.innerText()).replace(/\s+/g, " ");
    for (const row of ["How it gets done", "What you will be shown"]) {
      expect(words, `"${row}" is drawn ${where}`).not.toContain(row);
    }
    expect(await head.getByRole("link").count(), `the head holds a link ${where}`).toBe(0);
  };

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    // 1440 px, no view: in the Contents pane, at the left of the outline. The sentences are read
    // before the outline is waited for, so a slot that draws none fails on its own reading.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectTheThree(
      contentsPane(page).getByTestId("issue-three-things"),
      "in the Contents pane at 1440 px"
    );
    await expectIssuePageDrawn(page);
    await expect(contentsPane(page), "no Contents pane is drawn with no view on").toHaveCount(1);
    expect(await threeThings(page).count(), "the three sentences are drawn twice").toBe(1);
    const pane = await contentsPane(page).boundingBox();
    const outline = await tree(page).boundingBox();
    expect(pane, "the Contents pane has no box").not.toBeNull();
    expect(outline, "the outline has no box").not.toBeNull();
    expect(
      (pane?.x ?? 0) + (pane?.width ?? 0),
      "the Contents pane is not at the left of the outline"
    ).toBeLessThanOrEqual((outline?.x ?? 0) + 1);

    // Who wrote the sentences and when they were last read back wait on a read that holds them,
    // and no issue holds either, so the head draws no author and no gap for them.
    expect(
      await rowAuthor(threeThings(page)).count(),
      "the head names an author, which the model does not hold"
    ).toBe(0);
    expect(
      await rowGaps(threeThings(page)).count(),
      "the head draws a gap that every issue has, which tells the reader nothing about this one"
    ).toBe(0);

    // Under a view: at 1440 px the Contents pane beside it, holding the three, which the Spec
    // view does not draw again; at 390 px no pane, and the three once above the view's first
    // row, in Spec as its first group.
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      const wide = size.width >= 1024;
      for (const preset of PRESET_ORDER) {
        if (preset === "conversation") continue;
        const where = `with the ${PRESET_NAMES[preset]} view at ${size.width} px`;
        // A view's body draws rows of its own shape - a table under Verification, a chart under
        // Plan - so the page is waited for until it has stopped reading, which is what says it
        // has drawn everything it is going to.
        const traffic = watchApiRequests(page);
        await gotoIssue(page, presetPath(seeded.key, preset));
        if (preset === "spec" || preset === "graph") await expectIssuePageDrawn(page);
        await expectLoadSettled(traffic);
        expect(
          await contentsPane(page).count(),
          wide ? `no Contents pane is drawn ${where}` : `a Contents pane is drawn ${where}`
        ).toBe(wide ? 1 : 0);
        await expectTheThree(
          wide ? contentsPane(page).getByTestId("issue-three-things") : threeThings(page),
          wide ? `in the Contents pane ${where}` : where
        );
        if (wide || (preset !== "spec" && preset !== "graph")) continue;
        const head = await threeThings(page).boundingBox();
        const firstRow = await nodeRows(page).first().boundingBox();
        expect(
          head?.y ?? Number.POSITIVE_INFINITY,
          `the three sentences do not stand above the view's first row ${where}`
        ).toBeLessThan(firstRow?.y ?? 0);
      }
    }

    // 390 px, no view: above the outline's first row, and the sheet the Outline control opens
    // draws the list and not the three.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(() => nodeRows(page).count(), { message: "no row is drawn at 390 px" })
      .toBeGreaterThan(0);
    expect(await contentsPane(page).count(), "a Contents pane is drawn at 390 px").toBe(0);
    await expectTheThree(threeThings(page), "above the outline at 390 px");
    const head = await threeThings(page).boundingBox();
    const firstRow = await nodeRows(page).first().boundingBox();
    expect(
      head?.y ?? Number.POSITIVE_INFINITY,
      "the three sentences do not stand above the outline's first row at 390 px"
    ).toBeLessThan(firstRow?.y ?? 0);
    await outlineControl(page).click();
    await expect(outlineSheet(page), "the Outline control opens no sheet at 390 px").toHaveCount(1);
    await expect(
      outlineSheet(page).getByTestId("issue-three-things"),
      "the Outline sheet draws the three sentences the view draws above its first row at 390 px"
    ).toHaveCount(0);
  });

  await phase(browser, seedNoSummary, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    // The line waits on the spec's blocks read, which says whether the spec holds a summary
    // node at all: until it answers the head says nothing about one
    // (`web/src/features/issue-page/IssueThreeThings.tsx`), so the reading waits for the line.
    await expect(
      threeThings(page),
      "the head says something other than the page's own line for a spec with no summary"
    ).toContainText(EMPTY_STATE_LINES.spec_summary);
    expect(
      await threeThings(page).locator("li").count(),
      "a spec with no summary draws sentences all the same"
    ).toBe(0);
  });
});

// c-tree, the places that draw one line for a node: a Contents entry is the node's first line by
// R1's rule, cut at a word, never cut again mid-word by its box, and a line that is a whole
// sentence carries no ellipsis after its own full stop. The issue itself is no entry of the list:
// the header is the issue. The crumb half of the checkpoint's row reads the outline, and the
// outline's own rows read it.
// SLOW(>=2s): the whole seeded issue, its comments and asks read back, and the page's load.
test("a Contents entry draws a node's first line, cut at a word", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const asks = await listIssueAsks(seeded.key);
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const words = new Map<string, string>([
      ...(await listComments(seeded.key)).map((one) => [one.id, one.body] as const),
      ...asks.map(
        (ask) =>
          [
            ask.block_id == null ? ask.id : blockNodeId(specSlug, ask.block_id),
            ask.question,
          ] as const
      ),
    ]);
    // The spec's ask entries are drawn by block id, with the words the server indexed.
    const openQuestion = blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID);
    expect(words.has(openQuestion), "the seed's open question is none of the issue's asks").toBe(
      true
    );
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    const entries = contentsEntries(contentsPane(page));
    await expect
      .poll(() => entries.count(), { message: "the Contents pane draws no entry at 1440 px" })
      .toBeGreaterThan(0);
    await expectIssuePageDrawn(page);
    await expect(
      contentsPane(page).locator(`[data-contents-entry="${openQuestion}"]`),
      "the open question is no entry of the list"
    ).toHaveCount(1);
    // The open question is an entry once the issue read has answered, and the reader's own
    // thread only once the comments read has: the list is read whole after both.
    await expect(
      contentsPane(page).locator(`[data-contents-entry="${seeded.comments.threads[0]}"]`),
      "the reader's own thread is no entry of the list"
    ).toHaveCount(1);
    // Both entries the list waits on are drawn now, so one read of its rows is the list.
    const drawn = await entries.evaluateAll((rows) =>
      rows.map((row) => ({
        id: row.getAttribute("data-contents-entry") ?? "",
        line: (row.querySelector("[data-contents-line]")?.textContent ?? "").trim(),
      }))
    );
    // A line that is a whole sentence carries no ellipsis after its own full stop: "Thread 1
    // opens here.…" read as four dots.
    expect(
      drawn.filter((entry) => /[.!?]…$/.test(entry.line)).map((entry) => entry.line),
      "an entry ends a sentence and then an ellipsis"
    ).toEqual([]);
    expect(
      drawn.map((entry) => entry.id),
      "the issue itself is an entry of the list, which repeats the header"
    ).not.toContain(seeded.key);
    for (const entry of drawn) {
      const whole = words.get(entry.id);
      if (whole === undefined) continue;
      const first = firstLineOf(whole).line;
      const cut = entry.line.endsWith("…") ? entry.line.slice(0, -1) : entry.line;
      expect(first.startsWith(cut), `the entry ${entry.id} draws other than its first line`).toBe(
        true
      );
      expect(
        cut.length === first.length || first.charAt(cut.length) === " ",
        `the entry ${entry.id} is cut inside a word: "${entry.line}"`
      ).toBe(true);
    }
  });
});

// c-typeface, the typeface arriving late. The Contents list cuts each entry's first line at a word
// to the room its box leaves it, measuring the words on a canvas in the face the list is drawn in
// (`one-line.ts`), and `font-display: swap` (`styles.css`) draws the page's first frames in the
// fallback while the typeface's file is on its way. This question is made of words the two faces
// draw at very different widths - at 14 px a "1" is 5 px in Inter and 8 px in the DejaVu fallback
// the browser suite's machines draw - so at the pane's width and at the sheet's the fallback's cut
// holds two words or more fewer than the typeface's: an entry not cut again once the file arrives
// leaves out words its box has room for.
const FACE_MOVING_QUESTION =
  "Roll 1.1.11, 1.11.1, 11.1.1, 1.1.1, 11.11, 1.111, 111.1, 11.1.11 and 1.11.11 to the hosts first?";

// SLOW(>=2s): two readers load the page, the second with the typeface's file held until its list
// is drawn in the fallback, and each cut is waited for as the face arrives.
test("a Contents entry is cut again in the typeface once its file arrives @phone", async ({
  browser,
}) => {
  // The list is in the pane at 1440 px, and on a phone in the Outline sheet.
  const phone = test.info().project.name === "iphone";
  const size = phone ? IPHONE_13 : DESKTOP;
  await phase(browser, seedNoChildren, async (page, seeded) => {
    const question = await createAsk(seeded.key, { question: FACE_MOVING_QUESTION });
    await page.setViewportSize(size);
    await gotoIssue(page, `/issues/${seeded.key}`);
    if (phone) await outlineControl(page).click();
    await expect
      .poll(() => contentsEntries(phone ? outlineSheet(page) : contentsPane(page)).count(), {
        message: `the Contents list draws no entry at ${size.width} px`,
      })
      .toBeGreaterThan(0);
    if (phone) {
      await page.keyboard.press("Escape");
      await expect(outlineSheet(page), "Escape left the Outline sheet open").toHaveCount(0);
    }
    await expectIssuePageDrawn(page);
    const address = await typefaceAddress(page);

    // A reader whose request for the file is held until the list is drawn in the fallback, the
    // Outline sheet opened on a phone before the file arrives.
    const reader = await asUser(browser, "alice");
    const arrives = Promise.withResolvers<void>();
    try {
      await withIssuePage(reader, "alice");
      let held = 0;
      await reader.route(address, async (route) => {
        held += 1;
        await arrives.promise;
        await route.continue();
      });
      const late = await reader.newPage();
      await late.setViewportSize(size);
      // `commit`, because the page's load event waits on the held file.
      await late.goto(`/issues/${seeded.key}`, { waitUntil: "commit" });
      await expectIssuePageDrawn(late);
      if (phone) await outlineControl(late).click();
      const entry = (phone ? outlineSheet(late) : contentsPane(late)).locator(
        `[data-contents-entry="${question.id}"]`
      );
      await expect(entry, "the Contents list does not list the question").toBeVisible();
      await expect
        .poll(() => typefaceStatuses(late), { message: `the ${TYPEFACE} file is not held` })
        .toEqual(["loading"]);
      expect(held, "no request for the typeface reached the hold").toBeGreaterThan(0);
      await expect
        .poll(() => contentsLineFaults(entry, FACE_MOVING_QUESTION), {
          message: "the entry's line is not cut to its box in the fallback",
        })
        .toEqual([]);
      const inFallback = await entry.locator("[data-contents-line]").textContent();

      arrives.resolve();
      await expect
        .poll(() => typefaceStatuses(late), {
          message: `the ${TYPEFACE} face did not load once its file was let through`,
        })
        .toEqual(["loaded"]);
      await expect
        .poll(() => contentsLineFaults(entry, FACE_MOVING_QUESTION), {
          message: `the entry's line is not cut again in ${TYPEFACE} once its file arrived`,
        })
        .toEqual([]);
      expect(
        await entry.locator("[data-contents-line]").textContent(),
        "the typeface moved no word of the cut, so this row read nothing"
      ).not.toBe(inFallback);
    } finally {
      arrives.resolve();
      await reader.close();
    }
  });
});

// R2, from the Contents clause of the checkpoint's `one panel of properties holds no
// conversation list @phone`: each question and each of the reader's own threads is an entry, a
// press or Enter goes to its node in the outline, the entry the reader is in is marked, and Focus
// branch draws that branch alone, then the whole outline again. Below 1024 px the list is in the
// Outline sheet, which a choice closes, by pointer and by Enter from the foot of the page; where
// that choice leaves the focused row is the next row's. No view is turned on between the two
// readings of Focus branch: the outline drawn again after a view is the outline's own rows' to
// read.
// SLOW(>=2s): the whole seeded issue, two loads of the page, and six moves of the focus and the
// toggle, each waited on until the outline redraws.
test("a Contents entry goes to its node, and Focus branch draws that branch alone @phone", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const question = blockNodeId(specSlug, seeded.openQuestion.blockId);

    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    // The outline draws the tree read by read, and the issue read alone puts a child first:
    // the whole outline's first row is read once the page has stopped reading.
    const traffic = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    const entry = contentsPane(page).locator(`[data-contents-entry="${question}"]`);
    await expect
      .poll(() => entry.count(), {
        message: "the Contents list draws no entry for the open question",
      })
      .toBe(1);
    await expectIssuePageDrawn(page);
    await expect.poll(() => nodeRows(page).count()).toBeGreaterThan(0);
    await expectLoadSettled(traffic);
    const firstRow = (await drawnRows(page)).find((row) => row.visible)?.id;
    expect(firstRow, "the outline draws no first row").toBeDefined();
    await nodeRows(page).first().focus();
    await entry.focus();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => focusedNodeId(page), { message: "Enter on an entry went to no node" })
      .toBe(question);
    await expect(entry, "the entry the reader is in is not marked").toHaveAttribute(
      "aria-selected",
      "true"
    );
    await nodeRows(page).first().focus();
    await entry.click();
    await expect
      .poll(() => focusedNodeId(page), { message: "a press on an entry went to no node" })
      .toBe(question);
    const focusBranch = focusBranchControl(contentsPane(page));
    // The rendered rows, by the levels the outline draws them at: the first, and each later
    // one that is not under it.
    const branch = async () => {
      const rows = (await drawnRows(page)).filter((row) => row.visible);
      const level = Number(rows[0]?.level ?? "");
      return {
        first: rows[0]?.id,
        outside: idsOf(rows.slice(1).filter((row) => !(Number(row.level ?? "") > level))),
      };
    };
    await focusBranch.click();
    await expect(focusBranch).toHaveAttribute("aria-pressed", "true");
    await expect
      .poll(branch, { message: "Focus branch did not draw the entry's branch alone" })
      .toEqual({ first: question, outside: [] });
    await focusBranch.click();
    await expect(focusBranch).toHaveAttribute("aria-pressed", "false");
    await expect
      .poll(async () => (await drawnRows(page)).find((row) => row.visible)?.id, {
        message: "Focus branch turned off did not draw the whole outline again",
      })
      .toBe(firstRow);

    // 390 px: the list is in the Outline sheet, and a choice there goes to the node and closes
    // the sheet (`useContentsChosen`).
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await outlineControl(page).click();
    const chosen = outlineSheet(page).locator(`[data-contents-entry="${question}"]`);
    await expect(chosen, "the Outline sheet does not list the open question").toHaveCount(1);
    await chosen.click();
    await expect(outlineSheet(page), "a choice in the Outline sheet left it open").toHaveCount(0);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "a choice in the Outline sheet went to no node",
      })
      .toBe(question);

    // The same choice by Enter, from the foot of the page, where the pointer's choice did not
    // leave the reader.
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await outlineControl(page).click();
    await expect(
      chosen,
      "the Outline sheet opened again does not list the open question"
    ).toHaveCount(1);
    await chosen.focus();
    await expect(chosen).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(
      outlineSheet(page),
      "a choice in the Outline sheet by Enter left it open"
    ).toHaveCount(0);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "a choice in the Outline sheet by Enter went to no node",
      })
      .toBe(question);
  });
});

// Row (4)'s choice in the Outline sheet, by pointer and by Enter from the foot of the page,
// leaves the focused row below the sticky view row, not behind it. Where the row stands is the
// page's `scroll-padding-top` - the larger of the band's and the view row's heights - and the
// row's own placing (`useRowOnScreen`), both in this branch now, so this row, which skipped until
// they were, is one of c-rows' own.
// SLOW(>=2s): the whole seeded issue, a load of the page at 390 px, and two choices in the
// Outline sheet, each waited on until the focus lands and the page redraws.
test("a choice in the Outline sheet leaves the focused row below the sticky view row @phone @phone-only", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const question = blockNodeId(specSlug, seeded.openQuestion.blockId);
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const root = await page.evaluate(() => {
      const style = getComputedStyle(document.documentElement);
      return {
        padding: style.scrollPaddingTop,
        viewRow: style.getPropertyValue("--issue-view-row-height").trim(),
      };
    });
    expect(root.viewRow, "the view row publishes no --issue-view-row-height").not.toBe("");
    expect(root.padding, "the page keeps no scroll-padding-top for its sticky rows").not.toBe(
      "auto"
    );
    // The focused row's top less the sticky view row's bottom, once the move has been drawn:
    // -1 or more where the row stands below the view row.
    const belowViewRow = async (): Promise<number> => {
      await nextFrames(page);
      return await page.evaluate((id) => {
        const issue = document.querySelector("[data-issue-page]");
        const row = issue?.querySelector(`[data-node-row][data-node-id="${CSS.escape(id)}"]`);
        const bar = issue?.querySelector("[data-filter-row]");
        if (row == null || bar == null) return Number.NaN;
        return row.getBoundingClientRect().top - bar.getBoundingClientRect().bottom;
      }, question);
    };

    await outlineControl(page).click();
    const chosen = outlineSheet(page).locator(`[data-contents-entry="${question}"]`);
    await expect(chosen, "the Outline sheet does not list the open question").toHaveCount(1);
    await chosen.click();
    await expect(outlineSheet(page), "a choice in the Outline sheet left it open").toHaveCount(0);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "a choice in the Outline sheet went to no node",
      })
      .toBe(question);
    await expect
      .poll(belowViewRow, {
        message:
          "a choice in the Outline sheet by pointer puts the focused row under the sticky view row",
      })
      .toBeGreaterThanOrEqual(-1);

    // The same choice by Enter, from the foot of the page, so the choice itself must bring the
    // row back on screen rather than find it where the pointer's choice left it.
    await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
    await outlineControl(page).click();
    await expect(
      chosen,
      "the Outline sheet opened again does not list the open question"
    ).toHaveCount(1);
    await chosen.focus();
    await expect(chosen).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(
      outlineSheet(page),
      "a choice in the Outline sheet by Enter left it open"
    ).toHaveCount(0);
    await expect
      .poll(() => focusedNodeId(page), {
        message: "a choice in the Outline sheet by Enter went to no node",
      })
      .toBe(question);
    await expect
      .poll(belowViewRow, {
        message:
          "a choice in the Outline sheet by Enter puts the focused row under the sticky view row",
      })
      .toBeGreaterThanOrEqual(-1);
  });
});

// c-home. The Spec view's body: the decision beside the statements it shaped, in the words it
// came from, and one line on each statement's own row (R5), drawn with the focus elsewhere and
// the pointer off the page; the tester's F7 is read here too - the line is a link to the
// decision, and `w` moves the focus to it - and F20: no text of the view is a raw address or a
// fence. Partial on today's data: a statement carries no `from` yet, so no statement hangs under
// an answered ask and the body draws one group with no heading; a criterion, an agent decision's
// level of uncertainty and its reason, and the versions of a view's query wait on reads of their
// own, so this row reads the live Spec view.
// SLOW(>=2s): the whole seeded issue, its decisions and answer read back, and the pointer and
// the focus moved across the view, each waited on until it redraws.
test("a spec node shows the decisions that shaped it at every version", async ({ browser }) => {
  test.setTimeout(150_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const decisionNode = (index: number) => blockNodeId(specSlug, seeded.decisions[index] ?? "");
    const stored = (await getIssueDecisions(seeded.key)).decisions ?? [];
    expect(
      stored.some((one) => one.kind === "person"),
      "the seed holds no decision the server reads as a person's"
    ).toBe(true);
    const answered = await getAsk(seeded.answeredAskId);
    const answer = answered.ask.answer?.text ?? "";
    expect(answer, "the seeded ask holds no answer to quote").not.toBe("");
    const quote = firstLineOf(answer).line;
    // An agent's decision is quoted by its first block's line, as every one-line place cuts a
    // node's (`lineWords`): for a decision that is its own heading, never the heading run into
    // the paragraph under it.
    const agent = stored.find(
      (one) => one.kind === "agent" && one.text.includes("Chosen so the page makes no request")
    );
    // The decisions read gives the heading its own field and puts the whole typed block in text.
    const agentQuote = firstLineOf(agent?.heading ?? "").line;
    expect(agentQuote, "the seed holds no agent's decision to quote").not.toBe("");

    const traffic = watchApiRequests(page);
    await openPreset(page, seeded.key, "spec");
    const body = issuePage(page).locator("[data-spec-body]");
    await expect
      .poll(() => body.locator("[data-spec-statement]").count(), {
        message: "the Spec view draws no statement",
      })
      .toBeGreaterThan(0);
    await expectIssuePageDrawn(page);
    // A person's decision is drawn as theirs once the asks read has answered as well as the
    // decisions read, which go out together and answer in either order.
    const yours = issuePage(page).locator('[data-spec-cell][data-said="you"]');
    await expect(yours, "the person's decision is not drawn once").toHaveCount(1);
    // Nothing is focused and nothing is hovered: the lines are drawn by the page itself.
    await focusThePage(page);

    // The person's decision draws the three statements its ties point to, once, under one
    // cell, in the words it came from: the person's own answer to its first sentence end, the
    // question it answers, who answered, and a link to the whole answer.
    const cells = await decidedByCells(page);
    const mine = cells.filter((cell) => cell.said === "you");
    expect(mine, "the person's decision is not drawn once").toHaveLength(1);
    expect(
      mine[0]?.ids,
      "the person's decision does not stand beside the three statements it shaped"
    ).toEqual([statement(3), statement(4), statement(5)]);
    expect(mine[0]?.text).toContain(SPEC_WORDS.said_you);
    expect(mine[0]?.text).toContain(`“${quote}”`);
    expect(mine[0]?.text).toContain(answered.ask.question);
    expect(mine[0]?.text).toContain("alice");
    await expect(
      yours.locator("[data-spec-whole]"),
      "the cell carries no link to the whole answer"
    ).toHaveAttribute("href", `/issues/${seeded.key}/asks/${seeded.answeredAskId}`);

    // The agent's decision stands beside the two it shaped, marked as an agent's, in its own
    // words.
    const agents = cells.filter(
      (cell) => cell.said === "agent" && cell.text.includes("Chosen so the page makes no request")
    );
    expect(
      agents.flatMap((cell) => cell.ids).sort(),
      "the agent's decision does not stand beside the two statements it shaped"
    ).toEqual([statement(1), statement(2)].sort());
    expect(agents[0]?.text).toContain(SPEC_WORDS.said_agent);

    // One line on each statement's own row, with the same words, who decided and when; a
    // statement a second decision shaped names it after the first.
    const lines = await becauseLines(page);
    for (const index of [3, 4, 5]) {
      expect(
        lines[statement(index)],
        `statement ${index} carries no line for its decision`
      ).toContain(`${SPEC_WORDS.because} “${quote}”`);
      expect(lines[statement(index)]).toContain("alice");
    }
    for (const index of [4, 5]) {
      expect(
        lines[statement(index)],
        `statement ${index} does not name the second decision that shaped it`
      ).toContain(SPEC_WORDS.informed_by);
    }
    for (const index of [1, 2]) {
      expect(
        lines[statement(index)],
        `statement ${index} carries no line for the agent's decision`
      ).toContain(`${SPEC_WORDS.because} “${agentQuote}”`);
    }
    // A replaced decision draws no line: the statement it shaped stands with none, and its
    // cell says so, as does the cell of a statement no decision shaped (R35).
    expect(lines[statement(6)] ?? "", "the replaced decision draws a line").toBe("");
    expect(lines[statement(7)] ?? "", "a statement no decision shaped carries a line").toBe("");
    const unshaped = cells.find((cell) => cell.ids.includes(statement(6)));
    expect(
      unshaped?.said,
      "a statement only a replaced decision shaped names a decision"
    ).toBeNull();
    expect([
      EMPTY_STATE_LINES.spec_no_decision_one,
      EMPTY_STATE_LINES.spec_no_decision_many,
    ]).toContain(unshaped?.text);

    // Every statement of the query is a row of this body, and no decision and no issue is.
    const rows = await drawnRows(page);
    for (const requirement of seeded.requirements) {
      expect(
        rowOf(rows, blockNodeId(specSlug, requirement)),
        `the Spec view does not draw ${requirement}`
      ).toBeDefined();
    }
    expect(rowOf(rows, decisionNode(0)), "the Spec view draws a decision as a row").toBeUndefined();
    expect(rowOf(rows, seeded.key), "the Spec view draws the issue as a row").toBeUndefined();

    // F20: a reference is drawn by its name, never as its `dispatch://` address or a `:::`
    // fence of the stored source, in any text of the view, a cut or hidden one too.
    const every = await body.evaluate((element) => element.textContent ?? "");
    expect(/dispatch:\/\/|:::/.test(every), "the Spec view draws a raw reference").toBe(false);

    // Nothing opens on hover: the pointer crosses every cell and the body's text is the same.
    // The text is taken once the page has stopped reading, so a read that lands late is not
    // read as a change the pointer made.
    await expectLoadSettled(traffic);
    const before = await body.innerText();
    // The page has stopped reading, so the cells drawn now are every cell the view draws.
    for (const cell of await issuePage(page).locator("[data-spec-cell]").all()) await cell.hover();
    expect(await body.innerText(), "the body changed under the pointer").toBe(before);

    // F7: with the focus on a statement, its line is the link to the decision that shaped it,
    // and `w` moves the focus to that decision, which then draws the whole answer.
    const shaped = nodeRow(page, statement(3)).first();
    await shaped.focus();
    await expect(
      shaped.locator("[data-spec-because] a[data-spec-because-link]"),
      "the focused statement does not link the decision that shaped it"
    ).toHaveAttribute("href", `/issues/${seeded.key}/asks/${seeded.answeredAskId}`);
    await page.keyboard.press("w");
    await expect
      .poll(
        () =>
          page.evaluate(() => ({
            cell: document.activeElement?.hasAttribute("data-spec-cell") ?? false,
            said: document.activeElement?.getAttribute("data-said") ?? null,
          })),
        { message: "w did not move the focus to the decision behind the statement" }
      )
      .toEqual({ cell: true, said: "you" });
    // The focused cell draws the person's whole answer character for character: under its first
    // sentence where the answer says more, and as that sentence where it is all the person wrote,
    // as the seeded answer is, so the cell draws nothing twice (`spec-decided-by.ts`'s `whole`).
    await expect
      .poll(
        () =>
          yours.evaluate(
            (cell) =>
              (
                cell.querySelector("[data-spec-whole-answer]") ??
                cell.querySelector("[data-spec-quote] [data-author-text]")
              )?.textContent ?? null
          ),
        { message: "the focused decision does not draw the person's whole answer" }
      )
      .toBe(answer);

    // `w` on a statement the person's answer shaped and an agent's decision informed as well
    // goes to the cell beside it too, though the tree's order names the agent's decision first
    // (`decisionBehind`): the person's decision heads the statement's run.
    const informed = nodeRow(page, statement(4)).first();
    await informed.focus();
    await expect(informed).toBeFocused();
    await page.keyboard.press("w");
    await expect(yours, "w on statement 4 does not reach the decision beside it").toBeFocused();
    await expect(
      body.locator("[data-row-report]"),
      "w on statement 4 does not reach the decision beside it"
    ).toHaveCount(0);
  });
});

// c-presets' and c-chain's Spec halves (R4, R6, R10, R17). Under Spec the rows are the spec's
// statements alone, in the order the coverage read gives them, and each hangs under the issue, so
// no row draws a parent line and no group a heading; the view's count is their number. A
// decision's chip, followed from the outline, opens the view on the statement it names, and Enter
// on a statement opens the issue's address at its block, with the focus on its row of the
// outline. "No check" is a statement's mark and the legend's word, and never a gap beside the
// mark too. A child's Spec view draws its own statements and none of its parent's: the tree reads
// the issue's own coverage alone (reading (e) of the part's plan).
// SLOW(>=2s): two seeded issues, the whole one and a parent with its child, and four loads of the
// page and two followed links, each waited on until the page redraws.
test("the Spec view draws statements alone, each parent once, and no check once", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const decision = blockNodeId(specSlug, seeded.decisions[0] ?? "");
    const coverage = (await getIssueCoverage(seeded.key)).requirements ?? [];
    const statements = coverage.map((one) => blockNodeId(specSlug, one.block_id));
    const unchecked = coverage
      .filter((one) => one.checks === 0)
      .map((one) => blockNodeId(specSlug, one.block_id))
      .sort();
    expect(unchecked, "the seed holds no statement that no check serves").not.toEqual([]);
    const body = issuePage(page).locator("[data-spec-body]");

    await openPreset(page, seeded.key, "spec");
    await expect
      .poll(async () => (await drawnRows(page)).length, {
        message: "the Spec view draws no statement",
      })
      .toBeGreaterThan(0);
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: "the Spec view's rows are not the coverage read's statements, in its order",
      })
      .toEqual(statements);
    await expect(
      presetCount(page, "spec"),
      "the Spec count is not the number of the view's statements"
    ).toHaveText(String(statements.length));
    expect(
      await body.locator("[data-spec-statement] [data-node-parent-line]").count(),
      "a statement under the issue draws a parent line"
    ).toBe(0);
    expect(
      await body.locator("[data-spec-group-heading]").count(),
      "the Spec view heads a group of statements that hang under the issue"
    ).toBe(0);

    // With no view on, `f` steps to the decision's first chip and Enter follows it.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await focusRow(page, decision);
    await page.keyboard.press("f");
    await page.keyboard.press("Enter");
    await expect(page, "following the decision's chip does not open the Spec view").toHaveURL(
      new RegExp(`${presetPath(seeded.key, "spec")}(#|$)`)
    );
    await expect
      .poll(() => focusedNodeId(page), {
        message: "following the decision's chip does not put the focus on the statement",
      })
      .toBe(statement(1));

    // No check is the mark of exactly the statements no check serves, the legend names it, and
    // no statement says it again as a gap beside its mark.
    await expect
      .poll(
        async () =>
          (
            await body
              .locator('[data-spec-statement][data-spec-mark="no_check"]')
              .evaluateAll((rows) => rows.map((row) => row.getAttribute("data-node-id") ?? ""))
          ).sort(),
        { message: "the statements marked no check are not those no check serves" }
      )
      .toEqual(unchecked);
    await expect(
      body.locator("[data-spec-legend]"),
      "the legend does not name no check"
    ).toContainText(SPEC_WORDS.no_check);
    expect(
      await body.locator('[data-spec-statement] [data-gap-kind="no_check"]').count(),
      "the Spec filter says No check twice on one row"
    ).toBe(0);
    expect(await gapKinds(page), "a gap the Spec view draws names no kind").not.toContain("");
    // The view's own words, as row (7) reads them (reading (g)): an author's words are theirs.
    expect((await ownWords(body)).join(" "), "the Spec view reads proven").not.toMatch(/proven/i);

    // Enter on a statement puts the caret at the start of its text, and the statement is edited
    // in place, in its row of the Spec view, which stays drawn at its own address (part Y's R9).
    // A statement draws no body, so it is focused directly rather than by the outline's
    // `focusRow`, which waits for one.
    const third = nodeRow(page, statement(3)).first();
    await third.focus();
    await expect(third).toBeFocused();
    const address = page.url();
    await page.keyboard.press("Enter");
    await expect
      .poll(async () => (await caretInNode(page)) && (await focusedNodeId(page)) === statement(3), {
        message: "Enter on a statement does not put the caret in its text",
      })
      .toBe(true);
    expect(
      await textBeforeCaret(page, requirementBlockId(3)),
      "Enter on a statement does not put the caret at the start of its text"
    ).toBe("");
    expect(page.url(), "Enter on a statement leaves the Spec view's address").toBe(address);
    expect(await body.count(), "Enter on a statement does not keep the Spec view drawn").toBe(1);
  });

  await phase(browser, seedChildWithSpec, async (page, seeded) => {
    const own = seeded.child.requirements.map((id) => blockNodeId(seeded.child.specSlug, id));
    const parents = seeded.parent.requirements.map((id) => blockNodeId(seeded.parent.specSlug, id));
    await openPreset(page, seeded.child.key, "spec");
    await expectIssuePageDrawn(page);
    await expect
      .poll(async () => idsOf(await drawnRows(page)), {
        message: "the child's Spec view does not draw its own two statements alone",
      })
      .toEqual(own);
    const rows = await drawnRows(page);
    expect(
      rows.filter((row) => row.context).map((row) => row.id),
      "the child's Spec view draws its own statement as context"
    ).toEqual([]);
    expect(
      idsOf(rows).filter((id) => parents.includes(id)),
      "the child's Spec view draws a statement of its parent"
    ).toEqual([]);
  });
});

/** The text a caret can enter between the start of block `blockId` and the caret, in the editable
 *  that holds the focus; null where the caret is not in that block. "" is a caret at the start of
 *  its text. A block's label is drawn in `contenteditable="false"`, which no caret enters, so it
 *  is no text before the caret (`node-caret.ts`'s `firstPlace`). */
function textBeforeCaret(page: Page, blockId: string): Promise<string | null> {
  return page.evaluate((block) => {
    const selection = window.getSelection();
    const anchor = selection?.anchorNode ?? null;
    const editable = document.activeElement?.closest('[contenteditable="true"]') ?? null;
    const drawn = editable?.querySelector(`[data-block-id="${CSS.escape(block)}"]`) ?? null;
    if (selection === null || anchor === null || drawn === null || !drawn.contains(anchor)) {
      return null;
    }
    const before = document.createRange();
    before.setStart(drawn, 0);
    before.setEnd(anchor, selection.anchorOffset);
    let text = "";
    const walker = document.createTreeWalker(drawn, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      if (node.parentElement?.isContentEditable !== true || !before.intersectsNode(node)) continue;
      const end = node === before.endContainer ? before.endOffset : undefined;
      text += (node.textContent ?? "").slice(0, end);
    }
    return text;
  }, blockId);
}

/** The block id of the statement of 200 words the picture's row writes into the seeded spec,
 *  after a reference to one of the issue's children. */
const LONG_BLOCK_ID = "r-long";

// R4, R5 and R6 against the approved picture `page-v3-spec-1440.png`. At 1440 px, from
// the top: the legend's four marks, each a shape and its word; the column heads; the first
// group, the three sentences standing in the Contents pane beside the view and not in it, so
// they are drawn once. The page's own words never say "Requirement" and are each a word
// of `page-words.ts` (R7, reading (g) of the part's plan). A cell is 280 px wide and 20 px left of
// its statements. `t s` from the outline leaves the focus on a statement of the view, on screen.
// A statement of 200 words is cut at six lines with "more · N words" and drawn whole with the
// focus; a statement's chips stand after its last word, and a link in its words is the words'
// colour, read on the long statement, whose words open on a child's reference: an issue's
// reference is a link in a row's words, where a block's or a document's is a chip in place
// (`RowText.tsx`). Where the sticky heads stand after a scroll, against the header's
// `--issue-identity-height`, is the next row's (reading (l)). At 390 px, as
// `page-v3-spec-390.png` draws it, a run is one column, each cell above its first statement, and
// the page fits its viewport.
// SLOW(>=2s): the whole seeded issue and a statement of 200 words written into its spec, two loads
// of the page, the view's chord and the moves up its statements, each waited on until the page
// redraws.
test("the Spec view stands as the approved picture draws it @phone", async ({ browser }) => {
  test.setTimeout(150_000);
  const phone = test.info().project.name === "iphone";
  const size = phone ? PHONE : DESKTOP;
  const at = `${size.width} px`;
  const seed = async () => {
    const seeded = await seedWholeIssue();
    const child = seeded.children.keys[0];
    expect(child, "the seed holds no child for the long statement to cite").toBeDefined();
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(12)}`,
          markdown: statementBlock(
            LONG_BLOCK_ID,
            `Read dispatch://${child} first. ${longWords("long", 200)}`
          ),
          op: "insert",
        },
      ],
    });
    return seeded;
  };
  await phase(browser, seed, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const body = issuePage(page).locator("[data-spec-body]");
    const marks = body.locator("[data-spec-legend] [data-spec-legend-mark]");
    await page.setViewportSize({ height: size.height, width: size.width });
    await openPreset(page, seeded.key, "spec");
    await expect
      .poll(() => marks.count(), { message: `the Spec view draws no legend at ${at}` })
      .toBe(4);
    await expectIssuePageDrawn(page);
    // The cells draw their decisions once the decisions read has answered.
    await expect
      .poll(() => body.locator("[data-spec-cell] [data-spec-said]").count(), {
        message: `the Spec view draws no decision beside its statements at ${at}`,
      })
      .toBeGreaterThan(0);

    if (phone) {
      // The poll above saw the decisions drawn, so one read of the runs is the whole view.
      const runs = await specRuns(page);
      expect(runs.length, `the Spec view draws no run of statements at ${at}`).toBeGreaterThan(0);
      for (const { cell, first } of runs) {
        expect(cell, `a run draws no cell at ${at}`).not.toBeNull();
        expect(first, `a run draws no statement at ${at}`).not.toBeNull();
        if (cell === null || first === null) continue;
        expect(
          Math.abs(cell.left - first.left),
          `a run at ${at} draws its cell beside its statements, not in one column with them`
        ).toBeLessThanOrEqual(1);
        expect(
          cell.bottom,
          `a cell at ${at} reaches below the top of its first statement`
        ).toBeLessThanOrEqual(first.top + 0.5);
      }
      await assertPageFits(page);
      return;
    }

    // The chips name their targets once the batched read of names has answered, which is the
    // last of the page's reads.
    const cites = blockNodeId(specSlug, CITATION_BLOCK_ID);
    await expect
      .poll(
        async () => (await rowGroups(page)).find((row) => row.id === cites)?.chips.length ?? 0,
        {
          message: `${cites} on Spec at ${at} draws no chip`,
        }
      )
      .toBeGreaterThan(0);

    // From the top, by each box's top.
    const legend = await marks.evaluateAll((all) =>
      all.map((mark) => ({
        shapes: mark.querySelectorAll("svg[data-glyph]").length,
        top: mark.getBoundingClientRect().top,
        word: (mark.textContent ?? "").trim(),
      }))
    );
    expect(
      legend.map((mark) => mark.word),
      `the legend does not name the four marks in order at ${at}`
    ).toEqual([
      VERIFICATION_WORDS.states.met,
      VERIFICATION_WORDS.states.not_met,
      VERIFICATION_WORDS.states.unknown,
      SPEC_WORDS.no_check,
    ]);
    expect(
      legend.map((mark) => mark.shapes),
      `a mark of the legend draws no shape beside its word at ${at}`
    ).toEqual([1, 1, 1, 1]);
    const heads = body.locator("[data-spec-head]");
    expect(
      await heads.locator("[data-page-chrome]").allTextContents(),
      `the column heads are not Decided by and Statement at ${at}`
    ).toEqual([SPEC_WORDS.decided_by, sentenceCase(NODE_TYPE_NAMES.requirement)]);
    // From 1024 px up the three sentences stand in the Contents pane beside the view, so the
    // Spec view draws no first group of them: they are drawn once.
    await expect(
      contentsPane(page).getByTestId("issue-three-things"),
      `the Contents pane beside Spec draws no three sentences at ${at}`
    ).toHaveCount(1);
    await expect(
      body.getByTestId("issue-three-things"),
      `the Spec view draws the three sentences beside the pane that holds them at ${at}`
    ).toHaveCount(0);
    const topOf = async (part: Locator): Promise<number> =>
      (await part.boundingBox())?.y ?? Number.NaN;
    const order = [
      { name: "the legend", top: Math.max(...legend.map((mark) => mark.top)) },
      { name: "the column heads", top: await topOf(heads) },
      { name: "the first group", top: await topOf(body.locator("[data-spec-group]").first()) },
    ];
    for (const [place, part] of order.entries()) {
      const above = order[place - 1];
      if (above === undefined) continue;
      expect(part.top, `${part.name} does not stand under ${above.name} at ${at}`).toBeGreaterThan(
        above.top
      );
    }

    // The page's own words: no "Requirement", and each a word the page declares.
    const words = (await ownWords(issuePage(page))).flatMap((text) => [...wordsIn(text)]);
    expect(words, `the page draws "Requirement" in its own words on Spec at ${at}`).not.toContain(
      "requirement"
    );
    expect(
      [...new Set(words.filter((word) => !DECLARED_WORDS.includes(word)))],
      `the page's own words on Spec at ${at} hold words the page does not declare`
    ).toEqual([]);

    // Each cell is 280 px wide, and 20 px left of its statements.
    const runs = await specRuns(page);
    expect(runs.length, `the Spec view draws no run of statements at ${at}`).toBeGreaterThan(0);
    for (const { cell, first } of runs) {
      expect(cell, `a run draws no cell at ${at}`).not.toBeNull();
      expect(first, `a run draws no statement at ${at}`).not.toBeNull();
      if (cell === null || first === null) continue;
      expect(
        Math.abs(cell.width - 280),
        `a cell is ${cell.width} px wide at ${at}, not 280 px`
      ).toBeLessThanOrEqual(1);
      expect(
        Math.abs(first.left - cell.right - 20),
        `a cell's right edge stands ${first.left - cell.right} px left of its statements at ${at}, not 20 px`
      ).toBeLessThanOrEqual(1);
    }

    // A statement's chips stand after its last word (`rowGroups`).
    const row = (await rowGroups(page)).find((one) => one.id === cites);
    for (const chip of row?.chips ?? []) {
      expect(
        chip.inWords,
        `${cites} on Spec at ${at} draws the chip "${chip.text}" on a line under its words`
      ).toBe(true);
    }
    // A link in a statement's words is the words' colour, read on the long statement's link to
    // the child it cites.
    const longId = blockNodeId(specSlug, LONG_BLOCK_ID);
    await expect
      .poll(
        async () => (await rowGroups(page)).find((one) => one.id === longId)?.links.length ?? 0,
        { message: `${longId} on Spec at ${at} draws no link in its words` }
      )
      .toBeGreaterThan(0);
    // The poll above saw the row's link drawn, so one read of its groups holds the link.
    const linked = (await rowGroups(page)).find((one) => one.id === longId);
    for (const link of linked?.links ?? []) {
      expect(
        link.color,
        `${longId} on Spec at ${at} draws a link in its words in another colour than the words'`
      ).toBe(link.words);
    }

    // The statement of 200 words is cut at six lines with "more · N words", and whole with the
    // focus. The page measures a row's cut once the row stands within a screen of the viewport,
    // in the frame after it comes there, so the row is brought there before its cut is read.
    const long = nodeRow(page, longId).first();
    await long.scrollIntoViewIfNeeded();
    await expect(rowMore(long), `the long statement draws no more at ${at}`).toHaveCount(1);
    const cut = (await rowWords(long).boundingBox())?.height ?? Number.POSITIVE_INFINITY;
    expect(
      cut,
      `the long statement is ${cut} px tall at ${at}, more than six lines`
    ).toBeLessThanOrEqual(6 * 24);
    const more = ((await rowMore(long).textContent()) ?? "").trim();
    const held = new RegExp(
      `^${CONTROL_LABELS.read_more} · ([\\d,]+) ${CONTROL_LABELS.read_more_words}$`,
      "u"
    ).exec(more);
    expect(held, `the long statement draws "${more}", not more · N words`).not.toBeNull();
    expect(
      Number((held?.[1] ?? "0").replaceAll(",", "")),
      "the long statement's cut holds back 20 words or fewer"
    ).toBeGreaterThan(20);
    await long.focus();
    await expect(long).toBeFocused();
    await expect(rowMore(long), "the focused long statement draws more").toHaveCount(0);
    await expect
      .poll(async () => (await rowWords(long).boundingBox())?.height ?? 0, {
        message: "the focused long statement is drawn no taller than its cut",
      })
      .toBeGreaterThan(cut);

    // From the outline, `t s` leaves the focus on a statement of the view's tree, on screen.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectOpeningLanded(page);
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    const statements = body.locator("[data-spec-tree] [data-spec-statement]");
    await expect
      .poll(
        async () => {
          const focused = await focusedNodeId(page);
          const ids = await statements.evaluateAll((rows) =>
            rows.map((one) => one.getAttribute("data-node-id"))
          );
          return focused !== null && ids.includes(focused);
        },
        { message: "t s does not put the focus on a statement of the Spec view" }
      )
      .toBe(true);
    // The poll above saw a statement take the focus, so one read names that statement.
    const focused = (await focusedNodeId(page)) ?? "";
    await expect
      .poll(() => onScreen(nodeRow(page, focused)), {
        message: "t s does not put the focus on a statement on screen",
      })
      .toBe(true);

    // R11: the focused row is never under a sticky header, and the
    // view's column heads stay at the top as one (R4). Each move up the statements scrolls the
    // one it reaches into view, which must leave it below the heads, not behind them. Found by
    // the tester on a running build at 8caa4357, where each statement the arrows moved up to
    // stood with its top at the screen's edge and its first line under the 31 px of heads.
    const upward = await statements.evaluateAll((rows) =>
      rows.map((one) => one.getAttribute("data-node-id") ?? "")
    );
    await nodeRow(page, upward.at(-1) ?? "")
      .first()
      .focus();
    for (let step = upward.length - 2; step >= 0; step -= 1) {
      const id = upward[step] ?? "";
      await page.keyboard.press("ArrowUp");
      await expect
        .poll(() => focusedNodeId(page), { message: `ArrowUp does not move the focus to ${id}` })
        .toBe(id);
      const below = await heads.boundingBox();
      const top = (await nodeRow(page, id).first().boundingBox())?.y ?? Number.NaN;
      expect(
        top,
        `the focused statement ${id} stands under the view's sticky heads at ${at}`
      ).toBeGreaterThanOrEqual((below?.y ?? 0) + (below?.height ?? 0) - 1);
    }
  });
});

// Row (7)'s column heads stay at the top after a scroll, under the header's band, whose height
// the header publishes on the document's root as `--issue-identity-height`. That header is in
// this branch now, so this row, which skipped until it was, is one of c-rows' own.
// SLOW(>=2s): the whole seeded issue, a load of the Spec view and a scroll, waited on until the
// page redraws.
test("the Spec view's column heads stay under the header's band after a scroll", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const body = issuePage(page).locator("[data-spec-body]");
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await openPreset(page, seeded.key, "spec");
    await expect
      .poll(() => body.locator("[data-spec-legend] [data-spec-legend-mark]").count(), {
        message: "the Spec view draws no legend at 1440 px",
      })
      .toBe(4);
    await expectIssuePageDrawn(page);
    const band = await page.evaluate(() =>
      getComputedStyle(document.documentElement).getPropertyValue("--issue-identity-height").trim()
    );
    expect(band, "the header publishes no --issue-identity-height").not.toBe("");
    // The page is scrolled once its statements are drawn, so it is tall enough to scroll.
    await expect
      .poll(() => body.locator("[data-spec-statement]").count(), {
        message: "the Spec view draws no statement at 1440 px",
      })
      .toBeGreaterThan(0);
    await page.evaluate(() => window.scrollBy(0, 800));
    await nextFrames(page);
    const top = (await body.locator("[data-spec-head]").boundingBox())?.y ?? Number.NaN;
    expect(
      Math.abs(top - Number.parseFloat(band)),
      `after a scroll of 800 px the heads' top is not --issue-identity-height (${band})`
    ).toBeLessThanOrEqual(1);
  });
});

// c-reflow, on words the seeded issue does not carry. R17 binds the page a reader opens, not one
// seed's text: a requirement whose words name a file path holds a run of 108 characters with no
// space in it, which is longer than the card that draws it at every width this reads. Found by
// the tester on 2026-11-09 at 39649c0f269d, where the Spec preset's requirement list draws that
// run in a span that neither wraps it nor gives it its own line, and the whole page scrolls
// sideways: 884 px of page in a 390 px viewport, and 3,644 px in a 1,440 px one for a 300
// character run.
// SLOW(>=2s): three loads of the Spec view, one at each width, each measured once its statement
// is drawn.
test("a requirement whose words hold an unbroken run keeps the page inside its viewport @phone", async ({
  browser,
}) => {
  await phase(browser, seedUnbrokenWords, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const unbroken = issuePage(page).locator(
      `[data-spec-statement][data-node-id="${blockNodeId(specSlug, seeded.requirements[0] ?? "")}"]`
    );
    for (const width of [320, 390, 1440]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, presetPath(seeded.key, "spec"));
      await expect
        .poll(() => unbroken.count(), {
          message: `the Spec view draws no statement at ${width} px`,
        })
        .toBe(1);
      await expectIssuePageDrawn(page);
      // The words are on the page before it is measured, so a fit read before the list draws
      // cannot pass for the fit of a page that holds them.
      await expect(
        page.getByText(UNBROKEN_PATH, { exact: false }).first(),
        `the Spec view draws none of the requirement's words at ${width} px`
      ).toBeVisible();
      await assertPageFits(page);
    }
  });
});

// R7 and R9: the Spec view opened at its own address while neither read its statements come
// from can be read - the tree read, which holds the spec's blocks and each statement, with each
// view of it, and the coverage, which names each statement - draws the statements' line saying so
// and no row, and that line's Retry takes the focus: nothing else holds it on a fresh load, Tab
// from the tree never reaches the line, and the stage the page draws the view in gives a focus
// left on nothing to a failed read's Retry where the view draws no row (`slots/stage.tsx`'s
// `ViewStage`). With the tree read answered the view draws a row for each statement it holds,
// and the stage gives the focus to the tab-stop row instead.
// SLOW(>=2s): a seeded issue and a load of the Spec view with two reads refused, waited on until
// the refused reads' line is drawn.
test("the Spec view opened at its address with its statements' reads refused puts the focus on their Retry", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const refused = new Set([
      `/api/v1/issues/${seeded.key}/tree`,
      `/api/v1/issues/${seeded.key}/coverage`,
    ]);
    // Every read of the tree, a view of it included, and of the coverage is refused, from the
    // page's first load on.
    await page.route(
      (url) => refused.has(url.pathname),
      (route) =>
        route.fulfill({ json: { code: "INTERNAL", error: "read unavailable" }, status: 500 })
    );
    await gotoIssue(page, presetPath(seeded.key, "spec"));
    const line = issuePage(page)
      .locator("[data-failed-read]")
      .filter({ hasText: STATE_LINES.statements_unreadable });
    await expect(
      line,
      "the Spec view draws no line saying its statements could not be loaded"
    ).toContainText(STATE_LINES.statements_unreadable);
    await expect(
      line.getByRole("button", { name: CONTROL_LABELS.retry }),
      "the statements' Retry does not hold the focus after the page's load"
    ).toBeFocused();
  });
});
