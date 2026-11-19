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

  test("a server whose entry vanished publishes it again, at the back", async () => {
    const directory = join(root, "c")
    await registerServer(directory, 101, "s101")
    const abort = new AbortController()
    const probe = probeOf({ 101: "s101", [process.pid]: "me" })
    const published: number[] = []
    const waited = waitForOwnership(
      directory,
      process.pid,
      "me",
      abort.signal,
      (entry) => published.push(entry.seq),
      10,
      probe,
    )
    await Bun.sleep(30)
    expect(published).toHaveLength(1)
    rmSync(join(directory, `${published[0]}.json`))
    await Bun.sleep(50)
    // Published again, behind the live server that published first, and reported, so the server
    // can remove it when it stops.
    expect(published).toHaveLength(2)
    expect(published[1]).toBeGreaterThan(1)
    expect(readdirSync(directory)).toContain(`${published[1]}.json`)
    abort.abort()
    expect(await waited).toBeUndefined()
  })

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

  test("reads an end marked before the watch began, though nothing moves the record again", async () => {
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
    await Bun.sleep(100)
    stop()
    expect(seen[0]).toEqual(["s-1", true])
  })
})
