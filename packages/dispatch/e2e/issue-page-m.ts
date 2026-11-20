import { expect, type Locator, type Page, type Request } from "@playwright/test";

import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { gotoIssue, marginShell, openDetails } from "./issue-page";
import { focusRow, nodeRow, unfoldEveryBranch } from "./issue-page-g";
import { focusedNodeId, openEveryDeeperBranch } from "./issue-page-i";
import { settledState } from "./settled";

/**
 * What the rows of `e2e/issue-page-m.e2e.ts` read of Details on the new issue page: the
 * history, the steps under it, the facts of the focused row, and the way to them.
 *
 * This file is those rows' own: it imports the readers of `e2e/issue-page.ts`,
 * `e2e/issue-page-g.ts` and `e2e/issue-page-i.ts`, and `settledState` of `e2e/settled.ts`, and
 * copies none of them.
 *
 * The DOM a row reads, all of it inside Details (`[data-testid="desktop-margin-shell"]`):
 *
 * - **The history** `[data-testid="issue-history"]`, a `section` named "History"
 *   (`IssueHistory.tsx`). Each day is `[data-history-day="<YYYY-MM-DD>"]`, a day before today a
 *   `button` "<day> · <count>" with `aria-expanded` ("<day>" alone for the oldest day while older
 *   pages of the log are left, whose count is not known yet); each entry carries
 *   `data-history-entry` and `data-event-seq` (0 for a thread whose first turn's event is on a
 *   page not read yet); a turn of an agents' thread is `[data-history-item="<id>"]`, its Reply
 *   `[data-history-reply]` and its time the link `[data-history-time]`; an upload run is
 *   `[data-history-uploads="<n>"]`.
 * - **The facts** `[data-testid="focused-node-facts"]`, carrying `data-focused-node` with the
 *   focused node's id, or the issue's key with no node focused (`FocusedNodeFacts.tsx`); its
 *   deliveries are `[data-deliveries] li`, its controls `[data-focused-controls]`, and what
 *   references the row is `[data-node-references]` (`node-references/NodeReferences.tsx`).
 * - **The people** in the People section: "Subscribers" and the row
 *   `[data-details-row="collaborators"]` (`details-more.tsx`).
 * - **The steps** `[data-testid="issue-steps"]`, a `section` named "Steps" under the history
 *   (`DetailsSteps.tsx`): its heading "Steps (<runs>)", each task a `section` "Task <task>" with
 *   its `h3`, each run `[data-step-run="<record id>"]`, and each of a run's labelled values
 *   `[data-step-field="<field>"]`, its label the `dt` and its value the `dd`.
 */

/** The issue's history, in Details. */
export function historyPanel(page: Page): Locator {
  return page.getByTestId("issue-history");
}

/** One turn of an agents' thread in the history, by the comment's or the message's id. */
export function historyItem(page: Page, id: string): Locator {
  return historyPanel(page).locator(`[data-history-item="${id}"]`);
}

/** Every entry of the history, newest first. */
export function historyEntries(page: Page): Locator {
  return historyPanel(page).locator("[data-event-seq]");
}

/**
 * The day the history files a time under, worked out in the page from the browser's own clock as
 * the page works it out at each render: the local date (`dateKey` of
 * `features/activity/activity-model.ts`), which `data-history-day` carries, and the name the
 * day's line reads, "Today", "Yesterday" or the date written out (`buildHistory`, `dayLabel`).
 * The server stamps each seed with its own time, so a seed made before the reader's midnight and
 * read after it is a day before the reader's today: a row names a day by this, never by "Today"
 * or "Yesterday" written out.
 */
export interface HistoryDayRead {
  readonly date: string;
  readonly label: string;
  readonly today: boolean;
}

/** The day the history files the time `at` under, and its name (`HistoryDayRead`). */
export function historyDayOf(page: Page, at: string): Promise<HistoryDayRead> {
  return page.evaluate(
    ({ time, words }) => {
      const key = (moment: Date) =>
        `${moment.getFullYear()}-${String(moment.getMonth() + 1).padStart(2, "0")}-${String(moment.getDate()).padStart(2, "0")}`;
      const today = key(new Date());
      const date = key(new Date(time));
      // Yesterday is read back from today's noon, as the page reads it.
      const noon = new Date(`${today}T12:00:00`);
      noon.setDate(noon.getDate() - 1);
      const [year, month, day] = date.split("-").map(Number);
      const written = new Intl.DateTimeFormat(undefined, {
        day: "numeric",
        month: "long",
        weekday: "long",
        ...(date.slice(0, 4) === today.slice(0, 4) ? {} : { year: "numeric" }),
      }).format(new Date(year ?? 1970, (month ?? 1) - 1, day ?? 1));
      const label = date === today ? words.today : date === key(noon) ? words.yesterday : written;
      return { date, label, today: date === today };
    },
    {
      time: at,
      words: { today: CONTROL_LABELS.history_today, yesterday: CONTROL_LABELS.history_yesterday },
    }
  );
}

