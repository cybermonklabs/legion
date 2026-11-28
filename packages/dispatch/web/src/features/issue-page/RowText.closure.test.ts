import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, relative, resolve } from "node:path";
import * as ts from "typescript";

/** Follow relative static imports (including type-only and side-effect imports) and re-exports.
 *  Keep the first path to each module so a failure shows how the model was reached. */
function importClosure(entry: string): ReadonlyMap<string, readonly string[]> {
  const paths = new Map<string, readonly string[]>();
  const visit = (file: string, ancestors: readonly string[]): void => {
    if (paths.has(file)) return;
    const chain = [...ancestors, relative(import.meta.dir, file)];
    paths.set(file, chain);
    const source = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest);
    for (const statement of source.statements) {
      if (!ts.isImportDeclaration(statement) && !ts.isExportDeclaration(statement)) continue;
      const specifier = statement.moduleSpecifier;
      if (specifier === undefined || !ts.isStringLiteral(specifier)) continue;
      if (!specifier.text.startsWith("./") && !specifier.text.startsWith("../")) continue;
      const path = resolve(dirname(file), specifier.text);
      const candidates = /\.tsx?$/u.test(path)
        ? [path]
        : [`${path}.ts`, `${path}.tsx`, join(path, "index.ts"), join(path, "index.tsx")];
      const imported = candidates.find((candidate) => existsSync(candidate));
      if (imported !== undefined) visit(imported, chain);
    }
  };
  visit(entry, []);
  return paths;
}

test("a row's text imports nothing of the page's model", () => {
  const paths = importClosure(join(import.meta.dir, "RowText.tsx"));
  const leaks = ["issue-tree.ts", "tree-drafts.ts"].flatMap((file) => {
    const chain = paths.get(join(import.meta.dir, file));
    return chain === undefined ? [] : [chain.join(" -> ")];
  });
  expect(leaks).toEqual([]);
});
