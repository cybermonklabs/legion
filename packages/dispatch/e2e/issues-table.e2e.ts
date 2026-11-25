import type { Issue } from "../web/src/api/types";
import { SEND_DEADLINE_MS } from "../web/src/features/writes/keyed-write";
import { MARK_ATTRIBUTE } from "../web/src/hooks/useFootInset";
import { type FakeSession, setLiveSessions, setSessionLive } from "./agents";
import {
  createAgentMessage,
  createAsk,
  createIssue,
  createMessage,
  createPriority,
  createProject,
  getIssue,
  listIssueMessages,
  listIssues,
  patchIssue,
  replyToMessageDelivery,
  seedAgentStreamConversations,
} from "./api";
import { filterPicker } from "./filters";
import { openWithFloats, routeFloats, seedRefusedMove, updateBanner } from "./foot-floats";
import { nextFrames } from "./frames";
import { inboxPane, paneScroller, selectedRow } from "./inbox-pane";
import { APPROVAL_FOOT, inboxAt, seedApprovalPlace } from "./inbox-pane-scroll";
import { dropFirstAnswer, pendingWrites, watchWrites } from "./keyed-writes";
import { type Box, boxesApart } from "./layout";
import { expectConversationInFront } from "./live-view-cost";
import { frontLiveView } from "./live-views";
import {
  expectReadWhileHeld,
  holdIssuePatches,
  listRead,
  logReads,
  pagePicker,
  readAnsweredSince,
  savesEnded,
  watchCell,
  watchShown,
} from "./pick-writes";
import { navigateInApp } from "./sends";
import { expect, type Locator, type Page, test } from "./test";
import { asUser } from "./users";

test("the table sorts, searches facets and fuzzy text, groups, pages and receives a second page's edit live", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Table epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const rows = [];
  for (let index = 0; index < 30; index++) {
    const row = await createIssue({
      project: "CORE",
      parent: epic.key,
      title: `Searchable table work ${String(index).padStart(2, "0")}`,
    });
    rows.push(
      await patchIssue(row.key, {
        status: "todo",
        priority: ([0, 1, 2, 3] as const)[index % 4],
        labels: ["legion", "frontend"],
      })
    );
  }
  const backend = await createIssue({ project: "CORE", title: "Backend unrelated" });
  await patchIssue(backend.key, { labels: ["backend"] });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1920, height: 1080 });
    await page.goto("/projects/CORE/issues");
    const table = page.getByRole("table", { name: "Project issues table" });
    await expect(table).toBeVisible();
    const titleHeader = table
      .getByRole("columnheader")
      .filter({ has: page.getByRole("button", { name: /^Title(?: ▲| ▼)?$/ }) });
    await titleHeader.getByRole("button", { name: /^Title/ }).click();
    await expect(titleHeader).toHaveAttribute("aria-sort", "ascending");
    await expect(page.locator("[data-issue-row]").first()).toContainText("Backend unrelated");
    await titleHeader.getByRole("button", { name: /^Title/ }).click();
    await expect(titleHeader).toHaveAttribute("aria-sort", "descending");
    // The epic is an ordinary row of the table, so it sorts with the rest.
    await expect(page.locator("[data-issue-row]").first()).toContainText("Table epic");

    await filterPicker(page, "Labels").click();
    await page.getByRole("combobox", { name: "Search Labels" }).fill("front");
    await expect(page.getByRole("option", { name: /^frontend,/ })).toBeVisible();
    await expect(page.getByRole("option", { name: /^backend,/ })).toHaveCount(0);
    await page.getByRole("option", { name: /^frontend,/ }).click();
    await page.keyboard.press("Escape");
    await expect(page.getByText("Backend unrelated", { exact: true })).toHaveCount(0);
    await expect(page.getByText("30 of 32 issues", { exact: true })).toBeVisible();

    const search = page.getByRole("searchbox", { name: "Search issues" });
    await search.fill("searchble work");
    await expect(page.getByText("30 of 32 issues", { exact: true })).toBeVisible();
    await search.fill("searchble impossible");
    await expect(page.getByText("No issues match these filters.")).toBeVisible();
    await search.fill("searchble work");
    await page.getByRole("combobox", { name: "Group by" }).selectOption("epic");
    await expect(page.getByText(new RegExp(`${epic.key} · Table epic`)).first()).toBeVisible();
    await expect(page.getByText(/next in queue #1/)).toBeVisible();
    await page.getByRole("combobox", { name: "Rows per page" }).selectOption("25");
    await page.getByRole("button", { name: "Next ›", exact: true }).click();
    await expect(page.getByText("Page 2 of 2", { exact: true })).toBeVisible();
    await expect(page.getByText("(continued)", { exact: true })).toBeVisible();
    await page.reload();
    await expect(page.getByText("Page 2 of 2", { exact: true })).toBeVisible();
    await expect(search).toHaveValue("searchble work");
    await expect(page.getByRole("combobox", { name: "Group by" })).toHaveValue("epic");

    // A different browser page makes an ordinary issue edit. The first page is never navigated
    // or reloaded after the edit; its existing live subscription must update the visible row.
    const targetKey = await page.locator("[data-issue-row]").first().getAttribute("data-issue-row");
    expect(targetKey).not.toBeNull();
    const target = rows.find((row) => row.key === targetKey);
    if (!target) throw new Error(`Visible issue ${targetKey} was not seeded`);
    const targetSelection = page.getByRole("checkbox", {
      name: `Select ${target.key}`,
      exact: true,
    });
    await targetSelection.check();
    const rowTop = await page
      .locator(`[data-issue-row="${target.key}"]`)
      .evaluate((element) => element.getBoundingClientRect().top);
    const second = await context.newPage();
    await second.goto(`/issues/${target.key}`);
    await expect(second.getByRole("heading", { level: 1, name: target.title })).toBeVisible();
    const priority = second.getByRole("combobox", { name: `Priority of ${target.key}` });
    // The select is held to the priority the seed gave the issue before the edit picks the other
    // of 0 and 1, so the select read is one the page has drawn rather than a first empty value.
    const before = String(target.priority ?? "");
    await expect(priority).toHaveValue(before);
    const next = before === "0" ? "1" : "0";
    await priority.selectOption(next);
    await expect
      .poll(() => getIssue(target.key).then((issue) => issue.priority))
      .toBe(Number(next));
    await expect(
      page
        .locator(`[data-issue-row="${target.key}"]`)
        .getByRole("combobox", { name: `Priority of ${target.key}` })
    ).toHaveValue(next);
    await expect(page.getByText("Page 2 of 2", { exact: true })).toBeVisible();
    await expect(targetSelection).toBeChecked();
    expect(
      await page
        .locator(`[data-issue-row="${target.key}"]`)
        .evaluate((element) => element.getBoundingClientRect().top)
    ).toBe(rowTop);
    await page.screenshot({ path: testInfo.outputPath("issues-table-live.png"), fullPage: true });
    await page.getByRole("button", { name: "Clear selection", exact: true }).click();

    await page.getByRole("combobox", { name: "Group by" }).selectOption("");
    await page.getByRole("combobox", { name: "Rows per page" }).selectOption("50");
    const selected = rows[25];
    if (!selected) throw new Error("The queue seed did not create 26 issues");
    await page.getByRole("checkbox", { name: `Select ${selected.key}`, exact: true }).check();
    const scrollTop = await page
      .getByTestId("issues-table-scroll")
      .evaluate((element) => element.getBoundingClientRect().top);
    await page
      .getByRole("toolbar")
      .getByRole("button", { name: "Start next", exact: true })
      .click();
    await expect(
      page.getByRole("region", { name: "Project issues", exact: true }).getByRole("status")
    ).toContainText("now starts next");
    await expect
      .poll(() => getIssue(selected.key).then((issue) => issue.rank))
      .not.toBe(selected.rank);
    expect(
      await page
        .getByTestId("issues-table-scroll")
        .evaluate((element) => element.getBoundingClientRect().top)
    ).toBe(scrollTop);
    await expect(page.locator(`[data-issue-row="${selected.key}"]`)).toHaveAttribute(
      "data-moved",
      "true"
    );
    // The shared label parameter remains readable by the Board strip.
    await page.getByRole("button", { name: "Board", exact: true }).click();
    await expect(page.getByRole("button", { name: "Remove Label: frontend filter" })).toBeVisible();
    await page.getByRole("button", { name: "List", exact: true }).click();
    await expect(filterPicker(page, "Labels")).toHaveAttribute(
      "aria-label",
      "Filter Labels: 1 picked"
    );
  } finally {
    await context.close();
  }
});

test("merging an epic moves open descendants through closed intermediates and preserves closed history", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = await createIssue({ project: "CORE", title: "Source epic" });
  const target = await createIssue({ project: "CORE", title: "Target epic" });
  await patchIssue(source.key, { labels: ["epic"] });
  await patchIssue(target.key, { labels: ["epic"] });
  const open = await createIssue({ project: "CORE", parent: source.key, title: "Open child" });
  const closed = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Completed child",
  });
  const grandchild = await createIssue({
    project: "CORE",
    parent: closed.key,
    title: "Open below completed work",
  });
  await createMessage(closed.key, { body: "Completed this work." });
  const closedBefore = await patchIssue(closed.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.goto("/projects/CORE/issues?group=epic");
    const group = page
      .getByRole("row")
      .filter({ has: page.getByRole("button", { name: /Source epic/ }) });
    await group.getByRole("button", { name: "Epic actions ▾" }).click();
    await page.getByRole("combobox", { name: "Merge into another epic" }).selectOption(target.key);
    await page.getByRole("button", { name: "Merge…" }).click();
    await page
      .getByRole("dialog", { name: "Merge this epic?" })
      .getByRole("button", { name: "Merge", exact: true })
      .click();
    await expect.poll(() => getIssue(source.key).then((issue) => issue.status)).toBe("done");
    expect((await getIssue(open.key)).parent).toBe(target.key);
    expect((await getIssue(grandchild.key)).parent).toBe(target.key);
    const closedAfter = await getIssue(closed.key);
    expect(closedAfter.parent).toBe(source.key);
    expect(closedAfter.closed_at).toBe(closedBefore.closed_at);
    expect((await listIssueMessages(source.key)).map((read) => read.message.body)).toContain(
      `Merged into ${target.key} (“${target.title}”) from the issues table.`
    );
  } finally {
    await context.close();
  }
});

test("selection actions move, remove and create an epic, whose name can then change", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const target = await createIssue({ project: "CORE", title: "Existing container" });
  await patchIssue(target.key, { labels: ["epic"] });
  const first = await createIssue({ project: "CORE", title: "Fix authentication cookies" });
  const second = await createIssue({ project: "CORE", title: "Audit CSS backgrounds" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.goto("/projects/CORE/issues");
    const select = async (key: string) =>
      page.getByRole("checkbox", { name: `Select ${key}`, exact: true }).check();
    await select(first.key);
    await select(second.key);
    await page
      .getByRole("combobox", { name: "Move to epic", exact: true })
      .selectOption(target.key);
    await page.getByRole("button", { name: "Move", exact: true }).click();
    await expect
      .poll(async () => [(await getIssue(first.key)).parent, (await getIssue(second.key)).parent])
      .toEqual([target.key, target.key]);
    await expect(page.getByRole("toolbar")).toHaveCount(0);
    await select(first.key);
    await page.getByRole("button", { name: "Take out of epic" }).click();
    await expect.poll(() => getIssue(first.key).then((issue) => issue.parent)).toBeNull();
    await expect(page.getByRole("toolbar")).toHaveCount(0);
    await select(first.key);
    await select(second.key);
    await page.getByRole("button", { name: "New epic from these" }).click();
    await page
      .getByRole("dialog", { name: "Name the new epic" })
      .getByRole("textbox", { name: "Name" })
      .fill("Coordinate onboarding release");
    await page.getByRole("button", { name: "Create epic", exact: true }).click();
    await expect.poll(() => getIssue(first.key).then((issue) => issue.parent)).not.toBeNull();
    const epicKey = (await getIssue(first.key)).parent;
    if (!epicKey) throw new Error("The new epic was not assigned");
    await expect.poll(() => getIssue(second.key).then((issue) => issue.parent)).toBe(epicKey);
    expect((await getIssue(epicKey)).labels).toContain("epic");
    await page.getByRole("combobox", { name: "Group by" }).selectOption("epic");
    const group = page
      .getByRole("row")
      .filter({ has: page.getByRole("button", { name: /Coordinate onboarding release/ }) });
    await group.getByRole("button", { name: "Epic actions ▾" }).click();
    await page.getByRole("button", { name: "Rename", exact: true }).click();
    await page
      .getByRole("dialog", { name: "Rename the epic" })
      .getByRole("textbox", { name: "Name" })
      .fill("Release coordination");
    await page.getByRole("button", { name: "Rename epic", exact: true }).click();
    await expect
      .poll(() => getIssue(epicKey).then((issue) => issue.title))
      .toBe("Release coordination");
  } finally {
    await context.close();
  }
});

