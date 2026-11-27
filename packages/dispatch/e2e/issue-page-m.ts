import type { PlanRecordInput, PlanTaskInput, StepExperimentInput } from "@legion/contracts";
import { expect, type Locator, type Page, type Request, type Response } from "@playwright/test";

import { routeUrl } from "../web/src/api/paths";
import type { GraphReferences } from "../web/src/api/types";
import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { PAGE_SIZE } from "../web/src/features/issue-page/show-more-fixture";
import { issueProject } from "../web/src/lib/issue-key";
import { baseUrl, e2eAgentToken } from "./api";
import { gotoIssue, marginShell, openDetails } from "./issue-page";
import { focusRow, nodeRow, unfoldEveryBranch } from "./issue-page-g";
import { focusedNodeId, openEveryDeeperBranch } from "./issue-page-i";
import { settledState } from "./settled";

/**
 * What the rows of `e2e/issue-page-m.e2e.ts` read of Details on the new issue page: the
 * history, the steps under it, the facts of the focused row, and the way to them;
 * `putStepExperiment` and `putPlan`, the two writes of their seeds that `e2e/api.ts` has no
 * wrapper for, and `referencesTo`, the read of what references a node that a seed waits on;
 * and `daemonOf`, the Legion daemon of a project as their seed's writes name it.
 *
 * This file is those rows' own, and the reading list's drivers of Details' history
 * (`e2e/reading-conversation.e2e.ts`) take its `historyItem`, `historyPanel` and `daemonOf`: it
 * imports the readers of `e2e/issue-page.ts`, `e2e/issue-page-g.ts` and `e2e/issue-page-i.ts`,
 * `settledState` of `e2e/settled.ts`, and `baseUrl` and `e2eAgentToken` of `e2e/api.ts`, and
 * copies none of them.
 *
 * The DOM a row reads, all of it inside Details (`[data-testid="desktop-margin-shell"]`):
 *
 * - **The history** `[data-testid="issue-history"]`, a `section` named "History"
 *   (`IssueHistory.tsx`). Each day is `[data-history-day="<YYYY-MM-DD>"]`, drawn open under its
 *   line, a `p` that names the day; each entry carries `data-history-entry` and `data-event-seq`
 *   (0 for a thread whose first turn's event is on a page not read yet); a turn of an agents'
 *   thread is `[data-history-item="<id>"]`, its Reply `[data-history-reply]` and its time the link
 *   `[data-history-time]`; an upload run is `[data-history-uploads="<n>"]`. Under the last entry
 *   drawn stands one control, Show more, "Show more (<n> left)" while entries read are left to
 *   draw, its count `[data-history-left]` and "<n>+" while an older page of the log is unread; a
 *   press that would leave no entry read and not drawn reads that page first, the control reading
 *   "Loading older" meanwhile, and the control goes once the whole log is drawn.
 * - **The facts** `[data-testid="focused-node-facts"]`, carrying `data-focused-node` with the
 *   focused node's id, or the issue's key with no node focused (`FocusedNodeFacts.tsx`); its
 *   deliveries are `[data-deliveries] li`, its controls `[data-focused-controls]`, and what
 *   references the row is `[data-node-references]` (`node-references/NodeReferences.tsx`).
 * - **The people** in the People section: "Subscribers" and the row
 *   `[data-details-row="collaborators"]` (`details-more.tsx`).
 * - **The steps** `[data-testid="issue-steps"]`, a `section` named "Steps" under the history
 *   (`DetailsSteps.tsx`): its heading "Steps (<runs>)", under it the line `[data-step-variant]`
 *   where the newest run names an experiment, each task a `section` "Task <task>"
 *   `[data-step-task="<task>"]` with its `h3`, then the line of the tasks it needs
 *   `[data-step-needs]`, the list of the steps the plan gives it `[data-step-planned]`, and its
 *   runs, or the line `[data-step-no-run]` where none of its steps has run; each run
 *   `[data-step-run="<record id>"]`, and each of a run's labelled values
 *   `[data-step-field="<field>"]`, its label the `dt` and its value the `dd`.
 */

/** The issue's history, in Details. */
export function historyPanel(page: Page): Locator {
  return page.getByTestId("issue-history");
}

/** The history's heading, "History", in Details. */
export function historyHeading(page: Page): Locator {
  return historyPanel(page).getByRole("heading", { exact: true, name: CONTROL_LABELS.history });
}

/** Escapes `words` for a regular expression, so the page's words match as written. */
function literally(words: string): string {
  return words.replace(/[.*+?^${}()|[\]\\]/gu, "\\$&");
}

/** Show more's name, with or without its count of the entries left: "Show more (25 left)",
 *  "Show more (3+ left)" or "Show more". */
const SHOW_MORE = new RegExp(
  `^${literally(CONTROL_LABELS.show_more)}(?: \\(\\d+\\+? ${literally(CONTROL_LABELS.show_more_left)}\\))?$`,
  "u"
);

