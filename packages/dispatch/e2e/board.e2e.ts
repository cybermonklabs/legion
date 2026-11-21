import { setTimeout as sleep } from "node:timers/promises";

import { createIssue, createProject, getIssue, listIssues, patchIssue, putIssueState } from "./api";
import { pickFilterOption } from "./filters";
import { nextFrames } from "./frames";
import { pickFromMenu, priorityTrigger } from "./menus";
import {
  expectReadWhileHeld,
  expectShows,
  holdIssuePatches,
  lastEventSeq,
  listRead,
  logReads,
  PICK_HOLD_MS,
  pagePicker,
  pickIn,
  readAnsweredSince,
  savesEnded,
  storedValues,
  watchShown,
} from "./pick-writes";
import { navigateInApp } from "./sends";
import { expect, type Locator, type Page, test } from "./test";
import { centerOf, pressFinger, touchDrag, touchHold } from "./touch";
import { asUser } from "./users";

/** The visible priority tag of a card or row: the text its priority menu's trigger shows. */
function priorityBadge(scope: Locator, label: string): Locator {
  return scope
    .getByRole("button", { name: /^Priority of / })
    .filter({ hasText: new RegExp(`^${label}$`) });
}

function patchOf(page: Page, key: string) {
  return page.waitForResponse(
    (response) =>
      response.request().method() === "PATCH" &&
      new URL(response.url()).pathname === `/api/v1/issues/${key}`
  );
}

/**
 * Waits until no card is mid-flight. dnd-kit lands every reorder it did not animate itself - the
 * rollback and refetch after a rejected rank included - with a 200 ms transform transition, and a
 * card later in the DOM paints over the ones it crosses, so a press during that flight lifts
 * whichever card is passing the point rather than the one measured there.
 */
async function boardAtRest(page: Page): Promise<void> {
  await page.getByRole("region", { name: "Project board" }).evaluate(async (board) => {
    for (;;) {
      const running = board.getAnimations({ subtree: true });
      if (running.length === 0) {
        return;
      }
      await Promise.allSettled(running.map((animation) => animation.finished));
    }
  });
}

/**
 * Waits until a click reaches its target again. dnd-kit's pointer sensors stop every click at the
 * document (capture phase) from the lift until a 50 ms timer after the drop, and a busy main
 * thread runs that timer late, so a control clicked straight after a drop is silently ignored.
 * The probe is a node under `body` outside the React root: only that suppressor can stop it.
 */
async function clicksDelivered(page: Page): Promise<void> {
  await page.evaluate(async () => {
    const probe = document.body.appendChild(document.createElement("div"));
    let delivered = false;
    probe.addEventListener("click", () => {
      delivered = true;
    });
    try {
      for (;;) {
        delivered = false;
        probe.dispatchEvent(new MouseEvent("click"));
        if (delivered) {
          return;
        }
        const turn = Promise.withResolvers<void>();
        setTimeout(turn.resolve, 0);
        await turn.promise;
      }
    } finally {
      probe.remove();
    }
  });
}

/** Drags with the mouse from the middle of `source` to `target` (a card or a column). */
async function mouseDrag(page: Page, source: Locator, target: Locator, yOffset?: number) {
  await boardAtRest(page);
  // `hover` presses only once the card holds still and is the element under the pointer; a bare
  // `mouse.move` to a measured box would press on whatever is painted there.
  await source.hover();
  // The hover above waited for the card to be visible and still, so its box is there.
  const sourceBox = await source.boundingBox();
  if (sourceBox === null) {
    throw new Error("drag source is not visible");
  }
  await page.mouse.down();
  await page.mouse.move(sourceBox.x + sourceBox.width / 2 + 10, sourceBox.y + sourceBox.height / 2);
  await target.scrollIntoViewIfNeeded();
  // The scroll above waited for the target to be visible and still, so its box is there.
  const targetBox = await target.boundingBox();
  if (targetBox === null) {
    throw new Error("drag target is not visible");
  }
  await page.mouse.move(
    targetBox.x + targetBox.width / 2,
    yOffset === undefined ? targetBox.y + targetBox.height / 2 : targetBox.y + yOffset,
    { steps: 24 }
  );
  await page.mouse.up();
  await clicksDelivered(page);
}

