/** The shared search, sort and filter controls where proving them needs a server: seeded counts,
 *  writes, reloads, the keyboard, and the two pages a recording cannot answer - the Agents page,
 *  whose groups are a session's last-seen time against the clock, and the Delivery page, whose
 *  window is a range of dates. The rows that read only what a page draws over one fixed answer
 *  are in `table-controls.stub.e2e.ts`.
 */

import { CONTROL } from "../web/src/components/control-names";
import { plannerSession, reviewerSession, seedAgents, setLiveSessions } from "./agents";
import {
  createAsk,
  createComment,
  createIssue,
  createProject,
  mintAgentToken,
  patchIssue,
  putArchitectureSource,
  putRepoProject,
} from "./api";
import {
  PIPELINE_AT,
  seedDeliveryFixture,
  seedPipelineFixture,
  seedSessionGroupsFixture,
} from "./delivery-seed";
import {
  setCredentialGrants,
  setPendingCredentialRequests,
  setSignIns,
} from "./fake-broker-helpers";
import { seedFakeGithub } from "./fake-github-helpers";
import { control, controlRow, openFilterPanel } from "./filters";
import { drawer, peekTrigger } from "./inbox-drawer";
import { openFilterFields } from "./inbox-filter";
import { seedCommentThread } from "./inbox-threads";
import {
  bandHeadings,
  bandLabel,
  countsOf,
  drawnIds,
  lastActivity,
  seedEveryField,
  sortedIds,
  valuesOf,
} from "./inbox-view";
import { sessionIds } from "./session-ids";
import {
  closeControlPanel,
  drawnControlNames,
  heightOf,
  isPhoneProject,
  openDisplay,
  openSurface,
  pickInboxField,
  REAL_STACK_SURFACES,
  resetScroll,
  SURFACE_CONTROLS,
  SURFACE_SESSION,
  seedSurfaces,
  straysBetween,
  topOf,
  widthsOf,
} from "./table-controls";
import { expect, type Page, test } from "./test";
import { assertPageFits } from "./unclipped";

/** How far below its first reading the top of a list's first row may sit once a narrowing is
 *  on: one more line of chips, a 44 px control and the row's own 8 px gap. */
const CHIP_LINE = 52;

/** The Inbox's own ceiling: the first item starts this far down the window or less, with no
 *  narrowing on, with a panel open and with the display menu open. */
const INBOX_TOP = 203;
/** And this far down with three narrowings on, which is a second line of chips. */
const INBOX_TOP_NARROWED = 255;

/** The window the delivery fixture's two pull requests were merged in, with the list drawn. */
const DELIVERY_WINDOW = "from=2024-06-01T00%3A00%3A00Z&to=2024-06-02T00%3A00%3A00Z";

/** The seed the Agents and Delivery surfaces are read over. */
async function seedRealStackSurfaces(): Promise<{
  deliveryQuery: string;
  issue: string;
  project: string;
}> {
  const issue = await seedAgents();
  await seedDeliveryFixture();
  return { deliveryQuery: DELIVERY_WINDOW, issue, project: "CORE" };
}

// SLOW(>=2s): two pages opened at each of this project's widths, over two seeded fixtures.
test("the Agents page and the Delivery page each draw one row of controls, with nothing else between the title and the first row @phone", async ({
  page,
}, testInfo) => {
  const scene = await seedRealStackSurfaces();
  const phone = isPhoneProject(testInfo);
  const measured: string[] = [];
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of REAL_STACK_SURFACES) {
      const surface = await openSurface(page, name, scene, { phone });
      const where = `${name} at ${size.width}x${size.height}`;
      await expect(surface.row, `${where}: not exactly one control row`).toHaveCount(1);
      const height = Math.round(await heightOf(surface.row));
      measured.push(`${where}: control row ${height}px`);
      expect(height, `${where}: the control row has no height`).toBeGreaterThan(0);
      const strays = await straysBetween(
        surface.root,
        await topOf(surface.head),
        await topOf(surface.firstRow)
      );
      expect(
        strays,
        `${where} (control row ${height}px): ${strays.join("; ")} is drawn between the title and the first row and is outside the control row`
      ).toEqual([]);
      if (phone) await assertPageFits(page);
    }
  }
  await testInfo.attach("control row heights", {
    body: measured.join("\n"),
    contentType: "text/plain",
  });
});

// SLOW(>=2s): the same two pages, each narrowed and opened at each of this project's widths.
test("no narrowing or panel on the Agents page or the Delivery page moves the first row @phone", async ({
  page,
}, testInfo) => {
  const scene = await seedRealStackSurfaces();
  const phone = isPhoneProject(testInfo);
  for (const size of widthsOf(page, testInfo)) {
    if (!phone) await page.setViewportSize(size);
    for (const name of REAL_STACK_SURFACES) {
      const surface = await openSurface(page, name, scene, { phone });
      const where = `${name} at ${size.width}x${size.height}`;
      await resetScroll(page, surface.firstRow);
      const first = await topOf(surface.firstRow);

      await surface.narrow();
      await expect(surface.firstRow).toBeVisible();
      await resetScroll(page, surface.firstRow);
      const narrowed = await topOf(surface.firstRow);
      expect(
        narrowed - first,
        `${where}: one narrowing moved the first row from ${Math.round(first)}px to ${Math.round(narrowed)}px, which is more than the one line of chips the rule allows`
      ).toBeLessThanOrEqual(CHIP_LINE);
      expect(
        narrowed,
        `${where}: one narrowing moved the first row up, from ${Math.round(first)}px to ${Math.round(narrowed)}px`
      ).toBeGreaterThanOrEqual(first - 0.5);

      // Neither page draws a display menu or allows a selection, so the panel is the one act
      // left: it is portalled over the page and takes no room in the layout.
      if (surface.panel !== undefined) {
        await surface.panel();
        await resetScroll(page, surface.firstRow);
        const after = await topOf(surface.firstRow);
        expect(
          after,
          `${where}: the filter panel opened moved the first row from ${Math.round(narrowed)}px to ${Math.round(after)}px`
        ).toBeCloseTo(narrowed, 0);
        await closeControlPanel(surface.root);
      }
    }
  }
});

test("the Agents page and the Delivery page draw every shared control their design gives them, and no other", async ({
  page,
}) => {
  const scene = await seedRealStackSurfaces();
  await page.setViewportSize({ height: 900, width: 1440 });
  for (const name of REAL_STACK_SURFACES) {
    const surface = await openSurface(page, name, scene);
    // A chip is drawn only under a narrowing; neither page allows a selection.
    await surface.narrow();
    const expected = [...SURFACE_CONTROLS[name]].sort();
    for (const wanted of expected) {
      await expect(control(surface.root, wanted), `${name} draws no ${wanted}`).not.toHaveCount(0);
    }
    const drawn = await drawnControlNames(surface.root);
    const unexpected = drawn.filter((value) => !(expected as string[]).includes(value)).sort();
    expect(
      unexpected,
      `${name} draws ${unexpected.join(", ")}, which the design does not give it`
    ).toEqual([]);
  }
});

