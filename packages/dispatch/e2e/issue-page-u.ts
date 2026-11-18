import { expect, type Page, type Request, type Route } from "@playwright/test";

import type { IssueTreeAnswer, IssueTreeNode } from "../web/src/api/client";
import { NODE_STATE_WORDS, PRESET_NAMES } from "../web/src/features/issue-page/page-words";
import {
  mergeTreePages,
  type TreeIdReads,
} from "../web/src/features/issue-page/tree-read/tree-nodes";
import { baseUrl, editArtifact, getIssue, listIssueAsks, userHeaders } from "./api";
import { asPlanner } from "./issue-layout-seed-support";
import { requirementBlockId, specBlockRef } from "./issue-layout-seed-text";
import { presetCount, presetToggle } from "./issue-page";
import { blockNode } from "./issue-page-g";
import { expectActivePreset, pickPreset } from "./issue-page-i";
import {
  type DecisionsRead,
  type DecisionsReadWatch,
  drawnRowReadings,
  type Gate,
  OWED_BY,
  type RowReading,
  steadily,
  type TreeView,
} from "./issue-page-t";
import type { ApiRequests } from "./traffic";

/**
 * What the decision rows (`e2e/issue-page-u.e2e.ts`) read of the new issue page beside what the
 * other rows' helpers already read (`e2e/issue-page.ts` and `e2e/issue-page-{g,i,p,q,t}.ts`):
 * the rows' own helper file, which reuses those readers where their files export them. It keeps
 * its own `treePath`, `idReadsOf` and `releaseFilter`, since the files that hold the originals
 * export none of them (`e2e/issue-page-t.ts`' `treePath`) or are spec files
 * (`e2e/issue-page-t.e2e.ts`' `idReadsOf` and `releaseFilter`).
 *
 * - **The server's answer.** `decisionNodesOf` is the decision nodes the tree read
 *   (`GET /api/v1/issues/{key}/tree`) answers for some blocks, every page its cursor names read
 *   once as alice; `decisionSumOf` is the `text_sha256` one of them carries, and `decisionSum`
 *   that of one block as the tree read answers it now. `viewNodesOf` is the nodes one view of
 *   the tree answers (`?view=`). `heldDecisionsRead` is the decisions read that the gate on the
 *   seeds written as decision nodes (`decisionNodesGate`) reads, and `decisionsViewServedGate`
 *   the gate on the server serving the tree read's Decisions view.
 * - **How a row names a node.** `decisionRowId` is the id a decision node's row carries (the
 *   `<slug>#b-<block id>`), `askRowId` an ask's (its block's node where the asks read names its
 *   block, as `tree-read/tree-nodes.ts` names it), and `askOfNode` the asks read's row of an ask
 *   node; `idReadsOf` is the issue read and the asks read `treeIdOf` names nodes by.
 * - **What the page draws.** `reviewWordOf` is the state word a decision's row draws from its
 *   node; `specColumn` is, for each statement the Spec view draws, what stands beside it - the
 *   decision cell whose bracket spans it (`[data-spec-cell]`) and the links of its line
 *   (`[data-spec-because]`) - and `statementsBeside` the statements a decision stands beside.
 *   `readFilter` and `releaseFilter` press one filter on and off as the rows of
 *   `e2e/issue-page-t.e2e.ts` do.
 * - **What the page sends and receives.** `watchWrites` keeps every request under `/api/` that is
 *   no read, with its body; `holdTreeReads` holds every tree read the page sends until released.
 * - **Row 2's third decision.** `writeThirdDecision` writes, through the spec's edit route, a
 *   `:::decision` block whose `shaped` names `r1`, as the planner; `rewordThirdDecision` changes
 *   its words.
 */

/** The path of issue `key`'s tree read. */
function treePath(key: string): string {
  return `/api/v1/issues/${encodeURIComponent(key)}/tree`;
}

/**
 * Issue `key`'s tree read, or the read of one of its views, as alice reads it: every page its
 * cursor names, the pages merged as the page merges them (`mergeTreePages`). A cursor the read
 * names twice fails the read rather than walk the same pages again.
 */
