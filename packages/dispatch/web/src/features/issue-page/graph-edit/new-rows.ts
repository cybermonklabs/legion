import { hashKey, notifyManager, type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useSyncExternalStore } from "react";

import type { Actor, DisplayTree } from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import { editWords } from "../own-act";
import { ANSWER_LINES } from "../page-words";
import { outlineQuery, type TreeAnswer, type TreeAnswerNode } from "../tree-read/tree-read";
import { heldRead, type NewEntry, parentRefOf, placeNode, readOutlinesAgain } from "./node-writes";
import { type NewPlace, type PlaceLine, placingOf } from "./placement";
import { ownPathTo } from "./row-reveal";

/**
 * Graph's new rows (INFRA-5705's R3): a row New row below, New row under or Enter at the end of
 * a block made, drawn at once at the place `placement.ts` gave it, before the room holds its
 * block, and kept drawn there until the one `PATCH` that places its node is taken.
 *
 * A row is `drawn` until the reader's first character, which the row's editor writes into the
 * issue's own fragment as one paragraph under the block id the page chose for it
 * (`room-writes.ts`); nothing is stored before that, and a row left then is dropped
 * (`new-row-editing.ts`). From the first character it is `typed`, and waits for Graph's tree
 * read to hold its node by that block id with a `rank` and a `placement_seq`: the read the tree
 * artifact's version event sends (`api/event-refresh.ts`, which this adds no read to), and,
 * while the read Graph holds names the tree artifact among `owed_documents`, one read each 30 s
 * of the page's clock, the period of the block index's reconcile tick, for at most 2 minutes. The
 * first read within those 2 minutes that holds the node so sends the one `PATCH` to the row's
 * place (`placingOf`) on the node's chain (`node-writes.ts`' `placeNode`), `placing` until it
 * ends; a taken one leaves the node where its answer puts it in the held read, and the row leaves
 * this store once the outline has been told of that read, its node drawn there by the read
 * itself. A refused one leaves it where the server keeps it, with the row's line, and tells the
 * outline (`subscribeRefusedPlacings`), which keeps the focus on it. Past the 2 minutes the row is
 * `waited`: no read and no `PATCH` goes for it, then or later, and it says that the block is saved
 * and not yet placed, drawn where it was until a read holds the node, and then where the server
 * keeps it.
 *
 * The rows, their timers and their watch of the cache are one client's, as the node writes are
 * (`node-writes.ts`), by issue: the outline of an issue holds that issue's rows while it is drawn
 * (`useNewRows`), and once no outline of it is drawn every row of it is let go, each timer cleared
 * and nothing more read. No word of this file is a literal: each line is `ANSWER_LINES`'.
 */

/** The period of the block index's reconcile tick (`docs/block_index.go`), at which the page
 *  reads the tree again while its read owes the tree artifact's index (R3). */
export const NEW_ROW_READ_MS = 30_000;

/** How long from its first character a new row's node is waited for (R3). */
export const NEW_ROW_WAIT_MS = 120_000;

/** What a new row is made from: the entry that made it (Enter at the end is New row below), the
 *  block id the page chose for its block, the read id of the row it was made from and of the node
 *  it stands under (the issue's key for the issue), its place and its line (`placement.ts`), and
 *  the reader, its author. `parent` and `place.body` are the server's pointer, which the verdicts
 *  and the placing `PATCH` read; `under` is the place as the outline draws it: the id of the row
 *  it stands under (`TreeNode.id`), the parent of the own row of the node it was made from, or
 *  that row itself for New row under. The two need not name one node: an ask the asks read
 *  indexed from a block is drawn in that block's row, by its id and its read id
 *  (`tree-nodes.ts`' `treeIdOfNode`), while a criterion under it names the ask's own id. */
export interface NewRowAsk {
  readonly author?: Actor;
  readonly blockId: string;
  readonly entry: NewEntry;
  readonly from: string;
  readonly line?: PlaceLine;
  readonly parent: string;
  readonly place: NewPlace;
  readonly under: string;
}

/** How far a new row has come (above). */
export type NewRowStage = "drawn" | "placing" | "typed" | "waited";

/** One new row, with its words as the reader last left them and the key of the row that draws
 *  it (`rowKeyOf`), once it is edited there. */
export interface NewRow extends NewRowAsk {
  readonly rowKey?: string;
  readonly stage: NewRowStage;
  readonly words: string;
}

/** What the outline hears of a placing `PATCH` that was not taken: the node's read id and the
 *  row's block id. */
