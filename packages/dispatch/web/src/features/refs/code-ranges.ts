import { readerStep } from "@legion/contracts";

/**
 * Where a text writes code, read from the text itself: the browser's twin of the server's
 * `codeRanges` (`packages/envoy/internal/dispatch/text/code.go`), which the two readers of a
 * party reference are held together by. The rendered body masks code by what the markdown engine
 * built - a text node under `A`, `CODE` or `PRE` is left alone (`RefLink.tsx`) - and neither a
 * draft sitting in a textarea nor the one reader of what a text cites (`text-references.ts`) has
 * a parse to ask: both read the characters, as the server does, and must read them the same way
 * it does on every row of `DISPATCH_TEXT_REFERENCES`.
 *
 * Every line is read through the containers open at it - a block quote's `>` with the one space
 * after it, a list item's own content indentation, a footnote definition's four columns - so a
 * block begins and ends where CommonMark begins and ends it inside the container holding it: a
 * fence written inside a quote or an item is code, and the four spaces an ordinary nested bullet
 * carries, like the four a footnote definition's content stands in, are that container's own
 * text.
 *
 * The three contexts, and nothing else:
 *
 *   - A fenced code block. Its opening line carries, past the containers open at it, at most
 *     three columns of indentation and then three or more backticks or tildes, a backtick
 *     fence's info string holding no backtick of its own. It closes on the first line that holds
 *     those same containers and carries at most three columns of indentation, a run of the same
 *     character at least as long as the opener, and nothing after it but spaces and tabs; a
 *     fence nothing closes runs to the end of the text, and one whose containers a later line
 *     leaves ends with the line before that one. Both fence lines are part of the range, so an
 *     address written in an info string is in code too.
 *   - An indented code block: a line carrying four columns past where its containers' content
 *     starts, where such a line may open one at all - the line before it is blank or there is
 *     none, four columns under a paragraph being that paragraph's own text. It runs through the
 *     indented lines that follow, blank lines between them included, and ends at the last of
 *     them.
 *   - A code span, as CommonMark reads one: a run of N backticks opens one and the next run of
 *     exactly N closes it, the search ending where the paragraph does, which is every line
 *     `codeBlocks` records a break at (its `ends` and `heading` assignments), so a run nothing
 *     closes inside that paragraph is literal text. A backslash takes the backtick after it as
 *     a literal character, which opens no span, while a run that closes one is read as written
 *     (`` `a\` `` closes), which is where CommonMark puts the escape too. Spans are read in the
 *     text the blocks leave, so a backtick inside a fenced block opens nothing and no span
 *     reaches across a block.
 *
 * The containers it reads are a block quote, nested in another or not; a list item of a `-`,
 * `+`, `*` or numbered marker, whose content column is where the item's own text starts - a
 * marker followed by five columns or more of spaces leaving the rest of them to indented code
 * inside the item, and one that cannot interrupt a paragraph (an empty item, or a number other
 * than 1) opening nothing where the paragraph is the open tip; and a footnote definition,
 * whose content stands four columns in. A line carrying none of the containers it stands in
 * while a paragraph is open is that paragraph's own text, a lazy continuation line, which
 * opens nothing and leaves those containers open; any other line closes the containers it does
 * not carry. A thematic break is read too, since a line of `-`, `_` or `*` would otherwise
 * open a list item. A quote or a list opened past the renderer's bound on nesting opens
 * nothing, its marker being the line's own text as the renderer reads it
 * (`MAX_BLOCK_NESTING`, which says how blocks are counted), so a line of thousands of markers
 * opens at most a hundred containers.
 *
 * What it deliberately does not read. Each of these was measured on 2026-11-11 against three
 * parsers - goldmark 1.8.6 with the footnote, table and linkify extensions; this editor's own
 * parse (`proof.parseMarkdown`, remark and Milkdown's commonmark preset); and `pmdoc.Parse`,
 * the server's Milkdown-matched goldmark - on a body holding one reference, and what each
 * answered is written beside it. What keeps each of them out is written in its own bullet: for
 * the first, that the parsers disagree with one another and pmdoc refuses the body; for the
 * rest, that reading them would cost both readers a parse they do not make. A
 * shape every parser reads alike and this reader does not is a defect, and is fixed rather
 * than listed here.
 *
 *   - A table's rows, with an indented line under them: this reader and goldmark read that line
 *     as the paragraph's own text, this editor's parse reads it as a code block, and pmdoc
 *     refuses the body, naming that disagreement. So the two parses available answer
 *     differently, and the party written there is delivered to while the editor draws it as
 *     code.
 *   - A block of HTML or a link reference definition, with an indented line under it: this
 *     reader, goldmark and the editor all read that line as prose, and pmdoc refuses the body.
 *     Nothing is measurably wrong there today; it stays unread because the construct above it
 *     is. Under an underline they part: a link reference definition, or an HTML block that
 *     closes on its own line, with a `-` or `=` run under it, takes no setext underline in
 *     goldmark or in remark, because the paragraph above the run is no paragraph of theirs;
 *     this reader has no paragraph-level state to see that, so it ends the paragraph there and
 *     reads the indented line under the underline as code - the underline's own line is not in
 *     the range - muting a party both parsers draw as a link. Closing it needs that state
 *     rather than a condition, and the shapes are exotic, so they are recorded here rather than
 *     fixed.
 *   - Tab stops a container's marker splits. A tab is measured to the next four-column stop, but
 *     one straddling a container's own content column is taken whole by that container, so a tab
 *     after a list marker is the item's indentation rather than indented code inside it. All
 *     four readers answer alike on the body measured; the shapes where they part are unmeasured.
 *   - A code span crossing a construct only a real parse resolves. The paragraph a table's rows
 *     or a block of HTML ends carries on for this reader, so a run opened before one can be
 *     closed by a run after it.
 */

