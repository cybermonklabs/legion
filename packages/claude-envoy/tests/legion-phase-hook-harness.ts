// The phase-hook endpoint as the cases of legion-phase-hook.test.ts and
// legion-phase-hook-refusal.test.ts drive it: the pane's claim, the limits every case runs on, the
// record on disk and the endpoint over a daemon that answers the contracts' state fixture.
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import type { PublishInput, PublishResult } from "@legion/envoy-client/transport"
import { type ActivityInput, createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import {
  createPhaseHookEndpoint,
  type PhaseHookEndpoint,
  paneReminderLog,
  phaseStallFile,
} from "../src/legion-phase-hook"

export const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

/** The pane's claim: the implementer of LEGION-208, whose phase in the daemon's state fixture is
 * `implementing`, so the reminder's record check says this role still works this phase. */
export const implementer: ClaimCapability = {
  sessionID: SESSION,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 1,
}

/**
 * The four numbers every case runs on, through the environment the endpoint is given: no test
 * waits a production interval. The architect is held off at nine reminders so only the case that
 * drives the notice reaches it.
 *
 * The clock is the real one, as `waitFor`'s is: what the cases drive is the endpoint's own
 * `setTimeout`, whose callback then awaits a daemon read, so advancing a fake clock would settle
 * nothing on its own. Every case waits for the condition it is about; the few fixed waits are the
 * ones asserting that nothing happens, which have no condition to wait for, and each is a small
 * multiple of the interval above.
 */
const QUICK_LIMITS: Record<string, string> = {
  LEGION_STALL_REMINDER_MS: "20",
  LEGION_STALL_WAITING_MS: "80",
  LEGION_STALL_RESUME_MS: "20",
  LEGION_STALL_ARCHITECT_AFTER: "9",
}

/** One record on disk, as the Go bridge or an earlier run of this server left it. */
export function recorded(
  state: string,
  count: number,
  extra: Record<string, unknown> = {},
): string {
  return JSON.stringify({ state, count, reported: false, ...extra })
}

interface Harness {
  readonly endpoint: PhaseHookEndpoint
  readonly file: string
  /** The record on disk, or undefined when nothing has been written. */
  readonly record: () => Promise<unknown>
  /** The record as it stood when the daemon answered the record check. */
  readonly recordAtDaemonRead: () => unknown
  /** The lines this endpoint wrote into the bridge's inbox, in order. */
  readonly inbox: readonly string[]
  /** Every message published to the Envoy listener, in order. */
  readonly published: readonly PublishInput[]
  /** Every activity report this endpoint sent the daemon, in order. */
  readonly reports: readonly ActivityInput[]
  /** Settles the claim's boot, as `startLegionClaim`'s `booted` does. */
  readonly boot: () => void
  /** How often the endpoint has said a turn of this session started (`firstTurnStarted`). */
  readonly turnsStarted: () => number
  readonly lines: readonly string[]
  readonly close: () => Promise<void>
}

export async function harness(
  options: {
    readonly record?: string
    readonly claim?: ClaimCapability
    /** The state the daemon's record gives the implementer's claim; `working` in the fixture. */
    readonly claimState?: string
    /** False to take the implementer's claim off the daemon's provider-error ladder: the fixture
     * gives it a run of 2 provider errors and a pending task, which is the ladder holding it. */
    readonly onTheLadder?: boolean
    /** Added to (and overriding) `QUICK_LIMITS` and `LEGION_PROJECT`. */
    readonly env?: Record<string, string | undefined>
    /** False for a session outside a Legion pane, which has no inbox at all. */
    readonly inbox?: boolean
    /** What the bridge answers the `attempt`th inbox write; queued by default. */
    readonly inboxAnswer?: (attempt: number) => Promise<void>
    /** What the listener answers the `attempt`th publish; accepted by default. */
    readonly publish?: (
      input: PublishInput,
      attempt: number,
    ) => Promise<Pick<PublishResult, "kept">>
    /** Whether the daemon refuses every activity report: the hook still answers its worker. */
    readonly reportActivityFails?: boolean
  } = {},
): Promise<Harness> {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-phase-hook-"))
  const file = phaseStallFile(directory, SESSION)
  if (options.record !== undefined) await writeFile(file, options.record)
  const state = JSON.parse(
    await readFile(
      resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/state.json"),
      "utf8",
    ),
  ) as { issues: Record<string, { workers: Record<string, { claim: { state: string } }> }> }
  const implementerClaim = state.issues["LEGION-208"]?.workers["implementer"]?.claim as
    | { state: string; providerErrorRun?: number; pendingTask?: unknown }
    | undefined
  if (options.claimState !== undefined && implementerClaim !== undefined) {
    implementerClaim.state = options.claimState
  }
  if (options.onTheLadder === false && implementerClaim !== undefined) {
    delete implementerClaim.providerErrorRun
    delete implementerClaim.pendingTask
  }
  const record = async (): Promise<unknown> => {
    const raw = await readFile(file, "utf8").catch(() => undefined)
    return raw === undefined ? undefined : JSON.parse(raw)
  }
  let atRead: unknown
  const fetchStub = (async (_input, _init) => {
    atRead = await record()
    return Response.json(state)
  }) as typeof fetch
  const daemon = createLegionDaemonClient("http://daemon.test", fetchStub)
  const lines: string[] = []
  const inbox: string[] = []
  const published: PublishInput[] = []
  const reports: ActivityInput[] = []
  let turnsStarted = 0
  const booted = Promise.withResolvers<void>()
  const endpoint = createPhaseHookEndpoint({
    stateFile: file,
    claim: () => options.claim,
    booted: booted.promise,
    firstTurnStarted: () => {
      turnsStarted += 1
    },
    readState: (signal) => daemon.state({ signal }),
    reportActivity: async (input) => {
      reports.push(input)
      if (options.reportActivityFails === true) throw new Error("the daemon refused the report")
    },
    inbox:
      options.inbox === false
        ? undefined
        : async (text) => {
            inbox.push(text)
            if (options.inboxAnswer !== undefined) await options.inboxAnswer(inbox.length)
          },
    publish: async (input) => {
      published.push(input)
      return options.publish === undefined ? {} : options.publish(input, published.length)
    },
    env: { LEGION_PROJECT: "legion", ...QUICK_LIMITS, ...options.env },
    log: paneReminderLog((line) => lines.push(line)),
  })
  return {
    endpoint,
    file,
    record,
    recordAtDaemonRead: () => atRead,
    inbox,
    published,
    reports,
    boot: booted.resolve,
    turnsStarted: () => turnsStarted,
    lines,
    close: async () => {
      // As the server's own `stop` does: no armed timer outlives the case that armed it.
      endpoint.stop()
      await rm(directory, { recursive: true, force: true })
    },
  }
}
