/** The command bar in the visual review: one row per state of the bar's design (design v3 of
 *  dispatch://INFRA-1588, "How it is judged"), each driving the bar into that state through a run
 *  of the fake models and comparing what it draws against a committed baseline, at 1440 x 900,
 *  over the page and, for states 2, 3, 4, 6 and 10, in the panel at the side.
 *
 *  WHERE IT RUNS. In the `visual-bar` project (e2e/playwright.config.ts), inside the pinned
 *  Playwright image that `e2e/visual-run.sh` starts, beside `e2e/visual.e2e.ts`'s pages and under
 *  that file's rules of place: the shots are only the same rendering there, every row skips
 *  itself where `DISPATCH_E2E_VISUAL_SEED` is unset, and nothing imported here reaches `./psql`,
 *  which the image does not carry. The bar's locators come from `e2e/going-locators.ts` and the
 *  fake models' scripts from `e2e/fake-models-helpers.ts`, both written for that. The stack that
 *  script starts serves the fake writing model and the fake decision model
 *  (`DISPATCH_E2E_MODELS=fake`, e2e/harness-models.ts), so every row also skips itself where it
 *  does not.
 *
 *  ONE SEED, AND NO ROW SEEDS FOR ANOTHER LOGIN. The issues these rows read, plan on and change
 *  are the review corpus's own (`seedBarIssues`, e2e/review-seed.ts): seven issues of project
 *  OPS, bob's, with no ask among them, which no page of the shot set draws, and the dome routed
 *  to the workspace's Observer, so a message to the Observer is placed on the dome. A row reads
 *  their keys from the seed file and scripts the fake models with them, and writes nothing
 *  before it opens the bar but what its own login must have done: state 1 opens the two issues
 *  its Recent lists, and state 6 writes the three comments on the shutter its plan edits and
 *  deletes, since a plan edits or deletes only a comment its person wrote.
 *
 *  WHAT A ROW LEAVES. A row signs in as a login of its own (`bar-<state>`), so its runs, its
 *  conversations and History are its own: no other row's History lists them, and a plan that
 *  waits reaches that login's Inbox alone, never alice's, whose Inbox the pages' rows shoot. And
 *  each row ends what it starts: a run it holds working is stopped, a plan it leaves waiting is
 *  discarded, and a plan it applies is undone. What stays is a message on an issue of OPS, which
 *  Undo does not take back; the three comments state 6 writes on the shutter, which its discarded
 *  plan leaves as they were, whose words match no row's search, and which no row after it reads;
 *  and the one issue the filing row files, which no other row reads: the chart and the table
 *  read the seven seeded issues by key, and its words - a weather station on the roof - match
 *  none that another row searches for, so it is no hit of state 1's search or of state 3's
 *  search step. So a row draws the same whichever rows ran before it.
 *  These writes are the states under review, so the guard every page row installs
 *  (`e2e/visual-guard.ts`), which stops each write its page sends, is not installed here. This
 *  project runs after both of the pages' instead (`visual-bar` is the last visual project of
 *  e2e/playwright.config.ts, and the run has one worker), so no page row draws after any of it.
 *
 *  WHAT EACH ROW DOES THAT THE PAGES' ROWS DO NOT. A page row signs in, navigates and waits; a
 *  bar row drives the bar, which is the state under review: it presses Control+K, types, presses
 *  Enter, Escape and Control+Shift+K, and clicks the buttons the design names (Keep at the side,
 *  New, Close, History, Discard, Undo, the chip). A click leaves the pointer over what it
 *  clicked, and a hovered control draws in another colour, so the pointer is moved to the
 *  window's top left corner before each shot, where the bar's backdrop or the rail's own
 *  background is. The caret is hidden by `toHaveScreenshot`'s default (`caret: "hide"`), and a
 *  focus ring is the one the keys under review leave, which is the same on every run.
 *
 *  HOW A ROW REACHES THE BAR. Enter on the line Ask hands the run to the panel at the side (the
 *  approved choice of dispatch://INFRA-1588/ask/00f2a090-0324-49a1-b352-b04cb4ff4f50), so a row
 *  that shoots the conversation over the page asks and then brings it back with Ctrl-K, the
 *  panel's own key for that (`askIntoTheBar`): the bar holds the same conversation it held when
 *  it started the run, and states 2, 3, 4 and 6 move it to the side with its key for their panel
 *  shots, as before. State 10 is the handoff itself, the panel docked with the cursor in its box
 *  and then widened to the content area. Do and File as a new issue keep to the bar.
 *
 *  WHAT WOULD DRIFT BETWEEN TWO RUNS OF ONE STATE, and what is done about each:
 *
 *  - The clock. Each row installs the page's clock as `e2e/visual.e2e.ts` does
 *    (`page.clock.install`, not `pauseAt`), so the bar's own waits - the pause before the short
 *    answer, the decision's 500 ms, a refetch's interval - fire as they do for a person. It is
 *    installed at the row's own start rather than a minute after the seed, because what these
 *    rows draw is what the row wrote, at the server's own time: History and the Inbox stamp a
 *    conversation with the moment the server stored it, so a page clock pinned to the seed would
 *    read the gap between the seed and the row ("in 9 minutes"), which moves with the run's pace.
 *    At the row's start it reads "just now". Two states draw a time of day, which no install at
 *    the run's own time can hold still from one day to the next: a block's caption ("as of
 *    4:40 PM") and an applied plan's title ("Applied 3 changes · 4:40 PM"). Those two rows
 *    install the clock at 16:30 UTC of one fixed day instead (`fixedDay`), and set it forward
 *    to a minute of their own just before the Enter whose moment the shot draws
 *    (`setSystemTime`, which fires no timer), and the project draws times in en-US and in UTC.
 *    Only ever forward: the page's refresh scheduler reads a fetch that started later than a
 *    refresh was asked for as having answered it (`web/src/api/query-refresh.ts`), so a clock
 *    set back would keep an item chip on the title it read before a change.
 *  - Times. The `<time>` elements are masked, as `e2e/visual.e2e.ts` masks them, and each reads
 *    "just now" here, a box that holds its width. The words that count seconds from the
 *    person's Enter - the timer beside "Thinking…", the newest step line's timer and the time in
 *    the "Worked for 2 s · read 2 items" line the steps fold into - are masked too, each held to
 *    one width while the shot is taken (`e2e/going-bar.visual.css`, which says why), so the shot
 *    shows where each stands and the row reads the words around them with an assertion. The
 *    rail's Inbox entry is masked, for the reason `e2e/visual.e2e.ts` gives.
 *  - The run's pace. A run that worked for less than a second folds its steps into "Read 2
 *    items", with no time, and one that worked longer into "Worked for 2 s · read 2 items", so a
 *    run of the fake models, which answer at once, could draw either. Each run whose steps a shot
 *    draws folded takes one slow turn (`slow`), so it works for more than a second on every run.
 *  - Animations. `reducedMotion: "reduce"` turns the bar's shimmer, pulse and spinner off (each
 *    is a `motion-safe:` class), and `toHaveScreenshot` disables what is left, as it does for
 *    the pages.
 *  - The page under the bar. Every row opens the bar over project OPS's Documents tab, which
 *    holds no document and draws no time, and waits for it as `waitForRouteSettled` waits for a
 *    page of the shot set. Each shot is the window, not the full page: the bar is fixed to it.
 *  - The models. Each row scripts both fakes before it opens the bar - the writing model's runs,
 *    each by the words that start it, and its short answer, and the decision model's line - so a
 *    row draws its script's state whichever row ran before it. The comment above each row names
 *    the script that makes its state.
 *
 *  State 13's second half, a Dispatch with no answering model, is not drawn here: the bar reads
 *  whether there is one from the server's own settings, and the lane's one stack serves the fake
 *  models to every row. `e2e/going-unconfigured.e2e.ts` reads that sentence on the default
 *  stack, which has none.
 *
 *  The bounds are `e2e/visual.e2e.ts`'s - `threshold: 0`, `maxDiffPixels: 20` - which were
 *  measured on the pages; these rows' own drift has no measurement yet, and their first runs in
 *  the container are where it comes from. */
import { fileURLToPath } from "node:url";
import { type Browser, expect, type Locator, type Page, test } from "@playwright/test";

import type { Result } from "../web/src/api/paths";

