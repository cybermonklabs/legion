import { expect, type Locator, type Page, type Route, test } from "@playwright/test";

import type { IssueDecisions, IssueTreeRead } from "../web/src/api/types";
import type { Filter } from "../web/src/features/issue-page/presets";
import {
  mergeTreePages,
  nextTreeCursor,
  type TreeIdReads,
  treeIdOf,
} from "../web/src/features/issue-page/tree-read/tree-nodes";
import { parseNodeReference } from "../web/src/features/refs/routes";
import { nextFrames } from "./frames";
import { blockNode } from "./issue-page-g";
import { issueHeader } from "./issue-page-p";
import type { ApiRequest } from "./traffic";

/**
 * What the rows of the `e2e/issue-page-t-*.e2e.ts` files read of what the page received
 * besides what it drew, the gates they check first, and what the page draws beside the rows.
 * These helpers are kept apart from the row readers of `e2e/issue-page-t.ts`, which the recorder
 * (`e2e/tools/issue-page-t-record.ts`) loads on a base whose page builds its tree from the old
 * reads and holds no tree read: what reads the tree read names its modules and its answer's
 * types, which that base does not hold, so it is here, so that file loads no more than the
 * recorder needs; the steps and comparisons the rows share are in `e2e/issue-page-t-rows.ts`.
 *
 * `treeAnswerOf` and `viewAnswerOf` keep each answer of the tree read
 * (`GET /api/v1/issues/{key}/tree`) and of one of its views (`?view=`), every page of a read
 * merged; `decisionsReadOf` keeps the decisions read; `viewNodeIds` names the rows root R2 lets a
 * view draw. `treeReads` and `blocksReads` count what a load or a press sent. Each row's gate is
 * one `owedUnless` call on a probe (`treeReadGate`, `convertedGate`, `decisionsViewGate`,
 * `decisionNodesGate`, `partUGate`, `footLineGate`): where the gate is not on this branch's base
 * the row skips, naming the gate in an `owed` annotation, and never passes, since a row cannot
 * pass on code the base does not hold yet.
 */

// ---------------------------------------------------------------------------------------------
// What the page received of the tree read and its views, and what a load or a press sent.
// ---------------------------------------------------------------------------------------------

/** One of the four filters, by the `view` its read of the tree sends (`presets.ts`' `Filter`). */
export type TreeView = Filter;

/** The tree read's path, whatever issue it reads. */
export const TREE_PATH = /^\/api\/v1\/issues\/[^/]+\/tree$/;

/** The path of issue `key`'s tree read. */
function treePath(key: string): string {
  return `/api/v1/issues/${encodeURIComponent(key)}/tree`;
}

/** Matches every request of issue `key`'s tree read, with or without a query: the whole tree, a
 *  view of it (`?view=`) and each next page (`?cursor=`). One function per call, so a row that
 *  routes a hold through it unroutes the same one. */
export function treeReadRoute(key: string): (url: URL) => boolean {
  const path = treePath(key);
  return (url) => url.pathname === path;
}

/** What `refuseTreeRead` hands its row, to count its refusals and to end them. */
export interface RefusedTreeRead {
  /** Takes the refusal off, so the tree read reaches the server again. */
  lift(): Promise<void>;
  /** How many requests of the tree read have been refused so far, the page's own retries
   *  included. */
  refusals(): number;
  /** Settles once `count` requests of the tree read have been refused in all, and not before. */
  refused(count: number): Promise<void>;
}

/**
 * Refuses issue `key`'s tree read (`treeReadRoute`), with a 500 in the server's error shape, from
 * the next request the page sends until `lift`, as many times as the page asks it: what a row
 * reads of a view whose documents' blocks the tree read answers when that read fails. A refusal
 * is counted once its answer has been sent, so a row that waits on `refused(n)` reads a page that
 * has had its nth refusal.
 */
