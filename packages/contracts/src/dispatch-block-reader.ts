/**
 * The one reader of a Dispatch document's block structure in TypeScript (readAskBlocks). It reads
 * a document's blocks as the Dispatch server's parser reads them: goldmark's block structure
 * (`parseBlocks`, `openBlocks` and the default block parsers of github.com/yuin/goldmark v1.8.6),
 * with the rules of pmdoc's own parsers where pmdoc changes goldmark's
 * (packages/envoy/internal/dispatch/pmdoc), and Dispatch's typed blocks (pmdoc's directive.go).
 * Each function names the goldmark or pmdoc rule it follows. `askOpenerLines`
 * (dispatch-block-ref.ts) reads ask block openers here for the web and the CLI, and
 * packages/envoy-client's askgate-new-asks.ts reads ask blocks here for the ask gate. So a change
 * to how a block opens or closes is made here once, and both read it.
 */

/** A line that opens or closes a document's front matter. */
const FRONT_MATTER_FENCE = /^---[ \t]*$/;

/** The frame of a typed block's opening line: how many colons its fence holds, and the block's name. */
export interface TypedOpenerFrame {
  readonly colons: number;
  readonly name: string;
}

/** How readAskBlocks reads a document's lines, `F` being the frame that `typedOpener` gives for a
 *  typed block's opening line. */
export interface BlockReading<F extends TypedOpenerFrame> {
  /** Whether a front matter that the first line opens is read as front matter (frontMatterEnd). */
  readonly frontMatter: boolean;
  /** Whether the reader collects each ask block's lines (AskBlockLines.body). */
  readonly bodies: boolean;
  /**
   * The reader of a typed block's opening line, given the line from its first colon on. It gives
   * undefined where the line opens no typed block. A line of three colons or more that it gives
   * undefined for, other than `:::` alone, is a directive the server refuses (refusedDirective).
   */
  readonly typedOpener: (text: string) => F | undefined;
}

/** An ask block that readAskBlocks found. */
export interface AskBlockLines<F extends TypedOpenerFrame> {
  /** The block's opening line, from its first colon on. */
  readonly opener: string;
  /** The frame that BlockReading.typedOpener gave for the opening line. */
  readonly frame: F;
  /**
   * Each later line that the block holds, up to its closing line, blank lines included. A line is
   * read past the containers around the block, and past as many columns of its indentation as the
   * opening line stands past those containers. Empty where BlockReading.bodies is not set.
   */
  readonly body: readonly string[];
}

/**
 * Each ask block in `lines`, in document order: a typed block that `reading.typedOpener` names
 * `ask`, outside fenced and indented code and, where `reading.frontMatter` is set, outside the
 * front matter a document opens with. The reader reads the blocks as goldmark and pmdoc read them
 * (the module comment):
 *
 * - A line first goes past the containers it continues: a quote's `>` and one column after it, a
 *   list item's content columns, and a footnote definition's four columns. A blank line continues
 *   a list item and a footnote definition, but not a quote.
 * - A block opens only within three columns of those containers. A line four columns or more past
 *   them continues the paragraph before it, or else opens indented code. So the later line of an
 *   inline code span, which keeps its spaces, opens no block there.
 * - A container opens within three columns too: a `>`, a list marker (`-`, `*`, `+`, or one to
 *   nine digits and `.` or `)`) followed by a space, a tab or the line's end, or a footnote label
 *   (footnoteLabelEnd). Every space and tab after a label belongs to it.
 * - An empty list item, or one numbered other than 1, opens no list where it would interrupt a
 *   paragraph, or a block that the browser editor's parser holds open (interruptsOpenBlock).
 * - A list stays open at a line that reaches its empty last item's content column, where the line
 *   before it holds more than spaces, tabs and `>` (pmdoc's emptyItemGuard, continueList).
 * - A fence is three or more backticks or tildes, and a backtick fence's info string holds no
 *   backtick. A run of the same character at least as long closes it, within three columns of its
 *   containers and with only spaces and tabs after it. A container that ends ends the fence in it.
 * - A typed block closes at a line of as many colons or more, whatever block inside it the line
 *   would otherwise continue.
 * - A line that a container does not continue, and that opens no block, continues the paragraph
 *   in that container (a lazy line), and the container stays open.
 * - After a front-matter opener that no later line closes, no quote, list, footnote definition or
 *   typed block opens at the document's level (frontMatterEnd).
 *
 * HTML blocks, which the server refuses in a write, are not read.
 */
