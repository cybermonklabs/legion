/** the rows write a status, a reply and a Keep through the server and read each back: the
 *  status from its issue read, the reply and the Keep as the rows its tree and Decisions view
 *  reads draw again, with no reload */

import { getIssue } from "./api";
import { seedReviewList, seedWholeIssue } from "./issue-layout-seed";
import { phase } from "./issue-page";
import { blockNode, focusRow, nodeRow, rowAct, unfoldEveryBranch } from "./issue-page-g";
import { drawnRowReadings } from "./issue-page-t";
import {
  expectOneOrTwoReads,
  firstLoad,
  idReadsOf,
  KEPT_BY_ALICE,
  parentOfWords,
  pressFilter,
  REPLY_WORDS,
  ROW_MS,
  stateWordOf,
} from "./issue-page-t-rows";
import {
  decisionNodesGate,
  decisionsReadOf,
  decisionsViewGate,
  footLineGate,
  headerStatus,
  heldAnswer,
  owedUnless,
  treeAnswerOf,
  treeReadGate,
  treeReads,
} from "./issue-page-t-tree";
import { expect, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/**
 * Row 4 of the rows that draw the new issue page from the tree read, with its Keep clause as a
 * row of its own: a status and a reply written in the page are drawn from the tree read again,
 * with no reload. The head of `e2e/issue-page-t-rows.ts` says what the rows read and what their
 * gates are.
 */

/** The choice the page's status question offers to move the issue's parents into work with it,
 *  as a person reads and presses it: `ParentStatusDialog.tsx`' `CHOICE_LABELS.include_parent`,
 *  copied, since that file keeps the words to itself and this change leaves today's issue page
 *  as it is. */
const MOVE_PARENTS_TOO = "Move the parents into work too";

// c-fresh, with its status clause: a status write sends one or two tree reads - its event's, and
// one more where the event lands while the settle's read is out - and needs no line at the foot of
// the outline.
// SLOW(>=2s): the whole seeded issue, its load waited out, a status written and waited out, and a
// reply sent and waited for until its row is drawn.
test("a write is drawn without a reload", async ({ browser }) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const traffic = watchApiRequests(page);
    await firstLoad(page, traffic, seeded.key);
    let loads = 0;
    page.on("load", () => {
      loads += 1;
    });

    const status = headerStatus(page);
    // The header draws only from the answered issue read, and the load above was waited out, so
    // the Status control's value once attached is the status the row starts from, before a pick.
    const was = await status.inputValue();
    const next = was === "testing" ? "todo" : "testing";
    await status.selectOption(next);
    // The page's status writers ask Dispatch before they write (a dry run of the move), and ask
    // the person where Dispatch's rule would refuse it: the seed's issue stands under a parent
    // that is not in work, so a move into work asks, and the person moves the parents into work
    // too, which writes the status.
    const ask = page.getByRole("dialog", { name: new RegExp(`^Move ${seeded.key} to `) });
    const confirm = ask.getByRole("button", { exact: true, name: MOVE_PARENTS_TOO });
    await expect(confirm, "the status pick asks nothing that a person confirms").toBeEnabled();
    const picked = traffic.requests.length;
    await confirm.click();
    await expect
      .poll(async () => (await getIssue(seeded.key)).status, {
        message: "the status picked was never written",
      })
      .toBe(next);
    await expectLoadSettled(traffic);
    const sincePick = traffic.requests.slice(picked);
    const write = sincePick.findIndex(
      (request) =>
        request.method === "POST" && request.path === `/api/v1/issues/${seeded.key}/status-change`
    );
    expect(write, "the status pick sent no write of the issue").toBeGreaterThanOrEqual(0);
    expectOneOrTwoReads(
      treeReads(sincePick.slice(write + 1), seeded.key, null),
      "the status write sends one or two tree reads"
    );
    await expect(status, "the issue's row reads the new status").toHaveValue(next);

    const reads = await idReadsOf(seeded.key);
    const specSlug = (reads.issue.artifacts ?? []).find(
      (artifact) => artifact.id === seeded.specArtifactId
    )?.slug;
    expect(specSlug, "the issue holds no spec document").toBeDefined();
    const question = blockNode(specSlug ?? "", seeded.openQuestion.blockId);
    await unfoldEveryBranch(page);
    await rowAct(await focusRow(page, question), "reply").click();
    owedUnless(await footLineGate(page));
    const sent = traffic.requests.length;
    await page.keyboard.type(REPLY_WORDS);
    // The line sends as the page's other writing boxes send (`submitOnModifiedEnter`).
    await page.keyboard.press("ControlOrMeta+Enter");
    await expect
      .poll(
        async () => {
          await unfoldEveryBranch(page);
          return parentOfWords(await drawnRowReadings(page, { bodies: false }), REPLY_WORDS);
        },
        { message: "the new comment is a row" }
      )
      .toBe(question);
    expect(loads, "the page reloaded to draw the new comment").toBe(0);
    await expectLoadSettled(traffic);
    expectOneOrTwoReads(
      treeReads(traffic.requests.slice(sent), seeded.key, null),
      "the write sends one or two tree reads"
    );
  });
});

// c-fresh's Keep clause, owed on the model's decisions view of the tree read
// (`decisionsViewGate`) for the Decisions view read, and on decisions being nodes of the tree
// read on this branch's base (`decisionNodesGate`).
// SLOW(>=2s): a seeded issue, its load waited out, a Keep in the outline waited for, Decisions
// pressed and a second Keep there waited for.
test("a write is drawn without a reload, and Keep is drawn kept in the outline and in the Decisions view @owed", async ({
  browser,
}) => {
  test.setTimeout(ROW_MS);
  await phase(browser, seedReviewList, async (page, seeded) => {
    owedUnless(await treeReadGate(page, seeded.key));
    const tree = treeAnswerOf(page);
    const decisions = decisionsReadOf(page, seeded.key);
    const traffic = watchApiRequests(page);
    const load = await firstLoad(page, traffic, seeded.key);
    owedUnless(decisionsViewGate(load, seeded.key));
    owedUnless(decisionNodesGate(await heldAnswer(tree, "the tree read"), decisions.read()));
    await unfoldEveryBranch(page);
    const waiting = new Set(
      (await drawnRowReadings(page, { bodies: false })).flatMap((row) =>
        row.kind.type === "decision" && row.state?.token === "waiting_review" && row.id !== null
          ? [row.id]
          : []
      )
    );
    const [first, second] = [...waiting];
    if (first === undefined || second === undefined) {
      throw new Error(
        `${seeded.key} draws ${waiting.size} decisions that wait, and Keep needs two`
      );
    }

    const row = await focusRow(page, first);
    await row.locator('button[data-row-action="keep"]').click();
    await expect
      .poll(() => stateWordOf(page, first), { message: 'Keep makes its row read "Kept by" alice' })
      .toMatch(KEPT_BY_ALICE);

    await pressFilter(page, traffic, "decisions");
    const target = nodeRow(page, second).first();
    await target.focus();
    const kept = traffic.requests.length;
    await target.locator('button[data-row-action="keep"]').click();
    await expect
      .poll(() => stateWordOf(page, second), {
        message: "the decision reads kept in the Decisions view",
      })
      .toMatch(KEPT_BY_ALICE);
    await expectLoadSettled(traffic);
    expect(
      treeReads(traffic.requests.slice(kept), seeded.key, "decisions"),
      "Keep under Decisions sends the Decisions view read again"
    ).toBeGreaterThanOrEqual(1);
  });
});
