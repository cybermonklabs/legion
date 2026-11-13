import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import { HOLD_LIMIT_MS } from "../web/src/features/shell/content-drawn";
import {
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createNamedVersion,
  createProject,
  createProjectDocument,
  editArtifact,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  patchIssue,
} from "./api";
import { runDispatch } from "./dispatch-cli";
import {
  connectedDot,
  countDocumentSockets,
  cursorLabel,
  documentEditor,
  documentTransport,
  openDocumentSockets,
  openSpecAndAwaitHeadingIds,
  placeCaret,
  selectEditorText,
  typeAtEnd,
} from "./editor";
import { engineParagraphs } from "./read-back";
import { type BrowserContext, expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

const session = {
  actor: { kind: "session" as const, id: "e2e-doc-editor", origin: { tmux: "dispatch:1.3" } },
  as: "agent" as const,
};
const longToken = `sjawhar/legion#838-${"unbreakable".repeat(12)}`;
const initialMarkdown = `## Database\n\nUse SQLite\n\n| col | value |\n|---|---|\n| a | 1 |\n| b | ${longToken} |\n\n- [ ] task\n\n\`\`\`ts\nconst x = 1;\n\`\`\`\n\nEnd of spec.\n`;

test("the spec opens as a formatted, editable document with no source pane", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "READ", name: "Readable" });
  const issue = await createIssue({
    project: "READ",
    spec: initialMarkdown,
    title: "Rendering check",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);

    const documentArticle = page.locator('article[aria-label="Document"]');
    const editor = documentEditor(page);
    await expect(editor).toBeVisible();
    await expect(editor).toHaveAttribute("contenteditable", "true");

    const heading = documentArticle.getByRole("heading", { level: 2, name: "Database" });
    const paragraph = documentArticle.locator("p").first();
    await expect(heading).toBeVisible();
    await expect(paragraph).toBeVisible();
    const [headingSize, paragraphSize] = await Promise.all([
      heading.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
      paragraph.evaluate((element) => Number.parseFloat(getComputedStyle(element).fontSize)),
    ]);
    expect(headingSize).toBeGreaterThan(paragraphSize);

    const table = editor.locator("table");
    await expect(table).toBeVisible();
    const headerCell = table.locator("th").first();
    const [headerWeight, borderWidth, paddingLeft] = await Promise.all([
      headerCell.evaluate((element) => Number.parseInt(getComputedStyle(element).fontWeight, 10)),
      table
        .locator("td")
        .first()
        .evaluate((element) => Number.parseFloat(getComputedStyle(element).borderTopWidth)),
      table
        .locator("td")
        .first()
        .evaluate((element) => Number.parseFloat(getComputedStyle(element).paddingLeft)),
    ]);
    expect(headerWeight).toBeGreaterThanOrEqual(600);
    expect(borderWidth).toBeGreaterThan(0);
    expect(paddingLeft).toBeGreaterThan(0);
    // A cell holding an unbreakable token wraps instead of pushing the table into a horizontal
    // scroll, and on a wide desktop the editor is the column's width (the library's `prose` class
    // would cap it at 65ch, about 600px). The narrow-screen half is the row below this one.
    expect(
      await table.evaluate((element) => element.scrollWidth - element.clientWidth)
    ).toBeLessThanOrEqual(1);
    await page.setViewportSize({ width: 1800, height: 1000 });
    const [editorWidth, articleWidth] = await Promise.all([
      editor.evaluate((element) => element.getBoundingClientRect().width),
      documentArticle.evaluate((element) => element.getBoundingClientRect().width),
    ]);
    expect(articleWidth).toBeGreaterThan(700);
    expect(editorWidth).toBeGreaterThanOrEqual(articleWidth - 1);
    await expect(editor.locator('input[type="checkbox"]')).toBeVisible();
    await expect(editor.locator("pre code")).toBeVisible();
    await expect(page.getByRole("button", { name: /^(Edit|Preview)$/ })).toHaveCount(0);
    await expect(page.getByRole("tabpanel", { name: "Spec" }).locator("textarea")).toHaveCount(0);

    await page.screenshot({ path: testInfo.outputPath("rendered-document.png"), fullPage: true });
  } finally {
    await alice.close();
  }
});

// The narrow-screen half of the row above. The spec's table holds a token nothing can break, the
// widest thing a document can carry; on a 390 px screen it has to wrap inside its cell rather
// than push the table, and the page with it, sideways.
test("a spec whose table holds an unbreakable token keeps the page within the screen @phone @phone-only", async ({
  browser,
}) => {
  await createProject({ key: "READ", name: "Readable" });
  const issue = await createIssue({
    project: "READ",
    spec: initialMarkdown,
    title: "Rendering width",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    const table = documentEditor(page).locator("table");
    await expect(table).toBeVisible();
    expect(
      await table.evaluate((element) => element.scrollWidth - element.clientWidth)
    ).toBeLessThanOrEqual(1);
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)
    ).toBe(true);
  } finally {
    await alice.close();
  }
});

