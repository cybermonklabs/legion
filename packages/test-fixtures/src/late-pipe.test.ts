import { describe, expect, test } from "bun:test";
import { skipWithout } from "./host-tools";
import { readThroughLatePipe } from "./late-pipe";

const noPython = skipWithout("python3");
const BYTES = 200_000;
/** A Bun program's one write of BYTES bytes, which each case ends its own way. */
const WRITE = `process.stdout.write("x".repeat(${BYTES - 1}) + "\\n");`;

describe.skipIf(noPython)("readThroughLatePipe", () => {
  test("hands a program a one-page pipe, so process.exit after a write past it reaches the reader cut", async () => {
    const run = await readThroughLatePipe([process.execPath, "-e", `${WRITE} process.exit(0);`], {
      lateMs: 1_000,
    });
    expect(run.pipeBytes).toBe(4_096);
    expect(run.exitCode).toBe(0);
    expect(run.stdout.length).toBeLessThan(BYTES);
  }, 30_000);

  // The case above holds the pipe's size, not its reader's lateness: a process.exit right after a
  // write past a page is cut even by a reader that reads at once. The consumers' cases rest on the
  // reader being late, so the reader reports when it began to read, on its own clock from the
  // command's start, and that must come no earlier than lateMs: no length of a wait decides it,
  // and a reader that reads at once fails it on any machine.
  test("begins to read no earlier than lateMs after it started the command", async () => {
    const run = await readThroughLatePipe([process.execPath, "-e", `${WRITE} process.exit(0);`], {
      lateMs: 1_000,
    });
    expect(run.firstReadMs).toBeGreaterThanOrEqual(1_000);
  }, 30_000);

  test("gets every byte of a program that lets its writes drain before it ends", async () => {
    const run = await readThroughLatePipe(
      [process.execPath, "-e", `${WRITE} process.exitCode = 3;`],
      { lateMs: 1_000 }
    );
    expect(run.exitCode).toBe(3);
    expect(run.stdout.length).toBe(BYTES);
  }, 30_000);

  test("kills a program held mid-write once its pipe is full, and reads only what the pipe held", async () => {
    // The write past a page waits on the reader, and the timer keeps the program from ending.
    const killed = await readThroughLatePipe(
      [process.execPath, "-e", `${WRITE} setTimeout(() => undefined, 60_000);`],
      { killWhenFullMs: 100 }
    );
    expect(killed.killedWhenFull).toBe(true);
    expect(killed.exitCode).toBeNull();
    expect(killed.stdout.length).toBe(killed.pipeBytes);
    // A program that ends before its pipe is full is read as it ends, never killed.
    const ended = await readThroughLatePipe(
      [process.execPath, "-e", 'process.stdout.write("x\\n");'],
      { killWhenFullMs: 100 }
    );
    expect(ended.killedWhenFull).toBe(false);
    expect(ended.exitCode).toBe(0);
    expect(new TextDecoder().decode(ended.stdout)).toBe("x\n");
  }, 30_000);

  test("hands the command its stdin, and kills one still running at the timeout", async () => {
    const echoed = await readThroughLatePipe(["cat"], { lateMs: 0, stdin: "fed\n" });
    expect(new TextDecoder().decode(echoed.stdout)).toBe("fed\n");
    const hung = await readThroughLatePipe(["sleep", "30"], { lateMs: 0, timeoutMs: 500 });
    expect(hung.exitCode).toBeNull();
  }, 30_000);
});