export type RefusedPlacing = (node: string, blockId: string) => void;

/** The new rows of one issue and their wait. */
interface IssueRows {
  rows: readonly NewRow[];
  /** The page's clock at each typed row's first character, by block id, while its node is
   *  waited for. */
  readonly waits: Map<string, number>;
  /** Each waited row's 2 minutes, by block id. */
  readonly deadlines: Map<string, number>;
  /** The next 30 s read, while a row waits. */
  poll: number | undefined;
  /** The watch of Graph's read in the cache, while a row waits. */
  unwatch: (() => void) | undefined;
  readonly refused: Set<RefusedPlacing>;
  /** How many outlines of the issue hold its rows, and the let-go a last one leaving schedules. */
  holders: number;
  release: number | undefined;
}

interface NewRowsState {
  readonly issues: Map<string, IssueRows>;
  readonly listeners: Set<() => void>;
}

const states = new WeakMap<QueryClient, NewRowsState>();

const NO_ROWS: readonly NewRow[] = [];

function stateOf(client: QueryClient): NewRowsState {
  let state = states.get(client);
  if (state === undefined) {
    state = { issues: new Map(), listeners: new Set() };
    states.set(client, state);
  }
  return state;
}

function issueOf(client: QueryClient, issue: string): IssueRows {
  const { issues } = stateOf(client);
  let held = issues.get(issue);
  if (held === undefined) {
    held = {
      deadlines: new Map(),
      holders: 0,
      poll: undefined,
      refused: new Set(),
      release: undefined,
      rows: NO_ROWS,
      unwatch: undefined,
      waits: new Map(),
    };
    issues.set(issue, held);
  }
  return held;
}

function notify(client: QueryClient): void {
  for (const listener of [...stateOf(client).listeners]) listener();
}

/** Changes the row `blockId` of `issue` to what `change` answers, or drops it where it answers
 *  undefined, and tells each hook. Nothing where the issue holds no such row. */
function change(
  client: QueryClient,
  issue: string,
  blockId: string,
  next: (row: NewRow) => NewRow | undefined
): void {
  const held = stateOf(client).issues.get(issue);
  const at = held?.rows.findIndex((row) => row.blockId === blockId) ?? -1;
  const row = held?.rows[at];
  if (held === undefined || row === undefined) return;
  const changed = next(row);
  if (changed === row) return;
  const rows = [...held.rows];
  if (changed === undefined) rows.splice(at, 1);
  else rows[at] = changed;
  held.rows = rows;
  notify(client);
}

/** The new rows of `issue`, as the store holds them now. */
export function newRowsOf(client: QueryClient, issue: string): readonly NewRow[] {
  return stateOf(client).issues.get(issue)?.rows ?? NO_ROWS;
}

/** Draws a new row of `issue` (`drawn`): nothing is written for it yet. */
export function drawNewRow(client: QueryClient, issue: string, ask: NewRowAsk): void {
  const held = issueOf(client, issue);
  held.rows = [...held.rows, { ...ask, stage: "drawn", words: "" }];
  notify(client);
}

/** Notes the key of the row that draws the new row `blockId` as its editing begins. */
export function nameNewRow(
  client: QueryClient,
  issue: string,
  blockId: string,
  rowKey: string
): void {
  change(client, issue, blockId, (row) => (row.rowKey === rowKey ? row : { ...row, rowKey }));
}

/** The new row `blockId`'s first character, `words`, is written in the room: it is `typed`, and
 *  its node is waited for from now, by the page's clock. */
export function typeNewRow(
  client: QueryClient,
  issue: string,
  blockId: string,
  words: string
): void {
  change(client, issue, blockId, (row) =>
    row.stage === "drawn" ? { ...row, stage: "typed", words } : row
  );
  if (newRowsOf(client, issue).find((row) => row.blockId === blockId)?.stage !== "typed") return;
  const held = issueOf(client, issue);
  held.waits.set(blockId, Date.now());
  held.deadlines.set(
    blockId,
    window.setTimeout(() => waitedOut(client, issue, blockId), NEW_ROW_WAIT_MS)
  );
  if (held.poll === undefined) {
    held.poll = window.setTimeout(() => readOwed(client, issue), NEW_ROW_READ_MS);
  }
  if (held.unwatch === undefined) {
    const hash = hashKey(outlineQuery(issue, "graph").queryKey);
    held.unwatch = client.getQueryCache().subscribe((event) => {
      if (
        event.type === "updated" &&
        event.action.type === "success" &&
        event.query.queryHash === hash
      ) {
        // Out of the cache's own notification, so the `PATCH` it may send goes from a settled
        // cache.
        queueMicrotask(() => placeHeld(client, issue));
      }
    });
  }
  queueMicrotask(() => placeHeld(client, issue));
}

