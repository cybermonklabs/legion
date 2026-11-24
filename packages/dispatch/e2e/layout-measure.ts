/** The layout rule's instrument (`packages/dispatch/AGENTS.md`, "Layout"; dispatch://INFRA-5200):
 *  what a page breaks of the rule, read off the live DOM. `measureLayout` runs inside the page
 *  (`page.evaluate(measureLayout)`), so it is self-contained: it closes over nothing and
 *  imports nothing, and every helper is declared inside it. It reads sideways only: a box cut at
 *  its bottom, or one that scrolls down, is no breach of the rule.
 *
 *  It reports four kinds of breach:
 *  - `page-scroll`: the document is wider than the viewport, so the page scrolls sideways. One
 *    breach for each outermost element that runs past the viewport's right edge with no ancestor
 *    to hold it - an element inside a box that clips or scrolls is that box's, never the page's,
 *    and one on a fixed layer adds nothing to the page's width.
 *  - `box-scroll`: a box whose `overflow-x` is `auto` or `scroll` holds content wider than
 *    itself, so the box scrolls sideways. It is exempt only when everything that runs past its
 *    edges is, or sits in, a code block (`pre`; a `code` in a line of prose is none), a data table
 *    (`table`, `role=table`, `role=grid`), a board's lanes (a box marked
 *    `data-layout-scroll="board"`) or one picture (`img`, `picture`, `video`, `canvas`) the box
 *    scrolls no further than (with its own padding), or when the box is a code block scrolling its
 *    own lines, a data table's own box or the board box. Anything else that overflows - a row of
 *    chips next to a table, a strip of pictures narrow or wide, a picture pushed past the box by
 *    what stands beside it, an icon's `svg`, the box's own text - makes it a breach.
 *  - `clipped`: something is cut off at a side with no sign of it, in one of four ways.
 *    - An element runs past the left or right edge of the ancestor that clips it (`overflow-x`
 *      `hidden` or `clip`): only the outermost such element is reported, since its children are
 *      cut because it is. That ancestor's ellipsis excuses a plain `inline` element that draws
 *      in the ancestor's own lines - every element between them is `inline` too - whose text it
 *      stands in for, and nothing else: a span in a block or a flex item draws in that box's
 *      lines, which the ellipsis does not reach (`text-overflow` is not inherited), a chip
 *      (`inline-flex`), an `inline-block` or a block that runs past is pushed out of its row
 *      (point 2 of the rule), a picture loses the part the ellipsis does not stand in for, and an
 *      element's own ellipsis is cut away with the rest. A line clamp excuses only its last
 *      visible line, and only when it hides a line below the box: only then does it draw its
 *      ellipsis there.
 *    - A box that clips holds more than its width of its own content (`scrollWidth` past
 *      `clientWidth`), which is how a line of text is cut, with no `text-overflow: ellipsis`; or,
 *      for a line-clamped box, a line runs past its side that the clamp's ellipsis does not mark:
 *      one above its last visible line, or any line when the clamp hides none. A code block is
 *      such a box too: it may scroll its lines, not cut them.
 *    - An element that no ancestor clips, or its own text where it does not clip that text
 *      itself (`overflow-x: visible`), starts left of the viewport, where no scroll reaches.
 *    - An element on a fixed layer, which no ancestor clips, or its own text where it does not
 *      clip that text itself, runs past either side of the viewport: a fixed layer adds nothing
 *      to the page's width, so it is cut, not scrolled.
 *    Only the outermost element cut at the viewport is reported, and only one partly in view:
 *    one wholly outside it is not drawn at all. Skipped: a screen-reader-only element (a box of a
 *    pixel, an empty `clip`, `clip-path: inset(50%)`), what a picture holds and what a code block
 *    that clips or scrolls its own lines holds, which is that block's or that picture's to cut (a
 *    code block whose overflow is visible cuts nothing, so what it holds is read as any element),
 *    a picture's own drawing (an `svg` cuts what it draws past its box), and an element whose
 *    containing block is outside the ancestor, which does not clip it (an absolutely positioned
 *    element above its positioned ancestor, a fixed one).
 *  - `group-over-cap`: a group marked `data-layout-group` shows more entries than its
 *    `data-layout-max` (10 when unset), unless the reader opened it (`data-layout-open`). Each
 *    direct child the group draws is one entry, but for the one control marked
 *    `data-layout-more` that opens the rest. It is a breach too, however many it shows, when it
 *    declares a cap that is not a whole number from 1 to 10, or when more than one of its children
 *    is marked `data-layout-more`.
 *
 *  An element is read only where it draws (`checkVisibility`): the content of a closed
 *  `<details>` keeps a box in Chromium, and is no breach.
 *
 *  Each breach names its element by a `locator`, which an entry of `e2e/layout-breaches.ts`
 *  names exactly: the nearest `data-testid`, `data-layout-group` or `aria-label` on the element
 *  or above it, else the element's path.
 *
 *  `elements` and `contentElements` count what it read, the page and the page's own region, so a
 *  caller can refuse a blank page: a page that drew nothing breaks nothing, and that is not a
 *  pass. `groups` names every `data-layout-group` on the page that shows one of its entries, so
 *  a caller can require the groups a page must mark: a hidden copy of a list marks nothing. */