/** A half-open range of a text, `[start, end)`, and whether it is a fenced or an indented code
 *  block rather than a code span, as the server's `codeRange` marks it where it places the
 *  range, so a reader that wants one of the two tells them apart without working it out again. */
export interface CodeRange {
  readonly start: number;
  readonly end: number;
  readonly block: boolean;
}

/** One container open at the line being read - a block quote, a list item or a footnote
 *  definition - which is what the line carries before its own content. A block quote is held
 *  by its `>` marker; the other two by the column their own content starts at, which the lines
 *  under them carry as indentation and which is where code inside them opens from. */
interface Container {
  readonly quote: boolean;
  readonly content: number;
  /** A list item's type, as CommonMark says two markers are of the same type: its bullet, or
   *  an ordered item's `.` or `)`. A later item of the same list repeats it. 0 for a block
   *  quote and a footnote definition. */
  readonly listType: number;
  /** How many blocks the renderer counts around this container's content, which a container
   *  opened inside it on a later line counts from (`MAX_BLOCK_NESTING`): a quote's own count,
   *  the count of the list an item belongs to, and, for a footnote definition, which the
   *  renderer does not count, the count of the container around it. */
  readonly nest: number;
}

/** Where a scan of a line stopped, which the server's reader returns beside its offset: the
 *  column there, a list item's own content column, and the type of its marker (`listType`).
 *  One record is made per reading of a text and written by every scan of it, so a body of many
 *  lines allocates no more than a body of one. */
interface Scan {
  at: number;
  column: number;
  content: number;
  listType: number;
}

const BACKTICK = 0x60;
const BACKSLASH = 0x5c;
const TILDE = 0x7e;
const SPACE = 0x20;
const TAB = 0x09;
const CARRIAGE_RETURN = 0x0d;
const GREATER_THAN = 0x3e;
const NUMBER_SIGN = 0x23;
const HYPHEN = 0x2d;
const PLUS = 0x2b;
const ASTERISK = 0x2a;
const UNDERSCORE = 0x5f;
const PERIOD = 0x2e;
const CLOSE_PAREN = 0x29;
const DIGIT_ZERO = 0x30;
const DIGIT_NINE = 0x39;
const EQUALS = 0x3d;
const OPEN_BRACKET = 0x5b;
const CLOSE_BRACKET = 0x5d;
const CARET = 0x5e;
const COLON = 0x3a;

/** The stop a tab advances the column to, as CommonMark measures indentation. */
const TAB_WIDTH = 4;

/**
 * How many blocks a text may open one inside another before the renderer reads the next opener
 * as the text it is: `maxBlockNesting` in `packages/proof-editor/src/block-nesting.ts`, which is
 * `maxNesting` in `packages/envoy/internal/dispatch/pmdoc/nesting.go`. A refused opener's marker
 * and the rest of its line are the content of the innermost container still open, so the four
 * columns after such a marker are that content's own text and open no code block.
 *
 * It is counted as the renderer counts it. A list item that opens a list counts two, the list
 * and the item; a later item of the list open at its depth counts nothing, standing where the
 * first item did, and is never refused. A block quote counts one; a `>` that carries on a quote
 * open above it is that quote's and opens nothing. A footnote definition counts nothing: pmdoc's
 * server-side bound counts one, but the renderer reads definitions through
 * micromark-extension-gfm-footnote, which its bound does not reach, and this reader follows what
 * a reader of the text sees. A container opened after a footnote definition on the
 * definition's own line counts from 0, since the renderer takes a container with no count of
 * its own as 0 there (`blocksAround`), and one opened inside the definition on a later line
 * counts from the container around the definition. A typed block (`:::`) counts one for the
 * renderer but is no container of this reader's, so a list or a quote inside typed blocks is
 * counted one block less per typed block than the renderer counts it.
 */
