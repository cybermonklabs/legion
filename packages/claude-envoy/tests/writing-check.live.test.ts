/**
 * The plugin's own writes under Dispatch's writing check (INFRA-3231, R14; check 4370789f), in a
 * Claude Code session: the router of a person's messages (`../src/user-turn-router`) against a
 * real Dispatch server of this checkout with the check on. No model is configured, so the check
 * runs its structure rules alone: a sentence holding `spin up` breaks the `phrasal-verb` rule,
 * which the bundle enables, and that sentence is what each row writes. The Claude Code side is a
 * fake mod link; every Dispatch request is the real server's. The server runs no Envoy listener,
 * so a delivery attempt it records names no session: each row binds its attempt to this session
 * in the stack's own database, the one state no route reaches.
 */
import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { bearerCredential } from "@legion/envoy-client/bearer"
import {
  type DispatchDelivery,
  postDeliveryReply,
  renderInbound,
  WRITING_CHECK_ANSWER_WITHHELD,
} from "@legion/envoy-client/delivery"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
  useDispatchStackDirectory,
} from "@legion/envoy-client/test/dispatch-stack"
import { SQL } from "bun"
import type { ModCallArguments, ModCallName, ModCallResults } from "../hooks/mod-protocol"
import { modSession } from "../src/mod-session"
import { createUserTurnRouter } from "../src/user-turn-router"
import { readUserTurnState, userTurnState, userTurnStateFile } from "../src/user-turn-state"

const SESSION = "ses_claude_check"
const DIRECT = `notifications.agent.${SESSION}`
const PERSON = "alice"
const SENTENCE = "We will spin up the server and read the logs."
const REWRITTEN = "We will start the server and read the logs."

const stackDirectory = useDispatchStackDirectory()

