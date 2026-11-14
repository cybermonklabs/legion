import { messageFor } from "@legion/envoy-client/errors"
import { type CodexInboxItem, putInbox, takeInbox, takenBy } from "./codex-inbox"

/**
 * Puts a line into a Codex session's conversation (INFRA-3558).
 *
 * Codex has no call that writes into a running conversation, so a line takes one of two ways in:
 *
 * - While the session is idle, `codex queue`, which the Codex terminal takes as a new turn a few
 *   seconds later.
 * - While a turn runs, the plugin's inbox (`./codex-inbox`): the `PostToolUse` hook hands it to
 *   the model at the turn's next tool call, and when the turn ends with it still waiting, this
 *   hands it to `codex queue` for the next turn. A Claude Code session reads a line that arrives
 *   mid-turn at the same point, its next request, or after the turn.
 *
 * Whether a turn runs is what the session's transcript last said (`turnStarted`, `turnEnded`,
 * from `./codex-transcript`). A write resolves once the line is in the session - Codex stored
 * it in its queue, or the hook took it into the turn - and rejects when `codex queue` failed, so
 * the Envoy frame it carries is not answered and the listener sends it again.
 */
export interface CodexDeliveryOptions {
  /** The inbox directory of the session now bound (`codexInboxDirectory`). */
  readonly inboxDirectory: () => string
  /** `codex queue` for the session now bound. */
  readonly queue: (text: string) => Promise<void>
  /** Told of each line the hook took into a running turn, so the live view can show it: the
   *  transcript records a hook's text as an instruction, not as a message of the conversation. */
  readonly taken?: (item: CodexInboxItem) => void
  readonly warn: (text: string) => void
  /** How often a waiting line is looked at to see whether the hook took it. */
  readonly pollMs?: number
}

export interface CodexDelivery {
  /** Puts `item` into the session; see above. */
  deliver(item: CodexInboxItem): Promise<void>
  /** The transcript says a turn started. */
  turnStarted(): void
  /** The transcript says the turn ended: anything still waiting goes to `codex queue`. */
  turnEnded(): Promise<void>
  /** Whether a turn runs, as the transcript last said. */
  turnRunning(): boolean
  /** Stops watching waiting lines; a write still waiting rejects. */
  stop(): void
}

const WAITING_POLL_MS = 250

interface Waiter {
  readonly directory: string
  readonly item: CodexInboxItem
  readonly resolve: () => void
  readonly reject: (error: Error) => void
}

export function createCodexDelivery(options: CodexDeliveryOptions): CodexDelivery {
  const pollMs = options.pollMs ?? WAITING_POLL_MS
  let running = false
  let stopped = false
  /** Every line in the inbox whose write has not resolved, by file name. */
  const waiting = new Map<string, Waiter>()
  /** One sweep at a time, so a line is never queued twice by two sweeps. */
  let sweeping: Promise<void> = Promise.resolve()

  const sweep = (): Promise<void> => {
    sweeping = sweeping.then(async () => {
      const directory = options.inboxDirectory()
      for (const { name, item } of await takeInbox(directory, "server")) {
        const waiter = waiting.get(name)
        waiting.delete(name)
        try {
          await options.queue(item.content)
          waiter?.resolve()
        } catch (error) {
          const failure = error instanceof Error ? error : new Error(messageFor(error))
          if (waiter === undefined)
            options.warn(`a waiting message could not be queued: ${failure.message}`)
          else waiter.reject(failure)
        }
      }
    })
    return sweeping
  }

  /** Resolves the writes whose lines the hook took. */
  const check = async (): Promise<void> => {
    for (const [name, waiter] of [...waiting]) {
      if ((await takenBy(waiter.directory, name)) !== "hook") continue
      if (!waiting.delete(name)) continue
      options.taken?.(waiter.item)
      waiter.resolve()
    }
  }
  const timer = setInterval(() => {
    if (waiting.size > 0) void check()
  }, pollMs)
  timer.unref?.()

  return {
    async deliver(item) {
      if (stopped) throw new Error("the Codex session's delivery has stopped")
      if (!running) {
        await options.queue(item.content)
        return
      }
      const directory = options.inboxDirectory()
      const name = await putInbox(directory, item)
      const written = new Promise<void>((resolve, reject) => {
        waiting.set(name, { directory, item, reject, resolve })
      })
      // The turn may have ended while the line was being written, after its sweep ran.
      if (!running) void sweep()
      await written
    },
    turnStarted() {
      running = true
    },
    async turnEnded() {
      running = false
      await check()
      await sweep()
    },
    turnRunning: () => running,
    stop() {
      stopped = true
      clearInterval(timer)
      for (const waiter of waiting.values()) {
        waiter.reject(
          new Error("the Codex session's delivery stopped before the message was taken"),
        )
      }
      waiting.clear()
    },
  }
}
