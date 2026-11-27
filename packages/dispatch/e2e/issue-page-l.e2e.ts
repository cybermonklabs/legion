/** Each row writes from the box at the foot and then reads its own write: the comment, reply or
 *  read mark Dispatch stored, and the row the outline draws from the server's reads. */
import type { Locator, Page } from "@playwright/test";

import type { ArtifactBlock } from "../web/src/api/types";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  CURRENT_POINT,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
} from "../web/src/features/issue-page/page-words";
import { DWELL_MS } from "../web/src/features/shell/useDwell";
import {
  answerAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifactBlocks,
  getIssueEvents,
  getMyState,
  listComments,
} from "./api";
import { updateBanner } from "./foot-floats";
import { nextFrames } from "./frames";
import { seedReplyNesting } from "./issue-layout-seed";
import { layoutProject } from "./issue-layout-seed-support";
import { closeDetails, gotoIssue, openDetails, phase } from "./issue-page";
import {
  blockNode,
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
  focusRow,
  issuePage,
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
  focusInside,
  onScreen,
  openEveryDeeperBranch,
  pickPreset,
} from "./issue-page-i";
import {
  CITED_TITLE,
  citesWords,
  clearOfBox,
  composer,
  composerField,
  composerForm,
  composerKind,
  composerTarget,
  endPlace,
  outlineEnd,
  panelEntry,
  placesHolding,
  replyFromRow,
  STATEMENT_BLOCKS,
  STATEMENT_WORDS,
  type StatementsSeed,
  scrollEndUnderBox,
  seedFortyComments,
  seedHistoryTurns,
  seedPastFifty,
  seedResultAfterAct,
  seedStatements,
  seedUnreadIssue,
  sendFromComposer,
  sendWithKeys,
  wordsInField,
} from "./issue-page-l";
import { withIssuePage } from "./issue-page-option";
import { runClockUntil, watchWrites } from "./keyed-writes";
import { commentWithBody } from "./margin-helpers";
import { keyedSearchWord } from "./search";
import { expect, rowKey, test } from "./test";
import { asUser } from "./users";

/**
 * Writing on the new issue page: the page's one
 * box to write in, held at the foot of the screen; a reply drawn under what it answers; and the
 * read mark at the outline's end. Each row is one check of
 * the verification document, named by its `-g` string: rows (1) to (4) are
 * c-replies, (5) and (6) c-box, (7) c-mark, (8) the outline's half of c-statement-reply, and (9)
 * its Spec view half. Row (10) is R12's: the conversation's current point, which the outline
 * marks on one row (`r-current-point` of the spec). Row (11) is R13's half of c-outline-focus that
 * the reader causes, beside the row of `issue-page-i.e2e.ts` for the other half. A reading that a
 * check owes on a part not yet under this part's head is a row of its own beside its check's
 * rows, skipped with an annotation naming that part and tagged `@owed`: four of c-replies, one of
 * c-box, one of c-mark, two of c-statement-reply, (9) among them, and one of the current point.
 *
 * The rows are on the real stack, each through `phase` (a seed under the row's own keys beside
 * what the rows before it wrote, alice signed in, the new page turned on with `withIssuePage`):
 * every row but (11), which writes nothing, writes from the box and then reads its own write, as
 * Dispatch stored it and as the outline draws it from the server's reads. Rows (1) to (5) are the
 * checkpoint's
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
 * decision 43 of the parts). Read on a page of its own, signed in as the row's reader, whose
 * comment sends Dispatch answers `500` every time: a 5xx is no verdict, so the send is tried again
 * under its key until its deadline, goes late, and the reader stops it (`Stop trying`). That page's
 * clock runs past the deadline (`runClockUntil`), which costs no real time. A page's clock is its
 * browser context's, every page of the context and every load of one alike, so the page is in a
 * context of its own, and the page this row reads keeps the browser's own clock.
 */
async function expectRefusalControlsLeast(
  page: Page,
  key: string,
  size: { readonly height: number; readonly width: number },
  at: string
): Promise<void> {
  const browser = page.context().browser();
  if (browser === null) throw new Error("the row's page has no browser to open a context in");
  const context = await asUser(browser, READER);
  try {
    await withIssuePage(context, READER);
    const refusing = await context.newPage();
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
    await context.close();
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
// that takes a reply draws the control; a reply from the box lands under its target; and a
// comment with no target is drawn at the outline's top level, where the issue's own nodes are.
// On a phone a branch below the first level is one line counting its nodes, which opens them in
// place.
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

    // The box at the foot posts a comment drawn at the outline's top level, where the issue's
    // own nodes are: the issue itself is no row.
    const instead = composer(page).getByRole("button", { name: CONTROL_LABELS.write_on_issue });
    // The reply that landed above ended the line's target, so the box holds no reply and offers
    // no way back to the issue.
    await expect(instead, "the box still holds a target after the reply landed").toHaveCount(0);
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

// The tester's red row (round 15), R4 and c-use step 4: Reply on an agent's reply writes a mention
// of that agent into the box (`replyPrefill`), and a person types their words at once after it,
// as a person does. Dispatch stores those words as the reply, and the outline draws it under the
// agent's reply, with no refusal in the box. A send that `fill`s the field writes over the
// mention, so the rows above never type after it.
test("a reply typed at once after the mention the box writes for an agent is stored and drawn under the agent's reply", async ({
  browser,
}) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    const agents = await createComment(
      seeded.key,
      { body: "An agent's answer to the first entry.", reply_to: seeded.head },
      { actor: { id: "e2e-nesting-asker", kind: "session" }, as: "agent" }
    );
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    await openEveryDeeperBranch(page);
    const row = await focusRow(page, agents.id);
    await rowAct(row, "reply").click();
    const field = composerField(page).first();
    await expect(field, "Reply on the agent's reply put no caret in the box").toBeFocused();
    await expect(field, "the box wrote no mention of the agent").toHaveValue(
      /dispatch:\/\/agent\/e2e-nesting-asker/
    );
    const words = "Typed at once after the mention the box wrote.";
    await page.keyboard.press("End");
    await page.keyboard.type(words);
    await page.keyboard.press("Enter");
    await expect
      .poll(
        async () =>
          (await listComments(seeded.key)).find((comment) => comment.body.includes(words))?.id ??
          null,
        { message: "Dispatch stored no reply holding the words typed at once after the mention" }
      )
      .not.toBeNull();
    await expect(
      composer(page).getByRole("alert"),
      "the box shows a refusal of the reply typed at once after the mention"
    ).toHaveCount(0);
    const reply = (await listComments(seeded.key)).find((comment) => comment.body.includes(words));
    await expect.poll(() => parentOfRow(page, reply?.id ?? "")).toBe(agents.id);
  });
});

