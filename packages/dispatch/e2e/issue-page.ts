import { expect, type Locator, type Page } from "@playwright/test";

import { presetPath } from "../web/src/features/issue/issue-address";
import type { MarginSection } from "../web/src/features/issue/page-structure";
import { CONTROL_LABELS, PRESET_NAMES, type Preset } from "../web/src/features/issue/page-words";
import type { Box } from "./layout";
import { navigateInApp } from "./sends";

/**
 * The DOM of the redesigned issue page (dispatch://INFRA-1196), as one module.
 *
 * `e2e/issue-layout.e2e.ts` and the tree's rows in `e2e/keyboard-issue.e2e.ts` read the page only
 * through this module and spell no selector of their own, so this file is the single statement of
 * what the page's DOM is: the page is built to it, and anything named here is a requirement on
 * that page. Each item below names the check that reads it, by the `check` block of
 * `dispatch://INFRA-1196/artifact/verification-md`, so nothing is named that no check reads.
 *
 * It is also the one place every other spec reaches the page through: the moves at the end of
 * the file - a preset, one of the addresses the old tabs answered, the identity block's actions
 * - are here rather than in a second module of their own, so one page has one set of helpers and
 * no name means two things (`openPreset` the address and `pickPreset` the control).
 *
 * ## What the page draws
 *
 * - **The page root** `[data-issue-frame]`, the shell's frame the page registers
 *   (`web/src/features/shell/page-frame.tsx`, `ISSUE_FRAME`). Every check reads it, because
 *   every test asserts the page drew at all before it reads anything inside it.
 * - **The identity block** `header[data-issue-header]`: today's attribute, kept, because the
 *   keyboard scope finds the Status, priority, labels, title and pin controls through it, which
 *   `web/src/features/issue/IssueHeader.tsx` says of the attribute in its own doc comment.
 *   c-properties, c-nothing-lost, c-keyboard.
 * - **The head of the page** `[data-testid="issue-three-things"]`, holding the spec summary's
 *   sentences as `[data-author-text]` and two rows, `[data-testid="issue-plan-row"]` and
 *   `[data-testid="issue-checks-row"]`. c-three-things.
 * - **The children section** `[data-testid="issue-children"]`, its disclosure a `button`, each row
 *   `[data-child-row]` with `data-child-status`, and the collapsed header's two counts
 *   `[data-testid="issue-children-direct-count"]` and
 *   `[data-testid="issue-children-subtree-count"]`, each with a visible label. c-children,
 *   c-nothing-lost, c-keyboard (`t h`).
 * - **The filter row** `[data-filter-row]`, one line at every width, and in it the five
 *   controls `[data-testid="issue-presets"]`. At 1280 px and above they are a
 *   `role="toolbar"` named `CONTROL_LABELS.presets` of five toggle `button`s, Conversation,
 *   Spec, Verification, Plan and Decisions, each carrying `aria-pressed`, its accessible name
 *   the control's name alone, with its signal - its count in `[data-preset-count="<preset>"]`,
 *   and where it draws one what of it waits on the reader in `[data-preset-flag]` - beside the
 *   button and outside it, a `<label>` of the button in the same drawn box, named as the
 *   button's description (`aria-describedby`): a word drawn inside a control has to be in that
 *   control's name (WCAG 2.5.3), and each is held to its name by the inventory of the page it
 *   replaced. Below 1280 px the five become one control: a `button` with `aria-haspopup="menu"`
 *   drawing the one on and its signal, and so named by both, its name part
 *   `[data-preset-name]`, opening a `role="menu"` of five `role="menuitemradio"`s, each named
 *   by its control's name and its signal and checked where it is the one on. Each control has
 *   its own address (`issue-address.ts`'s `presetPath`, which this module imports). At the
 *   row's right end from 761 px up, each a small `button`: the version control's while a
 *   filter is on, the node menu's, the filter by words' and Keys; at 760 px and below the
 *   first three are in the Outline sheet. c-presets, c-three-things, c-children, c-urls,
 *   c-plain-words, c-keyboard.
 * - **The node menu** beside the presets: a `button` named by `CONTROL_LABELS.other_nodes`,
 *   opening a `role="menu"` whose items are its rows: each document and artifact of the issue
 *   is a `role="menuitem"` holding the button that opens it and the `button` that copies its
 *   reference, since a `menu` may own menu items alone. c-nodes, c-urls (`/artifacts`),
 *   c-keyboard (`t a`).
 * - **The tree**: `role="tree"` named "Issue tree", rows `role="treeitem"` carrying `aria-level`
 *   and, when they hold children, `aria-expanded`. Each row is `[data-node-row]` with
 *   `data-node-type` (the model's own token, a key of `NODE_TYPE_NAMES`), `data-node-id`,
 *   `data-node-state` (absent for a type the model gives no state) and, when folded,
 *   `data-folded-count`. Inside a row (R34): its bullet `[data-node-bullet]`, which is the
 *   `button` named `CONTROL_LABELS.fold` / `CONTROL_LABELS.unfold` where the node holds
 *   children; then `[data-node-content]`, whose first child is the node's words - drawn whole
 *   as `[data-node-words]`, or, on a folded branch, its one line `[data-node-first-line]
 *   [data-author-text]` with its count `[data-node-count]` - and which carries
 *   `[data-node-own]` on the reader's own words; an agent's words cut at six lines carry
 *   `[data-node-more]`, the control that opens the rest. At the right, `[data-node-margin]`
 *   holds a typed kind's tag `[data-node-tag]` (its word as page chrome and its shape as
 *   `svg[data-glyph]` with an accessible name), the author `[data-node-author]` (the page's
 *   words "Author unknown" where the model holds none), the time `[data-node-time]` and the
 *   state word as `[data-node-state]`'s own text. Each chip is `[data-node-chip]` naming its
 *   target, and an answered ask's answer is `[data-node-answer]` on the row - its answerer as
 *   `[data-answer-author]`, the whole of what the person wrote as `[data-answer-text]`. A
 *   branch deeper than the drawn levels is one row `[data-branch-more]` whose text says how
 *   many nodes it holds. c-tree, c-row, c-spine, c-focus-opens, c-presets, c-checks-readable,
 *   c-reflow, c-plain-words, c-keyboard.
 * - **A node's body** `[data-node-body]`, drawn for the focused row and for a row pinned open
 *   (`[data-node-body][data-pinned]`) and for no other row. c-tree, c-focus-opens, c-keyboard.
 * - **A gap the page names** `[data-gap]` with `data-gap-kind`. c-spine.
 * - **The panel, Details**: the shell's margin as a drawer over the right of the page,
 *   `[data-testid="desktop-margin-shell"]`, `hidden` while closed, which is how every load opens
 *   it; the identity block's first row carries the `button` named `CONTROL_LABELS.details` that
 *   opens it, and its head the `button` named `CONTROL_LABELS.close`. Inside it, the issue page's
 *   own slot `[data-testid="issue-margin"]`, its sections
 *   `[data-margin-section="development" | "people" | "references"]`, and the focused node's facts
 *   `[data-testid="focused-node-facts"]`. c-one-panel, c-properties, c-reflow.
 * - **The icon rail** `nav[data-testid="icon-rail"]`, the shell's navigation on the issue page
 *   from 768 px up, each entry a link named by its label, and its foot control the `button`
 *   named "Open navigation". Below 768 px that button is in the identity block's first row.
 *   c-one-panel.
 * - **The Contents pane** `aside[data-testid="issue-contents"]` from 1024 px up, holding the head
 *   of the page and the list `[role="tree"]` whose rows are `[data-contents-entry]`, and the
 *   Focus branch `button`; below 1024 px the `button` named `CONTROL_LABELS.outline` beside the
 *   presets opens the same list in `[data-testid="issue-outline-sheet"]`. c-three-things,
 *   c-one-panel, c-reflow.
 * - **The page's own words** carry `data-page-chrome`; text from a person or an agent carries
 *   `data-author-text` (`PAGE_CHROME_ATTRIBUTE`, `AUTHOR_TEXT_ATTRIBUTE`). c-plain-words.
 *
 * ## What this module adds to that contract, and the check that reads each
 *
 * Each of these is a thing a check's own sentence needs and the contract does not name. They are
 * requirements on the page like every item above.
 *
 * - **A node's id** (`data-node-id`) is the node's own address inside the issue, so two documents
 *   that both hold a block `r1` draw two different rows: the issue is its key, a document or other
 *   artifact its slug, a block `<slug>#b-<block id>` (`tree-drafts.ts`'s `blockNodeId`, which a
 *   test imports from there), and an ask, comment, message, handoff or result the id the model
 *   gives it. A child issue is its key. Read by every test that names one row. An ask the server
 *   indexed from a document's `ask` block is that block's node, under `blockNodeId`; an ask made
 *   on the issue itself is drawn under its own id.
 * - **A node's body sits inside its own row**, so a row and its body are read together
 *   (`[data-node-row] [data-node-body]`). c-tree, c-focus-opens.
 * - **`[data-node-context]`** on a row a filter draws to place its own - a requirement under
 *   Verification - and, with no filter on, on the chain above a match of the filter by words.
 *   c-presets.
 * - **`[data-node-parent-line]`** under a filter's row whose parent the filter draws as no row:
 *   one line carrying `data-parent-id` and `data-parent-type`, the parent's kind as page chrome
 *   and its first line as the author's text (R2). c-presets.
 * - **`[data-testid="issue-filter"]`**, the filter by words: a `searchbox` carrying the query.
 *   c-keyboard (`t f`), c-urls (`?q=`).
 * - **`[data-testid="issue-version-control"]`**, the version control beside the filters while a
 *   filter is on, holding the version picker, the name-a-version control and the link, which
 *   reads the document opened in place of the outline and, with none opened, the filter's own
 *   document; none is drawn with no filter on. It holds no approval, which stays in the identity
 *   block. c-presets, c-urls (`?v=`), c-nothing-lost (naming a version).
 * - **`[data-testid="opened-artifact"]`**, the document or file an address opens in place of
 *   the outline, named by its own name. c-nodes, c-urls.
 * - **`[data-testid="issue-composer"]`**, the one place to write at the foot of the outline, a
 *   region named `CONTROL_LABELS.write` holding the `Comment composer` form - its line, its kind
 *   chips (`[data-composer-kind]`), the target line naming what it replies to
 *   (`[data-composer-target]`) and Send - or, while it answers a question, the form named
 *   `CONTROL_LABELS.answering` (`[data-answering="<ask id>"]`), its options
 *   (`[data-answer-option]`), Decline to answer and Send answer. c-reply-nesting, c-keyboard
 *   (`r`, `a`), c-nothing-lost (a reply, a comment from the composer, answering an ask).
 * - **`[data-row-act]`** on each of the focused row's actions under its words - `reply`,
 *   `answer`, `change-answer` - and **`[data-pick-option]`** on an open question's options, which
 *   pick its answer. c-reply-nesting.
 * - **`[data-testid="issue-history"]`**, the issue's history in Details, a region named
 *   `CONTROL_LABELS.history`; each thread agents wrote among themselves is drawn by its turns,
 *   **`[data-history-item="<id>"]`**, each with its Reply (`[data-history-reply="<id>"]`), and one
 *   author's uploads within a minute are one `[data-history-uploads]` entry. c-conversation-state,
 *   c-urls (`/activity`, an agents-only comment's address), c-keyboard (`t v`).
 * - **`[data-testid="issue-pins"]`**, the reader's pins in Details, each a `[data-pin]` link to
 *   its row. c-one-panel.
 * - **`[data-node-command]`** on a check's `run` command, which renders as code beside its copy
 *   control. c-checks-readable.
 * - **The add control** (`CONTROL_LABELS.add`) opens a `role="menu"` listing every property with
 *   no value, which is how "an empty field is reachable from one add control that lists it" is
 *   read. c-properties.
 * - **The filter row holds the five controls alone**: the node menu's button, the filter by
 *   words and Keys sit beside it and outside it, so the one control at 1024 px and below is
 *   unambiguous. c-presets, c-nodes.
 * - **The children section's completion bar**, a `role="progressbar"` the collapsed header keeps
 *   beside its two counts. c-children.
 * - **A `role="status"` of the page's own** for what a key reports when it has nowhere to go,
 *   such as the next-question key with no further question. c-open-questions, c-keyboard.
 * - **A row's fold control**, its bullet: a `button` named `CONTROL_LABELS.fold` while its
 *   branch is drawn and `CONTROL_LABELS.unfold` while it is folded, drawn on every row that
 *   holds children: the page opens folded to the frontier (R1), so this is how a reader opens a
 *   branch the fold left shut, and `unfoldEveryBranch` is a check doing it. c-tree, c-row,
 *   c-checks-readable, c-nodes, c-nothing-lost.
 * - **The header's band**, from the top of the identity block to the top of the preset row,
 *   which is where R13 measures the header's height and its lines (`headerReading`).
 *   c-properties, c-typeface.
 * - **The typeface**: the app's stylesheet declares Inter (`TYPEFACE`) in an `@font-face` rule
 *   whose address the page loads it from, the document's font set holds that face, and the
 *   identity block's computed `font-family` names it first. c-typeface.
 *
 * ## How a test reads the page
 *
 * A `locator` that never resolves times out rather than asserting, so the readers below answer
 * through one `page.evaluate` over the document and return what is there - an empty list for a
 * page that draws none of it - and a test asserts on that answer. `gotoIssue` waits only for the
 * identity block, which the old page and the new one both draw, so a test against the old page
 * fails on its own assertion and never on a navigation that never settles.
 */

