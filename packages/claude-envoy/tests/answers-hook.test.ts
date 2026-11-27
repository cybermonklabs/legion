import { afterAll, afterEach, beforeAll, test as bunTest, expect } from "bun:test"
import { AsyncLocalStorage } from "node:async_hooks"
import { existsSync, readFileSync } from "node:fs"
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import type { SessionAnswer } from "@legion/contracts"
import { URL_NOT_PRINTED } from "@legion/contracts/log-text"
import {
  renderSessionAnswers,
  SESSION_ANSWERS_DEADLINE_MS,
  SESSION_ANSWERS_MARKER,
  type SessionAnswersCache,
  sessionAnswersFailureLine,
} from "@legion/envoy-client/session-answers"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import type { Subprocess } from "bun"
import { buildBundles } from "../scripts/build"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

// The answers hook as Claude Code runs it: the bundle `hooks.json` names
// (`dist/session-hook.js answers`), its whole environment given, its input on stdin. One stand-in
// plays Dispatch, whose answers route each case sets to answer with a list or to hang.
// Nothing commits dist/, so the bundle is one fresh build of this tree, as in
// tests/dist-fresh.test.ts.
const packageRoot = resolve(import.meta.dir, "..")
let bundle = ""
let bundleDirectory = ""
/** The directory of the hooks' timing files (`runHook`), kept until the file ends: a case's
 *  cleanup reads them for the refreshers its hooks named. */
let timingDirectory = ""

beforeAll(async () => {
  bundleDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-answers-bundle-"))
  timingDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-answers-timing-"))
  await buildBundles(bundleDirectory)
  bundle = join(bundleDirectory, "session-hook.js")
}, 60_000)

afterAll(async () => {
  await rm(bundleDirectory, { recursive: true, force: true })
  await rm(timingDirectory, { recursive: true, force: true })
})

const SESSION = "11111111-2222-4333-8444-555555555555"
/** The Dispatch token the hook is handed, in its environment or as its token command's output. */
const DISPATCH_TOKEN = fixtureSecret("dsp")
/** The Dispatch token a quick `!` token command prints. */
const COMMAND_TOKEN = fixtureSecret("dsp")
/** The Dispatch token the command envoy.json names first prints, before it names another. */
const BEFORE_TOKEN = fixtureSecret("dsp")

// Each row's `60_000` is its hang guard, and the afterEach's own `60_000` is the afterEach's: Bun
// times an afterEach by the `--timeout` the run names, 5 s when it names none, and not by its
// row's. It is the package test script's own `--timeout` (`bun test --timeout=60000`,
// package.json), given here so a run of this file alone has it too. It is a number at each row,
// not a named constant, because Biome formats a `test(…)` call whose last argument is a number as
// a test call and any other as a plain call. Each row waits for the event it needs (a
// refresher's outcome written and its lock gone, a process ended, a file another process writes)
// and decides on what that event shows. A row's body waits for at most one refresher's start-up,
// and its afterEach for at most one more (the refreshers its last hooks started, which run side
// by side), so the guard decides a row only when one refresher's start-up alone takes most of
// that minute: with the start of each refresher held 30 s, every row passes. Fixed bounds of 15 s
// and 20 s decided rows before: on a box at a load of 766, one row's first refresher wrote no
// outcome within the 15 s that row waited for it.

/** One row: whether it ended (its afterEach began), the end of its body, each hook it started
 *  with the end of its run, each hook's timing file, and its cleanup steps. Each belongs to its
 *  row alone, so an afterEach still running when the next row starts touches nothing of it. */
interface Row {
  ended: boolean
  settled: Promise<void>
  readonly hooks: { readonly process: Subprocess; readonly settled: Promise<void> }[]
  readonly timings: string[]
  readonly steps: (() => Promise<void>)[]
}

/** The row whose body the code running now belongs to: a body that goes on after its row ended
 *  still finds its own row, not the one running then. The afterEach's cleanup runs with none. */
const rowContext = new AsyncLocalStorage<Row | undefined>()
/** The row that started last, for its afterEach. */
let lastRow: Row | undefined

function currentRow(): Row {
  const current = rowContext.getStore()
  if (current === undefined) throw new Error("this step runs only in a row's body")
  return current
}

/** The current row's cleanup, run by its afterEach in the reverse order of its steps. */
const cleanup = {
  push(step: () => Promise<void>): void {
    currentRow().steps.push(step)
  },
}

/** Each row of this file: `bun:test`'s `test`, keeping the row's body for its afterEach. A body
 *  whose row timed out goes on until what it awaits settles, and then stops at its next hook or
 *  wait (`rowGoesOn`); its failure there is no verdict, Bun having given the row's, and the
 *  afterEach waits for it before the cleanup, so no body runs or fails beside the rows after it. */
function test(name: string, body: () => Promise<void>, timeout: number): void {
  bunTest(
    name,
    () => {
      const current: Row = {
        ended: false,
        settled: Promise.resolve(),
        hooks: [],
        timings: [],
        steps: [],
      }
      lastRow = current
      const run = rowContext.run(current, body).catch((error: unknown) => {
        if (!current.ended) throw error
      })
      current.settled = run.catch(() => {})
      return run
    },
    timeout,
  )
}

/** Throws once the row whose body runs this ended, so a body that timed out starts and waits for
 *  nothing more. The afterEach's cleanup, which is no row's body, goes on. */
function rowGoesOn(): void {
  if (rowContext.getStore()?.ended === true) throw new Error("the row ended before this step")
}

afterEach(async () => {
  const current = lastRow
  lastRow = undefined
  if (current === undefined) return
  current.ended = true
  // A hook still running when its row ended is the row's to end; once each has, the body
  // settles too.
  for (const hook of current.hooks) hook.process.kill("SIGKILL")
  await Promise.all(current.hooks.map((hook) => hook.settled))
  await current.settled
  // Outside any row, so a cleanup step's waits go on whichever row's context reached here.
  // Every step runs, a stand-in Dispatch's stop included, even when one before it failed; the
  // first failure is the afterEach's.
  let failure: { readonly error: unknown } | undefined
  await rowContext.run(undefined, async () => {
    for (const step of current.steps.splice(0).reverse()) {
      try {
        await step()
      } catch (error) {
        failure ??= { error }
      }
    }
  })
  if (failure !== undefined) throw failure.error
}, 60_000)

type Route =
  | { readonly kind: "list"; readonly items: readonly SessionAnswer[] }
  | { readonly kind: "hang" }
  | { readonly kind: "missing" }
  | { readonly kind: "not-a-list" }
  | { readonly kind: "proxy-404" }

/** The stand-in Dispatch: its answers route answers as `route.kind` says at the time of each
 *  request (`missing` as a Dispatch build without the route, `not-a-list` with a 200 that is no
 *  answers list, `proxy-404` with a proxy's 404 page); every request is recorded with its bearer. */
function standinDispatch(): {
  readonly base: string
  readonly requests: string[]
  route: Route
} {
  const state = {
    base: "",
    requests: [] as string[],
    route: { kind: "list", items: [] } as Route,
  }
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const url = new URL(request.url)
      state.requests.push(`${request.headers.get("authorization")} ${url.pathname}`)
      if (url.pathname !== `/api/v1/agents/${SESSION}/answers`) {
        return new Response("not found", { status: 404 })
      }
      const { route } = state
      if (route.kind === "hang") return new Promise<Response>(() => {})
      if (route.kind === "missing") {
        // What a Dispatch build without the route answers (its router's fallback).
        return Response.json(
          {
            code: "NOT_FOUND",
            error: `no route for GET ${url.pathname}`,
            hint: "GET /api/v1 lists every route",
          },
          { status: 404 },
        )
      }
      if (route.kind === "not-a-list") return Response.json({ session_id: SESSION, topics: [] })
      if (route.kind === "proxy-404") {
        return new Response("<html><body><h1>404 Not Found</h1><hr>nginx</body></html>", {
          status: 404,
          headers: { "Content-Type": "text/html" },
        })
      }
      return Response.json({
        session_id: SESSION,
        since: new Date(Date.now() - 72 * 3_600_000).toISOString(),
        total: route.items.length,
        items: route.items,
      })
    },
  })
  cleanup.push(async () => {
    await server.stop(true)
  })
  state.base = `http://127.0.0.1:${server.port}`
  return state
}

