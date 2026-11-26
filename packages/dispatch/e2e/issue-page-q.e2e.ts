import { isIdPrefix } from "@legion/contracts";
import type { Locator, Page, Route } from "@playwright/test";

import { IDENTITY_HEIGHT_PROPERTY } from "../web/src/features/issue-page/identity-block";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  DECISIONS_WORDS,
  NODE_STATE_WORDS,
  STATE_LINES,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/tree-drafts";
import { getArtifact, getAsk, getIssueDecisions, listComments } from "./api";
import { nextFrames } from "./frames";
import { seedDecisionsFilter, seedReviewList } from "./issue-layout-seed";
import { OPEN_QUESTION_BLOCK_ID } from "./issue-layout-seed-text";
import { gotoIssue, issuePage, phase } from "./issue-page";
import {
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
  expectOpeningLanded,
  expectRowGroups,
  expectRowLayouts,
  focusRow,
  nodeRow,
  PHONE,
  parentOfRow,
  type RowLayout,
  rowBody,
  rowGroups,
  rowLayouts,
  rowOf,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  focusedNodeId,
  focusInside,
  onScreen,
  openPreset,
  outlineRows,
  tabStopRow,
} from "./issue-page-i";
import { planBody } from "./issue-page-k";
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
import { openSettledView, verificationBody } from "./issue-page-v";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/** A stored text as the renderer draws its paragraphs and `drawnRows` reads them back: the blocks
 *  between its blank lines, each with the up to three spaces of indentation its first line may
 *  carry left out, which markdown reads as no part of the paragraph, and every other character
 *  as stored; joined as `answerText` joins the drawn paragraphs. A word changed, a paragraph
 *  dropped, two paragraphs run together or an inner double space collapsed each still differ. */
function paragraphForm(stored: string): string {
  return stored
    .split(/\n[ \t]*\n/u)
    .map((paragraph) => paragraph.replace(/^ {1,3}/u, ""))
    .join("\n\n");
}

/**
 * The browser rows of the new issue page that read one of its views:
 * the Decisions view - the open questions, the decisions that wait for the reader's review with
 * Keep and Change, the reader's own decisions, and the agents' other decisions folded at the
 * foot - read on the real stack. Each row turns the new page on for its reader through D's
 * `phase`, seeds what it reads with the seeds of `e2e/issue-layout-seed.ts` or the Decisions
 * view's own `e2e/issue-page-q.ts`, and reads the page through D's `e2e/issue-page.ts`, G's
 * `e2e/issue-page-g.ts`, I's `e2e/issue-page-i.ts` and the view's own `e2e/issue-page-q.ts`. The
 * row of the address a view draws no row for reads the Verification and Plan views too, through
 * V's `e2e/issue-page-v.ts` and K's `e2e/issue-page-k.ts`.
 *
 * Seven rows, each under the title of the check it answers:
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
 * - **the address the view draws no row for**, one row: "an address the view draws no row for
 *   draws the outline's line for it in the view's place, with the focus on that line": under
 *   Verification a comment of the issue, which the view draws as a row of its own, a comment
 *   the issue holds none of, and a plan task under a statement, which the view draws no row for
 *   and the outline lands on in its place; under Decisions a comment the issue holds none of, a
 *   block no node carries and a comment id written short that names none; under Plan a comment
 *   the issue holds none of, and a plan task, which the view draws as a step and lands on, then
 *   the first statement's fragment and the task's again, each changed with no load. Each is
 *   named on the view's address, and each that names nothing draws part I's line for it where
 *   the view was.
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
      agents: squeezed(`3 ${DECISIONS_WORDS.agents_many}`),
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
      // answer as the store holds it, by who answered, with no reading under it: the section of
      // decisions.md that restates it is no row of the filter. The answer is drawn by the
      // renderer, a paragraph to each block of its words, so it is read paragraph by paragraph
      // (`drawnRows`' `answerText`): each paragraph's words and inner spaces as stored, and only
      // the indentation a paragraph's first line may carry, up to three spaces, which markdown
      // reads as no part of it, left out, as the outline's answer row reads it (part Q's
      // decision 416).
      const asked = nodeRow(page, seeded.optionedAskId).first();
      await asked.focus();
      await expect
        .poll(async () => rowOf(await drawnRows(page), seeded.optionedAskId)?.answerText ?? null, {
          message: `the answered ask does not draw the person's answer as the store holds it ${at}`,
        })
        .toBe(paragraphForm(stored?.text ?? ""));
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

      expectRowLayouts(await rowLayouts(page, seeded.spec), where, false, size.width);
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
        // the frames after it comes there, and draws a far row whole until then, so each cut
        // read here is of a row brought into view and read two frames later.
        const cutOf = async (question: string) => {
          await nodeRow(page, question).first().scrollIntoViewIfNeeded();
          await nextFrames(page);
          return (await rowLayouts(page, seeded.spec)).find((row) => row.id === question);
        };
        // The control that opens a cut text, in the page's own words: "more · N words".
        const more = new RegExp(
          `^${CONTROL_LABELS.read_more} · ([\\d,]+) ${CONTROL_LABELS.read_more_words}$`,
          "u"
        );
        // The cut is waited for, and the words it holds back are read off the very layout the
        // wait matched, so no later reading of the row stands in for it.
        const answered: { layout?: RowLayout } = {};
        await expect
          .poll(
            async () => {
              answered.layout = await cutOf(seeded.answered);
              return answered.layout;
            },
            { message: `${where} does not cut the long answered question` }
          )
          .toMatchObject({ more: expect.stringMatching(more) });
        const hidden = Number(more.exec(answered.layout?.more ?? "")?.[1]?.replace(/,/gu, ""));
        expect(
          hidden,
          `${where} holds back 20 words or fewer of the answered question`
        ).toBeGreaterThan(20);
        // A row drawn whole is read once, two frames after it came into view. Two frames are an
        // assumed bound, not one the page promises: its measurer reads at most one slice of a
        // batch a frame and carries the rest into later frames (`clamp-measurer.ts`), so a row
        // it has not reached yet would read as whole here too.
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

/** A comment id of the right shape, a whole uuid, that no comment of the seeded issue has: the
 *  comment's own read answers 404 for it. */
