/** The command palette as the page draws it: which rows a route offers, where the arrows go,
 *  what a filtered list holds, which dialog a row opens, and how the list fits a short or
 *  shrinking window.
 *
 *  Every row here reads what the browser does with one fixed answer - the listing, the focus,
 *  the address a row navigates to, the panels a chord toggles - so each runs against a
 *  recording rather than against a harness (packages/dispatch/AGENTS.md, "The stubbed layer").
 *  What stayed in `keyboard-palette.e2e.ts`, and why, is written there.
 */
import { expect } from "@playwright/test";

import { QUERY_RETRY_DELAY_MS } from "../web/src/api/query-client";
import {
  createAsk,
  createIssue,
  createProject,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { selectedRow } from "./inbox-pane";
import { openIssue } from "./keyboard-palette";
import { stubbed } from "./stub/stub-test";

/** One issue per row, so each keeps the title it was written with, and the second project the
 *  `g p` and `Go to project…` rows pick from. */
const test = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Operations" });
  const issue = async (title: string) => (await createIssue({ project: "CORE", title })).key;
  return {
    astrolabe: await issue("Astrolabe calibration"),
    compactMargin: await issue("Compact margin"),
    documents: await issue("Documents from an issue"),
    failedSearch: await issue("Failed palette search"),
    longList: await issue("Long list"),
    openers: await issue("Openers"),
    panels: await issue("Panel toggles"),
    project: "CORE",
    railSearch: await issue("Rail search"),
    shortWindow: await issue("Short window"),
    shrinkingWindow: await issue("Shrinking window"),
  };
});

/** One ask in the Inbox, for the two rows that open the palette over a row in hand. */
const inboxTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Inbox palette" });
  const ask = await createAsk(
    issue.key,
    { options: [{ label: "Ship" }, { label: "Hold" }], question: "Ship it?" },
    { actor: { id: "e2e-palette", kind: "session" }, as: "agent" }
  );
  return { askId: ask.id, issue: issue.key };
});

/** One card in one project, so `j` from the board takes the card the row names. */
const boardTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Board palette" });
  return { issue: issue.key, project: "CORE" };
});

/** One row in one project's list, so `j` from the list takes the row the row names. */
const listTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "List palette" });
  return { issue: issue.key, project: "CORE" };
});

/** One architecture component, whose Open row the palette offers. The sync is the seeding's own
 *  check that the component reached the project, and runs in record mode alone. */
const architectureTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: { "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n" },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  expect((await syncArchitectureSource("CORE", "legion/arch")).last_error).toBeNull();
  return { project: "CORE" };
});

