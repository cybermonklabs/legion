import { Component, type ReactNode, type RefObject } from "react";

import { CONTROL, controlIn } from "../../components/control-names";
import { openerOf, pressArmed, pressMadeIn, scrollToNearest } from "../../components/focus-place";
import { refPreviewCardContains, refPreviewOpenedIn } from "../refs/ref-preview-card";
import { focusOnDocument, hearPressCancellation } from "./roving";

export interface AnchorRow {
  id: string;
  /** How the row was chosen: keyboard focus within it, the pointer resting on it, or, failing
   *  both, the row nearest the list's visible centre. */
  via: "center" | "focus" | "pointer";
}

/** A row above the current one that a press of the reader's is taking away (`leaving`), as the
 *  commit's snapshot reads it, immediately before the row goes. */
interface Leaving {
  /** How far the current row moves up once this row has gone from above it: the whole space the
   *  row takes in the list (`spaceTaken`), the part above the view's top included. */
  by: number;
  id: string;
  /** The element right before the row where that element is no row (a group's heading), and how
   *  far the current row moves up once that element has gone too, read as `by` is. Null where a
   *  row, or nothing, stands right before the row. A group's heading goes with the last row of
   *  its group. */
  lead: { by: number; element: Element } | null;
}

/** A row whose place a commit keeps, as it stands immediately before the commit. */
interface Place {
  /** The row's `group` attribute value immediately before the commit; null without one. */
  group: string | null;
  id: string;
  /** The rows above this one that a press of the reader's is taking away; empty where the host
   *  names none. */
  leaving: readonly Leaving[];
  /** The row's top, in viewport pixels, immediately before the commit. */
  top: number;
}

interface Snapshot extends AnchorRow, Place {
  /** Whether a press of the reader's is taking the current row itself away, as a thread's own
   *  Dismiss does: the row `leaving` names is the row the hand is on, not one above it. */
  selfLeaving: boolean;
  /** Where `selfLeaving`, the row that keeps the current row's place once it has gone
   *  (`standInFor`), with the current row among the rows leaving above it; null otherwise, and
   *  where no row can. */
  standIn: Place | null;
  /** The window's offset immediately before a commit in which this component holds the
   *  browser's own anchoring off for the window (`holdNativeAnchoring`), else null. */
  windowTop: number | null;
}

/** The page's control row (`components/ControlRow.tsx`) a press of the reader's is live in, on a
 *  control of that row or at its root (`pressMadeIn`), and the row's top, in viewport pixels,
 *  immediately before the commit: the reader's place in that press's commit (`pressedRow`). */
interface PressedRow {
  row: Element;
  top: number;
}

/** What the reader can see of the rows, top and bottom, in viewport pixels. */
interface View {
  bottom: number;
  top: number;
}

/** The space `element` takes in its list, in pixels: from its own top to the top of the element
 *  after it, the gap between them included, else to its own bottom. What a row a press of the
 *  reader's takes away frees for the rows after it (`ViewportAnchor`'s `leaving`), and what the
 *  selection bar's hand-off reads to tell where each row will stand once such rows have gone
 *  (`components/SelectionBar.tsx`), so the two agree. */
export function spaceTaken(element: Element): number {
  const box = element.getBoundingClientRect();
  const after = element.nextElementSibling;
  return (after === null ? box.bottom : after.getBoundingClientRect().top) - box.top;
}

export interface ViewportAnchorProps {
  as?: "div" | "section";
  children: ReactNode;
  className?: string;
  /** When false at commit time, the commit is left to shift the view (the Conversation while it
   *  follows the latest turn). Defaults to always compensating. */
  enabled?: () => boolean;
  /** Attribute naming the group a row sits in, e.g. `data-inbox-section`. A row whose value
   *  changed across a commit was relocated - it left its place for another group - rather than
   *  shifted by content around it, and the view is not dragged after it. */
  group?: string;
  /** Attribute naming each row and carrying its id, e.g. `data-inbox-row`. */
  item: string;
  /** Whether a press of the reader's is taking row `id` away from its place: for example a
   *  Dismiss, or a write that moves the row into another group once it lands. The anchor does not
   *  hold the current row in place against the going of such a row above it: the current row and
   *  the rows after it move up by the whole space the row took, and the view does not scroll. It
   *  is asked of the render that last drew the rows and of the render being committed alike,
   *  because a write can stop naming its rows in the same commit that moves them. With no
   *  function, no row is named. */
  leaving?: (id: string) => boolean;
  label?: string;
  /** Receives the rendered root element, for a host that also queries the rows itself. */
  rootRef?: RefObject<HTMLElement | null>;
  /** The element the rows scroll in, when they scroll in one rather than the document (the
   *  Inbox beside its reading pane, where each column scrolls in itself). The anchor then reads
   *  and moves that element's `scrollTop`, and measures the list against its box. With no ref,
   *  or one holding null, the window scrolls the rows as it always has. */
  scroller?: RefObject<HTMLElement | null>;
}

const probeSteps = 5;
const probeStepPx = 8;

