import { setTimeout as sleep } from "node:timers/promises";

import { expect, test } from "@playwright/test";

import { openAgents, seedAgents, shownAgentRows } from "./agents";
import { createIssue, createProject, getIssue, patchIssue } from "./api";
import { openIssue } from "./keyboard-palette";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The command palette, in the rows whose claim is the server's: an action that writes and is
 * read back, a search the index answers, a write held in flight, a refusal, and the Agents page,
 * whose rows fold under `Inactive` once their `last_seen` is ten minutes old - which a frozen
 * recording's would be. What the palette draws over one fixed answer - its listing, its focus,
 * the addresses its rows open and the way the list fits a window - is in
 * `keyboard-palette.stub.e2e.ts`.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

test("the palette lists the issue page's actions, guarded like their buttons, and runs one", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Palette actions" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Palette actions");
    await page.keyboard.press("Control+k");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    await expect(actions).toBeVisible();
    // An empty query still lists the actions, above the message the hits would answer.
    await expect(page.getByText("Type at least 2 characters")).toBeVisible();

    const close = actions.getByRole("option", { name: "Close issue" });
    await expect(close).toBeVisible();
    // A palette-only action has no key, so it offers no hint; a keyed one shows its key.
    await expect(close.locator("kbd")).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Create issue" }).locator("kbd")).toHaveText([
      "c",
    ]);
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Reopen issue" })).toHaveCount(0);
    // An issue page names its project in its key, so `g d` has a row here.
    await expect(actions.getByRole("option", { name: "Go to Documents" })).toHaveCount(1);
    // `$mod+k` fires inside an input, as the palette's own close, so it is no row, and `/` would
    // only reopen this palette with its actions taken away. A binding that opens anything else is
    // a row: it runs once this palette has closed (the rows below).
    for (const absent of ["Search and actions", "Search only"]) {
      await expect(actions.getByRole("option", { exact: true, name: absent })).toHaveCount(0);
    }

    // Arrows move over action rows exactly as they move over hits, from the first row. Which row
    // that is belongs to `actions()`'s sort, so this names none.
    const rows = actions.getByRole("option");
    const firstId = await rows.first().getAttribute("id");
    expect(firstId).not.toBeNull();
    await expect(input).toHaveAttribute("aria-activedescendant", firstId as string);
    await input.press("ArrowDown");
    await expect(input).not.toHaveAttribute("aria-activedescendant", firstId as string);
    await input.press("ArrowUp");
    await expect(input).toHaveAttribute("aria-activedescendant", firstId as string);

    // Arrow down to Close issue, wherever the sort put it, and Enter runs the highlighted row.
    const closeId = await close.getAttribute("id");
    const ids = await rows.evaluateAll((options) => options.map((option) => option.id));
    const closeIndex = ids.indexOf(closeId ?? "");
    expect(closeIndex).toBeGreaterThanOrEqual(0);
    for (let step = 0; step < closeIndex; step += 1) {
      await input.press("ArrowDown");
    }
    await expect(input).toHaveAttribute("aria-activedescendant", closeId as string);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect.poll(() => getIssue(issue.key).then((read) => read.status)).toBe("done");

    // The guards follow the buttons: the closed issue offers Reopen and neither Close nor a
    // priority, exactly as its header renders Reopen and disables the priority select.
    await page.reload();
    await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await expect(actions.getByRole("option", { name: "Reopen issue" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Close issue" })).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toHaveCount(0);

    // Named, not positional: which row is highlighted first is `actions()`'s decision, and this
    // step is about Reopen running, not about where Reopen sits.
    await actions.getByRole("option", { name: "Reopen issue" }).click();
    await expect(dialog).toHaveCount(0);
    await expect.poll(() => getIssue(issue.key).then((read) => read.status)).toBe("backlog");

    // A filtered action runs the same write its control does. The rows are taken when the palette
    // opens, and the Set priority rows, like the priority select, are withheld until the page has
    // taken in the Reopen answer, which under load can come after the server already reads
    // `backlog`: so open once the select takes a pick again.
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toBeEnabled();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await input.fill("priority p2");
    const setP2 = actions.getByRole("option", { name: "Set priority P2" });
    await expect(actions.getByRole("option")).toHaveCount(1);
    // `priority p2` sits inside the label rather than starting it, so the row is offered but not
    // highlighted: Enter alone runs nothing, even once the search has answered with no hits.
    await expect(dialog.getByText('No results for "priority p2"')).toBeVisible();
    await expect(input).not.toHaveAttribute("aria-activedescendant");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    await input.press("ArrowDown");
    await expect(input).toHaveAttribute(
      "aria-activedescendant",
      (await setP2.getAttribute("id")) as string
    );
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toHaveValue("2");
    await expect.poll(() => getIssue(issue.key).then((read) => read.priority)).toBe(2);
  } finally {
    await context.close();
  }
});