/** The selectors the readers below pass into the browser. Nothing outside this module spells one:
 *  a test reads the page through the locators and readers this module exports. */
const SELECTORS = {
  answerAuthor: "[data-answer-author]",
  answerLine: "[data-node-answer]",
  answerText: "[data-answer-text]",
  authorText: "[data-author-text]",
  branchMore: "[data-branch-more]",
  details: '[data-testid="desktop-margin-shell"]',
  childRow: "[data-child-row]",
  /** The whole filter row: the five controls or the one control, Outline, and the small
   *  controls at its right end (`IssuePresets.tsx`). */
  filterRow: "[data-filter-row]",
  gap: "[data-gap]",
  /** The part of the identity block that stays at the top of the screen from 761 px up: the
   *  key's row and the title (`IssueHeader.tsx`). */
  identityBand: "[data-identity-block]",
  identityBlock: "header[data-issue-header]",
  issuePage: "[data-issue-frame]",
  marginSlot: '[data-testid="issue-margin"]',
  nodeAuthor: "[data-node-author]",
  nodeBody: "[data-node-body]",
  nodeChip: "[data-node-chip]",
  nodeFirstLine: "[data-node-first-line]",
  nodeGlyph: "svg[data-glyph]",
  nodeRow: "[data-node-row]",
  nodeUnsure: "[data-node-unsure]",
  nodeTag: "[data-node-tag]",
  nodeWords: "[data-node-words]",
  nodeState: "[data-node-state]",
  pageChrome: "[data-page-chrome]",
  parentLine: "[data-node-parent-line]",
  presetCount: "[data-preset-count]",
  presetRow: '[data-testid="issue-presets"]',
  tree: '[role="tree"]',
} as const;

// ---------------------------------------------------------------------------------------------
// The page's parts, as locators.
// ---------------------------------------------------------------------------------------------

export function issuePage(page: Page): Locator {
  return page.locator(SELECTORS.issuePage);
}

export function identityBlock(page: Page): Locator {
  return page.locator(SELECTORS.identityBlock);
}

export function threeThings(page: Page): Locator {
  return page.getByTestId("issue-three-things");
}

/** The sentences of the spec's summary, each one the author's own text. */
export function threeThingsSentences(page: Page): Locator {
  return threeThings(page).locator(SELECTORS.authorText);
}

/** The children's section the page drew above the outline before the architect's ruling 5: no
 *  view draws one now, and a check reads this to say so. */
export function childrenSection(page: Page): Locator {
  return page.getByTestId("issue-children");
}

/** The children as the page draws them: rows of the outline, one group of them (ruling 5). */
export function childRows(page: Page): Locator {
  return tree(page).locator('[data-node-row][data-node-type="child_issue"]');
}

/** One child's row of the outline, by the child's key, which is its node's id. */
export function childRow(page: Page, key: string): Locator {
  return tree(page).locator(`[data-node-row][data-node-id="${key}"]`);
}

export function presetRow(page: Page): Locator {
  return page.locator(SELECTORS.presetRow);
}

/** The row of five, at 1280 px and above. */
export function presetToolbar(page: Page): Locator {
  return page.getByRole("toolbar", { exact: true, name: CONTROL_LABELS.presets });
}

/** The five toggles of the row, at 1280 px and above. */
export function presetToggles(page: Page): Locator {
  return presetRow(page).locator("button[aria-pressed]");
}

export function presetToggle(page: Page, preset: Preset): Locator {
  return presetRow(page).getByRole("button", { exact: true, name: PRESET_NAMES[preset] });
}

/** The count the row draws for one control: beside the toggle at a width that draws the row,
 *  and inside the one control below it. It sits beside the toggle rather than inside it because
 *  the toggle's accessible name has to hold every word drawn inside it (WCAG 2.5.3) and the
 *  toggle is named by its control's name alone; the toggle points at the signal with
 *  `aria-describedby`, so a reader still hears it (`web/src/features/issue/IssuePresets.tsx`). */
export function presetCount(page: Page, preset: Preset): Locator {
  return presetRow(page).locator(`[data-preset-count="${preset}"]`);
}

/** The one control the row becomes at 1024 px and below, named by the one on. */
export function presetChooser(page: Page): Locator {
  return presetRow(page).locator('button[aria-haspopup="menu"]');
}

/** The one menu the page has open, whether the preset chooser's or the node menu's: the page
 *  opens at most one at a time, which a test asserts by counting this. */
export function openMenu(page: Page): Locator {
  return page.getByRole("menu");
}

/** One item of the one control's menu: a `menuitemradio`, checked where it is the one on, named
 *  by what it draws - its control's name, then its signal (WCAG 2.5.3). */
export function presetMenuItem(page: Page, preset: Preset): Locator {
  return openMenu(page).getByRole("menuitemradio", {
    name: new RegExp(`^${PRESET_NAMES[preset]}( |$)`),
  });
}

export function nodeMenuButton(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.other_nodes });
}

export function nodeMenuItems(page: Page): Locator {
  return openMenu(page).getByRole("menuitem");
}

/** The one control that adds a property with no value yet, which Details holds (R13 bans
 *  "more"). */
export function addControl(page: Page): Locator {
  return marginSlot(page).getByRole("button", { exact: true, name: CONTROL_LABELS.add });
}

/** The header's labels: those that fit on its properties line (the ruling of
 *  dispatch://INFRA-1196/comment/63b73642-8fc5-4cc4-8f5d-a3d8989d3bf4). A chip past the line's
 *  end is not drawn at all, so every chip the line holds is one the reader sees. */
export function headerLabels(page: Page): Locator {
  return identityBlock(page).locator('[data-testid="issue-header-labels"] > [data-header-label]');
}

/** The count that ends the header's properties line with the labels left over, which opens
 *  Details at its Labels row. */
export function labelsLeft(page: Page): Locator {
  return identityBlock(page).getByTestId("issue-labels-left");
}

/** The header's count of what waits on the reader, or whose turn it is when nothing waits. */
export function waitingCount(page: Page): Locator {
  return identityBlock(page).getByTestId("issue-whose-turn");
}

/** One row of Details, by the label in its own column: the row's label and its value. */
export function detailsRow(page: Page, label: string): Locator {
  return marginSlot(page)
    .locator("[data-margin-section] > div > div")
    .filter({ has: page.getByText(label, { exact: true }) });
}

/** The value a badge-and-select control shows a reader: the badge drawn under the `select`
 *  named `label` (the assignee, the priority, the repository; `web/src/features/issue/
 *  badge-select.ts`). The badge is hidden from assistive technology, because the select
 *  carries the name, and the select's own `option` for the same value holds the same words -
 *  so a reading by that text alone finds two elements and says which is drawn for neither. */
export function badgeValue(page: Page, label: string): Locator {
  return page.locator(`:has(> select[aria-label="${cssValue(label)}"]) > [aria-hidden="true"]`);
}

export function tree(page: Page): Locator {
  return issuePage(page).getByRole("tree", { name: "Issue tree" });
}

export function treeItems(page: Page): Locator {
  return tree(page).getByRole("treeitem");
}

export function nodeRows(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.nodeRow);
}

export function nodeRow(page: Page, id: string): Locator {
  return nodeRows(page).filter({ has: page.locator(`:scope[data-node-id="${cssValue(id)}"]`) });
}

export function rowsOfType(page: Page, type: string): Locator {
  return nodeRows(page).filter({
    has: page.locator(`:scope[data-node-type="${cssValue(type)}"]`),
  });
}

/** The author named on one row, or on the head of the page, where the model holds one. */
export function rowAuthor(scope: Locator): Locator {
  return scope.locator(SELECTORS.nodeAuthor);
}

/** The pictures drawn inside one row: an image node's own, and a screenshot in a node's text. */
export function rowPictures(row: Locator): Locator {
  return row.locator("img");
}

export function rowBody(row: Locator): Locator {
  return row.locator(SELECTORS.nodeBody);
}

/** The node's words as one row draws them whole (R34, ruling 1). */
export function rowWords(row: Locator): Locator {
  return row.locator(SELECTORS.nodeWords).first();
}

/** The control that opens the rest of the words a row cut at six lines, and closes them. */
export function rowMore(row: Locator): Locator {
  return row.locator("[data-node-more]");
}

export function nodeBodies(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.nodeBody);
}

export function pinnedBodies(page: Page): Locator {
  return issuePage(page).locator(`${SELECTORS.nodeBody}[data-pinned]`);
}

export function branchMoreRows(page: Page): Locator {
  return issuePage(page).locator(SELECTORS.branchMore);
}

/** The gaps drawn on one row. */
export function rowGaps(row: Locator): Locator {
  return row.locator(SELECTORS.gap);
}

/** The hidden file input the upload opens, which is how a test hands it a file. */
export function uploadInput(page: Page): Locator {
  return page.locator('input[type="file"]');
}

