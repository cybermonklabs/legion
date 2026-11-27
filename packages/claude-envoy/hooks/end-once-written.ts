/** A stream the hook ends before it exits: `end` calls back once the stream has taken every byte. */
export interface EndableStream {
  end(callback: () => void): unknown
}

/**
 * Ends the open-asks hook once what it wrote is out. Its work is done when `openAsks` returns, but
 * a credential it asked the secrets broker for may still be trading in the background, under the
 * broker client's own bounds (a 30 s attempt, `TRADE_ATTEMPT_MS`; a metadata server's 10 s; a
 * pool's 20 s), and a process with a pending request or timer does not exit on its own: with a
 * broker that never answers, the hook ran for 30 s, past the 20 s hooks.json gives it, and Claude
 * Code would have killed it. Exiting cuts that trade, which writes nothing the next process reads,
 * except on the device-login route: there the trade holds the login's lock and may yet save a
 * refresh token the broker rotated, so a hook whose process holds that lock (`holdsLock`) is left
 * to finish as before.
 *
 * The exit waits until each of `streams` (stdout and stderr) has taken every byte written to it:
 * in Bun 1.4.4 an empty write's callback, `writableLength` and `writableNeedDrain` each let an exit
 * drop the tail of a 300,000-byte write to a pipe (2 or 3 runs of 5), and `end` lost nothing (5 of
 * 5). The lock is read after that, so a trade that takes it while they drain is left to finish too.
 * Nothing the trade runs writes to `process.stderr` (its retry line goes through `console.warn`,
 * which writes to the file descriptor and needs no open stream), so ending the streams first cannot
 * fail it.
 *
 * The read of the lock and the exit are one synchronous step, with no await between them, so no
 * other task of this process runs between the two: a read that waited (an async one) let this
 * process's own trade take the lock and start a refresh in between, which the exit then cut
 * (Legion's review 6014045405: 67 of 3,000 runs of its probe; none of 3,000 with a synchronous
 * read). `holdsLock` must therefore answer synchronously.
 */
export async function endOnceWritten(
  streams: readonly EndableStream[],
  holdsLock: () => boolean,
  exit: (code: number) => void,
): Promise<void> {
  for (const stream of streams) {
    const ended = Promise.withResolvers<void>()
    stream.end(() => ended.resolve())
    await ended.promise
  }
  if (holdsLock()) return
  exit(0)
}
