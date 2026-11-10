/**
 * What a message Envoy hands this session becomes, and what points the model at it.
 *
 * The mod's other half is transport: a socket, a queue of events and a poll loop (`./mod.ts`,
 * `createLink`). This half is the rule those two carry between them, and it lives here because
 * it is the rule three review rounds have been about (INFRA-1026 review, rounds nine to
 * eleven): a message is a row of the turn running or the prompt of a new turn, a row no request
 * read is quoted by one prompt when the turn ends, and a write is made once however many times
 * Envoy sends the event. Deciding it needs the turn's events and this plugin's writes in one
 * order, which is the mod and nowhere else.
 *
 * Every write is handed the session it writes into, by the request that asked for it, and this
 * half holds everything else itself. The four closures the transport half passes in are the
 * rest: the host for the one write no request asks for (`pointAt`), the event queue, the clock
 * and the session id.
 */
import type {
  PromptSubmitArgs,
  PromptSubmitResult,
  SessionAppendArgs,
  SessionAppendResult,
} from "claude-code"
import {
  MOD_PLUGIN_NAME,
  type ModCallArguments,
  type ModCallResults,
  type ModContentBlock,
  type ModEvent,
  type ModUnreadMessage,
  unreadRowPrompt,
} from "./mod-protocol"

/**
 * How long the writer watches for the row Claude Code writes for a prompt it submitted, to
 * stamp that row for the server (`claimRow`), counted from the answer of the `$.prompt.submit`
 * that carried it.
 *
 * The row is there before the call answers: in all six `$.prompt.submit` calls recorded on
 * 2.1.310 the prompt's own `prompt`-door row landed 15 ms to 26 ms before the call resolved
 * (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:144-145`,
 * `logs/mod3-q2-310.jsonl:51-52,86-87`, `logs/mod3-q1q3q5.jsonl:236-239`). So this is a bound
 * on a watch that is normally over before it starts, at about eighty times the largest gap.
 * Counted from the call instead it would bound something else, since that call waits for a
 * turn already running and such waits ran to 12.4 s (`submitPrompt`).
 */
const PROMPT_ROW_MS = 2_000

/**
 * How long a prompt the engine took has to start its turn before the writer stops holding the
 * rows handed over meanwhile for it.
 *
 * `turn.start` followed the submit's resolution by 8 ms to 11 ms in the same six submits, so a
 * prompt that has started no turn by now starts none: its rows are pointed at rather than left
 * waiting for a `turn.complete` that never comes (INFRA-1026 review, round three, repro
 * `R-C`).
 */
const TURN_START_MS = 2_000

/**
 * The most Envoy events the writer remembers the row of, in `$.store`.
 *
 * Each record is a key of some seventy bytes and a small object, so the whole of it is about
 * 30 KiB, against the 4 MiB of compact JSON `set` refuses past. The store is one file for every
 * session under one `CLAUDE_CONFIG_DIR`, so this bound is shared by all of them, and the
 * eviction pass (`remember`) drops the oldest record whichever session wrote it. The bound is
 * what keeps that store off the wall and off the cost of a big store: measured on 2.1.310, a
 * 3.9 MiB store cost `get` 7–20 ms, `set` 44 ms and `keys()` up to 16 ms, against about 1 ms
 * each on an empty one (`/legion/scratch/tmp/infra-1026-store/README.md`).
 */
const CARD_RECORD_MAX = 256

/** What every key of that record starts with, so the eviction pass reads this plugin's own keys
 *  and leaves anything else it may store alone. */
const CARD_KEY_PREFIX = "message:"

/**
 * What the writer calls on the session: the two writes a message can be, and this plugin's own
 * store. The transport half holds the rest of `$` and builds each of these as a closure of its
 * own, since `$` may not leave the hook it was given to (`./mod.ts`, `ModHost`).
 */
