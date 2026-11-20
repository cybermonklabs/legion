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

/** The line a failed run ends with, naming `directory` as the run prints it: from the repository
 *  root, where a reader of a CI log and the job's upload start. */
const shotsUnder = (directory: string) =>
  `the run failed; a failed row's shots (-actual.png, -expected.png, -diff.png) are under ${directory}/.`;
const seedMark = "write that mark in the seed (markWhatTheShotPagesRead, e2e/review-seed.ts)";

test(
  "a failed run in the normal order names Playwright's own results directory, and nothing of a seed mark",
  async () => {
    const run = await visualRun(1, undefined, []);
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results"));
    expect(run.stderr).not.toContain(seedMark);
  },
  rowLimit("short")
);

test(
  "a failed run in reverse order names the directory --output gives it, and says a row failing there alone needs a seed mark",
  async () => {
    const run = await visualRun(1, "reverse", ["--output=e2e/test-results-reverse"]);
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results-reverse"));
    expect(run.stderr).toContain(seedMark);
    expect(run.stderr).toContain("take no new baseline from this run");
  },
  rowLimit("short")
);

test(
  "--output given as two words names its directory too, and an absolute one is printed as given",
  async () => {
    const relative = await visualRun(1, undefined, ["--output", "./e2e/elsewhere"]);
    expect(relative.code).toBe(1);
    expect(relative.stderr).toContain(shotsUnder("packages/dispatch/e2e/elsewhere"));
    const absolute = await visualRun(1, undefined, [`--output=${stubs}/results`]);
    expect(absolute.code).toBe(1);
    expect(absolute.stderr).toContain(shotsUnder(`${stubs}/results`));
  },
  rowLimit("short", "short")
);

// Playwright keeps its own directory for an empty `--output=` and reads a word after `--` as no
// option, so the line names Playwright's directory for both rather than one Playwright never used.
test(
  "an empty --output= and an --output after -- leave Playwright's own directory named",
  async () => {
    const empty = await visualRun(1, undefined, ["--output="]);
    expect(empty.code).toBe(1);
    expect(empty.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results"));
    const positional = await visualRun(1, undefined, ["--", "--output=e2e/elsewhere"]);
    expect(positional.code).toBe(1);
    expect(positional.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results"));
  },
  rowLimit("short", "short")
);

test(
  "a run that passed says nothing of either, on either stream",
  async () => {
    const run = await visualRun(0, "reverse", ["--output=e2e/test-results-reverse"]);
    expect(run.code).toBe(0);
    for (const stream of [run.stderr, run.stdout]) {
      expect(stream).not.toContain("the run failed");
      expect(stream).not.toContain(seedMark);
    }
  },
  rowLimit("short")
);

// The status is the container's even when the lines cannot be written: a closed stderr makes
// `echo` fail, and under `set -e` that would end the script with echo's status instead.
test(
  "a failed run whose stderr is closed still exits with the container's status",
  async () => {
    const run = await runBounded({
      argv: ["sh", "-c", 'sh e2e/visual-run.sh "$@" 2>&-', "visual-run", "--output=e2e/x"],
      bound: "short",
      cwd: dispatchPackage,
      env: {
        DISPATCH_E2E_VISUAL_ORDER: "reverse",
        HOME: process.env.HOME,
        PATH: `${stubs}:/usr/bin:/bin`,
        STUB_DOCKER_STATUS: "130",
        TMPDIR: stubs,
      },
      from: import.meta.url,
    });
    expect(run.code).toBe(130);
  },
  rowLimit("short")
);
