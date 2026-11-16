/**
 * The mod's own hooks (`../hooks/mod.ts`), driven the way Claude Code drives them: `register`
 * hands its hooks over, `session.start` gives the link a `$` of this file's own, and the server on
 * the far side of the socket is the real one (`../src/mod-link.ts`), so a `set-effort` is in force
 * here exactly as the live view puts it in force.
 *
 * The module holds one link per load, as a mod loaded into a session does, so the whole file
 * shares one session: it is started once and every test drives steps through it.
 */
import { afterAll, beforeAll, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { request as httpRequest } from "node:http"
import { tmpdir } from "node:os"
import { join } from "node:path"
import type {
  EngineInterface,
  HookStream,
  HttpInit,
  HttpResponse,
  On,
  SessionAppendHook,
  SessionStartInput,
  SessionStartResult,
  TurnCompleteInput,
  TurnCompleteResult,
  TurnStartInput,
  TurnStartResult,
  TurnStepChunk,
  TurnStepInput,
  TurnStepResult,
} from "claude-code"
import { register } from "../hooks/mod"
import {
  MOD_INSTANCE_HEADER,
  MOD_MIN_CLAUDE_VERSION,
  type ModCallResults,
  type ModEvent,
  unreadRowPrompt,
} from "../hooks/mod-protocol"
import { type ModLink, startModLink } from "../src/mod-link"

/** The hook `on("session.start", ...)` takes, as `On` declares it. */
type SessionStartHook = (
  $: EngineInterface,
  e: SessionStartInput,
  next: (e: SessionStartInput) => Promise<SessionStartResult>,
) => Promise<SessionStartResult>

/** The hook `on("turn.start", ...)` takes. */
type TurnStartHook = (
  $: EngineInterface,
  e: TurnStartInput,
  next: (e: TurnStartInput) => Promise<TurnStartResult>,
) => Promise<TurnStartResult>

/** The hook `on("turn.complete", ...)` takes. */
type TurnCompleteHook = (
  $: EngineInterface,
  e: TurnCompleteInput,
  next: (e: TurnCompleteInput) => Promise<TurnCompleteResult>,
) => Promise<TurnCompleteResult>

/** The hook `on("turn.step", ...)` takes, as `On` declares it. */
type StepHook = (
  $: EngineInterface,
  e: TurnStepInput,
  next: (e: TurnStepInput) => HookStream<TurnStepChunk, TurnStepResult>,
) => AsyncGenerator<TurnStepChunk, TurnStepResult | undefined>

/** The session this file drives, once it has started. */
interface Session {
  readonly server: ModLink
  readonly engine: EngineInterface
  readonly step: StepHook
  readonly turnStart: TurnStartHook
  readonly turnComplete: TurnCompleteHook
  /** The `session.append` hook every door's registration shares. */
  readonly appended: SessionAppendHook
}

/** Short on purpose: the socket is `<base>/claude-envoy/ce-<hash>.sock` and that whole path has
 *  100 bytes to fit in (`MOD_SOCKET_PATH_MAX_BYTES`). */
const SESSION_ID = "mod-step"

/** A model Claude Code names an effort for: measured on 2.1.310, a request of it before any pick
 *  carried `output_config.effort: medium`. */
const MODEL_WITH_EFFORT = "claude-opus-5-5"

/** A model Claude Code names no effort for: measured on 2.1.310, its requests carried
 *  `thinking=None` while a pick of `low` was in force. */
const MODEL_WITHOUT_EFFORT = "claude-haiku-4-5"

/** The id Claude Code gives the nth row this file's engine appends. */
const appendedRow = (at: number): string => `appended-row-${at}`

/** The Dispatch message a person's own words are, where a test hands one over. */
const MESSAGE = "33333333-3333-4333-8333-333333333333"
/** Every row this file's engine appended, oldest first, and every prompt it took. The mod's own
 *  writes are read off these, as the live session's are read off its transcript. */
const appends: { readonly uuid: string; readonly text: string }[] = []
const submits: { readonly text: string; readonly asUser: boolean }[] = []
/** What the engine's next `$.prompt.submit` answers, where a test wants a prompt refused, and
 *  what holds that call open, where a test wants a message handed over while it is in flight. */
let dropNextPrompt: string | undefined
let holdNextPrompt: PromiseWithResolvers<void> | undefined
/** What holds the engine's next `$.session.append` open, where a test wants the session to move
 *  on while a row of it is still being stored. */
let holdNextAppend: PromiseWithResolvers<void> | undefined
/** This plugin's `$.store`, which is a JSON file on the live binary and a map here: values
 *  round-trip as `JSON.parse(JSON.stringify(value))` and `keys()` is insertion order, both
 *  measured on 2.1.310 (`/legion/scratch/tmp/infra-1026-store`). */
const store = new Map<string, string>()

let session: Session | undefined
let scratch: string | undefined
/** True once the tests are over: the mod's waits end at once from then on, so its poll loop spends
 *  its reconnect backoff instantly on the socket this file has unlinked instead of sleeping 31 s
 *  into the next test file. */
let over = false

/** One request over the socket, as the host's own `$.http.fetch({ socketPath })` makes it. */
function fetchOverSocket(url: string, init: HttpInit = {}): Promise<HttpResponse> {
  const { promise, resolve, reject } = Promise.withResolvers<HttpResponse>()
  const target = new URL(url)
  const sent = httpRequest(
    {
      socketPath: init.socketPath,
      path: target.pathname,
      method: init.method ?? "GET",
      headers: { host: target.host, ...(init.headers ?? {}) },
    },
    (response) => {
      let text = ""
      response.setEncoding("utf8")
      response.on("data", (chunk: string) => {
        text += chunk
      })
      response.on("end", () => {
        const status = response.statusCode ?? 0
        const headers: Record<string, string> = {}
        for (const [name, value] of Object.entries(response.headers)) {
          if (typeof value === "string") headers[name] = value
        }
        resolve({ status, ok: status >= 200 && status < 300, headers, text })
      })
    },
  )
  sent.on("error", reject)
  sent.end(init.body)
  return promise
}

/** Every request the mod made on the socket, as its `$.http.fetch` was given it. */
const requested: { readonly method: string; readonly instance: string | undefined }[] = []

/** The `$` the hooks are given: what the mod calls on it, and nothing else. A call no test makes
 *  rejects rather than answering something made up. */
function engineOn(directory: string): EngineInterface {
  const unused = (call: string): Promise<never> =>
    Promise.reject(new Error(`$.${call} is no call of this test`))
  return {
    session: {
      id: async () => SESSION_ID,
      version: async () => ({ version: MOD_MIN_CLAUDE_VERSION }),
      model: async () => MODEL_WITH_EFFORT,
      // What Claude Code answers a plugin's own append with, measured in the issue's spike on
      // 2.1.310: the row as stored, which the engine marks `isMeta` and gives the user role,
      // and its id (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q1q3q5.jsonl:36`). No
      // `session.append` hook runs for it: Claude Code does not re-enter a mod's own, which is
      // why the mod reports the row itself.
      append: async (args) => {
        const heldAppend = holdNextAppend
        holdNextAppend = undefined
        if (heldAppend !== undefined) await heldAppend.promise
        const uuid = appendedRow(appends.length + 1)
        appends.push({ text: rowText(args.message.content), uuid })
        return { message: { ...args.message, isMeta: true, role: "user" } as const, uuid }
      },
    },
    prompt: {
      submit: async (args) => {
        const held = holdNextPrompt
        holdNextPrompt = undefined
        if (held !== undefined) await held.promise
        const drop = dropNextPrompt
        dropNextPrompt = undefined
        if (drop !== undefined) return { drop }
        submits.push({ asUser: args.asUser === true, text: args.text })
        // Claude Code writes the prompt's row, and runs this mod's `session.append` hook for
        // it, before the call resolves: the row landed 15 ms to 26 ms ahead of the answer in
        // all six submits recorded on 2.1.310
        // (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q1q3q5.jsonl:236-239`,
        // `logs/mod3-q2-310.jsonl:51-52,86-87`, `logs/mod2.jsonl:144-145`).
        await promptRowWritten(args.text, args.asUser === true)
        return { text: args.text }
      },
    },
    command: { list: () => unused("command.list"), run: () => unused("command.run") },
    config: { list: () => unused("config.list"), set: () => unused("config.set") },
    model: { fork: () => unused("model.fork") },
    http: {
      fetch: (url, init) => {
        requested.push({
          method: init?.method ?? "GET",
          instance: init?.headers?.[MOD_INSTANCE_HEADER],
        })
        return fetchOverSocket(url, init)
      },
    },
    env: { get: async (name) => (name === "XDG_RUNTIME_DIR" ? directory : undefined) },
    clock: {
      sleep: async (ms) => {
        if (!over) await Bun.sleep(ms)
      },
    },
    // This plugin's own store, which 2.1.310 keeps as a JSON file: a value round-trips as
    // `JSON.parse(JSON.stringify(value))` and `keys()` answers in insertion order (the issue's
    // store spike, `/legion/scratch/tmp/infra-1026-store/README.md`).
    store: {
      get: async (key) => {
        const held = store.get(key)
        return held === undefined ? undefined : JSON.parse(held)
      },
      set: async (key, value) => {
        store.set(key, JSON.stringify(value))
      },
      delete: async (key) => {
        store.delete(key)
      },
      keys: async () => [...store.keys()],
    },
  }
}

/** The text of a row's blocks, joined, as this file's engine records what it was handed. */
function rowText(content: readonly { readonly type: string; readonly [key: string]: unknown }[]) {
  return content.map((block) => (typeof block["text"] === "string" ? block["text"] : "")).join("\n")
}

/** A row id for each prompt Claude Code takes here. */
let promptRows = 0

/** The row Claude Code writes for a prompt a plugin submitted, run through this mod's own
 *  `session.append` hook as the engine runs it: by the `prompt` door, under the plugin's
 *  origin, with Claude Code's own framing in front of a submit that named no `asUser`
 *  (measured on 2.1.310, `logs/mod3-q7-int.jsonl:52-53`). */
async function promptRowWritten(text: string, asUser: boolean): Promise<string> {
  promptRows += 1
  const uuid = `prompt-row-${promptRows}`
  const written = asUser ? text : `The claude-envoy plugin sent a message:\n${text}`
  const { appended, engine } = started()
  await appended(
    engine,
    {
      door: "prompt",
      message: { content: [{ text: written, type: "text" }], role: "user", type: "user" },
      origin: { kind: "plugin", name: "claude-envoy" },
      uuid,
    },
    async (e) => ({ message: e.message, uuid: e.uuid }),
  )
  return uuid
}

/** The session, or the reason there is none to drive. */
function started(): Session {
  if (session === undefined) throw new Error("the session did not start")
  return session
}

/** The engine beneath the hook: it keeps the request it was handed and streams one text chunk. */
function beneath(
  seen: TurnStepInput[],
): (e: TurnStepInput) => HookStream<TurnStepChunk, TurnStepResult> {
  return (e: TurnStepInput): HookStream<TurnStepChunk, TurnStepResult> => {
    seen.push(e)
    const result: TurnStepResult = { turnId: e.turnId, index: e.index, stopReason: "end_turn" }
    const stream = (async function* (): AsyncGenerator<TurnStepChunk, TurnStepResult> {
      yield { kind: "text", index: 0, text: "ready" }
      return result
    })()
    return Object.assign(stream, { result: Promise.resolve(result) })
  }
}

/** Run one step through the hook as Claude Code runs it - the stream read to its end - and answer
 *  with the request that reached the engine beneath. */
async function runStep(input: TurnStepInput): Promise<TurnStepInput> {
  const seen: TurnStepInput[] = []
  const { engine, step } = started()
  const chunks: TurnStepChunk[] = []
  for await (const chunk of step(engine, input, beneath(seen))) chunks.push(chunk)
  expect(chunks).toHaveLength(1)
  const requested = seen[0]
  if (requested === undefined) throw new Error("the hook made no request")
  return requested
}

/** The step snapshot the server reads once the step is whole, with everything sent before it -
 *  the hello, the model and the step's streaming snapshots - passed over. */
async function finishedStep(): Promise<Extract<ModEvent, { kind: "step" }>> {
  const { server } = started()
  for (;;) {
    const next = await server.events.next()
    if (next.done === true) throw new Error("the link ended before the step was whole")
    if (next.value.kind === "step" && next.value.done) return next.value
  }
}

/** The event the mod sends for the row `uuid`, with everything before it passed over. */
async function rowReported(uuid: string): Promise<Extract<ModEvent, { kind: "append" }>> {
  const { server } = started()
  for (;;) {
    const next = await server.events.next()
    if (next.done === true) throw new Error("the link ended before the row arrived")
    if (next.value.kind === "append" && next.value.uuid === uuid) return next.value
  }
}

/** Every event the server read up to and including the one `holds` names, oldest first: what
 *  the live view would have been handed over that stretch of the session. The mod posts its
 *  events in one queue, oldest first, so an event the mod sent before that one is in here. */
async function eventsThrough(holds: (event: ModEvent) => boolean): Promise<ModEvent[]> {
  const { server } = started()
  const seen: ModEvent[] = []
  for (;;) {
    const next = await server.events.next()
    if (next.done === true) throw new Error("the link ended before the event arrived")
    seen.push(next.value)
    if (holds(next.value)) return seen
  }
}

/** Poll until `holds`, which is how this file waits on work the mod does off a hook: the
 *  prompt at unread rows goes out after `turn.complete` has returned. */
async function until(what: string, holds: () => boolean): Promise<void> {
  for (let attempt = 0; attempt < 400; attempt += 1) {
    if (holds()) return
    await Bun.sleep(5)
  }
  throw new Error(`timed out waiting for ${what}`)
}

/** `prompt` with the mark it carries put aside, so a prompt the mod submitted and the prompt
 *  it should be compare equal: the mark is minted fresh for every prompt and checked absent
 *  from the messages it quotes (`unreadRowPrompt`), so this leaves every other character of
 *  the two to be compared exactly. A text naming no mark is no prompt of this plugin's. */
function promptLike(prompt: string | undefined): string {
  const text = prompt ?? ""
  const fence = /the mark \[([0-9a-z]+)\]/.exec(text)?.[1]
  if (fence === undefined) throw new Error("the prompt names no mark")
  return text.replaceAll(fence, "<mark>")
}

/** One message into the session, as the server hands one over. */
function message(
  text: string,
  eventId?: string,
  kind: "card" | "person" = "card",
): Promise<ModCallResults["session.message"]> {
  const { server } = started()
  return server.request("session.message", {
    content: [{ text, type: "text" }],
    message: kind === "card" ? { kind, text } : { kind, messageId: MESSAGE, text },
    ...(eventId === undefined ? {} : { eventId }),
  })
}

/** A turn, started and ended with the steps `requests` names, as Claude Code drives the hooks. */
async function turn(turnId: string, requests: number, inside?: () => Promise<void>): Promise<void> {
  const { engine, turnStart, turnComplete } = started()
  await turnStart(engine, { text: "go", turnId }, async (e) => ({ turnId: e.turnId }))
  for (let index = 0; index < requests; index += 1) {
    await runStep({ turnId, index, model: MODEL_WITH_EFFORT, messageCount: 2 })
  }
  await inside?.()
  await turnComplete(
    engine,
    { answer: "done", durationMs: 1, isAborted: false, reason: "answer", turnId },
    async (e) => ({ text: e.answer }),
  )
}

beforeAll(async () => {
  const directory = await mkdtemp(join(tmpdir(), "mod-"))
  scratch = directory
  const server = await startModLink({
    sessionId: SESSION_ID,
    env: { XDG_RUNTIME_DIR: directory },
  })
  const hooks = new Map<string, unknown>()
  const collect: On = (pattern: string, ...rest: unknown[]): void => {
    hooks.set(pattern, rest[rest.length - 1])
  }
  register(collect)
  const engine = engineOn(directory)
  // Captured before `session.start` runs: the engine's own `$.prompt.submit` runs the
  // `session.append` hook for the prompt's row, as Claude Code does.
  session = {
    appended: hooks.get("session.append") as SessionAppendHook,
    engine,
    server,
    step: hooks.get("turn.step") as StepHook,
    turnComplete: hooks.get("turn.complete") as TurnCompleteHook,
    turnStart: hooks.get("turn.start") as TurnStartHook,
  }
  const start = hooks.get("session.start") as SessionStartHook
  await start(engine, { cwd: directory, isInteractive: true }, async (e) => ({ cwd: e.cwd }))
  // The mod probes the socket off the hook, so the link is up a moment after `session.start`
  // returns; the server says when, rather than this file guessing a delay.
  while (!server.connected()) await Bun.sleep(5)
})

afterAll(async () => {
  over = true
  await session?.server.close()
  session = undefined
  if (scratch !== undefined) await rm(scratch, { recursive: true, force: true })
})

// One server serves one mod: it keeps the first instance that names itself in a post and refuses
// every other, so two Claude Code processes booted on one session id do not share a live view or
// a person's Send (INFRA-1026 review).
test("every post the mod makes names the one instance this process's mod is", () => {
  const posts = requested.filter(({ method }) => method === "POST")
  expect(posts.length).toBeGreaterThan(0)
  const instances = new Set(posts.map(({ instance }) => instance))
  expect(instances.size).toBe(1)
  expect([...instances][0]).toMatch(/^[0-9a-z]+-[0-9a-z]+$/)
  // The liveness probe claims nothing: it is how a mod finds a server, not how it holds one.
  const probes = requested.filter(({ method }) => method === "GET")
  expect(probes.length).toBeGreaterThan(0)
  expect(probes.every(({ instance }) => instance === undefined)).toBe(true)
})

// Measured live on 2.1.310: after a pick of `low` the requests of `claude-haiku-4-5` carried
// `thinking=None`, the model taking no effort whatever the mod set, while the mod went on
// reporting `low` on every step and the live view's header read `low` for a session thinking at no
// level at all. The step reports the request that ran, so the pick is left off a request Claude
// Code names no effort for, and off its step (INFRA-1026).
test("a step of a model with no effort of its own runs and reports none, pick or no pick", async () => {
  const { server } = started()
  expect(await server.request("set-effort", { effort: "low" })).toEqual({ effort: "low" })

  const requested = await runStep({
    turnId: "turn-1",
    index: 0,
    model: MODEL_WITHOUT_EFFORT,
    messageCount: 2,
  })

  // The step is read off the link before anything is asserted: a test that left its own
  // snapshots unread would hand them to the next one, whose failure would then name this step.
  const sent = await finishedStep()
  expect(requested.effort).toBeUndefined()
  expect(sent.model).toBe(MODEL_WITHOUT_EFFORT)
  expect(sent.effort).toBeUndefined()
})

// The other half of the same rule, and what makes a thinking pick work at all: a model Claude Code
// names an effort for takes the picked one in place of it, and the step says so.
test("a step of a model with an effort of its own runs and reports the picked one", async () => {
  const { server } = started()
  expect(await server.request("set-effort", { effort: "low" })).toEqual({ effort: "low" })

  const requested = await runStep({
    turnId: "turn-2",
    index: 0,
    model: MODEL_WITH_EFFORT,
    effort: "max",
    messageCount: 4,
  })

  const sent = await finishedStep()
  expect(requested.effort).toBe("low")
  expect(sent.model).toBe(MODEL_WITH_EFFORT)
  expect(sent.effort).toBe("low")
})

// Claude Code does not re-enter a mod's own `session.append` hook for a row that mod appended:
// measured in the issue's spike, where two `$.session.append` calls resolved with their rows
// and neither uuid ever reached a hook
// (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q1q3q5.jsonl:36,64`). So the row would reach
// the live view's mapper nowhere at all, and a card delivered during a turn was drawn nowhere
// (INFRA-1026, handoff v6). The mod reports it itself, out of what the call answered, stamped
// with what it wrote the row for.
test("a message handed to a running turn is a row, reported as the row this plugin wrote", async () => {
  const card = "Dispatch message from alice: the migration is approved."
  const { engine, turnStart, turnComplete } = started()
  await turnStart(engine, { text: "go", turnId: "turn-row" }, async (e) => ({ turnId: e.turnId }))
  await runStep({ turnId: "turn-row", index: 0, model: MODEL_WITH_EFFORT, messageCount: 2 })

  const written = await message(card)
  expect(written).toEqual({ as: "row", uuid: appendedRow(1) })

  const row = await rowReported(appendedRow(1))
  // The door and the origin the engine gives a plugin's own append, and the stamp the server
  // claims the row by: `message` says this row is the one message became, which is the one
  // `isMeta` row the live view draws (`../src/mod-session.ts`, `appendedRow`). Another
  // installed mod's append reaches this hook under that mod's own name and carries no stamp.
  expect(row.door).toBe("note")
  expect(row.origin).toEqual({ kind: "plugin", name: "claude-envoy" })
  expect(row.wrote).toBe("message")
  // The row as the call answered it, `isMeta` and all.
  expect(row.message).toEqual({
    content: [{ text: card, type: "text" }],
    isMeta: true,
    role: "user",
    type: "user",
  })

  // A request went out before the row and none after it, so the turn's end points the model at
  // it with the one prompt that quotes it in full, and the row that prompt writes is stamped
  // `pointer`: the live view draws nothing of it, since it quotes a row already drawn.
  await turnComplete(
    engine,
    { answer: "done", durationMs: 1, isAborted: false, reason: "answer", turnId: "turn-row" },
    async (e) => ({ text: e.answer }),
  )
  await until("the prompt at the unread row", () => submits.length === 1)
  expect(submits[0]?.asUser).toBe(false)
  expect(promptLike(submits[0]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "card", text: card }])),
  )
  expect((await rowReported("prompt-row-1")).wrote).toBe("pointer")
  // The prompt started its turn, which reads it: the mod stops holding rows for it.
  await turn("turn-after-pointer", 1)
})