/** That component with a child, for the level that has somewhere to go up to. */
const nestedArchitectureTest = stubbed(async () => {
  await createProject({ key: "CORE", name: "Core" });
  await seedFakeGithub({
    "legion/arch": {
      contents: "read",
      files: {
        "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n",
        "web.md": "---\ntitle: Web\nparent: platform\n---\nThe SPA.\n",
      },
      installation_id: 101,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: "legion/arch" });
  expect((await syncArchitectureSource("CORE", "legion/arch")).last_error).toBeNull();
  return { project: "CORE" };
});

test("a failed palette search keeps its Retry action @pr", async ({ page, scene }) => {
  let searches = 0;
  await page.route(
    (url) => url.pathname === "/api/v1/search",
    async (route) => {
      searches += 1;
      await route.fulfill({
        body: JSON.stringify({ code: "INTERNAL", error: "search index unavailable" }),
        contentType: "application/json",
        status: 500,
      });
    }
  );
  // A failed read is retried twice, `QUERY_RETRY_DELAY_MS` apart, before the alert: page
  // timers, which the page's own clock fires.
  await page.clock.install();
  await openIssue(page, scene.failedSearch, "Failed palette search");
  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Search" });
  const input = page.getByRole("combobox", { name: "Search" });
  await input.fill("astrolabe");

  const failure = dialog.getByRole("alert");
  // Each refusal arms the next retry, so the jump repeats until the palette gives the read up.
  await expect
    .poll(
      async () => {
        await page.clock.runFor(QUERY_RETRY_DELAY_MS + 50);
        return failure.count();
      },
      { intervals: [50, 50, 100, 200, 500] }
    )
    .toBe(1);
  await expect(failure).toHaveText(/Search failed\./);
  const retry = failure.getByRole("button", { name: "Retry" });
  await expect(retry).toBeVisible();
  const searchesBeforeRetry = searches;
  await retry.click();
  await expect.poll(() => searches).toBeGreaterThan(searchesBeforeRetry);
});

test("g d opens the route's project Documents, from an issue key or a project path, and g p picks a project @pr", async ({
  page,
  scene,
}) => {
  // One page load; every later step is an in-app navigation, which is what the keys do.
  await openIssue(page, scene.documents, "Documents from an issue");

  // An issue page names its project only in its key.
  await page.keyboard.press("g");
  await page.keyboard.press("d");
  await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);

  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("p");
  const dialog = page.getByRole("dialog", { name: "Search" });
  await expect(dialog.getByRole("group", { name: "Projects" })).toBeVisible();
  const input = page.getByRole("combobox", { name: "Search" });
  await input.fill("operations");
  await expect(dialog.getByRole("option")).toHaveCount(1);
  await input.press("Enter");
  await expect(page).toHaveURL(/\/projects\/OPS\/issues/);

  // A project path names it outright. The heading is the page the key reads its route from.
  await expect(page.getByRole("heading", { level: 1, name: "Operations" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("g");
  await page.keyboard.press("d");
  await expect(page).toHaveURL(/\/projects\/OPS\/documents$/);
});

test("Shift+S and Shift+M toggle the sidebar and the margin, and the choice survives a reload @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await openIssue(page, scene.panels, "Panel toggles");
  await expect(page.getByTestId("sidebar-rail")).toHaveCount(0);
  await expect(page.getByTestId("margin-rail")).toHaveCount(0);

  await page.keyboard.press("Shift+S");
  await expect(page.getByTestId("sidebar-rail")).toBeVisible();
  await page.keyboard.press("Shift+M");
  await expect(page.getByTestId("margin-rail")).toBeVisible();

  await page.reload();
  await expect(page.getByTestId("sidebar-rail")).toBeVisible();
  await expect(page.getByTestId("margin-rail")).toBeVisible();

  await page.locator("body").focus();
  await page.keyboard.press("Shift+S");
  await expect(page.getByTestId("sidebar-rail")).toHaveCount(0);
  await page.keyboard.press("Shift+M");
  await expect(page.getByTestId("margin-rail")).toHaveCount(0);
});

test("the rail's Search control shows the same actions as the keyboard palette @pr", async ({
  page,
  scene,
}) => {
  await openIssue(page, scene.railSearch, "Rail search");
  await page.getByRole("button", { name: /^search/i }).click();

  const dialog = page.getByRole("dialog", { name: "Search" });
  const actions = dialog.getByRole("group", { name: "Actions" });
  await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
  await expect(actions.getByRole("option", { name: "Go to Inbox" })).toBeVisible();
});

inboxTest(
  "a focus-dependent row is offered, survives filtering, and runs on the row that opened the palette @pr",
  async ({ page, scene }) => {
    await page.goto("/");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(1);
    await page.locator("body").focus();
    // Every other inbox binding is gated on the row in hand, so `j` first: with the reading
    // column on (INFRA-575) that hand is the selection the column is showing, not the focus,
    // and the movement rows are still absent while the row's own action is offered.
    await page.keyboard.press("j");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.askId);
    await page.keyboard.press("Control+k");

    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(
      actions.getByRole("option", { name: "Open the row's issue, document or thread" })
    ).toHaveCount(1);
    await expect(actions.getByRole("option", { name: "Next row" })).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Previous row" })).toHaveCount(0);

    // Filtering re-renders the palette; the list is the one the opener's focus produced, so a
    // row a reader can see must still be there once they have typed enough to reach it.
    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("open the row");
    const open = actions.getByRole("option", { name: "Open the row's issue, document or thread" });
    await expect(open).toHaveCount(1);

    // And it runs against that row, not against the palette input that has focus while it is
    // shown: `o` on the row goes to the ask, so the palette row must go to the same place.
    await input.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${scene.issue}/asks/`));
  }
);

boardTest(
  "a board card's row runs on the card that opened the palette @pr",
  async ({ page, scene }) => {
    await page.setViewportSize({ height: 900, width: 1280 });
    await page.goto(`/projects/${scene.project}/issues`);
    await page.getByRole("button", { name: "Board" }).click();
    const card = page.getByRole("article", { name: `${scene.issue} Board palette` });
    await expect(card).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(card).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions.getByRole("option", { name: "Open issue" })).toHaveCount(1);
    // `Enter` on the card opens it too, and `Open issue` is already that action's row. Soft, so
    // one run names every row that should not be there.
    for (const absent of [
      "Next card",
      "Previous card",
      "Next column",
      "Previous column",
      "Open the focused card's issue",
    ]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }
    await actions.getByRole("option", { name: "Open issue" }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${scene.issue}`));
  }
);