// c-replies (3). R4, R16 and the tester's F1: what the reader sends from the box at the foot is
// drawn at once, its branch unfolded, on screen clear of the box and with the focus on it,
// whether it is a reply under a thread the reader folded with its head's own fold control, a
// reply under an answered question, or a new comment or question on the issue, which both land
// at the outline's end. The page folds nothing itself, but the outline it draws now still opens
// the thread folded, so the row first unfolds it and then folds it with its head's control.
// SLOW(>=2s): the thread unfolded and folded again, and four writes from the box, each waited on
// until the outline draws and focuses it.
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

    // A reply under a thread the reader folded with its head's own fold control: `r`, the words,
    // Enter. The outline opens the thread folded, so its head unfolds it first.
    const head = nodeRow(page, seeded.head).first();
    await head.getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }).click();
    await expect(
      nodeRow(page, seeded.first),
      "the thread's head draws no reply once its fold control unfolds it"
    ).not.toHaveCount(0);
    await head.getByRole("button", { exact: true, name: CONTROL_LABELS.fold }).click();
    await expect(
      nodeRow(page, seeded.first),
      "the thread is open after its head's fold control, so a reply under it proves nothing about the fold"
    ).toHaveCount(0);
    await sendWithKeys(page, seeded.head, "A reply under the folded thread.");
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
    await sendWithKeys(page, seeded.answeredAsk, "A word under the answered question.");
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

// c-replies (4). The tester's F10: a send made with the keyboard alone - `r` on a row, the words,
// Enter - leaves the focus on a row of the outline, the new reply's own, and the next key goes on
// from there, not from the outline's first row, where a page with nothing focused starts.
// SLOW(>=2s): a seeded thread and three questions, and a reply written from the box with the keys
// alone and waited on until the outline draws it and gives its row the focus.
test("a send with the keyboard leaves the focus on the reader's row of the outline", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    // The answered question is the outline's second row: the thread's head is its first.
    const words = "A reply sent with the keys alone.";
    await sendWithKeys(page, seeded.answeredAsk, words);
    const reply = (await commentWithBody(seeded.key, undefined, words)).id;
    await expect
      .poll(() => focusedNodeId(page), {
        message: "after a reply sent with the keyboard the focus is not on the new reply's row",
      })
      .toBe(reply);
    expect(
      await parentOfRow(page, reply),
      "the reply is not drawn under the question it answers"
    ).toBe(seeded.answeredAsk);
    // Up from the new row is the question it answers, the row above it.
    await page.keyboard.press("k");
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the next key does not go on from the new reply's row",
      })
      .toBe(seeded.answeredAsk);
  });
});

// c-replies (3), owed on part T (root R40, decision 127): a new comment on an issue of more than
// 50 top-level rows stands past the first 50 the outline draws, behind the control for the rest,
// and the box takes the reader to it, drawn, on screen clear of the box and focused. Until part T
// pages the outline it draws every row, so the reading is skipped with an annotation naming T and
// tagged `@owed`, which the check's run leaves out (decision 135). The round that brings T under
// this part's head takes the skip and the tag off.
test.skip("what the reader sends from the box past the first 50 rows is drawn, in view and focused @owed", {
  annotation: {
    description:
      "Owed on part T (decision 127): the outline draws a node's first 50 children and a control for the rest only once T pages it, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedPastFifty, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(
      branchMoreRows(page),
      "an outline of more than 50 top-level rows draws no control for the rest"
    ).toHaveCount(1);
    const words = "A new comment past the first fifty rows.";
    await composerField(page).click();
    await sendFromComposer(page, words);
    const comment = (await commentWithBody(seeded.key, undefined, words)).id;
    const row = nodeRow(page, comment).first();
    await expect(row, "the new comment past the first 50 rows is not drawn").toBeVisible();
    await expect
      .poll(() => focusedNodeId(page), {
        message: "the focus is not on the new comment past the first 50 rows",
      })
      .toBe(comment);
    await expect
      .poll(() => clearOfBox(row), {
        message: "the new comment past the first 50 rows is off screen",
      })
      .toBe(true);
  });
});