/**
 * Waits until the history draws `entry`, the entry of a seed made at `at`, found by its id
 * (`historyItem`, `[data-event-seq]`, ...). The history draws every day before the reader's
 * today folded to one line, and the page reads its today from the browser's clock at each render,
 * so a seed made before the reader's midnight and drawn after it is in a folded day: where the
 * page drew the seed's own day folded, that day is opened first. So no row assumes which day a
 * seed falls in.
 */
export async function seededHistoryEntry(page: Page, entry: Locator, at: string): Promise<Locator> {
  const { date } = await historyDayOf(page, at);
  return drawnInItsDay(
    entry,
    historyPanel(page).locator(`[data-history-day="${date}"]`),
    `the history's entry ${entry.toString()}, of ${date}`
  );
}

/** Waits until the history draws its newest entry, whatever seed it is: the first entry of its
 *  first day, that day opened first where the page drew it folded (`seededHistoryEntry`). */
export function newestHistoryEntry(page: Page): Promise<Locator> {
  const day = historyPanel(page).locator("[data-history-day]").first();
  return drawnInItsDay(day.locator("[data-event-seq]").first(), day, "the history's newest entry");
}

/** `entry` drawn, its `day` opened first where the page drew that day folded; `what` names the
 *  entry for the failure. */
async function drawnInItsDay(entry: Locator, day: Locator, what: string): Promise<Locator> {
  const line = day.locator(':scope > button[aria-expanded="false"]');
  const shown = await settledState(what, [
    ["drawn", entry],
    ["folded", line],
  ]);
  // The day is opened for the row's own reading, so its line is pressed without taking the
  // focus: a row that reads where the reader's focus is finds it where the row left it.
  if (shown === "folded") await line.dispatchEvent("click");
  await expect(entry, `${what} is not drawn`).toBeVisible();
  return entry;
}

/** The facts of the row the reader's focus is on, in Details. */
export function focusedNodeFacts(page: Page): Locator {
  return marginShell(page).getByTestId("focused-node-facts");
}

/**
 * Opens the issue, puts the focus on node `id`'s row as a reader's press does, and opens Details
 * at its facts. An item's own address (`/asks/<id>`, `/comments/<id>`) does not put the focus on
 * its node yet, so a row reaches the node through the outline instead
 * (`focusedRowFacts`).
 */
export async function rowFacts(page: Page, issueKey: string, id: string): Promise<Locator> {
  await gotoIssue(page, `/issues/${issueKey}`);
  return focusedRowFacts(page, id);
}

/**
 * On a page already on the issue, puts the focus on node `id`'s row and opens Details at its
 * facts. The comments, messages and questions arrive after the tree (`useIssueTree.ts`), and a
 * node that arrives late may arrive folded, so every branch the outline folds is opened again
 * until the row is drawn, as `expectSeedDrawn` does.
 */
export async function focusedRowFacts(page: Page, id: string): Promise<Locator> {
  await expect
    .poll(async () => {
      await unfoldEveryBranch(page);
      await openEveryDeeperBranch(page);
      return nodeRow(page, id).count();
    }, `${id} is never drawn as a row of the outline`)
    .toBeGreaterThan(0);
  await focusRow(page, id);
  await expect.poll(() => focusedNodeId(page), `${id} did not take the focus`).toBe(id);
  await openDetails(page);
  const facts = focusedNodeFacts(page);
  await expect(facts).toHaveAttribute("data-focused-node", id);
  return facts;
}

/** The facts of the ask `askId`, focused through the outline (`rowFacts`): its hand-on control
 *  and who it is asked of are there. */
export function askFacts(page: Page, issueKey: string, askId: string): Promise<Locator> {
  return rowFacts(page, issueKey, askId);
}

/** The runs of the issue's steps, under the history in Details. */
export function stepsSection(page: Page): Locator {
  return marginShell(page).getByTestId("issue-steps");
}

