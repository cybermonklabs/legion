// The open-asks hook's exit (`hooks/end-once-written.ts`) with the real device-login lock read
// (cipher-client's `loginLockHeldBy`) on a lock file of this test's own. The streams and the exit
// are the test's: each stream ends in a later task, as a pipe's does, and the exit records whether
// this process held the lock at the moment it was called, where the real exit would end the
// process and cut a trade that held it. The trade is a write of the lock naming this process, run
// at a chosen point of the event loop's order, so no row is decided by a clock.
import { expect, test } from "bun:test"
import { writeFileSync } from "node:fs"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { loginLockHeldBy } from "@legion/cipher-client/device-login-file"
import { type EndableStream, endOnceWritten } from "../hooks/end-once-written"

/** A stream whose `end` calls back in the next task, then runs `after`. */
function endsLater(after: () => void = () => {}): EndableStream {
  return {
    end: (done) =>
      setImmediate(() => {
        done()
        after()
      }),
  }
}

async function withLogin(row: (login: string, takeLock: () => void) => Promise<void>) {
  const directory = await mkdtemp(join(tmpdir(), "end-once-written-"))
  const login = join(directory, "device-login.json")
  try {
    await row(login, () =>
      writeFileSync(`${login}.lock`, `${process.pid} ${crypto.randomUUID()}\n`),
    )
  } finally {
    await rm(directory, { recursive: true, force: true })
  }
}

test("a trade of this process that takes the device-login lock as the streams end is never cut by the exit", async () => {
  await withLogin(async (login, takeLock) => {
    // The trade takes the lock in the first task that can run once the last stream has ended: the
    // earliest a trade waiting for the lock gets to take it. A lock read that waited would let it
    // run between that read and the exit.
    let traded = false
    const streams = [
      endsLater(),
      endsLater(() =>
        queueMicrotask(() => {
          takeLock()
          traded = true
        }),
      ),
    ]
    const exits: { readonly code: number; readonly lockHeld: boolean }[] = []
    await endOnceWritten(
      streams,
      () => loginLockHeldBy(login, process.pid),
      (code) => exits.push({ code, lockHeld: loginLockHeldBy(login, process.pid) }),
    )
    await Promise.resolve()
    if (!traded) throw new Error("the trade never took the lock: the row forced nothing")
    if (exits.some((exit) => exit.lockHeld)) {
      throw new Error(
        `the hook exited while its own trade held the device-login lock: ${JSON.stringify(exits)}`,
      )
    }
    // The exit came before the trade, as a real exit would end the process before it.
    expect(exits).toEqual([{ code: 0, lockHeld: false }])
  })
})

test("a process that holds the device-login lock as the streams end is left to finish its trade", async () => {
  await withLogin(async (login, takeLock) => {
    const exits: number[] = []
    await endOnceWritten(
      [endsLater(takeLock), endsLater()],
      () => loginLockHeldBy(login, process.pid),
      (code) => exits.push(code),
    )
    expect(exits).toEqual([])
  })
})
