import type { DeleteCountMoved, NodeDeleted, NodeMoved } from "@legion/contracts";
import { hashKey, type QueryClient, useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo, useSyncExternalStore } from "react";

import {
  ApiError,
  api,
  REQUEST_DEADLINE_MS,
  type WriteRequest,
  withIfMatch,
  writeRequest,
} from "../../../api/client";
import { type Query, routeUrl } from "../../../api/paths";
import { refreshQueries } from "../../../api/query-refresh";
import { newSendKey } from "../../../lib/send-key";
import { plainWrite, writesOf } from "../../writes/write-store";
import { ANSWER_LINES, CONTROL_LABELS } from "../page-words";
import { nextTreeCursor } from "../tree-read/tree-nodes";
import {
  type Outline,
  outlineQuery,
  readNextTreePage,
  type TreeAnswer,
  type TreeAnswerNode,
} from "../tree-read/tree-read";
import {
  bandOf,
  type Entry,
  type GreyReason,
  type MoveBody,
  type PlacementRow,
  placementOf,
} from "./placement";

/**
 * The writes of Graph's node menu (R4, R6, R7 and R8 of INFRA-5705's spec): the moves of a node,
 * its delete and the delete's dry run, the line a row draws for a write that did not go, and the
 * re-read of the outline after them. The menu (`RowMenu.tsx`) and the editor's keys call these
 * same writes.
 *
 * Every move and every delete is one write of the write store (`Writes.run`), kept for a reload
 * with its request, its `If-Match` among it (`withIfMatch`), on the node's own line of the store
 * (`node:<key>:<read id>`), and holding the issue's tree reads while it is out (`issue`). The dry
 * run is no write: it goes with `api.send` beside the store, is kept nowhere, holds nothing and
 * sends no `If-Match` (`dryRunDelete`).
 *
 * The acts of one node wait in one chain, in press order, whichever hook pressed them. A move's
 * body is made by `placement.ts` as the move's turn comes, from Graph's held read with each
 * earlier answer of the chain written into it (`movedIn`): the answer's `parent`, `after` and
 * `placement_seq`, and nothing else, so every other field of the node stays as the read answered
 * it. A move whose entry is grey by then is dropped with its reason, and so is each move after
 * it; a refusal of an act drops each move after it too, and the row's line says why and how many
 * moves were not sent. A delete waits its turn the same way and goes with the `placement_seq`
 * the held read holds then. Where the body, or the node itself, stands on a page the page has not
 * loaded, that page is read first (`readNextTreePage`). No other read goes between two acts of a
 * chain: the next act goes out within the answer's own reaction, before the write store's hold on
 * the tree reads ends (`subscribeTreeFreshness`).
 *
 * A new row's node (R3) is placed the same way, on its own chain (`placeNode`): one `PATCH` of
 * the place its entry gave it, under the `placement_seq` the read that first held it ranked
 * answered, its answer written into the held read as a move's is (`new-rows.ts`).
 *
 * Once a node's chain is empty after a taken act, or after a refusal that says the page read a
 * stale tree or a node that is gone (412, `409 NODE_GONE`, 404), the page reads each outline read
 * it holds again, once, its loaded pages included (`readOutlinesAgain`). After any other refusal
 * it reads nothing. A retype, a write of the room that sends no request, has them read again once
 * it is taken, or once its node's chain is empty where one waits (`readAfterRoomWrite`). No word
 * of this file is a literal: each is `ANSWER_LINES`' or `CONTROL_LABELS`', from `page-words-r.ts`.
 */

/** The route of a node's move and of its delete (dispatch://INFRA-1253 R7). */
const NODE_ROUTE = "/api/v1/issues/{key}/nodes/{id}";

/** What the delete takes in its query: `dry_run`, or the `expected_count` the confirm named. */
type DeleteQuery = Query<typeof NODE_ROUTE, "delete">;

/** The refusal codes that do more than draw their words (R6, R8): a node that is gone, a
 *  container a move or a delete would leave empty, and a delete whose count has moved. */
const NODE_GONE = "NODE_GONE";
const CONTAINER_WOULD_EMPTY = "CONTAINER_WOULD_EMPTY";
const DELETE_COUNT_MOVED = "DELETE_COUNT_MOVED";

/** The statuses that say the page's tree is stale: the node is not where the page read it
 *  (412), or the reader may not see it (404). */
const PRECONDITION_FAILED_STATUS = 412;
const NOT_FOUND_STATUS = 404;

/** The page's words by key, for the words of a refusal code: `refused_` and the code in lower
 *  case. The words table is the one list of the codes that have words of their own. */
