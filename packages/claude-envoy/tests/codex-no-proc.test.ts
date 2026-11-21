// The Codex plugin's reads of a process on a host with no /proc (macOS), run on any host by
// pointing `processReads` at a directory that is no /proc and at a stand-in `ps`: the start and the
// arguments `ps` gives, a `ps` that does not answer in the time a hook has, one that cannot run,
// a spawn that fails (`ENOENT`, `EAGAIN`, `EMFILE`) where the server reads without a deadline, the
// owner probe, and the `SessionStart` hook's budget against Codex's timeout.

import { afterEach, beforeAll, describe, expect, test } from "bun:test"
import { chmodSync, existsSync, readFileSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { serverAlive } from "../src/codex/codex-owner"
import {
  codexSessionRecordFile,
  processReads,
  processStarted,
  processStartedLater,
  SESSION_START_HOOK_TIMEOUT_MS,
  sharedAppServer,
  sharedAppServerLater,
} from "../src/codex/codex-session-record"

const packageRoot = resolve(import.meta.dir, "..")
const root = temporaryDirectory("codex-no-proc")
const saved = { ...processReads }

/** A stand-in `ps` at `name` running `body` (sh), which logs each call to `<name>.calls`. */
function standInPs(name: string, body: string): string {
  const path = join(root, name)
  writeFileSync(path, `#!/bin/sh\necho "$*" >> '${path}.calls'\n${body}\n`)
  chmodSync(path, 0o755)
  return path
}

/** Answers `-o lstart=` with a fixed start and `-o args=` with a shared app server's arguments. */
const answering = join(root, "ps-answers")
/** Never answers. */
const hanging = join(root, "ps-hangs")
/** Never answers; the hook's own, so its calls are counted alone. */
const hookHanging = join(root, "ps-hangs-hook")

// After `root`'s own beforeAll, which makes the directory.
beforeAll(() => {
  standInPs(
    "ps-answers",
    'case "$2" in lstart=) echo "Sat Nov 21 22:00:00 2026" ;; args=) echo "codex app-server --listen x" ;; esac',
  )
  standInPs("ps-hangs", "exec sleep 60")
  standInPs("ps-hangs-hook", "exec sleep 60")
})

/** Points the reads at no /proc and at `ps`. */
function noProc(ps: string): void {
  processReads.hasProc = false
  processReads.proc = join(root, "no-proc")
  processReads.ps = ps
}

afterEach(() => {
  Object.assign(processReads, saved)
})

/** A spawn that fails as Bun's does when it cannot start a process, with errno `code`. */
function failingSpawn(code: string): () => never {
  return () => {
    throw Object.assign(new Error(`spawn ps ${code}`), { code })
  }
}

describe("with no /proc, a hook's read of a process", () => {
  test("is the start ps gives, prefixed ps:", () => {
    noProc(answering)
    expect(processStarted(process.pid, 60_000)).toBe("ps:Sat Nov 21 22:00:00 2026")
    expect(sharedAppServer(process.pid, 60_000)).toBe(true)
  }, 60_000)

  test("names a ps that does not answer within the time given, and reads no start", () => {
    noProc(hanging)
    let failure = ""
    expect(processStarted(process.pid, 300, (why) => (failure = why))).toBeUndefined()
    expect(failure).toBe("ps did not answer within 300 ms")
  }, 60_000)

  test("runs no ps when no time is left, and says so", () => {
    const quiet = standInPs("ps-unused", "exit 0")
    noProc(quiet)
    let failure = ""
    expect(processStarted(process.pid, 0, (why) => (failure = why))).toBeUndefined()
    expect(failure).toBe("no time was left to run ps")
    expect(sharedAppServer(process.pid, 0)).toBe(false)
    expect(existsSync(`${quiet}.calls`)).toBe(false)
  }, 60_000)

  test("names a ps that cannot run", () => {
    noProc(join(root, "no-such-ps"))
    let failure = ""
    expect(processStarted(process.pid, 60_000, (why) => (failure = why))).toBeUndefined()
    expect(failure).toStartWith("ps could not run: ")
    expect(sharedAppServer(process.pid, 60_000)).toBe(false)
  }, 60_000)
})

describe("with no /proc, the server's read of a process", () => {
  test("is the start and the arguments ps gives", async () => {
    noProc(answering)
    expect(await processStartedLater(process.pid)).toBe("ps:Sat Nov 21 22:00:00 2026")
    expect(await sharedAppServerLater(process.pid)).toBe(true)
  }, 60_000)

  test("reads as unknown, never a rejection, when ps is missing (ENOENT)", async () => {
    noProc(join(root, "no-such-ps"))
    expect(await processStartedLater(process.pid)).toBeUndefined()
    expect(await sharedAppServerLater(process.pid)).toBe(false)
  }, 60_000)

  for (const code of ["ENOENT", "EAGAIN", "EMFILE"]) {
    test(`reads as unknown, never a rejection, when the spawn fails with ${code}`, async () => {
      noProc(answering)
      processReads.spawn = failingSpawn(code)
      expect(await processStartedLater(process.pid)).toBeUndefined()
      expect(await sharedAppServerLater(process.pid)).toBe(false)
    }, 60_000)
  }

  test("in the owner probe, a start not read says nothing, is not kept, and is read again", async () => {
    noProc(answering)
    processReads.spawn = failingSpawn("EAGAIN")
    const entry = { pid: process.pid, seq: 1, started: "ps:another start" }
    // The read fails: the start says nothing either way, so the server is alive.
    expect(serverAlive(entry)).toBe(true)
    await Bun.sleep(0)
    // With ps working again, the probe reads anew rather than keeping the failure, and the start
    // it reads is not the entry's: the pid was used again.
    processReads.spawn = saved.spawn
    while (serverAlive(entry)) await Bun.sleep(20)
    expect(serverAlive(entry)).toBe(false)
  }, 60_000)
})

describe("the SessionStart hook with no /proc and a ps that never answers", () => {
  test("gives ps only what leaves its later steps their time, skips the token command by name, and records nothing", async () => {
    const home = join(root, "hook-home")
    const data = join(root, "hook-data")
    const marker = join(root, "token-command-ran")
    const config = join(root, "envoy.json")
    writeFileSync(
      config,
      JSON.stringify({
        dispatch: {
          enabled: true,
          serverUrl: "http://127.0.0.1:9",
          token: `!touch '${marker}'; sleep 30; printf token`,
        },
      }),
    )
    const preload = join(root, "no-proc-preload.ts")
    writeFileSync(
      preload,
      `import { processReads } from ${JSON.stringify(join(packageRoot, "src", "codex", "codex-session-record.ts"))}\n` +
        `processReads.hasProc = false\nprocessReads.proc = ${JSON.stringify(join(root, "no-proc"))}\n` +
        `processReads.ps = ${JSON.stringify(hookHanging)}\n`,
    )
    const hook = Bun.spawn(
      ["bun", "--preload", preload, join(packageRoot, "hooks", "codex-hook.ts"), "session-start"],
      {
        env: {
          CODEX_HOME: join(home, ".codex"),
          ENVOY_CONFIG_FILE: config,
          HOME: home,
          PATH: process.env["PATH"] ?? "",
          PLUGIN_DATA: data,
        },
        stdin: new TextEncoder().encode(
          JSON.stringify({
            cwd: root,
            session_id: "s-budget",
            source: "startup",
            transcript_path: join(root, "t.jsonl"),
          }),
        ),
        stderr: "pipe",
        stdout: "pipe",
      },
    )
    const [exitCode, stdout, stderr] = await Promise.all([
      hook.exited,
      new Response(hook.stdout).text(),
      new Response(hook.stderr).text(),
    ])
    expect(exitCode).toBe(0)
    // The first ps got what was left after the steps that follow (the writes and the 3 s asks
    // read), well inside Codex's 20 s, or none when Bun's own start took that much; the second got
    // none. Either way the reason is named, and ps ran at most once.
    const waited = /\((ps did not answer within (\d+) ms|no time was left to run ps)\)/.exec(stderr)
    expect(waited).not.toBeNull()
    expect(Number(waited?.[2] ?? 0)).toBeLessThanOrEqual(SESSION_START_HOOK_TIMEOUT_MS - 4_500)
    expect(stderr).toContain("this session was not recorded")
    expect(existsSync(codexSessionRecordFile(data, process.pid))).toBe(false)
    // With less left than the command's 10 s, the token command does not run, and the hook says so.
    expect(stdout).toContain(
      "Dispatch authored-ask summary unavailable (too little of this hook's time was left to run a command)",
    )
    expect(existsSync(marker)).toBe(false)
    const calls = existsSync(`${hookHanging}.calls`)
      ? readFileSync(`${hookHanging}.calls`, "utf8").trim().split("\n").length
      : 0
    expect(calls).toBeLessThanOrEqual(1)
  }, 60_000)
})
