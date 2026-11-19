import { GOING_RUN_HEADER } from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import type { IssueTreeAnswer } from "../web/src/api/client";
import { type Body, type Result, routeUrl } from "../web/src/api/paths";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  NODE_REFERENCES_WORDS,
  sentenceCase,
} from "../web/src/features/issue-page/page-words";
import { buildIssuePath } from "../web/src/features/refs/routes";
import { baseUrl, createComment, createIssue, createProject, userHeaders } from "./api";
import { marginSection } from "./issue-page-h";
import type { ApiRequests } from "./traffic";

/**
 * What the rows of `e2e/issue-page-z.e2e.ts` seed and read: a command bar run that changed an
 * issue, the run's row in Details' Referenced by, and the run's own page.
 *
 * This file is those rows' own: it imports `e2e/issue-page-h.ts` for Details' sections, its rows
 * open the page through `e2e/issue-page.ts`, and it copies none of their readers.
 *
 * The DOM a row reads:
 *
 * - **The control** of Details' References section (`[data-margin-section="references"]`), the
 *   `button` named `PROPERTY_LABELS.referenced_by`, and that label with the count of the rows
 *   the list shows once both its reads have answered (`IssueMargin.tsx`).
 * - **The list** it opens, a `section` named by the same words (`refs/ReferencedBy.tsx`): the
 *   rows of its own read of the issue's references are the `li`s of its `ul` that is not
 *   `[data-extra-references]`, and the rows of the reader's command bar runs, read from the
 *   outline's tree read, the `li`s of `[data-extra-references]` after them
 *   (`run-references/run-references.ts`). A tree read that failed is a line under the list,
 *   `[role="alert"]` beside its Retry (`details-error.tsx`).
 * - **The run's own page** at `buildRunPath(<id>)`, whose root is `[data-issue-page]`
 *   (`run-page/RunPage.tsx`), and in its place the app's not-found page for any reader but the
 *   asker, drawn into the shell's content element `[data-testid="main-content"]` (`app.tsx`,
 *   `shell/NotFoundPage.tsx`).
 */

/** The project every issue of the seed is in. */
const PROJECT = "CORE";

/** The question alice asks the command bar, written with no markdown character, so the run's
 *  row and its page draw it as typed. */
export const RUN_QUESTION = "Close this once the checks pass";

/** The comment the run writes on the issue it changes, with no markdown character, so the run's
 *  page draws it as its first line. */
export const RUN_COMMENT = "Labelled for the next release";

/** The word for what a run is: its row's link in Details and its page's heading. */
export const RUN_WORD = sentenceCase(NODE_REFERENCES_WORDS.kinds.interaction);

/** The labels the run's two changes of the issue leave on it, in turn. */
const FIRST_LABELS = ["release"];
const SECOND_LABELS = ["release", "checked"];

/** What `seedRunIssue` wrote. */
export interface RunSeed {
  /** The key of the issue whose comment cites the issue the run changed. */
  readonly citing: string;
  /** The key of the issue the run changed. */
  readonly key: string;
  /** That issue's page. */
  readonly path: string;
  /** The run's id. */
  readonly run: string;
  /** When the server says the run was asked (`created_at`). */
  readonly runAt: string;
}

/** Asks `words` of `project` as `login`, as the command bar does, and answers the run the server
 *  recorded. */
export async function runAs(
  login: string,
  words: string,
  project: string
): Promise<{ readonly created_at: string; readonly id: string }> {
  const input: Body<"/api/v1/going/requests", "post"> = { project, words };
  const response = await fetch(new URL("/api/v1/going/requests", baseUrl), {
    body: JSON.stringify(input),
    headers: { ...(await userHeaders(login)), "Content-Type": "application/json" },
    method: "POST",
  });
  if (response.status !== 201) {
    throw new Error(
      `POST /api/v1/going/requests as ${login}: ${response.status} ${await response.text()}`
    );
  }
  const run = (await response.json()) as Result<"/api/v1/going/requests", "post">;
  return { created_at: run.created_at, id: run.id };
}

/** One write as `login` inside the run `run`: it carries `GOING_RUN_HEADER`, and anything but a
 *  2xx answer throws. */
async function inRun<T>(
  run: string,
  login: string,
  method: string,
  path: string,
  body: object
): Promise<T> {
  const response = await fetch(new URL(path, baseUrl), {
    body: JSON.stringify(body),
    headers: {
      ...(await userHeaders(login)),
      "Content-Type": "application/json",
      [GOING_RUN_HEADER]: run,
    },
    method,
  });
  if (!response.ok) {
    throw new Error(`${method} ${path} in run ${run}: ${response.status} ${await response.text()}`);
  }
  return (response.status === 204 ? undefined : await response.json()) as T;
}

/** `PATCH /api/v1/issues/{key}` inside the run `run`. */
function patchIssueInRun(
  key: string,
  run: string,
  body: Body<"/api/v1/issues/{key}", "patch">,
  login: string
): Promise<Result<"/api/v1/issues/{key}", "patch">> {
  return inRun(run, login, "PATCH", routeUrl("/api/v1/issues/{key}", { key }), body);
}

/** `POST /api/v1/issues/{key}/comments` with `body` as the comment's words, inside the run
 *  `run`. */
function commentInRun(
  key: string,
  run: string,
  body: string,
  login: string
): Promise<Result<"/api/v1/issues/{key}/comments", "post">> {
  const input: Body<"/api/v1/issues/{key}/comments", "post"> = { body };
  return inRun(run, login, "POST", routeUrl("/api/v1/issues/{key}/comments", { key }), input);
}

