import { access, link, mkdir, readdir, readFile, rm, stat, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { hasErrnoCode, messageFor } from "@legion/envoy-client/errors"
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
 * meanwhile, below an entry published after its read, even a number freed while another server
 * was looking. Its temporary file is there all that time, and while a server that still runs has
 * one there, no owner is decided unless every number below it is taken by an entry still there,
 * unchanged, after the look found its server gone, which no server can remove any more
 * (`currentOwner`). So, while every entry is readable, no server owns while another does. No
 * server removes another's entry (`unregisterServer`), so a name a server judged dead and a name a
 * later server took are never confused; the `SessionStart` hook removes a dead codex process's
 * entries. Every server reads the same entries and reaches the same answer. The others stand by,
 * serving their thread's `dispatch` calls, until every server ahead of them is gone.
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
 * number read off nothing. `list` reads the directory; production passes nothing, and a test wraps
 * the read to hold a server right after it, as one the scheduler stalls there.
 */
export async function registerServer(
  directory: string,
  pid: number,
  started: string | null,
  list: (directory: string) => Promise<string[]> = (path) => readdir(path),
): Promise<CodexServerEntry> {
  await mkdir(directory, { mode: 0o700, recursive: true })
  const temporary = join(directory, `.${pid}.${process.hrtime.bigint()}.tmp`)
  await writeFile(temporary, JSON.stringify({ pid, started }), { mode: 0o600 })
  try {
    let seq = Math.max(0, ...numbered(await list(directory)).map(({ seq: taken }) => taken)) + 1
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

/**
 * Removes `self`'s entry, when the file at its number still names `self`. A server whose entry was
 * removed, and whose number another server then took, so leaves that server's entry alone.
 */
export async function unregisterServer(directory: string, self: CodexServerEntry): Promise<void> {
  const file = join(directory, entryName(self))
  const named = await readFile(file, "utf8").then(serverOf, () => undefined)
  if (named?.pid === self.pid && named.started === self.started) {
    await rm(file, { force: true })
    await rm(stoppingFile(directory, self), { force: true })
  }
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
 * A directory that is not there is an empty look; a listing that fails for any other reason
 * rejects, since a look read off nothing would have a server publish again, or decide, on entries
 * it never saw.
 *
 * `publishing` says a server that still runs is publishing: its whole temporary file is in the
 * listing, or was and is gone now, its entry placed after the listing. That server has read the
 * directory and not yet placed its entry, and the number it read may have been freed since. A
 * temporary file still empty or half-written says nothing: its server reads the directory only
 * once the file is whole, so it reads every entry placed by then and takes a number above them
 * all. `held` maps each number a file held in this look to the text read from it.
 */
async function readEntries(
  directory: string,
  probe: ProcessProbe,
  now: () => number = Date.now,
): Promise<
  | { entries: CodexServerEntry[]; publishing: boolean; held: ReadonlyMap<number, string> }
  | undefined
> {
  let names: string[]
  try {
    names = await readdir(directory)
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return { entries: [], publishing: false, held: new Map() }
    throw error
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
  const held = new Map<number, string>()
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
      held.set(seq, text)
      continue
    }
    held.set(seq, text)
    entries.push({ ...server, seq })
  }
  return { entries, publishing, held }
}

/**
 * The live server that owns the session: the live entry with the lowest number; undefined when
 * there is none, or when an entry could not be read yet, and no answer can be given. It rejects
 * when the directory cannot be listed (`readEntries`).
 *
 * While a server that still runs is publishing (`readEntries`), it may take any number below the
 * owner's that is free, so the answer is undefined unless every such number is taken by the file
 * this look read, still there unchanged once the look has found that file's server gone. A server
 * removes its own entry as it stops, so a file read before the look found its server gone may have
 * gone since; one still there after that is a dead server's, which nothing removes while this
 * codex process runs.
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
  if (owner === undefined || !seen.publishing) return owner
  for (let seq = 1; seq < owner.seq; seq++) {
    const text = seen.held.get(seq)
    if (text === undefined) return undefined // Free: the server publishing may take it.
    const now = await readFile(join(directory, entryName({ seq })), "utf8").catch(() => undefined)
    if (now !== text) return undefined // Freed, or taken, since this look read it.
  }
  return owner
}

/**
 * Resolves with `self`'s entry once it owns the session, looking every `pollMs`, or undefined when
 * `signal` aborts. It publishes the entry first, and tells `published` each entry it publishes, so
 * a server that stops while it stands by removes its own. A look that finds the entry gone (a
 * failed write, a directory someone removed, or another server's entry at its number) publishes it
 * again, at the back of the order, after removing the one it had if that is still its own. The
 * server owns when the owner is any entry of its own pid. A look that fails (a directory it cannot
 * list, an entry it cannot write) changes nothing and is tried again at the next look; `warn` is
 * told once when looks start to fail, and again only after one has succeeded.
 */
export function waitForOwnership(
  directory: string,
  pid: number,
  started: string | null,
  signal: AbortSignal,
  published: (entry: CodexServerEntry) => void = () => undefined,
  pollMs = 1_000,
  probe: ProcessProbe = SYSTEM_PROBE,
  warn: (text: string) => void = (text) => void process.stderr.write(`envoy-channel: ${text}\n`),
): Promise<CodexServerEntry | undefined> {
  return new Promise((done) => {
    let self: CodexServerEntry | undefined
    let timer: ReturnType<typeof setTimeout> | undefined
    /** Whether the last look failed, so looks that keep failing are told once. */
    let failing = false
    const look = async (): Promise<void> => {
      if (signal.aborted) return done(undefined)
      const seen = await readEntries(directory, probe)
      const had = self
      if (
        seen !== undefined &&
        !seen.entries.some(
          (entry) => entry.seq === had?.seq && entry.pid === pid && entry.started === started,
        )
      ) {
        // Cleared first, so a publish that fails leaves no number this server no longer holds.
        self = undefined
        if (had !== undefined) await unregisterServer(directory, had)
        self = await registerServer(directory, pid, started)
        published(self)
      }
      const owner = await currentOwner(directory, probe)
      failing = false
      if (owner?.pid === pid) done(self)
      else next()
    }
    const next = (): void => {
      timer = setTimeout(() => void look().catch(failed), pollMs)
    }
    const failed = (error: unknown): void => {
      if (!failing) {
        warn(
          `this plugin server could not read or publish its entry in ${directory}, so it stands by and tries again: ${messageFor(error)}`,
        )
      }
      failing = true
      next()
    }
    signal.addEventListener("abort", () => {
      if (timer !== undefined) clearTimeout(timer)
      done(undefined)
    })
    void look().catch(failed)
  })
}

function stoppingFile(directory: string, owner: CodexServerEntry): string {
  return join(directory, `${owner.seq}.${owner.pid}.stopping`)
}

/** Signal to hooks that an elected owner will stop scanning, before reading the final records. */
export async function markCodexServerStopping(
  directory: string,
  owner: CodexServerEntry,
): Promise<void> {
  await writeFile(stoppingFile(directory, owner), "", { mode: 0o600 })
}

/** An owner can remain elected while it releases the relay and its inbox. A hook that sees its
 *  stopping marker prepares a successor instead of depending on that owner's last scan. */
export async function acceptingCodexOwner(
  directory: string,
): Promise<CodexServerEntry | undefined> {
  const owner = await currentOwner(directory)
  if (owner === undefined) return undefined
  try {
    await access(stoppingFile(directory, owner))
    return undefined
  } catch (error) {
    if (!hasErrnoCode(error, "ENOENT")) throw error
    // The marker may have gone away with a departing owner. Its old entry is not accepting.
    const current = await currentOwner(directory)
    return current?.pid === owner.pid &&
      current.seq === owner.seq &&
      current.started === owner.started
      ? owner
      : undefined
  }
}

/** A live server beside the one leaving can take over its shared threads. */
export async function hasStandbyServer(
  directory: string,
  self: CodexServerEntry,
  probe: ProcessProbe = SYSTEM_PROBE,
): Promise<boolean> {
  const seen = await readEntries(directory, probe)
  return seen?.entries.some((entry) => entry.pid !== self.pid && serverAlive(entry, probe)) ?? false
}

/** Whether `self` still owns the session: true too when no owner can be decided now. The owner
 *  re-checks every few seconds and warns when it finds another owner, which the order of
 *  publication rules out while every entry is readable. It rejects when the directory cannot be
 *  listed. */
export async function stillOwner(
  directory: string,
  self: CodexServerEntry,
  probe: ProcessProbe = SYSTEM_PROBE,
): Promise<boolean> {
  const owner = await currentOwner(directory, probe)
  return owner === undefined || owner.pid === self.pid
}
