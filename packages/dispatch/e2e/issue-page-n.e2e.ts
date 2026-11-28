/** the rows upload documents and files, name their versions and read the references the server
 *  indexes, then read back what the server stored; the history's live feed and an opened
 *  document's live socket carry the server's own frames */

/*
 * Part N of dispatch://INFRA-1196 (dispatch://INFRA-3940): the rows that only the whole new issue
 * page can pass, read with the new page turned on (`withIssuePage`, through `phase` or each row's
 * own context). Each row is its source's, under its source's title: the checkpoint 29111db16c41's
 * `e2e/issue-layout.e2e.ts`, `e2e/artifacts.e2e.ts` and `e2e/keyboard-issue.e2e.ts`, and
 * `750e2716`'s copies of today's rows that the checkpoint put back to `main`'s text. `main`'s
 * files of those rows do not change: they read today's page.
 */

import { readFile, writeFile } from "node:fs/promises";
import { fixture } from "../web/src/__tests__/pmdoc-fixtures";
import type { ArtifactUploadResponse } from "../web/src/api/types";
import { assigneeControlName } from "../web/src/features/issue/AssigneeControl";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  CONTROL_LABELS,
  DASHBOARD_WORDS,
  EMPTY_STATE_LINES,
  FILE_WORDS,
  NODE_REFERENCES_WORDS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
  PRESET_ORDER,
  PULL_REQUEST_STATE_WORDS,
  STATE_LINES,
  sentenceCase,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/statement-rules";
import { VIEW_LOADING } from "../web/src/features/issue-page/view-loading-line";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  dashboardOrigin,
  documentBase,
  editArtifact,
  editAsk,
  getArtifact,
  getArtifactBlocks,
  getArtifactText,
  getArtifactVersion,
  getAsk,
  getIssue,
  getIssueEvents,
  getMessage,
  getMyState,
  listComments,
  listIssueArtifacts,
  patchIssue,
  putIssueState,
  requestApproval,
  rotateLiveSockets,
} from "./api";
import {
  countDocumentSockets,
  documentEditor,
  openDocumentSockets,
  selectTextIn,
  typeAtEnd,
} from "./editor";
import { nextFrames } from "./frames";
import {
  type AdvancingIssueSeed,
  seedWholeIssue,
  seedWholeIssueAdvancing,
  type WholeIssueSeed,
} from "./issue-layout-seed";
import { PROJECT } from "./issue-layout-seed-support";
import { requirementBlockId } from "./issue-layout-seed-text";
import {
  closeDetails,
  filterRow,
  gotoIssue,
  issuePage,
  marginShell,
  openDetails,
  openMenu,
  phase,
  presetChooser,
  presetToggle,
  presetToggles,
  presetToolbar,
} from "./issue-page";
import {
  DESKTOP,
  drawnRows,
  expectIssuePageDrawn,
  expectOpeningLanded,
  focusRow,
  focusThePage,
  nodeRow,
  nodeRows,
  PHONE,
  READER,
  rowWords,
  tree,
  unfoldEveryBranch,
} from "./issue-page-g";
import { marginSection, marginSlot } from "./issue-page-h";
import {
  activePreset,
  caretInNode,
  childRows,
  expectActivePreset,
  focusedNodeId,
  focusInside,
  onScreen,
  openPreset,
  outlineRows,
  treeActiveDescendant,
} from "./issue-page-i";
import {
  composer,
  composerField,
  composerTarget,
  seedHistoryTurns,
  sendFromComposer,
} from "./issue-page-l";
import {
  daemonOf,
  focusedNodeFacts,
  historyEntries,
  historyItem,
  historyPanel,
  rowFacts,
  stepsSection,
} from "./issue-page-m";
import {
  COLOUR_STATES,
  expectDocumentConnected,
  type HeaderSize,
  headerLimit,
  headerSize,
  openIssueAddress,
  openSpecDocument,
  pageOffThePalette,
  seedWaitingWalk,
} from "./issue-page-n";
import {
  documentLink,
  documentRow,
  documentsList,
  documentsRows,
  expectActiveControl,
  expectDashboardDrawn,
  filterBox,
  filterControl,
  firstDocumentLink,
  openedArtifact,
  rowSelector,
  specAddress,
  uploadInDocuments,
  VIEWS_FOCUS,
} from "./issue-page-n-documents";
import { withIssuePage } from "./issue-page-option";
import {
  expectHeaderDrawn,
  TWENTY_LABELS as HEADER_LABELS,
  IPHONE_13,
  issueHeader,
  labelsAgainstTheCount,
  lineOrder,
  MEASURED_TITLE,
  propertiesLine,
  seedHeader,
  stickyReading,
  viewportAt,
  waitingCount,
} from "./issue-page-p";
import { connect, isLiveSocket, type LiveSockets, watchLiveSockets } from "./live-socket-watch";
import { ONE_PIXEL_PNG } from "./one-pixel-png";
import { sql } from "./psql";
import {
  ANCHOR,
  type ReferenceWorld,
  referenceAgent,
  seedReferenceWorld,
} from "./reference-surfaces";
import { seedLargestScaleCopy } from "./scale-seed";
import { navigateInApp } from "./sends";
import { settledValue } from "./settled";
import {
  type Browser,
  type BrowserContext,
  test as dispatchTest,
  expect,
  type Locator,
  type Page,
  type Request,
  type Route,
  type TestInfo,
  type WebSocketRoute,
} from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";
import { assertWhole } from "./unclipped";
import { asUser } from "./users";

/** e2e/test.ts's `test`, whose own context - the `page` a row takes - has the new page turned on
 *  for the login it signs in (`withIssuePage`): each row of this file reads the new page. */
const test = dispatchTest.extend({
  context: async ({ context, signedIn }, use) => {
    if (signedIn !== false) await withIssuePage(context, signedIn);
    await use(context);
  },
});

/** A context signed in as `login` with the new page turned on, for a row that opens its own. */
async function asReader(browser: Browser, login: string): Promise<BrowserContext> {
  const context = await asUser(browser, login);
  await withIssuePage(context, login);
  return context;
}

// ---- from 29111db16c41:packages/dispatch/e2e/issue-layout.e2e.ts

// R5 of dispatch://INFRA-3940 (dispatch://INFRA-3678, under R13): the count of what waits on the
// reader is a control, and its number is the length of Dashboard's list. On the issue this row
// seeds - one question, one decision to review, an approval request on the spec, a stale approval
// of the Verification document and two unread threads: six items - a press, or Enter on the count,
// from an opened document closes it, pushes Dashboard's address and puts the focus on the list's
// first item. Each press after goes on to the next item, shows the next five past the fifth and
// comes round to the first, the list showing its first five again, so every item the count counts
// is reached by presses alone. An approval request alone, at the bare address that draws
// Dashboard, takes the press as a move of the focus, with no new address.
test("the count of what waits on the reader opens the first thing that waits @phone", async ({
  browser,
}) => {
  test.setTimeout(150_000);
  await phase(browser, seedWaitingWalk, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    const items = page.locator('[data-testid^="dashboard-item-"]');
    const nextFive = page.getByRole("button", {
      name: new RegExp(`^${DASHBOARD_WORDS.next_five}`),
    });
    const drawn = () =>
      items.evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-testid") ?? ""));
    const focused = () =>
      page.evaluate(() => document.activeElement?.getAttribute("data-testid") ?? "");
    const pressCount = async (key: "click" | "Enter") => {
      if (key === "click") await waitingCount(page).click();
      else {
        await waitingCount(page).focus();
        await page.keyboard.press("Enter");
      }
    };
    const settling = watchApiRequests(page);
    for (const [width, key] of (desktopProject
      ? [
          [1440, "click"],
          [390, "Enter"],
        ]
      : [[390, "click"]]) as readonly (readonly [number, "click" | "Enter"])[]) {
      await page.setViewportSize({ height: width < 1024 ? PHONE.height : DESKTOP.height, width });
      await gotoIssue(page, await specAddress(seeded.key));
      await expectLoadSettled(settling);
      await expect(openedArtifact(page), "the document did not open").toBeVisible();
      await expect(waitingCount(page)).toHaveText(`Waiting on you (${seeded.items})`);
      await pressCount(key);
      await expect(openedArtifact(page), "the count left the document open").toHaveCount(0);
      await expect
        .poll(focused, { message: `the focus is not on the first item that waits at ${width} px` })
        .toMatch(/^dashboard-item-/);
      await expect(page, "the count did not push Dashboard's address").toHaveURL(
        new RegExp(`/issues/${seeded.key}/dashboard$`)
      );
      await expectActiveControl(page, "dashboard");
      await expect(
        page.getByTestId("dashboard-waiting-count"),
        "the header's count is not the length of Dashboard's list"
      ).toHaveText(String(seeded.items));
      const first = (await drawn())[0];
      expect(await focused(), `the focus is off Dashboard's first item at ${width} px`).toBe(first);
      expect(await items.count(), "Dashboard did not draw its first five").toBe(5);

      // Each press after the first goes on to the next item in the list's order, and the one past
      // the fifth draws the next five: every item the count holds is reached by pressing it.
      const walked = [first];
      for (let press = 1; press < seeded.items; press += 1) {
        await pressCount(key);
        let next = "";
        await expect
          .poll(
            async () => {
              next = await focused();
              return next.startsWith("dashboard-item-") && !walked.includes(next);
            },
            { message: `press ${press + 1} of the count did not move the focus on at ${width} px` }
          )
          .toBe(true);
        walked.push(next);
      }
      expect(await drawn(), `the presses did not walk the list's order at ${width} px`).toEqual(
        walked
      );
      expect(await nextFive.count(), "the next five were drawn and their control stayed").toBe(0);
      const kinds = new Set(walked.map((id) => id.replace(/^dashboard-item-/, "").split("-")[0]));
      expect(kinds, "the presses reached no item of one of the five kinds").toEqual(
        new Set(["approval", "decision", "question", "stale", "thread"])
      );

      // The press after the last comes round to the first, and the list shows its first five.
      await pressCount(key);
      await expect
        .poll(focused, { message: `the focus did not come round at ${width} px` })
        .toBe(first);
      await expect(items, "the list did not show its first five again").toHaveCount(5);
      await expect(nextFive, "the control for the next five did not come back").toHaveCount(1);
    }

    // An approval request is all that waits: at the bare address, which draws Dashboard, the press
    // moves the focus to that request's item and writes no history entry.
    const lone = await createIssue({ force: true, project: PROJECT, spec: "Plan.", title: "Lone" });
    await requestApproval(lone.primary_artifact_id ?? "", { summary: "Read it." });
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${lone.key}`);
    await expect(waitingCount(page)).toHaveText("Waiting on you (1)");
    const entries = await page.evaluate(() => history.length);
    await waitingCount(page).click();
    await expect
      .poll(focused, { message: "the press at the bare address did not reach the request's item" })
      .toMatch(/^dashboard-item-approval-/);
    expect(
      await page.evaluate(() => history.length),
      "a press at the bare address wrote a history entry"
    ).toBe(entries);
    await expect(page).toHaveURL(new RegExp(`/issues/${lone.key}$`));
  });
});

/** How many characters of an id an address writes short: the fewest the server resolves. */
const SHORT_ID = 8;

/** The measure each reading of the header's labels is in the shipped code (`HeaderLabels`'s
 *  `measured`, `web/src/features/issue-page/IssuePropertiesRow.tsx`). */
const HEADER_LABELS_MEASURE = "issue-header-labels";

// c-header-labels (R21, decision 500 (3), the plan's g24): on production's build the header's
// reading of which label chips its line holds (`HeaderLabels`, `IssuePropertiesRow.tsx`) took
// about 1.1 s of a load of INFRA-1196 with 20 labels, several forced layouts a reading on a page
// that large. Each reading is one `issue-header-labels` measure, and the row sums them over one
// cold load of Conversation's address, `/issues/<key>/conversation`, the outline, where
// production's reading of about 1.1 s was measured; the bare address opens Dashboard, a smaller
// page, on which the merge-base's reading took 17.3 ms (builds Job e2e-rows-p6qk7) against
// 125.6 ms at Conversation (e2e-rows-5ldfv). The load is of the copy of INFRA-3149, the largest
// copy `e2e/scale-seed.ts` writes, written alone (`seedLargestScaleCopy`) with 20 labels, signed
// in as its reader, who never read it: at 1440 px under chromium and at 390 px under iphone, each
// project seeding a copy of its own. The sum is read at network idle (`expectLoadSettled`) and one
// second more, and printed; then the line, at rest, holds no chip past its first line or its end,
// and its count names every chip it leaves out.
// SLOW(>=2s): the copy of INFRA-3149 written - 69 documents of 45,738 blocks, 39 files and 113
// comments - and the block index's drain waited for, then one cold load of it.
test("the header fits its labels within 50 ms of a load @phone", async ({ browser }) => {
  // Measured in builds Job e2e-rows-5tkp5 at dd4508e8, with the cold load at Conversation's
  // address: 1.1 min on chromium and 37.1 s on iphone, each the copy written and the block index
  // drained, then one cold load; the bound is about three times the longer, for a slow leg.
  test.setTimeout(200_000);
  const viewport = test.info().project.name === "iphone" ? IPHONE_13 : DESKTOP;
  const { copy, reader } = await seedLargestScaleCopy();
  await patchIssue(copy.key, { labels: [...HEADER_LABELS] });
  const where = `the cold load of ${copy.key}, the copy of ${copy.census} with ${HEADER_LABELS.length} labels, at ${viewport.width} px`;
  expect(
    (await getMyState(reader))[copy.key]?.last_read_seq ?? 0,
    `${reader} has read ${copy.key} before ${where}, so it is no load of a person who never read it`
  ).toBe(0);
  const context = await asReader(browser, reader);
  try {
    const page = await context.newPage();
    await page.setViewportSize(viewport);
    const traffic = watchApiRequests(page);
    await gotoIssue(page, `/issues/${copy.key}/conversation`);
    await expectHeaderDrawn(page);
    await expectLoadSettled(traffic);
    // The second past network idle that c-header-labels names: a reading the load's last answers
    // set off, by a width they changed or a typeface that finished loading, is taken and
    // measured in the frames of that second.
    await page.waitForTimeout(1_000);
    const readings = await page.evaluate(
      (name) => performance.getEntriesByName(name, "measure").map((entry) => entry.duration),
      HEADER_LABELS_MEASURE
    );
    const sum = readings.reduce((total, duration) => total + duration, 0);
    const each = readings.map((duration) => duration.toFixed(1)).join(", ");
    const said = `SCALE header labels ${copy.census} ${viewport.width} px: ${readings.length} readings, ${sum.toFixed(1)} ms (${each})`;
    console.log(said);
    test.info().annotations.push({ description: said, type: "header-labels" });
    expect(
      readings.length,
      `${where} took no ${HEADER_LABELS_MEASURE} measure, so the header read none of its labels`
    ).toBeGreaterThan(0);
    expect(
      sum,
      `the header's label reading took ${sum.toFixed(1)} ms of ${where}, more than 50 ms, in ${readings.length} readings: ${each} ms`
    ).toBeLessThanOrEqual(50);

    // The line once it is at rest: the chips it holds, those of them drawn whole and those cut,
    // and the number its count names.
    const drawn = await settledValue(`the header's labels after ${where}`, async () => ({
      ...(await labelsAgainstTheCount(page)),
      held: await propertiesLine(page).evaluate(
        (properties) =>
          properties.querySelectorAll('[data-testid="issue-header-labels"] > [data-header-label]')
            .length
      ),
    }));
    expect(drawn.cut, `${where}: the labels' box cuts a chip the line holds`).toEqual([]);
    expect(
      drawn.held,
      `${where}: the line holds ${drawn.held} chips and draws ${drawn.whole} of them whole on its first line`
    ).toBe(drawn.whole);
    expect(
      drawn.whole + drawn.left,
      `${where}: ${drawn.whole} chips drawn whole and a count of ${drawn.left}, of ${HEADER_LABELS.length} labels`
    ).toBe(HEADER_LABELS.length);
  } finally {
    await context.close();
  }
});

// ---- from 29111db16c41:packages/dispatch/e2e/artifacts.e2e.ts

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

/** Opens the issue's Documents' list at its own address by a move inside the page, closing
 *  Details first where it covers a phone's page (dispatch://INFRA-1196 R14). The list took the
 *  Artifacts tab's list (part N's R2): each row carries what the tab's row offered beside the
 *  file - the link that opens it, its copy control, its latest version's download and, for a
 *  document the issue can drop, Remove - and the file's versions, its picture and what references
 *  it are on its own page, which the row's link opens. */
async function openDocumentsList(page: Page, isPhone: boolean) {
  if (isPhone) await closeDetails(page);
  await openIssueAddress(page, "documents");
  await expect(documentsList(page), "Documents' list did not open").toBeVisible();
}

// Choosing a file only stages it; the row then shows a Summary field plus a confirming
// "Upload" button (or "Cancel" to discard the pick) before it actually creates the version.
async function confirmUpload(page: Page): Promise<void> {
  await page.getByRole("button", { exact: true, name: CONTROL_LABELS.upload }).click();
}

// `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
// cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin).
function postAsPage(page: Page, path: string, data?: object) {
  return page.request.post(path, { data, headers: { Origin: dashboardOrigin } });
}

