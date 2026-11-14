import { expect, test } from "bun:test";
import { existsSync, readFileSync } from "node:fs";
import { psCommandLine, runnerFrom } from "./deadline";
import { skipWithout } from "./host-tools";

// The comparison needs both readings; a host without `/proc` is the one `ps` stands in for.
const withoutBothReadings = skipWithout("ps") || !existsSync("/proc/self/cmdline");

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

// `ps` prints the arguments joined by spaces, so it is compared through what the runner reads
// from it: an argument that holds a space (`-t "a row"`) splits in two there and nowhere else.
test.skipIf(withoutBothReadings)("ps reads the runner and guard that /proc reads", () => {
  const proc = readFileSync("/proc/self/cmdline", "utf8")
    .split("\0")
    .filter((arg) => arg !== "");
  expect(runnerFrom(proc).inBunTest).toBe(true);
  expect(runnerFrom(psCommandLine(process.pid))).toEqual(runnerFrom(proc));
});