// R24 (INFRA-4731, D12): New epic over an issue in work asks, before it makes the epic, to move
// the new epic into work too, since an epic stays in work while an issue under it is. The yes
// makes the epic, moves it into work, and then moves the issue under it.
test("New epic over an issue in progress, confirmed, leaves the epic in progress with the issue under it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const working = await createIssue({ project: "CORE", title: "Ship the parser" });
  await patchIssue(working.key, { status: "in_progress" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.goto("/projects/CORE/issues");
    await page.getByRole("checkbox", { name: `Select ${working.key}`, exact: true }).check();
    await page.getByRole("button", { name: "New epic from these" }).click();
    await page
      .getByRole("dialog", { name: "Name the new epic" })
      .getByRole("textbox", { name: "Name" })
      .fill("Parser rollout");
    await page.getByRole("button", { name: "Create epic", exact: true }).click();
    const question = page.getByRole("dialog", { name: "Move “Parser rollout” into work too?" });
    await expect(question.getByRole("list", { name: "Changes" })).toHaveText(
      "“Parser rollout” moves to In progress"
    );
    // The question comes before the create: the project holds no epic yet.
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([working.key]);
    await question.getByRole("button", { name: "Move into work too" }).click();
    await expect.poll(() => getIssue(working.key).then((issue) => issue.parent)).not.toBeNull();
    const after = await getIssue(working.key);
    if (!after.parent) throw new Error("The new epic did not take the issue");
    expect(after.status).toBe("in_progress");
    expect(await getIssue(after.parent)).toMatchObject({
      labels: ["epic"],
      parent: null,
      status: "in_progress",
      title: "Parser rollout",
    });
  } finally {
    await context.close();
  }
});

// A refused write leaves Pending changes open at the bottom of the window, as the issues table's
// Finish flows do. The selection bar's controls must still take a press while it is open: at
// 1600x1000 the panel lay over "New epic from these" and "Clear selection".
test("with a refused change pending, New epic from these and Clear selection still take a press @foot", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Parser rollout" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const refused = await createIssue({ project: "CORE", parent: epic.key, title: "Refused move" });
  const other = await createIssue({ project: "CORE", title: "Ship the parser" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.route(`**/api/v1/issues/${refused.key}`, async (route) => {
      if (route.request().method() === "PATCH")
        await route.fulfill({ status: 403, json: { error: "Move refused" } });
      else await route.continue();
    });
    await page.goto("/projects/CORE/issues");
    const select = (key: string) =>
      page.getByRole("checkbox", { name: `Select ${key}`, exact: true });
    await select(refused.key).check();
    await page.getByRole("button", { name: "Take out of epic" }).click();
    await expect(page.getByRole("region", { name: "Pending changes" })).toContainText(refused.key);
    await select(refused.key).setChecked(false);
    await select(other.key).check();
    // A press that another element takes fails here, where Playwright reads the element at the
    // control's point, rather than hanging until the test's own timeout.
    await page.getByRole("button", { name: "New epic from these" }).click({ timeout: 5_000 });
    const naming = page.getByRole("dialog", { name: "Name the new epic" });
    await naming.getByRole("button", { name: "Cancel" }).click();
    await expect(naming).toHaveCount(0);
    await page.getByRole("button", { name: "Clear selection" }).click({ timeout: 5_000 });
    await expect(select(other.key)).not.toBeChecked();
  } finally {
    await context.close();
  }
});

/** The issues table's rows, each with its checkbox; its box draws no other row that has one. */
const TABLE_ROWS = 'tbody tr:has(input[type="checkbox"])';

/** The three desktop sizes the foot rows read at, from the smallest up. */
const DESKTOP_SIZES: readonly { width: number; height: number }[] = [
  { width: 1280, height: 720 },
  { width: 1600, height: 1000 },
  { width: 1920, height: 1080 },
];

/** A box of the page and the name a problem calls it by. */
type NamedBox = { readonly name: string; readonly box: Box };

/**
 * A scroller under the foot's floats (`web/src/hooks/useFootInset.ts`), as a row walks it to the
 * end of its scroll: the name a problem calls it by and the name of its last content; the
 * element that scrolls, `html` where the document scrolls the content; and, where its last
 * content is a row, the selector of its rows. Otherwise its last content is its last element
 * child that has a box.
 */
interface Scroller {
  readonly name: string;
  readonly last: string;
  readonly element: Locator;
  readonly rows?: string;
}

/** The issues table's box, as a row walks it to the end of its scroll: its last content is its
 *  last row (`TABLE_ROWS`). */
function tableScroller(page: Page): Scroller {
  return {
    element: page.getByTestId("issues-table-scroll"),
    last: "the table's last row",
    name: "the table's box",
    rows: TABLE_ROWS,
  };
}

/** A scroller at the end of its scroll: its box, which is the window's where the document
 *  scrolls, and the box of its last content, or null where none lies inside its box. */
interface EndReading {
  readonly box: Box;
  readonly last: Box | null;
}

/**
 * Reads `scroller` at the end of its scroll, or answers null where it is not there, and then sets
 * its `scrollTop` to its `scrollHeight`, so the next reading is made at the end as it then
 * stands. It is at its end where `scrollHeight - scrollTop - clientHeight` is at most 1 on two
 * reads a frame apart, so a row that a virtualized scroller draws in the frame after a scroll is
 * drawn by the time it is read.
 *
 * A virtualized scroller draws rows past its visible edge, and the box of such a row is one
 * nobody sees: it meets a float or not by where it lands. So the last content is read only where
 * its box lies inside the scroller's. A row counts only where it lies wholly inside, and the last
 * such row is read. A last element child counts where its lower edge lies inside, and it is read
 * clipped to the scroller's box.
 */
function readEnd(scroller: Scroller): Promise<EndReading | null> {
  return scroller.element.evaluate(async (element, rows) => {
    const gap = () => element.scrollHeight - element.scrollTop - element.clientHeight;
    const first = gap();
    const frame = Promise.withResolvers<void>();
    requestAnimationFrame(() => frame.resolve());
    await frame.promise;
    if (first > 1 || gap() > 1) {
      element.scrollTop = element.scrollHeight;
      return null;
    }
    const boxOf = ({ height, width, x, y }: DOMRect): Box => ({ height, width, x, y });
    const box =
      element === document.documentElement
        ? { height: element.clientHeight, width: element.clientWidth, x: 0, y: 0 }
        : boxOf(element.getBoundingClientRect());
    const bottom = box.y + box.height;
    if (rows !== undefined) {
      const inside = [...element.querySelectorAll(rows)]
        .map((row) => row.getBoundingClientRect())
        .filter((row) => row.height > 0 && row.top >= box.y && row.bottom <= bottom + 1);
      const last = inside.at(-1);
      return { box, last: last === undefined ? null : boxOf(last) };
    }
    const drawn = [...element.children].filter((child) => child.getClientRects().length > 0);
    const last = drawn.at(-1)?.getBoundingClientRect();
    if (last === undefined || last.bottom > bottom + 1 || last.bottom <= box.y) {
      return { box, last: null };
    }
    const top = Math.max(last.top, box.y);
    const left = Math.max(last.left, box.x);
    const right = Math.min(last.right, box.x + box.width);
    return { box, last: { height: last.bottom - top, width: right - left, x: left, y: top } };
  }, scroller.rows);
}

/** Scrolls `scroller` to the real end of its scroll, as `readEnd` reads it. It sets `scrollTop`
 *  to `scrollHeight` first, so the first reading does not find a scroller still at its start. */
async function scrollToItsEnd(scroller: Scroller): Promise<void> {
  await scroller.element.evaluate((element) => {
    element.scrollTop = element.scrollHeight;
  });
  await expect(async () => {
    expect(await readEnd(scroller), `${scroller.name} did not reach its end`).not.toBeNull();
  }).toPass({ timeout: 15_000 });
}

/**
 * What is wrong at the end of `scroller`'s scroll at the size `at`, from `reading`, `readEnd`'s
 * reading there: that the scroller is not at its end or that no last content lies inside its
 * box, which is the same thing, and never a pass; or that its last content meets one of
 * `floats`, with where each lies.
 */
function endProblems(
  scroller: Scroller,
  reading: EndReading | null,
  floats: readonly NamedBox[],
  at: string
): string[] {
  if (reading === null || reading.last === null) {
    return [`${scroller.name} did not reach its end at ${at}`];
  }
  const { box, last } = reading;
  const span = ({ height, y }: Box) => `y ${Math.round(y)}-${Math.round(y + height)}`;
  return floats
    .filter((float) => !boxesApart(last, float.box))
    .map(
      (float) =>
        `${scroller.last} meets ${float.name} at ${at}: it lies at ${span(last)}, ` +
        `${float.name} at ${span(float.box)}, and ${scroller.name} ends at ` +
        `y ${Math.round(box.y + box.height)}`
    );
}

/** A control of a foot element, under a name the row gives it rather than one it reads off the
 *  page. With `every`, it stands for each control the locator matches, numbered in the order the
 *  page draws them, since an element shows as many of those as it holds. */
type FootControl = { name: string; control: Locator; every?: true };

/** One element at the foot of the window: its name, its box, and every control it shows.
 *  `float` marks a float of the foot (a layer above the page's), which the last content of a
 *  scroll has to clear. */
type FootElement = { name: string; box: Locator; controls: FootControl[]; float?: true };

/** The end of a scroll a reading is made at: the scroller, and the controls of its last content,
 *  whose centre nothing may cover. */
type ScrollEnd = { scroller: Scroller; controls: FootControl[] };

/** A control for each of `names`: the button of `scope` so named. */
function footButtons(scope: Locator, names: string[]): FootControl[] {
  return names.map((name) => ({ name, control: scope.getByRole("button", { name, exact: true }) }));
}

/** The two floats of `page`'s foot that the rows below show: Pending changes, which a refused
 *  change leaves open, and the update banner. */
function footFloats(page: Page): { pendingChanges: FootElement; update: FootElement } {
  const pending = pendingWrites(page);
  const banner = updateBanner(page);
  return {
    pendingChanges: {
      name: "Pending changes",
      box: pending,
      controls: footButtons(pending, ["Retry", "Discard"]),
      float: true,
    },
    update: {
      name: "the update banner",
      box: banner,
      controls: footButtons(banner, ["Reload", "Dismiss update notice"]),
      float: true,
    },
  };
}

