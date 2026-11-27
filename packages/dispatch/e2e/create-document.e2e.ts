import { expect, type Locator, type Page, test } from "@playwright/test";

import {
  createIssue,
  createProject,
  getArtifact,
  getArtifactVersion,
  getProjectArtifact,
  listIssueArtifacts,
} from "./api";
import { documentEditor } from "./editor";
import { watchWrites } from "./keyed-writes";
import { resetDatabase } from "./seed";
import { navigateInApp } from "./sends";
import { signIn } from "./users";

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
});

/** The control's open form: the Title the person types and the Create that writes it. */
function newDocumentForm(page: Page): Locator {
  return page.getByRole("form", { name: "New document" });
}

/** The whole press a person makes: `New document`, the title, `Create`. */
async function createDocument(page: Page, title: string): Promise<void> {
  await page.getByRole("button", { name: "New document" }).click();
  const form = newDocumentForm(page);
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

// SLOW(>=2s): the document made in the browser, its editor mounted and typed in, the settlement
// that versions what was typed, and the reload that reads the line back through a second editor.
test("a title typed on an issue's Documents tab makes an empty document and opens it to write in", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Launch\n\nThe spec itself.\n",
    title: "A document from a title",
  });

  await page.goto(`/issues/${issue.key}/artifacts`);
  // The create control is a second way to add a document, beside the upload rather than in
  // place of it: the upload's button is still on the strip and its file input took no file, so
  // "no file was chosen" below would be said of a control this page holds.
  await expect(page.getByRole("button", { name: "New document" })).toBeVisible();
  await expect(page.getByRole("button", { exact: true, name: "Upload" })).toBeVisible();
  expect(
    await page
      .getByLabel("Upload artifact")
      .evaluate((input: HTMLInputElement) => input.files?.length ?? -1)
  ).toBe(0);

  await createDocument(page, "Launch notes");
  // The person is taken to the document, which on an issue is the artifacts panel of the page
  // they are already on.
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/launch-notes`);
  await expect(page.getByRole("heading", { name: "Launch notes" })).toBeVisible();
  await expect(documentEditor(page)).toBeAttached();
  await expect(documentEditor(page)).toHaveText("");

  // An empty document holds no text a caret can enter, so the first line is typed where the
  // click puts the caret rather than after text (`typeAtEnd` throws on it).
  await documentEditor(page).click();
  await page.keyboard.type("The first line.");
  const document = await issueDocument(issue.key, "launch-notes");
  await expect
    .poll(async () => (await getArtifact(document.id)).versions?.length ?? 0)
    .toBeGreaterThan(1);

  await page.reload();
  await expect(documentEditor(page)).toBeAttached();
  await expect(documentEditor(page)).toContainText("The first line.");

  // What the two versions hold: the document was made holding nothing, and what the person
  // typed is the whole of what was written into it - no `# Launch notes` heading before it.
  const versions = (await getArtifact(document.id)).versions ?? [];
  expect(versions[0]?.number).toBe(1);
  expect((await getArtifactVersion(document.id, 1)).markdown).toBe("");
  const written = versions[versions.length - 1]?.number;
  expect(written).toBeGreaterThan(1);
  expect((await getArtifactVersion(document.id, written ?? 0)).markdown.trim()).toBe(
    "The first line."
  );
});

