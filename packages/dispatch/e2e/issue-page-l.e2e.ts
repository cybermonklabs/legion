import type { Locator, Page } from "@playwright/test";

import {
  ANSWER_LINES,
  CONTROL_LABELS,
  CURRENT_POINT,
  PRESET_NAMES,
} from "../web/src/features/issue-page/page-words";
import { DWELL_MS } from "../web/src/features/shell/useDwell";
import {
  createAsk,
  createIssue,
  createProject,
  getArtifactBlocks,
  getIssueEvents,
  getMyState,
  listComments,
} from "./api";
import { seedReplyNesting } from "./issue-layout-seed";
import { gotoIssue, phase } from "./issue-page";
import {
  blockNode,
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
  focusRow,
  nodeRow,
  nodeRows,
  PHONE,
  parentOfRow,
  READER,
  rowAct,
  rowOf,
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import {
  branchMoreRows,
  expectActivePreset,
  focusedNodeId,
  onScreen,
  openEveryDeeperBranch,
} from "./issue-page-i";
import {
  clearOfBox,
  composer,
  composerField,
  composerForm,
  composerKind,
  composerTarget,
  endPlace,
  outlineEnd,
  placesHolding,
  replyFromRow,
  STATEMENT_BLOCKS,
  STATEMENT_WORDS,
  scrollEndUnderBox,
  seedFortyComments,
  seedStatements,
  seedUnreadIssue,
  sendFromComposer,
  wordsInField,
} from "./issue-page-l";
import { runClockUntil, watchWrites } from "./keyed-writes";
import { commentWithBody } from "./margin-helpers";
import { expect, test } from "./test";
import { asUser } from "./users";

/**
 * Writing on the new issue page: the page's one
 * box to write in, held at the foot of the screen; a reply drawn under what it answers; and the
 * read mark at the outline's end. Each row is one check of
 * the verification document, named by its `-g` string: rows (1) to (4) are
 * c-replies, (5) and (6) c-box, (7) c-mark, (8) the outline's half of c-statement-reply, and (9)
 * its Spec view half, owed until the Spec view's keymap takes the box's writer and skipped until
 * then (`@owed`). Row (10) is R12's: the conversation's current point, which the outline marks on
 * one row (`r-current-point` of the spec). Row (11) is R13's half of c-outline-focus that the
 * reader causes, beside the row of `issue-page-i.e2e.ts` for the other half.
 *
 * The rows are on the real stack, each through `phase` (a reset database, alice signed in,
 * the new page turned on with `withIssuePage`): every row but (11), which writes nothing, writes
 * from the box and then reads its own write, as Dispatch stored it and as the outline draws it
 * from the server's reads. Rows (1) to (5) are the checkpoint's
 * (`git show 29111db16c41:packages/dispatch/e2e/issue-layout.e2e.ts`), where the issue itself is
 * no row of the outline now (ruling 5), so a node with no parent is read at the outline's top
 * level.
 */

/** The widest window that draws the phone's form of the page (decision 72 of the parts). */
const PHONE_FORM_MAX = 760;

/** The empty line's height, and the height it grows to before it scrolls inside itself (R6). */
const LINE_AT_REST = 26;
const LINE_AT_MOST = 150;

/** The least width and height of each control of the box (R2, the tester's finding F13). */
const CONTROL_LEAST = 24;

/** The width and height of every control of the box that is drawn, named in a failure by its
 *  accessible name or its words. */
function boxControls(
  page: Page
): Promise<{ readonly height: number; readonly name: string; readonly width: number }[]> {
  return composer(page).evaluate((section) =>
    [...section.querySelectorAll<HTMLElement>("button, textarea")]
      .filter((control) => control.getClientRects().length > 0)
      .map((control) => {
        const box = control.getBoundingClientRect();
        return {
          height: box.height,
          name: (control.getAttribute("aria-label") ?? control.textContent ?? "").trim(),
          width: box.width,
        };
      })
  );
}

/** Fails where a control of the box is smaller than 24 by 24 px. A rendered size is fractional,
 *  so a hundredth of a pixel short of the bound still meets it. */
async function expectControlsLeast(page: Page, when: string): Promise<void> {
  const controls = await boxControls(page);
  expect(controls.length, `the box draws no control ${when}`).toBeGreaterThan(0);
  expect(
    controls.filter(
      (control) => control.width < CONTROL_LEAST - 0.01 || control.height < CONTROL_LEAST - 0.01
    ),
    `a control of the box is smaller than ${CONTROL_LEAST} by ${CONTROL_LEAST} px ${when}`
  ).toEqual([]);
}

/**
 * Fails unless the box's refusal of a send - its Retry, and the Discard draft a send the reader
 * stopped offers beside it - draws each control of the box at least 24 by 24 px at `size` (R2,
 * decision 43 of the parts). Read on a page of its own in the same sign-in, whose comment sends
 * Dispatch answers `500` every time: a 5xx is no verdict, so the send is tried again under its
 * key until its deadline, goes late, and the reader stops it (`Stop trying`). The page's own
 * clock runs past the deadline (`runClockUntil`), which costs no real time, and the page this
 * row reads is left as it was.
 */
async function expectRefusalControlsLeast(
  page: Page,
  key: string,
  size: { readonly height: number; readonly width: number },
  at: string
): Promise<void> {
  const refusing = await page.context().newPage();
  try {
    await refusing.setViewportSize({ height: size.height, width: size.width });
    await refusing.route(`**/api/v1/issues/${key}/comments`, (route) =>
      route.request().method() === "POST"
        ? route.fulfill({
            body: JSON.stringify({ code: "UNAVAILABLE", error: "Dispatch is down." }),
            contentType: "application/json",
            status: 500,
          })
        : route.fallback()
    );
    await refusing.clock.install();
    await gotoIssue(refusing, `/issues/${key}`);
    await expectIssuePageDrawn(refusing);
    const field = composerField(refusing);
    await field.fill(`A comment Dispatch never takes ${at}.`);
    await field.press("Enter");
    const stop = composer(refusing).getByRole("button", { name: "Stop trying" });
    await runClockUntil(refusing, expect(stop).toBeVisible({ timeout: 60_000 }));
    await stop.click();
    const retry = composer(refusing).getByRole("button", { exact: true, name: "Retry" });
    const discard = composer(refusing).getByRole("button", { name: "Discard draft" });
    await expect(retry, `the stopped send draws no Retry ${at}`).toBeVisible();
    await expect(discard, `the stopped send draws no Discard draft ${at}`).toBeVisible();
    await expectControlsLeast(refusing, `${at}, the send stopped`);
    await discard.click();
    await expect(retry).toHaveCount(0);
  } finally {
    await refusing.close();
  }
}

/** The box of the line that is drawn. */
async function formBox(page: Page, when: string) {
  const box = await composerForm(page).boundingBox();
  expect(box, `the box's line has no box ${when}`).not.toBeNull();
  return box ?? { height: 0, width: 0, x: 0, y: 0 };
}

/** Fails unless the line's lower edge is 8 px above the viewport's foot (R1). */
async function expectFormAtFoot(page: Page, when: string): Promise<void> {
  const box = await formBox(page, when);
  const viewport = await page.evaluate(() => window.innerHeight);
  const gap = viewport - (box.y + box.height);
  expect(
    Math.abs(gap - 8),
    `the form is not 8 px above the viewport's foot ${when}: it is ${gap.toFixed(1)} px above it`
  ).toBeLessThanOrEqual(1);
}

/** Fails unless the line stands where the outline does: its left edge and width are the
 *  outline's, at most 1,000 px (R1). */
async function expectFormOnOutline(page: Page, when: string): Promise<void> {
  const box = await formBox(page, when);
  // The outline is measured once, at the instant the form above was, because the row compares
  // where the two stand now and both are laid out in the same frame.
  const outline = await tree(page).boundingBox();
  expect(outline, `the page draws no outline ${when}`).not.toBeNull();
  expect(
    Math.abs(box.x - (outline?.x ?? 0)),
    `the form's left edge is ${box.x.toFixed(1)} px and the outline's ${outline?.x.toFixed(1)} px ${when}`
  ).toBeLessThanOrEqual(1);
  expect(
    Math.abs(box.width - (outline?.width ?? 0)),
    `the form is ${box.width.toFixed(1)} px wide and the outline ${outline?.width.toFixed(1)} px ${when}`
  ).toBeLessThanOrEqual(1);
  expect(box.width, `the form is wider than 1,000 px ${when}`).toBeLessThanOrEqual(1000);
}

/** Fails unless the line is 10 px from each side of the viewport (R1, at 760 px and below). */
async function expectFormInset(page: Page, when: string): Promise<void> {
  const box = await formBox(page, when);
  const viewport = await page.evaluate(() => window.innerWidth);
  expect(
    Math.abs(box.x - 10),
    `the form's left edge is ${box.x.toFixed(1)} px ${when}`
  ).toBeLessThanOrEqual(1);
  expect(
    Math.abs(box.width - (viewport - 20)),
    `the form is ${box.width.toFixed(1)} px wide, not ${viewport - 20} px, ${when}`
  ).toBeLessThanOrEqual(1);
}

/** The id of the one row whose words are `words`, once the outline draws it: a write from the
 *  box is one new row, and no other element of the page holds its words. */
async function oneRowHolding(page: Page, words: string, what: string): Promise<string> {
  const rows = nodeRows(page).filter({ hasText: words });
  await expect(rows, `${what} is not drawn as one row`).toHaveCount(1);
  const id = (await rows.getAttribute("data-node-id")) ?? "";
  expect(
    (await placesHolding(page, words)).filter((place) => place !== id),
    `${what}'s words are held outside its row`
  ).toEqual([]);
  return id;
}

/** The rows the page names to a screen reader as the conversation's current point: each that
 *  carries `aria-current`, whatever its value (R12). */
function currentRows(page: Page): Locator {
  return nodeRows(page).filter({ has: page.locator(":scope[aria-current]") });
}

/** The rows that carry a mark of the conversation's current point (R12): the point's own row
 *  (`here`), or each drawn row above it on its path while its own row is folded away (`path`). */
function markedRows(page: Page): Locator {
  return nodeRows(page).filter({ has: page.locator(":scope[data-current-point]") });
}

/** Fails unless node `id`'s row, `what`, is the conversation's current point and no other row
 *  carries a mark of it (R12): that row is the current location for a screen reader
 *  (`aria-current`, `CURRENT_POINT`) and carries the point's own mark, and no other row carries
 *  either. */
async function expectPointOn(page: Page, id: string, what: string, when: string): Promise<void> {
  const row = nodeRow(page, id).first();
  await expect(row, `${what} is not the current location ${when}`).toHaveAttribute(
    "aria-current",
    CURRENT_POINT
  );
  await expect(row, `${what} does not carry the current point's own mark ${when}`).toHaveAttribute(
    "data-current-point",
    "here"
  );
  await expect(
    currentRows(page),
    `a row besides ${what} is the current location ${when}`
  ).toHaveCount(1);
  await expect(
    markedRows(page),
    `a row besides ${what} carries a mark of the current point ${when}`
  ).toHaveCount(1);
}

/** An outline as the browser computes it: its colour, its offset, its style and its width. */
interface Ring {
  readonly color: string;
  readonly offset: string;
  readonly style: string;
  readonly width: string;
}

/** A ring the page draws around a bullet's dot (R12): a solid outline `width` wide and `offset`
 *  off the dot, in the colour of the page's token `--color-<token>`. */
interface RingClass {
  readonly offset: string;
  readonly token: string;
  readonly width: string;
}

/** The ring on the dot of the current point's own row, its branch not folded (`theme/classes.ts`'s
 *  `pagePointRing`): 1.5 px of the page's quiet grey, 2 px off the dot. */
const POINT_RING: RingClass = { offset: "2px", token: "page-quiet", width: "1.5px" };

/** The lighter ring on the dot of a row above the current point on its path while the point's own
 *  row is folded away, that row's own branch folded (`pagePathRingFolded`): 1 px of the bullets'
 *  grey, 5 px off the dot, so it stands outside the folded branch's halo. */
const PATH_RING_FOLDED: RingClass = { offset: "5px", token: "page-bullet", width: "1px" };

/** The outline of the bullet's dot in `row` (`NodeRow.tsx`'s `Bullet`), as the browser computes
 *  it. */
function dotRing(row: Locator): Promise<Ring> {
  return row
    .locator("[data-node-bullet] > i")
    .first()
    .evaluate((dot) => {
      const style = getComputedStyle(dot);
      return {
        color: style.outlineColor,
        offset: style.outlineOffset,
        style: style.outlineStyle,
        width: style.outlineWidth,
      };
    });
}

/**
 * `ring` as this browser computes it, read off a probe that is given its outline and its token as
 * its `color` and is removed at once: the token's value in the scheme the page is drawn in, and
 * the width as the browser keeps it. A browser that snaps an outline's width to whole device
 * pixels at computed-value time, as CSS Values 4 has it ("snap as a border width"), keeps 1.5 px
 * as 1 px on a screen of one device pixel to the CSS pixel, so a dot is held to the width its
 * class computes to here rather than to the words of the class.
 */
function owedRing(page: Page, ring: RingClass): Promise<Ring> {
  return page.evaluate(({ offset, token, width }) => {
    const probe = document.createElement("i");
    probe.style.color = `var(--color-${token})`;
    probe.style.outline = `${width} solid`;
    probe.style.outlineOffset = offset;
    document.body.append(probe);
    const style = getComputedStyle(probe);
    const owed = {
      color: style.color,
      offset: style.outlineOffset,
      style: style.outlineStyle,
      width: style.outlineWidth,
    };
    probe.remove();
    return owed;
  }, ring);
}

// c-replies (1): a reply is drawn
// under the entry it answers, at any depth, a resolved question's reply included; every node
// that takes a reply draws the control; a reply from the box lands under its target; an answer
// is drawn in the question's row; and a comment with no target is drawn at the outline's top
// level, where the issue's own nodes are. On a phone a branch below the first level is one line
// counting its nodes, which opens them in place.
// SLOW(>=2s): a thread four levels deep unfolded press by press, three writes from the box each
// read back from the server and the outline, and a second load at the phone's width.
test("a reply is drawn under the entry it answers, and every node takes a reply @phone", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    // What the server stored, before the page is read: `reply_to` flattened onto the head and
    // the entry each reply answered kept beside it.
    const stored = await listComments(seeded.key);
    const second = stored.find((comment) => comment.id === seeded.second);
    expect(second?.reply_to, "the server did not flatten the thread onto its head").toBe(
      seeded.head
    );
    expect(second?.answers, "the server kept no answered entry on the second reply").toBe(
      seeded.first
    );

    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    // From 1024 px up the outline draws three levels and a branch deeper than them as one line
    // saying how many nodes it holds, which opens them in place: the reply that answers `second`
    // is the fourth level of the tree. The thread's folds are opened by their fold controls
    // alone, since G's `unfoldEveryBranch` also presses that line.
    const folds = nodeRows(page).getByRole("button", { exact: true, name: CONTROL_LABELS.unfold });
    // The outline's rows were drawn with their fold controls in one render (the wait above), and
    // each press redraws before the next count, so a count of none means every fold is open.
    for (let pressed = 0; pressed < 20 && (await folds.count()) > 0; pressed += 1) {
      await folds.first().click();
    }
    const deeper = branchMoreRows(page);
    await expect(deeper, "the fourth level is not folded to one line").not.toHaveCount(0);
    await expect(deeper.first()).toContainText("1");
    await expect(nodeRow(page, seeded.third)).toHaveCount(0);
    await openEveryDeeperBranch(page);
    await expect(nodeRow(page, seeded.third)).not.toHaveCount(0);
    expect(await parentOfRow(page, seeded.third)).toBe(seeded.second);

    expect(
      await parentOfRow(page, seeded.second),
      `the reply that answers another reply is not drawn under it; the thread's head is ${seeded.head}`
    ).toBe(seeded.first);
    expect(
      await parentOfRow(page, seeded.first),
      "the reply to the thread's head is not drawn under it"
    ).toBe(seeded.head);
    expect(
      await parentOfRow(page, seeded.head),
      "the thread's head is not drawn under the issue"
    ).toBeNull();

    // Every node that takes a reply draws the control, an answered and a resolved ask included;
    // the open ask draws the control that answers it.
    for (const id of [
      seeded.head,
      seeded.first,
      seeded.second,
      seeded.answeredAsk,
      seeded.resolvedAsk,
    ]) {
      const row = await focusRow(page, id);
      await expect(rowAct(row, "reply"), `${id} draws no reply control`).toBeVisible();
    }
    await expect(rowAct(await focusRow(page, seeded.openAsk), "answer")).toBeVisible();

    /** Fails unless node `id` is drawn under `parent`, `null` for the outline's top level, once
     *  every branch is open. */
    const drawnUnder = async (id: string, parent: string | null, what: string) => {
      await unfoldEveryBranch(page);
      await openEveryDeeperBranch(page);
      await expect.poll(() => parentOfRow(page, id), { message: what }).toBe(parent);
    };

    // A resolved ask still takes a reply: one sent from its control is drawn under that ask.
    await replyFromRow(page, seeded.resolvedAsk, "A reply on the resolved question.");
    await drawnUnder(
      (await commentWithBody(seeded.key, undefined, "A reply on the resolved question.")).id,
      seeded.resolvedAsk,
      "the reply on the resolved ask is not drawn under it"
    );
    // A reply sent from a reply's control is drawn under that reply.
    await replyFromRow(page, seeded.first, "A reply to the first answer.");
    await drawnUnder(
      (await commentWithBody(seeded.key, undefined, "A reply to the first answer.")).id,
      seeded.first,
      "the reply to a reply is not drawn under it"
    );

    // An answer sent from the open ask's control is drawn in the ask's row.
    await focusRow(page, seeded.openAsk);
    await nodeRow(page, seeded.openAsk).first().locator('[data-pick-option="Left"]').click();
    await composer(page).getByRole("button", { name: CONTROL_LABELS.send_answer }).click();
    await expect(nodeRow(page, seeded.openAsk).first()).toHaveAttribute(
      "data-node-state",
      "answered"
    );
    await expect(
      nodeRow(page, seeded.openAsk).first().locator('[data-node-option="chosen"]')
    ).toContainText("Left");

    // The box at the foot posts a comment drawn at the outline's top level, where the issue's
    // own nodes are: the issue itself is no row.
    const instead = composer(page).getByRole("button", { name: CONTROL_LABELS.write_on_issue });
    // The answer that landed above ended the line's target, so the box holds no reply and offers
    // no way back to the issue.
    await expect(instead, "the box still holds a target after the answer landed").toHaveCount(0);
    await sendFromComposer(page, "A comment on the issue from the composer.");
    await drawnUnder(
      (await commentWithBody(seeded.key, undefined, "A comment on the issue from the composer."))
        .id,
      null,
      "the composer's comment is not drawn under the issue"
    );

    // On a phone the outline draws one level below the node it is read from (R16): opening the
    // thread's head draws what it holds as one line saying how many nodes that is, and pressing
    // the line draws them in its place, under the head.
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await nodeRow(page, seeded.head)
      .first()
      .getByRole("button", { exact: true, name: CONTROL_LABELS.unfold })
      .click();
    const counted = branchMoreRows(page);
    await expect(
      counted,
      "the branch below the first level is not one line counting its nodes on a phone"
    ).toHaveCount(1);
    await expect(counted).toContainText(/\d+/);
    await expect(nodeRow(page, seeded.first)).toHaveCount(0);
    await counted.getByRole("button").click();
    await expect(
      nodeRow(page, seeded.first),
      "pressing the counted line draws none of the nodes it holds"
    ).not.toHaveCount(0);
    expect(await parentOfRow(page, seeded.first)).toBe(seeded.head);
  });
});