// SLOW(>=2s): three real uploads with the versions they build, the artifact page's version list
// and comparison, and the references and phone layout read after them.
// Off the pull-request lane: `main`'s source row in `artifacts.e2e.ts` runs there, and the lane's
// real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
test("artifacts upload, version, references, and phone layout @phone", async ({
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

  const specPath = await specAddress(issue.key);
  await page.goto(specPath);
  await expect(documentEditor(page)).toContainText("The original document.");
  await expect.poll(sockets).toBe(1);
  await openDocumentsList(page, testInfo.project.name === "iphone");

  // Documents' list's upload stages the picked file, with its Summary, Upload and Cancel.
  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagramUpload = await diagramFile(testInfo);
  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  const diagram = documentRow(page, "diagram.png");
  await expect(diagram).toBeVisible();
  await expect(
    diagram.getByRole("link", { name: `${CONTROL_LABELS.download_version} 1` })
  ).toBeVisible();

  await upload.setInputFiles(diagramUpload);
  await confirmUpload(page);
  await expect(
    diagram.getByRole("link", { name: `${CONTROL_LABELS.download_version} 2` })
  ).toHaveAttribute("download", "");
  // The list's row is compact: the versions, their count, the picture, the compare and the
  // references live on the artifact's own page, not under every row.
  await expect(diagram.getByRole("button", { name: "Show all versions" })).toHaveCount(0);
  await expect(page.getByLabel(CONTROL_LABELS.filter_files)).toBeVisible();

  await navigateInApp(page, `/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  await expect(page.getByTestId("artifact-header")).toContainText("diagram.png");
  await expect(page.getByRole("img", { name: "diagram.png version 2" })).toBeVisible();
  expect(sockets()).toBe(1);
  // Every version is drawn open under the file, with no control that hides any (part N's R2), so
  // the list is read with nothing pressed.
  const versions = page.getByRole("region", {
    exact: true,
    name: `${FILE_WORDS.versions_for} diagram.png`,
  });
  await expect(versions).toContainText("Version 1");
  await expect(versions).toContainText("Version 2");
  await expect(versions).not.toContainText("Version 3");
  await expect(versions).toContainText("SHA-256");
  await expect(
    versions.getByRole("link", { name: `${CONTROL_LABELS.download_version} 2` })
  ).toHaveAttribute("download", "");
  const compare = page.getByRole("region", {
    name: `${CONTROL_LABELS.compare_versions_for} diagram.png`,
  });
  await expect(compare).toBeVisible();
  await expect(compare).not.toContainText("From · Version");
  await compare.getByRole("button", { name: CONTROL_LABELS.show_comparison }).click();
  await expect(compare).toContainText("From · Version");
  await expect(compare).toContainText("To · Version");

  await navigateInApp(page, `/issues/${issue.key}`);
  await openDocumentsList(page, testInfo.project.name === "iphone");

  await upload.setInputFiles(await notesFile(testInfo));
  await confirmUpload(page);
  const notes = documentRow(page, "notes.md");
  await expect(notes).toBeVisible();
  await expect(notes.getByRole("button", { name: /make primary/i })).toHaveCount(0);
  await navigateInApp(page, specPath);
  await expect(documentEditor(page)).toContainText("The original document.");

  const notesCitation = await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/notes-md@v1 before deciding.`,
  });

  const diagramCitation = await createComment(issue.key, {
    body: `See dispatch://${issue.key}/artifact/diagram-png before deciding.`,
  });
  // No reload: both citations reach the open page on its event feed, and the panel below reads
  // what the feed left.
  await openDocumentsList(page, testInfo.project.name === "iphone");

  await documentLink(page, "diagram.png").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  // The panel names the record that cites this artifact and quotes it; its link goes to that
  // source, not back to the artifact the reader is already on. What references the opened
  // artifact is drawn open, in the page's own words (part N's R2, R12): a comment of the issue's
  // own is named by its words, beside the page's word for a comment, with no issue named, and the
  // words name the document they cite by the name the page's one batched read gave it, its file
  // name, as a plain line reads a link to it (main's one reference framework, #3125); the page's
  // word for a document stands only until that read answers.
  const diagramWords = "See diagram.png before deciding.";
  const diagramRow = openedArtifact(page)
    .getByRole("region", { name: PROPERTY_LABELS.referenced_by })
    .getByRole("listitem")
    .filter({ has: page.getByRole("link", { exact: true, name: diagramWords }) });
  const diagramSource = diagramRow.getByRole("link", { exact: true, name: diagramWords });
  await expect(diagramSource).toHaveAttribute(
    "href",
    `/issues/${issue.key}/comments/${diagramCitation.id}`
  );
  await expect(diagramRow).toContainText(NODE_REFERENCES_WORDS.kinds.comment);
  await diagramSource.click();
  await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/comments/`));

  // One cold load, so the notes.md panel below is read by a page that cached nothing.
  await page.goto(`/issues/${issue.key}`);
  await openDocumentsList(page, testInfo.project.name === "iphone");
  await documentLink(page, "notes.md").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/notes-md`);
  const notesWords = "See notes.md before deciding.";
  const notesRow = openedArtifact(page)
    .getByRole("region", { name: PROPERTY_LABELS.referenced_by })
    .getByRole("listitem")
    .filter({ has: page.getByRole("link", { exact: true, name: notesWords }) });
  await expect(notesRow.getByRole("link", { exact: true, name: notesWords })).toHaveAttribute(
    "href",
    `/issues/${issue.key}/comments/${notesCitation.id}`
  );
  await expect(notesRow).toContainText(NODE_REFERENCES_WORDS.kinds.comment);
  await navigateInApp(page, `/issues/${issue.key}`);
  await openDocumentsList(page, testInfo.project.name === "iphone");
  await page.screenshot({ path: testInfo.outputPath("artifacts-tab.png"), fullPage: true });

  if (testInfo.project.name === "iphone") {
    await page.goto("/");
    // A phone's Inbox lists short rows; the question reads on its row's first line.
    await expect(page.locator("[data-inbox-row]").first()).toContainText("Can this ship?");
    await page.screenshot({ path: testInfo.outputPath("iphone-inbox.png"), fullPage: true });

    const openNavigation = page.getByRole("button", { name: "Open navigation" });
    await expect(openNavigation).toBeVisible();
    await openNavigation.click();
    await expect(page.getByRole("navigation", { name: "Navigation" })).toBeVisible();
    await page.getByRole("button", { name: "Close navigation" }).click();
    await navigateInApp(page, `/issues/${issue.key}`);
    // On the issue page the panel is Details, which the header's control opens over the page,
    // the phone's sheet the whole width (dispatch://INFRA-1196 R14). The header's controls -
    // Open navigation and Details among them - take WCAG 2.2's 24 px target, not the 44 px floor
    // of today's page: the header at 390 px is held to eight lines and 250 px (ruling 8,
    // decision 47, the header's rule in `styles.css`).
    const openMargin = page
      .locator("header[data-issue-header]")
      .getByRole("button", { exact: true, name: CONTROL_LABELS.details });
    const [navigationBox, marginBox] = await Promise.all([
      openNavigation.boundingBox(),
      openMargin.boundingBox(),
    ]);
    expect(navigationBox?.width).toBeGreaterThanOrEqual(24);
    expect(navigationBox?.height).toBeGreaterThanOrEqual(24);
    expect(marginBox?.width).toBeGreaterThanOrEqual(24);
    expect(marginBox?.height).toBeGreaterThanOrEqual(24);
    await openMargin.click();
    await expect(
      page.getByRole("dialog", { exact: true, name: CONTROL_LABELS.details })
    ).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("iphone-details.png"), fullPage: true });
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
  const compare = page.getByRole("region", {
    name: `${CONTROL_LABELS.compare_versions_for} notes.md`,
  });
  await compare.getByRole("button", { name: CONTROL_LABELS.show_comparison }).click();
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
  await openDocumentsList(page, false);

  const upload = page.getByLabel(CONTROL_LABELS.upload_file);
  const diagram = documentRow(page, "diagram.png");
  const notes = documentRow(page, "notes.md");
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

// Documents' list's upload stages a picked file as the Artifacts list's did (dispatch://INFRA-1196
// R19, "nothing lost"; part N's R2): the file's name, an optional Summary for the version it makes,
// and Upload and Cancel. Cancel sends nothing; Upload sends the file with its summary.
test("the node menu's upload stages a file with a Summary for its version, and Cancel sends nothing", async ({
  page,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Staged upload" });
  const held = (await listIssueArtifacts(issue.key)).length;
  await page.goto(`/issues/${issue.key}/documents`);

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
  await expect(documentRow(page, "notes.md")).toBeVisible();
  const notes = (await listIssueArtifacts(issue.key)).find((one) => one.name === "notes.md");
  expect(notes?.versions?.map((version) => version.summary)).toEqual(["First review notes"]);
});

// SLOW(>=2s): a real upload the history links, and the keyboard tour of the seven presets with
// each tab's box measured, at 1440 px and at 390 px.
//
// The nine tabs are gone (dispatch://INFRA-1196 R3): the row is the views' controls, Documents
// among them (part N's R4), and how many artifacts an issue carries is Documents' list's, not a
// tab's badge, so the `Artifacts (N)` assertions this test carried are retired with it - what
// they proved, that the live feed refreshes the page's own counts without a reload, is proved
// here on the history, in Details.
//
// It is not tagged `@phone`: the row sets 390 px itself, so the iphone project would run the
// same assertions a second time (#2866, no browser row runs twice).
test("an uploaded document is linked from the history and the presets fit every width", async ({
  page,
}, testInfo) => {
  await createProject({ key: "OPS", name: "Ops" });
  const issue = await createIssue({ project: "OPS", title: "Customer update" });
  await page.goto(`/issues/${issue.key}/activity`);

  // The history is drawn before the upload, so the line below is one the live feed brought and
  // not one the first read already held: without this the upload can land before that read
  // answers, and the row would pass with the feed disconnected. This is what the retired
  // `Artifacts 1` badge reading proved - that the page's own counts move without a reload.
  const rows = historyEntries(page);
  await expect(rows.filter({ hasText: "created the issue" })).toHaveCount(1);

  const name = "cu-update-2026-09-15.md";
  const added = rows.filter({ hasText: `added ${name}` });
  await expect(added).toHaveCount(0);
  const upload = await createIssueArtifact(issue.key, {
    content: "# Draft\n\nHi team, here is this week's update.\n",
    name,
  });

  // The live event feed puts the upload on the open history without a reload.
  await expect(added).toHaveCount(1);
  await page.screenshot({ path: testInfo.outputPath("activity-added-artifact.png") });

  // A second upload under the same name is a new version of the document. A document's versions
  // are its own toolbar's, and the history draws no line for one (dispatch://INFRA-1196 R15,
  // c-conversation-state), where the Activity list drew `saved <name> v2`: read afresh once
  // Dispatch holds the version, the history still names the document on its one line. It
  // replaces the whole document, so it names the token of the version it replaces.
  await createIssueArtifact(issue.key, {
    content: "# Draft\n\nRevised.\n",
    name,
    precondition: await documentBase(upload.artifact.id),
  });
  await expect
    .poll(
      async () =>
        (await listIssueArtifacts(issue.key)).find((one) => one.name === name)?.versions?.length
    )
    .toBe(2);
  await page.reload();
  await expect(rows.filter({ hasText: "created the issue" })).toHaveCount(1);
  await expect(rows.filter({ hasText: name })).toHaveCount(1);
  await expect(added).toHaveCount(1);

  const viewport = page.viewportSize();
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 1440 });
  const toolbar = presetToolbar(page);
  const toggles = presetToggles(page);
  await expect(toggles).toHaveCount(PRESET_ORDER.length);
  const insideToolbar = async (toggle: Locator) => {
    const [rowBox, box] = await Promise.all([toolbar.boundingBox(), toggle.boundingBox()]);
    expect(rowBox).not.toBeNull();
    expect(box).not.toBeNull();
    expect(box?.x).toBeGreaterThanOrEqual(rowBox?.x ?? 0);
    expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(
      (rowBox?.x ?? 0) + (rowBox?.width ?? 0) + 0.5
    );
  };
  // What the row promises at 1440 px, which is a pointer's width: each toggle is the
  // prototype's tab, 35 px tall (`web/src/features/issue/IssuePresets.tsx`), and no taller than
  // the row it shares. The touch target is read below at the width a finger is what reaches the
  // filters: these seven are not drawn there at all, because below 1280 px the row is one
  // control instead, and that control carries the target.
  const fitsTheRow = async (toggle: Locator, label: string) => {
    const height = (await toggle.boundingBox())?.height ?? 0;
    expect(
      height,
      `the ${label} toggle is ${height} px tall, under the prototype's tab`
    ).toBeGreaterThanOrEqual(32);
    expect(
      height,
      `the ${label} toggle is ${height} px tall, taller than the row of seven`
    ).toBeLessThanOrEqual(48);
  };
  // Every control is reachable from the keyboard, each is inside the row as it takes focus, and
  // each is drawn at the height the row gives it.
  const [head, ...rest] = PRESET_ORDER;
  const first = presetToggle(page, head);
  await first.focus();
  for (const preset of rest) {
    await page.keyboard.press("ArrowRight");
    const toggle = presetToggle(page, preset);
    await expect(toggle).toBeFocused();
    await insideToolbar(toggle);
    await fitsTheRow(toggle, PRESET_NAMES[preset]);
  }
  await page.keyboard.press("Home");
  await expect(first).toBeFocused();
  await insideToolbar(first);
  await fitsTheRow(first, PRESET_NAMES[head]);

  // At 390 px the row is one control naming the one on, which opens the rest. This is the
  // width a finger is what reaches the filters, so this is where the touch target is read: the
  // one control a reader presses there carries it, and the row fails if it ever loses it. The
  // target is WCAG 2.2's 24 px (2.5.8), not the 44 px floor of today's tabs: the view row is one
  // line at most 51 px tall below 1280 px, 8 px above and below its controls, and each of them
  // is at least 24 px tall (part N's R4, ruling 5, the filter row's rule in `styles.css`).
  await page.setViewportSize({ height: viewport?.height ?? 800, width: 390 });
  // The history is in Details, which its address opens (dispatch://INFRA-1196 R15) and which on
  // a phone is a sheet over the whole page, so a reader closes it to reach the filters.
  await closeDetails(page);
  await expect(toggles).toHaveCount(0);
  const chooser = presetChooser(page);
  await expect(chooser).toBeVisible();
  const chooserHeight = (await chooser.boundingBox())?.height ?? 0;
  expect(
    chooserHeight,
    `the preset control is ${chooserHeight} px tall at 390 px, under the 24 px a touch needs`
  ).toBeGreaterThanOrEqual(24);
  await chooser.click();
  // Each view's control is a radio item of the menu, the one on checked (part D's
  // `IssuePresets.tsx`), Documents among them (part N's R4).
  await expect(page.getByRole("menu").getByRole("menuitemradio")).toHaveCount(PRESET_ORDER.length);
  await page.keyboard.press("Escape");
  await page.setViewportSize({ height: viewport?.height ?? 800, width: viewport?.width ?? 1280 });

  // The upload is a line of the history and nothing else: a document is never a row of the
  // outline (R3), which draws no row naming it. The issue holds nothing else, so the outline is
  // drawn empty, with the page's line saying so once its reads have answered.
  await page.goto(`/issues/${issue.key}/conversation`);
  await expect(tree(page)).toHaveCount(1);
  await expect(
    issuePage(page).getByText(EMPTY_STATE_LINES.conversation, { exact: true })
  ).toBeVisible();
  await expect(tree(page).getByText(name)).toHaveCount(0);
  await page.goto(`/issues/${issue.key}/activity`);

  await added.getByRole("link", { name }).click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/${upload.artifact.slug}`);
  await expect(page.getByTestId("artifact-header")).toContainText(name);
});

// The Artifacts tab's badge and its rows' detail lines went with the tab (dispatch://INFRA-1196
// R3): Documents' list holds one row per file, whose name is never cut, and a file's versions are
// drawn open on its own page (part N's R2).
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

  await page.goto(`/issues/${issue.key}/documents`);
  const files = documentsRows(page).filter({
    has: page.getByRole("link", { name: new RegExp(`^${CONTROL_LABELS.download_version}`) }),
  });
  await expect(files).toHaveCount(2);

  // Every row shows its whole name: it wraps where it must rather than truncating. The count of
  // two above waited for both rows, so one read lists them.
  for (const file of await files.all()) {
    const name = file.getByRole("link").first();
    expect(await name.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true);
  }
  await documentLink(page, "audit.md").click();
  // Every version is drawn open, with no control that hides any (part N's R2).
  await expect(
    page.getByRole("region", { exact: true, name: `${FILE_WORDS.versions_for} audit.md` })
  ).toContainText("Version 1");
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
  const row = documentRow(page, "cached-repair.md");
  const openDocument = () => documentLink(page, "cached-repair.md").click();

  await page.goto(`/issues/${issue.key}`);
  await openDocumentsList(page, false);
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

// ---- from 29111db16c41:packages/dispatch/e2e/keyboard-issue.e2e.ts

test("the issue scope reaches the header controls and the tab chords, and ? lists them under Issue", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard issue" });
  // `t h` is the outline's own chord here, offered where the tree holds a child and no key at
  // all where it holds none (part I's `useTreeKeymap.ts`, its row "the children are one group of
  // rows of the outline"), where the source bound it in the issue's scope from anywhere: so the
  // issue the chord is read on carries a child.
  await createIssue({ parent: issue.key, project: "CORE", title: "A child to move to" });
  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    // Conversation's own address, where the outline and its opening are: the issue's own draws
    // Dashboard (dispatch://INFRA-5702), which draws no outline. The keymap binds only once
    // sign-in resolves, so the header's title is waited for before a key is pressed.
    await page.goto(presetPath(issue.key, "conversation"));
    await expect(
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Keyboard issue" })
    ).toBeVisible();
    await page.locator("body").focus();
    // The outline opens with the focus on a row of its own and takes it back from whatever holds
    // the page while nothing does, until the reader acts (`IssueTree.tsx`'s opening): so the
    // opening is waited out and the reader acts first - a Shift, which no key of the page binds -
    // and every press below starts from the page itself.
    await expectOpeningLanded(page);
    await page.keyboard.press("Shift");
    await focusThePage(page);
    const header = page.getByTestId("issue-header");
    const priority = page.getByRole("combobox", { name: `Priority of ${issue.key}` });
    const status = page.getByRole("combobox", { name: "Status" });

    // p and s reach their own control: two assertions, so a p that focused Status fails.
    await page.keyboard.press("p");
    await expect(priority).toBeFocused();
    await focusThePage(page);
    await page.keyboard.press("s");
    await expect(status).toBeFocused();
    await focusThePage(page);

    // A digit sets P0–P3 through the same write the picker uses, and Shift+P pins through the
    // header's own control. Both are optimistic, so each write is waited for rather than raced,
    // and one reload below carries both: a second one would prove nothing the first does not.
    const patch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await page.keyboard.press("2");
    expect((await patch).postDataJSON()).toEqual({ priority: 2 });
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    await focusThePage(page);

    const pinned = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
    );
    await page.keyboard.press("Shift+P");
    // The pin is the overflow menu's action, pressed while the menu is shut (R13).
    await expect(header.locator('[data-issue-action="pin"]')).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    expect((await pinned).ok()).toBe(true);
    await focusThePage(page);

    await page.reload();
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect(header.locator('[data-issue-action="pin"]')).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    // The new load opens the outline on a row of its own again (decision 399), and the reader
    // acts before the focus leaves for the page, as above.
    await expectOpeningLanded(page);
    await page.keyboard.press("Shift");
    await focusThePage(page);

    // l opens the labels popover in Details' Labels row - the listbox itself, not just its
    // trigger (R13).
    await page.keyboard.press("l");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Search or create label" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toHaveCount(0);
    await focusThePage(page);

    // e edits the title, where every single key is text again: typing "sp0" neither focused
    // Status or the priority nor set P0, and Escape discards the edit.
    await page.keyboard.press("e");
    const titleInput = page.getByLabel("Issue title");
    await expect(titleInput).toBeFocused();
    await page.keyboard.type("sp0");
    await expect(titleInput).toBeFocused();
    await expect(titleInput).toHaveValue(/sp0/);
    await page.keyboard.press("Escape");
    await expect(
      page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Keyboard issue" })
    ).toBeVisible();
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        priority: 2,
        title: "Keyboard issue",
      });
    await focusThePage(page);

    // t is a chord: the indicator shows it waiting, then the second key picks its target. c is a
    // global shortcut on its own (the create dialog), and completing the chord is not it: `t c`
    // is the conversation, the page with every filter off (the key table). The chord's second
    // key must follow within the keymap's 1000 ms window, so nothing slow sits between the two.
    await page.keyboard.press("t");
    const indicator = page.getByTestId("chord-indicator");
    await expect(indicator).toHaveText(/t/);
    await page.keyboard.press("c");
    await expect(indicator).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Create issue" })).toHaveCount(0);
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await expectActivePreset(page, PRESET_NAMES.conversation);
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-conversation-${testInfo.project.name}.png`),
    });

    // Every chord of the key table opens its target's address (R11, R20): a filter's chord
    // turns it on from any other view and off from its own, `t c` turns every filter off, and
    // the rest open the steps, Documents' list, the history and the collaborators.
    for (const [key, address] of [
      ["k", "verification"],
      ["p", "plan"],
      ["e", "steps"],
      ["d", "decisions"],
      ["a", "documents"],
      ["v", "activity"],
      ["o", "collaborators"],
      ["s", "spec"],
    ] as const) {
      await page.keyboard.press("t");
      await page.keyboard.press(key);
      await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/${address}$`));
    }
    // Pressed from its own filter, the chord turns it off.
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await page.keyboard.press("t");
    await page.keyboard.press("p");
    await page.keyboard.press("t");
    await page.keyboard.press("c");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));

    // `t h` is the outline's own chord (part I's `useTreeKeymap.ts`), so it is pressed with the
    // outline drawn - every filter off, as it is now - where the source pressed it from the
    // Decisions filter, which draws a tree of its own.
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/children$`));

    // ? lists the scope as "Issue"; the digits collapse to one 0 – 3 row.
    await page.keyboard.press("?");
    const dialog = page.getByRole("dialog", { name: "Keyboard shortcuts" });
    await expect(dialog).toBeVisible();
    const issueSection = dialog.getByRole("region", { name: "Issue", exact: true });
    await expect(issueSection).toContainText("Set priority P0–P3");
    await expect(issueSection).toContainText("Focus the status");
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Set priority P0–P3" }).locator("kbd")
    ).toHaveText(["0", "3"]);
    // Each chord is listed under what it does on this page, not under a tab the page no longer
    // has (`web/src/features/issue/useIssueKeymap.ts`'s `CHORDS`): the filters by the row's own
    // names, and the panel, the children and Documents' list as the page names them.
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `${PRESET_NAMES.conversation}: turn every filter off` })
        .locator("kbd")
    ).toHaveText(["t", "c"]);
    // `t v` is the activity and `t c` the conversation, so Verification takes `k`.
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `Turn the ${PRESET_NAMES.verification} filter on or off` })
        .locator("kbd")
    ).toHaveText(["t", "k"]);
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `Turn the ${PRESET_NAMES.plan} filter on or off` })
        .locator("kbd")
    ).toHaveText(["t", "p"]);
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: CONTROL_LABELS.open_steps })
        .locator("kbd")
    ).toHaveText(["t", "e"]);
    // The history takes `t v`, since `t a` opens Documents' list and `l` the labels. Its words
    // are the page's own (`useIssueKeymap.ts`): `/activity` opens Details at the issue's history
    // (R15), as the user docs' address table has it.
    await expect(issueSection).toContainText("Open the issue's history in Details");
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: "Open the issue's history in Details" })
        .locator("kbd")
    ).toHaveText(["t", "v"]);
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: "Open the issue's collaborators in Details" })
        .locator("kbd")
    ).toHaveText(["t", "o"]);
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: /\bTree\b/ }),
      "the help still names a Tree"
    ).toHaveCount(0);
    // No row of the help names a tab this page does not have (R21).
    for (const gone of ["Verification tab", "Children tab", "Artifacts tab", "Collaborators tab"]) {
      await expect(
        issueSection.getByRole("listitem").filter({ hasText: gone }),
        `the help still lists "${gone}", which this page has no such thing for`
      ).toHaveCount(0);
    }
    await page.screenshot({
      path: testInfo.outputPath(`issue-shortcut-help-${testInfo.project.name}.png`),
    });
  } finally {
    await context.close();
  }
});

