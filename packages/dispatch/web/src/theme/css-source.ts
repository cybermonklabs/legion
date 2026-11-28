/**
 * The one scanner the look tests read a style sheet or a source file through. It knows the strings
 * (`"…"` and `'…'`, with escapes) and the unquoted `url(…)` of a style sheet, and the template
 * literals (`` `…${…}…` ``) and `//` line comments of a TypeScript source, so what it reports
 * (a comment, a rule's brace, a declaration's `;`, a template expression) is never text inside a
 * string. A pattern such as `/\/\*[\s\S]*?\*\//g` knows none of them: a `/*` inside `content: "/*"`
 * or `"src/**\/*.ts"` starts a comment for it, and everything up to the next `*\/` goes; a pattern
 * that counts braces stops at the `}` of a string.
 */

type Language = "css" | "ts";

/** What `scan` reports. A comment is `start` to `end`; a template expression is its `${` to just
 *  past its `}`; a mark is the `{`, the `}` or the `;` of a style sheet at `index`. */
type ScanEvent =
  | { readonly kind: "comment"; readonly end: number; readonly start: number }
  | { readonly kind: "expression"; readonly end: number; readonly start: number }
  | { readonly kind: "mark"; readonly char: "{" | "}" | ";"; readonly index: number };

/** A `${` being read: where it starts, and how many `{` of its own are still open. */
interface Opening {
  braces: number;
  readonly start: number;
}

/** The end of the string literal that opens at `start`: just past its closing quote, or at the
 *  line break that ends it unclosed, as a CSS string and a JavaScript string both end. */
function quotedEnd(source: string, start: number): number {
  const quote = source[start];
  let index = start + 1;
  while (index < source.length) {
    const char = source[index];
    if (char === "\\") {
      index += 2;
    } else if (char === quote) {
      return index + 1;
    } else if (char === "\n") {
      return index;
    } else {
      index++;
    }
  }
  return source.length;
}

