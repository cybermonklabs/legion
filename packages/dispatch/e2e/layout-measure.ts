/** The layout rule's instrument (`packages/dispatch/AGENTS.md`, "Layout"; dispatch://INFRA-5200):
 *  what a page breaks of the rule, read off the live DOM. `measureLayout` runs inside the page
 *  (`page.evaluate(measureLayout, listed)`), so it is self-contained: it closes over nothing and
 *  imports nothing, and every helper is declared inside it. It reads sideways only: a box cut at
 *  its bottom, or one that scrolls down, is no breach of the rule.
 *
 *  It reports four kinds of breach:
 *  - `page-scroll`: the document is wider than the viewport, so the page scrolls sideways. One
 *    breach for each outermost element that runs past the viewport's right edge with no ancestor
 *    to hold it - an element inside a box that clips or scrolls is that box's, never the page's.
 *  - `box-scroll`: a box whose `overflow-x` is `auto` or `scroll` holds content wider than
 *    itself, so the box scrolls sideways. It is exempt only when everything that runs past its
 *    edges is, or sits in, a code block (`pre`, `code`), a picture (`img`, `picture`, `video`,
 *    `canvas`), a data table (`table`, `role=table`, `role=grid`) or a board's lanes (a box marked
 *    `data-layout-scroll="board"`), or when the box is one of those itself: a code block
 *    scrolling its own lines, a data table's own box, the board box. Anything else that overflows
 *    beside them - a row of chips next to a table, an icon's `svg`, the box's own text - makes it
 *    a breach.
 *  - `clipped`: something is cut off at a side with no sign of it, in either of two ways. An
 *    element runs past the left or right edge of the ancestor that clips it (`overflow-x`
 *    `hidden` or `clip`): only the outermost such element is reported, since its children are cut
 *    because it is, and a cut an ellipsis or a line clamp of that ancestor shows is none - unless
 *    the element is a block, which an ellipsis never marks. Or a box that clips holds more than
 *    its width of its own content (`scrollWidth` past `clientWidth`), which is how a line of text
 *    is cut, with no `text-overflow: ellipsis` and no line clamp. A screen-reader-only element,
 *    clipped to a pixel on purpose, is skipped, and so is an element whose containing block is
 *    outside the ancestor, which does not clip it (an absolutely positioned element above its
 *    positioned ancestor, a fixed one).
 *  - `group-over-cap`: a group marked `data-layout-group` shows more entries than its
 *    `data-layout-max` (10 when unset), not counting the one control marked `data-layout-more`
 *    that opens the rest, unless the reader opened it (`data-layout-open`).
 *
 *  `listed` is a list of CSS selectors, the `element` of each known breach the caller may match
 *  (`e2e/layout-breaches.ts`). Each breach answers in `listed` the selectors its element sits in
 *  (`closest`); a page-scroll breach answers those of every element past the viewport inside it
 *  too, since the cause of a page's overflow is usually deeper than the outermost element that
 *  carries it.
 *
 *  `elements` is how many elements it read, so a caller can refuse a blank page: a page that drew
 *  nothing breaks nothing, and that is not a pass. `groups` names every `data-layout-group` on the
 *  page, so a caller can require the groups a page must mark. */
export interface LayoutBreach {
  readonly kind: "page-scroll" | "box-scroll" | "clipped" | "group-over-cap";
  /** A short CSS path to the element, the outermost of its four last parts first. */
  readonly where: string;
  /** The numbers: widths and edges in CSS pixels, or the entry count and the cap. */
  readonly detail: string;
  /** The element's own text, cut to 80 characters, so a reader can find it on the screen. */
  readonly text: string;
  /** The selectors of the caller's `listed` this breach sits in. */
  readonly listed: readonly string[];
  /** A stable locator a breach list entry may name it by: the nearest `data-testid`,
   *  `data-layout-group` or `aria-label` on its element or above it, else the element's path. A
   *  page-scroll breach is named by the deepest element past the viewport, its cause. */
  readonly locator: string;
}

/** The viewport widths the layout rule holds a page to: a phone, a tablet, the wide layout's
 *  first width (`WIDE_LAYOUT_MIN_PX` in `e2e/review-routes.ts`), a laptop and a desktop. The rule
 *  holds at every width from 360 to 1920 px; these are the widths the test samples. */