test("reading a document gives its headings ids without versioning it or crediting the reader, however the headings got there", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // An agent writes the spec, as an architect does, so every heading in it reaches the reader
  // without the id the editor derives.
  const issue = await createIssue(
    {
      project: "CORE",
      spec: "## Plan\n\nShip it.\n\n## Plan\n\nShip it again.\n",
      title: "Headed spec",
    },
    session
  );
  const artifactId = issue.primary_artifact_id;
  const readAs = async (context: BrowserContext, text: string) => {
    const page = await context.newPage();
    await openSpecAndAwaitHeadingIds(page, issue.key, artifactId, text);
    await page.close();
  };
  // A named version is written at once and names every pending author, so it is the next
  // version only if the read minted none, and it names only its namer only if the read credited
  // nobody.
  const nameVersion = async (summary: string) => {
    const named = await createNamedVersion(artifactId, summary, { login: "bob" });
    expect((named.authors ?? []).map(({ id }) => id)).toEqual(["bob"]);
    return named.number;
  };

  const alice = await asUser(browser, "alice");
  try {
    await readAs(alice, "Ship it again.");
    expect(await nameVersion("after the first read")).toBe(2);

    // An agent adds a heading, and the next reader's editor ids it.
    await editArtifact(
      artifactId,
      { ops: [{ after: "end", markdown: "## Rollout\n\nOne region first.", op: "insert" }] },
      session
    );
    await expect
      .poll(async () =>
        ((await getArtifact(artifactId)).versions ?? []).map(({ number }) => number)
      )
      .toEqual([1, 2, 3]);
    await readAs(alice, "One region first.");
    expect(await nameVersion("after the agent's heading")).toBe(4);
  } finally {
    await alice.close();
  }
});

// @pr: The two-context race at the heart of the editor: two people edit one spec, see each other's text and cursor, and settle one version.
test("two users edit the same spec, see each other's text and cursor, and settle one version attributed to both @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: initialMarkdown,
    title: "Database decision",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const [alicePage, bobPage] = await Promise.all([alice.newPage(), bob.newPage()]);
    // The two readers' loads overlap: neither waits on the other to mount its editor.
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const aliceEditor = documentEditor(alicePage);
    const bobEditor = documentEditor(bobPage);
    await expect(aliceEditor).toContainText("Use SQLite");
    await expect(bobEditor).toContainText("Use SQLite");
    await expect(connectedDot(alicePage)).toHaveText("connected");
    await expect(connectedDot(bobPage)).toHaveText("connected");

    await typeAtEnd(alicePage, "hello from alice");
    await expect(bobEditor).toContainText("hello from alice");

    await typeAtEnd(bobPage, "hello from bob");
    await expect(aliceEditor).toContainText("hello from bob");

    // Somewhere other than Alice's caret: a peer's caret on the focused local caret is not
    // drawn, and a bare click can land at the end of her line.
    await placeCaret(bobPage, "after", "Use SQLite");
    await expect(cursorLabel(alicePage, "bob")).toBeVisible();
    await expect(cursorLabel(bobPage, "alice")).toBeVisible();

    await expect
      .poll(() =>
        getArtifact(artifactId).then((artifact) =>
          (artifact.versions ?? [])
            .find(({ number }) => number === 2)
            ?.authors?.map(({ id }) => id)
            .sort()
        )
      )
      .toEqual(["alice", "bob"]);
    await expect
      .poll(() =>
        getArtifact(artifactId).then((artifact) =>
          (artifact.versions ?? []).find(({ number }) => number === 2)
        )
      )
      .toMatchObject({ named: false, number: 2, summary: null });
    await expect
      .poll(() => getArtifactVersion(artifactId, 2).then(({ markdown }) => markdown))
      .toContain("hello from alice");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("named versions, the version picker, and the diff stay current across users", async ({
  browser,
}) => {
  await createProject({ key: "VERS", name: "Versions" });
  const issue = await createIssue({
    project: "VERS",
    spec: initialMarkdown,
    title: "Version naming",
  });
  const artifactId = issue.primary_artifact_id;
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    const sockets = countDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    const editor = documentEditor(page);
    await expect(editor).toContainText("Use SQLite");
    await expect.poll(sockets).toBe(1);

    await editArtifact(
      artifactId,
      { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] },
      session
    );
    await expect(editor).toContainText("Use Postgres");
    await expect
      .poll(() =>
        getArtifact(artifactId).then((artifact) =>
          (artifact.versions ?? []).find(({ number }) => number === 2)
        )
      )
      .toMatchObject({ named: false, number: 2, summary: null });

    await page.getByRole("button", { name: "Name version" }).click();
    const nameDialog = page.getByRole("dialog", { name: "Name this version" });
    await nameDialog.getByLabel("What changed?").fill("Decided Postgres");
    await nameDialog.getByRole("button", { name: "Save" }).click();
    await expect(nameDialog).toHaveCount(0);
    await expect
      .poll(() =>
        getArtifact(artifactId).then((artifact) =>
          (artifact.versions ?? []).find(({ number }) => number === 3)
        )
      )
      .toMatchObject({ named: true, number: 3, summary: "Decided Postgres" });

    const versionPicker = page.getByRole("combobox", { name: "Version" });
    await expect(versionPicker).toContainText("Version 3 — Decided Postgres");
    await versionPicker.selectOption("1");
    const versionView = page.getByTestId("version-view");
    await expect(versionView).toContainText("Use SQLite");
    await expect(versionView.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
      "contenteditable",
      "false"
    );
    await expect.poll(sockets).toBe(1);

    await page.getByRole("button", { name: "Diff vs current" }).click();
    await expect(page.getByTestId("change-view").locator("del")).toHaveText("SQLite");
    await expect(page.getByTestId("change-view").locator("ins")).toHaveText("Postgres");

    await versionPicker.selectOption("");
    await expect(documentEditor(page)).toBeVisible();
    await expect.poll(sockets).toBe(1);
  } finally {
    await alice.close();
  }
});

