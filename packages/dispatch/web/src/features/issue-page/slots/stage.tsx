import { type ReactNode, type RefObject, useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { focusOnDocument } from "../../shell/roving";
import { DocumentStages } from "../document-stage";
import { bringOnScreen } from "../identity-block";
import { issueAddress } from "../issue-address";
import type { Preset } from "../page-words";
import type { SlotProps } from "../slot-props";
import {
  ADDRESS_LANDING,
  isAddressedRow,
  LANDING_MARKS,
  useAddressLanding,
  useStageLanding,
  ViewNodeKeeper,
  type ViewTold,
} from "../stage-landing";
import { focusIsLoose, TAB_STOP_ROW } from "../tree-focus";
import { BodySlot, ReaderActsWatch } from "./body";
import { FAILED_READ_RETRY, FailedLineLeaves } from "./failed-read-line";
import { useFollowing } from "./following";
import { ViewDecisionsSlot } from "./view-decisions";
import { ViewPlanSlot } from "./view-plan";
import { ViewSpecSlot } from "./view-spec";
import { ViewVerificationSlot } from "./view-verification";

/**
 * The stage: the outline, or one view in its place, never both. The `preset` the address opens
 * chooses one slot: the body, which draws the outline, with no view on, and that view's own
 * slot under Spec, Verification, Plan or Decisions. Each slot draws its own measure, so the
 * stage adds no box and no width. A view whose slot is not filled yet draws nothing.
 *
 * An address that names a node beside a view's own address and opens no document - an item
 * beside it (`?ask=`, `?comment=`), as `documentItemPath` writes the address of an item of the
 * spec, or a `#b-` fragment on it - is the outline's on that node (`issue-address.ts`). A view
 * whose part has not filled its slot draws nothing to land on, which left the reader on the
 * page's body: under such a view the stage draws the body, which lands on the node, or says in
 * the outline's place that the tree holds none. A view whose part has filled its slot draws the
 * node in its own tree, with the focus on the row it lands on or on its tree's tab stop, and says
 * whether its tree draws the node (`ViewSlotProps.onAddressDrawn`) once every read it needs has
 * settled, held or refused (`useOutlineReads`'s `readsSettled`): before that the node may not
 * have arrived, and the outline's fallback waits for those reads too (`useAddressFallback`). A
 * refused read that rows come from (`FailedRead.placesRows`) and that the view draws no failed
 * read's line of its own for is no row of the view: where one stands refused, the view says its
 * tree draws no node, and the outline is drawn in its place with that read's line and Retry, as
 * under a view whose part has not filled its slot. Where only reads the view draws its own line
 * for stand refused, the view says nothing and keeps its line, whose Retry takes the focus
 * (`ViewStage`). The stage takes the first thing the view says for each following of the address
 * (`useFollowing`: the location's key with the address) and none after it, so a view may say it
 * again on any render once its reads have settled. Where its tree draws no node - an item the
 * issue holds none of, an item of another issue, an id written short, an item whose read failed,
 * a turn of a thread the history holds (`agents-only.ts`), a block no node carries - the stage
 * draws the body in the view's place for the rest of that following, which lands as the outline
 * does; a new following asks the view again.
 *
 * `DocumentStages` stands around the chosen slot: a document has one editor on the page, which
 * the row being edited in takes onto its own stage (`document-stage.tsx`). The outline is drawn
 * in a stage's box as a view is (`StageBox`), so a failed read's line over its tree gives the
 * focus back as it leaves.
 *
 * Part N's first branch stands before them (its R1, R6): what an item's address lands on in
 * place of the stage, and the document or file an address opens, drawn in place of the outline
 * and of every view, inside the same `DocumentStages` (`stage-landing.tsx`).
 * An approval request's own address is the header's: the outline is drawn at the issue's own
 * address under it (`useStageLanding`'s `outline`), as with no view on, and lands on no item.
 * Until the view has told the stage whether its tree draws the node an address names - beside the
 * view's own address, or a block of the spec on the Spec view's address - the address's landing
 * is undecided (dispatch://INFRA-1196 decision 413): the stage marks the box it draws the view in
 * so (`useAddressLanding`), and the view's tab stop takes no focus meanwhile but where it is the
 * row of that node, whose focus is the landing (`ViewStage`), so the focus moves once, onto where
 * the address lands.
 */
export function StageSlot({
  filled = SLOT_FILLED,
  views = VIEW_SLOTS,
  ...props
}: SlotProps & {
  /** Whether each slot's own part has filled it: `SLOT_FILLED` where none is given, and a
   *  view's slot set filled in a test, as that view's part will set it. */
  readonly filled?: SlotsFilled;
  /** The slot each view draws in place of the outline: `VIEW_SLOTS` where none is given, and a
   *  stand-in view in a test, which reads the stage's rule apart from what each view draws. */
  readonly views?: ViewSlots;
}): ReactNode {
  const { hash, search } = useLocation();
  const landed = useStageLanding(props);
  const following = useFollowing();
  const address = issueAddress(props.route, search, hash, primarySpec(props.issue)?.slug);
  const { preset } = address;
  // Whether the address names a node beside the view's own address and opens no document, which
  // is the outline's to land on wherever the view draws none of it.
  const beside = address.target !== undefined && address.document === undefined;
  // The following of the address whose first call from the view the stage took, which takes no
  // other call in it, and the following for which the view said its tree draws no node the
  // address names (`ViewSlotProps.onAddressDrawn`): the body is drawn in the view's place for the
  // rest of that following. With it, whether the focus was the view's as the view said so: in the
  // box the view is drawn in (`viewBox`), or held by nothing, as where the view's Retry left the
  // page with its line in the render in which the read it asked again answered.
  // A view that says so in the commit that first draws it - its reads held as it is drawn, as on
  // Back to its address - says it before the box is set, since React sets an element's ref after
  // its children's layout effects: only a focus held by nothing is the view's then, not one on the
  // app's main element. That is before any act of the following, so the body lands either way:
  // handed the stage's watch, which no act has ended, or watching its own from its first draw.
  const told = useRef<string | undefined>(undefined);
  const [undrawn, setUndrawn] = useState<{ following: string; viewsFocus: boolean }>();
  const viewBox = useRef<HTMLDivElement>(null);
  // What the view told the stage first for this following, and whether the reader had acted by
  // then: until it has, the landing of an address that names a node is undecided, and the stage
  // marks the view's box so, with the node where the address names it whole, which the view's
  // tab stop reads (`useAddressLanding`, below, once the reader's acts are watched). A view tells
  // it whether or not the node is beside its own address, and each call but the first of a
  // following leaves it as it is.
  const [viewTold, setViewTold] = useState<ViewTold>();
  const onAddressDrawn = useCallback(
    (drawn: boolean) => {
      setViewTold((was) =>
        was?.following === following
          ? was
          : { following, reader: acts.current?.signal.aborted === true }
      );
      if (!beside || told.current === following) return;
      told.current = following;
      if (drawn) return;
      const box = viewBox.current;
      const viewsFocus = focusIsLoose(box) || box?.contains(document.activeElement) === true;
      setUndrawn({ following, viewsFocus });
    },
    [beside, following]
  );
  // The reader's acts since the address was followed (`ReaderActsWatch`), watched from the
  // following's drawing whichever slot is drawn and handed to the body, so a body drawn in the
  // view's place reads an act made while the view was drawn, as one made before its row drew
  // (`IssueTree`'s `addressActs`, `BodySlot`'s `takesFocus`): a control pressed outside the stage
  // keeps the focus. Where the focus was the view's, nothing outside the view holds it once the
  // body is drawn: the stage hands that body none, and it watches from its own first draw, so it
  // lands as it does where the reader made no act, a press on blank space included.
  const acts = useRef<AbortController | undefined>(undefined);
  useAddressLanding(viewBox, address.target, following, viewTold, acts);
  const swapped = undrawn?.following === following;
  const outline = beside && (!filled[preset] || swapped);
  const View = outline ? undefined : views[preset];
  if (landed.instead !== undefined) return landed.instead;
  if (landed.document !== undefined) return <DocumentStages>{landed.document}</DocumentStages>;
  if (landed.outline !== undefined) {
    return (
      <DocumentStages>
        <ReaderActsWatch into={acts} key={following} />
        <StageBox>
          <BodySlot {...props} acts={acts} route={landed.outline} />
        </StageBox>
      </DocumentStages>
    );
  }
  return (
    <DocumentStages>
      <ReaderActsWatch into={acts} key={following} />
      <ViewNodeKeeper lands={beside} viewOn={View !== undefined} />
      {View === undefined ? (
        <StageBox>
          <BodySlot {...props} acts={swapped && undrawn.viewsFocus ? undefined : acts} />
        </StageBox>
      ) : (
        <ViewStage box={viewBox} key={preset}>
          <View {...props} acts={acts} onAddressDrawn={onAddressDrawn} />
        </ViewStage>
      )}
    </DocumentStages>
  );
}

/** What the stage hands the view it draws in place of the outline: the slot's own props, and
 *  how the view tells the stage whether its tree draws the node the address names. */
export interface ViewSlotProps extends SlotProps {
  /** The reader's acts since the address was followed (`ReaderActsWatch`, keyed on the
   *  following): a controller their first act aborts, which a view's landing reads. */
  readonly acts?: RefObject<AbortController | undefined>;
  /**
   * Tells the stage whether the view's tree draws the node the address names (`drawn`). The
   * stage takes the first call of each following of the address (`useFollowing`) that names a
   * node beside the view's own address and opens no document, and no other call, so a view may
   * call it on every render once its reads have settled, by the rule `StageSlot` gives. Given
   * true first, the stage keeps the view, with the focus on the view's tab stop or the row it
   * lands on, which it takes once it is told - at once where its tab stop is the row of the node
   * the address names - and not where the reader acted first (decision 413); given false first,
   * it draws the body in the view's place for the rest of that
   * following, as under a view whose part has not filled its slot, and the body lands on the node
   * or says in the outline's place that the issue holds none of it, and takes the focus unless
   * the reader moved it outside the view: a control outside the view they pressed keeps it.
   */
  readonly onAddressDrawn: (drawn: boolean) => void;
}

/** The slot each view draws in place of the outline, by preset. */
export type ViewSlots = Readonly<Record<Preset, ((props: ViewSlotProps) => ReactNode) | undefined>>;

/** The slot each view draws in place of the outline; none for Conversation, which is the
 *  outline itself. */
export const VIEW_SLOTS: ViewSlots = {
  conversation: undefined,
  decisions: ViewDecisionsSlot,
  plan: ViewPlanSlot,
  spec: ViewSpecSlot,
  verification: ViewVerificationSlot,
};

/** Whether each slot's own part has filled it, by preset. */
export type SlotsFilled = Readonly<Record<Preset, boolean>>;

/** Whether each slot's own part has filled it: a view whose part has not draws nothing, and an
 *  address naming a node beside it draws the outline (`StageSlot`). Each view's part sets its
 *  entry here when it fills its slot; from then the address is that view's, which is to land on
 *  its tab stop or the row its tree draws for the node, and to tell the stage where it draws none
 *  (`ViewSlotProps.onAddressDrawn`): the stage then draws the outline in the view's place. */
export const SLOT_FILLED: SlotsFilled = {
  conversation: true,
  decisions: true,
  plan: true,
  spec: true,
  verification: true,
};

/** A slot's tree itself, which holds the focus only where it has no row to give it to. */
const SLOT_TREE = '[role="tree"]';

/**
 * One view, drawn where the outline is, which takes the keyboard focus on its tree's tab-stop
 * row once that row is drawn, where the tester found the focus left on the page's body: a page
 * opened at a view's address, or a view turned on while the focus was on the outline it
 * replaced, leaves the focus on the view and never on the page's body. A view whose read failed
 * draws that read's Retry (`FailedReadLine`) and no row, which left the focus on the body all the
 * same, so there the Retry takes it. The focus is taken only while nothing holds it - on the
 * page itself, or on an element that holds the stage, as the app's main element does when a
 * view's address is opened from another page - so a reader who pressed a control keeps it there,
 * and once it is taken this does nothing more for that view until the view's tree itself takes
 * the focus.
 *
 * The chord that opens a view (`t` then the view's key) reaches the stage through the view's
 * tree: where the tree draws no tab-stop row the chord focuses the tree itself (`reachPart`,
 * `useIssueKeymap.ts`), from wherever the focus was. A tree with no row holds nothing to act on,
 * and Tab from it never reaches the Retry over it, so a focus on the tree itself from outside the
 * view is taken as held by nothing: the stage gives it to a failed read's Retry, or to the
 * tab-stop row, now or once the view draws one. A focus the reader moves onto the tree from
 * inside the view - from its Retry, or a screen reader's cursor - rests there, except in the
 * frame the stage placed the focus itself, which is the chord's own focus of the tree. Where rows
 * are drawn a focus on the tree stays there, since a press on what the tree draws outside its
 * rows focuses the tree and moves no row's focus.
 *
 * A failed read's line that leaves the page with the focus in it gives the focus back as every
 * stage's box does (`StageBox`), and the view draws its own measure. The stage draws one for each
 * view, keyed on it, so a view turned on is a new one, which takes the focus once.
 *
 * An address that names a node lands once (decision 413): while the stage marks the box
 * undecided (`ADDRESS_LANDING`, `useAddressLanding`), the take gives no row the focus but the
 * row of the node the address names (`isAddressedRow`), whose focus is the landing itself: a
 * view that draws that row as its tab stop while its other reads are out, as the Spec view draws
 * a comment the address names, has the focus on it at once. It gives the focus to a failed
 * read's Retry where the view draws no row, since a view whose own read stands refused tells
 * the stage nothing. Once the mark is gone the take waits one frame more, the frame in which a
 * view's own landing on the row the address names takes the focus (`landOnRow`), and then takes
 * it where that left it held by nothing: on the view's tab stop, which is the named row where
 * the view starts its tab stop there. Marked `acted`, the reader acted first, and the take
 * places nothing: the landing moves no focus.
 */
export function ViewStage({
  box: given,
  children,
}: {
  /** The box's element, for a caller that reads it too (`StageSlot`); else the stage's own. */
  box?: RefObject<HTMLDivElement | null>;
  children: ReactNode;
}): ReactNode {
  const own = useRef<HTMLDivElement>(null);
  const box = given ?? own;
  useEffect(() => {
    const stage = box.current;
    if (stage === null) return;
    // Whether the focus placed is brought on screen too: once a focus on the tree itself started
    // the take again, as the chord's does, which scrolls the tree to the screen's top.
    let bring = false;
    // What the take gave the focus to, until the next frame. The chord that opens a view
    // focuses the tree a frame after it opens it (`reachPart`), so where the view draws its
    // Retry at once the stage has already given the Retry the focus, and the chord's focus on the
    // tree comes from that Retry: in that frame it is the chord's, and the take runs again. No
    // reader moves that fast, so after that frame a focus moved off the Retry onto the tree is
    // the reader's.
    let placed: Element | null = null;
    // The frames that end `placed`, which go with the stage: one left to run after the view is
    // gone would focus what the take placed the focus on, off the page.
    const ending = new Set<number>();
    // Whether the take waits for the address's landing, as the stage marks the box: from the
    // mark the box holds as the view is drawn, until the mark changes (`decided`, below).
    let waits = stage.getAttribute(ADDRESS_LANDING) === LANDING_MARKS.undecided;
    // The frame the take waits for once the landing is decided.
    let landing = 0;
    // Puts the focus on `to`, and brings it on screen too once `bring`.
    const place = (to: HTMLElement): void => {
      if (bring) focusOn(to);
      else to.focus({ preventScroll: true });
    };
    // True once the focus is placed, or once something other than the view's tree holds it.
    // Nothing holds it on the page itself, or on an element that holds the stage
    // (`focusIsLoose`): the app focuses its main element at each page change but the first
    // (`app.tsx`). A view that draws its row or Retry in a later commit than the page change
    // finds the focus there. One that draws them in the commit the page mounts in - its reads
    // cached, as on Back to its address - places the focus first, and the app then moves it to
    // its main element, since React runs a child's effects before its parent's. So the frame
    // that ends `placed` takes the focus again where nothing holds it then (which the element
    // placed no longer does), as the outline's opening waits a frame for its first row; a focus
    // the reader or a script put elsewhere in that frame stays.
    const take = (): boolean => {
      const active = document.activeElement;
      const onTree = active?.matches(SLOT_TREE) === true && stage.contains(active);
      if (!focusIsLoose(stage) && !onTree) return true;
      const to =
        stage.querySelector<HTMLElement>(TAB_STOP_ROW) ??
        stage.querySelector<HTMLElement>(FAILED_READ_RETRY);
      // While the landing is undecided no row takes the focus but the row of the node the address
      // names, whose focus is the landing; a Retry drawn with no row does.
      if (waits && to?.matches(TAB_STOP_ROW) === true && !isAddressedRow(stage, to)) return false;
      if (to === null) return false;
      place(to);
      placed = to;
      const frame = requestAnimationFrame(() => {
        ending.delete(frame);
        placed = null;
        if (focusIsLoose(stage)) place(to);
      });
      ending.add(frame);
      return true;
    };
    // A view draws its tree once its reads have answered, a frame or several after it mounts.
    const drawn = new MutationObserver(() => {
      if (take()) drawn.disconnect();
    });
    const watch = () => {
      if (!take()) drawn.observe(stage, { childList: true, subtree: true });
    };
    watch();
    // The mark's changes: undecided again, for a new following of the address, which the take
    // waits on; decided, after which the take runs in the next frame, unless the reader acted
    // first: it then places nothing, and runs again only for a focus on the tree itself (below).
    const decided = new MutationObserver(() => {
      const mark = stage.getAttribute(ADDRESS_LANDING);
      if (mark === LANDING_MARKS.undecided) {
        waits = true;
        return;
      }
      if (!waits) return;
      waits = false;
      drawn.disconnect();
      if (mark === null) landing = requestAnimationFrame(watch);
    });
    decided.observe(stage, { attributeFilter: [ADDRESS_LANDING] });
    // A focus on the tree itself, where it draws no tab-stop row, starts the take again,
    // whatever held the focus before outside the view: the chord's, or the stage's own once a
    // line left. A focus that moves onto the tree from inside the view - from its Retry, or a
    // screen reader's cursor - is the reader's, and rests there, but for one from what the take
    // focused in the frame it did so (`placed`).
    const treeFocused = (event: FocusEvent) => {
      const target = event.target;
      if (!(target instanceof Element) || !target.matches(SLOT_TREE)) return;
      const from = event.relatedTarget;
      if (from instanceof Node && from !== placed && stage.contains(from)) return;
      if (stage.querySelector(TAB_STOP_ROW) !== null) return;
      bring = true;
      watch();
    };
    stage.addEventListener("focusin", treeFocused);
    return () => {
      drawn.disconnect();
      decided.disconnect();
      cancelAnimationFrame(landing);
      stage.removeEventListener("focusin", treeFocused);
      for (const frame of ending) cancelAnimationFrame(frame);
    };
  }, [box]);
  return <StageBox box={box}>{children}</StageBox>;
}

/**
 * What the stage draws a slot in, the outline or a view: a box that adds none
 * (`display: contents`), a place to look for the slot's tree, which draws its own measure. A
 * failed read's line drawn in it that leaves the page with the focus in it says so
 * (`FailedLineLeaves`), and the focus goes to the next failed read's Retry, to the tree's
 * tab-stop row, or to the tree itself, in that order, unless something else took it in the
 * render the line left in: once the line is off the page the browser has moved the focus to the
 * page's body, where nothing says where the reader was.
 */
function StageBox({
  box: given,
  children,
}: {
  /** The box's element, for a caller that reads it too (`ViewStage`): the box keeps a ref of its
   *  own where none is given. */
  box?: RefObject<HTMLDivElement | null>;
  children: ReactNode;
}): ReactNode {
  const own = useRef<HTMLDivElement>(null);
  const box = given ?? own;
  // The line calls this while React takes it off the page, before what replaces it is drawn: the
  // focus is placed once that render is on the page.
  const lineLeaves = useCallback(() => {
    queueMicrotask(() => {
      const stage = box.current;
      if (stage === null || !focusOnDocument()) return;
      const to =
        stage.querySelector<HTMLElement>(FAILED_READ_RETRY) ??
        stage.querySelector<HTMLElement>(TAB_STOP_ROW) ??
        stage.querySelector<HTMLElement>(SLOT_TREE);
      if (to !== null) focusOn(to);
    });
  }, [box]);
  return (
    <FailedLineLeaves.Provider value={lineLeaves}>
      <div className="contents" ref={box}>
        {children}
      </div>
    </FailedLineLeaves.Provider>
  );
}

/** Puts the focus on `to` without scrolling to it, then brings it on screen on the next frame
 *  where it still holds the focus (`bringOnScreen`). The frame is for the chord that opens a view:
 *  `reachPart` scrolls the tree to the screen's top after it focuses it, which would leave a Retry
 *  that stands over the tree above the screen. */
function focusOn(to: HTMLElement): void {
  to.focus({ preventScroll: true });
  requestAnimationFrame(() => {
    if (document.activeElement === to) bringOnScreen(to);
  });
}
