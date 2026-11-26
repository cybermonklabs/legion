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
// - Dashboard at rest draws neither part L's writing box `[data-testid="issue-composer"]`
//   (`IssueComposer.tsx`, which the foot keeps mounted and `hidden` while it is not drawn) nor the
//   issue's read mark `[data-outline-end]` (`IssueReadMark.tsx`), and Conversation at rest draws
//   both: `slots/foot.tsx` draws them at rest in Conversation alone. These are the names part L
//   gives them (dispatch://INFRA-3949), so the row that reads them runs only where the head holds
//   part L's `IssueComposer.tsx` (c-dashboard-new (2)).
// - Dashboard's own region on the new page is `[data-dashboard]` (`slots/view-dashboard.tsx`), and
//   every text it draws carries one of the page's two marks (`page-structure.ts`): the page's
//   (`data-page-chrome`) on Dashboard's own words and on the ask body's and the decision card's,
//   and an author's (`data-author-text`) on every value, every author's words, and what a shared
//   control draws in words of its own (a writing box, the approval control).

import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";

import {
  ASK_APPROVAL_WORDS,
  ASK_FORM_WORDS,
  ASK_HAND_ON_WORDS,
  ASK_LINE_WORDS,
} from "../web/src/features/ask-body/ask-body-words";
import {
  DECISION_ACT_WORDS,
  DECISION_LINE_WORDS,
  DECISION_STATE_WORDS,
} from "../web/src/features/decision-card/decision-card-words";
import { URGENCY_LABELS } from "../web/src/features/inbox/ask-urgency";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  AUTHOR_TEXT_ATTRIBUTE,
  PAGE_CHROME_ATTRIBUTE,
} from "../web/src/features/issue-page/page-structure";
import {
  CONTROL_LABELS,
  DASHBOARD_WORDS,
  NODE_TYPE_NAMES,
  PHASE_WORDS,
  PRESET_NAMES,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { getComment, getIssue } from "./api";
import {
  type DashboardSeed,
  type ItemKind,
  READER,
  SEEDED_LEGION,
  seedDashboard,
  type WaitingItem,
} from "./dashboard-seed";
import {
  beyondShell,
  countOf,
  drawEveryItem,
  drawnItems,
  itemOf,
  itemsOf,
  literal,
  mainOfThisRun,
  namedByR8,
  nextFive,
  ROLE_WORDS,
  STEP_ROWS,
  shellPollsOf,
  shellRoutesOf,
  TILES,
  testIdOf,
  waitingOf,
} from "./dashboard-support";
import {
  contentsPane,
  gotoIssue,
  issuePage,
  marginShell,
  openMenu,
  phase,
  presetChooser,
  presetMenuItem,
  presetRow,
  presetToggle,
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

/** Whether the head holds part L's writing box (dispatch://INFRA-3949, #4246), which the foot's
 *  row reads with the issue's read mark; c-dashboard-new (2) gates that row on the file, not on
 *  part L's merge. */
const PART_L_FOOT = existsSync(
  fileURLToPath(new URL("../web/src/features/issue-page/IssueComposer.tsx", import.meta.url))
);

/** The widths the rows read, and the width from which the view row is a toolbar of toggles
 *  rather than one control with a menu (`IssuePresets.tsx`). */
const DESKTOP_WIDTH = 1440;
const PHONE_WIDTH = 390;
const TOOLBAR_FROM = 1280;

/** The most requests under `/api/` a cold load of Dashboard sends (c-dashboard-new (6)). */
const COLD_LOAD_AT_MOST = 40;

/** Which main this run's server is, read once per worker (R27). */
const thisMain = mainOfThisRun();

/** The Dashboard seed on the main this run's server is. */
function seeded(): Promise<DashboardSeed> {
  return seedDashboard(thisMain());
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
  await expect(viewOn(page), `${where} does not have Dashboard on`).toHaveText(
    PRESET_NAMES.dashboard
  );
  await expect(waitingOf(issuePage(page)), `${where} draws no Dashboard`).toBeVisible();
  await expect(tree(page), `${where} draws the outline with Dashboard`).toHaveCount(0);
}

/** Fails unless the page has Conversation on and draws the outline, and no Dashboard. */
async function expectConversation(page: Page, where: string): Promise<void> {
  await expect(viewOn(page), `${where} does not have Conversation on`).toHaveText(
    PRESET_NAMES.conversation
  );
  await expect(tree(page), `${where} draws no outline`).toHaveCount(1);
  await expect(waitingOf(issuePage(page)), `${where} draws Dashboard`).toHaveCount(0);
}

/** Fails unless Dashboard is the first control of the view row, in the row's shape at `width`:
 *  the first toggle from 1280 px up, and the first item of the one control's menu below. */
async function expectDashboardFirst(page: Page, width: number): Promise<void> {
  if (width >= TOOLBAR_FROM) {
    await expect(
      presetToggles(page).first().locator("[data-preset-name]"),
      `the view row's first control is not Dashboard at ${width} px`
    ).toHaveText(PRESET_NAMES.dashboard);
    return;
  }
  await expect(presetChooser(page), `the view row draws no one control at ${width} px`).toHaveCount(
    1
  );
  await presetChooser(page).click();
  await expect(
    openMenu(page).getByRole("menuitemradio").first(),
    `the view row's first item is not Dashboard at ${width} px`
  ).toHaveAccessibleName(new RegExp(`^${PRESET_NAMES.dashboard}( |$)`));
  await page.keyboard.press("Escape");
  await expect(openMenu(page)).toHaveCount(0);
}

/** Presses the control of `preset`, in the row's shape at `width`: its toggle from 1280 px up,
 *  and its item of the one control's menu below. */
async function pressView(page: Page, preset: Preset, width: number): Promise<void> {
  const name = PRESET_NAMES[preset];
  if (width >= TOOLBAR_FROM) {
    const toggle = presetToggle(page, preset);
    await expect(toggle, `the view row draws no ${name} control at ${width} px`).toHaveCount(1);
    await toggle.click();
    return;
  }
  await expect(presetChooser(page), `the view row draws no one control at ${width} px`).toHaveCount(
    1
  );
  await presetChooser(page).click();
  const item = presetMenuItem(page, preset);
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
    const waiting = waitingOf(issuePage(page));
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
    await expect(track.getByRole("listitem")).toHaveText(STEP_ROWS);
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
      await gotoIssue(page, presetPath(key, "dashboard"));
      await expectDashboard(page, `/dashboard ${at}`);

      // `/conversation` draws Conversation, and the Dashboard control draws Dashboard.
      await gotoIssue(page, presetPath(key, "conversation"));
      await expectConversation(page, `/conversation ${at}`);
      await pressView(page, "dashboard", width);
      await expect(page).toHaveURL(new RegExp(`${presetPath(key, "dashboard")}$`));
      await expectDashboard(page, `the Dashboard control ${at}`);

      // From 1280 px up, pressing Dashboard while it is on draws Conversation, as pressing each
      // view that is on turns it off. The one control's menu keeps the view that is on when its
      // item is chosen (`IssuePresets.tsx`, read by `e2e/issue-page-d.e2e.ts`), so below 1280 px
      // the Conversation item is the press that leaves Dashboard.
      if (width >= TOOLBAR_FROM) await pressView(page, "dashboard", width);
      else await pressView(page, "conversation", width);
      await expect(page).toHaveURL(new RegExp(`${presetPath(key, "conversation")}$`));
      await expectConversation(page, `the press that leaves Dashboard ${at}`);

      // `t w` draws Dashboard, and `t c` Conversation. A choice from the one control's menu (below
      // 1280 px) hands the focus back to the control by design, so the outline's opening, which
      // takes only a focus nobody holds, is waited for only where the press left it loose.
      if (width >= TOOLBAR_FROM) await expectOpeningLanded(page);
      await chord(page, "w");
      await expect(page, `t w did not open Dashboard ${at}`).toHaveURL(
        new RegExp(`${presetPath(key, "dashboard")}$`)
      );
      await expectDashboard(page, `t w ${at}`);
      await chord(page, "c");
      await expect(page, `t c did not open Conversation ${at}`).toHaveURL(
        new RegExp(`${presetPath(key, "conversation")}$`)
      );
      await expectConversation(page, `t c ${at}`);

      // The Conversation control draws Conversation from Dashboard.
      await pressView(page, "dashboard", width);
      await expectDashboard(page, `the Dashboard control ${at}`);
      await pressView(page, "conversation", width);
      await expect(page).toHaveURL(new RegExp(`${presetPath(key, "conversation")}$`));
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
      await gotoIssue(page, presetPath(key, "decisions"));
      const link = decisionsBody(page)
        .locator(`[data-node-row][data-node-id="${decision}"] a[data-entry-link]`)
        .first();
      await expect(
        link,
        "the decision's link in the conversation is not /conversation's"
      ).toHaveAttribute("href", `${presetPath(key, "conversation")}#b-${seed.decisions.first.id}`);
      await link.click();
      await expect(page).toHaveURL(
        new RegExp(`${presetPath(key, "conversation")}#b-${literal(seed.decisions.first.id)}$`)
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
    const waiting = waitingOf(issuePage(page));
    const counted = (items: number) => `Waiting on you (${items})`;

    // On Dashboard the header reads Dashboard's own count.
    await gotoIssue(page, presetPath(key, "dashboard"));
    await expectDashboard(page, "/dashboard");
    await expect(countOf(waiting)).toHaveText(String(seed.items.length));
    await expectHeaderDrawn(page);
    await expect(
      waitingCount(page),
      "the header's count on Dashboard is not its list's"
    ).toHaveText(counted(seed.items.length));

    // Every other view of the row, and a document, reads the same number.
    for (const view of ["conversation", "spec", "verification", "plan", "decisions"] as const) {
      await gotoIssue(page, presetPath(key, view));
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
    await gotoIssue(page, presetPath(key, "dashboard"));
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
      await gotoIssue(page, presetPath(seed.key, "dashboard"));
      await expectDashboard(page, "/dashboard");
      const link = itemOf(waitingOf(issuePage(page)), { id: thread.id, kind: "thread" }).locator(
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

    await gotoIssue(page, presetPath(seed.key, "dashboard"));
    await expectDashboard(page, "/dashboard");
    const waiting = waitingOf(issuePage(page));
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
    await expect(facts).toContainText(new RegExp(`\\b${NODE_TYPE_NAMES.ask}\\b`, "i"));

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

// R2: part L's foot draws its writing box and the issue's read mark at rest in Conversation
// alone, and `isFilter` reads Dashboard as no view, so the foot's test is the address naming
// Conversation with no document open. The view row is one control with a menu below 1280 px, so
// the row reads the toolbar at 1440 px and the menu at 390 px, which the `iphone` project reads
// with its own viewport.
// SLOW(>=2s): the whole Dashboard seed, then at each width one load of the bare address and one
// press of Conversation, each read until the page draws the view the press or the load names.
test("Dashboard at rest draws neither the writing box nor the read mark, and Conversation draws both @phone", async ({
  browser,
}) => {
  test.skip(
    !PART_L_FOOT,
    "The foot draws a writing box and a read mark only with part L's IssueComposer.tsx " +
      "(dispatch://INFRA-3949, #4246), which this head does not hold; the row runs once this " +
      "branch holds part L"
  );
  test.setTimeout(120_000);
  await phase(browser, seeded, async (page, seed) => {
    const key = seed.key;
    // Part L's writing box, which the foot keeps mounted and `hidden` while it does not draw it,
    // and the issue's read mark at the end of the outline.
    const box = page.getByTestId("issue-composer");
    const mark = page.locator("[data-outline-end]");
    const widths =
      test.info().project.name === "iphone" ? [PHONE_WIDTH] : [DESKTOP_WIDTH, PHONE_WIDTH];
    for (const width of widths) {
      await page.setViewportSize(viewportAt(width));
      const at = `at ${width} px`;

      // The bare address draws Dashboard. The foot is drawn in the commit that draws the issue,
      // before Dashboard's own reads answer, so once Dashboard is drawn the foot's absence holds.
      await gotoIssue(page, `/issues/${key}`);
      await expectDashboard(page, `the bare address ${at}`);
      await expect(box, `Dashboard at rest draws the writing box ${at}`).toBeHidden();
      await expect(mark, `Dashboard at rest draws the read mark ${at}`).toHaveCount(0);

      // The Conversation control draws Conversation at rest, with the box and the mark.
      await pressView(page, "conversation", width);
      await expect(page).toHaveURL(new RegExp(`${presetPath(key, "conversation")}$`));
      await expectConversation(page, `the Conversation control ${at}`);
      await expect(box, `Conversation at rest draws no writing box ${at}`).toBeVisible();
      await expect(mark, `Conversation at rest draws no read mark ${at}`).toHaveCount(1);
    }
  });
});

// SLOW(>=2s): the whole Dashboard seed, two loads of Spec and one cold load of Dashboard, each read
// until nothing has been in flight for 1.5 s, and a press of Dashboard read the same way.
test("pressing Dashboard from Spec sends only R8's reads, and a cold load stays under 40", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seeded, async (page, seed) => {
    const reads = watchApiRequests(page);
    await gotoIssue(page, presetPath(seed.key, "spec"));
    await expectLoadSettled(reads);
    // The same view in another tab, with no Dashboard, over the same time: what it sends is the
    // shell's own.
    const control = await page.context().newPage();
    await control.setViewportSize(viewportAt(DESKTOP_WIDTH));
    const shell = watchApiRequests(control);
    await gotoIssue(control, presetPath(seed.key, "spec"));
    await expectLoadSettled(shell);
    // The shell's own polls repeat on their own clock in either tab (`shellPollsOf`).
    const shellPolls = shellPollsOf(shell);

    const dashboard = presetToggle(page, "dashboard");
    await expect(dashboard, "Spec's view row draws no Dashboard control").toHaveCount(1);
    reads.reset();
    shell.reset();
    await dashboard.click();
    await expectDashboard(page, "the Dashboard control on Spec");
    await expect(itemsOf(waitingOf(issuePage(page)))).toHaveCount(5);
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
    expect(
      beyondShell(reads.requests, shellRoutesOf(shell), shellPolls)
        .filter((sent) => !namedByR8(seed, sent.method, new URL(sent.path, "http://dashboard")))
        .map((sent) => `${sent.method} ${sent.path}`),
      "the press on Dashboard sent reads R8 does not name"
    ).toEqual([]);
    await control.close();

    // A cold load of Dashboard's own address, in a tab of its own.
    const cold = await page.context().newPage();
    await cold.setViewportSize(viewportAt(DESKTOP_WIDTH));
    const coldReads = watchApiRequests(cold);
    await gotoIssue(cold, presetPath(seed.key, "dashboard"));
    await expectDashboard(cold, "a cold load of /dashboard");
    await expect(itemsOf(waitingOf(issuePage(cold)))).toHaveCount(5);
    await expectLoadSettled(coldReads);
    expect(
      coldReads.total(),
      `a cold load of /dashboard sent more than ${COLD_LOAD_AT_MOST} requests under /api/: ${JSON.stringify(coldReads.byRoute())}`
    ).toBeLessThanOrEqual(COLD_LOAD_AT_MOST);
    await cold.close();
  });
});

/** How the page divides one text Dashboard draws (`page-structure.ts`): by the mark of the
 *  nearest element inside Dashboard that carries one - the page's own words (`chrome`), an
 *  author's words or a value (`author`), or both on one element (`both`) - or by none. */
type TextMark = "author" | "both" | "chrome" | "none";

/** One text node Dashboard draws, its whitespace run together, and its mark. */
interface MarkedText {
  readonly mark: TextMark;
  readonly text: string;
}

/** The marks in the order `marksOf` lists them. */
const TEXT_MARKS: readonly TextMark[] = ["author", "both", "chrome", "none"];

/** The element each kind of item is drawn whole by once its reads have answered: a thread's
 *  first write, the ask body, the decision card, and the one approval control. */
const ITEM_PARTS: Readonly<Record<ItemKind, string>> = {
  approval: "[data-ask-body]",
  decision: "[data-decision-card]",
  question: "[data-ask-body]",
  stale: "[data-approval-artifact]",
  thread: "[data-thread-entry]",
};

/** Each text node Dashboard draws that has a box, with its mark. The search for a mark climbs no
 *  higher than Dashboard's own region (`[data-dashboard]`), so a mark on the region itself or on
 *  the page around it marks none of Dashboard's text. */
function dashboardTexts(page: Page): Promise<MarkedText[]> {
  const region = issuePage(page).locator("[data-dashboard]");
  return region.evaluate(
    (root, attributes) => {
      const markOf = (element: Element): TextMark => {
        for (let at: Element | null = element; at !== null && at !== root; at = at.parentElement) {
          const chrome = at.hasAttribute(attributes.chrome);
          const author = at.hasAttribute(attributes.author);
          if (chrome && author) return "both";
          if (chrome) return "chrome";
          if (author) return "author";
        }
        return "none";
      };
      const found: MarkedText[] = [];
      const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
      for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
        const text = (node.textContent ?? "").replace(/\s+/g, " ").trim();
        const parent = node.parentElement;
        if (text === "" || parent === null || parent.getClientRects().length === 0) continue;
        found.push({ mark: markOf(parent), text });
      }
      return found;
    },
    { author: AUTHOR_TEXT_ATTRIBUTE, chrome: PAGE_CHROME_ATTRIBUTE }
  );
}

/** The marks each of `texts` is drawn in, over every text node that reads it whole. */
function marksOf(
  drawn: readonly MarkedText[],
  texts: readonly string[]
): Record<string, TextMark[]> {
  return Object.fromEntries(
    texts.map((text): [string, TextMark[]] => [
      text,
      TEXT_MARKS.filter((mark) => drawn.some((one) => one.text === text && one.mark === mark)),
    ])
  );
}

/** A text as a failure names it: its mark, then its words. */
function described(one: MarkedText): string {
  return `${one.mark}: ${one.text}`;
}

/** Fails unless `items` are the items drawn, in order, each drawn whole (`ITEM_PARTS`). */
async function expectItemsDrawn(
  waiting: Locator,
  items: readonly WaitingItem[],
  where: string
): Promise<void> {
  await expect
    .poll(() => drawnItems(waiting), { message: `${where} are not the items drawn, in order` })
    .toEqual(items.map(testIdOf));
  for (const item of items) {
    await expect(
      itemOf(waiting, item).locator(ITEM_PARTS[item.kind]).first(),
      `${where} draws ${testIdOf(item)} with nothing in it`
    ).toBeVisible();
  }
}

/** Fails unless Dashboard draws each of `texts.chrome` in the page's mark alone and each of
 *  `texts.author` in an author's alone, read until each is drawn; then unless every text it draws
 *  carries one mark, every number an author's, and every name the seed wrote
 *  (`DashboardSeed.names`) an author's. */
async function expectMarked(
  page: Page,
  seed: DashboardSeed,
  where: string,
  texts: { readonly author: readonly string[]; readonly chrome: readonly string[] }
): Promise<void> {
  const wanted = [...texts.chrome, ...texts.author];
  const marked = Object.fromEntries([
    ...texts.chrome.map((text): [string, TextMark[]] => [text, ["chrome"]]),
    ...texts.author.map((text): [string, TextMark[]] => [text, ["author"]]),
  ]);
  await expect
    .poll(async () => marksOf(await dashboardTexts(page), wanted), {
      message: `${where} draws a label outside the page's mark, or a value outside an author's`,
    })
    .toEqual(marked);
  const drawn = await dashboardTexts(page);
  expect(
    drawn.filter((one) => one.mark === "none" || one.mark === "both").map(described),
    `${where} draws text that is neither the page's own words nor an author's`
  ).toEqual([]);
  expect(
    drawn.filter((one) => /^\d+$/.test(one.text) && one.mark !== "author").map(described),
    `${where} draws a number outside an author's mark`
  ).toEqual([]);
  const names = new Set(seed.names);
  expect(
    drawn.filter((one) => names.has(one.text) && one.mark !== "author").map(described),
    `${where} draws a name the seed wrote outside an author's mark`
  ).toEqual([]);
}

// R21's two marks on Dashboard (`page-structure.ts`), read on the seeded issue as the new page
// draws it with every part's reads answered - the phase track, the first five items and then all
// nine, with the approval request, the questions, the decisions, the stale approval and the two
// threads, the tiles and the agents - at the desktop width and the phone's. Every text carries
// exactly one mark, found no higher than Dashboard's own region; Dashboard's labels and the words
// of the ask body and the decision card carry the page's, and the count, each number, each name
// and an author's words carry an author's. Drawn with no mark, as Dashboard was before it marked
// its text, the labels and the values read `none` and the row fails at its first read.
// SLOW(>=2s): the whole Dashboard seed, then at each width one load of the bare address and one
// press of the control under the first five, each read until the texts it names are drawn.
test("every text Dashboard draws carries one mark, the page's on its labels and an author's on its values", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  await phase(browser, seeded, async (page, seed) => {
    const waiting = waitingOf(issuePage(page));
    const roles = SEEDED_LEGION.roles.filter((role) => role.state !== "retired");
    // Dashboard's own words, and the words of the ask body, the decision card and a thread item
    // that the first five items draw between them.
    const labels = [
      DASHBOARD_WORDS.parts.phase,
      DASHBOARD_WORDS.parts.waiting,
      DASHBOARD_WORDS.parts.progress,
      DASHBOARD_WORDS.parts.agents,
      ...Object.values(DASHBOARD_WORDS.tiles),
      PHASE_WORDS.steps.planning,
      ...roles.map((role) => ROLE_WORDS[role.state] ?? ""),
      DASHBOARD_WORDS.thread_link,
      PRESET_NAMES.verification,
      ASK_APPROVAL_WORDS.requested,
      ASK_LINE_WORDS.kind,
      ASK_HAND_ON_WORDS.asked_of,
      ASK_FORM_WORDS.answer,
      DECISION_STATE_WORDS.waiting_review,
      DECISION_ACT_WORDS.keep,
    ];
    // The values: the count, the reader every open ask is asked of, and each agent's role. The
    // decision card's author line is a name where the read names one, and the card's own word
    // where it names none.
    const values = [String(seed.items.length), READER, ...roles.map((role) => role.role)];
    const author = seed.decisions.author;
    const authorWord = author === DECISION_LINE_WORDS.author_unknown;
    const authorLine = { author: authorWord ? [] : [author], chrome: authorWord ? [author] : [] };
    for (const width of [DESKTOP_WIDTH, PHONE_WIDTH]) {
      const at = `at ${width} px`;
      await page.setViewportSize(viewportAt(width));
      await gotoIssue(page, `/issues/${seed.key}`);
      await expectDashboard(page, `the bare address ${at}`);
      // Dashboard with its reads answered, not a part still waiting: the count, the first five
      // each drawn whole, and the control under them with the number left.
      await expect(countOf(waiting)).toHaveText(String(seed.items.length));
      await expectItemsDrawn(waiting, seed.items.slice(0, 5), `the first five ${at}`);
      await expect(nextFive(waiting)).toBeVisible();
      await expectMarked(page, seed, `the first five ${at}`, {
        author: [...values, String(seed.items.length - 5), ...authorLine.author],
        chrome: [...labels, DASHBOARD_WORDS.next_five, ...authorLine.chrome],
      });

      // Every item: the questions below the first five add the urgency word and an option.
      await nextFive(waiting).click();
      await expectItemsDrawn(waiting, seed.items, `every item ${at}`);
      await expectMarked(page, seed, `every item ${at}`, {
        author: [...values, seed.asks.byOption.option, ...authorLine.author],
        chrome: [...labels, ASK_LINE_WORDS.urgency.high, ...authorLine.chrome],
      });
    }
  });
});