const MAX_BLOCK_NESTING = 100;

/** Where the line starting at `at` ends, before its line ending: at the `\n` that ends it, at
 *  the `\r` of a `\r\n`, or at the end of the text. A lone `\r` is the line's own character, as
 *  it is for the server's `lineBounds`. */
function lineEnd(text: string, at: number): number {
  const broken = text.indexOf("\n", at);
  if (broken < 0) {
    return text.length;
  }
  return broken > at && text.charCodeAt(broken - 1) === CARRIAGE_RETURN ? broken - 1 : broken;
}

/** Where the line whose characters end at `end` leaves off: the start of the next line, or the
 *  end of the text when this was the last. A `\r` standing there is a `\r\n`'s, since `lineEnd`
 *  stops before only that one. */
function nextLine(text: string, end: number): number {
  if (end >= text.length) {
    return text.length;
  }
  return text.charCodeAt(end) === CARRIAGE_RETURN ? end + 2 : end + 1;
}

/** Where the run of `character` starting at `at` ends, within `[at, end)`. */
function runEnd(text: string, at: number, end: number, character: number): number {
  let past = at;
  while (past < end && text.charCodeAt(past) === character) {
    readerStep();
    past += 1;
  }
  return past;
}

/** Whether `[at, end)` holds nothing but spaces and tabs. */
function isBlank(text: string, at: number, end: number): boolean {
  for (let index = at; index < end; index += 1) {
    readerStep();
    const character = text.charCodeAt(index);
    if (character !== SPACE && character !== TAB) {
      return false;
    }
  }
  return true;
}

/** Reads into `scan` where the spaces and tabs starting at `at` end, within `[at, end)`, and the
 *  column there, read from column `column`: a tab advances to the next four-column stop. It
 *  lands on `end` where the rest of the line is blank. */
function indentEnd(text: string, at: number, end: number, column: number, scan: Scan): void {
  let past = at;
  let reached = column;
  while (past < end) {
    readerStep();
    const character = text.charCodeAt(past);
    if (character === SPACE) {
      reached += 1;
    } else if (character === TAB) {
      reached += TAB_WIDTH - (reached % TAB_WIDTH);
    } else {
      break;
    }
    past += 1;
  }
  scan.at = past;
  scan.column = reached;
}

/** Reads into `scan` where the spaces and tabs starting at `at` reach column `want`, and the
 *  column there: the indentation a container takes off a line, the rest of it being the line's
 *  own. A tab is never split, so one straddling `want` lands past it. */
function indentTo(
  text: string,
  at: number,
  end: number,
  column: number,
  want: number,
  scan: Scan
): void {
  let past = at;
  let reached = column;
  while (past < end && reached < want) {
    readerStep();
    const character = text.charCodeAt(past);
    if (character === SPACE) {
      reached += 1;
    } else if (character === TAB) {
      reached += TAB_WIDTH - (reached % TAB_WIDTH);
    } else {
      break;
    }
    past += 1;
  }
  scan.at = past;
  scan.column = reached;
}

/** Reads into `scan` where the content of the block quote whose `>` stands at `marker`, in
 *  column `column`, begins and the column there: past the marker and past the one space or tab
 *  that may follow it, which the quote takes as its own. */
function quoteContent(text: string, marker: number, end: number, column: number, scan: Scan): void {
  const past = marker + 1;
  const reached = column + 1;
  if (past < end) {
    const character = text.charCodeAt(past);
    if (character === SPACE) {
      scan.at = past + 1;
      scan.column = reached + 1;
      return;
    }
    if (character === TAB) {
      scan.at = past + 1;
      scan.column = reached + TAB_WIDTH - (reached % TAB_WIDTH);
      return;
    }
  }
  scan.at = past;
  scan.column = reached;
}

