import { CONTROL_LABELS, PRESET_NAMES } from "../web/src/features/issue/page-words";
import { blockNodeId } from "../web/src/features/issue/tree-drafts";
import { DRAWN_LEVELS } from "../web/src/features/issue/tree-fold";
import { createAsk, createIssue, createProject, getArtifact, getIssue } from "./api";
import { barAction, selectEditorText, selectTextIn, specAddress } from "./editor";
import { seedWholeIssue, type WholeIssueSeed } from "./issue-layout-seed";
import { requirementBlockId } from "./issue-layout-seed-text";
import {
  activePreset,
  caretInNode,
  childrenSection,
  conversationStateControl,
  drawnRows,
  filterBox,
  focusedNodeId,
  focusInside,
  focusThePage,
  gotoIssue,
  identityBlockBottom,
  issuePage,
  marginSlot,
  nodeBodies,
  nodeMenuButton,
  nodeRow,
  onScreen,
  openedArtifact,
  openMenu,
  pickPreset,
  pinnedBodies,
  rowBody,
  tree,
  treeActiveDescendant,
} from "./issue-page";
import { leaveFocus, onBody, openIssue } from "./keyboard-issue";
import { type Browser, type BrowserContext, expect, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/**
 * The issue and project keyboard, in the rows whose claim reaches the server: a priority or a
 * pin written and read back, a refusal, and the digits that must write nothing. The roving,
 * the bands and what `?` greys where - everything the page draws over one fixed answer - is in
 * `keyboard-issue.stub.e2e.ts`.
 */

const session = {
  actor: { kind: "session" as const, id: "e2e-keyboard-inbox" },
  as: "agent" as const,
};

/** Whether a request is a pin or unpin write. The Conversation tab marks the issue read through
 *  the same `PUT /me/issues/<key>/state` (`{ last_read_seq }`), and that write can land at any
 *  moment after the page opens, so a row counting pins counts only the bodies that set `pinned`. */
function isPinWrite(request: Request): boolean {
  return request.method() === "PUT" && "pinned" in (request.postDataJSON() ?? {});
}

test("the issue scope reaches the header controls and the tab chords, and ? lists them under Issue", async ({
  browser,
}, testInfo) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Keyboard issue" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Keyboard issue");
    const header = page.getByTestId("issue-header");
    const priority = page.getByRole("combobox", { name: `Priority of ${issue.key}` });
    const status = page.getByRole("combobox", { name: "Status" });

    // p and s reach their own control: two assertions, so a p that focused Status fails.
    await page.keyboard.press("p");
    await expect(priority).toBeFocused();
    await leaveFocus(page);
    await page.keyboard.press("s");
    await expect(status).toBeFocused();
    await leaveFocus(page);

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
    await leaveFocus(page);

    const pinned = page.waitForResponse(
      (response) =>
        response.request().method() === "PUT" &&
        new URL(response.url()).pathname === `/api/v1/me/issues/${issue.key}/state`
    );
    await page.keyboard.press("Shift+P");
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    expect((await pinned).ok()).toBe(true);
    await leaveFocus(page);

    await page.reload();
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    await leaveFocus(page);

    // l opens the labels popover - the listbox itself, not just its trigger.
    await page.keyboard.press("l");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Search or create label" })).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.getByRole("listbox", { name: "Labels options" })).toHaveCount(0);
    await leaveFocus(page);

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
    await leaveFocus(page);

    // t is a chord: the indicator shows it waiting, then the second key picks that tab. c is a
    // global shortcut on its own (the create dialog), and completing the chord is not it. The
    // chord's second key must follow within the keymap's 1000 ms window, so nothing slow sits
    // between the two keys: the screenshot is taken once the tab is reached.
    await page.keyboard.press("t");
    const indicator = page.getByTestId("chord-indicator");
    await expect(indicator).toHaveText(/t/);
    await page.keyboard.press("c");
    await expect(indicator).toHaveCount(0);
    await expect(page.getByRole("dialog", { name: "Create issue" })).toHaveCount(0);
    await expect(conversationStateControl(page, "words")).toHaveAttribute("aria-selected", "true");
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-conversation-${testInfo.project.name}.png`),
    });

    // Every address the nine tabs answered still resolves (dispatch://INFRA-1196 R19), so each
    // chord of the spec's table opens its address; four of them are a preset's own and the rest
    // open the Tree preset, whose content the address names. `/collaborators` resolves too and
    // is read by its own address in `e2e/issue-layout.e2e.ts`, never by a key: the spec's table
    // names no chord for it (R9).
    for (const [key, address] of [
      ["k", "verification"],
      ["p", "plan"],
      ["h", "children"],
      ["a", "artifacts"],
      ["v", "activity"],
      ["s", "spec"],
    ] as const) {
      await page.keyboard.press("t");
      await page.keyboard.press(key);
      await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/${address}$`));
    }

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
    // Each chord is listed under what it opens on this page, not under a tab the page no
    // longer has (`web/src/features/issue/useIssueKeymap.ts`'s `CHORDS`): the presets by the
    // row's own names, and the panel, the children and the node menu as the page names them.
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: "Open the conversation in the panel" })
        .locator("kbd")
    ).toHaveText(["t", "c"]);
    // `t v` is the activity and `t c` the conversation, so the Checks preset takes `k`.
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `Open the ${PRESET_NAMES.checks} preset` })
        .locator("kbd")
    ).toHaveText(["t", "k"]);
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: `Open the ${PRESET_NAMES.plan} preset` })
        .locator("kbd")
    ).toHaveText(["t", "p"]);
    // The activity takes `t v`, since `t a` opens the node menu and `l` the labels.
    await expect(issueSection).toContainText("Open the activity in the panel");
    await expect(
      issueSection
        .getByRole("listitem")
        .filter({ hasText: "Open the activity in the panel" })
        .locator("kbd")
    ).toHaveText(["t", "v"]);
    // The collaborators have no chord: the spec's table names two chords into the panel, `t c`
    // and `t v`, and `t o` was a third it does not hold (dispatch://INFRA-1196 R9). The help
    // lists what the page binds, so it lists no row for them either.
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "collaborators" }),
      "the help still offers a key for the collaborators, which the spec's table does not name"
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

