/** Dashboard on the new issue page, with the reader's setting on: what the server decides for
 *  each of its parts and for the header's count is what these rows read - which open asks wait on
 *  the person and whose turn each is, which decisions wait for a review, which approval a later
 *  version made stale, which threads hold a write the person has not read, the issue's Legion
 *  phase, rounds and claims, and how each check stands at the pull request's head - with the
 *  addresses that open Dashboard and Conversation, the reads Dashboard and Details send, and what
 *  each act in place writes. */

// The DOM contract these rows read, which the Dashboard tasks build (dispatch://INFRA-5702). For
// Dashboard itself it is the contract `e2e/issue-dashboard-today.e2e.ts` states, drawn inside the
// new page's root `[data-issue-page]` rather than in today's tab panel:
//
// - Dashboard is the first control of the view row (`presetRow` of `e2e/issue-page.ts`). From
//   1280 px up it is the first toggle, whose `[data-preset-name]` reads "Dashboard" and which
//   carries `aria-pressed="true"` while Dashboard is on. Below 1280 px it is the first
//   `menuitemradio` of the one control's menu, and the one control's `[data-preset-name]` reads
//   "Dashboard" while Dashboard is on.
// - Its parts are the `region`s "Phase", "Waiting on you", "Progress" and "Agents at work", as the
//   today file's contract draws them: the count `dashboard-waiting-count`, each item
//   `dashboard-item-<kind>-<id>`, the button "Show the next five", the ask body `[data-ask-body]`
//   and the decision card `[data-decision-card]`. The ask body of a question asked at urgency
//   `high` draws that urgency's word ("High") beside its kind word, on one line.
// - Dashboard draws no outline (no `tree` "Issue tree" and no Contents pane), none of the labels
//   of the issue's three sentences, and no children's section (`issue-children`).
// - The header's count of what waits is `issue-whose-turn` (`waitingCount` of
//   `e2e/issue-page-p.ts`), which reads "Waiting on you (N)".
// - An ask item takes the page's focus when the focus goes inside it. Details then holds
//   `[data-testid="focused-node-facts"]`, whose `data-focused-node` is the ask's id, and inside it
//   `section[data-node-references]` with the `button` that opens what references the ask. These
//   are the names part M's `FocusedNodeFacts.tsx` and `node-references/NodeReferences.tsx` give
//   them (dispatch://INFRA-3948), so the Details row runs only where the head holds part M's file
//   (c-dashboard-new (4)); `features/dashboard/`'s unit tests read that the item sets the focus.

import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