export async function refuseTreeRead(page: Page, key: string): Promise<RefusedTreeRead> {
  const route = treeReadRoute(key);
  let refusals = 0;
  // Each wait a row asked for, by the count it waits on: the refusal that reaches a wait's count
  // is the one that settles it. The package compiles against ES2022, whose library names no type
  // for what `Promise.withResolvers` returns, so the two parts used are named here.
  const waiting = new Map<
    number,
    { readonly promise: Promise<void>; readonly resolve: () => void }
  >();
  const refuse = async (request: Route) => {
    await request.fulfill({ json: { code: "INTERNAL", error: "boom" }, status: 500 });
    refusals += 1;
    waiting.get(refusals)?.resolve();
    waiting.delete(refusals);
  };
  await page.route(route, refuse);
  return {
    lift: () => page.unroute(route, refuse),
    refusals: () => refusals,
    refused: (count) => {
      if (refusals >= count) return Promise.resolve();
      const wait = waiting.get(count) ?? Promise.withResolvers<void>();
      waiting.set(count, wait);
      return wait.promise;
    },
  };
}

/** What the page received of the tree route, for one view or none. */
export interface TreeAnswerWatch {
  /** The newest read whose every page has answered, its pages merged; undefined until one has. */
  answer(): IssueTreeRead | undefined;
}

/** One page of a read as the page received it: in which place it arrived, the cursor its request
 *  sent (none for a read's first page), and the page. */
interface ReceivedPage {
  readonly arrived: number;
  readonly cursor: string | null;
  readonly page: IssueTreeRead;
}

/** Keeps every answer of the tree route whose `view` is `view` (none for null), from now on. */
function watchTreeRoute(page: Page, view: TreeView | null): TreeAnswerWatch {
  const received: ReceivedPage[] = [];
  let arrivals = 0;
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (
      response.request().method() !== "GET" ||
      !TREE_PATH.test(url.pathname) ||
      url.searchParams.get("view") !== view ||
      !response.ok()
    ) {
      return;
    }
    const arrived = arrivals;
    arrivals += 1;
    const cursor = url.searchParams.get("cursor");
    void response.json().then(
      (body: IssueTreeRead) => {
        received.push({ arrived, cursor, page: body });
      },
      // A body the browser let go of before it was read, as the page moved on, is no answer.
      () => undefined
    );
  });
  return { answer: () => newestRead(received) };
}

/** The newest read every page of which has answered: a read starts at a page its request sent no
 *  cursor for, goes on with each page whose cursor the page before it named, and ends at a page
 *  that names none, each by the page's own rule (`nextTreeCursor`). */
function newestRead(received: readonly ReceivedPage[]): IssueTreeRead | undefined {
  const reads: IssueTreeRead[][] = [];
  for (const { cursor, page } of [...received].sort((a, b) => a.arrived - b.arrived)) {
    const read = reads[reads.length - 1];
    if (cursor === null) reads.push([page]);
    else if (read !== undefined && nextTreeCursor(read[read.length - 1]) === cursor) {
      read.push(page);
    }
  }
  for (let at = reads.length - 1; at >= 0; at -= 1) {
    const [first, ...rest] = reads[at] ?? [];
    if (first !== undefined && nextTreeCursor(rest[rest.length - 1] ?? first) === undefined) {
      return mergeTreePages([first, ...rest]);
    }
  }
  return undefined;
}

/** The tree read the page receives with no view, every page of its newest read merged. Start it
 *  before the navigation, so the first load's read is kept. */
export function treeAnswerOf(page: Page): TreeAnswerWatch {
  return watchTreeRoute(page, null);
}

/** `treeAnswerOf` for the read of one view, `?view=<filter>`. */
export function viewAnswerOf(page: Page, filter: TreeView): TreeAnswerWatch {
  return watchTreeRoute(page, filter);
}

/** Waits until `watch` holds a read whose every page answered, and answers it. */
export async function heldAnswer(watch: TreeAnswerWatch, what: string): Promise<IssueTreeRead> {
  const message = `${what} never answered its last page`;
  await expect.poll(() => watch.answer() !== undefined, { message }).toBe(true);
  const answer = watch.answer();
  if (answer === undefined) throw new Error(message);
  return answer;
}

