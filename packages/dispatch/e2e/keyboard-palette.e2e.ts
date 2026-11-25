import { setTimeout as sleep } from "node:timers/promises";
import { SEARCH_RESULT_KINDS } from "@legion/contracts";

import type { Result } from "../web/src/api/paths";
import type { SearchResponse } from "../web/src/api/types";
import { groupResults, optionId } from "../web/src/features/search/search-model";
import {
  agentSessions,
  expectPickerOffers,
  openAgents,
  seedAgents,
  seedPickerList,
  shownAgentRows,
} from "./agents";
import { createIssue, createProject, getIssue, listProjects, patchIssue } from "./api";
import { openIssue, searchPath } from "./keyboard-palette";
import { keyedSearchWord } from "./search";
import { resetDatabase } from "./seed";
import { expect, type Locator, type Page, rowKey, rowSessionId, test } from "./test";
import { asUser } from "./users";

/**
 * The command palette, in the rows whose claim is the server's: an action that writes and is
 * read back, a search the index answers, a write held in flight, a refusal, and the Agents page,
 * whose rows fold under `Inactive` once their `last_seen` is ten minutes old - which a frozen
 * recording's would be. What the palette draws over one fixed answer - its listing, its focus,
 * the addresses its rows open, the way the list fits a window, and where the highlight goes over
 * the hits one search answered - is in `keyboard-palette.stub.e2e.ts`. What a search answers is
 * the server's over every project of the stack, so a row here takes the answer the page itself
 * received (`pageAnswer`) and holds the bar to it (`expectAnswer`).
 */

/** The bar's hits: every option a search result draws, whose id is `optionId`'s, and none of the
 *  actions, projects or offers listed beside them. */
function hitOptions(dialog: Locator): Locator {
  return dialog.locator(
    SEARCH_RESULT_KINDS.map((kind) => `[role="option"][id^="search-option-${kind}-"]`).join(", ")
  );
}

/** The bar lists the server's own answer to `words` as its hits, and nothing else: each result by
 *  its option id, in the order the bar draws them - the server's ranked list under each owner
 *  (`groupResults`, the bar's own grouping, web/src/features/search/search-model.ts) - and
 *  `No results for "<words>"` where the answer holds none. Search reads every project of the
 *  stack, so which items match and how they rank is the server's over whatever the stack holds,
 *  other rows' items among them: a row takes the answer the page itself received
 *  (`pageAnswer`) and holds the page to it. Where the highlight goes over one answer, and what
 *  Enter then opens or runs, is the browser's, which `keyboard-palette.stub.e2e.ts` reads over
 *  the answer its row's own seeding makes. */
async function expectAnswer(dialog: Locator, words: string, answer: SearchResponse): Promise<void> {
  const drawn = groupResults(answer.results ?? []).flatMap((group) => group.results.map(optionId));
  await expect
    .poll(() => hitOptions(dialog).evaluateAll((options) => options.map((option) => option.id)))
    .toEqual(drawn);
  await expect(dialog.getByText(`No results for "${words}"`)).toHaveCount(
    drawn.length === 0 ? 1 : 0
  );
}

/** The answer the bar's list ends on for `words`: the whole search the page makes once the
 *  reader stops typing, by meaning as well as by words, which is the one request for them that
 *  carries no `keywords_only` (`useTieredSearch`, web/src/features/search/useTieredSearch.ts).
 *  Take it before the words are typed. A row holds the bar to this answer rather than to one it
 *  read itself: the server's ranking of the items other rows wrote can move between two reads
 *  with no write of the row's between them, and another row's issue, `LAYW2GX3-2`, stood seventh
 *  in an answer read at a row's start and first in the page's (nightly 45541644067). The page
 *  asks for the words and its own project, `numberProject`, which narrows nothing, and for no
 *  other member: a search the page narrowed by a filter the reader did not choose fails here,
 *  as an answer read apart from the page would have shown. */
async function pageAnswer(
  page: Page,
  words: string,
  numberProject: string
): Promise<SearchResponse> {
  const answered = await page.waitForResponse((response) => {
    const url = new URL(response.url());
    return (
      url.pathname === "/api/v1/search/results" &&
      url.searchParams.get("q") === words &&
      !url.searchParams.has("keywords_only")
    );
  });
  expect(
    Object.fromEntries(new URL(answered.url()).searchParams),
    `the members of the page's whole search for "${words}"`
  ).toEqual({ number_project: numberProject, q: words });
  return (await answered.json()) as Result<"/api/v1/search/results", "get">;
}

