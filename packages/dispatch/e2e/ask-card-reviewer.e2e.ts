import type { Page } from "@playwright/test";

import { setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createMessage,
  getHeld,
  getInbox,
  getIssue,
} from "./api";
import { asTester, OPTIONS, PROJECT, REVIEW_PROMPT, seedProject, TESTER } from "./ask-card";
import type { FakeModelRule } from "./fake-model";
import { modelRequests, setModelRules } from "./fake-model-helpers";
import { inboxPane, inboxRow, selectRow } from "./inbox-pane";
import { expectInboxCount, inboxCountBadge } from "./inbox-threads";
import { atReviewer, expect, REVIEWER_MODEL, type ReviewerServer, test } from "./reviewer-server";

/**
 * The reviewer as a person's open page meets it (INFRA-3231's R5 and R7, check 39b4610c). The
 * reviewer holds the asks, comments, messages and new issues an agent writes to a person, and a
 * stand-in model answers its review after a delay with a finding and a rewrite. The tester's
 * session is live and does not act on the finding, so once the writer's window ends the setting
 * posts the reviewer's rewrite. With alice's page open the whole time: the held text shows nowhere
 * while it is held, then shows with no reload, already rewritten, marked by the badge Rewritten
 * alone, with the tester's own wording one key or one press away. A review that finds nothing
 * posts the text as written, with no badge.
 *
 * These rows read their texts through a Dispatch server of their own, started with the reviewer's
 * settings (`e2e/reviewer-server.ts`); every other spec's server holds nothing.
 *
 * At the commit before the change this file fails: the Inbox shows the ask as written at once.
 */

test.skip(
  !!process.env.PLAYWRIGHT_BASE_URL,
  "a deployed run starts no server of its own, so it has no reviewer's server"
);

/** The phrase the stand-in reviewer finds, which states no fact. */
const VACUOUS = "This names the problem.";

/** The reviewer's answer: the finding of the vacuous phrase, and `rewrite`. */
function reviewed(rewrite: object): string {
  return JSON.stringify({
    findings: [
      { fix: "Say what fails.", quote: VACUOUS, rule_set: "phrases", why: "It states no fact." },
    ],
    rewrite,
  });
}

/** The stand-in reviewer, answering every review after `delayMs` with `text`. */
function reviewRule(text: string, delayMs = 2_000): FakeModelRule {
  return { contains: REVIEW_PROMPT, model: REVIEWER_MODEL, reply: { delayMs, text } };
}

/** The id a held write answered: an issue's key, else a uuid. */
function heldId(answer: unknown): string {
  if (typeof answer !== "object" || answer === null) return "";
  if ("key" in answer && typeof answer.key === "string" && answer.key !== "") return answer.key;
  return "id" in answer && typeof answer.id === "string" ? answer.id : "";
}

/** The issue alice holds, on which the tester writes, made through the stack's own server, and
 *  the tester live in the fake Envoy, so the reviewer sends it the finding and waits for it. */
async function seedIssue(): Promise<string> {
  await seedProject();
  await setLiveSessions([TESTER]);
  return (await createIssue({ assignee: "alice", project: PROJECT, title: "Mirror lag" })).key;
}

/** What the tester writes through the reviewer's server. */
function asWriter(reviewer: ReviewerServer) {
  return { ...asTester, baseUrl: reviewer.url };
}

/** How long a held text may stay held in these rows: the review's 2 s, the writer's 2 s window
 *  and up to the reviewer's 10-second sweep, with room to spare. */
const HOLD_MS = 30_000;

/**
 * Reads the held text `id` until it posts, and before each read counts with `shown` what the
 * open page draws of it: every count taken while the text was held is 0. The count comes first
 * and the state after it, so a text still held at that read was held while the page was counted,
 * and a text that shows anywhere in the hold - the review, the writer's window, the wait for the
 * sweep - fails the row where one look at one instant would pass it. A post commits with the
 * text's `posted` state, so `posting` is still held.
 *
 * The reads are one `expect.poll`'s, every 200 ms until the text posts or `HOLD_MS` ends. A count
 * that is not 0 fails the row at the read that took it: an assertion that throws in the polled
 * function ends the poll at once, since Playwright retries only the matcher the poll ends in.
 */
async function expectHiddenUntilPosted(id: string, shown: () => Promise<number>): Promise<void> {
  let counts = 0;
  await expect
    .poll(
      async () => {
        const count = await shown();
        const { state } = await getHeld(id);
        if (state === "posted") return state;
        expect(state, `the held text ${id} failed`).not.toBe("failed");
        expect(count, `what the page drew of ${id} while it was ${state}`).toBe(0);
        counts += 1;
        return state;
      },
      { intervals: [200], message: `${id} was still held after ${HOLD_MS} ms`, timeout: HOLD_MS }
    )
    .toBe("posted");
  // A text that posted before the first count proves nothing of its hold.
  expect(counts, `${id} posted before the page was counted once`).toBeGreaterThan(0);
}

