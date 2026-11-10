import { expect, type Locator, type Page } from "@playwright/test";

/**
 * Nothing anywhere inside `locator`, or above it, cuts its text: no clamp that is cutting
 * anything on the scope, on any of its descendants or on any of its ancestors, and no box set
 * to clip its own overflow (`overflow` other than `visible`) holding content that no longer
 * fits it. A page taller than its viewport scrolling normally (`overflowY: visible` on `html`)
 * is not a clip, so the walk never flags that; a box set to clip, with content past its edge,
 * is.
 *
 * The walk is every element of the subtree and not one chain from the first leaf up, because a
 * scope is usually a container - the identity block, the preset row, the panel - and a clamp
 * sits on whichever child drew it, not on the one chain a descent through first children
 * happens to take: a clamp off that chain was never read (dispatch://INFRA-1196, the review of
 * pull request 2813). It still covers the two cases the chain covered: an inner span the
 * locator wraps (LEGION-559's issue title, a `line-clamp-2` inside the `h1` the heading role
 * resolves to) and a locator that is itself the clamped element (approval.e2e.ts's
 * long-question `<p>`).
 *
 * **A clamp counts only while it is clamping**, and this is the part a later reader will want
 * to undo: do not. `truncate` computes `text-overflow: ellipsis` whether or not the text is
 * longer than its box, and a clamp that cuts nothing takes no word from a reader, so a reading
 * made on the declaration alone fails on a class name rather than on a fault. The old chain
 * walk only looked whole because it missed most elements; broadening it and keeping the
 * declaration-only rule would fail every surface that writes `truncate` anywhere. So
 * `text-overflow: ellipsis` counts only where `scrollWidth > clientWidth + 1`, and
 * `-webkit-line-clamp` only where `scrollHeight > clientHeight + 1`: the box is cutting text a
 * reader cannot otherwise reach. That is stronger than the reading this replaced in what it
 * covers and honest in what it calls a defect.
 *
 * **An element that is not drawn is not read, and neither is anything inside it.** The reading
 * is of what a reader on the screen loses, and an element taken out of the drawing and left to
 * assistive technology loses them nothing: the technology reads all of it. Such an element is
 * told by its computed box and never by a class name - a utility is one spelling of it and the
 * next surface may write the same CSS by hand - and it leaves two marks together: a border box
 * of a CSS pixel or less in each direction, and a `clip-path` other than `none`. Either mark
 * is enough, because both describe a box no reader can see: the 1x1 box clips 128 px of words
 * down to nothing by design, which the overflow rule would otherwise read as a clip of
 * something a person was meant to see (measured on the panel's own `sr-only` description,
 * `span: 128x20 in 1x1`). What is inside such an element is not drawn either, so the walk
 * stops there rather than descending.
 *
 * These two ask **does the reader have every word**, so a box that cuts counts however it
 * cuts: `overflow: hidden` is a finding exactly as an ellipsis is, because a word cut off is
 * a word gone whether or not the box advertised the cut. `assertFitsItsBox` below asks the
 * other question - does this box scroll sideways - and there `hidden` is no finding; a scope
 * picks the reading it means.
 *
 * **A box that holds one line hides nothing below it.** `white-space: nowrap` and `pre` put
 * every word of a box on a single line, so its height is that one line's and a `scrollHeight`
 * past its `clientHeight` is the font's leading - the few pixels a line box is taller than the
 * height the page set - and never a line a reader lost (measured on a row's title at 1440 px,
 * `span…truncate: 143x21 in 143x16`, five pixels of leading on a one-line box). So the
 * downward half of both rules above - the line clamp's and the overflow's - is not read for
 * such a box. The sideways half is unchanged, which is where its real cut shows: a nowrap box
 * is exactly the box whose words run off the end of the line.
 *
 * There is no exception list and no file is special-cased. A clamp that is cutting words is a
 * finding wherever it is drawn, and the answer is the page's or the scope's, never this
 * helper's.
 */
export async function assertWhole(locator: Locator): Promise<void> {
  await assertUnclipped(locator, { clamps: true, cuts: true, down: true });
}