// @pr: The live document: an agent's edit has to land in every open editor.
test("an agent edit lands live in every open editor @pr", async ({ browser }) => {
  await createProject({ key: "LIVE", name: "Live updates" });
  const issue = await createIssue({
    project: "LIVE",
    spec: initialMarkdown,
    title: "Agent edit",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const aliceEditor = documentEditor(alicePage);
    const bobEditor = documentEditor(bobPage);
    await expect(aliceEditor).toContainText("Use SQLite");
    await expect(bobEditor).toContainText("Use SQLite");

    await editArtifact(
      issue.primary_artifact_id,
      { ops: [{ find: "SQLite", op: "replace", with: "Postgres" }] },
      session
    );
    await expect(aliceEditor).toContainText("Use Postgres");
    await expect(bobEditor).toContainText("Use Postgres");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("reloading mid-edit reopens the same content", async ({ browser }) => {
  await createProject({ key: "LOAD", name: "Reloads" });
  const issue = await createIssue({
    project: "LOAD",
    spec: initialMarkdown,
    title: "Reload the document",
  });
  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");

  try {
    const alicePage = await alice.newPage();
    const aliceSockets = countDocumentSockets(alicePage);
    const bobPage = await bob.newPage();
    await Promise.all([
      alicePage.goto(`/issues/${issue.key}/spec`),
      bobPage.goto(`/issues/${issue.key}/spec`),
    ]);
    const bobEditor = documentEditor(bobPage);
    await expect(documentEditor(alicePage)).toContainText("Use SQLite");
    await expect(bobEditor).toContainText("Use SQLite");

    await typeAtEnd(alicePage, "before reload");
    await expect(bobEditor).toContainText("before reload");
    await alicePage.reload();
    // The twin of documentCollaborationLive's gate (document.e2e.ts): the editor's textbox role
    // is written at the end of the reload's admission chain, so wait for the reload's own socket
    // and for the editor that sync builds before reading the text back.
    await expect.poll(aliceSockets).toBe(2);
    await expect(documentEditor(alicePage)).toBeAttached();
    await expect(documentEditor(alicePage)).toContainText("Use SQLite");
    await expect(documentEditor(alicePage)).toContainText("before reload");
    await expect(alicePage.getByRole("combobox", { name: "Version" })).toContainText("Version 1");
  } finally {
    await bob.close();
    await alice.close();
  }
});

test("tab round-trips keep one document connection, the typed text, and the Conversation scroll position", async ({
  browser,
}) => {
  await createProject({ key: "TABS", name: "Tabs" });
  const issue = await createIssue({
    project: "TABS",
    spec: "# Keep this document",
    title: "Mounted panels",
  });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, session);
  }

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const sockets = countDocumentSockets(page);
    await page.goto(`/issues/${issue.key}/spec`);
    const editor = documentEditor(page);
    await typeAtEnd(page, "stay mounted");
    await expect(editor).toContainText("stay mounted");
    await expect.poll(sockets).toBe(1);

    await page.getByRole("tab", { name: "Conversation" }).click();
    await expect(page.getByRole("tab", { name: "Conversation" })).toBeFocused();
    await expect(page.getByText("Existing message 29")).toBeVisible();
    await page.evaluate(() => window.scrollTo(0, 500));
    const scrollPosition = await page.evaluate(() => window.scrollY);
    expect(scrollPosition).toBeGreaterThan(0);

    // Three tabs sit between them — Spec, Verification, Plan, Conversation — and the arrow
    // activates each tab it lands on, so the walk back mounts the two in between on the way.
    for (const from of ["Conversation", "Plan", "Verification"]) {
      await page.getByRole("tab", { name: from }).press("ArrowLeft");
    }
    await expect(page.getByRole("tab", { name: "Spec" })).toBeFocused();
    await expect(editor).toContainText("stay mounted");
    for (const from of ["Spec", "Verification", "Plan"]) {
      await page.getByRole("tab", { name: from }).press("ArrowRight");
    }
    await expect(page.getByRole("tab", { name: "Conversation" })).toBeFocused();
    await expect(page.getByText("Existing message 29")).toBeVisible();
    await expect.poll(sockets).toBe(1);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(scrollPosition);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): three visits to two documents, each mounting an editor, which is what one
// connection has to survive.
test("moving between issue documents leaves one open document connection, not one per visit", async ({
  browser,
}) => {
  await createProject({ key: "NAVDOC", name: "Navigation" });
  const first = await createIssue({
    project: "NAVDOC",
    spec: "# First document\n\nFirst body.\n",
    title: "First document",
  });
  const second = await createIssue({
    project: "NAVDOC",
    spec: "# Second document\n\nSecond body.\n",
    title: "Second document",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const open = openDocumentSockets(page);
    await page.goto(`/projects/NAVDOC/issues`);
    const table = page.getByRole("table", { name: "Project issues table" });
    await table.getByRole("link", { name: first.key, exact: true }).click();
    await expect(documentEditor(page)).toContainText("First body.");
    await expect.poll(open).toBe(1);

    // Reading around the tree the way a long-lived tab does — in-app navigation, no reload:
    // each document the reader leaves must take its connection with it, or the tab accumulates
    // live rooms until it stalls.
    // Each return to the list moves the pointer off it first. A pointer left on the link it just
    // followed rests on that link again once the list renders, and on a loaded machine rests long
    // enough to open its preview card, which covers the next row and takes the click meant for it.
    for (let visit = 0; visit < 4; visit += 1) {
      await page.mouse.move(0, 0);
      await page.goBack();
      await table.getByRole("link", { name: second.key, exact: true }).click();
      await expect(documentEditor(page)).toContainText("Second body.");
      await page.mouse.move(0, 0);
      await page.goBack();
      await table.getByRole("link", { name: first.key, exact: true }).click();
      await expect(documentEditor(page)).toContainText("First body.");
    }
    await expect.poll(open).toBe(1);
  } finally {
    await alice.close();
  }
});

// The invariant `preserveConnection: false` must preserve: now that destroying a document really
// closes its socket, a dropped transport must still bring the mounted editor back rather than
// end its session.
// SLOW(>=2s): two browser contexts and the provider's own reconnect after the drop, which no
// page clock can bring forward: the socket it reopens is a real one.
// @pr: The live document: a dropped transport reconnects the mounted editor and leaves one connection, not one per drop.
test("a dropped document transport reconnects the mounted editor and leaves one connection @pr", async ({
  browser,
}) => {
  await createProject({ key: "BLIP", name: "Blip" });
  const issue = await createIssue({
    project: "BLIP",
    spec: "# Transport blip\n\nOriginal body.\n",
    title: "Transport blip",
  });

  const alice = await asUser(browser, "alice");
  const bob = await asUser(browser, "bob");
  try {
    const page = await alice.newPage();
    const sockets = countDocumentSockets(page);
    const open = openDocumentSockets(page);
    const transport = await documentTransport(page);
    await page.goto(`/issues/${issue.key}/spec`);
    const editor = documentEditor(page);
    await expect(editor).toContainText("Original body.");
    await expect(connectedDot(page)).toHaveText("connected");
    await expect.poll(sockets).toBe(1);

    const bobPage = await bob.newPage();
    await bobPage.goto(`/issues/${issue.key}/spec`);
    await expect(documentEditor(bobPage)).toContainText("Original body.");

    await transport.sever();
    await expect.poll(sockets).toBe(2);
    await expect(connectedDot(page)).toHaveText("connected");
    await expect.poll(open).toBe(1);

    // The reconnected document is live in both directions, not merely re-opened.
    await typeAtEnd(page, "written after the drop");
    await expect(documentEditor(bobPage)).toContainText("written after the drop");
    await typeAtEnd(bobPage, "answered after the drop");
    await expect(editor).toContainText("answered after the drop");
    await expect.poll(open).toBe(1);
  } finally {
    await bob.close();
    await alice.close();
  }
});

/** How long a document socket that has delivered nothing is given before its provider closes it:
 *  `messageReconnectTimeout`, the HocuspocusProviderWebsocket default the editor's connection
 *  keeps (`web/src/features/doc/connection.ts`). */
const DOCUMENT_RECEIVE_TIMEOUT_MS = 30_000;
/** How far past that timeout the rows below take an idle document, so the provider has had every
 *  chance to close its socket. */
const PAST_THE_RECEIVE_TIMEOUT_MS = DOCUMENT_RECEIVE_TIMEOUT_MS + 12_000;
/** How much of the page's clock one step of that wait spends. The provider's connection check and
 *  the awareness renewal whose echo is the message it measures its silence against both run on a
 *  tenth of the timeout, so a step that size fires each of them in turn rather than firing a
 *  half-minute of them at once with no socket traffic in between. */
const PROVIDER_TIMER_STEP_MS = DOCUMENT_RECEIVE_TIMEOUT_MS / 10;
/** How long each step then leaves for what those timers sent to come back: the clock is the
 *  page's, the round trip over the socket is not. */
const PROVIDER_ECHO_MS = 100;

for (const row of [
  { key: "IDLE", reader: "an editor", readOnly: false },
  { key: "VIEW", reader: "a read-only viewer", readOnly: true },
]) {
  // SLOW(>=2s): fourteen clock steps through the provider's half-minute receive timeout, each
  // leaving PROVIDER_ECHO_MS of real time for what those timers sent to come back.
  test(`an idle document provider stays on one connection past its receive timeout, as ${row.reader}`, async ({
    browser,
  }) => {
    await createProject({ key: row.key, name: "Idle document" });
    const issue = await createIssue({
      project: row.key,
      spec: "# Quiet document\n\nIt stays connected.\n",
      title: "Idle document connection",
    });
    if (row.readOnly) {
      // A closed issue's document admits its socket read-only.
      await patchIssue(issue.key, { status: "done" });
    }

    const alice = await asUser(browser, "alice");
    try {
      const page = await alice.newPage();
      // The half-minute below is the provider's own, measured on the page's timers, so the page's
      // clock spends it rather than the run (`artifacts.e2e.ts:478` drives the same connection).
      await page.clock.install();
      const sockets = countDocumentSockets(page, issue.primary_artifact_id);
      await page.goto(`/issues/${issue.key}/spec`);
      await expect(documentEditor(page)).toContainText("It stays connected.");
      if (row.readOnly) {
        await expect(
          page.getByText("This issue is closed. Its document is read-only.")
        ).toBeVisible();
      }
      await expect(connectedDot(page)).toHaveText("connected");
      await expect.poll(sockets).toBe(1);

      // The provider closes a socket that has delivered nothing for DOCUMENT_RECEIVE_TIMEOUT_MS,
      // and what keeps it open is what its own timers ask for, so the wait goes a step at a time
      // with real time after each for the answer to land.
      const started = await page.evaluate(() => Date.now());
      for (let spent = 0; spent < PAST_THE_RECEIVE_TIMEOUT_MS; spent += PROVIDER_TIMER_STEP_MS) {
        await page.clock.runFor(PROVIDER_TIMER_STEP_MS);
        // The real time each step leaves for what those timers sent to come back: the clock
        // jumped above is the page's, and the round trip over the socket is not, so the step is
        // paced with `PROVIDER_ECHO_MS` of the wall clock before the next jump.
        await sleep(PROVIDER_ECHO_MS);
      }
      // However little of the run that cost, the page itself has spent the whole timeout: a
      // socket kept by a clock that never moved would prove nothing.
      const spentOnThePage = (await page.evaluate(() => Date.now())) - started;
      expect(spentOnThePage).toBeGreaterThanOrEqual(PAST_THE_RECEIVE_TIMEOUT_MS);

      await expect(connectedDot(page)).toHaveText("connected");
      expect(sockets()).toBe(1);
    } finally {
      await alice.close();
    }
  });
}

test("document marks and table alignment use classes rather than inline styles", async ({
  browser,
}) => {
  await createProject({ key: "MARKS", name: "Marks" });
  const issue = await createIssue({
    project: "MARKS",
    spec: "A marked phrase.\n\n| One | Two |\n| :--- | ---: |\n| Left | Right |\n",
    title: "Document decoration style",
  });
  await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "marked phrase" }, body: "Review this." },
    session
  );

  // Dispatch omits collaborative awareness below 1280 px, so exercise the peer selection in an
  // explicit desktop context.
  const [alice, selectionAlice, selectionBob] = await Promise.all([
    asUser(browser, "alice"),
    asUser(browser, "alice", { viewport: { height: 900, width: 1440 } }),
    asUser(browser, "bob", { viewport: { height: 900, width: 1440 } }),
  ]);
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    const editor = documentEditor(page);
    const decoration = editor.locator(".mark-comment");
    const rightCell = editor.locator("tbody td").nth(1);
    await expect(decoration).toHaveCount(1);
    await expect(editor.locator("[style]")).toHaveCount(0);
    await expect(rightCell).toHaveAttribute("data-proof-text-align", "right");
    await expect(rightCell.evaluate((cell) => getComputedStyle(cell).textAlign)).resolves.toBe(
      "right"
    );

    const selectionPage = await selectionAlice.newPage();
    await selectionPage.goto(`/issues/${issue.key}/spec`);
    const selectionEditor = documentEditor(selectionPage);
    const bobPage = await selectionBob.newPage();
    await bobPage.goto(`/issues/${issue.key}/spec`);
    await selectEditorText(bobPage, "marked phrase");
    const selection = selectionEditor.locator(".proof-collab-selection");
    await expect(selection).toHaveCount(1);
    await expect(selectionEditor.locator("[style]")).toHaveCount(0);
    await expect(
      selection.evaluate((span) => getComputedStyle(span).backgroundImage !== "none")
    ).resolves.toBe(true);
  } finally {
    await selectionBob.close();
    await selectionAlice.close();
    await alice.close();
  }
});

