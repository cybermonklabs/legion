import { Component, type ReactNode, type RefObject } from "react";

import { CONTROL, controlIn } from "../../components/control-names";
import { pressArmed, pressMadeIn, scrollToNearest } from "../../components/focus-place";
import { refPreviewCardContains, refPreviewOpenedIn } from "../refs/ref-preview-card";
import { PANEL_ATTRIBUTE } from "./keymap";

export interface AnchorRow {
  id: string;
  /** How the row was chosen: keyboard focus within it, the pointer resting on it, the place kept
   *  (`kept`: the host's, `keepRow`, or, where the host takes its box over, the one the reader's
   *  last act recorded), or, failing all three, the row nearest the list's visible centre. Where
   *  the row so chosen lies wholly outside the view of the box the host named, and the rule of
   *  `rowToKeep` applies to it (`keptOnlyInView`), the row that rule gives stands in for it
   *  (`view`). */
  via: "center" | "focus" | "kept" | "pointer" | "view";
}

/** An element right before or right after a row a press of the reader's is taking away, where
 *  that element is no row - a group's heading, a band's **Show more** - and how far the current
 *  row moves up once that element has gone too, read as the row's own `by` is (`beside`). */
interface Beside {
  by: number;
  element: Element;
}

/** A row above the current one that a press of the reader's is taking away (`leaving`), as the
 *  commit's snapshot reads it, immediately before the row goes. */
interface Leaving {
  /** How far the current row moves up once this row has gone from above it: the whole space the
   *  row takes in the list (`spaceTaken`), the part above the view's top included. */
  by: number;
  id: string;
  /** The element right before the row where that element is no row (a group's heading). Null
   *  where a row, or nothing, stands right before the row, and where that element is the `trail`
   *  of the row named before this one. A group's heading goes with the last row of its group. */
  lead: Beside | null;
  /** The element right after the row where that element is no row (a band's **Show more**).
   *  Null where a row, or nothing, stands right after the row. A band's **Show more** goes with
   *  the rows its fold takes away. */
  trail: Beside | null;
}

/** The list the current row stands in and every element in it, immediately before a commit in
 *  which the host names rows a press of the reader's brings in (`arriving`): once the commit is
 *  in, what tells an element that came in with those rows from one that stood there. */