// SLOW(>=2s): four whole-card drags, each paying dnd-kit's own 200 ms landing, plus the board's
// two fresh renders.
// @pr: Create through answer to close: the board is where a person moves an issue's status, and the drag has to order List and Board alike.
test("the board is the kanban: whole-card drag orders List and Board alike, Icebox and Done collapse behind a toggle @pr", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const first = await createIssue({ project: "CORE", title: "First card" });
  const second = await createIssue({ project: "CORE", title: "Second card" });
  const third = await createIssue({ project: "CORE", title: "Third card" });
  // Creation order is the rank order the row asserts; none of these three writes reads it.
  await Promise.all([
    patchIssue(first.key, { status: "todo" }),
    patchIssue(second.key, { priority: 0 }),
    patchIssue(third.key, { priority: 3 }),
  ]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ width: 1920, height: 900 });
  try {
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const board = page.getByRole("region", { name: "Project board" });
    const triage = page.getByRole("region", { name: "Triage" });
    await expect(triage.getByRole("article")).toHaveText([/Second card/, /Third card/]);

    // The card itself is the handle: no Reorder button, no status pill on the card.
    await expect(board.getByRole("button", { name: /Reorder/ })).toHaveCount(0);
    const thirdCard = triage.getByRole("article", { name: `${third.key} Third card` });
    await expect(thirdCard).not.toContainText("Triage");
    // Icebox and Done are collapsed rails; the other seven are full columns.
    await expect(board.getByTestId("board-column-header")).toHaveCount(7);
    await expect(board.getByTestId("board-column-rail")).toHaveCount(2);
    await expect(page.getByRole("region", { name: "Icebox (collapsed)" })).toBeVisible();
    const doneRail = page.getByRole("region", { name: "Done (collapsed)" });
    await expect(doneRail).toContainText("Done");

    // K1: the P3 card dragged above the P0 card stays first everywhere - rank is the only order.
    const secondCard = triage.getByRole("article", { name: `${second.key} Second card` });
    const reorderPatch = patchOf(page, third.key);
    await mouseDrag(page, thirdCard, secondCard, 4);
    expect((await reorderPatch).status()).toBe(200);
    await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);
    await expect(priorityBadge(thirdCard, "P3")).toBeVisible();
    await expect(priorityBadge(secondCard, "P0")).toBeVisible();

    await page.reload();
    await expect(triage.getByRole("article")).toHaveText([/Third card/, /Second card/]);
    await page.getByRole("button", { name: "List" }).click();
    await page.getByRole("button", { name: "Triage 2", exact: true }).click();
    await expect(page.locator("[data-issue-row]")).toHaveText([/Third card/, /Second card/]);
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      third.key,
      second.key,
      first.key,
    ]);

    // K2: a whole-card drag into another column changes the status.
    await page.getByRole("button", { name: "Board" }).click();
    const todoColumn = page.getByRole("region", { name: "Todo" });
    const inProgressColumn = page.getByRole("region", { name: "In progress" });
    const movePatch = patchOf(page, first.key);
    await mouseDrag(
      page,
      todoColumn.getByRole("article", { name: `${first.key} First card` }),
      inProgressColumn
    );
    expect((await movePatch).status()).toBe(200);
    await expect(todoColumn).not.toContainText("First card");
    await expect(inProgressColumn).toContainText("First card");
    await expect.poll(() => getIssue(first.key)).toMatchObject({ status: "in_progress" });

    // K3: a Retro card dropped on the collapsed Done rail closes it.
    await patchIssue(third.key, { status: "retro" });
    const retroColumn = page.getByRole("region", { name: "Retro" });
    const retroCard = retroColumn.getByRole("article", { name: `${third.key} Third card` });
    await expect(retroCard).toBeVisible();
    const closePatch = patchOf(page, third.key);
    await mouseDrag(page, retroCard, doneRail);
    const closeResponse = await closePatch;
    expect(closeResponse.status()).toBe(200);
    expect(closeResponse.request().postDataJSON()).toEqual({ status: "done", rank: {} });
    await expect(retroColumn).not.toContainText("Third card");
    await expect(doneRail).toContainText("1");
    await expect.poll(() => getIssue(third.key)).toMatchObject({ status: "done" });
    expect((await getIssue(third.key)).closed_at).not.toBeNull();

    // The toggle expands both edges, persists per login across a reload...
    const showEdges = page.getByRole("button", { name: "Show Icebox & Done" });
    await expect(showEdges).toHaveAttribute("aria-pressed", "false");
    await showEdges.click();
    const hideEdges = page.getByRole("button", { name: "Hide Icebox & Done" });
    await expect(hideEdges).toHaveAttribute("aria-pressed", "true");
    await expect(board.getByTestId("board-column-header")).toHaveCount(9);
    await expect(board.getByTestId("board-column-rail")).toHaveCount(0);
    const doneColumn = page.getByRole("region", { name: "Done" });
    await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);
    await page.reload();
    await expect(hideEdges).toHaveAttribute("aria-pressed", "true");
    await expect(doneColumn.getByRole("article")).toHaveText(/Third card/);

    // ...and dragging a Done card out reopens it.
    const backlogColumn = page.getByRole("region", { name: "Backlog" });
    const reopenPatch = patchOf(page, third.key);
    await mouseDrag(
      page,
      doneColumn.getByRole("article", { name: `${third.key} Third card` }),
      backlogColumn
    );
    expect((await reopenPatch).status()).toBe(200);
    await expect(backlogColumn.getByRole("article")).toHaveText(/Third card/);
    await expect.poll(() => getIssue(third.key)).toMatchObject({ closed_at: null });

    await hideEdges.click();
    await expect(board.getByTestId("board-column-rail")).toHaveCount(2);
    await expect(page.getByRole("region", { name: "Done (collapsed)" })).toContainText("0");
  } finally {
    await context.close();
  }
});

test("v toggles List and Board; Shift+J/K rank a focused card through the one move path; o, Enter, p and Escape work the card; Space lifts nothing", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const cards = [];
  for (const title of ["Alpha", "Bravo", "Charlie"]) {
    cards.push(await createIssue({ project: "CORE", title }));
  }
  const [alpha, bravo, charlie] = cards;
  if (alpha === undefined || bravo === undefined || charlie === undefined) {
    throw new Error("seed produced fewer cards than expected");
  }
  // Creation order is the rank order the row reads; none of these writes reads it.
  await Promise.all(cards.map((issue) => patchIssue(issue.key, { status: "todo" })));

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.setViewportSize({ width: 1920, height: 900 });
    const patches: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") {
        patches.push(request.url());
      }
    });
    const viewPreference = () =>
      page.evaluate(() => window.localStorage.getItem("dispatch.project.issue-view:alice"));

    // B5: `v` flips List and Board from the keyboard and persists the choice; on the Documents
    // tab there is no view to flip and it is inert.
    await page.goto("/projects/CORE");
    const listButton = page.getByRole("button", { name: "List" });
    const boardButton = page.getByRole("button", { name: "Board" });
    await expect(listButton).toHaveAttribute("aria-pressed", "true");
    await page.locator("body").focus();
    await page.keyboard.press("v");
    await expect(boardButton).toHaveAttribute("aria-pressed", "true");
    expect(await viewPreference()).toBe("board");
    await page.keyboard.press("v");
    await expect(listButton).toHaveAttribute("aria-pressed", "true");
    expect(await viewPreference()).toBe("list");
    await page.getByRole("tab", { name: "Documents" }).click();
    await expect(page).toHaveURL(/\/projects\/CORE\/documents$/);
    await page.locator("body").focus();
    await page.keyboard.press("v");
    await nextFrames(page);
    expect(await viewPreference()).toBe("list");
    await expect(page.getByRole("button", { name: "Board" })).toHaveCount(0);
    await page.getByRole("tab", { name: "Issues" }).click();
    await page.locator("body").focus();
    await page.keyboard.press("v");
    await expect(boardButton).toHaveAttribute("aria-pressed", "true");

    const todo = page.getByRole("region", { name: "Todo" });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    const alphaCard = todo.getByRole("article", { name: `${alpha.key} Alpha` });
    const charlieCard = todo.getByRole("article", { name: `${charlie.key} Charlie` });
    // dnd-kit renders its own unnamed `role=status` region; the board's is the named one.
    const live = page.getByRole("status", { name: "Board announcements" });

    // B2: Shift+J sends the same PATCH a drop would, names the visible neighbours, and the
    // card keeps focus in its new place while the live region says where it went.
    await alphaCard.focus();
    await expect(alphaCard).toBeFocused();
    const movePatch = patchOf(page, alpha.key);
    await page.keyboard.press("Shift+J");
    const moveResponse = await movePatch;
    expect(moveResponse.status()).toBe(200);
    expect(moveResponse.request().postDataJSON()).toEqual({
      rank: { after: bravo.key, before: charlie.key },
    });
    await expect(todo.getByRole("article")).toHaveText([/Bravo/, /Alpha/, /Charlie/]);
    await expect(alphaCard).toBeFocused();
    await expect(live).toHaveText(`${alpha.key} → Todo, position 2 of 3`);
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      bravo.key,
      alpha.key,
      charlie.key,
    ]);

    const backPatch = patchOf(page, alpha.key);
    await page.keyboard.press("Shift+K");
    expect((await backPatch).request().postDataJSON()).toEqual({ rank: { before: bravo.key } });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);
    await expect(alphaCard).toBeFocused();
    await expect(live).toHaveText(`${alpha.key} → Todo, position 1 of 3`);

    // At the column's ends there is nothing to do: no request, no announcement change. A move
    // that does rank is the witness below: requests reach the counter in order, so its PATCH
    // being the first proves the two presses above sent none.
    patches.length = 0;
    await page.keyboard.press("Shift+K");
    await charlieCard.focus();
    await page.keyboard.press("Shift+J");
    await expect(charlieCard).toBeFocused();
    await expect(live).toHaveText(`${alpha.key} → Todo, position 1 of 3`);
    const witness = patchOf(page, charlie.key);
    await page.keyboard.press("Shift+K");
    expect(patches).toEqual([(await witness).url()]);

    // B4: `o` opens the focused card's issue...
    await alphaCard.focus();
    await page.keyboard.press("o");
    await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));
    await page.goBack();
    await expect(page).toHaveURL(/\/projects\/CORE\/issues$/);
    await expect(alphaCard).toBeVisible();

    // ...and so does Enter on the card itself - once: the title link's own Enter is left to
    // the browser, so the card's binding never doubles a navigation.
    let navigations = 0;
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) {
        navigations += 1;
      }
    });
    await alphaCard.focus();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));
    await page.goBack();
    await expect(page).toHaveURL(/\/projects\/CORE\/issues$/);
    // Going back is the second navigation; a doubled Enter would have made one in between.
    expect(navigations).toBe(2);

    // `p` opens the focused card's priority menu on its checked choice; a letter pressed there
    // is no page key; Escape closes the menu back onto the card, then leaves the board.
    await alphaCard.focus();
    await page.keyboard.press("p");
    const menu = page.getByRole("menu", { name: `Priority of ${alpha.key}` });
    const unset = menu.getByRole("menuitemradio", { exact: true, name: "Unset" });
    await expect(unset).toBeFocused();
    await expect(unset).toHaveAttribute("aria-checked", "true");
    await page.keyboard.press("j");
    await expect(unset).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(menu).toHaveCount(0);
    await expect(alphaCard).toBeFocused();
    await page.keyboard.press("Escape");
    await expect
      .poll(() => page.evaluate(() => document.activeElement === document.body))
      .toBe(true);

    // B5: dnd-kit's keyboard grammar is gone - Space on a focused card lifts nothing.
    patches.length = 0;
    await alphaCard.focus();
    await page.keyboard.press("Space");
    await nextFrames(page);
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Space");
    await nextFrames(page);
    await expect(page.locator("article.opacity-50")).toHaveCount(0);
    // Again a real move is the witness: its PATCH is the first either Space could precede.
    await alphaCard.focus();
    const ranked = patchOf(page, alpha.key);
    await page.keyboard.press("Shift+J");
    expect(patches).toEqual([(await ranked).url()]);
    expect(new URL(page.url()).pathname).toBe("/projects/CORE/issues");
  } finally {
    await context.close();
  }
});

