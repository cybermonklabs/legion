import { describe, expect, test } from "bun:test"
import {
  chmodSync,
  existsSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs"
import { readdir } from "node:fs/promises"
import { join, resolve } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import {
  acceptingCodexOwner,
  currentOwner,
  hasStandbyServer,
  markCodexServerStopping,
  type ProcessProbe,
  registerServer,
  serverAlive,
  stillOwner,
  unregisterServer,
  waitForOwnership,
} from "../src/codex/codex-owner"
import {
  codexEndedFile,
  codexSessionRecordFile,
  markSessionEnded,
  processStarted,
  sessionEnded,
  watchRecord,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"
import { waitFor } from "./channel-session-harness"

const ownerModule = resolve(import.meta.dir, "../src/codex/codex-owner.ts")

/** A probe over a fixed table: pids not in it answer ESRCH. */
function probeOf(table: Record<number, string | undefined>): ProcessProbe {
  return {
    signal: (pid) => {
      if (!(pid in table)) throw Object.assign(new Error("no such process"), { code: "ESRCH" })
    },
    started: (pid) => table[pid],
  }
}

/** A probe over the servers in `running`, each started `s<pid>`, that calls `asked` first, so a row
 *  can act at the moment a look asks about a server. Pids not in `running` answer ESRCH. */
function probeOver(
  running: Set<number>,
  asked: (pid: number) => void = () => undefined,
): ProcessProbe {
  return {
    signal: (pid) => {
      asked(pid)
      if (!running.has(pid)) throw Object.assign(new Error("no such process"), { code: "ESRCH" })
    },
    started: (pid) => (running.has(pid) ? `s${pid}` : undefined),
  }
}

/** A listing for `registerServer` that holds the server right after it reads the directory, as a
 *  server the scheduler stalls there is: `reached` once it is held, `release` to let it go on. */
function heldListing() {
  const reached = Promise.withResolvers<void>()
  const released = Promise.withResolvers<void>()
  return {
    reached: reached.promise,
    release: () => released.resolve(),
    list: async (directory: string): Promise<string[]> => {
      const names = await readdir(directory)
      reached.resolve()
      await released.promise
      return names
    },
  }
}

// Root reads and writes a directory whatever its mode, so only another user can be refused.
const asRoot = process.getuid?.() === 0

/** The timeout of each row that waits for an event, given to its `test`: the package's own
 *  `--timeout=60000` (`bun run test`), stated here so the row keeps it under any runner. */
const WAITING_ROW_TIMEOUT_MS = 60_000

/** How long before its timeout a waiting row's waits give up. It covers the time from Bun starting
 *  the row's timer to the row's first line, and the last turn of `waitFor` (a 5 ms sleep and one
 *  look at the condition), so a wait that never ends fails by its own name before Bun's timeout
 *  fails the row by none. */
const WAITING_ROW_MARGIN_MS = 1_000

/** Read at a waiting row's start: the time its waits have left from now, its timeout less the
 *  margin. A wait takes it as its deadline, so the waits share the row's budget, not a bound of
 *  their own. */
function rowBudget(): () => number {
  const end = performance.now() + WAITING_ROW_TIMEOUT_MS - WAITING_ROW_MARGIN_MS
  return () => end - performance.now()
}

describe("the owner of a codex process's Envoy session", () => {
  const root = temporaryDirectory("codex-owner")

  test("a departing owner keeps the election until it finishes but stops accepting records", async () => {
    const owners = join(root, "departing")
    const owner = await registerServer(owners, process.pid, processStarted(process.pid) ?? null)
    expect(await acceptingCodexOwner(owners)).toEqual(owner)
    await markCodexServerStopping(owners, owner)
    expect(await currentOwner(owners)).toEqual(owner)
    expect(await acceptingCodexOwner(owners)).toBeUndefined()
    await unregisterServer(owners, owner)
    expect(await readdir(owners)).toEqual([])
  })

  test("is the live server that published first, whatever the clock says", async () => {
    const directory = join(root, "a")
    const first = await registerServer(directory, 101, "s101")
    const second = await registerServer(directory, 102, "s102")
    expect(second.seq).toBeGreaterThan(first.seq)
    const both = probeOf({ 101: "s101", 102: "s102" })
    expect((await currentOwner(directory, both))?.pid).toBe(101)
    expect(await stillOwner(directory, first, both)).toBe(true)
    expect(await stillOwner(directory, second, both)).toBe(false)
    expect(await hasStandbyServer(directory, first, both)).toBe(true)
    expect(await hasStandbyServer(directory, first, probeOf({ 101: "s101" }))).toBe(false)
    expect(
      await hasStandbyServer(directory, first, probeOf({ 101: "s101", 102: "reused-pid" })),
    ).toBe(false)
    // The first is gone: the second owns. No server removes another's entry.
    expect((await currentOwner(directory, probeOf({ 102: "s102" })))?.pid).toBe(102)
    await unregisterServer(directory, second)
    expect(readdirSync(directory)).toEqual([`${first.seq}.json`])
  })

  test("is gone only on ESRCH or a readable start that differs", () => {
    const entry = { pid: 101, seq: 1, started: "s101" }
    expect(serverAlive(entry, probeOf({ 101: "s101" }))).toBe(true)
    // A start that cannot be read now says nothing either way.
    expect(serverAlive(entry, probeOf({ 101: undefined }))).toBe(true)
    expect(serverAlive({ ...entry, started: null }, probeOf({ 101: "other" }))).toBe(true)
    expect(serverAlive(entry, probeOf({ 101: "reused" }))).toBe(false)
    expect(serverAlive(entry, probeOf({}))).toBe(false)
  })

  test("two servers that publish at once never hold one number", async () => {
    const directory = join(root, "b")
    const entries = await Promise.all(
      Array.from({ length: 20 }, (_, index) => registerServer(directory, 200 + index, null)),
    )
    expect(new Set(entries.map((entry) => entry.seq)).size).toBe(20)
    // Only whole entries are there: the temporary files are gone.
    expect(readdirSync(directory).filter((name) => name.endsWith(".tmp"))).toEqual([])
  })

  test("no owner is decided while an entry is still being written", async () => {
    const directory = join(root, "partial")
    await registerServer(directory, 102, "s102")
    writeFileSync(join(directory, "0.json"), "")
    expect(await currentOwner(directory, probeOf({ 102: "s102" }))).toBeUndefined()
    writeFileSync(join(directory, "0.json"), JSON.stringify({ pid: 103, started: "s103" }))
    expect((await currentOwner(directory, probeOf({ 102: "s102", 103: "s103" })))?.pid).toBe(103)
  })

  test("an entry left empty by a server killed in its write counts as dead after a few seconds", async () => {
    const directory = join(root, "stale-empty")
    await registerServer(directory, 102, "s102")
    writeFileSync(join(directory, "0.json"), "")
    const longAgo = new Date(Date.now() - 60_000)
    utimesSync(join(directory, "0.json"), longAgo, longAgo)
    expect((await currentOwner(directory, probeOf({ 102: "s102" })))?.pid).toBe(102)
  })

  test("no owner is decided above a freed number while a server is between its read of the directory and its link", async () => {
    const directory = join(root, "held")
    await registerServer(directory, 2 ** 22 + 17, "dead")
    const probe = probeOf({ 301: "s301", 302: "s302", 303: "s303" })
    // Server 301 reads the directory (one dead entry: its number is 2) and is held right after,
    // with its temporary file written before that read.
    const stalled = heldListing()
    const placing = registerServer(directory, 301, "s301", stalled.list)
    await stalled.reached
    // Meanwhile 302 publishes (2) and 303 (3), and 302, the owner, stops and frees its number.
    const first = await registerServer(directory, 302, "s302")
    const second = await registerServer(directory, 303, "s303")
    await unregisterServer(directory, first)
    // 301 can still take the number 302 freed, below 303's, so 303 is not told it owns.
    expect(await currentOwner(directory, probe)).toBeUndefined()
    stalled.release()
    const late = await placing
    expect(late.seq).toBeLessThan(second.seq)
    // The order a decision reads is final: 301 owns, and 303, which never owned, stands by.
    expect((await currentOwner(directory, probe))?.pid).toBe(301)
  })

  test("no owner is decided while a server is publishing and a server below stops inside the look that finds it gone", async () => {
    const directory = join(root, "stopped-inside")
    await registerServer(directory, 2 ** 22 + 17, "dead")
    // Server 301 reads the directory (its number is 2) and is held right after.
    const stalled = heldListing()
    const placing = registerServer(directory, 301, "s301", stalled.list)
    await stalled.reached
    const first = await registerServer(directory, 302, "s302") // 2, the owner
    const second = await registerServer(directory, 303, "s303") // 3
    // 303 looks. The look reads 302's entry; then 302 stops, removing its entry and exiting, before
    // the look asks whether 302 runs.
    const running = new Set([301, 302, 303])
    const probe = probeOver(running, (pid) => {
      if (pid !== first.pid || !running.has(pid)) return
      rmSync(join(directory, `${first.seq}.json`))
      running.delete(pid)
    })
    expect(await currentOwner(directory, probe)).toBeUndefined()
    stalled.release()
    const late = await placing
    // 301 took the number 302 freed, below 303's: 301 owns, and 303 was never told it does.
    expect(late.seq).toBeLessThan(second.seq)
    expect((await currentOwner(directory, probe))?.pid).toBe(301)
  })

  test("a temporary file the listing holds and a read then misses holds a decision back", async () => {
    const directory = join(root, "temporary-gone")
    await registerServer(directory, 2 ** 22 + 17, "dead")
    const first = await registerServer(directory, 302, "s302")
    await registerServer(directory, 303, "s303")
    await unregisterServer(directory, first) // 2 is free.
    // A name the listing holds and a read misses, as the temporary file of a server that linked its
    // entry at 2 and removed the file between this look's listing and its read: the listing holds
    // no entry at 2, and the look must not decide past it.
    symlinkSync(join(directory, "nothing"), join(directory, ".301.1.tmp"))
    expect(await currentOwner(directory, probeOf({ 301: "s301", 303: "s303" }))).toBeUndefined()
  })

  test("a server still publishing holds a decision back while it runs, its file is whole and a number below the owner's is free", async () => {
    const directory = join(root, "temporary")
    const first = await registerServer(directory, 101, "s101")
    await registerServer(directory, 102, "s102")
    writeFileSync(join(directory, ".401.1.tmp"), JSON.stringify({ pid: 401, started: "s401" }))
    // 101 is dead and its entry stays, so 401 can only take a number above 102's.
    expect((await currentOwner(directory, probeOf({ 102: "s102", 401: "s401" })))?.pid).toBe(102)
    // 101 stopped and freed its number: 401 may take it.
    await unregisterServer(directory, first)
    expect(await currentOwner(directory, probeOf({ 102: "s102", 401: "s401" }))).toBeUndefined()
    // Gone, 401 takes nothing; nor does 402, whose file is still empty: it reads the directory
    // only once the file is whole, and then takes a number above every entry there.
    expect((await currentOwner(directory, probeOf({ 102: "s102" })))?.pid).toBe(102)
    writeFileSync(join(directory, ".402.1.tmp"), "")
    expect((await currentOwner(directory, probeOf({ 102: "s102", 402: "s402" })))?.pid).toBe(102)
  })

  test.skipIf(asRoot)(
    "a server that cannot list the directory places no entry, so it takes no freed number",
    async () => {
      const directory = join(root, "unlisted")
      await registerServer(directory, 2 ** 22 + 17, "dead")
      const first = await registerServer(directory, 302, "s302")
      const second = await registerServer(directory, 303, "s303")
      await unregisterServer(directory, first)
      // Write and search, no read: a server can place a file there and cannot list what is there.
      chmodSync(directory, 0o300)
      try {
        await expect(registerServer(directory, 301, "s301")).rejects.toMatchObject({
          code: "EACCES",
        })
      } finally {
        chmodSync(directory, 0o700)
      }
      expect((await currentOwner(directory, probeOf({ 301: "s301", 303: "s303" })))?.pid).toBe(
        second.pid,
      )
    },
  )

  test.skipIf(asRoot)(
    "a standby that cannot list the directory keeps its entry and says so, and goes on once it can",
    async () => {
      const left = rowBudget()
      const directory = join(root, "standby-unlisted")
      await registerServer(directory, 101, "s101") // 1, the owner
      const running = new Set([101, 303])
      const abort = new AbortController()
      const published: number[] = []
      const warnings: string[] = []
      const state = () =>
        `published ${published.join(", ") || "nothing"}, ${warnings.length} warnings`
      const waited = waitForOwnership(
        directory,
        303,
        "s303",
        abort.signal,
        (entry) => published.push(entry.seq),
        10,
        probeOver(running),
        (text) => warnings.push(text),
      )
      let owned: number | undefined
      let ended = false
      void waited.then((entry) => {
        owned = entry?.seq
        ended = true
      })
      try {
        await waitFor(async () => published.length > 0, "the first entry", state, left())
        const entry = join(directory, `${published[0]}.json`)
        // Write and search, no read: the server cannot list the directory.
        chmodSync(directory, 0o300)
        try {
          // A look that read the failed listing as an empty directory would remove the entry.
          await waitFor(
            async () => warnings.length > 0 || !existsSync(entry),
            "a warning, or the entry gone",
            state,
            left(),
          )
          expect(existsSync(entry)).toBe(true)
        } finally {
          chmodSync(directory, 0o700)
        }
        expect(warnings).toHaveLength(1)
        expect(warnings[0]).toContain("EACCES")
        // The owner goes: the standby owns with the entry it published first.
        running.delete(101)
        await waitFor(async () => ended, "the standby to own", state, left())
        expect(owned).toBe(published[0])
        expect(published).toHaveLength(1)
      } finally {
        abort.abort()
      }
    },
    WAITING_ROW_TIMEOUT_MS,
  )

  test(
    "a standby whose number another server took publishes again behind it and leaves that server's entry",
    async () => {
      const left = rowBudget()
      const directory = join(root, "taken")
      await registerServer(directory, 101, "s101") // 1, the owner
      const running = new Set([101, 303, 404])
      const published: number[] = []
      let looks = 0
      let taken = false
      // Each look asks once whether 101 runs. Inside the first, after 303 published, 303's entry
      // goes (a failed write, or someone tidying) and 404 places its entry at that number.
      const probe = probeOver(running, (pid) => {
        if (pid !== 101) return
        looks++
        if (taken) return
        taken = true
        const entry = join(directory, `${published[0]}.json`)
        rmSync(entry)
        writeFileSync(entry, JSON.stringify({ pid: 404, started: "s404" }))
      })
      const state = () => `${looks} looks, published ${published.join(", ") || "nothing"}`
      const abort = new AbortController()
      const waited = waitForOwnership(
        directory,
        303,
        "s303",
        abort.signal,
        (entry) => published.push(entry.seq),
        10,
        probe,
      )
      let owned: number | undefined
      let ended = false
      void waited.then((entry) => {
        owned = entry?.seq
        ended = true
      })
      try {
        await waitFor(
          async () => published.length > 1 || looks > 3,
          "a second entry, or four looks",
          state,
          left(),
        )
        expect(published).toEqual([2, 3])
        expect(JSON.parse(readFileSync(join(directory, "2.json"), "utf8"))).toEqual({
          pid: 404,
          started: "s404",
        })
        // 101 goes: 404, ahead of 303, owns. Once 404 goes too, 303 owns with its new entry.
        running.delete(101)
        expect((await currentOwner(directory, probe))?.pid).toBe(404)
        running.delete(404)
        await waitFor(async () => ended, "303 to own", state, left())
        expect(owned).toBe(3)
      } finally {
        abort.abort()
      }
    },
    WAITING_ROW_TIMEOUT_MS,
  )

  test.skipIf(asRoot)(
    "a standby that cannot publish again says so once however many looks fail, and again after one has succeeded",
    async () => {
      const left = rowBudget()
      const directory = join(root, "unwritable")
      await registerServer(directory, 101, "s101") // 1, the owner
      // A dead server's whole temporary file: every look reads it and asks about 999 once for each
      // listing it reads, so the count of those asks counts the listings.
      writeFileSync(join(directory, ".999.1.tmp"), JSON.stringify({ pid: 999, started: "s999" }))
      const running = new Set([101, 303])
      const published: number[] = []
      const warnings: string[] = []
      let listings = 0
      let streakFrom: number | undefined
      let breakNext = true
      // Inside a look that asks whether 101 runs, 303's entry goes and the directory takes no new
      // file, so each look after it fails to publish again.
      const probe = probeOver(running, (pid) => {
        if (pid === 999) listings++
        if (pid !== 101 || !breakNext) return
        breakNext = false
        rmSync(join(directory, `${published.at(-1)}.json`))
        chmodSync(directory, 0o500)
        streakFrom = listings
      })
      const state = () =>
        `${listings} listings, published ${published.join(", ") || "nothing"}, ${warnings.length} warnings`
      const abort = new AbortController()
      const waited = waitForOwnership(
        directory,
        303,
        "s303",
        abort.signal,
        (entry) => published.push(entry.seq),
        10,
        probe,
        (text) => warnings.push(text),
      )
      const threeFailedLooks = () =>
        waitFor(
          async () => listings >= (streakFrom ?? Infinity) + 3,
          "three failed looks",
          state,
          left(),
        )
      try {
        await threeFailedLooks()
        expect(warnings).toHaveLength(1)
        expect(warnings[0]).toContain("EACCES")
        expect(published).toEqual([2])
        chmodSync(directory, 0o700)
        await waitFor(async () => published.length > 1, "an entry published again", state, left())
        expect(published).toEqual([2, 2])
        streakFrom = undefined
        breakNext = true
        await threeFailedLooks()
        expect(warnings).toHaveLength(2)
      } finally {
        chmodSync(directory, 0o700)
        abort.abort()
      }
      expect(await waited).toBeUndefined()
    },
    WAITING_ROW_TIMEOUT_MS,
  )

  test(
    "a server whose entry vanished publishes it again, at the back",
    async () => {
      const left = rowBudget()
      const directory = join(root, "c")
      await registerServer(directory, 101, "s101")
      const abort = new AbortController()
      const table = probeOf({ 101: "s101", [process.pid]: "me" })
      // A look decides whether to publish, then asks the probe whether server 101 still runs. 101's
      // entry stays first throughout, so the asks about it count the looks, and the row waits for
      // looks, never for a time.
      let looks = 0
      const probe: ProcessProbe = {
        ...table,
        signal: (pid) => {
          if (pid === 101) looks += 1
          table.signal(pid)
        },
      }
      const published: number[] = []
      const state = () => `${looks} looks, published ${published.join(", ") || "nothing"}`
      const waited = waitForOwnership(
        directory,
        process.pid,
        "me",
        abort.signal,
        (entry) => published.push(entry.seq),
        10,
        probe,
      )
      // The wait ends before the abort below only if the server owns, and its looks end with it; a
      // wait for looks then stops too, so the assertions say what went wrong.
      let ended = false
      void waited.then(() => {
        ended = true
      })
      try {
        // The first look publishes; the second finds the entry there and publishes nothing more.
        await waitFor(async () => looks >= 2 || ended, "two looks", state, left())
        expect(published).toHaveLength(1)
        rmSync(join(directory, `${published[0]}.json`))
        // A look under way may have read the directory before the removal. The next finds the entry
        // gone and publishes it again, and the one after finds it there.
        const removed = looks
        await waitFor(
          async () => looks >= removed + 3 || ended,
          "three looks after the removal",
          state,
          left(),
        )
        // Published again, behind the live server that published first, and reported, so the server
        // can remove it when it stops.
        expect(published).toHaveLength(2)
        expect(published[1]).toBeGreaterThan(1)
        expect(readdirSync(directory)).toContain(`${published[1]}.json`)
      } finally {
        abort.abort()
      }
      expect(await waited).toBeUndefined()
    },
    WAITING_ROW_TIMEOUT_MS,
  )

  test("two server processes racing for a dead owner's place: exactly one owns", async () => {
    for (let run = 0; run < 10; run++) {
      const directory = join(root, `race-${run}`)
      // A dead owner, published before both racers.
      await registerServer(directory, 2 ** 22 + 17, "dead")
      const racer = () =>
        Bun.spawn(
          [
            process.execPath,
            "-e",
            `const m = await import(${JSON.stringify(ownerModule)});
             const self = await m.registerServer(${JSON.stringify(directory)}, process.pid, null);
             await Bun.sleep(50);
             const owner = await m.currentOwner(${JSON.stringify(directory)});
             process.stdout.write(owner?.pid === process.pid ? "owner" : "standby");
             await Bun.sleep(300);`,
          ],
          { stdout: "pipe" },
        )
      const a = racer()
      const b = racer()
      const said = [await new Response(a.stdout).text(), await new Response(b.stdout).text()]
      await Promise.all([a.exited, b.exited])
      expect(said.sort()).toEqual(["owner", "standby"])
    }
  }, 60_000)
})

describe("a session's end", () => {
  const root = temporaryDirectory("codex-ended")

  test("leaves the marker and touches the record, so a server watching the record reads it", async () => {
    const record = codexSessionRecordFile(root, 7)
    await writeCodexSessionRecord(record, {
      codex_executable: null,
      codex_home: "/h",
      codex_started: "1",
      cwd: "/w",
      session_id: "s-1",
      source: null,
      transcript_path: "/t",
    })
    const before = statSync(record).mtimeMs
    await Bun.sleep(20)
    await markSessionEnded(root, 7, "s-1")
    expect(await sessionEnded(codexEndedFile(root, 7, "s-1"))).toBe(true)
    expect(statSync(record).mtimeMs).toBeGreaterThan(before)
    expect(await sessionEnded(codexEndedFile(root, 7, "s-2"))).toBe(false)
  })
})

describe("watching the record", () => {
  const root = temporaryDirectory("codex-watch-record")

  test(
    "reads an end marked before the watch began, though nothing moves the record again",
    async () => {
      const left = rowBudget()
      const record = codexSessionRecordFile(root, 9)
      await writeCodexSessionRecord(record, {
        codex_executable: null,
        codex_home: "/h",
        codex_started: processStarted(process.pid) ?? "1",
        cwd: "/w",
        session_id: "s-1",
        source: null,
        transcript_path: "/t",
      })
      await markSessionEnded(root, 9, "s-1")
      const seen: [string, boolean][] = []
      const stop = watchRecord(
        record,
        process.pid,
        (sessionId) => sessionEnded(codexEndedFile(root, 9, sessionId)),
        (next, ended) => seen.push([next.session_id, ended]),
        50,
      )
      try {
        // The watch reads the record once at once; nothing moves the record after that.
        await waitFor(async () => seen.length > 0, "the watch's first look", undefined, left())
      } finally {
        stop()
      }
      expect(seen[0]).toEqual(["s-1", true])
    },
    WAITING_ROW_TIMEOUT_MS,
  )
})
