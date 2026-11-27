import { randomUUID } from "node:crypto"
import { readFileSync, renameSync, rmSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import type { LegionOwed } from "@legion/contracts/legion-api"
import { messageFor } from "@legion/envoy-client/errors"
import type { ActivityInput, OwedInput } from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import {
  createPhaseReminder,
  type EnvoyPublish,
  type PhaseReminderLog,
} from "@legion/pi-shared/phase-reminder"
import { type PhaseStallEntry, phaseStallEntryFrom } from "@legion/pi-shared/phase-stall"
import {
  CLAUDE_BACKGROUND_TASK_KIND,
  reportActivityBetweenTurns,
  reportActivityWithin,
  sessionActivity,
  sessionActivityRequest,
} from "@legion/pi-shared/session-activity"
import { stallLimits } from "@legion/pi-shared/stall-limits"
import { workspaceRecreated, workspaceRecreatedNotice } from "@legion/pi-shared/workspace-recreated"
import { z } from "zod"
import { configuredValue } from "./claude-session"

/**
 * The endless phase reminder's Claude Code host (INFRA-220). A Claude Code phase worker runs no Oh
 * My Pi process, so the machine `packages/pi-shared/src/phase-reminder.ts` holds — the one
 * `packages/pi-legion/extensions/legion.ts` drives from Oh My Pi's session hooks — is driven here
 * instead, from Claude Code's own hooks: each fires an `mcp_tool` hook against `legion_phase_hook`
 * on the pane's `legion` MCP server, the server this file's endpoint is registered on
 * (`./envoy-channel-server`). Nothing of the machine is re-decided here.
 *
 * The record lives in a file rather than a transcript entry, `<CLAUDE_CONFIG_DIR>/
 * legion-phase-stall-<session id>.json`. Nothing else writes that file: the Go bridge, which
 * wrote it until this change made it pure transport, is now only what carries a line into the
 * session. The path and the atomic rename are kept as the bridge left them, so a session carried
 * across the cutover from the build now in the pods still reads its record.
 *
 * The first reminder of a run is the `Stop` hook's `block`, which Claude Code turns into one more
 * turn of the same session. Every one after it is written into the session over the bridge's
 * inbox, from a timer this server arms at a turn's end: a line written while a turn is running is
 * appended to that turn as its next step (`deliver`/`step` in
 * `packages/daemon/internal/claudebridge/bridge.go`), which would destroy the turn boundary the
 * whole split exists for.
 *
 * A turn Claude Code ended on an API error fires `StopFailure` rather than `Stop`, and that is
 * the provider refusing the turn: it earns no continuation, since the host would turn one into
 * another turn at once and the provider would refuse that one the same way. It does arm one
 * timer, because the task that refused turn carried ended with it and nothing else would wake
 * this worker, so a single transient refusal would otherwise leave the issue idle. What stops
 * the reminding for good is the runner's owed check: once the daemon has failed this role's
 * claim it answers that the claim owes nothing, and every reminder is dropped. The bridge reports
 * each turn's outcome to the daemon, with the status Claude Code named, and marks a turn no prompt
 * of the daemon's ran — a reminder written over the inbox — so the daemon judges that turn from
 * the report (dispatch://INFRA-4508): a refusal whose status cannot clear fails the claim at once
 * and holds the issue, and a refusal that can clear fails nothing, so the reminders keep their
 * pace. At six such turns in a row the daemon sends the `provider-errors` notice once
 * (`providerErrorRunReached` in the daemon's `workflow.go` says to whom).
 */

/** The tool's name: Claude Code shows it as `mcp__legion__legion_phase_hook`, and a hook entry
 * names it as the bare `legion_phase_hook` on the server `legion`. */
export const PHASE_HOOK_TOOL_NAME = "legion_phase_hook"

/** What the tool says it is to the model that sees it in the tool list, and what a call this
 * endpoint does not recognise is answered with. A model that calls the tool anyway must not be
 * able to move the state, so that answer records nothing. */
export const PHASE_HOOK_REFUSAL =
  "legion_phase_hook is Claude Code's hook endpoint for Legion's phase reminder: Claude Code's " +
  "own hooks call it with the event they fire for, and it does nothing for a call the model " +
  "makes. Do not call it."

/** The events the five generated hook entries fire (`legion/hooks/hooks.json`). */
const HOOK_EVENTS = [
  "Stop",
  "StopFailure",
  "PostToolUse",
  "PostToolUseFailure",
  "UserPromptSubmit",
] as const

/** One hook call's arguments: the event, and that event's own fields. Claude Code substitutes
 * `${…}` as strings, so every field is one — `stopHookActive` ("true"/"false") and
 * `backgroundTasks` (the JSON array, as text) included. */
const hookArguments = z.object({
  event: z.enum(HOOK_EVENTS),
  lastAssistantMessage: z.string().optional(),
  stopHookActive: z.string().optional(),
  backgroundTasks: z.string().optional(),
  toolName: z.string().optional(),
  agentId: z.string().optional(),
  prompt: z.string().optional(),
})

/** The `${background_tasks}` payload: one entry per task of the session, of which only the status
 * decides anything here. Claude Code 2.1.310 writes each as
 * `{ id, type, status, description, command }`. */
const backgroundTaskList = z.array(z.object({ status: z.string() }).loose())

/**
 * Whether the turn now ending leaves background work of this session's own still running, from
 * `${background_tasks}` in the `Stop` hook's input. Claude Code substitutes it as that JSON array
 * rendered to text, and as `[]` when there are none; measured on 2.1.310 against a `Bash` call
 * with `run_in_background: true`, whose task reads `"status":"running"` at the `Stop` of the turn
 * that started it.
 *
 * A status other than `running` — a task that finished inside the turn — is no reason to keep the
 * reminder back, and an argument an older binary does not substitute at all is read as no
 * background work, which is the behaviour before this field existed.
 */
function backgroundWorkRunning(tasks: string | undefined): boolean {
  if (tasks === undefined || tasks.trim() === "") return false
  let parsed: unknown
  try {
    parsed = JSON.parse(tasks)
  } catch {
    return false
  }
  const list = backgroundTaskList.safeParse(parsed)
  return list.success && list.data.some((task) => task.status === "running")
}

/** The tool as `tools/list` declares it. */
export function phaseHookToolDefinition() {
  return {
    name: PHASE_HOOK_TOOL_NAME,
    description: PHASE_HOOK_REFUSAL,
    inputSchema: z.toJSONSchema(hookArguments),
  }
}

/** One hook call's answer, as the tool's one JSON text block: `{}` when there is nothing to say,
 * `{"decision":"block","reason":…}` on the `Stop` that carries a reminder — which Claude Code
 * turns into one more turn of the same session, holding the reason — or the refusal. */
export interface PhaseHookAnswer {
  readonly decision?: "block"
  readonly reason?: string
  readonly error?: string
}

export interface PhaseHookEndpoint {
  /** Answers one hook call, with the record written before the answer. */
  readonly handle: (input: unknown) => Promise<PhaseHookAnswer>
  /** The `legion` tool's `ready_for_update` returned without throwing: the worker has pushed
   * what it wants kept and waits for the process its claim runs in to be replaced, and the `Stop`
   * of the turn it answered in must block nothing: the work a continuation did would go with the
   * pod, and the daemon would see no turn end. The CallTool handler of the same server calls it
   * (`./envoy-channel-server`), as `onUpdateAnswered` does on the Oh My Pi host. */
  readonly updateAnswered: () => void
  /** Drops the armed timer, so a 15-minute WAITING wait never holds a shutting-down server
   * open (the server's own `stop`). */
  readonly stop: () => void
}

export interface PhaseHookDeps {
  /** The record's file (`phaseStallFile`). */
  readonly stateFile: string
  /** The pane's registered claim, or undefined while its boot is still registering. */
  readonly claim: () => ClaimCapability | undefined
  /** Settles once the pane's claim boot has run (`startLegionClaim`'s `booted`). The resume
   * reminder and the recreated-workspace notice both need the claim, and neither is sent before
   * one exists. Never rejects. */
  readonly booted: Promise<void>
  /** A turn of this session has started, whatever started it: the daemon's task, an Envoy
   * delivery, a reminder, a person. The channel server answers it by firing the claim boot's
   * deferred role claim (`startLegionClaim`'s `onFirstTurn`), so the session takes its Envoy
   * name inside a turn that is already running (INFRA-1573). It is called for every
   * `UserPromptSubmit`, and the server decides which of them counts: one before the boot
   * registered its step — the bridge's seeding `/rename` — signals nothing, and one after the
   * name is taken costs nothing. */
  readonly firstTurnStarted: () => void
  /** What this session's claim owes now (`LegionDaemonClient.owed`), the one answer that decides
   * whether a reminder goes out, aborted with `signal` once the check's bound passes
   * (`PhaseReminderDeps.readOwed`). An answer that does not arrive is the shared reminder's to
   * decide (`UNREAD_CHECK`, `@legion/pi-shared/phase-reminder`), as it is on Oh My Pi: it reminds
   * no one. */
  readonly readOwed: (input: OwedInput, signal: AbortSignal) => Promise<LegionOwed>
  /** Sends this session's activity report to the daemon (`LegionDaemonClient.activity`), so the
   * idle rule can decide whether the pane's pod may be released (INFRA-945). The endpoint sends one
   * at each `Stop`, with the background work that `Stop`'s `${background_tasks}` shows, and one
   * when a timer clears the reminder wake, with the launch's reading of its background work
   * (`sendActivity`), since no other hook this host runs is shown the session's background tasks.
   * The timer's report goes on a fresh launch too, whose reading is no background work: a claim
   * never reported holds its pod. The request is to abort with `signal`, which is aborted once the
   * report's bound passes without an answer, so a daemon that hangs is left no open request. The
   * `Stop` report's bound is `ACTIVITY_REPORT_BOUND_MS` (`reportActivityWithin`), and the bound of
   * the report a timer sends after it clears the reminder wake is `ACTIVITY_BETWEEN_TURNS_BOUND_MS`
   * (`reportActivityBetweenTurns`), both in `@legion/pi-shared/session-activity`. */
  readonly reportActivity: (input: ActivityInput, signal: AbortSignal) => Promise<void>
  /** Writes one line into the session over the bridge's inbox, which turns it into a user turn,
   * and resolves once the bridge has queued it. Undefined for a session outside a Legion pane,
   * where nothing can reach the worker between its turns. */
  readonly inbox: ((text: string) => Promise<void>) | undefined
  /** The server's own Envoy client's `publish`, or undefined when Envoy is off for the session:
   * the architect is then not told, and the count still climbs. */
  readonly publish: EnvoyPublish | undefined
  /** The worker's environment: `LEGION_PROJECT`, the four `LEGION_STALL_*` numbers and
   * `LEGION_WORKSPACE_RECREATED`. */
  readonly env: Readonly<Record<string, string | undefined>>
  readonly log: PhaseReminderLog
}

/** `<CLAUDE_CONFIG_DIR>/legion-phase-stall-<session id>.json`, the path the Go bridge built while
 * it kept this record itself: kept as it was, so a session carried across the cutover from the
 * build now in the pods finds the record where it left it. */
export function phaseStallFile(configDir: string, sessionID: string): string {
  return join(configDir, `legion-phase-stall-${sessionID}.json`)
}

/** `PhaseReminderLog` over the pane's own log (`logLegion`, `./legion-claim`): one line each,
 * with its context as JSON. */
export function paneReminderLog(write: (line: string) => void): PhaseReminderLog {
  const line = (level: string, message: string, context?: Record<string, unknown>): void => {
    write(
      context === undefined
        ? `${level}: ${message}`
        : `${level}: ${message} ${JSON.stringify(context)}`,
    )
  }
  return {
    debug: (message, context) => line("debug", message, context),
    warn: (message, context) => line("warn", message, context),
  }
}

/**
 * What this host's state file holds: the record the machine shares with the Oh My Pi host, and the
 * keys only this host keeps. `PhaseStallEntry` is the machine's record and both hosts read it, so
 * this host's keys are composed around it here rather than added to it. Each key is written only
 * while it says something (`writePhaseStallRecord`), and a missing key reads as saying nothing, so
 * a record the Go bridge left behind, which names neither, still reads, and so does one an earlier
 * build wrote before `backgroundReading` existed.
 */
export interface PhaseStallRecord {
  /** The reminder's rhythm and count the file holds, or undefined when it holds none this build
   * knows. */
  readonly entry: PhaseStallEntry | undefined
  /** Whether this launch's recreated-workspace notice has already been written into the session
   * (`recreatedNoticeSent` in the file); false for a record that names no such key. It outlives
   * only a server Claude Code restarts inside one `claude` process: a launch that recreated
   * nothing clears it (`createPhaseHookEndpoint`). */
  readonly recreatedNoticeSent: boolean
  /** A launch's reading of its own background work (`backgroundReading` in the file), or
   * undefined for a record that holds none. A reading that says neither background work nor a
   * tool since is held as none and never written: none is what a fresh launch reads as, and a
   * report from either says the same, no background work (`createPhaseHookEndpoint`). */
  readonly backgroundReading: BackgroundReading | undefined
}

/**
 * What one launch of this session last knew of its own background work, kept in the record so a
 * server Claude Code restarts inside the same `claude` process reads it again. Claude Code shows a
 * hook the session's background tasks at `Stop` and `SubagentStop` only (`${background_tasks}`;
 * not at `StopFailure`, Claude Code v2.1.145, anthropics/claude-code issue 60692), so a restarted
 * server has read none, and a task the last `Stop` saw may still be running.
 */
export interface BackgroundReading {
  /** The claim's launch generation the reading was taken in (`ClaimCapability.generation`). A
   * server Claude Code restarts keeps its claim's generation, and a launch moves it, which also
   * clears the daemon's stored report (`packages/daemon/internal/store/claims.go`, putClaim): a
   * reading of another generation describes a process the launch replaced, and no task of that
   * process outlives it. */
  readonly generation: number
  /** Whether that launch's last turn end left background work of its own running. */
  readonly backgroundWork: boolean
  /** Whether a tool has run since that turn end, the worker's or a subagent's: a tool call may
   * have started a background task that only the next `Stop` can see. */
  readonly toolSince: boolean
}

/** `BackgroundReading` as the file holds it: a key of any other shape is no reading. */
const recordedBackgroundReading = z.object({
  generation: z.number().int(),
  backgroundWork: z.boolean(),
  toolSince: z.boolean(),
})

/** A file this session holds no record in: a fresh session, an unreadable one, or one written by
 * a build whose state this one does not know. */
const NO_RECORD: PhaseStallRecord = {
  entry: undefined,
  recreatedNoticeSent: false,
  backgroundReading: undefined,
}

/**
 * The record `file` holds: a missing file is no record at all, so a fresh session restores
 * nothing. The entry itself is assembled where the Oh My Pi host's is
 * (`phaseStallEntryFrom`), so a record an earlier build wrote — `{"state":"open"}` or
 * `{"state":"closed"}` with its count, `{"state":"quiet"}` or `{"state":"text-call"}` — reads as
 * the rhythm's `active` here too, and a missing `count`, `reported` or `reportKey` reads as 0,
 * false and absent. This host adds the warning for a record it understands none of, and its own
 * two keys: `recreatedNoticeSent`, false unless the file says true, and `backgroundReading`, none
 * unless the file holds one of its shape (`recordedBackgroundReading`), so a record written before
 * the key existed reads as a launch that has started no background work.
 */
export function readPhaseStallRecord(file: string, log: PhaseReminderLog): PhaseStallRecord {
  let raw: string
  try {
    raw = readFileSync(file, "utf8")
  } catch (error) {
    const missing =
      typeof error === "object" && error !== null && "code" in error && error.code === "ENOENT"
    if (!missing) {
      log.warn("legion: the phase-stall state could not be read; this session starts afresh", {
        file,
        error: messageFor(error),
      })
    }
    return NO_RECORD
  }
  let data: unknown
  try {
    data = JSON.parse(raw)
  } catch (error) {
    log.warn("legion: the phase-stall state is not JSON; this session starts afresh", {
      file,
      error: messageFor(error),
    })
    return NO_RECORD
  }
  if (typeof data !== "object" || data === null) {
    log.warn("legion: the phase-stall state is not an object; this session starts afresh", { file })
    return NO_RECORD
  }
  const entry = phaseStallEntryFrom(data)
  if (entry === undefined) {
    log.warn("legion: the phase-stall state holds no state this build knows; starting afresh", {
      file,
    })
    return NO_RECORD
  }
  const reading = recordedBackgroundReading.safeParse(
    "backgroundReading" in data ? data.backgroundReading : undefined,
  )
  return {
    entry,
    recreatedNoticeSent: "recreatedNoticeSent" in data && data.recreatedNoticeSent === true,
    backgroundReading: reading.success ? reading.data : undefined,
  }
}

/** Replaces `file` with `entry` and this host's own keys, through a temporary file in the same
 * directory renamed into place, so a process killed mid-write never leaves half a record to read:
 * the shape the Go bridge wrote the same file in while it kept this record, kept so a session
 * carried across the cutover reads whatever the build now in the pods left. Each key is written
 * only while it says something, `recreatedNoticeSent` once it is true and `backgroundReading`
 * while there is one, so a record this host writes is otherwise the shared entry and nothing more.
 * A write that fails is logged rather than thrown: the worker still gets its reminder. */
export function writePhaseStallRecord(
  file: string,
  entry: PhaseStallEntry,
  keys: Omit<PhaseStallRecord, "entry">,
  log: PhaseReminderLog,
): void {
  const temporary = join(dirname(file), `.legion-phase-stall-${randomUUID()}`)
  try {
    writeFileSync(
      temporary,
      JSON.stringify({
        ...entry,
        ...(keys.recreatedNoticeSent ? { recreatedNoticeSent: true } : {}),
        ...(keys.backgroundReading === undefined
          ? {}
          : { backgroundReading: keys.backgroundReading }),
      }),
      { mode: 0o600 },
    )
    renameSync(temporary, file)
  } catch (error) {
    rmSync(temporary, { force: true })
    log.warn("legion: the phase-stall state is not recorded for a relaunch", {
      file,
      error: messageFor(error),
    })
  }
}

/**
 * The endpoint the pane's `legion_phase_hook` tool calls, over this session's own record, which is
 * restored here before any hook can be answered.
 */
export function createPhaseHookEndpoint(deps: PhaseHookDeps): PhaseHookEndpoint {
  const limits = stallLimits(deps.env, deps.log, configuredValue(deps.env["LEGION_ROLE"]))
  const restored = readPhaseStallRecord(deps.stateFile, deps.log)
  // The machine's entry and this host's own keys are one record, so the last entry written is
  // kept for the writes the notice and the background reading make.
  let entry: PhaseStallEntry = restored.entry ?? { state: "active", count: 0, reported: false }
  let recreatedNoticeSent = restored.recreatedNoticeSent
  /** The last background reading written to the record (`BackgroundReading`), of this launch or
   * of the one before it: `readingOf` tells which. Undefined while none says anything
   * (`keepReading`). */
  let reading = restored.backgroundReading
  const saveRecord = (next: PhaseStallEntry): void => {
    entry = next
    writePhaseStallRecord(
      deps.stateFile,
      next,
      { recreatedNoticeSent, backgroundReading: reading },
      deps.log,
    )
  }
  // `recreatedNoticeSent` exists only so a server Claude Code restarts inside one `claude`
  // process, whose environment is still its launch's, does not repeat that launch's notice. The
  // launcher decides `LEGION_WORKSPACE_RECREATED` afresh for every launch, so a launch that
  // recreated nothing clears it, and the next launch that recreates the workspace again is told
  // again.
  if (recreatedNoticeSent && !workspaceRecreated(deps.env)) {
    recreatedNoticeSent = false
    saveRecord(entry)
  }

  /** The one timer this server has armed, or undefined when none is. One at a time: arming the
   * next drops it, and so does the server's `stop`. */
  let timer: ReturnType<typeof setTimeout> | undefined
  const dropTimer = (): void => {
    if (timer === undefined) return
    clearTimeout(timer)
    timer = undefined
  }
  const armTimer = (callback: () => void | Promise<void>, delayMs: number): void => {
    dropTimer()
    timer = setTimeout(() => {
      timer = undefined
      void callback()
    }, delayMs)
  }

  /** The reminder last written to the inbox: the prompt that carries it back is this endpoint's
   * own reminder, and a reminder must never reset the count it just moved. */
  let lastReminder: string | undefined
  /** Writes `text` into the session over the bridge's inbox, and settles as the inbox does: it
   * rejects when the bridge did not queue the line. A session with no inbox says so and writes
   * nothing, and that is no failure: no later attempt could reach the worker either. */
  const writeToSession = async (text: string, what: string): Promise<void> => {
    const { inbox } = deps
    if (inbox === undefined) {
      deps.log.warn(`legion: ${what} has no way to reach this worker: this session has no inbox`, {
        text,
      })
      return
    }
    await inbox(text)
  }

  // The inbox's failure goes back to the runner, which logs it, uncounts the reminder and arms
  // the next one: no turn starts on a line the bridge did not take, so no `Stop` would.
  const sendReminder = (text: string): Promise<void> => {
    lastReminder = text
    return writeToSession(text, "a phase reminder")
  }

  /**
   * What `claim`'s launch last knew of its own background work, which the report a timer sends
   * between turns repeats (`wakeChanged`): of the hooks this host runs, Claude Code shows the
   * session's background tasks to `Stop` alone, so nothing between turns can read them again.
   *
   * A reading of the claim's generation was taken by this launch, by this server or by the one
   * before it that Claude Code restarted inside this `claude` process, whose claim keeps its
   * generation: a task the last `Stop` saw may still be running, so that reading stands. A launch
   * whose record holds no reading or one of another generation is a fresh one, and its reading is
   * no background work and no tool since: no task of the process the launch replaced outlives that
   * process, only a turn of this process can start one, and every turn of it ends in `Stop` or
   * `StopFailure`, each of which takes this generation's reading (`keepReading`).
   */
  const readingOf = (claim: ClaimCapability): Omit<BackgroundReading, "generation"> =>
    reading?.generation === claim.generation ? reading : { backgroundWork: false, toolSince: false }
  /** Makes `backgroundWork` and `toolSince` the reading of `claim`'s launch and writes it into the
   * record at once, so a server Claude Code restarts after this call reads it again. A reading
   * that says neither is kept as none: it reads the same (`readingOf`), and the record stays the
   * shared entry alone. */
  const keepReading = (
    claim: ClaimCapability,
    backgroundWork: boolean,
    toolSince: boolean,
  ): void => {
    reading =
      backgroundWork || toolSince
        ? { generation: claim.generation, backgroundWork, toolSince }
        : undefined
    saveRecord(entry)
  }
  const reminder = createPhaseReminder({
    // The bridge turns one inbox line into a user turn, which is what every reminder after the
    // `Stop` continuation is.
    sendReminder,
    armTimer,
    // The record lands before the hook answers: every change of it is this synchronous write, and
    // the machine makes it inside `settle` before that promise resolves. Claude Code may restart
    // this server inside the same `claude` process, and a reminder the worker was given but whose
    // count was never recorded is what the count exists to catch.
    saveEntry: saveRecord,
    claim: deps.claim,
    readOwed: deps.readOwed,
    // A timer whose check decided not to remind has cleared the wake. This host otherwise reports
    // only at `Stop`, and no turn starts whose `Stop` would carry the change, so the report goes
    // now: a reported wake that is due and past holds the pod from its idle release. It goes on a
    // fresh launch too, before any `Stop`: the launch cleared the daemon's stored report, and a
    // first turn that ended in `StopFailure` sent none, so without this one the idle release would
    // hold the pod as unreported (`IdleHoldUnreported`) until a turn that may never come. Its
    // background work is the launch's reading (`readingOf`). No handler waits for this report, so
    // it goes under the bound for a report between turns.
    wakeChanged: () => {
      const claim = deps.claim()
      if (claim === undefined) return
      void sendActivity(claim, readingOf(claim).backgroundWork, reportActivityBetweenTurns)
    },
    // The server's own listener client, so the notice reaches the one this session already talks
    // to; Envoy off for the session leaves it undefined, the runner says so once at debug, and
    // the reminder keeps coming without a notice.
    publisher: async () => deps.publish,
    project: configuredValue(deps.env["LEGION_PROJECT"]),
    limits,
    log: deps.log,
  })
  reminder.restore(restored.entry)

  /**
   * INFRA-945: what this session is doing, reported to the daemon so the idle rule can decide
   * whether the pane's pod may be released. Of the hooks this host runs, Claude Code shows the
   * session's background tasks to `Stop` alone (`${background_tasks}`), so this host reports at
   * each `Stop`, saying what that `Stop` read, and between turns only when a timer's check cleared
   * the reminder wake (`wakeChanged`), repeating the launch's reading of what its last turn end
   * left running (`readingOf`), since nothing between turns can read the tasks again. The one kind
   * it can name is a Claude Code background task.
   *
   * A report that does not reach the daemon is logged and dropped: the turn's reminder is what
   * the hook owes its worker, and a claim with no report after its last turn is held rather than
   * released. The caller chooses the report's bound (`bounded`). The `Stop` report goes under
   * `ACTIVITY_REPORT_BOUND_MS` (`reportActivityWithin`), because the `Stop` hook awaits it: a
   * daemon that does not answer holds the turn end here for at most 3 s. A report that bound cuts
   * off leaves the claim unreported until its next report. No handler waits for the report
   * `wakeChanged` sends, so it goes under `ACTIVITY_BETWEEN_TURNS_BOUND_MS`
   * (`reportActivityBetweenTurns`): a daemon slower than 3 s still takes it.
   */
  const sendActivity = async (
    claim: ClaimCapability,
    backgroundWork: boolean,
    bounded: typeof reportActivityWithin,
  ): Promise<void> => {
    const request = sessionActivityRequest({
      project: configuredValue(deps.env["LEGION_PROJECT"]),
      claim,
      report: sessionActivity(
        backgroundWork ? [CLAUDE_BACKGROUND_TASK_KIND] : [],
        reminder.phaseWake(),
      ),
    })
    if (request === undefined) return
    try {
      await bounded(deps.reportActivity, request)
    } catch (error) {
      deps.log.warn("legion: this session's activity report did not reach the daemon", {
        issue: claim.issue,
        error: messageFor(error),
      })
    }
  }

  /**
   * D29: a worker whose launcher recreated its workspace is told so, as an Oh My Pi one is, over
   * the same inbox the reminders use. Once per launch: the record it is written in is the one the
   * reminder's rhythm and count live in, so a server Claude Code restarts inside the same
   * `claude` process does not repeat it, and a launch that recreated nothing clears it again. The
   * record lands before the line does — a notice a broken socket loses is one notice lost, while
   * one written twice is the thing it must not be — and the turn it starts is an inbound message
   * like any other, which says nothing of what the claim owes.
   */
  const sendRecreatedNotice = (issue: string): void => {
    if (recreatedNoticeSent) return
    const notice = workspaceRecreatedNotice(deps.env, issue)
    if (notice === undefined) return
    recreatedNoticeSent = true
    saveRecord(entry)
    void writeToSession(notice.content, "the recreated-workspace notice").catch(
      (error: unknown) => {
        deps.log.warn("legion: the recreated-workspace notice did not reach this worker", {
          error: messageFor(error),
        })
      },
    )
  }

  void deps.booted.then(() => {
    const claim = deps.claim()
    if (claim === undefined) return
    sendRecreatedNotice(claim.issue)
    // D10: a worker the daemon relaunched is reminded from a timer, since nothing else would wake
    // it, and the daemon's answer when it is due says whether anything is owed. The guard drops
    // that timer when a turn started meanwhile, which is exactly the case to drop — the daemon
    // re-sending an interrupted task arrives as a `UserPromptSubmit`.
    if (claim.role !== "architect") reminder.armResumeReminder()
  })

  return {
    updateAnswered: () => reminder.apply({ kind: "update-answered" }),
    stop: dropTimer,
    handle: async (input) => {
      const parsed = hookArguments.safeParse(input)
      // An event this endpoint does not know, or none at all, is the model calling the tool
      // itself: it is refused, and no state moves, so it cannot zero its own count.
      if (!parsed.success) return { error: PHASE_HOOK_REFUSAL }
      const event = parsed.data
      // Before the gate below, and before anything else this endpoint does: the claim's own
      // first-turn signal is not the reminder's business, and a turn is a turn whether or not a
      // claim has registered yet (the bridge's seeding `/rename` turn runs before one has) and
      // whatever role this pane holds, an architect's included. What a prompt before the boot
      // registered its step comes to is the server's to decide, not this endpoint's: it holds
      // the step the signal runs (`src/envoy-channel-server.ts`).
      if (event.event === "UserPromptSubmit") deps.firstTurnStarted()
      // The same gate as `phaseWorkerSession` in `packages/pi-legion/extensions/legion.ts`: the
      // machine runs for a phase worker whose claim has booted, and for nobody else. The bridge's
      // seeding `/rename` turn is what writes the transcript the claim boot waits for
      // (`waitForTranscript`, `./legion-claim`), so it runs before any claim exists, answers `{}`
      // here, and is never reminded.
      const claim = deps.claim()
      if (claim === undefined || claim.role === "architect") return {}
      switch (event.event) {
        case "Stop": {
          // `stop_hook_active` arrives as "true" once this hook has already continued the turn. It
          // decides nothing: the count is what bounds a run of reminders. `background_tasks` does
          // decide: a turn that ends with a background task of the worker's own still running is
          // a worker waiting to be woken by it, not a stalled one.
          const backgroundWork = backgroundWorkRunning(event.backgroundTasks)
          // The launch's reading is what this `Stop` read, with no tool since, and it is on disk
          // before the settle, so a server Claude Code restarts after this turn end repeats it
          // rather than reading a fresh launch's no background work.
          keepReading(claim, backgroundWork, false)
          deps.log.debug("legion: a Claude Code phase worker's turn is settling", {
            issue: claim.issue,
            role: claim.role,
            stopHookActive: event.stopHookActive ?? "",
            backgroundWork,
          })
          const reminded = await reminder.settle({
            kind: "settle",
            lastAssistantText: event.lastAssistantMessage ?? "",
            backgroundWork,
          })
          // After the settle, so the report carries the wake that settle armed. The daemon
          // releases no claim whose last report is older than its last turn, so it goes at every
          // settle, whatever it says.
          await sendActivity(claim, backgroundWork, reportActivityWithin)
          return reminded === undefined ? {} : { decision: "block", reason: reminded }
        }
        case "StopFailure": {
          // Claude Code runs this instead of `Stop` when the turn ended on an API error, and
          // `${last_assistant_message}` is then the error text itself rather than anything the
          // worker said ("Hooks reference", code.claude.com/docs/en/hooks). So this is the provider
          // refusing the turn, and the machine takes it as one: it earns no continuation, because
          // the next turn would be refused the same way, and one timer, because nothing else would
          // wake this worker once the task that turn carried ended with it. What ends the reminding
          // is the daemon failing the claim, after which the timer's own owed check is answered
          // that the claim owes nothing: it fails one at once for a refusal whose status cannot
          // clear, the status the bridge reports beside the turn's outcome, a reminder's own turn
          // included, and never for a refusal that can clear, of which the daemon sends the
          // `provider-errors` notice at six in a row (dispatch://INFRA-4508;
          // `providerErrorRunReached` in the daemon says to whom).
          // Claude Code ignores whatever this hook returns, which costs nothing now that there
          // is nothing to return.
          //
          // `StopFailure` carries no `${background_tasks}` (Claude Code v2.1.145;
          // anthropics/claude-code issue 60692), so what this turn end leaves running comes from
          // the launch's reading: what the launch's last turn end left running stands, and a
          // refused turn that ran a tool counts as leaving background work, since that tool may
          // have started a task. What that costs: the report the timer then sends holds the pod
          // as background work (`IdleHoldBackgroundWork`, `packages/daemon/internal/workflow/
          // idle.go`) until the next `Stop`, which a finishing task starts itself, and past
          // `worker_background_hours` the daemon tells the architect. A refused turn that ran no
          // tool started no task: its report repeats what the last turn end left running, and
          // when that was nothing, as on a fresh launch, it lets the idle release take the pod.
          const before = readingOf(claim)
          keepReading(claim, before.backgroundWork || before.toolSince, false)
          await reminder.settle({ kind: "refused", message: event.lastAssistantMessage ?? "" })
          return {}
        }
        case "PostToolUse":
        case "PostToolUseFailure": {
          // A tool call may have started a background task, which only the next `Stop` can see:
          // the launch's reading notes that a tool has run since its last turn end, so a turn
          // that `StopFailure` ends counts it. A subagent's call counts as the worker's does,
          // since it may start a task as well. The record is written only when the note changes,
          // at the first call after a turn end.
          const before = readingOf(claim)
          if (!before.toolSince) keepReading(claim, before.backgroundWork, true)
          // The worker is working, whatever it is working on, and an attempted call that failed
          // is work too. `${agent_id}` substitutes to "" in the main conversation and to a
          // non-empty id for a subagent (measured on the real binary), so a helper agent's call
          // is not this worker answering its reminder.
          if ((event.agentId ?? "") !== "") return {}
          reminder.apply({ kind: "tool-call" })
          return {}
        }
        case "UserPromptSubmit": {
          // D7: the only sight this server has of an inbound turn. The daemon's task reaches
          // Claude Code's stdin from the bridge and never this process, so a turn is read from the
          // prompt itself: this endpoint's own reminder, or an inbound message, which starts the
          // count again and says nothing of what the claim owes. The turn is declared first,
          // which makes every armed timer stale, and the pending one is dropped outright: the
          // runner's generation guard answers the same for the timers it armed, and dropping the
          // one timer this server holds is the plainest way to say the same thing.
          reminder.turnStarted()
          dropTimer()
          const prompt = event.prompt ?? ""
          if (prompt === lastReminder) return {}
          reminder.apply({ kind: "inbound" })
          return {}
        }
      }
    },
  }
}
