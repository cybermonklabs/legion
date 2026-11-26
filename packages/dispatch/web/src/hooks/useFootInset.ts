import { type RefObject, useLayoutEffect } from "react";

/**
 * The layers of the foot of the screen, from the bottom up. Everything fixed or stuck at the foot
 * of the screen stacks through them, so no float lies over a foot element and no float leaves the
 * screen, whichever of them show together and at any size.
 *
 * Registering an element through `useFootInset` is what makes it a foot element: no rule can tell
 * from an element's style that a float may lie over it, since a composer laid out in the page's
 * flow at the bottom of a page that fills the screen is as much at the foot as one fixed there.
 * A new foot element picks its layer here by name and joins this list.
 *
 * The elements of the `page` layer are obstacles. Each one's band is its box, clipped to the
 * screen, as offsets up from the screen's bottom edge. A page element counts while it has a box
 * on screen, wherever it sits and however it is placed - fixed, sticky to the foot, absolute or in
 * flow - unless it is sticky to the top (`position: sticky` with a `top` other than `auto`,
 * whatever its `bottom` says), which never counts.
 *
 * The elements of the layers above `page` are floats, all fixed. A float sits 1rem above the
 * property of the layer below its own, and never lower than its floor. Its floor is the custom
 * property `--foot-floor`, which its own classes set per breakpoint and its `bottom` reads. A float
 * that sets no floor has a floor of 0. So the hook places a float's band from that property, its
 * floor and its height, and never measures how high the float stands. Its left and right edges do
 * not depend on the property, so the hook holds them as it measures the float. A float with no box
 * counts nothing.
 *
 * Each layer's property, on the document element, is chosen for the floats of the layer above it
 * (`publish`): the lowest value at which each of those floats sits in a free band. A free band
 * keeps 1rem away from every page element and every float of a lower layer. So a float sits on
 * its floor under every obstacle it clears, or 1rem above an obstacle. Where no band is free, the
 * property is the highest value at which the floats stay on the screen, or else 0. A property is
 * unset while it holds 0. No page element's band is capped: every page element is bounded - the
 * live view's message box at 40dvh, the Agents section at 50vh, a composer a host's frame fixes
 * at the foot at the screen's height less 6rem (`FOOT_COMPOSER_FRAME_BOUND`) - so a band holds
 * the floats wherever the window is tall enough, and the fallback only keeps them on the screen.
 *
 * - `page` (`--foot-page-inset`): the page's own foot controls. Each keeps its own place, and the
 *   page lays them out against each other.
 *   - The docked composer below `sm`, and a composer a host's frame fixes at the foot: a phone
 *     thread's and a phone reply's in the Conversation, and a phone margin thread's (`ThreadCard`'s
 *     composer frame), all through `MentionComposer`. From `sm` up the docked composer sticks to
 *     the top and counts nothing.
 *   - The shared selection bar from `sm` up (`SelectionBar`), over the issues table's and the
 *     Inbox's marked rows. Below `sm` it sits under the header and counts nothing.
 *   - The margin's sheet below `xl` while it is collapsed (`MarginSheet`).
 *   - The Agents page's broadcast composer, sticky to the foot of the page's scroll
 *     (`AgentsPage`), wherever it is on screen, stuck or resting at the end of the list: the
 *     wrapper that holds it with the sends, and the composer's own section, which is what sticks
 *     on a narrow or short screen, where the wrapper has no box. Elsewhere the wrapper's band
 *     holds the section's.
 *   - The agent live view's composer (`AgentThread`), in the flow of a page that fills the
 *     screen; the rows laid out under it, the row of pickers and the switch lines, in the one box
 *     that holds both (`AgentConversationPage`); and the live thread's own `Jump to latest`, just
 *     above the composer while it shows. All three count only in the view in front.
 * - `banner` (`--foot-banner-inset`): the update banner (`DeploymentResilience`), which reads the
 *   page layer's property and has no floor.
 * - `status` (`--foot-status-inset`): the shell's status corner - Pending changes, the note on
 *   drafts the tab cannot keep, and the connection pill (`app.tsx`). It reads the banner layer's
 *   property, and its floor is 5rem below `xl` and 0 from `xl` up.
 * - `top` (no property): the Conversation's `Jump to latest` (`ConversationTab`) while it shows,
 *   with a floor of 5rem, 9rem from `sm` up and 6rem from `xl` up; and the chord indicator
 *   (`ChordIndicator`) while a chord is pending, with a floor of 8rem, and 4rem from `xl` up.
 *   Both read the status layer's property. No float sits above them, so this layer publishes no
 *   property.
 *
 * Not in the stack: the search palette and the phone Contents sheet, each a whole-screen surface
 * over the page while it is open, with nothing at the foot beside it; the margin's expanded sheet
 * below `xl`, a modal dialog over the page; and `MultiSelect`'s sticky foot, which sticks to the
 * foot of its own popover, not of the screen.
 *
 * A scroller under a float is the box that scrolls content a float lies over, so that its last
 * content would end under the float. No element registers as one: while a float has a box, the
 * hook finds every scroller under a float itself (`clearScrollers`), so no scroller can be left
 * off a list. A scroller is not a foot element: it has no band and no float, and nothing keeps
 * clear of it.
 *
 * - Why a hit test. Only the screen knows what lies under a float: a list of names misses the next
 *   scroller, and no rule on style alone can tell which of the page's scroll containers sits under
 *   one. So for each float the hook takes nine points across its box - 8px in from each side and
 *   its centre, each 2px inside its top edge, at the middle of its band and 2px inside its bottom
 *   edge - and asks `document.elementsFromPoint` what is there, so any part of a float over a
 *   scroller's foot is found: a point at the middle alone missed the update banner's upper half
 *   over the issues table's last row at 1280x720. It skips every element inside a float and takes
 *   the first one left: the content under the float. An element hit at several points of one
 *   float is walked once.
 * - Why the walk. From that element the hook walks up its ancestors, itself included
 *   (`scrollerUnder`), and stops at the first for which one of three questions, asked in this
 *   order, has the answer yes. So the scroller under the float is the box where the walk stops,
 *   or none, and end room goes only to a scroller whose scroll moves the content the float covers.
 *   - Is it a scroller whose foot reaches the float: its `overflow-y` is `auto` or `scroll`, its
 *     lower edge, clipped to the screen, lies below the float's top edge, and it can scroll on
 *     the block axis (below)? Then it is the scroller. It is the innermost box that scrolls that
 *     content, and the only one whose end room lets that content scroll clear: end room in a box
 *     further out moves the inner box whole. A scroll container whose lower edge is above the
 *     float's top does not hold that content where the float is: the content escapes it,
 *     absolute, and the walk goes on.
 *   - Is it fixed, or sticky on the block axis (`position: sticky` with a `top` or a `bottom`
 *     other than `auto`)? Then the walk stops, and that point gives no scroller end room. A box
 *     stuck to the top or the foot of a scroll holds its place on the screen while the scroll
 *     goes on, and a fixed box holds it always, so no scroll further out moves what it holds
 *     clear of the float: a fixed sheet, a stuck header, or a column stuck at the height of the
 *     screen that is not a scroller itself. A box sticky only on the inline axis - a pinned cell
 *     of the issues table, sticky with only a `left` - moves with a scroll on the block axis, the
 *     one end room lengthens, so the walk goes on past it to the table's box.
 *   - Does it carry `data-document-scroll`: `main` (`#main-content`), or, in the new issue page's
 *     frame, the frame's column? Then the document scrolls that content, and the scroller is the
 *     document's, that element, with no test of overflow, since its box grows with its content.
 *
 *   `body` never takes room, and a walk that reaches it finds no scroller. Room in `body` lies
 *   below the shell rather than in it, so at the end of the document's scroll it lifts the shell,
 *   and the columns stuck in it, off the top of the screen: under Pending changes over an issue
 *   page's Review margin it pushed the Navigation off and left an empty band at the foot. The
 *   document's own content is in the element that carries `data-document-scroll`, which takes
 *   the room while the document scrolls it.
 * - Why the page says where the document's content ends. On a page that fills the screen
 *   `<main>` is a bounded box and the document scrolls nothing: room in `main` there would
 *   squeeze the page above it rather than lengthen it, lift the live view's composer, and hold
 *   itself, with the floats under the composer over that room. No box metric tells a bounded
 *   `main` from one the document scrolls, since a page that fits the window exactly - the issues
 *   table sizes its box to it - reads the same. So `data-document-scroll` is the page's
 *   statement that the document scrolls the content of the element that carries it: `main` on
 *   every page the document scrolls outside the new issue page's frame (`app.tsx`), and in the
 *   frame the frame's column (`IssueOutlinePage.tsx`), the tallest child of the row that holds
 *   the Contents pane and the column. The room goes in that column, because the Contents pane is
 *   stuck in the row, which holds it within the row's content box, and that box ends where the
 *   column does: room in `main` lies below the row, and the row's own bottom padding lies outside
 *   its content box, and either carried the pane 180px above its place at the end of the
 *   document's scroll at 1024x500. Room in the column grows the column, the row's content box and
 *   the pane's range with it. Only while an element carries the statement is that element a
 *   scroller.
 * - Why a box can scroll on the block axis two ways (`scrollsOnBlockAxis`). Its content overflows
 *   it now, with the hook's own end room left out (`scrollHeight - room > clientHeight + 1`). Or
 *   its height is set by layout rather than by its content: it is taller than its content, so end
 *   room lengthens its scroll and not its box - a column at the height of the screen with short
 *   content, such as the Review margin, the contents column or the Navigation. A box as tall as
 *   its content counts by neither: a box that only scrolls sideways - a code block, the issue
 *   board, a settings table, whose `overflow-x: auto` makes `overflow-y` compute to `auto` too -
 *   or a box below its `max-height`. End room in it would grow it under the float and move the
 *   content after it. Neither way counts the hook's own room, so marking a scroller cannot change
 *   whether it counts.
 * - Why the height set by layout is read from the content's extent. Only a layout experiment -
 *   give the box room and see whether its box grows - could tell it directly, and the hook makes
 *   none. So it reads the extent of the content: from the top of the box's padding box, as its
 *   scroll lays the content out (its top, below its top border, less `scrollTop`), to the foot of
 *   that content, plus its own bottom padding without any room the hook gave it. A box taller
 *   than that by more than 1px (`clientHeight > extent + 1`) has a height set by layout. The foot
 *   of the content (`contentFoot`) is the lowest bottom margin edge of the box's element children,
 *   or its top padding where it has none. The margin counts, since a box that scrolls holds its
 *   last child's margin inside it. Every child is read, not only the last: in a row - a flexbox,
 *   a grid - the last child is not the lowest. A `display: contents` child has no box, so its
 *   own children are read in its place. Each pass reads a box's children once at most, however
 *   many points of the floats walk through it. The spacer is a pseudo-element, not a child, and
 *   bottom padding moves no child down, so the box's own room never makes it stop counting.
 * - Why content in lines counts only by overflow. No element's box reaches the foot of a line:
 *   the line's leading lies below its text. A code block's `code`, which is inline, ends 3px
 *   above the foot of the block at 12px type, so read from its box the code block would seem
 *   taller than its content and take end room it grows by. So where the content holds a text
 *   node of more than white space or an inline-level child, the box has no extent the hook can
 *   read, and its height is never taken as set by layout.
 *
 * What the reading cannot see is what is left. A bounded box whose content is laid out in lines
 * counts only once that content overflows it, so where nothing further out scrolls the box - it
 * is stuck, or the page fills the screen - its last line stays under a float until then. And a
 * box as tall as its content reads taller where something below its children is not read: a
 * bottom margin that collapses out of a child's own last child, or an `::after` of the box's
 * own; such a box would take end room it grows by. No box under a float on any page today is
 * either, so no row reads one. The rows that read what the reader sees at the end of each
 * scroller the floats lie over are in `e2e/issues-table.e2e.ts`: the Review margin's column, the
 * live view's transcript and the Inbox reading pane each scroll their last content clear, the
 * table's last row scrolls clear, and an issue page scrolled to its end keeps the shell's
 * Navigation on the screen; and `e2e/foot-sweep.e2e.ts` reads every reviewed route and eight
 * pages, the frame's among them, at every combination of the floats.
 *
 * A scroller's foot is the lower edge of its box, clipped to the screen, as an offset up from the
 * screen's bottom edge, or 0 where the document scrolls its content. Its clearance is how far
 * above its foot the floats found over it reach, plus 1rem, or 0 where none counts
 * (`footClearance`). The floats that count reach above its foot and lie wholly below its ceiling
 * (`footCeiling`): the lowest lower edge, at or above its foot, of a page element it holds that is
 * not fixed. A float can straddle the foot - Pending changes over the last row of the issues
 * table's box - and only the part of its band above the foot needs room.
 *
 * - Why fixed elements do not cap. A float lifted above a page element the scroller holds -
 *   Pending changes above the Agents page's broadcast composer at 1280x720 - sits over content that
 *   still scrolls clear of it, and counting it would let the end room move the element, which
 *   moves the float, which moves the end room. Only an element that scrolls with the scroller's
 *   content can be moved by its end room. A fixed one cannot - the selection bar, the margin's
 *   collapsed sheet below `xl` - and neither can a page element outside the scroller - for the
 *   live view's transcript, its composer, the rows under it and `Jump to latest` - so neither
 *   caps anything, and the floats above them count. Were `Jump to latest` to cap the transcript,
 *   the end room would go as soon as the reader scrolled back from the end, the transcript would
 *   end where they were, and the view would take them back to the end.
 * - Why a spacer rather than each scroller's own padding. The hook marks a scroller whose
 *   clearance is more than 0 (`data-foot-clearance`) and writes its end room, the clearance less
 *   its own bottom padding and never below 0, as `--foot-clearance` on its own inline style, unset
 *   while it holds 0. One rule in `styles.css` draws a spacer that tall after the content of every
 *   scroller marked `after`, so no scroller's classes need to know the floats, and a new scroller
 *   needs nothing at all. Its padding and the spacer together make the clearance, so its last
 *   content scrolls clear of those floats, and with no float over it its padding is its own. The
 *   spacer lands at the end of the content only in a block container or a column flexbox, where
 *   `flex: none` keeps it whole; a column flexbox's `gap` adds one more gap above it, which is
 *   extra room and covers nothing. In a grid it takes the next cell, in a row flexbox it lands
 *   beside the content, and on a scroller with an `::after` of its own the shared rule would
 *   overwrite that. So the hook reads a scroller's `display`, its `flex-direction` and its own
 *   `::after` as it marks it, and marks any other scroller `padding`: it takes the room as its
 *   inline bottom padding, the larger of its own and the clearance. Its own is read as it is
 *   marked, and again after a window resize, once the inline value is removed. The document's
 *   scroller, the element that carries `data-document-scroll`, always takes the room that way, as
 *   padding: a page sized to the window reads main's bottom padding as the room it keeps at the
 *   foot - the issues table's box is the window less that padding (`components/table-layout.ts`) - so
 *   a spacer there would lengthen the document under the box instead, and each scroll to the end
 *   and the measure after it would grow the box by that scroll. Its box grows with the room, or
 *   the page it holds gives way to it, and either way its foot stays 0.
 *
 * Marking a scroller the walk finds changes its scroll height, never its box, and the hit test
 * reads boxes. A point over the spacer hits the scroller itself, since a pseudo-element is not an
 * element. So such a mark never changes which scroller the next pass finds. The document's
 * padding can move what the floats lie over - the issues table's box gives way to it - but only
 * off the floats, which then lie over that padding, and a point there hits that element itself,
 * so the mark holds. Each pass finds every scroller under a float, unmarks every scroller marked
 * before that it no longer finds - its attribute, its property and any inline padding - unless
 * the reader stands in its room (below), and writes a mark, a property or a padding only where
 * its value changes. So no page keeps end room it no longer needs once the reader is out of it,
 * and at rest a pass writes nothing.
 *
 * - Why room the reader stands in stays. End room added below the end of a scroll moves nothing,
 *   but room taken away while the reader stands in it shortens the scroll under them: its
 *   `scrollTop` clamps to the new end, and the content they read jumps down by what was taken.
 *   A reader at the end of the issues table's box who discards Pending changes stands in room it
 *   gave: at 1280x720 the table's last row jumped 58px down as it went. A float that shows after
 *   the reader reached the end - the chord indicator - adds room below them, which they do not
 *   stand in. So room a scroller would lose - all it holds, where no float lies over it any
 *   more, or the part above what the floats left still need - stays whole while the reader
 *   stands in that part: while the scroll that room lengthens (the document's for the element
 *   that carries `data-document-scroll`) is off its start and has less than that part left to
 *   its end (`standsInRoom`). The first pass after a scroll that leaves it takes it away, and
 *   then nothing clamps. A scroller whose reader is out of its room loses it at once, at the pass
 *   that no longer needs it, and a window resize, which moves the content anyway, reads every
 *   mark afresh and keeps none.
 * - Why the document's room is kept for a box it holds too. The document's room is padding, and
 *   a page that gives way to it rather than lengthening passes the clamp on: the issues table
 *   sizes its box to the window less that padding, so taking the padding grows the box by it and
 *   shortens the box's own scroll by as much. At 1600x1000 the page fits the window, the
 *   document's own scroll is at its start and clamps nothing, and the table's last row still
 *   went down 58px as Pending changes was discarded. So the document's scroller keeps its room
 *   while a box it holds would be clamped by losing it: a box whose scroll is off its start with
 *   less than that room left to its end (`clampsABoxItHolds`). Only a box whose scroll has moved
 *   can clamp, which is what that walk reads first.
 * - Why the passes run only while a float shows or a scroller holds room. With no float nothing
 *   lies over content, so no scroller needs room, and a pass reads layout on every scroll. So
 *   while no float has a box and no scroller holds room, nothing asks for a pass: the hook listens
 *   for none of the events below (`listenWhileNeeded`). While a float has a box, or a scroller
 *   holds room, it listens for any scroll, a window resize, and elements that join or leave the
 *   page (a `MutationObserver` over `body`, for `childList` and `subtree` only), and each of them
 *   asks for a pass at the next animation frame. While a float has a box, each `publish` asks for
 *   one too. Passes asked for in one frame run once. A `publish` that leaves no float asks for
 *   none, but the one at which the last float loses its box runs a pass at once, which unmarks
 *   every scroller whose reader is out of its room. A pass stops the listening once no float has
 *   a box and no scroller holds room. The observer does not watch attributes or styles, so the
 *   hook's marks and end room do not wake it. Only taking end room away from a scroller scrolled
 *   to its end can clamp its scroll and wake one more pass, which then finds the same marks; and
 *   the hook takes none from under the reader.
 * - What listens with no float shown. The measures of the foot elements listen whatever the
 *   floats (`useFootInset`): each registered element keeps its own `ResizeObserver` and a window
 *   resize listener, and each page element that has a box and is neither fixed nor sticky to the
 *   top also keeps a capture scroll listener and the same observer over its parent (`followMoves`).
 *   Each of them measures its element and publishes again, and with no float shown that `publish`
 *   asks for no pass.
 *
 * `--foot-clearance` is registered as not inherited (`styles.css`), so a scroller inside a marked
 * one, while it holds no room of its own, reads 0, never the room of the scroller around it. The
 * spacer takes its own scroller's value by `inherit`, which reaches that scroller alone.
 */