/** A person's answer to the session's ask, written `minutesAgo` minutes ago. */
function answer(id: string, text: string, minutesAgo: number): SessionAnswer {
  return {
    kind: "answer",
    relation: "answered_your_ask",
    id,
    at: new Date(Date.now() - minutesAgo * 60_000).toISOString(),
    author: { kind: "user", id: "alice@example.com" },
    text,
    selected: ["The second way"],
    ask: { id, question: "Which way should the fix go?" },
    owner: { issue: { key: "LEGION-1", title: "The work" } },
    ref: `dispatch://LEGION-1/ask/${id}`,
    href: `/issues/LEGION-1#ask-${id}`,
  }
}

/** A scratch HOME, Dispatch state directory, Claude config directory and TMPDIR for one case.
 *  They go once every refresher the case's hooks started has ended: one still running writes
 *  into them, and one not yet past its start-up makes them again. Each case's own cleanup (a gate
 *  opened) runs first, since `scratch` comes before any hook. */
async function scratch(): Promise<{
  readonly home: string
  readonly state: string
  readonly config: string
  readonly tmp: string
}> {
  const root = await mkdtemp(join(tmpdir(), "claude-envoy-answers-"))
  const owner = currentRow()
  cleanup.push(async () => {
    await refreshersEnded(owner)
    await rm(root, { recursive: true, force: true })
  })
  const home = join(root, "home")
  const state = join(root, "state")
  const config = join(root, "claude")
  const tmp = join(root, "tmp")
  for (const directory of [home, state, config, tmp]) await mkdir(directory, { recursive: true })
  return { home, state, config, tmp }
}

/** Waits until the session's refresher (the process an answers hook starts to run a `!` token
 *  command) has finished: what it read written after `since` (epoch ms), and its lock gone. The
 *  row's hang guard is the only bound. */
async function refresherFinished(state: string, since: number): Promise<void> {
  const directory = join(state, "sessions", SESSION)
  for (;;) {
    rowGoesOn()
    const outcome = await readFile(join(directory, "answers-refresh.json"), "utf8").then(
      (text): { finished_at: number } => JSON.parse(text),
      () => undefined,
    )
    if (
      outcome !== undefined &&
      outcome.finished_at >= since &&
      !existsSync(join(directory, "answers-refresh.lock"))
    ) {
      return
    }
    // The refresher is another process, detached from the hook, that tells nobody it ended: its
    // files are the only signal, so the wait polls them on the real clock.
    await Bun.sleep(50)
  }
}

/** Waits until `file` exists: the mark another process writes once it got there, and which it
 *  tells nobody of, so the wait polls it on the real clock. The case's own timeout is the only
 *  bound. */
async function fileWritten(file: string): Promise<void> {
  while (!existsSync(file)) {
    rowGoesOn()
    await Bun.sleep(20)
  }
}

/** `file`'s text, or undefined when there is no such file. */
async function textOf(file: string): Promise<string | undefined> {
  return readFile(file, "utf8").catch((error: NodeJS.ErrnoException) => {
    if (error.code === "ENOENT") return undefined
    throw error
  })
}

/** A process a case knows of: its pid, and when it started (`procState`), which a later process
 *  given the same pid does not share; null on a host with no /proc. */
interface StartedProcess {
  readonly pid: number
  readonly started: string | null
}

/** Whether this host has /proc, which every read of a process here prefers. */
const PROC = existsSync("/proc/self/stat")

/** Process `pid`'s state and when it started, the 3rd and 22nd fields of `/proc/<pid>/stat`
 *  (each counted after the command in parentheses, which may itself hold spaces), as
 *  `procStarted` reads the start; undefined once no process has that pid. One process's file,
 *  never a scan (docs/testing.md, "A test that looks for a process it started"). */
function procState(pid: number): { state: string; started: string } | undefined {
  let stat: string
  try {
    stat = readFileSync(`/proc/${pid}/stat`, "utf8")
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code
    if (code === "ENOENT" || code === "ESRCH") return undefined
    throw error
  }
  // Field 3, the state, onwards: field n is at n - 3.
  const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ")
  return { state: fields[0] as string, started: fields[19] as string }
}

/** Whether `known` has ended. With /proc: no process has its pid, or that process is a zombie,
 *  or it is another process given the pid since (another start); read this way rather than by
 *  `kill(pid, 0)`, which answers for a zombie and for that other process. A process a kill reached
 *  still reads as running until the kernel has run its exit: in a gVisor sandbox a SIGKILLed
 *  subshell read S, R or D for 1 to 29 ms after the kill, and gVisor shows neither the exit flag
 *  nor the pending signal in /proc. So each read here is a wait (`processEnded`), never one look.
 *  With no /proc (macOS): `kill(pid, 0)` failing with ESRCH, at once. `ps` would read the start
 *  there, but procps `ps -p` reads every process (0.76 to 0.98 s unloaded on the dev box, up to
 *  8.2 s on a loaded CPU pair), and rows read processes again and again while a token command
 *  waits on their gate, so `ps`'s time would spend the row's minute. A zombie not yet reaped,
 *  or a pid given to another process within the row, makes such a wait longer, never shorter. */
function ended(known: StartedProcess): boolean {
  if (!PROC) {
    try {
      process.kill(known.pid, 0)
      return false
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ESRCH") return true
      throw error
    }
  }
  if (known.started === null) throw new Error(`no start was read for process ${known.pid}`)
  const now = procState(known.pid)
  return (
    now === undefined ||
    now.state.startsWith("Z") ||
    now.state.startsWith("X") ||
    now.started !== known.started
  )
}

/** Waits until `known` has ended. It tells nobody it ended, so the wait polls it on the real
 *  clock; one that ended answers at once. The row's hang guard is the only bound. */
async function processEnded(known: StartedProcess): Promise<void> {
  while (!ended(known)) {
    rowGoesOn()
    await Bun.sleep(20)
  }
}

/** The start the test read from /proc for a refresher whose hook named it with none (its own
 *  read of /proc missed the refresher), by the hook's timing file: read once, as soon as the case
 *  first looks, and null when the refresher had already ended by then. */
const startsRead = new Map<string, string | null>()

/** The refreshers the hooks of `owner` named in their timing files. A hook names its refresher as
 *  soon as it started it, so one ended before it exited still told; one with no file was ended
 *  before it started any. With /proc, one named with no start gets the start the test reads for
 *  it, and one that had ended before that read is left out. */
async function namedRefreshers(owner: Row = currentRow()): Promise<StartedProcess[]> {
  const named: StartedProcess[] = []
  for (const file of owner.timings) {
    const text = await textOf(file)
    if (text === undefined) continue
    const { refresher }: Pick<HookTiming, "refresher"> = JSON.parse(text)
    if (refresher === null) continue
    if (refresher.started !== null || !PROC) {
      named.push(refresher)
      continue
    }
    if (!startsRead.has(file)) startsRead.set(file, procState(refresher.pid)?.started ?? null)
    const started = startsRead.get(file) ?? null
    if (started !== null) named.push({ pid: refresher.pid, started })
  }
  return named
}

