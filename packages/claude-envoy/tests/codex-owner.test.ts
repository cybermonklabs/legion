import { describe, expect, test } from "bun:test"
import { readdirSync, rmSync, statSync, utimesSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { settled, until } from "@legion/test-fixtures/wait"
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
    const table = probeOf({ 101: "s101", [process.pid]: "me" })
    // Each look asks the probe about the server that published first once it has decided whether
    // to publish, so the count of those asks is the count of looks decided.
    let looks = 0
    const probe: ProcessProbe = {
      ...table,
      signal: (pid) => {
        if (pid === 101) looks += 1
        table.signal(pid)
      },
    }
    /** Waits until two more looks have asked the probe, the second a whole look after now. */
    const twoLooksOn = async (what: string): Promise<void> => {
      const from = looks
      await until(what, () => looks >= from + 2)
    }
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
    await until("the server's first entry", () => published.length > 0)
    await twoLooksOn("a look after the first entry")
    // One entry while it stands: a look publishes again only once the entry it had is gone, and
    // it is there.
    expect(published).toHaveLength(1)
    expect(readdirSync(directory)).toContain(`${published[0]}.json`)
    rmSync(join(directory, `${published[0]}.json`))
    await until("the entry published again", () => published.length > 1)
    await twoLooksOn("a look after the entry published again")
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
      // Each racer publishes, then waits at a gate this test opens once both have, so each reads
      // the owner with both entries there; it says what it read, and stays alive until this test
      // closes its stdin once both have said, so neither reads the other as gone.
      const racer = () => {
        const child = Bun.spawn(
          [
            process.execPath,
            "-e",
            `const m = await import(${JSON.stringify(ownerModule)});
             const gate = console[Symbol.asyncIterator]();
             await m.registerServer(${JSON.stringify(directory)}, process.pid, null);
             process.stdout.write("registered\\n");
             await gate.next();
             const owner = await m.currentOwner(${JSON.stringify(directory)});
             process.stdout.write(owner?.pid === process.pid ? "owner\\n" : "standby\\n");
             await gate.next();`,
          ],
          { stdin: "pipe", stdout: "pipe" },
        )
        let said = ""
        void (async () => {
          for await (const chunk of child.stdout.pipeThrough(new TextDecoderStream())) said += chunk
        })()
        return {
          child,
          lines: () => said.split("\n").filter((line) => line !== ""),
          open: () => {
            child.stdin.write("go\n")
            child.stdin.flush()
          },
          release: () => child.stdin.end(),
        }
      }
      const racers = [racer(), racer()]
      await until("both racers to publish", () =>
        racers.every((each) => each.lines().includes("registered")),
      )
      for (const each of racers) each.open()
      await until("both racers to say what they read", () =>
        racers.every((each) => each.lines().length > 1),
      )
      const said = racers.map((each) => each.lines()[1])
      for (const each of racers) each.release()
      await settled("both racers to exit", Promise.all(racers.map((each) => each.child.exited)))
      expect(said.sort()).toEqual(["owner", "standby"])
    }
  })
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
    // The touch is what a server watching the record reads (`watchFile` compares its stat), so
    // the record's stamp is set back first and must move.
    const longAgo = new Date(Date.now() - 60_000)
    utimesSync(record, longAgo, longAgo)
    const stale = statSync(record).mtimeMs
    await markSessionEnded(root, 7, "s-1")
    expect(await sessionEnded(codexEndedFile(root, 7, "s-1"))).toBe(true)
    expect(statSync(record).mtimeMs).not.toBe(stale)
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
    await until("the watch's first look", () => seen.length > 0)
    stop()
    expect(seen[0]).toEqual(["s-1", true])
  })
})
