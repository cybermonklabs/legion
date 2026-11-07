/**
 * The Claude Code mod this plugin loads through the `modules` key of `./hooks.json`: Claude Code
 * runs it from source, in an environment of its own, and it sees every hook of the session.
 *
 * It carries the session to this plugin's MCP server over the Unix socket both sides name the
 * same way (`./mod-protocol`): events one way, the server's requests the other. The server makes
 * the Agents page's live view out of those events (`../src/agent-stream-runner.ts`) and routes a
 * person's message back through those requests (`../src/user-turn-router.ts`); nothing of either
 * is here.
 *
 * Nor is what a message becomes once a request carries it in: a row of the turn running or the
 * prompt of a new turn, the one prompt at the rows no request read, and the write that happens
 * once however many copies of an Envoy event arrive. Those rules are `./mod-writer.ts`, which
 * this file gives the host, the event queue, the clock and the session id; what is left here is
 * the transport and the hooks.
 *
 * Four rules hold everywhere below:
 *
 *   - A hook always calls `next(e)` and returns its result. Every call on the engine and every
 *     post is wrapped, so a throw of the link's can never end a turn or a hook.
 *   - A mod that finds no server is silent: it connects over a bounded backoff and then stops for
 *     good, having changed nothing about the session. That is the release window, where a person
 *     has this mod beside the previously built server. It also has nowhere to complain to: the
 *     only surfaces a mod can write are the person's screen and the model's context, and this
 *     change adds to neither. The server says on its stderr what it could not do.
 *   - `$` is never passed, returned or read: Claude Code scans this module's source before it
 *     loads it and refuses one that spells `$` as anything but `$.noun.method(...)` at a call
 *     site (`claude plugin validate`). So the link below is handed a `ModHost`, one closure per
 *     call, built in the `session.start` hook where `$` is in scope. The writer is handed the
 *     part of it that writes (`./mod-writer`, `WriterHost`), for the same reason.
 *   - A hook whose event carries `agentId` passes it through and does nothing else: that event
 *     is a subagent's loop, not this session's turn (`ofSubagent`).
 *
 * It imports nothing but `./mod-protocol` and `./mod-writer`, which import nothing else at all:
 * a mod can import neither a `node:` builtin nor an npm package, and `$.http.fetch` is its only
 * way out.
 */
import type {
  CommandInfo,
  CommandRunArgs,
  CommandRunResult,
  ConfigRow,
  ConfigSetArgs,
  ConfigSetResult,
  HttpInit,
  HttpResponse,
  ModelForkRequest,
  ModelForkResult,
  On,
  SessionAppendDoor,
  SessionAppendHook,
  SessionVersion,
  TurnStepChunk,
  TurnStepInput,
  TurnStepResult,
} from "claude-code"
import {
  MOD_APPEND_DOORS,
  MOD_HEALTH_MARKER,
  MOD_INSTANCE_HEADER,
  MOD_MIN_CLAUDE_VERSION,
  MOD_ROUTE_EVENTS,
  MOD_ROUTE_HEALTH,
  MOD_ROUTE_REQUESTS,
  MOD_ROUTE_RESULTS,
  MOD_STATUS_TAKEN,
  MOD_URL_BASE,
  type ModAppendDoor,
  type ModAppendMessage,
  type ModAppendOrigin,
  type ModEffort,
  type ModEvent,
  type ModEventsBody,
  type ModRequest,
  type ModResult,
  type ModResultsBody,
  type ModResultValue,
  type ModStepPart,
  type ModStopReason,
  type ModTurnReason,
  modVersionAtLeast,
  resolveModSocketPath,
} from "./mod-protocol"
import { createMessageWriter, type WriterHost } from "./mod-writer"

/** Whether a row's door is one of the conversation's that this mod watches: the eight it
 *  registers for, which is every door but the ones the host hides from a mod a person installed
 *  and `queue`, which is no row of the conversation. */
function isWatchedDoor(door: SessionAppendDoor): door is ModAppendDoor {
  const watched: readonly string[] = MOD_APPEND_DOORS
  return watched.includes(door)
}

