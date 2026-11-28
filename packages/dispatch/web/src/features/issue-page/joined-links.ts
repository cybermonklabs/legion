import { nodeRouteFromHref, referenceRouteFromHref } from "../refs/routes";

/** The elements a mark of the display schema draws as, around a run of a text's words: bold,
 *  emphasis, a code span, a deletion and the like. */
const MARKS: Readonly<Record<string, true>> = {
  B: true,
  CODE: true,
  DEL: true,
  EM: true,
  I: true,
  MARK: true,
  S: true,
  STRONG: true,
  SUB: true,
  SUP: true,
  U: true,
};

/** Whether `node` is a mark's element whose one child is `child`. */
function marksOnly(node: Node | null, child: Node): node is HTMLElement {
  return (
    node instanceof HTMLElement &&
    MARKS[node.tagName] === true &&
    node.childNodes.length === 1 &&
    node.firstChild === child
  );
}

/** The marks of an anchor a pass has made its own: a reference's anchor, which the reference pass
 *  has emptied for the portal that draws into it (`collectReferenceAnchors`), a picture's, which
 *  the picture's portal draws into (`collectPictureAnchors`), and the row's own reference, whose
 *  words the row's pass renames (`linkRowReferences`). */
const OWNED = "[data-dispatch-ref], [data-dispatch-picture], [data-row-reference]";

/**
 * Whether `anchor` is a link this joins: every anchor with an `href` but the ones the reference
 * pass reads and the ones a pass has made its own (`OWNED`). The reference pass
 * (`collectReferenceAnchors`) reads an anchor's address as its author wrote it,
 * `data-dispatch-href` first (Proof draws a `dispatch://` reference `href=""` with its address
 * there), else `href` (`RefLink`'s `addressOf`), and reads exactly the anchors whose address is a
 * `dispatch://` one or one that `referenceRouteFromHref` or `nodeRouteFromHref` resolves against
 * this origin; which of the two it asks rests on the anchor's words, which a join changes, so
 * this asks both. It rewrites each such address, or makes the anchor its words where it names
 * nothing, and empties the anchor for a portal that draws into it. `DrawnText` runs the join both
 * after that pass (`onRendered`) and from a layout effect that can run before it, so a joined
 * anchor of that kind would take an anchor the pass needs, or has filled, off the page. Every
 * other anchor - another site's, a scheme of no site such as `mailto:`, a page of this dashboard
 * that names no node such as `/settings`, an in-page `#x` - the painter draws whole and no pass
 * rewrites, so it joins.
 */
function joinable(anchor: HTMLAnchorElement): boolean {
  const href = anchor.getAttribute("href");
  if (href === null || anchor.matches(OWNED)) return false;
  const address = anchor.getAttribute("data-dispatch-href") ?? href;
  return (
    !address.startsWith("dispatch://") &&
    referenceRouteFromHref(address, window.location.origin) === undefined &&
    nodeRouteFromHref(address, window.location.origin) === undefined
  );
}

/** The attribute `useOutOfTabOrder` (`RowText.tsx`) writes on a row's links as each slice of a
 *  long text lands, so the pieces of one link that two slices drew can differ in it alone. */
const TAB_INDEX = "tabindex";

/** Whether `a` and `b` carry the same attributes, each with the same value, `tabindex` aside
 *  (`TAB_INDEX`), which says nothing of the link a piece is part of. ProseMirror draws two link
 *  marks side by side as one anchor only where their attributes are equal, so two anchors whose
 *  attributes differ, a title say, are two links. */
function sameAttributes(a: Element, b: Element): boolean {
  if (
    a.attributes.length - Number(a.hasAttribute(TAB_INDEX)) !==
    b.attributes.length - Number(b.hasAttribute(TAB_INDEX))
  ) {
    return false;
  }
  for (const { name, value } of a.attributes) {
    if (name !== TAB_INDEX && b.getAttribute(name) !== value) return false;
  }
  return true;
}

/** The link `node` is, or that it holds as the one child of its marks, at any depth of them; null
 *  where it is neither or it is no link this joins (`joinable`). */
function linkOf(node: Node | null): HTMLAnchorElement | null {
  if (node instanceof HTMLAnchorElement) return joinable(node) ? node : null;
  const inner = node?.firstChild ?? null;
  return inner !== null && marksOnly(node, inner) ? linkOf(inner) : null;
}

/**
 * Joins each link a painted text draws as several anchors into one (decision 407). The display
 * schema ranks `link` innermost (emphasis 0, strong 1, inlineCode 2, link 3), so the serializer
 * closes the anchor around a marked run of its words and opens it again after it:
 * `[the **HTTP** rule](https://example.com/x)` draws `<a>the </a><strong><a>HTTP</a></strong><a>
 * rule</a>`, three stops of the row's `f` key and three links read aloud for one. Anchors with the
 * same attributes that stand side by side, with nothing between them but the mark elements around
 * each, are one link: the first anchor takes the words of every other, each marked run keeping
 * its mark inside it, and the others go. Two links to one address with any words between them
 * stay two, and so do two whose attributes differ (`sameAttributes`). A join can leave a mark
 * around one anchor only, which a pass that has gone by it never joins to its neighbours, so this
 * passes over the anchors until a pass joins nothing. Each paint draws the anchors anew, so the
 * row's pass calls this after each paint and from a layout effect, each time before it links what
 * the words cite (`RowText.tsx`). An anchor the reference pass reads never joins (`joinable`),
 * however far that pass has got.
 */
export function joinSplitLinks(element: Element): void {
  while (joinPass(element)) {
    // Each join takes at least one anchor off the page, so a pass that joins nothing comes.
  }
}

/** One pass of `joinSplitLinks` over the anchors of `element` in document order; whether it
 *  joined any. */
function joinPass(element: Element): boolean {
  let joined = false;
  for (const anchor of [...element.querySelectorAll<HTMLAnchorElement>("a")]) {
    // An anchor an earlier one took the words of has left the element; one the reference pass
    // reads, or one a pass made its own, stays as it is (`joinable`).
    if (!element.contains(anchor) || !joinable(anchor)) continue;
    let piece: ChildNode = anchor;
    while (piece.parentElement !== element && marksOnly(piece.parentElement, piece)) {
      piece = piece.parentElement;
    }
    const run: ChildNode[] = [piece];
    for (let next = piece.nextSibling; next !== null; next = next.nextSibling) {
      const link = linkOf(next);
      if (link === null || !sameAttributes(link, anchor)) break;
      run.push(next);
    }
    if (run.length > 1) {
      join(anchor, run);
      joined = true;
    }
  }
  return joined;
}

/** Makes `joined`, the link of the first piece of `run`, the one anchor around the words of every
 *  piece, each piece's marks kept around its own words. */
function join(joined: HTMLAnchorElement, run: readonly ChildNode[]): void {
  for (const [at, piece] of run.entries()) {
    const link = linkOf(piece);
    if (link === null) continue;
    if (piece === link) {
      if (at > 0) {
        joined.append(...link.childNodes);
        link.remove();
      }
      continue;
    }
    // A marked piece: its words stay inside its marks, which go inside the one anchor.
    link.replaceWith(...link.childNodes);
    if (at === 0) piece.before(joined);
    joined.append(piece);
  }
}