// c-replies (2): a question its asker resolved still takes a reply, which
// Dispatch stores with the question's `ask_id` and the outline draws under the question rather
// than under the issue or nowhere.
// SLOW(>=2s): a seeded thread and three questions, a reply written from the box and polled until
// Dispatch holds it, and every branch opened press by press to read where it is drawn.
test("a reply on a resolved ask is drawn under the ask", async ({ browser }) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(nodeRow(page, seeded.resolvedAsk).first()).toHaveAttribute(
      "data-node-state",
      "resolved"
    );
    await replyFromRow(page, seeded.resolvedAsk, "Still worth a word after it was resolved.");
    const reply = await commentWithBody(
      seeded.key,
      undefined,
      "Still worth a word after it was resolved."
    );
    expect(reply, "Dispatch did not store the reply with the question's ask_id").toMatchObject({
      ask_id: seeded.resolvedAsk,
    });
    // A resolved question is no path to anything open, so its branch arrives folded (R1).
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    await expect.poll(() => parentOfRow(page, reply.id)).toBe(seeded.resolvedAsk);
    await expect(nodeRow(page, reply.id).first()).toContainText(
      "Still worth a word after it was resolved."
    );
  });
});

// c-replies (3). R4, R16 and the tester's F1: what the reader sends from the box at the foot is
// drawn at once, its branch unfolded, on screen clear of the box and with the focus on it,
// whether it is a reply under a folded thread, a reply under an answered question, or a new
// comment or question on the issue, which both land at the outline's end.
// SLOW(>=2s): four writes from the box, each waited on until the outline draws and focuses it.
test("what the reader sends from the box is drawn, in view and focused, wherever it lands", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(nodeRow(page, seeded.head).first()).toBeVisible();
    /** The row drawing `words`, which the reader has just sent: drawn, focused, and on screen
     *  clear of the box. */
    const expectLandedOn = async (words: string, how: string): Promise<string> => {
      const row = nodeRows(page).filter({ hasText: words }).first();
      await expect(row, `${how}: the reader's own new words are not drawn`).toBeVisible();
      const id = (await row.getAttribute("data-node-id")) ?? "";
      await expect
        .poll(() => focusedNodeId(page), { message: `${how}: the focus is not on the new row` })
        .toBe(id);
      await expect
        .poll(() => clearOfBox(row), { message: `${how}: the new row is off screen` })
        .toBe(true);
      return id;
    };
    /** Sends `words` from the box with the keyboard alone, after `key` on node `id`'s row. */
    const sendWithKeys = async (id: string, key: string, words: string): Promise<void> => {
      await focusRow(page, id);
      await page.keyboard.press(key);
      await expect(composerField(page), `${key} put no caret in the box`).toBeFocused();
      await page.keyboard.type(words);
      await page.keyboard.press("Enter");
    };

    // A reply under a thread the page folds: `r`, the words, Enter.
    await expect(
      nodeRow(page, seeded.first),
      "the seed's thread is open, so a reply under it proves nothing about the fold"
    ).toHaveCount(0);
    await sendWithKeys(seeded.head, "r", "A reply under the folded thread.");
    const reply = await expectLandedOn("A reply under the folded thread.", "a reply under a fold");
    expect(await parentOfRow(page, reply), "the reply is not drawn under the row it answers").toBe(
      seeded.head
    );
    // The focus is the reader's place: the next move goes on from the new row, to the row
    // after it, and not from the page with nothing focused, which starts at the first row.
    await page.keyboard.press("j");
    await expect
      .poll(
        async () => {
          const on = await focusedNodeId(page);
          return on === reply ? "still on the new row" : on === seeded.head ? "the first row" : on;
        },
        { message: "after a keyboard send the next key does not go on from the new row" }
      )
      .toBe(seeded.answeredAsk);

    // A reply under an answered question.
    await sendWithKeys(seeded.answeredAsk, "r", "A word under the answered question.");
    const under = await expectLandedOn(
      "A word under the answered question.",
      "a reply under an answered question"
    );
    expect(await parentOfRow(page, under), "that reply is not drawn under the question").toBe(
      seeded.answeredAsk
    );

    // A new comment and a new question on the issue both land at the outline's end, in the
    // order they were written, each where the reader is taken.
    await composerField(page).click();
    await sendFromComposer(page, "A new comment on the issue.");
    const comment = await expectLandedOn("A new comment on the issue.", "a new comment");
    await composerField(page).click();
    await composerKind(page, "ask").click();
    await sendFromComposer(page, "A new question of the issue?");
    const question = await expectLandedOn("A new question of the issue?", "a new question");
    const top = (await drawnRows(page)).filter((row) => row.level === "1").map((row) => row.id);
    expect(
      top.slice(-2),
      "a new comment and a new question do not land together at the outline's end"
    ).toEqual([comment, question]);
  });
});