/** Twenty-two questions, so the list is longer than either window, with no credential request
 *  waiting and nothing selected. Each ask waits on Alice, who holds every issue. */
async function seedLongInbox(): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const priorities = [0, 1, 2, 3] as const;
  const urgencies = ["blocking", "high", "med", "low"] as const;
  for (let index = 0; index < 22; index++) {
    const issue = await createIssue({ project: "CORE", title: `Long inbox item ${index}` });
    await patchIssue(issue.key, { priority: priorities[index % 4] });
    await createAsk(
      issue.key,
      { question: `Item ${index}: what is the rule?`, urgency: urgencies[index % 4] },
      SURFACE_SESSION
    );
  }
}

// SLOW(>=2s): twenty-two seeded questions, read at two widths through four states each.
test("the Inbox's first item starts within 203 px of the window, and within 255 px under three narrowings", async ({
  page,
}) => {
  await seedLongInbox();
  const firstRow = page.locator("[data-inbox-row]").first();
  const main = page.getByTestId("main-content");
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
  ]) {
    await page.setViewportSize(size);
    await page.goto("/");
    await expect(firstRow).toBeVisible();
    await resetScroll(page, firstRow);
    const plain = await topOf(firstRow);
    expect(
      plain,
      `${size.width}x${size.height}: the first item starts ${plain}px down`
    ).toBeLessThanOrEqual(INBOX_TOP);

    // A panel draws over the page, so it takes no room in the layout.
    await openFilterPanel(page, main);
    await resetScroll(page, firstRow);
    const panelled = await topOf(firstRow);
    expect(
      panelled,
      `${size.width}x${size.height}: with the filter panel open the first item starts ${panelled}px down`
    ).toBeLessThanOrEqual(INBOX_TOP);
    await closeControlPanel(main);

    await openDisplay(page, main);
    await resetScroll(page, firstRow);
    const menued = await topOf(firstRow);
    expect(
      menued,
      `${size.width}x${size.height}: with the display menu open the first item starts ${menued}px down`
    ).toBeLessThanOrEqual(INBOX_TOP);

    // Three narrowings, opened in one address so the chips are the only thing that changed.
    await page.goto("/?priority=P0&urgency=blocking&type=question");
    await expect(firstRow).toBeVisible();
    // Three chips, one per narrowing: **Clear filters** beside them is a button, not a chip.
    await expect(control(controlRow(main), CONTROL.chip)).toHaveCount(3);
    await resetScroll(page, firstRow);
    const narrowed = await topOf(firstRow);
    expect(
      narrowed,
      `${size.width}x${size.height}: with three narrowings on the first item starts ${narrowed}px down`
    ).toBeLessThanOrEqual(INBOX_TOP_NARROWED);
  }
});

/** Nine narrowings over the long Inbox that still keep seventeen of its twenty-two items: three
 *  priorities, the three urgencies the seed pairs with them, two kinds and the one project. */
const NINE_NARROWINGS =
  "/?priority=P0,P1,P2&urgency=blocking,high,med&type=question,approval&project=CORE";

// SLOW(>=2s): twenty-two seeded questions, read at three widths with and without nine narrowings.
test("however many narrowings are on, their chips take at most one more line of the Inbox's row, at every width", async ({
  page,
}) => {
  await seedLongInbox();
  const firstRow = page.locator("[data-inbox-row]").first();
  const main = page.getByTestId("main-content");
  const readings: string[] = [];
  const over: string[] = [];
  for (const size of [
    { height: 900, width: 1440 },
    { height: 800, width: 1280 },
    { height: 664, width: 390 },
  ]) {
    await page.setViewportSize(size);
    await page.goto("/");
    await expect(firstRow).toBeVisible();
    await resetScroll(page, firstRow);
    const plain = await topOf(firstRow);

    await page.goto(NINE_NARROWINGS);
    await expect(firstRow).toBeVisible();
    // One chip per value picked, as the row draws them, so nine chips.
    await expect(control(controlRow(main), CONTROL.chip)).toHaveCount(9);
    await resetScroll(page, firstRow);
    const narrowed = await topOf(firstRow);
    const where = `${size.width}x${size.height}: nine narrowings moved the first item from ${Math.round(plain)}px to ${Math.round(narrowed)}px, ${Math.round(narrowed - plain)}px down`;
    readings.push(where);
    if (narrowed - plain > CHIP_LINE) over.push(where);
  }
  await test.info().attach("first item with and without nine narrowings", {
    body: readings.join("\n"),
    contentType: "text/plain",
  });
  // R1: the chips of the narrowings that are on may take one more line of the row, at most
  // 52 px, on any page and at every width the suite drives - not one more line per few chips.
  expect(
    over,
    `${over.join("; ")}, more than the ${CHIP_LINE}px of one more line of chips`
  ).toEqual([]);
});

/** The project List's narrowable columns and what shape each one's panel takes. No column of
 *  either table is a date or a number that narrows by two ends: Created and Updated narrow by
 *  the age bands they are grouped into, and Open asks by its own counts, so both are columns of
 *  values like the rest. */
const LIST_COLUMN_FILTERS: Record<string, "picks" | "text"> = {
  Assignee: "picks",
  "Claimed by": "picks",
  Created: "picks",
  "Created by": "picks",
  Epic: "picks",
  Key: "text",
  Labels: "picks",
  "Open asks": "picks",
  Priority: "picks",
  Queue: "picks",
  Status: "picks",
  Title: "text",
};

/** The Delivery list's, which are the five facets that are columns of that table. */
const DELIVERY_COLUMN_FILTERS: Record<string, "picks" | "text"> = {
  Author: "picks",
  Deployed: "picks",
  Issue: "picks",
  "Parent agent": "picks",
  Rework: "picks",
};

/** Reads one table's column filters: which headers carry one, that each sits inside its own
 *  header and at its right end, that a press opens exactly one panel, and what that panel
 *  holds. */
