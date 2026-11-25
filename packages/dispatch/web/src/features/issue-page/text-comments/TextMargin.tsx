import { type QueryClient, useQueryClient } from "@tanstack/react-query";
import {
  type FocusEvent,
  type KeyboardEvent,
  type ReactNode,
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
import { regionOrigin } from "../../margin/ThreadList";
import { parseIssuePath } from "../../refs/routes";
import { useMediaQuery } from "../../shell/useDialog";
import { outlineItemRoute } from "../address-target";
import { CONTENTS_PANE_QUERY } from "../contents-frame";
import { type IssueAddress, issueAddress } from "../issue-address";
import { useMarginDrawer } from "../margin-slot";
import { NoTooltips } from "../NoTooltips";
import { authorText } from "../page-structure";
import { MARGIN_WORDS } from "../page-words";
import type { SlotProps } from "../slot-props";
import { useFollowing } from "../slots/following";
import { moveToRow } from "../useTreeKeymap";
import { highlightThreadWords, type ThreadWords } from "./highlights";
import { quoteLine } from "./quote-line";
import { itemBlocksOf, type QuotedNode, rangeOf } from "./selection-quote";
import { TextThreadCard, useThreadCardHost } from "./TextThreadCard";
import type { NodeThreads, TextThread } from "./text-threads";
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
const THREAD_NODE_ATTRIBUTE = "data-text-thread-node";

/** The words a person selected, as the control hands them to the column to open its composer on
 *  (R5): the node they are in, by its tree id (`TreeNode.id`), and what the server needs to find
 *  them again. */
export interface TextComposerRequest {
  /** The item's block that holds the selection, or null for a node that has one text. */
  readonly block: string | null;
  /** The tree id of the node the words are in. */
  readonly nodeId: string;
  /** How many matches of `quote` start before the selection in the block's (or node's) text. */
  readonly occurrence: number;
  /** The selected words as plain text reads them. */
  readonly quote: string;
}

/** The composer of this part: the request it is open on, or undefined while none is open. */
export interface TextComposer {
  /** Closes the composer, keeping its draft as `MentionComposer` keeps one. */
  readonly close: () => void;
  /** Opens the composer in the column beside the request's node, or in that node's sheet below
   *  1024 px; the column is drawn for it. Details closes first where it is open (R2). */
  readonly open: (request: TextComposerRequest) => void;
  readonly request: TextComposerRequest | undefined;
}

/** The composer's one state for one app: the issue it is open on and the request, which the
 *  column and the control share wherever each is drawn on the page. */
interface ComposerStore {
  held: { readonly key: string; readonly request: TextComposerRequest } | undefined;
  readonly listeners: Set<() => void>;
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
      listeners,
      subscribe: (listener) => {
        listeners.add(listener);
        return () => listeners.delete(listener);
      },
    };
    composerStores.set(client, store);
  }
  return store;
}

/** Sets what `store` holds and tells each reader. */
function holdComposer(store: ComposerStore, held: ComposerStore["held"]): void {
  store.held = held;
  for (const listener of [...store.listeners]) listener();
}

/**
 * Closes the shell's Details drawer where it is open, as its own collapse does: what an address of
 * R8, or a composer of R5, calls before it opens a thread on text, which the column draws only
 * while Details is closed (R2).
 */
export function useCloseDetails(): () => void {
  const drawer = useMarginDrawer();
  return useCallback(() => {
    if (drawer?.open === true) drawer.setOpen(false);
  }, [drawer]);
}

/**
 * The composer's one state for the issue the router is at, which the column draws and the control
 * opens (`SelectionComment`, t8): reachable from anywhere inside the page's providers, since it is
 * the query client's (`composerStoreOf`). A request held for another issue is no composer here.
 */