/** Root R2's rows of each filter: the kinds a view draws as rows of its own, the nodes its query
 *  took, and as context, the nodes it holds to place them. Spec draws the statements;
 *  Verification each statement as context, with the checks under it and their results; Plan the
 *  plan tasks; Decisions the questions and the decisions. */
export const R2_ROW_KINDS: Readonly<
  Record<TreeView, { readonly context: readonly string[]; readonly query: readonly string[] }>
> = {
  decisions: { context: [], query: ["ask", "decision"] },
  plan: { context: [], query: ["plan_task"] },
  spec: { context: [], query: ["requirement"] },
  verification: { context: ["requirement"], query: ["check", "result"] },
};

/** The main kind each filter's control counts (R2). */
export const R2_COUNTED_KINDS: Readonly<Record<TreeView, readonly string[]>> = {
  decisions: ["ask", "decision"],
  plan: ["plan_task"],
  spec: ["requirement"],
  verification: ["check"],
};

/** A node a view draws as a row, by the id its row carries, and the role its view gives it. */
export interface ViewNodeId {
  readonly id: string;
  readonly role: "context" | "query";
}

/**
 * The rows root R2 lets `filter` draw of its view read `answer`: each node of this issue of the
 * filter's own kinds that the view's query took, and each of its context kinds the view holds as
 * context, by the id `treeIdOf` gives it (the id its row carries), once each, in the answer's
 * order. `reads` are the issue read and the asks read, which `treeIdOf` names an issue's
 * documents and an ask's block by.
 */
export function viewNodeIds(
  answer: IssueTreeRead,
  filter: TreeView,
  reads: TreeIdReads
): ViewNodeId[] {
  const idOf = treeIdOf(answer, reads);
  const kinds = R2_ROW_KINDS[filter];
  const named = new Set<string>();
  return (answer.nodes ?? []).flatMap((node): ViewNodeId[] => {
    const role = node.role === "query" ? "query" : "context";
    const foreign = node.issue !== undefined && node.issue !== reads.issue.key;
    const id = idOf(node.id);
    if (foreign || !kinds[role].includes(node.supertag) || named.has(id)) return [];
    named.add(id);
    return [{ id, role }];
  });
}

/** What `filter`'s control counts of its view read `answer`: the nodes of its main kind the
 *  view's query took, each once (root R2). */
export function viewCount(answer: IssueTreeRead, filter: TreeView): number {
  const kinds = R2_COUNTED_KINDS[filter];
  return new Set(
    (answer.nodes ?? []).flatMap((node) =>
      node.role === "query" && kinds.includes(node.supertag) ? [node.id] : []
    )
  ).size;
}

/** How many reads of issue `key`'s tree `requests` hold for `view` (none for null): each read's
 *  first page, a request that sends no cursor, since a read asks for its next pages itself. */
export function treeReads(
  requests: readonly ApiRequest[],
  key: string,
  view: TreeView | null
): number {
  return requests.filter((request) => {
    const url = new URL(request.path, "http://requests.invalid");
    return (
      request.method === "GET" &&
      url.pathname === treePath(key) &&
      url.searchParams.get("view") === view &&
      !url.searchParams.has("cursor")
    );
  }).length;
}

/** Every request of `requests` that is no page of a read of issue `key`'s view `view`, as its
 *  method and path: what a press sent beside its view's own read. */
export function besideViewRead(
  requests: readonly ApiRequest[],
  key: string,
  view: TreeView
): string[] {
  return requests.flatMap((request) => {
    const url = new URL(request.path, "http://requests.invalid");
    const own =
      request.method === "GET" &&
      url.pathname === treePath(key) &&
      url.searchParams.get("view") === view;
    return own ? [] : [`${request.method} ${request.path}`];
  });
}

/** The reads of a document's blocks (`GET /api/v1/artifacts/{id}/blocks`) `requests` hold, by
 *  path. */
export function blocksReads(requests: readonly ApiRequest[]): string[] {
  return requests.flatMap((request) =>
    request.method === "GET" && request.route === "/api/v1/artifacts/:id/blocks"
      ? [request.path]
      : []
  );
}

