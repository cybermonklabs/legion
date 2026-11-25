import { boundBlockNesting } from "@legion/proof-editor/block-nesting";
import { fromMarkdown, type Options } from "mdast-util-from-markdown";
import { frontmatterFromMarkdown } from "mdast-util-frontmatter";
import { gfmFootnoteFromMarkdown } from "mdast-util-gfm-footnote";
import { gfmStrikethroughFromMarkdown } from "mdast-util-gfm-strikethrough";
import { gfmTableFromMarkdown } from "mdast-util-gfm-table";
import { gfmTaskListItemFromMarkdown } from "mdast-util-gfm-task-list-item";
import { frontmatter } from "micromark-extension-frontmatter";
import { gfmFootnote } from "micromark-extension-gfm-footnote";
import { gfmStrikethrough } from "micromark-extension-gfm-strikethrough";
import { gfmTable } from "micromark-extension-gfm-table";
import { gfmTaskListItem } from "micromark-extension-gfm-task-list-item";

import type { DisplayTree } from "../../api/types";

// A stored text is parsed here on the page's own thread, often before the lazy markdown engine
// has loaded, so the bound that engine holds micromark's lists and quotes to is put in place
// first, before any parse. Without it a deeply nested body costs seconds or overflows the stack
// (sjawhar/legion#2519); with it, a list or quote marker past the document service's bound reads
// as the text it is, as the server reads it. The call rewrites the two constructs once however
// often it is made, so the engine's own call later changes nothing.
boundBlockNesting();

/** What the parse answers for a whole text: its root, whose children are the text's blocks. It is
 *  named from the parser's own answer because the package that declares the syntax tree's types
 *  (`@types/mdast`) reaches this package only through the parser, which declares it. */
type ParsedRoot = ReturnType<typeof fromMarkdown>;

/** One node of the parse below its root, a block or an inline one, told apart by its `type`. */
type Parsed = ParsedRoot["children"][number];

/** One row of a parsed table, the header row or a body row. */
type ParsedRow = Extract<Parsed, { type: "tableRow" }>;

/** A link of the parse, inline, an autolink, or by reference. */
type ParsedLink = Extract<Parsed, { type: "link" | "linkReference" }>;

/**
 * The parse a stored text is read with: CommonMark with GFM's tables, strikethrough with one
 * tilde or two, task boxes and footnotes, and YAML front matter, a `---` fenced block a text opens
 * with, its lists and quotes nesting within the document service's bound (`boundBlockNesting`).
 * It is the base of the parse the page's markdown engine reads a body with, since remark-parse,
 * remark-gfm and remark-frontmatter are these extensions over `mdast-util-from-markdown`, less
 * GFM's autolink literals. GFM is built here from its constructs' own extensions, in the order
 * and with the options `micromark-extension-gfm`'s `gfm()` and `mdast-util-gfm`'s
 * `gfmFromMarkdown()` combine them in, with autolink literals' two halves left out of both lists.
 * Left out rather than disabled by name: micromark breaks its text at every character a construct
 * is listed for whose `previous` allows it (`micromark/lib/initialize/text.js`) and only then
 * refuses a disabled one (`micromark/lib/create-tokenizer.js`), and the email construct is listed
 * for every ASCII letter and digit and allowed after any character but those, `+`, `-`, `.`, `_`
 * and `/`, so a disabled extension still costs a turn at the start of nearly every word, while
 * one that is not in the list costs nothing.
 *
 * Autolink literals are left out because a bare URL, `www.` address or email reads as the same
 * words whether it is linked or not, while that extension was the parse's largest single cost as
 * measured. The words differ where a bare address holds markup, since the extension reads an
 * address as written before emphasis, code or links form, so every character inside it is the
 * address's own: here a `*`, `_` or `~` pair inside a bare URL reads as a mark rather than as the
 * URL's own characters, and so does a `_` pair in an email's name, and a code span's backticks, a
 * link's brackets and target, a backslash escape and a character reference inside a bare URL read
 * as that markup. The server's reading, which reads bare addresses as the engine does
 * (`pmdoc/linkify.go`), then keeps those characters as written.
 *
 * The engine's other extensions on top of that base are not applied here; among them, a container
 * directive (a typed block such as `:::ask`) reads as its literal fence lines and the content
 * between them, and a Proof mark span (`<span data-proof=…>`) reads as HTML around its text. The
 * extensions hold nothing of any one parse, so one set serves every text.
 *
 * Its last step finds the target of each link by reference, its definition's url
 * (`resolveReferences`), which the link's mark holds (`withLink`).
 */
