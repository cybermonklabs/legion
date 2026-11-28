/** The count of what waits on the reader in the new issue page's header, pressed through
 *  Dashboard's list, with the reader's setting on: what the server decides for this row is what it
 *  reads - which open ask waits on the reader and whose turn it is, which decision waits for the
 *  reader's review, which approval request waits on the reader and which of the reader's
 *  approvals a later version made stale, and which threads hold a reply the reader has not read -
 *  and the header's count and Dashboard's list are both drawn from those reads. */

/*
 * Piece N3 of part N (dispatch://INFRA-3940, dispatch://INFRA-6230): the row of the count's
 * presses, moved here out of part N's `issue-page-n.e2e.ts` so it lands on `main` alone. The row
 * starts from `/issues/<key>/conversation`, and also from the spec opened in place of the outline
 * at its own address, `/issues/<key>/artifacts/<slug>` (dispatch://INFRA-6480 R5).
 */

import { PRESET_NAMES, type Preset } from "../web/src/features/issue-page/page-words";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  editArtifact,
  getIssue,
  getIssueDecisions,
  listMyReplies,
  requestApproval,
  reviewArtifact,
} from "./api";
import { mainHolds } from "./dashboard-seed";
import { countOf, drawnItems, itemsOf, nextFive, waitingOf } from "./dashboard-support";
import { asPlanner, PROJECT, pause } from "./issue-layout-seed-support";
import { gotoIssue, issuePage, phase, presetChooser, presetToolbar } from "./issue-page";
import { DESKTOP, PHONE } from "./issue-page-g";
import { openedArtifact, specAddress } from "./issue-page-n-documents";
import { waitingCount } from "./issue-page-p";
import { settledState } from "./settled";
import { expect, type Page, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/** The reader the seed's items wait on, and the person who approved the Verification document. */
const COUNT_READER = "alice";

/** What the row of the count's presses reads: the issue whose count is six items of Dashboard's
 *  list. */
interface WaitingWalkSeed {
  /** How many items wait on the reader, which the header's count and Dashboard's list agree on. */
  readonly items: number;
  readonly key: string;
}

/** Whether one entry of a decisions read of a `main` where decisions are nodes waits for the
 *  reader's review. */
function nodeEntryWaits(entry: unknown): boolean {
  return (
    typeof entry === "object" &&
    entry !== null &&
    "waiting_review" in entry &&
    entry.waiting_review === true
  );
}

/** Waits until the issue's decisions read holds one that waits for the reader's review, as the
 *  document service indexes a decision's links a moment after the write answers. */
async function awaitWaitingDecision(key: string, decisionsAreNodes: boolean): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const read = await getIssueDecisions(key);
    const waits = decisionsAreNodes
      ? "entries" in read && Array.isArray(read.entries) && read.entries.some(nodeEntryWaits)
      : (read.decisions ?? []).some(
          (decision) => decision.waiting_review && (decision.links ?? []).length > 0
        );
    if (waits) return;
    if (Date.now() > deadline) {
      throw new Error(`${key}'s decisions never read one waiting: ${JSON.stringify(read)}`);
    }
    await pause(100);
  }
}

/** Waits until the issue read gives the document `artifactId` the approval `state`, by `by` where
 *  one is named. */
async function awaitApprovalState(
  key: string,
  artifactId: string,
  state: string,
  by?: string
): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const approval = ((await getIssue(key)).artifacts ?? []).find(
      (artifact) => artifact.id === artifactId
    )?.approval;
    if (approval?.state === state && (by === undefined || approval.by?.id === by)) return;
    if (Date.now() > deadline) {
      throw new Error(`${key}'s document ${artifactId} reads ${JSON.stringify(approval)}`);
    }
    await pause(100);
  }
}

/** Waits until the reader's unread replies list each of `threads`. */
async function awaitUnreadThreads(key: string, threads: readonly string[]): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const rows = (await listMyReplies({ issue: key, state: "unread" })).rows ?? [];
    if (threads.every((thread) => rows.some((row) => row.thread_id === thread))) return;
    if (Date.now() > deadline) {
      throw new Error(
        `${key}'s unread threads read ${JSON.stringify(rows.map((row) => row.thread_id))}`
      );
    }
    await pause(100);
  }
}

/**
 * The row of the count's presses seeds six items that wait on the reader, no more: an open
 * question, a decision to review, an approval request on the spec, the reader's approval of the
 * Verification document that a later version made stale, and two threads with a reply the reader
 * has not read, one of comments and one of messages. Dashboard's own seed (`seedDashboard`) draws
 * nine, and the count of that seed is not filtered or mocked to six: this one writes the six.
 */
