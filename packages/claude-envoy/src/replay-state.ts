// One session's delivery record, in one file per session id under `CLAUDE_PLUGIN_DATA` (named
// `replay-<id>.json` since the first release that wrote one): the delivery ledger's
// (`@legion/envoy-client/delivery-ledger`: the topics it follows and the frames it was shown in
// the last 72 hours), consumer delivery's (`@legion/envoy-client/consumer-delivery`: the resume
// point and when each topic was first followed), and whether the session was last told it
// receives nothing, so a server that starts again mid-session tells it when it receives again.
// Claude Code can resume a session at any later time, so no sweep prunes these files; each holds
// only followed subjects, a resume point, that flag and at most 72 hours of ledger, since the
// ledger drops older entries before every write.
import { join } from "node:path"
import {
  type ConsumerDeliverySnapshot,
  ConsumerDeliverySnapshotSchema,
} from "@legion/envoy-client/consumer-delivery"
import { type LedgerSnapshot, LedgerSnapshotSchema } from "@legion/envoy-client/delivery-ledger"
import { z } from "zod"
import { writeAtomicStateFile } from "./session-state"
import { readStateFile } from "./state-file"

/**
 * A session's file: the delivery ledger's snapshot, the consumer delivery snapshot once the
 * session had one, and whether the session was last told it receives nothing (`inboundOutageTold`:
 * true from the outage notice until the notice that it receives again). `consumer` and
 * `inboundOutageTold` are optional, so a file a release before them wrote still reads, and such a
 * release reading a newer file passes over them.
 */
export const ReplayStateFileSchema = LedgerSnapshotSchema.extend({
  consumer: ConsumerDeliverySnapshotSchema.optional(),
  inboundOutageTold: z.boolean().optional(),
})

export type ReplayStateFile = z.infer<typeof ReplayStateFileSchema>

/** The `.json` suffix is what keeps a session id of `.` or `..` a file of the directory itself,
 *  since `encodeURIComponent` leaves a dot alone. */
export function replayStateFile(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, `replay-${encodeURIComponent(sessionId)}.json`)
}

/**
 * The record a session's file holds; an empty one when there is no file, which is a session that
 * never ran this plugin, or a new id (a `/clear`, a fresh start). A file that cannot be read or
 * holds no ledger snapshot throws.
 */
export async function readReplayState(file: string): Promise<ReplayStateFile> {
  return await readStateFile(file, ReplayStateFileSchema, { ledger: [] }, "holds no delivery state")
}

/** Replaces the session's file by rename, mode 0600, as every other state file is written. */
export async function writeReplayState(file: string, state: ReplayStateFile): Promise<void> {
  await writeAtomicStateFile(file, `${JSON.stringify(state)}\n`)
}

/** Writes one session's file from its three parts, each written whenever its owner changes it. */
export interface ReplayStateWriter {
  /** The delivery ledger's snapshot changed; resolves once a file holding it is in place. */
  ledger(snapshot: LedgerSnapshot): Promise<void>
  /** The consumer delivery snapshot changed; resolves once a file holding it is in place. */
  consumer(snapshot: ConsumerDeliverySnapshot): Promise<void>
  /** The session was told it receives nothing (`true`) or that it receives again (`false`);
   *  resolves once a file holding it is in place. */
  inboundOutage(told: boolean): Promise<void>
  /** Resolves once every write asked for so far has finished, failed ones included. */
  idle(): Promise<void>
}

/**
 * The writer of `file`, starting from what it held (`restored`). Every write replaces the whole
 * file with every part as it stands when it runs, one write at a time, so a write that finishes
 * last never puts back an older part another owner has replaced since.
 */
export function replayStateWriter(file: string, restored: ReplayStateFile): ReplayStateWriter {
  let current: ReplayStateFile = restored
  let writing: Promise<void> = Promise.resolve()
  const write = (next: ReplayStateFile): Promise<void> => {
    current = next
    const run = writing.then(() => writeReplayState(file, current))
    writing = run.catch(() => undefined)
    return run
  }
  return {
    ledger: (snapshot) => {
      // Every part but the ledger's is kept as it stands, without naming it, so a part added to
      // the file later is kept too. The ledger's snapshot replaces `state` and `ledger` whole
      // (`LedgerSnapshotSchema` holds only those), and one without a `state` leaves none.
      const { ledger: _ledger, state: _state, ...others } = current
      return write({ ...others, ...snapshot })
    },
    consumer: (snapshot) => write({ ...current, consumer: snapshot }),
    inboundOutage: (told) => write({ ...current, inboundOutageTold: told }),
    idle: () => writing,
  }
}
