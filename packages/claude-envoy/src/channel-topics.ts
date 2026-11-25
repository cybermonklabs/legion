import { DedupeIdentitySchema } from "@legion/envoy-client/delivery"
import { natsFailureReason } from "@legion/envoy-client/errors"
import type {
  InboundConnection,
  InboundMessage,
  InboundStatus,
  InboundSubscription,
} from "@legion/envoy-client/inbound-connection"
import { expandSubscriptionTopics } from "@legion/envoy-client/transport"
import { z } from "zod"

/**
 * A message as the connection yields it (a nats.js `Msg`, or a relay socket's). Two kinds carry a
 * reply: a relay frame, whose reply is the relay's own answer token (`RelayMessage`, consumer
 * delivery), and a request on the live view's control subject, answered there
 * (`./agent-stream-runner`). No Envoy envelope is answered on a reply: a session publishes no
 * receipt. Only a relay frame carries headers read here.
 */
export type ChannelBrokerMessage = InboundMessage

/** A subscription of the channel session's connection. */
export type ChannelTopicSubscription = InboundSubscription

/** A status event of the channel session's connection; only its `type` is read. */
export type ChannelConnectionStatus = InboundStatus

/**
 * The inbound connection a channel session holds, NATS or the listener's relay socket
 * (`@legion/envoy-client/inbound-connection`). Over NATS a channel session opens two
 * subscriptions on it: the session's relay subject, where every Envoy message arrives, and the
 * live view's control subject, which a viewer takes the session on (`./agent-stream-runner`). Over
 * the relay socket it opens the relay subject alone: the socket carries no live view.
 */
export type ChannelConnection = InboundConnection

/**
 * The topics this session follows, and the broker connection it holds now. Nothing is subscribed
 * for a topic: the listener relays every followed subject from the session's own durable consumer
 * (`ConsumerDelivery`), so a topic is followed by registering it, and the only subscription that
 * carries Envoy traffic is that consumer's relay subject.
 */
export interface ChannelTopics {
  /** Start following a topic; one already followed is left alone. */
  follow(topic: string): void
  /** Stop following the given topics, or every topic when the list is empty. */
  unfollow(topics: readonly string[]): void
  /** Topics currently followed, in follow order. */
  topics(): readonly string[]
  /** True once the connection held now is closed for good. */
  isClosed(): boolean
  /** Drain the connection held now; never rejects. */
  close(): Promise<void>
}

export interface ChannelTopicsOptions {
  /** How long close waits for a broker drain before closing outright. */
  readonly drainTimeoutMs?: number
}

// A delivery's identity fields parse independently: a field the shared rule refuses identifies
// nothing, and its valid sibling can still recognise a repeat. `topic` follows the same rule
// before it is exposed as the envelope topic.
const DeliveryIdentity = DedupeIdentitySchema.extend({
  event_id: DedupeIdentitySchema.shape.event_id.catch(undefined),
  dedupe_key: DedupeIdentitySchema.shape.dedupe_key.catch(undefined),
  source: DedupeIdentitySchema.shape.source.catch(undefined),
  source_event_id: DedupeIdentitySchema.shape.source_event_id.catch(undefined),
  topic: z.string().min(1).optional().catch(undefined),
})

const DEFAULT_DRAIN_TIMEOUT_MS = 1_000

/** A frame's identity fields, each kept only when valid; undefined for a frame that is not JSON. */
export function deliveryIdentity(raw: string): z.infer<typeof DeliveryIdentity> | undefined {
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return undefined
  }
  const identity = DeliveryIdentity.safeParse(parsed)
  return identity.success ? identity.data : undefined
}

/** A failed drain or close of the NATS connection, as a log may print it (`natsFailureReason`). */
function report(what: string, error: unknown): void {
  process.stderr.write(`envoy-channel: ${what} — ${natsFailureReason(error)}\n`)
}

/**
 * One channel process follows its direct Envoy route and every topic a human or a tool added to
 * it. Each is a concrete subject the registration carries (`expandSubscriptionTopics`), and the
 * listener relays all of them from the session's own consumer, so nothing here subscribes: the
 * set is what a registration advertises and what the relay is filtered on. `connection` answers
 * the connection the session holds now: a server opens a new one when its connection closes for
 * good (`ChannelSessionOptions.reopen`).
 */
export function createChannelTopics(
  connection: () => ChannelConnection,
  options: ChannelTopicsOptions = {},
): ChannelTopics {
  const following = new Set<string>()
  const drainTimeoutMs = options.drainTimeoutMs ?? DEFAULT_DRAIN_TIMEOUT_MS

  return {
    follow(topic) {
      for (const subject of expandSubscriptionTopics([topic])) following.add(subject)
    },
    unfollow(topics) {
      if (topics.length === 0) {
        following.clear()
        return
      }
      for (const subject of expandSubscriptionTopics(topics)) following.delete(subject)
    },
    topics() {
      return [...following]
    },
    isClosed() {
      return connection().isClosed()
    },
    async close() {
      following.clear()
      // The connection held as the close begins: the session swaps none in once it is stopping.
      const held = connection()
      if (held.isClosed()) return
      const timeout = Promise.withResolvers<void>()
      const timer = setTimeout(timeout.resolve, drainTimeoutMs)
      try {
        await Promise.race([held.drain(), timeout.promise])
      } catch (error) {
        report("draining the broker connection failed", error)
      }
      clearTimeout(timer)
      if (held.isClosed()) return
      try {
        await held.close()
      } catch (error) {
        report("closing the broker connection failed", error)
      }
    },
  }
}
