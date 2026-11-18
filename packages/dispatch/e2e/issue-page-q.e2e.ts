import type { Page } from "@playwright/test";

import { IDENTITY_HEIGHT_PROPERTY } from "../web/src/features/issue-page/identity-block";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  CONTROL_LABELS,
  DECISIONS_WORDS,
  NODE_STATE_WORDS,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { getArtifact, getAsk, getIssue, getIssueDecisions, listComments } from "./api";
import { seedDecisionsFilter, seedReviewList } from "./issue-layout-seed";
import { OPEN_QUESTION_BLOCK_ID } from "./issue-layout-seed-text";
import { gotoIssue, phase } from "./issue-page";
import {
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
  expectRowGroups,
  expectRowLayouts,
  focusRow,
  nodeRow,
  PHONE,
  parentOfRow,
  rowBody,
  rowGroups,
  rowLayouts,
  rowOf,
  unfoldEveryBranch,
} from "./issue-page-g";
import { focusedNodeId, focusInside, onScreen, openPreset } from "./issue-page-i";
import {
  computedBorderWidth,
  controlLook,
  decisionStatementNames,
  decisionsBody,
  decisionsFoot,
  decisionsPart,
  decisionsPartRows,
  decisionsPartsDrawn,
  decisionsRows,
  decisionsWordsWithSource,
  reviewFlag,
  seedDecisionsLayout,
} from "./issue-page-q";
import { expect, test } from "./test";

/**
 * The browser rows of the new issue page that read one of its views:
 * the Decisions view - the open questions, the decisions that wait for the reader's review with
 * Keep and Change, the reader's own decisions, and the agents' other decisions folded at the
 * foot - read on the real stack. Each row turns the new page on for its reader through D's
 * `phase`, seeds what it reads with the seeds of `e2e/issue-layout-seed.ts` or the Decisions
 * view's own `e2e/issue-page-q.ts`, and reads the page through D's `e2e/issue-page.ts`, G's
 * `e2e/issue-page-g.ts`, I's `e2e/issue-page-i.ts` and the view's own `e2e/issue-page-q.ts`.
 *
 * Six rows, each under the title of the check it answers:
 *
 * - **c-groups**, three rows that come from the checkpoint's `e2e/issue-layout.e2e.ts`: "the
 *   Decisions filter draws its four parts and quotes the person's answer"; "only a decision
 *   that changed a statement waits for review", whose words draw no source (the tester's F20);
 *   and "a change sent on a decision is drawn on it, and the decision leaves the review list"
 *   (the tester's F6), read again at 390 px and from an address that names the change.
 * - **c-keep**, two rows: "the Decisions view draws Keep and Change on each decision that
 *   waits, and Keep moves it": the two controls before any focus and their colours, Keep,
 *   `Shift+C`, `Shift+K`, where a load and, at 1440 px, `t d` leave the focus (the tester's
 *   F23), and a press on the link of a row that has no focus; and its 390 px `t d` reading, a
 *   row of its own that reads part P's header band first (decisions 80, 127, 135 and 141).
 * - **c-layout**, "under Decisions a row draws the outline's layout, short names and the
 *   six-line cut": G's layout and group readings on every row of the view, each statement named
 *   within 32 characters, the cut of a long answered question, and the view's width.
 *
 * Each row's first reading waits out the page's load under a message of its own, before G's
 * `expectIssuePageDrawn`: on a build whose slot draws nothing, every row fails on what the view
 * does not draw rather than on the page's frame (plan t6 and t11).
 */

/** The decisions' violet (`--color-page-decision`) and the page's paper (`--color-page-paper`),
 *  as computed: Keep is the violet with words of paper, Change paper with violet words and a
 *  violet border (the prototype's `.dbtn`). */
const VIOLET = "rgb(124, 58, 237)";
const PAPER = "rgb(255, 255, 255)";

