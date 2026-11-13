import { CONTROL_LABELS } from "../web/src/features/issue/page-words";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  listComments,
} from "./api";
import { blockId, specAddress } from "./editor";
import { gotoIssue, nodeRows } from "./issue-page";
import { serveWithoutDisplayTrees } from "./strip-trees";
import { expect, test } from "./test";

// The decision records end to end, against the real server: an agent writes `decisions.md` with
// one decision that links two spec blocks, and a person reads it on the Spec tab beside those
// blocks and on the Decisions tab, answers it with Keep and Change, and sees each block the
// decision has changed since marked out of date until that block changes too.
//
// Every block id comes from `GET /api/v1/artifacts/<id>/blocks` (`blockId`), never from a guess,
// and every seed goes through the API the agent uses.

const STALE_TITLE = "A decision that shaped this block changed after it.";
const DECISION = "Store notices in a table";
const REASON = "Chosen for the simplest storage.";
/** The decision whose text is no plain paragraph: a list below its heading, with a reference in
 *  one item and an issue key in the other. */
const LISTED = "Name the notice store";
const LISTED_OPENING = "Notices live in a table, as";
const LISTED_CHANGE = "Measure the file store first.";

// @pr: The Decisions tab's only row: a decision links the spec blocks it shaped, goes out of date with them, and takes Keep and Change.
test("a decision links the spec blocks it shaped, goes out of date with them, and takes Keep and Change @pr", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Notices are lost today.\n\nStore each notice.\n\nShow the newest first.\n",
    title: "Decision records",
  });
  const spec = await getArtifact(issue.primary_artifact_id ?? "");
  const stored = await blockId(spec.id, "Store each notice.");
  const newest = await blockId(spec.id, "Show the newest first.");
  const specVersions = (spec.versions ?? []).length;

  const reference = `dispatch://${issue.key}/spec`;
  const decisions = await createIssueArtifact(issue.key, {
    content: `## ${DECISION}\n\n${REASON} Shapes ${reference}#b-${stored} and ${reference}#b-${newest}.\n`,
    name: "decisions.md",
  });
  expect(decisions.artifact.slug).toBe("decisions-md");

  // Writing decisions.md writes the spec no version of its own.
  expect((await getArtifact(spec.id)).versions).toHaveLength(specVersions);

  // The Spec tab: the decision is a margin card, and no block is out of date yet.
  await page.goto(await specAddress(issue.key));
  const cards = page.getByRole("article", { name: `Decision: ${DECISION}` });
  await expect(cards).toHaveCount(2);
  await expect(page.locator(`[data-margin-item$=":${stored}"]`)).toHaveCount(1);
  await expect(page.locator(`[data-margin-item$=":${newest}"]`)).toHaveCount(1);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(0);

  // The Decisions preset: the decision is a node of the issue's tree, the preset opens on it
  // because it waits for the reader's review, and its body holds Keep and Change. Until
  // 2026-11-06 this read the Decisions tab's four sections of cards
  // (`features/issue/DecisionsPanel.tsx`, `region "Waiting for your review"`), which
  // dispatch://INFRA-1196 replaced with the tree: the sections are gone, so what waits for the
  // reader is the decision whose node carries Keep, and a shaped link that went out of date is
  // a word on that node's own chip instead of a line in a card.
  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  const decision = nodeRows(page).filter({ hasText: DECISION }).first();
  await expect(decision).toBeVisible();
  await expect(page.getByText(CONTROL_LABELS.target_stale)).toHaveCount(0);

  // Change opens the composer on the decision's own text and posts a comment anchored to it.
  await decision.focus();
  await decision.getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision }).click();
  const composer = page.getByRole("form", { name: `Change ${DECISION}` });
  await expect(composer.getByTestId("decision-quote")).toContainText(REASON);
  await composer.getByRole("textbox").fill("Measure the file store first.");
  await composer.getByRole("button", { name: CONTROL_LABELS.send_change }).click();
  await expect(composer).toBeHidden();

  // The agent changes the decision's text: both blocks it links are out of date, on the
  // Decisions preset and as a mark on the Spec tab.
  await editArtifact(decisions.artifact.id, {
    ops: [{ find: REASON, op: "replace", with: `${REASON} Measured against the file store.` }],
    summary: "measured",
  });
  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  await expect(page.getByText(CONTROL_LABELS.target_stale)).toHaveCount(2);
  await page.goto(await specAddress(issue.key));
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(2);

  // The agent changes one of those spec blocks: its mark goes and the other stays.
  await editArtifact(spec.id, {
    ops: [{ find: "Store each notice.", op: "replace", with: "Store each notice in a table." }],
    summary: "stored",
  });
  await page.goto(await specAddress(issue.key));
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(1);
  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  await expect(page.getByText(CONTROL_LABELS.target_stale)).toHaveCount(1);

  // Keep records the reader's agreement: the decision stops waiting for the reader's review,
  // so the control that takes it there is gone and the decision is still a node of the tree.
  const waiting = nodeRows(page).filter({ hasText: DECISION }).first();
  await waiting.focus();
  await waiting.getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision }).click();
  await expect(
    page.getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
  ).toHaveCount(0);
  await expect(nodeRows(page).filter({ hasText: DECISION }).first()).toBeVisible();
});

