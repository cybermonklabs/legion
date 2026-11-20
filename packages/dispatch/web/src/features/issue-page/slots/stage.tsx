import { type ReactNode, type RefObject, useCallback, useEffect, useRef } from "react";
import { useLocation } from "react-router-dom";

import { primarySpec } from "../../../api/issue-cache";
import { focusOnDocument } from "../../shell/roving";
import { DocumentStages } from "../document-stage";
import { bringOnScreen } from "../identity-block";
import { issueAddress } from "../issue-address";
import type { Preset } from "../page-words";
import type { SlotProps } from "../slot-props";
import { focusIsLoose, TAB_STOP_ROW } from "../tree-focus";
import { BodySlot } from "./body";
import { FAILED_READ_RETRY, FailedLineLeaves } from "./failed-read-line";
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
 * the outline's place that the tree holds none.
 *
 * `DocumentStages` stands around the chosen slot: a document has one editor on the page, which
 * the row being edited in takes onto its own stage (`document-stage.tsx`). The outline is drawn
 * in a stage's box as a view is (`StageBox`), so a failed read's line over its tree gives the
 * focus back as it leaves.
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
  const address = issueAddress(props.route, search, hash, primarySpec(props.issue)?.slug);
  const { preset } = address;
  const outline = !filled[preset] && address.target !== undefined && address.document === undefined;
  const View = outline ? undefined : views[preset];
  return (
    <DocumentStages>
      {View === undefined ? (
        <StageBox>
          <BodySlot {...props} />
        </StageBox>
      ) : (
        <ViewStage view={preset}>
          <View {...props} />
        </ViewStage>
      )}
    </DocumentStages>
  );
}

/** The slot each view draws in place of the outline, by preset. */
export type ViewSlots = Readonly<Record<Preset, ((props: SlotProps) => ReactNode) | undefined>>;

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
 *  the node in its own tree. */
export const SLOT_FILLED: SlotsFilled = {
  conversation: true,
  decisions: false,
  plan: true,
  spec: false,
  verification: false,
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
 * stage's box does (`StageBox`), and the view draws its own measure.
 */
export function ViewStage({ children, view }: { children: ReactNode; view: Preset }): ReactNode {
  const box = useRef<HTMLDivElement>(null);
  // biome-ignore lint/correctness/useExhaustiveDependencies: each view takes the focus once, so a new view is what starts this again.
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
      stage.removeEventListener("focusin", treeFocused);
      for (const frame of ending) cancelAnimationFrame(frame);
    };
  }, [view]);
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