// c-decisions. Clause by clause, at 1440 px and at 390 px. What waits on the decisions model
// and is not read here: an agent decision's author (`decided_by`), its level of uncertainty and
// its reason, the reading under an agent's decision, and the `shaped` tuple itself (today's tie
// is a decision block's link to a spec block). What waits on the conversion of issues: the
// unconverted issue, `?view=decisions`, and "the page makes no request to /decisions".
// SLOW(>=2s): the whole issue seeded with three more writes, the view loaded and read at two
// widths with a focus and the foot's unfolding waited on each time, then the outline loaded.
test("the Decisions filter draws its four parts and quotes the person's answer", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedDecisionsFilter, async (page, seeded) => {
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    const D = (index: number) => blockNodeId(decisionsSlug, seeded.decisions[index] ?? "");
    const open = blockNodeId(specSlug, OPEN_QUESTION_BLOCK_ID);
    const stored = (await getAsk(seeded.optionedAskId)).ask.answer;
    expect(stored?.text ?? "", "the seed's answered ask holds no answer").not.toBe("");
    // A heading is its words and its count; the foot's is its control's, which says whether the
    // rows under it are drawn. White space between the parts of one is the browser's to lay out.
    const squeezed = (text: string) => text.replace(/\s+/gu, "");
    const headings = {
      agents: squeezed(`▸ 3 ${DECISIONS_WORDS.agents_many}`),
      open_questions: squeezed(`${DECISIONS_WORDS.parts.open_questions} 1`),
      review: squeezed(`${DECISIONS_WORDS.parts.review} 2`),
      yours: squeezed(`${DECISIONS_WORDS.parts.yours} 4`),
    };
    const parts = async () =>
      (await decisionsPartsDrawn(page)).map((part) => ({
        ...part,
        heading: squeezed(part.heading),
      }));
    for (const size of [DESKTOP, PHONE]) {
      const at = `at ${size.width} px`;
      await page.setViewportSize(size);
      await openPreset(page, seeded.key, "decisions");
      // Four parts under their own headings, in this order: the open question first, the two
      // decisions waiting for review newest first, the reader's own decisions newest first -
      // the one kept, the question declined, the two answered - and the foot, folded. This is
      // the row's first reading, so a page whose view draws nothing fails here.
      await expect.poll(parts, { message: `the four parts ${at}` }).toEqual([
        { count: "1", heading: headings.open_questions, part: "open_questions", rows: [open] },
        { count: "2", heading: headings.review, part: "review", rows: [D(5), D(0)] },
        {
          count: "4",
          heading: headings.yours,
          part: "yours",
          rows: [D(4), seeded.declinedAskId, seeded.optionedAskId, seeded.answeredAskId],
        },
        { count: "3", heading: headings.agents, part: "agents", rows: [] },
      ]);
      await expectIssuePageDrawn(page);
      // No other sentence stands over them or between them.
      expect(
        await page.evaluate(() => {
          const body = document.querySelector("[data-decisions-body]")?.cloneNode(true);
          if (!(body instanceof Element)) return null;
          for (const row of body.querySelectorAll("[data-node-row]")) row.remove();
          return {
            paragraphs: body.querySelectorAll("p").length,
            text: (body.textContent ?? "").replace(/\s+/gu, ""),
          };
        }),
        `words outside the rows ${at}`
      ).toEqual({
        paragraphs: 0,
        text: `${headings.open_questions}${headings.review}${headings.yours}${headings.agents}`,
      });

      // A declined ask reads as a question the person declined, never as an answer.
      const declined = nodeRow(page, seeded.declinedAskId).first();
      await expect(declined.locator("[data-ask-declined] p").first()).toHaveText(
        `alice ${DECISIONS_WORDS.declined}`
      );
      await expect(declined.locator("[data-declined-note]")).toHaveText(
        "The release owner decides this one."
      );
      expect(rowOf(await drawnRows(page), seeded.declinedAskId)).toMatchObject({
        answerLine: null,
        answerText: null,
        state: "declined",
        stateWord: NODE_STATE_WORDS.ask.declined,
      });

      // The answered ask draws its question and options; with the focus on it, the person's
      // answer character for character as the store holds it, by who answered, with no reading
      // under it: the section of decisions.md that restates it is no row of the filter.
      const asked = nodeRow(page, seeded.optionedAskId).first();
      await asked.focus();
      await expect
        .poll(async () => rowOf(await drawnRows(page), seeded.optionedAskId)?.answerText ?? null, {
          message: `the answered ask does not draw the person's answer as the store holds it ${at}`,
        })
        .toBe(stored?.text ?? "");
      await expect(asked.locator('[data-node-option="chosen"]')).toHaveCount(1);
      await expect(asked.locator('[data-node-option="chosen"]')).toContainText("Rail");
      await expect(asked.locator("[data-node-option]")).toHaveCount(2);
      expect(
        rowOf(await drawnRows(page), seeded.optionedAskId)?.answerAuthor,
        `the answered ask does not name who answered ${at}`
      ).toBe(stored?.user);
      expect(await nodeRow(page, D(1)).count(), `the reading is a row of the filter ${at}`).toBe(0);

      // The three states, as words: waiting for review, kept with the keeper's name, replaced.
      await decisionsFoot(page).click();
      await expect
        .poll(async () => (await decisionsPartRows(page, "agents")).length, {
          message: `the foot did not unfold ${at}`,
        })
        .toBe(3);
      const rows = await drawnRows(page);
      expect(rowOf(rows, D(5))).toMatchObject({
        state: "waiting_review",
        stateWord: NODE_STATE_WORDS.decision.waiting_review,
      });
      expect(rowOf(rows, D(4))).toMatchObject({
        state: "kept",
        stateWord: `${NODE_STATE_WORDS.decision.kept} alice`,
      });
      expect(rowOf(rows, D(6))).toMatchObject({
        state: "replaced",
        stateWord: NODE_STATE_WORDS.decision.replaced,
      });
      expect(rowOf(rows, D(2))?.unsure, `the unsure decision does not say so ${at}`).toBe(
        CONTROL_LABELS.unsure
      );

      // The person's decision that shaped three statements is one entry with one node id - the
      // answer itself - and it reaches each of the three; the agent's reaches its two.
      expect(await nodeRow(page, seeded.answeredAskId).count()).toBe(1);
      await expect(
        nodeRow(page, seeded.answeredAskId).locator("[data-decision-statement] a"),
        `the person's decision does not reach the three statements it shaped ${at}`
      ).toHaveCount(3);
      await expect(nodeRow(page, D(0)).locator("[data-decision-statement] a")).toHaveCount(2);

      // Each entry links to the node that holds it, in the conversation.
      expect(
        await page.evaluate(() =>
          [...document.querySelectorAll("[data-decisions-body] [data-node-row]")]
            .filter((row) => row.querySelectorAll("a[data-entry-link]").length !== 1)
            .map((row) => row.getAttribute("data-node-id"))
        ),
        `entries with no link of their own ${at}`
      ).toEqual([]);
      await expect(
        nodeRow(page, seeded.optionedAskId).locator("a[data-entry-link]")
      ).toHaveAttribute("href", `/issues/${seeded.key}/asks/${seeded.optionedAskId}`);
    }

    // The same three words wherever a decision is drawn: with no filter on too, where the
    // section that restates the person's answer hangs under the ask it answers. The tree is
    // drawn with the issue alone and the decisions and the asks arrive after it, each maybe
    // folded where it lands, so the facts are read again, every branch unfolded first, until
    // the reads are in.
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          const outline = await drawnRows(page);
          return {
            kept: rowOf(outline, D(4))?.stateWord,
            parent: await parentOfRow(page, D(1)),
            replaced: rowOf(outline, D(6))?.stateWord,
            waiting: rowOf(outline, D(5))?.stateWord,
          };
        },
        {
          message:
            "with no filter on, a decision does not read its state, or the decision that answers the ask is not drawn under it",
        }
      )
      .toEqual({
        kept: `${NODE_STATE_WORDS.decision.kept} alice`,
        parent: seeded.answeredAskId,
        replaced: NODE_STATE_WORDS.decision.replaced,
        waiting: NODE_STATE_WORDS.decision.waiting_review,
      });
  });
});