// c-replies (4). The tester's F10: a send made with the keyboard alone - `a` on an open question,
// the words, Enter - leaves the focus on a row of the outline, the question's own, and the next
// key goes on from there, not from the outline's first row.
// SLOW(>=2s): a seeded thread and three questions, and an answer written from the box and waited
// on until the outline draws it in the question's row and gives the row the focus back.
test("a send with the keyboard leaves the focus on the reader's row of the outline", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await focusRow(page, seeded.openAsk);
    await page.keyboard.press("a");
    await expect(composerField(page), "a put no caret in the box").toBeFocused();
    await page.keyboard.type("Left, in my own words.");
    await page.keyboard.press("Enter");
    await expect(nodeRow(page, seeded.openAsk).first()).toContainText("Left, in my own words.");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "after an answer sent with the keyboard the focus is not on the question's row",
      })
      .toBe(seeded.openAsk);
    await page.keyboard.press("k");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the next key does not go on from the question's row",
      })
      .toBe(seeded.resolvedAsk);
  });
});

// c-box (5). The tester's F2 and F9, and the architect's ruling 5: empty and at rest the box is
// one line with Send, as the pictures draw it, at 390 px as at 1440 px, and it says the words go
// on the issue (R3, `issue_placeholder`, in its phone form at 760 px and below); while it replies
// its placeholder says where the words go; and on a phone the box that answers a question has the
// line's width, with the options under it.
// SLOW(>=2s): two cold loads, at 1440 px and at 390 px, each with a reply and an answer line
// opened and left again.
test("the box at the foot is one line at rest, and says where a reply goes @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      const at = `${size.width}x${size.height}`;
      const phone = size.width <= PHONE_FORM_MAX;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(composerField(page), `the box is not drawn at ${at}`).toBeVisible();
      // At rest: no caret in it. The page opens with the focus on a row of the outline.
      await expect.poll(async () => (await focusedNodeId(page)) !== null).toBe(true);
      const box = await formBox(page, `at ${at}`);
      console.log(`the box at rest at ${at}: ${box.width.toFixed(0)} x ${box.height.toFixed(0)}`);
      expect(
        box.height,
        `the empty box at rest is ${box.height.toFixed(0)} px tall at ${at}, where the picture draws one line, 38 px`
      ).toBeLessThanOrEqual(40);
      await expect(composerField(page)).toHaveAttribute(
        "placeholder",
        phone ? CONTROL_LABELS.issue_placeholder_phone : CONTROL_LABELS.issue_placeholder
      );
      // The box stands at the foot of the screen.
      const foot = await composer(page).boundingBox();
      expect(
        Math.round((foot?.y ?? 0) + (foot?.height ?? 0)),
        `the box does not stand at the foot of the screen at ${at}`
      ).toBe(size.height);
      // Standing there, it publishes how far up from the screen's foot it reaches, which the
      // shell's status corner rides above (`--foot-composer-inset`, `useFootInset`): the reach of
      // its wrapper, the element its host marks as held at the foot (`data-foot-held`).
      const wrapper = await page.locator("[data-foot-held]").boundingBox();
      const reach = size.height - (wrapper?.y ?? size.height);
      await expect
        .poll(
          async () =>
            Number.parseFloat(
              await page.evaluate(() =>
                getComputedStyle(document.documentElement).getPropertyValue("--foot-composer-inset")
              )
            ),
          { message: `the box does not publish its reach of ${reach.toFixed(1)} px at ${at}` }
        )
        .toBeCloseTo(reach, 0);

      // While it replies, its placeholder says where the words go.
      await focusRow(page, seeded.head);
      await page.keyboard.press("r");
      await expect(composerField(page)).toBeFocused();
      await expect(
        composerField(page),
        `while replying, the box still says its words go on the issue at ${at}`
      ).toHaveAttribute(
        "placeholder",
        phone ? CONTROL_LABELS.reply_placeholder_phone : CONTROL_LABELS.reply_placeholder
      );
      await page.keyboard.press("Escape");

      // The box that answers a question: its field has room to type in.
      await focusRow(page, seeded.openAsk);
      await page.keyboard.press("a");
      await expect(composerField(page)).toBeFocused();
      // The field is measured once it holds the caret (the wait above), because the row reads the
      // width it is drawn at for typing.
      const field = await composerField(page).boundingBox();
      console.log(
        `the answer field at ${at}: ${field?.width.toFixed(0)} x ${field?.height.toFixed(0)}`
      );
      expect(
        field?.width ?? 0,
        `the answer's field is ${field?.width.toFixed(0)} px wide at ${at}`
      ).toBeGreaterThanOrEqual(240);
      await page.keyboard.press("Escape");
      await expect
        .poll(() => focusedNodeId(page), {
          message: `Escape from the answering box does not give the question's row back at ${at}`,
        })
        .toBe(seeded.openAsk);
    }
  });
});

