import {
  createIssue,
  createIssueArtifact,
  createProject,
  editArtifact,
  getArtifact,
  listComments,
} from "./api";
import { blockId } from "./editor";
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
  await page.goto(`/issues/${issue.key}/spec`);
  const cards = page.getByRole("article", { name: `Decision: ${DECISION}` });
  await expect(cards).toHaveCount(2);
  await expect(page.locator(`[data-margin-item$=":${stored}"]`)).toHaveCount(1);
  await expect(page.locator(`[data-margin-item$=":${newest}"]`)).toHaveCount(1);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(0);

  // The Decisions tab lists it under the reader's review, with both blocks and neither out of
  // date.
  await page.goto(`/issues/${issue.key}/decisions`);
  const waiting = page.getByRole("region", { name: "Waiting for your review" });
  await expect(waiting.getByRole("heading", { name: DECISION })).toBeVisible();
  await expect(waiting.getByText("out of date")).toHaveCount(0);

  // Change opens the composer on the decision's own text and posts a comment anchored to it.
  await waiting.getByRole("button", { name: "Change" }).click();
  const composer = page.getByRole("form", { name: `Change ${DECISION}` });
  await expect(composer.getByTestId("decision-quote")).toContainText(REASON);
  await composer.getByRole("textbox").fill("Measure the file store first.");
  await composer.getByRole("button", { name: "Send" }).click();
  await expect(composer).toBeHidden();

  // The agent changes the decision's text: both blocks it links are out of date, on the Decisions
  // tab and as a mark on the Spec tab.
  await editArtifact(decisions.artifact.id, {
    ops: [{ find: REASON, op: "replace", with: `${REASON} Measured against the file store.` }],
    summary: "measured",
  });
  await page.goto(`/issues/${issue.key}/decisions`);
  await expect(page.getByText("out of date")).toHaveCount(2);
  await page.goto(`/issues/${issue.key}/spec`);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(2);

  // The agent changes one of those spec blocks: its mark goes and the other stays.
  await editArtifact(spec.id, {
    ops: [{ find: "Store each notice.", op: "replace", with: "Store each notice in a table." }],
    summary: "stored",
  });
  await page.goto(`/issues/${issue.key}/spec`);
  await expect(page.locator(`[title="${STALE_TITLE}"]`)).toHaveCount(1);
  await page.goto(`/issues/${issue.key}/decisions`);
  await expect(page.getByText("out of date")).toHaveCount(1);

  // Keep records the reader's agreement: the decision leaves the section that waits for it and
  // stands under the decisions that are theirs.
  await page
    .getByRole("region", { name: "Waiting for your review" })
    .getByRole("button", { name: "Keep" })
    .click();
  await expect(
    page.getByRole("region", { name: "Waiting for your review" }).getByRole("heading", {
      name: DECISION,
    })
  ).toHaveCount(0);
  await expect(
    page.getByRole("region", { name: "Your decisions" }).getByRole("heading", { name: DECISION })
  ).toBeVisible();
});

// The same tab for a decision whose text the server read into a tree with more in it than one
// paragraph: a list of two items that cites the spec and names an issue key. The tab draws that
// tree, so what it draws and what it quotes both come from the server's reading, and the quote
// has to be the words that reading flattens to - one space where one list item ends and the
// next begins (`pmdoc.buildFlattenedText`) - or the server finds nothing to anchor the Change
// on and refuses it.
// @pr: The Decisions tab draws a decision written as a list, keeps its link and its key, and anchors its Change on the words the server flattens that list to.
test("a decision written as a list keeps its link and its key, and anchors its Change on the words that list flattens to @pr", async ({
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

  await page.goto(`/issues/${issue.key}/decisions`);
  const card = page
    .getByRole("region", { name: "Waiting for your review" })
    .locator("[data-decision]");
  await expect(card.getByRole("heading", { name: LISTED })).toBeVisible();

  // The list is drawn as a list: one item each, both readable, rather than one run of words.
  const items = card.getByRole("listitem");
  await expect(items).toHaveCount(2);
  await expect(items.first()).toContainText(LISTED_OPENING);
  await expect(items.last()).toHaveText(
    `A ${issue.key}-style key names where each notice came from.`
  );

  // The reference is a link into the app and the key stands in a code span, both of which the
  // reading takes from the decision's own markdown: the words of a block hold neither a link's
  // target nor the backticks that end a key.
  await expect(card.locator(`a[data-dispatch-ref="${reference}"]`)).toHaveAttribute(
    "href",
    `/issues/${issue.key}/spec`
  );
  await expect(card.locator("code")).toHaveText(issue.key);

  // Change quotes that first block as the server flattens it, and the server finds those words
  // in the document's own text: the comment it writes is anchored to the list block the quote
  // names, which a quote with its items run together would never have reached.
  await card.getByRole("button", { name: "Change" }).click();
  const composer = page.getByRole("form", { name: `Change ${LISTED}` });
  await expect(composer.getByTestId("decision-quote")).toHaveText(words);
  await composer.getByRole("textbox").fill(LISTED_CHANGE);
  await composer.getByRole("button", { name: "Send" }).click();
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
// of the answers, as `parse-count.e2e.ts`'s own control takes them out. The tab then splits the
// markdown itself, as it did before this change and as the margin card still does, so each
// block is drawn once, with its own text and nothing else. The reading this row refuses is the
// one two rounds of this change got wrong: a body handed a text that is not its own draws that
// whole text in every slot.
// @pr: The Decisions tab of an answer with no reading draws each block once, with its own text.
test("a decision whose answer carries no reading is drawn block by block @pr", async ({ page }) => {
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

  await page.route(/\/api\/v1\//, async (route) => {
    const response = await route.fetch();
    const body = (await response.text()).replaceAll(/"[a-z_]+_tree":/g, '"dropped_tree":');
    const headers = { ...response.headers() };
    delete headers["content-encoding"];
    delete headers["content-length"];
    await route.fulfill({ body, headers, status: response.status() });
  });

  await page.goto(`/issues/${issue.key}/decisions`);
  const card = page
    .getByRole("region", { name: "Waiting for your review" })
    .locator("[data-decision]");
  await expect(card.getByRole("heading", { name: LISTED })).toBeVisible();

  // Two blocks below the heading, each drawn once and holding its own text: the doubt in the
  // accented slot, the rest outside it, and the heading line nowhere in the body.
  await expect(card.getByTestId("decision-unsure")).toHaveText("Unsure: the index may be slow.");
  await expect(card.getByText("Notices live in a table.")).toHaveCount(1);
  await expect(card).not.toContainText("##");

  // The Change still has a quote on this path: the first block's own markdown, which is what
  // the browser has when the server sent it no reading to flatten.
  await card.getByRole("button", { name: "Change" }).click();
  await expect(
    page.getByRole("form", { name: `Change ${LISTED}` }).getByTestId("decision-quote")
  ).toHaveText("Notices live in a table.");
});