export const FOOT_LAYERS = [
  { name: "page", property: "--foot-page-inset" },
  { name: "banner", property: "--foot-banner-inset" },
  { name: "status", property: "--foot-status-inset" },
  { name: "top", property: null },
] as const;

/** The name of one layer of `FOOT_LAYERS`. */
export type FootLayer = (typeof FOOT_LAYERS)[number]["name"];

/** A stretch of the screen's height, as offsets up from the screen's bottom edge: `lo` is its
 *  lower edge and `hi` its upper edge. */
interface Band {
  readonly hi: number;
  readonly lo: number;
}

/** A page element's band, and whether the element is fixed (`position: fixed`). */
interface PageBand extends Band {
  readonly fixed: boolean;
}

/** A float as the hook holds it: its floor (`--foot-floor`) and its height, and its left and right
 *  edges on the screen, all in pixels. */
interface Float {
  readonly floor: number;
  readonly height: number;
  readonly left: number;
  readonly right: number;
}

/** A float with a box, placed at the property `publish` last chose for it: its element, its band
 *  and its left and right edges. */
interface PlacedFloat {
  readonly band: Band;
  readonly element: Element;
  readonly left: number;
  readonly right: number;
}

/** How a marked scroller takes its end room (`FOOT_LAYERS` says which way and why), and `room`,
 *  the end room the hook gave it last, in pixels. `own` is its own bottom padding, for the
 *  `padding` way, and `room` there is the padding added above it. */