// c-replies (3), owed on part M (root R15, decision 127): a reply from the panel's history to a
// turn of a thread the Legion daemon began lands in the history, under that turn, on screen and
// focused, and the outline draws no row of it. Until part M is under this part's head the panel
// draws no history with a Reply, so the reading is skipped with an annotation naming M and tagged
// `@owed`, which the check's run leaves out (decision 135). The round that brings M under the
// head takes the skip and the tag off.
test.skip("what the reader sends from the box as a reply to a turn of the daemon's thread is drawn in the panel's history under that turn @owed", {
  annotation: {
    description:
      "Owed on part M (decision 127): the panel's history and its Reply come with part M, so this reading is skipped until M is under this part's head.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedHistoryTurns, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await openDetails(page);
    const turn = panelEntry(page, seeded.daemon.words);
    await expect(turn, "the panel's history draws no turn of the daemon's thread").toHaveCount(1);
    await turn.getByRole("button", { exact: true, name: CONTROL_LABELS.reply }).click();
    await expect(composerTarget(page), "the history's Reply draws no target line").toBeVisible();
    const words = "A reply to the daemon's turn, from the history.";
    await sendFromComposer(page, words);
    const reply = panelEntry(page, words);
    await expect(reply, "the reply is not drawn in the panel's history").toBeVisible();
    await expect(
      turn.filter({ hasText: words }),
      "the reply is not drawn under the daemon's turn in the history"
    ).toHaveCount(1);
    await expect
      .poll(() => focusInside(reply), { message: "the focus is not on the reply in the history" })
      .toBe(true);
    await expect
      .poll(() => onScreen(reply), { message: "the reply in the history is off screen" })
      .toBe(true);
    await expect(
      nodeRows(page).filter({ hasText: words }),
      "the outline draws a row of the reply to the daemon's turn"
    ).toHaveCount(0);
  });
});

// c-replies (3), owed on parts M and T (root R1 and R15, decision 127): a reply from the panel's
// history to an agent's comment tied to no text, in a thread no person wrote in, makes that thread
// a person's, so the outline draws the comment with the reply under it, on screen and focused.
// Until M's history with its Reply and T's outline are both under this part's head, the reading
// is skipped with an annotation naming M and T and tagged `@owed`, which the check's run leaves
// out (decision 135). The round that brings the second of them under the head takes the skip and
// the tag off.
test.skip("what the reader sends from the box as a reply to an agent's comment in the panel's history is drawn in the conversation under it @owed", {
  annotation: {
    description:
      "Owed on parts M and T (decision 127): the panel's history and its Reply come with part M, and the outline that then draws the agent's comment with T, so this reading is skipped until both are under this part's head.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedHistoryTurns, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expect(
      nodeRow(page, seeded.agent.id),
      "the outline draws the agent's comment before a person wrote in its thread"
    ).toHaveCount(0);
    await openDetails(page);
    const turn = panelEntry(page, seeded.agent.words);
    await expect(turn, "the panel's history draws no agent's comment").toHaveCount(1);
    await turn.getByRole("button", { exact: true, name: CONTROL_LABELS.reply }).click();
    await expect(composerTarget(page), "the history's Reply draws no target line").toBeVisible();
    const words = "A reply to the agent's comment, from the history.";
    await sendFromComposer(page, words);
    const reply = (await commentWithBody(seeded.key, undefined, words)).id;
    await expect
      .poll(() => parentOfRow(page, reply), {
        message: "the reply is not drawn in the conversation under the agent's comment",
      })
      .toBe(seeded.agent.id);
    await expect
      .poll(() => focusedNodeId(page), { message: "the focus is not on the reply's row" })
      .toBe(reply);
    await expect
      .poll(() => clearOfBox(nodeRow(page, reply)), { message: "the reply's row is off screen" })
      .toBe(true);
  });
});

/** The view control of Graph, by its word: the part that builds Graph adds it to the view row. */
const GRAPH = "Graph";

// c-replies (3), owed on part T (root R38, decision 127): a reply written in Graph through T's
// Reply lands in Graph, its row drawn there, on screen and focused. Until part T is under this
// part's head the page has no Graph, so the reading is skipped with an annotation naming T and
// tagged `@owed`, which the check's run leaves out (decision 135). The round that brings T under
// the head takes the skip and the tag off.
test.skip("what the reader sends from the box as a reply written in Graph is drawn in Graph, in view and focused @owed", {
  annotation: {
    description:
      "Owed on part T (decision 127): Graph and its Reply come with part T, so this reading is skipped until T is under this part's head.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await pickPreset(page, GRAPH);
    await expectActivePreset(page, GRAPH);
    const head = await focusRow(page, seeded.head);
    await rowAct(head, "reply").click();
    await expect(composerTarget(page), "Graph's Reply draws no target line").toBeVisible();
    const words = "A reply written in Graph.";
    await sendFromComposer(page, words);
    const reply = (await commentWithBody(seeded.key, undefined, words)).id;
    await expectActivePreset(page, GRAPH);
    const row = nodeRow(page, reply).first();
    await expect(row, "the reply written in Graph is not drawn there").toBeVisible();
    await expect
      .poll(() => focusedNodeId(page), { message: "the focus is not on the reply in Graph" })
      .toBe(reply);
    await expect
      .poll(() => clearOfBox(row), { message: "the reply in Graph is off screen" })
      .toBe(true);
  });
});