test("stop-word searches show empty results without Retry", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stop word search" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Stop word search");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });

    for (const query of ["down", "up", "is", "the"]) {
      await page.keyboard.press("Control+k");
      await input.fill(query);
      await expect(dialog.getByText(`No results for "${query}"`)).toBeVisible();
      await expect(dialog.getByRole("alert")).toHaveCount(0);
      await expect(dialog.getByRole("button", { name: "Retry" })).toHaveCount(0);
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
    }
  } finally {
    await context.close();
  }
});

test("/ searches only, and a query that is no action's start lists the hits above the actions", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Astrolabe issue calibration" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Astrolabe issue calibration");

    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });

    await page.keyboard.press("/");
    await expect(dialog).toBeVisible();
    await expect(actions).toHaveCount(0);
    await input.fill("astrolabe");
    await expect(dialog.getByRole("option")).toHaveCount(1);
    await expect(actions).toHaveCount(0);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // `/` reopens on its last search, selected so that typing replaces it; the hits come back
    // with it. `⌘K` still opens empty, since a query left there would filter this page's actions.
    await page.keyboard.press("/");
    await expect(input).toHaveValue("astrolabe");
    await expect
      .poll(() =>
        input.evaluate((field: HTMLInputElement) => [field.selectionStart, field.selectionEnd])
      )
      .toEqual([0, "astrolabe".length]);
    await expect(dialog.getByRole("option")).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // The same query under `$mod+k` lists the hits first. No action here starts with `issue`, so
    // the ones that hold it further in (`Close issue` among them) sit below the hits, and Enter
    // opens the first hit rather than closing the issue the reader is on.
    const statusWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      ) {
        statusWrites.push(request.postData() ?? "");
      }
    });
    await page.keyboard.press("Control+k");
    await expect(input).toHaveValue("");
    await input.fill("issue");
    const options = dialog.getByRole("option");
    await expect(options.first()).toHaveAttribute("id", /^search-option-issue-/);
    await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
    await expect(options.last()).toHaveAttribute("id", /^search-option-action-/);
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect(
      page
        .getByTestId("issue-header")
        .getByRole("heading", { level: 1, name: "Astrolabe issue calibration" })
    ).toBeVisible();
    await expect(page.getByRole("button", { exact: true, name: "Close issue" })).toBeVisible();
    expect(statusWrites).toEqual([]);
    expect((await getIssue(issue.key)).status).not.toBe("done");
  } finally {
    await context.close();
  }
});