// An O(1) hit test for the row nearest a height of the list's own visible portion - its centre, or
// the height a press landed at (`y`) - rather than a getBoundingClientRect scan over every row
// (hundreds of forced layout reads per update on a long list). The probe point is derived from
// the list's bounding rect - not window.innerWidth/innerHeight - because the main column is rarely
// centred in (or as wide as) the viewport: the sidebar and margin panel narrow it well before the
// window itself is narrow, and hit-testing the window's centre can land in a neighbouring column
// entirely, missing every probe. Its x is the middle of the list's own column for a press too: a
// press in the page's margin beside the list is level with the row it was made beside.
// elementFromPoint there can still miss within the column - landing on a gap, heading, or
// composer - in which case this probes downward first, then upward, in small steps: a content
// change lands between two rows more often than exactly on one, and probing downward first means
// the answer for a point in the gap is consistently "whichever row is below", matching reading
// order. What is visible is the window's own box, or `scroller`'s when the rows scroll in one: a
// list scrolled inside an element reaches above and below that element's edges, not the
// window's, and probing a point the scroller clips lands on whatever covers it instead.
function rowAtHeight(
  root: HTMLElement,
  selector: string,
  scroller: HTMLElement | null,
  y: number | null
): HTMLElement | null {
  const rect = root.getBoundingClientRect();
  const box = scroller === null ? null : scroller.getBoundingClientRect();
  const visibleLeft = Math.max(rect.left, box?.left ?? 0);
  const visibleRight = Math.min(rect.right, box?.right ?? window.innerWidth);
  const x = (visibleLeft + visibleRight) / 2;
  const visibleTop = Math.max(rect.top, box === null ? 0 : box.top);
  const visibleBottom = Math.min(rect.bottom, box === null ? window.innerHeight : box.bottom);
  const at = y ?? (visibleTop + visibleBottom) / 2;
  const hit = (offset: number): HTMLElement | null =>
    document.elementFromPoint(x, at + offset)?.closest<HTMLElement>(selector) ?? null;

  let found = hit(0);
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(step * probeStepPx);
  }
  for (let step = 1; found === null && step <= probeSteps; step += 1) {
    found = hit(-step * probeStepPx);
  }
  return found !== null && root.contains(found) ? found : null;
}

function rowWithin(root: HTMLElement, selector: string, node: Element | null): HTMLElement | null {
  const row = node?.closest<HTMLElement>(selector) ?? null;
  return row !== null && root.contains(row) ? row : null;
}

/**
 * Keeps the row the reader is currently on pinned to its viewport position across commits that
 * shift content around it: a row inserted or removed above, a reorder within its section, a
 * banner appearing. The current row is the one with keyboard focus within it, else the one under
 * the pointer, else the one nearest the list's visible centre - the first two hold even at the
 * top of the page (the hand is on that row), the centre only once the reader has scrolled, since
 * a reader at the top is looking for arrivals and should see them land. A row that itself leaves
 * its place for another `group` (a section) is not followed: the reader's place is where the row
 * was, not where it went. The pointer moving from a row onto the hover card a reference in that
 * row opened keeps that row the current one while the card is open: the card is the row's own
 * doing and sits outside the list only because it is `fixed`. So does the focus in a menu or a
 * list a control of a row opened - the priority's menu, the hand-on's list - which the page
 * portals out of the list and which hands the focus back into the row as it closes: while it is
 * open the focus is the hand on that row (`openerOf`). And the focus on its way from one element
 * to another is already the hand on the row it goes to (`focusGoingTo`). A press of the pointer
 * that takes the focus to no row - the list's background, the page's margin beside it - is the
 * hand where it landed while it is in flight, with neither the focus nor a resting pointer on a
 * row: the row it landed on, else the row nearest the height it landed at, else the one nearest
 * the centre. Neither of the last two is ever the row the focus left in that press, which the
 * press may be letting go: where that row is the one found, the row before it keeps its place,
 * and the rows after it move up into the place it leaves (`pressing`).
 *
 * One shift is not compensated: a row that a press of the reader's takes away from above the
 * current row (`leaving`: for example a Dismiss, or a snooze that moves its rows into another
 * group), with the heading of a group its going empties. The reader asked for that row to go, so
 * the current row and the rows after it move up by the whole space the row took, the part above
 * the view's top included: the view is not scrolled to hold them. Every other change of height
 * above the current row in the same commit, growing or shrinking, is compensated, so the place
 * the row left stays where it stood on screen: the Inbox's **Mark all read** goes with the last
 * unread rows, and below `sm` the header loses the line it stood on, so the view scrolls up by
 * that line.
 * Where the press takes the current row itself away, as a thread's own Dismiss does, the row that
 * keeps its place once it has gone (`standInFor`) is held the same way: the row after it, which
 * moves up into its place, else the row before it. A current row that holds the focus and is left
 * partly in sight is then brought whole into sight at its nearest edge, and a focus left wholly
 * out of sight is not scrolled to: the selection bar's hand-off has put the focus on a row that
 * will stand in sight once those rows have gone (`useLastMarkedPlace`). The browser's own scroll
 * anchoring is turned off on the box that scrolls the rows in every commit in which a row above
 * the current one is named as leaving, and in every commit in which the current row itself is
 * named: a thread's own Dismiss takes away the row the reader's hand is on, and the browser
 * holds the view against its going as much as against a row's above. The snapshot taken before
 * the commit cannot tell whether that row goes in it, and the browser would hold the view
 * against that going. It is turned back on as soon as this component has kept its place. Where
 * the window scrolls the rows and moved in such a commit all the same, it is put back where the
 * press found it (`keepPlace`). A row that goes in the same commit without the reader's press,
 * by a live update or another device, is compensated as any other shift is, so the rows in view
 * do not jump under it.
 *
 * A press of the reader's in the page's control row (`components/ControlRow.tsx`), while that
 * row stands outside the list in the box that scrolls it, makes the row their place for the
 * commit, ahead of every row: a press live on a control of the row or at its root
 * (`pressMadeIn`), wherever the focus is. The reader's place is the control they pressed, so the
 * row is put back where it stood (`pressedRow`): the rows the press brings back into the list or
 * takes out of it, and the list growing or shrinking under the row, move nothing above them, and
 * the control the row hands the focus to as a chip goes stays where it stood. A change above the
 * row in the same commit is made up for. A commit the reader did not make while the focus rests
 * in that row, by a live update or another device, is made up for around the row they are on, as
 * any other is.
 *
 * Rows are the elements matching `[item]`, identified by that attribute's value, and must keep
 * their identity across the commit (a remounted row is a new node and cannot be found again).
 * The document scrolls them unless the host names a `scroller`, in which case that element's
 * `scrollTop` is both what is read and what is moved, and the window's scroll is left alone.
 *
 * A class because `getSnapshotBeforeUpdate` has no hook equivalent, and the measurement must be
 * taken in the commit phase, immediately before React mutates the DOM: a render can complete and
 * then wait for its commit (React yields between the two for non-urgent updates), and anything
 * measured during that render is stale by the time the commit lands if the reader scrolled or a
 * click moved the page in between - the Conversation's Jump to latest was undone exactly this
 * way by a render-phase measurement. The same timing is why nothing cached from a `scroll`
 * handler can be trusted here: a scroll performed immediately before a live update may not have
 * dispatched its event before React commits. The one place that is cached (`held`) is therefore
 * used only while the box it was saved against still sits at the offset it was saved at, so
 * such a scroll drops it instead of being undone by it. `componentDidUpdate` then compensates
 * exactly once, before paint.
 */