/** What the page says when a key has nowhere to go, such as the next-question key with no
 *  further question: drawn at the row the key was pressed on, and gone with the reader's next
 *  move (c-open-questions, c-keyboard). */
export function pageStatus(page: Page): Locator {
  return tree(page).locator("[data-row-report]");
}

/** A check's `run` command, drawn as code with its copy control beside it. */
export function nodeCommands(page: Page): Locator {
  return issuePage(page).locator("[data-node-command]");
}

export function copyCommandControls(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.copy_command });
}

/** The box of the filter by words, which is `?q=`: drawn while it holds words or has the caret,
 *  and in the Outline sheet at 760 px and below. */
export function filterBox(page: Page): Locator {
  return page.getByTestId("issue-filter");
}

/** The filter by words' small control at the right end of the filter row, which opens its box
 *  with the caret in it; `t f` does the same (`IssuePresets.tsx`). */
export function filterControl(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.filter });
}

/** The version control: the version picker, the name-a-version control, the link, and beside an
 *  open editor its live connection (`VersionControl.tsx`). It is drawn - and counted - while a
 *  filter is on or a document is open, behind its small control at the right end of the filter
 *  row, which `openVersionControl` presses; at 760 px and below it is in the Outline sheet. */
export function versionControl(page: Page): Locator {
  return page.getByTestId("issue-version-control");
}

/** The version control's own small control at the right end of the filter row. */
export function versionControlButton(page: Page): Locator {
  return issuePage(page).getByRole("button", { exact: true, name: CONTROL_LABELS.version });
}

/** Opens the version control as a reader does, unless it is open: from its small control at the
 *  right end of the filter row, or, at 760 px and below where the row draws none, by opening the
 *  Outline sheet that holds it. */
export async function openVersionControl(page: Page): Promise<void> {
  if (await versionControl(page).isVisible()) return;
  const own = versionControlButton(page);
  await ((await own.count()) > 0 ? own : outlineControl(page)).click();
  await expect(versionControl(page), "the version control did not open").toBeVisible();
}

/** Opens the box of the filter by words as a reader does, unless it is drawn: from its small
 *  control, or, at 760 px and below, by opening the Outline sheet that holds it. */
export async function openFilterBox(page: Page): Promise<void> {
  if (await filterBox(page).isVisible()) return;
  const own = filterControl(page);
  await ((await own.count()) > 0 ? own : outlineControl(page)).click();
  await expect(filterBox(page), "the filter by words did not open").toBeVisible();
}

/** Which version the version control is showing, as the picker's own value: the empty string
 *  for the current version and the number of a pinned one. The control's text cannot say it - a
 *  `select`'s option texts are part of its container's `innerText`, so "Version 1" is in that
 *  text whichever version is shown - so the value is what is read, open or shut. */
export function versionPicker(page: Page): Locator {
  return versionControl(page).locator(`select[aria-label="${CONTROL_LABELS.version}"]`);
}

/** The document or file opened in place of the outline (`ArtifactRoutePanel.tsx`). */
export function openedArtifact(page: Page): Locator {
  return page.getByTestId("opened-artifact");
}

/** The editor of a document opened in place of the outline. */
export function openedEditor(page: Page): Locator {
  return openedArtifact(page).getByRole("textbox", { name: "Document editor" });
}

/** The issue page's own slot in the shell's margin. */
export function marginSlot(page: Page): Locator {
  return page.getByTestId("issue-margin");
}

export function marginSection(page: Page, section: MarginSection): Locator {
  return marginSlot(page).locator(`[data-margin-section="${section}"]`);
}

/** The one control that opens and closes a margin section. */
export function marginSectionDisclosure(page: Page, section: MarginSection): Locator {
  return marginSection(page, section).getByRole("button").first();
}

export function focusedNodeFacts(page: Page): Locator {
  return page.getByTestId("focused-node-facts");
}

/**
 * Counts, from the next load on, how many times the page draws the panel's content: each
 * distinct element of one of the panel's sections attached to the document. The page draws the
 * panel once and moves it between hosts (`IssuePage`'s `panelStage`), and a moved element is
 * the same element, so it counts once; a panel drawn a second time is a new element, whatever
 * its reads cost - a second copy over cached reads sends no request (INFRA-2004). Call it before
 * the load, and the reader it returns once the load has settled. c-one-panel.
 */
export async function watchPanelDraws(page: Page): Promise<() => Promise<number>> {
  await page.addInitScript(() => {
    // Kept on the document's own root, which outlives every render of the app.
    const seen = new Set<Element>();
    new MutationObserver((records) => {
      for (const record of records) {
        for (const node of record.addedNodes) {
          if (!(node instanceof Element)) continue;
          if (node.matches('[data-margin-section="development"]')) seen.add(node);
          for (const section of node.querySelectorAll('[data-margin-section="development"]')) {
            seen.add(section);
          }
        }
      }
      document.documentElement.dataset.panelDraws = String(seen.size);
    }).observe(document, { childList: true, subtree: true });
  });
  return () => page.evaluate(() => Number(document.documentElement.dataset.panelDraws ?? "0"));
}

/** The shell's own margin, which on the issue page is the Details drawer over the right of the
 *  page and on every other page of Dispatch the column it always was
 *  (`web/src/features/margin/Margin.tsx`). */
export function marginShell(page: Page): Locator {
  return page.getByTestId("desktop-margin-shell");
}

export function marginResizeHandle(page: Page): Locator {
  return page.getByLabel("Resize margin");
}

/** The control at the right of the identity block's first row that opens and closes Details. */
export function detailsControl(page: Page): Locator {
  return identityBlock(page).getByRole("button", { exact: true, name: CONTROL_LABELS.details });
}

/** Details' own Close, in its head. */
export function detailsClose(page: Page): Locator {
  // The drawer's own Close, in its head: a composer or a card inside it can carry a control of
  // the same name.
  return marginShell(page)
    .getByTestId("details-close")
    .and(page.getByRole("button", { exact: true, name: CONTROL_LABELS.close }));
}

/** Opens Details with its control, as a reader does, unless it is open already. The control is
 *  asserted first, so a page drawing none fails on that rather than on a press that never lands. */
export async function openDetails(page: Page): Promise<void> {
  await expect(detailsControl(page), "the page draws no Details control").toHaveCount(1);
  if (await marginShell(page).isVisible()) return;
  await detailsControl(page).click();
  await expect(marginShell(page), "Details did not open from its control").toBeVisible();
}

/** Closes Details with its own Close, unless it is closed already, and waits out the frames in
 *  which the drawer hands the focus back - on a phone the sheet does so after the frame it closes
 *  in - so a key pressed next reaches what the caller focuses, not the drawer's opener. */
export async function closeDetails(page: Page): Promise<void> {
  if (!(await marginShell(page).isVisible())) return;
  await expect(detailsClose(page), "Details draws no Close").toHaveCount(1);
  await detailsClose(page).click();
  await expect(marginShell(page), "Details did not close from its Close").toBeHidden();
  await page.evaluate(() => {
    const { promise, resolve } = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    return promise;
  });
  expect(
    await page.evaluate(
      (drawer) => document.querySelector(drawer)?.contains(document.activeElement) === true,
      SELECTORS.details
    ),
    "closing Details left the focus inside it"
  ).toBe(false);
}

/** The icon rail the shell draws on the issue page in place of its sidebar. */
export function iconRail(page: Page): Locator {
  return page.getByTestId("icon-rail");
}

/** The Contents pane at the page's left, from 1024 px up. */
export function contentsPane(page: Page): Locator {
  return page.getByTestId("issue-contents");
}

/** The rows of the Contents list, wherever it is drawn: the pane, or the Outline sheet. */
export function contentsEntries(scope: Locator): Locator {
  return scope.locator("[data-contents-entry]");
}

/** The Outline control beside the presets, below 1024 px, and the sheet it opens. */
export function outlineControl(page: Page): Locator {
  return page.getByRole("button", { exact: true, name: CONTROL_LABELS.outline });
}

export function outlineSheet(page: Page): Locator {
  return page.getByTestId("issue-outline-sheet");
}

/** The Focus branch toggle in the head of the Contents list. */
export function focusBranchControl(scope: Locator): Locator {
  return scope.getByRole("button", { exact: true, name: CONTROL_LABELS.focus_branch });
}

/** The one place to write, at the foot of the outline. */
export function composer(page: Page): Locator {
  return page.getByTestId("issue-composer");
}

/** The line of the place to write, whatever it is set to write: the one that is drawn. The
 *  line that writes on the issue stays mounted, hidden, while the line answers or edits. */
export function composerField(page: Page): Locator {
  return composer(page).locator("textarea:visible");
}

/** One of the focused row's actions under its words: Reply, Answer or Change answer. */
export function rowAct(row: Locator, act: "answer" | "change-answer" | "reply"): Locator {
  return row.locator(`[data-row-act="${act}"]`);
}

/** The issue's history, in Details. */
export function historyPanel(page: Page): Locator {
  return page.getByTestId("issue-history");
}

/** One turn of a thread agents wrote among themselves, in the history, by its own id. */
export function historyItem(page: Page, id: string): Locator {
  return historyPanel(page).locator(`[data-history-item="${id}"]`);
}

/** The reader's pins, in Details. */
export function pinsList(page: Page): Locator {
  return page.getByTestId("issue-pins");
}

/** The lines of the issue's history, in Details: an event, an upload or a thread agents wrote
 *  among themselves, each carrying its `data-event-seq`. */
export function historyEntries(page: Page): Locator {
  return historyPanel(page).locator("[data-event-seq]");
}

/** Puts the focus on node `id`'s row, as a press or the arrows put it there. */
export async function focusRow(page: Page, id: string): Promise<Locator> {
  const row = nodeRow(page, id).first();
  await row.focus();
  await expect(row.locator("[data-node-card]").first()).toBeVisible();
  return row;
}

/** The focused node's facts in Details, opened on ask `askId` of `issueKey` at the ask's own
 *  address, which focuses its node: where an open question's hand-on is, the old conversation
 *  card's control (`FocusedNodeFacts.tsx`, dispatch://INFRA-1196 R14, R19). */
export async function askFacts(page: Page, issueKey: string, askId: string): Promise<Locator> {
  await gotoIssue(page, `/issues/${issueKey}/asks/${askId}`);
  await expect.poll(() => focusedNodeId(page)).toBe(askId);
  await openDetails(page);
  const facts = page.getByTestId("focused-node-facts");
  await expect(facts).toHaveAttribute("data-focused-node", askId);
  return facts;
}

/** Writes `text` in the line at the foot of the outline and sends it with Enter, the line's own
 *  key: a comment on the issue, or the reply or edit the line is set to. */
export async function sendFromComposer(page: Page, text: string): Promise<void> {
  const field = composerField(page).first();
  await field.fill(text);
  await field.press("Enter");
  await expect(field).toHaveValue("");
}

/** Replies to node `id` the way a reader does: its row's Reply sets the line at the foot of the
 *  outline to it, and the line sends `text`. */
export async function replyFromRow(page: Page, id: string, text: string): Promise<void> {
  const row = await focusRow(page, id);
  await rowAct(row, "reply").click();
  await expect(composer(page).locator("[data-composer-target]")).toBeVisible();
  await sendFromComposer(page, text);
}