// c-box (6). R1, R2 and R6, the tester's F9 and F13, and decision 72 of the parts: on an outline
// that runs past the screen the box stays at the foot, as wide as the outline (10 px from each
// side at 760 px and below), and the focused last row ends above it; at rest its line is 26 px
// and each placeholder fits it whole; it grows to 150 px and then scrolls inside itself, and on a
// phone a draft that wraps fits the field again once the focus leaves it; Tab reaches the line,
// each kind and Send; each control is at least 24 by 24 px; and Enter sends a comment and a
// question, each one new row by the reader.
// SLOW(>=2s): an outline of forty-one rows loaded at four widths, each read from rest to two
// sends, with the answer line and a reply opened at the two widths the check names.
test("the box stays at the foot, grows as it is typed and writes both kinds @phone", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seedFortyComments, async (page, seeded) => {
    const last = seeded.comments.at(-1) ?? "";
    for (const size of [DESKTOP, PHONE]) {
      const at = `at ${size.width}x${size.height}`;
      const phone = size.width <= PHONE_FORM_MAX;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(composerField(page), `the box is not drawn ${at}`).toBeVisible();
      await expect(nodeRow(page, last), `the outline does not draw its last row ${at}`).toHaveCount(
        1
      );
      await expect
        .poll(() => focusedNodeId(page), {
          message: `the page opens with the focus on no row ${at}`,
        })
        .not.toBeNull();

      // Where it stands, before the reader moves.
      await expectFormAtFoot(page, `${at}, at load`);
      if (phone) await expectFormInset(page, at);
      else await expectFormOnOutline(page, at);

      // At rest: one line 26 px tall, which says where the words go and fits what it says.
      const field = composerField(page);
      // The line at rest is measured once, at the instant the form above was measured on the
      // outline, because the row reads the height it rests at.
      const rest = await field.boundingBox();
      expect(
        Math.abs((rest?.height ?? 0) - LINE_AT_REST),
        `the empty line is ${rest?.height.toFixed(1)} px tall ${at}`
      ).toBeLessThanOrEqual(0.5);
      await expect(field).toHaveAttribute(
        "placeholder",
        phone ? CONTROL_LABELS.issue_placeholder_phone : CONTROL_LABELS.issue_placeholder
      );
      if (phone) {
        expect(
          rest?.width ?? 0,
          `the field at rest is ${rest?.width.toFixed(0)} px wide ${at}`
        ).toBeGreaterThanOrEqual(240);
        for (const words of [
          CONTROL_LABELS.issue_placeholder_phone,
          CONTROL_LABELS.reply_placeholder_phone,
          CONTROL_LABELS.answer_placeholder_phone,
          CONTROL_LABELS.edit_placeholder_phone,
        ]) {
          const measured = await wordsInField(field, words);
          expect(
            measured.width,
            `"${words}" is ${measured.width.toFixed(1)} px in the field's font, and the field at rest draws ${measured.room.toFixed(1)} px ${at}`
          ).toBeLessThanOrEqual(measured.room);
        }
      }

      // Comment and Question, Comment pressed: on a phone drawn only once the box has the focus.
      for (const kind of ["comment", "ask"] as const) {
        if (phone) {
          await expect(composerKind(page, kind), `a kind is drawn at rest ${at}`).toBeHidden();
        } else {
          await expect(composerKind(page, kind), `a kind is not drawn ${at}`).toBeVisible();
        }
      }
      await expect(composerKind(page, "comment")).toHaveAttribute("aria-pressed", "true");
      await expect(composerKind(page, "ask")).toHaveAttribute("aria-pressed", "false");

      // End takes the reader to the outline's last row - the forty comments' last, or what an
      // earlier width sent after it: it ends above the box, which stays at the foot.
      const end = (await drawnRows(page)).at(-1)?.id ?? null;
      await page.keyboard.press("End");
      await expect
        .poll(() => focusedNodeId(page), { message: `End does not focus the last row ${at}` })
        .toBe(end);
      await expectFormAtFoot(page, `${at}, after End`);
      await expect
        .poll(() => clearOfBox(nodeRow(page, end ?? "")), {
          message: `the focused last row does not end above the box ${at}`,
        })
        .toBe(true);

      // Tab from the focused row reaches the line, Comment, Question and Send, in that order.
      for (let pressed = 0; pressed < 30; pressed += 1) {
        // A press of Tab moves the focus before it returns, so the focus read here is where the
        // last press put it.
        const inBox = await composer(page).evaluate((section) =>
          section.contains(document.activeElement)
        );
        if (inBox) break;
        await page.keyboard.press("Tab");
      }
      await expect(
        field,
        `Tab from the focused row does not reach the line first ${at}`
      ).toBeFocused();
      for (const kind of ["comment", "ask"] as const) {
        await expect(
          composerKind(page, kind),
          `a kind is not drawn while the box has the focus ${at}`
        ).toBeVisible();
      }
      const send = composer(page).getByRole("button", {
        exact: true,
        name: CONTROL_LABELS.send,
      });
      for (const [next, name] of [
        [composerKind(page, "comment"), "Comment"],
        [composerKind(page, "ask"), "Question"],
        [send, "Send"],
      ] as const satisfies readonly (readonly [Locator, string])[]) {
        await page.keyboard.press("Tab");
        await expect(next, `Tab does not go on to ${name} ${at}`).toBeFocused();
      }
      await expectControlsLeast(page, `${at}, writing on the issue`);
      // A send refused and its controls, read where nothing else lifts them to 24 px.
      if (!phone) await expectRefusalControlsLeast(page, seeded.key, size, at);

      // Ten lines typed with Shift+Enter: the line grows to 150 px and scrolls inside itself.
      await field.focus();
      for (let line = 1; line <= 10; line += 1) {
        if (line > 1) await page.keyboard.press("Shift+Enter");
        await page.keyboard.type(`Line ${line} of ten.`);
      }
      const grown = await field.evaluate((element) => ({
        client: element.clientHeight,
        height: element.getBoundingClientRect().height,
        scroll: element.scrollHeight,
      }));
      expect(
        Math.abs(grown.height - LINE_AT_MOST),
        `the line of ten lines is ${grown.height.toFixed(1)} px tall ${at}`
      ).toBeLessThanOrEqual(1);
      expect(
        grown.scroll,
        `the line of ten lines does not scroll inside itself ${at}`
      ).toBeGreaterThan(grown.client);
      await field.fill("");

      // On a phone the field has the line's whole width while the box holds the focus and shares
      // the line with Send once the focus leaves, so a draft that wraps takes more lines after
      // blur: under 150 px the field grows to them rather than scrolling inside itself (R6).
      if (phone) {
        await page.keyboard.type(
          "A draft that wraps over three lines or more while the box holds the focus, and over " +
            "more of them once the reader goes back to the outline and the field shares its " +
            "line with Send."
        );
        const fitted = await field.evaluate((element) => ({
          height: element.getBoundingClientRect().height,
          width: element.clientWidth,
        }));
        await focusRow(page, last);
        await expect
          .poll(() => field.evaluate((element) => element.scrollHeight - element.clientHeight), {
            message: `a draft fitted while the box held the focus scrolls inside the field after blur ${at}`,
          })
          .toBeLessThanOrEqual(1);
        const blurred = await field.evaluate((element) => ({
          height: element.getBoundingClientRect().height,
          width: element.clientWidth,
        }));
        expect(
          blurred.width,
          `the field is no narrower once the box has lost the focus ${at}`
        ).toBeLessThan(fitted.width);
        expect(
          blurred.height,
          `the draft is ${fitted.height.toFixed(1)} px tall with the focus and ${blurred.height.toFixed(1)} px after blur ${at}, so it takes no more lines after blur`
        ).toBeGreaterThan(fitted.height + 1);
        expect(
          blurred.height,
          `the draft after blur is ${blurred.height.toFixed(1)} px tall ${at}, past what the field holds unscrolled`
        ).toBeLessThan(LINE_AT_MOST);
        // Send pressed once with the pointer, while a row of the outline holds the focus, sends
        // the draft: a press on the box's controls leaves the focus where it is, so the line
        // keeps the form it has without the focus and Send stays under the pointer.
        const draft = await field.inputValue();
        await composer(page)
          .getByRole("button", { exact: true, name: CONTROL_LABELS.send })
          .click();
        await oneRowHolding(page, draft, `the draft sent with one press of Send ${at}`);
      }

      // Enter sends a comment, then a question: each one new row by the reader.
      const commentWords = `A comment from the box ${at}.`;
      await field.fill(commentWords);
      await field.press("Enter");
      const comment = await oneRowHolding(page, commentWords, `the comment sent ${at}`);
      const questionWords = `A question from the box ${at}?`;
      await field.click();
      await composerKind(page, "ask").click();
      await expect(composerKind(page, "ask")).toHaveAttribute("aria-pressed", "true");
      await field.fill(questionWords);
      await field.press("Enter");
      const question = await oneRowHolding(page, questionWords, `the question sent ${at}`);
      const rows = await drawnRows(page);
      expect(
        [rowOf(rows, comment), rowOf(rows, question)].map((row) => [row?.type, row?.author]),
        `the comment and the question sent ${at} are not the reader's comment and question`
      ).toEqual([
        ["comment", READER],
        ["ask", READER],
      ]);

      // A reply draws no kind, and each of its controls is 24 by 24 px.
      await focusRow(page, last);
      await page.keyboard.press("r");
      await expect(field, `r put no caret in the box ${at}`).toBeFocused();
      await expect(
        composer(page).locator("[data-composer-kind]"),
        `a reply draws a kind ${at}`
      ).toHaveCount(0);
      await expectControlsLeast(page, `${at}, replying`);
      await page.keyboard.press("Escape");

      // So is each control of the answer line.
      await focusRow(page, seeded.question);
      await page.keyboard.press("a");
      await expect(composerField(page), `a put no caret in the box ${at}`).toBeFocused();
      await expectControlsLeast(page, `${at}, answering`);
      await page.keyboard.press("Escape");
      await expect
        .poll(() => focusedNodeId(page), {
          message: `Escape from the answer line does not give the question's row back ${at}`,
        })
        .toBe(seeded.question);
    }

    // Decision 72: 760 px is the phone's form - 10 px from each side, the kinds drawn only once
    // the box has the focus - and 761 px the desktop's.
    await page.setViewportSize({ height: 900, width: PHONE_FORM_MAX });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(composerField(page)).toBeVisible();
    await expectFormInset(page, "at 760x900");
    await expect(composerKind(page, "comment"), "a kind is drawn at rest at 760x900").toBeHidden();
    await expect(composerKind(page, "ask"), "a kind is drawn at rest at 760x900").toBeHidden();
    await page.setViewportSize({ height: 900, width: PHONE_FORM_MAX + 1 });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(composerField(page)).toBeVisible();
    await expectFormOnOutline(page, "at 761x900");
    await expect(composerKind(page, "comment"), "a kind is not drawn at 761x900").toBeVisible();
    await expect(composerKind(page, "ask"), "a kind is not drawn at 761x900").toBeVisible();
  });
});

