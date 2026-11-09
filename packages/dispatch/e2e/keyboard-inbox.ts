/** The Inbox the keyboard rows are read against: the asks they walk, the reading pane they step
 *  into, and the moves a row makes before it presses a key. Shared by `keyboard-inbox.e2e.ts`
 *  and the rows of that family which run against a recording
 *  (`keyboard-inbox.stub.e2e.ts`). */
import { expect, type Locator, type Page } from "@playwright/test";

import {
  createAsk,
  createIssue,
  createProject,
  patchIssue,
  requestApproval,
  withOpenedAsk,
} from "./api";
import { askControls, hasReadingPane, paneScroller, selectRow } from "./inbox-pane";

export const session = {
  actor: { kind: "session" as const, id: "e2e-keyboard-inbox" },
  as: "agent" as const,
};

/** Three asks on three issues, so each row's `Select <owner>` checkbox has its own name. */
export async function seedInbox(): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["First", "Second", "Third"]) {
    const issue = await createIssue({ project: "CORE", title });
    await createAsk(issue.key, { question: `${title} decision` }, session);
  }
}

// The keymap binds only once sign-in resolves (`AuthGate` renders a skeleton until
// `/auth/whoami` answers), so a key pressed before the page renders reaches no handler.
export async function openInbox(page: Page, rows = 3): Promise<void> {
  await page.goto("/");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows);
  await page.locator("body").focus();
}

export async function askIdOf(page: Page, index: number): Promise<string> {
  const id = await page.locator("[data-inbox-row]").nth(index).getAttribute("data-inbox-row");
  expect(id).not.toBeNull();
  return id as string;
}

/**
 * Marks the rows the reader walks from the keyboard, starting where the Inbox left them.
 *
 * `x` marks the row the reader is on: with the reading column that is the row the column is
 * showing, which the Inbox selects for them when it opens, so the walk starts there; without
 * it, it is the row holding the focus, which `j` has to reach first.
 */
export async function markFromKeyboard(page: Page, count: number): Promise<void> {
  for (let index = 0; index < count; index += 1) {
    if (index > 0 || !hasReadingPane(page)) await page.keyboard.press("j");
    await page.keyboard.press("x");
  }
}

/** Puts the reader on a row: the focus on it and, with the reading column on, the selection the
 *  column follows, which is what `x` and `h` act on there. */
export async function goToRow(page: Page, index: number): Promise<void> {
  const row = page.locator("[data-inbox-row]").nth(index);
  await selectRow(page, await askIdOf(page, index));
  await row.focus();
}

/** The Snooze the reader reaches for one question: the row's own control, or - with the
 *  reading column on, where the row carries none - the column head's, the column being given
 *  that row first. */
export async function rowSnooze(page: Page, askId: string): Promise<Locator> {
  return (await askControls(page, askId)).locator("[data-inbox-snooze]");
}

/** A document taller than the pane: 80 paragraphs run several screens past the bottom of the
 *  column, so every scrolling key below has somewhere to go. */
const handoverLines = Array.from(
  { length: 80 },
  (_, index) =>
    `Handover note ${index + 1}: the ledger and the mirror each keep a clock of their own.`
);
const handoverSpec = ["# Ledger handover", "", ...handoverLines.flatMap((line) => [line, ""])].join(
  "\n"
);
export const RETRY_QUESTION = "Should the mirror keep retrying after a refused lease?";

export interface PaneSeed {
  /** The approval request the pane draws as a document, which is the taller one. */
  approvalAskId: string;
  /** The question in no document, which the pane draws as a card. */
  cardAskId: string;
}

/** Two questions, in the order the Inbox lists them (the issue's priority first): a request to
 *  approve a long document, and a question in no document. */
export async function seedPane(): Promise<PaneSeed> {
  await createProject({ key: "PANE", name: "Pane keyboard" });
  const handover = await createIssue({
    project: "PANE",
    spec: handoverSpec,
    title: "Ledger handover",
  });
  await patchIssue(handover.key, { priority: 0 });
  const approval = withOpenedAsk(
    await requestApproval(handover.primary_artifact_id, { summary: "The handover draft." }, session)
  );
  const retry = await createIssue({ project: "PANE", title: "Retry budget" });
  await patchIssue(retry.key, { priority: 1 });
  const card = await createAsk(
    retry.key,
    { options: [{ label: "Yes" }, { label: "No" }], question: RETRY_QUESTION },
    session
  );
  return { approvalAskId: approval.ask.id, cardAskId: card.id };
}

/** Opens the Inbox on both seeded rows with the pane beside them. The assignee filter is
 *  cleared, so a request to approve a document nobody holds is listed like any other question. */
export async function openPane(page: Page, seed: PaneSeed): Promise<void> {
  await page.goto("/?assignee=everyone");
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(2);
  await expect(rows.nth(0)).toHaveAttribute("data-inbox-row", seed.approvalAskId);
  await expect(rows.nth(1)).toHaveAttribute("data-inbox-row", seed.cardAskId);
  await expect(paneScroller(page)).toBeVisible();
  await page.locator("body").focus();
}
