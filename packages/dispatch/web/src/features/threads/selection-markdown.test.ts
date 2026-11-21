import { afterEach, expect, test } from "bun:test";

import { escapeLineStart, escapeMarkdownText, selectionMarkdown } from "./selection-markdown";

afterEach(() => {
  document.body.replaceChildren();
});

/** The Markdown blocks of `html` drawn on the page as a text drawn from Markdown is (`drawn`), or
 *  as the page's own elements around one: of all of it, or of the range `select` makes inside it. */
function markdownOf(html: string, select?: (root: HTMLElement) => Range, drawn = true): string[] {
  const root = document.createElement("div");
  if (drawn) root.className = "dispatch-markdown";
  root.innerHTML = html;
  document.body.append(root);
  let range = select?.(root);
  if (range === undefined) {
    range = document.createRange();
    range.selectNodeContents(root);
  }
  return selectionMarkdown(range, root);
}

/** A range from `from` in the first text holding it to the end of `to` in the first holding it. */
function between(root: HTMLElement, from: string, to: string): Range {
  const range = document.createRange();
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  let started = false;
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    const text = node.textContent ?? "";
    if (!started && text.includes(from)) {
      range.setStart(node, text.indexOf(from));
      started = true;
    }
    if (started && text.includes(to)) {
      range.setEnd(node, text.indexOf(to) + to.length);
      return range;
    }
  }
  throw new Error(`no "${from}" … "${to}" in the text`);
}

test("each paragraph is a block of its own, its line breaks and runs of space one space", () => {
  expect(markdownOf("<p>The audit\n   is done.</p>\n<p>Flip.</p>")).toEqual([
    "The audit is done.",
    "Flip.",
  ]);
});

test("bold, italics and strikethrough keep their marks, outside the spaces at their edges", () => {
  expect(
    markdownOf(
      "<p><strong>Recommended: Flip.</strong> It is <em>much </em>cheaper, <del>not</del> so.</p>"
    )
  ).toEqual(["**Recommended: Flip.** It is *much* cheaper, ~~not~~ so."]);
  expect(markdownOf("<p><b>bold</b> and <i>slanted</i> and <s>gone</s></p>")).toEqual([
    "**bold** and *slanted* and ~~gone~~",
  ]);
  // Bold inside italics keeps both.
  expect(markdownOf("<p><em>very <strong>much</strong> so</em></p>")).toEqual([
    "*very **much** so*",
  ]);
});

test("a part of a formatted run that is selected keeps the run's mark", () => {
  expect(
    markdownOf("<p>Before <strong>Recommended: ship it as is.</strong> After</p>", (root) =>
      between(root, "ship it", "as is")
    )
  ).toEqual(["**ship it as is**"]);
  expect(
    markdownOf("<p>The <code>--dry-run</code> flag</p>", (root) => between(root, "dry", "run"))
  ).toEqual(["`dry-run`"]);
});

test("a code span holds its text as it reads, with no escape, whatever backticks it holds", () => {
  expect(markdownOf("<p>Run <code>__tests__/*.ts</code> first</p>")).toEqual([
    "Run `__tests__/*.ts` first",
  ]);
  expect(markdownOf("<p><code>a `b` c</code></p>")).toEqual(["``a `b` c``"]);
  expect(markdownOf("<p><code>`tick`</code></p>")).toEqual(["`` `tick` ``"]);
  expect(markdownOf("<p>press <kbd>q</kbd></p>")).toEqual(["press `q`"]);
});

test("a code block is one block: its fence, its language, and its lines as they are", () => {
  expect(
    markdownOf('<pre data-language="ts"><code>const a = "*";\n\n  return __a__;\n</code></pre>')
  ).toEqual(['```ts\nconst a = "*";\n\n  return __a__;\n```']);
  expect(markdownOf('<pre><code class="language-sh">ls -la</code></pre>')).toEqual([
    "```sh\nls -la\n```",
  ]);
  // A fence the code itself holds takes a longer one around it.
  expect(markdownOf("<pre><code>```\ninner\n```</code></pre>")).toEqual([
    "````\n```\ninner\n```\n````",
  ]);
  // The selected lines alone.
  expect(
    markdownOf("<pre><code>one\ntwo\nthree</code></pre>", (root) => between(root, "two", "three"))
  ).toEqual(["```\ntwo\nthree\n```"]);
});

