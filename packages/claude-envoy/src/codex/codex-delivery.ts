import { messageFor } from "@legion/envoy-client/errors"
import {
  type CodexInboxItem,
  inboxItemName,
  putInbox,
  releaseClaims,
  takeInbox,
  takenBy,
  withdrawWaiting,
} from "./codex-inbox"

/**
 * Puts a line into a Codex session's conversation.
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
 *
 * Every write settles, because the session's frames are written one behind another and one that
 * never settled would hold back every later one: a write's waiter is in place before its line can
 * be seen, so whatever takes the line, a sweep or the hook, finds the write to settle; a line the
 * hook did not take is queued at the turn's end, and, while no turn runs, by a sweep every
 * `pollMs`; a sweep looks at the inbox of every session a line still waits in, so a `/new` that
 * moves the session to another id while a line waits for the old one queues it for the new one;
 * and a sweep that fails is logged and tried again rather than ending the ones after it.
 */
export interface CodexDeliveryOptions {
  /** The session the `codex` process runs now, as its record names it. */
  readonly sessionId: () => string
  /** The inbox directory of a session (`codexInboxDirectory`). */
  readonly inboxDirectory: (sessionId: string) => string
  /** `codex queue` for the session the process runs now. */
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
  /** The session ended (`SessionEnd`, which Codex runs when it unloads the thread,
   *  `thread_unload_delay` after it is idle with no subscriber, 60 s by default, or at exit): every
   *  write from now is refused, so its Envoy frame stays unanswered and is sent again, until
   *  `rebind` names a session that has not ended or `sessionResumed` says this one runs again. */
  sessionEnded(): void
  /** The session that had ended runs again (`/resume`): writes are taken again. */
  sessionResumed(): Promise<void>
  /** The process moved to another session (`/new`): no turn of it runs yet, and what waited for
   *  the old one goes to the new one, unless the new one has already ended (`ended`). */
  rebind(ended?: boolean): Promise<void>
  /** Whether a turn runs, as the transcript last said. */
  turnRunning(): boolean
  /** Stops watching waiting lines; a write still waiting rejects. Resolves once every waiting line
   *  is withdrawn or found taken: a line still being written is withdrawn once it is in place, and
   *  a sweep already running first settles the writes of the lines it takes. No sweep starts after
   *  this. */
  stop(): Promise<void>
}

const WAITING_POLL_MS = 250

interface Waiter {
  readonly directory: string
  readonly item: CodexInboxItem
  /** Whether the line's file is in place: false when writing it failed, and the write then rejects
   *  with that failure itself. */
  readonly landed: Promise<boolean>
  readonly resolve: () => void
  readonly reject: (error: Error) => void
}

export function createCodexDelivery(options: CodexDeliveryOptions): CodexDelivery {
  const pollMs = options.pollMs ?? WAITING_POLL_MS
  let running = false
  let stopped = false
  let ended = false
  /** Every line in an inbox whose write has not resolved, by file name. */
  const waiting = new Map<string, Waiter>()
  /** One sweep at a time, so a line is never queued twice by two sweeps. */
  let sweeping: Promise<void> = Promise.resolve()

  const sweepOnce = async (): Promise<void> => {
    const directories = new Set([options.inboxDirectory(options.sessionId())])
    for (const waiter of waiting.values()) directories.add(waiter.directory)
    for (const directory of directories) {
      // What a hook that is gone held waits again first, so this sweep queues it.
      await releaseClaims(directory)
      for (const { name, item } of await takeInbox(directory)) {
        const waiter = waiting.get(name)
        waiting.delete(name)
        try {
          await options.queue(item.content)
          waiter?.resolve()
        } catch (error) {
          const failure = error instanceof Error ? error : new Error(messageFor(error))
          if (waiter === undefined) {
            options.warn(`a waiting message could not be queued: ${failure.message}`)
          } else waiter.reject(failure)
        }
      }
    }
  }
  const sweep = (): Promise<void> => {
    // Once stop() has run, every line left is stop()'s to withdraw or find taken: a sweep that
    // queued one now would leave stop() a line it cannot withdraw and whose queueing it never saw.
    if (stopped) return sweeping
    sweeping = sweeping.then(sweepOnce).catch((error: unknown) => {
      options.warn(`the Codex inbox could not be swept, and will be again: ${messageFor(error)}`)
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
    if (waiting.size === 0) return
    void check()
      .then(() => (running || ended || waiting.size === 0 ? undefined : sweep()))
      .catch((error: unknown) => options.warn(messageFor(error)))
  }, pollMs)
  timer.unref?.()

  return {
    async deliver(item) {
      if (stopped) throw new Error("the Codex session's delivery has stopped")
      if (ended) {
        throw new Error(
          "the Codex session ended; the message is sent again once the session that follows starts",
        )
      }
      if (!running) {
        await options.queue(item.content)
        return
      }
      const directory = options.inboxDirectory(options.sessionId())
      // The waiter is in place before the write of its line starts. A waiter registered only once
      // the write returned missed a sweep or a stop() that came between: the sweep queued the line
      // with no write to settle, or stop() did not see it, and the write never settled.
      const name = inboxItemName()
      const written = Promise.withResolvers<void>()
      // A sweep can settle the write before putInbox returns; the write is awaited below.
      written.promise.catch(() => undefined)
      const landed = Promise.withResolvers<boolean>()
      waiting.set(name, {
        directory,
        item,
        landed: landed.promise,
        resolve: written.resolve,
        reject: written.reject,
      })
      try {
        await putInbox(directory, item, name)
        landed.resolve(true)
      } catch (error) {
        landed.resolve(false)
        waiting.delete(name)
        throw error
      }
      // The turn may have ended while the line was being written, after its sweep ran. A session
      // that ended meanwhile gets the line once it runs again, as the poll waits for that too.
      if (!running && !ended) void sweep()
      await written.promise
    },
    turnStarted() {
      running = true
    },
    async turnEnded() {
      running = false
      await check().catch((error: unknown) => options.warn(messageFor(error)))
      await sweep()
    },
    sessionEnded() {
      ended = true
      running = false
    },
    async sessionResumed() {
      ended = false
      await sweep()
    },
    async rebind(endedNow = false) {
      ended = endedNow
      running = false
      if (!ended) await sweep()
    },
    turnRunning: () => running,
    stop() {
      stopped = true
      clearInterval(timer)
      // A refused write leaves its Envoy frame unanswered, so Envoy sends it again. Each line is
      // withdrawn before its write is refused, so the hook cannot deliver it as well; a line the
      // hook handed Codex was delivered, and its write resolves. A line a hook still holds, writing
      // it to Codex (`claimed/`), is refused too: Envoy's copy is then the one that reaches the
      // session if that write fails, and a second one if it does not, since nothing waits here for
      // the hook to say which. A sweep that already started settles the writes of the lines it
      // takes, so stop() takes what is left only once it ends.
      return sweeping
        .then(() => {
          const left = [...waiting]
          waiting.clear()
          return Promise.all(
            left.map(async ([name, waiter]) => {
              // A write whose line never landed rejects with its own failure.
              if (!(await waiter.landed)) return
              const by = (await withdrawWaiting(waiter.directory, name))
                ? undefined
                : await takenBy(waiter.directory, name)
              if (by === undefined) {
                waiter.reject(
                  new Error("the Codex session's delivery stopped before the message was taken"),
                )
              } else {
                if (by === "hook") options.taken?.(waiter.item)
                waiter.resolve()
              }
            }),
          )
        })
        .then(() => undefined)
    },
  }
}