/** The box each of `elements` has on the page, with its name and whether it is a float. One that
 *  has none is left out, and is a problem of its own at the size `at`, pushed to `problems`. */
async function footBoxes(
  elements: readonly FootElement[],
  at: string,
  problems: string[]
): Promise<{ box: Box; float?: true; name: string }[]> {
  const boxes: { box: Box; float?: true; name: string }[] = [];
  for (const { box, float, name } of elements) {
    // Each caller reads the boxes inside an `expect.poll`, which takes its whole reading again
    // until it holds, so a box missing at one instant fails only that reading.
    const found = await box.boundingBox({ timeout: 1_000 }).catch(() => null);
    if (found === null) problems.push(`${name} has no box at ${at}`);
    else boxes.push({ box: found, float, name });
  }
  return boxes;
}

/** Both floats are gone, so nothing lies over content, and no scroller keeps the end room it took
 *  for them. */
async function expectNoEndRoom(page: Page): Promise<void> {
  await expect(page.locator(`[${MARK_ATTRIBUTE}]`)).toHaveCount(0);
}

/**
 * Fails unless, at each of `sizes`, no two of the elements `shown` on `page` meet and nothing
 * covers the centre of a control of theirs. Where `end` is given, its scroller is scrolled to the
 * real end of its scroll at each size, each reading is made at that end, and there the
 * scroller's last content meets no float shown and nothing covers the centre of a control of
 * `end.controls`. The scroller is then left at its start again, where the rows the row's next
 * press picks are drawn. Each size is one reading, taken again until it holds: the list of what
 * is wrong there.
 */
async function expectFootClear(
  page: Page,
  shown: FootElement[],
  sizes: { width: number; height: number }[],
  end?: ScrollEnd
): Promise<void> {
  for (const size of sizes) {
    await page.setViewportSize(size);
    if (end !== undefined) await scrollToItsEnd(end.scroller);
    const at = `${size.width}x${size.height}`;
    await expect
      .poll(
        async () => {
          const problems: string[] = [];
          const boxes = await footBoxes(shown, at, problems);
          for (const [index, first] of boxes.entries()) {
            for (const second of boxes.slice(index + 1)) {
              if (!boxesApart(first.box, second.box)) {
                problems.push(`${first.name} meets ${second.name} at ${at}`);
              }
            }
          }
          // The end room that lets the last content scroll clear of the floats can grow a frame
          // or more after a scroll to the end, as the floats move under the content the scroll
          // brings, which leaves the scroller short of its new end. So each reading is made at
          // the end as it then stands: `readEnd` reads a scroller short of it as a problem and
          // scrolls it there for the next reading.
          if (end !== undefined) {
            const floats = boxes.filter(({ float }) => float === true);
            problems.push(...endProblems(end.scroller, await readEnd(end.scroller), floats, at));
          }
          const last =
            end === undefined ? [] : [{ controls: end.controls, name: end.scroller.last }];
          for (const element of [...shown, ...last]) {
            for (const { control: matching, every, name: named } of element.controls) {
              const controls =
                every === true
                  ? (await matching.all()).map((control, index) => ({
                      control,
                      name: `${named} ${index + 1}`,
                    }))
                  : [{ control: matching, name: named }];
              for (const { control, name } of controls) {
                const label = `${name} in ${element.name}`;
                // The short timeout lets a control missing from the page fail this reading and
                // leave the next one to the poll, rather than hold the poll whole.
                const covered = await control
                  .evaluate(
                    (target) => {
                      // A control the reader scrolls to, such as one past the end of the
                      // selection bar, which scrolls sideways on a phone, is read where that
                      // scroll brings it, as a press brings it. A control already in view moves
                      // nothing.
                      target.scrollIntoView({ block: "nearest", inline: "nearest" });
                      const { height, left, top, width } = target.getBoundingClientRect();
                      const hit = document.elementFromPoint(left + width / 2, top + height / 2);
                      if (hit === null) return "has its centre off the screen";
                      if (target.contains(hit)) return null;
                      const tag = hit.tagName.toLowerCase();
                      const holder = hit.closest("[aria-label], [data-testid]");
                      if (holder === null) return `is covered by ${tag}`;
                      const called =
                        holder.getAttribute("aria-label") ?? holder.getAttribute("data-testid");
                      const owner = `${holder.tagName.toLowerCase()} "${called}"`;
                      return `is covered by ${holder === hit ? owner : `${tag} in ${owner}`}`;
                    },
                    undefined,
                    { timeout: 1_000 }
                  )
                  .catch((error: Error) => `cannot be read: ${error.message.split("\n")[0]}`);
                if (covered !== null) problems.push(`${label} ${covered} at ${at}`);
              }
            }
          }
          return problems;
        },
        { message: `the foot of the window at ${at}` }
      )
      .toEqual([]);
  }
  await end?.scroller.element.evaluate((element) => {
    element.scrollTop = 0;
  });
}

// The selection bar from the sm width up, Pending changes and the update banner all sit at the
// foot of the window, each in a layer of the foot (`web/src/hooks/useFootInset.ts`), so none
// lies over another; below sm the bar sits under the header. On the issues table the row walks,
// in one page, every combination of the bar, Pending changes and the banner that shows any. The
// banner comes first: a browser that focuses a new page itself asks for the builds then, and the
// page asks again only a minute later, so the answer that shows the banner has to be the one the
// page reads at its load. The banner is dismissed once no combination left shows it, and the
// refused change is discarded for the last. Each combination is read at the project's own size
// and, from 640 px wide, at three desktop sizes. At those, the table's box holds more rows than it
// shows, and each reading is made at the real end of its scroll (`readEnd`), where its last row
// meets neither Pending changes nor the banner. Once both floats are gone, no element is marked
// as a scroller that takes end room for them.
// Each reading is that no two of the elements' boxes meet, and that the element at the centre of
// every control of each is that control or inside it, never another element over it. A reading is
// taken again until it holds, since the foot moves a frame or more after a resize or a press. A
// trial press is no such reading: one passed with Pending changes lying over the Agents page's
// broadcast composer. The row after this one reads that composer the same way.
// SLOW(>=2s): the issues table's seven combinations at up to three sizes each, each desktop one at
// the end of the table's box, each a settled reading of every control shown.
test("no two of the selection bar, Pending changes and the update banner meet, and nothing covers the centre of a control of any of them @phone @foot", async ({
  browser,
}) => {
  const refused = await seedRefusedMove();
  const other = await createIssue({ project: "CORE", title: "Ship the parser" });
  // Enough issues after those two that the table's box scrolls at the desktop sizes, as in the
  // row for the table's last row below. They come after, so the two rows a press picks are drawn
  // while the box is at its start. Nothing reads their order among themselves, so they go out
  // side by side.
  await Promise.all(
    Array.from({ length: 40 }, (_, index) =>
      createIssue({ project: "CORE", title: `Filler issue number ${index}` })
    )
  );
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await routeFloats(page, refused.key);
    await page.goto("/projects/CORE/issues");
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    const select = (key: string) =>
      page.getByRole("checkbox", { name: `Select ${key}`, exact: true });
    const toolbar = page.getByRole("toolbar");
    const { pendingChanges, update } = footFloats(page);
    const pending = pendingChanges.box;
    const banner = update.box;
    const bar: FootElement = {
      name: "the selection bar",
      box: toolbar,
      controls: [
        {
          name: "Select all matching",
          control: toolbar.getByRole("button", { name: /^Select all \d+ matching$/ }),
        },
        { name: "Move to epic", control: toolbar.getByRole("combobox", { name: "Move to epic" }) },
        ...footButtons(toolbar, [
          "Start next",
          "Move",
          "Take out of epic",
          "New epic from these",
          "Clear selection",
        ]),
      ],
    };
    const own = page.viewportSize();
    if (!own) throw new Error("The project gave the page no viewport");
    const desktops = DESKTOP_SIZES.filter(
      (size) => size.width !== own.width || size.height !== own.height
    );
    const sizes = own.width >= 640 ? [own, ...desktops] : [own];
    // From 640 px wide the table's box holds more rows than it shows, and each reading of the
    // table is made at the real end of its scroll, where its last row has to clear the floats.
    const end: ScrollEnd | undefined =
      own.width >= 640 ? { controls: [], scroller: tableScroller(page) } : undefined;

    await expect(banner).toBeVisible();
    await expectFootClear(page, [update], sizes, end);

    await select(refused.key).check();
    await expectFootClear(page, [bar, update], sizes, end);

    await toolbar.getByRole("button", { name: "Take out of epic" }).click({ timeout: 5_000 });
    await expect(pending).toContainText(refused.key);
    await select(refused.key).setChecked(false);
    await select(other.key).check();
    await expectFootClear(page, [bar, pendingChanges, update], sizes, end);

    await toolbar.getByRole("button", { name: "Clear selection" }).click({ timeout: 5_000 });
    await expect(toolbar).toHaveCount(0);
    await expectFootClear(page, [pendingChanges, update], sizes, end);

    await banner.getByRole("button", { name: "Dismiss update notice" }).click({ timeout: 5_000 });
    await expect(banner).toHaveCount(0);
    await expectFootClear(page, [pendingChanges], sizes, end);

    await select(other.key).check();
    await expectFootClear(page, [bar, pendingChanges], sizes, end);

    await pending.getByRole("button", { name: "Discard", exact: true }).click({ timeout: 5_000 });
    await expect(pending).toHaveCount(0);
    await expectFootClear(page, [bar], sizes, end);
    await expectNoEndRoom(page);
  } finally {
    await context.close();
  }
});

