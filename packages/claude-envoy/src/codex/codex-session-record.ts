import { spawnSync } from "node:child_process"
import { readFileSync, readlinkSync, unwatchFile, watchFile } from "node:fs"
import { mkdir, readdir, readFile, rename, rm, utimes, writeFile } from "node:fs/promises"
import { homedir } from "node:os"
import { basename, dirname, join } from "node:path"
import { hasErrnoCode } from "@legion/envoy-client/errors"
import { z } from "zod"
import { processExists } from "../session-identity"
import { pruneStateDirectory, writeAtomicStateFile } from "../session-state"
import { threadIdShape } from "./codex-thread-id"

/**
 * How the Codex plugin's MCP server learns which session it serves.
 *
 * The MCP server and the hooks run as children of the Codex process, but only hooks know the id
 * before a thread makes a tool call. SessionStart writes a shared thread's record under the
 * process id and thread id. In a process with one session, it writes one record that moves to
 * the next thread on /new.
 */
export const CodexSessionRecord = z.object({
  session_id: z.string().min(1),
  transcript_path: z.string().nullable(),
  codex_home: z.string().min(1),
  cwd: z.string().min(1),
  source: z.string().nullable(),
  /** One load of a shared thread. An ended marker from a previous load cannot stop this one. */
  generation: z.string().optional(),
  /** The `codex` binary the session runs (`/proc/<pid>/exe`), or null where it cannot be read. */
  codex_executable: z.string().nullable(),
  /** When the `codex` process started (`processStarted`: `/proc/<pid>/stat`, or `ps` where there
   *  is no /proc). A record whose start is not the server's parent's was written by an earlier
   *  process that had the same id, and names nothing the server runs under; the hook writes no
   *  record where the start cannot be read. Null only in a record of an earlier build. */
  codex_started: z.string().nullable().default(null),
})
export type CodexSessionRecord = z.infer<typeof CodexSessionRecord>

/** The file the record for the `codex` process `pid` lives in. */
export function codexSessionRecordFile(stateDirectory: string, pid: number): string {
  return join(stateDirectory, "codex-sessions", `${pid}.json`)
}

function sharedCodexRecordsDirectory(stateDirectory: string, pid: number): string {
  return join(stateDirectory, "codex-sessions", String(pid))
}

export function sharedCodexRecordFile(stateDirectory: string, pid: number, thread: string): string {
  if (!threadIdShape(thread)) throw new Error("Codex supplied an invalid thread id")
  return join(sharedCodexRecordsDirectory(stateDirectory, pid), `${thread}.json`)
}

export async function writeSharedCodexRecord(
  file: string,
  record: CodexSessionRecord & { generation: string },
): Promise<void> {
  // Publish the new generation first so a scan cannot wake the previous load on resume.
  await writeCodexSessionRecord(file, record)
  await rm(`${file}.ended`, { force: true })
}

/** The shared marker belongs to one load, even if a resume crosses a scan of the records. */
export async function markSharedCodexThreadEnded(file: string, generation: string): Promise<void> {
  await writeAtomicStateFile(`${file}.ended`, generation)
}

function sharedCodexEndedGeneration(file: string): Promise<string | undefined> {
  return readFile(`${file}.ended`, "utf8").catch((error: unknown) => {
    if (!hasErrnoCode(error, "ENOENT")) throw error
    return undefined
  })
}

export async function sharedCodexRecordActive(file: string, generation: string): Promise<boolean> {
  const ended = await sharedCodexEndedGeneration(file)
  return ended !== generation && (await readCodexSessionRecord(file))?.generation === generation
}

/** Record the instance before its first registry write. A successor must be able to remove the
 *  old route after an unload, even if Codex stopped the owner before it could deregister it. */
export async function recordSharedCodexRegistration(
  threadFile: string,
  generation: string,
  clientInstanceID: string,
): Promise<void> {
  await writeAtomicStateFile(`${threadFile}.${clientInstanceID}.registration`, generation)
}

interface SharedRegistration {
  readonly record: CodexSessionRecord
  readonly clientInstanceID: string
  readonly file: string
}

