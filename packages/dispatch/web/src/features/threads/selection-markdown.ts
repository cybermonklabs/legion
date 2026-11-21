// Quote reply's Markdown (INFRA-3231's R7): the words a reader selected,
// read from the elements the page draws them with back into the Markdown that draws them again,
// as GitHub's Quote reply does (`@github/quote-selection` converts the selected HTML to
// Markdown). That library is not used: its latest release (2.2.0) ships no code, and 2.1.0 leaves
// text unescaped, so `__tests__` would post as a bold "tests". Inside a text the page drew from
// Markdown (`MarkdownBody`, `.dispatch-markdown`), bold, italics, strikethrough, code spans, code
// blocks, links, pictures, headings, quotes, list items and table rows keep their Markdown;
// outside one - an author's name, a list the page lays its replies out in - the same elements
// are the page's own and post as their words. Every other character posts as it reads.

/** The class every text the page draws from Markdown carries (`markdownClassName`). */
const MARKDOWN_CLASS = "dispatch-markdown";

/** The elements that open a block of their own whatever their style says. */
const BLOCK_TAGS: Record<string, true> = {
  ADDRESS: true,
  ARTICLE: true,
  ASIDE: true,
  BLOCKQUOTE: true,
  DD: true,
  DETAILS: true,
  DIV: true,
  DL: true,
  DT: true,
  FIELDSET: true,
  FIGCAPTION: true,
  FIGURE: true,
  FOOTER: true,
  FORM: true,
  H1: true,
  H2: true,
  H3: true,
  H4: true,
  H5: true,
  H6: true,
  HEADER: true,
  HR: true,
  LI: true,
  MAIN: true,
  NAV: true,
  OL: true,
  P: true,
  PRE: true,
  SECTION: true,
  SUMMARY: true,
  TABLE: true,
  TBODY: true,
  TFOOT: true,
  THEAD: true,
  TR: true,
  UL: true,
};

/** Controls and media: what a reader selects across them is the words around them. */
const SKIPPED_TAGS: Record<string, true> = {
  AUDIO: true,
  BUTTON: true,
  CANVAS: true,
  IFRAME: true,
  INPUT: true,
  NOSCRIPT: true,
  OPTION: true,
  SCRIPT: true,
  SELECT: true,
  STYLE: true,
  SVG: true,
  TEMPLATE: true,
  TEXTAREA: true,
  VIDEO: true,
};

const CODE_TAGS: Record<string, true> = { CODE: true, KBD: true, SAMP: true };

/**
 * `text` with every character Markdown would read as formatting escaped, so it posts as it reads:
 * backslashes, backticks, asterisks, brackets and tildes always; an underscore unless a letter or
 * digit stands on both sides of it, where it opens no emphasis (`snake_case` stays as typed); a
 * `<` that would open a tag or an autolink, and a `&` that would open an entity.
 */
export function escapeMarkdownText(text: string): string {
  const word = /[\p{L}\p{N}]/u;
  let out = "";
  for (let index = 0; index < text.length; index += 1) {
    const char = text.charAt(index);
    if (char === "_") {
      const intraword = word.test(text.charAt(index - 1)) && word.test(text.charAt(index + 1));
      out += intraword ? "_" : "\\_";
    } else if ("\\`*[]~".includes(char)) {
      out += `\\${char}`;
    } else if (char === "<" && /[A-Za-z/!?]/.test(text.charAt(index + 1))) {
      out += "\\<";
    } else if (char === "&" && /^#?[A-Za-z0-9]+;/.test(text.slice(index + 1))) {
      out += "\\&";
    } else {
      out += char;
    }
  }
  return out;
}

/** `text`, already escaped, where it opens a line: what Markdown reads as a block's mark there -
 *  a heading's `#`, a quote's `>`, a list item's `-`, `+` or `1.`, a thematic break or a setext
 *  underline, a directive's `:::` - posts as the characters it is. */
