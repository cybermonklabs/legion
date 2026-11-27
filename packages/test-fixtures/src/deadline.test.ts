import { expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  deadlineFor,
  isParallelWorker,
  markRow,
  psCommandLine,
  runnerFrom,
  useRowGuard,
} from "./deadline";
import { temporaryDirectory } from "./temporary-directories";

const dir = temporaryDirectory("test-fixtures-deadline");

test("a bun test command line names the runner and its --timeout in either form", () => {
  expect(runnerFrom(["bun", "test", "--timeout=777", "./a.test.ts"])).toEqual({
    inBunTest: true,
    guardMs: 777,
  });
  expect(runnerFrom(["bun", "test", "./a.test.ts", "--timeout", "778"])).toEqual({
    inBunTest: true,
    guardMs: 778,
  });
  expect(runnerFrom(["bun", "test", "--timeout=1", "--timeout=779"])).toEqual({
    inBunTest: true,
    guardMs: 779,
  });
});

test("a --parallel worker's command line names the runner and the guard it was handed", () => {
  // As Bun 1.4.4 starts a worker of `bun test --parallel --timeout 777 --preload ./p.ts`.
  const worker = [
    "/usr/local/bin/bun",
    "test",
    "--test-worker",
    "--isolate",
    "--timeout=777",
    "--max-concurrency=20",
    "--preload",
    "./p.ts",
  ];
  expect(runnerFrom(worker)).toEqual({ inBunTest: true, guardMs: 777 });
});

test("a bun test command line with no --timeout has Bun's default guard, and 0 is none", () => {
  expect(runnerFrom(["bun", "test", "src/tests", "tests"])).toEqual({
    inBunTest: true,
    guardMs: 5_000,
  });
  expect(runnerFrom(["bun", "--smol", "test"])).toEqual({ inBunTest: true, guardMs: 5_000 });
  expect(runnerFrom(["bun", "test", "--timeout", "0"])).toEqual({ inBunTest: true, guardMs: 0 });
});

test("a command line that runs a script is no bun test", () => {
  expect(runnerFrom(["bun", "./driver.ts", "test"]).inBunTest).toBe(false);
  expect(runnerFrom(["bun", "run", "test"]).inBunTest).toBe(false);
  expect(runnerFrom(["/usr/local/bin/bun"]).inBunTest).toBe(false);
});

test("a --timeout that is not milliseconds is refused", () => {
  expect(() => runnerFrom(["bun", "test", "--timeout=soon"])).toThrow(
    'the command line\'s --timeout is "soon", not milliseconds'
  );
});

test("a plain bun test with no --timeout takes the package's declared guard", () => {
  expect(runnerFrom(["bun", "test", "./a.test.ts"], 300_000)).toEqual({
    inBunTest: true,
    guardMs: 300_000,
  });
  // No declared guard still falls back to Bun's default.
  expect(runnerFrom(["bun", "test", "./a.test.ts"])).toEqual({ inBunTest: true, guardMs: 5_000 });
});

test("the declared guard wins over any command-line --timeout", () => {
  // The preload's setDefaultTimeout wins over --timeout for Bun's own kill, so the deadline the
  // waits end by follows the declared guard too, whatever the command line carries.
  expect(runnerFrom(["bun", "test", "--timeout", "5000", "./a.test.ts"], 300_000)).toEqual({
    inBunTest: true,
    guardMs: 300_000,
  });
});

test("a --parallel worker's injected --timeout=5000 does not override the declared guard", () => {
  const worker = [
    "bun",
    "test",
    "--test-worker",
    "--isolate",
    "--timeout=5000",
    "--max-concurrency=20",
  ];
  expect(runnerFrom(worker, 300_000)).toEqual({ inBunTest: true, guardMs: 300_000 });
  // With no declared guard the worker's command line still gives its guard.
  expect(runnerFrom(worker)).toEqual({ inBunTest: true, guardMs: 5_000 });
});

test("a package that declares no guard takes the command line's --timeout, unchanged", () => {
  // test-fixtures and pi-envoy declare no guard (they run non-parallel, where a preload's
  // setDefaultTimeout would reach only the first file), so their command's --timeout decides, as on
  // main. Bun's default stands when the command sets none.
  expect(runnerFrom(["bun", "test", "--timeout", "30000", "./a.test.ts"])).toEqual({
    inBunTest: true,
    guardMs: 30_000,
  });
  expect(runnerFrom(["bun", "test", "--timeout=250000"])).toEqual({
    inBunTest: true,
    guardMs: 250_000,
  });
  expect(runnerFrom(["bun", "test", "./a.test.ts"])).toEqual({ inBunTest: true, guardMs: 5_000 });
});

test("a package declares its guard only inside a --parallel worker, not a non-parallel run", () => {
  // pi-legion's test preload calls declareGuard only where isParallelWorker is true, so a plain
  // `bun test` (and a --parallel run of one file, whose parent takes no worker) keeps main's 5000 ms.
  const worker = ["bun", "test", "--test-worker", "--isolate", "--timeout=5000"];
  expect(isParallelWorker(worker)).toBe(true);
  expect(isParallelWorker(["bun", "test", "./a.test.ts"])).toBe(false);
  expect(isParallelWorker(["bun", "test", "--parallel=4", "./a.test.ts"])).toBe(false);
});

test("useRowGuard gives a row its own deadline; a row without it gets the command's guard", () => {
  // deadlineFor reads live module state: the row-deadline preload's markRow reset the row guard
  // before this row, this row sets and then forgets it, and the last mark leaves the next row bare.
  expect(deadlineFor(performance.now(), "a bare row").source).toContain("(bun test --timeout)");
  useRowGuard(123_456);
  expect(deadlineFor(performance.now(), "a cold row").source).toContain("123456 ms (useRowGuard)");
  markRow();
  expect(deadlineFor(performance.now(), "the next row").source).toContain("(bun test --timeout)");
  expect(() => useRowGuard(0)).toThrow("useRowGuard needs whole milliseconds above zero");
});

// `psCommandLine` is the reading of a host with no `/proc` (macOS). Here it asks a stand-in for
// `ps` that answers the one call it makes as procps-ng does, from that one process's
// `/proc/<pid>/cmdline` (its words joined by spaces), and refuses any other: a real `ps -p` reads
// every process on the machine, about a second a call on a loaded box (docs/testing.md, "A test
// that looks for a process it started"). The two are compared through what the runner reads, since
// `ps` joins the words by spaces: an argument that holds a space (`-t "a row"`) splits in two there
// and nowhere else.
test.skipIf(!existsSync("/proc/self/cmdline"))(
  "ps's line reads as the runner and guard that /proc reads",
  () => {
    const standIn = join(dir, "ps-stand-in");
    writeFileSync(
      standIn,
      [
        "#!/bin/sh",
        'if [ "$#" -ne 5 ] || [ "$1" != -ww ] || [ "$2" != -o ] || [ "$3" != args= ] || [ "$4" != -p ]; then',
        '  echo "the ps stand-in was asked: $*" >&2',
        "  exit 2",
        "fi",
        "tr '\\0' ' ' <\"/proc/$5/cmdline\"",
        "echo",
        "",
      ].join("\n"),
      { mode: 0o755 }
    );
    const proc = readFileSync("/proc/self/cmdline", "utf8")
      .split("\0")
      .filter((arg) => arg !== "");
    expect(runnerFrom(proc).inBunTest).toBe(true);
    expect(runnerFrom(psCommandLine(process.pid, standIn))).toEqual(runnerFrom(proc));
  }
);
