/** The priorities page over one recorded answer of the priorities read: the tab of priority work
 *  in backlog or icebox with the row of an issue that states a live priority and stands in
 *  backlog, the priorities table sorted by the column an older link names and drawn by the layout
 *  a reader's browser kept under that column's former id, the list's layout kept under the key it
 *  had before, and the rows the priorities table shows at a filter on that column under its
 *  former id and under its id now, beside a second live priority under which no issue stands.
 *  Each reads what the browser draws from that fixed answer and what it keeps in its own storage,
 *  and none of it is a claim about what the server decides, so the row runs against a recording
 *  rather than against a harness (packages/dispatch/README.md, "The stubbed layer"). Each load
 *  after the first starts from a storage seeded as the release before left it: each table moves
 *  its layout on its first render (`moveTableStore` and `renameStoredColumns`,
 *  `web/src/features/project/issues-table-storage.ts`), and the priorities table reads an
 *  address's column id the same way (`PRIORITY_FORMER_IDS`), and no filter at all for a count
 *  column, which has none (`readTableView`,
 *  `web/src/features/priorities/priority-table-view.ts`). */
import { expect, type Page } from "@playwright/test";

import { createIssue, createPriority, createProject, patchIssue } from "./api";
import { stubbed } from "./stub/stub-test";

/** The id the priorities table's backlog column had in the release before, which a reader's
 *  stored layout and an older link still name. */
const FORMER_COLUMN_ID = "parked";

/** The browser keys the page keeps its layouts under (`tableStorageKey`): the list of priority
 *  work in backlog or icebox now and in the release before, and the priorities table, whose key
 *  did not change. */
const BACKLOG_KEY = "dispatch.project-table.priorities-backlog";
const FORMER_BACKLOG_KEY = "dispatch.project-table.priorities-parked";
const PRIORITIES_KEY = "dispatch.project-table.priorities";

/** The tab of the list, by its words and its one count. */
const BACKLOG_TAB = "Priority work in backlog or icebox 1";

/** The list's layout as a reader of the release before stored it: a width, a place and a column
 *  hidden that the list opens shown. That list has no column of the former id. */
const formerBacklogLayout = {
  sizing: { status: 222 },
  order: ["status", "key"],
  visibility: { project: false },
};

/** The priorities table's layout as that reader stored it: the backlog column, under its former
 *  id, wider than it opens, moved before Active, and shown. */
const formerPrioritiesLayout = {
  sizing: { [FORMER_COLUMN_ID]: 240 },
  order: ["owner", FORMER_COLUMN_ID, "in-flight", "waiting"],
  visibility: { [FORMER_COLUMN_ID]: true },
};

/** The priorities table's layout as another reader stored it: the backlog column hidden. */
const formerHiddenLayout = { visibility: { [FORMER_COLUMN_ID]: false } };

/** Loads `target` from a storage that holds `layouts` and nothing else, as a fresh page of a
 *  reader of the release before would. The page is on the app's origin already, so its storage is
 *  the one the next load reads. */
async function loadWithStorage(page: Page, target: string, layouts: Record<string, unknown>) {
  await page.evaluate((seeded) => {
    localStorage.clear();
    for (const [key, layout] of Object.entries(seeded)) {
      localStorage.setItem(key, JSON.stringify(layout));
    }
  }, layouts);
  await page.goto(target);
}

const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const priority = await createPriority({ owner: "alice", title: "Ship the daemon everywhere" });
  // A second live priority, under which no issue stands: its backlog count is 0, so a filter on
  // that count at the count the first priority's row holds would leave its row out.
  const second = await createPriority({ owner: "alice", title: "Retire the old dashboard" });
  const issue = await createIssue({ project: "CORE", title: "Daemon rollout to the fleet" });
  await patchIssue(issue.key, { advances: [priority.id], status: "backlog" });
  return { issue: issue.key, priority: priority.title, second: second.title, title: issue.title };
});