export interface WriterHost {
  readonly append: (args: SessionAppendArgs) => Promise<SessionAppendResult>
  readonly submit: (args: PromptSubmitArgs) => Promise<PromptSubmitResult>
  /** `$.store.get(key)`: this plugin's own key-value store, kept between sessions. */
  readonly storeGet: (key: string) => Promise<unknown>
  readonly storeSet: (key: string, value: unknown) => Promise<void>
  readonly storeDelete: (key: string) => Promise<void>
  /** `$.store.keys()`, in insertion order (measured on 2.1.310). */
  readonly storeKeys: () => Promise<string[]>
}

/** One row this mod wrote into a turn, watched until a request reads it or the turn ends. */
interface WatchedRow {
  /** The turn the row was stored into, and undefined for a row stored while the prompt that
   *  starts that turn was still in flight: the turn adopts it when it reports itself. */
  turnId?: string
  /**
   * The requests this session had made when the row was stored: one made after it is the
   * request that read it.
   *
   * Counted once the append has resolved rather than before it: a request that went out while
   * the row was being stored cannot be proven to carry it, and a row quoted though the model
   * had read it costs a repetition, where a row counted as read that never was costs the
   * message (INFRA-1026 review, repro `R-B`).
   */
  readonly requests: number
  readonly message: ModUnreadMessage
}

/** One prompt this mod submitted whose row has not arrived: the text the row carries, and what
 *  that row is to the server, which the row's own hook stamps it with (`claimRow`). */
interface AwaitedRow {
  readonly text: string
  readonly wrote: "message" | "pointer"
}

/** What this mod recorded one Envoy event's write as, in `$.store`: the write it answered the
 *  server with, minus the refusals, which are no write and are recorded as none. Values
 *  round-trip as `JSON.parse(JSON.stringify(value))` (measured on 2.1.310), so this is plain
 *  JSON data. A prompt is recorded with no row of its own, as it is answered with none
 *  (`./mod-protocol`, `ModCallResults["session.message"]`): the write happened, so the resend
 *  answers from the record rather than writing the message a second time. */
type RecordedWrite = Exclude<ModCallResults["session.message"], { readonly deny: string }>

/** `value` as this mod recorded a write, or undefined where it is anything else: the store is
 *  this plugin's own, and a value it did not write is no record of one. */
function recordedWrite(value: unknown): RecordedWrite | undefined {
  if (typeof value !== "object" || value === null) return undefined
  if (!("as" in value)) return undefined
  const { as } = value
  if (as === "prompt") return { as }
  if (as !== "row") return undefined
  const uuid = "uuid" in value ? value.uuid : undefined
  return typeof uuid === "string" ? { uuid, as } : undefined
}

/** What the transport half gives the writer: the session, the event queue, the clock and the id
 *  the store's keys are scoped by. */
export interface MessageWriterOptions {
  /**
   * The session now, or undefined before `session.start` and after the link gave up.
   *
   * Read by `pointAt` alone: the prompt at a turn's unread rows is work of the writer's own,
   * at a turn's end with no request in hand. Every other write is made for one request, which
   * carries the session the server's call reached (`./mod.ts`, `invoke`).
   */
  readonly engine: () => WriterHost | undefined
  /** Queue one event for the server; never throws (`./mod.ts`, `send`). */
  readonly send: (event: ModEvent) => void
  /** Wait, through the host's own clock, and never throw (`./mod.ts`, `sleep`). */
  readonly sleep: (ms: number) => Promise<void>
  /** The id the session answers to now, which a `/clear` moves. */
  readonly sessionId: () => string | undefined
}