// Method names survive minification where class and import names do not: `getXmlFragment(`
// as a definition (not a `.getXmlFragment(` call) is Yjs's `Doc`, and `permissionDeniedHandler`
// is the Hocuspocus provider. Dispatch's own host code calls these; only the libraries define them.
const editorTransportMarkers = [/[^.\w$]getXmlFragment\(/u, /permissionDeniedHandler/u];

test("the Conversation tab downloads no editor transport; the Spec tab loads it on demand", async ({
  browser,
}) => {
  await createProject({ key: "LAZY", name: "Lazy" });
  const issue = await createIssue({
    project: "LAZY",
    spec: "# Lazy\n\nUse SQLite\n",
    title: "Transport on demand",
  });
  await createMessage(issue.key, { body: "A message, no document" }, session);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    const scripts = new Set<string>();
    page.on("request", (request) => {
      if (/\/assets\/.*\.js(\?|$)/u.test(request.url())) {
        scripts.add(request.url());
      }
    });
    const transportChunks = async () => {
      const chunks: string[] = [];
      for (const url of scripts) {
        const body = await (await page.request.get(url)).text();
        if (editorTransportMarkers.some((marker) => marker.test(body))) {
          chunks.push(url);
        }
      }
      return chunks;
    };

    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page.getByText("A message, no document")).toBeVisible();
    expect(scripts.size).toBeGreaterThan(1);
    expect(await transportChunks()).toEqual([]);

    await page.getByRole("tab", { name: "Spec" }).click();
    await expect(documentEditor(page)).toContainText("Use SQLite");
    expect((await transportChunks()).length).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the row waits out the shell's hold on purpose. A document whose transport never
// connects neither draws nor fails, so nothing but `HOLD_LIMIT_MS` ends the hold on the margin,
// the search bar, the preview layer and the event map (`web/src/features/shell/content-drawn.ts`);
// the row proves the page gets its margin and its live refresh all the same, with no key or
// click from the reader, since either would end the hold by itself.
test("a document that never connects still gets the page its margin and its live refresh", async ({
  browser,
}) => {
  await createProject({ key: "HOLD", name: "Held" });
  const issue = await createIssue({
    project: "HOLD",
    spec: "# Held\n\nThis document never connects.\n",
    title: "Before the event",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Holding every document socket: the editor never syncs, so it is never built.
    await documentTransport(page, { holding: true });
    await page.goto(`/issues/${issue.key}`);
    await expect(page.getByRole("heading", { level: 1, name: "Before the event" })).toBeVisible();

    // An event the feed takes while the hold is on: kept, and refreshed once the hold ends.
    await patchIssue(issue.key, { title: "After the event" });
    const pastTheLimit = { timeout: HOLD_LIMIT_MS + 10_000 };
    await expect(page.getByRole("heading", { level: 1, name: "After the event" })).toBeVisible(
      pastTheLimit
    );
    await expect(page.getByRole("complementary", { name: "Review margin" })).toBeVisible(
      pastTheLimit
    );
    await expect(documentEditor(page).locator("[data-block-id]")).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): eleven cold landings, each read for its own content and then for the margin.
test("every issue tab and a project's document get the margin once their own content has drawn", async ({
  browser,
}) => {
  await createProject({ key: "TABS", name: "Tabs" });
  const issue = await createIssue({
    project: "TABS",
    spec: ":::requirement{#r1}\nEach tab draws its margin.\n:::\n",
    title: "Every tab",
  });
  await createIssueArtifact(issue.key, {
    content: `:::check{#c1 serves="dispatch://${issue.key}/spec#b-r1" surface="browser" run="bun run e2e"}\nEach tab draws its margin.\n:::\n`,
    name: "verification.md",
  });
  await createIssueArtifact(issue.key, {
    content: `How ${issue.key} is built.\n\n:::plan_task{#t1 serves="dispatch://${issue.key}/spec#b-r1"}\nDraw the margin.\n:::\n`,
    name: "plan.md",
  });
  await createIssueArtifact(issue.key, {
    content: "# Notes\n\nA second document.\n",
    name: "notes.md",
  });
  await createProjectDocument("TABS", {
    content: "# Runbook\n\nA project document.\n",
    name: "runbook.md",
  });

  // The first block of a document is that page's content. The other tabs draw no document, so
  // their content is their panel once it holds more than the skeleton of its chunk.
  const block = (scope: string) => (page: Page) => page.locator(`${scope} [data-block-id]`).first();
  const panel = (tab: string) => async (page: Page) => {
    const element = page.locator(`#issue-${tab}-panel`);
    await expect(element.locator('[aria-busy="true"]')).toHaveCount(0);
    return element;
  };
  const landings: readonly {
    path: string;
    content: (page: Page) => Locator | Promise<Locator>;
    /** What the visual baseline of the page holds beside the margin, read when the margin is in. */
    withMargin?: (page: Page) => Locator;
  }[] = [
    { content: block("#issue-spec-panel"), path: `/issues/${issue.key}` },
    { content: block("#issue-verification-panel"), path: `/issues/${issue.key}/verification` },
    { content: block("#issue-plan-panel"), path: `/issues/${issue.key}/plan` },
    { content: panel("conversation"), path: `/issues/${issue.key}/conversation` },
    { content: panel("children"), path: `/issues/${issue.key}/children` },
    { content: panel("artifacts"), path: `/issues/${issue.key}/artifacts` },
    {
      content: block("#issue-artifacts-panel"),
      path: `/issues/${issue.key}/artifacts/notes-md`,
      // The ring that marks the artifact the address named is still on its header.
      withMargin: (page) => page.locator('[data-testid="artifact-header"].ring-2'),
    },
    { content: panel("activity"), path: `/issues/${issue.key}/activity` },
    { content: panel("decisions"), path: `/issues/${issue.key}/decisions` },
    { content: panel("collaborators"), path: `/issues/${issue.key}/collaborators` },
    {
      content: block('article[aria-label="Document"]'),
      path: "/projects/TABS/documents/runbook-md",
    },
  ];

  const alice = await asUser(browser, "alice");
  try {
    for (const { content, path, withMargin } of landings) {
      // A page of its own, with no key and no click: either would end the shell's hold by
      // itself (`web/src/features/shell/content-drawn.ts`), and the row is about what ends it
      // without the reader.
      const page = await alice.newPage();
      await page.goto(path);
      await expect(await content(page), path).toBeVisible();
      // Well under `HOLD_LIMIT_MS`: a landing whose own content never ends the hold gets its
      // margin only at that limit, and fails here.
      await expect(page.getByRole("tab", { name: "Comments" }), path).toBeVisible({
        timeout: 2_000,
      });
      if (withMargin !== undefined) await expect(withMargin(page), path).toBeVisible();
      await page.close();
    }
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the row waits out the shell's hold, as the row about a document that never
// connects does, so that it reads the ring over the whole hold and not for two seconds of it.
test("the ring on a landed artifact's header lasts two seconds from the moment its content draws", async ({
  browser,
}) => {
  await createProject({ key: "RING", name: "Ring" });
  const issue = await createIssue({ project: "RING", spec: "# Ring\n", title: "Ringed" });
  await createIssueArtifact(issue.key, {
    content: "# Notes\n\nNever connects.\n",
    name: "notes.md",
  });

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // Holding every document socket: the document never draws, so only `HOLD_LIMIT_MS` ends
    // the shell's hold (`web/src/features/shell/content-drawn.ts`).
    await documentTransport(page, { holding: true });
    await page.goto(`/issues/${issue.key}/artifacts/notes-md`);
    const header = page.getByTestId("artifact-header");
    const ringed = page.locator('[data-testid="artifact-header"].ring-2');
    await expect(ringed).toBeVisible();
    // Longer than two seconds from the header's mount: a ring counted from there is gone now.
    await sleep(3_000);
    await expect(ringed).toBeVisible();

    // The hold ends at its limit, and the margin comes in; the two seconds start then.
    await expect(page.getByRole("tab", { name: "Comments" })).toBeVisible({
      timeout: HOLD_LIMIT_MS + 10_000,
    });
    await expect(header).toBeVisible();
    await expect(ringed).toHaveCount(0, { timeout: 5_000 });
  } finally {
    await alice.close();
  }
});

test("a version deep link renders the comment's original quote highlighted in a read-only editor", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "LINK", name: "Linked documents" });
  const issue = await createIssue({
    project: "LINK",
    spec: "SQLite is local",
    title: "Deep link highlight",
  });
  const comment = await createComment(
    issue.key,
    { anchor: { artifact: "spec", quote: "SQLit" }, body: "Use the embedded store." },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(
      `/issues/${issue.key}/artifact/spec?v=1&comment=${encodeURIComponent(comment.id)}`
    );

    const versionView = page.getByRole("region", { name: "Document version 1" });
    const highlight = versionView.locator(`[data-id="${comment.id}"]`);
    await expect(highlight).toHaveText("SQLit");
    await expect(versionView.locator(".dispatch-mark-pulse")).toHaveCount(1);
    await expect(versionView.getByRole("textbox", { name: "Document editor" })).toHaveAttribute(
      "contenteditable",
      "false"
    );
    await page.screenshot({
      path: testInfo.outputPath("spec-deep-link-highlight.png"),
      fullPage: true,
    });
  } finally {
    await alice.close();
  }
});

test("clicking a Markdown dispatch:// link in the live editor navigates in-app without a reload", async ({
  browser,
}) => {
  await createProject({ key: "NAV", name: "Navigation" });
  const target = await createIssue({
    project: "NAV",
    title: "Navigation target",
  });
  const source = await createIssue({
    project: "NAV",
    spec: `See [the other issue](dispatch://${target.key}) for context.`,
    title: "Navigation source",
  });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${source.key}/spec`);

    const editor = documentEditor(page);
    // The live editor keeps the author's own link text (`@legion/proof-editor` exposes no
    // decoration hook to safely replace a live editable mark's rendered text) and surfaces the
    // resolved target in the hover card instead.
    const link = editor.getByRole("link", { name: "the other issue" });
    await expect(link).toBeVisible();
    await link.hover();
    await expect(page.getByRole("tooltip")).toContainText("Navigation target");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("tooltip")).toHaveCount(0);

    // A full page reload would reset this marker; an in-app route change preserves it.
    await page.evaluate(() => {
      (window as unknown as { __e2eNavMarker?: boolean }).__e2eNavMarker = true;
    });
    await link.click();

    await expect(page).toHaveURL(new RegExp(`/issues/${target.key}(/|$)`));
    expect(
      await page.evaluate(() => (window as unknown as { __e2eNavMarker?: boolean }).__e2eNavMarker)
    ).toBe(true);
  } finally {
    await alice.close();
  }
});

// INFRA-536: `\&` is an escaped ampersand, so `\&amp;` is the text `&amp;`, not `&` - in the
// browser editor's engine (remark/micromark), in the server's Go reading of the document, and in
// what an agent reads back from the command line.
test("a backslash before a character reference reads as its text in a document, a comment and the command line", async ({
  browser,
}, testInfo) => {
  const written = "Plain \\&amp; end.";
  const text = "Plain &amp; end.";
  await createProject({ key: "REF", name: "References" });
  const issue = await createIssue(
    { project: "REF", spec: `${written}\n`, title: "Escaped reference" },
    session
  );
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${issue.key}/spec`);
    const editor = documentEditor(page);
    await expect.poll(() => editor.locator("p").allTextContents()).toEqual([text]);

    // Stored as the canonical markdown for that text, which the engine reads back unchanged.
    const stored = (await getArtifactText(issue.primary_artifact_id)).markdown;
    expect(stored).toBe("Plain &amp;amp; end.\n");
    expect(engineParagraphs(stored)).toEqual([text]);

    // A comment's body is rendered by the engine alone: the control for the document's reading.
    const comment = await createComment(issue.key, { body: written }, session);
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(
      page.locator(`[data-turn="comment:${comment.id}"]`).getByText(text, { exact: true })
    ).toBeVisible();

    // The command runs outside the browser, so one project runs it.
    if (testInfo.project.name === "chromium") {
      const home = mkdtempSync(join(tmpdir(), "dispatch-cli-doc-read-"));
      try {
        const read = await runDispatch(
          ["doc-read", "--issue", issue.key, "--artifact", "spec"],
          home
        );
        expect(read.code, `${read.stdout}${read.stderr}`).toBe(0);
        const trailer = read.stdout.indexOf("Document token:");
        expect(trailer, read.stdout).toBeGreaterThan(-1);
        // The stored markdown, its own line ending, then the blank line before the trailer.
        expect(read.stdout.slice(0, trailer)).toBe("Plain &amp;amp; end.\n\n\n");
      } finally {
        rmSync(home, { force: true, recursive: true });
      }
    }
  } finally {
    await alice.close();
  }
});
