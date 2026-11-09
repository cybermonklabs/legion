/** The Inbox's one list as the **View** menu and the **Filter** bar describe it: the eleven
 *  rows the fields are read over, what each value is called, the order each field puts them in,
 *  and the controls a row opens. Shared by `inbox-view.e2e.ts` and the rows of that family
 *  which run against a recording (`inbox-view.stub.e2e.ts`).
 *
 *  Everything here is made up: this repository is public, so nothing names a real person, host
 *  or project.
 */
import { expect, type Locator, type Page } from "@playwright/test";
import type { InboxRow, IssuePriority } from "../web/src/api/types";

import { setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  getInbox,
  listMyReplies,
  markThreadRead,
  patchIssue,
  requestApproval,
} from "./api";
import { inboxList } from "./inbox-pane";
import {
  agentRowId,
  asSession,
  replyRowId,
  seedAgentThread,
  seedCommentThread,
  seedMessageThread,
  THREAD_SESSION,
} from "./inbox-threads";

/**
 * The Inbox's **View** menu and its **Filter** bar over the one list (INFRA-450): what each of
 * the five fields filters, bands and orders by, how a band folds, how the reading pane is sized,
 * and that a list of a hundred and twenty threads is ordered and counted by the same rules.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

/** The list this file reads: every row, whoever holds it, so the fields have something to say. */
export const INBOX = "/?assignee=everyone";

/** One seeded row, as the five fields describe it. The expectations are computed from this
 *  table, so a count, a band and an order all mean the same thing by the same row. */
export interface ViewRow {
  readonly assignee: string;
  readonly id: string;
  readonly issueKey: string | undefined;
  readonly priority: string;
  readonly project: string;
  readonly type: string;
  readonly urgency: string;
}