test("a fresh open starts on an empty query, so it lists the new page's actions @pr", async ({
  page,
  scene,
}) => {
  const input = page.getByRole("combobox", { name: "Search" });
  const dialog = page.getByRole("dialog", { name: "Search" });
  const actions = dialog.getByRole("group", { name: "Actions" });

  await page.goto(`/projects/${scene.project}/issues`);
  await expect(page.getByRole("heading", { level: 1, name: "Core" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await input.fill("astrolabe");
  await expect(dialog.getByRole("option")).toHaveCount(1);
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);

  // In-app navigation, never a reload: a reload would reset the palette's own state, which is
  // exactly what must not be what makes this work.
  await page
    .getByRole("link", { name: /Astrolabe calibration/ })
    .first()
    .click();
  await expect(
    page
      .getByTestId("issue-header")
      .getByRole("heading", { level: 1, name: "Astrolabe calibration" })
  ).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(input).toHaveValue("");
  await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
});

listTest(
  "a List row's own row runs on the row that opened the palette, and its movement keys are no rows @pr",
  async ({ page, scene }) => {
    await page.goto(`/projects/${scene.project}/issues`);
    const row = page.locator(`[data-issue-row]`, { hasText: "List palette" });
    await expect(row).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions.getByRole("option", { name: "Open issue" })).toHaveCount(1);
    // `Enter` from the row opens it too, and `Open issue` is already that action's row.
    // Soft, so one run names every row that should not be there.
    for (const absent of ["Next issue", "Previous issue", "Open the focused issue"]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }
    await actions.getByRole("option", { name: "Open issue" }).click();
    await expect(page).toHaveURL(new RegExp(`/issues/${scene.issue}$`));
  }
);

inboxTest(
  "the Inbox offers the selection's snooze but not the keys that mark and clear it @pr",
  async ({ page, scene }) => {
    await page.goto("/");
    const row = page.locator("[data-inbox-row]");
    await expect(row).toHaveCount(1);
    await page.locator("body").focus();
    // `j` puts the row in hand: with the reading column on (INFRA-575) that is the selection
    // the column is showing. The reader's own place is the row it selected, which is where the
    // palette hands the focus back.
    await page.keyboard.press("j");
    await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", scene.askId);
    await row.focus();
    await page.keyboard.press("x");
    const bar = page.getByRole("group", { name: "Selected rows" });
    await expect(bar).toContainText("1 selected");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    const snooze = actions.getByRole("option", {
      name: "Snooze the focused ask, or every selected ask",
    });
    await page.keyboard.press("Control+k");
    await expect(snooze).toHaveCount(1);
    await expect
      .soft(actions.getByRole("option", { name: "Select or deselect the selected ask" }))
      .toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(row).toBeFocused();

    // Off the row, with the mark still made: Escape here clears the selection, and in the
    // palette Escape closes the palette, so that binding is no row. The snooze still is.
    await page.keyboard.press("Escape");
    await expect(row).not.toBeFocused();
    await expect(bar).toContainText("1 selected");
    await page.keyboard.press("Control+k");
    await expect(snooze).toHaveCount(1);
    await expect.soft(actions.getByRole("option", { name: "Clear the selection" })).toHaveCount(0);

    // It runs on the page the palette opened over: the bulk picker takes focus, the mark kept.
    await snooze.click();
    await expect(dialog).toHaveCount(0);
    await expect(bar.getByRole("combobox", { name: "Snooze selected asks" })).toBeFocused();
    await expect(bar).toContainText("1 selected");
  }
);

test("below xl the margin is the route's own sheet, so Shift+M and its row change nothing @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 800, width: 1024 });
  await openIssue(page, scene.compactMargin, "Compact margin");
  const stored = () => page.evaluate(() => localStorage.getItem("dispatch.shell.margin:alice"));
  const before = await stored();

  await page.keyboard.press("Control+k");
  const dialog = page.getByRole("dialog", { name: "Search" });
  const actions = dialog.getByRole("group", { name: "Actions" });
  await expect(actions.getByRole("option", { name: "Create issue" })).toBeVisible();
  // Soft, so one run names both rows if either is offered.
  for (const absent of ["Toggle margin", "Toggle sidebar"]) {
    await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);

  await page.locator("body").focus();
  await page.keyboard.press("Shift+M");
  await page.waitForTimeout(300);
  expect(await stored()).toBe(before);
});

architectureTest(
  "the Architecture tab offers its focused component's Open row, and not its movement keys @pr",
  async ({ page, scene }) => {
    await page.goto(`/projects/${scene.project}/architecture`);
    const row = page.locator('[data-component-row="platform"]');
    await expect(row).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    const open = actions.getByRole("option", {
      name: "Open the focused component: its children, else its details",
    });
    await expect(open).toHaveCount(1);
    // Soft, so one run names every row that should not be there.
    for (const absent of ["Next component", "Previous component"]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }
    await open.click();
    await expect(page).toHaveURL(/\/projects\/CORE\/architecture\?component=platform$/);
  }
);