/** The decisions read the page received: today's (`GET /api/v1/issues/{key}/decisions`), or once
 *  the model moves it onto the tree read, the tree read's view `?view=decisions`. */
export type DecisionsRead =
  | { readonly answer: IssueDecisions; readonly kind: "decisions" }
  | { readonly answer: IssueTreeRead; readonly kind: "view" };

/** What the page received of its decisions read. */
export interface DecisionsReadWatch {
  /** The newest answer, the view's where the page read the view; undefined before one came. */
  read(): DecisionsRead | undefined;
}

/** Keeps the decisions read of issue `key` the page receives from now on, in either form. */
export function decisionsReadOf(page: Page, key: string): DecisionsReadWatch {
  const view = viewAnswerOf(page, "decisions");
  const path = `/api/v1/issues/${encodeURIComponent(key)}/decisions`;
  let newest: IssueDecisions | undefined;
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (response.request().method() !== "GET" || url.pathname !== path || !response.ok()) return;
    void response.json().then(
      (body: IssueDecisions) => {
        newest = body;
      },
      () => undefined
    );
  });
  return {
    read: () => {
      const answer = view.answer();
      if (answer !== undefined) return { answer, kind: "view" };
      return newest === undefined ? undefined : { answer: newest, kind: "decisions" };
    },
  };
}

/** When each decision of `read` last changed (`changed_at`), by every id a row may draw it by:
 *  its block's id alone, and `<slug>#b-<block id>` for each of `slugs`, the slugs of the issue's
 *  documents; for a decision node of a view, its block id and its document's form of it, from
 *  the decisions read's object the node carries (`decision`). */
export function decisionTimes(
  read: DecisionsRead | undefined,
  slugs: readonly string[]
): ReadonlyMap<string, string> {
  const times = new Map<string, string>();
  const at = (block: string, own: readonly string[], changed: unknown): void => {
    if (typeof changed !== "string") return;
    for (const id of [block, ...own, ...slugs.map((slug) => blockNode(slug, block))]) {
      times.set(id, changed);
    }
  };
  if (read?.kind === "decisions") {
    for (const decision of read.answer.decisions ?? []) {
      at(decision.block_id, [], decision.changed_at);
    }
  } else if (read?.kind === "view") {
    for (const node of read.answer.nodes ?? []) {
      if (node.supertag !== "decision") continue;
      const block = node.block_id ?? node.id;
      const own = node.document === undefined ? [] : [blockNode(node.document.slug, block)];
      at(block, own, node.decision?.changed_at);
    }
  }
  return times;
}

/** Whether the seed's decisions are nodes of the tree read `answer`: it holds a decision node of
 *  this issue, and every decision the decisions read answers is one - a decision node of this
 *  issue whose block id is the decision's `block_id`, in the document the decision's `ref` names
 *  where the node names its document. The check is this file's own, so the page's matching of
 *  decisions to nodes can move under it. */
export function decisionsAreNodes(answer: IssueTreeRead, read: DecisionsRead | undefined): boolean {
  const own = new Map(
    (answer.nodes ?? []).flatMap((node) =>
      node.supertag === "decision" && node.issue === undefined
        ? [[node.block_id ?? node.id, node] as const]
        : []
    )
  );
  if (own.size === 0 || read === undefined) return false;
  if (read.kind === "view") return true;
  return (read.answer.decisions ?? []).every((decision) => {
    const node = own.get(decision.block_id);
    if (node === undefined) return false;
    if (node.document === undefined) return true;
    const route = parseNodeReference(decision.ref);
    return route?.kind === "artifact" && route.slug === node.document.slug;
  });
}

// ---------------------------------------------------------------------------------------------
// The gates: a row, or a clause in a row of its own, whose gate is not on this branch's base
// skips.
// ---------------------------------------------------------------------------------------------

/** One gate a row waits on: whether its probe found it on this branch's base, and what the row
 *  owes while it is not, naming the gate, what the probe read and how an owed row counts. */
export interface Gate {
  readonly met: boolean;
  readonly owed: string;
}