// The Agents page's broadcast composer sits in the selection bar's layer of the foot, the page's
// own (`web/src/hooks/useFootInset.ts`), with Pending changes and the update banner floating
// above it. A refused change is pending, and the Agents page loads with the banner shown and three
// agents selected for broadcast. Scrolled to its end in a short window, the page leaves the
// composer resting above the section of ended sessions, where Pending changes once lay over its
// message box, its mode and Send. At four short sizes and at 1280x720, 1600x1000 and 1920x1080 the
// row reads the composer with Pending changes and the banner, then with Pending changes
// discarded, then alone, each reading as the row above takes it. There the last row of the
// section of ended sessions, at the real end of the document's scroll, meets neither Pending
// changes nor the banner, and nothing covers the centre of its session link, which the banner
// once lay over at 1280x720. Once both floats are gone, no element is marked as a scroller that
// takes end room for them.
// SLOW(>=2s): the composer's three combinations at seven sizes each, each a settled reading of
// every control shown at the real end of the document's scroll.
test("no two of the broadcast composer, Pending changes and the update banner meet on the Agents page, its ended session's row scrolls clear of the floats, and nothing covers the centre of a control of any of them @phone @foot", async ({
  browser,
}) => {
  /** A live session for the Agents page to list. Each id is a UUID, which Dispatch's agent
   *  routes require. */
  const session = (sessionID: string, title: string): FakeSession => ({
    capabilities: ["aside", "btw"],
    dir: `/workspaces/${title.toLowerCase()}`,
    last_seen: Date.now() - 5_000,
    machine_id: `${title.toLowerCase()}-host`,
    roles: [title.toLowerCase()],
    session_id: sessionID,
    title,
  });
  // The setup of `agents-groups.e2e.ts`'s row for the composer: fourteen live sessions, which run
  // the Agents page's list past a short window, and one that has ended with an unread reply to
  // the reader, which keeps it a row of the section of ended sessions under them.
  const titles = Array.from(
    { length: 14 },
    (_, index) => `Agent ${String(index + 1).padStart(2, "0")}`
  );
  const endedSession = "01a04731-0c07-7000-8000-000000000200";
  // The Agents setup and the issues setup touch nothing in common, so each chain goes out beside
  // the other, and each keeps its own order.
  const [, refused] = await Promise.all([
    (async () => {
      await setLiveSessions([
        ...titles.map((title, index) =>
          session(`01a04731-0c07-7000-8000-0000000001${String(index).padStart(2, "0")}`, title)
        ),
        session(endedSession, "Ended"),
      ]);
      const asked = await createAgentMessage(endedSession, {
        body: "How far along?",
        delivery: "aside",
      });
      await replyToMessageDelivery(
        asked.id,
        { attempt: 1, body: "Done and gone." },
        { id: endedSession, kind: "session" }
      );
      await setSessionLive(endedSession, false);
    })(),
    seedRefusedMove(),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await routeFloats(page, refused.key);
    await openWithFloats(page, refused.key, "/agents");
    const { pendingChanges, update } = footFloats(page);
    const broadcast = page.getByRole("region", { name: "Broadcast" });
    for (const title of titles.slice(0, 3)) {
      await page.getByRole("checkbox", { name: `Select ${title} for broadcast` }).check();
    }
    await expect(
      broadcast.getByRole("list", { name: "Selected agents" }).getByRole("listitem")
    ).toHaveCount(3);
    const composer: FootElement = {
      name: "the broadcast composer",
      box: broadcast,
      controls: [
        {
          name: "the message box",
          control: broadcast.getByRole("textbox", { name: "Broadcast message" }),
        },
        {
          name: "Delivery mode",
          control: broadcast.getByRole("combobox", { name: "Delivery mode" }),
        },
        ...footButtons(broadcast, ["Send to 3"]),
        {
          name: "chip",
          control: broadcast.getByRole("list", { name: "Selected agents" }).getByRole("button"),
          every: true,
        },
        { name: "link", control: broadcast.getByRole("link"), every: true },
      ],
    };
    const endedRow = page
      .getByRole("region", { name: "Replied, no longer connected" })
      .getByRole("article")
      .last();
    const sizes = [
      { width: 800, height: 600 },
      { width: 1024, height: 500 },
      { width: 640, height: 480 },
      { width: 360, height: 640 },
      ...DESKTOP_SIZES,
    ];
    // The document scrolls the Agents page. Its list sets its height from the rows it measures,
    // a frame or more after a resize or a scroll draws them, which can grow the page past a
    // scroll to its end made before that: at 640x480 the section of ended sessions once stayed
    // out of view. So each reading is made at the real end of the document's scroll, and the
    // ended session's row is read only where it lies wholly in the window.
    const end: ScrollEnd = {
      controls: [{ name: "the session link", control: endedRow.getByRole("link") }],
      scroller: {
        element: page.locator("html"),
        last: "the ended session's row",
        name: "the Agents page",
        rows: 'section[aria-label="Replied, no longer connected"] > article',
      },
    };
    await expectFootClear(page, [composer, pendingChanges, update], sizes, end);

    await pendingChanges.box
      .getByRole("button", { name: "Discard", exact: true })
      .click({ timeout: 5_000 });
    await expect(pendingChanges.box).toHaveCount(0);
    await expectFootClear(page, [composer, update], sizes, end);

    await update.box
      .getByRole("button", { name: "Dismiss update notice" })
      .click({ timeout: 5_000 });
    await expect(update.box).toHaveCount(0);
    await expectFootClear(page, [composer], sizes, end);
    await expectNoEndRoom(page);
  } finally {
    await context.close();
  }
});

// The other scrollers the foot's floats lie over (`web/src/hooks/useFootInset.ts`): the review
// margin's column from xl up, the agent live view's transcript and the Inbox's reading pane. Each
// page loads with a refused change pending, made on the issues table (`openWithFloats`), and
// with the update banner shown. At 1280x720, 1600x1000 and 1920x1080 the page's scroller is
// scrolled to the real end of its scroll (`readEnd`), and there its last element child, clipped
// to the scroller's box, meets neither float. Then the banner is dismissed and the change is
// discarded, and no element is marked any more: the end room goes with the floats.
// SLOW(>=2s): three pages, each read at three sizes after a change refused on the issues table,
// one of them behind a transcript at the stream's limit.
test("the margin column, the live view's transcript and the Inbox reading pane each scroll their last content clear of the floats @foot", async ({
  browser,
}) => {
  /** The session that asks the margin's questions. */
  const agent = {
    actor: { id: "e2e-foot-clearance", kind: "session" as const },
    as: "agent" as const,
  };
  /** A live session whose conversation is at the stream's limit. Its id is a UUID, which
   *  Dispatch's agent routes require. */
  const live: FakeSession = {
    capabilities: ["aside", "btw", "steer"],
    dir: "/workspaces/foot",
    last_seen: Date.now() - 5_000,
    machine_id: "foot-host",
    roles: [],
    session_id: "01a04731-0c07-7000-8000-000000000300",
    title: "Foot",
  };
  // Twelve words of the spec, each the anchor of one question in the margin.
  const words = [
    "alpha",
    "bravo",
    "charlie",
    "delta",
    "echo",
    "foxtrot",
    "golf",
    "hotel",
    "india",
    "juliet",
    "kilo",
    "lima",
  ];
  // The three setups touch nothing in common, so each goes out beside the others, and each keeps
  // its own order.
  const [{ refused, reviewed }, approval] = await Promise.all([
    (async () => {
      const refused = await seedRefusedMove();
      // A dozen questions, each anchored to its own word of the spec, so the margin's column
      // holds a card for each and scrolls.
      const reviewed = await createIssue({
        project: "CORE",
        spec: `${words.join(" ")}.\n`,
        title: "Margin review",
      });
      for (const word of words) {
        await createAsk(
          reviewed.key,
          {
            anchor: { artifact: "spec", quote: word },
            options: [{ label: "Keep" }, { label: "Drop" }],
            question: `Keep ${word}?`,
          },
          agent
        );
      }
      return { refused, reviewed };
    })(),
    seedApprovalPlace(),
    (async () => {
      await setLiveSessions([live]);
      await seedAgentStreamConversations([live.session_id]);
    })(),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await routeFloats(page, refused.key);
    const { pendingChanges, update } = footFloats(page);
    /**
     * Fails unless, at 1280x720, 1600x1000 and 1920x1080, the last content of `scroller` meets
     * neither float at the real end of its scroll: each size one reading, taken again until it
     * holds. Then closes both floats, and fails unless no element is marked any more as a
     * scroller that takes end room for them.
     */
    const expectEndClear = async (scroller: Scroller) => {
      for (const size of DESKTOP_SIZES) {
        await page.setViewportSize(size);
        await scrollToItsEnd(scroller);
        const at = `${size.width}x${size.height}`;
        await expect
          .poll(
            async () => {
              const problems: string[] = [];
              const floats = await footBoxes([update, pendingChanges], at, problems);
              problems.push(...endProblems(scroller, await readEnd(scroller), floats, at));
              return problems;
            },
            { message: `the end of ${scroller.name} at ${at}` }
          )
          .toEqual([]);
      }
      await update.box
        .getByRole("button", { name: "Dismiss update notice" })
        .click({ timeout: 5_000 });
      await expect(update.box).toHaveCount(0);
      await pendingChanges.box
        .getByRole("button", { name: "Discard", exact: true })
        .click({ timeout: 5_000 });
      await expect(pendingChanges.box).toHaveCount(0);
      await expectNoEndRoom(page);
    };

    // The margin's column from xl up, holding a card for each question.
    await openWithFloats(page, refused.key, `/issues/${reviewed.key}/spec`);
    const margin = page.getByTestId("margin-sheet");
    await expect(margin.locator("[data-margin-item]")).toHaveCount(words.length);
    await expectEndClear({
      element: margin,
      last: "the margin column's last element child",
      name: "the margin's column",
    });

    // The live view's transcript, with the whole conversation drawn.
    await openWithFloats(page, refused.key, `/agents/${live.session_id}/live`);
    await expectConversationInFront(page, live.session_id);
    await expectEndClear({
      element: frontLiveView(page).getByTestId("agent-thread"),
      last: "the transcript's last element child",
      name: "the live view's transcript",
    });

    // The Inbox's reading pane, holding the approval's long document.
    await openWithFloats(page, refused.key, inboxAt(approval.approvalAskId));
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", approval.approvalAskId);
    await expect(inboxPane(page)).toContainText(APPROVAL_FOOT);
    await expectEndClear({
      element: paneScroller(page),
      last: "the reading pane's last element child",
      name: "the Inbox reading pane",
    });
  } finally {
    await context.close();
  }
});

// The update banner sits at the foot of the window too, centred, as the selection bar does from
// the sm width up. With a selection made and the banner shown (every live process serves a build
// other than the page's), every control of the bar must still take a press at each width where
// the bar sits at the foot; on a phone the bar sits under the header instead.
test("with the update banner shown, every control of the selection bar takes a press @phone @foot", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Parser rollout" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const other = await createIssue({ project: "CORE", title: "Ship the parser" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    // Every live process now serves one build the page does not run (`deploymentChanged`).
    await page.route("**/api/v1/dashboard/builds", (route) =>
      route.fulfill({
        json: { live: ["/assets/index-next.js"], serving: "/assets/index-next.js" },
      })
    );
    await page.goto("/projects/CORE/issues");
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await expect(page.getByRole("status", { name: "A new version is available" })).toBeVisible();
    await page.getByRole("checkbox", { name: `Select ${other.key}`, exact: true }).check();
    const bar = page.getByRole("toolbar");
    // Move takes a press only once an epic is picked, and picking one sends nothing.
    const epics = bar.getByRole("combobox", { name: "Move to epic" });
    await epics.selectOption(epic.key);
    const controls = [...(await bar.getByRole("button").all()), epics];
    const own = page.viewportSize();
    if (!own) throw new Error("The project gave the page no viewport");
    const atFoot = [
      { width: 1600, height: 1000 },
      { width: 1280, height: 720 },
      { width: 800, height: 600 },
    ];
    for (const size of own.width >= 640 ? [own, ...atFoot] : [own]) {
      await page.setViewportSize(size);
      // A press that another element takes fails here, as in the rows above.
      for (const control of controls) await control.click({ trial: true, timeout: 5_000 });
    }
  } finally {
    await context.close();
  }
});

// The issues table's own scroll box and the floats over it. With a selection made, a refused
// change pending and the update banner shown, the bar, the banner and Pending changes stack up
// the foot (`web/src/hooks/useFootInset.ts`), and Pending changes rides above the banner. Once
// the table's box is scrolled to its end, its last row must still scroll clear of the floats, as
// a scroller's last content does there ("so its last content scrolls clear of those floats").
test("with the bar, the banner and Pending changes shown, the table's last row scrolls clear of the floats @foot", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Parser rollout" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const refused = await createIssue({ project: "CORE", parent: epic.key, title: "Refused move" });
  const other = await createIssue({ project: "CORE", title: "Ship the parser" });
  // Enough issues that the table's box scrolls at the desktop sizes below.
  for (let index = 0; index < 40; index++) {
    await createIssue({ project: "CORE", title: `Filler issue number ${index}` });
  }
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.route(`**/api/v1/issues/${refused.key}`, async (route) => {
      if (route.request().method() === "PATCH")
        await route.fulfill({ status: 403, json: { error: "Move refused" } });
      else await route.continue();
    });
    // Every live process now serves one build the page does not run (`deploymentChanged`).
    await page.route("**/api/v1/dashboard/builds", (route) =>
      route.fulfill({
        json: { live: ["/assets/index-next.js"], serving: "/assets/index-next.js" },
      })
    );
    await page.goto("/projects/CORE/issues");
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await expect(page.getByRole("status", { name: "A new version is available" })).toBeVisible();
    const select = (key: string) =>
      page.getByRole("checkbox", { name: `Select ${key}`, exact: true });
    await select(refused.key).check();
    await page.getByRole("toolbar").getByRole("button", { name: "Take out of epic" }).click();
    const pending = pendingWrites(page);
    await expect(pending).toContainText(refused.key);
    await select(refused.key).setChecked(false);
    await select(other.key).check();
    // The box is read at the real end of its scroll (`readEnd`).
    const table = tableScroller(page);
    for (const size of [
      { width: 1600, height: 1000 },
      { width: 1920, height: 1080 },
    ]) {
      await page.setViewportSize(size);
      await scrollToItsEnd(table);
      const at = `${size.width}x${size.height}`;
      await expect
        .poll(
          async () => {
            const panel = await pending.boundingBox();
            if (panel === null) return [`Pending changes has no box at ${at}`];
            const floats = [{ box: panel, name: "Pending changes" }];
            return endProblems(table, await readEnd(table), floats, at);
          },
          { message: `the table's last row against Pending changes at ${at}` }
        )
        .toEqual([]);
    }
  } finally {
    await context.close();
  }
});