type Mark =
  | { readonly way: "after"; readonly room: number }
  | { readonly way: "padding"; readonly own: number; readonly room: number };

/** The band of each page element that counts. */
const pageBands = new Map<Element, PageBand>();

/** The floats of each layer above `page` that have a box. */
const layerFloats = new Map<FootLayer, Map<Element, Float>>();

/** Every float that has a box, at the properties `publish` last chose. */
let placedFloats: readonly PlacedFloat[] = [];

/** The mark of each scroller the hook has marked (`FOOT_LAYERS` says what that is). */
const marks = new Map<HTMLElement, Mark>();

/** The attribute that marks a scroller, valued by the way it takes its end room. */
export const MARK_ATTRIBUTE = "data-foot-clearance";

/** The custom property that holds a scroller's end room, on its own inline style. */
export const CLEARANCE_PROPERTY = "--foot-clearance";

/** The attribute by which the page states, on `<main>` or, in the new issue page's frame, on the
 *  frame's column, that the document scrolls that element's content (`FOOT_LAYERS` says why the
 *  hook cannot tell, and why the frame's room goes in its column). */
export const DOCUMENT_SCROLL_ATTRIBUTE = "data-document-scroll";

/** The bound of a frame that fixes a composer at the foot of the screen: a phone thread's and a
 *  phone reply's in the Conversation, and a phone margin thread's. The frame leaves 6rem of the
 *  screen above it, the shell's status corner and the 1rem it keeps clear, so the band above it
 *  holds the floats; a composer taller than that scrolls inside its frame. Unbounded, the field's
 *  hint line, a four-line draft and a refusal's row filled a 320x568 screen to its top, and the
 *  connection pill fell back over the composer. */
