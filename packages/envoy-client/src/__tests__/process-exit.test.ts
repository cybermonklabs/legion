// A program that must end at once ends through exitProcess (the channel server, once its run
// returns: a handle nothing will use can outlive it), and one that must know whether its reader
// took its output writes it through writeOut (the Codex tool hook, which moves the messages it
// hands Codex on only then). What either wrote must reach the reader whole, however slow the
// reader and however full the pipe.
import { describe, expect, test } from "bun:test";
import { existsSync } from "node:fs";
import { join, resolve } from "node:path";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { readThroughLatePipe } from "@legion/test-fixtures/late-pipe";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const helper = JSON.stringify(resolve(import.meta.dir, "..", "process-exit.ts"));
const MIB = 1024 * 1024;
const BYTES = 200_000;
const makeDirectory = temporaryDirectories("envoy-client-process-exit");
const noPython = skipWithout("python3");

test("a process ended by exitProcess first writes out everything queued on stdout and stderr", async () => {
  // Written once the child has queued both writes and asked to exit: from then on the pipe is full
  // and the rest waits in the writer until a reader takes it.
  const exiting = join(makeDirectory(), "exiting");
  const script = `
    import { writeFileSync } from "node:fs";
    import { exitProcess } from ${helper};
    process.stdout.write("o".repeat(${MIB}));
    process.stderr.write("e".repeat(${MIB}));
    // A handle nothing will use, as a dial a stop ended is: the exit must not wait it out. It
    // outlasts the row's timeout, so an exit that waits for it fails there.
    setTimeout(() => undefined, 60_000);
    const exited = exitProcess(0);
    writeFileSync(${JSON.stringify(exiting)}, "");
    await exited;
  `;
  // This process's own Bun, by its path: PATH need not name a `bun` at all.
  const child = Bun.spawn([process.execPath, "-e", script], { stdout: "pipe", stderr: "pipe" });
  // A reader that starts only once the exit is asked for. A child that exits first, as one whose
  // exit drops what it queued does, ends the wait too, and the lengths below say what it dropped.
  while (!existsSync(exiting) && child.exitCode === null && child.signalCode === null) {
    await Bun.sleep(10);
  }
  const [out, err, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  expect(code).toBe(0);
  expect(out.length).toBe(MIB);
  expect(err.length).toBe(MIB);
}, 30_000);

// `Bun.spawn`'s own stdout is a socket with a 212,992-byte buffer, which the MiB above passes; a
// one-page pipe read late is the reader a loaded machine gives a hook or a `$(...)`.
describe.skipIf(noPython)("through a one-page pipe read late", () => {
  const write = `process.stdout.write("x".repeat(${BYTES - 1}) + "\\n");`;

  test("exitProcess hands the reader every byte, where process.exit hands it one page", async () => {
    // The child's timer is a handle nothing will use, as in the row above: the child cannot end by
    // itself, so the drain is exitProcess's alone.
    const drained = await readThroughLatePipe([
      process.execPath,
      "-e",
      `import { exitProcess } from ${helper}; ${write} setTimeout(() => undefined, 60_000); await exitProcess(4);`,
    ]);
    expect(drained.pipeBytes).toBe(4_096);
    expect(drained.exitCode).toBe(4);
    expect(drained.stdout.length).toBe(BYTES);
    // The control: the same write ended by process.exit reaches this reader cut.
    const cut = await readThroughLatePipe([process.execPath, "-e", `${write} process.exit(4);`]);
    expect(cut.exitCode).toBe(4);
    expect(cut.stdout.length).toBeLessThan(BYTES);
  }, 60_000);

  test("a write made after exitProcess cannot cut what came before, and a reader gone still ends the wait at once", async () => {
    // The late write comes in the same turn as the call, so it lands inside the wait however late
    // the child starts; the timer holds the child past this test's limit, so only the wait ends it.
    const late = `import { exitProcess } from ${helper}; ${write} setTimeout(() => undefined, 120_000); const exited = exitProcess(3); process.stdout.write("late\\n", (error) => process.stderr.write(String(error?.code))); await exited;`;
    const whole = await readThroughLatePipe([process.execPath, "-e", late]);
    expect(whole.exitCode).toBe(3);
    expect(whole.stdout.length).toBe(BYTES);
    // A reader that closed its end: EPIPE ends the wait, and the late write changes nothing.
    const gone = Bun.spawnSync(
      ["bash", "-c", 'set -o pipefail; "$0" -e "$1" | true', process.execPath, late],
      { stderr: "pipe", timeout: 30_000 }
    );
    expect(gone.exitCode).toBe(3);
  }, 60_000);

  test("writeOut answers true once the reader took every byte, and false when the reader is gone", async () => {
    const script = (then: string) =>
      `import { writeOut } from ${helper}; const whole = await writeOut(process.stdout, "x".repeat(${BYTES - 1}) + "\\n"); ${then}`;
    // The child exits the moment writeOut answers: an answer that came before the late reader took
    // every byte would drop what Bun still held, so the reader's count holds the answer's timing.
    const taken = await readThroughLatePipe([
      process.execPath,
      "-e",
      script("process.exit(whole ? 0 : 9);"),
    ]);
    expect(taken.exitCode).toBe(0);
    expect(taken.stdout.length).toBe(BYTES);
    // A reader that closed its end before the write: EPIPE, which the answer says.
    const gone = Bun.spawnSync(
      [
        "bash",
        "-c",
        '"$0" -e "$1" | true',
        process.execPath,
        script("process.stderr.write(String(whole));"),
      ],
      { stderr: "pipe" }
    );
    expect(gone.stderr.toString()).toBe("false");
  }, 60_000);
});
