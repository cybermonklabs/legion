/** One thread order everywhere (INFRA-450): the three threads the rows are read over, the parts
 *  of a collapsing thread, and the control that picks the order. Shared by
 *  `threads-order.e2e.ts` and the rows of that family which run against a recording
 *  (`threads-order.stub.e2e.ts`).
 *
 *  Everything here is made up: this repository is public, so nothing names a real person, host
 *  or project.
 */
import { expect, type Locator, type Page } from "@playwright/test";
import type { InboxRow } from "../web/src/api/types";

import { setLiveSessions } from "./agents";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createProject,
  getInbox,
  listComments,
  markThreadRead,
} from "./api";
import { inboxPane } from "./inbox-pane";
import { asSession, THREAD_SESSION } from "./inbox-threads";
export const ROOT = "How far back does a replay go?";
/** The six replies, oldest first; the newest two are the ones a collapsing surface keeps. */
export const REPLIES = [1, 2, 3, 4, 5, 6].map((place) => `Reply ${place} about the replay budget.`);
export const DECISION_BLOCK = "replay-window";
export const DECISION_QUESTION = "Which window does the mirror replay?";
export const decisionSpec = [
  "# Replay window",
  "",
  "The mirror replays the ledger on a cold start.",
  "",
  `:::ask{#${DECISION_BLOCK} urgency="med" multiple="false" state="open"}`,
  DECISION_QUESTION,
  "",
  "- The last hour",
  "- The last day",
  ":::",
  "",
].join("\n");

/** The Inbox row `match` names, once the server has indexed the block that carries it. */
async function indexedRow(match: (row: InboxRow) => boolean): Promise<InboxRow> {
  await expect.poll(async () => (await getInbox()).some(match)).toBe(true);
  const row = (await getInbox()).find(match);
  if (row === undefined) throw new Error("the seeded question left the Inbox before it was read");
  return row;
}

export interface Seeded {
  readonly askId: string;
  readonly askIssue: string;
  readonly blockAsk: string;
  readonly blockIssue: string;
  readonly comment: string;
  readonly commentIssue: string;
}

/**
 * Three threads of a root and six replies: a comment thread the reader wrote in, an answered
 * ask's thread, and the thread of an answered decision block in an issue's spec. Each comment
 * thread is read through its fourth reply, so replies 5 and 6 are the unread ones.
 */
export async function seedThreads(replies: readonly string[] = REPLIES): Promise<Seeded> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });

  const commentIssue = await createIssue({
    project: "CORE",
    spec: "# Replay budget\n\nThe mirror replays the ledger on a cold start.\n",
    title: "Replay budget",
  });
  // Anchored, so the issue's margin lists the thread beside the passage it is about.
  const comment = await createComment(commentIssue.key, {
    anchor: { artifact: "spec", quote: "replays the ledger" },
    body: ROOT,
  });
  for (const body of replies) {
    await createComment(commentIssue.key, { body, reply_to: comment.id }, asSession());
  }

  const askIssue = await createIssue({ project: "CORE", title: "Lease handover" });
  const ask = await createAsk(
    askIssue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship the lease handover?" },
    asSession()
  );
  await answerAsk(ask.id, { expected_edited_at: null, selected: ["Ship"] });
  for (const body of replies) {
    await createComment(askIssue.key, { ask_id: ask.id, body }, asSession());
  }

  // force, because this fixture files a near copy on purpose: "Replay window" shares its only
  // meaning sentence, "The mirror replays the ledger on a cold start.", with "Replay budget"
  // above, which the near-copy gate refuses as a copy of an open issue
  // (dispatch://INFRA-1160/spec#b-r2). The threads this spec reads are what it is about, so the
  // create goes in forced rather than the two texts being pulled apart.
  const blockIssue = await createIssue({
    force: true,
    project: "CORE",
    spec: decisionSpec,
    title: "Replay window",
  });
  const block = await indexedRow((row) => row.block_id === DECISION_BLOCK);
  await answerAsk(block.id, { expected_edited_at: null, selected: ["The last hour"] });
  for (const body of replies) {
    await createComment(blockIssue.key, { ask_id: block.id, body }, asSession());
  }

  return {
    askId: ask.id,
    askIssue: askIssue.key,
    blockAsk: block.id,
    blockIssue: blockIssue.key,
    comment: comment.id,
    commentIssue: commentIssue.key,
  };
}