/** How long to wait before each further attempt at `GET /healthz`, in milliseconds: eleven probes
 *  over about 31 seconds, growing. The server binds its socket while the MCP server starts, which
 *  is after Claude Code has run this mod's `session.start`, so the first probes normally miss. */
const CONNECT_DELAYS_MS = [250, 500, 1_000, 2_000, 3_000, 4_000, 5_000, 5_000, 5_000, 5_000]

/** The shortest gap between two snapshots of one `turn.step` while it streams. */
const STEP_SNAPSHOT_MS = 100

/** The shortest a poll pass may take when it answered nothing, so a server that answers at once
 *  without holding (a wrong one, or one that is refusing) cannot spin this loop. */
const POLL_FLOOR_MS = 250

/** How long to watch for the new session id after `session.end`, in tries and their gap: it is
 *  there within about 100 ms of the hook, and the process lives on only after a `/clear`. */
const SESSION_ID_TRIES = 10
const SESSION_ID_WAIT_MS = 30

/** The most events held while no server is connected. The live view's own frames are a later
 *  task's and are rebuilt from the session, so the oldest of an overflow is the one to drop. */
const EVENT_QUEUE_MAX = 2048

/**
 * Everything the link does to the session, one closure per call on `$`: the transport's own
 * calls, and the writer's (`./mod-writer`, `WriterHost`).
 *
 * It exists because `$` may not leave the hook it was given to: the host reads this module's
 * source and refuses a `$` that is passed, bound, spread, returned or read. Each closure spells
 * its call in full at its own call site, which is what the host asks for, and what it then lists
 * as the module's calls and the variables it reads.
 */
interface ModHost extends WriterHost {
  readonly fetch: (url: string, init: HttpInit) => Promise<HttpResponse>
  readonly sleep: (ms: number) => Promise<void>
  readonly sessionId: () => Promise<string>
  readonly version: () => Promise<SessionVersion>
  readonly model: () => Promise<string>
  /** `XDG_RUNTIME_DIR`, the first base the socket path is looked for in. */
  readonly runtimeDirectory: () => Promise<string | undefined>
  /** `CLAUDE_CONFIG_DIR`, Claude Code's own configuration directory, the second base. */
  readonly configDirectory: () => Promise<string | undefined>
  /** `HOME`, whose `.claude` is that configuration directory when the variable names none. */
  readonly homeDirectory: () => Promise<string | undefined>
  readonly runCommand: (args: CommandRunArgs) => Promise<CommandRunResult>
  readonly listCommands: () => Promise<CommandInfo[]>
  readonly fork: (request: ModelForkRequest) => Promise<ModelForkResult>
  readonly setConfig: (args: ConfigSetArgs) => Promise<ConfigSetResult>
  readonly listConfig: () => Promise<ConfigRow[]>
}

/** What the hooks below drive: the session's half of the link. The server's half is `ModLink` in
 *  `../src/mod-link.ts`; the two meet on the socket alone. */
interface SessionLink {
  /** Name the socket and start the poll loop, once, at the first `session.start`. */
  readonly begin: (host: ModHost, interactive: boolean) => Promise<void>
  /** Queue one event and start it on its way; never throws. */
  readonly send: (event: ModEvent) => void
  /** A turn started: reported, and taken as the turn a message handed over now is a row of. */
  readonly turnStarted: (turnId: string) => void
  /** A turn ended: reported, and every row of this mod's that no request of that turn read is
   *  pointed at by one prompt (`unreadRowPrompt`). */
  readonly turnEnded: (turnId: string, reason: ModTurnReason) => void
  /** A request of the turn is going out, counted at the top of the `turn.step` hook: a row
   *  stored before this moment is in this request, and one stored after it is in none. */
  readonly stepRequested: () => void
  /** One row of the conversation as the `session.append` hook handed it: stamped where this mod
   *  wrote it, and sent. */
  readonly row: (row: {
    readonly uuid: string
    readonly door: ModAppendDoor
    readonly origin: ModAppendOrigin
    readonly message: ModAppendMessage
  }) => void
  /** Watch for the id a `/clear` moves the session to and send it when it arrives. */
  readonly followSessionId: () => Promise<void>
  /** The effort the server last asked for, for the next `turn.step` that names one of its own;
   *  null for none. */
  readonly effortNow: () => ModEffort | null
}

