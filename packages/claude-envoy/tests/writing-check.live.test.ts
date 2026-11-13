/**
 * The plugin's own writes under Dispatch's writing check (INFRA-3231, R14; check 4370789f), in a
 * Claude Code session: the router of a person's messages (`../src/user-turn-router`) against a
 * real Dispatch server of this checkout with the check on and its detector on. The bundle ships
 * the detector off, so the server runs under the test-only
 * `DISPATCH_TEST_WRITING_CHECK_DETECTOR=on`, and its reader is a stand-in this file starts
 * (`DISPATCH_TEST_WRITING_CHECK_READER_URL`) that calls vacuous every sentence holding "names the
 * problem" and nothing else. "This names the problem." is the sentence each row writes, and the
 * detector's `vacuous-verb-phrase` is the rule that refuses it. The check reads a write only when
 * a person reads it: every row writes a reply to a direct message alice sent, which she reads, and
 * each row reads that reader on the check's own records. Text the plugin generates is sent with no
 * override first, and a second time with
 * `override_writing_check: "generated"` only where the check refused the first. The Claude Code
 * side is a fake mod link; every Dispatch request is the real server's. The server runs no Envoy
 * listener, so a delivery attempt it records names no session: each row binds its attempt to
 * this session in the stack's own database, the one state no route reaches.
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
  detectorOnWith,
  dispatchStackEnabled,
  type StandInReader,
  startDispatchStack,
  startStandInReader,
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
const SENTENCE = "This names the problem."
const REWRITTEN = "The first read was slow because the cache was cold."
const RULE = "Rule vacuous-verb-phrase"

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
  let reader: StandInReader | undefined
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

  const db = (): SQL => {
    if (database === undefined) throw new Error("the stack's database is not open")
    return database
  }

  /** A person's direct message to this session, its first attempt bound to this session. */
  const directMessage = async (body: string, delivery: "btw" | "steer"): Promise<string> => {
    const sent = await asPerson<{ readonly id: string }>(
      "POST",
      `/api/v1/agents/${SESSION}/messages`,
      { body, delivery },
    )
    await db()`update message_deliveries set session_id = ${SESSION}, state = 'pending', error = null where message_id = ${sent.id}`
    return sent.id
  }

  /** One decision the check recorded: what it decided, the override the write carried, and the
   *  person it decided reads the write. */
  interface CheckEvent {
    readonly outcome: string
    readonly override_value: string | null
    readonly reader: string | null
  }
  /** Where the check's records stand now: `eventsSince` reads the ones recorded after it. */
  const eventsMark = async (): Promise<string> => {
    const [row]: { readonly id: string }[] =
      await db()`select coalesce(max(id), 0)::text as id from writing_check_events`
    return row?.id ?? "0"
  }
  const eventsSince = async (mark: string): Promise<CheckEvent[]> => {
    const rows: CheckEvent[] =
      await db()`select outcome, override_value, reader from writing_check_events where id > ${mark}::bigint order by id`
    return [...rows]
  }
  /** What the check records for text a plugin generates and a person reads, where it breaks a
   *  rule: the first send, which carries no override, refused; the second, `generated`, forced. */
  const refusedThenForced: CheckEvent[] = [
    { outcome: "refused", override_value: null, reader: PERSON },
    { outcome: "forced", override_value: "generated", reader: PERSON },
  ]

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
    reader = startStandInReader("names the problem")
    stack = await startDispatchStack({ work: stackDirectory, env: detectorOnWith(reader) })
    database = new SQL(stack.databaseUrl)
  }, 600_000)
  afterAll(async () => {
    await database?.close()
    await stack?.stop()
    reader?.stop()
  }, 60_000)

  // A fork's answer is the agent's words: refused, it goes back to a fork of its own holding the
  // findings, and that answer is posted in its place. Nothing quoting the refused answer reaches
  // Dispatch, and no answer of the agent's carries an override.
  test("a fork's refused answer goes back to a fork, whose answer is posted", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const mark = await eventsMark()
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
      expect(await eventsSince(mark)).toEqual([
        { outcome: "refused", override_value: null, reader: PERSON },
        expect.objectContaining({ override_value: null, reader: PERSON }),
      ])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // Refused twice, the findings reach the main conversation as a turn of its own, and the
  // attempt is settled with an error that quotes none of the refused text. That error is the
  // plugin's own and breaks no rule, so it is written on its first send, with no override.
  test("a fork refused twice hands the main turn the findings, and posts no quote", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const mark = await eventsMark()
    const session = await router({ fork: () => ({ isAnswered: true as const, text: SENTENCE }) })
    try {
      await session.route(id, "Why was the first read slow?", "btw")
      const read = await answered(id)
      expect(read.replies ?? []).toEqual([])
      expect(read.message.deliveries?.at(-1)?.error).toBe(WRITING_CHECK_ANSWER_WITHHELD)
      expect(JSON.stringify(read)).not.toContain(SENTENCE)
      expect(session.notices).toHaveLength(1)
      expect(session.notices[0]).toContain(RULE)
      expect(session.notices[0]).toContain(`--in-reply-to ${id}`)
      expect(await eventsSince(mark)).toEqual([
        { outcome: "refused", override_value: null, reader: PERSON },
        { outcome: "refused", override_value: null, reader: PERSON },
        expect.objectContaining({ override_value: null, reader: PERSON }),
      ])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // A fork that fails is answered with its failure: the plugin's own error reply, refused on its
  // first send and written on the second, as generated.
  test("a plugin's own error reply is refused once, then written as generated", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const mark = await eventsMark()
    const session = await router({
      fork: () => ({ isAnswered: false as const, reason: SENTENCE }),
    })
    try {
      await session.route(id, "Why was the first read slow?", "btw")
      const read = await answered(id)
      expect(read.message.deliveries?.at(-1)?.error).toBe(SENTENCE)
      expect(await eventsSince(mark)).toEqual(refusedThenForced)
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // A slash command's output is the plugin's own text: refused on its first send, and written on
  // the second, as generated.
  test("a slash command's output is refused once, then written as generated", async () => {
    const id = await directMessage("/status", "steer")
    const mark = await eventsMark()
    const session = await router({ command: () => ({ text: SENTENCE }) })
    try {
      expect(await session.route(id, "/status", "steer")).toBe(true)
      const read = await answered(id)
      expect(read.replies?.map((reply) => reply.body)).toEqual([SENTENCE])
      expect(await eventsSince(mark)).toEqual(refusedThenForced)
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }, 60_000)

  // A reply's `error` is prose the check reads: an agent's words there, sent without the field,
  // are refused like a body's. An agent's own forced write is counted apart below.
  test("a delivery reply whose error holds the sentence, without the field, is refused", async () => {
    const id = await directMessage("Why was the first read slow?", "btw")
    const mark = await eventsMark()
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
    if (outcome.posted) return
    expect(outcome.refusal.findings).toEqual([
      expect.objectContaining({ field: "error", rule: "vacuous-verb-phrase", text: SENTENCE }),
    ])
    await new DispatchClient(server().url, bearerCredential(server().token)).deliveryReply(
      "message",
      id,
      {
        actor: { kind: "session", id: SESSION },
        attempt: 1,
        body: SENTENCE,
        override_writing_check: "agent",
      },
    )
    expect(await eventsSince(mark)).toEqual([
      { outcome: "refused", override_value: null, reader: PERSON },
      { outcome: "forced", override_value: "agent", reader: PERSON },
    ])
  }, 60_000)

  test("the day's counts show the generated writes apart from the agent's", async () => {
    const counts = await asPerson<{
      readonly outcomes: Readonly<Record<string, number>>
      readonly overrides: Readonly<Record<string, number>>
    }>("GET", "/api/v1/writing-check/counts")
    expect(counts.overrides["agent"]).toBe(1)
    // The fork's error and the slash command's output: each the second send of a text the check
    // refused once. No first send carried the field.
    expect(counts.overrides["generated"]).toBe(2)
    expect(counts.outcomes["forced"] ?? 0).toBeGreaterThanOrEqual(3)
    // The fork's first answer, both answers of the fork refused twice, the first send of each
    // generated text and the agent's error reply.
    expect(counts.outcomes["refused"] ?? 0).toBeGreaterThanOrEqual(6)
    // Each refusal above is the detector's, so the server called the stand-in reader.
    expect(reader?.calls() ?? 0).toBeGreaterThan(0)
  }, 60_000)
})
