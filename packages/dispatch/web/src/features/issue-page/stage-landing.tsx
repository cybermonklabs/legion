import { isIdPrefix, itemFromSearch } from "@legion/contracts";
import { useQuery } from "@tanstack/react-query";
import {
  type ReactNode,
  type RefObject,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Navigate, useLocation, useNavigate, useNavigationType } from "react-router-dom";

import { api } from "../../api/client";
import { useAddressedItem } from "../artifacts/ArtifactDocument";
import { useWholeRouteRead } from "../refs/reference-target";
import { buildIssuePath, type IssueRoute } from "../refs/routes";
import { claimBox, rewatch, watchedIn } from "../shell/ViewportAnchor";
import { useOpenedDocument, useOutlineAddress } from "./document-address";
import { useEscapeOrder } from "./escape-order";
import { useFocusedNode } from "./focused-node";
import { type AddressTarget, issueAddress } from "./issue-address";
import { DETAILS_ATTRIBUTE, useMarginDrawer } from "./margin-slot";
import { OpenedDocument } from "./OpenedDocument";
import { wholeOnScreen } from "./on-screen";
import { useAskOutline, useFocusBranch } from "./outline-request";
import { CONTROL_LABELS } from "./page-words";
import { isFilter } from "./presets";
import { scrollingAncestor } from "./scroll-box";
import type { SlotProps } from "./slot-props";
import { useFollowing } from "./slots/following";
import { blockNodeId } from "./tree-drafts";
import { nodeRowSelectorOf } from "./tree-focus";
import { useDocumentToolbars } from "./useDocumentToolbars";
import { useIssueDetail } from "./useIssueDetail";
import { type ItemLanding, isItemRoute, useItemLanding } from "./useItemLanding";
import { abortOnReaderAct } from "./useNodeEditing";

/**
 * What the stage draws before the outline or a view (dispatch://INFRA-1196 part N's R1, R6), for
 * what `main`'s outline does not land itself: `instead`, the address replaced where it writes an
 * id short beside a view's address (`?ask=`, `?comment=`) or under `?mark=`, with the whole id
 * under the same parameter, and nothing while that id or an ask the address names is still read;
 * `document`, the document or file the address opens in place of the outline and of every view
 * (`OpenedDocument.tsx`); and `outline`, the route the outline is drawn at in place of the
 * address's, which is the issue's own for an approval request's address, whose entry the header
 * draws, so the outline lands on no item of it. None, and the stage draws the outline or the
 * view. Every other item an address names - on screen in the outline, an item of another issue,
 * an id written short on an item's own address, several ids, none, a failed read - is the
 * outline's own landing (`slots/body.tsx`), which this does not do again.
 *
 * It also binds the page's own Escape steps between a caret and the outline (R1,
 * `escape-order.ts`), keeps the outline's scroll place across a round trip through a view or a
 * document, puts the focus on an approval request's entry in the header when the address names
 * one, and opens Details at the issue's steps on `/steps`.
 */