// The box stands 8 px above the screen's foot in every state it rests in, and the
// reach it publishes for the focused row and the read mark is its own height. A box that grows
// and then shrinks while the page stands at the outline's end is the case to read: a reply aimed
// at the last row and ended with the target line's own control, the pointer's way out of a reply.
// SLOW(>=2s): forty comments seeded, a reply aimed and ended, and the page scrolled to its end
// twice, each place polled until the box has settled.
test("the box that shrinks at the outline's end stands 8 px above the foot again", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedFortyComments, async (page, seeded) => {
    const at = `at ${DESKTOP.width}x${DESKTOP.height}`;
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(composerField(page), `the box is not drawn ${at}`).toBeVisible();
    const last = (await drawnRows(page)).at(-1)?.id ?? "";
    await focusRow(page, last);
    await page.keyboard.press("r");
    await expect(
      composerTarget(page),
      `r on the last row draws no target line ${at}`
    ).toBeVisible();

    // The reader scrolls to the page's end with the reply open, and ends the reply with the
    // target line's control.
    const toTheEnd = async () => {
      await page.mouse.move(DESKTOP.width / 2, DESKTOP.height / 2);
      await page.mouse.wheel(0, 100_000);
      await expect
        .poll(() =>
          page.evaluate(
            () => document.documentElement.scrollHeight - window.innerHeight - window.scrollY
          )
        )
        .toBeLessThanOrEqual(1);
    };
    await toTheEnd();
    await composer(page).getByRole("button", { name: CONTROL_LABELS.write_on_issue }).click();
    await expect(composerTarget(page), `the reply did not end ${at}`).toHaveCount(0);

    // The reach the box publishes is its own height once it has shrunk back to one line, and
    // the line stands 8 px above the foot at the outline's end.
    const reach = () =>
      composer(page).evaluate((section) => ({
        height: Math.round(section.getBoundingClientRect().height),
        published: Math.round(
          Number.parseFloat(
            document.documentElement.style.getPropertyValue("--issue-composer-height")
          )
        ),
      }));
    await expect
      .poll(async () => (await reach()).height, { message: `the box is not one line again ${at}` })
      .toBeLessThan(60);
    await toTheEnd();
    await expectFormAtFoot(page, `${at}, at the outline's end after the reply ended`);
    const settled = await reach();
    expect(
      settled.published,
      `the box publishes a reach of ${settled.published} px while it is ${settled.height} px tall ${at}`
    ).toBe(settled.height);
  });
});

// The answer line's options, Decline to answer and Send answer stand inside the box and inside
// the screen at every width, however many options the question offers: on a phone the controls
// stand under the field, and a row of them that is wider than the line wraps. A question of four
// options whose labels are a few words each, as the questions on real issues are, is the case to
// read.
// SLOW(>=2s): forty comments and a question of four options seeded, and the answer line opened
// at two widths.
test("the answer line's options and Send answer stand inside the box and the screen @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seed = async () => {
    const forty = await seedFortyComments();
    const question = await createAsk(
      forty.key,
      {
        options: [
          { label: "B, a document switcher (recommended)" },
          { label: "A, one scroll with no switcher" },
          { label: "C, four tabs, Decisions first, as your message named" },
          { label: "Other" },
        ],
        question: "Which layout does the issue page take?",
      },
      { actor: { id: "e2e-box-asker", kind: "session" as const }, as: "agent" as const }
    );
    return { ...forty, fourOptions: question.id };
  };
  await phase(browser, seed, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      const at = `at ${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await expect(composerField(page), `the box is not drawn ${at}`).toBeVisible();
      await focusRow(page, seeded.fourOptions);
      await page.keyboard.press("a");
      await expect(composerField(page), `a put no caret in the box ${at}`).toBeFocused();
      await expect(
        composer(page).locator("[data-answer-option]"),
        `the answer line draws no option ${at}`
      ).toHaveCount(4);

      // Each control the answer line draws ends inside the box and inside the screen.
      const outside = await composer(page).evaluate((section) => {
        const edge = Math.min(
          section.getBoundingClientRect().right,
          document.documentElement.clientWidth
        );
        return [...section.querySelectorAll<HTMLElement>("button")]
          .filter((control) => control.getClientRects().length > 0)
          .filter((control) => control.getBoundingClientRect().right > edge + 0.5)
          .map((control) =>
            (control.getAttribute("aria-label") ?? control.textContent ?? "").trim()
          );
      });
      expect(outside, `a control of the answer line ends past the box or the screen ${at}`).toEqual(
        []
      );
      const sideways = await page.evaluate(
        () => document.documentElement.scrollWidth - document.documentElement.clientWidth
      );
      expect(sideways, `the answer line makes the page scroll sideways ${at}`).toBeLessThanOrEqual(
        0
      );
      await page.keyboard.press("Escape");
    }
  });
});

// The answer line's field keeps room to type at every width, however much of the line the
// options take: words alone make an answer, and 240 px is the room the box gives the field to
// type in on a phone. A question of three options whose chips with Decline to answer and Send
// answer almost fill the line beside the field is the case to read: the controls then take a
// row of their own under the field rather than leave it a few words wide.
// SLOW(>=2s): forty comments and a question of three options seeded, and the answer line opened.
test("the answer line's field keeps room to type beside options that almost fill the line", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const seed = async () => {
    const forty = await seedFortyComments();
    const question = await createAsk(
      forty.key,
      {
        options: [
          { label: "The first choice of three, long one" },
          { label: "The second choice of three, long" },
          { label: "The third choice, also rather long ones here" },
        ],
        question: "Which of three long choices?",
      },
      { actor: { id: "e2e-box-asker", kind: "session" as const }, as: "agent" as const }
    );
    return { ...forty, threeOptions: question.id };
  };
  await phase(browser, seed, async (page, seeded) => {
    const at = `at ${DESKTOP.width}x${DESKTOP.height}`;
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(composerField(page), `the box is not drawn ${at}`).toBeVisible();
    await focusRow(page, seeded.threeOptions);
    await page.keyboard.press("a");
    const field = composerField(page);
    await expect(field, `a put no caret in the box ${at}`).toBeFocused();
    await expect(
      composer(page).locator("[data-answer-option]"),
      `the answer line draws no option ${at}`
    ).toHaveCount(3);
    const width = (await field.boundingBox())?.width ?? 0;
    expect(
      width,
      `the answer's field is ${width.toFixed(1)} px wide beside the options ${at}`
    ).toBeGreaterThanOrEqual(240);
  });
});