test("a stale board refreshes and says so when the server rejects the rank", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const cards = [];
  for (const title of ["Alpha", "Bravo", "Charlie"]) {
    cards.push(await createIssue({ project: "CORE", title }));
  }
  const [alpha, bravo, charlie] = cards;
  if (alpha === undefined || bravo === undefined || charlie === undefined) {
    throw new Error("seed produced fewer cards than expected");
  }
  // Creation order is the rank order the row reads; none of these writes reads it.
  await Promise.all(cards.map((issue) => patchIssue(issue.key, { status: "todo" })));

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    // Without the event subscription the board does not learn about the API move below.
    await page.routeWebSocket(/\/ws\/live$/u, (route) =>
      route.close({ code: 1011, reason: "test network drop" })
    );
    await page.setViewportSize({ width: 1920, height: 900 });
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const todo = page.getByRole("region", { name: "Todo" });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);

    await patchIssue(alpha.key, { rank: { after: charlie.key } });
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      bravo.key,
      charlie.key,
      alpha.key,
    ]);
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Bravo/, /Charlie/]);

    // Charlie between Alpha and Bravo names neighbours the server now has in the other order.
    const stalePatch = patchOf(page, charlie.key);
    await mouseDrag(
      page,
      todo.getByRole("article", { name: `${charlie.key} Charlie` }),
      todo.getByRole("article", { name: `${bravo.key} Bravo` }),
      4
    );
    const staleResponse = await stalePatch;
    expect(staleResponse.status()).toBe(400);
    expect(staleResponse.request().postDataJSON()).toEqual({
      rank: { after: alpha.key, before: bravo.key },
    });
    await expect(page.getByRole("alert")).toHaveText(
      "The board changed while you were moving this card - refreshed, try again."
    );
    await expect(todo.getByRole("article")).toHaveText([/Bravo/, /Charlie/, /Alpha/]);
    expect((await listIssues("CORE")).map((issue) => issue.key)).toEqual([
      bravo.key,
      charlie.key,
      alpha.key,
    ]);

    // Trying again from the refreshed board succeeds and clears the alert.
    const retryPatch = patchOf(page, charlie.key);
    await mouseDrag(
      page,
      todo.getByRole("article", { name: `${charlie.key} Charlie` }),
      todo.getByRole("article", { name: `${bravo.key} Bravo` }),
      4
    );
    expect((await retryPatch).status()).toBe(200);
    await expect(todo.getByRole("article")).toHaveText([/Charlie/, /Bravo/, /Alpha/]);
    await expect(page.getByRole("alert")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("board cards carry the unread dot the project list shows", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  const unread = await createIssue({ project: "CORE", title: "Unread card" });
  const read = await createIssue({ project: "CORE", title: "Read card" });
  await putIssueState(read.key, { last_read_seq: (await getIssue(read.key)).last_seq });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/projects/CORE");
    const unreadRow = page.getByRole("row", { name: `${unread.key} Unread card`, exact: true });
    await expect(unreadRow.locator('[title="Unread"]')).toHaveCount(1);
    await page.getByRole("button", { name: "Board" }).click();
    const unreadCard = page.getByRole("article", { name: `${unread.key} Unread card` });
    const readCard = page.getByRole("article", { name: `${read.key} Read card` });
    await expect(unreadCard.locator('[title="Unread"]')).toHaveCount(1);
    await expect(readCard).toBeVisible();
    await expect(readCard.locator('[title="Unread"]')).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("project list rows and board cards set an issue's priority in place", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Prioritized card" });
  await patchIssue(issue.key, { priority: 1 });
  const unset = await createIssue({ project: "CORE", title: "Unprioritized card" });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE");
    const listRow = page.getByRole("row", { name: `${issue.key} Prioritized card`, exact: true });
    const listControl = priorityTrigger(listRow, issue.key);
    await expect(priorityBadge(listRow, "P1")).toBeVisible();
    // The badge stays small; the tap target around it is 32 px from md.
    const listBox = await listControl.boundingBox();
    expect(listBox?.height ?? 0).toBeGreaterThanOrEqual(32);
    const listPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await pickFromMenu(listControl, "P3");
    expect((await listPatch).postDataJSON()).toEqual({ priority: 3 });
    await expect(priorityBadge(listRow, "P3")).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 3 });

    await page.getByRole("button", { name: "Board" }).click();
    const card = page.getByRole("article", { name: `${issue.key} Prioritized card` });
    await expect(priorityBadge(card, "P3")).toBeVisible();
    const cardControl = priorityTrigger(card, issue.key);
    const cardPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
    );
    await pickFromMenu(cardControl, "P2");
    expect((await cardPatch).postDataJSON()).toEqual({ priority: 2 });
    await expect(priorityBadge(card, "P2")).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });
    // Using the control neither followed the card's link nor dragged the card.
    expect(new URL(page.url()).pathname).toBe("/projects/CORE/issues");
    await expect(card).not.toHaveClass(/opacity-50/);
    await expect(page.getByRole("region", { name: "Triage" }).getByRole("article")).toHaveText([
      /Prioritized card/,
      /Unprioritized card/,
    ]);

    const unsetCard = page.getByRole("article", { name: `${unset.key} Unprioritized card` });
    await expect(priorityBadge(unsetCard, "Priority")).toBeVisible();
    const unsetPatch = page.waitForRequest(
      (request) =>
        request.method() === "PATCH" &&
        new URL(request.url()).pathname === `/api/v1/issues/${unset.key}`
    );
    await pickFromMenu(priorityTrigger(unsetCard, unset.key), "P0");
    expect((await unsetPatch).postDataJSON()).toEqual({ priority: 0 });
    await expect(priorityBadge(unsetCard, "P0")).toBeVisible();
    await expect.poll(() => getIssue(unset.key)).toMatchObject({ priority: 0 });

    // Tab order inside a card: the card itself (the drag activator), its title link, then the
    // priority control, so a keyboard shortcut can reach it too; the screenshot shows the
    // focus ring on the badge.
    await unsetCard.focus();
    await page.keyboard.press("Tab");
    await expect(unsetCard.getByRole("link")).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(priorityTrigger(unsetCard, unset.key)).toBeFocused();
    const shot = testInfo.outputPath(`board-priority-${testInfo.project.name}.png`);
    await page.screenshot({ path: shot });
    await testInfo.attach(`board priority (${testInfo.project.name})`, {
      contentType: "image/png",
      path: shot,
    });

    await page.reload();
    await expect(priorityBadge(card, "P2")).toBeVisible();
    await expect(priorityBadge(unsetCard, "P0")).toBeVisible();
  } finally {
    await context.close();
  }
});

