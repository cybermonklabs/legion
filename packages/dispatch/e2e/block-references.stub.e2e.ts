/** What the **Referenced by** panel draws over one fixed answer: a block of another document
 *  citing a block, two paragraphs of one document citing an issue, and a comment citing two
 *  blocks of the document on screen - each as a row linked to the block it cites.
 *
 *  Every row here reads what the page draws, so each runs against a recording rather than
 *  against a harness (packages/dispatch/AGENTS.md, "The stubbed layer"). The rows that edit a
 *  document or a comment and then read the link again - the pinned link, the upload, and the
 *  three that wait on a live event - stayed in `block-references.e2e.ts`.
 */
import { expect } from "@playwright/test";

import { createComment, createIssue, createIssueArtifact, createProject, getArtifact } from "./api";
import { blockId } from "./editor";
import { stubbed } from "./stub/stub-test";

/** One document citing a block of another, both on one issue. */
const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Block references" });
  const plan = await createIssueArtifact(issue.key, {
    content: "Intro.\n\nShip the block graph first.\n",
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  const cited = await blockId(plan.artifact.id, "Ship the block graph first.");
  const paragraph = `Decided: dispatch://${issue.key}/artifact/plan-md#b-${cited} holds.`;
  const decisions = await createIssueArtifact(issue.key, {
    content: `Context.\n\n${paragraph}\n`,
    name: "decisions.md",
  });
  expect(decisions.artifact.slug).toBe("decisions-md");
  const citing = await blockId(decisions.artifact.id, paragraph);
  return { citing, issue: issue.key, paragraph };
});

/** Two paragraphs of one document version citing one issue. The version is read where the
 *  seeding runs: the rows below are about what the panel draws, not about what it was. */
const twiceTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Cited twice" });
  const first = `First, dispatch://${target.key} decides it.`;
  const second = `Then dispatch://${target.key} ships it.`;
  const source = await createIssue({
    project: "CORE",
    spec: `${first}\n\n${second}\n`,
    title: "Citing source",
  });
  const firstId = await blockId(source.primary_artifact_id, first);
  const secondId = await blockId(source.primary_artifact_id, second);
  const artifact = await getArtifact(source.primary_artifact_id);
  expect(Math.max(...(artifact.versions ?? []).map((version) => version.number))).toBe(1);
  return { first, firstId, second, secondId, source: source.key, target: target.key };
});

/** One comment citing two blocks of the document on screen. */
const commentTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Two blocks cited" });
  const callout = (id: string, text: string) =>
    `:::callout{#${id} kind="note" title=""}\n${text}\n:::`;
  const plan = await createIssueArtifact(issue.key, {
    content: `${callout("ship_it", "Ship the block graph.")}\n\n${callout("hold_line", "Hold the line.")}\n`,
    name: "plan.md",
  });
  expect(plan.artifact.slug).toBe("plan-md");
  // `encodeBlockId` writes `_` as %5F, the form the comment cites and each row's link carries.
  const ship = "#b-ship%5Fit";
  const hold = "#b-hold%5Fline";
  const doc = `dispatch://${issue.key}/artifact/plan-md`;
  await createComment(issue.key, { body: `Ship per ${doc}${ship}, hold per ${doc}${hold}.` });
  return { hold, issue: issue.key, ship };
});

test("a block of another document citing a block shows as that document's row, linked to the citing block @pr", async ({
  page,
  scene,
}) => {
  await page.goto(`/issues/${scene.issue}/artifacts/plan-md`);
  const references = page.getByRole("region", { name: "Referenced by" });
  // One row: the citing paragraph, shown as the document holding it and linked to that block.
  await expect(references.getByRole("link", { name: `Artifact · ${scene.issue}` })).toHaveAttribute(
    "href",
    `/issues/${scene.issue}/artifacts/decisions-md#b-${scene.citing}`
  );
  await expect(references.getByText(scene.paragraph, { exact: true })).toBeVisible();
  await expect(references.getByRole("heading")).toHaveText("Referenced by (1)");
});

twiceTest(
  "two paragraphs of one document version citing an issue are two rows under that document, each linked to its block @pr",
  async ({ page, scene }) => {
    const { first, firstId, second, secondId, source } = scene;
    await page.goto(`/issues/${scene.target}`);
    // The target's own spec is attached to it, and each citing paragraph counts once.
    const toggle = page.getByRole("button", { name: /^Referenced by \(3\)$/ });
    await expect(toggle).toBeVisible();
    await toggle.click();
    const references = page.getByRole("region", { name: "Referenced by" });
    await expect(references.getByRole("link", { name: `Artifact · ${source}` })).toHaveAttribute(
      "href",
      `/issues/${source}/spec`
    );
    for (const [text, other, id] of [
      [first, second, firstId],
      [second, first, secondId],
    ] as const) {
      const row = references
        .getByRole("listitem")
        .filter({ hasText: text })
        .filter({ hasNotText: other });
      await expect(row).toHaveCount(1);
      await expect(row.getByText(text, { exact: true })).toBeVisible();
      await expect(row.locator(`a[href="/issues/${source}/spec#b-${id}"]`)).toHaveCount(1);
    }
  }
);

commentTest(
  "a comment citing two blocks of the shown document is two rows, each linked to the block it cites @pr",
  async ({ page, scene }) => {
    await page.goto(`/issues/${scene.issue}/artifacts/plan-md`);
    const references = page.getByRole("region", { name: "Referenced by" });
    await expect(references.getByRole("heading")).toHaveText("Referenced by (2)");
    // One source, the comment, holding one row for each block of this document it cites.
    const sources = references.getByRole("list").first().locator(":scope > li");
    await expect(sources).toHaveCount(1);
    await expect(sources.getByRole("link", { name: `Comment · ${scene.issue}` })).toBeVisible();
    const rows = sources.locator(":scope > ul > li");
    await expect(rows).toHaveCount(2);
    for (const [id, other, fragment] of [
      ["ship_it", "hold_line", scene.ship],
      ["hold_line", "ship_it", scene.hold],
    ] as const) {
      const row = rows
        .filter({ has: page.getByRole("link", { name: `block ${id}`, exact: true }) })
        .filter({ hasNot: page.getByRole("link", { name: `block ${other}`, exact: true }) });
      await expect(row).toHaveCount(1);
      await expect(row.getByRole("link", { name: `block ${id}`, exact: true })).toHaveAttribute(
        "href",
        `/issues/${scene.issue}/artifacts/plan-md${fragment}`
      );
    }
  }
);