/** Waits until every refresher the hooks of `owner` named has ended. */
async function refreshersEnded(owner: Row = currentRow()): Promise<void> {
  for (const refresher of await namedRefreshers(owner)) await processEnded(refresher)
}

/** Waits until one refresher the case's hooks named holds the session's lock (its pid in
 *  `answers-refresh.lock`) and every other one they named has ended, having lost the lock to it.
 *  From then until that refresher ends, no hook starts one, and none the hooks named can take the
 *  lock after it. */
async function refresherHoldsLock(state: string): Promise<void> {
  const lock = join(state, "sessions", SESSION, "answers-refresh.lock")
  for (;;) {
    rowGoesOn()
    const named = await namedRefreshers()
    const pid = Number((await textOf(lock)) ?? "")
    const holder = named.find((refresher) => refresher.pid === pid && !ended(refresher))
    if (holder !== undefined && named.every((other) => other === holder || ended(other))) return
    if (named.every(ended)) {
      throw new Error("every refresher the hooks started ended, and none took the lock")
    }
    await Bun.sleep(20)
  }
}

/** The start of a `!` token command that leaves a process behind: a subshell, started in the
 *  background, that runs while `home` exists (the case's scratch removes it), and whose pid the
 *  command writes to `file`, whole (through a rename). envoy-config runs the command as the leader
 *  of a process group of its own, which the subshell is in, and kills that group once the command
 *  exits; with no such kill the subshell outlives the command, and a wait for its end
 *  (`processEnded`) holds until the row's hang guard fails the row. */
function leftBehind(file: string, home: string): string {
  return `(while [ -d '${home}' ]; do sleep 0.02; done) >/dev/null 2>&1 & echo $! > '${file}.part' && mv '${file}.part' '${file}'; `
}

/** Waits until a command started with `leftBehind(file, …)` has started, and gives back the
 *  process it left behind, read while that command still waits on the case: before any group
 *  kill could reach it. */
async function leftBehindStarted(file: string): Promise<StartedProcess> {
  await fileWritten(file)
  const pid = Number(await readFile(file, "utf8"))
  return { pid, started: PROC ? (procState(pid)?.started ?? null) : null }
}

/** A `--preload` for a hook that writes `signal` once its process has started, then holds the
 *  hook's code until a refresher has finished after that start (its outcome written, its lock
 *  gone), with no bound of its own: when the case times out, bun ends the hook. The hook then
 *  finds that outcome at its first look, and starts a refresher of its own, so where a
 *  refresher's race with the hook's deadline would decide the case, this decides it. */
async function preloadAfterRefresher(home: string, state: string, signal: string): Promise<string> {
  const directory = join(state, "sessions", SESSION)
  const outcome = JSON.stringify(join(directory, "answers-refresh.json"))
  const lock = JSON.stringify(join(directory, "answers-refresh.lock"))
  const preload = join(home, "after-refresher.ts")
  await writeFile(
    preload,
    [
      `import { existsSync, readFileSync, writeFileSync } from "node:fs"`,
      `const startedAt = Date.now() - performance.now()`,
      `writeFileSync(${JSON.stringify(signal)}, "")`,
      `for (;;) {`,
      `  let finishedAt = 0`,
      `  try {`,
      `    finishedAt = JSON.parse(readFileSync(${outcome}, "utf8")).finished_at`,
      `  } catch {}`,
      `  if (finishedAt >= startedAt && !existsSync(${lock})) break`,
      `  Bun.sleepSync(20)`,
      `}`,
      "",
    ].join("\n"),
  )
  return preload
}

/** One wait the hook armed (`armedWait` in hooks/session-hook.ts): a look at the refresher's
 *  outcome, the Dispatch read, or the read left in flight after printing; when it armed it, for how
 *  long, and the time it armed it against. */
interface ArmedWait {
  readonly kind: "refresher" | "read" | "settle"
  readonly at: number
  readonly ms: number
  readonly limit: number
}

/** What the hook measured of itself, written to `ENVOY_ANSWERS_TIMING_FILE`, in ms after its
 *  process started: when its code began, when it printed (null: it failed before), and when it
 *  exited; the print deadline it applied and each wait it armed; and the refresher it started
 *  (null: it started none). */
interface HookTiming {
  readonly began: number
  readonly printed: number | null
  readonly ended: number
  readonly deadline: number
  readonly waits: readonly ArmedWait[]
  readonly refresher: StartedProcess | null
}

interface HookRun {
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
  readonly timing: HookTiming
}

/** How many hooks the file started, which names each one's timing file. */
let hookCount = 0

/** How long envoy-config lets a token command run in the hooks and refreshers of this file
 *  (`ENVOY_COMMAND_TIMEOUT_MS`, tests only): a row's own timeout, in place of the 10 s a user's
 *  command gets. Rows hold a command on a gate while they wait on hooks and refreshers whose
 *  start-ups a loaded host can stretch past 10 s, so the 10 s would decide them. */
const COMMAND_BOUND_MS = 60_000

function runHook(
  env: Record<string, string>,
  input: Record<string, unknown>,
  options: { readonly preload?: string } = {},
): Promise<HookRun> {
  rowGoesOn()
  const owner = currentRow()
  hookCount += 1
  const timingFile = join(timingDirectory, `hook-${hookCount}.json`)
  owner.timings.push(timingFile)
  const preload = options.preload === undefined ? [] : ["--preload", options.preload]
  const run = Bun.spawn([process.execPath, ...preload, bundle, "answers"], {
    env: {
      PATH: process.env["PATH"] ?? "",
      ...NO_TRANSPILER_CACHE,
      CLAUDE_PLUGIN_ROOT: packageRoot,
      ENVOY_ANSWERS_TIMING_FILE: timingFile,
      ENVOY_COMMAND_TIMEOUT_MS: String(COMMAND_BOUND_MS),
      ...env,
    },
    stdin: new TextEncoder().encode(JSON.stringify(input)),
    stdout: "pipe",
    stderr: "pipe",
  })
  const finished = (async (): Promise<HookRun> => {
    const [exitCode, stdout, stderr] = await Promise.all([
      run.exited,
      new Response(run.stdout).text(),
      new Response(run.stderr).text(),
    ])
    const timing: HookTiming = JSON.parse(await readFile(timingFile, "utf8"))
    return { exitCode, stdout, stderr, timing }
  })()
  // A handler of its own, so a run whose row ended before it did fails nothing after that row.
  owner.hooks.push({
    process: run,
    settled: finished.then(
      () => {},
      () => {},
    ),
  })
  return finished
}

/** The hook's print deadline as the design states it, in ms after its process started: 800 ms,
 *  or `ANSWERS_LEAST_WAIT_MS` (150 ms) after its code began when bun's start-up took longer. */
function printDeadline(run: HookRun): number {
  return Math.max(SESSION_ANSWERS_DEADLINE_MS, run.timing.began + 150)
}

/** The reason a hook whose refresher had not finished by its print deadline names. */
function notFinishedReason(run: HookRun): string {
  return `a "!" command in envoy.json had not finished ${Math.round(printDeadline(run))} ms after the hook started`
}