const WORDS: Readonly<Record<string, string>> = ANSWER_LINES;

/** The kind a refusal names a container by, from the type of the first block its node's text
 *  holds: a list item, of a bulleted or a numbered list, or a callout. */
const CONTAINER_KINDS: Readonly<Record<string, string>> = {
  bullet_list: CONTROL_LABELS.container_list_item,
  callout: CONTROL_LABELS.container_callout,
  list_item: CONTROL_LABELS.container_list_item,
  ordered_list: CONTROL_LABELS.container_list_item,
};

/** The outline reads the page may hold (`Outline`), each read again after the writes. */
const OUTLINES: readonly Outline[] = ["graph", "conversation"];

/** The four entries that move a node (R4). */
export type MoveEntry = Extract<Entry, "indent" | "move_down" | "move_up" | "outdent">;

/** The two entries that make a new row (R3), whose node one `PATCH` places (`placeNode`). */
export type NewEntry = Extract<Entry, "new_below" | "new_under">;

/** The line a row draws for a write that did not go (R8). */
export interface RefusalLine {
  /** The whole line, in the page's words. */
  readonly text: string;
  /** The read id of the row a `409 CONTAINER_WOULD_EMPTY` links to, where the held tree shows the
   *  node's parent as a list item or a callout. */
  readonly container?: string;
  /** Whether the page reads the tree again: a 412, a `409 NODE_GONE`, a 404. */
  readonly reads: boolean;
  /** Whether the row leaves where that read lacks it: a `409 NODE_GONE`, a 404. */
  readonly gone: boolean;
  /** Whether a press on the line tries the act again: the fallback line alone. */
  readonly retry: boolean;
}

/** What `refusalLine` names a container from: the held outline read, and the read id of the
 *  node that was written. */
export interface RefusalContext {
  readonly held: TreeAnswer;
  readonly node: string;
}

/** The line of one row (`NodeWrites.lines`): `refusalLine`'s, then how many moves were dropped,
 *  with the act made again (`again`) where it is the fallback line, answering how the act made
 *  again ended, as `move` and `remove` do: a delete's `count_moved` opens the confirm again. */
export interface NodeLine {
  readonly text: string;
  readonly container?: string;
  readonly again?: () => Promise<MoveOutcome | DeleteOutcome>;
}

/** How a move ended: taken; refused, sent or with its page unread; or dropped unsent, with the
 *  reason its entry turned grey, or with none behind a refusal or for a node the held read no
 *  longer holds. */
export type MoveOutcome =
  | { readonly status: "taken" }
  | { readonly status: "refused"; readonly line: RefusalLine }
  | { readonly status: "dropped"; readonly reason?: GreyReason };

/** How a confirmed delete ended: taken; refused with the count the delete takes now, which the
 *  confirm names when it opens again (`409 DELETE_COUNT_MOVED`); refused; or dropped unsent, for
 *  a node the held read no longer holds. */
export type DeleteOutcome =
  | { readonly status: "taken" }
  | { readonly status: "count_moved"; readonly count: DeleteCountMoved["count"] }
  | { readonly status: "refused"; readonly line: RefusalLine }
  | { readonly status: "dropped" };

/** What the delete's dry run answered (R6): the count the delete takes; a refusal, with its
 *  line; or no count read (a 5xx, any other code, no answer), which a press asks for again. */
export type DeleteCount =
  | { readonly status: "counted"; readonly count: NodeDeleted["count"] }
  | { readonly status: "refused"; readonly line: RefusalLine }
  | { readonly status: "failed" };

/** The issue the writes are for, as the page holds it. */
export interface NodeWritesIssue {
  readonly key: string;
  readonly status: string;
  readonly zoomRoot?: string;
}

/** The writes of Graph's node menu for one issue (`useNodeWrites`). */
export interface NodeWrites {
  /** Queues `entry` for the node of read id `node`, from `row` as `placementOf` takes it, and
   *  answers once it is taken, refused or dropped. */
  readonly move: (node: string, entry: MoveEntry, row?: PlacementRow) => Promise<MoveOutcome>;
  /** The delete's dry run of the node (`dryRunDelete`). Once `signal` aborts it rejects with the
   *  signal's reason, and its answer is dropped. A refusal that says the node is gone reads the
   *  outline reads again. */
  readonly count: (node: string, signal?: AbortSignal) => Promise<DeleteCount>;
  /** The confirmed delete of the node: sent once each act queued before it has ended, with the
   *  newest `placement_seq` and `expectedCount` as confirmed. It is never sent again by itself. */
  readonly remove: (node: string, expectedCount: number) => Promise<DeleteOutcome>;
  /** The line of each row of the issue a refusal or a drop wrote, by read id, until the next act
   *  of that node. */
  readonly lines: ReadonlyMap<string, NodeLine>;
  /** Draws `reason`, why a move key's entry is grey for the node of read id `node`, as the line
   *  of its rows (R4: at a grey edge a key writes nothing, and the row shows the entry's reason),
   *  until the node's next act. Nothing is sent or queued. */
  readonly grey: (node: string, reason: GreyReason) => void;
}