// The server is at-least-once: a frame it leaves unanswered is sent again, and the copy reaches
// the mod as the same Envoy event id. The write is idempotent by that id, so the engine sees one
// message however many copies arrive (INFRA-1026 review, round eleven). Two copies at once are
// the case the store cannot answer on its own: two `get`s of an unset key both answered
// undefined, 0 ms and 2 s apart, in one session and across two
// (`/legion/scratch/tmp/infra-1026-store/README.md`), so the mod chains them per id.
test("the same event id handed over twice is written once, at once or later", async () => {
  const card = "an architect's answer, sent twice"
  const before = appends.length
  await turn("turn-once", 1, async () => {
    const [first, second] = await Promise.all([
      message(card, "evt-once"),
      message(card, "evt-once"),
    ])
    expect(second).toEqual(first)
    // The copy that arrives after the first write has finished reads the record the store kept.
    expect(await message(card, "evt-once")).toEqual(first)
    expect(appends.length).toBe(before + 1)
  })
  // One row, so one message went unread, and the turn's end points at it once.
  await until("the prompt at the unread row", () => submits.length === 2)
  await turn("turn-after-once", 1)
})

// Repro `R-B` (INFRA-1026 review, round eleven): the server read "which request has gone out"
// off a `step-request` event and "the row exists" off the append's answer, and the two cross on
// the wire, so a row stored after the turn's last request looked read by it and was never
// quoted. In the mod the two are one order: the request is counted at the top of the
// `turn.step` hook and the row is watched when the append resolves, so the row is unread.
test("a row stored after the turn's last request is quoted, though its step answered first", async () => {
  const card = "the migration guard is ready for review"
  const sent = submits.length
  await turn("turn-unread", 2, async () => {
    expect((await message(card)).as).toBe("row")
  })
  await until("the prompt at the unread row", () => submits.length === sent + 1)
  expect(promptLike(submits[sent]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "card", text: card }])),
  )
  await turn("turn-after-unread", 1)
})