/** The words the reader left in the new row the row `rowKey` draws, as their latest edit of its
 *  block left them: what the row draws while the read does not hold its node. */
export function noteNewRowWords(
  client: QueryClient,
  issue: string,
  rowKey: string,
  words: string
): void {
  const row = newRowsOf(client, issue).find((each) => each.rowKey === rowKey);
  if (row === undefined || row.stage === "drawn") return;
  change(client, issue, row.blockId, (each) => (each.words === words ? each : { ...each, words }));
}

/** Drops the new row `blockId`, and its wait. */
export function dropNewRow(client: QueryClient, issue: string, blockId: string): void {
  const held = stateOf(client).issues.get(issue);
  if (held !== undefined) stopWait(held, blockId);
  change(client, issue, blockId, () => undefined);
}

/** Has `listener` hear each placing `PATCH` of `issue` that was not taken, until the answer is
 *  called. */
export function subscribeRefusedPlacings(
  client: QueryClient,
  issue: string,
  listener: RefusedPlacing
): () => void {
  const { refused } = issueOf(client, issue);
  refused.add(listener);
  return () => {
    refused.delete(listener);
  };
}

/** Ends the wait of the row `blockId`, and the issue's reads and watch with the last wait. */
function stopWait(held: IssueRows, blockId: string): void {
  window.clearTimeout(held.deadlines.get(blockId));
  held.deadlines.delete(blockId);
  held.waits.delete(blockId);
  if (held.waits.size > 0) return;
  window.clearTimeout(held.poll);
  held.poll = undefined;
  held.unwatch?.();
  held.unwatch = undefined;
}

/** The 2 minutes of the row `blockId` are up: no read and no `PATCH` goes for it from now. */
function waitedOut(client: QueryClient, issue: string, blockId: string): void {
  const held = stateOf(client).issues.get(issue);
  if (held === undefined) return;
  stopWait(held, blockId);
  change(client, issue, blockId, (row) =>
    row.stage === "typed" ? { ...row, stage: "waited" } : row
  );
}

/** Whether `answer` names its tree artifact among the documents whose index it owes. */
function owesTree(answer: TreeAnswer): boolean {
  const { tree } = answer;
  return (
    typeof tree === "string" &&
    (answer.owed_documents ?? []).some((owed) => owed.artifact_id === tree)
  );
}

/** One of the 30 s reads: Graph's read of `issue` is read again where it still owes the tree
 *  artifact's index and a typed row is within its 2 minutes, and the next is set where one still
 *  will be. A read at the 2 minutes themselves would answer past them, so none goes then. */
function readOwed(client: QueryClient, issue: string): void {
  const held = stateOf(client).issues.get(issue);
  if (held === undefined) return;
  held.poll = undefined;
  const now = Date.now();
  const typed = [...held.waits.values()];
  if (!typed.some((at) => now < at + NEW_ROW_WAIT_MS)) return;
  const answer = heldRead(client, issue);
  if (answer !== undefined && owesTree(answer)) readOutlinesAgain(client, issue, ["graph"]);
  if (typed.some((at) => now + NEW_ROW_READ_MS < at + NEW_ROW_WAIT_MS)) {
    held.poll = window.setTimeout(() => readOwed(client, issue), NEW_ROW_READ_MS);
  }
}

/** The node of the new row `blockId` in `answer`: the block of the issue's own fragment whose
 *  block id is exactly that one. */
export function readNodeOf(answer: TreeAnswer, blockId: string): TreeAnswerNode | undefined {
  return (answer.nodes ?? []).find(
    (node) => node.kind === "block" && node.block_id === blockId && (node.document ?? null) === null
  );
}

/** Sends the one placing `PATCH` of each typed row of `issue` whose node Graph's read holds now
 *  with a rank and a `placement_seq`, within its 2 minutes. */
