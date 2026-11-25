/** The probe of the foot of the screen (dispatch://INFRA-4731). It reads what the check
 *  `c-foot-clear` says, clause by clause. Each read of the page is self-contained, with no
 *  closure over this module, so Playwright can send it to the page as it stands, and the names by
 *  which the hook marks the page arrive as its argument (`FOOT_NAMES`): `await
 *  page.evaluate(readFootEnds, FOOT_NAMES)`, `await page.evaluate(readFootElements)`,
 *  `await page.evaluate(readKeptRoom, FOOT_NAMES)`, `await page.evaluate(readRoomAtStart,
 *  FOOT_NAMES)`, `await page.evaluate(readSidewaysBox, { after, code, toBanner })` and `await
 *  page.evaluate(sampleFootRest, { ms: 2000, names: FOOT_NAMES })`. Each reads the page. Only
 *  four write anything: `readFootEnds` and `readSidewaysBox` set scroll positions,
 *  `readRoomAtStart` scrolls every element back to its start, and `watchRoomHeld` keeps a record
 *  of its own in the page (`window.__footRoomHeld`).
 *
 *  The floats are Pending changes and the update banner, read by their accessible names, so a
 *  float with no box is left out. A "problem" is something a reader of the screen would see.
 *
 *  Where each clause of `c-foot-clear` is read:
 *  - "no float's box meets another foot element's box" and "no float covers the centre of a
 *    control, read where that centre lies inside every box that clips the control":
 *    `readFootElements`;
 *  - "at each scroller's real end (scrollHeight - scrollTop - clientHeight <= 1), no float covers
 *    its content: no leaf element of its own content, as far as it lies inside each box that
 *    clips it, the scroller's own box among them, meets a float's box on both axes", and "the
 *    content of a float, a scroller inside it included, is not read against the floats": the
 *    block "Cover" of `readFootEnds`;
 *  - "the shell's Navigation stays on the screen at the end of a page": the block "Navigation" of
 *    `readFootEnds`;
 *  - "a box that scrolls only sideways keeps its height under a float": `readSidewaysBox`;
 *  - "when a float goes, only a scroller whose reader stands in the end room it would lose keeps
 *    that room, no more than it held, ...": `readKeptRoom`, with `watchRoomHeld`;
 *  - "with no float shown and every scroller at its start, no scroller carries end room":
 *    `readRoomAtStart`;
 *  - "as a settled state: the same answer on two readings at least two animation frames apart":
 *    the block "Settled" at the end of each read above;
 *  - "a scroller is a box whose block overflow scrolls and that overflows or whose height the
 *    layout sets": `scrollsOnBlockAxis` in `readFootEnds`.
 *
 *  Beside the reads stand the helpers the rows that read the foot call (`e2e/foot-sweep.e2e.ts`,
 *  `e2e/issues-table.e2e.ts`, `e2e/agents-groups.e2e.ts`): `expectFootClear`, the reading of
 *  both reads while a float shows, against the foot elements `readFootElements` lists;
 *  `expectNoEndRoom`, the reading after the floats go; `navigationProblems`, the problems of a
 *  reading of the Navigation alone; and `footCellLine`, which writes the `FOOT-CELL` line a run's
 *  output carries for each reading. */
import { expect, type Page } from "@playwright/test";

import {
  CLEARANCE_PROPERTY,
  DOCUMENT_SCROLL_ATTRIBUTE,
  FOOT_LAYERS,
  MARK_ATTRIBUTE,
} from "../web/src/hooks/useFootInset";

/** The names by which the hook marks the page (`web/src/hooks/useFootInset.ts`): the attribute of
 *  a scroller that holds end room, the custom property that holds that room, the attribute of
 *  the element whose content the document scrolls, and the custom property of each layer of the
 *  foot that has one, from the bottom up. Each read of the page that reads a mark or a layer
 *  takes them as its argument. A read runs in the page and can import nothing there, and a name
 *  it spelled itself would, after a rename in the hook, find no mark and pass. */
export interface FootNames {
  readonly mark: string;
  readonly clearance: string;
  readonly documentScroll: string;
  readonly layers: readonly string[];
}

/** The hook's own names (`FootNames`), as the hook exports them. */
export const FOOT_NAMES: FootNames = {
  clearance: CLEARANCE_PROPERTY,
  documentScroll: DOCUMENT_SCROLL_ATTRIBUTE,
  layers: FOOT_LAYERS.flatMap(({ property }) => (property === null ? [] : [property])),
  mark: MARK_ATTRIBUTE,
};

/** A box of the page, as `getBoundingClientRect` answers it, cut to its four edges. */
interface Edges {
  readonly top: number;
  readonly bottom: number;
  readonly left: number;
  readonly right: number;
}

/** One scroller `readFootEnds` walked to its end: its name, its `data-testid` (null for the
 *  document, and for a scroller with none), the gap left to that end, its box as `[top, bottom]`,
 *  its mark and the room it holds, its last content where it has one, and each leaf of its own
 *  content that meets a float there (`covers`). Its last content is the lowest leaf of its own
 *  content on the screen inside its box: its text, its box as `[top, bottom]`, and `cell`,
 *  whether that leaf is in a cell of a table's row. */
export interface FootScroller {
  readonly name: string;
  readonly testid: string | null;
  readonly gap: number;
  readonly box: readonly [number, number];
  readonly mark: string;
  readonly last: {
    readonly text: string;
    readonly y: readonly [number, number];
    readonly cell: boolean;
  } | null;
  readonly covers: readonly string[];
}

/** The problems of a `readFootEnds` answer, by the clause of `c-foot-clear` each reads. */
export interface FootEndProblems {
  /** "no float covers its content": each scroller whose own content meets a float at its end. */
  readonly covers: string[];
  /** "at each scroller's real end": each scroller the passes left short of its end. */
  readonly short: string[];
  /** "the shell's Navigation stays on the screen at the end of a page". */
  readonly navigation: string[];
  /** "as a settled state": two readings two animation frames apart that differ. */
  readonly unsettled: string[];
}

/** What `readFootEnds` answers. */
export interface FootEnds {
  /** Each float shown, as `<name> y <top>-<bottom> x <left>-<right>`. */
  readonly floats: string[];
  /** The top of the shell's Navigation where it shows, or null. */
  readonly navigationTop: number | null;
  readonly scrollers: FootScroller[];
  readonly problems: FootEndProblems;
}

/** What `readFootElements` answers: each float and each foot element shown, by name, with its box
 *  as `[top, bottom]`, and the problems. */
export interface FootElements {
  readonly floats: Record<string, [number, number]>;
  readonly feet: Record<string, [number, number]>;
  readonly problems: string[];
}