// Repro `R-C` (INFRA-1026 review, round three):
// a row stored behind a submit that the session then drops waited for a turn that never starts,
// so nothing ever pointed the model at it. The mod gives the prompt it handed over
// `TURN_START_MS` to start its turn, and points at the rows behind a prompt that started none.
test("a row stored behind a prompt the session dropped is pointed at", async () => {
  const sent = submits.length
  const held = Promise.withResolvers<void>()
  holdNextPrompt = held
  // Nothing is running, so this message is the prompt — and the engine holds it open.
  const prompting = message("the first card, which the session will refuse")
  await until("the submit to be in flight", () => holdNextPrompt === undefined)
  // Handed over while that prompt is in flight: a row of the turn it is about to start, never
  // a second prompt behind it.
  const behind = "the second card, stored behind the first"
  expect((await message(behind)).as).toBe("row")

  dropNextPrompt = "a UserPromptSubmit hook blocked it"
  held.resolve()
  expect(await prompting).toEqual({ as: "prompt", deny: "a UserPromptSubmit hook blocked it" })

  await until("the prompt at the row the dropped prompt left", () => submits.length === sent + 1)
  expect(promptLike(submits[sent]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "card", text: behind }])),
  )
  await turn("turn-after-dropped", 1)
})

// The same row one tick later (INFRA-1026 review, round five): the prompt it was stored behind
// is dropped while its own store is still in flight. The drop's `stopPrompting` finds nothing,
// because the row is not watched yet, so the end of that store is the last moment anything can
// catch it. Read off the turn as it stood before the store — `undefined`, since it was a prompt
// and not a turn that was running — that moment said there was nothing to catch, and the row sat
// in the watch with its Envoy frame already answered, waiting for a turn nobody would start.
test("a row stored behind a prompt dropped while that row was being stored is pointed at", async () => {
  const sent = submits.length
  const held = Promise.withResolvers<void>()
  holdNextPrompt = held
  // Nothing is running, so this message is the prompt — and the engine holds it open.
  const prompting = message("the first card, which the session will refuse", "evt-strand-1")
  await until("the submit to be in flight", () => holdNextPrompt === undefined)

  // Handed over while that prompt is in flight: a row — and the engine holds its store open.
  const heldRow = Promise.withResolvers<void>()
  holdNextAppend = heldRow
  const behind = "the second card, stored while the first was being dropped"
  const storing = message(behind, "evt-strand-2")
  await until("the store of that row to be in flight", () => holdNextAppend === undefined)

  // The prompt is dropped with the row still in flight: no turn ever runs, and the drop sees
  // nothing of the row.
  dropNextPrompt = "a UserPromptSubmit hook blocked it"
  held.resolve()
  expect(await prompting).toEqual({ as: "prompt", deny: "a UserPromptSubmit hook blocked it" })
  heldRow.resolve()
  expect((await storing).as).toBe("row")

  await until("the prompt at the row the dropped prompt left", () => submits.length === sent + 1)
  expect(promptLike(submits[sent]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "card", text: behind }])),
  )
  await turn("turn-after-strand", 1)
})