/** Reads the comment thread through its `through`th reply, leaving the rest unread. */
export async function readThrough(issueKey: string, root: string, through: number): Promise<void> {
  const replies = (await listComments(issueKey))
    .filter((comment) => comment.reply_to === root)
    .sort((left, right) => (left.created_at < right.created_at ? -1 : 1));
  await markThreadRead(root, replies[through - 1]?.created_at ?? "");
}

/**
 * Reads an ask's own thread through its `through`th reply, leaving the rest unread. The ask's
 * replies are the only comments on its issue, so they are read in the order they were written.
 * An unread reply is never folded (`thread-layout.ts`), so a surface that reads the reader's
 * unread set - a document block's card does - folds nothing until the thread has been read.
 */
export async function readAskThrough(
  issueKey: string,
  askId: string,
  through: number
): Promise<void> {
  const replies = (await listComments(issueKey)).sort((left, right) =>
    left.created_at < right.created_at ? -1 : 1
  );
  await markThreadRead(askId, replies[through - 1]?.created_at ?? "");
}

/** The names of `items`, in the order the page draws them down the screen. */
export async function verticalOrder(items: Record<string, Locator>): Promise<string[]> {
  const placed: [string, number][] = [];
  for (const [name, locator] of Object.entries(items)) {
    const box = await locator.first().boundingBox();
    if (box === null) throw new Error(`${name} is not on the page`);
    placed.push([name, box.y]);
  }
  return placed.sort((left, right) => left[1] - right[1]).map(([name]) => name);
}

/** The parts of a collapsing thread, named: the root, the fold, the two newest replies, and the
 *  box the reader writes in. */
export function collapsedParts(
  scope: Locator,
  folded: number,
  box: Locator
): Record<string, Locator> {
  return {
    box,
    fold: scope.getByRole("button", {
      name: `Show ${folded} earlier ${folded === 1 ? "reply" : "replies"}`,
    }),
    root: scope.getByText(ROOT).first(),
    "reply-5": scope.getByText(REPLIES[4] ?? "").first(),
    "reply-6": scope.getByText(REPLIES[5] ?? "").first(),
  };
}

/** The same parts without the root, for a surface whose thread section holds the replies, the
 *  fold and the reply box while the question above it is the card's. */
export function foldParts(scope: Locator, folded: number): Record<string, Locator> {
  return {
    box: scope.getByRole("textbox", { name: "Reply" }),
    fold: scope.getByRole("button", {
      name: `Show ${folded} earlier ${folded === 1 ? "reply" : "replies"}`,
    }),
    "reply-5": scope.getByText(REPLIES[4] ?? "").first(),
    "reply-6": scope.getByText(REPLIES[5] ?? "").first(),
  };
}

/**
 * Opens a thread card that is still collapsed; an open one is left alone. The card may carry
 * its own `aria-expanded`, so the open state is read on the replies it reveals.
 *
 * The control arrives with the card, and `count()` waits for nothing, so a card still being
 * drawn would be read as a card with nothing to open and never opened at all: the wait is for
 * the card to have settled into one state or the other.
 */
export async function expand(card: Locator): Promise<void> {
  const replies = card.getByRole("list", { name: "Replies" });
  const collapsed = card.locator('button[aria-expanded="false"]').first();
  await expect(collapsed.or(replies).first()).toBeVisible();
  if ((await collapsed.count()) > 0) await collapsed.click();
  await expect(replies).toHaveCount(1);
}

/** Picks one of the two thread orders in the Inbox's **View** menu, which keeps it for this
 *  browser; every other surface reads the same choice. */
export async function setThreadOrder(page: Page, value: "newest" | "oldest"): Promise<void> {
  if (!page.url().endsWith("/")) await page.goto("/");
  await page.locator("[data-inbox-view]").click();
  const menu = page.getByRole("menu", { name: "View" });
  await menu
    .getByRole("group", { name: "Thread order" })
    .locator(`[data-inbox-view-choice="${value}"]`)
    .click();
  await page.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(0);
}

/** Selects a thread row and hands back the pane's thread body. */
export async function openInPane(page: Page, id: string): Promise<Locator> {
  const row = page.locator(`[data-inbox-row="${id}"]`);
  await expect(row).toBeVisible();
  await row.locator("time").first().click();
  await expect(row).toHaveAttribute("data-inbox-selected", "true");
  const body = inboxPane(page).locator("[data-inbox-thread-body]");
  await expect(body).toBeVisible();
  return body;
}

/** The two orders and the places a collapsing thread puts its fold and its reply box. */
export const FOLD_ORDERS = [
  ["oldest", ["fold", "reply-5", "reply-6", "box"]],
  ["newest", ["box", "reply-6", "reply-5", "fold"]],
] as const;
