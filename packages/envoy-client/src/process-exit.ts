/**
 * Ends the process with `code` once everything already written to stdout and stderr has reached
 * the pipe: a command's answer on stdout, its usage or refusal line on stderr. `process.exit` drops
 * what Bun still holds for a full pipe, and in Bun a write's callback, and `drain`, come before the
 * bytes leave
 * (docs/solutions/testing/bun-console-log-drops-what-a-full-non-blocking-pipe-cannot-take-and-exits-0.md).
 * Ending each stream is what waits: its callback runs once the pipe has taken every byte, however
 * slow the reader. A handle nothing will use can outlive the run (a NATS dial that a stop ended
 * waits out nats.js's own dial timeout), so the process ends here rather than when that handle
 * lets go.
 *
 * Where `flushMs` is given, the process exits once that has passed whatever is still queued: a
 * reader that takes nothing keeps it that long and no longer. Each caller that passes it sets it
 * inside the grace of whatever stops its process, and writes that grace beside its bound. A Bun
 * program that cannot import this module (the Slack poster, which runs as `bun -e`, and
 * `scripts/third-party-notices.ts`, which an image copies alone) ends the same way in a copy of its
 * own.
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