/** The writes and the turn's own events, as the hooks drive them through the link. */
export interface MessageWriter {
  /** Write one message into the session as the session stands now: a row of the turn running,
   *  or the prompt of a new turn. */
  readonly writeMessage: (
    engine: WriterHost,
    args: ModCallArguments["session.message"],
  ) => Promise<ModCallResults["session.message"]>
  /** Write the message of `eventId` once, however many times Envoy sends it. */
  readonly writeOnce: (
    engine: WriterHost,
    eventId: string,
    args: ModCallArguments["session.message"],
  ) => Promise<ModCallResults["session.message"]>
  /** A turn started: it reads every row stored for it, including the ones stored while the
   *  prompt that started it was still in flight. */
  readonly turnStarted: (turnId: string) => void
  /** A turn ended: every row of this mod's that no request of it read is carried by one prompt
   *  of its own (`unreadRowPrompt`). */
  readonly turnEnded: (turnId: string) => void
  /** A request of the turn is going out: a row stored before this moment is in this request,
   *  and one stored after it is in none. */
  readonly stepRequested: () => void
  /** One row of the `prompt` door, claimed for the prompt of this mod's it is the row of, and
   *  stamped with what that prompt wrote; undefined for a person's own prompt. */
  readonly claimRow: (content: readonly ModContentBlock[]) => "message" | "pointer" | undefined
}