export const FOOT_COMPOSER_FRAME_BOUND = "max-h-[calc(100dvh-6rem)] overflow-y-auto";

/** The kind of box each computed `display` makes, of those where a spacer can land below the
 *  content: a block container, or a flexbox, where it lands there only in a column. */
const SPACER_BOXES: Readonly<Record<string, "block" | "flexbox">> = {
  block: "block",
  "flow-root": "block",
  "inline-block": "block",
  "list-item": "block",
  flex: "flexbox",
  "inline-flex": "flexbox",
};

/** The frame `clearScrollers` waits for, or 0 while none is asked. */
let clearFrame = 0;

/** Whether the window has resized since the last pass, which reads every mark afresh. */
let resized = false;

/** What wakes a pass when elements join or leave the page, made at the first float. */
let mutations: MutationObserver | undefined;

/** Whether the hook listens for what moves content under the floats (`listenWhileNeeded`). */
let listening = false;

function floatsOf(layer: FootLayer): Map<Element, Float> {
  const floats = layerFloats.get(layer) ?? new Map<Element, Float>();
  layerFloats.set(layer, floats);
  return floats;
}

/** What 1rem is, in pixels: the root font size. */
function remPixels(): number {
  return Number.parseFloat(getComputedStyle(document.documentElement).fontSize);
}

