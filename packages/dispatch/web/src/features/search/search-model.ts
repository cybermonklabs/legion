import { SEARCH_OPERATORS, SEARCH_RESULT_KINDS } from "@legion/contracts";
import type {
  DisplayTree,
  SearchOperator,
  SearchOwner,
  SearchResult,
  SearchResultKind,
} from "../../api/types";

/** `value` as a result kind, or `undefined` where this dashboard knows no such kind. Every
 *  surface that needs the kind itself - the palette, the related-items list - narrows here. */
export function searchResultKind(value: string): SearchResultKind | undefined {
  return SEARCH_RESULT_KINDS.find((known) => known === value);
}

/** The operator forms `read` names, as the forms this dashboard knows. An answer's reading of
 *  the query carries each form as the plain string the route table declares, and
 *  `SEARCH_OPERATORS` is the closed list that string is one of; a form this build has no word
 *  for is left out, as is every form when a Dispatch that predates the field answers none. The
 *  one reader asks only whether a form was used, so the answer keeps `SEARCH_OPERATORS`' own
 *  order rather than the reading's. */
export function searchOperators(read: readonly string[] | null | undefined): SearchOperator[] {
  return SEARCH_OPERATORS.filter((known) => read?.includes(known) === true);
}

/** How many hits of one kind the bar lists (design v3, state 1). */
export const HITS_PER_KIND = 6;

/** The heading of each kind's group of hits. */
const KIND_HEADINGS: Readonly<Record<SearchResultKind, string>> = {
  ask: "Asks",
  comment: "Comments",
  document: "Documents",
  issue: "Issues",
  message: "Messages",
};

/** The hits of one kind, under the heading the bar draws above them. */
export interface ResultGroup {
  /** The kind, as the route table names it. */
  readonly kind: string;
  /** "Issues", "Documents", and so on; a kind this build has no word for keeps its own. */
  readonly heading: string;
  readonly results: SearchResult[];
}

/** The hits grouped by their kind, each group in the rank order of its first hit, and at most
 *  `HITS_PER_KIND` hits in a group, in the server's order. */
export function groupResults(results: readonly SearchResult[]): ResultGroup[] {
  const groups = new Map<string, ResultGroup>();

  for (const result of results) {
    const group = groups.get(result.kind);
    if (group === undefined) {
      const known = searchResultKind(result.kind);
      groups.set(result.kind, {
        heading: known === undefined ? result.kind : KIND_HEADINGS[known],
        kind: result.kind,
        results: [result],
      });
    } else if (group.results.length < HITS_PER_KIND) {
      group.results.push(result);
    }
  }

  return [...groups.values()];
}

/** What a hit's owner is called in the bar: an issue by its key and title, a project document by
 *  its project and name. */
export function ownerWords(owner: SearchOwner): { readonly key: string; readonly name: string } {
  return owner.kind === "issue"
    ? { key: owner.key ?? "", name: owner.title ?? "" }
    : { key: owner.project ?? "", name: owner.name ?? "" };
}

/** Whether a hit is the issue itself, whose text is that issue's title: its row names the key and
 *  the title once. A document, a comment, an ask or a message of an issue names its issue. */
export function hitIsItsIssue(result: SearchResult): boolean {
  return result.kind === "issue" && result.owner.kind === "issue";
}

/** A run of a title a hit shows: its words, and whether the search matched them. */
export interface TitleRun {
  readonly text: string;
  readonly mark: boolean;
}

/**
 * The title a hit that is the issue itself shows in place of its snippet, or `undefined` where
 * the snippet is the whole title and shows instead, with the words that matched marked.
 * `segments` is the snippet as runs of text, each marked where the search matched it
 * (`snippetSegments`). The server's headline returns a short title whole, but it can cut it to the
 * window around the words that matched ("rate limit headers" of "API: rate limit headers"), and
 * the row then shows the whole title, with the words the window marked marked at their place in
 * it, as a title the headline returned whole shows them. A window that stands nowhere in the title
 * as written marks nothing.
 */