// SLOW(>=2s): the document the two presses make, its editor mounted on the live document, and
// the list read again in the page after it.
test("two presses of Create in one go make one document", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "One document from two presses" });
  const path = `/api/v1/issues/${issue.key}/artifacts`;
  const posts = watchWrites(page, "POST", path);

  await page.goto(`/issues/${issue.key}/artifacts`);
  await page.getByRole("button", { name: "New document" }).click();
  const form = newDocumentForm(page);
  await form.getByRole("textbox", { name: "Title" }).fill("Launch notes");
  // Two synthetic clicks in the same task of the page, so the second press is made before the
  // first write can have been answered; Playwright's own click() serializes its actionability
  // checks and cannot (`e2e/writes.e2e.ts`).
  await form.getByRole("button", { name: "Create" }).evaluate((button: HTMLButtonElement) => {
    button.click();
    button.click();
  });

  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/launch-notes`);
  // Whatever reached Dispatch went under one key, which is what makes the second press make
  // nothing: one document, and one row for its title.
  expect(posts.length).toBeGreaterThan(0);
  expect(posts.every((post) => post.key === posts[0]?.key)).toBe(true);
  expect(posts[0]?.key).toEqual(expect.any(String));
  expect(
    (await listIssueArtifacts(issue.key)).filter((item) => item.name === "Launch notes")
  ).toHaveLength(1);

  await navigateInApp(page, `/issues/${issue.key}/artifacts`);
  await expect(page.getByRole("link", { exact: true, name: "Launch notes" })).toHaveCount(1);
});

// SLOW(>=2s): the project's document made in the browser and its editor mounted on the live
// document, which is the whole of what this row reads.
test("the project document list makes a document the same way, holding nothing", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });

  await page.goto("/projects/CORE/documents");
  await createDocument(page, "Launch notes");
  await expect(page).toHaveURL("/projects/CORE/documents/launch-notes");
  await expect(page.getByRole("heading", { name: "Launch notes" })).toBeVisible();
  await expect(documentEditor(page)).toBeAttached();
  await expect(documentEditor(page)).toHaveText("");

  // The list sends the title alone now: the document's first version is empty rather than the
  // `# Launch notes` heading the list used to write into it.
  const document = await getProjectArtifact("CORE", "launch-notes");
  expect(document.name).toBe("Launch notes");
  expect(document.versions ?? []).toHaveLength(1);
  expect((await getArtifactVersion(document.id, 1)).markdown).toBe("");
});

// The tester's red row of round 21 (a document made from the project's Documents list opens on its
// page with the focus on the page body: features/artifacts/CreateDocument.tsx:94 closes the form
// (`setCreating(false)`) and :100 navigates to the new document in the same answer, so the list
// leaves the page, the form's own hand-off to New document (TitleForm.tsx:71-74) with it; and
// app.tsx:841-853 moves the focus to the page's main region only when `pageIdentity` changes, which
// is the project (`parseProjectPath(...)?.project`) for the list and for its document alike, so
// this navigation is no page change and nothing gives the focus a place; the architect's ruling of
// 2026-11-26T05:17Z). New document, a title and Create make the document and open its page; the
// focus must then be in that page's content, its main region, which holds the title heading and
// the editor. At c3b536cc it is on the body in every sample: Chromium twice and Firefox once, at
// 1440x900 and at 390x664 (Enter in the Title box leaves it there too).
// SLOW(>=2s): one project seeded, the Documents list open, one document written through its form,
// and its page opened.
// @firefox: the engine's focus after the element that held it is removed (playwright.config.ts names the clause).
test("a document made from the project's Documents list opens on its page with the focus in the page's content, not on the page body @firefox", async ({
  page,
}) => {
  const step = (text: string) => `New document on a project's Documents list: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  await page.setViewportSize({ height: 900, width: 1440 });
  await page.goto("/projects/CORE/documents");
  await expect(
    page.getByRole("region", { exact: true, name: "Project documents" }),
    step("the Documents list is drawn.")
  ).toBeVisible();
  const form = newDocumentForm(page);
  const titleBox = form.getByRole("textbox", { name: "Title" });
  await page.getByRole("button", { name: "New document" }).click();
  await expect(
    titleBox,
    step("New document opens its form with the caret in the Title box.")
  ).toBeFocused();
  await titleBox.fill("Launch notes");
  await form.getByRole("button", { name: "Create" }).click();
  await expect(page, step("Create opens the new document's page.")).toHaveURL(
    "/projects/CORE/documents/launch-notes"
  );
  await expect(
    page.getByRole("heading", { name: "Launch notes" }),
    step("the document's page draws its title.")
  ).toBeVisible();
  await expect(form, step("the New document form is gone with the list.")).toHaveCount(0);
  // The main region itself counts: the shell focuses it on a page change (app.tsx:852), so the
  // region or anything inside it holding the focus is the page's content holding it.
  await expect(
    page.getByTestId("main-content").and(page.locator(":focus-within")),
    step(
      "the focus is on the document page's content - its main region, which holds the title heading and the editor - not on the page body."
    )
  ).toHaveCount(1);
});
