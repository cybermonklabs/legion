// exitProcess (`../process-exit`) is the way out of a Bun command whose last output must reach its
// reader: its answer, its usage or refusal line. What it already wrote to stdout and stderr must
// reach the reader all the same, even when the reader is slow and the pipe is full.
import { expect, test } from "bun:test";
import { closeSync, constants, createReadStream, existsSync, openSync } from "node:fs";
import { join, resolve } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { settled, until } from "@legion/test-fixtures/wait";

const helper = resolve(import.meta.dir, "..", "process-exit.ts");
const MIB = 1024 * 1024;
const scratch = temporaryDirectory("envoy-client-process-exit");

test("a process ended by exitProcess first writes out everything queued on stdout and stderr", async () => {
  // The child says when it has queued its output and reached exitProcess, with nobody reading
  // its pipes yet: the pipe is full, and the rest waits in the writer.
  const queued = join(scratch, "queued");
  const script = `
    import { writeFileSync } from "node:fs"
    import { exitProcess } from ${JSON.stringify(helper)}
    process.stdout.write("o".repeat(${MIB}))
    process.stderr.write("e".repeat(${MIB}))
    // A handle nothing will use, as a dial a stop ended is, and one that never ends: the exit
    // must not wait for it, so a child that exits at all did not.
    setInterval(() => undefined, 60_000)
    writeFileSync(${JSON.stringify(queued)}, "")
    await exitProcess(0)
  `;
  // This process's own Bun, by its path: PATH need not name a `bun` at all.
  const child = Bun.spawn([process.execPath, "-e", script], { stdout: "pipe", stderr: "pipe" });
  // A reader that starts only once the child has queued what it wrote and called exitProcess. A
  // child that exits first, as one whose exit drops what it queued does, ends the wait too, and
  // the lengths below say what it dropped.
  await until("the child's output queued, and exitProcess called", () => {
    return existsSync(queued) || child.exitCode !== null || child.signalCode !== null;
  });
  const [out, err, code] = await settled(
    "the child's output read whole and its exit",
    Promise.all([
      new Response(child.stdout).text(),
      new Response(child.stderr).text(),
      child.exited,
    ])
  );
  expect(code).toBe(0);
  expect(out.length).toBe(MIB);
  expect(err.length).toBe(MIB);
});

// An exit with a flush bound (a command's exit inside the grace of whatever stops it): the line
// that says why must reach a reader that takes it, and a reader that takes nothing must not keep
// the process past the bound, since what stops the process is close behind.

/**
 * A named pipe for a child's stderr, with a reader held open that reads nothing: a pipe nobody
 * drains fills, as Bun.spawn's own pipe does not (this process reads it as the child writes).
 * `release` closes that reader.
 */
function undrainedPipe(name: string): { readonly path: string; release(): void } {
  const path = join(scratch, name);
  const made = Bun.spawnSync(["mkfifo", path]);
  if (made.exitCode !== 0) throw new Error(`mkfifo ${path}: ${made.stderr.toString()}`);
  const idle = openSync(path, constants.O_RDONLY | constants.O_NONBLOCK);
  return { path, release: () => closeSync(idle) };
}

/** A child that queues a mebibyte and then the line on `stderr`, a pipe nobody drains, then calls
 *  exitProcess(1, flushMs): it says so in `queued` once it has. */
function childExitingOnABound(queued: string, stderr: string, flushMs: number) {
  const script = `
    import { writeFileSync } from "node:fs"
    import { exitProcess } from ${JSON.stringify(helper)}
    process.stderr.write("e".repeat(${MIB}))
    process.stderr.write("the line that says why\\n")
    setInterval(() => undefined, 60_000)
    writeFileSync(${JSON.stringify(queued)}, "")
    await exitProcess(1, ${flushMs})
  `;
  return Bun.spawn([process.execPath, "-e", script], {
    stdout: "ignore",
    stderr: Bun.file(stderr),
  });
}

test("an exit on a flush bound writes out what is queued for a reader that takes it", async () => {
  const queued = join(scratch, "queued-bounded");
  const pipe = undrainedPipe("stderr-read");
  try {
    // A bound past anything the reader below takes to start: what the row reads is that the exit
    // waits for the flush, not how long the flush took.
    const child = childExitingOnABound(queued, pipe.path, 600_000);
    await until("the child's output queued, and exitProcess called", () => existsSync(queued));
    const chunks: Buffer[] = [];
    const reader = createReadStream(pipe.path);
    reader.on("data", (chunk) => chunks.push(chunk as Buffer));
    const read = Promise.withResolvers<void>();
    reader.once("end", () => read.resolve());
    reader.once("error", read.reject);
    const [, code] = await settled(
      "the child's stderr read whole and its exit",
      Promise.all([read.promise, child.exited])
    );
    const err = Buffer.concat(chunks).toString();
    expect(code).toBe(1);
    expect(err.length).toBe(MIB + "the line that says why\n".length);
    expect(err.endsWith("the line that says why\n")).toBe(true);
  } finally {
    pipe.release();
  }
});

test("an exit on a flush bound exits on the bound when the reader takes nothing", async () => {
  const queued = join(scratch, "queued-unread");
  const pipe = undrainedPipe("stderr-unread");
  try {
    const child = childExitingOnABound(queued, pipe.path, 200);
    await until("the child's output queued, and exitProcess called", () => existsSync(queued));
    // Nobody reads the full pipe: the flush never ends, and only its bound lets the child exit.
    expect(await settled("the child's exit on its flush bound", child.exited)).toBe(1);
  } finally {
    pipe.release();
  }
});