test("a priority the server refuses shows the header's own failure, whether the digit key or the picker sent it", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Refused priority" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Refused priority");
    const header = page.getByTestId("issue-header");
    await page.route(`**/api/v1/issues/${issue.key}`, async (route) => {
      if (route.request().method() !== "PATCH") {
        await route.continue();
        return;
      }
      // Dispatch's verdict: a 5xx would be tried again under the write's key, not refused.
      await route.fulfill({
        body: JSON.stringify({ code: "ISSUE_INPUT", error: "Dispatch refused the priority" }),
        contentType: "application/json",
        status: 400,
      });
    });

    // The digit key and the picker are one write, so a refusal of either is reported once, by
    // the control the reader is looking at, and the badge rolls back.
    await page.keyboard.press("2");
    const failure = header
      .getByRole("alert")
      .filter({ hasText: `Could not update the priority of ${issue.key}.` });
    await expect(failure).toBeVisible();
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });

    // The picker goes through that same write, so its refusal lands in the one failure the
    // digit key already showed instead of raising a second beside it.
    await header.getByRole("combobox", { name: `Priority of ${issue.key}` }).selectOption("3");
    await expect(failure).toHaveCount(1);
    await expect(failure.getByRole("button", { name: "Retry" })).toBeVisible();
    await expect(header.locator("span", { hasText: /^Priority$/ })).toBeVisible();
    await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
  } finally {
    await context.close();
  }
});