const GONE_COMMENT = "5f0c2a9e-7d41-4b8a-9c3e-2e6d1b7a4c90";

/** The same id written short, its first eight hex digits, which name no comment either: the
 *  graph read that resolves an id written short finds nothing for it. */
const SHORT_GONE_COMMENT = GONE_COMMENT.slice(0, 8);

/** A block id no node of the seeded issue carries. */
const NO_BLOCK = "no-such-block";

/** The heading and the line part I's outline draws in its own place for an address that names
 *  nothing the issue holds (`slots/body.tsx`'s `ItemFallback` and `ABSENT_LINES`), for the
 *  issue `key`. That file keeps these words to itself, so they are written out here, as
 *  `deep-links.e2e.ts` writes the heading of a comment that is not found. */
const FALLBACK_LINES = {
  block: (key: string) => ({
    heading: "No row for this block",
    line: `That link names no row of ${key}'s outline.`,
  }),
  comment: (key: string) => ({
    heading: "Comment not found",
    line: `That link doesn't name a comment on ${key} — it may have been deleted.`,
  }),
} as const;

// An address on a view that names a node beside it: the view tells the stage whether it draws a
// row of the node, and where it draws none the stage draws part I's outline in the view's place,
// which lands on the node, or draws part I's line for an address that names nothing, with the
// focus on that line's heading and never left on the page's body.
//
// The Verification view first. A comment of the issue is of no kind the view draws, and the view
// keeps it as the address's own node and draws it as a row of its own, in the view and not in the
// outline, with the focus on the view's tab stop, which is the issue's row by the view's own rule.
// A comment id of the right shape that no comment of the issue has draws part I's line. A node
// the view keeps but draws no row for - one the tree places under a statement that is no check,
// since the view draws a statement's checks alone - answers false as that gone comment does, and
// the outline drawn in the view's place lands on the node's row as the conversation's address
// does, with the focus on that row. A plan task that serves a statement stands for such a node
// here: the tree places a comment under what it answers and never under the block it is anchored
// to (`tree-drafts.ts`'s `addTurns`), so the seed holds no comment under a statement.
//
// Then the Decisions view, with three addresses that name nothing the issue holds, each opened by
// a load of its own: the gone comment, a `#b-` fragment naming a block no node carries, and that
// comment id written short.
//
// Then the Plan view, which draws a row for a plan task alone: the gone comment draws part I's
// line, and the plan task under the statement keeps the view, which lands on the task's step as
// its own address does, with the focus on that step's row once the page has gone quiet. Then the
// fragment alone changes, with no load, as a browser changes it, and the browser gives its entry
// the key it gave the load's: the first statement's fragment draws the outline in the view's
// place, with the focus on the statement's row, and the task's again draws the view, with the
// focus on its step.
//
// Each view is read first at its own address, so each line is drawn where the view drew its
// rows; its gone comment is the first address read whose node the view draws no row for, so a
// stage that keeps the view fails on the words of that comment's line, and the Verification view
// comes first, so such a stage fails there. Each project at its own width: `chromium` the
// desktop's and `iphone` the phone's.
// SLOW(>=2s): the whole issue seeded with three more writes, then eleven loads and two changes of
// the fragment alone: one waited on until the view settles, two until the view draws its rows,
// four until the page goes quiet, one until the outline puts the focus on the plan task's row,
// one until the block's line is drawn, and four until the item's read, which the page asks again
// before it says the item is missing, has settled.
test("an address the view draws no row for draws the outline's line for it in the view's place, with the focus on that line @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const size = test.info().project.name === "iphone" ? PHONE : DESKTOP;
  const at = `at ${size.width} px`;
  await phase(browser, seedDecisionsFilter, async (page, seeded) => {
    // What makes each address name nothing: the issue holds comments, none of them under the
    // gone id or under an id that begins with its eight digits, and the eight digits are an id
    // written short where the whole id is not.
    const comments = (await listComments(seeded.key)).map((comment) => comment.id);
    expect(comments.length, "the seeded issue holds no comment").toBeGreaterThan(0);
    expect(
      comments.filter((id) => id.startsWith(SHORT_GONE_COMMENT)),
      `a comment of the issue begins with ${SHORT_GONE_COMMENT}`
    ).toEqual([]);
    expect(
      { short: isIdPrefix(SHORT_GONE_COMMENT), whole: isIdPrefix(GONE_COMMENT) },
      "the gone comment's id and its first eight digits are not a whole id and a short one"
    ).toEqual({ short: true, whole: false });

    await page.setViewportSize(size);
    const verification = presetPath(seeded.key, "verification");
    // The Verification view's first reading: it draws each statement with the check that serves
    // it at its own address.
    await openSettledView(page, seeded);
    await expectIssuePageDrawn(page);
    const thread = seeded.comments.threads[0] ?? "";
    const traffic = watchApiRequests(page);
    await gotoIssue(page, `${verification}?comment=${thread}`);
    await expectLoadSettled(traffic);
    await expect(
      verificationBody(page).locator(`[data-verification-other][data-node-id="${thread}"]`),
      `a comment of the issue draws no row of its own in the Verification view ${at}`
    ).toHaveCount(1);
    await expect(
      outlineRows(page),
      `a comment of the issue draws the outline in the Verification view's place ${at}`
    ).toHaveCount(0);
    await expect(
      tabStopRow(page),
      `a comment of the issue leaves the focus off the Verification view's tab stop, on the page's body or elsewhere ${at}`
    ).toBeFocused();
    await gotoIssue(page, `${verification}?comment=${GONE_COMMENT}`);
    await expectFallbackLine(
      page,
      FALLBACK_LINES.comment(seeded.key),
      verificationBody(page),
      `a gone comment under Verification ${at}`
    );
    // A plan task that serves the spec's first statement, which the tree places under that
    // statement, where the view draws its checks alone: the outline is drawn in the view's place
    // and lands on the task's row, as the conversation's address does.
    const [{ slug: planSlug }, { slug: specSlug }] = await Promise.all([
      getArtifact(seeded.planArtifactId),
      getArtifact(seeded.specArtifactId),
    ]);
    const taskBlock = seeded.planTasks[0] ?? "";
    const task = blockNodeId(planSlug, taskBlock);
    const statementBlock = seeded.requirements[0] ?? "";
    const statement = blockNodeId(specSlug, statementBlock);
    await gotoIssue(page, `${verification}#b-${taskBlock}`);
    await expect
      .poll(() => outlineRows(page).count(), {
        message: `a plan task under a statement draws no outline in the Verification view's place ${at}`,
      })
      .toBeGreaterThan(0);
    await expect
      .poll(() => focusedNodeId(page), {
        message: `a plan task under a statement leaves the focus off its row of the outline, on the page's body or elsewhere ${at}`,
      })
      .toBe(task);
    expect(
      await parentOfRow(page, task),
      `the plan task the address names stands under no statement ${at}`
    ).toBe(statement);
    await expect(
      verificationBody(page),
      `a plan task under a statement draws the Verification view beside the outline ${at}`
    ).toHaveCount(0);

    await openPreset(page, seeded.key, "decisions");
    // The Decisions view's first reading: it draws its rows at its own address.
    await expect
      .poll(() => decisionsBody(page).locator("[data-node-row]").count(), {
        message: `the Decisions view draws no row ${at}`,
      })
      .toBeGreaterThan(0);
    await expectIssuePageDrawn(page);

    const view = presetPath(seeded.key, "decisions");
    await gotoIssue(page, `${view}?comment=${GONE_COMMENT}`);
    await expectFallbackLine(
      page,
      FALLBACK_LINES.comment(seeded.key),
      decisionsBody(page),
      `a gone comment ${at}`
    );
    await gotoIssue(page, `${view}#b-${NO_BLOCK}`);
    await expectFallbackLine(
      page,
      FALLBACK_LINES.block(seeded.key),
      decisionsBody(page),
      `a block no node carries ${at}`
    );
    await gotoIssue(page, `${view}?comment=${SHORT_GONE_COMMENT}`);
    await expectFallbackLine(
      page,
      FALLBACK_LINES.comment(seeded.key),
      decisionsBody(page),
      `a comment id written short that names none ${at}`
    );

    await openPreset(page, seeded.key, "plan");
    // The Plan view's first reading: it draws its steps at its own address.
    await expect
      .poll(() => planBody(page).locator("[data-node-row]").count(), {
        message: `the Plan view draws no step ${at}`,
      })
      .toBeGreaterThan(0);
    await expectIssuePageDrawn(page);

    const plan = presetPath(seeded.key, "plan");
    await gotoIssue(page, `${plan}?comment=${GONE_COMMENT}`);
    await expectFallbackLine(
      page,
      FALLBACK_LINES.comment(seeded.key),
      planBody(page),
      `a gone comment under Plan ${at}`
    );
    // The plan task the Verification view drew no row for is a step of the Plan view, which keeps
    // the view and lands on the step once every read has answered, as the page going quiet says.
    await gotoIssue(page, `${plan}#b-${taskBlock}`);
    await expectLoadSettled(traffic);
    await expect
      .poll(() => focusedNodeId(page), {
        message: `a plan task under Plan leaves the focus off its step, on the page's body or elsewhere ${at}`,
      })
      .toBe(task);
    await expect(
      planBody(page).locator(`[data-node-row][data-node-id="${task}"]`),
      `a plan task under Plan leaves the focus off the view's step for it ${at}`
    ).toBeFocused();
    await expect(
      outlineRows(page),
      `a plan task under Plan draws the outline in the view's place ${at}`
    ).toHaveCount(0);
    // The first statement's fragment, then the task's again, each changed alone with no load, and
    // each read once the page has gone quiet.
    await gotoIssue(page, `${plan}#b-${statementBlock}`);
    await expectLoadSettled(traffic);
    await expect
      .poll(() => focusedNodeId(page), {
        message: `the first statement's fragment under Plan leaves the focus off its row of the outline ${at}`,
      })
      .toBe(statement);
    await expect(
      planBody(page),
      `the first statement's fragment under Plan draws the view beside the outline ${at}`
    ).toHaveCount(0);
    await gotoIssue(page, `${plan}#b-${taskBlock}`);
    await expectLoadSettled(traffic);
    await expect(
      planBody(page).locator(`[data-node-row][data-node-id="${task}"]`),
      `the task's fragment again under Plan leaves the focus off the view's step for it ${at}`
    ).toBeFocused();
    await expect(
      outlineRows(page),
      `the task's fragment again under Plan draws the outline in the view's place ${at}`
    ).toHaveCount(0);
  });
});