export function useStageLanding({ issue, route }: SlotProps): {
  readonly document?: ReactNode;
  readonly instead?: ReactNode;
  readonly outline?: IssueRoute;
} {
  const { hash, search } = useLocation();
  const { highlightTerm, primaryArtifact } = useIssueDetail(issue, route);
  const address = issueAddress(route, search, hash, primaryArtifact?.slug);
  const landing = useItemLanding(route);
  const opened = useOpenedDocument(issue, route);
  const outline = useOutlineAddress(issue.key);
  const navigate = useNavigate();
  const { diffs, showDiff, toolbarSink } = useDocumentToolbars();
  // The ask or comment an address's `?ask=` or `?comment=` names for the outline to open on. An
  // id written short is resolved as a document's address resolves one, and the address is
  // replaced with the whole id under the same parameter, which names the node
  // (`ArtifactDocument`'s `useAddressedItem`); a whole id reads nothing more. Several items that
  // one id names are the outline's to list. Until the short id's read first answers the outline
  // is not drawn: its own landing, which watches that read's state (`useAddressFallback`), can
  // see the answer a render before this does and replace the address with the item's own
  // (`/comments/<id>`). `queryRead` is that read, and sends nothing of its own.
  const namesItem = address.document === undefined && address.target?.kind === "item";
  const queryItem =
    namesItem && !isItemRoute(route) ? (itemFromSearch(search) ?? undefined) : undefined;
  const owner = { key: issue.key, kind: "issue" } as const;
  const queryLanding = useAddressedItem(owner, queryItem);
  const queryRead = useWholeRouteRead(
    queryItem === undefined ? undefined : { id: queryItem.id, key: issue.key, kind: queryItem.kind }
  );
  // `?mark=` names an ask or a comment by its id alone: a comment when the issue's comments hold
  // that id - or, for an id written short, one starting with it - and an ask otherwise. Only an
  // address that opens its document reads them here (a view's address draws the view), under the
  // key the outline reads them by, so it costs no request of its own. A short id is resolved here
  // and written whole under `?mark=`, so the document lands on the item under its whole id.
  const listed = opened === undefined ? undefined : address.documentItem;
  const marked = listed?.kind === "mark" ? listed.id : undefined;
  const comments = useQuery({
    enabled: marked !== undefined,
    queryFn: () => api.listComments(issue.key),
    queryKey: ["comments", issue.key],
  });
  const markedItem =
    marked === undefined || comments.data === undefined
      ? undefined
      : {
          id: marked,
          kind: comments.data.some(
            (comment) =>
              comment.id === marked || (isIdPrefix(marked) && comment.id.startsWith(marked))
          )
            ? ("comment" as const)
            : ("ask" as const),
        };
  const markLanding = useAddressedItem(owner, markedItem, "mark");
  const documentItem =
    listed?.kind !== "mark"
      ? listed
      : markedItem === undefined || isIdPrefix(markedItem.id)
        ? undefined
        : markedItem;
  useApprovalFocus(landing);
  useStepsAddress(route);
  useEscapeOrder({
    documents: address.preset === "documents" && opened === undefined,
    filter: address.filter,
    issueKey: issue.key,
    opened: opened?.route,
    viewOn: isFilter(address.preset),
  });
  // The outline's place is taken and put back only where the stage draws the outline: no view,
  // no document, and not Documents' list.
  useOutlinePlace(
    opened === undefined && address.preset === "conversation",
    address.target !== undefined
  );
  const queryAnswered = useAnsweredOnce(
    queryRead.prefix === undefined || !queryRead.resolution.isPending
  );
  const askAnswered = useAnsweredOnce(route.kind !== "ask" || landing?.kind !== "pending");

  // The rewrite replaces the address with the whole id rather than adding to the reader's
  // history, so Back still leaves the page they came from.
  if (queryLanding.rewrite !== undefined) {
    return { instead: <Navigate replace to={queryLanding.rewrite} /> };
  }
  if (!queryAnswered) return { instead: null };
  if (markLanding.rewrite !== undefined) {
    return { instead: <Navigate replace to={markLanding.rewrite} /> };
  }
  // An approval request is no node of the outline, whose own landing would take its address for
  // the document it reviews and replace it there: the address stays the request's, and the
  // outline is drawn at the issue's own address under the header that holds its entry. Until an
  // ask's own read has said whether it is one, the outline is not drawn, so its landing takes
  // none.
  if (landing?.kind === "approval") return { outline: { key: issue.key, kind: "issue" } };
  if (!askAnswered) return { instead: null };
  if (opened === undefined) return {};
  const openedId = opened.artifact?.id ?? "";
  return {
    document: (
      <OpenedDocument
        artifact={opened.artifact}
        artifactRoute={opened.route}
        highlightTerm={highlightTerm}
        isClosed={issue.closed_at !== null}
        issueKey={issue.key}
        item={documentItem}
        onShowDiff={(next) => showDiff(openedId, next)}
        onToolbarChange={toolbarSink(openedId)}
        // A version picked in the document's own editor opens it at that version, keeping the
        // outline it was opened from, which its way back reads.
        onVersionChange={(version) =>
          navigate(
            buildIssuePath({
              key: issue.key,
              kind: "artifact",
              slug: opened.route.slug,
              ...(version === null ? {} : { version }),
            }),
            { state: { outline } }
          )
        }
        showDiff={diffs.has(openedId)}
      />
    ),
  };
}