test("Escape leaves a focused header select, so the next chord is a chord and not type-ahead", async ({
  browser,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Type-ahead" });
  const context = await asUser(browser, "alice");
  try {
    const page = await context.newPage();
    await page.setViewportSize({ width: 1280, height: 900 });
    await openIssue(page, issue.key, "Type-ahead");
    // Every PATCH this page sends, so a status the select's own type-ahead picked is caught
    // even though the pill would show it for only as long as the reader stayed.
    const patched: string[] = [];
    page.on("request", (request) => {
      if (request.method() === "PATCH") {
        patched.push(new URL(request.url()).pathname);
      }
    });

    // Three of the status labels begin with `t` (Triage, Todo, Testing), so a `t` that reached
    // the Status select would write one of them instead of starting the tab chord.
    for (const [key, control] of [
      ["s", page.getByRole("combobox", { name: "Status" })],
      ["p", page.getByRole("combobox", { name: `Priority of ${issue.key}` })],
    ] as const) {
      await page.keyboard.press(key);
      await expect(control).toBeFocused();
      await page.keyboard.press("Escape");
      await expect.poll(() => onBody(page)).toBe(true);
      await page.keyboard.press("t");
      await expect(page.getByTestId("chord-indicator")).toHaveText(/t/);
      await page.keyboard.press("c");
      await expect(conversationStateControl(page, "words")).toHaveAttribute(
        "aria-selected",
        "true"
      );
      await pickPreset(page, "Spec");
      await leaveFocus(page);
    }

    expect(patched).toEqual([]);
    await expect
      .poll(() => getIssue(issue.key))
      .toMatchObject({
        priority: null,
        status: "triage",
      });
  } finally {
    await context.close();
  }
});
// The issue page's `0`-`3` write the issue's priority, and an ask card on that page carries option
// radios. A radio takes no typed text, so this slice's editable policy lets a digit through to the
// page - which must not turn "pick option 2" into "this issue is P2". The second half of this row
// is what makes the first mean anything: the same key, off the card, still writes.
test.describe("issue page", () => {
  test("a digit on an ask's option writes no priority, while the same digit on the page does", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Priority and options" });
    await createAsk(
      issue.key,
      { options: [{ label: "Ship" }, { label: "Hold" }], question: "Which way?" },
      session
    );
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(`/issues/${issue.key}`);
      await expect(
        page
          .getByTestId("issue-header")
          .getByRole("heading", { level: 1, name: "Priority and options" })
      ).toBeVisible();

      let patched = 0;
      await page.route(`**/api/v1/issues/${issue.key}`, (route) => {
        if (route.request().method() === "PATCH") patched += 1;
        return route.fallback();
      });

      // Focus an option radio, as a reader answering the ask would, and press a digit.
      const hold = page.getByRole("radio", { name: "Hold" });
      await hold.click();
      await expect(hold).toBeFocused();
      await page.keyboard.press("2");
      // The handler is synchronous, so a write it let through had left before a request the page
      // issues next could be answered: that answer is the barrier, not a wall-clock wait.
      await page.evaluate(() => fetch("/api/v1/auth/whoami").then(({ status }) => status));
      expect(patched).toBe(0);
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });

      // Off the card, the same key is the header's own: this is what proves the guard is a guard
      // and not a key that stopped working.
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      const patch = page.waitForRequest(
        (request) =>
          request.method() === "PATCH" &&
          new URL(request.url()).pathname === `/api/v1/issues/${issue.key}`
      );
      await page.keyboard.press("2");
      expect((await patch).postDataJSON()).toEqual({ priority: 2 });
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 2 });

      // `Shift+P` writes without focusing anything either, so it needs the same guard, and the
      // same two halves prove it: nothing from the card, the pin itself from the page.
      let pinned = 0;
      await page.route(`**/api/v1/me/issues/${issue.key}/state`, (route) => {
        if (isPinWrite(route.request())) pinned += 1;
        return route.fallback();
      });
      await hold.click();
      await expect(hold).toBeFocused();
      await page.keyboard.press("Shift+P");
      await page.evaluate(() => fetch("/api/v1/auth/whoami").then(({ status }) => status));
      expect(pinned).toBe(0);
      await expect(page.getByRole("button", { name: "Pin issue" })).toBeVisible();

      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press("Shift+P");
      await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();
      // The header shows the pin optimistically, before its `PUT` leaves the page, so the write
      // is waited for rather than read the moment the button flips.
      await expect.poll(() => pinned).toBe(1);
    } finally {
      await context.close();
    }
  });

  // The ask card is not the only control a reader's hand rests on. The margin ask composer's
  // `Allow multiple` is an ordinary page checkbox, outside every ask card, and a digit or
  // `Shift+P` straight after ticking it must not write the issue's priority or its pin either -
  // the rule is that these two keys write only from the page itself, not from a control.
  test("a digit on a page checkbox writes no priority, and neither does Shift+P", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({
      project: "CORE",
      spec: "The quick brown fox",
      title: "Composer and priority",
    });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto(await specAddress(issue.key));
      await selectEditorText(page, "brown");
      await barAction(page, "Ask");
      const multiple = page
        .getByRole("form", { name: "Comment composer" })
        .getByLabel("Allow multiple");
      await expect(multiple).toBeVisible();

      let patched = 0;
      await page.route(`**/api/v1/issues/${issue.key}`, (route) => {
        if (route.request().method() === "PATCH") patched += 1;
        return route.fallback();
      });
      let pinned = 0;
      await page.route(`**/api/v1/me/issues/${issue.key}/state`, (route) => {
        if (isPinWrite(route.request())) pinned += 1;
        return route.fallback();
      });

      await multiple.focus();
      await expect(multiple).toBeFocused();
      await page.keyboard.press("1");
      await page.keyboard.press("Shift+P");
      // The server's own answers are the wait: the issue carries no priority and the page still
      // offers Pin, each read over a round trip of its own. A write either key had made would
      // have left the page before those answers came back - it goes out in the task of the
      // keypress that made it - so the counters are read after them rather than after a window.
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
      await expect(page.getByRole("button", { name: "Pin issue" })).toBeVisible();
      expect({ patched, pinned }).toEqual({ patched: 0, pinned: 0 });

      // From the page itself both keys still write, which is what makes the first half a guard.
      await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
      await page.keyboard.press("1");
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: 1 });
      await page.keyboard.press("Shift+P");
      await expect(page.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    } finally {
      await context.close();
    }
  });
});

