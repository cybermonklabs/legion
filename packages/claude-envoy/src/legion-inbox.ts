import { randomUUID } from "node:crypto"
import { createConnection, type Socket } from "node:net"
import type { DeliveryCapability } from "@legion/contracts"
import type { ChannelNotification, ChannelNotifier } from "./envoy-channel-server"

/**
 * The variable `legion claude-bridge` exports to Claude Code, and so to this server, naming its
 * inbox socket (`packages/daemon/internal/claudebridge/inbox.go`): in a Legion pane each Envoy
 * delivery goes there and the bridge sends it to Claude Code as a user turn, which is what makes
 * a person's own message a turn of the pane's own conversation; and each of the claim's task
 * values goes there too, for the bridge to take (`LegionInbox.task`).
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
 * bucket's epoch; and the key's revision in it.
 */
export interface InboxTaskValue {
  readonly number: number
  readonly epoch: string
  readonly revision: number
}

/** The bridge's inbox: every line this server writes into a Legion pane. */
export interface LegionInbox extends ChannelNotifier {
  /**
   * Hands the bridge the claim's task value as one line `{id, kind: "task", number, epoch,
   * revision}`, and resolves once the bridge's loop has taken it. The bridge, which knows whether
   * Claude Code is in a turn, reports the value seen and takes its task from the daemon
   * (`packages/daemon/internal/claudebridge/take.go`); nothing of it reaches Claude Code as a line
   * of its own.
   */
  task(value: InboxTaskValue): Promise<void>
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
 * other. A task line (`task`) is answered `ok` once the bridge has taken the value. An answer with
 * an error, a closed socket or an answer for another id rejects. The bridge answers one
 * connection's lines in order, so one connection, opened on first use and again after it closes,
 * carries every line, and a task line carries an id of its own so that its answer is paired with
 * it and not with a delivery's.
 */
export function createInboxNotifier(socketPath: string): LegionInbox {
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

  /** Writes one line naming `id`, and resolves once the bridge answers it. */
  const send = (what: string, id: string, line: Record<string, unknown>): Promise<void> => {
    const settle = Promise.withResolvers<void>()
    waiting.push({ id, what, settle })
    connected().write(`${JSON.stringify(line)}\n`)
    return settle.promise
  }

  return {
    notification(notification: ChannelNotification): Promise<void> {
      const id = notification.eventId ?? randomUUID()
      // `blocks` is additional: `content` is the message's text whatever a reader makes of the
      // blocks, so a bridge that does not know the field delivers the message as it always did.
      return send("delivery", id, {
        id,
        content: notification.content,
        ...(notification.blocks === undefined ? {} : { blocks: notification.blocks }),
      })
    },
    task(value: InboxTaskValue): Promise<void> {
      const id = randomUUID()
      return send("task line", id, {
        id,
        kind: "task",
        number: value.number,
        epoch: value.epoch,
        revision: value.revision,
      })
    },
  }
}
