/** `bun packages/dispatch/e2e/tools/today-page-modules.ts`, from any directory: prints each module
 *  that today's issue page loads, one path to a line from the repository's root, sorted and
 *  unique. Verification v4's `c-today-added-lines` of dispatch://INFRA-5939 reads it: a file it
 *  prints may change only by added lines, so that a reader who has not turned the new issue page
 *  on is served the page as it was.
 *
 * Today's page is every module `packages/dispatch/web/src/main.tsx` reaches through its static
 * imports, its re-exports (`export … from`), its `import()` calls, each `lazy` page among them,
 * its Web Workers and its style sheets' `@import`s, except the `lazy` import of `IssueOutlinePage`
 * in `web/src/features/issue-page/IssuePage.tsx`, which loads the new page and is the one import
 * the walk does not follow. A Web Worker is a `new Worker(new URL("<relative path>",
 * import.meta.url))`, or a `new SharedWorker` of the same shape, which Vite bundles as a chunk of
 * the worker's own. The walk follows the workers and the `@import`s because today's page loads
 * them: the Markdown parse worker and the Mermaid worker run on its long bodies and its diagrams,
 * and an `@import`ed sheet is bundled into the page's CSS. It goes through the whole graph, so a
 * module is printed however far from the entry it is.
 *
 * It leaves out:
 * - an import of types alone (`import type`, `export type … from`, and an `import { type A }` or
 *   an `export { type A } from` whose every name is a type), which Vite's esbuild transform drops,
 *   so it loads nothing: no `tsconfig.json` of the packages the page reaches sets
 *   `verbatimModuleSyntax`, which would keep the second two. An import whose names the module uses
 *   only as types but does not mark so is followed, though esbuild drops it as well: the list errs
 *   toward one module more, never one less.
 * - a module under `node_modules`, and a Node built-in: neither is a file of this repository.
 * - an external URL (`https://…`, `//…`) and a data URL, which Vite leaves to the browser.
 * - a style sheet's `url()`: it names a font or a picture, an asset rather than a module.
 *
 * Each import is resolved as Vite 5 resolves it (`packages/vite/src/node/plugins/resolve.ts`).
 * The first entry of `resolve.alias` in `web/vite.config.ts` whose name is the specifier, or a
 * prefix of it followed by `/`, replaces that prefix, as `@rollup/plugin-alias` does. A relative
 * or absolute path, without its query (`?url`, `?raw`, …), then names a module by the first of
 * these that is a file: the path itself; where the importer is TypeScript and the path ends in
 * `.js`, `.mjs`, `.cjs` or `.jsx`, the TypeScript file of that name (`.ts` for each, and `.tsx` too
 * for `.js`); the path with the first of Vite's extensions, `.mjs .js .mts .ts .jsx .tsx .json`;
 * and, for a directory, its `index` with the first of them. Each module is named by its real path,
 * so an alias or a link into `node_modules` lands in `node_modules`. A bare name that no alias
 * matches is a package. A worker's path is resolved as an import's. A style sheet's `@import` is
 * resolved as Vite's resolver of CSS resolves it (`createCSSResolvers`, and the one
 * `@tailwindcss/vite` builds with the same options): `.css` is its one extension, it tries no
 * `index`, and a bare name that starts with a letter, a digit or `_` is tried as a path beside the
 * sheet before it is read as a package (`preferRelative`), as `@import "tailwindcss"` is.
 *
 * The aliases are read by importing the config, as Vite loads it, so each is the value the config
 * computes: `decode-named-character-reference`'s path is worked out by `createRequire` as the
 * config loads. Loading it builds its plugins' objects and writes nothing (measured with Bun 1.4.4:
 * no file appeared under the repository or under the directory the import ran in).
 *
 * A script is parsed by the TypeScript compiler. A style sheet is read by a scan that skips its
 * comments, its strings and its unquoted `url(…)`s, and takes each `@import` written outside a
 * block: `postcss`, which Vite parses CSS with, is no dependency of this package.
 *
 * A module imported with a query is printed by its path. Its own imports are followed only under
 * `?worker` and `?sharedworker`, which bundle it as code, and `?inline`, which inlines a style
 * sheet with its `@import`s; `?url` and `?raw` load it as an address or as text, which loads
 * nothing it imports. A module that is neither a script nor a style sheet (a font, a picture, JSON)
 * is printed and not read.
 *
 * What the walk cannot read stops it with the reason rather than printing a shorter list: an
 * `import()`, a worker or an `@import` of no literal path, a worker whose path is not relative, a
 * specifier no file answers, a directory with a `package.json` (Vite reads its entry; this tool
 * does not), a workspace package that no alias names (the same), a module outside the
 * repository's root, and a left-out import the walk never met, which means the switch to the new
 * page moved and the new page's files would be printed as today's.
 */