test("a link keeps its text and where it goes; a bare address stays bare", () => {
  expect(markdownOf('<p>See <a href="https://example.com/a_b">the <em>plan</em></a>.</p>')).toEqual(
    ["See [the *plan*](https://example.com/a_b)."]
  );
  expect(
    markdownOf('<p><a href="https://example.com/a_b">https://example.com/a_b</a></p>')
  ).toEqual(["https://example.com/a_b"]);
  expect(markdownOf('<p><a href="/files/my plan (v2).md">plan</a></p>')).toEqual([
    "[plan](</files/my plan (v2).md>)",
  ]);
  // A link with nowhere to go is its text.
  expect(markdownOf("<p><a>plain</a></p>")).toEqual(["plain"]);
});

test("a Dispatch reference drawn as a chip posts as the reference", () => {
  expect(
    markdownOf(
      '<p>As <a href="/issues/PANE-5/spec" data-dispatch-ref="dispatch://PANE-5/spec"><span>PANE-5 spec</span></a> says</p>'
    )
  ).toEqual(["As dispatch://PANE-5/spec says"]);
});

test("a picture keeps its description and its address", () => {
  expect(
    markdownOf('<p><img alt="The [first] chart" src="https://example.com/c.png"></p>')
  ).toEqual(["![The \\[first\\] chart](https://example.com/c.png)"]);
  expect(
    markdownOf(
      '<p><a data-dispatch-href="dispatch://PANE/artifact/chart-png" href="/x"><img alt="chart" src="blob:1"></a></p>'
    )
  ).toEqual(["![chart](dispatch://PANE/artifact/chart-png)"]);
});

test("each list item is a block with its own mark, numbered by its place in the list", () => {
  expect(markdownOf("<ul><li><p>one</p></li><li><p>two <code>2</code></p></li></ul>")).toEqual([
    "- one",
    "- two `2`",
  ]);
  expect(markdownOf("<ol><li>first</li><li>second</li><li>third</li></ol>")).toEqual([
    "1. first",
    "2. second",
    "3. third",
  ]);
  // A selection that starts at the second item keeps the number the page shows.
  expect(
    markdownOf("<ol><li>first</li><li>second</li><li>third</li></ol>", (root) =>
      between(root, "second", "third")
    )
  ).toEqual(["2. second", "3. third"]);
  expect(markdownOf('<ol start="4"><li>fourth</li><li>fifth</li></ol>')).toEqual([
    "4. fourth",
    "5. fifth",
  ]);
});

test("a nested list and an item's later paragraph stay in their item", () => {
  expect(
    markdownOf(
      "<ul><li><p>outer</p><p>more</p><ul><li><p>inner</p></li></ul></li><li><p>next</p></li></ul>"
    )
  ).toEqual(["- outer", "  more", "  - inner", "- next"]);
});

test("a task list item keeps its box", () => {
  expect(
    markdownOf(
      '<ul><li><input type="checkbox" checked> done</li><li><input type="checkbox"> open</li></ul>'
    )
  ).toEqual(["- [x] done", "- [ ] open"]);
  expect(markdownOf('<ul><li data-checked="true"><p>done</p></li></ul>')).toEqual(["- [x] done"]);
});

test("a quote, a heading and a rule keep their marks", () => {
  expect(
    markdownOf("<blockquote><p>said <em>before</em></p><p>and after</p></blockquote>")
  ).toEqual(["> said *before*", "> and after"]);
  expect(markdownOf("<h2>The plan</h2><p>text</p><hr><h4>Detail</h4>")).toEqual([
    "## The plan",
    "text",
    "---",
    "#### Detail",
  ]);
  expect(markdownOf("<blockquote><ul><li>quoted item</li></ul></blockquote>")).toEqual([
    "> - quoted item",
  ]);
});

test("a table row is a block, its cells apart", () => {
  expect(
    markdownOf(
      "<table><thead><tr><th>Lease</th><th>Shard</th></tr></thead><tbody><tr><td>4411</td><td><code>3</code></td></tr></tbody></table>"
    )
  ).toEqual(["Lease | Shard", "4411 | `3`"]);
});

test("a line break inside a paragraph is a hard break; one that ends it is dropped", () => {
  expect(markdownOf("<p>first<br>second<br></p>")).toEqual(["first\\\nsecond"]);
  expect(markdownOf("<p><strong>first<br></strong>second</p>")).toEqual(["**first**\\\nsecond"]);
});

