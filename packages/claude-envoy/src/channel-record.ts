import { jsonLineRecord } from "@legion/envoy-client/json-line-record"

/**
 * Tests only: the file the channel server appends one JSON line to for each step below, so a test
 * on the other side of the process boundary reads what the server did from the server's own
 * record rather than from a clock: the settings it dialled NATS with (`dial`), each status event
 * nats.js reported on that connection (`status`), each registration the listener answered on
 * consumer delivery with the epoch it named (`registered`), and each Envoy event the session was
 * shown (`shown`). The lines are in the order the server took the steps, which is what a test
 * reads of when each happened. Unset, as Claude Code runs the server, nothing is written.
 */
export const CHANNEL_RECORD_FILE = "ENVOY_CHANNEL_RECORD_FILE"

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

/** Appends `line` to the record; never throws. */
export type ChannelRecord = (line: ChannelRecordLine) => void

/** The record `env` names, or undefined where it names none. */
export function channelRecord(env: Record<string, string | undefined>): ChannelRecord | undefined {
  return jsonLineRecord<ChannelRecordLine>(env, CHANNEL_RECORD_FILE)
}