import { existsSync, readFileSync, realpathSync, statSync } from "node:fs";
import { isBuiltin } from "node:module";
import { dirname, extname, isAbsolute, join, relative, resolve, sep } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import * as ts from "typescript";

/** The import the walk does not follow: each `import()` of `specifier`, spelled as the source
 *  spells it, in the module at `importer`. */
export type LeftOutImport = { readonly importer: string; readonly specifier: string };

/** What one walk reads. */
export type TodayPageWalk = {
  /** The module the page starts from, `web/src/main.tsx` for the app. */
  readonly entry: string;
  /** The Vite config whose `resolve.alias` each import is resolved through. */
  readonly viteConfig: string;
  /** The directory each printed path is relative to. */
  readonly repositoryRoot: string;
  readonly leftOut: LeftOutImport;
};

/** One entry of `resolve.alias`: the name an import is matched on, and what replaces it. */
type Alias = readonly [find: string, replacement: string];

/** One import a module's source writes: its specifier, its line, and whether it is `import()`. */
type ModuleImport = {
  readonly specifier: string;
  readonly line: number;
  readonly dynamic: boolean;
};

/** A module an import loads: its real path, and whether the modules it imports load with it. */
type LoadedModule = { readonly file: string; readonly followed: boolean };

/** How the imports of one kind of module are resolved: the extensions tried after the path
 *  itself, whether a directory's `index` is tried, and whether a bare name is tried as a path
 *  beside the importer before it is read as a package. */
type Resolution = {
  readonly extensions: readonly string[];
  readonly index: boolean;
  readonly preferRelative: boolean;
};

/** A script's imports and its workers, by Vite's default `resolve.extensions`. */
const SCRIPT_RESOLUTION: Resolution = {
  extensions: [".mjs", ".js", ".mts", ".ts", ".jsx", ".tsx", ".json"],
  index: true,
  preferRelative: false,
};

/** A style sheet's `@import`s, by the options of Vite's resolver of CSS. */
const STYLE_RESOLUTION: Resolution = { extensions: [".css"], index: false, preferRelative: true };

/** A module whose imports the walk reads as a script's. */
const SCRIPT = /\.[cm]?[jt]sx?$/;

/** An importer Vite reads as TypeScript (`isTsRequest`), and a path that names what TypeScript
 *  compiles to (`isPossibleTsOutput`), which Vite also tries as the TypeScript file. */
const TYPESCRIPT = /\.(?:ts|mts|cts|tsx)$/;
const COMPILED = /\.(?:js|mjs|cjs|jsx)$/;

/** A query under which the module's own imports load: one that bundles it as a Web Worker's code,
 *  and one that inlines a style sheet. */
