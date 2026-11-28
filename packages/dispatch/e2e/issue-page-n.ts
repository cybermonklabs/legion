/*
 * The helpers part N's rows read (dispatch://INFRA-1196 decision 2): each helper of the
 * checkpoint's `e2e/issue-page.ts` that no earlier part's helper file holds, and the inventory
 * row's reading of what the new page carries, over `e2e/page-inventory.ts`, which reads what a
 * page offers for that row and the recorder. The helpers of piece 7's rows (dispatch://INFRA-6480)
 * are in `e2e/issue-page-n-documents.ts`, and this module imports from there those it reads.
 * Every other helper is imported from the part that brought it.
 */

import {
  expect,
  type Locator,
  type Page,
  type Request,
  type Response,
  test,
} from "@playwright/test";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import { KEY_REPORTS, PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  DASHBOARD_WORDS,
  PRESET_NAMES,
  type Preset,
} from "../web/src/features/issue-page/page-words";
import { showMoreName } from "../web/src/features/issue-page/show-more-fixture";
import {
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  editArtifact,
  getIssue,
  getIssueDecisions,
  getMyState,
  listMyReplies,
  requestApproval,
  reviewArtifact,
} from "./api";
import { mainHolds } from "./dashboard-seed";
import { connectedDot, documentEditor } from "./editor";
import { asPlanner, PROJECT, pause } from "./issue-layout-seed-support";
import {
  gotoIssue,
  issuePage,
  marginShell,
  openDetails,
  presetChooser,
  presetToolbar,
} from "./issue-page";
import { expectIssuePageDrawn } from "./issue-page-g";
import {
  historyEntries,
  historyPanel,
  historyShowMore,
  historyShowMoreState,
} from "./issue-page-m";
import {
  dashboardWaiting,
  documentsList,
  documentsRows,
  drawnColours,
  expectActiveControl,
  expectDashboardDrawn,
  offPalette,
  openVersionControl,
  SELECTORS,
  showMoreControl,
  specAddress,
  versionControl,
} from "./issue-page-n-documents";
import {
  describe,
  entryKey,
  entryOf,
  type InventoryEntry,
  type MeasuredEntry,
  measured,
  settled,
} from "./page-inventory";
import type { ScaleCopy, ScaleNodes } from "./scale-seed";
import { navigateInApp } from "./sends";
import { settledValue } from "./settled";
import {
  type ApiRequest,
  apiRoute,
  expectLoadSettled,
  isLiveSocket,
  watchApiRequests,
} from "./traffic";
import { assertPageFits } from "./unclipped";

/** Opens an issue's spec at its own address and waits until its editor shows `text` and its
 *  document is connected (`expectDocumentConnected`), so a selection or a margin card the row
 *  reaches for next is live. `e2e/editor.ts`'s `openSpec` opens `/spec`, which on the new page is
 *  the Spec view and draws no editor. */
export async function openSpecDocument(page: Page, issueKey: string, text: string): Promise<void> {
  await page.goto(await specAddress(issueKey));
  await expect(documentEditor(page)).toContainText(text);
  await expectDocumentConnected(page);
}

/** Waits until the opened document's editor is live: its connection dot reads "connected".
 *  Today's page draws the dot in the document's own toolbar; the new page draws it in the
 *  version control, beside an open editor alone (part N's R3), and the version control stands
 *  behind its small control in the filter row (R4), so the dot is read with the control open, as
 *  a reader sees it. The control is left open. */
export async function expectDocumentConnected(page: Page): Promise<void> {
  await openVersionControl(page);
  await expect(connectedDot(versionControl(page))).toHaveText("connected");
}

// ---------------------------------------------------------------------------------------------
// The page's parts, as locators.
// ---------------------------------------------------------------------------------------------

/** The crumbs of the outline's zoom: none while the outline draws the whole issue, and at least
 *  one once a twisty below 1024 px has zoomed it into a node. */
export function zoomCrumbs(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.crumb);
}

/** INV-3 (part N's R13, c-page-colours): the states of R7's seed whose every colour the row
 *  "every colour of the page and of Details is a page colour" reads, each at 1440 px and at
 *  390 px, in light and in dark, with the header's pull request link drawn in each: Conversation,
 *  with Details open at its history, its people's two lists, the facts of a focused row with what
 *  references it, and its Referenced by list; Dashboard, which the issue's bare address draws;
 *  Documents' list; and a view's loading line, Verification's while its reads are held. */
export const COLOUR_STATES = ["conversation", "dashboard", "documents", "loading"] as const;

/** The elements drawn in a colour a person or an agent chose, which R13 lets the page draw (a
 *  label's or a project's own colour, a picture): none. Dispatch stores no colour of a label or
 *  of a project - no read of its API carries one, and the page draws every label in its own
 *  `pageLabelChip` - and a picture's pixels are no colour the browser computes for its element,
 *  so no element is left out. A part that lets a person choose a colour names the element it
 *  draws that colour on here, by its selector. */
const CHOSEN_COLOURS: readonly string[] = [];

/** Each colour an element of the new page (`[data-issue-page]`) or of Details' drawer
 *  (`aside[data-details]`) draws that is none of B's `PAGE_COLORS` in `scheme`, not transparent,
 *  and on no element of `CHOSEN_COLOURS` (part N's R13), named with the element and the property
 *  it is drawn in (`offPalette`). A closed Details is hidden and draws nothing. */
export async function pageOffThePalette(page: Page, scheme: "dark" | "light"): Promise<string[]> {
  return offPalette(
    await drawnColours(page, [SELECTORS.issuePage, SELECTORS.detailsDrawer], CHOSEN_COLOURS),
    scheme
  );
}

// ---------------------------------------------------------------------------------------------
// Opening the page.
// ---------------------------------------------------------------------------------------------

/** Opens one of the addresses the old tabs answered, from wherever the page is on that issue:
 *  every one of them still resolves (R19, c-urls), and a spec that only needs to be there goes
 *  straight to it rather than pressing a control that no longer exists. The move is inside the
 *  page, as the tab it replaces was: a reload would drop a staged upload, a held transport and
 *  everything else the page is holding. */
export async function openIssueAddress(page: Page, tab: string): Promise<void> {
  const key = new URL(page.url()).pathname.match(/^\/issues\/([A-Z][A-Z0-9]*-\d+)/)?.[1] ?? "";
  await navigateInApp(page, `/issues/${key}/${tab}`);
}

/**
 * One link of the old page from a node to another node, which the new page carries by drawing
 * the target as that node's parent row, the row the left arrow reaches from it - the one form of
 * carrying an entry that is no landmark and no control of its own. A decision's card linked the
 * ask it answers; the outline draws the decision under that ask instead (R4), so the link's job
 * is done by the tree itself, and the entry is carried exactly when the tree does it.
 */
export interface ParentRowMapping {
  /** Words of the node the old link sat on, which its row holds. */
  readonly node: string;
  /** Words of the node the old link pointed at, which the node's parent row holds. */
  readonly parent: string;
  /** Why the parent row stands in for the link. */
  readonly reason: string;
}

/**
 * One entry of the old page that is the job of a row of the outline on the new page, which the
 * entry's own role and name no longer show: a link to a node that is now the node's row, a
 * thread's header that is now the comment's row, an event's link to a comment that is now the
 * comment's row (the Design's "Where each thing goes"). The entry is carried exactly when a row
 * of the outline that holds `words` is drawn in one of the views the check reads.
 */
export interface CarriedByRow {
  /** Why the row stands in for the entry. */
  readonly reason: string;
  /** Words of the node whose row carries the entry, which that row holds. */
  readonly words: string;
}

export interface CarriedOptions {
  /**
   * The fixture's rename table: each recorded entry, written as the `role "name"` a failure
   * prints it as, mapped to the entry the page offers in its place, in the same form. One form
   * serves a name that changed and a role that changed with it - `tab "Children"` became
   * `button "Children"` when the tabs became presets - and a rename is pasted straight from
   * the failure that named it. Where the page offers a different entry at a different width -
   * the preset row is a row of tabs on a desktop and one menu on a phone - the value names
   * each of them, joined by ` | `, and the entry is carried while any one of them is there. A
   * renamed entry is looked for as the entries it maps to and nothing else, so a rename is not
   * reported as a loss; a value that is not that form fails the check rather than quietly
   * matching nothing.
   */
  readonly renames: Readonly<Record<string, string>>;
  /** Recorded entries, in the same `role "name"` form, that only a part not yet on the page
   *  carries (`AWAITING_THREADS`, for Threads, dispatch://INFRA-5704): none is looked for, and
   *  one the page offers fails the check, since that part's own pull request is what moves it
   *  back into the reading. */
  readonly awaiting?: readonly string[];
  /**
   * The fixture's links carried by a parent row (`ParentRowMapping`), keyed by the recorded
   * entry in the same `role "name"` form. Such an entry is looked for in the outline's
   * `treeitem` rows and nowhere else: it is carried when a row holding the node's words is drawn
   * under a row holding the target's words, one level up, and the left arrow from that row
   * reaches that parent. Every other entry is read as a landmark or a control, as above.
   */
  readonly parentRows?: Readonly<Record<string, ParentRowMapping>>;
  /**
   * The fixture's entries carried by a row of the outline (`CarriedByRow`), keyed by the
   * recorded entry in the same `role "name"` form. Such an entry is looked for in the rows of the
   * outline and nowhere else.
   */
  readonly rows?: Readonly<Record<string, CarriedByRow>>;
  /** Puts the page where the outline draws the rows a parent-row mapping names, both of them:
   *  the walk's own views leave the page wherever the last one did. It returns once that outline
   *  is drawn - a load whose reads went quiet with every branch open, or below 1024 px the
   *  parent's own zoom drawn - since its rows are read the moment it returns. */
  readonly outline?: (mapping: ParentRowMapping) => Promise<void>;
  /** Which recorded state this is (`open-1440`), named in every failure. */
  readonly state: string;
  /**
   * The page's own views, each a call that puts the page in one of them and answers whether
   * that view is there. The recording is the merge of the views the old page had - its nine
   * tab panels, one at a time, because a page renders one and hides the rest
   * (`record-page-inventory.e2e.ts`) - so the page it is read against is merged the same way,
   * or a page that still offers everything would be read as having lost whatever its first
   * view does not show. Each view is read in turn and the readings merged, and every one of
   * them passes `assertPageFits` on its own.
   *
   * A view the page does not have at this width - the eighth row of a tree that draws five, a
   * control a wider page never folds away - answers `false` and leaves the page exactly as the
   * view before it left it, and the walk reads nothing for it: a reading there would record the
   * view before it a second time, under its own settle and its own fit. So a walk may name
   * more views than a width has without paying for the ones it does not, and a walk that
   * reaches no view at all fails the check below for want of anything to merge.
   *
   * A view that only moved the focus to one row of a tree a view before it loaded and waited
   * out answers that row instead of `true`. The focus draws nothing outside the row: the body
   * and the acts it opens are drawn inside it (`NodeRow.tsx`'s `NodeBody` and `RowActs`), from
   * reads the page has already answered. So the row is read alone, once it holds its tree's
   * one tab stop, which its tree gives it in the commit that draws its body, and read once,
   * with no settle: the rest of the page is as the views before it read it. Its boxes are the
   * page's own (`getBoundingClientRect`), so the sideways rule below reads them as it reads a
   * whole page's, and the page is still fitted to its viewport first.
   */
  readonly views: readonly (() => Promise<boolean | Locator>)[];
}