import { setLiveSessions } from "./agents";
import { dashboardOrigin } from "./api";
import type { ChatReply, ChatRunScript, ChatTurn } from "./fake-chat-model";
import {
  callTool,
  decided,
  endlessStream,
  findTools,
  modelsLane,
  proposePlan,
  prose,
  refusedWith,
  say,
  scriptChat,
  scriptDecision,
} from "./fake-models-helpers";
import {
  ANSWER_ENDS_MS,
  answerPanel,
  answerText,
  askTheAgent,
  bar,
  bringOverThePage,
  expectHandedToThePanel,
  lineOnTop,
  newestRun,
  openBar,
  planCard,
  planRows,
  RUN_STEPS_MS,
  searchBox,
  sidePanel,
} from "./going-locators";
import {
  type BarSeed,
  listedRequests,
  type ReviewedRoute,
  railInboxLink,
  readVisualSeed,
  type VisualSeed,
  waitForRouteSettled,
} from "./review-routes";
import { sessionIds } from "./session-ids";
import { asUser } from "./users";
import { workspaceSessions } from "./workspace";

/** The JSON `e2e/visual-seed.ts` wrote on the host. Unset says this run is not the containerised
 *  one, which is every run but `e2e/visual-run.sh`'s. */
const seedPath = process.env.DISPATCH_E2E_VISUAL_SEED;

const SKIP_REASON =
  "The bar's states are compared against baselines, which are only the same rendering inside " +
  "the pinned Playwright image: run `e2e/visual-run.sh` from packages/dispatch, which seeds on " +
  "the host and takes every shot in that container. DISPATCH_E2E_VISUAL_SEED is unset.";

/** The style sheet applied while a shot is taken, which holds each of the words that count
 *  seconds to one width, so the mask over it is the same box on every run. */
const heldWhileShot = fileURLToPath(new URL("./going-bar.visual.css", import.meta.url));

/** 16:30 UTC on a day before any run of this lane: where a row whose shot draws a time of day
 *  installs the page's clock, ahead of the minutes it then sets the clock forward to. */
const FIXED_DAY = Date.parse("2026-11-02T16:30:00Z");
/** The minutes those rows set the clock to just before the Enter whose moment they draw, so
 *  that time reads "4:40 PM" or "4:45 PM" on every run. */
const AT_4_40_PM = Date.parse("2026-11-02T16:40:00Z");
const AT_4_45_PM = Date.parse("2026-11-02T16:45:00Z");

/** The page every row opens the bar over: project OPS's Documents tab, which holds no document
 *  and draws no time, so the page under the bar is the same in every shot. It is no page of the
 *  shot set, so it carries no layer; its one fact is its empty list. */
const pageUnderTheBar: ReviewedRoute = {
  kind: "reviewed",
  layers: [],
  name: "bar-page",
  path: (seed) => `/projects/${seed.bar.project}/documents`,
  pattern: "/projects/:key/documents",
  ready: (page) => [page.getByRole("region", { name: "Project documents empty state" })],
};

/** The short answer the fake writing model gives a pause in every row that types words. It links
 *  the telescope mount by the href the search's hit of it carries, so the answer panel draws the
 *  mount's key, which the server keeps as words, as the mount's chip. The words around the chip
 *  say nothing its title says (design v3, state 4). */
function shortAnswer(issues: BarSeed): ChatReply {
  return prose(`One issue is open on it, ${naming(issues.mount)}.`);
}

// The live registry, put back after Playwright's own `globalSetup` cleared it, as
// `e2e/visual.e2e.ts` does and for its reason: the rail these shots draw counts the agents'
// replies, and a run of this project alone (`--project=visual-bar`) runs no row of that file.
test.beforeAll(async () => {
  if (seedPath === undefined) return;
  await setLiveSessions(workspaceSessions);
});

/** Scripts both fakes for one row: the writing model's `runs`, each by the words that start it,
 *  and `shortAnswer` for a pause; and the decision model's `line`, `none` for Ask. */
async function script(
  seed: VisualSeed,
  runs: readonly ChatRunScript[],
  line = "none"
): Promise<void> {
  await scriptChat({ default: shortAnswer(seed.bar), runs });
  await scriptDecision({ default: decided(line, 0.99) });
}

/** Opens `pageUnderTheBar`, waits for it to settle, and opens the bar over it. */
async function openTheBar(page: Page, seed: VisualSeed): Promise<void> {
  await page.goto(pageUnderTheBar.path(seed));
  await waitForRouteSettled(page, pageUnderTheBar, listedRequests(page));
  await openBar(page);
}

/** Compares the window against the baseline `name`, with the pointer off every control. Soft,
 *  as `e2e/visual.e2e.ts`'s shots are, so a row whose first shot differs still takes the rest.
 *  Each time of day and each count of seconds is masked. */
async function shoot(page: Page, name: string): Promise<void> {
  await page.mouse.move(0, 0);
  await expect.soft(page).toHaveScreenshot(`${name}.png`, {
    mask: [
      page.locator("time"),
      railInboxLink(page),
      page.getByTestId("going-thinking-timer"),
      page.getByTestId("going-step-timer"),
      page.getByTestId("going-steps-fold-time"),
    ],
    maxDiffPixels: 20,
    stylePath: heldWhileShot,
    threshold: 0,
  });
}

/** A turn that reads `issue` with `dispatch_read`. */
function read(issue: string): ChatTurn {
  return callTool("dispatch_read", { issue });
}

/** `turn`, written in one piece with 400 ms between its frames. A turn is four frames at least -
 *  the role, its words or its calls, the finish and the count of tokens - so it takes 1.2 s or
 *  more, and the run it is part of works for more than a second: the line its steps fold into
 *  then says how long, "Worked for 2 s · read 2 items", on every run (see the run's pace above). */
function slow(turn: ChatTurn): ChatTurn {
  return { ...turn, chunk: 4096, delayMs: 400 };
}

/** An answer naming `issue` by a link, which the bar draws as the issue's chip. */
function naming(issue: string): string {
  return `[${issue}](/issues/${issue})`;
}

/** Writes a comment of `body` on `issue` as the person `page` is signed in as, through the route
 *  the issue page's comment box posts to, and answers its id. `page.request` sends the page's
 *  session cookie and no `Origin`, and the server refuses a cookie-authenticated write whose
 *  `Origin` is not the dashboard's (enforceCookieOrigin), so the request names it. */
async function commentOn(page: Page, issue: string, body: string): Promise<string> {
  const answer = await page.request.post(`/api/v1/issues/${issue}/comments`, {
    data: { body },
    headers: { Origin: dashboardOrigin },
  });
  expect(answer.status(), `the comment on ${issue}: ${await answer.text()}`).toBe(201);
  return ((await answer.json()) as Result<"/api/v1/issues/{key}/comments", "post">).id;
}

/** Waits until every item chip under `where` has its status and title, which it reads after it
 *  draws its key. */
async function chipsRead(where: Locator, count: number): Promise<void> {
  await expect(where.getByTestId("item-chip")).toHaveCount(count);
  await expect(where.getByTestId("item-chip-status")).toHaveCount(count);
}

/** Reads that the waiting plan's Approve and Discard, in `surface` (the bar, or the panel), are
 *  pinned to the foot of the conversation's scroll (design v3, state 6; review R1): the bar that
 *  holds them ends where the scroll ends, to the pixel, so no row shows under it. */
async function approvePinned(surface: Locator): Promise<void> {
  const actions = surface.getByTestId("going-plan-actions");
  const view = surface.getByTestId("going-conversation-view");
  await expect(actions).toBeVisible();
  await expect(view).toBeVisible();
  // The wait above drew the Approve bar, and a drawn element has a box, so the box is there.
  const pinned = await actions.boundingBox();
  // The wait above drew the conversation's scroll as well, so its box is there as the bar's is.
  const scroll = await view.boundingBox();
  if (pinned === null || scroll === null) {
    throw new Error("the plan's Approve bar or the conversation's scroll is not drawn");
  }
  expect(
    Math.abs(pinned.y + pinned.height - (scroll.y + scroll.height)),
    "the gap between the pinned Approve bar and the foot of the scroll"
  ).toBeLessThanOrEqual(1);
}

/** Reads that the hint under the box of `surface` starts at the box's left edge, under the box
 *  and not under the mode button (design v3, Keys; review R2): its words begin where its own
 *  indent ends, which is where the box's frame begins. */
async function hintUnderBox(surface: Locator): Promise<void> {
  const words = await surface
    .getByTestId("going-box-hint")
    .evaluate(
      (hint) =>
        hint.getBoundingClientRect().left + Number.parseFloat(getComputedStyle(hint).paddingLeft)
    );
  const box = await surface
    .getByRole("textbox", { name: "Ask the command bar" })
    .evaluate((input) => input.parentElement?.getBoundingClientRect().left ?? Number.NaN);
  expect(
    Math.abs(words - box),
    "how far the hint's words start from the box's left edge"
  ).toBeLessThanOrEqual(1);
}

