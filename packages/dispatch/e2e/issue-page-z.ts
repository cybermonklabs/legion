import { GOING_RUN_HEADER } from "@legion/contracts";
import { expect, type Locator, type Page } from "@playwright/test";

import { type Body, type Result, routeUrl } from "../web/src/api/paths";
import { PROPERTY_LABELS } from "../web/src/features/issue-page/page-structure";
import {
  ANSWER_LINES,
  CONTROL_LABELS,
  NODE_REFERENCES_WORDS,
  sentenceCase,
} from "../web/src/features/issue-page/page-words";
import { buildDispatchReference, buildIssuePath } from "../web/src/features/refs/routes";
import { baseUrl, createComment, createIssue, createProject, userHeaders } from "./api";
import { marginSection } from "./issue-page-h";
import type { ApiRequests } from "./traffic";

/**
 * What the rows of `e2e/issue-page-z.e2e.ts` seed and read: a command bar run that changed an
 * issue, the run's row in Details' Referenced by, the run's own page, and the reads that name
 * what a run's words cite.
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
 *   tree read with no view, the `li`s of `[data-extra-references]` after them
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
 * comment cites the first by its reference, so that comment is one of the rows of the list's own
 * read of the first, and then asks the command bar `RUN_QUESTION`. What else that read holds -
 * the first issue's own spec, which the server answers as attached to it - is the server's to
 * answer, so a row counts the list's own rows as the list draws them (`listedEdgesOf`). At once,
 * inside the run's 30 s window and before any browser step, the run changes the first issue's
 * labels twice and writes one comment on it.
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

/** The words of the three comments the run of `seedRunReferences` cites, each with no markdown
 *  character, so the page names each comment by its words. */
const CITED_WORDS = ["Warm the cache", "Check the logs", "Ship the build"] as const;

/** The question the run of `seedRunReferences` asks, around the three references it cites, in
 *  words with no markdown character, so Details draws it as typed but for each reference, which it
 *  names. */
function citingQuestion(cited: readonly string[]): string {
  return `Should ${cited.join(" then ")} come before the release`;
}

/** The comment that run writes, around the same three references: one sentence with no markdown
 *  character, under a line's 100 characters once named (`FIRST_LINE_LIMIT`), so the run's page
 *  draws it whole as the comment's line. */
function citingComment(cited: readonly string[]): string {
  return `Ordered as ${cited.join(" then ")} for the release`;
}

/** What `seedRunReferences` wrote. */
export interface RunReferencesSeed {
  /** The `dispatch://<KEY>/comment/<id>` addresses of the three comments the run cites. */
  readonly cited: readonly string[];
  /** Those comments' ids, in the same order: what each comment's own read names. */
  readonly ids: readonly string[];
  /** The key of the issue the run changed. */
  readonly key: string;
  /** The run's comment as the run's page draws its line: each address named by its comment's
   *  words. */
  readonly namedComment: string;
  /** The run's question as Details draws it: each address named by its comment's words. */
  readonly namedQuestion: string;
  /** The run's id. */
  readonly run: string;
}

/**
 * A run whose question and whose comment cite the same three comments, as alice: she opens an
 * issue, opens a second issue holding three comments of words alone, and then asks the command bar
 * a question that cites each of the three by its address. At once, inside the run's 30 s window,
 * the run changes the first issue's labels, so the run is a row of that issue's Referenced by, and
 * writes on the second issue a comment that cites the same three addresses, so its line on the
 * run's page names them. The comment is on the second issue, not the first: the issue page's own
 * names read asks for every reference its tree's comments cite (`useIssueTree.ts`'s
 * `treeReferences`) and publishes the answers, so on the first issue the comment would have the
 * issue's load name the three, and Details' run rows would then read none of them.
 */