/** Where an unquoted `url(…)` opens: `url(`, spaces, and a first character that is no quote. */
const UNQUOTED_URL = /url\(\s*[^\s"']/iy;

/** The end of the unquoted `url(…)` that opens at `start`, just past its `)`: a CSS token whose
 *  whole text, `/*` and `;` too, is the address. -1 where none opens there; a quoted `url("…")` is
 *  a string inside parentheses and the string scan reads it. */
function unquotedUrlEnd(source: string, start: number): number {
  UNQUOTED_URL.lastIndex = start;
  if (!UNQUOTED_URL.test(source) || (start > 0 && /[\w-]/.test(source[start - 1]))) {
    return -1;
  }
  const close = source.indexOf(")", start + 4);
  return close === -1 ? source.length : close + 1;
}

/** The end of the block comment that opens at `start`, just past its `*\/`. */
function commentEnd(source: string, start: number): number {
  const close = source.indexOf("*/", start + 2);
  return close === -1 ? source.length : close + 2;
}

/** The end of the part of the template literal that starts at `from`, inside its backticks: just
 *  past the closing backtick, or just past a `${`, which `openings` then holds with no `{` of its
 *  own open yet. */
function templatePartEnd(source: string, from: number, openings: Opening[]): number {
  let index = from;
  while (index < source.length) {
    const char = source[index];
    if (char === "\\") {
      index += 2;
    } else if (char === "`") {
      return index + 1;
    } else if (char === "$" && source[index + 1] === "{") {
      openings.push({ braces: 0, start: index });
      return index + 2;
    } else {
      index++;
    }
  }
  return source.length;
}

/**
 * Walks `source` once and reports, in the order they close, its comments, and then by `language`:
 * the `{`, `}` and `;` of a `"css"` source that stand outside every string, comment and unquoted
 * `url(…)`, or the template expressions of a `"ts"` source, each balanced against the braces
 * inside it, a nested template's too.
 */
function* scan(source: string, language: Language): Generator<ScanEvent> {
  const typescript = language === "ts";
  const openings: Opening[] = [];
  let index = 0;
  while (index < source.length) {
    const char = source[index];
    if (char === '"' || char === "'") {
      index = quotedEnd(source, index);
    } else if (typescript && char === "`") {
      index = templatePartEnd(source, index + 1, openings);
    } else if (typescript && openings.length > 0 && (char === "{" || char === "}")) {
      const opening = openings[openings.length - 1];
      if (char === "{") {
        opening.braces++;
        index++;
      } else if (opening.braces > 0) {
        opening.braces--;
        index++;
      } else {
        openings.pop();
        yield { end: index + 1, kind: "expression", start: opening.start };
        index = templatePartEnd(source, index + 1, openings);
      }
    } else if (char === "/" && source[index + 1] === "*") {
      const end = commentEnd(source, index);
      yield { end, kind: "comment", start: index };
      index = end;
    } else if (typescript && char === "/" && source[index + 1] === "/") {
      const lineBreak = source.indexOf("\n", index);
      const end = lineBreak === -1 ? source.length : lineBreak;
      yield { end, kind: "comment", start: index };
      index = end;
    } else if (!typescript && (char === "u" || char === "U")) {
      const end = unquotedUrlEnd(source, index);
      index = end === -1 ? index + 1 : end;
    } else if (!typescript && (char === "{" || char === "}" || char === ";")) {
      yield { char, index, kind: "mark" };
      index++;
    } else {
      index++;
    }
  }
}

/**
 * The style sheet `source` with its comments taken out: each comment becomes the line breaks it
 * held, or one space where it held none, so no two tokens run together and every line keeps its
 * number. What a string or an unquoted `url(…)` holds is never a comment.
 */
export function withoutComments(source: string): string {
  let kept = "";
  let from = 0;
  for (const event of scan(source, "css")) {
    if (event.kind === "comment") {
      const lineBreaks = source.slice(event.start, event.end).replace(/[^\n]/g, "");
      kept += source.slice(from, event.start) + (lineBreaks || " ");
      from = event.end;
    }
  }
  return kept + source.slice(from);
}

/**
 * The template expressions of a TypeScript `source` (`${…}` inside backticks), each from its `${`
 * to just past its `}`, in the order written. An expression holds the braces and the templates
 * written inside it, and a `${` in a string or a comment is none.
 */
export function templateExpressions(source: string): { start: number; end: number }[] {
  const expressions: { start: number; end: number }[] = [];
  for (const event of scan(source, "ts")) {
    if (event.kind === "expression") {
      expressions.push({ end: event.end, start: event.start });
    }
  }
  return expressions.sort((a, b) => a.start - b.start);
}

/** One block of a style sheet: the selector or at-rule before its `{`, and what is inside. */
export interface CssBlock {
  readonly body: string;
  readonly prelude: string;
}

/**
 * The blocks of `css` that stand at its top level, in the order written: each with its prelude,
 * trimmed and with its comments out, and its body as written. A `{` or a `}` inside a string or an
 * unquoted `url(…)` opens and closes nothing. A body is a style sheet too, so `topLevelBlocks`
 * reads the blocks of an `@media` inside it.
 */
export function topLevelBlocks(css: string): CssBlock[] {
  const blocks: CssBlock[] = [];
  let depth = 0;
  let preludeStart = 0;
  let bodyStart = 0;
  let prelude = "";
  for (const event of scan(css, "css")) {
    if (event.kind !== "mark") {
      continue;
    }
    if (event.char === "{") {
      if (depth === 0) {
        prelude = withoutComments(css.slice(preludeStart, event.index)).trim();
        bodyStart = event.index + 1;
      }
      depth++;
    } else if (depth === 0) {
      preludeStart = event.index + 1;
    } else if (event.char === "}") {
      depth--;
      if (depth === 0) {
        blocks.push({ body: css.slice(bodyStart, event.index), prelude });
        preludeStart = event.index + 1;
      }
    }
  }
  return blocks;
}

/** One declaration of a style sheet: its property, its value with the comments taken out, and the
 *  text of the comment that ends it, `undefined` where none does. */
export interface CssDeclaration {
  readonly comment: string | undefined;
  readonly property: string;
  readonly value: string;
}

const PROPERTY_NAME = /^-{0,2}[a-zA-Z][\w-]*$/;

/** The declarations of `css`, which is a rule's inside where `inside` is 1 and a whole style sheet
 *  where it is 0, whose declarations are those of its rules. */
function declarationsAt(css: string, inside: number): CssDeclaration[] {
  const declarations: CssDeclaration[] = [];
  /** How many rules are open. */
  let depth = inside;
  /** Where the declaration or the selector being read began. */
  let start = 0;
  const declare = (end: number, comment: string | undefined): void => {
    if (depth === 0) {
      return;
    }
    const text = withoutComments(css.slice(start, end));
    const colon = text.indexOf(":");
    if (colon === -1) {
      return;
    }
    const property = text.slice(0, colon).trim();
    if (PROPERTY_NAME.test(property)) {
      declarations.push({ comment, property, value: text.slice(colon + 1).trim() });
    }
  };
  for (const event of scan(css, "css")) {
    if (event.kind !== "mark") {
      continue;
    }
    if (event.char === "{") {
      depth++;
      start = event.index + 1;
    } else if (event.char === "}") {
      declare(event.index, undefined);
      depth = Math.max(0, depth - 1);
      start = event.index + 1;
    } else {
      let after = event.index + 1;
      while (css[after] === " " || css[after] === "\t") {
        after++;
      }
      const close = css.startsWith("/*", after) ? css.indexOf("*/", after + 2) : -1;
      declare(event.index, close === -1 ? undefined : css.slice(after + 2, close).trim());
      start = close === -1 ? event.index + 1 : close + 2;
    }
  }
  declare(css.length, undefined);
  return declarations;
}

/**
 * Each declaration of the style sheet `css`, in the order written and at any depth of rule: its
 * property, its value and the comment that ends it, which is the comment that follows its `;` on
 * its own line. A comment of any other place ends no declaration and is part of none: a colour
 * named in a comment between two rules, or ahead of a declaration, or inside its value, is no
 * declaration's. A `;`, a `{`, a `}` or a `/*` inside a string or an unquoted `url(…)` ends nothing
 * and starts nothing. A declaration the last of its rule with no `;` has no comment.
 */
export function declarationsWithTrailingComment(css: string): CssDeclaration[] {
  return declarationsAt(css, 0);
}

/** `declarationsWithTrailingComment` for what is inside one rule, a `CssBlock`'s `body`. */
export function ruleDeclarations(body: string): CssDeclaration[] {
  return declarationsAt(body, 1);
}
