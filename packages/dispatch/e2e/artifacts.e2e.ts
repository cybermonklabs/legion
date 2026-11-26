import { writeFile } from "node:fs/promises";
import { setTimeout as sleep } from "node:timers/promises";

import { RECONNECT_SPREAD_MS } from "../web/src/api/live-socket";
import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";
import { scrollFadeTrailingPx } from "../web/src/theme/classes";

import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createProject,
  dashboardOrigin,
  documentBase,
  editArtifact,
  getArtifactText,
} from "./api";
import { countDocumentSockets, documentEditor, documentTransport, typeAtEnd } from "./editor";
import { barAction } from "./filters";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { navigateInApp } from "./sends";
import { expandedState } from "./settled";
import { expect, type Locator, type Page, type Request, type TestInfo, test } from "./test";
import { assertPageFits } from "./unclipped";

// The files a row uploads are written in that row's own output directory, which Playwright gives
// each row of each worker (`testInfo.outputPath`): two workers running these rows at once would
// otherwise write one path in the checkout, and a run must leave no file changed under e2e/.
async function diagramFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("diagram.png");
  await writeFile(path, Buffer.from(ONE_PIXEL_PNG, "base64"));
  return path;
}

async function notesFile(testInfo: TestInfo): Promise<string> {
  const path = testInfo.outputPath("notes.md");
  await writeFile(path, "# Review notes\n\nThese notes replace the initial spec.\n");
  return path;
}

/** The phone's review panel toggle, whose name says Open or Close as the panel is shut or open. */
function reviewPanelToggle(page: Page): Locator {
  return page.getByRole("button", { name: /review panel/i });
}

/** Shuts the phone's review panel where it is open over the page. The toggle says which it is,
 *  and is read once the page has drawn it rather than at the instant it attaches. */
async function shutReviewPanel(page: Page): Promise<void> {
  const toggle = reviewPanelToggle(page);
  if ((await expandedState("the review panel", toggle)) === "open") await toggle.click();
}

async function openArtifacts(page: Page, isPhone: boolean) {
  if (isPhone) await shutReviewPanel(page);
  await page.getByRole("tab", { name: "Artifacts" }).click();
}

// Choosing a file only stages it; the row then shows a Summary field plus a confirming
// "Upload" button (or "Cancel" to discard the pick) before it actually creates the version.
async function confirmUpload(page: Page): Promise<void> {
  await page.getByRole("button", { exact: true, name: "Upload" }).click();
}