async function seedWaitingWalk(): Promise<WaitingWalkSeed> {
  const holds = await mainHolds();
  await createProject({ key: PROJECT, name: "The count's presses" });
  const issue = await createIssue({
    project: PROJECT,
    spec: ":::requirement{#r1}\nThe header counts a decision that waits.\n:::\n",
    title: "The count walks Dashboard's list",
  });
  const key = issue.key;
  const specArtifactId = issue.primary_artifact_id ?? "";

  await createAsk(key, { question: "Which order does the list take?" }, asPlanner);

  const shaped = `dispatch://${key}/spec#b-r1`;
  if (holds.decisionsAreNodes) {
    await editArtifact(
      specArtifactId,
      {
        ops: [
          {
            after: "end",
            markdown: `## Decisions\n\n:::decision{#d-walk shaped="${shaped}"}\n### Count what waits by its items\n\nChosen so the count and the list agree.\n:::`,
            op: "insert",
          },
        ],
      },
      asPlanner
    );
  } else {
    await createIssueArtifact(
      key,
      {
        content: `## Count what waits by its items\n\nChosen so the count and the list agree. Shapes ${shaped}.\n`,
        name: "decisions.md",
      },
      asPlanner
    );
  }
  await awaitWaitingDecision(key, holds.decisionsAreNodes);

  await requestApproval(specArtifactId, {}, asPlanner);
  await awaitApprovalState(key, specArtifactId, "awaiting");

  const verification = await createIssueArtifact(
    key,
    { content: "# Verification\n\nOne check.\n", name: "verification.md" },
    asPlanner
  );
  await reviewArtifact(verification.artifact.id, { state: "approved" }, { login: COUNT_READER });
  await editArtifact(
    verification.artifact.id,
    { ops: [{ find: "One check.", op: "replace", with: "Two checks." }] },
    asPlanner
  );
  await awaitApprovalState(key, verification.artifact.id, "stale", COUNT_READER);

  const comment = await createComment(key, { body: "Does the count drop as soon as I act?" });
  await createComment(
    key,
    { body: "It drops when the write lands, with no reload.", reply_to: comment.id },
    asPlanner
  );
  const message = await createMessage(
    key,
    { body: "The tiles read the results and the coverage." },
    asPlanner
  );
  await createMessage(key, { body: "And the children?", in_reply_to: message.id });
  await createMessage(
    key,
    { body: "The children from the issue read.", in_reply_to: message.id },
    asPlanner
  );
  await awaitUnreadThreads(key, [comment.id, message.id]);
  return { items: 6, key };
}

/** Fails unless the view row's control `preset` is active, whichever shape the row draws at this
 *  width: the toolbar's pressed toggle from 1280 px up, or the one control below it. Unlike
 *  `issue-page-i.ts`'s `expectActivePreset`, this accepts a control that draws no count, as
 *  Dashboard's does: Dashboard draws no signal, so below 1280 px the one control is named by the
 *  view's word alone (`IssuePresets.tsx`). */
async function expectActiveControl(page: Page, preset: Preset): Promise<void> {
  const name = PRESET_NAMES[preset];
  // The toolbar is the row of toggles; the one control's own name can be a view's alone, as
  // Dashboard's is, so a button by that name does not tell the shapes apart.
  const shape = await settledState("the view row's shape", [
    ["toolbar", presetToolbar(page)],
    ["one control", presetChooser(page)],
  ]);
  if (shape === "toolbar") {
    await expect(
      presetToolbar(page).getByRole("button", { exact: true, name }),
      `${name} is not active in the view row`
    ).toHaveAttribute("aria-pressed", "true");
    return;
  }
  await expect(
    presetChooser(page),
    `${name} is not active in the one control`
  ).toHaveAccessibleName(new RegExp(`^${name}(?: \\d+\\b|$)`, "u"));
}