/** Holds the hook to the deadline the design gives it, by what the hook recorded of what it did,
 *  never by how long it took: the deadline it applied is the design's (`printDeadline`); it
 *  printed; it armed each look at the refresher's outcome before that deadline and for no longer
 *  than what was left of it, so it gave up at its first look past the deadline; it gave the
 *  Dispatch read what was left, or the least wait (150 ms, `ANSWERS_LEAST_WAIT_MS`); and after
 *  printing it gave a read still in flight at most until 50 ms past the deadline
 *  (`ANSWERS_SETTLE_MS`), once. The time the host leaves the hook unscheduled, bun's start-up
 *  included, is in none of these, so it never decides the check, and a hook that waits past its
 *  deadline arms a wait that does. A wait the hook would make without arming it through
 *  `armedWait` is not seen here; a token command or a gate it waited on shows as another outcome
 *  or a row that hangs, and the exact budgets it named are checked from what it printed
 *  (`notFinishedReason`, `expectReadBudget`). */
function expectWithinDeadline(run: HookRun): void {
  const { deadline, printed, waits } = run.timing
  expect(deadline).toBe(printDeadline(run))
  expect(printed).not.toBeNull()
  for (const wait of waits) {
    const limit = wait.kind === "settle" ? deadline + 50 : deadline
    const least = wait.kind === "read" ? 150 : 0
    expect({ kind: wait.kind, limit: wait.limit }).toEqual({ kind: wait.kind, limit })
    expect(wait.ms).toBeLessThanOrEqual(Math.max(least, Math.round(wait.limit - wait.at)))
    if (wait.kind === "refresher") expect(wait.at).toBeLessThan(wait.limit)
  }
  expect(waits.filter((wait) => wait.kind === "settle")).toHaveLength(1)
}

/** Holds a read that did not answer to the budget the design gives it, `waitedMs` as the hook's
 *  line names it: what was left of the deadline when the read started, never less than 150 ms
 *  (`ANSWERS_LEAST_WAIT_MS`), so at most what was left when the hook's code began. */
function expectReadBudget(run: HookRun, waitedMs: number): void {
  expect(waitedMs).toBeGreaterThanOrEqual(150)
  expect(waitedMs).toBeLessThanOrEqual(
    Math.max(150, Math.round(printDeadline(run) - run.timing.began)),
  )
}

/** A read the hook gave up on, as its line names it: Dispatch's answer missed the hook's wait. */
const READ_MISSED = /^Dispatch did not answer within \d+ ms$/

/** The same read, held to the least wait the design gives it, 150 ms (`ANSWERS_LEAST_WAIT_MS`):
 *  a hook whose start-up spent the whole deadline still waits that long, so a line naming a
 *  shorter wait, "within 0 ms" above all, fails the test. */
const READ_MISSED_LEAST_WAIT = /^Dispatch did not answer within (1[5-9]\d|[2-9]\d\d|\d{4,}) ms$/

/** A refresher the hook gave up on, as its line names it for `run`'s own deadline. */
function refresherMissed(run: HookRun): RegExp {
  return new RegExp(`^${notFinishedReason(run).replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`)
}

/** Which of the outcomes the design allows a hook whose start-up may have spent its budget
 *  `context` is: `fresh` (the list Dispatch holds, `items`), `cached` (that list as a hook read it
 *  before, its second line naming a `reason` this read failed), or `failure` (the failure line
 *  naming a `reason`, no list having been read). Anything else fails the test. On an unloaded host
 *  the hook gets the fresh list; on a loaded one its 150 ms read, or the refresher, can miss. */
function allowedOutcome(
  context: string,
  items: readonly SessionAnswer[],
  reason: RegExp,
): "fresh" | "cached" | "failure" {
  const fresh = renderSessionAnswers(items, { now: Date.now(), total: items.length }) as string
  if (context === fresh) return "fresh"
  const lines = context.split("\n")
  const header = /^As read at \S+; this read failed: (.*?)\.?$/.exec(lines[1] ?? "")
  if (header !== null && [lines[0], ...lines.slice(2)].join("\n") === fresh) {
    expect(header[1]).toMatch(reason)
    return "cached"
  }
  const prefix = sessionAnswersFailureLine("\u0000").split("\u0000")
  expect(context.startsWith(prefix[0] as string) && context.endsWith(prefix[1] as string)).toBe(
    true,
  )
  expect(context.slice((prefix[0] as string).length, -(prefix[1] as string).length)).toMatch(reason)
  return "failure"
}

function sessionStart(source: string, cwd: string): Record<string, unknown> {
  return { session_id: SESSION, source, cwd, hook_event_name: "SessionStart" }
}

/** The hook's additionalContext, checked to be under the event that ran it. */
function contextOf(run: HookRun, event: string): string {
  expect(run.exitCode).toBe(0)
  expect({ stdout: run.stdout === "" ? "" : "printed", stderr: run.stderr }).toMatchObject({
    stdout: "printed",
  })
  const output = JSON.parse(run.stdout)
  expect(output.hookSpecificOutput.hookEventName).toBe(event)
  const context: string = output.hookSpecificOutput.additionalContext
  // Claude Code keeps an additionalContext string whole only up to 10,000 characters.
  expect(context.length).toBeLessThan(10_000)
  return context
}

/** Writes `answers.json` as a read of `dispatchUrl` that answered `items` leaves it, in the read's
 *  own type (`SessionAnswersCache`), for a case about what a hook does with a list read before: a
 *  hook's own read writes it only when it lands within the wait the hook gives it, which a loaded
 *  host does not always allow. */
async function answersRead(
  state: string,
  dispatchUrl: string,
  items: readonly SessionAnswer[],
): Promise<void> {
  const directory = join(state, "sessions", SESSION)
  await mkdir(directory, { recursive: true })
  const cache: SessionAnswersCache = {
    dispatch_url: dispatchUrl,
    read_at: new Date().toISOString(),
    total: items.length,
    items,
  }
  await writeFile(join(directory, "answers.json"), JSON.stringify(cache))
}

test("puts the list into context at every SessionStart source and at SubagentStart, read for the session the input names", async () => {
  const dispatch = standinDispatch()
  const items = [
    answer("ask-2", "Go the second way; keep the cache.", 5),
    answer("ask-1", "Not yet.", 60),
  ]
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN,
  }
  const expected = renderSessionAnswers(items, { now: Date.now(), total: items.length })
  expect(expected?.startsWith(SESSION_ANSWERS_MARKER)).toBe(true)

  // Each hook carries the list Dispatch holds, or, when its start-up left the read only 150 ms
  // and Dispatch's answer missed it, the list a hook read before or the failure line, each naming
  // the missed read.
  const outcomes: string[] = []
  for (const source of ["startup", "resume", "clear", "compact", "fork"]) {
    const run = await runHook(env, sessionStart(source, home))
    outcomes.push(allowedOutcome(contextOf(run, "SessionStart"), items, READ_MISSED))
  }
  const subagent = await runHook(env, {
    session_id: SESSION,
    cwd: home,
    hook_event_name: "SubagentStart",
    agent_id: "agent-1",
    agent_type: "general-purpose",
  })
  outcomes.push(allowedOutcome(contextOf(subagent, "SubagentStart"), items, READ_MISSED))
  // Every read was for the session the input names, with the configured bearer.
  expect(dispatch.requests.length).toBeGreaterThanOrEqual(1)
  expect(dispatch.requests.length).toBeLessThanOrEqual(6)
  for (const request of dispatch.requests) {
    expect(request).toBe(`Bearer ${DISPATCH_TOKEN} /api/v1/agents/${SESSION}/answers`)
  }
  // A read that answered left the list for the next hook process.
  if (outcomes.includes("fresh")) {
    const file = join(state, "sessions", SESSION, "answers.json")
    expect(JSON.parse(await readFile(file, "utf8")).items).toEqual(items)
  }
}, 60_000)