export const LAYOUT_WIDTHS = [360, 900, 1280, 1440, 1920] as const;
export type LayoutWidth = (typeof LAYOUT_WIDTHS)[number];

export interface LayoutReading {
  /** The width of the viewport the page was read at, in CSS pixels. */
  readonly viewport: number;
  /** How many elements under `body` it read. */
  readonly elements: number;
  /** The name of every `data-layout-group` on the page, once each. */
  readonly groups: readonly string[];
  readonly breaches: readonly LayoutBreach[];
}

export function measureLayout(listed: readonly string[] = []): LayoutReading {
  const root = document.documentElement;
  const viewport = root.clientWidth;
  /** A code block or a picture: its own width is its content's, and it may be cut. */
  const CODE_OR_PICTURE = "pre, code, img, picture, svg, canvas, video";
  /** What a box may scroll sideways for: a code block, a picture, a data table, a board's lanes.
   *  An `svg` is no picture here: inline, it is an icon in a row of controls. */
  const SCROLLS_FOR =
    "pre, code, img, picture, video, canvas, table, [role='table'], [role='grid'], [data-layout-scroll='board']";
  /** One pixel of slack on every edge: a box's edges are fractional, its scroll sizes whole. */
  const SLACK = 1;

  const label = (el: Element): string => {
    const id = el.id === "" ? "" : `#${el.id}`;
    const testId = el.getAttribute("data-testid");
    const name = el.getAttribute("aria-label");
    const group = el.getAttribute("data-layout-group");
    return (
      el.tagName.toLowerCase() +
      id +
      (testId === null ? "" : `[data-testid="${testId}"]`) +
      (group === null ? "" : `[data-layout-group="${group}"]`) +
      (name === null ? "" : `[aria-label="${name.slice(0, 40)}"]`)
    );
  };
  const path = (el: Element): string => {
    const parts: string[] = [];
    for (
      let at: Element | null = el;
      at !== null && at !== document.body && parts.length < 4;
      at = at.parentElement
    ) {
      parts.unshift(label(at));
    }
    return parts.join(" > ");
  };
  const textOf = (el: Element): string =>
    ((el as HTMLElement).innerText ?? el.textContent ?? "")
      .replace(/\s+/g, " ")
      .trim()
      .slice(0, 80);
  const listedFor = (elements: readonly Element[]): string[] =>
    listed.filter((selector) => elements.some((el) => el.closest(selector) !== null));
  const locatorOf = (el: Element): string => {
    const attributes = ["data-testid", "data-layout-group", "aria-label"];
    const named = el.closest(attributes.map((attribute) => `[${attribute}]`).join(", "));
    for (const attribute of attributes) {
      const value = named?.getAttribute(attribute);
      if (value !== null && value !== undefined) return `[${attribute}=${JSON.stringify(value)}]`;
    }
    return path(el);
  };

  const all = [...document.body.querySelectorAll("*")];
  const styles = new Map<Element, CSSStyleDeclaration>();
  for (const el of all) styles.set(el, getComputedStyle(el));
  const styleOf = (el: Element): CSSStyleDeclaration => styles.get(el) ?? getComputedStyle(el);
  const shown = (el: Element): boolean => {
    const style = styleOf(el);
    return (
      style.display !== "none" && style.visibility !== "hidden" && el.getClientRects().length > 0
    );
  };
  /** A screen-reader-only element is clipped to a pixel on purpose. */
  const visuallyHidden = (el: Element): boolean => {
    const box = el.getBoundingClientRect();
    return box.width <= SLACK && box.height <= SLACK;
  };
  /** Whether a cut of `el`'s own content shows: an ellipsis or a line clamp. */
  const marksItsCut = (style: CSSStyleDeclaration): boolean =>
    style.textOverflow === "ellipsis" ||
    (style.webkitLineClamp !== "none" && style.webkitLineClamp !== "");
  /** The nearest ancestor that clips `el` sideways (`overflow-x` other than `visible`), read
   *  along `el`'s containing blocks: an ancestor between an absolutely positioned element and
   *  its containing block - the nearest positioned, transformed, filtered or contained one -
   *  does not clip it, and nothing above a fixed one does. `null` when nothing clips it. */
  const clipperOf = (el: Element): Element | null => {
    let style = styleOf(el);
    if (style.position === "fixed") return null;
    let needsContainingBlock = style.position === "absolute";
    for (let at = el.parentElement; at !== null && at !== document.body; at = at.parentElement) {
      style = styleOf(at);
      if (needsContainingBlock) {
        const containing =
          style.position !== "static" ||
          style.transform !== "none" ||
          style.filter !== "none" ||
          style.perspective !== "none" ||
          /paint|layout|strict|content/.test(style.contain);
        if (!containing) continue;
        needsContainingBlock = false;
      }
      if (style.overflowX !== "visible") return at;
      if (style.position === "fixed") return null;
      if (style.position === "absolute") needsContainingBlock = true;
    }
    return null;
  };
  /** The outermost of `elements`: those whose parent is not among them. */
  const outermostOf = (elements: readonly Element[]): Element[] => {
    const among = new Set(elements);
    return elements.filter((el) => el.parentElement === null || !among.has(el.parentElement));
  };

  const breaches: LayoutBreach[] = [];

  // A page that scrolls sideways: the outermost elements past the viewport that no box holds.
  const pageWidth = Math.max(root.scrollWidth, document.body.scrollWidth);
  if (pageWidth > viewport + SLACK) {
    const past = all.filter(
      (el) =>
        shown(el) && el.getBoundingClientRect().right > viewport + SLACK && clipperOf(el) === null
    );
    const outermost = outermostOf(past);
    for (const el of outermost) {
      const inside = past.filter((other) => el.contains(other));
      const deepest = inside.filter(
        (other) => !inside.some((inner) => inner !== other && other.contains(inner))
      );
      breaches.push({
        detail:
          `page ${pageWidth}px in a ${viewport}px viewport; this element ends at ` +
          `${Math.round(el.getBoundingClientRect().right)}px; deepest: ` +
          deepest
            .slice(0, 3)
            .map((other) => `${label(other)} ${JSON.stringify(textOf(other).slice(0, 40))}`)
            .join(", "),
        kind: "page-scroll",
        listed: listedFor(inside),
        locator: locatorOf(deepest[0] ?? el),
        text: textOf(el),
        where: path(el),
      });
    }
    if (outermost.length === 0) {
      breaches.push({
        detail: `page ${pageWidth}px in a ${viewport}px viewport, and no element ends past it`,
        kind: "page-scroll",
        listed: listedFor([root]),
        locator: "html",
        text: "",
        where: "html",
      });
    }
  }

  // A box that scrolls sideways for anything but a code block, a picture, a data table or a
  // board's lanes running past its edges.
  const scrollers = new Set<Element>();
  for (const el of all) {
    const style = styleOf(el);
    const scrolls = style.overflowX === "auto" || style.overflowX === "scroll";
    if (!scrolls || !shown(el) || visuallyHidden(el)) continue;
    if (el.scrollWidth <= el.clientWidth + SLACK) continue;
    scrollers.add(el);
    // The box is the exempt element itself: a code block scrolling its own lines, a data table
    // or a board's lanes.
    if (el.closest("pre, code") !== null || el.matches(SCROLLS_FOR)) continue;
    const box = el.getBoundingClientRect();
    const left = box.left + el.clientLeft;
    const right = left + el.clientWidth;
    const runsPast = (edges: DOMRect, contentRight = edges.right): boolean =>
      contentRight > right + SLACK || edges.left < left - SLACK;
    /** Whether a line of text directly inside `holder` runs past the box. */
    const textRunsPast = (holder: Element): boolean =>
      [...holder.childNodes].some((node) => {
        if (node.nodeType !== Node.TEXT_NODE || (node.textContent ?? "").trim() === "")
          return false;
        const range = document.createRange();
        range.selectNodeContents(node);
        return runsPast(range.getBoundingClientRect());
      });
    // An element runs past the box when its own box does, or when its content does: a block
    // that does not clip holds text wider than itself, which is how a `pre` overflows.
    const overflowing = [...el.querySelectorAll("*")].filter((inner) => {
      if (!shown(inner) || clipperOf(inner) !== el) return false;
      const edges = inner.getBoundingClientRect();
      return styleOf(inner).overflowX === "visible"
        ? runsPast(edges, Math.max(edges.right, edges.left + inner.scrollWidth))
        : runsPast(edges);
    });
    // What overflows is the deepest of them, and any whose own text runs past: an element that
    // runs past only because a child does is that child's wrapper, not the cause.
    const sources = overflowing.filter(
      (inner) =>
        !overflowing.some((other) => other !== inner && inner.contains(other)) ||
        textRunsPast(inner)
    );
    const ownText = textRunsPast(el);
    const forItsTables =
      !ownText &&
      sources.length > 0 &&
      sources.every((inner) => inner.closest(SCROLLS_FOR) !== null);
    if (forItsTables) continue;
    const causes = sources.filter((inner) => inner.closest(SCROLLS_FOR) === null);
    breaches.push({
      detail:
        `holds ${el.scrollWidth}px in a ${el.clientWidth}px box; past its edges: ` +
        [...(ownText || sources.length === 0 ? ["its own text"] : []), ...causes.map(label)]
          .slice(0, 3)
          .join(", "),
      kind: "box-scroll",
      listed: listedFor([el]),
      locator: locatorOf(el),
      text: textOf(el),
      where: path(el),
    });
  }

  // An element cut off by the box that clips it, with no ellipsis to say so.
  const cut = new Set<Element>();
  const cutInside = new Set<Element>();
  for (const el of all) {
    if (!shown(el) || visuallyHidden(el) || el.closest(CODE_OR_PICTURE) !== null) continue;
    const clipper = clipperOf(el);
    if (clipper === null || scrollers.has(clipper) || visuallyHidden(clipper)) continue;
    const box = el.getBoundingClientRect();
    if (box.width === 0) continue;
    const edge = clipper.getBoundingClientRect();
    if (box.right <= edge.right + SLACK && box.left >= edge.left - SLACK) continue;
    cut.add(el);
    // Its parent is cut too, inside the same box: the parent is the breach, not this.
    const parent = el.parentElement;
    if (parent !== null && parent !== clipper && cut.has(parent)) continue;
    cutInside.add(clipper);
    const style = styleOf(el);
    const inline = style.display.startsWith("inline");
    if (style.textOverflow === "ellipsis" || (inline && marksItsCut(styleOf(clipper)))) continue;
    breaches.push({
      detail: `spans ${Math.round(box.left)}-${Math.round(box.right)}px inside ${path(clipper)} at ${Math.round(edge.left)}-${Math.round(edge.right)}px`,
      kind: "clipped",
      listed: listedFor([el]),
      locator: locatorOf(el),
      text: textOf(el),
      where: path(el),
    });
  }

  // A box that clips its own text, with no ellipsis to say so. A box whose cut elements were
  // judged above is not read again: the cut is theirs.
  for (const el of all) {
    const style = styleOf(el);
    if (style.overflowX !== "hidden" && style.overflowX !== "clip") continue;
    if (cutInside.has(el) || !shown(el) || visuallyHidden(el) || marksItsCut(style)) continue;
    if (el.closest(CODE_OR_PICTURE) !== null) continue;
    if (el.scrollWidth <= el.clientWidth + SLACK) continue;
    breaches.push({
      detail: `its content is ${el.scrollWidth}px in a ${el.clientWidth}px box`,
      kind: "clipped",
      listed: listedFor([el]),
      locator: locatorOf(el),
      text: textOf(el),
      where: path(el),
    });
  }

  // A group that shows more entries than its cap, unless the reader opened it with its "+N"
  // control (`data-layout-open`): then it shows every entry by the reader's own choice. The
  // group itself may draw no box (`display: contents`), so only its entries are asked whether
  // they are shown.
  const groups = new Set<string>();
  for (const group of document.querySelectorAll("[data-layout-group]")) {
    groups.add(group.getAttribute("data-layout-group") ?? "");
    if (group.hasAttribute("data-layout-open")) continue;
    const max = Number(group.getAttribute("data-layout-max") ?? "10");
    const entries = [...group.children].filter(
      (child) => shown(child) && !child.hasAttribute("data-layout-more")
    ).length;
    if (entries > max) {
      breaches.push({
        detail: `shows ${entries} entries; the cap is ${max}`,
        kind: "group-over-cap",
        listed: listedFor([group]),
        locator: locatorOf(group),
        text: textOf(group),
        where: path(group),
      });
    }
  }

  return { breaches, elements: all.length, groups: [...groups], viewport };
}