function placeHeld(client: QueryClient, issue: string): void {
  const held = stateOf(client).issues.get(issue);
  const answer = heldRead(client, issue);
  if (held === undefined || answer === undefined) return;
  const now = Date.now();
  for (const [blockId, at] of [...held.waits]) {
    const row = held.rows.find((each) => each.blockId === blockId);
    const node = readNodeOf(answer, blockId);
    if (now >= at + NEW_ROW_WAIT_MS || row?.stage !== "typed" || node === undefined) continue;
    const placing = placingOf({ held: answer, node: node.id, place: row.place });
    if (placing.kind !== "send") continue;
    stopWait(held, blockId);
    change(client, issue, blockId, (each) => ({ ...each, stage: "placing" }));
    void placeNode(client, issue, node.id, placing.body, row.entry).then((outcome) => {
      if (outcome.status !== "taken") {
        for (const heard of [...held.refused]) heard(node.id, blockId);
        change(client, issue, blockId, () => undefined);
        return;
      }
      // The answer is in the held read already, and TanStack Query tells the outline of it in
      // its notify manager's next flush, a timer on in a browser, where this store tells it at
      // once: so the row leaves in the flush after that one. Left at once, the outline would draw
      // the node where the read held it before the answer, for one render - under the issue,
      // another row's key for a row under any other node - which ends the editing in the row
      // (`IssueTree.tsx`), and the letters the reader types on go nowhere.
      notifyManager.schedule(() => change(client, issue, blockId, () => undefined));
    });
  }
}

/** Lets every row of `issue` go, with each of its timers and its watch. */
function endIssue(client: QueryClient, issue: string): void {
  const { issues } = stateOf(client);
  const held = issues.get(issue);
  if (held === undefined) return;
  for (const deadline of held.deadlines.values()) window.clearTimeout(deadline);
  window.clearTimeout(held.poll);
  held.unwatch?.();
  issues.delete(issue);
  notify(client);
}

/** Holds the rows of `issue` while an outline of it is drawn: the last one to go lets them go, a
 *  task later, so an outline drawn again in the same commit keeps them. */
function holdIssue(client: QueryClient, issue: string): () => void {
  const held = issueOf(client, issue);
  held.holders += 1;
  window.clearTimeout(held.release);
  held.release = undefined;
  return () => {
    held.holders -= 1;
    if (held.holders > 0) return;
    held.release = window.setTimeout(() => {
      if (held.holders === 0) endIssue(client, issue);
    }, 0);
  };
}

/**
 * The new rows of `issue` (`NewRow`), in the order they were made, under the page's query
 * client, held while the caller is drawn (`holdIssue`). None where `issue` is undefined: an
 * outline that is not Graph's.
 */
export function useNewRows(issue: string | undefined): readonly NewRow[] {
  const client = useQueryClient();
  const state = stateOf(client);
  const subscribe = useCallback(
    (listener: () => void) => {
      state.listeners.add(listener);
      return () => {
        state.listeners.delete(listener);
      };
    },
    [state]
  );
  const rows = useSyncExternalStore(subscribe, () =>
    issue === undefined ? NO_ROWS : (state.issues.get(issue)?.rows ?? NO_ROWS)
  );
  useEffect(() => (issue === undefined ? undefined : holdIssue(client, issue)), [client, issue]);
  return rows;
}

/** Notes, at each edit of the reader's in a row's editor (`useNodeEditing.ts`' `onOwnEdit`), the
 *  words it left in the block of a new row of `issue` the row draws (`noteNewRowWords`). */
export function useNewRowWords(
  issue: string | undefined
): (row: string, block: DisplayTree) => void {
  const client = useQueryClient();
  return useCallback(
    (row, block) => {
      if (issue !== undefined) noteNewRowWords(client, issue, row, editWords(block));
    },
    [client, issue]
  );
}

/**
 * `root`, the tree Graph draws, with each new row of `rows` drawn at its place (R3): under the
 * row of its `under`, right after the child its place names, or first where it names none -
 * the node the read holds by the row's block id moved there from where the server keeps it, or,
 * while the read holds none, a prose row of the reader's own drawn from the row alone, with the
 * words they left in it. A row whose 2 minutes are up is drawn where the server keeps it once a
 * read holds its node. A row whose parent the tree does not draw is left out. The row keeps its
 * key throughout - the path to its parent and its block id, the node's id in the tree
 * (`tree-nodes.ts`' `treeIdOfNode`) - so it is the one row from the first frame to the placed
 * node's, and the caret in it stays.
 */
export function withNewRows(root: TreeNode, rows: readonly NewRow[]): TreeNode {
  let drawn = root;
  for (const row of rows) {
    const node = ownPathTo(drawn, row.blockId)?.at(-1);
    const read = node?.readId === undefined ? undefined : node;
    if (row.stage === "waited" && read !== undefined) continue;
    const rest = read === undefined ? drawn : (withoutChild(drawn, row.blockId) ?? drawn);
    drawn = withChild(rest, row.under, row.place.after, read ?? rowNode(row)) ?? drawn;
  }
  return drawn;
}

