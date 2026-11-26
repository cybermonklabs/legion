import { type ReactNode, type RefObject, useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { focusOnDocument } from "../../shell/roving";
import { ChildrenSection } from "../children-section";
import { ThreeThingsAbove } from "../contents-frame";
import { DocumentStages } from "../document-stage";
import { bringOnScreen } from "../identity-block";
import { issueAddress } from "../issue-address";
import type { Preset } from "../page-words";
import type { SlotProps } from "../slot-props";
import { focusIsLoose, TAB_STOP_ROW } from "../tree-focus";
import { ViewReadProvider } from "../tree-read/view-read";
import { BodySlot, ReaderActsWatch } from "./body";
import { FAILED_READ_RETRY, FailedLineLeaves } from "./failed-read-line";
import { useFollowing } from "./following";
import { ViewDecisionsSlot } from "./view-decisions";
import { LandsItself, useLandsItself, ViewGraphSlot } from "./view-graph";
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
 * a turn of a thread only agents wrote in, a block no node carries - the stage draws the body in
 * the view's place for the rest of that following, which lands as the outline does; a new
 * following asks the view again.
 *
 * `DocumentStages` stands around the chosen slot: a document has one editor on the page, which
 * the row being edited in takes onto its own stage (`document-stage.tsx`). The outline is drawn
 * in a stage's box as a view is (`StageBox`), so a failed read's line over its tree gives the
 * focus back as it leaves.
 *
 * Below 1024 px, where no Contents pane holds them, the issue's three sentences stand above the
 * slot (`contents-frame.tsx`'s `ThreeThingsAbove`), but under the Spec view, whose own first
 * group draws them (R13).
 *
 * Above the slot, after those sentences, stand the issue's children, a section of their own that
 * opens and closes (`children-section.tsx`), in every view that draws an outline read (R14).
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
  const told = useRef<string | undefined>(undefined);
  const [undrawn, setUndrawn] = useState<{ following: string; viewsFocus: boolean }>();
  const viewBox = useRef<HTMLDivElement>(null);
  const onAddressDrawn = useCallback(
    (drawn: boolean) => {
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
  const swapped = undrawn?.following === following;
  const outline = beside && (!filled[preset] || swapped);
  const View = outline ? undefined : views[preset];
  // Graph's slot draws the outline (`view-graph.tsx`), whose own landing puts the focus on the
  // node an address beside it names: the view stage is told so (`LandsItself`), and waits on that
  // landing for the first focus, which it takes itself once the address has lost the node
  // (`ViewStage`).
  const landsItself = beside && preset === "graph";
  return (
    <ViewReadProvider issue={props.issue} preset={preset}>
      <DocumentStages>
        <ReaderActsWatch into={acts} key={following} />
        {/* Below 1024 px, the issue's three sentences above the slot's first row (R13). */}
        <ThreeThingsAbove {...props} view={View !== undefined} />
        {/* The issue's children, above the slot's first row (R14). */}
        <ChildrenSection {...props} />
        {View === undefined ? (
          <StageBox>
            <BodySlot {...props} acts={swapped && undrawn.viewsFocus ? undefined : acts} />
          </StageBox>
        ) : (
          <LandsItself.Provider value={landsItself}>
            <ViewStage box={viewBox} key={preset}>
              <View {...props} onAddressDrawn={onAddressDrawn} />
            </ViewStage>
          </LandsItself.Provider>
        )}
      </DocumentStages>
    </ViewReadProvider>
  );
}

/** What the stage hands the view it draws in place of the outline: the slot's own props, and
 *  how the view tells the stage whether its tree draws the node the address names. */
export interface ViewSlotProps extends SlotProps {
  /**
   * Tells the stage whether the view's tree draws the node the address names (`drawn`). The
   * stage takes the first call of each following of the address (`useFollowing`) that names a
   * node beside the view's own address and opens no document, and no other call, so a view may
   * call it on every render once its reads have settled, by the rule `StageSlot` gives. Given
   * true first, the stage keeps the view, with the focus on the view's tab stop or the row it
   * lands on; given false first, it draws the body in the view's place for the rest of that
   * following, as under a view whose part has not filled its slot, and the body lands on the node
   * or says in the outline's place that the issue holds none of it, with the focus.
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
  // Graph draws Graph's read whole as one outline, through the outline's own rows (R10).
  graph: ViewGraphSlot,
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
 *  (`ViewSlotProps.onAddressDrawn`): the stage then draws the outline in the view's place. Each
 *  of the four views draws its own tree here (`view-spec.tsx`, `view-plan.tsx`,
 *  `view-verification.tsx`, `view-decisions.tsx`), so each is filled. */
export const SLOT_FILLED: SlotsFilled = {
  conversation: true,
  decisions: true,
  // Graph draws its tree here too (`view-graph.tsx`), so it is filled.
  graph: true,
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
 * the focus, but for one thing: a view's reads are refused in any order, and a view draws its
 * lines in an order of its own, so a line it draws above the Retry the focus was given to, while
 * the focus still rests on that Retry, takes the focus to its own Retry. The focus then rests on
 * the first line the view draws, as where that line had been drawn first. A focus that moves
 * anywhere else, or a tab-stop row drawn, ends that.
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
 * A view whose tree lands on the node the address names itself - Graph's, which draws the
 * outline (`view-graph.tsx`), on an address naming a node - is drawn under `LandsItself` set so
 * by `StageSlot`, and the stage waits on that landing rather than take the focus: the landing
 * puts the focus on the node's row in the frame after it draws the row, and reads a focus the
 * stage gave the tab stop before then as somebody else's, which it leaves there, so the address
 * would land on the tree's first row. The take is done once something holds the focus - the
 * node's row, or a control the reader pressed. Where the address loses its node first - the body
 * replaces an address whose node no read holds with the address without it (`addressView`) - the
 * stage's next look, at the tree the body draws in the replaced address's place, takes the focus
 * as for any address naming no node, so the address opens with the focus where that address
 * puts it.
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
  // Whether the view lands on the address's node itself (`LandsItself`), as the stage last drew
  // it: the take runs from an effect of the stage's first draw, and reads it whenever it runs.
  const landsItself = useRef(false);
  landsItself.current = useLandsItself();
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
    // Puts the focus on `to`, and brings it on screen too once `bring`.
    const place = (to: HTMLElement): void => {
      if (bring) focusOn(to);
      else to.focus({ preventScroll: true });
    };
    // The Retry the take gave the focus to, while the focus rests there or nothing holds it: a
    // line the view draws above it later takes the focus to its own Retry (`firstLine`), so the
    // focus rests on the first line the view draws. Undefined once the focus moved anywhere else,
    // or the view drew a tab-stop row, and while the take gave the focus to no Retry.
    let held: HTMLElement | undefined;
    const release = (): void => {
      held = undefined;
      firstLine.disconnect();
    };
    const firstLine = new MutationObserver(() => {
      if (held === undefined) return;
      if (
        stage.querySelector(TAB_STOP_ROW) !== null ||
        (document.activeElement !== held && !focusIsLoose(stage))
      ) {
        release();
        return;
      }
      const first = stage.querySelector<HTMLElement>(FAILED_READ_RETRY);
      if (first === null || first === held) return;
      held = first;
      place(first);
    });
    // Any focus the take did not place ends `held`, but for one nothing holds: the app's on its
    // main element, which the frame after the take gives back.
    const moved = (event: FocusEvent): void => {
      if (held !== undefined && event.target !== held && !focusIsLoose(stage)) release();
    };
    document.addEventListener("focusin", moved);
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
      // A view whose tree lands on the address's node itself (`LandsItself`) is waited on: the
      // take is done once something holds the focus (above), and goes on below to take it once
      // the address has lost its node.
      if (landsItself.current) return false;
      const to =
        stage.querySelector<HTMLElement>(TAB_STOP_ROW) ??
        stage.querySelector<HTMLElement>(FAILED_READ_RETRY);
      if (to === null) return false;
      held = to.matches(FAILED_READ_RETRY) ? to : undefined;
      place(to);
      placed = to;
      if (held === undefined) firstLine.disconnect();
      else firstLine.observe(stage, { childList: true, subtree: true });
      const frame = requestAnimationFrame(() => {
        ending.delete(frame);
        placed = null;
        if (focusIsLoose(stage)) place(held ?? to);
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
    // While the view lands on the address's node itself, a focus anything takes - the landing's on
    // the node's row, or a control the reader pressed - ends the take as a look would, so a focus
    // dropped after that is not taken for the view once the address has lost its node.
    const focusTaken = () => {
      if (landsItself.current && take()) drawn.disconnect();
    };
    document.addEventListener("focusin", focusTaken);
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
      firstLine.disconnect();
      document.removeEventListener("focusin", moved);
      stage.removeEventListener("focusin", treeFocused);
      document.removeEventListener("focusin", focusTaken);
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