/** How every owed row counts, which its `owed` annotation says after naming its gate: a row cannot
 *  pass on code the base does not hold yet, and a failure there would say nothing of the page. */
export const OWED_BY = "a row whose gate is not on the base counts as skipped, never as passed";

/**
 * Goes on where `gate` is met; where it is not, records an `owed` annotation naming it and skips
 * the row, which then counts as skipped and never as passed (`OWED_BY` says why). Call it
 * first thing a gate can be read, before the row asserts anything it waits on.
 */
export function owedUnless(gate: Gate): void {
  if (gate.met) return;
  test.info().annotations.push({ description: gate.owed, type: "owed" });
  test.skip(true, gate.owed);
}

/**
 * Waits until every event of the seed has landed: the workspace's event head, which the sign-in
 * check names (`event_head`), reads the same twice a second apart. A first load opened before
 * then would hear the seed's last events through the live feed and read the tree again for them,
 * which a row's count of one tree read would take for the page's own. Read through the signed-in
 * page's own request context.
 */
export async function seedEventsLanded(page: Page): Promise<void> {
  let last: string | undefined;
  await expect
    .poll(
      async () => {
        const response = await page.request.get("/auth/whoami");
        const answer: unknown = await response.json();
        const head =
          answer !== null &&
          typeof answer === "object" &&
          "event_head" in answer &&
          typeof answer.event_head === "string"
            ? answer.event_head
            : undefined;
        const still = head !== undefined && head === last;
        last = head;
        return still;
      },
      {
        intervals: [1_000],
        message: "the workspace's event head was still moving after the seed",
        timeout: 30_000,
      }
    )
    .toBe(true);
}

/** The model's tree read, which answers 404 for issue `key` while the route is absent. Read
 *  through the signed-in page's own request context. */
export async function treeReadGate(page: Page, key: string): Promise<Gate> {
  const response = await page.request.get(treePath(key));
  return {
    met: response.status() !== 404,
    owed:
      "owed until the model's tree read is on this branch's base: the route answers 404 here; " +
      OWED_BY,
  };
}

/** The model's issues born converted: issue `key`'s tree read says it is one. */
export async function convertedGate(page: Page, key: string): Promise<Gate> {
  const path = treePath(key);
  const response = await page.request.get(path);
  expect(response.ok(), `GET ${path} answers ${response.status()}`).toBe(true);
  const { converted } = (await response.json()) as IssueTreeRead;
  return {
    met: converted === true,
    owed:
      "owed until the model's issues born converted are on this branch's base: the tree read " +
      `answers converted: false here; ${OWED_BY}`,
  };
}

/** The model's decisions read moved onto the tree read: what the page sent, `requests`, holds a
 *  read of issue `key`'s tree with `view=decisions`. */
export function decisionsViewGate(requests: readonly ApiRequest[], key: string): Gate {
  return {
    met: treeReads(requests, key, "decisions") > 0,
    owed:
      "owed until the model's decisions view of the tree read is on this branch's base: the " +
      `page reads its decisions here with no view=decisions read of the tree; ${OWED_BY}`,
  };
}

/** What `decisionNodesGate` and `partUGate` read where they are not met: the seed's decisions,
 *  written as decision blocks of the spec, are no nodes of the tree read this base answers. */
const DECISIONS_ARE_NO_NODES =
  "the seed's decisions are no nodes of the tree read this base answers";

/** The decisions work that makes each decision a node of the tree read (`decisionsAreNodes`). */
export function decisionNodesGate(answer: IssueTreeRead, read: DecisionsRead | undefined): Gate {
  return {
    met: decisionsAreNodes(answer, read),
    owed:
      "owed until decisions are nodes of the tree read on this branch's base: " +
      `${DECISIONS_ARE_NO_NODES}; ${OWED_BY}`,
  };
}

/** The page drawing the decisions from the tree read's nodes, which waits on the decisions work
 *  that makes them nodes reaching this branch's base (`decisionsAreNodes`). */
