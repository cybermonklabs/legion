/** The pages the global keyboard rows are read against: two questions in the Inbox and a board
 *  of six cards. Shared by `keyboard.e2e.ts` and the rows of that family which run against a
 *  recording (`keyboard.stub.e2e.ts`). */
import { expect, type Page } from "@playwright/test";

import { createAsk, createIssue, createProject, patchIssue } from "./api";

export const session = {
  actor: { kind: "session" as const, id: "e2e-keyboard" },
  as: "agent" as const,
};

export async function seedInbox(): Promise<{ issueKey: string; rows: number }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard decision" });
  const options = [{ label: "Ship" }, { label: "Hold" }, { label: "Wait" }];
  await createAsk(issue.key, { options, question: "Older ask" }, session);
  await createAsk(issue.key, { options, question: "Newest ask" }, session);
  return { issueKey: issue.key, rows: 2 };
}

// This spec runs in `chromium` alone, at its 1280 px viewport, so the Inbox always draws its
// reading column: the questions are short rows, and the selected one's card is in the column
// beside them (`e2e/inbox-pane.ts`).
export async function openInbox(page: Page, rows = 2): Promise<void> {
  await page.goto("/");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows);
  await page.locator("body").focus();
}

/** Todo: Alpha, Bravo, Charlie; In progress: Delta; Retro: Echo, Foxtrot - the board seed. */
export async function seedBoard() {
  await createProject({ key: "CORE", name: "Core" });
  const seed = async (title: string, status: "todo" | "in_progress" | "retro") => {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
    return issue;
  };
  return {
    alpha: await seed("Alpha", "todo"),
    bravo: await seed("Bravo", "todo"),
    charlie: await seed("Charlie", "todo"),
    delta: await seed("Delta", "in_progress"),
    echo: await seed("Echo", "retro"),
    foxtrot: await seed("Foxtrot", "retro"),
  };
}

export async function openBoard(page: Page): Promise<void> {
  await page.goto("/projects/CORE");
  await page.getByRole("button", { name: "Board" }).click();
  await expect(page.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveCount(3);
  await page.locator("body").focus();
}
