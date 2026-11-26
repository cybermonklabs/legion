import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import {
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import type { ComposerAnchor } from "../../conversation/composer-model";
import { CardStackFocus, regionOrigin, useCardStack } from "../../margin/card-stack";
import type { MarkComposePlace } from "../../margin/margin-context";
import { parseIssuePath } from "../../refs/routes";
import { useMediaQuery } from "../../shell/useDialog";
import { outlineItemRoute } from "../address-target";
import { CONTENTS_PANE_QUERY } from "../contents-frame";
import { useFocusedNode } from "../focused-node";
import { type IssueAddress, issueAddress } from "../issue-address";
import { LensActContext, useLensAct } from "../lenses/LensMenu";
import { useMarginDrawer } from "../margin-slot";
import { NoTooltips } from "../NoTooltips";
import { EDITOR_BOX_ATTRIBUTE } from "../node-caret";
import { authorText } from "../page-structure";
import { MARGIN_WORDS } from "../page-words";
import type { SlotProps } from "../slot-props";
import { useFollowing } from "../slots/following";
import { moveToRow } from "../useTreeKeymap";
import { highlightThreadWords, type ThreadWords } from "./highlights";
import { ITEM_KINDS } from "./on-text";
import {
  type SelectedWords,
  SelectionComposer,
  type TextComposerOutcome,
  wordsKeyOf,
} from "./SelectionComposer";
import { itemBlocksOf, type QuotedNode, rangeOf } from "./selection-quote";
import { TextThreadCard, useThreadCardHost } from "./TextThreadCard";
import type { QuotedWords } from "./text-comment-write";
import { type NodeThreads, type TextThread, threadHolds } from "./text-threads";
import { useTextThreads, type ViewThreads } from "./useTextThreads";

/**
 * The data attribute the stage carries while the column of comments on text is drawn, which turns
 * on the container steps of Plan's chart (`PlanFilterBody.tsx`) and no other class of the stage
 * (`widths.test.ts`). The column sets it on the page's row that holds the stage and the column,
 * an ancestor of the stage, and takes it off as it goes.
 */
export const TEXT_MARGIN_ATTRIBUTE = "data-text-margin";

/** The attribute of the box that holds a composer of this part, in the column or in a node's
 *  sheet. */
export const TEXT_COMPOSER_ATTRIBUTE = "data-text-composer";

/** The attribute of the box each thread, and the composer, stands in in the column: the tree id of
 *  its node. */
export const THREAD_NODE_ATTRIBUTE = "data-text-thread-node";

/** The words a person selected, as the control hands them to the column to open its composer on
 *  (R5): the node they are in, by its tree id (`TreeNode.id`), and what the server needs to find
 *  them again. */
export type TextComposerRequest = SelectedWords;

/** The composer of this part: the request it is open on, or undefined while none is open, and
 *  what its last send left the page to do. */
export interface TextComposer {
  /** Closes the composer, keeping its draft as `MentionComposer` keeps one. */
  readonly close: () => void;
  /** Closes the composer on its send's 201 and holds what the page does next (R5): the new
   *  thread's root takes the focus once the re-read draws it, or one sentence says a reply's
   *  quote was not kept. */
  readonly land: (outcome: TextComposerOutcome) => void;
  /** What the last send left the page to do, until the page has done it (`settle`) or a
   *  composer opens again. */
  readonly landed: TextComposerOutcome | undefined;
  /** Opens the composer in the column beside the request's node, or in that node's sheet below
   *  1024 px; the column is drawn for it. Details closes first where it is open (R2). */
  readonly open: (request: TextComposerRequest) => void;
  readonly request: TextComposerRequest | undefined;
  /** Says the page has done what the last send left it to do. */
  readonly settle: () => void;
}

/** The composer's one state for one app: the issue it is open on and the request, and what its
 *  last send left for that issue's page, which the column and the control share wherever each is
 *  drawn on the page; and the column's place for a block's editor's composes while a column is
 *  mounted (`useMarkComposePlace`). */
interface ComposerStore {
  held: { readonly key: string; readonly request: TextComposerRequest } | undefined;
  landed: { readonly key: string; readonly outcome: TextComposerOutcome } | undefined;
  readonly listeners: Set<() => void>;
  place: MarkComposePlace | undefined;
  readonly subscribe: (listener: () => void) => () => void;
}

/** Each app's composer state, by its query client: one store per app, as there is one client,
 *  so it is reached from anywhere under the client's provider with no provider of its own. */
const composerStores = new WeakMap<QueryClient, ComposerStore>();

/** The composer state of `client`'s app, made on first use. */
function composerStoreOf(client: QueryClient): ComposerStore {
  let store = composerStores.get(client);
  if (store === undefined) {
    const listeners = new Set<() => void>();
    store = {
      held: undefined,
      landed: undefined,
      listeners,
      place: undefined,
      subscribe: (listener) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
    };
    composerStores.set(client, store);
  }
  return store;
}

/** Tells each reader of `store` that what it holds changed. */
function tell(store: ComposerStore): void {
  for (const listener of [...store.listeners]) listener();
}

/** Sets what `store` holds - the composer open, and what its last send left - and tells each
 *  reader. A compose of a block's editor that the composer leaves with no send out - closed,
 *  replaced by another, or gone with its page - rejects the editor's promise, so the editor takes
 *  its mark back out of the document; one whose send went out was resolved then, which this does
 *  not undo. */
function holdComposer(
  store: ComposerStore,
  held: ComposerStore["held"],
  landed: ComposerStore["landed"] = store.landed
): void {
  const left = store.held?.request.mark;
  store.held = held;
  store.landed = landed;
  if (left !== undefined && held?.request.mark !== left) {
    left.reject(new Error("the composer closed with no send out"));
  }
  tell(store);
}

/**
 * Closes the shell's Details drawer where it is open, as its own collapse does: what an address of
 * R8, or a composer of R5, calls before it opens a thread on text, which the column draws only
 * while Details is closed (R2).
 */
function useCloseDetails(): () => void {
  const drawer = useMarginDrawer();
  return useCallback(() => {
    if (drawer?.open === true) drawer.setOpen(false);
  }, [drawer]);
}

/**
 * The composer's one state for the issue the router is at, which the column draws and the control
 * opens (`SelectionComment`): reachable from anywhere inside the page's providers, since it is
 * the query client's (`composerStoreOf`). A request held for another issue is no composer here.
 */
export function useTextComposer(): TextComposer {
  const store = composerStoreOf(useQueryClient());
  const { pathname, search } = useLocation();
  const key = parseIssuePath(pathname, search)?.key;
  const held = useSyncExternalStore(store.subscribe, () => store.held);
  const landedHeld = useSyncExternalStore(store.subscribe, () => store.landed);
  const closeDetails = useCloseDetails();
  const request = held !== undefined && held.key === key ? held.request : undefined;
  const landed =
    landedHeld !== undefined && landedHeld.key === key ? landedHeld.outcome : undefined;
  return useMemo(
    () => ({
      close: () => {
        if (store.held !== undefined && store.held.key === key) holdComposer(store, undefined);
      },
      land: (outcome: TextComposerOutcome) => {
        if (key === undefined) return;
        const open = store.held !== undefined && store.held.key === key;
        holdComposer(store, open ? undefined : store.held, { key, outcome });
      },
      landed,
      open: (next: TextComposerRequest) => {
        if (key === undefined) return;
        closeDetails();
        holdComposer(store, { key, request: next }, undefined);
      },
      request,
      settle: () => {
        if (store.landed !== undefined && store.landed.key === key) {
          holdComposer(store, store.held, undefined);
        }
      },
    }),
    [closeDetails, key, landed, request, store]
  );
}

/**
 * The column's place for a compose a block's editor asks for (`MarkComposePlace`), which the
 * page hands the editor (`StagedDocument.tsx`): its selection bar and its chord open the
 * composer of R5 in the column, beside the block's node, or in the node's sheet below 1024 px,
 * and never in Details (R6). Undefined while no column of comments on text is mounted, as on
 * every page but the new issue page; with none, the editor's composer opens in the margin as it
 * always has, and Details hosts the editor's cards (`slots/details.tsx`).
 */
export function useMarkComposePlace(): MarkComposePlace | undefined {
  const store = composerStoreOf(useQueryClient());
  return useSyncExternalStore(store.subscribe, () => store.place);
}

/**
 * The node of `view` a compose of a block's editor stands beside: the node of the block of the
 * editor's mark (`placement.blockId`), else the node of the row the editor is drawn in
 * (`EDITOR_BOX_ATTRIBUTE`) - a closed issue's document names its words by their quote alone, in
 * no block. Undefined where neither is on the page.
 */
function composeNodeOf(view: ViewThreads, anchor: ComposerAnchor): string | undefined {
  const blockId = anchor.mark_id === undefined ? anchor.block_id : anchor.placement?.blockId;
  if (blockId !== undefined && blockId !== null) {
    for (const [id, node] of view.nodes) {
      if (node.document?.artifact_id === anchor.artifact && node.block_id === blockId) return id;
    }
  }
  const box = document.querySelector(`[${EDITOR_BOX_ATTRIBUTE}]`);
  return box?.closest<HTMLElement>("[data-node-id]")?.dataset.nodeId;
}

/**
 * The column at the right of the main column that draws each thread on text beside its node
 * (R2): an `aside` with `role="region"` named by the margin's words (`page-words-margin.ts`), the
 * third child of the page's row after the main column (`IssueOutlinePage.tsx`), so it follows the
 * main column in the tab order (R6). It is drawn from 1024 px (`CONTENTS_PANE_QUERY`) while the
 * view the stage draws holds a thread of a node it draws, or while a composer of this part is
 * open, 280 px wide and 320 px from 1280 px; the main column is `min-w-0 flex-1`, so the stage
 * and the box at its foot narrow by its width, and the header, the identity band and the view row
 * above the row keep theirs. Below 1024 px it draws nothing: each node's sheet takes the threads
 * there (`NodeThreads.tsx`). Its threads are the view's one answer (`useTextThreads`), which
 * sends no read.
 *
 * The column stacks its boxes through `useCardStack` (R2), with the box in focus as its stack's
 * `CardStackFocus`: the thread or the composer whose box holds the focus, else the first thread of
 * the node whose row holds it (`useFocusedNode`).
 */
export function TextMargin({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const wide = useMediaQuery(CONTENTS_PANE_QUERY);
  const view = useTextThreads(issue, address.preset);
  const composer = useTextComposer();
  const [focusedBox, setFocusedBox] = useState<string>();
  const focusedNode = useFocusedNode()?.id;
  const stackFocus =
    focusedBox ??
    (focusedNode === undefined ? undefined : view.threads.get(focusedNode)?.threads[0]?.thread.key);
  // A composer open on the issue goes with its page, so it is not open again on a later visit,
  // and so does what its last send left the page to do.
  const store = composerStoreOf(useQueryClient());
  const key = issue.key;
  useEffect(
    () => () => {
      const { held, landed } = store;
      if (held?.key !== key && landed?.key !== key) return;
      holdComposer(
        store,
        held?.key === key ? undefined : held,
        landed?.key === key ? undefined : landed
      );
    },
    [key, store]
  );
  // The column's place for a block's editor's composes (`useMarkComposePlace`), offered while the
  // column is mounted at every width, since the node's sheet draws the same composer below
  // 1024 px. It reads the view and the composer as they stand when the editor asks, and opens the
  // composer on the editor's words as the control does: Details closes first.
  const now = useRef({ composer, view });
  now.current = { composer, view };
  useEffect(() => {
    const place: MarkComposePlace = (compose) => {
      const { promise, reject, resolve } = Promise.withResolvers<void>();
      const nodeId = composeNodeOf(now.current.view, compose.anchor);
      if (nodeId === undefined) {
        reject(new Error("no node of the page holds the editor's words"));
        return promise;
      }
      now.current.composer.open({
        block: null,
        mark: { ...compose, reject, resolve },
        nodeId,
        occurrence: compose.anchor.occurrence ?? 0,
        quote: compose.anchor.quote,
      });
      return promise;
    };
    store.place = place;
    tell(store);
    return () => {
      if (store.place !== place) return;
      store.place = undefined;
      tell(store);
    };
  }, [store]);
  if (!wide || (view.threads.size === 0 && composer.request === undefined)) return null;
  return (
    <CardStackFocus.Provider value={stackFocus}>
      <Column
        address={address}
        composer={composer}
        issue={issue}
        onFocusBox={setFocusedBox}
        route={route}
        search={search}
        view={view}
      />
    </CardStackFocus.Provider>
  );
}

/** One thread the column draws, with the node it stands by and that node's stored parent. */
interface Placed {
  readonly nodeId: string;
  readonly parent: string;
  readonly thread: TextThread;
}

/** How far down the stage a node that the stage draws no row of yet is ranked. */
const UNRANKED = Number.MAX_SAFE_INTEGER;

/** The threads of `threads`, the nodes in `order` - their rows' order in the stage - and each
 *  node's threads in their own order (`textThreadsOf`): the order of their words down the view. */
function placedOf(threads: ReadonlyMap<string, NodeThreads>, order: readonly string[]): Placed[] {
  const rank = new Map(order.map((id, at) => [id, at]));
  return [...threads.keys()]
    .sort((left, right) => (rank.get(left) ?? UNRANKED) - (rank.get(right) ?? UNRANKED))
    .flatMap((nodeId) => {
      const held = threads.get(nodeId);
      return (held?.threads ?? []).map((thread) => ({
        nodeId,
        parent: held?.parent ?? "",
        thread,
      }));
    });
}

/**
 * The row the stage draws for the node `nodeId`: where it draws the node twice - a check or a plan
 * task under each requirement it serves - the row under its stored parent `parent`, by the path
 * the row's key names (`tree-keys.ts`' `rowKeyOf`), else the first.
 */
function rowIn(stage: Element, nodeId: string, parent: string | undefined): HTMLElement | null {
  return rowAmong(
    [...stage.querySelectorAll<HTMLElement>(`[data-node-id="${CSS.escape(nodeId)}"]`)],
    parent
  );
}

/** The row `rowIn` answers among `rows`, the rows the stage draws for one node in the order it
 *  draws them: the one under the stored parent `parent` where there are two or more, else the
 *  first. */
function rowAmong(rows: readonly HTMLElement[], parent: string | undefined): HTMLElement | null {
  if (rows.length < 2 || parent === undefined) return rows[0] ?? null;
  return rows.find((row) => row.dataset.rowKey?.split(">").at(-2) === parent) ?? rows[0] ?? null;
}

/** The rows the stage draws, by the tree id of each one's node, each node's rows in the order the
 *  stage draws them, so `measure` reads the stage once for all its boxes. */
function rowsByNode(stage: Element): Map<string, HTMLElement[]> {
  const rows = new Map<string, HTMLElement[]>();
  for (const row of stage.querySelectorAll<HTMLElement>("[data-node-id]")) {
    const id = row.dataset.nodeId;
    if (id === undefined) continue;
    const held = rows.get(id);
    if (held === undefined) rows.set(id, [row]);
    else held.push(row);
  }
  return rows;
}

/** The ids of the nodes that `holds` answers for, in the order the stage draws their rows. */
function rowOrder(stage: Element, holds: (id: string) => boolean): string[] {
  const order: string[] = [];
  for (const row of stage.querySelectorAll<HTMLElement>("[data-node-id]")) {
    const id = row.dataset.nodeId;
    if (id !== undefined && holds(id) && !order.includes(id)) order.push(id);
  }
  return order;
}

/** One box the column stacks (`useCardStack`), by its key: a thread's card, or the composer of a
 *  new comment, with the node it stands by. */
interface Box {
  readonly key: string;
  readonly nodeId: string;
}

/** The key of the composer's box in the column's stack, which no thread's key - its root's id -
 *  is. */
const COMPOSER_BOX = "#composer";

/** A box's key in the column's stack. */
const boxKey = (box: Box): string => box.key;

/**
 * The boxes the column stacks, in the order of their words down the view: the threads of each
 * node in `order`, the nodes whose rows the stage draws, each node's threads in their own order
 * and then the composer where it is open on that node's words (`composing`); and the composer
 * last where the stage draws its node's row no more. A thread whose node has no row drawn is not
 * drawn, and takes no place in the stack.
 */
function stackedOf(
  threads: ReadonlyMap<string, NodeThreads>,
  order: readonly string[],
  composing: string | undefined
): Box[] {
  const boxes: Box[] = [];
  for (const nodeId of order) {
    for (const { thread } of threads.get(nodeId)?.threads ?? []) {
      boxes.push({ key: thread.key, nodeId });
    }
    if (composing === nodeId) boxes.push({ key: COMPOSER_BOX, nodeId });
  }
  if (composing !== undefined && !order.includes(composing)) {
    boxes.push({ key: COMPOSER_BOX, nodeId: composing });
  }
  return boxes;
}

/** The stage the column stands beside: the element before it in the page's row. */
function stageOf(column: RefObject<HTMLElement | null>): Element | null {
  return column.current?.previousElementSibling ?? null;
}

/** The card of the thread `key` names in `container` - the column's list or a node's sheet - or
 *  null where it draws none. */
export function cardIn(container: Element | null, key: string): HTMLElement | null {
  return container?.querySelector<HTMLElement>(`[data-margin-item="${CSS.escape(key)}"]`) ?? null;
}

/** Gives `target` the focus, as a script gives it to an element out of the tab order: one with
 *  no `tabindex` takes `tabIndex = -1` first. */
export function takeFocus(target: HTMLElement): void {
  if (!target.hasAttribute("tabindex")) target.tabIndex = -1;
  target.focus();
}

/** What the column finds a box's words by in its node's row: the quote, which match of it, the
 *  item's block it is in, and whether the words left the node's text, which places the box at
 *  the row's first line. */
type Words = QuotedWords & { readonly firstLine: boolean };

/** The node `nodeId` as `rangeOf` and `quoteOf` read its words: its text, and an item's blocks in
 *  order. */
export function quotedNodeOf(view: ViewThreads, nodeId: string): QuotedNode | undefined {
  const node = view.nodes.get(nodeId);
  if (node === undefined) return undefined;
  const item = ITEM_KINDS[node.supertag] === true;
  return item && view.answer !== undefined
    ? { blocks: itemBlocksOf(view.answer, node.id), text: node.text, text_tree: node.text_tree }
    : node;
}

/** The range of `words` in `row`, the row of the node `nodeId` of `view`, or null where the row
 *  draws none of them. */
function wordsRange(row: Element, words: Words, view: ViewThreads, nodeId: string): Range | null {
  if (words.firstLine) return null;
  const node = quotedNodeOf(view, nodeId);
  const block =
    words.block === null ? undefined : node?.blocks?.find((one) => one.block_id === words.block);
  return rangeOf(row, words.quote, words.occurrence, {
    ...(block === undefined ? {} : { block }),
    ...(node === undefined ? {} : { node }),
  });
}

/** The following of an address that names an item of a thread the column draws, and the item. */
interface Landing {
  readonly following: string;
  readonly id: string;
}

function Column({
  address,
  composer,
  issue,
  onFocusBox,
  route,
  search,
  view,
}: SlotProps & {
  readonly address: IssueAddress;
  readonly composer: TextComposer;
  /** Says which box holds the focus: a thread's, by its key, the composer's, or none. */
  readonly onFocusBox: (key: string | undefined) => void;
  readonly search: string;
  readonly view: ViewThreads;
}): ReactNode {
  const host = useThreadCardHost(issue);
  // The Lens act each comment of a thread carries in Conversation and Graph (R7): the column is
  // outside the tree's rows, which provide it to theirs (`TreeRows.tsx`), so it provides it to its
  // cards from the same hook. In every other view there is none.
  const lensAct = useLensAct();
  const closeDetails = useCloseDetails();
  // While Details is open the column keeps its width and draws nothing: its boxes are hidden and
  // out of the page's reach, so Tab reaches none of them (R2).
  const detailsOpen = useMarginDrawer()?.open === true;
  const aside = useRef<HTMLElement>(null);
  // The nodes with threads, and the composer's, in the order the stage draws their rows, read
  // after each render and each change of the stage.
  const [order, setOrder] = useState<readonly string[]>([]);
  const placed = useMemo(() => placedOf(view.threads, order), [order, view.threads]);
  const request = composer.request;
  // The words each thread's box is placed by, by its card's key.
  const words = useMemo(
    () =>
      new Map(
        placed.map(({ thread }): [string, Words] => [
          thread.thread.key,
          {
            block: thread.replyRoot ? (thread.thread.anchor?.block_id ?? null) : null,
            firstLine: thread.firstLine,
            occurrence: thread.occurrence,
            quote: thread.quote,
          },
        ])
      ),
    [placed]
  );

  // The boxes stand where the stack places them (`useCardStack`, R2): in the order of their
  // words, none over another, each level with its words or 8 px under the box above, and the box
  // in focus level with its words (`CardStackFocus`, which `TextMargin` provides). Each box wants
  // the top `measure` last read for it.
  const stacked = useMemo(
    () => stackedOf(view.threads, order, request?.nodeId),
    [order, request?.nodeId, view.threads]
  );
  const [wanted, setWanted] = useState<ReadonlyMap<string, number>>(() => new Map());
  const wantedTop = useCallback((box: Box) => wanted.get(box.key), [wanted]);
  const { cardRef, height, region, topOf } = useCardStack(stacked, boxKey, wantedTop);
  const listRef = useCallback(
    (element: HTMLDivElement | null) => {
      region.current = element;
    },
    [region]
  );

  // The item an address of R8 names, where it is in a thread the column draws: Details closes
  // first, and the item takes the focus in its card once the column is drawn again. Once in each
  // following of the address (`useFollowing`). The item is the one the address names as a node
  // of the outline (`outlineItemRoute`), or the one it marks beside the document it opens
  // (`documentItem`), where the old page's landing sends an item of a document's words.
  const following = useFollowing();
  // A lens invoked from a thread lands as `?node=<its read id>` (R7), which names no row of the
  // outline but the lens drawn in its thread's card.
  const named =
    outlineItemRoute(route, search, address)?.id ??
    address.documentItem?.id ??
    (address.target?.kind === "node" ? address.target.id : undefined);
  const namedThread =
    named === undefined ? undefined : placed.find(({ thread }) => threadHolds(thread, named));
  const [landing, setLanding] = useState<Landing>();
  const landed = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (named === undefined || namedThread === undefined || landing?.following === following) {
      return;
    }
    closeDetails();
    setLanding({ following, id: named });
  }, [closeDetails, following, landing?.following, named, namedThread]);
  const currentReply =
    landing === undefined || namedThread === undefined
      ? undefined
      : namedThread.thread.thread.root.comment.id === landing.id
        ? undefined
        : { id: landing.id, thread: namedThread.thread.thread.key };

  // The words of each thread in a row drawn read-only, last measured, and the thread whose box
  // holds the focus, whose words take the stronger tint (R2).
  const marked = useRef<readonly { readonly key: string; readonly range: Range }[]>([]);
  const focusedKey = useRef<string | undefined>(undefined);
  const unmark = useRef<() => void>(() => {});
  const mark = useCallback(() => {
    unmark.current();
    unmark.current = highlightThreadWords(
      marked.current.map(
        ({ key, range }): ThreadWords => ({ focused: key === focusedKey.current, range })
      )
    );
  }, []);
  useEffect(() => () => unmark.current(), []);

  // Reads the top each box wants against the column's own origin (`regionOrigin`'s rule): the top
  // of the first rectangle of its words in its node's row, else of the row; and the order of the
  // rows. A thread's box whose node has no row drawn - folded or paged away - is not drawn and
  // wants no top, while the composer's wants the top its row last had, and keeps its draft; the
  // column draws nothing while no box is drawn. While any box is drawn, the page's row that holds
  // the stage carries `TEXT_MARGIN_ATTRIBUTE`. The words measured are marked (`highlights.ts`).
  // It places nothing itself: a top that moved draws the stack again.
  const measure = useCallback(() => {
    const column = aside.current;
    const list = region.current;
    const stage = stageOf(aside);
    if (column === null || list === null || stage === null) return;
    const drawn = rowOrder(stage, (id) => view.threads.has(id) || id === request?.nodeId);
    setOrder((current) =>
      current.length === drawn.length && current.every((id, at) => drawn[at] === id)
        ? current
        : drawn
    );
    const origin = regionOrigin(list);
    const rows = rowsByNode(stage);
    const tops = new Map<string, number>();
    const ranges: { key: string; range: Range }[] = [];
    let composerAway = false;
    for (const box of list.querySelectorAll<HTMLElement>(`[${THREAD_NODE_ATTRIBUTE}]`)) {
      const nodeId = box.getAttribute(THREAD_NODE_ATTRIBUTE) ?? "";
      const row = rowAmong(rows.get(nodeId) ?? [], box.dataset.textThreadParent);
      const composing = box.hasAttribute(TEXT_COMPOSER_ATTRIBUTE);
      box.hidden = row === null && !composing;
      const key = composing ? COMPOSER_BOX : box.dataset.textThread;
      if (key === undefined) continue;
      if (row === null) {
        composerAway ||= composing;
        continue;
      }
      const boxWords = !composing
        ? words.get(key)
        : request === undefined
          ? undefined
          : { ...request, firstLine: false };
      const range = boxWords === undefined ? null : wordsRange(row, boxWords, view, nodeId);
      const rect = range?.getClientRects()[0] ?? row.getBoundingClientRect();
      tops.set(key, Math.max(0, rect.top - origin));
      // A row in its editor draws the marks of its comments itself.
      const editing = row.querySelector('[contenteditable="true"]') !== null;
      if (range !== null && !composing && !editing) ranges.push({ key, range });
    }
    setWanted((current) => {
      const kept = composerAway ? current.get(COMPOSER_BOX) : undefined;
      const next = kept === undefined ? tops : new Map(tops).set(COMPOSER_BOX, kept);
      return current.size === next.size && [...next].every(([key, top]) => current.get(key) === top)
        ? current
        : next;
    });
    const shown = tops.size + (composerAway ? 1 : 0);
    column.hidden = shown === 0;
    column.parentElement?.toggleAttribute(TEXT_MARGIN_ATTRIBUTE, shown !== 0);
    marked.current = ranges;
    mark();
  }, [mark, region, request, view, words]);

  // After each render.
  useLayoutEffect(() => {
    measure();
  });

  // Measures again when the stage changes size or draws other rows, at most once in an animation
  // frame; the stack follows each box's own size (`useCardStack`).
  useEffect(() => {
    const stage = stageOf(aside);
    if (stage === null) return;
    let frame: number | undefined;
    const soon = () => {
      if (frame !== undefined) return;
      frame = requestAnimationFrame(() => {
        frame = undefined;
        measure();
      });
    };
    const sized = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(soon);
    sized?.observe(stage);
    const changed = new MutationObserver(soon);
    changed.observe(stage, { characterData: true, childList: true, subtree: true });
    return () => {
      sized?.disconnect();
      changed.disconnect();
      if (frame !== undefined) cancelAnimationFrame(frame);
    };
  }, [measure]);

  // The stage's attribute goes with the column, and so does the focus of its boxes.
  useEffect(() => {
    const pageRow = aside.current?.parentElement;
    return () => {
      pageRow?.removeAttribute(TEXT_MARGIN_ATTRIBUTE);
      onFocusBox(undefined);
    };
  }, [onFocusBox]);

  // The item the address names takes the focus once Details is closed and its card is drawn: the
  // card itself for the thread's root, else its reply's row, which the card marks as the one the
  // address names (`currentReplyId`). After the effects of the stage, which come first on the
  // page, so the stage's own opening focus (`ViewStage`) does not take it back.
  useEffect(() => {
    if (landing === undefined || landed.current === landing.following || detailsOpen) return;
    const thread = placed.find(({ thread: one }) => threadHolds(one, landing.id))?.thread.thread;
    if (thread === undefined) return;
    const card = cardIn(region.current, thread.key);
    // The thread's root takes the focus as the card, a reply as its row, and a lens as its own
    // row, which the card draws under the comment it was invoked on.
    const lens = card?.querySelector<HTMLElement>(
      `[data-thread-lens="${CSS.escape(landing.id)}"], [data-thread-lens-read="${CSS.escape(landing.id)}"]`
    );
    const target =
      thread.root.comment.id === landing.id
        ? card
        : (lens ?? card?.querySelector<HTMLElement>('li[data-reply][aria-current="true"]'));
    if (target === null || target === undefined) return;
    landed.current = landing.following;
    takeFocus(target);
  });

  // The root of the thread a send of this part wrote takes the focus once the re-read draws its
  // card, as the item an address names does (R5).
  const sent = composer.landed;
  useEffect(() => {
    if (sent === undefined || !("root" in sent) || detailsOpen) return;
    const thread = placed.find(({ thread: one }) => one.thread.root.comment.id === sent.root);
    const card = thread === undefined ? null : cardIn(region.current, thread.thread.thread.key);
    if (card === null) return;
    takeFocus(card);
    composer.settle();
  });

  // Escape in a thread, outside its composer, gives the focus to the row of the thread's node
  // (R6), as the tree moves its focus to a row.
  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
    const target = event.target;
    if (event.key !== "Escape" || event.defaultPrevented || !(target instanceof Element)) return;
    const inComposer = target.closest(
      `[${TEXT_COMPOSER_ATTRIBUTE}], textarea, input, select, [contenteditable="true"]`
    );
    const box = target.closest<HTMLElement>(`[${THREAD_NODE_ATTRIBUTE}]`);
    const stage = stageOf(aside);
    if (inComposer !== null || box === null || stage === null) return;
    const row = rowIn(
      stage,
      box.getAttribute(THREAD_NODE_ATTRIBUTE) ?? "",
      box.dataset.textThreadParent
    );
    if (row === null) return;
    event.preventDefault();
    moveToRow(row);
  };

  // The box that holds the focus: the stack stands it level with its words (`TextMargin`), and a
  // thread's words take the stronger tint.
  const focusBox = (box: HTMLElement | null | undefined) => {
    const key = box?.hasAttribute(TEXT_COMPOSER_ATTRIBUTE) ? COMPOSER_BOX : box?.dataset.textThread;
    onFocusBox(key);
    if (key === focusedKey.current) return;
    focusedKey.current = key;
    mark();
  };
  const onFocus = (event: FocusEvent<HTMLDivElement>) => {
    const target = event.target;
    if (!(target instanceof Element)) return;
    focusBox(target.closest<HTMLElement>(`[${THREAD_NODE_ATTRIBUTE}]`));
  };
  const onBlur = (event: FocusEvent<HTMLDivElement>) => {
    const next = event.relatedTarget;
    if (!(next instanceof Node) || !event.currentTarget.contains(next)) focusBox(undefined);
  };

  // Escape in the composer, and its own Close, close it with its draft kept and give the focus
  // to the row of its node (R5's table, R6).
  const leaveComposer = () => {
    const nodeId = request?.nodeId;
    const stage = stageOf(aside);
    composer.close();
    if (nodeId === undefined || stage === null) return;
    const row = rowIn(stage, nodeId, view.threads.get(nodeId)?.parent);
    if (row !== null) moveToRow(row);
  };

  return (
    // biome-ignore lint/a11y/useSemanticElements: the column is an `aside` beside the main column that is named as a region of its own (R6).
    <aside
      aria-label={MARGIN_WORDS.region}
      className="relative w-[280px] shrink-0 self-stretch pt-2.5 pr-3.5 min-[1280px]:w-[320px]"
      ref={aside}
      role="region"
    >
      <NoTooltips>
        {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handlers only read where the focus is in the cards' own controls, and Escape among them. */}
        <div
          className="relative"
          inert={detailsOpen ? true : undefined}
          onBlur={onBlur}
          onFocus={onFocus}
          onKeyDown={onKeyDown}
          ref={listRef}
          style={detailsOpen ? { minHeight: height, visibility: "hidden" } : { minHeight: height }}
        >
          {/* The threads' stored text is the authors' (R4), as `IssueMarginCards` marks the
              margin's cards. */}
          <div className="contents" {...authorText}>
            <LensActContext.Provider value={lensAct}>
              {placed.map(({ nodeId, parent, thread }) => (
                <div
                  className="absolute inset-x-0"
                  data-text-thread={thread.thread.key}
                  data-text-thread-parent={parent}
                  key={thread.thread.key}
                  ref={cardRef(thread.thread.key)}
                  style={{ top: topOf(thread.thread.key) }}
                  {...{ [THREAD_NODE_ATTRIBUTE]: nodeId }}
                >
                  <TextThreadCard
                    host={host}
                    thread={thread}
                    {...(currentReply?.thread === thread.thread.key
                      ? { currentReplyId: currentReply.id }
                      : {})}
                  />
                </div>
              ))}
            </LensActContext.Provider>
            {request === undefined ? null : (
              <div
                className="absolute inset-x-0"
                ref={cardRef(COMPOSER_BOX)}
                style={{ top: topOf(COMPOSER_BOX) }}
                {...{ [THREAD_NODE_ATTRIBUTE]: request.nodeId, [TEXT_COMPOSER_ATTRIBUTE]: "" }}
              >
                <SelectionComposer
                  issue={issue}
                  key={wordsKeyOf(request)}
                  onLand={composer.land}
                  onLeave={leaveComposer}
                  view={view}
                  words={request}
                />
              </div>
            )}
          </div>
        </div>
      </NoTooltips>
    </aside>
  );
}