// c-review-list, the rule for what waits on the reader's review. What waits on the decisions
// model: the `shaped` tuple itself; today's tie is a decision block's link to a block of the spec.
// SLOW(>=2s): a seeded issue whose view is loaded and read at two widths with the foot's
// unfolding waited on each time, then the outline loaded with two rows focused.
test("only a decision that changed a statement waits for review @phone", async ({ browser }) => {
  test.setTimeout(150_000);
  // The `iphone` project is the phone half of the pair, so it reads 390 px alone and leaves the
  // outline at the desktop's width to `chromium`: a fixed width every project reads would only
  // repeat that run (`AGENTS.md`, phone acceptance).
  const phone = test.info().project.name === "iphone";
  const sizes = phone ? [PHONE] : [DESKTOP, PHONE];
  await phase(browser, seedReviewList, async (page, seeded) => {
    const id = (block: string) => blockNodeId(seeded.decisionsSlug, block);
    for (const size of sizes) {
      const at = `at ${size.width} px`;
      await page.setViewportSize(size);
      await openPreset(page, seeded.key, "decisions");
      // The review list holds the two a tie names a statement for, newest first. This is the
      // row's first reading, so a page whose view draws nothing fails here.
      await expect
        .poll(() => decisionsPartRows(page, "review"), { message: `the review list ${at}` })
        .toEqual([id(seeded.tiedNewer), id(seeded.tiedOlder)]);
      await expectIssuePageDrawn(page);
      // Each with its Keep and Change controls, with no focus on it.
      for (const block of [seeded.tiedNewer, seeded.tiedOlder]) {
        await expect(
          nodeRow(page, id(block)).locator("button[data-row-action]"),
          `${block} draws no Keep and Change ${at}`
        ).toHaveText([CONTROL_LABELS.keep_decision, CONTROL_LABELS.change_decision]);
      }
      // The Decisions control's flag holds those two and neither other decision, where the
      // controls stand apart (part D's view row, from 1280 px up).
      if (size === DESKTOP) await expect(reviewFlag(page), `the flag ${at}`).toHaveText("2");
      // The other two are in the filter's foot, with no Keep and no Change.
      expect(
        await decisionsPartRows(page, "agents"),
        `the foot draws its rows unasked ${at}`
      ).toEqual([]);
      await decisionsFoot(page).click();
      await expect
        .poll(async () => [...(await decisionsPartRows(page, "agents"))].sort(), {
          message: `the foot's rows ${at}`,
        })
        .toEqual([id(seeded.tiedToNote), id(seeded.untied)].sort());
      for (const block of [seeded.tiedToNote, seeded.untied]) {
        await expect(
          nodeRow(page, id(block)).locator("button[data-row-action]"),
          `${block} draws a Keep or a Change ${at}`
        ).toHaveCount(0);
      }
      // No row's words draw the source they were written in: the seed writes each decision as
      // "Shapes dispatch://<key>/spec#b-…", and the excerpt of the statement it names opens with
      // its `:::requirement{#…}` fence, but a reference is drawn as plain words (the tester's
      // F20).
      await expect
        .poll(() => decisionsWordsWithSource(page), {
          message: `a decision's words draw their source ${at}`,
        })
        .toEqual([]);
    }
    if (phone) return;
    // The other two are drawn in the outline under the issue, with no Keep or Change control,
    // the focus on them included.
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    for (const block of [seeded.tiedToNote, seeded.untied]) {
      const row = await focusRow(page, id(block));
      expect(await parentOfRow(page, id(block)), `${block} hangs under another row`).toBeNull();
      await expect(
        row.locator("button[data-row-action]"),
        `${block} draws a Keep or a Change in the outline`
      ).toHaveCount(0);
    }
    await expect(reviewFlag(page)).toHaveText("2");
  });
});

