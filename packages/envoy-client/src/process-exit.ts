/**
 * Ends the process with `code` once everything already written to stdout and stderr has reached
 * the pipe: a command's answer on stdout, its usage or refusal line on stderr. `process.exit` drops
 * what Bun still holds for a full pipe
 * (docs/solutions/testing/bun-console-log-drops-what-a-full-non-blocking-pipe-cannot-take-and-exits-0.md),
 * so each stream is ended first and the exit waits on `end()`'s callback, which runs once the
 * stream has handed on every byte written to it, however slow the reader. Ending closes the
 * stream, so nothing written after it goes, and the one wait covers stderr as it covers stdout,
 * with no callback of each write to keep. A handle nothing will use can outlive the run (a NATS
 * dial that a stop ended waits out nats.js's own dial timeout), so the process ends here rather
 * than when that handle lets go.
 *
 * Where `flushMs` is given, the process exits once that has passed whatever is still queued: a
 * reader that takes nothing keeps it that long and no longer. Each caller that passes it sets it
 * inside the grace of whatever stops its process, and writes that grace beside its bound. A Bun
 * program that cannot import this module (the Slack poster, which runs as `bun -e`) ends the same
 * way in a copy of its own.
 */
export async function exitProcess(code: number, flushMs?: number): Promise<never> {
  const flushed = Promise.all([ended(process.stdout), ended(process.stderr)]);
  if (flushMs === undefined) await flushed;
  else await Promise.race([flushed, Bun.sleep(flushMs)]);
  process.exit(code);
}

/** Ends `stream` and settles once every byte written to it has been handed on, or it failed. */
function ended(stream: NodeJS.WriteStream): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  if (stream.destroyed || stream.writableFinished) {
    resolve();
  } else {
    // A reader gone (EPIPE) fails the stream: nothing is left to wait for.
    stream.once("error", () => resolve());
    stream.end(() => resolve());
  }
  return promise;
}
