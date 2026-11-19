import { afterAll, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { rowLimit, runBounded } from "./bounded-subprocess";

// What e2e/visual-run.sh tells a reader of a failed run: where the failed rows' shots are, and,
// for a run in reverse order, that a row failing there alone needs a seed mark and not a new
// baseline. The script runs whole, as CI runs it, with `bun` and `docker` standing in on PATH:
// the stand-in `bun` prints the stack's last variable for `e2e/harness-up.ts` and holds the
// stack up as the real one does, and the stand-in `docker` exits with the status the row asks
// for, which is the status the Playwright run inside the container would have.
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const stubs = mkdtempSync(join(tmpdir(), "visual-run-"));
afterAll(() => rmSync(stubs, { force: true, recursive: true }));

writeFileSync(
  join(stubs, "bun"),
  `#!/bin/sh
case "$1" in
  e2e/harness-up.ts) echo 'export DISPATCH_E2E_REUSE_SERVERS=1'; exec sleep 60 ;;
  *) exit 0 ;;
esac
`
);
writeFileSync(join(stubs, "docker"), '#!/bin/sh\nexit "$STUB_DOCKER_STATUS"\n');
chmodSync(join(stubs, "bun"), 0o755);
chmodSync(join(stubs, "docker"), 0o755);

/** One run of the script with the stand-ins, the container's status and the order given. */
function visualRun(status: number, order: string | undefined, args: readonly string[]) {
  return runBounded({
    argv: ["sh", "e2e/visual-run.sh", ...args],
    bound: "short",
    cwd: dispatchPackage,
    env: {
      DISPATCH_E2E_VISUAL_ORDER: order,
      HOME: process.env.HOME,
      PATH: `${stubs}:/usr/bin:/bin`,
      STUB_DOCKER_STATUS: String(status),
      TMPDIR: stubs,
    },
    from: import.meta.url,
  });
}

const shotsUnder = (directory: string) =>
  `the run failed; a failed row's shots (-actual.png, -expected.png, -diff.png) are under ${directory}/.`;
const seedMark = "write that mark in the seed (markWhatTheShotPagesRead, e2e/review-seed.ts)";

test(
  "a failed run in the normal order names Playwright's own results directory, and nothing of a seed mark",
  async () => {
    const run = await visualRun(1, undefined, []);
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("e2e/test-results"));
    expect(run.stderr).not.toContain(seedMark);
  },
  rowLimit("short")
);

test(
  "a failed run in reverse order names the directory --output gives it, and says a row failing there alone needs a seed mark",
  async () => {
    const run = await visualRun(1, "reverse", ["--output=e2e/test-results-reverse"]);
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("e2e/test-results-reverse"));
    expect(run.stderr).toContain(seedMark);
    expect(run.stderr).toContain("take no new baseline from this run");
  },
  rowLimit("short")
);

test(
  "--output given as two words names its directory too",
  async () => {
    const run = await visualRun(1, undefined, ["--output", "e2e/elsewhere"]);
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("e2e/elsewhere"));
  },
  rowLimit("short")
);

test(
  "a run that passed says nothing of either",
  async () => {
    const run = await visualRun(0, "reverse", ["--output=e2e/test-results-reverse"]);
    expect(run.code).toBe(0);
    expect(run.stderr).not.toContain("the run failed");
    expect(run.stderr).not.toContain(seedMark);
  },
  rowLimit("short")
);