// An idle session takes the message as the prompt of a new turn and nothing else: an appended
// row starts nothing, so a card only appended would wait for the person to type, and a card
// appended *and* quoted reached the model twice (INFRA-1026, handoff v6). The row Claude Code
// writes for that prompt is the prompt's own: the mod stamps it for the live view and answers
// the write with no row at all (`../hooks/mod-protocol.ts`, `ModCallResults["session.message"]`).
test("an idle session takes the message as its prompt, answered as a prompt and no row", async () => {
  const sent = submits.length
  const stored = appends.length
  const card = "a teammate's message, to an idle session"

  expect(await message(card)).toEqual({ as: "prompt" })
  expect(appends.length).toBe(stored)
  // The prompt's own row is claimed and stamped all the same: that stamp is what the live view
  // reads such a row by (`ModEvent`'s `wrote`).
  expect((await rowReported(`prompt-row-${promptRows}`)).wrote).toBe("message")
  // Without `asUser`: nobody at this session typed a card. A person's own words go with it.
  expect(submits[sent]).toEqual({ asUser: false, text: card })
  await turn("turn-after-card", 1)

  expect((await message("ship it", undefined, "person")).as).toBe("prompt")
  expect(submits[sent + 1]).toEqual({ asUser: true, text: "ship it" })
  await turn("turn-after-person", 1)
})