/** What `readKeptRoom` answers: each scroller that keeps end room, with the room it keeps and
 *  where its reader stands, and the problems. */
export interface KeptRoom {
  readonly kept: string[];
  readonly problems: string[];
}

/** What `readSidewaysBox` answers: the code block's computed overflow on both axes, its height,
 *  and the top of the paragraph after it in the document, which no scroll moves; whether the
 *  update banner lies over the block; and whether two readings two animation frames apart gave
 *  the same answer. */
export interface SidewaysReading {
  readonly overflow: string;
  readonly height: number;
  readonly after: number;
  readonly under: boolean;
  readonly settled: boolean;
}

declare global {
  interface Window {
    /** The room each element that carries the hook's mark held in the last animation frame in
     *  which a float showed (`watchRoomHeld`). */
    __footRoomHeld?: { readonly held: Map<Element, number> };
  }
}

/**
 * Scrolls the document and every scroller to the real end of its scroll, up to six passes three
 * frames apart, so a virtualized list has drawn its last rows and end room has landed; the
 * passes end early once a pass finds every scroll at its end with the heights the pass before
 * found. Then reads, for each scroller, whether a float covers its content there; and the top of
 * the shell's Navigation. `names` are the hook's (`FOOT_NAMES`).
 *
 * A scroller is the spec's: a box whose block overflow scrolls (`overflow-y` auto or scroll) and
 * that overflows, or whose height the layout sets (`scrollsOnBlockAxis`, which reads it as the
 * hook reads it). The document is a scroller where it overflows, or where an element carries
 * `names.documentScroll`: that is the page's statement that the document scrolls that element's
 * content, which the hook reads in place of a height the layout sets, since no box metric tells a
 * bounded `main` from one the document scrolls (`FOOT_LAYERS` in `web/src/hooks/useFootInset.ts`,
 * "Why the page says where the document's content ends"). On a page that fills the screen and
 * states neither, the document scrolls nothing, and only the scrollers inside it are read.
 *
 * At its end, a scroller's own content is each element it holds that has a box, but these: an
 * element inside a float; an element fixed, or sticky with its top or bottom set, or inside one,
 * between it and the scroller, since the scroll does not move it; and an element inside a
 * scroller inside it, which is read at its own end. A box sticky only to the left or right moves
 * with the block scroll, so its elements are the scroller's own content. The document's own
 * content is that of `main` (`#main-content`). A leaf is an element of that content with no child
 * element that has a box. A cover is a leaf whose part inside each box that clips it - each box
 * between it and the scroller, on its chain of containing blocks, whose overflow is not
 * `visible`, the scroller's box and the screen - meets a float's box on both axes by more than
 * 1px. The answer names each scroller with its gap to the end, its box, its mark (`names.mark`
 * and the room it holds; the document's is that of the element that carries
 * `names.documentScroll`), its last content (the lowest leaf) and each cover.
 *
 * The content of a float, a scroller inside it included, is not read against the floats: a
 * scroller that a float holds, as Pending changes holds its list of refused changes, is none of
 * the scrollers here. The passes do not scroll it, and no reading reads its content. The hook
 * does the same: its hit test skips every element inside a float.
 *
 * The Navigation is read as a reader sees it: the column stuck in the Navigation's aside, and the
 * aside itself only where nothing sticks in it. The aside is as tall as the shell and scrolls
 * with the page, so its top leaves the screen on every page the document scrolls, while the
 * column stuck in it stays at y 0: the issues table at 1280x720 with no float shown scrolls
 * 116px, and the aside's top read -116 there. The stuck column's top goes below 0 only where the
 * shell ends above the foot of the screen, which is what end room below the shell does to it.
 *
 * A leaf is read looking through any `display: contents` child to its own children: such a child
 * has no box of its own, so a read of its box alone takes its parent for a leaf whatever it
 * holds. The new issue page's column holds the whole page through one, and was read at 360x640
 * as a leaf as tall as the screen that met both floats.
 *
 * What it does not see: where a scroller takes its room as bottom padding (`names.mark` is
 * `padding`), the page does not state its own padding apart from the room, so that padding is
 * read as its own. Such a scroller counts by one of the two ways all the same: its content then
 * overflows it, or the box is taller than its content and that padding together. And a box that
 * clips by `clip-path`, `clip` or paint containment, not by its overflow, is not read as a box
 * that clips, so the part it hides is read as a part a float can cover.
 */