async function readColumnFilters(
  page: Page,
  table: string,
  expected: Record<string, "picks" | "text">
): Promise<void> {
  const headers = page.getByRole("columnheader");
  const named = await headers.evaluateAll((cells) =>
    cells.map((cell) => ({
      control: cell.querySelector('[data-control="column-filter"]') !== null,
      // The header's own label, which is its sort button's text or its plain text.
      name: (cell.querySelector("button")?.textContent ?? cell.textContent ?? "")
        .replace(/[▲▼]/g, "")
        .trim(),
      right: cell.getBoundingClientRect().right,
      filterRight: cell.querySelector('[data-control="column-filter"]')?.getBoundingClientRect()
        .right,
    }))
  );
  const carrying = named.filter((cell) => cell.control).map((cell) => cell.name);
  expect(
    [...carrying].sort(),
    `${table}: the headers carrying a filter are ${carrying.join(", ")}`
  ).toEqual(Object.keys(expected).sort());

  for (const cell of named) {
    if (!cell.control) continue;
    expect(
      (cell.filterRight ?? 0) - cell.right,
      `${table}: the ${cell.name} filter is not at the right end of its own header`
    ).toBeGreaterThanOrEqual(-24);
    expect((cell.filterRight ?? 0) - cell.right).toBeLessThanOrEqual(1);
  }

  for (const [name, kind] of Object.entries(expected)) {
    // A column that already narrows reads `Filter Status: 4 picked`, which is the project
    // List's own opening view.
    const trigger = page.getByRole("button", {
      name: new RegExp(`^Filter ${name}(: \\d+ picked)?$`),
    });
    await expect(trigger, `${table}: no filter control on ${name}`).toHaveCount(1);
    // A press on one header's funnel while another's panel is still shutting is taken by that
    // panel's own outside-dismiss, so the press is made until the trigger says it is open.
    await expect(async () => {
      if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
      expect(await trigger.getAttribute("aria-expanded")).toBe("true");
    }).toPass({ timeout: 10_000 });
    // One panel, and one only: a column of values opens a listbox over its options, a column
    // of free text a dialog holding the box to type in, and neither opens the other's.
    await expect(
      page.locator('[role="listbox"], [role="dialog"]'),
      `${table}: pressing the ${name} filter opened more than one panel`
    ).toHaveCount(1);
    if (kind === "text") {
      await expect(
        page.getByRole("dialog").getByRole("textbox", { name: `Filter ${name}` }),
        `${table}: the ${name} panel holds no text box`
      ).toHaveCount(1);
    } else {
      await expect(
        page.getByRole("combobox", { exact: true, name: `Search ${name}` }),
        `${table}: the ${name} panel holds no text box over its own options`
      ).toHaveCount(1);
    }
    await trigger.click();
    await expect(trigger).toHaveAttribute("aria-expanded", "false");
  }

  // Nothing draws a filter field between a header and the first row: every column filter of
  // this table is inside a header cell.
  const stray = await page.evaluate(() =>
    [...document.querySelectorAll('[data-control="column-filter"]')]
      .filter((element) => element.closest("th") === null)
      .map((element) => element.getAttribute("aria-label") ?? "")
  );
  expect(stray, `${table}: ${stray.join(", ")} is drawn outside a column header`).toEqual([]);
}

// SLOW(>=2s): two tables, every narrowable column of each opened and read.
test("every column that narrows does it from its own header, and no page draws a filter field under one", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  const scene = await seedSurfaces();
  const list = await openSurface(page, "project List", scene);
  await expect(list.firstRow).toBeVisible();
  await readColumnFilters(page, "the project List", LIST_COLUMN_FILTERS);

  // Typing in a column of values leaves only the matching options.
  const status = page.getByRole("button", { name: /^Filter Status(: \d+ picked)?$/ });
  await status.click();
  // The filter's own options: every row of the table draws a priority `select`, whose options
  // carry the same role.
  const options = page.getByRole("listbox", { name: "Status options" }).getByRole("option");
  const before = await options.count();
  await page.getByRole("combobox", { exact: true, name: "Search Status" }).fill("tod");
  await expect(options).toHaveCount(1);
  expect(before, "the Status filter offered only the one option before any text").toBeGreaterThan(
    1
  );
  await expect(options.first()).toHaveAccessibleName(/^Todo/);
  await status.click();

  await seedDeliveryFixture();
  const delivery = await openSurface(page, "Delivery page", {
    ...scene,
    deliveryQuery: DELIVERY_WINDOW,
  });
  await expect(delivery.firstRow).toBeVisible();
  await readColumnFilters(page, "the Delivery list", DELIVERY_COLUMN_FILTERS);
});

// SLOW(>=2s): four pages, each opened and its one trigger read.
test("every narrowing that is no column opens from its page's one filter trigger", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedAgents();
  const scene = await seedSurfaces();
  // The session-groups fixture rather than the timeline one: its pull requests name sessions,
  // and the Session facet draws nothing while no pull request in the window names one
  // (`Facet`, `features/delivery/DeliveryPage.tsx`). Its merges are in the last seven days,
  // which is the window the page opens on, so the Delivery surface below names none.
  await seedSessionGroupsFixture();

  const pages: { controls: string[]; name: Parameters<typeof openSurface>[1] }[] = [
    {
      controls: [
        "Filter by priority",
        "Filter by urgency",
        "Filter by type",
        "Filter by project",
        "Filter by assignee",
      ],
      name: "Inbox",
    },
    { controls: ["Labels", "Needs you", "Unread", "Unclaimed"], name: "project Board" },
    { controls: ["Filter by group", "Filter by machine", "Filter by role"], name: "Agents page" },
    { controls: ["Repository", "Session", "Priority", "Component"], name: "Delivery page" },
  ];
  for (const { controls, name } of pages) {
    const surface = await openSurface(page, name, { ...scene, deliveryQuery: "" });
    const triggers = control(controlRow(surface.root), CONTROL.panelTrigger);
    await expect(triggers, `${name} draws more than one filter trigger in its row`).toHaveCount(1);
    const panel = await openFilterPanel(page, surface.root);
    for (const label of controls) {
      // A narrowing is named either on its own control or beside it, which is what the
      // Delivery page's facets do.
      await expect(
        panel
          .getByRole("button", { name: new RegExp(`^${label}`) })
          .or(panel.getByText(label, { exact: true })),
        `${name}: its panel holds no ${label}`
      ).not.toHaveCount(0);
    }
    await closeControlPanel(surface.root);
  }
});

test("selecting a row moves no row, and the bar that appears is fixed over the list", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const scene = await seedSurfaces();
  for (const name of ["Inbox", "project List"] as const) {
    const surface = await openSurface(page, name, scene);
    await resetScroll(page, surface.firstRow);
    const before = await topOf(surface.firstRow);
    await surface.select?.();
    await resetScroll(page, surface.firstRow);
    const after = await topOf(surface.firstRow);
    expect(
      after,
      `${name}: selecting a row moved the first row from ${Math.round(before)}px to ${Math.round(after)}px`
    ).toBeCloseTo(before, 0);
    const bar = control(surface.root, CONTROL.selectionBar);
    await expect(bar, `${name}: no selection bar`).toBeVisible();
    await expect(bar, `${name}: the bar does not carry the count`).toContainText("1 selected");
    expect(
      await bar.evaluate((node) => getComputedStyle(node).position),
      `${name}: the selection bar is not fixed over the list`
    ).toBe("fixed");
  }
});