test("with Dispatch hanging, answers by the hook's deadline with the list a hook read before", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-3", "Read before Dispatch hung.", 2)]
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN,
  }
  contextOf(await runHook(env, sessionStart("startup", home)), "SessionStart")
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))

  dispatch.route = { kind: "hang" }
  const run = await runHook(env, sessionStart("compact", home))
  const context = contextOf(run, "SessionStart")
  // The header's second line names when the list was read and why this read failed.
  const waited =
    /^.*\nAs read at .*; this read failed: Dispatch did not answer within (\d+) ms\.\n/.exec(
      context,
    )
  expect(waited).not.toBeNull()
  expect(context).toBe(
    renderSessionAnswers(items, {
      now: Date.now(),
      total: 1,
      cached: { readAt: cache.read_at, reason: `Dispatch did not answer within ${waited?.[1]} ms` },
    }) as string,
  )
  expect(context).toContain("Read before Dispatch hung.")
  expectWithinDeadline(run)
  expectReadBudget(run, Number(waited?.[1]))
  expect(dispatch.requests).toHaveLength(2)
}, 60_000)

test("with Dispatch hanging after a read gave an empty list, the failure line names the wait rather than nothing", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "hang" }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN,
  }
  // The empty list Dispatch held, as the read that gave it left it in answers.json.
  await answersRead(state, dispatch.base, [])

  const context = contextOf(await runHook(env, sessionStart("resume", home)), "SessionStart")
  const waited =
    /^\[Dispatch: people's answers from the last 72 hours could not be read \(Dispatch did not answer within (\d+) ms\); run `dispatch answers`\.\]$/.exec(
      context,
    )
  expect(waited).not.toBeNull()
}, 60_000)

test("with Dispatch hanging and no list ever read, the failure line names the wait", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "hang" }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN,
  }

  const run = await runHook(env, sessionStart("startup", home))
  const context = contextOf(run, "SessionStart")
  const waited =
    /^\[Dispatch: people's answers from the last 72 hours could not be read \(Dispatch did not answer within (\d+) ms\); run `dispatch answers`\.\]$/.exec(
      context,
    )
  expect(waited).not.toBeNull()
  expect(context).toBe(
    sessionAnswersFailureLine(`Dispatch did not answer within ${waited?.[1]} ms`),
  )
  expectWithinDeadline(run)
  expectReadBudget(run, Number(waited?.[1]))
}, 60_000)

test("a list past the cap stays under 8,000 characters with the left-out line", async () => {
  const dispatch = standinDispatch()
  const items = Array.from({ length: 12 }, (_, index) =>
    answer(`ask-${index}`, `${index}: ${"word ".repeat(180)}`, index + 1),
  )
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN,
  }

  // A hook whose read missed its wait carries the failure line (no list read yet) or the list a
  // hook read before; whichever list a hook shows, fresh or read before, keeps to the cap.
  let shown: string | undefined
  for (const source of ["startup", "resume", "clear"]) {
    const context = contextOf(await runHook(env, sessionStart(source, home)), "SessionStart")
    if (allowedOutcome(context, items, READ_MISSED) !== "failure") {
      shown = context
      break
    }
  }
  expect(shown).toBeDefined()
  expect((shown as string).length).toBeLessThan(8_000)
  expect(shown).toMatch(
    /\n\n\d+ older items left out to stay under 8,000 characters: run `dispatch answers` to read every one in full\.$/,
  )
}, 60_000)

test("adds nothing when the list is empty or Dispatch is not configured", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [] }
  const { home, state } = await scratch()

  const empty = await runHook(
    {
      HOME: home,
      DISPATCH_STATE_DIR: state,
      DISPATCH_URL: dispatch.base,
      DISPATCH_TOKEN,
    },
    sessionStart("startup", home),
  )
  // An empty list adds nothing; a read that missed the hook's wait, with no list read before,
  // says so rather than looking like nothing.
  if (empty.stdout !== "") {
    expect(allowedOutcome(contextOf(empty, "SessionStart"), [], READ_MISSED)).toBe("failure")
  }
  expect(empty.exitCode).toBe(0)
  const off = await runHook(
    { HOME: home, DISPATCH_STATE_DIR: state },
    sessionStart("startup", home),
  )
  expect(off).toMatchObject({ exitCode: 0, stdout: "", stderr: "" })
  // The empty list came from the one read. A read that missed the wait (the hook printed the
  // failure line) may end with the hook's process before it reaches Dispatch. Dispatch off sends
  // nothing.
  const expectedRequests = empty.stdout === "" ? [1] : [0, 1]
  expect(expectedRequests).toContain(dispatch.requests.length)
}, 60_000)

/** What the hook writes to stderr, once, when Dispatch at `base` has no answers route. */
function noRouteLine(base: string): string {
  return `envoy: Dispatch at ${base} has no answers route (its build is older than the route), so people's answers cannot be read from it\n`
}

test("a Dispatch with no answers route adds nothing to the context, and the hook says so once on stderr", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "missing" }
  const { home, state } = await scratch()
  const run = await runHook(
    {
      HOME: home,
      DISPATCH_STATE_DIR: state,
      DISPATCH_URL: dispatch.base,
      DISPATCH_TOKEN,
    },
    sessionStart("startup", home),
  )
  expect(run.exitCode).toBe(0)
  if (run.stdout === "") {
    // The read landed: nothing for the model, and one line for whoever reads the hook's stderr.
    expect(run.stderr).toBe(noRouteLine(dispatch.base))
  } else {
    // A read that missed the hook's wait, with no list read before, says so, as on any Dispatch.
    expect(allowedOutcome(contextOf(run, "SessionStart"), [], READ_MISSED)).toBe("failure")
    expect(run.stderr).toBe("")
  }
  expect(run.stdout).not.toContain("answers route")
  expect(existsSync(join(state, "sessions", SESSION, "answers.json"))).toBe(false)
}, 60_000)

test("a 200 that is no answers list, and a proxy's 404 page, carry the failure line naming them", async () => {
  for (const [kind, reason] of [
    ["not-a-list", /^Dispatch answered with something that is not an answers list$/],
    ["proxy-404", /answered 404( Not Found)? with a body that is not Dispatch's error JSON/],
  ] as const) {
    const dispatch = standinDispatch()
    dispatch.route = { kind }
    const { home, state } = await scratch()
    const run = await runHook(
      {
        HOME: home,
        DISPATCH_STATE_DIR: state,
        DISPATCH_URL: dispatch.base,
        DISPATCH_TOKEN,
      },
      sessionStart("startup", home),
    )
    const named = new RegExp(`${reason.source}|${READ_MISSED.source}`)
    expect(allowedOutcome(contextOf(run, "SessionStart"), [], named)).toBe("failure")
    expect(run.stderr).toBe("")
  }
}, 60_000)

test("a refresher that met a Dispatch with no answers route hands the hook its notice, and no line", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "missing" }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: `!printf ${COMMAND_TOKEN}` } }),
  )

  // As in the failing-command case: the waiting hook holds its code until the refresher the
  // starting hook starts has finished, so it carries that refresher's outcome.
  const ready = join(home, "ready")
  const preload = await preloadAfterRefresher(home, state, ready)
  const waiting = runHook(env, sessionStart("resume", home), { preload })
  await fileWritten(ready)
  const starting = await runHook(env, sessionStart("startup", home))
  const carried = await waiting

  expect(carried).toMatchObject({ exitCode: 0, stdout: "", stderr: noRouteLine(dispatch.base) })
  expect(starting.exitCode).toBe(0)
  expect(starting.stdout).not.toContain("answers route")
  // The refresher whose notice the waiting hook carried made this read before it wrote that
  // outcome. The refresher the waiting hook then started decides nothing here: the case's scratch
  // waits for its end.
  expect(dispatch.requests).toContain(`Bearer ${COMMAND_TOKEN} /api/v1/agents/${SESSION}/answers`)
}, 60_000)