export function readAskBlocks<F extends TypedOpenerFrame>(
  lines: readonly string[],
  reading: BlockReading<F>
): AskBlockLines<F>[] {
  const frontMatter = reading.frontMatter ? frontMatterEnd(lines) : { end: 0, unclosed: false };
  const scan: BlockScan<F> = {
    lines: lines.slice(frontMatter.end),
    index: 0,
    opened: [],
    typedOpener: reading.typedOpener,
    bodies: reading.bodies,
    asks: [],
    emptyItemWithBlankLines: false,
    skipList: false,
    unclosedFrontMatter: frontMatter.unclosed,
  };
  const document: ScanBlock = {
    kind: "document",
    parent: undefined,
    previous: undefined,
    children: 0,
    last: undefined,
  };
  for (const [index, text] of scan.lines.entries()) {
    scan.index = index;
    readScanLine(scan, document, new ScanLine(text));
  }
  return scan.asks;
}

/**
 * Where `lines` go on past the front matter a document opens with, as pmdoc's
 * parseFrontmatterBlock reads it: a first line of `---` and any spaces or tabs opens it, and the
 * first later line that is the same closes it. pmdoc keeps it word for word, so no line of it opens
 * a block. `end` is 0 where the document opens with none, or with an opener nothing closes, which
 * the renderer never writes: a rule opening a document is written `***`. Past such an opener
 * (`unclosed`), no quote, list, footnote definition or typed block opens at the document's level
 * (pmdoc's frontmatterAttempt).
 */
function frontMatterEnd(lines: readonly string[]): { end: number; unclosed: boolean } {
  if (!FRONT_MATTER_FENCE.test(lines[0] ?? "")) return { end: 0, unclosed: false };
  const close = lines.findIndex((line, index) => index > 0 && FRONT_MATTER_FENCE.test(line));
  return close < 0 ? { end: 0, unclosed: true } : { end: close + 1, unclosed: false };
}

/** What the reader carries from line to line, as goldmark's parser context carries it. */
interface BlockScan<F extends TypedOpenerFrame = TypedOpenerFrame> {
  /** The markdown's lines past its front matter, and the index of the one being read. */
  readonly lines: readonly string[];
  index: number;
  /** The blocks open, outermost first, as `pc.OpenedBlocks()`; the document is not among them. */
  readonly opened: ScanBlock[];
  /** The reader of a typed block's opening line, and whether ask blocks' lines are collected
   *  (BlockReading). */
  readonly typedOpener: (text: string) => F | undefined;
  readonly bodies: boolean;
  /** Each ask block found so far. */
  readonly asks: AskBlockLines<F>[];
  /** goldmark's `emptyListItemWithBlankLines`. */
  emptyItemWithBlankLines: boolean;
  /** goldmark's `skipListParserKey`: a list item closed at a sibling's marker. */
  skipList: boolean;
  /** A front-matter opener that no later line closes (frontMatterEnd). */
  readonly unclosedFrontMatter: boolean;
}

interface ScanBlockBase {
  readonly parent: ScanBlock | undefined;
  /** The block opened before it in its parent (goldmark's `PreviousSibling`). */
  readonly previous: ScanBlock | undefined;
  /** How many blocks opened inside this one, and the last of them. */
  children: number;
  last: ScanBlock | undefined;
}

/** A block as the reader holds it open. A `leaf` is one that closes at the next line. */
type ScanBlock =
  | (ScanBlockBase & { readonly kind: "document" | "quote" | "footnote" | "leaf" })
  | (ScanBlockBase & {
      readonly kind: "paragraph";
      /** The index of its last line, and whether a line after its first is a table's delimiter row. */
      lastLine: number;
      table: boolean;
    })
  | (ScanBlockBase & {
      readonly kind: "code";
      /** The index of its last line that is not blank, and whether a blank line came before it. */
      lastLine: number;
      readonly blankBefore: boolean;
    })
  | (ScanBlockBase & { readonly kind: "list"; readonly marker: string })
  | (ScanBlockBase & {
      readonly kind: "item";
      /** The columns that the item's content stands past its container's. */
      readonly offset: number;
    })
  | (ScanBlockBase & {
      readonly kind: "typed";
      /** The colons it opened with: only a line of as many or more closes it. */
      readonly fence: number;
      /** The columns its opening line's text stands past its container's, taken off its lines. */
      readonly indent: number;
      /** An ask block's lines (AskBlockLines.body); undefined for a typed block of another name,
       *  and where the reader collects no lines (BlockReading.bodies). */
      readonly body: string[] | undefined;
    })
  | (ScanBlockBase & { readonly kind: "fence"; readonly char: string; readonly length: number });

