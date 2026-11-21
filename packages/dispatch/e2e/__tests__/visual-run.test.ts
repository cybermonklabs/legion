import { afterAll, expect, test } from "bun:test";
import {
  chmodSync,
  copyFileSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { rowLimit, runBounded } from "./bounded-subprocess";

// What e2e/visual-run.sh tells a reader of a failed run: where the failed rows' shots are, and,
// for a run in reverse order, that a row failing there alone needs a seed mark and not a new
// baseline; that a failed run which wrote no results, as when Playwright refuses its arguments,
// names no directory; and that however the run ends, the stack it started is stopped. The script
// runs whole, as CI runs it, with `bun` and `docker` standing in on PATH: the stand-in `bun`
// prints the stack's last variable for `e2e/harness-up.ts` and holds the stack up as the real one
// does, writing its pid to `STUB_STACK_PID` where that is set, and the stand-in `docker` exits
// with the status the row asks for, which is the status the Playwright run inside the container
// would have, after writing its pid to `STUB_DOCKER_STARTED` and holding `STUB_DOCKER_HOLD`
// seconds where those are set, and after writing a `.last-run.json` under `STUB_RESULTS` where
// that names a directory, as a Playwright run whose rows ran writes one under its results
// directory. The wait before that write is 50 ms: a file's time comes from the kernel's coarse
// clock, which moves in steps of a few milliseconds, and a write in the same step as the script's
// mark of the container's start would read as no newer than it, where a real run is seconds later.
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
if [ -n "$STUB_RESULTS" ]; then
  sleep 0.05
  mkdir -p -- "$STUB_RESULTS" && : > "$STUB_RESULTS/.last-run.json"
fi
exit "$STUB_DOCKER_STATUS"
`
);
chmodSync(join(stubs, "bun"), 0o755);
chmodSync(join(stubs, "docker"), 0o755);

/** What a run leaves under its tree: `results`, the directory the stand-in container writes its
 *  results in, as Playwright does where its rows ran, and `stale`, a directory that holds an
 *  earlier run's results, a minute old, before this run starts. Each is read from the package, as
 *  `--output` is. */
interface Results {
  readonly results?: string;
  readonly stale?: string;
}

/** One run of the script with the stand-ins, the container's status and the order given, in a
 *  tree of its own: a copy of the script at packages/dispatch/e2e/visual-run.sh under a new
 *  temporary root, so that what the run writes lands there and not in this checkout, and the line
 *  names it from that root as it names it from the repository's. */
function visualRun(
  status: number,
  order: string | undefined,
  args: readonly string[],
  { results, stale }: Results = {}
) {
  const runPackage = join(mkdtempSync(join(stubs, "tree-")), "packages/dispatch");
  mkdirSync(join(runPackage, "e2e"), { recursive: true });
  copyFileSync(join(dispatchPackage, "e2e/visual-run.sh"), join(runPackage, "e2e/visual-run.sh"));
  if (stale !== undefined) {
    const directory = resolve(runPackage, stale);
    mkdirSync(directory, { recursive: true });
    writeFileSync(join(directory, ".last-run.json"), "{}");
    const minuteAgo = new Date(Date.now() - 60_000);
    utimesSync(join(directory, ".last-run.json"), minuteAgo, minuteAgo);
    utimesSync(directory, minuteAgo, minuteAgo);
  }
  return runBounded({
    argv: ["sh", "e2e/visual-run.sh", ...args],
    bound: "short",
    cwd: runPackage,
    env: {
      DISPATCH_E2E_VISUAL_ORDER: order,
      HOME: process.env.HOME,
      PATH: `${stubs}:/usr/bin:/bin`,
      STUB_DOCKER_STATUS: String(status),
      STUB_RESULTS: results,
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
/** The line a failed run ends with where it wrote no results. */
const ranNoRow = "the run failed, and Playwright ran no row: it wrote no results in this run";

// In each row below that names a directory, the stand-in container writes its results in the
// directory Playwright itself uses for those arguments, so a line naming any other directory finds
// no results there and names none.

test(
  "a failed run in the normal order names Playwright's own results directory, and nothing of a seed mark",
  async () => {
    const run = await visualRun(1, undefined, [], { results: "e2e/test-results" });
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results"));
    expect(run.stderr).not.toContain(seedMark);
  },
  rowLimit("short")
);

test(
  "a failed run in reverse order names the directory --output gives it, and says a row failing there alone needs a seed mark",
  async () => {
    const run = await visualRun(1, "reverse", ["--output=e2e/test-results-reverse"], {
      results: "e2e/test-results-reverse",
    });
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
    const relative = await visualRun(1, undefined, ["--output", "./e2e/elsewhere"], {
      results: "e2e/elsewhere",
    });
    expect(relative.code).toBe(1);
    expect(relative.stderr).toContain(shotsUnder("packages/dispatch/e2e/elsewhere"));
    const absoluteDirectory = join(mkdtempSync(join(stubs, "absolute-")), "results");
    const absolute = await visualRun(1, undefined, [`--output=${absoluteDirectory}`], {
      results: absoluteDirectory,
    });
    expect(absolute.code).toBe(1);
    expect(absolute.stderr).toContain(shotsUnder(absoluteDirectory));
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
      const run = await visualRun(1, undefined, args, { results: "e2e/test-results" });
      expect(run.code).toBe(1);
      expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results"));
    }
  },
  rowLimit("short", "short", "short")
);

test(
  "the last --output names the directory, as Playwright reads the last one",
  async () => {
    const run = await visualRun(1, undefined, ["--output=e2e/a", "--output", "e2e/b"], {
      results: "e2e/b",
    });
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/b"));
  },
  rowLimit("short")
);

// The line names the directory as given: a backslash in it is printed as written, where an `echo`
// in dash would read `\t` as a tab.
test(
  "a directory with a backslash in it is printed as written",
  async () => {
    const run = await visualRun(1, undefined, ["--output=e2e/a\\tb"], { results: "e2e/a\\tb" });
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/a\\tb"));
  },
  rowLimit("short")
);

// A directory given with a trailing slash is named once with it, as `e2e/x/.`, not `e2e/x//.`.
test(
  "a directory given with a trailing slash is named with one slash before the dot",
  async () => {
    const run = await visualRun(1, undefined, ["--output=e2e/x/"], { results: "e2e/x/" });
    expect(run.code).toBe(1);
    expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/x"));
    expect(run.stderr).not.toContain("e2e/x//.");
  },
  rowLimit("short")
);

// Playwright reads the word after an option that takes a value as that value, whatever it looks
// like, so an `--output=` there names no directory; written as `--grep-invert=x`, or as a short
// option with its value in its cluster, the option takes no next word and the `--output` counts.
test(
  "an --output that another option takes as its value names no directory, as Playwright reads it",
  async () => {
    const value = "--output=e2e/not-output";
    for (const taken of [
      ["--grep-invert", value],
      ["-G", value],
      ["-xG", value],
    ]) {
      const run = await visualRun(1, undefined, taken, { results: "e2e/test-results" });
      expect(run.code).toBe(1);
      expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/test-results"));
    }
    for (const counted of [
      ["--grep-invert=x", "--output=e2e/is-output"],
      ["-Gx", "--output=e2e/is-output"],
    ]) {
      const run = await visualRun(1, undefined, counted, { results: "e2e/is-output" });
      expect(run.code).toBe(1);
      expect(run.stderr).toContain(shotsUnder("packages/dispatch/e2e/is-output"));
    }
  },
  rowLimit("short", "short", "short", "short", "short")
);

// Playwright refuses an argument it cannot read before it runs a row, and exits 1 as a failed row
// does: `-uG`, whose `G` is no `--update-snapshots` mode, and an option it does not know are two
// such lists. That run writes no results, so its line names no directory - neither the one its
// arguments name nor the one an earlier run left results in - says that Playwright ran no row, and
// says nothing of a seed mark, since no row failed in either order. A run with the same directory
// whose rows ran and failed still names it.
test(
  "a failed run that wrote no results, as when Playwright refuses its arguments, names no directory",
  async () => {
    for (const refused of [
      ["-uG", "--output=e2e/x"],
      ["--output=e2e/x", "--no-such-option"],
    ]) {
      const run = await visualRun(1, "reverse", refused, { stale: "e2e/x" });
      expect(run.code).toBe(1);
      expect(run.stderr).toContain(ranNoRow);
      expect(run.stderr).not.toContain("are under");
      expect(run.stderr).not.toContain("e2e/x");
      expect(run.stderr).not.toContain(seedMark);
    }
    const ran = await visualRun(1, "reverse", ["--output=e2e/x"], {
      results: "e2e/x",
      stale: "e2e/x",
    });
    expect(ran.code).toBe(1);
    expect(ran.stderr).toContain(shotsUnder("packages/dispatch/e2e/x"));
    expect(ran.stderr).toContain(seedMark);
    expect(ran.stderr).not.toContain(ranNoRow);
  },
  rowLimit("short", "short", "short")
);

/** The option names of one of visual-run.sh's two lists, sorted. */
function listed(script: string, name: string): string[] {
  const line = new RegExp(`^${name}="([^"]*)"$`, "m").exec(script);
  if (line === null) throw new Error(`e2e/visual-run.sh holds no ${name}="..." line`);
  return line[1].trim().split(/\s+/).sort();
}

// The script's two lists are the value-taking options of `playwright test` at the version the run
// uses, copied from its help; a Playwright that gains or drops one makes this row fail until the
// lists follow. The help is the one Playwright prints, read here through the tree's own package.
test(
  "the options visual-run.sh reads a value after are the pinned Playwright's",
  async () => {
    const help = await runBounded({
      argv: [process.execPath, "node_modules/@playwright/test/cli.js", "test", "--help"],
      bound: "short",
      cwd: dispatchPackage,
      from: import.meta.url,
    });
    expect(help.code).toBe(0);
    const required: string[] = [];
    const optional: string[] = [];
    for (const match of help.stdout.matchAll(/^ {2}(?:(-\w), )?(--[\w-]+|-\w)(?: ([<[]))?/gm)) {
      const [, short, name, value] = match;
      const names = short === undefined ? [name] : [short, name];
      if (value === "<") required.push(...names);
      if (value === "[") optional.push(...names);
    }
    expect(required).toContain("--output");
    const script = readFileSync(join(dispatchPackage, "e2e/visual-run.sh"), "utf8");
    expect(listed(script, "playwright_value_options")).toEqual(required.sort());
    expect(listed(script, "playwright_optional_options")).toEqual(optional.sort());
  },
  rowLimit("short")
);

test(
  "a run that passed says nothing of either, on either stream",
  async () => {
    const run = await visualRun(0, "reverse", ["--output=e2e/test-results-reverse"], {
      results: "e2e/test-results-reverse",
    });
    expect(run.code).toBe(0);
    for (const stream of [run.stderr, run.stdout]) {
      expect(stream).not.toContain("the run failed");
      expect(stream).not.toContain(seedMark);
    }
  },
  rowLimit("short")
);

// The status is the container's, and the stack stops, even when the lines cannot be written: a
// closed stderr makes the write fail, and under `set -e` an unguarded write would end the script
// with the write's status instead.
test(
  "a failed run whose stderr is closed still exits with the container's status and stops its stack",
  async () => {
    const dir = mkdtempSync(join(stubs, "closed-"));
    await runBounded({
      argv: [
        "sh",
        "-c",
        `sh e2e/visual-run.sh "$@" 2>&-; echo $? > "$TMPDIR/status"; ${stackAfter}`,
        "visual-run",
        "--output=e2e/x",
      ],
      bound: "short",
      cwd: dispatchPackage,
      env: { ...heldRun(dir, 130, undefined), DISPATCH_E2E_VISUAL_ORDER: "reverse" },
      from: import.meta.url,
    });
    expect(read(dir, "status")).toBe("130");
    expect(read(dir, "stack-after")).toBe("down");
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

// A Ctrl-C reaches the run's whole process group: the container stops at once, and the stand-in
// stack - a job the script started in the background, which a non-interactive shell starts with
// SIGINT ignored - does not, so the script's own INT trap must stop it. (The real harness keeps a
// SIGINT listener of its own; this row holds the script's trap, not the harness's.) The run goes
// in a session of its own (`setsid`), so the signal reaches the run's group, read from the
// container's pid, and not this test's; it waits for the container to start. The group is
// signalled with `/usr/bin/kill -s INT -- -PGID`.
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