/** What a move carries from its press to its turn. */
interface MovePress {
  readonly entry: MoveEntry;
  readonly row: PlacementRow | undefined;
  readonly status: string;
  readonly zoomRoot: string | undefined;
}

interface MoveAct extends MovePress {
  readonly kind: "move";
  readonly done: (outcome: MoveOutcome) => void;
}

interface DeleteAct {
  readonly kind: "delete";
  readonly count: number;
  readonly done: (outcome: DeleteOutcome) => void;
}

/** The one `PATCH` that places a new row's node where R3 puts it (`new-rows.ts`), from the entry
 *  that made the row: its body is the place `placement.ts` gave the row as it was made, and it
 *  goes under the node's `placement_seq` as the held read holds it at its turn. */
interface PlaceAct {
  readonly kind: "place";
  readonly body: MoveBody;
  readonly entry: NewEntry;
  readonly done: (outcome: MoveOutcome) => void;
}

type Act = MoveAct | DeleteAct | PlaceAct;

/** The acts of one node of one issue, in press order, the first of them the one whose turn it
 *  is. */
interface Chain {
  readonly issue: string;
  readonly node: string;
  readonly acts: Act[];
  /** Whether an act of the chain was taken, or refused with an answer that says the tree is
   *  stale: what reads the outline reads again once the chain is empty. */
  reads: boolean;
}

/** What the act whose turn it is does. */
type Next =
  | { readonly kind: "send"; readonly send: () => void }
  | { readonly kind: "read_next" }
  | { readonly kind: "drop"; readonly reason: GreyReason | undefined };

/** A container a refusal names: its row's read id, its kind and its first line. */
interface Container {
  readonly id: string;
  readonly kind: string;
  readonly line: string;
}

/** The chains and the lines of one client, as `tree-read.ts` keeps the pages out: one for the
 *  app, whichever hook pressed an act. */
interface NodeWriteState {
  readonly chains: Map<string, Chain>;
  readonly lines: Map<string, ReadonlyMap<string, NodeLine>>;
  readonly listeners: Set<() => void>;
}

const states = new WeakMap<QueryClient, NodeWriteState>();

const NO_LINES: ReadonlyMap<string, NodeLine> = new Map();
const TAKEN = { status: "taken" } as const;
const DROPPED = { status: "dropped" } as const;
const FAILED = { status: "failed" } as const;
const READ_NEXT: Next = { kind: "read_next" };
const GONE: Next = { kind: "drop", reason: undefined };

function stateOf(client: QueryClient): NodeWriteState {
  let state = states.get(client);
  if (state === undefined) {
    state = { chains: new Map(), lines: new Map(), listeners: new Set() };
    states.set(client, state);
  }
  return state;
}

/**
 * The line a row draws for a write that did not go (R8), from what the write store holds of it:
 * an `ApiError`, a `NetworkError`, a `RequestTimeoutError`, or any other error. Each refusal code
 * whose words the page holds (`ANSWER_LINES`, `refused_` and the code in lower case) draws them;
 * any other code, a 5xx and no answer draw the line that the change was not made, with the code
 * in brackets where there is one, and a press on it tries the act again. A
 * `409 CONTAINER_WOULD_EMPTY` names the container by its kind and its first line, with its read
 * id to link to, where `context`'s held read shows the node's parent as a list item or a callout,
 * and else quotes the server's message after the code's words.
 */
export function refusalLine(error: unknown, context?: RefusalContext): RefusalLine {
  const code = error instanceof ApiError && error.status < 500 ? error.code : undefined;
  const words = code === undefined ? undefined : wordsOf(code);
  if (!(error instanceof ApiError) || words === undefined) return notMade(error);
  const gone = error.status === NOT_FOUND_STATUS || code === NODE_GONE;
  const reads = gone || error.status === PRECONDITION_FAILED_STATUS;
  if (code !== CONTAINER_WOULD_EMPTY) return { gone, reads, retry: false, text: words };
  const container = context === undefined ? undefined : containerOf(context);
  if (container === undefined) {
    return { gone, reads, retry: false, text: `${words} “${error.message}”` };
  }
  const text = `${ANSWER_LINES.container_empties} ${container.kind}, ${container.line}`;
  return { container: container.id, gone, reads, retry: false, text };
}