/** What a block's continuation does with a line, as goldmark's `State`. */
type Continuation = "close" | "continue" | "children";

/** The column after `char`, which starts at `column`: a tab reaches the next multiple of four. */
function nextColumn(column: number, char: string): number {
  return char === "\t" ? column + 4 - (column % 4) : column + 1;
}

/**
 * One line of markdown, read from the column that the containers around a block have taken. A
 * column is measured forward from the first character not taken, and only when a reader asks for
 * it, as the Go twin's scanLine measures it
 * (packages/envoy/internal/dispatch/pmdoc/question_blocks.go).
 */
class ScanLine {
  readonly text: string;
  /** The index of the first character not taken, and the column it starts at. */
  #pos = 0;
  #start = 0;
  /** The column taken so far: #start, or more where a tab at #pos is partly taken. */
  #column = 0;

  constructor(text: string) {
    this.text = text;
  }

  /** The column taken so far. */
  get column(): number {
    return this.#column;
  }

  /** The column that the character at `index`, #pos or after it, starts at, and the line's end
   *  past its last one. */
  columnAt(index: number): number {
    let column = this.#start;
    for (let at = this.#pos; at < index; at++) column = nextColumn(column, this.text.charAt(at));
    return column;
  }

  /** The index of the first character from `index` on that is no space or tab. */
  #afterSpaces(index: number): number {
    let end = index;
    while (end < this.text.length && (this.text[end] === " " || this.text[end] === "\t")) end++;
    return end;
  }