test("board scrolls horizontally inside its own container at 1100px", async ({ browser }) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Board scrolls without widening the page" });
  const context = await asUser(browser, "alice");
  const page = await context.newPage();

  try {
    await page.setViewportSize({ width: 1100, height: 800 });
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const boardScroller = page.getByTestId("board-scroll-container");
    await expect(boardScroller).toBeVisible();
    const dimensions = await boardScroller.evaluate((element) => ({
      clientWidth: element.clientWidth,
      pageWidth: document.documentElement.scrollWidth,
      scrollWidth: element.scrollWidth,
      viewportWidth: window.innerWidth,
    }));
    expect(dimensions.scrollWidth).toBeGreaterThan(dimensions.clientWidth);
    expect(dimensions.pageWidth).toBeLessThanOrEqual(dimensions.viewportWidth);
  } finally {
    await context.close();
  }
});

/** Where the scroller sits against each column's or rail's start edge, in scroll-content px. */
async function snapOffsets(scroller: Locator): Promise<{ lefts: number[]; scrollLeft: number }> {
  return scroller.evaluate((element) => {
    const origin = element.getBoundingClientRect().left - element.scrollLeft;
    return {
      lefts: [...element.querySelectorAll("section")].map(
        (section) => section.getBoundingClientRect().left - origin
      ),
      scrollLeft: element.scrollLeft,
    };
  });
}

/**
 * Arms the scroller's next `scrollend` - fired once per scroll sequence, when the gesture, its
 * fling and the snap the fling ends in have all come to rest - and returns the wait for it. Arm
 * before the gesture so the listener cannot miss a scroll that already ended. `scrollLeft` alone
 * cannot tell rest from flight: a fling to Backlog passes the Icebox rail's edge on the way (a
 * snap point too), and a `scrollLeft` set while that fling still runs is overwritten by it.
 */
async function armScrollEnd(scroller: Locator): Promise<() => Promise<void>> {
  const armed = await scroller.evaluateHandle((element) => {
    const ended = Promise.withResolvers<void>();
    element.addEventListener("scrollend", () => ended.resolve(), { once: true });
    return { ended: ended.promise };
  });
  return () => armed.evaluate((scroll) => scroll.ended);
}

/**
 * The same wait for the page's own scrolling, which a vertical swipe sets off. `window.scrollTo`
 * does not stand in for it and does not end it: the page jumps where it is told and the fling
 * scrolls on from there, so the viewport settles hundreds of milliseconds later, somewhere else.
 * `window.scrollY` alone cannot tell rest from flight either. A box measured before this resolves
 * names a place the element has already left by the time the finger lands on it. The listener is
 * on the document, which is where the viewport's own `scrollend` is fired; a scroller's is fired
 * at the scroller and does not bubble, so only the page's scrolling resolves this.
 */
async function armPageScrollEnd(page: Page): Promise<() => Promise<void>> {
  const armed = await page.evaluateHandle(() => {
    const ended = Promise.withResolvers<void>();
    document.addEventListener("scrollend", () => ended.resolve(), { once: true });
    return { ended: ended.promise };
  });
  return () => armed.evaluate((scroll) => scroll.ended);
}

/**
 * Arms the card's next lift and returns the wait for it. dnd-kit draws a lifted card at
 * `opacity-50` from the render it commits for the sensor's activation until the release, so a
 * press held just past that activation shows the lift for tens of milliseconds - a window a poll
 * from Node steps over whole, and then reports a press that lifted nothing. The record is the
 * page's own, so arming before the press is what makes the wait sound: a lift already over by
 * the time it runs still resolves it.
 */
async function armLift(card: Locator): Promise<() => Promise<void>> {
  const armed = await card.evaluateHandle((element) => {
    const seen = Promise.withResolvers<void>();
    const check = () => {
      if (element.classList.contains("opacity-50")) {
        observer.disconnect();
        seen.resolve();
      }
    };
    const observer = new MutationObserver(check);
    observer.observe(element, { attributeFilter: ["class"], attributes: true });
    check();
    return { seen: seen.promise };
  });
  return () => armed.evaluate((lift) => lift.seen);
}

/**
 * Waits for the column's start edge to come within `within` of the viewport's start under a
 * carried card, and answers where it got to. dnd-kit auto-scrolls the board a step a frame while
 * the finger rests in an edge zone, so the watch is the page's own: a sample taken from Node
 * answers a round trip late, by which time the board has run on a column's width or more. The
 * watch also ends when the board stops scrolling, which is the column coming no closer, and the
 * caller is left to say that the edge never arrived.
 */