test("a quick `!` token command runs outside the hook and gives the bearer, and the next hook carries the list its refresher read", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-4", "Token from a command.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: `!printf ${COMMAND_TOKEN}` } }),
  )

  const quickSince = Date.now()
  const quick = await runHook(env, sessionStart("startup", home))
  // The command runs in the refresher, a second bun process: the design carries its list when it
  // lands by the hook's deadline, and otherwise names the unfinished command, on the list the
  // refresher already left in answers.json or on the failure line.
  allowedOutcome(contextOf(quick, "SessionStart"), items, refresherMissed(quick))
  expectWithinDeadline(quick)
  await refresherFinished(state, quickSince)
  expect(dispatch.requests).toEqual([`Bearer ${COMMAND_TOKEN} /api/v1/agents/${SESSION}/answers`])
  // Either way the next hook carries the list the refresher read with the command's token. The
  // refresher this hook starts reads with the token its command prints, as the slow command's
  // case shows; the case's scratch waits for its end.
  const next = await runHook(env, sessionStart("clear", home))
  expect(contextOf(next, "SessionStart")).toContain("Token from a command.")
}, 60_000)

test("a slow `!` token command cannot hold the hook, and its refresher still reads with the token it printed and leaves nothing running", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-4", "Token from a command.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  await answersRead(state, dispatch.base, items)

  // The command leaves a process behind, then waits until the test opens the gate, so no
  // refresher lands by a deadline.
  const gate = join(home, "gate")
  const left = join(home, "left-behind")
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!${leftBehind(left, home)}while [ ! -e '${gate}' ]; do sleep 0.02; done; printf late`,
      },
    }),
  )
  cleanup.push(() => writeFile(gate, ""))
  const slowSince = Date.now()
  const hanging = await runHook(env, sessionStart("resume", home))
  const context = contextOf(hanging, "SessionStart")
  // The list read before stands in, naming why this read gave none.
  expect(context.split("\n")[1]).toStartWith("As read at ")
  expect(context.split("\n")[1]).toEndWith(`; this read failed: ${notFinishedReason(hanging)}.`)
  expect(context).toContain("Token from a command.")
  expectWithinDeadline(hanging)

  // With no list read before, the failure line says why none was read now. The refresher the hook
  // above started holds the session's lock by then, so this hook starts none.
  await refresherHoldsLock(state)
  await rm(join(state, "sessions", SESSION, "answers.json"))
  const uncached = await runHook(env, sessionStart("compact", home))
  expect(contextOf(uncached, "SessionStart")).toBe(
    sessionAnswersFailureLine(notFinishedReason(uncached)),
  )
  expectWithinDeadline(uncached)
  expect(uncached.timing.refresher).toBeNull()
  expect(dispatch.requests).toEqual([])

  // The slow command still ends, its refresher reads with the token it printed, and envoy-config
  // killed the command's process group, the process it left behind with it: that process ends,
  // where with no group kill it runs until the case's cleanup.
  const behind = await leftBehindStarted(left)
  await writeFile(gate, "")
  await refresherFinished(state, slowSince)
  expect(dispatch.requests).toEqual([`Bearer late /api/v1/agents/${SESSION}/answers`])
  await processEnded(behind)
  expect(await readdir(tmp)).toEqual([])
}, 60_000)

test("a token command slower than the hook's deadline finishes and cleans up after the hook, and the next hook carries the list it read", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-5", "Read with a slow command's token.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  // The command leaves a process behind, then waits until the test opens the gate.
  const gate = join(home, "gate")
  const left = join(home, "left-behind")
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!${leftBehind(left, home)}while [ ! -e '${gate}' ]; do sleep 0.02; done; printf ${DISPATCH_TOKEN}`,
      },
    }),
  )

  const firstSince = Date.now()
  cleanup.push(() => writeFile(gate, ""))
  const first = await runHook(env, sessionStart("startup", home))
  expect(contextOf(first, "SessionStart")).toBe(sessionAnswersFailureLine(notFinishedReason(first)))
  expectWithinDeadline(first)
  // The command outlives the hook, in the refresher the hook started (whose own bun start-up may
  // end after the hook did).
  const behind = await leftBehindStarted(left)

  await writeFile(gate, "")
  await refresherFinished(state, firstSince)
  expect(await readdir(tmp)).toEqual([])
  // envoy-config killed the command's process group, the process it left behind with it: that
  // process ends, where with no group kill it runs until the case's cleanup.
  await processEnded(behind)
  expect(dispatch.requests).toEqual([`Bearer ${DISPATCH_TOKEN} /api/v1/agents/${SESSION}/answers`])

  // The next hook carries the list that refresher read. Its own refresher runs the command again,
  // which waits until the case's cleanup opens the gate, so it lands by no deadline; the slow
  // command's case shows such a refresher's end, and the case's scratch waits for this one's.
  await rm(gate)
  const second = await runHook(env, sessionStart("resume", home))
  const context = contextOf(second, "SessionStart")
  expect(context.split("\n")[1]).toStartWith("As read at ")
  expect(context.split("\n")[1]).toEndWith(`; this read failed: ${notFinishedReason(second)}.`)
  expect(context).toContain("Read with a slow command's token.")
  expectWithinDeadline(second)
}, 60_000)