/** One run of the steps, by its record's id. */
export function stepRun(page: Page, id: string): Locator {
  return stepsSection(page).locator(`[data-step-run="${id}"]`);
}

/** The page's reads of one issue's steps, as `watchStepsReads` sees them, counted from 0 in the
 *  order the page sent them since the watch began. */
export interface StepsReads {
  /** Whether read `index` has its answer. */
  answered(index: number): boolean;
  /** Whether no read is out and none has been sent or answered for the last `ms`. */
  quietFor(ms: number): boolean;
  /** How many reads the page has sent. */
  sent(): number;
}

/** Watches the page's reads of issue `key`'s steps (`GET /api/v1/issues/<key>/steps`) from the
 *  call on, so a row calls it before the page could send one. A read the network failed counts
 *  as answered, as one with a status does: either way it is no longer out. */
export function watchStepsReads(page: Page, key: string): StepsReads {
  const path = `/api/v1/issues/${key}/steps`;
  const reads: { answeredAt: number | undefined; readonly request: Request }[] = [];
  page.on("request", (request) => {
    if (request.method() === "GET" && new URL(request.url()).pathname === path) {
      reads.push({ answeredAt: undefined, request });
    }
  });
  const answer = (request: Request) => {
    const read = reads.find((each) => each.request === request);
    if (read !== undefined) read.answeredAt = Date.now();
  };
  page.on("requestfinished", answer);
  page.on("requestfailed", answer);
  return {
    answered: (index) => reads[index]?.answeredAt !== undefined,
    quietFor: (ms) =>
      reads.every((read) => read.answeredAt !== undefined && Date.now() - read.answeredAt >= ms),
    sent: () => reads.length,
  };
}

/** What the steps draw of the instant `at`, worked out in the page from the browser's own locale,
 *  zone and clock, as the Steps tab draws a run's start and end: the date, with its year where it
 *  is not this one, and the time of day. */
export function drawnInstant(page: Page, at: string): Promise<string> {
  return page.evaluate((iso) => {
    const date = new Date(iso);
    const day = new Intl.DateTimeFormat(undefined, {
      day: "numeric",
      month: "short",
      ...(date.getFullYear() === new Date().getFullYear() ? {} : { year: "numeric" }),
    });
    const time = new Intl.DateTimeFormat(undefined, { hour: "numeric", minute: "2-digit" });
    return `${day.format(date)} ${time.format(date)}`;
  }, at);
}

/**
 * What keeps a value of the steps from being read whole, one line each, or none: Details' box,
 * the nearest box above the section that scrolls it, scrolling sideways; and each run's value
 * wider than its own box, standing out past a side of Details' box, or cut by an ellipsis or a
 * line clamp on it or inside it.
 */
export function stepsUnread(page: Page): Promise<string[]> {
  return stepsSection(page).evaluate((section) => {
    let box: HTMLElement | null = section.parentElement;
    while (box !== null && !["auto", "scroll"].includes(getComputedStyle(box).overflowY)) {
      box = box.parentElement;
    }
    if (box === null) return ["the steps are in no box that scrolls"];
    const found: string[] = [];
    if (box.scrollWidth > box.clientWidth) {
      found.push(`Details scrolls sideways: ${box.scrollWidth} px in ${box.clientWidth} px`);
    }
    const sides = box.getBoundingClientRect();
    for (const value of section.querySelectorAll<HTMLElement>("[data-step-field] > dd")) {
      const field = value.parentElement?.getAttribute("data-step-field");
      const run = value.closest("[data-step-run]")?.getAttribute("data-step-run");
      const what = `${field} of ${run} ("${value.textContent}")`;
      if (value.scrollWidth > value.clientWidth) {
        found.push(`${what} is ${value.scrollWidth} px in ${value.clientWidth} px`);
      }
      const at = value.getBoundingClientRect();
      if (at.left < sides.left - 1 || at.right > sides.right + 1) {
        found.push(
          `${what} stands out of Details, ${at.left}-${at.right} in ${sides.left}-${sides.right}`
        );
      }
      for (const part of [value, ...value.querySelectorAll<HTMLElement>("*")]) {
        const style = getComputedStyle(part);
        if (style.textOverflow === "ellipsis" || style.webkitLineClamp !== "none") {
          found.push(`${what} is cut by ${part.tagName}'s ellipsis or line clamp`);
        }
      }
    }
    return found;
  });
}