const DECISION_BLOCK = "rollout-order";
const decisionSpec = [
  "# Rollout order",
  "",
  "The ledger and the mirror each keep a clock of their own.",
  "",
  `:::ask{#${DECISION_BLOCK} urgency="med" multiple="false" state="open"}`,
  "Which service moves first?",
  "",
  "- Ledger first",
  "- Mirror first",
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

/**
 * Eleven rows covering every kind, every priority and none, every urgency and none, two
 * projects and none, and three holders - the last of them a thread the reader has already read,
 * which the Inbox lists like any other, drawn as read, since a thread stays until it is
 * dismissed.
 */
export async function seedEveryField(): Promise<{
  read: string;
  rows: readonly ViewRow[];
}> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "MIRROR", name: "Mirror" });
  const rows: ViewRow[] = [];

  const question = async (
    title: string,
    priority: IssuePriority | null,
    urgency: "blocking" | "high" | "low" | "med" | undefined,
    assignee: string | null
  ): Promise<string> => {
    const issue = await createIssue({ project: "CORE", title });
    if (priority !== null) await patchIssue(issue.key, { priority });
    if (assignee !== "alice") await patchIssue(issue.key, { assignee });
    const ask = await createAsk(
      issue.key,
      { question: `${title}: what is the rule?`, ...(urgency === undefined ? {} : { urgency }) },
      asSession()
    );
    rows.push({
      assignee: assignee ?? "unassigned",
      id: ask.id,
      issueKey: issue.key,
      priority: priority === null ? "none" : `P${priority}`,
      project: "CORE",
      type: "question",
      // An ask whose author names no urgency is asked at `med` (the server's asks.go), so
      // only a thread row ever has none at all.
      urgency: urgency ?? "med",
    });
    return issue.key;
  };

  await question("Ledger rollout", 0, "blocking", "alice");
  await question("Mirror retries", 1, "high", "alice");
  // Bob's question, which a clarification hands back to the session: the one row under Waiting
  // on agents, so that banding has more than one band to show.
  const handed = await question("Lease handover", 2, "low", "bob");
  const handedAsk = rows[rows.length - 1];
  await createComment(
    handed,
    { ask_id: handedAsk?.id, body: "Checking the lease now.", turn: "agent" },
    asSession()
  );
  await question("Clock drift", 3, "med", "alice");
  await question("Replay budget", null, undefined, null);

  const decisionIssue = await createIssue({
    project: "CORE",
    spec: decisionSpec,
    title: "Rollout order",
  });
  await patchIssue(decisionIssue.key, { priority: 1 });
  const decision = await indexedRow((row) => row.block_id === DECISION_BLOCK);
  rows.push({
    assignee: "alice",
    id: decision.id,
    issueKey: decisionIssue.key,
    priority: "P1",
    project: "CORE",
    type: "decision",
    urgency: "med",
  });

  const approvalIssue = await createIssue({
    project: "CORE",
    spec: "# Window size\n\nThe mirror replays the last hour.\n",
    title: "Window size",
  });
  await patchIssue(approvalIssue.key, { priority: 2 });
  const approval = await requestApproval(
    approvalIssue.primary_artifact_id,
    { summary: "The window draft." },
    asSession()
  );
  if (approval.ask === null) throw new Error("the approval request recorded no ask");
  rows.push({
    assignee: "alice",
    id: approval.ask.id,
    issueKey: approvalIssue.key,
    // A review request is asked at `high` (the server's reviews.go), which no caller sets.
    priority: "P2",
    project: "CORE",
    type: "approval",
    urgency: "high",
  });

  const commentIssue = await createIssue({ project: "CORE", title: "Audit clock" });
  await patchIssue(commentIssue.key, { priority: 3 });
  const comment = await seedCommentThread(
    commentIssue.key,
    "Please fix the flaky clock test.",
    "Done: the clock test is green."
  );
  rows.push({
    assignee: "alice",
    id: replyRowId(comment),
    issueKey: commentIssue.key,
    priority: "P3",
    project: "CORE",
    type: "reply",
    urgency: "none",
  });

  const mirrorIssue = await createIssue({ project: "MIRROR", title: "Mirror runbook" });
  const message = await seedMessageThread(
    mirrorIssue.key,
    "Where is the nightly build?",
    "On the mirror, replaying."
  );
  rows.push({
    assignee: "alice",
    id: replyRowId(message),
    issueKey: mirrorIssue.key,
    priority: "none",
    project: "MIRROR",
    type: "reply",
    urgency: "none",
  });

  const agent = await seedAgentThread({ body: "What is the rollout status?", replies: ["Green."] });
  rows.push({
    assignee: "alice",
    id: agentRowId(agent),
    issueKey: undefined,
    priority: "none",
    project: "none",
    type: "agent",
    urgency: "none",
  });

  // One thread the reader has already read: a row of the list like any other, drawn as read.
  const readIssue = await createIssue({ project: "CORE", title: "Retired note" });
  const read = await seedCommentThread(
    readIssue.key,
    "Is the nightly mirror job still at 02:00?",
    "It moved to 03:00 last week."
  );
  const listed = await listMyReplies({ state: "unread" });
  const readRow = (listed.rows ?? []).find((row) => row.thread_id === read);
  if (readRow === undefined) throw new Error("the thread to mark read was never listed");
  await markThreadRead(read, readRow.reply.created_at);
  rows.push({
    assignee: "alice",
    id: replyRowId(read),
    issueKey: readIssue.key,
    priority: "none",
    project: "CORE",
    type: "reply",
    urgency: "none",
  });

  return { read: replyRowId(read), rows };
}

/** When each row was last written in, as the two reads answer it: what `newest` orders by. */
export async function lastActivity(): Promise<ReadonlyMap<string, string>> {
  const at = new Map<string, string>();
  for (const ask of await getInbox()) {
    at.set(ask.id, ask.last_reply?.created_at ?? ask.created_at);
  }
  for (const thread of (await listMyReplies({ kind: ["reply", "agent"], state: "inbox" })).rows ??
    []) {
    at.set(
      `${thread.kind === "agent" ? "agent" : "reply"}:${thread.thread_id}`,
      thread.reply.created_at
    );
  }
  return at;
}

/** Where a value sits among its own field's, as `valueRank` orders them. */
const VALUE_ORDER: Record<string, readonly string[]> = {
  assignee: [],
  priority: ["P0", "P1", "P2", "P3"],
  project: [],
  type: ["question", "decision", "approval", "reply", "agent"],
  urgency: ["blocking", "high", "med", "low"],
};

/** What each value is called where the reader reads it: a Filter row and a filter token. */
const VALUE_LABELS: Record<string, string> = {
  agent: "Agent message",
  approval: "Approval",
  blocking: "Blocking",
  decision: "Decision",
  high: "High",
  low: "Low",
  med: "Medium",
  question: "Question",
  reply: "Reply",
  unassigned: "Unassigned",
};

