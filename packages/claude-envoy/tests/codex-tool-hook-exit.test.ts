// The Codex PostToolUse hook takes the waiting messages out of the inbox before it prints them, so
// once it has taken them its stdout is the only place they are: an exit that drops what Bun still
// queues for a full pipe loses them with exit 0 and nothing said. The hook ends by exitProcess
// (`@legion/envoy-client/process-exit`), which waits for stdout to take them.
import { expect, test } from "bun:test"
import { closeSync, constants, createReadStream, openSync, writeSync } from "node:fs"
import { join, resolve } from "node:path"
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { settled, until } from "@legion/test-fixtures/wait"
import { codexInboxDirectory, putInbox, takenBy } from "../src/codex/codex-inbox"

const HOOK = resolve(import.meta.dir, "..", "hooks", "codex-tool-hook.ts")
const scratch = temporaryDirectory("codex-tool-hook-exit")

/**
 * A named pipe that is already full: a reader held open that reads nothing, and as many bytes as
 * the pipe takes, written before the hook starts. A write the hook makes then waits in Bun's
 * writer until someone reads. The test's own writer stays open until `closeWriter`, so a reader
 * opened after the hook has gone still opens, and sees the end only once the test closes it.
 * `release` closes the idle reader.
 */
function fullPipe(name: string): {
  readonly path: string
  readonly filled: number
  closeWriter(): void
  release(): void
} {
  const path = join(scratch, name)
  const made = Bun.spawnSync(["mkfifo", path])
  if (made.exitCode !== 0) throw new Error(`mkfifo ${path}: ${made.stderr.toString()}`)
  const idle = openSync(path, constants.O_RDONLY | constants.O_NONBLOCK)
  const writer = openSync(path, constants.O_WRONLY | constants.O_NONBLOCK)
  const chunk = Buffer.alloc(4096, "f")
  let filled = 0
  try {
    for (;;) filled += writeSync(writer, chunk)
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "EAGAIN") throw error
  }
  let open = true
  const closeWriter = () => {
    if (open) closeSync(writer)
    open = false
  }
  return {
    path,
    filled,
    closeWriter,
    release: () => {
      closeWriter()
      closeSync(idle)
    },
  }
}

test("the tool hook's exit writes out the messages it took for a reader that starts after it took them", async () => {
  const pluginData = join(scratch, "plugin-data")
  const session = "ses_codex_exit"
  const directory = codexInboxDirectory(pluginData, session)
  const content = `a message the hook takes: ${"m".repeat(6_000)}`
  const name = await putInbox(directory, { content })
  const pipe = fullPipe("stdout")
  try {
    const hook = Bun.spawn(["bun", HOOK], {
      stdin: new Blob([JSON.stringify({ session_id: session })]),
      stdout: Bun.file(pipe.path),
      stderr: "pipe",
      env: { ...process.env, ...bunCacheOff, PLUGIN_DATA: pluginData },
    })
    // The message is out of the inbox: from here on the hook's stdout is the only place it is.
    await until(
      "the hook took the message",
      async () => (await takenBy(directory, name)) === "hook",
    )
    const chunks: Buffer[] = []
    const reader = createReadStream(pipe.path)
    reader.on("data", (chunk) => chunks.push(chunk as Buffer))
    const read = Promise.withResolvers<void>()
    reader.once("end", () => read.resolve())
    reader.once("error", read.reject)
    const [code, stderr] = await settled(
      "the hook's exit",
      Promise.all([hook.exited, new Response(hook.stderr).text()]),
    )
    // The hook is gone, so what it wrote is in the pipe or nowhere: the test's writer, closed
    // now, was the last one, and the reader reads to the end.
    pipe.closeWriter()
    await settled("the hook's stdout read to its end", read.promise)
    expect(stderr).toBe("")
    expect(code).toBe(0)
    const printed = Buffer.concat(chunks).subarray(pipe.filled).toString()
    expect(printed).toStartWith('{"hookSpecificOutput"')
    const answer = JSON.parse(printed) as {
      hookSpecificOutput: { additionalContext: string; hookEventName: string }
    }
    expect(answer.hookSpecificOutput.hookEventName).toBe("PostToolUse")
    expect(answer.hookSpecificOutput.additionalContext).toContain(content)
  } finally {
    pipe.release()
  }
})
