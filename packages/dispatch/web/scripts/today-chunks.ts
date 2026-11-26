/**
 * c-chunk of dispatch://INFRA-5702 (R14 and R22, amending dispatch://INFRA-3946 R12): which
 * chunks today's pages fetch, which chunks Dashboard fetches, and which of the new page's modules
 * each holds.
 *
 * `bun web/scripts/today-chunks.ts <outDir>` reads a build made with `--manifest --sourcemap`, its
 * `<outDir>` read as Vite reads `--outDir`: a relative path is below `web/`, Vite's root. It reads
 * `<outDir>/.vite/manifest.json` for each chunk's static and dynamic imports, and each chunk's
 * source map (`<chunk>.js.map`) for the modules the chunk holds. It reads the maps, not strings in
 * the chunks, because a minified chunk keeps no module's name, while its map's `sources` list
 * every module whose code the chunk holds (a module tree-shaken away is left out). It finds the
 * chunks by the manifest's `src` keys and `isEntry`/`isDynamicEntry`, never by a hashed file name.
 *
 * It prints two sets, each chunk with its size in bytes:
 *
 * (1) Today's pages: every chunk the entry chunk reaches through its static and its dynamic
 *     imports, transitively, following no import into the new page's chunk (`IssueOutlinePage`)
 *     or the Dashboard chunk (`DashboardTab`). No chunk of this set holds the new page's word list
 *     (`page-words.ts` and its `page-words-*.ts` part files), `page-structure.ts`, `RowText.tsx`,
 *     the new page's own rules (`waiting.ts`, `decision-review.ts`, which the Dashboard chunk
 *     reads, and `verification-filter/statement-state.ts`) or the model (`tree-drafts.ts`,
 *     `issue-tree.ts`, `useIssueTree.ts`). The rules Dashboard shares with today's pages live in
 *     `statement-rules.ts`, `first-line.ts` and `target-naming.ts`, which import no model module,
 *     so this set may hold them.
 * (2) The Dashboard chunk: `DashboardTab` with every chunk it imports statically, transitively.
 *     This set holds `page-words.ts` and `RowText.tsx`, and holds no `issue-tree.ts`,
 *     `useIssueTree.ts` or `tree-drafts.ts`.
 *
 * (3) Neither set holds a module under `features/issue-page/threads/`, Threads' own folder
 *     (dispatch://INFRA-5704/spec#b-r-today-unchanged). This rule holds in every run.
 *
 * `bun web/scripts/today-chunks.ts --modules <outDir>` (c-today-files of dispatch://INFRA-5704)
 * prints the same two sets, and under each chunk every module its source map lists, by its path
 * below the repository's root where it is under that root, as `git diff --numstat` names a file.
 * A run with no option prints what it printed before the option was added.
 *
 * It exits 1 naming the module and the chunk where a rule breaks.
 */
import { readFileSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";

/** Vite's root, which a relative `--outDir` is below. */
const WEB_DIR = resolve(import.meta.dir, "..");

/** Vite's root is `web/`, so a manifest's `src` is a path below `web/`. */
const NEW_PAGE_SRC = "src/features/issue-page/IssueOutlinePage.tsx";
const DASHBOARD_SRC = "src/features/dashboard/DashboardTab.tsx";

/** Where a module of the new page sits in a source path a map resolves to. */
const ISSUE_PAGE_DIR = "/web/src/features/issue-page/";

/** Rule (1): the modules of `features/issue-page/` that no chunk of today's pages holds. */
const NOT_ON_TODAY: readonly string[] = [
  "page-words.ts",
  "page-structure.ts",
  "RowText.tsx",
  "waiting.ts",
  "decision-review.ts",
  "tree-drafts.ts",
  "issue-tree.ts",
  "useIssueTree.ts",
  "verification-filter/statement-state.ts",
];
/** Rule (1): a part file of the new page's word list. */
const PAGE_WORDS_PART = /^page-words-[^/]+\.ts$/;

/** Rule (2): the modules of `features/issue-page/` the Dashboard chunk's set holds, and never. */
const ON_DASHBOARD: readonly string[] = ["page-words.ts", "RowText.tsx"];
const NOT_ON_DASHBOARD: readonly string[] = ["issue-tree.ts", "useIssueTree.ts", "tree-drafts.ts"];

/** Rule (3): the folder of Threads' own modules, below `features/issue-page/`, which no chunk of
 *  either set holds. */
const THREADS_DIR = "threads/";

/** The option, before the out dir, that prints under each chunk every module its map lists. */
const MODULES_OPTION = "--modules";

/** Whether this run prints each chunk's modules: the script run with `MODULES_OPTION` first. */
const LISTS_MODULES = import.meta.main && process.argv[2] === MODULES_OPTION;

/** The repository's root, which a module is printed below: `web/` is `packages/dispatch/web/`. */
const REPO_DIR = resolve(WEB_DIR, "..", "..", "..");

/** The fields of a manifest chunk this script reads. */
interface ManifestChunk {
  file: string;
  src: string | null;
  isEntry: boolean;
  isDynamicEntry: boolean;
  imports: string[];
  dynamicImports: string[];
}

interface Chunk {
  /** The manifest's key. */
  key: string;
  file: string;
  src: string | null;
  bytes: number;
  /** Each module of `features/issue-page/` the chunk holds, as its path below that directory. */
  issuePageModules: string[];
  /** Each module the chunk's source map lists, in its order, as `repoPath` prints it. */
  modules: string[];
}

function isStringArray(value: unknown): value is string[] {
  return Array.isArray(value) && value.every((item) => typeof item === "string");
}

function readJson(path: string): unknown {
  let text: string;
  try {
    text = readFileSync(path, "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error(
        `${path} does not exist: build with --manifest --sourcemap into this out dir`
      );
    }
    throw error;
  }
  return JSON.parse(text);
}