/** The history's Show more, the one control under its last entry drawn, whatever its count says:
 *  it draws the next entries, reading the log's next page first where it holds too few. */
export function historyShowMore(page: Page): Locator {
  return historyPanel(page).getByRole("button", { name: SHOW_MORE });
}

/** Show more's name where its press reads the log's next page before it draws: "Show more
 *  (<n>+ left)" with `n` from 1 to `PAGE_SIZE`, so fewer entries are read and not drawn than a
 *  press draws. */
const READS_FIRST = new RegExp(
  `^${literally(CONTROL_LABELS.show_more)} \\((?:${Array.from({ length: PAGE_SIZE }, (_, index) => index + 1).join("|")})\\+ ${literally(CONTROL_LABELS.show_more_left)}\\)$`,
  "u"
);

/** The history's Show more where its press reads the log's next page before it draws the next
 *  entries: it says "<n>+" with `n` at most `PAGE_SIZE`. */
export function historyNextPage(page: Page): Locator {
  return historyPanel(page).getByRole("button", { name: READS_FIRST });
}

/** What the history's Show more says, read once the page shows one of them: `reads` where its
 *  press reads the log's next page before it draws (`historyNextPage`), `counted` where it says
 *  how many entries read are left and its press draws the next ones, `uncounted` where it says no
 *  number, `reading` while it reads, and `gone` where the history has drawn entries and no control
 *  stands under them. */
export function historyShowMoreState(
  page: Page
): Promise<"counted" | "gone" | "reading" | "reads" | "uncounted"> {
  const busy = 'button[aria-busy="true"]';
  return settledState("the history's Show more", [
    ["reads", historyNextPage(page)],
    ["counted", historyShowMore(page).filter({ has: page.locator("[data-history-left]") })],
    [
      "uncounted",
      historyPanel(page).getByRole("button", { exact: true, name: CONTROL_LABELS.show_more }),
    ],
    ["reading", historyPanel(page).locator(busy)],
    [
      "gone",
      historyPanel(page)
        .filter({ has: page.locator("[data-event-seq]") })
        .filter({ hasNot: page.getByRole("button", { name: SHOW_MORE }) })
        .filter({ hasNot: page.locator(busy) }),
    ],
  ]);
}

/** Whether `url` is a read of a page of issue `key`'s log older than the first (`GET
 *  /api/v1/issues/<key>/events` with `before`), the read the history sends for entries it holds
 *  too few of. */
export function isOlderPageRead(url: URL, key: string): boolean {
  return url.pathname === `/api/v1/issues/${key}/events` && url.searchParams.has("before");
}

/** Whether `url` is the read mark's read of issue `key`'s log (`IssueReadMark.tsx`, part L, on
 *  `main`): the issue's newest event, `limit=1&order=desc` and nothing else, which the new page
 *  sends to mark the issue read once the reader reaches the outline's end. It is no read of
 *  Details, so a row that counts Details' reads leaves it out. */
export function isReadMarkRead(url: URL, key: string): boolean {
  return (
    url.pathname === `/api/v1/issues/${key}/events` &&
    [...url.searchParams.keys()].sort().join(",") === "limit,order" &&
    url.searchParams.get("limit") === "1" &&
    url.searchParams.get("order") === "desc"
  );
}

/** The answer to the history's read of a page of issue `key`'s log older than the first, the
 *  read the history sends before it draws where it holds too few entries, and the one Show more
 *  sends where its press reads the next page first (`historyNextPage`), waited for from the call
 *  on: a row calls it before what sends that read. */
