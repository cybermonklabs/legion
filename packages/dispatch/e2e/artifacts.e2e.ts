import { writeFile } from "node:fs/promises";
import { setTimeout as sleep } from "node:timers/promises";

import { RECONNECT_SPREAD_MS } from "../web/src/api/live-socket";
import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";
import { CONTROL_LABELS } from "../web/src/features/issue/page-words";

import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  dashboardOrigin,
  editArtifact,
  getArtifactText,
  listIssueArtifacts,
} from "./api";
import {
  countDocumentSockets,
  documentEditor,
  documentTransport,
  specAddress,
  typeAtEnd,
} from "./editor";
import { openIssueAddress, presetChooser, presetTablist, presetTabs } from "./issue-page";
import { navigateInApp } from "./sends";
import { expect, type Locator, type Page, type TestInfo, test } from "./test";

const tinyPng =
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVQIHWP4z8DwHwAFgAI/ScL0eAAAAABJRU5ErkJggg==";

// The files a row uploads are written in that row's own output directory, which Playwright gives
// each row of each worker (`testInfo.outputPath`): two workers running these rows at once would
// otherwise write one path in the checkout, and a run must leave no file changed under e2e/.
async function diagramFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("diagram.png");
  await writeFile(path, Buffer.from(tinyPng, "base64"));
  return path;
}

async function notesFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("notes.md");
  await writeFile(path, "# Review notes\n\nThese notes replace the initial spec.\n");
  return path;
}

async function openArtifacts(page: Page, isPhone: boolean) {
  if (isPhone) {
    const reviewPanel = page.getByRole("button", { name: /review panel/i });
    if ((await reviewPanel.getAttribute("aria-expanded")) === "true") {
      await reviewPanel.click();
    }
  }
  await openIssueAddress(page, "artifacts");
}

/** The node menu's item for the file `name`. The menu took the Artifacts tab's list
 *  (dispatch://INFRA-1196 R3, the Design's "Where each thing goes"), and its item carries what
 *  the list's row offered beside the file: the control that opens it, its copy control, its
 *  latest version's download and, for a document the issue can drop, Remove. The file's versions,
 *  its picture and what references it are on its own page, which the item opens. */
function menuItem(page: Page, name: string): Locator {
  return page
    .getByRole("menu")
    .getByRole("menuitem")
    .filter({ has: page.getByRole("button", { exact: true, name }) });
}

// Choosing a file only stages it; the row then shows a Summary field plus a confirming
// "Upload" button (or "Cancel" to discard the pick) before it actually creates the version.
async function confirmUpload(page: Page): Promise<void> {
  await page.getByRole("button", { exact: true, name: "Upload" }).click();
}

// `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
// cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
function postAsPage(page: Page, path: string, data?: object) {
  return page.request.post(path, { data, headers: { Origin: dashboardOrigin } });
}