// c-box (5). The tester's F2 and F9, and the architect's ruling 5: empty and at rest the box is
// one line with Send, as the pictures draw it, at 390 px as at 1440 px, and it says the words go
// on the issue (R3, `issue_placeholder`, in its phone form at 760 px and below); while it replies
// its placeholder says where the words go; and `a` on an open question leaves it as it is at
// rest, since the box answers no ask (R5, `OutlineWriter.answer`): no target line, no option,
// no answer field and no caret in it, the focus left on the question's row.
// SLOW(>=2s): two cold loads, at 1440 px and at 390 px, each with a reply opened and left again
// and `a` pressed on the open question.
test("the box at the foot is one line at rest, and says where a reply goes @phone", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    // Every live process serves a build the page does not run, so each load shows the update
    // banner, the float that the foot's page layer publishes for (`routeFloats` in
    // `e2e/foot-floats.ts`).
    await page.route("**/api/v1/dashboard/builds", (route) =>
      route.fulfill({
        json: { live: ["/assets/index-next.js"], serving: "/assets/index-next.js" },
      })
    );
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
      // Standing there, it is an element of the foot's page layer, which publishes how far up from
      // the screen's foot it reaches for the update banner to ride above (`--foot-page-inset`,
      // `useFootInset`): the reach of its wrapper, the box's parent.
      await page.evaluate(() => window.dispatchEvent(new Event("focus")));
      await expect(updateBanner(page), `the update banner does not show at ${at}`).toBeVisible();
      const wrapper = await composer(page).locator("..").boundingBox();
      const reach = size.height - (wrapper?.y ?? size.height);
      await expect
        .poll(
          async () =>
            Number.parseFloat(
              await page.evaluate(() =>
                getComputedStyle(document.documentElement).getPropertyValue("--foot-page-inset")
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
      // Escape at rest keeps the caret in the box, which the page puts back on a frame once the
      // line is drawn (`OutlineWriter.focus`): the next row is taken after that frame, as a
      // reader's next press is, rather than inside it, where the caret would take it back.
      await nextFrames(page);

      // `a` on the open question: the box stays as it is at rest and the focus on the row.
      await focusRow(page, seeded.openAsk);
      await page.keyboard.press("a");
      // The key's handler and the render it asks for are done within these frames, so a box that
      // the key aimed at the question would have drawn its line and taken the caret by their end.
      await nextFrames(page);
      await expect(
        composerTarget(page),
        `a on the open question draws a target line in the box at ${at}`
      ).toHaveCount(0);
      for (const option of ["Left", "Right"]) {
        await expect(
          composer(page).getByRole("button", { exact: true, name: option }),
          `a on the open question draws an option in the box at ${at}`
        ).toHaveCount(0);
      }
      await expect(
        composer(page).getByRole("textbox", { exact: true, name: CONTROL_LABELS.answer }),
        `a on the open question draws an answer field in the box at ${at}`
      ).toHaveCount(0);
      await expect(
        composerField(page),
        `a on the open question put the caret in the box at ${at}`
      ).not.toBeFocused();
      await expect
        .poll(() => focusedNodeId(page), {
          message: `a on the open question took the focus off its row at ${at}`,
        })
        .toBe(seeded.openAsk);
      await expect(
        composerField(page),
        `a on the open question changed what the box at rest says at ${at}`
      ).toHaveAttribute(
        "placeholder",
        phone ? CONTROL_LABELS.issue_placeholder_phone : CONTROL_LABELS.issue_placeholder
      );
    }
  });
});

