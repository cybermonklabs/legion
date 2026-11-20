import { spawnSync } from "node:child_process"
import { readFileSync, readlinkSync, unwatchFile, watchFile } from "node:fs"
import { mkdir, readFile, rename, utimes, writeFile } from "node:fs/promises"
import { homedir } from "node:os"
import { basename, dirname, join } from "node:path"
import { z } from "zod"
import { processExists } from "../process-exists"
import { pruneStateDirectory } from "../session-state"

/**
 * How the Codex plugin's MCP server learns which session it serves.
 *
 * Codex starts the plugin's MCP server as a child of the `codex` process with a stripped
 * environment and no session id, and runs the plugin's hooks as children of that same process
 * with the session's id in their input. So the `SessionStart` hook writes this record under the
 * `codex` process's id, and the server, whose parent that process is, reads it. `SessionStart`
 * runs at the session's first prompt and again after `/new`, with the new session's id. Codex
 * starts one server per thread, so a process can have several; the one that owns the process's
 * session (`./codex-owner`) follows the record to each session the process runs. Measured on Codex
 * CLI 0.154.0.
 */
export const CodexSessionRecord = z.object({
  session_id: z.string().min(1),
  transcript_path: z.string().nullable(),
  codex_home: z.string().min(1),
  cwd: z.string().min(1),
  source: z.string().nullable(),
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

/** When process `pid` started: the 22nd field of `/proc/<pid>/stat` (clock ticks since boot), or
 *  where there is no /proc, `ps`'s start time to the second, prefixed `ps:`; undefined where
 *  neither can be read. Two processes that had one id never share it. */
export function processStarted(pid: number): string | undefined {
  try {
    const stat = readFileSync(`/proc/${pid}/stat`, "utf8")
    return stat.slice(stat.lastIndexOf(")") + 2).split(" ")[19]
  } catch {
    // No /proc (macOS): `ps` names the start to the second, which a reused pid does not repeat
    // within a second of the process it replaced exiting, and the hook and the server read it
    // the same way.
    const ps = spawnSync("ps", ["-o", "lstart=", "-p", String(pid)], PS_OPTIONS)
    const started = ps.status === 0 ? ps.stdout.trim() : ""
    return started === "" ? undefined : `ps:${started}`
  }
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
 * (measured on 0.170.0). This plugin keys a session by its process, so there two sessions would
 * share one record and one hand-off, and one would take the other's identity, so it registers none
 * of them with Envoy; `dispatch` still acts as the thread that calls it.
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

/** What a session of the shared app server is told about Envoy and Dispatch. */
export const SHARED_APP_SERVER_NOTE =
  "Codex runs this session in its shared app server (`codex app-server`), which runs several sessions in one process, so the Envoy plugin does not register it with Envoy: it receives no Envoy messages and does not show on Dispatch's Agents page. Its `dispatch` tool works, acting as this session."

/** Removes what `codex` processes that have exited left (their records, ended markers and server
 *  entries), so a later process that reuses a pid never finds any of it. */
export async function pruneCodexSessionRecords(stateDirectory: string): Promise<void> {
  // Records and ended markers, then the server entries (`./codex-owner`), of exited codex pids.
  await pruneStateDirectory(join(stateDirectory, "codex-sessions"), (file) => {
    const match = /^(\d+)\.(?:json|.+\.ended)$/.exec(basename(file))
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