/** A control of the page by the role and accessible name the old page's inventory recorded for it
 *  (`e2e/fixtures/issue-page-inventory.json`), which is how c-nothing-lost acts on each name that
 *  writes today. */
export function offered(page: Page, role: Parameters<Page["getByRole"]>[0], name: string): Locator {
  return page.getByRole(role, { exact: true, name });
}

/** What a `select` offers, as its value and its label: a test that writes a property picks the
 *  option it means rather than an index (c-nothing-lost). */
export function optionsOf(control: Locator): Promise<{ label: string; value: string }[]> {
  return control.evaluateAll((found) =>
    found.flatMap((element) =>
      [...(element as HTMLSelectElement).options].map((option) => ({
        label: option.label,
        value: option.value,
      }))
    )
  );
}

/** Leaves whatever control holds the focus, so a key the page binds in reading mode reaches it.
 *  `body.focus()` does not do it: `body` takes no focus and the control keeps it. */
export async function focusThePage(page: Page): Promise<void> {
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await expect.poll(() => page.evaluate(() => document.activeElement === document.body)).toBe(true);
}

/** A `"` inside an attribute value would end the selector's string, and no id, slug or kind the
 *  page draws holds one; a value that did would otherwise match nothing silently. */
function cssValue(value: string): string {
  expect(value, `${value} cannot be written into a selector`).not.toContain('"');
  return value;
}

// ---------------------------------------------------------------------------------------------
// The readers: one `page.evaluate` each, so a page that draws none of this answers an empty list
// instead of timing out.
// ---------------------------------------------------------------------------------------------

/** How wide one box lays its text out against how wide it draws it: `scroll` past `client` is
 *  text the box hides, which no word boundary decided. `nowrap` says the box lays its text out
 *  on one line at all. */
export interface LineBox {
  readonly client: number;
  readonly nowrap: boolean;
  readonly scroll: number;
}

/** One row of the tree, as the page draws it. */
export interface DrawnRow {
  /** The answerer an answered ask's own row names, or null where the row draws no answer. */
  readonly answerAuthor: string | null;
  /** The text of the answer line an answered ask draws on its own row, or null where none. */
  readonly answerLine: string | null;
  /** The whole of what the person wrote, as the open body draws it, every character kept; null
   *  where no body of this row draws an answer's text. */
  readonly answerText: string | null;
  /** The author the row names: a name, or the page's words for an author nobody recorded. */
  readonly author: string | null;
  /** The `data-glyph` token of the shape drawn beside the author, or null where none is. */
  readonly authorShape: string | null;
  /** Whether the row draws a body of its own. */
  readonly body: boolean;
  /** Where the row is, or null while it is not rendered. */
  readonly box: Box | null;
  /** The text of each chip on the row, in order. */
  readonly chips: readonly string[];
  /** The `data-gap-kind` of each gap the row draws, in order. */
  readonly gapKinds: readonly string[];
  /** `aria-expanded`, or null for a row that holds no children. */
  readonly expanded: string | null;
  /** `data-folded-count`, or null for a row that is not folded. */
  readonly foldedCount: string | null;
  /** The `data-glyph` token of each shape drawn on the row, with the accessible name of each. */
  readonly glyphs: readonly { readonly name: string; readonly token: string }[];
  readonly id: string | null;
  /** The row's own first line, as drawn. */
  readonly firstLine: string | null;
  /** The first line's own box against the text it holds: the reading a text reading cannot
   *  make, since a box that hides the tail of its text leaves every character of it in
   *  `firstLine` (R1). Null where the row draws no first line. */
  readonly firstLineBox: LineBox | null;
  /** `aria-level`, as drawn. */
  readonly level: string | null;
  /** Whether the row is drawn only as the ancestor of a match in this preset. */
  readonly context: boolean;
  /** Whether the row's body is pinned open. */
  readonly pinned: boolean;
  /** The one line a filter's row draws for its parent where the filter draws that parent as no
   *  row: its id and kind, and the line as drawn. Null where the row draws none. */
  readonly parent: { readonly id: string; readonly line: string; readonly type: string } | null;
  readonly role: string | null;
  /** `data-node-state`, the model's own token, or null for a type it gives no state. */
  readonly state: string | null;
  /** The state word the row draws, or null where it draws none. */
  readonly stateWord: string | null;
  /** The row's own `tabindex`, which the WAI-ARIA tree pattern roves over its rows. */
  readonly tabIndex: number;
  /** The whole visible text of the row, its body included. */
  readonly text: string;
  /** `data-node-type`, the model's own token for the kind of node. */
  readonly type: string | null;
  /** The one word a decision the model marks unsure says of itself, or null where none. */
  readonly unsure: string | null;
  readonly visible: boolean;
  /** The node's words as the row draws them whole, or null where it draws its one line. */
  readonly words: string | null;
  /** The same reading of the box a folded row's one line sits in, which is what hides a word
   *  beside the line rather than in it. Null where the row draws no first line. */
  readonly wordsBox: LineBox | null;
}

/** Every row the page draws, in document order. */
export function drawnRows(page: Page): Promise<DrawnRow[]> {
  return page.evaluate((selectors) => {
    const text = (element: Element | null): string | null =>
      element === null ? null : (element.textContent ?? "").replace(/\s+/g, " ").trim();
    const box = (element: Element) => {
      const rect = element.getBoundingClientRect();
      if (rect.width === 0 && rect.height === 0) return null;
      return { height: rect.height, width: rect.width, x: rect.x, y: rect.y };
    };
    const lineBox = (element: Element | null | undefined) =>
      element === null || element === undefined
        ? null
        : {
            client: element.clientWidth,
            nowrap: window.getComputedStyle(element).whiteSpace === "nowrap",
            scroll: element.scrollWidth,
          };
    const rows = [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)];
    return rows.map((row) => {
      const firstLine = row.querySelector(selectors.nodeFirstLine);
      const answerLine = row.querySelector(selectors.answerLine);
      return {
        answerAuthor: text(answerLine?.querySelector(selectors.answerAuthor) ?? null),
        answerLine: text(answerLine),
        answerText: row.querySelector(selectors.answerText)?.textContent ?? null,
        author: text(row.querySelector(selectors.nodeAuthor)),
        authorShape:
          row
            .querySelector(`${selectors.nodeAuthor} ${selectors.nodeGlyph}`)
            ?.getAttribute("data-glyph") ?? null,
        body: row.querySelector(selectors.nodeBody) !== null,
        box: box(row),
        chips: [...row.querySelectorAll(selectors.nodeChip)].map((chip) => text(chip) ?? ""),
        gapKinds: [...row.querySelectorAll(selectors.gap)].map(
          (gap) => gap.getAttribute("data-gap-kind") ?? ""
        ),
        context: row.hasAttribute("data-node-context"),
        expanded: row.getAttribute("aria-expanded"),
        firstLine: text(firstLine),
        firstLineBox: lineBox(firstLine),
        foldedCount: row.getAttribute("data-folded-count"),
        glyphs: [...row.querySelectorAll(selectors.nodeGlyph)].map((glyph) => ({
          name:
            glyph.getAttribute("aria-label") ??
            text(glyph.querySelector("title")) ??
            text(glyph.querySelector("desc")) ??
            "",
          token: glyph.getAttribute("data-glyph") ?? "",
        })),
        id: row.getAttribute("data-node-id"),
        level: row.getAttribute("aria-level"),
        pinned: row.querySelector(`${selectors.nodeBody}[data-pinned]`) !== null,
        parent: (() => {
          const line = row.querySelector(`[data-node-content] > ${selectors.parentLine}`);
          return line === null
            ? null
            : {
                id: line.getAttribute("data-parent-id") ?? "",
                line: text(line.querySelector("[data-author-text]")) ?? "",
                type: line.getAttribute("data-parent-type") ?? "",
              };
        })(),
        role: row.getAttribute("role"),
        state: row.getAttribute("data-node-state"),
        stateWord: text(row.querySelector(selectors.nodeState)),
        tabIndex: (row as HTMLElement).tabIndex,
        text: text(row) ?? "",
        type: row.getAttribute("data-node-type"),
        unsure: text(row.querySelector(selectors.nodeUnsure)),
        visible: row.getClientRects().length > 0,
        words: text(row.querySelector(selectors.nodeWords)),
        wordsBox: lineBox(firstLine?.parentElement),
      };
    });
  }, SELECTORS);
}

/** Where the parts of one row stand on the page, in CSS pixels (c-row, R34): its bullet, the
 *  left edge of its words and the right end of their first line, its tag, its byline and its
 *  time, and the colours a row draws them in. */
export interface RowLayout {
  /** The author's words' colour, and the time's, as computed. */
  readonly bylineColor: string | null;
  /** The author's name as drawn, and its computed colour. */
  readonly author: { readonly color: string; readonly name: string } | null;
  /** The left of the byline - the author and the time - and the top of its box. */
  readonly byline: { readonly left: number; readonly top: number } | null;
  /** The bullet's left, its dot's computed fill, and the fill its kind's token resolves to. */
  readonly bullet: { readonly fill: string; readonly kindFill: string; readonly left: number };
  readonly context: boolean;
  /** The fold control a row that holds nodes draws: its name and whether it is the bullet. */
  readonly fold: { readonly isBullet: boolean; readonly name: string } | null;
  /** Whether the first thing the row's content draws is the node's words, so nothing - no
   *  kind, mark or label - stands between the bullet and them. */
  readonly wordsFirst: boolean;
  readonly id: string;
  /** The words' first line: its top, its height and the right end of its last character. */
  readonly line: { readonly bottom: number; readonly right: number; readonly top: number };
  /** The text of the control that opens a cut text, or null where the words are whole. */
  readonly more: string | null;
  /** Whether these are the reader's own words, which carry the ink rule. */
  readonly own: boolean;
  /** Whether the ink rule is drawn at the content's leading edge. */
  readonly rule: boolean;
  /** The page's quiet grey, as computed, for comparison with the byline. */
  readonly quiet: string;
  /** The tag a typed kind draws, its text and its box; null where none. */
  readonly tag: {
    readonly left: number;
    readonly right: number;
    readonly text: string;
    readonly top: number;
  } | null;
  readonly time: string | null;
  readonly type: string;
  /** How many pixels of the words the row's box hides. */
  readonly wordsHidden: number;
  /** The left edge of the node's words, or of a folded row's one line. */
  readonly wordsLeft: number;
}

