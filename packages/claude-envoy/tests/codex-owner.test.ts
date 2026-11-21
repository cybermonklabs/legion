import { describe, expect, test } from "bun:test"
import { readdirSync, rmSync, statSync, utimesSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import {
  currentOwner,
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

  test("is the live server that published first, whatever the clock says", async () => {
    const directory = join(root, "a")
    const first = await registerServer(directory, 101, "s101")
    const second = await registerServer(directory, 102, "s102")
    expect(second.seq).toBeGreaterThan(first.seq)
    const both = probeOf({ 101: "s101", 102: "s102" })
    expect((await currentOwner(directory, both))?.pid).toBe(101)
    expect(await stillOwner(directory, first, both)).toBe(true)
    expect(await stillOwner(directory, second, both)).toBe(false)
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
