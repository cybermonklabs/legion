import { describe, expect, test } from "bun:test";
import { ignoresSignal, sleepRuns, until } from "./held-process";

describe("sleepRuns", () => {
  // A real `sleep 281` of this test's own, a command line no other test of this repository has,
  // so the read is of a process /proc names rather than of a stand-in for it.
  test("answers true while `sleep <seconds>` runs and false once it is gone", async () => {
    const sleep = Bun.spawn(["sleep", "281"], { stdout: "ignore", stderr: "ignore" });
    try {
      expect(await until(() => sleepRuns(sleep.pid, "281"), 10_000)).toBe(true);
      expect(sleepRuns(sleep.pid, "282")).toBe(false);
      sleep.kill("SIGKILL");
      await sleep.exited;
      expect(sleepRuns(sleep.pid, "281")).toBe(false);
    } finally {
      if (sleep.exitCode === null && sleep.signalCode === null) sleep.kill("SIGKILL");
    }
  });

  test("answers false for a process running another command line", () => {
    expect(sleepRuns(process.pid, "281")).toBe(false);
  });

  test("answers false for a pid /proc does not name", () => {
    expect(sleepRuns(0, "281")).toBe(false);
    expect(sleepRuns(-1, "281")).toBe(false);
  });
});

describe("ignoresSignal", () => {
  // A real `sleep 281` started under a shell that ignores SIGHUP, which sleep inherits, and one
  // started with nothing ignored, the control.
  for (const row of [
    { ignore: "trap '' HUP; ", hup: true },
    { ignore: "", hup: false },
  ]) {
    test(`reads a process started ${row.hup ? "ignoring SIGHUP" : "ignoring nothing"} as ${row.hup ? "ignoring" : "not ignoring"} it, and SIGTERM as not ignored`, async () => {
      const sleep = Bun.spawn(["/bin/sh", "-c", `${row.ignore}exec sleep 281`], {
        stdout: "ignore",
        stderr: "ignore",
      });
      try {
        expect(await until(() => sleepRuns(sleep.pid, "281"), 10_000)).toBe(true);
        expect(ignoresSignal(sleep.pid, "SIGHUP")).toBe(row.hup);
        expect(ignoresSignal(sleep.pid, "SIGTERM")).toBe(false);
      } finally {
        sleep.kill("SIGKILL");
        await sleep.exited;
      }
      expect(ignoresSignal(sleep.pid, "SIGHUP")).toBe(false);
    });
  }
});

describe("until", () => {
  test("answers true once holds does", async () => {
    let reads = 0;
    expect(await until(() => ++reads === 3, 10_000)).toBe(true);
    expect(reads).toBe(3);
  });

  test("answers false when the time passes first", async () => {
    const started = performance.now();
    expect(await until(() => false, 200)).toBe(false);
    expect(performance.now() - started).toBeGreaterThanOrEqual(200);
  });
});