/** How many presses of the left arrow reach a parent row: a row that holds rows of its own and
 *  is open folds on the first press, and moves to its parent on the second. */
const LEFT_ARROW_PRESSES = 2;

/**
 * Whether the outline carries one parent-row mapping: a `treeitem` holding the node's words,
 * drawn one level under the nearest `treeitem` above it that holds the target's words, from
 * which the left arrow moves the focus to that parent. Undefined where it does, and otherwise
 * what the rows showed instead.
 */
async function parentRowMissing(
  page: Page,
  mapping: ParentRowMapping
): Promise<string | undefined> {
  const rows = page.getByRole("treeitem");
  // The outline callback returns once the outline it opens is drawn (`CarriedOptions.outline`),
  // so one read lists every row it draws.
  const drawn = await rows.evaluateAll((items) =>
    items.map((item) => ({
      level: Number(item.getAttribute("aria-level")),
      text: (item.textContent ?? "").replace(/\s+/g, " "),
    }))
  );
  const placements = drawn.flatMap((row, at) => {
    if (!row.text.includes(mapping.node)) return [];
    let parent = at - 1;
    while (parent >= 0 && drawn[parent]?.level !== row.level - 1) parent -= 1;
    return [{ at, parent }];
  });
  if (placements.length === 0) return `no row of the outline holds "${mapping.node}"`;
  const under = placements.find(
    ({ parent }) => drawn[parent]?.text.includes(mapping.parent) === true
  );
  if (under === undefined) {
    const parents = placements.map(({ parent }) => drawn[parent]?.text ?? "the outline's top");
    return `"${mapping.node}" is drawn under ${JSON.stringify(parents)}, not under "${mapping.parent}"`;
  }
  await rows.nth(under.at).focus();
  for (let press = 0; press < LEFT_ARROW_PRESSES; press += 1) {
    await page.keyboard.press("ArrowLeft");
    // Where the whole tree is drawn the arrow moves the focus inside its own keydown, and in the
    // parent's zoom the callback opened below 1024 px it moves none, so one read is settled.
    const reached = await rows
      .nth(under.parent)
      .evaluate((row) => row === document.activeElement || row.contains(document.activeElement));
    if (reached) return undefined;
  }
  // Below 1024 px the tree is read one node at a time, and a node's parent is the zoom's own first
  // row, drawn above it: the key moves nothing there and says so at the row (`useTreeKeymap`'s
  // `tree-fold`), which is the parent the mapping names reached, since it is the row the page is
  // zoomed to.
  const said = await rows.nth(under.at).locator("[data-row-report]").allTextContents();
  if (under.parent === 0 && said.includes(KEY_REPORTS.parent_zoomed)) return undefined;
  return `the left arrow from "${mapping.node}" does not reach its parent row "${mapping.parent}"`;
}

/** What one row offers once a view moved the focus to it on a settled page
 *  (`CarriedOptions.views`): its `treeitem` and every landmark and control inside it, read once
 *  the row holds its tree's one tab stop, which the tree gives it in the commit that draws what
 *  the focus opens in it. */
async function focusedRowEntries(row: Locator): Promise<MeasuredEntry[]> {
  await expect(
    row,
    "the row a view focused never took its tree's tab stop, so its focus was never drawn"
  ).toHaveAttribute("tabindex", "0");
  return measured(row.ariaSnapshotJSON({ boxes: true }), true);
}

/**
 * Nothing recorded was lost: every recorded `{role, name}`, under the name the rename table gives
 * it, is a landmark or control of the page as it stands, and a reader reaches each of them
 * without scrolling sideways - the page is no wider than its viewport (`assertPageFits`) and
 * every instance of the entry sits inside the viewport's own width, so nothing is reached by
 * scrolling a row or a card sideways.
 *
 * What a state lost fails the row softly (`expect.soft`): the row goes on to the next state it
 * reads and fails at its end, so one run names the losses of every state rather than those of
 * the first state that lost anything.
 *
 * It asserts nothing about the redesign: a page may offer more than it did, in any order.
 */
export async function assertInventoryCarried(
  page: Page,
  recorded: readonly InventoryEntry[],
  options: CarriedOptions
): Promise<void> {
  // One row per recorded entry, each naming every entry that carries it: itself, or whatever
  // the rename table says stands in its place. An entry a parent row carries is read off the
  // outline's rows below, and is no row of this list.
  const seen = new Set<string>();
  const wanted: { carried: InventoryEntry[]; described: string; instances: MeasuredEntry[] }[] = [];
  const byParentRow: { described: string; mapping: ParentRowMapping }[] = [];
  const byRow: { described: string; row: CarriedByRow }[] = [];
  for (const entry of recorded) {
    const described = describe(entry);
    if (seen.has(described)) continue;
    seen.add(described);
    if (options.awaiting?.includes(described) === true) continue;
    const mapping = options.parentRows?.[described];
    if (mapping !== undefined) {
      byParentRow.push({ described, mapping });
      continue;
    }
    const row = options.rows?.[described];
    if (row !== undefined) {
      byRow.push({ described, row });
      continue;
    }
    const renamed = options.renames[described];
    const carried =
      renamed === undefined ? [entry] : renamed.split(" | ").map((one) => entryOf(one));
    expect(
      carried.every((one) => one !== undefined),
      `${options.state}: the rename of ${described} reads ${JSON.stringify(renamed)}, which is no \`role "name"\``
    ).toBe(true);
    wanted.push({
      carried: carried.filter((one): one is InventoryEntry => one !== undefined),
      described,
      instances: [],
    });
  }
  const present = new Map<string, MeasuredEntry[]>();
  let read = 0;
  for (const view of options.views) {
    const shown = await view();
    if (shown === false) continue;
    await assertPageFits(page);
    read += 1;
    for (const entry of shown === true
      ? await settled(() => measured(page.ariaSnapshotJSON({ boxes: true }), true))
      : await focusedRowEntries(shown)) {
      const key = entryKey(entry);
      present.set(key, [...(present.get(key) ?? []), entry]);
    }
  }
  expect(
    read,
    `${options.state}: every one of the ${options.views.length} views answered that it is not there, so nothing was read`
  ).toBeGreaterThan(0);
  for (const one of wanted) {
    one.instances = one.carried.flatMap((entry) => present.get(entryKey(entry)) ?? []);
  }
  const lost = wanted.filter((one) => one.instances.length === 0).map((one) => one.described);
  for (const { described, row } of byRow) {
    const held = [...present.keys()].some(
      (key) => key.startsWith("treeitem\u0000") && key.includes(row.words)
    );
    if (!held) lost.push(`${described} (no row of the outline holds "${row.words}")`);
  }
  // Each link a parent row carries, read once the views are done: the left arrow moves the
  // focus and may fold a row, which no view's own reading should meet.
  for (const { described, mapping } of byParentRow) {
    expect(
      options.outline,
      `${options.state}: ${described} is carried by a parent row, and the check names no way to the outline`
    ).toBeDefined();
    await options.outline?.(mapping);
    const missing = await parentRowMissing(page, mapping);
    if (missing !== undefined) lost.push(`${described} (${missing})`);
  }
  expect.soft(lost, `${options.state} no longer offers: ${lost.join(", ")}`).toEqual([]);
  const early = (options.awaiting ?? []).filter((described) => {
    const entry = entryOf(described);
    return entry !== undefined && (present.get(entryKey(entry)) ?? []).length > 0;
  });
  expect
    .soft(
      early,
      `${options.state} offers what only a part not yet on the page carries (Threads): ${early.join(", ")}`
    )
    .toEqual([]);

  const viewport = await page.evaluate(() => document.documentElement.clientWidth);
  const sideways = wanted
    .filter((one) => {
      // A control a page hides from sight and keeps for the keyboard - the shell's "Skip to
      // content" and Documents' file input, which are `sr-only` until they are focused - is a box
      // one pixel left of its container (`-m-px`), 1 by 1 and clipped, and below 1280 px
      // `styles.css`'s 44 px minimum makes it 44 by 44. It is reached by Tab and not by
      // scrolling, which cannot reach left of the page's start either, so that one pixel says
      // nothing about scrolling sideways and the rule passes over it.
      const placed = one.instances.filter(
        (instance) =>
          instance.box !== undefined && instance.box.width > 1 && instance.box.height > 1
      );
      return (
        placed.length > 0 &&
        placed.every(({ box }) => box !== undefined && (box.x < -1 || box.x + box.width > viewport))
      );
    })
    .map((one) => one.described);
  expect(
    sideways,
    `${options.state} reaches these only by scrolling sideways in a ${viewport}px viewport: ${sideways.join(", ")}`
  ).toEqual([]);
}