/** A lower edge `bottom`, clipped to the screen, as an offset up from the screen's bottom edge. */
function footOffset(bottom: number, screen: number): number {
  return screen - Math.min(bottom, screen);
}

/** A float's floor in pixels, from its `--foot-floor` in `px` or `rem`. An empty or other value
 *  is 0. */
function floorOf(element: Element): number {
  const floor = /^(\d*\.?\d+)(px|rem)$/.exec(
    getComputedStyle(element).getPropertyValue("--foot-floor").trim()
  );
  if (floor === null) return 0;
  const size = Number(floor[1]);
  return floor[2] === "px" ? size : size * remPixels();
}

/** The band of a float while the property it reads holds `inset`: on its floor, or `gap` above
 *  `inset`, whichever is higher, as its own `bottom` places it. */
function floatBand({ floor, height }: Float, inset: number, gap: number): Band {
  const lo = Math.max(floor, inset + gap);
  return { hi: lo + height, lo };
}

/**
 * The property for the floats of one layer: the lowest value at which no float of the layer
 * meets an obstacle. A float meets an obstacle where their bands come nearer than `gap`. The
 * values tried are 0 and then each obstacle's upper edge, from the lowest up, so a float sits on
 * its floor or `gap` above an obstacle. A value that puts a float past the top of the screen
 * (`screen`) is never taken. Where no value is free, it is the highest value tried that keeps the
 * floats on the screen, or else 0. A layer with no float gets 0.
 */
function freeInset(
  floats: readonly Float[],
  obstacles: readonly Band[],
  gap: number,
  screen: number
): number {
  if (floats.length === 0) return 0;
  let onScreen = 0;
  for (const inset of [0, ...obstacles.map(({ hi }) => hi).sort((a, b) => a - b)]) {
    const bands = floats.map((float) => floatBand(float, inset, gap));
    // A float's band only rises with the value, so no later value keeps the floats on the screen.
    if (bands.some(({ hi }) => hi > screen)) break;
    const free = bands.every(
      (band) =>
        !obstacles.some((obstacle) => band.lo < obstacle.hi + gap && band.hi > obstacle.lo - gap)
    );
    if (free) return inset;
    onScreen = inset;
  }
  return onScreen;
}

/**
 * The ceiling of a scroller whose foot is `lo` (`FOOT_LAYERS` says what that is), from the page
 * bands of the elements it holds (`held`): the lowest lower edge, at or above its foot, of the
 * band of an element that is not fixed, or no ceiling (infinity) where none is.
 */
function footCeiling(lo: number, held: readonly PageBand[]): number {
  let ceiling = Number.POSITIVE_INFINITY;
  for (const band of held) if (!band.fixed && band.lo >= lo) ceiling = Math.min(ceiling, band.lo);
  return ceiling;
}

/**
 * The clearance of a scroller whose foot is `lo` and whose ceiling is `ceiling` (`FOOT_LAYERS`
 * says what those are), from the bands of the floats found over it (`floats`): `gap` above the
 * highest upper edge of those that reach above its foot and lie wholly below its ceiling, as an
 * offset up from that foot, or 0 where none does.
 */
function footClearance(lo: number, ceiling: number, floats: readonly Band[], gap: number): number {
  let clearance = 0;
  for (const band of floats) {
    if (band.hi > lo && band.hi <= ceiling) clearance = Math.max(clearance, band.hi - lo + gap);
  }
  return clearance;
}

/** Writes `pixels` to `property` in `style` as `${pixels}px`, or removes the property while
 *  `pixels` is 0 (`setProperty` with an empty value removes it), and leaves a property that
 *  already holds that alone. */
function writePixels(style: CSSStyleDeclaration, property: string, pixels: number): void {
  const value = pixels === 0 ? "" : `${pixels}px`;
  if (style.getPropertyValue(property) !== value) style.setProperty(property, value);
}

/** The scroller under a point of a float, as `scrollerUnder` finds it: the scroller, its foot
 *  (`FOOT_LAYERS` says what that is), and whether the document scrolls its content. */
interface FoundScroller {
  readonly byDocument: boolean;
  readonly lo: number;
  readonly scroller: HTMLElement;
}

/**
 * The foot of the content `parent` lays out (`FOOT_LAYERS` says why): the lowest bottom margin
 * edge of its element children that have a box, and of the children of a `display: contents`
 * child, read in its place; never above `lowest`. Undefined where that content is laid out in
 * lines - a text node of more than white space, or an inline-level child - since no element's
 * box reaches the foot of a line.
 */
function contentFoot(parent: Element, lowest: number): number | undefined {
  let foot = lowest;
  for (const node of parent.childNodes) {
    if (node instanceof Text) {
      if (node.data.trim() !== "") return undefined;
    } else if (node instanceof Element) {
      const { display, marginBottom } = getComputedStyle(node);
      if (display === "contents") {
        const inner = contentFoot(node, foot);
        if (inner === undefined) return undefined;
        foot = inner;
      } else if (display.startsWith("inline") || display.startsWith("ruby")) {
        return undefined;
      } else if (display !== "none") {
        const { bottom } = node.getBoundingClientRect();
        foot = Math.max(foot, bottom + (Number.parseFloat(marginBottom) || 0));
      }
    }
  }
  return foot;
}

/**
 * Whether `box`, a scroll container whose top edge is `top` on the screen and whose computed
 * style is `computed`, can scroll on the block axis (`FOOT_LAYERS` says why each part holds):
 * its content overflows it with the hook's own end room left out, or its height is set by layout
 * rather than by its content. That is, it is taller by more than 1px than its content extent:
 * from the top of its padding box, as its scroll lays the content out, to the foot of that
 * content (`contentFoot`), plus its own bottom padding without any room the hook gave it. A box
 * whose content is laid out in lines has no extent the hook can read, so only an overflow counts.
 */
