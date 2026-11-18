import { fromMarkdown, type Options } from "mdast-util-from-markdown";
import { gfmFromMarkdown } from "mdast-util-gfm";
import { gfm } from "micromark-extension-gfm";

import type { DisplayTree } from "../../api/types";
import { rememberUpTo } from "../refs/worker-queue";

/** What the parse answers for a whole text: its root, whose children are the text's blocks. It is
 *  named from the parser's own answer because the package that declares the syntax tree's types
 *  (`@types/mdast`) reaches this package only through the parser, which declares it. */
type ParsedRoot = ReturnType<typeof fromMarkdown>;

/** One node of the parse below its root, a block or an inline one, told apart by its `type`. */
type Parsed = ParsedRoot["children"][number];

/** One row of a parsed table, the header row or a body row. */
type ParsedRow = Extract<Parsed, { type: "tableRow" }>;

/**
 * The parse a stored text is read with: CommonMark with GFM's syntax - tables, strikethrough
 * with one tilde or two, task boxes, autolink literals and footnotes. It is the parse the page's
 * markdown engine reads a body with, since remark-parse and remark-gfm are these two extensions
 * over `mdast-util-from-markdown`, and the server's reading of a text is written to agree with
 * that engine. The extensions hold nothing of any one parse, so one set serves every text.
 */
const GFM_PARSE: Options = { extensions: [gfm()], mdastExtensions: [gfmFromMarkdown()] };

/** How many readings are remembered (`storedReading`): enough that a page drawn again finds the
 *  readings its last draw made rather than parsing each text anew, and few enough that what is
 *  remembered stays small however many texts a session reads. */
const READING_LIMIT = 500;

/** The readings made so far, by the text each was read from. */
const readings = new Map<string, DisplayTree>();

/** The marks of a run that stands in no mark. */
const NO_MARKS: readonly DisplayTree[] = [];

/**
 * The reading of a stored markdown text: the text parsed as the page's markdown engine parses it
 * (`GFM_PARSE`) and held as a `DisplayTree` of the shape the server's reading of a text has. The
 * walks that read the server's reading - its words (`wordsOf`), a node's first block
 * (`lineWords`), its addresses written short (`withShortAddresses`) - so read a stored text the
 * same way, and a place that has only the text reads the words the server's reading of it holds
 * (R21).
 *
 * The reading holds what decides the words and where they stand apart: the blocks, the text runs
 * and the marks on them, by the server's names, and no attribute, since no word lies in one. A
 * block opens a space before its words and after them. A mark sits on the runs it covers, so a
 * marked run and the run beside it stay consecutive children of one block and read adjacent
 * (`a **b**c` reads `a bc`). A leaf that holds no text - a hard break, an image, a footnote
 * reference, a piece of HTML - opens a space as a block does. Markup is no word: a list's and a
 * quote's markers, a heading's `#`, a task box, a table's pipes and rule, a link's target, an
 * image's address and alt text, an HTML comment or tag, and a link reference definition. A run's
 * text is as the parse gives it, a soft line break inside it included; a caller collapses the
 * white space.
 *
 * The page reads a node's line on every draw, so the reading of each text is remembered, up to
 * `READING_LIMIT` texts. A reading is shared by every caller that reads its text, so nothing may
 * change one: `withShortAddresses` writes its addresses into a copy.
 */
export function storedReading(markdown: string): DisplayTree {
  const remembered = readings.get(markdown);
  if (remembered !== undefined) return remembered;
  const reading = withContent("doc", blocksOf(fromMarkdown(markdown, GFM_PARSE).children));
  rememberUpTo(readings, READING_LIMIT, markdown, reading);
  return reading;
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
 * server's schema carries HTML inline only. A link reference definition, which only names a
 * link's target, draws nothing and is `undefined`.
 */
function blockOf(node: Parsed): DisplayTree | undefined {
  switch (node.type) {
    case "paragraph":
    case "heading":
      return withContent(node.type, runsOf(node.children));
    case "blockquote":
      return withContent("blockquote", blocksOf(node.children));
    case "list":
      return withContent(
        node.ordered === true ? "ordered_list" : "bullet_list",
        blocksOf(node.children)
      );
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
        addText(runs, node.value, marks);
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
        addRuns(runs, node.children, withMark(marks, "link"));
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