export async function readFootEnds(names: FootNames): Promise<FootEnds> {
  const floatSelectors = [
    '[aria-label="Pending changes"]',
    '[aria-label="A new version is available"]',
  ];
  const meets = (a: Edges, b: Edges) =>
    Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 &&
    Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1;
  const frames = async (count: number) => {
    const { promise, resolve } = Promise.withResolvers<void>();
    let seen = 0;
    const tick = () => (++seen >= count ? resolve() : requestAnimationFrame(tick));
    requestAnimationFrame(tick);
    await promise;
  };
  // Scroller: "a box whose block overflow scrolls and that overflows or whose height the layout
  // sets". This is a copy of `contentFoot` and `scrollsOnBlockAxis` in
  // `web/src/hooks/useFootInset.ts`, read the same two ways, since a read in the page can import
  // nothing: change them together. The hook leaves its own room out of the overflow; here that is
  // the room `names.clearance` holds, the one room the page states apart.
  const contentFoot = (parent: Element, lowest: number): number | undefined => {
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
  };
  const scrollsOnBlockAxis = (element: Element): element is HTMLElement => {
    if (!(element instanceof HTMLElement) || element.clientHeight === 0) return false;
    const computed = getComputedStyle(element);
    if (computed.overflowY !== "auto" && computed.overflowY !== "scroll") return false;
    const { height, top } = element.getBoundingClientRect();
    if (height === 0) return false;
    // In the page, where this function runs: the DOM's own read of the mark at the instant of
    // this reading, which only says which room to leave out and waits for nothing.
    const way = element.getAttribute(names.mark);
    const room =
      way === "after" ? Number.parseFloat(element.style.getPropertyValue(names.clearance)) || 0 : 0;
    if (element.scrollHeight - room > element.clientHeight + 1) return true;
    const origin = top + element.clientTop - element.scrollTop;
    const foot = contentFoot(element, origin + (Number.parseFloat(computed.paddingTop) || 0));
    if (foot === undefined) return false;
    const own = Number.parseFloat(computed.paddingBottom) || 0;
    return element.clientHeight > foot - origin + own + 1;
  };
  // The floats shown, by their accessible names, so a float with no box is left out.
  const shownFloats = (): Element[] =>
    floatSelectors
      .map((selector) => document.querySelector(selector))
      .filter(
        (element): element is Element =>
          element !== null && element.getBoundingClientRect().height > 0
      );
  // Cover: "the content of a float, a scroller inside it included, is not read against the
  // floats". The scrollers are each scroller but one that a float holds (the doc comment above
  // says why), found before the passes, so that the passes do not scroll such a one.
  const scrollersOutside = (floatEls: readonly Element[]): HTMLElement[] =>
    [...document.querySelectorAll("*")].filter(
      (element): element is HTMLElement =>
        !floatEls.some((float) => float.contains(element)) && scrollsOnBlockAxis(element)
    );
  const scrollers = [document.documentElement, ...scrollersOutside(shownFloats())];
  let heights = "";
  for (let pass = 0; pass < 6; pass++) {
    for (const element of scrollers) element.scrollTop = element.scrollHeight;
    await frames(3);
    // Nothing is left to draw or to land once every scroll stands at its end with the heights
    // of the pass before.
    const now = scrollers.map((element) => element.scrollHeight).join(",");
    const atEnd = scrollers.every(
      (element) => element.scrollHeight - element.scrollTop - element.clientHeight <= 1
    );
    if (atEnd && now === heights) break;
    heights = now;
  }
  // Whether an element has a child with a box, looking through a `display: contents` child, which
  // has no box of its own, to its own children (the doc comment above says why).
  const holdsABox = (element: Element): boolean =>
    [...element.children].some((child) =>
      getComputedStyle(child).display === "contents"
        ? holdsABox(child)
        : child.getBoundingClientRect().height > 0
    );
  const read = (): FootEnds => {
    const floatEls = shownFloats();
    const floats = floatEls.map((element) => ({
      name: element.getAttribute("aria-label"),
      rect: element.getBoundingClientRect(),
    }));
    const inner = scrollersOutside(floatEls);
    // Scroller, for the document: it overflows, or the page states that it scrolls an element's
    // content (the doc comment above says why).
    const html = document.documentElement;
    const documentScrolls =
      html.scrollHeight > html.clientHeight + 1 ||
      document.querySelector(`[${names.documentScroll}]`) !== null;
    // Cover: "an element fixed, or sticky with its top or bottom set, which the scroll does not
    // move, and a scroller inside it, read at its own end, are not its own content, while the
    // leaf elements of a box sticky only to the left or right, which moves with the block
    // scroll, are". Whether the scroll leaves an element in place: it, or an element between it
    // and `stop`, is fixed or sticky on the block axis. Each element is read once per reading:
    // the walk below leaves out the content of each scroller inside the one it reads, so each
    // element has one `stop`, the scroller it is read for.
    const placed = new Map<Element, boolean>();
    const heldInPlace = (element: Element | null, stop: Element): boolean => {
      if (element === null || element === stop) return false;
      const known = placed.get(element);
      if (known !== undefined) return known;
      const { bottom, position, top } = getComputedStyle(element);
      const held =
        position === "fixed" ||
        (position === "sticky" && (top !== "auto" || bottom !== "auto")) ||
        heldInPlace(element.parentElement, stop);
      placed.set(element, held);
      return held;
    };
    // Clip: "inside each box that clips it". A box clips an element where it lies on the
    // element's chain of containing blocks: an element in flow is held by its parent, an absolute
    // one by the nearest box that is positioned or holds a fixed one, and a fixed one by the
    // nearest box that holds a fixed one (a transform of any kind, `perspective`, a filter or a
    // backdrop filter, layout or paint containment by `contain` or a size `container-type`, or a
    // `will-change` of one of those), else by the screen. Each box on that chain clips to its
    // padding box on each axis where its overflow is not `visible`, but not a box whose `display`
    // is inline or contents or a table's row or column, which takes no overflow, and not an SVG
    // element. The root's overflow, and the body's where the root's is `visible`, is the
    // screen's, which is read apart. `clipOf` answers the part of the screen that the boxes
    // between an element and `stop` (null: the document) leave it, with no edge where none clips
    // it. Each box is read once per reading, as in `heldInPlace`. `readFootElements` holds a copy
    // of this walk, since a read in the page can import nothing, so the two change together; the
    // copy has no `stop`, since it always reads up to the document.
    const open: Edges = { top: -Infinity, bottom: Infinity, left: -Infinity, right: Infinity };
    const cut = (a: Edges, b: Edges): Edges => ({
      top: Math.max(a.top, b.top),
      bottom: Math.min(a.bottom, b.bottom),
      left: Math.max(a.left, b.left),
      right: Math.min(a.right, b.right),
    });
    const rootStyle = getComputedStyle(document.documentElement);
    const screenClips = new Set<Element>([document.documentElement]);
    if (rootStyle.overflowX === "visible" && rootStyle.overflowY === "visible") {
      screenClips.add(document.body);
    }
    const holdsFixed = (style: CSSStyleDeclaration) =>
      [
        style.transform,
        style.translate,
        style.rotate,
        style.scale,
        style.perspective,
        style.filter,
        style.backdropFilter,
      ].some((value) => value !== "none") ||
      /layout|paint|strict|content/.test(style.contain) ||
      /size/.test(style.containerType) ||
      /transform|translate|rotate|scale|perspective|filter/.test(style.willChange);
    const rooms = new Map<Element, Edges>();
    const roomIn = (box: Element, stop: Element | null): Edges => {
      const known = rooms.get(box);
      if (known !== undefined) return known;
      const style = getComputedStyle(box);
      let room = clipOf(box, stop);
      if (
        box instanceof HTMLElement &&
        !screenClips.has(box) &&
        !/^(inline|contents)$|^table-(row|column|header|footer)/.test(style.display)
      ) {
        const { left, top } = box.getBoundingClientRect();
        const x = left + box.clientLeft;
        const y = top + box.clientTop;
        const clipsX = style.overflowX !== "visible";
        const clipsY = style.overflowY !== "visible";
        room = cut(room, {
          top: clipsY ? y : -Infinity,
          bottom: clipsY ? y + box.clientHeight : Infinity,
          left: clipsX ? x : -Infinity,
          right: clipsX ? x + box.clientWidth : Infinity,
        });
      }
      rooms.set(box, room);
      return room;
    };
    const clipOf = (element: Element, stop: Element | null): Edges => {
      const { position } = getComputedStyle(element);
      let holder = element.parentElement;
      if (position === "absolute" || position === "fixed") {
        while (holder !== null && holder !== stop) {
          const style = getComputedStyle(holder);
          if (holdsFixed(style) || (position === "absolute" && style.position !== "static")) break;
          holder = holder.parentElement;
        }
      }
      return holder === null || holder === stop ? open : roomIn(holder, stop);
    };
    const leftOut = new Set<Element>([...floatEls, ...inner]);
    const answers = [...(documentScrolls ? [html] : []), ...inner].map((scroller): FootScroller => {
      const isDocument = scroller === document.documentElement;
      const box = isDocument
        ? { top: 0, bottom: innerHeight, left: 0, right: innerWidth }
        : scroller.getBoundingClientRect();
      // The part of the screen inside the scroller's box, where a float can cover its content.
      const inside = cut(box, { top: 0, bottom: innerHeight, left: 0, right: innerWidth });
      const root = isDocument
        ? (document.getElementById("main-content") ?? document.body)
        : scroller;
      const stop = isDocument ? document.body : scroller;
      // Cover: "no leaf element of its own content, as far as it lies inside each box that clips
      // it, the scroller's own box among them, meets a float's box on both axes". Every leaf is
      // read. The walk leaves out each float and each scroller inside this one, with all they
      // hold; each element it reaches has its box read once, and the walks up to the scroller
      // come last.
      let last: {
        readonly rect: DOMRect;
        readonly text: string;
        readonly cell: boolean;
      } | null = null;
      const covers: string[] = [];
      const walk = document.createTreeWalker(root, NodeFilter.SHOW_ELEMENT, {
        acceptNode: (node) =>
          node !== scroller && leftOut.has(node as Element)
            ? NodeFilter.FILTER_REJECT
            : NodeFilter.FILTER_ACCEPT,
      });
      for (let node = walk.nextNode(); node !== null; node = walk.nextNode()) {
        const leaf = node as Element;
        const rect = leaf.getBoundingClientRect();
        if (rect.height === 0 || rect.width === 0) continue;
        const seen = cut(rect, inside);
        if (seen.bottom <= seen.top || seen.right <= seen.left) continue;
        if (holdsABox(leaf)) continue;
        if (heldInPlace(leaf, stop)) continue;
        const part = cut(seen, clipOf(leaf, isDocument ? null : scroller));
        if (part.bottom <= part.top || part.right <= part.left) continue;
        const text = (leaf.textContent || leaf.getAttribute("aria-label") || leaf.tagName)
          .trim()
          .slice(0, 40);
        if (last === null || rect.bottom > last.rect.bottom) {
          last = { cell: leaf.closest("tr > td, tr > th") !== null, rect, text };
        }
        for (const float of floats) {
          if (!meets(part, float.rect)) continue;
          covers.push(
            `"${text}" at y ${Math.round(part.top)}-${Math.round(part.bottom)} ` +
              `x ${Math.round(part.left)}-${Math.round(part.right)} meets ${float.name} at ` +
              `y ${Math.round(float.rect.top)}-${Math.round(float.rect.bottom)}`
          );
        }
      }
      const mark = isDocument
        ? [...document.querySelectorAll<HTMLElement>(`[${names.documentScroll}]`)]
            .filter((element) => element.hasAttribute(names.mark))
            .map((element) => `${element.tagName}:${element.style.paddingBottom}`)
            .join(",")
        : scroller.hasAttribute(names.mark)
          ? `${scroller.getAttribute(names.mark)}:${
              scroller.style.getPropertyValue(names.clearance) || scroller.style.paddingBottom
            }`
          : "";
      // In the page, where this function runs: the DOM's own read of the scroller's label at the
      // instant of this reading, which only names the scroller in a problem and waits for nothing.
      const label = scroller.getAttribute("aria-label");
      const testid = isDocument ? null : (scroller.dataset.testid ?? null);
      const name = isDocument
        ? "the document"
        : `${scroller.tagName}${scroller.id ? `#${scroller.id}` : ""}${label ? `[${label}]` : ""}${
            testid ? `(${testid})` : ""
          }`;
      return {
        name,
        testid,
        gap: Math.round(scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight),
        box: [Math.round(box.top), Math.round(box.bottom)],
        mark,
        last:
          last === null
            ? null
            : {
                text: last.text,
                y: [Math.round(last.rect.top), Math.round(last.rect.bottom)],
                cell: last.cell,
              },
        covers,
      };
    });
    // Navigation: "the shell's Navigation stays on the screen at the end of a page".
    const navigation = document.querySelector('aside[aria-label="Navigation"]');
    const column =
      navigation === null
        ? null
        : ([...navigation.children].find(
            (child) => getComputedStyle(child).position === "sticky"
          ) ?? navigation);
    const navRect = column?.getBoundingClientRect();
    const navigationTop = navRect && navRect.height > 0 ? Math.round(navRect.top) : null;
    const problems: FootEndProblems = {
      covers: answers
        .filter((answer) => answer.covers.length > 0)
        .map(
          (answer) =>
            `${answer.name}'s own content meets a float at its end: ${answer.covers
              .slice(0, 3)
              .join(
                "; "
              )}${answer.covers.length > 3 ? `; and ${answer.covers.length - 3} more` : ""}`
        ),
      short: answers
        .filter((answer) => answer.gap > 1)
        .map((answer) => `${answer.name} did not reach its end (${answer.gap}px left)`),
      navigation:
        navigationTop !== null && navigationTop < 0
          ? [`the Navigation's top is at y ${navigationTop} at the end of the scroll`]
          : [],
      unsettled: [],
    };
    return {
      floats: floats.map(
        (float) =>
          `${float.name} y ${Math.round(float.rect.top)}-${Math.round(float.rect.bottom)} x ${Math.round(float.rect.left)}-${Math.round(float.rect.right)}`
      ),
      navigationTop,
      scrollers: answers,
      problems,
    };
  };
  // Settled: "the same answer on two readings at least two animation frames apart". The answer
  // is each float's box, the Navigation's top, each scroller as read and the problems. Where the
  // two readings differ, the answer is the second one, with a problem that says what changed.
  const pieces = (reading: FootEnds) => [
    `floats ${reading.floats.join(", ") || "none"}`,
    `the Navigation's top ${reading.navigationTop ?? "not shown"}`,
    ...reading.scrollers.map(
      (scroller) =>
        `${scroller.name} ${scroller.gap}px from its end, box y ${scroller.box.join("-")}, ` +
        `mark ${scroller.mark || "none"}, last content y ${scroller.last?.y.join("-") ?? "none"}, ` +
        `${scroller.covers.length} covers`
    ),
    // Every clause's problems, so that a clause added to `FootEndProblems` is compared too.
    // `read()` leaves `unsettled` empty: only the comparison below fills it.
    ...Object.values(reading.problems).flat(),
  ];
  const first = read();
  await frames(2);
  const second = read();
  const before = pieces(first);
  const after = pieces(second);
  const went = before.filter((piece) => !after.includes(piece));
  const came = after.filter((piece) => !before.includes(piece));
  if (went.length === 0 && came.length === 0) return second;
  return {
    ...second,
    problems: {
      ...second.problems,
      unsettled: [
        `the foot was not settled: of two readings two animation frames apart, the first read ` +
          `${went.slice(0, 3).join("; ") || "nothing more"} and the second ` +
          `${came.slice(0, 3).join("; ") || "nothing more"}`,
      ],
    },
  };
}

