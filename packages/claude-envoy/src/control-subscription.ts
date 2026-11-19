// The control subject a viewer takes a session's live view on (`agentStreamControlSubject`), over
// the view's life, for both live views: Claude Code's (`./agent-stream-runner`) and Codex's
// (`./codex/codex-agent-stream`). It takes the subject only once the session's connection is
// admitted as the session (`admitted`), takes it again a moment after its subscription ended,
// moves it when the view moves to another session id or the server swaps in a new connection, and
// takes nothing on a connection closed for good or once the view stopped. What a control message
// is answered with is each view's own (`answer`).
import { type AgentStreamControlMessage, agentStreamControlSubject } from "@legion/contracts"
import { messageFor } from "@legion/envoy-client/errors"
import type { ChannelConnection, ChannelTopicSubscription } from "./channel-topics"

const encoder = new TextEncoder()
const decoder = new TextDecoder()

/** How long the view waits before taking its control subject again after the subscription's
 *  iterator ended, which is what a dropped connection looks like from here. */
const RESUBSCRIBE_MS = 1_000

export interface ControlSubscriptionOptions {
  /** The session's NATS connection as the view starts: the server swaps in a new one when it
   *  closes for good (`useConnection`). */
  readonly connection: ChannelConnection
  /** The session id the view names now, which the control subject names: it moves at a `/clear`
   *  or a `/new` (`moved`). */
  readonly session: () => string
  /**
   * Settles once the session's NATS connection is admitted as `session`, where the connection is
   * admitted as one session at a time: under its broker token a new id is the connection's only
   * once the move lands (`followSession`), and a subscribe before then is refused and closed by
   * the server. Where this is absent the connection may take every id's subjects, and the subject
   * is taken at once.
   */
  readonly admitted?: (session: string) => Promise<void>
  /** Where a failure that must not stop the session is written. */
  readonly warn: (text: string) => void
  /**
   * Answers one `replay` or `watch` message on the control subject of `forSession`, the session
   * the subscription was taken for: what goes to the message's reply inbox, if anything.
   * `replying` says whether the message carries one. A throw is warned of, and the next message
   * is read.
   */
  readonly answer: (
    request: AgentStreamControlMessage,
    replying: boolean,
    forSession: string,
  ) => string | undefined
}

export interface ControlSubscription {
  /** Whether the view stopped: it takes no subject, and its frames go nowhere, from then on. */
  readonly stopped: boolean
  /** Publishes on the connection held now. A closed connection throws on a publish, and a throw
   *  here would end the view's reader for good, so its frames are dropped until the server swaps
   *  in a new connection; the ring keeps them for a replay. */
  publish(subject: string, payload: string): void
  /** Takes the view's first control subject. */
  start(): void
  /** The view moved to the session id `session` names now: the old id's subject goes at once, and
   *  the new id's is taken once the connection may take it, so a later move before then takes its
   *  own. */
  moved(): void
  /** The session's NATS connection closed for good and the server opened `connection` in its
   *  place (`channelReopener`): frames go out on it from now on, and the control subject is taken
   *  on it again once the connection is admitted as the session. */
  useConnection(connection: ChannelConnection): void
  /** Stops answering the control subject. */
  stop(): void
}

export function controlSubscription(options: ControlSubscriptionOptions): ControlSubscription {
  /** The session's connection now; the server swaps in a new one when it closes for good. */
  let connection = options.connection
  let subscription: ChannelTopicSubscription | undefined
  let stopped = false

  const pump = async (taken: ChannelTopicSubscription, forSession: string): Promise<void> => {
    try {
      for await (const message of taken) {
        // Any client on the bus can publish here. A `null` payload parses cleanly and then
        // throws on `.type`, and a reply over the server's max_payload throws out of the
        // publish; either would end the iterator and take the session deaf with it.
        try {
          const parsed: unknown = JSON.parse(decoder.decode(message.data))
          if (typeof parsed !== "object" || parsed === null) continue
          const request = parsed as AgentStreamControlMessage
          if (request.type !== "replay" && request.type !== "watch") continue
          const reply = message.reply
          const answer = options.answer(request, reply !== undefined, forSession)
          if (answer === undefined || reply === undefined) continue
          connection.publish(reply, encoder.encode(answer))
        } catch (error) {
          options.warn(`a live view control message failed — ${messageFor(error)}`)
        }
      }
    } catch {
      // A dead iterator is a dropped connection, taken again below.
    }
    if (subscription === taken) subscription = undefined
    if (stopped || options.session() !== forSession) return
    const retry = setTimeout(() => {
      // Taken again meanwhile: by a new id's subject, or on the connection the server swapped in.
      if (stopped || options.session() !== forSession || subscription !== undefined) return
      subscribeWhenAdmitted(forSession)
    }, RESUBSCRIBE_MS)
    retry.unref()
  }

  /** Takes the control subject on the connection held now. A closed one throws on a subscribe, so
   *  the subject waits for the connection the server swaps in (`useConnection`). */
  function subscribe(): void {
    if (stopped || connection.isClosed()) return
    subscription?.unsubscribe()
    const forSession = options.session()
    const taken = connection.subscribe(agentStreamControlSubject(forSession))
    subscription = taken
    void pump(taken, forSession)
  }

  /** Takes `forSession`'s control subject once the connection may take it (`admitted`), unless the
   *  view moved on or took it meanwhile: a move whose credential move has not landed leaves the
   *  connection unable to take the new id's subjects, on the connection it began on and on any the
   *  server swaps in before the move lands. */
  function subscribeWhenAdmitted(forSession: string): void {
    const admitted = options.admitted?.(forSession)
    if (admitted === undefined) {
      subscribe()
      return
    }
    void admitted.then(() => {
      if (!stopped && options.session() === forSession && subscription === undefined) subscribe()
    })
  }

  return {
    get stopped(): boolean {
      return stopped
    },
    publish(subject: string, payload: string): void {
      if (!connection.isClosed()) connection.publish(subject, encoder.encode(payload))
    },
    start(): void {
      // The first subject goes through the gate too: a view starts after its session does, which
      // can be after a `/clear` or a `/new` whose move has not landed.
      subscribeWhenAdmitted(options.session())
    },
    moved(): void {
      subscription?.unsubscribe()
      subscription = undefined
      subscribeWhenAdmitted(options.session())
    },
    useConnection(next: ChannelConnection): void {
      connection = next
      if (stopped) return
      // The subscription on the closed connection is dead; the new one is taken through the gate.
      subscription?.unsubscribe()
      subscription = undefined
      subscribeWhenAdmitted(options.session())
    },
    stop(): void {
      if (stopped) return
      stopped = true
      subscription?.unsubscribe()
      subscription = undefined
    },
  }
}
