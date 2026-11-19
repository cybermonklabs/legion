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
import { issueAddress } from "../issue-address";
import { artifactsBySlug } from "../issue-artifacts";
import { NoTooltips } from "../NoTooltips";
import { useOutlineReads } from "../outline-reads";
import { useOutlineRequest } from "../outline-request";
import { KEY_REPORTS } from "../page-structure";
import { EMPTY_STATE_LINES } from "../page-words";
import { presetView } from "../presets";
import type { SlotProps } from "../slot-props";
import { specSummary } from "../spec-summary";
import { focusIsLoose } from "../tree-focus";
import { TreeReadFailed } from "../tree-read/view-read";
import { abortOnReaderAct } from "../useNodeEditing";
import { FailedReadLine } from "./failed-read-line";

/** Filled by INFRA-1196 part G, "A node's row", then part I, "The outline". */
export function BodySlot({ issue, route }: SlotProps): ReactNode {
  // The outline: the issue's one tree as the Conversation view leaves it, folded to the
  // frontier, in place of the rows drawn with every branch open before it. The spec's summary
  // callout is no row of it: the Spec view draws it as the issue's three sentences.
  const { hash, key: locationKey, search } = useLocation();
  const specSlug = primarySpec(issue)?.slug;
  const address = issueAddress(route, search, hash, specSlug);
  // The tree, and whether every read its nodes come from holds its answer (`useOutlineReads`).
  // Until then a tree with no node is one whose nodes have not arrived, and the outline's line
  // that the issue holds nothing would be false on every cold load, so the outline draws nothing
  // in its place; so would `q` and `w` saying there is no question or decision to go to, so they
  // say nothing.
  const { reads, readsHeld, root } = useOutlineReads(issue);
  const key = issue.key;
  // The address's own node is resolved against the whole tree and then kept by the view, so an
  // address that names a node the words filter would not have matched still draws it. An item
  // the tree holds no node of falls back to the item's own page once the reads are held, unless
  // the tree has held it since the address was followed (`useAddressFallback`); a turn the
  // outline leaves out lands on its nearest drawn ancestor, which the view keeps as it keeps the
  // address's own node; and a block or the children the tree holds no node of are said to be
  // so, in the outline's place.
  const addressed = useAddressTarget(root, address);
  const fallback = useAddressFallback({
    address,
    issue,
    reads,
    readsHeld,
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
  const view = useMemo(
    () => presetView(root, "conversation", address.filter, named, memo),
    [address.filter, memo, named, root]
  );
  const artifacts = useMemo(() => artifactsBySlug(issue), [issue]);
  const request = useOutlineRequest();
  // Whether the reader has acted since they followed the address, after which the address's row
  // drawn after the act does nothing to the page (`IssueTree`'s `addressActs`).
  const acts = useReaderActs(locationKey);
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
        {fallback?.kind === "landing" ? (
          // Keyed on the following, so a link to the same item followed again is drawn anew and
          // takes the focus again.
          <ItemFallback fallback={fallback} key={locationKey} takesFocus={takesFocus} />
        ) : fallback?.kind === "absent" ? (
          // Keyed on the following, as the item's own page is.
          <NotFoundLine
            heading={ABSENT_LINES[fallback.target.kind].heading}
            issueKey={key}
            key={locationKey}
            line={ABSENT_LINES[fallback.target.kind].line(key)}
            takesFocus={takesFocus}
          />
        ) : (
          <IssueTree
            addressActs={acts}
            addressKey={locationKey}
            artifacts={artifacts}
            context={view.context}
            emptyLine={
              address.filter === "" ? EMPTY_STATE_LINES.conversation : EMPTY_STATE_LINES.no_match
            }
            filtering={view.filtering}
            focusReveal
            // The words filter's Escape comes before the outline is left.
            holdsEscape={address.filter !== ""}
            isClosed={isClosed}
            {...(ancestor === undefined ? {} : { landingReport: KEY_REPORTS.agents_thread })}
            onEditing={onEditing}
            readsHeld={readsHeld}
            {...(request === undefined ? {} : { request })}
            reveal={ancestor ?? addressed.reveal}
            root={view.root}
            rootRow={view.rootRow}
          />
        )}
      </div>
      {panels}
    </NoTooltips>
  );
}

/**
 * The controller the reader's first act since the address was followed aborts (`READER_ACTS`):
 * one for each following (`following`, the location's own key), started as the following is
 * drawn. It is started in a layout effect, which runs before every passive effect of that commit,
 * so a line drawn in the commit of the following itself reads the following's own controller.
 */
function useReaderActs(following: string): ReaderActs {
  const acts = useRef<AbortController | undefined>(undefined);
  // biome-ignore lint/correctness/useExhaustiveDependencies: each following is watched anew, though no value of it is read.
  useLayoutEffect(() => {
    const watched = abortOnReaderAct();
    acts.current = watched;
    return () => watched.abort();
  }, [following]);
  return acts;
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

/** What the reader's first act since the address was followed aborts (`useReaderActs`). */
type ReaderActs = RefObject<AbortController | undefined>;

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
 *  the tree holds no node of (`absent`), for the issue `key`. */
const ABSENT_LINES: Readonly<
  Record<
    Extract<AddressFallback, { kind: "absent" }>["target"]["kind"],
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
