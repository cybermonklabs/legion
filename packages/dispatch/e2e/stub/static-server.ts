/** `bun e2e/stub/static-server.ts`: the one server the stubbed layer runs, which
 *  e2e/playwright.config.ts starts as the `stub` project's `webServer`.
 *
 *  It serves `packages/dispatch/web/dist` - the build under test, freshly built by the command
 *  that runs the suite - the way the Go router serves it (`packages/envoy/internal/dispatch/
 *  routes/router.go`, `servePage`): a file that exists is answered verbatim with an `ETag` of its
 *  bytes, and every other path is answered with `index.html`, which is the SPA fallback every
 *  route of the dashboard relies on.
 *
 *  Why a file server and not a recording of the document and its assets: `web/vite.config.ts`
 *  keeps the entry chunk's hash distinct for every build, so a recording that answered `/assets`
 *  would serve for ever the build it was recorded from, and a pull request's own front-end change
 *  would never be loaded. The layer would then prove nothing about the code under test. The
 *  recordings answer `/api/v1/**` and `/auth/whoami` and nothing else (e2e/stub/stub-test.ts).
 *
 *  It serves no API route at all: a stub row's API answers come from its recording, and a request
 *  the recording has none of is aborted by the fixture rather than answered here. */
import { createHash } from "node:crypto";
import { existsSync, readFileSync, statSync } from "node:fs";
import { extname, join, normalize, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { stubPort } from "./origin";

const dist = resolve(fileURLToPath(new URL("../../web/dist", import.meta.url)));

/** The content types the built SPA holds. A file whose extension is none of these is answered as
 *  bytes, which is what the browser asks of a font or a source map. */
const contentTypes: Readonly<Record<string, string>> = {
  ".css": "text/css; charset=utf-8",
  ".html": "text/html; charset=utf-8",
  ".ico": "image/x-icon",
  ".js": "text/javascript; charset=utf-8",
  ".json": "application/json",
  ".map": "application/json",
  ".png": "image/png",
  ".svg": "image/svg+xml",
  ".txt": "text/plain; charset=utf-8",
  ".woff": "font/woff",
  ".woff2": "font/woff2",
};

if (!existsSync(join(dist, "index.html"))) {
  throw new Error(
    `The stubbed layer serves ${dist}, which holds no index.html. Build the SPA first: ` +
      "`bun run build:web` in packages/dispatch, which `bun run e2e:stub` does for you."
  );
}

/** The file `path` names under `dist`, or undefined where the path names none: a directory, a
 *  path that climbs out of `dist`, or a route of the SPA, all of which take `index.html`. */
function fileFor(path: string): string | undefined {
  const within = normalize(path);
  if (within.includes("..")) return undefined;
  const file = join(dist, within);
  if (!file.startsWith(`${dist}/`)) return undefined;
  if (!existsSync(file) || !statSync(file).isFile()) return undefined;
  return file;
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: stubPort(),
  fetch(request) {
    const file = fileFor(new URL(request.url).pathname) ?? join(dist, "index.html");
    const bytes = readFileSync(file);
    return new Response(bytes, {
      headers: {
        "content-type": contentTypes[extname(file)] ?? "application/octet-stream",
        etag: `"${createHash("sha256").update(bytes).digest("hex").slice(0, 32)}"`,
      },
    });
  },
});

console.log(`dispatch stub static server: listening ${server.url} over ${dist}`);
