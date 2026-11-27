/**
 * What a person's message from Dispatch's Agents page becomes in a Claude Code session
 * (INFRA-1026): the session's own user turn, a command, a pick or a side turn, and the card every
 * refusal keeps.
 */
import { expect, setDefaultTimeout, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { IDEMPOTENCY_KEY_COMMITTED } from "@legion/contracts"
import { type DispatchDelivery, renderInbound } from "@legion/envoy-client/delivery"
import {
  type DispatchClient,
  DispatchGatewayError,
  DispatchServiceError,
  DispatchWriteLandedError,
  DispatchWriteUnsettledError,
} from "@legion/envoy-client/dispatch-http"
import { messageFor } from "@legion/envoy-client/errors"
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard"
import type { ModCallArguments, ModCallName, ModCallResults, ModEvent } from "../hooks/mod-protocol"
import { type ModSession, modSession } from "../src/mod-session"
import {
  commandPrintedNothing,
  createUserTurnRouter,
  modelPicked,
  thinkingPicked,
  type UserTurnRouter,
} from "../src/user-turn-router"
import {
  readUserTurnState,
  type UserTurnState,
  userTurnState,
  userTurnStateFile,
} from "../src/user-turn-state"
import { settled, waitFor } from "./channel-session-harness"

// The rows wait on files, processes or local servers, which a loaded machine slows. Their bound
// is a hang guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS)

const SESSION = "ses_claude"
const DIRECT = `notifications.agent.${SESSION}`
const MESSAGE = "33333333-3333-4333-8333-333333333333"
/** Where a reply to MESSAGE is posted, which a reply's landing names. */
const REPLY_URL = `https://dispatch.test/api/v1/messages/${MESSAGE}/reply`
/** A reply to MESSAGE Dispatch made and gave no copy of its answer for: what every reply of a
 *  harness built with `repliesLand` throws, and whose message the line about it carries. */
const REPLY_LANDED = new DispatchWriteLandedError(IDEMPOTENCY_KEY_COMMITTED, 409, "POST", REPLY_URL)

/** One of a person's direct messages to this session, as the listener publishes it. */
function personSends(
  body: string,
  overrides: {
    readonly mode?: "aside" | "steer" | "btw"
    readonly attempt?: number
    readonly eventId?: string
  } = {},
): string {
  return JSON.stringify({
    event_id: overrides.eventId ?? "evt-send",
    source: "dispatch",
    source_event_id: "src-send",
    topic: DIRECT,
    dedupe_key: `agent.${SESSION}.${MESSAGE}:${overrides.mode ?? "steer"}`,
    issued_at: 1_760_000_000_000,
    payload_summary: body,
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: null,
        payload: {
          author: { id: "alice", kind: "user" },
          body,
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id: MESSAGE,
          in_reply_to: null,
          issue_key: null,
          target: `session:${SESSION}`,
        },
        type: "message.created",
      },
      delivery: { attempt: overrides.attempt ?? 1, mode: overrides.mode ?? "steer" },
    }),
    trace_id: "trace-send",
  })
}

type Answer<N extends ModCallName> = (
  args: ModCallArguments[N],
) => ModCallResults[N] | Promise<ModCallResults[N]>

/** The link as `modSession` uses it. */
interface FakeModLink {
  connected(): boolean
  request<N extends ModCallName>(
    call: N,
    args: ModCallArguments[N],
    options?: { readonly timeoutMs?: number },
  ): Promise<ModCallResults[N]>
}

/** A mod that answers what the test says, and remembers what it was asked. */
interface FakeMod {
  readonly calls: readonly { readonly call: ModCallName; readonly args: unknown }[]
  readonly link: FakeModLink
  madeOf<N extends ModCallName>(call: N): ModCallArguments[N][]
}

function fakeMod(answers: { [N in ModCallName]?: Answer<N> } = {}): FakeMod {
  const calls: { call: ModCallName; args: unknown }[] = []
  return {
    calls,
    link: {
      connected: () => true,
      request<N extends ModCallName>(
        call: N,
        args: ModCallArguments[N],
      ): Promise<ModCallResults[N]> {
        calls.push({ call, args })
        const answer = answers[call]
        if (answer === undefined) return Promise.reject(new Error(`no answer for ${call}`))
        return Promise.resolve(answer(args))
      },
    },
    madeOf: <N extends ModCallName>(call: N): ModCallArguments[N][] =>
      calls.filter((made) => made.call === call).map((made) => made.args as ModCallArguments[N]),
  }
}

interface Harness {
  readonly router: UserTurnRouter
  /** The Claude Code session under the router: the card path writes through it too, as this
   *  server's notifier does (`createChannelDelivery`, `../src/envoy-channel-server.ts`). */
  readonly session: ModSession
  readonly state: UserTurnState
  readonly stateFile: string
  readonly directory: string
  readonly replies: {
    readonly delivery: DispatchDelivery
    readonly body?: string
    /** Whether the answer asked Dispatch to post it although the attempt is already answered
     *  (`?follow_up=true`), which every answer after the first needs. */
    readonly followUp?: boolean
  }[]
  /** Every pick handed to the live view, every reply posted and every settings refresh, in the
   *  order they happened. */
  readonly order: string[]
  readonly accepts: { readonly id: string; readonly attempt: number }[]
  readonly observe: (event: ModEvent) => void
  /** What the router wrote as a failure that must not stop the session, in order. */
  readonly warnings: string[]
}

