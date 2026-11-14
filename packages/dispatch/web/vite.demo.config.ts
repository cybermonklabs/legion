// The views demo (packages/dispatch/e2e/views-demo/README.md): Dispatch's own Vite config with a
// stubbed back end in the browser. The stub (src/demo/install.ts) goes into the page before the
// app's entry, so no app file imports it; the dev server serves the fixtures from
// DISPATCH_DEMO_FIXTURES and proxies nothing.
//
//   cd packages/dispatch/web
//   DISPATCH_DEMO_FIXTURES=<dir> bunx vite --config vite.demo.config.ts --port 5190 --host 127.0.0.1
//
// With DISPATCH_DEMO_SINGLE_FILE=1 a build makes one JavaScript file and one stylesheet, every
// asset inlined, and gives the app a router that works from a `file:` address
// (src/demo/file-router.ts); e2e/views-demo/single-file.ts builds that and inlines the rest.
import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { mergeConfig, type Plugin } from "vite";
import { FIXTURE_PATH } from "./src/demo/fixtures";
import base from "./vite.config";

const webRoot = fileURLToPath(new URL(".", import.meta.url));
const fixtures = process.env.DISPATCH_DEMO_FIXTURES;
const singleFile = process.env.DISPATCH_DEMO_SINGLE_FILE === "1";

const stub: Plugin = {
  name: "views-demo-stub",
  transformIndexHtml: {
    order: "pre",
    handler: () => [
      {
        tag: "script",
        attrs: { type: "module", src: "/src/demo/install.ts" },
        injectTo: "head-prepend",
      },
    ],
  },
  configureServer(server) {
    server.middlewares.use(FIXTURE_PATH, async (request, response) => {
      const name = (request.url ?? "").replace(/^\//, "").replace(/\?.*$/, "");
      if (fixtures === undefined || !/^[a-z-]+\.json$/.test(name)) {
        response.statusCode = 404;
        response.end(fixtures === undefined ? "set DISPATCH_DEMO_FIXTURES" : "no such fixture");
        return;
      }
      const body = await readFile(join(fixtures, name)).catch(() => undefined);
      response.statusCode = body === undefined ? 404 : 200;
      response.setHeader("content-type", "application/json");
      response.setHeader("cache-control", "no-store");
      response.end(body ?? "");
    });
  },
};

/** The single file's router: the app's entry imports react-router-dom's BrowserRouter, which
 *  this hands a HashRouter in its place. */
const fileRouter: Plugin = {
  name: "views-demo-file-router",
  enforce: "pre",
  resolveId(source, importer) {
    if (source !== "react-router-dom" || importer === undefined) return null;
    if (!importer.endsWith("/src/main.tsx")) return null;
    return join(webRoot, "src/demo/file-router.ts");
  },
};

const config = mergeConfig(base, {
  root: webRoot,
  plugins: singleFile ? [stub, fileRouter] : [stub],
  build: singleFile
    ? {
        assetsInlineLimit: Number.MAX_SAFE_INTEGER,
        cssCodeSplit: false,
        modulePreload: false,
        rollupOptions: { output: { inlineDynamicImports: true } },
      }
    : {},
});
// The demo answers every read itself: nothing goes to a server.
config.server = { ...config.server, proxy: undefined };

export default config;
