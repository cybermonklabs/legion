/** the Spec view reads the server's stored decisions, ties and results */
import type { Locator } from "@playwright/test";

import { presetPath } from "../web/src/features/issue-page/issue-address";
import { firstLineOf } from "../web/src/features/issue-page/issue-tree";
import {
  CONTROL_LABELS,
  DECLARED_WORDS,
  EMPTY_STATE_LINES,
  FILTER_SIGNAL_WORDS,
  GAP_LINES,
  GLYPH_TOKENS,
  NODE_STATE_WORDS,
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
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createMessage,
  editArtifact,
  getArtifact,
  getAsk,
  getIssueCoverage,
  getIssueDecisions,
  listComments,
  listIssueAsks,
} from "./api";
import { expectTimeout } from "./expect-timeout";
import { nextFrames } from "./frames";
import {
  seedNoChildren,
  seedNoSummary,
  seedUnbrokenWords,
  seedWholeIssue,
  UNBROKEN_PATH,
} from "./issue-layout-seed";
import { asPlanner, PROJECT } from "./issue-layout-seed-support";
import {
  CITATION_BLOCK_ID,
  OPEN_QUESTION_BLOCK_ID,
  requirementBlockId,
  requirementText,
  SUMMARY_SENTENCES,
  specBlockRef,
} from "./issue-layout-seed-text";
import {
  contentsPane,
  gotoIssue,
  issuePage,
  outlineControl,
  outlineSheet,
  phase,
  presetCount,
  presetToggle,
} from "./issue-page";
import {
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
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
  caretInRowBlock,
  expectFallbackLine,
  FALLBACK_LINES,
  focusedNodeId,
  GONE_COMMENT,
  gapKinds,
  idsOf,
  onScreen,
  openPreset,
  rowGaps,
} from "./issue-page-i";
import {
  becauseLines,
  briefRow,
  checksDrawn,
  contentsLineFaults,
  decidedByCells,
  drawnContentsEntries,
  expectOnScreen,
  focusBranchControl,
  ownWords,
  rowAuthor,
  scrollAtRest,
  seedChildWithSpec,
  seedTallSpec,
  specRuns,
  statementBlock,
  threeThings,
  wordAtCaret,
} from "./issue-page-j";
import { withIssuePage } from "./issue-page-option";
import { IPHONE_13 } from "./issue-page-p";
import { TYPEFACE, typefaceAddress, typefaceStatuses } from "./issue-page-typeface";
import { verificationCheck } from "./issue-page-v";
import { expect, test } from "./test";
import { type ApiRequests, expectLoadSettled, watchApiRequests } from "./traffic";
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