async function readTree(key: string, view: TreeView | null): Promise<IssueTreeAnswer> {
  const headers = await userHeaders("alice");
  const pages: IssueTreeAnswer[] = [];
  const cursors = new Set<string>();
  let cursor: string | null = null;
  do {
    const url = new URL(treePath(key), baseUrl);
    if (view !== null) url.searchParams.set("view", view);
    if (cursor !== null) url.searchParams.set("cursor", cursor);
    const response = await fetch(url, { headers });
    if (!response.ok) {
      throw new Error(
        `GET ${url.pathname}${url.search} failed: ${response.status} ${await response.text()}`
      );
    }
    const answered = (await response.json()) as IssueTreeAnswer;
    pages.push(answered);
    cursor = answered.cursor;
    if (cursor !== null && cursors.has(cursor)) {
      throw new Error(`${url.pathname} names the cursor ${cursor} twice`);
    }
    if (cursor !== null) cursors.add(cursor);
  } while (cursor !== null);
  const [first, ...rest] = pages;
  if (first === undefined) throw new Error(`${treePath(key)} answered no page`);
  return mergeTreePages([first, ...rest]);
}

/** The decision nodes of issue `key` whose blocks are `blocks`, in that order, from one tree
 *  read as it answers now. */
export async function decisionNodesOf(
  key: string,
  blocks: readonly string[]
): Promise<IssueTreeNode[]> {
  const { nodes } = await readTree(key, null);
  return blocks.map((block) => {
    const node = nodes.find(
      (one) => one.supertag === "decision" && one.issue === undefined && one.id === block
    );
    if (node === undefined) throw new Error(`${key}'s tree read holds no decision node ${block}`);
    return node;
  });
}

/** The `text_sha256` the tree read answers on `node`, a decision node of issue `key`: the sum a
 *  Keep of the words the node holds carries. */
export function decisionSumOf(key: string, node: IssueTreeNode): string {
  const sum = node.text_sha256;
  if (sum === undefined || sum === "") {
    throw new Error(`${key}'s tree read answers no text_sha256 on its decision node ${node.id}`);
  }
  return sum;
}

/** The `text_sha256` the tree read answers on issue `key`'s decision node `block`, every page its
 *  cursor names read: the sum a Keep of the words as they stand now carries. */
export async function decisionSum(key: string, block: string): Promise<string> {
  const [node] = await decisionNodesOf(key, [block]);
  return decisionSumOf(key, node);
}

/** The nodes the read of issue `key`'s view `view` answers, every page its cursor names. */
export async function viewNodesOf(key: string, view: TreeView): Promise<IssueTreeNode[]> {
  return (await readTree(key, view)).nodes;
}

/**
 * The decisions read that the gate on the seeds written as decision nodes (`decisionNodesGate`)
 * reads: the one the page received (`decisionsReadOf`), and where it received none - the page
 * sends its view read only once Decisions is pressed - the read of the Decisions view the row
 * sends itself, so the gate reads what the server holds and not whether the page asked.
 */
export async function heldDecisionsRead(
  key: string,
  watch: DecisionsReadWatch
): Promise<DecisionsRead> {
  return watch.read() ?? { answer: await readTree(key, "decisions"), kind: "view" };
}

/** The model's Decisions view of the tree read, as the server serves it: issue `key`'s tree read
 *  with `view=decisions` answers, read through the signed-in page's own request context. It is
 *  met by the server's answer, never by whether the page asked for the view, so a press of
 *  Decisions that sends no view read fails its row. */
export async function decisionsViewServedGate(page: Page, key: string): Promise<Gate> {
  const response = await page.request.get(`${treePath(key)}?view=decisions`);
  return {
    met: response.ok(),
    owed:
      "owed until the server serves the tree read's Decisions view on this branch's base: the " +
      `tree read answers ${response.status()} to view=decisions here; ${OWED_BY}`,
  };
}

/** The issue read and the asks read, as alice reads them: what `treeIdOf` names a node by. */
export async function idReadsOf(key: string): Promise<TreeIdReads> {
  const [issue, asks] = await Promise.all([getIssue(key), listIssueAsks(key)]);
  return { asks, issue };
}

/** The id the row of a decision node carries: `<slug>#b-<block id>` for a block of a
 *  document, which every decision node is. */
export function decisionRowId(node: IssueTreeNode): string {
  return node.document === undefined ? node.id : blockNode(node.document.slug, node.id);
}