/** The writer for one session: all of it closure state, as the link it belongs to is. */
export function createMessageWriter(options: MessageWriterOptions): MessageWriter {
  const { send, sleep } = options
  /** The turn running now, as `turn.start` and `turn.complete` report it. Read in the same tick
   *  as the write that follows it, which is what makes "a row of this turn, or the prompt of a
   *  new one" a decision and not a guess (INFRA-1026 review, round eleven). */
  let turn: string | undefined
  /**
   * True from the moment this mod hands the engine a prompt until the turn that prompt starts
   * has reported itself, or until the mod gives up on one (`TURN_START_MS`).
   *
   * A message handed over meanwhile is a row of that turn rather than a second prompt: the
   * engine takes one prompt at a time, so a second would sit behind this turn, and the row the
   * turn is about to read says the same thing sooner.
   */
  let prompting = false
  /** Bumped whenever a turn starts or this mod stops waiting for one, so the guard armed for
   *  one prompt never fires against a later one. */
  let promptEpoch = 0
  /** How many requests this session has made, counted at the top of every `turn.step`. */
  let requests = 0
  /** The rows this mod wrote that no request has read, by the uuid Claude Code stored each
   *  under. A row leaves it when its turn ends, which is also when the prompt pointing at it
   *  goes out, so this holds the rows of the turn running and nothing else. */
  const watched = new Map<string, WatchedRow>()
  /** The prompts this mod submitted whose row has not arrived, oldest first. An entry leaves
   *  it when the row arrives, and `PROMPT_ROW_MS` after the engine took the prompt where no
   *  row does; a submit still in flight holds its entry, since the row comes just before that
   *  call answers (`submitPrompt`). So nothing else bounds this. */
  const awaitedRows: AwaitedRow[] = []
  /** The write running for each Envoy event id, so a copy of that event which arrives while the
   *  first is still running waits for it and then finds what it recorded (`writeOnce`). */
  const writing = new Map<string, Promise<void>>()

  /** The key `$.store` holds one Envoy event's write under: this session's id with the event
   *  id. A `/clear` moves the id, and the conversation it moves to is one the card is not in,
   *  so its key is a key of its own. */
  const recordKey = (eventId: string): string =>
    `${CARD_KEY_PREFIX}${options.sessionId() ?? ""}:${eventId}`

  /** Record what `eventId`'s message was written as, and drop the oldest records past
   *  `CARD_RECORD_MAX`. `keys()` answers in insertion order, so the oldest are the first of
   *  them (the issue's store spike). A store that refuses the write leaves no record and the
   *  message stands: a later copy of that event would then be written a second time, which is
   *  the one case this rule misses and the one the crash case already covers
   *  (`docs/site/src/content/docs/dispatch/envoy.md`). */
  const remember = async (
    engine: WriterHost,
    eventId: string,
    write: RecordedWrite,
  ): Promise<void> => {
    try {
      await engine.storeSet(recordKey(eventId), write)
      const keys = (await engine.storeKeys()).filter((key) => key.startsWith(CARD_KEY_PREFIX))
      for (const old of keys.slice(0, keys.length - CARD_RECORD_MAX)) {
        await engine.storeDelete(old)
      }
    } catch {
      // The record is a saving, not the write itself; a store that refuses it costs nothing
      // here and there is nowhere in a session to report it to.
    }
  }

  /** Register the row Claude Code writes for the prompt carrying `text`, so that row is
   *  stamped `wrote` for the server (`claimRow`). Registered before the submit goes out,
   *  because the row reaches this mod's own `session.append` hook before the call resolves.
   *  Giving the entry up is the caller's, and `submitPrompt` times that from the call's
   *  answer: the gap `PROMPT_ROW_MS` bounds is the one between the row and that answer, and a
   *  submit itself can wait seconds for a turn already running. Nothing waits on the row: the
   *  write is answered with the prompt alone (`./mod-protocol`,
   *  `ModCallResults["session.message"]`). */
  const awaitRow = (text: string, wrote: "message" | "pointer"): { readonly drop: () => void } => {
    const waiting: AwaitedRow = { text, wrote }
    awaitedRows.push(waiting)
    const drop = (): void => {
      const at = awaitedRows.indexOf(waiting)
      if (at !== -1) awaitedRows.splice(at, 1)
    }
    return { drop }
  }

  /** One row of the `prompt` door, claimed for the prompt of this mod's it is the row of, if
   *  any: the oldest whose text this row carries, since Claude Code writes its own framing in
   *  front of a submit that named no `asUser`. Every other prompt row is a person's own. */
  const claimRow = (content: readonly ModContentBlock[]): AwaitedRow["wrote"] | undefined => {
    const text = content
      .map((block) => (typeof block["text"] === "string" ? block["text"] : ""))
      .join("\n")
    const at = awaitedRows.findIndex((waiting) => text.includes(waiting.text))
    const waiting = awaitedRows[at]
    if (waiting === undefined) return undefined
    awaitedRows.splice(at, 1)
    return waiting.wrote
  }

  /**
   * Take out of the watch every row `leaves` names, and point the model at the messages of
   * those no request read, with the one prompt that quotes them all (`unreadRowPrompt`).
   *
   * The three moments a row leaves the watch pass their own `leaves` and share everything
   * else: the turn that ends, the prompt that started no turn, and the one row the session
   * fell idle under while it was being stored. A row that leaves having been stored since
   * this session's last request is a row nothing read — in the last two moments every row
   * that leaves is such a row, since no request can have gone out meanwhile: a step belongs
   * to a turn, and in both there is no turn.
   */
  const pointAtUnread = (leaves: (uuid: string, row: WatchedRow) => boolean): void => {
    const unread: ModUnreadMessage[] = []
    for (const [uuid, row] of watched) {
      if (!leaves(uuid, row)) continue
      watched.delete(uuid)
      if (row.requests === requests) unread.push(row.message)
    }
    if (unread.length > 0) pointAt(unread)
  }

  /** Stop waiting for the turn a prompt was to start: it started none, so every row handed over
   *  while it was in flight is read by nothing and the model is pointed at it. A submit the
   *  engine dropped, one it refused, and one whose turn never came all end here (INFRA-1026
   *  review, repro `R-C`). */
  const stopPrompting = (): void => {
    prompting = false
    promptEpoch += 1
    pointAtUnread((_, row) => row.turnId === undefined)
  }

  /** Give the prompt the engine has just taken `TURN_START_MS` to start its turn. */
  const guardTurnStart = (): void => {
    const epoch = promptEpoch
    void (async () => {
      await sleep(TURN_START_MS)
      if (promptEpoch === epoch && prompting) stopPrompting()
    })()
  }

  /**
   * Hand the engine one prompt and answer with the reason it dropped it, where it did.
   *
   * `$.prompt.submit` resolves once the engine has taken the prompt, not at the end of the turn
   * it starts: in all six calls recorded on 2.1.310 the prompt's own row landed 15 ms to 26 ms
   * before the call resolved and `turn.start` 8 ms to 11 ms after it, and the long resolutions
   * — 8.5 s, 11.2 s and 12.4 s — are the wait for a turn that was already running, which ended
   * just before (`/legion/scratch/tmp/infra-1026-spike/logs/mod2.jsonl:136-147`,
   * `logs/mod3-q2-310.jsonl:72-89`, `logs/mod3-q1q3q5.jsonl:163-179`). So what the caller
   * waits for is the prompt entering, never a turn.
   *
   * The row that prompt becomes is the prompt's own and is answered to nobody, so nothing here
   * waits for it: it is watched only to be stamped (`awaitRow`, `./mod-protocol`,
   * `ModCallResults["session.message"]`).
   */
  const submitPrompt = async (
    engine: WriterHost,
    text: string,
    asUser: boolean,
    wrote: "message" | "pointer",
  ): Promise<{ readonly drop?: string }> => {
    // Set before the call and in the same tick as the check that chose this branch: a message
    // handed over from here on is a row of the turn this prompt starts.
    prompting = true
    const row = awaitRow(text, wrote)
    let answer: PromptSubmitResult
    try {
      answer = await engine.submit({ text, ...(asUser ? { asUser: true as const } : {}) })
    } catch (error) {
      row.drop()
      stopPrompting()
      throw error
    }
    if (answer.drop !== undefined) {
      row.drop()
      stopPrompting()
      return { drop: answer.drop }
    }
    // The engine has the prompt, so its row is due: it landed 15 ms to 26 ms before this
    // answer in all six submits recorded on 2.1.310, and the wait for the answer itself was
    // 8.5 s, 11.2 s and 12.4 s where a turn was already running. Timed from the call instead,
    // the window closed on a row still coming and the pointer prompt's row went unstamped,
    // which drew every message that prompt quotes a second time (INFRA-1026 review, round
    // six). Both paths above drop the entry themselves, so nothing is left behind for a
    // prompt that never entered.
    void sleep(PROMPT_ROW_MS).then(row.drop)
    guardTurnStart()
    return {}
  }

  /** Submit the one prompt that points the model at `unread`, quoting each message in full
   *  (`unreadRowPrompt`). Its own row is stamped `pointer`, so the live view draws nothing of
   *  it: it quotes rows that view has already drawn. */
  const pointAt = (unread: readonly ModUnreadMessage[]): void => {
    const engine = options.engine()
    if (engine === undefined) return
    void submitPrompt(engine, unreadRowPrompt(unread), false, "pointer").catch(() => undefined)
  }

  /** Write one message into the session as the session stands now: a row of the turn running,
   *  or the prompt of a new turn. */
  const writeMessage = async (
    engine: WriterHost,
    args: ModCallArguments["session.message"],
  ): Promise<ModCallResults["session.message"]> => {
    if (turn === undefined && !prompting) {
      // Nothing is running: an appended row would be read at the turn's next request, and an
      // idle session makes none, so the message is the prompt itself and nothing else is
      // stored for it — a row beside a prompt quoting it put the one message in front of the
      // model twice (INFRA-1026, handoff v6).
      const sent = await submitPrompt(
        engine,
        args.message.text,
        args.message.kind === "person",
        "message",
      )
      // A prompt the engine took is the write, with no row of its own; only one it dropped is
      // a refusal (`./mod-protocol`, `ModCallResults["session.message"]`).
      return sent.drop === undefined ? { as: "prompt" } : { deny: sent.drop, as: "prompt" }
    }
    const answer = await engine.append({ message: { type: "user", content: args.content } })
    if (answer.deny !== undefined) return { deny: answer.deny, as: "row" }
    // Claude Code does not re-enter this mod's own `session.append` hook for a row this mod
    // appended, so the row reaches the server's mapper nowhere else and a card delivered into
    // a running turn was drawn nowhere at all (INFRA-1026, handoff v6). Measured in the issue's
    // spike: two `$.session.append` calls resolved
    // `{message: {type: "user", role: "user", isMeta: true, content}, uuid}` and neither uuid
    // ever arrived at a `session.append` hook
    // (`/legion/scratch/tmp/infra-1026-spike/logs/mod3-q1q3q5.jsonl:36,64`). So the mod reports
    // the row itself, out of what the call answered (`SessionAppendResult`,
    // `./claude-mod.d.ts`).
    //
    // The door and the origin are the engine's to set and the result carries neither: a
    // plugin's own append comes in by `note`, under the calling plugin. `wrote` is what the
    // server reads it by, and it is on the event rather than on this call's answer because the
    // two cross (`./mod-protocol`, `ModEvent`).
    send({
      kind: "append",
      uuid: answer.uuid,
      door: "note",
      origin: { kind: "plugin", name: MOD_PLUGIN_NAME },
      message: answer.message,
      wrote: "message",
    })
    watched.set(answer.uuid, {
      message: args.message,
      requests,
      ...(turn === undefined ? {} : { turnId: turn }),
    })
    // Nothing is running any more. Reaching this branch meant something was - a turn, or a
    // prompt of this mod's waiting for one, read in the same tick as the check above - and it
    // ended while the row was being stored, with nothing starting in its place. So no request
    // will read the row, and the model is pointed at it here rather than at a `turn.complete`
    // or a `stopPrompting` that has been and gone: both ran while this row was not in the watch
    // yet. Read off the turn alone, this moment missed the dropped prompt, whose row then sat
    // in the watch with its Envoy frame answered (INFRA-1026 review, round five).
    if (turn === undefined && !prompting) {
      pointAtUnread((uuid) => uuid === answer.uuid)
    }
    return { uuid: answer.uuid, as: "row" }
  }

  /**
   * Write the message of `eventId` once, however many times Envoy sends it.
   *
   * The store has no compare-and-set: two `get`s of an unset key both answered undefined, 0 ms
   * and 2 s apart, in one session and across two (`/legion/scratch/tmp/infra-1026-store`). So a
   * copy that arrives while the first write is still running waits for it here, inside the one
   * process, and reads the record that write left; a copy that arrives after this process is
   * gone — the engine restarted, `claude --resume <id>` — finds that record in the store, which
   * `set` had written to disk before it resolved and which survived a `kill -9` taken at that
   * moment (same spike).
   */
  const writeOnce = (
    engine: WriterHost,
    eventId: string,
    args: ModCallArguments["session.message"],
  ): Promise<ModCallResults["session.message"]> => {
    const settle = Promise.withResolvers<ModCallResults["session.message"]>()
    const behind = writing.get(eventId)
    const run = (behind === undefined ? Promise.resolve() : behind.catch(() => undefined)).then(
      async () => {
        let written: ModCallResults["session.message"]
        try {
          const recorded = recordedWrite(
            await engine.storeGet(recordKey(eventId)).catch(() => undefined),
          )
          // A store that refuses the read answers nothing, so the message is written again:
          // at-least-once is this path's rule, and a message lost is the failure that matters.
          written = recorded ?? (await writeMessage(engine, args))
          if (recorded === undefined && written.deny === undefined) {
            // The write is the record: a prompt the engine took is recorded with no row of its
            // own, which is what keeps the resend from writing it again.
            await remember(engine, eventId, written)
          }
        } catch (error) {
          settle.reject(error)
          return
        }
        settle.resolve(written)
      },
    )
    writing.set(eventId, run)
    void run.finally(() => {
      if (writing.get(eventId) === run) writing.delete(eventId)
    })
    return settle.promise
  }

  return {
    writeMessage,
    writeOnce,
    turnStarted: (turnId: string): void => {
      turn = turnId
      prompting = false
      promptEpoch += 1
      for (const row of watched.values()) row.turnId ??= turnId
    },
    turnEnded: (turnId: string): void => {
      if (turn === turnId) turn = undefined
      pointAtUnread((_, row) => row.turnId === turnId)
    },
    stepRequested: (): void => {
      requests += 1
    },
    claimRow,
  }
}