/**
 * The rename table of the inventory row (part N's R7): each entry today's page offered, written
 * as the `role "name"` a failure prints it as, mapped to what the new page offers in its place,
 * from the Design's table "Where each thing goes" and ruling 5, and with root version 183's
 * renames: tab Artifacts to Documents (R2); part M's four, the Activity tab to History, the
 * Collaborators tab, the Steps tab to the steps in Details and a block's "Referenced by" to
 * "What references this row"; the margin's Comments to the margin beside their node; part L's,
 * today's composer's form to the box at the foot's, Draft; and dispatch://INFRA-5702's (#5545),
 * today's Dashboard tab to the view row's Dashboard control, its first. Each rename's reason is
 * in `INVENTORY_RENAME_REASONS`, under the same key. The fixture is recorded again after each
 * merge of `main`; the table is declared here, so a new recording does not lose it.
 *
 * The ask body of dispatch://INFRA-5702, which this stack holds, draws an ask on every surface of
 * the new page, and since #5545 `main`'s today page draws the same ask body: both pages offer
 * its Decline ("Decline to answer"), Hand on ("Hand this question to"), settled ask's record
 * ("Change answer") and answer box under the options by the same names, so each of its
 * controls stands unrenamed. Part M's Details facts of the focused ask carry an ask's edit
 * history, recipients and copy handles under their own names, which the fixture holds unchanged.
 */
export const INVENTORY_RENAMES: Readonly<Record<string, string>> = {
  'tab "Dashboard"': 'button "Dashboard" | menuitemradio "Dashboard"',
  'tab "Spec"': 'button "Spec" | menuitem "Spec" | menuitemradio "Spec"',
  'tab "Verification"':
    'button "Verification" | menuitem "Verification" | menuitemradio "Verification"',
  'tab "Plan"': 'button "Plan" | menuitem "Plan" | menuitemradio "Plan"',
  'tab "Steps"': 'region "Steps"',
  'region "Steps empty state"': 'region "Steps"',
  'tab "Conversation"': 'button "Conversation" | menuitem "Conversation"',
  'region "Conversation"': 'button "Conversation" | menuitem "Conversation"',
  'tab "Artifacts 7"': 'button "Documents" | menuitem "Documents"',
  'tab "Activity"': 'region "History"',
  'region "Activity"': 'region "History"',
  'tab "Decisions"': 'button "Decisions" | menuitem "Decisions" | menuitemradio "Decisions"',
  'tab "Collaborators"': 'button "Collaborators"',
  'button "All"': 'button "Filter"',
  'button "Open"': 'button "Filter"',
  'button "Closed"': 'button "Filter"',
  'button "Show fields"': 'button "Hide fields"',
  'button "Referenced by (1)"': 'button "What references this row" | button "Referenced by"',
  'button "Referenced by (14)"': 'button "Referenced by"',
  'button "Write a reply"': 'button "Reply"',
  'form "Comment composer"': 'form "Draft"',
  'button "Copy reference dispatch://LAY-2/comment/<id>"': 'button "Copy reference"',
  'button "Copy reference dispatch://LAY-2/message/<id>"': 'button "Copy reference"',
  'button "Reopen issue"': 'button "Reopen into Backlog"',
  'button "Upload artifact"': 'button "Upload a file"',
  'searchbox "Filter artifacts"': 'searchbox "Filter files and documents"',
  'combobox "Repository of LAY-2"': 'button "acme/issue-layout-<run>, Repository of LAY-2"',
  'link "#11 acme/issue-layout-<run>"': 'link "#11 Merged"',
  'link "#2 Keep every old address, inherited"': 'link "#2 Keep every old address"',
  'region "Open questions"': 'region "Decisions"',
  'region "Waiting for your review"': 'region "Decisions"',
  'region "Your decisions"': 'region "Decisions"',
  'region "Replaced"': 'region "Decisions"',
  'button "Hide margin"': 'button "Close"',
  'button "Hide sidebar"': 'button "Open navigation"',
  'complementary "Navigation"': 'navigation "App"',
  'banner ""': 'navigation "Navigation"',
  'button "Open review panel (1 open ask)"': 'button "Details"',
  'button "Open review panel (0 open asks)"': 'button "Details"',
  'complementary "Review panel"': 'complementary "Review margin"',
};

/** Why a child of the issue's link is the child's row on the new page. */
const CHILD_ROW_REASON =
  "The children are rows of the outline, not a section of their own (the architect's ruling that no children's section is drawn, root version 183's 'Children tab → the children's section'): a child's link is carried by the child's row, whose words are its title.";

/** Why the conversation's All, Open and Closed are one control on the new page. */
const FILTER_REASON =
  "The conversation's All, Open and Closed choice is the filter by words, which also matches a node's state word such as Open or Resolved (the Design's table 'Where each thing goes').";

/** Why a name of the Repository control is the row's text button on the new page. */
const REPOSITORY_REASON =
  "Details draws the repository as text, whose button opens the box and its clear control in its place (`RepoControl`'s `inRow`); the row's one control is that button, named by the repository and 'Repository of <key>' (R13).";

/** Why a name is part M's, from the Details panel (root version 183). */
const DETAILS_FACTS_REASON =
  "A block's 'Referenced by' list stands in the focused node's facts in Details as 'What references this row', and the issue's own is Details' 'Referenced by' row, which draws no count until its list is read (part M, root version 183).";

/** Why a card's copy control is the row's Copy reference. */
const COPY_REFERENCE_REASON =
  "A comment's or a message's copy control is the Copy reference of its row, which copies the node's address and is named by its words alone (part G's R6, root R19).";

/** Why a rename is the same control under a new name, keyed as `INVENTORY_RENAMES` is. */
export const INVENTORY_RENAME_REASONS: Readonly<Record<string, string>> = {
  'tab "Dashboard"':
    "Today's Dashboard tab (dispatch://INFRA-5702, #5545) is the Dashboard control of the view row, its first control, which the issue's bare address opens (R2, dispatch://INFRA-5702/spec#b-r-new-page): a button in the toolbar and a menu item in the one control.",
  'tab "Spec"':
    "The Spec tab is the Spec view of the view row (R2), a button in the toolbar and a menu item in the one control.",
  'tab "Verification"': "The Verification tab is the Verification view of the view row (R2).",
  'tab "Plan"': "The Plan tab is the Plan view of the view row (R2).",
  'tab "Steps"':
    "The Steps tab is the Steps section of Details, under the history (part M, root version 183).",
  'region "Steps empty state"':
    "The Steps tab's empty line is the Steps section's own, in Details (part M).",
  'tab "Conversation"':
    "The Conversation tab is Conversation, the control of the view row that turns every view off (R2).",
  'region "Conversation"':
    "The Conversation tab's panel is Conversation, the control of the view row that turns every view off (R2).",
  'tab "Artifacts 7"':
    "The Artifacts tab is Documents (root version 183, R2): the view row's control for the issue's documents and files.",
  'tab "Activity"': "The Activity tab is Details' History (part M, root version 183).",
  'region "Activity"': "The Activity tab's list is Details' History (part M, root version 183).",
  'tab "Decisions"': "The Decisions tab is the Decisions view of the view row (R2).",
  'tab "Collaborators"':
    "The Collaborators tab is Details' Collaborators control (part M, root version 183).",
  'button "All"': FILTER_REASON,
  'button "Open"': FILTER_REASON,
  'button "Closed"': FILTER_REASON,
  'button "Show fields"':
    "The conversation's handoff cards are the history's handoff lines, whose fields are drawn open until the reader folds them, so the toggle reads Hide fields (R15, `HistoryHandoff`).",
  'button "Referenced by (1)"': DETAILS_FACTS_REASON,
  'button "Referenced by (14)"': DETAILS_FACTS_REASON,
  'button "Write a reply"':
    "A collapsed thread's Write a reply is the Reply of the row that takes a reply, which sets the line of the box at the foot of the page (R16, part L's `IssueComposer`).",
  'form "Comment composer"':
    "Today's composer is the box at the foot of the page (part L's `IssueComposer`, dispatch://INFRA-3949), whose form a screen reader reads by the page's word for it, Draft (`CONTROL_LABELS.composer_form`, decision 527); its field and its Send keep their names, Comment and Send.",
  'button "Copy reference dispatch://LAY-2/comment/<id>"': COPY_REFERENCE_REASON,
  'button "Copy reference dispatch://LAY-2/message/<id>"': COPY_REFERENCE_REASON,
  'button "Reopen issue"':
    "Close and Reopen are behind the identity block's one actions control (R11).",
  'button "Upload artifact"':
    "The upload's file chooser is Documents' own (R2), named by its page word.",
  'searchbox "Filter artifacts"':
    "The Artifacts list's filter is Documents' filter (R2), named by its page word.",
  'combobox "Repository of LAY-2"': REPOSITORY_REASON,
  'link "#11 acme/issue-layout-<run>"':
    "The header's pull request link names its number and how it stands, Open, Merged or Closed (R22), where it named its number and repository.",
  'link "#2 Keep every old address, inherited"':
    "A priority the issue advances because an issue above it does is named by the priority alone; the row marks it inherited in words beside the link (R13).",
  'region "Open questions"':
    "The Decisions tab's four lists are the Decisions view's own parts (R2).",
  'region "Waiting for your review"': "As above: the part for a decision waiting on the reader.",
  'region "Your decisions"': "As above: the reader's own decisions.",
  'region "Replaced"': "As above: the decisions another replaced.",
  'button "Hide margin"':
    "The margin's collapse is Details' Close now: the panel opens over the right of the page as a drawer and closing it is its collapse (R14), the same control under the drawer's word.",
  'button "Hide sidebar"':
    "On the issue page the sidebar is the 56 px icon rail (R14), already collapsed; its one control over the navigation is the rail's foot, Open navigation, which opens the whole list over the page, or the header's on a phone.",
  'complementary "Navigation"':
    "The sidebar's landmark on the issue page is the icon rail's (R14), a navigation landmark named App so it is not taken for the whole navigation the rail's foot opens.",
  'banner ""':
    "On a phone the shell's bar was the issue page's banner, holding Dispatch, the inbox count and Menu; on the issue page the header's `☰` opens the navigation that holds them (R14), so the bar is that navigation now.",
  'button "Open review panel (1 open ask)"':
    "The phone's control that opened the panel is the header's Details control (R14); the panel it opens is the same.",
  'button "Open review panel (0 open asks)"':
    "As above, on the closed issue, which waits on no ask.",
  'complementary "Review panel"':
    "The phone's review sheet is the margin inside Details, which on the issue page is one box at every width and named as the margin is (R14); Details opens it over the whole screen.",
};