/** `error`'s text, bounded: it crosses as one JSON string into a result the server rejects with. */
function errorText(error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)
  return message.slice(0, 500)
}

/** The requests one `POST /requests` answered with. The only writer of this socket is the server
 *  that bound it, so an item that is shaped like a request at all is the request it says it is;
 *  anything else, and a body that is not an answer at all, is nothing to do. */
function readRequests(text: string): readonly ModRequest[] {
  let body: unknown
  try {
    body = JSON.parse(text)
  } catch {
    return []
  }
  if (typeof body !== "object" || body === null || !("requests" in body)) return []
  const requests = body.requests
  if (!Array.isArray(requests)) return []
  return requests.filter((request: unknown): request is ModRequest => {
    if (typeof request !== "object" || request === null) return false
    if (!("id" in request) || !("call" in request)) return false
    return typeof request.id === "string" && typeof request.call === "string"
  })
}

/** The link to the server, all of it closure state: one mod instance drives one session. */
function createLink(): SessionLink {
  /** This Claude Code process's mod, minted here: the link serves the first instance that names
   *  itself and refuses every other, so two processes that booted on one session id (`claude
   *  -c`, and `claude --resume <id>` in a second terminal) no longer share one server, one live
   *  view and one person's Send (INFRA-1026 review). The time and the random part together are
   *  as distinct as two processes of one session need: they rule out the one other instance this
   *  socket can see. */
  const instance = `${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`
  let host: ModHost | undefined
  let socketPath: string | undefined
  /** True while the last probe or post succeeded. */
  let connected = false
  /** True once the link gave up for good, or decided never to start. */
  let finished = false
  let started = false
  /** The id the socket is named for, which a `/clear` does not move. */
  let currentSessionId: string | undefined
  /** The effort the server last asked for, put on every later `turn.step` that names an effort
   *  of its own; null for none. */
  let effort: ModEffort | null = null
  const queued: ModEvent[] = []
  let flushing = false

  const sleep = async (ms: number): Promise<void> => {
    try {
      await host?.sleep(ms)
    } catch {
      // A refused wait is the host's business; the caller's loop carries on.
    }
  }

  /** The response body of a request that succeeded, or undefined for any failure at all: a socket
   *  that has gone, a refused request, a body that never arrived. A post the server refuses
   *  because another mod holds the link ends this one for good: that server belongs to the
   *  process that got there first, and there is no second socket to try. */
  const fetch = async (route: string, body: string | undefined): Promise<string | undefined> => {
    if (host === undefined || socketPath === undefined) return undefined
    try {
      const response = await host.fetch(
        `${MOD_URL_BASE}${route}`,
        body === undefined
          ? { socketPath }
          : {
              method: "POST",
              headers: { "content-type": "application/json", [MOD_INSTANCE_HEADER]: instance },
              body,
              socketPath,
            },
      )
      if (response.status === MOD_STATUS_TAKEN) {
        finished = true
        connected = false
        // Nothing will read these again: `send` refuses every later event and `flush` posts only
        // while connected.
        queued.length = 0
        return undefined
      }
      return response.ok ? response.text : undefined
    } catch {
      return undefined
    }
  }

  /** Whether a server of ours answers the socket now. */
  const probe = async (): Promise<boolean> => {
    const text = await fetch(MOD_ROUTE_HEALTH, undefined)
    if (text === undefined) return false
    let body: unknown
    try {
      body = JSON.parse(text)
    } catch {
      return false
    }
    if (typeof body !== "object" || body === null || !("link" in body)) return false
    return body.link === MOD_HEALTH_MARKER
  }

  /** Probe until a server answers or the backoff is spent; false means stop for good. */
  const connect = async (): Promise<boolean> => {
    for (const delay of CONNECT_DELAYS_MS) {
      if (await probe()) {
        connected = true
        return true
      }
      await sleep(delay)
    }
    connected = await probe()
    return connected
  }

  /** Post everything queued, oldest first, one batch at a time so no event overtakes another. A
   *  batch that fails is dropped and the link counts as lost: the poll loop reconnects. */
  const flush = async (): Promise<void> => {
    if (flushing) return
    flushing = true
    try {
      while (connected && queued.length > 0) {
        const events = queued.splice(0, queued.length)
        const body = { events } satisfies ModEventsBody
        if ((await fetch(MOD_ROUTE_EVENTS, JSON.stringify(body))) === undefined) {
          connected = false
          return
        }
      }
    } finally {
      flushing = false
    }
  }

  /** Queue one event and start it on its way. Never throws: every hook calls this. */
  const send = (event: ModEvent): void => {
    if (finished) return
    queued.push(event)
    if (queued.length > EVENT_QUEUE_MAX) queued.splice(0, queued.length - EVENT_QUEUE_MAX)
    void flush().catch(() => undefined)
  }

  /** What a message becomes in this session, and what points the model at a row no request
   *  read: the rules this link carries between the socket and the session, in a file of their
   *  own (`./mod-writer`). It reaches `$` through this link's own closures and holds the
   *  turn's state itself, so the two halves share these four and nothing else. */
  const writer = createMessageWriter({
    engine: () => host,
    send,
    sleep,
    sessionId: () => currentSessionId,
  })

  /** Make the call a request names and answer with its own value. */
  const invoke = async (engine: ModHost, request: ModRequest): Promise<ModResultValue> => {
    switch (request.call) {
      case "session.message": {
        const { eventId } = request.args
        return eventId === undefined
          ? await writer.writeMessage(engine, request.args)
          : await writer.writeOnce(engine, eventId, request.args)
      }
      case "command.run":
        return await engine.runCommand(request.args)
      case "model.fork":
        return await engine.fork(request.args)
      case "config.set": {
        const answer = await engine.setConfig(request.args)
        return answer.deny === undefined ? { value: answer.value } : { deny: answer.deny }
      }
      case "session.model":
        return await engine.model()
      case "config.list":
        return await engine.listConfig()
      case "command.list":
        return await engine.listCommands()
      case "set-effort":
        effort = request.args.effort
        return { effort }
    }
  }

  /** Run one request and post its result. A call that throws answers the throw, not nothing. */
  const answer = async (request: ModRequest): Promise<void> => {
    let result: ModResult
    try {
      if (host === undefined) throw new Error("the session is gone")
      result = { id: request.id, ok: true, value: await invoke(host, request) }
    } catch (error) {
      result = { id: request.id, ok: false, error: errorText(error) }
    }
    const body = { results: [result] } satisfies ModResultsBody
    await fetch(MOD_ROUTE_RESULTS, JSON.stringify(body))
  }

  /** Connect, then poll for work until the server goes for good. */
  const pump = async (): Promise<void> => {
    while (!finished) {
      if (!connected && !(await connect())) {
        finished = true
        // Dropped rather than kept: `send` refuses every later event now, and `flush` only posts
        // while connected, so nothing will ever read these again. They are not free to hold - a
        // queued `step` carries its whole accumulated text, so a step that streamed through the
        // backoff leaves one full copy of itself per snapshot pinned for the session's life.
        queued.length = 0
        return
      }
      const pollStartedAt = Date.now()
      await flush()
      const text = await fetch(MOD_ROUTE_REQUESTS, "{}")
      if (text === undefined) {
        // The server stopped, or refused: back to the bounded backoff rather than spinning.
        connected = false
        continue
      }
      const requests = readRequests(text)
      // Each request runs on its own: `session.message` serialises what has to be serialised
      // inside itself — the write for one Envoy event id, and the choice between a row and a
      // prompt, which is made in one tick (`writeOnce`, `writeMessage`). A chain here would
      // instead hold a call behind a `$.command.run` waiting on a dialog at the terminal.
      for (const request of requests) void answer(request).catch(() => undefined)
      if (requests.length === 0 && Date.now() - pollStartedAt < POLL_FLOOR_MS) {
        await sleep(POLL_FLOOR_MS)
      }
    }
  }

  /** Read the session's model and send it; a failure costs nothing. */
  const sendModel = async (engine: ModHost): Promise<void> => {
    try {
      send({ kind: "model", model: await engine.model() })
    } catch {
      // The model is one event, not a reason to fail a hook.
    }
  }

  /** Watch for the id a `/clear` moves the session to, which arrives shortly after the hook. */
  const followSessionId = async (): Promise<void> => {
    for (let attempt = 0; attempt < SESSION_ID_TRIES; attempt += 1) {
      await sleep(SESSION_ID_WAIT_MS)
      let id: string
      try {
        if (host === undefined) return
        id = await host.sessionId()
      } catch {
        return
      }
      if (id !== currentSessionId) {
        currentSessionId = id
        send({ kind: "session-id", sessionId: id })
        return
      }
    }
  }

  /** Name the socket, refuse a Claude Code too old for the hooks and calls this mod is written
   *  against, and start the poll loop. Never throws, and never waits on the connect: a session
   *  does not start more slowly because a server is not up yet. */
  const begin = async (engine: ModHost, interactive: boolean): Promise<void> => {
    if (started) return
    started = true
    host = engine
    try {
      const version = await engine.version()
      if (!modVersionAtLeast(version.version, MOD_MIN_CLAUDE_VERSION)) {
        finished = true
        return
      }
      // The boot id: `$.session.id()` at the first `session.start` is the id Claude Code gave the
      // MCP server as CLAUDE_CODE_SESSION_ID, and a later `/clear` does not move the socket.
      const sessionId = await engine.sessionId()
      currentSessionId = sessionId
      const resolved = resolveModSocketPath(sessionId, {
        XDG_RUNTIME_DIR: await engine.runtimeDirectory(),
        CLAUDE_CONFIG_DIR: await engine.configDirectory(),
        HOME: await engine.homeDirectory(),
      })
      if (resolved.path === undefined) {
        finished = true
        return
      }
      socketPath = resolved.path
      send({ kind: "hello", sessionId, claudeVersion: version.version, interactive })
      void sendModel(engine).catch(() => undefined)
      void pump().catch(() => undefined)
    } catch {
      finished = true
    }
  }

  /** A turn started: the writer takes it as the turn a message handed over now is a row of,
   *  and the server is told. */
  const turnStarted = (turnId: string): void => {
    writer.turnStarted(turnId)
    send({ kind: "turn-start", turnId })
  }

  /** The turn ended: the server is told, and the writer carries every row of this mod's that
   *  no request of that turn read into one prompt of its own (`unreadRowPrompt`). The event
   *  goes first, as it did when the two were one function: the prompt the writer submits is a
   *  row of the conversation after this turn, never inside it. */
  const turnEnded = (turnId: string, reason: ModTurnReason): void => {
    send({ kind: "turn-complete", turnId, stopReason: reason })
    writer.turnEnded(turnId)
  }

  /** One row of the conversation, stamped where this mod wrote it. */
  const row = (arrived: {
    readonly uuid: string
    readonly door: ModAppendDoor
    readonly origin: ModAppendOrigin
    readonly message: ModAppendMessage
  }): void => {
    const wrote = arrived.door === "prompt" ? writer.claimRow(arrived.message.content) : undefined
    send({ kind: "append", ...arrived, ...(wrote === undefined ? {} : { wrote }) })
  }

  return {
    begin,
    send,
    turnStarted,
    turnEnded,
    stepRequested: writer.stepRequested,
    row,
    followSessionId,
    effortNow: (): ModEffort | null => effort,
  }
}