interface OpenedTree {
  readonly context: BrowserContext;
  readonly page: Page;
  readonly seeded: WholeIssueSeed;
}

/** The whole issue seeded, and one signed-in reader's page at 1280 × 900 px that has loaded
 *  nothing yet: for a row whose first load is an address of its own. */
async function seedTreeContext(browser: Browser): Promise<OpenedTree> {
  const seeded = await seedWholeIssue();
  const context = await asReader(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ height: 900, width: 1280 });
  return { context, page, seeded };
}

/** The whole issue, opened by one signed-in reader with the focus on the page itself, which is
 *  where reading mode is. A key pressed before this returns reaches no handler, because the
 *  keymap binds only once sign-in resolves. It asserts only that the page's opening put the focus
 *  on a row of its own and that the focus then left for the page itself (`expectOpeningLanded`,
 *  `focusThePage`): each row below asserts the key or the tree property it is about first, and
 *  `expectTreeDrawn` after it. */
async function openTree(browser: Browser): Promise<OpenedTree> {
  const opened = await seedTreeContext(browser);
  await gotoIssue(opened.page, `/issues/${opened.seeded.key}/conversation`);
  await expectOpeningLanded(opened.page);
  await focusThePage(opened.page);
  return opened;
}

/** The redesigned page and its tree are on the screen, which every row asserts after its own
 *  subject: a page that answers a key but draws no tree fails here. */
async function expectTreeDrawn(page: Page): Promise<void> {
  expect(
    await issuePage(page).count(),
    "the page draws no [data-issue-page] root, so this is not the redesigned issue page"
  ).toBe(1);
  expect(await tree(page).count(), 'the page draws no role="tree" named "Issue tree"').toBe(1);
}

/** The ids of the rows a reader can see, in the order the keys move through them. */
async function visibleIds(page: Page): Promise<string[]> {
  return (await drawnRows(page)).filter((row) => row.visible).map((row) => row.id ?? "<no id>");
}

/** Where the keys have left the reader: the row holding the focus, or the one the tree names with
 *  `aria-activedescendant`. */
async function focusedRow(page: Page): Promise<string | null> {
  // Where the keys left the reader at this instant is what this answers, by the focus or else
  // by the row the tree names: a caller that waits for a row polls this whole function.
  return (await focusedNodeId(page)) ?? (await treeActiveDescendant(page));
}

test("every chord the page bound before the redesign brings a target on screen with the focus in it", async ({
  browser,
}) => {
  const { context, page } = await openTree(browser);
  try {
    // The filter chords of the key table: `t d`, `t k`, `t s` and `t p`, each turning its filter
    // on from the one before it, and `t c`, which turns every filter off. Each leaves the focus
    // in the outline.
    const chords = [
      { preset: PRESET_NAMES.decisions, second: "d" },
      { preset: PRESET_NAMES.verification, second: "k" },
      { preset: PRESET_NAMES.spec, second: "s" },
      { preset: PRESET_NAMES.plan, second: "p" },
      { preset: PRESET_NAMES.conversation, second: "c" },
    ] as const;
    for (const chord of chords) {
      await focusThePage(page);
      await page.keyboard.press("t");
      await expect(page.getByTestId("chord-indicator")).toHaveText(/t/);
      await page.keyboard.press(chord.second);
      await expect
        .poll(() => activePreset(page), {
          message: `t ${chord.second} does not open ${chord.preset}`,
        })
        .toBe(chord.preset);
      await expectTreeDrawn(page);
      // The tree is a column of its own and may be taller than the screen, which no reading of
      // "every pixel on screen" could pass; the page shows the control it leaves the reader on
      // where a part does not fit (`useIssueKeymap.ts`'s `showPart`), and that control is the
      // tree's one tab stop, so the row it lands on is what is read.
      await expect.poll(() => focusedRow(page)).not.toBeNull();
      const landed = await focusedRow(page);
      expect(
        await onScreen(nodeRow(page, landed ?? "").first()),
        `t ${chord.second} does not bring the row it lands on on screen`
      ).toBe(true);
      expect(
        await focusInside(tree(page)),
        `t ${chord.second} does not put the focus in the outline`
      ).toBe(true);
    }

    // The history, in Details: `t v` opens Details at it and puts the focus in it. The panel it
    // is drawn in is a full-height column, taller than the screen by construction, so what the
    // chord has to bring on screen is the history's own head, where it leaves the focus.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("v");
    await expect(historyPanel(page)).toBeVisible();
    await expect
      .poll(() => focusInside(historyPanel(page)), {
        message: "t v does not put the focus in the history",
      })
      .toBe(true);
    expect(
      await onScreen(historyPanel(page).getByRole("heading").first()),
      "t v leaves the history off screen"
    ).toBe(true);
    expect(await focusInside(marginSlot(page)), "t v does not put the focus in the panel").toBe(
      true
    );
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape does not close Details").toBeHidden();

    // The steps, in Details: `t e`, the Steps tab's chord on today's page (answer A),
    // opens Details at the issue's steps under the history and puts the focus on their heading,
    // which it brings to the top of Details' view.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("e");
    await expect(stepsSection(page)).toBeVisible();
    await expect
      .poll(() => focusInside(stepsSection(page)), {
        message: "t e does not put the focus in the steps",
      })
      .toBe(true);
    expect(
      await onScreen(stepsSection(page).getByRole("heading", { level: 2 })),
      "t e leaves the steps off screen"
    ).toBe(true);
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape does not close Details").toBeHidden();

    // The collaborators, in Details: `t o`. Details is a layer over the page, the identity block
    // included, with a scroll box of its own, so the chord scrolls that box to the row and leaves
    // the page under it where the reader had it.
    await focusThePage(page);
    await page.evaluate(() => window.scrollTo(0, 300));
    const pageTop = await page.evaluate(() => window.scrollY);
    expect(pageTop, "the seeded page is too short to be scrolled under Details").toBeGreaterThan(0);
    await page.keyboard.press("t");
    await page.keyboard.press("o");
    const collaborators = marginSlot(page).locator('[data-details-row="collaborators"]');
    await expect
      .poll(() => focusInside(collaborators), { message: "t o does not put the focus on them" })
      .toBe(true);
    expect(await onScreen(collaborators), "t o leaves the collaborators off screen").toBe(true);
    expect(
      await page.evaluate(() => window.scrollY),
      "t o scrolls the page under Details, which cannot move the row"
    ).toBe(pageTop);
    // Details is the layer over the page, which Escape closes before the page's own Escape
    // does anything else; the chords below are read on the page under it.
    await page.keyboard.press("Escape");
    await expect(marginShell(page), "Escape does not close Details").toBeHidden();

    // The children's group of rows in the outline: `t h` puts the focus on its first row
    // (the architect's ruling 5 on R12).
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    await expect
      .poll(
        () =>
          childRows(page)
            .first()
            .evaluate((row) => row === document.activeElement),
        {
          message: "t h does not put the focus on the first row of the children's group",
        }
      )
      .toBe(true);
    expect(await onScreen(childRows(page).first()), "t h leaves the children off screen").toBe(
      true
    );

    // The children's address takes the focus back from whatever holds the page while nothing
    // does, until the reader acts (`IssueTree.tsx`'s landing), so the reader acts first - a
    // Shift, which no key of the page binds - and the focus then leaves for the page for good.
    await page.keyboard.press("Shift");

    // Documents' list: `t a`, the Artifacts tab's chord on today's page, opens it with the
    // focus on its first row, on screen (part N's R4).
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("a");
    await expect(documentsList(page), "t a opens no Documents' list").toBeVisible();
    await expect(
      firstDocumentLink(page),
      "t a does not put the focus on Documents' first row"
    ).toBeFocused();
    expect(
      await onScreen(firstDocumentLink(page)),
      "t a leaves Documents' first row off screen"
    ).toBe(true);
    await expectActiveControl(page, "documents");
    const firstName = await firstDocumentLink(page).innerText();

    // From the row `t a` left the focus on, Enter opens its document in place of the list and of
    // the outline, and Escape - the key table's step after leaving a caret - closes it, back to
    // the list with the focus on that row (part N's R2); Escape on the list then draws the
    // outline again.
    await page.keyboard.press("Enter");
    await expect(
      openedArtifact(page),
      "Enter on Documents' first row opens no document"
    ).toHaveCount(1);
    expect(
      await tree(page).count(),
      "the opened document is not drawn in place of the outline"
    ).toBe(0);
    await page.keyboard.press("Escape");
    await expect(documentsList(page), "Escape does not close the opened document").toBeVisible();
    expect(await openedArtifact(page).count(), "Escape leaves the document drawn").toBe(0);
    await expect(
      documentLink(page, firstName),
      "Escape leaves the focus off the document's row"
    ).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(
      tree(page),
      "Escape on Documents' list does not draw the outline again"
    ).toBeVisible();
  } finally {
    await context.close();
  }
});

