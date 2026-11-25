import { useQuery } from "@tanstack/react-query";
import {
  type ReactNode,
  type RefObject,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
} from "react";
import { Link, Navigate, useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import {
  primaryButtonBg,
  primaryButtonHoverBg,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../../theme/classes";
import { ItemMatchList } from "../../issue/ItemMatchList";
import type { ItemLanding, ItemRoute } from "../../issue/useItemLanding";
import { buildIssuePath } from "../../refs/routes";
import {
  type AddressFallback,
  landingPath,
  namedBy,
  useAddressFallback,
  useAddressTarget,
} from "../address-target";
import { useOutlineEditing } from "../IssuePresetPanels";
import { IssueTree } from "../IssueTree";
import { type IssueAddress, issueAddress, presetPath } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import type { TreeNode } from "../issue-tree";
import { NoTooltips } from "../NoTooltips";
import { useOutlineReads } from "../outline-reads";
import { useOutlineRequest } from "../outline-request";
import { KEY_REPORTS } from "../page-structure";
import { EMPTY_STATE_LINES, type Preset } from "../page-words";
import { answerHolds, filterHolding, filterOfUndrawn, presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { specSummary } from "../spec-summary";
import { focusIsLoose } from "../tree-focus";
import { pathTo } from "../tree-fold";
import {
  outlineQuery,
  type TreeAnswer,
  useNextTreePage,
  useOlderComments,
  useReadUntilHeld,
} from "../tree-read/tree-read";
import { TreeReadFailed } from "../tree-read/view-read";
import { abortOnReaderAct } from "../useNodeEditing";
import { FailedReadLine } from "./failed-read-line";
import { useFollowing } from "./following";

/** Filled by INFRA-1196 part G, "A node's row", then part I, "The outline". */
export function BodySlot({ acts: given, issue, route }: BodySlotProps): ReactNode {
  // The outline: the issue's one tree as the Conversation view leaves it, every node of it
  // unfolded and whole (R11). The spec's summary callout is no row of it: the Spec view draws it
  // as the issue's three sentences.
  const { hash, search } = useLocation();
  const following = useFollowing();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  // The tree, and whether every read it is built from holds its answer (`useOutlineReads`).
  // Until then a tree with no node is one whose nodes have not arrived, and the outline's line
  // that the issue holds nothing would be false on every cold load, so the outline draws nothing
  // in its place; so would `q` and `w` saying there is no question or decision to go to, so they
  // say nothing. A read that stands failed holds nothing either, and the outline says so over
  // its rows (`failed`). Under a words filter the line that nothing holds the words waits on the
  // names as well (`namesHeld`), and the outline draws nothing in the line's place until they are
  // held. With no filter on the line reads no words, and does not wait on the names.
  const { failed, namesHeld, reads, readsHeld, readsSettled, root } = useOutlineReads(issue);
  const key = issue.key;
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the words filter would not have matched still draws it. An item
  // the tree holds no node of falls back to the item's own page once the reads are held, unless
  // the tree has held it since the address was followed (`useAddressFallback`); a turn the
  // outline leaves out lands on its nearest drawn ancestor, which the view keeps as it keeps the
  // address's own node; and a block or the children the tree holds no node of are said to be
  // so, in the outline's place.
  const addressed = useAddressTarget(root, address);
  const zoomed = address.zoom === undefined ? undefined : pathTo(root, address.zoom);
  // An address that names a node no page of the outline read yet holds - its own node, or the
  // node Graph's address zooms to - reads the read's next pages until one brings it, or none is
  // left (R15), and lands on it then as on a node the first page held: until the reading ends,
  // the landing and the outline's first focus wait on it as on the reads the tree is built from.
  // A next page refused is no last page: until a read of it answers - the control after the
  // outline's last row reads it again - the landing waits on it still, so the address keeps its
  // node and its zoom, while the outline takes its first focus as the reads settle.
  const unloaded =
    address.document === undefined &&
    ((address.target !== undefined && addressed.reveal === undefined) ||
      (address.zoom !== undefined && zoomed === undefined));
  const pages = useReadUntilHeld(issue, unloaded);
  const pagesHeld = !pages.reading && !pages.refused;
  const fallback = useAddressFallback({
    address,
    issue,
    reads,
    readsHeld: readsHeld && pagesHeld,
    reveal: addressed.reveal,
    root,
    route,
    search,
  });
  const ancestor = fallback?.kind === "agents-only" ? fallback.ancestor : undefined;
  const ancestorNamed = useMemo(
    () => (ancestor === undefined ? undefined : namedBy(ancestor)),
    [ancestor]
  );
  const named = ancestorNamed ?? addressed.named;
  const memo = useMemo(() => specSummary(root, specSlug ?? "")?.id, [root, specSlug]);
  // Graph draws the outline from Graph's read whole (`slots/view-graph.tsx`); every other
  // address draws Conversation, the page with no filter on.
  const outline = address.preset === "graph" ? "graph" : "conversation";
  const view = useMemo(
    () => presetView(root, outline, address.filter, named, memo),
    [address.filter, memo, named, outline, root]
  );
  // The older comments of the outline read, whose control stands above the outline's first row
  // under the issue while they remain, and its next page, whose control follows the outline's last
  // row while its answer names one (R15).
  const older = useOlderComments(issue);
  const more = useNextTreePage(issue);
  // `addressView` decides what opens in place of an address the outline cannot draw.
  // A load of Conversation sends no Graph read, so where Conversation's read holds no node of the
  // address's and the page holds no Graph read, the body sends Graph's read here, once, reads its
  // next pages until one brings the node or none is left, as Graph's own address does
  // (`useReadUntilHeld`), and decides once that reading has ended: Graph's `?node=` where its
  // pages hold the node, else the issue's own address without it, as where Graph's read, or a
  // next page of it, is refused. Until then the body draws what it draws while its own reads are
  // out for an address, and the outline takes no first focus. It is the read Graph's slot reads,
  // under the same key, which going there reads from the cache; a page that holds it already
  // sends nothing.
  const unheldId =
    address.preset === "conversation" &&
    fallback?.kind === "absent" &&
    fallback.target.kind === "node"
      ? fallback.target.id
      : undefined;
  const unheldNode = unheldId !== undefined;
  const graph = useQuery({
    ...outlineQuery(key, "graph"),
    enabled: (query) => unheldNode && query.state.status === "pending",
  });
  const graphHolds = graph.data?.nodes?.some((node) => node.id === unheldId) === true;
  const graphPages = useReadUntilHeld(
    issue,
    unheldNode && graph.data !== undefined && !graphHolds,
    "graph"
  );
  const graphOut = unheldNode && (graph.status === "pending" || graphPages.reading);
  const zoomUnheld = address.zoom !== undefined && readsHeld && pagesHeld && zoomed === undefined;
  const undrawn: AddressView | undefined = graphOut
    ? undefined
    : (addressView(address, addressed.reveal, root, fallback, graph.data) ??
      (zoomUnheld ? { keepsNode: true, preset: "graph" } : undefined));
  const keptZoom = zoomed === undefined ? undefined : address.zoom;
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const request = useOutlineRequest();
  // Whether the reader has acted since they followed the address, after which the address's row
  // drawn after the act does nothing to the page (`IssueTree`'s `addressActs`).
  const own = useRef<AbortController | undefined>(undefined);
  const acts = given ?? own;
  // Whether the focus is on the body's box - a row of the outline, or a line drawn in its place -
  // as it stays while those swap (`useFocusHeld`).
  const { box, held } = useFocusHeld();
  // Whether a line drawn in the outline's place takes the focus (`useFocusOnDraw`): always while
  // the reader has not acted since they followed the address, and after an act only where the
  // focus was on the body's box and nothing holds it as the line is drawn - it fell to the page's
  // body with what the line replaces. The mark stays set where the focus fell to the body with no
  // focusout leaving the box - a row the page took out while the outline stayed - and a control
  // outside the box the reader pressed after that holds the focus, which it keeps.
  const takesFocus = useCallback(
    (drawn: HTMLElement) =>
      acts.current?.signal.aborted !== true || (held.current && focusIsLoose(drawn)),
    [acts, held]
  );
  // The document a row of the outline is being edited in, and that document's one editor, which
  // the body draws for the row to take onto its own stage (`useOutlineEditing`).
  const { onEditing, panels } = useOutlineEditing(issue);
  const isClosed = issue.closed_at !== null;
  return (
    <NoTooltips>
      {given === undefined && <ReaderActsWatch into={own} key={following} />}
      {/* The outline's measure, the prototype's `#pane`: at most 1000 px, in the middle of the
          column. On a phone 4 px from the left edge and 10 px from the right, as the
          prototype's `#reading` is there, so the bullets keep the picture's place. Its
          `data-issue-body` is the body's own mark, which tells it from a view drawn in its place
          (`StageSlot`): a view draws a tree named as the outline is. */}
      <div
        className="mx-auto flex w-full max-w-[1000px] min-w-0 flex-col max-[761px]:-mr-1 max-[761px]:-ml-2.5 max-[761px]:w-auto"
        data-issue-body=""
        ref={box}
      >
        {/* A tree read refused before it first answered leaves the tree the issue alone, which
            draws no row, so the outline says so over the tree, with a Retry. */}
        <TreeReadFailed issue={issue} />
        {undrawn !== undefined ? (
          <Navigate replace to={viewPath(key, undrawn, keptZoom, search, hash)} />
        ) : fallback?.kind === "landing" ? (
          // Keyed on the following, so a link to the same item followed again is drawn anew and
          // takes the focus again.
          <ItemFallback fallback={fallback} key={following} takesFocus={takesFocus} />
        ) : fallback?.kind === "absent" && fallback.target.kind !== "node" ? (
          // Keyed on the following, as the item's own page is.
          <NotFoundLine
            heading={ABSENT_LINES[fallback.target.kind].heading}
            issueKey={key}
            key={following}
            line={ABSENT_LINES[fallback.target.kind].line(key)}
            takesFocus={takesFocus}
          />
        ) : (
          <IssueTree
            addressActs={acts}
            addressKey={following}
            artifacts={artifacts}
            context={view.context}
            emptyLine={
              address.filter === ""
                ? EMPTY_STATE_LINES[outline]
                : namesHeld
                  ? EMPTY_STATE_LINES.no_match
                  : undefined
            }
            failedReads={failed}
            focusReveal
            // The words filter's Escape comes before the outline is left.
            holdsEscape={address.filter !== ""}
            isClosed={isClosed}
            {...(ancestor === undefined ? {} : { landingReport: KEY_REPORTS.agents_thread })}
            more={more}
            older={older}
            onEditing={onEditing}
            readsHeld={readsHeld}
            readsSettled={readsSettled && !pages.reading && !graphOut}
            {...(request === undefined ? {} : { request })}
            // The children's own address lands on their section above the outline
            // (`children-section.tsx`), and on no row of the outline (R14).
            reveal={
              ancestor ?? (address.target?.kind === "children" ? undefined : addressed.reveal)
            }
            root={view.root}
            rootRow={view.rootRow}
            {...(address.zoom === undefined ? {} : { zoomAt: address.zoom })}
          />
        )}
      </div>
      {panels}
    </NoTooltips>
  );
}

/** Where an address opens in its own view's place (`addressView`): `preset`'s own address, with
 *  the address's search and fragment, less its `?node=` where `keepsNode` is false (`viewPath`). */
interface AddressView {
  readonly keepsNode: boolean;
  readonly preset: Preset;
}

/**
 * The view an address on the outline's own address, Conversation's or Graph's, opens in the
 * outline's place (`BodySlot`): `reveal` is the chain down to the node it names in the tree `root`
 * where the tree holds it, `fallback` where the address lands once the tree holds none of it
 * (`useAddressFallback`), and `graphRead` Graph's read as the page holds it: for a node address
 * Conversation's read holds none of, the body sends Graph's read where the page holds none, and
 * asks this once it has answered or been refused. On Conversation's address, a node the tree
 * holds that Conversation draws no row of - a plan task, a result - opens the filter of its kind
 * (`filterOfUndrawn`), and a block Conversation's read holds no node of the filter whose view
 * answer holds it (`filterHolding`), else Graph where Graph's read answers it (`answerHolds`). A
 * node address (`?node=`) is read by the outline alone (`issueAddress`): one whose node
 * Conversation's tree holds and draws no row of opens Graph at that node, since Graph's read is
 * the whole read, which holds every node Conversation's does; one whose node Conversation's read
 * holds none of opens Graph at it where Graph's read answers a node of its id, and else - Graph's
 * read lacking it, or refused - Conversation's own address without it. On Graph's address, a
 * node its read holds none of opens Graph's address without it, zoomed as the address zooms where
 * the tree holds that node (`viewPath`). No line is drawn for a node no read holds. In a filter's
 * place, where the stage draws the outline because that filter's view draws no row for the node
 * (`StageSlot`), the outline is Conversation's, and a node it draws no row of opens the filter of
 * its kind or Graph as on Conversation's address, but never the filter it stands in, whose view
 * drew none of it. Undefined for every other address.
 */
export function addressView(
  address: Pick<IssueAddress, "preset" | "target">,
  reveal: readonly TreeNode[] | undefined,
  root: TreeNode,
  fallback: AddressFallback | undefined,
  graphRead: TreeAnswer | undefined
): AddressView | undefined {
  const unheld =
    fallback?.kind === "absent" && fallback.target.kind === "node" ? fallback.target : undefined;
  if (address.preset === "graph") {
    return unheld === undefined ? undefined : { keepsNode: false, preset: "graph" };
  }
  if (reveal !== undefined) {
    const filter = filterOfUndrawn(reveal.at(-1) ?? root);
    if (filter === undefined || filter === address.preset) return undefined;
    return { keepsNode: true, preset: address.target?.kind === "node" ? "graph" : filter };
  }
  // A node address is the outline's alone (`issueAddress`), so a filter's place opens none.
  if (unheld !== undefined && address.preset !== "conversation") return undefined;
  if (unheld !== undefined) {
    return (graphRead?.nodes ?? []).some((node) => node.id === unheld.id)
      ? { keepsNode: true, preset: "graph" }
      : { keepsNode: false, preset: "conversation" };
  }
  if (fallback?.kind !== "absent" || fallback.target.kind !== "block") return undefined;
  const preset =
    filterHolding(root.views, fallback.target) ??
    (answerHolds(graphRead, fallback.target) ? "graph" : undefined);
  return preset === undefined || preset === address.preset
    ? undefined
    : { keepsNode: true, preset };
}

/** The address `view` opens for an address of `search` and `hash`: its preset's own address -
 *  Graph's zoomed to `zoom` (`/graph/<id>`) where the address zooms to a node the tree holds -
 *  with the same search, less its `?node=` where the view keeps no node, and the same fragment. */
function viewPath(
  key: string,
  view: AddressView,
  zoom: string | undefined,
  search: string,
  hash: string
): string {
  const path =
    view.preset === "graph" && zoom !== undefined
      ? buildIssuePath({ key, kind: "graph", node: zoom })
      : presetPath(key, view.preset);
  if (view.keepsNode) return `${path}${search}${hash}`;
  const query = new URLSearchParams(search);
  query.delete("node");
  const rest = query.toString();
  return `${path}${rest === "" ? "" : `?${rest}`}${hash}`;
}

/**
 * Starts the controller the reader's first act since the address was followed aborts
 * (`READER_ACTS`), and puts it in `into`: one for each following, as the body keys this on the
 * following (`useFollowing`) where it is handed no controller's ref - drawn by the stage in a
 * view's place where the focus was the view's (`StageSlot`), or bare in the outline's own tests -
 * and the stage keys its own so. It draws nothing. It is
 * started in a layout effect, which runs before every passive effect of that commit, and every
 * reader of the controller reads it in a passive effect (`IssueTree.tsx`'s effect that reads
 * `addressActs`, `useFocusOnDraw`'s that calls `takesFocus`): a line drawn in the commit of the
 * following itself reads the following's own controller.
 */
export function ReaderActsWatch({ into }: { into: ReaderActs }): ReactNode {
  useLayoutEffect(() => {
    const watched = abortOnReaderAct();
    into.current = watched;
    return () => watched.abort();
  }, [into]);
  return null;
}

/**
 * Whether the focus is on the body's box (`box`, given to it as its ref), which stays as the
 * outline and every line drawn in its place swap: what a line drawn in the outline's place reads
 * to take the focus the element it replaces took off the page with it (`BodySlot`'s `takesFocus`).
 *
 * It is read by listeners of the page's own on the box, which hear every focus inside its DOM,
 * an element a React portal draws there included - the document a row is being edited in, whose
 * React parent is the body's `panels`, beside the box - where React's `onFocus` and `onBlur` on
 * the box hear only what React draws inside it. The focus going into the box marks it held. The
 * focus leaving it - for an element outside it, or for none, as Tab out of the outline, the
 * page's last tab stop, or a press on blank space leaves it - clears the mark, unless the element
 * it left has left the page: Chromium fires blur and focusout, its relatedTarget null, at a
 * focused element React takes out of the page, while that element is still connected (measured
 * in Chromium 156), so the mark is cleared a microtask later, and only where that element is
 * still connected and the focus has not come back into the box. React commits synchronously, so
 * an element it took out is detached by then, and the line drawn in its place takes the focus.
 * The listeners are added in a layout effect, before the passive effect that focuses a line
 * drawn in the same commit (`useFocusOnDraw`), which they hear.
 */
function useFocusHeld(): { box: RefObject<HTMLDivElement | null>; held: FocusHeld } {
  const box = useRef<HTMLDivElement | null>(null);
  const held = useRef(false);
  useLayoutEffect(() => {
    const element = box.current;
    if (element === null) return;
    const listening = new AbortController();
    element.addEventListener(
      "focusin",
      () => {
        held.current = true;
      },
      { signal: listening.signal }
    );
    element.addEventListener(
      "focusout",
      (event) => {
        const to = event.relatedTarget;
        if (to instanceof Node && element.contains(to)) return;
        const left = event.target;
        queueMicrotask(() => {
          if (left instanceof Node && left.isConnected) {
            held.current = element.contains(document.activeElement);
          }
        });
      },
      { signal: listening.signal }
    );
    return () => listening.abort();
  }, []);
  return { box, held };
}

/**
 * The address's item's own page, drawn in the outline's place where the tree holds no node of
 * the item and the old page's landing has read it (`useAddressFallback`), as today's page draws
 * it (`issue/IssuePage.tsx`, `IssueDetail`): an item of another issue, or one beside a document,
 * is gone to, and so is the whole id an id written short names; the several items a short id
 * names are listed to choose from; and an item the issue holds none of, or whose read failed,
 * is said to be so. The list and the failure take the focus as they are drawn, so it is on the
 * line that names the item and never left on the page's body, unless the reader has acted since
 * the address was followed: the focus is then where they put it, nowhere included - a press on
 * blank space. A line drawn in place of what held the focus on the body's box takes it while
 * nothing else holds it, as the focus fell to the page's body with what it was on, which is no
 * place the reader put it: a row of the outline the reader had moved to, the outline replaced as
 * the item's read answered, or a Retry pressed, and its read answering that the item is gone, or
 * naming several. `takesFocus` says which (`BodySlot`).
 *
 * Going to a document goes to the address that opens it with the item marked (`landingPath`),
 * never to the one the old landing writes (`?comment=`), which this page reads as the item's
 * node again and would go to the same document again and again.
 */
function ItemFallback({
  fallback,
  takesFocus,
}: {
  fallback: Extract<AddressFallback, { kind: "landing" }>;
  takesFocus: TakesFocus;
}): ReactNode {
  const { failure, landing, retrying, route } = fallback;
  // The address is replaced rather than added to the reader's history, so Back still leaves the
  // page they came from.
  if (landing.kind === "redirect" || landing.kind === "rewrite") {
    return <Navigate replace to={landingPath(landing.to)} />;
  }
  if (landing.kind === "ambiguous") {
    return <MatchList candidates={landing.candidates} route={route} takesFocus={takesFocus} />;
  }
  const noun = ITEM_NOUNS[route.kind];
  if (landing.kind === "missing") {
    return (
      <NotFoundLine
        heading={`${noun} not found`}
        issueKey={route.key}
        line={`That link doesn't name ${route.kind === "ask" ? "an" : "a"} ${noun.toLowerCase()} on ${route.key} — it may have been deleted.`}
        takesFocus={takesFocus}
      />
    );
  }
  return (
    <ItemReadFailed
      failure={failure}
      noun={noun}
      onRetry={landing.retry}
      retrying={retrying}
      takesFocus={takesFocus}
    />
  );
}

/** The several items an id written short names, to choose from (`ambiguous`), which take the
 *  focus as they are drawn (`useFocusOnDraw`). */
function MatchList({
  candidates,
  route,
  takesFocus,
}: {
  candidates: Extract<ItemLanding, { kind: "ambiguous" }>["candidates"];
  route: ItemRoute;
  takesFocus: TakesFocus;
}): ReactNode {
  const list = useFocusOnDraw<HTMLDivElement>(takesFocus);
  return (
    <div ref={list} tabIndex={-1}>
      <ItemMatchList candidates={candidates} route={route} />
    </div>
  );
}

/** What the reader's first act since the address was followed aborts (`ReaderActsWatch`). */
type ReaderActs = RefObject<AbortController | undefined>;

/** The body's props: the slot's own, and the reader's acts since the address was followed, which
 *  the stage watches and hands the body it draws (`acts`). Left out, the body watches its own from
 *  its first draw: the stage leaves it out where it draws the body in a view's place while the
 *  focus was the view's (`StageSlot`), and the outline's own tests draw the body bare. */
type BodySlotProps = SlotProps & { readonly acts?: ReaderActs };

/** Whether the focus is on the body's box, or was as the element holding it there left the
 *  page (`BodySlot`'s `held`). */
type FocusHeld = RefObject<boolean>;

/** Whether an element drawn in the outline's place, `drawn`, takes the focus as it is drawn
 *  (`BodySlot`'s `takesFocus`). */
type TakesFocus = (drawn: HTMLElement) => boolean;

/**
 * The element its ref is given to takes the focus once it is drawn where `takesFocus` says it
 * does: while the reader has not acted since the address was followed, as the landing on a row
 * does (`IssueTree.tsx`), or where the focus was on the body's box and nothing else holds it as
 * the element is drawn, since what it is drawn in place of took the focus off the page with it -
 * a row of the outline the reader had moved to, or a Retry they pressed, whose read then
 * answered. Anywhere else the focus is the reader's after such an act, nowhere included: a line
 * drawn after they pressed blank space, or Tab from the outline to the page's body, takes none,
 * as the row takes none, and one drawn after they pressed a control outside the box leaves the
 * focus on that control.
 */
function useFocusOnDraw<T extends HTMLElement>(takesFocus: TakesFocus) {
  const element = useRef<T>(null);
  useEffect(() => {
    const drawn = element.current;
    if (drawn !== null && takesFocus(drawn)) drawn.focus();
  }, [takesFocus]);
  return element;
}

/** What each kind of item is called: a copy of `issue/IssuePage.tsx`'s `itemNoun`. */
const ITEM_NOUNS: Readonly<Record<ItemRoute["kind"], string>> = {
  ask: "Ask",
  comment: "Comment",
  message: "Message",
};

/** What the page says in the outline's place of a block or the children an address names that
 *  the tree holds no node of (`absent`), for the issue `key`. A node no read holds has no line:
 *  its address opens the outline's own address without it (`addressView`). */
const ABSENT_LINES: Readonly<
  Record<
    Exclude<Extract<AddressFallback, { kind: "absent" }>["target"]["kind"], "node">,
    { readonly heading: string; readonly line: (key: string) => string }
  >
> = {
  // A block of a document the outline draws no row for - a paragraph, a heading, a table - is no
  // node of the tree as much as a block id no document holds, so the words fit both.
  block: {
    heading: "No row for this block",
    line: (key) => `That link names no row of ${key}'s outline.`,
  },
  children: {
    heading: "No child issues",
    line: (key) => `That link names the child issues of ${key}, which has none.`,
  },
};

/**
 * What an address names that this issue's page does not hold - an item the issue holds none of,
 * a block no node carries, the children of an issue with none - said where the outline would
 * be, with the focus on its heading (`useFocusOnDraw`) and a way back to the issue's own address.
 *
 * A copy of the missing line of `issue/IssuePage.tsx`'s `ItemLandingFailure`, which that file
 * keeps to itself, with its words, as `IssueOutlinePage.tsx` copies today's failed states: today's
 * page is changed by additions alone, and the copy goes when that page is removed. It is drawn
 * as the shell's `NotFoundPage` draws its line, but here: that page names the whole window "Not
 * found", where the issue's page stands around it. The heading is the page's second level, under
 * the issue's.
 */
function NotFoundLine({
  heading,
  issueKey,
  line,
  takesFocus,
}: {
  heading: string;
  issueKey: string;
  line: string;
  takesFocus: TakesFocus;
}): ReactNode {
  const title = useFocusOnDraw<HTMLHeadingElement>(takesFocus);
  return (
    <section>
      <h2 className="mb-2 text-2xl font-semibold" ref={title} tabIndex={-1}>
        {heading}
      </h2>
      <p className={textSecondaryOnCanvas}>{line}</p>
      <Link
        className={`mt-4 inline-block rounded-lg px-3 py-2 text-sm font-medium ${primaryButtonBg} ${primaryButtonHoverBg}`}
        to={buildIssuePath({ key: issueKey, kind: "issue" })}
      >
        {`Back to ${issueKey}`}
      </Link>
    </section>
  );
}

/**
 * An address's item whose read failed (`unavailable`), named in the page, where the outline
 * would be, with the focus on its heading, as `issue/IssuePage.tsx`'s `ItemLandingFailure` names
 * it. Its Retry is the new page's failed read's line (`FailedReadLine`): busy while the read runs
 * again (`retrying`) and never disabled, and each failure (`failure`, the failures since the
 * address was followed) draws its alert afresh, so a screen reader says a second failure again.
 */
function ItemReadFailed({
  failure,
  noun,
  onRetry,
  retrying,
  takesFocus,
}: {
  failure: number;
  noun: string;
  onRetry: () => void;
  retrying: boolean;
  takesFocus: TakesFocus;
}): ReactNode {
  const heading = useFocusOnDraw<HTMLHeadingElement>(takesFocus);
  return (
    <section>
      <h2 className={`text-xl font-semibold ${textPrimaryOnCanvas}`} ref={heading} tabIndex={-1}>
        Couldn&apos;t load this {noun.toLowerCase()}
      </h2>
      <div className="mt-2">
        <FailedReadLine
          failure={failure}
          message={`Couldn't load this ${noun.toLowerCase()}.`}
          onRetry={onRetry}
          retrying={retrying}
        />
      </div>
    </section>
  );
}
