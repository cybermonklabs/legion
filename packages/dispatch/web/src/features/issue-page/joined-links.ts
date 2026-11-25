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

/** The link `node` is, or that it holds as the one child of its marks, at any depth of them; null
 *  where it is neither. */
function linkOf(node: Node | null): HTMLAnchorElement | null {
  if (node instanceof HTMLAnchorElement) return node.hasAttribute("href") ? node : null;
  const inner = node?.firstChild ?? null;
  return inner !== null && marksOnly(node, inner) ? linkOf(inner) : null;
}

/**
 * Joins each link a painted text draws as several anchors into one (decision 407). The display
 * schema ranks `link` innermost (emphasis 0, strong 1, inlineCode 2, link 3), so the serializer
 * closes the anchor around a marked run of its words and opens it again after it:
 * `[the **HTTP** rule](https://example.com/x)` draws `<a>the </a><strong><a>HTTP</a></strong><a>
 * rule</a>`, three stops of the row's `f` key and three links read aloud for one. Anchors to the
 * same `href` that stand side by side, with nothing between them but the mark elements around
 * each, are one link: the first anchor takes the words of every other, each marked run keeping
 * its mark inside it, and the others go. Two links to one address with any words between them
 * stay two. Each paint draws the anchors anew, so the row's pass calls this after each paint,
 * before it links what the words cite (`RowText.tsx`).
 */
export function joinSplitLinks(element: Element): void {
  for (const anchor of [...element.querySelectorAll<HTMLAnchorElement>("a[href]")]) {
    // An anchor an earlier one took the words of has left the element.
    if (!element.contains(anchor)) continue;
    let piece: ChildNode = anchor;
    while (piece.parentElement !== element && marksOnly(piece.parentElement, piece)) {
      piece = piece.parentElement;
    }
    const href = anchor.getAttribute("href");
    const run: ChildNode[] = [piece];
    for (
      let next = piece.nextSibling;
      next !== null && linkOf(next)?.getAttribute("href") === href;
      next = next.nextSibling
    ) {
      run.push(next);
    }
    if (run.length > 1) join(anchor, run);
  }
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
