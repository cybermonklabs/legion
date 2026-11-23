/** The layout rule's instrument (`packages/dispatch/AGENTS.md`, "Layout"; dispatch://INFRA-5200):
 *  what a page breaks of the rule, read off the live DOM. `measureLayout` runs inside the page
 *  (`page.evaluate(measureLayout, listed)`), so it is self-contained: it closes over nothing and
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
 *    `data-layout-scroll="board"`) or a picture (`img`, `picture`, `video`, `canvas`) that is
 *    itself wider than the box, or when the box is a code block scrolling its own lines, a data
 *    table's own box or the board box. Anything else that overflows - a row of chips next to a
 *    table, a strip of pictures each narrower than the box, an icon's `svg`, the box's own text -
 *    makes it a breach.
 *  - `clipped`: something is cut off at a side with no sign of it, in one of four ways.
 *    - An element runs past the left or right edge of the ancestor that clips it (`overflow-x`
 *      `hidden` or `clip`): only the outermost such element is reported, since its children are
 *      cut because it is. That ancestor's ellipsis excuses a plain `inline` element, whose text
 *      it stands in for, and nothing else: a chip (`inline-flex`), an `inline-block` or a block
 *      that runs past is pushed out of its row (point 2 of the rule), and an element's own
 *      ellipsis is cut away with the rest. A line clamp excuses only its last visible line, the
 *      one it draws its ellipsis on.
 *    - A box that clips holds more than its width of its own content (`scrollWidth` past
 *      `clientWidth`), which is how a line of text is cut, with no `text-overflow: ellipsis`; or,
 *      for a line-clamped box, a line above its last visible one runs past its side.
 *    - An element that no ancestor clips starts left of the viewport, where no scroll reaches.
 *    - An element on a fixed layer, which no ancestor clips, runs past either side of the
 *      viewport: a fixed layer adds nothing to the page's width, so it is cut, not scrolled.
 *    Only the outermost element cut at the viewport is reported, and only one partly in view:
 *    one wholly outside it is not drawn at all. Skipped: a screen-reader-only element (a box of a
 *    pixel, an empty `clip`, `clip-path: inset(50%)`), what a code block or a picture cuts of its
 *    own content, and an element whose containing block is outside the ancestor, which does not
 *    clip it (an absolutely positioned element above its positioned ancestor, a fixed one).
 *  - `group-over-cap`: a group marked `data-layout-group` shows more entries than its
 *    `data-layout-max` (10 when unset), not counting the one control marked `data-layout-more`
 *    that opens the rest, unless the reader opened it (`data-layout-open`); or it declares a cap
 *    that is not a whole number from 1 to 10, which is a breach however many it shows.
 *
 *  An element is read only where it draws (`checkVisibility`): the content of a closed
 *  `<details>` keeps a box in Chromium, and is no breach.
 *
 *  `listed` is a list of CSS selectors, the `element` of each known breach the caller may match
 *  (`e2e/layout-breaches.ts`). A page-scroll breach answers in `listed` the selectors of every
 *  element past the viewport inside it, since the cause of a page's overflow is usually deeper
 *  than the outermost element that carries it.
 *
 *  `elements` and `contentElements` count what it read, the page and the page's own region, so a
 *  caller can refuse a blank page: a page that drew nothing breaks nothing, and that is not a
 *  pass. `groups` names every `data-layout-group` on the page, so a caller can require the groups
 *  a page must mark. */
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
  /** How many of them are in the page's own region (`[data-testid="main-content"]`), the shell
   *  around it left out: what a route's floor (`minElements`) is read against. */
  readonly contentElements: number;
  /** The name of every `data-layout-group` on the page, once each. */
  readonly groups: readonly string[];
  readonly breaches: readonly LayoutBreach[];
}