test("with a slow token command, a list read from another Dispatch than envoy.json names now never stands in", async () => {
  const before = standinDispatch()
  const beforeItems = [answer("ask-a", "Read from the Dispatch before.", 1)]
  before.route = { kind: "list", items: beforeItems }
  const now = standinDispatch()
  now.route = { kind: "list", items: [answer("ask-b", "Read from the Dispatch now.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: before.base, token: `!printf ${BEFORE_TOKEN}` } }),
  )
  const firstSince = Date.now()
  const first = await runHook(env, sessionStart("startup", home))
  // The first hook carries `before`'s list when its refresher lands by the deadline, and otherwise
  // names the unfinished command, on the list the refresher already left or on the failure line;
  // either way the refresher leaves that list for the next hook.
  allowedOutcome(contextOf(first, "SessionStart"), beforeItems, refresherMissed(first))
  await refresherFinished(state, firstSince)
  expect(before.requests).toEqual([`Bearer ${BEFORE_TOKEN} /api/v1/agents/${SESSION}/answers`])
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))
  expect(cache.dispatch_url).toBe(before.base)

  // The command waits until the case's cleanup opens the gate, so its refresher lands by no
  // deadline however long the host leaves the hook unscheduled.
  const gate = join(home, "gate")
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: now.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf now-token`,
      },
    }),
  )
  cleanup.push(() => writeFile(gate, ""))
  const hanging = await runHook(env, sessionStart("resume", home))
  expect(contextOf(hanging, "SessionStart")).toBe(
    sessionAnswersFailureLine(notFinishedReason(hanging)),
  )
  expectWithinDeadline(hanging)
  // The refresher this hook started reads the Dispatch named now, as the case of a refresher
  // started after one read `before` shows; the case's scratch waits for its end.
}, 60_000)

test("a refresher started for the Dispatch envoy.json named before never hands its list to a hook for the Dispatch it names now", async () => {
  // The refresher is shared by every hook of the session, whatever Dispatch each resolves. A
  // refresher started while envoy.json named `before` that finishes after a later hook, started
  // after envoy.json moved to `now`, began must not give that hook `before`'s list, the
  // invariant the review asked for ("no list from another Dispatch URL"). `before` is named with
  // a user and a password, which the request carries and the hook's reason names by its fixed
  // phrase alone (`urlForText`), no part of it.
  const before = standinDispatch()
  before.route = { kind: "list", items: [answer("ask-x", "Read from the Dispatch before.", 1)] }
  const now = standinDispatch()
  now.route = { kind: "list", items: [] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  const gate = join(home, "gate")
  const started = join(home, "command-started")
  const user = "user-7f2a"
  const secret = "pw-4c7e19"
  const beforeUrl = before.base.replace("://", `://${user}:${secret}@`)

  // The first hook's refresher runs the command, which marks its start and waits until the gate
  // opens.
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: beforeUrl,
        token: `!: > '${started}'; while [ ! -e '${gate}' ]; do sleep 0.02; done; printf ${BEFORE_TOKEN}`,
      },
    }),
  )
  cleanup.push(() => writeFile(gate, ""))
  const first = await runHook(env, sessionStart("startup", home))
  expect(contextOf(first, "SessionStart")).toBe(sessionAnswersFailureLine(notFinishedReason(first)))

  // The refresher resolves envoy.json before its command runs: once the command runs, it reads
  // `before` whatever envoy.json names next.
  await fileWritten(started)

  // envoy.json now names another Dispatch. A hook starts while the refresher runs; it opens the
  // gate once its process has started and holds its code until the refresher has read `before`
  // and finished, so the hook finds that outcome, from after it started, at its first look. The
  // refresher this hook then starts runs a command that waits on a gate of its own, which the
  // case's cleanup opens, so it writes no outcome over that one before the hook has looked.
  const nowGate = join(home, "now-gate")
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: now.base,
        token: `!while [ ! -e '${nowGate}' ]; do sleep 0.02; done; printf now-token`,
      },
    }),
  )
  cleanup.push(() => writeFile(nowGate, ""))
  const preload = await preloadAfterRefresher(home, state, gate)
  const context = contextOf(
    await runHook(env, sessionStart("resume", home), { preload }),
    "SessionStart",
  )

  // The refresher did read `before`, so the case is not vacuous.
  expect(before.requests).toEqual([`Bearer ${BEFORE_TOKEN} /api/v1/agents/${SESSION}/answers`])
  expect(context).not.toContain("Read from the Dispatch before.")
  // The hook names why it carried nothing of that read; the list `before` answered is no list
  // read from `now`, so the failure line stands.
  expect(context).toBe(
    sessionAnswersFailureLine(
      `the refresher read another Dispatch URL ${URL_NOT_PRINTED}, not the Dispatch envoy.json names now`,
    ),
  )
  expect(context).not.toContain(user)
  expect(context).not.toContain(secret)
  // The refresher this hook started reads the Dispatch named now, as the next case shows; the
  // case's scratch waits for its end.
}, 60_000)

test("a refresher started after one read the Dispatch envoy.json named before reads the Dispatch it names now, with the token its own command printed", async () => {
  const before = standinDispatch()
  const beforeItems = [answer("ask-x", "Read from the Dispatch before.", 1)]
  before.route = { kind: "list", items: beforeItems }
  const now = standinDispatch()
  now.route = { kind: "list", items: [] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  // What a refresher that read `before` leaves: the list in answers.json, and its outcome, here
  // finished a minute from now so the hook below takes it as its own refresher's.
  await answersRead(state, before.base, beforeItems)
  await refresherOutcome(
    state,
    before.base,
    renderSessionAnswers(beforeItems, { now: Date.now(), total: 1 }) as string,
  )
  // The command waits until the test opens the gate, so the refresher the hook below starts
  // writes no outcome over the planted one before the hook has looked.
  const gate = join(home, "gate")
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({
      dispatch: {
        serverUrl: now.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf now-token`,
      },
    }),
  )
  cleanup.push(() => writeFile(gate, ""))

  const context = contextOf(await runHook(env, sessionStart("resume", home)), "SessionStart")
  expect(context).toBe(
    sessionAnswersFailureLine(
      `the refresher read ${before.base}, not the Dispatch envoy.json names now`,
    ),
  )
  // The refresher that hook started reads the Dispatch named now, never `before`. The wait is the
  // refresher's own end: the outcome planted above is dated a minute ahead.
  await writeFile(gate, "")
  await refreshersEnded()
  expect(now.requests).toEqual([`Bearer now-token /api/v1/agents/${SESSION}/answers`])
  expect(before.requests).toEqual([])
}, 60_000)

test("a token command that fails gives the hook its error, not a Dispatch envoy.json no longer names", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [answer("ask-f", "Never read: the token failed.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: "!echo boom >&2; exit 3" } }),
  )
  const failed = sessionAnswersFailureLine(
    `${envoyJson}: dispatch.token's command exited with status 3`,
  )

  // The waiting hook starts first and holds its code until a refresher started after its process
  // has finished, so it finds that refresher's outcome at its first look; the starting hook,
  // started once the waiting one runs, starts that refresher.
  const ready = join(home, "ready")
  const preload = await preloadAfterRefresher(home, state, ready)
  const waiting = runHook(env, sessionStart("resume", home), { preload })
  await fileWritten(ready)
  const starting = await runHook(env, sessionStart("startup", home))
  const context = contextOf(await waiting, "SessionStart")

  // The refresher targeted the URL envoy.json names, so its failure line, naming the command's
  // exit, stands; never the reason for a refresher that read another Dispatch.
  expect(context).toBe(failed)
  expect(context).toContain("exited with status 3")
  expect(context).not.toContain("not the Dispatch envoy.json names now")
  // The starting hook carries the same line, or names the command as unfinished when its
  // refresher lands after its deadline.
  expect([failed, sessionAnswersFailureLine(notFinishedReason(starting))]).toContain(
    contextOf(starting, "SessionStart"),
  )
  expect(dispatch.requests).toEqual([])
  // The refresher the waiting hook started runs the failing command too; the next case shows what
  // such a refresher leaves, and the case's scratch waits for its end.
}, 60_000)

test("a token command that fails reads no Dispatch and leaves nothing in TMPDIR", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [answer("ask-f", "Never read: the token failed.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: "!echo boom >&2; exit 3" } }),
  )

  const since = Date.now()
  const run = await runHook(env, sessionStart("startup", home))
  // The hook carries its refresher's failure line, or names the command as unfinished when its
  // refresher lands after its deadline.
  expect([
    sessionAnswersFailureLine(`${envoyJson}: dispatch.token's command exited with status 3`),
    sessionAnswersFailureLine(notFinishedReason(run)),
  ]).toContain(contextOf(run, "SessionStart"))
  await refresherFinished(state, since)
  expect(dispatch.requests).toEqual([])
  expect(await readdir(tmp)).toEqual([])
}, 60_000)

/** Writes, as a refresher would, what it read for `dispatchUrl` (null: no Dispatch), finished a
 *  minute from now so a hook started now takes it as its own refresher's. */
async function refresherOutcome(state: string, dispatchUrl: string | null, text: string) {
  const directory = join(state, "sessions", SESSION)
  await mkdir(directory, { recursive: true })
  await writeFile(
    join(directory, "answers-refresh.json"),
    JSON.stringify({ finished_at: Date.now() + 60_000, dispatch_url: dispatchUrl, text }),
  )
}

test("with envoy.json unreadable, a list read from another Dispatch never stands in: the failure line names why", async () => {
  const other = standinDispatch()
  const items = [answer("ask-o", "Read from a Dispatch envoy.json no longer names.", 1)]
  other.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  const otherToken = fixtureSecret("dsp")

  // A list read from `other`, in answers.json and as a refresher's fresh outcome.
  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: other.base, token: `!printf ${otherToken}` } }),
  )
  const since = Date.now()
  await runHook(env, sessionStart("startup", home))
  await refresherFinished(state, since)
  const cache = JSON.parse(await readFile(join(state, "sessions", SESSION, "answers.json"), "utf8"))
  expect(cache.dispatch_url).toBe(other.base)
  await refresherOutcome(
    state,
    other.base,
    renderSessionAnswers(items, { now: Date.now(), total: 1 }) as string,
  )

  // envoy.json caught mid-edit: no URL can be read from it.
  await writeFile(envoyJson, '{ "dispatch": ')
  const run = await runHook(env, sessionStart("resume", home))
  const context = contextOf(run, "SessionStart")
  const [opening, closing] = sessionAnswersFailureLine("\u0000").split("\u0000") as [string, string]
  expect(context.startsWith(opening) && context.endsWith(closing)).toBe(true)
  expect(context).toContain(envoyJson)
  expect(context).not.toContain("Read from a Dispatch envoy.json no longer names.")
  expect(context).not.toContain("As read at")
  expectWithinDeadline(run)
  // No refresher started for a configuration that names no Dispatch.
  expect(run.timing.refresher).toBeNull()
  expect(other.requests).toHaveLength(1)
}, 60_000)