interface ListBefore {
  elements: ReadonlySet<Element>;
  list: Element;
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
  /** The current row's list as it stands immediately before the commit, where the host names rows
   *  a press brings in (`arriving`); null otherwise, and where the row stands in no element. */
  listBefore: ListBefore | null;
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

/** Where the control that holds the focus in a row stands against the reader's sight (`View`). */
interface FocusSight {
  focused: Element;
  /** Whether the reader can see a part of it: an edge that only touches the view's edge counts
   *  as in sight. */
  meets: boolean;
  /** Whether the reader can see the whole of it. */
  whole: boolean;
}

/** The space `element` takes in its list, in pixels: from its own top to the top of the element
 *  after it, the gap between them included, else to its own bottom. What a row a press of the
 *  reader's takes away frees for the rows after it (`ViewportAnchor`'s `leaving`), and what a row
 *  such a press brings in takes from them (`arriving`). */
function spaceTaken(element: Element): number {
  const box = element.getBoundingClientRect();
  const after = element.nextElementSibling;
  return (after === null ? box.bottom : after.getBoundingClientRect().top) - box.top;
}

/** `element`, right before or right after a row leaving, with the space it takes (`Beside`); null
 *  where there is no such element or it is a row itself (`selector`). */
function beside(element: Element | null, selector: string): Beside | null {
  return element === null || element.matches(selector)
    ? null
    : { by: spaceTaken(element), element };
}

/** How far `side` lets the current row move up now the commit is in: its `by` where it went with
 *  the row beside it, else none. */
function freedBy(side: Beside | null): number {
  return side !== null && !side.element.isConnected ? side.by : 0;
}

/** The reader's place as `held` keeps it, or the place a host keeps (`keepRow`). */
interface HeldPlace {
  group: string | null;
  /** The row the host keeps the place of; null for a place the reader's own act recorded. */
  hostRow: HTMLElement | null;
  id: string;
  /** The element inside the row whose own place is kept with it, and its viewport top then; null
   *  where the row's top is. Which element keeps the place, and for how long, is the rule
   *  `keptIn` states. */
  inner: { element: Element; top: number } | null;
  scrollTop: number;
  top: number;
}

/** What names a place for `keepPlace` to keep: the row the reader's hand, the host or the place
 *  kept until now is at, or the row the reader reads by keys (`listReadAt`); what named that row
 *  (`AnchorRow`'s `via`), which says whether the rule of `rowToKeep` applies to it
 *  (`keptOnlyInView`); the element inside that row whose own place is kept with it, where one
 *  is named: the control that holds the focus there, or the element the host names; and whether
 *  the place is the host's (`keepRow`). */
interface NamedPlace {
  host: boolean;
  inner: Element | null;
  row: HTMLElement;
  via: AnchorRow["via"];
}

export interface ViewportAnchorProps {
  /** Whether a press of the reader's is bringing row `id` into the list: a band's unfold, which
   *  draws the rows its fold held back. The mirror of `leaving`: the anchor does not hold the
   *  current row in place against the coming of such a row above it, nor of an element that is
   *  no row and comes into the list right after it (the band's **Show more**), so the current row
   *  and the rows after it move down by the whole space they take, and the view does not scroll.
   *  It is asked once the commit is in, of the render that drew the rows. The host names only the
   *  rows that press brings in: never a row the list drew before the commit, nor one a live
   *  update draws in the same commit. With no function, no row is named, and no row is read for
   *  it. */
  arriving?: (id: string) => boolean;
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
  /** While a place is kept in the box `scroller` names, this anchor alone keeps that box's
   *  scroll (`keepPlace`): it turns the browser's own scroll anchoring off there, and keeps the
   *  place as the browser's anchoring would, for any change of height in the box. It watches the
   *  box's content around the place kept change height, at every depth, as it watches the root
   *  (`watchedIn`), and makes good a change no observation has reported yet before it reads a
   *  pointer, focus or scroll event (`contentResized`). The place kept then changes only at an
   *  act of the reader's: a scroll that a wheel, a key or a press made (`readerScrolls`), the
   *  pointer moving to a new point (`handMoved`, `pointerMoved`), the focus moving into the rows;
   *  and where a scroll of the page's leaves it out of the view, as the rule `rowToKeep` states,
   *  which every row named goes through here, whatever named it (`keptOnlyInView`). What the
   *  browser raises for a change of the page records nothing else: a row moving under a pointer
   *  that did not move, a pull-back of the box to its end, the browser's own anchoring, another
   *  script's scroll (`rebaseHeld`), the focus lost with a node that went (`readerKept`). While
   *  another keeper of a place claims the box (`claimBox`), this anchor keeps no place there and
   *  moves the box for nothing, and the box's own scroll anchoring is back on, until an act of the
   *  reader's ends the claim, as `takeBox` states, and the place it names is kept. Read once, as
   *  the anchor is built (`ownsBox`). Off by default, and of no effect where the rows scroll in
   *  the document. Left off, the browser keeps anchoring the box on a node of its own near the
   *  top of the view, which holds the rows in view as content between the kept row and the view
   *  changes height. Details' history turns it on: the place it keeps is often a control the page
   *  handed the focus to far down a long turn, and the browser's move of the box for its own node
   *  took that control out of the view. */
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

/** Where the focus is, as a row reads it: the focused element, or, while the focus is in a panel
 *  a control opened (`AnchoredPanel`, a `MultiSelect` list's), the control that names that panel
 *  (`aria-controls`). The panel is portalled out of the list, so without this a row whose list is
 *  open would hold no focus while the reader picks from it. */
function focusedControl(): Element | null {
  const active = document.activeElement;
  const panel = active?.closest(`[${PANEL_ATTRIBUTE}]`);
  if (panel == null || panel.id === "") return active;
  return document.querySelector(`[aria-controls~="${CSS.escape(panel.id)}"]`) ?? active;
}

/** True where `rect` and `box` share some height: what `rect` bounds lies in the view `box`
 *  bounds, wholly or in part. The one rule for "in view" that this anchor and Details' landing
 *  hold (`details-more.tsx`) each keep a place by. */
export function overlaps(rect: Pick<DOMRect, "bottom" | "top">, box: DOMRect): boolean {
  return rect.bottom > box.top && rect.top < box.bottom;
}

/** True where `element`, an element inside `row` other than the row itself, can keep the place
 *  at its own top (`held`), the one rule every element kept with a place is kept by: the element
 *  a host names inside the row it keeps (`keepRow`, `host`) does while it stays inside `row`,
 *  whether or not it holds the focus, and a control named by the reader's act does while it
 *  stays inside `row` and holds the focus. An element that left the row, a control that lost the
 *  focus, and the row itself keep no place of their own, and the row's top is kept instead. */
function keptIn(element: Element, row: HTMLElement, host: boolean): boolean {
  return element !== row && (host || element === document.activeElement) && row.contains(element);
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
 * key landed the focus at (`details-more.tsx`): the history's anchor and the hold both scroll
 * Details' box, each while the other keeps no place there (`claimBox`), and the event of one's
 * scroll can reach the other in the frame after it. Each tells its own scroll by a mark of its
 * own, built by this one rule.
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

/** A claim of a box by another keeper of a place (`claimBox`): what ends that keeper's hold. */
interface BoxClaim {
  end: () => void;
}

/** The boxes another keeper of a place claims now (`claimBox`), each with its one claim. */
const claimedBoxes = new Map<Element, BoxClaim>();

/** What each mounted anchor is told as a box is claimed (`claimBox`), so it lets its place go
 *  where that box is the one it keeps a place in. */
const claimHearers = new Set<(box: Element) => void>();

/**
 * Claims `box`, the box that scrolls an anchor's rows, for another keeper of a place in it while
 * that keeper holds its place there: Details' hold of the place a key or an address landed the
 * focus at (`details-more.tsx`), in the box the history's anchor keeps a place in too. One keeper
 * keeps a box at a time, so no change of height is made good toward two places, and a box holds
 * one claim at a time: a hold claims its box once while it keeps a place, and Details ends a
 * running hold before it starts the next (`useLandedPlaceHold`). Each anchor whose box it is lets
 * its place go as the claim starts, which gives the box its own scroll anchoring back
 * (`keepPlace`), and while the claim lasts it records no place and moves the box for nothing,
 * neither for a change of height (`contentResized`) nor at a commit (`componentDidUpdate`). The
 * anchor ends the claim as `takeBox` states: it calls `end`, which ends the keeper's hold, and
 * keeps the place the reader's act names. Once the claim ends, the anchor keeps places again by
 * its own rules, from the next one it records: an act of the reader's, the host's `keepRow`, a
 * scroll. Returns what ends the claim alone, the keeper's hold left running; calling it again, or
 * once the claim has ended, does nothing.
 */
export function claimBox(box: Element, end: () => void): () => void {
  const claim: BoxClaim = { end };
  claimedBoxes.set(box, claim);
  for (const hear of [...claimHearers]) hear(box);
  return () => {
    if (claimedBoxes.get(box) === claim) claimedBoxes.delete(box);
  };
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
 * doing and sits outside the list only because it is `fixed`. Where this component scrolls to
 * keep the row's place, a control that holds the focus in that row and stood whole in the
 * reader's sight just before the scroll is brought whole into sight again at its nearest edge:
 * the scroll keeps the row's place, not the focus's (`putPlaceBack`).
 *
 * Two shifts are not compensated, each the doing of a press of the reader's. One is a row that the
 * press takes away from above the current row (`leaving`: for example a Dismiss, or a snooze that
 * moves its rows into another group), with the heading of a group its going empties and an
 * element after it that goes with it (a band's **Show more**, which its fold takes away with its
 * rows). The reader asked for that row to go, so the current row and the rows after it move up by
 * the whole space the row took, the part above the view's top included: the view is not scrolled
 * to hold them. The other is its mirror: a row that the press brings into the list above the
 * current row (`arriving`: a band's unfold, which draws the rows its fold held back), with an
 * element after it that is no row and comes into the list with it (that band's **Show more**).
 * The reader asked for that row to come, so the current row and the rows after it move down by
 * the whole space the row takes, the part above the view's top included, and the control they
 * pressed stays where it stood. Measured in the Inbox at 390x664, with the window at 30 px and no
 * row in hand, where the anchor made up for such rows: the unfold of a band of 40 rows above the
 * row nearest the list's centre, by Enter on its toggle or a click on it, scrolled the window by
 * 3,600 px to hold that row, and left the toggle, and the focus on it, at -3,329 px, in Chromium,
 * Firefox and WebKit.
 * Every other change of height above the current row in the same commit, growing or shrinking, is
 * compensated, a row a live update draws there among them, so the place the row left, and the
 * control the press was made on, stay where they stood on screen: the Inbox's **Mark all read**
 * goes with the last unread rows, and below `sm` the header loses the line it stood on, so the
 * view scrolls up by that line.
 * Where a press takes the current row itself away, as a thread's own Dismiss does, the row that
 * keeps its place once it has gone (`standInFor`) is held the same way: the row after it, which
 * moves up into its place, else the row before it. A current row that holds the focus and is left
 * partly in sight is then brought whole into sight at its nearest edge, and a focus left wholly
 * out of sight is not scrolled to: while such rows are leaving, the selection bar's hand-off parks
 * the focus, and it lands only once they have gone and the foot's pass has run, by the least
 * scroll of its own (`useLastMarkedPlace`'s `parkOrLand` and `landAfterFootPass`). The browser's
 * own scroll anchoring is turned off on the box that scrolls the rows in every commit in which a
 * row above the current one is named as leaving, and in every commit in which the current row
 * itself is named: a thread's own Dismiss takes away the row the reader's hand is on, and the
 * browser holds the view against its going as much as against a row's above. The snapshot taken
 * before the commit cannot tell whether that row goes in it, and the browser would hold the view
 * against that going. It is turned back on two frames later, once a rendering update has run
 * with it off (`releaseAfterRendering`), or at the next commit no press takes rows away in. Where
 * the window scrolls the rows and moved in a commit it was off for all the same, it is put back
 * where the press found it (`putPlaceBack`). A row that goes in the same commit without the
 * reader's press, by a live update or another device, is compensated as any other shift is, so
 * the rows in view do not jump under it.
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
 * The anchor keeps only a place the reader can see, by the one rule `rowToKeep` states, where
 * the rows scroll in a box the host names. A host that takes that box over
 * (`ownsScrollAnchoring`) puts every place it keeps and every row a commit keeps through that
 * rule, whatever named the row; any other host only the row the focus is in, and it keeps the row
 * the pointer rests on, or a row it keeps itself (`keepRow`), wherever a scroll has left it
 * (`keptOnlyInView`). Where the rows scroll in the document the row named is kept wherever it is.
 *
 * A host can keep a row's place itself while no act of the reader's names one (`keepRow`): a
 * control of the host's outside the list holds the focus, and what it starts changes the list
 * around that control; or the host scrolled the box to an element an address names, and keeps
 * that element's own place inside its row. The row it keeps comes after the reader's focus and
 * pointer and before the centre, and the place goes when the host lets it go, when an act of the
 * reader's records another one, and when the row leaves the list.
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
 * pointer moving to a new point, the focus moving into the rows. Where no hand is on a row and
 * the focus is on an element of the box that holds the whole list, such an act records the row
 * nearest the middle of the view, the one a commit keeps then (`listReadAt`). A place a scroll of
 * the page's leaves out of the view is kept as the rule `rowToKeep` states. The events the
 * browser raises for a change of the page record nothing else, and while no act of the reader's
 * names a row, the place kept is the one a commit keeps too.
 *
 * Another keeper of a place in the box the host named can claim it while it holds its place
 * there (`claimBox`): Details' hold of the place a key or an address landed the focus at
 * (`details-more.tsx`). One keeper keeps a box at a time: the anchor lets its place go as the
 * claim starts, and while it lasts keeps no place there and moves the box for nothing, at a change
 * of height or at a commit, so no change is made good toward two places. An act of the reader's
 * takes the box back, as `takeBox` states, and the anchor keeps the place that act names from
 * then on. Once the claim ends otherwise it keeps places again by its own rules, from the next
 * one it records.
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
  /** The row the reader's hand is on, as `keepPlace` keeps it; the viewport top they last saw it
   *  at; and the two things that keep that top true: the offset the box was at when it was
   *  saved, and the group the row was in. The top is read rather than re-measured because the
   *  view can be wrong when a commit arrives: a change this component's own commits never see has
   *  already moved the row. A card below the hand answering itself, a thread body arriving,
   *  Markdown finishing - each is a child's own commit, which re-renders the child alone, so
   *  `getSnapshotBeforeUpdate` is never called for it. Measured in the Inbox drawer: the answered
   *  row's card shrinks by a card's height one commit before the list drops the row, and the
   *  reader's row ended exactly that far out.
   *
   *  Kept only where the host names a `scroller`. Where the rows scroll in the document the
   *  browser's own scroll anchoring covers that drift, and a cached place there would only undo
   *  a scroll whose event has not arrived yet. In a named box `contentResized` stands in for it:
   *  beside the browser's own anchoring there, which keeps a node of its own choosing rather than
   *  the reader's row, or alone where the host takes the box over (`ownsScrollAnchoring`).
   *
   *  A place the host keeps (`keepRow`) is kept the same way, by the same rule, and is marked as
   *  the host's (`hostRow`), so letting it go leaves a place of the reader's alone.
   *
   *  Where an element inside the row can keep the place (`keptIn`), a focused control or the one
   *  the host names inside the row it keeps (`keepRow`), as the history names the turn an address
   *  lands at, and lies in the view as the place is recorded, the place is that element's own
   *  (`inner`): the element and its viewport top then. A long row whose head is above the view is
   *  read at that element, and its own words above the element can change height, which moves the
   *  element and leaves the row's top where it was. `top` is then the row's top that puts the
   *  element back at its own (`contentResized` keeps the two together), so a commit, which measures
   *  the row, moves the element back with it. An element outside the view is kept no more than a
   *  row outside it is (`rowToKeep`). A press of the reader's inside the root, a key or the
   *  pointer, lets the element go (`readerPressed`): what the press opens in the row opens below
   *  the row's top, which is what holds then.
   *
   *  A different `scrollTop` means the view moved without this component hearing of it, unless
   *  the box sits at its end below the offset kept, where the browser pulled it back as its
   *  content shrank and the reader did not move the view (`pulledBack`). A different `group`
   *  means the row was relocated rather than shifted. On either the top no longer says where the
   *  place is, and the commit measures the row instead. Where the host takes its box over, a
   *  scroll that is not the reader's leaves the place kept, moves its top to where that scroll
   *  left it, and keeps it again by the rule (`rebaseHeld`), so the place holds at the box's new
   *  offset as far as the reader can still see it. */
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
   *  host takes the box over (`ownsBox`). */
  private readerActedAt: number | null = null;
  /** Whether the reader has acted in a way that scrolls the box (`readerActed`) since another
   *  keeper last claimed the box the host named (`claimHeard`). While a claim stands, a scroll
   *  is the reader's (`readerScrolls`) only where such an act made it (`ownedScrolled`): the key
   *  that started the claimant's landing, as `t v` starts Details' hold, came before the claim,
   *  and the landing's own scroll, in the frames after that key, is the page's. */
  private actedSinceClaim = false;
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
  /** The boxes whose own scroll anchoring is turned off for a press's commit, each with the
   *  inline `overflow-anchor` it had before (`holdNativeAnchoring`). Empty while it is on. */
  private nativeAnchoring: { box: HTMLElement; priority: string; value: string }[] = [];
  /** The frame request that gives the box its scroll anchoring back once a rendering update has
   *  run with it off (`releaseAfterRendering`). Null while none is pending. */
  private releaseFrame: number | null = null;

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
   *  built. It is the one value every branch that differs for such a host reads: whether this
   *  anchor hears the pointer's own moves and the reader's acts that scroll on the window
   *  (`pointerMoved`, `readerActed`), turns the box's own scroll anchoring off (`keepPlace`),
   *  keeps every row named only while the reader can see it (`keptOnlyInView`), and takes the box
   *  back from another keeper's claim (`takeBox`). Read once, the listeners removed as it
   *  unmounts are the ones added as it mounted, and every branch agrees, whatever the host passes
   *  in between. */
  private readonly ownsBox: boolean;

  constructor(props: ViewportAnchorProps) {
    super(props);
    this.ownsBox = props.ownsScrollAnchoring === true;
  }

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

  /** True while another keeper of a place claims the box the host named (`claimBox`): this
   *  anchor then keeps no place there and moves the box for nothing. */
  private boxClaimed(): boolean {
    const box = this.scrollerElement();
    return box !== null && claimedBoxes.has(box);
  }

  /** Heard as another keeper claims `box` (`claimBox`): where it is the box the host named, the
   *  place kept there goes, the box gets its own scroll anchoring back (`keepPlace`), and no act
   *  of the reader's has come since the claim began (`actedSinceClaim`). */
  private readonly claimHeard = (box: Element): void => {
    if (box !== this.scrollerElement()) return;
    this.actedSinceClaim = false;
    this.keepPlace(null);
  };

  /** Ends another keeper's claim of the box the host named (`claimBox`), where the host takes
   *  its box over and a claim stands: the claim goes, the keeper's hold ends with it (`end`), and
   *  this anchor keeps the place the reader's act names from then on. This is the one statement
   *  of what ends a claim. The reader's first act that names a row of this list as their place
   *  ends it (`rememberHeld`): the pointer moving onto a row (`handMoved`); the focus moving into
   *  a row, whoever moves it; a scroll of the reader's while the pointer rests on a row or the
   *  focus is in one. Where the focus is on an element of the box that holds the whole list, as
   *  Details' history section, which `t v` focuses, holds its entries, such an act names the row
   *  in the middle of the view where no row is under the pointer (`listReadAt`), and a scroll of
   *  the reader's ends the claim even where it names no row (`ownedScrolled`). While a claim
   *  stands, a scroll is the reader's (`readerScrolls`) only where the act of theirs that made
   *  it, a turn of the wheel, a key or a press, came after the claim began (`actedSinceClaim`).
   *  Nothing else ends a claim here: neither an act that names no row, nor a scroll of the
   *  page's, nor a commit. The keeper can end its claim itself, its hold left running, by what
   *  `claimBox` returns. */
  private takeBox(): void {
    const box = this.scrollerElement();
    if (!this.ownsBox || box === null) return;
    const claim = claimedBoxes.get(box);
    if (claim === undefined) return;
    claimedBoxes.delete(box);
    claim.end();
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

  /** The element kept with the place `held` (`inner`) while it still keeps the place inside
   *  `row`, the place's row, as the rule `keptIn` states; null otherwise. */
  private keptInner(held: HeldPlace, row: HTMLElement): HeldPlace["inner"] {
    const inner = held.inner;
    return inner !== null && keptIn(inner.element, row, held.hostRow !== null) ? inner : null;
  }

  /** Keeps the place `named` names, the reader's or the host's (`keepRow`), as `held`, or none:
   *  every place kept goes through here, and the place kept is the one `placeToKeep` gives,
   *  measured as it stands now: the row's top, the box's offset, the row's group, and the
   *  element named inside that row (`keptIn`) while it lies in the view. Where the rule
   *  puts another row in the named one's stead, the place a host kept stays the host's, at that
   *  row (`hostRow`). Where no row is in view, where the rows scroll in the document, and while
   *  another keeper claims the box (`claimBox`), nothing is kept. Where the host takes the box it
   *  named over (`ownsScrollAnchoring`), this anchor alone anchors that box while a place is kept
   *  there, and the browser's own scroll anchoring is turned off there (`overflow-anchor: none`)
   *  until none is kept. The browser anchors on a node of its own choosing near the top of the
   *  view, not on the kept row, so content between the two that changes height moves the row all
   *  the same, and the browser's move of the box for that node reads as a scroll this anchor did
   *  not make: where the frame observes the change after that move, the place no longer holds and
   *  nothing is put back, and where the move's scroll event comes first, the place is recorded
   *  where the change left the row. Measured in Details: the page's typeface arriving after Load
   *  older landed moved the box 39,180 px, and the control the page had handed the focus to
   *  1,492 px up, out of the view. Since the browser then keeps the view for no change in that
   *  box, this anchor watches the box's content around the place kept for one, at every depth, as
   *  it watches its own root (`watchedIn`). Any other host keeps the browser's anchoring, which
   *  holds the rows in view as content above the view changes height, as the Inbox's columns do. */
  private keepPlace(named: NamedPlace | null): void {
    const place = named === null || this.boxClaimed() ? null : this.placeToKeep(named);
    this.held = place;
    const box = this.ownsBox ? this.scrollerElement() : null;
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
    if (row !== null) this.watchOwnedBox(this.keptInner(place, row)?.element ?? row);
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
   *  `from` as it is now (`watchedIn`): the kept row, or the element kept in it. Content
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

  /** The place `keepPlace` keeps for `named`, measured as it stands now: at the row named, or,
   *  where the rule `rowToKeep` applies to that row (`keptOnlyInView`), at the row the rule gives.
   *  Null where that rule finds no row in view, and where the rows scroll in the document. */
  private placeToKeep({ host, inner: within, row: named, via }: NamedPlace): HeldPlace | null {
    const scroller = this.scrollerElement();
    if (scroller === null) return null;
    const box = scroller.getBoundingClientRect();
    const row = this.keptOnlyInView(via) ? this.rowToKeep(named, box) : named;
    if (row === null) return null;
    let inner: HeldPlace["inner"] = null;
    if (within !== null && keptIn(within, row, host)) {
      const at = within.getBoundingClientRect();
      if (overlaps(at, box)) inner = { element: within, top: at.top };
    }
    return {
      group: this.groupOf(row),
      hostRow: host ? row : null,
      id: this.idOf(row),
      inner,
      scrollTop: scroller.scrollTop,
      top: row.getBoundingClientRect().top,
    };
  }

  /**
   * The row to keep where a place is named at `place`, a row of this list, and the rows scroll in
   * a box whose view `box` bounds. This is the one rule a place is kept by wherever it applies
   * (`keptOnlyInView`): the anchor keeps only a place the reader can see, whatever named it, the
   * focus in a row, the focus outside the list, the pointer, the host (`keepRow`) or the place
   * kept until now, where the host takes its box over; the focus in a row, for any other host.
   * The row is `place` while any of it lies in the view; else the first row wholly in view, or,
   * where none is, the first in view in part; null where no row is in view, and then no place is
   * kept. An element kept with the place, a control that holds the focus or the host's, is kept
   * only while it is inside the row kept and any of it lies in the view; the place replaced by
   * another row keeps none.
   *
   * A row wholly outside the view is one the reader cannot see, and so is a cut of it, or a row
   * drawn between it and the view: keeping the first row in view then moves nothing on screen,
   * where keeping that row, or an element in it, moved every row in view by the change. Every
   * place kept goes through it where it applies (`placeToKeep`), and so does every row a commit
   * keeps (`current()`). It reads the layout, so `interacted()`, which a parent may call on every
   * render, leaves it to its callers.
   */
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

  /** True where a row that `via` names is kept only while the reader can see it, by the rule
   *  `rowToKeep` states: every row, whatever named it, where the host takes its box over
   *  (`ownsBox`); only the row the focus is in, for any other host, which keeps the row the
   *  pointer rests on, or a row it keeps itself (`keepRow`), wherever a scroll has left it. */
  private keptOnlyInView(via: AnchorRow["via"]): boolean {
    return this.ownsBox || via === "focus";
  }

  /** Records the hand's row, and, where a control inside it holds the focus, that control, as
   *  the place to keep, with the box offset and the group that place belongs to (`keepPlace`,
   *  `placeToKeep`); where no hand is on a row, the row the reader reads by keys (`listReadAt`).
   *  Called where the view is the reader's own: the pointer moving onto a row or off one, a
   *  scroll they made, the focus moving into the root, whoever moved it, and, where the host
   *  leaves the box's anchoring on, any other scroll of the box, the browser pulling it back to
   *  its end under a place of theirs included (`viewScrolled`). What it records, or nothing where
   *  neither names a row, takes the place of one the host kept (`keepRow`): the reader's own act
   *  says where they are now. A row it names while another keeper claims the box takes the box
   *  back first (`takeBox`). A host whose rows scroll in the document keeps nothing. */
  private readonly rememberHeld = (): void => {
    const current = this.interacted() ?? this.listReadAt();
    if (current !== null) this.takeBox();
    const control = current?.via === "focus" ? document.activeElement : null;
    this.keepPlace(
      current === null ? null : { host: false, inner: control, row: current.row, via: current.via }
    );
  };

  /** Keeps the place kept again where the box now stands (`keepPlace`): its top moves to where
   *  the box's move left it, and a place the reader can no longer see is kept as the rule
   *  `rowToKeep` states. Called where the host takes its box over and a scroll of the box that no
   *  act of the reader's made, another script's or the browser's pulling it back to its end, has
   *  moved the view without the reader choosing another place (`ownedScrolled`), and where a box
   *  at its end or its top could not follow a put-back the whole way (`contentResized`). The row
   *  is the one kept, the host's or the reader's, whatever the pointer rests on now, and a host's
   *  place stays the host's. An element kept with it, a focused control or the host's, stays
   *  only while the rule keeps it, and one the reader's press let go stays let go
   *  (`readerPressed`). A row that has left the list takes the place with it. */
  private rebaseHeld(): void {
    const held = this.held;
    if (held === null) return;
    const row = this.heldRow(held);
    this.keepPlace(
      row === null
        ? null
        : { host: held.hostRow !== null, inner: held.inner?.element ?? null, row, via: "kept" }
    );
  }

  /** The kept top of the row `id` (with an element kept in it, the row's top that puts that
   *  element back at its own: `held`), while it is still the place the reader saw: the box
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

  /** Called once a compensation has put `id` at `to` from the place it was kept at, `from`. `to`
   *  is `from` save where rows a press of the reader's took away let the row move up
   *  (`leaving`), or rows such a press brought in moved it down (`arriving`). The row is at its
   *  kept place again, so that place belongs to the offset the compensation left behind. Without
   *  this the first compensation would invalidate the kept place for every later one, and the
   *  next commit would put a row the press moved back where it stood before. An element kept in
   *  the row keeps its own top: `from` and `to` are the row's tops that put it there, so it moves
   *  by as far as the row does. */
  private restampHeld(id: string, from: number, to: number): void {
    const held = this.held;
    const scroller = this.scrollerElement();
    if (held === null || scroller === null || held.id !== id || held.top !== from) return;
    held.scrollTop = scroller.scrollTop;
    held.top = to;
    if (held.inner !== null) held.inner.top += to - from;
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
    if (this.ownsBox) {
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
   *  its box over (`readerScrolls`), and since any claim of the box that stands began
   *  (`actedSinceClaim`). */
  private readonly readerActed = (event: Event): void => {
    if (event.type === "pointerdown") this.readerPressing = true;
    else if (event.type === "pointerup" || event.type === "pointercancel") {
      this.readerPressing = false;
    }
    this.readerActedAt = performance.now();
    this.actedSinceClaim = true;
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

  /** Moves the box the host named by `delta`, a scroll the host makes of that box itself, as the
   *  history brings the control a press of its Show more gives the focus to into Details' view,
   *  and marks the offset that left the box at as this anchor's own (`ownScroll`), as every
   *  scroll of the anchor's own is: its event is no scroll of the reader's, and records no place.
   *  Where the rows scroll in the document it moves nothing, since a host scrolls its own box and
   *  never the page. */
  scrollBox(delta: number): void {
    if (this.scrollerElement() !== null) this.scrollBy(delta);
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
    if (scroller !== null && this.ownsBox) {
      this.ownedScrolled(scroller);
      return;
    }
    // The browser pulling the box back to its end after its content shrank is no scroll of the
    // reader's. Keeping the row's place here would keep it where the shrink moved it, so it goes
    // back where the reader last saw it, as on any other change of the content's height. The
    // place is then recorded as after any other scroll, at the offset the box is left at: the
    // row back where it was, or, where the box's end left no room to put it back, where the
    // pull-back moved it, which is where the reader now sees it. A place the host keeps is
    // recorded there too and stays the host's, with the element it names inside its row: a
    // pull-back is no act of the reader's to end it.
    const held = this.held;
    if (held !== null && scroller !== null && this.pulledBack(held.scrollTop, scroller)) {
      this.contentResized();
      const hostRow = this.held?.hostRow ?? null;
      if (hostRow !== null) {
        this.keepRow(hostRow, this.held?.inner?.element);
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
   *  (`readerScrolls`), at the row the pointer rests on now and the focus, or the row they read
   *  by keys (`listReadAt`). While another keeper claims the box, which scroll is theirs, and
   *  which takes the box back, is as `takeBox` states (`actedSinceClaim`). A scroll that left the
   *  box where the place holds moved nothing. A pull-back of the box to its end, the browser's
   *  own anchoring and another script's scroll record nothing: the place kept stays where the
   *  scroll left it, as far as the reader can still see it (`rebaseHeld`). Details' hold of the
   *  place a key landed the focus at scrolls the box in its own resize observation
   *  (`details-more.tsx`), and recording the place at that scroll's event took the row the
   *  pointer rested on then, or none, for the reader's. */
  private ownedScrolled(scroller: HTMLElement): void {
    const reader = this.readerScrolls() && (this.actedSinceClaim || !this.boxClaimed());
    this.contentResized();
    const held = this.held;
    if (held !== null && held.scrollTop === scroller.scrollTop) return;
    if (!reader || (held !== null && this.pulledBack(held.scrollTop, scroller))) {
      this.rebaseHeld();
      return;
    }
    if (this.focusHoldsList()) this.takeBox();
    this.hoverAtPointer();
    this.rememberHeld();
  }

  /** The root's content changed height outside a commit of this component's, or, where the host
   *  takes the box over (`ownsScrollAnchoring`), the box's content around the place kept did
   *  while one is kept there (`keepPlace`, `watchedIn`), or an event of the page is about to be
   *  read there, or the browser pulled the box back to its end for such a change
   *  (`viewScrolled`). Puts the place kept back where the reader last saw it, in the box the host
   *  named: the row kept for the reader's hand or for the host (`keepRow`), or, where an element
   *  inside it is kept with the place, a focused control or the host's, that element (`held`).
   *  With no such box the document's own scroll anchoring owns this and nothing here runs. The
   *  kept place is used under the same conditions a commit uses it under, the row's group
   *  included, and goes once its row has left the root.
   *
   *  Where a scroll whose event has not run yet moved the box off the offset the place was kept
   *  at, a host that leaves the box's anchoring on lets that event record the place again. One
   *  that takes the box over keeps it as the browser's anchoring would: a scroll that alone
   *  moved the place, or that left the place wholly outside the view, is left for its event to
   *  read (`ownedScrolled`), which keeps the place again by the rule `rowToKeep` states; one that
   *  put the place back where it was kept, as another script's write in the same frame can,
   *  leaves it holding at the new offset; and a change of height on top of the scroll is made
   *  good by as far as the change moved the place, the scroll kept. A put-back that a box at its
   *  end or its top could not follow the whole way leaves the place where the change left it,
   *  kept again by that rule where its row is then wholly outside the view (`rebaseHeld`). While
   *  another keeper claims the box no place is kept, and nothing runs here (`claimBox`). */
  private readonly contentResized = (): void => {
    const held = this.held;
    if (held === null || this.root === null) return;
    const element = this.heldRow(held);
    if (element === null) {
      this.keepPlace(null);
      return;
    }
    // While the kept element still keeps the place inside the row (`keptInner`), the place is
    // that element's: the row goes where the element is back at its own top, and that is the
    // row's kept top from then on, since the row's own words above the element can change height
    // and leave the row's top where it was.
    const inner = this.keptInner(held, element);
    this.watchOwnedBox(inner?.element ?? element);
    // Nothing is ever held without a box (`keepPlace`), and a row in another group than the one
    // it was kept in was relocated rather than shifted.
    const scroller = this.scrollerElement();
    if (scroller === null || this.groupOf(element) !== held.group) return;
    const offset = scroller.scrollTop;
    const holds = held.scrollTop === offset || this.pulledBack(held.scrollTop, scroller);
    if (!holds && !this.ownsBox) return;
    const at = element.getBoundingClientRect();
    const own = inner === null ? at : inner.element.getBoundingClientRect();
    const keptAt = inner === null ? held.top : inner.top;
    let drift = own.top - keptAt;
    if (!holds) {
      const scrolled = offset - held.scrollTop;
      if (drift !== 0) {
        // The scroll alone moved the place, or left it wholly outside the view, where the reader
        // no longer sees it (`rowToKeep`): nothing moves here, and the scroll's own event keeps
        // the place again (`ownedScrolled`).
        const left = keptAt - scrolled;
        const box = scroller.getBoundingClientRect();
        if (drift === -scrolled || !overlaps({ bottom: left + own.height, top: left }, box)) return;
        drift += scrolled;
        if (inner !== null) inner.top -= scrolled;
      }
      held.scrollTop = offset;
    }
    held.top = at.top - drift;
    if (drift === 0) return;
    this.scrollBy(drift);
    this.restampHeld(held.id, held.top, held.top);
    // Where the host takes the box over, a box at its end or its top that could not follow the
    // whole way leaves the place where the change left it, which is where the reader now sees it:
    // kept at the top it was put back to, the next change would scroll the row back there once
    // the box had room, a move of its own the reader never asked for.
    const short = drift - (scroller.scrollTop - offset);
    if (short === 0 || !this.ownsBox) return;
    held.top += short;
    if (inner !== null) inner.top += short;
    // A row the change left wholly outside the view is no place the reader sees: the place is
    // kept again by the rule `rowToKeep` states.
    if (!overlaps(element.getBoundingClientRect(), scroller.getBoundingClientRect())) {
      this.rebaseHeld();
    }
  };

  componentDidMount(): void {
    // Capture, because `scroll` does not bubble: one listener catches the window's and the
    // named box's alike, and the handler reads a rect only for what scrolls this anchor's rows.
    window.addEventListener("scroll", this.viewScrolled, true);
    // Another keeper's claim of the box the host named lets the place kept there go (`claimBox`).
    claimHearers.add(this.claimHeard);
    // A press that went down in the root can end anywhere on the page, so its end is heard on
    // the window (`pressEnded`).
    window.addEventListener("pointerup", this.pressEnded, true);
    window.addEventListener("pointercancel", this.pressEnded, true);
    // Only a host that takes its box over reads the pointer's own moves and the reader's acts
    // that scroll (`pointerMoved`, `readerScrolls`). Whether it does is read once, as this anchor
    // is built (`ownsBox`), so the listeners added here are the ones removed as it unmounts, even
    // where the host changes `ownsScrollAnchoring` in between.
    if (this.ownsBox) {
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
    claimHearers.delete(this.claimHeard);
    if (this.ownsBox) {
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
    this.releaseNativeAnchoring();
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
    if (this.ownsBox) {
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
    if (this.ownsBox) this.contentResized();
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
    if (this.ownsBox && !moved) {
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
    if (this.ownsBox) this.contentResized();
    const pressed = this.pressedOn;
    this.pressedOn = null;
    this.rememberHeld();
    if (pressed !== null && event.target instanceof Node && event.target.contains(pressed)) {
      this.readerPressed();
    }
  };

  // A key or the pointer pressed inside the root is the reader acting on their row, as "more"
  // drawing the rest of a cut turn's words: what the press opens, opens below the row's top,
  // which stays where it was. The element kept with the place, a focused control or the host's,
  // is let go, so only a change the reader did not make, a cut or a read arriving, keeps an
  // element's own place, until the focus next moves into the root or the host names another.
  // Heard as the event goes down to its target, so a control that stops its own event is
  // pressed all the same.
  private readonly readerPressed = (): void => {
    if (this.held !== null) this.held.inner = null;
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

  /** The row the reader's hand is on: focus within a row, or in a list a control of the row
   *  opened (`focusedControl`), else the pointer resting on one. No layout reads, so a parent may
   *  call it on every render. */
  interacted(): (AnchorRow & { row: HTMLElement }) | null {
    const root = this.root;
    if (root === null) return null;
    const selector = `[${this.props.item}]`;
    const focused = rowWithin(root, selector, focusedControl());
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

  /** The row a commit will anchor to: the row `named()` gives, put through the rule `rowToKeep`
   *  states where the rows scroll in the box the host named and the rule applies to that row
   *  (`keptOnlyInView`; `view` where the rule put another row in its stead, and null where no row
   *  is in view), so a commit never moves the box for such a row while the reader cannot see it.
   *  Layout reads. The box that scrolls is `scroller`'s element while the host gave one and it
   *  holds one, else the window, and `componentDidUpdate` moves whichever this read. */
  current(): (AnchorRow & { row: HTMLElement }) | null {
    const named = this.named();
    const box = this.scrollerElement();
    if (named === null || box === null || !this.keptOnlyInView(named.via)) return named;
    const row = this.rowToKeep(named.row, box.getBoundingClientRect());
    if (row === named.row) return named;
    return row === null ? null : { id: this.idOf(row), row, via: "view" };
  }

  /** Where the host takes its box over, once the box is scrolled, the row nearest the middle of
   *  the view while the focus is on an element of that box that holds this whole list
   *  (`focusHoldsList`): the reader reads the list by keys from there, and the row a commit keeps
   *  then is that one (`named`), so an act of theirs that names no other row records it
   *  (`rememberHeld`), and takes the box back from a claim (`takeBox`). Null otherwise. Layout
   *  reads. */
  private listReadAt(): (AnchorRow & { row: HTMLElement }) | null {
    return this.ownsBox && this.focusHoldsList() ? this.centerRow() : null;
  }

  /** True where the focus is on an element of the box the host named that holds this whole list,
   *  the root included: the reader's keys scroll the box from there, over the list. */
  private focusHoldsList(): boolean {
    const active = document.activeElement;
    const box = this.scrollerElement();
    const root = this.root;
    return (
      active !== null &&
      box !== null &&
      root !== null &&
      box.contains(active) &&
      active.contains(root)
    );
  }

  /** The row the reader's place is named at, before `current()` puts it through the rule where it
   *  applies: `interacted()`, else the place kept (`kept`): the row the host keeps the place of
   *  (`keepRow`), or, where the host takes its box over, the row of the reader's last recorded
   *  place (`readerKept`); else - only once the reader has scrolled - the row nearest the list's
   *  visible centre (a hit-test probe: layout reads). */
  private named(): (AnchorRow & { row: HTMLElement }) | null {
    const interacted = this.interacted();
    if (interacted !== null) return interacted;
    const kept = this.hostKept() ?? this.readerKept();
    if (kept !== null) return { id: this.idOf(kept), row: kept, via: "kept" };
    return this.centerRow();
  }

  /** The row nearest the list's visible centre, once the box that scrolls - the one the host
   *  named while it holds one, else the window - is scrolled off its top; null before then, and
   *  where no row is at the centre (a hit-test probe: layout reads). */
  private centerRow(): (AnchorRow & { row: HTMLElement }) | null {
    const root = this.root;
    if (root === null) return null;
    const scroller = this.scrollerElement();
    if ((scroller === null ? window.scrollY : scroller.scrollTop) <= 0) return null;
    const row = rowAtCenter(root, `[${this.props.item}]`, scroller);
    return row === null ? null : { id: this.idOf(row), row, via: "center" };
  }

  /** Where the host takes its box over and no act of the reader's names a row now, the row of the
   *  place their last act recorded, which the events the page raises since have not changed: the
   *  pointer's row moved from under it, the focused control gone with the node that held it.
   *  Null where no such place is kept, and for any other host, whose commits keep the centre
   *  then. */
  private readerKept(): HTMLElement | null {
    const held = this.held;
    if (!this.ownsBox || held === null || held.hostRow !== null) {
      return null;
    }
    return this.scrollerElement() === null ? null : this.rowById(held.id);
  }

  /** Keeps the place of `row`, a row of this list, for the host, while no act of the reader's
   *  names a place the reader can see: a control of the host's outside the list holds the focus,
   *  and what it starts changes the list around it, as a page of older rows read in under the
   *  list does; or the host has just scrolled the box to a row an address names, as the history
   *  lands an agents' turn, and keeps it at the top it landed at, whatever order a change of
   *  height and that scroll's event come in. Where the host names `element` inside `row`, as the
   *  history names the turn it landed, the place is that element's own (`held`): recorded at the
   *  top it stands at now, where any of it lies in the view, and kept as the rule `keptIn` states.
   *  The row's own content above the element can then change height, as a long turn above a reply
   *  the history landed is cut, and the element stays where it was while the row's top moves.
   *  Without `element`, or where `element` lies outside the view or can keep no place (`keptIn`),
   *  the row's own top is kept. The row's place is recorded as a reader's would be, by the rule
   *  `rowToKeep` states where the host takes its box over (`keptOnlyInView`): where that rule
   *  puts another row in its stead, the place kept is that row's top, and still the host's
   *  (`hostRow`). The box's own scroll anchoring is off meanwhile where the host takes the box
   *  over (`keepPlace`), and `current()` chooses the row after the reader's focus and pointer and
   *  before the centre. The place goes at `keepRow(null)`, which leaves a place the reader's own
   *  act recorded alone; when an act of the reader's records another place (`rememberHeld`): the
   *  focus moving into the root, the pointer moving onto a row or off one, a scroll they made;
   *  when the row leaves the root; and when that rule finds no row in view. A press of the
   *  reader's inside the root lets `element` go, and the row's top is kept from then on
   *  (`readerPressed`). The browser pulling the box back to its end is no act of the reader's,
   *  and the place stays the host's, recorded again where the pull-back left the box
   *  (`viewScrolled`); where the host takes its box over, so does any scroll that is not the
   *  reader's (`rebaseHeld`). While the reader's hand is on a row any of which lies in the view
   *  (`interacted()`: the focus within one, or the pointer resting on one), it keeps nothing and
   *  leaves the place as it is: the reader's act names the place, its kept top and a focused
   *  control's own place with it, and a host's keep never takes its place. A hand on a row wholly
   *  outside the view names no place the reader can see (`overlaps`), and the host's row is kept
   *  then. Anything that is not a row of this list lets the host's place go, as null does. Where
   *  the rows scroll in the document nothing is kept, as nothing is for the reader there, and
   *  while another keeper claims the box nothing is kept either (`claimBox`). */
  keepRow(row: HTMLElement | null, element?: Element): void {
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
    const hand = this.interacted();
    const view = scroller.getBoundingClientRect();
    if (hand !== null && overlaps(hand.row.getBoundingClientRect(), view)) return;
    this.keepPlace({ host: true, inner: element ?? null, row, via: "kept" });
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

  /** The rows above `row` that the host names as leaving (`leaving`), in the render that last
   *  drew them (`previous`) or in this one, each with how far `row` moves up once it has gone:
   *  the whole space it takes (`spaceTaken`), the part above the view's top included, and the
   *  elements that are no row right before and right after it. Layout reads for the rows named
   *  and those elements alone; none where the host names none. */
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
      if (!this.namedLeaving(id, previous)) continue;
      const lead = each.previousElementSibling;
      found.push({
        by: spaceTaken(each),
        id,
        // A heading between two named rows is the first one's trail: it is counted once.
        lead: found.at(-1)?.trail?.element === lead ? null : beside(lead, selector),
        trail: beside(each.nextElementSibling, selector),
      });
    }
    return found;
  }

  /** Whether the host names row `id` as leaving (`leaving`), in the render that last drew the
   *  rows (`previous`) or in this one: a write can stop naming its rows in the same commit that
   *  moves them. */
  private namedLeaving(id: string, previous: Readonly<ViewportAnchorProps>): boolean {
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
      } else if (!row.contains(each) && !this.namedLeaving(this.idOf(each), previous)) {
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
   *  elements before and after it their `by` too where those elements went as well. */
  private freedAbove(row: HTMLElement, leaving: readonly Leaving[]): number {
    let freed = 0;
    for (const { by, id, lead, trail } of leaving) {
      const now = this.rowById(id);
      const above =
        now !== null && (row.compareDocumentPosition(now) & Node.DOCUMENT_POSITION_PRECEDING) !== 0;
      if (above) continue;
      freed += by;
      freed += freedBy(lead);
      freed += freedBy(trail);
    }
    return freed;
  }

  /** How far the rows a press of the reader's brought into the list above `row` (`arriving`)
   *  moved it down, now the commit is in: the whole space each takes (`spaceTaken`), the part
   *  above the view's top included, and the space of the element right after it where that
   *  element is no row and came into the current row's list with it (a band's **Show more**,
   *  which comes with the rows its unfold draws), as the list stood before the commit (`before`)
   *  tells. Layout reads for the rows named and those elements alone; none, and no walk of the
   *  rows, where the host names none. */
  private broughtAbove(root: HTMLElement, row: HTMLElement, before: ListBefore | null): number {
    const { arriving, item } = this.props;
    if (arriving === undefined) return 0;
    const selector = `[${item}]`;
    let brought = 0;
    for (const each of root.querySelectorAll<HTMLElement>(selector)) {
      if (each === row) break;
      if (!arriving(this.idOf(each))) continue;
      brought += spaceTaken(each);
      const trail = each.nextElementSibling;
      if (
        trail !== null &&
        before !== null &&
        trail.parentElement === before.list &&
        !trail.matches(selector) &&
        !before.elements.has(trail)
      ) {
        brought += spaceTaken(trail);
      }
    }
    return brought;
  }

  /** Where the control that holds the focus in `row` stands against the reader's sight; null
   *  where the focus is not in `row`. */
  private focusSight(row: HTMLElement): FocusSight | null {
    const focused = document.activeElement;
    if (focused === null || !row.contains(focused)) return null;
    const { bottom, top } = this.view();
    const box = focused.getBoundingClientRect();
    return {
      focused,
      meets: box.bottom >= top && box.top <= bottom,
      whole: box.top >= top && box.bottom <= bottom,
    };
  }

  /** Brings the control that holds the focus in `row` whole into sight at its nearest edge where
   *  the reader can see a part of it, which the page's sight rule allows for a landing partly in
   *  sight (`components/focus-place.ts`). Uses `scrollToNearest` rather than `scrollIntoView` so
   *  the root's scroll padding cannot scroll the window past the cut. A control wholly in sight,
   *  or wholly out of it, is left where it is. */
  private showFocused(row: HTMLElement): void {
    const sight = this.focusSight(row);
    if (sight?.meets && !sight.whole) scrollToNearest(sight.focused);
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
    // While another keeper claims the box, a commit moves it for nothing (`claimBox`).
    if (this.boxClaimed()) return null;
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
    const selfLeaving = this.namedLeaving(current.id, previousProps);
    const top = held ?? current.row.getBoundingClientRect().top;
    const standIn = selfLeaving ? this.standInFor(current.row, previousProps) : null;
    const holding = leaving.length > 0 || selfLeaving;
    if (holding) this.holdNativeAnchoring();
    const windowTop = holding && this.scrollerElement() === null ? window.scrollY : null;
    // The current row's list and every element in it, where the host names rows a press brings
    // in: the commit cannot be read in advance for the rows to come, so what stands now is what
    // tells the elements that come with them (`broughtAbove`).
    const list = current.row.parentElement;
    const listBefore =
      this.props.arriving === undefined || list === null
        ? null
        : { elements: new Set(list.children), list };
    return {
      group: at,
      id: current.id,
      leaving,
      listBefore,
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
      // A place the host keeps goes with its row: a commit that took the row out of the root
      // ends it, and a box taken over gets its own scroll anchoring back (`keepPlace`).
      if (this.held !== null && this.held.hostRow !== null && this.hostKept() === null) {
        this.keepPlace(null);
      }
      this.putPlaceBack(snapshot);
    } finally {
      this.releaseAfterRendering();
    }
  }

  /** Puts the row `snapshot` measured back at its top, less the space the rows a press of the
   *  reader's took away from above it freed and more the space the rows such a press brought in
   *  above it take, once the commit is in: every other change of height above it in that commit,
   *  growing or shrinking, is made up for. Where a press took the row itself away, the row that
   *  stands in for it (`standIn`) is put back the same way. Where the window scrolls the rows and
   *  the browser moved it in a commit whose anchoring this component held off, the window is put
   *  back first: Firefox holds the view against the going of the rows a press took away while a
   *  dialog's scroll lock is on (`useDialog`), `overflow-anchor: none` on the root and the body
   *  notwithstanding, and moved the window 730 px up at 390 px when the Inbox's Decline dialog
   *  went with its row. Once the rows a press took away have gone, or the rows it brought in have
   *  come, a place kept for the hand's row (`held`) that is another row than the one put back -
   *  the pointer resting on a row while the focus stands in another - is let go, as a press in
   *  the control row lets it go (`keepPressedRow`). */
  private putPlaceBack(snapshot: Snapshot | PressedRow | null): void {
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
    const current = this.rowById(snapshot.id);
    const place = current === null ? snapshot.standIn : snapshot;
    const element = current ?? (place === null ? null : this.rowById(place.id));
    if (place === null || element === null) return;
    const { group } = this.props;
    if (group !== undefined && element.getAttribute(group) !== place.group) return;
    const freed = this.freedAbove(element, place.leaving);
    const brought = this.broughtAbove(root, element, snapshot.listBefore);
    const top = place.top - freed + brought;
    const delta = element.getBoundingClientRect().top - top;
    // Whether the rows a press of the reader's took away or brought in moved this row.
    const pressMoved = freed > 0 || brought > 0;
    if (delta === 0 && !pressMoved) return;
    // Whether the control that holds the focus in this row stands whole in the reader's sight just
    // before this component's own scroll: read once the commit is in, with every landing a child
    // made in it, so a control it finds whole and leaves cut was cut by that scroll alone.
    const wholeBefore = delta !== 0 && this.focusSight(element)?.whole === true;
    if (delta !== 0) this.scrollBy(delta);
    this.restampHeld(place.id, place.top, top);
    // The rows the press took away moved every row after them up, and the rows it brought in moved
    // every row after them down, as the press asked, and a place kept for another row is the place
    // it had before they went or came: the resize this commit makes of the list would put that row
    // back there (`contentResized`), and scroll the whole list back by their height. In the Inbox
    // at 1440x900, with the pointer resting on a row below the rows the bar's Snooze took and the
    // focus on the mark the bar's hand-off parked it on, that moved the row the hand-off had just
    // landed on by 134 px (builds Job `infra-3135-i-h5-g6tnd`).
    if (pressMoved && this.held?.id !== place.id) this.keepPlace(null);
    // The rows a press of the reader's took away moved this row up, or the rows it brought in
    // moved it down, and a focus in it must end in the reader's sight. So must a focus this
    // component's own scroll took out of it: the scroll keeps the row's place, not the focus's.
    // Measured in WebKit in the Inbox at 390x664: after the bar's Dismiss the hand-off landed the
    // focus on a mark at the window's bottom edge, and the scroll of -1.33 px that kept its row's
    // place left the mark 0.6 px past that edge. Where this component did not scroll, or its
    // scroll found the focus cut or out of sight, the second reason adds nothing.
    if ((pressMoved && snapshot.via === "focus") || wholeBefore) this.showFocused(element);
  }

  /** Puts the control row a press of the reader's was made in (`pressedRow`) back at its top once
   *  the commit is in, so what the press changed below it moves nothing above it, and a change
   *  above it is made up for. The place kept for the hand's row (`held`) is let go: the commit
   *  moved that row with the list, as the press asked, and a resize after it must not put the row
   *  back (`contentResized`). */
  private keepPressedRow({ row, top }: PressedRow): void {
    this.keepPlace(null);
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
