import { jsonLineRecord } from "@legion/envoy-client/json-line-record"

/**
 * Tests only: the file the channel server appends one JSON line to for each step below, so a test
 * on the other side of the process boundary reads the bound the server chose for a step and what
 * ended it, from the server's own record rather than from a clock: the settings it dialled NATS
 * with (`dial`), each status event nats.js reported on that connection (`status`), each
 * registration the listener answered on consumer delivery with the epoch it named (`registered`),
 * each Envoy event the session was shown (`shown`), each write of a role the session took, when it
 * was queued on the registry chain and when the chain made it (`roleWrite`), and, on the way out,
 * its start with the bound it holds the whole way to (`shutdown`), each step of the way with what
 * ended it and, where the step has a bound of its own, the delay it armed (`wait`), and how the way
 * ended (`shutdown`). The lines are in the order the server took the steps, which is what a test
 * reads of when each happened. Unset, as Claude Code runs the server, nothing is written.
 */
export const CHANNEL_RECORD_FILE = "ENVOY_CHANNEL_RECORD_FILE"

/**
 * One step of the way out, in the order the shutdown takes them (`./way-out`): `drain`, the NATS
 * drain; `close`, the drained connection's close; `settle`, the registration writes still in
 * flight, against the shutdown's settle budget; `deregister`, the DELETE of the session's record;
 * `state` and `turns`, the last writes of the state and turn records; `bind`, the mod link's bind,
 * which a stop can find still running; `mod`, the mod link's close; `start-drain` and
 * `start-close`, the drain and close of a connection whose start failed before a session came up;
 * `server`, the MCP server's close.
 */
export type ShutdownWait =
  | "drain"
  | "close"
  | "settle"
  | "deregister"
  | "state"
  | "turns"
  | "bind"
  | "mod"
  | "start-drain"
  | "start-close"
  | "server"

/** One line of the record. */
export type ChannelRecordLine =
  | {
      readonly dial: {
        readonly pingInterval: number | undefined
        readonly maxPingOut: number | undefined
        readonly timeout: number | undefined
        readonly reconnectTimeWait: number | undefined
        readonly reconnectJitter: number | undefined
      }
    }
  | { readonly status: string; readonly data: unknown }
  | {
      /** The epoch the registration named (`delivery.epoch`), which the listener answered on
       *  consumer delivery. */
      readonly registered: string
    }
  | {
      /** The Envoy event the session was shown (the mod's `session.message` `eventId`): no text
       *  of the message, which may be a person's own. */
      readonly shown: string
    }
  | {
      /** A role's record write (`ChannelSession.rememberRole`): `queued`, put on the registry
       *  chain, behind whatever the chain holds; `made`, the chain's turn came and the write was
       *  made. */
      readonly roleWrite: "queued" | "made"
      /** The role the record names. */
      readonly role: string
    }
  | {
      /** One step of the way out (`ShutdownWait`). */
      readonly wait: ShutdownWait
      /** The delay the step's own bound was armed with; absent for a step that waits for its
       *  fact alone. */
      readonly armedMs?: number
      /** `fact`: what the step waited for came first; `bound`: its own bound did. */
      readonly endedBy: "fact" | "bound"
    }
  | {
      /** The signal or the end of stdin that stops the server came: the way out starts, and the
       *  process exits once `exitBoundMs` have passed, whatever is still running. */
      readonly shutdown: "began"
      readonly exitBoundMs: number
    }
  | {
      /** `completed`: every step of the way out ended and nothing is left for the server to do;
       *  `exited`: the process's `exit` event, the last thing it runs. */
      readonly shutdown: "completed" | "exited"
    }
  | {
      /** The exit bound came before the way out ended, and the process exits 1 on it, dropping
       *  `dropped`: the steps still running, in the order they started. */
      readonly shutdown: "bound"
      readonly dropped: readonly ShutdownWait[]
    }

/** Appends `line` to the record; never throws. */
export type ChannelRecord = (line: ChannelRecordLine) => void

/** The record `env` names, or undefined where it names none. */
export function channelRecord(env: Record<string, string | undefined>): ChannelRecord | undefined {
  return jsonLineRecord<ChannelRecordLine>(env, CHANNEL_RECORD_FILE)
}
