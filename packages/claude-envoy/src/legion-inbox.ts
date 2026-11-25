import { randomUUID } from "node:crypto"
import { createConnection, type Socket } from "node:net"
import type { DeliveryCapability } from "@legion/contracts"
import { z } from "zod"
import type { ChannelNotification, ChannelNotifier } from "./envoy-channel-server"

/**
 * The variable `legion claude-bridge` exports to Claude Code, and so to this server, naming its
 * inbox socket (`packages/daemon/internal/claudebridge/inbox.go`): in a Legion pane each Envoy
 * delivery goes there and the bridge sends it to Claude Code as a user turn, which is what makes
 * a person's own message a turn of the pane's own conversation; and each of the claim's task
 * values, and the word that the claim's ready has answered, go there too, for the bridge to take
 * the task (`LegionInbox.task`, `LegionInbox.ready`).
 */
export const INBOX_VARIABLE = "LEGION_CLAUDE_INBOX"

/**
 * The delivery modes a session whose deliveries go to the bridge's inbox advertises. The bridge
 * (`deliver` in `packages/daemon/internal/claudebridge/bridge.go`) starts a turn with a delivery
 * when Claude Code is idle and otherwise sends it as the next user message of the turn running,
 * once Claude Code ends its current reply: a Send. An Aside gets the same handling, which keeps
 * its promise: the message joins the main conversation without interrupting the work in hand.
 * The inbox line carries no mode, since the bridge treats both alike. `btw` stays off: the bridge
 * runs no side turn.
 */
export const INBOX_CAPABILITIES = [
  "aside",
  "steer",
] as const satisfies readonly DeliveryCapability[]

/** How long a take-pending query (`LegionInbox.takePending`) waits for the bridge's answer. */
export const TAKE_PENDING_TIMEOUT_MS = 2_000

/** The bridge's line back for a take-pending query: `ok` under the query's id, with the answer.
 * An older bridge refuses the kind with an error instead, and its line carries no answer. */
const takePendingAnswer = z.object({
  id: z.string(),
  ok: z.literal(true),
  /**
   * What the bridge answers a take-pending query (`TakePendingAnswer` in
   * `packages/daemon/internal/claudebridge/inbox.go`): whether the claim's take holds a task — a
   * value it will ask about, a take in flight, an accepted take whose turn has not started, or a
   * take refused `in-turn` during the turn running — and the number of the task the claim is
   * owed that the turn running did not take, 0 when no turn runs or none waits.
   */
  takePending: z.object({ pending: z.boolean(), waiting: z.number().int().nonnegative() }),
})

/** What the bridge answers a take-pending query (`takePendingAnswer`'s `takePending`). */
export type TakePending = z.output<typeof takePendingAnswer>["takePending"]

/** `InboxAnswer`: the bridge's line back for one delivery, once Claude Code took it or cannot. */
interface InboxAnswer {
  readonly id: string
  readonly ok?: boolean
  readonly error?: string
}

/**
 * A claim's task value as the bridge's inbox takes it (`InboxDelivery` in
 * `packages/daemon/internal/claudebridge/inbox.go`): the number of the task the claim is owed, or 0
 * where the value was deleted at this revision and the claim has no task from it on; the task
 * bucket's epoch; the key's revision in it; and the number of the last task the daemon's reading
 * the value came with says the claim finished, 0 for a value that came with no reading (a relayed
 * one). The bridge holds the highest finished number any line carried, so a 0 lowers nothing.
 */
export interface InboxTaskValue {
  readonly number: number
  readonly epoch: string
  readonly revision: number
  readonly finished: number
}

/** The bridge's inbox: every line this server writes into a Legion pane. */
export interface LegionInbox extends ChannelNotifier {
  /**
   * Hands the bridge the claim's task value as one line `{id, kind: "task", number, epoch,
   * revision, finished}`, and resolves once the bridge's loop has taken it. The bridge, which
   * knows whether Claude Code is in a turn, holds the newest value and the finished number,
   * reports the value seen and takes its task from the daemon
   * (`packages/daemon/internal/claudebridge/take.go`); nothing of it reaches Claude Code as a line
   * of its own.
   */
  task(value: InboxTaskValue): Promise<void>
  /**
   * Tells the bridge the claim's `claims/ready` has answered, and the read of the claim's value
   * after it too, as one line `{id, kind: "ready"}` written behind that reading's own line, and
   * resolves once the bridge's loop has taken it. The daemon refuses a take before the ready, so
   * the bridge takes no task before this line, and it holds every Envoy delivery until it, so none
   * starts a turn ahead of a task the ready decision wrote.
   */
  ready(): Promise<void>
  /**
   * Hands the bridge a reading of the daemon's that holds no value (`task: null`), as one line
   * `{id, kind: "finished", finished}`: the number of the last task it says the claim finished,
   * which the bridge holds beside the numbers the task lines carried, and resolves once the
   * bridge's loop has taken it. A value the bridge holds stays held, as Oh My Pi's take keeps its
   * value at such a reading.
   */
  finished(finished: number): Promise<void>
  /**
   * Asks the bridge whether the claim's take holds a task, and which task the turn running did
   * not take, as one line `{id, kind: "take-pending"}` on a connection of its own, dialled for
   * this query alone and closed once it is answered: the bridge answers one connection's lines in
   * order, and a delivery's answer waits for Claude Code to take it, so a query written behind a
   * delivery would wait as long as the delivery does. The bridge answers the query at once,
   * whatever Claude Code is doing. Resolves undefined, and never rejects, where the bridge gives
   * no such answer: no bridge at the socket, a connection that closed first, a line that carries
   * no `takePending` for this id (an older bridge refuses the kind with an error), or no line
   * within `TAKE_PENDING_TIMEOUT_MS`.
   */
  takePending(): Promise<TakePending | undefined>
}