test("text that is not formatting is escaped, so it posts as it reads", () => {
  expect(markdownOf("<p>Run __tests__ over *.ts and snake_case_name</p>")).toEqual([
    "Run \\_\\_tests\\_\\_ over \\*.ts and snake_case_name",
  ]);
  expect(markdownOf("<p>a [link](x), a `tick`, a ~strike~ and a back\\slash</p>")).toEqual([
    "a \\[link\\](x), a \\`tick\\`, a \\~strike\\~ and a back\\\\slash",
  ]);
  expect(markdownOf("<p>use &lt;div&gt; and &amp;amp; but 3 &lt; 4 &amp; 5</p>")).toEqual([
    "use \\<div> and \\&amp; but 3 < 4 & 5",
  ]);
  expect(escapeMarkdownText("_lead and trail_")).toBe("\\_lead and trail\\_");
});

test("what Markdown reads as a block's mark at the start of a line is escaped there alone", () => {
  expect(markdownOf("<p># not a heading</p><p>- not an item</p><p>2. not second</p>")).toEqual([
    "\\# not a heading",
    "\\- not an item",
    "2\\. not second",
  ]);
  expect(markdownOf("<p>&gt; not a quote</p><p>+ plus</p><p>---</p><p>:::ask</p>")).toEqual([
    "\\> not a quote",
    "\\+ plus",
    "\\---",
    "\\:::ask",
  ]);
  // The same characters inside a line are the characters they are.
  expect(markdownOf("<p>issue #12 - done, 2. later > now</p>")).toEqual([
    "issue #12 - done, 2. later > now",
  ]);
  expect(markdownOf("<p>one<br># two</p>")).toEqual(["one\\\n\\# two"]);
  expect(escapeLineStart("#hashtag")).toBe("#hashtag");
  expect(escapeLineStart("-5 degrees")).toBe("-5 degrees");
  expect(escapeLineStart("2024 was")).toBe("2024 was");
});

test("a selection across blocks takes the selected part of each", () => {
  expect(
    markdownOf(
      "<p>The check asks for it. <strong>Recommended: Flip.</strong></p><ul><li>Keep it</li><li>Flip it now</li></ul>",
      (root) => between(root, "Recommended", "Flip it")
    )
  ).toEqual(["**Recommended: Flip.**", "- Keep it", "- Flip it"]);
});

test("outside a text drawn from Markdown, the page's own elements post as their words", () => {
  // A thread the page lays out as a list: its reply is no list item, its author's link no link.
  expect(
    markdownOf(
      '<ul><li data-reply=""><div class="dispatch-markdown"><p>Done in <strong>one</strong> build.</p><ul><li><p>first</p></li></ul></div><p><a href="/agents/x">lease-tool implementer</a> · <strong>2 hours ago</strong></p></li><li data-reply=""><h3>Reply</h3><code>id</code></li></ul>',
      undefined,
      false
    )
  ).toEqual([
    "Done in **one** build.",
    "- first",
    "lease-tool implementer · 2 hours ago",
    "Reply",
    "id",
  ]);
  // Their text is escaped all the same.
  expect(markdownOf("<p>*.ts by __init__</p>", undefined, false)).toEqual([
    "\\*.ts by \\_\\_init\\_\\_",
  ]);
});

test("controls, icons and text only a screen reader hears are left out", () => {
  expect(
    markdownOf(
      '<div><span class="sr-only">Answer options</span><button>Copy</button><input type="radio"><span aria-hidden="true">·</span><span role="img" aria-label="Selected">✓</span><span>Keep</span><textarea>draft</textarea></div><p hidden>gone</p>'
    )
  ).toEqual(["Keep"]);
});

test("what the page lays out as a block is a block, whatever element draws it", () => {
  // An option's row: its label, and its description on the line under it.
  expect(
    markdownOf(
      '<label style="display: flex"><input type="radio"><span><span><strong>Ship</strong> as is</span><span style="display: block">The flag ships on Monday.</span></span></label><label style="display: flex"><span>Hold</span></label>'
    )
  ).toEqual(["**Ship** as is", "The flag ships on Monday.", "Hold"]);
  // A wrapper that takes no box of its own breaks nothing.
  expect(markdownOf('<p>one <span style="display: contents">two</span> three</p>')).toEqual([
    "one two three",
  ]);
});

test("text that keeps its own line breaks reads one block a line", () => {
  expect(
    markdownOf('<p style="white-space: pre-wrap">first line\nsecond *line*\n\nthird</p>')
  ).toEqual(["first line", "second \\*line\\*", "third"]);
});