const FOLLOWED_QUERY = /[?&](?:(?:shared)?worker|inline)(?:[&=#]|$)/;

/** Vite's external URL (`isExternalUrl`) and data URL (`isDataUrl`), neither a file. */
const EXTERNAL_URL = /^(?:https?:)?\/\//;
const DATA_URL = /^\s*data:/i;

/** Each module the page at `walk.entry` loads, relative to `walk.repositoryRoot`, `/`-joined and
 *  sorted. */
export async function todayPageModules(walk: TodayPageWalk): Promise<string[]> {
  const aliases = await aliasesOf(walk.viteConfig);
  const leftOutImporter = realpathSync(walk.leftOut.importer);
  let leftOutMet = false;
  const loaded = new Set<string>();
  const read = new Set<string>();
  const pending: LoadedModule[] = [{ file: realpathSync(walk.entry), followed: true }];
  for (let next = pending.pop(); next !== undefined; next = pending.pop()) {
    loaded.add(next.file);
    const styleSheet = next.file.endsWith(".css");
    if (!next.followed || read.has(next.file) || !(styleSheet || SCRIPT.test(next.file))) continue;
    read.add(next.file);
    const resolution = styleSheet ? STYLE_RESOLUTION : SCRIPT_RESOLUTION;
    for (const imported of styleSheet ? styleImportsOf(next.file) : scriptImportsOf(next.file)) {
      if (
        imported.dynamic &&
        next.file === leftOutImporter &&
        imported.specifier === walk.leftOut.specifier
      ) {
        leftOutMet = true;
        continue;
      }
      const found = moduleOf(imported, next.file, aliases, resolution);
      if (found !== undefined) pending.push(found);
    }
  }
  if (!leftOutMet) {
    throw new Error(
      `the walk met no import() of ${walk.leftOut.specifier} in ${walk.leftOut.importer}, the ` +
        "import it leaves out, so it would print the new page's modules as today's"
    );
  }
  const root = realpathSync(walk.repositoryRoot);
  return [...loaded]
    .map((file) => {
      const path = relative(root, file);
      if (path === ".." || path.startsWith(`..${sep}`) || isAbsolute(path)) {
        throw new Error(`${file}, which the page loads, is outside ${root}`);
      }
      return path.split(sep).join("/");
    })
    .sort();
}

/** The entries of `resolve.alias` of the config at `viteConfig`, in the order Vite tries them.
 *  The config is imported by the path the caller gives (the app's, or the test's fixture), which
 *  no static import can name. */
async function aliasesOf(viteConfig: string): Promise<Alias[]> {
  const config: { default?: { resolve?: { alias?: unknown } } } = await import(
    pathToFileURL(viteConfig).href
  );
  const alias = config.default?.resolve?.alias;
  if (alias === undefined) return [];
  if (typeof alias !== "object" || alias === null || Array.isArray(alias)) {
    throw new Error(
      `${viteConfig}: resolve.alias is not an object of names and paths, the one form this tool reads`
    );
  }
  return Object.entries(alias).map(([find, replacement]): Alias => {
    if (typeof replacement !== "string") {
      throw new Error(`${viteConfig}: the alias ${find} is not a path`);
    }
    return [find, replacement];
  });
}

/** Each import of the script at `file` that loads a module: its static imports and re-exports,
 *  but those of types alone, its `import()` calls and its workers. */
function scriptImportsOf(file: string): ModuleImport[] {
  const source = ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest);
  const imports: ModuleImport[] = [];
  const lineOf = (node: ts.Node): number =>
    source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
  const add = (node: ts.Node, specifier: ts.Expression | undefined, dynamic: boolean): void => {
    if (specifier === undefined || !ts.isStringLiteralLike(specifier)) {
      throw new Error(
        `${file}:${lineOf(node)}: an import of no literal path, which the walk cannot follow`
      );
    }
    imports.push({ specifier: specifier.text, line: lineOf(node), dynamic });
  };
  const visit = (node: ts.Node): void => {
    const worker = workerPath(node);
    if (ts.isImportDeclaration(node)) {
      if (!importsTypesAlone(node)) add(node, node.moduleSpecifier, false);
    } else if (ts.isExportDeclaration(node)) {
      const clause = node.exportClause;
      const typesAlone =
        node.isTypeOnly ||
        (clause !== undefined && ts.isNamedExports(clause) && namesTypesAlone(clause.elements));
      if (node.moduleSpecifier !== undefined && !typesAlone) add(node, node.moduleSpecifier, false);
    } else if (ts.isCallExpression(node) && node.expression.kind === ts.SyntaxKind.ImportKeyword) {
      add(node, node.arguments[0], true);
    } else if (worker !== undefined) {
      if (ts.isStringLiteralLike(worker) && !worker.text.startsWith(".")) {
        throw new Error(
          `${file}:${lineOf(node)}: a worker of ${worker.text}, which is no relative path, the ` +
            "one kind the walk resolves"
        );
      }
      add(node, worker, false);
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  return imports;
}

/** The path argument of `new URL(<path>, import.meta.url)` where `node` is a `new Worker` or a
 *  `new SharedWorker` of one, the shape Vite bundles (`workerImportMetaUrlRE`), else nothing. */
function workerPath(node: ts.Node): ts.Expression | undefined {
  if (
    !ts.isNewExpression(node) ||
    !ts.isIdentifier(node.expression) ||
    (node.expression.text !== "Worker" && node.expression.text !== "SharedWorker")
  ) {
    return undefined;
  }
  const url = node.arguments?.[0];
  if (url === undefined || !ts.isNewExpression(url) || !ts.isIdentifier(url.expression)) {
    return undefined;
  }
  const [path, base] = url.arguments ?? [];
  const importMetaUrl =
    base !== undefined &&
    ts.isPropertyAccessExpression(base) &&
    base.name.text === "url" &&
    ts.isMetaProperty(base.expression) &&
    base.expression.keywordToken === ts.SyntaxKind.ImportKeyword;
  return url.expression.text === "URL" && importMetaUrl ? path : undefined;
}

/** Whether every name of an import or export list is marked `type`. An empty list is not: it
 *  names no type, and the module it names is still loaded. */
function namesTypesAlone(elements: readonly (ts.ImportSpecifier | ts.ExportSpecifier)[]): boolean {
  return elements.length > 0 && elements.every((element) => element.isTypeOnly);
}

/** Whether an import declaration loads nothing: `import type`, or a list of names each marked
 *  `type` with no default import beside it. */
function importsTypesAlone(node: ts.ImportDeclaration): boolean {
  const clause = node.importClause;
  if (clause === undefined) return false;
  if (clause.phaseModifier === ts.SyntaxKind.TypeKeyword) return true;
  const bindings = clause.namedBindings;
  return (
    clause.name === undefined &&
    bindings !== undefined &&
    ts.isNamedImports(bindings) &&
    namesTypesAlone(bindings.elements)
  );
}

/** An unquoted `url(…)` at the scan's index, read whole, so that what it holds is read as no
 *  comment, string or brace. A quoted one is read as the string it holds. */
const UNQUOTED_URL = /(?<![\w-])url\(\s*[^\s"')][^)]*\)?/iy;

/** An `@import` at the scan's index, with the path it imports where it has one: the string, or
 *  the `url(…)`, written after it, past white space and comments. */
const STYLE_IMPORT =
  /@import(?![\w-])(?:\s|\/\*[\s\S]*?\*\/)*(?:"([^"\n]*)"|'([^'\n]*)'|url\(\s*(?:"([^"\n]*)"|'([^'\n]*)'|([^\s"')]+))\s*\))?/iy;

