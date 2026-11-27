// The code of each TypeScript or JavaScript file that .github/scripts/ci_citations.py holds a
// citation token's name against: the file's text with every comment removed, each comment's
// newlines kept so that a line of the code stays where it was, and every string, template and
// regular expression literal kept as it is written; and the file's identifiers, which an
// identifier the token names must be one of, so that no string or comment holds one.
//
// The comments and the identifiers are the ones TypeScript's own parser finds through the
// scanner it drives (ts.createScanner). A scanner alone cannot tell a regular expression from a
// division, the `}` that resumes a template from one that closes a block, or JSX text from code:
// the parser rescans each of those from what it has already read. Every comment is trivia
// before some token, the end-of-file token included, so the comments are the comment ranges of
// each token's trivia, up to where that token starts: TypeScript calls the ones on the line of
// the token before trailing and the rest leading, and both are read. JSDoc nodes are skipped, as
// their text is the comment itself.
//
// Usage: bun .github/scripts/ci_citations_code.ts < paths
// The paths come on stdin, each ended by a NUL byte, as .github/scripts/ci_citations_code.go
// takes them. For each path, in the order given, it prints the code and then the identifiers,
// one a line, each followed by a NUL byte. A path that cannot be read, or whose extension is no
// TypeScript or JavaScript one, is named on stderr, and the exit status is then 1.
import { readFileSync } from "node:fs";
import ts from "typescript";

const KINDS: Record<string, ts.ScriptKind> = {
  ts: ts.ScriptKind.TS,
  tsx: ts.ScriptKind.TSX,
  js: ts.ScriptKind.JS,
  mjs: ts.ScriptKind.JS,
  cjs: ts.ScriptKind.JS,
};

function codeOf(path: string, text: string, kind: ts.ScriptKind): string {
  const file = ts.createSourceFile(path, text, ts.ScriptTarget.Latest, true, kind);
  // Comment start to comment end: one comment is read from the trivia of one token only, but
  // the trailing and the leading ranges of a token are read apart.
  const comments = new Map<number, number>();
  const identifiers = new Set<string>();
  const visit = (node: ts.Node): void => {
    if (node.kind >= ts.SyntaxKind.FirstJSDocNode && node.kind <= ts.SyntaxKind.LastJSDocNode) {
      return;
    }
    const children = node.getChildren(file);
    if (children.length > 0) {
      for (const child of children) {
        visit(child);
      }
      return;
    }
    if (ts.isIdentifier(node)) {
      identifiers.add(node.text);
    }
    const start = node.getStart(file);
    const ranges = [
      ...(ts.getTrailingCommentRanges(text, node.pos) ?? []),
      ...(ts.getLeadingCommentRanges(text, node.pos) ?? []),
    ];
    for (const range of ranges) {
      if (range.end <= start) {
        comments.set(range.pos, range.end);
      }
    }
  };
  visit(file);
  let code = "";
  let last = 0;
  for (const [pos, end] of [...comments].sort((a, b) => a[0] - b[0])) {
    code += text.slice(last, pos) + text.slice(pos, end).replace(/[^\n]/g, "");
    last = end;
  }
  return `${code}${text.slice(last)}\0${[...identifiers].join("\n")}\0`;
}

let failed = false;
const out: string[] = [];
const paths = readFileSync(0, "utf8").split("\0");
for (const path of paths.filter((path) => path !== "")) {
  const kind = KINDS[path.slice(path.lastIndexOf(".") + 1)];
  if (kind === undefined) {
    console.error(`${path}: no TypeScript or JavaScript file`);
    failed = true;
    continue;
  }
  let text: string;
  try {
    text = readFileSync(path, "utf8");
  } catch (error) {
    console.error(`${path}: ${error instanceof Error ? error.message : String(error)}`);
    failed = true;
    continue;
  }
  out.push(codeOf(path, text, kind));
}
process.stdout.write(out.join(""));
if (failed) {
  process.exitCode = 1;
}