/** Whether a list item marker stands at `marker`, the first character of a line's own content,
 *  in column `column`: a `-`, `+` or `*`, or one to nine digits and a `.` or `)`, followed by a
 *  space, a tab or the end of the line. `scan` reads where the item's content begins on this
 *  line, the column there, the column the item's content is indented to, which the lines under
 *  it carry to stay inside it, and the marker's type (`Container.listType`). Five columns or
 *  more of spaces after the marker are indented code inside the item, so the item takes one of
 *  them and leaves the rest. A marker that cannot interrupt a paragraph opens nothing while one
 *  is open: an empty item's, and an ordered one that does not count from 1. */
function listMarker(
  text: string,
  marker: number,
  end: number,
  column: number,
  paragraph: boolean,
  scan: Scan
): boolean {
  let past = marker;
  const character = text.charCodeAt(marker);
  if (character === HYPHEN || character === PLUS || character === ASTERISK) {
    past += 1;
  } else {
    while (past < end) {
      readerStep();
      const digit = text.charCodeAt(past);
      if (digit < DIGIT_ZERO || digit > DIGIT_NINE) {
        break;
      }
      past += 1;
    }
    if (past === marker || past - marker > 9 || past >= end) {
      return false;
    }
    const delimiter = text.charCodeAt(past);
    if (delimiter !== PERIOD && delimiter !== CLOSE_PAREN) {
      return false;
    }
    if (paragraph && (past - marker !== 1 || text.charCodeAt(marker) !== DIGIT_ZERO + 1)) {
      return false;
    }
    past += 1;
  }
  const width = column + past - marker;
  // The bullet, or the `.` or `)` an ordered marker ends with: the last character of either.
  scan.listType = text.charCodeAt(past - 1);
  indentEnd(text, past, end, width, scan);
  const spaces = scan.at;
  const spacesColumn = scan.column;
  if (spaces >= end) {
    if (paragraph) {
      return false;
    }
    scan.at = past;
    scan.column = width;
    scan.content = width + 1;
    return true;
  }
  if (spaces === past) {
    return false;
  }
  if (spacesColumn - width > 4 && text.charCodeAt(past) === SPACE) {
    scan.at = past + 1;
    scan.column = width + 1;
    scan.content = width + 1;
    return true;
  }
  scan.at = spaces;
  scan.column = spacesColumn;
  scan.content = spacesColumn;
  return true;
}

/** Whether the content of a line at `from` is a thematic break: three or more of `-`, `_` or
 *  `*`, all the same one, and nothing else but spaces and tabs. It opens no list item and no
 *  paragraph, so an indented line right under one opens a code block, as CommonMark opens one
 *  there. */
function isThematicBreak(text: string, from: number, end: number): boolean {
  const character = text.charCodeAt(from);
  if (character !== HYPHEN && character !== UNDERSCORE && character !== ASTERISK) {
    return false;
  }
  let count = 0;
  for (let at = from; at < end; at += 1) {
    readerStep();
    const current = text.charCodeAt(at);
    if (current === character) {
      count += 1;
    } else if (current !== SPACE && current !== TAB) {
      return false;
    }
  }
  return count >= 3;
}

/** Whether the content of a line at `from`, past its containers and its own indentation, is an
 *  ATX heading: one to six `#` and then a space, a tab or the end of the line. It is no
 *  paragraph's line and opens none, so it ends the paragraph above it and an indented line
 *  right under it opens a code block, as CommonMark opens one there. */
function isATXHeading(text: string, from: number, end: number): boolean {
  const at = runEnd(text, from, end, NUMBER_SIGN);
  if (at === from || at - from > 6) {
    return false;
  }
  if (at >= end) {
    return true;
  }
  const character = text.charCodeAt(at);
  return character === SPACE || character === TAB;
}

/** Whether the content of a line at `from`, past its containers and its own indentation, is a
 *  run of `=` or a run of `-` and nothing else but trailing spaces and tabs. Such a line is the
 *  underline of the paragraph it closes, and a run of one or two `-` is one as much as the
 *  three that would otherwise be a thematic break.
 *
 *  Being an underline is not this line's own property: its caller reads it only where a
 *  paragraph is open, where the line carried every container that paragraph stands in, and
 *  where it opened none of its own, since a line that left those containers is a lazy
 *  continuation of the paragraph and never an underline (CommonMark, example 93). */
function isSetextUnderline(text: string, from: number, end: number): boolean {
  if (from >= end) {
    return false;
  }
  const character = text.charCodeAt(from);
  if (character !== EQUALS && character !== HYPHEN) {
    return false;
  }
  const at = runEnd(text, from, end, character);
  return isBlank(text, at, end);
}