/**
 * The tree's own keyboard, which is c-keyboard of dispatch://INFRA-1196: every row of the key
 * table in the spec's Design section, the nine chords the page bound before the redesign, the
 * one Tab stop, the arrows, the focused row clear of the sticky header, `t f`, `$mod+Alt+m`, and
 * the WAI-ARIA tree pattern. Each row reads the page through `e2e/issue-page.ts` and spells no
 * selector of its own. The rows above about the project List and the Board are untouched: they
 * are not about this page.
 */

interface OpenedTree {
  readonly context: BrowserContext;
  readonly page: Page;
  readonly seeded: WholeIssueSeed;
}

/** The whole issue, opened by one signed-in reader with the focus on the page itself, which is
 *  where reading mode is. A key pressed before this returns reaches no handler, because the
 *  keymap binds only once sign-in resolves. It asserts nothing: each row below asserts the key
 *  or the tree property it is about first, and `expectTreeDrawn` after it. */
async function openTree(browser: Browser): Promise<OpenedTree> {
  const seeded = await seedWholeIssue();
  const context = await asUser(browser, "alice");
  const page = await context.newPage();
  await page.setViewportSize({ height: 900, width: 1280 });
  await gotoIssue(page, `/issues/${seeded.key}`);
  await focusThePage(page);
  return { context, page, seeded };
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
  return (await focusedNodeId(page)) ?? (await treeActiveDescendant(page));
}

/** The focused row is on screen and clear of the sticky identity block, which R9 requires after
 *  every move and not only at the end. */