/** Links of today's page that a parent row of the outline carries (`ParentRowMapping`), keyed
 *  as `INVENTORY_RENAMES` is. */
export const INVENTORY_PARENT_ROWS: Readonly<Record<string, ParentRowMapping>> = {
  'link "The ask it answers"': {
    node: "Keep every address the old page answered",
    parent: "Which addresses must the new page still answer?",
    reason:
      "A decision's card linked the ask it answers; the outline draws the decision under that ask, its parent row, which the left arrow reaches (R4).",
  },
};

/** Why a link to a node of the old page's lists is the node's row on the new page. */
const NODE_ROW_REASON =
  "A link to a node's own block in its document is the node's row in the outline, which focus reaches (the Design's table 'Where each thing goes').";

/** Why a thread's header is the comment's row on the new page. */
const THREAD_ROW_REASON =
  "The spec conversation's threads are rows of the outline, a comment with its replies under its twisty (R1); the old thread card's header is the comment's row.";

/** Why a reply's link is the reply's row on the new page. */
const REPLY_ROW_REASON =
  "A reply the old thread card linked is a row of the outline, under its comment's twisty (R1).";

/** The entries of today's page that a row of the outline carries (`CarriedByRow`), keyed as
 *  `INVENTORY_RENAMES` is. */
export const INVENTORY_ROWS: Readonly<Record<string, CarriedByRow>> = {
  'tab "Children"': {
    reason:
      "The Children tab is the issue's children as rows of the outline (the architect's ruling that no children's section is drawn, root version 183's 'The children's section').",
    words: "Child 1 of the redesign",
  },
  'button "Expand thread"': { reason: THREAD_ROW_REASON, words: "Thread 1 opens here." },
  'link "View in document"': {
    reason: NODE_ROW_REASON,
    words: "This requirement is the one the tree reads first.",
  },
  'link "LAY-3 · Child 1 of the redesign"': {
    reason: CHILD_ROW_REASON,
    words: "Child 1 of the redesign",
  },
  'link "LAY-4 · Child 2 of the redesign"': {
    reason: CHILD_ROW_REASON,
    words: "Child 2 of the redesign",
  },
  'link "LAY-5 · Child 3 of the redesign"': {
    reason: CHILD_ROW_REASON,
    words: "Child 3 of the redesign",
  },
  'link "LAY-6 · Child 4 of the redesign"': {
    reason: CHILD_ROW_REASON,
    words: "Child 4 of the redesign",
  },
  'link "LAY-7 · Child 5 of the redesign"': {
    reason: CHILD_ROW_REASON,
    words: "Child 5 of the redesign",
  },
  'link "LAY-8 · Child 6 of the redesign"': {
    reason: CHILD_ROW_REASON,
    words: "Child 6 of the redesign",
  },
  'link "LAY-9 · Child 7 of the redesign"': {
    reason: CHILD_ROW_REASON,
    words: "Child 7 of the redesign",
  },
  'button "Thread 1 opens here. Its second paragraph is a block of its own. 2 replies · last reply <time>"':
    { reason: THREAD_ROW_REASON, words: "Thread 1 opens here." },
  'button "Thread 2 opens here. Its second paragraph is a block of its own. 1 reply · last reply <time>"':
    { reason: THREAD_ROW_REASON, words: "Thread 2 opens here." },
  'link "— The fold budget counts no question."': {
    reason: REPLY_ROW_REASON,
    words: "The fold budget counts no question.",
  },
  'link "— Which is why the deepest question is still on the first screen."': {
    reason: REPLY_ROW_REASON,
    words: "Which is why the deepest question is still on the first screen.",
  },
  'link "view"': {
    reason:
      "The Activity tab's link to the comment an event made: the history leaves out each event of a row the outline draws, a comment created among them, so the comment's row is where it stands (`history-model.ts`, part M's R15).",
    words: "Thread 1 opens here.",
  },
  'link "Recorder (issue layout)"': {
    reason:
      "The Activity tab's link to the session that recorded a result: the history leaves out a result recorded against a check, so the result's row, which names who wrote it, is where it stands (`history-model.ts`, part M's R15).",
    words: "Written by an agent Recorder (issue layout)",
  },
  'link "Requirement 6 of the redesign stands on a row of its own."': {
    reason: NODE_ROW_REASON,
    words: "Requirement 6 of the redesign stands on a row of its own.",
  },
  'link "Requirement 7 of the redesign stands on a row of its own."': {
    reason: NODE_ROW_REASON,
    words: "Requirement 7 of the redesign stands on a row of its own.",
  },
  'link "Requirement 8 of the redesign stands on a row of its own."': {
    reason: NODE_ROW_REASON,
    words: "Requirement 8 of the redesign stands on a row of its own.",
  },
  'link "Task 1 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 1 of the redesign is one step of the build.",
  },
  'link "Task 2 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 2 of the redesign is one step of the build.",
  },
  'link "Task 3 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 3 of the redesign is one step of the build.",
  },
  'link "Task 4 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 4 of the redesign is one step of the build.",
  },
  'link "Task 5 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 5 of the redesign is one step of the build.",
  },
  'link "Task 6 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 6 of the redesign is one step of the build.",
  },
  'link "Task 7 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 7 of the redesign is one step of the build.",
  },
  'link "Task 8 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 8 of the redesign is one step of the build.",
  },
  'link "Task 9 of the redesign is one step of the build."': {
    reason: NODE_ROW_REASON,
    words: "Task 9 of the redesign is one step of the build.",
  },
};

/**
 * What only part L's box carries (part N's R11). Part L is on `main`, and `slots/foot.tsx` draws
 * its box at the foot of the page, so the list is empty: the inventory row reads each name of
 * today's composer again, as the fixture recorded it in every state, in the box or under the
 * name `INVENTORY_RENAMES` gives it there. The message route's button, `Messages default to
 * <agent>`, is no name of the box: Details' Route row carries it (R13), so it is read there.
 */
export const AWAITING_THE_BOX: readonly string[] = [];

/**
 * What only Threads carries (part N's R11), dispatch://INFRA-5704, which follows this part: the
 * names of today's page, as the fixture records them, whose only new home is Threads. The
 * inventory row looks for none of them and fails if the page offers one; Threads' pull request
 * empties this list, and the row then reads each of them again. It is empty: the fixture holds
 * no name that is New thread's alone. New thread, the control that sends a message to an agent
 * with an @ mention from Threads, is a control of the new page that the old page did not draw,
 * so no recording holds it; today's composer, which also sends such a message, is carried by
 * part L's box, which is on `main` and offers its names, so listing them here would make this
 * row fail.
 */
export const AWAITING_THREADS: readonly string[] = [];

// ---------------------------------------------------------------------------------------------
// INV-2: the states and widths of part N's R9.
// ---------------------------------------------------------------------------------------------

/** The header's size as root R13 states it: the height from the top of the identity block to the
 *  top of the filter row, and the lines of text and control values drawn between the two, each as
 *  the words it holds, left to right. */
export interface HeaderSize {
  readonly height: number;
  readonly lines: readonly string[];
}

/** Root R13's limit on the header at `width` (R22 repeats it for the pull request's link): at
 *  1440 px 3 lines and 130 px, at 390 px 6 lines and 180 px, none at any other width. */
export function headerLimit(
  width: number
): { readonly height: number; readonly lines: number } | undefined {
  if (width === 1440) return { height: 130, lines: 3 };
  if (width === 390) return { height: 180, lines: 6 };
  return undefined;
}

/**
 * The header as it is drawn now, with the page at its top: the lines are those of the text nodes
 * and the controls' values that stand between the top of the identity block and the top of the
 * filter row, each text that wraps counted line by line, and a line is every piece whose top
 * lies above the lowest bottom of the pieces gathered into it. Text laid out unseen (hidden,
 * transparent, or a clipped box a pixel wide or high) is left out, as are the options of a
 * select, which stand in the list and are drawn as its value. Null where the page draws no
 * header or no filter row.
 */
