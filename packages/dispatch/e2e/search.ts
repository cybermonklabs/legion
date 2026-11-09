/** The search fixture the palette rows are read over, and the control that opens it. Shared by
 *  `search.e2e.ts` and the rows of that family which run against a recording
 *  (`search.stub.e2e.ts`). */
import type { Page } from "@playwright/test";

import { createComment, createIssue, createMessage, createProject } from "./api";
export const searchTerm = "astrolabe";
export const searchSpec =
  "# Instruments\n\nThe astrolabe measures altitude. Every astrolabe is brass.\n";
export const searchSession = {
  actor: { kind: "session" as const, id: "e2e-search" },
  as: "agent" as const,
};

export interface SearchFixture {
  readonly commentID: string;
  readonly commentIssueKey: string;
  readonly firstIssueKey: string;
}

export async function seedSearchFixture(): Promise<SearchFixture> {
  await createProject({ key: "CORE", name: "Core" });
  const firstIssue = await createIssue({
    project: "CORE",
    spec: searchSpec,
    title: "Navigation instruments",
  });
  const secondIssue = await createIssue({ project: "CORE", title: "Other work" });
  const comment = await createComment(secondIssue.key, { body: "Move the astrolabe diagram." });
  const thirdIssue = await createIssue({ project: "CORE", title: "Unrelated" });
  await createMessage(thirdIssue.key, { body: "No instruments here." }, searchSession);

  return {
    commentID: comment.id,
    commentIssueKey: secondIssue.key,
    firstIssueKey: firstIssue.key,
  };
}

export async function openSearchFromRail(page: Page, isPhone: boolean): Promise<void> {
  await page.goto("/");
  if (isPhone) {
    await page.getByRole("button", { name: "Open navigation" }).click();
  }
  await page.getByRole("button", { name: /^search/i }).click();
}