async function expectFocusedRowClear(page: Page, after: string): Promise<void> {
  const id = await focusedRow(page);
  expect(id, `${after} left the focus on no row`).not.toBeNull();
  const row = (await drawnRows(page)).find((candidate) => candidate.id === id);
  const box = row?.box ?? null;
  expect(box, `${after} left the focus on a row with no box`).not.toBeNull();
  const header = (await identityBlockBottom(page)) ?? 0;
  expect(
    box?.y ?? -1,
    `after ${after} the focused row sits under the sticky header`
  ).toBeGreaterThanOrEqual(header - 1);
  expect(
    (box?.y ?? 0) + (box?.height ?? 0),
    `after ${after} the focused row is below the viewport`
  ).toBeLessThanOrEqual(900);
}

test("the tree's keys move, fold, zoom and open the focused node", async ({ browser }) => {
  const { context, page, seeded } = await openTree(browser);
  try {
    const order = await visibleIds(page);
    expect(
      order.length,
      "the keys have no rows to move through: the tree draws none"
    ).toBeGreaterThan(3);
    await expectTreeDrawn(page);

    // j, k and the arrows move to the next and previous visible row.
    await page.keyboard.press("Tab");
    expect(await focusedRow(page), "Tab did not put the reader on the first row").toBe(order[0]);
    for (const [index, step] of [
      [1, "j"],
      [2, "ArrowDown"],
      [1, "k"],
      [0, "ArrowUp"],
    ] as const) {
      await page.keyboard.press(step);
      expect(await focusedRow(page), `${step} did not move to row ${index}`).toBe(order[index]);
      await expectFocusedRowClear(page, step);
    }

    // Home and End reach the first and last visible row.
    await page.keyboard.press("End");
    expect(await focusedRow(page), "End did not reach the last visible row").toBe(
      order[order.length - 1]
    );
    await expectFocusedRowClear(page, "End");
    await page.keyboard.press("Home");
    expect(await focusedRow(page), "Home did not reach the first visible row").toBe(order[0]);

    // The right arrow unfolds the focused node and then moves into its first child; the left
    // arrow moves back to the parent and then folds it. The branch is one above the drawn
    // levels, because that is where unfolding draws the children as rows of their own: a
    // branch below them folds to one line the open-deeper key is for (`tree-fold.ts`), which
    // is no child row for an arrow to move into. The page opens folded to the frontier, so
    // there are plenty either way.
    const folded = (await drawnRows(page)).find(
      (row) => row.expanded === "false" && Number(row.level) < DRAWN_LEVELS
    );
    expect(
      folded,
      "no branch above the drawn levels is folded, so the arrows have nothing to unfold"
    ).toBeDefined();
    const branch = folded?.id ?? "";
    await nodeRow(page, branch).first().focus();
    await page.keyboard.press("ArrowRight");
    expect(
      (await drawnRows(page)).find((row) => row.id === branch)?.expanded,
      "the right arrow did not unfold the focused node"
    ).toBe("true");
    expect(
      await focusedRow(page),
      "the right arrow left the focused node before unfolding it"
    ).toBe(branch);
    // The branch's first child is the row after it one level deeper, which is where the second
    // right arrow has to land: "not the branch" would also hold for a focus that was lost or
    // that moved to the branch's own sibling.
    const unfolded = (await drawnRows(page)).filter((row) => row.visible);
    const at = unfolded.findIndex((row) => row.id === branch);
    const firstChild = unfolded[at + 1];
    expect(
      firstChild?.level,
      "the unfolded branch draws no row under it, so it has no first child to move into"
    ).toBe(String(Number(unfolded[at]?.level ?? "0") + 1));
    await page.keyboard.press("ArrowRight");
    expect(
      await focusedRow(page),
      "the second right arrow did not move into the branch's first child"
    ).toBe(firstChild?.id);
    await page.keyboard.press("ArrowLeft");
    expect(await focusedRow(page), "the left arrow did not move to the parent").toBe(branch);
    await page.keyboard.press("ArrowLeft");
    expect(
      (await drawnRows(page)).find((row) => row.id === branch)?.expanded,
      "the second left arrow did not fold the focused node"
    ).toBe("false");

    // o pins the focused node's body open, and again closes it.
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o pinned no body open").toBe(1);
    await page.keyboard.press("o");
    expect(await pinnedBodies(page).count(), "o did not close the pinned body").toBe(0);

    // . zooms into the focused node and , comes back out one level.
    const whole = await visibleIds(page);
    await page.keyboard.press(".");
    const zoomed = await visibleIds(page);
    expect(
      zoomed.length,
      "the zoom key drew the same rows, so it zoomed into nothing"
    ).toBeLessThan(whole.length);
    expect(zoomed[0], "the zoom key did not make the focused node the tree's root").toBe(branch);
    await page.keyboard.press(",");
    expect(await visibleIds(page), "the key back out did not restore the level above").toEqual(
      whole
    );

    // q moves to the next open question, whatever its depth.
    await page.keyboard.press("q");
    const question = await focusedRow(page);
    expect(
      (await drawnRows(page)).find((row) => row.id === question)?.type,
      "the next-question key reached a row that is no open question"
    ).toBe("ask");

    // w moves into the decision behind the focused node.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const decisionsSlug = (await getArtifact(seeded.decisionsArtifactId)).slug;
    await nodeRow(page, blockNodeId(specSlug, requirementBlockId(1)))
      .first()
      .focus();
    await page.keyboard.press("w");
    expect(await focusedRow(page), "w did not move into the decision behind the node").toBe(
      blockNodeId(decisionsSlug, seeded.decisions[0] ?? "")
    );

    // z cycles the whole tree: first lines, first lines with bodies to depth two, everything.
    await focusThePage(page);
    const firstLines = await visibleIds(page);
    await page.keyboard.press("z");
    expect(
      await nodeBodies(page).count(),
      "the second state of the cycle draws no bodies"
    ).toBeGreaterThan(1);
    await page.keyboard.press("z");
    expect(
      (await visibleIds(page)).length,
      "the third state of the cycle draws no more rows than the first"
    ).toBeGreaterThan(firstLines.length);
    await page.keyboard.press("z");
    expect(await visibleIds(page), "the cycle did not come back to the first lines").toEqual(
      firstLines
    );

    // Enter puts the caret in the focused node; Escape leaves the caret for the row, then the
    // tree. This extends the binding the page already has and adds none.
    const second = blockNodeId(specSlug, requirementBlockId(2));
    await nodeRow(page, second).first().focus();
    await page.keyboard.press("Enter");
    expect(await caretInNode(page), "Enter did not put the caret in the focused node").toBe(true);
    await page.keyboard.press("Escape");
    expect(await caretInNode(page), "Escape did not leave the caret").toBe(false);
    expect(await focusedRow(page), "Escape left the row as well as the caret").toBe(second);
    await page.keyboard.press("Escape");
    expect(await focusedRow(page), "the second Escape did not leave the tree").toBeNull();
  } finally {
    await context.close();
  }
});

