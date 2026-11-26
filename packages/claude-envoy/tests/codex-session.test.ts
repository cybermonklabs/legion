import { describe, expect, setDefaultTimeout, test } from "bun:test"
import { chmodSync, existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs"
import { join } from "node:path"
import { agentStreamControlSubject, IDEMPOTENCY_KEY_COMMITTED } from "@legion/contracts"
import type { DispatchDelivery, RenderInboundResult } from "@legion/envoy-client/delivery"
import { type DispatchClient, DispatchWriteLandedError } from "@legion/envoy-client/dispatch-http"
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import type { Subprocess } from "bun"
import { sessionAdmission } from "../src/channel-connection"
import type { ChannelConnection, ChannelTopicSubscription } from "../src/channel-topics"
import { startCodexAgentStream } from "../src/codex/codex-agent-stream"
import { codexProcessEnv, fitArgument, queueMessage } from "../src/codex/codex-cli"
import type { CodexInboxItem } from "../src/codex/codex-inbox"
import { callingThread } from "../src/codex/codex-server"
import {
  codexHome,
  codexPluginDataDirectory,
  codexProcessId,
  codexSessionRecordFile,
  codexSubcommand,
  processStarted,
  pruneCodexSessionRecords,
  readCodexSessionRecord,
  recordOfProcess,
  sharedAppServer,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"
import { CODEX_COMMAND_REFUSAL, createCodexUserTurnRouter } from "../src/codex/codex-user-turns"
import { readUserTurnState, userTurnState, userTurnStateFile } from "../src/user-turn-state"
import { FakeNats, waitFor } from "./channel-session-harness"

// The rows wait on files, processes or local servers, which a loaded machine slows. Their bound
// is a hang guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS)

const directory = temporaryDirectory("codex-session")

describe("the Codex session record", () => {
  test("is written whole and read back; a missing or foreign file reads as none", async () => {
    const file = codexSessionRecordFile(directory, 4242)
    expect(file).toBe(join(directory, "codex-sessions", "4242.json"))
    expect(await readCodexSessionRecord(file)).toBeUndefined()
    const record = {
      codex_executable: "/opt/codex",
      codex_home: "/home/a/.codex",
      codex_started: "123",
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

  test("a record is this process's only when it names this process's start", () => {
    const record = {
      codex_executable: null,
      codex_home: "/h",
      cwd: "/w",
      codex_started: processStarted(process.pid) ?? null,
      session_id: "s",
      source: null,
      transcript_path: null,
    }
    expect(record.codex_started).not.toBeNull()
    expect(recordOfProcess(record, process.pid)).toBe(true)
    expect(recordOfProcess({ ...record, codex_started: "1" }, process.pid)).toBe(false)
    // A record whose start was not read, or a process whose start cannot be read, is not taken.
    expect(recordOfProcess({ ...record, codex_started: null }, process.pid)).toBe(false)
    const exited = Bun.spawnSync(["true"]).pid
    expect(recordOfProcess(record, exited)).toBe(false)
  })

  test("Codex's shared app server is told apart from a codex process of its own", async () => {
    // Each shell waits on a read of its stdin, a pipe nothing writes, so it neither execs anything
    // in its own place, which would drop app-server from its arguments, nor ends before the test
    // kills it. `-c` takes a value for codex as for sh, so the subcommand is app-server. Both start
    // before either is waited for, so the `finally` kills each of them whichever wait fails.
    const sharedArgv = ["sh", "-c", "read line; :", "app-server"]
    const ownArgv = ["sh", "-c", "read line; :"]
    const shared = Bun.spawn(sharedArgv, { stdin: "pipe" })
    const own = Bun.spawn(ownArgv, { stdin: "pipe" })
    try {
      await untilListedAs(shared, sharedArgv)
      await untilListedAs(own, ownArgv)
      expect(sharedAppServer(shared.pid)).toBe(true)
      expect(sharedAppServer(own.pid)).toBe(false)
    } finally {
      shared.kill()
      own.kill()
    }
  })

  test("the subcommand is the first argument that is neither an option nor an option's value", () => {
    expect(codexSubcommand(["app-server", "--listen", "unix://"])).toBe("app-server")
    expect(codexSubcommand(["-c", "model=x", "app-server"])).toBe("app-server")
    expect(codexSubcommand(["-C", "app-server"])).toBeUndefined()
    expect(codexSubcommand(["resume", "app-server"])).toBe("resume")
    expect(codexSubcommand(["fix", "the", "app-server", "tests"])).toBe("fix")
    expect(codexSubcommand(["--dangerously-bypass-hook-trust"])).toBeUndefined()
    expect(codexSubcommand(["agents", "--no-daemon"])).toBe("agents")
  })

  test("the records of codex processes that exited are pruned, and a live one's is kept", async () => {
    const state = join(directory, "prune")
    const exited = Bun.spawnSync(["true"]).pid
    const record = {
      codex_executable: null,
      codex_home: "/h",
      codex_started: null,
      cwd: "/w",
      session_id: "s",
      source: null,
      transcript_path: null,
    }
    await writeCodexSessionRecord(codexSessionRecordFile(state, exited), record)
    await writeCodexSessionRecord(codexSessionRecordFile(state, process.pid), record)
    await pruneCodexSessionRecords(state)
    expect(readdirSync(join(state, "codex-sessions")).sort()).toEqual([`${process.pid}.json`])
  })

  test("a message too long for one argument is spilled to a file the queued line names", () => {
    expect(fitArgument("short", undefined)).toBe("short")
    const long = "x".repeat(200_000)
    expect(() => fitArgument(long, undefined)).toThrow("over what codex queue takes")
    const fitted = fitArgument(long, join(directory, "spill"))
    expect(Buffer.byteLength(fitted)).toBeLessThan(10_000)
    const file = /the whole of it is in (.+)\.\]$/.exec(fitted)?.[1]
    expect(file).toBeDefined()
    expect(readFileSync(file ?? "", "utf8")).toBe(long)
  })

  test("the plugin's state directory follows from where Codex installed it", () => {
    expect(
      codexPluginDataDirectory("/h/.codex/plugins/cache/legion-plugins/claude-envoy/1.2.3"),
    ).toBe("/h/.codex/plugins/data/claude-envoy-legion-plugins")
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

/** Whether this machine lists its processes under /proc, as Linux does. */
const HAS_PROC = existsSync("/proc/self/cmdline")

/** Whether the machine lists process `pid` as running `argv`: exactly, from /proc/<pid>/cmdline,
 *  or where there is no /proc, from the space-separated list `ps` prints. */
function listedAs(pid: number, argv: readonly string[]): boolean {
  if (HAS_PROC) {
    try {
      return readFileSync(`/proc/${pid}/cmdline`, "utf8") === argv.map((arg) => `${arg}\0`).join("")
    } catch {
      return false
    }
  }
  const ps = Bun.spawnSync(["ps", "-o", "args=", "-p", String(pid)])
  return ps.exitCode === 0 && ps.stdout.toString().trim() === argv.join(" ")
}

/**
 * Waits until the machine lists `child` as running `argv`, the arguments it was started with. On
 * Linux, Bun.spawn returns from a vfork while the child is still inside its execve, and until that
 * exec has laid out the new arguments, /proc/<pid>/cmdline reads the test process's own arguments
 * or nothing at all: right after a spawn, on a loaded box, the read was empty in most of a
 * thousand tries. The child tells nobody when its exec is done, so the wait polls. The row's own
 * deadline is its only bound, and a child that ends first fails the row by name.
 */
async function untilListedAs(child: Subprocess, argv: readonly string[]): Promise<void> {
  while (!listedAs(child.pid, argv)) {
    if (child.exitCode !== null || child.signalCode !== null) {
      throw new Error(
        `${JSON.stringify(argv)} ended (${child.exitCode ?? child.signalCode}) before the machine listed it as running`,
      )
    }
    await Bun.sleep(5)
  }
}

/** A connection that records what is published, the subjects subscribed and given up, and hands
 *  control messages to the stream. `closeForGood` closes it as nats.js closes one for good. */
function fakeConnection() {
  const published: { subject: string; payload: unknown }[] = []
  const queues: { push(message: { data: Uint8Array; reply?: string }): void }[] = []
  const subscribed = new Set<string>()
  let closedForGood = false
  const connection = {
    isClosed: () => closedForGood,
    publish(subject: string, data: Uint8Array) {
      published.push({ payload: JSON.parse(new TextDecoder().decode(data)), subject })
    },
    subscribe(subject: string): ChannelTopicSubscription {
      subscribed.add(subject)
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
          subscribed.delete(subject)
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
  return {
    connection,
    control,
    published,
    subscribed,
    closeForGood: () => {
      closedForGood = true
    },
  }
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
    stream.event({
      arguments: '{"cmd":"ls"}',
      at: 13,
      callId: "c1",
      kind: "tool-call",
      name: "exec_command",
    })
    stream.event({ at: 14, callId: "c1", kind: "tool-result", output: "README.md" })
    stream.delivered("mid-turn words", "message-2")
    control("watch")
    await Bun.sleep(10)
    control("replay", "_INBOX.1")
    await Bun.sleep(10)
    const reply = published.find((entry) => entry.subject === "_INBOX.1")?.payload as {
      frames: {
        kind: string
        message?: Record<string, unknown>
        result?: Record<string, unknown>
      }[]
    }
    const messages = reply.frames
      .filter((frame) => frame.kind === "message")
      .map((frame) => frame.message)
    expect(messages.map((message) => message?.["role"])).toEqual([
      "user",
      "assistant",
      "assistant",
      "user",
    ])
    expect(messages[0]).toMatchObject({ dispatchMessageId: "message-1" })
    expect(messages[1]).toMatchObject({ model: "openai/gpt-x" })
    expect(messages[2]?.["parts"]).toEqual([
      {
        argsText: '{\n  "cmd": "ls"\n}',
        toolCallId: "c1",
        toolName: "exec_command",
        type: "tool-call",
      },
    ])
    expect(messages[3]).toMatchObject({ dispatchMessageId: "message-2" })
    expect(reply.frames.find((frame) => frame.kind === "tool-result")?.result).toMatchObject({
      output: "README.md",
      toolCallId: "c1",
      toolName: "exec_command",
    })
    expect(reply.frames.at(-1)).toMatchObject({
      kind: "settings",
      model: "openai/gpt-x",
      reported: true,
    })
    stream.stop()
  })

  test("a replay before any watch hands out nothing", async () => {
    const { connection, control, published } = fakeConnection()
    const stream = startCodexAgentStream({ connection, sessionId: "s2" })
    stream.event({ at: 1, id: "u1", kind: "user", text: "hello" })
    control("replay", "_INBOX.2")
    await Bun.sleep(10)
    expect(published.find((entry) => entry.subject === "_INBOX.2")?.payload).toMatchObject({
      frames: [],
    })
    stream.stop()
  })

  // Under a broker token the connection is admitted as one session at a time, and the server
  // refuses and closes a subscribe to a subject the session it is admitted as may not take. These
  // run the channel server's own `sessionAdmission`, which `admit`s each id when its move lands.
  // The live view starts after the session does, which can be after a `/new` whose move has not
  // landed: its first control subject waits for the admission as every later one does.
  test("a live view started for an id the connection is not admitted as yet takes its control subject once it is", async () => {
    const { connection, subscribed } = fakeConnection()
    const admission = sessionAdmission("s1-before-new")
    const stream = startCodexAgentStream({
      admitted: admission.admitted,
      connection,
      sessionId: "s2",
    })
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([])
    admission.admit("s2")
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([agentStreamControlSubject("s2")])
    stream.stop()
  })

  test("a /new takes the new id's control subject only once the connection is admitted as it", async () => {
    const { connection, subscribed } = fakeConnection()
    const admission = sessionAdmission("s3")
    const stream = startCodexAgentStream({
      admitted: admission.admitted,
      connection,
      sessionId: "s3",
    })
    // The first subject goes through the gate too, which answers an id already admitted at once.
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([agentStreamControlSubject("s3")])
    stream.rebind("s4")
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([])
    admission.admit("s4")
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([agentStreamControlSubject("s4")])
    stream.stop()
  })

  test("a second /new before the first id is admitted takes only the latest id's subject", async () => {
    const { connection, subscribed } = fakeConnection()
    const admission = sessionAdmission("s5")
    const stream = startCodexAgentStream({
      admitted: admission.admitted,
      connection,
      sessionId: "s5",
    })
    stream.rebind("s6")
    stream.rebind("s7")
    admission.admit("s6")
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([])
    admission.admit("s7")
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([agentStreamControlSubject("s7")])
    stream.stop()
  })

  test("an admission that lands for an id the session has left takes nothing", async () => {
    // An `admitted` that settles every wait it handed out, the stale one included: the stream
    // takes only the id it is on now.
    const { connection, subscribed } = fakeConnection()
    const waits = new Map<string, PromiseWithResolvers<void>>()
    const stream = startCodexAgentStream({
      admitted: (session) => {
        const wait = Promise.withResolvers<void>()
        waits.set(session, wait)
        return wait.promise
      },
      connection,
      sessionId: "s8",
    })
    stream.rebind("s9")
    stream.rebind("s10")
    waits.get("s9")?.resolve()
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([])
    waits.get("s10")?.resolve()
    await Bun.sleep(10)
    expect([...subscribed]).toEqual([agentStreamControlSubject("s10")])
    stream.stop()
  })

  // A connection that closes for good while a `/new`'s move is still to land is replaced by one
  // that may still not take the new id's subjects, so the stream takes them through the same gate.
  test("on a connection swapped in while a /new's move is pending, the new id's control subject waits for the admission", async () => {
    const first = fakeConnection()
    const admission = sessionAdmission("s11")
    const stream = startCodexAgentStream({
      admitted: admission.admitted,
      connection: first.connection,
      sessionId: "s11",
    })
    stream.rebind("s12")
    first.closeForGood()
    const swapped = fakeConnection()
    stream.useConnection(swapped.connection)
    await Bun.sleep(10)
    expect([...swapped.subscribed]).toEqual([])
    admission.admit("s12")
    await Bun.sleep(10)
    expect([...swapped.subscribed]).toEqual([agentStreamControlSubject("s12")])
    expect([...first.subscribed]).toEqual([])
    stream.stop()
  })

  test("on a connection swapped in for an admitted id, the control subject is taken at once, and frames go out on it", async () => {
    const first = fakeConnection()
    const stream = startCodexAgentStream({
      admitted: sessionAdmission("s13").admitted,
      connection: first.connection,
      sessionId: "s13",
    })
    first.closeForGood()
    const swapped = fakeConnection()
    stream.useConnection(swapped.connection)
    await Bun.sleep(10)
    expect([...swapped.subscribed]).toEqual([agentStreamControlSubject("s13")])
    swapped.control("watch")
    await Bun.sleep(10)
    stream.event({ at: 1, id: "u1", kind: "user", text: "after the swap" })
    expect(swapped.published.length).toBeGreaterThan(0)
    stream.stop()
  })

  // A control subscription can end while the connection stays up (the server refused it): the
  // view takes the subject again a second later (`../src/control-subscription`), and takes nothing
  // for an id it moved off, or once it stopped, while that retake waits.
  test("a control subscription that ends on a live connection is taken again a second later, and not after a /new or a stop", async () => {
    const nats = new FakeNats()
    const stream = startCodexAgentStream({ connection: nats, sessionId: "s15", warn: () => {} })
    const subject = agentStreamControlSubject("s15")
    const renewed = agentStreamControlSubject("s16")
    const takes = (taken: string): number =>
      nats.calls.filter((call) => call === `nats.subscribe ${taken}`).length
    const refusal = Object.assign(new Error("Permissions Violation for Subscription"), {
      name: "NatsError",
      code: "PERMISSIONS_VIOLATION",
    })
    expect(takes(subject)).toBe(1)

    // Ended on a live connection: taken again about a second later.
    const endedAt = performance.now()
    nats.failSubscription(subject, refusal)
    await waitFor(async () => takes(subject) === 2, "the control subject taken again")
    expect(performance.now() - endedAt).toBeGreaterThanOrEqual(900)
    expect(nats.liveSubscriptions(subject)).toBe(1)

    // Ended again, and a `/new` lands while the retake waits: the old id is not taken again.
    nats.failSubscription(subject, refusal)
    await Bun.sleep(100)
    stream.rebind("s16")
    expect(takes(renewed)).toBe(1)
    await Bun.sleep(1_500)
    expect(takes(subject)).toBe(2)
    expect(takes(renewed)).toBe(1)

    // Ended again, and the view stops while the retake waits: nothing is taken again.
    nats.failSubscription(renewed, refusal)
    await Bun.sleep(100)
    stream.stop()
    await Bun.sleep(1_500)
    expect(takes(renewed)).toBe(1)
    expect(nats.liveSubscriptions(renewed)).toBe(0)
  }, 15_000)
})

/** The Dispatch message every frame of these rows delivers. */
const MESSAGE = "33333333-3333-4333-8333-333333333333"

/** A reply to MESSAGE Dispatch made and gave no copy of its answer for: what every reply of a
 *  harness built with `repliesLand` throws, and whose message the line about it carries. */
const REPLY_LANDED = new DispatchWriteLandedError(
  IDEMPOTENCY_KEY_COMMITTED,
  409,
  "POST",
  `https://dispatch.test/api/v1/messages/${MESSAGE}/reply`,
)

function frame(mode: "steer" | "btw", body: string): RenderInboundResult {
  const delivery = {
    attempt: 1,
    body,
    broadcastId: undefined,
    id: MESSAGE,
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
  async function harness(
    body: string,
    options: {
      /** Every reply is one Dispatch made and gave no copy of its answer for: it is recorded,
       *  then thrown (`REPLY_LANDED`). */
      readonly repliesLand?: boolean
      /** What the accept throws instead of answering with `body`. */
      readonly acceptThrows?: Error
    } = {},
  ) {
    const stateFile = userTurnStateFile(directory, `router-${Math.random()}`)
    const delivered: CodexInboxItem[] = []
    const replies: { body?: string; error?: string }[] = []
    const expected: string[] = []
    const asked: string[] = []
    const warnings: string[] = []
    /** Settles at the first warning: the last thing a BTW or a `/` reply that failed does. */
    const warned = Promise.withResolvers<void>()
    const router = createCodexUserTurnRouter({
      ask: async (question) => {
        asked.push(question)
        return `answer to ${question}`
      },
      deliver: async (item) => {
        delivered.push(item)
      },
      dispatch: async () =>
        ({
          acceptMessageDelivery: async (id: string) => {
            if (options.acceptThrows !== undefined) throw options.acceptThrows
            return { body, delivery: "steer", message_id: id }
          },
        }) as unknown as DispatchClient,
      expect: (text, id) => expected.push(`${id}:${text}`),
      reply: async (_delivery, result) => {
        replies.push(result)
        if (options.repliesLand) throw REPLY_LANDED
      },
      sessionId: () => "s1",
      state: userTurnState(stateFile, await readUserTurnState(stateFile)),
      warn: (text) => {
        warnings.push(text)
        warned.resolve()
      },
    })
    return { asked, delivered, expected, replies, router, warned: warned.promise, warnings }
  }

  test("a Send puts the body Dispatch stored into the session as the person's message", async () => {
    const { delivered, expected, router } = await harness("the stored words")
    expect(await router.route(frame("steer", "the frame's words"))).toBe(true)
    expect(delivered).toEqual([
      {
        content: "the stored words",
        dispatchMessageId: "33333333-3333-4333-8333-333333333333",
        eventId: "evt-1",
      },
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

  // A reply Dispatch made and gave no copy of its answer for answered the attempt: an error reply
  // after it would answer the person a second time with the error of a write that was made, and
  // the line says it landed, never that it did not reach Dispatch.
  test("a BTW answer that landed with no answer back is followed by no error reply", async () => {
    const { replies, router, warned, warnings } = await harness("unused", { repliesLand: true })
    expect(await router.route(frame("btw", "which file?"))).toBe(true)
    await warned
    expect(replies).toEqual([{ body: "answer to which file?" }])
    expect(warnings).toEqual([
      `a BTW answer to message ${MESSAGE} landed on Dispatch, which gave no copy of its answer — ${REPLY_LANDED.message}`,
    ])
  })

  test("a reply to a / message that landed with no answer back is not said to have missed Dispatch", async () => {
    const { replies, router, warned, warnings } = await harness("/model gpt-y", {
      repliesLand: true,
    })
    expect(await router.route(frame("steer", "/model gpt-y"))).toBe(true)
    await warned
    expect(replies).toEqual([{ body: `/model was not run: ${CODEX_COMMAND_REFUSAL}` }])
    expect(warnings).toEqual([
      `a reply to message ${MESSAGE} landed on Dispatch, which gave no copy of its answer — ${REPLY_LANDED.message}`,
    ])
  })

  // An accept Dispatch recorded and gave no copy of its answer for spent the attempt as this
  // session's, and the stored body a turn is made of is not in hand: the frame keeps its card, and
  // the line says the accept was recorded and why no turn follows, never that Dispatch did not
  // accept it.
  test("an accept that landed with no answer back keeps the card, saying the accept was recorded", async () => {
    const landed = new DispatchWriteLandedError(
      IDEMPOTENCY_KEY_COMMITTED,
      409,
      "POST",
      `https://dispatch.test/api/v1/messages/${MESSAGE}/deliveries/1/accept`,
    )
    const { delivered, replies, router, warnings } = await harness("unused", {
      acceptThrows: landed,
    })
    expect(await router.route(frame("steer", "ship it"))).toBe(false)
    expect(delivered).toEqual([])
    expect(replies).toEqual([])
    expect(warnings).toEqual([
      `Dispatch recorded this session's accept of message ${MESSAGE} and gave no copy of its answer (${landed.message}), so no turn follows; it arrives as a card`,
    ])
  })
})

describe("the environment a side question and codex queue run in", () => {
  test("is the codex process's own, read from /proc, so a model provider's env_key reaches it", () => {
    const read = (path: string): string => {
      expect(path).toBe("/proc/4242/environ")
      return "HOME=/home/José\0MODEL_GATEWAY_TOKEN=t=1\0EMPTY=\0"
    }
    expect(codexProcessEnv(4242, { HOME: "/server" }, read)).toEqual({
      EMPTY: "",
      HOME: "/home/José",
      MODEL_GATEWAY_TOKEN: "t=1",
    })
  })

  test("is the server's own where /proc cannot be read or holds nothing", () => {
    const fallback = { HOME: "/server" }
    const refuse = (): string => {
      throw new Error("ENOENT")
    }
    expect(codexProcessEnv(4242, fallback, refuse)).toBe(fallback)
    expect(codexProcessEnv(4242, fallback, () => "")).toBe(fallback)
  })

  test("reads a process's environment from /proc on Linux, non-ASCII values intact", async () => {
    if (process.platform !== "linux") return
    const child = Bun.spawn(["sh", "-c", "sleep 5; :"], { env: { NAME: "José", PATH: "/bin" } })
    try {
      await Bun.sleep(50)
      expect(codexProcessEnv(child.pid, {})["NAME"]).toBe("José")
    } finally {
      child.kill()
    }
  })
})

describe("codex queue", () => {
  const root = temporaryDirectory("codex-queue-argv")
  test("gets the message as one --message= argument, so a text that starts with - is not an option", async () => {
    const argvFile = join(root, "argv")
    const fake = join(root, "codex")
    writeFileSync(fake, `#!/bin/sh\nfor a in "$@"; do printf '%s\\0' "$a"; done > '${argvFile}'\n`)
    chmodSync(fake, 0o755)
    await queueMessage({ codexHome: root, env: {}, executable: fake }, "thread-1", "- item\n- two")
    expect(readFileSync(argvFile, "utf8").split("\0").slice(0, -1)).toEqual([
      "queue",
      "--thread",
      "thread-1",
      "--message=- item\n- two",
    ])
  })
})

describe("the Codex thread a tool call comes from", () => {
  test("is the _meta threadId Codex sends, else its turn metadata's thread_id", () => {
    expect(callingThread({ threadId: "t-1", "x-codex-turn-metadata": { thread_id: "t-2" } })).toBe(
      "t-1",
    )
    expect(callingThread({ "x-codex-turn-metadata": { thread_id: "t-2" } })).toBe("t-2")
    expect(callingThread({ progressToken: 1 })).toBeUndefined()
    expect(callingThread(undefined)).toBeUndefined()
    expect(callingThread({ threadId: "" })).toBeUndefined()
  })
})