/** No project of the stack has a key or a name holding `words`. The bar lists every project whose
 *  key or name holds the words in its box as a row of its own, above the hits and above every
 *  action that holds them further in (`projectRows`, web/src/features/search/SearchPalette.tsx),
 *  and it reads them from every project of the stack, not the row's alone. A row whose claim is
 *  that the list holds those actions and its hits with no project above them empties the stack
 *  itself (`resetDatabase`) and reads this after its own seed: a project above them would take
 *  the highlight, and Enter would open it and close the bar. */
async function expectNoProjectHolds(words: string): Promise<void> {
  const holding = (await listProjects())
    .map((project) => `${project.key} ${project.name}`)
    .filter((project) => project.toLowerCase().includes(words.toLowerCase()));
  expect(holding, `the projects the bar lists above the actions for "${words}"`).toEqual([]);
}

test("the palette lists the issue page's actions, guarded like their buttons, and runs one", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Palette actions" });
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
    // `$mod+k` fires inside an input, as the bar's own close, so it is no row, and `/` would
    // only reopen the bar the reader is in. A binding that opens anything else is a row: it
    // runs once this bar has closed (the rows below).
    for (const absent of ["Search and actions", "Search"]) {
      await expect(actions.getByRole("option", { exact: true, name: absent })).toHaveCount(0);
    }

    // Arrows move over every row of the list, from its first: the recent items this reader has
    // (the issue they are on) come before the actions. Which action row is where belongs to
    // `actions()`'s sort, so this names none.
    const rows = dialog.getByRole("option");
    const firstId = await rows.first().getAttribute("id");
    expect(firstId).not.toBeNull();
    await expect(input).toHaveAttribute("aria-activedescendant", firstId as string);
    await input.press("ArrowDown");
    await expect(input).not.toHaveAttribute("aria-activedescendant", firstId as string);
    await input.press("ArrowUp");
    await expect(input).toHaveAttribute("aria-activedescendant", firstId as string);

    // Arrow down to Close issue, wherever the sort put it, and Enter runs the highlighted row.
    const closeId = await close.getAttribute("id");
    await expect(rows.first()).toBeVisible();
    await expect(close).toBeVisible();
    // Both rows the bound is taken between are drawn, and the render that drew them drew every
    // row: the bar reads its recent items and its actions once, on the render that opens it
    // (`SearchPalette.tsx:420-429`), and an empty box adds no project and no hit (`:497-524`).
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
    const priorityAnswer = pageAnswer(page, "priority p2", core);
    await input.fill("priority p2");
    const setP2 = actions.getByRole("option", { name: "Set priority P2" });
    await expect(actions.getByRole("option")).toHaveCount(1);
    // `priority p2` sits inside the label rather than starting it, so the row is offered among
    // the hits the server answered the page: none over an emptied database, and other rows'
    // items where the stack holds them, since nothing this row writes holds `priority`. That the
    // row is then left unhighlighted, so Enter alone runs nothing once an answer with no hits is
    // in, is the browser's reading of that answer, and `keyboard-palette.stub.e2e.ts` reads it
    // over this row's own seeding. The write the row runs is the server's, so the row is chosen
    // by name, as Reopen is above.
    await expectAnswer(dialog, "priority p2", await priorityAnswer);
    await setP2.click();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByLabel(`Priority of ${issue.key}`)).toHaveValue("2");
    await expect.poll(() => getIssue(issue.key).then((read) => read.priority)).toBe(2);
  } finally {
    await context.close();
  }
});

