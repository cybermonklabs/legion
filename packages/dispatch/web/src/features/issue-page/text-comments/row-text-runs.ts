import { type DispatchReferenceRoute, parseNodeReference } from "../../refs/routes";
import { AUTHOR_TEXT_ATTRIBUTE, PAGE_CHROME_ATTRIBUTE } from "../page-structure";

/**
 * The words a row draws of its node's own text, read from the DOM the row drew (R5, the plan's
 * decision 6), so `RowText` changes by no line: which text nodes are the node's text, and the
 * one string they make when joined as the server's `FindInBlock` joins a block's runs
 * (`pmdoc.buildFlattenedText`): text runs in order, one space where two runs do not touch - between
 * two text blocks, and around an atom such as a hard break or a picture - and no space where
 * a mark (a link, emphasis, a code span) ends and the next run begins.
 *
 * A row draws more than its node's text inside its words element, and none of that is a run: a
 * typed block's head and an ask's card (`contenteditable="false"`), the page's own words
 * (`data-page-chrome`), a deleted line (`data-post-deleted`), the row's trail of ties and marks
 * (`data-node-trail`), and what a reference's link draws in place of the address the text holds
 * (`row-references.ts`' link pass): the name of its target, a chip's words and " · edited".
 * Where such a link draws the address's own words (an issue by its key, and any target as its
 * author wrote it until it is named), those words are a run that stands for the whole address.
 */

/** One run of a node's own text: the text node that holds it, and where it starts and ends in
 *  `TextRuns.text`. */
export interface TextRun {
  readonly end: number;
  readonly node: Text;
  readonly start: number;
  /** Set where the run is an address the row draws by its own words (a reference's link that
   *  names its target by its key, or one not yet named, drawn as its author wrote it): `text`
   *  holds the address as the node's text writes it, and a selection that takes part of the run
   *  takes all of it. */
  readonly whole?: true;
}

/** Where the row draws words that are no part of the node's text: in place of words of it (a
 *  reference's link that names its target, `start < end`, the span the words it stands for take
 *  in `text`), or between two runs (a typed block's head, a chip, a deleted line: `start` and
 *  `end` the same offset). No selection takes words across one. */
export interface ForeignSpan {
  readonly end: number;
  readonly node: Element;
  readonly start: number;
}

/** The characters a row's words element draws from the node's own text, joined as the server's
 *  `FindInBlock` joins a block's runs (one space between two text blocks, an atom as one space),
 *  with each run's place, and the way from an offset to a DOM point and back. */
export interface TextRuns {
  /** What the row draws among the runs that is not the node's text, in the order it is drawn. */
  readonly foreign: readonly ForeignSpan[];
  /** The offset of the DOM point in `text`, or null where the point is in no run: outside the
   *  node's text, or inside what the row draws that is not its text (`foreign`). A point between
   *  runs answers the start of the next run. A point inside a run that stands for a whole address
   *  (`TextRun.whole`) answers an offset inside that run's span, which a caller widens to it. */
  readonly offsetOf: (node: Node, offset: number) => number | null;
  /** The DOM point at which the character at `offset` in `text` is drawn: in the run that holds
   *  it, else (a separator) at the start of the next run, else at the end of the last run; null
   *  inside what the row draws in place of the node's words (`foreign`) and past `text`'s end. */
  readonly pointOf: (offset: number) => { readonly node: Text; readonly offset: number } | null;
  readonly runs: readonly TextRun[];
  /** The node's text as the server's plain text reads it, each U+0000 as U+FFFD as a read serves
   *  it, the words of an address the row draws as a link included. */
  readonly text: string;
}

/** The mark the element a row draws its node's words in carries (`NodeRow.tsx`, `CutWords.tsx`,
 *  each view's own rows). */
const WORDS = "[data-node-words]";

/** The mark every row carries, which names its node. */
const ROW = "[data-node-id]";

/** The element `RowText` draws as its root, and that each row draws its author's words in. */
const AUTHOR_TEXT = `[${AUTHOR_TEXT_ATTRIBUTE}]`;

/** A reference the row's link pass drew as a link (`row-references.ts`' `ROW_REFERENCE`), with
 *  the words the text wrote it as where the pass kept them (`ROW_WRITTEN`). */
const REFERENCE = "a[data-row-reference]";
const REFERENCE_ATTRIBUTE = "data-row-reference";
const WRITTEN_ATTRIBUTE = "data-row-written";

/** What a row draws inside its words that is not the node's text: what the reading draws that
 *  takes no place in the text (`contenteditable="false"`: a typed block's head, an ask's card),
 *  the page's own words, a deleted line, the row's trail, and anything hidden or drawn as a
 *  control or a picture. */
