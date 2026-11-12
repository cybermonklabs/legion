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
      // of this bundle today - `dispatch-api` by nothing in the repository, `dispatch-tools` by
      // one `import type` the compiler erases, and `dispatch-text-references` by three Bun
      // tests, which resolve through `exports` and never see this config - and they are kept so
      // that a value import of one resolves to its module rather than being rewritten into
      // `index.ts` by the bare key below it.
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
      "@legion/contracts/repo": fileURLToPath(
        new URL("../../contracts/src/repo.ts", import.meta.url)
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
    // Every size here was measured with `bun run build:web` on 2026-11-13, at this head. The
    // largest chunk is ECharts (an `index-*.js` of 1,134,772 bytes raw), which only the
    // Delivery page loads, and the entry - the one `assets/index-*.js` that dist/index.html
    // names - is 491,121 bytes, down from 1,028,690 before dispatch://INFRA-1109 split the
    // shell: the Inbox and its peek, the ask card, the search bar, the Create issue dialog,
    // the shortcut sheet, the reference-preview layer and the event-refresh map are each a
    // chunk of their own now, and `@legion/contracts`' subpaths keep that package's reference
    // table and zod's schema modules out of a page that reads neither.
    // `web/src/asset-sizes-in-build.ts` holds the entry to 500,000 bytes and `bun run
    // check:asset-sizes` reads it again; this `chunkSizeWarningLimit` is Vite's own warning
    // and is over the ECharts chunk alone.
    // beautiful-mermaid and ELK live only in the Mermaid worker's bundle
    // (features/refs/mermaid-render.worker.ts, 1,565,608 bytes), fetched once a body or a
    // document holds a mermaid block; Vite emits it as an asset and holds it to no chunk limit.
    // The editor's lazy chunk (`lib-*.js`, 208,284 bytes) carries none of the renderer.
    chunkSizeWarningLimit: 1200,
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