function readManifest(outDir: string): Map<string, ManifestChunk> {
  const path = join(outDir, ".vite", "manifest.json");
  const manifest = readJson(path);
  if (typeof manifest !== "object" || manifest === null) {
    throw new Error(`${path} is not a manifest of chunks`);
  }
  const chunks = new Map<string, ManifestChunk>();
  for (const [key, entry] of Object.entries(manifest)) {
    if (typeof entry !== "object" || entry === null || !("file" in entry)) {
      throw new Error(`${path}: ${key} names no file`);
    }
    const { file } = entry;
    if (typeof file !== "string") throw new Error(`${path}: ${key} names no file`);
    const imports = "imports" in entry ? entry.imports : [];
    const dynamicImports = "dynamicImports" in entry ? entry.dynamicImports : [];
    if (!isStringArray(imports) || !isStringArray(dynamicImports)) {
      throw new Error(`${path}: the imports of ${key} are not a list of chunks`);
    }
    chunks.set(key, {
      file,
      src: "src" in entry && typeof entry.src === "string" ? entry.src : null,
      isEntry: "isEntry" in entry && entry.isEntry === true,
      isDynamicEntry: "isDynamicEntry" in entry && entry.isDynamicEntry === true,
      imports,
      dynamicImports,
    });
  }
  return chunks;
}

/** Each source of the map beside `file`, resolved to an absolute path. */
function mapSources(outDir: string, file: string): string[] {
  const path = join(outDir, `${file}.map`);
  const map = readJson(path);
  if (typeof map !== "object" || map === null || !("sources" in map)) {
    throw new Error(`${path} lists no sources`);
  }
  const { sources } = map;
  if (!isStringArray(sources)) throw new Error(`${path} lists no sources`);
  const sourceRoot =
    "sourceRoot" in map && typeof map.sourceRoot === "string" ? map.sourceRoot : "";
  return sources.map((source) => resolve(dirname(path), sourceRoot, source));
}

/** The key of the dynamic entry whose `src` is `src`, or null where the build has none. */
function dynamicEntryKey(manifest: Map<string, ManifestChunk>, src: string): string | null {
  for (const [key, entry] of manifest) {
    if (entry.src === src && entry.isDynamicEntry) return key;
  }
  return null;
}

/** `roots` and every chunk they reach through `follow`, never entering a key of `stopAt`. */
function closure(
  manifest: Map<string, ManifestChunk>,
  roots: string[],
  follow: (entry: ManifestChunk) => string[],
  stopAt: ReadonlySet<string>
): string[] {
  const reached = new Set<string>();
  const queue = [...roots];
  for (let key = queue.pop(); key !== undefined; key = queue.pop()) {
    if (reached.has(key) || stopAt.has(key)) continue;
    const entry = manifest.get(key);
    if (entry === undefined) throw new Error(`the manifest imports ${key}, which it does not hold`);
    reached.add(key);
    queue.push(...follow(entry));
  }
  return [...reached].sort();
}

function readChunk(outDir: string, manifest: Map<string, ManifestChunk>, key: string): Chunk {
  const entry = manifest.get(key);
  if (entry === undefined) throw new Error(`the manifest holds no ${key}`);
  const issuePageModules: string[] = [];
  const modules: string[] = [];
  for (const source of mapSources(outDir, entry.file)) {
    modules.push(repoPath(source));
    const at = source.lastIndexOf(ISSUE_PAGE_DIR);
    if (at !== -1) issuePageModules.push(source.slice(at + ISSUE_PAGE_DIR.length));
  }
  return {
    key,
    file: entry.file,
    src: entry.src,
    bytes: statSync(join(outDir, entry.file)).size,
    issuePageModules,
    modules,
  };
}

function chunkName(chunk: Chunk): string {
  return chunk.src === null ? chunk.file : `${chunk.file} (${chunk.src})`;
}