export function headerSize(page: Page): Promise<HeaderSize | null> {
  return page.evaluate((selectors) => {
    window.scrollTo(0, 0);
    const header = document.querySelector(`${selectors.issuePage} ${selectors.identityBlock}`);
    const band = header?.querySelector(selectors.identityBand) ?? null;
    const row = document.querySelector(`${selectors.issuePage} ${selectors.filterRow}`);
    if (header === null || band === null || row === null) return null;
    const top = band.getBoundingClientRect().top;
    const bottom = row.getBoundingClientRect().top;
    const inBand = (box: DOMRect): boolean =>
      box.width > 0 && box.height > 0 && box.bottom > top && box.top < bottom;
    const drawn = (element: Element): boolean => {
      for (let at: Element | null = element; at !== null; at = at.parentElement) {
        const style = getComputedStyle(at);
        if (style.visibility !== "visible" || style.opacity === "0") return false;
        const box = at.getBoundingClientRect();
        if (style.overflow !== "visible" && (box.width <= 1 || box.height <= 1)) return false;
      }
      return true;
    };
    const pieces: { bottom: number; left: number; text: string; top: number }[] = [];
    const range = document.createRange();
    const walker = document.createTreeWalker(header, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const content = node.textContent ?? "";
      const element = node.parentElement;
      if (content.trim() === "" || element === null || element.closest("select") !== null) continue;
      range.selectNodeContents(node);
      const boxes = [...range.getClientRects()].filter(inBand);
      if (boxes.length === 0 || !drawn(element)) continue;
      // A text that wraps is one box per line: each line is given the characters drawn in it.
      const words = boxes.length === 1 ? [content] : boxes.map(() => "");
      for (let at = 0; boxes.length > 1 && at < content.length; at += 1) {
        range.setStart(node, at);
        range.setEnd(node, at + 1);
        const glyph = range.getClientRects()[0];
        if (glyph === undefined) continue;
        const line = boxes.findIndex(
          (box) => glyph.top >= box.top - 1 && glyph.bottom <= box.bottom + 1
        );
        if (line >= 0) words[line] += content[at];
      }
      boxes.forEach((box, line) => {
        const text = (words[line] ?? "").replace(/\s+/g, " ").trim();
        pieces.push({ bottom: box.bottom, left: box.left, text, top: box.top });
      });
    }
    for (const control of header.querySelectorAll("select, input:not([type='hidden']), textarea")) {
      const box = control.getBoundingClientRect();
      if (!inBand(box) || !drawn(control)) continue;
      const size = Number.parseFloat(getComputedStyle(control).fontSize);
      const middle = (box.top + box.bottom) / 2;
      const text =
        control instanceof HTMLSelectElement
          ? (control.selectedOptions[0]?.textContent ?? "").trim()
          : (control as HTMLInputElement).value;
      pieces.push({ bottom: middle + size / 2, left: box.left, text, top: middle - size / 2 });
    }
    pieces.sort((a, b) => a.top - b.top);
    const lines: (typeof pieces)[] = [];
    let lineBottom = Number.NEGATIVE_INFINITY;
    for (const piece of pieces) {
      const line = lines.at(-1);
      if (line !== undefined && piece.top < lineBottom - 1) {
        line.push(piece);
        lineBottom = Math.max(lineBottom, piece.bottom);
      } else {
        lines.push([piece]);
        lineBottom = piece.bottom;
      }
    }
    return {
      height: bottom - top,
      lines: lines.map((line) =>
        line
          .sort((a, b) => a.left - b.left)
          .map((piece) => piece.text)
          .join(" ")
      ),
    };
  }, SELECTORS);
}

// ---------------------------------------------------------------------------------------------
// The count's presses (part N's R5, the plan's ST-3, `c-addresses` (3)).
// ---------------------------------------------------------------------------------------------

/** The reader the seed's items wait on, and the person who approved the Verification document. */
const COUNT_READER = "alice";

/** What the row of the count's presses reads: the issue whose count is six items of Dashboard's
 *  list. */
export interface WaitingWalkSeed {
  /** How many items wait on the reader, which the header's count and Dashboard's list agree on. */
  readonly items: number;
  readonly key: string;
}

/** Whether one entry of a decisions read of a `main` where decisions are nodes waits for the
 *  reader's review. */
function nodeEntryWaits(entry: unknown): boolean {
  return (
    typeof entry === "object" &&
    entry !== null &&
    "waiting_review" in entry &&
    entry.waiting_review === true
  );
}

/** Waits until the issue's decisions read holds one that waits for the reader's review, as the
 *  document service indexes a decision's links a moment after the write answers. */
async function awaitWaitingDecision(key: string, decisionsAreNodes: boolean): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const read = await getIssueDecisions(key);
    const waits = decisionsAreNodes
      ? "entries" in read && Array.isArray(read.entries) && read.entries.some(nodeEntryWaits)
      : (read.decisions ?? []).some(
          (decision) => decision.waiting_review && (decision.links ?? []).length > 0
        );
    if (waits) return;
    if (Date.now() > deadline) {
      throw new Error(`${key}'s decisions never read one waiting: ${JSON.stringify(read)}`);
    }
    await pause(100);
  }
}

/** Waits until the issue read gives the document `artifactId` the approval `state`, by `by` where
 *  one is named. */
async function awaitApprovalState(
  key: string,
  artifactId: string,
  state: string,
  by?: string
): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const approval = ((await getIssue(key)).artifacts ?? []).find(
      (artifact) => artifact.id === artifactId
    )?.approval;
    if (approval?.state === state && (by === undefined || approval.by?.id === by)) return;
    if (Date.now() > deadline) {
      throw new Error(`${key}'s document ${artifactId} reads ${JSON.stringify(approval)}`);
    }
    await pause(100);
  }
}

/** Waits until the reader's unread replies list each of `threads`. */
async function awaitUnreadThreads(key: string, threads: readonly string[]): Promise<void> {
  const deadline = Date.now() + 20_000;
  for (;;) {
    const rows = (await listMyReplies({ issue: key, state: "unread" })).rows ?? [];
    if (threads.every((thread) => rows.some((row) => row.thread_id === thread))) return;
    if (Date.now() > deadline) {
      throw new Error(
        `${key}'s unread threads read ${JSON.stringify(rows.map((row) => row.thread_id))}`
      );
    }
    await pause(100);
  }
}

/**
 * The row of the count's presses seeds six items that wait on the reader, no more: an open
 * question, a decision to review, an approval request on the spec, the reader's approval of the
 * Verification document that a later version made stale, and two threads with a reply the reader
 * has not read, one of comments and one of messages. Dashboard's own seed (`seedDashboard`) draws
 * nine, and the count of that seed is not filtered or mocked to six: this one writes the six.
 */
export async function seedWaitingWalk(): Promise<WaitingWalkSeed> {
  const holds = await mainHolds();
  await createProject({ key: PROJECT, name: "The count's presses" });
  const issue = await createIssue({
    project: PROJECT,
    spec: ":::requirement{#r1}\nThe header counts a decision that waits.\n:::\n",
    title: "The count walks Dashboard's list",
  });
  const key = issue.key;
  const specArtifactId = issue.primary_artifact_id ?? "";

  await createAsk(key, { question: "Which order does the list take?" }, asPlanner);

  const shaped = `dispatch://${key}/spec#b-r1`;
  if (holds.decisionsAreNodes) {
    await editArtifact(
      specArtifactId,
      {
        ops: [
          {
            after: "end",
            markdown: `## Decisions\n\n:::decision{#d-walk shaped="${shaped}"}\n### Count what waits by its items\n\nChosen so the count and the list agree.\n:::`,
            op: "insert",
          },
        ],
      },
      asPlanner
    );
  } else {
    await createIssueArtifact(
      key,
      {
        content: `## Count what waits by its items\n\nChosen so the count and the list agree. Shapes ${shaped}.\n`,
        name: "decisions.md",
      },
      asPlanner
    );
  }
  await awaitWaitingDecision(key, holds.decisionsAreNodes);

  await requestApproval(specArtifactId, {}, asPlanner);
  await awaitApprovalState(key, specArtifactId, "awaiting");

  const verification = await createIssueArtifact(
    key,
    { content: "# Verification\n\nOne check.\n", name: "verification.md" },
    asPlanner
  );
  await reviewArtifact(verification.artifact.id, { state: "approved" }, { login: COUNT_READER });
  await editArtifact(
    verification.artifact.id,
    { ops: [{ find: "One check.", op: "replace", with: "Two checks." }] },
    asPlanner
  );
  await awaitApprovalState(key, verification.artifact.id, "stale", COUNT_READER);

  const comment = await createComment(key, { body: "Does the count drop as soon as I act?" });
  await createComment(
    key,
    { body: "It drops when the write lands, with no reload.", reply_to: comment.id },
    asPlanner
  );
  const message = await createMessage(
    key,
    { body: "The tiles read the results and the coverage." },
    asPlanner
  );
  await createMessage(key, { body: "And the children?", in_reply_to: message.id });
  await createMessage(
    key,
    { body: "The children from the issue read.", in_reply_to: message.id },
    asPlanner
  );
  await awaitUnreadThreads(key, [comment.id, message.id]);
  return { items: 6, key };
}

// ---------------------------------------------------------------------------------------------
// R21: the load bound on the largest issues (`c-scale-whole`, the plan's g14).
// ---------------------------------------------------------------------------------------------

/** R21's numbers, which no copy, control or width moves. */
const SCALE_LIMITS = {
  /** The rows Dashboard's list draws before the control that draws its next five. */
  dashboardPage: 5,
  /** The rows a list of Details draws before the control that draws its next 20. */
  detailsPage: 20,
  /** The rows Documents' list draws before the control that draws its next 20. */
  documentsPage: 20,
  /** Milliseconds to the first byte of a read's answer, at the 90th percentile of its timed
   *  sends. */
  firstByteMs: 200,
  /** The entries the history draws before the control that draws its next 20. */
  historyPage: 20,
  /** Bytes of JSON one cold load receives, summed from the decoded bodies of its answers: 2 MB. */
  jsonBytes: 2_000_000,
  /** Requests under `/api/` one cold load sends between navigation and network idle. */
  requests: 40,
  /** How many times the row sends each read of a load through the page's own session... */
  sends: 35,
  /** ...of which the first are warm-ups, left out of its times: 30 are timed. */
  warmUps: 5,
} as const;