function scrollsOnBlockAxis(box: HTMLElement, computed: CSSStyleDeclaration, top: number): boolean {
  const held = marks.get(box);
  if (box.scrollHeight - (held?.room ?? 0) > box.clientHeight + 1) return true;
  const origin = top + box.clientTop - box.scrollTop;
  const foot = contentFoot(box, origin + (Number.parseFloat(computed.paddingTop) || 0));
  if (foot === undefined) return false;
  const own = held?.way === "padding" ? held.own : Number.parseFloat(computed.paddingBottom) || 0;
  return box.clientHeight > foot - origin + own + 1;
}

/**
 * The scroller under a float whose upper edge is `hi`, from `hit`, the element at a point of the
 * float (`FOOT_LAYERS` says what that is and why each part holds). The walk goes up from `hit`,
 * itself included, and stops at the first of these: a box whose `overflow-y` is `auto` or
 * `scroll`, whose lower edge lies below the float's top edge and which can scroll on the block
 * axis (`scrollsOnBlockAxis`, read once per pass for each box and held in `blockScrolls`), which
 * is the scroller; a box that is fixed, or sticky on the block axis, under which no scroller is;
 * and an element that carries `data-document-scroll`, which is the document's scroller, with a
 * foot of 0. A walk that reaches `body` finds none.
 */
function scrollerUnder(
  hit: Element,
  hi: number,
  screen: number,
  blockScrolls: Map<HTMLElement, boolean>
): FoundScroller | undefined {
  for (
    let element: Element | null = hit;
    element !== null && element !== document.body && element !== document.documentElement;
    element = element.parentElement
  ) {
    if (!(element instanceof HTMLElement)) continue;
    const computed = getComputedStyle(element);
    const { overflowY, position } = computed;
    if (overflowY === "auto" || overflowY === "scroll") {
      const { bottom, top } = element.getBoundingClientRect();
      const lo = footOffset(bottom, screen);
      if (lo < hi) {
        let scrolls = blockScrolls.get(element);
        if (scrolls === undefined) {
          scrolls = scrollsOnBlockAxis(element, computed, top);
          blockScrolls.set(element, scrolls);
        }
        if (scrolls) return { byDocument: false, lo, scroller: element };
      }
    }
    if (
      position === "fixed" ||
      (position === "sticky" && (computed.top !== "auto" || computed.bottom !== "auto"))
    ) {
      return undefined;
    }
    if (element.hasAttribute(DOCUMENT_SCROLL_ATTRIBUTE)) {
      return { byDocument: true, lo: 0, scroller: element };
    }
  }
  return undefined;
}

/** Takes away the mark of `scroller` and the end room it gave: its attribute, its property and
 *  any inline bottom padding. */
function unmark(scroller: HTMLElement): void {
  const held = marks.get(scroller);
  marks.delete(scroller);
  scroller.removeAttribute(MARK_ATTRIBUTE);
  scroller.style.removeProperty(CLEARANCE_PROPERTY);
  if (held?.way === "padding") scroller.style.removeProperty("padding-bottom");
}

/**
 * The mark `scroller` takes for end room of `clearance` pixels, the one way it takes it
 * (`FOOT_LAYERS` says which and why), read from its styles as it is marked and from the mark it
 * holds after: the spacer, `clearance` less its own bottom padding, or its inline bottom padding,
 * the larger of its own and `clearance`. The document's scroller (`byDocument`) always takes the
 * padding. Each is rounded up to a whole pixel, which the inline style keeps as written.
 */
function markFor(scroller: HTMLElement, clearance: number, byDocument: boolean): Mark {
  let held = marks.get(scroller);
  if (held === undefined) {
    const computed = getComputedStyle(scroller);
    const after = getComputedStyle(scroller, "::after").content;
    const box = SPACER_BOXES[computed.display];
    const spacer =
      !byDocument &&
      (after === "none" || after === "normal") &&
      (box === "block" || (box === "flexbox" && computed.flexDirection === "column"));
    held = spacer
      ? { room: 0, way: "after" }
      : { own: Number.parseFloat(computed.paddingBottom) || 0, room: 0, way: "padding" };
  }
  if (held.way === "after") {
    const own = Number.parseFloat(getComputedStyle(scroller).paddingBottom) || 0;
    return { room: Math.max(0, Math.ceil(clearance - own)), way: "after" };
  }
  return {
    own: held.own,
    room: Math.max(held.own, Math.ceil(clearance)) - held.own,
    way: "padding",
  };
}

/** Marks `scroller` with `next` (`markFor`) and gives it the end room that holds: its property,
 *  or its inline bottom padding, its own and the room together. Only a value that changes is
 *  written. */
function mark(scroller: HTMLElement, next: Mark): void {
  if (next.way === "after") writePixels(scroller.style, CLEARANCE_PROPERTY, next.room);
  else writePixels(scroller.style, "padding-bottom", next.own + next.room);
  marks.set(scroller, next);
  if (scroller.getAttribute(MARK_ATTRIBUTE) !== next.way) {
    scroller.setAttribute(MARK_ATTRIBUTE, next.way);
  }
}

/**
 * Whether taking `room` from the document's scroller `scroller` would clamp a box it holds: a
 * box whose own scroll has moved off its start and has less than `room` left to its end. The
 * document's room is padding, and the page it holds can give way to it rather than lengthen
 * (`FOOT_LAYERS`): the issues table sizes its box to the window less that padding, so taking the
 * padding grows the box by it, the box's own scroll shortens by as much, and a reader at its end
 * is clamped - at 1600x1000 the table's last row went down 58px as Pending changes was
 * discarded. Only a box whose scroll has moved can clamp, so the walk reads `scrollTop` first,
 * which is a property read, and asks for a style only where it is not at a start.
 */
function clampsABoxItHolds(scroller: HTMLElement, room: number): boolean {
  for (const box of scroller.querySelectorAll<HTMLElement>("*")) {
    if (box.scrollTop <= 0 || box.scrollHeight - box.scrollTop - box.clientHeight >= room) {
      continue;
    }
    const { overflowY } = getComputedStyle(box);
    if (overflowY === "auto" || overflowY === "scroll") return true;
  }
  return false;
}