export interface SharedCodexState {
  readonly live: Map<string, CodexSessionRecord & { generation: string }>
  readonly endedRegistrations: readonly SharedRegistration[]
}

/** Only this app server's records count. Each instance has its own file so a failed replacement
 *  never overwrites the id of an earlier instance that still holds the listener route. */
export async function readSharedCodexState(
  stateDirectory: string,
  pid: number,
  started: string,
): Promise<SharedCodexState> {
  const directory = sharedCodexRecordsDirectory(stateDirectory, pid)
  const names = await readdir(directory).catch((error: unknown) => {
    if (!hasErrnoCode(error, "ENOENT")) throw error
    return []
  })
  const live: SharedCodexState["live"] = new Map()
  const ended = new Map<string, CodexSessionRecord>()
  await Promise.all(
    names.map(async (name) => {
      const thread = name.slice(0, -".json".length)
      if (!name.endsWith(".json") || !threadIdShape(thread)) return
      const file = join(directory, name)
      const record = await readCodexSessionRecord(file)
      const generation = record?.generation
      if (
        record?.session_id !== thread ||
        record.codex_started !== started ||
        generation === undefined
      )
        return
      if ((await sharedCodexEndedGeneration(file)) === generation) ended.set(thread, record)
      else live.set(thread, { ...record, generation })
    }),
  )
  const endedRegistrations = names.flatMap((name) => {
    const match = /^([0-9A-Za-z-]+)\.json\.([0-9a-f]{32})\.registration$/.exec(name)
    const thread = match?.[1]
    const clientInstanceID = match?.[2]
    const record = thread === undefined ? undefined : ended.get(thread)
    return record === undefined || clientInstanceID === undefined
      ? []
      : [{ record, clientInstanceID, file: join(directory, name) }]
  })
  return { live, endedRegistrations }
}

/**
 * The marker the `SessionEnd` hook leaves when session `sessionId` of `codex` process `pid` ended:
 * a file of its own beside the record, so the hook never rewrites a record a `SessionStart` may be
 * writing at that moment. A `SessionStart` that names the session again (`/resume`) removes it.
 */
export function codexEndedFile(stateDirectory: string, pid: number, sessionId: string): string {
  return join(stateDirectory, "codex-sessions", `${pid}.${encodeURIComponent(sessionId)}.ended`)
}

/**
 * Leaves session `sessionId`'s ended marker, then touches the record's times, unchanged: a server
 * watches the record file (`fs.watchFile`, which polls its stat), so the touch is what makes the
 * owning server read the marker, and so stop delivering into the ended thread, as soon as it is
 * there rather than at the next `SessionStart`.
 */
export async function markSessionEnded(
  stateDirectory: string,
  pid: number,
  sessionId: string,
): Promise<void> {
  const marker = codexEndedFile(stateDirectory, pid, sessionId)
  await mkdir(dirname(marker), { mode: 0o700, recursive: true })
  await writeFile(marker, "", { mode: 0o600 })
  const now = new Date()
  await utimes(codexSessionRecordFile(stateDirectory, pid), now, now)
}

/**
 * Calls `changed` with the record and whether its session has ended each time the record file's stat
 * changes (a new record, or `markSessionEnded`'s touch), and once at once: a marker left before the
 * watch began, when nothing would move the stat again, is read all the same. Answers the stop.
 */
export function watchRecord(
  file: string,
  pid: number,
  ended: (sessionId: string) => Promise<boolean>,
  changed: (record: CodexSessionRecord, ended: boolean) => void,
  intervalMs: number,
): () => void {
  const look = (): void => {
    void readCodexSessionRecord(file).then(async (record) => {
      if (record === undefined || !recordOfProcess(record, pid)) return
      changed(record, await ended(record.session_id))
    })
  }
  watchFile(file, { interval: intervalMs }, look)
  look()
  return () => unwatchFile(file, look)
}

/** Whether the marker `codexEndedFile` names is there. */
export async function sessionEnded(file: string): Promise<boolean> {
  try {
    await readFile(file)
    return true
  } catch {
    return false
  }
}