test("Escape leaves the caret, then closes a document, then clears the words, then turns every filter off, then leaves the outline", async ({
  browser,
}) => {
  const { context, page, seeded } = await seedTreeContext(browser);
  try {
    const spec = await getArtifact(seeded.specArtifactId);
    const words = "Requirement";
    await gotoIssue(page, `/issues/${seeded.key}/spec?q=${words}`);
    await expectActivePreset(page, PRESET_NAMES.spec);
    await expect(filterBox(page)).toHaveValue(words);

    // A document opened from a chip of the Spec view, with the caret in its text: the caret is
    // left first, and the next Escape closes the document, back to the view and the words the
    // chip was pressed under (part N's R2: a chip's document goes back to the chip).
    const chip = issuePage(page).locator(
      `a[data-row-chip][data-row-reference="dispatch://${seeded.key}/artifact/${seeded.documents.cited}"]`
    );
    await expect(chip, "the Spec view draws no chip for the cited document").toHaveCount(1);
    await chip.click();
    await expect(page).toHaveURL(
      new RegExp(`/issues/${seeded.key}/artifacts/${seeded.documents.cited}$`)
    );
    await expect(openedArtifact(page)).toHaveCount(1);
    const editor = openedArtifact(page).getByRole("textbox", { name: "Document editor" });
    await editor.click();
    await expect
      .poll(() => editor.evaluate((box) => box.contains(document.activeElement)))
      .toBe(true);
    await page.keyboard.press("Escape");
    await expect
      .poll(() => editor.evaluate((box) => box.contains(document.activeElement)))
      .toBe(false);
    await expect(openedArtifact(page), "Escape closed the document with the caret").toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(openedArtifact(page), "Escape does not close the document").toHaveCount(0);
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/spec\\?q=${words}$`));

    // The caret step of the key table is read in the outline, with the words still on: part J's
    // Spec view draws each statement and edits none - Enter on a row of it goes to that node in
    // the conversation (`spec-filter/SpecFilterBody.tsx`) - and the outline is where this page
    // puts a caret in a row (part I's `useTreeKeymap.ts`). What the step proves is the order:
    // Escape takes the caret before it clears the words.
    await gotoIssue(page, `/issues/${seeded.key}?q=${words}`);
    await expectOpeningLanded(page);
    const outlineRow = nodeRow(page, blockNodeId(spec.slug, requirementBlockId(2))).first();
    await outlineRow.focus();
    const caretRow = await focusedRow(page);
    expect(caretRow, "the words draw no row to stand on").not.toBeNull();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the row" })
      .toBe(true);
    await page.keyboard.press("Escape");
    expect(await caretInNode(page), "Escape did not leave the caret").toBe(false);
    expect(await focusedRow(page)).toBe(caretRow);

    // Back to the view with the words on, for the three steps below, standing on a row of it.
    await gotoIssue(page, `/issues/${seeded.key}/spec?q=${words}`);
    await expectActivePreset(page, PRESET_NAMES.spec);
    const row = nodeRow(page, blockNodeId(spec.slug, requirementBlockId(2))).first();
    await row.focus();
    const on = await focusedRow(page);
    expect(on, "the filter draws no row to stand on").not.toBeNull();

    // Then the words: the filter stays on, and the reader stays in the outline.
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/spec$`));
    // The box shuts into its Filter control as its words go (part N's R4: it is drawn while it
    // holds words or has the caret), where the source's box stayed drawn and empty.
    await expect(filterBox(page), "the words box is drawn with no words in it").toHaveCount(0);
    await expect(filterControl(page)).toBeVisible();
    await expectActivePreset(page, PRESET_NAMES.spec);

    // Then the filter: every filter off, with the reader on the same node of the outline, at
    // Conversation's own address (`escape-order.ts`): the issue's own draws Dashboard
    // (dispatch://INFRA-5702).
    await page.keyboard.press("Escape");
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/conversation$`));
    await expectActivePreset(page, PRESET_NAMES.conversation);
    await expect.poll(() => focusedRow(page), { message: "the reader left the outline" }).toBe(on);

    // Then the outline itself.
    await page.keyboard.press("Escape");
    await expect
      .poll(() => focusedRow(page), { message: "Escape did not leave the outline" })
      .toBe(null);
    await expect(page).toHaveURL(new RegExp(`/issues/${seeded.key}/conversation$`));
  } finally {
    await context.close();
  }
});

test("t f filters inside the preset, $mod+Alt+m comments on a selection, and / stays the global search", async ({
  browser,
}) => {
  const { context, page, seeded } = await openTree(browser);
  try {
    // t f filters inside the preset: the filter takes the focus and narrows the rows.
    const before = await visibleIds(page);
    await page.keyboard.press("t");
    await page.keyboard.press("f");
    expect(await filterBox(page).count(), "t f opens no filter").toBe(1);
    await expectTreeDrawn(page);
    expect(await focusInside(filterBox(page)), "t f does not put the focus in the filter").toBe(
      true
    );
    await page.keyboard.type("Requirement 2");
    await expect.poll(async () => (await visibleIds(page)).length).toBeLessThan(before.length);
    const narrowed = (await drawnRows(page)).filter((row) => row.visible && !row.context);
    expect(
      narrowed.filter((row) => !row.text.includes("Requirement 2")).map((row) => row.id),
      "the filter kept rows that do not match what was typed"
    ).toEqual([]);
    // A filter that hides every row meets that, and meets the count above too, so what is left
    // is asserted as well: the row the words name is still drawn.
    expect(
      narrowed.map((row) => row.id),
      "the filter hid every row of the tree, matches included"
    ).not.toEqual([]);

    // `/` is the application's global search, which this page does not shadow.
    await page.keyboard.press("Escape");
    await focusThePage(page);
    await page.keyboard.press("/");
    await expect(page.getByRole("dialog", { name: /search/i })).toBeVisible();
    await page.keyboard.press("Escape");

    // $mod+Alt+m still comments on a selection inside a node's own text, which the editor takes
    // the place of in the row's words (`NodeRow.tsx`'s stage).
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const requirement = nodeRow(page, blockNodeId(specSlug, requirementBlockId(2))).first();
    await requirement.focus();
    await page.keyboard.press("Enter");
    await expect
      .poll(() => caretInNode(page), { message: "Enter did not put the caret in the node" })
      .toBe(true);
    await selectTextIn(page, rowWords(requirement), "requirement");
    await page.keyboard.press("ControlOrMeta+Alt+m");
    // What the chord does with part L's box on the page: it marks the selection as a comment in
    // the row's own editor, which is this page's proof of R30 of the root spec. The box at the
    // foot is aimed only by a Reply or an Edit (`IssueComposer.tsx`), never by the editor: it
    // stays as it was, drawn and writing on the issue, with no target line and the issue's own
    // placeholder, and the chord puts no caret in it.
    await expect(
      rowWords(requirement).locator(".mark-comment"),
      "$mod+Alt+m marked no selection in the row's editor"
    ).toHaveCount(1);
    await expect(composer(page), "the box at the foot is not drawn after the chord").toBeVisible();
    await expect(
      composerTarget(page),
      "$mod+Alt+m aimed the box at the foot at the selection"
    ).toHaveCount(0);
    await expect(
      composerField(page),
      "the box at the foot no longer writes on the issue after the chord"
    ).toHaveAttribute("placeholder", CONTROL_LABELS.issue_placeholder);
    await expect(
      composerField(page),
      "$mod+Alt+m put the caret in the box at the foot"
    ).not.toBeFocused();
  } finally {
    await context.close();
  }
});

/**
 * Part L's `seedHistoryTurns` issue, which holds a comment of the reader's own, with a message the
 * Legion daemon of the issue's project writes with no target: it begins a thread the history holds
 * whole, whoever writes in it (`agents-only.ts`). The seed's own `daemon` writes as the session
 * `legion-daemon`, which is no project's daemon (`legion-daemon:<PROJECT>`), so the history holds
 * no turn of it.
 */
async function seedDaemonTurnAndOwnComment() {
  const seeded = await seedHistoryTurns();
  const words = "The daemon moved the review to its next round.";
  const message = await createMessage(
    seeded.key,
    { body: words },
    daemonOf(seeded.key.slice(0, seeded.key.indexOf("-")))
  );
  return { ...seeded, legion: { id: message.id, words } };
}

// R10 of dispatch://INFRA-3940 (c-carried; new, for part M): a reply from the history and an edit
// from a node's facts go through part L's box. The Reply of a turn of the thread the Legion daemon
// began, in Details' history (`history-entries.tsx`), aims the box at that turn, and what the box
// sends is Dispatch's reply to the turn, drawn in the history under it. The Edit of Details' facts
// of the row of a comment the reader wrote (`FocusedNodeFacts.tsx`) opens the comment's words in
// the box, and the words the box saves are the comment's new body in Dispatch and in its row.
// SLOW(>=2s): a project, an issue and four posts written, two loads of the issue with Details open,
// and a reply and an edit sent from the box, each read back from Dispatch and from the page.
test("a reply from the history and an edit from a node's facts go through the box", async ({
  browser,
}) => {
  // Not measured in a browser yet: two loads, at the 1.7 s a load that "the filter row is one
  // line at every width" measured (27 loads in 47.0 s), Details opened after each, and two writes
  // from the box each read back from Dispatch; the bound is several times that, for a slow leg.
  test.setTimeout(90_000);
  await phase(browser, seedDaemonTurnAndOwnComment, async (page, seeded) => {
    // A reply from the history: the Reply of the daemon's turn aims the box at the turn.
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await openDetails(page);
    const turn = historyItem(page, seeded.legion.id);
    await expect(
      turn,
      "the history draws no turn of the thread the Legion daemon began"
    ).toContainText(seeded.legion.words);
    await turn.getByRole("button", { exact: true, name: CONTROL_LABELS.reply }).click();
    await expect(
      composerTarget(page),
      "the Reply of the daemon's turn in the history draws no target line in the box"
    ).toContainText(CONTROL_LABELS.replying_to);
    const replyWords = "A reply to the daemon's turn, sent from the box.";
    await sendFromComposer(page, replyWords);
    await expect
      .poll(
        async () =>
          ((await getMessage(seeded.key, seeded.legion.id)).replies ?? [])
            .filter((one) => one.body === replyWords)
            .map((one) => [one.in_reply_to, one.author.kind, one.author.id]),
        {
          message:
            "Dispatch holds the box's words as no reply of the reader's to the daemon's turn",
        }
      )
      .toEqual([[seeded.legion.id, "user", READER]]);
    const sent = ((await getMessage(seeded.key, seeded.legion.id)).replies ?? []).find(
      (one) => one.body === replyWords
    );
    if (sent === undefined) throw new Error("Dispatch holds no reply of the box's words");
    // The thread's replies stand in its list under its first turn (`history-entries.tsx`'s
    // `Entry`).
    const drawn = historyPanel(page)
      .locator(`[data-history-entry="thread:${seeded.legion.id}"]`)
      .locator(`ol [data-history-item="${sent.id}"]`);
    await expect(
      drawn,
      "the history does not draw the reply under the daemon's turn"
    ).toBeVisible();
    await expect(
      drawn,
      "the reply the history draws under the daemon's turn is not the box's words"
    ).toContainText(replyWords);

    // An edit from a node's facts: the Edit of the row of the reader's own comment opens its words
    // in the box, and the box saves the words it is given in their place.
    const facts = await rowFacts(page, seeded.key, seeded.own.id);
    await facts
      .locator("[data-focused-controls]")
      .getByRole("button", { exact: true, name: CONTROL_LABELS.edit })
      .click();
    await expect(
      composerTarget(page),
      "the Edit of the facts of the reader's comment draws no editing line in the box"
    ).toContainText(CONTROL_LABELS.editing);
    await expect(
      composerField(page),
      "the box does not hold the words of the comment whose Edit was pressed"
    ).toHaveValue(seeded.own.words);
    const edited = "The reader's comment, as its writer rewrote it in the box.";
    await sendFromComposer(page, edited);
    await expect
      .poll(
        async () =>
          (await listComments(seeded.key))
            .filter((one) => one.body === edited)
            .map((one) => one.id),
        { message: "Dispatch does not hold the saved words as the reader's comment's body alone" }
      )
      .toEqual([seeded.own.id]);
    await expect(
      rowWords(nodeRow(page, seeded.own.id).first()),
      "the row of the reader's comment does not draw its new words"
    ).toHaveText(edited);
  });
});

// ---- from 750e2716:packages/dispatch/e2e/live-socket.e2e.ts

const bob = { actor: { id: "bob", kind: "session" as const }, as: "agent" as const };

/** Waits until the page holds one open live socket and its event feed has started on it. */
async function untilFeedStarts(sockets: LiveSockets): Promise<void> {
  await expect.poll(() => sockets.open.size).toBe(1);
  await expect.poll(() => sockets.received.includes("events_start")).toBe(true);
}

function trackApiRequests(page: Page): Set<Request> {
  const pending = new Set<Request>();
  page.on("request", (request) => {
    if (new URL(request.url()).pathname.startsWith("/api/")) pending.add(request);
  });
  const settled = (request: Request) => pending.delete(request);
  page.on("requestfailed", settled);
  page.on("requestfinished", settled);
  return pending;
}

async function waitForReads(pending: ReadonlySet<Request>): Promise<void> {
  await expect.poll(() => pending.size).toBe(0);
}

/** What a recorded read path carries in place of the issue's key, so that two pages, each
 *  reading an issue of its own, compare as lists. */
const ISSUE_PATH = "/api/v1/issues/<issue>";

/** The reads the issue page makes for itself at its first load. A window that counts
 *  what an event costs a page opens only after these have gone out, since they are the page's
 *  own lazy first reads and not the event's doing. */
const FIRST_ISSUE_READS = [`${ISSUE_PATH}/comments`, `${ISSUE_PATH}/asks`, `${ISSUE_PATH}/events`];

/** Every read the page makes of the one issue `key` names, in order, as the request's pathname
 *  with that key replaced by `<issue>`. Reads of anything else are left out: the agent list, the
 *  inbox and the reader's own state are not what a comment on this issue invalidates, so a
 *  periodic one of those landing inside one page's window and outside another's would make two
 *  equal pages count differently. The query string is dropped because it names the page's own
 *  issue's rows - an incremental event read asks for the ids it is missing - which differ
 *  between two pages by construction and would read as different reads. */
function trackIssueReads(page: Page, key: string): string[] {
  const reads: string[] = [];
  const prefix = `/api/v1/issues/${key}`;
  page.on("request", (request) => {
    const { pathname } = new URL(request.url());
    if (pathname === prefix || pathname.startsWith(`${prefix}/`)) {
      reads.push(`${ISSUE_PATH}${pathname.slice(prefix.length)}`);
    }
  });
  return reads;
}

/** Leaves the page in the state a counting window needs: its own first reads of the issue have
 *  gone out, and nothing it started is still in flight. */
async function untilIssueReadsSettle(
  reads: readonly string[],
  pending: ReadonlySet<Request>
): Promise<void> {
  for (const read of FIRST_ISSUE_READS) await expect.poll(() => reads).toContain(read);
  await waitForReads(pending);
}

/** Every document the page fetched for itself, in order: its first load and every reload after
 *  it. The `load` event is not that count - Playwright's Firefox fires one for the page's initial
 *  `about:blank`, Chromium none - while a reload is a navigation request of the main frame in
 *  every engine. */
function trackDocumentLoads(page: Page): string[] {
  const loaded: string[] = [];
  page.on("request", (request) => {
    if (request.isNavigationRequest() && request.frame() === page.mainFrame()) {
      loaded.push(request.url());
    }
  });
  return loaded;
}

// The page rotates before it has seen any event, so its event feed resumes from the sign-in
// check's head: the change committed while the next socket waits arrives on it, and the
// rotation costs no read of the issue beyond what that change costs a page with no rotation
// (the control). Reads of the issue are what the title claims and all this row counts; a
// request to any other route is outside both.
// Each window counts that page's reads of its own issue, by path: a count of every `/api/`
// request counts two different things, since the control's window holds its own lazy first reads
// of `/comments` and `/asks?state=all` and a periodic `/api/v1/agents`, which no comment event
// invalidates, while the rotating page made those reads long before its own window opened. So
// both windows open only once that page's own first reads have gone out and settled, and the two
// lists are compared read for read.
// SLOW(>=2s): the two quiet windows below, one on the control page and one on the rotating
// page, are what "no extra read of the issue" is counted over. A count of what a page did not
// do over a window is that window's own real time, and the two must stay equal for the counts
// to compare.
// Both tags in the title are what select this row: `@phone` puts it in `iphone`, and `@firefox`
// in `firefox`, which takes rows by title tag rather than by file name since
// cybermonklabs/legion#2866. The rotation's faults this row reads were seen in exactly those
// two projects, so the row keeps both.
test("a rotation loses no event and makes no extra read of the issue @phone @firefox", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const baselineIssue = await createIssue({ project: "CORE", title: "Comment request control" });

  const alice = await asReader(browser, "alice");
  try {
    const control = await alice.newPage();
    const controlPending = trackApiRequests(control);
    const controlReads = trackIssueReads(control, baselineIssue.key);
    const controlSocket = control.waitForEvent("websocket", isLiveSocket);
    await control.goto(`/issues/${baselineIssue.key}/activity`);
    // The title is drawn as the page's heading and again as a fact of the focused node
    // (dispatch://INFRA-1196 R12): the page has loaded when its heading says it.
    await expect(
      control.getByRole("heading", { exact: true, name: "Comment request control" })
    ).toBeVisible();
    await controlSocket;
    // The rotating page's window opens long after its first reads; this leaves the control page
    // in that same state, so that neither window holds a read the page would have made anyway.
    await untilIssueReadsSettle(controlReads, controlPending);

    const controlWindow = controlReads.length;
    const controlComment = await createComment(
      baselineIssue.key,
      { body: "Control comment request count" },
      bob
    );
    await expect(
      control.locator(`a[href="/issues/${baselineIssue.key}/comments/${controlComment.id}"]`)
    ).toBeVisible();
    // A read an event invalidates goes out in the frame after that event, so a window that
    // starts once the comment's link is on screen is a settle rather than a guess.
    await control.waitForTimeout(1_500);
    const expectedReads = controlReads.slice(controlWindow).sort();
    await control.close();

    const issue = await createIssue({ project: "CORE", title: "Rotation target" });
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    const reads = trackIssueReads(page, issue.key);
    const sockets = watchLiveSockets(page);
    let holdReconnect = false;
    let held: WebSocketRoute | undefined;
    const reconnectReleased = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      if (holdReconnect) {
        held = route;
        await reconnectReleased.promise;
        connect(route);
        return;
      }
      connect(route);
    });
    const first = page.waitForEvent("websocket", isLiveSocket);
    await page.goto(`/issues/${issue.key}/activity`);
    await expect(page.getByRole("heading", { exact: true, name: "Rotation target" })).toBeVisible();
    await expect(historyPanel(page)).toBeVisible();
    await first;
    await expect.poll(() => sockets.received.includes("events_start")).toBe(true);
    await untilIssueReadsSettle(reads, pending);

    expect(sockets.received).not.toContain("event");
    holdReconnect = true;
    await rotateLiveSockets();
    await expect.poll(() => held !== undefined).toBe(true);
    const comment = await createComment(issue.key, { body: "Committed while rotation waits" }, bob);
    await expect(page.getByTestId("connection-pill")).toHaveCount(0);

    const rotationWindow = reads.length;
    reconnectReleased.resolve();
    await expect.poll(() => sockets.received).toContain("event");
    await expect(
      page.locator(`a[href="/issues/${issue.key}/comments/${comment.id}"]`)
    ).toBeVisible();
    await expect(page.getByTestId("connection-pill")).toHaveCount(0);
    // The control's window, to the millisecond: the two lists are of equal quiet.
    await page.waitForTimeout(1_500);
    // Read for read, sorted, so a failure names the read that differs rather than a count.
    expect(reads.slice(rotationWindow).sort()).toEqual(expectedReads);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): a cold load in WebKit and in Firefox with the page's first live socket held open
// through it, which each engine then has to replay the missed change on.
// Off the pull-request lane: `main`'s source row in `live-socket.e2e.ts` runs there, and the
// lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
test("a change committed while the page loads is shown without a reload", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Before the page load" });

  const alice = await asReader(browser, "alice");
  try {
    const page = await alice.newPage();
    const pending = trackApiRequests(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    const loaded = trackDocumentLoads(page);
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      held = route;
      await firstSocketReleased.promise;
      connect(route);
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    // The title is the page's heading; the focused node's facts draw it a second time.
    await expect(
      page.getByRole("heading", { exact: true, name: "Before the page load" })
    ).toBeVisible();
    await expect.poll(() => held !== undefined).toBe(true);
    await waitForReads(pending);

    await patchIssue(issue.key, { title: "Committed during the page load" }, bob);
    const opened = page.waitForEvent("websocket", isLiveSocket);
    firstSocketReleased.resolve();
    await opened;

    await expect(
      page.getByRole("heading", { exact: true, name: "Committed during the page load" })
    ).toBeVisible({ timeout: 5_000 });
    expect(loaded).toHaveLength(1);
  } finally {
    await alice.close();
  }
});

test("a first live socket opening without a change makes no API request", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Quiet page load" });
  // The reader has read this issue through its head already. The outline's end is on screen the
  // moment an empty issue loads, and a second there writes an unread issue's read mark
  // (`IssueReadMark.tsx`): a change of the reader's state, whose own event the opening socket
  // then delivers and the page reads the state back for. That is the reader reading, not the
  // socket's open, so the seed leaves the mark nothing to write, and the open is the one thing
  // left that could cause a request.
  const [head] = await getIssueEvents(issue.key, { limit: 1, order: "desc" });
  await putIssueState(issue.key, { last_read_seq: head?.seq ?? 0 }, { login: "alice" });

  const alice = await asReader(browser, "alice");
  try {
    const page = await alice.newPage();
    const traffic = watchApiRequests(page);
    const sockets = watchLiveSockets(page);
    let held: WebSocketRoute | undefined;
    const firstSocketReleased = Promise.withResolvers<void>();
    await page.routeWebSocket(/\/ws\/live$/u, async (route) => {
      held = route;
      await firstSocketReleased.promise;
      connect(route);
    });
    // Nothing writes the reader's state on this load: a mark written here would be a change.
    const stateWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PUT" &&
        new URL(request.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
      ) {
        stateWrites.push(request.postData() ?? "");
      }
    });

    await page.goto(`/issues/${issue.key}/conversation`);
    // The title is the page's heading; the focused node's facts draw it a second time.
    await expect(page.getByRole("heading", { exact: true, name: "Quiet page load" })).toBeVisible();
    await expect.poll(() => held !== undefined).toBe(true);
    // The load is over once nothing has been in flight for the recorded baseline's window, not at
    // the first moment nothing is: the outline's one batched read of the names its references
    // cite goes out only after each of the tree's reads has answered (`useIssueTree.ts`), so the
    // reads can all be in for a moment before it is sent, and a count started then would take it
    // for the open's own (`reference-targets?ref=dispatch://CORE-1`).
    await expectLoadSettled(traffic);
    traffic.reset();
    firstSocketReleased.resolve();
    // The socket opened and the feed started on it, so the quiet below is the open's own.
    await untilFeedStarts(sockets);
    // One second of real quiet. A read the open itself causes goes out in the same tick as the
    // `events_start` awaited above, so this window is many frames past it. The page's clock
    // cannot stand in for it: a jump long enough to be worth making also fires the shell's own
    // credential poll (15 s, `features/credentials/pending.ts`), a read on a cadence of its own
    // that this row does not count.
    await page.waitForTimeout(1_000);
    expect(stateWrites, "the load wrote the reader's state of an issue they had read").toEqual([]);
    expect(
      traffic.requests.map(({ method, path }) => `${method} ${path}`),
      "the first live socket's open sent requests under /api/"
    ).toEqual([]);
  } finally {
    await alice.close();
  }
});

