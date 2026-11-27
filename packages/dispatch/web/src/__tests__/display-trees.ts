/**
 * The readings the server would have sent, for a test that builds its own records.
 *
 * A view draws the tree Go read a stored text into, so a record a test invents needs one too.
 * Where the text is the test's own, `displayTree` reads it with the page's own engine: the two
 * readings are the same document, which is what the pmdoc differential holds, and the shape Go
 * writes is pinned by the fixture `pmdoc`'s `TestWebDisplayFixture` writes
 * (`features/refs/__fixtures__/display-trees.json`).
 */

import { snippetSegments } from "@legion/contracts/dispatch-snippet";

import type { DisplayTree } from "../api/types";
import { loadedEngine, type MarkdownEngine } from "../features/refs/markdown-engine";

/** The reading of `markdown` the server would send. `warmMarkdownRenderer` must have resolved
 *  before this is called; `undefined` for a text the engine refuses, as the server's own
 *  reading is never. */
export function displayTree(markdown: string): DisplayTree | undefined {
  return loadedEngine()?.parse(markdown)?.toJSON() as DisplayTree | undefined;
}

/**
 * The reading the server sends a statement whose words call the footnotes `labels`, each of
 * which its document defines outside it, as `api/coverage.go`'s `requirementBlocksIn` reads it:
 * the statement's markdown read alone reads such a call as text, so it is read with a stand-in
 * definition `[^<label>]: x` of each label after it, a blank line before each, and those
 * definitions, the last blocks of the reading, are taken out of its document. Each call is then
 * a `footnote_reference`, as the document and the editor hold it. `warmMarkdownRenderer` must
 * have resolved before this is called.
 */
export function displayTreeWithStandIns(markdown: string, labels: readonly string[]): DisplayTree {
  const tree = displayTree(`${markdown}${labels.map((label) => `\n\n[^${label}]: x`).join("")}`);
  if (tree === undefined || !Array.isArray(tree.content)) {
    throw new Error(`the engine reads no document from ${JSON.stringify(markdown)}`);
  }
  const kept = tree.content.length - labels.length;
  if (kept < 1 || tree.content.slice(kept).some((block) => block?.type !== "footnote_definition")) {
    throw new Error(`the reading of ${JSON.stringify(markdown)} ends in no stand-in definitions`);
  }
  return { ...tree, content: tree.content.slice(0, kept) };
}

/** A count of the markdown parses the page's shared engine makes, from the call that made it
 *  until `restore`, which gives the engine its own `parse` back. */
export interface EngineParses {
  readonly count: () => number;
  readonly restore: () => void;
}

/**
 * Counts every markdown parse the page's shared engine makes from now on: the engine is one
 * object the whole page holds, and its `parse` is the one parser every view of a stored text can
 * reach (`stored-text-drawing.test.tsx` says why). A tree `displayTree` builds is a parse too, so
 * a test builds its trees before it starts counting. `warmMarkdownRenderer` must have resolved.
 */
export function countEngineParses(): EngineParses {
  const engine = loadedEngine();
  if (engine === undefined) {
    throw new Error("the Markdown engine did not load");
  }
  const own = engine.parse;
  const counted: { parse: MarkdownEngine["parse"] } = engine;
  let parses = 0;
  counted.parse = (markdown, softBreaks) => {
    parses += 1;
    return own.call(engine, markdown, softBreaks);
  };
  return {
    count: () => parses,
    restore: () => {
      counted.parse = own;
    },
  };
}

/**
 * The literal text a view is handed beside a search hit's tree: the server's snippet with the
 * sentinels it marks the matched runs with taken out (`webSnippetStart`/`webSnippetStop` of
 * `pmdoc/web_fixtures_test.go`, which the display-tree fixture's markdown keeps). This is the
 * text the palette passes (`SearchPalette.tsx`, through `snippetSegments`), and it is what the
 * references a body cites are read from, so a reference standing between two sentinels is read
 * as the reference it is.
 */
export function snippetSource(markdown: string): string {
  return markdown.replaceAll("\uE000", "").replaceAll("\uE001", "");
}

/**
 * The tree of a search hit's snippet, with the words the server matched carrying the highlight
 * mark: the shape `pmdoc.DisplaySnippetJSON` writes for a snippet of plain words, built here
 * from the `<mark>` runs the test's own snippet names (`snippetSegments`, the encoding the
 * server's answer uses). A hit whose snippet holds formatting belongs in the Go fixture
 * instead, which reads it as the server does.
 */
export function snippetTree(snippet: string): DisplayTree {
  const content = snippetSegments(snippet)
    .filter((segment) => segment.text !== "")
    .map((segment) =>
      segment.mark
        ? { marks: [{ type: "searchHighlight" }], text: segment.text, type: "text" }
        : { text: segment.text, type: "text" }
    );
  return {
    content: [{ attrs: { blockId: "b-000001" }, content, type: "paragraph" }],
    type: "doc",
  };
}