/** The page draws part I's line `words` in the place of the view whose body is `view` for the
 *  address it has just loaded, `what`: its heading and its line, with the focus on the heading,
 *  and none of the view beside it. */
async function expectFallbackLine(
  page: Page,
  words: { readonly heading: string; readonly line: string },
  view: Locator,
  what: string
): Promise<void> {
  const named = { exact: true, name: words.heading } as const;
  const heading = issuePage(page).getByRole("heading", named);
  await expect(heading, `${what} draws no "${words.heading}" in the view's place`).toBeVisible();
  // The inner locator of `has` is read inside each section, so it names no root of its own.
  await expect(
    issuePage(page)
      .locator("section")
      .filter({ has: page.getByRole("heading", named) })
      .locator("p"),
    `${what} draws another line under "${words.heading}"`
  ).toHaveText(words.line);
  await expect(
    heading,
    `${what} leaves the focus off "${words.heading}", on the page's body or elsewhere`
  ).toBeFocused();
  await expect(view, `${what} draws the view beside the line`).toHaveCount(0);
}

// The tester's red row of round 17 (the class of every hold on a write letting go at the write's
// request deadline, REQUEST_DEADLINE_MS, 30 s): offline, the Decisions view's Send change lets its
// hold go within that deadline. At 1e21af01 it is still held 32 s after the press: its hold
// (`DecisionActions.tsx`, `sendHeld`) waits on a read of the document's text that has no bound,
// and offline that read pauses and never settles.
// SLOW(>=2s): a seeded issue whose Decisions view is loaded, a Change sent offline, and its hold
// read past the request deadline.
test("a change sent offline on a decision lets its Send change go within the request deadline", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const id = (block: string) => blockNodeId(seeded.decisionsSlug, block);
    const changed = id(seeded.tiedNewer);
    await page.setViewportSize(DESKTOP);
    await openPreset(page, seeded.key, "decisions");
    await expect
      .poll(() => decisionsPartRows(page, "review"), {
        message: "the review list before the Change",
      })
      .toEqual([changed, id(seeded.tiedOlder)]);
    const row = nodeRow(page, changed).first();
    await row.locator('button[data-row-action="change"]').click();
    await row.getByLabel(CONTROL_LABELS.your_change).fill("Tie it to the first statement instead.");
    await page.context().setOffline(true);
    const send = row.getByRole("button", { exact: true, name: CONTROL_LABELS.send_change });
    await send.click();
    // The hold is read once the 30 s request deadline and two seconds more have passed.
    await page.waitForTimeout(32_000);
    await expect(
      send,
      "Send change is no longer held past the request deadline"
    ).not.toHaveAttribute("aria-disabled", "true");
  });
});