export async function seedRunReferences(): Promise<RunReferencesSeed> {
  await createProject({ key: PROJECT, name: "Core" });
  const issue = await createIssue({ project: PROJECT, title: "Release checklist" });
  const holder = await createIssue({ project: PROJECT, title: "Release steps" });
  const ids: string[] = [];
  for (const body of CITED_WORDS) ids.push((await createComment(holder.key, { body })).id);
  const cited = ids.map((id) => buildDispatchReference({ id, key: holder.key, kind: "comment" }));
  const run = await runAs("alice", citingQuestion(cited), PROJECT);
  await patchIssueInRun(issue.key, run.id, { labels: FIRST_LABELS }, "alice");
  await commentInRun(holder.key, run.id, citingComment(cited), "alice");
  return {
    cited,
    ids,
    key: issue.key,
    namedComment: citingComment(CITED_WORDS),
    namedQuestion: citingQuestion(CITED_WORDS),
    run: run.id,
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

/**
 * The edges the list's own rows draw, one line apiece inside the row of the source that cites by
 * it: `listedEdges` of the server's answer as Details reads it, which is what Referenced by's
 * control counts of the list's own. A row counts them as the list draws them, not from the
 * server's answer: `listedEdges` (`refs/ReferencedBy.tsx`) reaches the document schema's JSON,
 * which Playwright's Node does not load (`issue-page/details-names.ts`), and which edges that
 * answer holds is the server's and the list's to say, not these rows'.
 */
export function listedEdgesOf(page: Page): Locator {
  return listedRowsOf(page).locator(":scope > ul > li");
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

/** The route of the batched read of reference targets (`api.getReferenceTargets`), which names
 *  each reference it reads in a `ref` of its query. */
const REFERENCE_TARGETS_ROUTE = "/api/v1/reference-targets";

/** For each batched read of reference targets `traffic` saw that names any of `addresses`, in the
 *  order sent, the ones among `addresses` it names, in their order: one entry per such request. */
export function targetReadsNaming(traffic: ApiRequests, addresses: readonly string[]): string[][] {
  return traffic.requests.flatMap(({ path, route }) => {
    if (route !== REFERENCE_TARGETS_ROUTE) return [];
    const named = new URL(path, baseUrl).searchParams.getAll("ref");
    const held = addresses.filter((address) => named.includes(address));
    return held.length === 0 ? [] : [held];
  });
}

/** The id of each comment among `ids` that a read of the comment alone (`api.getComment`,
 *  `GET /api/v1/comments/{id}`) `traffic` saw names, one per request, in the order sent. */
export function commentReads(traffic: ApiRequests, ids: readonly string[]): string[] {
  return traffic.requests.flatMap(({ method, path }) => {
    if (method !== "GET") return [];
    const { pathname } = new URL(path, baseUrl);
    return ids.filter((id) => pathname === routeUrl("/api/v1/comments/{id}", { id }));
  });
}

/** The route of the issue's tree read (`GET /api/v1/issues/{key}/tree`), as `apiRoute` groups its
 *  paths (`e2e/traffic.ts`): whatever issue it reads. */
const TREE_ROUTE = "/api/v1/issues/:id/tree";

/** What a request of the tree route names in its query beside the first page of the read with no
 *  view and no kind: a view (`view`), Conversation's kinds (`kind`), and a next page or an older
 *  comment page of a read (`cursor`, `comments_before`). */
const NOT_THE_TREE_READ = ["view", "kind", "cursor", "comments_before"] as const;

/** How many reads of the issue's tree with no view and no kind `traffic` saw - Graph's read, which
 *  the run rows take too (`issueTreeQuery`) - each counted by its first page: a request of the
 *  tree route whose query names none of `NOT_THE_TREE_READ`, since a read asks for its next pages
 *  and its older comments itself. */
export function noViewTreeReads(traffic: ApiRequests): number {
  return traffic.requests.filter(({ path, route }) => {
    if (route !== TREE_ROUTE) return false;
    const query = new URL(path, baseUrl).searchParams;
    return !NOT_THE_TREE_READ.some((name) => query.has(name));
  }).length;
}