/** `source` below the repository's root where it is under that root, and as the map resolves it
 *  otherwise. */
function repoPath(source: string): string {
  return source.startsWith(`${REPO_DIR}/`) ? source.slice(REPO_DIR.length + 1) : source;
}

function printSet(title: string, chunks: Chunk[]): void {
  const total = chunks.reduce((sum, chunk) => sum + chunk.bytes, 0);
  console.log(`${title}: ${chunks.length} chunks, ${total} bytes`);
  const width = String(Math.max(0, ...chunks.map((chunk) => chunk.bytes))).length;
  for (const chunk of chunks) {
    console.log(`  ${String(chunk.bytes).padStart(width)}  ${chunkName(chunk)}`);
    if (LISTS_MODULES) {
      for (const module of chunk.modules) console.log(`  ${" ".repeat(width)}    ${module}`);
    }
  }
}

function main(outDir: string): number {
  const manifest = readManifest(outDir);
  const breaks: string[] = [];

  const entries = [...manifest].filter(([, entry]) => entry.isEntry).map(([key]) => key);
  if (entries.length === 0) breaks.push("the manifest names no entry chunk");
  const newPage = dynamicEntryKey(manifest, NEW_PAGE_SRC);
  if (newPage === null) breaks.push(`no dynamic entry chunk has the src ${NEW_PAGE_SRC}`);
  const dashboard = dynamicEntryKey(manifest, DASHBOARD_SRC);
  if (dashboard === null) breaks.push(`no dynamic entry chunk has the src ${DASHBOARD_SRC}`);

  const lazyPages = new Set([newPage, dashboard].filter((key) => key !== null));
  // A chunk both sets reach is read once.
  const read = new Map<string, Chunk>();
  const chunkOf = (key: string): Chunk => {
    const known = read.get(key);
    if (known !== undefined) return known;
    const chunk = readChunk(outDir, manifest, key);
    read.set(key, chunk);
    return chunk;
  };
  const today = closure(
    manifest,
    entries,
    (entry) => [...entry.imports, ...entry.dynamicImports],
    lazyPages
  ).map(chunkOf);
  const dashboardSet = (
    dashboard === null ? [] : closure(manifest, [dashboard], (entry) => entry.imports, new Set())
  ).map(chunkOf);

  printSet("(1) Today's pages", today);
  printSet("(2) The Dashboard chunk", dashboardSet);

  for (const chunk of today) {
    for (const module of chunk.issuePageModules) {
      if (NOT_ON_TODAY.includes(module) || PAGE_WORDS_PART.test(module)) {
        breaks.push(
          `features/issue-page/${module} is in ${chunkName(chunk)}, a chunk of today's pages (1)`
        );
      }
    }
  }
  if (dashboard !== null) {
    for (const chunk of dashboardSet) {
      for (const module of chunk.issuePageModules) {
        if (NOT_ON_DASHBOARD.includes(module)) {
          breaks.push(
            `features/issue-page/${module} is in ${chunkName(chunk)}, a chunk of the Dashboard chunk's set (2)`
          );
        }
      }
    }
    const held = new Set(dashboardSet.flatMap((chunk) => chunk.issuePageModules));
    const dashboardChunk = dashboardSet.find((chunk) => chunk.key === dashboard);
    for (const module of ON_DASHBOARD.filter((name) => !held.has(name))) {
      breaks.push(
        `features/issue-page/${module} is in no chunk of the Dashboard chunk's set (2), which starts at ${
          dashboardChunk === undefined ? dashboard : chunkName(dashboardChunk)
        }`
      );
    }
  }

  // Rule (3): no chunk of either set holds a module of Threads' own folder.
  for (const [set, chunks] of [
    ["a chunk of today's pages (1)", today],
    ["a chunk of the Dashboard chunk's set (2)", dashboardSet],
  ] as const) {
    for (const chunk of chunks) {
      for (const module of chunk.issuePageModules) {
        if (module.startsWith(THREADS_DIR)) {
          breaks.push(`features/issue-page/${module} is in ${chunkName(chunk)}, ${set}`);
        }
      }
    }
  }

  for (const line of breaks) console.error(`today-chunks: ${line}`);
  return breaks.length === 0 ? 0 : 1;
}

if (import.meta.main) {
  // The option goes before the out dir, so it is taken off the arguments here and the out dir is
  // read as a run with no option reads it.
  if (LISTS_MODULES) process.argv.splice(2, 1);
  const outDir = process.argv[2];
  if (outDir === undefined) {
    console.error(
      "usage: bun web/scripts/today-chunks.ts <outDir of a --manifest --sourcemap build>\n" +
        "  a relative outDir is below web/, as Vite reads --outDir"
    );
    process.exit(2);
  }
  process.exit(main(resolve(WEB_DIR, outDir)));
}