// The tester's F6, under R7 and R35: a Change sent on a decision is a comment anchored to the
// decision's words. Once it is sent the decision says a change was asked for, leaves the review
// list for the reader's own decisions, takes no Keep and no Change, and draws the change under
// its words with a link to it; the Decisions control's flag drops by one. At 390 px a new load
// draws the decision the same way. An address that names the change opens the view on the
// change's own row, first and in no part, with the focus on it. With no filter on the decision
// says the same and the change hangs under it.
// SLOW(>=2s): a seeded issue whose view is loaded and a Change sent and waited on until the
// server's reads redraw the list, then three more loads: at 390 px, at the change's address,
// and the outline.
test("a change sent on a decision is drawn on it, and the decision leaves the review list", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const id = (block: string) => blockNodeId(seeded.decisionsSlug, block);
    const changed = id(seeded.tiedNewer);
    const words = "Tie it to the first statement instead.";

    await page.setViewportSize(DESKTOP);
    await openPreset(page, seeded.key, "decisions");
    // The row's first reading, so a page whose view draws nothing fails here.
    await expect
      .poll(() => decisionsPartRows(page, "review"), {
        message: "the review list before the Change",
      })
      .toEqual([changed, id(seeded.tiedOlder)]);
    await expectIssuePageDrawn(page);
    await expect(reviewFlag(page)).toHaveText("2");

    const row = nodeRow(page, changed).first();
    await row.locator('button[data-row-action="change"]').click();
    await row.getByLabel(CONTROL_LABELS.your_change).fill(words);
    await row.getByRole("button", { exact: true, name: CONTROL_LABELS.send_change }).click();

    // The decision leaves the review list and stands with the reader's own decisions.
    await expect
      .poll(() => decisionsPartRows(page, "review"), {
        message: "the changed decision still waits for review",
      })
      .toEqual([id(seeded.tiedOlder)]);
    expect(
      await decisionsPartRows(page, "yours"),
      "the changed decision is not among the reader's own"
    ).toEqual([changed]);
    // It says a change was asked for, by a word and a shape, and takes no Keep and no Change.
    expect(rowOf(await drawnRows(page), changed)).toMatchObject({
      state: "change_requested",
      stateWord: NODE_STATE_WORDS.decision.change_requested,
    });
    await expect(row.locator("button[data-row-action]")).toHaveCount(0);
    // The change is drawn under the decision's words, word for word, by who asked, with a link
    // to it in the conversation.
    const comment = (await listComments(seeded.key)).find((one) => one.body === words);
    expect(comment, "the server holds no comment for the change").toBeDefined();
    await expect(row.locator("[data-decision-change] [data-change-words]")).toHaveText(words);
    await expect(row.locator("[data-decision-change] [data-change-by]")).toHaveText("alice");
    await expect(row.locator("[data-decision-change] [data-change-link]")).toHaveAttribute(
      "href",
      `/issues/${seeded.key}/comments/${comment?.id}`
    );
    // The flag moves: one decision fewer waits for the reader's review.
    await expect(reviewFlag(page), "the flag did not move").toHaveText("1");
    const commentId = comment?.id ?? "";

    // At 390 px, from a new load and with no second Change, the decision stands with the
    // reader's own, says so, takes no Keep and no Change, and draws the change under its words.
    // The flag is not read here: the view row draws it from 1280 px up alone.
    await page.setViewportSize(PHONE);
    await openPreset(page, seeded.key, "decisions");
    await expect
      .poll(() => decisionsPartRows(page, "yours"), {
        message: "the changed decision is not among the reader's own at 390 px",
      })
      .toEqual([changed]);
    expect(rowOf(await drawnRows(page), changed), "the changed decision at 390 px").toMatchObject({
      state: "change_requested",
      stateWord: NODE_STATE_WORDS.decision.change_requested,
    });
    await expect(row.locator("button[data-row-action]")).toHaveCount(0);
    await expect(row.locator("[data-decision-change] [data-change-words]")).toHaveText(words);
    await expect(row.locator("[data-decision-change] [data-change-by]")).toHaveText("alice");
    await expect(row.locator("[data-decision-change] [data-change-link]")).toHaveAttribute(
      "href",
      `/issues/${seeded.key}/comments/${commentId}`
    );

    // An address that names the change under the view, which is the view's own address with
    // `?comment=` beside it (`issue-address.ts`): the comment is neither a question nor a
    // decision, so its row stands first in the list, in no part, and takes the focus.
    await page.setViewportSize(DESKTOP);
    await gotoIssue(page, `${presetPath(seeded.key, "decisions")}?comment=${commentId}`);
    await expect
      .poll(async () => (await decisionsRows(page))[0], {
        message: "an address naming the change does not draw its row first, in no part",
      })
      .toMatchObject({ id: commentId, part: null });
    await expect
      .poll(() => focusedNodeId(page), {
        message: "an address naming the change focuses no row of the view",
      })
      .toBe(commentId);

    // With no filter on, the decision says the same and the change hangs under it. The
    // decisions and the comments arrive after the tree is drawn, each maybe folded where it
    // lands, so the facts are read again, every branch unfolded first, until the reads are in.
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          const decision = rowOf(await drawnRows(page), changed);
          return {
            parent: await parentOfRow(page, commentId),
            state: decision?.state,
            stateWord: decision?.stateWord,
          };
        },
        {
          message:
            "with no filter on, the changed decision does not say a change was asked for, or the change is not drawn under it",
        }
      )
      .toEqual({
        parent: changed,
        state: "change_requested",
        stateWord: NODE_STATE_WORDS.decision.change_requested,
      });
    const outlined = await focusRow(page, changed);
    await expect(outlined.locator("button[data-row-action]")).toHaveCount(0);
    await expect(reviewFlag(page)).toHaveText("1");
  });
});