/** Where the content of a footnote definition opening at `from` begins - `[^label]:` with a
 *  label of at least one character holding no bracket, no space and no tab, then the one space
 *  or tab that may follow the colon - or `undefined` where the line opens none. */
function footnoteDefinition(text: string, from: number, end: number): number | undefined {
  if (
    from + 1 >= end ||
    text.charCodeAt(from) !== OPEN_BRACKET ||
    text.charCodeAt(from + 1) !== CARET
  ) {
    return undefined;
  }
  let at = from + 2;
  while (at < end && text.charCodeAt(at) !== CLOSE_BRACKET) {
    readerStep();
    const character = text.charCodeAt(at);
    if (character === OPEN_BRACKET || character === SPACE || character === TAB) {
      return undefined;
    }
    at += 1;
  }
  if (at >= end || at === from + 2 || at + 1 >= end || text.charCodeAt(at + 1) !== COLON) {
    return undefined;
  }
  at += 2;
  const after = at < end ? text.charCodeAt(at) : 0;
  return after === SPACE || after === TAB ? at + 1 : at;
}

/** How many of `stack`'s containers, in order, hold the line `[at, end)`, with `scan` reading
 *  where the content of the innermost of those begins and the column there. A block quote holds
 *  a line carrying its `>` under four columns of indentation, the marker and one space or tab
 *  after it belonging to the quote; a list item holds a line indented to the item's content
 *  column, and a blank line, which no block quote holds. */
function matchContainers(
  text: string,
  at: number,
  end: number,
  stack: readonly Container[],
  scan: Scan
): number {
  let content = at;
  let column = 0;
  let matched = 0;
  while (matched < stack.length) {
    readerStep();
    const open = stack[matched];
    if (open.quote) {
      indentEnd(text, content, end, column, scan);
      const marker = scan.at;
      if (marker >= end || scan.column - column > 3 || text.charCodeAt(marker) !== GREATER_THAN) {
        break;
      }
      quoteContent(text, marker, end, scan.column, scan);
      content = scan.at;
      column = scan.column;
      matched += 1;
      continue;
    }
    if (isBlank(text, content, end)) {
      matched += 1;
      continue;
    }
    indentTo(text, content, end, column, open.content, scan);
    if (scan.column < open.content) {
      break;
    }
    content = scan.at;
    column = scan.column;
    matched += 1;
  }
  scan.at = content;
  scan.column = column;
  return matched;
}

/** The opening fence of a line's content at `at`, past its containers and its own indentation,
 *  which its caller has read as under four columns: the character it is written with and how
 *  long its run is, or undefined where the line opens no fence. A backtick fence whose info
 *  string holds a backtick is no fence at all, that backtick being a code span's. */
function fenceOpener(
  text: string,
  at: number,
  end: number
): { character: number; width: number } | undefined {
  const character = text.charCodeAt(at);
  if (character !== BACKTICK && character !== TILDE) {
    return undefined;
  }
  const past = runEnd(text, at, end, character);
  if (past - at < 3) {
    return undefined;
  }
  if (character === BACKTICK) {
    for (let index = past; index < end; index += 1) {
      readerStep();
      if (text.charCodeAt(index) === BACKTICK) {
        return undefined;
      }
    }
  }
  return { character, width: past - at };
}

/** Whether a line's content at `at`, its own indentation read as under four columns, closes a
 *  fence of `character` opened with `width` of them: a run at least as long, and nothing after
 *  it but spaces and tabs. */
function closesFence(
  text: string,
  at: number,
  end: number,
  character: number,
  width: number
): boolean {
  const past = runEnd(text, at, end, character);
  return past - at >= width && isBlank(text, past, end);
}

/** What one walk of a text's lines reads: the code blocks it holds, and where each of its
 *  paragraphs ends. */
interface Blocks {
  /** The fenced and indented code blocks, in order, each from the first character of its
   *  opening line to the end of its last line. */
  blocks: CodeRange[];
  /** The start of every line that ends the paragraph above it, in order, which is where a code
   *  span's search for its closing run ends too (`appendCodeSpans`). */
  breaks: number[];
}

/** The code blocks and paragraph breaks of `text`, read in one walk of its lines. The walk holds
 *  the containers open at the line, whether the line before it was a paragraph's - which is what
 *  an indented line needs not to be to open a block of its own - and the code block the line
 *  stands in, if any. A block's range runs from the start of its opening line, containers'
 *  markers included, since nothing of a line before the code is a reference. */