test("Tab enters and leaves the tree at one stop and every move keeps the row on screen", async ({
  browser,
}) => {
  const { context, page } = await openTree(browser);
  try {
    const order = await visibleIds(page);

    // One stop in: Tab reaches the tree, and the next Tab leaves it rather than stepping through
    // its rows, as the WAI-ARIA tree pattern requires.
    await page.keyboard.press("Tab");
    expect(await focusInside(tree(page)), "Tab did not enter the tree").toBe(true);
    await expectTreeDrawn(page);
    await page.keyboard.press("j");
    const inside = await focusedRow(page);
    expect(inside, "j moved to no row inside the tree").toBe(order[1]);
    await page.keyboard.press("Tab");
    expect(
      await focusInside(tree(page)),
      "Tab stepped to another row of the tree instead of leaving it"
    ).toBe(false);
    await page.keyboard.press("Shift+Tab");
    expect(await focusInside(tree(page)), "Shift+Tab did not come back into the tree").toBe(true);
    expect(await focusedRow(page), "Shift+Tab came back to another row than the one left").toBe(
      inside
    );

    // The focused row is inside the viewport and clear of the sticky header after every move,
    // including the ones that scroll the page.
    for (let step = 0; step < Math.min(order.length - 1, 12); step += 1) {
      await page.keyboard.press("j");
      await expectFocusedRowClear(page, `move ${step + 1} with j`);
    }
    await page.keyboard.press("End");
    await expectFocusedRowClear(page, "End");
    await page.keyboard.press("Home");
    await expectFocusedRowClear(page, "Home");
  } finally {
    await context.close();
  }
});

