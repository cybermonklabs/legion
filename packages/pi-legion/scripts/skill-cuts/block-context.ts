/**
 * The rows Oh My Pi adds to a read whose window does not show every line of its text: each
 * enclosing block's boundary line outside the window, in source order, with `…` rows across the
 * gaps (packages/coding-agent/src/utils/block-context.ts at the tag
 * v18.18.6-sami.20261119-162325 of cybermonklabs/oh-my-pi, ported as it is, but for where the
 * native addon comes from). For each multi-line block that opens on a shown line and closes past
 * the window, its closing line; for each that closes on a shown line and opens before it, its
 * opening line. The blocks are tree-sitter's (`enclosingBlockBoundaries`), through the pinned Oh
 * My Pi's own native addon (natives.ts, which opens only the pin's and says why where it has
 * none); for a file of no language the addon knows, or one that does not parse cleanly, they are a
 * lexical scan's of its brackets (`lexicalBracketContext`). A read that needs block context where
 * the runner has no pinned addon throws: it never falls back to the lexical scan for want of the
 * addon. A window whose blocks the pin finds with a WebAssembly grammar it downloads first is one
 * a caller leaves unanswered (natives.ts's `missingGrammar`).
 */
import { natives } from "./natives";

const OPEN_TO_CLOSE: Record<string, string> = {
  "(": ")",
  "[": "]",
  "{": "}",
};

const CLOSE_TO_OPEN: Record<string, string> = {
  ")": "(",
  "]": "[",
  "}": "{",
};

/**
 * Lines containing any of these characters need char-by-char scanning when
 * the scanner is in code mode: brackets, string quotes, or comment markers.
 * Anything else can neither add boundaries nor change scanner state.
 */