export function issueHitTitle(
  result: SearchResult,
  segments: readonly TitleRun[]
): readonly TitleRun[] | undefined {
  const title = result.owner.title ?? "";
  const windowText = segments.map((segment) => segment.text).join("");
  if (windowText.trim() === title.trim()) return undefined;
  // The window's words, the spaces at its two ends aside, which the headline may add or drop.
  const words = trimmedRuns(segments);
  const core = words.map((run) => run.text).join("");
  const at = core === "" ? -1 : title.indexOf(core);
  if (at < 0) return [{ mark: false, text: title }];
  const runs: TitleRun[] = [{ mark: false, text: title.slice(0, at) }, ...words];
  runs.push({ mark: false, text: title.slice(at + core.length) });
  // Neighbouring runs of one kind are one run, so the title is its fewest pieces.
  const joined: TitleRun[] = [];
  for (const run of runs) {
    const previous = joined.at(-1);
    if (run.text === "") continue;
    if (previous?.mark === run.mark) {
      joined[joined.length - 1] = { mark: run.mark, text: previous.text + run.text };
    } else {
      joined.push(run);
    }
  }
  return joined;
}

/** `runs` without the whitespace at the start of the first and the end of the last. */
function trimmedRuns(runs: readonly TitleRun[]): TitleRun[] {
  const trimmed = runs.map((run) => ({ ...run }));
  const first = trimmed[0];
  if (first !== undefined) trimmed[0] = { ...first, text: first.text.trimStart() };
  const lastAt = trimmed.length - 1;
  const last = trimmed[lastAt];
  if (last !== undefined) trimmed[lastAt] = { ...last, text: last.text.trimEnd() };
  return trimmed.filter((run) => run.text !== "");
}

/** One piece of a directive's syntax in a text: the characters from `start` to `end` come out,
 *  and `put` goes in their place. */
interface DirectiveCut {
  readonly start: number;
  readonly end: number;
  readonly put: string;
}

/** A directive's fence where a line of it began: three colons or more, and the directive's name
 *  where the fence opens one (`:::plan_task`), as Proof's tokenizer reads a name
 *  (`parseDisplayDirectiveOpen`, `pmdoc/display.go`). A soft break reads as a space in the tree, so
 *  a fence of a line after the first stands after whitespace. */
const DIRECTIVE_FENCE = /(?<=^|\s):{3,}(?:[A-Za-z][\w-]*)?/gu;

/** The cuts of a directive's attributes from `start` to `end` of `text`: the words of each quoted
 *  value stay, a space before each after the first, and the rest goes - `{`, `#t1`, `serves=`,
 *  the quotes, `}`. `inValue` says whether `start` is inside a value already, as it is where the
 *  window began partway through one. */
function attributeCuts(text: string, start: number, end: number, inValue: boolean): DirectiveCut[] {
  const cuts: DirectiveCut[] = [];
  let inside = inValue;
  // Whether a value's words were kept already, so the next value's are set apart by a space.
  let kept = inValue;
  let from = start;
  for (let at = start; at < end; at += 1) {
    if (text[at] !== '"') continue;
    if (inside) {
      from = at;
    } else {
      cuts.push({ end: at + 1, put: kept ? " " : "", start: from });
      kept = true;
    }
    inside = !inside;
  }
  if (!inside) cuts.push({ end, put: "", start: from });
  return cuts;
}

/** Where the window began inside a directive's attributes: the text up to their `}`, which
 *  stands before any `{` and right after a quote, then whitespace or the text's end
 *  (`serves="A page is"} The plan…`, or `page is"} The plan…` cut inside the value); -1 where the
 *  text starts any other way. */
function cutAttributesEnd(text: string): number {
  const close = text.indexOf("}");
  if (close < 1 || text[close - 1] !== '"' || text.lastIndexOf("{", close) !== -1) return -1;
  return close + 1 === text.length || /\s/u.test(text[close + 1] ?? "") ? close + 1 : -1;
}

/**
 * Where `text` holds a Proof directive's syntax and what each piece leaves: the fence and its
 * name come out; a label keeps its words and loses its brackets; the attributes after it, to
 * their `}` or to the text's end where the window cut them, keep the words of their quoted values
 * and lose the rest (`attributeCuts`); and so do the attributes a window that began inside them
 * starts with (`cutAttributesEnd`). Nothing else in the text is touched.
 */
