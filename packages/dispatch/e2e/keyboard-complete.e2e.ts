/** The browser checks of dispatch://INFRA-2160, every page of Dispatch by the keyboard: one row
 *  or more for each check of its Verification document, each row's title starting with the id
 *  of the check it proves, so `-g '<id>'` runs that check's rows. Every step of a check is an
 *  assertion whose message names the check and the step, so a failure names the step it stopped
 *  at - which is also how the negative control reads: the same rows run on a build of the merge
 *  base, where they fail at the first step the merge base does not do.
 *
 *  One row proves no check of the document: its title starts with `held-press`, the name of the
 *  file whose rule it holds in the browser (`web/src/components/held-press.ts`: a control whose
 *  write is out is held, not disabled, so it keeps the keyboard's focus), in place of a check's
 *  id.
 *
 *  The rows at the end of the file whose titles start `inbox reading view:` prove no check of the
 *  document either: they are the tester's red rows of round 22, each of which fails at the head
 *  e4ece294 by the focus step its message names (the architect's ruling of 2026-11-26T22:59Z).
 *
 *  The rows write - snoozes, priorities, statuses, assignees, repositories, hand-ons - and count
 *  each write at the server's own route, so they run on the real stack. Their own imports from
 *  `web/src` are of modules this tree does not change (`components/control-names.ts`): what they
 *  need from the app they read off the page or work out in `e2e/keyboard-complete.ts`. The e2e
 *  helpers they share with main's rows reach four modules this tree does change: `api/client.ts`
 *  (through `e2e/editor.ts`), `theme/classes.ts` (through `e2e/inbox-pane-scroll.ts`), and
 *  `features/issue-page/page-words.ts` with the `features/shell/key-text.ts` it reads (through
 *  `e2e/issue-page.ts`, which k-list-focus's Details row opens the page with). */
import { CONTROL } from "../web/src/components/control-names";
import { REF_PREVIEW_CARD_ID } from "../web/src/features/refs/ref-preview-card";
import { openAgents, setLiveSessions, shownAgentRows } from "./agents";
import {
  answerAsk,
  baseUrl,
  createArtifactAsk,
  createAsk,
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  createProjectDocument,
  editArtifact,
  editAsk,
  getIssue,
  getIssueArtifactText,
  markThreadRead,
  patchIssue,
  putArchitectureSource,
  putProjectDefaultRepository,
  requestApproval,
  reviewArtifact,
  sessionCookie,
  snoozeAsk,
  syncArchitectureSource,
  withOpenedAsk,
} from "./api";
import { recordClipboard } from "./clipboard";
import { seedDeliveryFixture, TIMELINE_FIXTURE_QUERY } from "./delivery-seed";
import {
  barAction,
  documentEditor,
  indexedBlockAsk,
  openSpec,
  placeCaret,
  selectEditorText,
} from "./editor";
import { expectTimeout } from "./expect-timeout";
import { seedFakeGithub } from "./fake-github-helpers";
import { control, controlRow, displayChoice, listOf } from "./filters";
import { nextFrames, settledScrollTop } from "./frames";
import { drawer, peekTrigger } from "./inbox-drawer";
import {
  backToList,
  inboxList,
  inboxPane,
  inboxRow,
  paneScroller,
  readingView,
  selectedRow,
  selectRow,
} from "./inbox-pane";
import {
  DOCUMENT_FOOT,
  headingOffset,
  holdReads,
  inboxAt,
  restingColumn,
  SECTION_HEADING,
  seedApprovalWithMarks,
  seedQuestionColumn,
  waitOutPlacement,
} from "./inbox-pane-scroll";
import {
  agentRowId,
  asSession,
  replyRowId,
  seedAgentThread,
  seedCommentThread,
  THREAD_SESSION,
} from "./inbox-threads";
import { gotoIssue, marginShell } from "./issue-page";
import { withIssuePage } from "./issue-page-option";
import {
  ARCHITECTURE_LEAF,
  answerWrites,
  asker,
  contrastOf,
  dismissThread,
  dismissWrites,
  expectKeyHint,
  type HeldWrites,
  handOnControl,
  handOnWrites,
  holdWrites,
  issueWrites,
  MACHINES,
  markDocument,
  newPageAt,
  OTHER_SITE,
  reloaded,
  requestsSent,
  returnMornings,
  reviewWrites,
  type SeededApproval,
  type SpreadAsk,
  seedApprovalAtVersionTwo,
  seedArchitectureLeaf,
  seedAsksOverTwoProjects,
  seedPriorityWithWork,
  seedSecondRepositoryPull,
  seedTallAsks,
  seedTwoMachines,
  snoozedUntil,
  snoozeWrites,
  unsnoozeWrites,
  versionReviewWrites,
} from "./keyboard-complete";
import { onBody, openIssue } from "./keyboard-issue";
import { firstAttempts, type KeyedWrite, watchWrites, writesSent } from "./keyed-writes";
import { machineSession } from "./machines";
import { commentWithBody } from "./margin-helpers";
import { expectHighlight, openList, openTrigger, priorityTrigger, statusTrigger } from "./menus";
import { inboxRead, logReads, readAnsweredSince } from "./pick-writes";
import { refPicker } from "./ref-picker";
import {
  listedRequests,
  type ReviewedRoute,
  type ReviewSeed,
  reviewRoutes,
  waitForRouteSettled,
} from "./review-routes";
import { seedReviewRoutes } from "./review-seed";
import { sessionIds } from "./session-ids";
import { settledState } from "./settled";
import { openIssuesTab } from "./table-controls";
import { type BrowserContext, expect, type Locator, type Page, type Request, test } from "./test";
import { asUser } from "./users";

// ---------------------------------------------------------------------------
// Where the Inbox draws a question and its controls.
// ---------------------------------------------------------------------------

/** The four places the Inbox draws a question with its Snooze and its priority. Beside the
 *  reading column. Below the column's width, where the page lists short rows and reads the row the
 *  reader opens in a reading view in the list's place (cybermonklabs/legion#5223): from the
 *  short-row list, whose keys open the focused row's reading view and act there, and inside that
 *  view. And in the drawer over another page, whose rows are cards at every width. */
type InboxPlace = "the reading pane" | "the short-row list" | "the reading view" | "the drawer";

/** A width the Inbox draws its reading column at (it does from 1280 px). */
const WIDE = { height: 900, width: 1440 };

/** A width below the reading column's, where the Inbox lists short rows and reads the row the
 *  reader opens in its reading view: 1024x768, a laptop's window, where the Verification reads
 *  it. */
const NARROW = { height: 768, width: 1024 };

/** Whether `place` is one of the two below the reading column's width. */
function belowColumn(place: InboxPlace): boolean {
  return place === "the short-row list" || place === "the reading view";
}

/** `place` as a row's title says where it runs. */
function placeWords(place: InboxPlace): string {
  switch (place) {
    case "the reading pane":
      return "beside the reading pane";
    case "the short-row list":
      return "in the short-row list at 1024x768";
    case "the reading view":
      return "in the reading view at 1024x768";
    case "the drawer":
      return "in the drawer";
  }
}

/** The Inbox as `openInboxAt` opened it, and what the rows reach for in it. */
interface InboxAt {
  readonly place: InboxPlace;
  /** What the Inbox is drawn in: the page, or the drawer over it. */
  readonly scope: Locator;
  /** A place to press that does nothing of its own: the Inbox's heading, which the page draws
   *  over the list and over a reading view alike. */
  readonly outside: Locator;
  /** The questions' ids, in the order the list draws them as it opens. */
  readonly order: readonly string[];
}

/** Opens the Inbox drawer with `i` over CORE's project page, from the page itself, and answers
 *  the drawer once it shows; `step` names that check where a row names its own. */
async function openDrawer(page: Page, step?: string): Promise<Locator> {
  await page.goto("/projects/CORE");
  await expect(page.getByRole("button", { exact: true, name: "Board" })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("i");
  await expect(drawer(page), step).toBeVisible();
  return drawer(page);
}

/** Opens the Inbox where `place` draws it, holding `rows` questions. The keymap binds only once
 *  sign-in resolves, so the page is read drawn before a key is pressed on it. */
async function openInboxAt(page: Page, place: InboxPlace, rows: number): Promise<InboxAt> {
  await page.setViewportSize(belowColumn(place) ? NARROW : WIDE);
  let scope: Locator;
  let outside: Locator;
  if (place === "the drawer") {
    scope = await openDrawer(page);
    outside = scope.getByRole("heading", { exact: true, name: "Inbox" });
  } else {
    await page.goto("/");
    scope = page.locator("body");
    outside = page.getByRole("heading", { exact: true, level: 1, name: "Inbox" });
  }
  const list = scope.locator("[data-inbox-row]");
  await expect(list).toHaveCount(rows);
  const order = await list.evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
  );
  return { order, outside, place, scope };
}

/** The row of the question `id`. */
function rowOf(inbox: InboxAt, id: string): Locator {
  return inbox.scope.locator(`[data-inbox-row="${id}"]`);
}

/** Below the column's width, brings the short-row list back where a row's reading view stands
 *  in its place (`backToList`); nothing where the list shows, and nothing in any other place. */
async function showList(inbox: InboxAt): Promise<void> {
  if (!belowColumn(inbox.place)) return;
  const page = inbox.scope.page();
  const state = await settledState("the Inbox below the column's width", [
    ["open", readingView(page)],
    ["list", inboxList(page)],
  ]);
  if (state === "open") await backToList(page);
}

/** Puts the reader on the question `id`, where the Inbox's keys act on the question in hand.
 *  Beside the reading column a click on the row's age selects it, which the column follows, and
 *  the focus is then on the row; in the short-row list and the drawer the focus is on the row,
 *  the list brought back first where a reading view stands in its place. Inside the reading view
 *  the row's view is opened as a click opens it (`selectRow`), and the focus is on the view's
 *  scroller, the stop the click and `l` put it on. */
async function goTo(inbox: InboxAt, id: string): Promise<void> {
  const page = inbox.scope.page();
  if (inbox.place === "the reading view") {
    await selectRow(page, id);
    await paneScroller(page).focus();
    return;
  }
  await showList(inbox);
  if (inbox.place === "the reading pane") await selectRow(page, id);
  await rowOf(inbox, id).focus();
}

/** The box holding the controls of the question `id`, which the reader is on: the head of the
 *  reading column, and below the column's width the head of the row's reading view, since a short
 *  row draws none of its own; in the drawer the question's own card. */
function controlsOf(page: Page, inbox: InboxAt, id: string): Locator {
  return inbox.place === "the drawer" ? rowOf(inbox, id) : paneHead(page);
}

/** Holds the reader on the question `id` once a menu a key opened on it has closed, for the step
 *  `step`: the focus is back where the key was pressed. Beside the reading column and in the drawer
 *  that is the question's row, the row still the one the column shows beside it; inside the
 *  reading view it is the view's scroller, the view still on `id`. From the short-row list the key
 *  opened the row's reading view and pressed its head's `trigger` there (`useReadingViewLanding`),
 *  so the close gives the focus back to that trigger, and Escape on it is one level out: the list
 *  again, with the focus on the row, where the row's next key is pressed. */
async function expectStillOn(
  inbox: InboxAt,
  id: string,
  trigger: Locator,
  step: string
): Promise<void> {
  const row = rowOf(inbox, id);
  if (!belowColumn(inbox.place)) {
    await expect(row, `${step}: the focus is back on the ask's row`).toBeFocused();
    if (inbox.place === "the reading pane") {
      await expect(row, `${step}: the reading pane still shows the ask`).toHaveAttribute(
        "data-inbox-selected",
        "true"
      );
    }
    return;
  }
  const page = inbox.scope.page();
  await expect(readingView(page), `${step}: the reading view still shows the ask`).toHaveAttribute(
    "data-inbox-reading-row",
    id
  );
  if (inbox.place === "the reading view") {
    await expect(
      paneScroller(page),
      `${step}: the focus is back on the view's scroller, where the key was pressed`
    ).toBeFocused();
    return;
  }
  await expect(
    trigger,
    `${step}: the focus is back on the control of the view's head the key pressed`
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(row, `${step}: Escape there puts the focus back on the ask's row`).toBeFocused();
}

/** The toggle of the Inbox's Later band while it holds `count` questions. */
function laterBand(inbox: InboxAt, count: number): Locator {
  return inbox.scope.getByRole("button", { name: new RegExp(`^Later \\(${count}\\)`) });
}

/** How many writes the lists of writes hold together, each list counted by `writesSent`. */
function countOf(writes: readonly (readonly KeyedWrite<unknown>[])[]): number {
  return writes.reduce((sum, list) => sum + writesSent(list), 0);
}

// ---------------------------------------------------------------------------
// The menus' choices. Snooze, the priority and the status are each the shared single pick
// (`MultiSelect`) with D5's rules: a text box (`combobox`) over a `listbox` of `option`s, each
// named by its words, its digit drawn in a `kbd` hidden from assistive tech and named in its
// `aria-keyshortcuts`, and the highlighted one named by the box's `aria-activedescendant`
// (`openList`, `expectHighlight` in `e2e/menus.ts`).
// ---------------------------------------------------------------------------

/** The Snooze menu's choices, in order, each with the digit that writes it. */
const SNOOZE_CHOICES = [
  ["Later today", "1"],
  ["Tomorrow", "2"],
  ["Next week", "3"],
  ["Until I clear it", "4"],
] as const;

/** The choice a snoozed question's Snooze menu adds after them. */
const UNSNOOZE_CHOICE = ["Un-snooze", "0"] as const;

/** The priority menu's choices, in order: Unset, which no digit writes, and P0 to P3 on 0 to 3. */
const PRIORITY_CHOICES = ["Unset", "P0", "P1", "P2", "P3"] as const;

/** The status menu's choices on an open issue, in order: every status but Done. */
const OPEN_STATUSES = [
  "Triage",
  "Icebox",
  "Backlog",
  "Todo",
  "In progress",
  "Testing",
  "Needs review",
  "Retro",
] as const;

/** Holds the option `choice` of an open menu to the digit that writes it, or to none: the digit
 *  drawn in its `kbd` and named in its `aria-keyshortcuts`, for the step `step`. */
async function expectDigit(
  choice: Locator,
  label: string,
  digit: string | undefined,
  step: string
): Promise<void> {
  if (digit === undefined) {
    await expect(choice, `${step}: no digit writes ${label}`).not.toHaveAttribute(
      "aria-keyshortcuts"
    );
    await expect(choice.locator("kbd"), `${step}: ${label} shows no number`).toHaveCount(0);
    return;
  }
  await expect(choice, `${step}: ${label} is pressed with ${digit}`).toHaveAttribute(
    "aria-keyshortcuts",
    digit
  );
  await expect(choice.locator("kbd"), `${step}: ${label} shows its number`).toHaveText(digit);
}

/** Holds the open Snooze menu `menu` to its choices, each an `option` with its number, for the
 *  step `step`. */
async function expectSnoozeChoices(menu: Locator, step: string, snoozed = false): Promise<void> {
  const expected = snoozed ? [...SNOOZE_CHOICES, UNSNOOZE_CHOICE] : SNOOZE_CHOICES;
  const choices = menu.getByRole("option");
  await expect(choices, `${step}: the menu holds ${expected.length} choices`).toHaveCount(
    expected.length
  );
  for (const [index, [label, digit]] of expected.entries()) {
    const choice = choices.nth(index);
    await expect(choice, `${step}: choice ${index + 1} is ${label}`).toHaveAccessibleName(label);
    await expectDigit(choice, label, digit, step);
  }
}

/** Holds the open priority menu `menu` to Unset and P0 to P3, each an `option` with its number,
 *  `current` the one marked as the issue's value (`aria-selected`), for the step `step`. */
async function expectPriorityChoices(menu: Locator, current: string, step: string): Promise<void> {
  const choices = menu.getByRole("option");
  await expect(choices, `${step}: the menu holds Unset and P0 to P3`).toHaveCount(
    PRIORITY_CHOICES.length
  );
  for (const [index, label] of PRIORITY_CHOICES.entries()) {
    const choice = choices.nth(index);
    await expect(choice, `${step}: choice ${index + 1} is ${label}`).toHaveAccessibleName(label);
    await expect(
      choice,
      `${step}: ${label} is ${label === current ? "" : "not "}the current value`
    ).toHaveAttribute("aria-selected", `${label === current}`);
    await expectDigit(choice, label, label === "Unset" ? undefined : label.slice(1), step);
  }
}

/** Holds the open menu `menu` to the focus in its text box, for the step `step`: where a key or
 *  a press on a fine pointer opens it. */
async function expectFocusInBox(menu: Locator, step: string): Promise<void> {
  await expect(
    menu.getByRole("combobox"),
    `${step}: the focus is in the menu's text box`
  ).toBeFocused();
}

// ---------------------------------------------------------------------------
// k-snooze-menu
// ---------------------------------------------------------------------------

/** What a pointer's press focuses by the browser's own default: the nearest element at or above
 *  the press that takes the focus, a control or an element with a `tabindex`. */
const PRESS_FOCUSABLE =
  'a[href], button, input, select, textarea, summary, [tabindex], [contenteditable="true"]';

/** Every step of k-snooze-menu where `place` draws the Inbox, over four asks of their own. */
async function snoozeMenuSteps(page: Page, place: InboxPlace): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Ledger", "Mirror", "Lease", "Replay"]) {
    const issue = await createIssue({ project: "CORE", title: `${title} rollout` });
    await createAsk(issue.key, { question: `Ship the ${title.toLowerCase()} rollout?` }, asker);
  }
  const inbox = await openInboxAt(page, place, 4);
  const [first, second, third] = inbox.order;
  if (first === undefined || second === undefined || third === undefined) {
    throw new Error("the Inbox lists fewer than the four asks seeded");
  }
  const snoozes = new Map(inbox.order.map((id) => [id, snoozeWrites(page, id)] as const));
  const snoozesOf = (id: string) => snoozes.get(id) ?? [];
  const unsnoozes = unsnoozeWrites(page, first);
  const watched = [...snoozes.values(), unsnoozes];
  const sent = () => countOf(watched);
  const requests = () => requestsSent(watched);
  const step = (text: string) => `k-snooze-menu: ${place}: ${text}`;
  const menu = openList(page);
  const headSnooze = paneHead(page).getByRole("button", { name: /^Snooze / });
  const leftOpen = async (what: string) => {
    if (place === "the drawer") {
      await expect(inbox.scope, step(`${what} leaves the drawer open`)).toBeVisible();
    }
  };

  // b on the first ask, then the arrows, Home, End and Escape: nothing is written.
  await goTo(inbox, first);
  const band = (await rowOf(inbox, first).getAttribute("data-inbox-section")) ?? "";
  await page.keyboard.press("b");
  await expect(menu, step("b shows a menu")).toBeVisible();
  await expectSnoozeChoices(menu, step("b shows a menu"));
  await expectFocusInBox(menu, step("b"));
  await expectHighlight(menu, "Later today", step("b highlights the first choice"));
  const beforeKeys = requests();
  await page.keyboard.press("ArrowDown");
  await expectHighlight(menu, "Tomorrow", step("ArrowDown moves the highlight to Tomorrow"));
  await page.keyboard.press("End");
  await expectHighlight(menu, "Until I clear it", step("End moves the highlight to the last"));
  await page.keyboard.press("Home");
  await expectHighlight(menu, "Later today", step("Home moves the highlight to the first"));
  expect(requests(), step("ArrowDown, End and Home send no request")).toBe(beforeKeys);
  await expectFocusInBox(menu, step("after ArrowDown, End and Home"));
  const beforeEscape = requests();
  await page.keyboard.press("Escape");
  await expect(menu, step("Escape closes the menu")).toHaveCount(0);
  expect(requests(), step("Escape sends no request")).toBe(beforeEscape);
  await expect(rowOf(inbox, first), step("Escape leaves the ask in its band")).toHaveAttribute(
    "data-inbox-section",
    band
  );
  await expectStillOn(inbox, first, headSnooze, step("Escape"));
  await leftOpen("Escape");

  // h opens the same menu, and Enter writes the highlighted Later today.
  await page.keyboard.press("h");
  await expect(menu, step("h opens the same menu")).toBeVisible();
  await expectSnoozeChoices(menu, step("h opens the same menu"));
  await expectFocusInBox(menu, step("h"));
  await expectHighlight(menu, "Later today", step("h highlights the first choice"));
  const laterFrom = Date.now();
  await page.keyboard.press("Enter");
  await expect
    .poll(() => writesSent(snoozesOf(first)), {
      message: step("Enter writes Later today: one request"),
    })
    .toBe(1);
  expect(sent(), step("Enter sends one request")).toBe(1);
  const laterToday = snoozedUntil(firstAttempts(snoozesOf(first))[0]);
  expect(laterToday, step("Enter writes Later today: a moment ahead")).toBeGreaterThan(laterFrom);
  expect(laterToday, step("Enter writes Later today: three hours on at most")).toBeLessThanOrEqual(
    Date.now() + 3 * 60 * 60 * 1000
  );
  // Below the column's width the snooze opens the next ask in the reading view, which hides the
  // list with its bands, so the band is read on the list.
  await showList(inbox);
  await expect(laterBand(inbox, 1), step("Enter puts the ask into Later")).toBeVisible();

  // b on the second ask, a narrowing to Next week, then 2: one request, for Tomorrow, which the
  // narrowing hides.
  await goTo(inbox, second);
  await page.keyboard.press("b");
  await expect(menu, step("b opens the second ask's menu")).toBeVisible();
  const beforeLetters = requests();
  await page.keyboard.type("next");
  await expect(menu.getByRole("option"), step("next narrows the menu to Next week")).toHaveText([
    /^Next week/,
  ]);
  expect(requests(), step("the letters send no request")).toBe(beforeLetters);
  const mornings = await returnMornings(page);
  await page.keyboard.press("2");
  await expect
    .poll(() => writesSent(snoozesOf(second)), { message: step("2 sends one request") })
    .toBe(1);
  expect(sent(), step("2 sends one request")).toBe(2);
  expect(
    snoozedUntil(firstAttempts(snoozesOf(second))[0]),
    step("2 snoozes the ask until Tomorrow, which the narrowing hides")
  ).toBe(mornings.tomorrow);

  // The third ask's menu from a click on its Snooze control: Tab closes it and gives the focus
  // back to that control, and a click outside closes it and gives the focus back there only where
  // the click lands on nothing that takes the focus; one on an element that does leaves it there.
  await goTo(inbox, third);
  // A short row draws no Snooze: below the column's width the click is on the head of the row's
  // reading view, which a click on the row opens.
  if (belowColumn(place)) await selectRow(page, third);
  const snooze = controlsOf(page, inbox, third).getByRole("button", { name: /^Snooze / });
  await snooze.click();
  await expect(menu, step("a click on the Snooze control opens its menu")).toBeVisible();
  await expectFocusInBox(menu, step("a click on the Snooze control"));
  const beforeTab = requests();
  await page.keyboard.press("Tab");
  await expect(menu, step("Tab closes the menu")).toHaveCount(0);
  expect(requests(), step("Tab sends no request")).toBe(beforeTab);
  await expect(snooze, step("Tab leaves the focus on the Snooze control")).toBeFocused();
  await leftOpen("Tab");
  await snooze.click();
  await expect(menu, step("a click on the Snooze control opens its menu again")).toBeVisible();
  const beforeOutside = requests();
  await inbox.outside.click();
  await expect(menu, step("a click outside the menu closes it")).toHaveCount(0);
  expect(requests(), step("a click outside the menu sends no request")).toBe(beforeOutside);
  // A press outside on an element that takes the focus gives it that element by the press's own
  // default, as the Inbox page's `main` (`tabindex="-1"`) takes it from a press on its heading;
  // a press on nothing that takes it, as on the drawer's heading, gives the focus back to the
  // Snooze control (D5).
  const pressFocuses = await inbox.outside.evaluate(
    (node, selector) => node.closest(selector) !== null,
    PRESS_FOCUSABLE
  );
  if (pressFocuses) {
    await expect
      .poll(
        () =>
          inbox.outside.evaluate(
            (node, selector) => node.closest(selector) === document.activeElement,
            PRESS_FOCUSABLE
          ),
        { message: step("a click outside the menu leaves the focus where the click put it") }
      )
      .toBe(true);
  } else {
    await expect(
      snooze,
      step("a click outside the menu, on nothing that takes the focus, gives it back to Snooze")
    ).toBeFocused();
  }
  await leftOpen("a click outside the menu");

  // The second ask, out in Later, and the third, selected with x: the bar's Snooze opens the
  // same menu, and b then 3 snoozes both until Next week.
  await showList(inbox);
  await laterBand(inbox, 2).click();
  await expect(rowOf(inbox, second), step("the Later band opens on the second ask")).toBeVisible();
  for (const id of [second, third]) {
    await goTo(inbox, id);
    await page.keyboard.press("x");
  }
  const bar = inbox.scope.getByRole("toolbar", { name: "Selected rows" });
  await expect(bar, step("x selects the second and the third ask")).toContainText("2 selected");
  const barSnooze = bar.getByRole("button", { exact: true, name: "Snooze selected asks" });
  await barSnooze.click();
  await expect(
    openTrigger(page),
    step("a click on the bar's Snooze opens the same menu")
  ).toHaveAccessibleName("Snooze selected asks");
  await expectSnoozeChoices(menu, step("a click on the bar's Snooze opens the same menu"));
  const beforeBarEscape = requests();
  await page.keyboard.press("Escape");
  await expect(menu, step("Escape closes the bar's menu")).toHaveCount(0);
  expect(requests(), step("Escape on the bar's menu sends no request")).toBe(beforeBarEscape);
  await expect(barSnooze, step("Escape gives the focus back to the bar's Snooze")).toBeFocused();
  // The third ask in hand: its row, and inside the reading view, which still shows the third ask
  // x marked, the view's scroller.
  await (place === "the reading view" ? paneScroller(page) : rowOf(inbox, third)).focus();
  await page.keyboard.press("b");
  await expect(
    openTrigger(page),
    step("b with asks selected opens the bar's menu")
  ).toHaveAccessibleName("Snooze selected asks");
  const week = await returnMornings(page);
  await page.keyboard.press("3");
  await expect
    .poll(() => [writesSent(snoozesOf(second)), writesSent(snoozesOf(third))], {
      message: step("b then 3 sends one snooze for each selected ask"),
    })
    .toEqual([2, 1]);
  expect(sent(), step("b then 3 sends two requests")).toBe(4);
  for (const [id, index] of [
    [second, 1],
    [third, 0],
  ] as const) {
    expect(
      snoozedUntil(firstAttempts(snoozesOf(id))[index]),
      step("b then 3 snoozes each selected ask until Next week")
    ).toBe(week.nextWeek);
  }
  await expect(bar, step("the bulk snooze lands and clears the selection")).toHaveCount(0);

  // The first ask, snoozed: its menu also holds Un-snooze, which 0 writes.
  await goTo(inbox, first);
  await page.keyboard.press("b");
  await expect(menu, step("b opens a snoozed ask's menu")).toBeVisible();
  await expectSnoozeChoices(menu, step("a snoozed ask's menu also holds Un-snooze"), true);
  await expectHighlight(menu, "Later today", step("b highlights a snoozed ask's first choice"));
  await page.keyboard.press("0");
  await expect
    .poll(() => writesSent(unsnoozes), { message: step("0 un-snoozes the ask: one request") })
    .toBe(1);
  expect(sent(), step("0 sends one request, the un-snooze")).toBe(5);
}

// SLOW(>=2s): four asks seeded and snoozed one by one, with the band and the bar read each time.
test("k-snooze-menu: b and h open the Snooze menu beside the reading pane, where Enter or a digit writes and an arrow, Escape, Tab or a click outside writes nothing", async ({
  page,
}) => {
  await snoozeMenuSteps(page, "the reading pane");
});

// SLOW(>=2s): four asks seeded and snoozed one by one, with the band and the bar read each time.
test("k-snooze-menu: b and h open the Snooze menu from the short-row list at 1024x768, where Enter or a digit writes and an arrow, Escape, Tab or a click outside writes nothing", async ({
  page,
}) => {
  await snoozeMenuSteps(page, "the short-row list");
});

// SLOW(>=2s): four asks seeded and snoozed one by one, with the band and the bar read each time.
test("k-snooze-menu: b and h open the Snooze menu in the reading view at 1024x768, where Enter or a digit writes and an arrow, Escape, Tab or a click outside writes nothing", async ({
  page,
}) => {
  await snoozeMenuSteps(page, "the reading view");
});

// SLOW(>=2s): four asks seeded and snoozed one by one, with the band and the bar read each time.
test("k-snooze-menu: b and h open the Snooze menu in the Inbox drawer, where Enter or a digit writes and an arrow, Escape, Tab or a click outside writes nothing", async ({
  page,
}) => {
  await snoozeMenuSteps(page, "the drawer");
});

// ---------------------------------------------------------------------------
// k-priority-menu
// ---------------------------------------------------------------------------

/** An ask on an issue at P2, an ask on a project document, which has no issue, and a reply
 *  thread on an issue, all waiting on alice. */
async function seedPriorityInbox(): Promise<{
  askId: string;
  documentAskId: string;
  issueKey: string;
  threadRowId: string;
}> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await patchIssue(issue.key, { priority: 2 });
  const ask = await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);
  const runbook = await createProjectDocument(
    "CORE",
    { content: "# Runbook\n\nRestart, reseed, reopen.\n", name: "Runbook" },
    asker
  );
  const documentAsk = await createArtifactAsk(
    runbook.artifact.id,
    { question: "Keep the runbook in the repository?" },
    asker
  );
  const threadIssue = await createIssue({ project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(
    threadIssue.key,
    "How far back does a replay go?",
    "One hour, then it gives up."
  );
  return {
    askId: ask.id,
    documentAskId: documentAsk.id,
    issueKey: issue.key,
    threadRowId: replyRowId(thread),
  };
}

/** The priority mark of the question `id`, about the issue `issueKey`: its short row's own tag
 *  beside the reading column and in the short-row list, the face of the priority control of the
 *  reading view's head, which the reader sees in the list's place, and in the drawer the face of
 *  the card's priority control. */
function priorityMarkOf(inbox: InboxAt, id: string, issueKey: string): Locator {
  const row = rowOf(inbox, id);
  if (inbox.place === "the drawer") return priorityTrigger(row, issueKey);
  if (inbox.place === "the reading view") {
    return priorityTrigger(paneHead(inbox.scope.page()), issueKey);
  }
  return row.locator("[data-inbox-priority]");
}

/** Every Inbox step of k-priority-menu where `place` draws it. */
async function priorityMenuSteps(page: Page, place: InboxPlace): Promise<void> {
  const seed = await seedPriorityInbox();
  const inbox = await openInboxAt(page, place, 3);
  const writes = issueWrites(page, seed.issueKey);
  const requests = () => requestsSent([writes]);
  const step = (text: string) => `k-priority-menu: ${place}: ${text}`;
  const menu = openList(page);

  // p on the ask, then ArrowUp, Home, End and Escape: nothing is written.
  await goTo(inbox, seed.askId);
  const mark = priorityMarkOf(inbox, seed.askId, seed.issueKey);
  const control = priorityTrigger(controlsOf(page, inbox, seed.askId), seed.issueKey);
  await page.keyboard.press("p");
  await expect(menu, step("p shows a menu")).toBeVisible();
  await expectPriorityChoices(menu, "P2", step("p shows a menu"));
  await expectFocusInBox(menu, step("p"));
  await expectHighlight(menu, "P2", step("p highlights the current P2"));
  const beforeKeys = requests();
  await page.keyboard.press("ArrowUp");
  await expectHighlight(menu, "P1", step("ArrowUp moves the highlight to P1"));
  await page.keyboard.press("Home");
  await expectHighlight(menu, "Unset", step("Home moves the highlight to Unset"));
  await page.keyboard.press("End");
  await expectHighlight(menu, "P3", step("End moves the highlight to P3"));
  expect(requests(), step("ArrowUp, Home and End send no request")).toBe(beforeKeys);
  const beforeEscape = requests();
  await page.keyboard.press("Escape");
  await expect(menu, step("Escape closes the menu")).toHaveCount(0);
  expect(requests(), step("Escape sends no request")).toBe(beforeEscape);
  // The focus is read first: from the short-row list it takes the reader back to the list, where
  // the row's mark is drawn.
  await expectStillOn(inbox, seed.askId, control, step("after Escape"));
  await expect(mark, step("after Escape the row's mark still reads P2")).toHaveText("P2");

  // p, then Tab: nothing is written, and the focus goes back to where p was pressed.
  await page.keyboard.press("p");
  await expect(menu, step("p opens the menu again")).toBeVisible();
  const beforeTab = requests();
  await page.keyboard.press("Tab");
  await expect(menu, step("Tab closes the menu")).toHaveCount(0);
  expect(requests(), step("Tab sends no request")).toBe(beforeTab);
  await expectStillOn(inbox, seed.askId, control, step("after Tab"));

  // p then 0 writes P0; p, ArrowDown, Enter writes the highlighted P1.
  await page.keyboard.press("p");
  await expect(menu, step("p opens the menu a third time")).toBeVisible();
  await page.keyboard.press("0");
  await expect
    .poll(() => writesSent(writes), { message: step("p then 0 sends one write") })
    .toBe(1);
  expect(firstAttempts(writes)[0]?.input, step("p then 0 writes P0")).toEqual({ priority: 0 });
  await expectStillOn(inbox, seed.askId, control, step("after p then 0"));
  await expect(mark, step("after p then 0 the row's mark reads P0")).toHaveText("P0");
  await page.keyboard.press("p");
  await expectHighlight(menu, "P0", step("p opens on the current P0"));
  await page.keyboard.press("ArrowDown");
  await expectHighlight(menu, "P1", step("ArrowDown moves the highlight to P1"));
  await page.keyboard.press("Enter");
  await expect
    .poll(() => writesSent(writes), { message: step("p, ArrowDown, Enter sends one write") })
    .toBe(2);
  expect(firstAttempts(writes)[1]?.input, step("Enter writes the highlighted P1")).toEqual({
    priority: 1,
  });
  await expectStillOn(inbox, seed.askId, control, step("after p, ArrowDown, Enter"));
  await expect(mark, step("after Enter the row's mark reads P1")).toHaveText("P1");

  // A click on the priority control opens the same menu. From here on nothing writes the
  // priority: not the click, nor its Escape, nor p and ? on the document's ask and the thread.
  const beforeRest = requests();
  // A short row draws no priority control: below the column's width the click is on the head of
  // the row's reading view, which a click on the row opens.
  if (belowColumn(place)) await selectRow(page, seed.askId);
  await control.click();
  await expect(menu, step("a click on the priority control opens the same menu")).toBeVisible();
  await expectPriorityChoices(
    menu,
    "P1",
    step("a click on the priority control opens the same menu")
  );
  await expectFocusInBox(menu, step("a click on the priority control"));
  await page.keyboard.press("Escape");
  await expect(menu, step("Escape closes the clicked menu")).toHaveCount(0);
  await expect(control, step("Escape gives the focus back to the clicked control")).toBeFocused();

  // On the document's ask and on the thread, p opens nothing, and `?` shows the key unavailable.
  for (const [id, what] of [
    [seed.documentAskId, "the document's ask"],
    [seed.threadRowId, "the thread"],
  ] as const) {
    await goTo(inbox, id);
    await page.keyboard.press("p");
    await expect(menu, step(`on ${what} p opens nothing`)).toHaveCount(0);
    await page.keyboard.press("?");
    await expect(
      helpLine(page, "Inbox", "Open the priority menu"),
      step(`on ${what} the ? help shows the priority key not available`)
    ).toHaveAttribute("data-enabled", "false");
    await page.keyboard.press("Escape");
    await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toHaveCount(0);
  }
  expect(requests(), step("nothing else writes the priority")).toBe(beforeRest);
}

// SLOW(>=2s): three kinds of item seeded, two writes read back, and the help opened twice.
test("k-priority-menu: p opens the priority menu beside the reading pane, and on no item without an issue", async ({
  page,
}) => {
  await priorityMenuSteps(page, "the reading pane");
});

// SLOW(>=2s): three kinds of item seeded, two writes read back, and the help opened twice.
test("k-priority-menu: p opens the priority menu from the short-row list at 1024x768, and on no item without an issue", async ({
  page,
}) => {
  await priorityMenuSteps(page, "the short-row list");
});

// SLOW(>=2s): three kinds of item seeded, two writes read back, and the help opened twice.
test("k-priority-menu: p opens the priority menu in the reading view at 1024x768, and on no item without an issue", async ({
  page,
}) => {
  await priorityMenuSteps(page, "the reading view");
});

// SLOW(>=2s): the issue page, the Board and the List, each loaded and its menu opened.
test("k-priority-menu: p opens the priority menu on the issue page and on a Board card, and a click opens it on the List", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger rollout";
  const issue = await createIssue({ project: "CORE", title });
  await patchIssue(issue.key, { priority: 2 });
  await page.setViewportSize(WIDE);
  const writes = issueWrites(page, issue.key);
  const requests = () => requestsSent([writes]);
  const menu = openList(page);

  // Today's issue page: p opens on the current P2, 1 writes P1, and the focus goes back to the
  // page p was pressed on.
  await openIssue(page, issue.key, title);
  await page.keyboard.press("p");
  await expect(menu, "k-priority-menu: on the issue page p shows a menu").toBeVisible();
  await expectPriorityChoices(menu, "P2", "k-priority-menu: on the issue page p shows a menu");
  await expectFocusInBox(menu, "k-priority-menu: on the issue page p");
  await expectHighlight(menu, "P2", "k-priority-menu: on the issue page p highlights P2");
  await page.keyboard.press("1");
  await expect
    .poll(() => writesSent(writes), {
      message: "k-priority-menu: on the issue page 1 sends one write",
    })
    .toBe(1);
  expect(firstAttempts(writes)[0]?.input, "k-priority-menu: on the issue page 1 writes P1").toEqual(
    { priority: 1 }
  );
  await expect(
    priorityTrigger(page.getByTestId("issue-header"), issue.key),
    "k-priority-menu: on the issue page the header reads P1"
  ).toHaveText("P1");
  await expect
    .poll(() => onBody(page), {
      message: "k-priority-menu: on the issue page 1 gives the focus back to the page",
    })
    .toBe(true);

  // The project Board: p on the focused card opens its menu, and Escape writes nothing and gives
  // the focus back to the card.
  await page.goto("/projects/CORE");
  await page.getByRole("button", { exact: true, name: "Board" }).click();
  const card = page.getByRole("article", { name: `${issue.key} ${title}` });
  await card.focus();
  await page.keyboard.press("p");
  await expect(
    openTrigger(page),
    "k-priority-menu: on the Board p opens the card's menu"
  ).toHaveAccessibleName(`Priority of ${issue.key} P1`);
  await expectPriorityChoices(menu, "P1", "k-priority-menu: on the Board p opens the card's menu");
  await expectFocusInBox(menu, "k-priority-menu: on the Board p");
  // The count is read just before Escape and again once the menu is closed, so any request
  // between the two fails the step, a resend of any key included (Verification, "A write is one
  // key"). A request the issue page's write sent again before then - one the load of this page
  // sends again because it was still out, which CI run 44535117619 counted here - is not Escape's.
  const beforeBoardEscape = requests();
  await page.keyboard.press("Escape");
  await expect(menu, "k-priority-menu: on the Board Escape closes the menu").toHaveCount(0);
  expect(requests(), "k-priority-menu: on the Board Escape writes nothing").toBe(beforeBoardEscape);
  await expect(
    card,
    "k-priority-menu: on the Board Escape gives the focus back to the card"
  ).toBeFocused();

  // The project List: a click on a row's priority control opens the same menu, and nothing more
  // is written.
  const beforeList = requests();
  await page.getByRole("button", { exact: true, name: "List" }).click();
  const listControl = priorityTrigger(
    page.getByRole("row", { exact: true, name: `${issue.key} ${title}` }),
    issue.key
  );
  await listControl.click();
  await expect(menu, "k-priority-menu: on the List a click opens the same menu").toBeVisible();
  await expectPriorityChoices(
    menu,
    "P1",
    "k-priority-menu: on the List a click opens the same menu"
  );
  await page.keyboard.press("Escape");
  await expect(menu, "k-priority-menu: on the List Escape closes the menu").toHaveCount(0);
  expect(requests(), "k-priority-menu: on the List nothing more is written").toBe(beforeList);
  await expect(
    listControl,
    "k-priority-menu: on the List Escape gives the focus back to the clicked control"
  ).toBeFocused();
});

// ---------------------------------------------------------------------------
// k-status-menu
// ---------------------------------------------------------------------------

test("k-status-menu: s opens the status menu on the issue page, where the arrows and Escape write nothing", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger rollout";
  const issue = await createIssue({ project: "CORE", title });
  await patchIssue(issue.key, { status: "todo" });
  await page.setViewportSize(WIDE);
  const writes = issueWrites(page, issue.key);
  const requests = () => requestsSent([writes]);
  const status = statusTrigger(page);
  const menu = listOf(status);

  await openIssue(page, issue.key, title);
  await page.keyboard.press("s");
  await expect(menu, "k-status-menu: s shows a menu").toBeVisible();
  const choices = menu.getByRole("option");
  await expect(choices, "k-status-menu: the menu holds the issue's statuses").toHaveCount(
    OPEN_STATUSES.length
  );
  for (const [index, label] of OPEN_STATUSES.entries()) {
    await expect(
      choices.nth(index),
      `k-status-menu: choice ${index + 1} is ${label}`
    ).toHaveAccessibleName(label);
  }
  await expect(menu.locator("kbd"), "k-status-menu: no digit writes a status").toHaveCount(0);
  await expect(
    menu.getByRole("option", { exact: true, name: "Todo" }),
    "k-status-menu: Todo is the current value"
  ).toHaveAttribute("aria-selected", "true");
  await expectFocusInBox(menu, "k-status-menu: s");
  await expectHighlight(menu, "Todo", "k-status-menu: s highlights the current Todo");
  const beforeArrowDown = requests();
  await page.keyboard.press("ArrowDown");
  await expectHighlight(
    menu,
    "In progress",
    "k-status-menu: ArrowDown moves the highlight to In progress"
  );
  expect(requests(), "k-status-menu: ArrowDown sends no request").toBe(beforeArrowDown);
  const beforeEscape = requests();
  await page.keyboard.press("Escape");
  await expect(menu, "k-status-menu: Escape closes the menu").toHaveCount(0);
  expect(requests(), "k-status-menu: Escape sends no request").toBe(beforeEscape);
  await expect(status, "k-status-menu: after Escape the status still reads Todo").toHaveText(
    "Todo"
  );
  await expect
    .poll(() => onBody(page), { message: "k-status-menu: Escape gives the focus back to the page" })
    .toBe(true);

  await page.keyboard.press("s");
  await expectHighlight(menu, "Todo", "k-status-menu: s opens the menu again on Todo");
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect
    .poll(() => writesSent(writes), {
      message: "k-status-menu: s, ArrowDown, Enter sends one write",
    })
    .toBe(1);
  expect(
    firstAttempts(writes)[0]?.input,
    "k-status-menu: Enter writes the highlighted In progress"
  ).toEqual({ status: "in_progress" });
  await expect(status, "k-status-menu: the header shows In progress").toHaveText("In progress");
  await expect
    .poll(() => onBody(page), { message: "k-status-menu: Enter gives the focus back to the page" })
    .toBe(true);
});

// ---------------------------------------------------------------------------
// k-choice-writes-on-enter
// ---------------------------------------------------------------------------

/** The ids k-choice-writes-on-enter and k-menu-contrast's lists read, and the issue's title. */
interface ChoiceSeeds {
  readonly askId: string;
  readonly issueKey: string;
  readonly title: string;
}

/** The seeds of k-choice-writes-on-enter (Verification v15): two people who have signed in, a
 *  project whose default repository is one repository, an open issue whose own repository is
 *  another, and a question on that issue that waits on the reader, so the issue header's
 *  repository list holds three choices: Inherit, the project's default and the issue's own. The
 *  people are alice and bob, whom the reset signs in; the project is CORE, whose default is
 *  acme/widgets; the issue is CORE's, on acme/gadgets, and alice holds it as its creator; and the
 *  question waits on her. */
async function seedChoices(): Promise<ChoiceSeeds> {
  await createProject({ key: "CORE", name: "Core" });
  await putProjectDefaultRepository("CORE", "acme/widgets");
  const title = "Ledger rollout";
  const issue = await createIssue({ project: "CORE", repo: "acme/gadgets", title });
  const ask = await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);
  return { askId: ask.id, issueKey: issue.key, title };
}

/** The keys k-choice-writes-on-enter presses on a choice control, in order, before the letter
 *  that starts a choice. */
const CHOICE_KEYS = ["ArrowDown", "ArrowUp", "Home", "End", "PageDown", "PageUp"] as const;

/** How many presses of Tab may pass before the focus reaches a control from the page's start. */
const TAB_PRESSES = 200;

/** One choice control of R6, as k-choice-writes-on-enter drives it. */
interface ChoiceControl {
  /** What the failures call it: "the assignee". */
  readonly what: string;
  readonly control: Locator;
  /** Every write the control sends, counted at the server's route. */
  readonly writes: readonly KeyedWrite<unknown>[];
  /** A letter that starts a choice other than the item's value. */
  readonly letter: string;
  /** The body of the write of the choice the control shows as `choice`. */
  readonly body: (choice: string) => unknown;
  /** The rows of the list a click opens, as `MultiSelect` draws each: its label after the `✓` of
   *  the item's value. Unset where the row does not read them. */
  readonly choices?: readonly string[];
}

/** What a choice control shows: a native select's own value; else, while its list is open, the
 *  option the list's text box highlights, and whether that option is the item's value
 *  (`aria-selected`); else the text of the control itself. */
interface Shown {
  readonly open: boolean;
  readonly text: string;
  /** A native select's value. */
  readonly value?: string;
  /** Whether the option an open list highlights is the one that holds the item's value. */
  readonly picked?: boolean;
}

/** Reads what `control` shows now (`Shown`). */
function shownBy(control: Locator): Promise<Shown> {
  return control.evaluate((element): Shown => {
    if (element instanceof HTMLSelectElement) {
      const text = element.selectedOptions[0]?.textContent ?? "";
      return { open: false, text: text.trim(), value: element.value };
    }
    const expanded = element.getAttribute("aria-expanded") === "true";
    const panel = expanded
      ? document.getElementById(element.getAttribute("aria-controls") ?? "")
      : null;
    const box = panel?.querySelector('[role="combobox"]');
    const active = document.getElementById(box?.getAttribute("aria-activedescendant") ?? "");
    if (active !== null) {
      return {
        open: true,
        picked: active.getAttribute("aria-selected") === "true",
        text: (active.textContent ?? "").replace("✓", "").trim(),
      };
    }
    return { open: panel !== null, text: (element.textContent ?? "").trim() };
  });
}

/** Whether `shown` is a choice other than the item's value, which the control showed as `rest`
 *  with the focus on it and nothing pressed. */
function showsOther(shown: Shown, rest: Shown): boolean {
  if (shown.value !== undefined) return shown.value !== rest.value;
  if (shown.picked !== undefined) return !shown.picked;
  return shown.text !== rest.text;
}

/** Puts the focus on `control` with the keyboard: Tab, press by press from where the focus is,
 *  until the control holds it. A control no press reaches, one the page does not draw included
 *  (`isActive`), fails by `message` once the presses run out. */
async function tabTo(page: Page, control: Locator, message: string): Promise<void> {
  // A Tab moves the focus inside its own key press, so each read after one is settled.
  for (let press = 0; press < TAB_PRESSES && !(await isActive(control)); press += 1) {
    await page.keyboard.press("Tab");
  }
  expect(await isActive(control), message).toBe(true);
}

/** Presses `CHOICE_KEYS` and then `choice.letter`, reading before and after each press that it
 *  sent no request, and that at least one press showed a choice other than the item's value
 *  (`rest`). The choice shown after the last press is the answer. A press that sent a request
 *  fails on that step's own words, also where the request took the control off the page. */
async function pressChoiceKeys(
  page: Page,
  choice: ChoiceControl,
  rest: Shown,
  step: string
): Promise<Shown> {
  const say = `k-choice-writes-on-enter: ${choice.what}, ${step}`;
  let shown = rest;
  let other = false;
  for (const key of [...CHOICE_KEYS, choice.letter]) {
    const before = requestsSent([choice.writes]);
    await page.keyboard.press(key);
    // A request the press sends goes out in its handler, or in the render and the effects that
    // handler queued, all done within two frames. The control is waited for after them with the
    // bound of the row's own waits, and the count is read before that wait's answer is: a press
    // that wrote can take the control off the page - the hand-on's write hands the question on,
    // and its card leaves - and the step then fails as a press that sent a request, not on a read
    // of a control that left.
    await nextFrames(page);
    const gone = await choice.control.waitFor({ state: "attached", timeout: expectTimeout }).then(
      () => undefined,
      (error: unknown) => error
    );
    expect(requestsSent([choice.writes]), `${say}: ${key} sends no request`).toBe(before);
    if (gone !== undefined) throw gone;
    shown = await shownBy(choice.control);
    other ||= showsOther(shown, rest);
  }
  expect(other, `${say}: a key shows a choice other than the item's value, ${rest.text}`).toBe(
    true
  );
  return shown;
}

/** Presses `key`, the step `step`, and reads that the press sent no request, and that `choice`
 *  shows the item's value again (`rest`), with its list closed. The count is read before the
 *  control is, as `pressChoiceKeys` reads it: an Escape or a Tab that wrote can take the control
 *  off the page - the hand-on's write hands the question on, and its card leaves - and the step
 *  then fails as a press that sent a request, not on a read of a control that left. */
async function pressToItemValue(
  page: Page,
  choice: ChoiceControl,
  rest: Shown,
  key: "Escape" | "Tab",
  step: string
): Promise<void> {
  const say = `k-choice-writes-on-enter: ${choice.what}, ${step}`;
  const before = requestsSent([choice.writes]);
  await page.keyboard.press(key);
  await nextFrames(page);
  expect(requestsSent([choice.writes]), `${say} sends no request`).toBe(before);
  await expect
    .poll(
      async () => {
        const shown = await shownBy(choice.control);
        return !shown.open && (shown.value ?? shown.text) === (rest.value ?? rest.text);
      },
      { message: `${say} shows the item's value, ${rest.text}` }
    )
    .toBe(true);
}

/** Every step of k-choice-writes-on-enter on one choice control, on the page that draws it. */
async function choiceWritesOnEnter(page: Page, choice: ChoiceControl): Promise<void> {
  const say = `k-choice-writes-on-enter: ${choice.what}`;
  await tabTo(page, choice.control, `${say}: Tab reaches the control`);
  const rest = await shownBy(choice.control);

  // The keys with the focus on the control, then Escape.
  await pressChoiceKeys(page, choice, rest, "the keys on the focused control");
  await pressToItemValue(
    page,
    choice,
    rest,
    "Escape",
    "Escape after the keys on the focused control"
  );

  // The keys in the list a click opened, then Escape.
  await choice.control.click();
  if (choice.choices !== undefined) {
    await expect(
      listOf(choice.control).getByRole("option"),
      `${say}: a click opens the list of ${choice.choices.join(", ")}`
    ).toHaveText([...choice.choices]);
  }
  await pressChoiceKeys(page, choice, rest, "the keys in the list a click opened");
  await pressToItemValue(
    page,
    choice,
    rest,
    "Escape",
    "Escape after the keys in the list a click opened"
  );

  // The keys again, then Tab.
  await expect(choice.control, `${say}: Escape puts the focus back on the control`).toBeFocused();
  await pressChoiceKeys(page, choice, rest, "the keys before Tab");
  await pressToItemValue(page, choice, rest, "Tab", "Tab after the keys");

  // The keys again, then Enter: one request, of the choice shown.
  await expect(choice.control, `${say}: Tab leaves the focus on the control`).toBeFocused();
  const chosen = await pressChoiceKeys(page, choice, rest, "the keys before Enter");
  await page.keyboard.press("Enter");
  await expect
    .poll(() => writesSent(choice.writes), { message: `${say}: Enter sends one request` })
    .toBe(1);
  expect(
    firstAttempts(choice.writes)[0]?.input,
    `${say}: Enter writes the choice shown, ${chosen.text}`
  ).toEqual(choice.body(chosen.text));
}

// SLOW(>=2s): the issue page loaded and its assignee driven through four passes of seven keys.
// @webkit @firefox: the keys each engine sends to a focused control and to its list's text box,
// and the Tab that leaves that box.
test("k-choice-writes-on-enter: on the issue page the assignee's keys only change the choice shown, and only Enter writes @firefox @webkit", async ({
  page,
}) => {
  const { issueKey, title } = await seedChoices();
  await page.setViewportSize(WIDE);
  const writes = issueWrites(page, issueKey);
  await openIssue(page, issueKey, title);
  await choiceWritesOnEnter(page, {
    body: (choice) => ({ assignee: choice }),
    control: page.getByLabel(`Assignee of ${issueKey}`, { exact: true }),
    letter: "b",
    what: "the assignee",
    writes,
  });
});

// SLOW(>=2s): the issue page loaded and its repository driven through four passes of seven keys.
// @webkit @firefox: the keys each engine sends to a focused control and to its list's text box,
// and the Tab that leaves that box.
test("k-choice-writes-on-enter: on the issue page the repository's keys only change the choice shown, and only Enter writes @firefox @webkit", async ({
  page,
}) => {
  const { issueKey, title } = await seedChoices();
  await page.setViewportSize(WIDE);
  const writes = issueWrites(page, issueKey);
  await openIssue(page, issueKey, title);
  await choiceWritesOnEnter(page, {
    body: (choice) => ({ repo: choice }),
    // Inherit, CORE's default and the issue's own, which the issue holds (`seedChoices`).
    choices: ["Inherit", "acme/widgets", "✓acme/gadgets"],
    control: page.getByLabel(`Repository of ${issueKey}`, { exact: true }),
    letter: "a",
    what: "the repository",
    writes,
  });
});

// SLOW(>=2s): a question's reading view opened at 1024x768 and its hand-on driven through four
// passes.
// @webkit @firefox: the keys each engine sends to a focused control and to its list's text box,
// and the Tab that leaves that box.
test("k-choice-writes-on-enter: on a question's card in the reading view at 1024x768 the hand-on's keys only change the choice shown, and only Enter writes @firefox @webkit", async ({
  page,
}) => {
  const { askId } = await seedChoices();
  const writes = handOnWrites(page, askId);
  const inbox = await openInboxAt(page, "the reading view", 1);
  await goTo(inbox, askId);
  await choiceWritesOnEnter(page, {
    body: (choice) => ({ asked_of: choice }),
    control: handOnControl(inboxPane(page), askId),
    letter: "b",
    what: "the hand-on control",
    writes,
  });
});

/** The people the assignee list's row signs in beside alice and bob, whom the reset signs in:
 *  seven people, and with Unassigned eight rows, more than the list's box shows at once. */
const MORE_PEOPLE = ["carol", "dave", "erin", "frank", "grace"] as const;

/** Where the row an open single pick's list highlights stands against the list's box
 *  (`MultiSelect`): its words, whether its top is above the bottom of the sticky band that holds
 *  the list's search box, and whether its bottom is below the inner bottom of the panel's scroll
 *  box, each read from `getBoundingClientRect`. */
interface HighlightedRow {
  readonly name: string;
  readonly underSearch: boolean;
  readonly belowBox: boolean;
}

/** The row the open list of `control`, a single pick's trigger, highlights (`aria-activedescendant`
 *  of its search box), or undefined while the list is closed or highlights nothing. Its name is
 *  not `ref-picker.ts`'s `highlightedRow`, a locator: the zero-wait scan reads a wrapper by its
 *  name in every file (`zero-wait-probes.ts`), so each call of that locator would read as this
 *  probe. */
function highlightedRowPlace(control: Locator): Promise<HighlightedRow | undefined> {
  return control.evaluate((trigger): HighlightedRow | undefined => {
    const panel = document.getElementById(trigger.getAttribute("aria-controls") ?? "");
    const search = panel?.querySelector('[role="combobox"]');
    const row = document.getElementById(search?.getAttribute("aria-activedescendant") ?? "");
    if (!panel || !search || row === null) return undefined;
    const own = row.getBoundingClientRect();
    const band = (search.parentElement ?? search).getBoundingClientRect();
    const inner = panel.getBoundingClientRect().top + panel.clientTop + panel.clientHeight;
    return {
      belowBox: own.bottom > inner + 0.5,
      name: (row.textContent ?? "").replace("✓", "").trim(),
      underSearch: own.top < band.bottom - 0.5,
    };
  });
}

/** The scroll box of an open single pick's list (`MultiSelect`'s panel): the height of its rows,
 *  the height it shows, and how far down it is scrolled. */
interface ListScroll {
  readonly clientHeight: number;
  readonly scrollHeight: number;
  readonly scrollTop: number;
}

/** How the open list of `control`, a single pick's trigger, is scrolled (`ListScroll`), or
 *  undefined while the list is closed. */
function listScroll(control: Locator): Promise<ListScroll | undefined> {
  return control.evaluate((trigger): ListScroll | undefined => {
    const panel = document.getElementById(trigger.getAttribute("aria-controls") ?? "");
    if (panel === null) return undefined;
    const { clientHeight, scrollHeight, scrollTop } = panel;
    return { clientHeight, scrollHeight, scrollTop };
  });
}

// SLOW(>=2s): five more people signed in, the issue page loaded, and the assignee's list walked
// from End up to its first row and opened again by ArrowUp.
// @webkit @firefox: the scroll that keeps the list's highlighted row in sight under its sticky
// search box (`MultiSelect`'s layout effect), which rests on each engine's layout of the list,
// and the keys each engine sends to a closed trigger: End and ArrowUp open the list and move its
// highlight.
test("k-choice-writes-on-enter: on the issue page with seven people and no assignee, End on the closed assignee, each ArrowUp after it up to the first row, and ArrowUp on the closed assignee each leave the highlighted row inside the list's box, below its search box @firefox @webkit", async ({
  page,
}) => {
  const { issueKey, title } = await seedChoices();
  await patchIssue(issueKey, { assignee: null });
  for (const login of MORE_PEOPLE) await sessionCookie(login);
  await page.setViewportSize(WIDE);
  await openIssue(page, issueKey, title);
  const control = page.getByLabel(`Assignee of ${issueKey}`, { exact: true });
  const say = (text: string) => `k-choice-writes-on-enter: the assignee's list, ${text}`;
  const inView = (step: string) =>
    expect
      .poll(() => highlightedRowPlace(control), {
        message: say(
          `${step}: the highlighted row stands inside the list's box, below its search box`
        ),
      })
      .toMatchObject({ belowBox: false, underSearch: false });

  // A click opens the list once, so its read of the people lands before the keys walk it.
  await control.click();
  const options = listOf(control).getByRole("option");
  await expect
    .poll(() => options.count(), { message: say("the list holds Unassigned and seven people") })
    .toBeGreaterThanOrEqual(8);
  // The poll above saw every row of the list, which one read of the people draws together.
  const count = await options.count();
  await page.keyboard.press("Escape");
  await expect(listOf(control), say("Escape closes the list")).toHaveCount(0);
  await expect(control, say("Escape leaves the focus on the closed assignee")).toBeFocused();

  await page.keyboard.press("End");
  await inView("End on the closed assignee");
  // The highlighted row can stand under the search box or below the box only while the list
  // scrolls, which it does only while its rows overflow its box (`max-h-72`): End has scrolled
  // it, or no read of this row tests the scroll.
  const scroll = await listScroll(control);
  if (scroll === undefined) throw new Error(say("End on the closed assignee leaves no list open"));
  expect(
    scroll.scrollHeight,
    say(`after End the list's rows overflow its box, ${scroll.clientHeight} px high`)
  ).toBeGreaterThan(scroll.clientHeight);
  expect(scroll.scrollTop, say("after End the list is scrolled down from its top")).toBeGreaterThan(
    0
  );
  for (let press = 1; press < count; press += 1) {
    await page.keyboard.press("ArrowUp");
    await inView(`ArrowUp ${press} after End`);
  }
  await page.keyboard.press("Escape");
  await expect(listOf(control), say("Escape closes the list again")).toHaveCount(0);
  await expect(control, say("Escape puts the focus back on the closed assignee")).toBeFocused();
  await page.keyboard.press("ArrowUp");
  await inView("ArrowUp on the closed assignee, which shows no one");
});

// ---------------------------------------------------------------------------
// k-menu-contrast (the menus' part; the single-pick lists' part is its own row)
// ---------------------------------------------------------------------------

/** The two themes k-menu-contrast reads every menu and list in. Dispatch follows the system's
 *  `prefers-color-scheme` alone (`styles.css`), so `emulateMedia` switches a page it has loaded
 *  from one to the other, and each page is loaded once for both. */
const THEMES = ["light", "dark"] as const;

/** Reads the contrast of `target`, failing softly under 4.5:1 with `what` and the choice `name`
 *  named, and at once, with the same names, where `contrastOf` refuses the element: a native
 *  `<select>` or `<option>` with a transparent background, or no opaque background under it. */
async function expectReadable(target: Locator, what: string, name: string): Promise<void> {
  const reading = await contrastOf(target).catch((error: unknown) => {
    throw new Error(
      `k-menu-contrast: ${what}, ${name}: ${error instanceof Error ? error.message : String(error)}`,
      { cause: error }
    );
  });
  expect
    .soft(
      reading.ratio,
      `k-menu-contrast: ${what}, ${name}: ${reading.color} on ${reading.background}`
    )
    .toBeGreaterThanOrEqual(4.5);
}

/** Opens a menu with `key` and reads the contrast of each of its choices, the options named
 *  `names`, and of the digit each draws, failing softly for each pair under 4.5:1 with the theme,
 *  the menu and the choice named. */
async function readMenuContrast(
  page: Page,
  key: string,
  what: string,
  names: readonly string[]
): Promise<void> {
  const menu = openList(page);
  await page.keyboard.press(key);
  await expect(menu, `k-menu-contrast: ${what}: ${key} opens the menu`).toBeVisible();
  for (const name of names) {
    const choice = menu.getByRole("option", { exact: true, name });
    await expectReadable(choice, what, name);
    // The menu draws each choice's digit in the render that drew the choice, read above.
    if ((await choice.locator("kbd").count()) > 0) {
      await expectReadable(choice.locator("kbd"), what, `${name}'s digit`);
    }
  }
  await page.keyboard.press("Escape");
  await expect(menu, `k-menu-contrast: ${what}: Escape closes the menu`).toHaveCount(0);
}

// SLOW(>=2s): the Inbox and the issue page each loaded once, and three menus read in each of two
// themes.
test("k-menu-contrast: every choice of the snooze, priority and status menus reads at 4.5:1 or more in the light and the dark theme", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger rollout";
  const issue = await createIssue({ project: "CORE", title });
  await patchIssue(issue.key, { priority: 2 });
  const ask = await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);
  const inbox = await openInboxAt(page, "the reading pane", 1);
  await goTo(inbox, ask.id);
  for (const colorScheme of THEMES) {
    await page.emulateMedia({ colorScheme });
    const theme = `the ${colorScheme} theme`;
    await readMenuContrast(
      page,
      "b",
      `${theme}, the Snooze menu`,
      SNOOZE_CHOICES.map(([label]) => label)
    );
    await readMenuContrast(page, "p", `${theme}, the priority menu`, PRIORITY_CHOICES);
  }
  await openIssue(page, issue.key, title);
  for (const colorScheme of THEMES) {
    await page.emulateMedia({ colorScheme });
    await readMenuContrast(page, "s", `the ${colorScheme} theme, the status menu`, OPEN_STATUSES);
  }
});

/** Reads the contrast of the shared list's trigger `control` and of each choice of its list,
 *  failing softly for each pair under 4.5:1, with the theme, the control and the choice named.
 *  The trigger is read on the element that draws its text, and its choices in the list a click
 *  on it opens, once that list holds `fromRead`, a choice only the list's own read brings. A
 *  native select draws its open list outside the page, so it is read itself, with each of its
 *  `<option>`s where it stands and no click; `contrastOf` refuses either by name when its own
 *  background is transparent. */
async function readChoiceContrast(control: Locator, what: string, fromRead: string): Promise<void> {
  // The caller found the control drawn, and whether it is a native select is its tag, which no
  // later read of the page changes, so one instant's answer is the answer.
  const native = await control.evaluate((element) => element instanceof HTMLSelectElement);
  if (native) {
    await expectReadable(control, what, "the control");
    const nativeOptions = control.locator("option");
    // A native select holds every one of its options in the page from its first draw.
    const nativeCount = await nativeOptions.count();
    for (let index = 0; index < nativeCount; index += 1) {
      const option = nativeOptions.nth(index);
      // An option's words are in the page from the select's first draw, and they only name the
      // choice in a failure's message.
      await expectReadable(option, what, `the choice ${(await option.textContent())?.trim()}`);
    }
    return;
  }
  await expectReadable(
    control.locator("xpath=.//*[text()[normalize-space()]]").first(),
    what,
    "the control"
  );
  await control.click();
  const list = listOf(control);
  const options = list.getByRole("option");
  await expect(
    list.getByRole("option", { exact: true, name: fromRead }),
    `k-menu-contrast: ${what}: a click opens its list, and its read brings ${fromRead}`
  ).toBeVisible();
  // The wait above saw a choice only the list's read brings, so every choice is drawn.
  const count = await options.count();
  for (let index = 0; index < count; index += 1) {
    const option = options.nth(index);
    await expectReadable(
      option,
      what,
      `the choice ${(await option.innerText()).replace("✓", "").trim()}`
    );
  }
  await control.page().keyboard.press("Escape");
  await expect(list, `k-menu-contrast: ${what}: Escape closes its list`).toHaveCount(0);
}

// SLOW(>=2s): the issue page and the Inbox each loaded once, and three lists read in each of two
// themes.
test("k-menu-contrast: every choice of the assignee, repository and hand-on lists reads at 4.5:1 or more in the light and the dark theme", async ({
  page,
}) => {
  const { askId, issueKey, title } = await seedChoices();
  await page.setViewportSize(WIDE);
  await openIssue(page, issueKey, title);
  for (const colorScheme of THEMES) {
    await page.emulateMedia({ colorScheme });
    const theme = `the ${colorScheme} theme`;
    await readChoiceContrast(
      page.getByLabel(`Assignee of ${issueKey}`, { exact: true }),
      `${theme}, the assignee`,
      "bob"
    );
    // The issue's own acme/gadgets is a choice from the list's first draw; the project's default,
    // acme/widgets, is the one the project list's read brings.
    await readChoiceContrast(
      page.getByLabel(`Repository of ${issueKey}`, { exact: true }),
      `${theme}, the repository`,
      "acme/widgets"
    );
  }
  // The hand-on control of the question's card, which the question's reading view draws at
  // 1024x768 (a short row draws no card).
  const inbox = await openInboxAt(page, "the reading view", 1);
  await goTo(inbox, askId);
  for (const colorScheme of THEMES) {
    await page.emulateMedia({ colorScheme });
    await readChoiceContrast(
      handOnControl(inboxPane(page), askId),
      `the ${colorScheme} theme, the hand-on control`,
      "bob"
    );
  }
});

// ---------------------------------------------------------------------------
// k-phone
// ---------------------------------------------------------------------------

/** Holds the list `list`, which a tap opened on a phone, to D5's rules there, naming `what`: the
 *  list holds the focus and its text box does not, so no on-screen keyboard covers the choices,
 *  and each choice stands inside the viewport and is 44 px high or more. */
async function expectPhoneList(page: Page, list: Locator, what: string): Promise<void> {
  const choices = list.getByRole("option");
  await expect(choices.first(), `k-phone: ${what} has choices`).toBeVisible();
  await expect(
    list.getByRole("combobox"),
    `k-phone: ${what}'s text box does not have the focus`
  ).not.toBeFocused();
  await expect(list.getByRole("listbox"), `k-phone: ${what}'s list has the focus`).toBeFocused();
  // A menu draws all its choices in the render that drew the first, waited for above.
  const rects = await choices.evaluateAll((nodes) =>
    nodes.map((node) => {
      const words = node.cloneNode(true) as Element;
      for (const hidden of words.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
      const { bottom, height, left, right, top } = node.getBoundingClientRect();
      return { bottom, height, left, name: (words.textContent ?? "").trim(), right, top };
    })
  );
  const viewport = page.viewportSize();
  if (viewport === null) throw new Error("k-phone: the phone project sets no viewport");
  expect(rects.length, `k-phone: ${what} has choices`).toBeGreaterThan(0);
  for (const { bottom, height, left, name, right, top } of rects) {
    expect(
      height,
      `k-phone: ${what}'s choice ${name} is 44 px high or more`
    ).toBeGreaterThanOrEqual(44);
    expect(
      top >= 0 && left >= 0 && bottom <= viewport.height && right <= viewport.width,
      `k-phone: ${what}'s choice ${name} stands inside the viewport, ${viewport.width} by ${viewport.height}`
    ).toBe(true);
  }
}

// SLOW(>=2s): two asks seeded, two menus tapped open and two writes read back on a phone.
test("k-phone: a tap opens the Snooze and the priority menus, whose choices take a tap and write only when tapped @phone @phone-only", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issueOf = new Map<string, string>();
  for (const title of ["Ledger", "Mirror"]) {
    const issue = await createIssue({ project: "CORE", title: `${title} rollout` });
    await patchIssue(issue.key, { priority: 2 });
    const ask = await createAsk(
      issue.key,
      { question: `Ship the ${title.toLowerCase()} rollout?` },
      asker
    );
    issueOf.set(ask.id, issue.key);
  }
  await page.goto("/");
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(2);
  // The count above waited for both rows, so their ids are read once they are drawn.
  const [first, second] = await rows.evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
  );
  const secondKey = issueOf.get(second ?? "");
  if (first === undefined || second === undefined || secondKey === undefined) {
    throw new Error("the Inbox lists other asks than the two seeded");
  }
  const snoozes = [snoozeWrites(page, first), snoozeWrites(page, second)];
  const priorityWrites = issueWrites(page, secondKey);
  const otherWrites = [...issueOf.values()]
    .filter((key) => key !== secondKey)
    .map((key) => issueWrites(page, key));
  const watched = [...snoozes, priorityWrites, ...otherWrites];
  const sent = () => countOf(watched);

  // A tap on the first row's age, the part of a short row that is neither its owner link nor a
  // control (`selectRow`), opens its reading view, whose head draws the Snooze a short row does
  // not.
  await inboxRow(page, first).locator("time").first().tap();
  await expect(
    readingView(page),
    "k-phone: a tap on the first row opens its reading view"
  ).toHaveAttribute("data-inbox-reading-row", first);
  const snooze = paneHead(page).getByRole("button", { name: /^Snooze / });
  const snoozeMenu = listOf(snooze);
  // Waited for here, within `expectTimeout`, where the tap would wait out the action's timeout
  // for a head that draws no such button, so the base fails the row's first step by name.
  await expect(
    snooze,
    "k-phone: the first row's reading view draws its Snooze menu's trigger in its head"
  ).toBeVisible();
  await snooze.tap();
  await expect(
    snoozeMenu,
    "k-phone: a tap on the reading view head's Snooze opens the snooze menu"
  ).toBeVisible();
  await expectPhoneList(page, snoozeMenu, "the snooze menu");
  const beforeTapOutside = requestsSent(watched);
  await page.getByRole("heading", { exact: true, level: 1, name: "Inbox" }).tap();
  await expect(snoozeMenu, "k-phone: a tap outside the menu closes it").toHaveCount(0);
  expect(requestsSent(watched), "k-phone: a tap outside the menu sends no request").toBe(
    beforeTapOutside
  );
  await snooze.tap();
  await expectPhoneList(page, snoozeMenu, "the snooze menu a second tap opens");
  const snoozeBox = snoozeMenu.getByRole("combobox");
  await snoozeBox.tap();
  await expect(snoozeBox, "k-phone: a tap on the menu's text box gives it the focus").toBeFocused();
  await expect(snoozeMenu, "k-phone: a tap on the text box leaves the menu open").toBeVisible();
  expect(requestsSent(watched), "k-phone: a tap on the text box sends no request").toBe(
    beforeTapOutside
  );
  await snoozeMenu.getByRole("option", { exact: true, name: "Tomorrow" }).tap();
  await expect
    .poll(() => writesSent(snoozes[0] ?? []), {
      message: "k-phone: a tap on Tomorrow sends one snooze",
    })
    .toBe(1);
  expect(sent(), "k-phone: a tap on Tomorrow sends nothing else").toBe(1);

  // The snooze opens the next ask in the reading view; `selectRow` opens the second ask's view
  // where it shows another.
  await selectRow(page, second);
  const priority = priorityTrigger(paneHead(page), secondKey);
  const priorityMenu = listOf(priority);
  await priority.tap();
  await expect(
    priorityMenu,
    "k-phone: with the second ask open in the reading view, a tap on its head's priority opens its menu"
  ).toBeVisible();
  await expectPhoneList(page, priorityMenu, "the priority menu");
  await priorityMenu.getByRole("option", { exact: true, name: "P1" }).tap();
  await expect
    .poll(() => writesSent(priorityWrites), { message: "k-phone: a tap on P1 sends one write" })
    .toBe(1);
  expect(firstAttempts(priorityWrites)[0]?.input, "k-phone: a tap on P1 writes P1").toEqual({
    priority: 1,
  });
  expect(sent(), "k-phone: a tap on P1 sends nothing else").toBe(2);
});

// ---------------------------------------------------------------------------
// held-press (dispatch://INFRA-4248, folded into this issue: no check of the Verification names
// it). A control whose write is out is held, not disabled, so the keyboard's focus stays on it.
// ---------------------------------------------------------------------------

/** Whether `control` is the page's `document.activeElement`, read at once: a control the page
 *  does not draw is not, where `evaluate` would wait out its timeout for it, so `tabTo` walks
 *  its presses and fails by its own message. A locator that matches more than one element
 *  throws, as `evaluate`'s strict mode does. */
async function isActive(control: Locator): Promise<boolean> {
  // Read at once, not waited for: the Tab walk reads it after each press, which the press has
  // settled, and a control the page does not draw must read as not active rather than wait.
  const held = await control.evaluateAll((elements) =>
    elements.map((element) => element === document.activeElement)
  );
  if (held.length > 1) throw new Error(`isActive: the control matches ${held.length} elements`);
  return held[0] === true;
}

/** Presses `control` by the keyboard with `key`, and again while the write it sent is held, and
 *  reads that the focus stays on it while the write is out and once it has landed, and that the
 *  two presses sent one write (`writes`, as `holdWrites` caught them, let go after the second
 *  press). */
async function pressTwiceWhileHeld(
  page: Page,
  control: Locator,
  key: "Enter" | "Space",
  what: string,
  writes: HeldWrites
): Promise<void> {
  await control.focus();
  expect(await isActive(control), `held-press: ${what} takes the focus`).toBe(true);
  await page.keyboard.press(key);
  await expect(
    control,
    `held-press: ${what} is held (aria-disabled) while its write is out`
  ).toHaveAttribute("aria-disabled", "true");
  expect(
    await isActive(control),
    `held-press: ${what} keeps the focus while its write is out`
  ).toBe(true);
  await page.keyboard.press(key);
  expect(
    await isActive(control),
    `held-press: ${what} keeps the focus after a second ${key} while held`
  ).toBe(true);
  writes.release();
  await expect(control, `held-press: ${what} lets go once its write lands`).not.toHaveAttribute(
    "aria-disabled",
    "true"
  );
  expect(await isActive(control), `held-press: ${what} keeps the focus once its write lands`).toBe(
    true
  );
  expect(writes.bodies, `held-press: ${what} sent exactly one write for two presses`).toHaveLength(
    1
  );
}

// held-press proves no check of the Verification document; the file's header says why its title
// starts with that name.
// SLOW(>=2s): three writes, each held until its control is pressed again, on the issue page and
// on Settings.
test("held-press: a button, a form's submit and a checkbox keep the keyboard's focus while their write is out, and two presses send one write", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger rollout";
  const issue = await createIssue({ project: "CORE", title });
  await page.setViewportSize(WIDE);

  // A plain button: the issue header's pin.
  const pins = await holdWrites(page, "PUT", new RegExp(`^/api/v1/me/issues/${issue.key}/state$`));
  await openIssue(page, issue.key, title);
  const pin = page.getByTestId("issue-header").getByRole("button", { name: /^(Un)?pin issue$/i });
  await pressTwiceWhileHeld(page, pin, "Enter", "the issue's Pin button", pins);
  await expect(pin, "held-press: the pin landed").toHaveAccessibleName("Unpin issue");

  // A form's submit: Settings' New token.
  const mints = await holdWrites(page, "POST", /^\/api\/v1\/me\/agent-tokens$/);
  const scopeWrites = await holdWrites(page, "PATCH", /^\/api\/v1\/me\/agent-tokens\/[^/]+$/);
  await page.goto("/settings");
  await page.getByLabel("Token label").fill("Held press");
  const mint = page.getByRole("button", { exact: true, name: "New token" });
  await pressTwiceWhileHeld(page, mint, "Enter", "Settings' New token submit", mints);
  const approve = page.getByRole("checkbox", {
    exact: true,
    name: "Approve and control credentials (Held press)",
  });
  await expect(approve, "held-press: the minted token holds Approve").toBeChecked();

  // A checkbox: the minted token's Approve scope.
  await pressTwiceWhileHeld(page, approve, "Space", "the token's Approve checkbox", scopeWrites);
  await expect(approve, "held-press: the one write cleared Approve").not.toBeChecked();
  expect(
    JSON.parse(scopeWrites.bodies[0] ?? "{}"),
    "held-press: the write names Act as you alone"
  ).toEqual({ scopes: ["person"] });
});

// ---------------------------------------------------------------------------
// k-link-click, k-link-keys and k-link-copy (R1, R2, R23): a link of a document, a comment, a
// question or a past version opens on a click and from the keyboard, and `y` copies the address
// of the link the focus is on.
// ---------------------------------------------------------------------------

/** The other site every web link below points to, which `newPageAt` serves. */
const AWAY = `${OTHER_SITE}/a`;
/** The other site the link inside the spec's commented text points to. */
const COMMENTED_AWAY = `${OTHER_SITE}/b`;

/** The issue the links name, and the issue whose spec holds them. */
interface LinkSeed {
  readonly other: { readonly key: string; readonly title: string };
  readonly issueKey: string;
  /** The comment on the issue that holds the first three links, on its Conversation tab. */
  readonly commentId: string;
  /** The approval request of the spec, which the Inbox's reading pane draws as the spec. */
  readonly approvalAskId: string;
}

/** The seeds of k-link-click and k-link-keys: an open issue whose spec holds a link to another
 *  site, a `dispatch://` link to another issue, that issue's bare key, its address as a link
 *  whose text is the address (whose title the editor draws beside a link it hides), and a web
 *  link inside text a comment is anchored to; a comment on the issue holding the first three
 *  links; a later version of the spec, so its version 1 is a past version; and an approval
 *  request of the spec. */
async function seedLinkSpec(): Promise<LinkSeed> {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger handover";
  const other = await createIssue({ project: "CORE", title });
  const issue = await createIssue({
    project: "CORE",
    spec: [
      "# Mirror replay",
      "",
      `Read [a page elsewhere](${AWAY}) first.`,
      "",
      `See [the other issue](dispatch://${other.key}) for the handover.`,
      "",
      `The bare key ${other.key} names it too.`,
      "",
      `Its address <dispatch://${other.key}> reads as its title.`,
      "",
      `A reviewer asked about [the commented page](${COMMENTED_AWAY}) here.`,
      "",
    ].join("\n"),
    title: "Mirror replay",
  });
  await createComment(issue.key, {
    anchor: { artifact: "spec", quote: "the commented page" },
    body: "Is this the right page?",
  });
  const comment = await createComment(issue.key, {
    body: `Read [a page elsewhere](${AWAY}), see [the other issue](dispatch://${other.key}) and ${other.key}.`,
  });
  await editArtifact(
    issue.primary_artifact_id,
    { ops: [{ after: "end", markdown: "A later line.", op: "insert" }] },
    asker
  );
  const approval = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary: "The replay draft." }, asker)
  );
  return {
    approvalAskId: approval.ask.id,
    commentId: comment.id,
    issueKey: issue.key,
    other: { key: other.key, title },
  };
}

/** The four places a reader reads the spec's links in: the issue's Spec tab, which the reader
 *  edits, version 1 of it, the Inbox's reading pane on the approval request of it, and the
 *  comment on the issue's Conversation tab. */
type LinkView = "the editable spec" | "the past version" | "the reading pane" | "the comment";

/** The three links of a view: the web link, the `dispatch://` link, and the title drawn for the
 *  bare key (in the comment, the reference link the body draws for it). */
interface ViewLinks {
  readonly scope: Locator;
  readonly away: Locator;
  readonly issue: Locator;
  readonly key: Locator;
}

/** Opens `view` at 1440x900 and answers its links, once the key's title is drawn. */
async function openLinkView(page: Page, seed: LinkSeed, view: LinkView): Promise<ViewLinks> {
  await page.setViewportSize(WIDE);
  let scope: Locator;
  if (view === "the editable spec") {
    await openSpec(page, seed.issueKey, "here.");
    scope = documentEditor(page);
  } else if (view === "the past version") {
    await page.goto(`/issues/${seed.issueKey}/artifacts/spec?v=1`);
    scope = page
      .getByRole("region", { name: "Document version 1" })
      .getByRole("textbox", { name: "Document editor" });
  } else if (view === "the reading pane") {
    await page.goto("/?assignee=everyone");
    await selectRow(page, seed.approvalAskId);
    scope = inboxPane(page).getByRole("textbox", { name: "Document editor" });
  } else {
    await page.goto(`/issues/${seed.issueKey}/conversation`);
    scope = page.locator(`[data-turn="comment:${seed.commentId}"]`);
  }
  const reference = `dispatch://${seed.other.key}`;
  const links: ViewLinks =
    view === "the comment"
      ? {
          away: scope.getByRole("link", { exact: true, name: "a page elsewhere" }),
          issue: scope.locator(`a[data-dispatch-ref="${reference}"]`).nth(0),
          key: scope.locator(`a[data-dispatch-ref="${reference}"]`).nth(1),
          scope,
        }
      : {
          away: scope.getByRole("link", { exact: true, name: "a page elsewhere" }),
          issue: scope.getByRole("link", { exact: true, name: "the other issue" }),
          // The bare key's own title, in its own paragraph: the address link of the next
          // paragraph draws the same reference's title and can draw it first, so a wait for the
          // first title anywhere let a row reach for the bare key's before the editor had drawn
          // it (CI run 44535117619, whose page shows the bare key as plain text).
          key: scope
            .locator("p", { hasText: "The bare key" })
            .locator(`a.dispatch-reference[data-dispatch-href="${reference}"]`),
          scope,
        };
  await expect(links.away, `${view} draws the web link`).toBeVisible();
  await expect(links.key, `${view} draws the key's title`).toContainText(seed.other.title);
  // The pointer stays where the last step left it, which a new page can draw a link under: the
  // key's title, say, which a row clicks and then opens the view again. Resting there, it opens
  // that link's hover card (`RefPreview.tsx`), and the card covers the line below it, so a click
  // the row makes next lands on the card - Firefox measured it, 2 of 4 runs of the first row
  // below in the tester's pod (test handoff v14). The pointer goes to the page's corner, on no
  // link, and the row waits out the card's close.
  await page.mouse.move(1, 1);
  await expect(
    page.locator(`#${REF_PREVIEW_CARD_ID}`),
    `${view} shows no hover card once the pointer has left its links`
  ).toBeHidden();
  return links;
}

/**
 * Runs `open`, which opens a new page of `page`'s context and answers it, or `undefined` when it
 * opened none, and hands what it answered to `read`; then closes that page and waits until
 * `page`'s window has the size it had before `open` ran. Answers what `read` answers. Every row
 * here that closes a new page and goes on with the first page closes it here, and the size is
 * read here, before `open`, so no row can leave it unread.
 *
 * Firefox under Playwright gives the first page's window the closed page's size and keeps it:
 * 1280x806 outside, where it was 1440x986, the page still reading 1440x900 inside. A press then
 * lands away from its aim. A click near the bottom of the viewport reached no element of the
 * page (builds Jobs `infra-2160-probe-how-qp655` and `infra-2160-probe-low-8858d`, 2026-11-23).
 * A press aimed past x 1280, as at the spec margin's card, is outside the window: the page moves
 * 160 px left in the window as the press reaches it, and the press's first events can land where
 * the page stood before it moved. A click aimed at the margin card's `dispatch://` link at
 * x 1316 went down at 1156, on the web link beside it, and came up at 1316, the page's left edge
 * on the screen going from x 4 to -156 between the two, so the click went to the card's preview,
 * which unfolded the card and opened no link (builds Job `infra-2160-ffl-probe2-click-4dvtv`,
 * 2026-11-24). That is the harness's window around its emulated viewport: a person's press
 * reaches the page where their pointer is. Setting the viewport again sizes the window to the
 * page again, and the wait holds until the window has the size it had before `open`, since the
 * window can take it after `setViewportSize` has answered (18 of 82 reads straight after it
 * still found 1280x806, builds Job `infra-2160-ffl-probe3-click-cxzpd`).
 */
async function withNewPage<Opened extends Page | undefined, T>(
  page: Page,
  open: () => Promise<Opened>,
  read: (opened: Opened) => T | Promise<T>
): Promise<T> {
  const size = await page.evaluate(() => [window.outerWidth, window.outerHeight] as const);
  const opened = await open();
  const answer = await read(opened);
  if (opened === undefined) return answer;
  await opened.close();
  await page.setViewportSize(page.viewportSize() ?? WIDE);
  await page.waitForFunction(
    ([width, height]) => window.outerWidth === width && window.outerHeight === height,
    size
  );
  return answer;
}

/** What `act` opens in a new page of `page`'s context, whose address it answers once loaded; the
 *  page is closed (`withNewPage`). A press that opens no page fails by `what`, the press's own
 *  words (`newPageAt`). */
async function openedAddress(
  page: Page,
  act: () => Promise<unknown>,
  what: string
): Promise<string> {
  return withNewPage(
    page,
    () => newPageAt(page.context(), act, what),
    (opened) => opened.url()
  );
}

/** Whether `page` shows the other issue's page, reached with no new load of the application. */
async function expectInApp(page: Page, seed: LinkSeed, what: string): Promise<void> {
  await expect(page, `${what} moves the page to ${seed.other.key}`).toHaveURL(
    new RegExp(`/issues/${seed.other.key}(/|$)`)
  );
  expect(await reloaded(page), `${what} loads no new document`).toBe(false);
}

/** The spec's stored Markdown, as the server holds it. */
async function storedSpec(seed: LinkSeed): Promise<string> {
  return (await getIssueArtifactText(seed.issueKey, "spec")).markdown;
}

/** A plain primary click on each of `view`'s three links: the web link opens a new page at the
 *  other site and the Dispatch page stays where it was; the `dispatch://` link and the key move
 *  the same page to the other issue, with no new load of the application. */
async function clickEachLink(page: Page, seed: LinkSeed, view: LinkView): Promise<void> {
  let links = await openLinkView(page, seed, view);
  const at = page.url();
  expect(
    await openedAddress(
      page,
      () => links.away.click(),
      `k-link-click: in ${view}, a click on the web link`
    ),
    `k-link-click: in ${view}, a click on the web link opens a new page at ${AWAY}`
  ).toBe(AWAY);
  expect(page.url(), `k-link-click: in ${view}, the Dispatch page stays at its address`).toBe(at);

  await markDocument(page);
  await links.issue.click();
  await expectInApp(page, seed, `k-link-click: in ${view}, a click on the dispatch:// link`);

  links = await openLinkView(page, seed, view);
  await markDocument(page);
  await links.key.click();
  await expectInApp(page, seed, `k-link-click: in ${view}, a click on the key's title`);
}

// SLOW(>=2s): the spec opened four times, two new pages loaded, and four new tabs of Dispatch.
// @firefox: the click each engine sends on a link, whose fields decide the path the link opens
// through (Firefox sends Enter's click on a focused link with `detail` 1, Chromium with 0).
test("k-link-click: in the spec the reader edits, a plain click opens each link, a pointer over one shows no Open link card, and Ctrl and a middle click open a new tab @firefox", async ({
  page,
}) => {
  const seed = await seedLinkSpec();
  await clickEachLink(page, seed, "the editable spec");

  // The web link inside the text a comment is anchored to opens too, rather than the comment.
  await openLinkView(page, seed, "the editable spec");
  const commented = documentEditor(page).getByRole("link", {
    exact: true,
    name: "the commented page",
  });
  expect(
    await openedAddress(
      page,
      () => commented.click(),
      "k-link-click: in the editable spec, a click on the link inside commented text"
    ),
    `k-link-click: in the editable spec, a click on the link inside commented text opens ${COMMENTED_AWAY}`
  ).toBe(COMMENTED_AWAY);

  const links = await openLinkView(page, seed, "the editable spec");
  await links.away.hover();
  // The Open link card showed as the pointer rested on a link, so the pointer rests the
  // half second the check names before the row looks for the card.
  await page.waitForTimeout(500);
  const cards = await page
    .getByText("Open link", { exact: true })
    .evaluateAll((nodes) => nodes.filter((node) => (node as HTMLElement).checkVisibility()).length);
  expect(cards, "k-link-click: a pointer over the web link shows no Open link card").toBe(0);

  // Headless Chromium has no primary selection, so a middle click pastes nothing here whether or
  // not the editor prevents its press and release: that part of the check is held by the
  // editor's own unit test, "a middle button's press and release on a link are prevented, so the
  // browser pastes nothing" (packages/proof-editor/tests/dispatch-link-open.test.ts). The row
  // reads that the Markdown is the same after the middle clicks all the same.
  const before = await storedSpec(seed);
  const otherPage = new URL(`/issues/${seed.other.key}`, page.url()).href;
  for (const [link, what] of [
    [links.issue, "the dispatch:// link"],
    [links.key, "the key's title"],
  ] as const) {
    expect(
      await openedAddress(
        page,
        () => link.click({ modifiers: ["ControlOrMeta"] }),
        `k-link-click: a Ctrl+click on ${what}`
      ),
      `k-link-click: a Ctrl+click on ${what} opens a new page at the other issue`
    ).toMatch(new RegExp(`^${otherPage}(/|$)`));
    expect(
      await openedAddress(
        page,
        () => link.click({ button: "middle" }),
        `k-link-click: a middle click on ${what}`
      ),
      `k-link-click: a middle click on ${what} opens a new page at the other issue`
    ).toMatch(new RegExp(`^${otherPage}(/|$)`));
  }
  expect(
    page.url(),
    "k-link-click: the spec stays open through the Ctrl and middle clicks"
  ).toContain(`/issues/${seed.issueKey}/spec`);
  expect(
    await storedSpec(seed),
    "k-link-click: the middle clicks leave the spec's Markdown as it was"
  ).toBe(before);
});

// SLOW(>=2s): the spec and a project document each opened once, a new tab loaded by a Ctrl+click
// in each, and one move in place from each.
// @firefox: the click each engine sends on a link (the row above says why).
test("k-link-click: in the spec the reader edits and in a project document, a plain click on a link opens it after a Ctrl+click on that link opened it in a new tab @firefox", async ({
  page,
}) => {
  const seed = await seedLinkSpec();
  const runbook = await createProjectDocument("CORE", {
    content: `# Replay runbook\n\nSee [the other issue](dispatch://${seed.other.key}) for the handover.\n`,
    name: "replay-runbook.md",
  });
  await page.setViewportSize(WIDE);
  for (const [view, path] of [
    ["the editable spec", `/issues/${seed.issueKey}/spec`],
    ["the project document", `/projects/CORE/documents/${runbook.artifact.slug}`],
  ] as const) {
    const step = (text: string) => `k-link-click: in ${view}, ${text}`;
    await page.goto(path);
    await expect(page.getByRole("status", { name: "connected" })).toHaveText("connected");
    const link = documentEditor(page).getByRole("link", { exact: true, name: "the other issue" });
    await expect(link, step("the document draws the dispatch:// link")).toBeVisible();
    // A Ctrl+click opens the link in a new tab; the editor may select around the link as it does.
    expect(
      await openedAddress(
        page,
        () => link.click({ modifiers: ["ControlOrMeta"] }),
        step("a Ctrl+click on the dispatch:// link")
      ),
      step("a Ctrl+click on the dispatch:// link opens a new page at the other issue")
    ).toMatch(new RegExp(`/issues/${seed.other.key}(/|$)`));
    // R1: a primary click with no modifier key on a link opens it, whatever the reader pressed
    // before.
    await markDocument(page);
    await link.click();
    await expect(
      page,
      step("a plain click on the link after the Ctrl+click moves the page to the other issue")
    ).toHaveURL(new RegExp(`/issues/${seed.other.key}(/|$)`));
    expect(await reloaded(page), step("the plain click loads no new document")).toBe(false);
  }
});

// SLOW(>=2s): the spec and a project document each opened once, a text selected in each and one
// move in place from each.
// @firefox: the click each engine sends on a link (k-link-click's first row says why).
test("k-link-click: in the spec the reader edits and in a project document, one plain click on a link inside text the reader selected opens it @firefox", async ({
  page,
}) => {
  const seed = await seedLinkSpec();
  const runbook = await createProjectDocument("CORE", {
    content: `# Replay runbook\n\nSee [the other issue](dispatch://${seed.other.key}) for the handover.\n`,
    name: "replay-runbook.md",
  });
  await page.setViewportSize(WIDE);
  for (const [view, path] of [
    ["the editable spec", `/issues/${seed.issueKey}/spec`],
    ["the project document", `/projects/CORE/documents/${runbook.artifact.slug}`],
  ] as const) {
    const step = (text: string) => `k-link-click: in ${view}, ${text}`;
    await page.goto(path);
    await expect(page.getByRole("status", { name: "connected" })).toHaveText("connected");
    const link = documentEditor(page).getByRole("link", { exact: true, name: "the other issue" });
    await expect(link, step("the document draws the dispatch:// link")).toBeVisible();
    // The reader selects the sentence the link stands in, as to comment on it, and then clicks
    // the link: a press and a release on the link, with no drag between them.
    await selectEditorText(page, "the other issue for the handover");
    expect(
      await page.evaluate(() => document.getSelection()?.isCollapsed),
      step("the sentence is selected")
    ).toBe(false);
    // R1: a primary click with no modifier key on a link opens it.
    await markDocument(page);
    await link.click();
    await expect(
      page,
      step("one plain click on the link inside the selected text moves the page to the other issue")
    ).toHaveURL(new RegExp(`/issues/${seed.other.key}(/|$)`));
    expect(await reloaded(page), step("the plain click loads no new document")).toBe(false);
  }
});

// SLOW(>=2s): the spec and a past version each opened once on a phone, and one move in place from
// each.
// @phone-only: a tap is a phone's primary click. Chromium sends a tap's click after its mouseup to
// the anchor the tap landed on, even once the page has moved away from it, and the anchor then
// follows its own `href`.
test("k-link-click: on a phone, a tap on a dispatch:// link in the spec the reader edits and in a past version moves the page to the other issue with no new load of the application @phone-only", async ({
  page,
}) => {
  const seed = await seedLinkSpec();
  for (const [view, path] of [
    ["the editable spec", `/issues/${seed.issueKey}/spec`],
    ["the past version", `/issues/${seed.issueKey}/artifacts/spec?v=1`],
  ] as const) {
    const step = (text: string) => `k-link-click: on a phone, in ${view}, ${text}`;
    await page.goto(path);
    const link = page
      .getByRole("textbox", { name: "Document editor" })
      .getByRole("link", { exact: true, name: "the other issue" })
      .first();
    await expect(link, step("the document draws the dispatch:// link")).toBeVisible();
    // Every load of a new document in the page from the tap on: the page's own move to the other
    // issue loads none.
    const loads: string[] = [];
    const onRequest = (request: Request) => {
      if (request.resourceType() === "document" && request.frame() === page.mainFrame()) {
        loads.push(new URL(request.url()).pathname);
      }
    };
    page.on("request", onRequest);
    await markDocument(page);
    await link.tap();
    await expect(page, step("a tap on the link moves the page to the other issue")).toHaveURL(
      new RegExp(`/issues/${seed.other.key}(/|$)`)
    );
    await expect(
      page.getByText(seed.other.title).first(),
      step("the other issue's page shows its title")
    ).toBeVisible();
    page.off("request", onRequest);
    expect(loads, step("the tap loads no new document")).toEqual([]);
    expect(await reloaded(page), step("the page keeps the document it had")).toBe(false);
  }
});

for (const view of ["the past version", "the reading pane", "the comment"] as const) {
  // SLOW(>=2s): the view opened twice, a new page loaded and two moves to the other issue.
  // @firefox: the click each engine sends on a link (k-link-click's first row says why).
  test(`k-link-click: in ${view}, a plain click opens each link @firefox`, async ({ page }) => {
    const seed = await seedLinkSpec();
    await clickEachLink(page, seed, view);
  });
}

/** An issue whose spec holds the requirement `r1`, and whose plan holds a task that serves it
 *  (`:::plan_task{#t1 serves="dispatch://KEY/spec#b-r1"}`), at version 2, so its version 1 is a
 *  past version. The task's typed block draws its `serves` in its header, as a link to the spec's
 *  block. */
async function seedPlanTask(): Promise<{ readonly key: string; readonly reference: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: ":::requirement{#r1}\nThe mirror replays the ledger.\n:::\n",
    title: "Mirror replay",
  });
  const reference = `dispatch://${issue.key}/spec#b-r1`;
  const plan = await createIssueArtifact(issue.key, {
    content: `# Plan\n\n:::plan_task{#t1 serves="${reference}"}\nReplay the ledger into the mirror.\n:::\n`,
    name: "plan.md",
  });
  await editArtifact(
    plan.artifact.id,
    { ops: [{ after: "end", markdown: "A later line.", op: "insert" }] },
    asker
  );
  return { key: issue.key, reference };
}

// SLOW(>=2s): a plan seeded at version 2, and its page and its version 1 each opened and left by
// a click.
// @firefox: the click each engine sends on a link (k-link-click's first row says why).
test("k-link-click: in the plan the reader edits and in its past version, a plain click on a task's serves link moves the page to the spec with no new load of the application @firefox", async ({
  page,
}) => {
  const seed = await seedPlanTask();
  await page.setViewportSize(WIDE);
  const plan = `/issues/${seed.key}/artifacts/plan-md`;
  for (const view of ["the plan the reader edits", "the plan's past version"] as const) {
    const step = `k-link-click: in ${view}, a click on the task's serves link`;
    let scope: Locator;
    if (view === "the plan the reader edits") {
      await page.goto(plan);
      await expect(page.getByRole("status", { name: "connected" })).toHaveText("connected");
      scope = documentEditor(page);
    } else {
      await page.goto(`${plan}?v=1`);
      scope = page
        .getByRole("region", { name: "Document version 1" })
        .getByRole("textbox", { name: "Document editor" });
    }
    // The link the task's header draws for its `serves` (`typed-block-drawing.ts`).
    const serves = scope.locator(
      `header[data-proof-block-summary] a[data-dispatch-href="${seed.reference}"]`
    );
    await expect(serves, `k-link-click: ${view} draws the task's serves link`).toBeVisible();
    await markDocument(page);
    // Bounded at `expectTimeout`, as the row's own assertions are: a link the page draws but that
    // takes no click fails here by name, where the click would wait out the action's timeout.
    await serves.click({ timeout: expectTimeout }).catch((error: unknown) => {
      throw new Error(`${step}: the link took no click within ${expectTimeout} ms`, {
        cause: error,
      });
    });
    await expect(page, `${step} moves the page to the spec`).toHaveURL(
      new RegExp(`/issues/${seed.key}/spec(?:[?#].*)?$`)
    );
    expect(await reloaded(page), `${step} loads no new document`).toBe(false);
  }
});

/** A person's press on `link` - a click, a Ctrl+click or a middle click - with the button held
 *  down for 150 ms, as a hand holds it, where Playwright's own `click` releases it at once, at the
 *  middle of the link's first line box, since a link the card wraps over two lines has words that
 *  are not its own inside its bounding box. Answers the address of the page it opened in
 *  `context` once that page has left `about:blank`, or `none` when no page opened within 5 s; a
 *  page it opened is closed (`withNewPage`). */
async function heldPressOpens(
  context: BrowserContext,
  page: Page,
  link: Locator,
  how: "click" | "Ctrl+click" | "middle click"
): Promise<string> {
  await link.scrollIntoViewIfNeeded();
  // Read once the link is scrolled into view, where its line boxes are settled: the press goes to
  // the middle of its first line box, which is the link's own words wherever the card wraps it.
  const point = await link.evaluate((anchor) => {
    const line = anchor.getClientRects()[0];
    return line === undefined
      ? undefined
      : { x: line.left + line.width / 2, y: line.top + line.height / 2 };
  });
  if (point === undefined) throw new Error(`k-link-click: the link to ${how} is drawn`);
  return withNewPage(
    page,
    async () => {
      const opened = context.waitForEvent("page", { timeout: 5000 }).catch(() => undefined);
      await page.mouse.move(point.x, point.y);
      if (how === "Ctrl+click") await page.keyboard.down("ControlOrMeta");
      const button = how === "middle click" ? "middle" : "left";
      await page.mouse.down({ button });
      // Held 150 ms, as a hand holds a button down, where Playwright's own click releases at once.
      await page.waitForTimeout(150);
      await page.mouse.up({ button });
      if (how === "Ctrl+click") await page.keyboard.up("ControlOrMeta");
      return opened;
    },
    async (tab) => {
      if (tab === undefined) return "none";
      await tab.waitForURL((url) => url.href !== "about:blank", { timeout: 5000 }).catch(() => {});
      return tab.url();
    }
  );
}

/** Runs `press` on `link` and holds the element it pressed to still being in the page once the
 *  press has settled: connected, drawn, and still the one element `link` finds, not a link of
 *  the same name drawn again in its place; softly, so one run names every press that goes wrong.
 *  Answers what `press` answers. */
async function keepsPressedLink<T>(
  link: Locator,
  step: string,
  press: () => Promise<T>
): Promise<T> {
  // Held from before the press, the card at rest and folded, so the read after the press tells
  // this element from a link of the same name drawn again in its place.
  const pressed = await link.elementHandle();
  const answer = await press();
  expect
    .soft(
      // Read after the press has settled: `press` waited out the new page it may open, so a
      // fold or a draw again that the press set off has happened by now.
      await link.evaluateAll(
        (found, held) => ({
          connected: held.isConnected,
          same: found.length === 1 && found[0] === held,
          visible: held.checkVisibility(),
        }),
        pressed
      ),
      `${step} leaves the pressed link in the page`
    )
    .toEqual({ connected: true, same: true, visible: true });
  await pressed.dispose();
  return answer;
}

// SLOW(>=2s): the Conversation tab or the spec opened for each of ten presses.
// @firefox: the click each engine sends on a link (k-link-click's first row says why).
test("k-link-click: in a folded comment on the Conversation tab and in the spec's margin, a press on a link leaves the comment folded and the link in the page, and a Ctrl+click and a middle click open its dispatch:// link in a new page @firefox", async ({
  context,
  page,
}) => {
  test.setTimeout(240_000);
  const seed = await seedLinkSpec();
  // A web link's own new page is served, though this row reads only where it leaves the card:
  // headless Chromium opens a held Ctrl+click's or middle click's page at another site at
  // random, a plain page's link outside Dispatch included, so whether that page opens is read
  // where Dispatch opens it itself (the rows above).
  await context.route(`${OTHER_SITE}/**`, (route) =>
    route.fulfill({
      body: "<!doctype html><title>Another site</title><p>Another site.</p>",
      contentType: "text/html",
    })
  );
  await page.setViewportSize(WIDE);
  const reference = `dispatch://${seed.other.key}`;
  const margin = await createComment(seed.issueKey, {
    anchor: { artifact: "spec", quote: "names it too" },
    body: `Read [a page elsewhere](${AWAY}), see [the other issue](${reference}) and ${seed.other.key}.`,
  });
  // The folded card of each comment, as its page draws it: the comment's own turn on the
  // Conversation tab, and the margin's card of the comment anchored in the spec. Each card's
  // folded preview is one button that opens the thread, and it keeps its links live
  // (`MarkdownPreview`'s `links="live"`), whose host skips a press under a link: a link pressed
  // there opens the link and leaves the thread as it was, with no caret in its reply box.
  const places = [
    {
      card: () => page.locator(`[data-turn="comment:${seed.commentId}"]`),
      open: async () => {
        await page.goto(`/issues/${seed.issueKey}/conversation`);
      },
      where: "the Conversation tab's folded comment",
    },
    {
      card: () => page.getByTestId(`margin-comment-${margin.id}`),
      open: () => openSpec(page, seed.issueKey, "names it too"),
      where: "the spec margin's folded comment",
    },
  ];
  const presses = [
    ["the web link", "click"],
    ["the web link", "Ctrl+click"],
    ["the web link", "middle click"],
    ["the dispatch:// link", "Ctrl+click"],
    ["the dispatch:// link", "middle click"],
  ] as const;
  for (const { card, open, where } of places) {
    for (const [name, how] of presses) {
      await open();
      // The press waits for the card to draw the bare key's title, the last thing the card
      // draws as it opens, so it never lands on words the card is still drawing.
      await expect(
        card().locator(`a[data-dispatch-ref="${reference}"]`).nth(1),
        `k-link-click: ${where} draws the key's title`
      ).toContainText(seed.other.title);
      const link =
        name === "the web link"
          ? card().getByRole("link", { exact: true, name: "a page elsewhere" })
          : card().locator(`a[data-dispatch-ref="${reference}"]`).first();
      await expect(link, `k-link-click: ${where} draws ${name}`).toBeVisible();
      // The card's own fold: the margin's card itself, or the card inside the turn.
      const folded = card().locator("xpath=descendant-or-self::*[@aria-expanded]").first();
      await expect(folded, `k-link-click: ${where} is folded`).toHaveAttribute(
        "aria-expanded",
        "false"
      );
      const at = page.url();
      const address = await keepsPressedLink(
        link,
        `k-link-click: in ${where}, a ${how} on ${name}`,
        () => heldPressOpens(context, page, link, how)
      );
      // Soft, so one run names every place and press that goes wrong.
      if (name === "the dispatch:// link") {
        expect
          .soft(address, `k-link-click: in ${where}, a ${how} on ${name} opens it in a new page`)
          .toMatch(new RegExp(`^${new URL(`/issues/${seed.other.key}`, at).href}(/|$)`));
      }
      expect
        .soft(page.url(), `k-link-click: in ${where}, a ${how} on ${name} keeps the page`)
        .toBe(at);
      await expect
        .soft(folded, `k-link-click: in ${where}, a ${how} on ${name} leaves the comment folded`)
        .toHaveAttribute("aria-expanded", "false");
      expect
        .soft(
          await page.evaluate(() => document.activeElement?.tagName),
          `k-link-click: in ${where}, a ${how} on ${name} puts no caret in a reply box`
        )
        .not.toBe("TEXTAREA");
    }
  }
});

// SLOW(>=2s): the Conversation tab or the spec opened for each of ten presses, and each of the four presses that move the page waits out 5 s for a new page that never opens.
// @firefox: the click each engine sends on a link (k-link-click's first row says why).
test("k-link-click: in a folded comment on the Conversation tab and in the spec's margin, a click on its web link opens a new page, a click on its dispatch:// link or its key's title moves the page to the other issue, and a Ctrl+click and a middle click on its key's title open it in a new page, the comment staying folded and the pressed link in the page @firefox", async ({
  context,
  page,
}) => {
  test.setTimeout(240_000);
  const seed = await seedLinkSpec();
  await context.route(`${OTHER_SITE}/**`, (route) =>
    route.fulfill({
      body: "<!doctype html><title>Another site</title><p>Another site.</p>",
      contentType: "text/html",
    })
  );
  await page.setViewportSize(WIDE);
  const reference = `dispatch://${seed.other.key}`;
  const margin = await createComment(seed.issueKey, {
    anchor: { artifact: "spec", quote: "names it too" },
    body: `Read [a page elsewhere](${AWAY}), see [the other issue](${reference}) and ${seed.other.key}.`,
  });
  // The folded card of each comment, as the row above opens it.
  const places = [
    {
      card: () => page.locator(`[data-turn="comment:${seed.commentId}"]`),
      open: async () => {
        await page.goto(`/issues/${seed.issueKey}/conversation`);
      },
      where: "the Conversation tab's folded comment",
    },
    {
      card: () => page.getByTestId(`margin-comment-${margin.id}`),
      open: () => openSpec(page, seed.issueKey, "names it too"),
      where: "the spec margin's folded comment",
    },
  ];
  // A click on the web link, and a Ctrl+click and a middle click on the key's title, open a new
  // page and keep the Dispatch page; a click on the `dispatch://` link or on the key's title
  // moves the Dispatch page itself.
  const presses = [
    ["the web link", "click"],
    ["the dispatch:// link", "click"],
    ["the key's title", "click"],
    ["the key's title", "Ctrl+click"],
    ["the key's title", "middle click"],
  ] as const;
  for (const { card, open, where } of places) {
    for (const [name, how] of presses) {
      const step = `k-link-click: in ${where}, a ${how} on ${name}`;
      await open();
      // The bare key's title is the last thing the card draws as it opens (the row above).
      const title = card().locator(`a[data-dispatch-ref="${reference}"]`).nth(1);
      await expect(title, `k-link-click: ${where} draws the key's title`).toContainText(
        seed.other.title
      );
      const link =
        name === "the web link"
          ? card().getByRole("link", { exact: true, name: "a page elsewhere" })
          : name === "the dispatch:// link"
            ? card().locator(`a[data-dispatch-ref="${reference}"]`).first()
            : title;
      await expect(link, `k-link-click: ${where} draws ${name}`).toBeVisible();
      const folded = card().locator("xpath=descendant-or-self::*[@aria-expanded]").first();
      await expect(folded, `k-link-click: ${where} is folded`).toHaveAttribute(
        "aria-expanded",
        "false"
      );
      const at = page.url();

      if (how === "click" && name !== "the web link") {
        await markDocument(page);
        expect
          .soft(await heldPressOpens(context, page, link, how), `${step} opens no new page`)
          .toBe("none");
        await expectInApp(page, seed, step);
        continue;
      }

      const address = await keepsPressedLink(link, step, () =>
        heldPressOpens(context, page, link, how)
      );
      // Soft, so one run names every place and press that goes wrong.
      if (name === "the web link") {
        expect.soft(address, `${step} opens a new page at ${AWAY}`).toBe(AWAY);
      } else {
        expect
          .soft(address, `${step} opens a new page at the other issue`)
          .toMatch(new RegExp(`^${new URL(`/issues/${seed.other.key}`, at).href}(/|$)`));
      }
      expect.soft(page.url(), `${step} keeps the Dispatch page at its address`).toBe(at);
      await expect
        .soft(folded, `${step} leaves the comment folded`)
        .toHaveAttribute("aria-expanded", "false");
      expect
        .soft(
          // Read after the press has settled: the held press and the reads above it waited out
          // its new page, so a caret the press put in a reply box would be there by now.
          await page.evaluate(() => {
            const active = document.activeElement;
            return active instanceof HTMLTextAreaElement ||
              active?.closest("[data-thread-reply]") != null
              ? `${active?.tagName} ${active?.getAttribute("aria-label") ?? ""}`
              : "none";
          }),
          `${step} puts no caret in a reply box`
        )
        .toBe("none");
    }
  }
});

// SLOW(>=2s): the spec opened three times and a new page loaded.
// @firefox: the click each engine sends on a link (k-link-click's first row says why).
test("k-link-keys: in the spec the reader edits, Alt+Enter with the caret in a link or beside a key's title opens it and changes no text @firefox", async ({
  page,
}) => {
  const seed = await seedLinkSpec();
  await openLinkView(page, seed, "the editable spec");
  const before = await storedSpec(seed);

  // The caret into the web link's words, "Read a p|age elsewhere", by the arrow keys.
  await placeCaret(page, "before", "Read");
  for (let step = 0; step < 8; step += 1) await page.keyboard.press("ArrowRight");
  expect(
    await openedAddress(
      page,
      () => page.keyboard.press("Alt+Enter"),
      "k-link-keys: Alt+Enter in the web link"
    ),
    `k-link-keys: Alt+Enter in the web link opens a new page at ${AWAY}`
  ).toBe(AWAY);
  expect(await storedSpec(seed), "k-link-keys: Alt+Enter in the web link changes no text").toBe(
    before
  );

  // "See th|e other issue".
  await placeCaret(page, "before", "See");
  for (let step = 0; step < 6; step += 1) await page.keyboard.press("ArrowRight");
  await markDocument(page);
  await page.keyboard.press("Alt+Enter");
  await expectInApp(page, seed, "k-link-keys: Alt+Enter in the dispatch:// link");
  expect(
    await storedSpec(seed),
    "k-link-keys: Alt+Enter in the dispatch:// link changes no text"
  ).toBe(before);

  // The caret beside the key's title: one step on from the space before the key.
  await openLinkView(page, seed, "the editable spec");
  await placeCaret(page, "after", "The bare key");
  await page.keyboard.press("ArrowRight");
  await markDocument(page);
  await page.keyboard.press("Alt+Enter");
  await expectInApp(page, seed, "k-link-keys: Alt+Enter beside the key's title");
  expect(
    await storedSpec(seed),
    "k-link-keys: Alt+Enter beside the key's title changes no text"
  ).toBe(before);
});

/** One stop of the focus inside a scope: the text a reader sees on it, whether it draws anything
 *  at all (a box of some size holding visible text), its name, its `aria-label` else that text,
 *  and the text of the paragraph or list item it stands in, which tells two stops of the same
 *  text apart. */
interface TabStop {
  readonly text: string;
  readonly drawn: boolean;
  readonly name: string;
  readonly block: string;
}

/** Presses Tab from where the focus is until it has entered `scope` and left it again, at most
 *  `limit` times, and answers each stop inside it, in order. */
async function tabWalk(page: Page, scope: Locator, limit: number): Promise<TabStop[]> {
  const stops: TabStop[] = [];
  for (let press = 0; press < limit; press += 1) {
    await page.keyboard.press("Tab");
    // A Tab moves the focus inside its own key press, so this reads where it settled.
    const stop = await scope.evaluate((root): TabStop | null => {
      const active = document.activeElement;
      if (!(active instanceof HTMLElement) || !root.contains(active)) return null;
      const box = active.getBoundingClientRect();
      const text = active.innerText.trim();
      return {
        block: active.closest("p, li")?.textContent?.trim() ?? "",
        drawn: box.width > 0 && box.height > 0 && text !== "",
        name: active.getAttribute("aria-label") ?? text,
        text,
      };
    });
    if (stop !== null) {
      stops.push(stop);
    } else if (stops.length > 0) {
      break;
    }
  }
  return stops;
}

// SLOW(>=2s): two views walked by Tab, and each of their three links opened by Enter.
// @firefox: the click each engine sends for Enter on a focused link (k-link-click's first row
// says why).
test("k-link-keys: in the past version and the reading pane, Tab reaches each link, the key's title included, and Enter opens it @firefox", async ({
  page,
}) => {
  const seed = await seedLinkSpec();
  const titleText = `${seed.other.key} ${seed.other.title}`;
  for (const view of ["the past version", "the reading pane"] as const) {
    let links = await openLinkView(page, seed, view);
    if (view === "the past version") {
      // The Version picker is named by its list and then the version it shows.
      await page.getByRole("button", { name: /^Version(?: |$)/ }).focus();
    } else {
      await page.keyboard.press("l");
      await expect(paneScroller(page), "k-link-keys: l puts the focus in the pane").toBeFocused();
    }
    const stops = await tabWalk(page, links.scope, 60);
    // The title drawn over the address link reads as the bare key's does, so the bare key's is
    // the stop of that text in the paragraph the bare key stands in.
    const order = [
      stops.findIndex((stop) => stop.text === "a page elsewhere"),
      stops.findIndex((stop) => stop.text === "the other issue"),
      stops.findIndex((stop) => stop.text === titleText && stop.block.startsWith("The bare key")),
    ];
    expect(
      order.every((index, at) => index !== -1 && (at === 0 || index > (order[at - 1] ?? -1))),
      `k-link-keys: in ${view}, Tab reaches the web link, the dispatch:// link and the bare key's title in turn: ${JSON.stringify(stops.map((stop) => `${stop.text} (in "${stop.block}")`))}`
    ).toBe(true);
    expect(
      stops.filter((stop) => !stop.drawn),
      `k-link-keys: in ${view}, Tab stops on nothing that draws nothing`
    ).toEqual([]);

    await links.away.focus();
    expect(
      await openedAddress(
        page,
        () => page.keyboard.press("Enter"),
        `k-link-keys: in ${view}, Enter on the web link`
      ),
      `k-link-keys: in ${view}, Enter on the web link opens a new page at ${AWAY}`
    ).toBe(AWAY);
    await links.issue.focus();
    await markDocument(page);
    await page.keyboard.press("Enter");
    await expectInApp(page, seed, `k-link-keys: in ${view}, Enter on the dispatch:// link`);
    links = await openLinkView(page, seed, view);
    await links.key.focus();
    await markDocument(page);
    await page.keyboard.press("Enter");
    await expectInApp(page, seed, `k-link-keys: in ${view}, Enter on the key's title`);
  }
});

/** The issue the copied links name, and the question, comment and spec that hold them. */
interface CopySeed {
  readonly otherKey: string;
  readonly hostKey: string;
  readonly askId: string;
  readonly commentId: string;
}

/** The seeds of k-link-copy: a question whose text holds a link to another site, a `dispatch://`
 *  link to another issue, that issue's bare key and a web link to that issue's Dispatch page; a
 *  comment anchored in the spec of the question's issue that holds the same four; and that spec
 *  at version 2, whose version 1 holds the same four. */
async function seedLinkCopy(): Promise<CopySeed> {
  await createProject({ key: "CORE", name: "Core" });
  const other = await createIssue({ project: "CORE", title: "Ledger handover" });
  const fourLinks = (page: string) =>
    `Read [a page elsewhere](${AWAY}), see [the other issue](dispatch://${other.key}), the bare key ${other.key} and [the other issue's page](${page}).`;
  const host = await createIssue({
    project: "CORE",
    spec: `# Mirror replay\n\n${fourLinks(`${baseUrl}/issues/${other.key}`)}\n\nThe anchor of a review comment.\n`,
    title: "Mirror replay",
  });
  const comment = await createComment(host.key, {
    anchor: { artifact: "spec", quote: "The anchor of a review comment." },
    body: fourLinks(`${baseUrl}/issues/${other.key}`),
  });
  await editArtifact(
    host.primary_artifact_id,
    { ops: [{ after: "end", markdown: "A later line.", op: "insert" }] },
    asker
  );
  const ask = await createAsk(
    host.key,
    { question: `Which page? ${fourLinks(`${baseUrl}/issues/${other.key}`)}` },
    asker
  );
  return { askId: ask.id, commentId: comment.id, hostKey: host.key, otherKey: other.key };
}

/** Presses `y` with the focus on `link`, waits until the page has put something on the clipboard
 *  (`recordClipboard`), and answers the last thing it put there. */
async function copiedFrom(
  page: Page,
  link: Locator,
  copied: () => Promise<string[]>
): Promise<string | undefined> {
  await link.focus();
  await expect(link).toBeFocused();
  const count = (await copied()).length;
  await page.keyboard.press("y");
  await expect
    .poll(async () => (await copied()).length, { message: "k-link-copy: y copies something" })
    .toBeGreaterThan(count);
  return (await copied()).at(-1);
}

/** The four links of a rendered body: the web link, then the three a body draws as reference
 *  links to the other issue - the `dispatch://` link, the bare key and the link to its page. */
function bodyLinks(scope: Locator, seed: CopySeed): Array<[Locator, string, string]> {
  const reference = `dispatch://${seed.otherKey}`;
  const references = scope.locator(`a[data-dispatch-ref="${reference}"]`);
  return [
    [scope.getByRole("link", { exact: true, name: "a page elsewhere" }), "the web link", AWAY],
    [references.nth(0), "the dispatch:// link", reference],
    [references.nth(1), "the bare key's link", reference],
    [references.nth(2), "the link to the other issue's page", reference],
  ];
}

// SLOW(>=2s): the Inbox's reading pane walked by Tab, then the drawer opened over a project page.
test("k-link-copy: y on a focused link copies its address in the Inbox's reading pane and in the drawer, before the Inbox's own y", async ({
  page,
}) => {
  const copied = await recordClipboard(page);
  const seed = await seedLinkCopy();
  await page.setViewportSize(WIDE);
  await page.goto("/");
  await selectRow(page, seed.askId);
  await page.keyboard.press("l");
  await expect(paneScroller(page), "k-link-copy: l puts the focus in the pane").toBeFocused();
  for (const [link, what, address] of bodyLinks(paneScroller(page), seed)) {
    await tabTo(page, link, `k-link-copy: in the reading pane, Tab reaches ${what}`);
    expect(
      await copiedFrom(page, link, copied),
      `k-link-copy: in the reading pane, y on ${what} copies ${address}`
    ).toBe(address);
  }

  await openDrawer(page, "k-link-copy: i opens the drawer");
  expect(
    await copiedFrom(
      page,
      drawer(page).getByRole("link", { exact: true, name: "a page elsewhere" }),
      copied
    ),
    "k-link-copy: in the drawer, y on the question's web link copies its URL"
  ).toBe(AWAY);
});

// SLOW(>=2s): the Conversation tab, version 1 of the spec and the margin's sheet, each opened.
test("k-link-copy: y on a focused link copies its address on the Conversation tab, in a past version and in the margin's sheet", async ({
  page,
}) => {
  const copied = await recordClipboard(page);
  const seed = await seedLinkCopy();
  const reference = `dispatch://${seed.otherKey}`;
  await page.setViewportSize(WIDE);

  await page.goto(`/issues/${seed.hostKey}/conversation`);
  const turn = page.locator(`[data-turn="comment:${seed.commentId}"]`);
  expect(
    await copiedFrom(
      page,
      turn.getByRole("link", { exact: true, name: "a page elsewhere" }),
      copied
    ),
    "k-link-copy: on the Conversation tab, y on the comment's web link copies its URL, not the comment's reference"
  ).toBe(AWAY);

  await page.goto(`/issues/${seed.hostKey}/artifacts/spec?v=1`);
  const version = page
    .getByRole("region", { name: "Document version 1" })
    .getByRole("textbox", { name: "Document editor" });
  const title = version.locator(`a.dispatch-reference[data-dispatch-href="${reference}"]`);
  await expect(title, "k-link-copy: version 1 draws the bare key's title").toBeVisible();
  for (const [link, what, address] of [
    [version.getByRole("link", { exact: true, name: "a page elsewhere" }), "the web link", AWAY],
    [
      version.getByRole("link", { exact: true, name: "the other issue" }),
      "the dispatch:// link",
      reference,
    ],
    [title, "the bare key's title", reference],
    [
      version.getByRole("link", { exact: true, name: "the other issue's page" }),
      "the link to the other issue's page",
      reference,
    ],
  ] as const) {
    expect(
      await copiedFrom(page, link, copied),
      `k-link-copy: in version 1, y on ${what} copies ${address}`
    ).toBe(address);
  }

  // The margin is a sheet below 1280 px, opened with its own control.
  await page.setViewportSize({ height: 768, width: 1024 });
  await page.goto(`/issues/${seed.hostKey}/spec`);
  await page.getByRole("button", { name: /Open review panel/ }).click();
  const sheet = page.getByTestId("margin-sheet");
  await expect(sheet, "k-link-copy: the margin's sheet opens").toHaveAttribute(
    "data-expanded",
    "true"
  );
  const card = sheet.locator(`[data-margin-item="${seed.commentId}"]`);
  expect(
    await copiedFrom(
      page,
      card.getByRole("link", { exact: true, name: "a page elsewhere" }),
      copied
    ),
    "k-link-copy: in the margin's sheet, y on the comment's web link copies its URL"
  ).toBe(AWAY);
});

// ---------------------------------------------------------------------------
// k-list-and-document (R3): the open document's own keys, pressed from the list. Space and
// Shift+Space page it, `g g` and Shift+G take it to its ends, and the selection and the focus
// stay where they are; on a control that Space presses itself the key is the control's.
// ---------------------------------------------------------------------------

/** Where the reading column stands, how tall it is, and how tall the document drawn in it is. */
interface ColumnAt {
  readonly height: number;
  readonly scrollHeight: number;
  readonly top: number;
}

function columnAt(page: Page): Promise<ColumnAt> {
  return paneScroller(page).evaluate((column) => ({
    height: column.clientHeight,
    scrollHeight: column.scrollHeight,
    top: column.scrollTop,
  }));
}

/** What the document's keys leave alone, in words: the row the column shows, and the element
 *  that holds the focus - by its row, as the page body, or by its tag and name. */
function handOf(page: Page): Promise<string> {
  return page.evaluate(() => {
    const selected = document
      .querySelector('[data-inbox-selected="true"]')
      ?.getAttribute("data-inbox-row");
    const active = document.activeElement;
    const focus =
      active === null || active === document.body
        ? "the body"
        : (active.getAttribute("data-inbox-row") ??
          `${active.tagName} ${active.getAttribute("aria-label") ?? ""}`);
    return `${selected ?? "nothing"} selected, the focus on ${focus}`;
  });
}

/** Presses `keys` one after the other and reads that the column moved as `moved` says of where
 *  it stood and where it stands, and that the selection and the focus are what they were. */
async function pressInDocument(
  page: Page,
  keys: readonly string[],
  step: string,
  moved: (before: ColumnAt, after: ColumnAt) => boolean
): Promise<void> {
  const before = await columnAt(page);
  const hand = await handOf(page);
  for (const key of keys) {
    await page.keyboard.press(key);
  }
  await expect
    .poll(async () => moved(before, await columnAt(page)), {
      message: `k-list-and-document: ${step}`,
    })
    .toBe(true);
  expect(
    await handOf(page),
    `k-list-and-document: ${step}, and the selection and the focus stay where they were`
  ).toBe(hand);
}

/** The tall approval requests, each of a spec at least four times the column's height, and a
 *  question its asker reworded, whose card draws the `<summary>` of its earlier wording: forty
 *  paragraphs of its question stand above that summary, past the column's height, and its eight
 *  options (the most a question takes) below it, so the column can stand away from its top at
 *  the summary with room to scroll on down. */
async function seedListAndDocument(): Promise<{
  readonly reworded: string;
  readonly tall: readonly SeededApproval[];
}> {
  await createProject({ key: "CORE", name: "Core" });
  const tall = await seedTallAsks("CORE");
  const issue = await createIssue({ force: true, project: "CORE", title: "Lease audit" });
  const windows = Array.from(
    { length: 40 },
    (_, index) => `Lease window ${index + 1} closes at noon.`
  ).join("\n\n");
  const ask = await createAsk(
    issue.key,
    {
      options: Array.from({ length: 8 }, (_, index) => ({ label: `Window ${index + 1}` })),
      question: `Which lease does the audit read?\n\n${windows}`,
    },
    asker
  );
  await editAsk(
    ask.id,
    { question: `Which lease window does the audit read?\n\n${windows}` },
    asker
  );
  return { reworded: ask.id, tall };
}

/** Waits for the approval request the column shows to draw its spec whole: the last of its
 *  numbered paragraphs (`seedTallAsks`). */
async function tallDrawn(page: Page, title: string): Promise<void> {
  await expect(
    paneScroller(page).getByText(`${title} note 160:`),
    `k-list-and-document: the column draws the whole of the ${title} spec`
  ).toHaveCount(1);
  const drawn = await columnAt(page);
  expect(
    drawn.scrollHeight,
    `k-list-and-document: the ${title} spec is at least four times the column's height`
  ).toBeGreaterThanOrEqual(4 * drawn.height);
}

// SLOW(>=2s): two tall specs drawn in the column, then a document whose reads are held and
// served, and the placement's bound waited out.
test("k-list-and-document: Space, Shift+Space, g g and Shift+G scroll the open document from the list, and leave the selection, the focus and a control's own Space alone", async ({
  page,
}) => {
  const copied = await recordClipboard(page);
  const seed = await seedListAndDocument();
  const [first, second] = seed.tall;
  if (first === undefined || second === undefined) {
    throw new Error("seedTallAsks seeded fewer than two requests");
  }
  const pagedDown = (before: ColumnAt, after: ColumnAt) =>
    after.top - before.top >= before.height / 2 && after.top - before.top <= before.height;
  const pagedUp = (before: ColumnAt, after: ColumnAt) =>
    before.top - after.top >= before.height / 2 && before.top - after.top <= before.height;
  await page.setViewportSize(WIDE);
  await page.goto("/");

  // A click on the first row selects it and leaves the focus on it.
  await selectRow(page, first.askId);
  const firstRow = inboxRow(page, first.askId);
  await expect(
    firstRow,
    "k-list-and-document: the click leaves the focus on the row"
  ).toBeFocused();
  await tallDrawn(page, "Ledger handover");
  await pressInDocument(
    page,
    ["Space"],
    "Space scrolls the column down by half to all of its height",
    pagedDown
  );
  await pressInDocument(
    page,
    ["Shift+Space"],
    "Shift+Space scrolls it back up by half to all of its height",
    pagedUp
  );
  await pressInDocument(
    page,
    ["Shift+G"],
    "Shift+G puts the column at its end",
    (_before, after) => Math.abs(after.top + after.height - after.scrollHeight) <= 1
  );
  await pressInDocument(
    page,
    ["g", "g"],
    "g then g puts the column at its top",
    (_before, after) => after.top === 0
  );

  // With the focus on the page body, as on the row.
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  expect(await onBody(page), "k-list-and-document: the row let go, so the body has the focus").toBe(
    true
  );
  await pressInDocument(
    page,
    ["Space"],
    "with the focus on the body, Space scrolls the column down by half to all of its height",
    pagedDown
  );

  await page.keyboard.press("j");
  await expect(selectedRow(page), "k-list-and-document: j selects the second ask").toHaveAttribute(
    "data-inbox-row",
    second.askId
  );
  await tallDrawn(page, "Mirror replay");

  // On the pane head's Copy reference, Space presses the button and scrolls nothing.
  const copy = paneHead(page).getByRole("button", { name: /^Copy reference/ });
  await copy.focus();
  const copiedBefore = (await copied()).length;
  const beforeCopy = await columnAt(page);
  await page.keyboard.press("Space");
  await expect
    .poll(async () => (await copied()).length, {
      message: "k-list-and-document: Space on the pane head's Copy reference presses it",
    })
    .toBe(copiedBefore + 1);
  expect((await copied()).at(-1), "k-list-and-document: it copies the second ask").toContain(
    second.askId
  );
  expect(
    (await columnAt(page)).top,
    "k-list-and-document: Space on Copy reference moves no scrollTop"
  ).toBe(beforeCopy.top);

  // On a focused `<summary>` of the pane, Space opens or closes it and scrolls nothing. The column
  // is put where the summary stands at its foot: the question above it runs past the column's
  // height and its options below it (`seedListAndDocument`), so the column stands away from its
  // top with room to scroll on down, and a Space the page took as a scroll would move it.
  await selectRow(page, seed.reworded);
  const summary = inboxPane(page).locator("summary", { hasText: "Show 1 previous version" });
  await expect(summary, "k-list-and-document: the reworded question draws a summary").toBeVisible();
  await summary.focus();
  await summary.evaluate((node) => {
    const column = node.closest('[data-testid="inbox-pane-scroller"]');
    if (column === null) throw new Error("the summary is not in the reading column");
    column.scrollTop += node.getBoundingClientRect().bottom - column.getBoundingClientRect().bottom;
  });
  const opened = () =>
    summary.evaluate((node) => (node.parentElement as HTMLDetailsElement | null)?.open === true);
  const wasOpen = await opened();
  const beforeSummary = await columnAt(page);
  expect(
    beforeSummary.top,
    "k-list-and-document: at the summary the column stands away from its top"
  ).toBeGreaterThan(0);
  expect(
    beforeSummary.top + beforeSummary.height,
    "k-list-and-document: at the summary the column has room to scroll on down"
  ).toBeLessThan(beforeSummary.scrollHeight - 1);
  await page.keyboard.press("Space");
  await expect
    .poll(opened, {
      message: "k-list-and-document: Space on a focused summary of the pane opens or closes it",
    })
    .toBe(!wasOpen);
  expect(
    (await columnAt(page)).top,
    "k-list-and-document: Space on the summary moves no scrollTop"
  ).toBe(beforeSummary.top);

  // An ask whose automatic placement is still running: its document's reads are held, so the
  // column places the question's section, far down the document, as soon as they return.
  const placed = await seedQuestionColumn("heading");
  const release = await holdReads(page, [
    `**/api/v1/artifacts/${placed.artifactId}`,
    `**/api/v1/artifacts/${placed.artifactId}/text`,
  ]);
  await page.goto(inboxAt(placed.item));
  await expect(selectedRow(page), "k-list-and-document: the held ask is selected").toHaveAttribute(
    "data-inbox-row",
    placed.item
  );
  await expect(paneScroller(page), "k-list-and-document: its column is drawn").toBeVisible();
  await page.keyboard.press("Space");
  const spaced = (await columnAt(page)).top;
  await release();
  await expect(
    paneScroller(page).getByText(DOCUMENT_FOOT),
    "k-list-and-document: the held document is drawn once its reads return"
  ).toHaveCount(1);
  await waitOutPlacement(page);
  expect(
    Math.abs((await restingColumn(page)).top - spaced),
    "k-list-and-document: Space pressed while the placement runs leaves the column where the Space put it after the reads return"
  ).toBeLessThanOrEqual(1);
  expect(
    await headingOffset(page, SECTION_HEADING),
    "k-list-and-document: the section the placement would have opened at is still below the column's top"
  ).toBeGreaterThan(8);
});

// ---------------------------------------------------------------------------
// k-other-key (R17): the digit after a question's last real option is Other's, each option row of
// the item in hand shows its digit, and a pick of Other puts the caret in the answer box on every
// surface that draws a question.
// ---------------------------------------------------------------------------

/** The two options of the questions below that need no more than two. */
const SHIP_OR_HOLD = [{ label: "Ship" }, { label: "Hold" }];

/** The decision block of k-other-key's spec. */
const LEASE_BLOCK = "lease-window";

/** A decision block `id` asking `question` with `options`, as a spec writes one. */
function decisionBlock(id: string, question: string, options: readonly string[]): string {
  return [
    `:::ask{#${id} urgency="med" multiple="false" state="open"}`,
    question,
    "",
    ...options.map((option) => `- ${option}`),
    ":::",
  ].join("\n");
}

/** A spec holding a decision block with two options, and the line an ask is anchored to. */
const DECISION_SPEC = [
  "# Lease handover",
  "",
  "The mirror hands the lease back when the replay is done.",
  "",
  decisionBlock(LEASE_BLOCK, "Which window does the lease cover?", [
    "The last hour",
    "The last day",
  ]),
  "",
].join("\n");

/** The decision block of nine options. The server takes at most eight options on an ask it is
 *  sent (`maxAskOptions`, `packages/envoy/internal/dispatch/api/asks.go`), so the question of
 *  nine is one a spec writes, which the server indexes into an ask. */
const NINE_BLOCK = "replay-window";

interface OtherSeed {
  readonly answeredId: string;
  readonly anchoredId: string;
  readonly decisionIssueKey: string;
  readonly nineId: string;
  readonly threeId: string;
}

/** A question of three options and one of nine, both waiting on alice; a question alice
 *  answered with its first option; and an issue whose spec holds a decision block with two
 *  options and whose conversation holds an ask anchored to that spec, so its margin draws it.
 *  Each issue is made past the server's duplicate check (`force`). */
async function seedOtherKey(): Promise<OtherSeed> {
  await createProject({ key: "CORE", name: "Core" });
  const threeIssue = await createIssue({ force: true, project: "CORE", title: "Ledger rollout" });
  const three = await createAsk(
    threeIssue.key,
    {
      options: [{ label: "Ship" }, { label: "Hold" }, { label: "Drop" }],
      question: "Which way does the ledger roll out?",
    },
    asker
  );
  const nineIssue = await createIssue({
    force: true,
    project: "CORE",
    spec: `# Mirror replay\n\n${decisionBlock(
      NINE_BLOCK,
      "Which window does the mirror replay?",
      Array.from({ length: 9 }, (_, index) => `Window ${index + 1}`)
    )}\n`,
    title: "Mirror replay",
  });
  const answeredIssue = await createIssue({ force: true, project: "CORE", title: "Clock audit" });
  const answered = await createAsk(
    answeredIssue.key,
    { options: SHIP_OR_HOLD, question: "Ship the clock audit?" },
    asker
  );
  await answerAsk(answered.id, { expected_edited_at: null, selected: ["Ship"] });
  const decided = await createIssue({
    force: true,
    project: "CORE",
    spec: DECISION_SPEC,
    title: "Lease handover",
  });
  const anchored = await createAsk(
    decided.key,
    {
      anchor: { artifact: "spec", quote: "hands the lease back" },
      options: SHIP_OR_HOLD,
      question: "Hand the lease back at once?",
    },
    asker
  );
  return {
    answeredId: answered.id,
    anchoredId: anchored.id,
    decisionIssueKey: decided.key,
    nineId: (await indexedBlockAsk(nineIssue.key, NINE_BLOCK)).id,
    threeId: three.id,
  };
}

/** Clicks the Other of the question drawn in `scope` and reads that the caret is in its answer
 *  box, as the step `step` says. */
async function clickOther(scope: Locator, step: string): Promise<void> {
  await scope.getByRole("radio", { name: "Other" }).click();
  await expect(scope.locator("[data-ask-answer]"), `k-other-key: ${step}`).toBeFocused();
}

// SLOW(>=2s): two answers in the reading pane, then the reading view at 1024x768, the Spec tab
// with its decision block and margin, the Conversation tab and the Answers page, each opened.
test("k-other-key: the digit after a question's last option picks Other and puts the caret in its answer box, and a click on Other does on every surface", async ({
  page,
}) => {
  const seed = await seedOtherKey();
  await page.setViewportSize(WIDE);
  await page.goto("/");
  const pane = inboxPane(page);

  // The question of three options, from its row: each option row shows its digit, Other's 4
  // included, and 4 is Other.
  await selectRow(page, seed.threeId);
  const three = pane.getByTestId(`ask-${seed.threeId}`);
  await expect(three.getByRole("radio", { name: "Other" })).toBeVisible();
  await expect(
    three.locator("label:has(input[type=radio]) kbd"),
    "k-other-key: each of the first question's four option rows, Other's included, shows its digit, 1 to 4"
  ).toHaveText(["1", "2", "3", "4"]);
  const answers = answerWrites(page, seed.threeId);
  await page.keyboard.press("4");
  await expect(
    three.getByRole("radio", { name: "Other" }),
    "k-other-key: 4 checks Other on the question with three options"
  ).toBeChecked();
  await expect(
    three.locator("[data-ask-answer]"),
    "k-other-key: after 4 the caret is in the answer box"
  ).toBeFocused();
  await page.keyboard.type("my own words");
  await page.keyboard.press("Control+Enter");
  await expect
    .poll(() => writesSent(answers), { message: "k-other-key: Ctrl+Enter sends one answer" })
    .toBe(1);
  expect(
    firstAttempts(answers)[0]?.input,
    "k-other-key: the answer is the words alone, with no option"
  ).toMatchObject({ selected: [], text: "my own words" });

  // The question of nine options, a decision block the column draws in its document: Other is
  // the tenth, `0`, and its pick opens the block card's note box with the caret in it.
  await selectRow(page, seed.nineId);
  const nine = pane.getByTestId(`ask-${seed.nineId}`);
  await expect(nine.getByRole("radio", { name: "Other" })).toBeVisible();
  await page.keyboard.press("0");
  await expect(
    nine.getByRole("radio", { name: "Other" }),
    "k-other-key: 0 checks Other on the question with nine options"
  ).toBeChecked();
  await expect(
    nine.locator("[data-ask-answer]"),
    "k-other-key: after 0 the caret is in the answer box"
  ).toBeFocused();

  // A click on Other, on every surface that draws a question: at 1024x768 the question's card is
  // in the reading view its short row opens.
  await page.setViewportSize(NARROW);
  await page.goto("/");
  await selectRow(page, seed.nineId);
  await clickOther(
    inboxPane(page).getByTestId(`ask-${seed.nineId}`),
    "in the reading view at 1024x768, a click on Other puts the caret in the answer box"
  );

  await page.setViewportSize(WIDE);
  await openSpec(page, seed.decisionIssueKey, "hands the lease back");
  await clickOther(
    documentEditor(page).locator(`[data-dispatch-ask-block="${LEASE_BLOCK}"]`),
    "on the decision block of the Spec tab, a click on Other puts the caret in the answer box"
  );
  await clickOther(
    page.locator(`[data-margin-item="${seed.anchoredId}"]`),
    "on that ask in the margin of the Spec tab, a click on Other puts the caret in the answer box"
  );

  await page.goto(`/issues/${seed.decisionIssueKey}/conversation`);
  await clickOther(
    page.locator(`[data-turn="ask:${seed.anchoredId}"]`),
    "on the ask of the Conversation tab, a click on Other puts the caret in the answer box"
  );

  // The Answers page, where a change of an answer given with a real option begins.
  await page.goto("/answers");
  await page
    .locator(`[data-answer-row="${seed.answeredId}"]`)
    .first()
    .getByRole("button", { name: "Change answer" })
    .click();
  const changing = page.locator(`[data-answer-card="${seed.answeredId}"]`);
  await expect(changing.getByRole("radio", { name: "Ship" })).toBeChecked();
  await clickOther(
    changing,
    "on the Answers page, a click on Other where a change of the answer begins puts the caret in the answer box"
  );
});

// ---------------------------------------------------------------------------
// k-answer-box (R18): Enter puts the caret in the answer box of the item in hand from its row,
// the page body, the column after `l` and an option of its card, and `r` in the reply box of the
// thread in hand.
// ---------------------------------------------------------------------------

/** The ask of a decision block alice answered and a session replied under, which the Inbox lists
 *  as a Reply row; its block, in the column, folds the reply box behind "Write a reply". */
async function seedDecisionThread(): Promise<string> {
  const issue = await createIssue({
    force: true,
    project: "CORE",
    spec: DECISION_SPEC,
    title: "Lease handover",
  });
  const askId = (await indexedBlockAsk(issue.key, LEASE_BLOCK)).id;
  await answerAsk(askId, { expected_edited_at: null, selected: ["The last hour"] });
  await createComment(issue.key, { ask_id: askId, body: "The hour holds for now." }, asker);
  return askId;
}

// SLOW(>=2s): four starts in the reading pane, a short row and its reading view at 1024x768,
// then two threads opened in the pane.
test("k-answer-box: Enter puts the caret in the answer box from the row, the body, the column and an option, and r puts it in a thread's reply box", async ({
  page,
}) => {
  const copied = await recordClipboard(page);
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const asks: string[] = [];
  for (const title of ["Ledger rollout", "Mirror replay", "Clock audit"]) {
    const issue = await createIssue({ force: true, project: "CORE", title });
    asks.push(
      (await createAsk(issue.key, { options: SHIP_OR_HOLD, question: `Ship the ${title}?` }, asker))
        .id
    );
  }
  const [firstAsk, secondAsk, thirdAsk] = asks;
  if (firstAsk === undefined || secondAsk === undefined || thirdAsk === undefined) {
    throw new Error("three asks were seeded");
  }
  const threadIssue = await createIssue({ force: true, project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(
    threadIssue.key,
    "How far back does a replay go?",
    "One hour, then it gives up."
  );
  const decisionThread = await seedDecisionThread();
  await page.setViewportSize(WIDE);
  await page.goto("/");
  const pane = inboxPane(page);
  const box = pane.getByTestId(`ask-${firstAsk}`).locator("[data-ask-answer]");
  await selectRow(page, firstAsk);
  const row = inboxRow(page, firstAsk);
  await expect(box).toBeVisible();

  // From the row.
  await row.focus();
  await page.keyboard.press("Enter");
  await expect(
    box,
    "k-answer-box: Enter from the row puts the caret in the answer box"
  ).toBeFocused();
  // From the page body.
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  await page.keyboard.press("Enter");
  await expect(
    box,
    "k-answer-box: Enter from the page body puts the caret in the answer box"
  ).toBeFocused();
  // From the document column, after `l`.
  await row.focus();
  await page.keyboard.press("l");
  await expect(paneScroller(page), "k-answer-box: l puts the focus in the column").toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    box,
    "k-answer-box: Enter from the document column puts the caret in the answer box"
  ).toBeFocused();
  // From an option of the card, after a click on it.
  const ship = pane.getByTestId(`ask-${firstAsk}`).getByRole("radio", { name: "Ship" });
  await ship.click();
  await expect(ship).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    box,
    "k-answer-box: Enter after a click on an option puts the caret in the answer box"
  ).toBeFocused();

  // On the pane head's Copy reference, Enter presses the button and the caret stays out.
  const copy = paneHead(page).getByRole("button", { name: /^Copy reference/ });
  await copy.focus();
  const copiedBefore = (await copied()).length;
  await page.keyboard.press("Enter");
  await expect
    .poll(async () => (await copied()).length, {
      message: "k-answer-box: Enter on the pane head's Copy reference presses it",
    })
    .toBe(copiedBefore + 1);
  await expect(copy, "k-answer-box: the focus stays on Copy reference").toBeFocused();

  // The first question, answered with 1, Enter and Ctrl+Enter: the second question's row has the
  // focus (R8) - it is the item after the first question in the list, or the item before it
  // where the first is the last - and Enter from there puts the caret in its answer box.
  const listed = await page
    .locator("[data-inbox-row]")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
  const answeredAt = listed.indexOf(firstAsk);
  expect(
    listed[answeredAt + 1] ?? listed[answeredAt - 1],
    "k-answer-box: the second question is the item after the first, or the one before it"
  ).toBe(secondAsk);
  const answers = answerWrites(page, firstAsk);
  await row.focus();
  await page.keyboard.press("1");
  await page.keyboard.press("Enter");
  await expect(box, "k-answer-box: 1 then Enter puts the caret in the answer box").toBeFocused();
  await page.keyboard.press("Control+Enter");
  await expect(
    inboxRow(page, secondAsk),
    "k-answer-box: after the answer the second question's row has the focus"
  ).toBeFocused();
  expect(writesSent(answers), "k-answer-box: Ctrl+Enter sends one answer").toBe(1);
  const secondBox = pane.getByTestId(`ask-${secondAsk}`).locator("[data-ask-answer]");
  await expect(secondBox, "k-answer-box: the column shows the second question").toBeVisible();
  await page.keyboard.press("Enter");
  await expect(
    secondBox,
    "k-answer-box: Enter from the second question's row puts the caret in its answer box"
  ).toBeFocused();

  // At 1024x768, Enter on the third question's focused short row opens its reading view with the
  // caret in its answer box; after a click on an option there, Enter puts it back in the box.
  await page.setViewportSize(NARROW);
  await page.goto("/");
  await inboxRow(page, thirdAsk).focus();
  await page.keyboard.press("Enter");
  await expect(
    readingView(page),
    "k-answer-box: at 1024x768, Enter on the third question's focused short row opens its reading view"
  ).toHaveAttribute("data-inbox-reading-row", thirdAsk);
  const third = inboxPane(page).getByTestId(`ask-${thirdAsk}`);
  await expect(
    third.locator("[data-ask-answer]"),
    "k-answer-box: at 1024x768, Enter on the third question's focused short row puts the caret in its answer box"
  ).toBeFocused();
  await third.getByRole("radio", { name: "Ship" }).click();
  await page.keyboard.press("Enter");
  await expect(
    third.locator("[data-ask-answer]"),
    "k-answer-box: in the reading view at 1024x768, Enter after a click on an option of the third question puts the caret in its answer box"
  ).toBeFocused();

  // `r` on the reply thread's row puts the caret in the thread's reply box.
  await page.setViewportSize(WIDE);
  await page.goto("/");
  await selectRow(page, replyRowId(thread));
  await expect(pane.locator("[data-inbox-thread-body] textarea").first()).toBeVisible();
  await page.keyboard.press("r");
  expect(
    await page.evaluate(
      () =>
        document.activeElement instanceof HTMLTextAreaElement &&
        document.activeElement.closest('[data-testid="inbox-pane"] [data-inbox-thread-body]') !==
          null
    ),
    "k-answer-box: r on the reply thread's row puts the caret in the thread's reply box"
  ).toBe(true);

  // On the thread of a decision block, whose block folds the box behind "Write a reply", `r`
  // presses that first.
  await selectRow(page, replyRowId(decisionThread));
  const writeReply = pane.getByRole("button", { name: "Write a reply" });
  await expect(writeReply).toHaveAttribute("aria-expanded", "false");
  await page.keyboard.press("r");
  await expect(
    writeReply,
    "k-answer-box: r opens the decision block's Write a reply"
  ).toHaveAttribute("aria-expanded", "true");
  expect(
    await page.evaluate(
      () =>
        document.activeElement instanceof HTMLTextAreaElement &&
        document.activeElement.closest('[data-testid="inbox-pane"]') !== null
    ),
    "k-answer-box: r on a decision block's thread puts the caret in the reply box it opens"
  ).toBe(true);
});

// ---------------------------------------------------------------------------
// k-new-tab (R20), the Inbox's part: Shift+O opens what `o` opens - the item's owner link, which
// `o` follows - in a new page of the browser, beside the reading pane, in the short-row list at
// 1024x768 and in the drawer, and the first page keeps its address.
// ---------------------------------------------------------------------------

/** Presses Shift+O on `page` and reads that one new page of `context` opened at `address`, and
 *  that `page` kept its own address; the new page is closed (`withNewPage`). */
async function shiftOOpens(
  context: BrowserContext,
  page: Page,
  address: string,
  place: string
): Promise<void> {
  const before = page.url();
  const pages = context.pages().length;
  await withNewPage(
    page,
    () =>
      test.step(`k-new-tab: ${place}, Shift+O opens a new page`, () =>
        newPageAt(context, () => page.keyboard.press("Shift+O"), `k-new-tab: ${place}, Shift+O`)),
    (opened) => {
      expect(opened.url(), `k-new-tab: ${place}, the new page is at the address o opens`).toBe(
        address
      );
      expect(context.pages().length, `k-new-tab: ${place}, Shift+O opens one new page`).toBe(
        pages + 1
      );
      expect(page.url(), `k-new-tab: ${place}, the first page keeps its address`).toBe(before);
    }
  );
}

/** The absolute address of the owner link in `scope`, the link `o` follows. */
function ownerAddress(scope: Locator): Promise<string> {
  return scope
    .locator("[data-inbox-owner]")
    .first()
    .evaluate((link) => (link as HTMLAnchorElement).href);
}

// SLOW(>=2s): the Inbox opened three times, each with a page opened from it.
test("k-new-tab: Shift+O on the Inbox opens the item o opens in a new page, beside the reading pane, in the short-row list at 1024x768 and in the drawer", async ({
  context,
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);
  const runbook = await createProjectDocument(
    "CORE",
    { content: "# Runbook\n\nRestart, reseed, reopen.\n", name: "Runbook" },
    asker
  );
  await createArtifactAsk(
    runbook.artifact.id,
    { question: "Keep the runbook in the repository?" },
    asker
  );

  // Beside the reading pane, `j` selects the second item, and the column's head holds its link.
  const { order } = await openInboxAt(page, "the reading pane", 2);
  await expect(selectedRow(page)).toHaveAttribute("data-inbox-row", order[0] ?? "");
  await page.locator("body").focus();
  await page.keyboard.press("j");
  await expect(selectedRow(page), "k-new-tab: j selects the second item").toHaveAttribute(
    "data-inbox-row",
    order[1] ?? ""
  );
  await expect(inboxPane(page).getByTestId(`ask-${order[1]}`)).toBeVisible();
  await shiftOOpens(context, page, await ownerAddress(paneHead(page)), "beside the reading pane");

  // In the short-row list at 1024x768, `j` focuses the first row, whose owner link `o` follows.
  await openInboxAt(page, "the short-row list", 2);
  await page.locator("body").focus();
  await page.keyboard.press("j");
  const focused = page.locator("[data-inbox-row]:focus");
  await expect(
    focused,
    "k-new-tab: in the short-row list at 1024x768, j focuses a row"
  ).toHaveCount(1);
  await shiftOOpens(
    context,
    page,
    await ownerAddress(focused),
    "in the short-row list at 1024x768"
  );

  // In the drawer over a project page.
  await page.setViewportSize(WIDE);
  await openDrawer(page, "k-new-tab: i opens the drawer");
  await expect(drawer(page).locator("[data-inbox-row]")).toHaveCount(2);
  await page.keyboard.press("j");
  const inDrawer = drawer(page).locator("[data-inbox-row]:focus");
  await expect(inDrawer, "k-new-tab: in the drawer, j focuses a row").toHaveCount(1);
  await shiftOOpens(context, page, await ownerAddress(inDrawer), "in the drawer");
  await expect(drawer(page), "k-new-tab: the drawer stays open").toBeVisible();
});

// ---------------------------------------------------------------------------
// k-new-tab (R20), every other place: the project List, the Board, the Priorities page, a
// priority's page, the Architecture page, the project's Documents list and the command bar.
// ---------------------------------------------------------------------------

/** The absolute address `link` points at: the link `o` follows for the item in hand. */
function hrefOf(link: Locator): Promise<string> {
  return link.evaluate((node) => (node as HTMLAnchorElement).href);
}

/** Shift+O on `page` opens `link`'s address in one new page and leaves `page` where it is
 *  (`shiftOOpens`); `o` then opens that same address in `page` itself, which is what makes it
 *  the address `o` opens. */
async function opensWhereOOpens(
  context: BrowserContext,
  page: Page,
  link: Locator,
  place: string
): Promise<void> {
  const address = await hrefOf(link);
  await shiftOOpens(context, page, address, place);
  await page.keyboard.press("o");
  await expect(page, `k-new-tab: ${place}, o opens the same address in this page`).toHaveURL(
    address
  );
}

/** Takes the focus off whatever holds it, onto the page itself, and reads that it is there. */
async function focusPage(page: Page, step: string): Promise<void> {
  await page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
  await expectFocusOnPage(page, step);
}

/** Reads that the focus is on the page's body, the element that had it before a key. */
async function expectFocusOnPage(page: Page, step: string): Promise<void> {
  await expect.poll(() => onBody(page), { message: step }).toBe(true);
}

// SLOW(>=2s): three pages of the project, each with a page opened from it.
test("k-new-tab: Shift+O on the project List, the Board and the Architecture page opens the item o opens in a new page, and the first page keeps its address", async ({
  context,
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  await seedPriorityWithWork("CORE", "Ledger replay");
  await createIssue({ project: "CORE", title: "Mirror audit" });
  await seedArchitectureLeaf("CORE");
  await page.setViewportSize(WIDE);

  // The List: `j` from the page focuses its first row.
  await openIssuesTab(page, "CORE", "List");
  await expect(page.locator("[data-issue-row]")).toHaveCount(2);
  await focusPage(page, "k-new-tab: on the List, the focus starts on the page");
  await page.keyboard.press("j");
  const row = page.locator("[data-issue-row]:focus");
  await expect(row, "k-new-tab: on the List, j focuses a row").toHaveCount(1);
  await opensWhereOOpens(context, page, row.locator("[data-issue-link]"), "on the project List");

  // The Board: `j` from the page focuses the first card.
  await openIssuesTab(page, "CORE", "Board");
  await expect(page.locator("[data-board-card]")).toHaveCount(2);
  await focusPage(page, "k-new-tab: on the Board, the focus starts on the page");
  await page.keyboard.press("j");
  const card = page.locator("[data-board-card]:focus");
  await expect(card, "k-new-tab: on the Board, j focuses a card").toHaveCount(1);
  await opensWhereOOpens(context, page, card.locator("[data-issue-link]"), "on the Board");

  // The Architecture page: the one component, which has no children, by its own link.
  await page.goto("/projects/CORE/architecture");
  const component = page.locator(`[data-component-row="${ARCHITECTURE_LEAF.id}"]`);
  await expect(component).toBeVisible();
  await focusPage(page, "k-new-tab: on the Architecture page, the focus starts on the page");
  await page.keyboard.press("j");
  await expect(
    component,
    "k-new-tab: on the Architecture page, j focuses the component"
  ).toBeFocused();
  await opensWhereOOpens(
    context,
    page,
    component.getByRole("link", { exact: true, name: ARCHITECTURE_LEAF.title }),
    "on the Architecture page"
  );
});

// SLOW(>=2s): the Priorities page and a priority's page, each with a page opened from it.
test("k-new-tab: Shift+O on the Priorities page and on a priority's page opens the item o opens in a new page, and the first page keeps its address", async ({
  context,
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const { issueKey } = await seedPriorityWithWork("CORE", "Ledger replay");
  await page.setViewportSize(WIDE);

  // The Priorities page: `j` focuses the one priority, whose `o` opens its own page.
  await page.goto("/priorities");
  await expect(page.locator("[data-priority-row]")).toHaveCount(1);
  await focusPage(page, "k-new-tab: on the Priorities page, the focus starts on the page");
  await page.keyboard.press("j");
  const priority = page.locator("[data-priority-row]:focus");
  await expect(priority, "k-new-tab: on the Priorities page, j focuses a priority").toHaveCount(1);
  await opensWhereOOpens(
    context,
    page,
    priority.locator("[data-priority-link]"),
    "on the Priorities page"
  );

  // That `o` opened the priority's page, whose table lists the work under it.
  const work = page.locator(`[data-issue-row="${issueKey}"]`);
  await expect(work, "k-new-tab: the priority's page lists its work").toBeVisible();
  await focusPage(page, "k-new-tab: on a priority's page, the focus starts on the page");
  await page.keyboard.press("j");
  await expect(work, "k-new-tab: on a priority's page, j focuses its work").toBeFocused();
  await opensWhereOOpens(context, page, work.locator("[data-issue-link]"), "on a priority's page");
});

test("k-new-tab: on the project's Documents list j and k walk the documents, Shift+O opens the focused one in a new page, and o and Enter open it in this page", async ({
  context,
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  for (const [name, line] of [
    ["Runbook", "Restart, reseed, reopen."],
    ["Roster", "Who holds the pager this week."],
  ] as const) {
    await createProjectDocument("CORE", { content: `# ${name}\n\n${line}\n`, name }, asker);
  }
  await page.setViewportSize(WIDE);
  await page.goto("/projects/CORE/documents");
  const documents = page
    .getByRole("region", { name: "Project documents" })
    .locator("li[aria-label]");
  await expect(documents).toHaveCount(2);
  // A project document's row holds one link, its name, which `o` follows.
  const [first, second] = [documents.nth(0), documents.nth(1)];

  await focusPage(page, "k-new-tab: on the Documents list, the focus starts on the page");
  await page.keyboard.press("j");
  await expect(
    first,
    "k-new-tab: on the Documents list, j focuses the first document"
  ).toBeFocused();
  await page.keyboard.press("j");
  await expect(
    second,
    "k-new-tab: on the Documents list, j focuses the second document"
  ).toBeFocused();
  await page.keyboard.press("k");
  await expect(first, "k-new-tab: on the Documents list, k goes back to the first").toBeFocused();
  await page.keyboard.press("j");
  await expect(second).toBeFocused();
  await opensWhereOOpens(context, page, second.getByRole("link").first(), "on the Documents list");

  // Enter on the row itself opens it too.
  await page.goBack();
  await expect(documents).toHaveCount(2);
  const firstAddress = await hrefOf(first.getByRole("link").first());
  await focusPage(page, "k-new-tab: back on the Documents list, the focus starts on the page");
  await page.keyboard.press("j");
  await expect(first).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page, "k-new-tab: Enter on a document's row opens it in this page").toHaveURL(
    firstAddress
  );
});

// SLOW(>=2s): two searches of the command bar, each with a page opened from it.
test("k-new-tab: in the command bar Ctrl+Enter on an issue's hit and a Ctrl+click on a document's hit open it in a new page, and the first page and the bar stay as they were", async ({
  context,
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  const runbook = await createProjectDocument(
    "CORE",
    { content: "# Runbook\n\nRestart, reseed, reopen.\n", name: "Runbook" },
    asker
  );
  await page.setViewportSize(WIDE);
  await page.goto("/projects/CORE");
  await expect(page.getByRole("button", { exact: true, name: "Board" })).toBeVisible();
  const before = page.url();
  await focusPage(page, "k-new-tab: before the command bar, the focus is on the page");
  await page.keyboard.press("Control+k");
  const bar = page.getByRole("dialog", { name: "Search" });
  const box = bar.getByRole("combobox", { name: "Search" });
  await expect(box, "k-new-tab: the command bar opens with the focus in its box").toBeFocused();

  // Ctrl+Enter on the issue's hit, which its key puts at the head of the list.
  await box.fill(issue.key);
  const issueHit = `search-option-issue-${issue.key}`;
  await expect(
    bar.locator(`[id="${issueHit}"]`),
    "k-new-tab: the bar lists the issue"
  ).toBeVisible();
  await expect(box, "k-new-tab: the issue's hit is highlighted").toHaveAttribute(
    "aria-activedescendant",
    issueHit
  );
  await withNewPage(
    page,
    () =>
      test.step("k-new-tab: in the command bar, Ctrl+Enter opens a new page", () =>
        newPageAt(
          context,
          () => box.press("Control+Enter"),
          "k-new-tab: in the command bar, Ctrl+Enter on the issue's hit"
        )),
    (fromKey) => {
      expect(
        new URL(fromKey.url()).pathname,
        "k-new-tab: Ctrl+Enter opens the issue's page in the new page"
      ).toBe(`/issues/${issue.key}`);
    }
  );
  expect(page.url(), "k-new-tab: after Ctrl+Enter the first page keeps its address").toBe(before);
  await expect(box, "k-new-tab: after Ctrl+Enter the bar keeps its words").toHaveValue(issue.key);
  await expect(box, "k-new-tab: after Ctrl+Enter the bar keeps its highlight").toHaveAttribute(
    "aria-activedescendant",
    issueHit
  );
  await expect(box, "k-new-tab: after Ctrl+Enter the focus is in the bar's box").toBeFocused();

  // A Ctrl+click on the document's hit.
  await box.fill("reseed");
  const documentHit = bar.getByRole("option", { name: /^doc / });
  await expect(documentHit, "k-new-tab: the bar lists the document").toBeVisible();
  await expect(documentHit).toHaveAttribute("aria-selected", "true");
  await withNewPage(
    page,
    () =>
      test.step("k-new-tab: in the command bar, a Ctrl+click opens a new page", () =>
        newPageAt(
          context,
          () => documentHit.click({ modifiers: ["Control"] }),
          "k-new-tab: in the command bar, a Ctrl+click on the document's hit"
        )),
    (fromClick) => {
      expect(fromClick.url(), "k-new-tab: a Ctrl+click opens the document in the new page").toMatch(
        new RegExp(`/projects/CORE/documents/${runbook.artifact.slug}\\?q=reseed$`)
      );
    }
  );
  expect(page.url(), "k-new-tab: after a Ctrl+click the first page keeps its address").toBe(before);
  await expect(bar, "k-new-tab: after a Ctrl+click the bar stays open").toBeVisible();
  await expect(box, "k-new-tab: after a Ctrl+click the bar keeps its words").toHaveValue("reseed");
  await expect(
    documentHit,
    "k-new-tab: after a Ctrl+click the bar keeps its highlight"
  ).toHaveAttribute("aria-selected", "true");
  await expect(box, "k-new-tab: after a Ctrl+click the focus is in the bar's box").toBeFocused();
});

// ---------------------------------------------------------------------------
// k-filter-keys (R24): Shift+F opens a page's filter panel and Shift+V its display menu, each on
// its first control; the panel's controls work from the keyboard, and Escape closes an open list,
// then the panel, and puts the focus back where it was before the key.
// ---------------------------------------------------------------------------

/** Presses Shift+F and reads that the page's filter panel opened with the focus on `first`, its
 *  first control. The panel is `Filter` on the Inbox, the Agents page and the Delivery page, and
 *  `Filters` on the project List and the Board. */
async function shiftFOpens(page: Page, first: Locator, place: string): Promise<Locator> {
  await page.keyboard.press("Shift+F");
  const panel = page.getByRole("dialog", { name: /^Filters?$/ });
  await expect(panel, `k-filter-keys: ${place}, Shift+F opens the filter panel`).toBeVisible();
  await expect(
    first,
    `k-filter-keys: ${place}, the focus is on the panel's first control`
  ).toBeFocused();
  return panel;
}

/** Works a field of an open panel from the keyboard: Enter on its trigger opens its list,
 *  `typed` narrows the list to the one value `option` names, and ArrowDown and Enter turn that
 *  value on, since the list's active row starts on its first; Escape then closes the list alone,
 *  with the focus back on the field. */
async function tickField(
  page: Page,
  field: Locator,
  typed: string,
  option: RegExp,
  place: string
): Promise<void> {
  await expect(field, `k-filter-keys: ${place}, the focus is on the field`).toBeFocused();
  await page.keyboard.press("Enter");
  const list = listOf(field);
  await expect(list, `k-filter-keys: ${place}, Enter opens the field's list`).toBeVisible();
  await page.keyboard.type(typed);
  const options = list.getByRole("option");
  await expect(
    options,
    `k-filter-keys: ${place}, typing narrows the list to one value`
  ).toHaveCount(1);
  await expect(options).toHaveAccessibleName(option);
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect(
    options,
    `k-filter-keys: ${place}, ArrowDown and Enter turn the value on`
  ).toHaveAttribute("aria-selected", "true");
  await page.keyboard.press("Escape");
  await expect(list, `k-filter-keys: ${place}, Escape closes the field's list`).toBeHidden();
  await expect(
    field,
    `k-filter-keys: ${place}, Escape leaves the focus on the field`
  ).toBeFocused();
}

/** Presses Escape in an open panel and reads that it closed and that the focus went back to
 *  `before`, the element that had it before the key; the page itself when that is null. */
async function escapeCloses(
  page: Page,
  panel: Locator,
  before: Locator | null,
  place: string
): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(panel, `k-filter-keys: ${place}, Escape closes the panel`).toBeHidden();
  const step = `k-filter-keys: ${place}, Escape puts the focus back where it was before the key`;
  if (before === null) await expectFocusOnPage(page, step);
  else await expect(before, step).toBeFocused();
}

/** Presses Shift+V and reads that the View menu opened with the focus on the first choice of
 *  its first section. */
async function shiftVOpens(page: Page, section: string, place: string): Promise<Locator> {
  await page.keyboard.press("Shift+V");
  const menu = page.getByRole("dialog", { name: "View" });
  await expect(menu, `k-filter-keys: ${place}, Shift+V opens the View menu`).toBeVisible();
  await expect(
    menu.getByRole("group", { name: section }).getByRole("radio").first(),
    `k-filter-keys: ${place}, the focus is on the menu's first control`
  ).toBeFocused();
  return menu;
}

/** In an open View menu, with the focus on the choice `from` of `section`: ArrowDown selects the
 *  choice after it, `to`, with the focus on it, as a group of radios does. */
async function arrowToNext(
  page: Page,
  menu: Locator,
  section: string,
  [from, to]: readonly [string, string],
  place: string
): Promise<void> {
  await expect(displayChoice(menu, section, from)).toBeFocused();
  await page.keyboard.press("ArrowDown");
  const next = displayChoice(menu, section, to);
  await expect(
    next,
    `k-filter-keys: ${place}, ArrowDown selects the next ${section} choice`
  ).toBeChecked();
  await expect(next).toBeFocused();
}

/** Two projects of two questions each, at P1 and P2 (`seedAsksOverTwoProjects`), which it
 *  answers, and a third issue of CORE in another status with no question, labelled `ledger`
 *  with CORE's P1 issue. */
async function seedFilterKeys(): Promise<SpreadAsk[]> {
  await createProject({ key: "CORE", name: "Core" });
  await createProject({ key: "OPS", name: "Ops" });
  const asks = await seedAsksOverTwoProjects(["CORE", "OPS"]);
  const quiet = await createIssue({ project: "CORE", title: "Mirror audit" });
  await patchIssue(quiet.key, { labels: ["ledger"], status: "in_progress" });
  const first = asks.find((ask) => ask.project === "CORE" && ask.priority === 1);
  if (first === undefined) throw new Error("k-filter-keys: the seed holds no P1 question of CORE");
  await patchIssue(first.issueKey, { labels: ["ledger"] });
  return asks;
}

/** Presses `j` in the drawer `peek` and answers the row it put the focus on, by that row's id. */
async function focusRowWithJ(page: Page, peek: InboxAt): Promise<Locator> {
  await page.keyboard.press("j");
  const row = peek.scope.locator("[data-inbox-row]:focus");
  await expect(row, "k-filter-keys: in the drawer, j focuses a row").toHaveCount(1);
  return rowOf(peek, String(await row.getAttribute("data-inbox-row")));
}

// SLOW(>=2s): the Inbox opened twice, with its filter panel worked by key in each.
test("k-filter-keys: on the Inbox page and in its drawer Shift+F opens the Filter panel on its first field, whose keys narrow the list, and Escape twice hands the focus back", async ({
  page,
}) => {
  await seedFilterKeys();

  // The Inbox page, from the page itself.
  const inbox = await openInboxAt(page, "the reading pane", 4);
  const rows = inbox.scope.locator("[data-inbox-row]");
  await focusPage(page, "k-filter-keys: on the Inbox, the focus starts on the page");
  const field = page.getByRole("button", { name: "Filter by priority" });
  const panel = await shiftFOpens(page, field, "on the Inbox");
  await tickField(page, field, "P1", /^P1,/, "on the Inbox");
  await expect(rows, "k-filter-keys: on the Inbox, the field narrows the list").toHaveCount(2);
  await escapeCloses(page, panel, null, "on the Inbox");
  await expect(rows, "k-filter-keys: on the Inbox, the narrowing stays").toHaveCount(2);
  await expect(page).toHaveURL(/[?&]priority=P1(&|$)/);

  // The drawer, from its first row.
  const peek = await openInboxAt(page, "the drawer", 4);
  const inDrawer = peek.scope.locator("[data-inbox-row]");
  const drawerRow = await focusRowWithJ(page, peek);
  const drawerField = page.getByRole("button", { name: "Filter by priority" });
  const drawerPanel = await shiftFOpens(page, drawerField, "in the drawer");
  await tickField(page, drawerField, "P1", /^P1,/, "in the drawer");
  await expect(inDrawer, "k-filter-keys: in the drawer, the field narrows the list").toHaveCount(2);
  await escapeCloses(page, drawerPanel, drawerRow, "in the drawer");
  await expect(inDrawer, "k-filter-keys: in the drawer, the narrowing stays").toHaveCount(2);
  await expect(drawer(page), "k-filter-keys: the drawer stays open").toBeVisible();
});

// SLOW(>=2s): the Inbox opened twice, with its View menu worked by key in each.
test("k-filter-keys: on the Inbox page and in its drawer Shift+V opens the View menu on its first choice, whose arrows sort and group the list, and Escape hands the focus back", async ({
  page,
}) => {
  const asks = await seedFilterKeys();

  // The Inbox page: Sort by, then Group by, which Tab reaches.
  const inbox = await openInboxAt(page, "the reading pane", 4);
  await focusPage(page, "k-filter-keys: on the Inbox, the focus starts on the page");
  const menu = await shiftVOpens(page, "Sort by", "on the Inbox");
  await arrowToNext(page, menu, "Sort by", ["newest", "priority"], "on the Inbox");
  await expect(page, "k-filter-keys: on the Inbox, the list is sorted by priority").toHaveURL(
    /[?&]sort=priority(&|$)/
  );
  await page.keyboard.press("Tab");
  await arrowToNext(page, menu, "Group by", ["priority", "urgency"], "on the Inbox");
  await expect(page, "k-filter-keys: on the Inbox, the list is grouped by urgency").toHaveURL(
    /[?&]group=urgency(&|$)/
  );
  await expect(
    inbox.scope.getByRole("button", { name: /^Medium \(4\)$/ }),
    "k-filter-keys: on the Inbox, the list is banded by urgency"
  ).toBeVisible();
  // Sorted by priority and banded by urgency, the one band lists both P1 questions before both
  // P2 ones. Newest first, by each row's last write, lists the last asked first, and each project
  // was asked at P1 and then at P2 (`seedAsksOverTwoProjects`), so it lists P2, P1, P2, P1.
  const priorityOf = new Map(asks.map((ask) => [ask.askId, ask.priority]));
  await expect
    .poll(
      async () =>
        (
          await inbox.scope
            .locator("[data-inbox-row]")
            .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""))
        ).map((id) => priorityOf.get(id)),
      { message: "k-filter-keys: on the Inbox, the band lists the rows by priority" }
    )
    .toEqual([1, 1, 2, 2]);
  await escapeCloses(page, menu, null, "the Inbox's View menu");

  // The drawer, from its first row: Tab to Group by.
  const peek = await openInboxAt(page, "the drawer", 4);
  const drawerRow = await focusRowWithJ(page, peek);
  const drawerMenu = await shiftVOpens(page, "Sort by", "in the drawer");
  await page.keyboard.press("Tab");
  await arrowToNext(page, drawerMenu, "Group by", ["priority", "urgency"], "in the drawer");
  await expect(
    peek.scope.getByRole("button", { name: /^Medium \(4\)$/ }),
    "k-filter-keys: in the drawer, the list is banded by urgency"
  ).toBeVisible();
  await escapeCloses(page, drawerMenu, drawerRow, "the drawer's View menu");
  await expect(drawer(page), "k-filter-keys: the drawer stays open").toBeVisible();
});

// SLOW(>=2s): the List, the Board, a reload of it and the Priorities page, each worked by key.
test("k-filter-keys: on the project List, the Board and the Priorities page Shift+F and Shift+V open the panel and the menu, whose keys narrow and group the list, and on the Board's open panel Shift+F moves the focus into it", async ({
  page,
}) => {
  await seedFilterKeys();
  await seedPriorityWithWork("CORE", "Ledger replay");
  await seedPriorityWithWork("CORE", "Mirror cutover");
  await page.setViewportSize(WIDE);

  // The List: its panel holds chips, the first of which is Needs you.
  await openIssuesTab(page, "CORE", "List");
  const rows = page.locator("[data-issue-row]");
  await expect(rows).toHaveCount(5);
  await focusPage(page, "k-filter-keys: on the List, the focus starts on the page");
  const listChip = page
    .getByRole("dialog", { name: "Filters" })
    .getByRole("button", { exact: true, name: "Needs you" });
  const listPanel = await shiftFOpens(page, listChip, "on the List");
  await page.keyboard.press("Space");
  await expect(rows, "k-filter-keys: on the List, Space on the chip narrows the list").toHaveCount(
    2
  );
  await escapeCloses(page, listPanel, null, "on the List");
  await expect(rows, "k-filter-keys: on the List, the narrowing stays").toHaveCount(2);
  const listMenu = await shiftVOpens(page, "Group by", "on the List");
  const listGroups = listMenu.getByRole("group", { name: "Group by" }).getByRole("radio");
  await page.keyboard.press("ArrowDown");
  await expect(
    listGroups.nth(1),
    "k-filter-keys: on the List, ArrowDown selects the next grouping"
  ).toBeChecked();
  await expect(
    page.getByRole("button", { name: /^Collapse .+, .*\bissues?\b/ }).first(),
    "k-filter-keys: on the List, the list is grouped"
  ).toBeVisible();
  await escapeCloses(page, listMenu, null, "the List's View menu");

  // The Board: a field, Labels, and a chip, Needs you.
  await openIssuesTab(page, "CORE", "Board");
  const cards = page.locator("[data-board-card]");
  await expect(cards).toHaveCount(5);
  await focusPage(page, "k-filter-keys: on the Board, the focus starts on the page");
  const labels = page
    .getByRole("dialog", { name: "Filters" })
    .getByRole("button", { name: /^Labels/ });
  const boardPanel = await shiftFOpens(page, labels, "on the Board");
  await tickField(page, labels, "led", /^ledger/, "on the Board");
  await expect(cards, "k-filter-keys: on the Board, the field narrows the board").toHaveCount(2);
  await page.keyboard.press("Tab");
  const boardChip = boardPanel.getByRole("button", { exact: true, name: "Needs you" });
  await expect(boardChip, "k-filter-keys: on the Board, Tab moves to the chip").toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    cards,
    "k-filter-keys: on the Board, Enter on the chip narrows the board"
  ).toHaveCount(1);
  await escapeCloses(page, boardPanel, null, "on the Board");
  await expect(cards, "k-filter-keys: on the Board, the narrowing stays").toHaveCount(1);

  // The Board with its panel open from the stored choice, which a load opens with no focus in it:
  // Shift+F opens it (and stores that), and the load after it opens it again on its own.
  await shiftFOpens(page, labels, "on the Board again");
  await page.reload();
  const trigger = control(controlRow(page.getByTestId("main-content")), CONTROL.panelTrigger);
  await expect(trigger, "k-filter-keys: the load opens the Board's panel").toHaveAttribute(
    "aria-expanded",
    "true"
  );
  await expect(cards).toHaveCount(1);
  await focusPage(page, "k-filter-keys: on the Board's open panel, the focus starts on the page");
  const openPanel = await shiftFOpens(page, labels, "on the Board's open panel");
  await expect(
    trigger,
    "k-filter-keys: Shift+F leaves the Board's open panel open"
  ).toHaveAttribute("aria-expanded", "true");
  await escapeCloses(page, openPanel, null, "on the Board's open panel");

  // The Priorities page: its View menu's Group by.
  await page.goto("/priorities");
  await expect(page.locator("[data-priority-row]")).toHaveCount(2);
  await focusPage(page, "k-filter-keys: on the Priorities page, the focus starts on the page");
  const prioritiesMenu = await shiftVOpens(page, "Group by", "on the Priorities page");
  const priorityGroups = prioritiesMenu.getByRole("group", { name: "Group by" }).getByRole("radio");
  await page.keyboard.press("ArrowDown");
  await expect(
    priorityGroups.nth(1),
    "k-filter-keys: on the Priorities page, ArrowDown selects the next grouping"
  ).toBeChecked();
  await expect(
    page.getByRole("button", { name: /^Collapse .+, .*\bpriorit/ }).first(),
    "k-filter-keys: on the Priorities page, the list is grouped"
  ).toBeVisible();
  await escapeCloses(page, prioritiesMenu, null, "the Priorities page's View menu");
});

// SLOW(>=2s): the Agents page and both views of the Delivery page, each worked by key.
test("k-filter-keys: on the Agents page and the Delivery list Shift+F opens the Filter panel on its first field, whose keys narrow the list, and Escape twice hands the focus back; on the Delivery timeline Shift+F opens the same panel on its first field and Escape hands the focus back", async ({
  page,
}) => {
  // Two live sessions on two machines; the first asked a question, which puts it in Needs you
  // and leaves the second alone in Idle.
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await seedTwoMachines(Date.now());
  await createAsk(
    issue.key,
    { question: "Which mirror replays first?" },
    { actor: { id: sessionIds.observer, kind: "session" as const }, as: "agent" as const }
  );
  await seedDeliveryFixture();
  await seedSecondRepositoryPull();
  await page.setViewportSize(WIDE);

  // The Agents page: its first field is Group.
  await openAgents(page);
  const agents = shownAgentRows(page);
  await expect(agents).toHaveCount(2);
  await focusPage(page, "k-filter-keys: on the Agents page, the focus starts on the page");
  const group = page.getByRole("button", { name: "Filter by group" });
  const agentsPanel = await shiftFOpens(page, group, "on the Agents page");
  await tickField(page, group, "Idl", /^Idle/, "on the Agents page");
  await expect(agents, "k-filter-keys: on the Agents page, the field narrows the list").toHaveCount(
    1
  );
  await escapeCloses(page, agentsPanel, null, "on the Agents page");
  await expect(agents, "k-filter-keys: on the Agents page, the narrowing stays").toHaveCount(1);

  // The Delivery page's List view: its first field is Repository. The fixture's two pull
  // requests of acme/widgets and the one of acme/gadgets are listed, and the pick of acme/widgets
  // leaves its two.
  await page.goto(`/delivery?${TIMELINE_FIXTURE_QUERY}&mode=list`);
  const pulls = page.locator("tbody tr[data-pr-row]");
  await expect(pulls).toHaveCount(3);
  await focusPage(page, "k-filter-keys: on the Delivery list, the focus starts on the page");
  const repository = page.getByRole("button", { name: /^Repository:/ });
  const deliveryPanel = await shiftFOpens(page, repository, "on the Delivery list");
  await tickField(page, repository, "widg", /^acme\/widgets,/, "on the Delivery list");
  await expect(
    pulls,
    "k-filter-keys: on the Delivery list, the field narrows the list"
  ).toHaveCount(2);
  await escapeCloses(page, deliveryPanel, null, "on the Delivery list");
  await expect(
    controlRow(page.getByTestId("main-content")).getByRole("button", {
      name: /Repository: acme\/widgets/,
    }),
    "k-filter-keys: on the Delivery list, the narrowing stays as its chip"
  ).toBeVisible();
  await expect(page).toHaveURL(/[?&]repo=acme%2Fwidgets(&|$)/);
  await expect(pulls, "k-filter-keys: on the Delivery list, the narrowing stays").toHaveCount(2);

  // The Timeline view opens the same panel on the same field by the same key, and Escape closes
  // it with the focus back on the page.
  await page.goto(`/delivery?${TIMELINE_FIXTURE_QUERY}`);
  await expect(page.getByRole("button", { exact: true, name: "List" })).toBeVisible();
  await focusPage(page, "k-filter-keys: on the Delivery timeline, the focus starts on the page");
  const timelinePanel = await shiftFOpens(page, repository, "on the Delivery timeline");
  await escapeCloses(page, timelinePanel, null, "on the Delivery timeline");
});

// ---------------------------------------------------------------------------
// k-list-focus: the six lists of the shared single- or multi-pick list (`MultiSelect`) the check
// names - the Agents Filter's Role and Machine lists, Details' labels and components lists, and
// the assignee and the repository of today's issue page header - each opened by the keyboard, a
// choice reached by the arrows and picked by Enter. Where the list stays open the focus is in its
// search box, also where the pick takes the picked row away; where the pick closes the list the
// focus is on its trigger; and in no case is it on the page's body or on another control. Two
// other hosts take their whole list away on a pick by design and leave the focus to their panel's
// hand-off: the search bar's Filter, whose Project or Status list a pick removes
// (`SearchPalette.tsx`), and a Delivery facet whose last picked value is unpicked and which has no
// values (`DeliveryPage.tsx`'s `Facet`). Every step's message names the list, so a failure names
// the host it failed on.
// ---------------------------------------------------------------------------

/** Where the focus is, read against `target` at once: `on it` where `target` holds it, else what
 *  holds it - the page's body, or an element by its role (its tag where it has none) and its
 *  `aria-label` (its words where it has none). A target the page does not draw, or draws more
 *  than once, reads so rather than being waited for. */
function focusAgainst(target: Locator): Promise<string> {
  return target.evaluateAll((elements) => {
    if (elements.length !== 1) return `on no one target: the page draws ${elements.length}`;
    const focused = document.activeElement;
    if (focused === elements[0]) return "on it";
    if (focused === null || focused === document.body) return "on the page's body";
    const name = focused.getAttribute("aria-label") ?? (focused.textContent ?? "").trim();
    return `on ${focused.getAttribute("role") ?? focused.tagName.toLowerCase()} "${name.slice(0, 80)}"`;
  });
}

/** Reads that `target` holds the focus, and still holds it two frames later, by when the commit
 *  the last key asked for and the hand-offs its effects make of a focus it took away have run;
 *  else fails by `step`, with what holds the focus instead. */
async function expectFocusOn(page: Page, target: Locator, step: string): Promise<void> {
  await expect.poll(() => focusAgainst(target), { message: step }).toBe("on it");
  await nextFrames(page);
  // One read: the two frames are the last the key's commit and its hand-offs take.
  expect(await focusAgainst(target), `${step}, two frames later`).toBe("on it");
}

/** The words of the row an open list's search box highlights (`aria-activedescendant`), its tick
 *  left out; empty where it highlights none. */
function highlightedBy(search: Locator): Promise<string> {
  return search.evaluate((box) => {
    const row = document.getElementById(box.getAttribute("aria-activedescendant") ?? "");
    return (row?.textContent ?? "").replace("✓", "").trim();
  });
}

/** Presses ArrowDown, or ArrowUp, as many times as `to` stands below, or above, `from` in `rows`,
 *  the open list's rows in their order, and reads that its search box then highlights `to`. */
async function arrowTo(
  page: Page,
  search: Locator,
  rows: readonly string[],
  [from, to]: readonly [string, string],
  step: string
): Promise<void> {
  const by = rows.indexOf(to) - rows.indexOf(from);
  for (let press = 0; press < Math.abs(by); press += 1) {
    await page.keyboard.press(by > 0 ? "ArrowDown" : "ArrowUp");
  }
  await expect.poll(() => highlightedBy(search), { message: step }).toBe(to);
}

/** One multi-pick list of k-list-focus, its list just opened by a key. */
interface MultiPick {
  /** What the failures call it: "the Agents Role list". */
  readonly what: string;
  readonly trigger: Locator;
  /** The name of the list's search box. */
  readonly search: string;
  /** The list's rows as it opens, in their order. */
  readonly rows: readonly string[];
  /** The row the arrows move to and Enter picks, which the pick keeps on the page. */
  readonly pick: string;
}

/** From a multi-pick list a key has just opened: its search box holds the focus, the arrows move
 *  the highlight from its first row down to the next and on to `pick`, and Enter picks it, which
 *  keeps its row and the list open with the focus in the search box. Answers the search box. */
async function multiPickKeepsFocus(page: Page, list: MultiPick): Promise<Locator> {
  const say = (step: string) => `k-list-focus: ${list.what}, ${step}`;
  const search = listOf(list.trigger).getByRole("combobox", { name: list.search });
  await expectFocusOn(page, search, say("the key opens the list with the focus in its search box"));
  await expect(
    listOf(list.trigger).getByRole("option"),
    say(`the list holds ${list.rows.join(", ")}`)
  ).toHaveText(list.rows.map((row) => new RegExp(`^✓?${row}$`)));
  const [first = "", next = first] = list.rows;
  await expect
    .poll(() => highlightedBy(search), { message: say("the list opens on its first row") })
    .toBe(first);
  await arrowTo(page, search, list.rows, [first, next], say(`ArrowDown moves to ${next}`));
  await arrowTo(page, search, list.rows, [next, list.pick], say(`the arrows move to ${list.pick}`));
  await page.keyboard.press("Enter");
  const picked = listOf(list.trigger).getByRole("option", { exact: true, name: list.pick });
  await expect(picked, say(`Enter picks ${list.pick}`)).toHaveAttribute("aria-selected", "true");
  await expectFocusOn(
    page,
    search,
    say(`Enter picks ${list.pick}, which keeps its row, and leaves the focus in the search box`)
  );
  await expect(list.trigger, say("Enter keeps the list open")).toHaveAttribute(
    "aria-expanded",
    "true"
  );
  return search;
}

/** One list of the Agents page's Filter that lists a picked value no live session reports, only
 *  while it is picked: Role and Machine, which each list such a value among the live ones, in
 *  their sorted order (`filterOptions`). */
interface GhostList {
  readonly what: string;
  /** Its parameter in the page's address, and the word its trigger is named by. */
  readonly field: "machine" | "role";
  /** The value no live session reports, which the address the row opens picks. */
  readonly ghost: string;
  readonly search: string;
  /** The list's rows as it opens with `ghost` picked, in their order. */
  readonly rows: readonly string[];
  /** A value a live session reports, which the arrows reach and Enter picks. */
  readonly live: string;
}

/** Every step of k-list-focus on `list`, over the two machines' sessions and the Planner's, on the
 *  first of them, the one session that holds a role. */
async function ghostListFocus(page: Page, list: GhostList): Promise<void> {
  const say = (step: string) => `k-list-focus: ${list.what}, ${step}`;
  const now = Date.now();
  const planner = machineSession(sessionIds.planner, "Planner", {
    host: MACHINES[0],
    lastSeen: now - 30_000,
  });
  await seedTwoMachines(now, [{ ...planner, roles: ["planner"] }]);
  await page.setViewportSize(WIDE);
  const trigger = page.getByRole("button", { name: `Filter by ${list.field}` });
  const row = (value: string) => listOf(trigger).getByRole("option", { exact: true, name: value });
  // The page with the ghost picked, its Filter panel opened by Shift+F, and the list by Tab to its
  // trigger and Enter.
  const openList = async (pass: string) => {
    await page.goto(`/agents?${list.field}=${list.ghost}`);
    await expect(page.getByRole("heading", { level: 1, name: "Agents" })).toBeVisible();
    await focusPage(page, say(`${pass}: the focus starts on the page`));
    await page.keyboard.press("Shift+F");
    await expect(
      page.getByRole("dialog", { name: "Filter" }),
      say(`${pass}: Shift+F opens the Filter panel`)
    ).toBeVisible();
    await tabTo(page, trigger, say(`${pass}: Tab reaches the list's trigger`));
    await page.keyboard.press("Enter");
  };

  // The arrows and Enter in the search box: the pick of a live value keeps its row, and the
  // unpick of the ghost takes its row away, the focus in the search box through both.
  await openList("by the arrows");
  const search = await multiPickKeepsFocus(page, {
    pick: list.live,
    rows: list.rows,
    search: list.search,
    trigger,
    what: list.what,
  });
  await arrowTo(
    page,
    search,
    list.rows,
    [list.live, list.ghost],
    say(`the arrows move to ${list.ghost}`)
  );
  await page.keyboard.press("Enter");
  await expect(
    row(list.ghost),
    say(`Enter unpicks ${list.ghost} and takes its row away`)
  ).toHaveCount(0);
  await expectFocusOn(
    page,
    search,
    say(
      `Enter unpicks ${list.ghost}, which takes its row away, and leaves the focus in the search box`
    )
  );
  await expect(trigger, say("the unpick keeps the list open")).toHaveAttribute(
    "aria-expanded",
    "true"
  );

  // Enter on the ghost's own row, focused as a screen reader's cursor focuses it: no key moves the
  // focus from the search box to a row, since Tab there closes the list. The pick takes the
  // focused row away inside the Filter panel, whose own hand-off for a control a press takes out
  // of it (`useFocusPlace`) would put the focus on the list's trigger.
  await openList("on its focused row");
  await expectFocusOn(page, search, say("Enter on the trigger opens the list on its search box"));
  await row(list.ghost).focus();
  await expect(row(list.ghost), say(`the ${list.ghost} row takes the focus`)).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    row(list.ghost),
    say(`Enter on the focused ${list.ghost} row unpicks it and takes the row away`)
  ).toHaveCount(0);
  await expectFocusOn(
    page,
    search,
    say(
      `Enter on the focused ${list.ghost} row, which takes the row away, leaves the focus in the search box`
    )
  );
  await expect(trigger, say("the unpick keeps the list open")).toHaveAttribute(
    "aria-expanded",
    "true"
  );
}

// SLOW(>=2s): the Agents page loaded twice, its Role list opened by key each time and picked from
// three times.
// @webkit @firefox: where each engine leaves the focus of a focused row a commit takes off the
// page, which the list reads through `focusLost`, and the order of its check and the panel's.
test("k-list-focus: on the Agents page's Filter, Enter on a role the arrows reach, on the picked role no live session reports, and on that role's focused row leaves the focus in the Role list's search box @firefox @webkit", async ({
  page,
}) => {
  await ghostListFocus(page, {
    field: "role",
    ghost: "ghost-role",
    live: "planner",
    rows: ["ghost-role", "planner"],
    search: "Search roles",
    what: "the Agents Role list",
  });
});

// SLOW(>=2s): the Agents page loaded twice, its Machine list opened by key each time and picked
// from three times.
// @webkit @firefox: where each engine leaves the focus of a focused row a commit takes off the
// page, which the list reads through `focusLost`, and the order of its check and the panel's.
test("k-list-focus: on the Agents page's Filter, Enter on a machine the arrows reach, on the picked machine no live session reports, and on that machine's focused row leaves the focus in the Machine list's search box @firefox @webkit", async ({
  page,
}) => {
  await ghostListFocus(page, {
    field: "machine",
    ghost: "ghost-box",
    live: MACHINES[0],
    rows: ["ghost-box", ...MACHINES],
    search: "Search machines",
    what: "the Agents Machine list",
  });
});

/** The repository on the fake GitHub that CORE's architecture is read from. */
const LIST_FOCUS_ARCHITECTURE = "legion/list-focus-arch";

/** CORE with an issue for Details on the new issue page: another issue of CORE holds the labels
 *  `ledger` and `mirror`, which the labels list offers, and CORE's architecture, imported from the
 *  fake GitHub, holds the components `ledger` and `mirror`, which the components list offers.
 *  Answers the first issue's key. */
async function seedDetailsLists(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  const other = await createIssue({ project: "CORE", title: "Mirror audit" });
  await patchIssue(other.key, { labels: ["ledger", "mirror"] });
  await seedFakeGithub({
    [LIST_FOCUS_ARCHITECTURE]: {
      contents: "read",
      files: {
        "ledger.md": "---\ntitle: Ledger\n---\nWhat the ledger records.\n",
        "mirror.md": "---\ntitle: Mirror\n---\nWhat the mirror replays.\n",
      },
      installation_id: 103,
    },
  });
  await putArchitectureSource("CORE", { branch: "main", repo: LIST_FOCUS_ARCHITECTURE });
  expect(
    (await syncArchitectureSource("CORE", LIST_FOCUS_ARCHITECTURE)).last_error,
    "k-list-focus: CORE's architecture did not import"
  ).toBeNull();
  return issue.key;
}

// SLOW(>=2s): the fake GitHub's architecture imported, the new issue page loaded, and two lists
// of Details opened by key and picked from.
// @webkit @firefox: where each engine leaves the focus as Escape closes the labels list, which
// takes its focused search box off the page and asks `focusLost` (`MultiSelect`'s close); the
// Enter picks in the search box keep the focus there and ask it nothing.
test("k-list-focus: in Details on the new issue page, Enter on a label or a component the arrows reach leaves the focus in the labels or the components list's search box @firefox @webkit", async ({
  page,
}) => {
  const key = await seedDetailsLists();
  await withIssuePage(page.context(), "alice");
  await page.setViewportSize(WIDE);
  await gotoIssue(page, `/issues/${key}`);
  const labels = marginShell(page).locator(`[data-property-control="labels ${key}"]`);
  const components = marginShell(page).locator(`[data-property-control="components ${key}"]`);

  // `l` opens Details at the Labels row with the labels' control pressed.
  await focusPage(page, "k-list-focus: Details' labels list, the focus starts on the page");
  await page.keyboard.press("l");
  await multiPickKeepsFocus(page, {
    pick: "mirror",
    rows: ["ledger", "mirror"],
    search: "Search or create label",
    trigger: labels,
    what: "Details' labels list",
  });
  await page.keyboard.press("Escape");
  await expectFocusOn(
    page,
    labels,
    "k-list-focus: Details' labels list, Escape puts the focus back on its trigger"
  );

  // Tab from the labels' control reaches the components', and Enter opens its list.
  await tabTo(page, components, "k-list-focus: Details' components list, Tab reaches its trigger");
  await page.keyboard.press("Enter");
  await multiPickKeepsFocus(page, {
    pick: "mirror · Mirror",
    rows: ["ledger · Ledger", "mirror · Mirror"],
    search: "Search components",
    trigger: components,
    what: "Details' components list",
  });
});

/** From the page itself: Tab reaches the trigger of the single-pick list `what`, Enter opens the
 *  list on its search box, ArrowDown moves the highlight off the value it opened on, and Enter
 *  picks the row it moved to, which closes the list with the focus on its trigger, before and
 *  after the one write of `writes`, the `sent`th, goes out. */
async function singlePickFocus(
  page: Page,
  what: string,
  trigger: Locator,
  writes: readonly KeyedWrite<unknown>[],
  sent: number
): Promise<void> {
  const say = (step: string) => `k-list-focus: ${what}, ${step}`;
  await focusPage(page, say("the focus starts on the page"));
  await tabTo(page, trigger, say("Tab reaches the list's trigger"));
  await page.keyboard.press("Enter");
  const search = listOf(trigger).getByRole("combobox");
  await expectFocusOn(page, search, say("Enter opens the list with the focus in its search box"));
  const opened = await highlightedBy(search);
  await page.keyboard.press("ArrowDown");
  await expect
    .poll(() => highlightedBy(search), { message: say("ArrowDown moves to another row") })
    .not.toBe(opened);
  await page.keyboard.press("Enter");
  await expect(trigger, say("Enter closes the list")).toHaveAttribute("aria-expanded", "false");
  await expectFocusOn(page, trigger, say("Enter puts the focus on the list's trigger"));
  await expect.poll(() => writesSent(writes), { message: say("Enter sends one write") }).toBe(sent);
  await expectFocusOn(
    page,
    trigger,
    say("the focus is on the list's trigger once the pick's write has gone out")
  );
}

// SLOW(>=2s): today's issue page loaded, and its assignee's and its repository's lists each
// opened by key, picked from and written.
// @webkit @firefox: where each engine leaves the focus as a single pick closes its list: the pick
// focuses the list's trigger before the commit that takes the list off the page.
test("k-list-focus: on today's issue page, Enter on an assignee or a repository the arrows reach closes its list with the focus on the list's trigger @firefox @webkit", async ({
  page,
}) => {
  const { issueKey, title } = await seedChoices();
  await page.setViewportSize(WIDE);
  const writes = issueWrites(page, issueKey);
  await openIssue(page, issueKey, title);
  await singlePickFocus(
    page,
    "the header's assignee list",
    page.getByLabel(`Assignee of ${issueKey}`, { exact: true }),
    writes,
    1
  );
  await singlePickFocus(
    page,
    "the header's repository list",
    page.getByLabel(`Repository of ${issueKey}`, { exact: true }),
    writes,
    2
  );
});

// ---------------------------------------------------------------------------
// k-approval-keys (R7), the Inbox's part: on the approval request in hand `a` approves the version
// it shows with one press and Shift+R opens Request changes with the focus in its reason, beside
// the reading pane, from the short-row list and in the reading view at 1024x768, and in the
// drawer; before the document's read returns, and on a question, both send nothing.
// ---------------------------------------------------------------------------

/** The reason the rows' Request changes gives. */
const ROLLBACK_REASON = "Name the rollback path.";

/** Every review a page sends of one approval request, on each route a review can take: the
 *  versioned route `a`, Shift+R and Approve post to, the route with no version the merge base's
 *  Approve posts to, and the ask's answer, which a card sends. */
interface ReviewsSent {
  readonly answers: KeyedWrite<unknown>[];
  readonly unversioned: KeyedWrite<unknown>[];
  readonly versioned: KeyedWrite<unknown>[];
}

function watchReviews(page: Page, approvals: readonly SeededApproval[]): ReviewsSent[] {
  return approvals.map((approval) => ({
    answers: answerWrites(page, approval.askId),
    unversioned: reviewWrites(page, approval.artifactId),
    versioned: versionReviewWrites(page, approval.artifactId, approval.version),
  }));
}

/** Every watched list of `reviews`, on every route. */
function reviewLists(reviews: readonly ReviewsSent[]): KeyedWrite<unknown>[][] {
  return reviews.flatMap((sent) => [sent.answers, sent.unversioned, sent.versioned]);
}

/** How many reviews the watched lists hold together, on every route. */
function reviewCount(reviews: readonly ReviewsSent[]): number {
  return countOf(reviewLists(reviews));
}

// SLOW(>=2s): three approval requests seeded at version 2, one document's read held while `a` is
// pressed, two reviews read back, and the help opened once.
test("k-approval-keys: beside the reading pane a approves the version the request shows once its document is read, Shift+R opens Request changes, and on a question both send nothing", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const approvals: SeededApproval[] = [];
  for (const title of ["Ledger handover", "Mirror replay", "Clock audit"]) {
    approvals.push(await seedApprovalAtVersionTwo("CORE", title));
  }
  const [first, second] = approvals;
  if (first === undefined || second === undefined) {
    throw new Error("the first two requests were not seeded");
  }
  const question = await createAsk(
    first.issueKey,
    { question: "Which ledger does the handover read?" },
    asker
  );
  const reviews = watchReviews(page, approvals);
  const questionAnswers = answerWrites(page, question.id);
  const sent = () => reviewCount(reviews) + writesSent(questionAnswers);
  const requests = () => requestsSent([...reviewLists(reviews), questionAnswers]);
  const step = (text: string) => `k-approval-keys: the reading pane: ${text}`;
  const head = paneHead(page);
  // The Approve that takes a press: the head draws Approve from its first draw and holds it
  // (`aria-disabled`) until the document's read returns.
  const approve = head.getByRole("button", { disabled: false, exact: true, name: "Approve" });
  const dialog = page.getByRole("dialog", { name: "Request changes" });

  // The first request's document read is held until after `a`, so the key is pressed while that
  // read is out.
  const documentPath = `/api/v1/artifacts/${first.artifactId}`;
  const release = await holdReads(page, [`**${documentPath}`]);
  const requested = page.waitForRequest(
    (request) => new URL(request.url()).pathname === documentPath
  );
  await page.setViewportSize(WIDE);
  await page.goto(inboxAt(first.askId));
  await expect(selectedRow(page), step("the first request is selected")).toHaveAttribute(
    "data-inbox-row",
    first.askId
  );
  const firstRow = inboxRow(page, first.askId);
  await firstRow.focus();
  await requested;
  const beforeEarlyA = requests();
  await page.keyboard.press("a");
  await expect(
    approve,
    step("the head's Approve takes no press yet, so a was pressed before the read returned")
  ).toHaveCount(0);
  await release();
  await expect(
    approve,
    step("the read returns and the head's Approve takes a press")
  ).toBeVisible();
  expect(requests(), step("a before the document's read returns sends nothing")).toBe(beforeEarlyA);
  await expect(firstRow, step("the focus is still on the request's row")).toBeFocused();
  await page.keyboard.press("a");
  await expect
    .poll(() => writesSent(reviews[0]?.versioned ?? []), {
      message: step("a sends one review of version 2"),
    })
    .toBe(1);
  expect(firstAttempts(reviews[0]?.versioned ?? [])[0]?.input, step("the review approves")).toEqual(
    { state: "approved" }
  );
  expect(sent(), step("a sends that one review and nothing else")).toBe(1);
  await expect(dialog, step("a opens no dialog")).toHaveCount(0);

  // The second request: Shift+R opens Request changes with the focus in its reason, Escape closes
  // it sending nothing, and a reason sent with Ctrl+Enter is one change request.
  await selectRow(page, second.askId);
  await expect(approve, step("the second request's head's Approve takes a press")).toBeVisible();
  await page.keyboard.press("Shift+R");
  await expect(dialog, step("Shift+R opens Request changes")).toBeVisible();
  await expect(
    dialog.getByLabel("Reason"),
    step("Shift+R puts the focus in the reason field")
  ).toBeFocused();
  const beforeEscape = requests();
  await page.keyboard.press("Escape");
  await expect(dialog, step("Escape closes Request changes")).toHaveCount(0);
  expect(requests(), step("Escape sends nothing")).toBe(beforeEscape);
  await page.keyboard.press("Shift+R");
  await expect(
    dialog.getByLabel("Reason"),
    step("Shift+R again puts the focus in the reason field")
  ).toBeFocused();
  await page.keyboard.type(ROLLBACK_REASON);
  await page.keyboard.press("Control+Enter");
  await expect
    .poll(() => writesSent(reviews[1]?.versioned ?? []), {
      message: step("Ctrl+Enter sends one review of version 2"),
    })
    .toBe(1);
  expect(
    firstAttempts(reviews[1]?.versioned ?? [])[0]?.input,
    step("the review requests changes, with the reason")
  ).toEqual({ reason: ROLLBACK_REASON, state: "changes_requested" });
  expect(sent(), step("Ctrl+Enter sends that one review and nothing else")).toBe(2);

  // On the question there is nothing to approve: both keys send nothing, and `?` shows both not
  // available.
  await selectRow(page, question.id);
  await expect(inboxPane(page).getByTestId(`ask-${question.id}`)).toBeVisible();
  const beforeQuestion = requests();
  await page.keyboard.press("a");
  await page.keyboard.press("Shift+R");
  await expect(dialog, step("on the question Shift+R opens nothing")).toHaveCount(0);
  await page.keyboard.press("?");
  for (const label of ["Approve", "Request changes"]) {
    await expect(
      helpSection(page, "Inbox")
        .getByRole("listitem")
        .filter({ hasText: new RegExp(`^${label}`) }),
      step(`on the question the ? help shows ${label} not available`)
    ).toHaveAttribute("data-enabled", "false");
  }
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toHaveCount(0);
  expect(requests(), step("on the question a and Shift+R send nothing")).toBe(beforeQuestion);
});

/** The steps of k-approval-keys where `place` draws the approval request as a card, with
 *  Approve and Request changes as its options: Shift+R picks Request changes, Escape in its
 *  reason lets go of it, and `a` approves with one press, through the ask's answer. */
async function approvalCardSteps(page: Page, place: "the drawer"): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const approval = await seedApprovalAtVersionTwo("CORE", "Clock audit");
  const reviews = watchReviews(page, [approval]);
  const step = (text: string) => `k-approval-keys: ${place}: ${text}`;
  const inbox = await openInboxAt(page, place, 1);
  const row = rowOf(inbox, approval.askId);
  // Each option's name is its label and the description the server writes under it.
  const requestChanges = row.getByRole("radio", { name: /^Request changes/ });
  await expect(
    row.getByRole("radio", { name: /^Approve/ }),
    step("the request's card draws Approve")
  ).toBeVisible();
  await row.focus();

  const beforeShiftR = requestsSent(reviewLists(reviews));
  await page.keyboard.press("Shift+R");
  await expect(requestChanges, step("Shift+R picks Request changes")).toBeChecked();
  await expect(
    row.locator("[data-ask-answer]"),
    step("Shift+R puts the focus in the reason field")
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(requestChanges, step("Escape lets go of Request changes")).not.toBeChecked();
  await expect(row, step("Escape puts the focus back on the row")).toBeFocused();
  expect(requestsSent(reviewLists(reviews)), step("Shift+R and Escape send nothing")).toBe(
    beforeShiftR
  );

  await page.keyboard.press("a");
  await expect
    .poll(() => writesSent(reviews[0]?.answers ?? []), { message: step("a sends one answer") })
    .toBe(1);
  expect(
    firstAttempts(reviews[0]?.answers ?? [])[0]?.input,
    step("the answer approves")
  ).toMatchObject({
    selected: ["Approve"],
  });
  expect(reviewCount(reviews), step("a sends that one review and nothing else")).toBe(1);
}

/** The steps of k-approval-keys below the reading column's width, where the request is read in
 *  its reading view, whose head draws Approve and Request changes as the reading pane's does
 *  (`ReadingPaneHead`): from the short-row list, whose `a` and Shift+R open the request's view and
 *  press its head's control there once the document's read has enabled it
 *  (`useReadingViewLanding`), and inside the view. Shift+R opens Request changes with the focus in
 *  its reason, Escape closes it sending nothing and gives the focus back to the view's scroller,
 *  and `a` sends one review, approved, of the version the request shows. */
async function approvalViewSteps(
  page: Page,
  place: "the short-row list" | "the reading view"
): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  const approval = await seedApprovalAtVersionTwo("CORE", "Clock audit");
  const reviews = watchReviews(page, [approval]);
  const step = (text: string) => `k-approval-keys: ${place}: ${text}`;
  const dialog = page.getByRole("dialog", { name: "Request changes" });
  const inbox = await openInboxAt(page, place, 1);
  await goTo(inbox, approval.askId);
  // Inside the view the keys press the head's own controls, which take a press once the
  // document's read returns; from the short-row list the key's landing waits for that read.
  if (place === "the reading view") {
    await expect(
      paneHead(page).getByRole("button", { disabled: false, exact: true, name: "Approve" }),
      step("the request's head draws Approve, and it takes a press")
    ).toBeVisible();
  }

  const beforeShiftR = requestsSent(reviewLists(reviews));
  await page.keyboard.press("Shift+R");
  await expect(readingView(page), step("Shift+R reads the request in its view")).toHaveAttribute(
    "data-inbox-reading-row",
    approval.askId
  );
  await expect(dialog, step("Shift+R opens Request changes")).toBeVisible();
  await expect(
    dialog.getByLabel("Reason"),
    step("Shift+R puts the focus in the reason field")
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(dialog, step("Escape closes Request changes")).toHaveCount(0);
  // Inside the view Shift+R was pressed on the scroller, which the box gives the focus back to;
  // from the short-row list the row it was pressed on went with the list, and the landing presses
  // the head's Request changes from the view's scroller, so the box gives the focus back there.
  await expect(
    paneScroller(page),
    step("Escape puts the focus back on the view's scroller, and not on the page's body")
  ).toBeFocused();
  expect(requestsSent(reviewLists(reviews)), step("Shift+R and Escape send nothing")).toBe(
    beforeShiftR
  );

  // `a`, from the request's short row again in the list, and inside the view from its scroller.
  await goTo(inbox, approval.askId);
  await page.keyboard.press("a");
  await expect
    .poll(() => writesSent(reviews[0]?.versioned ?? []), {
      message: step("a sends one review of version 2"),
    })
    .toBe(1);
  expect(firstAttempts(reviews[0]?.versioned ?? [])[0]?.input, step("the review approves")).toEqual(
    { state: "approved" }
  );
  expect(reviewCount(reviews), step("a sends that one review and nothing else")).toBe(1);
}

test("k-approval-keys: from the short-row list at 1024x768 a approves the request with one press in the reading view it opens, and Shift+R opens Request changes there, which Escape closes", async ({
  page,
}) => {
  await approvalViewSteps(page, "the short-row list");
});

test("k-approval-keys: in the reading view at 1024x768 a approves the request in hand with one press, and Shift+R opens Request changes, which Escape closes", async ({
  page,
}) => {
  await approvalViewSteps(page, "the reading view");
});

test("k-approval-keys: in the drawer a approves the request in hand with one press, and Shift+R picks Request changes, whose reason Escape lets go of", async ({
  page,
}) => {
  await approvalCardSteps(page, "the drawer");
});

// ---------------------------------------------------------------------------
// k-approval-keys (R7), today's issue page: `a` and Shift+R press the header's Approve and Request
// changes while the issue's spec has an approval request that waits on the reader, send nothing
// while the focus is on the turn of a question on the Conversation tab (R22's turns take it), and
// send nothing while only another document's request waits - the Verification document's, since
// Dispatch refuses an approval request of a plan (`PLAN_APPROVED_BY_REVIEW`, the plan's D1).
// ---------------------------------------------------------------------------

// SLOW(>=2s): an approval request and a question seeded, the issue page loaded on two tabs and
// one review read back.
test("k-approval-keys: on today's issue page a approves the spec from the header's Approve and Shift+R opens its Request changes while the spec's request waits on the reader, and on the Conversation tab a question's turn holds both back", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger handover";
  const approval = await seedApprovalAtVersionTwo("CORE", title);
  const question = await createAsk(
    approval.issueKey,
    { question: "Which ledger does the handover read?" },
    asker
  );
  const reviews = watchReviews(page, [approval]);
  const requests = () => requestsSent(reviewLists(reviews));
  const step = (text: string) => `k-approval-keys: the issue page: ${text}`;
  const header = page.getByTestId("issue-header");
  const dialog = page.getByRole("dialog", { name: "Request changes" });
  const approve = header.getByRole("button", { disabled: false, exact: true, name: "Approve" });

  // On the Conversation tab, with the focus on the question's turn, the reader is on that turn
  // and not on the header.
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${approval.issueKey}/conversation`);
  await expect(
    approve,
    step("the header draws the spec's Approve on the Conversation tab")
  ).toBeVisible();
  const questionTurn = page.locator(`#issue-conversation-panel [data-turn="ask:${question.id}"]`);
  await questionTurn.focus();
  await expect(questionTurn, step("the question's turn takes the focus")).toBeFocused();
  const beforeTurn = requests();
  await page.keyboard.press("a");
  await page.keyboard.press("Shift+R");
  await expect(dialog, step("on the question's turn Shift+R opens nothing")).toHaveCount(0);
  await expect(questionTurn, step("on the question's turn the focus stays there")).toBeFocused();
  expect(requests(), step("on the question's turn a and Shift+R send nothing")).toBe(beforeTurn);

  await openIssue(page, approval.issueKey, title);
  await expect(approve, step("the header draws the spec's Approve")).toBeVisible();

  const beforeShiftR = requests();
  await page.keyboard.press("Shift+R");
  await expect(dialog, step("Shift+R opens Request changes")).toBeVisible();
  await expect(
    dialog.getByLabel("Reason"),
    step("Shift+R puts the focus in the reason field")
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(dialog, step("Escape closes Request changes")).toHaveCount(0);
  expect(requests(), step("Shift+R and Escape send nothing")).toBe(beforeShiftR);

  await page.keyboard.press("a");
  await expect
    .poll(() => writesSent(reviews[0]?.versioned ?? []), {
      message: step("a sends one review of the spec's version 2"),
    })
    .toBe(1);
  expect(firstAttempts(reviews[0]?.versioned ?? [])[0]?.input, step("the review approves")).toEqual(
    { state: "approved" }
  );
  expect(reviewCount(reviews), step("a sends that one review and nothing else")).toBe(1);
  await expect(dialog, step("a opens no dialog")).toHaveCount(0);
});

test("k-approval-keys: on today's issue page a and Shift+R send nothing while only the Verification document's request waits on the reader, and ? shows both not available", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Mirror replay";
  const issue = await createIssue({ project: "CORE", spec: `# ${title}\n\nThe plan.\n`, title });
  // The spec is approved, so while another document's request is open the header still draws
  // the spec's Request changes beside its pill: a key that read that button alone would act.
  await reviewArtifact(issue.primary_artifact_id, { state: "approved" }, { login: "alice" });
  const verification = await createIssueArtifact(issue.key, {
    content: "# Verification\n\nThe mirror replays the ledger.\n",
    name: "verification.md",
  });
  const requested = withOpenedAsk(
    await requestApproval(verification.artifact.id, { summary: "The checks." }, asker)
  );
  expect(
    requested.ask,
    "k-approval-keys: the Verification document's request waits on the reader"
  ).toMatchObject({ state: "open", waiting_on_person: "alice" });
  const writes = [
    reviewWrites(page, issue.primary_artifact_id),
    versionReviewWrites(page, issue.primary_artifact_id, 1),
    reviewWrites(page, verification.artifact.id),
    versionReviewWrites(page, verification.artifact.id, requested.version),
    answerWrites(page, requested.ask.id),
  ];
  const step = (text: string) =>
    `k-approval-keys: the issue whose Verification document alone waits: ${text}`;
  const header = page.getByTestId("issue-header");
  const dialog = page.getByRole("dialog", { name: "Request changes" });

  await page.setViewportSize(WIDE);
  await openIssue(page, issue.key, title);
  await expect(
    header.getByRole("button", { exact: true, name: "Request changes" }),
    step("the header draws the spec's Request changes")
  ).toBeVisible();
  const beforeKeys = requestsSent(writes);
  await page.keyboard.press("a");
  await page.keyboard.press("Shift+R");
  await expect(dialog, step("Shift+R opens no Request changes")).toHaveCount(0);
  await page.keyboard.press("?");
  for (const label of ["Approve", "Request changes"]) {
    await expect(
      helpSection(page, "Issue")
        .getByRole("listitem")
        .filter({ hasText: new RegExp(`^${label}`) }),
      step(`the ? help shows ${label} not available`)
    ).toHaveAttribute("data-enabled", "false");
  }
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog", { name: "Keyboard shortcuts" })).toHaveCount(0);
  expect(requestsSent(writes), step("a and Shift+R send nothing")).toBe(beforeKeys);
});

// ---------------------------------------------------------------------------
// k-next-item (R8): after the reader snoozes, dismisses, moves back to the Inbox, approves,
// requests changes on or answers the item in hand, the item after it in the list is selected and
// its row has the keyboard focus - the item before it where none came after, and the list's empty
// message where none is left - with the Later band folded and open, beside the reading pane, at
// 1024x768 in the short-row list and in the reading view, and in the drawer.
// ---------------------------------------------------------------------------

/** The word a search narrows k-next-item's list by: the title of the second thread's issue
 *  holds it, and nothing else the list draws does. */
const NARROWING_WORD = "zephyr";

/** The reason k-next-item's request for changes gives. */
const NEXT_ITEM_REASON = "Name the lease owner.";

/** k-next-item's list, by row id, top to bottom as the Inbox draws it. */
interface NextItemSeed {
  /** Two approval requests, each of its own issue's spec at version 2: the first two items. */
  readonly approvals: readonly [SeededApproval, SeededApproval];
  /** A question with two options, after them. */
  readonly question: string;
  /** Asks A, B, C, D and E, in that order, after the question. */
  readonly asks: readonly [string, string, string, string, string];
  /** Two reply threads, the last two items; only the second one's issue says `NARROWING_WORD`. */
  readonly threads: readonly [string, string];
  /** A reply thread the reader has dismissed: in the Dismissed view alone. */
  readonly dismissed: string;
  /** Every row the Inbox lists, top to bottom. */
  readonly order: readonly string[];
}

/** Seeds k-next-item's list bottom first. Every item is on an issue at no priority, so one band
 *  holds them all, newest first: the item made last is the first row. The dismissed thread is
 *  made first, so the Move to Inbox puts it back as the last row. */
async function seedNextItem(): Promise<NextItemSeed> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const thread = async (title: string, root: string): Promise<string> => {
    const issue = await createIssue({ force: true, project: "CORE", title });
    return replyRowId(await seedCommentThread(issue.key, root, "Noted, I will look."));
  };
  const dismissed = await thread("Mirror window", "Which mirror window holds?");
  await dismissThread(dismissed);
  const secondThread = await thread("Zephyr budget", "How far back does a replay go?");
  const firstThread = await thread("Ledger cutover", "How long does a lease last?");
  const asks: string[] = [];
  for (const name of ["Echo", "Delta", "Charlie", "Bravo", "Alpha"]) {
    const issue = await createIssue({ force: true, project: "CORE", title: `${name} rollout` });
    const ask = await createAsk(issue.key, { question: `Ship the ${name} rollout?` }, asker);
    asks.unshift(ask.id);
  }
  const questionIssue = await createIssue({ force: true, project: "CORE", title: "Clock audit" });
  const question = (
    await createAsk(
      questionIssue.key,
      { options: SHIP_OR_HOLD, question: "Ship the clock audit?" },
      asker
    )
  ).id;
  const secondApproval = await seedApprovalAtVersionTwo("CORE", "Quota review");
  const firstApproval = await seedApprovalAtVersionTwo("CORE", "Lease handover");
  const [a, b, c, d, e] = asks;
  if (a === undefined || b === undefined || c === undefined || d === undefined || e === undefined) {
    throw new Error("five asks were seeded");
  }
  return {
    approvals: [firstApproval, secondApproval],
    asks: [a, b, c, d, e],
    dismissed,
    order: [
      firstApproval.askId,
      secondApproval.askId,
      question,
      ...asks,
      firstThread,
      secondThread,
    ],
    question,
    threads: [firstThread, secondThread],
  };
}

/** Opens k-next-item's list where `place` draws it, and holds the list to the order seeded, so
 *  every "the item after" below names the row that came after. */
async function openNextItem(page: Page, place: InboxPlace): Promise<[InboxAt, NextItemSeed]> {
  const seed = await seedNextItem();
  const inbox = await openInboxAt(page, place, seed.order.length);
  expect(inbox.order, `k-next-item: ${place}: the Inbox lists the items as seeded`).toEqual(
    seed.order
  );
  return [inbox, seed];
}

/** Puts the reader on the item `id` (`goTo`), then takes the pointer off every row: the list
 *  holds a row under the pointer where the list moves it, and each step reads where the list
 *  hands the reader once the row they acted on goes. */
async function putOn(inbox: InboxAt, id: string): Promise<void> {
  await goTo(inbox, id);
  await inbox.outside.hover();
}

/** Where an act at 1024x768 from the short-row list leaves the reader: `e` and the bar's acts
 *  after `x` stay in the list, and a key that acts on one item - `b`, `a`, Shift+R, an answer's -
 *  opens that item's reading view and acts there (`openFocused`), so the item after it opens in
 *  the view (Verification v26). */
type Landing = "the list" | "the view";

/** Reads that the item `id` is in hand, for the step `step`. Beside the reading column its row is
 *  the one `aria-current` row and holds the keyboard focus (`document.activeElement`). Where the
 *  act ran in a reading view - inside it, or from the short-row list by a key that opened it
 *  (`landing`) - the view reads the item, its row is the one `aria-current` row, and the view's
 *  scroller holds the focus. In the short-row list and in the drawer no row is selected, and the
 *  item's row holds the focus. */
async function expectInHand(
  inbox: InboxAt,
  id: string,
  step: string,
  landing: Landing = "the list"
): Promise<void> {
  const page = inbox.scope.page();
  const current = inbox.scope.locator('[data-inbox-row][aria-current="true"]');
  const viewed =
    inbox.place === "the reading view" ||
    (inbox.place === "the short-row list" && landing === "the view");
  if (inbox.place === "the reading pane" || viewed) {
    await expect(rowOf(inbox, id), `${step}: the item is selected`).toHaveAttribute(
      "aria-current",
      "true"
    );
    await expect(current, `${step}: no other item is selected`).toHaveCount(1);
  } else {
    await expect(current, `${step}: no row is selected here`).toHaveCount(0);
  }
  if (viewed) {
    await expect(readingView(page), `${step}: the reading view reads the item`).toHaveAttribute(
      "data-inbox-reading-row",
      id
    );
    await expect(
      paneScroller(page),
      `${step}: the focus is on the scroller of the item's reading view`
    ).toBeFocused();
    return;
  }
  await expect(rowOf(inbox, id), `${step}: the item's row has the focus`).toBeFocused();
}

/** Shows the view `label` names, Inbox or Dismissed: from the address on the Inbox page, and from
 *  the drawer's own View menu, since the drawer writes nothing to the address of the page it is
 *  over. */
async function showView(page: Page, inbox: InboxAt, label: "Inbox" | "Dismissed"): Promise<void> {
  if (inbox.place !== "the drawer") {
    await page.goto(label === "Dismissed" ? "/?show=dismissed" : "/");
    return;
  }
  const view = inbox.scope.getByRole("button", { exact: true, name: "View" });
  const menu = page.getByRole("dialog", { name: "View" });
  await view.click();
  await menu.getByRole("group", { name: "Show" }).getByRole("radio", { name: label }).click();
  // The pick leaves the menu open, or closes it where the drawer draws the view anew: the
  // trigger is pressed until the menu is shut, its state read again on each pass.
  await expect(async () => {
    if ((await view.getAttribute("aria-expanded")) === "true") await view.click();
    await expect(menu).toHaveCount(0, { timeout: 1_000 });
  }).toPass();
}

/** k-next-item's snoozes where `place` draws the Inbox: one with the Later band folded, one with
 *  it open, and one of two selected asks. */
async function nextItemSnoozeSteps(page: Page, place: InboxPlace): Promise<void> {
  const [inbox, seed] = await openNextItem(page, place);
  const [, b, c, d, e] = seed.asks;
  const [firstThread] = seed.threads;
  const step = (text: string) => `k-next-item: ${place}: ${text}`;
  const menu = openList(page);

  // With the Later band folded: B, snoozed with b and 1, hands the reader on to C.
  await putOn(inbox, b);
  await page.keyboard.press("b");
  await expect(menu, step("b opens B's Snooze menu")).toBeVisible();
  await page.keyboard.press("1");
  await expectInHand(
    inbox,
    c,
    step("with the Later band folded, a snooze of B hands on to C"),
    "the view"
  );
  await expect(rowOf(inbox, b), step("B folds into Later")).toHaveCount(0);

  // With the Later band open: C, snoozed, hands the reader on to D, and C, drawn in the band, is
  // not the item in hand.
  await showList(inbox);
  await laterBand(inbox, 1).click();
  await expect(rowOf(inbox, b), step("the Later band opens on B")).toBeVisible();
  await putOn(inbox, c);
  await page.keyboard.press("b");
  await expect(menu, step("b opens C's Snooze menu")).toBeVisible();
  await page.keyboard.press("1");
  await expectInHand(
    inbox,
    d,
    step("with the Later band open, a snooze of C hands on to D"),
    "the view"
  );
  await expect(rowOf(inbox, c), step("the snoozed C is drawn in Later")).toHaveAttribute(
    "data-inbox-section",
    "later"
  );

  // D and E selected with x, both snoozed: the item after E, the first thread, is in hand.
  for (const id of [d, e]) {
    await putOn(inbox, id);
    await page.keyboard.press("x");
  }
  const bar = inbox.scope.getByRole("toolbar", { name: "Selected rows" });
  await expect(bar, step("x selects D and E")).toContainText("2 selected");
  await page.keyboard.press("b");
  await expect(openTrigger(page), step("b opens the bar's Snooze menu")).toHaveAccessibleName(
    "Snooze selected asks"
  );
  await page.keyboard.press("1");
  await expectInHand(
    inbox,
    firstThread,
    step("a snooze of D and E, selected, hands on to the item after E")
  );
  await expect(bar, step("the bulk snooze lands and the bar goes")).toHaveCount(0);
}

/** k-next-item's dismissals where `place` draws the Inbox: the Dismissed view's Move to Inbox of
 *  its only thread, a dismiss of the first thread, and a dismiss of the last item. */
async function nextItemDismissSteps(page: Page, place: InboxPlace): Promise<void> {
  const [inbox, seed] = await openNextItem(page, place);
  const [firstThread, secondThread] = seed.threads;
  const step = (text: string) => `k-next-item: ${place}: ${text}`;
  const dismissals = dismissWrites(page);
  const empty = inbox.scope.getByRole("region", { name: "Inbox empty state" });

  // In the Dismissed view, e on its one thread moves it back to the Inbox: no item is left in
  // the view, so its empty message has the focus.
  await showView(page, inbox, "Dismissed");
  await expect(
    inbox.scope.locator("[data-inbox-row]"),
    step("the Dismissed view lists the dismissed thread alone")
  ).toHaveCount(1);
  await putOn(inbox, seed.dismissed);
  await page.keyboard.press("e");
  await expect(empty, step("e in the Dismissed view leaves its empty message")).toHaveText(
    "Nothing dismissed"
  );
  await expect(empty, step("the Dismissed view's empty message has the focus")).toBeFocused();
  expect(writesSent(dismissals), step("e sends one Move to Inbox")).toBe(1);

  // Back in the Inbox, the moved thread is the last item. e on the first thread hands the reader
  // on to the item after it.
  await showView(page, inbox, "Inbox");
  await expect(rowOf(inbox, seed.dismissed), step("the thread is back in the Inbox")).toHaveCount(
    1
  );
  await putOn(inbox, firstThread);
  await page.keyboard.press("e");
  await expectInHand(
    inbox,
    secondThread,
    step("a dismiss of the first thread hands on to the next")
  );

  // The last item of the list: the item before it is in hand.
  await putOn(inbox, seed.dismissed);
  await page.keyboard.press("e");
  await expectInHand(
    inbox,
    secondThread,
    step("a dismiss of the last item hands on to the item before it")
  );
  await expect
    .poll(() => writesSent(dismissals), { message: step("each e sends one write") })
    .toBe(3);
}

/** k-next-item's reviews and answer where `place` draws the Inbox - `a` on the first approval
 *  request, Shift+R on the second, an answer to the question - and the only item a search leaves,
 *  dismissed. */
async function nextItemReviewSteps(page: Page, place: InboxPlace): Promise<void> {
  const [inbox, seed] = await openNextItem(page, place);
  const [firstApproval, secondApproval] = seed.approvals;
  const [a] = seed.asks;
  const [, secondThread] = seed.threads;
  const step = (text: string) => `k-next-item: ${place}: ${text}`;
  const reviews = watchReviews(page, seed.approvals);
  const answers = answerWrites(page, seed.question);
  // Every place but the drawer reads the requests in a pane whose head draws Approve and Request
  // changes: the reading column, and below its width the reading view.
  const pane = place !== "the drawer";
  const head = paneHead(page);
  // From the short-row list each case starts on the item's short row, which draws neither, and
  // its key opens the item's reading view and acts there once the document's read has enabled
  // the head's control (`useReadingViewLanding`).
  const fromRow = place === "the short-row list";

  // `a` approves the first request once its document is read in the reading pane or view, whose
  // head holds its Approve until then; a card draws its Approve at once.
  await putOn(inbox, firstApproval.askId);
  if (!fromRow) {
    await expect(
      pane
        ? head.getByRole("button", { disabled: false, exact: true, name: "Approve" })
        : rowOf(inbox, firstApproval.askId).getByRole("radio", { name: /^Approve/ }),
      step("the first request draws its Approve")
    ).toBeVisible();
  }
  await page.keyboard.press("a");
  await expectInHand(
    inbox,
    secondApproval.askId,
    step("a on the first request hands on to the second"),
    "the view"
  );
  expect(reviewCount(reviews), step("a sends one review")).toBe(1);

  // Shift+R on the second, a reason, and Ctrl+Enter.
  if (fromRow) {
    await putOn(inbox, secondApproval.askId);
  } else {
    await expect(
      pane
        ? head.getByRole("button", { disabled: false, exact: true, name: "Request changes" })
        : rowOf(inbox, secondApproval.askId).getByRole("radio", { name: /^Request changes/ }),
      step("the second request draws its Request changes")
    ).toBeVisible();
  }
  await page.keyboard.press("Shift+R");
  await expect(
    pane
      ? page.getByRole("dialog", { name: "Request changes" }).getByLabel("Reason")
      : rowOf(inbox, secondApproval.askId).locator("[data-ask-answer]"),
    step("Shift+R puts the focus in the reason")
  ).toBeFocused();
  await page.keyboard.type(NEXT_ITEM_REASON);
  await page.keyboard.press("Control+Enter");
  await expectInHand(
    inbox,
    seed.question,
    step("a request for changes on the second request hands on to the question"),
    "the view"
  );
  expect(reviewCount(reviews), step("Ctrl+Enter sends one review more")).toBe(2);

  // The question, answered with 1, Enter and Ctrl+Enter. From the short-row list `1` on its row
  // opens its reading view, whose card it picks Ship on.
  const card = (pane ? inboxPane(page) : rowOf(inbox, seed.question)).getByTestId(
    `ask-${seed.question}`
  );
  if (fromRow) {
    await putOn(inbox, seed.question);
  } else {
    await expect(
      card.locator("[data-ask-answer]"),
      step("the question's card is drawn")
    ).toBeVisible();
  }
  await page.keyboard.press("1");
  await expect(card.getByRole("radio", { name: "Ship" }), step("1 picks Ship")).toBeChecked();
  await page.keyboard.press("Enter");
  await expect(
    card.locator("[data-ask-answer]"),
    step("Enter puts the caret in the answer box")
  ).toBeFocused();
  await page.keyboard.press("Control+Enter");
  await expectInHand(inbox, a, step("an answer to the question hands on to A"), "the view");
  expect(writesSent(answers), step("Ctrl+Enter sends one answer")).toBe(1);

  // A search that leaves one item, the second thread, which e dismisses: the list's empty message
  // has the focus, and the Inbox's keys work from it. Below the column's width the search box is
  // the list's, which a reading view hides with it, so the search is typed on the list; inside
  // the reading view the thread's view is then opened, and its `e` closes it on the message.
  await showList(inbox);
  await page.keyboard.press("f");
  await page.keyboard.type(NARROWING_WORD);
  await expect(
    inbox.scope.locator("[data-inbox-row]"),
    step("the search leaves the second thread alone")
  ).toHaveCount(1);
  await page.keyboard.press("Escape");
  await expect(rowOf(inbox, secondThread), step("Escape puts the focus on its row")).toBeFocused();
  if (place === "the reading view") await goTo(inbox, secondThread);
  await page.keyboard.press("e");
  const empty = inbox.scope.getByRole("region", { name: "Inbox empty state" });
  await expect(empty, step("e on the only item leaves the empty message")).toHaveText(
    "Nothing matches this text"
  );
  await expect(empty, step("the empty message has the focus")).toBeFocused();
  if (place === "the drawer") {
    // In the drawer the row reads the Inbox's own `f` from the empty message, a key of the
    // drawer's dialog scope; the drawer's `?` is k-default-keys' drawer row's.
    await page.keyboard.press("f");
    await expect(
      inbox.scope.getByRole("searchbox", { name: "Search asks" }),
      step("f from the empty message puts the focus in the search box")
    ).toBeFocused();
    return;
  }
  await page.keyboard.press("?");
  await expect(
    page.getByRole("dialog", { name: "Keyboard shortcuts" }),
    step("? from the empty message opens the help")
  ).toBeVisible();
}

/** Snoozes the item in hand until later today with `h`, by whichever control the build draws, so
 *  the row below reaches its case on the merge base too: this tree's `h` opens the Snooze menu,
 *  where `1` picks Later today; the merge base's `h` puts the focus on the column head's native
 *  snooze select, whose Later today is picked as a select's option is. */
async function snoozeLaterToday(page: Page, step: string): Promise<void> {
  await page.keyboard.press("h");
  const menu = openList(page);
  const select = page.locator("select:focus");
  await expect(
    menu.or(select),
    `${step}: h opens the Snooze menu, or puts the focus on the snooze select`
  ).toHaveCount(1);
  // The wait above settled on the menu or the select, so one count tells which one it is.
  if ((await menu.count()) === 0) {
    await select.selectOption({ label: "Later today" });
    return;
  }
  await page.keyboard.press("1");
}

// The check's negative control: at the merge base, with the Later band open, a snooze of the
// selected ask leaves the selection on that ask, now in Later. The case stands first in a row of
// its own, and its snooze takes either build's control, so the base fails it by name.
// SLOW(>=2s): ten items seeded, one snoozed by the API, and one snoozed beside the reading pane.
test("k-next-item: beside the reading pane, with the Later band open, a snooze of the selected ask selects and focuses the item after it, and not the ask now in Later", async ({
  page,
}) => {
  const seed = await seedNextItem();
  const [, b, c, d] = seed.asks;
  const step = (text: string) => `k-next-item: the reading pane: ${text}`;
  // B is in Later before the page opens, so the band is there to open.
  await snoozeAsk(b, new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString());
  const listed = seed.order.filter((id) => id !== b);
  const inbox = await openInboxAt(page, "the reading pane", listed.length);
  expect(
    inbox.order,
    step("the Inbox lists the items as seeded, B in the folded Later band")
  ).toEqual(listed);
  await laterBand(inbox, 1).click();
  await expect(rowOf(inbox, b), step("the Later band opens on B")).toBeVisible();

  await putOn(inbox, c);
  await snoozeLaterToday(page, step("C's snooze"));
  await expectInHand(inbox, d, step("with the Later band open, a snooze of C hands on to D"));
  await expect(rowOf(inbox, c), step("the snoozed C is drawn in Later")).toHaveAttribute(
    "data-inbox-section",
    "later"
  );
});

// SLOW(>=2s): ten items seeded, three snoozes written and read back beside the reading pane.
test("k-next-item: beside the reading pane a snooze, with the Later band folded and open, and of two selected asks, selects and focuses the item after", async ({
  page,
}) => {
  await nextItemSnoozeSteps(page, "the reading pane");
});

// The architect's rulings Q1(b) and Q2(a): a short row's item keys act in its reading view.
// SLOW(>=2s): ten items seeded, three snoozes written and read back from the short rows.
test("k-next-item: in the short-row list at 1024x768 a snooze, with the Later band folded and open, and of two selected asks, hands on to the item after, in its reading view or on its row", async ({
  page,
}) => {
  await nextItemSnoozeSteps(page, "the short-row list");
});

// SLOW(>=2s): ten items seeded, three snoozes written and read back in the reading view.
test("k-next-item: in the reading view at 1024x768 a snooze, with the Later band folded and open, and of two selected asks, opens the item after with the focus on its scroller", async ({
  page,
}) => {
  await nextItemSnoozeSteps(page, "the reading view");
});

// SLOW(>=2s): ten items seeded, three snoozes written and read back in the drawer.
test("k-next-item: in the drawer a snooze, with the Later band folded and open, and of two selected asks, focuses the row of the item after", async ({
  page,
}) => {
  await nextItemSnoozeSteps(page, "the drawer");
});

// SLOW(>=2s): ten items seeded, a thread moved back and two dismissed beside the reading pane.
test("k-next-item: beside the reading pane a dismiss selects and focuses the item after, the item before at the end, and the Dismissed view's empty message once it is empty", async ({
  page,
}) => {
  await nextItemDismissSteps(page, "the reading pane");
});

// The architect's rulings Q1(b) and Q2(a): `e` and the bar's acts stay in the short-row list.
// SLOW(>=2s): ten items seeded, a thread moved back and two dismissed from the short rows.
test("k-next-item: in the short-row list at 1024x768 a dismiss focuses the row of the item after, the item before at the end, and the Dismissed view's empty message once it is empty", async ({
  page,
}) => {
  await nextItemDismissSteps(page, "the short-row list");
});

// SLOW(>=2s): ten items seeded, a thread moved back and two dismissed in the reading view.
test("k-next-item: in the reading view at 1024x768 a dismiss opens the item after with the focus on its scroller, the item before at the end, and the Dismissed view's empty message once it is empty", async ({
  page,
}) => {
  await nextItemDismissSteps(page, "the reading view");
});

// SLOW(>=2s): ten items seeded, a thread moved back and two dismissed in the drawer.
test("k-next-item: in the drawer a dismiss focuses the item after, the item before at the end, and the Dismissed view's empty message once it is empty", async ({
  page,
}) => {
  await nextItemDismissSteps(page, "the drawer");
});

/** How many requests `dismisses` held before the first that names the Inbox row `rowId` among
 *  the threads it takes out (`POST /api/v1/me/inbox/dismiss`'s `threads`): that request's place
 *  in the list, or -1 where none names it. */
function requestsBeforeDismissalOf(
  dismisses: readonly KeyedWrite<unknown>[],
  rowId: string
): number {
  return dismisses.findIndex(
    ({ input }) =>
      typeof input === "object" &&
      input !== null &&
      "threads" in input &&
      Array.isArray(input.threads) &&
      input.threads.includes(rowId)
  );
}

test("k-next-item: beside the reading pane a Dismiss on a row that is not in hand leaves the focus on no other row, so Enter after it dismisses nothing", async ({
  page,
}) => {
  const step = (text: string) => `k-next-item: a Dismiss on a row not in hand: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  const rows: string[] = [];
  for (const name of ["A", "B", "C", "D"]) {
    rows.push(replyRowId(await seedCommentThread(issue.key, `Root ${name}`, `Reply ${name}`)));
  }
  // The list draws the newest first: D, C, B, A.
  const [a, b, c, d] = rows;
  if (a === undefined || b === undefined || c === undefined || d === undefined) {
    throw new Error("the four threads were not seeded");
  }
  const dismisses = dismissWrites(page);
  await page.setViewportSize(WIDE);
  await page.goto(inboxAt(d));
  await expect(selectedRow(page), step("D is the item being read")).toHaveAttribute(
    "data-inbox-row",
    d
  );

  // The pointer's Dismiss on B, a row that is not in hand: the button takes the focus as it is
  // pressed, and goes with B.
  const rowB = inboxRow(page, b);
  await rowB.hover();
  await rowB.getByRole("button", { name: /^Dismiss/ }).click();
  await expect(rowB, step("the Dismiss takes B out")).toHaveCount(0);
  await expect(selectedRow(page), step("D stays the item being read")).toHaveAttribute(
    "data-inbox-row",
    d
  );
  const rowA = inboxRow(page, a);
  await expect(rowA, step("A, the row in B's place, does not take the focus")).not.toBeFocused();
  await expect(
    rowA.locator(":focus"),
    step("no control of A, the row in B's place, takes the focus")
  ).toHaveCount(0);

  // Enter presses a focused button (R18), so a Dismiss of A that held the focus, with no ring
  // drawn after a pointer's press, would take A out too.
  const beforeEnter = requestsSent([dismisses]);
  await page.keyboard.press("Enter");
  // The pointer's Dismiss on C is sent after anything Enter sent, so the requests before C's
  // dismissal are the ones before Enter where Enter sent none; one between the two, a resend of
  // any key included, fails the step.
  const rowC = inboxRow(page, c);
  await rowC.hover();
  await rowC.getByRole("button", { name: /^Dismiss/ }).click();
  await expect(rowC, step("the Dismiss takes C out")).toHaveCount(0);
  expect(writesSent(dismisses), step("the two Dismisses send two dismissals")).toBe(2);
  expect(
    requestsBeforeDismissalOf(dismisses, c),
    step("Enter sends no request: C's dismissal is the first request after it")
  ).toBe(beforeEnter);
  await expect(rowA, step("A, which the reader did not dismiss, is still listed")).toHaveCount(1);
});

// The same Dismiss while the reading pane keeps a question whose row the list does not draw: an
// answer typed and not sent keeps it there once a search hides its row. A thread is no such item:
// nothing keeps a thread past its row, so a search that hides a selected thread moves the
// selection to a row it leaves. With no row of the item in hand to take the focus, the pane's
// scroller takes it, and Enter from there carries the reader into the question's answer box.
test("k-next-item: beside the reading pane a Dismiss on a row that is not in hand, while a search hides the item in hand, leaves the focus on the pane and on no row, so Enter after it dismisses nothing", async ({
  page,
}) => {
  const step = (text: string) => `k-next-item: a Dismiss while a search hides the item: ${text}`;
  const unsent = "Whichever the mirror already holds.";
  await createProject({ key: "CORE", name: "Core" });
  // The question's words hold no "ledger"; each thread's issue title does.
  const questionIssue = await createIssue({ force: true, project: "CORE", title: "Clock audit" });
  const question = (
    await createAsk(questionIssue.key, { question: "Ship the clock audit?" }, asker)
  ).id;
  const issue = await createIssue({ force: true, project: "CORE", title: "Ledger rollout" });
  const rows: string[] = [];
  for (const name of ["A", "B", "C"]) {
    rows.push(replyRowId(await seedCommentThread(issue.key, `Root ${name}`, `Reply ${name}`)));
  }
  // The list draws the newest first: C, B, A, then the question. B, the row dismissed first,
  // stands between two threads, and A moves up into its place.
  const [a, b, c] = rows;
  if (a === undefined || b === undefined || c === undefined) {
    throw new Error("the three threads were not seeded");
  }
  const dismisses = dismissWrites(page);
  await page.setViewportSize(WIDE);
  await page.goto(inboxAt(question));
  await expect(selectedRow(page), step("the question is the item being read")).toHaveAttribute(
    "data-inbox-row",
    question
  );

  // An answer typed and not sent, then a search by the threads' word: the pane keeps the
  // question, and the list draws no row of it.
  const answerBox = inboxPane(page).getByTestId(`ask-${question}`).locator("[data-ask-answer]");
  await answerBox.fill(unsent);
  await page.getByRole("searchbox", { name: "Search asks" }).fill("ledger");
  await expect(
    page.locator("[data-inbox-row]"),
    step("the search lists the three threads alone")
  ).toHaveCount(3);
  await expect(inboxRow(page, question), step("the search hides the question's row")).toHaveCount(
    0
  );
  await expect(answerBox, step("the pane keeps the question and its unsent answer")).toHaveValue(
    unsent
  );

  // The pointer's Dismiss on B, a row that is not in hand: the button takes the focus as it is
  // pressed, and goes with B.
  const rowB = inboxRow(page, b);
  await rowB.hover();
  await rowB.getByRole("button", { name: /^Dismiss/ }).click();
  await expect(rowB, step("the Dismiss takes B out")).toHaveCount(0);
  await expect(
    paneScroller(page),
    step("the pane's scroller, the item being read, has the focus")
  ).toBeFocused();
  await expect(
    page.locator("[data-inbox-row]:focus, [data-inbox-row] :focus"),
    step("no row and no control of a row has the focus")
  ).toHaveCount(0);

  // Enter on the pane carries the reader into the question's answer box (R18); on the Dismiss of
  // A, the row in B's place, it would take A out.
  const beforeEnter = requestsSent([dismisses]);
  await page.keyboard.press("Enter");
  await expect(answerBox, step("Enter puts the caret in the question's answer box")).toBeFocused();
  // The pointer's Dismiss on C is sent after anything Enter sent, so the requests before C's
  // dismissal are the ones before Enter where Enter sent none; one between the two, a resend of
  // any key included, fails the step.
  const rowC = inboxRow(page, c);
  await rowC.hover();
  await rowC.getByRole("button", { name: /^Dismiss/ }).click();
  await expect(rowC, step("the Dismiss takes C out")).toHaveCount(0);
  expect(writesSent(dismisses), step("the two Dismisses send two dismissals")).toBe(2);
  expect(
    requestsBeforeDismissalOf(dismisses, c),
    step("Enter sends no request: C's dismissal is the first request after it")
  ).toBe(beforeEnter);
  await expect(
    inboxRow(page, a),
    step("A, the row in B's place, which the reader did not dismiss, is still listed")
  ).toHaveCount(1);
});

// SLOW(>=2s): ten items seeded, two reviews and an answer written, and a search beside the pane.
test("k-next-item: beside the reading pane an approval, a request for changes and an answer select and focus the item after, and the only item a search leaves hands the focus to the empty message", async ({
  page,
}) => {
  await nextItemReviewSteps(page, "the reading pane");
});

// The architect's rulings Q1(b) and Q2(a): a short row's item keys act in its reading view.
// SLOW(>=2s): ten items seeded, two reviews and an answer written from the short rows, and a
// search.
test("k-next-item: in the short-row list at 1024x768 an approval, a request for changes and an answer open the item after in its reading view, and the only item a search leaves hands the focus to the empty message", async ({
  page,
}) => {
  await nextItemReviewSteps(page, "the short-row list");
});

// SLOW(>=2s): ten items seeded, two reviews and an answer written in the reading view, and a
// search.
test("k-next-item: in the reading view at 1024x768 an approval, a request for changes and an answer open the item after with the focus on its scroller, and the only item a search leaves hands the focus to the empty message", async ({
  page,
}) => {
  await nextItemReviewSteps(page, "the reading view");
});

// SLOW(>=2s): ten items seeded, two reviews and an answer written, and a search in the drawer.
test("k-next-item: in the drawer an approval, a request for changes and an answer focus the item after, and the only item a search leaves hands the focus to the empty message", async ({
  page,
}) => {
  await nextItemReviewSteps(page, "the drawer");
});

// ---------------------------------------------------------------------------
// k-every-action (R11), a first form for the Inbox: every button and menu trigger the Inbox draws
// for an item - on its row, its card, the reading pane's head, the reading view's own line at
// 1024x768 and the bar of selected rows - is an action of the spec's keys table or of the command
// bar's list, and each such action has its key in the `?` help under Inbox, or is a row of ⌘K
// with the item in hand, its key beside it where it has one.
// ---------------------------------------------------------------------------

/** One action of the spec's Design ("The keys" and the command bar's actions), as the page names
 *  it: its label in `?` and in ⌘K, and the key both show beside it, where it has one. */
interface InboxAction {
  readonly label: string;
  readonly key?: string;
}

/** The row's table: the name of each control the Inbox draws for an item, by a pattern, and the
 *  action it is - an action of the spec's keys table, of its list of the command bar's actions,
 *  or one of the four the plan's decision on R11 adds to that list (Change answer, Show previous
 *  versions, Show earlier answers, and Dismiss, or Move to Inbox, the selected items). A key
 *  reads as `?` and ⌘K draw it: `b` or `h` as `borh`, Escape as `Esc`. */
const INBOX_ACTIONS: readonly (readonly [RegExp, InboxAction])[] = [
  [/^Approve( v\d+)?$/, { key: "a", label: "Approve" }],
  [/^Request changes$/, { key: "Shift+R", label: "Request changes" }],
  // The bar's Snooze selected asks is the same action, on every marked ask.
  [/^Snooze /, { key: "borh", label: "Open the snooze menu" }],
  [/^Un-snooze /, { label: "Un-snooze" }],
  [/^Priority of /, { key: "p", label: "Open the priority menu" }],
  [/^Assign \S+ to me$/, { label: "Assign to me" }],
  // The item's own Copy reference, which `y` presses, named apart by its mark (`controlNames`);
  // any other Copy reference `controlNames` reads matches no row of this table.
  [/^item's own: Copy reference/, { key: "y", label: "Copy the row's reference" }],
  [/^(Dismiss|Move to Inbox): /, { key: "e", label: "Dismiss, or Move to Inbox" }],
  [/^Mark read$/, { label: "Mark read" }],
  [/^Show (\d+ earlier|fewer) repl/, { label: "Show earlier replies" }],
  // The asker's button, which names the session by the short form of its id, and the three
  // copies its card of handles holds.
  [new RegExp(`^session:${asker.actor.id.slice(0, 8)}`), { label: "Show the asker's handles" }],
  [/^Copy session ID$/, { label: "Copy session ID" }],
  [/^Copy session title$/, { label: "Copy session title" }],
  [/^Copy tmux target$/, { label: "Copy tmux target" }],
  // The hand-on list's trigger, named by its list and then by the card's badge: whom the ask is
  // asked of, nobody where the server names no person, or the hand-on while it is out.
  [/^Hand this question to (Asked of \S+|Handing on…)$/, { label: "Hand on" }],
  [/^Referenced by/, { label: "Referenced by" }],
  [/^(Show|Hide) diff$/, { label: "Show the earlier approval's diff" }],
  // Answer is Ctrl+Enter in the answer box, and the note's disclosure is Enter's: both are the
  // answer box Enter puts the caret in. Save answer is Answer while an answer is being changed.
  [
    /^(Answer|Save answer|Add a note or answer in your own words)$/,
    { key: "Enter", label: "Answer the ask in hand" },
  ],
  [/^Ask back$/, { label: "Ask back" }],
  // An answer shaped as a question asks which it is: these two, in place of Answer and Ask back.
  [/^Ask back instead$/, { label: "Ask back instead" }],
  [/^Answer with it anyway$/, { label: "Answer with it anyway" }],
  [/^Decline to answer$/, { label: "Decline" }],
  // An answered question's record: Change answer, and its two disclosures; Cancel ends a change.
  [/^Change answer$/, { label: "Change answer" }],
  [/^Cancel$/, { label: "Cancel" }],
  [/^Show \d+ previous versions?$/, { label: "Show previous versions" }],
  [/^Show \d+ earlier answers?$/, { label: "Show earlier answers" }],
  // Recipients names how many it reaches once the ask's thread is read.
  [/^(Recipients|Reaches \d+)$/, { label: "Recipients" }],
  [/^Skip to the answer$/, { label: "Skip to the answer" }],
  [/^Next change$/, { label: "Next change" }],
  [/^(Awaiting approval|Approved|Changes requested)/, { label: "Show the reviews" }],
  [
    /^(Dismiss|Move to Inbox) selected threads$/,
    { label: "Dismiss, or Move to Inbox, the selected items" },
  ],
  [/^Select all$/, { label: "Select all" }],
  [/^Clear selection$/, { key: "Esc", label: "Clear the selection" }],
  // The reading view's Inbox control below 1280 px (`ReadingView`, cybermonklabs/legion#5223):
  // the way back to the list, which Escape is there (`inbox.leave-reading`).
  [/^Back to the Inbox list$/, { key: "Esc", label: "Back to the list" }],
  // A thread's reply box, which `r` puts the caret in: its Send is Ctrl+Enter there, and its own
  // controls are reached by Tab from it. An answered question's reply box under its record
  // (`AskReplyComposer`) is that box too: its submit is Reply, and Replying… while it is out.
  [
    /^(Send|Cancel reply|Reply|Replying…)$/,
    { key: "r", label: "Reply in the thread of the item in hand" },
  ],
];

/** What marks a turn of the thread an item's card or the pane draws: a message or a reply of its
 *  thread body (`ThreadBody`'s turns), and a reply of an ask's thread (`AskThread`). */
const THREAD_TURN = "[data-inbox-thread-turn], [data-turn], [data-reply]";

/** The names of the buttons, menu triggers and disclosures (`<summary>`) `scope` shows that take
 *  a press now, as assistive tech reads them: the elements its `aria-labelledby` names (a single
 *  pick's trigger, named by its list and then what it shows), else its `aria-label`, else its
 *  text - each text without what it hides from assistive tech (the key a control draws beside
 *  its label in the pane's head). A disabled one - Ask back with nothing typed, Answer with
 *  nothing picked - is no action of the item in hand until it takes a press; the question the
 *  reader asks back on reads Ask back once it does.
 *
 *  The item's own Copy reference, inside its `[data-copy-ref="item"]` mark (the one the Inbox's
 *  `y` presses), is named `item's own: ` and then its name. The Copy reference a message or a
 *  reply of the item's thread draws for itself, inside its turn (`THREAD_TURN`), is left out by
 *  that place, not by its name: it acts on that message and not on the item, R22 gives it as a
 *  control Tab reaches, and k-threads proves it (Verification v10). Any other Copy reference is
 *  read under its own name. */
function controlNames(scope: Locator): Promise<string[]> {
  return scope.locator('button, [role="button"], summary').evaluateAll((nodes, turns) => {
    const shown = (element: Element) => {
      const named = element.cloneNode(true) as Element;
      for (const hidden of named.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
      return named.textContent ?? "";
    };
    return nodes
      .filter(
        (node) =>
          node.getClientRects().length > 0 &&
          !node.matches(":disabled") &&
          !(node.closest("[data-copy-ref]") !== null && node.closest(turns) !== null)
      )
      .map((node) => {
        const namedBy = node.getAttribute("aria-labelledby");
        const name =
          namedBy === null
            ? (node.getAttribute("aria-label") ?? shown(node))
            : namedBy
                .split(/\s+/)
                .map((id) => {
                  const part = document.getElementById(id);
                  return part === null ? "" : shown(part);
                })
                .join(" ");
        const spoken = name.replace(/\s+/g, " ").trim();
        return node.closest('[data-copy-ref="item"]') === null ? spoken : `item's own: ${spoken}`;
      });
  }, THREAD_TURN);
}

/** The labels of the `?` help's Inbox section, opened over the item in hand and closed again. */
async function inboxHelpLabels(page: Page): Promise<string[]> {
  let labels: string[] = [];
  await inHelp(page, async () => {
    labels = await helpSection(page, "Inbox")
      .getByRole("listitem")
      .evaluateAll((items) =>
        items.map((item) => item.querySelector("span")?.textContent?.trim() ?? "")
      );
  });
  return labels;
}

/** The text of each row of ⌘K's Actions, its label and the key drawn beside it, opened over the
 *  item in hand and closed again. */
async function commandBarRows(page: Page): Promise<string[]> {
  await page.keyboard.press("Control+k");
  const bar = page.getByRole("dialog", { name: "Search" });
  const rows = bar.getByRole("group", { exact: true, name: "Actions" }).getByRole("option");
  await expect(rows.first()).toBeVisible();
  const texts = await rows.evaluateAll((options) =>
    options.map((option) => (option.textContent ?? "").replace(/\s+/g, ""))
  );
  await page.keyboard.press("Escape");
  await expect(bar).toHaveCount(0);
  return texts;
}

/** Holds every control `names` names to the table, and each action they are to `?` and ⌘K, read
 *  over the item in hand, for the step `step`. */
async function expectEveryAction(
  page: Page,
  names: readonly string[],
  step: (text: string) => string
): Promise<void> {
  const actions = new Map<string, InboxAction>();
  for (const name of names) {
    const action = INBOX_ACTIONS.find(([pattern]) => pattern.test(name))?.[1];
    // Soft, so one run names every control the table does not.
    expect
      .soft(
        action,
        step(`the control "${name}" is an action of the keys table or of the command bar`)
      )
      .toBeDefined();
    if (action !== undefined) actions.set(action.label, action);
  }
  const help = await inboxHelpLabels(page);
  const bar = await commandBarRows(page);
  const squeezed = (text: string) => text.replace(/\s+/g, "");
  for (const action of actions.values()) {
    const listed = bar.includes(squeezed(action.label + (action.key ?? "")));
    expect
      .soft(
        listed || (action.key !== undefined && help.includes(action.label)),
        step(`${action.label}: ? lists its key under Inbox, or ⌘K lists it with the item in hand`)
      )
      .toBe(true);
    if (action.key !== undefined) {
      expect
        .soft(
          bar.includes(squeezed(action.label)),
          step(`${action.label}: ⌘K shows its key ${action.key} beside it`)
        )
        .toBe(false);
    }
  }
}

/** k-every-action's items: a question nobody holds, on an issue whose assignee is cleared, a
 *  question with options the reader can ask back on, an approval request, a snoozed question, a
 *  reply thread and an agent thread; and the issues of the first two, which Assign to me and Ask
 *  back write to. */
interface EveryActionSeed {
  readonly unassigned: string;
  readonly askBack: string;
  readonly approval: string;
  readonly snoozed: string;
  readonly reply: string;
  readonly agent: string;
  readonly issues: { readonly unassigned: string; readonly askBack: string };
}

async function seedEveryAction(): Promise<EveryActionSeed> {
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const question = async (title: string, input: Parameters<typeof createAsk>[1]) => {
    const issue = await createIssue({ force: true, project: "CORE", title });
    return { id: (await createAsk(issue.key, input, asker)).id, issue: issue.key };
  };
  const unassigned = await question("Lease rollout", { question: "Who takes the lease rollout?" });
  // Dispatch gives every issue it creates an assignee - alice, who creates it - and only a write
  // of none clears it (`issue_assignee.go`'s `defaultAssignee`), so Assign to me's write is what
  // makes the reader its assignee.
  await patchIssue(unassigned.issue, { assignee: null });
  const askBack = await question("Mirror replay", {
    options: SHIP_OR_HOLD,
    question: "Ship the mirror replay?",
  });
  const approval = (await seedApprovalAtVersionTwo("CORE", "Clock audit")).askId;
  const snoozed = (await question("Key rotation", { question: "Rotate the keys this week?" })).id;
  await snoozeAsk(snoozed, new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString());
  const threadIssue = await createIssue({ force: true, project: "CORE", title: "Replay budget" });
  const reply = replyRowId(
    await seedCommentThread(threadIssue.key, "How far back does a replay go?", "One hour.")
  );
  const agent = agentRowId(
    await seedAgentThread({ body: "Is the mirror caught up?", replies: ["It is, as of noon."] })
  );
  return {
    agent,
    approval,
    askBack: askBack.id,
    issues: { askBack: askBack.issue, unassigned: unassigned.issue },
    reply,
    snoozed,
    unassigned: unassigned.id,
  };
}

/** The two widths k-every-action reads the Inbox at: beside the reading pane, and at 1024x768,
 *  where it lists short rows and reads the row the reader opens in its reading view in the list's
 *  place (cybermonklabs/legion#5223). */
type EveryActionPlace = "the reading pane" | "at 1024x768";

/** Where `itemControlNames` reads an item's controls: beside the reading column its row and the
 *  column; at 1024x768 its short row in the list, or the reading view the row opens, whose own
 *  line over the pane holds the view's Inbox control (`ReadingView`). */
type ControlPlace = "the reading pane" | "the short row" | "the reading view";

/** Opens the Inbox where `place` draws it over k-every-action's items. The first question's issue
 *  holds nobody, but every project names a default assignee, and a question on an issue nobody
 *  holds waits on its project's (`coalesce(a.asked_of, i.assignee, p.default_assignee)`), so no
 *  seed makes a question nobody holds: the Inbox's reads answer as if the first one were. */
async function openEveryActionInbox(
  page: Page,
  place: EveryActionPlace,
  seed: EveryActionSeed
): Promise<void> {
  await page.route("**/api/v1/inbox", async (route) => {
    const response = await route.fetch();
    const rows = (await response.json()) as { id: string }[];
    await route.fulfill({
      json: rows.map((row) =>
        row.id === seed.unassigned ? { ...row, waiting_on_person: null } : row
      ),
      response,
    });
  });
  await page.setViewportSize(place === "at 1024x768" ? NARROW : WIDE);
  await page.goto("/?assignee=everyone");
}

/** The names of the controls the Inbox draws for the item `id`, which the reader is on, where
 *  `where` reads them: its row, and the pane's head, the item's card and its thread's body, read
 *  once the item's reads are in - beside the reading column both, at 1024x768 the short row in
 *  the list, or the pane the row's reading view draws with the view's own line over it. While a
 *  read is out, the part it draws shows a busy skeleton (`LoadingSkeleton`'s `aria-busy`), and an
 *  `approval` request's state and its Approve wait on its document's read, which the pane starts
 *  only once the selection has rested (`PaneDocument`): so the row waits for no busy skeleton in
 *  the row or the pane, and on an approval request for Approve to take a press. */
async function itemControlNames(
  page: Page,
  where: ControlPlace,
  id: string,
  step: string,
  approval = false
): Promise<string[]> {
  const names: string[] = [];
  if (where !== "the reading view") {
    const row = inboxRow(page, id);
    await expect(row.locator('[aria-busy="true"]'), `${step}: its row's reads are in`).toHaveCount(
      0
    );
    names.push(...(await controlNames(row)));
  }
  if (where === "the short row") return names;
  const pane = inboxPane(page);
  const head = paneHead(page);
  await expect(head, `${step}: its head is drawn`).toBeVisible();
  if (approval) {
    await expect(
      head.getByRole("button", { disabled: false, exact: true, name: "Approve" }),
      `${step}: its document's read is in, and Approve takes a press`
    ).toBeVisible();
  }
  await expect(pane.locator('[aria-busy="true"]'), `${step}: the pane's reads are in`).toHaveCount(
    0
  );
  if (where === "the reading view") {
    names.push(...(await controlNames(readingView(page).locator(":scope > div").first())));
  }
  names.push(...(await controlNames(head)));
  const card = pane.getByTestId(`ask-${id}`);
  // The pane's reads are in, as waited for above, so the card is drawn now where it is one.
  if ((await card.count()) > 0) names.push(...(await controlNames(card)));
  const thread = pane.locator("[data-inbox-thread-body]");
  // The same reads drew any thread body of the item, so this count is the drawn one.
  if ((await thread.count()) > 0) names.push(...(await controlNames(thread.first())));
  return names;
}

/** Every step of k-every-action's first form where `place` draws the Inbox. */
async function everyActionSteps(page: Page, place: EveryActionPlace): Promise<void> {
  const seed = await seedEveryAction();
  const step = (text: string) => `k-every-action: ${place}: ${text}`;
  await openEveryActionInbox(page, place, seed);
  await expect(inboxRow(page, seed.unassigned)).toBeVisible();
  await page.getByRole("button", { name: /^Later \(1\)/ }).click();
  const pane = inboxPane(page);
  const row = (id: string) => inboxRow(page, id);

  const items = [
    [seed.unassigned, "the question nobody holds"],
    [seed.askBack, "the question the reader asks back on"],
    [seed.approval, "the approval request"],
    [seed.snoozed, "the snoozed question"],
    [seed.reply, "the reply thread"],
    [seed.agent, "the agent thread"],
  ] as const;
  for (const [id, what] of items) {
    await expect(row(id), step(`${what} is listed`)).toBeVisible();
    if (place === "at 1024x768") {
      // The short row's own controls, with the focus on the row, in the list.
      await row(id).focus();
      await expectEveryAction(
        page,
        await itemControlNames(page, "the short row", id, step(`${what}'s short row`)),
        (text) => step(`${what}'s short row: ${text}`)
      );
    }
    await selectRow(page, id);
    if (id === seed.askBack) {
      // Ask back takes a press once a question is typed in the answer box.
      await pane
        .getByTestId(`ask-${id}`)
        .locator("[data-ask-answer]")
        .fill("Which format does it ship in?");
    }
    if (place === "the reading pane") {
      await row(id).focus();
      const names = await itemControlNames(page, place, id, step(what), id === seed.approval);
      await expectEveryAction(page, names, (text) => step(`${what}: ${text}`));
      continue;
    }
    // The row's reading view, its own line and its pane, with the focus on its scroller; then
    // the view's Inbox control takes the reader back to the list for the next item.
    await paneScroller(page).focus();
    const names = await itemControlNames(
      page,
      "the reading view",
      id,
      step(`${what}'s reading view`),
      id === seed.approval
    );
    await expectEveryAction(page, names, (text) => step(`${what}'s reading view: ${text}`));
    await backToList(page);
  }

  // With two items marked, the bar of selected rows: beside the reading pane each selected and
  // marked from its row, and at 1024x768 each marked from its short row in the list.
  for (const id of [seed.reply, seed.askBack]) {
    if (place === "the reading pane") await selectRow(page, id);
    await row(id).focus();
    await page.keyboard.press("x");
  }
  const bar = page.getByRole("toolbar", { name: "Selected rows" });
  await expect(bar, step("x marks two items")).toContainText("2 selected");
  await expectEveryAction(page, await controlNames(bar), (text) =>
    step(`the bar of selected rows: ${text}`)
  );
}

/** The question the reader answered twice after its asker reworded it, and the asker's reply
 *  under it, which the Inbox lists as a Reply row: its card draws the question's record, with
 *  Change answer and the disclosures of its earlier wording and its earlier answer. Answers the
 *  row's id. */
async function seedAnsweredThread(): Promise<string> {
  const issue = await createIssue({ force: true, project: "CORE", title: "Replay format" });
  const ask = await createAsk(
    issue.key,
    { options: SHIP_OR_HOLD, question: "Ship the replay format?" },
    asker
  );
  const edited = await editAsk(ask.id, { question: "Ship the replay format this week?" }, asker);
  const first = await answerAsk(ask.id, {
    expected_edited_at: edited.edited_at ?? null,
    selected: ["Ship"],
  });
  // A second answer names the one it replaces, as Change answer does.
  const replaced = first.answer?.at;
  if (replaced === undefined) throw new Error("the first answer came back with no time");
  await answerAsk(ask.id, {
    expected_answer_at: replaced,
    expected_edited_at: edited.edited_at ?? null,
    selected: ["Hold"],
  });
  await createComment(issue.key, { ask_id: ask.id, body: "Holding it, then." }, asker);
  return replyRowId(ask.id);
}

/** Runs the command bar's action `label` over the item in hand as the reader does: ⌘K, then its
 *  row, which closes the bar and hands the focus back before the action runs. */
async function runFromCommandBar(page: Page, label: string, step: string): Promise<void> {
  await page.keyboard.press("Control+k");
  const bar = page.getByRole("dialog", { name: "Search" });
  const action = bar
    .getByRole("group", { exact: true, name: "Actions" })
    .getByRole("option", { exact: true, name: label });
  await expect(action, `${step}: ⌘K lists ${label} with the item in hand`).toHaveCount(1);
  await action.click();
  await expect(bar, `${step}: the command bar closes`).toHaveCount(0);
}

/** A read mark's write: a thread's (`PUT /api/v1/me/threads/{id}/read`) and an Agent row's
 *  session state (`PUT /api/v1/me/agents/{id}/state`). */
const READ_MARK_PATH = /^\/api\/v1\/me\/(threads\/[^/]+\/read|agents\/[^/]+\/state)$/;

/** Every step of k-every-action that runs an action from ⌘K where `place` draws the Inbox -
 *  Decline, Ask back instead, Assign to me, Un-snooze and Skip to the answer, which the reading
 *  column and the reading view draw alike - and reads its effect; at 1024x768 Mark read too, which
 *  only the drawer's thread card draws; and the controls of each state those runs reach, and of an
 *  answered question's record and of the bar of selected rows in the Dismissed view, read through
 *  the table. */
async function everyActionRunSteps(page: Page, place: EveryActionPlace): Promise<void> {
  const seed = await seedEveryAction();
  const answered = await seedAnsweredThread();
  const deep = await seedQuestionColumn("heading");
  const step = (text: string) => `k-every-action: ${place}: ${text}`;
  await openEveryActionInbox(page, place, seed);
  const pane = inboxPane(page);
  const row = (id: string) => inboxRow(page, id);
  const cardOf = (id: string) => pane.getByTestId(`ask-${id}`);
  /** Puts the reader on the item `id`, where the command bar takes the item in hand from: beside
   *  the pane a click selects it and the focus is on its row; at 1024x768 the row's reading view
   *  is opened as a click opens it, with the focus on the view's scroller. */
  const take = async (id: string) => {
    if (place === "the reading pane") {
      await expect(row(id), step(`the item ${id} is listed`)).toBeVisible();
      await selectRow(page, id);
      await row(id).focus();
      return;
    }
    await selectRow(page, id);
    await expect(
      readingView(page),
      step(`the item ${id} is listed, and its reading view opens`)
    ).toHaveAttribute("data-inbox-reading-row", id);
    await paneScroller(page).focus();
  };
  const readThrough = async (id: string, what: string) =>
    expectEveryAction(
      page,
      await itemControlNames(
        page,
        place === "the reading pane" ? place : "the reading view",
        id,
        step(what)
      ),
      (text) => step(`${what}: ${text}`)
    );

  // Decline opens the decline note, with the caret in it; Escape closes it and writes nothing.
  await take(seed.unassigned);
  await runFromCommandBar(page, "Decline", step("Decline"));
  const decline = page.getByRole("dialog", { name: "Decline to answer" });
  await expect(decline, step("Decline opens the decline note")).toBeVisible();
  await expect(
    decline.getByLabel("Note (optional)"),
    step("Decline puts the caret in the note")
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(decline, step("Escape closes the decline note")).toHaveCount(0);

  // Assign to me makes the reader the issue's assignee. The issue holds nobody before it, so the
  // assignee read after it is the write's.
  expect(
    (await getIssue(seed.issues.unassigned)).assignee,
    step("Assign to me: the issue holds nobody before it")
  ).toBeNull();
  const assigns = issueWrites(page, seed.issues.unassigned);
  await take(seed.unassigned);
  await runFromCommandBar(page, "Assign to me", step("Assign to me"));
  await expect
    .poll(() => writesSent(assigns), {
      message: step("Assign to me writes the issue's assignee"),
    })
    .toBe(1);
  expect(firstAttempts(assigns)[0]?.input, step("Assign to me names the reader")).toEqual({
    assignee: "alice",
  });
  await expect
    .poll(async () => (await getIssue(seed.issues.unassigned)).assignee, {
      message: step("Assign to me: the issue's assignee is the reader"),
    })
    .toBe("alice");

  // Un-snooze takes the snoozed question out of Later, whose band the list draws.
  if (place === "at 1024x768") await backToList(page);
  await page.getByRole("button", { name: /^Later \(1\)/ }).click();
  const unsnoozes = unsnoozeWrites(page, seed.snoozed);
  await take(seed.snoozed);
  await runFromCommandBar(page, "Un-snooze", step("Un-snooze"));
  await expect
    .poll(() => writesSent(unsnoozes), { message: step("Un-snooze sends one write") })
    .toBe(1);
  await expect(row(seed.snoozed), step("Un-snooze: the ask leaves Later")).not.toHaveAttribute(
    "data-inbox-section",
    "later"
  );

  // Mark read is drawn by the drawer's thread card alone (`ThreadRowCard`): the reading view at
  // 1024x768, as the column at 1440x900, reads a thread as it opens it and draws no Mark read
  // (cybermonklabs/legion#5223), and ⌘K opens over no dialog, the drawer's among them (keymap.ts's
  // `consulted`). So at 1024x768 the drawer's card's Mark read is pressed from the keyboard, Tab
  // to it and Enter, over a project page at 1440x900, the drawer rows' width; then the reading
  // view of the thread in hand draws no Mark read, and ⌘K lists none. The card's thread also
  // marks itself read, with the same write, once its newest message has been on screen for a
  // second (`useMarkThreadRead`), which the walk to Mark read can outlast: every read mark is
  // held while the step reads Mark read's own, which its button names while it is out, so the
  // dwell's mark can neither stand in for it nor count against it.
  if (place === "at 1024x768") {
    const marks = await holdWrites(page, "PUT", READ_MARK_PATH);
    await page.setViewportSize(WIDE);
    const peek = await openDrawer(page, step("i opens the drawer"));
    const card = peek.locator(`[data-inbox-row="${seed.reply}"]`);
    const unread = card.locator("[data-inbox-thread-unread]");
    const markRead = card.locator('[data-inbox-action="mark-read"]');
    await expect(
      unread.first(),
      step("the reply thread's card in the drawer shows its reply unread")
    ).toBeVisible();
    await card.focus();
    await tabTo(page, markRead, step("in the drawer, Tab reaches the card's Mark read"));
    await page.keyboard.press("Enter");
    await expect(markRead, step("Enter on Mark read sends its read mark")).toHaveText(
      "Marking read…"
    );
    marks.release();
    await expect(unread, step("Mark read: the thread reads as read")).toHaveCount(0);

    await page.setViewportSize(NARROW);
    await page.goto("/?assignee=everyone");
    await take(seed.reply);
    await expect(
      pane.locator('[data-inbox-action="mark-read"]'),
      step("the reply thread's reading view draws no Mark read")
    ).toHaveCount(0);
    expect(
      (await commandBarRows(page)).filter((text) => text.startsWith("Markread")),
      step("⌘K lists no Mark read for the thread in hand")
    ).toEqual([]);
  }

  // An answer shaped as a question asks which it is, and Ask back instead sends it back.
  const backs = watchWrites(
    page,
    "POST",
    `/api/v1/issues/${encodeURIComponent(seed.issues.askBack)}/comments`
  );
  await take(seed.askBack);
  const question = "Which format does it ship in?";
  const box = cardOf(seed.askBack).locator("[data-ask-answer]");
  await box.fill(question);
  await box.press("Control+Enter");
  await expect(
    cardOf(seed.askBack).getByRole("button", { exact: true, name: "Ask back instead" }),
    step("an answer shaped as a question offers Ask back instead")
  ).toBeVisible();
  await readThrough(seed.askBack, "the question answered with a question");
  await runFromCommandBar(page, "Ask back instead", step("Ask back instead"));
  await expect
    .poll(() => writesSent(backs), { message: step("Ask back instead sends the question back") })
    .toBe(1);
  expect(
    firstAttempts(backs)[0]?.input,
    step("Ask back instead sends the question to its asker")
  ).toEqual({
    ask_id: seed.askBack,
    body: question,
  });

  // The answered question's record, then a change of its answer, which Cancel ends.
  await take(answered);
  await readThrough(answered, "the answered question");
  await runFromCommandBar(page, "Change answer", step("Change answer"));
  await expect(
    page.getByRole("button", { exact: true, name: "Cancel" }),
    step("Change answer begins a change, which Cancel ends")
  ).toBeVisible();
  await readThrough(answered, "the answer being changed");
  await runFromCommandBar(page, "Cancel", step("Cancel"));
  await expect(
    page.getByRole("button", { exact: true, name: "Cancel" }),
    step("Cancel ends the change")
  ).toHaveCount(0);

  // Skip to the answer puts the column back at the question's block. In the reading view the
  // document's own `g g` acts from the page's body (`scrollsFromList`), the browser's own keys
  // being the scroller's, so the focus is let go there first.
  await take(deep.item);
  if (place === "at 1024x768") {
    await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur());
  }
  const skip = pane.getByRole("button", { name: "Skip to the answer" });
  await expect(
    skip,
    step("the question far down a document offers Skip to the answer")
  ).toBeVisible();
  await waitOutPlacement(page);
  const block = paneScroller(page).locator(`[data-dispatch-ask-block="${deep.blockId}"]`);
  await page.keyboard.press("g");
  await page.keyboard.press("g");
  await expect(
    block,
    step("g g puts the column at its top, above the answer")
  ).not.toBeInViewport();
  await runFromCommandBar(page, "Skip to the answer", step("Skip to the answer"));
  await expect(block, step("Skip to the answer moves the column to the answer")).toBeInViewport();

  // The bar of selected rows in the Dismissed view, where it moves them back to the Inbox.
  await dismissThread(seed.reply);
  await dismissThread(seed.agent);
  await page.goto("/?show=dismissed&assignee=everyone");
  for (const id of [seed.reply, seed.agent]) {
    await take(id);
    await page.keyboard.press("x");
  }
  const bar = page.getByRole("toolbar", { name: "Selected rows" });
  await expect(bar, step("x marks two dismissed threads")).toContainText("2 selected");
  await expectEveryAction(page, await controlNames(bar), (text) =>
    step(`the bar of selected rows in the Dismissed view: ${text}`)
  );
}

// SLOW(>=2s): six items of four kinds seeded, each read with ? and ⌘K, then two marked.
test("k-every-action: beside the reading pane every control of an item is a key or a command bar action of the item in hand", async ({
  page,
}) => {
  await everyActionSteps(page, "the reading pane");
});

// SLOW(>=2s): six items of four kinds seeded, each read on its short row and in its reading view
// with ? and ⌘K, then two marked.
test("k-every-action: at 1024x768 every control of an item, on its short row and in its reading view, is a key or a command bar action of the item in hand", async ({
  page,
}) => {
  await everyActionSteps(page, "at 1024x768");
});

// SLOW(>=2s): eight items seeded, seven actions run from ⌘K with their effects read back, and the
// Dismissed view loaded.
test("k-every-action: beside the reading pane Decline, Ask back instead, Assign to me, Un-snooze and Skip to the answer run from the command bar on the item in hand", async ({
  page,
}) => {
  await everyActionRunSteps(page, "the reading pane");
});

// SLOW(>=2s): eight items seeded, seven actions run from ⌘K in the reading view with their
// effects read back, the drawer's Mark read pressed, and the Dismissed view loaded.
test("k-every-action: at 1024x768 Decline, Ask back instead, Assign to me, Un-snooze and Skip to the answer run from the command bar in the reading view on the item in hand, and Mark read from the keyboard in the drawer", async ({
  page,
}) => {
  await everyActionRunSteps(page, "at 1024x768");
});

// SLOW(>=2s): the command bar opened six times over a project page, each row followed.
test("k-every-action: on a project page the command bar goes to every page the navigation always shows", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const bar = page.getByRole("dialog", { name: "Search" });
  for (const [label, address] of [
    ["Go to Inbox", "/"],
    ["Go to Agents", "/agents"],
    ["Go to Priorities", "/priorities"],
    ["Go to Delivery", "/delivery"],
    ["Go to Pipeline", "/pipeline"],
    ["Go to Settings", "/settings"],
  ] as const) {
    await page.goto("/projects/CORE");
    await expect(page.getByRole("button", { exact: true, name: "Board" })).toBeVisible();
    await page.locator("body").focus();
    await page.keyboard.press("Control+k");
    const go = bar
      .getByRole("group", { exact: true, name: "Actions" })
      .getByRole("option", { name: new RegExp(`^${label}`) });
    await expect(go, `k-every-action: on a project page ⌘K lists ${label}`).toHaveCount(1);
    await go.click();
    await expect(page, `k-every-action: ${label} moves the page to ${address}`).toHaveURL(
      new RegExp(`${address}$`)
    );
  }
});

// ---------------------------------------------------------------------------
// k-pane-head (R9): the controls of the reading pane's head in a row of their own above the
// title, each in one place whatever the title's length and whenever the document's read returns,
// and each showing its key.
// ---------------------------------------------------------------------------

/** A title of one character, and one of 300, the longest k-pane-head asks for (the server takes
 *  1,000). */
const SHORT_TITLE = "Q";
const LONG_TITLE = `${"The mirror replays the ledger on a cold start, window by window, and hands the lease back. ".repeat(4).slice(0, 299)}.`;

/** A box in the reading pane, in px from the pane's own top left corner. */
interface PaneRect {
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
  readonly top: number;
}

/** Where `box` stands in the reading pane: its own box, relative to the pane's. */
function paneRectOf(box: Locator): Promise<PaneRect> {
  return box.evaluate((node) => {
    const pane = node.closest('[data-testid="inbox-pane"]');
    if (pane === null) throw new Error("the box is not in the reading pane");
    const own = node.getBoundingClientRect();
    const frame = pane.getBoundingClientRect();
    return {
      bottom: own.bottom - frame.top,
      left: own.left - frame.left,
      right: own.right - frame.left,
      top: own.top - frame.top,
    };
  });
}

/** A control of the pane's head: its name in the rows' messages, the key it shows where it has
 *  one, and how the head draws it. */
interface HeadControl {
  readonly name: string;
  readonly key?: string;
  readonly find: (head: Locator) => Locator;
}

const APPROVE: HeadControl = {
  find: (head) => head.getByRole("button", { exact: true, name: "Approve" }),
  key: "a",
  name: "Approve",
};
const REQUEST_CHANGES: HeadControl = {
  find: (head) => head.getByRole("button", { exact: true, name: "Request changes" }),
  key: "Shift+R",
  name: "Request changes",
};
const OPEN_PAGE: HeadControl = {
  find: (head) => head.getByRole("link", { exact: true, name: "Open page" }),
  name: "Open page",
};
const PRIORITY: HeadControl = {
  find: (head) => head.getByRole("button", { name: /^Priority of / }),
  key: "p",
  name: "the priority",
};
const SNOOZE: HeadControl = {
  find: (head) => head.getByRole("button", { name: /^Snooze / }),
  key: "b",
  name: "Snooze",
};
const COPY_REFERENCE: HeadControl = {
  find: (head) => head.getByRole("button", { name: /^Copy reference / }),
  key: "y",
  name: "Copy reference",
};
const DISMISS: HeadControl = {
  find: (head) => head.getByRole("button", { name: /^Dismiss: / }),
  key: "e",
  name: "Dismiss",
};

/** The approval's state, which `ApprovalChip` draws in the head once the document's read returns
 *  (`data-inbox-action="show-reviews"`). */
const SHOW_REVIEWS: HeadControl = {
  find: (head) => head.locator('[data-inbox-action="show-reviews"]'),
  name: "the approval's state",
};

/** The controls of an approval request's head, Approve first, which the check's negative control
 *  names; the same once the document's read is in, with the approval's state; of a question's;
 *  and of a reply thread's, whose Open page is what `o` opens. */
const APPROVAL_CONTROLS = [APPROVE, REQUEST_CHANGES, OPEN_PAGE, PRIORITY, SNOOZE, COPY_REFERENCE];
const READ_APPROVAL_CONTROLS = [...APPROVAL_CONTROLS, SHOW_REVIEWS];
const QUESTION_CONTROLS = [OPEN_PAGE, PRIORITY, SNOOZE, COPY_REFERENCE];
const THREAD_CONTROLS = [{ ...OPEN_PAGE, key: "o" }, COPY_REFERENCE, DISMISS];

/** The reading pane's head, by the element it has been at every build. */
function paneHead(page: Page): Locator {
  return inboxPane(page).locator("header").first();
}

/** What a row reads of the head of the item the pane shows: the box of each control, by name,
 *  and the title's heading, which the merge base does not draw. */
interface HeadReading {
  readonly controls: ReadonlyMap<string, PaneRect>;
  readonly title:
    | { readonly rect: PaneRect; readonly text: string; readonly whole: boolean }
    | undefined;
}

async function readHead(page: Page, controls: readonly HeadControl[]): Promise<HeadReading> {
  const head = paneHead(page);
  const rects = new Map<string, PaneRect>();
  for (const control of controls) {
    rects.set(control.name, await paneRectOf(control.find(head)));
  }
  const title = await head.evaluate((node) => {
    const heading = node.querySelector("h2");
    const pane = node.closest('[data-testid="inbox-pane"]');
    if (heading === null || pane === null) return undefined;
    const own = heading.getBoundingClientRect();
    const frame = pane.getBoundingClientRect();
    // Whole: nothing in the heading is wider than its own box, and nothing draws an ellipsis.
    const cut = [heading, ...heading.querySelectorAll("*")].some(
      (element) =>
        element.scrollWidth > element.clientWidth ||
        getComputedStyle(element).textOverflow === "ellipsis"
    );
    return {
      rect: {
        bottom: own.bottom - frame.top,
        left: own.left - frame.left,
        right: own.right - frame.left,
        top: own.top - frame.top,
      },
      text: heading.textContent ?? "",
      whole: !cut,
    };
  });
  return { controls: rects, title };
}

/** Holds each of `controls` to the same left and top, within 1 px, in two readings, for the step
 *  `step`. */
function expectSamePlaces(
  before: HeadReading,
  after: HeadReading,
  controls: readonly HeadControl[],
  step: (text: string) => string
): void {
  for (const { name } of controls) {
    const was = before.controls.get(name);
    const is = after.controls.get(name);
    if (was === undefined || is === undefined) throw new Error(`${name} was not read twice`);
    expect(Math.abs(is.left - was.left), step(`${name}'s left is the same`)).toBeLessThanOrEqual(1);
    expect(Math.abs(is.top - was.top), step(`${name}'s top is the same`)).toBeLessThanOrEqual(1);
  }
}

/** Holds a reading's title to a heading of its own holding `title` whole, under every control
 *  read with it, for the step `step`. */
function expectTitleWhole(
  reading: HeadReading,
  title: string,
  step: (text: string) => string
): void {
  const heading = reading.title;
  expect(heading, step("the title is a heading of its own")).toBeDefined();
  if (heading === undefined) return;
  expect(heading.text, step("the heading holds the whole title")).toContain(title);
  expect(heading.whole, step("the title is drawn whole, with nothing cut or elided")).toBe(true);
  for (const [name, rect] of reading.controls) {
    expect(rect.bottom, step(`${name} is above the title`)).toBeLessThanOrEqual(
      heading.rect.top + 1
    );
  }
}

/** Holds each of `controls` that has a key to showing it, for the step `step`. */
async function expectKeysShown(
  page: Page,
  controls: readonly HeadControl[],
  step: (text: string) => string
): Promise<void> {
  for (const { find, key, name } of controls) {
    if (key === undefined) continue;
    await expectKeyHint(find(paneHead(page)), key, step(`${name} shows ${key}`));
  }
}

/** Opens the item `id` in the reading pane and waits for `drawn`, the control that says its head
 *  is the one the row reads. */
async function openInPane(
  page: Page,
  id: string,
  drawn: Locator,
  step: (text: string) => string
): Promise<void> {
  await page.goto(inboxAt(id));
  await expect(selectedRow(page), step("the item is selected")).toHaveAttribute(
    "data-inbox-row",
    id
  );
  await expect(drawn, step("its head is drawn")).toBeVisible();
}

// SLOW(>=2s): five items seeded, each opened in the reading pane and its head read control by
// control.
test("k-pane-head: on approval requests and questions whose titles are 1 and 300 characters, and on a thread, each control of the head keeps one place above the whole title and shows its key", async ({
  page,
}) => {
  expect(LONG_TITLE).toHaveLength(300);
  await createProject({ key: "CORE", name: "Core" });
  // Each pair shows the same controls: no issue has a priority, and each is the project's
  // default assignee's, so neither draws Assign to me.
  const approvals: string[] = [];
  const questions: string[] = [];
  for (const title of [SHORT_TITLE, LONG_TITLE]) {
    const forApproval = await createIssue({
      force: true,
      project: "CORE",
      spec: "# Rollout\n\nThe mirror hands the lease back.\n",
      title,
    });
    const requested = withOpenedAsk(
      await requestApproval(forApproval.primary_artifact_id, { summary: "The rollout." }, asker)
    );
    approvals.push(requested.ask.id);
    const forQuestion = await createIssue({ force: true, project: "CORE", title });
    questions.push(
      (await createAsk(forQuestion.key, { question: "Which window does it replay?" }, asker)).id
    );
  }
  const threadIssue = await createIssue({ force: true, project: "CORE", title: "Lease handover" });
  const thread = replyRowId(
    await seedCommentThread(threadIssue.key, "Who holds the lease?", "The mirror does.")
  );
  await page.setViewportSize(WIDE);
  const head = paneHead(page);

  let requestChangesRight: number | undefined;
  for (const [what, ids, controls, drawn] of [
    [
      "the approval requests",
      approvals,
      READ_APPROVAL_CONTROLS,
      head.getByRole("button", { disabled: false, exact: true, name: "Request changes" }),
    ],
    ["the questions", questions, QUESTION_CONTROLS, SNOOZE.find(head)],
  ] as const) {
    const step = (text: string) => `k-pane-head: ${what}: ${text}`;
    const [short, long] = ids;
    if (short === undefined || long === undefined) throw new Error(`${what} were not seeded`);
    await openInPane(page, short, drawn, step);
    // Waited for here, within `expectTimeout`, where `readHead`'s first read of the priority, an
    // `evaluate`, would wait out its timeout for a head that draws no such button, so the base
    // fails the row by name.
    await expect(PRIORITY.find(head), step("the head draws its priority control")).toBeVisible();
    const shortHead = await readHead(page, controls);
    await openInPane(page, long, drawn, step);
    const longHead = await readHead(page, controls);
    expectSamePlaces(shortHead, longHead, controls, (text) =>
      step(`for the 1- and the 300-character title, ${text}`)
    );
    expectTitleWhole(shortHead, SHORT_TITLE, (text) => step(`the 1-character title: ${text}`));
    expectTitleWhole(longHead, LONG_TITLE, (text) => step(`the 300-character title: ${text}`));
    await expectKeysShown(page, controls, step);
    requestChangesRight ??= longHead.controls.get(REQUEST_CHANGES.name)?.right;
  }

  // The thread draws Dismiss at the end of the row, where Request changes ends.
  const step = (text: string) => `k-pane-head: the reply thread: ${text}`;
  await openInPane(page, thread, DISMISS.find(head), step);
  const threadHead = await readHead(page, THREAD_CONTROLS);
  if (requestChangesRight === undefined) throw new Error("Request changes was not read");
  expect(
    Math.abs((threadHead.controls.get(DISMISS.name)?.right ?? 0) - requestChangesRight),
    step("Dismiss's right edge is Request changes' right edge")
  ).toBeLessThanOrEqual(1);
  expectTitleWhole(threadHead, threadIssue.key, step);
  await expectKeysShown(page, THREAD_CONTROLS, step);
});

/** How many buttons of the reading pane's head Chromium's own accessibility tree, the one a
 *  screen reader reads, names exactly `name` (`Accessibility.queryAXTree`, which computes each
 *  name in the browser). */
async function headButtonsNamedByChromium(page: Page, name: string): Promise<number> {
  const cdp = await page.context().newCDPSession(page);
  try {
    const { root } = await cdp.send("DOM.getDocument", { depth: 0 });
    const { nodeId } = await cdp.send("DOM.querySelector", {
      nodeId: root.nodeId,
      selector: '[data-testid="inbox-pane"] header',
    });
    const { nodes } = await cdp.send("Accessibility.queryAXTree", {
      accessibleName: name,
      nodeId,
      role: "button",
    });
    return nodes.filter((node) => node.ignored !== true).length;
  } finally {
    await cdp.detach();
  }
}

// The key a control draws is CSS generated content, which a browser puts into a name computed
// from the content, and the hint's `aria-hidden` is what keeps it out: here each name is read as
// assistive tech gets it, by Playwright's name and by Chromium's own accessibility tree. Approve
// and Request changes are named by their content, so a key that reached either name would show
// in it; the priority is named for its menu and then the priority it shows, not for its key.
test("k-pane-head: a control of the head is named without the key it shows, Approve and Request changes by their labels alone and the priority by its menu and then its priority", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    force: true,
    project: "CORE",
    spec: "# Rollout\n\nThe mirror hands the lease back.\n",
    title: "Ledger rollout",
  });
  await patchIssue(issue.key, { priority: 2 });
  const requested = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary: "The rollout." }, asker)
  );
  await page.setViewportSize(WIDE);
  const head = paneHead(page);
  const step = (text: string) => `k-pane-head: the names: ${text}`;
  await openInPane(
    page,
    requested.ask.id,
    head.getByRole("button", { disabled: false, exact: true, name: "Request changes" }),
    step
  );
  for (const [name, key] of [
    ["Approve", "a"],
    ["Request changes", "Shift+R"],
    [`Priority of ${issue.key} P2`, "p"],
  ] as const) {
    const control = head.getByRole("button", { exact: true, name });
    await expect(control, step(`a button is named "${name}", without its key`)).toBeVisible();
    await expectKeyHint(control, key, step(`"${name}" shows ${key}`));
    await expect
      .poll(() => headButtonsNamedByChromium(page, name), {
        message: step(`Chromium's accessibility tree names one button "${name}"`),
      })
      .toBe(1);
  }
});

test("k-pane-head: no control of the head moves when the document's read of an approval request returns", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: "# Ledger handover\n\nThe plan.\n",
    title: "Ledger handover",
  });
  const request = withOpenedAsk(
    await requestApproval(issue.primary_artifact_id, { summary: "The handover." }, asker)
  );
  await page.setViewportSize(WIDE);
  const head = paneHead(page);
  const step = (text: string) => `k-pane-head: the late read: ${text}`;
  const path = `/api/v1/artifacts/${issue.primary_artifact_id}`;
  const release = await holdReads(page, [`**${path}`]);
  const requested = page.waitForRequest((sent) => new URL(sent.url()).pathname === path);
  await page.goto(inboxAt(request.ask.id));
  await expect(selectedRow(page), step("the request is selected")).toHaveAttribute(
    "data-inbox-row",
    request.ask.id
  );
  await requested;
  for (const control of [APPROVE, REQUEST_CHANGES]) {
    await expect(
      control.find(head),
      step(`${control.name} is drawn before the read returns`)
    ).toBeVisible();
  }
  const before = await readHead(page, APPROVAL_CONTROLS);
  await release();
  await expect(
    head.getByRole("button", { disabled: false, exact: true, name: "Approve" }),
    step("the read returns and Approve takes a press")
  ).toBeVisible();
  expectSamePlaces(before, await readHead(page, APPROVAL_CONTROLS), APPROVAL_CONTROLS, step);
});

test("k-pane-head: on a request of a document approved at its latest version, the place at the row's end keeps its box and no other control moves when the read returns", async ({
  page,
}) => {
  // A request of version 2 of a document approved at version 1, which the row's read then
  // answers as approved at version 2: no route of the API leaves an open request of a version
  // already approved.
  const approved = await seedApprovalWithMarks();
  const path = `/api/v1/artifacts/${approved.artifactId}`;
  await page.setViewportSize(WIDE);
  const head = paneHead(page);
  const step = (text: string) => `k-pane-head: the approved document: ${text}`;
  const held = Promise.withResolvers<void>();
  await page.route(`**${path}`, async (route) => {
    await held.promise;
    // A read the page gave up on while it was held is answered already, and nobody waits on it.
    await route
      .fetch()
      .then(async (response) => {
        const read = (await response.json()) as { approval?: { latest_version: number } };
        await route.fulfill({
          json:
            read.approval === undefined
              ? read
              : {
                  ...read,
                  approval: {
                    ...read.approval,
                    state: "approved",
                    version: read.approval.latest_version,
                  },
                },
          response,
        });
      })
      .catch(() => undefined);
  });
  const requested = page.waitForRequest((sent) => new URL(sent.url()).pathname === path);
  await page.goto(inboxAt(approved.approvalAskId));
  await expect(selectedRow(page), step("the request is selected")).toHaveAttribute(
    "data-inbox-row",
    approved.approvalAskId
  );
  await requested;
  const place = head.locator("[data-inbox-head-place]");
  await expect(place, step("the head draws its place before the read returns")).toBeVisible();
  const placeBefore = await paneRectOf(place);
  const before = await readHead(page, APPROVAL_CONTROLS);
  held.resolve();
  await expect(
    head.getByRole("button", { name: /^Approved v/ }),
    step("the read returns the document approved at its latest version")
  ).toBeVisible();
  await expect(APPROVE.find(head), step("an approved document draws no Approve")).toHaveCount(0);
  const placeAfter = await paneRectOf(place);
  for (const side of ["left", "top", "right", "bottom"] as const) {
    expect(
      Math.abs(placeAfter[side] - placeBefore[side]),
      step(`the place's ${side} edge is the same`)
    ).toBeLessThanOrEqual(1);
  }
  const drawnBoth = APPROVAL_CONTROLS.filter((control) => control !== APPROVE);
  expectSamePlaces(before, await readHead(page, drawnBoth), drawnBoth, step);
});

// SLOW(>=2s): a request whose version changed six paragraphs, its baseline's read held while the
// head is read the first time.
test("k-pane-head: no control drawn before Next change moves when Next change appears", async ({
  page,
}) => {
  const seed = await seedApprovalWithMarks();
  const step = (text: string) => `k-pane-head: Next change: ${text}`;
  // The version the request is marked against: until it is read, nothing is marked.
  const release = await holdReads(page, [`**/api/v1/artifacts/${seed.artifactId}/versions/1`]);
  await page.setViewportSize(WIDE);
  const head = paneHead(page);
  const nextChange = head.getByRole("button", { exact: true, name: "Next change" });
  await openInPane(
    page,
    seed.approvalAskId,
    head.getByRole("button", { disabled: false, exact: true, name: "Request changes" }),
    step
  );
  await expect(nextChange, step("nothing is marked yet, so no Next change is drawn")).toHaveCount(
    0
  );
  const before = await readHead(page, READ_APPROVAL_CONTROLS);
  await release();
  await expect(nextChange, step("the version is marked and Next change appears")).toBeVisible();
  const after = await readHead(page, READ_APPROVAL_CONTROLS);
  expectSamePlaces(before, after, READ_APPROVAL_CONTROLS, step);
  // It appears after the other controls, in reading order: on Copy reference's line to its
  // right, or on a line below it. The controls of one line share their centre line.
  const copy = after.controls.get(COPY_REFERENCE.name);
  const appeared = await paneRectOf(nextChange);
  if (copy === undefined) throw new Error("Copy reference was not read");
  const below = (appeared.top + appeared.bottom) / 2 - (copy.top + copy.bottom) / 2;
  expect(
    below > 1 || (Math.abs(below) <= 1 && appeared.left > copy.right),
    step("Next change comes after Copy reference")
  ).toBe(true);
});

// ---------------------------------------------------------------------------
// k-remap (R14): Settings' Keyboard shortcuts lists every action with its keys, changes them for
// the signed-in person, and every open page of the browser takes the change at once.
// ---------------------------------------------------------------------------

/** Snooze's label, as Settings, `?` and ⌘K write it. */
const SNOOZE_LABEL = "Open the snooze menu";

/** The Inbox's Copy reference, as Settings, `?` and ⌘K write it. */
const COPY_REFERENCE_LABEL = "Copy the row's reference";

/** The labels of the actions whose first press is `g`, of which the refusal of `g` names one. */
const FIRST_PRESS_G =
  /Go to (Inbox|Agents|Settings|Documents|project…)|Scroll the open document to its top/;

/** Settings' Keyboard shortcuts section. */
function shortcutsSection(page: Page): Locator {
  return page.getByRole("region", { exact: true, name: "Keyboard shortcuts" });
}

/** The line of the action `label` under `scope` in Settings' Keyboard shortcuts. */
function shortcutLine(page: Page, scope: string, label: string): Locator {
  return shortcutsSection(page)
    .getByRole("region", { exact: true, name: scope })
    .getByRole("listitem", { exact: true, name: label });
}

/** Each action Settings lists, with the keys it draws for it: `inbox.snooze: b h`. */
function settingsKeys(page: Page): Promise<string[]> {
  return shortcutsSection(page)
    .locator("li[data-keymap-action]")
    .evaluateAll((items) =>
      items.map(
        (item) =>
          `${item.getAttribute("data-keymap-action")}: ${[...item.querySelectorAll("kbd")]
            .map((kbd) => kbd.textContent)
            .join(" ")}`
      )
    );
}

/** The section `scope` of the open `?` help. */
function helpSection(page: Page, scope: string): Locator {
  return page
    .getByRole("dialog", { name: "Keyboard shortcuts" })
    .getByRole("region", { exact: true, name: scope });
}

/** The line of the action `label` under `scope` in the open `?` help. */
function helpLine(page: Page, scope: string, label: string): Locator {
  return helpSection(page, scope).getByRole("listitem").filter({ hasText: label });
}

/** Opens the `?` help over the focused row, holds it to `check`, and closes it again. */
async function inHelp(page: Page, check: () => Promise<void>): Promise<void> {
  const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await page.keyboard.press("?");
  await expect(help).toBeVisible();
  await check();
  await page.keyboard.press("Escape");
  await expect(help).toHaveCount(0);
}

/** Selects the ask `askId` on the Inbox `page` shows, and focuses its row, where the Inbox's keys
 *  act on it. */
async function selectAsk(page: Page, askId: string): Promise<Locator> {
  await selectRow(page, askId);
  const row = inboxRow(page, askId);
  await row.focus();
  return row;
}

// SLOW(>=2s): two pages of one browser, six keys recorded on one (each waiting out the second
// press), and each read on the other through its keys, `?`, ⌘K and the rail, then both reloaded.
test("k-remap: a key changed in Settings works at once on the Inbox in another page, a key another action holds is refused, and Reset all puts every key back", async ({
  browser,
}) => {
  const step = (text: string) => `k-remap: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  const ask = await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);
  const context = await asUser(browser, "alice", { viewport: WIDE });
  try {
    const settings = await context.newPage();
    const inbox = await context.newPage();
    const copied = await recordClipboard(inbox);

    // On Settings: every action, by its scope, whether or not its page is open.
    await settings.goto("/settings");
    await expect(
      shortcutsSection(settings),
      step("Settings has a Keyboard shortcuts section")
    ).toBeVisible();
    const snooze = shortcutLine(settings, "Inbox", SNOOZE_LABEL);
    await expect(snooze.locator("kbd"), step("Settings lists Snooze with b and h")).toHaveText([
      "b",
      "h",
    ]);
    await expect(
      shortcutLine(settings, "Board", "Move card down").locator("kbd"),
      step("Settings lists the Board's actions, though no Board is open")
    ).toHaveText(["Shift+J"]);
    const agentsSearch = shortcutLine(settings, "Agents", "Search agents");
    await expect(
      agentsSearch.locator("kbd"),
      step("Settings lists the Agents page's actions, though no Agents page is open")
    ).toHaveText(["/"]);
    await expect(
      agentsSearch,
      step("the Agents page's own / says that Search does not work there")
    ).toContainText("Search does not work on the Agents page.");
    const defaults = await settingsKeys(settings);
    expect(defaults.length, step("Settings lists every action of the keymap")).toBeGreaterThan(100);

    // On the Inbox, before any change: y copies the ask's reference.
    await inbox.goto("/");
    let row = await selectAsk(inbox, ask.id);
    await inbox.keyboard.press("y");
    await expect
      .poll(async () => (await copied()).length, { message: step("y copies before the change") })
      .toBe(1);

    // b becomes Shift+B on Settings, and the Inbox takes it with no reload.
    await snooze.getByRole("button", { exact: true, name: "Change b" }).click();
    const recorder = settings.getByRole("button", { name: "Press the new key" });
    await expect(recorder, step("Change b waits for the new key")).toBeFocused();
    await settings.keyboard.press("Shift+B");
    // The recorder draws a press it holds as a key too, so the change is read once it is kept.
    await expect(
      snooze.getByRole("button", { exact: true, name: "Change Shift+B" }),
      step("b changes to Shift+B")
    ).toBeVisible();
    await expect(snooze.locator("kbd"), step("Snooze's keys are Shift+B and h")).toHaveText([
      "Shift+B",
      "h",
    ]);
    // The Inbox takes the change when the browser tells it, with no reload; the pane head's
    // Snooze shows it as it does.
    const headSnooze = paneHead(inbox).getByRole("button", { name: /^Snooze / });
    await expectKeyHint(headSnooze, "Shift+B", step("the pane head's Snooze shows Shift+B"));
    const menu = openList(inbox);
    await row.focus();
    await inbox.keyboard.press("Shift+B");
    await expect(menu, step("on the Inbox Shift+B opens the snooze menu")).toBeVisible();
    await inbox.keyboard.press("Escape");
    await expect(menu).toHaveCount(0);
    await expect(row, step("Escape puts the focus back on the row")).toBeFocused();
    await inbox.keyboard.press("b");
    await expect(menu, step("on the Inbox b opens nothing")).toHaveCount(0);
    await inbox.keyboard.press("h");
    await expect(menu, step("on the Inbox h still opens the snooze menu")).toBeVisible();
    await inbox.keyboard.press("Escape");
    await expect(menu).toHaveCount(0);
    await inHelp(inbox, async () => {
      await expect(
        helpLine(inbox, "Inbox", SNOOZE_LABEL).locator("kbd"),
        step("? lists Shift+B and h for Snooze")
      ).toHaveText(["Shift+B", "h"]);
    });
    const squeezed = (text: string) => text.replace(/\s+/g, "");
    expect(await commandBarRows(inbox), step("⌘K shows Shift+B beside Snooze")).toContain(
      squeezed(`${SNOOZE_LABEL} Shift+B or h`)
    );

    // Keys another action holds are refused, each naming it; c is taken, with a notice.
    await snooze.getByRole("button", { exact: true, name: "Add key" }).click();
    await expect(recorder, step("Add key waits for the new key")).toBeFocused();
    const refusal = snooze.getByRole("alert");
    await settings.keyboard.press("e");
    await expect(refusal, step("e is refused, and the refusal names Dismiss")).toContainText(
      "Dismiss"
    );
    await settings.keyboard.press("g");
    await expect(
      refusal,
      step("g is refused, and the refusal names an action whose first press is g")
    ).toHaveText(FIRST_PRESS_G);
    await settings.keyboard.press("1");
    await expect(refusal, step("1 is refused")).toHaveText(/^1 /);
    await settings.keyboard.press("c");
    await expect(
      snooze.getByRole("button", { exact: true, name: "Change c" }),
      step("c is taken")
    ).toBeVisible();
    await expect(refusal, step("c is taken, and no refusal shows")).toHaveCount(0);
    await expect(snooze.locator("kbd"), step("c is one more key of Snooze")).toHaveText([
      "Shift+B",
      "h",
      "c",
    ]);
    await expect(
      snooze,
      step("c is taken, and Settings says that Create issue does not work on the Inbox")
    ).toContainText("Create issue does not work on the Inbox.");

    // Go to Priorities takes g then r, two presses within one second.
    const priorities = shortcutLine(settings, "Global", "Go to Priorities");
    await priorities.getByRole("button", { exact: true, name: "Add key" }).click();
    await expect(recorder, step("Go to Priorities' Add key waits for the new key")).toBeFocused();
    await settings.keyboard.press("g");
    await settings.keyboard.press("r");
    await expect(
      priorities.getByRole("button", { exact: true, name: "Change g then r" }),
      step("g then r within one second is one key of two presses")
    ).toBeVisible();
    await expect(priorities.locator("kbd"), step("Go to Priorities' key is g then r")).toHaveText([
      "g",
      "r",
    ]);
    await row.focus();
    await inHelp(inbox, async () => {
      await expect(
        helpLine(inbox, "Global", "Go to Priorities").locator("kbd"),
        step("on the Inbox ? lists g r for Go to Priorities")
      ).toHaveText(["g", "r"]);
    });
    await inbox.keyboard.press("g");
    await inbox.keyboard.press("r");
    await expect(inbox, step("on the Inbox g then r opens the Priorities page")).toHaveURL(
      /\/priorities$/
    );
    await inbox.goto("/");
    row = await selectAsk(inbox, ask.id);

    // Create issue takes n.
    const create = shortcutLine(settings, "Global", "Create issue");
    await create.getByRole("button", { exact: true, name: "Change c" }).click();
    await expect(recorder, step("Change c waits for the new key")).toBeFocused();
    await settings.keyboard.press("n");
    await expect(
      create.getByRole("button", { exact: true, name: "Change n" }),
      step("Create issue's c changes to n")
    ).toBeVisible();
    const newIssue = inbox.getByRole("button", { name: /^New issue/ });
    await expect(newIssue.locator("kbd"), step("the rail's New issue shows n")).toHaveText("n");
    await expect(newIssue, step("the rail's New issue is pressed with N")).toHaveAttribute(
      "aria-keyshortcuts",
      "N"
    );
    const createDialog = inbox.getByRole("dialog", { name: "Create issue" });
    await row.focus();
    await inbox.keyboard.press("n");
    await expect(createDialog, step("on the Inbox n opens the create dialog")).toBeVisible();
    await inbox.keyboard.press("Escape");
    await expect(createDialog).toHaveCount(0);

    // Copy reference loses its only key: y does nothing, and ⌘K still lists the action.
    const copyReference = shortcutLine(settings, "Inbox", COPY_REFERENCE_LABEL);
    await copyReference.getByRole("button", { exact: true, name: "Remove y" }).click();
    await expect(copyReference.locator("kbd"), step("Copy reference has no key")).toHaveCount(0);
    await expect(
      paneHead(inbox)
        .getByRole("button", { name: /^Copy reference / })
        .locator("kbd"),
      step("the pane head's Copy reference shows no key")
    ).toHaveCount(0);
    // The recorder starts again on each page load, so it is read just before the press.
    const copiedBefore = (await copied()).length;
    await row.focus();
    await inbox.keyboard.press("y");
    expect(await copied(), step("on the Inbox y copies nothing")).toHaveLength(copiedBefore);
    expect(await commandBarRows(inbox), step("⌘K still lists Copy reference")).toContain(
      squeezed(COPY_REFERENCE_LABEL)
    );

    // A reload of both pages keeps each change.
    await settings.reload();
    await expect(snooze.locator("kbd"), step("a reload keeps Snooze's keys")).toHaveText([
      "Shift+B",
      "h",
      "c",
    ]);
    await expect(
      priorities.locator("kbd"),
      step("a reload keeps Go to Priorities' g r")
    ).toHaveText(["g", "r"]);
    await expect(create.locator("kbd"), step("a reload keeps Create issue's n")).toHaveText(["n"]);
    await expect(
      copyReference.locator("kbd"),
      step("a reload keeps Copy reference with no key")
    ).toHaveCount(0);
    await inbox.reload();
    row = await selectAsk(inbox, ask.id);
    await inHelp(inbox, async () => {
      await expect(
        helpLine(inbox, "Inbox", SNOOZE_LABEL).locator("kbd"),
        step("after a reload ? lists Shift+B, h and c for Snooze")
      ).toHaveText(["Shift+B", "h", "c"]);
      await expect(
        helpLine(inbox, "Global", "Go to Priorities").locator("kbd"),
        step("after a reload ? lists g r for Go to Priorities")
      ).toHaveText(["g", "r"]);
      await expect(
        helpLine(inbox, "Global", "Create issue").locator("kbd"),
        step("after a reload ? lists n for Create issue")
      ).toHaveText(["n"]);
      await expect(
        helpLine(inbox, "Inbox", COPY_REFERENCE_LABEL),
        step("after a reload ? lists no key for Copy reference")
      ).toHaveCount(0);
    });

    // Reset all puts back each key, on Settings and on the Inbox.
    await shortcutsSection(settings)
      .getByRole("button", { exact: true, name: "Reset all" })
      .click();
    await expect
      .poll(() => settingsKeys(settings), { message: step("Reset all puts back each key") })
      .toEqual(defaults);
    await expect(newIssue.locator("kbd"), step("after Reset all the rail shows c")).toHaveText("c");
    await inHelp(inbox, async () => {
      await expect(
        helpLine(inbox, "Inbox", SNOOZE_LABEL).locator("kbd"),
        step("after Reset all ? lists b and h for Snooze")
      ).toHaveText(["b", "h"]);
      await expect(
        helpLine(inbox, "Global", "Create issue").locator("kbd"),
        step("after Reset all ? lists c for Create issue")
      ).toHaveText(["c"]);
      await expect(
        helpLine(inbox, "Global", "Go to Priorities"),
        step("after Reset all ? lists no key for Go to Priorities")
      ).toHaveCount(0);
      await expect(
        helpLine(inbox, "Inbox", COPY_REFERENCE_LABEL).locator("kbd"),
        step("after Reset all ? lists y for Copy reference")
      ).toHaveText(["y"]);
    });
    await inbox.keyboard.press("b");
    await expect(menu, step("after Reset all b opens the snooze menu")).toBeVisible();
    await inbox.keyboard.press("Escape");
    await expect(menu).toHaveCount(0);

    // The help has a link that opens the section.
    await inbox.keyboard.press("?");
    const help = inbox.getByRole("dialog", { name: "Keyboard shortcuts" });
    await help.getByRole("link", { name: /Settings/ }).click();
    await expect(inbox, step("the help's link opens Settings at the section")).toHaveURL(
      /\/settings#keyboard-shortcuts$/
    );
    await expect(help, step("the help closes on the move")).toHaveCount(0);
    await expect(shortcutsSection(inbox), step("the section is in view")).toBeInViewport();
    await expect(shortcutsSection(inbox), step("the section has the focus")).toBeFocused();
  } finally {
    await context.close();
  }
});

/** How long Settings' recorder waits for a key's second press before the first alone is the key:
 *  `CHORD_TIMEOUT_MS` of `web/src/features/shell/keymap.ts`, written here for two reasons. A row
 *  cannot load that module, which reads `import.meta.env` as it loads, and Playwright's process
 *  does not define it; and the merge base the rows also run on does not export it. */
const CHORD_TIMEOUT_MS = 1_000;

/** How many times the row of a key left within the second tries its press and click: a machine
 *  slow enough to take the second over one try does not fail the row wrongly. */
const IN_TIME_TRIES = 3;

// SLOW(>=2s): a key recorded on Settings, tried again where the click came after the second, and
// read on the Inbox of another page, then on Settings loaded again.
test("k-remap: a key pressed in Settings and left by a click on the page before the second press's second ends is the new key, as R14 makes the next key press", async ({
  browser,
}) => {
  const step = (text: string) => `k-remap: a key left within the second: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  const ask = await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);
  const context = await asUser(browser, "alice", { viewport: WIDE });
  try {
    const settings = await context.newPage();
    const inbox = await context.newPage();
    await settings.goto("/settings");
    const snooze = shortcutLine(settings, "Inbox", SNOOZE_LABEL);
    const recorder = settings.getByRole("button", { name: "Press the new key" });
    // While the recorder waits for a second press it already draws `Shift+B then …`, so the read
    // of Snooze's keys below passes whether or not the one second has run out and kept Shift+B:
    // only the time from the press to the click says that the click ended the recording. A try
    // whose click came later proves nothing, and the timer kept its key, so Reset all puts b back
    // and the row tries again.
    let inTime = false;
    for (let attempt = 1; attempt <= IN_TIME_TRIES && !inTime; attempt += 1) {
      const tried = (text: string) => step(`try ${attempt}, ${text}`);
      if (attempt > 1) {
        await shortcutsSection(settings)
          .getByRole("button", { exact: true, name: "Reset all" })
          .click();
        await expect(snooze.locator("kbd"), tried("Reset all puts back b and h")).toHaveText([
          "b",
          "h",
        ]);
      }
      await snooze.getByRole("button", { exact: true, name: "Change b" }).click();
      await expect(recorder, tried("Change b waits for the new key")).toBeFocused();
      const pressedAt = performance.now();
      await settings.keyboard.press("Shift+B");
      await expect(snooze.locator("kbd"), tried("Snooze draws Shift+B and h")).toHaveText([
        "Shift+B",
        "h",
      ]);
      // The click lands inside the one second a second press may still come in, or the try does
      // not count.
      await settings.getByRole("heading").first().click();
      inTime = performance.now() - pressedAt < CHORD_TIMEOUT_MS;
    }
    expect(inTime, step("the click never came within the second in three tries")).toBe(true);

    // The Inbox takes the person's keys when the browser tells it, with no reload.
    await inbox.goto(inboxAt(ask.id));
    await expect(selectedRow(inbox), step("the ask is the item being read")).toHaveAttribute(
      "data-inbox-row",
      ask.id
    );
    const headSnooze = paneHead(inbox).getByRole("button", { name: /^Snooze / });
    await expectKeyHint(headSnooze, "Shift+B", step("the pane head's Snooze shows Shift+B"));
    await settings.reload();
    await expect(
      shortcutLine(settings, "Inbox", SNOOZE_LABEL).locator("kbd"),
      step("Settings, loaded again, lists Shift+B and h for Snooze")
    ).toHaveText(["Shift+B", "h"]);
  } finally {
    await context.close();
  }
});

// ---------------------------------------------------------------------------
// k-issue-tabs (R21): on today's issue page `]` and `[` walk the tab strip in its order and wrap
// at either end, the `t` chords stay, and the chord hint lists each second key of `t` and `g` with
// its action until the second press or the end of the one second; Escape in the spec the reader
// edits takes the focus out of the editor, so the page's keys work, and Tab back into the document
// puts the caret where it was.
// ---------------------------------------------------------------------------

/** The issue page's tabs in the order of its strip: each tab's address segment and its name. */
const ISSUE_TABS = [
  ["spec", "Spec"],
  ["verification", "Verification"],
  ["plan", "Plan"],
  ["steps", "Steps"],
  ["conversation", "Conversation"],
  ["children", "Children"],
  ["artifacts", "Artifacts"],
  ["activity", "Activity"],
  ["decisions", "Decisions"],
  ["collaborators", "Collaborators"],
] as const;

type IssueTabAt = (typeof ISSUE_TABS)[number];

/** Each key the chord hint lists after `t`, with the action it names. A chord the new issue page
 *  shares names its purpose, then what it does on the new page. */
const TAB_CHORD_HINT = [
  "s Spec: open its tab (on the new page, its filter)",
  "k Verification: open its tab (on the new page, its filter)",
  "p Plan: open its tab (on the new page, its filter)",
  "e Steps tab",
  "c Conversation: open its tab (on the new page, every filter off)",
  "h Children: open its tab (on the new page, move to them)",
  "a Artifacts tab",
  "v Activity tab",
  "d Decisions: open its tab (on the new page, its filter)",
  "o Collaborators tab",
];

/** Each key the chord hint lists after `g`, with the action it names. */
const GO_CHORD_HINT = [
  "i Go to Inbox",
  "a Go to Agents",
  "s Go to Settings",
  "d Go to Documents",
  "p Go to project…",
];

/** The paragraph and the list item of k-issue-tabs' spec the rows click into. */
const TABS_PARAGRAPH = "The ledger keeps one clock for every mirror.";
const TABS_ITEM = "The second mirror replays the ledger.";

/** An open issue of CORE whose spec holds a paragraph and a bulleted list, and whose plan and
 *  Verification document each hold text. */
async function seedIssueTabs(): Promise<{ key: string; title: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger tabs";
  const issue = await createIssue({
    project: "CORE",
    spec: `# ${title}\n\n${TABS_PARAGRAPH}\n\n- The first mirror reads the ledger.\n- ${TABS_ITEM}\n`,
    title,
  });
  await createIssueArtifact(issue.key, {
    content: "# Plan\n\nRead the ledger, then each mirror.\n",
    name: "plan.md",
  });
  await createIssueArtifact(issue.key, {
    content: "# Verification\n\nEach mirror agrees with the ledger.\n",
    name: "verification.md",
  });
  return { key: issue.key, title };
}

/** Opens the issue's Spec tab with the focus on the page itself, outside the document. */
async function openSpecTab(page: Page, key: string, title: string): Promise<void> {
  await page.goto(`/issues/${key}/spec`);
  await expect(
    page.getByTestId("issue-header").getByRole("heading", { level: 1, name: title })
  ).toBeVisible();
  await page.locator("body").focus();
}

/** Waits until the strip has `tab` selected and the address names it. */
async function expectTab(page: Page, key: string, tab: IssueTabAt, message: string): Promise<void> {
  const [segment, name] = tab;
  await expect(
    page
      .getByRole("tablist", { name: "Issue detail" })
      .getByRole("tab", { name: new RegExp(`^${name}`) }),
    message
  ).toHaveAttribute("aria-selected", "true");
  await expect(page, `${message}, and the address follows`).toHaveURL(
    new RegExp(`/issues/${key}/${segment}$`)
  );
}

/** Each line the chord hint lists, its second key and the action it names, in key order. */
function chordHint(page: Page): Promise<string[]> {
  return page
    .getByTestId("chord-indicator")
    .getByRole("listitem")
    .evaluateAll((items) =>
      items
        .map(
          (item) =>
            `${item.querySelector("kbd")?.textContent} ${item.querySelector("span")?.textContent}`
        )
        .sort()
    );
}

// SLOW(>=2s): the issue page loaded, every tab of its strip opened twice, and a chord hint left
// to its one second.
test("k-issue-tabs: ] and [ walk the tab strip in its order and wrap, t c selects Conversation, and the chord hint lists each second key of t and g with its action", async ({
  page,
}) => {
  const seed = await seedIssueTabs();
  const step = (text: string) => `k-issue-tabs: ${text}`;
  const hint = page.getByTestId("chord-indicator");
  await page.setViewportSize(WIDE);
  await openSpecTab(page, seed.key, seed.title);

  for (const tab of [...ISSUE_TABS.slice(1), ISSUE_TABS[0]]) {
    await page.keyboard.press("]");
    await expectTab(page, seed.key, tab, step(`] selects ${tab[1]}`));
  }
  for (const tab of [...ISSUE_TABS].reverse()) {
    await page.keyboard.press("[");
    await expectTab(page, seed.key, tab, step(`[ selects ${tab[1]}`));
  }

  await page.keyboard.press("t");
  await expect(hint.locator("kbd").first(), step("t shows the chord hint")).toHaveText("t");
  await expect
    .poll(() => chordHint(page), {
      message: step("after t the chord hint lists each tab's key with its tab"),
    })
    .toEqual([...TAB_CHORD_HINT].sort());
  await page.keyboard.press("c");
  await expect(hint, step("c ends the chord and its hint")).toHaveCount(0);
  await expectTab(
    page,
    seed.key,
    ["conversation", "Conversation"],
    step("t c selects Conversation")
  );

  await page.locator("body").focus();
  await page.keyboard.press("g");
  await expect
    .poll(() => chordHint(page), {
      message: step("after g the chord hint lists each page's key with its page"),
    })
    .toEqual([...GO_CHORD_HINT].sort());
  await expect(
    hint,
    step("the hint goes at the end of the one second, with no second press")
  ).toHaveCount(0, { timeout: 5_000 });
  await expect(page, step("a g with no second press goes nowhere")).toHaveURL(
    new RegExp(`/issues/${seed.key}/conversation$`)
  );
});

/** Whether the focus is inside the Spec tab's editor. */
function focusInSpec(page: Page): Promise<boolean> {
  return specEditor(page).evaluate((root) => root.contains(document.activeElement));
}

/** The Spec tab's editor. */
function specEditor(page: Page): Locator {
  return page
    .getByRole("tabpanel", { name: "Spec" })
    .getByRole("textbox", { name: "Document editor" });
}

/** Where the page's caret stands: the text of its node and its offset there. */
interface Caret {
  readonly collapsed: boolean;
  readonly offset: number;
  readonly text: string;
}

/** Where the page's caret stands now, or null with no selection. */
function caretOf(page: Page): Promise<Caret | null> {
  return page.evaluate(() => {
    const selection = window.getSelection();
    if (selection === null || selection.rangeCount === 0) return null;
    return {
      collapsed: selection.isCollapsed,
      offset: selection.anchorOffset,
      text: selection.anchorNode?.textContent ?? "",
    };
  });
}

test("k-issue-tabs: Escape in the spec the reader edits takes the focus out of the editor so ] works, changes no text, and Tab back in puts the caret where it was", async ({
  page,
}) => {
  const seed = await seedIssueTabs();
  const step = (text: string) => `k-issue-tabs: ${text}`;
  await page.setViewportSize(WIDE);
  await openSpec(page, seed.key, TABS_PARAGRAPH);
  const before = (await getIssueArtifactText(seed.key, "spec")).markdown;
  const editor = specEditor(page);

  let caret: Caret | null = null;
  for (const [text, where] of [
    [TABS_PARAGRAPH, "a paragraph"],
    [TABS_ITEM, "an item of the list"],
  ] as const) {
    await editor.getByText(text).click();
    await expect(editor, step(`a click puts the caret in ${where}`)).toBeFocused();
    caret = await caretOf(page);
    expect(caret?.text, step(`the caret stands in ${where}`)).toBe(text);
    await page.keyboard.press("Escape");
    expect(
      await focusInSpec(page),
      step(`Escape in ${where} takes the focus out of the editor`)
    ).toBe(false);
    await page.keyboard.press("]");
    await expectTab(
      page,
      seed.key,
      ["verification", "Verification"],
      step(`after Escape in ${where}, ] selects the next tab`)
    );
    expect(
      (await getIssueArtifactText(seed.key, "spec")).markdown,
      step(`Escape in ${where} leaves the spec's Markdown as it was`)
    ).toBe(before);
    await page.keyboard.press("[");
    await expectTab(page, seed.key, ["spec", "Spec"], step("[ goes back to the Spec tab"));
  }

  // Back on the Spec tab, Tab goes on from where the focus last was until it is in the document.
  // The `[` above moved the focus inside its own key press and its tab was waited for, so this
  // read is settled.
  let inSpec = await focusInSpec(page);
  for (let press = 0; press < TAB_PRESSES && !inSpec; press += 1) {
    await page.keyboard.press("Tab");
    // A Tab moves the focus inside its own key press, so this reads where it settled.
    inSpec = await focusInSpec(page);
  }
  expect(inSpec, step("Tab goes back into the document")).toBe(true);
  await expect
    .poll(() => caretOf(page), {
      message: step("Tab back into the document puts the caret where it was"),
    })
    .toEqual(caret);
  expect(
    (await getIssueArtifactText(seed.key, "spec")).markdown,
    step("the spec's Markdown is as it was")
  ).toBe(before);
});

// ---------------------------------------------------------------------------
// k-threads (R22): on an issue's Conversation tab `j` and `k` walk the turns, an open thread's
// replies among them; Enter and `o` open and close a thread or a fold with the focus kept on the
// turn; `r` puts the caret in the thread's reply box, and Escape in the empty box gives the focus
// back; `y` and Shift+Y copy a turn's reference and its web address, and nothing on a handoff.
// Each comment, reply and message of the issue has a Copy reference that Tab reaches, on the
// Conversation tab, in the margin of the spec, and in the Inbox's reading pane, reading view and
// drawer.
// ---------------------------------------------------------------------------

/** The text of k-threads' spec that its comment thread is anchored to. */
const THREAD_ANCHOR = "The mirror renews the ledger's lease every hour.";

/** k-threads' issue, and the ids of what its conversation holds, each list oldest first. */
interface ThreadSeed {
  readonly key: string;
  readonly message: string;
  readonly messageReply: string;
  readonly comment: string;
  readonly commentReplies: readonly string[];
  readonly ask: string;
  readonly askReplies: readonly string[];
  readonly handoff: string;
}

/** An issue of CORE whose conversation holds the reader's message with bob's reply, the reader's
 *  comment anchored in the spec with three of bob's replies, the reader's question with four of
 *  them, and a handoff, so the Inbox lists each of the three threads. Each thread is marked read
 *  through its newest reply: the Inbox and the Conversation then fold the same earlier replies
 *  from their first draw, and no read mark a row's dwell writes folds one under the keys. */
async function seedThreads(): Promise<ThreadSeed> {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger lease";
  const issue = await createIssue({
    project: "CORE",
    spec: `# ${title}\n\n${THREAD_ANCHOR}\n`,
    title,
  });
  const bob = { login: "bob" };
  const message = await createMessage(issue.key, { body: "Who holds the ledger's lease?" });
  const messageReply = await createMessage(
    issue.key,
    { body: "The mirror holds it.", in_reply_to: message.id },
    bob
  );
  const comment = await createComment(issue.key, {
    anchor: { artifact: "spec", quote: THREAD_ANCHOR },
    body: "Is an hour long enough?",
  });
  const commentReplies = [];
  for (const body of ["First thread reply.", "Second thread reply.", "Third thread reply."]) {
    commentReplies.push(await createComment(issue.key, { body, reply_to: comment.id }, bob));
  }
  const ask = await createAsk(issue.key, { question: "Do we shorten the lease?" });
  const askReplies = [];
  for (const body of ["First ask reply.", "Second ask reply.", "Third ask reply.", "Fourth."]) {
    askReplies.push(await createComment(issue.key, { ask_id: ask.id, body }, bob));
  }
  const handoff = await createHandoff(issue.key, "implement", { summary: "The lease renews." });
  const newestComment = commentReplies.at(-1);
  const newestAskReply = askReplies.at(-1);
  if (newestComment === undefined || newestAskReply === undefined) {
    throw new Error("k-threads: the replies were not seeded");
  }
  await markThreadRead(message.id, messageReply.created_at);
  await markThreadRead(comment.id, newestComment.created_at);
  await markThreadRead(ask.id, newestAskReply.created_at);
  return {
    ask: ask.id,
    askReplies: askReplies.map(({ id }) => id),
    comment: comment.id,
    commentReplies: commentReplies.map(({ id }) => id),
    handoff: handoff.id,
    key: issue.key,
    message: message.id,
    messageReply: messageReply.id,
  };
}

/** The Conversation tab of `seed`'s issue at 1440x900, every conversation drawn, the focus on
 *  the page itself; answers the list of its turns. */
async function openConversation(page: Page, seed: ThreadSeed): Promise<Locator> {
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${seed.key}/conversation`);
  const turns = page.getByRole("list", { name: "Conversation turns" });
  for (const turn of [
    `handoff:${seed.handoff}`,
    `ask:${seed.ask}`,
    `comment:${seed.comment}`,
    `message:${seed.message}`,
    `message:${seed.messageReply}`,
  ]) {
    await expect(turns.locator(`[data-turn="${turn}"]`)).toBeVisible();
  }
  await page.locator("body").focus();
  return turns;
}

/** The Copy reference names of every comment, reply and message of `seed`'s three threads, by
 *  thread. */
function threadCopyNames(seed: ThreadSeed): Record<"ask" | "comment" | "message", string[]> {
  const name = (kind: "ask" | "comment" | "message", id: string) =>
    `Copy reference dispatch://${seed.key}/${kind}/${id}`;
  return {
    ask: [name("ask", seed.ask), ...seed.askReplies.map((id) => name("comment", id))],
    comment: [
      name("comment", seed.comment),
      ...seed.commentReplies.map((id) => name("comment", id)),
    ],
    message: [name("message", seed.message), name("message", seed.messageReply)],
  };
}

/** Holds that Tab, walking `scope` from where the focus is (`tabWalk`, at most `TAB_PRESSES`
 *  presses), reaches each control `expected` names by its `aria-label`, else its text, in
 *  `where`. */
async function expectTabReaches(
  page: Page,
  scope: Locator,
  expected: readonly string[],
  where: string
): Promise<void> {
  const names = (await tabWalk(page, scope, TAB_PRESSES)).map((stop) => stop.name);
  for (const name of expected) {
    expect(names, `k-threads: ${where}, Tab reaches ${name}`).toContain(name);
  }
}

// SLOW(>=2s): an issue with four conversations and eight replies seeded, the Conversation tab
// walked by its keys and five copies read back.
test("k-threads: j and k walk the turns and an open thread's replies, Enter and o open and close a thread with the focus kept on the turn, r reaches its reply box and Escape comes back, and y and Shift+Y copy a turn's reference and address", async ({
  page,
}) => {
  const copied = await recordClipboard(page);
  const seed = await seedThreads();
  const step = (text: string) => `k-threads: ${text}`;
  const turns = await openConversation(page, seed);
  const turn = (id: string) => turns.locator(`[data-turn="${id}"]`);
  const askReply = (id: string) =>
    turns.locator(`[data-reply][data-turn-ref="dispatch://${seed.key}/comment/${id}"]`);
  const commentTurn = turn(`comment:${seed.comment}`);
  const replies = commentTurn.locator("[data-reply]");
  const origin = new URL(page.url()).origin;
  const lastCopy = async () => (await copied()).at(-1);

  // Newest activity first: the handoff, the question with its two newest replies (the fold holds
  // the two before them), the comment with its thread closed, the message and its reply.
  const walk: readonly (readonly [Locator, string])[] = [
    [turn(`handoff:${seed.handoff}`), "the handoff"],
    [turn(`ask:${seed.ask}`), "the question"],
    [askReply(seed.askReplies[2] ?? ""), "the question's third reply"],
    [askReply(seed.askReplies[3] ?? ""), "the question's fourth reply"],
    [commentTurn, "the comment"],
    [turn(`message:${seed.message}`), "the message"],
    [turn(`message:${seed.messageReply}`), "the message's reply"],
  ];
  for (const [row, what] of walk) {
    await page.keyboard.press("j");
    await expect(row, step(`j focuses ${what}`)).toBeFocused();
  }
  await expect(
    turns.locator("[data-turn], [data-reply]"),
    step("j walked every turn and reply the tab draws")
  ).toHaveCount(walk.length);
  await page.keyboard.press("k");
  await expect(turn(`message:${seed.message}`), step("k goes back to the message")).toBeFocused();
  await page.keyboard.press("k");
  await expect(commentTurn, step("k goes back to the comment")).toBeFocused();

  // The collapsed comment thread: Enter opens it with the focus kept on the turn, and `j` then
  // walks its replies.
  await expect(replies, step("the comment's thread is closed")).toHaveCount(0);
  await page.keyboard.press("Enter");
  await expect(replies, step("Enter opens the comment's thread")).toHaveCount(3);
  await expect(commentTurn, step("Enter keeps the focus on the comment's turn")).toBeFocused();
  await page.keyboard.press("j");
  await expect(replies.first(), step("j then focuses the thread's first reply")).toBeFocused();
  await page.keyboard.press("r");
  await expect(
    commentTurn.getByRole("textbox", { name: "Reply" }),
    step("r on the reply puts the caret in the thread's reply box")
  ).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(
    replies.first(),
    step("Escape in the empty box puts the focus back on the reply")
  ).toBeFocused();
  await expect(replies, step("Escape keeps the thread open")).toHaveCount(3);
  await page.keyboard.press("k");
  await expect(commentTurn, step("k goes back to the comment's turn")).toBeFocused();
  await page.keyboard.press("o");
  await expect(replies, step("o on the thread closes it")).toHaveCount(0);
  await expect(commentTurn, step("o keeps the focus on the comment's turn")).toBeFocused();

  // `y` and Shift+Y on the comment: its reference and its page's address.
  await page.keyboard.press("y");
  await expect
    .poll(lastCopy, { message: step("y copies the comment's reference") })
    .toBe(`dispatch://${seed.key}/comment/${seed.comment}`);
  await page.keyboard.press("Shift+Y");
  await expect
    .poll(lastCopy, { message: step("Shift+Y copies the comment's web address") })
    .toBe(`${origin}/issues/${seed.key}/comments/${seed.comment}`);

  // The handoff: Enter opens its fields, and it has no reference to copy. The next copy, `y` on
  // the question, is then the first copy since the handoff's keys.
  const handoff = turn(`handoff:${seed.handoff}`);
  for (const [row, what] of [...walk.slice(0, 4)].reverse()) {
    await page.keyboard.press("k");
    await expect(row, step(`k goes back to ${what}`)).toBeFocused();
  }
  await page.keyboard.press("Enter");
  await expect(
    handoff.getByRole("button", { name: "Hide fields" }),
    step("Enter on the handoff opens its fields")
  ).toBeVisible();
  await expect(handoff, step("Enter keeps the focus on the handoff")).toBeFocused();
  const copiesBefore = (await copied()).length;
  await page.keyboard.press("y");
  await page.keyboard.press("Shift+Y");
  await page.keyboard.press("j");
  await expect(turn(`ask:${seed.ask}`), step("j goes on to the question")).toBeFocused();
  await page.keyboard.press("y");
  await expect
    .poll(async () => (await copied()).slice(copiesBefore), {
      message: step("y and Shift+Y on the handoff copy nothing, and y on the question copies it"),
    })
    .toEqual([`dispatch://${seed.key}/ask/${seed.ask}`]);

  // On the third reply of the comment's thread, `y` copies that reply's reference.
  for (const [row, what] of walk.slice(2, 5)) {
    await page.keyboard.press("j");
    await expect(row, step(`j focuses ${what}`)).toBeFocused();
  }
  await page.keyboard.press("Enter");
  await expect(replies, step("Enter opens the comment's thread again")).toHaveCount(3);
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await page.keyboard.press("j");
  await expect(replies.nth(2), step("j focuses the thread's third reply")).toBeFocused();
  await page.keyboard.press("y");
  await expect
    .poll(lastCopy, { message: step("y on the third reply copies its reference") })
    .toBe(`dispatch://${seed.key}/comment/${seed.commentReplies[2]}`);
});

// SLOW(>=2s): an issue with four conversations seeded, the Conversation tab and the spec's margin
// each walked by Tab.
test("k-threads: each comment, reply and message on the Conversation tab and in the spec's margin has a Copy reference that Tab reaches", async ({
  page,
}) => {
  const seed = await seedThreads();
  const names = threadCopyNames(seed);

  // The Conversation tab, with the comment's thread and the question's earlier replies open, walked
  // from its filter, above the turns.
  const turns = await openConversation(page, seed);
  await turns
    .locator(`[data-turn="comment:${seed.comment}"]`)
    .getByRole("button", { name: "Expand thread" })
    .click();
  await turns
    .locator(`[data-turn="ask:${seed.ask}"]`)
    .getByRole("button", { name: "Show 2 earlier replies" })
    .click();
  await expect(turns.locator("[data-reply]")).toHaveCount(8);
  await page
    .getByRole("group", { name: "Conversations" })
    .getByRole("button", { name: "All" })
    .click();
  await expectTabReaches(
    page,
    turns,
    [...names.message, ...names.comment, ...names.ask],
    "on the Conversation tab"
  );

  // The spec's margin: the comment's card, open, walked from the text it quotes.
  await page.goto(`/issues/${seed.key}/spec`);
  const card = page.locator(`[data-margin-item="${seed.comment}"]`);
  await expectTabReaches(page, card, [names.comment[0] ?? ""], "on the margin's closed card");
  await card.getByRole("button", { expanded: false }).click();
  await expect(card.locator("[data-reply]")).toHaveCount(3);
  await card.locator("blockquote").first().click();
  await expectTabReaches(page, card, names.comment, "on the margin's open card");
});

/** The Inbox's steps of k-threads where `place` draws it: beside the reading pane, and in the
 *  reading view at 1024x768, each thread is opened, its fold opened and its column walked by Tab
 *  after `l`; in the drawer over a project page, each thread's card has its fold opened and is
 *  walked by Tab from its row. One row per place, as each walks the three threads. */
async function inboxThreadCopySteps(
  page: Page,
  place: "the reading pane" | "the reading view" | "the drawer"
): Promise<void> {
  const seed = await seedThreads();
  const names = threadCopyNames(seed);
  // Each thread's row, the fold its older replies are drawn behind, and its Copy reference names.
  // The message and the comment are the reader's Reply rows. The question waits on the reader,
  // the project's default assignee, so the Inbox lists it as their ask, whose card draws its
  // thread, and not as a Reply row (`myRepliesOwner`, `api/replies.go`).
  const threads = [
    [replyRowId(seed.message), undefined, names.message, "the message's thread"],
    [replyRowId(seed.comment), "Show 1 earlier reply", names.comment, "the comment's thread"],
    [seed.ask, "Show 2 earlier replies", names.ask, "the question's thread"],
  ] as const;

  if (place !== "the drawer") {
    const narrow = place === "the reading view";
    await page.setViewportSize(narrow ? NARROW : WIDE);
    await page.goto("/");
    for (const [id, fold, expected, what] of threads) {
      if (narrow) {
        await selectRow(page, id);
        await expect(
          readingView(page),
          `k-threads: the Inbox lists ${what}, and a click opens its reading view`
        ).toHaveAttribute("data-inbox-reading-row", id);
      } else {
        await expect(inboxRow(page, id), `k-threads: the Inbox lists ${what}`).toBeVisible();
        await selectRow(page, id);
      }
      if (fold !== undefined) await paneScroller(page).getByRole("button", { name: fold }).click();
      // Beside the pane `l` is pressed from the row, the click on its age putting the focus back
      // there; the reading view hides the list, and `l` is pressed from the fold the click left
      // the focus on.
      if (!narrow) await inboxRow(page, id).locator("time").first().click();
      await page.keyboard.press("l");
      await expect(paneScroller(page), `k-threads: l puts the focus in ${what}`).toBeFocused();
      await expectTabReaches(
        page,
        paneScroller(page),
        expected,
        `${narrow ? "in the reading view at 1024x768" : "in the reading pane"}, in ${what}`
      );
    }
    return;
  }

  await page.setViewportSize(WIDE);
  const scope = await openDrawer(page, "k-threads: i opens the drawer");
  for (const [id, fold, expected, what] of threads) {
    const row = scope.locator(`[data-inbox-row="${id}"]`);
    await expect(row, `k-threads: in ${place}, the Inbox lists ${what}`).toBeVisible();
    if (fold !== undefined) await row.getByRole("button", { name: fold }).click();
    await row.focus();
    await expect(row, `k-threads: in ${place}, ${what}'s row takes the focus`).toBeFocused();
    await expectTabReaches(page, row, expected, `in ${place}, in ${what}`);
  }
}

// SLOW(>=2s): an issue with four conversations seeded, and the reading pane walked by Tab over
// each of its three threads.
test("k-threads: beside the Inbox's reading pane, each comment, reply and message of an issue has a Copy reference that Tab reaches", async ({
  page,
}) => {
  await inboxThreadCopySteps(page, "the reading pane");
});

// SLOW(>=2s): an issue with four conversations seeded, and the reading view walked by Tab over
// each of its three threads at 1024x768.
test("k-threads: in the Inbox's reading view at 1024x768, each comment, reply and message of an issue has a Copy reference that Tab reaches", async ({
  page,
}) => {
  await inboxThreadCopySteps(page, "the reading view");
});

// SLOW(>=2s): an issue with four conversations seeded, and each of its three thread cards walked
// by Tab in the drawer over a project page.
test("k-threads: in the Inbox drawer, each comment, reply and message of an issue has a Copy reference that Tab reaches", async ({
  page,
}) => {
  await inboxThreadCopySteps(page, "the drawer");
});

// ---------------------------------------------------------------------------
// k-comment-focus (R19): each new comment on a document, from Ctrl+Alt+M or the selection bar's
// Comment, puts the caret in the margin's comment box - with a box already open on the document,
// after the margin was hidden and shown again, and below 1280 px, where the margin is a modal
// sheet the reader closes before the next comment. Escape in the empty box, and a sent comment,
// put the caret back in the document inside the commented word; below 1280 px they close the
// sheet too. A sent comment does so as well when the page's frames come later than an armed
// press waits for one.
// ---------------------------------------------------------------------------

/** k-comment-focus' spec: three paragraphs, each with the word the rows comment on. */
const FOCUS_PARAGRAPHS = [
  ["The ledger keeps one clock for every mirror.", "clock"],
  ["Each mirror replays the ledger after a cold start.", "replays"],
  ["The lease returns to the ledger at noon.", "lease"],
] as const;

/** The word of each paragraph, in the spec's order. */
const [[, FIRST_WORD], [, SECOND_WORD], [, THIRD_WORD]] = FOCUS_PARAGRAPHS;

/** The width below 1280 px where the margin is a sheet over the document. */
const SHEET_VIEWPORT = { height: 768, width: 1024 };

/** An open issue of CORE whose spec holds `FOCUS_PARAGRAPHS`. */
async function seedCommentFocus(): Promise<{ artifactId: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({
    project: "CORE",
    spec: `# Comment focus\n\n${FOCUS_PARAGRAPHS.map(([text]) => text).join("\n\n")}\n`,
    title: "Comment focus",
  });
  return { artifactId: issue.primary_artifact_id, key: issue.key };
}

/** The margin's comment box. */
function commentBox(page: Page): Locator {
  return page.getByTestId("margin-sheet").getByRole("form", { name: "Comment composer" });
}

/** Waits until the caret is in the margin's comment box, under the quote of `word`: the box's own
 *  quote, never the text of a kept draft the box offers beside its field, which it draws inside a
 *  `role="status"` block (`DraftOffer`). */
async function expectCaretInBox(page: Page, word: string, message: string): Promise<void> {
  const box = commentBox(page);
  await expect(box.getByLabel("Comment"), message).toBeFocused();
  await expect(
    box.locator("blockquote:not([role='status'] blockquote)"),
    `${message}, under the quote of "${word}"`
  ).toHaveText(word);
}

/** Selects `word` in the document and presses Ctrl+Alt+M: the caret is in the comment box. */
async function commentByKey(page: Page, word: string, message: string): Promise<void> {
  await selectEditorText(page, word);
  await page.keyboard.press("Control+Alt+KeyM");
  await expectCaretInBox(page, word, message);
}

/** Where the caret stands in the document's editor: the text of the paragraph it is in and the
 *  span of that text it covers, or null while the focus or the selection is not in the editor. */
interface DocumentCaret {
  readonly end: number;
  readonly paragraph: string;
  readonly start: number;
}

function documentCaret(page: Page): Promise<DocumentCaret | null> {
  return documentEditor(page).evaluate((root) => {
    const selection = window.getSelection();
    if (!root.contains(document.activeElement) || selection === null || selection.rangeCount === 0)
      return null;
    const range = selection.getRangeAt(0);
    const node = range.startContainer;
    const paragraph = (node instanceof Element ? node : node.parentElement)?.closest("p");
    if (paragraph === null || paragraph === undefined || !root.contains(paragraph)) return null;
    const before = document.createRange();
    before.setStart(paragraph, 0);
    before.setEnd(node, range.startOffset);
    const start = before.toString().length;
    return { end: start + range.toString().length, paragraph: paragraph.textContent ?? "", start };
  });
}

/** Waits until the caret is in the document's editor, inside `word`. */
async function expectCaretInDocument(page: Page, word: string, message: string): Promise<void> {
  await expect(documentEditor(page), `${message}: the editor has the focus`).toBeFocused();
  await expect
    .poll(
      async () => {
        const caret = await documentCaret(page);
        if (caret === null) return "no caret in the editor";
        const at = caret.paragraph.indexOf(word);
        return at >= 0 && caret.start >= at && caret.end <= at + word.length
          ? `inside "${word}"`
          : `at ${caret.start}-${caret.end} of "${caret.paragraph}"`;
      },
      { message: `${message}: the caret is inside "${word}"` }
    )
    .toBe(`inside "${word}"`);
}

/** How late the late-frame rows make each animation frame: three times the 100 ms an armed press
 *  waits for one before it goes on without it (`FRAME_WAIT_MS` in
 *  `web/src/components/focus-place.ts`). */
const LATE_FRAME_MS = 300;

/** Runs every animation frame the page asks for from here on `ms` late. This is how a loaded
 *  machine's late frame reaches the page: its timers run while its frames wait. */
async function delayFrames(page: Page, ms: number): Promise<void> {
  await page.evaluate((delay) => {
    const frame = window.requestAnimationFrame.bind(window);
    window.requestAnimationFrame = (callback) =>
      frame((time) => {
        window.setTimeout(() => callback(time), delay);
      });
  }, ms);
}

// SLOW(>=2s): the spec opened and connected, four comments started on it, and one of them sent.
test("k-comment-focus: at 1440x900 each new comment puts the caret in the margin's box, with a box open and after the margin was hidden, and Escape and a send put it back in the commented word", async ({
  page,
}) => {
  const seed = await seedCommentFocus();
  const step = (text: string) => `k-comment-focus: at 1440x900, ${text}`;
  await page.setViewportSize(WIDE);
  await openSpec(page, seed.key, FOCUS_PARAGRAPHS[2][0]);

  await commentByKey(
    page,
    FIRST_WORD,
    step("Ctrl+Alt+M on the first word puts the caret in the box")
  );
  await commentByKey(
    page,
    SECOND_WORD,
    step("Ctrl+Alt+M on the second word with the box open puts the caret in the box again")
  );
  await page.keyboard.press("Escape");
  await expect(commentBox(page), step("Escape in the empty box closes it")).toHaveCount(0);
  await expectCaretInDocument(page, SECOND_WORD, step("Escape in the empty box"));

  await selectEditorText(page, THIRD_WORD);
  await barAction(page, "Comment");
  await expectCaretInBox(page, THIRD_WORD, step("the bar's Comment puts the caret in the box"));
  const body = "Why does the lease return at noon?";
  await page.keyboard.type(body);
  await page.keyboard.press("Control+Enter");
  await expect(commentBox(page), step("a sent comment closes the box")).toHaveCount(0);
  const sent = await commentWithBody(seed.key, seed.artifactId, body);
  expect(sent.anchor?.quote, step("the comment is sent on the third word")).toBe(THIRD_WORD);
  await expectCaretInDocument(page, THIRD_WORD, step("a sent comment"));

  // Escape takes the focus out of the editor, where Shift+M would type a letter.
  await page.keyboard.press("Escape");
  await page.keyboard.press("Shift+M");
  await expect(page.getByTestId("margin-rail"), step("Shift+M hides the margin")).toBeVisible();
  await page.keyboard.press("Shift+M");
  await expect(page.getByTestId("margin-rail"), step("Shift+M shows the margin")).toHaveCount(0);
  await commentByKey(
    page,
    FIRST_WORD,
    step("after the margin was hidden and shown, Ctrl+Alt+M puts the caret in the box")
  );
});

test("k-comment-focus: at 1440x900 a sent comment puts the caret back in the commented word while every frame of the page comes late", async ({
  page,
}) => {
  const seed = await seedCommentFocus();
  const step = (text: string) => `k-comment-focus: at 1440x900 with late frames, ${text}`;
  await page.setViewportSize(WIDE);
  await openSpec(page, seed.key, FOCUS_PARAGRAPHS[2][0]);

  await selectEditorText(page, THIRD_WORD);
  await barAction(page, "Comment");
  await expectCaretInBox(page, THIRD_WORD, step("the bar's Comment puts the caret in the box"));
  await page.keyboard.type("Why does the lease return at noon?");
  await delayFrames(page, LATE_FRAME_MS);
  await page.keyboard.press("Control+Enter");
  await expect(commentBox(page), step("a sent comment closes the box")).toHaveCount(0);
  await expectCaretInDocument(page, THIRD_WORD, step("a sent comment"));
});

test("k-comment-focus: at 1024x768 a sent comment closes the sheet and puts the caret back in the commented word while every frame of the page comes late", async ({
  page,
}) => {
  const seed = await seedCommentFocus();
  const step = (text: string) => `k-comment-focus: at 1024x768 with late frames, ${text}`;
  const sheet = page.getByTestId("margin-sheet");
  await page.setViewportSize(SHEET_VIEWPORT);
  await openSpec(page, seed.key, FOCUS_PARAGRAPHS[2][0]);

  await commentByKey(
    page,
    THIRD_WORD,
    step("Ctrl+Alt+M on the third word puts the caret in the box")
  );
  await expect(sheet, step("the comment opens the sheet")).toHaveAttribute("data-expanded", "true");
  await page.keyboard.type("Why does the lease return at noon?");
  await delayFrames(page, LATE_FRAME_MS);
  await page.keyboard.press("Control+Enter");
  await expect(sheet, step("a sent comment closes the sheet")).toHaveAttribute(
    "data-expanded",
    "false"
  );
  await expectCaretInDocument(page, THIRD_WORD, step("a sent comment"));
});

// SLOW(>=2s): the spec opened and connected at a sheet's width, three comments started on it, the
// sheet opened and closed three times, and one comment sent.
test("k-comment-focus: at 1024x768, where the margin is a sheet, each new comment puts the caret in the box, Escape over a kept draft asks Discard draft? and keeps the sheet open, and once the draft is discarded Escape in the empty box and a send close the sheet and put the caret back in the commented word", async ({
  page,
}) => {
  const seed = await seedCommentFocus();
  const step = (text: string) => `k-comment-focus: at 1024x768, ${text}`;
  const sheet = page.getByTestId("margin-sheet");
  await page.setViewportSize(SHEET_VIEWPORT);
  await openSpec(page, seed.key, FOCUS_PARAGRAPHS[2][0]);

  await commentByKey(
    page,
    FIRST_WORD,
    step("Ctrl+Alt+M on the first word puts the caret in the box")
  );
  await expect(sheet, step("the comment opens the sheet")).toHaveAttribute("data-expanded", "true");
  const draft = "Half a thought";
  await page.keyboard.type(draft);
  await page.getByRole("button", { name: /^Close review panel/ }).click();
  await expect(sheet, step("the sheet's close control closes it")).toHaveAttribute(
    "data-expanded",
    "false"
  );

  await commentByKey(
    page,
    SECOND_WORD,
    step("Ctrl+Alt+M on a word of another paragraph puts the caret in the box again")
  );
  await expect(sheet, step("the second comment opens the sheet")).toHaveAttribute(
    "data-expanded",
    "true"
  );
  // The second comment draws a box of its own with an empty field, and the first box's draft
  // waits beside it (`KeptDraftOffers`) until the reader takes it into the field or drops it.
  const box = commentBox(page);
  await expect(box.getByLabel("Comment"), step("the second comment's field is empty")).toHaveValue(
    ""
  );
  const offer = box.getByRole("status").filter({ hasText: `for other words: “${FIRST_WORD}”` });
  await expect(
    offer.locator("blockquote"),
    step("the first box's draft is kept beside the field")
  ).toHaveText(draft);
  await expect(
    box.getByRole("button", { name: "Use my draft" }),
    step("the kept draft has the press that takes it into the field")
  ).toBeVisible();
  // With the kept draft offered the box is not yet empty, so Escape there asks `Discard draft?`
  // and keeps the box and the sheet open (`closeOrAsk`, main's kept-draft offer of
  // cybermonklabs/legion#3000). The row answers the offer with Discard my draft, which leaves the
  // field empty with nothing offered. The focus stays in the box, where the box's Escape listener
  // reads the key.
  await page.keyboard.press("Escape");
  await expect(
    sheet.getByRole("alert").filter({ hasText: "Discard draft?" }),
    step("Escape with the kept draft offered asks Discard draft?")
  ).toBeVisible();
  await expect(
    sheet,
    step("Escape with the kept draft offered keeps the sheet open")
  ).toHaveAttribute("data-expanded", "true");
  await expect(box, step("Escape with the kept draft offered keeps the box")).toBeVisible();
  await box.getByRole("button", { name: "Discard my draft" }).press("Enter");
  await expect(offer, step("Discard my draft drops the kept draft")).toHaveCount(0);
  await expect
    .poll(() => box.evaluate((form) => form.contains(document.activeElement)), {
      message: step("the focus stays in the box once the kept draft is dropped"),
    })
    .toBe(true);
  await page.keyboard.press("Escape");
  await expect(sheet, step("Escape in the empty box closes the sheet")).toHaveAttribute(
    "data-expanded",
    "false"
  );
  await expect(commentBox(page), step("Escape in the empty box closes it")).toHaveCount(0);
  await expectCaretInDocument(page, SECOND_WORD, step("Escape in the empty box"));

  await commentByKey(
    page,
    THIRD_WORD,
    step("Ctrl+Alt+M on the third word puts the caret in the box")
  );
  const body = "Why does the lease return at noon?";
  await page.keyboard.type(body);
  await page.keyboard.press("Control+Enter");
  await expect(sheet, step("a sent comment closes the sheet")).toHaveAttribute(
    "data-expanded",
    "false"
  );
  const sent = await commentWithBody(seed.key, seed.artifactId, body);
  expect(sent.anchor?.quote, step("the comment is sent on the third word")).toBe(THIRD_WORD);
  await expectCaretInDocument(page, THIRD_WORD, step("a sent comment"));
});

// ---------------------------------------------------------------------------
// k-keyboard-reach (R12): on each page R12 names, every control the page shows takes the focus
// from Tab - or, in a group whose members share one Tab stop by the WAI-ARIA Authoring Practices
// (a group of radios, a tab strip, a menu), Tab reaches the group's stop and the group's arrow
// keys the member (departure D3) - or is a row or a card the page's own keys reach. On the
// Inbox, a control that shows while the pointer is over a row shows too while the row has the
// focus.
// ---------------------------------------------------------------------------

/** A group of controls that share one Tab stop, the arrow key that moves among them, and how many
 *  members the page holds, shown or not: a tab its strip has scrolled out of view is one. */
interface ReachGroup {
  readonly id: number;
  readonly arrow: "ArrowDown" | "ArrowRight";
  readonly size: number;
}

/** A control a page shows: where it stands in the page's list, the name a failure gives it - its
 *  role, its name and the test id of the part it is drawn in - and the group it shares one Tab
 *  stop with, where it has one. */
interface ShownControl {
  readonly index: number;
  readonly name: string;
  readonly group: ReachGroup | null;
}

/** Where the focus is after a key: the listed control that holds it, or -1, whether it is back on
 *  the first element a Tab of the walk gave it to, and how many elements the list does not hold
 *  took the focus since the list was read, each counted once (`others`). */
interface FocusAt {
  readonly index: number;
  readonly back: boolean;
  readonly others: number;
}

/** What `installReach` leaves on the page's `window`. */
interface KeyboardReach {
  /** Lists the controls the page shows inside `root`. */
  list(root: Element): ShownControl[];
  /** Of the listed controls, the ones still in the page, by their place in the list: connected,
   *  with a box of their own (client rects). */
  kept(): number[];
  /** The names of the controls `root` shows, by `list`'s reading. */
  shownIn(root: Element): string[];
  at(): FocusAt;
  focus(index: number): void;
  /** Every listed control that took the focus since it was listed, for however short a time: a
   *  radio the arrows check can hand the focus on at once, as Other hands it to its answer box. */
  focused(): number[];
  /** The listed controls a row key reached: the one with the focus, and the row or card the key
   *  focused or selected, where it is one of them. */
  rowsAt(rows: string): number[];
}

declare global {
  interface Window {
    __keyboardReach?: KeyboardReach;
  }
}

/** Puts R12's reading of `page` on its `window`, so each key press reads where the focus went in
 *  one call. What it puts there runs in the page, and reads nothing of this module's: what R12
 *  counts as a control - a link, a button, a checkbox, a radio, a select, a menu trigger, a text
 *  field and a tab, a disclosure's `<summary>` among the buttons, and a table column's grip, the
 *  one separator that takes the focus (`role="separator"` with a `tabindex`, the architect's
 *  ruling of dispatch://INFRA-2160/comment/fe52ffac-4f51-4669-be7e-f26e8994fc13) - shown in the
 *  viewport, leaving out a control that takes no press (`:disabled`) and a link inside a
 *  document the reader can edit, which R2's Alt+Enter opens rather than Tab. */
async function installReach(page: Page): Promise<void> {
  await page.evaluate(() => {
    // Once for each document: a page the app moves to in place keeps what it put here.
    if (window.__keyboardReach !== undefined) return;
    const controls = [
      "a[href]",
      "button",
      "summary",
      "select",
      "textarea",
      'input:not([type="hidden"])',
      '[contenteditable]:not([contenteditable="false"])',
      '[role="button"]',
      '[role="link"]',
      '[role="checkbox"]',
      '[role="switch"]',
      '[role="radio"]',
      '[role="tab"]',
      '[role="combobox"]',
      '[role="textbox"]',
      '[role="searchbox"]',
      '[role="menuitem"]',
      '[role="menuitemradio"]',
      '[role="menuitemcheckbox"]',
      '[aria-haspopup]:not([aria-haspopup="false"])',
      '[role="separator"][tabindex]',
    ].join(", ");
    let listed: Element[] = [];
    let first: Element | null = null;
    const focused = new Set<number>();
    // The elements outside `listed` that took the focus since `list`: a control of a row or a
    // column the viewport did not show when the list was read, which Tab stops on on its way.
    const others = new Set<Element>();
    const groups: { readonly name: string; readonly node: Node; readonly size: number }[] = [];
    document.addEventListener(
      "focusin",
      (event) => {
        const index = event.target instanceof Element ? listed.indexOf(event.target) : -1;
        if (index !== -1) focused.add(index);
        else if (event.target instanceof Element && event.target !== document.body) {
          others.add(event.target);
        }
      },
      true
    );

    /** Whether `element` shows in the viewport: drawn, not hidden, not see-through, and inside every
     *  box that clips it. */
    const shown = (element: Element): boolean => {
      if (element.getClientRects().length === 0) return false;
      if (getComputedStyle(element).visibility !== "visible") return false;
      if (element.closest("[inert]") !== null) return false;
      const box = element.getBoundingClientRect();
      let left = Math.max(box.left, 0);
      let top = Math.max(box.top, 0);
      let right = Math.min(box.right, window.innerWidth);
      let bottom = Math.min(box.bottom, window.innerHeight);
      let clipped = true;
      for (let node: Element | null = element; node !== null; node = node.parentElement) {
        const style = getComputedStyle(node);
        if (Number(style.opacity) === 0) return false;
        const clips =
          node !== element &&
          clipped &&
          style.display !== "contents" &&
          (style.overflowX !== "visible" || style.overflowY !== "visible");
        if (clips) {
          const clip = node.getBoundingClientRect();
          left = Math.max(left, clip.left);
          top = Math.max(top, clip.top);
          right = Math.min(right, clip.right);
          bottom = Math.min(bottom, clip.bottom);
        }
        // A box the viewport places is clipped by none of the boxes around it.
        if (style.position === "fixed") clipped = false;
      }
      return right - left >= 1 && bottom - top >= 1;
    };

    /** The text `node` shows assistive tech: its text without what it hides from it. */
    const textOf = (node: Element): string => {
      const copy = node.cloneNode(true) as Element;
      for (const hidden of copy.querySelectorAll('[aria-hidden="true"]')) hidden.remove();
      return copy.textContent ?? "";
    };

    const nameOf = (element: Element): string => {
      const labelledBy = element.getAttribute("aria-labelledby");
      const labels =
        element instanceof HTMLInputElement ||
        element instanceof HTMLTextAreaElement ||
        element instanceof HTMLSelectElement
          ? [...(element.labels ?? [])].map(textOf).join(" ")
          : "";
      const label =
        labelledBy === null
          ? (element.getAttribute("aria-label") ??
            (labels ||
              textOf(element) ||
              element.getAttribute("title") ||
              element.getAttribute("placeholder") ||
              ""))
          : labelledBy
              .split(/\s+/)
              .map((id) => {
                const part = document.getElementById(id);
                return part === null ? "" : textOf(part);
              })
              .join(" ");
      const role =
        element.getAttribute("role") ??
        (element instanceof HTMLInputElement
          ? `${element.type} input`
          : element.tagName.toLowerCase());
      const part = element.closest("[data-testid]")?.getAttribute("data-testid");
      const shortLabel = label.replace(/\s+/g, " ").trim().slice(0, 80);
      return `${role} "${shortLabel}"${part === undefined ? "" : ` in ${part}`}`;
    };

    /** The group `element` shares one Tab stop with: the radios of one name in one form, the radios
     *  of a `radiogroup`, the tabs of a `tablist`, the items of a `menu` or a `menubar`. Its size
     *  is read as the group is first met: every member the page holds that takes a press, shown
     *  or not, as the group's arrow keys skip a disabled one. */
    const groupOf = (element: Element): ReachGroup | null => {
      const groupAt = (
        node: Node,
        name: string,
        arrow: ReachGroup["arrow"],
        members: () => number
      ): ReachGroup => {
        const known = groups.findIndex((group) => group.node === node && group.name === name);
        const id = known === -1 ? groups.push({ name, node, size: members() }) - 1 : known;
        return { arrow, id, size: groups[id]?.size ?? 0 };
      };
      if (element instanceof HTMLInputElement && element.type === "radio") {
        if (element.name === "") return null;
        const scope = element.form ?? document;
        return groupAt(
          scope,
          element.name,
          "ArrowDown",
          () =>
            [...scope.querySelectorAll('input[type="radio"]:not(:disabled)')].filter(
              (radio) =>
                radio instanceof HTMLInputElement &&
                radio.name === element.name &&
                radio.form === element.form
            ).length
        );
      }
      const role = element.getAttribute("role") ?? "";
      const owners =
        role === "radio"
          ? '[role="radiogroup"]'
          : role === "tab"
            ? '[role="tablist"]'
            : role.startsWith("menuitem")
              ? '[role="menu"], [role="menubar"]'
              : null;
      if (owners === null) return null;
      const owner = element.closest(owners);
      if (owner === null) return null;
      const across =
        owner.getAttribute("role") === "menubar" ||
        (owner.getAttribute("role") === "tablist" &&
          owner.getAttribute("aria-orientation") !== "vertical");
      const kind = role.startsWith("menuitem") ? '[role^="menuitem"]' : `[role="${role}"]`;
      return groupAt(
        owner,
        "",
        across ? "ArrowRight" : "ArrowDown",
        () =>
          [...owner.querySelectorAll(`${kind}:not(:disabled)`)].filter(
            (member) => member.closest(owners) === owner
          ).length
      );
    };

    const controlsIn = (root: Element | Document): Element[] =>
      [...root.querySelectorAll(controls)].filter(
        (element) =>
          shown(element) &&
          !element.matches(":disabled") &&
          // An editor's own nodes are part of its editing host, which is the one text field.
          !(
            element.matches("[contenteditable]") &&
            element.parentElement instanceof HTMLElement &&
            element.parentElement.isContentEditable
          ) &&
          // A document drawn read-only keeps its editor's textbox role but takes no typed text,
          // so it is no text field; the links in it are listed on their own.
          !(
            element.matches('[role="textbox"]:not(input, textarea)') &&
            element instanceof HTMLElement &&
            !element.isContentEditable
          ) &&
          !(element.matches("a[href]") && element.closest('[contenteditable="true"]') !== null)
      );

    window.__keyboardReach = {
      at: () => {
        const active = document.activeElement;
        const index = active === null ? -1 : listed.indexOf(active);
        let back = false;
        if (active !== null && active !== document.body && active !== document.documentElement) {
          if (first === null) first = active;
          else back = active === first;
        }
        return { back, index, others: others.size };
      },
      kept: () =>
        listed.flatMap((element, index) =>
          element.isConnected && element.getClientRects().length > 0 ? [index] : []
        ),
      focus: (index) => {
        const element = listed[index];
        if (element instanceof HTMLElement || element instanceof SVGElement) element.focus();
      },
      focused: () => [...focused],
      list: (root) => {
        listed = controlsIn(root);
        first = null;
        focused.clear();
        others.clear();
        groups.length = 0;
        return listed.map((element, index) => ({
          group: groupOf(element),
          index,
          name: nameOf(element),
        }));
      },
      rowsAt: (rows) => {
        const active = document.activeElement;
        const reached = [
          active,
          active?.closest(rows) ?? null,
          ...document.querySelectorAll(
            `:is(${rows}):is([aria-current="true"], [aria-selected="true"], [data-inbox-selected="true"])`
          ),
        ];
        return reached.flatMap((element) => {
          const index = element === null ? -1 : listed.indexOf(element);
          return index === -1 ? [] : [index];
        });
      },
      shownIn: (root) => controlsIn(root).map(nameOf),
    };
  });
}

/** A point inside `row` that is on none of its controls and none of its references, whose card
 *  a pointer over one opens; in the page's own px from the viewport's top left corner. */
function pointOffReferences(row: Element): { x: number; y: number } {
  const box = row.getBoundingClientRect();
  const taken =
    'a, button, input, select, textarea, summary, label, [role="button"], [role="link"], [aria-haspopup], [data-dispatch-ref], [data-dispatch-href]';
  for (let y = Math.max(box.top, 0) + 3; y < Math.min(box.bottom, window.innerHeight) - 2; y += 5) {
    for (let x = box.left + 3; x < box.right - 2; x += 5) {
      const under = document.elementFromPoint(x, y);
      if (under !== null && row.contains(under) && under.closest(taken) === null) return { x, y };
    }
  }
  throw new Error("the row shows no place off its controls and its references");
}

/** The rows or the cards of a page and the key that walks them. */
interface PageRows {
  readonly selector: string;
  readonly key: string;
}

/** A page k-keyboard-reach reads: its name in the row's messages, the dialog it is drawn in where
 *  it is one (the drawer), whose controls alone are listed, and its rows or cards. */
interface ReachPage {
  readonly name: string;
  readonly scope?: Locator;
  readonly rows?: PageRows;
}

/** Turns off the page's eases of colour and opacity, so a control a pointer or the focus shows
 *  is read shown, and one it hides read hidden, in the frame after it changed rather than part of
 *  the way through, as `e2e/accessibility.e2e.ts` reads every colour. */
async function withoutEases(page: Page): Promise<void> {
  await page.addStyleTag({
    content: "*, *::before, *::after { transition: none !important; animation: none !important; }",
  });
}

/** Holds `page`, drawn as `at` names it, to R12: each control it shows, and still holds once Tab
 *  has walked it, takes the focus from Tab, or from its group's arrow keys once Tab reached the
 *  group's stop, or is a row or a card the page's own keys reach. The caller has waited for the
 *  page to draw what it shows. */
async function expectKeyboardReach(page: Page, at: ReachPage): Promise<void> {
  const step = (text: string) => `k-keyboard-reach: ${at.name}: ${text}`;
  await installReach(page);
  // The caller waited for the page to draw what it shows, so one reading lists all of it.
  const controls = await (at.scope ?? page.locator("body")).evaluate(
    (root) => window.__keyboardReach?.list(root) ?? []
  );
  expect(controls.length, step("the page shows controls")).toBeGreaterThan(0);
  const reached = new Set<number>();
  /** Adds every listed control that took the focus so far, however briefly. */
  const addFocused = async () => {
    // Each focus the page took was recorded as it landed, so one reading holds all of them.
    for (const index of await page.evaluate(() => window.__keyboardReach?.focused() ?? [])) {
      reached.add(index);
    }
  };

  // Tab until the focus comes back to the first element it took, or at most twice as many presses
  // as the page shows controls and as the other elements that took the focus in the walk, each
  // counted once (Verification version 23): on a page from its start, and in a dialog, which
  // keeps the focus inside it, from the control the dialog put the focus on as it opened. Tab
  // stops on the controls of the rows and the columns outside the viewport too, which the list
  // does not hold, on its way to a control below them: the project's List draws its pager after
  // 24 rows, and its walk reaches the pager at the 180th press of a page that lists 89 controls
  // (builds Job `infra-2160-pr-reach-gvjzg`). A walk that goes round the same stops adds nothing,
  // so a focus trap or a cycle still ends it, and the row fails by name.
  if (at.scope === undefined) {
    await page.evaluate(() => {
      const active = document.activeElement;
      if (active instanceof HTMLElement) active.blur();
    });
  } else {
    // The dialog put the focus on its control as it opened, which the caller waited for.
    const start = await page.evaluate(
      () => window.__keyboardReach?.at() ?? { back: false, index: -1 }
    );
    if (start.index >= 0) reached.add(start.index);
  }
  let others = 0;
  for (let press = 0; press < 2 * (controls.length + others); press += 1) {
    await page.keyboard.press("Tab");
    // A Tab moves the focus inside its own key press, so this reads where it settled.
    const focus = await page.evaluate(
      () => window.__keyboardReach?.at() ?? { back: true, index: -1, others: 0 }
    );
    if (focus.back) break;
    if (focus.index >= 0) reached.add(focus.index);
    others = focus.others;
  }
  await addFocused();

  // The walk owes a stop to each listed control the page still holds once Tab has walked it, read
  // here, before the arrows and the row keys, which can take a control away themselves: `j`
  // beside the reading pane selects another item and draws a head of its own, and a tab strip's
  // arrow moves a past version's page to another tab. Only a control the Tab walk took out of the
  // page, or left with no box, is owed no stop. The Conversation's `Jump to latest` pill is one:
  // it shows when a turn that reaches the page over the live socket after its first read leaves
  // the window off the newest card (`e2e/review-routes.ts`, the `issue-conversation` route), and
  // the walk's first Tab scrolls the window back to the page's top, which takes the pill away
  // before any Tab reaches it. Each Tab moved the focus inside its own key press, so this one
  // reading says what the page holds once the walk has ended.
  const keptAfterTab = await page.evaluate(() => window.__keyboardReach?.kept() ?? []);

  // A group's members from the stop Tab reached, by the group's arrow key, once round the whole
  // group and back to that stop. The round counts every member the page holds, not only the
  // ones it shows: the issue page's ten tabs overflow their strip beside the margin, so the tabs
  // scrolled out of view are no listed control, and a round of the listed ones alone would end
  // short of the stop, on a tab whose panel then hides the one the page was opened on.
  const groups = new Map<number, ShownControl[]>();
  for (const control of controls) {
    if (control.group !== null) {
      groups.set(control.group.id, [...(groups.get(control.group.id) ?? []), control]);
    }
  }
  for (const members of groups.values()) {
    const stop = members.find((member) => reached.has(member.index));
    const group = members[0]?.group;
    if (
      stop === undefined ||
      group === null ||
      group === undefined ||
      members.every((member) => reached.has(member.index))
    ) {
      continue;
    }
    await page.evaluate((index) => window.__keyboardReach?.focus(index), stop.index);
    for (let press = 0; press < group.size; press += 1) {
      await page.keyboard.press(group.arrow);
      // A group's arrow moves the focus inside its own key press, so this reads where it settled.
      const focus = await page.evaluate(
        () => window.__keyboardReach?.at() ?? { back: true, index: -1 }
      );
      if (focus.index >= 0) reached.add(focus.index);
    }
  }

  // The rows or the cards, by the page's own key, from the page body.
  if (at.rows !== undefined) {
    const rows = page.locator(at.rows.selector);
    await expect(rows.first(), step("the page draws its rows")).toBeVisible();
    // The first row was waited for above, and the page draws every row of its list with it.
    const count = await rows.count();
    await page.evaluate(() => {
      const active = document.activeElement;
      if (active instanceof HTMLElement) active.blur();
    });
    for (let press = 0; press < count; press += 1) {
      await page.keyboard.press(at.rows.key);
      // A row key moves the focus or the selection inside its own key press, so this reads it.
      const hit = await page.evaluate(
        (selector) => window.__keyboardReach?.rowsAt(selector) ?? [],
        at.rows.selector
      );
      for (const index of hit) reached.add(index);
    }
  }
  await addFocused();

  const takenAway = controls
    .filter((control) => !keptAfterTab.includes(control.index))
    .map((control) => control.name);
  expect(
    controls
      .filter((control) => keptAfterTab.includes(control.index) && !reached.has(control.index))
      .map((control) => control.name),
    step(
      `each control the page shows takes the focus from Tab, or from its group's arrow keys once Tab reached the group, or is a row or a card its keys reach${takenAway.length === 0 ? "" : `; the Tab walk took these out of the page, so they are owed no stop: ${takenAway.join(", ")}`}`
    )
  ).toEqual([]);
}

/** Holds each of the `count` rows of `rows` to R12's rule for a pointer: the controls that show
 *  while the pointer is over the row - at a place off every control and every reference in it,
 *  whose card a pointer over one opens - show too once the pointer has moved away, to `away`,
 *  and the row has the focus. */
async function expectFocusShowsWhatHoverShows(
  page: Page,
  rows: Locator,
  count: number,
  away: Locator,
  step: (text: string) => string
): Promise<void> {
  await installReach(page);
  await expect(rows, step("the Inbox lists its rows")).toHaveCount(count);
  for (let index = 0; index < count; index += 1) {
    const row = rows.nth(index);
    await row.scrollIntoViewIfNeeded();
    const point = await row.evaluate(pointOffReferences);
    await page.mouse.move(point.x, point.y);
    const hovered = await row.evaluate((node) => window.__keyboardReach?.shownIn(node) ?? []);
    await away.hover();
    await row.focus();
    await expect
      .poll(() => row.evaluate((node) => window.__keyboardReach?.shownIn(node) ?? []), {
        message: step(
          `row ${index + 1}: each control a pointer over it shows also shows while it has the focus: ${JSON.stringify(hovered)}`
        ),
      })
      .toEqual(expect.arrayContaining(hovered));
  }
}

/** k-keyboard-reach on the Inbox where `place` draws it, over k-every-action's items: a question,
 *  a question with options, an approval request, a reply thread and an agent thread, and a
 *  snoozed question folded in Later. At 1024x768 the short-row list is walked, and then the
 *  reading view the first row opens in the list's place. */
async function reachInboxSteps(
  page: Page,
  place: "the reading pane" | "the short-row list" | "the drawer"
): Promise<void> {
  await seedEveryAction();
  const inbox = await openInboxAt(page, place, 5);
  await withoutEases(page);
  if (place === "the reading pane") {
    await expect(paneHead(page)).toBeVisible();
  }
  if (place === "the drawer") {
    await expect(inbox.scope.getByRole("button", { exact: true, name: "Close" })).toBeFocused();
  }
  const where = place === "the short-row list" ? "the short-row list at 1024x768" : place;
  const step = (text: string) => `k-keyboard-reach: the Inbox in ${where}: ${text}`;
  await expectKeyboardReach(page, {
    name: `the Inbox in ${where}`,
    rows: { key: "j", selector: "[data-inbox-row]" },
    scope: place === "the drawer" ? inbox.scope : undefined,
  });
  await expectFocusShowsWhatHoverShows(
    page,
    inbox.scope.locator("[data-inbox-row]"),
    5,
    inbox.outside,
    step
  );
  if (place !== "the short-row list") return;
  await selectRow(page, itemAt(inbox.order, 0));
  await expect(paneHead(page), step("the first row opens its reading view")).toBeVisible();
  await expectKeyboardReach(page, { name: "the Inbox's reading view at 1024x768" });
}

// SLOW(>=2s): five items seeded, the page walked by Tab and j, and each row hovered and focused.
test("k-keyboard-reach: on the Inbox beside the reading pane, Tab reaches every control, and a row's focus shows each control a pointer over the row shows", async ({
  page,
}) => {
  await reachInboxSteps(page, "the reading pane");
});

// SLOW(>=2s): five items seeded, the short rows walked by Tab and j and each hovered and focused,
// then a row's reading view walked by Tab.
test("k-keyboard-reach: on the Inbox at 1024x768, Tab reaches every control of the short-row list and of a row's reading view, and a row's focus shows each control a pointer over the row shows", async ({
  page,
}) => {
  await reachInboxSteps(page, "the short-row list");
});

// SLOW(>=2s): five items seeded, the drawer opened over a project page, walked by Tab and j, and
// each row hovered and focused.
test("k-keyboard-reach: in the Inbox drawer, Tab reaches every control, and a row's focus shows each control a pointer over the row shows", async ({
  page,
}) => {
  await reachInboxSteps(page, "the drawer");
});

/** Opens the review corpus's page `name` (`e2e/review-routes.ts`) and waits for it to draw what
 *  it shows, by that page's own locators. */
async function openReviewRoute(page: Page, seed: ReviewSeed, name: string): Promise<void> {
  const route = reviewRoutes.find(
    (entry): entry is ReviewedRoute => entry.kind === "reviewed" && entry.name === name
  );
  if (route === undefined) throw new Error(`e2e/review-routes.ts reviews no page named ${name}`);
  await page.goto(route.path(seed));
  await waitForRouteSettled(page, route, listedRequests(page));
  await withoutEases(page);
}

for (const [tab] of ISSUE_TABS) {
  // SLOW(>=2s): the review corpus seeded, and the issue page walked by Tab on one of its tabs.
  test(`k-keyboard-reach: on today's issue page, its header and its ${tab} tab, Tab reaches every control`, async ({
    page,
  }) => {
    const seed = await seedReviewRoutes({ boundMs: expectTimeout });
    await page.setViewportSize(WIDE);
    if (tab === "collaborators") {
      await page.goto(`/issues/${seed.issue}/collaborators`);
      await expect(
        page
          .locator("#issue-collaborators-panel")
          .locator('section[aria-label], [aria-label="Collaborators empty state"]')
          .first()
      ).toBeVisible();
      await withoutEases(page);
    } else if (tab === "steps") {
      // No reviewed route draws the Steps tab (`e2e/review-routes.ts`), and the corpus writes no
      // step record, so the tab has answered once it draws its empty state.
      await page.goto(`/issues/${seed.issue}/steps`);
      await expect(
        page.locator("#issue-steps-panel").getByRole("region", { name: "Steps empty state" })
      ).toBeVisible();
      await withoutEases(page);
    } else {
      await openReviewRoute(page, seed, `issue-${tab}`);
    }
    await expectKeyboardReach(page, {
      name: `today's issue page on its ${tab} tab`,
      rows: tab === "conversation" ? { key: "j", selector: "[data-turn]" } : undefined,
    });
  });
}

// SLOW(>=2s): a spec at version 2 and a project document seeded, and both pages walked by Tab.
test("k-keyboard-reach: on a project document page and on a past version of a spec, Tab reaches every control, the title drawn for a bare key included", async ({
  page,
}) => {
  const seed = await seedLinkSpec();
  const document = await createProjectDocument("CORE", {
    content: `# Replay runbook\n\nRead [a page elsewhere](${AWAY}) and the handover in ${seed.other.key}.\n`,
    name: "replay-runbook.md",
  });
  await page.setViewportSize(WIDE);

  await page.goto(`/projects/CORE/documents/${document.artifact.slug}`);
  await expect(page.getByRole("status", { name: "connected" })).toHaveText("connected");
  await withoutEases(page);
  await expectKeyboardReach(page, { name: "a project document page" });

  const links = await openLinkView(page, seed, "the past version");
  await withoutEases(page);
  await expect(links.key, "k-keyboard-reach: the past version draws the key's title").toBeVisible();
  await expectKeyboardReach(page, { name: "a past version of the spec" });
});

// SLOW(>=2s): the review corpus seeded, and the project's List and Board each walked by Tab and j.
test("k-keyboard-reach: on a project's List and Board, Tab, or j for a row or a card, reaches every control", async ({
  page,
}) => {
  const seed = await seedReviewRoutes({ boundMs: expectTimeout });
  await page.setViewportSize(WIDE);
  await openIssuesTab(page, seed.project, "List");
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  await withoutEases(page);
  await expectKeyboardReach(page, {
    name: "the project's List",
    rows: { key: "j", selector: "[data-issue-row]" },
  });
  await openIssuesTab(page, seed.project, "Board");
  await expect(page.locator("[data-board-card]").first()).toBeVisible();
  await withoutEases(page);
  await expectKeyboardReach(page, {
    name: "the project's Board",
    rows: { key: "j", selector: "[data-board-card]" },
  });
});

/** A column of a shared table, worked by the keyboard alone (R12, by the architect's ruling of
 *  dispatch://INFRA-2160/comment/fe52ffac-4f51-4669-be7e-f26e8994fc13): Tab from the page's start
 *  reaches the column's grip; ArrowRight widens the header's box, Shift+ArrowLeft then narrows it
 *  below where it began, and Enter gives it back that first width; and the column's Move menu,
 *  two Shift+Tabs back past its filter, moves it one place right, which swaps it with the column
 *  on its right, and leaves the focus on the menu's trigger. The caller names a column that
 *  starts wider than its minimum, with a filter and a column on its right that is not pinned. */
async function expectColumnKeys(
  page: Page,
  at: { readonly column: string; readonly name: string; readonly table: Locator }
): Promise<void> {
  const step = (text: string) => `k-keyboard-reach: ${at.name}: ${text}`;
  const grip = at.table.getByRole("separator", { exact: true, name: `Resize ${at.column}` });
  // A `has` locator is read inside each header, so it starts from the page, not from the table.
  const header = at.table.getByRole("columnheader").filter({
    has: page.getByRole("separator", { exact: true, name: `Resize ${at.column}` }),
  });
  const width = async () => (await header.boundingBox())?.width ?? 0;
  const order = () =>
    at.table
      .getByRole("columnheader")
      .evaluateAll((cells) =>
        cells.map((cell) =>
          (cell.querySelector("button")?.textContent ?? cell.textContent ?? "")
            .replace(/[▲▼]/g, "")
            .trim()
        )
      );

  await page.evaluate(() => {
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
  });
  await tabTo(page, grip, step(`Tab reaches the ${at.column} column's grip`));
  // The grip took the focus inside the Tab's own key press, and the header it stands in was drawn
  // with the rows the caller waited for, so its box is read at rest.
  const first = await width();
  expect(first, step(`the ${at.column} column has a box`)).toBeGreaterThan(0);
  await page.keyboard.press("ArrowRight");
  await expect
    .poll(width, { message: step("ArrowRight on the grip widens the column") })
    .toBeGreaterThan(first);
  await page.keyboard.press("Shift+ArrowLeft");
  await expect
    .poll(width, {
      message: step("Shift+ArrowLeft narrows it by more than ArrowRight widened it"),
    })
    .toBeLessThan(first);
  await page.keyboard.press("Enter");
  await expect
    .poll(async () => Math.round(await width()), {
      message: step("Enter gives the column back its first width"),
    })
    .toBe(Math.round(first));
  await expect(grip, step("the grip keeps the focus through its keys")).toBeFocused();

  const menu = at.table.getByRole("button", { exact: true, name: `Move ${at.column}` });
  await page.keyboard.press("Shift+Tab");
  await page.keyboard.press("Shift+Tab");
  await expect(
    menu,
    step("two Shift+Tabs from the grip reach the column's Move menu")
  ).toBeFocused();
  // The header's order is drawn with the header, which the reads above waited on.
  const before = await order();
  const place = before.indexOf(at.column);
  expect(place, step(`the header row names ${at.column}`)).toBeGreaterThanOrEqual(0);
  const after = [...before];
  after.splice(place, 2, before[place + 1] ?? "", at.column);
  await page.keyboard.press("Enter");
  await expect(
    page
      .getByRole("menu", { name: `Move ${at.column}` })
      .getByRole("menuitem")
      .last(),
    step("the Move menu offers Move right")
  ).toHaveText("Move right");
  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await expect
    .poll(order, { message: step(`Move right swaps ${at.column} with the column on its right`) })
    .toEqual(after);
  await expect(menu, step("the focus stays on the moved column's Move menu")).toBeFocused();
}

// SLOW(>=2s): the review corpus seeded, and a column of the List and one of the Priorities page
// resized, given back their widths and moved, by the keyboard alone.
test("k-keyboard-reach: on a project's List and on the Priorities page, the keyboard alone resizes a column by its grip, gives it back its width, and moves it by its menu", async ({
  page,
}) => {
  const seed = await seedReviewRoutes({ boundMs: expectTimeout });
  await page.setViewportSize(WIDE);
  await openIssuesTab(page, seed.project, "List");
  await expect(page.locator("[data-issue-row]").first()).toBeVisible();
  await expectColumnKeys(page, {
    column: "Epic",
    name: "the project's List",
    table: page.getByRole("table", { name: "Project issues table" }),
  });
  await openReviewRoute(page, seed, "priorities");
  await expectColumnKeys(page, {
    column: "Owner",
    name: "the Priorities page",
    table: page.getByRole("table", { name: "Priorities table" }),
  });
});

/** The pages of the review corpus k-keyboard-reach reads on their own, with their rows. */
const REVIEW_PAGES = [
  { name: "agents", rows: { key: "j", selector: "[data-agent-row]" }, title: "the Agents page" },
  {
    name: "priorities",
    rows: { key: "j", selector: "[data-priority-row]" },
    title: "the Priorities page",
  },
  { name: "settings", rows: undefined, title: "the Settings page" },
] as const;

for (const reviewed of REVIEW_PAGES) {
  // SLOW(>=2s): the review corpus seeded, and one page of it walked by Tab and its row key.
  test(`k-keyboard-reach: on ${reviewed.title}, Tab reaches every control`, async ({ page }) => {
    const seed = await seedReviewRoutes({ boundMs: expectTimeout });
    await page.setViewportSize(WIDE);
    await openReviewRoute(page, seed, reviewed.name);
    await expectKeyboardReach(page, { name: reviewed.title, rows: reviewed.rows });
  });
}

// SLOW(>=2s): the review corpus seeded, and both views of the Delivery page walked by Tab.
test("k-keyboard-reach: on the Delivery page, on its timeline and on its list, Tab, or ArrowDown for a pull request's row, reaches every control", async ({
  page,
}) => {
  const seed = await seedReviewRoutes({ boundMs: expectTimeout });
  await page.setViewportSize(WIDE);
  await openReviewRoute(page, seed, "delivery");
  await expectKeyboardReach(page, { name: "the Delivery page's timeline" });
  await page.goto(`/delivery?${seed.deliveryQuery}&mode=list`);
  await expect(page.locator("[data-pr-row]").first()).toBeVisible();
  await withoutEases(page);
  await expectKeyboardReach(page, {
    name: "the Delivery page's list",
    rows: { key: "ArrowDown", selector: "[data-pr-row]" },
  });
});

// ---------------------------------------------------------------------------
// k-default-keys (R13): with no key changed, the `?` help lists the keys of the spec's table "The
// keys" under their scopes, on a page and in the Inbox drawer, and a menu shows the keys of the
// table's menu rows.
// ---------------------------------------------------------------------------

/** The keys of each row of the spec's table "The keys" whose scope names the Inbox, as the spec
 *  writes them (dispatch://INFRA-2160/spec, Design). Written out rather than read from the app,
 *  so the row holds the help to the spec, and runs on a build that has no catalogue of keys. */
const SPEC_INBOX_KEYS: readonly string[] = [
  // Next item, previous item.
  "j",
  "ArrowDown",
  "k",
  "ArrowUp",
  // Scroll the open document down and up, to its top and to its end.
  "Space",
  "Shift+Space",
  "g g",
  "Shift+G",
  // Into the open document, back out of it, and the answer box.
  "l",
  "Escape",
  "Enter",
  // The options of a question, and Other's digit after them.
  "1",
  "2",
  "3",
  "4",
  "5",
  "6",
  "7",
  "8",
  "9",
  "0",
  // Approve, Request changes, Reply, the snooze menu, the priority menu, Dismiss, select, open,
  // copy the reference, search.
  "a",
  "Shift+R",
  "r",
  "b",
  "h",
  "p",
  "e",
  "x",
  "o",
  "y",
  "f",
  // Open in a new tab, the filter panel, the display menu.
  "Shift+O",
  "Shift+F",
  "Shift+V",
];

/** The keys of the table's rows for today's issue page outside its Conversation tab, for the
 *  document, and for the Conversation tab, as the spec writes them. */
const SPEC_ISSUE_KEYS: readonly string[] = ["a", "Shift+R", "s", "p", "[", "]"];
const SPEC_DOCUMENT_KEYS: readonly string[] = ["Alt+Enter", "Escape"];
const SPEC_TURN_KEYS: readonly string[] = ["j", "k", "Enter", "o", "r", "y", "Shift+Y"];

/** The keys of the table's menu rows that a menu shows in its foot line, as the spec names them,
 *  each with the glyph the menu draws for it. */
const SPEC_MENU_KEYS: readonly (readonly [string, string])[] = [
  ["ArrowDown", "↓"],
  ["ArrowUp", "↑"],
  ["Home", "Home"],
  ["End", "End"],
  ["Enter", "Enter"],
  ["Escape", "Esc"],
];

/** The keys the open `?` help lists under `scope`, each as the spec writes a key: the presses of
 *  a chord joined by a space, a glyph by its key's name (↓ as ArrowDown, Esc as Escape), and a run
 *  of digits (`1 – 9, 0`) as each of its digits. */
async function helpKeys(page: Page, scope: string): Promise<string[]> {
  const section = helpSection(page, scope);
  await expect(section, `k-default-keys: ? lists a ${scope} section`).toBeVisible();
  return section.getByRole("listitem").evaluateAll((items) => {
    const names: Record<string, string> = {
      "←": "ArrowLeft",
      "↑": "ArrowUp",
      "→": "ArrowRight",
      "↓": "ArrowDown",
      "⌥": "Alt",
      Esc: "Escape",
    };
    const spelled = (token: string) =>
      token
        .split("+")
        .map((part) => names[part] ?? part)
        .join("+");
    return items.flatMap((item) => {
      const hints = item.lastElementChild;
      if (hints === null) return [];
      const run = [...hints.children]
        .filter((child) => child.tagName === "KBD")
        .map((kbd) => kbd.textContent?.trim() ?? "");
      const [from, to, tenth] = run;
      if (from !== undefined && to !== undefined) {
        const digits: string[] = [];
        for (let code = from.charCodeAt(0); code <= to.charCodeAt(0); code += 1) {
          digits.push(String.fromCharCode(code));
        }
        return tenth === undefined ? digits : [...digits, tenth];
      }
      return [...hints.children].map((alternative) =>
        [...alternative.querySelectorAll("kbd")]
          .map((kbd) => spelled(kbd.textContent?.trim() ?? ""))
          .join(" ")
      );
    });
  });
}

/** Holds the open menu `menu` to the keys of the table's menu rows: its foot line shows the keys
 *  that move the highlight, write it and close the menu, and its choices show `digits`, each in
 *  the `kbd` its option draws. */
async function expectMenuKeys(
  menu: Locator,
  digits: readonly string[],
  what: string
): Promise<void> {
  await expect(menu, `k-default-keys: ${what} opens`).toBeVisible();
  for (const [key, glyph] of SPEC_MENU_KEYS) {
    await expect(menu, `k-default-keys: ${what} shows ${key}`).toContainText(glyph);
  }
  await expect(menu.locator("kbd"), `k-default-keys: ${what} shows its digits`).toHaveText([
    ...digits,
  ]);
}

// SLOW(>=2s): the Inbox read through ? and three menus, then the issue page's Spec and
// Conversation tabs through ?.
test("k-default-keys: with no key changed, ? lists the Inbox keys of the spec's table and no other, the link's y, and the issue page's and the document's keys, and the menus show the keys of the table's menu rows", async ({
  page,
}) => {
  const step = (text: string) => `k-default-keys: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const approval = await seedApprovalAtVersionTwo("CORE", "Clock audit");
  const later = await createIssue({ force: true, project: "CORE", title: "Key rotation" });
  const snoozed = await createAsk(later.key, { question: "Rotate the keys this week?" }, asker);
  await snoozeAsk(snoozed.id, new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString());

  // The Inbox, with the approval request selected and the focus on its row.
  await page.setViewportSize(WIDE);
  await page.goto("/");
  await selectRow(page, approval.askId);
  await page.keyboard.press("?");
  // `helpKeys` waited for the section, which the help draws whole in the render that opened it.
  const inboxKeys = await helpKeys(page, "Inbox");
  for (const key of SPEC_INBOX_KEYS) {
    expect.soft(inboxKeys, step(`? lists ${key} under Inbox`)).toContain(key);
  }
  for (const key of new Set(inboxKeys)) {
    expect
      .soft(SPEC_INBOX_KEYS, step(`? lists ${key} under Inbox: a key of the table`))
      .toContain(key);
  }
  expect(
    await helpKeys(page, "Global"),
    step("? lists the focused link's y under Global")
  ).toContain("y");
  await page.keyboard.press("Escape");

  // The menus of the approval request, and of the snoozed question, show the menu rows' keys.
  const menu = openList(page);
  await expect(paneHead(page)).toBeVisible();
  await page.keyboard.press("b");
  await expectMenuKeys(menu, ["1", "2", "3", "4"], "the Snooze menu");
  await page.keyboard.press("Escape");
  await page.keyboard.press("p");
  await expectMenuKeys(menu, ["0", "1", "2", "3"], "the priority menu");
  await page.keyboard.press("Escape");
  await page.getByRole("button", { name: /^Later \(1\)/ }).click();
  await selectRow(page, snoozed.id);
  await page.keyboard.press("b");
  await expectMenuKeys(menu, ["1", "2", "3", "4", "0"], "a snoozed question's Snooze menu");
  await page.keyboard.press("Escape");

  // Today's issue page, with the focus outside the document: the issue's keys and the document's.
  await openSpec(page, approval.issueKey, "The revised plan.");
  await page.evaluate(() => {
    const active = document.activeElement;
    if (active instanceof HTMLElement) active.blur();
  });
  await page.keyboard.press("?");
  const issueKeys = await helpKeys(page, "Issue");
  for (const key of SPEC_ISSUE_KEYS) {
    expect.soft(issueKeys, step(`on the Spec tab ? lists ${key} under Issue`)).toContain(key);
  }
  const documentKeys = await helpKeys(page, "Document");
  for (const key of SPEC_DOCUMENT_KEYS) {
    expect.soft(documentKeys, step(`on the Spec tab ? lists ${key} under Document`)).toContain(key);
  }
  await page.keyboard.press("Escape");

  await page.goto(`/issues/${approval.issueKey}/conversation`);
  await expect(page.getByRole("list", { name: "Conversation turns" })).toBeVisible();
  await page.keyboard.press("?");
  const turnKeys = await helpKeys(page, "Issue");
  for (const key of SPEC_TURN_KEYS) {
    expect
      .soft(turnKeys, step(`on the Conversation tab ? lists ${key} under Issue`))
      .toContain(key);
  }
});

test("k-default-keys: in the Inbox drawer ? opens the help over it, with the Inbox's keys under Inbox and the dialog's Escape under Dialog, Escape closes only the help, gives the focus back and leaves a ticked row ticked, and the drawer's search box types ?", async ({
  page,
}) => {
  const step = (text: string) => `k-default-keys: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  const ask = await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);

  // The drawer over CORE's project page, with the focus on the question's row.
  const peek = await openInboxAt(page, "the drawer", 1);
  const row = rowOf(peek, ask.id);
  await goTo(peek, ask.id);
  await expect(row, step("in the drawer the question's row has the focus")).toBeFocused();

  await page.keyboard.press("?");
  const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await expect(help, step("? in the drawer opens the help")).toBeVisible();
  await expect(
    help.getByRole("searchbox", { name: "Filter shortcuts" }),
    step("the help over the drawer takes the focus")
  ).toBeFocused();
  const inboxKeys = await helpKeys(page, "Inbox");
  for (const key of SPEC_INBOX_KEYS) {
    expect.soft(inboxKeys, step(`in the drawer ? lists ${key} under Inbox`)).toContain(key);
  }
  expect(
    await helpKeys(page, "Dialog"),
    step("in the drawer ? lists the dialog's own Escape under Dialog, and no key of the Inbox")
  ).toEqual(["Escape"]);

  await page.keyboard.press("Escape");
  await expect(help, step("Escape closes the help")).toHaveCount(0);
  await expect(drawer(page), step("Escape out of the help leaves the drawer open")).toBeVisible();
  await expect(row, step("Escape out of the help gives the focus back to the row")).toBeFocused();

  // The drawer's keys stand down while the help stands over it, though they share its scope: with
  // the row ticked and the focus on the help's Close, where no text is typed, Escape is the help's
  // and not the Inbox's Clear the selection.
  const tick = row.getByRole("checkbox");
  await page.keyboard.press("x");
  await expect(tick, step("x in the drawer ticks the row")).toBeChecked();
  await page.keyboard.press("?");
  await expect(help, step("? on a ticked row opens the help")).toBeVisible();
  await expect(
    help.getByRole("searchbox", { name: "Filter shortcuts" }),
    step("the help over the ticked row takes the focus")
  ).toBeFocused();
  await help.getByRole("button", { exact: true, name: "Close" }).focus();
  await page.keyboard.press("Escape");
  await expect(help, step("Escape on the help's Close closes the help")).toHaveCount(0);
  await expect(tick, step("Escape on the help's Close leaves the row ticked")).toBeChecked();
  await expect(
    drawer(page),
    step("Escape on the help's Close leaves the drawer open")
  ).toBeVisible();

  // A text field of the drawer keeps `?` as a typed character.
  const search = peek.scope.getByRole("searchbox", { name: "Search asks" });
  await search.focus();
  await page.keyboard.press("?");
  await expect(search, step("? in the drawer's search box is typed")).toHaveValue("?");
  await expect(help, step("? in the drawer's search box opens no help")).toHaveCount(0);
});

// The tester's red rows of round 16 (R4, R5): the menu's own key, pressed while its control has
// the focus, opens that menu as the key does from the row or the page - on its first choice or on
// the current value, with every choice shown and nothing typed in its box - and so `p` or `s`
// then Enter writes nothing. At 9d3e0777 the closed list's trigger types the key into the box
// (`MultiSelect.tsx`, `openFromKey`): `b` shows no choice, and `p` then Enter writes P0.

/** `count` asks of CORE, each on an issue of its own at P2 in `todo`, waiting on alice: their ids
 *  and their issues' keys, in the order they were made. */
async function seedAsksAtP2(
  count: number
): Promise<{ asks: string[]; keyOf: Map<string, string> }> {
  await createProject({ key: "CORE", name: "Core" });
  const asks: string[] = [];
  const keyOf = new Map<string, string>();
  for (const title of ["Ledger", "Mirror", "Lease", "Replay", "Quota"].slice(0, count)) {
    const issue = await createIssue({ project: "CORE", title: `${title} rollout` });
    await patchIssue(issue.key, { priority: 2, status: "todo" });
    const ask = await createAsk(
      issue.key,
      { question: `Ship the ${title.toLowerCase()} rollout?` },
      asker
    );
    asks.push(ask.id);
    keyOf.set(ask.id, issue.key);
  }
  return { asks, keyOf };
}

/** The item `index` of `order`, which must hold it. */
function itemAt(order: readonly string[], index: number): string {
  const id = order[index];
  if (id === undefined) throw new Error(`the Inbox lists no item ${index + 1}`);
  return id;
}

const KEY_ON_ITS_CONTROL = [
  { check: "k-snooze-menu", control: "Snooze", key: "b", where: "the reading pane" },
  { check: "k-snooze-menu", control: "Snooze", key: "h", where: "the reading pane" },
  { check: "k-snooze-menu", control: "Snooze", key: "b", where: "the reading view" },
  { check: "k-priority-menu", control: "priority", key: "p", where: "the reading pane" },
  { check: "k-priority-menu", control: "priority", key: "p", where: "the reading view" },
  { check: "k-priority-menu", control: "priority", key: "p", where: "the issue page" },
  { check: "k-priority-menu", control: "priority", key: "p", where: "the Board" },
  { check: "k-status-menu", control: "Status", key: "s", where: "the issue page" },
] as const;

for (const { check, control, key, where } of KEY_ON_ITS_CONTROL) {
  const first = control === "Snooze" ? "Later today" : control === "Status" ? "Todo" : "P2";
  // SLOW(>=2s): an Inbox, an issue page or a Board drawn, and its menu opened and closed.
  // @firefox: the keys a text box takes, which the shared single pick's closed trigger reads as it
  // opens its list (playwright.config.ts names the clause).
  test(`${check}: in ${where}, with the focus on the ${control} control, ${key} opens its menu on ${first} with every choice shown and nothing typed, and Escape writes nothing @firefox`, async ({
    page,
  }) => {
    const step = (text: string) =>
      `${check}: in ${where}, ${key} on the focused ${control}: ${text}`;
    const seed = await seedAsksAtP2(1);
    const askId = itemAt(seed.asks, 0);
    const issueKey = seed.keyOf.get(askId) ?? "";
    const snoozes = [snoozeWrites(page, askId)];
    const writes = issueWrites(page, issueKey);
    const menu = openList(page);
    let trigger: Locator;
    if (where === "the reading pane" || where === "the reading view") {
      const inbox = await openInboxAt(page, where, 1);
      await goTo(inbox, askId);
      const controls = controlsOf(page, inbox, askId);
      trigger =
        control === "Snooze"
          ? controls.getByRole("button", { name: /^Snooze / })
          : priorityTrigger(controls, issueKey);
    } else if (where === "the issue page") {
      await page.setViewportSize(WIDE);
      await openIssue(page, issueKey, "Ledger rollout");
      trigger =
        control === "Status"
          ? statusTrigger(page)
          : priorityTrigger(page.getByTestId("issue-header"), issueKey);
    } else {
      await page.setViewportSize(WIDE);
      await page.goto("/projects/CORE");
      await page.getByRole("button", { exact: true, name: "Board" }).click();
      trigger = priorityTrigger(
        page.getByRole("article", { name: `${issueKey} Ledger rollout` }),
        issueKey
      );
    }
    const count =
      control === "Snooze"
        ? SNOOZE_CHOICES.length
        : control === "Status"
          ? OPEN_STATUSES.length
          : PRIORITY_CHOICES.length;
    await trigger.focus();
    await expect(trigger, step("the control has the focus")).toBeFocused();
    await page.keyboard.press(key);
    await expect(menu, step(`${key} opens the menu`)).toBeVisible();
    await expect(menu.getByRole("combobox"), step(`${key} types nothing in the box`)).toHaveValue(
      ""
    );
    await expect(menu.getByRole("option"), step(`${key} shows every choice`)).toHaveCount(count);
    await expectHighlight(menu, first, step(`${key} highlights ${first}`));
    await page.keyboard.press("Escape");
    await expect(menu, step("Escape closes the menu")).toHaveCount(0);
    await expect(trigger, step("Escape gives the focus back to the control")).toBeFocused();
    // A write the keys sent reaches the page's request events within this second and a half.
    await page.waitForTimeout(1_500);
    expect(requestsSent([...snoozes, writes]), step("nothing is written")).toBe(0);
  });
}

for (const control of ["priority", "Status"] as const) {
  const key = control === "priority" ? "p" : "s";
  const held = control === "priority" ? "P2" : "Todo";
  const check = control === "priority" ? "k-priority-menu" : "k-status-menu";
  // SLOW(>=2s): an Inbox or an issue page drawn, and its menu opened twice.
  // @firefox: the keys a text box takes, which the shared single pick's closed trigger reads as it
  // opens its list (playwright.config.ts names the clause).
  test(`${check}: ${control === "priority" ? "beside the reading pane" : "on today's issue page"}, ${key} then Enter writes nothing with the focus on the ${control} control, as it writes nothing from ${control === "priority" ? "the row" : "the page"} @firefox`, async ({
    page,
  }) => {
    const step = (text: string) => `${check}: ${key} then Enter: ${text}`;
    const seed = await seedAsksAtP2(1);
    const askId = itemAt(seed.asks, 0);
    const issueKey = seed.keyOf.get(askId) ?? "";
    const writes = issueWrites(page, issueKey);
    const menu = openList(page);
    let trigger: Locator;
    if (control === "priority") {
      const inbox = await openInboxAt(page, "the reading pane", 1);
      await goTo(inbox, askId);
      trigger = priorityTrigger(paneHead(page), issueKey);
    } else {
      await page.setViewportSize(WIDE);
      await openIssue(page, issueKey, "Ledger rollout");
      trigger = statusTrigger(page);
    }
    await page.keyboard.press(key);
    await expect(
      menu,
      step(`${key} from ${control === "priority" ? "the row" : "the page"} opens the menu`)
    ).toBeVisible();
    await page.keyboard.press("Enter");
    await expect(menu, step("Enter closes the menu")).toHaveCount(0);
    await trigger.focus();
    await page.keyboard.press(key);
    await expect(menu, step(`${key} on the focused control opens the menu`)).toBeVisible();
    await page.keyboard.press("Enter");
    await expect(menu, step("Enter on the focused control's menu closes it")).toHaveCount(0);
    // A write Enter sent reaches the page's request events within this second and a half.
    await page.waitForTimeout(1_500);
    expect(writesSent(writes), step(`Enter on the menu's own value, ${held}, writes nothing`)).toBe(
      0
    );
    await expect(trigger, step(`the ${control} still shows ${held}`)).toContainText(held);
  });
}

// The tester's red rows of round 16 (R4): a key that names another choice control, pressed while
// the focus is on a closed list's trigger, opens that other control's menu. At 9d3e0777 the
// focused trigger types the key into its own list's box (`MultiSelect.tsx`, `openFromKey`).
/** Opens `other`'s list with a click and closes it with Escape, which gives the focus back to
 *  `other`'s trigger (D5: "a click on the trigger returns to the trigger"); then presses `key`,
 *  which names `trigger`'s control (R4: "A key that names a choice control opens that control's
 *  menu"): `trigger`'s list opens, and no list of `other`'s. */
async function keyFromOtherTrigger(
  page: Page,
  other: Locator,
  key: string,
  trigger: Locator,
  step: (text: string) => string
): Promise<void> {
  await other.click();
  await expect(listOf(other), step("a click opens the other control's list")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(other, step("Escape gives the focus back to the other control")).toBeFocused();
  await page.keyboard.press(key);
  await expect(listOf(trigger), step(`${key} opens its own control's menu`)).toBeVisible();
  await expect(listOf(other), step(`${key} opens no list of the other control's`)).toHaveCount(0);
  await page.keyboard.press("Escape");
}

// SLOW(>=2s): an issue page drawn, and two lists opened by a click and by a key.
test("k-status-menu: on today's issue page s opens the status menu while the focus is on the header's priority, and p the priority menu while it is on the Status", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger rollout";
  const issue = await createIssue({ project: "CORE", title });
  await patchIssue(issue.key, { priority: 2, status: "todo" });
  await page.setViewportSize(WIDE);
  const writes = issueWrites(page, issue.key);
  await openIssue(page, issue.key, title);
  const status = statusTrigger(page);
  const priority = priorityTrigger(page.getByTestId("issue-header"), issue.key);
  const step = (text: string) => `k-status-menu: from the other control's trigger, ${text}`;
  await keyFromOtherTrigger(page, priority, "s", status, step);
  await keyFromOtherTrigger(page, status, "p", priority, step);
  expect(requestsSent([writes]), step("nothing is written")).toBe(0);
});

// SLOW(>=2s): the new issue page drawn, and two lists opened by a click and by a key.
test("k-status-menu: on the new issue page s opens the status list while the focus is on the properties row's priority, and p the priority list while it is on the status", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await patchIssue(issue.key, { priority: 2, status: "todo" });
  await withIssuePage(page.context(), "alice");
  await page.setViewportSize(WIDE);
  const writes = issueWrites(page, issue.key);
  await gotoIssue(page, `/issues/${issue.key}`);
  const row = page.getByTestId("issue-state-actions");
  const status = row.getByRole("button", { name: /^Status/ });
  const priority = priorityTrigger(row, issue.key);
  const step = (text: string) =>
    `k-status-menu: on the new issue page, from the other control's trigger, ${text}`;
  await keyFromOtherTrigger(page, priority, "s", status, step);
  await keyFromOtherTrigger(page, status, "p", priority, step);
  expect(requestsSent([writes]), step("nothing is written")).toBe(0);
});

// SLOW(>=2s): a reading view drawn, and two lists opened by a click and by a key.
test("k-priority-menu: in the reading view at 1024x768 p opens the priority menu while the focus is on its head's Snooze, and b the Snooze menu while it is on its head's priority", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await patchIssue(issue.key, { priority: 2 });
  const ask = await createAsk(issue.key, { question: "Ship the ledger rollout?" }, asker);
  const watched = [snoozeWrites(page, ask.id), issueWrites(page, issue.key)];
  const inbox = await openInboxAt(page, "the reading view", 1);
  await goTo(inbox, ask.id);
  const head = controlsOf(page, inbox, ask.id);
  const snooze = head.getByRole("button", { name: /^Snooze / });
  const priority = priorityTrigger(head, issue.key);
  const step = (text: string) =>
    `k-priority-menu: the reading view, from the other control's trigger, ${text}`;
  await keyFromOtherTrigger(page, snooze, "p", priority, step);
  await keyFromOtherTrigger(page, priority, "b", snooze, step);
  expect(requestsSent(watched), step("nothing is written")).toBe(0);
});

// The tester's red rows of round 16 (R5, plan D5): a press on a control far down the page while
// the Snooze menu is open closes the menu, writes nothing, and acts in that one press, as the same
// press does with no menu open. At 9d3e0777 the focus the menu gives back to the first card
// scrolls the cards between the press's mousedown and its mouseup, so the press lands on another
// element and the control does nothing. At 1024x768 the Inbox lists short rows (#5223), and the
// list's one Snooze menu is the bar's, which `b` opens over the rows `x` marked and which gives
// the focus back to the row it was pressed on: the first row, far up a list of sixteen that
// scrolls; the far control is the Later band's toggle, or the last row's own checkbox.
for (const target of ["the Later band's toggle", "the last row's checkbox"] as const) {
  // SLOW(>=2s): seventeen asks seeded, a control pressed with no menu open and with one open.
  // @firefox: where the engine scrolls a list when a focus is given back between a press's
  // mousedown and its mouseup (playwright.config.ts names the clause).
  test(`k-snooze-menu: in the short-row list at 1024x768, with the Snooze menu b opened on the first row, which x marked, a press on ${target}, far down the page, closes the menu, writes nothing and acts in that one press @firefox`, async ({
    page,
  }) => {
    const step = (text: string) =>
      `k-snooze-menu: the short-row list, a press on ${target}: ${text}`;
    await createProject({ key: "CORE", name: "Core" });
    const asks: string[] = [];
    for (let n = 1; n <= 17; n += 1) {
      const issue = await createIssue({ force: true, project: "CORE", title: `Rollout ${n}` });
      asks.push((await createAsk(issue.key, { question: `Ship rollout ${n}?` }, asker)).id);
    }
    const later = itemAt(asks, 16);
    await snoozeAsk(later, new Date(Date.now() + 7 * 24 * 60 * 60 * 1000).toISOString());
    const inbox = await openInboxAt(page, "the short-row list", 16);
    const first = itemAt(inbox.order, 0);
    const last = itemAt(inbox.order, 15);
    const snoozes = asks.flatMap((id) => [snoozeWrites(page, id), unsnoozeWrites(page, id)]);
    const menu = openList(page);
    const band = target === "the Later band's toggle";
    const control = band ? laterBand(inbox, 1) : rowOf(inbox, last).getByRole("checkbox");
    const acted = band
      ? rowOf(inbox, later)
      : rowOf(inbox, last).locator('input[type="checkbox"]:checked');
    await control.click();
    await expect(acted, step("with no menu open, one press acts")).toHaveCount(1);
    await control.click();
    await expect(acted, step("the control is put back")).toHaveCount(0);
    await goTo(inbox, first);
    await page.keyboard.press("x");
    await page.keyboard.press("b");
    await expect(menu, step("b opens the Snooze menu over the first row")).toBeVisible();
    await control.click();
    await expect(openTrigger(page), step("the press closes the Snooze menu")).toHaveCount(0);
    await expect(acted, step("the one press acts")).toHaveCount(1);
    // A write the press sent reaches the page's request events within this second and a half.
    await page.waitForTimeout(1_500);
    expect(requestsSent(snoozes), step("nothing is written")).toBe(0);
  });
}

// The tester's red rows of round 16 (R5, R8): a key held down past the system's repeat delay acts
// once. A held digit in the Snooze menu writes one snooze, and its repeats tick no option of the
// question R8 puts in hand after it; a held `b` or `p` opens its menu with nothing typed in its
// box. At 9d3e0777 the repeats land after the press: the digit's on the next question's row,
// where the Inbox's `1` ticks its first option, and the opener's in the menu's box.

/** Holds `key` down as a hand does: one press, a system's repeat delay of half a second, then
 *  `repeats` repeats 33 ms apart, which Playwright sends with `repeat: true`. */
async function holdKey(page: Page, key: string, repeats: number): Promise<void> {
  await page.keyboard.down(key);
  // Half a second is a common system's delay before a held key starts to repeat.
  await page.waitForTimeout(500);
  for (let repeat = 0; repeat < repeats; repeat += 1) {
    await page.keyboard.down(key);
    // Thirty-three ms apart is a common system's rate of a held key's repeats.
    await page.waitForTimeout(33);
  }
  await page.keyboard.up(key);
}

// SLOW(>=2s): three questions in the reading pane, a digit held for a second.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-snooze-menu: beside the reading pane a 1 held down in the Snooze menu writes one snooze, answers nothing, and its repeats tick no option of the question R8 puts in hand @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-snooze-menu: a held 1: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Ledger", "Mirror", "Lease"]) {
    const issue = await createIssue({ project: "CORE", title: `${title} rollout` });
    await createAsk(
      issue.key,
      {
        options: [{ label: "Ship" }, { label: "Hold" }, { label: "Drop" }],
        question: `Ship the ${title.toLowerCase()} rollout?`,
      },
      asker
    );
  }
  const inbox = await openInboxAt(page, "the reading pane", 3);
  const first = itemAt(inbox.order, 0);
  const second = itemAt(inbox.order, 1);
  const firstSnoozes = snoozeWrites(page, first);
  const snoozes = inbox.order.flatMap((id) => [snoozeWrites(page, id), unsnoozeWrites(page, id)]);
  const answers = inbox.order.map((id) => answerWrites(page, id));
  const menu = openList(page);
  await goTo(inbox, first);
  await page.keyboard.press("b");
  await expect(menu, step("b opens the Snooze menu")).toBeVisible();
  await holdKey(page, "1", 15);
  await expect
    .poll(() => writesSent(firstSnoozes), { message: step("the held 1 snoozes the ask") })
    .toBe(1);
  await expectInHand(inbox, second, step("the snooze hands on to the next question"));
  // A write the repeats sent reaches the page's request events within this second and a half.
  await page.waitForTimeout(1_500);
  expect(requestsSent(snoozes), step("the held 1 sends one snooze in all")).toBe(1);
  expect(requestsSent(answers), step("the held 1 answers no question")).toBe(0);
  await expect(
    inboxPane(page).getByRole("radio", { checked: true }),
    step("the held 1's repeats tick no option of the question now in hand")
  ).toHaveCount(0);
});

// SLOW(>=2s): an ask in the reading pane, two keys each held for a second.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-snooze-menu: beside the reading pane a b held down opens the Snooze menu on Later today with every choice shown and nothing typed, and a p held down the priority menu on P2 @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-snooze-menu: a held opener: ${text}`;
  const seed = await seedAsksAtP2(1);
  const askId = itemAt(seed.asks, 0);
  const inbox = await openInboxAt(page, "the reading pane", 1);
  const writes = [snoozeWrites(page, askId), issueWrites(page, seed.keyOf.get(askId) ?? "")];
  const menu = openList(page);
  await goTo(inbox, askId);
  for (const [key, first, count] of [
    ["b", "Later today", SNOOZE_CHOICES.length],
    ["p", "P2", PRIORITY_CHOICES.length],
  ] as const) {
    await holdKey(page, key, 10);
    await expect(menu, step(`a held ${key} opens its menu`)).toBeVisible();
    await expect(menu.getByRole("combobox"), step(`a held ${key} types nothing`)).toHaveValue("");
    await expect(menu.getByRole("option"), step(`a held ${key} shows every choice`)).toHaveCount(
      count
    );
    await expectHighlight(menu, first, step(`a held ${key} highlights ${first}`));
    await page.keyboard.press("Escape");
    await expect(menu, step(`Escape closes the menu a held ${key} opened`)).toHaveCount(0);
    await expect(rowOf(inbox, askId), step("Escape gives the focus back to the row")).toBeFocused();
  }
  // A write the keys sent reaches the page's request events within this second and a half.
  await page.waitForTimeout(1_500);
  expect(requestsSent(writes), step("nothing is written")).toBe(0);
});

// The tester's red rows of round 16 (R8): a snooze of the only item the list shows, with the
// Later band folded, leaves the focus in the Inbox. At 9d3e0777 the focus goes to the page's body.
/** k-next-item's snooze of the only item the list shows while the Later band is folded: no row is
 *  left in sight and no empty message is drawn, so the hand-off has neither a row nor the message
 *  to give the focus to. R8: "If no item is left, the list's empty message has the focus, and the
 *  Inbox's keys still work. This is true with the Later band folded or open". The focus stays in
 *  the Inbox, never on the page's body. */
async function nextItemOnlyShownSteps(page: Page, place: InboxPlace): Promise<void> {
  const step = (text: string) => `k-next-item: ${place}: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Clock audit" });
  const ask = (await createAsk(issue.key, { question: "Ship the clock audit?" }, asker)).id;
  const inbox = await openInboxAt(page, place, 1);
  await putOn(inbox, ask);
  await page.keyboard.press("b");
  await expect(openList(page), step("b opens the only ask's Snooze menu")).toBeVisible();
  await page.keyboard.press("1");
  await expect(laterBand(inbox, 1), step("the snooze folds the only ask into Later")).toBeVisible();
  await expect(rowOf(inbox, ask), step("no row is left in sight")).toHaveCount(0);
  await expect
    .poll(
      () =>
        inbox.scope.evaluate(
          (scope) =>
            document.activeElement !== document.body && scope.contains(document.activeElement)
        ),
      {
        message: step(
          "a snooze of the only item shown, the Later band folded, leaves the focus in the Inbox and not on the page's body"
        ),
      }
    )
    .toBe(true);
}

for (const place of [
  "the reading pane",
  "the short-row list",
  "the reading view",
  "the drawer",
] as const) {
  // SLOW(>=2s): an ask snoozed into the folded Later band, and the focus read after it.
  test(`k-next-item: ${placeWords(place)} a snooze of the only item shown, with the Later band folded, leaves the focus in the Inbox and not on the page's body`, async ({
    page,
  }) => {
    await nextItemOnlyShownSteps(page, place);
  });
}

// The tester's red rows of round 16 (R8, and 9feff4e4's no scroll while the rows still stand): in
// the short-row list at 1024x768 (in card mode before #5223) and in the drawer a snooze of two
// selected rows scrolls the list nowhere. At 9d3e0777 the list jumps by about 200 to 500 px and
// the next row ends at the top edge.
/** Where `row` stands in the box that scrolls the list it is in - the list column, the drawer, or
 *  the document where no element around it scrolls: its top against the box's visible top, and
 *  the box's scrollTop. */
function listPlace(row: Locator): Promise<{ scrollTop: number; top: number }> {
  return row.evaluate((node) => {
    let box: Element | null = node.parentElement;
    while (box !== null) {
      const style = getComputedStyle(box);
      if (/(auto|scroll)/.test(style.overflowY) && box.scrollHeight > box.clientHeight) break;
      box = box.parentElement;
    }
    const scroller = box ?? document.scrollingElement ?? document.documentElement;
    const top = box === null ? 0 : Math.max(box.getBoundingClientRect().top, 0);
    return {
      scrollTop: Math.round(scroller.scrollTop),
      top: Math.round(node.getBoundingClientRect().top - top),
    };
  });
}

/** k-next-item's snooze of two selected rows in the middle of a list that scrolls: the row after
 *  the last of them takes the focus (R8: "When the reader acts on selected items, the item after
 *  the last of them is selected"), and the list does not scroll - that row moves up into the place
 *  where the first selected row stood, as beside the reading pane (9feff4e4: "after an act on rows
 *  the next row takes the focus with no scroll while the rows still stand, and is brought into
 *  view once they have gone"). */
async function nextItemBulkNoScrollSteps(page: Page, place: InboxPlace): Promise<void> {
  const step = (text: string) => `k-next-item: ${place}: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  for (let n = 18; n >= 1; n -= 1) {
    const issue = await createIssue({ force: true, project: "CORE", title: `Rollout ${n}` });
    await createAsk(issue.key, { question: `Ship rollout ${n}?` }, asker);
  }
  const inbox = await openInboxAt(page, place, 18);
  const [first, second, after] = inbox.order.slice(9, 12);
  if (first === undefined || second === undefined || after === undefined) {
    throw new Error("the Inbox lists fewer than the eighteen asks seeded");
  }
  for (const id of [first, second]) {
    await goTo(inbox, id);
    await page.keyboard.press("x");
  }
  await expect(
    inbox.scope.getByRole("toolbar", { name: "Selected rows" }),
    step("x selects two rows")
  ).toContainText("2 selected");
  // The first selected row is scrolled to the middle of what the list shows, and the pointer is
  // taken off every row.
  await rowOf(inbox, first).evaluate((node) => {
    let box: Element | null = node.parentElement;
    while (box !== null) {
      const style = getComputedStyle(box);
      if (/(auto|scroll)/.test(style.overflowY) && box.scrollHeight > box.clientHeight) break;
      box = box.parentElement;
    }
    const r = node.getBoundingClientRect();
    if (box === null) window.scrollBy(0, r.top + r.height / 2 - innerHeight / 2);
    else {
      const b = box.getBoundingClientRect();
      box.scrollTop += r.top + r.height / 2 - (b.top + b.height / 2);
    }
  });
  await page.mouse.move(2, 2);
  await settledScrollTop(page, 5);
  const before = await listPlace(rowOf(inbox, first));
  await page.keyboard.press("b");
  await expect(openTrigger(page), step("b opens the bar's Snooze")).toHaveAccessibleName(
    "Snooze selected asks"
  );
  await page.keyboard.press("1");
  await expectInHand(
    inbox,
    after,
    step("a snooze of two selected rows hands on to the row after them")
  );
  await settledScrollTop(page, 10);
  const now = await listPlace(rowOf(inbox, after));
  expect(
    Math.abs(now.scrollTop - before.scrollTop),
    step(
      `a snooze of two selected rows scrolls the list nowhere (scrollTop ${before.scrollTop} before, ${now.scrollTop} after)`
    )
  ).toBeLessThanOrEqual(4);
  expect(
    Math.abs(now.top - before.top),
    step(
      `the row after them stands where the first selected row stood (${before.top} px, now ${now.top} px)`
    )
  ).toBeLessThanOrEqual(4);
}

// The architect's rulings Q1(b) and Q2(a): the bar's acts after `x` stay in the short-row list.
// SLOW(>=2s): eighteen asks seeded, two selected and snoozed, and the list read as it settles.
test("k-next-item: in the short-row list at 1024x768 a snooze of two selected rows in a list that scrolls hands on to the row after them with no scroll", async ({
  page,
}) => {
  await nextItemBulkNoScrollSteps(page, "the short-row list");
});

// SLOW(>=2s): eighteen asks seeded, two selected and snoozed, and the list read as it settles.
test("k-next-item: in the drawer a snooze of two selected rows in a list that scrolls hands on to the row after them with no scroll", async ({
  page,
}) => {
  await nextItemBulkNoScrollSteps(page, "the drawer");
});

// The tester's red row of round 16 (R15): on a phone a tap at the centre of each choice of the
// status menu lands on that choice. At 9d3e0777 the text box and the foot line are drawn over the
// first and the last choice.
/** k-phone for the status: a tap on today's header Status opens its menu, and a tap at the
 *  centre of each choice lands on that choice, not on the menu's text box or its foot line drawn
 *  over it (R15: "a tap on a choice writes it"; `MultiSelect`'s `ListMenu`: "The panel is tall
 *  enough for a menu's choices, each 44 px high, under the box and over the foot line"). */
// SLOW(>=2s): an issue page on a phone, and its status menu opened by a tap.
test("k-phone: a tap opens the status menu of today's issue page, and a tap at the centre of each of its choices lands on that choice @phone @phone-only", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger rollout";
  const issue = await createIssue({ project: "CORE", title });
  await patchIssue(issue.key, { status: "todo" });
  await openIssue(page, issue.key, title);
  const status = statusTrigger(page);
  const menu = listOf(status);
  await expect(status, "k-phone: the header draws its Status").toBeVisible();
  await status.tap();
  await expect(menu, "k-phone: a tap on the Status opens the status menu").toBeVisible();
  await expect(menu.getByRole("option"), "k-phone: the menu holds the open statuses").toHaveCount(
    OPEN_STATUSES.length
  );
  for (const label of OPEN_STATUSES) {
    const choice = menu.getByRole("option", { exact: true, name: label });
    const lands = await choice.evaluate((node) => {
      const r = node.getBoundingClientRect();
      const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return hit !== null && node.contains(hit);
    });
    expect(lands, `k-phone: a tap at the centre of ${label} lands on ${label}`).toBe(true);
  }
});

/** On a phone, opens the Inbox drawer over CORE's project page with a tap on the compact top bar's
 *  Peek (`peekTrigger`), and answers it once it draws its `count` cards. */
async function tapOpenDrawer(page: Page, count: number): Promise<Locator> {
  await page.goto("/projects/CORE");
  await peekTrigger(page).tap();
  const peek = drawer(page);
  await expect(peek.locator("[data-inbox-row]"), "k-phone: the drawer draws its cards").toHaveCount(
    count
  );
  return peek;
}

// The tester's red row of round 16 (R15): on a phone, right after a tap on P1 moves a card, a tap
// on another card's Snooze opens its menu. At 9d3e0777 that tap scrolls the page and opens nothing.
// Since cybermonklabs/legion#5223 the Inbox page lists short rows at a phone's width, and the
// Peek drawer is where cards with their own priority and Snooze stand: its list is banded by
// priority, as the page's was, and scrolls in the drawer, the box the list's viewport anchor holds
// (`InboxDrawer`'s `scroller`).
/** k-phone after a pick that moves a card: six asks at P2 on a phone, in the drawer, a tap on P1
 *  in a low card's priority menu, which moves that card up into the P1 band, then a tap on another
 *  card's Snooze (R15: "a tap on a control of R4 opens its menu"): its menu opens, and the tapped
 *  Snooze stays where the finger tapped it. */
// SLOW(>=2s): six asks on a phone, a priority picked by taps, and a second card's Snooze tapped.
test("k-phone: right after a tap on P1 moves a card, a tap on another card's Snooze opens its menu and stays where the finger tapped it @phone @phone-only", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issueOf = new Map<string, string>();
  for (const title of ["Ledger", "Mirror", "Lease", "Replay", "Quota", "Clock"]) {
    const issue = await createIssue({ force: true, project: "CORE", title: `${title} rollout` });
    await patchIssue(issue.key, { priority: 2 });
    const ask = await createAsk(
      issue.key,
      { question: `Ship the ${title.toLowerCase()} rollout?` },
      asker
    );
    issueOf.set(ask.id, issue.key);
  }
  const peek = await tapOpenDrawer(page, 6);
  const card = (id: string) => peek.locator(`[data-inbox-row="${id}"]`);
  const order = await peek
    .locator("[data-inbox-row]")
    .evaluateAll((nodes) => nodes.map((node) => node.getAttribute("data-inbox-row") ?? ""));
  const [moved, other] = order.slice(3, 5);
  const movedKey = issueOf.get(moved ?? "");
  if (moved === undefined || other === undefined || movedKey === undefined) {
    throw new Error("the Inbox lists other asks than the six seeded");
  }
  /** Scrolls the drawer so `control`'s top stands at `at` of the viewport's height: the drawer
   *  takes the whole of a phone's screen, from its top. */
  const scrollTo = async (control: Locator, at: number) => {
    await control.evaluate((node, fraction) => {
      const box = node.closest('[data-testid="inbox-drawer"]');
      if (box !== null) box.scrollTop += node.getBoundingClientRect().top - innerHeight * fraction;
    }, at);
    await settledScrollTop(peek, 5);
  };
  const writes = issueWrites(page, movedKey);
  const priority = priorityTrigger(card(moved), movedKey);
  await scrollTo(priority, 0.8);
  await priority.tap();
  await expect(
    listOf(priority),
    "k-phone: a tap on the low card's priority opens its menu"
  ).toBeVisible();
  await listOf(priority).getByRole("option", { exact: true, name: "P1" }).tap();
  await expect
    .poll(() => writesSent(writes), { message: "k-phone: a tap on P1 sends one write" })
    .toBe(1);
  // The server answers the pick's write and the card moves into the P1 band within this second
  // and a half.
  await page.waitForTimeout(1500);

  const snooze = card(other).getByRole("button", { name: /^Snooze / });
  await scrollTo(snooze, 0.85);
  // The scroll above read this Snooze's own box to place it, so it is drawn and has a box.
  const box = await snooze.boundingBox();
  if (box === null) throw new Error("k-phone: the other card's Snooze has no box");
  const before = box.y;
  await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
  await expect(
    listOf(snooze),
    "k-phone: right after the pick, a tap on another card's Snooze opens its menu"
  ).toBeVisible();
  const after = (await snooze.boundingBox())?.y;
  expect(
    Math.abs((after ?? Number.NaN) - before),
    `k-phone: the tapped Snooze stays where the finger tapped it (y ${before} before, ${after} after)`
  ).toBeLessThanOrEqual(4);
});

/** The same taps in the Inbox page's reading view on a phone (cybermonklabs/legion#5223): a tap on
 *  P1 in the head's priority menu moves the row into the P1 band of the list the view hides, and
 *  right after it a tap on the head's Snooze opens its menu, and the Snooze stays where the finger
 *  tapped it. */
// SLOW(>=2s): six asks on a phone, a row's reading view opened, a priority picked by taps, and its
// Snooze tapped.
test("k-phone: in the Inbox's reading view, right after a tap on P1 in its head's priority menu, a tap on the head's Snooze opens its menu and stays where the finger tapped it @phone @phone-only", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issueOf = new Map<string, string>();
  for (const title of ["Ledger", "Mirror", "Lease", "Replay", "Quota", "Clock"]) {
    const issue = await createIssue({ force: true, project: "CORE", title: `${title} rollout` });
    await patchIssue(issue.key, { priority: 2 });
    const ask = await createAsk(
      issue.key,
      { question: `Ship the ${title.toLowerCase()} rollout?` },
      asker
    );
    issueOf.set(ask.id, issue.key);
  }
  await page.goto("/");
  const rows = page.locator("[data-inbox-row]");
  await expect(rows).toHaveCount(6);
  const order = await rows.evaluateAll((nodes) =>
    nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
  );
  const moved = order[3];
  const movedKey = issueOf.get(moved ?? "");
  if (moved === undefined || movedKey === undefined) {
    throw new Error("the Inbox lists other asks than the six seeded");
  }
  await inboxRow(page, moved).locator("time").first().tap();
  await expect(
    readingView(page),
    "k-phone: a tap on the fourth row opens its reading view"
  ).toHaveAttribute("data-inbox-reading-row", moved);
  const writes = issueWrites(page, movedKey);
  const priority = priorityTrigger(paneHead(page), movedKey);
  await priority.tap();
  await expect(
    listOf(priority),
    "k-phone: a tap on the head's priority opens its menu"
  ).toBeVisible();
  await listOf(priority).getByRole("option", { exact: true, name: "P1" }).tap();
  await expect
    .poll(() => writesSent(writes), { message: "k-phone: a tap on P1 sends one write" })
    .toBe(1);
  // The server answers the pick's write and the row moves into the P1 band within this second
  // and a half.
  await page.waitForTimeout(1500);

  const snooze = paneHead(page).getByRole("button", { name: /^Snooze / });
  await expect(snooze, "k-phone: the reading view's head draws its Snooze").toBeVisible();
  // The wait above found the Snooze drawn, so it has a box.
  const box = await snooze.boundingBox();
  if (box === null) throw new Error("k-phone: the head's Snooze has no box");
  const before = box.y;
  await page.touchscreen.tap(box.x + box.width / 2, box.y + box.height / 2);
  await expect(
    listOf(snooze),
    "k-phone: right after the pick, a tap on the head's Snooze opens its menu"
  ).toBeVisible();
  const after = (await snooze.boundingBox())?.y;
  expect(
    Math.abs((after ?? Number.NaN) - before),
    `k-phone: the tapped Snooze stays where the finger tapped it (y ${before} before, ${after} after)`
  ).toBeLessThanOrEqual(4);
});

// W9 of round 17 (R15): on a phone, right after a tap on P1 moves a card, a tap on the page beside
// the cards lets that card go and moves the page nowhere. At 99f90b1e the tap takes the focus out
// of the card to the page's `main`, the Inbox lets the card go inside that move, and the view
// follows the card up into the P1 band, 1,711 px. Since cybermonklabs/legion#5223 the cards stand
// in the Peek drawer alone at a phone's width (as for the row above), so the row taps in the
// drawer: its list is banded by priority, scrolls in the drawer, which the list's viewport anchor
// holds (`InboxDrawer`'s `scroller`), and a tap on the drawer beside the cards takes the focus out
// of the card to the drawer itself, as a tap on the page took it to `main`.
/** k-phone after a pick that moves a card, then a tap on no card: six asks at P2 on a phone, in
 *  the drawer. Twice, a tap on P1 in the fourth card's priority menu, which moves that card into
 *  the P1 band once the focus leaves it, the level the finger taps scrolled to the middle of the
 *  screen, then a tap on the drawer's margin: the first time level with the gap between that card
 *  and the card after it, which stays where the finger saw it, the second time level with the
 *  card's middle, where the card before it stays. Each time the card goes to the P1 band and the
 *  tap opens nothing. The taps are in the margin and not in the 12 px gap itself, since Chromium
 *  moves a tap that close to a card onto that card's link. */
// SLOW(>=2s): six asks on a phone, two priorities picked by taps, and the page beside them tapped.
test("k-phone: right after a tap on P1 moves a card, a tap on the page beside the cards lets the card go and leaves the cards beside the tap where they stood @phone @phone-only", async ({
  page,
}) => {
  await createProject({ key: "CORE", name: "Core" });
  const issueOf = new Map<string, string>();
  for (const title of ["Ledger", "Mirror", "Lease", "Replay", "Quota", "Clock"]) {
    const issue = await createIssue({ force: true, project: "CORE", title: `${title} rollout` });
    await patchIssue(issue.key, { priority: 2 });
    const ask = await createAsk(
      issue.key,
      { question: `Ship the ${title.toLowerCase()} rollout?` },
      asker
    );
    issueOf.set(ask.id, issue.key);
  }
  const peek = await tapOpenDrawer(page, 6);
  const rows = peek.locator("[data-inbox-row]");
  const cardOf = (id: string) => peek.locator(`[data-inbox-row="${id}"]`);
  // Every read of the Inbox, with when it went out and when it was answered (`logReads`).
  const inboxReads = logReads(page, inboxRead);
  for (const level of ["the gap after it", "its middle"] as const) {
    const step = (text: string) => `k-phone: a tap on the page level with ${level}: ${text}`;
    // The fourth card down is in the P2 band both times: the first pick puts its card above all.
    const order = await rows.evaluateAll((nodes) =>
      nodes.map((node) => node.getAttribute("data-inbox-row") ?? "")
    );
    const [before, moved, after] = order.slice(2, 5);
    const movedKey = issueOf.get(moved ?? "");
    if (
      before === undefined ||
      moved === undefined ||
      after === undefined ||
      movedKey === undefined
    ) {
      throw new Error("the Inbox lists other asks than the six seeded");
    }
    const card = cardOf(moved);
    const writes = issueWrites(page, movedKey);
    // The moment the runner hears the server's answer to the pick's write, noted in that event,
    // before the page can act on the answer: each Inbox read the landing makes goes out after it.
    let answeredAt = Number.POSITIVE_INFINITY;
    const answered = page.waitForResponse((response) => {
      const ours =
        response.request().method() === "PATCH" &&
        new URL(response.url()).pathname === `/api/v1/issues/${encodeURIComponent(movedKey)}`;
      if (ours) answeredAt = Date.now();
      return ours;
    });
    const priority = priorityTrigger(card, movedKey);
    await priority.evaluate((node) => {
      const box = node.closest('[data-testid="inbox-drawer"]');
      if (box !== null) box.scrollTop += node.getBoundingClientRect().top - innerHeight * 0.8;
    });
    await settledScrollTop(peek, 5);
    await priority.tap();
    await expect(
      listOf(priority),
      step("a tap on the card's priority opens its menu")
    ).toBeVisible();
    await listOf(priority).getByRole("option", { exact: true, name: "P1" }).tap();
    await expect
      .poll(() => writesSent(writes), { message: step("a tap on P1 sends one write") })
      .toBe(1);
    // The band is read once the pick's write has landed, the Inbox has been read again after it,
    // and the page has drawn those answers. On a slow server the row waits for them, so the read
    // cannot pass on a band the write has not reached yet.
    await answered;
    await expect
      .poll(() => readAnsweredSince(inboxReads, answeredAt), {
        message: step("the Inbox is read again once the pick's write has landed"),
      })
      .toBe(true);
    await nextFrames(page);
    await expect(
      card,
      step("the picked card stays in the P2 band while the focus is in it")
    ).toHaveAttribute("data-inbox-group", "P2");

    /** How far down the window the finger taps: level with the gap after the card, or with the
     *  card's middle. */
    const tapLevel = (cardAt: { y: number; height: number }, afterAt: { y: number }) =>
      level === "the gap after it"
        ? (cardAt.y + cardAt.height + afterAt.y) / 2
        : cardAt.y + cardAt.height / 2;
    // The level the finger taps goes to the middle of the screen, which the drawer covers from its
    // top. An ask's card here, with its answer box and that box's hint, stands taller than the
    // window (682 px of 664 at 85f13ed5), so with the card's own middle there the gap after it
    // would be below the window.
    const [cardAt, afterAt] = await Promise.all([card.boundingBox(), cardOf(after).boundingBox()]);
    if (cardAt === null || afterAt === null) throw new Error("k-phone: a card has no box");
    await peek.evaluate(
      (box, at) => {
        box.scrollTop += at - innerHeight / 2;
      },
      tapLevel(cardAt, afterAt)
    );
    await settledScrollTop(peek, 5);
    const kept = level === "the gap after it" ? after : before;
    const [cardBox, afterBox, keptBox] = await Promise.all([
      card.boundingBox(),
      cardOf(after).boundingBox(),
      cardOf(kept).boundingBox(),
    ]);
    if (cardBox === null || afterBox === null || keptBox === null) {
      throw new Error("k-phone: a card has no box");
    }
    // The drawer's margin beside the cards, its own 16 px padding from the screen's edge, where
    // the drawer stands on a phone: 3 px in from that edge is 13 px from the cards, the farthest a
    // finger can stand from them inside the drawer (the page's margin left 21 px).
    const x = 3;
    const y = tapLevel(cardBox, afterBox);
    const onNothing = await page.evaluate(
      (at) => {
        const under = document.elementFromPoint(at.x, at.y);
        return under !== null && under.closest("[data-inbox-row], a[href], button") === null;
      },
      { x, y }
    );
    expect(
      onNothing,
      step("the finger taps the page beside the cards, on no card and no control")
    ).toBe(true);
    const address = page.url();
    await page.touchscreen.tap(x, y);
    await expect(card, step("the tap lets the picked card go into the P1 band")).toHaveAttribute(
      "data-inbox-group",
      "P1"
    );
    await settledScrollTop(peek, 10);
    const now = (await cardOf(kept).boundingBox())?.y;
    expect(
      Math.abs((now ?? Number.NaN) - keptBox.y),
      step(
        `the card ${level === "the gap after it" ? "after the gap" : "before the picked card"} stays where it stood (y ${keptBox.y} before, ${now} after)`
      )
    ).toBeLessThanOrEqual(4);
    expect(page.url(), step("the tap opens nothing")).toBe(address);
  }
});

// The tester's red rows of round 17 (R7, R17, R18, and 62b47002's "a held key acts once but for
// the steps and scrolls that repeat"): a key whose press puts the caret in a text field, held down
// past the system's repeat delay, types nothing there. At 1e21af01 the registry reads a repeat
// against its own target (`handleKeyDown`'s `event.repeat` branch, `features/shell/keymap.ts`):
// once the press has moved the focus into the field, no binding of the field's takes the key, so
// the repeat goes on to the field and is typed - "4444444444" in the answer box, ten new lines,
// "ffffffffff" in the search box, "rrrrrrrrrr" in the reply box, "RRRRRRRRRR" in the reason.

/** Three questions of CORE, each on an issue of its own with three options, waiting on alice. */
async function seedHeldQuestions(): Promise<void> {
  await createProject({ key: "CORE", name: "Core" });
  for (const title of ["Ledger", "Mirror", "Lease"]) {
    const issue = await createIssue({ force: true, project: "CORE", title: `${title} rollout` });
    await createAsk(
      issue.key,
      {
        options: [{ label: "Ship" }, { label: "Hold" }, { label: "Drop" }],
        question: `Ship the ${title.toLowerCase()} rollout?`,
      },
      asker
    );
  }
}

const HELD_INTO_A_FIELD = [
  {
    check: "k-other-key",
    field: "answer",
    key: "4",
    what: "a 4 held down on a question of three options picks Other with the caret in its answer box",
  },
  {
    check: "k-answer-box",
    field: "answer",
    key: "Enter",
    what: "an Enter held down on a question's row puts the caret in its answer box",
  },
  {
    check: "k-default-keys",
    field: "search",
    key: "f",
    what: "an f held down puts the caret in the Inbox's search box",
  },
] as const;

for (const { check, field, key, what } of HELD_INTO_A_FIELD) {
  // SLOW(>=2s): three questions in the reading pane, a key held for a third of a second.
  // @firefox: the repeats the engine sends for a held key, and where they land once the press has
  // moved the focus (playwright.config.ts names the clause).
  test(`${check}: beside the reading pane ${what}, and its repeats type nothing there and answer nothing @firefox`, async ({
    page,
  }) => {
    const step = (text: string) => `${check}: a held ${key}: ${text}`;
    await seedHeldQuestions();
    const inbox = await openInboxAt(page, "the reading pane", 3);
    const first = itemAt(inbox.order, 0);
    const answers = inbox.order.map((id) => answerWrites(page, id));
    await goTo(inbox, first);
    const card = inboxPane(page).getByTestId(`ask-${first}`);
    const box =
      field === "answer"
        ? card.locator("[data-ask-answer]")
        : inbox.scope.getByRole("searchbox", { name: "Search asks" });
    await expect(box, step("the field is drawn")).toBeVisible();
    await holdKey(page, key, 10);
    await expect(box, step("the press puts the caret in the field")).toBeFocused();
    if (key === "4") {
      await expect(card.getByRole("radio", { name: "Other" }), step("4 picks Other")).toBeChecked();
    }
    // A write the repeats sent reaches the page's request events within this second and a half.
    await page.waitForTimeout(1_500);
    expect(requestsSent(answers), step("nothing is answered")).toBe(0);
    await expect(box, step("the repeats type nothing in the field")).toHaveValue("");
  });
}

// SLOW(>=2s): a reply thread opened in the reading pane, and r held for a third of a second.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-answer-box: beside the reading pane an r held down on a reply thread's row puts the caret in its reply box, and its repeats type nothing there @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-answer-box: a held r: ${text}`;
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ force: true, project: "CORE", title: "Replay budget" });
  const thread = await seedCommentThread(
    issue.key,
    "How far back does a replay go?",
    "One hour, then it gives up."
  );
  await page.setViewportSize(WIDE);
  await page.goto("/");
  await selectRow(page, replyRowId(thread));
  const reply = inboxPane(page).locator("[data-inbox-thread-body] textarea").first();
  await expect(reply, step("the thread's reply box is drawn")).toBeVisible();
  await holdKey(page, "r", 10);
  await expect(reply, step("r puts the caret in the reply box")).toBeFocused();
  await expect(reply, step("the repeats type nothing in the reply box")).toHaveValue("");
});

// SLOW(>=2s): an approval request's reading view at 1024x768, and Shift+R held for a third of a
// second.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-approval-keys: in the reading view at 1024x768 a Shift+R held down opens Request changes with the caret in its reason, and its repeats type nothing there @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-approval-keys: a held Shift+R: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const approval = await seedApprovalAtVersionTwo("CORE", "Clock audit");
  const inbox = await openInboxAt(page, "the reading view", 1);
  await goTo(inbox, approval.askId);
  // The head's Request changes takes a press once the document's read returns, so the press
  // moves the focus into the reason and the repeats come after it.
  await expect(
    paneHead(page).getByRole("button", { disabled: false, exact: true, name: "Request changes" }),
    step("the request's head draws Request changes, and it takes a press")
  ).toBeVisible();
  await page.keyboard.down("Shift");
  await holdKey(page, "R", 10);
  await page.keyboard.up("Shift");
  const dialog = page.getByRole("dialog", { name: "Request changes" });
  await expect(dialog, step("Shift+R opens Request changes")).toBeVisible();
  const reason = dialog.getByLabel("Reason");
  await expect(reason, step("the caret is in the reason")).toBeFocused();
  await expect(reason, step("the repeats type nothing in the reason")).toHaveValue("");
});

for (const [key, what] of [
  ["c", "the new issue's title"],
  ["/", "the command bar's box"],
] as const) {
  // SLOW(>=2s): two questions in the reading pane, a key held for a third of a second.
  // @firefox: the repeats the engine sends for a held key, and where they land once the press has
  // moved the focus (playwright.config.ts names the clause).
  test(`k-default-keys: beside the reading pane a ${key} held down puts the caret in ${what}, and its repeats type nothing there @firefox`, async ({
    page,
  }) => {
    const step = (text: string) => `k-default-keys: a held ${key}: ${text}`;
    await seedHeldQuestions();
    const inbox = await openInboxAt(page, "the reading pane", 3);
    await goTo(inbox, itemAt(inbox.order, 0));
    await holdKey(page, key, 10);
    await nextFrames(page);
    const typed = await page.evaluate(() => {
      const active = document.activeElement;
      return active instanceof HTMLInputElement || active instanceof HTMLTextAreaElement
        ? active.value
        : null;
    });
    expect(typed, step(`the press puts the caret in ${what}`)).not.toBeNull();
    expect(typed, step(`the repeats type nothing in ${what}`)).toBe("");
  });
}

// The tester's red row of round 17 (R24: "Escape closes the list that is open, then the panel or
// the menu", and 62b47002's "a held key acts once but for the steps and scrolls that repeat"): an
// Escape held down in the Filter panel's open list closes that list alone. At 1e21af01 the panel's
// own Escape (`AnchoredPanel`'s capture-phase `dismissOnEscape`) reads no `repeat`, so the first
// repeat after the list has closed, on the field inside the panel, closes the panel too.
// SLOW(>=2s): the Inbox's Filter panel with a list open, and Escape held for a third of a second.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-filter-keys: on the Inbox page an Escape held down in the Filter panel's open list closes the list alone, and the panel stays open with the focus on the field @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-filter-keys: a held Escape in the list: ${text}`;
  await seedFilterKeys();
  await openInboxAt(page, "the reading pane", 4);
  await focusPage(page, step("the focus starts on the page"));
  const field = page.getByRole("button", { name: "Filter by priority" });
  const panel = await shiftFOpens(page, field, "on the Inbox");
  await page.keyboard.press("Enter");
  const list = listOf(field);
  await expect(list, step("Enter opens the field's list")).toBeVisible();
  await holdKey(page, "Escape", 10);
  await expect(list, step("the Escape closes the list")).toBeHidden();
  await expect(panel, step("the repeats leave the panel open")).toBeVisible();
  await expect(field, step("the focus is on the field")).toBeFocused();
});

// The tester's red row of round 17 (62b47002's "a held key acts once but for the steps and scrolls
// that repeat", and keyboard.md's "Every other key acts once for each press"): Enter held down on
// the issue header's focused Pin issue writes the pin once. At 1e21af01 a repeat that no binding
// takes goes on to the browser, which presses the focused button again on each repeat: one held
// Enter wrote 16 pins and unpins in Chromium, Firefox and WebKit.
// SLOW(>=2s): an issue page drawn and Enter held on its Pin issue for a third of a second.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-default-keys: on today's issue page an Enter held down on the header's focused Pin issue writes the pin once and leaves the issue pinned @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-default-keys: a held Enter on Pin issue: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  const pins = watchWrites(page, "PUT", `/api/v1/me/issues/${issue.key}/state`);
  await page.setViewportSize(WIDE);
  await openIssue(page, issue.key, "Ledger rollout");
  const header = page.getByTestId("issue-header");
  await header.getByRole("button", { name: "Pin issue" }).focus();
  await holdKey(page, "Enter", 10);
  // A write the repeats sent reaches the page's request events within this second and a half.
  await page.waitForTimeout(1_500);
  expect(writesSent(pins), step("a held Enter writes the pin once")).toBe(1);
  await expect(
    header.getByRole("button", { name: "Unpin issue" }),
    step("the issue is pinned once the held Enter is let go")
  ).toBeVisible();
});

// The tester's red row of round 17 (the class of 62b47002's "a held key acts once", which the
// architect ruled on 2026-11-25T04:12Z): Enter held down on the issue header's Close issue opens
// the parent-status question once, and its repeats press nothing. At 1e21af01 each repeat that no
// binding takes presses the focused button: the first presses the question's Keep as it is, which
// closes it, the next Close issue again, which asks again, and so on, so the question ends open or
// closed by the count of repeats (3 repeats: closed; measured in infra-2160-t-close-ffrbs).
/** A parent in work with two open children, one in todo and one in the backlog: the parent's key
 *  and title. */
async function seedParentWithOpenChildren(): Promise<{ key: string; title: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const title = "Ledger parent";
  const parent = await createIssue({ project: "CORE", title });
  const working = await createIssue({ parent: parent.key, project: "CORE", title: "Ledger todo" });
  const waiting = await createIssue({ parent: parent.key, project: "CORE", title: "Ledger later" });
  await patchIssue(parent.key, { status: "in_progress" });
  await patchIssue(working.key, { status: "todo" });
  await patchIssue(waiting.key, { status: "backlog" });
  return { key: parent.key, title };
}

// SLOW(>=2s): a parent with two children, and Enter held on its Close issue for three repeats.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-default-keys: on today's issue page an Enter held down on Close issue opens the parent-status question once, and its repeats press nothing @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-default-keys: a held Enter on Close issue: ${text}`;
  const seed = await seedParentWithOpenChildren();
  const posts = watchWrites(page, "POST", `/api/v1/issues/${seed.key}/status-change`);
  await page.setViewportSize(WIDE);
  await openIssue(page, seed.key, seed.title);
  await page.getByRole("button", { exact: true, name: "Close issue" }).focus();
  // Every click on a button while the key is held, by the button's words.
  await page.evaluate(() => {
    const pressed: string[] = [];
    Object.assign(window, { heldPresses: pressed });
    document.addEventListener(
      "click",
      (event) => {
        const button = event.target instanceof Element ? event.target.closest("button") : null;
        if (button !== null) pressed.push((button.textContent ?? "").trim());
      },
      true
    );
  });
  await holdKey(page, "Enter", 3);
  const dialog = page.getByRole("dialog", { name: `Move ${seed.key} to Done?` });
  // The question's dry runs and its first draw are over within this second and a half.
  await page.waitForTimeout(1_500);
  const presses = await page.evaluate(() =>
    "heldPresses" in window ? (window.heldPresses as string[]) : []
  );
  expect(
    presses.length,
    step(`the held Enter presses one button once (${JSON.stringify(presses)})`)
  ).toBe(1);
  await expect(dialog, step("the question stays open")).toBeVisible();
  expect(
    posts.filter((write) => (write.input as { dry_run?: boolean } | null)?.dry_run !== true).length,
    step("the held Enter writes no status")
  ).toBe(0);
});

// The tester's red row of round 17 (a79f53b0's "a pick gives the focus back with no scroll", which
// the architect ruled on 2026-11-25T04:12Z extends to the list's Use row): on a phone-width page,
// Use "owner/repo" in the issue header's repository list gives the focus back to its trigger with
// no scroll of the page. At 1e21af01 the create's hand-back (`MultiSelect.tsx`, `create`'s
// `handBack()`) takes the engine's own scroll, 22 px for a trigger half out of view.
// SLOW(>=2s): an issue page on a phone-width window, and its repository list worked by the keyboard.
test('k-choice-writes-on-enter: on a phone-width page, Use "owner/repo" in the header\'s repository list gives the focus back to its trigger with no scroll of the page', async ({
  page,
}) => {
  const step = (text: string) => `k-choice-writes-on-enter: the repository's Use row: ${text}`;
  const { issueKey, title } = await seedChoices();
  await page.setViewportSize({ height: 664, width: 390 });
  await openIssue(page, issueKey, title);
  const trigger = page.getByTestId("issue-header").getByRole("button", { name: /^Repository/ });
  await expect(trigger, step("the header draws its repository control")).toBeVisible();
  await trigger.click();
  const list = listOf(trigger);
  await expect(list, step("a click opens the repository list")).toBeVisible();
  await list.getByRole("combobox").fill("acme/newone");
  await expect(
    list.getByRole("option", { name: 'Use "acme/newone"' }),
    step("the list offers Use")
  ).toBeVisible();
  // The page is scrolled so the trigger stands half out of view at the top.
  await trigger.evaluate((node) => {
    const box = node.getBoundingClientRect();
    window.scrollBy(0, box.top + box.height / 2);
  });
  const before = await settledScrollTop(page, 5);
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Enter");
  await expect(list, step("Enter on Use closes the list")).toBeHidden();
  await expect(trigger, step("the focus goes back to the trigger")).toBeFocused();
  const after = await settledScrollTop(page, 5);
  expect(
    Math.abs(after - before),
    step(`the Use pick scrolls the page nowhere (scrollY ${before} before, ${after} after)`)
  ).toBeLessThanOrEqual(4);
});

// The tester's red rows of round 18 (cc76c363's held-key rule). Each sends a press and then a
// repeat of it to one element, as a browser sends a held key, and reads the repeat's own
// `defaultPrevented`: `Keymap.captureKeyDown` (keymap.ts:779-784) sets it on each repeat it stops,
// and no binding or control of the page names these keys, so nothing else sets it. The plain `a`
// and the ArrowDown are the rows' controls: the head leaves their repeats alone, so each row fails
// only at the step that names its defect. Playwright sends neither AltGr nor a browser's own
// accelerator, so the rows send the events the browser sends.
/** A key as `heldPressOf` sends it: its fields and the modifiers held with it. */
interface HeldKeyInit {
  readonly altKey?: boolean;
  readonly code: string;
  readonly ctrlKey?: boolean;
  readonly key: string;
  readonly modifierAltGraph?: boolean;
}

/** The page's own read of a press and then a repeat of each key of `inits` at the focused element
 *  (or the body): whether the press and the repeat were default-prevented. */
function heldPressOf(
  page: Page,
  inits: readonly HeldKeyInit[]
): Promise<{ press: boolean; repeat: boolean }[]> {
  return page.evaluate(
    (rows: HeldKeyInit[]) => {
      const target = document.activeElement ?? document.body;
      return rows.map((row) => {
        const init: KeyboardEventInit = {
          altKey: row.altKey === true,
          bubbles: true,
          cancelable: true,
          code: row.code,
          composed: true,
          ctrlKey: row.ctrlKey === true,
          key: row.key,
        };
        // Chromium reads `modifierAltGraph` into `getModifierState("AltGraph")`; the DOM's typing
        // of `KeyboardEventInit` does not list it.
        Object.assign(init, { modifierAltGraph: row.modifierAltGraph === true });
        const press = new KeyboardEvent("keydown", { ...init, repeat: false });
        target.dispatchEvent(press);
        const repeat = new KeyboardEvent("keydown", { ...init, repeat: true });
        target.dispatchEvent(repeat);
        target.dispatchEvent(new KeyboardEvent("keyup", init));
        return { press: press.defaultPrevented, repeat: repeat.defaultPrevented };
      });
    },
    [...inits]
  );
}

// keyboard.md: "In a text field that had the focus before you pressed the key, typing repeats as
// usual: a held character". Chromium and Edge on Windows report AltGr as Ctrl+Alt (with
// `getModifierState("AltGraph")`), so the `@` of AltGr+Q on a German keyboard comes with
// `ctrlKey` and `altKey` set. At a97f63a2 `writesText` (components/key-targets.ts:88) is false for
// every `ctrlKey`, so the repeats of a held `@`, `€`, `{`, `[` or `\\` are stopped.
test("k-default-keys: in the Inbox's search box the repeat of a character typed with AltGr, which Windows reports as Ctrl+Alt, is left to the field as a plain character's is", async ({
  page,
}) => {
  const step = (text: string) => `k-default-keys: a held AltGr character: ${text}`;
  await seedHeldQuestions();
  const inbox = await openInboxAt(page, "the reading pane", 3);
  const search = inbox.scope.getByRole("searchbox", { name: "Search asks" });
  await search.focus();
  const [plain, altGr] = await heldPressOf(page, [
    { code: "KeyA", key: "a" },
    { altKey: true, code: "KeyQ", ctrlKey: true, key: "@", modifierAltGraph: true },
  ]);
  expect(plain?.repeat, step("the repeat of a plain a is left to the field")).toBe(false);
  expect(altGr?.press, step("the press of AltGr+Q is the field's")).toBe(false);
  expect(
    altGr?.repeat,
    step("the repeat of AltGr+Q is left to the field, as the repeat of a plain a is")
  ).toBe(false);
});

// keymap.ts: the rule keeps from the page "each repeat of a held key that would act". Ctrl+- and
// Ctrl+= are the browser's zoom, which a page's keydown can cancel, and no control or binding of
// the page names them. At a97f63a2 each of their repeats is cancelled (keymap.ts:783), so a held
// Ctrl+- zooms out one step only.
test("k-default-keys: on the Inbox page the repeats of Ctrl+- and Ctrl+=, which no control of the page names, are left to the browser's zoom", async ({
  page,
}) => {
  const step = (text: string) => `k-default-keys: a held Ctrl+- or Ctrl+=: ${text}`;
  await seedHeldQuestions();
  await openInboxAt(page, "the reading pane", 3);
  await page.evaluate(() => {
    if (document.activeElement instanceof HTMLElement) document.activeElement.blur();
  });
  const [control, out, inward] = await heldPressOf(page, [
    { code: "ArrowDown", key: "ArrowDown" },
    { code: "Minus", ctrlKey: true, key: "-" },
    { code: "Equal", ctrlKey: true, key: "=" },
  ]);
  expect(
    control?.repeat,
    step("the repeat of an ArrowDown, which scrolls, is left to the browser")
  ).toBe(false);
  expect(out?.press, step("the press of Ctrl+- is the browser's")).toBe(false);
  expect(out?.repeat, step("the repeat of Ctrl+- (zoom out) is the browser's too")).toBe(false);
  expect(inward?.repeat, step("the repeat of Ctrl+= (zoom in) is the browser's too")).toBe(false);
});

// The tester's red rows of round 19 (d25fab46's held-key rule, keyboard.md: "A key that does
// something on the page acts once for each press as well, `Enter` and `Escape` included ... a
// held `Escape` closes one list or panel"). At c6dc939c a repeat goes on to the page where the
// press was not default-prevented: an Escape that a handler takes with no preventDefault acts
// again on each repeat, on the element the press moved the focus to (IssueTitleRow.tsx's edit,
// then Margin.tsx's Details) or on the same field (MentionComposer.tsx's handleEscape). Measured
// in Chromium, Firefox and WebKit, twice each, in builds Jobs infra-2160-t-ph19-a-fj8ml and
// infra-2160-t-ph19-b-8k7zf.

/** Every write (a non-GET request to /api) the page sends from now on, as "METHOD /path". */
function writesFrom(page: Page): string[] {
  const sent: string[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() !== "GET" && url.pathname.startsWith("/api/")) {
      sent.push(`${request.method()} ${url.pathname}`);
    }
  });
  return sent;
}

// SLOW(>=2s): a new issue page drawn with Details open, and Escape held in its title editor.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-default-keys: on the new issue page with Details open an Escape held down in the title editor ends the edit alone, and Details stays open @firefox", async ({
  page,
}) => {
  const step = (text: string) =>
    `k-default-keys: a held Escape in the new page's title editor: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await withIssuePage(page.context(), "alice");
  await page.setViewportSize(WIDE);
  await gotoIssue(page, `/issues/${issue.key}`);
  const details = marginShell(page);
  await focusPage(page, step("the focus starts on the page"));
  await page.keyboard.press("Shift+M");
  await expect(details, step("Shift+M opens Details")).toBeVisible();
  const title = page.locator("[data-title-control]");
  const editor = page.getByRole("textbox", { name: "Issue title" });
  // The row's control: one press of Escape in the editor ends the edit and leaves Details open.
  await title.focus();
  await page.keyboard.press("Enter");
  await expect(editor, step("Enter on the title opens its editor")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(editor, step("one Escape ends the edit")).toHaveCount(0);
  await expect(title, step("one Escape puts the focus back on the title")).toBeFocused();
  await expect(details, step("one Escape press in the editor leaves Details open")).toBeVisible();
  await page.keyboard.press("Enter");
  await expect(editor, step("Enter on the title opens its editor again")).toBeFocused();
  await page.keyboard.type(" draft");
  const writes = writesFrom(page);
  await holdKey(page, "Escape", 10);
  // A repeat that acted would have closed Details within this second and a half.
  await page.waitForTimeout(1_500);
  await expect(editor, step("the held Escape ends the edit")).toHaveCount(0);
  await expect(title, step("the title is put back")).toHaveText("Ledger rollout");
  await expect(details, step("the held Escape leaves Details open")).toBeVisible();
  expect(writes, step("nothing is written")).toEqual([]);
});

// SLOW(>=2s): the Conversation tab drawn, its mention list opened twice, and Escape held.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-comment-focus: on today's issue page an Escape held down in the comment composer's open mention list closes the list alone, asks no Discard draft? and keeps the draft @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-comment-focus: a held Escape in the mention list: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${issue.key}/conversation`);
  const form = page.getByRole("form", { name: "Comment composer" });
  const field = form.getByLabel("Comment");
  await expect(field, step("the Conversation tab draws its comment box")).toBeVisible();
  // main's #3125 draws one reference picker for `[[`, `@` and `#` (`e2e/ref-picker.ts`).
  const suggestions = refPicker(page);
  const discardAsk = page.getByText("Discard draft?", { exact: true });
  // The row's control: one press of Escape closes the list and asks nothing.
  await field.fill("Ship it now @");
  await expect(suggestions, step("@ opens the mention list")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(suggestions, step("one Escape closes the mention list")).toHaveCount(0);
  await expect(discardAsk, step("one Escape asks no Discard draft?")).toHaveCount(0);
  await field.fill("Ship it later @");
  await expect(suggestions, step("@ opens the mention list again")).toBeVisible();
  const writes = writesFrom(page);
  await holdKey(page, "Escape", 10);
  // A repeat that acted would have asked Discard draft? within this second and a half.
  await page.waitForTimeout(1_500);
  await expect(suggestions, step("the held Escape closes the mention list")).toHaveCount(0);
  await expect(discardAsk, step("the held Escape asks no Discard draft?")).toHaveCount(0);
  await expect(field, step("the draft is kept")).toHaveValue("Ship it later @");
  await expect(field, step("the caret stays in the field")).toBeFocused();
  expect(writes, step("nothing is written")).toEqual([]);
});

// SLOW(>=2s): a spec drawn, a comment box opened over a typed draft, and Escape held in it.
// @firefox: the repeats the engine sends for a held key, and where they land once the press has
// moved the focus (playwright.config.ts names the clause).
test("k-comment-focus: on today's issue page an Escape held down in the margin's comment box over a typed draft asks Discard draft? once, and the box and its draft stay @firefox", async ({
  page,
}) => {
  const step = (text: string) =>
    `k-comment-focus: a held Escape in the margin's comment box: ${text}`;
  const seed = await seedCommentFocus();
  await page.setViewportSize(WIDE);
  await openSpec(page, seed.key, FOCUS_PARAGRAPHS[2][0]);
  await selectEditorText(page, THIRD_WORD);
  await barAction(page, "Comment");
  await expectCaretInBox(page, THIRD_WORD, step("the bar's Comment puts the caret in the box"));
  const body = "Why does the lease return at noon?";
  await page.keyboard.type(body);
  const box = commentBox(page);
  const discardAsk = page.getByText("Discard draft?", { exact: true });
  // The row's control: one press of Escape asks Discard draft? and keeps the box.
  await page.keyboard.press("Escape");
  await expect(discardAsk, step("one Escape asks Discard draft?")).toBeVisible();
  await expect(box, step("one Escape keeps the box")).toHaveCount(1);
  await box.getByRole("button", { exact: true, name: "Keep" }).click();
  await expect(discardAsk, step("Keep takes the question away")).toHaveCount(0);
  await expect(
    box.getByLabel("Comment"),
    step("Keep puts the caret back in the box")
  ).toBeFocused();
  const writes = writesFrom(page);
  await holdKey(page, "Escape", 10);
  // A repeat that acted would have closed the box within this second and a half.
  await page.waitForTimeout(1_500);
  await expect(box, step("the held Escape leaves the comment box open")).toHaveCount(1);
  await expect(discardAsk, step("the held Escape asks Discard draft? once")).toBeVisible();
  await expect(box.getByLabel("Comment"), step("the draft is kept")).toHaveValue(body);
  expect(writes, step("nothing is written")).toEqual([]);
});

/** An issue of CORE whose Conversation holds one comment of alice's, the reader's own. */
async function plantOwnComment(): Promise<{ commentId: string; key: string }> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger clock" });
  const comment = await createComment(issue.key, { body: "Alice asks why the clock is shared." });
  return { commentId: comment.id, key: issue.key };
}

// The tester's red row of round 21 (an inline editor a key or a button ends leaves the focus on the
// page body: the Discard of `Discard draft?` runs MentionComposer.tsx:907-915's `discard`, whose
// `setConfirmingDiscard(false)` (:913) takes the question away with the Discard button that holds
// the focus (:1084-1092), and whose `onClose()` (:914) is ConversationTab.tsx:1524's
// `onClose={() => setReplyTo(null)}`, which moves no focus, where the question's Keep gives it back
// to the field that held it (`focusIntoView(field)`, MentionComposer.tsx:1100-1101); the
// architect's ruling of 2026-11-26T05:17Z). A typed draft in the Conversation's docked composer,
// Escape, which asks Discard draft?, and a click on its Discard; the caret must be back in the
// composer's Comment box, which held it before the question. At c3b536cc the focus is on the body
// in every sample: Chromium four times and Firefox twice, at 1440x900 and at 390x664.
// SLOW(>=2s): one issue seeded, one Conversation tab open, and a typed draft in its composer
// discarded, with no write.
// @firefox: the engine's focus after the element that held it is removed (playwright.config.ts names the clause).
test("composer hand-back: on today's issue page Discard over a typed draft in the Conversation's comment composer puts the caret back in its Comment box @firefox", async ({
  page,
}) => {
  const step = (text: string) =>
    `composer hand-back: Discard in the Conversation's comment composer: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${issue.key}/conversation`);
  // The issue holds no comment, so the docked composer is the page's one comment form.
  const form = page.getByRole("form", { name: "Comment composer" });
  const field = form.getByRole("textbox", { exact: true, name: "Comment" });
  await expect(field, step("the Conversation tab draws its comment composer.")).toBeVisible();
  await field.click();
  await page.keyboard.type("Half a thought about the clock");
  await expect(field, step("the typed draft is in the Comment box.")).toHaveValue(
    "Half a thought about the clock"
  );
  await expect(field, step("the Comment box holds the caret.")).toBeFocused();
  const question = form.getByRole("alert").filter({ hasText: "Discard draft?" });
  await page.keyboard.press("Escape");
  await expect(question, step("Escape over the typed draft asks Discard draft?.")).toBeVisible();
  await question.getByRole("button", { exact: true, name: "Discard" }).click();
  await expect(question, step("Discard takes the question away.")).toHaveCount(0);
  await expect(field, step("Discard empties the Comment box.")).toHaveValue("");
  await expect(
    field,
    step(
      "after Discard the caret is back in the composer's Comment box, which held it before the question, not on the page body."
    )
  ).toBeFocused();
});

// The tester's red row of round 21 (an inline editor a key or a button ends leaves the focus on the
// page body: a second Escape over `Discard draft?` runs MentionComposer.tsx:585-591's `closeOrAsk`,
// which with the question up (`confirmingDiscard`, :588) calls `onClose()` (:590); a comment's edit
// composer's `onClose` is ThreadCard.tsx:462's `() => setEditingId(undefined)`, on the Conversation
// tab ConversationTab.tsx:1664's `setEditing(item.root.id, id)` (:1177-1185), which unmounts the
// editor that holds the focus, and the Edit button that opened it (ThreadCard.tsx:476-482) has no
// ref, and the file no `.focus(` that gives the focus back to it; the architect's ruling of
// 2026-11-26T05:17Z). On the Conversation tab, Expand thread, Edit on alice's own comment, a change
// to its text, Escape, which asks Discard draft?, and Escape again end the edit unsaved; the focus
// must be back on the comment's Edit button. At c3b536cc it is on the body in every sample:
// Chromium four times and Firefox twice, at 1440x900 and at 390x664.
// SLOW(>=2s): one comment seeded, one Conversation tab open, its thread opened, and the comment's
// edit composer opened and ended unsaved, with no write.
// @firefox: the engine's focus after the element that held it is removed (playwright.config.ts names the clause).
test("composer hand-back: on today's issue page a second Escape over Discard draft? in a Conversation comment's edit composer gives the focus back to the comment's Edit button @firefox", async ({
  page,
}) => {
  const step = (text: string) =>
    `composer hand-back: Escape twice in a Conversation comment's edit composer: ${text}`;
  const seed = await plantOwnComment();
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${seed.key}/conversation`);
  const turn = page.locator(`[data-turn="comment:${seed.commentId}"]`);
  const edit = turn.getByRole("button", { exact: true, name: "Edit" });
  const field = turn.getByRole("textbox", { name: "Edit comment" });
  await turn.getByRole("button", { exact: true, name: "Expand thread" }).click();
  await expect(edit, step("the open thread offers Edit on alice's own comment.")).toBeVisible();
  await edit.click();
  await expect(field, step("Edit opens the comment's editor.")).toBeVisible();
  await field.click();
  await field.fill("Alice asks why the clock is shared, and since when.");
  await expect(field, step("the changed text is in the editor.")).toHaveValue(
    "Alice asks why the clock is shared, and since when."
  );
  await expect(field, step("the editor holds the caret.")).toBeFocused();
  const question = turn.getByRole("alert").filter({ hasText: "Discard draft?" });
  await page.keyboard.press("Escape");
  await expect(question, step("Escape over the changed text asks Discard draft?.")).toBeVisible();
  await expect(field, step("the editor keeps the caret while it asks.")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(field, step("a second Escape ends the edit and takes the editor away.")).toHaveCount(
    0
  );
  await expect(
    edit,
    step(
      "after the second Escape the focus is back on the comment's Edit button, which opened the editor, not on the page body."
    )
  ).toBeFocused();
});

// The tester's red row of round 21 (an inline editor a key or a button ends leaves the focus on the
// page body: Escape in a thread's reply box runs MentionComposer.tsx:687-689's `cancelReply`
// (:573-577), whose `onCancelReply` is ThreadCard.tsx:512's `onReplyCancel ?? onToggle`; the
// Conversation tab hands a card `onReplyCancel` (turn-keys.ts:267-276's `giveBackFromThread`) only
// for a thread `r` opened (ConversationTab.tsx:1665-1669), so for a thread Expand thread opened it
// is the turn's `toggleThread` (ConversationTab.tsx:856-865), which closes the thread, takes the
// reply box that holds the focus away with it (ThreadCard.tsx:501-502) and gives the focus no
// place; the architect's ruling of 2026-11-26T05:17Z). Expand thread on alice's comment, a click in
// the thread's Reply box, and Escape close the thread; the focus must be on the turn's Expand
// thread toggle, which opened it. At c3b536cc it is on the body in every sample at 1440x900:
// Chromium four times and Firefox twice.
// SLOW(>=2s): one comment seeded, one Conversation tab open, and its thread opened and closed by
// Escape in its reply box, with no write.
// @firefox: the engine's focus after the element that held it is removed (playwright.config.ts names the clause).
test("composer hand-back: on today's issue page Escape in the reply box of a thread Expand thread opened closes the thread with the focus on its Expand thread toggle @firefox", async ({
  page,
}) => {
  const step = (text: string) =>
    `composer hand-back: Escape in the reply box of a thread Expand thread opened: ${text}`;
  const seed = await plantOwnComment();
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${seed.key}/conversation`);
  const turn = page.locator(`[data-turn="comment:${seed.commentId}"]`);
  const toggle = turn.getByRole("button", { name: /^(?:Expand|Collapse) thread$/ });
  const reply = turn.getByRole("textbox", { exact: true, name: "Reply" });
  await expect(toggle, step("the turn draws its thread toggle, closed.")).toHaveText(
    "Expand thread"
  );
  await toggle.click();
  await expect(toggle, step("Expand thread opens the thread.")).toHaveAttribute(
    "aria-expanded",
    "true"
  );
  await reply.click();
  await expect(reply, step("a click puts the caret in the thread's Reply box.")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(reply, step("Escape ends the reply and takes its box away.")).toHaveCount(0);
  await expect(toggle, step("Escape closes the thread.")).toHaveAttribute("aria-expanded", "false");
  await expect(
    toggle,
    step(
      "after Escape the focus is on the turn's Expand thread toggle, which opened the thread, not on the page body."
    )
  ).toBeFocused();
});

// The tester's red row of round 21 (an inline editor a key or a button ends leaves the focus on the
// page body: Cancel reply in the docked composer (MentionComposer.tsx:1033-1041, drawn only while
// the composer holds a reply, :1012) runs ConversationTab.tsx:1520-1523's `onCancelReply`, whose
// `setReplyTo(null)` takes that button away with the focus it holds, and whose
// `giveBackFromDockedReply()` (turn-keys.ts:327-331) focuses only a turn `r` noted
// (`dockedReplyFrom`, set at turn-keys.ts:258), which a click's `beginReply`
// (ConversationTab.tsx:1264-1265) clears (`beginDockedReply`, turn-keys.ts:324-326); the
// architect's ruling of 2026-11-26T05:17Z). The Reply button on the turn of alice's comment starts
// a reply in the docked composer, and a click on its Cancel reply ends it; the focus must be back
// on that Reply button. At c3b536cc it is on the body in every sample: Chromium four times and
// Firefox twice, at 1440x900 and at 390x664.
// SLOW(>=2s): one comment seeded, one Conversation tab open, and a reply started in its docked
// composer and cancelled, with no write.
// @firefox: the engine's focus after the element that held it is removed (playwright.config.ts names the clause).
test("composer hand-back: on today's issue page Cancel reply in the Conversation's docked composer gives the focus back to the Reply button that started the reply @firefox", async ({
  page,
}) => {
  const step = (text: string) =>
    `composer hand-back: Cancel reply in the Conversation's docked composer: ${text}`;
  const seed = await plantOwnComment();
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${seed.key}/conversation`);
  const turn = page.locator(`[data-turn="comment:${seed.commentId}"]`);
  const replyButton = turn.getByRole("button", { exact: true, name: "Reply" });
  // The docked composer is the `Comment composer` form whose box is named `Comment` exactly: a
  // thread's reply box is `Reply`, and the thread Reply opens draws one too, hidden while the
  // docked composer answers its comment.
  const form = page
    .getByRole("form", { name: "Comment composer" })
    .filter({ has: page.getByRole("textbox", { exact: true, name: "Comment" }) });
  const field = form.getByRole("textbox", { exact: true, name: "Comment" });
  const cancel = form.getByRole("button", { exact: true, name: "Cancel reply" });
  await expect(field, step("the Conversation tab draws its docked composer.")).toBeVisible();
  await expect(cancel, step("the docked composer holds no reply yet.")).toHaveCount(0);
  await replyButton.click();
  await expect(
    cancel,
    step("Reply starts a reply in the docked composer, with its Cancel reply.")
  ).toBeVisible();
  await expect(field, step("Reply puts the caret in the docked composer's box.")).toBeFocused();
  await cancel.click();
  await expect(cancel, step("Cancel reply ends the reply and takes itself away.")).toHaveCount(0);
  await expect(
    replyButton,
    step(
      "after Cancel reply the focus is back on the Reply button that started the reply, not on the page body."
    )
  ).toBeFocused();
});

// The tester's red row of round 21 (an inline editor a key or a button ends leaves the focus on the
// page body: a second Escape over `Discard draft?` runs MentionComposer.tsx:585-591's `closeOrAsk`,
// which with the question up (`confirmingDiscard`, :588) calls `onClose()` (:590), ThreadCard.tsx
// :462's `() => setEditingId(undefined)`, in the spec's margin useMarginSheet.ts:733's
// `setEditingCommentId`, which unmounts the editor that holds the focus, and the card's Edit button
// (ThreadCard.tsx:476-482) has no ref, and the file no `.focus(` that gives the focus back to it;
// the architect's ruling of 2026-11-26T05:17Z). In the spec's margin, alice's comment on "lease"
// opened, its Edit, a change to its text, Escape, which asks Discard draft?, and Escape again end
// the edit unsaved; the focus must be back on the card's Edit button. At c3b536cc it is on the
// body in every sample: Chromium four times and Firefox twice, at 1440x900 and at 390x664.
// SLOW(>=2s): one comment on the spec seeded, the spec opened and connected, the comment's card
// opened, and its edit composer opened and ended unsaved, with no write.
// @firefox: the engine's focus after the element that held it is removed (playwright.config.ts names the clause).
test("composer hand-back: on today's issue page a second Escape over Discard draft? in the spec margin's edit composer gives the focus back to the card's Edit button @firefox", async ({
  page,
}) => {
  const step = (text: string) =>
    `composer hand-back: Escape twice in the spec margin's edit composer: ${text}`;
  const seed = await seedCommentFocus();
  const comment = await createComment(seed.key, {
    anchor: { artifact: "spec", quote: THIRD_WORD },
    body: "Why does the lease return at noon?",
  });
  await page.setViewportSize(WIDE);
  await openSpec(page, seed.key, FOCUS_PARAGRAPHS[2][0]);
  const card = page.getByTestId(`margin-comment-${comment.id}`);
  const edit = card.getByRole("button", { exact: true, name: "Edit" });
  const field = card.getByRole("textbox", { name: "Edit comment" });
  await card.getByRole("button", { expanded: false }).first().click();
  await expect(card, step("a click on the card's preview opens its thread.")).toHaveAttribute(
    "aria-expanded",
    "true"
  );
  await expect(edit, step("the open card offers Edit on alice's own comment.")).toBeVisible();
  await edit.click();
  await expect(field, step("Edit opens the comment's editor.")).toBeVisible();
  await field.click();
  await field.fill("Why does the lease return at midnight?");
  await expect(field, step("the changed text is in the editor.")).toHaveValue(
    "Why does the lease return at midnight?"
  );
  await expect(field, step("the editor holds the caret.")).toBeFocused();
  const question = card.getByRole("alert").filter({ hasText: "Discard draft?" });
  await page.keyboard.press("Escape");
  await expect(question, step("Escape over the changed text asks Discard draft?.")).toBeVisible();
  await expect(field, step("the editor keeps the caret while it asks.")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(field, step("a second Escape ends the edit and takes the editor away.")).toHaveCount(
    0
  );
  await expect(
    edit,
    step(
      "after the second Escape the focus is back on the card's Edit button, which opened the editor, not on the page body."
    )
  ).toBeFocused();
});

// The tester's red row of round 19 (R12; the architect's ruling of 2026-11-25T13:35Z): on today's
// issue page one Escape in the header's title editor ends the edit with the focus back on Edit
// title, as the new issue page puts it back on its title. At c6dc939c IssueHeader.tsx's Escape
// ends the edit and leaves the focus on the page's body, so the keyboard reader's next Tab starts
// from the top of the page. Measured in Chromium, Firefox and WebKit (infra-2160-t-ph19-b-8k7zf).
// @firefox: where the engine puts the focus when the element that held it is taken away
// (playwright.config.ts names the clause).
test("k-keyboard-reach: on today's issue page an Escape in the header's title editor ends the edit with the focus on Edit title @firefox", async ({
  page,
}) => {
  const step = (text: string) => `k-keyboard-reach: Escape in today's title editor: ${text}`;
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ project: "CORE", title: "Ledger rollout" });
  await page.setViewportSize(WIDE);
  await page.goto(`/issues/${issue.key}`);
  const header = page.getByTestId("issue-header");
  const editTitle = header.getByRole("button", { name: "Edit title" });
  const editor = page.getByRole("textbox", { name: "Issue title" });
  await expect(
    header.getByRole("heading", { level: 1, name: "Ledger rollout" }),
    step("the header draws the title")
  ).toBeVisible();
  await editTitle.focus();
  await page.keyboard.press("Enter");
  await expect(editor, step("Enter on Edit title opens the editor")).toBeFocused();
  await page.keyboard.type(" draft");
  await page.keyboard.press("Escape");
  await expect(editor, step("Escape ends the edit")).toHaveCount(0);
  await expect(
    header.getByRole("heading", { level: 1, name: "Ledger rollout" }),
    step("Escape puts the title back")
  ).toBeVisible();
  await expect(editTitle, step("Escape gives the focus back to Edit title")).toBeFocused();
});

// ---------------------------------------------------------------------------
// The tester's rows of round 22 for the Inbox's reading view below 1280 px (INFRA-2160, head
// e4ece294): the last item's hand-off, Escape in the answer box that Enter opens, and j and k from
// one kind of item to another. Each is a row that fails at the head by the focus step it names.
// ---------------------------------------------------------------------------

/** Seeds CORE with one open question that has no options, and answers its row id. */
async function seedSoleQuestion(): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  const issue = await createIssue({ force: true, project: "CORE", title: "Last rollout" });
  return (await createAsk(issue.key, { question: "Who owns the last rollout?" }, asker)).id;
}

/** Waits until the head of the reading view draws `name` as a button that takes a press: the
 *  head draws Approve and Request changes from its first draw and holds them until the document's
 *  read returns. */
async function headButtonTakesPress(
  page: Page,
  name: "Approve" | "Request changes",
  step: string
): Promise<void> {
  await expect(
    paneHead(page).getByRole("button", { disabled: false, exact: true, name }),
    step
  ).toBeVisible();
}

/** Whether the keyboard focus is inside the Inbox's empty message (`data-inbox-empty`). */
function focusIsInEmptyMessage(page: Page): Promise<boolean> {
  return page.evaluate(() => document.activeElement?.closest("[data-inbox-empty]") != null);
}

/** What the only item of the list is acted on by, and where the act is made. */
const SOLE_ITEM_ACTS = [
  { act: "answered with Enter, text and Ctrl+Enter", from: "its reading view" },
  { act: "answered with Enter, text and Ctrl+Enter", from: "its short row" },
  { act: "approved with a", from: "its reading view" },
  { act: "approved with a", from: "its short row" },
  { act: "sent back with Shift+R, a reason and Ctrl+Enter", from: "its reading view" },
  { act: "declined with its Decline to answer button", from: "its reading view" },
] as const;

for (const sole of SOLE_ITEM_ACTS) {
  // The tester's red row of round 22 (R8: "If no item is left, the list's empty message has the
  // focus"; ruling Q2: "[data-inbox-empty] takes the focus"): the architect's ruling of
  // 2026-11-26T22:59Z, failure I1. The last item of the list, acted on in its reading view or from
  // its short row below 1280 px by an answer, an approval, a request for changes or a decline,
  // leaves the view closed and the empty message with the focus. At e4ece294 the focus falls to the
  // page's body: those acts hand on through `recordAnswered` (`Inbox.tsx:1445-1449`), which
  // begins no hand-off, so in the reading view `stillThere` (`pane-selection.ts:495-508`) refuses
  // it and `landHandOff` returns at `pane-selection.ts:545`, before `focusTo` is set (`:563`);
  // snooze and dismiss begin one and pass.
  // SLOW(>=2s): one item seeded and the Inbox opened once at 1024x768, its reading view opened by a
  // click or a key, and one write sent.
  // @firefox: where the engine leaves the focus once the element that held it is removed (the view
  // closes on the last item), which playwright.config.ts names in its clause.
  test(`inbox reading view: the only item, ${sole.act} from ${sole.from}, leaves the list's empty message focused @firefox`, async ({
    page,
  }) => {
    const step = (text: string) =>
      `inbox reading view: the only item, ${sole.act} from ${sole.from}: ${text}`;
    const fromView = sole.from === "its reading view";
    let id: string;
    if (sole.act.startsWith("approved") || sole.act.startsWith("sent back")) {
      await createProject({ key: "CORE", name: "Core" });
      id = (await seedApprovalAtVersionTwo("CORE", "Last rollout")).askId;
    } else {
      id = await seedSoleQuestion();
    }
    await page.setViewportSize(NARROW);
    await page.goto("/");
    await expect(inboxRow(page, id), step("the item is listed")).toBeVisible();
    if (fromView) {
      await selectRow(page, id);
    } else {
      await inboxRow(page, id).focus();
    }
    if (sole.act.startsWith("answered")) {
      await page.keyboard.press("Enter");
      await expect(
        page.locator("[data-ask-answer]"),
        step("Enter puts the caret in the answer box")
      ).toBeFocused();
      await page.keyboard.type("Dana.");
      await page.keyboard.press("Control+Enter");
    } else if (sole.act.startsWith("approved")) {
      if (fromView)
        await headButtonTakesPress(page, "Approve", step("the head's Approve takes a press"));
      await page.keyboard.press("a");
    } else if (sole.act.startsWith("sent back")) {
      if (fromView) {
        await headButtonTakesPress(
          page,
          "Request changes",
          step("the head's Request changes takes a press")
        );
      }
      await page.keyboard.press("Shift+R");
      await expect(
        page.getByRole("dialog", { name: "Request changes" }).getByLabel("Reason"),
        step("Shift+R puts the caret in the reason")
      ).toBeFocused();
      await page.keyboard.type(NEXT_ITEM_REASON);
      await page.keyboard.press("Control+Enter");
    } else {
      await inboxPane(page).getByRole("button", { name: "Decline to answer" }).focus();
      await page.keyboard.press("Enter");
      const dialog = page.getByRole("dialog", { name: "Decline to answer" });
      await expect(dialog, step("the Decline box opens")).toBeVisible();
      await dialog.getByRole("button", { name: "Decline to answer" }).focus();
      await page.keyboard.press("Enter");
    }
    await expect(readingView(page), step("the view closes: nothing is left")).toHaveCount(0);
    await expect(inboxRow(page, id), step("the item has left the list")).toHaveCount(0);
    await expect
      .poll(() => focusIsInEmptyMessage(page), {
        message: step(
          "the empty message has the focus (R8: 'If no item is left, the list's empty message has the focus')"
        ),
      })
      .toBe(true);
  });
}

for (const draft of ["", "A half answer"]) {
  // The tester's red row of round 22 (keys table, Inbox Escape: "Go back: from the document to the
  // list"; ruling Q1: Enter on a short row puts the caret in the answer box): the architect's
  // ruling of 2026-11-26T22:59Z, failure I2. In the answer box that Enter opened, the first Escape
  // puts the focus on the view's scroller and a second Escape returns to the list at the row. At
  // e4ece294 Escape does nothing there, however often it is pressed: `inbox.back` stands down in the
  // reading view (`useInboxKeymap.ts:877-880`) and `inbox.leave-reading` stands down in a text
  // field (`useInboxKeymap.ts:900-904`), so no binding takes the key, while beside the column and in
  // the drawer the same Escape leaves the box.
  // SLOW(>=2s): one question seeded and the Inbox opened once at 1024x768, its reading view opened
  // by Enter on the short row, then two Escapes.
  // @firefox: where the engine leaves the focus when Escape is pressed in a textarea and the focus
  // moves to the scroller that holds it (playwright.config.ts names the clause).
  test(`inbox reading view: Escape in the answer box that Enter opened, ${draft === "" ? "empty" : "holding a draft"}, puts the focus on the view's scroller, and a second Escape returns to the list at its row @firefox`, async ({
    page,
  }) => {
    const step = (text: string) =>
      `inbox reading view: Escape in the answer box, ${draft === "" ? "empty" : "holding a draft"}: ${text}`;
    const id = await seedSoleQuestion();
    await page.setViewportSize(NARROW);
    await page.goto("/");
    await expect(inboxRow(page, id), step("the question is listed")).toBeVisible();
    await inboxRow(page, id).focus();
    await page.keyboard.press("Enter");
    await expect(
      page.locator("[data-ask-answer]"),
      step("Enter on the short row puts the caret in the answer box")
    ).toBeFocused();
    if (draft !== "") await page.keyboard.type(draft);
    await page.keyboard.press("Escape");
    await expect(
      paneScroller(page),
      step("the first Escape puts the focus on the view's scroller")
    ).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(readingView(page), step("a second Escape leaves the view")).toHaveCount(0);
    await expect(
      inboxRow(page, id),
      step("a second Escape puts the focus on the question's row")
    ).toBeFocused();
  });
}

// The tester's red row of round 22 (ruling Q2: the focus on the view's scroller, "so Space scrolls
// it"; the column's `j` and `k` leave the focus where it is): the architect's ruling of
// 2026-11-26T22:59Z, failure I3. In the reading view `k` from a thread to an approval request keeps
// the focus on the view's scroller, so PageDown and the arrows keep scrolling the request. At
// e4ece294 the focus falls to the page's body whenever the next item is another kind, since the
// new kind draws its own scroller and `move` has forgotten the focus the view held
// (`useInboxKeymap.ts:374-381`, which sets `paneFocus.current` to null before it steps), so the
// hand-off of `useInboxKeymap.ts:452-464` has nothing to give to the new scroller.
// SLOW(>=2s): a thread with a reply and two tall approval requests seeded and the Inbox opened
// once at 1024x768, the thread's reading view opened by a click, then one k and one PageDown.
// @firefox: where the engine leaves the focus once the element that held it, the scroller of the
// thread's frame, is replaced by the request's (playwright.config.ts names the clause).
test("inbox reading view: k from a thread to an approval request keeps the focus on the view's scroller, where PageDown scrolls @firefox", async ({
  page,
}) => {
  const step = (text: string) => `inbox reading view: k from a thread to a request: ${text}`;
  await setLiveSessions([THREAD_SESSION]);
  await createProject({ key: "CORE", name: "Core" });
  const threadIssue = await createIssue({ force: true, project: "CORE", title: "Plain thread" });
  const root = await createComment(threadIssue.key, { body: "Root of the plain thread?" });
  await createComment(
    threadIssue.key,
    { body: "One reply.", reply_to: root.id },
    asSession(THREAD_SESSION)
  );
  const [, tall] = await seedTallAsks("CORE");
  if (tall === undefined) throw new Error("two tall approval requests were seeded");
  const thread = replyRowId(root.id);
  await page.setViewportSize(NARROW);
  await page.goto("/");
  await expect(page.locator("[data-inbox-row]"), step("the list holds three items")).toHaveCount(3);
  await selectRow(page, thread);
  await expect(
    paneScroller(page),
    step("the click puts the focus on the thread's scroller")
  ).toBeFocused();
  await page.keyboard.press("k");
  await expect(
    readingView(page),
    step("k reads the approval request above the thread")
  ).toHaveAttribute("data-inbox-reading-row", tall.askId);
  await expect(
    paneScroller(page),
    step(
      "after k the focus is still on the view's scroller, where PageDown, ArrowDown and Space scroll"
    )
  ).toBeFocused();
  await expect
    .poll(() => paneScroller(page).evaluate((node) => node.scrollHeight - node.clientHeight), {
      message: step("the request's document is drawn, with room to scroll"),
    })
    .toBeGreaterThan(200);
  const before = await settledScrollTop(paneScroller(page), 3);
  await page.keyboard.press("PageDown");
  const after = await settledScrollTop(paneScroller(page), 5);
  expect(after, step("PageDown after k scrolls the request")).toBeGreaterThan(before);
});