export function escapeLineStart(text: string): string {
  if (/^(-{3,}|=+)\s*$/.test(text)) return `\\${text}`;
  return text
    .replace(/^(#{1,6}(?=\s|$)|>|[-+](?=\s|$)|:::)/, "\\$1")
    .replace(/^(\d{1,9})([.)])(?=\s|$)/, "$1\\$2");
}

/** A code span holding `text`: fenced by one backtick more than the longest run inside it, and
 *  padded where it starts or ends with one, so it holds any text as it reads. */
function codeSpan(text: string): string {
  const longest = Math.max(0, ...[...text.matchAll(/`+/g)].map((run) => run[0].length));
  const fence = "`".repeat(longest + 1);
  const pad = text.startsWith("`") || text.endsWith("`") ? " " : "";
  return `${fence}${pad}${text}${pad}${fence}`;
}

/** A link's destination, in angle brackets where a space or a parenthesis would end it early. */
function destination(href: string): string {
  return /[\s()<>]/.test(href) ? `<${href.replace(/[<>]/g, encodeURIComponent)}>` : href;
}

interface Context {
  /** What opens each line of a block written here: `> ` for each quote it stands in, then the
   *  indent that keeps a list item's later blocks in that item. */
  readonly prefix: string;
  /** Inside an element that keeps its text's own line breaks (`white-space: pre*`). */
  readonly keepsLines: boolean;
  /** Inside a text the page drew from Markdown, whose elements are that Markdown's marks. */
  readonly markdown: boolean;
}

/** The Markdown blocks being written: the current block's inline text, and where it opens. */
class Writer {
  readonly blocks: string[] = [];
  private text = "";
  private prefix = "";
  private first = "";
  /** The mark the next block opens with - a list item's bullet, a heading's `#` - and the prefix
   *  of the lines it stands on, which the item's own later blocks indent past. */
  private pending: { prefix: string; first: string } | undefined;

  open(prefix: string, first: string): void {
    this.flush();
    this.pending = { first, prefix };
  }

  /** Ends the element `open` was for: its block, and its mark where no words took it. */
  close(): void {
    this.flush();
    this.pending = undefined;
  }

  get atLineStart(): boolean {
    return this.text === "" || this.text.endsWith("\n");
  }

  /** Appends inline Markdown; the first of a block takes the block's prefix and mark. */
  append(markdown: string, ctx: Context): void {
    let next = markdown;
    if (this.atLineStart) next = next.trimStart();
    if (next === "") return;
    if (this.text === "") {
      this.prefix = this.pending?.prefix ?? ctx.prefix;
      this.first = this.pending?.first ?? "";
      this.pending = undefined;
    }
    if (this.text.endsWith(" ") && next.startsWith(" ")) next = next.slice(1);
    this.text += next;
  }

  /** Wraps what was appended since `start` in `open` and `close`, outside its edge spaces and the
   *  hard breaks that end it, while it holds words and no block ended inside it. */
  wrapSince(start: { length: number; blocks: number }, open: string, close = open): void {
    if (this.blocks.length !== start.blocks || this.text.length <= start.length) return;
    const inner = this.text.slice(start.length);
    const lead = /^\s*/.exec(inner)?.[0] ?? "";
    const trail = /(\\\n|\s)*$/.exec(inner.slice(lead.length))?.[0] ?? "";
    const core = inner.slice(lead.length, inner.length - trail.length);
    if (core === "") return;
    this.text = `${this.text.slice(0, start.length)}${lead}${open}${core}${close}${trail}`;
  }

  /** Replaces what was appended since `start` with `markdown`, as one inline piece. */
  replaceSince(start: { length: number; blocks: number }, markdown: string): void {
    if (this.blocks.length !== start.blocks) return;
    this.text = this.text.slice(0, start.length) + markdown;
  }

  mark(): { length: number; blocks: number } {
    return { blocks: this.blocks.length, length: this.text.length };
  }

  /** A whole block of its own, each line after the block's prefix: a code block, a rule. */
  block(lines: string, ctx: Context): void {
    this.flush();
    const prefix = this.pending?.prefix ?? ctx.prefix;
    const first = this.pending?.first ?? "";
    this.pending = undefined;
    this.blocks.push(
      lines
        .split("\n")
        .map((line, index) => `${prefix}${index === 0 ? first : " ".repeat(first.length)}${line}`)
        .join("\n")
    );
  }

  /** Ends the current block; one that holds no words is dropped. */
  flush(): void {
    const body = this.text.replace(/(\\\n|\s)+$/, "");
    this.text = "";
    if (body === "") return;
    const indent = " ".repeat(this.first.length);
    this.blocks.push(
      body
        .split("\n")
        .map((line, index) => `${this.prefix}${index === 0 ? this.first : indent}${line}`)
        .join("\n")
    );
  }
}

/**
 * The Markdown of the words `range` selects under `root`, one string per block the page draws -
 * a paragraph, a list item, an option's label, a code block - in the order they read. A block
 * starts with its own mark (a list item's `- ` or `2. `, a heading's `## `); text the page does
 * not show as words (controls, icons, text only a screen reader hears) is left out.
 */
export function selectionMarkdown(range: Range, root: Node): string[] {
  const writer = new Writer();

  /** The selected part of every text under `node`, as the characters it holds. */
  const selectedText = (node: Node): string => {
    if (node.nodeType === Node.TEXT_NODE) {
      const data = (node as Text).data;
      const start = node === range.startContainer ? range.startOffset : 0;
      const end = node === range.endContainer ? range.endOffset : data.length;
      return data.slice(start, end);
    }
    let text = "";
    for (const child of node.childNodes) {
      if (range.intersectsNode(child)) text += selectedText(child);
    }
    return text;
  };

  const style = (element: Element): CSSStyleDeclaration | undefined => {
    try {
      return getComputedStyle(element);
    } catch {
      return undefined;
    }
  };

  const unseen = (element: Element, computed: CSSStyleDeclaration | undefined): boolean =>
    SKIPPED_TAGS[element.tagName.toUpperCase()] === true ||
    element.hasAttribute("hidden") ||
    element.getAttribute("aria-hidden") === "true" ||
    element.getAttribute("role") === "img" ||
    element.classList.contains("sr-only") ||
    computed?.display === "none" ||
    computed?.visibility === "hidden";

  const isBlock = (element: Element, computed: CSSStyleDeclaration | undefined): boolean => {
    const display = computed?.display ?? "";
    if (display === "contents") return false;
    if (BLOCK_TAGS[element.tagName.toUpperCase()] === true) return true;
    return display !== "" && !display.startsWith("inline");
  };

  const children = (node: Node, ctx: Context): void => {
    for (const child of node.childNodes) walk(child, ctx);
  };

  const text = (node: Text, ctx: Context): void => {
    const raw = selectedText(node);
    if (raw === "") return;
    if (ctx.keepsLines) {
      // Text that keeps its own line breaks reads one block a line.
      raw.split("\n").forEach((line, index) => {
        if (index > 0) writer.flush();
        appendText(line.replace(/[\t ]+/g, " "), ctx);
      });
      return;
    }
    appendText(raw.replace(/[\t\n\r\f ]+/g, " ").replace(/\u00a0/g, " "), ctx);
  };

  const appendText = (value: string, ctx: Context): void => {
    const escaped = escapeMarkdownText(value);
    writer.append(writer.atLineStart ? escapeLineStart(escaped.trimStart()) : escaped, ctx);
  };

  const listItem = (item: Element, ctx: Context): void => {
    const list = item.parentElement;
    const tag = list?.tagName.toUpperCase();
    let bullet = "- ";
    if (tag === "OL" && list !== null) {
      const start = Number(list.getAttribute("start") ?? "1");
      const place = [...list.children].filter((one) => one.tagName === item.tagName).indexOf(item);
      bullet = `${(Number.isFinite(start) ? start : 1) + Math.max(0, place)}. `;
    }
    const checkbox = item.querySelector(
      ":scope > input[type=checkbox], :scope > * > input[type=checkbox]"
    );
    const checked = item.getAttribute("data-checked");
    if (checkbox instanceof HTMLInputElement) {
      bullet += checkbox.checked ? "[x] " : "[ ] ";
    } else if (checked === "true" || checked === "false") {
      bullet += checked === "true" ? "[x] " : "[ ] ";
    }
    writer.open(ctx.prefix, bullet);
    children(item, { ...ctx, prefix: ctx.prefix + " ".repeat(bullet.length) });
    writer.close();
  };

  const codeBlock = (pre: Element, ctx: Context): void => {
    const code = selectedText(pre).replace(/\n+$/, "");
    if (code.trim() === "") return;
    const language =
      pre.getAttribute("data-language") ??
      /(?:^|\s)language-(\S+)/.exec(pre.querySelector("code")?.className ?? "")?.[1] ??
      "";
    const longest = Math.max(0, ...[...code.matchAll(/`{3,}/g)].map((run) => run[0].length));
    const fence = "`".repeat(Math.max(3, longest + 1));
    writer.block(`${fence}${language}\n${code}\n${fence}`, ctx);
  };

  const link = (anchor: Element, ctx: Context): void => {
    // A Dispatch reference the page draws as a chip posts as the reference it is.
    const reference = anchor.getAttribute("data-dispatch-ref");
    if (reference !== null) {
      writer.append(reference, ctx);
      return;
    }
    const href = anchor.getAttribute("data-dispatch-href") ?? anchor.getAttribute("href") ?? "";
    const picture = anchor.querySelector("img");
    if (picture !== null && href !== "") {
      writer.append(
        `![${escapeMarkdownText(picture.getAttribute("alt") ?? "")}](${destination(href)})`,
        ctx
      );
      return;
    }
    const start = writer.mark();
    children(anchor, ctx);
    if (href === "") return;
    const plain = selectedText(anchor).trim();
    if (plain === href && /^(https?:|mailto:|dispatch:)/.test(href)) {
      writer.replaceSince(start, href);
      return;
    }
    writer.wrapSince(start, "[", `](${destination(href)})`);
  };

  /** Writes `node` as the Markdown mark it is, and says whether it is one. */
  const marked = (node: Element, tag: string, ctx: Context, inner: Context): boolean => {
    if (tag === "PRE") {
      codeBlock(node, ctx);
      return true;
    }
    if (CODE_TAGS[tag] === true) {
      const code = selectedText(node).replace(/\s*\n\s*/g, " ");
      if (code !== "") writer.append(codeSpan(code), ctx);
      return true;
    }
    if (tag === "IMG") {
      const src = node.getAttribute("src") ?? "";
      if (src !== "") {
        const alt = escapeMarkdownText(node.getAttribute("alt") ?? "");
        writer.append(`![${alt}](${destination(src)})`, ctx);
      }
      return true;
    }
    if (tag === "A") {
      link(node, ctx);
      return true;
    }
    if (tag === "LI") {
      listItem(node, inner);
      return true;
    }
    if (tag === "HR") {
      writer.block("---", ctx);
      return true;
    }
    if (tag === "BLOCKQUOTE") {
      writer.flush();
      children(node, { ...inner, prefix: `${ctx.prefix}> ` });
      writer.flush();
      return true;
    }
    const heading = /^H([1-6])$/.exec(tag);
    if (heading !== null) {
      writer.open(ctx.prefix, `${"#".repeat(Number(heading[1]))} `);
      children(node, inner);
      writer.close();
      return true;
    }
    if (tag === "TD" || tag === "TH") {
      if (!writer.atLineStart) writer.append(" | ", ctx);
      children(node, inner);
      return true;
    }
    const wrapper =
      tag === "STRONG" || tag === "B"
        ? "**"
        : tag === "EM" || tag === "I"
          ? "*"
          : tag === "DEL" || tag === "S" || tag === "STRIKE"
            ? "~~"
            : undefined;
    if (wrapper === undefined) return false;
    const start = writer.mark();
    children(node, inner);
    writer.wrapSince(start, wrapper);
    return true;
  };

  function walk(node: Node, ctx: Context): void {
    if (!range.intersectsNode(node)) return;
    if (node.nodeType === Node.TEXT_NODE) {
      text(node as Text, ctx);
      return;
    }
    if (!(node instanceof Element)) return;
    const computed = style(node);
    if (unseen(node, computed)) return;
    const tag = node.tagName.toUpperCase();
    const keepsLines = ctx.keepsLines || (computed?.whiteSpace ?? "").startsWith("pre");
    const markdown = ctx.markdown || node.classList.contains(MARKDOWN_CLASS);
    const inner: Context =
      keepsLines === ctx.keepsLines && markdown === ctx.markdown
        ? ctx
        : { ...ctx, keepsLines, markdown };
    if (tag === "BR") {
      if (ctx.keepsLines) writer.flush();
      else if (!writer.atLineStart) writer.append("\\\n", ctx);
      return;
    }
    if (ctx.markdown && marked(node, tag, ctx, inner)) return;
    if (isBlock(node, computed)) {
      writer.flush();
      children(node, inner);
      writer.flush();
      return;
    }
    children(node, inner);
  }

  const within = root instanceof Element ? root : root.parentElement;
  walk(root, {
    keepsLines: false,
    markdown: within?.closest(`.${MARKDOWN_CLASS}`) != null,
    prefix: "",
  });
  writer.flush();
  return writer.blocks;
}