/**
 * Keeps the node the reader was last on in a view while the stage turns the view off - by
 * Escape, `t c`, Conversation or Back - and then asks the outline for it, which draws it focused
 * and brings it on screen (R14). Each view's tree sets that node as its rows take the focus
 * (`focused-node.tsx`); the focus leaving a view gives Details the issue back, by the outline's
 * rule (`useReleaseFocus`) or the Decisions view's own, so what is kept is the last node a view
 * set, not what F holds as the view goes. Where the address names a node of its own to land on,
 * the outline lands there instead, and nothing is asked.
 *
 * Where the view set no node of its own, the outline is drawn again at the place the reader left
 * it (`useOutlinePlace`, R1), and the row the reader was on in it - the node F held in the commit
 * the view was drawn in, kept across a switch from one view to another - takes the focus where
 * that place shows it whole; where the reader had scrolled away from it, the first row the place
 * shows whole takes it instead (`focusOnScreen`). So the outline's one tab stop, which a chord
 * back (`t c`) puts the focus on, stands on screen, and nothing moves the page. That node is kept
 * for one turn off; a page opened at a view's address keeps none from before it.
 *
 * It is an element of its own, so a move of the focus draws it alone and not the stage.
 */
export function ViewNodeKeeper({
  lands,
  viewOn,
}: {
  /** Whether the address names a node beside the view for the outline to land on. */
  readonly lands: boolean;
  /** Whether the stage draws a view in the outline's place. */
  readonly viewOn: boolean;
}): null {
  const focused = useFocusedNode();
  const ask = useAskOutline();
  const [focusBranch] = useFocusBranch();
  const kept = useRef<string | undefined>(undefined);
  // The node F held as the view was drawn, until the view goes.
  const before = useRef<string | undefined>(undefined);
  const seen = useRef(focused);
  const wasOn = useRef(viewOn);
  // In the commit that draws the outline, before its own effects take the request.
  useLayoutEffect(() => {
    const from = wasOn.current;
    wasOn.current = viewOn;
    if (from === viewOn) return;
    const own = kept.current;
    const prior = before.current;
    kept.current = undefined;
    before.current = viewOn ? focused?.id : undefined;
    seen.current = focused;
    if (!from || lands) return;
    if (own !== undefined) {
      ask(own, focusBranch);
      return;
    }
    // The stage puts the outline's place back in a layout effect of its own, which runs after
    // this one in the same commit (`useOutlinePlace`): the row is read against that place once
    // the commit is done, before the browser paints and before a chord's frame.
    if (prior !== undefined) queueMicrotask(() => focusOnScreen(prior));
  }, [ask, focusBranch, focused, lands, viewOn]);
  useLayoutEffect(() => {
    if (focused === seen.current) return;
    seen.current = focused;
    if (viewOn && focused !== undefined) kept.current = focused.id;
  }, [focused, viewOn]);
  return null;
}

/** The outline, by its name: the Contents pane's list is a tree too. */
const OUTLINE_TREE = `[role="tree"][aria-label="${CONTROL_LABELS.tree}"]`;

/**
 * Puts the focus, without scrolling, on a row of `node` in the outline where the page shows it
 * whole, else on the outline's first row that the page shows whole, and on nothing where it
 * shows none (`wholeOnScreen`). A row the outline draws already is reached by its own focus, as
 * the outline reaches one (`IssueTree.tsx`'s `reach`), in this task: an ask of the outline goes
 * through two renders, and could come after the frame in which a chord back puts the focus on
 * the outline's tab stop (`useIssueKeymap.ts`'s `reachPart`), which this row then is.
 */
function focusOnScreen(node: string): void {
  const tree = document.querySelector(OUTLINE_TREE);
  if (tree === null) return;
  const rows = [
    ...tree.querySelectorAll<HTMLElement>(nodeRowSelectorOf(node)),
    ...tree.querySelectorAll<HTMLElement>("[data-node-row]"),
  ];
  rows.find(wholeOnScreen)?.focus({ preventScroll: true });
}

/** What the stage keeps of the first thing a view told it for a following of an address that
 *  names a node (`ViewSlotProps.onAddressDrawn`): that following, and whether the reader had
 *  acted since it began (`ReaderActsWatch`) by then. */
export interface ViewTold {
  readonly following: string;
  readonly reader: boolean;
}

/** The mark the stage sets on the box it draws a view in (`useAddressLanding`), which the box's
 *  take of the focus reads (`ViewStage`), with the values it takes (`LANDING_MARKS`). */