// c-three-things, as the architect reads R1 and R4: the three sentences stand each after its
// label, and under them the two rows, "How it gets done" and "What you will be shown", each a
// control that turns its filter on; with no view on they are in the Contents pane at 1440 px and
// in the Outline sheet at 390 px, never above the outline; under Spec they are the view's first
// group, above its first statement; under Verification, Plan and Decisions they are not drawn; no
// Contents pane is drawn under a view. A spec with no summary draws the two rows alone, and an
// issue with no check draws the second row's one line. Partial on today's data: who wrote the
// sentences and when they were last read back wait on a read that holds them, so the head draws
// neither and this test never asserts an author.
// SLOW(>=2s): two seeded issues and twelve loads of the page, each read for its head and the
// page's own draw.
test("the three sentences stand with their labels in the Contents pane, the Spec filter and the Outline sheet @phone", async ({
  browser,
}) => {
  // Twelve loads of the page, each read for its head and the page's own draw.
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
  /** The head `head` draws: the spec's own three sentences, each after its label, with no link
   *  in it. The sentences are the summary node's words, which the batched reference read names
   *  last of the page's reads, so the reading waits for them. */
  const expectTheThree = async (head: Locator, where: string): Promise<void> => {
    await expect(head, `the three sentences are not drawn ${where}`).toHaveCount(1);
    await expect
      .poll(() => spoken(head), {
        message: `the spec's own three sentences are not drawn ${where}`,
      })
      .toEqual([...SUMMARY_SENTENCES]);
    expect(await labelled(head), `the three sentences carry no labels ${where}`).toEqual(labels);
    expect(await head.getByRole("link").count(), `the head holds a link ${where}`).toBe(0);
  };
  // The seed's six checks, by the state each stands in, in the order the page declares the
  // states: one failed, one with no result, one out of date, two passed (the one whose result
  // records no negative control is a pass like any other) and one the page cannot tell
  // (`seedWholeIssue`).
  const seededStates = [
    ["failed", 1],
    ["no-result", 1],
    ["older", 1],
    ["passed", 2],
    ["unmarked", 1],
  ] as const;

  await phase(browser, seedWholeIssue, async (page, seeded) => {
    /** The two rows under the sentences of `head`: each a button named by its label alone, the
     *  first drawing the Plan control's count and noun (`plan`), the second the number of the
     *  seed's checks and each state that holds a check, as its shape, count and word, adding up
     *  to the seed's checks. */
    const expectTheRows = async (head: Locator, where: string, plan: string): Promise<void> => {
      const checks = seeded.checks.all.length;
      await expect(
        head.locator("[data-brief-row]"),
        `the two rows are not drawn ${where}`
      ).toHaveCount(2);
      await expect(
        briefRow(head, "plan"),
        `the first row is not named "${CONTROL_LABELS.in_short_how}" ${where}`
      ).toHaveAccessibleName(CONTROL_LABELS.in_short_how);
      await expect(
        briefRow(head, "verification"),
        `the second row is not named "${CONTROL_LABELS.in_short_shown}" ${where}`
      ).toHaveAccessibleName(CONTROL_LABELS.in_short_shown);
      await expect(
        briefRow(head, "plan").locator("[data-brief-counts]"),
        `"${CONTROL_LABELS.in_short_how}" does not draw the Plan control's count and noun ${where}`
      ).toHaveText(`${plan} ${FILTER_SIGNAL_WORDS.steps}`);
      await expect(
        briefRow(head, "verification").locator("[data-brief-counts]"),
        `"${CONTROL_LABELS.in_short_shown}" does not draw the number of the seed's checks ${where}`
      ).toContainText(`${checks} ${VERIFICATION_WORDS.check_many}`);
      const drawn = await checksDrawn(briefRow(head, "verification"));
      expect(drawn.total, `the number of checks and its noun ${where}`).toBe(
        `${checks} ${VERIFICATION_WORDS.check_many}`
      );
      expect(
        drawn.states,
        `the second row does not draw each state that holds a check, with its count, word and shape ${where}`
      ).toEqual(
        seededStates.map(([state, count]) => ({
          count: String(count),
          shape: GLYPH_TOKENS.states.check[state],
          state,
          word: NODE_STATE_WORDS.check[state],
        }))
      );
      expect(
        drawn.states.reduce((sum, state) => sum + Number(state.count), 0),
        `the counts of the states do not add up to the seed's checks ${where}`
      ).toBe(checks);
    };

    // 1440 px, no view: in the Contents pane, at the left of the outline. The sentences are read
    // before the outline is waited for, so a slot that draws none fails on its own reading.
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    const pane = contentsPane(page).getByTestId("issue-three-things");
    await expectTheThree(pane, "in the Contents pane at 1440 px");
    // The rows count what the filter row counts: the Plan control's count, and the Verification
    // control's, which is the number of the seed's checks.
    await expect(presetCount(page, "plan"), "the Plan control draws no count").toHaveText(/^\d+$/);
    const plan = (await presetCount(page, "plan").innerText()).trim();
    await expect(
      presetCount(page, "verification"),
      "the Verification control does not count the seed's checks"
    ).toHaveText(String(seeded.checks.all.length));
    await expectTheRows(pane, "in the Contents pane at 1440 px", plan);
    await expectIssuePageDrawn(page);
    await expect(contentsPane(page), "no Contents pane is drawn with no view on").toHaveCount(1);
    expect(await threeThings(page).count(), "the three sentences are drawn twice").toBe(1);
    const paneBox = await contentsPane(page).boundingBox();
    const outline = await tree(page).boundingBox();
    expect(paneBox, "the Contents pane has no box").not.toBeNull();
    expect(outline, "the outline has no box").not.toBeNull();
    expect(
      (paneBox?.x ?? 0) + (paneBox?.width ?? 0),
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

    // A press on each row turns its filter on: Plan, then, from the page with no view on again,
    // Verification.
    await briefRow(contentsPane(page), "plan").click();
    await expect(
      presetToggle(page, "plan"),
      `a press on "${CONTROL_LABELS.in_short_how}" did not turn the Plan filter on`
    ).toHaveAttribute("aria-pressed", "true");
    expect(new URL(page.url()).pathname, "the Plan filter's address").toBe(
      presetPath(seeded.key, "plan")
    );
    await page.goBack();
    await expect(contentsPane(page), "going back did not draw the Contents pane").toHaveCount(1);
    await briefRow(contentsPane(page), "verification").click();
    await expect(
      presetToggle(page, "verification"),
      `a press on "${CONTROL_LABELS.in_short_shown}" did not turn the Verification filter on`
    ).toHaveAttribute("aria-pressed", "true");
    expect(new URL(page.url()).pathname, "the Verification filter's address").toBe(
      presetPath(seeded.key, "verification")
    );

    // Under a view, at both widths: no Contents pane; the Spec view draws the three, and the two
    // rows under them, as its first group, above its first statement; the other three draw none.
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const preset of PRESET_ORDER) {
        if (preset === "conversation") continue;
        const where = `with the ${PRESET_NAMES[preset]} view at ${size.width} px`;
        // A view's body draws rows of its own shape - a table under Verification, a chart under
        // Plan - so the page is waited for until it has stopped reading, which is what says it
        // has drawn everything it is going to. A view whose part has not filled its slot draws
        // no tree, so only the Spec view is read for one.
        const traffic = watchApiRequests(page);
        await gotoIssue(page, presetPath(seeded.key, preset));
        if (preset === "spec") await expectIssuePageDrawn(page);
        await expectLoadSettled(traffic);
        expect(await contentsPane(page).count(), `a Contents pane is drawn ${where}`).toBe(0);
        if (preset !== "spec") {
          expect(await threeThings(page).count(), `the three sentences are drawn ${where}`).toBe(0);
          expect(
            await page.locator("[data-brief-row]").count(),
            `the two rows are drawn ${where}`
          ).toBe(0);
          continue;
        }
        await expectTheThree(threeThings(page), where);
        await expectTheRows(threeThings(page), where, plan);
        const head = await threeThings(page).boundingBox();
        const lastRow = await briefRow(page, "verification").boundingBox();
        const firstRow = await nodeRows(page).first().boundingBox();
        expect(
          head?.y ?? Number.POSITIVE_INFINITY,
          `the three sentences are not the Spec view's first group ${where}`
        ).toBeLessThan(firstRow?.y ?? 0);
        expect(
          (lastRow?.y ?? Number.POSITIVE_INFINITY) + (lastRow?.height ?? 0),
          `the two rows do not stand above the first statement ${where}`
        ).toBeLessThanOrEqual((firstRow?.y ?? 0) + 1);
      }
    }

    // 390 px, no view: not above the outline, and in the sheet the Outline control opens, which
    // holds the three sentences, the two rows and the Contents list.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(() => nodeRows(page).count(), { message: "no row is drawn at 390 px" })
      .toBeGreaterThan(0);
    expect(await contentsPane(page).count(), "a Contents pane is drawn at 390 px").toBe(0);
    expect(
      await threeThings(page).count(),
      "the three sentences are drawn above the outline at 390 px"
    ).toBe(0);
    await outlineControl(page).click();
    const sheet = outlineSheet(page).getByTestId("issue-three-things");
    await expectTheThree(sheet, "in the Outline sheet at 390 px");
    await expectTheRows(sheet, "in the Outline sheet at 390 px", plan);
    expect(
      await drawnContentsEntries(outlineSheet(page)).count(),
      "the Outline sheet holds no Contents list"
    ).toBeGreaterThan(0);
  });

  // A spec with no summary draws the two rows alone, and an issue with no check draws the second
  // row's one line: this issue holds no plan task either, which is the first row's.
  await phase(browser, seedNoSummary, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    // The rows wait on every read the tree is built from, which the spec's blocks are one of:
    // until they answer a row draws its label alone, so the reading waits for the lines.
    await expect(
      briefRow(threeThings(page), "verification").locator("[data-brief-counts]"),
      "an issue with no check does not draw the page's line for it under the second row"
    ).toHaveText(EMPTY_STATE_LINES.verification);
    await expect(
      briefRow(threeThings(page), "plan").locator("[data-brief-counts]"),
      "a plan with no task does not draw the page's line for it under the first row"
    ).toHaveText(EMPTY_STATE_LINES.plan);
    await expect(
      threeThings(page).locator("[data-brief-row]"),
      "the head does not draw the two rows"
    ).toHaveCount(2);
    expect(
      await threeThings(page).locator("li").count(),
      "a spec with no summary draws sentences all the same"
    ).toBe(0);
    await expect(
      threeThings(page).locator("ul, p"),
      "a spec with no summary draws something other than the two rows"
    ).toHaveCount(0);
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
    const words = new Map<string, string>([
      ...(await listComments(seeded.key)).map((one) => [one.id, one.body] as const),
      ...asks.map((one) => [one.id, one.question] as const),
    ]);
    // The open question is the spec's `ask` block, so its entry is the block's node, whose words
    // the server indexed into the ask.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const openQuestion = blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID);
    const indexed = asks.find((one) => one.id === seeded.openQuestion.askId)?.question;
    expect(indexed, "the seed's open question is none of the issue's asks").toBeDefined();
    words.set(openQuestion, indexed ?? "");
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    const entries = drawnContentsEntries(contentsPane(page));
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
      .poll(() => drawnContentsEntries(phone ? outlineSheet(page) : contentsPane(page)).count(), {
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

// Enter or a press on a Contents entry goes to its node in the outline and marks the entry.
// Focus branch draws that branch alone, then the whole outline again. Below 1024 px choosing an
// entry in the Outline sheet closes it. The reader's message is drawn in the outline but has no
// entry: no node was placed under it. Spec has a count, but Conversation does not.
// SLOW(>=2s): the whole seeded issue, two loads of the page, and six moves of the focus and the
// toggle, each waited on until the outline redraws.
test("a Contents entry goes to its node, and Focus branch draws that branch alone @phone", async ({
  browser,
}) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const question = blockNodeId(specSlug, seeded.openQuestion.blockId);
    // The API call uses the same login as `phase`.
    const message = await createMessage(seeded.key, { body: "I have read the spec." });
    const statements = (await getIssueCoverage(seeded.key)).requirements ?? [];

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
    await expect(
      nodeRow(page, message.id),
      "the reader's message is not in the outline"
    ).toHaveCount(1);
    await expect(
      presetCount(page, "spec"),
      "Spec does not show the number of statements"
    ).toHaveText(String(statements.length));
    await expect(presetCount(page, "conversation"), "Conversation drew a count").toHaveCount(0);
    await expect(
      contentsPane(page).locator(`[data-contents-entry="${message.id}"]`),
      "the reader's message with no node under it is an entry of the Contents list"
    ).toHaveCount(0);
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

/** The first sentence of the question row (5) asks of its own, which runs past the 100
 *  characters a one-line place cuts a node's first line at (R1), so a cell or a line that cut it
 *  as one would end it in an ellipsis. */
const LONG_QUESTION_FIRST =
  "Which of the old page's addresses - its tabs, its anchors and each query string a reader saved in a bookmark - must the new page still answer?";
/** What the question says after its first sentence, which neither the cell nor the line draws. */
const LONG_QUESTION_REST = "A link that stops answering loses its reader.";
/** The person's answer to that question, one sentence, so the quote is the whole of it. */
const LONG_ANSWER = "Each of them, the anchors as well.";
/** The heading of the person's section the row adds to the decisions document. */
const LONG_DECISION = "Answer every address a reader saved";
/** The words the row gives statement 7 once its decision ties it. */
const REWRITTEN_SEVENTH = "Requirement 7 of the redesign stands on a row of its own, as it did.";

// c-home. The Spec view's body: the decision beside the statements it shaped, in the words it
// came from, and one line on each statement's own row (R5), drawn with the focus elsewhere and
// the pointer off the page. The line of a decision that answers a question gives, after
// "because:", that question's first sentence, then the quote. The tester's F7 is read here too -
// the line is a link to the decision, and `w` moves the focus to it - and F20: no text of the
// view is a raw address or a fence. Then the row asks a question of its own whose first sentence
// runs past 100 characters, answers it, and writes a version of the decisions document whose
// person's section cites that answer and ties statement 7: the cell beside it and its line each
// draw that first sentence to its end, with no ellipsis. Once the row rewrites statement 7 after
// that tie, its line says "Changed since", and no other statement's does. Partial on today's
// data: a statement carries no `from` yet, so no statement hangs under an answered ask and the
// body draws one group with no heading; a criterion, an agent decision's level of uncertainty and
// its reason, and the versions of a view's query wait on reads of their own, so this row reads
// the live Spec view.
// SLOW(>=2s): the whole seeded issue, its decisions and answer read back, the pointer and the
// focus moved across the view, and two more loads after a question, a decision and a statement
// written, each waited on until it redraws.
test("a spec node shows the decisions that shaped it at every version", async ({ browser }) => {
  test.setTimeout(210_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const decisionNode = (index: number) =>
      blockNodeId(decisionsSlug, seeded.decisions[index] ?? "");
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
    const opening = (agent?.text ?? "").trimStart().split(/\n\s*\n/u)[0] ?? "";
    const agentQuote = firstLineOf(/^#{1,6} +(.*)/u.exec(opening)?.[1] ?? opening).line;
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
    // statement a second decision shaped names it after the first. The person's decision answers
    // a question, so its line gives that question's first sentence after "because:", then the
    // quote: the seeded question is one sentence, so its first sentence is the whole of it.
    const question = answered.ask.question;
    expect(question, "the seeded question is not one sentence").toMatch(/^[^.!?]+[.!?]$/u);
    // The lines are read again until every one of them holds what follows: a line is drawn from
    // the tree as well as from the decisions and asks reads, and a quote or a question names a
    // reference it cites by the names read, which the page sends last. `toPass` keeps no expect
    // timeout of its own, so it is given the one every other assertion of the row waits under.
    await expect(async () => {
      const lines = await becauseLines(page);
      for (const index of [3, 4, 5]) {
        expect(
          lines[statement(index)],
          `statement ${index} carries no line for its decision`
        ).toContain(`${SPEC_WORDS.because} ${question} “${quote}”`);
        expect(
          lines[statement(index)]?.startsWith(`${SPEC_WORDS.because} ${question} `),
          `statement ${index}'s line does not start with the question its decision answers: "${lines[statement(index)]}"`
        ).toBe(true);
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
    }).toPass({ timeout: expectTimeout });
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

    // A question of the row's own, whose first sentence runs past the 100 characters a one-line
    // place cuts at, answered by the reader, and a new version of the decisions document whose
    // person's section cites that answer and ties statement 7, which no decision shaped before:
    // the cell beside statement 7 and its line each draw that first sentence to its end.
    const asked = await createAsk(
      seeded.key,
      { question: `${LONG_QUESTION_FIRST} ${LONG_QUESTION_REST}`, urgency: "high" },
      asPlanner
    );
    await answerAsk(asked.id, { expected_edited_at: null, selected: [], text: LONG_ANSWER });
    await editArtifact(seeded.decisionsArtifactId, {
      ops: [
        {
          after: "end",
          markdown:
            `## ${LONG_DECISION}\n\nDecided in dispatch://${seeded.key}/ask/${asked.id}. ` +
            `Shapes ${specBlockRef(seeded.key, requirementBlockId(7))}.`,
          op: "insert",
        },
      ],
    });
    /** The tie of the row's own decision to statement 7, as today's decisions read holds it. */
    const seventhTie = async () => {
      const decisions = (await getIssueDecisions(seeded.key)).decisions ?? [];
      const made = decisions.find((one) => one.heading === LONG_DECISION);
      return made?.links?.find((link) => link.to.block_id === requirementBlockId(7));
    };
    await expect
      .poll(async () => (await seventhTie()) !== undefined, {
        message: "the decisions read holds no tie of the row's own decision to statement 7",
      })
      .toBe(true);
    await openPreset(page, seeded.key, "spec");
    const seventh = nodeRow(page, statement(7)).first();
    const seventhCell = issuePage(page).locator(
      `[data-spec-cell][data-spec-statements~="${statement(7)}"]`
    );
    await expect(
      seventhCell,
      "the row's own decision is not drawn as the reader's beside statement 7"
    ).toHaveAttribute("data-said", "you");
    await expect(
      seventhCell.locator("[data-spec-question]"),
      "the cell beside statement 7 does not draw its question's first sentence whole"
    ).toHaveText(LONG_QUESTION_FIRST);
    await expect(
      seventh.locator("[data-spec-because] [data-spec-question]"),
      "statement 7's line does not draw its question's first sentence whole"
    ).toHaveText(LONG_QUESTION_FIRST);
    const seventhLine = (await becauseLines(page))[statement(7)] ?? "";
    expect(
      seventhLine.startsWith(`${SPEC_WORDS.because} ${LONG_QUESTION_FIRST} “${LONG_ANSWER}”`),
      `statement 7's line does not start with its question's first sentence, then the answer: "${seventhLine}"`
    ).toBe(true);
    const questionsDrawn = [
      seventhLine,
      ...(await seventhCell.locator("[data-spec-question]").allTextContents()),
    ];
    expect(
      questionsDrawn.filter((text) => text.includes("…")),
      "the question's first sentence is cut with an ellipsis beside statement 7"
    ).toEqual([]);
    /** The statements whose line says "Changed since", in the order they are drawn. */
    const changedSince = () =>
      issuePage(page)
        .locator("[data-spec-statement] [data-spec-because] [data-spec-changed-since]")
        .evaluateAll((marks) =>
          marks.map(
            (mark) => mark.closest("[data-spec-statement]")?.getAttribute("data-node-id") ?? ""
          )
        );
    // The cell above drew the decisions read's answer, which holds every tie and when each
    // statement last changed, so one reading now is what that answer says.
    expect(
      await changedSince(),
      "a statement says Changed since though none changed after its tie"
    ).toEqual([]);

    // Statement 7 rewritten after its tie: its line says "Changed since", and no other line does.
    await editArtifact(seeded.specArtifactId, {
      ops: [{ find: requirementText(7), op: "replace", with: REWRITTEN_SEVENTH }],
    });
    await expect
      .poll(
        async () => {
          const link = await seventhTie();
          const changed = link?.to.changed_at ?? null;
          return (
            link !== undefined &&
            changed !== null &&
            Date.parse(changed) > Date.parse(link.created_at)
          );
        },
        { message: "the decisions read does not hold statement 7 as changed after its tie" }
      )
      .toBe(true);
    await openPreset(page, seeded.key, "spec");
    await expect(
      rowWords(seventh),
      "statement 7 is not drawn in the words the row rewrote it in"
    ).toContainText(REWRITTEN_SEVENTH);
    await expect
      .poll(changedSince, {
        message: "the statement rewritten after its tie, and it alone, does not say Changed since",
      })
      .toEqual([statement(7)]);
    await expect(
      seventh.locator("[data-spec-changed-since]"),
      "statement 7's line does not say Changed since in the page's words"
    ).toHaveText(SPEC_WORDS.changed_since);
  });
});

/** The words a press on a statement's words types there, which a reload reads back. */
const PRESSED_WORD = "Pressword";
/** The block id of the check the row writes with an empty `serves`. */
const LOOSE_CHECK_ID = "c-serves-nothing";

// c-presets' and c-chain's Spec halves (R4, R6, R9, R10, R17). Under Spec the rows are the spec's
// statements alone, in the order the coverage read gives them, and each hangs under the issue, so
// no row draws a parent line and no group a heading; the view's count is their number. A
// decision's chip, followed from the outline, opens the view on the statement it names. "No
// check" is a statement's mark and the legend's word, and never a gap beside the mark too. Enter
// on a statement, and a press on its words, put the caret in its words, in the editor staged in
// its row, and what is typed there reads back after a reload. A statement a check serves carries
// the number of its checks, as the coverage read counts them, at the right of its first line, a
// link that a press, or `f` and Enter, follows to the Verification view with the focus on that
// statement's row; a statement no check serves carries none. A check the row writes with no
// statement it serves stands under the issue in the Verification view with the gap that says
// so, and no seeded check carries a gap. A child's Spec view draws its own statements and none of
// its parent's: the tree reads the issue's own coverage alone (reading (e) of the plan).
// SLOW(>=2s): two seeded issues, the whole one and a parent with its child, eight loads of the
// page or more, two followed links, two statements edited and a check written, each waited on
// until the page redraws.
test("the Spec view draws statements alone, each parent once, and no check once", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const statement = (index: number) => blockNodeId(specSlug, requirementBlockId(index));
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const decision = blockNodeId(decisionsSlug, seeded.decisions[0] ?? "");
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

    // With no view on, `f` steps to the decision's first chip and Enter follows it. The page's
    // reads are watched from that load on, so the keys below wait until they have settled.
    const followed = watchApiRequests(page);
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

    // Enter on a statement puts the caret in its words, in the spec's editor staged in its row in
    // place of them, and Escape gives the row back. The landing the followed address makes once
    // the page's reads settle would take the focus from a row a script focused before them.
    await expectLoadSettled(followed);
    const third = nodeRow(page, statement(3)).first();
    await third.focus();
    await expect(third).toBeFocused();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInRowBlock(page, requirementBlockId(3)), {
        message: "Enter on a statement does not put the caret in its words",
      })
      .toBe(true);
    expect(
      await focusedNodeId(page),
      "Enter on a statement puts the caret in another row than its own"
    ).toBe(statement(3));
    await page.keyboard.press("Escape");
    await expect
      .poll(() => caretInNode(page), {
        message: "Escape does not take the caret out of the statement's words",
      })
      .toBe(false);
    await expect(third, "Escape does not give the statement's row the focus back").toBeFocused();

    // A press on a statement's words puts the caret in them too, and what is typed there is
    // stored: the statement's words drawn after a reload hold it. The press lands on the first
    // word, clear of a link or a chip, which a press follows instead.
    const eighth = nodeRow(page, statement(8)).first();
    await rowWords(eighth).click({ position: { x: 2, y: 12 } });
    await expect
      .poll(() => caretInRowBlock(page, requirementBlockId(8)), {
        message: "a press on a statement's words does not put the caret in them",
      })
      .toBe(true);
    expect(
      await focusedNodeId(page),
      "a press on a statement's words puts the caret in another row than its own"
    ).toBe(statement(8));
    await page.keyboard.type(`${PRESSED_WORD} `);
    await page.keyboard.press("Escape");
    // The coverage read holds the statement's stored words, so the page is reloaded once they
    // hold the typed word, and draws them from that load.
    await expect
      .poll(
        async () =>
          (await getIssueCoverage(seeded.key)).requirements?.find(
            (one) => one.block_id === requirementBlockId(8)
          )?.text ?? "",
        { message: "the words typed in a statement are not stored", timeout: 30_000 }
      )
      .toContain(PRESSED_WORD);
    const reloaded = watchApiRequests(page);
    await page.reload();
    await expect(
      nodeRow(page, statement(8)),
      "the Spec view draws no statement 8 after a reload"
    ).toHaveCount(1);
    await expect(
      rowWords(nodeRow(page, statement(8)).first()),
      "the words typed in a statement do not read back after a reload"
    ).toContainText(PRESSED_WORD);
    await expectLoadSettled(reloaded);

    // Each statement a check serves carries the number of its checks, as the coverage read
    // counts them, at the right end of its first line; a statement no check serves carries none.
    await expect
      .poll(
        () =>
          body
            .locator("[data-spec-statement]")
            .evaluateAll((rows) =>
              Object.fromEntries(
                rows.map((row) => [
                  row.getAttribute("data-node-id") ?? "",
                  (row.querySelector("[data-spec-checks]")?.textContent ?? "")
                    .replace(/\s+/g, " ")
                    .trim(),
                ])
              )
            ),
        {
          message:
            "the statements do not each carry the number of the checks that serve them, or none",
        }
      )
      .toEqual(
        Object.fromEntries(
          coverage.map((one) => [
            blockNodeId(specSlug, one.block_id),
            one.checks === 0
              ? ""
              : `${one.checks} ${one.checks === 1 ? VERIFICATION_WORDS.check_one : VERIFICATION_WORDS.check_many}`,
          ])
        )
      );
    const misplaced = await body.locator("[data-spec-statement]").evaluateAll((rows) =>
      rows.flatMap((row) => {
        const count = row.querySelector("[data-spec-checks]")?.getBoundingClientRect();
        const words = row.querySelector("[data-node-words]")?.getBoundingClientRect();
        if (count === undefined || words === undefined) return [];
        const placed = Math.abs(count.top - words.top) <= 1 && count.left >= words.right - 1;
        return placed
          ? []
          : [
              `${row.getAttribute("data-node-id")}: its count stands at ${count.left},${count.top}, its words from ${words.left},${words.top} to ${words.right}`,
            ];
      })
    );
    expect(
      misplaced,
      "a statement's count of checks does not stand at the right of its first line"
    ).toEqual([]);

    // A press on the count, and `f` and Enter on it, open the Verification view with the focus
    // on the statement's row.
    const counted = coverage.filter((one) => one.checks > 0);
    expect(
      counted.length,
      "the seed holds fewer than two statements a check serves"
    ).toBeGreaterThan(1);
    const followsCount = async (blockId: string, how: string): Promise<void> => {
      await expect(
        page,
        `${how} on a statement's count of checks does not open the Verification view at its block`
      ).toHaveURL(new RegExp(`${presetPath(seeded.key, "verification")}#b-${blockId}$`));
      await expect(
        presetToggle(page, "verification"),
        `${how} on a statement's count of checks does not turn Verification on`
      ).toHaveAttribute("aria-pressed", "true");
      await expect
        .poll(() => focusedNodeId(page), {
          message: `${how} on a statement's count of checks does not put the focus on its row of the Verification view`,
        })
        .toBe(blockNodeId(specSlug, blockId));
    };
    const pressed = counted[0]?.block_id ?? "";
    await nodeRow(page, blockNodeId(specSlug, pressed))
      .first()
      .locator("[data-spec-checks]")
      .click();
    await followsCount(pressed, "a press");

    const keyed = counted[1]?.block_id ?? "";
    const backToSpec = watchApiRequests(page);
    await openPreset(page, seeded.key, "spec");
    const keyedRow = nodeRow(page, blockNodeId(specSlug, keyed)).first();
    const keyedCount = keyedRow.locator("[data-spec-checks]");
    await expect(keyedCount, "the Spec view drawn again draws no count of checks").toHaveCount(1);
    await expectLoadSettled(backToSpec);
    await keyedRow.focus();
    await expect(keyedRow).toBeFocused();
    // `f` steps through the row's links and controls in turn, so it is pressed until it reaches
    // the count, at most once for each of them.
    const stops = await keyedRow.locator("a[href], button").count();
    for (let step = 0; step < stops; step += 1) {
      await page.keyboard.press("f");
      // The keymap moves the focus inside its keydown listener (`useTreeKeymap.ts`'s
      // `tree-link`), which has run once the press resolves, so one read is where `f` left it.
      if (await keyedCount.evaluate((count) => count === document.activeElement)) break;
    }
    await expect(keyedCount, "f does not reach a statement's count of checks").toBeFocused();
    await page.keyboard.press("Enter");
    await followsCount(keyed, "f and Enter");

    // Every seeded check serves a statement, so the row writes one whose `serves` is empty, in a
    // new version of the Verification document. With Verification on it stands under the issue
    // with the gap that says it serves nothing, and no seeded check carries a gap.
    const verificationSlug = (await getArtifact(seeded.verificationArtifactId)).slug;
    await editArtifact(seeded.verificationArtifactId, {
      ops: [
        {
          after: "end",
          markdown: `:::check{#${LOOSE_CHECK_ID} surface="cli" run="bun run e2e"}\nThis check names nothing it proves.\n:::`,
          op: "insert",
        },
      ],
    });
    await openPreset(page, seeded.key, "verification");
    const loose = verificationCheck(page, blockNodeId(verificationSlug, LOOSE_CHECK_ID));
    await expect(
      loose,
      "the check that serves no statement does not stand under the issue in the Verification view"
    ).toHaveAttribute("data-verification-loose", "");
    await expect
      .poll(
        () =>
          loose
            .locator("[data-gap-kind]")
            .evaluateAll((gaps) => gaps.map((gap) => gap.getAttribute("data-gap-kind"))),
        { message: "the check that serves no statement carries no gap that says so" }
      )
      .toEqual(["serves_nothing"]);
    await expect(
      loose.locator("[data-gap-kind]"),
      "the gap of the check that serves no statement does not say so in the page's words"
    ).toHaveText(GAP_LINES.serves_nothing);
    for (const check of seeded.checks.all) {
      const drawn = verificationCheck(page, blockNodeId(verificationSlug, check));
      await expect(
        drawn.first(),
        `the seeded check ${check} is not drawn in the Verification view`
      ).toBeAttached();
      expect(
        await drawn.locator("[data-gap-kind]").count(),
        `the seeded check ${check} carries a gap`
      ).toBe(0);
    }
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

/** The block id of the statement of 200 words the picture's row writes into the seeded spec,
 *  after a reference to one of the issue's children. */
const LONG_BLOCK_ID = "r-long";

// R4, R5 and R6 against the approved picture `page-v3-spec-1440.png`. At 1440 px, from
// the top: the legend's four marks, each a shape and its word; the column heads; the three
// sentences; the first group. The page's own words never say "Requirement" and are each a word
// of `page-words.ts` (R7, reading (g) of the plan). A cell is 280 px wide and 20 px left of its
// statements. A statement of 200 words is drawn whole, every word of it, with no "more" control,
// while the focus is elsewhere; a statement's chips stand after its last word, and a link in its
// words is the words' colour, read on the long statement, whose words open on a child's
// reference: an issue's reference is a link in a row's words, where a block's or a document's is
// a chip in place (`RowText.tsx`). `t s` from the outline leaves the focus on a statement of the
// view, on screen, and once the page's reads have settled each move up the statements leaves the
// one it reaches below the view's sticky heads. Where the sticky heads stand after a scroll,
// against the header's `--issue-identity-height`, is the next row's (reading (l)). At 390 px, as
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
    const three = body.getByTestId("issue-three-things");
    await expect(three, `the Spec view draws no three sentences at ${at}`).toHaveCount(1);
    const topOf = async (part: Locator): Promise<number> =>
      (await part.boundingBox())?.y ?? Number.NaN;
    const order = [
      { name: "the legend", top: Math.max(...legend.map((mark) => mark.top)) },
      { name: "the column heads", top: await topOf(heads) },
      { name: "the three sentences", top: await topOf(three) },
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

    // The statement of 200 words is drawn whole, every word of it, with no "more" control, while
    // the focus is on no part of it: its words' box clips none of them and runs past six lines.
    const long = nodeRow(page, longId).first();
    expect(
      await long.evaluate((row) => row.contains(document.activeElement)),
      `the long statement holds the focus at ${at}, so its words are not read with the focus elsewhere`
    ).toBe(false);
    await expect(rowMore(long), `the long statement draws a more control at ${at}`).toHaveCount(0);
    const whole = await rowWords(long).evaluate((words) => ({
      clipped: words.scrollHeight - words.clientHeight,
      height: words.getBoundingClientRect().height,
      words: (words as HTMLElement).innerText.split(/\s+/u),
    }));
    expect(
      longWords("long", 200)
        .split(/\s+/u)
        .filter((word) => word !== "" && !whole.words.includes(word)),
      `the long statement at ${at} draws none of these of its 200 words`
    ).toEqual([]);
    expect(
      whole.clipped,
      `the long statement's words at ${at} hide ${whole.clipped} px of themselves`
    ).toBeLessThanOrEqual(1);
    expect(
      whole.height,
      `the long statement is ${whole.height} px tall at ${at}, cut at six lines`
    ).toBeGreaterThan(6 * 24);

    // From the outline, `t s` leaves the focus on a statement of the view's tree, on screen. The
    // page's reads are watched from this load on, for the moves up the statements below.
    const reads = watchApiRequests(page);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
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
    // stood with its top at the screen's edge and its first line under the 31 px of heads. The
    // moves start once the page's reads have settled: a read that answered as the first ArrowUp
    // went down left the focus on the first statement, not on the one above the last.
    await expectLoadSettled(reads);
    const upward = await statements.evaluateAll((rows) =>
      rows.map((one) => one.getAttribute("data-node-id") ?? "")
    );
    const last = nodeRow(page, upward.at(-1) ?? "").first();
    await last.focus();
    await expect(last, `the last statement does not take the focus at ${at}`).toBeFocused();
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
// character run. The Spec view is read at the four widths R4 names, from 320 px to 1920 px: 320,
// 390, 1440 and 1920 px.
// SLOW(>=2s): four loads of the Spec view, one at each width, each measured once its statement
// is drawn.
test("a requirement whose words hold an unbroken run keeps the page inside its viewport @phone", async ({
  browser,
}) => {
  await phase(browser, seedUnbrokenWords, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const unbroken = issuePage(page).locator(
      `[data-spec-statement][data-node-id="${blockNodeId(specSlug, seeded.requirements[0] ?? "")}"]`
    );
    for (const width of [320, 390, 1440, 1920]) {
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
// from can be read - the spec's blocks and the coverage, which names each statement - draws the
// statements' line saying so and no row, and that line's Retry takes the focus: nothing else
// holds it on a fresh load, Tab from the tree never reaches the line, and the stage the page
// draws the view in gives a focus left on nothing to a failed read's Retry where the view draws
// no row (`slots/stage.tsx`'s `ViewStage`). With the coverage answered the view draws a row for
// each statement it names, and the stage gives the focus to the tab-stop row instead.
// SLOW(>=2s): a seeded issue and a load of the Spec view with two reads refused, waited on until
// the refused reads' line is drawn.
test("the Spec view opened at its address with its statements' reads refused puts the focus on their Retry", async ({
  browser,
}) => {
  await phase(browser, seedNoChildren, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const refused = new Set([
      `/api/v1/artifacts/${seeded.specArtifactId}/blocks`,
      `/api/v1/issues/${seeded.key}/coverage`,
    ]);
    // Every read of the spec's blocks and of the coverage is refused, from the page's first load
    // on.
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

// An address on the Spec view, loaded whole from another page as a reader who follows a link to
// it loads it, lands on the row it names with that row inside the viewport once the page has
// gone quiet: a statement, a comment the view draws, and a comment the issue holds none of. Each
// case is a full load of Settings, then a full load of the address, never a press or a move
// inside the app, so the view draws every row from an empty cache. The seeded spec's sixty
// statements stand well past one screen, so a row drawn after them can stand below it.
//
// A `#b-` fragment names the spec's own block, so it is the view's own address and not one
// beside it (`issue-address.ts`'s `namesSpecBlock`): the view lands on the statement itself and
// the stage takes no call. The last statement is read, which is not the tab stop and stands below
// the first screen.
//
// A comment of the issue is an address beside the view that the view draws a row for, after its
// statements. The statements come from the coverage read (`tree-drafts.ts`'s `addRequirements`),
// and from the spec's blocks read where it answers first, so both reads are held until the
// comment's row is drawn and then let go together: the statements are drawn above a row that
// already stands, as on a large issue whose coverage answers after its comments. While that row
// is the only one drawn the stage may put the focus on it; the view lands on it again once its
// reads have settled, which brings it on screen. A view that lands on the comment's row before
// the statements are drawn, and does not hold it on screen, leaves it below the screen once they
// are drawn above it, and fails this case's check of the viewport. A page that draws no Spec view
// fails its first check.
//
// A key during that load: with both reads held the comment's row, drawn alone, holds the focus,
// and the reader presses End on it, which leaves the focus on that row, the view's last. The
// landing then does nothing, since the reader has acted, and the row the key left the reader on
// stays on screen once the statements are drawn above it. A view that holds nothing after a key
// leaves the row where the statements pushed it, below the screen, since at the page's top the
// browser's scroll anchoring holds the header still and not the row, and fails this case's check
// of the viewport.
//
// A comment id of the right shape that no comment of the issue has draws the outline's line for
// it in the view's place, with the focus on that line's heading, which stands in view.
// SLOW(>=2s): a seeded issue of sixty statements, then four loads of Settings and four of the
// Spec view, each waited on until the page goes quiet.
test("an address on the Spec view lands on a statement, on a comment it draws and on a comment the issue holds none of, each loaded from another page with the focused row in view", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedTallSpec, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const spec = presetPath(seeded.key, "spec");
    const body = issuePage(page).locator("[data-spec-body]");
    const statements = body.locator("[data-spec-statement]");
    /** A full load of Settings, waited on until it is drawn, then a full load of `address`,
     *  watched from its start so the page going quiet can be waited on. */
    const fromSettings = async (address: string): Promise<ApiRequests> => {
      await page.goto("/settings");
      await expect(
        page.getByRole("heading", { level: 1, name: "Settings" }),
        `Settings is not drawn before ${address} is loaded`
      ).toBeVisible();
      const traffic = watchApiRequests(page);
      await gotoIssue(page, address);
      return traffic;
    };

    // The last statement, by its fragment.
    const last = seeded.requirements.at(-1) ?? "";
    const statement = body.locator(
      `[data-spec-statement][data-node-id="${blockNodeId(seeded.specSlug, last)}"]`
    );
    await expectLoadSettled(await fromSettings(`${spec}#b-${last}`));
    await expect(
      statements,
      "the last statement's fragment does not draw every statement of the spec"
    ).toHaveCount(seeded.requirements.length);
    await expect(
      statement,
      "the last statement's fragment leaves the focus off its row of the Spec view"
    ).toBeFocused();
    await expectOnScreen(statement, "the last statement's fragment");

    // A comment of the issue, with the coverage read and the spec's blocks read held until the
    // comment's row is drawn.
    const comment = body.locator(`[data-node-row][data-node-id="${seeded.commentId}"]`);
    const heldPaths = new Set([
      `/api/v1/artifacts/${seeded.specArtifactId}/blocks`,
      `/api/v1/issues/${seeded.key}/coverage`,
    ]);
    const heldRead = (url: URL): boolean => heldPaths.has(url.pathname);
    const held = Promise.withResolvers<void>();
    await page.route(heldRead, async (route) => {
      await held.promise;
      await route.continue();
    });
    const traffic = await fromSettings(`${spec}?comment=${seeded.commentId}`);
    await expect(
      comment,
      "the comment's address draws no row of the Spec view for it while the coverage and the spec's blocks are held"
    ).toBeVisible();
    held.resolve();
    await expectLoadSettled(traffic);
    await expect(
      statements,
      "the comment's address does not draw every statement of the spec once the coverage and the spec's blocks answer"
    ).toHaveCount(seeded.requirements.length);
    await expect(
      comment,
      "the comment's address leaves the focus off its row of the Spec view once the statements are drawn above it"
    ).toBeFocused();
    await expectOnScreen(comment, "the comment's address, its statements drawn above it");
    await page.unroute(heldRead);

    // A key during that load: End on the comment's row, with the coverage read and the spec's
    // blocks read held until the key is pressed.
    const keyHeld = Promise.withResolvers<void>();
    await page.route(heldRead, async (route) => {
      await keyHeld.promise;
      await route.continue();
    });
    const keyTraffic = await fromSettings(`${spec}?comment=${seeded.commentId}`);
    await expect(
      comment,
      "the comment's address draws no row of the Spec view for it while the coverage and the spec's blocks are held, before End"
    ).toBeVisible();
    await expect(
      comment,
      "the comment's row does not hold the focus while the coverage and the spec's blocks are held, before End"
    ).toBeFocused();
    await page.keyboard.press("End");
    keyHeld.resolve();
    await expectLoadSettled(keyTraffic);
    await expect(
      statements,
      "the comment's address, End pressed on its row during the load, does not draw every statement of the spec once the coverage and the spec's blocks answer"
    ).toHaveCount(seeded.requirements.length);
    await expectOnScreen(
      comment,
      "End on the comment's row during the load, its statements drawn above it"
    );
    await expect(
      comment,
      "End on the comment's row during the load leaves the focus off that row once the statements are drawn above it"
    ).toBeFocused();
    await page.unroute(heldRead);

    // A comment the issue holds none of.
    expect(
      (await listComments(seeded.key)).map((one) => one.id),
      `a comment of the seeded issue has the id ${GONE_COMMENT}`
    ).not.toContain(GONE_COMMENT);
    const fallback = FALLBACK_LINES.comment(seeded.key);
    await expectLoadSettled(await fromSettings(`${spec}?comment=${GONE_COMMENT}`));
    const heading = await expectFallbackLine(page, fallback, body, "a gone comment under Spec");
    await expectOnScreen(heading, "a gone comment under Spec");
  });
});

// The tester's finding at 1f2cdd53: a key that only scrolls the page keeps the reader's place. A
// key the Spec view's tree does not bind - Page Down, Space, Page Up - scrolls the page and leaves
// the focus on the statement it was pressed on, so it takes the reader nowhere, and nothing may
// move the page back to that statement after it. At 1f2cdd53 the view holds the focused item on
// screen after every key (`holdFocusedItem`), and the hold's first look brings the statement back
// as soon as the key has scrolled it away: at 1440 x 900 on a large issue's copy, Page Down moved
// the page 290 px where the outline moves it 732 px, Space moved it up 485 px, and Page Up moved
// it down 497 px. At b3934ff2, which holds nothing after a key, each key moved the page a screen
// its own way.
// SLOW(>=2s): a seeded issue of sixty statements and a long one, a load of the Spec view, and
// three keys, each read once the page has stopped moving.
test("a key that only scrolls the page keeps the reader's place in the Spec view: Page Down and Space scroll down, Page Up up", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seed = async () => {
    const seeded = await seedTallSpec();
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(3)}`,
          markdown: statementBlock(LONG_BLOCK_ID, longWords("long", 200)),
          op: "insert",
        },
      ],
    });
    return seeded;
  };
  await phase(browser, seed, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const body = issuePage(page).locator("[data-spec-body]");
    const statements = body.locator("[data-spec-statement]");
    const traffic = watchApiRequests(page);
    await openPreset(page, seeded.key, "spec");
    await expectLoadSettled(traffic);
    await expect(
      statements,
      "the Spec view does not draw every statement of the spec and the long one"
    ).toHaveCount(seeded.requirements.length + 1);
    const long = body.locator(
      `[data-spec-statement][data-node-id="${blockNodeId(seeded.specSlug, LONG_BLOCK_ID)}"]`
    );
    // A script's focus, which starts no hold of the reader's: the keys below are the reader's.
    await long.focus();
    await expect(long, "the long statement does not take the focus").toBeFocused();
    /** The page's scroll once it has stopped moving: two frames apart, two reads that agree. */
    const restingScroll = (): Promise<number> =>
      page.evaluate(async () => {
        let last = window.scrollY;
        for (;;) {
          const frames = Promise.withResolvers<void>();
          requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
          await frames.promise;
          if (window.scrollY === last) return Math.round(last);
          last = window.scrollY;
        }
      });
    const screen = DESKTOP.height;
    for (const [key, down] of [
      ["PageDown", true],
      ["Space", true],
      ["PageUp", false],
    ] as const) {
      const before = await restingScroll();
      await page.keyboard.press(key);
      const moved = (await restingScroll()) - before;
      expect(
        down ? moved : -moved,
        `${key} on the focused statement moves the page ${moved} px, not half a screen (${screen / 2} px) or more ${down ? "down" : "up"}`
      ).toBeGreaterThanOrEqual(screen / 2);
      await expect(
        long,
        `${key} moves the focus off the statement it was pressed on`
      ).toBeFocused();
    }
  });
});

// The tester's finding at cac836d5: a press on a statement's words puts the caret at the press
// and keeps the reader's place. The press opens the statement's editor with the caret where the
// reader pressed, as a press on any text a reader may edit does, so the page has nothing to bring
// on screen and does not move. At cac836d5 the press puts the caret at the start of the statement's
// words, offset 0 of its first text, whatever word was pressed, and where the statement's top
// stands above the screen the page jumps up to that start: at 1440 x 900 on a large issue's copy,
// 471 px for a statement whose top stood 400 px above the screen and 191 px for one 120 px above
// it, and 459 px and 179 px at 390 x 844 by touch.
// SLOW(>=2s): a seeded issue of sixty statements and one of 600 words, a load of the Spec view, a
// turn of the wheel and a press, each read once the page has stopped moving.
test("a press on a Spec statement's words puts the caret at the press and keeps the reader's place", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seed = async () => {
    const seeded = await seedTallSpec();
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(3)}`,
          markdown: statementBlock(LONG_BLOCK_ID, longWords("long", 600)),
          op: "insert",
        },
      ],
    });
    return seeded;
  };
  await phase(browser, seed, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const body = issuePage(page).locator("[data-spec-body]");
    const statements = body.locator("[data-spec-statement]");
    const traffic = watchApiRequests(page);
    await openPreset(page, seeded.key, "spec");
    await expectLoadSettled(traffic);
    await expect(
      statements,
      "the Spec view does not draw every statement of the spec and the long one"
    ).toHaveCount(seeded.requirements.length + 1);
    const long = body.locator(
      `[data-spec-statement][data-node-id="${blockNodeId(seeded.specSlug, LONG_BLOCK_ID)}"]`
    );
    // A turn of the wheel, the reader's own act, stands the long statement's top 200 px above the
    // screen, with the rest of its 600 words on screen under the column heads.
    const heads = body.locator("[data-spec-head]");
    const headsBottom = await heads.evaluate((head) => head.getBoundingClientRect().bottom);
    const top = await long.evaluate((row) => row.getBoundingClientRect().top);
    const wheelAt = await scrollAtRest(page);
    await page.mouse.move(DESKTOP.width / 2, DESKTOP.height / 2);
    await page.mouse.wheel(0, top + 200);
    await expect
      .poll(() => scrollAtRest(page), { message: "the wheel does not scroll the page" })
      .not.toBe(wheelAt);
    const box = await long.evaluate((row) => {
      const { bottom, left, top: rowTop, width } = row.getBoundingClientRect();
      return { bottom, left, top: rowTop, width };
    });
    expect(
      box.top,
      "after the wheel the long statement's top is not above the screen"
    ).toBeLessThan(0);
    expect(
      box.bottom - headsBottom,
      "after the wheel the long statement shows under 100 px of its words below the column heads"
    ).toBeGreaterThanOrEqual(100);
    // The press is on a word of the statement's words, clear of a link or a chip, 60 px under the
    // column heads: the word the browser would put the caret in there is read before the press.
    const at = { x: box.left + box.width / 3, y: headsBottom + 60 };
    expect(
      await long.evaluate(
        (row, point) => {
          const hit = document.elementFromPoint(point.x, point.y);
          return row.contains(hit) && hit?.closest("a, button") === null;
        },
        { x: at.x, y: at.y }
      ),
      `the point (${at.x}, ${at.y}) is not on the long statement's words`
    ).toBe(true);
    const pressed = await wordAtCaret(page, at);
    expect(pressed, "the press's point holds no word of the long statement").toMatch(/^long\d+/);
    const pressedAt = await scrollAtRest(page);
    await page.mouse.click(at.x, at.y);
    await expect
      .poll(() => caretInRowBlock(page, LONG_BLOCK_ID), {
        message: "a press on the long statement's words does not put the caret in them",
      })
      .toBe(true);
    const moved = (await scrollAtRest(page)) - pressedAt;
    expect(
      await wordAtCaret(page, "selection"),
      `a press on the word "${pressed}" puts the caret in another word`
    ).toBe(pressed);
    expect(
      Math.abs(moved),
      `a press on the long statement's words, its top above the screen, moves the page ${moved} px`
    ).toBeLessThanOrEqual(1);
  });
});

// The tester's finding at cac836d5: a row the Spec view keeps that is no statement - a comment of
// the issue its address names - draws its text whole, as the outline draws that comment and as
// every node's text is drawn in every view. At cac836d5 the row is whole while it holds the focus
// and is cut at six lines behind "more" once the focus leaves it: on a large issue's copy a comment
// of 240 words stood 632 px tall with the focus and 195 px, behind "more · 183 words", after
// ArrowUp to the statement above it, where the outline draws it whole either way.
// SLOW(>=2s): a seeded issue of sixty statements and a comment of 240 words, a load of Settings
// and of the comment's address under Spec, and a key.
test("a comment the Spec view's address keeps is drawn whole with the focus on it and after it leaves", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seed = async () => {
    const seeded = await seedTallSpec();
    const long = await createComment(seeded.key, { body: longWords("said", 240) });
    return { ...seeded, longCommentId: long.id };
  };
  await phase(browser, seed, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const body = issuePage(page).locator("[data-spec-body]");
    await page.goto("/settings");
    await expect(
      page.getByRole("heading", { level: 1, name: "Settings" }),
      "Settings is not drawn before the comment's address is loaded"
    ).toBeVisible();
    const traffic = watchApiRequests(page);
    await gotoIssue(page, `${presetPath(seeded.key, "spec")}?comment=${seeded.longCommentId}`);
    await expectLoadSettled(traffic);
    const comment = body.locator(`[data-node-row][data-node-id="${seeded.longCommentId}"]`);
    await expect(
      comment,
      "the comment's address under Spec leaves the focus off the comment's row of the Spec view"
    ).toBeFocused();
    await expect(
      rowMore(comment),
      "the comment's row cuts its words behind more while it holds the focus"
    ).toHaveCount(0);
    // ArrowUp takes the focus to the row above the comment's, a statement of the view.
    await page.keyboard.press("ArrowUp");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "ArrowUp from the comment's row leaves the focus on the comment",
      })
      .not.toBe(seeded.longCommentId);
    await expect(
      rowMore(comment),
      "the comment's row cuts its words behind more once the focus has left it"
    ).toHaveCount(0);
    expect(
      await rowWords(comment).evaluate((words) => words.scrollHeight - words.clientHeight),
      "the comment's words are clipped once the focus has left its row"
    ).toBeLessThanOrEqual(1);
  });
});

