/** `bun e2e/stub/static-server.ts`: the one server the stubbed layer runs, which
 *  e2e/playwright.config.ts starts as the `stub` project's `webServer`.
 *
 *  It serves `packages/dispatch/web/dist` - the build under test, freshly built by the command
 *  that runs the suite - the way the Go router serves it (`packages/envoy/internal/dispatch/
 *  routes/router.go`, `servePage`): a file that exists is answered verbatim, and every other
 *  path is answered with `index.html`, which is the SPA fallback every route of the dashboard
 *  relies on. It sends no `ETag`, and the comment at the answer below says why.
 *
 *  Why a file server and not a recording of the document and its assets: `web/vite.config.ts`
 *  keeps the entry chunk's hash distinct for every build, so a recording that answered `/assets`
 *  would serve for ever the build it was recorded from, and a pull request's own front-end change
 *  would never be loaded. The layer would then prove nothing about the code under test. The
 *  recordings answer `/api/v1/**` and `/auth/whoami` and nothing else (e2e/stub/stub-test.ts).
 *
 *  It serves no API route at all: a stub row's API answers come from its recording, and a request
 *  the recording has none of is aborted by the fixture rather than answered here. */
import { existsSync, statSync } from "node:fs";
import { basename, extname, join, normalize, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { stubPort } from "./origin";

const dist = resolve(fileURLToPath(new URL("../../web/dist", import.meta.url)));

/** The content types the built SPA holds. A file whose extension is none of these is answered as
 *  bytes. */
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

/** The file `path` names under `dist`, or undefined where the path names none: a directory, or a
 *  path that climbs out of `dist`. */
function fileFor(path: string): string | undefined {
  const within = normalize(path);
  if (within.includes("..")) return undefined;
  const file = join(dist, within);
  if (!file.startsWith(`${dist}/`)) return undefined;
  // One stat, not an `existsSync` and a `statSync`: this runs for every asset of every page
  // load, and one process serves every lane.
  const seen = statSync(file, { throwIfNoEntry: false });
  if (seen === undefined || !seen.isFile()) return undefined;
  return file;
}

/** Whether a path the build holds no file for takes the SPA's own document, which is the Go
 *  router's rule (`isBrowserRoute`, packages/envoy/internal/dispatch/routes/router.go): never
 *  under `/assets`, always under `/issues`, which carries slugs and ids that hold a dot, and
 *  otherwise only where the last segment holds no dot.
 *
 *  It is the rule and not the fallback a file server usually writes, because the difference is
 *  what several rows assert: a page whose entry chunk 404s recovers by reloading
 *  (web/src/features/shell/DeploymentResilience.tsx), and a server that answered that chunk
 *  with the document would load an HTML file as a script and start no recovery at all. */
function takesTheDocument(path: string): boolean {
  if (path === "/assets" || path.startsWith("/assets/")) return false;
  if (path === "/issues" || path.startsWith("/issues/")) return true;
  return !basename(path).includes(".");
}

/** The routes a recording answers, which this server must never answer. A stub row installs
 *  `routeFromHAR` with `notFound: "abort"`, so a request its recording has none of is aborted
 *  in the browser and never reaches here - but a row whose fixture is not installed, or a
 *  request made before it is, would otherwise be given `index.html` with a 200 and parse an
 *  HTML document as JSON somewhere far from this cause.
 *
 *  Every path under `/api` and `/auth` and the two bare names themselves, not `/api/v1` alone:
 *  the SPA also asks `/api/github/rest/...` (`web/src/api/client.ts`), and a server that
 *  answered one of those with the document would hide the same fault in a new place. */
const recordedRoutes = /^\/(api|auth)(\/|$)/;

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: stubPort(),
  fetch(request) {
    const path = new URL(request.url).pathname;
    if (recordedRoutes.test(path)) {
      return new Response(
        `${path} is answered by a row's recording, not by this server. A request that reaches ` +
          "here is one no `routeFromHAR` caught: the row's fixture was not installed, or it " +
          "was installed after the request was made.\n",
        { headers: { "content-type": "text/plain; charset=utf-8" }, status: 404 }
      );
    }
    const found = path === "/" ? join(dist, "index.html") : fileFor(path);
    if (found === undefined && !takesTheDocument(path)) {
      return new Response("not found\n", {
        headers: { "content-type": "text/plain; charset=utf-8" },
        status: 404,
      });
    }
    // `Bun.file` hands the bytes to the kernel rather than copying them through this process.
    // No `ETag`: nothing reads one here, because this server answers no conditional request and
    // every row opens a context with an empty cache, and hashing two megabytes of document and
    // chunks for each of several hundred rows cost 25 ms a row on the one process all four
    // lanes share.
    const file = found ?? join(dist, "index.html");
    return new Response(Bun.file(file), {
      headers: {
        // The Go router's two: a page is revalidated before it runs, since it names the hashed
        // assets of the build that wrote it, and a hashed asset is kept for a year.
        "cache-control":
          extname(file) === ".html" ? "no-cache" : "public, max-age=31536000, immutable",
        "content-type": contentTypes[extname(file)] ?? "application/octet-stream",
      },
    });
  },
});

console.log(`dispatch stub static server: listening ${server.url} over ${dist}`);
