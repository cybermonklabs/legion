import { afterAll, expect, test } from "bun:test";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { rowLimit, runBounded } from "./bounded-subprocess";

// What e2e/visual-run.sh tells a reader of a failed run: where the failed rows' shots are, and,
// for a run in reverse order, that a row failing there alone needs a seed mark and not a new
// baseline; and that however the run ends, the stack it started is stopped. The script runs
// whole, as CI runs it, with `bun` and `docker` standing in on PATH: the stand-in `bun` prints
// the stack's last variable for `e2e/harness-up.ts` and holds the stack up as the real one does,
// writing its pid to `STUB_STACK_PID` where that is set, and the stand-in `docker` exits with the
// status the row asks for, which is the status the Playwright run inside the container would
// have, after writing its pid to `STUB_DOCKER_STARTED` and holding `STUB_DOCKER_HOLD` seconds
// where those are set.
const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const stubs = mkdtempSync(join(tmpdir(), "visual-run-"));
afterAll(() => rmSync(stubs, { force: true, recursive: true }));

writeFileSync(
  join(stubs, "bun"),
  `#!/bin/sh
case "$1" in
  e2e/harness-up.ts)
    [ -z "$STUB_STACK_PID" ] || echo $$ > "$STUB_STACK_PID"
    echo 'export DISPATCH_E2E_REUSE_SERVERS=1'
    exec sleep 60 2>/dev/null ;;
  *) exit 0 ;;
esac
`
);
writeFileSync(
  join(stubs, "docker"),
  `#!/bin/sh
[ -z "$STUB_DOCKER_STARTED" ] || echo $$ > "$STUB_DOCKER_STARTED"
[ -z "$STUB_DOCKER_HOLD" ] || sleep "$STUB_DOCKER_HOLD"
exit "$STUB_DOCKER_STATUS"
`
);
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

// Playwright keeps its own directory for an empty `--output=` or `--output ""` (an unset variable
// passed as the value) and reads a word after `--` as no option, so the line names Playwright's
// directory for each rather than one Playwright never used.
test(
  "an empty --output in either form and an --output after -- leave Playwright's own directory named",
  async () => {
    for (const args of [["--output="], ["--output", ""], ["--", "--output=e2e/elsewhere"]]) {
      const run = await visualRun(1, undefined, args);
      expect(run.code).toBe(1);
      expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results"));
    }
  },
  rowLimit("short", "short", "short")
);

test(
  "the last --output names the directory, as Playwright reads the last one",
  async () => {
    const run = await visualRun(1, undefined, ["--output=e2e/a", "--output", "e2e/b"]);
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/b"));
  },
  rowLimit("short")
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

/** The environment of a run whose stack writes its pid to `STUB_STACK_PID`, and whose container
 *  writes its own to `STUB_DOCKER_STARTED`, holds `hold` seconds and exits with `status`. */
function heldRun(dir: string, status: number, hold: string | undefined) {
  return {
    HOME: process.env.HOME,
    PATH: `${stubs}:/usr/bin:/bin`,
    STUB_DOCKER_HOLD: hold,
    STUB_DOCKER_STARTED: join(dir, "docker"),
    STUB_DOCKER_STATUS: String(status),
    STUB_STACK_PID: join(dir, "stack"),
    TMPDIR: dir,
  };
}

// Whether the stack is still up once the script has ended, read by the driver itself, in the
// moment after the script returns: `runBounded` kills whatever its child leaves behind as soon as
// the child exits, so a read from this test would find every stack gone, stopped or not. The
// script's own stop waits for the stack, so a stopped stack is reaped by then; one the script left
// behind is still asleep.
const stackAfter =
  'if kill -0 "$(cat "$STUB_STACK_PID")" 2>/dev/null; then echo up; else echo down; fi > "$TMPDIR/stack-after"';
const read = (dir: string, name: string) => readFileSync(join(dir, name), "utf8").trim();

// A reader of a failed run's output that goes before the run ends - `| head`, `| grep -m1`, a
// pager quit early - leaves stderr a pipe with no reader, and a write to it raises SIGPIPE. Were
// the signal not ignored, it would end the script before its EXIT trap stopped the stack. The
// pipe here has no reader from the start: opened read-write on a fifo, then its read end closed.
test(
  "a failed run whose stderr reader has gone exits with the container's status and stops its stack",
  async () => {
    const dir = mkdtempSync(join(stubs, "pipe-"));
    await runBounded({
      argv: [
        "sh",
        "-c",
        'mkfifo "$1/err" && exec 4<>"$1/err" && exec 2>"$1/err" && exec 4<&- && shift && ' +
          `sh e2e/visual-run.sh "$@"; echo $? > "$TMPDIR/status"; ${stackAfter}`,
        "visual-run",
        dir,
        "--output=e2e/x",
      ],
      bound: "short",
      cwd: dispatchPackage,
      env: { ...heldRun(dir, 1, undefined), DISPATCH_E2E_VISUAL_ORDER: "reverse" },
      from: import.meta.url,
    });
    expect(read(dir, "status")).toBe("1");
    expect(read(dir, "stack-after")).toBe("down");
  },
  rowLimit("short")
);

// A Ctrl-C reaches the run's whole process group: the container stops at once, the stack - a job
// the script started in the background, so one that ignores SIGINT - does not, and the script's
// own INT trap must stop it. The run goes in a session of its own (`setsid --wait`, which forks
// when its caller already leads a group, as a spawned child does), so the signal reaches the
// run's group, read from the container's pid, and not this test's; it waits for the container to
// start. The group is signalled with procps's `kill`, since dash's takes no negative pid.
test(
  "a Ctrl-C to a running run stops its stack and exits 130",
  async () => {
    const dir = mkdtempSync(join(stubs, "int-"));
    const run = await runBounded({
      argv: [
        "sh",
        "-c",
        '(until [ -s "$STUB_DOCKER_STARTED" ]; do sleep 0.05; done; ' +
          '/usr/bin/kill -s INT -- "-$(ps -o pgid= -p "$(cat "$STUB_DOCKER_STARTED")" | tr -d " ")") & ' +
          `setsid --wait sh e2e/visual-run.sh "$@"; status=$?; ${stackAfter}; exit "$status"`,
        "visual-run",
      ],
      bound: "short",
      cwd: dispatchPackage,
      env: heldRun(dir, 0, "30"),
      from: import.meta.url,
    });
    expect(run.code).toBe(130);
    expect(read(dir, "stack-after")).toBe("down");
  },
  rowLimit("short")
);

// A supervisor's stop sends SIGTERM to the script alone. The script's TERM trap runs once the
// container has returned, and its exit stops the stack.
test(
  "a SIGTERM to a running run stops its stack and exits 143",
  async () => {
    const dir = mkdtempSync(join(stubs, "term-"));
    const run = await runBounded({
      argv: [
        "sh",
        "-c",
        'sh e2e/visual-run.sh "$@" & run=$!; ' +
          '(until [ -s "$STUB_DOCKER_STARTED" ]; do sleep 0.05; done; kill -TERM "$run") & ' +
          `wait "$run"; status=$?; ${stackAfter}; exit "$status"`,
        "visual-run",
      ],
      bound: "short",
      cwd: dispatchPackage,
      env: heldRun(dir, 1, "1"),
      from: import.meta.url,
    });
    expect(run.code).toBe(143);
    expect(read(dir, "stack-after")).toBe("down");
  },
  rowLimit("short")
);