import { URGENCY_LABELS } from "../web/src/features/inbox/ask-urgency";
import {
  CONTROL_LABELS,
  NODE_TYPE_NAMES,
  PRESET_NAMES,
} from "../web/src/features/issue-page/page-words";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { getComment, getIssue } from "./api";
import {
  type DashboardSeed,
  type MainHolds,
  mainHolds,
  ROUNDS,
  SEEDED_LEGION,
  seedDashboard,
  type WaitingItem,
} from "./dashboard-seed";
import {
  contentsPane,
  gotoIssue,
  issuePage,
  marginShell,
  openMenu,
  phase,
  presetChooser,
  presetRow,
  presetToggles,
  viewportAt,
} from "./issue-page";
import { blockNode, expectOpeningLanded, focusThePage, nodeRow, tree } from "./issue-page-g";
import { childrenSection, focusedNodeId } from "./issue-page-i";
import { expectHeaderDrawn, waitingCount } from "./issue-page-p";
import { decisionsBody } from "./issue-page-q";
import { expect, type Locator, type Page, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/** Whether the head holds part M's Details facts of a focused node (dispatch://INFRA-3948), which
 *  the Details row reads; c-dashboard-new (4) gates that row on the file, not on part M's merge. */
const PART_M_DETAILS = existsSync(
  fileURLToPath(new URL("../web/src/features/issue-page/FocusedNodeFacts.tsx", import.meta.url))
);

/** The view row's word for Dashboard, which `PRESET_NAMES` holds once Dashboard is a preset. */
const DASHBOARD = "Dashboard";

/** The widths the rows read, and the width from which the view row is a toolbar of toggles
 *  rather than one control with a menu (`IssuePresets.tsx`). */
const DESKTOP_WIDTH = 1440;
const PHONE_WIDTH = 390;
const TOOLBAR_FROM = 1280;

/** The most requests under `/api/` a cold load of Dashboard sends (c-dashboard-new (6)). */
const COLD_LOAD_AT_MOST = 40;

/** The eight steps of the track, in order (R3). */
const STEPS = [
  "Admitted",
  "Planning",
  "Implementing",
  "Testing",
  "Reviewing",
  "Accepting",
  "Retro",
  "Merging",
] as const;

/** The rounds each counted step carries: the seed's handoffs of its phase (R3). */
const STEP_ROUNDS: Readonly<Partial<Record<(typeof STEPS)[number], number>>> = {
  Implementing: ROUNDS.implement,
  Planning: ROUNDS.plan,
  Reviewing: ROUNDS.review,
  Testing: ROUNDS.test,
};

/** The role words of each claim state (R7): any other state reads "in another state", and a
 *  `retired` claim is not listed. */
const ROLE_WORDS: Readonly<Record<string, string>> = {
  failed: "failed",
  idle: "idle",
  launch_uncertain: "starting",
  launching: "starting",
  queued: "starting",
  ready: "idle",
  registered: "idle",
  shim_connected: "starting",
  suspended: "paused",
  working: "in a turn",
};

const TILES = {
  children: "Children done",
  met: "Statements met",
  passed: "Checks passed",
  questions: "Questions waiting on others",
} as const;

/** Which main this run's server is, read once per worker (R27). */
let holds: MainHolds | undefined;

test.beforeAll(async () => {
  holds = await mainHolds();
});

function thisMain(): MainHolds {
  if (holds === undefined) throw new Error("the file's beforeAll read no main");
  return holds;
}

/** The Dashboard seed on the main this run's server is. */
function seeded(): Promise<DashboardSeed> {
  return seedDashboard(thisMain());
}

/** `text` as a pattern that matches it literally. */
function literal(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** The own address of `view` on `key`: `/issues/<KEY>/<view>`. */
function viewPath(key: string, view: string): string {
  return `/issues/${key}/${view}`;
}

/** The page's address is `view`'s own, with nothing after it. */
function atView(key: string, view: string): RegExp {
  return new RegExp(`/issues/${literal(key)}/${view}$`);
}

function waitingOf(page: Page): Locator {
  return issuePage(page).getByRole("region", { exact: true, name: "Waiting on you" });
}

function itemsOf(waiting: Locator): Locator {
  return waiting.locator('[data-testid^="dashboard-item-"]');
}

function testIdOf(item: WaitingItem): string {
  return `dashboard-item-${item.kind}-${item.id}`;
}

function itemOf(waiting: Locator, item: WaitingItem): Locator {
  return waiting.getByTestId(testIdOf(item));
}

/** The test id of each item drawn, in the order drawn. */
function drawnItems(waiting: Locator): Promise<string[]> {
  return itemsOf(waiting).evaluateAll((items) =>
    items.map((item) => item.getAttribute("data-testid") ?? "")
  );
}

function countOf(waiting: Locator): Locator {
  return waiting.getByTestId("dashboard-waiting-count");
}

function nextFive(waiting: Locator): Locator {
  return waiting.getByRole("button", { name: /^Show the next five/ });
}

/** Draws every item of the list: one press of the control for each five past the first. */
async function drawEveryItem(waiting: Locator, seed: DashboardSeed): Promise<void> {
  await expect(countOf(waiting)).toHaveText(String(seed.items.length));
  for (let drawn = 5; drawn < seed.items.length; drawn += 5) await nextFive(waiting).click();
  await expect(itemsOf(waiting)).toHaveCount(seed.items.length);
}

/** The name of the view the row has on: its pressed toggle's from 1280 px up, and the one
 *  control's below. */
function viewOn(page: Page): Locator {
  return presetRow(page).locator(
    'button[aria-pressed="true"] [data-preset-name], button[aria-haspopup="menu"] [data-preset-name]'
  );
}

/** Fails unless the page has Dashboard on and draws it, and draws no outline beside it. */
async function expectDashboard(page: Page, where: string): Promise<void> {
  await expect(viewOn(page), `${where} does not have Dashboard on`).toHaveText(DASHBOARD);
  await expect(waitingOf(page), `${where} draws no Dashboard`).toBeVisible();
  await expect(tree(page), `${where} draws the outline with Dashboard`).toHaveCount(0);
}

/** Fails unless the page has Conversation on and draws the outline, and no Dashboard. */
async function expectConversation(page: Page, where: string): Promise<void> {
  await expect(viewOn(page), `${where} does not have Conversation on`).toHaveText(
    PRESET_NAMES.conversation
  );
  await expect(tree(page), `${where} draws no outline`).toHaveCount(1);
  await expect(waitingOf(page), `${where} draws Dashboard`).toHaveCount(0);
}

/** Fails unless Dashboard is the first control of the view row, in the row's shape at `width`:
 *  the first toggle from 1280 px up, and the first item of the one control's menu below. */
async function expectDashboardFirst(page: Page, width: number): Promise<void> {
  if (width >= TOOLBAR_FROM) {
    await expect(
      presetToggles(page).first().locator("[data-preset-name]"),
      `the view row's first control is not Dashboard at ${width} px`
    ).toHaveText(DASHBOARD);
    return;
  }
  await expect(presetChooser(page), `the view row draws no one control at ${width} px`).toHaveCount(
    1
  );
  await presetChooser(page).click();
  await expect(
    openMenu(page).getByRole("menuitemradio").first(),
    `the view row's first item is not Dashboard at ${width} px`
  ).toHaveAccessibleName(new RegExp(`^${DASHBOARD}( |$)`));
  await page.keyboard.press("Escape");
  await expect(openMenu(page)).toHaveCount(0);
}

/** Presses the control of the view named `name`, in the row's shape at `width`: its toggle from
 *  1280 px up, and its item of the one control's menu below. */
async function pressView(page: Page, name: string, width: number): Promise<void> {
  if (width >= TOOLBAR_FROM) {
    const toggle = presetRow(page).getByRole("button", { exact: true, name });
    await expect(toggle, `the view row draws no ${name} control at ${width} px`).toHaveCount(1);
    await toggle.click();
    return;
  }
  await expect(presetChooser(page), `the view row draws no one control at ${width} px`).toHaveCount(
    1
  );
  await presetChooser(page).click();
  const item = openMenu(page).getByRole("menuitemradio", { name: new RegExp(`^${name}( |$)`) });
  await expect(item, `the view row's menu holds no ${name} at ${width} px`).toHaveCount(1);
  await item.click();
}

/** Presses the chord `t` and then `second`, with the focus on the page itself. */
async function chord(page: Page, second: "c" | "w"): Promise<void> {
  await focusThePage(page);
  await page.keyboard.press("t");
  await page.keyboard.press(second);
}

/** The slug of each document of `key`, by the document's id. */
async function slugsOf(key: string): Promise<ReadonlyMap<string, string>> {
  return new Map(
    ((await getIssue(key)).artifacts ?? []).map((artifact) => [artifact.id, artifact.slug])
  );
}

/** Where the first whole-word use of `first` and of `second` stand in the visible text of
 *  `scope`: whether each is drawn, whether the two share a line, and whether they stand side by
 *  side, no more than a small gap apart. */
function wordsBeside(
  scope: Locator,
  first: string,
  second: string
): Promise<{ beside: boolean; drawn: boolean[]; sameLine: boolean }> {
  return scope.evaluate(
    (root, words) => {
      const boxes = words.map((word) => {
        const pattern = new RegExp(`(^|[^\\p{L}\\p{N}])(${word})(?![\\p{L}\\p{N}])`, "iu");
        const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
        for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
          const parent = node.parentElement;
          const match = pattern.exec(node.textContent ?? "");
          if (parent === null || match === null || !parent.checkVisibility()) continue;
          const start = match.index + (match[1] ?? "").length;
          const range = document.createRange();
          range.setStart(node, start);
          range.setEnd(node, start + word.length);
          return range.getBoundingClientRect();
        }
        return null;
      });
      const [one, two] = boxes;
      const drawn = boxes.map((box) => box !== null);
      if (one == null || two == null) return { beside: false, drawn, sameLine: false };
      const gap = Math.max(one.left, two.left) - Math.min(one.right, two.right);
      return { beside: gap <= 24, drawn, sameLine: one.top < two.bottom && two.top < one.bottom };
    },
    [first, second]
  );
}

// SLOW(>=2s): the whole Dashboard seed - an issue with seven children, six handoffs, five checks
// and four results read through to the fake GitHub, six questions, five decisions, two approvals
// and three threads, each written through its route - before one load of the bare address.
test("Dashboard is the first tab and each of its numbers is the seed's", async ({ browser }) => {
  await phase(browser, seeded, async (page, seed) => {
    await gotoIssue(page, `/issues/${seed.key}`);
    await expectDashboardFirst(page, DESKTOP_WIDTH);
    await expectDashboard(page, "the bare address");

    // What waits: the count, the newest five, a control that says how many are left, then each
    // item once, newest first.
    const waiting = waitingOf(page);
    await expect(countOf(waiting)).toHaveText(String(seed.items.length));
    await expect
      .poll(() => drawnItems(waiting), { message: "the first five are not the newest five" })
      .toEqual(seed.items.slice(0, 5).map(testIdOf));
    await expect(nextFive(waiting)).toContainText(String(seed.items.length - 5));
    await nextFive(waiting).click();
    await expect
      .poll(() => drawnItems(waiting), { message: "the list is not each item once, newest first" })
      .toEqual(seed.items.map(testIdOf));
    await expect(nextFive(waiting)).toHaveCount(0);

    // The Legion daemon's thread is no item; the open ask stands once, in the questions part; the
    // spec's stale approval folds into its request's item, which names the version it asks for.
    await expect(itemOf(waiting, { id: seed.threads.daemon, kind: "thread" })).toHaveCount(0);
    await expect(itemOf(waiting, { id: seed.asks.replied, kind: "thread" })).toHaveCount(0);
    await expect(itemOf(waiting, { id: seed.specArtifactId, kind: "stale" })).toHaveCount(0);
    await expect(itemOf(waiting, { id: seed.verificationArtifactId, kind: "stale" })).toHaveCount(
      1
    );
    const request = itemOf(waiting, { id: seed.asks.approval.id, kind: "approval" });
    await expect(request.locator("[data-ask-body]")).toHaveCount(1);
    await expect(request).toContainText(new RegExp(`\\b${seed.asks.approval.version}\\b`));
    for (const item of seed.items.filter((one) => one.kind === "question")) {
      await expect(itemOf(waiting, item).locator("[data-ask-body]")).toHaveCount(1);
    }
    for (const item of seed.items.filter((one) => one.kind === "decision")) {
      const card = itemOf(waiting, item).locator("[data-decision-card]");
      await expect(card).toHaveCount(1);
      await expect(card).toContainText(seed.decisions.author);
    }

    // The question asked at urgency high draws that urgency's word beside its kind.
    const urgent = itemOf(waiting, { id: seed.asks.urgent, kind: "question" }).locator(
      "[data-ask-body]"
    );
    await expect(urgent).toContainText(URGENCY_LABELS.high);
    expect(
      await wordsBeside(urgent, NODE_TYPE_NAMES.ask, URGENCY_LABELS.high),
      `the urgent question does not draw "${URGENCY_LABELS.high}" beside its kind`
    ).toEqual({ beside: true, drawn: [true, true], sameLine: true });

    // The track marks Planning, and the counted steps carry the seed's rounds.
    const track = issuePage(page).getByRole("region", { exact: true, name: "Phase" });
    await expect(track, "Dashboard draws no phase track").toBeVisible();
    await expect(track.getByRole("listitem")).toHaveText(
      STEPS.map((step) => {
        const rounds = STEP_ROUNDS[step] ?? 0;
        return new RegExp(rounds === 0 ? `^${step}$` : `^${step}\\s*${rounds}$`);
      })
    );
    await expect(track.locator('[aria-current="step"]')).toHaveText(/^Planning\b/);
    await expect(track.getByRole("button")).toHaveCount(0);

    // Each tile counts what the seed made it count, of the whole the seed made.
    const progress = issuePage(page).getByRole("region", { exact: true, name: "Progress" });
    await expect(progress, "Dashboard draws no tiles").toBeVisible();
    for (const [tile, name] of Object.entries(TILES) as [keyof typeof TILES, string][]) {
      const meter = progress
        .getByRole("button", { name: new RegExp(`^${name}`) })
        .getByRole("meter");
      await expect(meter, `the ${name} tile`).toHaveAttribute(
        "aria-valuenow",
        String(seed.tiles[tile].ids.length)
      );
      await expect(meter, `the ${name} tile`).toHaveAttribute(
        "aria-valuemax",
        String(seed.tiles[tile].whole)
      );
    }

    // The agents at work are the seeded claims, in the order the Legion state gives, each in its
    // role words.
    const agents = issuePage(page).getByRole("region", { exact: true, name: "Agents at work" });
    await expect(agents, "Dashboard draws no agents at work").toBeVisible();
    await expect(agents.getByRole("listitem")).toHaveText(
      SEEDED_LEGION.roles
        .filter((role) => role.state !== "retired")
        .map(
          (role) =>
            new RegExp(
              `^${literal(role.role)}\\b[\\s\\S]*\\b${ROLE_WORDS[role.state] ?? "in another state"}$`,
              "i"
            )
        )
    );

    // Dashboard draws no outline, none of the issue's three sentences and no children's section.
    await expect(tree(page), "Dashboard draws the outline").toHaveCount(0);
    await expect(contentsPane(page), "Dashboard draws the Contents pane").toHaveCount(0);
    for (const label of [
      CONTROL_LABELS.in_short,
      CONTROL_LABELS.in_short_what,
      CONTROL_LABELS.in_short_why,
      CONTROL_LABELS.in_short_done,
    ]) {
      await expect(
        issuePage(page).getByText(label, { exact: true }),
        `Dashboard draws the three sentences' "${label}"`
      ).toHaveCount(0);
    }
    await expect(childrenSection(page), "Dashboard draws a children's section").toHaveCount(0);
    await expect(
      issuePage(page).locator('[data-node-type="child_issue"]'),
      "Dashboard draws the children as rows"
    ).toHaveCount(0);
  });
});

// The view row is one control with a menu below 1280 px, and that menu is what a phone presses,
// so the row reads both shapes: the toolbar at 1440 px and the menu at 390 px, which the `iphone`
// project reads with its touch screen and its own viewport.
// SLOW(>=2s): the whole Dashboard seed, then at each width four loads of Dashboard's and
// Conversation's addresses, six presses of the view row and two chords, and at the first width
// five loads of addresses that name a node, each waited on until the named row holds the focus.
test("the bare address opens Dashboard, and a bare address naming a node opens Conversation at it @phone", async ({
  browser,
}) => {
  test.setTimeout(240_000);
  await phase(browser, seeded, async (page, seed) => {
    const key = seed.key;
    const widths =
      test.info().project.name === "iphone" ? [PHONE_WIDTH] : [DESKTOP_WIDTH, PHONE_WIDTH];
    for (const width of widths) {
      await page.setViewportSize(viewportAt(width));
      const at = `at ${width} px`;

      // The bare address and `/dashboard` draw Dashboard, the view row's first control.
      await gotoIssue(page, `/issues/${key}`);
      await expectDashboard(page, `the bare address ${at}`);
      await expectDashboardFirst(page, width);
      await gotoIssue(page, viewPath(key, "dashboard"));
      await expectDashboard(page, `/dashboard ${at}`);

      // `/conversation` draws Conversation, and the Dashboard control draws Dashboard.
      await gotoIssue(page, viewPath(key, "conversation"));
      await expectConversation(page, `/conversation ${at}`);
      await pressView(page, DASHBOARD, width);
      await expect(page).toHaveURL(atView(key, "dashboard"));
      await expectDashboard(page, `the Dashboard control ${at}`);

      // From 1280 px up, pressing Dashboard while it is on draws Conversation, as pressing each
      // view that is on turns it off. The one control's menu keeps the view that is on when its
      // item is chosen (`IssuePresets.tsx`, read by `e2e/issue-page-d.e2e.ts`), so below 1280 px
      // the Conversation item is the press that leaves Dashboard.
      if (width >= TOOLBAR_FROM) await pressView(page, DASHBOARD, width);
      else await pressView(page, PRESET_NAMES.conversation, width);
      await expect(page).toHaveURL(atView(key, "conversation"));
      await expectConversation(page, `the press that leaves Dashboard ${at}`);

      // `t w` draws Dashboard, and `t c` Conversation. A choice from the one control's menu (below
      // 1280 px) hands the focus back to the control by design, so the outline's opening, which
      // takes only a focus nobody holds, is waited for only where the press left it loose.
      if (width >= TOOLBAR_FROM) await expectOpeningLanded(page);
      await chord(page, "w");
      await expect(page, `t w did not open Dashboard ${at}`).toHaveURL(atView(key, "dashboard"));
      await expectDashboard(page, `t w ${at}`);
      await chord(page, "c");
      await expect(page, `t c did not open Conversation ${at}`).toHaveURL(
        atView(key, "conversation")
      );
      await expectConversation(page, `t c ${at}`);

      // The Conversation control draws Conversation from Dashboard.
      await pressView(page, DASHBOARD, width);
      await expectDashboard(page, `the Dashboard control ${at}`);
      await pressView(page, PRESET_NAMES.conversation, width);
      await expect(page).toHaveURL(atView(key, "conversation"));
      await expectConversation(page, `the Conversation control ${at}`);

      // Where an address names a node, what it opens is the same at every width: the first width
      // this project reads reads it.
      if (width !== widths[0]) continue;
      const slugs = await slugsOf(key);

      // A bare address naming a node opens Conversation with that node's row focused.
      for (const named of [
        {
          address: `/issues/${key}#b-r1`,
          node: blockNode(slugs.get(seed.specArtifactId) ?? "", "r1"),
          what: "#b-r1",
        },
        {
          address: `/issues/${key}?ask=${seed.asks.byOption.id}`,
          node: seed.asks.byOption.id,
          what: "?ask=",
        },
        {
          address: `/issues/${key}?comment=${seed.threads.comment}`,
          node: seed.threads.comment,
          what: "?comment=",
        },
      ]) {
        await gotoIssue(page, named.address);
        await expectConversation(page, `the bare address with ${named.what}`);
        await expectOpeningLanded(page);
        await expect
          .poll(() => focusedNodeId(page), {
            message: `the bare address with ${named.what} does not focus the row it names`,
          })
          .toBe(named.node);
      }

      // A bare address with a words filter opens Conversation narrowed to the nodes whose words
      // hold it: the comment the words are from, and not the message thread.
      const words = (await getComment(seed.threads.comment)).comment.body;
      await gotoIssue(page, `/issues/${key}?q=${encodeURIComponent(words)}`);
      await expectConversation(page, "the bare address with ?q=");
      await expect(
        nodeRow(page, seed.threads.comment).first(),
        "?q= draws no row of the comment its words are from"
      ).toBeVisible();
      await expect(
        nodeRow(page, seed.threads.message),
        "?q= draws a row whose words do not hold it"
      ).toHaveCount(0);

      // The Decisions view's link from a decision to its node in the conversation lands on
      // `/conversation`, at that node.
      const decision = blockNode(
        slugs.get(seed.decisionsDocumentId) ?? "",
        seed.decisions.first.id
      );
      await gotoIssue(page, viewPath(key, "decisions"));
      const link = decisionsBody(page)
        .locator(`[data-node-row][data-node-id="${decision}"] a[data-entry-link]`)
        .first();
      await expect(
        link,
        "the decision's link in the conversation is not /conversation's"
      ).toHaveAttribute("href", `${viewPath(key, "conversation")}#b-${seed.decisions.first.id}`);
      await link.click();
      await expect(page).toHaveURL(
        new RegExp(`/issues/${literal(key)}/conversation#b-${literal(seed.decisions.first.id)}$`)
      );
      await expectConversation(page, "the Decisions view's link in the conversation");
      await expect
        .poll(() => focusedNodeId(page), {
          message: "the Decisions view's link in the conversation does not focus the decision",
        })
        .toBe(decision);
    }
  });
});

// SLOW(>=2s): the whole Dashboard seed, then seven loads - each view of the row and a document -
// and seven acts in place, each waited on until the header reads one less.
test("the header counts Dashboard's items on every view and drops one per act", async ({
  browser,
}) => {
  test.setTimeout(180_000);
  await phase(browser, seeded, async (page, seed) => {
    const key = seed.key;
    const waiting = waitingOf(page);
    const counted = (items: number) => `Waiting on you (${items})`;

    // On Dashboard the header reads Dashboard's own count.
    await gotoIssue(page, viewPath(key, "dashboard"));
    await expectDashboard(page, "/dashboard");
    await expect(countOf(waiting)).toHaveText(String(seed.items.length));
    await expectHeaderDrawn(page);
    await expect(
      waitingCount(page),
      "the header's count on Dashboard is not its list's"
    ).toHaveText(counted(seed.items.length));

    // Every other view of the row, and a document, reads the same number.
    for (const view of ["conversation", "spec", "verification", "plan", "decisions"]) {
      await gotoIssue(page, viewPath(key, view));
      await expectHeaderDrawn(page);
      await expect(
        waitingCount(page),
        `the header's count on ${view} is not the number of Dashboard's items`
      ).toHaveText(counted(seed.items.length));
    }
    const slug = (await slugsOf(key)).get(seed.verificationArtifactId) ?? "";
    await gotoIssue(page, buildIssuePath({ key, kind: "artifact", slug }));
    await expectHeaderDrawn(page);
    await expect(
      waitingCount(page),
      "the header's count on a document is not the number of Dashboard's items"
    ).toHaveText(counted(seed.items.length));

    // Each act in place takes its item out of the list, and one from the header's count.
    await gotoIssue(page, viewPath(key, "dashboard"));
    await expectDashboard(page, "/dashboard");
    let left = seed.items.length;
    const gone = async (item: WaitingItem) => {
      left -= 1;
      await expect(itemOf(waiting, item), `${testIdOf(item)} stays in the list`).toHaveCount(0);
      await expect(countOf(waiting)).toHaveText(String(left));
      await expect(
        waitingCount(page),
        `the header does not read one less once ${testIdOf(item)} left`
      ).toHaveText(counted(left));
    };

    // The approval request, answered with Approve.
    const request: WaitingItem = { id: seed.asks.approval.id, kind: "approval" };
    // The ask body answers an approval request as every surface does: its Approve option, then
    // Answer.
    await itemOf(waiting, request)
      .getByRole("radio", { name: /^Approve/ })
      .check();
    await itemOf(waiting, request).getByRole("button", { exact: true, name: "Answer" }).click();
    await gone(request);

    // The stale Verification approval, approved again at the newest version.
    const stale: WaitingItem = { id: seed.verificationArtifactId, kind: "stale" };
    await itemOf(waiting, stale)
      .getByRole("button", { name: /^Approve v\d+$/ })
      .click();
    await gone(stale);

    // A Change on the second decision.
    const changed: WaitingItem = { id: seed.decisions.second.id, kind: "decision" };
    const changedCard = itemOf(waiting, changed).locator("[data-decision-card]");
    await changedCard.getByRole("button", { exact: true, name: "Change" }).click();
    await changedCard
      .getByRole("textbox", { name: "Your change" })
      .fill("Count the threads apart.");
    await changedCard.getByRole("button", { exact: true, name: "Send change" }).click();
    await gone(changed);

    // Keep on the first decision.
    const kept: WaitingItem = { id: seed.decisions.first.id, kind: "decision" };
    await itemOf(waiting, kept)
      .locator("[data-decision-card]")
      .getByRole("button", { exact: true, name: "Keep" })
      .click();
    await gone(kept);

    // A question answered by one of its options.
    const byOption: WaitingItem = { id: seed.asks.byOption.id, kind: "question" };
    await itemOf(waiting, byOption).getByRole("radio", { name: seed.asks.byOption.option }).check();
    await itemOf(waiting, byOption).getByRole("button", { exact: true, name: "Answer" }).click();
    await gone(byOption);

    // A question answered in words.
    const inWords: WaitingItem = { id: seed.asks.inWords, kind: "question" };
    await itemOf(waiting, inWords).locator("[data-ask-answer]").fill("Nothing waits on you.");
    await itemOf(waiting, inWords).getByRole("button", { exact: true, name: "Answer" }).click();
    await gone(inWords);

    // A reply to the comment thread, which moves the reader's read mark past the planner's reply.
    const thread: WaitingItem = { id: seed.threads.comment, kind: "thread" };
    await itemOf(waiting, thread).getByRole("textbox", { name: "Reply" }).fill("Thank you.");
    await itemOf(waiting, thread).getByRole("button", { exact: true, name: "Send" }).click();
    await gone(thread);
  });
});

// SLOW(>=2s): the whole Dashboard seed, then two loads of Dashboard, each followed by a thread's
// link and waited on until the thread's row holds the focus.
test("a thread item's link opens the thread", async ({ browser }) => {
  await phase(browser, seeded, async (page, seed) => {
    // dispatch://INFRA-5704 is not on this main, so the link is the thread's own address.
    for (const thread of [
      {
        address: buildIssuePath({ id: seed.threads.message, key: seed.key, kind: "message" }),
        id: seed.threads.message,
      },
      {
        address: buildIssuePath({ id: seed.threads.comment, key: seed.key, kind: "comment" }),
        id: seed.threads.comment,
      },
    ]) {
      await gotoIssue(page, viewPath(seed.key, "dashboard"));
      await expectDashboard(page, "/dashboard");
      const link = itemOf(waitingOf(page), { id: thread.id, kind: "thread" }).locator(
        `a[href$="${thread.address}"]`
      );
      await expect(link, `the thread item draws no link to ${thread.address}`).not.toHaveCount(0);
      await link.first().click();
      await expect(page).toHaveURL(new RegExp(`${literal(thread.address)}$`));
      await expectConversation(page, thread.address);
      await expect
        .poll(() => focusedNodeId(page), {
          message: `${thread.address}, followed from Dashboard, does not focus the thread's row`,
        })
        .toBe(thread.id);
    }
  });
});

// SLOW(>=2s): the whole Dashboard seed before one load of Dashboard, every item drawn, and Details
// and the focused ask's references opened, each read until its request is sent.
test("Details on a focused ask reads that ask and its references", async ({ browser }) => {
  test.skip(
    !PART_M_DETAILS,
    "Details draws a focused node's facts only with part M's FocusedNodeFacts.tsx " +
      "(dispatch://INFRA-3948), which this head does not hold; part M's branch runs this row"
  );
  await phase(browser, seeded, async (page, seed) => {
    const ask = seed.asks.byOption.id;
    const reference = `dispatch://${seed.key}/ask/${ask}`;
    const reads = watchApiRequests(page);
    const askReads = () =>
      reads.requests.filter((sent) => sent.method === "GET" && sent.path === `/api/v1/asks/${ask}`)
        .length;
    const referenceReads = () =>
      reads.requests.filter((sent) => {
        const url = new URL(sent.path, "http://dashboard");
        return (
          sent.method === "GET" &&
          url.pathname === "/api/v1/references" &&
          url.searchParams.get("to") === reference
        );
      }).length;

    await gotoIssue(page, viewPath(seed.key, "dashboard"));
    await expectDashboard(page, "/dashboard");
    const waiting = waitingOf(page);
    await drawEveryItem(waiting, seed);

    // The focus goes inside the question's item, to its answer box.
    const answer = itemOf(waiting, { id: ask, kind: "question" }).locator("[data-ask-answer]");
    await expect(answer, "the question's item draws no answer box").toHaveCount(1);
    await answer.focus();
    await expect(answer).toBeFocused();
    await expectLoadSettled(reads);
    expect(askReads(), "Dashboard read the focused question before Details opened").toBe(0);

    // Details, opened by its control, which leaves the focus where it is, reads the ask and draws
    // its facts.
    const control = page.getByTestId("issue-details-control");
    await expect(control, "the page draws no Details control").toHaveCount(1);
    await control.click();
    await expect(marginShell(page), "the Details control opened no Details").toBeVisible();
    await expect(answer, "opening Details took the focus off the question").toBeFocused();
    await expect
      .poll(askReads, {
        message: `opening Details on the focused question sent no GET /api/v1/asks/${ask}`,
      })
      .toBeGreaterThan(0);
    const facts = marginShell(page).getByTestId("focused-node-facts");
    await expect(facts, "Details draws no facts of the focused question").toHaveAttribute(
      "data-focused-node",
      ask
    );
    const kind = facts
      .locator("dl > div")
      .filter({ has: page.getByText(`${CONTROL_LABELS.node_kind}:`, { exact: true }) })
      .locator("dd");
    await expect(kind).toHaveText(NODE_TYPE_NAMES.ask, { ignoreCase: true });

    // What references the ask is read when the reader opens it, and not before.
    const references = facts.locator("section[data-node-references] > button[aria-expanded]");
    await expect(references, "the question's facts draw no control for its references").toHaveCount(
      1
    );
    expect(referenceReads(), "Details read the question's references before they opened").toBe(0);
    await references.click();
    await expect(references).toHaveAttribute("aria-expanded", "true");
    await expect
      .poll(referenceReads, {
        message: `opening the question's references sent no GET /api/v1/references?to=${reference}`,
      })
      .toBeGreaterThan(0);
  });
});

/** Whether a read of Dashboard's first load is one R8 names on this main, for `seed`: the issue,
 *  its open asks, its handoffs, its Legion state, the reader's unread replies of the issue with
 *  no `limit`, the own route of each thread drawn, the decisions and the Changes' comments of
 *  the decisions document, the results and the coverage, the batched reference targets, and a
 *  picture's version with the summary. Where dispatch://INFRA-1253 is on main, its tree read. It
 *  is the rule `e2e/issue-dashboard-today.e2e.ts` reads today's page by, since R8 names one set of
 *  reads for both pages. */
function namedByR8(seed: DashboardSeed, method: string, sent: URL): boolean {
  if (method !== "GET") return false;
  const issue = `/api/v1/issues/${seed.key}`;
  const query = [...sent.searchParams]
    .map(([name, value]) => `${name}=${value}`)
    .sort()
    .join("&");
  const threads = seed.items.filter((item) => item.kind === "thread").map((item) => item.id);
  switch (sent.pathname) {
    case issue:
    case `${issue}/handoffs`:
    case `${issue}/legion`:
    case `${issue}/decisions`:
    case `${issue}/results`:
    case `${issue}/coverage`:
    case `${issue}/summary`:
      return query === "";
    case `${issue}/asks`:
      return query === "state=open";
    case "/api/v1/me/replies":
      return query === `issue=${seed.key}&state=unread`;
    case `${issue}/comments`:
      return query === `artifact=${seed.decisionsDocumentId}`;
    case "/api/v1/reference-targets":
      return true;
    case `${issue}/tree`:
      return seed.holds.treeRead;
  }
  const own = /^\/api\/v1\/(?:comments|issues\/[^/]+\/messages)\/([^/]+)$/.exec(sent.pathname);
  if (own !== null) return threads.includes(own[1] ?? "");
  return new RegExp(`^${literal(issue)}/artifacts/[^/]+/versions/\\d+$`).test(sent.pathname);
}

// SLOW(>=2s): the whole Dashboard seed, two loads of Spec and one cold load of Dashboard, each read
// until nothing has been in flight for 1.5 s, and a press of Dashboard read the same way.
test("pressing Dashboard from Spec sends only R8's reads, and a cold load stays under 40", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seeded, async (page, seed) => {
    const reads = watchApiRequests(page);
    await gotoIssue(page, viewPath(seed.key, "spec"));
    await expectLoadSettled(reads);
    // The same view in another tab, with no Dashboard, over the same time: what it sends is the
    // shell's own.
    const control = await page.context().newPage();
    await control.setViewportSize(viewportAt(DESKTOP_WIDTH));
    const shell = watchApiRequests(control);
    await gotoIssue(control, viewPath(seed.key, "spec"));
    await expectLoadSettled(shell);
    // The shell's own polls - the reads of no issue that the control tab made as it loaded, the
    // agents registry among them - repeat on their own clock in either tab: a window the control
    // tab polled in is no window this tab polls in. A read of the issue is held to the windows
    // below.
    const shellPolls = new Set(
      shell.requests
        .filter((sent) => !sent.route.includes("/issues/"))
        .map((sent) => `${sent.method} ${sent.route}`)
    );

    const dashboard = presetRow(page).getByRole("button", { exact: true, name: DASHBOARD });
    await expect(dashboard, "Spec's view row draws no Dashboard control").toHaveCount(1);
    reads.reset();
    shell.reset();
    await dashboard.click();
    await expectDashboard(page, "the Dashboard control on Spec");
    await expect(itemsOf(waitingOf(page))).toHaveCount(5);
    await expectLoadSettled(reads);
    expect(
      reads.requests
        .filter((sent) => sent.method === "GET")
        .map((sent) => new URL(sent.path, "http://dashboard"))
        .filter(
          (sent) =>
            (sent.pathname === `/api/v1/issues/${seed.key}/comments` &&
              !sent.searchParams.has("artifact")) ||
            sent.pathname === `/api/v1/issues/${seed.key}/messages`
        )
        .map((sent) => `${sent.pathname}${sent.search}`),
      "the press on Dashboard read the issue's whole comments or messages list"
    ).toEqual([]);
    const shellRoutes = new Set(shell.requests.map((sent) => `${sent.method} ${sent.route}`));
    expect(
      reads.requests
        .filter(
          (sent) =>
            !shellRoutes.has(`${sent.method} ${sent.route}`) &&
            !shellPolls.has(`${sent.method} ${sent.route}`)
        )
        .filter((sent) => !namedByR8(seed, sent.method, new URL(sent.path, "http://dashboard")))
        .map((sent) => `${sent.method} ${sent.path}`),
      "the press on Dashboard sent reads R8 does not name"
    ).toEqual([]);
    await control.close();

    // A cold load of Dashboard's own address, in a tab of its own.
    const cold = await page.context().newPage();
    await cold.setViewportSize(viewportAt(DESKTOP_WIDTH));
    const coldReads = watchApiRequests(cold);
    await gotoIssue(cold, viewPath(seed.key, "dashboard"));
    await expectDashboard(cold, "a cold load of /dashboard");
    await expect(itemsOf(waitingOf(cold))).toHaveCount(5);
    await expectLoadSettled(coldReads);
    expect(
      coldReads.total(),
      `a cold load of /dashboard sent more than ${COLD_LOAD_AT_MOST} requests under /api/: ${JSON.stringify(coldReads.byRoute())}`
    ).toBeLessThanOrEqual(COLD_LOAD_AT_MOST);
    await cold.close();
  });
});
