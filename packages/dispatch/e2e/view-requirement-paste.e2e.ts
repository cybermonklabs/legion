/** The server decides that the child is a view on its parent's spec (`view_of` of its tree
 *  read), which decides the schema the page gives the spec editor, and it stores what the live
 *  editor pastes (`/ws/doc`) as the blocks and the coverage the row reads back. */
import { type Result, routeUrl } from "../web/src/api/paths";

import {
  baseUrl,
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifactVersion,
  getIssueArtifactText,
  getIssueCoverage,
  userHeaders,
} from "./api";
import { type Clipboard, connectedDot, documentEditor, paste, placeCaret } from "./editor";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

// The spec of check c-view-editor: the page's editor makes no requirement block on a view, and
// makes one on the view's ancestor (R23). A child born converted under a parent whose spec holds
// a requirement is a view on that spec while its own spec holds none, and its document editors
// get a schema in which a requirement may not be created (`editorBlockSchema`,
// web/src/features/doc/schema.ts). The parent is no view, so its editor gets the compiled one.

/** The words the paste carries, which each editor keeps whether or not it makes the block. */
const PASTED = "Pasted words.";

/** A requirement block as an editor's clipboard carries one: its HTML, which the editor reads as
 *  it reads every paste that carries HTML, and its markdown as the plain text. */
const REQUIREMENT_CLIPBOARD: Clipboard = {
  html: `<section data-proof-block-type="requirement"><p>${PASTED}</p></section>`,
  text: `:::requirement\n${PASTED}\n:::\n`,
};

/** What the editor draws a requirement block as (`typedBlockSpec` in packages/proof-editor). */
const REQUIREMENT_ELEMENT = '[data-proof-block-type="requirement"]';

/** How the coverage read's line for a requirement block of a view's own spec ends, after the
 *  block's id (`viewRequirementReason` in api/coverage.go). */
const VIEW_REQUIREMENT_LINE_END = " is a requirement on a view";

/** One read as alice of a route `e2e/api.ts` has no wrapper for, named by its own path pattern,
 *  as that file names each of its routes. */
async function readAsAlice<T>(pattern: string, params: object, query: object = {}): Promise<T> {
  const path = routeUrl(pattern, params, query);
  const response = await fetch(new URL(path, baseUrl), { headers: await userHeaders("alice") });
  if (!response.ok) {
    throw new Error(`GET ${path} failed: ${response.status} ${await response.text()}`);
  }
  return (await response.json()) as T;
}

/** `GET /api/v1/issues/{key}/tree?view=spec`, whose every view answers `view_of`: the issue
 *  whose spec this issue is a view on, or null. */
function specTree(key: string): Promise<Result<"/api/v1/issues/{key}/tree", "get">> {
  return readAsAlice("/api/v1/issues/{key}/tree", { key }, { view: "spec" });
}

/** The blocks read of the issue's spec, `GET /api/v1/issues/{key}/artifacts/{slug}/blocks`. */
function specBlocks(
  key: string
): Promise<Result<"/api/v1/issues/{key}/artifacts/{slug}/blocks", "get">> {
  return readAsAlice("/api/v1/issues/{key}/artifacts/{slug}/blocks", { key, slug: "spec" });
}

/** The words of each requirement block the blocks read answers for the issue's spec, each the
 *  span of the spec's text the block covers, as `blockId` (e2e/editor.ts) reads a block's words. */
async function requirementWords(key: string): Promise<string[]> {
  const [{ markdown }, blocks] = await Promise.all([
    getIssueArtifactText(key, "spec"),
    specBlocks(key),
  ]);
  return blocks
    .filter((block) => block.type === "requirement")
    .map((block) => markdown.slice(block.from, block.to));
}

/** The markdown of the issue's spec at its latest stored version, empty while it has none: what
 *  the room stored once it settled the paste, which the live text holds before it is stored. */
async function storedSpec(key: string, specId: string): Promise<string> {
  const { version } = await getIssueArtifactText(key, "spec");
  return version === null ? "" : (await getArtifactVersion(specId, version)).markdown;
}

/** Opens the issue's page at `/issues/<key>`, waits until the spec's editor shows `words` and its
 *  document is connected, puts the caret after `words` and pastes the requirement block there.
 *  `openWithCaret` (e2e/editor.ts) is not used: it creates an issue of its own. */
