import { spawn } from "node:child_process"
import { randomBytes } from "node:crypto"
import { mkdirSync, writeFileSync } from "node:fs"
import { join } from "node:path"

/**
 * The two things the Codex plugin asks of Codex itself (INFRA-3558): put a message into a running
 * session (`codex queue`), and answer a side question in a throwaway copy of the session's
 * conversation (`codex app-server`'s `thread/fork`). Both run the same `codex` binary the session
 * runs, against the session's own Codex home.
 */
export interface CodexCommand {
  /** The `codex` binary the session runs. */
  readonly executable: string
  /** The session's Codex home, which `codex queue` and the app server read the session from. */
  readonly codexHome: string
  /** The environment to run it in; `CODEX_HOME` is set over it. */
  readonly env: Readonly<Record<string, string | undefined>>
}

/** How long `codex queue` may take before it counts as failed. */
const QUEUE_TIMEOUT_MS = 30_000

function childEnv(command: CodexCommand): Record<string, string> {
  const env: Record<string, string> = {}
  for (const [name, value] of Object.entries(command.env)) {
    if (value !== undefined) env[name] = value
  }
  env["CODEX_HOME"] = command.codexHome
  return env
}

/**
 * Queues `message` for the session `threadId`: the Codex terminal running it takes the message
 * as a new turn of its own, a few seconds later when it is idle and once its running turn ends
 * otherwise (measured on Codex CLI 0.154.0). Resolves once Codex has stored it; rejects naming
 * why when `codex queue` fails.
 */
export function queueMessage(
  command: CodexCommand,
  threadId: string,
  text: string,
  spillDirectory?: string,
): Promise<void> {
  let message: string
  try {
    message = fitArgument(text, spillDirectory)
  } catch (error) {
    return Promise.reject(error)
  }
  return new Promise((resolve, reject) => {
    const child = spawn(command.executable, ["queue", "--thread", threadId, "--message", message], {
      env: childEnv(command),
      stdio: ["ignore", "pipe", "pipe"],
    })
    let output = ""
    child.stdout.on("data", (chunk: Buffer) => {
      output += chunk.toString()
    })
    child.stderr.on("data", (chunk: Buffer) => {
      output += chunk.toString()
    })
    const timer = setTimeout(() => child.kill("SIGKILL"), QUEUE_TIMEOUT_MS)
    child.on("error", (error) => {
      clearTimeout(timer)
      reject(new Error(`could not run ${command.executable} queue: ${error.message}`))
    })
    child.on("close", (code, signal) => {
      clearTimeout(timer)
      if (code === 0) resolve()
      else {
        reject(
          new Error(
            `${command.executable} queue exited ${signal ?? code}: ${output.trim().slice(-500)}`,
          ),
        )
      }
    })
  })
}

/** The longest message `codex queue` is handed as its argument. Linux refuses a single argument
 *  over 128 KiB (`MAX_ARG_STRLEN`) and the spawn would fail on every resend of the frame. */
const QUEUE_ARGUMENT_MAX_BYTES = 96 * 1024
/** How much of a longer message is quoted before the line naming the file holding all of it. */
const QUEUE_HEAD_CHARS = 8_000

/**
 * `text` as one argument `codex queue` can take: itself, or, past the bound, its start and a line
 * naming a file under `spillDirectory` that holds the whole of it. Throws when it is too long and
 * there is nowhere to put it.
 */
export function fitArgument(text: string, spillDirectory: string | undefined): string {
  if (Buffer.byteLength(text) <= QUEUE_ARGUMENT_MAX_BYTES) return text
  if (spillDirectory === undefined) {
    throw new Error(`the message is ${Buffer.byteLength(text)} bytes, over what codex queue takes`)
  }
  mkdirSync(spillDirectory, { mode: 0o700, recursive: true })
  const file = join(spillDirectory, `${Date.now()}-${randomBytes(4).toString("hex")}.txt`)
  writeFileSync(file, text, { mode: 0o600 })
  return `${text.slice(0, QUEUE_HEAD_CHARS)}\n\n[This message is ${Buffer.byteLength(text)} bytes long; the whole of it is in ${file}.]`
}

/** How long a side question may run before it is abandoned. */
const SIDE_QUESTION_TIMEOUT_MS = 5 * 60_000

interface JsonRpcMessage {
  readonly id?: number
  readonly method?: string
  readonly params?: Record<string, unknown>
  readonly result?: Record<string, unknown>
  readonly error?: { readonly message?: string }
}

/**
 * Answers `question` in a throwaway copy of the session `threadId`'s conversation and resolves
 * with what the model wrote. The copy runs in a `codex app-server` of its own with plugins and
 * hooks off (so it registers nothing and runs none of this plugin), a read-only sandbox and no
 * approvals (so it can read files and change nothing), and `ephemeral`, so Codex keeps no
 * transcript of it. The session's own conversation is left as it was.
 */