test("a query that holds a card move further in moves no card on Enter", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  // Two cards in one column, so `Move card down` from the first one has somewhere to go.
  const first = await createIssue({ project: "CORE", title: "Board query one" });
  const second = await createIssue({ project: "CORE", title: "Board query two" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const writes: string[] = [];
    page.on("request", (request) => {
      if (request.method() !== "GET" && new URL(request.url()).pathname.startsWith("/api/")) {
        writes.push(`${request.method()} ${new URL(request.url()).pathname}`);
      }
    });
    await page.setViewportSize({ height: 900, width: 1280 });
    await page.goto("/projects/CORE/issues");
    await page.getByRole("button", { name: "Board" }).click();
    await expect(page.locator(`[data-board-card="${first.key}"]`)).toBeVisible();
    await expect(page.locator(`[data-board-card="${second.key}"]`)).toBeVisible();
    // `j` from the page takes the top card, the one `Move card down` would move.
    const card = page.locator("[data-board-card]").first();
    await page.locator("body").focus();
    await page.keyboard.press("j");
    await expect(card).toBeFocused();

    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    // Each query sits inside a move's label without starting it: the move is offered, and only
    // an arrow can highlight it, so Enter, even once the search has answered, moves nothing.
    // `down` has no searchable term, so it returns no hits, and the move stays unhighlighted
    // through that answer too.
    for (const [query, move] of [
      ["status", "Move card to the next status"],
      ["down", "Move card down"],
    ] as const) {
      await page.keyboard.press("Control+k");
      await input.fill(query);
      await expect(actions.getByRole("option", { name: move })).toHaveCount(1);
      await expect(dialog.getByText("Searching…")).toHaveCount(0);
      await expect(input).not.toHaveAttribute("aria-activedescendant");
      await input.press("Enter");
      await expect(dialog).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
      await expect(card).toBeFocused();
    }

    // Nor can an arrow pressed while the search is still out: `next` leads no label, and Down
    // then would have taken the move to the next status before any answer came.
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await page.keyboard.press("Control+k");
    await input.fill("next");
    const nextMove = actions.getByRole("option", { name: "Move card to the next status" });
    await expect(nextMove).toHaveCount(1);
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    expect(writes).toEqual([]);
    await expect.poll(() => held).toBeGreaterThan(0);
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    await expect(input).not.toHaveAttribute("aria-activedescendant");
    release.resolve();
    await expect(dialog.getByText("Searching…")).toHaveCount(0);
    await expect(input).not.toHaveAttribute("aria-activedescendant");
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(card).toBeFocused();
    expect(writes).toEqual([]);
    const [after, other] = await Promise.all([getIssue(first.key), getIssue(second.key)]);
    expect(after.status).toBe(first.status);
    expect(other.status).toBe(second.status);
  } finally {
    await context.close();
  }
});

test("a header write in flight withdraws the rows whose buttons it disables", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "In-flight guard" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // Hold the PATCH so the header's write stays in flight for the whole assertion.
    const release = Promise.withResolvers<void>();
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      if (route.request().method() === "PATCH") {
        await release.promise;
      }
      await route.continue();
    });
    await openIssue(page, issue.key, "In-flight guard");

    const closeButton = page.getByRole("button", { name: "Close issue" });
    await closeButton.click();
    await expect(closeButton).toBeDisabled();
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toBeDisabled();

    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const actions = page.getByRole("dialog", { name: "Search" }).getByRole("group", {
      name: "Actions",
    });
    await expect(actions.getByRole("option", { name: "Go to Inbox" })).toBeVisible();
    await expect(actions.getByRole("option", { name: "Close issue" })).toHaveCount(0);
    await expect(actions.getByRole("option", { name: "Set priority P2" })).toHaveCount(0);
    release.resolve();
  } finally {
    await context.close();
  }
});