async function columnArrived(column: Locator, within: number): Promise<number> {
  return column.evaluate(async (element, within) => {
    const scroller = document.querySelector('[data-testid="board-scroll-container"]');
    if (scroller === null) {
      throw new Error("the board scroller is not in the page");
    }
    for (;;) {
      const left = element.getBoundingClientRect().left;
      if (left <= within) {
        return left;
      }
      const was = scroller.scrollLeft;
      const frame = Promise.withResolvers<void>();
      requestAnimationFrame(() => requestAnimationFrame(() => frame.resolve()));
      await frame.promise;
      if (scroller.scrollLeft === was) {
        return element.getBoundingClientRect().left;
      }
    }
  }, within);
}

/** Waits until the scroller has held one frame still. dnd-kit scrolls it with a step a frame
 *  while a carried card is in an edge zone and stops a frame or two after the finger leaves, and
 *  a card in flight takes the snapping off the scroller (`IssueBoard.tsx`), so there is no fling
 *  and no snap behind that scrolling: one unchanged frame is rest. */
async function scrollerAtRest(scroller: Locator): Promise<void> {
  await scroller.evaluate(async (element) => {
    for (;;) {
      const was = element.scrollLeft;
      const frame = Promise.withResolvers<void>();
      requestAnimationFrame(() => requestAnimationFrame(() => frame.resolve()));
      await frame.promise;
      if (element.scrollLeft === was) {
        return;
      }
    }
  });
}

/** Puts the `index`-th column's start edge at the scroller's start and waits for the scroll to end. */
async function scrollToColumn(scroller: Locator, index: number): Promise<void> {
  await scrollerAtRest(scroller);
  // The scroller came to rest above, so the offsets read here are the ones it holds still at.
  const target = await scroller.evaluate((element, index) => {
    const section = element.querySelectorAll("section")[index];
    if (section === undefined) {
      throw new Error("no such column");
    }
    const left =
      section.getBoundingClientRect().left -
      (element.getBoundingClientRect().left - element.scrollLeft);
    return element.scrollLeft === left ? undefined : left;
  }, index);
  if (target === undefined) {
    return;
  }
  const ended = await armScrollEnd(scroller);
  await scroller.evaluate((element, target) => {
    element.scrollLeft = target;
  }, target);
  await ended();
}

// SLOW(>=2s): a real finger: per-frame touch steps, two snap-scroll flings, the sensor's own
// 150 ms activation twice over, an issue opened and left, and a carried card.
test("on the phone a finger drives the board: hold lifts, tap opens, swipes scroll and snap @phone @phone-only", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const cards = [];
  for (const title of ["Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf"]) {
    cards.push(await createIssue({ project: "CORE", title }));
  }
  const [alpha, bravo] = cards;
  if (alpha === undefined || bravo === undefined) {
    throw new Error("seed produced fewer cards than expected");
  }
  // Creation order is the rank order the board draws; none of these writes reads it.
  await Promise.all(cards.map((issue) => patchIssue(issue.key, { status: "todo" })));

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  const patches: string[] = [];
  page.on("request", (request) => {
    if (request.method() === "PATCH") {
      patches.push(new URL(request.url()).pathname);
    }
  });
  try {
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();
    const scroller = page.getByTestId("board-scroll-container");
    const todo = page.getByRole("region", { name: "Todo" });
    await expect(todo.getByRole("article")).toHaveCount(7);

    // K5: a horizontal swipe lands on a column or rail edge (snap-x snap-mandatory).
    const start = await snapOffsets(scroller);
    expect(start.scrollLeft).toBe(0);
    await expect(scroller).toBeVisible();
    // The wait above drew the scroller, and a drawn element has a box, so the box is there.
    const box = await scroller.boundingBox();
    if (box === null) {
      throw new Error("board scroller is not visible");
    }
    const swipeY = box.y + 30;
    const swipeEnded = await armScrollEnd(scroller);
    await touchDrag(page, { x: box.x + 340, y: swipeY }, { x: box.x + 60, y: swipeY }, 0, 8);
    await swipeEnded();
    const landed = await snapOffsets(scroller);
    expect(landed.scrollLeft).toBeGreaterThan(100);
    expect(landed.lefts.some((left) => Math.abs(left - landed.scrollLeft) <= 2)).toBe(true);

    // Bring Todo to the start (its own snap point) for the gestures on its cards.
    await scrollToColumn(scroller, 3);
    const atTodo = await snapOffsets(scroller);
    expect(atTodo.scrollLeft).toBe(atTodo.lefts[3]);
    const inProgress = page.getByRole("region", { name: "In progress" });

    // A vertical swipe scrolls the page and lifts nothing. Both the fling it hands Chromium and
    // the scroll back to the top have to come to rest before the gestures below measure a card:
    // the page is where the finger aims, and it is still moving for as long as the fling runs.
    const alphaCard = todo.getByRole("article", { name: `${alpha.key} Alpha` });
    const alphaCenter = await centerOf(alphaCard);
    const swipeSettled = await armPageScrollEnd(page);
    await touchDrag(page, { x: alphaCenter.x, y: 600 }, { x: alphaCenter.x, y: 250 }, 0, 6);
    await expect.poll(() => page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
    await swipeSettled();
    await expect(todo.locator("article.opacity-50")).toHaveCount(0);
    expect(patches).toEqual([]);
    // The page is below the top, which is what says this scroll has somewhere to go: a `scrollTo`
    // that moves nothing fires no `scrollend`, and this wait would then have nothing to wait for.
    const backAtTop = await armPageScrollEnd(page);
    await page.evaluate(() => window.scrollTo(0, 0));
    await backAtTop();

    // A tap on the priority badge opens its menu without lifting the card...
    const badge = priorityTrigger(alphaCard, alpha.key);
    const menu = page.getByRole("menu", { name: `Priority of ${alpha.key}` });
    await touchHold(page, await centerOf(badge), 0);
    await expect(menu).toBeVisible();
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    // ...and neither does a long press on it, which closes that menu again.
    await touchHold(page, await centerOf(badge), 300);
    await expect(menu).toHaveCount(0);
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    // The press ran twice the sensor's 150 ms delay (`IssueBoard.tsx`), so a lift would be on the
    // card by now; the margin is for a navigation the release could still be sending.
    await sleep(100);
    expect(patches).toEqual([]);
    expect(new URL(page.url()).pathname).toBe("/projects/CORE/issues");

    // A quick tap on the title opens the issue.
    await touchHold(page, await centerOf(alphaCard.getByRole("link")), 0);
    await expect(page).toHaveURL(new RegExp(`/issues/${alpha.key}$`));
    await page.goBack();
    await expect(todo.getByRole("article")).toHaveCount(7);

    // A long press on the title lifts the card instead of following the link (the accepted
    // trade-off of a whole-card handle); with no movement it drops back where it was. The press
    // outlasts the sensor's 150 ms delay, and the drop back is what says the lift is over. The
    // lift itself is read from the page, not polled for: the card carries `opacity-50` only from
    // the render React commits for the sensor's activation to the release, which on a loaded
    // machine leaves it standing for tens of milliseconds - less than one round trip of a poll,
    // which then reports a press that lifted nothing. Reading the page's own record is also what
    // lets the press keep the length written here, instead of lasting however long a poll takes
    // to catch up with a card that is in flight meanwhile.
    const link = alphaCard.getByRole("link");
    const lifted = await armLift(alphaCard);
    await touchHold(page, await centerOf(link), 250);
    await lifted();
    await expect(alphaCard).not.toHaveClass(/opacity-50/);
    // The margin is for a navigation the press could still be sending.
    await sleep(100);
    expect(new URL(page.url()).pathname).toBe("/projects/CORE/issues");
    expect(patches).toEqual([]);

    // K2: hold to lift, carry the card into the right-hand auto-scroll zone (the board scrolls
    // faster the deeper the finger goes, so stay shallow) until In progress has arrived, step
    // back into the middle of the board and let go - the card changes column.
    const bravoCard = todo.getByRole("article", { name: `${bravo.key} Bravo` });
    const from = await centerOf(bravoCard);
    const movePatch = patchOf(page, bravo.key);
    const finger = await pressFinger(page, from);
    await expect(bravoCard).toHaveClass(/opacity-50/);
    await expect(scroller).toBeVisible();
    // The wait above drew the scroller, and a drawn element has a box, so the box is there.
    const scrollerBox = await scroller.boundingBox();
    if (scrollerBox === null) {
      throw new Error("board scroller is not visible");
    }
    const edge = scrollerBox.x + scrollerBox.width;
    await finger.moveTo({ x: edge - scrollerBox.width * 0.2 + 20, y: from.y }, 16);
    // In progress's arrival is watched from inside the page, a frame at a time. A sample taken
    // from Node answers a round trip late and the board scrolls on all the while: between the
    // sample that saw the column's edge arrive and the box read to aim at it, the board ran that
    // edge 300 px further on this machine, past the drop the row then refused to make.
    expect(await columnArrived(inProgress, 150)).toBeLessThanOrEqual(150);
    // The drop is in the middle of the scroller, between the auto-scroll zones of its outer
    // fifths, so one step there both stops the board and is already where the finger lets go.
    // Reading the column's box only once the board has come to rest is what makes that reading
    // worth anything.
    const drop = scrollerBox.x + scrollerBox.width / 2;
    await finger.moveTo({ x: drop, y: from.y }, 1);
    await scrollerAtRest(scroller);
    // The scroller came to rest above, so the column's box read now is where the drop lands.
    const landing = await inProgress.boundingBox();
    if (landing === null || landing.x > drop - 40 || landing.x + landing.width < drop + 40) {
      throw new Error(
        `In progress is not under the touch drop at ${drop}: ${JSON.stringify(landing)}`
      );
    }
    // A finger resting before it lifts: the drop target is read at the lift, so this pause is
    // part of the gesture under test.
    await page.waitForTimeout(100);
    await finger.lift();
    const moveResponse = await movePatch;
    expect(moveResponse.status()).toBe(200);
    expect(moveResponse.request().postDataJSON()).toEqual({
      status: "in_progress",
      rank: {},
    });
    await expect(inProgress).toContainText("Bravo");
    await expect(todo).not.toContainText("Bravo");
  } finally {
    await context.close();
  }
});