/** The forty comments of `seedFortyComments`, an issue of one comment, whose outline is shorter
 *  than the screen, and an issue with a spec of one statement: each unread for the reader. */
async function seedReadMarks() {
  const forty = await seedFortyComments();
  return {
    forty,
    short: await seedUnreadIssue("One comment, shorter than the screen"),
    spec: await seedUnreadIssue(
      "A spec read whole at its view",
      ":::requirement{#r-one}\nThe spec of this issue is read whole at its view.\n:::\n"
    ),
  };
}

// c-mark (7). R9: the end of the outline, three quarters on screen for a second, marks the issue
// read through its newest event, once; nothing else does - not an outline whose end is off
// screen or under the box, and not a view, whose end is not the conversation's. An outline
// shorter than the screen has its end on screen at load, and is marked with no scroll.
// SLOW(>=2s): five waits of two dwells each for a write that must not come, and two marks polled.
test("the end of the outline on screen for a second marks the issue read", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedReadMarks, async (page, seeded) => {
    // The read mark's write of each issue the row reads (`IssueReadMark.tsx`).
    const stateWrites = (key: string) => watchWrites(page, "PUT", `/api/v1/me/issues/${key}/state`);
    const fortyWrites = stateWrites(seeded.forty.key);
    const shortWrites = stateWrites(seeded.short);
    const specWrites = stateWrites(seeded.spec);
    const newest = async (key: string) =>
      (await getIssueEvents(key, { limit: 1, order: "desc" }))[0]?.seq ?? 0;
    const mark = async (key: string) => (await getMyState())[key]?.last_read_seq ?? 0;

    // The forty comments: the end off screen, then under the box, then on screen.
    const { key } = seeded.forty;
    const last = seeded.forty.comments.at(-1) ?? "";
    const head = await newest(key);
    expect(await mark(key), "the seed left the issue read for the reader").toBeLessThan(head);
    await gotoIssue(page, `/issues/${key}`);
    await expectIssuePageDrawn(page);
    await expect(composerField(page)).toBeVisible();
    await expect(nodeRow(page, last), "the outline does not draw its last row").toHaveCount(1);
    await expect(outlineEnd(page), "the outline draws no read mark").toHaveCount(1);
    expect(
      await onScreen(outlineEnd(page)),
      "the outline's end is on screen at load, so its absence proves nothing"
    ).toBe(false);
    // The read mark must not be written while the outline's end is off screen, and a write that
    // never comes leaves nothing to poll for, so this span is the check: two dwells long, so a
    // dwell begun even as it starts ends at its middle, and its write leaves before its end.
    await page.waitForTimeout(2 * DWELL_MS);
    expect(fortyWrites, "the end of the outline off screen wrote the read mark").toEqual([]);
    expect(await mark(key), "the end of the outline off screen marked the issue read").toBeLessThan(
      head
    );

    // The last row ends under the middle of the box: on screen, under the box, unread.
    await scrollEndUnderBox(page, last);
    const under = await endPlace(page, last);
    expect(
      under !== null && under.lastBottom > under.boxTop && under.lastBottom < under.viewport,
      `the outline's last row does not end under the box: ${JSON.stringify(under)}`
    ).toBe(true);
    // Nor while the end is under the box, which hides it from the reader though the window holds
    // it: the span is the check again, two dwells long, so a dwell begun by the scroll above ends
    // by its middle, and its write leaves before its end.
    await page.waitForTimeout(2 * DWELL_MS);
    expect(fortyWrites, "the end of the outline under the box wrote the read mark").toEqual([]);
    expect(
      await mark(key),
      "the end of the outline under the box marked the issue read"
    ).toBeLessThan(head);

    await page.keyboard.press("End");
    await expect
      .poll(() => mark(key), {
        message: "the end of the outline on screen did not mark the issue read",
        timeout: 3_000,
      })
      .toBe(head);
    // The mark goes out once and never again, and a write that never comes leaves nothing to poll
    // for, so this span is the check: a repeat would leave as the first write's answer is drawn,
    // and a second dwell begun then would write one dwell later, so two dwells see either.
    await page.waitForTimeout(2 * DWELL_MS);
    expect(
      fortyWrites.map((write) => write.input),
      "the end of the outline on screen did not write the mark once"
    ).toEqual([expect.objectContaining({ last_read_seq: head })]);

    // An outline shorter than the screen: its end is on screen at load.
    const short = seeded.short;
    const shortHead = await newest(short);
    expect(await mark(short), "the seed left the short issue read for the reader").toBeLessThan(
      shortHead
    );
    await gotoIssue(page, `/issues/${short}`);
    await expectIssuePageDrawn(page);
    await expect
      .poll(() => mark(short), {
        message: "the end of an outline shorter than the screen did not mark the issue read",
        timeout: 3_000,
      })
      .toBe(shortHead);
    expect(
      await page.evaluate(() => window.scrollY),
      "the page scrolled to mark an outline shorter than the screen read"
    ).toBe(0);
    // Once and never again here too: a repeat would leave as the first write's answer is drawn,
    // and a second dwell begun then would write one dwell later, so two dwells see either.
    await page.waitForTimeout(2 * DWELL_MS);
    expect(shortWrites, "the short outline's end did not write the mark once").toHaveLength(1);

    // A view's end is not the conversation's: the Spec view, whole on screen, marks nothing.
    const spec = seeded.spec;
    const specHead = await newest(spec);
    const before = await mark(spec);
    expect(before, "the seed left the spec's issue read for the reader").toBeLessThan(specHead);
    await gotoIssue(page, `/issues/${spec}/spec`);
    await expectActivePreset(page, PRESET_NAMES.spec);
    // The view's end on screen too: the page is scrolled as far as it goes.
    await page.evaluate(() =>
      window.scrollTo({ behavior: "instant", top: document.documentElement.scrollHeight })
    );
    expect(
      await page.evaluate(
        () => window.scrollY + window.innerHeight >= document.documentElement.scrollHeight - 1
      ),
      "the Spec view is not whole on screen"
    ).toBe(true);
    // The Spec view must not write the read mark, and a write that never comes leaves nothing to
    // poll for, so this span is the check: two dwells long, so a mark drawn at the view's end, on
    // screen since the scroll above, would dwell by its middle and be sent before its end.
    await page.waitForTimeout(2 * DWELL_MS);
    expect(specWrites, "the Spec view wrote the read mark").toEqual([]);
    expect(await mark(spec), "the Spec view marked the issue read").toBe(before);
  });
});