/**
 * The bridge's inbox at `socketPath`. Each delivery is one JSON line `{id, content}` (the
 * envelope's `event_id`, else a fresh id; the rendered content), resolved once the bridge answers
 * `{id, ok: true}`. A line whose delivery carries more than its text - a person's own message and
 * the pictures it embeds - also carries `blocks`, Claude Code's own content blocks for it
 * (INFRA-1026); `content` stays the text, so a bridge that does not know the field delivers the
 * message as it always did. The bridge answers `ok` only when Claude Code re-emits that user
 * message (`--replay-user-messages`), that is once it took the delivery into a step, and an error
 * when Claude Code never takes it: a relayed frame is answered `done` on the one and `nak` on the
 * other. A task line (`task`), the ready line (`ready`) and a finished line (`finished`) are
 * answered `ok` once the bridge has taken them. An answer with an error, a closed socket or an
 * answer for another id rejects. The bridge answers one connection's lines in order, so one
 * connection, opened on first use and again after it closes, carries every line, in the order
 * this server writes them, and each line of the claim's carries an id of its own so that each
 * answer is paired with its line and not with a delivery's. A take-pending query alone goes on a
 * connection of its own (`takePending`), bounded at `takePendingTimeoutMs`.
 */
export function createInboxNotifier(
  socketPath: string,
  options: { readonly takePendingTimeoutMs?: number } = {},
): LegionInbox {
  const takePendingTimeoutMs = options.takePendingTimeoutMs ?? TAKE_PENDING_TIMEOUT_MS
  let socket: Socket | undefined
  let buffered = ""
  const waiting: {
    readonly id: string
    readonly what: string
    readonly settle: PromiseWithResolvers<void>
  }[] = []

  const fail = (error: Error): void => {
    for (const pending of waiting.splice(0)) pending.settle.reject(error)
  }

  const answer = (line: string): void => {
    const pending = waiting.shift()
    if (pending === undefined) return
    let parsed: InboxAnswer
    try {
      parsed = JSON.parse(line) as InboxAnswer
    } catch {
      pending.settle.reject(new Error(`the Legion inbox answered ${JSON.stringify(line)}`))
      return
    }
    if (parsed.ok === true && parsed.id === pending.id) pending.settle.resolve()
    else {
      pending.settle.reject(
        new Error(
          `the Legion inbox refused ${pending.what} ${pending.id}: ${parsed.error ?? `answered ${line}`}`,
        ),
      )
    }
  }

  const connected = (): Socket => {
    if (socket !== undefined && !socket.destroyed) return socket
    buffered = ""
    const opened = createConnection(socketPath)
    opened.setEncoding("utf8")
    opened.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = buffered.slice(0, end)
        buffered = buffered.slice(end + 1)
        answer(line)
      }
    })
    opened.on("error", (error) =>
      fail(new Error(`the Legion inbox ${socketPath}: ${error.message}`)),
    )
    opened.on("close", () => {
      fail(new Error(`the Legion inbox ${socketPath} closed before it answered`))
      if (socket === opened) socket = undefined
    })
    socket = opened
    return opened
  }

  /** Writes `line`, which names its `id`, and resolves once the bridge answers that id. */
  const send = (
    what: string,
    line: { readonly id: string } & Record<string, unknown>,
  ): Promise<void> => {
    const settle = Promise.withResolvers<void>()
    waiting.push({ id: line.id, what, settle })
    connected().write(`${JSON.stringify(line)}\n`)
    return settle.promise
  }

  return {
    notification(notification: ChannelNotification): Promise<void> {
      // `blocks` is additional: `content` is the message's text whatever a reader makes of the
      // blocks, so a bridge that does not know the field delivers the message as it always did.
      return send("delivery", {
        id: notification.eventId ?? randomUUID(),
        content: notification.content,
        ...(notification.blocks === undefined ? {} : { blocks: notification.blocks }),
      })
    },
    task(value: InboxTaskValue): Promise<void> {
      return send("task line", {
        id: randomUUID(),
        kind: "task",
        number: value.number,
        epoch: value.epoch,
        revision: value.revision,
        finished: value.finished,
      })
    },
    ready(): Promise<void> {
      return send("ready line", { id: randomUUID(), kind: "ready" })
    },
    finished(finished: number): Promise<void> {
      return send("finished line", { id: randomUUID(), kind: "finished", finished })
    },
    takePending(): Promise<TakePending | undefined> {
      const id = randomUUID()
      const answered = Promise.withResolvers<TakePending | undefined>()
      const query = createConnection(socketPath)
      // The first of the answer, an error, a close and the bound settles the query; the rest
      // find it settled.
      const settle = (answer: TakePending | undefined): void => {
        clearTimeout(bound)
        query.destroy()
        answered.resolve(answer)
      }
      const bound = setTimeout(() => settle(undefined), takePendingTimeoutMs)
      let line = ""
      query.setEncoding("utf8")
      query.on("data", (chunk: string) => {
        line += chunk
        const end = line.indexOf("\n")
        if (end < 0) return
        let parsed: unknown
        try {
          parsed = JSON.parse(line.slice(0, end))
        } catch {
          settle(undefined)
          return
        }
        const answer = takePendingAnswer.safeParse(parsed)
        settle(answer.success && answer.data.id === id ? answer.data.takePending : undefined)
      })
      query.on("error", () => settle(undefined))
      query.on("close", () => settle(undefined))
      query.write(`${JSON.stringify({ id, kind: "take-pending" })}\n`)
      return answered.promise
    },
  }
}