const NOT_TEXT = [
  '[contenteditable="false"]',
  `[${PAGE_CHROME_ATTRIBUTE}]`,
  "[data-post-deleted]",
  "[data-node-trail]",
  "[hidden]",
  '[aria-hidden="true"]',
  "button",
  "noscript",
  "script",
  "select",
  "style",
  "svg",
  "template",
  "textarea",
].join(", ");

/** The elements a reading draws a mark as, which hold a run inside its text block and part it
 *  from no other run: every other element is a block, whose start and end part two runs. */
const INLINE: Readonly<Record<string, true>> = {
  a: true,
  abbr: true,
  b: true,
  bdi: true,
  bdo: true,
  cite: true,
  code: true,
  data: true,
  del: true,
  dfn: true,
  em: true,
  font: true,
  i: true,
  ins: true,
  kbd: true,
  label: true,
  mark: true,
  q: true,
  s: true,
  samp: true,
  small: true,
  span: true,
  strong: true,
  sub: true,
  sup: true,
  time: true,
  u: true,
  var: true,
};

/** The elements that draw an atom: a node that takes a place in the text and holds no run of it,
 *  which parts the runs on either side by one space. */
const ATOMS: Readonly<Record<string, true>> = {
  audio: true,
  br: true,
  canvas: true,
  embed: true,
  hr: true,
  iframe: true,
  img: true,
  input: true,
  object: true,
  picture: true,
  video: true,
};

/**
 * The words element of a row: the one element the row's site draws its node's own words in
 * (`[data-node-words]`, which `CutWords` and the site draw around `RowText`), or null where the
 * row has none or more than one. A words element of a row drawn inside this one (a Verification
 * statement draws its checks in its row, and a check its latest result) is that row's own.
 */
export function rowTextOf(row: Element): HTMLElement | null {
  let own: HTMLElement | null = null;
  for (const words of row.querySelectorAll<HTMLElement>(WORDS)) {
    if (words.closest(ROW) !== row) continue;
    if (own !== null) return null;
    own = words;
  }
  return own;
}

/**
 * The runs of `element`'s text that come from the node's own text: a run that a chip, a
 * reference's drawn name, an edited mark, an ask card or a deleted line draws is no run. The
 * node's text is the element `RowText` draws as its root inside `element` (`data-author-text`),
 * or `element` itself where it draws the words with no `RowText` (Plan's rows draw the task's
 * own words so).
 */
