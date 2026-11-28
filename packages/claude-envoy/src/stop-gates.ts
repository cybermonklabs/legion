/**
 * A channel server's tell gate over its stderr (R24 of dispatch://INFRA-3589). Once the session's
 * stop has begun, the server tells the session and its stderr nothing (the tell gate), and writes
 * no role record and no ledger entry (the write gate). Each notice card, role record and ledger
 * entry reads the stop itself before it is told or written; the stderr lines of the server's own
 * go through the writer below. The stop's own lines are written with `process.stderr.write`,
 * which has no gate (`ChannelSession.shutdown`, `ChannelTopics.close`). The listener calls have a
 * gate of their own, in the client (`EnvoyClientConfig.stopping`).
 */

/** A writer of stderr lines for one stop reading, `stopping`: a session's, or its server's. It
 *  writes each line to stderr before the stop has begun, and nothing once it has. */
export function gatedStderr(stopping: () => boolean): (line: string) => void {
  return (line) => {
    if (!stopping()) process.stderr.write(line)
  }
}