function directiveCuts(text: string): DirectiveCut[] {
  const cuts: DirectiveCut[] = [];
  const leading = cutAttributesEnd(text);
  if (leading !== -1) {
    const quotes = text.slice(0, leading - 2).split('"').length - 1;
    cuts.push(...attributeCuts(text, 0, leading, quotes % 2 === 0));
  }
  DIRECTIVE_FENCE.lastIndex = Math.max(leading, 0);
  for (let fence = DIRECTIVE_FENCE.exec(text); fence !== null; fence = DIRECTIVE_FENCE.exec(text)) {
    let at = fence.index + fence[0].length;
    cuts.push({ end: at, put: "", start: fence.index });
    if (text[at] === "[") {
      const close = text.indexOf("]", at);
      cuts.push({ end: at + 1, put: "", start: at });
      if (close !== -1) {
        cuts.push({ end: close + 1, put: "", start: close });
        at = close + 1;
      } else {
        at = text.length;
      }
    }
    if (text[at] === "{") {
      const close = text.indexOf("}", at);
      const end = close === -1 ? text.length : close + 1;
      cuts.push(...attributeCuts(text, at, end, false));
      at = end;
    }
    DIRECTIVE_FENCE.lastIndex = Math.max(DIRECTIVE_FENCE.lastIndex, at);
  }
  return cuts.filter((cut) => cut.end > cut.start || cut.put !== "");
}

/** `text` with `cuts` made: each cut's characters out, its `put` in their place, at `offset` of
 *  the whole text the cuts were found in. */
function cutText(text: string, cuts: readonly DirectiveCut[], offset = 0): string {
  let out = "";
  for (let index = 0; index < text.length; index += 1) {
    const at = offset + index;
    let removed = false;
    for (const cut of cuts) {
      if (cut.start === at) out += cut.put;
      if (at >= cut.start && at < cut.end) removed = true;
    }
    if (!removed) out += text[index];
  }
  return out;
}

/** `text` with the syntax of a Proof directive taken out (`directiveCuts`), its spaces run
 *  together; `text` itself where it holds none. */
export function withoutDirectiveSyntax(text: string): string {
  const cuts = directiveCuts(text);
  return cuts.length === 0 ? text : cutText(text, cuts).replace(/\s+/gu, " ").trim();
}

/** A node of a display tree, as far as the cut below reads one. */
interface TreeNode {
  readonly type: string;
  readonly text?: string;
  readonly content?: readonly TreeNode[];
  readonly [key: string]: unknown;
}

/** `block`'s inline content with the directive syntax its text holds taken out, across its text
 *  nodes, whose marks stay: the text of the block is read whole, since a mark - a link, a search
 *  highlight - splits a fence or its attributes over several nodes. Any inline node that is not
 *  text (a picture, a reference) stands as one character no cut takes. A node the cut empties
 *  goes, as a tree holds no empty text, and the block's spaces are run together and trimmed at
 *  its ends. `block` itself where it holds no directive syntax. */
function cutBlock(block: TreeNode): TreeNode {
  const inline = block.content ?? [];
  const whole = inline
    .map((node) => (node.type === "text" ? (node.text ?? "") : "\uFFFC"))
    .join("");
  const cuts = directiveCuts(whole);
  if (cuts.length === 0) return block;
  const content: TreeNode[] = [];
  let offset = 0;
  for (const node of inline) {
    if (node.type !== "text") {
      content.push(node);
      offset += 1;
      continue;
    }
    const text = node.text ?? "";
    const cut = cutText(text, cuts, offset).replace(/\s{2,}/gu, " ");
    offset += text.length;
    if (cut !== "") content.push({ ...node, text: cut });
  }
  const first = content[0];
  if (first?.type === "text") content[0] = { ...first, text: (first.text ?? "").trimStart() };
  const last = content.at(-1);
  if (last?.type === "text")
    content[content.length - 1] = { ...last, text: (last.text ?? "").trimEnd() };
  return { ...block, content: content.filter((node) => node.type !== "text" || node.text !== "") };
}

/**
 * A hit's snippet tree with the syntax of a Proof directive taken out of its text (design v3,
 * rule 1: no marker text; review N2). The server reads a hit's headline for display
 * (`pmdoc.DisplaySnippetJSON`), but the headline is a window of the body, and a window that
 * starts or ends inside a directive leaves its fence or its attributes cut - an opening line
 * whose `{` never closes, a closing `:::` whose opener the window left out - which the reader
 * reads as no directive, so they reach the tree as paragraph text, a soft break between them a
 * space: `table. ::: :::plan_task{#t1 serves="A page is`. The words, their formatting and the
 * search's highlight stay. `tree` itself where no textblock of it holds directive syntax.
 */
