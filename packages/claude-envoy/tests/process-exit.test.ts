// The channel server's process ends by exitProcess once its run returns (bin/envoy-channel.ts): a
// handle nothing will use can outlive the run, such as a dial a stop ended. What it already wrote
// to stdout (an MCP reply, which Claude Code reads there) and to stderr must reach the reader all
// the same, even when the reader is slow and the pipe is full.
import { expect, test } from "bun:test"
import { existsSync } from "node:fs"
import { join, resolve } from "node:path"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"

const helper = resolve(import.meta.dir, "..", "src", "process-exit.ts")
const MIB = 1024 * 1024
const makeDirectory = temporaryDirectories("claude-envoy-process-exit")

test("a process ended by exitProcess first writes out everything queued on stdout and stderr", async () => {
  // Written once the child has queued both writes and asked to exit: from then on the pipe is full
  // and the rest waits in the writer until a reader takes it.
  const exiting = join(makeDirectory(), "exiting")
  const script = `
    import { writeFileSync } from "node:fs"
    import { exitProcess } from ${JSON.stringify(helper)}
    process.stdout.write("o".repeat(${MIB}))
    process.stderr.write("e".repeat(${MIB}))
    // A handle nothing will use, as a dial a stop ended is: the exit must not wait it out. It
    // outlasts the row's timeout, so an exit that waits for it fails there.
    setTimeout(() => undefined, 60_000)
    const exited = exitProcess(0)
    writeFileSync(${JSON.stringify(exiting)}, "")
    await exited
  `
  const child = Bun.spawn(["bun", "-e", script], { stdout: "pipe", stderr: "pipe" })
  // A reader that starts only once the exit is asked for. A child that exits first, as one whose
  // exit drops what it queued does, ends the wait too, and the lengths below say what it dropped.
  while (!existsSync(exiting) && child.exitCode === null && child.signalCode === null) {
    await Bun.sleep(10)
  }
  const [out, err, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ])
  expect(code).toBe(0)
  expect(out.length).toBe(MIB)
  expect(err.length).toBe(MIB)
}, 30_000)