/**
 * Reads the floats against the foot elements it lists, each by a name and a CSS selector, as the
 * page stands, with no scroll: a float meeting a foot element, the two floats meeting each other,
 * a float off the screen, and every control of a float or a foot element whose centre another
 * element covers (`elementFromPoint`), read where that centre lies inside every box that clips
 * the control. A control whose centre a box clips away, as Pending changes' list of refused
 * changes scrolls its rows out of its view, is not read. A trial click is no proof of this: one
 * passed over a control a float covered.
 */
export async function readFootElements(): Promise<FootElements> {
  const meets = (a: Edges, b: Edges) =>
    Math.min(a.right, b.right) - Math.max(a.left, b.left) > 1 &&
    Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1;
  const shown = (element: Element | null): element is Element =>
    element !== null && element.getBoundingClientRect().height > 0;
  const read = (): FootElements => {
    const floats = Object.entries({
      "Pending changes": '[aria-label="Pending changes"]',
      "the update banner": '[aria-label="A new version is available"]',
    })
      .map(([name, selector]) => [name, document.querySelector(selector)] as const)
      .filter((entry): entry is readonly [string, Element] => shown(entry[1]));
    // Every foot element of the page layer a page of the rows that read the foot can show
    // (`FOOT_LAYERS` in `web/src/hooks/useFootInset.ts`), each by a selector that matches it and
    // no other element of those pages, and the Conversation's and the live view's `Jump to latest`.
    // `readFootElements` leaves out each one a page does not show. The selection bar is the one
    // toolbar with no name: the new issue page's view row is a toolbar named by its controls. The
    // live view's rows - its row of pickers and its switch lines - stand under its composer,
    // outside `agent-composer`, in the box `AgentConversationPage` registers. On a wide screen the
    // Agents page's composer, its queued sends above it, or both stand in the stuck box
    // `BroadcastFoot` registers, which the selector finds by either; on a narrow or short screen
    // that box is `display: contents`, has no box and shows nothing here, and the composer sticks
    // by itself. `expectFootClear` reads all of them.
    const feet = Object.entries({
      "the selection bar": '[role="toolbar"]:not([aria-label])',
      "the broadcast composer": 'section[aria-label="Broadcast"]',
      "the broadcast foot":
        'div.sticky.bottom-0:has(> section[aria-label="Broadcast"], > section[aria-label="Sends"])',
      "the comment composer": 'form[aria-label="Comment composer"]',
      "the margin's sheet": 'aside[aria-label="Review panel"]',
      "the live view's composer": '[data-testid="agent-composer"]',
      "the live view's rows": '[data-testid="agent-composer-footer"]',
      "Jump to latest": '[data-testid="jump-to-latest"]',
    })
      .map(([name, selector]) => [name, document.querySelector(selector)] as const)
      .filter((entry): entry is readonly [string, Element] => shown(entry[1]));
    const problems: string[] = [];
    // "no float's box meets another foot element's box", read against each foot element and
    // the other float, and no float leaves the screen.
    for (const [name, element] of floats) {
      const rect = element.getBoundingClientRect();
      if (rect.top < 0 || rect.bottom > innerHeight) problems.push(`${name} is off the screen`);
      for (const [foot, footElement] of feet) {
        if (meets(rect, footElement.getBoundingClientRect())) {
          problems.push(`${name} meets ${foot}`);
        }
      }
    }
    if (
      floats.length === 2 &&
      meets(floats[0][1].getBoundingClientRect(), floats[1][1].getBoundingClientRect())
    ) {
      problems.push("Pending changes meets the update banner");
    }
    // Clip: "inside every box that clips the control". A box clips an element where it lies on
    // the element's chain of containing blocks: an element in flow is held by its parent, an
    // absolute one by the nearest box that is positioned or holds a fixed one, and a fixed one by
    // the nearest box that holds a fixed one (a transform of any kind, `perspective`, a filter or
    // a backdrop filter, layout or paint containment by `contain` or a size `container-type`, or
    // a `will-change` of one of those), else by the screen. Each box on that chain clips to its
    // padding box on each axis where its overflow is not `visible`, but not a box whose
    // `display` is inline or contents or a table's row or column, which takes no overflow, and
    // not an SVG element. The root's overflow, and the body's where the root's is `visible`, is
    // the screen's, which is read apart. `clipOf` answers the part of the screen that the boxes
    // between an element and the document leave it, with no edge where none clips it. Each box
    // is read once per reading. This is the walk with which `readFootEnds` clips each leaf,
    // written again here, since a read in the page can import nothing, but with no `stop`, since
    // this copy always reads up to the document.
    const open: Edges = { top: -Infinity, bottom: Infinity, left: -Infinity, right: Infinity };
    const cut = (a: Edges, b: Edges): Edges => ({
      top: Math.max(a.top, b.top),
      bottom: Math.min(a.bottom, b.bottom),
      left: Math.max(a.left, b.left),
      right: Math.min(a.right, b.right),
    });
    const rootStyle = getComputedStyle(document.documentElement);
    const screenClips = new Set<Element>([document.documentElement]);
    if (rootStyle.overflowX === "visible" && rootStyle.overflowY === "visible") {
      screenClips.add(document.body);
    }
    const holdsFixed = (style: CSSStyleDeclaration) =>
      [
        style.transform,
        style.translate,
        style.rotate,
        style.scale,
        style.perspective,
        style.filter,
        style.backdropFilter,
      ].some((value) => value !== "none") ||
      /layout|paint|strict|content/.test(style.contain) ||
      /size/.test(style.containerType) ||
      /transform|translate|rotate|scale|perspective|filter/.test(style.willChange);
    const rooms = new Map<Element, Edges>();
    const roomIn = (box: Element): Edges => {
      const known = rooms.get(box);
      if (known !== undefined) return known;
      const style = getComputedStyle(box);
      let room = clipOf(box);
      if (
        box instanceof HTMLElement &&
        !screenClips.has(box) &&
        !/^(inline|contents)$|^table-(row|column|header|footer)/.test(style.display)
      ) {
        const { left, top } = box.getBoundingClientRect();
        const x = left + box.clientLeft;
        const y = top + box.clientTop;
        const clipsX = style.overflowX !== "visible";
        const clipsY = style.overflowY !== "visible";
        room = cut(room, {
          top: clipsY ? y : -Infinity,
          bottom: clipsY ? y + box.clientHeight : Infinity,
          left: clipsX ? x : -Infinity,
          right: clipsX ? x + box.clientWidth : Infinity,
        });
      }
      rooms.set(box, room);
      return room;
    };
    const clipOf = (element: Element): Edges => {
      const { position } = getComputedStyle(element);
      let holder = element.parentElement;
      if (position === "absolute" || position === "fixed") {
        while (holder !== null) {
          const style = getComputedStyle(holder);
          if (holdsFixed(style) || (position === "absolute" && style.position !== "static")) break;
          holder = holder.parentElement;
        }
      }
      return holder === null ? open : roomIn(holder);
    };
    // "no float covers the centre of a control, read where that centre lies inside every box
    // that clips the control", read for every control of each float and each foot element on the
    // screen. A control of a scroller's content at its end is a leaf that `readFootEnds` reads,
    // by its part inside each box that clips it.
    for (const [name, root] of [...floats, ...feet]) {
      for (const control of root.querySelectorAll(
        "button, select, textarea, input:not([type=hidden]), a[href]"
      )) {
        const rect = control.getBoundingClientRect();
        if (rect.width === 0 || rect.height === 0) continue;
        const x = rect.left + rect.width / 2;
        const y = rect.top + rect.height / 2;
        if (y < 0 || y >= innerHeight || x < 0 || x >= innerWidth) continue;
        const clip = clipOf(control);
        if (y < clip.top || y >= clip.bottom || x < clip.left || x >= clip.right) continue;
        const hit = document.elementFromPoint(x, y);
        if (hit && !(control === hit || control.contains(hit) || hit.contains(control))) {
          const label = (
            control.getAttribute("aria-label") ||
            control.textContent ||
            control.tagName
          )
            .trim()
            .slice(0, 30);
          const by = hit.closest("[aria-label]")?.getAttribute("aria-label") || hit.tagName;
          problems.push(`${name}'s control "${label}" is covered by ${by}`);
        }
      }
    }
    const boxes = (entries: readonly (readonly [string, Element])[]) =>
      Object.fromEntries(
        entries.map(([name, element]): [string, [number, number]] => {
          const rect = element.getBoundingClientRect();
          return [name, [Math.round(rect.top), Math.round(rect.bottom)]];
        })
      );
    return { floats: boxes(floats), feet: boxes(feet), problems };
  };
  // Settled: "the same answer on two readings at least two animation frames apart". The answer
  // is each float's box, each foot element's box and the problems; where the two readings
  // differ, the answer is the second one, with a problem that says what changed.
  const pieces = (reading: FootElements) => [
    ...Object.entries({ ...reading.floats, ...reading.feet }).map(
      ([name, [top, bottom]]) => `${name} at y ${top}-${bottom}`
    ),
    ...reading.problems,
  ];
  const first = read();
  const frames = Promise.withResolvers<void>();
  requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
  await frames.promise;
  const second = read();
  const before = pieces(first);
  const after = pieces(second);
  const went = before.filter((piece) => !after.includes(piece));
  const came = after.filter((piece) => !before.includes(piece));
  if (went.length === 0 && came.length === 0) return second;
  return {
    ...second,
    problems: [
      ...second.problems,
      `the foot elements were not settled: of two readings two animation frames apart, the ` +
        `first read ${went.slice(0, 3).join("; ") || "nothing more"} and the second ` +
        `${came.slice(0, 3).join("; ") || "nothing more"}`,
    ],
  };
}