// A subagent runs a loop of its own, and Claude Code says so with one key: measured live on
// 2.1.310 (`/legion/scratch/tmp/infra-1026-turn`), a subagent's `turn.start`, `turn.step`,
// `turn.complete` and `session.append` carry `agentId` — `a` and 16 hex digits — while the main
// loop's carry no such key at all, in the same run and the same log, and a foreground
// subagent's whole turn opens and closes inside the main turn, between two of its steps.
// Counted as the main turn's, that turn's `turn.complete` cleared the turn a message joins, so
// the next message went in as a prompt that waits for the whole running turn, and the
// subagent's steps and rows reached the live view as the main conversation's, which the spec
// puts out of scope ("their work shows as the parent's tool call").
test("a subagent's turn inside the main turn is none of the main turn's", async () => {
  const { appended, engine, turnStart, turnComplete } = started()
  const sent = submits.length
  const agentId = "a97c898d2a50d5679"
  const card = "the reviewer answered while a subagent was running"
  await turnStart(engine, { text: "go", turnId: "turn-main" }, async (e) => ({ turnId: e.turnId }))
  await runStep({ turnId: "turn-main", index: 0, model: MODEL_WITH_EFFORT, messageCount: 2 })

  // The subagent's whole turn, between two steps of the main one: its own turn id, and
  // `agentId` on every event of it.
  await turnStart(engine, { agentId, text: "search the tree", turnId: "turn-sub" }, async (e) => ({
    turnId: e.turnId,
  }))
  await runStep({
    agentId,
    turnId: "turn-sub",
    index: 0,
    model: MODEL_WITH_EFFORT,
    messageCount: 2,
  })
  await appended(
    engine,
    {
      agentId,
      door: "response",
      message: {
        content: [{ text: "the subagent's own answer", type: "text" }],
        role: "assistant",
        type: "assistant",
      },
      origin: { kind: "model", model: MODEL_WITH_EFFORT },
      uuid: "row-subagent",
    },
    async (e) => ({ message: e.message, uuid: e.uuid }),
  )
  await turnComplete(
    engine,
    {
      agentId,
      answer: "found it",
      durationMs: 1,
      isAborted: false,
      reason: "answer",
      turnId: "turn-sub",
    },
    async (e) => ({ text: e.answer }),
  )

  // The main turn is still the turn: the message is a row of it, read at its next request,
  // rather than a prompt waiting behind everything the main turn has left to do.
  expect((await message(card)).as).toBe("row")

  // No request of the main turn's read that row — the subagent's was not one — so the main
  // turn's end points the model at it.
  await turnComplete(
    engine,
    { answer: "done", durationMs: 1, isAborted: false, reason: "answer", turnId: "turn-main" },
    async (e) => ({ text: e.answer }),
  )
  await until("the prompt at the unread row", () => submits.length === sent + 1)
  expect(promptLike(submits[sent]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "card", text: card }])),
  )

  // And nothing of that loop reached the live view: no turn of its own, no step of its own, no
  // row of its own.
  const stream = await eventsThrough(
    (event) => event.kind === "turn-complete" && event.turnId === "turn-main",
  )
  expect(
    stream.filter(
      (event) =>
        ("turnId" in event && event.turnId === "turn-sub") ||
        (event.kind === "append" && event.uuid === "row-subagent"),
    ),
  ).toEqual([])
  await turn("turn-after-subagent", 1)
})

