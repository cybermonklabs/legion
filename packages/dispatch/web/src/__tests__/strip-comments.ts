import * as ts from "typescript";

/**
 * A source file with its comments taken out and its lines left where they were.
 *
 * Every guard that reads the repository as text has the same problem: it has to describe the
 * marks it refuses in prose, and prose sits in the very files it scans, so a guard that read
 * its own comments would refuse the code that documents it. Stripping first is what lets each
 * one say what it looks for (`theme/no-raw-colors.test.ts`,
 * `components/one-definition.test.ts`).
 *
 * TypeScript decides what is a comment, not a pattern. A `/*` or a `//` inside a string
 * (`accept="image/*"`, a URL), a template literal or a regular expression belongs to that
 * literal. In JSX text, a `//` is text the page draws. The parser drives its scanner through each
 * of these in the context that decides what it is, so a comment is only what is left between two
 * tokens, and `createScanner` reads it there out of whitespace and comments alone. `fileName`'s
 * extension picks the grammar: a `.ts` file reads `<T>(value: T) => value` as a generic arrow
 * function, where a `.tsx` file reads it as JSX.
 *
 * Every character of a comment becomes a space and every newline stays, so a line number read
 * off the result is the line number in the file the reader has to open.
 */
export function stripComments(source: string, fileName: string): string {
  const file = ts.createSourceFile(fileName, source, {
    jsDocParsingMode: ts.JSDocParsingMode.ParseNone,
    languageVersion: ts.ScriptTarget.Latest,
  });
  const trivia = ts.createScanner(ts.ScriptTarget.Latest, false);
  const comments: { pos: number; end: number }[] = [];
  const visit = (node: ts.Node): void => {
    if (!ts.isToken(node)) {
      for (const child of node.getChildren(file)) visit(child);
      return;
    }
    // JSX text has no trivia of its own: its whitespace and any `//` in it are the text.
    if (node.kind === ts.SyntaxKind.JsxText || node.kind === ts.SyntaxKind.JsxTextAllWhiteSpaces) {
      return;
    }
    trivia.setText(source, node.pos, node.getStart(file) - node.pos);
    for (let kind = trivia.scan(); kind !== ts.SyntaxKind.EndOfFileToken; kind = trivia.scan()) {
      if (
        kind === ts.SyntaxKind.SingleLineCommentTrivia ||
        kind === ts.SyntaxKind.MultiLineCommentTrivia
      ) {
        comments.push({ end: trivia.getTokenEnd(), pos: trivia.getTokenStart() });
      }
    }
  };
  visit(file);
  let stripped = "";
  let kept = 0;
  for (const { pos, end } of comments) {
    stripped += source.slice(kept, pos) + source.slice(pos, end).replace(/[^\n]/g, " ");
    kept = end;
  }
  return stripped + source.slice(kept);
}