/** Writes the record whole, through a temporary file renamed into place. */
export async function writeCodexSessionRecord(
  file: string,
  value: CodexSessionRecord,
): Promise<void> {
  await mkdir(dirname(file), { recursive: true, mode: 0o700 })
  const temporary = `${file}.${process.pid}.tmp`
  await writeFile(temporary, JSON.stringify(value), { mode: 0o600 })
  await rename(temporary, file)
}

/** The record in `file`, or undefined when there is none or it does not parse. */
export async function readCodexSessionRecord(
  file: string,
): Promise<CodexSessionRecord | undefined> {
  try {
    const parsed = CodexSessionRecord.safeParse(JSON.parse(await readFile(file, "utf8")))
    return parsed.success ? parsed.data : undefined
  } catch {
    return undefined
  }
}

/** Codex's home as the person runs it: `CODEX_HOME`, else `~/.codex`, Codex's own default. */
export function codexHome(env: Readonly<Record<string, string | undefined>>): string {
  const set = env["CODEX_HOME"]
  return set !== undefined && set !== "" ? set : join(env["HOME"] || homedir(), ".codex")
}

/**
 * The plugin's state directory, worked out from where Codex installed the plugin. A hook is handed
 * it as `PLUGIN_DATA`; the MCP server is not, but runs in the plugin's own directory, which Codex
 * installs at `<Codex home>/plugins/cache/<marketplace>/<plugin>/<version>` and whose state
 * directory is `<Codex home>/plugins/data/<plugin>-<marketplace>` (measured on 0.154.0). Answers
 * undefined for a directory of any other shape.
 */
export function codexPluginDataDirectory(pluginRoot: string): string | undefined {
  const version = pluginRoot
  const plugin = dirname(version)
  const marketplace = dirname(plugin)
  const cache = dirname(marketplace)
  const plugins = dirname(cache)
  if (basename(cache) !== "cache" || basename(plugins) !== "plugins") return undefined
  return join(plugins, "data", `${basename(plugin)}-${basename(marketplace)}`)
}

/** How many parents `codexProcessId` follows before it gives up. */
const MAX_ANCESTORS = 16

/**
 * The `codex` process this process belongs to: the nearest ancestor Linux names `codex`, else the
 * parent. Codex runs a hook's command through a shell, which runs a single command in its own
 * place, so the parent is normally `codex` itself; the walk covers a shell that does not.
 */
export function codexProcessId(
  readComm: (pid: number) => string | undefined = procComm,
  readParent: (pid: number) => number | undefined = procParent,
): number {
  let pid: number | undefined = process.ppid
  for (let step = 0; pid !== undefined && pid > 1 && step < MAX_ANCESTORS; step++) {
    if (readComm(pid) === "codex") return pid
    pid = readParent(pid)
  }
  return process.ppid
}

function procComm(pid: number): string | undefined {
  try {
    return readFileSync(`/proc/${pid}/comm`, "utf8").trim()
  } catch {
    return undefined
  }
}

function procParent(pid: number): number | undefined {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8")
    const parent = Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1])
    return Number.isInteger(parent) ? parent : undefined
  } catch {
    return undefined
  }
}

/** How the hook and the server run `ps`: in one fixed environment, so the start time each prints
 *  is the same text (macOS's `ps` formats it by the locale and the time zone), and briefly. */
const PS_OPTIONS = {
  encoding: "utf8",
  env: { LC_ALL: "C", PATH: process.env["PATH"] ?? "/usr/bin:/bin", TZ: "UTC" },
  timeout: 1_000,
} as const

/** When process `pid` started by `/proc/<pid>/stat` alone: its 22nd field (clock ticks since
 *  boot); undefined where that file cannot be read (no /proc, or no such process). */
export function procStarted(pid: number): string | undefined {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8")
    return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19]
  } catch {
    return undefined
  }
}

/** When process `pid` started: `procStarted`, or where that cannot be read (macOS has no /proc),
 *  `ps`'s start time to the second, prefixed `ps:`; undefined where neither can be read. Two
 *  processes that had one id never share it. */
