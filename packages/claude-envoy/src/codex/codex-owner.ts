import { link, mkdir, readdir, readFile, rm, stat, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { processStarted } from "./codex-session-record"

/**
 * Which of a `codex` process's plugin servers owns its Envoy session.
 *
 * Codex starts one MCP server per thread (measured on 0.154.0: `/new` starts a second server under
 * the same `codex` process while the first runs on until Codex unloads its thread, and `/side` and
 * `/btw` start one each for their fork). The record of the session the process runs is keyed by the
 * process, so every one of those servers would bind it and register the same session.
 *
 * So each server publishes an entry, `codex-servers/<codex pid>/<seq>.json`, taking the next free
 * sequence number by linking a fully written temporary file into place (`link` fails when the
 * name exists), so two live entries never hold one number and the order is the order they
 * published in, whatever the clock says. The owner is the live entry with the lowest number. No
 * server removes another's entry, so a name a server judged dead and a name a later server took
 * are never confused; the `SessionStart` hook removes a dead codex process's entries. Every server
 * reads the same entries and reaches the same answer. The others stand by, serving their thread's
 * `dispatch` calls, until every server that published before them is gone.
 */
export interface CodexServerEntry {
  /** The publication number: lower owns. */
  readonly seq: number
  /** The server's pid. */
  readonly pid: number
  /** When it started (`processStarted`), so a pid used again names nothing; null where it could
   *  not be read, and then only `ESRCH` says the server is gone. */
  readonly started: string | null
}

/** The directory of `codex` process `codexPid`'s servers, under the plugin's state directory. */
export function codexServersDirectory(stateDirectory: string, codexPid: number): string {
  return join(stateDirectory, "codex-servers", String(codexPid))
}

const ENTRY_NAME = /^(\d+)\.json$/

function entryName(entry: { readonly seq: number }): string {
  return `${entry.seq}.json`
}

/**
 * Publishes an entry for server `pid`, at the lowest number above every entry there, and answers
 * it. The entry is written whole to a temporary file and linked into place, and `link` fails when
 * the name exists, so a server that loses a number to another takes the next one, and a reader
 * never sees a half-written entry.
 */
export async function registerServer(
  directory: string,
  pid: number,
  started: string | null,
): Promise<CodexServerEntry> {
  await mkdir(directory, { mode: 0o700, recursive: true })
  const temporary = join(directory, `.${pid}.${process.hrtime.bigint()}.tmp`)
  await writeFile(temporary, JSON.stringify({ pid, started }), { mode: 0o600 })
  try {
    let seq = Math.max(0, ...(await entryNames(directory)).map(({ seq: taken }) => taken)) + 1
    for (;;) {
      try {
        await place(temporary, join(directory, entryName({ seq })), pid, started)
        return { pid, seq, started }
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "EEXIST") throw error
        seq++
      }
    }
  } finally {
    await rm(temporary, { force: true })
  }
}

/** Where `link` is refused (a file system without hard links), whether to fall back to an exclusive
 *  create; set once seen, so the fallback is not tried before every link. */
let linkRefused = false

/**
 * Puts the entry at `target`, failing with `EEXIST` when the name is taken: `link` of the fully
 * written `temporary`, or, on a file system that refuses hard links (`EPERM`, `ENOTSUP`,
 * `EXDEV`), an exclusive create that writes the whole entry in one small write.
 */
async function place(
  temporary: string,
  target: string,
  pid: number,
  started: string | null,
): Promise<void> {
  if (!linkRefused) {
    try {
      await link(temporary, target)
      return
    } catch (error) {
      const code = (error as NodeJS.ErrnoException).code
      if (code !== "EPERM" && code !== "ENOTSUP" && code !== "EXDEV") throw error
      linkRefused = true
    }
  }
  await writeFile(target, JSON.stringify({ pid, started }), { flag: "wx", mode: 0o600 })
}

/** Removes `self`'s entry. */
export async function unregisterServer(directory: string, self: CodexServerEntry): Promise<void> {
  await rm(join(directory, entryName(self)), { force: true })
}

/** How a liveness check reads a process: `kill(pid, 0)` and its start. */
export interface ProcessProbe {
  readonly signal: (pid: number) => void
  readonly started: (pid: number) => string | undefined
}

/** The start each live pid was last read at, so `ps` (off Linux) runs once per pid, not every
 *  poll. A pid's start changes only when the pid is used again, which `ESRCH` comes before. */
const startedOf = new Map<number, string | undefined>()

const SYSTEM_PROBE: ProcessProbe = {
  signal: (pid) => process.kill(pid, 0),
  started: (pid) => {
    if (!startedOf.has(pid)) startedOf.set(pid, processStarted(pid))
    return startedOf.get(pid)
  },
}

/** Whether a liveness check found the pid gone (`ESRCH`), and not merely unreadable. */
function gone(pid: number, probe: ProcessProbe): boolean {
  try {
    probe.signal(pid)
    return false
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") return false // EPERM: a process runs.
    startedOf.delete(pid)
    return true
  }
}