export function askSideQuestion(
  command: CodexCommand,
  threadId: string,
  question: string,
  timeoutMs: number = SIDE_QUESTION_TIMEOUT_MS,
): Promise<string> {
  return new Promise((resolve, reject) => {
    const child = spawn(
      command.executable,
      ["app-server", "--disable", "plugins", "--disable", "hooks"],
      { env: childEnv(command), stdio: ["pipe", "pipe", "pipe"] },
    )
    let settled = false
    let stderr = ""
    const finish = (error: Error | undefined, answer?: string): void => {
      if (settled) return
      settled = true
      clearTimeout(timer)
      child.kill("SIGTERM")
      if (error !== undefined) reject(error)
      else resolve(answer ?? "")
    }
    const timer = setTimeout(
      () => finish(new Error(`the side question had no answer after ${timeoutMs} ms`)),
      timeoutMs,
    )
    let next = 1
    const pending = new Map<number, (message: JsonRpcMessage) => void>()
    const call = (method: string, params: unknown): Promise<Record<string, unknown>> =>
      new Promise((done, fail) => {
        const id = next++
        pending.set(id, (message) => {
          if (message.error !== undefined) {
            fail(new Error(`${method}: ${message.error.message ?? "refused"}`))
          } else done(message.result ?? {})
        })
        child.stdin.write(`${JSON.stringify({ id, jsonrpc: "2.0", method, params })}\n`)
      })
    let forkId: string | undefined
    let turnId: string | undefined
    const answers: string[] = []
    let buffer = ""
    child.stdout.on("data", (chunk: Buffer) => {
      buffer += chunk.toString()
      let newline = buffer.indexOf("\n")
      while (newline >= 0) {
        const line = buffer.slice(0, newline)
        buffer = buffer.slice(newline + 1)
        newline = buffer.indexOf("\n")
        let message: JsonRpcMessage
        try {
          message = JSON.parse(line) as JsonRpcMessage
        } catch {
          continue
        }
        if (message.id !== undefined && message.method === undefined) {
          pending.get(message.id)?.(message)
          pending.delete(message.id)
          continue
        }
        const params = message.params ?? {}
        if (params["threadId"] !== forkId) continue
        if (message.method === "item/completed") {
          const item = params["item"] as Record<string, unknown> | undefined
          if (item?.["type"] === "agentMessage" && typeof item["text"] === "string") {
            answers.push(item["text"])
          }
        } else if (message.method === "turn/completed") {
          const turn = params["turn"] as Record<string, unknown> | undefined
          if (turnId === undefined || turn?.["id"] === turnId) {
            const status = turn?.["status"]
            if (status === "failed") {
              const error = turn?.["error"] as { message?: string } | null | undefined
              finish(
                new Error(
                  `the side question's turn failed: ${error?.message ?? "no reason given"}`,
                ),
              )
            } else finish(undefined, answers.join("\n\n"))
          }
        }
      }
    })
    child.stderr.on("data", (chunk: Buffer) => {
      stderr = (stderr + chunk.toString()).slice(-2000)
    })
    // A write to an app server that has died raises EPIPE here; its `close` says why it ended.
    child.stdin.on("error", () => undefined)
    child.on("error", (error) =>
      finish(new Error(`could not run ${command.executable} app-server: ${error.message}`)),
    )
    child.on("close", (code) => {
      finish(
        new Error(
          `${command.executable} app-server exited ${code} before answering: ${stderr.trim()}`,
        ),
      )
    })
    void (async () => {
      try {
        await call("initialize", { clientInfo: { name: "claude-envoy-btw", version: "1" } })
        child.stdin.write(`${JSON.stringify({ jsonrpc: "2.0", method: "initialized" })}\n`)
        const fork = await call("thread/fork", {
          approvalPolicy: "never",
          ephemeral: true,
          excludeTurns: true,
          sandbox: "read-only",
          threadId,
        })
        const thread = fork["thread"] as Record<string, unknown> | undefined
        forkId = typeof thread?.["id"] === "string" ? thread["id"] : undefined
        if (forkId === undefined) throw new Error("thread/fork answered no thread")
        const turn = await call("turn/start", {
          input: [{ text: question, text_elements: [], type: "text" }],
          threadId: forkId,
        })
        const started = turn["turn"] as Record<string, unknown> | undefined
        turnId = typeof started?.["id"] === "string" ? started["id"] : undefined
      } catch (error) {
        finish(error instanceof Error ? error : new Error(String(error)))
      }
    })()
  })
}