test("the priorities page lists priority work in backlog or icebox and moves the layouts a reader kept @pr", async ({
  page,
  scene,
}) => {
  // The first load, from an empty storage, on the tab of priority work in backlog or icebox: its
  // one count, and the row of the issue that stands in backlog under the live priority it states,
  // which names that priority by its place in the order and its title.
  await page.goto("/priorities?tab=backlog");
  await expect(page.getByRole("tab", { name: BACKLOG_TAB })).toHaveAttribute(
    "aria-selected",
    "true"
  );
  const section = page.getByRole("region", { name: "Priority work in backlog or icebox" });
  const table = section.getByRole("table", { name: "Priority work in backlog or icebox table" });
  const row = table.getByRole("row", { exact: true, name: `${scene.issue} ${scene.title}` });
  await expect(row).toBeVisible();
  await expect(row.getByRole("link", { name: `#1 ${scene.priority}` })).toBeVisible();
  await expect(row.getByText("Backlog", { exact: true })).toBeVisible();

  // The second load, from the layouts the release before stored, at an older link's sort: the
  // priorities table is sorted by the backlog column, which the address names by its former id,
  // and it draws that column at the reader's width and where the reader put it, before Active.
  await loadWithStorage(page, `/priorities?p.sort=${FORMER_COLUMN_ID}`, {
    [FORMER_BACKLOG_KEY]: formerBacklogLayout,
    [PRIORITIES_KEY]: formerPrioritiesLayout,
  });
  const priorities = page.getByRole("table", { name: "Priorities table" });
  // A count's sort arrow stands before its header's words, which stand at its right edge.
  const backlogHeader = priorities.getByRole("columnheader", { name: /Backlog, icebox$/ });
  await expect(backlogHeader).toHaveAttribute("aria-sort", "ascending");
  expect((await backlogHeader.boundingBox())?.width).toBeCloseTo(240, -1);
  const headers = await priorities.getByRole("columnheader").allTextContents();
  const at = (words: string) => headers.findIndex((text) => text.includes(words));
  expect(at("Backlog, icebox")).toBeGreaterThan(-1);
  expect(at("Backlog, icebox")).toBeLessThan(at("Active"));

  // The priorities table's stored layout moved on that first render: the width, place and
  // visibility of the backlog column are under its id now. The mount writes the layout whole, so
  // Closed, 7 days, which opens hidden, is written hidden beside it.
  const stored = async (key: string) =>
    page.evaluate((name) => JSON.parse(localStorage.getItem(name) ?? "null"), key);
  await expect
    .poll(() => stored(PRIORITIES_KEY))
    .toEqual({
      sizing: { backlog: 240 },
      order: ["owner", "backlog", "in-flight", "waiting"],
      visibility: { closed: false, backlog: true },
    });

  // The list's layout moves when its tab first draws it: under its key now, with the former key
  // gone.
  await page.getByRole("tab", { name: BACKLOG_TAB }).click();
  await expect(row).toBeVisible();
  await expect.poll(() => stored(FORMER_BACKLOG_KEY)).toBeNull();
  const backlog = await stored(BACKLOG_KEY);
  expect(backlog.sizing).toEqual(formerBacklogLayout.sizing);
  expect(backlog.order).toEqual(formerBacklogLayout.order);
  expect(backlog.visibility.project).toBe(false);

  // The third load, from a layout that hid the backlog column under its former id, at an older
  // link's filter on that column at the count the first priority's row holds: the column stays
  // hidden, under its id now. The fourth, from the same layout, at the same count under the
  // column's id now: the column hidden there too. A count has no filter (`hasFilter`), so each
  // filter, read as the column's, filters nothing: both loads show the same rows, and they are
  // every row the table holds, the second priority's, whose count is 0, among them.
  const heldCount = 1;
  const priorityRow = priorities.getByRole("row", { name: new RegExp(scene.priority) });
  const rowNames = () =>
    priorities
      .locator("[data-issue-row]")
      .evaluateAll((rows) => rows.map((drawn) => drawn.getAttribute("aria-label") ?? ""));
  await loadWithStorage(page, `/priorities?p.f.${FORMER_COLUMN_ID}=${heldCount}`, {
    [PRIORITIES_KEY]: formerHiddenLayout,
  });
  await expect(priorityRow).toBeVisible();
  await expect(backlogHeader).toHaveCount(0);
  await expect
    .poll(async () => (await stored(PRIORITIES_KEY))?.visibility)
    .toEqual({ closed: false, backlog: false });
  const underFormerId = await rowNames();
  await loadWithStorage(page, `/priorities?p.f.backlog=${heldCount}`, {
    [PRIORITIES_KEY]: formerHiddenLayout,
  });
  await expect(priorityRow).toBeVisible();
  await expect(backlogHeader).toHaveCount(0);
  const underId = await rowNames();

  // The rows the table shows with no filter, read in a fresh load of the same layout at the
  // address with no filter at all, so the filter is all that differs from the two loads above.
  await loadWithStorage(page, "/priorities", { [PRIORITIES_KEY]: formerHiddenLayout });
  await expect(priorityRow).toBeVisible();
  const unfiltered = await rowNames();
  expect(underId).toEqual(underFormerId);
  expect(underId).toEqual([`#1 ${scene.priority}`, `#2 ${scene.second}`]);
  expect(unfiltered).toEqual(underId);
});