export function processStarted(pid: number): string | undefined {
  const started = procStarted(pid)
  if (started !== undefined) return started
  // No /proc (macOS): `ps` names the start to the second, which a reused pid does not repeat
  // within a second of the process it replaced exiting, and the hook and the server read it the
  // same way.
  const ps = spawnSync("ps", ["-o", "lstart=", "-p", String(pid)], PS_OPTIONS)
  const fromPs = ps.status === 0 ? ps.stdout.trim() : ""
  return fromPs === "" ? undefined : `ps:${fromPs}`
}

/** Whether `record` belongs to the `codex` process `pid` now runs, rather than to an earlier
 *  process that had the same id. A record or a process whose start cannot be read is not taken:
 *  records outlive their process, and a new `codex` that reused the pid would otherwise bind to the
 *  old one's session and register it until its first prompt. */
export function recordOfProcess(record: CodexSessionRecord, pid: number): boolean {
  const started = processStarted(pid)
  return record.codex_started !== null && started !== undefined && record.codex_started === started
}

/**
 * Whether the `codex` process `pid` is Codex's shared app server (`codex app-server`), which runs
 * several sessions in one process: a later plain `codex` reuses one `codex agents` started, and
 * Codex 0.170.0's terminal runs every session in a background `codex app-server --managed-daemon`
 * (measured on 0.170.0). Each of those threads has its own session and record.
 */
export function sharedAppServer(pid: number): boolean {
  let argv: string[]
  try {
    argv = readFileSync(`/proc/${pid}/cmdline`, "utf8").split("\0")
  } catch {
    const ps = spawnSync("ps", ["-o", "args=", "-p", String(pid)], PS_OPTIONS)
    if (ps.status !== 0) return false
    argv = ps.stdout.trim().split(/\s+/)
  }
  return SHARED_SUBCOMMANDS.has(codexSubcommand(argv.slice(1)) ?? "")
}

/** The subcommands that run several sessions in one process: `app-server` (the daemon `codex agents`
 *  and 0.170.0's terminal start) and `agents` (`codex agents --no-daemon` runs the app server inside
 *  its own process, 0.170.0). */
const SHARED_SUBCOMMANDS = new Set(["agents", "app-server"])

/** The options of `codex` that take a value as the next argument (`codex --help`, 0.154.0). */
const VALUE_OPTIONS = new Set([
  "-c",
  "--config",
  "--enable",
  "--disable",
  "--remote",
  "--remote-auth-token-env",
  "-i",
  "--image",
  "-m",
  "--model",
  "--local-provider",
  "-p",
  "--profile",
  "-s",
  "--sandbox",
  "-a",
  "--ask-for-approval",
  "-C",
  "--cd",
  "--add-dir",
])

/** The subcommand of a `codex` command line (its arguments after the binary): the first argument
 *  that is neither an option nor an option's value, or undefined when there is none. */
export function codexSubcommand(args: readonly string[]): string | undefined {
  for (let index = 0; index < args.length; index++) {
    const arg = args[index] ?? ""
    if (arg === "--") return args[index + 1]
    if (!arg.startsWith("-")) return arg
    if (VALUE_OPTIONS.has(arg)) index++
  }
  return undefined
}

/** Removes what `codex` processes that have exited left (their records, ended markers and server
 *  entries), so a later process that reuses a pid never finds any of it. */
export async function pruneCodexSessionRecords(stateDirectory: string): Promise<void> {
  // Records and ended markers, then the server entries (`./codex-owner`), of exited codex pids.
  await pruneStateDirectory(join(stateDirectory, "codex-sessions"), (file) => {
    const match = /^(\d+)(?:\.(?:json|.+\.ended))?$/.exec(basename(file))
    return match !== null && !processExists(Number(match[1]))
  })
  await pruneStateDirectory(join(stateDirectory, "codex-servers"), (file) => {
    const name = basename(file)
    return /^\d+$/.test(name) && !processExists(Number(name))
  })
}

/** The binary the `codex` process `pid` runs, so the plugin calls the same Codex the session
 *  runs; undefined where the process cannot be read. */
export function codexExecutable(pid: number): string | undefined {
  try {
    return readlinkSync(`/proc/${pid}/exe`)
  } catch {
    return undefined
  }
}
