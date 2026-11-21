import { SEND_DEADLINE_MS } from "../web/src/features/writes/keyed-write";
import {
  createAsk,
  createIssue,
  createIssueArtifact,
  createProject,
  getIssue,
  patchIssue,
  putIssueState,
  putRepoProject,
} from "./api";
import { filterPicker, pickFilterOption } from "./filters";
import { nextFrames } from "./frames";
import { pendingWrites } from "./keyed-writes";
import {
  detailRead,
  expectReadWhileHeld,
  holdIssuePatches,
  ISSUE_PAGE_PICKERS,
  lastEventSeq,
  logReads,
  pagePicker,
  readAnsweredSince,
  refuseIssuePatches,
  revealPagePicker,
  savesEnded,
  stopTrying,
  storedValues,
  watchShown,
} from "./pick-writes";
import { navigateInApp } from "./sends";
import { expect, rowKey, test } from "./test";
import { asUser } from "./users";

/**
 * The project page's one walk: nine issues across eight statuses, the table over them, eight
 * filter presses, and the Documents tab down to a document created in its editor. What the
 * page draws over one fixed answer - its fit on a phone, its long search, its reference
 * closure, its status groups and its rows - is in `project.stub.e2e.ts`.
 */

const session = { actor: { id: "e2e-project", kind: "session" as const }, as: "agent" as const };