test("every chord the page bound before the redesign brings a target on screen with the focus in it", async ({
  browser,
}) => {
  const { context, page } = await openTree(browser);
  try {
    // The preset chords of the key table, which hold five of the nine the page bound before this
    // change: `t d`, `t k`, `t s` and `t p`, with `t t` the table's own addition.
    const chords = [
      { preset: PRESET_NAMES.tree, second: "t" },
      { preset: PRESET_NAMES.decisions, second: "d" },
      { preset: PRESET_NAMES.checks, second: "k" },
      { preset: PRESET_NAMES.spec, second: "s" },
      { preset: PRESET_NAMES.plan, second: "p" },
    ] as const;
    for (const chord of chords) {
      await focusThePage(page);
      await page.keyboard.press("t");
      await expect(page.getByTestId("chord-indicator")).toHaveText(/t/);
      await page.keyboard.press(chord.second);
      expect(
        await activePreset(page),
        `t ${chord.second} does not open the ${chord.preset} preset`
      ).toBe(chord.preset);
      await expectTreeDrawn(page);
      // The tree is a column of its own and may be taller than the screen, which no reading of
      // "every pixel on screen" could pass; the page shows the control it leaves the reader on
      // where a part does not fit (`useIssueKeymap.ts`'s `showPart`), and that control is the
      // tree's one tab stop, so the row it lands on is what is read.
      const landed = await focusedRow(page);
      expect(landed, `t ${chord.second} leaves the focus on no row of the tree`).not.toBeNull();
      expect(
        await onScreen(nodeRow(page, landed ?? "").first()),
        `t ${chord.second} does not bring the row it lands on on screen`
      ).toBe(true);
      expect(
        await focusInside(tree(page)),
        `t ${chord.second} does not put the focus in the preset's content`
      ).toBe(true);
    }

    // The conversation in its words state and in its everything state: `t c` and `t v`. The
    // panel the conversation is drawn in is a full-height column (`margin/MarginSheet.tsx`'s
    // `h-dvh`), taller than the screen by construction, so what the chord has to bring on
    // screen is the conversation's own head - the state controls it leaves the focus in - and
    // not every pixel of the panel around it, which no panel of that shape could satisfy.
    for (const [second, state] of [
      ["c", "words"],
      ["v", "everything"],
    ] as const) {
      await focusThePage(page);
      await page.keyboard.press("t");
      await page.keyboard.press(second);
      expect(
        await conversationStateControl(page, state).getAttribute("aria-selected"),
        `t ${second} does not open the conversation in its ${state} state`
      ).toBe("true");
      expect(
        await onScreen(conversationStateControl(page, state)),
        `t ${second} leaves the conversation off screen`
      ).toBe(true);
      expect(
        await focusInside(marginSlot(page)),
        `t ${second} does not put the focus in the panel`
      ).toBe(true);
    }

    // The children section: `t h`.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("h");
    expect(await onScreen(childrenSection(page)), "t h leaves the children off screen").toBe(true);
    expect(
      await focusInside(childrenSection(page)),
      "t h does not put the focus in the children section"
    ).toBe(true);

    // The node menu: `t a`, which named the Artifacts tab before this change.
    await focusThePage(page);
    await page.keyboard.press("t");
    await page.keyboard.press("a");
    expect(await openMenu(page).count(), "t a opens no menu").toBe(1);
    expect(await focusInside(openMenu(page)), "t a does not put the focus in the node menu").toBe(
      true
    );
    expect(
      await nodeMenuButton(page).count(),
      `the menu t a opened is not the one named "${CONTROL_LABELS.other_nodes}" beside the presets`
    ).toBe(1);

    // From the menu `t a` left the focus in, Enter opens the document on the focused item in
    // place of the outline, and Escape - the key table's step after leaving a caret - closes it
    // and draws the outline again.
    await page.keyboard.press("Enter");
    await expect(
      openedArtifact(page),
      "Enter on the node menu's item opens no document"
    ).toHaveCount(1);
    expect(
      await tree(page).count(),
      "the opened document is not drawn in place of the outline"
    ).toBe(0);
    await page.keyboard.press("Escape");
    await expect(tree(page), "Escape does not close the opened document").toBeVisible();
    expect(await openedArtifact(page).count(), "Escape leaves the document drawn").toBe(0);
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

    // $mod+Alt+m still comments on a selection inside a node's own text.
    const specSlug = (await getArtifact(seeded.specArtifactId)).slug;
    const requirement = nodeRow(page, blockNodeId(specSlug, requirementBlockId(2))).first();
    await requirement.focus();
    await page.keyboard.press("Enter");
    expect(await caretInNode(page), "Enter did not put the caret in the node").toBe(true);
    await selectTextIn(page, rowBody(requirement), "requirement");
    await page.keyboard.press("ControlOrMeta+Alt+m");
    await expect(page.getByRole("form", { name: "Comment composer" })).toBeVisible();
  } finally {
    await context.close();
  }
});

