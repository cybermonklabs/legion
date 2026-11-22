// packages/envoy-client/src/inbound-connection.ts
//
// What a session plugin holds its inbound messages over: a nats.js `NatsConnection` is one, and so
// is the listener's relay socket (`./listener-relay.ts`). Declared here once, apart from either, so
// a host describes its connection without importing the socket's implementation, and both hosts
// read one contract (claude-envoy names it `ChannelConnection`, `../../claude-envoy/src/channel-topics.ts`).

import type { DebugEvents, Events } from "nats";
import type { RelayHeaders } from "./consumer-delivery";

/** A message on an inbound subscription: a nats.js `Msg` is one, and so is a relay socket's. */
export interface InboundMessage {
  readonly subject: string;
  readonly data: Uint8Array;
  readonly reply?: string;
  readonly headers?: RelayHeaders;
}

/** A subscription of an inbound connection, which ends when it is closed, or with the error a
 *  server refused it with. */
export interface InboundSubscription extends AsyncIterable<InboundMessage> {
  unsubscribe(): void;
}

/** A connection's status event, as nats.js names it; only its `type` is read (`disconnect`,
 *  `reconnect`), so a host that compares it against a name nats.js has none for fails to compile. */
export interface InboundStatus {
  readonly type: `${Events}` | `${DebugEvents}`;
}

/** The connection a session plugin holds its inbound messages over. */
export interface InboundConnection {
  subscribe(subject: string): InboundSubscription;
  publish(subject: string, data?: Uint8Array): void;
  /** A round trip: resolves once the server has taken everything sent before it. */
  flush(): Promise<void>;
  isClosed(): boolean;
  drain(): Promise<void>;
  close(): Promise<void>;
  /** The connection's status events, which end when it closes for good. */
  status(): AsyncIterable<InboundStatus>;
  /** Settles once the connection is closed for good; nats.js settles it with the error that closed
   *  the connection, where one did. */
  closed(): Promise<unknown>;
}
