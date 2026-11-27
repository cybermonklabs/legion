/** The search fixture the palette rows are read over, and the control that opens it. Shared by
 *  `search.e2e.ts` and the rows of that family which run against a recording
 *  (`search.stub.e2e.ts`). */
import type { Page } from "@playwright/test";

import { createComment, createIssue, createMessage, createProject, type RowKey } from "./api";
export const searchTerm = "astrolabe";

/** `word` with the row's own project key run onto it, lower-cased, as the one word a real-stack
 *  row of `search.e2e.ts` writes and searches for: `astrolabe` in `COREQZVMK` is
 *  `astrolabecoreqzvmk`. Search reads every project of the stack, so a word another row also
 *  writes (`astrolabe`, `sextant`) lists that row's items beside the row's own, and the key
 *  written beside the word (`astrolabe COREQZVMK`) keeps them out only while the two match
 *  something together: a query whose words match nothing whole is searched again for its rarest
 *  words ORed (`searchRarestWords`, packages/envoy/internal/dispatch/api/search_request.go), any
 *  of which another row's item may hold. One word holding the key is matched by the row's own
 *  items alone, and a query of one word is never broadened. It stays one word, so the palette
 *  marks it whole, the address carries it as `?q=`, and the document's highlight, which is the
 *  query found whole in the text (`setSearchHighlights`, web/src/features/doc/ProofDocument.tsx),
 *  is that word. `word` keeps its case (`Astrolabe` starts a title). The stubbed rows search for
 *  `searchTerm` itself, the word their recordings were made over. */
export function keyedSearchWord(word: string, project: string): string {
  return `${word}${project.toLowerCase()}`;
}

/** The first issue's spec, which holds `term` twice. */
function searchSpec(term: string): string {
  return `# Instruments\n\nThe ${term} measures altitude. Every ${term} is brass.\n`;
}

export const searchSession = {
  actor: { kind: "session" as const, id: "e2e-search" },
  as: "agent" as const,
};

export interface SearchFixture {
  readonly commentID: string;
  readonly commentIssueKey: string;
  readonly firstIssueKey: string;
}

/** `project` is the key of the project the fixture is made in, and `term` the word its spec and
 *  its comment hold: `search.e2e.ts` passes its row's own (`rowKey`, e2e/test.ts, and
 *  `keyedSearchWord`), and the stubbed rows replay a recording made under `CORE` and
 *  `searchTerm`. */
export async function seedSearchFixture(
  project: RowKey,
  term = searchTerm
): Promise<SearchFixture> {
  await createProject({ key: project, name: "Core" });
  const firstIssue = await createIssue({
    project,
    spec: searchSpec(term),
    title: "Navigation instruments",
  });
  const secondIssue = await createIssue({ project, title: "Other work" });
  const comment = await createComment(secondIssue.key, { body: `Move the ${term} diagram.` });
  const thirdIssue = await createIssue({ project, title: "Unrelated" });
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