function codeBlocks(text: string): Blocks {
  const blocks: CodeRange[] = [];
  const breaks: number[] = [];
  const scan: Scan = { at: 0, column: 0, content: 0, listType: 0 };
  const stack: Container[] = [];
  let paragraph = false;
  let fence: { character: number; width: number } | undefined;
  let indentedOpen = false;
  let start = 0;
  let last = 0;

  for (let at = 0; at < text.length; ) {
    readerStep();
    const end = lineEnd(text, at);
    const next = nextLine(text, end);
    const matched = matchContainers(text, at, end, stack, scan);
    let content = scan.at;
    let column = scan.column;
    indentEnd(text, content, end, column, scan);
    let indent = scan.at;
    let reached = scan.column;

    if (fence !== undefined) {
      if (matched === stack.length) {
        if (reached - column <= 3 && closesFence(text, indent, end, fence.character, fence.width)) {
          blocks.push({ block: true, end: next, start });
          fence = undefined;
        }
        at = next;
        continue;
      }
      // The line left the containers the fence opened in, and no code block is carried on by a
      // line that leaves them: the block ends with the line before this one, which is then read
      // as any other.
      blocks.push({ block: true, end: at, start });
      fence = undefined;
    } else if (indentedOpen) {
      if (matched === stack.length) {
        if (indent >= end) {
          // A blank line holds the block open; the blanks trailing it are not its own, so `last`
          // stays where the last indented line ended.
          at = next;
          continue;
        }
        if (reached - column >= 4) {
          last = next;
          at = next;
          continue;
        }
      }
      blocks.push({ block: true, end: last, start });
      indentedOpen = false;
    }

    // The containers this line opens of its own, each inside the last: its content is what is
    // left after them. `around` is how many blocks the renderer counts around the next of them
    // (`MAX_BLOCK_NESTING`): at first the count of the innermost container the line carried,
    // then that of the last one it opened.
    let depth = matched;
    let around = matched > 0 ? stack[matched - 1].nest : 0;
    let thematic = false;
    let setext = false;
    while (indent < end && reached - column <= 3) {
      readerStep();
      if (
        paragraph &&
        depth === matched &&
        matched === stack.length &&
        isSetextUnderline(text, indent, end)
      ) {
        // A run of `=` or `-` under the paragraph it closes, read only where the line carried
        // every container that paragraph stands in and opened none of its own: a line that
        // left those containers is a lazy continuation of the paragraph, which CommonMark
        // never reads as an underline.
        setext = true;
        break;
      }
      if (text.charCodeAt(indent) === GREATER_THAN) {
        if (around + 1 > MAX_BLOCK_NESTING) {
          // A quote past the bound opens nothing: its `>` is the line's own text.
          break;
        }
        around += 1;
        stack.length = depth;
        stack.push({ content: 0, listType: 0, nest: around, quote: true });
        depth += 1;
        quoteContent(text, indent, end, reached, scan);
        content = scan.at;
        column = scan.column;
        paragraph = false;
        indentEnd(text, content, end, column, scan);
        indent = scan.at;
        reached = scan.column;
        continue;
      }
      if (isThematicBreak(text, indent, end)) {
        thematic = true;
        break;
      }
      const note = footnoteDefinition(text, indent, end);
      if (note !== undefined) {
        // A footnote definition is a container whose content stands four columns past where
        // its own container's content begins - not four past the marker, which a definition
        // indented one to three spaces would put too far right, reading its note's lines as an
        // indented code block. So the indented lines under it are the note's own text: a party
        // written there is named to every reader of the record, and every parser measured
        // reads those lines as prose. The renderer counts no block for it, so a
        // container opened inside it on a later line counts from the one around it, and one
        // opened after it on this line counts from 0 (`MAX_BLOCK_NESTING`).
        stack.length = depth;
        stack.push({
          content: column + 4,
          listType: 0,
          nest: depth > 0 ? stack[depth - 1].nest : 0,
          quote: false,
        });
        around = 0;
        depth += 1;
        content = note;
        column = reached + (note - indent);
        paragraph = false;
        indentEnd(text, content, end, column, scan);
        indent = scan.at;
        reached = scan.column;
        continue;
      }
      // CommonMark holds an empty item's marker and an ordered one that does not count from 1
      // back only where the paragraph is the open tip: a line that left a container the
      // paragraph stands in starts the next item instead, so item 2 of a numbered list ends
      // item 1's paragraph as a bullet does.
      if (!listMarker(text, indent, end, reached, paragraph && matched === stack.length, scan)) {
        break;
      }
      // A later item of the list open at this depth, on a line that carried every container
      // around that list, goes on in it where its first item stood, and the renderer never
      // refuses it; any other item opens a list of its own, which past the bound opens nothing
      // and leaves its marker as the line's own text.
      const open = depth === matched && depth < stack.length ? stack[depth] : undefined;
      let nest = around + 2;
      if (open !== undefined && open.listType === scan.listType) {
        nest = open.nest;
      } else if (nest > MAX_BLOCK_NESTING) {
        break;
      }
      around = nest;
      const itemContent = scan.at;
      const itemColumn = scan.column;
      stack.length = depth;
      stack.push({ content: scan.content, listType: scan.listType, nest, quote: false });
      depth += 1;
      content = itemContent;
      column = itemColumn;
      paragraph = false;
      indentEnd(text, content, end, column, scan);
      indent = scan.at;
      reached = scan.column;
    }

    // A line opening a container of its own - a list item's marker, a `>` - interrupts the
    // paragraph above it.
    let ends = depth > matched;
    let heading = false;
    let prose = false;
    if (setext) {
      // The underline the loop above read: it ends the paragraph it closes, and the break on
      // the line under it leaves the heading's own text to itself, as an ATX heading's does.
      // An indented line under it therefore opens a code block, which is what every parser
      // measured reads there.
      ends = true;
      heading = true;
    } else if (indent >= end || thematic) {
      // A line blank past the containers it carries - a bare `>` among them - and a thematic
      // break open nothing and end the paragraph above them.
      ends = true;
    } else if (reached - column >= 4 && !paragraph) {
      indentedOpen = true;
      start = at;
      last = next;
    } else if (reached - column <= 3) {
      const opener = fenceOpener(text, indent, end);
      if (opener !== undefined) {
        fence = opener;
        start = at;
      } else if (isATXHeading(text, indent, end)) {
        // An ATX heading is no paragraph's line and opens none: it ends the paragraph above it,
        // and a break on the line under it leaves the heading's own text to itself, where a
        // span of its own may stand.
        ends = true;
        heading = true;
      } else {
        prose = true;
      }
    } else {
      // Four columns or more under a paragraph are that paragraph's own text.
      prose = true;
    }
    if (ends) {
      breaks.push(at);
    }
    if (heading) {
      breaks.push(next);
    }
    // A line that carries none of the containers it stands in, opens nothing and continues the
    // paragraph above it is that paragraph's own text: the containers stay open for the lines
    // after it. Every other line closes the ones it does not carry.
    if (!prose || !paragraph || depth > matched) {
      stack.length = depth;
    }
    paragraph = prose;
    at = next;
  }

  if (fence !== undefined) {
    blocks.push({ block: true, end: text.length, start });
  } else if (indentedOpen) {
    blocks.push({ block: true, end: last, start });
  }
  return { blocks, breaks };
}