// ---- from 750e2716:packages/dispatch/e2e/live.e2e.ts

/** A person writing on the issue, whose message is a node of the outline (dispatch://INFRA-1196
 *  R1): an agent's message no person took part in is the history's, in Details (R15). */
const carol = { login: "carol" };

// ---- from 750e2716:packages/dispatch/e2e/phone-landing.e2e.ts

const session = {
  actor: { kind: "session" as const, id: "e2e-phone-landing" },
  as: "agent" as const,
};

// The panel's list of the conversation, which these rows held to opening on its newest message
// whichever of its reads answered last (INFRA-303), is gone (dispatch://INFRA-1196 R14): the
// conversation is the outline, read from the same three reads, and what a phone reader must
// have on the first screen of it is the question that waits on them (R6), clear of the line to
// write in at the foot (R16). These rows hold that landing to the same three orders and the same
// failure and absence the list's rows held its own landing to. The issue's own read carries its
// open questions (`tree-drafts.ts`'s `addAsks`), so a question waiting on the reader is drawn
// even while the asks read has failed or not answered, and the reader lands on it then too.

/** Seeds project CORE and an issue titled `title` whose conversation holds an open question
 *  waiting on its person, a root comment a person wrote, and nine messages an agent wrote. */
async function seedConversation(title: string) {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title });
  const question = await createAsk(
    issue.key,
    { question: "Which landing should the reader get?" },
    session
  );
  await createComment(issue.key, { body: "A root comment under the ask" });
  for (let index = 0; index < 9; index += 1) {
    await createMessage(issue.key, { body: `Agent message ${index}` }, session);
  }
  return { issue, question };
}

/** The three reads the outline's conversation is built from, under the keys the page reads them
 *  by: the asks, the comments, the messages. */
const READS = ["asks", "comments", "messages"] as const;

/** Matches the outline's read `read` for `issueKey`. */
function conversationRead(issueKey: string, read: (typeof READS)[number]): (url: URL) => boolean {
  return (url) => url.pathname === `/api/v1/issues/${issueKey}/${read}`;
}

/** The question's row in the outline. */
function questionRow(page: Page, askId: string): Locator {
  return nodeRow(page, askId).first();
}

/** A new page in `context` at the iPhone 13 viewport INFRA-95 measured the landing fault at
 *  (`IPHONE_13`), below the 390x844 `phone` layout `phone-conversation.e2e.ts` uses. */
async function openPhonePage(context: BrowserContext): Promise<Page> {
  const page = await context.newPage();
  await page.setViewportSize(IPHONE_13);
  return page;
}

/** Waits until `row` has finished rendering: visible, then its rect unchanged over 10 consecutive
 *  frames, sampled in the page. It only samples: nothing here scrolls, focuses, taps or hovers,
 *  so where the page put the row is where it is measured. */
async function settle(row: Locator, label: string): Promise<void> {
  await expect(row, `${label}: the question's row`).toBeVisible({ timeout: 20_000 });
  // The page function waits in the page until the row holds still or its deadline passes, so
  // the answer is that wait's own verdict.
  const never = await row.evaluate((element) => {
    const { promise, resolve } = Promise.withResolvers<string | null>();
    const deadline = Date.now() + 20_000;
    let last = "";
    let unchanged = 0;
    const sample = () => {
      const rect = JSON.stringify(element.getBoundingClientRect());
      unchanged = rect === last ? unchanged + 1 : 0;
      last = rect;
      if (unchanged >= 10) resolve(null);
      else if (Date.now() > deadline) resolve(last);
      else requestAnimationFrame(sample);
    };
    sample();
    return promise;
  });
  if (never !== null) {
    throw new Error(`${label}: the question's row never held still within 20 s (last ${never})`);
  }
}

/** One-shot: the question's row starts inside the first screen and ends at or above the top of
 *  the box at the foot of the screen, part L's line to write in (`IssueComposer.tsx`), so the box
 *  covers none of it: its source's measure against that line, read as L's `clearOfBox` reads a
 *  row against the box, to the top of its `section` as it is drawn now. */
async function expectQuestionInView(page: Page, row: Locator, label: string): Promise<void> {
  // Every caller settled the row first, visible and still, so its card is drawn and has a box.
  const box = await row.locator("[data-node-card]").first().boundingBox();
  // Every caller stands on Conversation's address, where the box at the foot is drawn at rest
  // (`useComposerShown`), so it has a box once the row has one.
  const foot = await composer(page).boundingBox();
  const view = await page.evaluate(() => ({ height: window.innerHeight, scrollY: window.scrollY }));
  if (box === null) throw new Error(`${label}: expected the question's row to have a box`);
  if (foot === null) throw new Error(`${label}: expected the box at the foot to have a box`);
  const where = `the question at ${JSON.stringify(box)}, the box at the foot at ${JSON.stringify(foot)}, the viewport ${view.height} tall at window.scrollY ${view.scrollY}`;
  expect(
    box.y >= 0 && box.y < view.height,
    `${label}: the question is off the first screen: ${where}`
  ).toBe(true);
  expect(
    box.y + box.height <= foot.y + 1,
    `${label}: the question ends under the top of the box at the foot: ${where}`
  ).toBe(true);
}

/** Opens the conversation on a fresh page with `last` held until the other two reads have
 *  answered and the page has had time to draw what they brought, then lets `last` answer. */
async function openWithReadLast(
  context: BrowserContext,
  issueKey: string,
  last: (typeof READS)[number]
): Promise<Page> {
  const page = await openPhonePage(context);
  const held: Route[] = [];
  let released = false;
  const heldRead = conversationRead(issueKey, last);
  const hold = async (route: Route) => {
    if (released) await route.continue();
    else held.push(route);
  };
  await page.route(heldRead, hold);
  const answered = READS.filter((read) => read !== last).map((read) =>
    page.waitForEvent("requestfinished", {
      predicate: (request) => conversationRead(issueKey, read)(new URL(request.url())),
    })
  );
  await page.goto(`/issues/${issueKey}/conversation`);
  await Promise.all(answered);
  await expect.poll(() => held.length).toBeGreaterThanOrEqual(1);
  // No in-page signal says the page has drawn the partial data, so this fixed margin - two
  // macrotasks, two frames and 300 ms - is the defended choice.
  await page.evaluate(async () => {
    for (let i = 0; i < 2; i++) {
      const turn = Promise.withResolvers<void>();
      setTimeout(turn.resolve, 0);
      await turn.promise;
    }
    for (let i = 0; i < 2; i++) {
      const frame = Promise.withResolvers<void>();
      requestAnimationFrame(() => frame.resolve());
      await frame.promise;
    }
    const margin = Promise.withResolvers<void>();
    setTimeout(margin.resolve, 300);
    await margin.promise;
  });
  // The held reads answer before the handler goes, which would otherwise hand them on itself.
  released = true;
  for (const route of held) await route.continue();
  await page.unroute(heldRead, hold);
  return page;
}

// SLOW(>=2s): three opens, each a fresh page, each holding the 300 ms margin the partial-data
// state has no in-page signal for.
// Off the pull-request lane: `main`'s source row in `phone-landing.e2e.ts` runs there, and the
// lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
test("on a phone, the conversation opens with its open question in view whichever read answers last @phone", async ({
  browser,
}) => {
  const { issue, question } = await seedConversation("Landing on the open question");
  const alice = await asReader(browser, "alice");

  try {
    let page: Page | undefined;
    for (const last of READS) {
      const label = `${last} answering last`;
      await page?.close();
      page = await openWithReadLast(alice, issue.key, last);
      await settle(questionRow(page, question.id), label);
      await expectQuestionInView(page, questionRow(page, question.id), label);
    }
  } finally {
    await alice.close();
  }
});

// A failed asks read leaves the question the issue's own read carries, and the reader lands on it;
// a refetch that then brings the asks keeps them there. The refetch is TanStack Query's own on
// focus: the `visibilitychange` this row dispatches on `document` bubbles to `window`, where its
// focusManager listens, and a read in error holds no data, so it is fetched again.
test("on a phone, a conversation whose asks read failed opens on its open question, and a refetch keeps it there @phone", async ({
  browser,
}) => {
  const { issue, question } = await seedConversation("Landing after a failed read");
  const alice = await asReader(browser, "alice");

  try {
    const page = await openPhonePage(alice);
    const asksRead = conversationRead(issue.key, "asks");
    let failing = true;
    let failed = 0;
    await page.route(asksRead, async (route) => {
      if (!failing) {
        await route.continue();
        return;
      }
      failed += 1;
      await route.fulfill({
        body: JSON.stringify({ code: "SERVER_ERROR", error: "boom" }),
        contentType: "application/json",
        status: 500,
      });
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect(tree(page)).toBeVisible();
    // The app's own retries of the asks read fail too before the page settles on its failure.
    await expect.poll(() => failed, { timeout: 15_000 }).toBeGreaterThanOrEqual(3);
    await settle(questionRow(page, question.id), "with the asks read failed");
    await expectQuestionInView(page, questionRow(page, question.id), "with the asks read failed");

    failing = false;
    const answered = page.waitForResponse(
      (response) => asksRead(new URL(response.url())) && response.status() === 200
    );
    await page.evaluate(() =>
      document.dispatchEvent(new Event("visibilitychange", { bubbles: true }))
    );
    await answered;
    await settle(questionRow(page, question.id), "after the refetch");
    await expectQuestionInView(page, questionRow(page, question.id), "after the refetch");
  } finally {
    await alice.close();
  }
});

// INFRA-303: a reader who leaves the conversation while it is still being read was never in it,
// so coming back is a first open, not a return to a place: they land on the question, which they
// had on the first screen before the asks read answered too.
test("on a phone, a reader who leaves the conversation while it loads lands on its open question when they come back @phone", async ({
  browser,
}) => {
  const { issue, question } = await seedConversation("Leaving while it loads");
  const alice = await asReader(browser, "alice");

  try {
    const page = await openPhonePage(alice);
    const asksRead = conversationRead(issue.key, "asks");
    const held: Route[] = [];
    await page.route(asksRead, (route) => {
      held.push(route);
    });
    await page.goto(`/issues/${issue.key}/conversation`);
    await expect.poll(() => held.length).toBeGreaterThanOrEqual(1);
    await settle(questionRow(page, question.id), "before the asks read answers");
    await expectQuestionInView(
      page,
      questionRow(page, question.id),
      "before the asks read answers"
    );

    await openIssueAddress(page, "activity");
    const answered = page.waitForResponse(
      (response) => asksRead(new URL(response.url())) && response.status() === 200
    );
    for (const route of held) await route.continue();
    await page.unroute(asksRead);
    await answered;

    await openIssueAddress(page, "conversation");
    await settle(questionRow(page, question.id), "back on the conversation");
    await expectQuestionInView(page, questionRow(page, question.id), "back on the conversation");
  } finally {
    await alice.close();
  }
});

// ---- from 750e2716:packages/dispatch/e2e/ask-part-references.e2e.ts

const agent = {
  actor: { id: "e2e-ask-parts", kind: "session" as const },
  as: "agent" as const,
};

/** Opens the hover card of `link` from the keyboard, which every project can do (the phone
 *  project has no hover), and answers the card. No Escape first: the link may be in Details,
 *  which Escape closes, and no card is open before this. */
async function openCard(page: Page, link: Locator): Promise<Locator> {
  await page.mouse.move(0, 0);
  await page.keyboard.press("Tab");
  await link.focus();
  const card = page.getByRole("tooltip");
  await expect(card).toBeVisible();
  return card;
}

// Off the pull-request lane: `main`'s source row in `ask-part-references.e2e.ts` runs there, and
// the lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this
// copy.
test("a reference to an ask's option in a comment is a link that opens the ask at that part", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ask part references" });
  const ask = await createAsk(
    issue.key,
    {
      options: [
        { description: "Ship at the start of the week.", label: "Monday" },
        { description: "Ship once the review lands.", label: "Tuesday" },
      ],
      question:
        "The release train ships the reference graph. Monday ships it before the week's reviews " +
        "and holds nothing; Tuesday waits on the review that lands Monday evening. Which day " +
        "does it ship on? Recommendation: Monday, so a reader has the graph for the reviews.",
    },
    agent
  );
  // A write's own answer carries no part id: the ask's own read is what names the part each of
  // its options is stored as.
  const read = await getAsk(ask.id);
  const option = (read.ask.options ?? [])[1];
  expect(
    option?.id,
    "the ask's read names the part each of its options is stored as"
  ).toBeDefined();
  const part = option?.id ?? "";
  // A part's text is what the server stores for it: an option is its label, an em dash and its
  // description.
  const optionText = "Tuesday — Ship once the review lands.";
  const reference = `dispatch://${issue.key}/ask/${ask.id}#b-${part}`;
  const comment = await createComment(issue.key, { body: `We take ${reference}.` }, agent);

  // An agent wrote the comment, in a thread no person wrote in, so it is a turn of the history,
  // in Details, which `/activity` opens (dispatch://INFRA-1196 R15).
  await page.goto(`/issues/${issue.key}/activity`);
  const turns = historyItem(page, comment.id);
  // The link's text is the part's own text, read from the graph, not the reference it was
  // written as.
  const link = turns.getByRole("link", { name: optionText });
  await expect(link).toBeVisible();
  await expect(link).toHaveAttribute("data-dispatch-ref", reference);
  await expect(link).toHaveAttribute(
    "href",
    `/issues/${issue.key}/asks/${ask.id}#b-${encodeURIComponent(part)}`
  );

  // The card the link opens heads with the ask the part belongs to, where a block of a document
  // heads with its document, and shows the part's text.
  const card = await openCard(page, link);
  await expect(card).toContainText(`${issue.key} · Ask`);
  await expect(card).toContainText(optionText);
  await page.keyboard.press("Escape");
  await expect(page.getByRole("tooltip")).toHaveCount(0);

  // Following it opens the ask at that part: the ask's node of the outline takes the focus, on
  // screen (R20).
  await link.click();
  await expect(page).toHaveURL(`/issues/${issue.key}/asks/${ask.id}#b-${encodeURIComponent(part)}`);
  await expect.poll(() => focusedNodeId(page)).toBe(ask.id);
  const turn = nodeRow(page, ask.id).first();
  await expect(turn).toBeVisible();
  await expect(turn).toBeInViewport();
  // The ask shows an option as its label and its description, which the part's stored text joins
  // with an em dash: the part the link named is the option the reader lands on.
  await expect(turn).toContainText("Tuesday");
  await expect(turn).toContainText("Ship once the review lands.");
});

test("the backlink an ask's option writes names the ask as its source, not an artifact", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Target of an option's link" });
  const source = await createIssue({ project: "CORE", title: "Asking issue" });
  const ask = await createAsk(
    source.key,
    {
      options: [
        { description: "Ship the reference graph now.", label: "Ship" },
        { description: `Hold until dispatch://${target.key} ships.`, label: "Wait" },
      ],
      question:
        "The reference graph ships this week, and the issue it depends on has not landed. Ship " +
        "now and backfill, or wait for it? Recommendation: ship now, so the graph is readable " +
        "while the reviews run.",
    },
    agent
  );
  // The option's stored text, which the panel shows as the row's excerpt: its label, an em dash
  // and its description, drawn by the renderer, so the address in it reads as the issue's name.
  const optionText = `Wait — Hold until ${target.key} ${target.title} ships.`;

  await page.goto(`/issues/${target.key}`);
  // The backlink panel is the References section of the issue page's one panel, Details, where
  // what references the issue is one of its properties (dispatch://INFRA-1196 R12, R14).
  await openDetails(page);
  const panel = page.locator('[data-margin-section="references"]');
  await panel
    .getByRole("button", { name: new RegExp(`^${PROPERTY_LABELS.referenced_by}`) })
    .click();
  const references = panel.getByRole("region", { name: PROPERTY_LABELS.referenced_by });

  // The source is the ask the part belongs to, as a block of a comment reads as its comment, and
  // it links to the ask at that part. A part of an ask is no document, so nothing here reads
  // "Artifact", the label a block of a document carries - linked or as the plain text a build
  // that cannot place the part's reference writes.
  const askSource = references.getByRole("link", { name: `Ask · ${source.key}` });
  await expect(askSource).toBeVisible();
  await expect(references.getByText(/^Artifact · /)).toHaveCount(0);
  await expect(references.locator(".dispatch-markdown")).toHaveText(optionText);
  // One row, not two: an ask's own links are its question's, so the option's citation is the
  // part's edge alone.
  await expect(askSource).toHaveCount(1);

  // It opens the ask at the part the option is stored as, which the ask's own read names.
  const read = await getAsk(ask.id);
  const option = (read.ask.options ?? [])[1];
  expect(
    option?.id,
    "the ask's read names the part each of its options is stored as"
  ).toBeDefined();
  await expect(askSource).toHaveAttribute(
    "href",
    `/issues/${source.key}/asks/${ask.id}#b-${encodeURIComponent(option?.id ?? "")}`
  );
});