/** Reads that the plan card's pinned bar (`surface`'s) says rows stand under it, how many and
 *  which of those cannot be undone, by name, with its soft shadow on its top edge (review A1): a
 *  person sees at once that more is below, and what Enter would approve there that nothing
 *  takes back. Of the rows that stand under the bar in each shot, the message is the one. */
async function rowsUnderTheBar(surface: Locator): Promise<void> {
  const more = surface.getByTestId("going-plan-more");
  await expect(more).toBeVisible();
  await expect(more).toHaveText(/^\d+ more ↓ · includes a message that cannot be undone$/u);
  await expect(more.getByRole("button", { name: /^\d+ more$/u })).toBeVisible();
  await expect(surface.getByTestId("going-plan-actions")).toHaveClass(/shadow-/u);
}

/** Where the parts of `surface` that never scroll stand in the window (review B1): its header -
 *  the title, History, New, the move and Close - and its box, each as the left, top, width and
 *  height of its box. */
function framePlaces(surface: Locator): Promise<number[]> {
  return surface.evaluate((frame) =>
    [
      frame.querySelector('[data-testid="going-header"]'),
      frame.querySelector('[aria-label="Ask the command bar"]')?.parentElement,
    ].flatMap((part) => {
      if (part === null || part === undefined) throw new Error("no header or box is drawn");
      const { height, width, x, y } = part.getBoundingClientRect();
      return [x, y, width, height];
    })
  );
}

/** How far, in pixels, the furthest edge of `surface`'s header or box now stands from where
 *  `placed` (`framePlaces`) says it stood. */
async function frameMoved(surface: Locator, placed: readonly number[]): Promise<number> {
  const now = await framePlaces(surface);
  return Math.max(...now.map((value, at) => Math.abs(value - Number(placed[at]))));
}

/** How many rows of the plan card in `surface` meet its Approve bar, as the window lays them
 *  out. */
function rowsMeetingTheBar(surface: Locator): Promise<number> {
  return surface.evaluate((frame) => {
    const bar = frame.querySelector('[data-testid="going-plan-actions"]')?.getBoundingClientRect();
    if (bar === undefined) throw new Error("the plan's Approve bar is not drawn");
    return [...frame.querySelectorAll('[data-testid="going-plan-row"]')].filter((row) => {
      const box = row.getBoundingClientRect();
      return (
        box.left < bar.right && bar.left < box.right && box.top < bar.bottom && bar.top < box.bottom
      );
    }).length;
  });
}

/** Presses that line's button, which scrolls the conversation to the rows, then scrolls the
 *  conversation to its end, so every row kind of the plan has stood above the bar and the line
 *  and shadow are gone: the bar shows no cue because nothing stands under it. Only the
 *  conversation scrolls (review B1): the header and the box of `surface` stand where they stood
 *  before the press, to the pixel, after it and at the end. At the end the bar is the card's
 *  last part, not pinned: the conversation's own foot padding stands below it. So the end is
 *  read once it has settled: the scroll at its end, the header and the box in place, and no row
 *  of the card meeting the bar. */
async function scrollPlanToItsEnd(surface: Locator): Promise<void> {
  const view = surface.getByTestId("going-conversation-view");
  const actions = surface.getByTestId("going-plan-actions");
  await expect(surface.getByTestId("going-header")).toBeVisible();
  await expect(surface.getByRole("textbox", { name: "Ask the command bar" })).toBeVisible();
  // The waits above drew the header and the box, so each has a place to read before the press.
  const placed = await framePlaces(surface);
  await surface.getByTestId("going-plan-more-button").click();
  await expect
    .poll(() => frameMoved(surface, placed), {
      message: "how far the header or the box moved at the press of the line",
    })
    .toBeLessThanOrEqual(1);
  await view.evaluate((scroll) => {
    scroll.scrollTop = scroll.scrollHeight;
  });
  await expect(surface.getByTestId("going-plan-more")).toHaveCount(0);
  await expect(actions).not.toHaveClass(/shadow-/u);
  await expect
    .poll(
      async () => ({
        atItsEnd: await view.evaluate(
          (scroll) => scroll.scrollHeight - scroll.clientHeight - scroll.scrollTop <= 1
        ),
        inPlace: (await frameMoved(surface, placed)) <= 1,
        rowsMeetingTheBar: await rowsMeetingTheBar(surface),
      }),
      { message: "the plan at its end: its scroll, the header and the box, and its rows" }
    )
    .toEqual({ atItsEnd: true, inPlace: true, rowsMeetingTheBar: 0 });
}

/** Whether two boxes a locator measured share any point. A box that is not drawn is refused: the
 *  row asks about two that are. */
function boxesMeet(
  one: { x: number; y: number; width: number; height: number } | null,
  other: { x: number; y: number; width: number; height: number } | null
): boolean {
  if (one === null || other === null) throw new Error("a box to compare is not drawn");
  return (
    one.x < other.x + other.width &&
    other.x < one.x + one.width &&
    one.y < other.y + other.height &&
    other.y < one.y + one.height
  );
}

/** Moves the conversation to the panel at the side with its key. */
async function keepAtTheSide(page: Page): Promise<void> {
  await page.keyboard.press("Control+Shift+K");
  await expect(bar(page)).toHaveCount(0);
  await expect(sidePanel(page)).toBeVisible();
}

/** Stops the working run from the panel's empty box, with Escape. */
async function stopInThePanel(page: Page): Promise<void> {
  await sidePanel(page).getByRole("textbox", { name: "Ask the command bar" }).press("Escape");
  await expect(newestRun(page)).toHaveAttribute("data-state", "stopped_person", {
    timeout: RUN_STEPS_MS,
  });
}

/** Starts a new conversation from the bar's header, which brings the search back. */
async function newConversation(page: Page): Promise<void> {
  await bar(page).getByTestId("going-new").click();
  await expect(searchBox(page)).toHaveAttribute("placeholder", "Search or ask…");
}

/** Asks `words` with Enter on the line Ask, which starts the run and hands the conversation to the
 *  panel at the side, and brings the conversation back over the page with Ctrl-K, so the row reads
 *  and shoots it in the bar. */
async function askIntoTheBar(page: Page, words: string): Promise<void> {
  await askTheAgent(page, words);
  await expectHandedToThePanel(page);
  await bringOverThePage(page);
}

/** The seven seeded issues, which the chart and the table read and nothing else. */
function barKeys(issues: BarSeed): string[] {
  const { craters, dome, eyepiece, finder, mirror, mount, shutter } = issues;
  return [mount, craters, mirror, dome, shutter, eyepiece, finder];
}

/** One state of the design: its number, what it shows, how a row reaches it, and whether its
 *  shots draw a time of day, so the row installs the clock on the fixed day. */
interface BarRow {
  readonly state: string;
  readonly shows: string;
  readonly reach: (page: Page, seed: VisualSeed) => Promise<void>;
  readonly fixedDay?: true;
}