/** The plural each Type band is headed by. */
const TYPE_BANDS: Record<string, string> = {
  agent: "Agent messages",
  approval: "Approvals",
  decision: "Decisions",
  question: "Questions",
  reply: "Replies",
};

export function valueLabel(field: string, value: string): string {
  if (value === "none") {
    if (field === "priority") return "No priority";
    if (field === "urgency") return "No urgency";
    return "No project";
  }
  return VALUE_LABELS[value] ?? value;
}

export function bandLabel(field: string, value: string): string {
  if (field === "type") return TYPE_BANDS[value] ?? value;
  return valueLabel(field, value);
}

/** The values of `field` the seed holds, in the order the control and the bands list them. */
export function valuesOf(rows: readonly ViewRow[], field: keyof ViewRow): string[] {
  const fixed = VALUE_ORDER[field] ?? [];
  const seen = [...new Set(rows.map((row) => String(row[field])))];
  const ranked = (value: string) =>
    value === "none" || value === "unassigned"
      ? Number.MAX_SAFE_INTEGER
      : fixed.length === 0
        ? 0
        : fixed.indexOf(value);
  return seen.sort((left, right) => ranked(left) - ranked(right) || left.localeCompare(right));
}

export function countsOf(rows: readonly ViewRow[], field: keyof ViewRow): Map<string, number> {
  const counts = new Map<string, number>();
  for (const row of rows) {
    const value = String(row[field]);
    counts.set(value, (counts.get(value) ?? 0) + 1);
  }
  return counts;
}

/** The row ids in the order `sort` puts them under **Group by None**: the field's own order,
 *  `none` last, ties newest first and the row id breaking a tie that reaches it. */
export function sortedIds(
  rows: readonly ViewRow[],
  field: keyof ViewRow | "newest",
  at: ReadonlyMap<string, string>
): string[] {
  const order = field === "newest" ? [] : valuesOf(rows, field);
  return [...rows]
    .sort((left, right) => {
      if (field !== "newest") {
        const byField = order.indexOf(String(left[field])) - order.indexOf(String(right[field]));
        if (byField !== 0) return byField;
      }
      const leftAt = at.get(left.id) ?? "";
      const rightAt = at.get(right.id) ?? "";
      if (leftAt !== rightAt) return leftAt < rightAt ? 1 : -1;
      return left.id.localeCompare(right.id);
    })
    .map((row) => row.id);
}

/** The ids the list is drawing, top to bottom. */
export async function drawnIds(page: Page): Promise<string[]> {
  return page
    .locator("[data-inbox-row]")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
}

/** Every band heading the list is drawing, top to bottom, with its count. */
export async function bandHeadings(page: Page): Promise<string[]> {
  return inboxList(page)
    .locator("h2 button")
    .evaluateAll((nodes) => nodes.map((node) => (node.textContent ?? "").trim()));
}

/** Opens the bar's Filter panel, which is shut until the reader presses **Filter**. */
export async function openFilterFields(page: Page): Promise<Locator> {
  const fields = page.locator("[data-inbox-filter-fields]");
  if ((await fields.count()) === 0) await page.locator("[data-inbox-filter]").click();
  await expect(fields).toHaveCount(1);
  return fields;
}

/** Opens one field's popover and hands back its listbox, shutting whichever was open. */
export async function openField(page: Page, field: string): Promise<Locator> {
  const fields = await openFilterFields(page);
  const trigger = fields.getByRole("button", { name: `Filter by ${field}` });
  if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
  const label = field === "assignee" ? "Assignee" : field[0]?.toUpperCase() + field.slice(1);
  const options = page.getByRole("listbox", { name: `${label} options` });
  await expect(options).toHaveCount(1);
  return options;
}

/** The **View** menu, opened. */
export async function openView(page: Page): Promise<Locator> {
  const menu = page.getByRole("menu", { name: "View" });
  if ((await menu.count()) === 0) await page.locator("[data-inbox-view]").click();
  await expect(menu).toHaveCount(1);
  return menu;
}

/** Picks one choice of one **View** section; the sections share value words, so each is read
 *  through its own fieldset. */
export async function chooseView(page: Page, section: string, value: string): Promise<void> {
  const menu = await openView(page);
  await menu
    .getByRole("group", { name: section })
    .locator(`[data-inbox-view-choice="${value}"]`)
    .click();
}