/** Every visible row's layout, in document order (c-row). */
export function rowLayouts(page: Page): Promise<RowLayout[]> {
  return page.evaluate((selectors) => {
    const fillOf = (token: string): string => {
      const probe = document.createElement("i");
      probe.style.backgroundColor = `var(--color-${token})`;
      document.body.append(probe);
      const fill = getComputedStyle(probe).backgroundColor;
      probe.remove();
      return fill;
    };
    const quiet = (() => {
      const probe = document.createElement("i");
      probe.style.color = "var(--color-page-quiet)";
      document.body.append(probe);
      const color = getComputedStyle(probe).color;
      probe.remove();
      return color;
    })();
    const KIND: Record<string, string> = {
      ask: "page-question",
      callout: "page-issue",
      check: "page-check",
      child_issue: "page-child",
      decision: "page-decision",
      issue: "page-issue",
      plan_task: "page-step",
      requirement: "page-statement",
      result: "page-check",
    };
    const rows = [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)];
    return rows
      .filter((row) => row.getClientRects().length > 0)
      .map((row) => {
        const type = row.getAttribute("data-node-type") ?? "";
        const author = row.querySelector(selectors.nodeAuthor);
        const kindToken =
          KIND[type] ??
          (author?.getAttribute("data-author-kind") === "person" ? "page-person" : "page-bullet");
        const content = row.querySelector("[data-node-content]");
        const words =
          row.querySelector(selectors.nodeWords) ??
          row.querySelector(selectors.nodeFirstLine)?.parentElement ??
          null;
        const bullet = row.querySelector("[data-node-bullet]");
        const dot = bullet?.querySelector("i") ?? null;
        // The words' first line: the boxes of their text alone - a range over an element answers
        // the element's own box too, which spans the whole column - the first line being those
        // whose top is the first box's.
        const rects: DOMRect[] = [];
        if (words !== null) {
          const walker = document.createTreeWalker(words, NodeFilter.SHOW_TEXT);
          for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
            const range = document.createRange();
            range.selectNodeContents(node);
            rects.push(...[...range.getClientRects()].filter((rect) => rect.width > 0));
          }
        }
        const top = rects[0]?.top ?? 0;
        const first = rects.filter((rect) => Math.abs(rect.top - top) < 4);
        const tag = row.querySelector(selectors.nodeTag);
        const tagBox = tag?.getBoundingClientRect();
        const byline = row.querySelector("[data-node-byline]");
        const bylineBox = byline?.getBoundingClientRect();
        const time = row.querySelector("[data-node-time]");
        const name = author?.querySelector("[data-author-text], [data-page-chrome]") ?? null;
        const fold = bullet?.matches("button") === true ? bullet : null;
        return {
          author:
            name === null
              ? null
              : { color: getComputedStyle(name).color, name: (name.textContent ?? "").trim() },
          byline: bylineBox === undefined ? null : { left: bylineBox.left, top: bylineBox.top },
          bylineColor: time === null ? null : getComputedStyle(time).color,
          bullet: {
            fill: dot === null ? "" : getComputedStyle(dot).backgroundColor,
            kindFill: fillOf(kindToken),
            left: bullet?.getBoundingClientRect().left ?? -1,
          },
          context: row.hasAttribute("data-node-context"),
          fold:
            row.getAttribute("aria-expanded") === null
              ? null
              : {
                  isBullet: fold !== null,
                  name:
                    (fold ?? row.querySelector("button[aria-label]"))?.getAttribute("aria-label") ??
                    "",
                },
          id: row.getAttribute("data-node-id") ?? "",
          line: {
            bottom: Math.max(...first.map((rect) => rect.bottom), top),
            right: Math.max(...first.map((rect) => rect.right), 0),
            top,
          },
          more:
            row.querySelector("[data-node-more]")?.textContent?.replace(/\s+/g, " ").trim() ?? null,
          own: content?.hasAttribute("data-node-own") === true,
          quiet,
          rule: content !== null && getComputedStyle(content).boxShadow.includes("inset"),
          tag:
            tag === null || tagBox === undefined
              ? null
              : {
                  left: tagBox.left,
                  right: tagBox.right,
                  text: (tag.textContent ?? "").trim(),
                  top: tagBox.top,
                },
          time: time?.textContent ?? null,
          type,
          wordsFirst: content?.firstElementChild === words,
          wordsHidden: words === null ? 0 : Math.max(0, words.scrollHeight - words.clientHeight),
          wordsLeft: words?.getBoundingClientRect().left ?? -1,
        };
      });
  }, SELECTORS);
}

/** One fact a node's body draws: the label of a `dt` and the text of the `dd` after it. */
export interface DrawnFact {
  readonly label: string;
  readonly value: string;
}

/** The facts `body` draws, in order (`web/src/features/issue/NodeFacts.tsx`): a check's
 *  attributes, and what a recorded result says of itself. Each pair is read as a pair, because
 *  the body's own text holds no divider between a label and its value - "failed" and "2" are
 *  both in it whatever the other says, and `observed` text of the result's own may hold either
 *  word - so a reading of that text cannot say what a fact is, or that a fact is absent. */
export function factsOf(body: Locator): Promise<DrawnFact[]> {
  return body.evaluate((element) =>
    [...element.querySelectorAll("dt")].map((term) => ({
      label: (term.textContent ?? "").replace(/\s+/g, " ").trim(),
      value: (term.nextElementSibling?.textContent ?? "").replace(/\s+/g, " ").trim(),
    }))
  );
}

/** The row `id` sits under, by the tree's own `aria-level`: the nearest row before it one level
 *  up. `null` for a row at the top level, and `undefined` for a row the page does not draw, which
 *  a test tells apart from "under the issue". */
export async function parentOfRow(page: Page, id: string): Promise<string | null | undefined> {
  const rows = await drawnRows(page);
  const index = rows.findIndex((row) => row.id === id);
  if (index === -1) return undefined;
  const level = Number(rows[index]?.level ?? "");
  if (!Number.isFinite(level)) return null;
  for (let before = index - 1; before >= 0; before -= 1) {
    const candidate = rows[before];
    if (candidate === undefined) continue;
    if (Number(candidate.level ?? "") === level - 1) return candidate.id;
  }
  return null;
}

/** The chain of ancestors above `id`, outermost first, as the drawn levels give it. */
export async function ancestorsOfRow(page: Page, id: string): Promise<string[]> {
  const chain: string[] = [];
  for (let at: string | null | undefined = id; at !== null && at !== undefined; ) {
    const parent: string | null | undefined = await parentOfRow(page, at);
    if (parent === null || parent === undefined) break;
    chain.unshift(parent);
    at = parent;
  }
  return chain;
}

/** The id of the row the keyboard focus is on: the row holding the focus, or the row the tree's
 *  `aria-activedescendant` names, whichever the page uses. */
export function focusedNodeId(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const active = document.activeElement;
    const own = active?.closest(selectors.nodeRow) ?? null;
    if (own !== null) return own.getAttribute("data-node-id");
    const named = active?.getAttribute("aria-activedescendant") ?? null;
    if (named === null) return null;
    const row = document.getElementById(named)?.closest(selectors.nodeRow) ?? null;
    return row?.getAttribute("data-node-id") ?? null;
  }, SELECTORS);
}

/** Whether the keyboard focus is inside `scope`: a chord that reaches a target has to put the
 *  focus in it, not only draw it (c-keyboard). A scope the page does not draw answers false. */
export async function focusInside(scope: Locator): Promise<boolean> {
  if ((await scope.count()) === 0) return false;
  return await scope
    .first()
    .evaluate(
      (element) => element.contains(document.activeElement) || element === document.activeElement
    );
}

/**
 * Whether every pixel of `scope` is inside the viewport and clear of the sticky identity block,
 * which is what "brings a target on screen" is read as (c-keyboard, c-open-questions) and what
 * the page's own `bringOnScreen` (`web/src/features/issue/identity-block.ts`) places a target
 * by: at or below the block's lower edge, and above the foot of the screen. Details' drawer is a
 * layer over the page, the block included, so a target inside it needs only the viewport.
 *
 * A scope taller than the screen can satisfy neither, by construction, so a caller reads the
 * control the page leaves the reader on rather than the whole part around it, as the page
 * itself does (`useIssueKeymap.ts`'s `showPart`).
 */
export async function onScreen(scope: Locator): Promise<boolean> {
  if ((await scope.count()) === 0) return false;
  return await scope.first().evaluate((element, selectors) => {
    const rect = element.getBoundingClientRect();
    // What stays at the top of the screen: the identity block from 761 px up, and the filter
    // row's band at 760 px and below - whichever is `position: sticky` and at the top now. The
    // `<header>` itself draws no box (`IssueHeader.tsx`).
    let sticky = 0;
    if (element.closest(selectors.details) === null) {
      for (const start of [selectors.identityBand, selectors.presetRow]) {
        for (let at = document.querySelector(start); at !== null; at = at.parentElement) {
          if (getComputedStyle(at).position !== "sticky") continue;
          const band = at.getBoundingClientRect();
          if (band.top <= 1) sticky = Math.max(sticky, band.bottom);
          break;
        }
      }
    }
    return (
      rect.height > 0 &&
      rect.top >= sticky - 1 &&
      rect.bottom <= window.innerHeight + 1 &&
      rect.left >= -1 &&
      rect.right <= window.innerWidth + 1
    );
  }, SELECTORS);
}

/** Whether the caret is in a node's own text: the focus is inside an editable inside a row, which
 *  is what the edit key does and what Escape leaves (c-keyboard). */
export function caretInNode(page: Page): Promise<boolean> {
  return page.evaluate((selectors) => {
    const editable = document.activeElement?.closest('[contenteditable="true"]') ?? null;
    return editable !== null && editable.closest(selectors.nodeRow) !== null;
  }, SELECTORS);
}

/** The node id the tree's `aria-activedescendant` names, for a tree that roves with it rather
 *  than by moving the focus itself. Null when the tree names none. */
export function treeActiveDescendant(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const tree = document.querySelector(`${selectors.issuePage} ${selectors.tree}`);
    const named = tree?.getAttribute("aria-activedescendant") ?? null;
    if (named === null) return null;
    return (
      document.getElementById(named)?.closest(selectors.nodeRow)?.getAttribute("data-node-id") ??
      null
    );
  }, SELECTORS);
}

/** The name of the control the page is on: the pressed toggle's own name at a width that draws
 *  the row, and the one control's name part below it. Null while the page draws neither. */
export function activePreset(page: Page): Promise<string | null> {
  return page.evaluate((selectors) => {
    const row = document.querySelector(selectors.presetRow);
    if (row === null) return null;
    const pressed = row.querySelector('button[aria-pressed="true"] [data-preset-name]');
    if (pressed !== null) return (pressed.textContent ?? "").trim();
    const chooser = row.querySelector('button[aria-haspopup="menu"] [data-preset-name]');
    return chooser === null ? null : (chooser.textContent ?? "").trim();
  }, SELECTORS);
}

/** What the page says in its own words: the text of each `[data-page-chrome]` element with every
 *  `[data-author-text]` descendant removed, which is what c-plain-words reads word by word. */
export function chromeTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const found: string[] = [];
    for (const element of document.querySelectorAll(
      `${selectors.issuePage} ${selectors.pageChrome}`
    )) {
      const copy = element.cloneNode(true) as Element;
      for (const authored of copy.querySelectorAll(selectors.authorText)) authored.remove();
      const text = (copy.textContent ?? "").replace(/\s+/g, " ").trim();
      if (text !== "") found.push(text);
    }
    return found;
  }, SELECTORS);
}

/** Every element of the page that carries visible text and no `[data-page-chrome]` or
 *  `[data-author-text]` ancestor or self, which is text the page divides into neither. */