// The tester's finding at 130a685b: a press on a statement's words after a reference still puts
// the caret in the word pressed. The Spec view draws a reference to another issue by that issue's
// key and title, while the statement's editor holds the reference as it is written, so a count of
// the drawn characters before the press is not the editor's count. At 130a685b the press is
// placed by that count, and every press after such a reference lands in another word: on a large
// issue's copy, "with" put the caret in "which", "adds" in "reading" and "checks" in "included:",
// at 1440 x 900 and at 390 x 844 alike, while a press before any reference lands in its word.
// SLOW(>=2s): a seeded issue of sixty statements, a second issue to cite, a load of the Spec view,
// a turn of the wheel and a press, each read once the page has stopped moving.
test("a press on a Spec statement's words after a reference puts the caret in the word pressed", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seed = async () => {
    const seeded = await seedTallSpec();
    const cited = await createIssue({
      force: true,
      project: PROJECT,
      title: "The issue this statement cites by its key, drawn by its key and its title",
    });
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(3)}`,
          markdown: statementBlock(
            LONG_BLOCK_ID,
            `Read dispatch://${cited.key} first. ${longWords("long", 120)}`
          ),
          op: "insert",
        },
      ],
    });
    return seeded;
  };
  await phase(browser, seed, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const body = issuePage(page).locator("[data-spec-body]");
    const traffic = watchApiRequests(page);
    await openPreset(page, seeded.key, "spec");
    await expectLoadSettled(traffic);
    const long = body.locator(
      `[data-spec-statement][data-node-id="${blockNodeId(seeded.specSlug, LONG_BLOCK_ID)}"]`
    );
    await expect(long, "the Spec view draws no statement that cites the other issue").toHaveCount(
      1
    );
    await expect(
      long.locator("[data-node-words] a"),
      "the statement draws no link for the reference it holds"
    ).not.toHaveCount(0);
    /** The middle of the drawn word `word` of the statement's words, in the viewport. */
    const wordBox = (word: string): Promise<{ x: number; y: number } | null> =>
      long.evaluate((row, wanted) => {
        const words = row.querySelector("[data-node-words]");
        if (words === null) return null;
        const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
        for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
          const found = ` ${text.textContent ?? ""} `.indexOf(` ${wanted} `);
          if (found < 0) continue;
          const range = document.createRange();
          range.setStart(text, found + 1);
          range.setEnd(text, found + 2);
          const box = range.getBoundingClientRect();
          return { x: box.left + 1, y: (box.top + box.bottom) / 2 };
        }
        return null;
      }, word);
    // A turn of the wheel, the reader's own act, brings the word "long60" to the screen's middle.
    const pressed = "long60";
    const far = await wordBox(pressed);
    expect(far, `the statement draws no word "${pressed}"`).not.toBeNull();
    const wheelAt = await scrollAtRest(page);
    await page.mouse.move(DESKTOP.width / 2, DESKTOP.height / 2);
    await page.mouse.wheel(0, (far?.y ?? 0) - DESKTOP.height / 2);
    await expect
      .poll(() => scrollAtRest(page), { message: "the wheel does not scroll the page" })
      .not.toBe(wheelAt);
    // The poll above waited until the page stopped moving after the wheel, so one read of the
    // word's box is where it stands when it is pressed.
    const at = await wordBox(pressed);
    expect(at, `the word "${pressed}" is not on the page after the wheel`).not.toBeNull();
    const point = at ?? { x: 0, y: 0 };
    expect(
      await wordAtCaret(page, point),
      `the point (${point.x}, ${point.y}) is not on the word "${pressed}"`
    ).toBe(pressed);
    await page.mouse.click(point.x, point.y);
    await expect
      .poll(() => caretInRowBlock(page, LONG_BLOCK_ID), {
        message:
          "a press on the statement's words after its reference does not put the caret in them",
      })
      .toBe(true);
    expect(
      await wordAtCaret(page, "selection"),
      `a press on the word "${pressed}", after the statement's reference, puts the caret in another word`
    ).toBe(pressed);
  });
});