// The same preset for a decision whose text the server read into a tree with more in it than
// one paragraph: a list of two items that cites the spec and names an issue key. The quote a
// Change anchors on has to be the words that reading flattens to - one space where one list
// item ends and the next begins (`pmdoc.buildFlattenedText`) - or the server finds nothing to
// anchor the Change on and refuses it. Until dispatch://INFRA-1196 this row read the Decisions
// tab's card (`DecisionsPanel.tsx`), which drew the list as a list with its link and its code
// span; the decision is a node of the tree now, whose body draws its text as written, so what
// this row holds the node to is the anchor its Change writes (`DecisionActions.tsx`).
// @pr: The Decisions preset anchors the Change of a decision written as a list on the words the server flattens that list to.
test("a decision written as a list anchors its Change on the words that list flattens to @pr", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Notices are lost today.\n\nStore each notice.\n",
    title: "Decisions written as a list",
  });
  const reference = `dispatch://${issue.key}/spec`;
  // The markdown an agent writes and the words the server matches a quote against differ by
  // exactly what this row is about: the link's target and the backticks around the key are the
  // markdown's alone, and the end of the first item is one space from the start of the second.
  const list = [
    `- ${LISTED_OPENING} [the spec](${reference}) asks.`,
    `- A \`${issue.key}\`-style key names where each notice came from.`,
  ].join("\n");
  const words = `${LISTED_OPENING} the spec asks. A ${issue.key}-style key names where each notice came from.`;
  const decisions = await createIssueArtifact(issue.key, {
    content: `## ${LISTED}\n\n${list}\n\nNothing else about a notice changes.\n`,
    name: "decisions.md",
  });
  const listBlock = await blockId(decisions.artifact.id, list);

  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  const decision = nodeRows(page).filter({ hasText: LISTED }).first();
  await expect(decision).toBeVisible();

  // Change quotes that first block as the server flattens it, and the server finds those words
  // in the document's own text: the comment it writes is anchored to the list block the quote
  // names, which a quote with its items run together would never have reached.
  await decision.focus();
  await decision.getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision }).click();
  const composer = page.getByRole("form", { name: `Change ${LISTED}` });
  await expect(composer.getByTestId("decision-quote")).toHaveText(words);
  await composer.getByRole("textbox").fill(LISTED_CHANGE);
  await composer.getByRole("button", { name: CONTROL_LABELS.send_change }).click();
  await expect(composer).toBeHidden();
  await expect
    .poll(async () => {
      const comments = await listComments(issue.key, decisions.artifact.id);
      return comments.find((comment) => comment.body === LISTED_CHANGE)?.anchor;
    })
    .toMatchObject({ block_id: listBlock, orphaned: false, quote: words });
});

// The same decision from a server that answers no reading of its text, which is what a pod of
// the build before this one serves during a rolling deploy: every `*_tree` member is taken out
// of the answers, as `parse-count.e2e.ts`'s own control takes them out. The node still offers
// its Change, quoting what the browser has when the server sent it no reading to flatten. Until
// dispatch://INFRA-1196 this row also read the Decisions tab's card drawing each block once in
// a slot of its own (`DecisionsPanel.tsx`); the node's body draws the decision's text once, as
// written, and has no slots for a text to be repeated in.
// @pr: The Decisions preset of an answer with no reading still anchors a Change on the decision's first block.
test("a decision whose answer carries no reading still quotes its first block @pr", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "Notices are lost today.\n",
    title: "Decisions with no reading",
  });
  await createIssueArtifact(issue.key, {
    content: `## ${LISTED}\n\nNotices live in a table.\n\nUnsure: the index may be slow.\n`,
    name: "decisions.md",
  });

  await serveWithoutDisplayTrees(page);

  await gotoIssue(page, `/issues/${issue.key}/decisions`);
  const decision = nodeRows(page).filter({ hasText: LISTED }).first();
  await expect(decision).toBeVisible();

  // The first block's own markdown, which is what the browser has when the server sent it no
  // reading to flatten.
  await decision.focus();
  await decision.getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision }).click();
  await expect(
    page.getByRole("form", { name: `Change ${LISTED}` }).getByTestId("decision-quote")
  ).toHaveText("Notices live in a table.");
});
