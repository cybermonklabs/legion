/**
 * Ends the channel server's process with `code` once everything already written to stdout and
 * stderr has reached the pipe: an MCP reply Claude Code reads on stdout, or the failure line on
 * stderr. A handle nothing will use can outlive the run (a NATS dial that a stop ended waits out
 * nats.js's own dial timeout), so the process ends here rather than when that handle lets go; but
 * `process.exit` drops what Bun still holds for a full pipe, and in Bun a write's callback, and
 * `drain`, come before the bytes leave. Ending each stream is what waits: its callback runs once
 * the pipe has taken every byte, however slow the reader.
 */
export async function exitProcess(code: number): Promise<never> {
  await Promise.all([ended(process.stdout), ended(process.stderr)])
  process.exit(code)
}

/** Ends `stream` and settles once every byte written to it has been handed on, or it failed. */
function ended(stream: NodeJS.WriteStream): Promise<void> {
  return new Promise((settle) => {
    if (stream.destroyed || stream.writableFinished) return settle()
    // A reader gone (EPIPE) fails the stream: nothing is left to wait for.
    stream.once("error", () => settle())
    stream.end(() => settle())
  })
}