test("stop-word searches show empty results without Retry", async ({ browser }) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Stop word search" });
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

test("/ opens the same bar, and a query that is no action's start lists the hits above the actions", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  // The word searched for is the row's own (`keyedSearchWord`, e2e/search.ts): search reads
  // every project of the stack, and other rows' issues hold `astrolabe` too.
  const word = keyedSearchWord("astrolabe", core);
  const title = `${keyedSearchWord("Astrolabe", core)} issue calibration`;
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openIssue(page, issue.key, title);

    const dialog = page.getByRole("dialog", { name: "Search" });
    const input = page.getByRole("combobox", { name: "Search" });
    const actions = dialog.getByRole("group", { name: "Actions" });

    await page.keyboard.press("/");
    await expect(dialog).toBeVisible();
    // The bar `⌘K` opens: `/` is another key to it, not a list of its own, so this page's
    // actions are here too.
    await expect(actions.getByRole("option", { name: "Close issue" })).toHaveCount(1);
    await input.fill(word);
    // No action's label and no project's key or name holds the row's word, so the issue's own
    // hit is the whole list.
    await expect(dialog.getByRole("option")).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // Every open starts on an empty box, whichever key opened it: a query left in it would
    // filter the next page's actions, and its recent items, down to nothing.
    await page.keyboard.press("/");
    await expect(input).toHaveValue("");
    await expect(actions.getByRole("option", { name: "Close issue" })).toHaveCount(1);
    await page.keyboard.press("Escape");
    await expect(dialog).toHaveCount(0);

    // The same query under `$mod+k` lists the hits the server answered the page for `issue`
    // (`pageAnswer`): this row's own issue, and beside it every other item of the stack that
    // holds the word, ranked as the server ranks them, below any project whose key or name holds
    // it. No action here starts with `issue`, so the ones that hold it further in (`Close issue`
    // among them) sit below the hits, and Enter, which opens the row heading the list, closes
    // nothing. That the head is this row's own issue, highlighted, and that Enter opens it, is
    // the browser's reading of an answer that ranks that issue first and names no project, and
    // `keyboard-palette.stub.e2e.ts` reads it over this row's own seeding.
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
    const issueAnswer = pageAnswer(page, "issue", core);
    await input.fill("issue");
    await expectAnswer(dialog, "issue", await issueAnswer);
    await expect(actions.getByRole("option", { name: "Close issue" })).toBeVisible();
    await expect(dialog.getByRole("option").last()).toHaveAttribute("id", /^search-option-action-/);
    await input.press("Enter");
    await expect(dialog).toHaveCount(0);
    expect(statusWrites).toEqual([]);
    expect((await getIssue(issue.key)).status).not.toBe("done");
  } finally {
    await context.close();
  }
});

test("a query that holds a card move further in moves no card on Enter", async ({ browser }) => {
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  // Two cards in one column, so `Move card down` from the first one has somewhere to go.
  const first = await createIssue({ project: core, title: "Board query one" });
  const second = await createIssue({ project: core, title: "Board query two" });
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
    await page.goto(`/projects/${core}/issues`);
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
    // Each query sits inside a move's label without starting it: the move is offered, beside the
    // hits the server answered the page for the query (`pageAnswer`) - none over an emptied
    // database, and other rows' items where the stack holds them, since nothing this row writes
    // holds `status` or `next`. `down` has no searchable term, so the server answers no hits for
    // it. That only an arrow can highlight the move, so Enter after an answer with no hits moves
    // nothing, is the browser's reading of that answer, and `keyboard-palette.stub.e2e.ts` reads
    // it over this row's own two cards.
    for (const [query, move] of [
      ["status", "Move card to the next status"],
      ["down", "Move card down"],
    ] as const) {
      await page.keyboard.press("Control+k");
      const answer = pageAnswer(page, query, core);
      await input.fill(query);
      await expect(actions.getByRole("option", { name: move })).toHaveCount(1);
      await expect(dialog.getByText("Searching…")).toHaveCount(0);
      await expectAnswer(dialog, query, await answer);
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
      await expect(card).toBeFocused();
    }

    // And no arrow pressed while the search is still out reaches the move, whatever the server
    // answers once it is let through: `next` leads no label, and Down then would have taken the
    // move to the next status before any answer came. So Enter moves no card, and the server
    // holds both where they were.
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(searchPath, async (route) => {
      held += 1;
      await release.promise;
      await route.continue();
    });
    await page.keyboard.press("Control+k");
    const nextAnswer = pageAnswer(page, "next", core);
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
    await expectAnswer(dialog, "next", await nextAnswer);
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
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "In-flight guard" });
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
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Refused palette priority" });
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
  const core = rowKey("CORE");
  // The Planner and the Reviewer are sessions of the row's own (`rowSessionId`), and the issues
  // the picker's steps reach are the row's own, at the head of the list (`seedPickerList`).
  const issueKey = await seedAgents(core, agentSessions(rowSessionId));
  const list = await seedPickerList(core, [issueKey, `${core}-2`]);
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
    await expectPickerOffers(picker, list);
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("ArrowDown");
    await expect(picker).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(toggle).toContainText(`${core}-2`);
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
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Go held search" });
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    // Hold the search the way a slow network would, so the reader arrows before the hits arrive.
    const release = Promise.withResolvers<void>();
    let held = 0;
    await page.route(searchPath, async (route) => {
      held += 1;
      await release.promise;
      await route.continue();
    });
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
    // Every row left is one `go to` leads to, so the list drawn is the one the query filtered.
    await expect(rows.filter({ hasNotText: /^Go to / })).toHaveCount(0);
    await expect(rows.first()).toBeVisible();
    // The count above waited for this row, and a row's id is built from the action it runs
    // (`search-option-action-<scope>-<id>`, `SearchPalette.tsx`), so it reads the same at any
    // instant after.
    const documentsId = await documents.getAttribute("id");
    // The bar filters the actions it read on the render that opened it by the words in the box,
    // all in one render (`SearchPalette.tsx:429` and `:478-492`), so the render that drew the
    // rows waited for above drew every row `go to` leads to.
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
    await expect(page).toHaveURL(new RegExp(`/projects/${core}/documents$`));
  } finally {
    await context.close();
  }
});