// ---- from 750e2716:packages/dispatch/e2e/address-links.e2e.ts

const entry = fixture("bare-addresses-each-with-its-own-host");

/** The links the engine reads in the entry's nine address paragraphs, one list per paragraph, in
 *  the order of the paragraphs after the corpus file's opening sentence. Taken from the entry's
 *  `pm_json` link marks, which `pmdoc/gen/differential.ts` recorded from the engine; a host per
 *  paragraph keeps every link name unique on the page, since a reader tells two links apart by
 *  their accessible names. The engine's two readers end the last two addresses differently: its
 *  tokenizer drops a trailing `_` after `;`, its transform keeps it after an escaped colon. */
const links: { href: string; name: string }[][] = [
  [{ href: "http://lower.test", name: "http://lower.test" }],
  [{ href: "https://semi.test", name: "https://semi.test" }],
  [{ href: "https://hash.test", name: "https://hash.test" }],
  [{ href: "https://amp.test", name: "https://amp.test" }],
  [{ href: "https://star.test", name: "https://star.test" }],
  [
    { href: "http://www.a.test/?u=", name: "www.a.test/?u=" },
    { href: "https://b.test", name: "https://b.test" },
  ],
  [{ href: "https://quote.test", name: "https://quote.test" }],
  [{ href: "https://a.test/a", name: "https://a.test/a" }],
  [{ href: "http://b.test/a_", name: "http://b.test/a_" }],
];

test.describe("a bare web address is a link in a document written through the API, as in a comment", () => {
  // SLOW(>=2s): 3,811 ms in a four-worker CI report of this head, 2,545 ms at the merge base.
  // Nine paragraphs, each a bare address, asserted twice - once in the comment's row of the
  // outline and once in the document editor - which is ten link lookups per surface.
  // Off the pull-request lane: `main`'s source row in `address-links.e2e.ts` runs there, and the
  // lane's real-stack budget (`e2e/__tests__/pull-request-lane.test.ts`) has no room for this copy.
  test("after punctuation or an escaped scheme colon", async ({ browser }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: entry.markdown,
      title: "Address links",
    });
    const comment = await createComment(issue.key, { body: entry.markdown });

    const context = await asReader(browser, "alice");
    try {
      const page = await context.newPage();
      // The corpus file's opening sentence holds no address; the nine paragraphs after it are the
      // rows, each paired with its own links.
      const rows = links.map((row, index) => ({
        links: row,
        text: entry.reading.paragraphs[index + 1].text,
      }));

      /** Asserts `root` shows every row's text and the name and href of every link in it. */
      const expectRows = async (root: Locator): Promise<void> => {
        for (const row of rows) {
          await expect(root).toContainText(row.text);
          for (const link of row.links) {
            await expect(root.getByRole("link", { name: link.name, exact: true })).toHaveAttribute(
              "href",
              link.href
            );
          }
        }
      };

      // A person's comment is a node of the outline, drawn with its words in full
      // (dispatch://INFRA-1196 R1, ruling 1).
      await page.goto(`/issues/${issue.key}/conversation`);
      await expectRows(nodeRow(page, comment.id).first());

      // Each row's text is asserted before its links, so a row Go read as text fails as "text
      // present, no link".
      await openSpecDocument(page, issue.key, rows[0].text);
      await expectRows(documentEditor(page));

      expect((await getArtifactText(issue.primary_artifact_id)).markdown).not.toContain("\\:");
    } finally {
      await context.close();
    }
  });
});

// ---- from 750e2716:packages/dispatch/e2e/download-names.e2e.ts

/** A file to upload, in the shape Playwright's multipart writes and file inputs take. */
interface UploadFile {
  buffer: Buffer;
  mimeType: string;
  name: string;
}

/** A file the server stores as a file and not a picture: `%PDF-` is no picture's signature. */
function pdf(name: string, text: string): UploadFile {
  return { buffer: Buffer.from(`%PDF-1.4\n% ${text}\n`), mimeType: "application/pdf", name };
}

/** Uploads `file` as the signed-in page through `path`, an owner's artifacts route, as a multipart
 *  write. `page.request` sends the page's session cookie and no `Origin`, and the server refuses a
 *  cookie-authenticated write whose `Origin` is not the dashboard's (enforceCookieOrigin). */
async function upload(
  page: Page,
  path: string,
  file: UploadFile
): Promise<ArtifactUploadResponse["artifact"]> {
  const answer = await page.request.post(path, {
    headers: { Origin: dashboardOrigin },
    multipart: { file, name: file.name },
  });
  expect(answer.status(), await answer.text()).toBeLessThan(300);
  return ((await answer.json()) as ArtifactUploadResponse).artifact;
}

/** The download `link` starts when it is clicked: the name the browser saves it under, the
 *  address it came from and, once it has finished, the file it wrote. */
async function saveFrom(page: Page, link: Locator) {
  const saved = page.waitForEvent("download");
  await link.click();
  return saved;
}

/** The path a link or a picture names for version `number`: the download address with
 *  `download`, the version address a picture is drawn from without it. */
function versionAddress(id: string, number: number, download = false): string {
  return `/api/v1/artifacts/${id}/versions/${number}${download ? "?download=true" : ""}`;
}

/** Opens the issue's Documents' list by a load of its own address: the list took the Artifacts
 *  tab's list (part N's R2), one row per file with the download of its latest version, and the
 *  file's versions and picture are on its own page, which the row's link opens. */
async function loadDocumentsList(page: Page, issueKey: string): Promise<void> {
  await page.goto(`/issues/${issueKey}/documents`);
  await expect(documentsList(page), "Documents' list did not open").toBeVisible();
}

// SLOW(>=2s): a cold load of the issue's Documents' list, then the artifact's page and its whole
// version list, each link and picture read where it is drawn.
// @webkit @firefox: the address the engine follows for a download and the one it draws a picture
// from, which the rows below read the saved names of.
test("each Download version link names the download address and each picture the version address @webkit @firefox", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Download addresses" });
  const picture = {
    buffer: Buffer.from(ONE_PIXEL_PNG, "base64"),
    mimeType: "image/png",
    name: "diagram.png",
  };
  const diagram = await upload(page, `/api/v1/issues/${issue.key}/artifacts`, picture);
  await upload(page, `/api/v1/issues/${issue.key}/artifacts`, picture);

  await loadDocumentsList(page, issue.key);
  // The row draws no picture: the file's picture is on its own page, read below.
  const item = documentRow(page, "diagram.png");
  await expect(item.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "href",
    versionAddress(diagram.id, 2, true)
  );

  await documentLink(page, "diagram.png").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/diagram-png`);
  const shown = page.getByRole("region", { exact: true, name: "diagram.png version 2" });
  await expect(shown.getByRole("img", { name: "diagram.png version 2" })).toHaveAttribute(
    "src",
    versionAddress(diagram.id, 2)
  );
  await expect(shown.getByRole("link", { name: "Download version 2" })).toHaveAttribute(
    "href",
    versionAddress(diagram.id, 2, true)
  );
  // Every version is drawn open, with no control that hides any (part N's R2).
  const versions = page.getByRole("region", { exact: true, name: "Versions for diagram.png" });
  for (const number of [1, 2]) {
    await expect(
      versions.getByRole("link", { name: `Download version ${number}` })
    ).toHaveAttribute("href", versionAddress(diagram.id, number, true));
  }
});

// SLOW(>=2s): two uploads through Documents' list, each written and listed, and four downloads
// the browser saves from the list, the artifact's page and its version list.
// @webkit @firefox: the name the engine saves from `filename*` and `filename`, and whether it asks
// the server again for a download address it already followed: version 1's address saves a new
// name once version 2 exists.
test("a file saves under its own name, and its older version under its number once a newer one exists @webkit @firefox", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Report downloads" });
  await loadDocumentsList(page, issue.key);
  const item = documentRow(page, "report.pdf");

  await uploadInDocuments(page, pdf("report.pdf", "first draft"));
  const first = await saveFrom(page, item.getByRole("link", { name: "Download version 1" }));
  expect(first.suggestedFilename()).toBe("report.pdf");

  await uploadInDocuments(page, pdf("report.pdf", "second draft"));
  await expect(item.getByRole("link", { name: "Download version 2" })).toBeVisible();
  await documentLink(page, "report.pdf").click();
  await expect(page).toHaveURL(`/issues/${issue.key}/artifacts/report-pdf`);
  const shown = page.getByRole("region", { exact: true, name: "report.pdf version 2" });
  const current = await saveFrom(page, shown.getByRole("link", { name: "Download version 2" }));
  expect(current.suggestedFilename()).toBe("report.pdf");

  // Every version is drawn open, with no control that hides any (part N's R2).
  const versions = page.getByRole("region", { exact: true, name: "Versions for report.pdf" });
  const older = await saveFrom(page, versions.getByRole("link", { name: "Download version 1" }));
  expect(older.url()).toBe(first.url());
  expect(older.suggestedFilename()).toBe("report-v1.pdf");
  const newest = await saveFrom(page, versions.getByRole("link", { name: "Download version 2" }));
  expect(newest.suggestedFilename()).toBe("report.pdf");
});

// SLOW(>=2s): a cold load of the issue's Documents' list, an upload through it, and the download
// the browser saves.
// @webkit @firefox: the name the engine saves from `filename*` where the name holds spaces and a
// character outside ASCII, which the ASCII `filename` beside it cannot carry, and WebKit's own
// rule of writing each space of the name it read as `_`.
test("a file whose name holds spaces and a character outside ASCII saves under that name @webkit @firefox", async ({
  page,
  browserName,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Accented downloads" });
  await loadDocumentsList(page, issue.key);

  const name = "Q3 final résumé.pdf";
  await uploadInDocuments(page, pdf(name, "the quarter"));
  const saved = await saveFrom(
    page,
    documentRow(page, name).getByRole("link", { name: "Download version 1" })
  );
  // WebKit reads `filename*`, since the `é` stays where the ASCII `filename` has `_`, and then
  // writes each space as `_` (CI run 43750246861). Chromium and Firefox save the name exactly.
  const expected = browserName === "webkit" ? "Q3_final_résumé.pdf" : name;
  expect(saved.suggestedFilename()).toBe(expected);
});

// SLOW(>=2s): a cold load of the issue's Documents' list, and a download the browser saves and
// the row reads back from disk.
// @webkit @firefox: the name the engine saves a document's Markdown under, and the bytes it writes.
test("the spec saves as spec.md holding its Markdown @webkit @firefox", async ({ page }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Launch plan\n\nEvery download keeps its own name.\n",
    title: "Spec download",
  });
  const specName = (await getArtifact(issue.primary_artifact_id)).name;
  await loadDocumentsList(page, issue.key);

  const item = documentRow(page, specName);
  const saved = await saveFrom(page, item.getByRole("link", { name: "Download version 1" }));
  expect(saved.suggestedFilename()).toBe("spec.md");
  const spec = await getArtifactVersion(issue.primary_artifact_id, 1);
  expect(spec.markdown).toContain("Every download keeps its own name.");
  expect(await readFile(await saved.path(), "utf8")).toBe(spec.markdown);
});

// ---- from 750e2716:packages/dispatch/e2e/prefix-links.e2e.ts

/** `id` as an address writes it short (`SHORT_ID`). */
function short(id: string): string {
  return id.slice(0, SHORT_ID);
}

/** The id of the last filler paragraph of the host's spec, which stands below the fold. */
async function blockBelowTheFold(world: ReferenceWorld): Promise<string> {
  const { markdown } = await getArtifactText(world.host.primary_artifact_id);
  const blocks = await getArtifactBlocks(world.host.primary_artifact_id);
  const filler = blocks.filter((block) =>
    markdown.slice(block.from, block.to).startsWith("Filler paragraph ")
  );
  const last = filler.at(-1);
  if (last === undefined) {
    throw new Error("setup: the host's spec holds no filler paragraph");
  }
  return last.id;
}

// A `/spec` address naming a comment by `?comment=` opens the issue page on that comment, not the
// spec it quotes (dispatch://INFRA-1196 R20); written short, the id is resolved first and the
// address replaced by the whole one. The comment is on the spec's text, which part M's history in
// Details holds no turn of: the page draws it as a node, and the address focuses that node, on
// screen, with its text (part N's R6), as today's page opened its item in the margin. Threads
// lands a comment on text once dispatch://INFRA-5704 is on `main`.
test("a spec address that writes an anchored comment's id short opens it and is replaced by the whole id", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const anchored = await createComment(
    world.host.key,
    { anchor: { artifact: "spec", quote: ANCHOR }, body: "Anchored on the first line." },
    referenceAgent
  );
  expect(anchored.anchor?.quote).toBe(ANCHOR);

  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    const address = `/issues/${world.host.key}/spec?comment=`;
    await page.goto(`${address}${short(anchored.id)}`);
    await expect(page).toHaveURL(`${address}${anchored.id}`);
    await expect
      .poll(() => focusedNodeId(page), { message: "the address does not focus the comment" })
      .toBe(anchored.id);
    const turn = nodeRow(page, anchored.id).first();
    await expect(turn).toBeInViewport();
    await expect(turn).toContainText("Anchored on the first line.");
  } finally {
    await context.close();
  }
});

test("a document address whose block fragment writes the block's id short opens the document at that block", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const block = await blockBelowTheFold(world);

  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    const address = `${await specAddress(world.host.key)}#b-`;
    await page.goto(`${address}${short(block)}`);
    await expectDocumentConnected(page);
    await expect(documentEditor(page).locator(`[data-block-id="${block}"]`)).toBeInViewport();
    await expect(page).toHaveURL(`${address}${block}`);
  } finally {
    await context.close();
  }
});

test("a comment that writes an ask's id short reads as the ask's title, opens it, and follows an edit of the question with no reload", async ({
  browser,
}) => {
  const world = await seedReferenceWorld();
  const ask = world.items.ask;
  const reference = `dispatch://${world.host.key}/ask/${short(ask.id)}`;
  const comment = await createComment(
    world.host.key,
    { body: `Settled in ${reference} already.` },
    referenceAgent
  );

  const context = await asReader(browser, "alice");
  try {
    const page = await context.newPage();
    // An agent wrote this comment, in a thread no person wrote in, so it is a turn of the
    // history, in Details, which `/activity` opens (dispatch://INFRA-1196 R15), marked by part
    // M's `data-history-item`.
    await page.goto(`/issues/${world.host.key}/activity`);
    const link = historyItem(page, comment.id).locator(
      `a[data-dispatch-ref="${world.items.ask.reference}"]`
    );
    await expect(link).toHaveText("Which cache backs the rollout?");
    // A reload would drop this mark.
    await page.evaluate(() => {
      document.documentElement.dataset.e2eNoReload = "kept";
    });
    await editAsk(ask.id, { question: "Which cache backs the rollout now?" }, referenceAgent);
    await expect(link).toHaveText("Which cache backs the rollout now?");
    await expect(page.locator("html")).toHaveAttribute("data-e2e-no-reload", "kept");

    await link.click();
    await expect(page).toHaveURL(`/issues/${world.host.key}/asks/${ask.id}`);
    // The question is a node of the outline (dispatch://INFRA-1196 R1), its row the turn today's
    // page drew.
    await expect(nodeRow(page, ask.id).first()).toContainText("Which cache backs the rollout now?");
  } finally {
    await context.close();
  }
});

// ---- from 750e2716:packages/dispatch/e2e/doc.e2e.ts