// End room that moves nothing a float lies over (`web/src/hooks/useFootInset.ts`). From the xl
// width up, Pending changes sits at the window's lower right, over the issue page's Review margin,
// a box of the screen's height whose content does not overflow it. End room in the document's
// own scroller lets none of that content scroll clear, and only lengthens the page past the
// shell: at the end of its scroll, the shell's Navigation, sticky at the top, slides up off the
// screen and leaves an empty band at the foot. So with a refused change pending, an issue page
// scrolled to its end must keep the Navigation's top on the screen.
test("with a refused change pending, an issue page scrolled to its end keeps the shell's Navigation on the screen @foot", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Parser rollout" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const refused = await createIssue({ project: "CORE", parent: epic.key, title: "Refused move" });
  const other = await createIssue({ project: "CORE", title: "Ship the parser" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1600, height: 1000 });
    await page.route(`**/api/v1/issues/${refused.key}`, async (route) => {
      if (route.request().method() === "PATCH")
        await route.fulfill({ status: 403, json: { error: "Move refused" } });
      else await route.continue();
    });
    await page.goto("/projects/CORE/issues");
    await page.getByRole("checkbox", { name: `Select ${refused.key}`, exact: true }).check();
    await page.getByRole("toolbar").getByRole("button", { name: "Take out of epic" }).click();
    const pending = pendingWrites(page);
    await expect(pending).toContainText(refused.key);
    await navigateInApp(page, `/issues/${other.key}`);
    await expect(page.getByRole("heading", { name: "Ship the parser" })).toBeVisible();
    await expect(pending).toContainText(refused.key);
    const navigation = page.getByRole("complementary", { name: "Navigation" });
    for (const size of [
      { width: 1600, height: 1000 },
      { width: 1920, height: 1080 },
      { width: 1280, height: 800 },
    ]) {
      await page.setViewportSize(size);
      await expect
        .poll(
          async () => {
            await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
            await nextFrames(page);
            return (await navigation.boundingBox())?.y ?? Number.NaN;
          },
          {
            message: `the Navigation's top at the end of the page at ${size.width}x${size.height}`,
          }
        )
        .toBeGreaterThanOrEqual(0);
    }
  } finally {
    await context.close();
  }
});