/** A router on a scratch state file, with the mod and Dispatch both faked. */
async function harness(input: {
  readonly link: FakeModLink
  /** What the accept route answers; a throw is the route refusing. */
  readonly accept?: (id: string, attempt: number) => unknown
  readonly inbox?: (line: {
    readonly content: string
    readonly blocks?: unknown
    readonly eventId?: string
  }) => Promise<void>
  readonly commandWaitingMs?: number
  /** How long a run whose echo row has been claimed waits for the output row behind it; short,
   *  so a test that drives that wait does not sit through the real bound. */
  readonly commandOutputMs?: number
  /** How long after its own answer a run may still claim an echo row; short, for the same
   *  reason. */
  readonly commandEchoMs?: number
  readonly directory?: string
  /** What a reply fails with, after it was recorded as posted; undefined, it is taken. */
  readonly replyFails?: (result: { readonly body?: string; readonly error?: string }) => unknown
  /** Every reply is one Dispatch made and gave no copy of its answer for: it is recorded, then
   *  thrown as `409 KEY_COMMITTED` (`REPLY_LANDED`). */
  readonly repliesLand?: boolean
  /** Every reply ends in this error, Dispatch's refusal or a post that may have landed: it is
   *  recorded, then this is thrown. */
  readonly repliesFail?: Error
}): Promise<Harness> {
  const directory = input.directory ?? (await mkdtemp(join(tmpdir(), "claude-envoy-turns-")))
  const stateFile = userTurnStateFile(directory, SESSION)
  const state = userTurnState(stateFile, await readUserTurnState(stateFile))
  const replies: { delivery: DispatchDelivery; body?: string }[] = []
  const order: string[] = []
  const accepts: { id: string; attempt: number }[] = []
  const warnings: string[] = []
  const session = modSession({
    link: input.link,
    warn: () => undefined,
    ...(input.commandEchoMs === undefined ? {} : { commandEchoMs: input.commandEchoMs }),
    ...(input.commandOutputMs === undefined ? {} : { commandOutputMs: input.commandOutputMs }),
  })
  const router = createUserTurnRouter({
    dispatch: async () =>
      ({
        acceptMessageDelivery: async (id: string, attempt: number) => {
          accepts.push({ id, attempt })
          if (input.accept === undefined) return { message_id: id, body: "", delivery: "steer" }
          return input.accept(id, attempt)
        },
      }) as unknown as DispatchClient,
    effortPicked: (effort) => order.push(`picked:${effort}`),
    refreshSettings: () => order.push("refresh"),
    reply: async (delivery, result) => {
      order.push(`reply:${result.body ?? result.error ?? ""}`)
      // `followUp` is the field an answer after the first needs; until the router carries one
      // the read is simply absent, which is what the test below pins.
      const asked = "followUp" in result ? result.followUp : undefined
      replies.push({
        delivery,
        ...(result.body === undefined ? {} : { body: result.body }),
        ...(asked === undefined ? {} : { followUp: asked }),
      })
      const failure = input.replyFails?.(result)
      if (failure !== undefined) throw failure
      if (input.repliesLand === true) {
        throw REPLY_LANDED
      }
      if (input.repliesFail !== undefined) {
        throw input.repliesFail
      }
    },
    session,
    sessionId: () => SESSION,
    state,
    warn: (text) => {
      warnings.push(text)
    },
    ...(input.commandWaitingMs === undefined ? {} : { commandWaitingMs: input.commandWaitingMs }),
    ...(input.inbox === undefined ? {} : { inbox: input.inbox }),
  })
  return {
    accepts,
    directory,
    // As the live view taps the link (`startAgentStream`, `../src/agent-stream-runner`): the
    // Claude Code session sees each event first, then the router.
    observe: (event) => {
      session.observe(event)
      router.observe(event)
    },
    order,
    replies,
    router,
    session,
    state,
    stateFile,
    warnings,
  }
}

function accepted(body: string, mode: "aside" | "steer" = "steer") {
  return { message_id: MESSAGE, body, delivery: mode }
}

function frame(raw: string) {
  return renderInbound(raw, SESSION, DIRECT)
}

/** One row of the `command` door, as the mod reports it. */
function commandRow(text: string, uuid: string): ModEvent {
  return {
    door: "command",
    kind: "append",
    message: { content: [{ text, type: "text" }], role: "user", type: "user" },
    origin: { kind: "engine" },
    uuid,
  }
}

/** The echo row Claude Code writes of a slash command it ran, as 2.1.310 records it: one text
 *  block naming the command, the message it carries and its arguments, each tag on its own line
 *  and every one after the first indented
 *  (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q2-310.jsonl:98-102`). */
function commandEcho(name: string, args: string): ModEvent {
  const text = [
    `<command-name>/${name}</command-name>`,
    `            <command-message>${name}</command-message>`,
    `            <command-args>${args}</command-args>`,
  ].join("\n")
  return commandRow(text, `row-echo-${name}`)
}