async function pasteAfter(page: Page, key: string, words: string): Promise<void> {
  await page.goto(`/issues/${key}`);
  await expect(documentEditor(page)).toContainText(words);
  await expect(connectedDot(page)).toHaveText("connected");
  await placeCaret(page, "after", words);
  await paste(page, REQUIREMENT_CLIPBOARD);
}

// SLOW(>=2s): two issue pages, each waiting for its document to connect, and two pastes each
// waited for until the room stores it as a version, which the room's settlement writes 2 s after
// the edit (docs.New's default settle).
test("a pasted requirement block makes no requirement in a view's spec editor and makes one in its ancestor's", async ({
  browser,
}) => {
  await createProject({ key: "VIEW", name: "Views" });
  const parent = await createIssue({
    project: "VIEW",
    spec: "Parent words.\n\n:::requirement{#r1}\nThe requirement the child serves.\n:::\n",
    title: "The parent a child is a view on",
  });
  // Born converted under the parent, with a spec that holds no requirement and one check that
  // serves the parent's: a view on the parent's spec.
  const child = await createIssue({
    parent: parent.key,
    project: "VIEW",
    spec: "Child words.\n",
    title: "The child whose spec is a view",
  });
  await createIssueArtifact(child.key, {
    content: `:::check{#c1 serves="dispatch://${parent.key}/spec#b-r1" surface="browser" run="bunx playwright test --config e2e/playwright.config.ts --project=chromium e2e/view-requirement-paste.e2e.ts"}\nThe child proves the requirement of its parent.\n:::\n`,
    name: "verification.md",
  });
  await expect
    .poll(async () => (await specTree(child.key)).view_of, {
      message: `the tree read of ${child.key} answers no view_of naming ${parent.key}`,
    })
    .toBe(parent.key);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();

    // The view: the editor keeps the words as text and draws no requirement block.
    await pasteAfter(page, child.key, "Child words.");
    const childEditor = documentEditor(page);
    await expect(
      childEditor,
      `the spec editor of ${child.key} holds no "${PASTED}" after the paste`
    ).toContainText(PASTED);
    await expect(
      childEditor.locator(REQUIREMENT_ELEMENT),
      `the spec editor of ${child.key} made a requirement block of the paste`
    ).toHaveCount(0);

    // Once the stored spec holds the words, the blocks read answers no requirement block and
    // the coverage read no line for one. The coverage reads the block index, so it is read once
    // the index owes no document of the child.
    await expect
      .poll(() => storedSpec(child.key, child.primary_artifact_id), {
        message: `the spec of ${child.key} stores no version holding "${PASTED}"`,
      })
      .toContain(PASTED);
    const childRequirements = (await specBlocks(child.key))
      .filter((block) => block.type === "requirement")
      .map((block) => block.id);
    expect(
      childRequirements,
      `${child.key} holds a requirement block after the paste: ${childRequirements.join(", ")}`
    ).toEqual([]);
    // The coverage read that owes no document is the one whose lines are read.
    let settled: Result<"/api/v1/issues/{key}/coverage", "get"> | undefined;
    await expect
      .poll(
        async () => {
          settled = await getIssueCoverage(child.key);
          return settled.owed_documents ?? [];
        },
        { message: `the coverage of ${child.key} still owes the block index of its documents` }
      )
      .toEqual([]);
    const viewLines = (settled?.gaps ?? []).filter((line) =>
      line.endsWith(VIEW_REQUIREMENT_LINE_END)
    );
    expect(
      viewLines,
      `the coverage of ${child.key} answers a gap for the paste: ${viewLines.join("; ")}`
    ).toEqual([]);

    // The ancestor: the same paste makes a requirement block, drawn and stored as one.
    await pasteAfter(page, parent.key, "Parent words.");
    await expect(
      documentEditor(page).locator(REQUIREMENT_ELEMENT).filter({ hasText: PASTED }),
      `the spec editor of ${parent.key} made no requirement block of the paste`
    ).not.toHaveCount(0);
    await expect
      .poll(() => storedSpec(parent.key, parent.primary_artifact_id), {
        message: `the spec of ${parent.key} stores no version holding "${PASTED}"`,
      })
      .toContain(PASTED);
    await expect
      .poll(() => requirementWords(parent.key), {
        message: `the spec editor of ${parent.key} made no requirement block of the paste`,
      })
      .toContainEqual(expect.stringContaining(PASTED));
  } finally {
    await alice.close();
  }
});