// The tester's red row of round 19 (27b5a621: Send change says why its document read has not
// settled, `DecisionActions.tsx`'s `unsettled`). A Change whose read of the decisions document is
// never answered is sent at the request deadline with no place counted, and the form says why
// beside Send change, in the words of a read of that document that failed and then the error such
// a read ends with. At c6dc939c it never does: the read is a direct GET (`api.getArtifactText`)
// whose own 30 s deadline (`client.ts`'s `exchange`) is set before `readWithinBound`'s bound and
// ends the read first, and the send takes that failure for a settled read (`() => undefined`), so
// `readWithinBound` answers no reason. Dispatch's refusal of the change is stubbed, so the form
// stays drawn with what it says. Measured in Chromium and Firefox.
// SLOW(>=2s): a seeded issue whose Decisions view is loaded, and its document read left
// unanswered past the 30 s request deadline.
// @firefox: the bound and the read's own deadline are two timers, which each engine runs.
test("a change whose document read is never answered says why beside Send change at the request deadline @firefox", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const step = (text: string) => `a document read never answered: ${text}`;
    const row = nodeRow(page, blockNodeId(seeded.decisionsSlug, seeded.tiedNewer)).first();
    await page.setViewportSize(DESKTOP);
    await openPreset(page, seeded.key, "decisions");
    await row.locator('button[data-row-action="change"]').click();
    await row.getByLabel(CONTROL_LABELS.your_change).fill("Tie it to the first statement instead.");
    const commentsPath = `/api/v1/issues/${seeded.key}/comments`;
    let pressed = false;
    await page.route(
      (url) => url.pathname.startsWith("/api/"),
      async (route) => {
        const request = route.request();
        const path = new URL(request.url()).pathname;
        if (request.method() === "POST" && path === commentsPath) {
          await route.fulfill({
            body: JSON.stringify({
              code: "TARGET_AMBIGUOUS",
              error:
                '"Tie it" matches 2 places; pass a zero-based occurrence, or quote more of the surrounding text',
            }),
            contentType: "application/json",
            status: 409,
          });
          return;
        }
        // Once Send change is pressed, no read of a document's text is answered.
        if (
          pressed &&
          request.method() === "GET" &&
          /^\/api\/v1\/artifacts\/[^/]+\/text$/.test(path)
        ) {
          return;
        }
        await route.fallback();
      }
    );
    const send = row.getByRole("button", { exact: true, name: CONTROL_LABELS.send_change });
    pressed = true;
    await send.click();
    await expect(send, step("Send change is held while the document is read")).toHaveAttribute(
      "aria-disabled",
      "true"
    );
    await expect(
      row.getByText(ANSWER_LINES.change_not_sent),
      step("the change goes at the request deadline, and its refusal is said")
    ).toBeVisible({ timeout: 35_000 });
    await expect(send, step("Send change lets go at the request deadline")).not.toHaveAttribute(
      "aria-disabled",
      "true"
    );
    await expect(
      row.getByText(
        `${STATE_LINES.decisions_document_unreadable} Dispatch did not answer within 30 seconds.`
      ),
      step("the form says why the document read has not settled")
    ).toBeVisible();
  });
});

