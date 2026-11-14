import { type FSWatcher, watch } from "node:fs"
import { open } from "node:fs/promises"

/**
 * What a Codex session's transcript says, one line at a time.
 *
 * Codex appends each session to one JSON-lines file as it runs (`<Codex home>/sessions/<date>/
 * rollout-<time>-<session id>.jsonl`; every hook is handed its path as `transcript_path`). Each
 * line is `{timestamp, type, payload}`. This reads the few kinds the Codex plugin needs - a turn's
 * start and end, the model and effort a turn ran with, what the person typed, what the model
 * wrote, and each tool call and its output - and ignores every other kind, so a Codex release
 * that adds one changes nothing here. Measured on Codex CLI 0.154.0 (INFRA-3558).
 */
export type CodexTranscriptEvent =
  | { readonly kind: "turn-started"; readonly turnId: string; readonly at: number }
  | { readonly kind: "turn-ended"; readonly turnId: string | undefined; readonly at: number }
  | {
      readonly kind: "settings"
      readonly model: string
      readonly effort: string | null
      readonly at: number
    }
  | { readonly kind: "user"; readonly id: string; readonly text: string; readonly at: number }
  | { readonly kind: "assistant"; readonly id: string; readonly text: string; readonly at: number }
  | { readonly kind: "reasoning"; readonly id: string; readonly text: string; readonly at: number }
  | {
      readonly kind: "tool-call"
      readonly callId: string
      readonly name: string
      readonly arguments: string
      readonly at: number
    }
  | {
      readonly kind: "tool-result"
      readonly callId: string
      readonly output: string
      readonly at: number
    }

type Json = Record<string, unknown>

function record(value: unknown): Json | undefined {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Json)
    : undefined
}

function text(value: unknown): string | undefined {
  return typeof value === "string" ? value : undefined
}

/** The text of a content list: every part that carries a `text` string, joined by newlines. */
function contentText(content: unknown): string {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  const pieces: string[] = []
  for (const part of content) {
    const piece = text(record(part)?.["text"])
    if (piece !== undefined) pieces.push(piece)
  }
  return pieces.join("\n")
}

/** A tool call's output as text: Codex writes a string, or a list of content parts. */
function outputText(output: unknown): string {
  if (typeof output === "string") return output
  const asRecord = record(output)
  if (asRecord !== undefined) return contentText(asRecord["content"] ?? asRecord["body"])
  return contentText(output)
}

/** The line's own timestamp in milliseconds, or `fallback` when it has none Codex wrote. */
function lineTime(line: Json, fallback: number): number {
  const stamp = text(line["timestamp"])
  const parsed = stamp === undefined ? Number.NaN : Date.parse(stamp)
  return Number.isFinite(parsed) ? parsed : fallback
}

/**
 * The events one transcript line holds: none for a line this reader does not use or cannot
 * parse. `now` stands in for a line with no timestamp of its own.
 */
export function transcriptEvents(raw: string, now: number = Date.now()): CodexTranscriptEvent[] {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return []
  }
  const line = record(parsed)
  const payload = record(line?.["payload"])
  if (line === undefined || payload === undefined) return []
  const at = lineTime(line, now)
  const kind = text(payload["type"])
  switch (line["type"]) {
    case "event_msg": {
      if (kind === "task_started") {
        const turnId = text(payload["turn_id"])
        return turnId === undefined ? [] : [{ at, kind: "turn-started", turnId }]
      }
      if (kind === "task_complete" || kind === "turn_aborted") {
        return [{ at, kind: "turn-ended", turnId: text(payload["turn_id"]) }]
      }
      // What the person sent, as Codex's own record of the user turn: the model-facing user
      // messages beside it also carry the environment and instruction blocks Codex adds.
      if (kind === "item_completed") {
        const item = record(payload["item"])
        const id = text(item?.["id"])
        if (item?.["type"] === "UserMessage" && id !== undefined) {
          return [{ at, id, kind: "user", text: contentText(item["content"]) }]
        }
      }
      return []
    }
    case "turn_context": {
      const model = text(payload["model"])
      if (model === undefined) return []
      const settings = record(record(payload["collaboration_mode"])?.["settings"])
      const effort = text(settings?.["reasoning_effort"]) ?? text(payload["effort"]) ?? null
      return [{ at, effort, kind: "settings", model }]
    }
    case "response_item": {
      if (kind === "message" && payload["role"] === "assistant") {
        const id = text(payload["id"]) ?? `assistant-${at}`
        return [{ at, id, kind: "assistant", text: contentText(payload["content"]) }]
      }
      if (kind === "reasoning") {
        const summary = contentText(payload["summary"])
        if (summary === "") return []
        return [
          { at, id: text(payload["id"]) ?? `reasoning-${at}`, kind: "reasoning", text: summary },
        ]
      }
      if (kind === "function_call" || kind === "custom_tool_call") {
        const callId = text(payload["call_id"])
        if (callId === undefined) return []
        const args = payload["arguments"] ?? payload["input"]
        return [
          {
            arguments: typeof args === "string" ? args : JSON.stringify(args ?? {}),
            at,
            callId,
            kind: "tool-call",
            name: text(payload["name"]) ?? "tool",
          },
        ]
      }
      if (kind === "function_call_output" || kind === "custom_tool_call_output") {
        const callId = text(payload["call_id"])
        if (callId === undefined) return []
        return [{ at, callId, kind: "tool-result", output: outputText(payload["output"]) }]
      }
      return []
    }
    default:
      return []
  }
}

