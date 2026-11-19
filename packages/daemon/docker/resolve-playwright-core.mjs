// The plugin stage of packages/daemon/docker/worker.Dockerfile runs this with the stage's Bun,
// `bun resolve-playwright-core.mjs <package.json>`, naming packages/dispatch's package.json, to find
// the playwright-core of the Playwright that package locks. The stage's install uses Bun's isolated
// linker, so playwright-core is not hoisted at the root's node_modules. This walks the chain
// @playwright/test, playwright, playwright-core with Node's module resolution, resolving each
// package's package.json from the one before it, which answers with the real path rather than
// assuming where the package lies, and prints the directory of the playwright-core it reaches.
// Any error propagates: a package of the chain that does not resolve exits non-zero.
import { createRequire } from "node:module";
import path from "node:path";

if (process.argv.length !== 3) {
  throw new Error("usage: bun resolve-playwright-core.mjs <package.json>");
}
let found = path.resolve(process.argv[2]);
for (const name of ["@playwright/test", "playwright", "playwright-core"]) {
  found = createRequire(found).resolve(`${name}/package.json`);
}
console.log(path.dirname(found));