test("the tree answers the WAI-ARIA tree pattern's roles and properties", async ({ browser }) => {
  const { context, page } = await openTree(browser);
  try {
    const rows = await drawnRows(page);
    expect(
      rows.length,
      'the tree pattern has nothing to answer for: no role="treeitem" row is drawn'
    ).toBeGreaterThan(0);
    await expectTreeDrawn(page);
    expect(
      rows.filter((row) => row.role !== "treeitem").map((row) => row.id),
      "these rows of the tree are no treeitem"
    ).toEqual([]);
    expect(
      rows.filter((row) => !/^[1-9]\d*$/.test(row.level ?? "")).map((row) => row.id),
      "these rows carry no aria-level"
    ).toEqual([]);

    // A row that holds children says whether it is open; a row that holds none says nothing.
    const levels = rows.map((row) => Number(row.level ?? "0"));
    const holdsChildren = rows.map(
      (row, index) => (levels[index + 1] ?? 0) > (levels[index] ?? 0) || row.foldedCount !== null
    );
    expect(
      rows
        .filter((row, index) => holdsChildren[index] === true && row.expanded === null)
        .map((row) => row.id),
      "these rows hold children and carry no aria-expanded"
    ).toEqual([]);
    expect(
      rows
        .filter((row, index) => holdsChildren[index] !== true && row.expanded !== null)
        .map((row) => row.id),
      "these rows hold no children and carry aria-expanded all the same"
    ).toEqual([]);

    // The tree is one tab stop: it roves with one tabbable row, or it names the row it is on.
    const tabbable = rows.filter((row) => row.tabIndex === 0);
    const named = await treeActiveDescendant(page);
    expect(
      `${tabbable.length}/${named === null ? "none" : "named"}`,
      `the tree has ${tabbable.length} tabbable rows and ${named === null ? "no" : "an"} aria-activedescendant, so it is not one stop`
    ).toMatch(/^(1\/none|0\/named|1\/named)$/);
  } finally {
    await context.close();
  }
});
