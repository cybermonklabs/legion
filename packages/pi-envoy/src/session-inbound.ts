import type { InboundConnection } from "@legion/envoy-client/inbound-connection";

/**
 * This session's inbound connection as the extension dialled it: NATS, or the listener's relay
 * socket under `ENVOY_INBOUND=listener`. Which of the two is decided once, at the dial, from the
 * session's settings, and what differs between them travels with the connection, so no caller
 * asks which it holds.
 */
export interface SessionInbound extends InboundConnection {
  /** What a log line calls it. */
  readonly name: "NATS connection" | "relay socket";
  /** Whether it carries the agent stream a live view takes: the relay socket carries none, and
   *  refuses the control subject (`subject_not_granted`). */
  readonly liveView: boolean;
  /**
   * Follows the session the host runs now, within `boundMs`: the relay socket opens again for it
   * with that session's own token (`ListenerRelay.follow`); a NATS connection that presents a
   * broker token is admitted again with that session's (`followNatsCredential`); a NATS
   * connection with any other credential has nothing to do.
   */
  follow(boundMs: number): Promise<void>;
  /** Starts what keeps it for its life (a NATS connection's broker-token keeper,
   *  `keepNatsCredential`), and answers how to stop that. */
  keep(): () => void;
}

/** `connection`, each of whose `InboundConnection` methods answers as its own, with `traits`. */
export function sessionInbound(
  connection: InboundConnection,
  traits: Pick<SessionInbound, "name" | "liveView" | "follow" | "keep">
): SessionInbound {
  return {
    subscribe: (subject) => connection.subscribe(subject),
    publish: (subject, data) => connection.publish(subject, data),
    flush: () => connection.flush(),
    isClosed: () => connection.isClosed(),
    drain: () => connection.drain(),
    close: () => connection.close(),
    status: () => connection.status(),
    closed: () => connection.closed(),
    ...traits,
  };
}