export const ADDRESS_LANDING = "data-address-landing";

/** The values of `ADDRESS_LANDING`: the view has not told the stage yet whether it draws the
 *  node the address names (`undecided`), or told it after the reader's first act since the
 *  address was followed (`acted`). */
export const LANDING_MARKS = { acted: "acted", undecided: "undecided" } as const;

/** What the stage sets on the box beside the `undecided` mark (`useAddressLanding`): the id of
 *  the node the address names, where the address says that id whole (`wholeNodeId`), until the
 *  reader's first act since the address was followed. The box's take reads it
 *  (`isAddressedRow`). */
const ADDRESS_NODE = "data-address-node";

/**
 * The id of the node `target` names where the address says it whole, so that a row can be told
 * to be that node's without the tree: an item's own id, which is its node's, and a block of the
 * document the address names (`blockNodeId`). Undefined for every other target, whose node only
 * the tree tells (`pathToTarget`): a block of no named document is the first node holding that
 * block id, the issue's children are their first row, and an ask indexed from a block is that
 * block's node, which no row carries the ask's id on.
 */
function wholeNodeId(target: AddressTarget | undefined): string | undefined {
  if (target?.kind === "item") return target.id;
  if (target?.kind === "block" && target.document !== undefined) {
    return blockNodeId(target.document, target.id);
  }
  return undefined;
}

/**
 * An address that names a node moves the focus once, onto where it lands (dispatch://INFRA-1196
 * decision 413): the row the view draws for the node, the outline's row drawn in the view's place,
 * the line there that the issue holds none of it, or a failed read's Retry. Where the view takes
 * the focus on its tab stop as it draws its rows (`ViewStage`), before every read it needs has
 * settled and it can say whether it draws the node, the focus moved first to the view's first
 * row, then to the outline drawn in its place, then to the line drawn in the outline's place. So
 * from the following's start until the view has told the stage what it draws (`told`), the stage
 * marks the box it draws the view in (`box`) undecided, and the box's take of the focus gives
 * none to a row meanwhile, but to the row of the node the address names (`isAddressedRow`): its
 * focus is the landing itself, which the view takes at once as it draws that row its tab stop,
 * as a comment the Spec view draws while the statements' reads are out. Told after the reader's
 * first act since the address was followed (`acts`), it marks the box `acted`: the landing moves
 * nothing then, as the view's own landing on its row does nothing after such an act, and the
 * named row is no landing from that act on either. The marks are set in the commit that draws
 * the view, before the box's take runs, and read again as they change. An address that names no
 * node (`target`) leaves the box unmarked, so the view's tab stop takes the focus as its rows
 * are drawn.
 */
export function useAddressLanding(
  box: RefObject<HTMLElement | null>,
  target: AddressTarget | undefined,
  following: string,
  told: ViewTold | undefined,
  acts: RefObject<AbortController | undefined>
): void {
  const mark =
    target === undefined
      ? undefined
      : told?.following !== following
        ? LANDING_MARKS.undecided
        : told.reader
          ? LANDING_MARKS.acted
          : undefined;
  const node = mark === LANDING_MARKS.undecided ? wholeNodeId(target) : undefined;
  // At every commit of the stage, so a box drawn anew for another view takes the marks too.
  useLayoutEffect(() => {
    const element = box.current;
    if (element === null) return;
    if (mark === undefined) element.removeAttribute(ADDRESS_LANDING);
    else element.setAttribute(ADDRESS_LANDING, mark);
    // The following's watch of the reader's acts (`ReaderActsWatch`), which the stage draws and
    // which starts it in a layout effect of the same commit, before this one: an act ends it,
    // and the node's mark with it, though the act draws nothing.
    const watched = acts.current;
    if (node === undefined || watched === undefined || watched.signal.aborted) {
      element.removeAttribute(ADDRESS_NODE);
      return;
    }
    element.setAttribute(ADDRESS_NODE, node);
    const acted = () => element.removeAttribute(ADDRESS_NODE);
    watched.signal.addEventListener("abort", acted);
    return () => watched.signal.removeEventListener("abort", acted);
  });
}

/** Whether `row`, the tab-stop row of the view drawn in `box`, is the row of the node the address
 *  names while the stage marks the box undecided and the reader has not acted since the address
 *  was followed (`useAddressLanding`): its focus is then the landing itself, the one move. */