test("a list read for the Dispatch envoy.json names stands in even when a refresher read none", async () => {
  const dispatch = standinDispatch()
  const items = [answer("ask-s", "Read for the Dispatch envoy.json names.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  const envoyJson = join(config, "envoy.json")
  const sameToken = fixtureSecret("dsp")

  await writeFile(
    envoyJson,
    JSON.stringify({ dispatch: { serverUrl: dispatch.base, token: `!printf ${sameToken}` } }),
  )
  const firstSince = Date.now()
  await runHook(env, sessionStart("startup", home))
  await refresherFinished(state, firstSince)
  expect(dispatch.requests).toEqual([`Bearer ${sameToken} /api/v1/agents/${SESSION}/answers`])

  // A refresher that ran while envoy.json was broken recorded no Dispatch and an error line.
  await refresherOutcome(state, null, sessionAnswersFailureLine("envoy.json: invalid JSON"))
  // envoy.json names the same Dispatch again, behind a command that waits on the test's gate.
  const gate = join(home, "gate")
  await writeFile(
    envoyJson,
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!while [ ! -e '${gate}' ]; do sleep 0.02; done; printf ${sameToken}`,
      },
    }),
  )
  // The refresher the hook below starts runs the gated command, so it outlives its hook: the case
  // opens the gate, and its scratch waits for that refresher's end.
  cleanup.push(() => writeFile(gate, ""))
  const run = await runHook(env, sessionStart("resume", home))
  const context = contextOf(run, "SessionStart")

  // The list read for this Dispatch stands in, naming why this read gave none; never the stale
  // error line of a refresher that read no Dispatch.
  expect(allowedOutcome(context, items, /not the Dispatch envoy\.json names now$/)).toBe("cached")
  expect(context).toContain("the refresher read no Dispatch, not the Dispatch envoy.json names now")
  expect(context).not.toContain("invalid JSON")
  expectWithinDeadline(run)
}, 60_000)

test("hooks started together run a slow token command once", async () => {
  const dispatch = standinDispatch()
  dispatch.route = { kind: "list", items: [answer("ask-6", "One run.", 1)] }
  const { home, state, config, tmp } = await scratch()
  const env = { HOME: home, DISPATCH_STATE_DIR: state, CLAUDE_CONFIG_DIR: config, TMPDIR: tmp }
  // The command counts its runs, then waits until the test opens the gate.
  const runs = join(home, "runs")
  const gate = join(home, "gate")
  await writeFile(
    join(config, "envoy.json"),
    JSON.stringify({
      dispatch: {
        serverUrl: dispatch.base,
        token: `!printf 'run\\n' >> '${runs}'; while [ ! -e '${gate}' ]; do sleep 0.02; done; printf ${DISPATCH_TOKEN}`,
      },
    }),
  )
  cleanup.push(() => writeFile(gate, ""))

  const hooks = await Promise.all([
    runHook(env, sessionStart("startup", home)),
    runHook(env, {
      session_id: SESSION,
      cwd: home,
      hook_event_name: "SubagentStart",
      agent_id: "agent-1",
      agent_type: "general-purpose",
    }),
    runHook(env, {
      session_id: SESSION,
      cwd: home,
      hook_event_name: "SubagentStart",
      agent_id: "agent-2",
      agent_type: "general-purpose",
    }),
  ])
  for (const hook of hooks) expect(hook.exitCode).toBe(0)
  // The gate opens once one refresher the hooks started holds the lock and every other one has
  // ended: each tried for the lock while the command ran, however late its hook or its own
  // start-up was, so a second run of the command is a refresher that ran it without the lock.
  await refresherHoldsLock(state)
  await writeFile(gate, "")
  await refreshersEnded()
  expect(await readFile(runs, "utf8")).toBe("run\n")
  expect(dispatch.requests).toHaveLength(1)
  expect(await readdir(tmp)).toEqual([])
}, 60_000)

test("a hook input it cannot read adds nothing and still exits 0", async () => {
  const { home } = await scratch()
  const run = await runHook(
    { HOME: home, DISPATCH_URL: "http://127.0.0.1:9", DISPATCH_TOKEN: "t" },
    {
      hook_event_name: "SessionStart",
    },
  )
  expect(run.exitCode).toBe(0)
  expect(run.stdout).toBe("")
  expect(run.stderr).toContain("envoy: people's answers were not added")
}, 60_000)

test("a hook whose start-up took past 800 ms still gives Dispatch its least wait, never none", async () => {
  // Claude Code starts the open-asks, dispatch-first and answers hooks at once beside itself, so on
  // a loaded host the answers hook's own bun start-up can pass 800 ms before its code runs. The
  // preload stands in for that start-up: it holds the process until 820 ms after it started.
  // Dispatch then answers at once, so the session gets the list Dispatch holds now, or, when even
  // the least wait is missed, the failure line naming that wait: never "did not answer within 0 ms".
  const dispatch = standinDispatch()
  const items = [answer("ask-slow", "SLOW-START-ANSWER: written before this resume.", 1)]
  dispatch.route = { kind: "list", items }
  const { home, state } = await scratch()
  const preload = join(home, "slow-start.ts")
  await writeFile(preload, "while (performance.now() < 820) {}\n")
  const env = {
    HOME: home,
    DISPATCH_STATE_DIR: state,
    DISPATCH_URL: dispatch.base,
    DISPATCH_TOKEN,
  }
  const run = await runHook(env, sessionStart("resume", home), { preload })

  // On a quiet host the read lands and the list is carried. On a loaded one the stand-in can miss
  // even the 150 ms least wait, and the design then allows the failure line; what it never allows
  // is a read given no wait at all, which this case exists to catch ("within 0 ms").
  const outcome = allowedOutcome(contextOf(run, "SessionStart"), items, READ_MISSED_LEAST_WAIT)
  expect(run.timing.began).toBeGreaterThanOrEqual(820)
  expectWithinDeadline(run)
  // A read that missed its wait may not have reached the stand-in before the hook exited.
  const read = `Bearer ${DISPATCH_TOKEN} /api/v1/agents/${SESSION}/answers`
  if (outcome === "fresh") expect(dispatch.requests).toEqual([read])
  else expect(dispatch.requests.filter((request) => request !== read)).toEqual([])
}, 60_000)