// c-statement-reply, its outline half (R11, decision 74 of the parts): `r` on a focused statement
// aims the box at it, and what is sent is a comment anchored to that statement - its document
// and its block - drawn under it and focused; the box then writes on the issue again. A
// statement's address in its words reads in the target line as its name, never raw. A
// statement of two paragraphs is anchored to the statement itself, and one whose words the spec
// holds twice is refused, in the page's words and with no Retry, the draft kept. The Spec view's
// half is the row after this one, owed on part J (decisions 74 and 61).
// SLOW(>=2s): five writes from the box, each read back from the server and the outline.
test("r on a statement writes a comment under it", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedStatements, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const blocks = await getArtifactBlocks(seeded.specArtifactId);
    for (const one of [
      {
        block: STATEMENT_BLOCKS.cites,
        cited: seeded.cited,
        first: STATEMENT_WORDS.citesLead,
        what: "the statement that cites an issue",
        words: "A comment on the statement that cites an issue.",
      },
      {
        block: STATEMENT_BLOCKS.two,
        first: STATEMENT_WORDS.first,
        not: STATEMENT_WORDS.second,
        what: "the statement of two paragraphs",
        words: "A comment on the statement of two paragraphs.",
      },
    ]) {
      const statement = blockNode(seeded.specSlug, one.block);
      await expect(nodeRow(page, statement), `${one.what} draws no row`).toHaveCount(1);
      await focusRow(page, statement);
      await page.keyboard.press("r");
      await expect(composerField(page), `r on ${one.what} put no caret in the box`).toBeFocused();
      const target = composerTarget(page);
      await expect(target, `r on ${one.what} draws no target line`).toBeVisible();
      await expect(target).toContainText(CONTROL_LABELS.replying_to);
      await expect(target, `the target line does not name ${one.what}'s first line`).toContainText(
        one.first
      );
      if (one.not !== undefined) await expect(target).not.toContainText(one.not);
      // The issue it cites by its address reads as that issue's name, never as the address
      // (R7, the reading list's `issue-page-composer-target`).
      if (one.cited !== undefined) {
        await expect(target, `the target line draws ${one.what}'s address raw`).not.toContainText(
          "dispatch://"
        );
        await expect(
          target,
          `the target line does not name the issue ${one.what} cites`
        ).toContainText(one.cited);
      }
      await page.keyboard.type(one.words);
      await page.keyboard.press("Enter");

      // Stored as a comment anchored to the statement's block, or a block inside its range.
      const comment = await commentWithBody(seeded.key, undefined, one.words);
      expect(
        comment.anchor?.artifact_id,
        `the comment on ${one.what} is not anchored in the spec`
      ).toBe(seeded.specArtifactId);
      const range = blocks.find((block) => block.id === one.block);
      const anchored = blocks.find((block) => block.id === comment.anchor?.block_id);
      expect(
        range !== undefined &&
          anchored !== undefined &&
          anchored.from >= range.from &&
          anchored.to <= range.to,
        `the comment on ${one.what} is anchored to block ${comment.anchor?.block_id}, not inside ${one.block}`
      ).toBe(true);
      // Drawn under the statement, with the focus on it.
      await expect
        .poll(() => focusedNodeId(page), {
          message: `the focus is not on the comment sent on ${one.what}`,
        })
        .toBe(comment.id);
      expect(
        await parentOfRow(page, comment.id),
        `the comment on ${one.what} is not drawn under it`
      ).toBe(statement);

      // The box's next send is a comment on the issue, with no anchor.
      await expect(composerTarget(page), "the box still names the statement").toHaveCount(0);
      await composerField(page).click();
      const next = `${one.words} Then one on the issue.`;
      await sendFromComposer(page, next);
      const onIssue = await commentWithBody(seeded.key, undefined, next);
      expect(
        onIssue.anchor,
        `the send after the comment on ${one.what} carries an anchor`
      ).toBeNull();
      await expect
        .poll(() => parentOfRow(page, onIssue.id), {
          message: `the send after the comment on ${one.what} is not drawn on the issue`,
        })
        .toBeNull();
    }

    // Words the spec holds twice: the server refuses the quote, and the box says why in the
    // page's words - not the server's, which ask for an occurrence no control sends - with no
    // Retry, which would send the same quote to the same refusal; the draft stays.
    const twice = blockNode(seeded.specSlug, STATEMENT_BLOCKS.twice);
    const refused = "A comment on the statement whose words the spec holds twice.";
    await focusRow(page, twice);
    await page.keyboard.press("r");
    await expect(
      composerField(page),
      "r on the twice-held statement put no caret in the box"
    ).toBeFocused();
    await expect(composerTarget(page)).toContainText(STATEMENT_WORDS.twice);
    await page.keyboard.type(refused);
    await page.keyboard.press("Enter");
    const refusal = composer(page).getByRole("alert");
    await expect(
      refusal,
      "the box does not say in the page's words why the twice-held statement's comment was refused"
    ).toContainText(ANSWER_LINES.statement_twice);
    await expect(refusal, "the box draws the server's advice").not.toContainText("occurrence");
    await expect(
      composer(page).getByRole("button", { exact: true, name: "Retry" }),
      "the box offers a Retry of the quote the server refuses"
    ).toHaveCount(0);
    await expect(composerField(page), "the refused comment's draft is gone").toHaveValue(refused);
    await expect(composerTarget(page), "the refused comment's target is gone").toContainText(
      STATEMENT_WORDS.twice
    );
    expect(
      (await listComments(seeded.key)).some((one) => one.body === refused),
      "Dispatch holds the comment it refused"
    ).toBe(false);
  });
});

// c-statement-reply, its Spec view half (R11): `r` on a focused statement of the Spec view aims
// the box at it as on the outline, and what is sent is a comment anchored to that statement.
// Owed on part J (decision 74 of the parts: J passes the writer to the view's keymap; decision
// 61: the check names an owed reading and the part it waits on). Until J is under this part's
// head the Spec view draws no rows (`issue-page-switch.e2e.ts`), so the reading is skipped with
// an annotation naming J and those decisions (decision 127) and tagged `@owed`, which the
// check's run leaves out (decision 135). The round that brings J under the head takes the skip
// and the tag off.
test.skip("r on a statement in the Spec view writes a comment under it @owed", {
  annotation: {
    description:
      "Owed on part J (decisions 74 and 61): the Spec view's rows take the writer's r only once J passes the writer to the view's keymap, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedStatements, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}/spec`);
    await expectActivePreset(page, PRESET_NAMES.spec);
    const statement = blockNode(seeded.specSlug, STATEMENT_BLOCKS.two);
    await expect(
      nodeRow(page, statement),
      "the Spec view draws no row of the statement"
    ).toHaveCount(1);
    await focusRow(page, statement);
    await page.keyboard.press("r");
    await expect(
      composerField(page),
      "r on the Spec view's statement put no caret in the box"
    ).toBeFocused();
    await expect(composerTarget(page)).toContainText(STATEMENT_WORDS.first);
    const words = "A comment on the statement from the Spec view.";
    await page.keyboard.type(words);
    await page.keyboard.press("Enter");
    const comment = await commentWithBody(seeded.key, undefined, words);
    const blocks = await getArtifactBlocks(seeded.specArtifactId);
    const range = blocks.find((block) => block.id === STATEMENT_BLOCKS.two);
    const anchored = blocks.find((block) => block.id === comment.anchor?.block_id);
    expect(
      comment.anchor?.artifact_id === seeded.specArtifactId &&
        range !== undefined &&
        anchored !== undefined &&
        anchored.from >= range.from &&
        anchored.to <= range.to,
      `the comment from the Spec view is anchored to ${JSON.stringify(comment.anchor)}, not inside ${STATEMENT_BLOCKS.two}`
    ).toBe(true);
  });
});

// Tester's finding, round 4 of the test of part L: the foot inset counts a sticky element held at
// the foot, the new page's box. Today's docked composer is sticky too, held at the top from 640 px
// up, and keeps the 64 px `bottom` of its fixed look below 640 px, so a wide window whose foot
// meets its bottom edge counted it, published its whole height and drew the shell's status corner
// above the screen's top edge, which `main` never does (R10).
test("today's docked composer lifts nothing at the foot of a short wide window", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  // An issue of a project of the row's own: the database is empty at each row's start.
  await createProject({ key: "DOCK", name: "Today's docked composer" });
  const { key } = await createIssue({
    force: true,
    project: "DOCK",
    title: "Today's page in a window whose foot meets its composer",
  });
  // Today's page: the reader has not turned the new page on.
  const context = await asUser(browser, READER);
  try {
    const page = await context.newPage();
    await page.setViewportSize(DESKTOP);
    await page.goto(`/issues/${key}/conversation`);
    const docked = page.getByRole("form", { name: "Comment composer" });
    await expect(docked, "today's Conversation tab draws no docked composer").toBeVisible();
    // The window's foot on the composer's bottom edge, which is where the inset reads its box: the
    // window is set to the composer's bottom until the two meet, as a reader's short window does.
    const bottomOf = async () => {
      // The composer was seen drawn by the wait above, and each try of the loop below reads its
      // box where the last resize left it.
      const box = await docked.boundingBox();
      return box === null ? Number.NaN : Math.round(box.y + box.height);
    };
    let bottom = await bottomOf();
    for (
      let tries = 0;
      tries < 3 && Math.abs((page.viewportSize()?.height ?? 0) - bottom) > 1;
      tries++
    ) {
      expect(bottom, "the docked composer's foot is not on a desktop screen").toBeLessThan(
        DESKTOP.height
      );
      await page.setViewportSize({ height: bottom, width: DESKTOP.width });
      await expect
        .poll(bottomOf, { message: "the docked composer moved on the resize" })
        .toBeGreaterThan(0);
      bottom = await bottomOf();
    }
    expect(
      Math.abs((page.viewportSize()?.height ?? 0) - bottom),
      "the window's foot never met the docked composer's bottom edge"
    ).toBeLessThanOrEqual(1);
    const inset = () =>
      page.evaluate(() =>
        getComputedStyle(document.documentElement).getPropertyValue("--foot-composer-inset")
      );
    // Two frames after the window met it: the resize and the composer's size observer have both
    // run by then, so the inset read is the one the shell draws its status corner by.
    await page.evaluate(
      () => new Promise((done) => requestAnimationFrame(() => requestAnimationFrame(done)))
    );
    expect(
      await inset(),
      `today's docked composer published a foot inset at ${DESKTOP.width}x${bottom}`
    ).toMatch(/^(0px)?$/);
  } finally {
    await context.close();
  }
});