// c-keep (Keep, Change, where they leave the focus, and their keys): Keep and Change stand on
// each decision that waits for the reader's review before any focus, in the decisions' violet,
// and on no other row. Keep moves the decision to the reader's own as kept by her, the server
// holds the Keep, and the focus goes on to the next decision that waits, where `Shift+C` opens
// Change with the caret in its field. A direct load leaves the focus on the list's first row
// (the tester's F23), and a press on the link of a row under it that has no focus follows the
// link; `Shift+K` keeps the last decision that waits and the review part goes; and `t c` then
// `t d` leave the focus on a row of the list, on screen.
// SLOW(>=2s): a seeded issue whose view is loaded three times, with a Keep and a Shift+K each
// waited on until the server's answer redraws the list, a link followed, and two chords each
// waited on until it moves.
test("the Decisions view draws Keep and Change on each decision that waits, and Keep moves it @phone", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  // Each project at its own width: `chromium` the desktop's and `iphone` the phone's. The
  // `iphone` project's own viewport is 390 by 664 and `phase` opens every page at 1440 by 900,
  // so the row sets the size the check names.
  const size = test.info().project.name === "iphone" ? PHONE : DESKTOP;
  const at = `at ${size.width} px`;
  await phase(browser, seedReviewList, async (page, seeded) => {
    const id = (block: string) => blockNodeId(seeded.decisionsSlug, block);
    const newer = id(seeded.tiedNewer);
    const older = id(seeded.tiedOlder);
    const review = decisionsPart(page, "review");

    await page.setViewportSize(size);
    await openPreset(page, seeded.key, "decisions");
    // Keep on both decisions that wait while neither has the focus: the stage puts it on the
    // list's first row, an open question. Both facts in one reading, so it fails on a view that
    // draws nothing and on a view that draws Keep only on the focused row. This is the row's
    // first reading.
    await expect
      .poll(
        async () => ({
          keeps: await review.locator('button[data-row-action="keep"]').count(),
          reviewFocused: await focusInside(review),
        }),
        { message: "a decision of the review part draws no Keep before it has the focus" }
      )
      .toEqual({ keeps: 2, reviewFocused: false });
    await expectIssuePageDrawn(page);

    // Each row of the review part draws Keep and then Change, and no other row draws either.
    const rows = await decisionsRows(page);
    expect(
      rows.filter((row) => row.part === "review"),
      `the review part's Keep and Change ${at}`
    ).toEqual([
      { actions: ["keep", "change"], id: newer, part: "review" },
      { actions: ["keep", "change"], id: older, part: "review" },
    ]);
    expect(
      rows.filter((row) => row.part !== "review" && row.actions.length > 0).map((row) => row.id),
      `a row outside the review part draws Keep or Change ${at}`
    ).toEqual([]);

    // Keep is the violet with words of paper; Change is paper with violet words and a violet
    // border 1.5 px wide, read on each of its four sides. The width is compared with what the
    // browser computes for a border written 1.5px wide, which it rounds down to whole device
    // pixels: 1px at the desktop's device scale of 1, 1.33333px at the phone's 3.
    const border = await computedBorderWidth(page, "1.5px");
    for (const decision of [newer, older]) {
      const row = nodeRow(page, decision).first();
      expect(
        await controlLook(row.locator('button[data-row-action="keep"]')),
        `${decision}'s Keep ${at}`
      ).toMatchObject({ background: VIOLET, color: PAPER });
      expect(
        await controlLook(row.locator('button[data-row-action="change"]')),
        `${decision}'s Change ${at}`
      ).toEqual({
        background: PAPER,
        borderColors: [VIOLET, VIOLET, VIOLET, VIOLET],
        borderWidths: [border, border, border, border],
        color: VIOLET,
      });
    }

    // Keep on the newer decision: it stands with the reader's own, kept by her, the server
    // holds her Keep, and the focus goes on to the older one, the next that waits.
    await nodeRow(page, newer).first().locator('button[data-row-action="keep"]').click();
    await expect
      .poll(() => decisionsPartRows(page, "yours"), {
        message: `Keep did not move the decision to the reader's own ${at}`,
      })
      .toEqual([newer]);
    expect(rowOf(await drawnRows(page), newer), `the kept decision ${at}`).toMatchObject({
      state: "kept",
      stateWord: `${NODE_STATE_WORDS.decision.kept} alice`,
    });
    const kept = ((await getIssueDecisions(seeded.key)).decisions ?? []).find(
      (decision) => decision.block_id === seeded.tiedNewer
    )?.kept;
    expect(kept?.kept_by, "the server holds no Keep by alice of the newer decision").toBe("alice");
    await expect
      .poll(() => focusedNodeId(page), {
        message: `Keep did not leave the focus on the next decision that waits ${at}`,
      })
      .toBe(older);
    // There `Shift+C` opens Change with the caret in its field.
    await page.keyboard.press("Shift+C");
    await expect(
      nodeRow(page, older).first().getByLabel(CONTROL_LABELS.your_change),
      `Shift+C did not put the caret in ${CONTROL_LABELS.your_change} ${at}`
    ).toBeFocused();

    // A direct load of the view leaves the focus on the list's first row (the tester's F23).
    const focusOnFirst = async () => {
      const first = (await decisionsRows(page))[0]?.id ?? null;
      const focused = await focusedNodeId(page);
      return { first, focused, onFirst: first !== null && focused === first };
    };
    await openPreset(page, seeded.key, "decisions");
    await expect
      .poll(focusOnFirst, {
        message: "a direct load of the Decisions view leaves the focus off the list",
      })
      .toMatchObject({ onFirst: true });
    // A press on the link of a row under it, which has no focus, follows the link. The focused
    // row's body is open, so a focus taken before the press ends would close it and move the
    // link up from under the pointer, and the click would be lost.
    const { first } = await focusOnFirst();
    await expect(
      rowBody(nodeRow(page, first ?? "").first()),
      `the focused first row draws no body ${at}`
    ).toHaveCount(1);
    const link = nodeRow(page, newer).first().locator("a[data-entry-link]");
    const address = await link.evaluate((element) => (element as HTMLAnchorElement).href);
    await link.click();
    await expect(page, `a press on a row without the focus is lost ${at}`).toHaveURL(address);
    // The view again, with its focus placed on its first row before a key acts on a row.
    await openPreset(page, seeded.key, "decisions");
    await expect
      .poll(focusOnFirst, {
        message: `the Decisions view loaded again leaves the focus off the list ${at}`,
      })
      .toMatchObject({ onFirst: true });
    // `Shift+K` on the older decision keeps it, and with nothing left to review the part goes.
    await focusRow(page, older);
    await page.keyboard.press("Shift+K");
    await expect
      .poll(() => review.count(), {
        message: `Shift+K did not keep the last decision that waits ${at}`,
      })
      .toBe(0);

    // `t c` turns the view off, and `t d` turns it on again with the focus on a row of its list,
    // on screen. At 390 px that reading stands below part P's header band, which is a row of its
    // own below (decisions 80, 127, 135 and 141).
    if (size === DESKTOP) await expectTdReachesTheList(page, seeded.key, at);
  });
});