/**
 * Keeps, in the page, the room each element that carries `names.mark` holds in each animation
 * frame in which a float shows (Pending changes or the update banner), so that once the floats
 * go, `readKeptRoom` reads "no more than it held" against the room each held in the last frame
 * a float showed. The room is the custom property `names.clearance` where the mark is `after`,
 * and the inline bottom padding where it is `padding`. It runs from its first call until the page
 * unloads, and a second call on the same page does nothing. `expectFootClear` calls it.
 */
export function watchRoomHeld(names: FootNames): void {
  if (window.__footRoomHeld !== undefined) return;
  const held = new Map<Element, number>();
  window.__footRoomHeld = { held };
  // Each float's element, looked up again only where the one found before is gone from the page.
  const floats = [
    '[aria-label="Pending changes"]',
    '[aria-label="A new version is available"]',
  ].map((selector) => ({ selector, element: null as Element | null }));
  const frame = () => {
    const floatShown = floats.some((float) => {
      if (float.element === null || !float.element.isConnected) {
        float.element = document.querySelector(float.selector);
      }
      return (float.element?.getBoundingClientRect().height ?? 0) > 0;
    });
    if (floatShown) {
      held.clear();
      for (const element of document.querySelectorAll<HTMLElement>(`[${names.mark}]`)) {
        // In the page, where this function runs: the DOM's own read of the mark in this frame,
        // which only says where the room is written and waits for nothing.
        const way = element.getAttribute(names.mark);
        const room =
          way === "after"
            ? element.style.getPropertyValue(names.clearance)
            : element.style.paddingBottom;
        held.set(element, Number.parseFloat(room) || 0);
      }
    }
    requestAnimationFrame(frame);
  };
  frame();
}