/** The page's words of refusal `code`, or undefined where it holds none. */
function wordsOf(code: string): string | undefined {
  const key = `refused_${code.toLowerCase()}`;
  return Object.hasOwn(WORDS, key) ? WORDS[key] : undefined;
}

/** The fallback line: the change was not made, with the code where the refusal names one. */
function notMade(error: unknown): RefusalLine {
  const code = error instanceof ApiError ? error.code : undefined;
  const text =
    code === undefined || code === ""
      ? ANSWER_LINES.not_made
      : `${ANSWER_LINES.not_made} (${ANSWER_LINES.not_made_code} ${code})`;
  return { gone: false, reads: false, retry: true, text };
}

/** The container the held read shows the node under, as a container refusal names it; undefined
 *  where the parent is no list item and no callout, or the read gives no first line of it. */
function containerOf({ held, node }: RefusalContext): Container | undefined {
  const parent = held.nodes?.find((each) => each.id === node)?.parent;
  if (parent === undefined || parent === null || parent.first_line === "") return undefined;
  const top = topBlockOf(held.nodes?.find((each) => each.id === parent.id)) ?? "";
  const kind = Object.hasOwn(CONTAINER_KINDS, top) ? CONTAINER_KINDS[top] : undefined;
  return kind === undefined ? undefined : { id: parent.id, kind, line: parent.first_line };
}

/** The type of the first block of `node`'s text (`text_tree`), or undefined. */
function topBlockOf(node: TreeAnswerNode | undefined): string | undefined {
  const content = node?.text_tree?.content;
  if (!Array.isArray(content)) return undefined;
  const blocks: readonly { readonly type?: unknown }[] = content;
  const type = blocks[0]?.type;
  return typeof type === "string" ? type : undefined;
}

/** The `DELETE` of the node of read id `node` with `query`, as the contract types it. A node R2
 *  admits is a block, which the route names by its read id. */
function deleteRequest(key: string, node: string, query: DeleteQuery): WriteRequest<NodeDeleted> {
  const params = { id: node, key };
  return {
    ...writeRequest("delete", NODE_ROUTE, params),
    path: routeUrl(NODE_ROUTE, params, query),
  };
}

/** `request` under the node's `placement_seq` as the held read holds it (`withIfMatch`); with no
 *  precondition where the read gives it none, which the route refuses with words of its own. */
function placed<T>(request: WriteRequest<T>, node: TreeAnswerNode): WriteRequest<T> {
  const seq = node.placement_seq;
  return seq === null || seq === undefined ? request : withIfMatch(request, seq);
}

/**
 * The delete's dry run of the node of read id `node` (R6): `DELETE ?dry_run=true` with
 * `api.send`, outside the write store, so it is kept nowhere, holds no tree read and shows in no
 * write line. It sends no `If-Match`, goes under a key of its own, which a `DELETE` never sends,
 * and gives up after the read deadline. Once `signal` aborts - the menu closed - it rejects with
 * the signal's reason, and an answer that comes later is dropped.
 */
export function dryRunDelete(
  key: string,
  node: string,
  signal?: AbortSignal
): Promise<NodeDeleted> {
  if (signal?.aborted === true) return Promise.reject(signal.reason);
  const answer = api.send(deleteRequest(key, node, { dry_run: true }), {
    deadlineMs: REQUEST_DEADLINE_MS,
    key: newSendKey(),
  });
  if (signal === undefined) return answer;
  const { promise, reject, resolve } = Promise.withResolvers<NodeDeleted>();
  const drop = () => reject(signal.reason);
  signal.addEventListener("abort", drop, { once: true });
  void answer.then(resolve, reject).finally(() => signal.removeEventListener("abort", drop));
  return promise;
}

/** Graph's held outline read of `issue`, which the moves are made on. */
function heldRead(client: QueryClient, issue: string): TreeAnswer | undefined {
  return client.getQueryData<TreeAnswer>(outlineQuery(issue, "graph").queryKey);
}

/**
 * Reads each outline read of `issue` the cache holds again, once, its loaded pages and older
 * comment pages included (`readTree`), and marks it stale: each of `outlines`, both by default. A
 * read a write of the issue holds (`subscribeTreeFreshness`, which leaves it no enabled observer)
 * is only marked: the hold owes it, and reads it once the last write settles. The scheduler reads
 * an observed outline after any older request settles, so that request's answer cannot pay for
 * the refresh. A new row's wait reads Graph's alone (`new-rows.ts`).
 */