// Measured in the same run: a background subagent's `turn.step` arrives while the main turn is
// running, with `agentId`, and its turn can still be open after the main turn completed. Its
// request is none of this session's, so it raises no count here — counted, a row stored after
// the main turn's last request looked read by it and was never quoted, which is repro `R-B`
// again. The mod never saw this turn start: when the plugin itself spawns a subagent, that
// loop's `turn.start` and `turn.step` never reach the spawning plugin, so the rule is the key
// the event carries and not a turn this mod watched begin.
test("a background subagent's step is none of the main turn's requests", async () => {
  const { engine, turnStart, turnComplete } = started()
  const sent = submits.length
  const card = "an issue event, stored after the main turn's last request"
  await turnStart(engine, { text: "go", turnId: "turn-bg" }, async (e) => ({ turnId: e.turnId }))
  await runStep({ turnId: "turn-bg", index: 0, model: MODEL_WITH_EFFORT, messageCount: 2 })

  expect((await message(card)).as).toBe("row")
  await runStep({
    agentId: "a0f31c7b5d2e4a908",
    turnId: "turn-bg-sub",
    index: 4,
    model: MODEL_WITH_EFFORT,
    messageCount: 9,
  })

  await turnComplete(
    engine,
    { answer: "done", durationMs: 1, isAborted: false, reason: "answer", turnId: "turn-bg" },
    async (e) => ({ text: e.answer }),
  )
  await until("the prompt at the unread row", () => submits.length === sent + 1)
  expect(promptLike(submits[sent]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "card", text: card }])),
  )
  await turn("turn-after-background", 1)
})