/**
 * The same walk for a box that is meant to scroll down: the issue page's right-hand panel holds
 * the properties, the focused node's facts and the whole conversation, so it is taller than the
 * screen and scrolls vertically by design (`features/margin/MarginSheet.tsx`'s `overflow-y-auto`
 * on a `h-dvh` rail). R17 bans scrolling in *two* dimensions, never in one, so this reading
 * drops the vertical half of the overflow rule and keeps the rest: a sideways clip still
 * fails, and so does a line clamp that is cutting lines, since a line a clamp took is a line
 * the reader does not have whichever way the box scrolls.
 */
export async function assertWholeAcross(locator: Locator): Promise<void> {
  await assertUnclipped(locator, { clamps: true, cuts: true, down: false });
}

/**
 * The weaker of the two readings, for a box that may shorten its own text: no box inside
 * `locator` *scrolls* past its own edge, and `locator`'s own right edge is inside the box that
 * holds it. Neither a clamp nor a box that cuts is read at all.
 *
 * The two readings answer two different questions, which is the whole of the difference
 * between them. `assertWhole` and `assertWholeAcross` ask **does the reader have every word**:
 * there, `overflow: hidden` is a finding exactly as an ellipsis is, because a word cut off is
 * a word gone whether the box advertised the cut or not. This asks **does this box scroll
 * sideways**, which is R17's question (it bans scrolling in two dimensions, never cutting), so
 * here only a box that can actually be scrolled counts - `overflow` of `auto`, `scroll` or
 * `overlay` - and `hidden` and `clip` do not.
 *
 * A row of the issue tree is the case this exists for (`e2e/issue-layout.e2e.ts`, c-reflow). A
 * row is one line by design and cuts its first line at a sentence end, or at a word boundary
 * by 100 characters (`web/src/features/issue/NodeRow.tsx`), and the focused node's body holds
 * that text whole - c-tree reads the body against the stored text - so the reader loses no
 * word to the cut: they open the node. Tailwind's `truncate` is `overflow: hidden` with
 * `text-overflow: ellipsis`, so both halves of the stronger reading fail on that one cut,
 * measured at 1440 px as `span…truncate: 323x20 in 186x20`, and both would be right to for a
 * scope that promises every word on screen. A row promises something else, which is what this
 * reads: whatever it shortened, it fits the box it is drawn in and takes the page nowhere
 * sideways.
 *
 * So the two readings stand side by side and a scope picks the one it means. This is not an
 * exception inside `assertWhole`: the identity block, the preset row and the panel keep that
 * reading unchanged, and a scope that wants this one asks for it by name.
 */
export async function assertFitsItsBox(locator: Locator): Promise<void> {
  await assertUnclipped(locator, { clamps: false, cuts: false, down: true });
  const over = await locator.evaluate((element) => {
    const parent = element.parentElement;
    if (parent === null) return null;
    const box = element.getBoundingClientRect();
    const holder = parent.getBoundingClientRect();
    if (box.right <= holder.right + 1) return null;
    return `${Math.round(box.right)} past the ${Math.round(holder.right)} its container ends at`;
  });
  expect(over, over ?? "").toBeNull();
}

/** How many clipped elements a failure names. A scope holds hundreds, and a page that clips at
 *  all usually clips in several places at once; ten of them, each with its tag, its classes and
 *  its two sizes, say where to look without printing a subtree. */
const CLIPS_NAMED = 10;