export function readOutlinesAgain(
  client: QueryClient,
  issue: string,
  outlines: readonly Outline[] = OUTLINES
): void {
  const cache = client.getQueryCache();
  for (const outline of outlines) {
    const options = outlineQuery(issue, outline);
    const { queryKey } = options;
    const query = cache.find({ exact: true, queryKey });
    if (query === undefined) continue;
    void client.invalidateQueries({ exact: true, queryKey, refetchType: "none" });
    if (query.isActive()) refreshQueries(client, [{ exact: true, queryKey }]);
    else if (!query.isDisabled()) void client.fetchQuery(options).catch(() => undefined);
  }
}

/**
 * R7's re-read after a write of the room that was taken (R5's retype of the node of read id
 * `node`), which sent no request and so ends no chain: each outline read of `issue` is read again
 * once (`readOutlinesAgain`), now where no act of the node waits in its chain, else once that
 * chain is empty, as its own taken acts are.
 */
export function readAfterRoomWrite(client: QueryClient, issue: string, node: string): void {
  const chain = stateOf(client).chains.get(hashKey([issue, node]));
  if (chain === undefined) readOutlinesAgain(client, issue);
  else chain.reads = true;
}

/** Whether `address`, a node as the node routes answer it (a block by its read id, any other node
 *  by `<kind>/<read id>`, as `placement.ts` names one in a body), names `node`. */
function names(node: TreeAnswerNode, address: string): boolean {
  return node.id === address || `${node.kind}/${node.id}` === address;
}

/** The index after the subtree of `nodes[at]` in the walk: the run of nodes after it that stand
 *  under it, which the walk keeps together, as every parent comes before its children. */
function subtreeEnd(nodes: readonly TreeAnswerNode[], at: number): number {
  const inside = new Set([nodes[at]?.id]);
  let end = at + 1;
  for (; end < nodes.length; end += 1) {
    const node = nodes[end];
    if (node === undefined || node.parent === null || !inside.has(node.parent.id)) break;
    inside.add(node.id);
  }
  return end;
}

/** The parent reference the read gives `parent` on a node under it; where it holds none, one made
 *  of the parent's own fields, with no first line, which the read gives only on a child. */
function parentRef(
  nodes: readonly TreeAnswerNode[],
  parent: TreeAnswerNode
): NonNullable<TreeAnswerNode["parent"]> {
  const held = nodes.find((each) => each.parent?.id === parent.id)?.parent;
  if (held !== undefined && held !== null) return held;
  const { id, issue, kind, supertag } = parent;
  return { first_line: "", id, kind, supertag, ...(issue === undefined ? {} : { issue }) };
}

/** The index in `rest` a subtree of `band` goes to under the parent at `rest[from]`: right after
 *  the subtree of the child `after` names; first in its band where `after` is null, before the
 *  first ranked child of that band or a later one (dispatch://INFRA-1253 R7); and last under the
 *  parent where no child it holds is the one `after` names, which stands past the loaded pages. */
function placeIn(
  rest: readonly TreeAnswerNode[],
  from: number,
  after: string | null,
  band: number
): number {
  const parent = rest[from]?.id;
  const end = subtreeEnd(rest, from);
  for (let at = from + 1; at < end; at += 1) {
    const child = rest[at];
    if (child === undefined || child.parent?.id !== parent) continue;
    if (after !== null && names(child, after)) return subtreeEnd(rest, at);
    if (after === null && typeof child.rank === "string" && bandOf(child) >= band) return at;
  }
  return end;
}

/** `held` with the taken move of `node` written into it: the node's subtree under the answer's
 *  `parent`, after its `after`, in the walk's order, so each later body reads it there, and the
 *  node's `placement_seq` the answer's. Where the read holds no such parent, the node keeps its
 *  place and takes the sequence alone. */
function movedIn(held: TreeAnswer, node: string, answer: NodeMoved): TreeAnswer {
  const nodes = held.nodes ?? [];
  const at = nodes.findIndex((each) => each.id === node);
  const own = nodes[at];
  if (own === undefined) return held;
  const renewed = { ...own, placement_seq: answer.placement_seq };
  const end = subtreeEnd(nodes, at);
  const rest = [...nodes.slice(0, at), ...nodes.slice(end)];
  const from = rest.findIndex((each) => names(each, answer.parent));
  const parent = rest[from];
  if (parent === undefined) {
    return { ...held, nodes: [...nodes.slice(0, at), renewed, ...nodes.slice(at + 1)] };
  }
  const ref = own.parent?.id === parent.id ? own.parent : parentRef(rest, parent);
  const subtree = [{ ...renewed, parent: ref }, ...nodes.slice(at + 1, end)];
  const to = placeIn(rest, from, answer.after, bandOf(own));
  return { ...held, nodes: [...rest.slice(0, to), ...subtree, ...rest.slice(to)] };
}

