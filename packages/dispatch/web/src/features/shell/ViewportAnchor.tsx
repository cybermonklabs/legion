import { Component, type ReactNode, type RefObject } from "react";

import { refPreviewCardContains, refPreviewOpenedIn } from "../refs/ref-preview-card";

export interface AnchorRow {
  id: string;
  /** How the row was chosen: keyboard focus within it, the pointer resting on it, the place kept
   *  (`kept`: the host's, `keepRow`, or, where the host takes its box over, the one the reader's
   *  last act recorded), or, failing all three, the row nearest the list's visible centre. Where
   *  the focus, or the place kept for the reader, is in a row wholly outside the view of the box
   *  the host named, the first row in view stands in for that row (`view`, `rowToKeep`). */
  via: "center" | "focus" | "kept" | "pointer" | "view";
}

interface Snapshot extends AnchorRow {
  /** The row's `group` attribute value immediately before the commit; null without one. */
  group: string | null;
  /** The row's top, in viewport pixels, immediately before the commit. */
  top: number;
}

/** The reader's place as `held` keeps it, or the place a host keeps (`keepRow`). */
interface HeldPlace {
  focus: { element: Element; top: number } | null;
  group: string | null;
  /** The row the host keeps the place of; null for a place the reader's own act recorded. */
  hostRow: HTMLElement | null;
  id: string;
  scrollTop: number;
  top: number;
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
  label?: string;
  /** While a place is kept in the box `scroller` names, this anchor alone keeps that box's
   *  scroll (`keepPlace`): it turns the browser's own scroll anchoring off there, and keeps the
   *  place as the browser's anchoring would, for any change of height in the box. It watches the
   *  box's content around the place kept change height, at every depth, as it watches the root
   *  (`watchedIn`), and makes good a change no observation has reported yet before it reads a
   *  pointer, focus or scroll event (`contentResized`). The place kept then changes only at an
   *  act of the reader's: a scroll that a wheel, a key or a press made (`readerScrolls`), the
   *  pointer moving to a new point (`handMoved`, `pointerMoved`), the focus moving into the rows.
   *  What the browser raises for a change of the page records nothing: a row moving under a
   *  pointer that did not move, a pull-back of the box to its end, the browser's own anchoring,
   *  another script's scroll (`rebaseHeld`), the focus lost with a node that went (`readerKept`).
   *  Off by default, and of no effect where the rows scroll in the document. Left off, the
   *  browser keeps anchoring the box on a node of its own near the top of the view, which holds
   *  the rows in view as content between the kept row and the view changes height. Details'
   *  history turns it on: the place it keeps is often a control the page handed the focus to far
   *  down a long turn, and the browser's move of the box for its own node took that control out
   *  of the view. */
  ownsScrollAnchoring?: boolean;
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

/** How long after an act of the reader's that scrolls, or the last scroll event that act raised,
 *  a scroll of the box is still that act's (`readerScrolls`). A wheel turn or a key raises its
 *  first scroll event in the frame after it, and a smooth scroll, a drag's or a touch's fling one
 *  event a frame until it stops, so the window spans a few slow frames; a key that scrolls
 *  nothing, as Shift does, leaves no act behind once it has passed. */
const READER_SCROLL_MS = 500;

/** What the reader does on the page that can scroll the box the host names: a turn of the wheel,
 *  a key (one that scrolls, or Tab, which brings the control it focuses into the view), and a
 *  press, which drags the scrollbar or the rows under a finger until it ends. */
const READER_SCROLL_ACTS = ["keydown", "pointercancel", "pointerdown", "pointerup", "wheel"];

// An O(1) hit test for the row nearest the centre of the list's own visible portion, rather than
// a getBoundingClientRect scan over every row (hundreds of forced layout reads per update on a
// long list). The probe point is derived from the list's bounding rect - not
// window.innerWidth/innerHeight - because the main column is rarely centred in (or as wide as)
// the viewport: the sidebar and margin panel narrow it well before the window itself is narrow,
// and hit-testing the window's centre can land in a neighbouring column entirely, missing every
// probe. elementFromPoint at the exact centre can still miss within the column - landing on a
// gap, heading, or composer - in which case this probes downward first, then upward, in small
// steps: a content change lands between two rows more often than exactly on one, and probing
// downward first means the answer for a point in the gap is consistently "whichever row is
// below", matching reading order. What is visible is the window's own box, or `scroller`'s when
// the rows scroll in one: a list scrolled inside an element reaches above and below that
// element's edges, not the window's, and probing a point the scroller clips lands on whatever
// covers it instead.
function rowAtCenter(
  root: HTMLElement,
  selector: string,
  scroller: HTMLElement | null
): HTMLElement | null {
  const rect = root.getBoundingClientRect();
  const box = scroller === null ? null : scroller.getBoundingClientRect();
  const visibleLeft = Math.max(rect.left, box?.left ?? 0);
  const visibleRight = Math.min(rect.right, box?.right ?? window.innerWidth);
  const x = (visibleLeft + visibleRight) / 2;
  const visibleTop = Math.max(rect.top, box === null ? 0 : box.top);
  const visibleBottom = Math.min(rect.bottom, box === null ? window.innerHeight : box.bottom);
  const y = (visibleTop + visibleBottom) / 2;
  const hit = (offset: number): HTMLElement | null =>
    document.elementFromPoint(x, y + offset)?.closest<HTMLElement>(selector) ?? null;

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

/** True where `rect` and `box` share some height: what `rect` bounds lies in the view `box`
 *  bounds, wholly or in part. */
function overlaps(rect: DOMRect, box: DOMRect): boolean {
  return rect.bottom > box.top && rect.top < box.bottom;
}

/** How far the box that scrolls the rows is scrolled: `scroller`'s offset, or the window's. */
function scrolledBy(scroller: HTMLElement | null): number {
  return scroller === null ? window.scrollY : scroller.scrollTop;
}

/** What tells the scroll events a box raises for its owner's own scrolls from any other's: the
 *  offset the owner's last scroll left the box at, while the frame that reports it has not run
 *  (`ownScrollMark`). */
export interface OwnScrollMark {
  /** Ends the mark, and the frame asked for to end it. */
  end(): void;
  /** Called after a scroll of the owner's own moved the box from `before` to `at`: that offset is
   *  the owner's own until the frame that reports the move has run. A second move before then
   *  takes the mark with it: the frame raises one event for the box, at the offset the last move
   *  left. A move the box did not follow, already at its end, leaves the mark as it was. */
  move(before: number, at: number): void;
  /** Called at a scroll event of the box, which stands at `at`: true where the event is the
   *  owner's own scroll's, the box standing where that scroll left it. The mark ends here either
   *  way: a frame raises one scroll event for the box, whatever moved it. */
  take(at: number): boolean;
}

/**
 * The mark of a scroll its owner made of the box that scrolls its content, so the scroll event
 * that scroll raises is not taken for the reader moving the view: scroll events run before resize
 * observations in a frame, so the event of a put-back would otherwise record the place it just
 * corrected. Each anchor keeps one (`ViewportAnchor`), and so does Details' hold of the place a
 * key landed the focus at (`details-more.tsx`): the history's anchor and the hold scroll Details'
 * box in the same frames, and each tells its own scroll by a mark of its own, built by this one
 * rule.
 *
 * An event does not say what moved the box, so it is taken for the owner's own only while the box
 * stands at the offset the owner's last scroll left it at, and only within the frame the browser
 * reports the scroll in. A scroll that another write puts back in the same frame leaves the box
 * where the frame found it, and the browser raises no event for it: a mark kept for the next event
 * would outlive that frame and take the reader's next scroll for the owner's own, and their place
 * would not be recorded. A frame runs its scroll steps, which raise the event of a scroll made
 * before them, ahead of its animation-frame callbacks, so a frame asked for at the scroll ends the
 * mark after any event the scroll raises. A scroll made after a frame's scroll steps, in its
 * animation-frame callbacks or its resize observations, is reported in the next frame, and the
 * frame asked for then is that next one, whose callbacks again follow its scroll steps.
 */
export function ownScrollMark(): OwnScrollMark {
  let mark: { at: number; frame: number } | null = null;
  const end = (): void => {
    if (mark !== null) window.cancelAnimationFrame(mark.frame);
    mark = null;
  };
  return {
    end,
    move(before, at) {
      if (at === before) return;
      end();
      const frame = window.requestAnimationFrame(() => {
        mark = null;
      });
      mark = { at, frame };
    },
    take(at) {
      const own = mark;
      end();
      return own !== null && own.at === at;
    },
  };
}

/**
 * The content of `box` around `from` whose change of height can move `from`: each element between
 * `from` and the box, and each element beside `from` or beside one of those. A change of height
 * anywhere in the box that moves `from` changes the height of one of them. Watching less is not
 * enough. Where one of the box's children holds both `from` and the content above it, as Details'
 * one child holds its sections and the history, a section above that grows while another shrinks
 * by as much in the same frame leaves that child's height as it was; and an entry of a list above
 * `from` that grows while one below it shrinks by as much leaves the list's own height as it was.
 * Either moves `from`. Empty where the box does not hold `from`.
 */
export function watchedIn(from: Element, box: Element): Set<Element> {
  const watched = new Set<Element>();
  if (!box.contains(from)) return watched;
  for (let node: Element = from; node !== box; ) {
    const parent: HTMLElement | null = node.parentElement;
    if (parent === null) break;
    for (const beside of parent.children) if (beside !== node) watched.add(beside);
    if (parent !== box) watched.add(parent);
    node = parent;
  }
  return watched;
}

/** Has `observer` watch `now` in place of `watched`, what it watched until now (`watchedIn`):
 *  content drawn around the place since is watched from then on, and content that went is not. */
export function rewatch(
  observer: ResizeObserver | null,
  watched: ReadonlySet<Element>,
  now: ReadonlySet<Element>
): void {
  for (const element of watched) if (!now.has(element)) observer?.unobserve(element);
  for (const element of now) if (!watched.has(element)) observer?.observe(element);
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
 * doing and sits outside the list only because it is `fixed`.
 *
 * The anchor keeps what the reader can see. Where the rows scroll in a box the host names, a
 * focused row that lies wholly outside that box's view is not the row kept: the first row wholly
 * in view is (`rowToKeep`), as a place of the reader's with no focused control, and a focused
 * control's own place is kept only while that control is in the view. A cut of a row wholly
 * outside the view, or a row drawn between such a row and the view, then moves nothing on
 * screen, where keeping the focused row's place, or its control's, moved every row in view by
 * as much. Where the rows scroll in the document the focused row is kept wherever it is.
 *
 * A host can keep a row's place itself while no act of the reader's names one (`keepRow`): a
 * control of the host's outside the list holds the focus, and what it starts changes the list
 * around that control. The row it keeps comes after the reader's focus and pointer and before
 * the centre, and the place goes when the host lets it go, when an act of the reader's records
 * another one, and when the row leaves the list.
 *
 * Where it names a `scroller`, a host can also take that box over while a place is kept there
 * (`ownsScrollAnchoring`): this anchor then alone keeps the box's scroll, with the browser's own
 * scroll anchoring off (`keepPlace`). The choice is the host's. Without it the browser keeps
 * anchoring the box on a node of its own near the top of the view, which holds the rows in view
 * as content between the kept row and the view changes height; the Inbox's columns leave it on.
 * Details' history takes the box over, since the place it keeps is often a control far down a
 * long turn, and the browser's move of the box for its own node took that control out of the
 * view. Such a host's place then stands in for the browser's anchoring, as the CSS scroll
 * anchoring spec keeps its own node: any change of height in the box, whatever element changed,
 * moves the box by as far as it moved the kept place, and that is done before a pointer, focus or
 * scroll event of the same change is read, since Chromium raises the boundary events of a row
 * moved from under a resting pointer at the start of the frame, before its resize observations.
 * Only the reader's own act records another place: a scroll their wheel, key or press made, the
 * pointer moving to a new point, the focus moving into the rows. The events the browser raises
 * for a change of the page record nothing, and while no act of the reader's names a row, the
 * place kept is the one a commit keeps too.
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
 * used only while the box it was saved against still sits at the offset it was saved at, or sits
 * at its end below that offset, where the browser pulled it back as its content shrank and no
 * scroll of the reader's leaves it (`pulledBack`), so such a scroll drops it instead of being
 * undone by it. `componentDidUpdate` then compensates exactly once, before paint.
 */
export class ViewportAnchor extends Component<ViewportAnchorProps, object, Snapshot | null> {
  private root: HTMLElement | null = null;
  private hovered: HTMLElement | null = null;
  /** Set while the pointer left the hovered row for the hover card a reference in that row
   *  opened. The card is `fixed` and portaled into `document.body`
   *  (`features/refs/RefPreview.tsx`), so no later boundary event on this root says where the
   *  pointer went from there, and the row is held as the reader's place only for as long as
   *  that card is open - once the pointer leaves it the card closes itself
   *  (`REF_PREVIEW_CLOSE_DELAY_MS`) and `interacted` lets the row go. */
  private onOwnCard = false;
  /** The row the reader's hand is on, or, where the focus is in a row wholly outside the view,
   *  the first row in view (`rowToKeep`); the viewport top they last saw it at; and the two
   *  things that keep that top true: the offset the box was at when it was saved, and the group
   *  the row was in. The top is read rather than re-measured because the view can be wrong when
   *  a commit arrives: a change this component's own commits never see has already moved the
   *  row. A card below the hand answering itself, a thread body arriving, Markdown finishing -
   *  each is a child's own commit, which re-renders the child alone, so
   *  `getSnapshotBeforeUpdate` is never called for it. Measured in the Inbox drawer: the
   *  answered row's card shrinks by a card's height one commit before the list drops the row,
   *  and the reader's row ended exactly that far out.
   *
   *  Kept only where the host names a `scroller`. Where the rows scroll in the document the
   *  browser's own scroll anchoring covers that drift, and a cached place there would only undo
   *  a scroll whose event has not arrived yet. In a named box `contentResized` stands in for it:
   *  beside the browser's own anchoring there, which keeps a node of its own choosing rather than
   *  the reader's row, or alone where the host takes the box over (`ownsScrollAnchoring`).
   *
   *  A place the host keeps (`keepRow`) is kept the same way, at the row the host named, and
   *  is marked as the host's (`hostRow`), so letting it go leaves a place of the reader's
   *  alone.
   *
   *  Where the focus is on a control inside the row and that control lies in the view as the
   *  place is recorded, the place is that control's own (`focus`): the control and its viewport
   *  top then. A long row whose head is above the view is read at the control, and its own words
   *  above the control can change height, which moves the control and leaves the row's top
   *  where it was. `top` is then the row's top that puts the control back at its own
   *  (`contentResized` keeps the two together), so a commit, which measures the row, moves the
   *  control back with it. A control outside the view is no place the reader sees: keeping it
   *  there would move what is in view by as much as its row's words above it changed. A press
   *  of the reader's inside the root, a key or the pointer, lets the control go
   *  (`readerPressed`): what the press opens in the row opens below the row's top, which is
   *  what holds then.
   *
   *  A different `scrollTop` means the view moved without this component hearing of it, unless
   *  the box sits at its end below the offset kept, where the browser pulled it back as its
   *  content shrank and the reader did not move the view (`pulledBack`). A different `group`
   *  means the row was relocated rather than shifted. On either the top no longer says where the
   *  place is, and the commit measures the row instead. Where the host takes its box over, a
   *  scroll that is not the reader's leaves the place kept and moves its top to where that scroll
   *  left it (`rebaseHeld`), so the place holds at the box's new offset. */
  private held: HeldPlace | null = null;
  /** The box whose own scroll anchoring this anchor turned off while it keeps a place there, for
   *  a host that takes the box over (`ownsScrollAnchoring`, `keepPlace`), the `overflow-anchor`
   *  the box's own style gave it before, and the content of the box around the place kept that
   *  this anchor watches change height meanwhile (`watchedIn`). Null while no place is kept
   *  there, and always for a host that does not take its box over. */
  private ownedBox: { anchoring: string; element: HTMLElement; watched: Set<Element> } | null =
    null;
  /** When the reader last acted in a way that scrolls the box, or a scroll that act raised was
   *  last heard (`readerScrolls`), by `performance.now()`; null before any. Read only where the
   *  host takes the box over (`ownsScrollAnchoring`). */
  private readerActedAt: number | null = null;
  /** Whether a press of the reader's is down anywhere on the page: a drag of the box's scrollbar,
   *  or of the rows under a finger, scrolls the box until it ends. */
  private readerPressing = false;
  private contentSize: ResizeObserver | null = null;
  /** The mark of this anchor's own last scroll of what scrolls the rows, the box the host named or
   *  the window (`ownScrollMark`). The scroll event that scroll raises is not the reader moving
   *  the view: recorded, the event of a put-back would keep the place it just corrected, and keep
   *  again a focused control that a press of the reader's let go (`readerPressed`). It is the one
   *  thing that tells that event from the reader's, for every host. */
  private readonly ownScroll = ownScrollMark();

  /** Where the pointer last was, in viewport pixels. A row moving under a pointer that has not
   *  moved makes the browser dispatch `pointerout`/`pointerover` again, with the same
   *  coordinates: that is the list moving under the hand, which is the thing being compensated
   *  for, so it must not be read as the reader looking somewhere new. Where the host takes its
   *  box over it is where the last `pointermove` left the pointer (`pointerMoved`), and null until
   *  the first is heard after the list mounts: a hand that comes to rest moves a few pixels inside
   *  the row it reached, which raises no boundary event, and the browser raises those of a row
   *  moved from under it at that resting point. Elsewhere it is where the last boundary event on
   *  the root put it. */
  private pointerAt: { x: number; y: number } | null = null;

  /** What the reader's last pointer press inside the root went down on, while that press lasts:
   *  until the focus next moves into the root, or until the task queued as the press ended runs
   *  (`pressEnds`). The focus a press gives the control it went down on is that press's own
   *  (`focusMoved`), not a control to keep the place at. A press can give the root no focus: one
   *  on words that take none, in a browser that does not focus a pressed button, or one whose
   *  focus goes outside the root. Its mark kept for the next focus would take a much later one on
   *  that control, the reader tabbing to it or code handing it the focus, for the press's own, and
   *  let go the focused control kept with the place (`readerPressed`). */
  private pressedOn: Node | null = null;

  /** The task that ends the mark of the reader's last press (`pressedOn`), queued as that press
   *  ended inside the root and cancelled by the next press. It runs after the focus the press
   *  gives: a mouse press gives it as it goes down, before it ends, and a touch press with the
   *  click that follows its end in the same task. */
  private pressEnds: number | undefined;

  /** Whether the host takes its box over (`ownsScrollAnchoring`), read once as this anchor is
   *  built: only then does it hear the pointer's own moves and the reader's acts that scroll on
   *  the window (`pointerMoved`, `readerActed`). Read once, the listeners removed as it unmounts
   *  are the ones added as it mounted, whatever the host passes in between. */
  private readonly hearsReaderActs: boolean;

  constructor(props: ViewportAnchorProps) {
    super(props);
    this.hearsReaderActs = props.ownsScrollAnchoring === true;
  }

  /** The box the host named, while it holds one: what this anchor reads its offset from and
   *  what it moves. Null where the rows scroll in the document, which is the window's own
   *  scroll and the browser's own anchoring. */
  private scrollerElement(): HTMLElement | null {
    return this.props.scroller?.current ?? null;
  }

  /** The group `row` is in, null where the host names none. A kept place belongs to one: a row
   *  that is in another group was relocated rather than shifted, and the place no longer says
   *  where the reader was looking. */
  private groupOf(row: Element): string | null {
    const { group } = this.props;
    return group === undefined ? null : row.getAttribute(group);
  }

  /** The row of this list whose `item` value is `id`; null where none is drawn. */
  private rowById(id: string): HTMLElement | null {
    return (
      this.root?.querySelector<HTMLElement>(`[${this.props.item}="${CSS.escape(id)}"]`) ?? null
    );
  }

  /** The row of the place `held` keeps: the row the host keeps (`keepRow`) while it is still in
   *  the root, else the row with the id kept. Null where that row has left the list. */
  private heldRow(held: HeldPlace): HTMLElement | null {
    const host = held.hostRow;
    if (host === null) return this.rowById(held.id);
    return this.root?.contains(host) === true ? host : null;
  }

  /** The focused control kept with the place `held` (`focus`) while that control still holds the
   *  focus inside `row`, the place's row; null otherwise. A control that lost the focus, or left
   *  the row, keeps no place of its own. */
  private keptControl(held: HeldPlace, row: HTMLElement): HeldPlace["focus"] {
    const control = held.focus;
    const holds =
      control !== null &&
      control.element === document.activeElement &&
      row.contains(control.element);
    return holds ? control : null;
  }

  /** Keeps `place`, the reader's or the host's (`keepRow`), as `held`, or none. Where the host
   *  takes the box it named over (`ownsScrollAnchoring`), this anchor alone anchors that box
   *  while a place is kept there, and the browser's own scroll anchoring is turned off there
   *  (`overflow-anchor: none`) until none is kept. The browser anchors on a node of its own
   *  choosing near the top of the view, not on the kept row, so content between the two that
   *  changes height moves the row all the same, and the browser's move of the box for that node
   *  reads as a scroll this anchor did not make: where the frame observes the change after that
   *  move, the place no longer holds and nothing is put back, and where the move's scroll event
   *  comes first, the place is recorded where the change left the row. Measured in Details: the
   *  page's typeface arriving after Load older landed moved the box 39,180 px, and the control
   *  the page had handed the focus to 1,492 px up, out of the view. Since the browser then keeps
   *  the view for no change in that box, this anchor watches the box's content around the place
   *  kept for one, at every depth, as it watches its own root (`watchedIn`). Any other host keeps
   *  the browser's anchoring, which holds the rows in view as content above the view changes
   *  height, as the Inbox's columns do. */
  private keepPlace(place: HeldPlace | null): void {
    this.held = place;
    const box = this.props.ownsScrollAnchoring === true ? this.scrollerElement() : null;
    if (place === null || box === null) {
      this.releaseBox();
      return;
    }
    if (this.ownedBox?.element !== box) {
      this.releaseBox();
      this.ownedBox = {
        anchoring: box.style.getPropertyValue("overflow-anchor"),
        element: box,
        watched: new Set(),
      };
      box.style.setProperty("overflow-anchor", "none");
    }
    const row = this.heldRow(place);
    if (row !== null) this.watchOwnedBox(this.keptControl(place, row)?.element ?? row);
  }

  /** Gives the box whose scroll anchoring `keepPlace` turned off its own back, and stops
   *  watching its content around the place kept. */
  private releaseBox(): void {
    const owned = this.ownedBox;
    if (owned === null) return;
    this.ownedBox = null;
    owned.element.style.setProperty("overflow-anchor", owned.anchoring);
    for (const element of owned.watched) this.contentSize?.unobserve(element);
  }

  /** Brings what this anchor watches in the box it took over up to the box's content around
   *  `from` as it is now (`watchedIn`): the kept row, or the focused control kept in it. Content
   *  drawn around the place since it was kept is watched from then on, and content that went is
   *  not. The root is left out: it watches itself for as long as it is drawn (`setRoot`). */
  private watchOwnedBox(from: Element): void {
    const owned = this.ownedBox;
    if (owned === null) return;
    const now = watchedIn(from, owned.element);
    if (this.root !== null) now.delete(this.root);
    rewatch(this.contentSize, owned.watched, now);
    owned.watched = now;
  }

  /** The row to keep for `place`, the row of the reader's place, where the rows scroll in a box
   *  whose view `box` bounds: the row the focus is in (`rememberHeld`, `rebaseHeld`,
   *  `current()`), or the row of the place the reader's last act recorded, which can be the
   *  pointer's (`readerKept`). That row while any of it lies in the view; else the first row
   *  wholly in view, or, where none is, the first in view in part; null where no row is in view.
   *  The anchor keeps what the reader can see. A row wholly outside the view is one they cannot,
   *  and so is a cut of it, or a row drawn between it and the view: keeping the first row in view
   *  then moves nothing on screen, where keeping that row, or a control in it, moved every row in
   *  view by the change. Layout reads: `interacted()`, which a parent may call on every render,
   *  leaves this to its callers. */
  private rowToKeep(place: HTMLElement, box: DOMRect): HTMLElement | null {
    if (overlaps(place.getBoundingClientRect(), box)) return place;
    let inPart: HTMLElement | null = null;
    for (const row of this.root?.querySelectorAll<HTMLElement>(`[${this.props.item}]`) ?? []) {
      const rect = row.getBoundingClientRect();
      if (rect.top >= box.top && rect.bottom <= box.bottom) return row;
      // The rows stand down the list in document order: none after one below the view is in it.
      if (rect.top >= box.bottom) break;
      if (inPart === null && overlaps(rect, box)) inPart = row;
    }
    return inPart;
  }

  /** Records where the hand's row sits now as the place to keep it, with the box offset and the
   *  group that place belongs to, and, where a control inside the row holds the focus and lies in
   *  the view, that control and where it sits. Where the focus is in a row wholly outside the
   *  view, the place is the first row in view instead (`rowToKeep`), with no focused control: that
   *  row is what the reader sees. Called where the view is the reader's own: the pointer moving
   *  onto a row or off one, a scroll they made, the focus moving into the root, whoever moved
   *  it, and, where the host leaves the box's anchoring on, any other scroll of the box, the
   *  browser pulling it back to its end under a place of theirs included (`viewScrolled`). What
   *  it records, or nothing where the hand is on no row, takes the place of one the host kept
   *  (`keepRow`): the reader's own act says where they are now. A host whose rows scroll in the
   *  document keeps nothing. */
  private readonly rememberHeld = (): void => {
    const scroller = this.scrollerElement();
    const current = scroller === null ? null : this.interacted();
    let row = current?.row ?? null;
    let focus: HeldPlace["focus"] = null;
    if (scroller !== null && current?.via === "focus") {
      const box = scroller.getBoundingClientRect();
      row = this.rowToKeep(current.row, box);
      // The row itself holding the focus says no more than the row's own top, and a control
      // outside the view is no place the reader sees.
      const active = row === current.row ? document.activeElement : null;
      const at = active === null || active === row ? null : active.getBoundingClientRect();
      if (active !== null && at !== null && overlaps(at, box)) {
        focus = { element: active, top: at.top };
      }
    }
    if (scroller === null || row === null) {
      this.keepPlace(null);
      return;
    }
    this.keepPlace({
      focus,
      group: this.groupOf(row),
      hostRow: null,
      id: this.idOf(row),
      scrollTop: scroller.scrollTop,
      top: row.getBoundingClientRect().top,
    });
  };

  /** Where the host takes its box over, a scroll of the box that no act of the reader's made,
   *  another script's or the browser's pulling it back to its end, has moved the view without
   *  the reader choosing another place: the place kept stays, and only its top moves to where the
   *  scroll left it, so the place holds at the box's new offset. The row is the one kept, the
   *  host's or the reader's, whatever the pointer rests on now. Only where the focus is in a row
   *  of the list does the place follow the rule it was recorded by: the focused row while any of
   *  it is in view, else the first row in view (`rowToKeep`), and a focused control kept with
   *  it only while it holds the focus in the view. A control the reader's press let go stays let
   *  go (`readerPressed`). A row that has left the list takes the place with it. What this anchor
   *  watches in the box then follows the place (`watchOwnedBox`). */
  private rebaseHeld(): void {
    const held = this.held;
    const scroller = this.scrollerElement();
    if (held === null || this.root === null || scroller === null) return;
    const box = scroller.getBoundingClientRect();
    let row = this.heldRow(held);
    const current = held.hostRow === null ? this.interacted() : null;
    if (current !== null && current.via === "focus") {
      const keep = this.rowToKeep(current.row, box);
      if (keep !== row) {
        row = keep;
        held.focus = null;
      }
    }
    if (row === null) {
      this.keepPlace(null);
      return;
    }
    held.group = this.groupOf(row);
    held.id = this.idOf(row);
    held.scrollTop = scroller.scrollTop;
    held.top = row.getBoundingClientRect().top;
    const control = this.keptControl(held, row);
    const at = control === null ? null : control.element.getBoundingClientRect();
    if (control !== null && at !== null && overlaps(at, box)) control.top = at.top;
    else held.focus = null;
    this.watchOwnedBox(held.focus?.element ?? row);
  }

  /** The kept top of the row `id` (with a focused control kept in it, the row's top that puts
   *  that control back at its own: `held`), while it is still the place the reader saw: the box
   *  the host named has not moved since, or moved only because the browser pulled it back to
   *  its end (`pulledBack`), and the row is still in the group it was saved in. Null otherwise,
   *  and the caller measures the row. */
  private heldTop(id: string, group: string | null): number | null {
    const held = this.held;
    const scroller = this.scrollerElement();
    if (held === null || scroller === null) return null;
    const unmoved =
      held.scrollTop === scroller.scrollTop || this.pulledBack(held.scrollTop, scroller);
    const holds = held.id === id && held.group === group && unmoved;
    return holds ? held.top : null;
  }

  /** True when `scroller` sits at its end at a lower offset than `savedAt`, the one it was at
   *  when the place was kept: its content shrank by more than the room that was left below the
   *  view, and the browser pulled the scroll back to the new end. The reader did not move the
   *  view there, so their place is still the kept one. A scroll of their own never looks like
   *  this: one up leaves the end, and one down raises the offset. */
  private pulledBack(savedAt: number, scroller: HTMLElement): boolean {
    const { clientHeight, scrollHeight, scrollTop } = scroller;
    // A pixel of slack: `scrollHeight` and `clientHeight` are rounded to whole pixels, and
    // `scrollTop` is not.
    return scrollTop < savedAt && scrollTop >= scrollHeight - clientHeight - 1;
  }

  /** Called once a compensation has put `id` back at `top`: the row is at the kept place again,
   *  so that place belongs to the offset the compensation left behind. Without this the first
   *  compensation would invalidate the kept place for every later one. A focused control kept
   *  in the row keeps its own top: `top` is the row's top that puts it there. */
  private restampHeld(id: string, top: number): void {
    const held = this.held;
    const scroller = this.scrollerElement();
    if (held === null || scroller === null || held.id !== id || held.top !== top) return;
    held.scrollTop = scroller.scrollTop;
  }

  /** True when this pointer event is the hand moving rather than the list moving under it: its
   *  point is not where the pointer last was (`pointerAt`). Where the host leaves its box's
   *  anchoring on, that is where the last boundary event on the root put it, which this event
   *  updates; where the host takes the box over, it is where the last `pointermove` left it
   *  (`pointerMoved`). A hand that moves raises its boundary events at its new point before the
   *  `pointermove` that takes it there, so they read as the hand moving; those the browser
   *  raises for a row moved from under a resting pointer come at the point it rests at. Before
   *  the first `pointermove` heard there, nothing says where the pointer rests, and no boundary
   *  event reads as the hand moving: the browser raises them at a pointer resting where it was as
   *  the list was drawn under it, and as rows move from under it, and that first move is the act
   *  of the hand's that records the place (`pointerMoved`). */
  private handMoved(event: PointerEvent): boolean {
    const at = this.pointerAt;
    if (this.props.ownsScrollAnchoring === true) {
      return at !== null && (at.x !== event.clientX || at.y !== event.clientY);
    }
    this.pointerAt = { x: event.clientX, y: event.clientY };
    return at === null || at.x !== event.clientX || at.y !== event.clientY;
  }

  /** The pointer's own moves, heard on the window as they go down to their target, wherever on
   *  the page the pointer is (`pointerAt`), where the host takes its box over. The first heard
   *  after the list mounts is the hand's act that the boundary events before it were not taken
   *  for (`handMoved`): where it goes down to a target in the root, the place is recorded at the
   *  row the pointer is on now, as a hand moving onto that row records it, once a change of
   *  height not yet observed is made good (`contentResized`). A first move elsewhere on the page
   *  names no row of this list, and records nothing, as later moves there record nothing. */
  private readonly pointerMoved = (event: PointerEvent): void => {
    const first = this.pointerAt === null;
    this.pointerAt = { x: event.clientX, y: event.clientY };
    const root = this.root;
    const inRoot = root !== null && event.target instanceof Node && root.contains(event.target);
    if (!first || !inRoot) return;
    this.contentResized();
    this.hoverAtPointer();
    this.rememberHeld();
  };

  /** The reader acting in a way that scrolls the box (`READER_SCROLL_ACTS`), heard on the window
   *  as the event goes down to its target, wherever on the page it lands, where the host takes
   *  its box over (`readerScrolls`). */
  private readonly readerActed = (event: Event): void => {
    if (event.type === "pointerdown") this.readerPressing = true;
    else if (event.type === "pointerup" || event.type === "pointercancel") {
      this.readerPressing = false;
    }
    this.readerActedAt = performance.now();
  };

  /** True when a scroll of the box heard now is the reader's own: a press of theirs is down, or
   *  they turned the wheel, pressed a key or ended a press, or a scroll that act made was heard,
   *  within the last `READER_SCROLL_MS`. Such a scroll keeps the act going, so a smooth scroll
   *  or a fling stays the reader's for as long as its events come. Anything else that scrolls
   *  the box is the page's: the browser's own anchoring, another script, a pull-back. */
  private readerScrolls(): boolean {
    const at = this.readerActedAt;
    const now = performance.now();
    const reader = this.readerPressing || (at !== null && now - at <= READER_SCROLL_MS);
    if (reader) this.readerActedAt = now;
    return reader;
  }

  /** Where the host takes its box over, the row the pointer rests on now (`hovered`), read at the
   *  point it rests at (`pointerAt`): the rows moved under it, or the box was scrolled under it,
   *  and no act of the reader's says where it is. A pointer on the card its row's reference
   *  opened stays on that row while the card is open. */
  private hoverAtPointer(): void {
    const at = this.pointerAt;
    const root = this.root;
    if (at === null || root === null) return;
    if (this.onOwnCard && this.hovered !== null && refPreviewOpenedIn(this.hovered)) return;
    this.hovered = rowWithin(root, `[${this.props.item}]`, document.elementFromPoint(at.x, at.y));
    this.onOwnCard = false;
  }

  /** Moves what scrolls the rows, the box the host named or the window, by `delta`, and marks
   *  the offset that left it at as this anchor's own (`ownScroll`). */
  private scrollBy(delta: number): void {
    const scroller = this.scrollerElement();
    const before = scrolledBy(scroller);
    if (scroller === null) window.scrollBy(0, delta);
    else scroller.scrollTop += delta;
    this.ownScroll.move(before, scrolledBy(scroller));
  }

  /** A scroll of what scrolls the rows: the box the host named, or the document. Where it is
   *  this anchor's own (`ownScroll`), it records nothing: the place was just put back. Where the
   *  host takes the box over, `ownedScrolled` reads the event. Any other host's is the reader's,
   *  or the page's, and the place is recorded where the view now shows it; a pull-back of the
   *  box to its end first puts the place back (`pulledBack`). */
  private readonly viewScrolled = (event: Event): void => {
    const scroller = this.scrollerElement();
    const mine = scroller === null ? event.target === document : event.target === scroller;
    if (!mine) return;
    if (this.ownScroll.take(scrolledBy(scroller))) return;
    if (scroller !== null && this.props.ownsScrollAnchoring === true) {
      this.ownedScrolled(scroller);
      return;
    }
    // The browser pulling the box back to its end after its content shrank is no scroll of the
    // reader's. Keeping the row's place here would keep it where the shrink moved it, so it goes
    // back where the reader last saw it, as on any other change of the content's height. The
    // place is then recorded as after any other scroll, at the offset the box is left at: the
    // row back where it was, or, where the box's end left no room to put it back, where the
    // pull-back moved it, which is where the reader now sees it. A place the host keeps is
    // recorded there too and stays the host's: a pull-back is no act of the reader's to end it.
    const held = this.held;
    if (held !== null && scroller !== null && this.pulledBack(held.scrollTop, scroller)) {
      this.contentResized();
      const hostRow = this.held?.hostRow ?? null;
      if (hostRow !== null) {
        this.keepRow(hostRow);
        return;
      }
    }
    this.rememberHeld();
  };

  /** A scroll of the box a host took over (`ownsScrollAnchoring`) that this anchor did not make.
   *  A change of height no observation has reported yet is made good first: the browser can
   *  raise this event between the change and the frame's resize observations, and recording the
   *  place then would keep the row wherever the change moved it. Measured in Details: as the
   *  page's typeface arrived after Load older landed, the box's scroll event came before the
   *  frame observed the rows laid out again, which had moved the focused control 38,600 px down.
   *  Then the event records the reader's place only where the scroll is theirs
   *  (`readerScrolls`), at the row the pointer rests on now and the focus. A scroll that left the
   *  box where the place holds moved nothing. A pull-back of the box to its end, the browser's
   *  own anchoring and another script's scroll record nothing: the place kept stays where the
   *  scroll left it (`rebaseHeld`). Details' hold of the place a key landed the focus at scrolls
   *  the box in its own resize observation (`details-more.tsx`), and recording the place at that
   *  scroll's event took the row the pointer rested on then, or none, for the reader's. */
  private ownedScrolled(scroller: HTMLElement): void {
    const reader = this.readerScrolls();
    this.contentResized();
    const held = this.held;
    if (held !== null && held.scrollTop === scroller.scrollTop) return;
    if (!reader || (held !== null && this.pulledBack(held.scrollTop, scroller))) {
      this.rebaseHeld();
      return;
    }
    this.hoverAtPointer();
    this.rememberHeld();
  }

  /** The root's content changed height outside a commit of this component's, or, where the host
   *  takes the box over (`ownsScrollAnchoring`), the box's content around the place kept did
   *  while one is kept there (`keepPlace`, `watchedIn`), or an event of the page is about to be
   *  read there, or the browser pulled the box back to its end for such a change
   *  (`viewScrolled`). Puts the hand's row, or the first row in view kept for a focused row
   *  wholly outside it (`rowToKeep`), back where the reader last saw it, or, where a control
   *  inside it is kept with the place, that control (`held`), or the row the host keeps
   *  (`keepRow`), in the box the host named; with no such box the document's own scroll
   *  anchoring owns this and nothing here runs. The kept place is used under the same conditions
   *  a commit uses it under, the row's group included, and goes once its row has left the root.
   *
   *  Where a scroll whose event has not run yet moved the box off the offset the place was kept
   *  at, a host that leaves the box's anchoring on lets that event record the place again. One
   *  that takes the box over keeps it as the browser's anchoring would: a scroll that alone
   *  moved the place is left for its event to read (`ownedScrolled`); one that put the place
   *  back where it was kept, as Details' hold does in a resize observation ahead of this one,
   *  leaves it holding at the new offset; and a change of height on top of the scroll is made
   *  good by as far as the change moved the place, the scroll kept. */
  private readonly contentResized = (): void => {
    const held = this.held;
    if (held === null || this.root === null) return;
    const element = this.heldRow(held);
    if (element === null) {
      this.keepPlace(null);
      return;
    }
    // While the kept control still holds the focus inside the row, the place is that control's:
    // the row goes where the control is back at its own top, and that is the row's kept top from
    // then on, since the row's own words above the control can change height and leave the row's
    // top where it was.
    const control = this.keptControl(held, element);
    this.watchOwnedBox(control?.element ?? element);
    // Nothing is ever held without a box (`rememberHeld`, `keepRow`), and a row in another group
    // than the one it was kept in was relocated rather than shifted.
    const scroller = this.scrollerElement();
    if (scroller === null || this.groupOf(element) !== held.group) return;
    const offset = scroller.scrollTop;
    const holds = held.scrollTop === offset || this.pulledBack(held.scrollTop, scroller);
    if (!holds && this.props.ownsScrollAnchoring !== true) return;
    const at = element.getBoundingClientRect().top;
    let drift =
      control === null ? at - held.top : control.element.getBoundingClientRect().top - control.top;
    if (!holds) {
      const scrolled = offset - held.scrollTop;
      if (drift !== 0 && drift === -scrolled) return;
      if (drift !== 0) {
        drift += scrolled;
        if (control !== null) control.top -= scrolled;
      }
      held.scrollTop = offset;
    }
    held.top = at - drift;
    if (drift === 0) return;
    this.scrollBy(drift);
    this.restampHeld(held.id, held.top);
    // Where the host takes the box over, a box at its end or its top that could not follow the
    // whole way leaves the place where the change left it, which is where the reader now sees it:
    // kept at the top it was put back to, the next change would scroll the row back there once
    // the box had room, a move of its own the reader never asked for.
    const short = drift - (scroller.scrollTop - offset);
    if (short === 0 || this.props.ownsScrollAnchoring !== true) return;
    held.top += short;
    if (control !== null) control.top += short;
  };

  componentDidMount(): void {
    // Capture, because `scroll` does not bubble: one listener catches the window's and the
    // named box's alike, and the handler reads a rect only for what scrolls this anchor's rows.
    window.addEventListener("scroll", this.viewScrolled, true);
    // A press that went down in the root can end anywhere on the page, so its end is heard on
    // the window (`pressEnded`).
    window.addEventListener("pointerup", this.pressEnded, true);
    window.addEventListener("pointercancel", this.pressEnded, true);
    // Only a host that takes its box over reads the pointer's own moves and the reader's acts
    // that scroll (`pointerMoved`, `readerScrolls`). Whether it does is read once, as this anchor
    // is built (`hearsReaderActs`), so the listeners added here are the ones removed as it
    // unmounts, even where the host changes `ownsScrollAnchoring` in between.
    if (this.hearsReaderActs) {
      window.addEventListener("pointermove", this.pointerMoved, { capture: true, passive: true });
      for (const type of READER_SCROLL_ACTS) {
        window.addEventListener(type, this.readerActed, { capture: true, passive: true });
      }
    }
  }

  componentWillUnmount(): void {
    window.removeEventListener("scroll", this.viewScrolled, true);
    window.removeEventListener("pointerup", this.pressEnded, true);
    window.removeEventListener("pointercancel", this.pressEnded, true);
    if (this.hearsReaderActs) {
      window.removeEventListener("pointermove", this.pointerMoved, true);
      for (const type of READER_SCROLL_ACTS) {
        window.removeEventListener(type, this.readerActed, true);
      }
    }
    this.ownScroll.end();
    window.clearTimeout(this.pressEnds);
    this.releaseBox();
    this.contentSize?.disconnect();
    this.contentSize = null;
  }

  // Both listeners hang off the rendered root, not `document`: `pointerover`/`pointerout` bubble,
  // so a pointer entering a row still reaches the root, and leaving a row for anything outside
  // still fires `pointerout` on the row being left with the same `relatedTarget`. Where the host
  // takes its box over, a change of height not yet observed is made good before either is read
  // (`contentResized`): Chromium raises the boundary events of a row moved from under a resting
  // pointer at the start of the frame, before the frame's resize observations. Measured in
  // Details: Blocked by, above the view, grew by 300 px as the titles it names landed, and the
  // `pointerout` of the entry under the pointer came 35 ms later with nothing put back yet. Such
  // an event records nothing, and the row the pointer rests on is read where it rests once the
  // place is back (`hoverAtPointer`).
  private readonly pointerOver = (event: PointerEvent): void => {
    const root = this.root;
    if (root === null || !(event.target instanceof Element)) {
      return;
    }
    const moved = this.handMoved(event);
    if (this.props.ownsScrollAnchoring === true) {
      this.contentResized();
      if (!moved) {
        this.hoverAtPointer();
        return;
      }
    }
    this.hovered = rowWithin(root, `[${this.props.item}]`, event.target);
    this.onOwnCard = false;
    if (moved) this.rememberHeld();
  };

  // A pointer moving from a row onto the card that row's own reference opened has left nothing
  // the reader can see leaving: the card is the row's doing, drawn outside the list only
  // because it is `fixed`. Releasing the row there is what moved the row under the reader when
  // a refetch landed with the pointer on the card.
  private readonly pointerOut = (event: PointerEvent): void => {
    const owns = this.props.ownsScrollAnchoring === true;
    if (owns) this.contentResized();
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
    const moved = this.handMoved(event);
    if (owns && !moved) {
      this.hoverAtPointer();
      return;
    }
    this.hovered = null;
    this.onOwnCard = false;
    if (moved) this.rememberHeld();
  };

  // The focus moving into the root is the place from then on, whoever moved it: the reader
  // tabbing to a control, or a host handing the focus on by code with no scroll, which raises
  // nothing else this anchor hears, so a later change of the row's height would take the
  // control with it. The focus a pointer press gives the control it went down on, while that
  // press lasts (`pressedOn`), is the press's own, the reader acting on the row
  // (`readerPressed`), and the row's top is what holds then. Where the host takes its box over,
  // a change of height not yet observed is made good first. The focus leaving the root records
  // nothing, and neither does its loss with a node that went, as Chromium raises `focusout` for
  // a focused node it removes: the place kept stays, and a commit keeps it (`current()`).
  private readonly focusMoved = (event: FocusEvent): void => {
    if (this.props.ownsScrollAnchoring === true) this.contentResized();
    const pressed = this.pressedOn;
    this.pressedOn = null;
    this.rememberHeld();
    if (pressed !== null && event.target instanceof Node && event.target.contains(pressed)) {
      this.readerPressed();
    }
  };

  // A key or the pointer pressed inside the root is the reader acting on their row, as "more"
  // drawing the rest of a cut turn's words: what the press opens, opens below the row's top,
  // which stays where it was. The focused control kept with the place is let go, so only a
  // change the reader did not make, a cut or a read arriving, keeps a control's own place, until
  // the focus next moves into the root. Heard as the event goes down to its target, so a
  // control that stops its own event is pressed all the same.
  private readonly readerPressed = (): void => {
    if (this.held !== null) this.held.focus = null;
  };

  private readonly pointerPressed = (event: PointerEvent): void => {
    this.readerPressed();
    window.clearTimeout(this.pressEnds);
    this.pressEnds = undefined;
    this.pressedOn = event.target instanceof Node ? event.target : null;
  };

  // A press ends at its pointerup, or at its pointercancel where the browser takes the pointer
  // for itself, wherever on the page the pointer is then, and its mark goes in a task queued then
  // (`pressEnds`). Heard as the event goes down to its target, as the press itself is.
  private readonly pressEnded = (): void => {
    if (this.pressedOn === null) return;
    window.clearTimeout(this.pressEnds);
    this.pressEnds = window.setTimeout(() => {
      this.pressEnds = undefined;
      this.pressedOn = null;
    }, 0);
  };

  private readonly setRoot = (node: HTMLElement | null): void => {
    if (this.root !== null) {
      this.root.removeEventListener("pointerover", this.pointerOver);
      this.root.removeEventListener("pointerout", this.pointerOut);
      this.root.removeEventListener("focusin", this.focusMoved);
      this.root.removeEventListener("keydown", this.readerPressed, true);
      this.root.removeEventListener("pointerdown", this.pointerPressed, true);
    }
    // A place kept among the old root's rows goes with them, and a box taken over gets its own
    // scroll anchoring back (`keepPlace`).
    this.keepPlace(null);
    this.contentSize?.disconnect();
    this.contentSize = null;
    this.root = node;
    if (node !== null) {
      node.addEventListener("pointerover", this.pointerOver);
      node.addEventListener("pointerout", this.pointerOut);
      node.addEventListener("focusin", this.focusMoved);
      node.addEventListener("keydown", this.readerPressed, true);
      node.addEventListener("pointerdown", this.pointerPressed, true);
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

  /** The row the reader's hand is on: focus within a row, else the pointer resting on one. No
   *  layout reads, so a parent may call it on every render. */
  interacted(): (AnchorRow & { row: HTMLElement }) | null {
    const root = this.root;
    if (root === null) return null;
    const selector = `[${this.props.item}]`;
    const focused = rowWithin(root, selector, document.activeElement);
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

  /** `interacted()`, else the place kept (`kept`): the row the host keeps the place of
   *  (`keepRow`), or, where the host takes its box over, the row of the reader's last recorded
   *  place (`readerKept`); else - only once the reader has scrolled - the row nearest the list's
   *  visible centre (a hit-test probe: layout reads). The row a commit will anchor to. Where the
   *  focus is in a row wholly outside the view of the box the host named, that is the first row
   *  in view instead (`rowToKeep`, `view`), or none where no row is in view: a row the commit
   *  draws between the focused row and the view then moves no row the reader sees. The box that
   *  scrolls is `scroller`'s element while the host gave one and it holds one, else the window,
   *  and `componentDidUpdate` moves whichever this read. */
  current(): (AnchorRow & { row: HTMLElement }) | null {
    const interacted = this.interacted();
    if (interacted !== null) {
      const box = this.scrollerElement();
      if (interacted.via !== "focus" || box === null) return interacted;
      const row = this.rowToKeep(interacted.row, box.getBoundingClientRect());
      if (row === interacted.row) return interacted;
      return row === null ? null : { id: this.idOf(row), row, via: "view" };
    }
    const kept = this.hostKept();
    if (kept !== null) return { id: this.idOf(kept), row: kept, via: "kept" };
    const reader = this.readerKept();
    if (reader !== undefined) return reader;
    const root = this.root;
    if (root === null) return null;
    const scroller = this.scrollerElement();
    if ((scroller === null ? window.scrollY : scroller.scrollTop) <= 0) return null;
    const row = rowAtCenter(root, `[${this.props.item}]`, scroller);
    return row === null ? null : { id: this.idOf(row), row, via: "center" };
  }

  /** Where the host takes its box over and no act of the reader's names a row now, the row of the
   *  place their last act recorded, which the events the page raises since have not changed: the
   *  pointer's row moved from under it, the focused control gone with the node that held it. The
   *  first row in view stands in for it while it lies wholly outside the view (`rowToKeep`), and
   *  null answers that no row is in view. Undefined where no such place is kept, and for any other
   *  host, whose commits keep the centre then. */
  private readerKept(): (AnchorRow & { row: HTMLElement }) | null | undefined {
    const held = this.held;
    const box = this.scrollerElement();
    if (this.props.ownsScrollAnchoring !== true || held === null || held.hostRow !== null) {
      return undefined;
    }
    if (box === null) return undefined;
    const row = this.rowById(held.id);
    if (row === null) return undefined;
    const keep = this.rowToKeep(row, box.getBoundingClientRect());
    if (keep === null) return null;
    return { id: this.idOf(keep), row: keep, via: keep === row ? "kept" : "view" };
  }

  /** Keeps the place of `row`, a row of this list, for the host, while no act of the reader's
   *  names a place: a control of the host's outside the list holds the focus, and what it
   *  starts changes the list around it, as a page of older rows read in under the list does.
   *  The row's place is recorded as a reader's would be (`held`), with the box's own scroll
   *  anchoring off meanwhile where the host takes the box over (`keepPlace`), and `current()`
   *  chooses the row after the reader's focus and pointer and before the centre. The place goes
   *  at `keepRow(null)`, which leaves a place the reader's own act recorded alone; when an act of
   *  the reader's records another place (`rememberHeld`): the focus moving into the root, the
   *  pointer moving onto a row or off one, a scroll they made; and when the row leaves the root.
   *  The browser pulling the box back to its end is no act of the reader's, and the place stays
   *  the host's, recorded again where the pull-back left the box (`viewScrolled`); where the host
   *  takes its box over, so does any scroll that is not the reader's (`rebaseHeld`). While the
   *  reader's hand is on a row (`interacted()`: the focus within one, or the pointer resting on
   *  one), it keeps nothing and leaves the place as it is: the reader's act names the place, its
   *  kept top and a focused control's own place with it, and a host's keep never takes its place.
   *  Anything that is not a row of this list lets the host's place go, as null does. Where the
   *  rows scroll in the document nothing is kept, as nothing is for the reader there. */
  keepRow(row: HTMLElement | null): void {
    const root = this.root;
    const scroller = this.scrollerElement();
    if (
      row === null ||
      root === null ||
      scroller === null ||
      rowWithin(root, `[${this.props.item}]`, row) !== row
    ) {
      if (this.held !== null && this.held.hostRow !== null) this.keepPlace(null);
      return;
    }
    if (this.interacted() !== null) return;
    this.keepPlace({
      focus: null,
      group: this.groupOf(row),
      hostRow: row,
      id: this.idOf(row),
      scrollTop: scroller.scrollTop,
      top: row.getBoundingClientRect().top,
    });
  }

  /** The row the host keeps the place of (`keepRow`), while that place is kept and the row is
   *  still in the root; null otherwise. */
  private hostKept(): HTMLElement | null {
    const held = this.held;
    return held === null || held.hostRow === null ? null : this.heldRow(held);
  }

  private idOf(row: HTMLElement): string {
    const id = row.getAttribute(this.props.item);
    if (id === null) {
      throw new Error(`viewport anchor row is missing its ${this.props.item} value`);
    }
    return id;
  }

  getSnapshotBeforeUpdate(): Snapshot | null {
    const { enabled } = this.props;
    if (enabled !== undefined && !enabled()) return null;
    const current = this.current();
    if (current === null) return null;
    const at = this.groupOf(current.row);
    // Where the reader last saw the row they have their hand on, while that place still holds:
    // a change no commit of this component's saw may already have moved it (`held`), and
    // measuring again there would take that drift for the place to keep.
    const held = this.heldTop(current.id, at);
    return {
      group: at,
      id: current.id,
      top: held ?? current.row.getBoundingClientRect().top,
      via: current.via,
    };
  }

  componentDidUpdate(
    _previousProps: Readonly<ViewportAnchorProps>,
    _previousState: Readonly<object>,
    snapshot: Snapshot | null
  ): void {
    // A place the host keeps goes with its row: a commit that took the row out of the root
    // ends it, and a box taken over gets its own scroll anchoring back (`keepPlace`).
    if (this.held !== null && this.held.hostRow !== null && this.hostKept() === null) {
      this.keepPlace(null);
    }
    if (snapshot === null) return;
    const element = this.rowById(snapshot.id);
    if (element === null) return;
    const { group } = this.props;
    if (group !== undefined && element.getAttribute(group) !== snapshot.group) return;
    const delta = element.getBoundingClientRect().top - snapshot.top;
    if (delta === 0) return;
    this.scrollBy(delta);
    this.restampHeld(snapshot.id, snapshot.top);
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