/** What the open Inbox draws of held asks: its rows, its count, and alice's Inbox as read. */
async function inboxShown(page: Page): Promise<number> {
  const rows = await page.locator("[data-inbox-row]").count();
  // One instant's badge is what each read means to take: the poll that calls this reads it
  // again every 200 ms, and a held ask shows its number at no instant of the hold.
  const counted = /\d/.test((await inboxCountBadge(page).textContent()) ?? "") ? 1 : 0;
  return rows + counted + (await getInbox({ login: "alice" })).length;
}

const ASKED = `Ship the flip audit?\nThe mirror run is done. ${VACUOUS}`;
const ASK_REWRITE =
  "Ship the flip audit?\nThe mirror's first run fails on one row; ship it today or wait for a fix.";

// SLOW(>=2s): the stand-in reviews for 2 s, the writer's window is 2 s, and the reviewer's sweep
// that posts the rewrite runs every 10 s.
test("a held ask reaches the open Inbox only once it posts, rewritten, marked by its badge alone", async ({
  browser,
  reviewer,
}) => {
  const key = await seedIssue();
  await setModelRules([
    reviewRule(
      reviewed({
        body: ASK_REWRITE,
        multiple: false,
        options: OPTIONS.map((option) => ({
          description: option.description ?? "",
          label: option.label,
        })),
      })
    ),
  ]);
  const alice = await atReviewer(browser, reviewer);
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { exact: true, level: 1, name: "Inbox" })).toBeVisible();
    await expectInboxCount(page, 0);

    const written = await createAsk(
      key,
      { options: [...OPTIONS], question: ASKED },
      asWriter(reviewer)
    );
    const id = heldId(written);
    expect(id).not.toBe("");
    // The ask shows nowhere while it is held, at any count before it posts: not in the page's
    // rows, not in its count, not in alice's Inbox.
    await expectHiddenUntilPosted(id, () => inboxShown(page));

    // The setting posts the reviewer's rewrite once the window ends: the Inbox shows it, with
    // no reload, under the badge alone.
    const row = inboxRow(page, id);
    await expect(row).toBeVisible({ timeout: 30_000 });
    await expectInboxCount(page, 1);
    await selectRow(page, id);
    const card = inboxPane(page).locator(`[data-ask-card="${id}"]`);
    const badge = card.locator("[data-ask-rewrite]");
    const question = card.locator("[data-ask-question]");
    await expect(badge).toHaveText("Rewritten");
    await expect(question).toContainText("ship it today or wait for a fix");
    await expect(card).not.toContainText(VACUOUS);
    await expect(card).not.toContainText(/states no fact|Say what fails|reviewer|rewrote/i);
    expect((await getHeld(id)).outcome).toBe("rewrite_posted");

    // `w` shows the tester's held wording, and again the rewrite.
    await row.focus();
    await page.keyboard.press("w");
    await expect(badge).toHaveText("Original");
    await expect(question).toContainText(VACUOUS);
    await page.keyboard.press("w");
    await expect(badge).toHaveText("Rewritten");
    await expect(question).not.toContainText(VACUOUS);
  } finally {
    await alice.close();
  }
});

const COMMENTED = `Status\nThe mirror is slow. ${VACUOUS}`;
const COMMENT_REWRITE = "Status\nThe mirror lags the ledger by ten minutes.";

