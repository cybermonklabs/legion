import { expect, type Page, test } from "@playwright/test";

import { createIssue, createProject, getArtifact, listIssueArtifacts } from "./api";
import { documentEditor } from "./editor";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { signIn } from "./users";

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
});

/** The whole press a person makes on the Documents tab: `New document`, the title, `Create`. */
async function createDocument(page: Page, title: string): Promise<void> {
  await page.getByRole("button", { name: "New document" }).click();
  const form = page.getByRole("form", { name: "New document" });
  await form.getByRole("textbox", { name: "Title" }).fill(title);
  await form.getByRole("button", { name: "Create" }).click();
}

/** The issue's document of `slug`, read through the list the Documents tab itself reads. */
async function issueDocument(issue: string, slug: string) {
  const document = (await listIssueArtifacts(issue)).find((item) => item.slug === slug);
  if (document === undefined) {
    throw new Error(`the issue ${issue} holds no document ${slug}`);
  }
  return document;
}

// SLOW(>=2s): four documents made in the browser, each with its own editor mounted on the live
// document, the settlement that versions what was typed into the fourth, and the issue's whole
// Activity read after them.
test("a document's title is corrected and a document made by mistake is removed, from its row and from its own page", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Launch\n\nThe spec itself.\n",
    title: "The two ways back",
  });
  const documents = `/issues/${issue.key}/artifacts`;
  const header = page.getByTestId("artifact-header");

  await page.goto(documents);
  await createDocument(page, "Lauch notes");
  await expect(page).toHaveURL(`${documents}/lauch-notes`);
  await expect(documentEditor(page)).toBeAttached();
  await expect(header.getByRole("heading", { name: "Lauch notes" })).toBeVisible();

  // The correction, from the document's own page.
  await header.getByRole("button", { name: "Correct title" }).click();
  const correction = page.getByRole("form", { name: "Correct title" });
  await correction.getByRole("textbox", { name: "Title" }).fill("Launch notes");
  await correction.getByRole("button", { name: "Save" }).click();
  await expect(header.getByRole("heading", { name: "Launch notes" })).toBeVisible();
  // The write carries the name alone, so the slug does not move: the address every citation of
  // the document holds is the address the person is still on.
  await expect(page).toHaveURL(`${documents}/lauch-notes`);
  expect((await issueDocument(issue.key, "lauch-notes")).name).toBe("Launch notes");

  // The issue's document list, reached without loading the page again, reads the new title too.
  await navigateInApp(page, documents);
  await expect(page.getByRole("link", { exact: true, name: "Launch notes" })).toBeVisible();
  await expect(page.getByRole("link", { exact: true, name: "Lauch notes" })).toHaveCount(0);

  // A second document, made by mistake and removed from its row in that list.
  await createDocument(page, "Draft notes");
  await expect(page).toHaveURL(`${documents}/draft-notes`);
  await expect(documentEditor(page)).toBeAttached();
  await navigateInApp(page, documents);
  const draftRow = page.getByTestId("artifact-draft-notes");
  await draftRow.getByRole("button", { name: "Remove" }).click();
  await expect(draftRow).toHaveCount(0);

  // A third, removed from its own page: the person stays on the address the issue no longer
  // holds, where the panel says the document is gone rather than failing to load.
  await createDocument(page, "Spare notes");
  await expect(page).toHaveURL(`${documents}/spare-notes`);
  await expect(documentEditor(page)).toBeAttached();
  await header.getByRole("button", { name: "Remove" }).click();
  await expect(page.getByText(`No artifact spare-notes on ${issue.key}.`)).toBeVisible();
  await expect(page).toHaveURL(`${documents}/spare-notes`);
  expect((await listIssueArtifacts(issue.key)).map((item) => item.slug)).not.toContain(
    "spare-notes"
  );

  // A fourth the person wrote in: the removal is refused in words that say so, and the document
  // is still there after it.
  await navigateInApp(page, documents);
  await createDocument(page, "Kept notes");
  await expect(page).toHaveURL(`${documents}/kept-notes`);
  await expect(documentEditor(page)).toBeAttached();
  // An empty document holds no text a caret can enter, so the first line is typed where the
  // click puts the caret rather than after text (`typeAtEnd` throws on it).
  await documentEditor(page).click();
  await page.keyboard.type("A line nobody is to lose.");
  const kept = await issueDocument(issue.key, "kept-notes");
  // The writing is the server's to see, so the row waits for the settlement that versions it.
  await expect
    .poll(async () => (await getArtifact(kept.id)).versions?.length ?? 0)
    .toBeGreaterThan(1);
  await header.getByRole("button", { name: "Remove" }).click();
  await expect(header.getByRole("alert")).toHaveText(
    "artifact kept-notes (Kept notes) is not untouched: it holds writing; a document is removed " +
      "only while it holds nothing"
  );
  await expect(header.getByRole("heading", { name: "Kept notes" })).toBeVisible();
  expect((await listIssueArtifacts(issue.key)).map((item) => item.slug)).toContain("kept-notes");

  // What the issue records of all of it: one line for the correction, naming both titles, and
  // one for each removal, naming the document.
  await page.getByRole("tab", { name: "Activity" }).click();
  const lines = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
  await expect(lines.filter({ hasText: "renamed Lauch notes to Launch notes" })).toHaveCount(1);
  await expect(lines.filter({ hasText: "removed Draft notes" })).toHaveCount(1);
  await expect(lines.filter({ hasText: "removed Spare notes" })).toHaveCount(1);
});