// SLOW(>=2s): three real uploads with the versions they build, the artifact page's version list
// and comparison, and the references and phone layout read after them.
// @pr: Upload and download: three real uploads, the versions they build, the Download link on a version, and the references that cite them.
test("artifacts upload, version, references, and phone layout @phone @pr", async ({
  page,
}, testInfo) => {
  const sockets = countDocumentSockets(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Initial spec\n\nThe original document.",
    title: "Artifact review",
  });
  await createAsk(issue.key, { question: "Can this ship?" });

  await page.goto(await specAddress(issue.key));
  await expect(documentEditor(page)).toContainText("The original document.");
  await expect.poll(sockets).toBe(1);
  await openArtifacts(page, testInfo.project.name === "iphone");

  // The menu's upload stages the picked file, with its Summary, Upload and Cancel.
  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagramUpload = await diagramFile(testInfo);
  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  const diagram = menuItem(page, "diagram.png");
  await expect(diagram).toBeVisible();
  await expect(diagram.getByRole("link", { name: "Download version 1" })).toBeVisible();

  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  await expect(diagram.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  // The menu item is compact: the versions, their count, the picture, the compare and the
  // references live on the artifact's own page, not under every item.
  await expect(diagram.getByRole("button", { name: "Show all versions" })).toHaveCount(0);
  await expect(page.getByLabel(CONTROL_LABELS.filter_files)).toBeVisible();

  await navigateInApp(page, `/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page.getByTestId("artifact-header")).toContainText("diagram.png");
  await expect(page.getByRole("img", { name: "diagram.png version 2" })).toBeVisible();
  expect(sockets()).toBe(1);
  await page.getByRole("button", { name: "Show all versions" }).click();
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  await expect(versions).toContainText("Version 1");
  await expect(versions).toContainText("Version 2");
  await expect(versions).not.toContainText("Version 3");
  await expect(versions).toContainText("SHA-256");
  await expect(versions.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  const compare = page.getByRole("region", { name: "Compare versions for diagram.png" });
  await expect(compare).toBeVisible();
  await expect(compare).not.toContainText("From · Version");
  await compare.getByRole("button", { name: "Show comparison" }).click();
  await expect(compare).toContainText("From · Version");
  await expect(compare).toContainText("To · Version");

  await navigateInApp(page, `/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");

  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  const notes = menuItem(page, "notes.md");
  await expect(notes).toBeVisible();
  await expect(notes.getByRole("button", { name: /make primary/i })).toHaveCount(0);
  await navigateInApp(page, await specAddress(issue.key));
  await expect(documentEditor(page)).toContainText("The original document.");

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`,
  });

  await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/diagram-png before deciding.`,
  });
  // No reload: both citations reach the open page on its event feed, and the panel below reads
  // what the feed left.
  await openArtifacts(page, testInfo.project.name === "iphone");

  await diagram.getByRole("button", { exact: true, name: "diagram.png" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  // The panel names the record that cites this artifact and quotes it; its link goes to that
  // source, not back to the artifact the reader is already on.
  const diagramReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(diagramReferencedBy).toContainText(`Comment · ${issue.key}`);
  await expect(diagramReferencedBy).toContainText(
    `See dispatch://${issue.key}/artifact/diagram-png before deciding.`
  );
  await diagramReferencedBy.getByRole("link", { name: `Comment · ${issue.key}` }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/comments/`));

  // One cold load, so the notes.md panel below is read by a page that cached nothing.
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await notes.getByRole("button", { exact: true, name: "notes.md" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  const notesReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(notesReferencedBy).toContainText(`Comment · ${issue.key}`);
  await expect(notesReferencedBy).toContainText(
    `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`
  );
  await navigateInApp(page, `/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await page.screenshot({ path: testInfo.outputPath("artifacts-tab.png"), fullPage: true });

  if (testInfo.project.name === "iphone") {
    await page.goto("/");
    await expect(page.getByTestId(/ask-/).first()).toContainText("Can this ship?");
    await page.screenshot({ path: testInfo.outputPath("iphone-inbox.png"), fullPage: true });

    const openNavigation = page.getByRole("button", { name: "Open navigation" });
    await expect(openNavigation).toBeVisible();
    await openNavigation.click();
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
    await page.getByRole("button", { name: "Close navigation" }).click();
    await navigateInApp(page, `/issues/${issue.key}`);
    const openMargin = page.getByRole("button", { name: /review panel/i });
    if ((await openMargin.getAttribute("aria-expanded")) === "true") {
      await openMargin.click();
    }
    const [navigationBox, marginBox] = await Promise.all([
      openNavigation.boundingBox(),
      openMargin.boundingBox(),
    ]);
    expect(navigationBox?.width).toBeGreaterThanOrEqual(44);
    expect(navigationBox?.height).toBeGreaterThanOrEqual(44);
    expect(marginBox?.width).toBeGreaterThanOrEqual(44);
    expect(marginBox?.height).toBeGreaterThanOrEqual(44);
    await openMargin.click();
    await expect(page.getByTestId("margin-sheet")).toHaveAttribute("data-expanded", "true");
    await page.screenshot({ path: testInfo.outputPath("iphone-margin-sheet.png"), fullPage: true });
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
  }
});

// The From/To compare of a document shows what changed as the change view, word by word. Every
// artifact's own page draws `ArtifactDetails` - the version history and this compare - the spec's
// as much as any other (`ArtifactRoutePanel`); the row compares a second document so the spec's
// own text stays out of the comparison.
test("the version history compares two versions of a document with the change view", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Lease handover\n\nThe spec itself is the primary document.\n",
    title: "Version compare",
  });
  const notes = await createIssueArtifact(issue.key, {
    content: "# Review notes\n\nThe ledger keeps three leases.\n",
    name: "notes.md",
  });
  await editArtifact(
    notes.artifact.id,
    {
      ops: [
        {
          find: "The ledger keeps three leases.",
          op: "replace",
          with: "The ledger keeps seven leases.",
        },
      ],
      summary: "Seven leases",
    },
    { as: "agent" }
  );

  await page.goto(`/issues/${issue.key}/artifacts/notes-md`);
  const compare = page.getByRole("region", { name: "Compare versions for notes.md" });
  await compare.getByRole("button", { name: "Show comparison" }).click();
  await compare.getByLabel("Compare notes.md from").selectOption("1");
  await compare.getByLabel("Compare notes.md to").selectOption("2");
  const changed = compare.getByTestId("change-view").locator('[data-change="changed"]');
  await expect(changed).toHaveCount(1);
  await expect(changed.locator("del")).toHaveText(["three"]);
  await expect(changed.locator("ins")).toHaveText(["seven"]);
});