const LINE_SCAN_PATTERN = /[()[\]{}'"`/#]/;

export interface LineSpan {
  startLine: number;
  endLine: number;
}

/** Where the source came from, so tree-sitter can pick a grammar: its path, and the whole text
 *  `fullLines` was split from. */
export interface BlockContextSource {
  path?: string;
  text?: string;
}

export type LineEntry =
  | { kind: "line"; lineNumber: number; text: string; context: boolean }
  | { kind: "ellipsis" };

interface StackEntry {
  opener: string;
  lineNumber: number;
  text: string;
  visible: boolean;
}

type ScannerMode = "code" | "single" | "double" | "template" | "blockComment";

function normalizeLineSpans(spans: readonly LineSpan[], totalLines: number): LineSpan[] {
  if (totalLines <= 0) return [];
  const normalized: LineSpan[] = [];
  for (const span of spans) {
    const startLine = Math.max(1, Math.trunc(span.startLine));
    const endLine = Math.min(totalLines, Math.trunc(span.endLine));
    if (endLine < startLine) continue;
    normalized.push({ startLine, endLine });
  }
  if (normalized.length <= 1) return normalized;
  normalized.sort(
    (left, right) => left.startLine - right.startLine || left.endLine - right.endLine
  );
  const merged: LineSpan[] = [];
  for (const span of normalized) {
    const previous = merged[merged.length - 1];
    if (previous && span.startLine <= previous.endLine + 1) {
      previous.endLine = Math.max(previous.endLine, span.endLine);
      continue;
    }
    merged.push({ ...span });
  }
  return merged;
}

function visibleLineNumbers(spans: readonly LineSpan[]): Set<number> {
  const visible = new Set<number>();
  for (const span of spans) {
    for (let line = span.startLine; line <= span.endLine; line++) {
      visible.add(line);
    }
  }
  return visible;
}

function hasEveryLineVisible(visible: ReadonlySet<number>, totalLines: number): boolean {
  return totalLines > 0 && visible.size >= totalLines;
}

/** Whether `spans` show all of a `totalLines`-line source, so block context has nothing to add. */
export function spansCoverEveryLine(spans: readonly LineSpan[], totalLines: number): boolean {
  return hasEveryLineVisible(visibleLineNumbers(normalizeLineSpans(spans, totalLines)), totalLines);
}

/** Collapse a set of visible line numbers into sorted, merged inclusive spans. */
function visibleSetToSpans(visible: ReadonlySet<number>): LineSpan[] {
  const sorted = [...visible].sort((left, right) => left - right);
  const spans: LineSpan[] = [];
  for (const line of sorted) {
    const previous = spans[spans.length - 1];
    if (previous && line <= previous.endLine + 1) {
      previous.endLine = line;
      continue;
    }
    spans.push({ startLine: line, endLine: line });
  }
  return spans;
}

/**
 * Tree-sitter-backed block boundaries. For each multi-line named node whose
 * span crosses the visible window, the native side returns the boundary line
 * outside that window (closer when the opener is shown, opener when the closer
 * is shown). Returns `null` when the language is unrecognized or the source has
 * a syntax error so the caller can fall back to a lexical bracket scan.
 */
function nativeBlockContext(
  fullLines: readonly string[],
  visible: ReadonlySet<number>,
  source: BlockContextSource
): Map<number, string> | null {
  if (!source.path) return null;
  const ranges = visibleSetToSpans(visible);
  if (ranges.length === 0) return new Map();
  // Opened outside the try: an addon that cannot be had is the runner's fault, not a parse's.
  const addon = natives();
  let boundaries: number[] | null;
  try {
    boundaries = addon.enclosingBlockBoundaries({
      code: source.text ?? fullLines.join("\n"),
      path: source.path,
      ranges,
    });
  } catch {
    // The pin logs it and uses the lexical bracket fallback.
    return null;
  }
  if (boundaries === null) return null;
  const context = new Map<number, string>();
  for (const lineNumber of boundaries) {
    if (visible.has(lineNumber)) continue;
    context.set(lineNumber, fullLines[lineNumber - 1] ?? "");
  }
  return context;
}

function findMatchingStackIndex(stack: readonly StackEntry[], opener: string): number {
  for (let index = stack.length - 1; index >= 0; index--) {
    if (stack[index].opener === opener) return index;
  }
  return -1;
}

function isHashCommentStart(line: string, index: number): boolean {
  if (line[index] !== "#") return false;
  for (let i = 0; i < index; i++) {
    const ch = line[i];
    if (ch !== " " && ch !== "\t") return false;
  }
  return true;
}

/**
 * Lexical bracket-matching fallback for sources tree-sitter can't parse
 * (unknown extensions, syntax errors). Pairs `()[]{}` while skipping strings
 * and line/block comments, and reports the matching line when one endpoint is
 * visible and the other is not.
 */
function lexicalBracketContext(
  fullLines: readonly string[],
  visible: ReadonlySet<number>
): Map<number, string> {
  const context = new Map<number, string>();
  const stack: StackEntry[] = [];
  let mode: ScannerMode = "code";
  let escaped = false;
  // Set when the single pass below sees a bracket opener outside strings
  // and comments. Without one, no boundary lines can exist and the trailing
  // visible-line sweep is skipped.
  let sawBracket = false;
  for (let lineIndex = 0; lineIndex < fullLines.length; lineIndex++) {
    const line = fullLines[lineIndex] ?? "";
    // In code mode a line without brackets, quotes, or comment markers can
    // neither add boundaries nor change scanner state: skip it char-by-char
    // cost without a separate pre-scan pass. Other modes still scan every
    // line so multi-line strings and block comments track their end.
    if (mode === "code" && !LINE_SCAN_PATTERN.test(line)) continue;
    const lineNumber = lineIndex + 1;
    const lineVisible = visible.has(lineNumber);
    let index = 0;
    while (index < line.length) {
      const ch = line[index];
      const next = index + 1 < line.length ? line[index + 1] : "";

      if (mode === "blockComment") {
        if (ch === "*" && next === "/") {
          mode = "code";
          index += 2;
          continue;
        }
        index++;
        continue;
      }

      if (mode === "single" || mode === "double" || mode === "template") {
        if (escaped) {
          escaped = false;
          index++;
          continue;
        }
        if (ch === "\\") {
          escaped = true;
          index++;
          continue;
        }
        if (
          (mode === "single" && ch === "'") ||
          (mode === "double" && ch === '"') ||
          (mode === "template" && ch === "`")
        ) {
          mode = "code";
        }
        index++;
        continue;
      }

      if (ch === "/" && next === "/") break;
      if (ch === "/" && next === "*") {
        mode = "blockComment";
        index += 2;
        continue;
      }
      if (isHashCommentStart(line, index)) break;
      if (ch === "'") {
        mode = "single";
        escaped = false;
        index++;
        continue;
      }
      if (ch === '"') {
        mode = "double";
        escaped = false;
        index++;
        continue;
      }
      if (ch === "`") {
        mode = "template";
        escaped = false;
        index++;
        continue;
      }

      if (OPEN_TO_CLOSE[ch]) {
        sawBracket = true;
        stack.push({ opener: ch, lineNumber, text: line, visible: lineVisible });
        index++;
        continue;
      }

      const opener = CLOSE_TO_OPEN[ch];
      if (opener) {
        const matchIndex = findMatchingStackIndex(stack, opener);
        if (matchIndex !== -1) {
          const [matched] = stack.splice(matchIndex);
          if (matched) {
            if (lineVisible && !matched.visible) context.set(matched.lineNumber, matched.text);
            if (matched.visible && !lineVisible) context.set(lineNumber, line);
          }
        }
      }

      index++;
    }

    if (mode === "single" || mode === "double") {
      mode = "code";
      escaped = false;
    }
  }
  // No openers outside strings/comments means no boundary lines exist;
  // skip the visible-line sweep entirely.
  if (sawBracket) for (const lineNumber of visible) context.delete(lineNumber);
  return context;
}

/**
 * Resolve the off-window boundary lines for a visible window: tree-sitter
 * syntactic spans first (covers brace and indentation languages), falling back
 * to a lexical bracket scan when the grammar is unavailable. Returns a map of
 * `lineNumber → source text` for the lines to surface, never including a line
 * already visible.
 */
function findBlockContextLines(
  fullLines: readonly string[],
  visible: ReadonlySet<number>,
  source: BlockContextSource
): Map<number, string> {
  if (visible.size === 0 || hasEveryLineVisible(visible, fullLines.length)) return new Map();
  return (
    nativeBlockContext(fullLines, visible, source) ?? lexicalBracketContext(fullLines, visible)
  );
}

/**
 * Build display entries for `visibleSpans` plus any off-window block-boundary
 * lines, in source order, with `{ kind: "ellipsis" }` markers inserted across
 * non-contiguous gaps. `options.lineText` lets callers substitute display text
 * (e.g. column-truncated lines) for a given line number.
 */
export function buildLineEntriesWithBlockContext(
  fullLines: readonly string[],
  visibleSpans: readonly LineSpan[],
  source: BlockContextSource = {},
  options: {
    lineText?: (lineNumber: number, sourceText: string, context: boolean) => string;
  } = {}
): LineEntry[] {
  const spans = normalizeLineSpans(visibleSpans, fullLines.length);
  const visible = visibleLineNumbers(spans);
  const context = findBlockContextLines(fullLines, visible, source);
  const allLines = new Set<number>(visible);
  for (const lineNumber of context.keys()) allLines.add(lineNumber);

  const sorted = [...allLines].sort((left, right) => left - right);
  const entries: LineEntry[] = [];
  let previousLine: number | undefined;
  for (const lineNumber of sorted) {
    if (previousLine !== undefined && lineNumber > previousLine + 1) {
      entries.push({ kind: "ellipsis" });
    }
    const sourceText = fullLines[lineNumber - 1] ?? "";
    const isContext = context.has(lineNumber);
    entries.push({
      kind: "line",
      lineNumber,
      text: options.lineText?.(lineNumber, sourceText, isContext) ?? sourceText,
      context: isContext,
    });
    previousLine = lineNumber;
  }

  return entries;
}

/** The `…` row the pin shows across a gap (read-format.ts's `BRACKET_CONTEXT_ELLIPSIS`). */
export const BRACKET_CONTEXT_ELLIPSIS = "…";

export function lineEntriesToPlainText(
  entries: readonly LineEntry[],
  ellipsis = BRACKET_CONTEXT_ELLIPSIS
): string {
  return entries.map((entry) => (entry.kind === "ellipsis" ? ellipsis : entry.text)).join("\n");
}