// The pick is the session's, so it rides on every request of the session, a subagent's loop
// included (requirement R8, `docs/site/src/content/docs/dispatch/agents.md`). Measured on
// 2.1.310, a subagent's `turn.step` carries the session's own effort, so a hook that passed one
// straight through left the main loop at the picked level while every Task subagent ran at the
// level Claude Code had saved, under a header reading the pick (INFRA-1026 review, round five).
// What a subagent's step is still none of is this session's bookkeeping: the count the unread
// rule reads, and the live view.
test("a subagent's step takes the session's pick and is still none of this session's requests", async () => {
  const { engine, server, turnStart, turnComplete } = started()
  expect(await server.request("set-effort", { effort: "low" })).toEqual({ effort: "low" })
  const sent = submits.length
  const card = "a card stored after the main turn's last request"
  await turnStart(engine, { text: "go", turnId: "turn-pick" }, async (e) => ({ turnId: e.turnId }))
  await runStep({
    turnId: "turn-pick",
    index: 0,
    model: MODEL_WITH_EFFORT,
    effort: "max",
    messageCount: 2,
  })
  expect((await message(card)).as).toBe("row")

  // The subagent's own step, with the effort Claude Code saved for this session on it: the
  // request that goes out carries the pick instead, as the main loop's does.
  const requested = await runStep({
    agentId: "a0f31c7b5d2e4a908",
    turnId: "turn-pick-sub",
    index: 0,
    model: MODEL_WITH_EFFORT,
    effort: "medium",
    messageCount: 9,
  })
  expect(requested.effort).toBe("low")

  // And it raised no count of this session's: the row stored before it is still unread, so the
  // main turn's end points the model at it.
  await turnComplete(
    engine,
    { answer: "done", durationMs: 1, isAborted: false, reason: "answer", turnId: "turn-pick" },
    async (e) => ({ text: e.answer }),
  )
  await until("the prompt at the unread row", () => submits.length === sent + 1)
  expect(promptLike(submits[sent]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "card", text: card }])),
  )

  // And the live view was told nothing of that step: the spec puts a subagent's work inside the
  // parent's tool call.
  const stream = await eventsThrough(
    (event) => event.kind === "turn-complete" && event.turnId === "turn-pick",
  )
  expect(stream.filter((event) => "turnId" in event && event.turnId === "turn-pick-sub")).toEqual(
    [],
  )
  await turn("turn-after-pick", 1)
})