/** The runs of backticks of one width that the search for closing runs has read and closed no
 *  span with (`closingRun`), and how far the searches of that width have passed through them. */
interface Runs {
  /** Where each run of this width starts, in order. */
  readonly starts: number[];
  /** The index in `starts` of the first run no search of this width has passed. */
  next: number;
}

/** What reading the code spans of a text carries from one stretch of it to the next, the
 *  stretches the blocks leave and the paragraphs in them being read in order (`codeRanges`). */
interface SpanReading {
  readonly text: string;
  /** The paragraph breaks `codeBlocks` read, in order. */
  readonly breaks: readonly number[];
  /** The index in `breaks` of the first break the reading has not passed. */
  nextBreak: number;
  /** Where the search for closing runs stopped reading the text (`closingRun`). */
  scanned: number;
  /** The runs of backticks that search read and closed no span with, by width. */
  readonly runs: Map<number, Runs>;
  /** The code contexts read so far, in order. */
  readonly found: CodeRange[];
}

/** Where the first run of exactly `width` backticks starting in `[from, to)` starts, or -1 when
 *  none does: a run of any other length closes no span of that width. `from` is where an
 *  opener's run ends, and a text's openers are read in order, so the text is read for runs once,
 *  each search going on from where the last one stopped (`scanned`) and filing every run it reads
 *  that closes nothing by its width, where a later opener of that width finds it; each width's
 *  runs are passed once however many openers look among them. cmark, CommonMark's reference
 *  parser, likewise keeps where it read the runs of each length (`scan_to_closing_backticks`)
 *  rather than reading the rest of the paragraph again from each opener. */