export function useTextComposer(): TextComposer {
  const store = composerStoreOf(useQueryClient());
  const { pathname, search } = useLocation();
  const key = parseIssuePath(pathname, search)?.key;
  const held = useSyncExternalStore(store.subscribe, () => store.held);
  const closeDetails = useCloseDetails();
  const request = held !== undefined && held.key === key ? held.request : undefined;
  return useMemo(
    () => ({
      close: () => {
        if (store.held !== undefined && store.held.key === key) holdComposer(store, undefined);
      },
      open: (next: TextComposerRequest) => {
        if (key === undefined) return;
        closeDetails();
        holdComposer(store, { key, request: next });
      },
      request,
    }),
    [closeDetails, key, request, store]
  );
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
 */
export function TextMargin({ issue, route }: SlotProps): ReactNode {
  const { hash, search } = useLocation();
  const address = issueAddress(route, search, hash, primarySpec(issue)?.slug);
  const wide = useMediaQuery(CONTENTS_PANE_QUERY);
  const view = useTextThreads(issue, address.preset);
  const composer = useTextComposer();
  // A composer open on the issue goes with its page, so it is not open again on a later visit.
  const store = composerStoreOf(useQueryClient());
  const key = issue.key;
  useEffect(
    () => () => {
      if (store.held?.key === key) holdComposer(store, undefined);
    },
    [key, store]
  );
  if (!wide || (view.threads.size === 0 && composer.request === undefined)) return null;
  return (
    <Column
      address={address}
      composer={composer}
      issue={issue}
      route={route}
      search={search}
      view={view}
    />
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
  const rows = [...stage.querySelectorAll<HTMLElement>(`[data-node-id="${CSS.escape(nodeId)}"]`)];
  if (rows.length < 2 || parent === undefined) return rows[0] ?? null;
  return rows.find((row) => row.dataset.rowKey?.split(">").at(-2) === parent) ?? rows[0] ?? null;
}

/** The ids of the nodes of `threads` in the order the stage draws their rows. */
function rowOrder(stage: Element, threads: ReadonlyMap<string, NodeThreads>): string[] {
  const order: string[] = [];
  for (const row of stage.querySelectorAll<HTMLElement>("[data-node-id]")) {
    const id = row.dataset.nodeId;
    if (id !== undefined && threads.has(id) && !order.includes(id)) order.push(id);
  }
  return order;
}

/** Whether the item `id` names is the root of `thread` or a reply in it. */
function threadHolds(thread: TextThread, id: string): boolean {
  const card = thread.thread;
  return card.root.comment.id === id || card.replies.some((reply) => reply.id === id);
}

/** What the column finds a box's words by in its node's row: the quote, which match of it, the
 *  item's block it is in, and whether the words left the node's text, which places the box at
 *  the row's first line. */
interface Words {
  readonly block: string | null;
  readonly firstLine: boolean;
  readonly occurrence: number;
  readonly quote: string;
}

/** The node `nodeId` as `rangeOf` reads its words: its text, and an item's blocks in order. */
function quotedNodeOf(view: ViewThreads, nodeId: string): QuotedNode | undefined {
  const node = view.nodes.get(nodeId);
  if (node === undefined) return undefined;
  const item =
    node.supertag === "ask" || node.supertag === "comment" || node.supertag === "message";
  return item && view.answer !== undefined
    ? { blocks: itemBlocksOf(view.answer, node.id), text: node.text, text_tree: node.text_tree }
    : node;
}

/** The range of `words` in `row`, or null where the row draws none of them. */
function wordsRange(row: Element, words: Words, node: QuotedNode | undefined): Range | null {
  if (words.firstLine) return null;
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
  route,
  search,
  view,
}: SlotProps & {
  readonly address: IssueAddress;
  readonly composer: TextComposer;
  readonly search: string;
  readonly view: ViewThreads;
}): ReactNode {
  const host = useThreadCardHost(issue);
  const closeDetails = useCloseDetails();
  // While Details is open the column keeps its width and draws nothing: its boxes are hidden and
  // out of the page's reach, so Tab reaches none of them (R2).
  const detailsOpen = useMarginDrawer()?.open === true;
  const aside = useRef<HTMLElement>(null);
  const list = useRef<HTMLDivElement>(null);
  // The nodes with threads, in the order the stage draws their rows, read after each render.
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
            firstLine: thread.firstLine || thread.orphaned,
            occurrence: thread.occurrence,
            quote: thread.quote,
          },
        ])
      ),
    [placed]
  );

  // The item an address of R8 names, where it is in a thread the column draws: Details closes
  // first, and the item takes the focus in its card once the column is drawn again. Once in each
  // following of the address (`useFollowing`). The item is the one the address names as a node
  // of the outline (`outlineItemRoute`), or the one it marks beside the document it opens
  // (`documentItem`), where the old page's landing sends an item of a document's words.
  const following = useFollowing();
  const named = outlineItemRoute(route, search, address)?.id ?? address.documentItem?.id;
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

  // Places each box at its wanted top against the column's own origin (`regionOrigin`'s rule):
  // the top of the first rectangle of its words in its node's row, else of the row. Before pull
  // request 3125 each stands at its wanted top (the stacking, `useCardStack`, comes at G5). A box
  // whose node has no row drawn - folded or paged away - is not drawn, and the column draws
  // nothing while no box has a row. While any box is drawn, the page's row that holds the stage
  // carries `TEXT_MARGIN_ATTRIBUTE`. The words measured are marked (`highlights.ts`).
  const measure = useCallback(() => {
    const column = aside.current;
    const region = list.current;
    const stage = column?.previousElementSibling ?? null;
    if (column === null || region === null || stage === null) return;
    const origin = regionOrigin(region);
    const ranges: { key: string; range: Range }[] = [];
    let bottom = 0;
    let shown = 0;
    for (const box of region.querySelectorAll<HTMLElement>(`[${THREAD_NODE_ATTRIBUTE}]`)) {
      const nodeId = box.getAttribute(THREAD_NODE_ATTRIBUTE) ?? "";
      const row = rowIn(stage, nodeId, box.dataset.textThreadParent);
      box.hidden = row === null;
      if (row === null) continue;
      shown += 1;
      const key = box.dataset.textThread;
      const boxWords =
        key !== undefined
          ? words.get(key)
          : request === undefined
            ? undefined
            : { ...request, firstLine: false };
      const range =
        boxWords === undefined ? null : wordsRange(row, boxWords, quotedNodeOf(view, nodeId));
      const rect = range?.getClientRects()[0] ?? row.getBoundingClientRect();
      const top = Math.max(0, rect.top - origin);
      box.style.top = `${top}px`;
      bottom = Math.max(bottom, top + box.offsetHeight);
      // A row in its editor draws the marks of its comments itself.
      const editing = row.querySelector('[contenteditable="true"]') !== null;
      if (range !== null && key !== undefined && !editing) ranges.push({ key, range });
    }
    region.style.minHeight = `${bottom}px`;
    column.hidden = shown === 0;
    if (shown === 0) column.parentElement?.removeAttribute(TEXT_MARGIN_ATTRIBUTE);
    else column.parentElement?.setAttribute(TEXT_MARGIN_ATTRIBUTE, "");
    marked.current = ranges;
    mark();
  }, [mark, request, view, words]);

  // After each render: the order of the rows the threads stand by, then the places.
  useLayoutEffect(() => {
    const stage = aside.current?.previousElementSibling ?? null;
    if (stage !== null) {
      const next = rowOrder(stage, view.threads);
      if (next.length !== order.length || next.some((id, at) => order[at] !== id)) setOrder(next);
    }
    measure();
  });

  // Measures again when the stage or a box changes size or the stage draws other rows, at most
  // once in an animation frame.
  useEffect(() => {
    const column = aside.current;
    const stage = column?.previousElementSibling ?? null;
    if (column === null || stage === null) return;
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
    for (const box of column.querySelectorAll(`[${THREAD_NODE_ATTRIBUTE}]`)) sized?.observe(box);
    const changed = new MutationObserver(soon);
    changed.observe(stage, { characterData: true, childList: true, subtree: true });
    return () => {
      sized?.disconnect();
      changed.disconnect();
      if (frame !== undefined) cancelAnimationFrame(frame);
    };
  }, [measure]);

  // The stage's attribute goes with the column.
  useEffect(() => {
    const pageRow = aside.current?.parentElement;
    return () => pageRow?.removeAttribute(TEXT_MARGIN_ATTRIBUTE);
  }, []);

  // A card's reply composer takes the focus as it mounts (`MentionComposer`'s reply prefill), as
  // a card the reader expands asks it to; every card here is drawn expanded, so a thread that
  // loads would pull the reader's focus into the column. The focus a commit found is given back
  // where only a card's own effect moved it there: after every effect under the column, which
  // runs before the column's own. A focus the reader or the address put in the column stays, and
  // so does one in the composer of a new comment, which the reader opened.
  const focusBefore = useRef<Element | null>(null);
  useLayoutEffect(() => {
    focusBefore.current = document.activeElement;
  });

  // The item the address names takes the focus once Details is closed and its card is drawn: the
  // card itself for the thread's root, else its reply's row, which the card marks as the one the
  // address names (`currentReplyId`). After the effects of the stage, which come first on the
  // page, so the stage's own opening focus (`ViewStage`) does not take it back.
  useEffect(() => {
    if (landing === undefined || landed.current === landing.following || detailsOpen) return;
    const thread = placed.find(({ thread: one }) => threadHolds(one, landing.id))?.thread.thread;
    if (thread === undefined) return;
    const card = list.current?.querySelector<HTMLElement>(
      `[data-margin-item="${CSS.escape(thread.key)}"]`
    );
    const target =
      thread.root.comment.id === landing.id
        ? card
        : card?.querySelector<HTMLElement>('li[data-reply][aria-current="true"]');
    if (target === null || target === undefined) return;
    landed.current = landing.following;
    if (!target.hasAttribute("tabindex")) target.tabIndex = -1;
    target.focus();
    focusBefore.current = target;
  });

  useEffect(() => {
    const column = aside.current;
    const before = focusBefore.current;
    const now = document.activeElement;
    if (column === null || now === null || now === before || !column.contains(now)) return;
    if (before !== null && column.contains(before)) return;
    if (now.closest(`[${TEXT_COMPOSER_ATTRIBUTE}]`) !== null) return;
    if (before instanceof HTMLElement && before !== document.body && before.isConnected) {
      before.focus({ preventScroll: true });
    } else if (now instanceof HTMLElement) {
      now.blur();
    }
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
    const stage = aside.current?.previousElementSibling ?? null;
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

  // The thread whose box holds the focus, for the stronger tint of its words.
  const focusThread = (key: string | undefined) => {
    if (key === focusedKey.current) return;
    focusedKey.current = key;
    mark();
  };
  const onFocus = (event: FocusEvent<HTMLDivElement>) => {
    const target = event.target;
    if (!(target instanceof Element)) return;
    focusThread(target.closest<HTMLElement>(`[${THREAD_NODE_ATTRIBUTE}]`)?.dataset.textThread);
  };
  const onBlur = (event: FocusEvent<HTMLDivElement>) => {
    const next = event.relatedTarget;
    if (!(next instanceof Node) || !event.currentTarget.contains(next)) focusThread(undefined);
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
          ref={list}
          style={detailsOpen ? { visibility: "hidden" } : undefined}
        >
          {/* The threads' stored text is the authors' (R4), as `IssueMarginCards` marks the
              margin's cards. */}
          <div className="contents" {...authorText}>
            {placed.map(({ nodeId, parent, thread }) => (
              <div
                className="absolute inset-x-0"
                data-text-thread={thread.thread.key}
                data-text-thread-parent={parent}
                key={thread.thread.key}
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
            {request === undefined ? null : (
              <div
                className="absolute inset-x-0"
                {...{ [THREAD_NODE_ATTRIBUTE]: request.nodeId, [TEXT_COMPOSER_ATTRIBUTE]: "" }}
              >
                <blockquote className="border-l-2 pl-2 text-sm">
                  {quoteLine(request.quote)}
                </blockquote>
              </div>
            )}
          </div>
        </div>
      </NoTooltips>
    </aside>
  );
}
