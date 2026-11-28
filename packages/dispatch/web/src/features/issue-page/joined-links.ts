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

/** Whether `anchor` is a link this joins: one whose address opens another site, or a scheme of no
 *  site such as `mailto:`, which the painter draws whole and no later pass rewrites or draws into.
 *  An address of this dashboard never joins: a `dispatch://` reference (Proof draws it `href=""`
 *  with its address in `data-dispatch-href`, which `RefLink`'s `addressOf` reads first), and a
 *  path or a URL of this origin. The reference pass rewrites each such address, and a portal draws
 *  into its anchor (`collectReferenceAnchors`, `collectPictureAnchors`, `linkRowReferences`); a
 *  text painted in slices is joined after each slice, before that pass, so joining one would take
 *  an anchor that pass needs off the page, and its target or its words with it. An address that
 *  does not parse is no link this joins. */
function joinable(anchor: HTMLAnchorElement): boolean {
  const href = anchor.getAttribute("href");
  if (href === null) return false;
  let url: URL;
  try {
    url = new URL(anchor.getAttribute("data-dispatch-href") ?? href, window.location.href);
  } catch {
    return false;
  }
  return url.protocol !== "dispatch:" && url.origin !== window.location.origin;
}

/** Whether `a` and `b` carry the same attributes, each with the same value. ProseMirror draws two
 *  link marks side by side as one anchor only where their attributes are equal, so two anchors
 *  whose attributes differ, a title say, are two links. */
function sameAttributes(a: Element, b: Element): boolean {
  if (a.attributes.length !== b.attributes.length) return false;
  for (const { name, value } of a.attributes) {
    if (b.getAttribute(name) !== value) return false;
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
 * row's pass calls this after each paint, before it links what the words cite (`RowText.tsx`).
 * Only a link to another site joins (`joinable`): an address of this dashboard is left as it is,
 * however far the reference pass has got.
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
    // An anchor an earlier one took the words of has left the element; one to this dashboard
    // stays as it is.
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