const STORED_TEXT_PARSE: Options = {
  extensions: [
    gfmFootnote(),
    gfmStrikethrough(),
    gfmTable(),
    gfmTaskListItem(),
    frontmatter(["yaml"]),
  ],
  mdastExtensions: [
    gfmFootnoteFromMarkdown(),
    gfmStrikethroughFromMarkdown(),
    gfmTableFromMarkdown(),
    gfmTaskListItemFromMarkdown(),
    frontmatterFromMarkdown(["yaml"]),
    { transforms: [resolveReferences] },
  ],
};

/** One remembered reading (`readings`): the reading, and how many registrations (`releases`) of
 *  the holders that asked for its text still hold it. */
interface HeldReading {
  readonly reading: DisplayTree;
  holders: number;
}

/** The readings some living holder asked for (`storedReading`), by the text each was read from.
 *  An entry goes when the last registration that holds it is released (`releases`). */
const readings = new Map<string, HeldReading>();

/** The text each holder last asked for and that text's reading, so a holder asking again for the
 *  same text finds its own answer and is registered no second time (`storedReading`). It holds
 *  no holder alive. */
const held = new WeakMap<object, { readonly markdown: string; readonly reading: DisplayTree }>();

/** Releases one holder's hold on a text once that holder is collected: the held value is the
 *  text, whose count of holders goes down by one, and its reading goes with the last of them. */
const releases = new FinalizationRegistry<string>((markdown) => {
  const entry = readings.get(markdown);
  if (entry === undefined) return;
  entry.holders -= 1;
  if (entry.holders === 0) readings.delete(markdown);
});

/** The marks of a run that stands in no mark. */
const NO_MARKS: readonly DisplayTree[] = [];

/** A line ending, in any of the three forms markdown reads as one: inside a text run, where the
 *  parse leaves it, and between the lines of a plain text (`plainReading`). */
const LINE_FEED = /\r\n?|\n/gu;

/**
 * Whatever in a stored text the parse could read as markup (`isPlainText`). Each alternative errs
 * toward the parse: a text it flags is parsed, so it may flag a text that reads as plain, and it
 * misses no markup.
 * - Anywhere: `\` an escape or a hard break; `` ` `` code; `*` and `_` emphasis and strong, and a
 *   list or a thematic break made of them; `[` and `]` a link, an image, a footnote, a task box
 *   or a link reference definition; `<` and `>` HTML, an autolink and a quote; `&` an entity or
 *   character reference; `|` a table; `~` strikethrough and a tilde fence; NUL, which the parse
 *   replaces with U+FFFD; a byte order mark, which the parse drops where a text opens with one,
 *   so the line after it can open a block.
 * - A line opening, past spaces and tabs, with `#`, `+`, `:`, `=` or `-`: a heading, a list, a
 *   table's delimiter row, a setext underline, a thematic break or front matter; or with up to
 *   nine digits and `.` or `)`: an ordered list. A delimiter row may open with the colon that
 *   aligns its column, and needs no pipe, so `a` over `:--` is a table of one column.
 * - A line indented into code: a tab after at most three spaces, which reaches the fourth column,
 *   or four spaces.
 */
