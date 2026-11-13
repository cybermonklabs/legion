import { writeFile } from "node:fs/promises";
import { expect, type Locator, type Page, test } from "@playwright/test";

import { CONTROL_LABELS } from "../web/src/features/issue/page-words";
import {
  createIssue,
  createIssueArtifact,
  createProject,
  createProjectDocument,
  dashboardOrigin,
  getProjectArtifact,
  listIssueArtifacts,
  patchIssue,
} from "./api";
import { resetDatabase } from "./seed";
import { signIn } from "./users";

test.beforeEach(async ({ context }) => {
  await resetDatabase();
  await signIn(context, "alice");
});

/** The control's open form: the Title the person types and the Create that writes it. */
function newDocumentForm(page: Page): Locator {
  return page.getByRole("form", { name: "New document" });
}

// `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
// cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
function postAsPage(page: Page, path: string, data: object) {
  return page.request.post(path, { data, headers: { Origin: dashboardOrigin } });
}

// SLOW(>=2s): the refused title read twice in the page and the issue's documents read again
// after each refusal and after the write's own.
test("a title the issue already holds is refused by the control, by its name and by its slug", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Launch\n\nThe spec itself.\n",
    title: "A title already held",
  });
  await createIssueArtifact(issue.key, {
    content: "# Launch notes\n\nWritten already.\n",
    name: "Launch notes",
  });
  const held = (await listIssueArtifacts(issue.key)).length;

  await page.goto(`/issues/${issue.key}/artifacts`);
  await page.getByRole("menuitem", { name: "New document" }).click();
  const form = newDocumentForm(page);
  // The write compares the title with each document by name and by slug, and the control says
  // so before it writes: both spellings name the one document that holds the title.
  for (const typed of ["Launch notes", "launch-notes"]) {
    await form.getByRole("textbox", { name: "Title" }).fill(typed);
    await form.getByRole("button", { name: "Create" }).click();
    await expect(form.getByRole("alert")).toHaveText(
      "A document on this issue already holds that title: Launch notes."
    );
    expect(await listIssueArtifacts(issue.key)).toHaveLength(held);
  }

  // The same title through the API, with no content: the write's own refusal names the document
  // that holds the title in the same way, and asks for no content at all.
  const refused = await postAsPage(page, `/api/v1/issues/${issue.key}/artifacts`, {
    name: "Launch notes",
  });
  expect(refused.status()).toBe(400);
  const body = await refused.json();
  expect(body.code).toBe("ARTIFACT_INPUT");
  expect(body.error).toBe(
    '"Launch notes" names artifact launch-notes (Launch notes), which exists; a write with no ' +
      "content makes no document and writes no version: edit it with dispatch doc-edit, or give " +
      "the new document another name"
  );
  expect(body.error).not.toContain("content is required");
  expect(await listIssueArtifacts(issue.key)).toHaveLength(held);
});

// SLOW(>=2s): the refused create, a real upload of plan.md with the version it builds, and the
// API create that writes a second version of it.
test("a title whose slug is a role slug is refused, and the real plan.md is still uploaded", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Launch\n\nThe spec itself.\n",
    title: "A title Dispatch keeps for itself",
  });

  await page.goto(`/issues/${issue.key}/artifacts`);
  await page.getByRole("menuitem", { name: "New document" }).click();
  const form = newDocumentForm(page);
  await form.getByRole("textbox", { name: "Title" }).fill("Plan.md");
  await form.getByRole("button", { name: "Create" }).click();
  // The slug the title gives is read, not the title: `Plan.md` gives `plan-md`, which is the
  // slug of the plan document Dispatch writes itself.
  await expect(form.getByRole("alert")).toHaveText(
    '"Plan.md" gives the slug plan-md, the slug of the issue\'s plan document, which Dispatch ' +
      "makes itself; give this document another name"
  );
  expect((await listIssueArtifacts(issue.key)).map((item) => item.slug)).not.toContain("plan-md");
  await form.getByRole("button", { name: "Cancel" }).click();

  // What that slug is kept for: a real plan.md, uploaded from the same node menu.
  const planFile = testInfo.outputPath("plan.md");
  await writeFile(planFile, "# Plan\n\nThe steps, in order.\n");
  // The node menu's upload stages the file, and its Upload sends it; the menu lists it.
  await page.getByLabel(CONTROL_LABELS.upload_file).setInputFiles(planFile);
  await page.getByRole("button", { exact: true, name: "Upload" }).click();
  await page.getByRole("button", { exact: true, name: CONTROL_LABELS.other_nodes }).click();
  await expect(
    page.getByRole("menu").getByRole("button", { exact: true, name: "plan.md" })
  ).toBeVisible();

  const plan = (await listIssueArtifacts(issue.key)).find((item) => item.role === "plan");
  expect(plan?.slug).toBe("plan-md");
  expect(plan?.name).toBe("plan.md");

  // The refusal is of a create that carries nothing, never of the name: the same name with
  // content is taken, and writes a version of the issue's plan document.
  const accepted = await postAsPage(page, `/api/v1/issues/${issue.key}/artifacts`, {
    content: "# Plan\n\nThe steps, in order, corrected.\n",
    name: "plan.md",
  });
  expect(accepted.status()).toBe(201);
  const made = await accepted.json();
  expect(made.artifact.slug).toBe("plan-md");
  expect(made.artifact.id).toBe(plan?.id);
});