/** Writes the taken move of `node` into Graph's held read of `issue` (`movedIn`), keeping the
 *  stale mark an event or a write put on it, as `readNextTreePage` does. */
function placeHeld(client: QueryClient, issue: string, node: string, answer: NodeMoved): void {
  const { queryKey } = outlineQuery(issue, "graph");
  const held = client.getQueryData<TreeAnswer>(queryKey);
  if (held === undefined) return;
  const stale = client.getQueryCache().find({ exact: true, queryKey })?.state.isInvalidated;
  client.setQueryData(queryKey, movedIn(held, node, answer));
  if (stale === true) {
    void client.invalidateQueries({ exact: true, queryKey, refetchType: "none" });
  }
}

/** Sets the line of `node` on `issue`'s rows, or clears it, and tells each hook. */
function setLine(
  client: QueryClient,
  issue: string,
  node: string,
  line: NodeLine | undefined
): void {
  const state = stateOf(client);
  const now = state.lines.get(issue) ?? NO_LINES;
  if (line === undefined && !now.has(node)) return;
  const next = new Map(now);
  if (line === undefined) next.delete(node);
  else next.set(node, line);
  state.lines.set(issue, next);
  for (const listener of [...state.listeners]) listener();
}

/** Writes the row's line: `lead`, then how many moves were dropped, where any were. */
function writeLine(
  client: QueryClient,
  chain: Chain,
  lead: string | undefined,
  dropped: number,
  extra: Omit<NodeLine, "text"> = {}
): void {
  const count =
    dropped === 0
      ? undefined
      : `${dropped} ${dropped === 1 ? ANSWER_LINES.dropped_move : ANSWER_LINES.dropped_moves}`;
  const text = [lead, count].filter((part) => part !== undefined).join(" ");
  if (text !== "") setLine(client, chain.issue, chain.node, { ...extra, text });
}

/** Takes every move queued in `chain` out of it; its deletes stay. */
function takeMoves(chain: Chain): MoveAct[] {
  const moves = chain.acts.filter((act): act is MoveAct => act.kind === "move");
  const rest = chain.acts.filter((act) => act.kind !== "move");
  chain.acts.splice(0, chain.acts.length, ...rest);
  return moves;
}

/** The act whose turn it is was refused, sent or not: it ends with `error`'s line, every move
 *  after it is dropped, and the row says why and how many. */
function refuse(client: QueryClient, chain: Chain, error: unknown): void {
  const act = chain.acts.shift();
  if (act === undefined) return;
  const held = heldRead(client, chain.issue);
  const line = refusalLine(error, held === undefined ? undefined : { held, node: chain.node });
  const dropped = takeMoves(chain);
  act.done({ line, status: "refused" });
  for (const move of dropped) move.done(DROPPED);
  if (line.reads) chain.reads = true;
  const again =
    act.kind === "move"
      ? () => pressMove(client, chain.issue, chain.node, act)
      : act.kind === "place"
        ? () => placeNode(client, chain.issue, chain.node, act.body, act.entry)
        : () => pressDelete(client, chain.issue, chain.node, act.count);
  writeLine(client, chain, line.text, dropped.length, {
    ...(line.container === undefined ? {} : { container: line.container }),
    ...(line.retry ? { again } : {}),
  });
}

/** The act whose turn it is goes unsent: its entry is grey for `reason`, or the held read no
 *  longer holds its node (no reason). It and every move after it are dropped; a new row's
 *  placing (`PlaceAct`) answers as a move does. */
function drop(client: QueryClient, chain: Chain, reason: GreyReason | undefined): void {
  const act = chain.acts.shift();
  if (act === undefined) return;
  const dropped = [...(act.kind === "delete" ? [] : [act]), ...takeMoves(chain)];
  if (act.kind === "delete") act.done(DROPPED);
  const outcome: MoveOutcome = reason === undefined ? DROPPED : { reason, status: "dropped" };
  for (const move of dropped) move.done(outcome);
  const lead = reason === undefined ? undefined : ANSWER_LINES[reason];
  writeLine(client, chain, lead, dropped.length);
}

