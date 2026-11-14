import { readdirSync } from "node:fs";
import { join } from "node:path";
import ts from "typescript";

/**
 * Reading the app's own source as the compiler reads it, for the rows that hold a rule no type can
 * hold: which modules the app ships, and each one parsed by TypeScript itself, so a name in a
 * comment, a string, a regex literal or JSX text is never taken for code, and an apostrophe in JSX
 * text or a quote in a regex literal hides nothing after it.
 */

/** `web/src`, the root every path named here is read from. */
const webSrc = join(import.meta.dir, "..");

let shipped: readonly string[] | undefined;

/** The app's own source: every module under `web/src` but the tests and their fixtures, read
 *  from the directory once and kept, since one file holds several rules over the same list. The
 *  separators are those of a path a reader writes - `/` on every host - before the fixtures are
 *  filtered out by it. */
export function shippedSources(): readonly string[] {
  shipped ??= readdirSync(webSrc, { recursive: true })
    .map((entry) => String(entry).replaceAll("\\", "/"))
    .filter(
      (path) =>
        (path.endsWith(".ts") || path.endsWith(".tsx")) &&
        !path.includes(".test.") &&
        !path.startsWith("__tests__/")
    )
    .sort();
  return shipped;
}

/** `source`, the module at `path`, parsed as the compiler parses it: a `.tsx` module as TSX, any
 *  other as TypeScript, where `<T>(value: T) => value` is a generic function and not JSX. */
export function parsed(path: string, source: string): ts.SourceFile {
  const kind = path.endsWith(".tsx") ? ts.ScriptKind.TSX : ts.ScriptKind.TS;
  return ts.createSourceFile(path, source, ts.ScriptTarget.Latest, true, kind);
}

/** Every node of `file`, parents before their children, in source order. */
export function nodesOf(file: ts.SourceFile): ts.Node[] {
  const nodes: ts.Node[] = [];
  const visit = (node: ts.Node) => {
    nodes.push(node);
    ts.forEachChild(node, visit);
  };
  visit(file);
  return nodes;
}

/** `file`'s path and the line `node` starts on, counted from one: `path:line`. */
export function siteOf(file: ts.SourceFile, node: ts.Node): string {
  return `${file.fileName}:${file.getLineAndCharacterOfPosition(node.getStart(file)).line + 1}`;
}