export function snippetWithoutDirectives(tree: DisplayTree | undefined): DisplayTree | undefined {
  if (tree === undefined) return undefined;
  let changed = false;
  const walk = (node: TreeNode): TreeNode => {
    const content = node.content;
    if (content === undefined) return node;
    if (content.some((child) => child.type === "text")) {
      const cut = cutBlock(node);
      changed ||= cut !== node;
      return cut;
    }
    const walked = content.map(walk);
    return walked.every((child, index) => child === content[index])
      ? node
      : { ...node, content: walked };
  };
  const walked = walk(tree as TreeNode);
  return changed ? (walked as DisplayTree) : tree;
}

/** The tail of a sentence a window began on, and the space after it: words that start with a
 *  small letter, up to a full stop, a question mark or an exclamation mark and any closing quote
 *  or bracket after it, then whitespace. */
const SENTENCE_TAIL = /^\p{Ll}[^.!?]*[.!?]+["'’”)\]]*\s+/u;

/**
 * A cut snippet's tree started at the first word of a sentence (review A2): the server's headline
 * window can begin on the tail of the sentence before the match ("table. A page is read…"), which
 * reads as a fragment. Where the tree's first text starts with a small letter, carries no mark -
 * no matched word, no link, no formatting - and ends its sentence inside that text, the tail
 * comes out, and the line starts at the word after it. `tree` itself where it starts any other
 * way, and where the tail is all the text its block holds. The row asks this only of a snippet
 * whose start the server says its window cut (`snippetCut`): a whole text's first words are no
 * tail, whatever letter they start with.
 */
export function snippetFromSentence(tree: DisplayTree | undefined): DisplayTree | undefined {
  if (tree === undefined) return undefined;
  const cut = (node: TreeNode): TreeNode | undefined => {
    const [first, ...rest] = node.content ?? [];
    if (first === undefined) return undefined;
    if (first.type !== "text") {
      // A block that holds no text of its own starts where its first child does.
      const inner = node.content?.some((child) => child.type === "text") ? undefined : cut(first);
      return inner === undefined ? undefined : { ...node, content: [inner, ...rest] };
    }
    const text = first.text ?? "";
    const tail =
      Array.isArray(first.marks) && first.marks.length > 0 ? null : SENTENCE_TAIL.exec(text);
    if (tail === null) return undefined;
    const left = text.slice(tail[0].length);
    if (left === "" && rest.length === 0) return undefined;
    return { ...node, content: left === "" ? rest : [{ ...first, text: left }, ...rest] };
  };
  const started = cut(tree as TreeNode);
  return started === undefined ? tree : (started as DisplayTree);
}

/** Where the server's headline window cut the text a hit's snippet was read out of. */
export interface SnippetCut {
  /** The window begins after a word of that text. */
  readonly start: boolean;
  /** The window stops before a word of that text. */
  readonly end: boolean;
}

/**
 * Where the server's headline window cut the text a hit's snippet was read out of, as the server
 * measured it and wrote it on the snippet tree's doc node (`pmdoc.SnippetCutStartAttr`,
 * `pmdoc.SnippetCutEndAttr`, `packages/envoy/internal/dispatch/pmdoc/display_json.go`): the row
 * begins the line with "…" where the start was cut and ends it with one where the end was. Only
 * the server can say: ts_headline's window is counted in the text-search parser's tokens, and a
 * URL or a directive's attributes are many of them, so no count of the snippet's own words tells
 * a cut window from a whole text. A tree that says neither, and a hit that came with no tree, is
 * the whole text.
 */
export function snippetCut(tree: DisplayTree | undefined): SnippetCut {
  const attrs: Record<string, unknown> =
    typeof tree?.attrs === "object" && tree.attrs !== null
      ? (tree.attrs as Record<string, unknown>)
      : {};
  return { end: attrs.cutEnd === true, start: attrs.cutStart === true };
}

/** Mirrors the server's first positive websearch token for document deep links. */
export function firstHighlightTerm(query: string): string | undefined {
  for (const token of query.trim().split(/\s+/u)) {
    if (token === "" || token.startsWith("-") || token.toLowerCase() === "or") {
      continue;
    }
    const term = token.replace(/^"+|"+$/gu, "");
    if (term !== "") {
      return term;
    }
  }
  return undefined;
}

export function stepActive(index: number, delta: 1 | -1, count: number): number {
  return count === 0 ? 0 : (index + delta + count) % count;
}

export function kindLabel(kind: SearchResultKind): "issue" | "doc" | "comment" | "ask" | "message" {
  return kind === "document" ? "doc" : kind;
}

export function optionId(result: SearchResult): string {
  return `search-option-${result.kind}-${result.id}`;
}