// Whole inside the window: no part of `control` left of the window's left edge or past its right
// edge, where a phone reader can neither see nor press it. `name` is what a failure calls it. The
// window is the width the browser was given, not `window.innerWidth`: a page wider than a phone's
// window widens `innerWidth` to the page (`assertPageFits`, `unclipped.ts`), so a 480 px control
// on a 390 px phone sits inside an `innerWidth` of 504. The failure names both.
async function expectWithinWindow(page: Page, control: Locator, name: string): Promise<void> {
  await expect(control).toBeVisible();
  const [box, innerWidth] = await Promise.all([
    control.boundingBox(),
    page.evaluate(() => window.innerWidth),
  ]);
  const windowWidth = page.viewportSize()?.width;
  if (box === null || windowWidth === undefined) {
    throw new Error(`expected ${name} to have a box in a window of a set width`);
  }
  const right = box.x + box.width;
  const where = `${name} spans ${box.x}px to ${right}px (${box.width}px wide) in a ${windowWidth}px window (innerWidth ${innerWidth}px)`;
  expect(box.x, where).toBeGreaterThanOrEqual(0);
  expect(right, where).toBeLessThanOrEqual(windowWidth);
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

  await page.goto(`/issues/${issue.key}`);
  await page.getByRole("tab", { name: "Spec" }).click();
  await expect(documentEditor(page)).toContainText("The original document.");
  await expect.poll(sockets).toBe(1);
  await openArtifacts(page, testInfo.project.name === "iphone");
  // The tab's own slot of the control row ran a staged upload off a 390 px window once (CI run
  // 43204682061), and #3508 put the `New document` control in that same slot beside `Upload`. So
  // on the phone each control there lies whole inside the window and takes a press, and the form
  // `New document` opens fits too. Both are now actions of the tab's control bar, items of its
  // More below 640 px of the bar's width, as at the phone's width (R8).
  if (testInfo.project.name === "iphone") {
    const newDocument = await barAction(page, "New document");
    const pickFile = await barAction(page, "Upload");
    await expectWithinWindow(page, newDocument, "New document");
    await expectWithinWindow(page, pickFile, "Upload");
    // Playwright holds the chooser the press opens, and it is left unanswered: nothing is staged.
    const chooser = page.waitForEvent("filechooser");
    await pickFile.click();
    await chooser;
    await (await barAction(page, "New document")).click();
    const newDocumentForm = page.getByRole("form", { name: "New document" });
    await expectWithinWindow(
      page,
      newDocumentForm.getByRole("textbox", { exact: true, name: "Title" }),
      "the New document form's Title field"
    );
    await expectWithinWindow(
      page,
      newDocumentForm.getByRole("button", { exact: true, name: "Create" }),
      "the New document form's Create"
    );
    await newDocumentForm.getByRole("button", { exact: true, name: "Cancel" }).click();
    await expect(newDocumentForm).toHaveCount(0);
  }

  const upload = page.getByLabel("Upload artifact");
  const diagramUpload = await diagramFile(testInfo);
  await upload.setInputFiles(diagramUpload);
  // What run 43204682061 caught: the staged file takes the slot's line, and its Summary field and
  // the two presses that store or discard it stay inside the window.
  if (testInfo.project.name === "iphone") {
    await expectWithinWindow(
      page,
      page.getByRole("textbox", { exact: true, name: "Summary (optional)" }),
      "the staged upload's Summary field"
    );
    await expectWithinWindow(
      page,
      page.getByRole("button", { exact: true, name: "Upload" }),
      "the staged upload's Upload"
    );
    await expectWithinWindow(
      page,
      page.getByRole("button", { exact: true, name: "Cancel" }),
      "the staged upload's Cancel"
    );
  }
  await confirmUpload(page);
  const diagram = page.getByTestId("artifact-diagram-png");
  await expect(diagram).toContainText("diagram.png");
  await expect(diagram).toContainText("1 version");
  await expect(diagram.getByRole("img", { name: "diagram.png version 1" })).toBeVisible();

  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  await expect(diagram).toContainText("2 versions");
  await expect(diagram.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "download",
    ""
  );
  // The tab is a compact row list now — versions, compare, and references live on the
  // artifact's own page, not stacked under every row.
  await expect(diagram.getByRole("button", { name: "Show all versions" })).toHaveCount(0);
  await expect(page.getByLabel("Filter artifacts")).toBeVisible();

  await navigateInApp(page, `/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page.getByTestId("artifact-header")).toContainText("diagram.png");
  expect(sockets()).toBe(1);
  await page.getByRole("button", { name: "Show all versions" }).click();
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  await expect(versions).toContainText("Version 1");
  await expect(versions).toContainText("Version 2");
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
  const notes = page.getByTestId("artifact-notes-md");
  await expect(notes).toContainText("notes.md");
  await expect(notes.getByRole("button", { name: /make primary/i })).toHaveCount(0);
  await page.getByRole("tab", { name: "Spec" }).click();
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

  await diagram.getByRole("link", { name: "diagram.png" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  // The panel names the record that cites this artifact and quotes it; its link goes to that
  // source, not back to the artifact the reader is already on. The quote is the head of the
  // comment's body, drawn by the renderer, so the citation in it is a reference to the artifact
  // and no address is left as text.
  const diagramReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(diagramReferencedBy).toContainText(`Comment · ${issue.key}`);
  const diagramExcerpt = diagramReferencedBy.locator(".dispatch-markdown");
  await expect(diagramExcerpt).toContainText("before deciding.");
  await expect(diagramExcerpt.locator("[data-dispatch-ref]")).toHaveAttribute(
    "data-dispatch-ref",
    `dispatch://${issue.key}/artifact/diagram-png`
  );
  await expect(diagramExcerpt).not.toContainText("dispatch://");
  await diagramReferencedBy.getByRole("link", { name: `Comment · ${issue.key}` }).click();
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/comments/`));

  // One cold load, so the notes.md panel below is read by a page that cached nothing.
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, testInfo.project.name === "iphone");
  await notes.getByRole("link", { name: "notes.md" }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  const notesReferencedBy = page.getByRole("region", { name: "Referenced by" });
  await expect(notesReferencedBy).toContainText(`Comment · ${issue.key}`);
  const notesExcerpt = notesReferencedBy.locator(".dispatch-markdown");
  await expect(notesExcerpt).toContainText("before deciding.");
  await expect(notesExcerpt.locator("[data-dispatch-ref]")).toHaveAttribute(
    "data-dispatch-ref",
    `dispatch://${issue.key}/artifact/notes-md@v1`
  );
  await expect(notesExcerpt).not.toContainText("dispatch://");
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
    const openMargin = reviewPanelToggle(page);
    await shutReviewPanel(page);
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
    // `scrollWidth <= innerWidth` cannot fail on a phone, which widens `innerWidth` to a page
    // wider than its window; `assertPageFits` holds `innerWidth` to the window as well.
    await assertPageFits(page);
  }
});

