/** The walk of `e2e/tools/today-page-modules.ts`, driven on the tree under
 *  `fixtures/today-page-modules/` rather than on the web app, so that each kind of import the walk
 *  keeps or leaves out is held by a file of its own, and a change of the app moves nothing here.
 *
 * The tree is laid out as the app is: `web/vite.config.ts` with two aliases, and `web/src/main.tsx`
 * as the entry. Each file it holds, and why the walk prints it or not:
 *
 * - `web/src/main.tsx`, the entry, printed. It imports `react` and `react-dom/client`, packages of
 *   `node_modules`, which the walk leaves out, and `./shapes` with `import type`, which loads
 *   nothing.
 * - `web/src/parse.worker.ts`, printed, which `main.tsx` starts with
 *   `new Worker(new URL("./parse.worker.ts", import.meta.url))`, as the app starts the Markdown
 *   parse worker.
 * - `web/src/app.tsx`, which `main.tsx` imports statically, printed. It imports `./shapes` with
 *   `import { type Shape, type ShapeName }`, whose every name is a type, so that import loads
 *   nothing either.
 * - `web/src/styles.css`, which `main.tsx` imports for its side effect, printed: the page loads it.
 *   Its `@import "tailwindcss"` names a package of `node_modules`, left out, and a comment that
 *   spells an `@import` of a file the tree does not hold imports nothing.
 * - `web/src/base.css`, printed, which only the `@import "./base.css"` of `styles.css` reaches.
 * - `web/src/features/issue-page/IssuePage.tsx`, printed, which only a re-export of `app.tsx`
 *   (`export { IssuePage } from …`) reaches.
 * - `editor/src/dashboard.tsx`, printed, which `app.tsx` loads with `lazy(() => import(…))` through
 *   the alias `@fixture/editor`, as the app loads `@legion/proof-editor` with `import()`. The alias
 *   names a directory, so the module is found by Vite's extension order (`.tsx`).
 * - `contracts/src/index.ts`, printed, which `IssuePage.tsx` imports through the alias
 *   `@fixture/contracts`. The alias names a directory, so the module is its `index.*`.
 * - `web/src/features/issue-page/IssueOutlinePage.tsx`, not printed: only the `lazy` import of
 *   `IssueOutlinePage` in `IssuePage.tsx` reaches it, which is the import the walk is told to leave
 *   out, as the app's new page is left out of today's.
 * - `web/src/shapes.ts`, not printed: only the two imports of types alone reach it.
 */

import { expect, test } from "bun:test";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { todayPageModules } from "../tools/today-page-modules";

const fixture = fileURLToPath(new URL("fixtures/today-page-modules", import.meta.url));

test("the walk lists every module main.tsx reaches and leaves out the lazy new page", async () => {
  const modules = await todayPageModules({
    entry: join(fixture, "web/src/main.tsx"),
    viteConfig: join(fixture, "web/vite.config.ts"),
    repositoryRoot: fixture,
    leftOut: {
      importer: join(fixture, "web/src/features/issue-page/IssuePage.tsx"),
      specifier: "./IssueOutlinePage",
    },
  });
  expect(modules).toEqual([
    "contracts/src/index.ts",
    "editor/src/dashboard.tsx",
    "web/src/app.tsx",
    "web/src/base.css",
    "web/src/features/issue-page/IssuePage.tsx",
    "web/src/main.tsx",
    "web/src/parse.worker.ts",
    "web/src/styles.css",
  ]);
});