// The tester's red row of round 20 (b45cd7f8, DecisionActions.tsx: "The line is the press's own:
// it goes at the next press of Send change and with the form"). When a press's document read is
// ended by the 30 s bound (`readWithinBound`) while the read is still out - its first attempt
// answered 404 at 26 s, which restarts `client.ts`'s 30 s silence deadline, and its retry stays
// unanswered - `keepUnsettled` holds the reason in `useUnsettledRead`. At the next press of Send
// change, whose read is answered 200, DecisionActions.tsx:421 clears `readFailed` alone and leaves
// `unsettled` drawn, so the form still shows "Dispatch did not answer within 30 seconds." after
// the second change goes. Measured in Chromium and Firefox, twice each.
// SLOW(>=2s): a seeded issue's Decisions view, a read held past the 30 s bound, and a second press.
// @firefox: the bound and the read's own deadline are two timers, which each engine runs.
test("a Change whose document read the bound ended loses that read's line at the next press of Send change @firefox", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const step = (text: string) => `a press after the bound: ${text}`;
    const row = nodeRow(page, blockNodeId(seeded.decisionsSlug, seeded.tiedNewer)).first();
    await page.setViewportSize(DESKTOP);
    await openPreset(page, seeded.key, "decisions");
    await row.locator('button[data-row-action="change"]').click();
    await row.getByLabel(CONTROL_LABELS.your_change).fill("Tie it to the first statement instead.");
    const commentsPath = `/api/v1/issues/${seeded.key}/comments`;
    const held: Route[] = [];
    let posts = 0;
    let reads = 0;
    let pressed = false;
    await page.route(
      (url) => url.pathname.startsWith("/api/"),
      async (route) => {
        const request = route.request();
        const path = new URL(request.url()).pathname;
        if (request.method() === "POST" && path === commentsPath) {
          posts += 1;
          await route.fulfill({
            body: JSON.stringify({ code: "TARGET_AMBIGUOUS", error: '"Tie it" matches 2 places' }),
            contentType: "application/json",
            status: 409,
          });
          return;
        }
        if (
          pressed &&
          request.method() === "GET" &&
          /^\/api\/v1\/artifacts\/[^/]+\/text$/.test(path)
        ) {
          reads += 1;
          if (reads === 1) {
            // Held 26 s before answering 404 so the client's 30 s silence deadline restarts on its
            // retry and readWithinBound ends the wait at 30 s while that retry is still out.
            const waited = Promise.withResolvers<void>();
            setTimeout(waited.resolve, 26_000);
            await waited.promise;
            await route.fulfill({
              body: JSON.stringify({ code: "NOT_FOUND", error: `no route for GET ${path}` }),
              contentType: "application/json",
              status: 404,
            });
            return;
          }
          if (reads === 2) {
            held.push(route);
            return;
          }
        }
        await route.fallback();
      }
    );
    const send = row.getByRole("button", { exact: true, name: CONTROL_LABELS.send_change });
    const unread = STATE_LINES.decisions_document_unreadable;
    const readLine = row.getByRole("alert").filter({ hasText: unread });
    pressed = true;
    await send.click();
    await expect(
      readLine,
      step("at the 30 s bound the form says why the read has not settled")
    ).toHaveText([`${unread} Dispatch did not answer within 30 seconds.`], { timeout: 45_000 });
    await expect
      .poll(() => posts, { message: step("the first change goes"), timeout: 10_000 })
      .toBe(1);
    await expect.poll(() => held.length, { message: step("the read's retry is held") }).toBe(1);
    try {
      await send.click();
      await expect
        .poll(() => posts, { message: step("the second press sends its change"), timeout: 15_000 })
        .toBe(2);
      await expect(
        readLine,
        step("the next press of Send change, whose read is answered, takes the line away")
      ).toHaveCount(0, { timeout: 2_000 });
    } finally {
      await held[0]?.abort("internetdisconnected").catch(() => undefined);
    }
  });
});