// The base a multipart upload names: the value of its `precondition` form field, parsed, and
// null when the upload carries no such field. The page posts a `FormData` for a picked file, and
// the server reads the base from that field (`multipartArtifactUpload` in the server's
// `api/artifacts.go`), so this is the base as the server receives it rather than as the page
// meant it. No unit test reads this: every one of them mocks the client the page uploads
// through, so what the request carries is read here or nowhere.
function uploadedBase(request: Request): unknown {
  const field = /name="precondition"\r?\n\r?\n([^\r\n]*)/.exec(request.postData() ?? "");
  return field === null ? null : JSON.parse(field[1] ?? "");
}

// A person uploading a markdown document over a name the issue already holds must name the
// token of the version they are replacing, so what the page sends and what it shows are read
// here rather than in jsdom alone. The page reads that token when the person chooses the file
// and every attempt of the press sends the one it read; the upload is then held in its own
// route while an agent changes the document under it, so the change lands between the page's
// read and the server's reading of the request, and no wall clock decides it. The second half
// is what `web/src/features/artifacts/ArtifactUpload.test.tsx` asserts against a mocked client
// - the server's own sentence names `dispatch doc-read` and `--precondition-json`, and a person
// at a browser runs no command and pastes no token, so the row must say what the refusal means
// instead.
test("an upload over a document carries the token the page read, and says what a change under it means", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Upload over a document" });
  const notes = await createIssueArtifact(issue.key, {
    content: "# Review notes\n\nThe ledger keeps three leases.\n",
    name: "notes.md",
  });

  const bases: unknown[] = [];
  // What the next upload meets at the server: the first one meets nothing and is taken, and the
  // second meets this edit, made after the page read its token.
  let changeUnderTheUpload: (() => Promise<unknown>) | null = null;
  await page.route(`**/api/v1/issues/${issue.key}/artifacts`, async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    bases.push(uploadedBase(route.request()));
    const change = changeUnderTheUpload;
    changeUnderTheUpload = null;
    if (change !== null) {
      await change();
    }
    return route.fallback();
  });

  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, false);
  const upload = page.getByLabel("Upload artifact");
  const row = page.getByTestId("artifact-notes-md");
  await expect(row).toContainText("1 version");

  // The base the live document holds now, which is what the page reads for itself before it
  // posts: the upload names it, and the version it writes is the proof the server took it.
  const base = await documentBase(notes.artifact.id);
  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  await expect(row).toContainText("2 versions");
  expect(bases).toEqual([base]);
  expect((await getArtifactText(notes.artifact.id)).markdown).toBe(
    "# Review notes\n\nThese notes replace the initial spec.\n"
  );

  // The second upload names the token the document holds now, and an agent moves the document
  // past it while the upload is held, so the server reads a base that is one version behind.
  const held = await documentBase(notes.artifact.id);
  changeUnderTheUpload = () =>
    editArtifact(
      notes.artifact.id,
      {
        ops: [
          {
            find: "These notes replace the initial spec.",
            op: "replace",
            with: "The reviewer rewrote these notes.",
          },
        ],
        summary: "The reviewer's own words",
      },
      { as: "agent" }
    );
  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);

  const refusal = page.getByRole("alert");
  await expect(refusal).toContainText("changed");
  await expect(refusal).not.toContainText("dispatch doc-read");
  await expect(refusal).not.toContainText("--precondition-json");
  expect(bases).toEqual([base, held]);
  // Nothing of the refused upload was written: the document still says what the agent left.
  expect((await getArtifactText(notes.artifact.id)).markdown).toBe(
    "# Review notes\n\nThe reviewer rewrote these notes.\n"
  );
});

