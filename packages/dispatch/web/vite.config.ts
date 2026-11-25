import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";
import {
  type GeneratedCode,
  packageRoot,
  thirdPartyNotices,
  viteBundleInputs,
  viteRuntimeGeneratedCode,
} from "../../../scripts/third-party-notices";

/**
 * `decode-named-character-reference`'s DOM-free implementation, which micromark decodes every
 * named character reference through in every bundle this config builds. The package's `browser`
 * export condition is a module whose body is `document.createElement('i')`, and Vite resolves a
 * Web Worker's bundle with the client's conditions, so the parse worker
 * (features/refs/markdown-parse.worker.ts) would throw before its first message. Its default
 * condition answers from the `character-entities` table instead - the table Bun, Node, the unit
 * suites and the pmdoc fixture generator already run - so a body reads the same above and below
 * the parse worker's threshold, and the same as the generator that pins Go's fixtures
 * (sjawhar/legion#2519), for 11.6 KB gzipped on the page's bundle.
 *
 * The package publishes no `./index.js` subpath, so the file is resolved rather than named: from
 * the editor, which `micromark-core-commonmark` is a dependency of, and from that package, which
 * decodes references with it. `require`'s own conditions hold no `browser`, so this is `index.js`.
 */
const proofEditor = createRequire(
  fileURLToPath(new URL("../../proof-editor/src/lib-headless.ts", import.meta.url))
);
const characterReferenceDecoder = createRequire(
  proofEditor.resolve("micromark-core-commonmark")
).resolve("decode-named-character-reference");
const vite = packageRoot("vite", fileURLToPath(new URL(".", import.meta.url)));
/**
 * The code the build emits from no source file it records: Vite's own helpers (module preload,
 * and the CommonJS interop of the @rollup/plugin-commonjs build Vite 5 carries inside it), and
 * the wrappers that interop puts around a CommonJS module, whose own file the build records.
 */
const GENERATED_CODE: GeneratedCode[] = [
  viteRuntimeGeneratedCode(vite),
  { id: /^\0(commonjsHelpers\.js|commonjs-dynamic-modules)$/, packages: [vite] },
  { id: /^\0\/.*\?commonjs-(module|exports)$/, packages: [] },
];

/**
 * Writes dist/THIRD_PARTY_NOTICES.txt, which Dispatch serves at /THIRD_PARTY_NOTICES.txt as text:
 * the license of every third-party package the build copies into dist.
 *
 * A Web Worker (`new Worker(new URL(…), …)`) is a bundle of its own, built while the page's build
 * transforms the module that creates it, so before the page's bundle is generated; the page's
 * build then copies the worker's files in as assets that name no source. So `worker`, the plugin
 * of each worker bundle, records that bundle's files and inputs, and `page` leaves those files
 * out of the page's bundle and adds those inputs to its own.
 */
function thirdPartyNoticesFile(): { page: Plugin; worker: () => Plugin } {
  let root = "";
  const workerFiles = new Set<string>();
  const workerInputs = new Set<string>();
  return {
    page: {
      name: "third-party-notices",
      apply: "build",
      configResolved(config) {
        root = config.root;
      },
      async generateBundle(_options, bundle) {
        const pageBundle = Object.fromEntries(
          Object.entries(bundle).filter(([fileName]) => !workerFiles.has(fileName))
        );
        const inputs = await viteBundleInputs(this, pageBundle, root, GENERATED_CODE);
        for (const input of workerInputs) inputs.add(input);
        this.emitFile({
          type: "asset",
          fileName: "THIRD_PARTY_NOTICES.txt",
          source: await thirdPartyNotices(inputs, root),
        });
      },
    },
    worker: () => ({
      name: "third-party-notices-worker",
      apply: "build",
      async generateBundle(_options, bundle) {
        for (const fileName of Object.keys(bundle)) workerFiles.add(fileName);
        for (const input of await viteBundleInputs(this, bundle, root, GENERATED_CODE)) {
          workerInputs.add(input);
        }
      },
    }),
  };
}