test("the node menu's filter narrows its files by name and kind", async ({ page }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Artifact filtering" });
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, false);

  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagram = menuItem(page, "diagram.png");
  const notes = menuItem(page, "notes.md");
  await upload.setInputFiles(await diagramFile(testInfo));
  await confirmUpload(page);
  await expect(diagram).toBeVisible();
  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  await expect(notes).toBeVisible();

  const filter = page.getByLabel(CONTROL_LABELS.filter_files);
  await filter.fill("notes");
  await expect(notes).toBeVisible();
  await expect(diagram).toHaveCount(0);

  await filter.fill("image");
  await expect(diagram).toBeVisible();
  await expect(notes).toHaveCount(0);

  await filter.fill("");
  await expect(diagram).toBeVisible();
  await expect(notes).toBeVisible();
});

// The node menu's upload stages a picked file as the Artifacts list's did (dispatch://INFRA-1196
// R19, "nothing lost"): the file's name, an optional Summary for the version it makes, and Upload
// and Cancel. Cancel sends nothing; Upload sends the file with its summary.
test("the node menu's upload stages a file with a Summary for its version, and Cancel sends nothing", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Staged upload" });
  const held = (await listIssueArtifacts(issue.key)).length;
  await page.goto(`/issues/${issue.key}/artifacts`);

  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const summary = page.getByRole("textbox", { name: "Summary (optional)" });
  await upload.setInputFiles(await notesFile(testInfo));
  await expect(page.getByText("notes.md", { exact: true })).toBeVisible();
  await expect(summary).toBeVisible();
  await page.getByRole("button", { exact: true, name: "Cancel" }).click();
  await expect(summary).toHaveCount(0);
  expect(await listIssueArtifacts(issue.key)).toHaveLength(held);

  await upload.setInputFiles(await notesFile(testInfo));
  await summary.fill("First review notes");
  await confirmUpload(page);
  await expect(menuItem(page, "notes.md")).toBeVisible();
  const notes = (await listIssueArtifacts(issue.key)).find((one) => one.name === "notes.md");
  expect(notes?.versions?.map((version) => version.summary)).toEqual(["First review notes"]);
});