test("a live insertion above a vertically and horizontally scrolled reader keeps their row in place", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Live root epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const parent = await createIssue({
    project: "CORE",
    parent: epic.key,
    title: "Closed intermediate",
  });
  await createMessage(parent.key, { body: "Closed intermediate work." });
  await patchIssue(parent.key, { status: "done" });
  for (let index = 0; index < 80; index++)
    await createIssue({ project: "CORE", title: `B work ${String(index).padStart(3, "0")}` });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1000, height: 800 });
    await page.goto("/projects/CORE/issues?sort=title&size=250");
    // 80 seeded rows and the epic, which is a row of the table like any other.
    await expect(page.getByText("81 of 81 issues", { exact: true })).toBeVisible();
    const scroller = page.getByTestId("issues-table-scroll");
    // The handle is taken once on purpose: it names this scroller element, which the row checks
    // later is still the one connected.
    const original = await scroller.elementHandle();
    const ancestorRequested = Promise.withResolvers<void>();
    const ancestorReady = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${parent.key}`, async (route) => {
      ancestorRequested.resolve();
      await ancestorReady.promise;
      await route.continue();
    });
    await scroller.evaluate((element) => {
      element.scrollTop = 1600;
      element.scrollLeft = 600;
    });
    const visibleAnchor = () =>
      scroller.evaluate((element) => {
        const box = element.getBoundingClientRect();
        const row = document
          .elementFromPoint(box.left + box.width / 2, box.top + 130)
          ?.closest<HTMLElement>("[data-issue-row]");
        return row ? { key: row.dataset.issueRow, top: row.getBoundingClientRect().top } : null;
      });
    await expect.poll(visibleAnchor).not.toBeNull();
    // The poll above waited until a row sat under that point, and nothing has moved the table
    // since, so this reads the row the poll found.
    const anchor = await visibleAnchor();
    if (!anchor?.key) throw new Error("No visible issue could be anchored after scrolling");
    expect(await scroller.evaluate((element) => element.scrollLeft)).toBeGreaterThan(0);
    await createIssue({ project: "CORE", parent: parent.key, title: "A newly inserted issue" });
    await ancestorRequested.promise;
    expect(await original?.evaluate((element) => element.isConnected)).toBe(true);
    expect(await scroller.evaluate((element) => element.scrollTop)).toBeGreaterThan(0);
    ancestorReady.resolve();
    await expect(page.getByText("82 of 82 issues", { exact: true })).toBeVisible();
    await expect
      .poll(() =>
        page
          .locator(`[data-issue-row="${anchor.key}"]`)
          .evaluate((element) => element.getBoundingClientRect().top)
      )
      .toBe(anchor.top);
  } finally {
    await context.close();
  }
});

test("a refused bulk write retries after reload while the other operations have already landed @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Bulk source" });
  await patchIssue(epic.key, { labels: ["epic"] });
  const rows: Issue[] = [];
  for (const title of ["First move", "Second move", "Third move"])
    rows.push(await createIssue({ project: "CORE", parent: epic.key, title }));
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    const second = rows[1];
    const third = rows[2];
    if (!second || !third) throw new Error("Missing bulk fixtures");
    await page.route(`**/api/v1/issues/${second.key}`, async (route) => {
      if (route.request().method() === "PATCH")
        await route.fulfill({
          status: 403,
          json: { error: "Bulk probe refusal", code: "FORBIDDEN" },
        });
      else await route.continue();
    });
    await page.goto("/projects/CORE/issues");
    for (const row of rows) {
      await page.getByRole("checkbox", { name: `Select ${row.key}`, exact: true }).check();
      // The bar is fixed to the window, so the geometry that matters is the page at its top,
      // where the bar sits under the header and the table begins below it.
      await page.evaluate(() => window.scrollTo(0, 0));
      const toolbar = page.getByRole("toolbar");
      const table = page.getByTestId("issues-table-scroll");
      await expect(toolbar).toBeVisible();
      await expect(table).toBeVisible();
      // The waits above drew the bar and the table, and a drawn element has a box.
      const bar = await toolbar.boundingBox();
      // The table was waited for above too, so its box is there as the bar's is.
      const box = await table.boundingBox();
      if (!bar || !box) throw new Error("The selection toolbar or table is missing");
      expect(
        bar.y + bar.height <= box.y || bar.y >= box.y + box.height,
        "the phone selection toolbar never covers the working table"
      ).toBe(true);
    }
    await page.getByRole("button", { name: "Take out of epic" }).click();
    await expect.poll(() => getIssue(third.key).then((issue) => issue.parent)).toBeNull();
    await expect(page.getByRole("region", { name: "Pending changes" })).toContainText(
      `Update ${second.key}`
    );
    await expect(page.getByRole("region", { name: "Pending changes" })).not.toContainText(
      `Update ${third.key}`
    );
    await page.reload();
    await expect(page.getByRole("region", { name: "Pending changes" })).toContainText(
      `Update ${second.key}`
    );
    expect((await getIssue(third.key)).parent).toBeNull();
    await page.unroute(`**/api/v1/issues/${second.key}`);
    await page
      .getByRole("region", { name: "Pending changes" })
      .getByRole("button", { name: "Retry", exact: true })
      .click();
    await expect
      .poll(async () =>
        Promise.all(rows.map((row) => getIssue(row.key).then((issue) => issue.parent)))
      )
      .toEqual([null, null, null]);
    await expect(page.getByRole("region", { name: "Pending changes" })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("phone headers, saved widths, popover focus and short-window paging stay usable @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Phone epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  await createIssue({ project: "CORE", parent: epic.key, title: "Phone member" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues");
    await page.evaluate(() =>
      localStorage.setItem(
        "dispatch.project-table.CORE",
        JSON.stringify({ sizing: { title: 450 } })
      )
    );
    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload();
    const box = page.getByTestId("issues-table-scroll");
    await page.getByRole("button", { name: "Columns", exact: true }).click();
    await page
      .getByRole("dialog", { name: "Columns" })
      .getByRole("checkbox", { name: "Updated", exact: true })
      .check();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("searchbox", { name: "Search issues" })).toBeVisible();
    const search = await page.getByRole("searchbox", { name: "Search issues" }).boundingBox();
    expect(search?.width).toBeGreaterThan(300);
    const keyHeader = page
      .getByRole("columnheader")
      .filter({ has: page.getByRole("button", { name: "Key", exact: true }) });
    const titleHeader = page
      .getByRole("columnheader")
      .filter({ has: page.getByRole("button", { name: "Title", exact: true }) });
    await expect(keyHeader).toBeVisible();
    await expect(titleHeader).toBeVisible();
    await expect(box).toBeVisible();
    // The waits above drew both headers and the table, and a drawn element has a box.
    const keyBefore = await keyHeader.boundingBox();
    // The title header was waited for above too, so its box is there as the key's is.
    const titleBefore = await titleHeader.boundingBox();
    // The table was waited for above too, so its box is there as the key's is.
    const tableBox = await box.boundingBox();
    if (!keyBefore || !titleBefore || !tableBox)
      throw new Error("The identifying columns are missing");
    // The pinned key and title leave an edge of the next column, and the title is wide enough
    // to read a few words, not a fragment.
    expect(titleBefore.x + titleBefore.width - tableBox.x).toBeLessThanOrEqual(
      tableBox.width * 0.85 + 1
    );
    expect(titleBefore.width).toBeGreaterThanOrEqual(120);
    await box.evaluate((element) => {
      element.scrollLeft = element.scrollWidth;
    });
    // The headers were waited for above, and a scroll moves them without taking them off the
    // page, so the key header's box is there.
    const keyAfter = await keyHeader.boundingBox();
    // The title header was waited for above as well, so its box is there as the key's is.
    const titleAfter = await titleHeader.boundingBox();
    if (!keyAfter || !titleAfter)
      throw new Error("The identifying columns disappeared after scrolling");
    expect(keyAfter.x).toBeCloseTo(keyBefore.x, 0);
    expect(titleAfter.x).toBeCloseTo(titleBefore.x, 0);
    await expect(
      page.locator("[data-issue-row]").first().locator("[data-issue-link]")
    ).toBeVisible();
    const remainingControl = page.getByRole("button", { name: "Filter Updated", exact: true });
    await expect(remainingControl).toBeVisible();
    // The wait above drew the control, and a drawn element has a box, so the box is there.
    const controlAtEnd = await remainingControl.boundingBox();
    if (!controlAtEnd) throw new Error("The Updated filter is missing");
    await box.evaluate(
      (element, reveal) => {
        element.scrollLeft -= reveal;
      },
      Math.max(0, titleAfter.x + titleAfter.width - controlAtEnd.x)
    );
    // The control was waited for above, and a scroll moves it without taking it off the page, so
    // its box is there.
    const controlBox = await remainingControl.boundingBox();
    if (!controlBox) throw new Error("No control is visible beside the identifying columns");
    expect(controlBox.x).toBeGreaterThanOrEqual(titleAfter.x + titleAfter.width - 1);
    expect(controlBox.x + controlBox.width).toBeLessThanOrEqual(tableBox.x + tableBox.width);
    await remainingControl.click();
    await page.getByRole("combobox", { name: "Search Updated", exact: true }).fill("day");
    await page.keyboard.press("Escape");
    for (const name of ["Priority", "Key", "Title"]) {
      await box.evaluate((element) => {
        element.scrollLeft = 0;
      });
      await page.getByRole("button", { name: `Filter ${name}`, exact: true }).click();
      await page.keyboard.press("Escape");
    }
    await page.getByRole("button", { name: "Columns", exact: true }).click();
    const panel = page.getByRole("dialog", { name: "Columns" });
    await page.keyboard.press("Tab");
    await expect(panel).toBeVisible();
    expect(await panel.evaluate((element) => element.contains(document.activeElement))).toBe(true);
    await page.keyboard.press("Escape");
    await expect(page.getByRole("button", { name: "Columns", exact: true })).toBeFocused();
    // An epic is an ordinary row of the table, searchable and selectable like any other, and
    // becomes a heading only in the grouping that makes it one.
    await expect(page.locator(`[data-issue-row="${epic.key}"]`)).toHaveCount(1);
    await page.getByRole("combobox", { name: "Group by" }).selectOption("epic");
    await expect(page.locator(`[data-issue-row="${epic.key}"]`)).toHaveCount(0);
    await expect(
      page.getByRole("link", { name: `${epic.key} · Phone epic`, exact: true }).first()
    ).toHaveAttribute("href", `/issues/${epic.key}`);
    await expect(page.getByText("1 of 1 issue in 1 group", { exact: true })).toBeVisible();
    await page.screenshot({ path: testInfo.outputPath("issues-table-phone.png"), fullPage: true });
    await page.setViewportSize({ width: 900, height: 400 });
    await page.getByTestId("issues-table-pager").scrollIntoViewIfNeeded();
    const pager = await page.getByTestId("issues-table-pager").boundingBox();
    expect((pager?.y ?? 400) + (pager?.height ?? 1)).toBeLessThanOrEqual(400);
    await page.screenshot({
      path: testInfo.outputPath("issues-table-short-window.png"),
      fullPage: true,
    });
  } finally {
    await context.close();
  }
});

test("compact tables keep three full issues readable after scrolling the page @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  for (const title of [
    "Investigate authorization headers",
    "Review deployment pipeline",
    "Measure database connection pressure",
    "Correct dashboard rendering",
    "Document release verification",
    "Restore unavailable session",
  ])
    await createIssue({ project: "CORE", title });
  const context = await asUser(browser, "alice");
  try {
    for (const viewport of [
      { width: 390, height: 844 },
      { width: 900, height: 400 },
    ]) {
      const page = await context.newPage();
      await page.setViewportSize(viewport);
      await page.goto("/projects/CORE/issues");
      const box = page.getByTestId("issues-table-scroll");
      await expect(box.locator("[data-issue-row]")).toHaveCount(6);
      const capacity = await box.evaluate((element) => {
        const header = element.querySelector("thead")?.getBoundingClientRect().height ?? 0;
        const rows = [...element.querySelectorAll("[data-issue-row]")].slice(0, 3);
        return {
          available: element.clientHeight - header,
          required: rows.reduce((height, row) => height + row.getBoundingClientRect().height, 0),
        };
      });
      expect(capacity.available).toBeGreaterThanOrEqual(capacity.required);
      await box.evaluate((element) => element.scrollIntoView({ block: "start" }));
      await expect
        .poll(() =>
          box.evaluate((element) => {
            const bounds = element.getBoundingClientRect();
            const header =
              element.querySelector("thead")?.getBoundingClientRect().bottom ?? bounds.top;
            return [...element.querySelectorAll("[data-issue-row]")].filter((row) => {
              const rect = row.getBoundingClientRect();
              return (
                rect.top >= header && rect.bottom <= Math.min(bounds.bottom, window.innerHeight)
              );
            }).length;
          })
        )
        .toBeGreaterThanOrEqual(3);
      expect(await box.evaluate((element) => element.scrollTop)).toBe(0);
      await page.close();
    }
  } finally {
    await context.close();
  }
});

test("List and Board share case-insensitive every-label matching", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const both = await createIssue({ project: "CORE", title: "Carries both labels" });
  await patchIssue(both.key, { labels: ["FrontEnd", "BACKEND"] });
  const one = await createIssue({ project: "CORE", title: "Carries one label" });
  await patchIssue(one.key, { labels: ["frontend"] });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues?label=frontend&label=backend");
    await expect(page.locator(`[data-issue-row="${both.key}"]`)).toBeVisible();
    await expect(page.locator(`[data-issue-row="${one.key}"]`)).toHaveCount(0);
    await page.getByRole("button", { name: "Board", exact: true }).click();
    await expect(page.getByRole("link", { name: /Carries both labels/ }).first()).toBeVisible();
    await expect(page.getByText("Carries one label", { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("compact windows give the table three full rows and let the page scroll @phone", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 8; index++)
    await createIssue({ project: "CORE", title: `Compact row ${index}` });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    for (const viewport of [
      { width: 390, height: 844 },
      { width: 900, height: 400 },
    ]) {
      await page.setViewportSize(viewport);
      await page.goto("/projects/CORE/issues");
      await expect(page.locator("[data-issue-row]").nth(2)).toBeAttached();
      const visibleRows = await page.getByTestId("issues-table-scroll").evaluate((box) => {
        const bounds = box.getBoundingClientRect();
        const header = box.querySelector("th")?.getBoundingClientRect().bottom ?? bounds.top;
        return [...box.querySelectorAll("[data-issue-row]")].filter((row) => {
          const rect = row.getBoundingClientRect();
          return rect.top >= header - 1 && rect.bottom <= bounds.bottom + 1;
        }).length;
      });
      expect(visibleRows).toBeGreaterThanOrEqual(3);
      expect(await page.evaluate(() => document.documentElement.scrollHeight > innerHeight)).toBe(
        true
      );
      await page.getByTestId("issues-table-scroll").scrollIntoViewIfNeeded();
      await page.screenshot({
        path: testInfo.outputPath(`three-rows-${viewport.width}.png`),
        fullPage: true,
      });
      await page.getByTestId("issues-table-pager").scrollIntoViewIfNeeded();
      await expect(page.getByRole("combobox", { name: "Rows per page" })).toBeVisible();
    }
  } finally {
    await context.close();
  }
});

test("a roomy window is filled by the box, which grows when the window does", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 6; index++)
    await createIssue({ project: "CORE", title: `Roomy row ${index}` });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1400, height: 900 });
    await page.goto("/projects/CORE/issues");
    await expect(page.locator("[data-issue-row]").nth(5)).toBeAttached();
    // The box takes the room the window leaves: the pager's last pixel lands at the window's
    // bottom, less the padding the page keeps under it for the floating selection bar, and
    // nothing scrolls. A box that only ever shrank would pass a page-height check and fail
    // this one.
    const room = async () =>
      await page.evaluate(() => {
        const pager = document.querySelector('[data-testid="issues-table-pager"]');
        const box = document.querySelector('[data-testid="issues-table-scroll"]');
        const main = box?.closest("main");
        if (!pager || !box || !main) return null;
        const padding = (element: Element) =>
          Number.parseFloat(getComputedStyle(element).paddingBottom) || 0;
        return {
          slack:
            window.innerHeight -
            padding(box.parentElement as Element) -
            padding(main) -
            pager.getBoundingClientRect().bottom,
          box: box.getBoundingClientRect().height,
          page: document.documentElement.scrollHeight - window.innerHeight,
        };
      });
    await expect.poll(async () => (await room())?.slack).toBeLessThanOrEqual(1);
    expect((await room())?.slack).toBeGreaterThanOrEqual(-1);
    expect((await room())?.page).toBeLessThanOrEqual(1);
    await page.mouse.wheel(0, 400);
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    await expect(page.getByRole("combobox", { name: "Rows per page" })).toBeInViewport();
    const short = (await room())?.box ?? 0;
    await page.setViewportSize({ width: 1400, height: 1200 });
    await expect.poll(async () => (await room())?.box ?? 0).toBeGreaterThan(short + 200);
    expect((await room())?.slack).toBeLessThanOrEqual(1);
  } finally {
    await context.close();
  }
});

test("phone status labels are readable and closed issues load from the Status filter @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Triage member" });
  const todo = await createIssue({ project: "CORE", title: "Todo member" });
  await patchIssue(todo.key, { status: "todo" });
  const testing = await createIssue({ project: "CORE", title: "Testing member" });
  await patchIssue(testing.key, { status: "testing" });
  const closed = await createIssue({ project: "CORE", title: "Completed member" });
  await createMessage(closed.key, { body: "Finished this member." });
  await patchIssue(closed.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/projects/CORE/issues");
    const strip = page.getByRole("group", { name: "Issues by status" });
    // The seed's three open statuses, Triage, Todo and Testing, each draw one cell.
    await expect(strip.getByRole("button")).toHaveCount(3);
    // No Done cell at all while closed issues are unloaded: the bar counts what it has.
    await expect(strip.getByRole("button", { name: /^Done/ })).toHaveCount(0);
    // The count of three above waited for every cell the strip draws, so one read lists them.
    for (const button of await strip.getByRole("button").all()) {
      await button.scrollIntoViewIfNeeded();
      const readable = await button.evaluate((element) => {
        const text = document.createRange();
        text.selectNodeContents(element);
        const bounds = element.getBoundingClientRect();
        const glyphs = text.getBoundingClientRect();
        return {
          height: bounds.height,
          readable: glyphs.width <= bounds.width && glyphs.height <= bounds.height,
        };
      });
      expect(readable.height).toBeGreaterThanOrEqual(44);
      expect(readable.readable).toBe(true);
      await expect(button).toBeInViewport();
    }
    await page.getByRole("button", { name: /^Filter Status/ }).click();
    await page.getByRole("option", { name: "Done, not loaded" }).click();
    await expect(page.locator(`[data-issue-row="${closed.key}"]`)).toBeAttached();
  } finally {
    await context.close();
  }
});

test("on a desktop the status bar spans its box and stays one short line", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  for (const [status, title] of [
    ["todo", "Queued member"],
    ["testing", "Testing member"],
    ["backlog", "Parked member"],
  ] as const) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status });
  }
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/projects/CORE/issues");
    const strip = page.getByRole("group", { name: "Issues by status" });
    await expect(strip.getByRole("button", { name: /^Done/ })).toHaveCount(0);
    const geometry = await strip.evaluate((element) => {
      const box = element.getBoundingClientRect();
      const buttons = [...element.querySelectorAll("button")].map((button) =>
        button.getBoundingClientRect()
      );
      return {
        left: box.left,
        right: box.right,
        height: box.height,
        first: buttons[0]?.left ?? 0,
        last: buttons.at(-1)?.right ?? 0,
      };
    });
    expect(Math.abs(geometry.first - geometry.left)).toBeLessThanOrEqual(1);
    expect(Math.abs(geometry.right - geometry.last)).toBeLessThanOrEqual(1);
    expect(geometry.height).toBeLessThanOrEqual(32);
  } finally {
    await context.close();
  }
});

test("a session creator renders its live name, state and conversation link @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const id = "issues-table-creator";
  await setLiveSessions([{ session_id: id, title: "Live table creator" }]);
  const issue = await createIssue(
    { project: "CORE", title: "Created by a session" },
    {
      as: "agent",
      actor: { kind: "session", id, origin: { session_title: "Stored creator name" } },
    }
  );
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/projects/CORE/issues");
    const row = page.locator(`[data-issue-row="${issue.key}"]`);
    const creatorCell = row.getByRole("cell").filter({
      has: page.getByRole("link", { name: /Live table creator/ }),
    });
    const author = creatorCell.getByRole("link", { name: /Live table creator/ });
    await author.scrollIntoViewIfNeeded();
    await expect(author).toHaveAttribute("href", new RegExp(id));
    await expect(creatorCell.getByRole("img", { name: "Running", exact: true })).toBeVisible();
    await expect(row.getByText("Stored creator name", { exact: true })).toHaveCount(0);
  } finally {
    await context.close();
    await setLiveSessions([]);
  }
});

test("an unavailable ancestor uses one retry policy per attempt @phone", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const parent = await createIssue({ project: "CORE", title: "Unavailable ancestor" });
  await createIssue({ project: "CORE", parent: parent.key, title: "Open child" });
  await createMessage(parent.key, { body: "Closed ancestor." });
  await patchIssue(parent.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 390, height: 844 });
    let attempts = 0;
    await page.route(`**/api/v1/issues/${parent.key}`, async (route) => {
      attempts++;
      await route.fulfill({ status: 503, json: { error: "Ancestor unavailable" } });
    });
    await page.goto("/projects/CORE/issues");
    await expect(page.getByRole("alert")).toContainText("Could not load 1 ancestor");
    expect(attempts).toBe(3);
    await page.getByRole("button", { name: "Retry loading ancestors" }).click();
    await expect.poll(() => attempts).toBe(6);
    await expect(page.getByRole("alert")).toContainText("Unknown epic");
    await expect(page.getByRole("table", { name: "Project issues table" })).toBeAttached();
  } finally {
    await context.close();
  }
});

test("a refused merge move leaves the source open without a close reason", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = await createIssue({ project: "CORE", title: "Merge source" });
  const target = await createIssue({ project: "CORE", title: "Merge destination" });
  await patchIssue(source.key, { labels: ["epic"] });
  await patchIssue(target.key, { labels: ["epic"] });
  const bad = await createIssue({ project: "CORE", parent: source.key, title: "Refused move" });
  const good = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Independent move",
  });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.route(`**/api/v1/issues/${bad.key}`, async (route) => {
      if (route.request().method() === "PATCH")
        await route.fulfill({ status: 403, json: { error: "Move refused" } });
      else await route.continue();
    });
    await page.goto("/projects/CORE/issues?group=epic");
    // The childless destination epic keeps a heading of its own, so the merge starts at the
    // source's rather than at whichever heading comes first.
    const heading = page.locator("tr", { has: page.getByRole("link", { name: /Merge source/ }) });
    await heading.getByRole("button", { name: "Epic actions ▾" }).click();
    await page.getByRole("combobox", { name: "Merge into another epic" }).selectOption(target.key);
    await page.getByRole("button", { name: "Merge…" }).click();
    await page
      .getByRole("dialog", { name: "Merge this epic?" })
      .getByRole("button", { name: "Merge", exact: true })
      .click();
    await expect(page.getByRole("alert")).toContainText(bad.key);
    await expect.poll(() => getIssue(good.key).then((issue) => issue.parent)).toBe(target.key);
    expect((await getIssue(source.key)).status).not.toBe("done");
    expect(await listIssueMessages(source.key)).toHaveLength(0);
  } finally {
    await context.close();
  }
});

test("a stopped committed epic retries one key and reload finishes only its remaining adoption @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "Preserve the caller identity" });
  const closed = await createIssue({ project: "CORE", title: "Document release verification" });
  const context = await asUser(browser, "alice");
  const held = Promise.withResolvers<void>();
  try {
    const page = await context.newPage();
    const posts = watchWrites(page, "POST", "/api/v1/issues");
    const committed = dropFirstAnswer(page, "**/api/v1/issues", "POST", held.promise);
    await page.clock.install();
    await page.goto("/projects/CORE/issues");
    await page.getByRole("checkbox", { name: `Select ${first.key}`, exact: true }).check();
    await page.getByRole("checkbox", { name: `Select ${closed.key}`, exact: true }).check();
    await page.getByRole("button", { name: "New epic from these", exact: true }).click();
    await page
      .getByRole("dialog", { name: "Name the new epic" })
      .getByRole("textbox", { name: "Name" })
      .fill("Recover one created epic");
    await page.getByRole("button", { name: "Create epic", exact: true }).click();
    expect(await committed).toBe(201);
    await page.clock.fastForward(SEND_DEADLINE_MS + 1);
    const pending = page.getByRole("region", { name: "Pending changes" });
    await pending.getByRole("button", { name: "Stop trying", exact: true }).click();
    await expect(pending).toContainText("Not saved");
    held.resolve();
    await page.reload();
    await expect(pending).toContainText("Not saved");
    await pending.getByRole("button", { name: "Retry", exact: true }).click();
    const interrupted = page.getByRole("region", { name: "Interrupted table action" });
    await expect(interrupted).toContainText("not yet adopted");
    await expect.poll(() => posts.length).toBe(2);
    expect(posts[0]?.key).toEqual(expect.any(String));
    expect(posts[1]?.key).toBe(posts[0]?.key);
    const epics = (await listIssues("CORE")).filter((issue) => issue.labels?.includes("epic"));
    expect(epics).toHaveLength(1);
    const epic = epics[0];
    if (!epic) throw new Error("The committed epic is missing");
    await expect(interrupted).toContainText(epic.key);
    expect((await getIssue(first.key)).parent).toBeNull();
    await createMessage(closed.key, { body: "Completed while the action was interrupted." });
    await expect(
      page.getByRole("region", { name: "Project issues" }).getByRole("alert")
    ).toHaveCount(0);
    await patchIssue(closed.key, { status: "done" });
    await page.reload();
    await expect(interrupted).toContainText(epic.key);
    expect((await getIssue(first.key)).parent).toBeNull();
    await interrupted.getByRole("button", { name: "Finish", exact: true }).click();
    await expect.poll(() => getIssue(first.key).then((issue) => issue.parent)).toBe(epic.key);
    expect((await getIssue(closed.key)).parent).toBeNull();
    await expect(interrupted).toHaveCount(0);
    expect(
      (await listIssues("CORE")).filter((issue) => issue.labels?.includes("epic"))
    ).toHaveLength(1);
    expect(posts).toHaveLength(2);
  } finally {
    held.resolve();
    await context.close();
  }
});

test("reload mid Start next finishes the recorded order without repeating its completed rank @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const rows = [];
  for (const title of [
    "Queue anchor",
    "Review access rules",
    "Correct query ordering",
    "Publish operator documentation",
  ]) {
    const issue = await createIssue({ project: "CORE", title });
    await patchIssue(issue.key, { status: "todo", labels: ["legion"] });
    rows.push(issue);
  }
  const [anchor, first, middle, last] = rows;
  if (!anchor || !first || !middle || !last) throw new Error("Queue fixtures are missing");
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const completed = watchWrites(page, "PATCH", `/api/v1/issues/${last.key}`);
    let mode: "hold" | "refuse" | "through" = "hold";
    let held = false;
    await page.route(`**/api/v1/issues/${middle.key}`, async (route) => {
      if (route.request().method() !== "PATCH" || mode === "through") return route.fallback();
      if (mode === "hold") {
        held = true;
        return;
      }
      await route.fulfill({ status: 403, json: { error: "Rank refused during reload" } });
    });
    await page.goto("/projects/CORE/issues");
    for (const issue of [first, middle, last])
      await page.getByRole("checkbox", { name: `Select ${issue.key}`, exact: true }).check();
    await page
      .getByRole("toolbar")
      .getByRole("button", { name: "Start next", exact: true })
      .click();
    await expect.poll(() => held).toBe(true);
    expect(completed).toHaveLength(1);
    mode = "refuse";
    await page.reload();
    const interrupted = page.getByRole("region", { name: "Interrupted table action" });
    await expect(interrupted).toContainText("Start next");
    await expect(page.getByRole("region", { name: "Pending changes" })).toContainText("Not saved");
    mode = "through";
    await interrupted.getByRole("button", { name: "Finish", exact: true }).click();
    await expect(interrupted).toHaveCount(0);
    for (const [index, issue] of [first, middle, last, anchor].entries())
      await expect(
        page
          .getByRole("row", { name: `${issue.key} ${issue.title}`, exact: true })
          .getByRole("cell", { name: `#${index + 1}`, exact: true })
      ).toBeVisible();
    expect(completed).toHaveLength(1);
    await page.reload();
    await expect(interrupted).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("reload mid merge moves current open descendants then records one reason and closes @phone", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = await createIssue({ project: "CORE", title: "Source workflow" });
  const target = await createIssue({ project: "CORE", title: "Combined destination" });
  await patchIssue(source.key, { labels: ["epic"] });
  await patchIssue(target.key, { labels: ["epic"] });
  const bridge = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Archived intermediate",
  });
  const pendingChild = await createIssue({
    project: "CORE",
    parent: bridge.key,
    title: "Retain the retry key",
  });
  const completedChild = await createIssue({
    project: "CORE",
    parent: source.key,
    title: "Independent completed move",
  });
  await createMessage(bridge.key, { body: "Intermediate completed." });
  await patchIssue(bridge.key, { status: "done" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const completed = watchWrites(page, "PATCH", `/api/v1/issues/${completedChild.key}`);
    let mode: "hold" | "refuse" | "through" = "hold";
    await page.route(`**/api/v1/issues/${pendingChild.key}`, async (route) => {
      if (route.request().method() !== "PATCH" || mode === "through") return route.fallback();
      if (mode === "hold") return;
      await route.fulfill({ status: 403, json: { error: "Move refused during reload" } });
    });
    await page.goto("/projects/CORE/issues?group=epic");
    // The childless destination epic keeps a heading of its own, so this names the source's.
    const heading = page.locator("tr", {
      has: page.getByRole("link", { name: /Source workflow/ }),
    });
    await heading.getByRole("button", { name: "Epic actions ▾" }).click();
    await page.getByRole("combobox", { name: "Merge into another epic" }).selectOption(target.key);
    await page.getByRole("button", { name: "Merge…" }).click();
    await page
      .getByRole("dialog", { name: "Merge this epic?" })
      .getByRole("button", { name: "Merge", exact: true })
      .click();
    await expect
      .poll(() => getIssue(completedChild.key).then((issue) => issue.parent))
      .toBe(target.key);
    mode = "refuse";
    await page.reload();
    const interrupted = page.getByRole("region", { name: "Interrupted table action" });
    await expect(interrupted).toContainText(`Merge ${source.key} into ${target.key}`);
    expect((await getIssue(source.key)).status).not.toBe("done");
    expect(await listIssueMessages(source.key)).toHaveLength(0);
    const added = await createIssue({
      project: "CORE",
      parent: bridge.key,
      title: "New descendant created during interruption",
    });
    mode = "through";
    await interrupted.getByRole("button", { name: "Finish", exact: true }).click();
    await expect.poll(() => getIssue(source.key).then((issue) => issue.status)).toBe("done");
    for (const child of [pendingChild, completedChild, added])
      expect((await getIssue(child.key)).parent).toBe(target.key);
    expect((await getIssue(bridge.key)).parent).toBe(source.key);
    expect((await listIssueMessages(source.key)).map(({ message }) => message.body)).toEqual([
      expect.stringContaining(`Merged into ${target.key}`),
    ]);
    expect(completed).toHaveLength(1);
    await expect(interrupted).toHaveCount(0);
    await page.reload();
    await expect(interrupted).toHaveCount(0);
    expect(await listIssueMessages(source.key)).toHaveLength(1);
  } finally {
    await context.close();
  }
});