/**
 * The bounds of R21 that a part's code holds, by the names `AWAITING_SCALE` gives them and a
 * failure prints: Dashboard's list of five (dispatch://INFRA-5702); Documents' list of 20 (R2's);
 * part M's (dispatch://INFRA-3948), the history's 20 entries and Details' lists of 20, which the
 * row reads in Details opened after the Conversation's cold load, since Details draws closed on
 * every load (`openDetails` in `e2e/issue-page.ts`); and part T's (dispatch://INFRA-4062): the
 * comments read 200 at a time and, on each load whose tree holds every comment, its 2 MB, its 40
 * requests and the 200 ms of its names reads (`GET /api/v1/reference-targets`), a node's first 50
 * children and the children's section's 20 rows. Threads' 20 rows wait with `AWAITING_THREADS`
 * (R11): the view row draws no Threads on this branch.
 */
const SCALE_BOUNDS = [
  "Dashboard's list",
  "Documents' list",
  "the history",
  "Details' lists",
  "the comments read",
  "the 2 MB of a load whose tree holds every comment",
  "the 40 requests of a load whose tree holds every comment",
  "the 200 ms of the names reads of a load whose tree holds every comment",
  "a node's first 50 children",
  "the children's section's 20 rows",
] as const;

export type ScaleBound = (typeof SCALE_BOUNDS)[number];

/** The bounds of `SCALE_BOUNDS` the row reads wherever `AWAITING_SCALE` does not name them. A
 *  node's first 50 children and the children's section's 20 rows are not among them: the outline
 *  of this branch draws every child of a node and no children's section, so the control their
 *  reading finds is part T's own, and part T's pull request adds their reading here as it takes
 *  their entries off `AWAITING_SCALE`. Until one of the two holds each bound, the row fails
 *  (`unheldScaleBounds`). */
const SCALE_BOUNDS_READ: readonly ScaleBound[] = [
  "Dashboard's list",
  "Documents' list",
  "the history",
  "Details' lists",
  "the comments read",
  "the 2 MB of a load whose tree holds every comment",
  "the 40 requests of a load whose tree holds every comment",
  "the 200 ms of the names reads of a load whose tree holds every comment",
];

/** How the row finds that a bound's owner's code is under the tested head: a read that only the
 *  owner's code makes, in the load of any of `controls`; a control that only the owner's code
 *  draws, in the load of `preset`; or one that it draws in Details, which the row opens after the
 *  Conversation's cold load (`readScaleLoad`). */
export type OwnerSign =
  | {
      readonly controls: readonly Preset[];
      readonly kind: "read";
      readonly method: string;
      /** The read's route as `apiRoute` (`e2e/traffic.ts`) writes it. */
      readonly route: string;
    }
  | {
      readonly control: (page: Page) => Locator;
      readonly kind: "control";
      readonly preset: Preset;
    }
  | {
      readonly control: (page: Page) => Locator;
      readonly kind: "details";
    };

/** One bound whose owner's code is not under the tested head (`AWAITING_SCALE`). */
export interface AwaitedBound {
  readonly bound: ScaleBound;
  /** The part that brings the bound's code. */
  readonly owner: string;
  readonly sign: OwnerSign;
}

/** The controls of the view row that draw the outline. */
const OUTLINE_CONTROLS: readonly Preset[] = [
  "conversation",
  "spec",
  "verification",
  "plan",
  "decisions",
];

/** Part T's sign (R21): the tree read, which only part T's code makes, in the load of a control
 *  that draws the outline. `main`'s outline reads the comments whole instead
 *  (`useIssueTree.ts`' `issueCommentsQuery`). */
const TREE_READ: OwnerSign = {
  controls: OUTLINE_CONTROLS,
  kind: "read",
  method: "GET",
  route: "/api/v1/issues/:id/tree",
};

/** Part T of dispatch://INFRA-1196, which brings the tree read. */
const PART_T = "dispatch://INFRA-4062";

/**
 * Each bound of R21 whose owner's code is not under the tested head, on `main` or on a branch
 * this part holds, with its owner and its sign (spec version 85's R21). Of the owners INV-7 names,
 * part T's entries alone are here: part M's code and dispatch://INFRA-5702's are under this part's
 * head, so the row reads Dashboard's list, whose sign is the control that draws its next five
 * (`dashboardNextFive`), and the history and Details' lists, whose sign is a control of theirs
 * that says how many are left. The row reads no bound listed here, and fails with
 * `AWAITING_SCALE names a bound whose owner is under the head: <bound>` wherever it finds the
 * bound's sign, so each bound is held from the merge that brings its owner, of `main` or of the
 * owner's branch: part T's pull request empties this list.
 */
export const AWAITING_SCALE: readonly AwaitedBound[] = [
  { bound: "the comments read", owner: PART_T, sign: TREE_READ },
  { bound: "the 2 MB of a load whose tree holds every comment", owner: PART_T, sign: TREE_READ },
  {
    bound: "the 40 requests of a load whose tree holds every comment",
    owner: PART_T,
    sign: TREE_READ,
  },
  {
    bound: "the 200 ms of the names reads of a load whose tree holds every comment",
    owner: PART_T,
    sign: TREE_READ,
  },
  { bound: "a node's first 50 children", owner: PART_T, sign: TREE_READ },
  { bound: "the children's section's 20 rows", owner: PART_T, sign: TREE_READ },
];

/** Whether `AWAITING_SCALE` names `bound`. */
function awaitsScale(bound: ScaleBound): boolean {
  return AWAITING_SCALE.some((entry) => entry.bound === bound);
}

/** Each bound of R21 that the row does not read and `AWAITING_SCALE` does not name. */
export function unheldScaleBounds(): ScaleBound[] {
  return SCALE_BOUNDS.filter((bound) => !SCALE_BOUNDS_READ.includes(bound) && !awaitsScale(bound));
}

/** The control under Dashboard's list that draws its next five and says how many are left,
 *  "Show the next five · 1 left" (`WaitingList.tsx`): what the row reads the list's count by, and
 *  dispatch://INFRA-5702's sign. */
function dashboardNextFive(page: Page): Locator {
  return dashboardWaiting(page).getByRole("button", {
    name: new RegExp(`^${DASHBOARD_WORDS.next_five}\\b`, "u"),
  });
}

/** Prints one reading of R21's row and keeps it among the row's annotations, under `type`. */
function report(type: string, line: string): void {
  console.log(line);
  test.info().annotations.push({ description: line, type });
}

/** One GET under `/api/` a cold load sent, as the row sends it again: its path and query, its
 *  route, the headers the page sent it with, and the status the page's own answer took, which
 *  stays undefined where the page's read got none. */
interface ScaleRead {
  readonly address: string;
  readonly headers: Readonly<Record<string, string>>;
  readonly route: string;
  status: number | undefined;
}

/** What one cold load sends and receives, from before its navigation: its requests under
 *  `/api/` (`watchApiRequests`), each distinct GET among them as the row sends it again, and the
 *  bytes of every JSON answer, read from the decoded bodies the browser received, with each answer
 *  whose body could not be read. */
function watchScaleLoad(page: Page) {
  const api = watchApiRequests(page);
  const reads = new Map<string, ScaleRead>();
  const bodies: Promise<number | string>[] = [];
  const onRequest = (request: Request) => {
    const url = new URL(request.url());
    const address = `${url.pathname}${url.search}`;
    if (request.method() !== "GET" || !url.pathname.startsWith("/api/") || reads.has(address)) {
      return;
    }
    const route = apiRoute(url.pathname);
    reads.set(address, { address, headers: request.headers(), route, status: undefined });
  };
  const onResponse = (response: Response) => {
    const url = new URL(response.url());
    if (isLiveSocket(url)) return;
    const read = reads.get(`${url.pathname}${url.search}`);
    if (read !== undefined && read.status === undefined && response.request().method() === "GET") {
      read.status = response.status();
    }
    if (!(response.headers()["content-type"] ?? "").includes("json")) return;
    bodies.push(
      response.body().then(
        (body) => body.length,
        () => response.url()
      )
    );
  };
  page.on("request", onRequest);
  page.on("response", onResponse);
  return {
    api,
    json: async (): Promise<{ readonly bytes: number; readonly unread: readonly string[] }> => {
      const answers = await Promise.all(bodies);
      const sizes = answers.filter((answer) => typeof answer === "number");
      return {
        bytes: sizes.reduce((sum, size) => sum + size, 0),
        unread: answers.filter((answer) => typeof answer === "string"),
      };
    },
    reads: (): readonly ScaleRead[] => [...reads.values()],
    /** Stops the census: no later request or answer of the page is read. */
    stop: (): void => {
      page.off("request", onRequest);
      page.off("response", onResponse);
    },
  };
}

/** Each route of `requests` that addresses, in its path, more than one node of one of `lists`: a
 *  read sent once for each node, which R21 allows a load none of. A batched read names its nodes
 *  in its query, in one request, and is no such read. */
function readsPerNode(requests: readonly ApiRequest[], lists: readonly ScaleNodes[]): string[] {
  const found: string[] = [];
  for (const list of lists) {
    const named = new Map<string, Set<string>>();
    for (const request of requests) {
      const route = `${request.method} ${request.route}`;
      for (const segment of (request.path.split("?")[0] ?? "").split("/")) {
        const id = decodeURIComponent(segment);
        if (list.ids.has(id)) named.set(route, (named.get(route) ?? new Set<string>()).add(id));
      }
    }
    for (const [route, ids] of named) {
      if (ids.size > 1) found.push(`${route}, one for each of ${ids.size} ${list.noun}`);
    }
  }
  return found;
}