test("URL-backed labels drive the List table and Board strip and survive a reload", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Plain card" });
  const labelled = await createIssue({ project: "CORE", title: "Labelled card" });
  await patchIssue(labelled.key, { labels: ["frontend"] });
  await createIssue({ project: "CORE", title: "Another plain" });

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  try {
    await page.goto("/projects/CORE");
    await expect(page.getByText("Plain card")).toBeVisible();
    // Pick a label from the table header; the Board reads the same label parameter.
    await pickFilterOption(page, "Labels", "frontend");
    await expect(page).toHaveURL(/[?&]label=frontend(?:&|$)/);
    await expect(page.getByText("Labelled card")).toBeVisible();
    await expect(page.getByText("Plain card")).toBeHidden();

    // The Board obeys the same URL state and shows the same card set.
    await page.getByRole("button", { name: "Board" }).click();
    const board = page.getByRole("region", { name: "Project board" });
    await expect(board.getByRole("article")).toHaveText([/Labelled card/]);
    await expect(page).toHaveURL(/[?&]label=frontend(?:&|$)/);

    // A reload keeps the filter, on the Board.
    await page.reload();
    await expect(board.getByRole("article")).toHaveText([/Labelled card/]);
    await expect(page.getByRole("button", { name: "Filters · 1", exact: true })).toBeVisible();

    // Removing the chip on the Board shows every card again.
    await page.getByRole("button", { name: "Remove Label: frontend filter" }).click();
    await expect(board.getByRole("article")).toHaveCount(3);
    await expect(page).not.toHaveURL(/[?&]label=/);
  } finally {
    await context.close();
  }
});

test("a filtered drag names the visible neighbours and the hidden card interleaves without error", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  // Todo ranks Alpha < Bravo < Charlie < Delta (creation order); the filter hides Bravo -
  // every other Todo card carries the label.
  const seedCard = async (title: string, labels?: string[]) => {
    const card = await createIssue({ project: "CORE", title });
    await patchIssue(
      card.key,
      labels === undefined ? { status: "todo" } : { labels, status: "todo" }
    );
    return card;
  };
  const alpha = await seedCard("Alpha", ["keep"]);
  const bravo = await seedCard("Bravo");
  const charlie = await seedCard("Charlie", ["keep"]);
  const delta = await seedCard("Delta", ["keep"]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ width: 1920, height: 900 });
  try {
    await page.goto("/projects/CORE?label=keep");
    await page.getByRole("button", { name: "Board" }).click();
    const todo = page.getByRole("region", { name: "Todo" });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Charlie/, /Delta/]);

    // Delta dropped above Charlie: the visible neighbours are Alpha and Charlie - the hidden
    // Bravo, ranked between them, is never named.
    const deltaCard = todo.getByRole("article", { name: `${delta.key} Delta` });
    const charlieCard = todo.getByRole("article", { name: `${charlie.key} Charlie` });
    const movePatch = patchOf(page, delta.key);
    await mouseDrag(page, deltaCard, charlieCard, 4);
    const moveResponse = await movePatch;
    expect(moveResponse.status()).toBe(200);
    expect(moveResponse.request().postDataJSON()).toEqual({
      rank: { after: alpha.key, before: charlie.key },
    });
    await expect(todo.getByRole("article")).toHaveText([/Alpha/, /Delta/, /Charlie/]);

    // Lifting the filter shows the hidden card interleaved among the moved ones - no alert,
    // Alpha still first and Charlie still last.
    await page.getByRole("button", { name: "Remove Label: keep filter" }).click();
    await expect(todo.getByRole("article")).toHaveCount(4);
    await expect(todo.getByRole("article").first()).toHaveText(/Alpha/);
    await expect(todo.getByRole("article").last()).toHaveText(/Charlie/);
    await expect(todo.getByRole("article", { name: `${bravo.key} Bravo` })).toBeVisible();
    await expect(page.getByRole("alert")).toHaveCount(0);
  } finally {
    await context.close();
  }
});