test("Go to project… and Keyboard shortcuts are rows, and each opens its own dialog @pr", async ({
  page,
  scene,
}) => {
  await openIssue(page, scene.openers, "Openers");
  const dialog = page.getByRole("dialog", { name: "Search" });
  const actions = dialog.getByRole("group", { name: "Actions" });

  // A row runs once the palette has closed, so a row that opens the projects list opens it.
  await page.keyboard.press("Control+k");
  await actions.getByRole("option", { name: "Go to project…" }).click();
  await expect(dialog.getByRole("group", { name: "Projects" })).toBeVisible();
  await expect(page.getByRole("combobox", { name: "Search" })).toHaveAttribute(
    "placeholder",
    "Go to project"
  );
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);

  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await actions.getByRole("option", { name: "Keyboard shortcuts" }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeVisible();
});

test("the arrows keep the highlighted row in view when the list is longer than the palette @pr", async ({
  page,
  scene,
}) => {
  await openIssue(page, scene.longList, "Long list");
  await page.keyboard.press("Control+k");
  const input = page.getByRole("combobox", { name: "Search" });
  const rows = page
    .getByRole("dialog", { name: "Search" })
    .getByRole("group", { name: "Actions" })
    .getByRole("option");
  await expect(rows.first()).toBeInViewport();
  // The issue page's rows run past what the list shows at once; were they to fit, this row
  // would prove nothing.
  const last = rows.last();
  await expect(last).not.toBeInViewport();

  // ArrowUp from the first row wraps to the last, which the list scrolls into view.
  await input.press("ArrowUp");
  await expect(input).toHaveAttribute(
    "aria-activedescendant",
    (await last.getAttribute("id")) as string
  );
  await expect(last).toBeInViewport();
});

nestedArchitectureTest(
  "a nested Architecture level offers Up one level, and not the keys that move into children @pr",
  async ({ page, scene }) => {
    await page.goto(`/projects/${scene.project}/architecture?component=platform`);
    const row = page.locator('[data-component-row="web"]');
    await expect(row).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    const up = actions.getByRole("option", { name: "Up one level" });
    await expect(up).toHaveCount(1);
    // `l` into children is `o`'s Open row where there are children and nothing on a leaf, so
    // `descend` stays out of the palette.
    await expect(
      actions.getByRole("option", { name: "Into the focused component's children" })
    ).toHaveCount(0);
    await up.click();
    await expect(page).toHaveURL(/\/projects\/CORE\/architecture$/);
  }
);

test("on a short window the palette fits the screen and each highlighted row shows whole @pr", async ({
  page,
  scene,
}) => {
  await openIssue(page, scene.shortWindow, "Short window");
  const dialog = page.getByRole("dialog", { name: "Search" });
  const input = page.getByRole("combobox", { name: "Search" });
  // The list keeps one row's height, so the shortest windows here cut the key hints and the
  // message, never the row: 240 px is the least that holds the input and one row from `xl`,
  // 200 px close to it below.
  for (const [width, height] of [
    [1440, 300],
    [1440, 240],
    [1279, 200],
  ] as const) {
    await page.setViewportSize({ height, width });
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await expect(dialog.getByRole("group", { name: "Actions" })).toBeVisible();
    await expect.soft(dialog, `${width}x${height}`).toBeInViewport({ ratio: 1 });
    for (let step = 0; step < 6; step += 1) {
      const active = await input.getAttribute("aria-activedescendant");
      await expect(page.locator(`[id="${active}"]`), `${width}x${height}`).toBeInViewport({
        ratio: 1,
      });
      await input.press("ArrowDown");
    }
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
  }
});

test("a window that shrinks under an open palette keeps the highlighted row in view @pr", async ({
  page,
  scene,
}) => {
  await page.setViewportSize({ height: 900, width: 1440 });
  await openIssue(page, scene.shrinkingWindow, "Shrinking window");
  await page.keyboard.press("Control+k");
  const input = page.getByRole("combobox", { name: "Search" });
  await expect(
    page.getByRole("dialog", { name: "Search" }).getByRole("group", { name: "Actions" })
  ).toBeVisible();
  // Down to the eleventh row, near the foot of the list at this height.
  for (let step = 0; step < 10; step += 1) {
    await input.press("ArrowDown");
  }
  const active = page.locator(`[id="${await input.getAttribute("aria-activedescendant")}"]`);
  await expect(active).toBeInViewport({ ratio: 1 });

  // No key is pressed after the resize: the list itself brings the row back into view.
  await page.setViewportSize({ height: 420, width: 1440 });
  await expect(active).toBeInViewport({ ratio: 1 });
});