/** The link's own accumulation of one `turn.step`: the parts as they stream, and which part each
 *  content block of the response is building. */
interface StepParts {
  readonly parts: ModStepPart[]
  readonly blocks: Map<number, number>
}

/** Add one text or thinking piece to the part its block is building, opening one if it is new. */
function appendText(
  step: StepParts,
  index: number,
  kind: "text" | "reasoning",
  text: string,
): void {
  const at = step.blocks.get(index)
  const existing = at === undefined ? undefined : step.parts[at]
  const merged = existing !== undefined && existing.type === kind ? existing.text + text : text
  const part: ModStepPart =
    kind === "text" ? { type: "text", text: merged } : { type: "reasoning", text: merged }
  if (at !== undefined && existing !== undefined && existing.type === kind) step.parts[at] = part
  else {
    step.blocks.set(index, step.parts.length)
    step.parts.push(part)
  }
}

/** Fold one streamed chunk into the step's parts. The engine's own items and the stop carry
 *  nothing the live view shows, so they only pass through. */
function accumulate(step: StepParts, chunk: TurnStepChunk): void {
  if (chunk.kind === "text") appendText(step, chunk.index, "text", chunk.text)
  else if (chunk.kind === "thinking") appendText(step, chunk.index, "reasoning", chunk.text)
  else if (chunk.kind === "tool") {
    step.blocks.set(chunk.index, step.parts.length)
    step.parts.push({ type: "tool-call", id: chunk.id, name: chunk.name, args: "" })
  } else if (chunk.kind === "input") {
    const at = step.blocks.get(chunk.index)
    const existing = at === undefined ? undefined : step.parts[at]
    if (at !== undefined && existing !== undefined && existing.type === "tool-call") {
      step.parts[at] = { ...existing, args: existing.args + chunk.json }
    }
  }
}