  /** The columns of the spaces and tabs that start at `index`, #pos or after it. */
  spacesAt(index: number): number {
    return this.columnAt(this.#afterSpaces(index)) - this.columnAt(index);
  }

  /** The columns of space and tab from the column taken, and the index of the character after them. */
  indent(): { width: number; at: number } {
    const at = this.#afterSpaces(this.#pos);
    return { width: this.columnAt(at) - this.#column, at };
  }

  /** Whether nothing but spaces and tabs is left. */
  blank(): boolean {
    return this.indent().at === this.text.length;
  }

  /** Takes `columns` columns of the spaces and tabs ahead, a tab partly if it spans more. */
  skip(columns: number): void {
    this.#column += columns;
    while (this.#pos < this.text.length) {
      const next = nextColumn(this.#start, this.text.charAt(this.#pos));
      if (next > this.#column) return;
      this.#pos++;
      this.#start = next;
    }
  }

  /** Takes the line up to the character at `index`, #pos or after it. */
  advanceTo(index: number): void {
    this.#start = this.columnAt(index);
    this.#pos = index;
    this.#column = this.#start;
  }

  /** Takes the rest of the line, as a block of the rest of the line does. It measures no columns:
   *  nothing is left to measure. */
  takeAll(): void {
    this.#pos = this.text.length;
    this.#start = this.#column;
  }

  /** What is left of the line, a tab partly taken written as the columns it has left. */
  rest(): string {
    if (this.#pos < this.text.length && this.#start < this.#column) {
      return (
        " ".repeat(this.columnAt(this.#pos + 1) - this.#column) + this.text.slice(this.#pos + 1)
      );
    }
    return this.text.slice(this.#pos);
  }
}

/** One line through the open blocks, then the blocks it opens: goldmark's `parseBlocks` loop body. */
function readScanLine(scan: BlockScan, document: ScanBlock, line: ScanLine): void {
  if (scan.opened.length === 0) {
    if (!line.blank()) openScanBlocks(scan, document, line);
    return;
  }
  // No continuation changes scan.opened, and the loop returns where it opens or closes a block:
  // each index reads a block that was open when the line began.
  const lastIndex = scan.opened.length - 1;
  for (let index = 0; index <= lastIndex; index++) {
    const block = scan.opened[index] as ScanBlock;
    // A paragraph is continued by openScanBlocks, when the line opens no block that interrupts it.
    if (block.kind !== "paragraph") {
      const continuation = continueScanBlock(scan, block, line);
      if (continuation === "children" && index === lastIndex) {
        openScanBlocks(scan, block, line);
        return;
      }
      if (continuation !== "close") continue;
    }
    // The block closes here, or the line continues the paragraph last opened lazily.
    if (openScanBlocks(scan, scan.opened[index - 1] ?? document, line) !== "paragraph") {
      scan.opened.splice(index, lastIndex - index + 1);
    }
    return;
  }
}

/** goldmark's `openBlocks`: the blocks `line` opens inside `parent`, else a paragraph's continuation. */
function openScanBlocks(
  scan: BlockScan,
  parent: ScanBlock,
  line: ScanLine
): "opened" | "paragraph" | "none" {
  const continuable = scan.opened.at(-1)?.kind === "paragraph";
  let result: "opened" | "none" = "none";
  for (let into = parent; ; ) {
    const { width, at } = line.indent();
    if (at === line.text.length) break;
    const char = line.text.charAt(at);
    const interrupting = continuable && result === "none";
    let opened: { block: ScanBlock; children: boolean } | undefined;
    for (const parser of SCAN_PARSERS) {
      if (parser.triggers !== undefined && !parser.triggers.includes(char)) continue;
      if (interrupting && !parser.interrupts) continue;
      // No block but indented code opens four columns or more past its container.
      if (width > 3 && !parser.indented) continue;
      opened = parser.open(scan, into, line, width, at);
      if (opened !== undefined) break;
    }
    if (opened === undefined) break;
    into.children++;
    into.last = opened.block;
    scan.opened.push(opened.block);
    result = "opened";
    if (!opened.children) break;
    into = opened.block;
  }
  const last = scan.opened.at(-1);
  if (result === "none" && last?.kind === "paragraph" && !line.blank()) {
    if (TABLE_DELIMITER_ROW.test(line.rest())) last.table = true;
    last.lastLine = scan.index;
    return "paragraph";
  }
  return result;
}

/** A block parser: goldmark's `BlockParser` as the reader needs it. */
interface ScanParser {
  /** The characters a line's text starts with for this parser to try it; any, when undefined. */
  readonly triggers?: string;
  /** Whether the block may open where it would interrupt a paragraph (`CanInterruptParagraph`). */
  readonly interrupts: boolean;
  /** Whether the block may open four columns or more into its container (`CanAcceptIndentedLine`). */
  readonly indented: boolean;
  open(
    scan: BlockScan,
    parent: ScanBlock,
    line: ScanLine,
    width: number,
    at: number
  ): { block: ScanBlock; children: boolean } | undefined;
}

/** A block's place in the tree, empty, inside `parent`. */
function under(parent: ScanBlock): ScanBlockBase {
  return { parent, previous: parent.last, children: 0, last: undefined };
}

/** A block that holds nothing and closes at the next line: a heading, a rule, a refused directive. */
function leafAt(parent: ScanBlock, line: ScanLine): { block: ScanBlock; children: boolean } {
  line.takeAll();
  return { block: { kind: "leaf", ...under(parent) }, children: false };
}

/** Whether an unclosed front-matter opener keeps a list, quote, footnote definition or typed
 *  block from opening in `parent` (frontMatterEnd). */
function frontMatterHolds(scan: BlockScan, parent: ScanBlock): boolean {
  return scan.unclosedFrontMatter && parent.kind === "document";
}

// goldmark's default block parsers and Dispatch's own, in goldmark's order of priority: setext
// heading (100), thematic break (200), list (300), list item (400), indented code (500), ATX
// heading (600), fenced code (700), blockquote (800), Dispatch's refused directive (900) and typed
// block (950), footnote definition (999), paragraph (1000).
const SCAN_PARSERS: readonly ScanParser[] = [
  {
    triggers: "-=",
    interrupts: true,
    indented: false,
    open(scan, parent, line, _width, at) {
      const last = scan.opened.at(-1);
      if (last?.kind !== "paragraph" || last.parent !== parent) return undefined;
      return setextUnderline(line.text.slice(at)) ? leafAt(parent, line) : undefined;
    },
  },
  {
    triggers: "-*_",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      thematicBreak(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: "-+*0123456789",
    interrupts: true,
    indented: false,
    open(scan, parent, line) {
      const last = scan.opened.at(-1);
      if (last?.kind === "list" || scan.skipList) {
        scan.skipList = false;
        return undefined;
      }
      const marker = listMarker(line);
      if (marker === undefined || frontMatterHolds(scan, parent)) return undefined;
      // An empty item, or one numbered other than a lone `1`, opens no list where it would
      // interrupt a paragraph or an indented code block (pmdoc's emptyItemGuard, stricter than
      // goldmark, which lets `01.` interrupt a paragraph and anything interrupt code).
      if (marker.empty || (marker.number !== undefined && marker.number !== "1")) {
        if (last?.kind === "paragraph" && last.parent === parent) return undefined;
        if (interruptsOpenBlock(scan, parent)) return undefined;
      }
      scan.emptyItemWithBlankLines = false;
      return { block: { kind: "list", marker: marker.char, ...under(parent) }, children: true };
    },
  },
  {
    triggers: "-+*0123456789",
    interrupts: true,
    indented: false,
    open(scan, parent, line) {
      if (parent.kind !== "list") return undefined;
      const previous = parent.last?.kind === "item" ? parent.last.offset : 0;
      const marker = listMarker(line);
      if (marker === undefined || marker.indent - previous > 3) return undefined;
      scan.emptyItemWithBlankLines = false;
      // The content's columns past the marker; one, when the item opens with indented code.
      const after = line.spacesAt(marker.end);
      const spacing = marker.empty || after > 4 ? 1 : after;
      const offset = line.columnAt(marker.end) - line.column + spacing;
      const item: ScanBlock = { kind: "item", offset, ...under(parent) };
      if (marker.empty) return { block: item, children: false };
      line.advanceTo(marker.end);
      line.skip(spacing);
      return { block: item, children: true };
    },
  },
  {
    interrupts: false,
    indented: true,
    open(scan, parent, line, width) {
      if (width < 4) return undefined;
      line.skip(4);
      const previous = scan.lines[scan.index - 1];
      const block: ScanBlock = {
        kind: "code",
        lastLine: scan.index,
        blankBefore: previous === undefined || /^[ \t]*$/.test(previous),
        ...under(parent),
      };
      return { block, children: false };
    },
  },
  {
    triggers: "#",
    interrupts: true,
    indented: false,
    open: (_scan, parent, line, _width, at) =>
      /^#{1,6}(?:[ \t]|$)/.test(line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: "`~",
    interrupts: true,
    indented: false,
    open(_scan, parent, line, _width, at) {
      const char = line.text.charAt(at);
      const length = runOf(line.text, at, char);
      if (length < 3) return undefined;
      // A backtick fence's info string holds no backtick: such a line is inline code.
      if (char === "`" && line.text.slice(at + length).includes("`")) return undefined;
      line.takeAll();
      return { block: { kind: "fence", char, length, ...under(parent) }, children: false };
    },
  },
  {
    triggers: ">",
    interrupts: true,
    indented: false,
    open: (scan, parent, line) =>
      !frontMatterHolds(scan, parent) && quoteMarker(line)
        ? { block: { kind: "quote", ...under(parent) }, children: true }
        : undefined,
  },
  {
    triggers: ":",
    interrupts: true,
    indented: false,
    open: (scan, parent, line, _width, at) =>
      refusedDirective(scan, line.text.slice(at)) ? leafAt(parent, line) : undefined,
  },
  {
    triggers: ":",
    interrupts: true,
    indented: false,
    open(scan, parent, line, width, at) {
      const text = line.text.slice(at);
      const frame = scan.typedOpener(text);
      if (frame === undefined || frontMatterHolds(scan, parent)) return undefined;
      let body: string[] | undefined;
      if (frame.name === "ask") {
        body = scan.bodies ? [] : undefined;
        scan.asks.push({ opener: text, frame, body: body ?? [] });
      }
      line.takeAll();
      const block: ScanBlock = {
        kind: "typed",
        fence: frame.colons,
        indent: width,
        body,
        ...under(parent),
      };
      return { block, children: true };
    },
  },
  {
    triggers: "[",
    interrupts: true,
    indented: false,
    open(scan, parent, line, _width, at) {
      const label = footnoteLabelEnd(line.text, at);
      if (label === undefined || frontMatterHolds(scan, parent)) return undefined;
      // Every space and tab after the label's `]:` belongs to the definition's prefix, so a block
      // written on the same line starts at column zero inside it. goldmark leaves that whitespace
      // to the block it opens (extension/footnote.go, footnoteBlockParser.Open advances to the
      // byte after the `:` alone), which makes `[^x]:    :::ask{#a}` indented code; pmdoc's
      // footnoteDefinitionParser.Open skips it, as the browser editor's parser does, and leaves a
      // line holding nothing but whitespace after the label alone (pmdoc/footnotes.go).
      const content = line.text.slice(label).search(/[^ \t]/);
      line.advanceTo(content < 0 ? label : label + content);
      return { block: { kind: "footnote", ...under(parent) }, children: true };
    },
  },
  {
    interrupts: false,
    indented: false,
    open: (scan, parent, line) => {
      const block: ScanBlock = {
        kind: "paragraph",
        lastLine: scan.index,
        table: false,
        ...under(parent),
      };
      line.takeAll();
      return { block, children: false };
    },
  },
];

/**
 * pmdoc's interruptsOpenBlock: whether a list opening in `parent` interrupts a block the browser
 * editor's parser holds open there, other than a paragraph `parent` holds: a paragraph that ends on
 * the line before, reached across the containers the line opens first and forming no table, or an
 * indented code block with only blank lines after it, unless that code stands right after a list,
 * or right after a quote with no blank line between them. Where the code is `parent`'s own last
 * block, a line of only `>` after it leaves it open too. Where the list reaches the code only out
 * of the containers its line opens (`opensContainers`), such a line is one that a container was
 * already open on, so the code holds nothing open on the list's line.
 */
function interruptsOpenBlock(scan: BlockScan, parent: ScanBlock): boolean {
  let previous = parent.last;
  const opensContainers = previous === undefined;
  let node: ScanBlock | undefined = parent;
  while (previous === undefined && node !== undefined && node.kind !== "document") {
    previous = node.previous;
    node = node.parent;
  }
  if (previous?.kind === "paragraph") {
    return opensContainers && !previous.table && scan.index - previous.lastLine <= 1;
  }
  if (previous?.kind !== "code") return false;
  if (previous.previous?.kind === "list") return false;
  if (previous.previous?.kind === "quote" && !previous.blankBefore) return false;
  return scan.lines
    .slice(previous.lastLine + 1, scan.index)
    .every((row) => /^[ \t]*$/.test(row) || (!opensContainers && /^[ \t>]*$/.test(row)));
}

/**
 * A table's delimiter row, which makes the paragraph above it a table (GFM's tables): a `|`
 * somewhere in the row, and cells of `-` with an optional `:` at each end, between `|`s and
 * spaces and tabs. A `|`, a `:` or a `-` stands between each two runs of spaces and tabs, so the
 * pattern matches a long run in one way only, and it reads a row in time linear in its length.
 */
const TABLE_DELIMITER_ROW =
  /^(?=.*\|)[ \t]*(?:\|[ \t]*)?:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*(?:\|[ \t]*)?$/;

/** goldmark's `Continue` of each block the reader holds open. */
function continueScanBlock(scan: BlockScan, block: ScanBlock, line: ScanLine): Continuation {
  switch (block.kind) {
    case "quote":
      return quoteMarker(line) ? "children" : "close";
    case "list":
      return continueList(scan, block, line);
    case "item":
      return continueItem(scan, block, line);
    case "footnote": {
      if (line.blank()) return "children";
      const { width } = line.indent();
      if (width < 4) return "close";
      line.skip(4);
      return "children";
    }
    case "typed": {
      const { width, at } = line.indent();
      // A line of at least the fence's colons closes the block, whatever block inside it the line
      // would otherwise continue (directive.go, typedDirectiveParser.Continue).
      if (width < 4 && at < line.text.length && fenceColons(line.text.slice(at)) >= block.fence) {
        line.takeAll();
        return "close";
      }
      if (at < line.text.length) line.skip(Math.min(width, block.indent));
      block.body?.push(line.rest());
      return "children";
    }
    case "fence": {
      const { width, at } = line.indent();
      if (width < 4) {
        const length = runOf(line.text, at, block.char);
        if (length >= block.length && /^[ \t]*$/.test(line.text.slice(at + length))) {
          line.takeAll();
          return "close";
        }
      }
      line.takeAll();
      return "continue";
    }
    case "code": {
      if (line.blank()) return "continue";
      if (line.indent().width < 4) return "close";
      block.lastLine = scan.index;
      line.takeAll();
      return "continue";
    }
    default:
      // A heading, a rule or a refused directive holds one line; the document never closes.
      return block.kind === "document" ? "children" : "close";
  }
}

/**
 * goldmark's `listParser.Continue` (continueListAt), which pmdoc's emptyItemGuard.Continue keeps
 * from closing the list at a line that holds its empty last item's content: a line that reaches
 * the item's content column, a list marker there included, where the line before it holds more
 * than spaces, tabs and `>`.
 */
function continueList(
  scan: BlockScan,
  list: ScanBlock & { kind: "list" },
  line: ScanLine
): Continuation {
  const last = list.last;
  if (line.blank()) {
    if (last === undefined || last.children === 0) scan.emptyItemWithBlankLines = true;
    return "children";
  }
  const { width, at } = line.indent();
  const continuation = continueListAt(scan, list, line, width, at);
  // The first line has no line before it, which reads here as a blank one (pmdoc's `start == 0`).
  const before = scan.lines[scan.index - 1] ?? "";
  if (
    continuation === "close" &&
    last?.kind === "item" &&
    last.children === 0 &&
    width >= last.offset &&
    !/^[ \t>]*$/.test(before)
  ) {
    return "children";
  }
  return continuation;
}

/**
 * goldmark's `listParser.Continue` of a line that is not blank, its text at index `at` and `width`
 * columns past the list's container.
 */
function continueListAt(
  scan: BlockScan,
  list: ScanBlock & { kind: "list" },
  line: ScanLine,
  width: number,
  at: number
): Continuation {
  const last = list.last;
  const lastIsEmpty = last === undefined || last.children === 0;
  const offset = last?.kind === "item" ? last.offset : 0;
  if (width < offset || lastIsEmpty) {
    if (width < 4) {
      const marker = listMarker(line);
      if (marker !== undefined && marker.indent - offset < 4) {
        if (marker.char !== list.marker) return "close";
        // A thematic break outranks a list item, unless it underlines a paragraph as a heading.
        const fromMarker = line.text.slice(at);
        const heading = scan.opened.at(-1)?.kind === "paragraph" && setextUnderline(fromMarker);
        if (thematicBreak(fromMarker) && !heading) return "close";
        return "children";
      }
    }
    if (!lastIsEmpty) return "close";
  }
  if (lastIsEmpty && width < offset) return "close";
  // Content cannot follow an empty item and blank lines in the same list.
  if (scan.emptyItemWithBlankLines) return "close";
  return "children";
}

/** goldmark's `listItemParser.Continue`, a blank line taking no more than the item's columns. */
function continueItem(
  scan: BlockScan,
  item: ScanBlock & { kind: "item" },
  line: ScanLine
): Continuation {
  const { width } = line.indent();
  if (line.blank()) {
    line.skip(Math.min(width, item.offset));
    return "children";
  }
  const isEmpty = item.children === 0 && scan.emptyItemWithBlankLines;
  if ((isEmpty || width < item.offset) && width < 4) {
    if (listMarker(line) !== undefined) {
      scan.skipList = true;
      return "close";
    }
    if (!isEmpty) return "close";
  }
  line.skip(Math.min(width, item.offset));
  return "children";
}

/** A list item's marker opening what is left of a line. */
interface ListMarker {
  /** The columns of indentation before it. */
  readonly indent: number;
  /** The index after it. */
  readonly end: number;
  /** The character a list continues only with: the bullet, or the ordered delimiter. */
  readonly char: string;
  /** An ordered marker's number as written. */
  readonly number: string | undefined;
  /** Whether nothing but spaces and tabs follows it. */
  readonly empty: boolean;
}

/** goldmark's `parseListItem`, indentation measured in the columns its tabs span (pmdoc's tabIndented). */
function listMarker(line: ScanLine): ListMarker | undefined {
  const { width, at } = line.indent();
  if (width > 3) return undefined;
  const { text } = line;
  const first = text.charAt(at);
  let end = at;
  let number: string | undefined;
  if (first === "-" || first === "*" || first === "+") {
    end = at + 1;
  } else {
    while (end < text.length && end - at <= 9 && /[0-9]/.test(text.charAt(end))) end++;
    const delimiter = text.charAt(end);
    if (end === at || end - at > 9 || (delimiter !== "." && delimiter !== ")")) return undefined;
    number = text.slice(at, end);
    end++;
  }
  const after = text.charAt(end);
  if (end < text.length && after !== " " && after !== "\t") return undefined;
  return {
    indent: width,
    end,
    char: text.charAt(end - 1),
    number,
    empty: /^[ \t]*$/.test(text.slice(end)),
  };
}

/** goldmark's blockquote `process`: a `>`, then one column of the space or tab after it. */
function quoteMarker(line: ScanLine): boolean {
  const { width, at } = line.indent();
  if (width > 3 || line.text.charAt(at) !== ">") return false;
  line.advanceTo(at + 1);
  const next = line.text.charAt(at + 1);
  if (next === " " || next === "\t") line.skip(1);
  return true;
}

/** An ASCII punctuation character, which a backslash before it escapes (goldmark's IsPunct). */
const ASCII_PUNCTUATION = /^[\x21-\x2f\x3a-\x40\x5b-\x60\x7b-\x7e]$/;

/**
 * The index after the `]:` of the footnote label at index `at` of `text`, as goldmark's footnote
 * parser reads one (extension/footnote.go, through util.FindClosure) and pmdoc keeps it: `[^`, the
 * label, `]`, then `:` at once. A backslash before an ASCII punctuation character escapes that
 * character, so `\]` and `\[` are part of the label. An unescaped `[` makes the line no label, and
 * so does a label of nothing but spaces and tabs. Undefined where no label stands at `at`.
 */
function footnoteLabelEnd(text: string, at: number): number | undefined {
  if (!text.startsWith("[^", at)) return undefined;
  let close = at + 2;
  for (; close < text.length && text[close] !== "]"; close++) {
    if (text[close] === "[") return undefined;
    if (text[close] === "\\" && ASCII_PUNCTUATION.test(text.charAt(close + 1))) close++;
  }
  if (text.charAt(close + 1) !== ":" || /^[ \t\r]*$/.test(text.slice(at + 2, close))) {
    return undefined;
  }
  return close + 2;
}

/**
 * pmdoc's unsupportedDirectiveReason: a line of directive syntax that the server refuses, which
 * opens a block of one line. A line of three colons or more is refused where the scan's reader of
 * a typed block's opening line (BlockReading.typedOpener) reads no opener in it, unless it is
 * `:::` alone.
 */
function refusedDirective(scan: BlockScan, text: string): boolean {
  if (text.startsWith(":::")) return text !== ":::" && scan.typedOpener(text) === undefined;
  if (text.startsWith("::")) return directiveNameChar(text.charAt(2));
  return text.startsWith(":") && directiveNameChar(text.charAt(1)) && text.includes("{");
}

/** goldmark's `isThematicBreak`: three or more of one of `*`, `-`, `_`, with spaces between. */
function thematicBreak(text: string): boolean {
  const marks = text.replace(/[ \t]/g, "");
  return marks.length >= 3 && /^(?:\*+|-+|_+)$/.test(marks);
}

/** goldmark's `matchesSetextHeadingBar`: a run of `=` or of `-`, then spaces. */
function setextUnderline(text: string): boolean {
  return /^(?:=+|-+)[ \t]*$/.test(text);
}

/**
 * directive.go's `fenceColons`: the colons `text` holds when they are all it holds but spaces and
 * tabs. The colons stand between the two runs of spaces and tabs, so the pattern reads `text` in
 * time linear in its length.
 */
function fenceColons(text: string): number {
  // Group 1 is in each match, so `colons` is undefined only where `text` does not match.
  const [, colons] = /^[ \t]*(:+)[ \t]*$/.exec(text) ?? [];
  return colons?.length ?? 0;
}

/** How many times `char` stands in a row in `text` from `index`. */
export function runOf(text: string, index: number, char: string): number {
  let end = index;
  while (text.charAt(end) === char) end++;
  return end - index;
}

/** directive.go's `directiveNameByte`: a letter, a digit, `_` or `-`. */
export function directiveNameChar(char: string): boolean {
  return /^[A-Za-z0-9_-]$/.test(char);
}