export function textRuns(element: HTMLElement): TextRuns {
  const root = element.matches(AUTHOR_TEXT)
    ? element
    : ([...element.querySelectorAll<HTMLElement>(AUTHOR_TEXT)].find(
        (candidate) => !apartFromText(candidate, element)
      ) ?? element);
  let text = "";
  // Whether a block's edge or an atom stands between the last run and the next.
  let apart = false;
  const runs: TextRun[] = [];
  const foreign: ForeignSpan[] = [];
  const separate = (): void => {
    if (apart && text !== "") text += " ";
    apart = false;
  };
  const visit = (parent: Node): void => {
    for (const child of parent.childNodes) {
      if (child instanceof Text) {
        const data = asRead(child.data);
        if (data === "") continue;
        separate();
        runs.push({ end: text.length + data.length, node: child, start: text.length });
        text += data;
        continue;
      }
      if (!(child instanceof Element)) continue;
      const tag = child.localName;
      if (child.matches(REFERENCE)) {
        reference(child);
        continue;
      }
      if (child.matches(NOT_TEXT)) {
        // What a reading draws apart from its text, a typed block's head, stands as a block does;
        // so does an atom a reading draws as a control of its own.
        if (INLINE[tag] !== true || child.getAttribute("contenteditable") === "false") {
          apart = true;
        }
        // Words a reader sees there stand between the runs on either side.
        if ((child.textContent ?? "").trim() !== "") {
          foreign.push({ end: text.length, node: child, start: text.length });
        }
        continue;
      }
      if (ATOMS[tag] === true) {
        apart = true;
        continue;
      }
      const block = INLINE[tag] !== true;
      if (block) apart = true;
      visit(child);
      if (block) apart = true;
    }
  };
  // A reference's link: the address it stands for, as the text writes it, and where it draws the
  // address's own words, a run that stands for all of it; what it draws besides is the target's
  // name.
  const reference = (anchor: Element): void => {
    const drawn = textNodesIn(anchor);
    const marked = anchor.getAttribute(WRITTEN_ATTRIBUTE);
    const short = ownWords(
      parseNodeReference(anchor.getAttribute(REFERENCE_ATTRIBUTE) ?? ""),
      drawn,
      marked
    );
    const written = marked ?? short?.data ?? "";
    const named = drawn.some((one) => one !== short && one.data.trim() !== "");
    if (written === "") {
      if (named) foreign.push({ end: text.length, node: anchor, start: text.length });
      return;
    }
    separate();
    const start = text.length;
    text += asRead(written);
    if (short !== undefined) {
      runs.push({ end: text.length, node: short, start, whole: true });
      if (named) foreign.push({ end: text.length, node: anchor, start: text.length });
    } else {
      foreign.push({ end: text.length, node: anchor, start });
    }
  };
  if (!root.matches(NOT_TEXT)) visit(root);
  const byNode = new Map(runs.map((run) => [run.node, run]));
  return {
    foreign,
    offsetOf: (node, offset) => {
      if (node instanceof Text) {
        const run = byNode.get(node);
        if (run === undefined) return null;
        if (run.whole === true) {
          if (offset <= 0) return run.start;
          return offset >= node.length ? run.end : Math.min(run.start + 1, run.end);
        }
        return run.start + Math.min(Math.max(offset, 0), run.end - run.start);
      }
      if (!(node instanceof Element) || !root.contains(node) || apartFromText(node, root)) {
        return null;
      }
      const next = offset < node.childNodes.length ? node.childNodes[offset] : undefined;
      const after = runs.find((run) =>
        next === undefined ? follows(node, run.node) : atOrAfter(next, run.node)
      );
      return after === undefined ? text.length : after.start;
    },
    pointOf: (offset) => {
      if (offset < 0 || offset > text.length) return null;
      const holding = runs.find((run) => run.start <= offset && offset < run.end);
      if (holding !== undefined) return pointIn(holding, offset);
      if (foreign.some((span) => span.start <= offset && offset < span.end)) return null;
      const next = runs.find((run) => run.start >= offset);
      if (next !== undefined) return pointIn(next, next.start);
      const last = runs.at(-1);
      return last === undefined ? null : pointIn(last, last.end);
    },
    runs,
    text,
  };
}

/** `data` as a read serves the text it is drawn from: each U+0000 a browser edit left as U+FFFD,
 *  as the server matches a quote (`nulAsReplacement`). Both are one UTF-16 unit, so no offset
 *  moves. */
export function asRead(data: string): string {
  return data.replaceAll("\u0000", "\uFFFD");
}

/** The DOM point of `offset` in `run`: a run that stands for a whole address is its drawn words'
 *  start or end. */
function pointIn(run: TextRun, offset: number): { readonly node: Text; readonly offset: number } {
  if (run.whole === true)
    return { node: run.node, offset: offset <= run.start ? 0 : run.node.length };
  return { node: run.node, offset: offset - run.start };
}

/** The text nodes under `element` that hold a character, in order. */
function textNodesIn(element: Element): Text[] {
  const found: Text[] = [];
  const walker = element.ownerDocument.createTreeWalker(element, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    if (node instanceof Text && node.data !== "") found.push(node);
  }
  return found;
}

/** The text node of a reference's link that draws the address's own words, where one does: an
 *  issue's key, which leads the link before its title (`row-references.ts`' `nameReference`), and
 *  the words its author wrote (`written`) where the link draws nothing else, its target not yet
 *  named (`rowReferenceWords`). */
function ownWords(
  route: DispatchReferenceRoute | undefined,
  drawn: readonly Text[],
  written: string | null
): Text | undefined {
  if (route === undefined) return undefined;
  const first = drawn[0];
  if (drawn.length === 1 && written !== null && first?.data === written) return first;
  return route.kind === "issue" && first?.data === route.key ? first : undefined;
}

/** Whether `node`, inside `root`, stands in what a row draws apart from its node's text: a
 *  reference's link, or anything else that is no run (`NOT_TEXT`). */
function apartFromText(node: Element, root: Element): boolean {
  for (let at: Element | null = node; at !== null && at !== root; at = at.parentElement) {
    if (at.matches(REFERENCE) || at.matches(NOT_TEXT)) return true;
  }
  return root.matches(NOT_TEXT);
}

/** Whether `text` is `node` or comes after it in the document, inside it or past it. */
function atOrAfter(node: Node, text: Text): boolean {
  return (
    node === text || (node.compareDocumentPosition(text) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
  );
}

/** Whether `text` comes after the end of `element`, outside it. */
function follows(element: Element, text: Text): boolean {
  return (
    !element.contains(text) &&
    (element.compareDocumentPosition(text) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0
  );
}