/**
 * Whether the reader stands in the last `room` pixels of the end room `scroller` holds, so that
 * taking them away would clamp a scroll and move what they read (`FOOT_LAYERS` says why those
 * stay): the scroll that room lengthens - the document's where `scroller` carries
 * `data-document-scroll`, its own otherwise - is off its start and has less than `room` left to
 * its end; or, for the document's scroller, a box it holds would be clamped by the page giving
 * way to that padding (`clampsABoxItHolds`). A scroll at its start never clamps, and a scroller
 * no longer in the page holds no reader.
 */
function standsInRoom(scroller: HTMLElement, room: number): boolean {
  if (!scroller.isConnected) return false;
  const byDocument = scroller.hasAttribute(DOCUMENT_SCROLL_ATTRIBUTE);
  const scroll = byDocument ? document.documentElement : scroller;
  if (scroll.scrollTop > 0 && scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < room) {
    return true;
  }
  return byDocument && clampsABoxItHolds(scroller, room);
}

/**
 * One pass over the scrollers under the floats (`FOOT_LAYERS` says what those are and why each
 * part holds). It finds the scroller under each point of each float and the floats found over
 * each scroller, and gives each scroller whose clearance is more than 0 its end room. Room a
 * scroller would lose - all it holds, where the pass does not find it, or the part above what it
 * now needs - it keeps whole while the reader stands in that part (`standsInRoom`), and every
 * other scroller marked before that it does not find it unmarks. After a window resize it
 * unmarks every scroller first, so each mark it keeps is read afresh. It reads every box before
 * it writes anything, and between unmarking and marking a scroller it reads only styles, so the
 * browser never lays out a scroller without its end room and clips a scroll at its end. It
 * then listens, or stops, as `listenWhileNeeded` says.
 */
function clearScrollers(): void {
  clearFrame = 0;
  const screen = window.innerHeight;
  const gap = remPixels();
  const found = new Map<HTMLElement, FoundScroller & { readonly floats: Band[] }>();
  const blockScrolls = new Map<HTMLElement, boolean>();
  for (const { band, left, right } of placedFloats) {
    const walked = new Set<Element>();
    for (const offset of [band.hi - 2, (band.hi + band.lo) / 2, band.lo + 2]) {
      for (const x of [left + 8, (left + right) / 2, right - 8]) {
        const hit = document
          .elementsFromPoint(x, screen - offset)
          .find((element) => !placedFloats.some((float) => float.element.contains(element)));
        if (hit === undefined || walked.has(hit)) continue;
        walked.add(hit);
        const under = scrollerUnder(hit, band.hi, screen, blockScrolls);
        if (under === undefined) continue;
        const over = found.get(under.scroller) ?? { ...under, floats: [] };
        found.set(under.scroller, over);
        if (!over.floats.includes(band)) over.floats.push(band);
      }
    }
  }
  if (resized) for (const scroller of marks.keys()) unmark(scroller);
  resized = false;
  const next = new Map<HTMLElement, Mark>();
  for (const [scroller, { byDocument, floats, lo }] of found) {
    const held: PageBand[] = [];
    for (const [element, band] of pageBands) if (scroller.contains(element)) held.push(band);
    const clearance = footClearance(lo, footCeiling(lo, held), floats, gap);
    if (clearance > 0) next.set(scroller, markFor(scroller, clearance, byDocument));
  }
  const kept = new Set<HTMLElement>();
  for (const [scroller, held] of marks) {
    const room = next.get(scroller)?.room ?? 0;
    if (room < held.room && standsInRoom(scroller, held.room - room)) kept.add(scroller);
  }
  for (const scroller of marks.keys()) {
    if (!next.has(scroller) && !kept.has(scroller)) unmark(scroller);
  }
  for (const [scroller, taken] of next) if (!kept.has(scroller)) mark(scroller, taken);
  listenWhileNeeded();
}

/** Asks for a pass at the next animation frame, once however often it is asked. */
function clearNextFrame(): void {
  if (clearFrame === 0) clearFrame = requestAnimationFrame(clearScrollers);
}

/** Asks for a pass that reads every mark afresh, after a window resize. */
function clearAfterResize(): void {
  resized = true;
  clearNextFrame();
}

/**
 * Listens for what moves content under the floats, and asks for a pass at each, while a float has
 * a box or a scroller still holds end room, and stops once neither holds (`FOOT_LAYERS` says
 * why): a pass leaves a scroller its room while the reader stands in it, and only a pass after
 * the scroll that leaves it takes it away.
 */
function listenWhileNeeded(): void {
  const wanted = placedFloats.length > 0 || marks.size > 0;
  if (wanted === listening) return;
  listening = wanted;
  if (wanted) {
    window.addEventListener("scroll", clearNextFrame, { capture: true, passive: true });
    window.addEventListener("resize", clearAfterResize);
    mutations ??= new MutationObserver(clearNextFrame);
    mutations.observe(document.body, { childList: true, subtree: true });
    return;
  }
  window.removeEventListener("scroll", clearNextFrame, { capture: true });
  window.removeEventListener("resize", clearAfterResize);
  mutations?.disconnect();
}

/**
 * Holds `placed`, every float that has a box, for the passes (`placedFloats`). While a float has a
 * box, asks for a pass at the next frame; once the last one has gone, runs the pass at once, so
 * a scroller whose reader is out of its room loses it at once and one whose reader stands in it
 * keeps it (`FOOT_LAYERS` says why).
 */
function followFloats(placed: readonly PlacedFloat[]): void {
  const hadFloats = placedFloats.length > 0;
  placedFloats = placed;
  if (placed.length > 0) {
    listenWhileNeeded();
    clearNextFrame();
    return;
  }
  if (!hadFloats) return;
  cancelAnimationFrame(clearFrame);
  clearScrollers();
}