export function isAddressedRow(box: HTMLElement, row: Element): boolean {
  const node = box.getAttribute(ADDRESS_NODE);
  return node !== null && row.matches(nodeRowSelectorOf(node));
}

/** An approval request's own address puts the focus on the approval the header draws for the
 *  document it reviews, since the outline draws no node for it (`useItemLanding`'s `approval`,
 *  R20). It runs once the request is resolved, and again only for another landing: after that
 *  the focus is the reader's. */
function useApprovalFocus(landing: ItemLanding | undefined): void {
  const approvalOf = landing?.kind === "approval" ? landing.artifactId : undefined;
  useEffect(() => {
    if (approvalOf === undefined) return;
    document
      .querySelector<HTMLElement>(`[data-approval-artifact="${CSS.escape(approvalOf)}"]`)
      ?.focus();
  }, [approvalOf]);
}

/**
 * Whether `answered` has held at any render since the address was followed (`useFollowing`): a
 * read the stage waits for has answered once. A later run of that read - a Retry, or a failed
 * read asked again as the outline mounts, which sets it back to pending - holds nothing back,
 * so what the outline drew for its answer stays drawn while it runs.
 */
function useAnsweredOnce(answered: boolean): boolean {
  const following = useFollowing();
  const [answeredIn, setAnsweredIn] = useState<string | undefined>(undefined);
  if (answered && answeredIn !== following) setAnsweredIn(following);
  return answered || answeredIn === following;
}

/** How many frames `/steps` waits for Details to draw the steps and the history above them once
 *  it has opened Details: about two seconds, the history's first read on a cold load. */
const STEPS_FRAMES = 120;

/** The heading of the steps inside Details (`DetailsSteps.tsx`), which takes the focus. */
const STEPS_HEADING = `aside[${DETAILS_ATTRIBUTE}] [data-testid="issue-steps"] h2`;

/** The history above the steps in Details, drawn: its list, or the line that it failed
 *  (`IssueHistory.tsx`). Its entries arriving after the steps took the focus would push them out
 *  of Details' view, which the hold of the heading (`holdSteps`) then brings back. */
const HISTORY_DRAWN = `aside[${DETAILS_ATTRIBUTE}] [data-testid="issue-history"] :is(ol, [role="alert"])`;

/**
 * Keeps the steps' heading at the top of Details' view after `reachSteps` brought it there, until
 * the reader acts or `landing` ends. Content above the steps that draws after the landing - the
 * history's entries, the pinned entries and what each of them reads - moves the heading down,
 * and where the box was clamped at its end when the heading was brought up, it moves it out of the
 * view: the box is scrolled again at each change of height around the heading (`watchedIn`).
 * The hold claims the box (`claimBox`), so the history's anchor keeps no place in it meanwhile,
 * as Details' hold of a landed place does (`details-more.tsx`), and it ends with the claim, at the
 * reader's first act (`abortOnReaderAct`), where Details closes and where `landing` ends.
 */
function holdSteps(heading: HTMLElement, landing: AbortSignal): void {
  const box = scrollingAncestor(heading);
  if (box === null) return;
  const acts = abortOnReaderAct(() => release());
  let watched: ReadonlySet<Element> = new Set();
  const watch = () => {
    const now = watchedIn(heading, box);
    rewatch(sizes, watched, now);
    watched = now;
  };
  const sizes = new ResizeObserver(() => {
    if (!heading.isConnected || heading.closest("[hidden]") !== null) {
      release();
      return;
    }
    watch();
    heading.scrollIntoView({ block: "start" });
  });
  const unclaim = claimBox(box, () => release());
  function release(): void {
    landing.removeEventListener("abort", release);
    acts.abort();
    sizes.disconnect();
    unclaim();
  }
  landing.addEventListener("abort", release, { once: true });
  watch();
}

/** Puts the focus on the steps' heading inside Details and brings it to the top of Details' view,
 *  once Details has drawn it and the history above it; `left` frames are waited for that, and
 *  then the heading is reached as it stands, and held there (`holdSteps`). A page that never
 *  draws it leaves the focus where it was. */