const notices = thirdPartyNoticesFile();

export default defineConfig({
  root: "web",
  plugins: [react(), tailwindcss(), notices.page],
  // The Mermaid worker (features/refs/mermaid-render.worker.ts) is created with
  // `{ type: "module" }`, so it is bundled as an ES module.
  worker: { format: "es", plugins: () => [notices.worker()] },
  define: {
    // Keep the entry chunk distinct across deployments, including no-source-change image rebuilds.
    // DISPATCH_BUILD wins over GITHUB_SHA, which CI sets on every step, so one job can build two
    // distinct dashboards of one commit (`bun run e2e:multiprocess`'s second build).
    __DISPATCH_BUILD__: JSON.stringify(
      process.env.DISPATCH_BUILD ?? process.env.GITHUB_SHA ?? Date.now().toString()
    ),
  },
  resolve: {
    // The source-only editor and Dispatch both reach this constructor. Historical-version
    // rendering passes a Proof document to Dispatch's DOMSerializer, so Vite must emit one
    // module identity rather than one per workspace import path.
    dedupe: ["prosemirror-model"],
    alias: {
      "decode-named-character-reference": characterReferenceDecoder,
      // Every subpath of `@legion/contracts` this tree reads, named before the package itself:
      // Vite's alias matches a prefix, so `@legion/contracts` alone would resolve every subpath
      // to a path inside `index.ts`. The subpaths are what keeps the barrel out of this bundle
      // - one import of it reaches every module of the package, and rollup may drop none whose
      // top level builds anything, which is how the whole `DISPATCH_TEXT_REFERENCES` table and
      // zod's schema modules were in the entry chunk of a page that reads neither
      // (dispatch://INFRA-1109, R13). Each is published by that package's own `exports`, which
      // is what Bun, tsc and every other consumer read. Three keys are read by no shipped module
      // of this bundle today - `dispatch-api` by nothing in the repository,
      // `dispatch-text-references` by three Bun tests, which resolve through `exports` and never
      // see this config, and `dispatch-tools` by a type import alone (`theme/classes.ts`, which
      // the build erases) and by browser rows, which Playwright resolves through `exports` too -
      // and the three are kept so that a value import of one resolves to its module rather than
      // being rewritten into `index.ts` by the bare key below it.
      "@legion/contracts/actor-label": fileURLToPath(
        new URL("../../contracts/src/actor-label.ts", import.meta.url)
      ),
      "@legion/contracts/agent-stream": fileURLToPath(
        new URL("../../contracts/src/agent-stream.ts", import.meta.url)
      ),
      "@legion/contracts/claim-holds": fileURLToPath(
        new URL("../../contracts/src/claim-holds.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-api": fileURLToPath(
        new URL("../../contracts/src/dispatch-api.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-api-constants": fileURLToPath(
        new URL("../../contracts/src/dispatch-api-constants.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-api-graph": fileURLToPath(
        new URL("../../contracts/src/dispatch-api-graph.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-block-ref": fileURLToPath(
        new URL("../../contracts/src/dispatch-block-ref.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-href": fileURLToPath(
        new URL("../../contracts/src/dispatch-href.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-text-references": fileURLToPath(
        new URL("../../contracts/src/dispatch-text-references.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-text-rules": fileURLToPath(
        new URL("../../contracts/src/dispatch-text-rules.ts", import.meta.url)
      ),
      "@legion/contracts/legion-roles": fileURLToPath(
        new URL("../../contracts/src/legion-roles.ts", import.meta.url)
      ),
      "@legion/contracts/related-forms": fileURLToPath(
        new URL("../../contracts/src/related-forms.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-snippet": fileURLToPath(
        new URL("../../contracts/src/dispatch-snippet.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-tools": fileURLToPath(
        new URL("../../contracts/src/dispatch-tools.ts", import.meta.url)
      ),
      "@legion/contracts/dispatch-tools-constants": fileURLToPath(
        new URL("../../contracts/src/dispatch-tools-constants.ts", import.meta.url)
      ),
      "@legion/contracts": fileURLToPath(new URL("../../contracts/src/index.ts", import.meta.url)),
      "@legion/proof-editor/document-json": fileURLToPath(
        new URL("../../proof-editor/src/document-json.ts", import.meta.url)
      ),
      "@legion/proof-editor/headless": fileURLToPath(
        new URL("../../proof-editor/src/lib-headless.ts", import.meta.url)
      ),
      "@legion/proof-editor/mermaid": fileURLToPath(
        new URL("../../proof-editor/src/mermaid-svg.ts", import.meta.url)
      ),
      "@legion/proof-editor/mermaid-render": fileURLToPath(
        new URL("../../proof-editor/src/mermaid-render.ts", import.meta.url)
      ),
      "@legion/proof-editor/style.css": fileURLToPath(
        new URL("../../proof-editor/src/lib.css", import.meta.url)
      ),
      "@legion/proof-editor/block-nesting": fileURLToPath(
        new URL("../../proof-editor/src/block-nesting.ts", import.meta.url)
      ),
      "@legion/proof-editor": fileURLToPath(
        new URL("../../proof-editor/src/lib.ts", import.meta.url)
      ),
      // The upstream package declares no main, no module, no types, no exports and no files,
      // so it publishes no entry point at all and the editor's upstream imports reach its
      // src/ by path. Same mapping as packages/proof-editor/tsconfig.json, which Bun and tsc read.
      "proof-sdk-upstream/src": fileURLToPath(
        new URL("../../proof-editor/node_modules/proof-sdk-upstream/src", import.meta.url)
      ),
    },
  },
  build: {
    outDir: "dist",
    // The entry - the one `assets/index-*.js` that dist/index.html names - read 499,485 bytes,
    // measured with `bun run build:web` and a 39-character `DISPATCH_BUILD` at `4935c4af` (the
    // same at the merges 4548096d and e15ee5e4) on 2026-11-23; the build id is in the entry, so
    // CI's 40-character commit writes one byte more. On that day the default branch's own build
    // at `d795b25c` writes 1,144,188, before dispatch://INFRA-1109
    // split the shell: the Inbox and its peek, the ask card, the search bar, the Create issue
    // dialog, the shortcut sheet, the reference-preview layer, the event-refresh map, the headless
    // Markdown engine the shell warms at boot, the new issue page's frame shell, the pending-writes
    // indicator and the kinds of write are each a chunk of their own now, and
    // `@legion/contracts`' subpaths keep that package's reference table and zod's schema modules
    // out of a page that reads neither - in the editor's chunks too, which import that package
    // by subpath alone (`packages/proof-editor/src/block-pointers.ts`).
    // The merge of 4b22ece4 read 500,672 bytes, over the budget; the cut in `01fc0e41` brought it
    // to 499,380, and `97c0cde6`'s focus helper added 105, so `4935c4af` reads those 499,485 bytes,
    // 515 under the budget. That cut made two moves. The Inbox's `Blocked on you` strip and the
    // project page's pill left the entry for a file of
    // their own, `features/inbox/BlockedOnYouLink.tsx`, a chunk of 866 bytes the Inbox's and the
    // project page's chunks share, and without that move the entry at `01fc0e41` measured 500,104
    // bytes. The modulepreload polyfill, 710 bytes of the entry, is off, for the reason beside
    // `modulePreload` below.
    // The other sizes here were measured the same way at `53286717` the same day: the largest
    // chunk is ECharts (an `index-*.js` of 1,134,772 bytes raw), which only the Delivery page
    // loads.
    // The loading box and the alert of the surfaces a key opens
    // (`features/shell/asked-surface.tsx`, review 6034817973) then took the entry to 500,530
    // bytes, over the budget again. The rail's pinned agents answered it: they draw a session
    // from `AgentName.tsx`'s session half alone (`features/refs/SessionName.tsx`), so the rest of
    // that module is a chunk the pages' chunks share, and the entry read 497,401 bytes with a
    // 39-character `DISPATCH_BUILD` on 2026-11-24. After the changes since, Details' one Escape
    // rule among them (`escapeClosesDrawer`, `features/shell/useDialog.ts`), the entry read
    // 497,933 bytes with a 39-character `DISPATCH_BUILD` the same day, and the two lists' alert
    // for a render error (`features/writes/write-hosts.tsx`) takes it to 498,553. The sign-in
    // page's list of kept writes, in the entry so that its warning depends on no chunk
    // (`features/writes/KeptWrites.tsx`), takes it to 499,583. The merge of the default branch's
    // correction and deletion of a post (#3512) took it to 500,975, over the budget. Three moves
    // then took code that no first paint draws out of the entry, none of them a new lazy load, and
    // the entry reads 498,776 bytes with a 39-character `DISPATCH_BUILD` on 2026-11-24: a held
    // send refreshes a comment list under `ownerCommentsQuery`'s key, so
    // `features/conversation/composer-model.ts` is out of the entry (411 bytes); the
    // rail's state mark reads its relative time from `features/refs/relative-time.ts`, so
    // `<Timestamp>` is a chunk the pages' chunks share (646 bytes); and the recent items read a
    // document's route from `features/refs/routes.ts`, so the search bar's chip
    // (`features/search/bar-scope.ts`) is a chunk that the bar and the issue list share
    // (1,142 bytes). That left 498,776 bytes; the correction's mark then joined the event map's
    // chunk (`api/event-refresh-load.ts`), and the entry read 498,590. The forward merges of the
    // default branch after it, whose layers of the page's foot (#4631) added about 6,500 bytes,
    // and the fixes after them took the entry to 506,935, over the budget. The page addresses
    // that the shell reads then left `features/refs/routes.ts` for a module of their own
    // (`features/refs/page-routes.ts`), with no new lazy load: the grammar of references and the
    // modules that only it imports are two chunks that the pages' chunks share (`routes-*.js`,
    // 10,989 bytes, and `dispatch-block-ref-*.js`, 3,632), and the entry reads 492,802 bytes with
    // a 39-character `DISPATCH_BUILD` on 2026-11-25.
    // `web/src/asset-sizes-in-build.ts` holds the entry to 500,000 bytes and keeps its history,
    // and `bun run check:asset-sizes` reads it again; this `chunkSizeWarningLimit` is Vite's own
    // warning and is over the ECharts chunk alone.
    // beautiful-mermaid and ELK live only in the Mermaid worker's bundle
    // (features/refs/mermaid-render.worker.ts, 1,565,608 bytes), fetched once a body or a
    // document holds a mermaid block; Vite emits it as an asset and holds it to no chunk limit.
    // The editor's lazy chunk (`lib-*.js`, 188,972 bytes) carries none of the renderer.
    chunkSizeWarningLimit: 1200,
    // No modulepreload polyfill. Vite's copy of it is 710 bytes of the entry chunk, and it does
    // nothing in a browser that has `<link rel=modulepreload>`: Chrome 66+, Safari 17+ and
    // Firefox 115+. Every browser without that link also lacks `Promise.withResolvers`, which
    // only Chrome 119+, Safari 17.4+ and Firefox 121+ have, and the app's own code calls it: each
    // write the page makes (`run` in `features/writes/write-store.ts`) and each upload
    // (`xhrUpload` in `api/client.ts`). So in those browsers every write fails as the reader
    // makes it, with the polyfill or without it: they do not run the app, and the polyfill acted
    // only in browsers the app already excludes. No browser that runs the app changes without it.
    // Turned off for the entry's budget (dispatch://INFRA-1109, 2026-11-23).
    modulePreload: { polyfill: false },
    emptyOutDir: true,
  },
  server: {
    proxy: {
      "/api": "http://localhost:8766",
      "/auth": "http://localhost:8766",
      "/ws": {
        target: "ws://localhost:8766",
        ws: true,
      },
    },
  },
});