/**
 * Sends `read` `SCALE_LIMITS.sends` times, one after another, through the page's own session
 * (`page.request`, with the context's cookies and the headers the page sent it with), and
 * answers the times to the first byte of the answers after the warm-ups: each answer's
 * `responseStart` less its `requestStart` (`APIResponse.timing()`, both in milliseconds from its
 * `startTime`), which is the server's time to its first byte, not the download of the whole body.
 * Every send must be answered as the page's own read was, and a timing that is missing (-1) or
 * runs backwards fails rather than being dropped. Only reads are sent, so no send writes.
 */
async function firstByteTimes(page: Page, read: ScaleRead): Promise<number[]> {
  const times: number[] = [];
  let answered = read.status;
  for (let send = 1; send <= SCALE_LIMITS.sends; send += 1) {
    const response = await page.request.get(read.address, { headers: read.headers });
    const status = response.status();
    const { requestStart, responseStart } = response.timing();
    await response.dispose();
    answered ??= status;
    if (status !== answered) {
      throw new Error(
        `GET ${read.address} was answered ${status} at send ${send}, where ${read.status === undefined ? "its first send" : "the page's own read"} was answered ${answered}`
      );
    }
    if (send <= SCALE_LIMITS.warmUps) continue;
    if (requestStart < 0 || responseStart < requestStart) {
      throw new Error(
        `GET ${read.address}'s send ${send} gives no time to its first byte: its request started at ${requestStart} ms and its answer at ${responseStart} ms`
      );
    }
    times.push(responseStart - requestStart);
  }
  return times;
}

/** Fails, naming each bound of `AWAITING_SCALE` whose owner's sign `place` shows - the load of a
 *  control, or Details opened after the Conversation's - with `AWAITING_SCALE names a bound whose
 *  owner is under the head: <bound>`: the sign's read among `requests`, the load's, or its
 *  control drawn, read once the page has settled on it. */
async function failOnOwnerSigns(
  page: Page,
  place: Preset | "details",
  requests: readonly ApiRequest[]
): Promise<void> {
  const shown: ScaleBound[] = [];
  for (const { bound, sign } of AWAITING_SCALE) {
    if (sign.kind === "read") {
      const { method, route } = sign;
      const sent = requests.some((request) => request.method === method && request.route === route);
      if (sent && place !== "details" && sign.controls.includes(place)) shown.push(bound);
    } else if (sign.kind === "details" ? place === "details" : sign.preset === place) {
      const drawn = await settledValue(`${bound}'s sign`, () => sign.control(page).count());
      if (drawn > 0) shown.push(bound);
    }
  }
  if (shown.length > 0) {
    throw new Error(
      `AWAITING_SCALE names a bound whose owner is under the head: ${shown.join("; ")}`
    );
  }
}

/**
 * Draw wait for INFRA-663's copy alone (decision 503): while `main`'s outline still renders all
 * 9,272 rows of its comments read at once (until part T's tree read lands), probe Job
 * `e2e-rows-9pkrd` at `37d1efb9` measured `[data-issue-page]` at 5.0–21.5 s and the view row at
 * 18.7–33.0 s at 1440 px, and 17.9 s and 27.7 s at 390 px (all 9,272 rows drawn). The reads of
 * the active control (`expectActiveControl`) take it too: in run `46010521078` at `d5afe3d2`,
 * shard 1, one `isVisible()` probe of the toolbar answered after 15.0 s while `main`'s outline
 * drew all 9,272 rows (dispatch://INFRA-3940/comment/208a7fd8-bdbe-443b-97eb-73af438c01c0).
 * Every other copy (`INFRA-3149`, `INFRA-1098`) and every other wait keeps the default 15 s.
 */
const INFRA_663_DRAW_TIMEOUT_MS = 60_000;

/** Opens `path` for `readScaleLoad`: on `INFRA-663`'s copy alone, waits up to
 *  `INFRA_663_DRAW_TIMEOUT_MS` for `[data-issue-page]` and `issueRead`'s column-or-heading at
 *  both widths (decision 503, probe Job `e2e-rows-9pkrd`); every other copy uses `gotoIssue`'s
 *  default 15 s. */
async function gotoScaleIssue(page: Page, copy: ScaleCopy, path: string): Promise<void> {
  if (copy.census !== "INFRA-663") {
    await gotoIssue(page, path);
    return;
  }
  await page.goto(path);
  await expect(issuePage(page), `${path} drew no [data-issue-page]`).toHaveCount(1, {
    timeout: INFRA_663_DRAW_TIMEOUT_MS,
  });
  await expect(
    page
      .getByTestId("issue-column")
      .or(issuePage(page).getByRole("heading", { level: 1 }))
      .first(),
    `${path} is still loading its issue`
  ).toBeVisible({ timeout: INFRA_663_DRAW_TIMEOUT_MS });
}

/** Waits until the load of `preset` draws what the row reads: Dashboard with its list read
 *  (`expectDashboardDrawn`); else the control on, and Documents' list with its rows, or the
 *  outline under a control that draws it. The longer view-row wait applies only to the
 *  largest comments copy. The outline and list checks keep their own waits. */
async function expectScaleControlDrawn(
  page: Page,
  preset: Preset,
  where: string,
  drawTimeoutMs?: number
): Promise<void> {
  if (drawTimeoutMs !== undefined) {
    await expect(
      presetToolbar(page).or(presetChooser(page)).first(),
      `${where} draws no view row`
    ).toBeVisible({ timeout: drawTimeoutMs });
  }
  if (preset === "dashboard") {
    await expectDashboardDrawn(page, where);
    return;
  }
  // Run `46010521078` at `d5afe3d2`, shard 1: one `isVisible()` probe of the toolbar answered
  // after 15.0 s while `main`'s outline drew all 9,272 rows, so the active control's reads take
  // the view row's wait (dispatch://INFRA-3940/comment/208a7fd8-bdbe-443b-97eb-73af438c01c0).
  await expectActiveControl(page, preset, drawTimeoutMs);
  if (preset === "documents") {
    await expect(documentsList(page), `${where} draws no Documents' list`).toBeVisible();
    await expect(documentsRows(page).first(), `${where} draws no row in its list`).toBeVisible();
  } else if (OUTLINE_CONTROLS.includes(preset)) {
    await expectIssuePageDrawn(page);
  }
}

/** Fails unless Dashboard's list draws at most its page of five, with the control that says how
 *  many of the copy's things that wait are left. The count it draws, the seed's six, is what
 *  puts the list past its page, and is asserted first. */
async function expectDashboardPaged(
  page: Page,
  copy: ScaleCopy,
  where: string,
  leg: string
): Promise<void> {
  const waiting = dashboardWaiting(page);
  const reading = await settledValue(`${where}: Dashboard's list`, async () => ({
    count: (await waiting.getByTestId("dashboard-waiting-count").innerText()).trim(),
    drawn: await waiting.locator('[data-testid^="dashboard-item-"]').count(),
    more: await wordsOf(dashboardNextFive(page)),
  }));
  report(
    "scale-list",
    `SCALE list ${leg}: Dashboard's list counts ${reading.count}, draws ${reading.drawn}, under it ${JSON.stringify(reading.more)}`
  );
  expect(
    reading.count,
    `${where} counts ${reading.count} things that wait on the reader, not the ${copy.waiting} the seed wrote`
  ).toBe(String(copy.waiting));
  expect
    .soft(
      reading.drawn,
      `${where}: Dashboard's list draws ${reading.drawn} of its ${copy.waiting} items, more than its page of ${SCALE_LIMITS.dashboardPage}`
    )
    .toBeLessThanOrEqual(SCALE_LIMITS.dashboardPage);
  expect
    .soft(
      reading.more,
      `${where}: Dashboard's list draws no control that says how many of its ${copy.waiting} items are left`
    )
    .toEqual([
      `${DASHBOARD_WORDS.next_five} · ${copy.waiting - reading.drawn} ${DASHBOARD_WORDS.left}`,
    ]);
}

/** Fails unless Documents' list draws at most its page of 20 rows, with the control that says how
 *  many of the documents and files the issue read lists are left (`showMoreControl`). */
async function expectDocumentsPaged(
  page: Page,
  copy: ScaleCopy,
  where: string,
  leg: string
): Promise<void> {
  const reading = await settledValue(`${where}: Documents' list`, async () => ({
    drawn: await documentsRows(page).count(),
    more: await wordsOf(showMoreControl(documentsList(page))),
  }));
  report(
    "scale-list",
    `SCALE list ${leg}: Documents' list draws ${reading.drawn} of ${copy.listed}, under it ${JSON.stringify(reading.more)}`
  );
  expect
    .soft(
      reading.drawn,
      `${where}: Documents' list draws ${reading.drawn} rows of the ${copy.listed} documents and files, more than its page of ${SCALE_LIMITS.documentsPage}`
    )
    .toBeLessThanOrEqual(SCALE_LIMITS.documentsPage);
  expect
    .soft(
      reading.more,
      `${where}: Documents' list draws no control that says how many of its ${copy.listed} documents and files are left`
    )
    .toEqual([showMoreName(copy.listed - reading.drawn)]);
}

/** What each control `controls` finds says, each with its runs of space made one: the words of a
 *  list's Show more. */
async function wordsOf(controls: Locator): Promise<string[]> {
  return (await controls.allTextContents()).map((text) => text.replace(/\s+/gu, " ").trim());
}

/**
 * Opens Details on the page of the Conversation's cold load, whose census `readScaleLoad` has
 * taken, so nothing the open reads counts in that load, and reads each list of Details that holds
 * more than its page on the copy (R21, part M's): the history, which Details draws under its
 * properties 20 entries at a time, under a Show more that says how many of the entries read are
 * left, with "+" while an older page of the log is unread (`IssueHistory.tsx`); its Files, every
 * document and file of the issue, 20 at a time (`IssueMargin.tsx`); and Referenced by, where more
 * than 20 things reference the issue, which its control reads once pressed. Details' other lists -
 * its pull requests, links, components and subscribers, the issues that wait on this one and the
 * reader's pins - hold none of the copy's records. A listed owner's sign that Details shows fails
 * the row (`failOnOwnerSigns`).
 */