test("the board groups into priority swimlanes on a toggle that persists per signed-in user, and dragging across lanes reassigns priority", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const p0 = await createIssue({ project: "CORE", title: "P0 card" });
  const p3 = await createIssue({ project: "CORE", title: "P3 card" });
  const none = await createIssue({ project: "CORE", title: "No priority card" });
  // Creation order is the rank order inside a lane; none of these writes reads it.
  await Promise.all([
    patchIssue(p0.key, { status: "todo", priority: 0 }),
    patchIssue(p3.key, { status: "todo", priority: 3 }),
    patchIssue(none.key, { status: "todo" }),
  ]);

  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ width: 1920, height: 900 });
  try {
    await page.goto("/projects/CORE");
    await page.getByRole("button", { name: "Board" }).click();

    // Off by default: one flat Todo column holding every card.
    const lanesToggle = page.getByRole("button", { name: "Show lanes" });
    await expect(lanesToggle).toHaveAttribute("aria-pressed", "false");
    const flatTodo = page.getByRole("region", { name: "Todo" });
    await expect(flatTodo.getByRole("article")).toHaveCount(3);

    await lanesToggle.click();
    await expect(page.getByRole("button", { name: "Hide lanes" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );

    const p0Lane = page.getByRole("region", { name: "P0 lane" });
    const p3Lane = page.getByRole("region", { name: "P3 lane" });
    const noPriorityLane = page.getByRole("region", { name: "No priority lane" });
    await expect(p0Lane.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveText([
      /P0 card/,
    ]);
    await expect(p3Lane.getByRole("region", { name: "Todo" }).getByRole("article")).toHaveText([
      /P3 card/,
    ]);
    await expect(
      noPriorityLane.getByRole("region", { name: "Todo" }).getByRole("article")
    ).toHaveText([/No priority card/]);

    // The choice persists per signed-in user, not the URL: a reload keeps it.
    await page.reload();
    await expect(page.getByRole("button", { name: "Hide lanes" })).toHaveAttribute(
      "aria-pressed",
      "true"
    );
    await expect(page.getByRole("region", { name: "P0 lane" })).toBeVisible();

    // Dragging the no-priority card into the P0 lane's Todo column reassigns its priority -
    // the same gesture that reassigns status when a card crosses a column.
    const noPriorityCard = page
      .getByRole("region", { name: "No priority lane" })
      .getByRole("article", { name: `${none.key} No priority card` });
    const p0Todo = page.getByRole("region", { name: "P0 lane" }).getByRole("region", {
      name: "Todo",
    });
    const reassignPatch = patchOf(page, none.key);
    await mouseDrag(page, noPriorityCard, p0Todo.getByRole("article").first());
    const reassignResponse = await reassignPatch;
    expect(reassignResponse.status()).toBe(200);
    expect(reassignResponse.request().postDataJSON()).toEqual({
      priority: 0,
      rank: { before: p0.key },
    });
    await expect(p0Todo.getByRole("article")).toHaveText([/No priority card/, /P0 card/]);
    await expect(
      page.getByRole("region", { name: "No priority lane" }).getByRole("article")
    ).toHaveCount(0);
    await expect.poll(() => getIssue(none.key)).toMatchObject({ priority: 0 });

    // Turning lanes off restores the flat board - the toggle state, not the data, decides the
    // layout.
    await page.getByRole("button", { name: "Hide lanes" }).click();
    await expect(page.getByRole("region", { name: "P0 lane" })).toHaveCount(0);
  } finally {
    await context.close();
  }
});

// The board's own toggles are controls a finger presses, so on a phone each holds the 44 px
// touch target; what the toggles do is the two walks above, which assert it at any width.
test("the board's view, lanes and edge toggles keep touch-sized targets @phone @phone-only", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await createIssue({ project: "CORE", title: "Toggle card" });

  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.goto("/projects/CORE");
    const boardToggle = page.getByRole("button", { name: "Board" });
    await expect(boardToggle).toHaveCSS("min-height", "44px");
    await boardToggle.click();
    for (const name of ["Show lanes", "Show Icebox & Done"]) {
      await expect(page.getByRole("button", { name })).toHaveCSS("min-height", "44px");
    }
  } finally {
    await context.close();
  }
});

// The rows of dispatch://INFRA-299 on the board: while a pick is live - held by Playwright's
// route control at least `PICK_HOLD_MS` (`holdIssuePatches`) - the card shows it over every
// answer of the project's list, whose read a rename of another issue starts (every event of an
// issue reads its project's list again, `api/event-refresh.ts`), and after the picker that made
// it has left the screen. Each row records every value the card's picker shows, at every
// animation frame (`watchShown`), from the pick until the saves end.

/** A project with an issue at P3 to pick on and another issue whose rename reads the list. */
async function boardWithIssueAtP3(): Promise<{ key: string; other: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Priority under a held save" });
  const other = await createIssue({ project: "CORE", title: "Another issue of the project" });
  await Promise.all([
    patchIssue(issue.key, { priority: 3, status: "todo" }),
    patchIssue(other.key, { status: "todo" }),
  ]);
  return { key: issue.key, other: other.key };
}

/** The project's issues as the Board view, read from the server. */
async function openBoard(page: Page): Promise<void> {
  await page.getByRole("button", { name: "Board" }).click();
  await expect(page.getByRole("region", { name: "Project board" })).toBeVisible();
}

