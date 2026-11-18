/**
 * The cold-load lane: every module the web app loads lazily is held back each time it loads once
 * a test file's rows have begun.
 *
 * `bun run test:web:cold` is `test:web` with this file added last to its `--preload` list. It
 * registers its plugin in its `beforeAll`, not as it loads, so it needs no place of its own in
 * that list.
 *
 * The app loads some modules on first use only: each `lazy(() => import(...))` page and each other
 * `import(...)`, such as the Markdown engine's `@legion/proof-editor/headless` in
 * `features/refs/markdown-engine.ts`. `--parallel` implies `--isolate`, so a test file loads each
 * such module once: the row that runs first in the file finds it unloaded, and every later row
 * finds it loaded. A row that reads what such a module renders without waiting for it passes in
 * file order, and when random order puts it first it can still pass, since the module can load
 * within the row's wait on something else. Here each load of such a module after the file's rows
 * begin first waits `DISPATCH_COLD_HOLD_MS` milliseconds (1000 when unset), so that row fails
 * whenever it runs first, and so does every row that runs while the module is held. One second
 * keeps a chain of holds, one lazy module loading the next (the Markdown engine's module loads
 * `yjs`, which is held too), well inside the 10 s a `findBy*` waits.
 *
 * The rows begin at this file's `beforeAll`, which registers the plugin. A plugin sees only the
 * loads after it is registered, so it holds every load it sees. That `beforeAll` is registered
 * before the test file's own and so runs first: a lazy load that a file's own `beforeAll` starts
 * is held too. A module a test file imports itself, directly or through the modules it imports,
 * is loaded before its rows begin, so it is not held: no row can find it unloaded, and a hold
 * there would only slow the file. Under `--rerun-each`, Bun runs that `beforeAll` again for each
 * run of the file and so registers the plugin again, and a module first loaded in a later run is
 * still held once, not once for each registration (both measured on Bun 1.4.4).
 *
 * The list is read as this file loads, once per test file and not again for a rerun of it: every
 * `import("<literal>")` of the sources under `web/src` that are not tests, resolved from the file
 * that writes it. Only the file the specifier resolves to is held, not the rest of its package,
 * and a specifier that resolves to no JavaScript or TypeScript file, such as a style sheet, is
 * not held. The wait is a child `sleep`: a fake clock a row installs holds `setTimeout` and
 * `Bun.sleep` (Bun 1.4.4), and would hold the load with them.
 *
 * Reading a failure. A row that fails here and passes under `test:web` with the same seed reads
 * text or a part a lazily loaded module renders before it waits for it: it waits with a `findBy*`
 * query or `warmMarkdownRenderer()`, as packages/dispatch/AGENTS.md ("## Commands") says. A row
 * that does wait and still fails here ran out its wait behind a chain of holds, one lazy module
 * loading the next: a smaller `DISPATCH_COLD_HOLD_MS` tells the two apart.
 */

import { beforeAll } from "bun:test";
import { readFileSync } from "node:fs";
import { basename, dirname, extname, join } from "node:path";
import { plugin } from "bun";

/** The web app's sources, whose lazy loads this file holds. */
const SOURCES = join(import.meta.dir, "..");

/** A dynamic import of a string literal. A type's `typeof import("...")` loads nothing. */
const LAZY_IMPORT = /(?<!typeof\s+)(?<![\w$.])import\(\s*(["'])([^"']+)\1\s*\)/g;

/** The extensions of a JavaScript or TypeScript file, the only files this holds. */
const SCRIPT_EXTENSIONS: ReadonlySet<string> = new Set([
  ".js",
  ".mjs",
  ".cjs",
  ".jsx",
  ".ts",
  ".mts",
  ".cts",
  ".tsx",
]);

/** How long each load of a held module waits, from `DISPATCH_COLD_HOLD_MS`. */
function holdSeconds(text: string | undefined): string {
  if (text === undefined || text === "") return "1";
  if (!/^\d+$/.test(text)) {
    throw new Error(`DISPATCH_COLD_HOLD_MS is a whole number of milliseconds, not ${text}`);
  }
  return String(Number(text) / 1000);
}

/** Each file the web app loads lazily. */
function lazilyLoaded(): Set<string> {
  const held = new Set<string>();
  for (const relative of new Bun.Glob("**/*.{ts,tsx}").scanSync(SOURCES)) {
    if (basename(relative).includes(".test.") || relative.split("/").includes("__tests__")) {
      continue;
    }
    const file = join(SOURCES, relative);
    for (const [, , specifier] of readFileSync(file, "utf8").matchAll(LAZY_IMPORT)) {
      const path = Bun.resolveSync(specifier, dirname(file));
      if (SCRIPT_EXTENSIONS.has(extname(path))) held.add(path);
    }
  }
  if (held.size === 0) throw new Error(`no module under ${SOURCES} is loaded lazily`);
  return held;
}

const seconds = holdSeconds(process.env.DISPATCH_COLD_HOLD_MS);
const held = lazilyLoaded();
const paths = [...held].map((path) => path.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
const filter = new RegExp(`^(?:${paths.join("|")})$`);

beforeAll(() => {
  plugin({
    name: "hold lazy loads",
    setup(build) {
      build.onLoad({ filter }, async ({ path }) => {
        const status = await Bun.spawn(["sleep", seconds], { stdout: "ignore" }).exited;
        if (status !== 0) {
          throw new Error(`sleep ${seconds} exited ${status}, so ${path} was not held`);
        }
        // No `loader`, so Bun picks it from the extension as it does under `test:web` (`jsx` for
        // a `.js` file). The contents are read here, since an `onLoad` cannot hand the load back
        // to Bun: it refuses an `undefined` result ("Expected module mock to return an object").
        return { contents: readFileSync(path, "utf8") };
      });
    },
  });
});
