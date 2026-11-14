// The ask card demo (packages/dispatch/e2e/ask-card-demo/README.md): Dispatch's own Vite config
// with a stubbed back end in the browser. The stub (src/demo/install.ts) goes into the page before
// the app's entry, so no app file imports it, and the dev server proxies nothing.
//
//   cd packages/dispatch/web
//   bunx vite --config vite.demo.config.ts --port 5191 --host 127.0.0.1
//
// With DISPATCH_DEMO_SINGLE_FILE=1 a build makes one JavaScript file and one stylesheet, every
// asset inlined, and gives the app a router that works from a `file:` address
// (src/demo/file-router.ts); e2e/ask-card-demo/single-file.ts builds that and inlines the rest.
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { mergeConfig, type Plugin } from "vite";
import base from "./vite.config";

const webRoot = fileURLToPath(new URL(".", import.meta.url));
const singleFile = process.env.DISPATCH_DEMO_SINGLE_FILE === "1";

const stub: Plugin = {
  name: "ask-card-demo-stub",
  transformIndexHtml: {
    order: "pre",
    // The stub's module goes in ahead of the app's entry, so it replaces fetch, WebSocket and
    // Date before any app module runs.
    handler: () => [
      {
        tag: "script",
        attrs: { type: "module", src: "/src/demo/install.ts" },
        injectTo: "head-prepend",
      },
    ],
  },
};

/** The single file's router: the app's entry imports react-router-dom's BrowserRouter, which
 *  this hands a HashRouter in its place. */
const fileRouter: Plugin = {
  name: "ask-card-demo-file-router",
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