/**
 * Kept room, read as the page stands, where the floats left the reader: "when a float goes, only
 * a scroller whose reader stands in the end room it would lose keeps that room, no more than it
 * held, and the document's scroller also while a box it holds would be clamped without that
 * room, each until a scroll leaves it". So each element that carries `names.mark` is a problem
 * unless the reader stands in its room - the scroll that room lengthens (the document's for the
 * element that carries `names.documentScroll`, its own otherwise) is off its start with less than
 * that room left to its end - or, for the document's scroller, a box it holds would be clamped
 * without it: a box whose block overflow scrolls, off its start with less than that room left to
 * its end. And each is a problem where it keeps more room than it held in the last frame a float
 * showed (`watchRoomHeld`), or room it did not hold then.
 *
 * While a float of the foot's top layer shows - the Conversation's `Jump to latest` or the chord
 * indicator - the hook takes room for it as for any float, so that room is no kept room, and this
 * read reads none. `readRoomAtStart` then reads every mark at the start of every scroll, where
 * `Jump to latest` hides.
 *
 * What it does not see: where a scroller takes its room as bottom padding (the document's
 * scroller always does), the page does not state its own padding apart from the room, so its
 * whole inline padding is read as the room. A reader at the end of the scroll stands in both, and
 * a reader at its start in neither, which are the two places the rows leave the reader.
 */