// SLOW(>=2s): eleven seeded rows read through each of the Inbox's own parameters.
test("each page's own parameters open the rows they name", async ({ page }) => {
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const idsWhere = (match: (row: (typeof rows)[number]) => boolean) =>
    rows.filter(match).map((row) => row.id);
  const expectRows = async (address: string, expected: readonly string[], why: string) => {
    await page.goto(address);
    await expect(page.locator("[data-inbox-row]")).toHaveCount(expected.length);
    expect([...(await drawnIds(page))].sort(), why).toEqual([...expected].sort());
  };

  // The Inbox: the four comma lists, the assignee, the text, the section and the agent.
  await expectRows(
    "/?assignee=everyone&type=question",
    idsWhere((row) => row.type === "question"),
    "?type= lists the questions"
  );
  await expectRows(
    "/?assignee=everyone&priority=P0",
    idsWhere((row) => row.priority === "P0"),
    "?priority= lists the P0 rows"
  );
  await expectRows(
    "/?assignee=everyone&urgency=blocking",
    idsWhere((row) => row.urgency === "blocking"),
    "?urgency= lists the blocking rows"
  );
  await expectRows(
    "/?assignee=everyone&project=MIRROR",
    idsWhere((row) => row.project === "MIRROR"),
    "?project= lists the Mirror rows"
  );
  await expectRows(
    "/?assignee=bob",
    idsWhere((row) => row.assignee === "bob"),
    "?assignee= lists one person's rows"
  );
  await expectRows(
    "/?assignee=everyone&q=Lease handover",
    idsWhere((row) => row.id === rows[2]?.id),
    "?q= lists the one row whose question holds those words"
  );
  // `?show=dismissed` is the reader's other list, which nothing has been dismissed into. The
  // reading is the **Show** control's own, not the empty list's: a list that has not drawn yet
  // is empty too, so a count of nothing would pass whatever the address said.
  await page.goto("/?assignee=everyone&show=dismissed");
  const showing = await openDisplay(page, page.getByTestId("main-content"));
  await expect(
    showing.getByRole("group", { name: "Show" }).locator('input[value="dismissed"]'),
    "?show=dismissed does not put the Show control on the dismissed list"
  ).toBeChecked();
  await page.keyboard.press("Escape");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(0);
  // `?sort=` and `?group=` keep every row and change its place.
  await page.goto("/?assignee=everyone&sort=priority&group=none");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  expect(await drawnIds(page), "?sort=priority orders by priority").toEqual(
    sortedIds(rows, "priority", at)
  );
  await page.goto("/?assignee=everyone&group=type");
  // A band heading reads its plural and its count in brackets.
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
  expect(await bandHeadings(page), "?group=type bands by type").toEqual(
    valuesOf(rows, "type").map((value) => {
      const plural = {
        agent: "Agent messages",
        approval: "Approvals",
        decision: "Decisions",
        question: "Questions",
        reply: "Replies",
      }[value];
      return `${plural} (${rows.filter((row) => row.type === value).length})`;
    })
  );
  // `?section=needs-you` is the asks waiting on the reader; `?agent=` one session's asks.
  await page.goto("/?section=needs-you");
  // Read the list once it has drawn: an address read before the first row lands is an empty
  // list whatever the address said, which is how CI run 43270255462 read 0 rows here while a
  // faster machine read them all.
  await expect(
    page.locator("[data-inbox-row]").first(),
    "?section=needs-you lists the asks waiting on the reader"
  ).toBeVisible();
  const waiting = await drawnIds(page);
  expect(
    waiting,
    "?section=needs-you drops the one question handed back to its agent"
  ).not.toContain(rows[2]?.id);
  expect(waiting.length, "?section=needs-you lists the asks waiting on the reader").toBeGreaterThan(
    0
  );
  await page.goto(`/?assignee=everyone&agent=${encodeURIComponent("e2e-inbox-view")}`);
  await expect(page.locator("[data-inbox-row]")).toHaveCount(0);
});

test("the project List, the Board and the Agents page open the rows their parameters name", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedSurfaces();
  const listed = async (address: string): Promise<string[]> => {
    await page.goto(address);
    await expect(page.getByRole("table", { name: "Project issues table" })).toBeVisible();
    return page
      .locator("[data-issue-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-issue-row") ?? ""));
  };
  // Six issues, CORE-1 to CORE-6, every one of them `todo`, holding one open ask, the odd ones
  // labelled `frontend` and the even ones `backend`.
  const every = ["CORE-1", "CORE-2", "CORE-3", "CORE-4", "CORE-5", "CORE-6"];
  expect(await listed("/projects/CORE/issues?sort=-key"), "?sort= orders by a column").toEqual(
    [...every].reverse()
  );
  expect(await listed("/projects/CORE/issues?q=Rollout"), "?q= searches the rows").toEqual([
    "CORE-1",
    "CORE-2",
  ]);
  expect(await listed("/projects/CORE/issues?label=frontend"), "?label= picks a label").toEqual([
    "CORE-1",
    "CORE-3",
    "CORE-5",
  ]);
  expect(await listed("/projects/CORE/issues?status=todo"), "?status= picks a status").toEqual(
    every
  );
  expect(
    await listed("/projects/CORE/issues?f.assignee=alice"),
    "?f.<column>= picks another column's value"
  ).toEqual(every);
  expect(await listed("/projects/CORE/issues?needs-you=1"), "?needs-you=1 keeps the asked").toEqual(
    every
  );
  expect(await listed("/projects/CORE/issues?unread=1"), "?unread=1 keeps the unread").toEqual(
    every
  );
  expect(
    await listed("/projects/CORE/issues?unclaimed=1"),
    "?unclaimed=1 keeps the unclaimed"
  ).toEqual(every);
  expect(
    await listed("/projects/CORE/issues?nofilter=1&size=10&page=1"),
    "?nofilter=, ?size= and ?page= page the whole list"
  ).toEqual(every);
  await page.goto("/projects/CORE/issues?group=status");
  await expect(
    page.getByRole("button", { name: /^Todo/ }).first(),
    "?group= bands the table"
  ).toBeVisible();

  // The Board reads the same five of them.
  const carded = async (address: string): Promise<string[]> => {
    await page.goto(address);
    await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
    return page
      .locator("[data-board-card]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-board-card") ?? ""));
  };
  await page.goto("/projects/CORE/issues");
  const board = page.getByRole("button", { exact: true, name: "Board" });
  if ((await board.getAttribute("aria-pressed")) !== "true") await board.click();
  expect(await carded("/projects/CORE/issues?label=backend"), "the Board's ?label=").toEqual([
    "CORE-2",
    "CORE-4",
    "CORE-6",
  ]);
  expect(await carded("/projects/CORE/issues?q=Ledger"), "the Board's ?q=").toEqual(["CORE-3"]);
  expect(await carded("/projects/CORE/issues?needs-you=1"), "the Board's ?needs-you=").toEqual(
    every
  );
  expect(await carded("/projects/CORE/issues?unread=1"), "the Board's ?unread=").toEqual(every);
  expect(await carded("/projects/CORE/issues?unclaimed=1"), "the Board's ?unclaimed=").toEqual(
    every
  );

  // The Agents page's own two.
  await setLiveSessions([plannerSession, reviewerSession]);
  await page.goto("/agents?q=Reviewer");
  await expect(page.locator(`[data-agent-row="${sessionIds.reviewer}"]`)).toBeVisible();
  await expect(page.locator(`[data-agent-row="${sessionIds.planner}"]`)).toHaveCount(0);
  await page.goto("/agents?machine=box-1");
  await expect(page.locator(`[data-agent-row="${sessionIds.reviewer}"]`)).toBeVisible();
  await expect(page.locator(`[data-agent-row="${sessionIds.planner}"]`)).toBeVisible();
  await page.goto("/agents?machine=nowhere");
  await expect(page.locator("[data-agent-row]")).toHaveCount(0);
});

