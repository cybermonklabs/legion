/**
 * The cold-load lane: every module the web app loads lazily is held back each time it loads.
 *
 * `bun run test:web:cold` is `test:web` with this file first in its `--preload` list, so its
 * plugin is registered before any module loads. It imports nothing that reads the DOM, so
 * `register-dom.ts`, the preload after it, still runs before every module that does.
 *
 * The app loads some modules on first use only: each `lazy(() => import(...))` page and each other
 * `import(...)`, such as the Markdown engine's `@legion/proof-editor/headless` in
 * `features/refs/markdown-engine.ts`. `--parallel` implies `--isolate`, so a test file loads each
 * such module once: the row that runs first in the file finds it unloaded, and every later row
 * finds it loaded. A row that reads what such a module renders without waiting for it passes in
 * file order and fails only when random order puts it first. Here each load of such a module
 * first waits `DISPATCH_COLD_HOLD_MS` milliseconds (1000 when unset), so that row fails in any
 * order. One second keeps a chain of holds, one lazy module loading the next (the Markdown
 * engine's module loads `yjs`, which is held too), well inside the 10 s a `findBy*` waits.
 *
 * The list is read each time this file runs, once per test file: every `import("<literal>")` of
 * the sources under `web/src` that are not tests, resolved from the file that writes it. Only the
 * file the specifier resolves to is held, not the rest of its package, and a specifier that
 * resolves to no JavaScript or TypeScript file, such as a style sheet, is not held. The wait is a
 * child `sleep`: a fake clock a row installs holds `setTimeout` and `Bun.sleep` (Bun 1.4.4), and
 * would hold the load with them.
 *
 * Reading a failure. A row that fails here and passes under `test:web` with the same seed reads
 * text or a part a lazily loaded module renders before it waits for it: it waits with a `findBy*`
 * query or `warmMarkdownRenderer()`, as packages/dispatch/AGENTS.md ("## Commands") says. A row
 * that does wait and still fails here ran out its wait behind a chain of holds, one lazy module
 * loading the next: a smaller `DISPATCH_COLD_HOLD_MS` tells the two apart.
 */

import { readFileSync } from "node:fs";
import { basename, dirname, extname, join } from "node:path";
import { type Loader, plugin } from "bun";

/** The web app's sources, whose lazy loads this file holds. */
const SOURCES = join(import.meta.dir, "..");

/** A dynamic import of a string literal. A type's `typeof import("...")` loads nothing. */
const LAZY_IMPORT = /(?<!typeof\s+)(?<![\w$.])import\(\s*(["'])([^"']+)\1\s*\)/g;

/** The loader of each extension of a JavaScript or TypeScript file. */
const LOADERS: Readonly<Record<string, Loader>> = {
  ".js": "js",
  ".mjs": "js",
  ".cjs": "js",
  ".jsx": "jsx",
  ".ts": "ts",
  ".mts": "ts",
  ".cts": "ts",
  ".tsx": "tsx",
};

/** How long each load of a held module waits, from `DISPATCH_COLD_HOLD_MS`. */
function holdSeconds(text: string | undefined): string {
  if (text === undefined || text === "") return "1";
  if (!/^\d+$/.test(text)) {
    throw new Error(`DISPATCH_COLD_HOLD_MS is a whole number of milliseconds, not ${text}`);
  }
  return String(Number(text) / 1000);
}

/** Each file the web app loads lazily, with its loader. */
function lazilyLoaded(): Map<string, Loader> {
  const held = new Map<string, Loader>();
  for (const relative of new Bun.Glob("**/*.{ts,tsx}").scanSync(SOURCES)) {
    if (basename(relative).includes(".test.") || relative.split("/").includes("__tests__")) {
      continue;
    }
    const file = join(SOURCES, relative);
    for (const [, , specifier] of readFileSync(file, "utf8").matchAll(LAZY_IMPORT)) {
      const path = Bun.resolveSync(specifier, dirname(file));
      const loader = LOADERS[extname(path)];
      if (loader !== undefined) held.set(path, loader);
    }
  }
  if (held.size === 0) throw new Error(`no module under ${SOURCES} is loaded lazily`);
  return held;
}

const seconds = holdSeconds(process.env.DISPATCH_COLD_HOLD_MS);
const held = lazilyLoaded();
const paths = [...held.keys()].map((path) => path.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));

plugin({
  name: "hold lazy loads",
  setup(build) {
    build.onLoad({ filter: new RegExp(`^(?:${paths.join("|")})$`) }, async ({ path }) => {
      const status = await Bun.spawn(["sleep", seconds], { stdout: "ignore" }).exited;
      if (status !== 0) {
        throw new Error(`sleep ${seconds} exited ${status}, so ${path} was not held`);
      }
      return { contents: readFileSync(path, "utf8"), loader: held.get(path) };
    });
  },
});