export function undividedTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const found: string[] = [];
    const walker = document.createTreeWalker(
      document.querySelector(selectors.issuePage) ?? document.createElement("div"),
      NodeFilter.SHOW_TEXT
    );
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const text = (node.textContent ?? "").replace(/\s+/g, " ").trim();
      if (text === "") continue;
      const element = node.parentElement;
      if (element === null) continue;
      if (element.closest(`${selectors.pageChrome}, ${selectors.authorText}`) !== null) continue;
      if (element.getClientRects().length === 0) continue;
      found.push(text);
    }
    return found;
  }, SELECTORS);
}

/** Every `title` attribute the page carries, which is every tooltip a pointer can raise. */
export function tooltipTexts(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    return [...document.querySelectorAll(`${selectors.issuePage} [title]`)].map(
      (element) => element.getAttribute("title") ?? ""
    );
  }, SELECTORS);
}

/** The whole visible text of the tree, for a reading taken before and after a pointer crosses it. */
export function treeText(page: Page): Promise<string> {
  return page.evaluate((selectors) => {
    const tree = document.querySelector(`${selectors.issuePage} ${selectors.tree}`);
    return ((tree?.textContent ?? "") as string).replace(/\s+/g, " ").trim();
  }, SELECTORS);
}

/** Where the sticky identity block ends, which is the line the focused row must clear; 0 where
 *  the block is not sticky, which is 760 px and below. */
export function identityBlockBottom(page: Page): Promise<number | null> {
  return page.evaluate((selectors) => {
    const block = document.querySelector(selectors.identityBand);
    if (block === null) return null;
    const rect = block.getBoundingClientRect();
    return getComputedStyle(block).position === "sticky" ? rect.bottom : 0;
  }, SELECTORS);
}

/** What stays at the top of the screen and what scrolls away (ruling 5 of
 *  dispatch://INFRA-1196/artifact/build-rulings-md), read where the page stands now. */
export interface StickyReading {
  /** The identity block - the key's row and the title - by its box on the screen. */
  readonly block: { readonly bottom: number; readonly height: number; readonly top: number };
  /** Whether the header says it is stuck (`data-condensed`), which draws its shadow. */
  readonly condensed: boolean;
  /** The band that holds the filter row, by its box on the screen. */
  readonly filters: { readonly bottom: number; readonly height: number; readonly top: number };
  /** The properties line, by its box on the screen. */
  readonly properties: { readonly bottom: number; readonly top: number };
  /** The height the page publishes for whatever stays (`--issue-identity-height`), in px. */
  readonly published: number;
  /** How far the page has scrolled. */
  readonly scrolled: number;
  /** The title's box and its type, as computed. */
  readonly title: {
    readonly bottom: number;
    readonly line: string;
    readonly size: string;
    readonly top: number;
    readonly weight: string;
  };
}

/** `StickyReading` of the page, or null where it draws no header or no filter row. The filter
 *  row's band is the row's nearest `position: sticky` ancestor, or the row itself where none is. */
export function stickyReading(page: Page): Promise<StickyReading | null> {
  return page.evaluate((selectors) => {
    const header = document.querySelector(selectors.identityBlock);
    const block = document.querySelector(selectors.identityBand);
    const title = header?.querySelector("h1") ?? null;
    const properties = header?.querySelector('[data-testid="issue-state-actions"]') ?? null;
    const row = document.querySelector(selectors.filterRow);
    if (
      header === null ||
      block === null ||
      title === null ||
      properties === null ||
      row === null
    ) {
      return null;
    }
    let band: Element = row;
    for (let at: Element | null = row; at !== null; at = at.parentElement) {
      if (getComputedStyle(at).position === "sticky") {
        band = at;
        break;
      }
    }
    const box = (element: Element) => {
      const rect = element.getBoundingClientRect();
      return { bottom: rect.bottom, height: rect.height, top: rect.top };
    };
    const type = getComputedStyle(title);
    return {
      block: box(block),
      condensed: header.getAttribute("data-condensed") === "true",
      filters: box(band),
      properties: box(properties),
      published: Number.parseFloat(
        getComputedStyle(document.documentElement).getPropertyValue("--issue-identity-height")
      ),
      scrolled: window.scrollY,
      title: { ...box(title), line: type.lineHeight, size: type.fontSize, weight: type.fontWeight },
    };
  }, SELECTORS);
}

/** The filter row as it is drawn (ruling 5: one line at every width). */
export interface FilterRowReading {
  /** The name of each control the row draws, left to right: what a reader hears it called. */
  readonly controls: readonly string[];
  /** The row's own height in CSS pixels. */
  readonly height: number;
  /** How many lines its controls stand on: controls whose boxes overlap vertically are one. */
  readonly lines: number;
  /** How far the row's content runs past its own right edge, in px; 0 where it fits. */
  readonly overflow: number;
}

/** `FilterRowReading` of the page, or null where it draws no filter row. A control is a button,
 *  a box or a select the row draws on the page itself: one inside a menu, a dialog or a part
 *  the row keeps shut is not on the row. */