// B1: a `?status=` this bundle does not know — a bookmark, a link from an older deploy — is a
// filter that matches nothing. The page it opens is the table, never the error boundary.
test("a status the bundle does not know leaves the whole page standing", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Status row" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    for (const status of ["bogus", "Done", "in-progress"]) {
      await page.goto(`/projects/CORE/issues?status=${status}`);
      await expect(page.getByRole("table", { name: "Project issues table" })).toBeVisible();
      await expect(page.getByRole("searchbox", { name: "Search issues" })).toBeVisible();
      await expect(page.getByText("Something went wrong in this page")).toHaveCount(0);
      await page.getByRole("button", { name: /^Filter Status/ }).click();
      await expect(
        page.getByRole("option", { name: new RegExp(`^${status}`) }).first()
      ).toBeVisible();
      await page.keyboard.press("Escape");
    }
  } finally {
    await context.close();
  }
});

// B2: a closed `<select>` commits on an arrow key in Chrome on Linux and Windows. The merge
// select stages a target instead, and the confirmation is what sends anything.
test("an arrow key on the merge select closes no epic, and the confirmed merge does", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const source = await createIssue({ project: "CORE", title: "Arrow source" });
  const target = await createIssue({ project: "CORE", title: "Arrow target" });
  await patchIssue(source.key, { labels: ["epic"] });
  await patchIssue(target.key, { labels: ["epic"] });
  const child = await createIssue({ project: "CORE", parent: source.key, title: "Arrow child" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues?group=epic");
    const heading = page.locator("tr", { has: page.getByRole("link", { name: /Arrow source/ }) });
    await heading.getByRole("button", { name: "Epic actions ▾" }).click();
    const select = page.getByRole("combobox", { name: "Merge into another epic" });
    await select.focus();
    await page.keyboard.press("ArrowDown");
    // Nothing must follow the keystroke: a merge the arrow key had started would leave the page
    // in that keypress's own task, so this window is where its write and its dialog would show.
    await page.waitForTimeout(500);
    expect((await getIssue(source.key)).status).not.toBe("done");
    expect((await getIssue(child.key)).parent).toBe(source.key);
    await expect(page.getByRole("dialog", { name: "Merge this epic?" })).toHaveCount(0);

    // The confirmed path still merges, and says what it is about to do first.
    await select.selectOption(target.key);
    await page.getByRole("button", { name: "Merge…" }).click();
    const asked = page.getByRole("dialog", { name: "Merge this epic?" });
    await expect(asked).toContainText(source.key);
    await expect(asked).toContainText(target.key);
    await expect(asked).toContainText("closes as done");
    await asked.getByRole("button", { name: "Merge", exact: true }).click();
    await expect.poll(() => getIssue(source.key).then((issue) => issue.status)).toBe("done");
    expect((await getIssue(child.key)).parent).toBe(target.key);
  } finally {
    await context.close();
  }
});

