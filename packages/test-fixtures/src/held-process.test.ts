import { describe, expect, test } from "bun:test";
import { ignoresSignal, skipWithoutIgnoreMasks } from "./held-process";

const withoutMasks = skipWithoutIgnoreMasks("packages/test-fixtures/src/held-process.test.ts");

describe("ignoresSignal", () => {
  // A real `sleep` started under a shell that ignores SIGHUP, which sleep inherits, and one
  // started with nothing ignored, the control. The shell prints a line once its trap is set, and
  // the mask is read after that line: an ignore the shell has set stays through its `exec`, so the
  // read is the same whether the pid is still the shell's or already sleep's.
  for (const row of [
    { ignore: "trap '' HUP; ", hup: true },
    { ignore: "", hup: false },
  ]) {
    test.skipIf(withoutMasks)(
      `reads a process started ${row.hup ? "ignoring SIGHUP" : "ignoring nothing"} as ${row.hup ? "ignoring" : "not ignoring"} it, and SIGTERM as not ignored`,
      async () => {
        const sleep = Bun.spawn(["/bin/sh", "-c", `${row.ignore}echo set; exec sleep 281`], {
          stdout: "pipe",
          stderr: "ignore",
        });
        try {
          const reader = sleep.stdout.getReader();
          const { value } = await reader.read();
          expect(new TextDecoder().decode(value)).toBe("set\n");
          expect(ignoresSignal(sleep.pid, "SIGHUP")).toBe(row.hup);
          expect(ignoresSignal(sleep.pid, "SIGTERM")).toBe(false);
        } finally {
          sleep.kill("SIGKILL");
          await sleep.exited;
        }
        expect(ignoresSignal(sleep.pid, "SIGHUP")).toBe(false);
      }
    );
  }
});

test("a pid /proc does not name reads as ignoring nothing", () => {
  expect(ignoresSignal(0, "SIGHUP")).toBe(false);
  expect(ignoresSignal(-1, "SIGHUP")).toBe(false);
});