/**
 * Publishes every layer's property, from the bottom up (`FOOT_LAYERS` says what each holds). The
 * obstacles start as the page bands. For each layer that has a property, it chooses that property
 * for the floats of the layer above (`freeInset`), writes it, and adds those floats' bands at that
 * property to the obstacles of the layers higher up. The gap is 1rem. Then it holds every float
 * at the properties it has just chosen, for the scrollers under the floats, and asks for a pass
 * over them while a float has a box, or runs one at once as the last float goes, which takes the
 * room the reader is out of (`followFloats`).
 *
 * It measures nothing. A published property moves the floats that read it, but the hook places a
 * float's band from that property, not from where the float is. No page element reads a layer's
 * property, so a published one moves none. End room in a scroller can move a page element the
 * scroller holds that is not fixed: that is why a scroller has a ceiling (`FOOT_LAYERS`).
 */
function publish(): void {
  const style = document.documentElement.style;
  const gap = remPixels();
  const screen = window.innerHeight;
  const obstacles: Band[] = [...pageBands.values()];
  const placed: PlacedFloat[] = [];
  for (const [index, { property }] of FOOT_LAYERS.entries()) {
    if (property === null) break;
    const floats = floatsOf(FOOT_LAYERS[index + 1].name);
    const inset = freeInset([...floats.values()], obstacles, gap, screen);
    writePixels(style, property, inset);
    for (const [element, float] of floats) {
      const band = floatBand(float, inset, gap);
      obstacles.push(band);
      placed.push({ band, element, left: float.left, right: float.right });
    }
  }
  followFloats(placed);
}

/**
 * While `active`, makes `ref`'s element a foot element of `layer` (`FOOT_LAYERS` says what that
 * means), and publishes every layer's property again each time it measures the element.
 *
 * A page element is measured whenever it or the screen changes size - a refusal's row grows a
 * composer upward, a widening moves it - and the hook holds its band: its box's top and bottom
 * edges, clipped to the screen, as offsets up from the screen's bottom edge, and whether it is
 * fixed, which a scroller's ceiling reads. It counts while it has a box on screen, however it is
 * placed, so it counts where it rests above the foot too - a sticky element at the end of its
 * scroll, or anything mid-screen under a short list - and the floats keep clear of it, since a
 * float lies over a foot element wherever their bands meet, not only where the element touches
 * the screen's bottom edge. An element sticky to the top (`position: sticky` with a `top` other
 * than `auto`) never counts, whatever its `bottom` says: the docked composer is `fixed` with a
 * `bottom` below `sm` and sticks to the top from `sm` up, keeping that `bottom`.
 *
 * A page element that is not fixed moves when its scroll container scrolls, and when the content
 * around it grows or shrinks - agents joining or leaving the Agents list - and neither a
 * `ResizeObserver` on the element nor a `window` resize reports either move. So while such an
 * element has a box and is not sticky to the top, the hook also measures it on any scroll of a
 * container that holds it, once per animation frame, and whenever its parent changes size: the
 * parent element itself, or, where that is `display: contents` and has no box, the nearest
 * ancestor that has one, found again at each measure, since a resize can change which that is.
 *
 * A float is measured only when its own size changes or the window resizes, and the hook holds
 * its height, its floor (`--foot-floor`) and its left and right edges, read again at each
 * measure, since a window resize can cross a breakpoint that changes the floor and moves the
 * edges. A float with no box (height 0) counts nothing.
 */
export function useFootInset(
  ref: RefObject<HTMLElement | null>,
  active: boolean,
  layer: FootLayer
): void {
  useLayoutEffect(() => {
    const element = ref.current;
    if (!active || element === null) return;
    const sizes = new ResizeObserver(() => measure());
    let frame = 0;
    const measureNextFrame = () => {
      if (frame !== 0) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        measure();
      });
    };
    // The scroll listener captures, since a scroll event does not bubble: it hears the scroll of
    // any container, and measures for one that holds the element. A page's own scroll targets the
    // document, which holds every element.
    const measureOnScroll = (event: Event) => {
      if (event.target instanceof Node && event.target.contains(element)) measureNextFrame();
    };
    let following = false;
    let parent: Element | null = null;
    const followMoves = (moves: boolean) => {
      if (moves !== following) {
        following = moves;
        if (moves) {
          window.addEventListener("scroll", measureOnScroll, { capture: true, passive: true });
        } else {
          window.removeEventListener("scroll", measureOnScroll, { capture: true });
        }
      }
      let boxed = moves ? element.parentElement : null;
      while (boxed !== null && getComputedStyle(boxed).display === "contents") {
        boxed = boxed.parentElement;
      }
      if (boxed === parent) return;
      if (parent !== null) sizes.unobserve(parent);
      if (boxed !== null) sizes.observe(boxed);
      parent = boxed;
    };
    const floats = floatsOf(layer);
    const measurePage = () => {
      const box = element.getBoundingClientRect();
      const { position, top } = getComputedStyle(element);
      const placed = box.height > 0 && !(position === "sticky" && top !== "auto");
      const fixed = position === "fixed";
      followMoves(placed && !fixed);
      const screen = window.innerHeight;
      if (placed && box.top < screen && box.bottom > 0) {
        pageBands.set(element, {
          fixed,
          hi: screen - Math.max(box.top, 0),
          lo: footOffset(box.bottom, screen),
        });
      } else {
        pageBands.delete(element);
      }
      publish();
    };
    const measureFloat = () => {
      const { height, left, right } = element.getBoundingClientRect();
      if (height > 0) floats.set(element, { floor: floorOf(element), height, left, right });
      else floats.delete(element);
      publish();
    };
    const measure = layer === "page" ? measurePage : measureFloat;
    sizes.observe(element);
    window.addEventListener("resize", measure);
    measure();
    return () => {
      followMoves(false);
      sizes.disconnect();
      window.removeEventListener("resize", measure);
      cancelAnimationFrame(frame);
      pageBands.delete(element);
      floats.delete(element);
      publish();
    };
  }, [active, layer, ref]);
}
