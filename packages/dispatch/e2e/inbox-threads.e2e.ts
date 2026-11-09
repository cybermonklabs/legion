import { expect, type Locator, test } from "@playwright/test";

import { setLiveSessions } from "./agents";
import { createIssue, createProject } from "./api";
import { hasReadingPane, inboxPane } from "./inbox-pane";
import { replyRowId, seedCommentThread, seedTwelve, THREAD_SESSION } from "./inbox-threads";
import { resetDatabase } from "./seed";
import { asUser } from "./users";

/**
 * The two rows of the Inbox's thread family that need the real stack, each with the reason it
 * is not a stubbed row above it. The rest - the old Replies address and the Type filter over
 * the one list - is in `inbox-threads.stub.e2e.ts`.
 *
 * Everything here is made up: this repository is public, so nothing names a real person, host or
 * project.
 */

test.beforeEach(async () => {
  await resetDatabase();
});

// It is no stubbed row: it opens the Inbox and then navigates to another page while the shell's
// `GET /api/v1/agents` is still in flight, so a recording of it holds that read cancelled, with
// no body. `routeFromHAR` serves the first entry that matches a URL, so every replay is given
// the empty one and the Agents page draws "Could not load agents" instead of its heading.
test("the drawer's own bar filters the drawer alone, and the Inbox page keeps its list", async ({
  browser,
}) => {
  await seedTwelve();
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    // The Inbox page, narrowed to its Reply rows: the list the drawer must not touch.
    await page.goto("/?type=reply");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(4);

    // The drawer is offered on every page but the Inbox itself, so the reader opens it with `i`
    // from a page of its own.
    await page.goto("/agents");
    await expect(page.getByRole("heading", { name: "Agents" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("i");
    const drawer = page.getByTestId("inbox-drawer");
    await expect(drawer).toBeVisible();
    const drawerRows = drawer.locator("[data-inbox-row]");
    await expect(drawerRows).toHaveCount(12);

    await drawer.locator("[data-inbox-filter]").click();
    await drawer.getByRole("button", { name: "Filter by type" }).click();
    const options = page.getByRole("listbox", { name: "Type options" });
    await options.getByRole("option", { name: /^Agent message, / }).click();
    await expect(drawerRows).toHaveCount(3);
    // The drawer's pick is its own: the address under it never moved.
    await expect(page).toHaveURL(/\/agents$/);

    await page.keyboard.press("Escape");
    await drawer.getByRole("button", { name: "Close" }).click();
    await expect(drawer).toHaveCount(0);
    await page.goto("/?type=reply");
    await expect(page.locator("[data-inbox-row]")).toHaveCount(4);
    await expect(page.locator('[data-inbox-filter-token="type:agent"]')).toHaveCount(0);
  } finally {
    await alice.close();
  }
});

// ---------------------------------------------------------------------------
// A3: a long reply, shown whole.
// ---------------------------------------------------------------------------

/** Forty lines holding a list, a fenced code block and a table: a reply no surface may clamp. */
const LONG_REPLY = [
  "Here is the whole rollout note, in full.",
  "",
  ...Array.from({ length: 10 }, (_, index) => `- Step ${index + 1} of the mirror replay.`),
  "",
  "```sh",
  ...Array.from({ length: 6 }, (_, index) => `dispatch replay --window ${index + 1}h`),
  "```",
  "",
  "| Window | Attempts |",
  "| --- | --- |",
  ...Array.from({ length: 6 }, (_, index) => `| ${index + 1}h | ${index + 2} |`),
  "",
  ...Array.from({ length: 7 }, (_, index) => `Closing note ${index + 1} on the replay budget.`),
  "",
  "The last line of the reply, which has to be reachable.",
].join("\n");

const LAST_LINE = "The last line of the reply, which has to be reachable.";

/**
 * Whether any element of the drawn message is clamped: a `line-clamp-*` class, or a box that
 * hides what does not fit in it. An element whose overflow is visible hides nothing - an
 * inline link's own hit-box padding (`.dispatch-inline-link`, 6px above and below a 16px line
 * box) paints past its line and cuts no word - so only a box that clips is a fault, and each
 * one names its classes so the clamp can be found.
 */
async function clampedParts(turn: Locator): Promise<string[]> {
  return turn.evaluate((node) => {
    const faults: string[] = [];
    for (const element of [node, ...node.querySelectorAll("*")]) {
      if (!(element instanceof HTMLElement)) continue;
      if (/\bline-clamp-/.test(element.className)) faults.push(`clamped: ${element.tagName}`);
      const clips = getComputedStyle(element).overflowY !== "visible";
      if (clips && element.scrollHeight > element.clientHeight + 1) {
        faults.push(
          `cut: ${element.tagName}.${element.className} ${element.scrollHeight} > ${element.clientHeight}`
        );
      }
    }
    return faults;
  });
}

// It is no stubbed row either: it carries `@phone`, and the `iphone` project runs none.
test("a long reply shows whole in the reading pane and on a card @phone", async ({ browser }) => {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(issue.key, "How far does the replay go?", LONG_REPLY);

  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto("/");
    const row = page.locator(`[data-inbox-row="${replyRowId(thread)}"]`);
    await expect(row).toBeVisible();
    // The pane shows the thread of the selected row; below its width the row is its own card.
    const body = hasReadingPane(page)
      ? inboxPane(page).locator("[data-inbox-thread-body]")
      : page.locator(`[data-inbox-row="${replyRowId(thread)}"] [data-inbox-thread-body]`);
    if (hasReadingPane(page)) {
      await row.locator("time").first().click();
      await expect(row).toHaveAttribute("data-inbox-selected", "true");
    }
    await expect(body).toBeVisible();

    // The reply renders as what it is: a list, a code block and a table.
    const turn = body.locator("[data-inbox-thread-turn]").last();
    await expect(turn.locator("li")).toHaveCount(10);
    await expect(turn.locator("pre")).toHaveCount(1);
    await expect(turn.locator("table")).toHaveCount(1);
    await expect(turn.locator("table td")).toHaveCount(12);

    // Its last line is reachable by scrolling, and nothing of it is clamped or cut.
    const last = turn.getByText(LAST_LINE);
    await last.scrollIntoViewIfNeeded();
    await expect(last).toBeInViewport();
    expect(await clampedParts(turn)).toEqual([]);
  } finally {
    await alice.close();
  }
});