/** One row of the asks read. */
type ReadAsk = NonNullable<TreeIdReads["asks"]>[number];

/** The id the row of ask `askId` carries: the node of the block the asks read says it was indexed
 *  from, in that block's document, and its own id where it names none. */
export function askRowId(reads: TreeIdReads, askId: string): string {
  const ask = (reads.asks ?? []).find((one) => one.id === askId);
  if (ask === undefined || ask.block_id === null) return askId;
  const slug =
    ask.block_artifact?.slug ??
    reads.issue.artifacts?.find((artifact) => artifact.id === ask.artifact_id)?.slug;
  return slug === undefined ? askId : blockNode(slug, ask.block_id);
}

/** The row of the asks read that the ask node `node` is: by the block it is in its document, or
 *  by its own id where it is a block of none. */
export function askOfNode(node: IssueTreeNode, reads: TreeIdReads): ReadAsk | undefined {
  const { document } = node;
  return (reads.asks ?? []).find((ask) =>
    document === undefined
      ? ask.id === node.id
      : ask.block_id === node.id &&
        (ask.block_artifact?.id ?? ask.artifact_id) === document.artifact_id
  );
}

/** The state word a decision's row draws from its node: "Replaced", "Waiting for review", or
 *  "Kept by" and the person whose Keep stands; none for a decision in none of them. */
export function reviewWordOf(node: IssueTreeNode): string | null {
  const words = NODE_STATE_WORDS.decision;
  if (node.replaced === true) return words.replaced;
  if (node.waiting_review === true) return words.waiting_review;
  return node.kept == null ? null : `${words.kept} ${node.kept.kept_by}`;
}

/** What stands beside one statement of the Spec view. */
export interface SpecColumnRow {
  /** The `data-node-id` and `data-decision-id` of the cell whose bracket spans the statement. */
  readonly cell: readonly string[];
  /** The statement's row id. */
  readonly id: string;
  /** The `href` of each link of the statement's line: the decision of its run, and each other
   *  decision that informed it. */
  readonly links: readonly string[];
}

/** Each statement the Spec view draws, in drawing order, with what stands beside it. */
export function specColumn(page: Page): Promise<SpecColumnRow[]> {
  return page.evaluate(() => {
    const cells = [...document.querySelectorAll("[data-issue-page] [data-spec-cell]")];
    return [...document.querySelectorAll("[data-issue-page] [data-spec-statement]")].map(
      (statement) => {
        const id = statement.getAttribute("data-node-id") ?? "";
        const cell = cells.find((one) =>
          (one.getAttribute("data-spec-statements") ?? "").split(" ").includes(id)
        );
        return {
          cell: [cell?.getAttribute("data-node-id"), cell?.getAttribute("data-decision-id")].filter(
            (name): name is string => typeof name === "string" && name !== ""
          ),
          id,
          links: [...statement.querySelectorAll("[data-spec-because] a[href]")].map(
            (link) => link.getAttribute("href") ?? ""
          ),
        };
      }
    );
  });
}

/** Whether the link `href` goes to a node `names` holds: a block by its fragment (`#b-<id>`), an
 *  ask by its address (`/asks/<id>`). */
function linksTo(href: string, names: readonly string[]): boolean {
  const url = new URL(href, "http://links.invalid");
  return names.some((name) => url.hash === `#b-${name}` || url.pathname.endsWith(`/asks/${name}`));
}

/** The statements of `column` a decision stands beside, by any of its `names` (its row id, its
 *  block id, its ask's id): the cell beside the statement is the decision's, or the statement's
 *  line links to it. */
export function statementsBeside(
  column: readonly SpecColumnRow[],
  names: readonly string[]
): string[] {
  return column.flatMap((row) =>
    row.cell.some((name) => names.includes(name)) || row.links.some((href) => linksTo(href, names))
      ? [row.id]
      : []
  );
}

/** One filter as it holds still once pressed: its control's count and the rows it draws. */
export interface FilterReading {
  readonly count: string | null;
  readonly rows: readonly RowReading[];
}