test("a Set priority row the server refuses is reported by the header, in the digit key's one failure", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refused palette priority" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, "Refused palette priority");
    let patches = 0;
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      if (route.request().method() !== "PATCH") {
        await route.continue();
        return;
      }
      patches += 1;
      // Dispatch's verdict: a 5xx would be tried again under the write's key, not refused.
      await route.fulfill({
        body: JSON.stringify({ code: "ISSUE_INPUT", error: "Dispatch refused the priority" }),
        contentType: "application/json",
        status: 400,
      });
    });

    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    await dialog
      .getByRole("group", { name: "Actions" })
      .getByRole("option", { name: "Set priority P2" })
      .click();
    await expect(dialog).toHaveCount(0);

    // The row writes through the page's one priority write, so the header reports its refusal
    // and the badge rolls back, exactly as for the picker and the digit keys.
    const header = page.getByTestId("issue-header");
    const failure = header
      .getByRole("alert")
      .filter({ hasText: `Could not update the priority of ${issue.key}.` });
    await expect(failure).toBeVisible();
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
    expect(patches).toBe(1);

    // A digit is the same write, so its refusal lands in that failure rather than beside it.
    await page.locator("body").focus();
    await page.keyboard.press("3");
    await expect.poll(() => patches).toBe(2);
    await expect(failure).toHaveCount(1);
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("the Agents page offers a row's picker, which then commits a keyboard pick on Enter", async ({
  browser,
}) => {
  await seedAgents();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openAgents(page);
    const row = shownAgentRows(page).nth(0);
    await page.keyboard.press("j");
    await expect(row).toBeFocused();

    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });
    for (const offered of [
      "Message the focused agent",
      "Pick an issue for the message",
      "Pin or unpin the focused agent",
    ]) {
      await expect(actions.getByRole("option", { name: offered })).toHaveCount(1);
    }
    for (const absent of ["Next agent", "Previous agent", "Select or deselect the focused agent"]) {
      await expect.soft(actions.getByRole("option", { name: absent })).toHaveCount(0);
    }

    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("pick an issue");
    await expect(actions.getByRole("option")).toHaveCount(1);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);

    // The row lands in the picker exactly as `i` does, and the picker's own rule holds: the
    // arrows only move the selection, and Enter commits it and hands the reader to the composer.
    const picker = row.getByRole("combobox", { name: "Issue" });
    const toggle = row.getByRole("button", { name: "Choose issue" });
    await expect(picker).toBeFocused();
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    await expect(picker).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(toggle).toContainText("CORE-2");
    const field = row.getByRole("textbox", { name: "Comment" });
    await expect(field).toBeFocused();
    await expect(field).toHaveValue("@Planner");
  } finally {
    await context.close();
  }
});

test("hits that arrive after the reader has arrowed keep the highlight on the row they chose", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Go held search" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // Hold the search the way a slow network would, so the reader arrows before the hits arrive.
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await openIssue(page, issue.key, "Go held search");
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    // `go to` starts every `Go to …` label, so those rows lead and can be arrowed among while
    // the search is out.
    const rows = dialog.getByRole("group", { exact: true, name: "Actions" }).getByRole("option");
    await input.fill("go to");
    const documents = rows.filter({ hasText: "Go to Documents" });
    await expect(documents).toHaveCount(1);
    const documentsId = await documents.getAttribute("id");
    const ids = await rows.evaluateAll((options) => options.map((option) => option.id));
    await expect(input).toHaveAttribute("aria-activedescendant", ids[0] ?? "");
    for (let step = 0; step < ids.indexOf(documentsId ?? ""); step += 1) {
      await input.press("ArrowDown");
    }
    await expect(input).toHaveAttribute("aria-activedescendant", documentsId as string);
    await expect.poll(() => held).toBeGreaterThan(0);

    // The hits arrive below the rows the query leads to, and the chosen row keeps the highlight.
    release.resolve();
    await expect(dialog.getByRole("option", { name: /Go held search/ })).toHaveCount(1);
    await expect(input).toHaveAttribute("aria-activedescendant", documentsId as string);
    await input.press("Enter");
    await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);
  } finally {
    await context.close();
  }
});

