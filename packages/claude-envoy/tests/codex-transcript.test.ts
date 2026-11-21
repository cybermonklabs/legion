import { describe, expect, test } from "bun:test"
import { appendFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { span, until } from "@legion/test-fixtures/wait"
import {
  type CodexTranscriptEvent,
  findTranscript,
  followTranscript,
  latestTurnCwd,
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
  // The exact lines at the end hold what came before them too: the half line `{"b":` handed before
  // its newline would stand among them.
  test("waits for the file, reads it from the start, then each whole line appended", async () => {
    const path = join(directory, "rollout.jsonl")
    const lines: string[] = []
    const follower = followTranscript(path, (line) => lines.push(line), { pollMs: 20 })
    try {
      await span(
        60,
        "a follower of a missing file keeps no state a test can read, and it looks again on its " +
          "own poll timer, so no later action's effect comes after its looks: the span gives it " +
          "its first look and two polls, before the file it waits for appears",
      )
      expect(lines).toEqual([])
      writeFileSync(path, '{"a":1}\n{"b":')
      await follower.caughtUp
      expect(lines).toEqual(['{"a":1}'])
      appendFileSync(path, '2}\n{"c":3}\n')
      await until("the lines appended after the first read", () => lines.length >= 3)
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
    await span(
      80,
      "a stopped follower keeps no state a test can read, and the read that would hand the line " +
        "comes from its own poll timer or a change notification of the filesystem, so no later " +
        "action's effect comes after it: the span gives four of its polls and the notification " +
        "their turn",
    )
    expect(lines).toEqual(['{"a":1}'])
  })
})

describe("latestTurnCwd", () => {
  const root = temporaryDirectory("codex-turn-cwd")

  test("names the directory of the newest turn, which /cd moves", async () => {
    const file = join(root, "rollout.jsonl")
    const turn = (cwd: string): string =>
      JSON.stringify({ payload: { cwd, model: "m" }, timestamp: "t", type: "turn_context" })
    writeFileSync(
      file,
      `${[
        turn("/start"),
        JSON.stringify({ payload: { type: "message" }, type: "response_item" }),
        turn("/moved"),
        JSON.stringify({ payload: { text: 'a "turn_context" in prose' }, type: "event_msg" }),
      ].join("\n")}\n`,
    )
    expect(await latestTurnCwd(file)).toBe("/moved")
  })

  test("names nothing for a missing file, no file, or a transcript with no turn yet", async () => {
    const empty = join(root, "empty.jsonl")
    writeFileSync(empty, "")
    expect(await latestTurnCwd(join(root, "missing.jsonl"))).toBeUndefined()
    expect(await latestTurnCwd(null)).toBeUndefined()
    expect(await latestTurnCwd(empty)).toBeUndefined()
  })
})

describe("latestTurnCwd on a long turn", () => {
  const root = temporaryDirectory("codex-turn-cwd-long")

  test("finds the newest turn however much was written after it", async () => {
    const file = join(root, "rollout.jsonl")
    const turn = JSON.stringify({ payload: { cwd: "/deep", model: "m" }, type: "turn_context" })
    const filler = JSON.stringify({ payload: { text: "y".repeat(1000) }, type: "event_msg" })
    writeFileSync(file, `${turn}\n${Array.from({ length: 2000 }, () => filler).join("\n")}\n`)
    expect(await latestTurnCwd(file)).toBe("/deep")
  })

  test("finds a turn whose line is longer than one read", async () => {
    const file = join(root, "long-line.jsonl")
    const turn = JSON.stringify({
      payload: { cwd: "/long", model: "m", summary: "z".repeat(600 * 1024) },
      type: "turn_context",
    })
    writeFileSync(file, `${turn}\n${JSON.stringify({ type: "event_msg" })}\n`)
    expect(await latestTurnCwd(file)).toBe("/long")
  })
})

describe("findTranscript", () => {
  const home = temporaryDirectory("codex-find-transcript")

  test("finds a thread's transcript under the Codex home, and nothing for another thread", async () => {
    const day = join(home, "sessions", "2026", "11", "18")
    mkdirSync(day, { recursive: true })
    const file = join(day, "rollout-2026-11-18T04-44-41-01a1ecf8-823b-78b3.jsonl")
    writeFileSync(file, "")
    expect(await findTranscript(home, "01a1ecf8-823b-78b3")).toBe(file)
    expect(await findTranscript(home, "01a1ffff-0000-0000")).toBeNull()
    expect(await findTranscript(home, "../escape")).toBeNull()
  })
})