// The tester's finding at 8b8986a7: a press on a statement's words after inline HTML puts the
// caret in the word pressed. The Spec view draws a statement's inline HTML, such as a placeholder
// `<name>`, as its characters, while the statement's editor holds it as an inline `html` node that
// the caret cannot enter, so a count of the drawn characters before the press is not the editor's
// count. At 8b8986a7 the press counts it as drawn, and a press after it lands that many characters
// on: on a large issue's copy, a statement that writes "Kept by <name>" twice put the caret in
// "before" for a press on "merged", and in "that" for one on "make".
// SLOW(>=2s): a seeded issue of sixty statements, a load of the Spec view, a turn of the wheel and
// a press, each read once the page has stopped moving.
test("a press on a Spec statement's words after inline HTML puts the caret in the word pressed", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seed = async () => {
    const seeded = await seedTallSpec();
    await editArtifact(seeded.specArtifactId, {
      ops: [
        {
          after: `block:${requirementBlockId(3)}`,
          markdown: statementBlock(
            LONG_BLOCK_ID,
            `It draws "Kept by <name>" and "Replaced by <name>". ${longWords("long", 120)}`
          ),
          op: "insert",
        },
      ],
    });
    return seeded;
  };
  await phase(browser, seed, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    const body = issuePage(page).locator("[data-spec-body]");
    const traffic = watchApiRequests(page);
    await openPreset(page, seeded.key, "spec");
    await expectLoadSettled(traffic);
    const long = body.locator(
      `[data-spec-statement][data-node-id="${blockNodeId(seeded.specSlug, LONG_BLOCK_ID)}"]`
    );
    await expect(long, "the Spec view draws no statement that holds inline HTML").toHaveCount(1);
    await expect(
      long.locator('[data-node-words] [data-type="html"]'),
      "the statement draws no inline HTML for the placeholders it holds"
    ).toHaveCount(2);
    /** The middle of the drawn word `word` of the statement's words, in the viewport. */
    const wordBox = (word: string): Promise<{ x: number; y: number } | null> =>
      long.evaluate((row, wanted) => {
        const words = row.querySelector("[data-node-words]");
        if (words === null) return null;
        const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
        for (let text = walker.nextNode(); text !== null; text = walker.nextNode()) {
          const found = ` ${text.textContent ?? ""} `.indexOf(` ${wanted} `);
          if (found < 0) continue;
          const range = document.createRange();
          range.setStart(text, found + 1);
          range.setEnd(text, found + 2);
          const box = range.getBoundingClientRect();
          return { x: box.left + 1, y: (box.top + box.bottom) / 2 };
        }
        return null;
      }, word);
    // A turn of the wheel, the reader's own act, brings the word "long60" to the screen's middle.
    const pressed = "long60";
    const far = await wordBox(pressed);
    expect(far, `the statement draws no word "${pressed}"`).not.toBeNull();
    const wheelAt = await scrollAtRest(page);
    await page.mouse.move(DESKTOP.width / 2, DESKTOP.height / 2);
    await page.mouse.wheel(0, (far?.y ?? 0) - DESKTOP.height / 2);
    await expect
      .poll(() => scrollAtRest(page), { message: "the wheel does not scroll the page" })
      .not.toBe(wheelAt);
    // The poll above waited until the page stopped moving after the wheel, so one read of the
    // word's box is where it stands when it is pressed.
    const at = await wordBox(pressed);
    expect(at, `the word "${pressed}" is not on the page after the wheel`).not.toBeNull();
    const point = at ?? { x: 0, y: 0 };
    expect(
      await wordAtCaret(page, point),
      `the point (${point.x}, ${point.y}) is not on the word "${pressed}"`
    ).toBe(pressed);
    await page.mouse.click(point.x, point.y);
    await expect
      .poll(() => caretInRowBlock(page, LONG_BLOCK_ID), {
        message:
          "a press on the statement's words after its inline HTML does not put the caret in them",
      })
      .toBe(true);
    expect(
      await wordAtCaret(page, "selection"),
      `a press on the word "${pressed}", after the statement's inline HTML, puts the caret in another word`
    ).toBe(pressed);
  });
});