/**
 * Whether the server `entry` names still runs. It is gone only when its pid answers `ESRCH`, or
 * when both starts can be read and differ (the pid was used again); a start that cannot be read
 * now, as when `ps` fails, says nothing either way, so a live owner is never judged gone for it.
 */
export function serverAlive(entry: CodexServerEntry, probe: ProcessProbe = SYSTEM_PROBE): boolean {
  if (gone(entry.pid, probe)) return false
  const now = probe.started(entry.pid)
  return entry.started === null || now === undefined || now === entry.started
}

async function entryNames(directory: string): Promise<{ seq: number; name: string }[]> {
  let names: string[]
  try {
    names = await readdir(directory)
  } catch {
    return []
  }
  return names.flatMap((name) => {
    const match = ENTRY_NAME.exec(name)
    return match === null ? [] : [{ name, seq: Number(match[1]) }]
  })
}

/** How long a numbered entry may stay unreadable before it counts as dead: one small write takes
 *  far less, so an entry still empty after this was left by a server killed in that write. */
const UNREADABLE_ENTRY_MS = 5_000

/** The entries `directory` holds, or undefined when one of them could not be read yet: a numbered
 *  file that is still empty or half-written (placed by an exclusive create where the file system
 *  has no hard links), which no look may decide past. Such a file older than
 *  `UNREADABLE_ENTRY_MS` counts as dead, so a server killed in that write blocks no owner for
 *  long. */
async function readEntries(
  directory: string,
  now: () => number = Date.now,
): Promise<CodexServerEntry[] | undefined> {
  const entries: CodexServerEntry[] = []
  for (const { name, seq } of await entryNames(directory)) {
    let text: string
    try {
      text = await readFile(join(directory, name), "utf8")
    } catch {
      continue // Removed meanwhile.
    }
    let parsed: { pid?: unknown; started?: unknown } | undefined
    try {
      parsed = JSON.parse(text) as { pid?: unknown; started?: unknown }
    } catch {
      parsed = undefined
    }
    if (typeof parsed?.pid !== "number") {
      const written = await stat(join(directory, name)).then(
        (info) => info.mtimeMs,
        () => undefined,
      )
      if (written === undefined || now() - written > UNREADABLE_ENTRY_MS) continue
      return undefined
    }
    entries.push({
      pid: parsed.pid,
      seq,
      started: typeof parsed.started === "string" ? parsed.started : null,
    })
  }
  return entries
}

/** The live server that owns the session: the live entry with the lowest number; undefined when
 *  there is none, or when an entry could not be read yet and no answer can be given. */
export async function currentOwner(
  directory: string,
  probe: ProcessProbe = SYSTEM_PROBE,
): Promise<CodexServerEntry | undefined> {
  const entries = await readEntries(directory)
  if (entries === undefined) return undefined
  const live = entries.filter((entry) => serverAlive(entry, probe))
  live.sort((a, b) => a.seq - b.seq)
  return live[0]
}

/**
 * Resolves with `self`'s entry once it owns the session, looking every `pollMs`, or undefined when
 * `signal` aborts. It publishes the entry first, and tells `published` each entry it publishes, so
 * a server that stops while it stands by removes its own. A look that finds the entry gone (a
 * failed write, or a directory someone removed) publishes it again, at the back of the order,
 * after removing the one it had. The server owns when the owner is any entry of its own pid.
 */
export function waitForOwnership(
  directory: string,
  pid: number,
  started: string | null,
  signal: AbortSignal,
  published: (entry: CodexServerEntry) => void = () => undefined,
  pollMs = 1_000,
  probe: ProcessProbe = SYSTEM_PROBE,
): Promise<CodexServerEntry | undefined> {
  return new Promise((done) => {
    let self: CodexServerEntry | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    const look = async (): Promise<void> => {
      if (signal.aborted) return done(undefined)
      const entries = await readEntries(directory)
      if (
        entries !== undefined &&
        (self === undefined || !entries.some((entry) => entry.seq === self?.seq))
      ) {
        if (self !== undefined) await unregisterServer(directory, self)
        self = await registerServer(directory, pid, started)
        published(self)
      }
      const owner = await currentOwner(directory, probe)
      if (owner?.pid === pid) done(self)
      else timer = setTimeout(() => void look().catch(retry), pollMs)
    }
    const retry = (): void => {
      timer = setTimeout(() => void look().catch(retry), pollMs)
    }
    signal.addEventListener("abort", () => {
      if (timer !== undefined) clearTimeout(timer)
      done(undefined)
    })
    void look().catch(retry)
  })
}

/** Whether `self` still owns the session. The owner re-checks every few seconds and warns when it
 *  finds another owner, which the order of publication rules out while every entry is readable. */
export async function stillOwner(
  directory: string,
  self: CodexServerEntry,
  probe: ProcessProbe = SYSTEM_PROBE,
): Promise<boolean> {
  const entries = await readEntries(directory)
  if (entries === undefined) return true
  const owner = await currentOwner(directory, probe)
  return owner === undefined || owner.pid === self.pid
}