/** Turns `filter` on, as `pickPreset` presses it, and reads it once it holds still. */
export async function readFilter(
  page: Page,
  traffic: ApiRequests,
  filter: TreeView
): Promise<FilterReading> {
  const name = PRESET_NAMES[filter];
  await pickPreset(page, name);
  await expectActivePreset(page, name);
  return steadily(page, traffic, `the ${name} filter`, async () => ({
    count: await presetCount(page, filter).textContent(),
    rows: await drawnRowReadings(page, { bodies: false }),
  }));
}

/** Turns `filter` off, which puts the page back on Conversation. */
export async function releaseFilter(page: Page, filter: TreeView): Promise<void> {
  const name = PRESET_NAMES[filter];
  await expect(presetToggle(page, filter), `the ${name} control is drawn`).toBeVisible();
  await presetToggle(page, filter).click();
  await expectActivePreset(page, PRESET_NAMES.conversation);
}

/** One request the page sent under `/api/` that is no read: its method, its path, and its body,
 *  as JSON where it is JSON and as its text otherwise. */
export interface SentWrite {
  readonly body: unknown;
  readonly method: string;
  readonly path: string;
}

/** A request's body, as JSON where it is JSON. `postDataJSON` throws on a body that is not, and
 *  such a body is kept as its text. */
function bodyOf(request: Request): unknown {
  try {
    return request.postDataJSON();
  } catch {
    return request.postData();
  }
}

/** Keeps every request the page sends under `/api/` from now on that is no `GET`, in the order
 *  the page sent them. */
export function watchWrites(page: Page): readonly SentWrite[] {
  const writes: SentWrite[] = [];
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "GET" || !url.pathname.startsWith("/api/")) return;
    writes.push({ body: bodyOf(request), method: request.method(), path: url.pathname });
  });
  return writes;
}

/** The tree reads `holdTreeReads` holds. */
export interface HeldReads {
  /** How many requests of the tree route are held. */
  count(): number;
  /** Stops holding, and lets every held request go on to the server. */
  release(): Promise<void>;
}

/** Holds every request of issue `key`'s tree route the page sends from now on, its views' with
 *  it, until `release`. */
export async function holdTreeReads(page: Page, key: string): Promise<HeldReads> {
  const path = treePath(key);
  const held: Route[] = [];
  const matches = (url: URL): boolean => url.pathname === path;
  const hold = (route: Route): void => {
    held.push(route);
  };
  await page.route(matches, hold);
  return {
    count: () => held.length,
    release: async () => {
      await page.unroute(matches, hold);
      for (const route of held.splice(0)) {
        // A read the page gave up while it was held - a newer read of the same query takes its
        // place - has no request left to go on, and Playwright refuses to continue it.
        await route.continue().catch(() => undefined);
      }
    },
  };
}

/** Row 2's third decision: its block, the words it is written with, and the words the row
 *  changes them to. */
export const THIRD_DECISION = {
  block: "d-tie-again",
  reworded: "Tie the first statement once again",
  words: "Tie the first statement a second time",
} as const;

/** The id of issue `key`'s spec, the document its decisions are blocks of. */
async function specOf(key: string): Promise<string> {
  const spec = (await getIssue(key)).primary_artifact_id;
  if (spec === undefined || spec === null) throw new Error(`${key} holds no spec`);
  return spec;
}

/**
 * Writes row 2's third decision at the end of issue `key`'s spec, through the spec's edit route
 * (`POST /api/v1/artifacts/{id}/edits`), as the planner, so it is an agent's: a `:::decision`
 * block whose `shaped` names the first statement `r1`, so it waits for review too, and newest of
 * the issue's decisions. Answers the spec's id.
 */
export async function writeThirdDecision(key: string): Promise<string> {
  const spec = await specOf(key);
  const statement = specBlockRef(key, requirementBlockId(1));
  const markdown =
    `:::decision{#${THIRD_DECISION.block} shaped="${statement}"}\n` +
    `${THIRD_DECISION.words}\n\nShapes ${statement}.\n:::`;
  await editArtifact(spec, { ops: [{ after: "end", markdown, op: "insert" }] }, asPlanner);
  return spec;
}

/** Changes the third decision's words in the spec `spec`, as the planner. */
export async function rewordThirdDecision(spec: string): Promise<void> {
  await editArtifact(
    spec,
    { ops: [{ find: THIRD_DECISION.words, op: "replace", with: THIRD_DECISION.reworded }] },
    asPlanner
  );
}