/** A person's direct message `id` to this session, as the listener publishes it. */
function personSends(id: string, body: string, mode: "steer" | "btw"): string {
  return JSON.stringify({
    event_id: `evt-${id}`,
    source: "dispatch",
    source_event_id: `src-${id}`,
    topic: DIRECT,
    dedupe_key: `agent.${SESSION}.${id}:${mode}`,
    issued_at: 1_760_000_000_000,
    payload_summary: body,
    payload: JSON.stringify({
      event: {
        actor: { id: PERSON, kind: "user" },
        issue_key: null,
        payload: {
          author: { id: PERSON, kind: "user" },
          body,
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id,
          in_reply_to: null,
          issue_key: null,
          target: `session:${SESSION}`,
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode },
    }),
    trace_id: `trace-${id}`,
  })
}

describe.skipIf(!dispatchStackEnabled)("the writing check on the plugin's own writes", () => {
  let stack: DispatchStack | undefined
  let database: SQL | undefined
  const server = (): DispatchStack => {
    if (stack === undefined) throw new Error("the Dispatch stack did not start")
    return stack
  }
  const config = () => ({ url: server().url, credential: bearerCredential(server().token) })

  /** A person's own request, which the server's header identity takes as that person. */
  const asPerson = async <T>(method: string, route: string, body?: unknown): Promise<T> => {
    const answer = await fetch(`${server().url}${route}`, {
      method,
      headers: { "Content-Type": "application/json", "X-Dispatch-User": PERSON },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    })
    const text = await answer.text()
    if (!answer.ok) throw new Error(`${method} ${route}: ${answer.status} ${text}`)
    return JSON.parse(text) as T
  }

  /** A person's direct message to this session, its first attempt bound to this session. */
  const directMessage = async (body: string, delivery: "btw" | "steer"): Promise<string> => {
    const sent = await asPerson<{ readonly id: string }>(
      "POST",
      `/api/v1/agents/${SESSION}/messages`,
      { body, delivery },
    )
    if (database === undefined) throw new Error("the stack's database is not open")
    await database`update message_deliveries set session_id = ${SESSION}, state = 'pending', error = null where message_id = ${sent.id}`
    return sent.id
  }

  interface Thread {
    readonly message: {
      readonly deliveries?: readonly { readonly state: string; readonly error?: string | null }[]
    }
    readonly replies?: readonly { readonly body: string }[]
  }
  /** Waits until the message's thread holds a reply or its attempt failed, and answers it. */
  const answered = async (id: string): Promise<Thread> => {
    const deadline = performance.now() + 20_000
    for (;;) {
      const read = await asPerson<Thread>("GET", `/api/v1/messages/${id}`)
      if ((read.replies ?? []).length > 0) return read
      if (read.message.deliveries?.some((attempt) => attempt.state === "failed")) return read
      if (performance.now() > deadline) throw new Error(`message ${id} was never answered`)
      // A real server writes the reply on its own clock and signals nothing this process can
      // await, so the thread is read again after a short pause.
      await Bun.sleep(50)
    }
  }

  /** A router whose Claude Code answers as `answers` says, and what it was asked. */
  const router = async (answers: {
    readonly fork?: (prompt: string) => ModCallResults["model.fork"]
    readonly command?: () => ModCallResults["command.run"]
  }) => {
    const directory = await mkdtemp(join(tmpdir(), "claude-envoy-writing-check-"))
    const stateFile = userTurnStateFile(directory, SESSION)
    const prompts: string[] = []
    const notices: string[] = []
    const link = {
      connected: () => true,
      request: async <N extends ModCallName>(
        call: N,
        args: ModCallArguments[N],
      ): Promise<ModCallResults[N]> => {
        if (call === "model.fork" && answers.fork !== undefined) {
          const { prompt } = args as ModCallArguments["model.fork"]
          prompts.push(prompt)
          return answers.fork(prompt) as ModCallResults[N]
        }
        if (call === "command.run" && answers.command !== undefined) {
          return answers.command() as ModCallResults[N]
        }
        throw new Error(`the mod was asked ${call}`)
      },
    }
    const routed = createUserTurnRouter({
      dispatch: (ms) =>
        new DispatchClient(
          server().url,
          bearerCredential(server().token),
          fetch,
          AbortSignal.timeout(ms),
        ),
      effortPicked: () => undefined,
      refreshSettings: () => undefined,
      // The reply exactly as `postDispatchReply` (`../src/envoy-channel-server`) posts it, with
      // the session's Dispatch read from this stack rather than from an envoy.json.
      reply: (delivery: DispatchDelivery, result) =>
        postDeliveryReply(config(), SESSION, delivery, result),
      toMainTurn: (notice) => notices.push(notice),
      session: modSession({ link, warn: () => undefined, commandOutputMs: 10, commandEchoMs: 10 }),
      sessionId: () => SESSION,
      state: userTurnState(stateFile, await readUserTurnState(stateFile)),
      warn: () => undefined,
      commandWaitingMs: 60_000,
    })
    return {
      directory,
      notices,
      prompts,
      route: (id: string, body: string, mode: "steer" | "btw") =>
        routed.route(renderInbound(personSends(id, body, mode), SESSION, DIRECT)),
    }
  }

  beforeAll(async () => {
    stack = await startDispatchStack({
      work: stackDirectory,
      env: { DISPATCH_WRITING_CHECK: "on", DISPATCH_TEST_HOOKS: "1" },
    })
    database = new SQL(stack.databaseUrl)
  }, 600_000)
  afterAll(async () => {
    await database?.close()
    await stack?.stop()
  }, 60_000)

  // A fork's answer is the agent's words: refused, it goes back to a fork of its own holding the
  // findings, and that answer is posted in its place. Nothing quoting the refused answer reaches
  // Dispatch.
  test("a fork's refused answer goes back to a fork, whose answer is posted", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const texts = [SENTENCE, REWRITTEN]
    const session = await router({
      fork: () => ({ isAnswered: true as const, text: texts.shift() ?? "" }),
    })
    try {
      expect(await session.route(id, "Why was the first read slow?", "btw")).toBe(true)
      const read = await answered(id)
      expect(read.replies?.map((reply) => reply.body)).toEqual([REWRITTEN])
      expect(JSON.stringify(read)).not.toContain(SENTENCE)
      expect(session.prompts).toHaveLength(2)
      expect(session.prompts[1]).toContain("Dispatch's writing check refused your answer")
      expect(session.prompts[1]).toContain(SENTENCE)
      expect(session.notices).toEqual([])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // Refused twice, the findings reach the main conversation as a turn of its own, and the
  // attempt is settled with an error that quotes none of the refused text.
  test("a fork refused twice hands the main turn the findings, and posts no quote", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const session = await router({ fork: () => ({ isAnswered: true as const, text: SENTENCE }) })
    try {
      await session.route(id, "Why was the first read slow?", "btw")
      const read = await answered(id)
      expect(read.replies ?? []).toEqual([])
      expect(read.message.deliveries?.at(-1)?.error).toBe(WRITING_CHECK_ANSWER_WITHHELD)
      expect(JSON.stringify(read)).not.toContain(SENTENCE)
      expect(session.notices).toHaveLength(1)
      expect(session.notices[0]).toContain("Rule phrasal-verb")
      expect(session.notices[0]).toContain(`--in-reply-to ${id}`)
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // A fork that fails is answered with its failure: the plugin's own error reply, generated.
  test("a plugin's own error reply is written as generated", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const session = await router({
      fork: () => ({ isAnswered: false as const, reason: SENTENCE }),
    })
    try {
      await session.route(id, "Why was the first read slow?", "btw")
      const read = await answered(id)
      expect(read.message.deliveries?.at(-1)?.error).toBe(SENTENCE)
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // A slash command's output is the plugin's own text: generated, so written.
  test("a slash command's output is written as generated", async () => {
    const id = await directMessage("/status", "steer")
    const session = await router({ command: () => ({ text: SENTENCE }) })
    try {
      expect(await session.route(id, "/status", "steer")).toBe(true)
      const read = await answered(id)
      expect(read.replies?.map((reply) => reply.body)).toEqual([SENTENCE])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // A reply's `error` is prose the check reads: an agent's words there, sent without the field,
  // are refused like a body's. An agent's own forced write is counted apart below.
  test("a delivery reply whose error holds the sentence, without the field, is refused", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const delivery: DispatchDelivery = {
      resource: "message",
      id,
      attempt: 1,
      mode: "btw",
      replyPath: `/api/v1/messages/${id}/reply`,
      replyFields: {},
      issueKey: null,
      body: "Why?",
    }
    const outcome = await postDeliveryReply(config(), SESSION, delivery, { error: SENTENCE })
    expect(outcome.posted).toBe(false)
    expect(
      await postDeliveryReply(config(), SESSION, delivery, { body: SENTENCE, override: "agent" }),
    ).toEqual({ posted: true })
  }, 60_000)

  test("the day's counts show the generated writes apart from the agent's", async () => {
    const counts = await asPerson<{
      readonly outcomes: Readonly<Record<string, number>>
      readonly overrides: Readonly<Record<string, number>>
    }>("GET", "/api/v1/writing-check/counts")
    expect(counts.overrides["agent"]).toBe(1)
    // The fork's error, the withheld answer's error and the slash command's output.
    expect(counts.overrides["generated"] ?? 0).toBeGreaterThanOrEqual(3)
    expect(counts.outcomes["forced"] ?? 0).toBeGreaterThanOrEqual(3)
    expect(counts.outcomes["refused"] ?? 0).toBeGreaterThanOrEqual(4)
  }, 60_000)
})