/** Each `@import` of the style sheet at `file` that is written outside a block, where CSS
 *  allows one. The scan skips comments, strings and unquoted `url(…)`s, so none of them is read
 *  as an `@import` or as a brace. */
function styleImportsOf(file: string): ModuleImport[] {
  const text = readFileSync(file, "utf8");
  const imports: ModuleImport[] = [];
  let depth = 0;
  let index = 0;
  while (index < text.length) {
    const char = text[index];
    STYLE_IMPORT.lastIndex = index;
    const rule = depth === 0 && char === "@" ? STYLE_IMPORT.exec(text) : null;
    UNQUOTED_URL.lastIndex = index;
    if (rule !== null) {
      const specifier = rule.slice(1).find((part) => part !== undefined);
      const line = text.slice(0, index).split("\n").length;
      if (specifier === undefined || specifier === "") {
        throw new Error(`${file}:${line}: an @import of no path the walk can read`);
      }
      imports.push({ specifier, line, dynamic: false });
      index = STYLE_IMPORT.lastIndex;
    } else if (text.startsWith("/*", index)) {
      const end = text.indexOf("*/", index + 2);
      index = end === -1 ? text.length : end + 2;
    } else if (char === '"' || char === "'") {
      index = stringEnd(text, index);
    } else if ((char === "u" || char === "U") && UNQUOTED_URL.test(text)) {
      index = UNQUOTED_URL.lastIndex;
    } else {
      if (char === "{") depth += 1;
      else if (char === "}") depth -= 1;
      index += 1;
    }
  }
  return imports;
}

/** The index just past the CSS string that opens at `start`: past its closing quote, or at the
 *  line end that cuts it short, which CSS reads as the end of a bad string. */
function stringEnd(text: string, start: number): number {
  const quote = text[start];
  let index = start + 1;
  while (index < text.length) {
    const char = text[index];
    if (char === "\\") index += 2;
    else if (char === quote) return index + 1;
    else if (char === "\n") return index;
    else index += 1;
  }
  return text.length;
}

/** The module that `imported`, written in the module at `importer`, loads, or nothing where it
 *  is a module the walk leaves out. */
