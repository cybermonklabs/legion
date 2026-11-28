import { expect, test } from "bun:test";
import { readFileSync, statSync } from "node:fs";
import { dirname, join, relative, resolve, sep } from "node:path";

/**
 * Today's pages load none of the new issue page's model or its words (dispatch://INFRA-1196
 * decisions 31 and 512): the entry, `main.tsx`, and every module the build puts in its chunk
 * import no file of the model, whose words would ship in that chunk. The test walks the entry's
 * static import closure as the build keeps it and names the path of imports from `main.tsx` to
 * each model file it reaches. A file that imports the model pulls it in, so reaching such a file
 * reaches a model file, whose path the walk names; the rest of `features/issue-page/` - the frame's
 * small files `app.tsx` imports, `decision-review.ts`, `IssuePagePreference.tsx` - is no part of
 * the model, and today's pages import it on purpose.
 *
 * An edge counts exactly where the build keeps it. The build (`vite build`) transforms each file
 * with esbuild under `packages/dispatch/tsconfig.json`, which sets neither `verbatimModuleSyntax`
 * nor `preserveValueImports`, so esbuild erases an `import type`, a `type` specifier, and an import
 * whose bindings the file uses only as types or not at all, as TypeScript does. Bun's transpiler
 * with `trimUnusedImports` erases the same imports before `scan` reads them, so the walk reads
 * what the transform leaves: static imports, side-effect imports (`import "x"`) and re-exports
 * (`export … from`), and never a dynamic `import()`, which `app.tsx` loads each page through. Its
 * JSX is read under the classic runtime, which changes no import of a file of `web/src`: the
 * automatic runtime adds only `react/jsx-runtime`, a package, and Bun refuses to scan a file that
 * puts `key` after a spread under it.
 *
 * A relative specifier resolves in Vite's order: the path itself, then `.ts`, `.tsx`,
 * `index.ts` and `index.tsx` (`web/src` holds no `.js`, `.mjs` or `.mts` file Vite would try
 * first). A package, an alias of `web/vite.config.ts` (each names a file of another package), a
 * stylesheet, a JSON file, and any file outside `web/src`, ends the walk on that edge.
 */

/** The web app's sources. */
const SOURCES = resolve(import.meta.dir, "..");

/** The new issue page's model and its words, under `web/src`. */
const MODEL_FILES = [
  "features/issue-page/page-words.ts",
  "features/issue-page/page-structure.ts",
  "features/issue-page/tree-drafts.ts",
  "features/issue-page/issue-tree.ts",
] as const;

const TS = new Bun.Transpiler({ loader: "ts", trimUnusedImports: true });
const TSX = new Bun.Transpiler({
  loader: "tsx",
  trimUnusedImports: true,
  tsconfig: JSON.stringify({ compilerOptions: { jsx: "react" } }),
});

/** The imports of `file` that its transform leaves, each with its kind. */
function scannedImports(file: string): readonly Bun.Import[] {
  return (file.endsWith(".tsx") ? TSX : TS).scan(readFileSync(file, "utf8")).imports;
}

/** The TypeScript file of `web/src` that `specifier`, written in `from`, names, or undefined where
 *  the edge leaves the walk. */
function resolveImport(from: string, specifier: string): string | undefined {
  if (!specifier.startsWith(".")) return undefined;
  const base = resolve(dirname(from), specifier);
  const found = [
    base,
    `${base}.ts`,
    `${base}.tsx`,
    join(base, "index.ts"),
    join(base, "index.tsx"),
  ].find((candidate) => statSync(candidate, { throwIfNoEntry: false })?.isFile() === true);
  return found?.startsWith(`${SOURCES}${sep}`) && /\.tsx?$/.test(found) ? found : undefined;
}

/** The walks made so far, by the file each started from: two tests walk from `main.tsx`. */
const closures = new Map<string, ReadonlyMap<string, string | null>>();

/** Each file the walk from `start` (under `web/src`) reaches, to the file whose import reached it
 *  first, breadth first, so the path to each is a shortest one. */
function closure(start: string): ReadonlyMap<string, string | null> {
  const walked = closures.get(start);
  if (walked !== undefined) return walked;
  const first = join(SOURCES, start);
  const reachedFrom = new Map<string, string | null>([[first, null]]);
  closures.set(start, reachedFrom);
  const queue = [first];
  for (let file = queue.shift(); file !== undefined; file = queue.shift()) {
    for (const { kind, path } of scannedImports(file)) {
      if (kind !== "import-statement") continue;
      const next = resolveImport(file, path);
      if (next === undefined || reachedFrom.has(next)) continue;
      reachedFrom.set(next, file);
      queue.push(next);
    }
  }
  return reachedFrom;
}

/** The path of imports to `file` (under `web/src`) in `reachedFrom`, each hop by its path under
 *  `web/src`, or undefined where the walk did not reach it. */
function pathTo(reachedFrom: ReadonlyMap<string, string | null>, file: string): string | undefined {
  const target = join(SOURCES, file);
  if (!reachedFrom.has(target)) return undefined;
  const hops: string[] = [];
  for (let at: string | null | undefined = target; at; at = reachedFrom.get(at)) {
    hops.unshift(relative(SOURCES, at));
  }
  return hops.join(" -> ");
}

/** The path to each model file the walk from `start` reaches. */
function modelPaths(start: string): string[] {
  const reachedFrom = closure(start);
  return MODEL_FILES.flatMap((file) => pathTo(reachedFrom, file) ?? []);
}

test("the entry's static imports reach no file of the new issue page's model", () => {
  expect(
    modelPaths("main.tsx"),
    "each path of imports from main.tsx to a model file, which today's pages would load"
  ).toEqual([]);
});

// Today's issue page is a chunk of its own, which `app.tsx` loads through a dynamic import and the
// walk from the entry does not follow (the case below), so the same rule is read from its own
// module: it loads none of the model either, the margin and the reading pane it draws included.
test("today's issue page's static imports reach no file of the new issue page's model", () => {
  expect(
    modelPaths("features/issue/IssuePage.tsx"),
    "each path of imports from today's issue page to a model file, which its chunk would load"
  ).toEqual([]);
});

// The control of the walk: from the tree's hook it reaches the model, and names the path.
test("the walk from the tree's hook reaches the model and names the path to it", () => {
  expect(modelPaths("features/issue-page/useIssueTree.ts")).toContain(
    "features/issue-page/useIssueTree.ts -> features/issue-page/tree-drafts.ts"
  );
});

// The control of the dynamic edge: `app.tsx` loads the issue page through a dynamic import, which
// the walk does not follow, and no static edge from the entry reaches it.
test("the walk does not follow app.tsx's dynamic import of the issue page", () => {
  const app = join(SOURCES, "app.tsx");
  const lazy = scannedImports(app)
    .filter(({ kind }) => kind === "dynamic-import")
    .map(({ path }) => resolveImport(app, path));
  expect(lazy).toContain(join(SOURCES, "features/issue-page/IssuePage.tsx"));
  expect(pathTo(closure("main.tsx"), "features/issue-page/IssuePage.tsx")).toBeUndefined();
});

// The control of the erased edge: `decision-review.ts`, which today's Decisions tab and margin
// import, imports a type of `issue-tree.ts` alone, which the build erases, and so does the walk.
test("the walk erases an import of types alone, as the build does", () => {
  const review = join(SOURCES, "features/issue-page/decision-review.ts");
  expect(readFileSync(review, "utf8")).toContain(
    'import type { DocumentBlocks } from "./issue-tree";'
  );
  expect(scannedImports(review).map(({ path }) => path)).not.toContain("./issue-tree");
});
