import { expect, type Page, type Route, test } from "@playwright/test";

import {
  createIssue,
  createIssueArtifact,
  createProject,
  getArtifact,
  listIssueArtifacts,
} from "./api";
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

/** A document of an issue opened on its own page from the issue's Artifacts tab, with the reads
 *  `holds` names held from the row's `hold` on until its `answer`, the clock installed and the live
 *  feed dropped (a removal's event reads the list beside the read the press holds through). */
async function openDocumentWithHeldReads(
  page: Page,
  holds: (key: string) => (url: URL) => boolean
): Promise<{
  readonly answer: () => Promise<void>;
  readonly document: { readonly id: string; readonly slug: string };
  readonly hold: () => void;
  readonly key: string;
  readonly out: () => number;
}> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "The spec itself.",
    title: "The two ways back",
  });
  const made = (await createIssueArtifact(issue.key, { name: "Draft notes" })).artifact;
  let holding = false;
  let kept = 0;
  const waiting: Route[] = [];
  await page.route(holds(issue.key), (route) => {
    if (!holding || route.request().method() !== "GET") return route.fallback();
    kept += 1;
    waiting.push(route);
    return undefined;
  });
  await page.clock.install();
  await page.routeWebSocket(/\/ws\/live$/u, (route) =>
    route.close({ code: 1011, reason: "test network drop" })
  );
  await page.goto(`/issues/${issue.key}/artifacts`);
  await page
    .getByTestId(`artifact-${made.slug}`)
    .getByRole("link", { exact: true, name: made.name })
    .click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${made.slug}`);
  return {
    // A read the page gave up has no answer left to take, and its route refuses the fallback.
    answer: async () => {
      holding = false;
      for (const route of waiting.splice(0)) await route.fallback().catch(() => undefined);
    },
    document: { id: made.id, slug: made.slug },
    hold: () => {
      holding = true;
    },
    key: issue.key,
    out: () => kept,
  };
}

/** What a document's removal says once the read after it has outlived its 30 s bound. */
const ARTIFACTS_UNREAD = "Could not load artifacts. Dispatch did not answer within 30 seconds.";

// The tester's red row of round 22 (B3: a document page's Remove holds through a read nothing on
// that page draws, so the page never says why; the architect's ruling of 2026-11-26T23:11Z). The
// header's Remove holds through the issue's document list (DocumentWayBack.tsx:270-273, reading
// `["artifacts", key]` at ArtifactUpload.tsx:235), which only the Artifacts tab draws
// (ArtifactRoutePanel.tsx:46-48): on the document's own page that read fetches nothing, so the
// hold ends at once (ArtifactHeader.tsx:126). At e4ece294, with the issue read the page draws the
// document from held past the bound, the header says nothing. Measured in Chromium and Firefox,
// twice each.
// SLOW(>=2s): one document seeded and opened on its own page, one removal written, and the page's
// clock moved once past the 30 s bound.
// @firefox: the timers of a read's own deadline and of the bound a held control waits on, which
// each engine runs.
test("a Remove on a document's own page whose reads the bound ended says why beside the header's Remove @firefox", async ({
  page,
}) => {
  const step = (text: string) => `Remove on a document's own page: ${text}`;
  const opened = await openDocumentWithHeldReads(
    page,
    (key) => (url) =>
      url.pathname === `/api/v1/issues/${key}/artifacts` || url.pathname === `/api/v1/issues/${key}`
  );
  const header = page.getByTestId("artifact-header");
  const remove = header.getByRole("button", { exact: true, name: "Remove" });
  await expect(
    remove,
    step("the document's header offers Remove once its editor is up.")
  ).toBeVisible({ timeout: 20_000 });
  const unreadLine = header
    .getByRole("alert")
    .filter({ hasText: /did not answer within 30 seconds/ });

  opened.hold();
  const removal = page.waitForResponse(
    (response) =>
      response.request().method() === "DELETE" &&
      new URL(response.url()).pathname === `/api/v1/artifacts/${opened.document.id}`
  );
  await remove.click();
  expect((await removal).status(), step("the removal lands at Dispatch.")).toBe(204);
  await expect
    .poll(opened.out, { message: step("a read the removal starts is out.") })
    .toBeGreaterThan(0);
  await page.clock.fastForward(30_000);
  await expect(
    unreadLine,
    step("at the 30 s bound the header says why the document's reads are not answered.")
  ).toHaveText([ARTIFACTS_UNREAD]);

  await opened.answer();
  await page.clock.runFor(1_000);
  await expect(unreadLine, step("the reads answered take the line away.")).toHaveCount(0);
});

// The tester's red row of round 22 (B3, the way back; the architect's ruling of
// 2026-11-26T23:11Z). The removal's way back keeps the reader on the document's address, which
// says the document is gone, and its "View artifacts" opens the Artifacts tab, whose read of the
// list the row holds. The removal's hold ended at once (DocumentWayBack.tsx:270-273,
// ArtifactUpload.tsx:235) and the tab's own hold (ArtifactsTab.tsx:467) never saw the removal, so
// at e4ece294 the tab lists the removed document past the 30 s bound and says nothing. Measured
// in Chromium and Firefox, twice each.
// SLOW(>=2s): one document seeded and opened on its own page, one removal written, the Artifacts
// tab opened again, and the page's clock moved once past the 30 s bound.
// @firefox: the timers of a read's own deadline and of the bound a held control waits on, which
// each engine runs.
test("a Remove on a document's own page says why on the Artifacts tab its way back opens while the list read is out @firefox", async ({
  page,
}) => {
  const step = (text: string) => `Remove on a document's own page, then its way back: ${text}`;
  const opened = await openDocumentWithHeldReads(
    page,
    (key) => (url) => url.pathname === `/api/v1/issues/${key}/artifacts`
  );
  const header = page.getByTestId("artifact-header");
  const remove = header.getByRole("button", { exact: true, name: "Remove" });
  await expect(
    remove,
    step("the document's header offers Remove once its editor is up.")
  ).toBeVisible({ timeout: 20_000 });

  opened.hold();
  const removal = page.waitForResponse(
    (response) =>
      response.request().method() === "DELETE" &&
      new URL(response.url()).pathname === `/api/v1/artifacts/${opened.document.id}`
  );
  await remove.click();
  expect((await removal).status(), step("the removal lands at Dispatch.")).toBe(204);
  await expect(
    page.getByText(`No artifact ${opened.document.slug} on ${opened.key}.`),
    step("the way back: the document's own address says the document is gone.")
  ).toBeVisible();
  await page.getByRole("link", { exact: true, name: "View artifacts" }).click();
  await expect(page, step("View artifacts opens the Artifacts tab.")).toHaveURL(
    `/issues/${opened.key}/artifacts`
  );
  const row = page.getByTestId(`artifact-${opened.document.slug}`);
  await expect(row, step("the tab lists the removed document, its list read out.")).toBeVisible();
  await expect
    .poll(opened.out, { message: step("the Artifacts tab's read of the list is out.") })
    .toBeGreaterThan(0);
  await page.clock.fastForward(30_000);
  await expect(
    page.getByRole("alert").filter({ hasText: /did not answer within 30 seconds/ }),
    step(
      "at the 30 s bound, with the list read out and no later removal, the page says why the list is not read."
    )
  ).toHaveText([ARTIFACTS_UNREAD]);

  await opened.answer();
  await page.clock.runFor(1_000);
  await expect(row, step("the list read answered takes the removed document away.")).toHaveCount(0);
});