test("an arrow pressed before the search answers reaches no action the query finds mid-label", async ({
  browser,
}) => {
  const core = rowKey("CORE");
  // The claim is the empty case: before the search answers, the list holds only the actions
  // `issue` finds mid-label, and so no highlight. The projects the bar lists above them are the
  // whole stack's whose key or name holds the word, and no key of this row's keeps another row's
  // out, since the word must be one inside a fixed action's label (`Close issue`). So the row
  // empties the stack itself. It empties through `resetDatabase`, which counts the database as
  // the worker's, so the datasets the worker keeps are written again for the rows after this one.
  await resetDatabase();
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Arrow before the issue search" });
  await patchIssue(issue.key, { status: "in_progress" });
  // The list `issue` draws before the search answers holds no project (`expectNoProjectHolds`).
  await expectNoProjectHolds("issue");
  // Signed in after the emptying, as e2e/issue.e2e.ts's row is: the emptying deletes every
  // session a sign-in wrote but the ones it writes again for alice and bob (e2e/users.ts).
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
    await page.route(searchPath, async (route) => {
      held += 1;
      await release.promise;
      await route.continue();
    });
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
  const core = rowKey("CORE");
  // The claim is the empty case: the list `issue` draws holds no project above its hit, so the
  // highlight starts on the hit, Down stays among the hits, and Enter opens one. The projects the
  // bar lists are the whole stack's whose key or name holds the word, and no key of this row's
  // keeps another row's out, since the word must be one inside a fixed action's label
  // (`Close issue`). So the row empties the stack itself, and over it the one hit is this row's
  // own issue, whose page Enter opens. It empties through `resetDatabase`, which counts the
  // database as the worker's, so the datasets the worker keeps are written again for the rows
  // after this one.
  await resetDatabase();
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Stale answer for an issue search" });
  await patchIssue(issue.key, { status: "in_progress" });
  // The list `issue` draws holds no project above its hit (`expectNoProjectHolds`).
  await expectNoProjectHolds("issue");
  // Signed in after the emptying, as e2e/issue.e2e.ts's row is: the emptying deletes every
  // session a sign-in wrote but the ones it writes again for alice and bob (e2e/users.ts).
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
    await page.route(searchPath, async (route) => {
      held += 1;
      await release.promise;
      await route.continue();
    });
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
  const core = rowKey("CORE");
  await createProject({ key: core, name: "Core" });
  const issue = await createIssue({ project: core, title: "Stale row" });
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
    // The margin the paragraph above describes: a PATCH the row might have written left in the
    // task that closed the palette, so this window covers its dispatch alone, and the read below
    // is the round trip that would have carried it.
    await sleep(100);
    expect((await getIssue(issue.key)).status).toBe("in_progress");
    expect(patches).toBe(0);
  } finally {
    await context.close();
  }
});