// c-box (6). R1, R2 and R6, the tester's F9 and F13, and decision 72 of the parts: on an outline
// that runs past the screen the box stays at the foot, as wide as the outline (10 px from each
// side at 760 px and below), and the focused last row ends above it; at rest its line is 26 px
// and each placeholder fits it whole; it grows to 150 px and then scrolls inside itself, and on a
// phone a draft that wraps fits the field again once the focus leaves it; Tab reaches the line,
// each kind and Send; each control is at least 24 by 24 px, after `r` too and after `a`, which
// leaves the box as it is at rest; and Enter sends a comment and a question, each one new row by
// the reader.
// SLOW(>=2s): an outline of forty-one rows loaded at four widths, each read from rest to two
// sends, with a reply opened and `a` pressed at the two widths the check names.
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
        // The box's placeholders in their phone words: on the issue, under a row, on a
        // statement, and over the reader's own comment.
        for (const words of [
          CONTROL_LABELS.issue_placeholder_phone,
          CONTROL_LABELS.reply_placeholder_phone,
          CONTROL_LABELS.statement_placeholder_phone,
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
      // Escape at rest keeps the caret in the box, which the page puts back on a frame once the
      // line is drawn: the question's row is taken after that frame, as a reader's next press is.
      await nextFrames(page);

      // `a` on the open question leaves the box as it is at rest, since the box answers no ask
      // (`OutlineWriter.answer`): no target line, and each control at least 24 by 24 px.
      await focusRow(page, seeded.question);
      await page.keyboard.press("a");
      // The key's handler and the render it asks for are done within these frames, so a box that
      // the key aimed at the question would have drawn its target line by their end.
      await nextFrames(page);
      await expect(
        composerTarget(page),
        `a on the open question draws a target line in the box ${at}`
      ).toHaveCount(0);
      await expectControlsLeast(page, `${at}, after a on the open question`);
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

/** The view control of Threads, by its word: the part that builds Threads adds it to the view
 *  row. */
const THREADS = "Threads";

// c-box, owed on the part that builds Threads (decision 127): Threads carries a box of its own
// for a thread opened there, so the box at the foot is never drawn in Threads, and the panel's
// Reply on a turn of its history and its Edit on the reader's own comment set no target there;
// Conversation opened after them draws the box at rest, with no target line. Until Threads is
// under this part's head the page draws no Threads view, so the reading is skipped with an
// annotation naming that part and tagged `@owed`, which the check's run leaves out (decision
// 135). The round that brings Threads under the head takes the skip and the tag off.
test.skip("the box at the foot is never drawn in Threads, and the panel's Reply or Edit there sets no target @phone @owed", {
  annotation: {
    description:
      "Owed on the part that builds Threads (decision 127): the page draws no Threads view until that part builds it, so this reading is skipped until Threads is under this part's head.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedHistoryTurns, async (page, seeded) => {
    for (const size of [DESKTOP, PHONE]) {
      const at = `at ${size.width}x${size.height}`;
      await page.setViewportSize({ height: size.height, width: size.width });
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectIssuePageDrawn(page);
      await pickPreset(page, THREADS);
      await expectActivePreset(page, THREADS);
      await expect(composer(page), `the box at the foot is drawn in Threads ${at}`).toHaveCount(0);
      await openDetails(page);
      await panelEntry(page, seeded.daemon.words)
        .getByRole("button", { exact: true, name: CONTROL_LABELS.reply })
        .click();
      // The press's handler and the render it asks for are done within these frames, so a box
      // the press aimed would be drawn by their end.
      await nextFrames(page);
      await expect(
        composer(page),
        `the panel's Reply on a turn of the history drew the box in Threads ${at}`
      ).toHaveCount(0);
      await panelEntry(page, seeded.own.words)
        .getByRole("button", { exact: true, name: CONTROL_LABELS.edit })
        .click();
      // The same frames, for the Edit's press.
      await nextFrames(page);
      await expect(
        composer(page),
        `the panel's Edit on the reader's own comment drew the box in Threads ${at}`
      ).toHaveCount(0);
      await closeDetails(page);
      await pickPreset(page, PRESET_NAMES.conversation);
      await expectActivePreset(page, PRESET_NAMES.conversation);
      await expect(
        composerField(page),
        `Conversation draws no box after Threads ${at}`
      ).toBeVisible();
      await expect(
        composerTarget(page),
        `the panel's Reply or Edit in Threads set a target the box draws in Conversation ${at}`
      ).toHaveCount(0);
    }
  });
});

// The tester's red row (round 13), the root spec's R18 and R19: no part of the page scrolls in
// two dimensions from 320 px to 1440 px, and nothing a reader could read before is lost. A
// document's address runs past a phone's width, and the page draws no tooltip to give back what an
// ellipsis cuts, so on the issue page a row of the box's picker draws its whole detail, wrapped
// (`RefPickerList`'s `WRAPPED_LINE`). So when such a row is listed, the list stays inside the box,
// the page stays one screen wide, and no part of the row cuts its detail.
test("the box's picker keeps the page one screen wide when a row's detail is long @phone", async ({
  browser,
}) => {
  await phase(browser, seedReplyNesting, async (page, seeded) => {
    const name =
      "the-tester-notes-on-a-document-address-that-runs-past-the-width-of-a-phone-screen";
    // Search reads every project of the stack, so the word the picker finds holds the row's own
    // project key (`keyedSearchWord`): no other row's document answers it.
    const word = keyedSearchWord("zebrafish", layoutProject());
    await createIssueArtifact(seeded.key, {
      content: `# Notes\n\nA ${word} paragraph the picker's search finds.\n`,
      name: `${name}.md`,
    });
    // The row's detail: the document's address in its short form, as today's composers give it
    // whole in the row's tooltip.
    const detail = `${seeded.key} ${name}-md`;
    await page.setViewportSize({ height: PHONE.height, width: PHONE.width });
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await composerField(page).click();
    await page.keyboard.type(`#${word}`);
    const list = composer(page).getByRole("listbox", { name: "References" });
    const row = list.getByRole("option", { name: new RegExp(name) }).first();
    await expect(row, "the picker lists no row for the long document").toBeVisible();
    await expect(
      row.getByText(detail, { exact: true }),
      `the row does not draw its detail, "${detail}"`
    ).toBeVisible();
    const right = await list.evaluate((element) => element.getBoundingClientRect().right);
    expect(
      right,
      `the picker's list ends ${right.toFixed(0)} px from the left of a ${PHONE.width} px screen`
    ).toBeLessThanOrEqual(PHONE.width);
    const width = await page.evaluate(() => document.documentElement.scrollWidth);
    expect(
      width,
      `the page is ${width} px wide at ${PHONE.width} px, so it scrolls sideways`
    ).toBeLessThanOrEqual(PHONE.width);
    const cut = await row.evaluate(
      (option, words) =>
        [...option.querySelectorAll("*")].filter(
          (part) => part.textContent === words && part.scrollWidth > part.clientWidth + 1
        ).length,
      detail
    );
    expect(cut, `the row cuts its detail, "${detail}", and no tooltip gives it back`).toBe(0);
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

// c-mark (7), owed on part T (root R40, decision 127): an outline whose last row is followed by
// a control that shows more rows does not end there, so its end on screen for a second leaves the
// mark where it was, and the read mark is not drawn. Until part T pages the outline it draws every
// row and ends in the read mark, so the reading is skipped with an annotation naming T and tagged
// `@owed`, which the check's run leaves out (decision 135). The round that brings T under this
// part's head takes the skip and the tag off.
test.skip("the end of the outline on screen for a second marks the issue read, and not where a control that shows more rows ends it @owed", {
  annotation: {
    description:
      "Owed on part T (decision 127): the outline ends in a control that shows more rows only once T pages it, so this reading is skipped until T is under this part's head.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedPastFifty, async (page, seeded) => {
    const { key } = seeded;
    const writes = watchWrites(page, "PUT", `/api/v1/me/issues/${key}/state`);
    const head = (await getIssueEvents(key, { limit: 1, order: "desc" }))[0]?.seq ?? 0;
    const mark = async () => (await getMyState())[key]?.last_read_seq ?? 0;
    expect(await mark(), "the seed left the issue read for the reader").toBeLessThan(head);
    await gotoIssue(page, `/issues/${key}`);
    await expectIssuePageDrawn(page);
    await expect(composerField(page)).toBeVisible();
    const more = branchMoreRows(page);
    await expect(
      more,
      "an outline of more than 50 top-level rows ends in no control that shows more rows"
    ).toHaveCount(1);
    await page.keyboard.press("End");
    await expect
      .poll(() => onScreen(more), {
        message: "the control that shows more rows is not on screen after End",
      })
      .toBe(true);
    await expect(
      outlineEnd(page),
      "the outline draws its read mark while a control that shows more rows ends it"
    ).toHaveCount(0);
    // The mark must not be written while the outline goes on past that control, and a write that
    // never comes leaves nothing to poll for, so this span is the check: two dwells long, so a
    // dwell begun at End ends by its middle, and its write leaves before its end.
    await page.waitForTimeout(2 * DWELL_MS);
    expect(
      writes,
      "an outline that a control that shows more rows ends wrote the read mark"
    ).toEqual([]);
    expect(
      await mark(),
      "an outline that a control that shows more rows ends marked the issue read"
    ).toBeLessThan(head);
  });
});

/** One statement of `seedStatements` a row writes a comment on from the box. */
interface StatementCase {
  readonly block: string;
  /** Words of the statement, or of the issue it cites, that its target line draws none of. */
  readonly drawsNone: readonly string[];
  /** The statement's whole words, which the comment is anchored by. */
  readonly quote: string;
  /** How a failure names the statement. */
  readonly what: string;
  /** The words of the comment the row writes on it. */
  readonly words: string;
}

/** The two statements of `seedStatements` a comment lands on: the one that cites an issue by its
 *  address, and the one of two paragraphs, whose comment the server anchors to the statement
 *  itself. */
function statementCases(seeded: StatementsSeed): {
  readonly cites: StatementCase;
  readonly two: StatementCase;
} {
  return {
    cites: {
      block: STATEMENT_BLOCKS.cites,
      drawsNone: [STATEMENT_WORDS.citesLead, seeded.cited, CITED_TITLE],
      quote: citesWords(seeded.cited),
      what: "the statement that cites an issue",
      words: "A comment on the statement that cites an issue.",
    },
    two: {
      block: STATEMENT_BLOCKS.two,
      drawsNone: [STATEMENT_WORDS.first, STATEMENT_WORDS.second],
      quote: `${STATEMENT_WORDS.first} ${STATEMENT_WORDS.second}`,
      what: "the statement of two paragraphs",
      words: "A comment on the statement of two paragraphs.",
    },
  };
}

/**
 * Writes `one.words` as a comment on the statement `one` names, with the keys alone, from the
 * stage the page draws now - the outline or the Spec view. Fails unless `r` on the statement's
 * row puts the caret in the box; the target line says "Replying to:" and the page's word for a
 * statement, and draws none of `one.drawsNone` and no address, since the statement stands whole
 * in its own row; the placeholder is the statement's own; the box sends the statement's whole
 * words as the anchor's quote and no occurrence, and Dispatch stores the comment anchored in the
 * spec to the statement's block, or a block inside it, with that quote, as `blocks`, the spec's
 * blocks, which the caller reads once, since no comment changes them; the focus is back on the
 * statement's row; and, once the page's read of the issue's comments holds the comment, no row of
 * it is drawn under the statement. The outline draws it as a row under the issue, at its end, and
 * no margin beside the statement draws it yet; neither is checked here.
 */
async function commentOnStatement(
  page: Page,
  seeded: StatementsSeed,
  one: StatementCase,
  blocks: readonly ArtifactBlock[]
): Promise<void> {
  const statement = blockNode(seeded.specSlug, one.block);
  await expect(nodeRow(page, statement), `${one.what} draws no row`).toHaveCount(1);
  await focusRow(page, statement);
  await page.keyboard.press("r");
  await expect(composerField(page), `r on ${one.what} put no caret in the box`).toBeFocused();
  const target = composerTarget(page);
  await expect(target, `r on ${one.what} draws no target line`).toBeVisible();
  await expect(
    target,
    `the target line of ${one.what} does not say the box is replying`
  ).toContainText(CONTROL_LABELS.replying_to);
  await expect(target, `the target line does not name ${one.what} a statement`).toContainText(
    NODE_TYPE_NAMES.requirement
  );
  for (const words of one.drawsNone) {
    await expect(
      target,
      `the target line of ${one.what} draws "${words}", words of the statement`
    ).not.toContainText(words);
  }
  await expect(target, `the target line of ${one.what} draws an address`).not.toContainText(
    "dispatch://"
  );
  await expect(
    composerField(page),
    `the box's placeholder on ${one.what} is not the statement's own`
  ).toHaveAttribute("placeholder", CONTROL_LABELS.statement_placeholder);
  const writes = watchWrites(page, "POST", `/api/v1/issues/${seeded.key}/comments`);
  // The page's read of the issue's comments that holds the new one: the read the outline and each
  // view draw a comment from.
  const read = page.waitForResponse(
    async (response) =>
      response.request().method() === "GET" &&
      new URL(response.url()).pathname === `/api/v1/issues/${seeded.key}/comments` &&
      (await response.text().catch(() => "")).includes(one.words)
  );
  await page.keyboard.type(one.words);
  await page.keyboard.press("Enter");

  // Sent anchored by the statement's whole words alone, with no occurrence, and stored anchored
  // in the spec to the statement's block, or a block inside its range, with those words.
  await expect
    .poll(() => writes.map((write) => write.input), {
      message: `the box did not anchor the comment on ${one.what} by the statement's whole words and no occurrence`,
    })
    .toContainEqual(
      expect.objectContaining({
        anchor: { artifact: seeded.specSlug, quote: one.quote },
        body: one.words,
      })
    );
  const comment = await commentWithBody(seeded.key, undefined, one.words);
  expect(
    comment.anchor?.artifact_id,
    `the comment on ${one.what} is not anchored in the spec`
  ).toBe(seeded.specArtifactId);
  expect(
    comment.anchor?.quote,
    `the comment on ${one.what} is not anchored by the statement's whole words`
  ).toBe(one.quote);
  const range = blocks.find((block) => block.id === one.block);
  const anchored = blocks.find((block) => block.id === comment.anchor?.block_id);
  expect(
    range !== undefined &&
      anchored !== undefined &&
      anchored.from >= range.from &&
      anchored.to <= range.to,
    `the comment on ${one.what} is anchored to block ${comment.anchor?.block_id}, not inside ${one.block}`
  ).toBe(true);

  await expect
    .poll(() => focusedNodeId(page), {
      message: `the focus is not back on ${one.what}'s row once its comment landed`,
    })
    .toBe(statement);
  // The read that holds the comment has answered, and the render it asks for is done within the
  // frames after it, so a row the page drew of the comment under the statement is drawn by then.
  await read;
  await nextFrames(page);
  expect(
    await parentOfRow(page, comment.id),
    `the comment on ${one.what} is drawn in a row under it`
  ).not.toBe(statement);
}

// c-statement-reply, its outline half (R11): `r` on a focused statement aims the box at it. Its
// target line says "Replying to:" and the page's word for a statement and draws none of the
// statement's words - not its first line, not the issue it cites, no address - since the
// statement stands whole in its own row; the placeholder is the statement's own. What is sent is
// a comment on the statement's text, anchored in the spec to its block by its whole words and no
// occurrence, drawn in no row under it but under the issue at the outline's end; the focus
// is back on the statement's row, and the box then writes on the issue again. A statement of two
// paragraphs is anchored to the statement itself, and one whose words the spec holds twice is
// refused, in the page's words and with no Retry, the draft and the target kept. The row after
// this one reads the statement's author on the target line, and the one after that the Spec
// view's half, each owed on another part.
// SLOW(>=2s): five writes from the box, each read back from the server and the outline.
test("r on a statement writes a comment on its text", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedStatements, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const { cites, two } = statementCases(seeded);
    const blocks = await getArtifactBlocks(seeded.specArtifactId);
    for (const one of [cites, two]) {
      await commentOnStatement(page, seeded, one, blocks);

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
    await expect(
      composerTarget(page),
      "r on the twice-held statement draws no target line naming a statement"
    ).toContainText(NODE_TYPE_NAMES.requirement);
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
      NODE_TYPE_NAMES.requirement
    );
    // The target line names no words of the statement, so the conversation's current point, which
    // is the box's target, says which statement the box still holds.
    await expectPointOn(
      page,
      twice,
      "the twice-held statement's row",
      "while the box holds its refused comment"
    );
    expect(
      (await listComments(seeded.key)).some((one) => one.body === refused),
      "Dispatch holds the comment it refused"
    ).toBe(false);
  });
});

// c-statement-reply, the statement's author on its target line (R11), owed on part T (decision
// 127): the line names the first person the statement's node lists as its author, as the
// statement's row names it. On this base a statement's node carries no author, so the line names
// none; T's tree read gives the node its author. Until T is under this part's head the reading is
// skipped with an annotation naming T and tagged `@owed`, which the check's run leaves out
// (decision 135). The round that brings T under the head takes the skip and the tag off.
test.skip("r on a statement writes a comment on its text, and its target line names the first author its node lists @owed", {
  annotation: {
    description:
      "Owed on part T (decision 127): a statement's node carries its author only once T's tree read gives it the first person its node lists, so this reading is skipped until T is under this part's head.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedStatements, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const statement = blockNode(seeded.specSlug, STATEMENT_BLOCKS.two);
    await expect(nodeRow(page, statement), "the outline draws no row of the statement").toHaveCount(
      1
    );
    // The author the statement's row names: the first its node lists.
    const authorOfRow = async () => rowOf(await drawnRows(page), statement)?.author ?? null;
    await expect
      .poll(authorOfRow, { message: "the statement's row names no author" })
      .not.toBeNull();
    const author = (await authorOfRow()) ?? "";
    await focusRow(page, statement);
    await page.keyboard.press("r");
    await expect(composerField(page), "r on the statement put no caret in the box").toBeFocused();
    await expect(
      composerTarget(page),
      `the target line does not name ${author}, the first author the statement's node lists`
    ).toContainText(author);
  });
});

