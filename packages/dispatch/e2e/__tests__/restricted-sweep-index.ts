/** What the restricted sweep's tests read of the tree, each in one place: the script and the parts
 *  of it a test reads, the route index it walks, the GitHub proxy reads it makes (asked of the
 *  script itself, which restricted-sweep.test.ts holds to the proxy's own expression), and the read
 *  check's own table, which tells a build with a read check from one without
 *  (restricted-sweep-stack.ts). */

import { readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { fileURLToPath } from "node:url";

export const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
export const sweepScript = fileURLToPath(
  new URL("../acceptance/restricted-sweep.sh", import.meta.url)
);
/** The sweep's fills part, which lists the fills whole fills hold (`fill_names`). */
export const fillsPart = fileURLToPath(
  new URL("../acceptance/restricted-sweep-fills.sh", import.meta.url)
);
/** The sweep's rows part, which names the GitHub proxy's answer for a repository the App is not
 *  installed on (`not_installed_code`). */
export const judgePart = fileURLToPath(
  new URL("../acceptance/restricted-sweep-judge.sh", import.meta.url)
);
export const routeIndex = fileURLToPath(
  new URL("../../../envoy/internal/dispatch/api/testdata/routes.txt", import.meta.url)
);
/** The GitHub proxy's source, whose `readable` expression names the only reads it forwards. */
export const proxySource = fileURLToPath(
  new URL("../../../envoy/internal/dispatch/githubapi/proxy.go", import.meta.url)
);
/** The read check's own table (`readclass` of every route, `read_classes.go`): a tree without it
 *  has no read check. */
export const readCheckFile = "packages/envoy/internal/dispatch/api/read_classes.go";

/** Refuses a GET row of the route index with no path: read as "", it would match a listing's
 *  empty line and a call's ` GET ` alike, so no row that reads the index could fail on it. */
function noPath(): never {
  throw new Error(`${routeIndex} has a GET row with no path`);
}

/** The path of each GET row of the route index, in its order. */
export const getRows: readonly string[] = readFileSync(routeIndex, "utf8")
  .split("\n")
  .map((line) => line.split(" "))
  .filter(([method]) => method === "GET")
  .map(([, path]) => path || noPath());

/** The SHA-256 of `bytes`, in hex. */
export function sha256Hex(bytes: string | Uint8Array): string {
  return new Bun.CryptoHasher("sha256").update(bytes).digest("hex");
}

/** The SHA-256 of the route index, in hex, which fills made from it name and a runner holds them
 *  to. */
export const routeIndexSha256 = sha256Hex(readFileSync(routeIndex));

/** The fills whole fills hold, as the fills part lists them (`fill_names`), each of which a runner
 *  refuses fills without. */
export const fillNames: readonly string[] = (() => {
  const listed = /^readonly fill_names='([^']*)'$/m.exec(readFileSync(fillsPart, "utf8"));
  if (listed === null) throw new Error(`${fillsPart} lists no fill_names`);
  return (listed[1] ?? "").split(/\s+/).filter((name) => name !== "");
})();

/** The GitHub proxy reads the script makes, after `/api/github/rest/repos/{owner}/{repo}/`, as it
 *  lists them (SWEEP_LIST_PROXY=1, exit 4). */
export const proxyReads: readonly string[] = (() => {
  const listed = Bun.spawnSync(["bash", sweepScript], {
    cwd: dispatchPackage,
    env: { PATH: process.env.PATH ?? "", TMPDIR: tmpdir(), SWEEP_LIST_PROXY: "1" },
    stderr: "pipe",
    stdout: "pipe",
  });
  const reads = listed.stdout.toString().trim().split("\n");
  if (listed.exitCode !== 4 || reads.some((read) => read === "")) {
    throw new Error(
      `SWEEP_LIST_PROXY=1 exited ${listed.exitCode}, listing ${JSON.stringify(reads)}: ${listed.stderr.toString()}`
    );
  }
  return reads;
})();