// SLOW(>=2s): the issue closed under the open page, which reaches it on the live feed, and the
// strip read again after it.
test("a closed issue refuses both ways to add a document, in one sentence", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Launch\n\nThe spec itself.\n",
    title: "A closed issue takes no document",
  });
  const held = (await listIssueArtifacts(issue.key)).length;

  await page.goto(`/issues/${issue.key}/artifacts`);
  const create = page.getByRole("menuitem", { name: "New document" });
  const upload = page.getByRole("menuitem", { exact: true, name: "Upload" });
  await expect(create).toBeVisible();
  await expect(upload).toBeVisible();

  await patchIssue(issue.key, { status: "done" });
  const refusal = "This issue is closed. Reopen it to add a document.";
  // Each refused item of the menu says that one sentence as its description, drawn inside it -
  // a menu owns menu items alone, so the line cannot stand beside them as it stood beside the
  // Artifacts list's strip - rather than as a `title`, since the issue page raises no tooltip at
  // all (dispatch://INFRA-1196 R21, `components/tooltips.tsx`).
  await expect(create).toHaveAttribute("aria-disabled", "true");
  await expect(create).toHaveAccessibleDescription(refusal);
  await expect(create).not.toHaveAttribute("title", /./);
  await expect(upload).toHaveAttribute("aria-disabled", "true");
  await expect(upload).toHaveAccessibleDescription(refusal);
  await expect(upload).not.toHaveAttribute("title", /./);
  await expect(page.getByText(refusal, { exact: true })).toHaveCount(2);

  // `force`: the refusal is `aria-disabled`, which Playwright reads as not enabled, and the
  // press a person can still make is exactly what this reads - it opens no form.
  await create.click({ force: true });
  await expect(newDocumentForm(page)).toHaveCount(0);
  expect(await listIssueArtifacts(issue.key)).toHaveLength(held);
});

// SLOW(>=2s): the project's document list opened in the browser, a refusal read in it and the
// document the second title makes, with its editor mounted on the live document.
test("a project refuses a title it already holds and takes one whose slug is a role slug", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createProjectDocument("CORE", {
    content: "# Launch notes\n\nWritten already.\n",
    name: "Launch notes",
  });

  await page.goto("/projects/CORE/documents");
  await page.getByRole("button", { name: "New document" }).click();
  const form = newDocumentForm(page);
  await form.getByRole("textbox", { name: "Title" }).fill("Launch notes");
  await form.getByRole("button", { name: "Create" }).click();
  await expect(form.getByRole("alert")).toHaveText(
    "A document on this project already holds that title: Launch notes."
  );

  // A project holds no document Dispatch makes itself, so the title an issue refuses is a
  // document of its own here.
  await form.getByRole("textbox", { name: "Title" }).fill("Plan.md");
  await form.getByRole("button", { name: "Create" }).click();
  await expect(page).toHaveURL("/projects/CORE/documents/plan-md");
  expect((await getProjectArtifact("CORE", "plan-md")).name).toBe("Plan.md");
});
