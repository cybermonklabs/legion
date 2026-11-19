/** How many Dispatch server processes the harness runs, and how a request names one of them.
 *
 * `DISPATCH_E2E_PROCESSES` unset (or `1`) is the single server on `DISPATCH_E2E_PORT`. `2` runs
 * Dispatch A and B on `DISPATCH_E2E_PORT_A` and `DISPATCH_E2E_PORT_B` against the one
 * `DATABASE_URL`, behind `e2e/round-robin-proxy.ts` on `DISPATCH_E2E_PORT`, so `baseUrl` and every
 * helper still address one origin. Any other value is refused naming the allowed ones, rather
 * than read as one of them: a run that meant two processes and got one passes every spec that
 * cannot tell them apart. `e2e/run-server.sh` refuses it the same way.
 *
 * Like `e2e/harness-ports.ts`, which imports it, this module reads only the environment, so every
 * process in `e2e/` may import it and each refuses a bad value alike. Two of its helpers read a
 * file as well: `webDistOf` names a build's dist and `entryChunkOf` reads the entry chunk that
 * dist's `index.html` names, which the two multiprocess specs share rather than each deciding for
 * itself.
 */

import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

/** One of the two processes of two-process mode. */
export type DispatchProcess = "a" | "b";

export const dispatchProcesses: readonly DispatchProcess[] = ["a", "b"];

/** The request header that sends one request to one process, whatever the cookie says. */
export const processPinHeader = "X-Dispatch-E2E-Process";

/** The cookie that sends every request of a browser context to one process, websocket upgrades
 *  included, which carry no header a page can set. */
export const processPinCookie = "dispatch-e2e-process";

/** The cookies that split a browser context between the two processes: `pagePinCookie` sends its
 *  page requests (a navigation, `index.html`, `/assets/*`) to one process and `apiPinCookie` every
 *  other request (the API, its event stream and websockets, `/auth`) to the other, so one build's
 *  page runs against the other build's server. Each beats `processPinCookie` for its kind. */
export const pagePinCookie = "dispatch-e2e-page";
export const apiPinCookie = "dispatch-e2e-api";

/** packages/envoy, which a path in `DISPATCH_E2E_BINARY_B` is read from. */
const envoyDirectory = fileURLToPath(new URL("../../envoy", import.meta.url));

/** The envoy-dispatch Dispatch B runs in place of the one e2e/run-server.sh builds from this tree,
 *  when `DISPATCH_E2E_BINARY_B` names one (a path from packages/envoy): a previous release's build
 *  (packages/envoy/scripts/build-dispatch-build.sh), so a run drives two builds of the server. Such
 *  a build must be legion-envoy-v30.11.0 or later: it serves on the socket the harness holds for it
 *  and signs cookies with the run's shared key, as a build from this tree does.
 *
 *  It is made absolute here, against packages/envoy, because run-server.sh takes the path it is
 *  handed (`DISPATCH_E2E_SERVER_BIN`) from its own caller's directory, which is not that one.
 *  Every reader of this value then names one file, whoever resolves it. */
export const binaryB = process.env.DISPATCH_E2E_BINARY_B
  ? resolve(envoyDirectory, process.env.DISPATCH_E2E_BINARY_B)
  : undefined;

/** The web dist each process's server serves, which is where a page of that process comes from.
 *  A's is always this tree's `web/dist`. B's is the `web/` beside `binaryB` when a previous build
 *  runs, which build-dispatch-build.sh wrote into that build's one directory; with no previous
 *  build it is what the harness was told to serve (`DISPATCH_E2E_WEB_DIST_B`, a path from
 *  packages/envoy, as e2e/harness-listeners.ts hands it to run-server.sh), and A's dist where that
 *  is unset, which is the default run-server.sh itself takes. One place decides this, so a spec
 *  that reads a page's own build reads the same answer as the one the harness was told, where the
 *  caller gave the harness that same value: it is the rule both follow, not a check that they
 *  agree, and a caller that hands run-server.sh some other dist is what the cross-build spec's own
 *  assertion catches. */
export function webDistOf(target: DispatchProcess): string {
  if (target === "a") return resolve(envoyDirectory, "../dispatch/web/dist");
  if (binaryB !== undefined) return resolve(dirname(binaryB), "web");
  return resolve(envoyDirectory, process.env.DISPATCH_E2E_WEB_DIST_B || "../dispatch/web/dist");
}

/** The entry chunk a dist's `index.html` names, `/assets/index-*.js`: the file a page of that
 *  build loads itself from, and the one thing in the page that names the build it came from. */
export function entryChunkOf(dist: string): string {
  const page = join(dist, "index.html");
  const entry = /<script type="module" crossorigin src="(\/assets\/[^"]+\.js)"><\/script>/.exec(
    readFileSync(page, "utf8")
  )?.[1];
  if (entry === undefined) throw new Error(`${page} names no entry chunk`);
  return entry;
}

/** Headers that send a fetch to `target`. */
export function pinnedTo(target: DispatchProcess): Record<string, string> {
  return { [processPinHeader]: target };
}

function resolveProcessCount(): 1 | 2 {
  const requested = process.env.DISPATCH_E2E_PROCESSES;
  if (requested === undefined || requested === "" || requested === "1") return 1;
  if (requested === "2") return 2;
  throw new Error(
    `DISPATCH_E2E_PROCESSES must be "1", "2" or unset, not ${JSON.stringify(requested)}. ` +
      "Leave it unset (or 1) to run one Dispatch server, or set it to 2 to run two behind the " +
      "round-robin proxy, as `bun run e2e:multiprocess` does."
  );
}

export const harnessProcessCount = resolveProcessCount();
