import type { Socket } from "bun"

/** One connection the gate accepted, by its ports. */
export interface GateLeg {
  /** The port the client dialed the gate from: its socket's local port. */
  readonly clientPort: number
  /** The port the gate dialed the broker from, once that leg opened: what the broker sees as the
   *  client's port. */
  upstreamPort: number | undefined
}

export interface NatsGate {
  /** The address a channel server dials, in place of the broker's. */
  readonly url: string
  /** Connections the gate has accepted, shut or open: one per dial the client made. */
  readonly attempts: number
  /** Every connection accepted, in order. */
  readonly legs: readonly GateLeg[]
  /** Carry each new connection to the broker from now on. */
  open(): void
  /** Close each new connection at once from now on; a connection already carried stays up. */
  shut(): void
  stop(): void
}

/**
 * A TCP gate in front of the broker at `brokerUrl`, a FakeNatsServer's url: while shut, it closes
 * each connection at once, which fails a NATS dial at once; once opened, it carries each new
 * connection to the broker. It starts shut. `accepted` is told each connection as it opens, while
 * the client's socket is still up. It listens on the broker's own address, at a port of its own,
 * so a client dialing 127.0.0.1 never reaches it and never counts among its `attempts`
 * (fake-nats-server.ts's fakeHost).
 */
export function natsGate(brokerUrl: string, accepted?: (leg: GateLeg) => void): NatsGate {
  interface Leg {
    peer?: Socket<Leg>
    queued: Uint8Array[]
    leg?: GateLeg
  }
  const broker = new URL(brokerUrl)
  let open = false
  const legs: GateLeg[] = []
  const listener = Bun.listen<Leg>({
    hostname: broker.hostname,
    port: 0,
    socket: {
      open(client) {
        const leg: GateLeg = { clientPort: client.remotePort, upstreamPort: undefined }
        legs.push(leg)
        accepted?.(leg)
        client.data = { queued: [], leg }
        if (!open) {
          client.end()
          return
        }
        void Bun.connect<Leg>({
          hostname: broker.hostname,
          port: Number(broker.port),
          socket: {
            open(upstream) {
              leg.upstreamPort = upstream.localPort
              upstream.data = { peer: client, queued: [] }
              client.data.peer = upstream
              for (const chunk of client.data.queued) upstream.write(chunk)
              client.data.queued = []
            },
            data(upstream, chunk) {
              upstream.data.peer?.write(chunk)
            },
            close(upstream) {
              upstream.data.peer?.end()
            },
          },
        })
      },
      data(client, chunk) {
        if (client.data.peer === undefined) client.data.queued.push(new Uint8Array(chunk))
        else client.data.peer.write(chunk)
      },
      close(client) {
        client.data.peer?.end()
      },
    },
  })
  return {
    url: `nats://${broker.hostname}:${listener.port}`,
    get attempts() {
      return legs.length
    },
    legs,
    open: () => {
      open = true
    },
    shut: () => {
      open = false
    },
    stop: () => listener.stop(true),
  }
}