async function expectDetailsPaged(
  page: Page,
  copy: ScaleCopy,
  where: string,
  leg: string,
  awaited: (bound: ScaleBound) => boolean
): Promise<void> {
  await openDetails(page);
  await failOnOwnerSigns(page, "details", []);
  const opened = `${where}, with Details opened after it`;
  if (!awaited("the history")) {
    await expect(historyPanel(page), `${opened}: Details draws no history`).toBeVisible();
    // What the history's Show more says, read once the history has drawn one of its states.
    const control = await historyShowMoreState(page);
    const history = await settledValue(`${opened}: the history`, async () => ({
      drawn: await historyEntries(page).count(),
      more: await wordsOf(historyShowMore(page)),
    }));
    report(
      "scale-list",
      `SCALE list ${leg} Details: the history draws ${history.drawn} entries, under it ${JSON.stringify(history.more)}, its Show more ${control}`
    );
    expect
      .soft(
        history.drawn,
        `${opened}: the history draws ${history.drawn} entries, more than its page of ${SCALE_LIMITS.historyPage}`
      )
      .toBeLessThanOrEqual(SCALE_LIMITS.historyPage);
    expect
      .soft(
        control,
        `${opened}: the history draws no control that says how many entries are left: ${JSON.stringify(history.more)}`
      )
      .toBe("counted");
  }
  if (awaited("Details' lists")) return;
  const files = marginShell(page).getByTestId("issue-files");
  await expect(files, `${opened}: Details draws no list of the issue's files`).toBeVisible();
  const listed = await settledValue(`${opened}: Details' Files`, async () => ({
    drawn: await files.getByRole("listitem").count(),
    // The list's Show more stands under it, in the row that holds it.
    more: await wordsOf(showMoreControl(files.locator(".."))),
  }));
  report(
    "scale-list",
    `SCALE list ${leg} Details: Files draws ${listed.drawn} of ${copy.listed}, under it ${JSON.stringify(listed.more)}`
  );
  expect
    .soft(
      listed.drawn,
      `${opened}: Details' Files draws ${listed.drawn} rows of the ${copy.listed} documents and files, more than its page of ${SCALE_LIMITS.detailsPage}`
    )
    .toBeLessThanOrEqual(SCALE_LIMITS.detailsPage);
  expect
    .soft(
      listed.more,
      `${opened}: Details' Files draws no control that says how many of its ${copy.listed} documents and files are left`
    )
    .toEqual([showMoreName(copy.listed - listed.drawn)]);
  if (copy.referencedBy <= SCALE_LIMITS.detailsPage) {
    report(
      "scale-list",
      `SCALE list ${leg} Details: Referenced by holds ${copy.referencedBy}, no more than its page`
    );
    return;
  }
  await marginShell(page)
    .getByRole("button", { name: new RegExp(`^${PROPERTY_LABELS.referenced_by}`, "u") })
    .click();
  const references = marginShell(page).getByRole("region", {
    exact: true,
    name: PROPERTY_LABELS.referenced_by,
  });
  const rows = references.locator("[data-reference-rows]");
  await expect(rows.first(), `${opened}: Referenced by draws no row`).toBeVisible();
  const referenced = await settledValue(`${opened}: Referenced by`, async () => ({
    drawn: await rows.count(),
    more: await wordsOf(showMoreControl(references)),
  }));
  report(
    "scale-list",
    `SCALE list ${leg} Details: Referenced by draws ${referenced.drawn} of ${copy.referencedBy}, under it ${JSON.stringify(referenced.more)}`
  );
  expect
    .soft(
      referenced.drawn,
      `${opened}: Referenced by draws ${referenced.drawn} rows, more than its page of ${SCALE_LIMITS.detailsPage}`
    )
    .toBeLessThanOrEqual(SCALE_LIMITS.detailsPage);
  expect
    .soft(
      referenced.more,
      `${opened}: Referenced by draws no control that says how many of what references the issue are left`
    )
    .toHaveLength(1);
}

/**
 * One cold load of `preset`'s address on `copy`, in the new context `page` is the first page of,
 * signed in as `reader`, who has read nothing of the copy, read as R21 reads it: at most 40
 * requests under `/api/` between navigation and network idle (`expectLoadSettled`) and none per
 * node (`readsPerNode`), at most 2 MB of JSON, `assertPageFits`, Dashboard's list and Documents'
 * list each at most its page with the control that says how many are left, and then each distinct
 * read of the load that no load before it made (`timed`) sent 35 times through the page's own
 * session (`firstByteTimes`), each at most 200 ms to its first byte at the 90th percentile of the
 * last 30. After the Conversation's load, and outside its census, it opens Details and reads the
 * history and Details' lists (`expectDetailsPaged`). A bound `AWAITING_SCALE` names is not read,
 * and is printed as awaited where it applies; the load fails at once where it shows a listed
 * owner's sign (`failOnOwnerSigns`). A bound that fails is recorded (`expect.soft`), so one run
 * names every bound a copy, a control and a width breaks, and the row fails at its end. A load
 * reads every comment where it sends the issue's whole comments read, which is how `main`'s
 * outline builds its tree.
 */
export async function readScaleLoad(
  page: Page,
  copy: ScaleCopy,
  preset: Preset,
  reader: string,
  timed: Set<string>
): Promise<void> {
  const width = page.viewportSize()?.width ?? 0;
  const leg = `${copy.census} ${preset} ${width} px`;
  const where = `the cold load of ${PRESET_NAMES[preset]} on the copy of ${copy.census} (${copy.key}) at ${width} px`;
  expect(
    (await getMyState(reader))[copy.key]?.last_read_seq ?? 0,
    `${reader} has read ${copy.key} before ${where}, so it is no load of a person who never read it`
  ).toBe(0);

  const load = watchScaleLoad(page);
  const drawTimeoutMs = copy.census === "INFRA-663" ? INFRA_663_DRAW_TIMEOUT_MS : undefined;
  await gotoScaleIssue(page, copy, presetPath(copy.key, preset));
  await expectScaleControlDrawn(page, preset, where, drawTimeoutMs);
  await expectLoadSettled(load.api);
  // What the load sent between navigation and network idle; a poll the page sends later is no
  // part of it.
  const requests = [...load.api.requests];
  const reads = load.reads();
  const routes = load.api.byRoute().map(({ count, route }) => `${count} × ${route}`);
  const json = await load.json();
  load.stop();
  const comments = `/api/v1/issues/${copy.key}/comments`;
  const whole = requests.some((request) => request.method === "GET" && request.path === comments);
  report(
    "scale-load",
    `SCALE load ${leg}: ${requests.length} requests under /api/, ${json.bytes} bytes of JSON (${json.unread.length} answers unread), ${whole ? "its tree holds every comment" : "no read of every comment"}; ${routes.join(", ")}`
  );

  await failOnOwnerSigns(page, preset, requests);
  /** Whether `AWAITING_SCALE` names `bound`, which the load then leaves unread, saying so. */
  const awaited = (bound: ScaleBound): boolean => {
    if (!awaitsScale(bound)) return false;
    report("scale-awaited", `SCALE awaited ${leg}: ${bound}`);
    return true;
  };

  if (!(whole && awaited("the 40 requests of a load whose tree holds every comment"))) {
    expect
      .soft(
        requests.length,
        `${where} sent ${requests.length} requests under /api/ between navigation and network idle, more than ${SCALE_LIMITS.requests}: ${routes.join(", ")}`
      )
      .toBeLessThanOrEqual(SCALE_LIMITS.requests);
  }
  expect.soft(readsPerNode(requests, copy.nodes), `${where} sent a read per node`).toEqual([]);
  if (!(whole && awaited("the 2 MB of a load whose tree holds every comment"))) {
    expect
      .soft(json.unread, `${where}: answers whose JSON was not read, so the load has no total`)
      .toEqual([]);
    expect
      .soft(json.bytes, `${where} received ${json.bytes} bytes of JSON, more than 2 MB`)
      .toBeLessThanOrEqual(SCALE_LIMITS.jsonBytes);
  }

  if (preset === "dashboard" && !awaited("Dashboard's list")) {
    await expectDashboardPaged(page, copy, where, leg);
  }
  if (preset === "documents" && !awaited("Documents' list")) {
    await expectDocumentsPaged(page, copy, where, leg);
  }
  await assertPageFits(page);

  const names =
    whole && awaited("the 200 ms of the names reads of a load whose tree holds every comment");
  for (const read of reads) {
    if (timed.has(read.address)) continue;
    if (
      (read.address === comments && awaitsScale("the comments read")) ||
      (names && read.route === "/api/v1/reference-targets")
    ) {
      report("scale-awaited", `SCALE awaited ${leg}: the 200 ms of GET ${read.address}`);
      continue;
    }
    timed.add(read.address);
    const times = await firstByteTimes(page, read);
    const sorted = [...times].sort((a, b) => a - b);
    // The 90th percentile by nearest rank: the 27th of the 30 times.
    const p90 = sorted[Math.ceil(sorted.length * 0.9) - 1] ?? Number.POSITIVE_INFINITY;
    const spread = `${p90.toFixed(1)} ms at the 90th percentile of ${times.length}, ${sorted[0]?.toFixed(1)} to ${sorted.at(-1)?.toFixed(1)} ms`;
    report("scale-read", `SCALE read ${leg}: GET ${read.address} ${spread}`);
    expect
      .soft(p90, `GET ${read.address}, a read of ${where}, answers its first byte in ${spread}`)
      .toBeLessThanOrEqual(SCALE_LIMITS.firstByteMs);
  }
  if (preset === "conversation") await expectDetailsPaged(page, copy, where, leg, awaited);
}