/** The prose row a new row draws while the read holds no node of it. */
function rowNode(row: NewRow): TreeNode {
  return {
    alsoUnder: [],
    ...(row.author === undefined ? {} : { author: row.author }),
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id: row.blockId,
    question: false,
    replaced: false,
    text: row.words,
    transcluded: false,
    type: "prose",
    waiting: false,
  };
}

/** `node` with its own child `id`, at any depth, left out, and the counts above it lowered;
 *  undefined where it holds none. */
function withoutChild(node: TreeNode, id: string): TreeNode | undefined {
  for (const [at, child] of node.children.entries()) {
    if (child.transcluded) continue;
    const inner = child.id === id ? null : withoutChild(child, id);
    if (inner === undefined) continue;
    const children = [...node.children];
    if (inner === null) children.splice(at, 1);
    else children[at] = inner;
    const left = inner === null ? child.held + 1 : child.held - inner.held;
    return { ...node, children, held: node.held - left };
  }
  return undefined;
}

/** `node` with `child` under its own node `under` (`TreeNode.id`), at any depth, right after that
 *  node's child of read id `after` - last where it holds none such - or first where `after` is
 *  null, and the counts above it raised; undefined where it holds no such node. */
function withChild(
  node: TreeNode,
  under: string,
  after: string | null,
  child: TreeNode
): TreeNode | undefined {
  const raised = node.held + child.held + 1;
  if (node.id === under) {
    const children = [...node.children];
    const before = children.findIndex(
      (each) => !each.transcluded && (each.readId ?? each.id) === after
    );
    children.splice(after === null ? 0 : before === -1 ? children.length : before + 1, 0, child);
    return { ...node, children, held: raised };
  }
  for (const [at, each] of node.children.entries()) {
    if (each.transcluded) continue;
    const inner = withChild(each, under, after, child);
    if (inner === undefined) continue;
    const children = [...node.children];
    children[at] = inner;
    return { ...node, children, held: raised };
  }
  return undefined;
}

/**
 * `answer`, Graph's held read, as the node menu's verdicts and the editor's keys read it
 * (`placement.ts`): with a node for each new row of `rows` whose block it does not hold yet,
 * under the row's parent, named by the row's block id, unranked and with no `placement_seq`, so
 * every entry that writes a node's place is grey on it. A row whose parent the read lacks adds
 * none.
 */
export function heldWithNewRows(answer: TreeAnswer, rows: readonly NewRow[]): TreeAnswer {
  const nodes = answer.nodes ?? [];
  const added = rows.flatMap((row): TreeAnswerNode[] => {
    const parent = nodes.find((node) => node.id === row.parent);
    if (parent === undefined || readNodeOf(answer, row.blockId) !== undefined) return [];
    return [
      {
        attributes: null,
        author: row.author === undefined ? null : [row.author],
        block_id: row.blockId,
        id: row.blockId,
        kind: "block",
        made_at: null,
        parent: parentRefOf(parent),
        placement_seq: null,
        rank: null,
        role: "tree",
        state: "live",
        supertag: "block",
        text: row.words,
        text_tree: { content: [{ type: "paragraph" }], type: "doc" },
        visibility: "project",
      },
    ];
  });
  return added.length === 0 ? answer : { ...answer, nodes: [...nodes, ...added] };
}

/** Whether the node of read id `node` is a new row's of `rows` that no `PATCH` has placed: one
 *  named by its block id while the read does not hold it, or the node `answer` holds by that
 *  block id. */
export function newRowUnplaced(
  rows: readonly NewRow[],
  answer: TreeAnswer | undefined,
  node: string
): boolean {
  return rows.some(
    (row) =>
      row.blockId === node || (answer !== undefined && readNodeOf(answer, row.blockId)?.id === node)
  );
}

/** The line the row of the node `id` (`TreeNode.id`) draws as a new row (R3): that its block is
 *  saved and not yet placed, past its 2 minutes, else that it stands after the spec's blocks,
 *  where its place says so; undefined for any other row. */
export function newRowLine(rows: readonly NewRow[], id: string): string | undefined {
  const row = rows.find((each) => each.blockId === id);
  if (row === undefined) return undefined;
  if (row.stage === "waited") return ANSWER_LINES.saved_not_placed;
  return row.line === undefined ? undefined : ANSWER_LINES[row.line];
}