async function assertUnclipped(
  locator: Locator,
  reading: { clamps: boolean; cuts: boolean; down: boolean }
): Promise<void> {
  // One `evaluate` over the whole subtree: a round trip per element would be hundreds of them
  // per scope, and the six widths of c-reflow pay for every one.
  const clips = await locator.evaluate(
    (element, { named, readsClamps, readsCuts, readsDown }) => {
      const found: string[] = [];
      const where = (node: Element) => {
        const classes = node.getAttribute("class");
        return `${node.tagName.toLowerCase()}${classes === null ? "" : `.${classes}`}`;
      };
      // Whether a reader on the screen sees this box at all: an element the page takes out of
      // the drawing and leaves to assistive technology is a box of a pixel or less, or one
      // with a `clip-path`, and either way nothing it holds is drawn.
      const drawn = (node: Element, style: CSSStyleDeclaration) => {
        const rect = node.getBoundingClientRect();
        return (rect.width > 1 || rect.height > 1) && style.clipPath === "none";
      };
      const read = (node: Element) => {
        const style = getComputedStyle(node);
        if (!drawn(node, style)) return false;
        const size = `${node.scrollWidth}x${node.scrollHeight} in ${node.clientWidth}x${node.clientHeight}`;
        const cutsAcross = node.scrollWidth > node.clientWidth + 1;
        // A box that puts every word on one line holds one line, so nothing of it can be
        // hidden below: what stands past its height is the leading of that line (the doc
        // above). Its sideways cut is read as any other box's.
        const oneLine = style.whiteSpace === "nowrap" || style.whiteSpace === "pre";
        const cutsDown = !oneLine && node.scrollHeight > node.clientHeight + 1;
        // A declaration cuts nothing until the text is longer than its box, in the direction
        // that declaration cuts: an ellipsis takes words off the end of a line, a line clamp
        // takes lines off the end of a box.
        if (
          readsClamps &&
          ((style.textOverflow === "ellipsis" && cutsAcross) ||
            (style.webkitLineClamp !== "none" && cutsDown))
        ) {
          found.push(
            `${where(node)}: line clamp ${style.webkitLineClamp}, ${style.textOverflow}, ${size}`
          );
        }
        // What counts as a box holding content past its edge: every box that is not `visible`
        // for the reading that asks whether the reader has every word, and only a box that can
        // be scrolled for the reading that asks whether anything scrolls sideways.
        const holds = (overflow: string) =>
          readsCuts
            ? overflow !== "visible"
            : overflow === "auto" || overflow === "scroll" || overflow === "overlay";
        if (
          (readsDown && holds(style.overflowY) && cutsDown) ||
          (holds(style.overflowX) && cutsAcross)
        ) {
          found.push(`${where(node)}: ${size}`);
        }
        return true;
      };
      // Down the subtree child by child rather than over one flat list, so an element that is
      // not drawn takes its own descendants out of the walk with it.
      const descend = (node: Element) => {
        if (!read(node)) return;
        for (const child of node.children) descend(child);
      };
      descend(element);
      for (let node = element.parentElement; node !== null; node = node.parentElement) read(node);
      if (found.length <= named) return found;
      return [...found.slice(0, named), `and ${found.length - named} more`];
    },
    {
      named: CLIPS_NAMED,
      readsClamps: reading.clamps,
      readsCuts: reading.cuts,
      readsDown: reading.down,
    }
  );
  expect(clips, clips.join(", ")).toEqual([]);
}

/**
 * Nothing on the page is wider than the viewport the browser was given. A phone zooms out to fit
 * a page wider than the device, and which measure that zoom moves depends on the page: the
 * visual viewport can grow, so `window.innerWidth` exceeds the device width while `clientWidth`
 * stays 390 (an unbroken document title in a flex row), or the layout viewport can widen with
 * it, so `scrollWidth` alone, compared against the zoomed-out `clientWidth`, passes (a 1,000-
 * character search chip). So both are checked: `innerWidth` is still the viewport's width, and
 * nothing is wider than `clientWidth`. Both are whole pixels, so the comparisons are exact.
 *
 * The read and both checks are retried because a resize settles on two clocks: the engine applies
 * the shell's `xl:` breakpoint the moment the viewport reaches 1280px, while the margin's wrapper
 * only stops being `contents` when React re-renders it, a task or two later, on the
 * `useMediaQuery` state it reads. In between, the compact bottom sheet is an in-flow, full-width
 * flex child of the shell, which takes most of the row and squeezes `<main>` down to its padding,
 * so content inside it sticks out past the viewport. The retry hides no real overflow: a settled
 * page that does not fit never passes, and the failure at the timeout is the check below, naming
 * both widths.
 */
export async function assertPageFits(page: Page): Promise<void> {
  await expect(async () => {
    const layout = await page.evaluate(() => ({
      clientWidth: document.documentElement.clientWidth,
      innerWidth: window.innerWidth,
      scrollWidth: document.documentElement.scrollWidth,
    }));
    expect(
      layout.innerWidth,
      `the page zoomed out to ${layout.innerWidth}px to fit content wider than the viewport`
    ).toBe(page.viewportSize()?.width);
    expect(
      layout.scrollWidth,
      `page ${layout.scrollWidth}px wide in a ${layout.clientWidth}px viewport`
    ).toBeLessThanOrEqual(layout.clientWidth);
  }).toPass({ timeout: 5_000 });
}