const barRows: readonly BarRow[] = [
  {
    // State 1. Scripts a short answer and the decision `none`, so the line on top reads Ask. The
    // words are two of the mount's title, so the search, which matches every word, lists it, and
    // not the whole title, which would rank above the line on top. The short answer names the
    // mount by its key, which the panel draws as the mount's chip, in the text and nowhere else.
    // First the person opens two issues and asks one question, which New leaves behind, so the
    // empty box shows what the design draws there: "Continue: ‹title›" for the conversation from
    // the last hour, then Recent, the items opened lately. The rows sign in with a login of their
    // own, so this row makes that login's conversation and its recent items itself, as a person
    // does, rather than a seed written for another login.
    reach: async (page, seed) => {
      const words = "telescope mount";
      const earlier = "what waits on the observatory dome";
      await script(seed, [{ turns: [say("The dome waits for its motor.")], words: earlier }]);
      for (const [key, title] of [
        [seed.bar.eyepiece, "Polish the eyepiece"],
        [seed.bar.dome, "Raise the observatory dome"],
      ] as const) {
        // The issue page marks its spec read once it shows it (`spec-marks.ts`, a PUT of
        // `/api/v1/me/documents/{id}/read`), a moment after its title shows. A goto made while
        // that write is out cuts it, and the browser finishes it after the page has gone
        // (`keepalive`), so the record of this page's requests holds it with no answer and no
        // failure for good: the next page's settle wait counts it as an Inbox read still out
        // and never settles (CI at 9e517ae6, state 01, two cut marks 4 ms and 11 ms before the
        // two gotos). So the row leaves each issue once its mark is answered.
        const marked = page.waitForResponse(
          (response) =>
            response.request().method() === "PUT" &&
            /^\/api\/v1\/me\/documents\/[^/]+\/read$/u.test(new URL(response.url()).pathname)
        );
        await page.goto(`/issues/${key}`);
        // The title has come, so the item is recorded with its title, as Recent lists it.
        await expect(page.getByText(title).first()).toBeVisible();
        expect((await marked).ok(), `the read mark of ${key}'s spec`).toBe(true);
      }
      await openTheBar(page, seed);
      await askIntoTheBar(page, earlier);
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      await newConversation(page);
      await expect(bar(page).locator("#search-option-continue")).toHaveText(
        "Continue: What waits on the observatory dome"
      );
      // Each recent item reads as an Issues hit does (review a12): its kind's icon, its key, its
      // title and its status at the row's end, which the row reads from the issue's summary after
      // it draws its key, so the shot waits for them.
      const recent = bar(page).getByRole("group", { name: "Recent" }).getByRole("option");
      await expect(recent.getByTestId("search-hit-title")).toHaveText([
        "Raise the observatory dome",
        "Polish the eyepiece",
      ]);
      await expect(recent.getByTestId("search-hit-status")).toHaveCount(2);
      // New put the cursor back in the box, so its focus shows and typing lands in it.
      await expect(searchBox(page)).toBeFocused();
      // The box is on top; History and the move are in the foot, and Close is Esc there alone.
      await expect(bar(page).getByTestId("going-header")).toHaveCount(0);
      await expect(
        bar(page).getByTestId("search-foot").getByTestId("going-history-button")
      ).toBeVisible();
      await expect(bar(page).getByRole("button", { name: "Close" })).toHaveCount(0);
      await shoot(page, "01-empty");
      // "/" in the empty box opens the filters; the Filters button closes them.
      await searchBox(page).press("/");
      await expect(bar(page).getByTestId("search-filters-panel")).toBeVisible();
      await shoot(page, "01-filters");
      await bar(page).getByTestId("search-filters-button").click();
      await expect(bar(page).getByTestId("search-filters-panel")).toHaveCount(0);
      await searchBox(page).fill(words);
      await expect(bar(page).locator(`#search-option-issue-${seed.bar.mount}`)).toBeVisible();
      await expect(answerPanel(page)).toHaveAttribute("data-state", "ended", {
        timeout: ANSWER_ENDS_MS,
      });
      await expect(bar(page).getByTestId("search-line-deciding")).toHaveCount(0);
      await expect(lineOnTop(page)).toContainText(`Ask ${words}`);
      await chipsRead(answerText(page), 1);
      // The preview is a card above the hits, with its one button outside the list.
      await expect(answerPanel(page).getByRole("button", { name: "Open in panel" })).toBeVisible();
      await expect(answerPanel(page).getByTestId("going-answer-item")).toHaveCount(0);
      await shoot(page, "01-typed");
    },
    shows: "the empty box, the filters, and typed words with the line on top",
    state: "01",
  },
  {
    // State 2. Scripts a run whose one model call streams without end and calls nothing, so the
    // bar holds the question and "Thinking…" until the row stops it.
    reach: async (page, seed) => {
      const words = "watch the sky over the observatory tonight";
      await script(seed, [{ turns: [endlessStream()], words }]);
      await openTheBar(page, seed);
      await askIntoTheBar(page, words);
      await expect(newestRun(page)).toHaveAttribute("data-state", "working");
      await expect(newestRun(page).getByTestId("going-thinking")).toBeVisible();
      await shoot(page, "02-thinking-bar");
      await keepAtTheSide(page);
      await expect(sidePanel(page).getByTestId("going-thinking")).toBeVisible();
      await shoot(page, "02-thinking-panel");
      await stopInThePanel(page);
    },
    shows: "the moment after Enter",
    state: "02",
  },
  {
    // State 3. Scripts a run that looks for a tool twice, reads two issues in one model call
    // under the words it wrote beside them, searches, then streams without end: five finished
    // steps, of which the newest three show as lines and the two oldest, the looks, fold into
    // the one grey line above them, which counts them (review a7, N2); then "Thinking…" with the
    // run's timer, since no call runs while the model writes, and the run still working. Words
    // sent while it works wait under it; the row drops them before
    // it stops the run.
    reach: async (page, seed) => {
      const words = "check the telescope work";
      const reading: ChatTurn = {
        text: "Reading the telescope's issues…",
        toolCalls: [
          {
            arguments: { arguments: { issue: seed.bar.mount }, tool: "dispatch_read" },
            name: "call_tool",
          },
          {
            arguments: { arguments: { issue: seed.bar.craters }, tool: "dispatch_read" },
            name: "call_tool",
          },
        ],
      };
      const searching: ChatTurn = {
        toolCalls: [
          { arguments: { project: seed.bar.project, query: "telescope" }, name: "search" },
        ],
      };
      await script(seed, [
        {
          turns: [
            findTools("list the issues of a project"),
            findTools("read an issue"),
            reading,
            searching,
            endlessStream(),
          ],
          words,
        },
      ]);
      await openTheBar(page, seed);
      await askIntoTheBar(page, words);
      const run = newestRun(page);
      const finished = '[data-testid="going-step"][data-running="false"]';
      // The fifth finished step folds at once: the two looks, the oldest, into the line above,
      // which says how many steps it holds.
      await expect(run.getByTestId("going-steps-older")).toHaveAccessibleName("2 earlier steps", {
        timeout: RUN_STEPS_MS,
      });
      await expect(run.locator(finished)).toHaveCount(3);
      await expect(run.getByTestId("going-step-label").first()).toHaveText(/^Read OPS-\d+$/u);
      // Both calls under the model's words ended, so its header reads in the past tense.
      await expect(run.getByTestId("going-step-words")).toHaveText("Read the telescope's issues");
      // No call runs while the model streams, so the run ends with "Thinking…" and its timer
      // rather than a step line carrying it; the timer is masked in the shot, so it is read here.
      await expect(run.getByTestId("going-step-timer")).toHaveCount(0);
      await expect(run.getByTestId("going-steps-thinking")).toContainText("Thinking…");
      await expect(
        run.getByTestId("going-steps-thinking").getByTestId("going-thinking-timer")
      ).toHaveText(/^\d+ s$/);
      await expect(run).toHaveAttribute("data-state", "working");
      await searchBox(page).fill("and the primary mirror too");
      await searchBox(page).press("Enter");
      await expect(bar(page).getByTestId("going-queued")).toBeVisible();
      await shoot(page, "03-working-bar");
      await keepAtTheSide(page);
      await expect(sidePanel(page).getByTestId("going-queued")).toBeVisible();
      await expect(sidePanel(page).locator(finished)).toHaveCount(3);
      await shoot(page, "03-working-panel");
      await sidePanel(page).getByTestId("going-queued-drop").click();
      await expect(sidePanel(page).getByTestId("going-queued")).toHaveCount(0);
      await stopInThePanel(page);
    },
    shows: "a run that works",
    state: "03",
  },
  {
    // State 4. Scripts a run that reads two issues, the first in a slow turn, and answers with
    // both as links, which the bar draws as chips under the line its steps fold into.
    reach: async (page, seed) => {
      const words = "what telescope work is still open";
      const { craters, mount } = seed.bar;
      await script(seed, [
        {
          turns: [
            slow(read(mount)),
            read(craters),
            say(
              `Two pieces of telescope work are open:\n\n- ${naming(mount)}\n- ${naming(craters)}`
            ),
          ],
          words,
        },
      ]);
      await openTheBar(page, seed);
      await askIntoTheBar(page, words);
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      // The folded line's time is masked in the shot, so its words are read here.
      await expect(newestRun(page).getByTestId("going-steps-fold")).toHaveAccessibleName(
        /^Worked for \d+ s · read 2 items$/
      );
      await chipsRead(newestRun(page).getByTestId("going-run-answer"), 2);
      await shoot(page, "04-answer-bar");
      await keepAtTheSide(page);
      await chipsRead(sidePanel(page).getByTestId("going-run-answer"), 2);
      await shoot(page, "04-answer-panel");
    },
    shows: "an answer with its chips under the folded steps",
    state: "04",
  },
  {
    // State 5. Scripts two runs, each listing OPS's issues with `dispatch_issues` in a slow turn
    // and drawing a block over the seven seeded issues alone: a chart of them by status, then, in
    // a new conversation, a table of them by key. The clock is set forward, to 16:40 before the
    // chart's Enter and to 16:45 before the table's, since each caption says when its read was
    // made.
    fixedDay: true,
    reach: async (page, seed) => {
      const chartWords = "chart the telescope work by status";
      const tableWords = "list the telescope work";
      const listing = slow(callTool("dispatch_issues", { project: seed.bar.project }));
      const telescopeWork = { field: "key", in: barKeys(seed.bar), op: "filter" };
      // The listing is the first model call's tool call, so its result is step 2, call 1.
      const listed = [{ call: 1, step: 2 }];
      await script(seed, [
        {
          turns: [
            listing,
            callTool("draw_block", {
              chain: [telescopeWork, { field: "status", op: "group" }, { op: "count" }],
              kind: "chart",
              steps: listed,
              title: "Telescope work by status",
              x: "status",
              y: "count",
            }),
            say("Most of the telescope work is still in triage."),
          ],
          words: chartWords,
        },
        {
          turns: [
            listing,
            callTool("draw_block", {
              chain: [telescopeWork, { field: "key", op: "sort" }],
              columns: ["key", "status", "priority"],
              kind: "table",
              steps: listed,
              title: "Telescope work",
            }),
            say("Here is the telescope work, issue by issue."),
          ],
          words: tableWords,
        },
      ]);
      await openTheBar(page, seed);
      await page.clock.setSystemTime(AT_4_40_PM);
      await askIntoTheBar(page, chartWords);
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      const chart = newestRun(page).getByTestId("going-block");
      await expect(chart).toHaveAttribute("data-kind", "chart");
      await expect(chart.getByTestId("going-block-chart")).toHaveAttribute(
        "data-chart-drawn",
        "true"
      );
      await expect(chart.getByTestId("going-block-caption")).toHaveText(
        /^Telescope work by status · as of 4:40\sPM$/
      );
      await expect(newestRun(page).getByTestId("going-steps-fold")).toHaveAccessibleName(
        /^Worked for \d+ s · /
      );
      await shoot(page, "05-chart");
      await newConversation(page);
      await page.clock.setSystemTime(AT_4_45_PM);
      await askIntoTheBar(page, tableWords);
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      const table = newestRun(page).getByTestId("going-block");
      await expect(table).toHaveAttribute("data-kind", "table");
      await expect(table.getByTestId("going-block-caption")).toHaveText(
        /^Telescope work · as of 4:45\sPM$/
      );
      await chipsRead(table, barKeys(seed.bar).length);
      await expect(newestRun(page).getByTestId("going-steps-fold")).toHaveAccessibleName(
        /^Worked for \d+ s · /
      );
      await shoot(page, "05-table");
    },
    shows: "a chart, and a table",
    state: "05",
  },
  {
    // State 6. Scripts a run that proposes, in a slow turn, one row of each kind - a new issue, an
    // edit and a delete of a comment (a post's two rows), a change of fields, an edit of a long
    // comment, a document's text, a message to an agent and a close - and waits. A plan edits or
    // deletes only a comment its person wrote, so the row writes the three comments on the
    // shutter itself, as its own login, before it opens the bar. The short edit's and the
    // delete's rows come right after the new issue's, so each shot shows them under the
    // question; the long edit's comes later, so the plan's end shows its folded diff. The message
    // goes to the Observer, whom the seed routes the dome to (`seedBarIssues`), so its row names
    // the session, the issue it is posted on and its delivery (R7). The row discards the plan, so
    // nothing is sent, edited or deleted.
    reach: async (page, seed) => {
      const words = "plan a motor for the observatory dome";
      const { dome, mount, project, shutter } = seed.bar;
      // Sharing no word with any row's search, so no comment is a hit of one: state 13's shot
      // draws the hits of its words. Two are short; the third is long enough to fold, so its edit
      // reads as its first sentences, the words it changes marked, with its diff closed under them
      // and Edit on the diff's line (reviews a3, m1 and m2).
      const hingeNow = "Oil its hinge every week.";
      const hingeNext = "Oil its hinge every month.";
      const latchNow = "Its latch rattles in wind.";
      const railsNow =
        "Wipe its rails with a dry cloth after each night of use.\n\nTighten the four bolts at both ends of the track once a month, and replace the felt strip when it frays.";
      const railsNext =
        "Wipe its rails with a dry cloth after each night of use.\n\nTighten the four bolts at both ends of the track every two weeks, and replace the felt strip when it frays.";
      const hingeNote = await commentOn(page, shutter, hingeNow);
      const latchNote = await commentOn(page, shutter, latchNow);
      const railsNote = await commentOn(page, shutter, railsNow);
      await script(seed, [
        {
          afterwards: say("Discarded."),
          turns: [
            slow(
              proposePlan(
                "Fit a dome motor, retire the old shutter, and tidy your comments on it.",
                [
                  {
                    arguments: {
                      parent: dome,
                      priority: 2,
                      project,
                      spec: "The dome turns by a motor once it has one. The motor takes the place of the hand crank. The old shutter goes with it.",
                      title: "Fit a motor to the dome",
                    },
                    tool: "dispatch_issue",
                  },
                  {
                    arguments: { body: hingeNext, comment: hingeNote },
                    tool: "dispatch_edit_comment",
                  },
                  { arguments: { comment: latchNote }, tool: "dispatch_delete_comment" },
                  {
                    arguments: { issue: mount, labels: ["optics"], priority: 2 },
                    tool: "dispatch_issue_update",
                  },
                  {
                    arguments: { body: railsNext, comment: railsNote },
                    tool: "dispatch_edit_comment",
                  },
                  {
                    arguments: {
                      artifact: "spec",
                      body: "The dome turns by motor once it has one, so the spec says so.",
                      issue: dome,
                      quote: "The dome turns by hand.",
                      replace_with: "The dome turns by motor.",
                    },
                    tool: "dispatch_suggest",
                  },
                  {
                    arguments: {
                      body: "A motor for the dome is planned.",
                      delivery: "btw",
                      session: sessionIds.observer,
                    },
                    tool: "dispatch_send_to_agent",
                  },
                  {
                    arguments: {
                      issue: shutter,
                      reason: "The motor's shutter replaces it.",
                      status: "done",
                    },
                    tool: "dispatch_issue_update",
                  },
                ]
              )
            ),
          ],
          words,
        },
      ]);
      // The three post rows, in the bar and then in the panel: each says what it does to whose
      // comment on the shutter, by the shutter's chip - "your comment", since the author is the
      // row's own login (`drawState`; review a3). The short edit shows its body once, the words
      // it changes marked as a diff marks them; the delete shows the words it takes out, struck
      // through, and that a delete cannot be undone. Both stand in view under the question. The
      // long edit reads as the first sentences of what it writes, "once a month" struck and
      // "every two weeks" marked in them, its diff folded under them (review m1).
      const postRows = planCard(page).locator('[data-testid="going-plan-row"][data-kind="post"]');
      const longEdit = postRows.nth(2);
      const postRowsDrawn = async (): Promise<void> => {
        await expect(postRows).toHaveCount(3);
        const edit = postRows.nth(0);
        await expect(edit.getByTestId("going-plan-post")).toHaveText(
          new RegExp(`^Edit your comment on ${shutter}(?!\\d)`)
        );
        const body = edit.getByTestId("going-plan-field");
        await expect(body).toHaveAttribute("data-field", "body");
        await expect(body).not.toContainText("Body:");
        await expect(body.locator("del")).toHaveText(/^week/u);
        await expect(body.locator("ins")).toHaveText(/^month/u);
        const remove = postRows.nth(1);
        await expect(remove.getByTestId("going-plan-post")).toHaveText(
          new RegExp(`^Delete your comment on ${shutter}(?!\\d)`)
        );
        await expect(remove.getByTestId("going-plan-quote").locator("del")).toHaveText(latchNow);
        await expect(remove.getByTestId("going-plan-final")).toHaveText(
          "Cannot be undone once deleted"
        );
        await expect(edit).toBeInViewport();
        await expect(remove).toBeInViewport();
        await expect(longEdit.getByTestId("going-plan-post")).toHaveText(
          new RegExp(`^Edit your comment on ${shutter}(?!\\d)`)
        );
        await expect(longEdit.getByTestId("going-plan-sentences")).toHaveText(
          /^Wipe its rails with a dry cloth after each night of use\. /u
        );
        const preview = longEdit.getByTestId("going-plan-sentences");
        await expect(preview.locator("del")).toHaveText("once a month");
        await expect(preview.locator("ins")).toHaveText("every two weeks");
        await expect(longEdit.getByTestId("going-plan-diff")).toHaveJSProperty("open", false);
      };
      await openTheBar(page, seed);
      await askIntoTheBar(page, words);
      await expect(newestRun(page)).toHaveAttribute("data-state", "waiting", {
        timeout: RUN_STEPS_MS,
      });
      await expect(planCard(page)).toHaveAttribute("data-state", "waiting", {
        timeout: RUN_STEPS_MS,
      });
      await expect(planRows(planCard(page))).toHaveCount(8);
      await expect(
        planCard(page)
          .getByTestId("item-chip")
          .filter({ hasNot: page.getByTestId("item-chip-status") })
      ).toHaveCount(0);
      // The message row names the session, the issue it is posted on by its chip, and the
      // delivery; the suggestion names the issue whose spec it changes by its chip.
      await expect(planCard(page).getByTestId("going-plan-message")).toHaveText(
        new RegExp(`^Message to Observer on ${dome}(?!\\d).* · as a BTW$`)
      );
      await expect(planCard(page).getByTestId("going-plan-document")).toHaveText(
        new RegExp(`^Suggest a change to the spec of ${dome}(?!\\d)`)
      );
      await postRowsDrawn();
      // The card is taller than the view: it shows from its question down, its Approve and
      // Discard pinned at the foot of the conversation's scroll with no row under them, and the
      // hint under the box, from the box's left edge, says Enter approves.
      await expect(newestRun(page).getByTestId("going-run-words")).toBeInViewport();
      await expect(planCard(page).getByTestId("going-plan-approve")).toBeInViewport();
      await expect(planCard(page).getByTestId("going-plan-reject")).toBeInViewport();
      await approvePinned(bar(page));
      await expect(bar(page).getByTestId("going-box-hint")).toHaveText("↵ Approve");
      await hintUnderBox(bar(page));
      // Rows stand under the pinned bar, which says so and what Enter would approve there
      // (review A1); the shot after it is the plan scrolled to its end, every row kind read, the
      // header and the box where they stood (review B1), and the long edit's diff folded under
      // its sentences.
      await rowsUnderTheBar(bar(page));
      await shoot(page, "06-plan-bar");
      await scrollPlanToItsEnd(bar(page));
      await expect(longEdit).toBeInViewport({ ratio: 1 });
      await shoot(page, "06-plan-end");
      await keepAtTheSide(page);
      await expect(planRows(planCard(page))).toHaveCount(8);
      await expect(
        planCard(page)
          .getByTestId("item-chip")
          .filter({ hasNot: page.getByTestId("item-chip-status") })
      ).toHaveCount(0);
      await postRowsDrawn();
      // The panel scrolls as the bar does: the question at its top, nothing cut under its header,
      // Approve pinned at the foot of its scroll, and the hint under its box.
      await expect(newestRun(page).getByTestId("going-run-words")).toBeInViewport();
      await expect(planCard(page).getByTestId("going-plan-approve")).toBeInViewport();
      await approvePinned(sidePanel(page));
      await expect(sidePanel(page).getByTestId("going-box-hint")).toHaveText("↵ Approve");
      await hintUnderBox(sidePanel(page));
      await rowsUnderTheBar(sidePanel(page));
      await shoot(page, "06-plan-panel");
      // The panel's line scrolls its conversation alone, as the bar's does (review B1).
      await scrollPlanToItsEnd(sidePanel(page));
      await planCard(page).getByTestId("going-plan-reject").click();
      await expect(planCard(page)).not.toHaveAttribute("data-state", "waiting", {
        timeout: RUN_STEPS_MS,
      });
    },
    shows: "a plan of each row kind that waits",
    state: "06",
  },
  {
    // State 7. Scripts a run that proposes, in a slow turn, two changes and a message on an issue,
    // then answers once they ran. The message is on the eyepiece and to no session: a message to
    // an agent that ran here would reach the workspace's live session, whose pages the pages'
    // rows shoot. Nothing routes, claims or follows the eyepiece, so its row says it reaches no
    // session, in the form every message row has, and reads the same in the card that waits and
    // in the card that ran. The row approves with Enter, with the clock set forward to 16:40,
    // since the applied title says when; then it presses Undo, which takes both changes back and
    // leaves the message sent, which its row says, so the run reads undone and the line its steps
    // fold into still says what the run did. Each shot waits for the finder scope's chip to read
    // its title as the change, then the Undo, left it.
    fixedDay: true,
    reach: async (page, seed) => {
      const words = "raise the eyepiece and rename the finder scope";
      const { eyepiece, finder } = seed.bar;
      await script(seed, [
        {
          afterwards: say("Done."),
          turns: [
            slow(
              proposePlan("Raise the eyepiece first, and give the finder scope its full name.", [
                { arguments: { issue: eyepiece, priority: 1 }, tool: "dispatch_issue_update" },
                {
                  arguments: { issue: finder, title: "Align the finder scope to the mount" },
                  tool: "dispatch_issue_update",
                },
                {
                  arguments: { body: "The eyepiece comes first now.", issue: eyepiece },
                  tool: "dispatch_message",
                },
              ])
            ),
            say("Raised the eyepiece, renamed the finder scope and said so on the eyepiece."),
          ],
          words,
        },
      ]);
      // The message row names whom it reaches, the issue it is on by its chip, and no delivery,
      // since a message on an issue has none.
      const message = planCard(page).getByTestId("going-plan-message");
      const reachesNoSession = new RegExp(`^Message to no session on ${eyepiece}(?!\\d)`);
      await openTheBar(page, seed);
      await askIntoTheBar(page, words);
      await expect(newestRun(page)).toHaveAttribute("data-state", "waiting", {
        timeout: RUN_STEPS_MS,
      });
      await expect(planCard(page)).toHaveAttribute("data-state", "waiting", {
        timeout: RUN_STEPS_MS,
      });
      await expect(message).toHaveText(reachesNoSession);
      await page.clock.setSystemTime(AT_4_40_PM);
      await searchBox(page).focus();
      await searchBox(page).press("Enter");
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      await expect(planCard(page).getByTestId("going-plan-title")).toHaveText(
        /^Applied .* · 4:40\sPM$/
      );
      // Undo names what Dispatch says it can take back: the two changes, never the message.
      await expect(planCard(page).getByTestId("going-undo-button")).toHaveText("Undo 2 changes");
      await expect(message).toHaveText(reachesNoSession);
      // The message reached no session, so its result says where it went (review a8), and names
      // the issue no second time: its chip stands in the line above (review N1).
      await expect(
        planCard(page)
          .locator('[data-testid="going-plan-row"][data-kind="message"]')
          .getByTestId("going-plan-row-state")
      ).toHaveText("Posted in the issue's conversation");
      const fold = newestRun(page).getByTestId("going-steps-fold");
      await expect(fold).toHaveAccessibleName(/^Worked for \d+ s · applied 3 changes$/);
      const finderChip = planCard(page)
        .getByTestId("going-plan-row")
        .filter({ hasText: "Align the finder scope to the mount" })
        .getByTestId("item-chip");
      await expect(finderChip).toContainText("Align the finder scope to the mount");
      await shoot(page, "07-applied");
      await planCard(page).getByTestId("going-undo-button").click();
      await expect(planCard(page).getByTestId("going-undo-line")).toHaveCount(3, {
        timeout: RUN_STEPS_MS,
      });
      await expect(planCard(page).getByTestId("going-plan-title")).toHaveText(/^Undone/);
      // The folded line is the run's work and stays as it was (review R3); the line under the
      // answer says what Dispatch's record says of the Undo: no change of the run stays, a
      // message sent being no change Undo takes back or leaves.
      await expect(fold).toHaveAccessibleName(/^Worked for \d+ s · applied 3 changes$/);
      await expect(newestRun(page).getByTestId("going-run-state")).toHaveText("Undone");
      await expect(finderChip).not.toContainText("to the mount");
      // Each row shows its change as the plan card did, plain "old → new" with no strike, and
      // its own status says it was reverted (review N3).
      await expect(planCard(page).getByTestId("going-plan-field").locator("del")).toHaveCount(0);
      await shoot(page, "07-undone");
    },
    shows: "a plan applied, and undone",
    state: "07",
  },
  {
    // State 8. Scripts two runs of one conversation, each reading one issue in a slow turn and
    // answering with it as a link: the question, then the follow-up typed in the box under the
    // first answer.
    reach: async (page, seed) => {
      const asked = "what is the primary mirror waiting on";
      const followUp = "and the telescope mount";
      const { mirror, mount } = seed.bar;
      await script(seed, [
        {
          turns: [slow(read(mirror)), say(`It waits on one issue, ${naming(mirror)}.`)],
          words: asked,
        },
        {
          turns: [slow(read(mount)), say(`It waits on one issue, ${naming(mount)}.`)],
          words: followUp,
        },
      ]);
      await openTheBar(page, seed);
      await askIntoTheBar(page, asked);
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      await searchBox(page).fill(followUp);
      await searchBox(page).press("Enter");
      await expect(page.getByTestId("going-run")).toHaveCount(2, { timeout: RUN_STEPS_MS });
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      await chipsRead(bar(page), 2);
      await expect(bar(page).getByTestId("going-steps-fold")).toHaveText([
        /^Worked for \d+ s · read 1 item›$/,
        /^Worked for \d+ s · read 1 item›$/,
      ]);
      await shoot(page, "08-follow-up");
    },
    shows: "a follow-up under the first answer",
    state: "08",
  },
  {
    // State 9. Three conversations at the Fast level, which the row chooses in the mode menu.
    // The first reads two issues, the first in a slow turn, and then writes what it found, which
    // the service cuts at the call's output tokens (`cut`), so the run stops at its reply length
    // limit with that text kept. It is the one bound that keeps what the agent found so far: a
    // run that spends its model calls or its time ends with no reply of its own, since a reply
    // in prose that calls no tool ends the turn done (going/agent's run.go, `saveReply`). So the
    // bound's card stands under the findings, as the design draws it, and the findings end with
    // "…", since the cut fell mid-sentence (review M3). The second reads one issue
    // in a slow turn and then streams without end, and the row stops it with Escape; the third
    // is answered 500 twice, the call and its one retry, so it fails. The three end in one card
    // with one next step - Continue at the bound and after the Stop, Try again after the failure
    // - and the steps of each run that stopped fold into one closed line, which counts what the
    // bound's card counts (design v3, rule 3: fold the work, never the result).
    reach: async (page, seed) => {
      const bounded = "what is the telescope work waiting on";
      const stopped = "keep reading the telescope mount";
      const failed = "what is the finder scope waiting on";
      const { craters, mount } = seed.bar;
      const found: ChatTurn = {
        cut: true,
        text: `One issue is ready to start, ${naming(mount)}. The next one waits on`,
      };
      await script(seed, [
        { turns: [slow(read(mount)), read(craters), found], words: bounded },
        { turns: [slow(read(mount)), endlessStream()], words: stopped },
        { turns: [refusedWith(500), refusedWith(500)], words: failed },
      ]);
      await openTheBar(page, seed);
      const level = bar(page).getByTestId("going-level-button");
      await level.focus();
      await level.press("Enter");
      // The menu opens on the mode that is on, Standard; Fast is the row above it.
      await page.keyboard.press("ArrowUp");
      await page.keyboard.press("Enter");
      await expect(level).toHaveText(/^Fast/);
      await askIntoTheBar(page, bounded);
      await expect(newestRun(page)).toHaveAttribute("data-state", "stopped_bound", {
        timeout: RUN_STEPS_MS,
      });
      await expect(newestRun(page).getByTestId("going-run-bound")).toBeVisible();
      await expect(newestRun(page).getByTestId("going-steps-fold")).toHaveAccessibleName(
        /^Worked for \d+ s · read 2 items$/
      );
      await expect(newestRun(page).getByTestId("going-steps-fold")).toHaveAttribute(
        "aria-expanded",
        "false"
      );
      await expect(newestRun(page).getByTestId("going-step")).toHaveCount(0);
      // What the agent found so far stands above the card, the issue it names as its chip; the
      // cut fell mid-sentence, so the text ends with "…".
      const findings = newestRun(page).getByTestId("going-run-answer");
      await expect(findings).toContainText("One issue is ready to start,");
      await expect(findings).toHaveText(/ The next one waits on…$/);
      await expect(findings).toHaveAttribute("data-cut", "true");
      await chipsRead(findings, 1);
      await expect(newestRun(page).getByTestId("going-run-state")).toHaveText(
        "Stopped at Fast's reply length limit · read 2 items so far"
      );
      await expect(newestRun(page).getByTestId("going-run-continue")).toHaveText(/^Continue/);
      await expect(newestRun(page).getByTestId("going-run-continue-deep")).toHaveText(
        "Continue in Deep"
      );
      await shoot(page, "09-bound");
      await newConversation(page);
      await askIntoTheBar(page, stopped);
      await expect(newestRun(page).getByTestId("going-step").first()).toHaveAttribute(
        "data-running",
        "false",
        { timeout: RUN_STEPS_MS }
      );
      await searchBox(page).press("Escape");
      await expect(newestRun(page)).toHaveAttribute("data-state", "stopped_person", {
        timeout: RUN_STEPS_MS,
      });
      await expect(newestRun(page).getByTestId("going-run-state")).toHaveText(
        "Stopped · read 1 item"
      );
      await expect(newestRun(page).getByTestId("going-run-continue")).toHaveText(/^Continue/);
      await expect(newestRun(page).getByTestId("going-run-action")).toHaveCount(0);
      await expect(newestRun(page).getByTestId("going-steps-fold")).toHaveAccessibleName(
        /^Worked for \d+ s · read 1 item$/
      );
      await shoot(page, "09-stopped");
      await newConversation(page);
      await askIntoTheBar(page, failed);
      await expect(newestRun(page)).toHaveAttribute("data-state", "failed", {
        timeout: RUN_STEPS_MS,
      });
      await expect(newestRun(page).getByTestId("going-run-state")).toContainText(
        "Dispatch could not reach the model."
      );
      await expect(newestRun(page).getByTestId("going-run-action")).toHaveText(/^Try again/u);
      await shoot(page, "09-failed");
    },
    shows: "a stop at a bound, a Stop, and a failure",
    state: "09",
  },
  {
    // State 10. Scripts a run that reads one issue in a slow turn and answers with it as a link.
    // Enter on the line Ask hands the run to the panel at the side, which takes the focus in its
    // box, and the row shoots it docked. The panel's header moves it back with an icon whose words
    // are its tooltip, its box holds the context chip, and a second icon widens it to the content
    // area, with the navigation still beside it (A6): the second shot, which docks it again.
    reach: async (page, seed) => {
      const words = "what does the observatory dome need";
      const { dome } = seed.bar;
      await script(seed, [
        {
          turns: [
            slow(read(dome)),
            say(`The dome turns by hand and needs a motor, ${naming(dome)}.`),
          ],
          words,
        },
      ]);
      await openTheBar(page, seed);
      await askTheAgent(page, words);
      await expectHandedToThePanel(page);
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      await chipsRead(sidePanel(page).getByTestId("going-run-answer"), 1);
      await expect(
        sidePanel(page).getByRole("button", { name: "Open over the page" })
      ).toHaveAttribute("title", "Open over the page");
      // The title is the question with a capital, as a title starts.
      await expect(sidePanel(page).getByTestId("going-title")).toHaveText(
        "What does the observatory dome need"
      );
      await shoot(page, "10-side-panel");
      await sidePanel(page).getByRole("button", { name: "Widen the panel" }).click();
      await expect(sidePanel(page)).toHaveAttribute("data-wide", "true");
      await chipsRead(sidePanel(page).getByTestId("going-run-answer"), 1);
      await shoot(page, "10-side-panel-wide");
      await sidePanel(page).getByRole("button", { name: "Dock at the side" }).click();
      await expect(sidePanel(page)).toHaveAttribute("data-wide", "false");
    },
    shows: "the side panel, docked and widened",
    state: "10",
  },
  {
    // State 11. Scripts two conversations: an answer in prose, then a run that reads one issue
    // and streams without end. The row closes the bar while the second works, so the chip names
    // it; it opens it again from the chip, stops it, and opens History, which lists both.
    reach: async (page, seed) => {
      const answered = "summarise the eyepiece work";
      const working = "keep watching the eyepiece";
      await script(seed, [
        { turns: [say("The eyepiece waits for its polish.")], words: answered },
        { turns: [read(seed.bar.eyepiece), endlessStream()], words: working },
      ]);
      await openTheBar(page, seed);
      await askIntoTheBar(page, answered);
      await expect(newestRun(page)).toHaveAttribute("data-state", "done", {
        timeout: RUN_STEPS_MS,
      });
      await newConversation(page);
      await askIntoTheBar(page, working);
      await expect(newestRun(page).getByTestId("going-step").first()).toHaveAttribute(
        "data-running",
        "false",
        { timeout: RUN_STEPS_MS }
      );
      // The page's title stands where it stood while the bar was open and no chip showed: the
      // chip floats over the page and moves none of it (design v3, state 11).
      const title = page.locator("#main-content h1").first();
      const titleWithoutChip = await title.boundingBox();
      await bar(page).getByTestId("going-close").click();
      await expect(bar(page)).toHaveCount(0);
      const chip = page.getByTestId("going-chip");
      await expect(chip).toHaveAttribute("data-state", "working");
      await expect(chip.getByTestId("going-chip-text")).toHaveText(
        "Working · Keep watching the eyepiece"
      );
      expect(await title.boundingBox(), "the chip moved the page's title").toEqual(
        titleWithoutChip
      );
      await shoot(page, "11-chip");
      // The chip sits clear of the page's title at every width (design v3, state 11): it floats
      // over the top of the page, 24 px tall, and ends above where the title starts, so the two
      // never meet, from the wide layout down to a phone's.
      const shotSize = page.viewportSize();
      for (const width of [1440, 1024, 768, 390]) {
        await page.setViewportSize({ height: 900, width });
        await expect(chip).toBeVisible();
        await expect(title).toBeVisible();
        const chipBox = await chip.boundingBox();
        // The chip is a target a pointer and a finger can press: 24 px tall (review R4).
        expect(chipBox?.height ?? 0, `the chip's height at ${width} px`).toBeGreaterThanOrEqual(24);
        expect(
          boxesMeet(chipBox, await title.boundingBox()),
          `the chip and the title meet at ${width} px`
        ).toBe(false);
      }
      if (shotSize !== null) await page.setViewportSize(shotSize);
      await chip.getByTestId("going-chip-open").click();
      await expect(bar(page)).toBeVisible();
      await searchBox(page).press("Escape");
      await expect(newestRun(page)).toHaveAttribute("data-state", "stopped_person", {
        timeout: RUN_STEPS_MS,
      });
      await bar(page).getByTestId("going-history-button").click();
      // History is the one box on the screen, under a header that names it, its first row
      // selected (design v3, state 11).
      await expect(bar(page).getByTestId("going-history-heading")).toHaveText("History");
      await expect(bar(page).getByRole("textbox", { name: "Ask the command bar" })).toBeHidden();
      const listed = bar(page).getByTestId("going-history-conversation");
      await expect(listed).toHaveCount(2);
      await expect(listed.first()).toHaveAttribute("data-state", "stopped_person");
      await expect(listed.last()).toHaveAttribute("data-state", "done");
      await expect(listed.first().getByTestId("going-history-open")).toHaveAttribute(
        "data-selected",
        "true"
      );
      await expect(bar(page).getByRole("searchbox", { name: "Filter history" })).toHaveAttribute(
        "placeholder",
        "Filter history…"
      );
      await shoot(page, "11-history");
    },
    shows: "the chip, and History",
    state: "11",
  },
  {
    // State 12. No run: the mode menu, opened from its button with Enter.
    reach: async (page, seed) => {
      await script(seed, []);
      await openTheBar(page, seed);
      const level = bar(page).getByTestId("going-level-button");
      await level.focus();
      await level.press("Enter");
      const menu = page.getByRole("menu", { name: "Mode" });
      await expect(menu).toBeVisible();
      await expect(menu.getByRole("menuitemradio", { checked: true })).toBeFocused();
      await shoot(page, "12-mode-menu");
      await page.keyboard.press("Escape");
      await expect(menu).toHaveCount(0);
    },
    shows: "the mode menu",
    state: "12",
  },
  {
    // State 13. Scripts the decision `dispatch_issue`, so the line on top is File as a new issue,
    // and the new-issue run Enter on it starts: one create of an issue of OPS, which the server
    // files at once. Before Enter the row waits for the line on top to settle on File and for the
    // pause's search to answer, never for the short answer, which a filing line does not draw.
    // The bar closes, and the notice names the issue it filed, by its key and its title. Its
    // words share none with another row's search, which would list the issue were that row to
    // run after this one.
    reach: async (page, seed) => {
      const words = "we need a weather station on the roof";
      const { project } = seed.bar;
      await script(
        seed,
        [
          {
            afterwards: say("Filed."),
            turns: [
              proposePlan(`File a weather station on the roof in ${project}.`, [
                {
                  arguments: {
                    project,
                    spec: "We need a weather station on the roof.\n",
                    title: "A weather station on the roof",
                  },
                  tool: "dispatch_issue",
                },
              ]),
            ],
            words,
          },
        ],
        "dispatch_issue"
      );
      await openTheBar(page, seed);
      // The pause's whole search, meaning as well as words, whose answer may re-rank the hits the
      // keystrokes listed: the shot waits for it, as it waits for the line on top to settle.
      const pauseSearched = page.waitForResponse(
        (response) => {
          const url = new URL(response.url());
          return (
            /^\/api\/v1\/search(?:\/results)?$/u.test(url.pathname) &&
            url.searchParams.get("q") === words &&
            url.searchParams.get("keywords_only") !== "true"
          );
        },
        { timeout: RUN_STEPS_MS }
      );
      await searchBox(page).fill(words);
      expect((await pauseSearched).ok()).toBe(true);
      await expect(lineOnTop(page)).toContainText(`File as a new issue in ${project}`, {
        timeout: RUN_STEPS_MS,
      });
      await expect(bar(page).getByTestId("search-line-deciding")).toHaveCount(0);
      await expect(bar(page).getByText("Searching…")).toHaveCount(0);
      // A filing line draws no short answer: Enter files the words, and answers nothing.
      await expect(answerPanel(page)).toHaveCount(0);
      // The hit under the line is a document of an issue: its snippet's line stops before the
      // issue's status at the row's end (review R4).
      const hit = bar(page)
        .getByRole("option")
        .filter({ has: page.getByTestId("search-hit-snippet") })
        .first();
      const snippetLine = hit.getByTestId("search-hit-snippet");
      const statusMark = hit.getByTestId("search-hit-status");
      await expect(snippetLine).toBeVisible();
      await expect(statusMark).toBeVisible();
      // The wait above drew the hit's snippet, and a drawn element has a box, so the box is there.
      const snippet = await snippetLine.boundingBox();
      // The wait above drew the hit's status as well, so its box is there as the snippet's is.
      const status = await statusMark.boundingBox();
      if (snippet === null || status === null) {
        throw new Error("the hit's snippet or its status is not drawn");
      }
      expect(snippet.x + snippet.width, "where the snippet's line ends").toBeLessThanOrEqual(
        status.x
      );
      await shoot(page, "13-filing-line");
      await searchBox(page).press("Enter");
      const notice = page.getByTestId("going-notice");
      await expect(notice.getByTestId("going-notice-text")).toHaveText(
        new RegExp(`^Filed ${project}-\\d+: A weather station on the roof$`),
        { timeout: RUN_STEPS_MS }
      );
      await expect(bar(page)).toHaveCount(0);
      await shoot(page, "13-filed");
    },
    shows: "File as a new issue, and the notice of the filing",
    state: "13",
  },
  {
    // State 14. The Settings page's command-bar section just after a choice of approval mode,
    // which saves in the press and says "Saved" beside the mode for 3 s: Auto-review is chosen
    // and the clock held while the mark shows, so the shot is of that moment on every run. No
    // run and no model: the page is the state.
    reach: async (page, seed) => {
      await script(seed, []);
      await page.goto("/settings");
      await expect(page.getByRole("heading", { level: 1, name: "Settings" })).toBeVisible();
      const heading = page.getByRole("heading", { name: "Command bar" });
      await heading.evaluate((element) => element.scrollIntoView({ block: "start" }));
      await page.getByRole("radio", { name: "Auto-review" }).check();
      const saved = page.getByTestId("going-settings-saved-mark");
      await expect(saved).toHaveText("✓ Saved");
      await expect(page.getByTestId("going-settings-saved")).toHaveText("Saved: Auto-review.");
      // Held a second on, well inside the mark's 3 s, so it is on the screen for the shot.
      await page.clock.pauseAt(new Date(Date.now() + 1000));
      await expect(saved).toBeVisible();
      await shoot(page, "14-settings-saved");
      await page.clock.resume();
    },
    shows: "the settings page just after a choice of approval mode, saying Saved",
    state: "14",
  },
];

/** One row's work: the skips, a login of its own, the page's clock, and the state's shots. */
async function drawState(browser: Browser, row: BarRow): Promise<void> {
  if (seedPath === undefined) {
    test.skip(true, SKIP_REASON);
    return;
  }
  modelsLane();
  const seed = readVisualSeed(seedPath);
  const context = await asUser(browser, `bar-${row.state}`);
  try {
    const page = await context.newPage();
    await page.clock.install({ time: row.fixedDay === true ? FIXED_DAY : Date.now() });
    await row.reach(page, seed);
  } finally {
    await context.close();
  }
}

// The rows once each, in their listed order, as `e2e/visual.e2e.ts` runs its own.
for (const row of barRows) {
  // SLOW(>=2s): a sign-in, a settled page, the bar driven through runs of the fake models, and
  // a screenshot comparison of each state the row reaches.
  test(`the command bar's state ${row.state}, ${row.shows}, matches its baselines`, async ({
    browser,
  }) => {
    test.setTimeout(150_000);
    await drawState(browser, row);
  });
}