/** This session's link, one per loaded module, so the hook the `session.append` registrations
 *  share can be declared here: the host resolves a named hook only against this file's own
 *  top-level functions. */
const link = createLink()

/**
 * Whether an event is a subagent's rather than this session's own main loop, which every hook
 * whose event can carry that key reads before it does anything else.
 *
 * A subagent runs a loop of its own: its own turn, its own requests, its own rows. None of it
 * is the session's turn - a foreground subagent's whole turn opens and closes *inside* the main
 * turn, between two of its steps, and a background subagent's turn can still be open after the
 * main turn completed - so counted as the main turn's it moves where the next message goes,
 * what counts as read, and what the live view draws, for a loop the person is not in.
 *
 * Claude Code says which loop an event is in by one key: measured on 2.1.310, a subagent's
 * `turn.start`, `turn.step`, `turn.complete`, `tool.call`, `tool.check`, `prompt.attachment`,
 * `session.receive` and `session.append` (on every door seen) carry `agentId` - `a` and 16 hex
 * digits - while the main loop's carry no such key at all, in the same run and the same log
 * (`/legion/scratch/tmp/infra-1026-turn`). So the rule is the key and not a turn this mod
 * watched start: when the plugin itself spawns a subagent, that loop's `turn.start` and
 * `turn.step` never reach the spawning plugin and only its `turn.complete` does, which a rule
 * keyed off "I saw this turn start" would take for the main turn's end.
 */