// The From/To compare of a document shows what changed as the change view, word by word. It is
// seeded on a document that is not the issue's primary one, because `ArtifactRoutePanel` mounts
// `ArtifactDetails` - the version history and this compare - only off the primary document's own
// route (`isPrimaryDocumentArtifactRoute`, `features/refs/routes.ts`).
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
  // Each side is a list, portalled out of the region, and the one open list's options are the
  // ones pressed.
  await compare.getByRole("button", { name: "Compare notes.md from" }).click();
  await page.getByRole("option", { name: /^Version 1(?: — |$)/ }).click();
  await compare.getByRole("button", { name: "Compare notes.md to" }).click();
  await page.getByRole("option", { name: /^Version 2(?: — |$)/ }).click();
  const changed = compare.getByTestId("change-view").locator('[data-change="changed"]');
  await expect(changed).toHaveCount(1);
  await expect(changed.locator("del")).toHaveText(["three"]);
  await expect(changed.locator("ins")).toHaveText(["seven"]);
});

test("the artifacts tab filter narrows rows by name and kind", async ({ page }, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Artifact filtering" });
  await page.goto(`/issues/${issue.key}`);
  await openArtifacts(page, false);

  const upload = page.getByLabel("Upload artifact");
  await upload.setInputFiles(await diagramFile(testInfo));
  await confirmUpload(page);
  await expect(page.getByTestId("artifact-diagram-png")).toBeVisible();
  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  await expect(page.getByTestId("artifact-notes-md")).toBeVisible();

  const filter = page.getByLabel("Filter artifacts");
  await filter.fill("notes");
  await expect(page.getByTestId("artifact-notes-md")).toBeVisible();
  await expect(page.getByTestId("artifact-diagram-png")).toHaveCount(0);

  await filter.fill("image");
  await expect(page.getByTestId("artifact-diagram-png")).toBeVisible();
  await expect(page.getByTestId("artifact-notes-md")).toHaveCount(0);

  await filter.fill("");
  await expect(page.getByTestId("artifact-diagram-png")).toBeVisible();
  await expect(page.getByTestId("artifact-notes-md")).toBeVisible();
});