test("round-trips through the filters leave one open document connection and the typed text", async ({
  browser,
}) => {
  await createProject({ key: "TABS", name: "Tabs" });
  const issue = await createIssue({
    project: "TABS",
    spec: "# Keep this document",
    title: "Mounted panels",
  });
  for (let index = 0; index < 30; index += 1) {
    await createMessage(issue.key, { body: `Existing message ${index}` }, carol);
  }

  const alice = await asReader(browser, "alice");
  try {
    const page = await alice.newPage();
    // Open connections, not connections ever opened: a document opened in place of the outline
    // is unmounted when the reader leaves it for a filter (dispatch://INFRA-1196 R3), which
    // closes its socket, and coming back opens one again. What must never happen is a tab that
    // accumulates live rooms.
    const sockets = openDocumentSockets(page);
    const specPath = await specAddress(issue.key);
    await page.goto(specPath);
    const editor = documentEditor(page);
    await typeAtEnd(page, "stay mounted");
    await expect(editor).toContainText("stay mounted");
    await expect.poll(sockets).toBe(1);

    // The conversation is the outline, at its own address, reached by a move inside the page as
    // the Conversation tab was (dispatch://INFRA-1196 R1, R20): a fresh load of the address
    // would open a second document connection by itself and prove nothing.
    await navigateInApp(page, `/issues/${issue.key}/conversation`);
    await expect(tree(page).getByText("Existing message 29")).toBeVisible();
    // The outline drawn again opens with the focus on its first row (`IssueTree.tsx`'s opening),
    // which brings that row on screen: the reader's scroll is made after it, as a reader's is.
    await expectOpeningLanded(page);

    // A round-trip through the filter row: the Spec filter on, then every filter off again with
    // Conversation (dispatch://INFRA-1196 R2), the outline drawn again at the end of it where the
    // reader left it (part N's R1). It is made at 390 px, where the view row stays at the top of
    // the screen as the page scrolls, so the reader presses it where they stand. At 1440 px the
    // view row scrolls away with the header, so a press there comes after the reader has scrolled
    // back up to it, and the place they leave the outline at is the one they pressed from. Below
    // 1280 px the seven toggles are one control whose menu holds them (part D's
    // `IssuePresets.tsx`), so each press is that control's and then its item's, and the view on is
    // read from the control's name (`expectActiveControl`), where the source read each toggle's
    // `aria-pressed`.
    //
    // The reader leaves the outline's row for the page before they scroll, as the source's reader
    // left the focus on the Conversation tab: the focused row draws its body, which closes as the
    // focus goes to the view row's control, and the browser's scroll anchoring then moves the page
    // by that body's height to keep what the reader sees, so the place they left would no longer
    // be the number they scrolled to.
    const wide = page.viewportSize();
    await page.setViewportSize({ height: 844, width: 390 });
    await focusThePage(page);
    await page.evaluate(() => window.scrollTo(0, 500));
    const scrollPosition = await page.evaluate(() => window.scrollY);
    expect(scrollPosition).toBeGreaterThan(0);
    for (const preset of ["spec", "conversation"] as const) {
      await presetChooser(page).click();
      await openMenu(page)
        .getByRole("menuitemradio", { name: new RegExp(`^${PRESET_NAMES[preset]}( |$)`) })
        .click();
      await expectActiveControl(page, preset);
    }
    await expect(tree(page).getByText("Existing message 29")).toBeVisible();
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(scrollPosition);

    // The same round-trip by the key table's chords, at the row's own width: `t s` turns the
    // Spec filter on, and `t c` turns every filter off (part N's R1). The reader stands on a row
    // the page shows, as one who moves by keys does, and goes to it as the keys go, the page
    // bringing it on screen. The outline is drawn again at that place with that row focused
    // (`stage-landing.tsx`'s `ViewNodeKeeper`), and `t c`, which puts the focus on the outline's
    // one tab stop, leaves the row where it stands (`useIssueKeymap.ts`'s `showPart`).
    if (wide !== null) await page.setViewportSize(wide);
    const spec = page.getByRole("button", { exact: true, name: PRESET_NAMES.spec });
    const conversation = page.getByRole("button", {
      exact: true,
      name: PRESET_NAMES.conversation,
    });
    const standing = tree(page).getByRole("treeitem").filter({ hasText: "Existing message 20" });
    await standing.focus();
    await expect(standing).toBeInViewport();
    const chordPosition = await page.evaluate(() => window.scrollY);
    expect(chordPosition).toBeGreaterThan(0);
    await page.keyboard.press("t");
    await page.keyboard.press("s");
    await expect(spec).toHaveAttribute("aria-pressed", "true");
    await page.keyboard.press("t");
    await page.keyboard.press("c");
    await expect(conversation).toHaveAttribute("aria-pressed", "true");
    await expect(tree(page).getByText("Existing message 29")).toBeVisible();
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBe(chordPosition);

    // Back to the document, with the text typed into it and one connection open, not two.
    await navigateInApp(page, specPath);
    await expect(editor).toContainText("stay mounted");
    await expect.poll(sockets).toBe(1);
  } finally {
    await alice.close();
  }
});

// Method names survive minification where class and import names do not: `getXmlFragment(`
// as a definition (not a `.getXmlFragment(` call) is Yjs's `Doc`, and `permissionDeniedHandler`
// is the Hocuspocus provider. Dispatch's own host code calls these; only the libraries define them.
const editorTransportMarkers = [/[^.\w$]getXmlFragment\(/u, /permissionDeniedHandler/u];

test("the conversation downloads no editor transport; a document loads it on demand", async ({
  browser,
}) => {
  await createProject({ key: "LAZY", name: "Lazy" });
  const issue = await createIssue({
    project: "LAZY",
    spec: "# Lazy\n\nUse SQLite\n",
    title: "Transport on demand",
  });
  await createMessage(issue.key, { body: "A message, no document" }, carol);

  const alice = await asReader(browser, "alice");
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
    await expect(tree(page).getByText("A message, no document")).toBeVisible();
    expect(scripts.size).toBeGreaterThan(1);
    expect(await transportChunks()).toEqual([]);

    await navigateInApp(page, await specAddress(issue.key));
    await expect(documentEditor(page)).toContainText("Use SQLite");
    expect((await transportChunks()).length).toBeGreaterThan(0);
  } finally {
    await alice.close();
  }
});

/** Holds each read of `reads`, by its path, from the page's next request on. `seen` names each
 *  held read that has gone out; `release` lets them all go, and takes the hold off only once each
 *  has gone on: a hold taken off while its reads still wait hands them on itself, and the wait's
 *  own `continue` then finds them handled. */
async function holdReads(
  page: Page,
  reads: readonly string[]
): Promise<{ readonly release: () => Promise<void>; readonly seen: ReadonlySet<string> }> {
  const released = Promise.withResolvers<void>();
  const seen = new Set<string>();
  const matchReads = ({ pathname }: URL) => reads.includes(pathname);
  const going: Promise<void>[] = [];
  const hold = (route: Route) => {
    seen.add(new URL(route.request().url()).pathname);
    const goes = released.promise.then(() => route.continue());
    going.push(goes);
    return goes;
  };
  await page.route(matchReads, hold);
  return {
    release: async () => {
      released.resolve();
      await Promise.all(going);
      await page.unroute(matchReads, hold);
    },
    seen,
  };
}

// c-view-waiting. Hold the view's real API requests before opening its address. The
// assertion of the status is the first reading of the view, not a wait for any row.
test("a view that waits for its reads says what it waits for", async ({ browser }) => {
  // Eight cold opens of a seeded issue, including four first-read failures with query retries.
  test.setTimeout(120_000);
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    const issue = `/api/v1/issues/${seeded.key}`;
    const spec = `/api/v1/artifacts/${seeded.specArtifactId}/blocks`;
    const plan = `/api/v1/artifacts/${seeded.planArtifactId}/blocks`;
    const verification = `/api/v1/artifacts/${seeded.verificationArtifactId}/blocks`;
    const coverage = `${issue}/coverage`;
    const results = `${issue}/results`;
    const decisions = `${issue}/decisions`;
    const asks = `${issue}/asks`;
    const views = [
      {
        body: "[data-spec-body]",
        failed: STATE_LINES.statements_unreadable,
        loading:
          "Loading which checks serve each statement, the statements, the results, the verification document, the decisions and the questions…",
        reads: [coverage, spec, results, verification, decisions, asks],
        ready: "[data-spec-statement]",
        view: "spec",
      },
      {
        body: "[data-verification-body]",
        failed: STATE_LINES.coverage_unreadable,
        loading: "Loading which checks serve each statement and the checks' results…",
        reads: [coverage, results, verification],
        ready: "[data-verification-check]",
        view: "verification",
      },
      {
        body: "[data-plan-body]",
        failed: STATE_LINES.plan_unreadable,
        loading: "Loading the plan and the spec…",
        reads: [plan, spec],
        ready: "[data-plan-bar-row]",
        view: "plan",
      },
      {
        body: "[data-decisions-body]",
        failed: STATE_LINES.issue_decisions_unreadable,
        loading: "Loading the decisions and the questions…",
        reads: [decisions, asks],
        ready: '[data-decisions-part="open_questions"] [data-node-row]',
        view: "decisions",
      },
    ] as const;
    for (const { body, failed, loading, reads, ready, view } of views) {
      await test.step(PRESET_NAMES[view], async () => {
        const slot = issuePage(page).locator(body);
        const status = slot.getByRole("status").and(slot.locator(VIEW_LOADING));
        const held = await holdReads(page, reads);
        try {
          await openPreset(page, seeded.key, view);
          await expect(status, "the view was blank while its reads were out").toBeVisible({
            timeout: 1_000,
          });
          await expect(status).toHaveText(loading, { timeout: 1_000 });
          await expect(outlineRows(page)).toHaveCount(0);
          await expect
            .poll(() => reads.every((read) => held.seen.has(read)), {
              message: "the page did not ask for every held read",
            })
            .toBe(true);
          await held.release();
          await expect(
            slot.locator(ready).first(),
            "the view did not draw after its reads answered"
          ).toBeVisible();
          await expect(status).toHaveCount(0);
        } finally {
          await held.release();
        }

        // A new page load has no cached answer, so this failure replaces the loading line.
        const refusedRead = ({ pathname }: URL) => pathname === reads[0];
        const refuse = (route: Route) =>
          route.fulfill({ json: { code: "SERVER_ERROR", error: "unreadable" }, status: 500 });
        await page.route(refusedRead, refuse);
        try {
          await openPreset(page, seeded.key, view);
          const line = slot.locator("[data-failed-read]").filter({ hasText: failed });
          await expect(line.getByRole("button", { exact: true, name: "Retry" })).toBeVisible();
          await expect(status).toHaveCount(0);
        } finally {
          await page.unroute(refusedRead, refuse);
        }
      });
    }
  });
});

/** The repository the pull requests of `c-pull-request-link` are written in. */
const PULL_REQUEST_REPOSITORY = "acme/widgets";

/** One `pull_request_heads` row attributed to `issue`, written as the `pull_request` envelope
 *  would have stored it (`e2e/verification.e2e.ts`'s `seedCICheck`), with the address and the
 *  time of the event it reported set by the row. */
async function writePullRequestHead(row: {
  readonly event: string;
  readonly issue: string;
  readonly number: number;
  readonly state: "closed" | "merged" | "open";
  readonly url: string;
}): Promise<void> {
  const sha = String(row.number).padStart(40, "0");
  await sql(
    `INSERT INTO pull_request_heads (repo, number, head_sha, state, issue_key, attribution_source, title, url, event_at, checked_at)
     VALUES ('${PULL_REQUEST_REPOSITORY}', ${row.number}, '${sha}', '${row.state}', '${row.issue}', 'title_body', '${row.issue} the header', '${row.url}', '${row.event}', now())`
  );
}

/** The header as it is drawn at `width`, read with the page at its top, and what it took, in the
 *  run's annotations and its output. `where` names the seed. At 390 px the title is drawn whole,
 *  so the reading counts all of it. */
async function readHeader(page: Page, width: number, where: string): Promise<HeaderSize> {
  await expect(filterRow(page), `the page draws no view row ${where}`).toBeVisible();
  // The view row waited for above is drawn after the header and under it, so both are in the page
  // by now and a null reading only throws.
  const reading = await headerSize(page);
  if (reading === null) throw new Error(`the page draws no header or no view row ${where}`);
  const description = `${where} at ${width} px: ${reading.lines.length} lines, ${reading.height.toFixed(1)} px`;
  console.log(`header ${description}: ${JSON.stringify(reading.lines)}`);
  test.info().annotations.push({ description, type: "header" });
  if (width === IPHONE_13.width) {
    await assertWhole(issueHeader(page).getByRole("heading", { level: 1 }));
  }
  return reading;
}

/** The header's limit at 390 px on the seeds of the 115-character title with 20 labels, with an
 *  approval request waiting on the reader or with none: part P's limit while a request waits, 9
 *  lines and 280 px (decision 17 of dispatch://INFRA-1196/artifact/part-decisions-md, `headerLimit`
 *  of `e2e/issue-page-p.ts`, which gives 8 lines and 250 px while none waits), applied to both
 *  seeds. Root R13's 6 lines and 180 px hold for a title of at most two lines:
 *  the owner's answer to the root's ask, "Long titles excepted"
 *  (dispatch://INFRA-1196/ask/7c644731-f514-498b-b57a-4e76136ce1a3), which the architect of
 *  INFRA-3940 applied to N's measurement (dispatch://INFRA-3940/comment/a5dd812e-6287-4be1-823a-220f3c1fa097).
 *  This title takes four lines, and with no pull request link the header takes 9 lines and
 *  265.0 px there with a request waiting, 8 lines and 235.0 px with none. A ruling that sets
 *  another limit changes this one line. */
const WORST_HEADER_AT_390 = { height: 280, lines: 9 } as const;

/** The limit on the header at `width` for a seed of the row: root R13's, but on a seed with the
 *  115-character title at 390 px `WORST_HEADER_AT_390`. */
function headerLimitOf(width: number, longTitle: boolean): { height: number; lines: number } {
  const limit = longTitle && width === IPHONE_13.width ? WORST_HEADER_AT_390 : headerLimit(width);
  if (limit === undefined) throw new Error(`no limit on the header at ${width} px`);
  return limit;
}

/** `reading` against the limit on the header at `width`, soft so that one run reports every
 *  width and seed, with the lines it counted in the message. `longTitle` is a seed with the
 *  115-character title. */
function expectHeaderWithinItsLimit(
  reading: HeaderSize,
  width: number,
  where: string,
  longTitle: boolean
): void {
  const limit = headerLimitOf(width, longTitle);
  const drawnAs = `${reading.lines.length} lines, ${reading.height.toFixed(1)} px`;
  expect
    .soft(
      reading.lines.length,
      `the header takes ${drawnAs} ${where} at ${width} px: ${JSON.stringify(reading.lines)}`
    )
    .toBeLessThanOrEqual(limit.lines);
  expect
    .soft(reading.height, `the header takes ${drawnAs} ${where} at ${width} px`)
    .toBeLessThanOrEqual(limit.height);
}

/** The header with the link against the same header without it: the link takes no line and no
 *  pixel of the header's height. */
function expectLinkAddsNothing(
  reading: HeaderSize,
  without: HeaderSize,
  width: number,
  where: string
): void {
  const lines = reading.lines.length - without.lines.length;
  const height = reading.height - without.height;
  expect
    .soft(lines, `the link adds ${lines} lines to the header ${where} at ${width} px`)
    .toBeLessThanOrEqual(0);
  expect
    .soft(height, `the link adds ${height.toFixed(1)} px to the header ${where} at ${width} px`)
    .toBeLessThanOrEqual(0);
}

// c-pull-request-link (R22): the header draws the issue's pull request as one link on the
// properties line, after the assignee and before the count of what waits on the reader, with its
// number and its state in the page's words, and keeps root R13's sizes: 3 lines and 130 px at
// 1440 px, 6 lines and 180 px at 390 px. The harness runs with NATS off and serves no webhook, so
// the row writes the `pull_request_heads` rows an issue's pull requests would have left, an open
// one and a newer merged one, and reads the answer of `GET /api/v1/issues/{key}` as the server
// gives it. An issue with none carries no `pull_request` and draws no link. Then the header with
// the 115-character title and 20 labels is read without the link and with it, twice: with only
// the two questions waiting on the reader, where the line of the count and the labels has the
// least room left, and with an approval request waiting too, the worst header. Each is at 1440 px
// inside R13's sizes and at 390 px inside `WORST_HEADER_AT_390`, and the link takes no line and
// no pixel of the header the issue draws without it. A pass does not show a state change while
// the page stays open (the field comes with the issue's read).
// SLOW(>=2s): 14.1 s on chromium and 8.2 s on iphone (one worker, the pod): a project and
// the header's seed, four pull request rows written and deleted, a title, 20 labels and an
// approval request written, and six loads of the issue at each of the row's widths, each read
// for its header's size.
test("the header links the issue's pull request @phone", async ({ browser }) => {
  test.setTimeout(180_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    const desktopProject = test.info().project.name !== "iphone";
    const widths = desktopProject ? [DESKTOP.width, IPHONE_13.width] : [IPHONE_13.width];
    const openUrl = `https://github.com/${PULL_REQUEST_REPOSITORY}/pull/101`;
    const mergedUrl = `https://github.com/${PULL_REQUEST_REPOSITORY}/pull/102`;
    /** The header's links to a pull request: the one link the line draws, none where absent. */
    const link = () => propertiesLine(page).getByRole("link");
    const loadAt = async (width: number) => {
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seeded.key}`);
      await expectHeaderDrawn(page);
    };
    /** The line's parts in the order it draws them: the assignee, the link, the count. */
    const lineParts = () =>
      lineOrder(page, [
        { name: "assignee", selector: `select[aria-label="${assigneeControlName(seeded.key)}"]` },
        { name: "pull request", selector: '[data-testid="issue-pull-request"]' },
        { name: "count", selector: '[data-testid="issue-whose-turn"]' },
      ]);
    /** Reads the header's link as the open pull request, after the assignee and before the count,
     *  at each width, and hands the header's size to `judge`. */
    const expectOpenLinked = async (
      where: string,
      judge: (reading: HeaderSize, width: number) => void
    ) => {
      for (const width of widths) {
        await loadAt(width);
        await expect(link(), "the header draws no pull request link").toHaveCount(1);
        await expect(link(), "the header's link names another pull request").toHaveAttribute(
          "href",
          openUrl
        );
        await expect(link(), "the header's link says no number or no state").toHaveText(
          `#101 ${PULL_REQUEST_STATE_WORDS.open}`
        );
        expect(await lineParts(), "the link is not between the assignee and the count").toEqual([
          "assignee",
          "pull request",
          "count",
        ]);
        judge(await readHeader(page, width, where), width);
      }
    };
    /** Reads the header of `where`, which has the 115-character title, at each width with no pull
     *  request and then with the open one: each inside its limit, and the link taking no line
     *  and no pixel of the header drawn without it. The pull request is deleted after. */
    const expectLinkAddsNothingOn = async (where: string) => {
      const without = new Map<number, HeaderSize>();
      for (const width of widths) {
        await loadAt(width);
        await expect(issueHeader(page)).toContainText(MEASURED_TITLE);
        await expect(link(), "an issue with no pull request draws a link").toHaveCount(0);
        const reading = await readHeader(page, width, `${where}, no pull request`);
        without.set(width, reading);
        expectHeaderWithinItsLimit(reading, width, `${where}, no pull request`, true);
      }
      await writePullRequestHead({
        event: "2026-11-10T10:00:00Z",
        issue: seeded.key,
        number: 101,
        state: "open",
        url: openUrl,
      });
      await expectOpenLinked(`${where}, and a pull request`, (reading, width) => {
        const bare = without.get(width);
        if (bare === undefined)
          throw new Error(`no reading of the header without the link at ${width} px`);
        expectHeaderWithinItsLimit(reading, width, `${where}, and a pull request`, true);
        expectLinkAddsNothing(reading, bare, width, where);
      });
      await sql(`DELETE FROM pull_request_heads WHERE issue_key = '${seeded.key}'`);
    };

    // An issue that holds no pull request carries no field, and draws no link.
    expect(
      Object.hasOwn(await getIssue(seeded.key), "pull_request"),
      "an issue with no pull request carries a pull_request"
    ).toBe(false);
    for (const width of widths) {
      await loadAt(width);
      await expect(link(), "an issue with no pull request draws a link").toHaveCount(0);
    }

    // An open pull request and a newer merged one: the server names the open one, and the header
    // links it.
    await writePullRequestHead({
      event: "2026-11-10T10:00:00Z",
      issue: seeded.key,
      number: 101,
      state: "open",
      url: openUrl,
    });
    await writePullRequestHead({
      event: "2026-11-12T10:00:00Z",
      issue: seeded.key,
      number: 102,
      state: "merged",
      url: mergedUrl,
    });
    expect(
      (await getIssue(seeded.key)).pull_request,
      "the issue names another pull request"
    ).toEqual({ number: 101, repo: PULL_REQUEST_REPOSITORY, state: "open", url: openUrl });
    const seededIssue = "on the seeded issue";
    await expectOpenLinked(seededIssue, (reading, width) =>
      expectHeaderWithinItsLimit(reading, width, seededIssue, false)
    );

    // The pull requests gone, the field and the link go with them.
    await sql(`DELETE FROM pull_request_heads WHERE issue_key = '${seeded.key}'`);
    expect(
      Object.hasOwn(await getIssue(seeded.key), "pull_request"),
      "the issue still carries a pull_request"
    ).toBe(false);

    // The 115-character title and 20 labels, with only the two questions waiting on the reader:
    // the line that holds the count holds the labels and their count too, with the least room
    // left of any line of the header.
    await patchIssue(seeded.key, { labels: [...HEADER_LABELS], title: MEASURED_TITLE });
    await expectLinkAddsNothingOn("with the longest title and 20 labels, no request");
    // The worst header: the same, and an approval request waiting on the reader.
    await requestApproval(seeded.specArtifactId, { summary: "Read it back." });
    await expectLinkAddsNothingOn("with the longest title, 20 labels and a request");
  });
});