// c-keep's reading at 390 px (decisions 80, 127, 135 and 141): after `t c` and `t d` the focus
// is on a row of the list, on screen, below the band that stays at the top of the page. Where
// that band ends is part P's header, which publishes `--issue-identity-height`, and P is under Q,
// so the row reads that the header has published it before the chords. Its title carries
// c-keep's words, so c-keep's run (`-g 'Keep and Change on each decision that waits'`) runs it.
// SLOW(>=2s): a seeded issue whose view is loaded at 390 px, and two chords each waited on until
// the page moves.
test("the Decisions view draws Keep and Change on each decision that waits, and t d at 390 px leaves the focus on a row of its list @phone @phone-only", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const at = `at ${PHONE.width} px`;
    await page.setViewportSize(PHONE);
    await openPreset(page, seeded.key, "decisions");
    await expect
      .poll(() => decisionsBody(page).locator("[data-node-row]").count(), {
        message: `the Decisions view draws no row ${at}`,
      })
      .toBeGreaterThan(0);
    await expectIssuePageDrawn(page);
    await expect
      .poll(
        () =>
          page.evaluate(
            (property) =>
              getComputedStyle(document.documentElement).getPropertyValue(property).trim(),
            IDENTITY_HEIGHT_PROPERTY
          ),
        { message: `the header publishes no ${IDENTITY_HEIGHT_PROPERTY} ${at}` }
      )
      .not.toBe("");
    await expectTdReachesTheList(page, seeded.key, at);
  });
});

