import { baseUrl, createAsk, createIssue, createProject, getIssue } from "./api";
import { barAction, selectEditorText } from "./editor";
import { nextFrames } from "./frames";
import { focusThePage } from "./issue-page-g";
import { onBody, openIssue } from "./keyboard-issue";
import { keptWrites } from "./keyed-writes";
import { navigateInApp } from "./sends";
import {
  chunkFile,
  entryScriptPath,
  holdAsset,
  holdAssetUntilReleased,
  ranChunk,
  recordTabReloads,
  tabReloads,
} from "./shell";
import { expect, type Page, type Request, test } from "./test";
import { asUser } from "./users";

/**
 * The issue and project keyboard, in the rows whose claim reaches the server: a priority or a
 * pin written and read back, a refusal, and the digits that must write nothing - on an ask's
 * option, on a page checkbox, and straight after a key that opens a surface over the page, with
 * the rows on what such a surface does when its chunk lands late beside them. The roving, the
 * bands and what `?` greys where - everything the page draws over one fixed answer - is in
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
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    expect((await pinned).ok()).toBe(true);
    await focusThePage(page);

    await page.reload();
    await expect(header.locator("span", { hasText: /^P2$/ })).toBeVisible();
    await expect(header.getByRole("button", { name: "Unpin issue" })).toBeVisible();
    await focusThePage(page);

    // l opens the labels popover - the listbox itself, not just its trigger.
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
    await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
      "aria-selected",
      "true"
    );
    await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`));
    await page.screenshot({
      path: testInfo.outputPath(`issue-tab-conversation-${testInfo.project.name}.png`),
    });

    for (const [key, tab] of [
      ["k", "Verification"],
      ["p", "Plan"],
      ["e", "Steps"],
      ["h", "Children"],
      ["a", "Artifacts"],
      ["v", "Activity"],
      ["o", "Collaborators"],
      ["s", "Spec"],
    ] as const) {
      await page.keyboard.press("t");
      await page.keyboard.press(key);
      await expect(page.getByRole("tab", { name: tab })).toHaveAttribute("aria-selected", "true");
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
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Conversation tab" }).locator("kbd")
    ).toHaveText(["t", "c"]);
    // `t v` is Activity and `t c` the Conversation, so Verification takes its checks' `k`.
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Verification tab" }).locator("kbd")
    ).toHaveText(["t", "k"]);
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Plan tab" }).locator("kbd")
    ).toHaveText(["t", "p"]);
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Steps tab" }).locator("kbd")
    ).toHaveText(["t", "e"]);
    // Collaborators is the last tab and takes `t o`; Activity takes `t v`, since `t a` is
    // Artifacts and `l` the labels.
    await expect(issueSection).toContainText("Activity tab");
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Activity tab" }).locator("kbd")
    ).toHaveText(["t", "v"]);
    await expect(
      issueSection.getByRole("listitem").filter({ hasText: "Collaborators tab" }).locator("kbd")
    ).toHaveText(["t", "o"]);
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
      await expect(page.getByRole("tab", { name: "Conversation" })).toHaveAttribute(
        "aria-selected",
        "true"
      );
      await page.getByRole("tab", { name: "Spec" }).click();
      await focusThePage(page);
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
      await page.goto(`/issues/${issue.key}/spec`);
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

/** Counts the issue's `PATCH`es: the priority a digit writes and the title `e` edits go out as
 *  one each. */
async function patchesOf(page: Page, key: string): Promise<() => number> {
  let patched = 0;
  await page.route(`**/api/v1/issues/${key}`, (route) => {
    if (route.request().method() === "PATCH") patched += 1;
    return route.fallback();
  });
  return () => patched;
}

/** A round trip of the page's own. A write a key let through leaves in the task of that key, so
 *  it is out before this answer can come back - unless the store holds it for the take-up of
 *  what a reload kept (`Writes.run`), which keeps it in the tab from its press: the rows read the
 *  tab's kept writes beside the requests (`keptWrites`). */
async function afterAnyWrite(page: Page): Promise<void> {
  await page.evaluate(() => fetch("/api/v1/auth/whoami").then(({ status }) => status));
}