export function filterRowReading(page: Page): Promise<FilterRowReading | null> {
  return page.evaluate((selectors) => {
    const row = document.querySelector(selectors.filterRow);
    if (row === null) return null;
    const controls = [...row.querySelectorAll<HTMLElement>("button, input, select")]
      .filter(
        (control) =>
          control.checkVisibility() &&
          control.closest('[role="menu"], [role="dialog"], [hidden]') === null &&
          control.getAttribute("type") !== "file"
      )
      .map((control) => {
        // The name a reader hears: the label, what names it, or its own words less the parts
        // hidden from them (a control's `▾`).
        const heard = control.cloneNode(true) as HTMLElement;
        for (const hidden of heard.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
        const named = control.getAttribute("aria-labelledby");
        const name =
          control.getAttribute("aria-label") ??
          (named === null
            ? null
            : named
                .split(" ")
                .map((id) => document.getElementById(id)?.textContent ?? "")
                .join(" ")) ??
          heard.textContent ??
          "";
        return { box: control.getBoundingClientRect(), name: name.replace(/\s+/g, " ").trim() };
      })
      .sort((a, b) => a.box.left - b.box.left);
    let lines = 0;
    let lineBottom = Number.NEGATIVE_INFINITY;
    for (const { box } of [...controls].sort((a, b) => a.box.top - b.box.top)) {
      if (box.top >= lineBottom - 1) lines += 1;
      lineBottom = Math.max(lineBottom, box.bottom);
    }
    const edge = row.getBoundingClientRect();
    return {
      controls: controls.map((control) => control.name),
      height: edge.height,
      lines,
      overflow: Math.max(0, ...controls.map((control) => control.box.right - edge.right)),
    };
  }, SELECTORS);
}

/** The header as R13 measures it: the band from the top of the identity block to the top of the
 *  preset row. c-properties, c-typeface. */
export interface HeaderReading {
  /** The band's height in CSS pixels. */
  readonly height: number;
  /** The words of each line drawn in the band, top to bottom. */
  readonly lines: readonly string[];
}

/**
 * The header's band and the lines drawn in it. A line is a run of drawn text: every text node of
 * the page inside the band and inside the identity block's own column (so a panel beside it
 * counts for nothing), one box per line the text wraps to, and a form control's value as a box
 * of its own font size at the control's middle, since a select draws its value without a text
 * node and a control can stand taller than the line it sits on. Boxes that overlap vertically
 * are one line. Text a box of at most a pixel clips away (`sr-only`), or that is invisible or
 * transparent, is not drawn. Null when the page draws no identity block or no preset row.
 */
export function headerReading(page: Page): Promise<HeaderReading | null> {
  return page.evaluate((selectors) => {
    const block = document.querySelector(selectors.identityBlock);
    const presets = document.querySelector(selectors.presetRow);
    const root = document.querySelector(selectors.issuePage);
    if (block === null || presets === null || root === null) return null;
    // The `<header>` draws no box of its own (`display: contents`), so its column is the box
    // around what it holds: the identity block, the properties line and what a write answers.
    const parts = [...block.children].map((part) => part.getBoundingClientRect());
    const column = {
      left: Math.min(...parts.map((part) => part.left)),
      right: Math.max(...parts.map((part) => part.right)),
      top: Math.min(...parts.filter((part) => part.height > 0).map((part) => part.top)),
    };
    const top = column.top;
    const bottom = presets.getBoundingClientRect().top;
    const inBand = (box: DOMRect): boolean =>
      box.width > 0 &&
      box.height > 0 &&
      box.bottom > top &&
      box.top < bottom &&
      box.right > column.left &&
      box.left < column.right;
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
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
      const content = node.textContent ?? "";
      const element = node.parentElement;
      if (content.trim() === "" || element === null || element.closest("select") !== null) continue;
      const range = document.createRange();
      range.selectNodeContents(node);
      const boxes = [...range.getClientRects()].filter(inBand);
      if (boxes.length === 0 || !drawn(element)) continue;
      // A text that wraps is one box per line; each line is given the characters drawn in it,
      // so a failure names the words of each line rather than the whole text on each.
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
    for (const control of root.querySelectorAll("select, input:not([type='hidden']), textarea")) {
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

/** The family `web/src/styles.css` declares for the typeface the app ships and draws in. */
export const TYPEFACE = "Inter";

/** The address the page's stylesheet loads the typeface from, read from its own `@font-face`
 *  rule; null when no rule declares the family. c-typeface. */
export function typefaceAddress(page: Page): Promise<string | null> {
  return page.evaluate((family) => {
    const find = (rules: CSSRuleList, base: string): string | null => {
      for (const rule of rules) {
        if (rule instanceof CSSFontFaceRule) {
          const named = rule.style.getPropertyValue("font-family").replace(/["']/g, "").trim();
          const source = /url\(["']?([^"')]+)["']?\)/.exec(rule.style.getPropertyValue("src"));
          if (named === family && source?.[1] !== undefined) return new URL(source[1], base).href;
        } else if (rule instanceof CSSGroupingRule) {
          const found = find(rule.cssRules, base);
          if (found !== null) return found;
        }
      }
      return null;
    };
    for (const sheet of document.styleSheets) {
      const found = find(sheet.cssRules, sheet.href ?? document.baseURI);
      if (found !== null) return found;
    }
    return null;
  }, TYPEFACE);
}

/** How each face of the typeface's family stands in the document's font set: `loaded` once the
 *  file arrived, `error` once its request failed. c-typeface. */
export function typefaceStatuses(page: Page): Promise<string[]> {
  return page.evaluate(
    (family) =>
      [...document.fonts]
        .filter((face) => face.family.replace(/["']/g, "") === family)
        .map((face) => face.status),
    TYPEFACE
  );
}

/** The stack the identity block's text is drawn in, as its computed `font-family`, each family
 *  unquoted. c-typeface. */
export function identityBlockFamilies(page: Page): Promise<string[]> {
  return page.evaluate((selectors) => {
    const block = document.querySelector(selectors.identityBlock);
    if (block === null) return [];
    return getComputedStyle(block)
      .fontFamily.split(",")
      .map((family) => family.replace(/["']/g, "").trim());
  }, SELECTORS);
}

/** One element that reaches past the box it is drawn in. */
export interface Overflowing {
  /** The box's own description: its tag and its node id where it has one. */
  readonly card: string;
  /** How far past the card the element reaches, in CSS pixels. */
  readonly over: number;
  readonly what: string;
}

/** Which boxes a measurement is read against: a node's row, the body it opens, or a card of
 *  the Verification filter, which draws its own (c-checks-readable). */
export type CardScope = "bodies" | "rows" | "verification";

/** The selector of the boxes one scope names. */
const VERIFICATION_CARD = "[data-verification-card]";

/**
 * Every element whose painted box reaches more than a pixel past the card it sits in. A card is a
 * node's row or its body: c-checks-readable reads "no element's box reaches past its card"
 * against both.
 */
export function boxesPastTheirCard(page: Page, cards: CardScope): Promise<Overflowing[]> {
  return page.evaluate(
    ({ cards, selectors }) => {
      const scope =
        cards === "rows"
          ? selectors.nodeRow
          : cards === "bodies"
            ? selectors.nodeBody
            : selectors.verificationCard;
      const describe = (element: Element): string => {
        const id = element.closest(selectors.nodeRow)?.getAttribute("data-node-id") ?? "";
        const own = (element.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40);
        return `${element.tagName.toLowerCase()}${id === "" ? "" : ` in ${id}`} "${own}"`;
      };
      const found: { card: string; over: number; what: string }[] = [];
      for (const card of document.querySelectorAll(`${selectors.issuePage} ${scope}`)) {
        const bounds = card.getBoundingClientRect();
        if (bounds.width === 0) continue;
        for (const element of card.querySelectorAll("*")) {
          const rect = element.getBoundingClientRect();
          if (rect.width === 0 && rect.height === 0) continue;
          const over = Math.max(bounds.left - rect.left, rect.right - bounds.right);
          if (over > 1) found.push({ card: describe(card), over, what: describe(element) });
        }
      }
      return found;
    },
    { cards, selectors: { ...SELECTORS, verificationCard: VERIFICATION_CARD } }
  );
}

/** Two pieces of text drawn over each other. */
export interface Overlap {
  readonly first: string;
  readonly second: string;
}

/**
 * Every pair of the page's own text boxes that share a point on screen, inside `scope`: a node's
 * rows, the bodies they open, or the Details drawer (its cards and its sections). Only a leaf
 * that carries text of its own is measured, so a box that holds another is not read as
 * overlapping it. Text kept for a screen reader alone (inside `.sr-only`, whose box clips it to
 * a pixel while its own spans keep their laid-out width) and a closed `select`'s options are no
 * box the page draws, and are left out.
 */
export function overlappingTexts(page: Page, cards: CardScope | "details"): Promise<Overlap[]> {
  return page.evaluate(
    ({ cards, selectors }) => {
      const scope =
        cards === "rows"
          ? selectors.nodeRow
          : cards === "bodies"
            ? selectors.nodeBody
            : cards === "verification"
              ? selectors.verificationCard
              : selectors.details;
      const boxes: { label: string; rect: DOMRect }[] = [];
      for (const root of document.querySelectorAll(`${selectors.issuePage} ${scope}`)) {
        for (const element of root.querySelectorAll("*")) {
          if (element.children.length > 0 || element.tagName === "OPTION") continue;
          if (element.closest(".sr-only") !== null) continue;
          const text = (element.textContent ?? "").replace(/\s+/g, " ").trim();
          if (text === "") continue;
          const rect = element.getBoundingClientRect();
          if (rect.width === 0 || rect.height === 0) continue;
          boxes.push({ label: `${element.tagName.toLowerCase()} "${text.slice(0, 40)}"`, rect });
        }
      }
      const found: { first: string; second: string }[] = [];
      for (let i = 0; i < boxes.length; i += 1) {
        for (let j = i + 1; j < boxes.length; j += 1) {
          const a = boxes[i];
          const b = boxes[j];
          if (a === undefined || b === undefined) continue;
          const apart =
            a.rect.right <= b.rect.left + 0.5 ||
            b.rect.right <= a.rect.left + 0.5 ||
            a.rect.bottom <= b.rect.top + 0.5 ||
            b.rect.bottom <= a.rect.top + 0.5;
          if (!apart) found.push({ first: a.label, second: b.label });
        }
      }
      return found;
    },
    { cards, selectors: { ...SELECTORS, verificationCard: VERIFICATION_CARD } }
  );
}

/** The kind each gap the page names carries, in document order: a gap with no kind reads as the
 *  empty string, so an unnamed gap is named in the failure rather than dropped. */
export function gapKinds(page: Page): Promise<string[]> {
  return page.evaluate(
    (selectors) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.gap}`)].map(
        (gap) => gap.getAttribute("data-gap-kind") ?? ""
      ),
    SELECTORS
  );
}

/** Everything the page shows, as a reader sees it: the rendered text of the page root, which
 *  leaves out what is hidden. A property drawn behind a control a reader has to open is not in
 *  it, which is what c-properties reads each value against. */
export function visibleText(page: Page): Promise<string> {
  return page.evaluate((selectors) => {
    const root = document.querySelector(selectors.issuePage);
    return root === null ? "" : (root as HTMLElement).innerText.replace(/\s+/g, " ").trim();
  }, SELECTORS);
}

/** The three things inside a node's own text that may scroll sideways (R17): a code block, a
 *  table and a picture. c-reflow names them by selector and leaves them out of its walk. */
export const AUTHOR_SCROLLERS = ["pre", "table", "img"] as const;

/** Each of those three that is drawn outside a `[data-author-text]` element, which is where the
 *  exception does not reach. */
export function scrollersOutsideAuthorText(page: Page): Promise<string[]> {
  return page.evaluate(
    ({ scrollers, selectors }) => {
      const found: string[] = [];
      for (const kind of scrollers) {
        for (const element of document.querySelectorAll(`${selectors.issuePage} ${kind}`)) {
          if (element.closest(selectors.authorText) !== null) continue;
          if (element.getClientRects().length === 0) continue;
          found.push(`${kind} in ${element.parentElement?.tagName.toLowerCase() ?? "?"}`);
        }
      }
      return found;
    },
    { scrollers: AUTHOR_SCROLLERS, selectors: SELECTORS }
  );
}

/** Every row of the tree that holds none of those three, which is the set c-reflow walks with
 *  `assertWhole`. */
export async function rowsWithoutAuthorScrollers(page: Page): Promise<Locator[]> {
  const rows = await drawnRows(page);
  const held = await page.evaluate(
    ({ scrollers, selectors }) =>
      [...document.querySelectorAll(`${selectors.issuePage} ${selectors.nodeRow}`)].map(
        (row) => row.querySelector(scrollers.join(", ")) !== null
      ),
    { scrollers: AUTHOR_SCROLLERS, selectors: SELECTORS }
  );
  const walked: Locator[] = [];
  rows.forEach((row, index) => {
    if (held[index] === true || !row.visible) return;
    walked.push(nodeRows(page).nth(index));
  });
  return walked;
}

// ---------------------------------------------------------------------------------------------
// Opening the page.
// ---------------------------------------------------------------------------------------------

/**
 * Opens one address of the issue page and waits for the identity block, which the old page and
 * the redesigned one both draw: the wait is what keeps a test's first failure an assertion of its
 * own rather than a navigation that never settles. The keymap binds only once sign-in resolves,
 * so a key pressed before this returns reaches no handler.
 */
export async function gotoIssue(page: Page, path: string): Promise<void> {
  await page.goto(path);
  await expect(identityBlock(page)).toBeVisible();
}

/** Opens `preset` on `key` at its own address. */
export async function openPreset(page: Page, key: string, preset: Preset): Promise<void> {
  await gotoIssue(page, presetPath(key, preset));
}

/** How many presses `pressEachUntilNone` makes before it gives up: well past the branches any
 *  seeded issue holds, so the bound costs a settled tree nothing and a tree that draws the
 *  control whatever is pressed ends as the caller's own failed reading rather than as this
 *  helper running until the test's deadline. */
const BRANCHES_OPENED_AT_MOST = 200;

/** Presses the first of `controls` and reads the page again, until none is left: a press draws
 *  the rows under it, and the controls they hold, so the set is read anew each time. The bound
 *  and the exit live here alone; `what` names the control in the failure past the bound. */
async function pressEachUntilNone(controls: Locator, what: string): Promise<void> {
  for (let pressed = 0; pressed < BRANCHES_OPENED_AT_MOST; pressed += 1) {
    const first = controls.first();
    if ((await first.count()) === 0) return;
    await first.click();
  }
  expect(
    await controls.count(),
    `the tree still draws ${what} after ${BRANCHES_OPENED_AT_MOST} openings`
  ).toBe(0);
}

/**
 * Opens every branch the fold left shut, which is what a reader does to read past one: the page
 * opens folded to the frontier (R1, `web/src/features/issue/tree-fold.ts`), so a check that
 * reads what a branch holds presses its twisty rather than asserting that the first drawing
 * carried it. The reader's own opening outranks the fold, so what this opens stays open.
 *
 * Only at a width that draws the whole tree: at 1024 px and below a row's twisty zooms into
 * that row instead of unfolding it in place (`IssueTree.tsx`), which is a move of its own.
 */
export async function unfoldEveryBranch(page: Page): Promise<void> {
  await pressEachUntilNone(
    nodeRows(page).getByRole("button", { exact: true, name: CONTROL_LABELS.unfold }),
    "a folded branch"
  );
}

/** Opens every branch the tree folded to one line because it sits below the drawn levels
 *  (`tree-fold.ts`'s `DRAWN_LEVELS`), so a test reads the nodes such a branch holds as rows of
 *  their own, as `unfoldEveryBranch` does with a twisty. A tree that draws none is left exactly
 *  as it is: how deep a node sits is the model's own placement, which is what a test may be
 *  reading. */
export async function openEveryDeeperBranch(page: Page): Promise<void> {
  await pressEachUntilNone(branchMoreRows(page).getByRole("button"), "a deeper branch");
}

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
 * Moves to `name` by pressing the filter row itself, however that row draws at the width a test
 * runs at (R2): a row of five toggles from 1280 px, and one control opening a menu of five at
 * 1024 px and below. A toggle already pressed is left as it is, since pressing it again turns
 * it off. `openPreset` goes to the control's own address instead, which is a load; this is for
 * a spec whose page is holding something a load would drop.
 */
export async function pickPreset(page: Page, name: string): Promise<void> {
  const row = presetRow(page);
  // The row has to be on the page before its shape can be read: a count taken while the page
  // is still loading answers zero for both shapes and would send every caller to the control
  // that is not there.
  await row.waitFor();
  const toggle = row.getByRole("button", { exact: true, name });
  if ((await toggle.count()) > 0) {
    if ((await toggle.getAttribute("aria-pressed")) !== "true") await toggle.click();
    return;
  }
  await presetChooser(page).click();
  await openMenu(page).getByRole("menuitem", { exact: true, name }).click();
}

/** Fails unless the page is on `name`, whichever shape the row has at this width: the row of
 *  toggles presses the one it is on (`aria-pressed`), and the one control below 1025 px draws
 *  its name and its signal as its own name (`web/src/features/issue/IssuePresets.tsx`). */
export async function expectActivePreset(page: Page, name: string): Promise<void> {
  const row = presetRow(page);
  await row.waitFor();
  const toggle = row.getByRole("button", { exact: true, name });
  if ((await toggle.count()) > 0) {
    await expect(toggle).toHaveAttribute("aria-pressed", "true");
    return;
  }
  await expect(presetChooser(page)).toHaveAccessibleName(new RegExp(`^${name} \\d+\\b`));
}

/** The identity block's one actions control, open. Close, reopen and the control that gives the
 *  issue a parent are behind it now (R11), where the nine-tab page drew them as bare buttons.
 *  The control toggles, so this opens it only while it is shut: a spec that asks twice leaves
 *  the actions on screen rather than hiding them again. */
export async function openIssueActions(page: Page): Promise<void> {
  const control = offered(page, "button", CONTROL_LABELS.issue_actions);
  if ((await control.getAttribute("aria-expanded")) !== "true") {
    await control.click();
  }
}

/** Presses one of the identity block's actions by the words it shows, which are its name
 *  (`CONTROL_LABELS.close_issue`, `CONTROL_LABELS.reopen_into_backlog`), opening the control
 *  that holds it first. */
export async function runIssueAction(page: Page, name: string): Promise<void> {
  await openIssueActions(page);
  await offered(page, "button", name).click();
}

/** Fails until the page has taken in that its issue is closed. The identity block's own line is
 *  what says so (R11): reopening is behind the actions control, so a closed issue no longer
 *  draws a bare control for a spec to wait on. */
export async function expectIssueClosed(page: Page): Promise<void> {
  await expect(page.getByText("This issue is closed.", { exact: true })).toBeVisible();
}

/**
 * The filter bodies' own marks (dispatch://INFRA-1196 ruling 5: each filter draws as its
 * approved picture), read by c-decisions, c-review-list, c-home, c-checks-readable and
 * c-plan-order:
 *
 * - **Decisions** `[data-decisions-body]`: each part `[data-decisions-part="open_questions" |
 *   "review" | "yours" | "others" | "agents"]` with its heading `[data-decisions-heading]` and
 *   count `[data-decisions-count]`, its rows the outline's own `[data-node-row]`; the foot's
 *   control is the `button` in `[data-decisions-foot]`.
 * - **Spec** `[data-spec-body]`: each Decided by cell `[data-spec-cell]` with `data-said`
 *   (`you`, `person`, `agent`; absent where no decision shaped the run) and
 *   `data-spec-statements`, the ids of the statements its bracket spans; each statement
 *   `[data-spec-statement]` with `data-spec-mark` and its line `[data-spec-because]`.
 * - **Plan** `[data-plan-body]`: the control `[data-plan-view="chart" | "steps"]`; the chart
 *   `[data-plan-chart]`, each step's row `[data-plan-bar-row]` with `data-plan-start`, its bar
 *   `[data-plan-bar]`, `[data-plan-no-size]`, what it needs `[data-plan-need]` and what it
 *   serves `[data-plan-serve]`; a cycle's one line `[data-plan-cycle]`.
 */
export interface DrawnPart {
  readonly count: string;
  readonly heading: string;
  readonly part: string;
  readonly rows: readonly string[];
}

/** Each part of the Decisions filter's body, in the order it is drawn. */
export function decisionsPartsDrawn(page: Page): Promise<DrawnPart[]> {
  return page.evaluate((selector) => {
    const text = (element: Element | null) =>
      (element?.textContent ?? "").replace(/\s+/g, " ").trim();
    return [...document.querySelectorAll(`${selector} [data-decisions-part]`)].map((part) => ({
      count: text(part.querySelector("[data-decisions-count]")),
      heading: text(part.querySelector("[data-decisions-heading]")),
      part: part.getAttribute("data-decisions-part") ?? "",
      rows: [...part.querySelectorAll("[data-node-row]")].map(
        (row) => row.getAttribute("data-node-id") ?? ""
      ),
    }));
  }, SELECTORS.issuePage);
}

/** The control at the Decisions filter's foot, which unfolds the agents' untied decisions. */
export function decisionsFoot(page: Page): Locator {
  return issuePage(page).locator("[data-decisions-foot] button");
}

/** One Decided by cell of the Spec filter: whose words it draws, the statements its bracket
 *  spans, and its drawn text. */
export interface DecidedByCell {
  readonly ids: readonly string[];
  readonly said: string | null;
  readonly text: string;
}

/** Each Decided by cell of the Spec filter, in the order they are drawn. */
export function decidedByCells(page: Page): Promise<DecidedByCell[]> {
  return page.evaluate(() =>
    [...document.querySelectorAll("[data-spec-cell]")].map((cell) => ({
      ids: (cell.getAttribute("data-spec-statements") ?? "").split(" ").filter(Boolean),
      said: cell.getAttribute("data-said"),
      text: (cell.textContent ?? "").replace(/\s+/g, " ").trim(),
    }))
  );
}

/** The one line under each statement of the Spec filter that names the decision that shaped
 *  it, by statement id; `""` for a statement that draws none. */
export function becauseLines(page: Page): Promise<Record<string, string>> {
  return page.evaluate(() =>
    Object.fromEntries(
      [...document.querySelectorAll("[data-spec-statement]")].map((row) => [
        row.getAttribute("data-node-id") ?? "",
        (row.querySelector("[data-spec-because]")?.textContent ?? "").replace(/\s+/g, " ").trim(),
      ])
    )
  );
}

/** One row of the Plan filter's chart: a step with where its bar starts, what it says of its
 *  size, the steps it needs and the statements it serves; or a cycle's one line. */
export interface ChartRow {
  readonly id: string;
  readonly kind: "bar" | "cycle";
  readonly needs: readonly string[];
  readonly serves: readonly string[];
  readonly size: string;
  /** The bar's left edge and its width, in px, or null for a cycle's line. */
  readonly bar: { readonly left: number; readonly width: number } | null;
  readonly start: number;
  readonly text: string;
}

/** Every row of the Plan filter's chart, in the order it is drawn. */
export function planChartRows(page: Page): Promise<ChartRow[]> {
  return page.evaluate(() => {
    const text = (element: Element | null) =>
      (element?.textContent ?? "").replace(/\s+/g, " ").trim();
    return [
      ...document.querySelectorAll("[data-plan-chart] :is([data-plan-bar-row], [data-plan-cycle])"),
    ].map((row) => {
      const bar = row.querySelector("[data-plan-bar]")?.getBoundingClientRect();
      return {
        bar: bar === undefined ? null : { left: bar.left, width: bar.width },
        id: row.getAttribute("data-node-id") ?? "",
        kind: row.hasAttribute("data-plan-cycle") ? ("cycle" as const) : ("bar" as const),
        needs: [...row.querySelectorAll("[data-plan-need]")].map(
          (need) => need.getAttribute("data-plan-need") ?? ""
        ),
        serves: [...row.querySelectorAll("[data-plan-serve]")].map(text),
        size: text(row.querySelector("[data-plan-no-size]")),
        start: Number(row.getAttribute("data-plan-start") ?? "-1"),
        text: text(row),
      };
    });
  });
}

/** One of the Plan filter's two controls, the chart or the ordered steps (R26). */
export function planView(page: Page, view: "chart" | "steps"): Locator {
  return issuePage(page).locator(`[data-plan-view="${view}"]`);
}

/**
 * The Verification filter's body (`[data-verification-body]`): each statement a card
 * `[data-verification-card]` with `data-node-id`, `data-statement-state` (met, not_met,
 * unknown) and `data-node-context`; inside it each check `[data-verification-check]` with
 * `data-node-id` and `data-node-state`, its words the control
 * `[data-verification-check-words]` that opens its facts, its latest result
 * `[data-verification-result]` (`data-result-stands` on the one its state stands on) and the
 * control `[data-verification-earlier]` that opens its earlier ones. Over them the summary
 * `[data-verification-summary]` with `[data-verification-covered]`, `[data-verification-bar]`,
 * `[data-verification-legend]`, `[data-verification-check-legend]` and
 * `[data-verification-commit]`, and the chips `[data-verification-chip]`.
 */
export interface DrawnCheck {
  /** How many results the check's fold holds beside its latest, as its control says it. */
  readonly earlier: string | null;
  readonly id: string;
  /** How many results the check draws. */
  readonly results: number;
  /** The id of the latest result drawn under it, or null where it draws none. */
  readonly resultId: string | null;
  /** The `data-glyph` token of the shape its state is drawn with, or null where it draws none. */
  readonly shape: string | null;
  /** `data-node-state`, the model's own token for how the check stands. */
  readonly state: string | null;
  /** The word the page draws for its state. */
  readonly word: string | null;
  readonly words: string;
}

/** One statement the Verification filter's body draws, with the checks that serve it. */
export interface DrawnStatement {
  readonly checks: readonly DrawnCheck[];
  readonly context: boolean;
  readonly gapKinds: readonly string[];
  readonly id: string;
  /** `data-statement-state`: met, not_met or unknown (R17). */
  readonly state: string | null;
  /** The word the page draws for that state. */
  readonly stateWord: string | null;
  readonly words: string;
}

/** Every statement the Verification filter's body draws, in document order, with its checks
 *  and their latest results (c-checks-readable, c-presets). */
export function verificationCards(page: Page): Promise<DrawnStatement[]> {
  return page.evaluate(
    ({ card, issue }) => {
      const text = (element: Element | null): string =>
        (element?.textContent ?? "").replace(/\s+/g, " ").trim();
      return [...document.querySelectorAll(`${issue} ${card}`)].map((drawn) => ({
        checks: [...drawn.querySelectorAll("[data-verification-check]")].map((check) => ({
          earlier: text(check.querySelector("[data-verification-earlier]")) || null,
          id: check.getAttribute("data-node-id") ?? "",
          resultId:
            check.querySelector("[data-verification-result]")?.getAttribute("data-node-id") ?? null,
          results: check.querySelectorAll("[data-verification-result]").length,
          shape:
            check
              .querySelector("span[data-node-state]")
              ?.parentElement?.querySelector("svg[data-glyph]")
              ?.getAttribute("data-glyph") ?? null,
          state: check.getAttribute("data-node-state"),
          word: text(check.querySelector("span[data-node-state]")) || null,
          words: text(check.querySelector("[data-node-words]")),
        })),
        context: drawn.hasAttribute("data-node-context"),
        gapKinds: [...drawn.querySelectorAll("[data-gap-kind]")].map(
          (gap) => gap.getAttribute("data-gap-kind") ?? ""
        ),
        id: drawn.getAttribute("data-node-id") ?? "",
        state: drawn.getAttribute("data-statement-state"),
        stateWord: text(drawn.querySelector("span[data-statement-state]")) || null,
        words: text(drawn.querySelector("[data-node-words]")),
      }));
    },
    { card: VERIFICATION_CARD, issue: SELECTORS.issuePage }
  );
}

/** The one commit the page names for the issue's results, or null where it names none or more
 *  than one. */
export function namedCommit(page: Page): Promise<string | null> {
  return page.evaluate((issue) => {
    const drawn = document.querySelectorAll(`${issue} [data-verification-commit]`);
    if (drawn.length !== 1) return null;
    return (drawn[0]?.querySelector("code")?.textContent ?? "").trim();
  }, SELECTORS.issuePage);
}
