// The channel server's process ends by exitProcess once its run returns (bin/envoy-channel.ts): a
// handle nothing will use can outlive the run, such as a dial a stop ended. What it already wrote
// to stdout (an MCP reply, which Claude Code reads there) and to stderr must reach the reader all
// the same, even when the reader is slow and the pipe is full.
import { expect, test } from "bun:test"
import { resolve } from "node:path"

const helper = resolve(import.meta.dir, "..", "src", "process-exit.ts")
const MIB = 1024 * 1024

test("a process ended by exitProcess first writes out everything queued on stdout and stderr", async () => {
  const script = `
    import { exitProcess } from ${JSON.stringify(helper)}
    process.stdout.write("o".repeat(${MIB}))
    process.stderr.write("e".repeat(${MIB}))
    // A handle nothing will use, as a dial a stop ended is: the exit must not wait it out.
    setTimeout(() => undefined, 60_000)
    await exitProcess(0)
  `
  const child = Bun.spawn(["bun", "-e", script], { stdout: "pipe", stderr: "pipe" })
  // A reader that is slow to start: the pipe fills, and the rest waits in the writer.
  await Bun.sleep(500)
  const reading = performance.now()
  const [out, err, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ])
  expect(code).toBe(0)
  expect(out.length).toBe(MIB)
  expect(err.length).toBe(MIB)
  expect(performance.now() - reading).toBeLessThan(10_000)
}, 30_000)