export class ViewportAnchor extends Component<
  ViewportAnchorProps,
  object,
  Snapshot | PressedRow | null
> {
  private root: HTMLElement | null = null;
  private hovered: HTMLElement | null = null;
  /** Set while the pointer left the hovered row for the hover card a reference in that row
   *  opened. The card is `fixed` and portaled into `document.body`
   *  (`features/refs/RefPreview.tsx`), so no later boundary event on this root says where the
   *  pointer went from there, and the row is held as the reader's place only for as long as
   *  that card is open - once the pointer leaves it the card closes itself
   *  (`REF_PREVIEW_CLOSE_DELAY_MS`) and `interacted` lets the row go. */
  private onOwnCard = false;
  /** The row the reader's hand is on, the viewport top they last saw it at, and the two things
   *  that keep that top true: the offset the box was at when it was saved, and the group the
   *  row was in. The top is read rather than re-measured because the view can be wrong when a
   *  commit arrives: a change this component's own commits never see has already moved the
   *  row. A card below the hand answering itself, a thread body arriving, Markdown finishing -
   *  each is a child's own commit, which re-renders the child alone, so
   *  `getSnapshotBeforeUpdate` is never called for it. Measured in the Inbox drawer: the
   *  answered row's card shrinks by a card's height one commit before the list drops the row,
   *  and the reader's row ended exactly that far out.
   *
   *  Kept only where the host names a `scroller`. Where the rows scroll in the document the
   *  browser's own scroll anchoring covers that drift, and a cached place there would only undo
   *  a scroll whose event has not arrived yet; in a named box the browser does not cover it,
   *  which is what `contentResized` stands in for.
   *
   *  A different `scrollTop` means the view moved without this component hearing of it, and a
   *  different `group` means the row was relocated rather than shifted. On either the top no
   *  longer says where the reader's place is, and the commit measures the row instead. */
  private held: { group: string | null; id: string; scrollTop: number; top: number } | null = null;
  private contentSize: ResizeObserver | null = null;
  /** Set by a scroll this component made, so the scroll event it raises is not read as the
   *  reader moving the view. Scroll events run before resize observations in a frame, so
   *  without this the compensation's own event would record the place it just corrected. */
  private selfScrolled = false;

  /** Where the pointer last was, in viewport pixels. A row moving under a pointer that has not
   *  moved makes the browser dispatch `pointerout`/`pointerover` again, with the same
   *  coordinates: that is the list moving under the hand, which is the thing being compensated
   *  for, so it must not be read as the reader looking somewhere new. */
  private pointerAt: { x: number; y: number } | null = null;
  /** The boxes whose own scroll anchoring is turned off for a press's commit, each with the
   *  inline `overflow-anchor` it had before (`holdNativeAnchoring`). Empty while it is on. */
  private nativeAnchoring: { box: HTMLElement; priority: string; value: string }[] = [];
  /** The frame request that gives the box its scroll anchoring back once a rendering update has
   *  run with it off (`releaseAfterRendering`). Null while none is pending. */
  private releaseFrame: number | null = null;
  /** Where the focus is going while it moves: the `relatedTarget` of the `focusout` that leaves an
   *  element, until the `focusin` on the element it lands on. A commit made inside that move - one
   *  a row's own `blur` asks for, as the Inbox's does as the focus leaves the row it holds - finds
   *  the focus on no element, the body, and the hand is where the focus is going. On a phone a
   *  tap's `mousedown` moves the focus, and its `mouseup` and `click` follow on whatever then
   *  stands under the finger, which has left the screen: right after a tap on P1 held the picked
   *  card in its band, a tap on another card's Snooze let that card go inside the move, the commit
   *  kept the row at the list's centre - the very card it moved up its group - and the window
   *  followed it 1,700 px up, so the tap opened nothing. */
  private focusGoingTo: Element | null = null;
  /** The pointer's press in flight, from its `mousedown` to its `mouseup`, or until the browser
   *  ends it without a release (`hearPressCancellation`). Without those other endings, a drag
   *  of a link could hold its row as the reader's place in every later commit, even at the top
   *  of the page. It holds the element the press landed on, the height
   *  it landed at in viewport pixels, and the row the focus left as the press moved it
   *  (`focusLeaving`), null while it left none. The press's `mousedown` moves the focus, and a
   *  press on no row - the list's background, the page's margin beside it - takes it to the
   *  page's `main` or to none, where it is the hand on no row, while a row its host holds as long
   *  as the focus is in it is let go inside that move: the Inbox lets go of a card a priority pick
   *  moved into another band (`releaseOnFocusOut`), and the commit moves the card up its group.
   *  Measured with that card at the list's centre: the centre was that card, and the
   *  view followed it up, 1,351 px in the Inbox's cards at 1024 px and 1,375 px in its drawer at
   *  1440 px on a click on the list's background, and 1,711 px on a phone on a tap on the page's
   *  margin. So while the press is in flight it is the hand on the row it landed on, else on the
   *  row nearest the height it landed at, and the row the focus left is never the place to keep
   *  (`current`). Nor is holding no row: the browser's own anchoring takes that card for its
   *  anchor - React moves the rows above it, not the card - and followed it all the same,
   *  1,711 px, on a tap on the page's margin level with the card's middle. */
  private pressing: { left: HTMLElement | null; target: Element; y: number } | null = null;
  private stopHearingCancellations: (() => void) | null = null;

  /** The box the host named, while it holds one: what this anchor reads its offset from and
   *  what it moves. Null where the rows scroll in the document, which is the window's own
   *  scroll and the browser's own anchoring. */
  private scrollerElement(): HTMLElement | null {
    return this.props.scroller?.current ?? null;
  }

  /** What the reader can see of the rows: the box the host named, cut to the window, else the
   *  window. */
  private view(): View {
    const box = this.scrollerElement()?.getBoundingClientRect();
    return {
      bottom: Math.min(window.innerHeight, box?.bottom ?? window.innerHeight),
      top: Math.max(0, box?.top ?? 0),
    };
  }

  /** Turns the browser's own scroll anchoring off on the box that scrolls the rows for the
   *  commit in flight: the box the host named, else the window, whose anchoring the document's
   *  root sets and, while a dialog's scroll lock puts `overflow: hidden` on the body
   *  (`useDialog`), the body too: Chromium then takes the window's anchoring from the body, and
   *  held the view at 390 px against the going of the Inbox row a Decline dialog went with. The
   *  browser would hold the view against the going of the rows a press of the reader's takes
   *  away, which is the shift this component lets happen. The value is read back at once, so it
   *  is in force before React removes the rows. A hold still pending from an earlier press keeps
   *  the value each box had before it. `releaseAfterRendering` turns it back on. */
  private holdNativeAnchoring(): void {
    this.cancelRelease();
    const scroller = this.scrollerElement();
    const boxes = scroller === null ? [document.documentElement, document.body] : [scroller];
    if (this.nativeAnchoring[0]?.box !== boxes[0]) {
      this.releaseNativeAnchoring();
      this.nativeAnchoring = boxes.map((box) => ({
        box,
        priority: box.style.getPropertyPriority("overflow-anchor"),
        value: box.style.getPropertyValue("overflow-anchor"),
      }));
    }
    for (const box of boxes) box.style.setProperty("overflow-anchor", "none");
    for (const box of boxes) void getComputedStyle(box).overflowAnchor;
  }

  /** Gives the box its scroll anchoring back once a rendering update has run with it off: two
   *  frames on, since a frame's callbacks run before that frame's layout. WebKit adjusts for the
   *  shift a commit made at a layout after it rather than in it, so anchoring turned back on in
   *  the commit's own task let it scroll the window against the going of the rows a press took
   *  away, a move this component had already let happen. */
  private releaseAfterRendering(): void {
    if (this.nativeAnchoring.length === 0) return;
    this.cancelRelease();
    this.releaseFrame = requestAnimationFrame(() => {
      this.releaseFrame = requestAnimationFrame(() => {
        this.releaseFrame = null;
        this.releaseNativeAnchoring();
      });
    });
  }

  private cancelRelease(): void {
    if (this.releaseFrame === null) return;
    cancelAnimationFrame(this.releaseFrame);
    this.releaseFrame = null;
  }

  /** Gives each box back the scroll anchoring `holdNativeAnchoring` turned off, with the inline
   *  value it had, at once. */
  private releaseNativeAnchoring(): void {
    this.cancelRelease();
    const held = this.nativeAnchoring;
    this.nativeAnchoring = [];
    for (const { box, priority, value } of held) {
      if (value === "") {
        box.style.removeProperty("overflow-anchor");
      } else {
        box.style.setProperty("overflow-anchor", value, priority);
      }
    }
  }

  /** The group `row` is in, null where the host names none. A kept place belongs to one: a row
   *  that is in another group was relocated rather than shifted, and the place no longer says
   *  where the reader was looking. */
  private groupOf(row: Element): string | null {
    const { group } = this.props;
    return group === undefined ? null : row.getAttribute(group);
  }

  /** Records where the hand's row sits now as the place to keep it, with the box offset and the
   *  group that place belongs to. Called where the view is the reader's own: the pointer moving
   *  onto a row or off one, and a scroll they made. A host whose rows scroll in the document
   *  keeps nothing. */
  private readonly rememberHeld = (): void => {
    const scroller = this.scrollerElement();
    const current = scroller === null ? null : this.interacted();
    if (scroller === null || current === null) {
      this.held = null;
      return;
    }
    this.held = {
      group: this.groupOf(current.row),
      id: current.id,
      scrollTop: scroller.scrollTop,
      top: current.row.getBoundingClientRect().top,
    };
  };

  /** The kept top for `id`, while it is still the place the reader saw: the box the host named
   *  has not moved since, and the row is still in the group it was saved in. Null otherwise,
   *  and the caller measures the row. */
  private heldTop(id: string, group: string | null): number | null {
    const held = this.held;
    const scroller = this.scrollerElement();
    if (held === null || scroller === null) return null;
    const holds = held.id === id && held.group === group && held.scrollTop === scroller.scrollTop;
    return holds ? held.top : null;
  }

  /** Called once a compensation has put `id` at `to` from the place it was kept at, `from`. `to`
   *  is `from` save where rows a press of the reader's took away let the row move up
   *  (`leaving`). The row is at its kept place again, so that place belongs to the offset the
   *  compensation left behind. Without this the first compensation would invalidate the kept
   *  place for every later one, and the next commit would put a row the press let move up back
   *  where it stood before. */
  private restampHeld(id: string, from: number, to: number): void {
    const held = this.held;
    const scroller = this.scrollerElement();
    if (held === null || scroller === null || held.id !== id || held.top !== from) return;
    held.scrollTop = scroller.scrollTop;
    held.top = to;
  }

  /** True when this pointer event is the hand moving rather than the list moving under it. */
  private handMoved(event: PointerEvent): boolean {
    const at = this.pointerAt;
    this.pointerAt = { x: event.clientX, y: event.clientY };
    return at === null || at.x !== event.clientX || at.y !== event.clientY;
  }

  /** Moves the box this anchor owns and remembers that it did. */
  private scrollBy(delta: number): void {
    const scroller = this.scrollerElement();
    if (scroller === null) {
      const before = window.scrollY;
      window.scrollBy(0, delta);
      this.selfScrolled = window.scrollY !== before;
      return;
    }
    const before = scroller.scrollTop;
    scroller.scrollTop += delta;
    this.selfScrolled = scroller.scrollTop !== before;
  }

  private readonly viewScrolled = (event: Event): void => {
    const scroller = this.scrollerElement();
    const mine = scroller === null ? event.target === document : event.target === scroller;
    if (!mine) return;
    if (this.selfScrolled) {
      this.selfScrolled = false;
      return;
    }
    this.rememberHeld();
  };

  /** The root's content changed height outside a commit of this component's. Puts the hand's
   *  row back where the reader last saw it, in the box the host named; with no such box the
   *  document's own scroll anchoring owns this and nothing here runs. The kept place is used
   *  under the same conditions a commit uses it under, the row's group included. */
  private readonly contentResized = (): void => {
    const held = this.held;
    const root = this.root;
    // No box check of its own: nothing is ever held without one (`rememberHeld`), and `heldTop`
    // below answers null where the host names none.
    if (held === null || root === null) return;
    const element = this.rowById(root, held.id);
    if (element === null) {
      this.held = null;
      return;
    }
    const top = this.heldTop(held.id, this.groupOf(element));
    if (top === null) return;
    const delta = element.getBoundingClientRect().top - top;
    if (delta === 0) return;
    this.scrollBy(delta);
    this.restampHeld(held.id, top, top);
  };

  private readonly focusLeaving = (event: FocusEvent): void => {
    this.focusGoingTo = event.relatedTarget instanceof Element ? event.relatedTarget : null;
    const press = this.pressing;
    if (press !== null && event.target instanceof Element) press.left = this.handRow(event.target);
  };

  private readonly focusArrived = (): void => {
    this.focusGoingTo = null;
  };

  private readonly pressStarted = (event: MouseEvent): void => {
    const { clientY: y, target } = event;
    this.pressing = target instanceof Element ? { left: null, target, y } : null;
  };

  private readonly pressEnded = (): void => {
    this.pressing = null;
  };

  componentDidMount(): void {
    // Capture, because `scroll` does not bubble: one listener catches the window's and the
    // named box's alike, and the handler reads a rect only for the box this anchor owns. The
    // focus's move is read at the document's capture phase too, before a row's own `blur`
    // handler asks for the commit that reads it (`focusGoingTo`), and so is a press of the
    // pointer, whose `mousedown` comes before the move it makes (`pressing`).
    window.addEventListener("scroll", this.viewScrolled, true);
    document.addEventListener("focusout", this.focusLeaving, true);
    document.addEventListener("focusin", this.focusArrived, true);
    document.addEventListener("mousedown", this.pressStarted, true);
    document.addEventListener("mouseup", this.pressEnded, true);
    this.stopHearingCancellations = hearPressCancellation(document, this.pressEnded);
  }

  componentWillUnmount(): void {
    window.removeEventListener("scroll", this.viewScrolled, true);
    document.removeEventListener("focusout", this.focusLeaving, true);
    document.removeEventListener("focusin", this.focusArrived, true);
    document.removeEventListener("mousedown", this.pressStarted, true);
    document.removeEventListener("mouseup", this.pressEnded, true);
    this.stopHearingCancellations?.();
    this.stopHearingCancellations = null;
    this.pressing = null;
    this.contentSize?.disconnect();
    this.contentSize = null;
    this.releaseNativeAnchoring();
  }

  // Both listeners hang off the rendered root, not `document`: `pointerover`/`pointerout` bubble,
  // so a pointer entering a row still reaches the root, and leaving a row for anything outside
  // still fires `pointerout` on the row being left with the same `relatedTarget`.
  private readonly pointerOver = (event: PointerEvent): void => {
    const root = this.root;
    if (root === null || !(event.target instanceof Element)) {
      return;
    }
    this.hovered = rowWithin(root, `[${this.props.item}]`, event.target);
    this.onOwnCard = false;
    if (this.handMoved(event)) this.rememberHeld();
  };

  // A pointer moving from a row onto the card that row's own reference opened has left nothing
  // the reader can see leaving: the card is the row's doing, drawn outside the list only
  // because it is `fixed`. Releasing the row there is what moved the row under the reader when
  // a refetch landed with the pointer on the card.
  private readonly pointerOut = (event: PointerEvent): void => {
    const row = this.hovered;
    if (
      row === null ||
      (event.relatedTarget instanceof Node && row.contains(event.relatedTarget))
    ) {
      return;
    }
    if (refPreviewCardContains(event.relatedTarget) && refPreviewOpenedIn(row)) {
      this.onOwnCard = true;
      return;
    }
    this.hovered = null;
    this.onOwnCard = false;
    if (this.handMoved(event)) this.rememberHeld();
  };

  private readonly setRoot = (node: HTMLElement | null): void => {
    if (this.root !== null) {
      this.root.removeEventListener("pointerover", this.pointerOver);
      this.root.removeEventListener("pointerout", this.pointerOut);
    }
    this.contentSize?.disconnect();
    this.contentSize = null;
    this.root = node;
    if (node !== null) {
      node.addEventListener("pointerover", this.pointerOver);
      node.addEventListener("pointerout", this.pointerOut);
      // Not every environment the unit tests run in has one (happy-dom); there the hold is the
      // commit path alone, which is what those tests exercise.
      if (typeof ResizeObserver !== "undefined") {
        this.contentSize = new ResizeObserver(this.contentResized);
        this.contentSize.observe(node);
      }
    }
    const { rootRef } = this.props;
    if (rootRef !== undefined) rootRef.current = node;
  };

  /** The row a focus on `element` is the hand on: the row of the list `element` is in, else the
   *  row of the control that opened the menu or the list `element` is in (`openerOf`). */
  private handRow(element: Element | null): HTMLElement | null {
    const root = this.root;
    if (root === null || element === null) return null;
    const selector = `[${this.props.item}]`;
    return rowWithin(root, selector, element) ?? rowWithin(root, selector, openerOf(element));
  }

  /** The row the reader's hand is on: focus within a row, or within a menu or a list a control
   *  of a row opened (`openerOf`), else the pointer resting on one. While the focus moves, and is
   *  on no element but the body, it is on the element it is going to (`focusGoingTo`), while that
   *  element is still on the page. No layout reads, so a parent may call it on every render. */
  interacted(): (AnchorRow & { row: HTMLElement }) | null {
    const root = this.root;
    if (root === null) return null;
    const going = this.focusGoingTo;
    const inTransit = going?.isConnected === true && focusOnDocument();
    const focused = this.handRow(inTransit ? going : document.activeElement);
    const resting =
      this.hovered !== null && (!this.onOwnCard || refPreviewOpenedIn(this.hovered))
        ? this.hovered
        : null;
    const hovered = resting !== null && root.contains(resting) ? resting : null;
    const chosen =
      focused !== null
        ? { row: focused, via: "focus" as const }
        : hovered !== null
          ? { row: hovered, via: "pointer" as const }
          : null;
    return chosen === null ? null : { ...chosen, id: this.idOf(chosen.row) };
  }

  /** `interacted()`, else, while a press of the pointer is in flight, the row it landed on or the
   *  one nearest the height it landed at (`pressing`), else - only once the reader has scrolled -
   *  the row nearest the list's visible centre (both hit-test probes: layout reads), each probe's
   *  row kept for by another where it is the row the focus left in that press (`keptFor`). The
   *  row a commit will anchor to. The box that scrolls is `scroller`'s element while the host
   *  gave one and it holds one, else the window, and `componentDidUpdate` moves whichever this
   *  read. */
  current(): (AnchorRow & { row: HTMLElement }) | null {
    const interacted = this.interacted();
    if (interacted !== null) return interacted;
    const root = this.root;
    if (root === null) return null;
    const selector = `[${this.props.item}]`;
    const scroller = this.scrollerElement();
    const press = this.pressing;
    const letGo = press?.left ?? null;
    const pressed =
      press === null
        ? null
        : (rowWithin(root, selector, press.target) ??
          this.keptFor(rowAtHeight(root, selector, scroller, press.y), letGo));
    if (pressed !== null) return { id: this.idOf(pressed), row: pressed, via: "pointer" };
    if ((scroller === null ? window.scrollY : scroller.scrollTop) <= 0) return null;
    const row = this.keptFor(rowAtHeight(root, selector, scroller, null), letGo);
    return row === null ? null : { id: this.idOf(row), row, via: "center" };
  }

  /** `found`, or, where it is `letGo` - the row the focus left in a press, which the press may be
   *  letting go (`pressing`) - the row that keeps the place around it as it goes: the row before
   *  it, which stays where it stood while the rows after it move up into the place it leaves,
   *  else the row after it. Null where the list holds no other row. */
  private keptFor(found: HTMLElement | null, letGo: HTMLElement | null): HTMLElement | null {
    const root = this.root;
    if (found === null || found !== letGo || root === null) return found;
    let passed = false;
    let before: HTMLElement | null = null;
    for (const each of root.querySelectorAll<HTMLElement>(`[${this.props.item}]`)) {
      if (each === found) {
        if (before !== null) return before;
        passed = true;
      } else if (!each.contains(found) && !found.contains(each)) {
        if (passed) return each;
        before = each;
      }
    }
    return null;
  }

  private idOf(row: HTMLElement): string {
    const id = row.getAttribute(this.props.item);
    if (id === null) {
      throw new Error(`viewport anchor row is missing its ${this.props.item} value`);
    }
    return id;
  }

  /** The row of `root` whose `item` attribute is `id`, else null. */
  private rowById(root: HTMLElement, id: string): HTMLElement | null {
    return root.querySelector<HTMLElement>(`[${this.props.item}="${CSS.escape(id)}"]`);
  }

  /** The rows above `row` that the host names as leaving (`leaving`), in the render that last
   *  drew them (`previous`) or in this one, each with how far `row` moves up once it has gone:
   *  the whole space it takes (`spaceTaken`), the part above the view's top included. Layout
   *  reads for the rows named alone; none where the host names none. */
  private leavingAbove(row: HTMLElement, previous: Readonly<ViewportAnchorProps>): Leaving[] {
    const root = this.root;
    if (root === null || (previous.leaving === undefined && this.props.leaving === undefined)) {
      return [];
    }
    const selector = `[${this.props.item}]`;
    const found: Leaving[] = [];
    for (const each of root.querySelectorAll<HTMLElement>(selector)) {
      if (each === row) break;
      const id = this.idOf(each);
      if (!this.named(id, previous)) continue;
      const lead = each.previousElementSibling;
      found.push({
        by: spaceTaken(each),
        id,
        lead:
          lead === null || lead.matches(selector) ? null : { by: spaceTaken(lead), element: lead },
      });
    }
    return found;
  }

  /** Whether the host names row `id` as leaving (`leaving`), in the render that last drew the
   *  rows (`previous`) or in this one: a write can stop naming its rows in the same commit that
   *  moves them. */
  private named(id: string, previous: Readonly<ViewportAnchorProps>): boolean {
    return previous.leaving?.(id) === true || this.props.leaving?.(id) === true;
  }

  /** The row that keeps the place of `row` once a press of the reader's has taken `row` away: the
   *  first row after it that the host does not name as leaving, which moves up into its place,
   *  else the nearest such row before it. Measured as `measure` measures the current row, so the
   *  rows named above it, `row` among them where it stands before, free their space. Null where
   *  the host names every other row. Layout reads for that row and the rows named above it
   *  alone. */
  private standInFor(row: HTMLElement, previous: Readonly<ViewportAnchorProps>): Place | null {
    const root = this.root;
    if (root === null) return null;
    let before: HTMLElement | null = null;
    let after: HTMLElement | null = null;
    let passed = false;
    for (const each of root.querySelectorAll<HTMLElement>(`[${this.props.item}]`)) {
      if (each === row) {
        passed = true;
      } else if (!row.contains(each) && !this.named(this.idOf(each), previous)) {
        if (passed) {
          after = each;
          break;
        }
        before = each;
      }
    }
    const stands = after ?? before;
    if (stands === null) return null;
    return {
      group: this.groupOf(stands),
      id: this.idOf(stands),
      leaving: this.leavingAbove(stands, previous),
      top: stands.getBoundingClientRect().top,
    };
  }

  /** How far the rows `leaving` names let `row` move up, now the commit is in: each row no longer
   *  above `row` - gone from the list, or moved to a place after it - its own `by`, and the
   *  element before it its `by` too where that element went as well. */
  private freedAbove(root: HTMLElement, row: HTMLElement, leaving: readonly Leaving[]): number {
    let freed = 0;
    for (const { by, id, lead } of leaving) {
      const now = this.rowById(root, id);
      const above =
        now !== null && (row.compareDocumentPosition(now) & Node.DOCUMENT_POSITION_PRECEDING) !== 0;
      if (above) continue;
      freed += by;
      if (lead !== null && !lead.element.isConnected) freed += lead.by;
    }
    return freed;
  }

  /** Brings the control that holds the focus in `row` whole into sight at its nearest edge where
   *  the reader can see a part of it, which the page's sight rule allows for a landing partly in
   *  sight (`components/focus-place.ts`): an edge that only touches the view's edge counts as in
   *  sight. Uses `scrollToNearest` rather than `scrollIntoView` so the root's scroll padding
   *  cannot scroll the window past the cut. A control wholly in sight, or wholly out of it, is
   *  left where it is. */
  private showFocused(row: HTMLElement): void {
    const focused = document.activeElement;
    if (focused === null || !row.contains(focused)) return;
    const { bottom, top } = this.view();
    const box = focused.getBoundingClientRect();
    const meets = box.bottom >= top && box.top <= bottom;
    const whole = box.top >= top && box.bottom <= bottom;
    if (meets && !whole) scrollToNearest(focused);
  }

  getSnapshotBeforeUpdate(
    previousProps: Readonly<ViewportAnchorProps>
  ): Snapshot | PressedRow | null {
    const snapshot = this.measure(previousProps);
    // A commit no press of the reader's takes rows away in is one the browser's own anchoring
    // holds the view through, so a hold still pending from a press ends before it. A press in the
    // control row takes no row away from above the place it keeps, which is that row.
    if (
      snapshot === null ||
      "row" in snapshot ||
      (snapshot.leaving.length === 0 && !snapshot.selfLeaving)
    ) {
      this.releaseNativeAnchoring();
    }
    return snapshot;
  }

  /** The page's control row (`components/ControlRow.tsx`) and its top, where a press of the
   *  reader's is live on a control of that row or at its root (`pressMadeIn`), while the row stands
   *  outside the list in the box that scrolls it - the first control row in that box; else null.
   *  The press is read from the row rather than from the focus, which WebKit leaves on the body
   *  after a click on a button. Measured in the Inbox at 390x664 with the window at 30 px: Enter on
   *  the only chip, or on Clear filters, drew the threads the narrowing had left out above the row
   *  nearest the list's centre, and holding that row scrolled the window to 1,269 px, which left
   *  the Filter trigger the control row hands the focus to at -1,140 px, in Chromium, Firefox and
   *  WebKit. The page is searched for the row only while a press is armed (`pressArmed`), so a
   *  commit with none searches nothing. */
  private pressedRow(): PressedRow | null {
    const root = this.root;
    if (root === null || !pressArmed()) return null;
    const row = controlIn(this.scrollerElement() ?? document, CONTROL.controlRow);
    if (row === null || root.contains(row) || !pressMadeIn(row)) return null;
    return { row, top: row.getBoundingClientRect().top };
  }

  /** The row the reader's hand is on and where it stands before the commit, with the rows a
   *  press of theirs takes away above it, the native anchoring held off where there are any and
   *  where the press is taking that row itself away, and then the row that stands in for it; or,
   *  ahead of every row, the control row a press of theirs was made in (`pressedRow`). */
  private measure(previousProps: Readonly<ViewportAnchorProps>): Snapshot | PressedRow | null {
    const { enabled } = this.props;
    if (enabled !== undefined && !enabled()) return null;
    const pressed = this.pressedRow();
    if (pressed !== null) return pressed;
    const current = this.current();
    if (current === null) return null;
    const at = this.groupOf(current.row);
    // Where the reader last saw the row they have their hand on, while that place still holds:
    // a change no commit of this component's saw may already have moved it (`held`), and
    // measuring again there would take that drift for the place to keep.
    const held = this.heldTop(current.id, at);
    const leaving = this.leavingAbove(current.row, previousProps);
    // Whether the press is taking the current row itself away, named in the render that last
    // drew the rows or in this one, as a row above it is. The reader asked for that row to go,
    // so it frees its whole height: the browser's own anchoring, which would hold the view
    // against its going, is turned off for the commit just as for a row above, and the row that
    // stands in for it keeps its place. Measured in the Inbox at 390x664: a thread's own Dismiss
    // moved the window from 437 px to 0 in Chromium, Firefox and WebKit, and the view held at 437
    // with the anchoring off.
    const selfLeaving = this.named(current.id, previousProps);
    const top = held ?? current.row.getBoundingClientRect().top;
    const standIn = selfLeaving ? this.standInFor(current.row, previousProps) : null;
    const holding = leaving.length > 0 || selfLeaving;
    if (holding) this.holdNativeAnchoring();
    const windowTop = holding && this.scrollerElement() === null ? window.scrollY : null;
    return {
      group: at,
      id: current.id,
      leaving,
      selfLeaving,
      standIn,
      top,
      via: current.via,
      windowTop,
    };
  }

  componentDidUpdate(
    _previousProps: Readonly<ViewportAnchorProps>,
    _previousState: Readonly<object>,
    snapshot: Snapshot | PressedRow | null
  ): void {
    try {
      this.keepPlace(snapshot);
    } finally {
      this.releaseAfterRendering();
    }
  }

  /** Puts the row `snapshot` measured back at its top, less the space the rows a press of the
   *  reader's took away from above it freed, once the commit is in: every other change of height
   *  above it in that commit, growing or shrinking, is made up for. Where that press took the row
   *  itself away, the row that stands in for it (`standIn`) is put back the same way. Where the
   *  window scrolls the rows and the browser moved it in a commit whose anchoring this component
   *  held off, the window is put back first: Firefox holds the view against the going of the rows
   *  a press took away while a dialog's scroll lock is on (`useDialog`), `overflow-anchor: none`
   *  on the root and the body notwithstanding, and moved the window 730 px up at 390 px when the
   *  Inbox's Decline dialog went with its row. */
  private keepPlace(snapshot: Snapshot | PressedRow | null): void {
    const root = this.root;
    if (snapshot === null || root === null) return;
    if ("row" in snapshot) {
      this.keepPressedRow(snapshot);
      return;
    }
    const { windowTop } = snapshot;
    if (windowTop !== null && Math.abs(window.scrollY - windowTop) > 0.5) {
      this.scrollBy(windowTop - window.scrollY);
    }
    const current = this.rowById(root, snapshot.id);
    const place = current === null ? snapshot.standIn : snapshot;
    const element = current ?? (place === null ? null : this.rowById(root, place.id));
    if (place === null || element === null) return;
    const { group } = this.props;
    if (group !== undefined && element.getAttribute(group) !== place.group) return;
    const freed = this.freedAbove(root, element, place.leaving);
    const top = place.top - freed;
    const delta = element.getBoundingClientRect().top - top;
    if (delta === 0 && freed === 0) return;
    if (delta !== 0) this.scrollBy(delta);
    this.restampHeld(place.id, place.top, top);
    // The rows a press of the reader's took away moved this row up, and a focus in it must end
    // in the reader's sight.
    if (freed > 0 && snapshot.via === "focus") this.showFocused(element);
  }

  /** Puts the control row a press of the reader's was made in (`pressedRow`) back at its top once
   *  the commit is in, so what the press changed below it moves nothing above it, and a change
   *  above it is made up for. The place kept for the hand's row (`held`) is let go: the commit
   *  moved that row with the list, as the press asked, and a resize after it must not put the row
   *  back (`contentResized`). */
  private keepPressedRow({ row, top }: PressedRow): void {
    this.held = null;
    if (!row.isConnected) return;
    const delta = row.getBoundingClientRect().top - top;
    if (delta !== 0) this.scrollBy(delta);
  }

  render(): ReactNode {
    const { as: Tag = "div", children, className, label } = this.props;
    return (
      <Tag aria-label={label} className={className} ref={this.setRoot}>
        {children}
      </Tag>
    );
  }
}
