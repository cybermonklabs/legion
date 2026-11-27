/** What the rows of Dashboard on both pages read it by - today's page
 *  (`e2e/issue-dashboard-today.e2e.ts`) and the new one (`e2e/issue-page-dashboard.e2e.ts`): the
 *  words of its parts, which main the server is, the locators of what waits on the reader, and
 *  the reads R8 names for its first load, told apart from the shell's own. */

import {
  type DashboardSeed,
  type MainHolds,
  mainHolds,
  ROUNDS,
  type WaitingItem,
} from "./dashboard-seed";
import { expect, type Locator, test } from "./test";
import type { ApiRequest, ApiRequests } from "./traffic";

/** The eight steps of the track, in order (R3). */
export const STEPS = [
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

/** Each step of the track as it reads over the seed's rounds (`writeRounds`): its name, and the
 *  count of its rounds where it has any. */
export const STEP_ROWS: readonly RegExp[] = STEPS.map((step) => {
  const rounds = STEP_ROUNDS[step] ?? 0;
  return new RegExp(rounds === 0 ? `^${step}$` : `^${step}\\s*${rounds}$`);
});

/** The role words of each claim state (R7): any other state reads "in another state", and a
 *  `retired` claim is not listed. */
export const ROLE_WORDS: Readonly<Record<string, string>> = {
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

export const TILES = {
  children: "Children done",
  met: "Statements met",
  passed: "Checks passed",
  questions: "Questions waiting on others",
} as const;

/** Reads which main this run's server is once per worker, in a `beforeAll` of the file that calls
 *  it (R27), and answers what it read; a row that asks before the hook ran fails. */
export function mainOfThisRun(): () => MainHolds {
  let holds: MainHolds | undefined;
  test.beforeAll(async () => {
    holds = await mainHolds();
  });
  return () => {
    if (holds === undefined) throw new Error("the file's beforeAll read no main");
    return holds;
  };
}

/** `text` as a pattern that matches it literally. */
export function literal(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** What waits on the reader, inside `root`: today's tab panel, or the new page's root. */
export function waitingOf(root: Locator): Locator {
  return root.getByRole("region", { exact: true, name: "Waiting on you" });
}

export function itemsOf(waiting: Locator): Locator {
  return waiting.locator('[data-testid^="dashboard-item-"]');
}

export function testIdOf(item: WaitingItem): string {
  return `dashboard-item-${item.kind}-${item.id}`;
}

export function itemOf(waiting: Locator, item: WaitingItem): Locator {
  return waiting.getByTestId(testIdOf(item));
}

/** The test id of each item drawn, in the order drawn. */
export function drawnItems(waiting: Locator): Promise<string[]> {
  return itemsOf(waiting).evaluateAll((items) =>
    items.map((item) => item.getAttribute("data-testid") ?? "")
  );
}

export function countOf(waiting: Locator): Locator {
  return waiting.getByTestId("dashboard-waiting-count");
}

export function nextFive(waiting: Locator): Locator {
  return waiting.getByRole("button", { name: /^Show the next five/ });
}

/** Draws every item of the list: one press of the control for each five past the first. */
export async function drawEveryItem(waiting: Locator, seed: DashboardSeed): Promise<void> {
  await expect(countOf(waiting)).toHaveText(String(seed.items.length));
  for (let drawn = 5; drawn < seed.items.length; drawn += 5) await nextFive(waiting).click();
  await expect(itemsOf(waiting)).toHaveCount(seed.items.length);
}

/** Whether a read of Dashboard's first load is one R8 names on this main, for `seed`: the issue,
 *  its open asks, its handoffs, its Legion state, the reader's unread replies of the issue with
 *  no `limit`, the own route of each thread drawn, the decisions and the Changes' comments of
 *  the decisions document, the results and the coverage, the batched reference targets, and a
 *  picture's version with the summary. Where dispatch://INFRA-1253 is on main, its tree read. R8
 *  names one set of reads for both pages. */
export function namedByR8(seed: DashboardSeed, method: string, sent: URL): boolean {
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

/** A read's method and route, by which a page's read is matched to the shell's. */
function routeOf(sent: ApiRequest): string {
  return `${sent.method} ${sent.route}`;
}

/** The routes `shell`, a page of the same view with no Dashboard, read since its last `reset`. */
export function shellRoutesOf(shell: ApiRequests): ReadonlySet<string> {
  return new Set(shell.requests.map(routeOf));
}

/** The shell's own polls: the reads of no issue that `shell`, a page of the same view with no
 *  Dashboard, made as it loaded - the agents registry and the credential requests among them.
 *  They repeat on their own clock, on either page, at any time after: a window `shell` polled in
 *  is no window the other page polls in. A read of the issue is held to the windows a row reads. */
export function shellPollsOf(shell: ApiRequests): ReadonlySet<string> {
  return new Set(shell.requests.filter((sent) => !sent.route.includes("/issues/")).map(routeOf));
}

/** Each of `requests` whose route is neither one of `shellRoutes` nor one of `shellPolls`: what
 *  the page sent beside the shell. */
export function beyondShell(
  requests: readonly ApiRequest[],
  shellRoutes: ReadonlySet<string>,
  shellPolls: ReadonlySet<string>
): ApiRequest[] {
  return requests.filter(
    (sent) => !shellRoutes.has(routeOf(sent)) && !shellPolls.has(routeOf(sent))
  );
}