export function partUGate(answer: IssueTreeRead, read: DecisionsRead | undefined): Gate {
  return {
    met: decisionsAreNodes(answer, read),
    owed:
      "owed until the page draws the decisions from the tree read's nodes, once decisions are " +
      `nodes of the tree read on this branch's base: ${DECISIONS_ARE_NO_NODES}; ${OWED_BY}`,
  };
}

/** The model storing a decision that settles a question under that question, which waits on
 *  the model: the tree read answers the decision block `decision` with the ask `ask` as its
 *  parent. The page places no node itself, so until then the decision stands where the read's
 *  parent puts it. */
export function decisionUnderAskGate(answer: IssueTreeRead, decision: string, ask: string): Gate {
  return {
    met: (answer.nodes ?? []).some(
      (node) => node.block_id === decision && node.parent?.id.endsWith(ask) === true
    ),
    owed:
      "owed until the tree read answers a decision that settles a question with that question " +
      `as its parent: this base answers it under the issue; ${OWED_BY}`,
  };
}

/** How many frames the page's writer looks for its line in after a Reply (`outline-writer.tsx`:
 *  ten after the first), and one more, as the rows of `e2e/issue-page-g.e2e.ts` wait them out. */
const WRITER_FRAMES = 12;

/** The line at the foot of the outline, which a row's Reply puts the caret in: once a Reply was
 *  pressed, the focus is on a field the reader types in, inside the page. The work that draws
 *  the line is not on this branch's base (`slots/foot.tsx` draws nothing until it is). */
export async function footLineGate(page: Page): Promise<Gate> {
  for (let passed = 0; passed < WRITER_FRAMES; passed += 2) await nextFrames(page);
  const met = await page.evaluate(() => {
    const focused = document.activeElement;
    return (
      focused instanceof HTMLElement &&
      focused.closest("[data-issue-page]") !== null &&
      (focused.isContentEditable ||
        focused.matches("textarea, input:not([type]), input[type=text]"))
    );
  });
  return {
    met,
    owed:
      "owed until the line at the foot of the outline is on this branch's base: a Reply puts " +
      `the caret in no line here; ${OWED_BY}`,
  };
}

// ---------------------------------------------------------------------------------------------
// What the page draws beside the rows.
// ---------------------------------------------------------------------------------------------

/** The status control of the page's header (`header[data-issue-header]`), the one place the
 *  new page draws the issue's own status: the outline draws no row for the issue. */
export function headerStatus(page: Page): Locator {
  return issueHeader(page).getByRole("combobox", { exact: true, name: "Status" });
}

/** One node a filter draws: a row, or a check's result inside its row (the Verification view). */
export interface DrawnNode {
  /** Whether it is drawn as context (`data-node-context`). */
  readonly context: boolean;
  readonly id: string;
  readonly type: string;
}

/** Every node the page draws with its id and its kind (`[data-node-id][data-node-type]`), in
 *  drawing order: each row, and each result the Verification view draws inside its check's row. */
export function drawnNodes(page: Page): Promise<DrawnNode[]> {
  return page.evaluate(() =>
    Array.from(
      document.querySelectorAll<HTMLElement>("[data-issue-page] [data-node-id][data-node-type]"),
      (element) => ({
        context: element.hasAttribute("data-node-context"),
        id: element.dataset.nodeId ?? "",
        type: element.dataset.nodeType ?? "",
      })
    )
  );
}

/** Whether the link `href` goes to a node `names` holds: a block by its fragment (`#b-<id>`), a
 *  block by its row id, the link's last path segment followed by its fragment (`<slug>#b-<id>`,
 *  as `/issues/<KEY>/artifacts/<slug>#b-<id>` ends), an ask by its address (`/asks/<id>`). */
export function linksTo(href: string, names: readonly string[]): boolean {
  const url = new URL(href, "http://links.invalid");
  // A row id always ends in a block's fragment, so a link with none is no block's row id.
  const rowId = url.hash.startsWith("#b-")
    ? `${url.pathname.slice(url.pathname.lastIndexOf("/") + 1)}${url.hash}`
    : null;
  return names.some(
    (name) => url.hash === `#b-${name}` || rowId === name || url.pathname.endsWith(`/asks/${name}`)
  );
}