// SLOW(>=2s): a save held three seconds while the board reads its list again.
test("picker keeps a pick on screen: the priority on the project board, over a list read that answers while its save is out", async ({
  browser,
}) => {
  const { key, other } = await boardWithIssueAtP3();
  const renamed = "Another issue, renamed while a save is out";
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${key}`);
    const held = await holdIssuePatches(page, key);
    const reads = logReads(page, listRead("project", "CORE"));
    await page.goto("/projects/CORE");
    await openBoard(page);
    // This tree's priority is a menu (INFRA-2160's R4), and only a pick in it writes (R5).
    const picker = priorityTrigger(page, key);
    await expectShows(picker, "3");

    await pickIn(picker, "1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // A rename of another issue, straight to the server: its live event has the board read the
    // project's list again, and the answer, carrying P3, lands while the pick's save is held.
    const renamedAt = Date.now();
    await patchIssue(other, { title: renamed });
    await expect(page.getByRole("article", { name: `${other} ${renamed}` })).toBeVisible();
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, renamedAt);

    expect(shown.values.slice(fromPick), "every value the card showed from the pick on").toEqual([
      "1",
    ]);
    expect(await getIssue(key)).toMatchObject({ priority: 1 });
    await expectShows(picker, "1");
  } finally {
    await context.close();
  }
});

// The board split by priority: with every save of the issue held at least `PICK_HOLD_MS`, a pick
// on a card and then two drags of it, each to another priority lane. The row sends no write of
// its own straight to the server: the project's list read waits for the board's moves
// (`boardMovesSettled`), so no answer of it lands while they are out.
// SLOW(>=2s): three saves each held three seconds, two drags across lanes, dnd-kit's landings and
// a reload.
test("picker keeps a pick on screen: a card dragged to another priority lane while its pick is out stays in that lane", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const anchor = await createIssue({ project: "CORE", title: "P0 card" });
  const issue = await createIssue({ project: "CORE", title: "Picked card" });
  await Promise.all([
    patchIssue(anchor.key, { priority: 0, status: "todo" }),
    patchIssue(issue.key, { priority: 3, status: "todo" }),
  ]);
  const seq = await lastEventSeq(issue.key);
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ width: 1920, height: 900 });
  try {
    const shown = await watchShown(page, `Priority of ${issue.key}`);
    const held = await holdIssuePatches(page, issue.key);
    await page.goto("/projects/CORE");
    await openBoard(page);
    await page.getByRole("button", { name: "Show lanes" }).click();
    const card = page.getByRole("article", { name: `${issue.key} Picked card` });
    const picker = priorityTrigger(card, issue.key);
    await expectShows(picker, "3");

    await pickIn(picker, "1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    const p1Lane = page.getByRole("region", { name: "P1 lane" });
    await expect(p1Lane.getByRole("article", { name: `${issue.key} Picked card` })).toBeVisible();

    // The pick is still out when the card is dragged into the P0 lane: the move joins the
    // issue's line behind it, and the server will store the move's priority after the pick's.
    const p0Todo = page.getByRole("region", { name: "P0 lane" }).getByRole("region", {
      name: "Todo",
    });
    await mouseDrag(page, card, p0Todo.getByRole("article").first());
    await expect.poll(() => shown.values.at(-1)).toBe("0");
    const fromDrop = shown.values.length - 1;
    await expect(p0Todo.getByRole("article", { name: `${issue.key} Picked card` })).toBeVisible();
    // A second drag, into the P2 lane, while the pick and the first drag are both out: the newer
    // drag is the card's lane from the moment it is made, although its save waits for the first
    // drag's answer before it goes out.
    const p2Todo = page.getByRole("region", { name: "P2 lane" }).getByRole("region", {
      name: "Todo",
    });
    await mouseDrag(page, card, p2Todo);
    await expect.poll(() => shown.values.at(-1)).toBe("2");
    const fromSecondDrop = shown.values.length - 1;
    await expect(p2Todo.getByRole("article", { name: `${issue.key} Picked card` })).toBeVisible();
    held.release();
    await savesEnded(page, held);

    // Every save of the issue - the pick's and each drag's - was held at least `PICK_HOLD_MS`.
    const holds = held.sent.map((patch) => ({
      body: patch.body,
      heldMs: (patch.endedAt ?? Number.NaN) - patch.arrivedAt,
    }));
    console.log(`PICK-HOLDS [${testInfo.project.name}] ${JSON.stringify(holds)}`);
    expect(holds.length, `the pick's save and each drag's: ${JSON.stringify(holds)}`).toBe(3);
    expect(
      holds.every((hold) => hold.heldMs >= PICK_HOLD_MS),
      `every save of the issue was held at least ${PICK_HOLD_MS} ms: ${JSON.stringify(holds)}`
    ).toBe(true);

    expect(
      shown.values.slice(fromPick, fromDrop).some((value) => value.split("|").includes("3")),
      `every value the card showed from the pick to the first drop: ${JSON.stringify(shown.values.slice(fromPick, fromDrop))}`
    ).toBe(false);
    expect(
      shown.values.slice(fromDrop, fromSecondDrop),
      "every value the card showed from the first drop to the second"
    ).toEqual(["0"]);
    expect(
      shown.values.slice(fromSecondDrop),
      "every value the card showed from the second drop on"
    ).toEqual(["2"]);
    await expect(p2Todo.getByRole("article", { name: `${issue.key} Picked card` })).toBeVisible();
    expect(await storedValues(issue.key, "priority", seq)).toEqual([1, 0, 2]);
    expect(await getIssue(issue.key)).toMatchObject({ priority: 2 });
    // A fresh read of the board, once every save has ended: the card is in the P2 lane.
    await page.reload();
    await expect(p2Todo.getByRole("article", { name: `${issue.key} Picked card` })).toBeVisible();
    await expectShows(picker, "2");
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): a save held three seconds while the project's board is opened and read, and read
// again for a rename of another issue.
test("picker keeps a pick on screen: a pick made on the issue page shows on the board opened while its save is out", async ({
  browser,
}) => {
  const { key, other } = await boardWithIssueAtP3();
  const renamed = "Another issue, renamed while the board shows the pick";
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    const shown = await watchShown(page, `Priority of ${key}`);
    const held = await holdIssuePatches(page, key);
    const reads = logReads(page, listRead("project", "CORE"));
    await page.goto(`/issues/${key}`);
    const picker = pagePicker(page, `Priority of ${key}`);
    await expectShows(picker, "3");

    await pickIn(picker, "1");
    await expect.poll(() => shown.values.at(-1)).toBe("1");
    const fromPick = shown.values.length - 1;
    await expect.poll(() => held.sent.length).toBe(1);
    // The picker leaves the screen with its save still out, and the project's list is read
    // from the server, which still holds P3.
    const leftAt = Date.now();
    await navigateInApp(page, "/projects/CORE");
    await openBoard(page);
    const card = page.getByRole("article", { name: `${key} Priority under a held save` });
    await expectShows(priorityTrigger(card, key), "1");
    await expect.poll(() => reads.some((read) => read.answeredAt !== undefined)).toBe(true);
    // A rename of another issue of the project, straight to the server, with the save still
    // held: its live event has the board read the project's list again, and the answer, carrying
    // P3, lands while the save is out.
    const renamedAt = Date.now();
    await patchIssue(other, { title: renamed });
    await expect(page.getByRole("article", { name: `${other} ${renamed}` })).toBeVisible();
    await expect.poll(() => readAnsweredSince(reads, renamedAt)).toBe(true);
    await nextFrames(page);
    await expectShows(priorityTrigger(card, key), "1");
    held.release();
    await savesEnded(page, held);

    expectReadWhileHeld(reads, held, leftAt);
    expectReadWhileHeld(reads, held, renamedAt);

    // The issue page's picker, then the table's row and the board's card, with no picker drawn
    // between them (`(none)`): each shows the pick.
    expect(
      [...new Set(shown.values.slice(fromPick).filter((value) => value !== "(none)"))],
      `every value shown from the pick on: ${JSON.stringify(shown.values.slice(fromPick))}`
    ).toEqual(["1"]);
    expect(await getIssue(key)).toMatchObject({ priority: 1 });
    await expectShows(priorityTrigger(card, key), "1");
  } finally {
    await context.close();
  }
});
