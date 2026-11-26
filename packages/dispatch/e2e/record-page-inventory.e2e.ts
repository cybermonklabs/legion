/** it records today's issue page as the server draws it for an issue it seeds through Dispatch,
 *  open and then closed through the API, so the fixture the new page's inventory row reads is
 *  the real stack's */

import { execFileSync } from "node:child_process";
import { mkdir, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";

import { issueTabs } from "../web/src/features/issue/issue-tab-list";
import { patchIssue } from "./api";
import { seedWholeIssueAdvancing } from "./issue-layout-seed";
import {
  INVENTORY_CONTROLS,
  type InventoryEntry,
  mergeInventory,
  recordInventory,
  recordScopeInventory,
} from "./page-inventory";
import { resetDatabase } from "./seed";
import { expect, type Page, test } from "./test";
import { asUser } from "./users";

/**
 * The recorder of what today's issue page offers (dispatch://INFRA-1196 part N's R7): every
 * landmark and every control of the whole issue under two priorities, one its own and one its
 * parent's (`seedWholeIssueAdvancing`), in four states - open and closed, at 1440x900 and at
 * 390x844 CSS pixels - written to `e2e/fixtures/issue-page-inventory.json`. The new page's own
 * row reads that fixture and asserts the new page still carries all of it
 * (`assertInventoryCarried`, "the new page carries every element of the old one"), so the
 * fixture is the ground truth and has to be recorded against today's page.
 *
 * **It is a recorder and no check of the page it runs against.** It reads the page through the
 * tabs today's page has (`#issue-<tab>-tab`), as `issueTabs` lists them in the checkout it runs
 * in. The fixture describes today's page at the branch's merge-base with `main`, because the
 * part's check `c-inventory` holds the fixture to that commit, and while the branch holds a tab
 * `main` lacks (dispatch://INFRA-5702's Dashboard) its own today's page is not that page. So the
 * recorder runs on today's page of the merge-base: a checkout at that commit, with this branch's
 * e2e files as the patch, and the new page's preference off; `test.skip` leaves it alone
 * everywhere else. Re-run it so after each merge of `main`, because `main` moves and the fixture
 * describes its page; `recordState` holds the tablist the page draws to `issueTabs`' list.
 *
 * Two things it deliberately is not, so neither is decided again:
 *
 * - It is **not** repointed at the new page. A fixture recorded from the new page would have the
 *   inventory row compare the new page against a recording of itself, which proves nothing.
 * - It is **not** retired in favour of the inventory row. Retiring it would leave nothing able
 *   to record the fixture the next time `main` moves.
 *
 * `RECORD_PAGE_INVENTORY=1` is what makes it run at all: without it the row skips, because the
 * page it reads is not the page the suite's other rows read. Any other value is refused rather
 * than read as "do not record", so a typed `RECORD_PAGE_INVENTORY=true` cannot silently skip the
 * recording a person asked for.
 *
 * The fixture names two commits: `commit` is the branch's merge-base with `main`
 * (`git merge-base HEAD origin/main`, which in that checkout is its own commit), whose today's
 * page the fixture describes and which `c-inventory` holds it to, and `recordedAtHead` is the
 * checkout's own commit (`git rev-parse HEAD`), under the patch that carries the seeds and this
 * spec. The page recorded is today's page only while it draws no `data-issue-page`, the new
 * page's mark, so each state asserts that: a recording that reached the new page fails rather
 * than recording it as the old ground truth.
 *
 * That page renders only the active tab's panel - every other panel is `hidden`, which the
 * accessibility snapshot leaves out - so a state's inventory is the merge of every tab's panel,
 * each visited in turn at each width, and each panel is asserted to offer a control of its
 * own. The priority row's picker is a view of its own too, because no tab holds it: the row's
 * add control opens it, and an open state records the page with it open (`recordPriorityPicker`).
 */

/** The tabs of the issue page, read from `web/src/features/issue/issue-tab-list.ts`'s `issueTabs`
 *  (`IssueTabs.tsx` renders that list) in the checkout the recorder runs in, so the list is that
 *  page's own. A tab's button and panel carry the DOM ids `Tabs.tsx`'s `tabButtonId` and
 *  `IssuePage.tsx` give them, which is how a tab is selected and its panel scoped here.
 *  `recordState` holds this list to the tablist the page draws, so a tab the page draws that the
 *  list lacks fails the recording instead of leaving its controls out of the fixture. */
const ISSUE_TABS: readonly string[] = issueTabs(0).map((tab) => tab.id);

interface RecordedState {
  /** Whether the issue is closed in this state. */
  readonly closed: boolean;
  /** The fixture key this state is written under. */
  readonly key: "closed-1440" | "closed-390" | "open-1440" | "open-390";
  readonly viewport: { readonly height: number; readonly width: number };
}

/** The four states, open before closed: the issue is closed once, between the two pairs. */
const STATES: readonly RecordedState[] = [
  { closed: false, key: "open-1440", viewport: { height: 900, width: 1440 } },
  { closed: false, key: "open-390", viewport: { height: 844, width: 390 } },
  { closed: true, key: "closed-1440", viewport: { height: 900, width: 1440 } },
  { closed: true, key: "closed-390", viewport: { height: 844, width: 390 } },
];

interface PageInventoryFixture {
  /** The commit whose page this describes: the branch's base. */
  readonly commit: string;
  /** The checkout's own commit when it was recorded, under the patch that carries the recorder. */
  readonly recordedAtHead: string;
  readonly recordedAt: string;
  readonly "open-1440": readonly InventoryEntry[];
  readonly "open-390": readonly InventoryEntry[];
  readonly "closed-1440": readonly InventoryEntry[];
  readonly "closed-390": readonly InventoryEntry[];
}

const fixturePath = fileURLToPath(new URL("./fixtures/issue-page-inventory.json", import.meta.url));

/** Whether this run records the fixture. The guard is read as an assertion: a value the spec does
 *  not understand fails the test instead of being taken for "skip". */
function recording(): boolean {
  const guard = process.env.RECORD_PAGE_INVENTORY ?? "";
  expect(
    ["", "1"],
    `RECORD_PAGE_INVENTORY is "${guard}": set it to 1 to record the fixture, or leave it unset to skip the recording`
  ).toContain(guard);
  return guard === "1";
}

/** `git` in the checkout this spec lives in, as one line. */
function git(...args: readonly string[]): string {
  return execFileSync("git", args, {
    cwd: fileURLToPath(new URL(".", import.meta.url)),
    encoding: "utf8",
  }).trim();
}

function resolves(ref: string): boolean {
  try {
    git("rev-parse", "--verify", "--quiet", `${ref}^{commit}`);
    return true;
  } catch {
    return false;
  }
}

const controlRoles: readonly string[] = INVENTORY_CONTROLS;

/** How the recorder's own report names one control. */
function named(entry: InventoryEntry): string {
  return `${entry.role} "${entry.name}"`;
}

/**
 * The priority row's picker (dispatch://INFRA-2947 R3), which the row's "Edit priorities" control
 * opens (`IssuePriorityLine.tsx`, `MultiSelect` in a portal) and no tab holds: in an open state
 * the page is recorded once more with it open, and it is closed by Escape. A closed issue's row
 * draws the control refused (`disabled`), so a closed state has no picker to open, and asserting
 * that is what keeps an open state whose control could not be pressed from recording no picker
 * without a word.
 */
async function recordPriorityPicker(page: Page, state: RecordedState): Promise<InventoryEntry[]> {
  const row = page.getByTestId("issue-advances");
  const edit = row.getByRole("button", { exact: true, name: "Edit priorities" });
  if (state.closed) {
    await expect(
      edit,
      `the closed issue's priority row of ${state.key} refuses its edit control`
    ).toBeDisabled();
    return [];
  }
  await edit.click();
  const picker = page.getByRole("combobox", { exact: true, name: "Priority to advance" });
  await expect(picker).toBeVisible();
  const picked = await recordInventory(page);
  await page.keyboard.press("Escape");
  await expect(picker, `Escape left the priority picker of ${state.key} open`).toHaveCount(0);
  return picked;
}

/**
 * One state's inventory: the page at that width, in that open or closed state, with every tab
 * visited in turn, then the priority row's picker opened, and what each offers merged into one
 * list. The tablist the page draws is read first and held to `ISSUE_TABS`, so a tab the page
 * draws and `issueTabs` does not list fails the recording rather than leaving that panel's
 * controls out of the fixture. Each panel is then asserted to offer a control of its own (or,
 * for `steps`, which on a seed with no Legion runs draws only its `EmptyState` region, at least
 * a landmark or control), so a tab whose panel never rendered fails here rather than being
 * counted as covered. It also prints, for each panel, an entry no other panel of that state
 * offers - the evidence that the state covers every tab - falling back on its first entry when
 * every one of them is offered elsewhere too.
 */
async function recordState(
  page: Page,
  issueKey: string,
  state: RecordedState
): Promise<InventoryEntry[]> {
  await page.setViewportSize(state.viewport);
  await page.goto(`/issues/${issueKey}/spec`, { waitUntil: "networkidle" });
  await expect(page.getByTestId("issue-header")).toBeVisible();
  // Today's page and not the new one: the new page's root carries `data-issue-page`, and the
  // preference that draws it is off for this login, so a recording that reached it would record
  // the new page as the old ground truth.
  await expect(
    page.locator("[data-issue-page]"),
    `the page at ${state.key} draws [data-issue-page], so it is the new page and not today's`
  ).toHaveCount(0);
  const drawn = await page
    .getByTestId("issue-tabs")
    .getByRole("tab")
    .evaluateAll((nodes) => nodes.map((node) => node.id.slice("issue-".length, -"-tab".length)));
  expect(
    drawn,
    `the page at ${state.key} draws other tabs than this recorder reads, so the fixture would miss a panel`
  ).toEqual([...ISSUE_TABS]);
  const found: InventoryEntry[] = [];
  const panels = new Map<string, readonly InventoryEntry[]>();
  for (const tab of ISSUE_TABS) {
    const button = page.locator(`#issue-${tab}-tab`);
    await button.click();
    await expect(button).toHaveAttribute("aria-selected", "true");
    found.push(...(await recordInventory(page)));
    const panel = await recordScopeInventory(page.locator(`#issue-${tab}-panel`));
    const controls = panel.filter((entry) => controlRoles.includes(entry.role));
    const covered = tab === "steps" ? panel : controls;
    expect(
      covered[0],
      `the ${tab} panel of ${state.key} offers ${tab === "steps" ? "no landmark or control" : "no control"} at all, so the state covers none of it`
    ).toBeDefined();
    panels.set(tab, covered);
  }
  found.push(...(await recordPriorityPicker(page, state)));
  for (const [tab, controls] of panels) {
    const elsewhere = new Set(
      [...panels].filter(([other]) => other !== tab).flatMap(([, entries]) => entries.map(named))
    );
    const own = controls.find((entry) => !elsewhere.has(named(entry)));
    const shown = own ?? controls[0];
    console.log(
      `${state.key} ${tab}: ${controls.length} controls, ${shown === undefined ? "none" : named(shown)}${own === undefined ? " (shared)" : ""}`
    );
  }
  return mergeInventory(found);
}

test("the issue page's landmarks and controls, open and closed, at 1440 and at 390", async ({
  browser,
}) => {
  // A recorder, not a check: it reads today's page through its tabs, so it runs only when a
  // person asks it to record (the header above).
  test.skip(!recording(), "set RECORD_PAGE_INVENTORY=1 to record today's page");
  test.setTimeout(600_000);
  await resetDatabase();
  // The fixture describes today's page at the branch's merge-base with `main`, which is the
  // checkout the recorder runs in (the header says why); each state asserts it read today's page
  // and not the new one (`recordState`).
  const commit = git("merge-base", "HEAD", resolves("origin/main") ? "origin/main" : "main");
  const recordedAtHead = git("rev-parse", "HEAD");
  expect(commit, "git merge-base answered no commit").toMatch(/^[0-9a-f]{40}$/);
  expect(recordedAtHead, "git rev-parse HEAD answered no commit").toMatch(/^[0-9a-f]{40}$/);
  const seeded = await seedWholeIssueAdvancing();

  const alice = await asUser(browser, "alice");
  const recorded = new Map<RecordedState["key"], InventoryEntry[]>();
  try {
    const page = await alice.newPage();
    // The issue is closed once, between the two open states and the two closed ones: a second
    // close is refused (`ISSUE_CLOSED`).
    let closed = false;
    for (const state of STATES) {
      if (state.closed && !closed) {
        await patchIssue(seeded.key, { status: "done" });
        closed = true;
      }
      recorded.set(state.key, await recordState(page, seeded.key, state));
    }
  } finally {
    await alice.close();
  }

  const fixture: PageInventoryFixture = {
    commit,
    recordedAtHead,
    recordedAt: new Date().toISOString(),
    "open-1440": recorded.get("open-1440") ?? [],
    "open-390": recorded.get("open-390") ?? [],
    "closed-1440": recorded.get("closed-1440") ?? [],
    "closed-390": recorded.get("closed-390") ?? [],
  };
  await mkdir(dirname(fixturePath), { recursive: true });
  await writeFile(fixturePath, `${JSON.stringify(fixture, null, 2)}\n`);
});
