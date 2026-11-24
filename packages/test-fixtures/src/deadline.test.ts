import { expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { psCommandLine, runnerFrom } from "./deadline";
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