export async function readKeptRoom(names: FootNames): Promise<KeptRoom> {
  const roomHeld = window.__footRoomHeld?.held;
  const read = (): KeptRoom => {
    const topFloat = [
      ...document.querySelectorAll(
        '[data-testid="jump-to-latest"], [data-testid="chord-indicator"]'
      ),
    ].some(
      (element) =>
        getComputedStyle(element).position === "fixed" && element.getBoundingClientRect().height > 0
    );
    if (topFloat) return { kept: [], problems: [] };
    const kept: string[] = [];
    const problems: string[] = [];
    for (const element of document.querySelectorAll<HTMLElement>(`[${names.mark}]`)) {
      const name = `${element.tagName}${element.id ? `#${element.id}` : ""}${
        element.dataset.testid ? `(${element.dataset.testid})` : ""
      }`;
      // In the page, where this function runs: the DOM's own read of the mark at the instant of
      // this reading, which only says where the room is written and waits for nothing.
      const way = element.getAttribute(names.mark);
      const room =
        Number.parseFloat(
          way === "after"
            ? element.style.getPropertyValue(names.clearance)
            : element.style.paddingBottom
        ) || 0;
      const byDocument = element.hasAttribute(names.documentScroll);
      const scroll = byDocument ? document.documentElement : element;
      const gap = Math.round(scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight);
      const stands = scroll.scrollTop > 0 && gap < room;
      // Whether a box would be clamped without the room counts only where the reader is out of it.
      const clamps =
        !stands &&
        byDocument &&
        Array.prototype.some.call(
          element.querySelectorAll<HTMLElement>("*"),
          (box: HTMLElement) =>
            box.scrollTop > 0 &&
            box.scrollHeight - box.scrollTop - box.clientHeight < room &&
            /^(auto|scroll)$/.test(getComputedStyle(box).overflowY)
        );
      const held = roomHeld?.get(element);
      kept.push(`${name} ${room}px, its reader ${gap}px from its end`);
      if (!stands && !clamps) {
        problems.push(
          `${name} keeps ${room}px of end room while its reader is out of it: its scroll is ` +
            `${Math.round(scroll.scrollTop)}px from its start and ${gap}px from its end` +
            (byDocument ? ", and no box it holds would be clamped without it" : "")
        );
      }
      if (held === undefined) {
        problems.push(`${name} keeps ${room}px of end room it did not hold while a float showed`);
      } else if (room > held + 0.5) {
        problems.push(
          `${name} keeps ${room}px of end room, more than the ${held}px it held while a float showed`
        );
      }
    }
    return { kept, problems };
  };
  // Settled: "the same answer on two readings at least two animation frames apart"; where the
  // two readings differ, the answer is the second one, with a problem that says what changed.
  const first = read();
  const frames = Promise.withResolvers<void>();
  requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
  await frames.promise;
  const second = read();
  const before = [...first.kept, ...first.problems];
  const after = [...second.kept, ...second.problems];
  const went = before.filter((piece) => !after.includes(piece));
  const came = after.filter((piece) => !before.includes(piece));
  if (went.length === 0 && came.length === 0) return second;
  return {
    ...second,
    problems: [
      ...second.problems,
      `the kept room was not settled: of two readings two animation frames apart, the first ` +
        `read ${went.slice(0, 3).join("; ") || "nothing more"} and the second ` +
        `${came.slice(0, 3).join("; ") || "nothing more"}`,
    ],
  };
}

/**
 * No room at the start: "with no float shown and every scroller at its start, no scroller
 * carries end room". Scrolls the document and every element back to the start of its scroll,
 * waits two frames, so the foot's pass for that scroll has run, and answers each element that
 * still carries `names.mark`. The start is out of every scroller's room, so the hook keeps none
 * there. It is also where the Conversation's `Jump to latest`, a float of the foot's top layer,
 * is gone: the Conversation lists its newest turn first, so the button shows once the reader
 * leaves the top, and the document takes room for it there, as the rule asks. A mark read while
 * it shows says so.
 */
export async function readRoomAtStart(names: FootNames): Promise<string[]> {
  const frames = async () => {
    const { promise, resolve } = Promise.withResolvers<void>();
    requestAnimationFrame(() => requestAnimationFrame(() => resolve()));
    await promise;
  };
  for (const element of document.querySelectorAll("*")) {
    if (element.scrollTop > 0) element.scrollTop = 0;
  }
  await frames();
  const read = () => {
    const jump = document.querySelector('[data-testid="jump-to-latest"]');
    const jumpShown = jump !== null && jump.getBoundingClientRect().height > 0;
    return [...document.querySelectorAll(`[${names.mark}]`)].map(
      (element) =>
        `${element.tagName}${element.id ? `#${element.id}` : ""} carries ${names.mark}` +
        (jumpShown ? " while Jump to latest shows" : "")
    );
  };
  // Settled: "the same answer on two readings at least two animation frames apart".
  const first = read();
  await frames();
  const second = read();
  return first.join("|") === second.join("|")
    ? second
    : [
        ...second,
        `the marks were not settled: two readings two animation frames apart read ` +
          `${first.join(", ") || "none"} and then ${second.join(", ") || "none"}`,
      ];
}

/**
 * A box that scrolls only sideways: "a box that scrolls only sideways keeps its height under a
 * float". Reads the first `pre` that holds `code`, and the paragraph whose text is `after`: the
 * block's computed overflow on both axes, its height, the top of that paragraph in the document,
 * and whether the update banner lies over the block (`SidewaysReading`). Where `toBanner`, first
 * scrolls the document so the block's middle stands at the banner's. Settled: two readings two
 * animation frames apart, and `settled` says whether they gave the same answer.
 */
export async function readSidewaysBox({
  after,
  code,
  toBanner,
}: {
  readonly after: string;
  readonly code: string;
  readonly toBanner: boolean;
}): Promise<SidewaysReading> {
  const block = [...document.querySelectorAll("pre")].find((pre) =>
    (pre.textContent ?? "").includes(code)
  );
  const next = [...document.querySelectorAll("p")].find(
    (paragraph) => paragraph.textContent === after
  );
  if (block === undefined || next === undefined) {
    return {
      after: Number.NaN,
      height: Number.NaN,
      overflow: "no block",
      settled: true,
      under: false,
    };
  }
  const banner = document.querySelector('[aria-label="A new version is available"]');
  if (toBanner && banner !== null) {
    const box = block.getBoundingClientRect();
    const bannerBox = banner.getBoundingClientRect();
    window.scrollBy(0, box.top + box.height / 2 - (bannerBox.top + bannerBox.height / 2));
  }
  const read = () => {
    const box = block.getBoundingClientRect();
    const bannerBox = banner?.getBoundingClientRect();
    const { overflowX, overflowY } = getComputedStyle(block);
    const under =
      bannerBox !== undefined &&
      bannerBox.height > 0 &&
      Math.min(box.right, bannerBox.right) - Math.max(box.left, bannerBox.left) > 1 &&
      Math.min(box.bottom, bannerBox.bottom) - Math.max(box.top, bannerBox.top) > 1;
    return {
      after: Math.round(next.getBoundingClientRect().top + window.scrollY),
      height: Math.round(box.height),
      overflow: `${overflowX}/${overflowY}`,
      under,
    };
  };
  // Settled: "the same answer on two readings at least two animation frames apart".
  const first = read();
  const frames = Promise.withResolvers<void>();
  requestAnimationFrame(() => requestAnimationFrame(() => frames.resolve()));
  await frames.promise;
  const second = read();
  return { ...second, settled: JSON.stringify(first) === JSON.stringify(second) };
}

/**
 * Samples the foot every 100 ms for `ms` milliseconds: each layer property of the foot
 * (`names.layers`) on the document element, the document's height, the scroll, and every mark
 * (`names.mark`) with the room it holds. Answers the distinct states seen; a page at rest answers
 * one.
 */
export async function sampleFootRest({
  ms,
  names,
}: {
  readonly ms: number;
  readonly names: FootNames;
}): Promise<string[]> {
  const seen = new Set<string>();
  const start = performance.now();
  while (performance.now() - start < ms) {
    const css = getComputedStyle(document.documentElement);
    const marks = [...document.querySelectorAll<HTMLElement>(`[${names.mark}]`)]
      .map(
        (element) =>
          `${element.tagName}:${element.getAttribute(names.mark)}:${
            element.style.getPropertyValue(names.clearance) || element.style.paddingBottom
          }`
      )
      .join(",");
    seen.add(
      [
        ...names.layers.map((property) => css.getPropertyValue(property)),
        document.documentElement.scrollHeight,
        Math.round(scrollY),
        marks,
      ].join("|")
    );
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  return [...seen];
}

/** The line a run's output carries for one reading: `FOOT-CELL <cell> clean`, or the problems
 *  of that reading after `<cell>`, joined by ` | `. `cell` is the reading's page, its size and
 *  the floats it shows, `<page> <WxH> <floats>`, each one word. */
export function footCellLine(cell: string, problems: readonly string[]): string {
  return `FOOT-CELL ${cell} ${problems.length === 0 ? "clean" : problems.join(" | ")}`;
}

/** The problems of `ends` that a row which reads only the Navigation takes: one where the probe
 *  finds no Navigation that shows, the Navigation's own, each of a scroller that stopped short of
 *  its end, and the one of two readings that differ, since the Navigation is read at the end of
 *  every scroll, as a settled state. */
export function navigationProblems(ends: FootEnds): string[] {
  return [
    ...(ends.navigationTop === null ? ["the Navigation does not show"] : []),
    ...ends.problems.short,
    ...ends.problems.navigation,
    ...ends.problems.unsettled,
  ];
}

/**
 * Fails unless the foot of `page` is clear: one `expect.poll` that takes both reads until
 * neither answers a problem, of any clause, `readFootEnds` first, so `readFootElements` reads
 * every foot element it lists as the page stands at the end of every scroll, where the
 * last content of each scroller is. Each read is settled itself: it answers a problem unless two
 * readings two animation frames apart give the same answer, so a foot that flips between clean
 * and not clean does not pass. `message` names the reading, `<page> <WxH> <floats>`; the poll
 * fails under it, with the problems of the last reading, and the reading's `FOOT-CELL` line
 * (`footCellLine`) is printed either way. Answers the last `readFootEnds` answer, which a row
 * reads for a scroller it names. First starts `watchRoomHeld` on the page, so `expectNoEndRoom`
 * reads the room each scroller held while a float showed.
 */
export async function expectFootClear(page: Page, message: string): Promise<FootEnds> {
  let ends: FootEnds = {
    floats: [],
    navigationTop: null,
    scrollers: [],
    problems: { covers: [], short: [], navigation: [], unsettled: [] },
  };
  let problems: readonly string[] = ["the foot was never read"];
  try {
    await page.evaluate(watchRoomHeld, FOOT_NAMES);
    await expect
      .poll(
        async () => {
          ends = await page.evaluate(readFootEnds, FOOT_NAMES);
          const elements = await page.evaluate(readFootElements);
          problems = [...Object.values(ends.problems).flat(), ...elements.problems];
          return problems;
        },
        { message }
      )
      .toEqual([]);
  } finally {
    console.log(footCellLine(message, problems));
  }
  return ends;
}

/**
 * Fails unless the end room of `page` reads as `c-foot-clear` says once the floats have gone, in
 * its two halves. First, as the page stands, where the floats left the reader: only a scroller
 * whose reader stands in its room, or the document's scroller while a box it holds would be
 * clamped without it, keeps room, and none keeps more than it held while a float showed
 * (`readKeptRoom`). Then, with the document and every scroller back at the start of its scroll,
 * no element carries `MARK_ATTRIBUTE` (`readRoomAtStart`). Each half is read until it holds.
 * `cell` names the reading in its `FOOT-CELL` line, which is printed either way, with the
 * problems of the half that failed, and after it each room the first half read as kept.
 */
export async function expectNoEndRoom(page: Page, cell: string): Promise<void> {
  let problems: readonly string[] = ["the page was never read"];
  let kept: readonly string[] = [];
  try {
    await expect
      .poll(
        async () => {
          const reading = await page.evaluate(readKeptRoom, FOOT_NAMES);
          kept = reading.kept;
          problems = reading.problems;
          return problems;
        },
        {
          message: `${cell}: only a scroller whose reader stands in its room keeps it, no more than it held`,
        }
      )
      .toEqual([]);
    await expect
      .poll(
        async () => {
          problems = await page.evaluate(readRoomAtStart, FOOT_NAMES);
          return problems;
        },
        {
          message: `${cell}: no element carries ${MARK_ATTRIBUTE} with every scroller at its start`,
        }
      )
      .toEqual([]);
  } finally {
    console.log(
      `${footCellLine(cell, problems)}${kept.length > 0 ? ` (kept: ${kept.join(", ")})` : ""}`
    );
  }
}