export interface LayoutBreach {
  readonly kind: "page-scroll" | "box-scroll" | "clipped" | "group-over-cap";
  /** A short CSS path to the element, the outermost of its four last parts first. */
  readonly where: string;
  /** The numbers: widths and edges in CSS pixels, or the entry count and the cap. */
  readonly detail: string;
  /** The element's own text, cut to 80 characters, so a reader can find it on the screen. */
  readonly text: string;
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
  /** How many of them are in the page's own region (`[data-testid="main-content"]`), the shell
   *  around it left out: what a route's floor (`minElements`) is read against. */
  readonly contentElements: number;
  /** The name of every `data-layout-group` on the page, once each. */
  readonly groups: readonly string[];
  readonly breaches: readonly LayoutBreach[];
}

export function measureLayout(): LayoutReading {
  const root = document.documentElement;
  const viewport = root.clientWidth;
  const PICTURE = "img, picture, video, canvas";
  const PICTURE_OR_ICON = `${PICTURE}, svg`;
  const CODE_OR_PICTURE = `pre, ${PICTURE_OR_ICON}`;
  const SCROLLS_FOR = "pre, table, [role='table'], [role='grid'], [data-layout-scroll='board']";
  /** A line's or a box's horizontal extent. */
  type Span = { readonly left: number; readonly right: number };
  /** One pixel of slack on every edge: a box's edges are fractional, its scroll sizes whole. */
  const SLACK = 1;

  const label = (el: Element): string => {
    const id = el.id === "" ? "" : `#${el.id}`;
    // The DOM's own attribute, read in the page by `page.evaluate` after the row settled.
    const testId = el.getAttribute("data-testid");
    // The DOM's own attribute, read in the page by `page.evaluate` after the row settled.
    const name = el.getAttribute("aria-label");
    // The DOM's own attribute, read in the page by `page.evaluate` after the row settled.
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
  const locatorOf = (el: Element): string => {
    const attributes = ["data-testid", "data-layout-group", "aria-label"];
    const named = el.closest(attributes.map((attribute) => `[${attribute}]`).join(", "));
    for (const attribute of attributes) {
      // The DOM's own attribute, read in the page by `page.evaluate` after the row settled.
      const value = named?.getAttribute(attribute);
      if (value !== null && value !== undefined) return `[${attribute}=${JSON.stringify(value)}]`;
    }
    return path(el);
  };

  const all = [...document.body.querySelectorAll("*")];
  const styles = new Map<Element, CSSStyleDeclaration>();
  for (const el of all) styles.set(el, getComputedStyle(el));
  const styleOf = (el: Element): CSSStyleDeclaration => styles.get(el) ?? getComputedStyle(el);
  /** Whether `el` draws: not `display: none` and not hidden, by `visibility` or by a closed
   *  `<details>` (whose content keeps a box in Chromium, so a box alone does not say it shows). */
  const shown = (el: Element): boolean =>
    el.getClientRects().length > 0 && el.checkVisibility({ visibilityProperty: true });
  /** A screen-reader-only element is clipped to a pixel on purpose. */
  const visuallyHidden = (el: Element): boolean => {
    const box = el.getBoundingClientRect();
    return (
      (box.width <= SLACK && box.height <= SLACK) ||
      // Cut to an empty `clip`: Tailwind's `sr-only` on a link that also takes a touch target's
      // size, such as the shell's "Skip to content" until it is focused.
      styleOf(el).clip === "rect(0px, 0px, 0px, 0px)" ||
      styleOf(el).clipPath === "inset(50%)"
    );
  };
  /** Whether `style` clamps its box's lines (`-webkit-line-clamp`), which draws its ellipsis on
   *  the last visible line only. */
  const clamps = (style: CSSStyleDeclaration): boolean =>
    style.webkitLineClamp !== "none" && style.webkitLineClamp !== "";
  /** What holds `el` sideways, read along its containing blocks: the nearest ancestor that clips
   *  it (`overflow-x` other than `visible`), else `"fixed"` when it is, or sits on, a fixed layer,
   *  else `"page"`. An ancestor between an absolutely positioned element and its containing block
   *  - the nearest positioned, transformed, filtered or contained one - does not clip it, and
   *  nothing above a fixed one does. */
  const holderOf = (el: Element): Element | "fixed" | "page" => {
    let style = styleOf(el);
    if (style.position === "fixed") return "fixed";
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
      if (style.position === "fixed") return "fixed";
      if (style.position === "absolute") needsContainingBlock = true;
    }
    return "page";
  };
  const clipperOf = (el: Element): Element | null => {
    const holder = holderOf(el);
    return typeof holder === "string" ? null : holder;
  };
  /** Where the text directly inside `el` (not its children's) draws, from its leftmost to its
   *  rightmost line, or `null` when `el` holds none. */
  const ownTextSpan = (el: Element): Span | null => {
    let left = Number.POSITIVE_INFINITY;
    let right = Number.NEGATIVE_INFINITY;
    for (const node of el.childNodes) {
      if (node.nodeType !== Node.TEXT_NODE || (node.textContent ?? "").trim() === "") continue;
      const range = document.createRange();
      range.selectNodeContents(node);
      for (const rect of range.getClientRects()) {
        if (rect.width === 0) continue;
        left = Math.min(left, rect.left);
        right = Math.max(right, rect.right);
      }
    }
    return left === Number.POSITIVE_INFINITY ? null : { left, right };
  };
  /** Where `el`'s own text draws when `el` does not clip that text itself (`overflow-x:
   *  visible`) and the text does not `fit`; else `null`. Text an element clips is that element's
   *  own content, which the own-content pass reads. */
  const ownTextOutside = (el: Element, fits: (span: Span) => boolean): Span | null => {
    if (styleOf(el).overflowX !== "visible") return null;
    const span = ownTextSpan(el);
    return span === null || fits(span) ? null : span;
  };
  /** Whether `el` is what a picture holds, or what a code block that clips or scrolls its own
   *  lines holds: that picture's or that block's to cut. A code block whose overflow is visible
   *  cuts nothing, so what it holds is cut, if at all, by the box that clips it. */
  const heldByCodeOrPicture = (el: Element): boolean => {
    const holder = el.parentElement?.closest(CODE_OR_PICTURE);
    if (holder === null || holder === undefined) return false;
    return holder.matches(PICTURE_OR_ICON) || styleOf(holder).overflowX !== "visible";
  };
  /** Whether `el` and every element between it and `clipper` is `display: inline` (or draws no
   *  box of its own, `display: contents`): only then does `el` draw in `clipper`'s own lines,
   *  where `clipper`'s ellipsis stands. A span in a block, or in a flex item, draws in that box's
   *  lines, and `text-overflow` is not inherited. */
  const inLinesOf = (el: Element, clipper: Element): boolean => {
    for (let at: Element | null = el; at !== null && at !== clipper; at = at.parentElement) {
      const display = styleOf(at).display;
      if (display !== "inline" && display !== "contents") return false;
    }
    return true;
  };
  /** Whether line-clamped `box` cuts `node`'s text at its side with no sign. The clamp draws its
   *  ellipsis on its last visible line, and only when it hides a line below the box
   *  (`scrollHeight` past `clientHeight`): so a line above the last that runs past the box's side
   *  is cut bare, and so is the last line when the clamp hides nothing. A line is read off the
   *  text's own rects (a `Range`), and one below the box's bottom edge is the clamp's to hide,
   *  not a sideways cut. */
  const clampCutsBare = (box: Element, node: Node): boolean => {
    const edge = box.getBoundingClientRect();
    const hidesALine = box.scrollHeight > box.clientHeight + SLACK;
    const visibleRects = (of: Node): DOMRect[] => {
      const range = document.createRange();
      range.selectNodeContents(of);
      return [...range.getClientRects()].filter(
        (rect) => rect.width > 0 && rect.top < edge.bottom - SLACK && rect.bottom > edge.top + SLACK
      );
    };
    const lines = visibleRects(box);
    if (lines.length === 0) return false;
    const lastTop = Math.max(...lines.map((rect) => rect.top));
    return visibleRects(node).some(
      (rect) =>
        (!hidesALine || rect.bottom <= lastTop + SLACK) &&
        (rect.right > edge.right + SLACK || rect.left < edge.left - SLACK)
    );
  };
  const outermostOf = (elements: readonly Element[]): Element[] => {
    const among = new Set(elements);
    return elements.filter((el) => el.parentElement === null || !among.has(el.parentElement));
  };

  const breaches: LayoutBreach[] = [];
  const report = (
    kind: LayoutBreach["kind"],
    el: Element,
    detail: string,
    locator = locatorOf(el)
  ): void => {
    breaches.push({ detail, kind, locator, text: textOf(el), where: path(el) });
  };

  // A page that scrolls sideways: the outermost elements past the viewport that no box holds. An
  // element on a fixed layer adds nothing to the page's width; the viewport pass below reads it.
  const pageWidth = Math.max(root.scrollWidth, document.body.scrollWidth);
  if (pageWidth > viewport + SLACK) {
    const past = all.filter(
      (el) =>
        shown(el) &&
        el.getBoundingClientRect().right + window.scrollX > viewport + SLACK &&
        holderOf(el) === "page"
    );
    const outermost = outermostOf(past);
    for (const el of outermost) {
      const inside = past.filter((other) => el.contains(other));
      const deepest = inside.filter(
        (other) => !inside.some((inner) => inner !== other && other.contains(inner))
      );
      report(
        "page-scroll",
        el,
        `page ${pageWidth}px in a ${viewport}px viewport; this element ends at ` +
          `${Math.round(el.getBoundingClientRect().right)}px; deepest: ` +
          deepest
            .slice(0, 3)
            .map((other) => `${label(other)} ${JSON.stringify(textOf(other).slice(0, 40))}`)
            .join(", "),
        locatorOf(deepest[0] ?? el)
      );
    }
    if (outermost.length === 0) {
      report(
        "page-scroll",
        root,
        `page ${pageWidth}px in a ${viewport}px viewport, and no element ends past it`,
        "html"
      );
    }
  }

  const scrollers = new Set<Element>();
  for (const el of all) {
    const style = styleOf(el);
    const scrolls = style.overflowX === "auto" || style.overflowX === "scroll";
    if (!scrolls || !shown(el) || visuallyHidden(el)) continue;
    if (el.scrollWidth <= el.clientWidth + SLACK) continue;
    scrollers.add(el);
    if (el.closest("pre") !== null || el.matches(SCROLLS_FOR)) continue;
    const box = el.getBoundingClientRect();
    const left = box.left + el.clientLeft;
    const right = left + el.clientWidth;
    const runsPast = (edges: Span, contentRight = edges.right): boolean =>
      contentRight > right + SLACK || edges.left < left - SLACK;
    const textRunsPast = (holder: Element): boolean => {
      const text = ownTextSpan(holder);
      return text !== null && runsPast(text);
    };
    // An element runs past the box when its own box does, or when its content does: a block
    // that does not clip holds text wider than itself, which is how a `pre` overflows.
    const overflowingDescendants = [...el.querySelectorAll("*")].filter((inner) => {
      if (!shown(inner) || clipperOf(inner) !== el) return false;
      const edges = inner.getBoundingClientRect();
      return styleOf(inner).overflowX === "visible"
        ? runsPast(edges, Math.max(edges.right, edges.left + inner.scrollWidth))
        : runsPast(edges);
    });
    const overflowSources = overflowingDescendants.filter(
      (inner) =>
        !overflowingDescendants.some((other) => other !== inner && inner.contains(other)) ||
        textRunsPast(inner)
    );
    const ownText = textRunsPast(el);
    // Exempt only what a code block, a data table or a board's lanes inside this box hold: a
    // strip in a table's cell or a board's card is no table and no board scrolling in its own box.
    const isExemptOverflowSource = (inner: Element): boolean => {
      const holder = inner.closest(SCROLLS_FOR);
      if (holder !== null && el.contains(holder)) return true;
      const picture = inner.closest(PICTURE);
      if (picture === null) return false;
      const style = styleOf(el);
      const padding = Number.parseFloat(style.paddingLeft) + Number.parseFloat(style.paddingRight);
      return el.scrollWidth <= picture.getBoundingClientRect().width + padding + SLACK;
    };
    const allOverflowIsExempt =
      !ownText && overflowSources.length > 0 && overflowSources.every(isExemptOverflowSource);
    if (allOverflowIsExempt) continue;
    const causes = overflowSources.filter((inner) => !isExemptOverflowSource(inner));
    report(
      "box-scroll",
      el,
      `holds ${el.scrollWidth}px in a ${el.clientWidth}px box; past its edges: ` +
        [...(ownText || overflowSources.length === 0 ? ["its own text"] : []), ...causes.map(label)]
          .slice(0, 3)
          .join(", ")
    );
  }

  const reportedCut = new Set<Element>();
  const cutInside = new Set<Element>();
  for (const el of all) {
    if (!shown(el) || visuallyHidden(el) || heldByCodeOrPicture(el)) continue;
    const clipper = clipperOf(el);
    if (clipper === null || scrollers.has(clipper) || visuallyHidden(clipper)) continue;
    const box = el.getBoundingClientRect();
    if (box.width === 0) continue;
    const edge = clipper.getBoundingClientRect();
    const clipStyle = styleOf(clipper);
    const inline = styleOf(el).display === "inline";
    const within = (span: Span) =>
      span.right <= edge.right + SLACK && span.left >= edge.left - SLACK;
    if (within(box)) {
      // A box's ellipsis stands in for its own inline text, not for a line of a block (or an
      // `inline-block`) inside it: that line is cut bare, though the box's own-content pass below
      // reads the box's ellipsis as the sign of a cut.
      if (clipStyle.textOverflow !== "ellipsis" || clamps(clipStyle) || inline) continue;
      const text = ownTextOutside(el, within);
      if (text === null) continue;
      cutInside.add(clipper);
      reportedCut.add(el);
      report(
        "clipped",
        el,
        `its own text spans ${Math.round(text.left)}-${Math.round(text.right)}px inside ${path(clipper)} at ${Math.round(edge.left)}-${Math.round(edge.right)}px, a box whose ellipsis stands in for its own inline text alone`
      );
      continue;
    }
    // Inside a reported element, every descendant is cut because it is: not reported again. An
    // element its clipper's ellipsis excuses is not reported, so what it holds is judged below.
    const parent = el.parentElement;
    if (parent !== null && parent !== clipper && reportedCut.has(parent)) {
      reportedCut.add(el);
      continue;
    }
    cutInside.add(clipper);
    // An ellipsis stands in for inline text its box cuts, so only a plain `inline` element that
    // draws in the box's own lines (`inLinesOf`) is excused by one. A span in a block or a flex
    // item draws in that box's lines, which the ellipsis does not reach. A chip (`inline-flex`),
    // an `inline-block` or a block that runs past the box is an element pushed out of its row
    // (point 2 of the rule), whether the browser cuts it or hides it behind the box's ellipsis, a
    // picture (`inline` too) loses what the ellipsis does not stand in for, and an element's own
    // ellipsis is cut away with the rest. A line clamp excuses an inline element only where
    // `clampCutsBare` finds no line of it cut bare. What an excused element holds is judged in its
    // own right: a chip inside it is still pushed out.
    const picture = el.matches(PICTURE_OR_ICON);
    const markedByClipper =
      clipStyle.textOverflow === "ellipsis" || (clamps(clipStyle) && !clampCutsBare(clipper, el));
    if (inLinesOf(el, clipper) && !picture && markedByClipper) continue;
    reportedCut.add(el);
    report(
      "clipped",
      el,
      `spans ${Math.round(box.left)}-${Math.round(box.right)}px inside ${path(clipper)} at ${Math.round(edge.left)}-${Math.round(edge.right)}px`
    );
  }

  // A box that clips its own text, with no ellipsis to say so. A box whose cut elements were
  // judged above is not read again: the cut is theirs. A line-clamped box is cut bare only where
  // `clampCutsBare` says so. A code block is read as any box; a picture's own drawing is its own.
  for (const el of all) {
    const style = styleOf(el);
    if (style.overflowX !== "hidden" && style.overflowX !== "clip") continue;
    if (cutInside.has(el) || !shown(el) || visuallyHidden(el)) continue;
    if (style.textOverflow === "ellipsis" || el.closest(PICTURE_OR_ICON) !== null) continue;
    if (el.scrollWidth <= el.clientWidth + SLACK) continue;
    if (clamps(style) && !clampCutsBare(el, el)) continue;
    report("clipped", el, `its content is ${el.scrollWidth}px in a ${el.clientWidth}px box`);
  }

  // An element cut at the viewport, which no ancestor clips: one that starts left of it, where no
  // scroll reaches, and one on a fixed layer that runs past either side, since a fixed layer adds
  // nothing to the page's width. Only the outermost such element, and only one partly in view:
  // one wholly outside the viewport is not drawn at all. Where the element fits, its own text is
  // read the same way, unless the element clips that text itself: then it is the element's own
  // content, which the pass above reads.
  const cutAtViewport = new Set<Element>();
  for (const el of all) {
    if (!shown(el) || visuallyHidden(el) || heldByCodeOrPicture(el)) continue;
    const holder = holderOf(el);
    if (typeof holder !== "string") continue;
    const box = el.getBoundingClientRect();
    // The page scrolls sideways under an element it holds, so its edges are read in the document,
    // and it is partly on the page if any of it lies between the page's two edges; a fixed layer
    // does not scroll, so its edges are read in the viewport.
    const shift = holder === "page" ? window.scrollX : 0;
    const extent = holder === "page" ? pageWidth : viewport;
    const onPage = (span: Span) => span.right + shift > 0 && span.left + shift < extent;
    if (box.width === 0 || !onPage(box)) continue;
    const pastEdges = (span: Span) => ({
      left: span.left + shift < -SLACK,
      right: holder === "fixed" && span.right > viewport + SLACK,
    });
    const boxPast = pastEdges(box);
    const boxCut = boxPast.left || boxPast.right;
    if (boxCut) cutAtViewport.add(el);
    const parent = el.parentElement;
    if (parent !== null && cutAtViewport.has(parent)) continue;
    // Text wholly off the page is not drawn, so it fits as much as text inside the viewport does.
    const text = boxCut
      ? null
      : ownTextOutside(el, (span) => {
          const past = pastEdges(span);
          return !onPage(span) || (!past.left && !past.right);
        });
    if (!boxCut && text === null) continue;
    const what = text === null ? "it" : "its own text";
    const span = text ?? box;
    const past = pastEdges(span);
    report(
      "clipped",
      el,
      `${what} spans ${Math.round(span.left)}-${Math.round(span.right)}px in a ${viewport}px viewport` +
        (holder === "fixed" ? ", on a fixed layer" : "") +
        (past.left ? `; ${what} starts left of the viewport, where no scroll reaches` : "") +
        (past.right ? `; ${what} runs past the viewport's right edge` : "")
    );
  }

  // A group that shows more entries than its cap, unless the reader opened it with its "+N"
  // control (`data-layout-open`): then it shows every entry by the reader's own choice. The
  // group itself may draw no box (`display: contents`), so only its entries are asked whether
  // they are shown, and a group that shows none of them marks nothing. A cap is a whole number
  // from 1 to 10: a group that declares any other (`data-layout-max="11"`, "0", "abc") has raised
  // or dropped the rule's cap, and is a breach however many entries it shows, opened or not. A
  // group has one "+N" control: more children marked `data-layout-more` would each go
  // uncounted, so the group is a breach for them too.
  const groups = new Set<string>();
  for (const group of document.querySelectorAll("[data-layout-group]")) {
    const entries = [...group.children].filter(
      (child) => shown(child) && !child.hasAttribute("data-layout-more")
    ).length;
    if (entries > 0) groups.add(group.getAttribute("data-layout-group") ?? "");
    // The DOM's own attribute, read in the page by `page.evaluate` after the row settled.
    const declared = group.getAttribute("data-layout-max");
    const max = declared === null ? 10 : Number(declared);
    if (declared !== null && !(/^\d+$/.test(declared) && max >= 1 && max <= 10)) {
      report(
        "group-over-cap",
        group,
        `declares the cap ${JSON.stringify(declared)}; a cap is a whole number from 1 to 10`
      );
      continue;
    }
    const marked = [...group.children].filter((child) => child.hasAttribute("data-layout-more"));
    if (marked.length > 1) {
      report(
        "group-over-cap",
        group,
        `marks ${marked.length} children data-layout-more; a group has one "+N" control`
      );
      continue;
    }
    if (group.hasAttribute("data-layout-open")) continue;
    if (entries > max) {
      report("group-over-cap", group, `shows ${entries} entries; the cap is ${max}`);
    }
  }

  const content = document.querySelector("[data-testid='main-content']");
  const contentElements = content === null ? 0 : content.querySelectorAll("*").length;
  return { breaches, contentElements, elements: all.length, groups: [...groups], viewport };
}