export function olderPageAnswered(page: Page, key: string): Promise<Response> {
  return page.waitForResponse((response) => isOlderPageRead(new URL(response.url()), key));
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
 * (`historyItem`, `[data-event-seq]`, ...), in the day the page files `at` under
 * (`historyDayOf`). Every day is drawn open, and the page reads its today from the browser's
 * clock at each render, so a seed made before the reader's midnight and drawn after it stands in
 * a day before today: no row assumes which day a seed falls in.
 */
export async function seededHistoryEntry(page: Page, entry: Locator, at: string): Promise<Locator> {
  const { date } = await historyDayOf(page, at);
  const what = `the history's entry ${entry.toString()}, of ${date}`;
  await expect(entry, `${what} is not drawn`).toBeVisible();
  await expect(
    entry.locator("xpath=ancestor::li[@data-history-day][1]"),
    `${what} is drawn in another day`
  ).toHaveAttribute("data-history-day", date);
  return entry;
}

/** Waits until the history draws its newest entry, whatever seed it is: the first entry of its
 *  first day. */
export async function newestHistoryEntry(page: Page): Promise<Locator> {
  const entry = historyEntries(page).first();
  await expect(entry, "the history's newest entry is not drawn").toBeVisible();
  return entry;
}

/** The facts of the row the reader's focus is on, in Details. */
export function focusedNodeFacts(page: Page): Locator {
  return marginShell(page).getByTestId("focused-node-facts");
}

/**
 * Opens Conversation, puts the focus on node `id`'s row as a reader's press does, and opens Details
 * at its facts. An item's own address (`/asks/<id>`, `/comments/<id>`) does not put the focus on
 * its node yet, so a row reaches the node through the outline instead
 * (`focusedRowFacts`).
 */
export async function rowFacts(page: Page, issueKey: string, id: string): Promise<Locator> {
  await gotoIssue(page, `/issues/${issueKey}/conversation`);
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

/** The facts of the ask `askId`, focused through the outline (`rowFacts`): its hand-on control,
 *  who it is asked of, its edit history, who its answer reaches and its asker's copy handles
 *  are there. */
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

/** The group of one task of the steps, by the task. */
export function stepTask(page: Page, task: string): Locator {
  return stepsSection(page).locator(`[data-step-task="${task}"]`);
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

/** The Legion daemon of `project`, which writes on the project's issues as the session
 *  `legion-daemon:<PROJECT>`: a message it writes with no target begins a thread the history
 *  holds whole, whoever writes in it (`agents-only.ts`), and it is the one writer the steps'
 *  routes admit for that project's issues. */
export function daemonOf(project: string) {
  return {
    actor: { id: `legion-daemon:${project}`, kind: "session" as const },
    as: "agent" as const,
  };
}

/**
 * Sends `input` to the route `route` of issue `key` (`params` holding the rest of its path) as the
 * Legion daemon of the issue's project writes there: a PUT under the agent bearer, `input`
 * naming the session of `daemonOf`, the one writer the steps' routes admit for that project's
 * issues. It throws, naming `what`, the status and the body, where the answer is not ok.
 */
async function putAsDaemon(
  route: string,
  params: { readonly id: string; readonly key: string },
  input: object,
  what: string
): Promise<void> {
  const response = await fetch(new URL(routeUrl(route, params), baseUrl), {
    body: JSON.stringify(input),
    headers: { Authorization: `Bearer ${e2eAgentToken}`, "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!response.ok) {
    throw new Error(`${what} failed: ${response.status} ${await response.text()}`);
  }
}

/**
 * Writes the experiment and the variant of run `id` of issue `key`, as the Legion daemon of the
 * issue's project writes them after it has written the run
 * (`PUT /api/v1/issues/{key}/steps/{id}/experiment`, `putAsDaemon`).
 */
export async function putStepExperiment(
  key: string,
  id: string,
  drawn: Pick<StepExperimentInput, "experiment" | "variant">
): Promise<void> {
  const input: StepExperimentInput = {
    actor: daemonOf(issueProject(key)).actor,
    ...drawn,
  };
  await putAsDaemon(
    "/api/v1/issues/{key}/steps/{id}/experiment",
    { id, key },
    input,
    `write the experiment of run ${id} of ${key}`
  );
}

/**
 * Writes the plan the first run of issue `key` ran from, with its tasks `tasks` in their order,
 * as the Legion daemon of the issue's project writes it when the run's work starts
 * (`PUT /api/v1/issues/{key}/plans/{id}`, `putAsDaemon`): an owner's plan at version 1 of its
 * document. The read of the steps takes each task's steps and needs from the plan alone and opens
 * no document, so the plan names a document id no artifact holds, as the server's own tests of
 * that read do. A task written with no `needs` has them not recorded, as a daemon of the release
 * before writes it.
 */
export async function putPlan(key: string, tasks: PlanTaskInput[]): Promise<void> {
  const input: PlanRecordInput = {
    actor: daemonOf(issueProject(key)).actor,
    artifact_id: crypto.randomUUID(),
    generation: 1,
    source: "owner",
    tasks,
    version: 1,
  };
  await putAsDaemon(
    "/api/v1/issues/{key}/plans/{id}",
    { id: crypto.randomUUID(), key },
    input,
    `write the plan of ${key}`
  );
}

/**
 * What references `reference`, as the references read answers it under the agent bearer
 * (`GET /api/v1/references?to=`), which the list of what references a row reads. A document's
 * mention reaches it once the document's index has placed the mention in its block, after the
 * version that holds it is written, and the list reads it once, when the reader opens it, so a
 * seed waits on this for such a mention before the page loads. It throws, naming the reference,
 * the status and the body, where the answer is not ok.
 */
export async function referencesTo(reference: string): Promise<GraphReferences> {
  const response = await fetch(
    new URL(routeUrl("/api/v1/references", {}, { to: reference }), baseUrl),
    { headers: { Authorization: `Bearer ${e2eAgentToken}` } }
  );
  if (!response.ok) {
    throw new Error(
      `read what references ${reference} failed: ${response.status} ${await response.text()}`
    );
  }
  return (await response.json()) as GraphReferences;
}