/** What the act whose turn it is does, from the held read as it stands now. */
function nextOf(client: QueryClient, chain: Chain, act: Act): Next {
  const held = heldRead(client, chain.issue);
  if (held === undefined) return GONE;
  const node = held.nodes?.find((each) => each.id === chain.node);
  if (node === undefined) return nextTreeCursor(held) === undefined ? GONE : READ_NEXT;
  if (act.kind === "delete") {
    const query = { expected_count: act.count };
    const request = placed(deleteRequest(chain.issue, chain.node, query), node);
    return { kind: "send", send: () => sendDelete(client, chain, act, request) };
  }
  const params = { id: chain.node, key: chain.issue };
  if (act.kind === "place") {
    const request = placed(writeRequest("patch", NODE_ROUTE, params, act.body), node);
    const label = CONTROL_LABELS[`edit_${act.entry}`];
    return { kind: "send", send: () => sendMove(client, chain, act, request, label) };
  }
  const verdict = placementOf({
    held,
    node: chain.node,
    row: act.row,
    status: act.status,
    zoomRoot: act.zoomRoot,
  })[act.entry];
  switch (verdict.kind) {
    case "grey":
      return { kind: "drop", reason: verdict.reason };
    case "read_next":
      return READ_NEXT;
    case "move": {
      const request = placed(writeRequest("patch", NODE_ROUTE, params, verdict.body), node);
      const label = CONTROL_LABELS[`edit_${act.entry}`];
      return { kind: "send", send: () => sendMove(client, chain, act, request, label) };
    }
    default:
      throw new Error(`placement answered ${verdict.kind} for ${act.entry}`);
  }
}

/**
 * Runs `chain` from the act whose turn it is: drops each act that goes unsent, and sends the
 * first that goes, or first reads the page it needs, and is run again from that answer's own
 * reaction. Nothing awaits between an answer and the next act going out, so no tree read the
 * store's hold would let go is sent between two acts. Once the chain is empty it leaves the
 * client, and reads the outline reads again where an act of it asked for that.
 */
function advance(client: QueryClient, chain: Chain): void {
  for (let act = chain.acts[0]; act !== undefined; act = chain.acts[0]) {
    try {
      const next = nextOf(client, chain, act);
      if (next.kind === "drop") {
        drop(client, chain, next.reason);
        continue;
      }
      if (next.kind === "send") {
        next.send();
        return;
      }
      void readNextTreePage(client, chain.issue, "graph").then(
        () => advance(client, chain),
        (error: unknown) => {
          refuse(client, chain, error);
          advance(client, chain);
        }
      );
      return;
    } catch (error) {
      // An act no request can be made for from the held read ends as a refused one does.
      refuse(client, chain, error);
    }
  }
  const { chains } = stateOf(client);
  const id = hashKey([chain.issue, chain.node]);
  if (chains.get(id) === chain) chains.delete(id);
  if (chain.reads) readOutlinesAgain(client, chain.issue);
}

/** The write of one act of `chain`, as the store makes and keeps it: on the node's own line, its
 *  refusal drawn on the row (`inline`), holding the issue's tree reads while it is out. */
function writeOf<T>(chain: Chain, label: string, request: WriteRequest<T>) {
  return {
    detail: null,
    inline: true,
    issue: chain.issue,
    kind: plainWrite,
    label,
    presentation: "indicator",
    request,
    target: `node:${chain.issue}:${chain.node}`,
  } as const;
}

/** The `PATCH` of a move, or of a new row's placing, labelled `label` in the write store: a taken
 *  answer is written into the held read at its place, and the chain goes on. */
function sendMove(
  client: QueryClient,
  chain: Chain,
  act: MoveAct | PlaceAct,
  request: WriteRequest<NodeMoved>,
  label: string
): void {
  const sent = writesOf(client).run(writeOf(chain, label, request));
  void sent.then(
    (answer) => {
      chain.acts.shift();
      chain.reads = true;
      act.done(TAKEN);
      try {
        // A landing with no answer to give says nothing of the place: the next act goes from the
        // read as it stands, and its refusal reads the tree again.
        if (answer !== null) placeHeld(client, chain.issue, chain.node, answer);
      } finally {
        advance(client, chain);
      }
    },
    (error: unknown) => {
      refuse(client, chain, error);
      advance(client, chain);
    }
  );
}

function sendDelete(
  client: QueryClient,
  chain: Chain,
  act: DeleteAct,
  request: WriteRequest<NodeDeleted>
): void {
  const sent = writesOf(client).run(writeOf(chain, CONTROL_LABELS.edit_delete, request));
  void sent.then(
    () => {
      chain.acts.shift();
      chain.reads = true;
      act.done(TAKEN);
      advance(client, chain);
    },
    (error: unknown) => {
      // The count moved: the confirm opens again with the server's count, and nothing more is
      // sent for it until the reader confirms that one.
      if (
        error instanceof ApiError &&
        error.code === DELETE_COUNT_MOVED &&
        error.count !== undefined
      ) {
        chain.acts.shift();
        act.done({ count: error.count, status: "count_moved" });
      } else {
        refuse(client, chain, error);
      }
      advance(client, chain);
    }
  );
}