// SLOW(>=2s): the review's 2 s, the writer's 2 s and the reviewer's 10-second sweep.
test("a held comment reaches the open Conversation once it posts, its badge in its header and w showing its original", async ({
  browser,
  reviewer,
}) => {
  const key = await seedIssue();
  await setModelRules([reviewRule(reviewed({ body: COMMENT_REWRITE }))]);
  const alice = await atReviewer(browser, reviewer);
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}/conversation`);
    await expect(page.getByTestId("issue-header")).toContainText("Mirror lag");

    const id = heldId(await createComment(key, { body: COMMENTED }, asWriter(reviewer)));
    expect(id).not.toBe("");
    const turn = page.locator(`[data-turn="comment:${id}"]`);
    // The comment shows nowhere in the open Conversation while it is held.
    await expectHiddenUntilPosted(id, () => turn.count());

    await expect(turn).toBeVisible({ timeout: 30_000 });
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const comment = turn.locator(`[data-margin-item="${id}"]`);
    const badge = comment.locator("[data-ask-rewrite]");
    await expect(badge).toHaveText("Rewritten");
    await expect(comment).toContainText("The mirror lags the ledger by ten minutes.");
    await expect(comment).not.toContainText(VACUOUS);

    // `w` with the comment in focus shows its original in place, and again the rewrite.
    await comment.focus();
    await page.keyboard.press("w");
    await expect(badge).toHaveText("Original");
    await expect(comment).toContainText(VACUOUS);
    await page.keyboard.press("w");
    await expect(badge).toHaveText("Rewritten");
    await expect(comment).not.toContainText(VACUOUS);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the review's 2 s, the writer's 2 s and the reviewer's 10-second sweep.
test("a held message reaches the issue's messages once it posts, and its badge shows its original", async ({
  browser,
  reviewer,
}) => {
  const key = await seedIssue();
  await setModelRules([reviewRule(reviewed({ body: COMMENT_REWRITE }))]);
  const alice = await atReviewer(browser, reviewer);
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}/conversation`);
    await expect(page.getByTestId("issue-header")).toContainText("Mirror lag");

    const id = heldId(await createMessage(key, { body: COMMENTED }, asWriter(reviewer)));
    expect(id).not.toBe("");
    const turn = page.locator(`[data-turn="message:${id}"]`);
    // The message shows nowhere in the open Conversation while it is held.
    await expectHiddenUntilPosted(id, () => turn.count());

    await expect(turn).toBeVisible({ timeout: 30_000 });
    const badge = turn.locator("[data-ask-rewrite]");
    await expect(badge).toHaveText("Rewritten");
    await expect(turn).toContainText("The mirror lags the ledger by ten minutes.");
    await expect(turn).not.toContainText(VACUOUS);
    await badge.click();
    await expect(badge).toHaveText("Original");
    await expect(turn).toContainText(VACUOUS);
    await badge.click();
    await expect(badge).toHaveText("Rewritten");
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the review's 2 s, the writer's 2 s and the reviewer's 10-second sweep.
test("a held new issue shows once it posts, rewritten, and the badge beside its title shows its original title and spec", async ({
  browser,
  reviewer,
}) => {
  await seedIssue();
  await setModelRules([
    reviewRule(
      reviewed({
        body: "The mirror lags the ledger by ten minutes.",
        title: "Fix the mirror's lag",
      })
    ),
  ]);
  const key = heldId(
    await createIssue(
      { project: PROJECT, spec: `The mirror is slow. ${VACUOUS}`, title: "Mirror trouble" },
      asWriter(reviewer)
    )
  );
  expect(key).toMatch(/^DEMO-\d+$/);
  // No person reads the issue while it is held.
  const unseen = await getIssue(key).then(
    () => "read",
    (error: unknown) => String(error)
  );
  expect(unseen).toContain("404");
  await expect.poll(async () => (await getHeld(key)).state, { timeout: 30_000 }).toBe("posted");

  const alice = await atReviewer(browser, reviewer);
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${key}/spec`);
    const header = page.getByTestId("issue-header");
    const title = header.getByRole("heading", { level: 1 });
    await expect(title).toContainText("Fix the mirror's lag");
    const badge = header.locator("[data-ask-rewrite]");
    await expect(badge).toHaveText("Rewritten");
    await badge.click();
    await expect(badge).toHaveText("Original");
    await expect(title).toContainText("Mirror trouble");
    await expect(page.locator("[data-issue-original-spec]")).toContainText(VACUOUS);
    await badge.click();
    await expect(badge).toHaveText("Rewritten");
    await expect(title).toContainText("Fix the mirror's lag");
    await expect(page.locator("[data-issue-original-spec]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the stand-in reviews for 1.5 s before it answers that it found nothing.
test("an ask the reviewer finds nothing in reaches the open Inbox as written, with no badge", async ({
  browser,
  reviewer,
}) => {
  const key = await seedIssue();
  await setModelRules([reviewRule(JSON.stringify({ findings: [] }), 1_500)]);
  const alice = await atReviewer(browser, reviewer);
  try {
    const page = await alice.newPage();
    await page.goto("/");
    await expect(page.getByRole("heading", { exact: true, level: 1, name: "Inbox" })).toBeVisible();
    const question = "Ship the flip audit?\nThe mirror's first run passed every row.";
    const id = heldId(
      await createAsk(key, { options: [...OPTIONS], question }, asWriter(reviewer))
    );
    // The ask shows nowhere in the open Inbox while the review runs.
    await expectHiddenUntilPosted(id, () => inboxShown(page));

    const row = inboxRow(page, id);
    await expect(row).toBeVisible({ timeout: 30_000 });
    await selectRow(page, id);
    const card = inboxPane(page).locator(`[data-ask-card="${id}"]`);
    await expect(card.locator("[data-ask-question]")).toContainText("passed every row");
    await expect(card.locator("[data-ask-rewrite]")).toHaveCount(0);
    expect((await getHeld(id)).outcome).toBe("passed");
    expect(
      (await modelRequests()).filter((request) => request.model === REVIEWER_MODEL)
    ).toHaveLength(1);
  } finally {
    await alice.close();
  }
});