export interface TranscriptFollower {
  /** Stops reading; the callback is not called again. */
  stop(): void
  /** Resolves once everything the file held when the follower started has been handed over. */
  readonly caughtUp: Promise<void>
}

/** How often a follower looks at the file again when no change notification arrived. */
const TRANSCRIPT_POLL_MS = 500

/**
 * Hands every line of the transcript at `path` to `onLine`, from its first line, then each line
 * Codex appends, whole lines only: a line still being written waits for its newline. A file that
 * does not exist yet is waited for. Change notifications wake it early; a poll covers a
 * filesystem that sends none.
 */
export function followTranscript(
  path: string,
  onLine: (line: string) => void,
  options: { readonly pollMs?: number; readonly warn?: (text: string) => void } = {},
): TranscriptFollower {
  const pollMs = options.pollMs ?? TRANSCRIPT_POLL_MS
  let offset = 0
  let partial = ""
  let stopped = false
  let reading = false
  let again = false
  let watcher: FSWatcher | undefined
  const caughtUp = Promise.withResolvers<void>()
  const decoder = new TextDecoder()

  const read = async (): Promise<void> => {
    if (stopped) return
    if (reading) {
      again = true
      return
    }
    reading = true
    try {
      do {
        again = false
        let handle: Awaited<ReturnType<typeof open>> | undefined
        try {
          handle = await open(path, "r")
        } catch {
          return
        }
        try {
          const { size } = await handle.stat()
          if (size < offset) {
            // The file was replaced or cut: read it again from the start.
            offset = 0
            partial = ""
          }
          while (offset < size && !stopped) {
            const buffer = new Uint8Array(Math.min(size - offset, 1 << 20))
            const { bytesRead } = await handle.read(buffer, 0, buffer.length, offset)
            if (bytesRead === 0) break
            offset += bytesRead
            partial += decoder.decode(buffer.subarray(0, bytesRead), { stream: true })
            let newline = partial.indexOf("\n")
            while (newline >= 0) {
              const line = partial.slice(0, newline)
              partial = partial.slice(newline + 1)
              if (line.trim() !== "" && !stopped) onLine(line)
              newline = partial.indexOf("\n")
            }
          }
          caughtUp.resolve()
        } finally {
          await handle.close()
        }
        if (watcher === undefined && !stopped) {
          try {
            watcher = watch(path, () => void read())
            watcher.on("error", () => undefined)
          } catch {
            // No change notifications here: the poll below reads the file anyway.
          }
        }
      } while (again && !stopped)
    } catch (error) {
      options.warn?.(`could not read the Codex transcript ${path}: ${String(error)}`)
    } finally {
      reading = false
    }
  }

  const timer = setInterval(() => void read(), pollMs)
  timer.unref?.()
  void read()
  return {
    caughtUp: caughtUp.promise,
    stop: () => {
      stopped = true
      clearInterval(timer)
      watcher?.close()
      caughtUp.resolve()
    },
  }
}