test("the Delivery page's facet parameters open the pull requests they name", async ({ page }) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedDeliveryFixture();
  const listed = async (query: string): Promise<string[]> => {
    await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list&${query}`);
    await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
    await expect(page.locator("table")).not.toHaveCount(0);
    return page
      .locator("[data-pr-row]")
      .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-pr-row") ?? ""));
  };
  // Two pull requests in the window: #1, shipped and attributed to ACME-1, and #2, a rework
  // with no issue. The page spells each facet in the address with its own key, not the
  // server's: `parentAgent`, never `parent_agent`.
  // The list opens newest merge first, so #2, merged at 03:00, is above #1, merged at 01:00.
  const both = ["acme/widgets#2", "acme/widgets#1"];
  expect(await listed("repo=acme%2Fwidgets"), "?repo=").toEqual(both);
  expect(await listed("author=octocat"), "?author=").toEqual(both);
  expect(await listed("issue=ACME-1"), "?issue=").toEqual(["acme/widgets#1"]);
  expect(await listed("rework=rework"), "?rework=").toEqual(["acme/widgets#2"]);
  expect(await listed("deployed=deployed"), "?deployed=").toEqual(["acme/widgets#1"]);
  expect(await listed("parentAgent=__no_session__"), "?parentAgent=").toEqual(both);
  // The fixture's two pull requests name no session, and the server counts none under the
  // placeholder, so this parameter narrows the list to nothing where `parentAgent` keeps both:
  // the two are read apart.
  expect(await listed("session=__no_session__"), "?session=").toEqual([]);
  // ACME-1, the issue #1 is attributed to, is the window's one P0; #2 links no issue at all.
  expect(await listed("priority=0"), "?priority=").toEqual(["acme/widgets#1"]);
  // ACME-1 carries no component, and #2 links no issue at all, so the server counts only the
  // attributed pull request under the placeholder.
  expect(await listed("component=__no_component__"), "?component=").toEqual(["acme/widgets#1"]);
  // And the window and the mode: a preset window outside the fixture's day lists nothing, and
  // without `mode=list` there is no table at all.
  await page.goto(`/delivery?window=7d&mode=list`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await expect(page.locator("[data-pr-row]")).toHaveCount(0);
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await expect(page.locator("[data-pr-row]")).toHaveCount(0);
});

// SLOW(>=2s): four parameters, each set, read in the address and found again after a reload.
test("the four parameters this change adds ride in the address and survive a reload, and the drawer writes none", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await seedAgents();
  const scene = await seedSurfaces();
  await seedDeliveryFixture();

  // The Agents page's `?group=` and `?role=`, neither in the address until its narrowing is on.
  await page.goto("/agents");
  await expect(page).not.toHaveURL(/[?&]group=/);
  await expect(page).not.toHaveURL(/[?&]role=/);
  const agents = page.getByTestId("main-content");
  await openFilterPanel(page, agents);
  for (const [name, option] of [
    ["group", "Needs you"],
    ["role", "planner"],
  ] as const) {
    const trigger = page.getByRole("button", { name: `Filter by ${name}` });
    if ((await trigger.getAttribute("aria-expanded")) !== "true") await trigger.click();
    await page.getByRole("option", { exact: true, name: option }).click();
    await trigger.click();
  }
  await closeControlPanel(agents);
  await expect(page).toHaveURL(/[?&]group=/);
  await expect(page).toHaveURL(/[?&]role=planner/);
  const agentsAddress = new URL(page.url()).search;
  await page.reload();
  expect(new URL(page.url()).search, "the Agents page's narrowing is gone after a reload").toBe(
    agentsAddress
  );
  await expect(control(controlRow(agents), CONTROL.chip)).not.toHaveCount(0);

  // The Artifacts tab's `?q=`.
  await page.goto(`/issues/${scene.issue}/artifacts`);
  await expect(page).not.toHaveURL(/[?&]q=/);
  const tab = page.getByTestId("main-content");
  await control(controlRow(tab), CONTROL.searchBox).fill("plan");
  await expect(page).toHaveURL(/[?&]q=plan/);
  await page.reload();
  await expect(page, "the Artifacts tab's narrowing is gone after a reload").toHaveURL(
    /[?&]q=plan/
  );
  await expect(control(controlRow(tab), CONTROL.searchBox)).toHaveValue("plan");
  await expect(page.locator('[data-testid^="artifact-"]')).toHaveCount(1);

  // The Delivery page's `?sort=`, absent until a header is pressed.
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  await expect(page.locator("[data-pr-row]").first()).toBeVisible();
  await expect(page).not.toHaveURL(/[?&]sort=/);
  await page.getByRole("button", { exact: true, name: "Author" }).click();
  await expect(page).toHaveURL(/[?&]sort=author/);
  await page.reload();
  await expect(page, "the Delivery page's sort is gone after a reload").toHaveURL(
    /[?&]sort=author/
  );

  // The drawer's own state is its own: narrowing it over the project List writes no parameter.
  await page.goto(`/projects/${scene.project}/issues`);
  const list = page.getByRole("button", { exact: true, name: "List" });
  if ((await list.getAttribute("aria-pressed")) !== "true") await list.click();
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  const before = page.url();
  await peekTrigger(page).click();
  const peek = drawer(page);
  await expect(peek.locator("[data-inbox-row]").first()).toBeVisible();
  await pickInboxField(page, "type", /^Question,/, peek);
  await expect(control(controlRow(peek), CONTROL.chip)).not.toHaveCount(0);
  expect(page.url(), "the drawer wrote its narrowing into the page's address").toBe(before);
});

// SLOW(>=2s): eleven seeded rows worked through every field, sort, grouping and view.
test("the Inbox keeps every field, sort, grouping, view and clear it had", async ({ page }) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  const { rows } = await seedEveryField();
  const at = await lastActivity();
  const main = page.getByTestId("main-content");
  const chips = control(controlRow(main), CONTROL.chip);
  await page.goto("/?assignee=everyone");
  await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);

  // Each of the four fields in turn: only the matching rows are left, and a chip for it is in
  // the control row.
  for (const [field, value, matches] of [
    ["type", /^Question,/, (row: (typeof rows)[number]) => row.type === "question"],
    ["priority", /^P0,/, (row: (typeof rows)[number]) => row.priority === "P0"],
    ["urgency", /^Blocking,/, (row: (typeof rows)[number]) => row.urgency === "blocking"],
    ["project", /^CORE,/, (row: (typeof rows)[number]) => row.project === "CORE"],
  ] as const) {
    await page.goto("/?assignee=everyone");
    await pickInboxField(page, field, value, main);
    const expectedIds = rows
      .filter(matches)
      .map((row) => row.id)
      .sort();
    // The list is read once it holds the narrowed count, never in the frame the pick landed in.
    await expect(page.locator("[data-inbox-row]")).toHaveCount(expectedIds.length);
    expect([...(await drawnIds(page))].sort(), `the ${field} field`).toEqual(expectedIds);
    await expect(chips.filter({ hasText: new RegExp(`^${field}`, "i") })).not.toHaveCount(0);
  }

  // The count beside each value of another field is the count under the narrowing that is on.
  await page.goto("/?assignee=everyone&priority=P0");
  const fields = await openFilterFields(page);
  const typeTrigger = page.getByRole("button", { name: "Filter by type" });
  await typeTrigger.click();
  const names = await page
    .getByRole("listbox", { name: "Type options" })
    .getByRole("option")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("aria-label") ?? ""));
  expect(
    names.find((name) => name.startsWith("Question")),
    "the Type counts are not taken under the priority narrowing"
  ).toBe(
    `Question, ${rows.filter((row) => row.priority === "P0" && row.type === "question").length} rows`
  );
  await typeTrigger.click();
  await expect(fields).toBeVisible();
  await closeControlPanel(main);

  // The assignee control.
  await page.goto("/?assignee=bob");
  const bobsIds = rows
    .filter((row) => row.assignee === "bob")
    .map((row) => row.id)
    .sort();
  await expect(page.locator("[data-inbox-row]")).toHaveCount(bobsIds.length);
  expect([...(await drawnIds(page))].sort(), "the assignee control").toEqual(bobsIds);

  // The six sorts and the seven groupings, worked from the display menu.
  await page.goto("/?assignee=everyone&group=none");
  for (const sort of ["newest", "priority", "urgency", "type", "project", "assignee"] as const) {
    const menu = await openDisplay(page, main);
    await menu.getByRole("group", { name: "Sort by" }).locator(`input[value="${sort}"]`).click();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    if (sort !== "assignee") {
      expect(await drawnIds(page), `the ${sort} sort`).toEqual(sortedIds(rows, sort, at));
    }
  }
  await page.keyboard.press("Escape");
  // Each banding draws the bands the fixture holds, each with its own count: the five fields
  // one band per value they hold, the turn bands the four rows waiting on a person and the one
  // handed back to its session, and None no band at all.
  for (const group of [
    "priority",
    "urgency",
    "type",
    "project",
    "assignee",
    "waiting-on",
    "none",
  ] as const) {
    const menu = await openDisplay(page, main);
    await menu.getByRole("group", { name: "Group by" }).locator(`input[value="${group}"]`).click();
    await expect(page.locator("[data-inbox-row]")).toHaveCount(rows.length);
    const expected =
      group === "none"
        ? []
        : group === "waiting-on"
          ? [`Waiting on a person (${rows.length - 1})`, "Waiting on agents (1)"]
          : valuesOf(rows, group).map(
              (value) => `${bandLabel(group, value)} (${countsOf(rows, group).get(value)})`
            );
    expect(await bandHeadings(page), `the ${group} grouping`).toEqual(expected);
  }
  await page.keyboard.press("Escape");

  // The dismissed view and the thread order.
  const menu = await openDisplay(page, main);
  await menu.getByRole("group", { name: "Show" }).locator('input[value="dismissed"]').click();
  await expect(page).toHaveURL(/show=dismissed/);
  await menu.getByRole("group", { name: "Show" }).locator('input[value="inbox"]').click();
  await menu.getByRole("group", { name: "Thread order" }).locator("input").last().click();
  await expect(
    menu.getByRole("group", { name: "Thread order" }).locator("input").last()
  ).toBeChecked();
  await page.keyboard.press("Escape");

  // Mark all read, and the fold of one group.
  await page.goto("/?assignee=everyone&group=type");
  const markAll = page.locator("[data-inbox-mark-all]");
  await expect(markAll).toBeVisible();
  await markAll.click();
  await expect(markAll).toHaveCount(0);
  const band = page.getByTestId("inbox-list").locator("h2 button").first();
  await band.click();
  await expect(band).toHaveAttribute("aria-expanded", "false");

  // Clear filters takes off the four fields, the assignee, the agent and the needs-you section,
  // and leaves the text, the sort, the grouping and the view.
  await page.goto(
    "/?assignee=bob&type=question&priority=P0&urgency=blocking&project=CORE&section=needs-you&q=rule&sort=priority&group=type&show=dismissed"
  );
  await page.locator("[data-inbox-clear-filters]").click();
  const address = new URL(page.url());
  for (const gone of ["assignee", "type", "priority", "urgency", "project", "section", "agent"]) {
    expect(address.searchParams.has(gone), `Clear filters left ?${gone}=`).toBe(false);
  }
  expect(address.searchParams.get("q"), "Clear filters took the text").toBe("rule");
  expect(address.searchParams.get("sort"), "Clear filters took the sort").toBe("priority");
  expect(address.searchParams.get("group"), "Clear filters took the grouping").toBe("type");
  expect(address.searchParams.get("show"), "Clear filters took the view").toBe("dismissed");
});

// SLOW(>=2s): the List's whole layout worked and then read back after a reload.
test("the project List keeps its grouping, columns, pager and layout, and the Board its disclosure", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await createProject({ key: "CORE", name: "Core" });
  for (let index = 0; index < 60; index++) {
    const issue = await createIssue({ project: "CORE", title: `Listed work ${index}` });
    await patchIssue(issue.key, { labels: ["legion"], status: "todo" });
  }
  await page.goto("/projects/CORE/issues");
  const list = page.getByRole("button", { exact: true, name: "List" });
  if ((await list.getAttribute("aria-pressed")) !== "true") await list.click();
  const table = page.getByRole("table", { name: "Project issues table" });
  await expect(table).toBeVisible();
  const main = page.getByTestId("main-content");

  // The bar of status counts, which this change leaves alone.
  await expect(page.getByRole("button", { name: /^Todo/ }).first()).toBeVisible();

  // The virtual rows: the table draws a window of its page, never all sixty.
  const rows = page.locator("[data-issue-row]");
  const drawn = await rows.count();
  expect(drawn, "the table drew every row of the page at once").toBeLessThan(50);
  expect(drawn, "the table drew no rows").toBeGreaterThan(0);

  // The pager: its own words, and a second page.
  const pager = control(main, CONTROL.pager);
  await expect(pager).toContainText("60 of 60 issues");
  await pager.getByRole("button", { name: "Next" }).click();
  await expect(page).toHaveURL(/[?&]page=2/);
  await pager.getByRole("combobox").selectOption("100");
  await expect(page).toHaveURL(/[?&]size=100/);

  // The grouping, from the display menu.
  const menu = await openDisplay(page, main);
  await menu.getByRole("group", { name: "Group by" }).locator('input[value="status"]').click();
  await expect(page).toHaveURL(/[?&]group=status/);
  await page.keyboard.press("Escape");

  // The column visibility, from the columns menu.
  await control(controlRow(main), CONTROL.columnsMenu).click();
  await page
    .getByRole("dialog", { name: "Columns" })
    .getByRole("checkbox", { exact: true, name: "Updated" })
    .check();
  await page.keyboard.press("Escape");
  const updated = page
    .getByRole("columnheader")
    .filter({ has: page.getByRole("button", { exact: true, name: /^Updated/ }) });
  await expect(updated).toHaveCount(1);

  // The column resize, dragged on the header's own edge.
  const header = page
    .getByRole("columnheader")
    .filter({ has: page.getByRole("button", { exact: true, name: /^Epic/ }) });
  const before = await header.boundingBox();
  if (before === null) throw new Error("the Epic column has no box");
  await page.mouse.move(before.x + before.width - 2, before.y + before.height / 2);
  await page.mouse.down();
  await page.mouse.move(before.x + before.width + 60, before.y + before.height / 2, { steps: 8 });
  await page.mouse.up();
  const after = await header.boundingBox();
  expect(after?.width ?? 0, "the Epic column did not resize").toBeGreaterThan(before.width + 20);

  // The column reorder. The header moves a column on HTML5 drag events - `dragstart` on its
  // sort button, `dragover` and `drop` on the header it lands in (`components/DataTable.tsx`) -
  // which a synthesised mouse drag never produces, so the events are dispatched as the browser
  // would, sharing one `DataTransfer` so the drop reads what the start wrote.
  const order = async () =>
    page
      .getByRole("columnheader")
      .evaluateAll((cells) =>
        cells.map((cell) => (cell.querySelector("button")?.textContent ?? "").replace(/[▲▼]/g, ""))
      );
  const orderBefore = await order();
  // The start and the drop are frames apart: the header cell takes a drop only once React has
  // re-rendered with the dragged column in hand, which it has not done in the tick the
  // `dragstart` was dispatched in.
  await page.evaluate(async () => {
    const headerOf = (label: string): HTMLElement | undefined =>
      [...document.querySelectorAll<HTMLElement>("th")].find((cell) =>
        (cell.querySelector("button")?.textContent ?? "").startsWith(label)
      );
    const handle = headerOf("Epic")?.querySelector("button");
    const to = headerOf("Labels");
    if (handle === null || handle === undefined || to === undefined) {
      throw new Error("the Epic and Labels headers are not both drawn");
    }
    const dataTransfer = new DataTransfer();
    handle.dispatchEvent(new DragEvent("dragstart", { bubbles: true, dataTransfer }));
    for (let frame = 0; frame < 3; frame++) {
      const drawn = Promise.withResolvers<void>();
      requestAnimationFrame(() => drawn.resolve());
      await drawn.promise;
    }
    to.dispatchEvent(new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer }));
    to.dispatchEvent(new DragEvent("drop", { bubbles: true, dataTransfer }));
    handle.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer }));
  });
  await expect.poll(order, { message: "the columns did not reorder" }).not.toEqual(orderBefore);

  // Reloaded, the stored layout is as it was left.
  const widths = await page
    .getByRole("columnheader")
    .evaluateAll((cells) => cells.map((cell) => Math.round(cell.getBoundingClientRect().width)));
  const kept = await order();
  await page.reload();
  await expect(table).toBeVisible();
  await expect
    .poll(order, { message: "the stored column order is not as it was left" })
    .toEqual(kept);
  expect(
    await page
      .getByRole("columnheader")
      .evaluateAll((cells) => cells.map((cell) => Math.round(cell.getBoundingClientRect().width))),
    "the stored column widths are not as they were left"
  ).toEqual(widths);

  // The Board: the filters open, the last chip cleared, and the disclosure as it was left.
  const board = page.getByRole("button", { exact: true, name: "Board" });
  await board.click();
  await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
  await page.goto("/projects/CORE/issues?needs-you=1&unread=1");
  const boardChips = control(controlRow(main), CONTROL.chip);
  await expect(boardChips).toHaveCount(2);
  await boardChips.last().click();
  await expect(boardChips).toHaveCount(1);
  const edges = page.getByRole("button", { name: /Icebox & Done$/ });
  const edgesLabel = await edges.textContent();
  await edges.click();
  await page.reload();
  await expect(
    page.getByRole("button", { name: /Icebox & Done$/ }),
    "the Board's disclosure is not as it was left"
  ).not.toHaveText(edgesLabel ?? "");
});

test("the Delivery list still answers the keyboard and stays windowed, and each page's own bindings still run", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await seedDeliveryFixture();
  await page.goto(`/delivery?${DELIVERY_WINDOW}&mode=list`);
  const prs = page.locator("tbody tr[data-pr-row]");
  await expect(prs).toHaveCount(2);
  await prs.first().focus();
  await page.keyboard.press("ArrowDown");
  await expect(prs.nth(1)).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await expect(prs.first()).toBeFocused();
  await page.keyboard.press("End");
  await expect(prs.nth(1)).toBeFocused();
  await page.keyboard.press("Home");
  await expect(prs.first()).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog", { name: "Details" })).toBeVisible();
  await page.keyboard.press("Escape");
  // The rows are drawn inside the shared table's scroll box, which is the thing that windows
  // them. This fixture holds two pull requests, fewer than any window would cut, so nothing
  // read here could fail for a window that was lost; what the windowing itself is held to is
  // the project List's row above, which seeds 60 issues and reads a drawn count under 50.
  await expect(
    control(page.getByTestId("main-content"), CONTROL.dataTable).locator("tbody tr[data-pr-row]"),
    "the Delivery list is not drawn in the shared table"
  ).toHaveCount(2);

  // The global binding that opens each page, and each page's own list binding.
  await seedAgents();
  await seedSurfaces();
  await page.goto("/");
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("a");
  await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
  await page.keyboard.press("g");
  await page.keyboard.press("i");
  await expect(page.getByRole("heading", { level: 1, name: "Inbox" })).toBeVisible();
  // The Inbox's own list keys: `j` moves the hand down the list and `x` marks the row.
  await page.locator("body").focus();
  await page.keyboard.press("j");
  await page.keyboard.press("x");
  await expect(control(page.getByTestId("main-content"), CONTROL.selectionBar)).toBeVisible();
  // The project page's own `v`, which flips List and Board.
  await page.goto("/projects/CORE/issues");
  await page.locator("body").focus();
  const boardBefore = await page
    .getByRole("button", { exact: true, name: "Board" })
    .getAttribute("aria-pressed");
  await page.keyboard.press("v");
  await expect(page.getByRole("button", { exact: true, name: "Board" })).not.toHaveAttribute(
    "aria-pressed",
    boardBefore ?? "false"
  );
});

// SLOW(>=2s): a seeded inbox of asks, threads and one credential request, read twice.
test("the control in the row carries what the banner carried, and narrows the Inbox to what waits on the reader", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await createProject({ key: "CORE", name: "Core" });
  // Three asks waiting on Alice, one waiting on Bob, and one handed back to its agent.
  const mine: string[] = [];
  for (const title of ["Ledger rollout", "Mirror retries", "Clock drift"]) {
    const issue = await createIssue({ project: "CORE", title });
    const ask = await createAsk(issue.key, { question: `${title}?` }, SURFACE_SESSION);
    mine.push(ask.id);
  }
  const bobs = await createIssue({ project: "CORE", title: "Lease handover" });
  await patchIssue(bobs.key, { assignee: "bob" });
  await createAsk(bobs.key, { question: "Whose lease?" }, SURFACE_SESSION);
  const answered = await createIssue({ project: "CORE", title: "Replay budget" });
  const answeredAsk = await createAsk(
    answered.key,
    { question: "How many replays?" },
    SURFACE_SESSION
  );
  // Handed back to its agent, so it waits on nobody here.
  await createComment(
    answered.key,
    { ask_id: answeredAsk.id, body: "Checking the budget now.", turn: "agent" },
    SURFACE_SESSION
  );
  // One unread thread, which the sidebar's own number adds and the control does not: a reply
  // under a comment the reader wrote, which is what makes it a thread rather than a comment.
  const thread = await createIssue({ project: "CORE", title: "Nightly mirror" });
  await seedCommentThread(
    thread.key,
    "Is the nightly job still at 02:00?",
    "It moved to 03:00 last week."
  );
  // And one credential request waiting on her.
  await setPendingCredentialRequests([
    {
      approver: "alice",
      identifiers: ["ANTHROPIC_API_KEY"],
      kind: "agent_secret",
      record_id: "record-table-controls",
      requested_at: "2026-09-27T00:00:00Z",
      session: { enrollment: null, request: null },
    },
  ]);

  await page.goto("/");
  const main = page.getByTestId("main-content");
  const list = page.getByTestId("inbox-list");
  await expect(list).toBeVisible();
  // No element above the list carries the words the banner carried.
  await expect(
    main.getByText(/^Blocked on you: /),
    "the old banner's words are still drawn above the Inbox's list"
  ).toHaveCount(0);

  // The control carries the asks waiting on her plus the credential request: three and one.
  const control_ = page.locator("[data-inbox-needs-you]");
  await expect(control_).toHaveText("Blocked on you · 4");
  await expect(control_).toHaveAttribute("aria-label", /^Blocked on you: 4 items/);
  const sidebar = page.getByRole("navigation", { name: "Navigation" }).getByRole("link", {
    name: /^Inbox/,
  });
  const sidebarCount = Number((await sidebar.textContent())?.replace(/\D+/g, "") ?? "0");
  expect(sidebarCount, "the sidebar's number does not add the unread thread").toBeGreaterThan(4);

  // A press narrows the list to the asks waiting on her, keeps the credential section, and puts
  // `section=needs-you` in the address while taking `assignee` out of it.
  await page.goto("/?assignee=bob");
  await page.locator("[data-inbox-needs-you]").click();
  await expect(page).toHaveURL(/section=needs-you/);
  await expect(page).not.toHaveURL(/assignee=/);
  const credentials = page.getByRole("region", { name: "Credential requests" });
  await expect(
    credentials,
    "the credential requests section went with the narrowing"
  ).toBeVisible();
  await expect(page.locator("[data-inbox-row]")).toHaveCount(mine.length);
  const drawn = await drawnIds(page);
  expect([...drawn].sort(), "the narrowed list is not the asks waiting on the reader").toEqual(
    [...mine].sort()
  );
  expect(
    drawn.length + (await credentials.getByRole("listitem").count()),
    "the page lists a different number of items than the control carries"
  ).toBe(4);

  // On the project page the same component's banner is drawn as it is today.
  await page.goto("/projects/CORE/issues");
  await expect(
    page.getByRole("link", { name: /^Blocked on you · \d+$/ }),
    "the project page's pill is gone"
  ).toBeVisible();
});

// SLOW(>=2s): seven tables across three pages, each seeded and read.
test("every table with no controls draws its rows in the shared markup and offers no search, filter or sort", async ({
  page,
}) => {
  await page.setViewportSize({ height: 900, width: 1600 });
  await createProject({ key: "CORE", name: "Core" });
  await putRepoProject("owner/repo", { project: "CORE" });
  await mintAgentToken("table-controls", "alice");
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: { "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  await setSignIns([
    {
      created_at: "2026-10-28T09:00:00Z",
      last_used_at: "2026-11-01T18:30:00Z",
      machine: "workstations@example-project.iam.gserviceaccount.com",
      person: "alice",
      signin_id: "11111111-1111-4111-8111-111111111111",
      thumbprint: "aaaabbbbccccddddeeeeffffgggghhhhiiiijjjjkkk",
    },
  ]);
  await setCredentialGrants([
    {
      approver: "alice",
      created_at: "2026-10-03T12:07:00Z",
      decided_by: "alice",
      enrollment: { kind: "pod", operator: "", runtime_id: "pod-7", slot: null },
      expires_at: "2026-10-03T12:22:00Z",
      grant_id: "22222222-2222-4222-8222-222222222222",
      granted: "approval",
      listedFor: "alice",
      names: ["BILLING_KEY"],
      record_id: "bb22cc33dd44ee55ff6677889900aa11bb22cc33dd44ee55ff6677889900aa11",
    },
  ]);

  /** Reads one plain table: it is inside the shared markup, it has rows, and it offers no
   *  search box, no panel trigger and no sort control of its own. */
  const readPlainTable = async (name: string): Promise<void> => {
    const shell = page.locator(`[data-control="${CONTROL.tableShell}"]`).filter({
      has: page.getByRole("table", { name }),
    });
    await expect(shell, `${name} is not drawn in the shared table markup`).toHaveCount(1);
    await expect(
      shell.getByRole("row"),
      `${name} draws no rows at all, not even a head`
    ).not.toHaveCount(0);
    for (const forbidden of [CONTROL.searchBox, CONTROL.panelTrigger, CONTROL.columnFilter]) {
      await expect(
        shell.locator(`[data-control="${forbidden}"]`),
        `${name} draws a ${forbidden}`
      ).toHaveCount(0);
    }
    // What a sort control is: the header button `DataTable`'s `TableHeaderCell` draws for a
    // sortable column. `th[aria-sort]` would say nothing here, since that attribute is written
    // only while a column IS sorted, so a freshly drawn sortable table carries none either.
    await expect(shell.locator("thead button"), `${name} draws a sort control`).toHaveCount(0);
    await expect(
      control(shell, CONTROL.dataTable),
      `${name} is drawn by the shared TanStack table rather than the plain shell`
    ).toHaveCount(0);
  };

  await page.goto("/settings");
  for (const name of [
    "Projects",
    "Repository mappings",
    "Agent tokens",
    "Architecture sources",
    "Sign-ins",
    "Live grants",
  ]) {
    await readPlainTable(name);
  }

  await seedPipelineFixture();
  await page.goto(`/pipeline?at=${PIPELINE_AT}`);
  await expect(page.getByRole("heading", { level: 1, name: "Pipeline" })).toBeVisible();
  for (const name of ["PR CI: slowest jobs", "PR CI: jobs that failed in the last 24 hours"]) {
    await readPlainTable(name);
  }

  // No second delivery fixture: `seedPipelineFixture` already wrote the one delivery settings
  // row, and a second insert is refused by its primary key. The measures table is behind the
  // panel's own fold, which the page opens shut.
  await page.goto(`/delivery?${DELIVERY_WINDOW}`);
  await expect(page.getByRole("heading", { level: 1, name: "Delivery timeline" })).toBeVisible();
  await page.getByRole("button", { name: /Definitions & alternatives/ }).click();
  await readPlainTable("Per day (UTC), newest first");
});
