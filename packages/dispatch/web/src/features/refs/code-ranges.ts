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
 * after it, a list item's own content indentation - so a block begins and ends where CommonMark
 * begins and ends it inside the container holding it: a fence written inside a quote or an item
 * is code, and the four spaces an ordinary nested bullet carries are the item's own text.
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
 *     exactly N closes it, the search ending where the paragraph does - at a blank line, at a
 *     line blank past the containers it carries such as a bare `>` inside a quote, at a list
 *     item's marker, at an ATX heading, at a thematic break - so a run nothing closes inside
 *     that paragraph is literal text. A backslash takes the backtick after it as a literal
 *     character, which opens no span, while a run that closes one is read as written
 *     (`` `a\` `` closes), which is where CommonMark puts the escape too. Spans are read in the
 *     text the blocks leave, so a backtick inside a fenced block opens nothing and no span
 *     reaches across a block.
 *
 * The containers it reads are a block quote, nested in another or not, and a list item of a `-`,
 * `+`, `*` or numbered marker, whose content column is where the item's own text starts - a
 * marker followed by five columns or more of spaces leaving the rest of them to indented code
 * inside the item, and one that cannot interrupt a paragraph (an empty item, or a number other
 * than 1) opening nothing. A line carrying none of the containers it stands in while a paragraph
 * is open is that paragraph's own text, a lazy continuation line, which opens nothing and leaves
 * those containers open; any other line closes the containers it does not carry. A thematic
 * break is read too, since a line of `-`, `_` or `*` would otherwise open a list item.
 *
 * What it deliberately does not read, each because the server does not and a markdown parse is
 * what either would cost:
 *
 *   - A line that is no paragraph's and opens none: a setext heading's underline, a table's
 *     rows, a block of HTML, a link reference definition. An indented line right under one is
 *     read as that paragraph's text, so a code block opening there is read as prose and the
 *     party it holds is delivered to, where no reader of the record sees one named. An ATX
 *     heading is not one of them: it is read, and ends the paragraph above it.
 *   - A footnote definition's container. Its content, four columns in, is read as an indented
 *     code block where a blank line stands between it and the `[^id]:` line - a party written
 *     there is delivered to nobody, which is the silent half of being wrong.
 *   - Tab stops a container's marker splits. A tab is measured to the next four-column stop, but
 *     one straddling a container's own content column is taken whole by that container, so a tab
 *     after a list marker is the item's indentation rather than indented code inside it: code
 *     read as prose again.
 *   - A code span crossing a construct only a real parse resolves. The paragraph a line of the
 *     first kind ends carries on for this reader, so a run opened before a setext heading's
 *     underline, a table's rows or a block of HTML can be closed by a run after it.
 *
 * `codeSpans` is also the one reading of where a code span begins on this side: the snippet
 * cutter reads it from here (`markdown-fragment.ts`) rather than keeping a second copy of the
 * backtick-run rule, so a span cannot start in one place and not in the other.
 */

/** A half-open range of a text, `[start, end)`. */
export interface CodeRange {
  readonly start: number;
  readonly end: number;
}

/** One block quote or list item open at the line being read: what the line carries before its
 *  own content. A block quote is held by its `>` marker; a list item by the column its own
 *  content starts at, which the lines under it carry as indentation and which is where code
 *  inside that item opens from. */
interface Container {
  readonly quote: boolean;
  readonly content: number;
}

/** Where a scan of a line stopped, which the server's reader returns beside its offset: the
 *  column there, and a list item's own content column. One record is made per reading of a text
 *  and written by every scan of it, so a body of many lines allocates no more than a body of
 *  one. */
interface Scan {
  at: number;
  column: number;
  content: number;
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

/** The stop a tab advances the column to, as CommonMark measures indentation. */
const TAB_WIDTH = 4;

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
    past += 1;
  }
  return past;
}

/** Whether `[at, end)` holds nothing but spaces and tabs. */
function isBlank(text: string, at: number, end: number): boolean {
  for (let index = at; index < end; index += 1) {
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
 *  line, the column there, and the column the item's content is indented to, which the lines
 *  under it carry to stay inside it. Five columns or more of spaces after the marker are
 *  indented code inside the item, so the item takes one of them and leaves the rest. A marker
 *  that cannot interrupt a paragraph opens nothing while one is open: an empty item's, and an
 *  ordered one that does not count from 1. */
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
    const stray = text.indexOf("`", past);
    if (stray >= 0 && stray < end) {
      return undefined;
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
  const scan: Scan = { at: 0, column: 0, content: 0 };
  const stack: Container[] = [];
  let paragraph = false;
  let fence: { character: number; width: number } | undefined;
  let indentedOpen = false;
  let start = 0;
  let last = 0;

  for (let at = 0; at < text.length; ) {
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
          blocks.push({ end: next, start });
          fence = undefined;
        }
        at = next;
        continue;
      }
      // The line left the containers the fence opened in, and no code block is carried on by a
      // line that leaves them: the block ends with the line before this one, which is then read
      // as any other.
      blocks.push({ end: at, start });
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
      blocks.push({ end: last, start });
      indentedOpen = false;
    }

    // The containers this line opens of its own, each inside the last: its content is what is
    // left after them.
    let depth = matched;
    let thematic = false;
    while (indent < end && reached - column <= 3) {
      if (text.charCodeAt(indent) === GREATER_THAN) {
        stack.length = depth;
        stack.push({ content: 0, quote: true });
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
      if (!listMarker(text, indent, end, reached, paragraph, scan)) {
        break;
      }
      const itemContent = scan.at;
      const itemColumn = scan.column;
      stack.length = depth;
      stack.push({ content: scan.content, quote: false });
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
    if (indent >= end || thematic) {
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
    blocks.push({ end: text.length, start });
  } else if (indentedOpen) {
    blocks.push({ end: last, start });
  }
  return { blocks, breaks };
}

/** Where the first run of exactly `width` backticks at or after `from` starts, or -1 when
 *  `[from, to)` holds none: a run of any other length closes no span of that width, so the
 *  search carries on past it. */
function nextBacktickRun(text: string, from: number, to: number, width: number): number {
  for (let at = from; at < to; ) {
    const found = text.indexOf("`", at);
    if (found < 0 || found >= to) {
      return -1;
    }
    at = runEnd(text, found, to, BACKTICK);
    if (at - found === width) {
      return found;
    }
  }
  return -1;
}

/** The code spans of `[from, to)`, a stretch of `text` no paragraph break cuts, appended to
 *  `found`, in order: the run of backticks at a span's start is closed by the next run of
 *  exactly that length, a longer run being no closer, and a run with no closer is literal text
 *  that the next character is read on from. A backslash takes the character after it, so an
 *  escaped backtick opens no span; a run that closes one is read as written, as CommonMark
 *  reads a closer. */
function appendParagraphSpans(text: string, from: number, to: number, found: CodeRange[]): void {
  for (let at = from; at < to; ) {
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
    const closer = nextBacktickRun(text, opener, to, width);
    if (closer < 0) {
      at = opener;
      continue;
    }
    found.push({ end: closer + width, start: at });
    at = closer + width;
  }
}

/** The index in `breaks` of the first paragraph break past `from`: a break on `from` itself cuts
 *  nothing, the paragraph starting there. */
function breakAfter(breaks: readonly number[], from: number): number {
  let low = 0;
  let high = breaks.length;
  while (low < high) {
    const middle = (low + high) >> 1;
    if (breaks[middle] <= from) {
      low = middle + 1;
    } else {
      high = middle;
    }
  }
  return low;
}

/** The code spans of `[from, to)` appended to `found`, in order, each read inside the paragraph
 *  that holds it: the search for a span's closing run ends where that paragraph ends, at the
 *  first of `breaks` past where the search began - a blank line, a line blank past its
 *  containers, a list item's marker, an ATX heading, a thematic break - so an unmatched backtick
 *  mutes nothing written past its own paragraph. */
function appendCodeSpans(
  text: string,
  from: number,
  to: number,
  breaks: readonly number[],
  found: CodeRange[]
): void {
  let start = from;
  for (let index = breakAfter(breaks, from); index < breaks.length && breaks[index] < to; index++) {
    appendParagraphSpans(text, start, breaks[index], found);
    start = breaks[index];
  }
  appendParagraphSpans(text, start, to, found);
}

/** The code spans of `text`, in order: the one reading of where a span begins on this side,
 *  which the snippet cutter reads too (`markdown-fragment.ts`). It reads the text's paragraph
 *  breaks for itself, so a span begins where `codeRanges` begins one. */
export function codeSpans(text: string): CodeRange[] {
  const spans: CodeRange[] = [];
  appendCodeSpans(text, 0, text.length, codeBlocks(text).breaks, spans);
  return spans;
}

/** Every code context of `text`, in order and never overlapping: the fenced and indented blocks,
 *  and the code spans of the text they leave. It is read once per reading of a text, by the one
 *  pass that reading already makes (`readTextReferences`), and every reference is marked from it
 *  with `isInCode`. */
export function codeRanges(text: string): CodeRange[] {
  const { blocks, breaks } = codeBlocks(text);
  const ranges: CodeRange[] = [];
  let at = 0;
  for (const block of blocks) {
    appendCodeSpans(text, at, block.start, breaks, ranges);
    ranges.push(block);
    at = block.end;
  }
  appendCodeSpans(text, at, text.length, breaks, ranges);
  return ranges;
}

/** Whether the character at `offset` sits in one of `ranges`, which `codeRanges` wrote in order:
 *  the reference whose first character it is was written in a code context. A binary search, so
 *  a body of many spans costs a reference no more than a body of few. */
export function isInCode(ranges: readonly CodeRange[], offset: number): boolean {
  let low = 0;
  let high = ranges.length - 1;
  while (low <= high) {
    const middle = (low + high) >> 1;
    const range = ranges[middle];
    if (range.end <= offset) {
      low = middle + 1;
    } else if (range.start > offset) {
      high = middle - 1;
    } else {
      return true;
    }
  }
  return false;
}
