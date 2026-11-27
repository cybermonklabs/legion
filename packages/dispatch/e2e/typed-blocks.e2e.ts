import {
  baseUrl,
  createComment,
  createIssue,
  createProject,
  createProjectDocument,
  userHeaders,
} from "./api";
import {
  builtSchemaVersion,
  makeStagedTypeCreatable,
  restoreStagedType,
  servedStagedCreatableSince,
  shippedStagedCreatableSince,
  STAGED_TYPE_REFUSAL,
} from "./block-schema";
import { documentEditor } from "./editor";
import { expect, type Locator, test } from "./test";
import { asUser } from "./users";

/** Puts plan_role back at the creatable_since the store ships it with, and waits until the server
 *  serves it so. The last row has the store create plan_role (makeStagedTypeCreatable), and the
 *  reset keeps the schema's own nodes (e2e/seed.ts), so every row does this before and after: a
 *  row that failed before its end must not leave the next one on another schema. */
async function shipStagedType(): Promise<void> {
  await restoreStagedType();
  await expect.poll(() => servedStagedCreatableSince()).toBe(shippedStagedCreatableSince());
}

test.beforeEach(shipStagedType);
test.afterEach(shipStagedType);

test("typedBlockDirectiveRoundTrip", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n',
    name: "Typed blocks",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/typed-blocks");

    const callout = documentEditor(page).locator('[data-proof-block-type="callout"]');
    await expect(callout).toContainText("Body text.");
    await expect(callout).toHaveAttribute("data-block-id", "callout-1");
    await expect(callout.locator("[data-proof-block-name]")).toHaveText("callout");
    await expect(callout.locator('[data-proof-block-attribute="kind"]')).toHaveText("warning");
    await expect(callout.locator('[data-proof-block-attribute="title"]')).toHaveText("Read this");
  } finally {
    await alice.close();
  }
});

// A header listing every node attribute would show a reader `callout / kind / title / blockId` as
// bare lines - the internal block id included - above the block's own words.
// @pr: The typed-block header's own row: a reader sees the author's attributes and none of the internals, the block id included.
test("a typed block's header shows the author's attributes and none of the internals @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: ':::callout{#callout-1 kind="warning" title="Read this"}\nBody text.\n:::\n',
    name: "Typed blocks",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/typed-blocks");
    const callout = documentEditor(page).locator('[data-proof-block-type="callout"]');
    const header = callout.locator("[data-proof-block-summary]");
    await expect(header).toBeVisible();

    // The author's own attributes, each in its own role.
    await expect(header.locator("[data-proof-block-name]")).toHaveText("callout");
    await expect(header.locator("[data-proof-block-badge]")).toHaveText("warning");
    await expect(header.locator("[data-proof-block-title]")).toHaveText("Read this");

    // Nothing the author did not write: no block id, anywhere in the header.
    await expect(header).not.toContainText("callout-1");
    await expect(header).not.toContainText("blockId");
    await expect(header.locator('[data-proof-block-attribute="blockId"]')).toHaveCount(0);

    // The header reads as a label rather than a stack of prose lines: one row.
    const box = await header.boundingBox();
    expect(box?.height ?? 0).toBeLessThanOrEqual(40);
  } finally {
    await alice.close();
  }
});

/**
 * Each attribute in `header` whose value is not drawn beside its label, as
 * `<label>: <what is wrong>`: the value's text starts on another line than the label's text, or
 * further from the label (its colon included) than the list's own 8 px gap. The texts are read
 * through a range, so a margin or a padding that moves the words and not the box is seen too.
 */
function attributesOffTheirLabels(header: Locator): Promise<string[]> {
  return header.locator("[data-proof-block-attributes] > dt").evaluateAll((labels) =>
    labels.flatMap((label) => {
      const value = label.nextElementSibling;
      if (value?.tagName !== "DD") return [`${label.textContent}: no value follows it`];
      const words = (element: Element) => {
        const range = document.createRange();
        range.selectNodeContents(element);
        return range.getBoundingClientRect();
      };
      const labelWords = words(label);
      const valueWords = words(value);
      const after = valueWords.left - label.getBoundingClientRect().right;
      const wrong = [
        ...(Math.abs(valueWords.top - labelWords.top) > 1
          ? [`its value's line starts at ${valueWords.top}, its label's at ${labelWords.top}`]
          : []),
        ...(after > 8.5 ? [`its value starts ${after} px after its label`] : []),
      ];
      return wrong.length === 0 ? [] : [`${label.textContent}: ${wrong.join("; ")}`];
    })
  );
}

// A typed block's attributes read as `name: value` pairs on the header's row. The document's
// typography (`.prose`, in a document and in a body alike) gives every `dt` a top margin and
// every `dd` an indent, which drew each value above its label and far to its right.
//
// This build reads plan_role and creates it only from block schema version 8 (blocks.json), so
// the document's write is refused first, 400 INVALID_MARKDOWN. The row then has the store create
// plan_role from this build's version and reads the block that build stores.
test("a typed block's attribute values sit beside their labels, in a document and in a body", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const block =
    ':::plan_role{#prober type="testing" model="prober-model"}\nProbe the change.\n:::\n';
  const refused = await fetch(new URL("/api/v1/projects/CORE/artifacts", baseUrl), {
    body: JSON.stringify({ content: block, name: "Typed blocks" }),
    headers: { "Content-Type": "application/json", ...(await userHeaders("alice")) },
    method: "POST",
  });
  expect(refused.status).toBe(400);
  expect(await refused.json()).toMatchObject({
    code: "INVALID_MARKDOWN",
    error: expect.stringContaining(STAGED_TYPE_REFUSAL),
  });
  // This models a store whose manifest creates plan_role at version 6, not a real version-8 build.
  await makeStagedTypeCreatable();
  await expect.poll(() => servedStagedCreatableSince()).toBe(builtSchemaVersion);
  await createProjectDocument("CORE", { content: block, name: "Typed blocks" });
  const issue = await createIssue({ project: "CORE", title: "A role in a comment" });
  const comment = await createComment(
    issue.key,
    { body: block },
    { actor: { id: "e2e-typed-blocks-agent", kind: "session" }, as: "agent" }
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto("/projects/CORE/documents/typed-blocks");
    const inDocument = documentEditor(page).locator(
      '[data-proof-block-type="plan_role"] [data-proof-block-summary]'
    );
    await expect(inDocument.locator('[data-proof-block-attribute="model"]')).toHaveText(
      "prober-model"
    );
    // Soft, so a run reports the body below as well.
    expect.soft(await attributesOffTheirLabels(inDocument)).toEqual([]);

    await page.goto(`/issues/${issue.key}/conversation`);
    const turn = page
      .getByRole("list", { name: "Conversation turns" })
      .locator(`:scope > li[data-turn="comment:${comment.id}"]`);
    await turn.getByRole("button", { name: "Expand thread" }).click();
    const inBody = turn.locator('[data-proof-block-type="plan_role"] [data-proof-block-summary]');
    await expect(inBody.locator('[data-proof-block-attribute="model"]')).toHaveText("prober-model");
    expect(await attributesOffTheirLabels(inBody)).toEqual([]);
  } finally {
    await alice.close();
  }
});