/** Brings the chunk named `name` into the page's module map, as a page that has fetched and run
 *  it holds it, whatever the code under test starts of its own accord. The entry chunk names the
 *  path of every chunk the shell draws a surface from, and an import of that path runs it; it
 *  settles once the module has run. It does not run the surface's own load (`chunk`,
 *  `web/src/features/shell/chunk-component.tsx`), whose answer is what tells the surface its
 *  chunk is in, so a press soon after it can still draw the box that holds the keys for a moment
 *  (`web/src/features/shell/asked-surface.tsx`): a row that brings its chunk in this way holds
 *  whichever way the press goes, and the row that holds the press to the surface's own commit
 *  brings nothing in.
 */
async function chunkIn(page: Page, name: string): Promise<void> {
  const entry = await entryScriptPath(baseUrl);
  const source = await (await fetch(new URL(entry, baseUrl))).text();
  const file = new RegExp(`${name}-[\\w-]{8}\\.js`, "u").exec(source)?.[0];
  if (file === undefined) throw new Error(`the entry chunk names no ${name} chunk`);
  await page.evaluate((path) => import(path).then(() => undefined), `/assets/${file}`);
}

/** How long React holds back the content of a `Suspense` boundary that has shown its fallback: no
 *  sooner than 300 ms after that fallback (`FALLBACK_THROTTLE_MS` in react-dom). A surface drawn
 *  behind `lazy` could still open that long after the press that showed its fallback. */
const SUSPENSE_REVEAL_HOLD_MS = 300;

/** The window a surface whose chunk has landed would still open in, and then two frames of the
 *  page (`nextFrames`): React wakes with a render it schedules as a task of its own, which runs
 *  inside the frame it was scheduled in, so a surface the landing would draw is on the page by
 *  the end. React's hold is a timer of the page's, so the page's clock spends it (the row installs
 *  the clock before its first load), and no real time is waited for it. */
async function lateOpenWindow(page: Page): Promise<void> {
  await page.clock.runFor(SUSPENSE_REVEAL_HOLD_MS);
  await nextFrames(page);
}