/** `t c` turns the Decisions view of `key` off, and `t d` turns it on again with the focus on a
 *  row of its list, on screen (R6). */
async function expectTdReachesTheList(page: Page, key: string, at: string): Promise<void> {
  await page.keyboard.press("t");
  await page.keyboard.press("c");
  await expect(page, `t c did not turn the Decisions view off ${at}`).toHaveURL(
    new RegExp(`${presetPath(key, "conversation")}$`)
  );
  await page.keyboard.press("t");
  await page.keyboard.press("d");
  await expect(page, `t d did not turn the Decisions view on ${at}`).toHaveURL(
    new RegExp(`${presetPath(key, "decisions")}$`)
  );
  await expect
    .poll(
      async () => {
        const focused = await focusedNodeId(page);
        if (focused === null) return { inList: false, onScreen: false };
        const row = nodeRow(page, focused).and(decisionsBody(page).locator("[data-node-row]"));
        return { inList: (await row.count()) === 1, onScreen: await onScreen(row) };
      },
      { message: "t d does not put the focus on a row of the list" }
    )
    .toEqual({ inList: true, onScreen: true });
}

// c-layout (the outline's layout and its six-line cut, kept under this view): the
// checkpoint's c-row walk of the Decisions view. Every row the view draws, the foot's included,
// reads G's layout and groups: one left edge, the kind's tag after the first line, the group's
// place, no "Author unknown" in a group, the colours of a link in a node's words, which the
// seed writes into one question. A statement on an "informs" line is named within 32 characters
// and by no address, which G's group reading, of chips alone, does not see. At 1440 px a long
// question a person answered is cut at six lines and a long one that waits on the reader is
// drawn whole, in a list 960 px wide; at 390 px nothing is wider than the screen.
// SLOW(>=2s): the whole issue seeded with three questions, two of them long, then the view
// loaded and every row read off the page's layout at two widths, with the foot's unfolding and
// the cut waited on.
test("under Decisions a row draws the outline's layout, short names and the six-line cut @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  // The `iphone` project is the phone half of the pair, so it reads 390 px alone: the desktop
  // is `chromium`'s to read, and 1440 px in a phone's context is a layout no reader has.
  const sizes = test.info().project.name === "iphone" ? [PHONE] : [DESKTOP, PHONE];
  await phase(browser, seedDecisionsLayout, async (page, seeded) => {
    const addresses = new RegExp(`dispatch://|${seeded.key} \\S+( block)?$|-md\\b`, "u");
    // The spec's slug, by which `rowLayouts` tells a statement from a block of another document.
    const spec = (await getIssue(seeded.key)).artifacts?.find((one) => one.primary)?.slug ?? "";
    for (const size of sizes) {
      const where = `Decisions at ${size.width} px`;
      await page.setViewportSize(size);
      await openPreset(page, seeded.key, "decisions");
      // The row's first reading, before anything is pressed.
      await expect
        .poll(() => decisionsBody(page).locator("[data-node-row]").count(), {
          message: `${where} draws no row`,
        })
        .toBeGreaterThan(0);
      await expectIssuePageDrawn(page);
      // The seed's agents wrote two decisions that shape no statement and one that was
      // replaced, so the view draws its foot; its rows are rows of the view and read with the
      // rest. The press takes the focus off every row, so no row's body is open below.
      await decisionsFoot(page).click();
      await expect(decisionsFoot(page), `${where} does not unfold its foot`).toHaveAttribute(
        "aria-expanded",
        "true"
      );
      await expect(
        decisionsBody(page).locator("[data-node-body]"),
        `${where} keeps a row's body open after the foot's press`
      ).toHaveCount(0);
      // The two long questions are drawn, and the statements the decisions shaped are named by
      // their own words, which come with the coverage read.
      for (const question of [seeded.answered, seeded.waiting]) {
        await expect(nodeRow(page, question), `${where} draws no row for ${question}`).toHaveCount(
          1
        );
      }
      await expect
        .poll(() => decisionStatementNames(page), { message: `${where} names no statement` })
        .not.toEqual([]);

      expectRowLayouts(await rowLayouts(page, spec), where, false, size.width);
      // The seed's linked question draws its link in its words, so the links' colours below are
      // read on at least one link.
      await expect
        .poll(async () => (await rowGroups(page)).flatMap((row) => row.links).length, {
          message: `${where} draws no link in a row's words`,
        })
        .toBeGreaterThan(0);
      expectRowGroups(await rowGroups(page), where, size.width, addresses);
      const names = await decisionStatementNames(page);
      expect(
        names.filter((name) => name.length > 32),
        `${where} names a statement by more than 32 characters`
      ).toEqual([]);
      expect(
        names.filter((name) => addresses.test(name)),
        `${where} names a statement by an address`
      ).toEqual([]);

      if (size.width === DESKTOP.width) {
        // Ruling 1 on a question: the long one a person answered is cut, with more than 20
        // words held back, and the long one that waits on the reader is drawn whole.
        // The page measures a row's cut once the row stands within a screen of the viewport, in
        // the frame after it comes there, and draws a far row whole until then, so each cut read
        // here is of a row brought into view and read two frames later: a row read as drawn
        // whole is then one the page has measured, not one it has not reached yet.
        const cutOf = async (question: string) => {
          await nodeRow(page, question).first().scrollIntoViewIfNeeded();
          await page.evaluate(() => {
            const frames = Promise.withResolvers<void>();
            requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
            return frames.promise;
          });
          return (await rowLayouts(page, spec)).find((row) => row.id === question);
        };
        // The control that opens a cut text, in the page's own words: "more · N words".
        const more = new RegExp(
          `^${CONTROL_LABELS.read_more} · ([\\d,]+) ${CONTROL_LABELS.read_more_words}$`,
          "u"
        );
        await expect
          .poll(async () => (await cutOf(seeded.answered))?.more ?? null, {
            message: `${where} does not cut the long answered question`,
          })
          .toMatch(more);
        const hidden = Number(
          more.exec((await cutOf(seeded.answered))?.more ?? "")?.[1]?.replace(/,/gu, "")
        );
        expect(
          hidden,
          `${where} holds back 20 words or fewer of the answered question`
        ).toBeGreaterThan(20);
        expect(
          await cutOf(seeded.waiting),
          `${where} cuts the long question that waits on the reader`
        ).toMatchObject({ more: null, wordsHidden: 0 });
        const width = (await decisionsBody(page).boundingBox())?.width ?? 0;
        expect(
          Math.abs(width - 960),
          `${where}: the list is ${width} px wide, not 960`
        ).toBeLessThanOrEqual(1);
      } else {
        const widths = await page.evaluate(() => ({
          client: document.documentElement.clientWidth,
          scroll: document.documentElement.scrollWidth,
        }));
        expect(widths.scroll, `${where} draws wider than the screen`).toBe(widths.client);
      }
    }
  });
});
