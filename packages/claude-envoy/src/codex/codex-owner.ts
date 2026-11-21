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
 * name exists), so two live entries never hold one number, whatever the clock says. The owner is
 * the live entry with the lowest number. A server reads the directory before it links, and a
 * server that stops removes its entry, so a server held between the two can take a number freed
 * meanwhile, below an entry published after its read; its temporary file is there all that time,
 * and while a server that still runs has one there, no owner is decided above a free number, so
 * the order a decision reads is final and no server owns while another does. No server removes
 * another's entry, so a name a server judged dead and a name a later server took are never
 * confused, and an entry below the owner's, which is dead, stays; the `SessionStart` hook removes
 * a dead codex process's entries. Every server reads the same entries and reaches the same
 * answer. The others stand by, serving their thread's `dispatch` calls, until every server ahead
 * of them is gone.
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

/** A server's temporary file (`registerServer`): `.<pid>.<n>.tmp`. */
const TEMPORARY_NAME = /^\.\d+\.\d+\.tmp$/

function entryName(entry: { readonly seq: number }): string {
  return `${entry.seq}.json`
}

/**
 * Publishes an entry for server `pid`, at the lowest free number above every entry there, and
 * answers it. The entry is written whole to a temporary file and linked into place, and `link`
 * fails when the name exists, so a server that loses a number to another takes the next one, and a
 * reader never sees a half-written entry. The temporary file is there from before the directory is
 * read until the entry is placed, so no owner is decided meanwhile above a number the server could
 * take (`currentOwner`). A listing that fails is no empty one: its failure stands, rather than a
 * number read off nothing. `beforePlace` runs between the read and the placing; nothing waits
 * there in production, and a test holds a server there to stand for one the scheduler stalls.
 */
export async function registerServer(
  directory: string,
  pid: number,
  started: string | null,
  beforePlace: () => Promise<void> = async () => undefined,
): Promise<CodexServerEntry> {
  await mkdir(directory, { mode: 0o700, recursive: true })
  const temporary = join(directory, `.${pid}.${process.hrtime.bigint()}.tmp`)
  await writeFile(temporary, JSON.stringify({ pid, started }), { mode: 0o600 })
  try {
    let seq = Math.max(0, ...numbered(await readdir(directory)).map(({ seq: taken }) => taken)) + 1
    await beforePlace()
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
export function serverAlive(
  entry: Pick<CodexServerEntry, "pid" | "started">,
  probe: ProcessProbe = SYSTEM_PROBE,
): boolean {
  if (gone(entry.pid, probe)) return false
  const now = probe.started(entry.pid)
  return entry.started === null || now === undefined || now === entry.started
}

/** The numbered entries among a listing's `names`. */
function numbered(names: readonly string[]): { seq: number; name: string }[] {
  return names.flatMap((name) => {
    const match = ENTRY_NAME.exec(name)
    return match === null ? [] : [{ name, seq: Number(match[1]) }]
  })
}

/** The server a file names, or undefined while the file is still empty or half-written. */
function serverOf(text: string): Pick<CodexServerEntry, "pid" | "started"> | undefined {
  let parsed: { pid?: unknown; started?: unknown } | undefined
  try {
    parsed = JSON.parse(text) as { pid?: unknown; started?: unknown }
  } catch {
    return undefined
  }
  if (typeof parsed?.pid !== "number") return undefined
  return { pid: parsed.pid, started: typeof parsed.started === "string" ? parsed.started : null }
}

/** How long a numbered entry may stay unreadable before it counts as dead: one small write takes
 *  far less, so an entry still empty after this was left by a server killed in that write. */
const UNREADABLE_ENTRY_MS = 5_000

/**
 * One look at `directory`, from one listing of it, or undefined when an entry could not be read
 * yet: a numbered file that is still empty or half-written (placed by an exclusive create where
 * the file system has no hard links), which no look may decide past. Such a file older than
 * `UNREADABLE_ENTRY_MS` counts as dead, so a server killed in that write blocks no owner for long.
 *
 * `publishing` says a server that still runs is publishing: its whole temporary file is in the
 * listing, or was and is gone now, its entry placed after the listing. That server has read the
 * directory and not yet placed its entry, and the number it read may have been freed since. A
 * temporary file still empty or half-written says nothing: its server reads the directory only
 * once the file is whole, so it reads every entry placed by then and takes a number above them
 * all. `lowestFree` is the lowest number no file held in this look.
 */
async function readEntries(
  directory: string,
  probe: ProcessProbe,
  now: () => number = Date.now,
): Promise<{ entries: CodexServerEntry[]; publishing: boolean; lowestFree: number } | undefined> {
  let names: string[]
  try {
    names = await readdir(directory)
  } catch {
    return { entries: [], publishing: false, lowestFree: 1 }
  }
  let publishing = false
  for (const name of names.filter((each) => TEMPORARY_NAME.test(each))) {
    let text: string
    try {
      text = await readFile(join(directory, name), "utf8")
    } catch {
      publishing = true // Its entry was placed after the listing, which does not hold it.
      break
    }
    const server = serverOf(text)
    if (server !== undefined && serverAlive(server, probe)) {
      publishing = true
      break
    }
  }
  const entries: CodexServerEntry[] = []
  const held = new Set<number>()
  for (const { name, seq } of numbered(names)) {
    let text: string
    try {
      text = await readFile(join(directory, name), "utf8")
    } catch {
      continue // Removed meanwhile.
    }
    const server = serverOf(text)
    if (server === undefined) {
      const written = await stat(join(directory, name)).then(
        (info) => info.mtimeMs,
        () => undefined,
      )
      if (written === undefined) continue // Removed meanwhile.
      if (now() - written <= UNREADABLE_ENTRY_MS) return undefined
      held.add(seq)
      continue
    }
    held.add(seq)
    entries.push({ ...server, seq })
  }
  let lowestFree = 1
  while (held.has(lowestFree)) lowestFree++
  return { entries, publishing, lowestFree }
}

/**
 * The live server that owns the session: the live entry with the lowest number; undefined when
 * there is none, or when an entry could not be read yet, and no answer can be given. Undefined too
 * while a server that still runs is publishing and a number below the owner's is free
 * (`readEntries`): that server may take it. Every entry below the owner's is dead and stays, so
 * with none free it can only take a number above the owner's.
 */
export async function currentOwner(
  directory: string,
  probe: ProcessProbe = SYSTEM_PROBE,
): Promise<CodexServerEntry | undefined> {
  const seen = await readEntries(directory, probe)
  if (seen === undefined) return undefined
  const live = seen.entries.filter((entry) => serverAlive(entry, probe))
  live.sort((a, b) => a.seq - b.seq)
  const owner = live[0]
  if (owner !== undefined && seen.publishing && seen.lowestFree < owner.seq) return undefined
  return owner
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
      const seen = await readEntries(directory, probe)
      if (
        seen !== undefined &&
        (self === undefined || !seen.entries.some((entry) => entry.seq === self?.seq))
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
  const entries = await readEntries(directory, probe)
  if (entries === undefined) return true
  const owner = await currentOwner(directory, probe)
  return owner === undefined || owner.pid === self.pid
}