// The surfaces a key opens over this page - the Create issue dialog, the shortcut sheet, the search
// bar and the Inbox peek - are chunks of their own, and the keys a reader types straight after the
// opening key are the surface's. On this page the page's own shortcuts would take them: a digit
// writes the issue's priority and `e` puts what follows into the issue's title
// (`web/src/features/issue/IssuePage.tsx`). So a surface whose chunk is in opens in the commit of
// its key, and while one is not in, a box from the entry holds the focus and the keyboard and
// hands what was typed to the surface when it lands (`web/src/features/shell/asked-surface.tsx`).
// Behind `lazy` each drew no sooner than 300 ms after its first render, its chunk in or not, and
// the page took the keys meanwhile.
// SLOW(>=2s): each row lands cold on an issue page, then brings in, or holds and releases, a
// chunk the page downloads on the wire before it reads the issue back from the server.
test.describe("keys typed straight after a key that opens a surface", () => {
  // The row brings the dialog's chunk in itself (`chunkIn`), so its claim - the keys reach the
  // dialog and not the page - holds whether the press draws the dialog or the box before it.
  test("c then 1e at once opens the create dialog with 1e in its title and writes nothing to the issue", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Keys after c" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      const patched = await patchesOf(page, issue.key);
      await openIssue(page, issue.key, "Keys after c");
      await chunkIn(page, "CreateIssueDialog");

      await page.keyboard.press("c");
      await page.keyboard.type("1e");
      const title = page
        .getByRole("dialog", { name: "Create issue" })
        .getByRole("textbox", { name: "Title" });
      await expect(title).toHaveValue("1e");
      await expect(title).toBeFocused();
      await afterAnyWrite(page);
      expect(patched()).toBe(0);
      expect(await keptWrites(page)).toBe(0);
      await expect
        .poll(() => getIssue(issue.key))
        .toMatchObject({ priority: null, title: "Keys after c" });
    } finally {
      await context.close();
    }
  });

  test("? then 1 at once opens the shortcut sheet with 1 in its filter and writes no priority", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Keys after ?" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      const patched = await patchesOf(page, issue.key);
      await openIssue(page, issue.key, "Keys after ?");
      await chunkIn(page, "ShortcutHelp");

      await page.keyboard.press("?");
      await page.keyboard.press("1");
      const filter = page
        .getByRole("dialog", { name: "Keyboard shortcuts" })
        .getByRole("searchbox", { name: "Filter shortcuts" });
      await expect(filter).toHaveValue("1");
      await afterAnyWrite(page);
      expect(patched()).toBe(0);
      expect(await keptWrites(page)).toBe(0);
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
    } finally {
      await context.close();
    }
  });

  // The bar's chunk is fetched after the page's first paint (`web/src/features/search/
  // SearchPaletteHost.tsx`), and the row brings it in itself, as the first row does the dialog's,
  // so its claim holds whether the press draws the bar or the box before it.
  test("$mod+k then 1 at once opens the search bar with 1 in its box and writes no priority", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Keys after mod k" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      const patched = await patchesOf(page, issue.key);
      await openIssue(page, issue.key, "Keys after mod k");
      await chunkIn(page, "SearchPalette");

      await page.keyboard.press("ControlOrMeta+k");
      await page.keyboard.press("1");
      const box = page
        .getByRole("dialog", { name: "Search" })
        .getByRole("combobox", { name: "Search" });
      await expect(box).toHaveValue("1");
      await expect(box).toBeFocused();
      await afterAnyWrite(page);
      expect(patched()).toBe(0);
      expect(await keptWrites(page)).toBe(0);
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
    } finally {
      await context.close();
    }
  });

  // The Create issue dialog's chunk and the shortcut sheet's are started by the page itself once
  // it has drawn (`web/src/features/issue/CreateIssueDialogHost.tsx`,
  // `web/src/features/shell/ShortcutHelpHost.tsx`), so a press of `c` or `?` after that meets the
  // surface in the commit of the press, with no box standing in for it
  // (`web/src/features/shell/asked-surface.tsx`). The rows above bring each chunk in themselves
  // (`chunkIn`), which leaves the surface's own load unrun, so they hold whichever way the press
  // goes; this one brings nothing in. It waits for the page's own download of each chunk and for
  // that module to have run (`ranChunk`), then reads, for each press, whether its surface is in
  // the document in a task the press's own keydown queues: React commits what a key's handler
  // sets before the browser runs another task, so a surface drawn in the commit of its press is
  // there, and one behind `lazy` and `Suspense` is not, since its first render suspends and React
  // shows that boundary's content no sooner than 300 ms after its fallback. And it keeps every box
  // the page adds from the press on: a press that draws its surface in its own commit adds none,
  // and one that draws the box adds it there.
  test("c and ? after the page's own download of their chunks open each surface in the commit of the press, with no box", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Prefetched surfaces" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await openIssue(page, issue.key, "Prefetched surfaces");
      await ranChunk(page, chunkFile("CreateIssueDialog"));
      await ranChunk(page, chunkFile("ShortcutHelp"));
      await page.evaluate(() => {
        const boxes: string[] = [];
        Reflect.set(window, "__loadingBoxes", boxes);
        const box = 'input[aria-label$=" (loading)"]';
        new MutationObserver((records) => {
          for (const record of records) {
            for (const node of record.addedNodes) {
              if (node instanceof Element && (node.matches(box) || node.querySelector(box))) {
                boxes.push(document.activeElement?.tagName ?? "");
              }
            }
          }
        }).observe(document.body, { childList: true, subtree: true });
        // The surface each key opens, by the name its dialog carries, and whether it was in the
        // document in the task the key's keydown queued. In the capture phase on the window, so
        // it hears the key before any handler of the page can stop it.
        const surfaces: Record<string, string> = { "?": "Keyboard shortcuts", c: "Create issue" };
        const inTheirTask: Record<string, boolean> = {};
        Reflect.set(window, "__surfacesInTheirTask", inTheirTask);
        window.addEventListener(
          "keydown",
          (event) => {
            const name = surfaces[event.key];
            if (name === undefined) return;
            setTimeout(() => {
              inTheirTask[event.key] =
                document.querySelector(`[role="dialog"][aria-label="${name}"]`) !== null;
            }, 0);
          },
          { capture: true }
        );
      });

      await page.keyboard.press("c");
      const dialog = page.getByRole("dialog", { name: "Create issue" });
      await expect(dialog).toBeVisible();
      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
      await page.keyboard.press("?");
      await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toBeVisible();
      expect(
        await page.evaluate(() => Reflect.get(window, "__loadingBoxes")),
        "a press drew the box that holds the keys, so its surface's chunk was not in"
      ).toEqual([]);
      expect(
        await page.evaluate(() => Reflect.get(window, "__surfacesInTheirTask")),
        "a press's surface was not in the document in the task its keydown queued"
      ).toEqual({ "?": true, c: true });
    } finally {
      await context.close();
    }
  });

  // The peek's chunk is not fetched in advance on an issue page (`web/src/features/inbox/
  // InboxHost.tsx` says why), so `i` here starts its download, which this row holds on the wire.
  test("i then 1 while the peek's chunk is on its way writes no priority, and Escape calls the peek off", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Keys after i" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await holdAsset(page, chunkFile("inbox-chunk"));
      const patched = await patchesOf(page, issue.key);
      await openIssue(page, issue.key, "Keys after i");

      await page.keyboard.press("i");
      await page.keyboard.press("1");
      await afterAnyWrite(page);
      expect(patched()).toBe(0);
      expect(await keptWrites(page)).toBe(0);
      await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
      const box = page.getByRole("textbox", { exact: true, name: "Inbox (loading)" });
      await expect(box).toBeFocused();
      await expect(box).toHaveValue("1");

      await page.keyboard.press("Escape");
      await expect(box).toHaveCount(0);
      expect(await onBody(page)).toBe(true);
    } finally {
      await context.close();
    }
  });

  test("c then 1e while the create dialog's chunk is on its way writes nothing, and the dialog opens with 1e in its title", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Held dialog" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      const { release } = await holdAssetUntilReleased(page, chunkFile("CreateIssueDialog"));
      const patched = await patchesOf(page, issue.key);
      await openIssue(page, issue.key, "Held dialog");

      await page.keyboard.press("c");
      await page.keyboard.type("1e");
      await afterAnyWrite(page);
      expect(patched()).toBe(0);
      expect(await keptWrites(page)).toBe(0);
      release();
      const title = page
        .getByRole("dialog", { name: "Create issue" })
        .getByRole("textbox", { name: "Title" });
      await expect(title).toHaveValue("1e");
      await expect(title).toBeFocused();
      await expect
        .poll(() => getIssue(issue.key))
        .toMatchObject({ priority: null, title: "Held dialog" });
    } finally {
      await context.close();
    }
  });

  // The page starts the shortcut sheet's chunk and the search bar's as soon as it has drawn
  // (`web/src/features/shell/ShortcutHelpHost.tsx`, `web/src/features/search/
  // SearchPaletteHost.tsx`), so each row holds that download on the wire, and the press finds the
  // chunk not in. The box that holds the keys is found by the name it carries for its surface,
  // "<name> (loading)" (`Placeholder` in `web/src/features/shell/asked-surface.tsx`).
  for (const surface of [
    {
      chunk: "ShortcutHelp",
      dialog: "Keyboard shortcuts",
      field: { name: "Filter shortcuts", role: "searchbox" },
      fieldWord: "filter",
      key: "?",
      keyWord: "?",
      loading: "Keyboard shortcuts (loading)",
      name: "shortcut sheet",
      noun: "sheet",
    },
    {
      chunk: "SearchPalette",
      dialog: "Search",
      field: { name: "Search", role: "combobox" },
      fieldWord: "box",
      key: "ControlOrMeta+k",
      keyWord: "$mod+k",
      loading: "Search (loading)",
      name: "search bar",
      noun: "bar",
    },
  ] as const) {
    test(`${surface.keyWord} then 1 while the ${surface.name}'s chunk is on its way writes no priority, and the ${surface.noun} opens with 1 in its ${surface.fieldWord}`, async ({
      browser,
    }) => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: `Held ${surface.noun}` });
      const context = await asUser(browser, "alice");
      try {
        const page = await context.newPage();
        await page.setViewportSize({ width: 1280, height: 900 });
        const { release } = await holdAssetUntilReleased(page, chunkFile(surface.chunk));
        const patched = await patchesOf(page, issue.key);
        await openIssue(page, issue.key, `Held ${surface.noun}`);

        await page.keyboard.press(surface.key);
        await page.keyboard.press("1");
        await afterAnyWrite(page);
        expect(patched()).toBe(0);
        expect(await keptWrites(page)).toBe(0);
        const box = page.getByRole("textbox", { exact: true, name: surface.loading });
        await expect(box).toBeFocused();
        await expect(box).toHaveValue("1");
        release();
        const field = page
          .getByRole("dialog", { name: surface.dialog })
          .getByRole(surface.field.role, { name: surface.field.name });
        await expect(field).toHaveValue("1");
        await expect(field).toBeFocused();
        await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
      } finally {
        await context.close();
      }
    });
  }

  // Tab and Shift+Tab stay in the box that holds the keys, as they stay in an open dialog
  // (`Placeholder` in `web/src/features/shell/asked-surface.tsx`). Letting Shift+Tab leave the box
  // would focus the page, blur the box to cancel the open, and let the next digit write the
  // issue's priority. Letting Tab leave the box, the shell's last control, would take focus out of
  // the document so the open stays while the reader's keys go elsewhere. The box is found by the
  // text it shows, so each row reads the keys' claim before the box's name.
  for (const key of ["Shift+Tab", "Tab"]) {
    test(`c, ${key} and 1 while the create dialog's chunk is on its way write no priority, and the dialog opens with 1 in its title`, async ({
      browser,
    }) => {
      await createProject({ key: "CORE", name: "Core" });
      const issue = await createIssue({ project: "CORE", title: `Held ${key}` });
      const context = await asUser(browser, "alice");
      try {
        const page = await context.newPage();
        await page.setViewportSize({ width: 1280, height: 900 });
        const { release } = await holdAssetUntilReleased(page, chunkFile("CreateIssueDialog"));
        const patched = await patchesOf(page, issue.key);
        await openIssue(page, issue.key, `Held ${key}`);

        await page.keyboard.press("c");
        const box = page.getByPlaceholder("Loading…");
        await expect(box).toBeFocused();
        await page.keyboard.press(key);
        await page.keyboard.press("1");
        await afterAnyWrite(page);
        expect(patched(), `${key} let the digit reach the page`).toBe(0);
        expect(await keptWrites(page)).toBe(0);
        await expect(box, `${key} took the focus out of the box`).toBeFocused();
        await expect(box).toHaveValue("1");
        await expect(box).toHaveAccessibleName("Create issue (loading)");
        await expect(box).toHaveAttribute("aria-busy", "true");

        release();
        const title = page
          .getByRole("dialog", { name: "Create issue" })
          .getByRole("textbox", { name: "Title" });
        await expect(title).toHaveValue("1");
        await expect(title).toBeFocused();
        await expect.poll(() => getIssue(issue.key)).toMatchObject({ priority: null });
      } finally {
        await context.close();
      }
    });
  }

  // The focus goes back to the control the open was pressed from as the chunk lands, and the page
  // does not scroll to it (`preventScroll`): a reader who scrolled while the box was up stays
  // where they scrolled to.
  test("a create dialog whose chunk lands after the reader scrolled leaves the page where they scrolled it", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Scrolled away" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 400 });
      const { release } = await holdAssetUntilReleased(page, chunkFile("CreateIssueDialog"));
      await openIssue(page, issue.key, "Scrolled away");
      const opener = page.getByTestId("issue-header").getByRole("button", { name: "Pin issue" });
      await opener.focus();

      await page.keyboard.press("c");
      await expect(page.getByPlaceholder("Loading…")).toBeFocused();
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      // What makes a focus on the opener scroll the page: the scroll took it out of view.
      await expect
        .poll(() => opener.evaluate((element) => element.getBoundingClientRect().bottom), {
          message: "the page did not scroll the opener out of view",
        })
        .toBeLessThan(0);
      const from = await page.evaluate(() => window.scrollY);

      release();
      await expect(
        page.getByRole("dialog", { name: "Create issue" }).getByRole("textbox", { name: "Title" })
      ).toBeFocused();
      // Two frames on, so a scroll the landing's focus started is read.
      await nextFrames(page);
      expect(await page.evaluate(() => window.scrollY), "the landing scrolled the page").toBe(from);
    } finally {
      await context.close();
    }
  });

  // An Escape in the box calls the open off, and the box gives the focus back to the control the
  // open was pressed from as it goes (`Placeholder` in `asked-surface.tsx`). The page does not
  // scroll to that control (`preventScroll`): a reader who scrolled while the box was up stays
  // where they scrolled to, as they do when the chunk lands (above).
  test("a create dialog whose open the reader called off by Escape after they scrolled leaves the page where they scrolled it", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Called off after a scroll" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 400 });
      await holdAsset(page, chunkFile("CreateIssueDialog"));
      await openIssue(page, issue.key, "Called off after a scroll");
      const opener = page.getByTestId("issue-header").getByRole("button", { name: "Pin issue" });
      await opener.focus();

      await page.keyboard.press("c");
      const box = page.getByPlaceholder("Loading…");
      await expect(box).toBeFocused();
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      // What makes a focus on the opener scroll the page: the scroll took it out of view.
      await expect
        .poll(() => opener.evaluate((element) => element.getBoundingClientRect().bottom), {
          message: "the page did not scroll the opener out of view",
        })
        .toBeLessThan(0);
      const from = await page.evaluate(() => window.scrollY);

      await page.keyboard.press("Escape");
      await expect(box, "Escape did not call the open off").toHaveCount(0);
      await expect(opener, "the box gave the focus back to no opener").toBeFocused();
      // Two frames on, so a scroll the box's focus return started is read.
      await nextFrames(page);
      expect(
        await page.evaluate(() => window.scrollY),
        "the box's focus return scrolled the page"
      ).toBe(from);
    } finally {
      await context.close();
    }
  });

  // A dialog that closes gives the focus back to the control it was opened from, and the page
  // does not scroll to it (`useDialog`, `preventScroll`): a reader who scrolled while the dialog
  // was up stays where they scrolled to.
  test("a create dialog closed after the reader scrolled leaves the page where they scrolled it", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Scrolled under the dialog" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 400 });
      await openIssue(page, issue.key, "Scrolled under the dialog");
      const opener = page.getByTestId("issue-header").getByRole("button", { name: "Pin issue" });
      await opener.focus();

      await page.keyboard.press("c");
      const dialog = page.getByRole("dialog", { name: "Create issue" });
      await expect(dialog.getByRole("textbox", { name: "Title" })).toBeFocused();
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      // What makes a focus on the opener scroll the page: the scroll took it out of view.
      await expect
        .poll(() => opener.evaluate((element) => element.getBoundingClientRect().bottom), {
          message: "the page did not scroll the opener out of view",
        })
        .toBeLessThan(0);
      const from = await page.evaluate(() => window.scrollY);

      await page.keyboard.press("Escape");
      await expect(dialog).toHaveCount(0);
      await expect(opener).toBeFocused();
      // Two frames on, so a scroll the close's focus started is read.
      await nextFrames(page);
      expect(await page.evaluate(() => window.scrollY), "the close scrolled the page").toBe(from);
    } finally {
      await context.close();
    }
  });

  // A dialog whose chunk lands after the reader has moved on opens nothing: neither on a page they
  // went back to, whose address moved under the box that held the open, nor over a field they went
  // on to after calling the open off. The late chunk takes no focus from where they are, even when
  // its download ends minutes later after an outage.
  //
  // The row goes back twice, because the two moves call the open off in two ways. Back from an
  // issue tab to the same issue's page, the shell leaves the focus where it is - it moves the focus
  // to `<main>` only for another issue or another top-level route (`pageIdentity`, `app.tsx`) - so
  // the box keeps the focus, and only its own close on an address change (`useCloseOnNavigation`
  // in `asked-surface.tsx`) calls the open off. Back from the issue to the Inbox, the shell moves
  // the focus to `<main>`, and the box's blur calls it off: both addresses carry the key `default`
  // (below), so its close on an address change sees no move there.
  test("a create dialog whose chunk lands after the reader went back a page opens nothing there", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Went back" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.clock.install();
      const { release } = await holdAssetUntilReleased(page, chunkFile("CreateIssueDialog"));
      await page.goto("/");
      await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
      await navigateInApp(page, `/issues/${issue.key}`);
      await expect(
        page.getByTestId("issue-header").getByRole("heading", { level: 1, name: "Went back" })
      ).toBeVisible();

      // The tab opens from its own button, as a reader opens it, so the router records the move
      // under a key of its own: the box's close compares location keys, and `navigateInApp`'s
      // bare `pushState` gives every address it makes the one key `default`.
      await page.getByRole("tab", { name: "Conversation" }).click();
      await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}/conversation$`, "u"));
      await page.locator("body").focus();
      await page.keyboard.press("c");
      const box = page.getByRole("textbox", { exact: true, name: "Create issue (loading)" });
      await expect(box).toBeFocused();
      await page.goBack();
      await expect(page).toHaveURL(new RegExp(`/issues/${issue.key}$`, "u"));
      await expect(box, "going back within the issue left the box holding the open").toHaveCount(0);

      await page.locator("body").focus();
      await page.keyboard.press("c");
      await page.goBack();
      await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible();
      release();
      await chunkIn(page, "CreateIssueDialog");
      await lateOpenWindow(page);
      await expect(page.getByRole("dialog", { name: "Create issue" })).toHaveCount(0);
      await expect(page.locator("main")).toBeFocused();
    } finally {
      await context.close();
    }
  });

  test("a create dialog whose chunk lands after Escape and a move to another field opens nothing", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Called off" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.clock.install();
      const { release } = await holdAssetUntilReleased(page, chunkFile("CreateIssueDialog"));
      await openIssue(page, issue.key, "Called off");

      await page.keyboard.press("c");
      await page.keyboard.press("Escape");
      const status = page.getByTestId("issue-header").getByRole("combobox", { name: "Status" });
      await status.focus();
      release();
      await chunkIn(page, "CreateIssueDialog");
      await lateOpenWindow(page);
      await expect(page.getByRole("dialog", { name: "Create issue" })).toHaveCount(0);
      await expect(status).toBeFocused();
    } finally {
      await context.close();
    }
  });

  // A create dialog whose chunk fails to download online draws the alert "Could not open Create
  // issue." in the box's place, and the alert holds the focus and the keys (`CouldNotOpen` in
  // `web/src/features/shell/asked-surface.tsx`). The tab's recovery reloads are spent, so no
  // reload follows (`web/src/features/shell/DeploymentResilience.tsx`): an open that closed with no
  // word would give the keys back to the page, and the next digit would write the issue's
  // priority. The alert's Close gives the focus back to the page the press came from, and the
  // next `c` asks for the chunk again. The row records the reloads as spent before the issue page
  // loads, so no failure of the chunk on that page finds a reload to spend. Every download of the
  // chunk fails, the page's own prefetch among them; the row waits for that prefetch first, so the
  // count after the second `c` is that press's alone.
  test("c whose chunk fails online with the tab's reloads spent draws an alert that holds the keys, so 1 writes no priority", async ({
    browser,
  }) => {
    await createProject({ key: "CORE", name: "Core" });
    const issue = await createIssue({ project: "CORE", title: "Keys after a failed c" });
    const context = await asUser(browser, "alice");
    try {
      const page = await context.newPage();
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.goto("/");
      await expect(page.getByText("Signed in as alice")).toBeVisible();
      const spent = await recordTabReloads(page, [3, 2, 1]);
      let asked = 0;
      await page.route(chunkFile("CreateIssueDialog"), (route) => {
        asked += 1;
        return route.abort();
      });
      const patched = await patchesOf(page, issue.key);
      await openIssue(page, issue.key, "Keys after a failed c");
      await expect
        .poll(() => asked, "the page's own prefetch of the chunk went out")
        .toBeGreaterThan(0);

      await page.keyboard.press("c");
      const alert = page.getByRole("alertdialog", { name: "Could not open Create issue." });
      await expect(alert, "the failed download drew no alert").toBeVisible();
      const close = alert.getByRole("button", { name: "Close" });
      await expect(close, "the alert took no focus").toBeFocused();
      await page.keyboard.type("1");
      await afterAnyWrite(page);
      expect(patched(), "a digit typed after the failure reached the page").toBe(0);
      expect(await keptWrites(page)).toBe(0);
      expect(await getIssue(issue.key)).toMatchObject({ priority: null });
      await expect(close).toBeFocused();
      expect(await tabReloads(page), "the failure spent a reload").toEqual(spent);

      await close.click();
      await expect(alert).toHaveCount(0);
      // The press came from the page itself (`openIssue`), so that is where the focus goes back.
      await expect
        .poll(() => onBody(page), "the alert gave the focus back to no opener")
        .toBe(true);
      const before = asked;
      await page.keyboard.press("c");
      await expect.poll(() => asked, "the next c asked for no chunk").toBeGreaterThan(before);
      await expect(alert).toBeVisible();
    } finally {
      await context.close();
    }
  });
});