// (10) The outline marks the conversation's current point on one row - the node the reader's
// next turn answers - as the current location for a screen reader and with a quiet ring on its
// bullet's dot. On this seed it is the open question at load, whether that question waits on the
// reader or is only the newest act, since the seed asks it last. A Reply moves the mark to the
// row the box is aimed at and Escape moves it back, the focus staying the box's; while the
// point's own row is folded away no row is the current location and the row above it on its path
// carries a lighter ring; and an answer and then a new comment, each the newest act, take the
// mark with them.
// SLOW(>=2s): a seeded thread and three questions, the thread opened press by press twice, two
// replies aimed and ended, and an answer and a comment written from the box, each waited on until
// the outline marks it.
test("the outline marks the current point of the conversation on one row, and the mark follows the box and each new act @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    // At load: the one question waiting on the reader, if the server says it waits on alice, and
    // else the newest act on the issue, which is that question too, asked after every other node.
    await expectPointOn(page, seeded.openAsk, "the open question's row", "at load");
    // The probe reads what the page's own values compute to, which no state of the page moves, so
    // one reading holds for the whole row.
    const pointRing = await owedRing(page, POINT_RING);
    await expect
      .poll(() => dotRing(nodeRow(page, seeded.openAsk).first()), {
        message:
          "the open question's dot is not ringed as the point's own row: a solid 1.5 px line 2 px off it, in the page's quiet grey",
      })
      .toEqual(pointRing);

    // A Reply aims the box at the thread's head: the mark moves to its row, and the focus is the
    // box's, where the Reply put it, and not the mark's. Escape in the box ends the reply, and the
    // mark goes back to the open question.
    await focusRow(page, seeded.head);
    await page.keyboard.press("r");
    await expect(
      composerField(page),
      "r on the thread's head put no caret in the box"
    ).toBeFocused();
    await expectPointOn(page, seeded.head, "the thread's head", "while the box replies to it");
    await expect(
      nodeRow(page, seeded.openAsk).first(),
      "the open question's row is still the current location while the box replies to the thread's head"
    ).not.toHaveAttribute("aria-current");
    await expect(composerField(page), "the mark took the focus from the box").toBeFocused();
    await page.keyboard.press("Escape");
    await expect(composerTarget(page), "Escape in the box did not end the reply").toHaveCount(0);
    await expectPointOn(
      page,
      seeded.openAsk,
      "the open question's row",
      "once Escape ended the reply to the thread's head"
    );

    /** Opens the thread's folds by their fold controls and the line its fourth level is folded
     *  to at this width, as the first row of this file does, so `third`'s row is drawn. */
    const openThread = async (): Promise<void> => {
      const folds = nodeRows(page).getByRole("button", {
        exact: true,
        name: CONTROL_LABELS.unfold,
      });
      // Each press redraws before the next count, so a count of none means every fold is open.
      for (let pressed = 0; pressed < 20 && (await folds.count()) > 0; pressed += 1) {
        await folds.first().click();
      }
      await openEveryDeeperBranch(page);
      await expect(
        nodeRow(page, seeded.third),
        "the thread's fourth level is not drawn once its branches are open"
      ).not.toHaveCount(0);
    };

    // The folded path: with the box replying to the fourth level, folding the thread's head folds
    // the point's own row away. No row is then the current location, and the head, the one row
    // of its path still drawn, carries the path's lighter ring, outside its folded halo.
    await openThread();
    await focusRow(page, seeded.third);
    await page.keyboard.press("r");
    await expect(
      composerField(page),
      "r on the fourth level's reply put no caret in the box"
    ).toBeFocused();
    await expectPointOn(
      page,
      seeded.third,
      "the fourth level's row",
      "while the box replies to it"
    );
    const head = nodeRow(page, seeded.head).first();
    await head.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
    await expect(
      nodeRow(page, seeded.third),
      "folding the thread's head left its fourth level drawn"
    ).toHaveCount(0);
    await expect(
      currentRows(page),
      "a row is the current location while the point's own row is folded away"
    ).toHaveCount(0);
    await expect(
      head,
      "the thread's head does not carry the path's mark while the point is folded away under it"
    ).toHaveAttribute("data-current-point", "path");
    await expect(
      markedRows(page),
      "a row besides the thread's head carries a mark while the point is folded away under it"
    ).toHaveCount(1);
    // Read once, as the point's ring above is: the page's own values, which no state moves.
    const pathRing = await owedRing(page, PATH_RING_FOLDED);
    await expect
      .poll(() => dotRing(head), {
        message:
          "the folded head's dot is not ringed as a row on the point's path: a solid 1 px line 5 px off it, outside the fold's halo, in the bullets' grey",
      })
      .toEqual(pathRing);
    // Unfolded again, the point's own row is drawn and carries the mark once more.
    await head.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
    await openThread();
    await expectPointOn(
      page,
      seeded.third,
      "the fourth level's row",
      "once the thread's head is unfolded again"
    );
    // A press on the deeper line's control, where that line came back, takes the focus from the
    // box, as a press on a fold control does not, so the caret is put back in the box for Escape.
    await composerField(page).focus();
    await page.keyboard.press("Escape");
    await expect(
      composerTarget(page),
      "Escape in the box did not end the reply to the fourth level"
    ).toHaveCount(0);
    await expectPointOn(
      page,
      seeded.openAsk,
      "the open question's row",
      "once Escape ended the reply to the fourth level"
    );

    // A new act moves the mark: the answer sent from the open question's row is the newest act,
    // on that question's own row, and no question waits once it is answered; then a comment on
    // the issue is the newest act, and the mark is on its row alone.
    await focusRow(page, seeded.openAsk);
    await nodeRow(page, seeded.openAsk).first().locator('[data-pick-option="Left"]').click();
    await composer(page).getByRole("button", { name: CONTROL_LABELS.send_answer }).click();
    await expect(
      nodeRow(page, seeded.openAsk).first(),
      "the answer sent from the open question's row did not land"
    ).toHaveAttribute("data-node-state", "answered");
    await expectPointOn(
      page,
      seeded.openAsk,
      "the open question's row",
      "once its answer, the newest act, landed"
    );
    const words = "A comment on the issue, the newest act of all.";
    await sendFromComposer(page, words);
    const comment = (await commentWithBody(seeded.key, undefined, words)).id;
    await expect(nodeRow(page, comment), "the new comment is not drawn").not.toHaveCount(0);
    await expectPointOn(page, comment, "the new comment's row", "once it landed on the issue");
  });
});

// (11) A row that holds the focus and leaves the tree hands the focus on only where the reader
// caused it to leave, since the page would otherwise choose the reader's next place for them.
// This row is the half the reader causes: a key of the reader's that takes the focused row out of
// the view hands the focus to the nearest row still drawn, as a key's move gives it - the row now
// at the dropped row's place among the rows the tree draws, or the last row where the tree now
// ends before that place. The key is the cycle key `z`, pressed in its last state, which draws
// every branch, to go back to its first, which draws the reader's own folds: the thread's fourth
// level, which holds the focus, is folded away under the thread. The other half, a read the
// reader did nothing toward, is the row of `issue-page-i.e2e.ts` that leaves the focus on the
// page's body.
// SLOW(>=2s): a seeded thread and three questions, the tree cycled to every branch and back.
test("a key of the reader's that drops the focused row from the view hands the focus to the nearest row", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await focusRow(page, seeded.head);
    // The cycle's last state draws every branch, the thread's fourth level among them.
    await page.keyboard.press("z");
    await page.keyboard.press("z");
    await expect(
      nodeRow(page, seeded.third),
      "the cycle's last state does not draw the thread's fourth level"
    ).not.toHaveCount(0);
    await focusRow(page, seeded.third);
    /** The keys of the rows the tree draws, in order, and the key of the one that holds the
     *  focus, or null where no row of the tree holds it. */
    const drawn = () =>
      tree(page).evaluate((outline) => {
        const focused = document.activeElement?.closest<HTMLElement>("[data-node-row]");
        return {
          focused:
            focused !== null && focused !== undefined && outline.contains(focused)
              ? (focused.dataset.rowKey ?? null)
              : null,
          keys: [...outline.querySelectorAll<HTMLElement>("[data-node-row]")].map(
            (row) => row.dataset.rowKey ?? ""
          ),
        };
      });
    const before = await drawn();
    const place = before.focused === null ? -1 : before.keys.indexOf(before.focused);
    expect(place, "the thread's fourth level does not hold the focus").not.toBe(-1);

    await page.keyboard.press("z");
    await expect(
      nodeRow(page, seeded.third),
      "the cycle's first state still draws the thread's fourth level"
    ).toHaveCount(0);
    await expect
      .poll(
        async () => {
          const after = await drawn();
          const nearest = after.keys[Math.min(place, after.keys.length - 1)];
          return after.focused !== null && after.focused === nearest;
        },
        {
          message:
            "the key that dropped the focused row left the focus on no row of the tree, or not on the row now at its place or the last row",
        }
      )
      .toBe(true);
  });
});