function reachSteps(left: number, landing: AbortSignal): void {
  if (landing.aborted) return;
  const heading = document.querySelector<HTMLElement>(STEPS_HEADING);
  const drawn = heading !== null && heading.closest("[hidden]") === null;
  if (!(drawn && document.querySelector(HISTORY_DRAWN) !== null) && left > 0) {
    requestAnimationFrame(() => reachSteps(left - 1, landing));
    return;
  }
  if (!drawn) return;
  heading.focus({ preventScroll: true });
  heading.scrollIntoView({ block: "start" });
  holdSteps(heading, landing);
}

/**
 * `/steps`, the address of today's Steps tab, opens Details at the issue's steps, which part M
 * draws under the history there (part N's R6): Details opens, as `/activity` opens it at the
 * history, and the steps' heading takes the focus once Details draws it, as `/collaborators`
 * puts it on that row's control. It opens once per arrival at the address, which is one following
 * of it (`useFollowing`), so the reader's Close stands until they arrive again - by `t e`, which
 * opens that address (`useIssueKeymap.ts`), or by Back to it from another address, as
 * `/activity` opens again (`details-more.tsx`'s `DetailsHistory`). The search waits two frames
 * first, as `useOpenDetailsAt` does, for the drawer gives the focus to its own Close as it opens.
 * The landing and the hold of the heading at the top of Details' view (`holdSteps`) end where
 * the following does, and with the page.
 */
function useStepsAddress(route: IssueRoute): void {
  const following = useFollowing();
  const openDetails = useMarginDrawer()?.setOpen;
  const steps = route.kind === "steps";
  const openedFor = useRef<string | undefined>(undefined);
  const landing = useRef<AbortController | undefined>(undefined);
  useEffect(() => {
    if (!steps) {
      if (openedFor.current !== following) openedFor.current = undefined;
      landing.current?.abort();
      return;
    }
    if (openedFor.current === following) return;
    openedFor.current = following;
    landing.current?.abort();
    const own = new AbortController();
    landing.current = own;
    openDetails?.(true);
    requestAnimationFrame(() => requestAnimationFrame(() => reachSteps(STEPS_FRAMES, own.signal)));
  }, [following, openDetails, steps]);
  useEffect(() => () => landing.current?.abort(), []);
}

/**
 * Keeps the outline's scroll place across a round trip through a view or a document (R1, the
 * plan's ST-5). The place is recorded on each scroll while the outline is drawn, for the
 * following it is drawn in (`useFollowing`), and kept while a view or a document is drawn in its
 * place. When the outline is drawn again it is put back before the browser paints: by Back, the
 * place of the following Back returns to; by Conversation, `t c` or Escape off a view - each an
 * address of the outline that names no node - or by the way back of a document a chip opened, to
 * the address the chip was pressed at (`document-address.ts`' `escapeWayBack`), the outline's last
 * place. A new address that names a node, a link to it, puts nothing back: it lands by its own
 * rules (`slots/body.tsx`). A scroll the browser makes as a shorter view clamps the page lands
 * after that view is drawn, so it is never the outline's.
 */
function useOutlinePlace(drawn: boolean, namesNode: boolean): void {
  const { pathname, search } = useLocation();
  const following = useFollowing();
  const move = useNavigationType();
  // The address as the way back of a document a chip opened writes it, its fragment aside.
  const address = `${pathname}${search}`;
  const places = useRef(new Map<string, number>());
  const last = useRef<{ readonly address: string; readonly top: number } | undefined>(undefined);
  const now = useRef({ address, drawn, following });
  useEffect(() => {
    const record = () => {
      const { address, drawn, following } = now.current;
      if (!drawn) return;
      places.current.set(following, window.scrollY);
      last.current = { address, top: window.scrollY };
    };
    window.addEventListener("scroll", record, { passive: true });
    return () => window.removeEventListener("scroll", record);
  }, []);
  useLayoutEffect(() => {
    const wasDrawn = now.current.drawn;
    now.current = { address, drawn, following };
    if (wasDrawn || !drawn) return;
    const returned = move === "POP" ? places.current.get(following) : undefined;
    const outline =
      last.current !== undefined && (!namesNode || last.current.address === address)
        ? last.current.top
        : undefined;
    const top = returned ?? outline;
    if (top !== undefined) window.scrollTo({ top });
  }, [address, drawn, following, move, namesNode]);
}