function moduleOf(
  imported: ModuleImport,
  importer: string,
  aliases: readonly Alias[],
  resolution: Resolution
): LoadedModule | undefined {
  const at = `${importer}:${imported.line}`;
  const { specifier } = imported;
  const alias = aliases.find(([find]) => specifier === find || specifier.startsWith(`${find}/`));
  const id = alias === undefined ? specifier : alias[1] + specifier.slice(alias[0].length);
  if (EXTERNAL_URL.test(id) || DATA_URL.test(id)) return undefined;
  const path = id.replace(/[?#].*$/s, "");
  if (path === "") throw new Error(`${at}: an import of no path`);
  const query = id.slice(path.length);
  const pathLike = id.startsWith(".") || isAbsolute(id);
  const file =
    pathLike || (resolution.preferRelative && /^\w/.test(id))
      ? fileOf(isAbsolute(path) ? path : resolve(dirname(importer), path), importer, at, resolution)
      : undefined;
  if (file === undefined) {
    if (pathLike) throw new Error(`${at}: no file answers the import of ${specifier}`);
    checkPackage(id, importer, at);
    return undefined;
  }
  if (file.split(sep).includes("node_modules")) return undefined;
  return { file, followed: query === "" || FOLLOWED_QUERY.test(query) };
}

/** Checks that a bare `id` written in `importer` names a module the walk leaves out: a Node
 *  built-in, or a package installed under `node_modules`, found as Node finds it, in the
 *  `node_modules` of the importer's directory or of the nearest directory above it that holds
 *  the package. A workspace package that Bun links there instead is resolved by Vite through its
 *  `package.json`, which this tool does not read, so one that no alias names is an error. */
function checkPackage(id: string, importer: string, at: string): void {
  if (isBuiltin(id)) return;
  const name = id.split("/", id.startsWith("@") ? 2 : 1).join("/");
  for (let directory = dirname(importer); ; directory = dirname(directory)) {
    const installed = join(directory, "node_modules", name);
    if (existsSync(installed)) {
      const real = realpathSync(installed);
      if (real.split(sep).includes("node_modules")) return;
      throw new Error(
        `${at}: ${id} is the workspace package at ${real}, which no alias names; Vite resolves ` +
          "it through its package.json, which this tool does not read"
      );
    }
    if (dirname(directory) === directory) {
      throw new Error(`${at}: no package ${name} is installed where ${importer} can import it`);
    }
  }
}

/** The real path of the module that `path`, imported by `importer`, names, by Vite's
 *  `tryCleanFsResolve` under `resolution`, or nothing where no file answers it. */
function fileOf(
  path: string,
  importer: string,
  at: string,
  resolution: Resolution
): string | undefined {
  const entry = statSync(path, { throwIfNoEntry: false });
  if (entry?.isFile()) return realpathSync(path);
  if (statSync(dirname(path), { throwIfNoEntry: false })?.isDirectory()) {
    if (TYPESCRIPT.test(importer) && COMPILED.test(path)) {
      const extension = extname(path);
      const stem = path.slice(0, -extension.length);
      const source = [`${stem}${extension.replace("js", "ts")}`];
      if (extension === ".js") source.push(`${stem}.tsx`);
      const found = source.find(isFile);
      if (found !== undefined) return realpathSync(found);
    }
    const withExtension = resolution.extensions
      .map((extension) => `${path}${extension}`)
      .find(isFile);
    if (withExtension !== undefined) return realpathSync(withExtension);
  }
  if (resolution.index && entry?.isDirectory()) {
    if (existsSync(join(path, "package.json"))) {
      throw new Error(
        `${at}: ${path} is a directory with a package.json, whose entry Vite reads and this ` +
          "tool does not"
      );
    }
    const index = resolution.extensions
      .map((extension) => join(path, `index${extension}`))
      .find(isFile);
    if (index !== undefined) return realpathSync(index);
  }
  return undefined;
}

function isFile(path: string): boolean {
  return statSync(path, { throwIfNoEntry: false })?.isFile() === true;
}

if (import.meta.main) {
  const repositoryRoot = fileURLToPath(new URL("../../../..", import.meta.url));
  const web = join(repositoryRoot, "packages/dispatch/web");
  const modules = await todayPageModules({
    entry: join(web, "src/main.tsx"),
    viteConfig: join(web, "vite.config.ts"),
    repositoryRoot,
    leftOut: {
      importer: join(web, "src/features/issue-page/IssuePage.tsx"),
      specifier: "./IssueOutlinePage",
    },
  });
  process.stdout.write(modules.map((path) => `${path}\n`).join(""));
}