function ofSubagent(e: { readonly agentId?: string }): boolean {
  return e.agentId !== undefined
}

/** One row of the conversation, by whichever door it came in. The door is read off the row
 *  rather than the registration, so the eight registrations share this one hook. A subagent's
 *  row is its loop's and reaches neither the live view nor this mod's watch of the turn. */
const appended: SessionAppendHook = async (_, e, next) => {
  if (!ofSubagent(e) && isWatchedDoor(e.door)) {
    link.row({ uuid: e.uuid, door: e.door, origin: e.origin, message: e.message })
  }
  return await next(e)
}

/** The hooks this mod registers, and nothing else. In particular no `classic.*` hook: on a
 *  machine with managed settings Claude Code's built-in guard writes a notice into what the model
 *  reads for every hook it skips, and this change adds nothing to what the agent reads. For the
 *  same reason `session.append` is registered for the doors of the conversation alone. */
export function register(on: On): void {
  on("session.start", async ($, e, next) => {
    const result = await next(e)
    // The one place `$` is spelled: every call the link makes is a closure of its own, built
    // here, so `$` itself never leaves this hook.
    await link.begin(
      {
        fetch: (url, init) => $.http.fetch(url, init),
        sleep: (ms) => $.clock.sleep(ms),
        sessionId: () => $.session.id(),
        version: () => $.session.version(),
        model: () => $.session.model(),
        runtimeDirectory: () => $.env.get("XDG_RUNTIME_DIR"),
        configDirectory: () => $.env.get("CLAUDE_CONFIG_DIR"),
        homeDirectory: () => $.env.get("HOME"),
        append: (args) => $.session.append(args),
        submit: (args) => $.prompt.submit(args),
        runCommand: (args) => $.command.run(args),
        listCommands: () => $.command.list(),
        fork: (request) => $.model.fork(request),
        setConfig: (args) => $.config.set(args),
        listConfig: () => $.config.list(),
        storeGet: (key) => $.store.get(key),
        storeSet: (key, value) => $.store.set(key, value),
        storeDelete: (key) => $.store.delete(key),
        storeKeys: () => $.store.keys(),
      },
      e.isInteractive,
    )
    return result
  })

  on("session.end", async (_, e, next) => {
    link.send({ kind: "session-end", reason: e.reason })
    const result = await next(e)
    void link.followSessionId().catch(() => undefined)
    return result
  })

  on("turn.start", async (_, e, next) => {
    if (ofSubagent(e)) return await next(e)
    link.turnStarted(e.turnId)
    return await next(e)
  })

  on("turn.complete", async (_, e, next) => {
    if (ofSubagent(e)) return await next(e)
    link.turnEnded(e.turnId, e.reason)
    return await next(e)
  })

  on("turn.step", async function* (_, e, next) {
    // The effort the request really carries. Claude Code names the effort of the model the
    // request is for, and names none for a model that takes none: measured on 2.1.310, a request
    // of `claude-opus-5-5` before any pick carried `output_config.effort: medium`, while with a
    // pick of `low` in force a request of `claude-haiku-4-5` carried `thinking=None` and dropped
    // the effort set here (the stack's model-request log, 2026-11-05). So the server's pick rides
    // on an effort Claude Code named and never fills in an absent one, and the step reports what
    // went out rather than what was set: a step that echoed a pick its own request did not carry
    // held the live view's header on a level nothing was thinking at (INFRA-1026).
    const set = link.effortNow()
    const request: TurnStepInput =
      set === null || e.effort === undefined ? e : { ...e, effort: set }
    // The pick is the session's, so it is on this request whichever loop made it: a subagent's
    // step carries the session's own effort (measured on 2.1.310), and a pick left off it ran
    // every Task subagent at the level Claude Code had saved while the header read the pick
    // (INFRA-1026 review, round five). What is skipped for a subagent is this session's
    // bookkeeping, which its loop is none of: its request raises no count here - a row stored
    // after the main turn's last request looked read by a subagent's step and was never quoted -
    // and the live view publishes nothing of it, which is where the spec puts a subagent's work
    // ("their work shows as the parent's tool call").
    if (ofSubagent(e)) return yield* next(request)
    const step: StepParts = { parts: [], blocks: new Map() }
    const snapshot = (done: boolean, stopReason?: ModStopReason): void => {
      link.send({
        kind: "step",
        turnId: request.turnId,
        index: request.index,
        model: request.model,
        ...(request.effort === undefined ? {} : { effort: request.effort }),
        parts: [...step.parts],
        done,
        ...(stopReason === undefined ? {} : { stopReason }),
      })
    }
    // Before the request: the rows stored by now are the ones it carries, and a row stored
    // after this moment is in no request of this step. Counted here rather than reported,
    // because the one reader of that count is this mod (`SessionLink.stepRequested`).
    link.stepRequested()
    const stream = next(request)
    let snapshotAt = 0
    for await (const chunk of stream) {
      accumulate(step, chunk)
      const now = Date.now()
      if (now - snapshotAt >= STEP_SNAPSHOT_MS) {
        snapshotAt = now
        snapshot(false)
      }
      yield chunk
    }
    let result: TurnStepResult | undefined
    try {
      result = await stream.result
    } catch {
      result = undefined
    }
    if (result === undefined) {
      // The step failed beneath: say it is over and leave the result to the chain.
      snapshot(true)
      return
    }
    snapshot(true, result.stopReason)
    return result
  })

  // One registration per door, each door spelled where it is registered: the host reads the
  // matchers off this source (`claude plugin validate` prints them), and a door it cannot read
  // is a door it cannot attest this mod stays away from.
  on("session.append", { door: "prompt" }, appended)
  on("session.append", { door: "command" }, appended)
  on("session.append", { door: "response" }, appended)
  on("session.append", { door: "tool-result" }, appended)
  on("session.append", { door: "tool-message" }, appended)
  on("session.append", { door: "delivery" }, appended)
  on("session.append", { door: "note" }, appended)
  on("session.append", { door: "notice" }, appended)
}