// A person's message whose answer was late is sent again by Envoy and arrives as a card, the
// accept having spent its Dispatch attempt (`../src/user-turn-router.ts`). The two carry the
// one Envoy event id, so they are one write and the resend is answered with the row the
// person's own words became.
test("a person's message and the card its resend becomes are one write", async () => {
  const words = "ship it, and tell me when it is out"
  const stored = appends.length
  const sent = submits.length
  await turn("turn-resend", 1, async () => {
    const written = await message(words, "evt-person", "person")
    expect(written.as).toBe("row")
    expect(await message(words, "evt-person")).toEqual(written)
    expect(appends.length).toBe(stored + 1)
  })
  // One row, so one message went unread, and the turn's end quotes the person's own words once.
  await until("the prompt at the unread row", () => submits.length === sent + 1)
  expect(promptLike(submits[sent]?.text)).toBe(
    promptLike(unreadRowPrompt([{ kind: "person", messageId: MESSAGE, text: words }])),
  )
  await turn("turn-after-resend", 1)
})

// A prompt the engine took is the write itself, and it names no row of this mod's: the answer
// carries no uuid, and no `deny`, because the session has the prompt. The Envoy event is
// recorded under it all the same, which is what keeps the resend of that event from writing
// the message a second time — unrecorded, it was written twice, and for a person's own
// message Dispatch told them their session had dropped a prompt it took (INFRA-1026 review).
test("a prompt the session took is recorded, so the resend of its event writes nothing", async () => {
  const words = "the staging box is yours for the afternoon"
  const sent = submits.length
  const stored = appends.length

  // No uuid, because there is no row to name — and no `deny`, because the session has it.
  expect(await message(words, "evt-taken", "person")).toEqual({ as: "prompt" })
  expect(submits[sent]).toEqual({ asUser: true, text: words })

  // The write was recorded, so the card Envoy's resend becomes is answered from the record and
  // the session is written nothing further.
  expect(await message(words, "evt-taken")).toEqual({ as: "prompt" })
  expect(submits.length).toBe(sent + 1)
  expect(appends.length).toBe(stored)
  await turn("turn-after-taken", 1)
})

// A submit resolves when the engine takes the prompt, and one that waits behind a turn already
// running took 8.5 s, 11.2 s and 12.4 s on 2.1.310
// (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:136-147`) while the row still landed
// 15 ms to 26 ms before that answer. So the watch for the row is bounded from the answer and
// not from the call: bounded from the call, a prompt the engine held longer than
// `PROMPT_ROW_MS` had its row arrive unclaimed, the pointer prompt's row went unstamped, and
// the live view drew every message that prompt quotes a second time (INFRA-1026 review, round
// six).
test("a pointer prompt the engine holds past the row window still has its row stamped pointer", async () => {
  const card = "the deploy window moved to Thursday"
  const sent = submits.length
  const { engine, turnStart, turnComplete } = started()
  await turnStart(engine, { text: "go", turnId: "turn-slow" }, async (e) => ({ turnId: e.turnId }))
  await runStep({ turnId: "turn-slow", index: 0, model: MODEL_WITH_EFFORT, messageCount: 2 })
  expect((await message(card)).as).toBe("row")

  // The turn's end points the model at that unread row, and the engine holds the prompt it
  // submits for 2.5 s — past `PROMPT_ROW_MS` (2 s) — before taking it and writing its row.
  // The wait is real because the window is: the mod spends it on the host's own clock
  // (`$.clock.sleep`, which this file answers with `Bun.sleep`), and there is no handle on
  // that wait to advance, so crossing the window means crossing it in real time.
  const held = Promise.withResolvers<void>()
  holdNextPrompt = held
  await turnComplete(
    engine,
    { answer: "done", durationMs: 1, isAborted: false, reason: "answer", turnId: "turn-slow" },
    async (e) => ({ text: e.answer }),
  )
  await until("the pointer submit to be in flight", () => holdNextPrompt === undefined)
  await Bun.sleep(2_500)
  held.resolve()
  await until("the pointer submitted", () => submits.length === sent + 1)

  expect((await rowReported(`prompt-row-${promptRows}`)).wrote).toBe("pointer")
  await turn("turn-after-slow-pointer", 1)
}, 20_000)