const MARKUP = /[\\`*_[\]<>&|~\0\uFEFF]|^[ \t]*(?:[#+:=-]|\d{1,9}[.)])|^ {0,3}\t|^ {4}/mu;

/** The spaces and tabs a line opens and ends with, which the parse leaves out of a paragraph's
 *  words; no other white space, which it keeps. */
const EDGE_SPACE = /^[ \t]+|[ \t]+$/gu;

/**
 * The reading of a stored markdown text: the text parsed by the base of the page's markdown
 * engine's parse less GFM's autolink literals (`STORED_TEXT_PARSE`) and held as a `DisplayTree`
 * of the shape the server's reading of a text has, or, for a text that holds no markup
 * (`isPlainText`), built as that parse would read it (`plainReading`). The walks that read the
 * server's reading - its words (`wordsOf`), a node's first block (`lineWords`), its addresses
 * written short (`withShortAddresses`) - so read a stored text the same way. Its words are the
 * words of that parse, which are what the server's reading holds for every text that holds none
 * of three things: a container directive, a Proof mark span and a bare address holding markup
 * (`STORED_TEXT_PARSE`), so a place that has only the text reads what the server's reading of it
 * would hold (R21).
 *
 * The reading holds what decides the words and where they stand apart: the blocks, the text runs
 * and the marks on them, by the server's names, a link's mark with the link's target (`href`)
 * as the server's reading holds it, which `lenses/store-words.ts` reads to name the node a link
 * to a store address names. It holds no other attribute, since no word lies in one,
 * but the number a numbered list starts at where that is not 1 (`order`), which it holds as the
 * server's reading does so that the two readings have one shape; nothing on the page reads it. A
 * block opens a space before its words and after them. A mark sits on the runs it covers, so a
 * marked run and the run beside it stay consecutive children of one block and read adjacent
 * (`a **b**c` reads `a bc`). A leaf that holds no text - a hard break, an image, a footnote
 * reference, a piece of HTML - opens a space as a block does. Markup is no word: a list's and a
 * quote's markers, a heading's `#`, a task box, a table's pipes and rule, a link's target, an
 * image's address and alt text, an HTML comment or tag, and a link reference definition. A text
 * run holds each line feed as a space, as the server writes a soft line break (`parseTextValue`,
 * `pmdoc/parse.go`); code - a code span and a code block - holds its text as the parse gives it,
 * and so does front matter, which reads between its two fences as the server writes it.
 *
 * The page reads a node's line on every draw. A plain text's reading costs a split, so it is
 * built each time and remembered nowhere. Any other text's reading is remembered while any
 * `holder` that asked for it lives - a tree node, a read's answer, another caller's object - and
 * goes once the last of them is collected. The memo has no bound on how many readings it holds:
 * a memo bounded by count drops every reading on a redraw once an issue holds more texts than the
 * bound, while this one holds each reading exactly as long as a tree node, a read's answer or
 * another caller that asked for it still lives, and a FinalizationRegistry's cleanup never runs
 * inside the synchronous code that drew, so one draw's readings are all there for the next. A
 * caller that names no holder is answered the reading a holder holds where there is one, and
 * otherwise a reading parsed for it alone and remembered nowhere, since nothing would ever
 * release it. A reading is shared by every caller that reads its text, so nothing may change
 * one: `withShortAddresses` writes an address into a copy of the nodes it changes.
 */
export function storedReading(markdown: string, holder?: object): DisplayTree {
  // A holder asking again for the text it last asked for is answered first, before even the
  // plain path's test: that is every redraw of a node whose text holds markup. A plain text is
  // never held, so it passes this and takes the plain path.
  const own = holder === undefined ? undefined : held.get(holder);
  if (own?.markdown === markdown) return own.reading;
  if (isPlainText(markdown)) return plainReading(markdown);
  if (holder === undefined) return readings.get(markdown)?.reading ?? parsedReading(markdown);
  let entry = readings.get(markdown);
  if (entry === undefined) {
    entry = { holders: 0, reading: parsedReading(markdown) };
    readings.set(markdown, entry);
  }
  // A holder new to this text holds it until the holder is collected, even where it asks for
  // another text after: this registration releases this text then, and the other's its own.
  entry.holders += 1;
  releases.register(holder, markdown);
  held.set(holder, { markdown, reading: entry.reading });
  return entry.reading;
}

/** Whether `markdown` holds nothing the parse could read as markup (`MARKUP`), so that its
 *  reading is its paragraphs' words alone (`plainReading`). */
export function isPlainText(markdown: string): boolean {
  return !MARKUP.test(markdown);
}

/**
 * The reading the parse gives a plain text (`isPlainText`), built without the parse. Such a text
 * is paragraphs and nothing else, each a run of lines between lines that hold only spaces and
 * tabs, which are blank. The parse leaves out of a paragraph's words the spaces and tabs each of
 * its lines opens and ends with, and reads a soft line break and a hard one - two spaces or more
 * before the line ending - each as one space between words, so a paragraph's words are its lines
 * trimmed of spaces and tabs, joined by one space. A text with no paragraph is an empty document.
 */
export function plainReading(markdown: string): DisplayTree {
  const paragraphs: DisplayTree[] = [];
  let lines: string[] = [];
  for (const line of markdown.split(LINE_FEED)) {
    const words = line.replace(EDGE_SPACE, "");
    if (words !== "") {
      lines.push(words);
    } else if (lines.length > 0) {
      paragraphs.push(paragraphOf(lines));
      lines = [];
    }
  }
  if (lines.length > 0) paragraphs.push(paragraphOf(lines));
  return withContent("doc", paragraphs);
}

/** A paragraph of a plain text (`plainReading`): its trimmed lines as one text run, one space
 *  between each two. */
function paragraphOf(lines: readonly string[]): DisplayTree {
  return { content: [{ text: lines.join(" "), type: "text" }], type: "paragraph" };
}

/** The reading of a stored text by the parse alone (`STORED_TEXT_PARSE`), with no plain path and
 *  nothing remembered: what `storedReading` reads a text by where it holds no reading of it, and
 *  what a test holds the plain path (`plainReading`) equal to. */
export function parsedReading(markdown: string): DisplayTree {
  return withContent("doc", blocksOf(fromMarkdown(markdown, STORED_TEXT_PARSE).children));
}

/** The readings of `nodes`, the blocks of one container, in order, less each that draws nothing
 *  (`blockOf`). */
function blocksOf(nodes: readonly Parsed[]): DisplayTree[] {
  const blocks: DisplayTree[] = [];
  for (const node of nodes) {
    const block = blockOf(node);
    if (block !== undefined) blocks.push(block);
  }
  return blocks;
}

/**
 * The reading of one block of the parse and what it holds, by the server's name for it. A block
 * of HTML is a paragraph holding it as one `html` leaf, as the server shows one, since the
 * server's schema carries HTML inline only. Front matter is one text run of its lines between
 * two plain `---` fences, and of the two fences alone where it holds nothing, as the server
 * writes it (`parseFrontmatterBlock`, `pmdoc/lines.go`). A link reference definition, which only
 * names a link's target, draws nothing and is `undefined`.
 */
function blockOf(node: Parsed): DisplayTree | undefined {
  switch (node.type) {
    case "paragraph":
    case "heading":
      return withContent(node.type, runsOf(node.children));
    case "blockquote":
      return withContent("blockquote", blocksOf(node.children));
    case "list": {
      if (node.ordered !== true) return withContent("bullet_list", blocksOf(node.children));
      const list = withContent("ordered_list", blocksOf(node.children));
      // The server's reading writes the number an ordered list starts at as the list's `order`,
      // and leaves it out where that is 1, its schema's default, so a list from 3 reads apart
      // from a list from 1 here as it does there.
      if (node.start != null && node.start !== 1) return { attrs: { order: node.start }, ...list };
      return list;
    }
    case "listItem":
      return withContent("list_item", blocksOf(node.children));
    case "code": {
      const runs: DisplayTree[] = [];
      addText(runs, node.value, NO_MARKS);
      return withContent("code_block", runs);
    }
    case "thematicBreak":
      return { type: "hr" };
    case "html":
      return { content: [{ type: "html" }], type: "paragraph" };
    case "table":
      return withContent(
        "table",
        node.children.map((row, at) => rowOf(row, at === 0))
      );
    case "footnoteDefinition":
      return withContent("footnote_definition", blocksOf(node.children));
    case "yaml": {
      const text = node.value === "" ? "---\n---" : `---\n${node.value}\n---`;
      return { content: [{ text, type: "text" }], type: "frontmatter" };
    }
    default:
      return undefined;
  }
}

/** The reading of one table row: the header row as `table_header_row` of `table_header` cells
 *  and a body row as `table_row` of `table_cell` cells, as the server names them, each cell's
 *  words in a paragraph of its own, so two cells' words stand apart as two blocks' do. */
function rowOf(row: ParsedRow, header: boolean): DisplayTree {
  const cell = header ? "table_header" : "table_cell";
  return withContent(
    header ? "table_header_row" : "table_row",
    row.children.map((each) => ({
      content: [withContent("paragraph", runsOf(each.children))],
      type: cell,
    }))
  );
}

/** The text runs and the leaves of one block's inline content (`addRuns`). */
function runsOf(nodes: readonly Parsed[]): DisplayTree[] {
  const runs: DisplayTree[] = [];
  addRuns(runs, nodes, NO_MARKS);
  return runs;
}

/**
 * Adds to `runs` the runs and the leaves of `nodes`, inline content standing in `marks`. A mark's
 * content is flattened into runs carrying that mark as well, as the server holds a mark on the
 * text it covers rather than as a node around it, so nothing comes between a marked run and its
 * neighbours. A link by reference is a link, as an inline one is; an image by reference is an
 * image.
 */
function addRuns(
  runs: DisplayTree[],
  nodes: readonly Parsed[],
  marks: readonly DisplayTree[]
): void {
  for (const node of nodes) {
    switch (node.type) {
      case "text":
        addText(runs, node.value.replace(LINE_FEED, " "), marks);
        break;
      case "inlineCode":
        addText(runs, node.value, withMark(marks, "inlineCode"));
        break;
      case "strong":
        addRuns(runs, node.children, withMark(marks, "strong"));
        break;
      case "emphasis":
        addRuns(runs, node.children, withMark(marks, "emphasis"));
        break;
      case "delete":
        addRuns(runs, node.children, withMark(marks, "strike_through"));
        break;
      case "link":
      case "linkReference":
        addRuns(runs, node.children, withLink(marks, node));
        break;
      case "break":
        runs.push({ type: "hardbreak" });
        break;
      case "image":
      case "imageReference":
        runs.push({ type: "image" });
        break;
      case "footnoteReference":
        runs.push({ type: "footnote_reference" });
        break;
      case "html":
        runs.push({ type: "html" });
        break;
    }
  }
}

/** `marks` with a mark of `type` added, unless one is there already: a run carries each mark
 *  once, as the server's reading holds a run's marks as a set. */
function withMark(marks: readonly DisplayTree[], type: string): readonly DisplayTree[] {
  return marks.some((mark) => mark.type === type) ? marks : [...marks, { type }];
}

/** `marks` with a link mark added, unless one is there already (`withMark`): the mark holds the
 *  target of `link`, its url or, for a link by reference, its definition's (`referenceTargets`),
 *  as the server's reading holds it, and nothing where no definition names it. */
function withLink(marks: readonly DisplayTree[], link: ParsedLink): readonly DisplayTree[] {
  const href = link.type === "link" ? link.url : referenceTargets.get(link);
  return href === undefined || marks.some((mark) => mark.type === "link")
    ? withMark(marks, "link")
    : [...marks, { attrs: { href }, type: "link" }];
}

/** The url each link by reference of a parse links to, by the reference's own node
 *  (`resolveReferences`). A parse's nodes are its own, so each entry goes with its parse. */
const referenceTargets = new WeakMap<ParsedLink, string>();

/** The parse's last step (`STORED_TEXT_PARSE`): puts in `referenceTargets` the url each link by
 *  reference in `root` links to, the first definition of its label, as CommonMark reads it.
 *  Definitions stand anywhere in the text, so every one is found before any reference reads. */
function resolveReferences(root: ParsedRoot): void {
  const urls = new Map<string, string>();
  const references: Extract<ParsedLink, { type: "linkReference" }>[] = [];
  const walk = (nodes: readonly Parsed[]): void => {
    for (const node of nodes) {
      if (node.type === "definition" && !urls.has(node.identifier)) {
        urls.set(node.identifier, node.url);
      }
      if (node.type === "linkReference") references.push(node);
      if ("children" in node) walk(node.children);
    }
  };
  walk(root.children);
  for (const reference of references) {
    const url = urls.get(reference.identifier);
    if (url !== undefined) referenceTargets.set(reference, url);
  }
}

/** Adds a run of `text` carrying `marks` to `runs`, unless the text is empty: an empty text node
 *  is no node a ProseMirror document holds. */
function addText(runs: DisplayTree[], text: string, marks: readonly DisplayTree[]): void {
  if (text === "") return;
  runs.push(marks.length === 0 ? { text, type: "text" } : { marks, text, type: "text" });
}

/** A node of `type` holding `content`, with no `content` at all where it holds nothing, as a
 *  ProseMirror document writes an empty node. */
function withContent(type: string, content: readonly DisplayTree[]): DisplayTree {
  return content.length === 0 ? { type } : { content, type };
}
