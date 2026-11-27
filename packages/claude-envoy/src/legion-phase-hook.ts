import { randomUUID } from "node:crypto"
import { readFileSync, renameSync, rmSync, writeFileSync } from "node:fs"
import { dirname, join } from "node:path"
import type { LegionOwed, LegionWaitNews } from "@legion/contracts/legion-api"
import { messageFor } from "@legion/envoy-client/errors"
import type { ActivityInput, OwedInput, WaitNewsInput } from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import {
  createPhaseReminder,
  type EnvoyPublish,
  type PhaseReminderLog,
} from "@legion/pi-shared/phase-reminder"
import { type PhaseStallEntry, phaseStallEntryFrom } from "@legion/pi-shared/phase-stall"
import {
  CLAUDE_BACKGROUND_TASK_KIND,
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
<<<<<<< 7fa22de66d
 * claim it answers that the claim owes nothing, and every reminder is dropped — the refused turns
 * themselves are counted by the bridge, which reports each turn's outcome to the daemon
 * (INFRA-714).
 *
 * A wait the worker declared (`declare_wait`) and the design gate's hold that keeps a merger back
 * are the daemon's owed answer to give (dispatch://INFRA-2040). While either stands, the machine
 * continues no turn, arms no timer and sends no reminder but the one for a tool call written as
 * text, and every activity report says that no reminder is due, so the daemon can release the
 * pod. Only the worker's own background work buys a quiet wait (`${background_tasks}` at `Stop`).
 * News is what this server's channel writes into the session after the declaration: each relayed
 * Envoy delivery, under the frame's own key, and each person's message, with none (D8 of
 * dispatch://INFRA-2040's plan). The channel server reports it through `news`, from its own
 * process, whichever turn Claude Code takes it into: the bridge's inbox and these hooks never see
 * the frame's key. The CallTool handler of the same server tells this endpoint the wait a
 * `declare_wait` recorded (`waitDeclared`).
=======
 * claim it answers that the claim owes nothing, and every reminder is dropped. The bridge reports
 * each turn's outcome to the daemon, with the status Claude Code named, and marks a turn no prompt
 * of the daemon's ran — a reminder written over the inbox — so the daemon judges that turn from
 * the report (dispatch://INFRA-4508): a refusal whose status cannot clear fails the claim at once
 * and holds the issue, and a refusal that can clear fails nothing, so the reminders keep their
 * pace. At six such turns in a row the daemon sends the `provider-errors` notice once
 * (`providerErrorRunReached` in the daemon's `workflow.go` says to whom).
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
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
  /** The `legion` tool's `declare_wait` returned the wait `waitId` the daemon recorded
   * (`onWaitDeclared` in `@legion/pi-shared/legion-phase-ops`): news this server writes after the
   * call's result, in the declaring turn or a later one, is reported against that wait. The
   * CallTool handler of the same server calls it (`./envoy-channel-server`), as the Oh My Pi
   * host's `onWaitDeclared` does. An architect's session declares no wait and runs no reminder. */
  readonly waitDeclared: (waitId: string) => void
  /** News this server wrote into the session (D8 of dispatch://INFRA-2040's plan): a relayed Envoy
   * delivery, with `deliveryKey` the frame's own dedupe key (`deliveryIdentity` in
   * `deliverRelayed`, `./envoy-channel-server`), or a person's message, with none. It starts the
   * count again, and while a wait this endpoint knows of stands it is reported to the daemon
   * against that wait, the key verbatim (`PhaseReminder.news`). Settles once the daemon answered
   * or the report failed and was kept to be sent again; it never rejects. An architect's session
   * runs no reminder, and its news moves nothing. */
  readonly news: (deliveryKey?: string) => Promise<void>
  /** Drops the armed timer, so no reminder timer, the quiet wait of the worker's own background
   * work the longest of them, holds a shutting-down server open (the server's own `stop`). */
  readonly stop: () => void
}

export interface PhaseHookDeps {
  /** The record's file (`phaseStallFile`). */
  readonly stateFile: string
  /** The pane's registered claim, or undefined while its boot is still registering. */
  readonly claim: () => ClaimCapability | undefined
  /** Settles once the pane's claim boot has run (`startLegionClaim`'s `booted`). The resume
   * reminder, the owed read before it, the boot's activity report and the recreated-workspace
   * notice all need the claim, and none is sent before one exists. Never rejects. */
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
   * whether a reminder goes out and that gives the wait the worker declared and the design gate's
   * hold, aborted with `signal` once the check's bound passes (`PhaseReminderDeps.readOwed`). An
   * answer that does not arrive is the shared reminder's to decide (`UNREAD_CHECK`,
   * `@legion/pi-shared/phase-reminder`), as it is on Oh My Pi. */
  readonly readOwed: (input: OwedInput, signal: AbortSignal) => Promise<LegionOwed>
  /** Reports news after the declaration of a wait to the daemon (`LegionDaemonClient.waitNews`),
   * aborted with `signal` once the bound passes (`PhaseReminderDeps.reportNews`). */
  readonly reportNews: (input: WaitNewsInput, signal: AbortSignal) => Promise<LegionWaitNews>
  /** Sends this session's activity report to the daemon (`LegionDaemonClient.activity`), so the
   * idle rule can decide whether the pane's pod may be released (INFRA-945): at each `Stop`, and
   * once at boot after the owed read. */
  readonly reportActivity: (input: ActivityInput) => Promise<void>
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
 * one key only this host keeps. `PhaseStallEntry` is the machine's record and both hosts read it,
 * so the extra key is composed around it here rather than added to it, and a record the Go bridge
 * left behind, which has no such key, still reads.
 */
export interface PhaseStallRecord {
  /** The reminder's rhythm and count the file holds, with the wait news is reported against
   * (`waitId`) and each report of news the daemon never answered (`unsentNews`, each with its
   * wait and its delivery's key), or undefined when it holds none this build knows. */
  readonly entry: PhaseStallEntry | undefined
  /** Whether this launch's recreated-workspace notice has already been written into the session
   * (`recreatedNoticeSent` in the file); false for a record that names no such key. It outlives
   * only a server Claude Code restarts inside one `claude` process: a launch that recreated
   * nothing clears it (`createPhaseHookEndpoint`). */
  readonly recreatedNoticeSent: boolean
}

/** A file this session holds no record in: a fresh session, an unreadable one, or one written by
 * a build whose state this one does not know. */
const NO_RECORD: PhaseStallRecord = { entry: undefined, recreatedNoticeSent: false }

/**
 * The record `file` holds: a missing file is no record at all, so a fresh session restores
 * nothing. The entry itself is assembled where the Oh My Pi host's is
 * (`phaseStallEntryFrom`), so a record an earlier build wrote — `{"state":"open"}` or
 * `{"state":"closed"}` with its count, `{"state":"quiet"}` or `{"state":"text-call"}` — reads as
 * the rhythm's `active` here too, and a missing `count`, `reported` or `reportKey` reads as 0,
 * false and absent. A record with no `waitId` or `unsentNews`, which every build before
 * dispatch://INFRA-2040 wrote, reads as no wait known and no report kept; a record that has them
 * gives the relaunched process the wait and the reports to send again at its boot. This host adds
 * the warning for a record it understands none of, and its own `recreatedNoticeSent`.
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
  return {
    entry,
    recreatedNoticeSent: "recreatedNoticeSent" in data && data.recreatedNoticeSent === true,
  }
}

/** Replaces `file` with `entry` and this host's own key, through a temporary file in the same
 * directory renamed into place, so a process killed mid-write never leaves half a record to read:
 * the shape the Go bridge wrote the same file in while it kept this record, kept so a session
 * carried across the cutover reads whatever the build now in the pods left. The key is written
 * only once it is true, so a record this host writes is otherwise the shared entry and nothing
 * more, the wait's id and the unsent reports of news among it. A write that fails is logged
 * rather than thrown: the worker still gets its reminder. */
export function writePhaseStallRecord(
  file: string,
  entry: PhaseStallEntry,
  recreatedNoticeSent: boolean,
  log: PhaseReminderLog,
): void {
  const temporary = join(dirname(file), `.legion-phase-stall-${randomUUID()}`)
  try {
    writeFileSync(
      temporary,
      JSON.stringify({ ...entry, ...(recreatedNoticeSent ? { recreatedNoticeSent } : {}) }),
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
  // The machine's entry and this host's own key are one record, so the last entry written is kept
  // for the write the notice makes.
  let entry: PhaseStallEntry = restored.entry ?? { state: "active", count: 0, reported: false }
  let recreatedNoticeSent = restored.recreatedNoticeSent
  const saveRecord = (next: PhaseStallEntry): void => {
    entry = next
    writePhaseStallRecord(deps.stateFile, next, recreatedNoticeSent, deps.log)
  }
  // The key exists only so a server Claude Code restarts inside one `claude` process, whose
  // environment is still its launch's, does not repeat that launch's notice. The launcher decides
  // `LEGION_WORKSPACE_RECREATED` afresh for every launch, so a launch that recreated nothing
  // clears it, and the next launch that recreates the workspace again is told again.
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
    reportNews: deps.reportNews,
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
   * INFRA-945: what this session is doing, reported to the daemon as each turn settles, so the
   * idle rule can decide whether the pane's pod may be released. Claude Code's only sight of
   * background work is the `Stop` hook's `${background_tasks}`, so this host names background work
   * at the settle alone; the one kind it can name is a Claude Code background task. The boot
   * reports once too, after its owed read (below), and names no background work: it has no
   * `${background_tasks}` to read.
   *
   * A report that does not reach the daemon is logged and dropped: the turn's reminder is what
   * the hook owes its worker, and a claim with no report after its last turn is held rather than
   * released.
   */
  const sendActivity = async (claim: ClaimCapability, backgroundWork: boolean): Promise<void> => {
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
      await deps.reportActivity(request)
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
   * like any other, which says nothing of what the claim owes. It is this plugin's own notice, not
   * news, so it ends no wait (D8 of dispatch://INFRA-2040's plan).
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

  void deps.booted.then(async () => {
    const claim = deps.claim()
    if (claim === undefined) return
    sendRecreatedNotice(claim.issue)
    if (claim.role === "architect") return
    // D10: a worker the daemon relaunched is reminded from a timer, since nothing else would wake
    // it, and the daemon's answer when it is due says whether anything is owed. The guard drops
    // that timer when a turn started meanwhile, which is exactly the case to drop — the daemon
    // re-sending an interrupted task arrives as a `UserPromptSubmit`. The arming first sends
    // every report of news the restored record kept unsent, then reads the owed answer, which
    // names the wait that stands, and arms nothing while a wait the worker declared or the
    // design gate's hold stands (`armResumeReminder`, `@legion/pi-shared/phase-reminder`).
    await reminder.armResumeReminder()
    // `sendActivity` otherwise runs only at a `Stop`, and a worker relaunched during a wait may
    // run no turn before the wait ends. So the boot reports once the owed answer is read: while a
    // wait or a hold stands it says that no reminder is due, and the daemon can release the pod.
    // It names no background work. A server Claude Code restarts inside one `claude` process
    // may leave a background task of the worker's running that this report does not name; the
    // next `Stop` names it.
    await sendActivity(claim, false)
  })

  return {
    updateAnswered: () => reminder.apply({ kind: "update-answered" }),
    // An architect's session runs no phase reminder (the gate in `handle`), so neither entry below
    // moves anything for one. A delivery written before the claim has registered is news all the
    // same: the record restored above may name a wait, and a report kept for want of a claim goes
    // at the boot.
    waitDeclared: (waitId) => {
      if (deps.claim()?.role === "architect") return
      reminder.waitDeclared(waitId)
    },
    news: async (deliveryKey) => {
      if (deps.claim()?.role === "architect") return
      await reminder.news(deliveryKey)
    },
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
          // a worker waiting to be woken by it, not a stalled one, and that is the one quiet wait
          // a turn's end buys. A wait the worker declared and the design gate's hold are the
          // daemon's owed answer to give, which the settle reads: while either stands it
          // continues nothing and arms nothing, but for a tool call written as text.
          const backgroundWork = backgroundWorkRunning(event.backgroundTasks)
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
          await sendActivity(claim, backgroundWork)
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
          await reminder.settle({ kind: "refused", message: event.lastAssistantMessage ?? "" })
          return {}
        }
        case "PostToolUse":
        case "PostToolUseFailure": {
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
          // count again and says nothing of what the claim owes. It reports no news either: the
          // daemon's task, the resume task and the wait's own task are prompts, and the news among
          // these turns — a delivery or a person's message this server wrote — was reported as the
          // channel server wrote it, with the frame's key this hook never sees (`news`). The turn
          // is declared first, which makes every armed timer stale, and the pending one is dropped
          // outright: the runner's generation guard answers the same for the timers it armed, and
          // dropping the one timer this server holds is the plainest way to say the same thing.
          reminder.turnStarted()
          dropTimer()
          const prompt = event.prompt ?? ""
          if (prompt === lastReminder) return {}
          reminder.apply({ kind: "prompt" })
          return {}
        }
      }
    },
  }
}