/**
 * The issue a command bar run changed, as alice: she opens it, opens a second issue whose
 * comment cites the first by its reference, so the list's own read of the first holds one row,
 * and then asks the command bar `RUN_QUESTION`. At once, inside the run's 30 s window and before
 * any browser step, the run changes the first issue's labels twice and writes one comment on it.
 */
export async function seedRunIssue(): Promise<RunSeed> {
  await createProject({ key: PROJECT, name: "Core" });
  const issue = await createIssue({ project: PROJECT, title: "Release checklist" });
  const citing = await createIssue({ project: PROJECT, title: "Rollout notes" });
  await createComment(citing.key, { body: `Ship once dispatch://${issue.key} is closed.` });
  const run = await runAs("alice", RUN_QUESTION, PROJECT);
  await patchIssueInRun(issue.key, run.id, { labels: FIRST_LABELS }, "alice");
  await patchIssueInRun(issue.key, run.id, { labels: SECOND_LABELS }, "alice");
  await commentInRun(issue.key, run.id, RUN_COMMENT, "alice");
  return {
    citing: citing.key,
    key: issue.key,
    path: buildIssuePath({ key: issue.key, kind: "issue" }),
    run: run.id,
    runAt: run.created_at,
  };
}

/** The address the run's page reads its run at (`run-page/run-page.ts`'s `runAddress`). */
export function runReference(run: string): string {
  return `dispatch://interaction/${run}`;
}

/** Details' control of its Referenced by list: named by the label alone, or, with `count`, by
 *  the label and that count. */
export function referencesControl(page: Page, count?: number): Locator {
  return marginSection(page, "references").getByRole("button", {
    exact: true,
    name:
      count === undefined
        ? PROPERTY_LABELS.referenced_by
        : `${PROPERTY_LABELS.referenced_by} (${count})`,
  });
}

/** The list the control opens. */
function referencesList(page: Page): Locator {
  return marginSection(page, "references").getByRole("region", {
    exact: true,
    name: PROPERTY_LABELS.referenced_by,
  });
}

/** The rows of the list's own read of the issue's references: one for each citing source. */
export function listedRowsOf(page: Page): Locator {
  return referencesList(page).locator(":scope > ul:not([data-extra-references]) > li");
}

/** The rows of the reader's command bar runs, after the list's own. */
export function runRowsOf(page: Page): Locator {
  return marginSection(page, "references").locator("[data-extra-references] > li");
}

/** The line under the list that says the tree read failed. */
export function treeReadFailure(page: Page): Locator {
  return marginSection(page, "references")
    .getByRole("alert")
    .filter({ hasText: ANSWER_LINES.references_unreadable });
}

/** The Retry of a failure line: a control of the line itself, beside its words. */
export function retryBeside(failure: Locator): Locator {
  return failure
    .locator("xpath=..")
    .getByRole("button", { exact: true, name: CONTROL_LABELS.retry });
}

/** The shell's content element: the page a route draws, and nothing beside it. */
export function mainContent(page: Page): Locator {
  return page.getByTestId("main-content");
}

/** The words of the app's not-found page, read at an address no route holds. */
export async function notFoundText(page: Page): Promise<string> {
  await page.goto("/no-such-page");
  await expect(
    mainContent(page).getByRole("heading", { level: 1 }),
    "/no-such-page drew no not-found page"
  ).toBeVisible();
  return (await mainContent(page).textContent()) ?? "";
}

/** Whether `later` stands after `earlier` in the document, each locator's one element. The
 *  handle waits for `later`'s element, and throws where none comes or more than one matches. */
export async function follows(earlier: Locator, later: Locator): Promise<boolean> {
  const handle = await later.elementHandle();
  try {
    return await earlier.evaluate(
      (first, second) =>
        (first.compareDocumentPosition(second) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0,
      handle
    );
  } finally {
    await handle.dispose();
  }
}

/** One read of `/api/v1/references` the page sent: the reference it named and its kind, null
 *  where it named none. */
export interface ReferenceRead {
  readonly kind: string | null;
  readonly to: string | null;
}

/** The route of the reads of what references a node. */
const REFERENCES_ROUTE = "/api/v1/references";

/** Every request `traffic` saw whose route is under `/api/v1/references`, in the order sent. */
export function referenceReads(traffic: ApiRequests): ReferenceRead[] {
  return traffic.requests
    .filter(({ route }) => route === REFERENCES_ROUTE || route.startsWith(`${REFERENCES_ROUTE}/`))
    .map(({ path }) => {
      const query = new URL(path, baseUrl).searchParams;
      return { kind: query.get("kind"), to: query.get("to") };
    });
}

/**
 * Whether the server answers alice, who asked the run `run`, the run's own read
 * (`GET /api/v1/references?to=<address>&kind=changed_by_run`), which the server's run context
 * adds: a 200 says this stack serves runs, and any other answer says it does not yet.
 */
export async function runReadServed(run: string): Promise<boolean> {
  const path = routeUrl(REFERENCES_ROUTE, {}, { kind: "changed_by_run", to: runReference(run) });
  const response = await fetch(new URL(path, baseUrl), { headers: await userHeaders("alice") });
  await response.body?.cancel();
  return response.status === 200;
}

/**
 * Whether the server's tree read of the issue `key` (`GET /api/v1/issues/{key}/tree`) answers
 * alice with a 200 that holds a command bar run, a node of the `interaction` supertag
 * (`run-references/run-references.ts`), which the server's run context writes.
 */
export async function treeRunServed(key: string): Promise<boolean> {
  const response = await fetch(new URL(routeUrl("/api/v1/issues/{key}/tree", { key }), baseUrl), {
    headers: await userHeaders("alice"),
  });
  if (response.status !== 200) {
    await response.body?.cancel();
    return false;
  }
  const tree = (await response.json()) as IssueTreeAnswer;
  return tree.nodes.some((node) => node.supertag === "interaction");
}
