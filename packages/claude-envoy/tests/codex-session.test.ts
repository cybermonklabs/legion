import { describe, expect, test } from "bun:test"
import { join } from "node:path"
import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery"
import type { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import type { ChannelConnection, ChannelTopicSubscription } from "../src/channel-topics"
import { startCodexAgentStream } from "../src/codex/codex-agent-stream"
import type { CodexInboxItem } from "../src/codex/codex-inbox"
import {
  codexHome,
  codexPluginDataDirectory,
  codexProcessId,
  codexSessionRecordFile,
  readCodexSessionRecord,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"
import { CODEX_COMMAND_REFUSAL, createCodexUserTurnRouter } from "../src/codex/codex-user-turns"
import { readUserTurnState, userTurnState, userTurnStateFile } from "../src/user-turn-state"

const directory = temporaryDirectory("codex-session")

describe("the Codex session record", () => {
  test("is written whole and read back; a missing or foreign file reads as none", async () => {
    const file = codexSessionRecordFile(directory, 4242)
    expect(file).toBe(join(directory, "codex-sessions", "4242.json"))
    expect(await readCodexSessionRecord(file)).toBeUndefined()
    const record = {
      codex_executable: "/opt/codex",
      codex_home: "/home/a/.codex",
      cwd: "/work",
      session_id: "01a1",
      source: "startup",
      transcript_path: "/home/a/.codex/sessions/r.jsonl",
    }
    await writeCodexSessionRecord(file, record)
    expect(await readCodexSessionRecord(file)).toEqual(record)
    await Bun.write(file, '{"session_id": ""}')
    expect(await readCodexSessionRecord(file)).toBeUndefined()
  })

  test("the plugin's state directory follows from where Codex installed it", () => {
    expect(codexPluginDataDirectory("/h/.codex/plugins/cache/legion-plugins/claude-envoy/1.2.3")).toBe(
      "/h/.codex/plugins/data/claude-envoy-legion-plugins",
    )
    expect(codexPluginDataDirectory("/data/checkout/packages/claude-envoy")).toBeUndefined()
  })

  test("Codex's home is CODEX_HOME, else ~/.codex", () => {
    expect(codexHome({ CODEX_HOME: "/x", HOME: "/h" })).toBe("/x")
    expect(codexHome({ CODEX_HOME: "", HOME: "/h" })).toBe("/h/.codex")
  })

  test("the codex process is the nearest ancestor named codex, else the parent", () => {
    const parents = new Map([
      [process.ppid, 500],
      [500, 400],
    ])
    const names = new Map([[500, "codex"]])
    expect(
      codexProcessId(
        (pid) => names.get(pid),
        (pid) => parents.get(pid),
      ),
    ).toBe(500)
    expect(
      codexProcessId(
        () => "bash",
        () => undefined,
      ),
    ).toBe(process.ppid)
  })
})

/** A connection that records what is published and hands control messages to the stream. */
function fakeConnection() {
  const published: { subject: string; payload: unknown }[] = []
  const queues: { push(message: { data: Uint8Array; reply?: string }): void }[] = []
  const connection = {
    publish(subject: string, data: Uint8Array) {
      published.push({ payload: JSON.parse(new TextDecoder().decode(data)), subject })
    },
    subscribe(): ChannelTopicSubscription {
      const pending: { data: Uint8Array; reply?: string }[] = []
      let wake: (() => void) | undefined
      let closed = false
      queues.push({
        push(message) {
          pending.push(message)
          wake?.()
        },
      })
      const iterator = {
        async *[Symbol.asyncIterator]() {
          while (!closed) {
            const next = pending.shift()
            if (next !== undefined) yield next
            else await new Promise<void>((resolve) => (wake = resolve))
          }
        },
      }
      return Object.assign(iterator, {
        unsubscribe() {
          closed = true
          wake?.()
        },
      }) as unknown as ChannelTopicSubscription
    },
  } as unknown as ChannelConnection
  const control = (type: string, reply?: string): void => {
    for (const queue of queues) {
      queue.push({
        data: new TextEncoder().encode(JSON.stringify({ type })),
        ...(reply === undefined ? {} : { reply }),
      })
    }
  }
  return { connection, control, published }
}

describe("the Codex live view", () => {
  test("replays the transcript's conversation, tagged rows and settings once a viewer watches", async () => {
    const { connection, control, published } = fakeConnection()
    let clock = 1_000
    const stream = startCodexAgentStream({ connection, now: () => clock++, sessionId: "s1" })
    stream.event({ at: 10, effort: null, kind: "settings", model: "gpt-x" })
    stream.expect("ship it", "message-1")
    stream.event({ at: 11, id: "u1", kind: "user", text: "ship it" })
    stream.event({ at: 12, id: "a1", kind: "assistant", text: "on it" })
    stream.event({ arguments: '{"cmd":"ls"}', at: 13, callId: "c1", kind: "tool-call", name: "exec_command" })
    stream.event({ at: 14, callId: "c1", kind: "tool-result", output: "README.md" })
    stream.delivered("mid-turn words", "message-2")
    control("watch")
    await Bun.sleep(10)
    control("replay", "_INBOX.1")
    await Bun.sleep(10)
    const reply = published.find((entry) => entry.subject === "_INBOX.1")?.payload as {
      frames: { kind: string; message?: Record<string, unknown>; result?: Record<string, unknown> }[]
    }
    const messages = reply.frames.filter((frame) => frame.kind === "message").map((frame) => frame.message)
    expect(messages.map((message) => message?.["role"])).toEqual(["user", "assistant", "assistant", "user"])
    expect(messages[0]).toMatchObject({ dispatchMessageId: "message-1" })
    expect(messages[1]).toMatchObject({ model: "openai/gpt-x" })
    expect(messages[2]?.["parts"]).toEqual([
      { argsText: '{\n  "cmd": "ls"\n}', toolCallId: "c1", toolName: "exec_command", type: "tool-call" },
    ])
    expect(messages[3]).toMatchObject({ dispatchMessageId: "message-2" })
    expect(reply.frames.find((frame) => frame.kind === "tool-result")?.result).toMatchObject({
      output: "README.md",
      toolCallId: "c1",
      toolName: "exec_command",
    })
    expect(reply.frames.at(-1)).toMatchObject({ kind: "settings", model: "openai/gpt-x", reported: true })
    stream.stop()
  })

  test("a replay before any watch hands out nothing", async () => {
    const { connection, control, published } = fakeConnection()
    const stream = startCodexAgentStream({ connection, sessionId: "s2" })
    stream.event({ at: 1, id: "u1", kind: "user", text: "hello" })
    control("replay", "_INBOX.2")
    await Bun.sleep(10)
    expect(published.find((entry) => entry.subject === "_INBOX.2")?.payload).toMatchObject({ frames: [] })
    stream.stop()
  })
})

function frame(mode: "steer" | "btw", body: string): RenderInboundResult {
  const delivery = {
    attempt: 1,
    body,
    broadcastId: undefined,
    id: "33333333-3333-4333-8333-333333333333",
    issueKey: null,
    mode,
    resource: "message",
  } as unknown as DispatchDelivery
  return {
    content: body,
    delivery,
    dispatchActor: { id: "alice", kind: "user" },
    envelope: { event_id: "evt-1", source: "dispatch" },
  } as unknown as RenderInboundResult
}

describe("the Codex router of a person's messages", () => {
  async function harness(body: string) {
    const stateFile = userTurnStateFile(directory, `router-${Math.random()}`)
    const delivered: CodexInboxItem[] = []
    const replies: { body?: string; error?: string }[] = []
    const expected: string[] = []
    const asked: string[] = []
    const router = createCodexUserTurnRouter({
      ask: async (question) => {
        asked.push(question)
        return `answer to ${question}`
      },
      deliver: async (item) => {
        delivered.push(item)
      },
      dispatch: () =>
        ({
          acceptMessageDelivery: async (id: string) => ({ body, delivery: "steer", message_id: id }),
        }) as unknown as DispatchClient,
      expect: (text, id) => expected.push(`${id}:${text}`),
      reply: async (_delivery, result) => {
        replies.push(result)
      },
      sessionId: () => "s1",
      state: userTurnState(stateFile, await readUserTurnState(stateFile)),
      warn: () => undefined,
    })
    return { asked, delivered, expected, replies, router }
  }

  test("a Send puts the body Dispatch stored into the session as the person's message", async () => {
    const { delivered, expected, router } = await harness("the stored words")
    expect(await router.route(frame("steer", "the frame's words"))).toBe(true)
    expect(delivered).toEqual([
      { content: "the stored words", dispatchMessageId: "33333333-3333-4333-8333-333333333333", eventId: "evt-1" },
    ])
    expect(expected).toEqual(["33333333-3333-4333-8333-333333333333:the stored words"])
  })

  test("a BTW is answered in a side question and posted back, writing nothing into the session", async () => {
    const { asked, delivered, replies, router } = await harness("unused")
    expect(await router.route(frame("btw", "which file?"))).toBe(true)
    await Bun.sleep(10)
    expect(asked).toEqual(["which file?"])
    expect(replies).toEqual([{ body: "answer to which file?" }])
    expect(delivered).toEqual([])
  })

  test("a / message is answered that it was not run", async () => {
    const { delivered, replies, router } = await harness("/model gpt-y")
    expect(await router.route(frame("steer", "/model gpt-y"))).toBe(true)
    await Bun.sleep(10)
    expect(replies).toEqual([{ body: `/model was not run: ${CODEX_COMMAND_REFUSAL}` }])
    expect(delivered).toEqual([])
  })
})