// SLOW(>=2s): a real upload the live feed counts into the tab, and the keyboard tour of ten
// tabs with each tab's box and the tablist's fade measured, at 390 px and back, then the comment
// margin hidden and shown again.
test("an uploaded document is linked from the Activity tab and counted on its own tab", async ({
  page,
}, testInfo) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({ project: "OPS", title: "Customer update" });
  await page.goto(`/issues/${issue.key}/conversation`);
  // Every issue has its own document, so the tab opens counting one.
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts 1" })).toBeVisible();

  const name = "cu-update-2026-09-15.md";
  const upload = await createIssueArtifact(issue.key, {
    content: "# Draft\n\nHi team, here is this week's update.\n",
    name,
  });

  // The live event feed refreshes the issue and its tabs: no reload.
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts 2" })).toBeVisible();
  const tablist = page.getByRole("tablist", { name: "Issue detail" });
  const tabs = tablist.getByRole("tab");
  await expect(tabs).toHaveCount(10);
  const insideTablist = async (tab: Locator) => {
    const [listBox, box] = await Promise.all([tablist.boundingBox(), tab.boundingBox()]);
    expect(listBox).not.toBeNull();
    expect(box).not.toBeNull();
    expect(box?.height).toBeLessThanOrEqual(48);
    expect(box?.x).toBeGreaterThanOrEqual(listBox?.x ?? 0);
    expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(
      (listBox?.x ?? 0) + (listBox?.width ?? 0) + 0.5
    );
  };
  // Ten tabs, one of them counted, no longer fit a 390 px viewport: the tablist scrolls
  // sideways there (as it already did at 360 px). No label is shortened to buy the room, every
  // tab is reachable from the keyboard, each is scrolled fully into view as it takes focus, and
  // each keeps its 44 px touch target. While tabs lie past its trailing edge the tablist wears
  // the trailing fade, which says the cut is a scroll, and a tab taking focus stops clear of it;
  // at the end of the scroll the fade is off, since it would hide the last tab. A tablist that
  // narrows with no change of tab - a narrower window, the comment margin opening - brings the
  // selected tab back whole and clear of the fade too.
  const masked = () => tablist.evaluate((node) => getComputedStyle(node).maskImage !== "none");
  // Polled: the reveal that answers a narrower tablist runs on the browser's next resize
  // observation, after the change that narrowed it.
  const wholeAndClearOfFade = (tab: Locator) =>
    expect
      .poll(async () => {
        const [faded, listBox, box] = await Promise.all([
          masked(),
          tablist.boundingBox(),
          tab.boundingBox(),
        ]);
        const listLeft = listBox?.x ?? 0;
        const listRight = listLeft + (listBox?.width ?? 0);
        const left = box?.x ?? Number.NEGATIVE_INFINITY;
        const right = left + (box?.width ?? 0);
        const visibleRight = faded ? listRight - scrollFadeTrailingPx + 1 : listRight + 0.5;
        return left >= listLeft - 0.5 && right <= visibleRight;
      })
      .toBe(true);
  const viewport = page.viewportSize();
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 390 });
  await expect(tabs).toHaveText([
    "Spec",
    "Verification",
    "Plan",
    "Steps",
    "Conversation",
    "Children",
    "Artifacts 2",
    "Activity",
    "Decisions",
    "Collaborators",
  ]);
  expect(await tablist.evaluate((node) => node.scrollWidth > node.clientWidth + 0.5)).toBe(true);
  // The row opened on Conversation, which the narrower window left cut off at the edge.
  const conversationTab = page.getByRole("tab", { exact: true, name: "Conversation" });
  await expect(conversationTab).toHaveAttribute("aria-selected", "true");
  await wholeAndClearOfFade(conversationTab);
  await page.getByRole("tab", { name: "Spec" }).focus();
  await expect.poll(masked).toBe(true);
  for (const label of [
    "Verification",
    "Plan",
    "Steps",
    "Conversation",
    "Children",
    "Artifacts 2",
    "Activity",
    "Decisions",
    "Collaborators",
  ] as const) {
    await page.keyboard.press("ArrowRight");
    const tab = page.getByRole("tab", { exact: true, name: label });
    await expect(tab).toBeFocused();
    await insideTablist(tab);
    await wholeAndClearOfFade(tab);
    expect((await tab.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
  }
  await page.keyboard.press("Home");
  const specTab = page.getByRole("tab", { name: "Spec" });
  await expect(specTab).toBeFocused();
  await insideTablist(specTab);
  expect((await specTab.boundingBox())?.height ?? 0).toBeGreaterThanOrEqual(44);
  await expect.poll(masked).toBe(true);
  await page.keyboard.press("End");
  await expect(page.getByRole("tab", { exact: true, name: "Collaborators" })).toBeFocused();
  await expect.poll(masked).toBe(false);
  await page.setViewportSize({ height: viewport?.height ?? 800, width: viewport?.width ?? 1280 });
  // At 1280 px every tab fits beside the hidden margin, and Activity, chosen there, is whole. The
  // margin coming back narrows the tablist with no change of tab, and Activity, which it left past
  // the trailing edge, is brought back whole and clear of the fade.
  await page.getByRole("button", { name: "Hide margin" }).click();
  await expect(page.getByTestId("margin-rail")).toBeVisible();
  const activityTab = page.getByRole("tab", { name: "Activity" });
  await activityTab.click();
  await expect(activityTab).toHaveAttribute("aria-selected", "true");
  await expect.poll(masked).toBe(false);
  await page.getByRole("button", { name: "Show margin" }).click();
  await expect.poll(masked).toBe(true);
  await wholeAndClearOfFade(activityTab);

  const rows = page.getByRole("list", { name: "Issue activity" }).getByRole("listitem");
  const added = rows.filter({ hasText: `added ${name}` });
  await expect(added).toHaveCount(1);
  await page.screenshot({ path: testInfo.outputPath("activity-added-artifact.png") });

  // A second upload under the same name is a new version; its line links that version. It
  // replaces the whole document, so it names the token of the version it replaces.
  await createIssueArtifact(issue.key, {
    content: "# Draft\n\nRevised.\n",
    name,
    precondition: await documentBase(upload.artifact.id),
  });
  const saved = rows.filter({ hasText: `saved ${name} v2` });
  await expect(saved).toHaveCount(1);
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts 2" })).toBeVisible();
  await expect(saved.getByRole("link", { name })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/artifacts/${upload.artifact.slug}?v=2`
  );

  // The upload is an Activity line and nothing else: the Conversation draws no row for it.
  await page.getByRole("tab", { name: "Conversation" }).click();
  await expect(
    page.getByRole("list", { name: "Conversation turns" }).getByText(`added ${name}`)
  ).toHaveCount(0);
  await page.getByRole("tab", { name: "Activity" }).click();

  await added.getByRole("link", { name }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByTestId("artifact-header")).toContainText(name);
});

// The issue's tab strip rests where its last layout puts the selected tab: at the smallest scroll
// that shows that tab whole and clear of the trailing fade. The tablist can be narrower for a
// moment while the page lays out, and a reveal that only ever added the scroll it needed left the
// strip where that moment put it once the tablist widened again: the visual row of this page then
// read the strip at the end of its scroll, 212 px from where its baseline is shot. Here the
// tablist is made 300 px wide for a moment, with the Artifacts tab selected, and released: it must
// scroll for the narrow moment, and come back to the scroll its own layout gives it, not stay.
test("the issue's tab strip rests where its last layout puts the selected tab, whatever narrower layout came first", async ({
  page,
}) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({ project: "OPS", title: "A strip that settles" });
  await page.goto(`/issues/${issue.key}/artifacts`);
  const tablist = page.getByRole("tablist", { name: "Issue detail" });
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts 1" })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  await expect(tablist.getByRole("tab")).toHaveCount(10);
  // The comment margin loads after the first paint, and until it is drawn the tablist is wider
  // and every tab fits: a resting point read then would be the start, and the strip moves again
  // once the margin arrives. So the resting point is read only with the margin drawn.
  await expect(page.getByTestId("desktop-margin-shell")).toBeVisible();
  // Where the strip is, and where its layout alone puts the selected tab: its right edge in the
  // strip's content, less what the fade leaves of the tablist. Sub-pixel layout rounds a scroll
  // to a whole pixel, so the two are compared within one.
  const place = () =>
    tablist.evaluate((list, fade) => {
      const tab = list.querySelector('[role="tab"][aria-selected="true"]');
      const right = (tab?.getBoundingClientRect().right ?? 0) - list.getBoundingClientRect().left;
      const target = Math.max(0, right + list.scrollLeft - (list.clientWidth - fade));
      return {
        offBy: Math.abs(list.scrollLeft - target),
        overflow: list.scrollWidth - list.clientWidth,
        scrollLeft: list.scrollLeft,
      };
    }, scrollFadeTrailingPx);
  await expect.poll(async () => (await place()).offBy).toBeLessThanOrEqual(1);
  const { overflow, scrollLeft: rested } = await place();
  // The strip can scroll past where it rests, so a strip left at the end of its scroll is a
  // different reading from the resting one, and the last assertion below can fail.
  expect(overflow).toBeGreaterThan(rested + 1);

  await tablist.evaluate((list) => {
    list.style.width = "300px";
  });
  // The narrow moment moves the strip, or this row proves nothing.
  await expect.poll(async () => (await place()).scrollLeft).toBeGreaterThan(rested + 50);
  await tablist.evaluate((list) => {
    list.style.width = "";
  });
  // Polled: the strip answers a layout change on the browser's next resize observation.
  await expect.poll(async () => (await place()).offBy).toBeLessThanOrEqual(1);
  expect(Math.abs((await place()).scrollLeft - rested)).toBeLessThanOrEqual(1);
});

test("the Artifacts badge counts the rows the tab lists, and a row's details are not truncated @phone", async ({
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
  const rows = page.locator("[data-testid^='artifact-']");
  await expect(rows).toHaveCount(2);
  await expect(page.getByRole("tab", { exact: true, name: "Artifacts 2" })).toBeVisible();

  // Every row shows its whole detail line: it wraps where it must rather than truncating on a
  // row with room beside it. The count of two above waited for both rows, so one read lists them.
  for (const row of await rows.all()) {
    const details = row.locator("p").first();
    await expect(details).toContainText("Updated");
    expect(await details.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
      true
    );
  }
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
  await expect(page.getByText("A person decides whether to replace the document")).toBeVisible();
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
  const row = page.getByTestId(`artifact-${upload.artifact.slug}`);
  const openDocument = () => row.getByRole("link", { name: "cached-repair.md" }).click();

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

  await expect(page.getByText("A person decides whether to replace the document")).toBeVisible();
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

  await expect(page.getByText("A person decides whether to replace the document")).toBeVisible();
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
