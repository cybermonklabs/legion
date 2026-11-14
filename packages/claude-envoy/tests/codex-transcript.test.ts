import { describe, expect, test } from "bun:test"
import { appendFileSync, readFileSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import {
  type CodexTranscriptEvent,
  followTranscript,
  transcriptEvents,
} from "../src/codex/codex-transcript"

/** A transcript Codex CLI 0.154.0 wrote for a session of two turns, the second running one shell
 *  command, with long instruction text cut and the scratch paths replaced. */
const FIXTURE = join(import.meta.dir, "fixtures", "codex", "rollout-0.154.0.jsonl")

const directory = temporaryDirectory("codex-transcript")

function eventsOf(path: string): CodexTranscriptEvent[] {
  return readFileSync(path, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .flatMap((line) => transcriptEvents(line, 0))
}

describe("transcriptEvents", () => {
  test("reads a real Codex transcript into turns, settings, messages and one tool call", () => {
    const events = eventsOf(FIXTURE)
    expect(events.map((event) => event.kind)).toEqual([
      "turn-started",
      "settings",
      "user",
      "assistant",
      "turn-ended",
      "turn-started",
      "settings",
      "user",
      "tool-call",
      "tool-result",
      "assistant",
      "turn-ended",
    ])
    const [, settings, user, assistant] = events
    expect(settings).toMatchObject({ effort: null, kind: "settings", model: "standin-model" })
    expect(user).toMatchObject({ kind: "user", text: "2" })
    expect(assistant).toMatchObject({ id: "msg_18", kind: "assistant", text: "ECHO: 2" })
    const call = events.find((event) => event.kind === "tool-call")
    expect(call).toEqual({
      arguments: '{"cmd":"echo step"}',
      at: Date.parse("2026-11-13T23:59:18.668Z"),
      callId: "call_20",
      kind: "tool-call",
      name: "exec_command",
    })
    const result = events.find((event) => event.kind === "tool-result")
    expect(result).toMatchObject({ callId: "call_20", kind: "tool-result" })
    expect(result?.kind === "tool-result" && result.output).toContain("Output:\nstep\n")
  })

  test("the model-facing user messages Codex adds (environment, instructions) are not the person's", () => {
    const users = eventsOf(FIXTURE).filter((event) => event.kind === "user")
    expect(users.map((event) => (event.kind === "user" ? event.text : ""))).toEqual([
      "2",
      "RUN: echo step",
    ])
  })

  test("a reasoning summary is read, an empty or encrypted one is not", () => {
    const line = (summary: unknown): string =>
      JSON.stringify({
        payload: { encrypted_content: "x", id: "rs_1", summary, type: "reasoning" },
        timestamp: "2026-11-14T00:00:00.000Z",
        type: "response_item",
      })
    expect(transcriptEvents(line([{ text: "thinking it over", type: "summary_text" }]))).toEqual([
      {
        at: Date.parse("2026-11-14T00:00:00.000Z"),
        id: "rs_1",
        kind: "reasoning",
        text: "thinking it over",
      },
    ])
    expect(transcriptEvents(line([]))).toEqual([])
  })

  test("a turn the person interrupted ends the turn", () => {
    const line = JSON.stringify({
      payload: { reason: "interrupted", turn_id: "t1", type: "turn_aborted" },
      timestamp: "2026-11-14T00:00:00.000Z",
      type: "event_msg",
    })
    expect(transcriptEvents(line)).toEqual([
      { at: Date.parse("2026-11-14T00:00:00.000Z"), kind: "turn-ended", turnId: "t1" },
    ])
  })

  test("a line that is not JSON, or a kind it does not use, yields nothing", () => {
    expect(transcriptEvents("{not json")).toEqual([])
    expect(transcriptEvents(JSON.stringify({ payload: {}, type: "world_state" }))).toEqual([])
    expect(transcriptEvents(JSON.stringify({ type: "event_msg" }))).toEqual([])
  })
})

describe("followTranscript", () => {
  test("waits for the file, reads it from the start, then each whole line appended", async () => {
    const path = join(directory, "rollout.jsonl")
    const lines: string[] = []
    const follower = followTranscript(path, (line) => lines.push(line), { pollMs: 20 })
    try {
      await Bun.sleep(60)
      expect(lines).toEqual([])
      writeFileSync(path, '{"a":1}\n{"b":')
      await follower.caughtUp
      await Bun.sleep(60)
      expect(lines).toEqual(['{"a":1}'])
      appendFileSync(path, '2}\n{"c":3}\n')
      await Bun.sleep(100)
      expect(lines).toEqual(['{"a":1}', '{"b":2}', '{"c":3}'])
    } finally {
      follower.stop()
    }
  })

  test("hands over nothing once stopped", async () => {
    const path = join(directory, "stopped.jsonl")
    writeFileSync(path, '{"a":1}\n')
    const lines: string[] = []
    const follower = followTranscript(path, (line) => lines.push(line), { pollMs: 20 })
    await follower.caughtUp
    follower.stop()
    appendFileSync(path, '{"b":2}\n')
    await Bun.sleep(80)
    expect(lines).toEqual(['{"a":1}'])
  })
})