function closingRun(reading: SpanReading, width: number, from: number, to: number): number {
  const same = reading.runs.get(width);
  if (same !== undefined) {
    const { starts } = same;
    while (same.next < starts.length && starts[same.next] < from) {
      readerStep();
      same.next += 1;
    }
    if (same.next < starts.length && starts[same.next] < to) {
      return starts[same.next];
    }
  }
  const { text } = reading;
  for (let at = Math.max(reading.scanned, from); at < to; ) {
    readerStep();
    if (text.charCodeAt(at) !== BACKTICK) {
      at += 1;
      continue;
    }
    const end = runEnd(text, at, to, BACKTICK);
    if (end - at === width) {
      reading.scanned = end;
      return at;
    }
    const other = reading.runs.get(end - at);
    if (other === undefined) {
      reading.runs.set(end - at, { next: 0, starts: [at] });
    } else {
      other.starts.push(at);
    }
    at = end;
  }
  reading.scanned = to;
  return -1;
}

/** The code spans of `[from, to)`, a stretch of the text no paragraph break cuts, appended to
 *  what `reading` found, in order: the run of backticks at a span's start is closed by the next
 *  run of exactly that length, a longer run being no closer, and a run with no closer is literal
 *  text that the next character is read on from. A backslash takes the character after it, so
 *  an escaped backtick opens no span; a run that closes one is read as written, as CommonMark
 *  reads a closer. The closer is found by `closingRun`, which reads each run of the text once,
 *  never by reading the rest of the paragraph again from each opener. */
function appendParagraphSpans(reading: SpanReading, from: number, to: number): void {
  const { text } = reading;
  for (let at = from; at < to; ) {
    readerStep();
    const character = text.charCodeAt(at);
    if (character === BACKSLASH) {
      at += 2;
      continue;
    }
    if (character !== BACKTICK) {
      at += 1;
      continue;
    }
    const opener = runEnd(text, at, to, BACKTICK);
    const width = opener - at;
    const closer = closingRun(reading, width, opener, to);
    if (closer < 0) {
      at = opener;
      continue;
    }
    reading.found.push({ block: false, end: closer + width, start: at });
    at = closer + width;
  }
}

/** The code spans of `[from, to)` appended to what `reading` found, in order, each read inside
 *  the paragraph that holds it: the search for a span's closing run ends where that paragraph
 *  ends, at the first of the breaks past where the search began, which `codeBlocks` writes one
 *  of for every line that ends a paragraph, so an unmatched backtick mutes nothing written past
 *  its own paragraph. The stretches are read in order, so the breaks are walked once for all of
 *  them, each going on from where the last one stopped. */
function appendCodeSpans(reading: SpanReading, from: number, to: number): void {
  const { breaks } = reading;
  while (reading.nextBreak < breaks.length && breaks[reading.nextBreak] <= from) {
    readerStep();
    reading.nextBreak += 1;
  }
  let start = from;
  for (; reading.nextBreak < breaks.length && breaks[reading.nextBreak] < to; reading.nextBreak++) {
    readerStep();
    appendParagraphSpans(reading, start, breaks[reading.nextBreak]);
    start = breaks[reading.nextBreak];
  }
  appendParagraphSpans(reading, start, to);
}

/** Every code context of `text`, in order and never overlapping: the fenced and indented blocks,
 *  and the code spans of the text they leave. It is read once per reading of a text, by the one
 *  pass that reading already makes (`readTextReferences`), and every reference is marked from it
 *  with `codeCursor`. */
export function codeRanges(text: string): CodeRange[] {
  const { blocks, breaks } = codeBlocks(text);
  const reading: SpanReading = {
    breaks,
    found: [],
    nextBreak: 0,
    runs: new Map(),
    scanned: 0,
    text,
  };
  let at = 0;
  for (const block of blocks) {
    readerStep();
    appendCodeSpans(reading, at, block.start);
    reading.found.push(block);
    at = block.end;
  }
  appendCodeSpans(reading, at, text.length);
  return reading.found;
}

/** Whether the character at each offset it is asked about sits in one of `ranges`, which
 *  `codeRanges` wrote in order: the reference whose first character it is was written in a code
 *  context. The offsets are asked in order, never one before the last, so each answer goes on
 *  through the ranges from where the last one stopped, and a text's references cost one walk of
 *  its ranges rather than a search of them each. */
export function codeCursor(ranges: readonly CodeRange[]): (offset: number) => boolean {
  let next = 0;
  return (offset) => {
    while (next < ranges.length && ranges[next].end <= offset) {
      readerStep();
      next += 1;
    }
    return next < ranges.length && ranges[next].start <= offset;
  };
}