/** The output row Claude Code writes for a command it ran: one text block wrapped in
 *  `<local-command-stdout>`, which is where most commands put everything they printed. */
function commandOutput(text: string): ModEvent {
  return commandRow(`<local-command-stdout>${text}</local-command-stdout>`, "row-stdout")
}

test("a Send becomes the person's own message only once Dispatch has accepted that attempt", async () => {
  const mod = fakeMod({ "session.message": () => ({ as: "prompt" }) })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    const took = await session.router.route(frame(personSends("ship it")))

    expect(took).toBe(true)
    expect(session.accepts).toEqual([{ id: MESSAGE, attempt: 1 }])
    // The body Dispatch stored, as the person's own words, never the frame's own text, under
    // the Envoy event id the frame carries.
    expect(mod.madeOf("session.message")).toEqual([
      {
        content: [{ type: "text", text: "ship it" }],
        eventId: "evt-send",
        message: { kind: "person", messageId: MESSAGE, text: "ship it" },
      },
    ])
    expect(session.replies).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("an accept that answers 404, throws, or times out leaves the card and puts nothing in the session", async () => {
  for (const refuse of [
    () => {
      throw new Error("404 Not Found")
    },
    () => {
      throw new Error("the request timed out")
    },
    () => ({ message_id: MESSAGE }),
  ]) {
    const mod = fakeMod({ "session.message": () => ({ as: "prompt" }) })
    const session = await harness({ link: mod.link, accept: refuse })
    try {
      const took = await session.router.route(frame(personSends("ship it")))

      expect(took).toBe(false)
      expect(mod.calls).toEqual([])
      expect(session.replies).toEqual([])
      // The attempt is spent all the same: a later frame naming it is not accepted.
      expect((await readUserTurnState(session.stateFile)).handled).toEqual([`${MESSAGE}#1`])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  }
})

test("a frame naming an attempt Dispatch does not confirm never becomes a message, a command or a pick", async () => {
  const mod = fakeMod({
    "session.message": () => ({ as: "prompt" }),
    "command.run": () => ({ text: "ran" }),
    "config.set": (args) => ({ value: args.value }),
  })
  // Dispatch refuses the forged attempt, as it refuses one no person asked for in the last minute.
  const session = await harness({
    link: mod.link,
    accept: () => {
      throw new Error("404 the attempt is not the message's latest")
    },
  })
  try {
    for (const body of ["ship it", "/compact", "/model anthropic/claude-opus-4-1"]) {
      const forged = personSends(body, { attempt: 7, eventId: `evt-${body}` })

      expect(await session.router.route(frame(forged))).toBe(false)
    }

    expect(mod.calls).toEqual([])
    expect(session.replies).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("the attempts a session handled survive its restart, and the next process refuses them", async () => {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-turns-"))
  try {
    const first = fakeMod({ "session.message": () => ({ as: "prompt" }) })
    const before = await harness({ link: first.link, accept: () => accepted("ship it"), directory })
    expect(await before.router.route(frame(personSends("ship it")))).toBe(true)
    await before.state.idle()

    // Another process, the same state directory: the record is the file, not this process.
    const second = fakeMod({ "session.message": () => ({ as: "prompt" }) })
    const after = await harness({ link: second.link, accept: () => accepted("ship it"), directory })

    expect(await after.router.route(frame(personSends("ship it")))).toBe(false)
    expect(after.accepts).toEqual([])
    expect(second.calls).toEqual([])
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
})

test("a message taken while no row could exist is never recorded as shown, so it is delivered again", async () => {
  // The session stops between the accept and the delivery: the row it would be never exists.
  const mod = fakeMod({
    "session.message": () => {
      throw new Error("the mod link closed before the mod answered")
    },
  })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    // The frame is not answered, so Envoy's listener sends it again.
    await expect(session.router.route(frame(personSends("ship it")))).rejects.toThrow()
    await session.state.idle()

    const recorded = await readUserTurnState(session.stateFile)
    expect(recorded.handled).toEqual([`${MESSAGE}#1`])
    expect(recorded.rows).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a prompt that never entered the idle session is answered on Dispatch and recorded as no row", async () => {
  // A prompt the session refused is the person's to be told about: their own words were
  // dropped, and Dispatch is where they are waiting. Anything else that went wrong leaves the
  // frame unanswered instead (`../src/mod-session`, `deliver`).
  const mod = fakeMod({
    "session.message": () => ({ as: "prompt", deny: "a UserPromptSubmit hook blocked it" }),
  })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    expect(await session.router.route(frame(personSends("ship it")))).toBe(true)
    await waitFor(async () => session.replies.length === 1, "the reply naming the failure")
    await session.state.idle()

    expect(session.replies[0]?.body).toContain("a UserPromptSubmit hook blocked it")
    expect((await readUserTurnState(session.stateFile)).rows).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})
test("the body goes to the mod as the person's own message, and its row is recorded", async () => {
  const mod = fakeMod({ "session.message": () => ({ as: "row", uuid: "row-1" }) })
  const session = await harness({ link: mod.link, accept: () => accepted("look at this") })
  try {
    expect(await session.router.route(frame(personSends("look at this")))).toBe(true)

    // One call, carrying the person's own words, the Dispatch message they are and the Envoy
    // event the frame is: what that becomes in the session — a row of the turn running, or the
    // prompt of a new turn — and what points the model at a row no request read are the mod's
    // (`../hooks/mod.ts`).
    expect(mod.madeOf("session.message")).toEqual([
      {
        content: [{ type: "text", text: "look at this" }],
        eventId: "evt-send",
        message: { kind: "person", messageId: MESSAGE, text: "look at this" },
      },
    ])
    // The row the mod answered with is recorded as that Dispatch message, which is what tags it
    // on the live view.
    expect(session.state.messageOf("row-1")).toBe(MESSAGE)
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// The reviewer's "one time" (INFRA-1026 review, round twelve): the mod's answer is late, so the
// frame is left unanswered and Envoy sends it again — and the resend is a *card*, because the
// accept refuses an attempt this session has already handled. Both writes carry the frame's own
// Envoy event id, so the mod writes the person's words once and answers the card with the row
// that write left. Without the id on the person's write the model reads their words twice, once
// as their own prompt and once as a card.
test("a person's message whose answer was late is one write with the card its resend becomes", async () => {
  const written: ModCallArguments["session.message"][] = []
  /** The rows the mod recorded, by Envoy event id: its own rule, which `writeOnce` holds
   *  (`../hooks/mod-writer.ts`, driven against the real `$` in `./mod.test.ts`). */
  const rows = new Map<string, ModCallResults["session.message"]>()
  let late = true
  const link: FakeModLink = {
    connected: () => true,
    request<N extends ModCallName>(call: N, args: ModCallArguments[N]): Promise<ModCallResults[N]> {
      if (call !== "session.message") {
        return Promise.reject(new Error(`the message path made an unexpected mod call: ${call}`))
      }
      const message = args as ModCallArguments["session.message"]
      const recorded = message.eventId === undefined ? undefined : rows.get(message.eventId)
      // A copy of an event id the mod has written is answered with the recorded row, and
      // nothing is written for it.
      if (recorded !== undefined) return Promise.resolve(recorded) as Promise<ModCallResults[N]>
      written.push(message)
      const row = { as: "row", uuid: `row-${written.length}` } as const
      if (message.eventId !== undefined) rows.set(message.eventId, row)
      if (!late) return Promise.resolve(row) as Promise<ModCallResults[N]>
      // The write landed, its answer did not: the caller's own bound ends the wait, as the
      // link's does (`../src/mod-link.ts`, `request`).
      late = false
      return Promise.reject(new Error("the mod did not answer session.message within 30000 ms"))
    },
  }
  const session = await harness({ link, accept: () => accepted("ship it") })
  try {
    const sent = frame(personSends("ship it", { eventId: "evt-late" }))

    await expect(session.router.route(sent)).rejects.toThrow("did not answer")
    // Envoy sends the frame again. The attempt is spent, so the accept refuses it and this
    // server writes the frame as the card it now is, under the one event id
    // (`createChannelDelivery`, `../src/envoy-channel-server.ts`).
    expect(await session.router.route(sent)).toBe(false)
    const eventId = sent.envelope?.event_id
    await session.session.show({
      content: [{ text: sent.content, type: "text" }],
      text: sent.content,
      ...(eventId === undefined ? {} : { eventId }),
    })

    // One write, which the card read back: `show` resolved, so the frame is answered and
    // Envoy stops sending it.
    expect(written).toHaveLength(1)
    expect(written[0]?.eventId).toBe("evt-late")
    expect(written[0]?.message).toEqual({ kind: "person", messageId: MESSAGE, text: "ship it" })
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A message the mod took as the prompt of a new turn answers with the prompt alone and no row
// of its own: the row Claude Code writes for a prompt is the prompt's, the router recognises
// the message by its text instead, and nothing is recorded here (`../src/mod-session`,
// `deliver`).
test("a body the mod took as a prompt is recorded as no row of its own", async () => {
  const mod = fakeMod({ "session.message": () => ({ as: "prompt" }) })
  const session = await harness({ link: mod.link, accept: () => accepted("ship it") })
  try {
    expect(await session.router.route(frame(personSends("ship it")))).toBe(true)
    await session.state.idle()

    expect((await readUserTurnState(session.stateFile)).rows).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a slash message runs as the command it is and its output is the reply", async () => {
  const mod = fakeMod({ "command.run": () => ({ text: "Compacted 42 messages." }) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/compact keep the plan"),
  })
  try {
    expect(await session.router.route(frame(personSends("/compact keep the plan")))).toBe(true)
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(mod.madeOf("command.run")).toEqual([{ command: "compact", args: "keep the plan" }])
    expect(session.replies.map((posted) => posted.body)).toEqual(["Compacted 42 messages."])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// Measured on 2.1.310 in a terminal session: `$.command.run("context")` resolves
// `{context: ["## Context Usage\n\n**Model:** …"]}` with no `text` field at all
// (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q7-int.jsonl:70-73`). Answering `text ?? ""`
// left the person who sent `/context` from the live view with no reply at all (INFRA-1026).
test("a command whose result puts its output in `context` reaches the person as the reply", async () => {
  const table = "## Context Usage\n\n**Model:** claude-haiku-4-5  \n**Tokens:** 32k / 200k (16%)"
  const mod = fakeMod({ "command.run": () => ({ context: [table] }) })
  const session = await harness({ link: mod.link, accept: () => accepted("/context") })
  try {
    expect(await session.router.route(frame(personSends("/context")))).toBe(true)
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(mod.madeOf("command.run")).toEqual([{ command: "context" }])
    expect(session.replies.map((posted) => posted.body)).toEqual([table])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// Most commands resolve with nothing and put everything they printed in the row they wrote:
// `/effort low` on 2.1.310 resolved `{}` and wrote its `<local-command-stdout>` row 158 ms
// later (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q2-310.jsonl:98-102`). The row and the
// result reach this process on two separate posts, so the row is waited for a bounded moment
// rather than answered empty at once.
test("a command whose result carries no output is answered from the row Claude Code wrote", async () => {
  const mod = fakeMod({ "command.run": () => ({}) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/cost"),
    commandOutputMs: 500,
  })
  try {
    expect(await session.router.route(frame(personSends("/cost")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")

    session.observe(commandEcho("cost", ""))
    session.observe(commandOutput("Total cost:            $0.0993"))
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual(["Total cost:            $0.0993"])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// `typedInputReply` answers undefined for an empty output, which posted nothing: a person who
// sent a command that printed nothing was left with silence (INFRA-1026).
test("a command that printed nothing is answered saying so rather than with silence", async () => {
  const mod = fakeMod({ "command.run": () => ({}) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/clear"),
    commandOutputMs: 500,
  })
  try {
    expect(await session.router.route(frame(personSends("/clear")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")

    // An output row that arrives empty is itself the answer that the command printed nothing.
    session.observe(commandEcho("clear", ""))
    session.observe(commandOutput(""))
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual([commandPrintedNothing("/clear")])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A command does not run under the name the person sent it with: measured live on 2.1.310, a
// `/cost` from Dispatch was run and echoed by Claude Code as `/usage`, and a
// `/model anthropic/haiku` as `/model haiku` (INFRA-1026). So what ties the rows to the run is
// that they arrived just after its call answered, never the command the rows name.
test("a command Claude Code runs under another name is still answered from that run's output", async () => {
  const mod = fakeMod({ "command.run": () => ({}) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/cost"),
    commandOutputMs: 500,
  })
  try {
    expect(await session.router.route(frame(personSends("/cost")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")
    expect(mod.madeOf("command.run")).toEqual([{ command: "cost" }])

    session.observe(commandEcho("usage", ""))
    session.observe(commandOutput("Total cost:            $0.0993"))
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual(["Total cost:            $0.0993"])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A run whose command opened a dialog at the terminal has written no row of its own while the
// person answers it, and the rows that arrive meanwhile are the person's own typing. Measured
// on live Claude Code 2.1.310 (INFRA-1026): a Dispatch `/cost` asked at 08:09:56.390 was still
// unanswered when the person typed `/config` at that terminal 36 seconds later, and the rule
// before this one answered their `/cost` with `Config dialog dismissed` - the other command's
// output, posted to Dispatch as the reply to theirs.
test("a command a person typed at the terminal is not the answer to one still waiting", async () => {
  const dialog = Promise.withResolvers<ModCallResults["command.run"]>()
  const mod = fakeMod({ "command.run": () => dialog.promise })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/cost"),
    commandEchoMs: 100,
    commandOutputMs: 100,
  })
  try {
    expect(await session.router.route(frame(personSends("/cost")))).toBe(true)
    await waitFor(async () => mod.madeOf("command.run").length === 1, "the command to run")

    // What the person typed at their own terminal while the dialog waited for them.
    session.observe(commandEcho("config", ""))
    session.observe(commandOutput("Config dialog dismissed"))
    await settled()
    expect(session.replies).toEqual([])

    // Their `/cost` answers for itself once the dialog closes, and it printed nothing.
    dialog.resolve({})
    await waitFor(async () => session.replies.length === 1, "the command's reply")

    expect(session.replies.map((posted) => posted.body)).toEqual([commandPrintedNothing("/cost")])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a command that has not ended in time is answered that it is waiting, and its output follows", async () => {
  const held = Promise.withResolvers<void>()
  const mod = fakeMod({
    "command.run": async () => {
      await held.promise
      return { text: "done at the terminal" }
    },
  })
  // The real bound is ten seconds; the rule under test is that the waiting reply goes out while
  // the command still runs, which only a real timer fires.
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/clear"),
    commandWaitingMs: 10,
  })
  try {
    await session.router.route(frame(personSends("/clear")))
    await waitFor(async () => session.replies.length === 1, "the waiting reply")

    expect(session.replies[0]?.body).toContain("/clear is still running")

    held.resolve()
    await waitFor(async () => session.replies.length === 2, "the command's output")
    expect(session.replies.map((posted) => posted.body)).toEqual([
      expect.stringContaining("/clear is still running"),
      "done at the terminal",
    ])
  } finally {
    held.resolve()
    await rm(session.directory, { recursive: true, force: true })
  }
})

// Dispatch settles a delivery attempt with the first reply it takes, and answers a second POST to
// the same attempt by handing the stored reply back and posting nothing, unless the request asks
// for a follow-up (`?follow_up=true`; `packages/envoy/internal/dispatch/api/message_delivery.go`,
// and `DispatchClient.messageReply`'s own `followUp` option). So the answer that carries a slow
// command's output - the one the waiting reply promised "follows in another reply" - has to ask
// for it, or the person is left with the waiting reply and never gets the output. Driven live on
// a Claude Code 2.1.310 pane: `/context` inside a turn got the waiting reply, the mod's POST of
// the table was answered 200 with nothing inserted, and the conversation holds one reply
// (INFRA-1026).
test("the answer that follows a waiting reply asks Dispatch to post it", async () => {
  const held = Promise.withResolvers<void>()
  const mod = fakeMod({
    "command.run": async () => {
      await held.promise
      return { text: "done at the terminal" }
    },
  })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/clear"),
    commandWaitingMs: 10,
  })
  try {
    await session.router.route(frame(personSends("/clear")))
    await waitFor(async () => session.replies.length === 1, "the waiting reply")
    held.resolve()
    await waitFor(async () => session.replies.length === 2, "the command's output")

    // The first answer settles the attempt, so it asks for nothing; the second is the one
    // Dispatch drops without the ask.
    expect(session.replies.map((posted) => posted.followUp === true)).toEqual([false, true])
  } finally {
    held.resolve()
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a model pick sets the model and answers naming it, with the header refreshed first", async () => {
  const mod = fakeMod({ "config.set": (args) => ({ value: args.value }) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/model anthropic/claude-opus-4-1"),
  })
  try {
    await session.router.route(frame(personSends("/model anthropic/claude-opus-4-1")))
    await waitFor(async () => session.replies.length === 1, "the pick's reply")

    expect(mod.madeOf("config.set")).toEqual([{ key: "model", value: "claude-opus-4-1" }])
    expect(session.order).toEqual(["refresh", `reply:${modelPicked("anthropic/claude-opus-4-1")}`])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a thinking pick sets the effort of every later request and answers naming it", async () => {
  const mod = fakeMod({ "set-effort": (args) => ({ effort: args.effort }) })
  const session = await harness({ link: mod.link, accept: () => accepted("/effort high") })
  try {
    await session.router.route(frame(personSends("/effort high")))
    await waitFor(async () => session.replies.length === 1, "the pick's reply")

    expect(mod.madeOf("set-effort")).toEqual([{ effort: "high" }])
    // The live view is told the level the pick put in force before the reply naming it goes
    // out: Claude Code reports an effort only on a request of its own, and an idle session
    // makes none.
    expect(session.order).toEqual(["picked:high", "refresh", `reply:${thinkingPicked("high")}`])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a BTW is answered from the model's fork and adds nothing to the conversation", async () => {
  const mod = fakeMod({
    "model.fork": () => ({ isAnswered: true as const, text: "It is the migration guard." }),
  })
  const session = await harness({ link: mod.link })
  try {
    expect(await session.router.route(frame(personSends("what blocks it?", { mode: "btw" })))).toBe(
      true,
    )
    await waitFor(async () => session.replies.length === 1, "the BTW's answer")

    expect(mod.madeOf("model.fork")).toEqual([{ prompt: "what blocks it?" }])
    expect(mod.madeOf("session.message")).toEqual([])
    expect(session.replies.map((posted) => posted.body)).toEqual(["It is the migration guard."])
    // A BTW is no attempt of a person's own turn: nothing of it is spent.
    expect(session.accepts).toEqual([])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A BTW's reply Dispatch is still running when its deadline ends lands without this side and
// settles the attempt when it does, so no error is posted over it, which could settle the attempt
// first with words the person would read in place of the answer; the warning names its key, and
// says the reply is running only where Dispatch said so: after an attempt cut off for silence
// alone it may still be. Any other failure of the reply is still answered with its error, so the
// person sees the attempt fail.
test.each([
  {
    reply: "is still running on Dispatch",
    error: new DispatchWriteUnsettledError(
      new DOMException("The operation timed out.", "TimeoutError"),
      21,
      "reply-key-1",
      `POST http://dispatch.test/api/v1/messages/${MESSAGE}/reply is still running on Dispatch, and its deadline ended the wait for its outcome`,
      true,
      true,
    ),
    warning: `a BTW's reply to message ${MESSAGE} is still running on Dispatch under key reply-key-1; nothing more is posted`,
  },
  {
    reply: "may still be running on Dispatch",
    error: new DispatchWriteUnsettledError(
      new DOMException("The operation timed out.", "TimeoutError"),
      3,
      "reply-key-1",
      `POST http://dispatch.test/api/v1/messages/${MESSAGE}/reply may still be running on Dispatch, and its deadline ended the wait for its outcome`,
      true,
      false,
    ),
    warning: `a BTW's reply to message ${MESSAGE} may still be running on Dispatch under key reply-key-1; nothing more is posted`,
  },
  {
    reply: "had no definitive answer",
    error: new DispatchWriteUnsettledError(
      new DOMException("The operation timed out.", "TimeoutError"),
      5,
      "reply-key-1",
      `POST http://dispatch.test/api/v1/messages/${MESSAGE}/reply had no definitive answer when its deadline ended it`,
    ),
    warning: undefined,
  },
])("a BTW whose reply $reply posts an error over it only where it is not still running", async (row) => {
  const mod = fakeMod({
    "model.fork": () => ({ isAnswered: true as const, text: "It is the migration guard." }),
  })
  const session = await harness({
    link: mod.link,
    replyFails: (result) => (result.body === undefined ? undefined : row.error),
  })
  try {
    await session.router.route(frame(personSends("what blocks it?", { mode: "btw" })))
    await waitFor(
      async () => session.order.length + session.warnings.length === 2,
      "the BTW's reply and what followed it",
    )
    await settled()

    expect(session.order).toEqual([
      "reply:It is the migration guard.",
      ...(row.warning === undefined ? [`reply:${messageFor(row.error)}`] : []),
    ])
    expect(session.warnings).toEqual(row.warning === undefined ? [] : [row.warning])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A reply Dispatch made and gave no copy of its answer for answered the attempt: an error reply
// after it would answer the person a second time with the error of a write that was made, and
// the line says it landed, never that it did not reach Dispatch.
test("a BTW answer that landed with no answer back is followed by no error reply", async () => {
  const mod = fakeMod({
    "model.fork": () => ({ isAnswered: true as const, text: "It is the migration guard." }),
  })
  const session = await harness({ link: mod.link, repliesLand: true })
  try {
    await session.router.route(frame(personSends("what blocks it?", { mode: "btw" })))
    await waitFor(async () => session.warnings.length === 1, "the BTW answer's line")

    // Every reply the router posted, an error reply included, in the order it posted them.
    expect(session.order).toEqual(["reply:It is the migration guard."])
    expect(session.warnings).toEqual([
      `a BTW answer to message ${MESSAGE} landed on Dispatch, which gave no copy of its answer — ${REPLY_LANDED.message}`,
    ])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("a reply that landed with no answer back is not said to have missed Dispatch", async () => {
  const mod = fakeMod({ "config.set": (args) => ({ value: args.value }) })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/model anthropic/claude-opus-4-1"),
    repliesLand: true,
  })
  try {
    await session.router.route(frame(personSends("/model anthropic/claude-opus-4-1")))
    await waitFor(async () => session.warnings.length === 1, "the reply's line")

    expect(session.order).toEqual(["refresh", `reply:${modelPicked("anthropic/claude-opus-4-1")}`])
    expect(session.warnings).toEqual([
      `a reply to message ${MESSAGE} landed on Dispatch, which gave no copy of its answer — ${REPLY_LANDED.message}`,
    ])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A reply Dispatch itself refused reached Dispatch, so the line names it as not posted, with
// Dispatch's code, and never as a write that did not reach Dispatch. A BTW answer refused so is
// answered with the error, and when that error reply is refused too the line says the answer was
// not posted, with the error reply's refusal.
test("a reply or a BTW answer Dispatch refused is said not posted, with Dispatch's code", async () => {
  const refusal = new DispatchServiceError("ATTEMPT_SETTLED", 409, "attempt 1 is settled")
  const mod = fakeMod({
    "config.set": (args) => ({ value: args.value }),
    "model.fork": () => ({ isAnswered: true as const, text: "It is the migration guard." }),
  })
  const session = await harness({
    link: mod.link,
    accept: () => accepted("/model anthropic/claude-opus-4-1"),
    repliesFail: refusal,
  })
  try {
    await session.router.route(frame(personSends("/model anthropic/claude-opus-4-1")))
    await waitFor(async () => session.warnings.length === 1, "the reply's line")
    expect(session.warnings).toEqual([
      `a reply to message ${MESSAGE} was not posted — ATTEMPT_SETTLED: attempt 1 is settled`,
    ])

    await session.router.route(frame(personSends("what blocks it?", { mode: "btw" })))
    await waitFor(async () => session.warnings.length === 2, "the BTW answer's line")
    expect(session.order.slice(-2)).toEqual([
      "reply:It is the migration guard.",
      "reply:ATTEMPT_SETTLED: attempt 1 is settled",
    ])
    expect(session.warnings[1]).toBe(
      `a BTW answer to message ${MESSAGE} was not posted — ATTEMPT_SETTLED: attempt 1 is settled`,
    )
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A reply that ended with no definitive answer while an attempt of it may have reached Dispatch
// (`DispatchWriteUnsettledError`), or on a status after which it may have (`mayHaveReachedServer`:
// a 5xx, or a 408, which a gateway can answer after it forwarded the write), may have been posted,
// so the line says it may or may not have been, never that it was not. A BTW answer that ends so
// is answered with the error, and when that error reply ends so too the line says the same of the
// BTW answer. Each error is injected as the host's reply fails, so the 5xx and 408 rows hold the
// router's own reading of an error that reaches it unclassified; the Dispatch client's keyed POST
// ends both statuses as `DispatchWriteUnsettledError` before any host sees them.
for (const [name, failure, tail] of [
  [
    "a write left unsettled",
    new DispatchWriteUnsettledError(
      new DispatchServiceError("RATE_LIMITED", 429, "slow down"),
      2,
      "reply-key",
    ),
    "RATE_LIMITED: slow down (the last of 2 attempts), and an earlier attempt of the write may have reached Dispatch, so it may or may not have taken effect. Dispatch finishes an attempt it received whatever this command does, so read the write back before you send it again: a new command sends it under a new key.",
  ],
  [
    "a 5xx",
    new DispatchServiceError("INTERNAL", 500, "internal error"),
    "INTERNAL: internal error",
  ],
  [
    "a gateway's 408",
    new DispatchGatewayError(408, "answered 408", "ask again"),
    "HTTP_408: answered 408, so ask again.",
  ],
] as const) {
  test(`a reply that ended in ${name} may have been posted, and its line says so`, async () => {
    const mod = fakeMod({ "config.set": (args) => ({ value: args.value }) })
    const session = await harness({
      link: mod.link,
      accept: () => accepted("/model anthropic/claude-opus-4-1"),
      repliesFail: failure,
    })
    try {
      await session.router.route(frame(personSends("/model anthropic/claude-opus-4-1")))
      await waitFor(async () => session.warnings.length === 1, "the reply's line")
      expect(session.warnings).toEqual([
        `a reply to message ${MESSAGE} may or may not have been posted — ${tail}`,
      ])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  })

  test(`a BTW answer that ended in ${name} may have been posted, and its line says so`, async () => {
    const mod = fakeMod({
      "model.fork": () => ({ isAnswered: true as const, text: "It is the migration guard." }),
    })
    const session = await harness({ link: mod.link, repliesFail: failure })
    try {
      await session.router.route(frame(personSends("what blocks it?", { mode: "btw" })))
      await waitFor(async () => session.warnings.length === 1, "the BTW answer's line")
      expect(session.order).toEqual(["reply:It is the migration guard.", `reply:${tail}`])
      expect(session.warnings).toEqual([
        `a BTW answer to message ${MESSAGE} may or may not have been posted — ${tail}`,
      ])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  })
}

// An accept Dispatch recorded and gave no copy of its answer for spent the attempt as this
// session's, and the stored body a turn is made of is not in hand: the frame keeps its card, and
// the line says the accept was recorded and why no turn follows, never that Dispatch did not
// accept it.
test("an accept that landed with no answer back keeps the card, saying the accept was recorded", async () => {
  const mod = fakeMod({})
  const landed = new DispatchWriteLandedError(
    IDEMPOTENCY_KEY_COMMITTED,
    409,
    "POST",
    `https://dispatch.test/api/v1/messages/${MESSAGE}/deliveries/1/accept`,
  )
  const session = await harness({
    link: mod.link,
    accept: () => {
      throw landed
    },
  })
  try {
    expect(await session.router.route(frame(personSends("ship it")))).toBe(false)

    expect(mod.calls).toEqual([])
    expect(session.warnings).toEqual([
      `Dispatch recorded this session's accept of message ${MESSAGE} and gave no copy of its answer (${landed.message}), so no turn follows; it arrives as a card`,
    ])
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

test("in a Legion pane the person's message goes to the bridge's inbox and its row is recognised", async () => {
  const lines: { content: string; blocks?: unknown; eventId?: string }[] = []
  const mod = fakeMod({})
  const session = await harness({
    link: mod.link,
    accept: () => accepted("have a look"),
    inbox: async (line) => {
      lines.push(line)
    },
  })
  try {
    expect(await session.router.route(frame(personSends("have a look")))).toBe(true)

    expect(lines).toEqual([{ content: "have a look", eventId: MESSAGE }])
    // The pane's message never goes through the mod.
    expect(mod.calls).toEqual([])

    // The row the bridge makes of it is recognised by its text and recorded as that message.
    session.observe({
      kind: "append",
      uuid: "row-9",
      door: "prompt",
      origin: { kind: "user" },
      message: { type: "user", content: [{ type: "text", text: "have a look" }] },
    })
    expect(session.state.messageOf("row-9")).toBe(MESSAGE)
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})

// A person's message that embeds a picture reaches the session as its text and the picture
// beside it, and the row written back for it holds the text alone - measured live on a Legion
// pane (Claude Code 2.1.310), with the picture's markdown gone from it, the picture having become
// a block of its own. Read against the stored body as it stands, that row is recognised as no
// message at all, so nothing records which Dispatch message it is and the Agents page draws its
// stored copy beside it (INFRA-1026).
test("in a Legion pane the row of a message embedding a picture is recognised without its markdown", async () => {
  const body =
    "What colour is this picture? ![blue](dispatch://agent/ses_claude/artifact/i3-blue96-png@v1)"
  const lines: { content: string; blocks?: unknown; eventId?: string }[] = []
  const mod = fakeMod({})
  const session = await harness({
    link: mod.link,
    accept: () => accepted(body),
    inbox: async (line) => {
      lines.push(line)
    },
  })
  try {
    expect(await session.router.route(frame(personSends(body)))).toBe(true)
    expect(lines.map((line) => line.content)).toEqual([body])

    session.observe({
      kind: "append",
      uuid: "row-picture",
      door: "prompt",
      origin: { kind: "user" },
      message: { type: "user", content: [{ type: "text", text: "What colour is this picture? " }] },
    })
    expect(session.state.messageOf("row-picture")).toBe(MESSAGE)
  } finally {
    await rm(session.directory, { recursive: true, force: true })
  }
})