/** The layers of the title block's computed `box-shadow`, in words: "the lower line" for the
 *  1 px line in `lineColour`, the page's line colour as the browser writes it, "the soft shadow"
 *  for the prototype's 6 px shadow, each other layer as the browser writes it, and nothing for
 *  `none` or for the transparent layers Tailwind sets under every shadow. A comma inside a
 *  colour's parentheses splits no layer. */
function titleBlockEdge(shadow: string, lineColour: string): string[] {
  if (shadow === "none") return [];
  return shadow
    .split(/,(?![^(]*\))/)
    .map((layer) => layer.trim())
    .filter((layer) => !layer.startsWith("rgba(0, 0, 0, 0) "))
    .map((layer) => {
      if (layer === `${lineColour} 0px 1px 0px 0px`) return "the lower line";
      return layer.endsWith(" 0px 6px 14px 0px") ? "the soft shadow" : layer;
    });
}

// c-title-edge (R22): from 761 px, where the title block stays at the top of the screen, it draws
// its lower line, 1 px in the page's line colour, from the first pixel the page scrolls, and its
// soft shadow under that line past 40 px as the prototype does, so the properties line passes
// under an edge and not under a cut; at the page's top it draws neither. It is read on part P's
// header seed with 20 labels written, so the properties line that passes under the block holds
// labels and a photo, the assignee's circle: at 1440 px the block's computed `box-shadow` is
// `none` at the top, holds the line and no soft shadow at 1, 20 and 40 px scrolled, and the line
// and the soft shadow at 320 px. At 390 px the header scrolls away with the page and the block
// draws neither at any of those places. A pass does not show 760 px or 761 px, which the row does
// not read; part P's sticky rows read both.
// SLOW(>=2s): the header's seed with 20 labels written, then a load of the issue into a short
// window at each of two widths, each page scrolled to five places and its edge read once it holds
// still (`settledValue`).
test("the title block draws its lower line from the first pixel scrolled", async ({ browser }) => {
  test.setTimeout(120_000);
  await phase(browser, seedHeader, async (page, seeded) => {
    await patchIssue(seeded.key, { labels: [...HEADER_LABELS] });
    for (const width of [DESKTOP.width, IPHONE_13.width]) {
      const stays = width > 760;
      // A short window and a page made taller than it, so the page scrolls 320 px at both widths,
      // opened as part P's sticky rows open it (`openScrollingPage` in `e2e/issue-page-p.e2e.ts`):
      // the outline's opening lands and the focus then leaves it, or the page's growth would
      // scroll back to the row it holds.
      await page.setViewportSize({ height: 420, width });
      await gotoIssue(page, `/issues/${seeded.key}/conversation`);
      await expectHeaderDrawn(page);
      await expectOpeningLanded(page);
      await focusThePage(page);
      await page.evaluate(() => {
        const issue = document.querySelector("[data-issue-page]");
        if (issue instanceof HTMLElement) issue.style.minHeight = "2000px";
        window.scrollTo(0, 0);
      });
      // The page's line colour as the browser writes a computed colour, read off an element that
      // takes it, so it compares with a layer of a computed `box-shadow`.
      const lineColour = await page.evaluate(() => {
        const root = getComputedStyle(document.documentElement);
        if (root.getPropertyValue("--color-page-line").trim() === "") {
          throw new Error("the page defines no --color-page-line");
        }
        const probe = document.createElement("span");
        probe.style.color = "var(--color-page-line)";
        document.body.append(probe);
        const colour = getComputedStyle(probe).color;
        probe.remove();
        return colour;
      });
      if (stays) {
        // What passes under the block as the page scrolls: the seed's labels and its photo.
        await expect(
          propertiesLine(page).locator("[data-header-label]").first(),
          "the properties line holds no label"
        ).toBeVisible();
        await expect(
          propertiesLine(page).locator("[data-initial]"),
          "the properties line holds no photo"
        ).toBeVisible();
      }
      for (const y of [0, 1, 20, 40, 320]) {
        await page.evaluate((top) => window.scrollTo(0, top), y);
        await expect
          .poll(async () => (await stickyReading(page))?.scrolled, {
            message: `the page did not scroll to ${y} px at ${width} px`,
          })
          .toBe(y);
        // The band read once two reads a poll apart agree: the scroll's handler and the render it
        // asked for are done by then.
        const reading = await settledValue(`the band at ${y} px scrolled at ${width} px`, () =>
          stickyReading(page)
        );
        if (reading === null) throw new Error(`the page draws no header at ${width} px`);
        expect(reading.scrolled, `the page left ${y} px scrolled at ${width} px`).toBe(y);
        if (!stays) {
          expect
            .soft(
              reading.shadow,
              `the title block draws an edge at ${y} px scrolled at ${width} px, where the header scrolls away`
            )
            .toBe("none");
          if (y === 320) {
            expect(
              reading.block.bottom,
              `the header stayed on screen at ${width} px`
            ).toBeLessThanOrEqual(0);
          }
          continue;
        }
        if (y === 0) {
          expect
            .soft(reading.shadow, "the title block draws an edge at the page's top")
            .toBe("none");
          continue;
        }
        const drawn = titleBlockEdge(reading.shadow, lineColour);
        const under = drawn.filter((layer) => layer !== "the lower line");
        expect
          .soft(drawn, `the title block draws no lower line at ${y} px scrolled`)
          .toContain("the lower line");
        if (y <= 40) {
          expect
            .soft(under, `the title block draws more than its lower line at ${y} px scrolled`)
            .toEqual([]);
        } else {
          expect
            .soft(under, `the title block draws no soft shadow at ${y} px scrolled`)
            .toEqual(["the soft shadow"]);
          expect(
            Math.abs(reading.block.top),
            `the title block left the top of the screen at ${y} px scrolled`
          ).toBeLessThanOrEqual(1);
        }
      }
    }
  });
});

/** Waits until the focus is on a row other than `from` (none, the first time), and Details' "The row
 *  you are on" names that row's node and its kind, and answers the row's node. A key that moved
 *  nothing, or a move Details did not follow, fails with `where`. */
async function expectDetailsFollowsRow(
  page: Page,
  from: string | null,
  where: string
): Promise<string> {
  const seen: { row: string | null } = { row: null };
  await expect
    .poll(
      async () => {
        seen.row = await focusedNodeId(page);
        const named = await focusedNodeFacts(page).getAttribute("data-focused-node");
        if (seen.row === null) return "the focus is on no row";
        if (seen.row === from) return `the focus did not leave ${from}`;
        return seen.row === named
          ? "follows"
          : `the focus is on ${seen.row} and Details names ${named}`;
      },
      { message: where }
    )
    .toBe("follows");
  const at = seen.row ?? "";
  // The Verification view's first row is the issue's own summary, which carries no kind of node:
  // the focused row is then the issue itself, whose facts name its title after "Issue:".
  const type = await page.locator(rowSelector(at)).first().getAttribute("data-node-type");
  const kind = NODE_TYPE_NAMES[(type ?? "issue") as keyof typeof NODE_TYPE_NAMES];
  expect(
    kind,
    `${where}: the row of ${at} is of kind ${type}, which the page has no word for`
  ).toBeDefined();
  const named = `${CONTROL_LABELS.node_kind}:\\s*${sentenceCase(kind ?? "")}`;
  await expect(
    focusedNodeFacts(page),
    `${where}: Details does not name the kind of ${at}`
  ).toContainText(new RegExp(`(?:${named}|${sentenceCase(NODE_TYPE_NAMES.issue)}:)`, "u"));
  return at;
}

// R14 (INV-4, `VIEWS_FOCUS`): under every view the row that holds the focus is part F's focused
// node, as the outline's row is, so Details' facts of 'The row you are on' follow `j` and `k`, and
// Escape or `t c` leaves that node focused in the outline. Details draws a node's kind and
// whether it stands, and the issue's own title: no first line of a node, so the node and its kind
// are what the row reads.
test("Details follows the focused row of every view", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    for (const preset of VIEWS_FOCUS) {
      for (const leave of ["Escape", "t c"] as const) {
        const view = `the ${PRESET_NAMES[preset]} view, left by ${leave}`;
        await gotoIssue(page, presetPath(seeded.key, preset));
        await expectActiveControl(page, preset);
        await expect
          .poll(() => nodeRows(page).count(), { message: `${view} draws too few rows` })
          .toBeGreaterThan(3);
        await openDetails(page);
        await page.locator('[data-node-row][tabindex="0"]').first().focus();
        let at = await expectDetailsFollowsRow(page, null, `${view}, on its first row`);
        for (const key of ["j", "j", "k"]) {
          await page.keyboard.press(key);
          at = await expectDetailsFollowsRow(page, at, `${view}, after ${key}`);
        }
        if (leave === "Escape") {
          // Details, open beside the view, takes the first Escape as it takes its own; the view
          // is turned off by the next.
          await page.keyboard.press("Escape");
          await expect(marginShell(page), `Escape from ${view} leaves Details open`).toBeHidden();
          await page.keyboard.press("Escape");
        } else {
          await page.keyboard.press("t");
          await page.keyboard.press("c");
        }
        await expect(
          page.locator(`[data-issue-body] ${rowSelector(at)}`).first(),
          `${leave} from ${view} leaves no row of ${at} focused in the outline`
        ).toBeFocused();
      }
    }
  });
});

// Decision 396 (3): from an act of a focused row, Copy reference, Ctrl+K and then Escape give the
// focus back to the row that held the act, which went with the focus when the palette took it.
test("a dialog opened from a row's act gives the focus back to the row", async ({ browser }) => {
  await phase(browser, seedWholeIssue, async (page, seeded) => {
    await page.setViewportSize({ height: DESKTOP.height, width: DESKTOP.width });
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    await unfoldEveryBranch(page);
    const [thread = ""] = seeded.comments.threads;
    const row = await focusRow(page, thread);
    const copy = row.locator("[data-row-copy]");
    await expect(copy, "the focused row draws no Copy reference").toBeVisible();
    await copy.focus();
    await page.keyboard.press("ControlOrMeta+k");
    await expect(page.getByRole("dialog"), "Ctrl+K opens no dialog").toBeVisible();
    await expect(copy, "the act stayed drawn once the row lost the focus").toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog"), "Escape leaves the dialog open").toHaveCount(0);
    await expect(
      row,
      "the dialog's close left the focus off the row that held the act"
    ).toBeFocused();
  });
});

/** Holds the Verification view's reads (`slots/view-verification.tsx`: what serves each
 *  statement, the checks' results and the Verification document's blocks), as c-view-waiting's
 *  row holds them (`holdReads`), so the view draws its loading line in their place, until the
 *  function it answers lets them go. */
async function holdVerificationReads(
  page: Page,
  seeded: WholeIssueSeed
): Promise<() => Promise<void>> {
  const issue = `/api/v1/issues/${seeded.key}`;
  const held = await holdReads(page, [
    `${issue}/coverage`,
    `${issue}/results`,
    `/api/v1/artifacts/${seeded.verificationArtifactId}/blocks`,
  ]);
  return held.release;
}

/**
 * Opens `state` of `COLOUR_STATES` on R7's seed and waits until it is drawn whole (part N's R13):
 * Conversation, at `/conversation`, with Details open at the facts of the answered question and
 * what references it, its people's two lists, its Referenced by list, which holds the issue's
 * children, and its history; Dashboard, at the issue's bare address; Documents' list; and the
 * Verification view, whose reads the caller holds (`holdVerificationReads`), with its loading line.
 */
async function openColourState(
  page: Page,
  seeded: AdvancingIssueSeed,
  state: (typeof COLOUR_STATES)[number],
  where: string
): Promise<void> {
  if (state === "dashboard") {
    await gotoIssue(page, `/issues/${seeded.key}`);
    await expectDashboardDrawn(page, where);
    return;
  }
  if (state === "documents") {
    await gotoIssue(page, presetPath(seeded.key, "documents"));
    await expect(
      documentsRows(page).first(),
      `${where} draws no row of Documents' list`
    ).toBeVisible();
    return;
  }
  if (state === "loading") {
    await gotoIssue(page, presetPath(seeded.key, "verification"));
    const body = issuePage(page).locator("[data-verification-body]");
    await expect(
      body.getByRole("status").and(body.locator(VIEW_LOADING)),
      `${where} draws no loading line while the view's reads are out`
    ).toBeVisible();
    return;
  }
  const facts = await rowFacts(page, seeded.key, seeded.answeredAskId);
  const references = facts.locator("[data-node-references]");
  await references
    .getByRole("button", { exact: true, name: NODE_REFERENCES_WORDS.what_references })
    .click();
  await expect(
    references
      .getByRole("listitem")
      .or(references.getByText(CONTROL_LABELS.none, { exact: true }))
      .first(),
    `${where}: what references the answered question is never drawn`
  ).toBeVisible();
  const people = marginSection(page, "people");
  await people
    .getByRole("button", { name: new RegExp(`^${PROPERTY_LABELS.subscribers}`, "u") })
    .click();
  await expect(
    people.getByRole("region", { exact: true, name: "Subscribed agents" }),
    `${where}: Details draws no subscribed agent`
  ).toBeVisible();
  await people.getByRole("button", { exact: true, name: PROPERTY_LABELS.collaborators }).click();
  await expect(
    people.getByRole("region", { exact: true, name: PROPERTY_LABELS.collaborators }),
    `${where}: Details draws no list of collaborators`
  ).toBeVisible();
  const referencedBy = marginSection(page, "references").getByRole("button", {
    name: new RegExp(`^${PROPERTY_LABELS.referenced_by}`, "u"),
  });
  await referencedBy.click();
  await expect(
    page
      .locator(`[id="${await referencedBy.getAttribute("aria-controls")}"]`)
      .getByRole("listitem")
      .first(),
    `${where}: Referenced by lists none of the issue's children`
  ).toBeVisible();
  await expect(
    historyEntries(page).first(),
    `${where}: Details draws no entry of the history`
  ).toBeVisible();
}

// R13 (c-page-colours, INV-3): every colour an element of the new page or of Details computes -
// its text, its ground and each border it draws - is one of B's `PAGE_COLORS` in the scheme
// drawn, a transparent one, or one a person or an agent chose (`CHOSEN_COLOURS`, none), on R7's
// seed with the header's pull request link, in each of `COLOUR_STATES` at 1440 px and at 390 px,
// in light and in dark (`pageOffThePalette`). Each reading is soft, so one run names every
// element and colour off the palette in every state. `styles.css` eases a control's colours over
// 150 ms, so each load turns transitions off before its scheme changes, as
// `e2e/accessibility.e2e.ts` does, and the pointer rests in the window's top left corner, so no
// element is read in its hover colours.
// SLOW(>=2s): R7's seed, eight loads, four of Details' lists opened at each width, and sixteen
// readings of every element of the page and of Details.
test("every colour of the page and of Details is a page colour", async ({ browser }) => {
  // Not measured in a browser yet: eight loads, at the 1.7 s a load that "the filter row is one
  // line at every width" measured (27 loads in 47.0 s), and Details' four lists opened at each
  // width; the bound is several times that, for a slow leg.
  test.setTimeout(180_000);
  await phase(browser, seedWholeIssueAdvancing, async (page, seeded) => {
    await writePullRequestHead({
      event: "2026-11-10T10:00:00Z",
      issue: seeded.key,
      number: 101,
      state: "open",
      url: `https://github.com/${PULL_REQUEST_REPOSITORY}/pull/101`,
    });
    for (const size of [DESKTOP, PHONE]) {
      await page.setViewportSize({ height: size.height, width: size.width });
      for (const state of COLOUR_STATES) {
        const where = `${state} at ${size.width} px`;
        const release = state === "loading" ? await holdVerificationReads(page, seeded) : undefined;
        try {
          await openColourState(page, seeded, state, where);
          await expect(
            page.getByTestId("issue-pull-request"),
            `${where}: the header draws no pull request link`
          ).toBeVisible();
          await page.addStyleTag({
            content: "*, *::before, *::after { transition: none !important; }",
          });
          for (const scheme of ["light", "dark"] as const) {
            await page.emulateMedia({ colorScheme: scheme });
            await nextFrames(page);
            await page.mouse.move(0, 0);
            expect
              .soft(
                await pageOffThePalette(page, scheme),
                `${where} in the ${scheme} scheme draws a colour that is no page colour`
              )
              .toEqual([]);
          }
        } finally {
          await page.emulateMedia({ colorScheme: "light" });
          await release?.();
        }
      }
    }
  });
});