// c-statement-reply, its Spec view half (R11): `r` on a focused statement of the Spec view aims
// the box at it as on the outline, and what is sent is a comment on the statement's text, read as
// the outline's half reads it. Owed on part J (decision 74 of the parts: J passes the writer to
// the view's keymap; decision 61: the check names an owed reading and the part it waits on).
// Until J is under this part's head the Spec view draws no rows (`issue-page-switch.e2e.ts`), so
// the reading is skipped with an annotation naming J and those decisions (decision 127) and
// tagged `@owed`, which the check's run leaves out (decision 135). The round that brings J under
// the head takes the skip and the tag off.
test.skip("r on a statement in the Spec view writes a comment on its text @owed", {
  annotation: {
    description:
      "Owed on part J (decisions 74 and 61): the Spec view's rows take the writer's r only once J passes the writer to the view's keymap, so this reading is skipped until then.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedStatements, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}/spec`);
    await expectActivePreset(page, PRESET_NAMES.spec);
    await commentOnStatement(
      page,
      seeded,
      {
        ...statementCases(seeded).two,
        what: "the Spec view's statement of two paragraphs",
        words: "A comment on the statement from the Spec view.",
      },
      await getArtifactBlocks(seeded.specArtifactId)
    );
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
  // An issue of a project of the row's own (`rowKey`, e2e/test.ts), beside what the rows before
  // it wrote.
  const project = rowKey("DOCK");
  await createProject({ key: project, name: "Today's docked composer" });
  const { key } = await createIssue({
    force: true,
    project,
    title: "Today's page in a window whose foot meets its composer",
  });
  // Today's page: the reader has not turned the new page on.
  const context = await asUser(browser, READER);
  try {
    const page = await context.newPage();
    // Every live process serves a build the page does not run, so the load shows the update
    // banner, the float that the foot's page layer publishes for (`routeFloats` in
    // `e2e/foot-floats.ts`).
    await page.route("**/api/v1/dashboard/builds", (route) =>
      route.fulfill({
        json: { live: ["/assets/index-next.js"], serving: "/assets/index-next.js" },
      })
    );
    await page.setViewportSize(DESKTOP);
    await page.goto(`/issues/${key}/conversation`);
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await expect(
      updateBanner(page),
      "today's Conversation tab shows no update banner"
    ).toBeVisible();
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
        getComputedStyle(document.documentElement).getPropertyValue("--foot-page-inset")
      );
    // Two frames after the window met it: the resize and the composer's size observer have both
    // run by then, so the inset read is the one the shell draws its update banner by.
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
// carries a lighter ring; a new act takes the mark with it - an answer a second login writes to
// the open question after a comment of the reader's, then another comment; and no filter draws
// the mark: the Spec filter, opened at the same point, marks no row.
// SLOW(>=2s): a seeded thread and three questions, the thread opened press by press twice, two
// replies aimed and ended, two comments written from the box and an answer through the API, each
// waited on until the outline marks it, and the Spec filter opened.
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

    // A new act moves the mark. A comment of the reader's lands first; then a second login
    // answers the open question: once it is answered no question waits on the reader, and its
    // answer, written after that comment, is the newest act, so the mark is on the question's own
    // row whether or not the question waited on the reader before; then another comment on the
    // issue is the newest act, and the mark is on its row alone.
    const before = "A comment on the issue, written before the question is answered.";
    await sendFromComposer(page, before);
    const earlier = (await commentWithBody(seeded.key, undefined, before)).id;
    await expect(
      nodeRow(page, earlier),
      "the comment written before the answer is not drawn"
    ).not.toHaveCount(0);
    await answerAsk(
      seeded.openAsk,
      { expected_edited_at: null, selected: ["Left"] },
      { login: "bob" }
    );
    await expect(
      nodeRow(page, seeded.openAsk).first(),
      "the answer a second login wrote to the open question is not drawn"
    ).toHaveAttribute("data-node-state", "answered");
    await expectPointOn(
      page,
      seeded.openAsk,
      "the open question's row",
      "once a second login's answer to it, the newest act, landed"
    );
    const words = "A comment on the issue, the newest act of all.";
    await sendFromComposer(page, words);
    const comment = (await commentWithBody(seeded.key, undefined, words)).id;
    await expect(nodeRow(page, comment), "the new comment is not drawn").not.toHaveCount(0);
    await expectPointOn(page, comment, "the new comment's row", "once it landed on the issue");

    // No filter draws the mark: the Spec filter, opened at this point, marks nothing. The same
    // reading finds the comment's mark in the conversation first, so it reads the marks the page
    // draws.
    const marks = issuePage(page).locator(
      `[aria-current="${CURRENT_POINT}"], [data-current-point]`
    );
    await expect(marks, "the conversation's mark is not read where the page draws it").toHaveCount(
      1
    );
    await pickPreset(page, PRESET_NAMES.spec);
    await expectActivePreset(page, PRESET_NAMES.spec);
    await expect(
      issuePage(page).locator("[data-issue-body]"),
      "the Spec filter still draws the conversation's outline"
    ).toHaveCount(0);
    await expect(marks, "the Spec filter draws a mark of the current point").toHaveCount(0);
  });
});