export function measureLayout(listed: readonly string[] = []): LayoutReading {
  const root = document.documentElement;
  const viewport = root.clientWidth;
  /** A code block or a picture: its own width is its content's, and it may be cut. A `code` in a
   *  line of prose is no code block. */
  const CODE_OR_PICTURE = "pre, img, picture, svg, canvas, video";
  /** What a box may scroll sideways for whatever its width: a code block, a data table, a board's
   *  lanes. */
  const SCROLLS_FOR = "pre, table, [role='table'], [role='grid'], [data-layout-scroll='board']";
  /** A picture, which a box may scroll sideways for only when the picture is wider than the box.
   *  An `svg` is no picture here: inline, it is an icon in a row of controls. */
  const PICTURE = "img, picture, video, canvas";
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
  const listedFor = (elements: readonly Element[]): string[] =>
    listed.filter((selector) => elements.some((el) => el.closest(selector) !== null));
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
  /** The nearest ancestor that clips `el` sideways, or `null` when nothing does. */
  const clipperOf = (el: Element): Element | null => {
    const holder = holderOf(el);
    return typeof holder === "string" ? null : holder;
  };
  /** Whether line-clamped `box` cuts `node`'s text on a line above its last visible one, with no
   *  sign: the clamp draws its ellipsis on its last visible line only, so a line above it that
   *  runs past the box's side is cut bare. A line is read off the text's own rects (a `Range`),
   *  and one below the box's bottom edge is the clamp's to hide, not a sideways cut. */
  const cutAboveLastLine = (box: Element, node: Node): boolean => {
    const edge = box.getBoundingClientRect();
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
        rect.bottom <= lastTop + SLACK &&
        (rect.right > edge.right + SLACK || rect.left < edge.left - SLACK)
    );
  };
  /** The outermost of `elements`: those whose parent is not among them. */
  const outermostOf = (elements: readonly Element[]): Element[] => {
    const among = new Set(elements);
    return elements.filter((el) => el.parentElement === null || !among.has(el.parentElement));
  };

  const breaches: LayoutBreach[] = [];

  // A page that scrolls sideways: the outermost elements past the viewport that no box holds. An
  // element on a fixed layer adds nothing to the page's width; the viewport pass below reads it.
  const pageWidth = Math.max(root.scrollWidth, document.body.scrollWidth);
  if (pageWidth > viewport + SLACK) {
    const past = all.filter(
      (el) =>
        shown(el) && el.getBoundingClientRect().right > viewport + SLACK && holderOf(el) === "page"
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

  // A box that scrolls sideways for anything but a code block, a data table, a board's lanes or a
  // picture wider than itself running past its edges.
  const scrollers = new Set<Element>();
  for (const el of all) {
    const style = styleOf(el);
    const scrolls = style.overflowX === "auto" || style.overflowX === "scroll";
    if (!scrolls || !shown(el) || visuallyHidden(el)) continue;
    if (el.scrollWidth <= el.clientWidth + SLACK) continue;
    scrollers.add(el);
    // The box is the exempt element itself: a code block scrolling its own lines, a data table
    // or a board's lanes.
    if (el.closest("pre") !== null || el.matches(SCROLLS_FOR)) continue;
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
    /** Whether this source of the overflow is one the box may scroll for: a code block, a data
     *  table or a board's lanes it sits in, or a picture it sits in that is wider than the box. A
     *  strip of pictures each narrower than the box is a row, and scrolls as one. */
    const exempt = (inner: Element): boolean => {
      if (inner.closest(SCROLLS_FOR) !== null) return true;
      const picture = inner.closest(PICTURE);
      return picture !== null && picture.getBoundingClientRect().width > el.clientWidth + SLACK;
    };
    const forItsTables = !ownText && sources.length > 0 && sources.every(exempt);
    if (forItsTables) continue;
    const causes = sources.filter((inner) => !exempt(inner));
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
    // An ellipsis stands in for inline text its box cuts, so only a plain `inline` element is
    // excused by one. A chip (`inline-flex`), an `inline-block` or a block that runs past the box
    // is an element pushed out of its row (point 2 of the rule), whether the browser cuts it or
    // hides it behind the box's ellipsis, and an element's own ellipsis is cut away with the
    // rest. A line clamp draws its ellipsis on its last visible line only, so it excuses an
    // inline element only where that element is not cut on a line above it.
    const inline = styleOf(el).display === "inline";
    const clipStyle = styleOf(clipper);
    const markedByClipper =
      clipStyle.textOverflow === "ellipsis" ||
      (clamps(clipStyle) && !cutAboveLastLine(clipper, el));
    if (inline && markedByClipper) continue;
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
  // judged above is not read again: the cut is theirs. A line-clamped box is cut bare only on a
  // line above its last visible one.
  for (const el of all) {
    const style = styleOf(el);
    if (style.overflowX !== "hidden" && style.overflowX !== "clip") continue;
    if (cutInside.has(el) || !shown(el) || visuallyHidden(el)) continue;
    if (style.textOverflow === "ellipsis" || el.closest(CODE_OR_PICTURE) !== null) continue;
    if (el.scrollWidth <= el.clientWidth + SLACK) continue;
    if (clamps(style) && !cutAboveLastLine(el, el)) continue;
    breaches.push({
      detail: `its content is ${el.scrollWidth}px in a ${el.clientWidth}px box`,
      kind: "clipped",
      listed: listedFor([el]),
      locator: locatorOf(el),
      text: textOf(el),
      where: path(el),
    });
  }

  // An element cut at the viewport, which no ancestor clips: one that starts left of it, where no
  // scroll reaches, and one on a fixed layer that runs past either side, since a fixed layer adds
  // nothing to the page's width. Only the outermost such element, and only one partly in view:
  // one wholly outside the viewport is not drawn at all.
  const cutAtViewport = new Set<Element>();
  for (const el of all) {
    if (!shown(el) || visuallyHidden(el)) continue;
    // What a code block or a picture holds is its own to cut; the block or picture itself is not.
    const holderOfContent = el.parentElement?.closest(CODE_OR_PICTURE);
    if (holderOfContent !== null && holderOfContent !== undefined) continue;
    const holder = holderOf(el);
    if (typeof holder !== "string") continue;
    const box = el.getBoundingClientRect();
    if (box.width === 0 || box.right <= 0 || box.left >= viewport) continue;
    const pastLeft = box.left < -SLACK;
    const pastRight = holder === "fixed" && box.right > viewport + SLACK;
    if (!pastLeft && !pastRight) continue;
    cutAtViewport.add(el);
    const parent = el.parentElement;
    if (parent !== null && cutAtViewport.has(parent)) continue;
    breaches.push({
      detail:
        `spans ${Math.round(box.left)}-${Math.round(box.right)}px in a ${viewport}px viewport` +
        (holder === "fixed" ? ", on a fixed layer" : "") +
        (pastLeft ? "; it starts left of the viewport, where no scroll reaches" : "") +
        (pastRight ? "; it runs past the viewport's right edge" : ""),
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
  // they are shown. A cap is a whole number from 1 to 10: a group that declares any other
  // (`data-layout-max="11"`, "0", "abc") has raised or dropped the rule's cap, and is a breach
  // however many entries it shows, opened or not.
  const groups = new Set<string>();
  for (const group of document.querySelectorAll("[data-layout-group]")) {
    groups.add(group.getAttribute("data-layout-group") ?? "");
    // The DOM's own attribute, read in the page by `page.evaluate` after the row settled.
    const declared = group.getAttribute("data-layout-max");
    const max = declared === null ? 10 : Number(declared);
    if (declared !== null && !(/^\d+$/.test(declared) && max >= 1 && max <= 10)) {
      breaches.push({
        detail: `declares the cap ${JSON.stringify(declared)}; a cap is a whole number from 1 to 10`,
        kind: "group-over-cap",
        listed: listedFor([group]),
        locator: locatorOf(group),
        text: textOf(group),
        where: path(group),
      });
      continue;
    }
    if (group.hasAttribute("data-layout-open")) continue;
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

  const content = document.querySelector("[data-testid='main-content']");
  const contentElements = content === null ? 0 : content.querySelectorAll("*").length;
  return { breaches, contentElements, elements: all.length, groups: [...groups], viewport };
}