// SLOW(>=2s): the project's table, status and label filters, and a created project document.
test("project table filters narrow issues; documents stay scoped to the project", async ({
  browser,
}, testInfo) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const triage = await createIssue({ project: core, title: "Triage work" });
  const icebox = await createIssue({ project: core, title: "Icebox work" });
  const backlog = await createIssue({ project: core, title: "Backlog work" });
  const parent = await createIssue({ project: core, title: "Parent work" });
  await createIssueArtifact(parent.key, {
    content: "# Issue-only document\n",
    name: "Issue-only document",
  });
  const needsYou = await createIssue({ project: core, title: "Needs attention" });
  const unread = await createIssue({ project: core, title: "Unread work" });
  const child = await createIssue({ parent: parent.key, project: core, title: "Child work" });
  const done = await createIssue({ project: core, title: "Done work" });
  await patchIssue(triage.key, { status: "triage" });
  await patchIssue(icebox.key, { status: "icebox" });
  await patchIssue(backlog.key, { status: "backlog" });
  await patchIssue(parent.key, { status: "todo" });
  await patchIssue(needsYou.key, { labels: ["frontend"], status: "todo" });
  await patchIssue(unread.key, { labels: ["backend"], status: "in_progress" });
  await patchIssue(child.key, { status: "testing" });
  await patchIssue(done.key, { status: "done" });
  await createAsk(needsYou.key, { question: "Does this need review?" }, session);
  // The negative control for the header's pill: another person's question in this same project.
  const bobs = await createIssue({ project: core, title: "Bob's work" }, { login: "bob" });
  await patchIssue(bobs.key, { status: "in_progress" });
  await createAsk(bobs.key, { question: "Does Bob need to answer this?" }, session);
  // Seven read marks, one round of round trips: the rows differ, the writes do not wait on
  // each other.
  await Promise.all(
    [triage, icebox, backlog, parent, needsYou, child, done].map(async (issue) => {
      const latest = await getIssue(issue.key);
      await putIssueState(issue.key, { last_read_seq: latest.last_seq });
    })
  );

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ width: 1280, height: 800 });

  try {
    await page.goto(`/projects/${core}`);
    await expect(page).toHaveTitle(`${core} · Core · Dispatch`);
    await expect(page.getByRole("heading", { name: "Core" })).toBeVisible();
    // The pill counts this project's questions waiting on the signed-in reader.
    await expect(
      page
        .locator(`[data-issue-row="${child.key}"]`)
        .getByRole("link", { name: parent.key, exact: true })
    ).toBeVisible();
    const blockedOnYou = page.getByRole("link", { name: "Blocked on you · 1" });
    const issueTable = page.getByRole("table", { name: "Project issues table" });
    await expect(issueTable).toBeVisible();
    expect((await page.getByTestId("issues-table-scroll").boundingBox())?.y).toBeLessThan(420);
    const main = page.getByTestId("main-content");
    await expect(blockedOnYou).toBeVisible();
    await expect(main).toBeVisible();
    // The waits above drew the pill and the main column, and a drawn element has a box.
    const blockerBox = await blockedOnYou.boundingBox();
    // The main column was waited for above too, so its box is there as the pill's is.
    const mainBox = await main.boundingBox();
    if (blockerBox === null || mainBox === null) {
      throw new Error("project header is not visible");
    }
    expect(blockerBox.x + blockerBox.width).toBeLessThanOrEqual(mainBox.x + mainBox.width);
    await expect(page.getByRole("tab", { name: "Issues" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    // Closed issues load when the Status filter lets Done through; the bar's own Done cell,
    // which only exists once they are loaded, then narrows to them.
    await pickFilterOption(page, "Status", "Done");
    await page.getByRole("button", { name: "Done 1", exact: true }).click();
    await expect(page.getByText("Done work", { exact: true })).toBeVisible();
    await expect(page.getByText("Child work", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "Testing 1", exact: true }).click();
    await expect(page.getByText("Child work", { exact: true })).toBeVisible();
    await expect(page.getByText("Backlog work", { exact: true })).toHaveCount(0);
    await pickFilterOption(page, "Status", "Triage");
    await expect(page.getByText("Triage work", { exact: true })).toBeVisible();
    await expect(page.getByText("Child work", { exact: true })).toBeVisible();
    await expect(page).toHaveURL(/status=testing.*status=triage/);
    await filterPicker(page, "Status").click();
    await page.getByRole("button", { name: "Clear status", exact: true }).click();
    await page.keyboard.press("Escape");
    await expect(page.getByText("Backlog work", { exact: true })).toBeVisible();
    await pickFilterOption(page, "Labels", "frontend");
    await expect(page.getByText("Needs attention", { exact: true })).toBeVisible();
    await expect(page.getByText("Backlog work", { exact: true })).toHaveCount(0);
    await filterPicker(page, "Labels").click();
    await page.getByRole("button", { name: "Clear labels", exact: true }).click();
    await page.keyboard.press("Escape");
    const search = page.getByRole("searchbox", { name: "Search issues" });
    await search.fill("backlog");
    await expect(page.getByText("Backlog work", { exact: true })).toBeVisible();
    await expect(page.getByText("Child work", { exact: true })).toHaveCount(0);
    await search.clear();
    await page.getByRole("button", { name: "Needs you", exact: true }).click();
    await expect(page.getByText("Needs attention", { exact: true })).toBeVisible();
    await expect(page.getByText("Backlog work", { exact: true })).toHaveCount(0);

    await page.getByRole("tab", { name: "Documents" }).click();
    await expect(page).toHaveURL(`/projects/${core}/documents`);
    await page.getByRole("button", { name: "New document" }).click();
    await page.getByRole("textbox", { name: "Title" }).fill("Design notes");
    await page.getByRole("button", { name: "Create" }).click();
    await expect(page).toHaveURL(`/projects/${core}/documents/design-notes`);
    // The document opens empty, under the title the person typed: the title is the page's
    // heading, and the editor holds nothing until the person writes in it.
    await expect(page.getByRole("heading", { name: "Design notes" })).toBeVisible();
    await expect(page.getByRole("textbox", { name: "Document editor" })).toHaveText("");
    await navigateInApp(page, `/projects/${core}/documents`);
    await expect(page.getByRole("link", { name: "Design notes", exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("project-documents.png"), fullPage: true });
    await expect(page.getByText("Issue-only document", { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The rows of dispatch://INFRA-299 on the issue page of an issue of the project: each pick of a
// priority or a repository is the page's write store's from its press, on the issue's line
// (`issue:<KEY>`), so the server stores the newest pick last, and while a pick is live the page
// shows it over every answer of a read. Each save is held by Playwright's route control at least
// `PICK_HOLD_MS` (`holdIssuePatches`), what the server received is read from the issue's own
// events (`storedValues`), and the value stored from a fresh read once every save has ended.

/** A project, and an issue of it at P3, which the server keeps while the row holds its saves:
 *  a value that reaches the screen from the server shows as P3, never one of the row's picks. */
async function issueAtP3(): Promise<string> {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Priority under a held save" });
  await patchIssue(issue.key, { priority: 3 });
  return issue.key;
}

// SLOW(>=2s): two saves each held three seconds, around a reload.
test("picker keeps the newest pick: a reload before the saves end sends both picks, and the server stores the last", async ({
  browser,
}) => {
  const key = await issueAtP3();
  const seq = await lastEventSeq(key);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const held = await holdIssuePatches(page, key);
    const picker = pagePicker(page, `Priority of ${key}`);
    await page.goto(`/issues/${key}`);
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => held.sent.length).toBe(1);
    // P1 is out, held: P2 waits its turn in the store's line, kept for a reload.
    await picker.selectOption("2");
    await expect(picker).toHaveValue("2");
    await page.reload();
    await expect(picker).toBeVisible();
    held.release();
    await savesEnded(page, held);

    expect(
      await storedValues(key, "priority", seq),
      `the server received the picks in this order; the route passed on ${JSON.stringify(held.forwarded)}`
    ).toEqual([1, 2]);
    expect(await getIssue(key)).toMatchObject({ priority: 2 });
    await expect(picker).toHaveValue("2");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): two saves each held three seconds, around four in-app navigations.
test("picker keeps the newest pick: a pick made after leaving and coming back is stored last, and the waiting pick it replaces is never sent", async ({
  browser,
}) => {
  const key = await issueAtP3();
  const other = await createIssue({
    project: rowKey("CORE"),
    title: "Another issue of the project",
  });
  const seq = await lastEventSeq(key);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const held = await holdIssuePatches(page, key);
    const picker = pagePicker(page, `Priority of ${key}`);
    await page.goto(`/issues/${key}`);
    await expect(picker).toHaveValue("3");

    await picker.selectOption("0");
    await expect.poll(() => held.sent.length).toBe(1);
    await picker.selectOption("1");
    await expect(picker).toHaveValue("1");

    // Another issue and back: the picker that made P1 is gone, and P2 is made on the one the
    // return mounts, while P0 is still out.
    await navigateInApp(page, `/issues/${other.key}`);
    await expect(pagePicker(page, `Priority of ${other.key}`)).toBeVisible();
    await navigateInApp(page, `/issues/${key}`);
    await expect(picker).toHaveValue("1");
    await picker.selectOption("2");
    await expect(picker).toHaveValue("2");
    expect(
      held.sent.map((patch) => patch.body),
      "P0 is still the only save out"
    ).toEqual([{ priority: 0 }]);
    held.release();
    await savesEnded(page, held);

    expect(
      await storedValues(key, "priority", seq),
      `the server received the picks in this order; the route passed on ${JSON.stringify(held.forwarded)}`
    ).toEqual([0, 2]);
    expect(await getIssue(key)).toMatchObject({ priority: 2 });
    await expect(picker).toHaveValue("2");
  } finally {
    await context.close();
  }
});

// Two picks made while one save is out are one request, the newest, and the badge shows the
// newest pick from each pick until every save has ended - through the moment the first save ends
// while the newest is still out.
// SLOW(>=2s): two saves each held three seconds.
test("picker keeps the newest pick: two picks made while a save is out send only the newest, which the badge shows until every save ends", async ({
  browser,
}) => {
  const key = await issueAtP3();
  const seq = await lastEventSeq(key);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${key}`, "main");
    const held = await holdIssuePatches(page, key);
    const picker = pagePicker(page, `Priority of ${key}`);
    await page.goto(`/issues/${key}`);
    await expect(picker).toHaveValue("3");
    await expect.poll(() => shown.values.at(-1)).toBe("3");
    const fromFirst = shown.values.length;

    // Each pick shows before the next is made, so the badge's record from the first pick on is
    // the newest pick at each moment, and nothing else.
    await picker.selectOption("1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    await expect.poll(() => held.sent.length).toBe(1);
    await picker.selectOption("2");
    await expect.poll(() => shown.values.at(-1)).toBe("2");
    await picker.selectOption("0");
    await expect.poll(() => shown.values.at(-1)).toBe("0");
    held.release();
    await savesEnded(page, held);

    expect(
      held.sent.map((patch) => patch.body),
      "P0 went out once P1's save had ended"
    ).toEqual([{ priority: 1 }, { priority: 0 }]);
    expect(
      shown.values.slice(fromFirst),
      "every value the badge showed from the pick of P1 until every save ended"
    ).toEqual(["1", "2", "0"]);

    expect(
      await storedValues(key, "priority", seq),
      `the server received the picks in this order; the route passed on ${JSON.stringify(held.forwarded)}`
    ).toEqual([1, 0]);
    expect(await getIssue(key)).toMatchObject({ priority: 0 });
    await expect(picker).toHaveValue("0");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a save held three seconds while the issue page reads the issue again.
test("picker keeps a pick on screen: the priority on the issue page, over a read that answers while its save is out", async ({
  browser,
}) => {
  const key = await issueAtP3();
  const renamed = "Priority under a held save, renamed while it is out";
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${key}`, "main");
    const held = await holdIssuePatches(page, key);
    const reads = logReads(page, detailRead(key));
    const picker = pagePicker(page, `Priority of ${key}`);
    await page.goto(`/issues/${key}`);
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // A rename of the issue, straight to the server: its live event has the page read the issue
    // again, and the answer, carrying P3, lands while the pick's save is held.
    const renamedAt = Date.now();
    await patchIssue(key, { title: renamed });
    await expect(page.getByRole("heading", { name: renamed })).toBeVisible();
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, renamedAt);

    expect(shown.values.slice(fromPick), "every value the page showed from the pick on").toEqual([
      "1",
    ]);
    expect(await getIssue(key)).toMatchObject({ priority: 1, title: renamed });
    await expect(picker).toHaveValue("1");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a save held three seconds while the issue page reads the issue again.
test("picker keeps a pick on screen: the repository on the issue page, over a read that answers while its save is out", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  await putRepoProject("acme/widgets", { default: true, project: core });
  await putRepoProject("acme/gadgets", { project: core });
  const issue = await createIssue({ project: core, title: "Repository under a held save" });
  await patchIssue(issue.key, { repo: "acme/widgets" });
  const renamed = "Repository under a held save, renamed while it is out";
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Repository of ${issue.key}`, ISSUE_PAGE_PICKERS);
    const held = await holdIssuePatches(page, issue.key);
    const reads = logReads(page, detailRead(issue.key));
    const picker = pagePicker(page, `Repository of ${issue.key}`);
    await page.goto(`/issues/${issue.key}`);
    // The repository is in `main` on the issue page with the sidebar, and in Details on that of
    // dispatch://INFRA-1196, which the reader opens to reach it.
    await revealPagePicker(page, `Repository of ${issue.key}`);
    await expect(picker).toHaveValue("acme/widgets");

    // The reader types the repository into the box and presses Enter, which writes it.
    await picker.fill("acme/gadgets");
    await picker.press("Enter");
    await expect.poll(() => shown.values.at(-1)).toBe("acme/gadgets");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // A rename of the issue, straight to the server: its live event has the page read the issue
    // again, and the answer, carrying acme/widgets, lands while the pick's save is held.
    const renamedAt = Date.now();
    await patchIssue(issue.key, { title: renamed });
    await expect(page.getByRole("heading", { name: renamed })).toBeVisible();
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, renamedAt);

    expect(shown.values.slice(fromPick), "every value the page showed from the pick on").toEqual([
      "acme/gadgets",
    ]);
    expect(await getIssue(issue.key)).toMatchObject({ repo: "acme/gadgets", title: renamed });
    await expect(picker).toHaveValue("acme/gadgets");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): two saves each held three seconds, around a reload.
test("picker keeps a pick on screen: a reload shows the newest pick from its first paint until the saves end", async ({
  browser,
}) => {
  const key = await issueAtP3();
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${key}`, "main");
    const held = await holdIssuePatches(page, key);
    const reads = logReads(page, detailRead(key));
    const picker = pagePicker(page, `Priority of ${key}`);
    await page.goto(`/issues/${key}`);
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => held.sent.length).toBe(1);
    await picker.selectOption("2");
    await expect.poll(() => shown.values.at(-1)).toBe("2");
    const beforeReload = shown.values.length;
    const reloadedAt = Date.now();
    await page.reload();
    await expect(picker).toBeVisible();
    held.release();
    await savesEnded(page, held);

    // The reloaded page's first read of the issue was answered while the P1 it sent again was
    // held: that answer, carrying P3, landed under the newest pick.
    const timeline = JSON.stringify({ reads, reloadedAt, saves: held.sent });
    const resent = held.sent.find(
      (patch) =>
        patch.arrivedAt >= reloadedAt &&
        JSON.stringify(patch.body) === JSON.stringify({ priority: 1 })
    );
    const firstRead = reads.find((read) => read.sentAt >= reloadedAt);
    expect(resent, `the reloaded page sent P1 again: ${timeline}`).toBeDefined();
    expect(
      firstRead?.answeredAt !== undefined &&
        resent !== undefined &&
        resent.endedAt !== undefined &&
        firstRead.answeredAt >= resent.arrivedAt &&
        firstRead.answeredAt <= resent.endedAt,
      `the first read of the issue after the reload was answered while the resent P1 was held: ${timeline}`
    ).toBe(true);

    // From the reloaded document's first frame: no picker until the page draws one, and then P2
    // alone, the pick the store sends last.
    expect(
      [...new Set(shown.values.slice(beforeReload).filter((value) => value !== "(none)"))],
      `every value the badge showed from the reload on: ${JSON.stringify(shown.values.slice(beforeReload))}`
    ).toEqual(["2"]);
    expect(await getIssue(key)).toMatchObject({ priority: 2 });
    await expect(picker).toHaveValue("2");
  } finally {
    await context.close();
  }
});

// A pick Dispatch keeps answering `503` passes the write store's deadline and is still tried,
// under its key, each second: it is late, and the shell offers Stop trying for it. A late pick is
// as live as one within its deadline, so a read of the issue answered while it is late, carrying
// P3, never reaches the badge, and the pick is stored once a save of it gets through. The page's
// own clock runs the pick past its deadline (`SEND_DEADLINE_MS`), and runs on at the wall clock's
// pace around that jump.
// SLOW(>=2s): the store waits a second between attempts (`Retry-After: 1`), and the row waits for
// one attempt after the read is answered and for the first one after `admit`.
test("a pick that is late and still being tried stays on screen over a read, and is stored once a save gets through", async ({
  browser,
}) => {
  const key = await issueAtP3();
  const seq = await lastEventSeq(key);
  const renamed = "Priority under a late save, renamed while it is late";
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${key}`, "main");
    const busy = await refuseIssuePatches(page, key);
    const reads = logReads(page, detailRead(key));
    const picker = pagePicker(page, `Priority of ${key}`);
    const late = stopTrying(page, `${key} set to P1`);
    await page.clock.install();
    await page.goto(`/issues/${key}`);
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    // The store has tried the pick, so its deadline runs: the page's clock jumps past it.
    await expect.poll(() => busy.refused.length, "the store tried the pick").toBeGreaterThan(0);
    await page.clock.fastForward(SEND_DEADLINE_MS + 1);
    await expect(
      late,
      "the shell offers Stop trying once the pick is past its deadline"
    ).toBeVisible();
    // A rename of the issue, straight to the server: its live event has the page read the issue
    // again, and the answer, carrying P3, lands while the pick is late.
    const renamedAt = Date.now();
    await patchIssue(key, { title: renamed });
    await expect(page.getByRole("heading", { name: renamed })).toBeVisible();
    await expect
      .poll(() => readAnsweredSince(reads, renamedAt), "the page read the issue after the rename")
      .toBe(true);
    await expect(late, "the pick was still late once that read was answered").toBeVisible();
    const answeredBy = Date.now();
    await expect
      .poll(() => busy.refused.some((at) => at >= answeredBy), "the store still tries the pick")
      .toBe(true);
    // The answer is drawn by now, and the badge read at the frames after it.
    await nextFrames(page);
    expect(
      shown.values.slice(fromPick),
      "every value the badge showed from the pick on, while it was late"
    ).toEqual(["1"]);

    busy.admit();
    await expect.poll(async () => (await getIssue(key)).priority).toBe(1);
    await expect(pendingWrites(page)).toHaveCount(0);
    expect(
      await storedValues(key, "priority", seq),
      `the server stored the pick once; the route passed on ${JSON.stringify(busy.forwarded)}`
    ).toEqual([1]);
    expect(await getIssue(key)).toMatchObject({ priority: 1, title: renamed });
    await expect(picker).toHaveValue("1");
    expect(shown.values.slice(fromPick), "every value the badge showed from the pick on").toEqual([
      "1",
    ]);
  } finally {
    await context.close();
  }
});