// (10), owed on part T (root R40, decision 127): a result recorded after the newest act that the
// conversation draws is no act of it, so the mark stays on that act's row; and a current point
// that stands past its parent's first 50 children, behind the control for the rest, rings that
// control with the path's lighter ring, and no row is the current location. Until part T is under
// this part's head the outline draws every row and no control for the rest, so the reading is
// skipped with an annotation naming T and tagged `@owed`, which the check's run leaves out
// (decision 135). The round that brings T under the head takes the skip and the tag off.
test.skip("the current point stays on the newest act past a result recorded after it, and rings the control for the rest that it stands behind @owed", {
  annotation: {
    description:
      "Owed on part T (decision 127): the outline T moves onto its tree read draws a control for the rest of a node's children past its first 50, and the half of the current point that reads a result after the newest act waits on T's outline as well, so this reading is skipped until T is under this part's head.",
    type: "owed",
  },
}, async ({ browser }) => {
  await phase(browser, seedResultAfterAct, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    await expectPointOn(
      page,
      seeded.comment,
      "the comment's row, the newest act the conversation draws",
      "with a check's result recorded after it"
    );
  });
  await phase(browser, seedPastFifty, async (page, seeded) => {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectIssuePageDrawn(page);
    const more = branchMoreRows(page);
    await expect(
      more,
      "an outline of more than 50 top-level rows draws no control for the rest"
    ).toHaveCount(1);
    await expect(
      nodeRow(page, seeded.comments.at(-1) ?? ""),
      "the newest comment, the current point, is drawn among the first 50 rows"
    ).toHaveCount(0);
    await expect(
      currentRows(page),
      "a row is the current location while the point stands behind the control for the rest"
    ).toHaveCount(0);
    await expect(
      more.first(),
      "the control for the rest does not carry the path's mark while the point stands behind it"
    ).toHaveAttribute("data-current-point", "path");
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
    // One read is enough: focusRow above waited until the fourth level's row held the focus.
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