// R5 of dispatch://INFRA-3940 (dispatch://INFRA-3678, under R13): the count of what waits on the
// reader is a control, and its number is the length of Dashboard's list. On the issue this row
// seeds - one question, one decision to review, an approval request on the spec, a stale approval
// of the Verification document and two unread threads: six items - a press, or Enter on the count,
// from Conversation pushes Dashboard's address and puts the focus on the list's first item. Each
// press after goes on to the next item, shows the next five past the fifth and comes round to the
// first, the list showing its first five again, so every item the count counts is reached by
// presses alone. The row also starts from the spec opened in place of the outline: there the first
// press closes the document, pushes Dashboard's address and puts the focus on the list's first
// item (dispatch://INFRA-6480 R5). An approval request alone, at the bare address that draws
// Dashboard, takes the press as a move of the focus, with no new address.
// SLOW(>=2s): the seed of six items over five kinds, each polled until the server reads it; four
// loads on chromium and two on iphone, half of them of the opened spec, with eight presses of the
// count at each width; and the lone approval request's issue, written and loaded.
test("the count of what waits on the reader opens the first thing that waits @phone", async ({
  browser,
}) => {
  test.setTimeout(170_000);
  await phase(browser, seedWaitingWalk, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    const waiting = waitingOf(issuePage(page));
    const items = itemsOf(waiting);
    const showMore = nextFive(waiting);
    const focused = () =>
      page.evaluate(() => document.activeElement?.getAttribute("data-testid") ?? "");
    const pressCount = async (key: "click" | "Enter") => {
      if (key === "click") await waitingCount(page).click();
      else {
        await waitingCount(page).focus();
        await page.keyboard.press("Enter");
      }
    };
    const settling = watchApiRequests(page);
    for (const [width, key] of (desktopProject
      ? [
          [1440, "click"],
          [390, "Enter"],
        ]
      : [[390, "click"]]) as readonly (readonly [number, "click" | "Enter"])[]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, `/issues/${seeded.key}/conversation`);
      await expectLoadSettled(settling);
      await expect(waitingCount(page)).toHaveText(`Waiting on you (${seeded.items})`);
      await pressCount(key);
      await expect
        .poll(focused, { message: `the focus is not on the first item that waits at ${width} px` })
        .toMatch(/^dashboard-item-/);
      await expect(page, "the count did not push Dashboard's address").toHaveURL(
        new RegExp(`/issues/${seeded.key}/dashboard$`)
      );
      await expectActiveControl(page, "dashboard");
      await expect(
        countOf(waiting),
        "the header's count is not the length of Dashboard's list"
      ).toHaveText(String(seeded.items));
      const first = (await drawnItems(waiting))[0];
      expect(await focused(), `the focus is off Dashboard's first item at ${width} px`).toBe(first);
      expect(await items.count(), "Dashboard did not draw its first five").toBe(5);

      // Each press after the first goes on to the next item in the list's order, and the one past
      // the fifth draws the next five: every item the count holds is reached by pressing it.
      const walked = [first];
      for (let press = 1; press < seeded.items; press += 1) {
        await pressCount(key);
        let next = "";
        await expect
          .poll(
            async () => {
              next = await focused();
              return next.startsWith("dashboard-item-") && !walked.includes(next);
            },
            { message: `press ${press + 1} of the count did not move the focus on at ${width} px` }
          )
          .toBe(true);
        walked.push(next);
      }
      expect(
        await drawnItems(waiting),
        `the presses did not walk the list's order at ${width} px`
      ).toEqual(walked);
      expect(await showMore.count(), "the next five were drawn and their control stayed").toBe(0);
      const kinds = new Set(walked.map((id) => id.replace(/^dashboard-item-/, "").split("-")[0]));
      expect(kinds, "the presses reached no item of one of the five kinds").toEqual(
        new Set(["approval", "decision", "question", "stale", "thread"])
      );

      // The press after the last comes round to the first, and the list shows its first five.
      await pressCount(key);
      await expect
        .poll(focused, { message: `the focus did not come round at ${width} px` })
        .toBe(first);
      await expect(items, "the list did not show its first five again").toHaveCount(5);
      await expect(showMore, "the control for the next five did not come back").toHaveCount(1);
    }

    // From the spec opened in place of the outline, the first press closes the document, pushes
    // Dashboard's address and puts the focus on the list's first item (dispatch://INFRA-6480 R5).
    for (const [width, key] of (desktopProject
      ? [
          [1440, "click"],
          [390, "Enter"],
        ]
      : [[390, "click"]]) as readonly (readonly [number, "click" | "Enter"])[]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, await specAddress(seeded.key));
      await expectLoadSettled(settling);
      await expect(openedArtifact(page), "the document did not open").toBeVisible();
      await expect(waitingCount(page)).toHaveText(`Waiting on you (${seeded.items})`);
      await pressCount(key);
      await expect(openedArtifact(page), "the count left the document open").toHaveCount(0);
      await expect
        .poll(focused, {
          message: `the focus is not on the first item that waits after a press from the opened document at ${width} px`,
        })
        .toMatch(/^dashboard-item-/);
      await expect(page, "the count did not push Dashboard's address").toHaveURL(
        new RegExp(`/issues/${seeded.key}/dashboard$`)
      );
      expect(
        await focused(),
        `the focus is off Dashboard's first item after a press from the opened document at ${width} px`
      ).toBe((await drawnItems(waiting))[0]);
    }

    // An approval request is all that waits: at the bare address, which draws Dashboard, the press
    // moves the focus to that request's item and writes no history entry.
    const lone = await createIssue({ force: true, project: PROJECT, spec: "Plan.", title: "Lone" });
    await requestApproval(lone.primary_artifact_id ?? "", { summary: "Read it." });
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${lone.key}`);
    await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    const entries = await page.evaluate(() => history.length);
    await waitingCount(page).click();
    await expect
      .poll(focused, { message: "the press at the bare address did not reach the request's item" })
      .toMatch(/^dashboard-item-approval-/);
    expect(
      await page.evaluate(() => history.length),
      "a press at the bare address wrote a history entry"
    ).toBe(entries);
    await expect(page).toHaveURL(new RegExp(`/issues/${lone.key}$`));
  });
});