/** Puts `act` at the back of its node's chain, clearing the row's line, and runs the chain where
 *  it was empty. */
function press(client: QueryClient, issue: string, node: string, act: Act): void {
  setLine(client, issue, node, undefined);
  const { chains } = stateOf(client);
  const id = hashKey([issue, node]);
  const chain = chains.get(id);
  if (chain !== undefined) {
    chain.acts.push(act);
    return;
  }
  const begun: Chain = { acts: [act], issue, node, reads: false };
  chains.set(id, begun);
  advance(client, begun);
}

function pressMove(
  client: QueryClient,
  issue: string,
  node: string,
  move: MovePress
): Promise<MoveOutcome> {
  const { promise, resolve } = Promise.withResolvers<MoveOutcome>();
  const { entry, row, status, zoomRoot } = move;
  press(client, issue, node, { done: resolve, entry, kind: "move", row, status, zoomRoot });
  return promise;
}

function pressDelete(
  client: QueryClient,
  issue: string,
  node: string,
  count: number
): Promise<DeleteOutcome> {
  const { promise, resolve } = Promise.withResolvers<DeleteOutcome>();
  press(client, issue, node, { count, done: resolve, kind: "delete" });
  return promise;
}

/**
 * Places the node of read id `node`, a new row's (R3), with the one `PATCH` of `body` - the place
 * `placement.ts` gave the row as `entry` made it - under the node's `placement_seq` as Graph's
 * held read of `issue` holds it, on the node's own chain, as a move goes (`MoveOutcome`): a taken
 * answer is written into the held read at its `parent` and `after`, a refusal draws the row's line
 * (R8), and once the chain is empty the outline reads are read again (R7).
 */
export function placeNode(
  client: QueryClient,
  issue: string,
  node: string,
  body: MoveBody,
  entry: NewEntry
): Promise<MoveOutcome> {
  const { promise, resolve } = Promise.withResolvers<MoveOutcome>();
  press(client, issue, node, { body, done: resolve, entry, kind: "place" });
  return promise;
}

/**
 * The writes of Graph's node menu for `issue` (`NodeWrites`), under the page's query client,
 * whose held tree is Graph's outline read. Every hook of one client shares each node's chain and
 * each row's line, so the menu and the editor's keys queue on one chain, and a line outlives the
 * menu that drew it. A move takes the issue's status and zoom root as they stood at its press;
 * each act reads the held read as it stands at its turn, never one a render held.
 */
export function useNodeWrites({ key, status, zoomRoot }: NodeWritesIssue): NodeWrites {
  const queryClient = useQueryClient();
  const state = stateOf(queryClient);
  const subscribe = useCallback(
    (listener: () => void) => {
      state.listeners.add(listener);
      return () => {
        state.listeners.delete(listener);
      };
    },
    [state]
  );
  const lines = useSyncExternalStore(subscribe, () => state.lines.get(key) ?? NO_LINES);
  const move = useCallback(
    (node: string, entry: MoveEntry, row?: PlacementRow) =>
      pressMove(queryClient, key, node, { entry, row, status, zoomRoot }),
    [key, queryClient, status, zoomRoot]
  );
  const remove = useCallback(
    (node: string, expectedCount: number) => pressDelete(queryClient, key, node, expectedCount),
    [key, queryClient]
  );
  const count = useCallback(
    async (node: string, signal?: AbortSignal): Promise<DeleteCount> => {
      try {
        const { count: taken } = await dryRunDelete(key, node, signal);
        return { count: taken, status: "counted" };
      } catch (error) {
        if (signal?.aborted === true) throw error;
        const held = heldRead(queryClient, key);
        const line = refusalLine(error, held === undefined ? undefined : { held, node });
        if (line.retry) return FAILED;
        if (line.reads) readOutlinesAgain(queryClient, key);
        return { line, status: "refused" };
      }
    },
    [key, queryClient]
  );
  const grey = useCallback(
    (node: string, reason: GreyReason) =>
      setLine(queryClient, key, node, { text: ANSWER_LINES[reason] }),
    [key, queryClient]
  );
  return useMemo(() => ({ count, grey, lines, move, remove }), [count, grey, lines, move, remove]);
}