test("an arrow pressed before the search answers reaches no action the query finds mid-label", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Arrow before the issue search" });
  await patchIssue(issue.key, { status: "in_progress" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const statusWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      ) {
        statusWrites.push(request.postData() ?? "");
      }
    });
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await openIssue(page, issue.key, "Arrow before the issue search");
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    await input.fill("issue");
    await expect(dialog.getByRole("option", { name: "Close issue" })).toHaveCount(1);
    // Down and Enter right after typing, and again once the request is held: the list holds only
    // the actions `issue` finds mid-label, `Close issue` first, and the hits are still to arrive
    // above them, so neither burst highlights or runs anything.
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    expect(statusWrites).toEqual([]);
    await expect.poll(() => held).toBeGreaterThan(0);
    await input.press("ArrowDown");
    await input.press("Enter");
    await expect(dialog).toBeVisible();
    await expect(input).not.toHaveAttribute("aria-activedescendant");

    release.resolve();
    await expect(dialog.getByRole("option").first()).toHaveAttribute("id", /^search-option-issue-/);
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("button", { exact: true, name: "Close issue" })).toBeVisible();
    expect(statusWrites).toEqual([]);
    expect((await getIssue(issue.key)).status).toBe("in_progress");
  } finally {
    await context.close();
  }
});

test("a stale answer refreshing for the same query keeps the arrows off the actions it finds mid-label", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stale answer for an issue search" });
  await patchIssue(issue.key, { status: "in_progress" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    const statusWrites: string[] = [];
    page.on("request", (request) => {
      if (
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      ) {
        statusWrites.push(request.postData() ?? "");
      }
    });
    // The clock is the page's own, so the cached answer can go stale without a 30 s wait.
    await page.clock.install();
    await openIssue(page, issue.key, "Stale answer for an issue search");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const hit = dialog.getByRole("option").first();

    // The reader searched `issue` once: the answer is cached, with `Close issue` below the hit.
    await page.keyboard.press("Control+k");
    await input.fill("issue");
    await expect(hit).toHaveAttribute("id", /^search-option-issue-/);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // Past the answer's 30 s staleTime, the same query shows that answer and refetches it.
    await page.clock.fastForward("00:31");
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(
      (url) => url.pathname === "/api/v1/search",
      async (route) => {
        held += 1;
        await release.promise;
        await route.continue();
      }
    );
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    await input.fill("issue");
    await expect.poll(() => held).toBeGreaterThan(0);
    await expect(hit).toHaveAttribute("id", /^search-option-issue-/);
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    // The hits may still change above `Close issue`, so Down stays among them.
    await input.press("ArrowDown");
    await expect(input).toHaveAttribute("aria-activedescendant", /^search-option-issue-/);
    release.resolve();
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole("button", { exact: true, name: "Close issue" })).toBeVisible();
    expect(statusWrites).toEqual([]);
    expect((await getIssue(issue.key)).status).toBe("in_progress");
  } finally {
    await context.close();
  }
});

test("a row whose control has gone since the palette opened does nothing", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Stale row" });
  await patchIssue(issue.key, { status: "done" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    let patches = 0;
    page.on("request", (request) => {
      if (request.method() === "PATCH" && request.url().endsWith(`/api/v1/issues/${issue.key}`)) {
        patches += 1;
      }
    });
    await openIssue(page, issue.key, "Stale row");
    await expect(page.getByRole("button", { name: "Reopen issue" })).toBeVisible();
    await page.keyboard.press("Control+k");
    const dialog = page.getByRole("dialog", { name: "Search" });
    const reopen = dialog
      .getByRole("group", { name: "Actions" })
      .getByRole("option", { name: "Reopen issue" });
    await expect(reopen).toBeVisible();

    // Someone else reopens the issue while the palette is open; the header's Reopen button goes.
    // A CSS locator, since what is asserted is the header behind the modal palette.
    await patchIssue(issue.key, { status: "in_progress" }, { login: "bob" });
    await expect(
      page.locator('[data-testid="issue-header"] button[aria-label="Reopen issue"]')
    ).toHaveCount(0);

    // The row checks its control again when it runs, so it writes nothing. The palette closing
    // is the run's own signal - a PATCH would have left with the same task - and the read below
    // is a round trip past it; the margin covers the request's dispatch alone.
    await reopen.click();
    await expect(dialog).toHaveCount(0);
    await sleep(100);
    expect((await getIssue(issue.key)).status).toBe("in_progress");
    expect(patches).toBe(0);
  } finally {
    await context.close();
  }
});