// F2 (dispatch://INFRA-2542 item 1): a press on a panel's own prose focuses nothing, and the
// panel must still be the Escape key's owner afterwards.
test("Escape closes a panel after a click on its own text", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const epic = await createIssue({ project: "CORE", title: "Hint text epic" });
  await patchIssue(epic.key, { labels: ["epic"] });
  await createIssue({ project: "CORE", parent: epic.key, title: "Hint text child" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE/issues?group=epic");
    const heading = page.locator("tr", { has: page.getByRole("link", { name: /Hint text epic/ }) });
    await heading.getByRole("button", { name: "Epic actions ▾" }).click();
    const hint = page.getByText("To split it, tick some of its issues");
    await expect(hint).toBeVisible();
    await hint.click();
    await page.keyboard.press("Escape");
    await expect(hint).toHaveCount(0);
    await expect(heading.getByRole("button", { name: "Epic actions ▾" })).toBeFocused();
  } finally {
    await context.close();
  }
});

// The rows of dispatch://INFRA-299 on the issues table, the project's and the ones of the
// Priorities view (`/priorities` and `/priorities/<id>`): while a pick is live - held by
// Playwright's route control at least `PICK_HOLD_MS` (`holdIssuePatches`) - its row shows it over
// every answer of the table's list, whose read another issue's write starts
// (`api/event-refresh.ts`: every event of an issue reads its project's list again; a status move
// reads the work under each priority, and the work under none). Each row records every value the
// row shows - its picker's (`watchShown`), or the plain text of its cell (`watchCell`) - at every
// animation frame, from the pick until the save ends.

// SLOW(>=2s): a save held three seconds while the table reads its list again.
test("picker keeps a pick on screen: the priority on the project table, over a list read that answers while its save is out", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Priority under a held save" });
  const other = await createIssue({ project: "CORE", title: "Another issue of the project" });
  await patchIssue(issue.key, { priority: 3 });
  const renamed = "Another issue, renamed while a save is out";
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${issue.key}`);
    const held = await holdIssuePatches(page, issue.key);
    const reads = logReads(page, listRead("project", "CORE"));
    await page.goto("/projects/CORE");
    const picker = page.getByRole("combobox", { name: `Priority of ${issue.key}` });
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // A rename of another issue, straight to the server: its live event has the table read the
    // project's list again, and the answer, carrying P3, lands while the pick's save is held.
    const renamedAt = Date.now();
    await patchIssue(other.key, { title: renamed });
    await expect(page.getByRole("link", { name: renamed })).toBeVisible();
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, renamedAt);

    expect(shown.values.slice(fromPick), "every value the row showed from the pick on").toEqual([
      "1",
    ]);
    expect(await getIssue(issue.key)).toMatchObject({ priority: 1 });
    await expect(picker).toHaveValue("1");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a save held three seconds while the table reads the work under the priority again.
test("picker keeps a pick on screen: the priority on the table of an item of the Priorities view, over a read that answers while its save is out", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const item = await createPriority({ owner: "alice", title: "Ship the release" });
  const issue = await createIssue({ project: "CORE", title: "Priority under a held save" });
  const other = await createIssue({ project: "CORE", title: "Another issue of the item" });
  await Promise.all([
    patchIssue(issue.key, { advances: [item.id], priority: 3, status: "in_progress" }),
    patchIssue(other.key, { advances: [item.id], status: "in_progress" }),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${issue.key}`);
    const held = await holdIssuePatches(page, issue.key);
    const reads = logReads(page, listRead("advances", item.id));
    await page.goto(`/priorities/${item.id}`);
    const picker = page.getByRole("combobox", { name: `Priority of ${issue.key}` });
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // A status move of the item's other issue, straight to the server: its live event has the
    // table read the work under the item again, and the answer, carrying P3, lands while the
    // pick's save is held.
    const movedAt = Date.now();
    await patchIssue(other.key, { status: "testing" });
    await expect.poll(() => readAnsweredSince(reads, movedAt)).toBe(true);
    await nextFrames(page);
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, movedAt);

    expect(shown.values.slice(fromPick), "every value the row showed from the pick on").toEqual([
      "1",
    ]);
    expect(await getIssue(issue.key)).toMatchObject({ priority: 1 });
    await expect(picker).toHaveValue("1");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a save held three seconds while the table reads the work under no priority again.
test("picker keeps a pick on screen: the priority on a table of the Priorities view's own page, over a read that answers while its save is out", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Priority under a held save" });
  const other = await createIssue({ project: "CORE", title: "Another issue in flight" });
  // Both are worked on under no priority, so `/priorities` lists both in its table of that fault.
  await Promise.all([
    patchIssue(issue.key, { priority: 3, status: "in_progress" }),
    patchIssue(other.key, { status: "in_progress" }),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${issue.key}`);
    const held = await holdIssuePatches(page, issue.key);
    const reads = logReads(page, listRead("advances", "none"));
    await page.goto("/priorities");
    const table = page.getByRole("region", { name: "In flight under no priority" });
    const picker = table.getByRole("combobox", { name: `Priority of ${issue.key}` });
    await expect(picker).toHaveValue("3");

    await picker.selectOption("1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // A status move of the table's other issue, straight to the server, to another status the
    // table lists: its live event has the table read the work under no priority again, and the
    // answer, carrying P3, lands while the pick's save is held.
    const movedAt = Date.now();
    await patchIssue(other.key, { status: "testing" });
    await expect.poll(() => readAnsweredSince(reads, movedAt)).toBe(true);
    await nextFrames(page);
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, movedAt);

    expect(shown.values.slice(fromPick), "every value the row showed from the pick on").toEqual([
      "1",
    ]);
    expect(await getIssue(issue.key)).toMatchObject({ priority: 1 });
    await expect(picker).toHaveValue("1");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a save held three seconds while the table of an item is opened and read, and read
// again for a status move.
test("picker keeps a pick on screen: the assignee in the table of an item of the Priorities view, picked on the issue page and shown from the table's first paint while its save is out", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const item = await createPriority({ owner: "alice", title: "Ship the release" });
  const issue = await createIssue({ project: "CORE", title: "Assignee under a held save" });
  const other = await createIssue({ project: "CORE", title: "Another issue of the item" });
  await Promise.all([
    patchIssue(issue.key, { advances: [item.id], assignee: "bob", status: "in_progress" }),
    patchIssue(other.key, { advances: [item.id], status: "in_progress" }),
  ]);
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    // The table draws the assignee as plain text in its Assignee column, which it opens shown.
    const shownInTable = await watchCell(page, issue.key, "Assignee");
    const held = await holdIssuePatches(page, issue.key);
    const reads = logReads(page, listRead("advances", item.id));
    await page.goto(`/issues/${issue.key}`);
    const picker = pagePicker(page, `Assignee of ${issue.key}`);
    await expect(picker).toHaveValue("bob");

    // Its people are read once the reader reaches for the control.
    await picker.focus();
    await picker.selectOption("alice");
    await expect(picker).toHaveValue("alice");
    await expect.poll(() => held.sent.length).toBe(1);
    // The picker leaves the screen with its save still out, and the work under the item is read
    // from the server, which still holds bob.
    const openedAt = Date.now();
    await navigateInApp(page, `/priorities/${item.id}`);
    await expect(page.getByRole("columnheader", { name: /^Assignee/ })).toBeVisible();
    await expect(page.locator(`tr[data-issue-row="${issue.key}"]`)).toBeVisible();
    await expect.poll(() => readAnsweredSince(reads, openedAt)).toBe(true);
    // A status move of the item's other issue, straight to the server: its live event has the
    // table read the work under the item again, and the answer, carrying bob, lands while the
    // pick's save is held.
    const movedAt = Date.now();
    await patchIssue(other.key, { status: "testing" });
    await expect.poll(() => readAnsweredSince(reads, movedAt)).toBe(true);
    await nextFrames(page);
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, openedAt);
    expectReadWhileHeld(reads, held, movedAt);

    // No row on the issue page (`(none)`), then the table's row from its first paint: alice
    // alone, never bob.
    expect(
      [...new Set(shownInTable.values.filter((value) => value !== "(none)"))],
      `every assignee the table's row showed: ${JSON.stringify(shownInTable.values)}`
    ).toEqual(["alice"]);
    expect(await getIssue(issue.key)).toMatchObject({ assignee: "alice" });
    await expect.poll(() => shownInTable.values.at(-1)).toBe("alice");
    await expect(page.locator(`tr[data-issue-row="${issue.key}"]`)).toContainText("alice");
  } finally {
    await context.close();
  }
});