// SLOW(>=2s): a real upload the Activity list links, and the keyboard tour of the five presets
// with each tab's box measured, at 1440 px and at 390 px.
//
// The nine tabs are gone (dispatch://INFRA-1196 R3): the row is five presets, and how many
// artifacts an issue carries is the node menu's, not a tab's badge, so the `Artifacts (N)`
// assertions this test carried are retired with it - what they proved, that the live feed
// refreshes the page's own counts without a reload, is proved here on the Activity list.
//
// It is not tagged `@phone`: the row sets 390 px itself, so the iphone project would run the
// same assertions a second time (#2866, no browser row runs twice).
test("an uploaded document is linked from the Activity list and the presets fit every width", async ({
  page,
}, testInfo) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({ project: "OPS", title: "Customer update" });
  await page.goto(`/issues/${issue.key}/activity`);

  // The list is drawn before the upload, so the line below is one the live feed brought and
  // not one the first read already held: without this the upload can land before that read
  // answers, and the row would pass with the feed disconnected. This is what the retired
  // `Artifacts 1` badge reading proved - that the page's own counts move without a reload.
  const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
  await expect(rows.filter({ hasText: "created the issue" })).toHaveCount(1);

  const name = "cu-update-2026-09-15.md";
  const added = rows.filter({ hasText: `added ${name}` });
  await expect(added).toHaveCount(0);
  const upload = await createIssueArtifact(issue.key, {
    content: "# Draft\n\nHi team, here is this week's update.\n",
    name,
  });

  // The live event feed puts the upload on the open Activity list without a reload.
  await expect(added).toHaveCount(1);
  await page.screenshot({ path: testInfo.outputPath("activity-added-artifact.png") });

  // A second upload under the same name is a new version; its line links that version.
  await createIssueArtifact(issue.key, { content: "# Draft\n\nRevised.\n", name });
  const saved = rows.filter({ hasText: `saved ${name} v2` });
  await expect(saved).toHaveCount(1);
  await expect(saved.getByRole("link", { name })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/artifacts/${upload.artifact.slug}?v=2`
  );

  const viewport = page.viewportSize();
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 1440 });
  const tablist = presetTablist(page);
  const tabs = presetTabs(page);
  await expect(tabs).toHaveCount(5);
  const insideTablist = async (tab: Locator) => {
    const [listBox, box] = await Promise.all([tablist.boundingBox(), tab.boundingBox()]);
    expect(listBox).not.toBeNull();
    expect(box).not.toBeNull();
    expect(box?.x).toBeGreaterThanOrEqual(listBox?.x ?? 0);
    expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(
      (listBox?.x ?? 0) + (listBox?.width ?? 0) + 0.5
    );
  };
  // What the row promises at 1440 px, which is a pointer's width: a tab of the page's own
  // pointer height and no taller than the row it shares. The presets declare
  // `min-h-11 md:min-h-9` (`web/src/features/issue/IssuePresets.tsx`), the idiom the whole app
  // writes for a control - 44 px where a finger reaches it, 36 px from `md` where a pointer
  // does (`web/src/components/Tabs.tsx`, `CopyButton.tsx`) - so 36 px is what it is held to
  // here. The 44 px floor is a touch rule and is read below at the width a finger is what
  // reaches the presets: these five tabs are not drawn there at all, because at 1024 px and
  // below the row is one control instead (`ZOOMED_TREE_QUERY`), and that control carries the
  // floor.
  const fitsTheRow = async (tab: Locator, label: string) => {
    const height = (await tab.boundingBox())?.height ?? 0;
    expect(
      height,
      `the ${label} tab is ${height} px tall, under the 36 px a pointer control is drawn at`
    ).toBeGreaterThanOrEqual(36);
    expect(
      height,
      `the ${label} tab is ${height} px tall, taller than the row of five`
    ).toBeLessThanOrEqual(48);
  };
  // Every preset is reachable from the keyboard, each is inside the row as it takes focus, and
  // each is drawn at the height the row gives it.
  await page.getByRole("tab", { exact: true, name: "Tree" }).focus();
  for (const label of ["Decisions", "Checks", "Spec", "Plan"] as const) {
    await page.keyboard.press("ArrowRight");
    const tab = page.getByRole("tab", { exact: true, name: label });
    await expect(tab).toBeFocused();
    await insideTablist(tab);
    await fitsTheRow(tab, label);
  }
  await page.keyboard.press("Home");
  const treeTab = page.getByRole("tab", { exact: true, name: "Tree" });
  await expect(treeTab).toBeFocused();
  await insideTablist(treeTab);
  await fitsTheRow(treeTab, "Tree");

  // At 390 px the row is one control naming the preset the page is on, which opens the five.
  // This is the width a finger is what reaches the presets, so this is where the 44 px touch
  // target is read: the one control a reader presses there carries it, and the row fails if it
  // ever loses it.
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 390 });
  await expect(tabs).toHaveCount(0);
  const chooser = presetChooser(page);
  await expect(chooser).toBeVisible();
  const chooserHeight = (await chooser.boundingBox())?.height ?? 0;
  expect(
    chooserHeight,
    `the preset control is ${chooserHeight} px tall at 390 px, under the 44 px a touch needs`
  ).toBeGreaterThanOrEqual(44);
  await chooser.click();
  await expect(page.getByRole("menu").getByRole("menuitem")).toHaveCount(5);
  await page.keyboard.press("Escape");
  await page.setViewportSize({ height: viewport?.height ?? 800, width: viewport?.width ?? 1280 });

  // The upload is an Activity line and nothing else: the Conversation draws no row for it.
  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(
    page.getByRole("list", { name: "Conversation turns" }).getByText(`added ${name}`)
  ).toHaveCount(0);
  await page.goto(`/issues/${issue.key}/activity`);

  await added.getByRole("link", { name }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByTestId("artifact-header")).toContainText(name);
});

// The Artifacts tab's badge and its rows' detail lines went with the tab (dispatch://INFRA-1196
// R3): the node menu holds one item per file, whose name is never cut, and a file's version, size
// and upload time are on its own page.
test("the node menu lists every file of the issue, and an item's name is not truncated @phone", async ({
  page,
}) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({
    project: "OPS",
    spec: "# Spec\n\nThe issue's own document.\n",
    title: "Counted tab",
  });
  await createIssueArtifact(issue.key, { content: "# Audit\n\nOne more.\n", name: "audit.md" });

  await page.goto(`/issues/${issue.key}/artifacts`);
  const files = page
    .getByRole("menu")
    .getByRole("menuitem")
    .filter({ has: page.getByRole("link", { name: /^Download version/ }) });
  await expect(files).toHaveCount(2);

  // Every item shows its whole name: it wraps where it must rather than truncating.
  for (const file of await files.all()) {
    const name = file.getByRole("button").first();
    expect(await name.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
  }
  await files.getByRole("button", { exact: true, name: "audit.md" }).click();
  await page.getByRole("button", { name: "Show all versions" }).click();
  await expect(
    page.getByRole("region", { exact: true, name: "Versions for audit.md" })
  ).toContainText("Version 1");
});

// The repair replaces the unreadable document whatever the picked file is called: a second
// document beside the broken one would leave it unreadable.
test("an out-of-schema document names its repair and uploads replacement markdown", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Repair an unreadable document" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "repair.md",
  });
  const corrupted = await postAsPage(
    page,
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);

  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await page.getByLabel("Upload artifact").setInputFiles({
    buffer: Buffer.from("repaired\n"),
    mimeType: "text/markdown",
    name: "repair (1).md",
  });
  await confirmUpload(page);
  await expect(documentEditor(page)).toContainText("repaired");
  expect((await getArtifactText(upload.artifact.id)).markdown).toBe("repaired\n");
});

// The reader leaves the document and opens it again without a page load, so the page still holds
// the text it read on the first visit. The second admission reads the text afresh and shows the
// repair; connecting on the cached text would open a socket into a tree outside the schema. The
// count is this document's sockets alone: the issue's own spec holds a socket of its own.
test("an out-of-schema document does not reconnect from cached text", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Do not reconnect from stale text" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "cached-repair.md",
  });
  const sockets = countDocumentSockets(page, upload.artifact.id);
  const row = menuItem(page, "cached-repair.md");
  const openDocument = () =>
    row.getByRole("button", { exact: true, name: "cached-repair.md" }).click();

  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, false);
  // A page load would drop this marker, and the page's cached reads with it.
  await page.evaluate(() => Object.assign(window, { __e2eNavMarker: true }));
  await openDocument();
  await expect(documentEditor(page)).toContainText("before");
  await expect.poll(sockets).toBe(1);

  await page.goBack();
  await expect(row).toBeVisible();
  const corrupted = await postAsPage(
    page,
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);
  await openDocument();

  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await expect(documentEditor(page)).toBeHidden();
  expect(await page.evaluate(() => "__e2eNavMarker" in window)).toBe(true);
  expect(sockets()).toBe(1);
});

// A mounted editor's provider reconnects on its own after a dropped socket. Another client makes
// the stored tree invalid while that socket is down; the server refuses the reconnect before any
// sync, so the browser never holds the tree it would normalize and write back, and the page turns
// the refusal into the repair message instead of reconnecting again.
test("a mounted editor does not reconnect into a document made unreadable while its socket was down", async ({
  page,
}) => {
  const sockets = countDocumentSockets(page);
  const transport = await documentTransport(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    title: "No reconnect into an unreadable room",
  });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "reconnect-repair.md",
  });

  // The reconnect below waits a draw from [0, RECONNECT_SPREAD_MS) set at the previous open
  // (`web/src/features/doc/connection.ts`), which the page's own clock spends.
  await page.clock.install();
  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");
  await expect.poll(sockets).toBe(1);

  // The reconnect the drop starts is held, so it reaches the server only after the tree is made
  // invalid. A held socket opens no network connection, so it is counted once it is released.
  transport.hold();
  await transport.sever();
  const corrupted = await postAsPage(
    page,
    `/api/v1/artifacts/${upload.artifact.id}/_test/outside-schema`
  );
  expect(corrupted.status()).toBe(204);
  await transport.release();
  // Exactly the spread: a longer jump would arm the refusal's next re-admission and open a third
  // socket, which is the count this row reads.
  await page.clock.fastForward(RECONNECT_SPREAD_MS);

  await expect(page.getByText("replace the document from markdown to repair it")).toBeVisible();
  await expect(documentEditor(page)).toBeHidden();
  const text = await page.request.get(`/api/v1/artifacts/${upload.artifact.id}/text`);
  expect(text.status()).toBe(409);
  await expect(text.text()).resolves.toContain(`"code":"DOC_SCHEMA"`);
  expect(sockets()).toBe(2);
});

// A refetch of the document's text that fails says nothing about the editor already connected:
// an edit typed while its socket was down lives only in the editor, so an outage that answers the
// refetch 503 leaves the editor, and the edit reaches the server once the socket is back.
test("an edit typed while the socket was down survives a text refetch the server answers 503", async ({
  page,
}) => {
  const transport = await documentTransport(page);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keep an offline edit" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "offline-edit.md",
  });

  // Two page timers below are the page's, not the server's: the read's two retries
  // (`QUERY_RETRY_DELAY_MS`) and the reconnect draw from [0, RECONNECT_SPREAD_MS).
  await page.clock.install();
  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");

  transport.hold();
  await transport.sever();
  await typeAtEnd(page, "offline edit");
  let refused = 0;
  const textRoute = `**/api/v1/artifacts/${upload.artifact.id}/text`;
  await page.route(textRoute, async (route) => {
    refused += 1;
    await route.fulfill({
      body: JSON.stringify({
        code: "DOC_SERVICE_UNAVAILABLE",
        error: "document service is unavailable",
      }),
      contentType: "application/json",
      status: 503,
    });
  });
  // A comment anchored in the document names it, so its event refreshes the document's reads, its
  // text among them; the app retries a 5xx twice before it fails the read. A drop of the page's
  // live socket would not: Dispatch replays every event the page missed (`replay_complete`), so a
  // gap that replay covers refreshes nothing.
  await createComment(issue.key, {
    anchor: { artifact: upload.artifact.slug, quote: "before" },
    body: "On the text.",
  });
  // Each refusal arms the next retry, so the jump repeats until the read has been given up.
  await expect
    .poll(
      async () => {
        await page.clock.runFor(QUERY_RETRY_DELAY_MS + 50);
        return refused;
      },
      { intervals: [50, 50, 100, 200, 500] }
    )
    .toBeGreaterThanOrEqual(3);
  // The third answer fails the read; nothing on a page that keeps its editor marks that moment, so
  // the page is given a moment of real time to render the failure before what it shows is read.
  await sleep(250);
  await expect(documentEditor(page)).toBeVisible();
  await expect(documentEditor(page)).toContainText("offline edit");
  await expect(page.getByRole("button", { name: "Rebuild from the latest version" })).toHaveCount(
    0
  );
  await expect(page.getByText("This document could not load")).toHaveCount(0);

  await page.unroute(textRoute);
  await transport.release();
  await page.clock.fastForward(RECONNECT_SPREAD_MS);
  await expect
    .poll(async () => (await getArtifactText(upload.artifact.id)).markdown)
    .toBe("before\n\noffline edit\n");
  await expect(documentEditor(page)).toContainText("offline edit");
});

test("a live document refuses a rebuild", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refuse live rebuild" });
  const upload = await createIssueArtifact(issue.key, {
    content: "before\n",
    name: "live-rebuild.md",
  });

  await page.goto(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(documentEditor(page)).toContainText("before");
  const rebuilt = await postAsPage(page, `/api/v1/artifacts/${upload.artifact.id}/rebuild`, {});
  expect(rebuilt.status()).toBe(409);
  await expect(rebuilt.text()).resolves.toContain(`"code":"DOCUMENT_LIVE"`);
  await expect(documentEditor(page)).toContainText("before");
});
