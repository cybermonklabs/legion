// How a program of this repository ends once what it wrote has reached its reader.
//
// Bun writes to a pipe without blocking, keeps in a queue what a full pipe refuses, and drops that
// queue at `process.exit`, with the exit status unchanged (node does the same). So a program that
// writes past what its pipe holds and then calls `process.exit` hands a reader the machine runs
// late its output cut at any byte: measured with Bun 1.4.4, 200,000 bytes written and then
// `process.exit(0)` reached a one-page pipe read 3 s late as 4,096 bytes, 3 runs of 3. A program
// that ends by itself (`process.exitCode`) waits for the queue; one that must end at once, because
// a handle nothing will use can outlive its run, ends through `exitProcess`.

/**
 * Ends this process with `code` once everything written to stdout and stderr before this call has
 * reached the pipe, however slow the reader. Ending each stream is what waits: its callback runs
 * once the pipe has taken every byte (200,000 bytes reached a one-page pipe read 3 s late whole,
 * 3 runs of 3). A reader gone (EPIPE) ends the wait too, since nothing is left to wait for.
 *
 * A write made after this call is refused, its callback given `ERR_STREAM_WRITE_AFTER_END`, as
 * any write after `end` is; the refusal is made here, before the stream sees the write. At Bun
 * 1.4.4 the stream's own refusal emits that error and leaves the stream never finished, so `end`'s
 * callback never comes: a wait that settled on the error exited with the queue unwritten (4,096
 * of 200,000 bytes reached the late reader), and one that waited past it never ended.
 */
export async function exitProcess(code: number): Promise<never> {
  await Promise.all([ended(process.stdout), ended(process.stderr)]);
  process.exit(code);
}

/**
 * Writes `text` to `stream` and answers, once the reader has taken all of it or will take none,
 * whether every byte reached it: true when the write's callback runs with no error, false when it
 * runs with one, as it does with EPIPE once the reader closed its end. Bun runs the callback only
 * after the last byte reached the pipe (200,000 bytes, a one-page pipe read 3 s late, 3 runs of 3).
 * A stream's `end` cannot say which: Bun runs its callback with no error after an EPIPE too. After
 * a failed write Bun also emits the error on the stream, which would end the process with it
 * unhandled, so it is taken here.
 */
export function writeOut(stream: NodeJS.WriteStream, text: string): Promise<boolean> {
  const { promise, resolve } = Promise.withResolvers<boolean>();
  const failed = () => resolve(false);
  stream.once("error", failed);
  stream.write(text, (error) => {
    if (error === null || error === undefined) {
      stream.off("error", failed);
      resolve(true);
    } else resolve(false);
  });
  return promise;
}

/** Ends `stream` and settles once every byte written to it has been handed on, or it failed.
 *  From the call on the stream takes no write (`exitProcess` says why). */
function ended(stream: NodeJS.WriteStream): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  stream.write = refuseWrite;
  if (stream.destroyed || stream.writableFinished) resolve();
  else {
    // A reader gone (EPIPE) fails the stream: nothing is left to wait for.
    stream.once("error", () => resolve());
    stream.end(() => resolve());
  }
  return promise;
}

/** A stream's `write` once `exitProcess` has ended it: the write is refused, as a write after
 *  `end` is, through its callback, and never reaches the stream. */
function refuseWrite(_chunk: unknown, encoding?: unknown, callback?: unknown): boolean {
  const done = typeof encoding === "function" ? encoding : callback;
  if (typeof done === "function") {
    const error = Object.assign(new Error("write after end"), {
      code: "ERR_STREAM_WRITE_AFTER_END",
    });
    process.nextTick(done, error);
  }
  return false;
}