// The tester's red row of round 21 (an inline form a button ends leaves the focus on the page's
// body: DecisionActions.tsx:567-574, the Change form's Cancel, `onClick={onDone}`, whose `onDone`
// (:293) is `() => change(false)`, and `change` (:192-197) sets `changing` and the draft and
// moves no focus, so the form goes with the Cancel that held the focus and nothing gives it back
// to the decision's Change (:267-277) that opened the form; the architect's ruling of
// 2026-11-26T05:17Z). The row opens the Change form of the newer decision that waits for review
// by a click on its Change, types a change and clicks Cancel. At c3b536cc the focus is then on
// the page's body, at 1440 and at 390 px, in Chromium (4 of 4) and Firefox (2 of 2).
// SLOW(>=2s): one seeded issue whose Decisions view is loaded once, and one Change form opened,
// typed in and cancelled; no write.
// @firefox: the engine's focus after the element that held it is removed (playwright.config.ts names the clause).
test("a decision's Change form ended by its Cancel gives the focus back to the Change that opened it @firefox", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReviewList, async (page, seeded) => {
    const step = (text: string) => `the Decisions view's Change form ended by Cancel: ${text}`;
    const row = nodeRow(page, blockNodeId(seeded.decisionsSlug, seeded.tiedNewer)).first();
    const change = row.locator('button[data-row-action="change"]');
    const field = row.getByLabel(CONTROL_LABELS.your_change);
    await page.setViewportSize(DESKTOP);
    await openPreset(page, seeded.key, "decisions");
    await expectOpeningLanded(page);
    await expect(
      change,
      step("the newer decision waits for review and draws its Change.")
    ).toBeVisible();
    await change.click();
    await expect(field, step("Change opens the form with the caret in Your change.")).toBeFocused();
    await field.fill("Tie it to the first statement instead.");
    await row.getByRole("button", { exact: true, name: CONTROL_LABELS.cancel }).click();
    await expect(field, step("Cancel takes the form away.")).toHaveCount(0);
    await expect(
      change,
      step(
        "after Cancel the focus is on the decision's Change that opened the form, not on the page's body."
      )
    ).toBeFocused();
  });
});
