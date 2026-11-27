import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { daemonContract } from "@legion/contracts/daemon-contract"
import {
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport"
import {
  type ActivityInput,
  createLegionDaemonClient,
  type LegionDaemonClient,
} from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { owedReminder, UNREAD_REMINDER } from "@legion/pi-shared/phase-stall"
<<<<<<< 7fa22de66d
import { UPDATE_ASK_KEY, type WaitStandIn, waitStandIn } from "@legion/pi-shared/test/wait-stand-in"
import { legion } from "../package.json" with { type: "json" }
import { startChannelSession } from "../src/envoy-channel-server"
=======
import pkg from "../package.json" with { type: "json" }
>>>>>>> 67bcd7c78393eb8d85c10bbae0ceb998d1b01253
import {
  createPhaseHookEndpoint,
  PHASE_HOOK_REFUSAL,
  PHASE_HOOK_TOOL_NAME,
  type PhaseHookEndpoint,
  paneReminderLog,
  phaseHookToolDefinition,
  phaseStallFile,
} from "../src/legion-phase-hook"
import { executeLegionTool } from "../src/legion-tool"
import { SessionIdentity } from "../src/session-identity"
import {
  directSubject,
  FakeNats,
  FakeNotifier,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

// The cases that run the channel server's delivery path (`startChannelSession`) read no envoy.json
// of the caller's own.
isolatePaneEnvironment()
const scratchState = useScratchState()

const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
/** The contract this build speaks, as `legion-claim.ts` reads it (`daemonContract`): this
 * checkout's. */
const PLUGIN_CONTRACT = daemonContract(pkg, import.meta.dir)

/** The pane's claim: the implementer of LEGION-208. */
const implementer: ClaimCapability = {
  sessionID: SESSION,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 1,
}

/** What the stand-in daemon calls the work the claim owes, and the reminder that names it: the
 * reminder renders the daemon's answer and nothing of its own. */
const WORK = "the implementing phase of LEGION-208"
const REMINDER = owedReminder(WORK)
/** The stand-in daemon's answers to `POST /legion/v1/claims/owed`. */
const owesWork = () => Response.json({ owes: true, work: WORK })
const owesNothing = () => Response.json({ owes: false, work: "" })

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

/** One record on disk, as an earlier run of this server, or an earlier release, left it. */
function recorded(state: string, count: number, extra: Record<string, unknown> = {}): string {
  return JSON.stringify({ state, count, reported: false, ...extra })
}

interface Harness {
  readonly endpoint: PhaseHookEndpoint
  readonly file: string
  /** The real daemon client the endpoint asks, on the stand-in daemon: what the pane's `legion`
   * tool posts with too. */
  readonly daemon: LegionDaemonClient
  /** The record on disk, or undefined when nothing has been written. */
  readonly record: () => Promise<unknown>
  /** The record as it stood when the daemon was asked what the claim owes. */
  readonly recordAtDaemonRead: () => unknown
  /** Every route this endpoint asked the daemon for, in order. */
  readonly daemonPaths: readonly string[]
  /** The lines this endpoint wrote into the bridge's inbox, in order. */
  readonly inbox: readonly string[]
  /** Every message published to the Envoy listener, in order. */
  readonly published: readonly PublishInput[]
  /** Every activity report this endpoint sent the daemon, in order. */
  readonly reports: readonly ActivityInput[]
  /** Every question this endpoint asked the daemon about what the claim owes, in order. */
  readonly asked: readonly unknown[]
  /** What the daemon answers `claims/owed` from now on: its record has moved. */
  readonly answersOwed: (answer: () => Response) => void
  /** Settles the claim's boot, as `startLegionClaim`'s `booted` does. */
  readonly boot: () => void
  /** How often the endpoint has said a turn of this session started (`firstTurnStarted`). */
  readonly turnsStarted: () => number
  readonly lines: readonly string[]
  readonly close: () => Promise<void>
}

async function harness(
  options: {
    readonly record?: string
    readonly claim?: ClaimCapability
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
    /** What the daemon answers `claims/owed`: that the claim owes `WORK`, unless given. */
    readonly owed?: () => Response
    /** The stand-in daemon's answer to a route the case serves itself, tried before every other
     * route: the wait's declaration and its news (`waitStandIn`). */
    readonly routes?: (path: string, body: unknown) => Response | undefined
  } = {},
): Promise<Harness> {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-phase-hook-"))
  const file = phaseStallFile(directory, SESSION)
  if (options.record !== undefined) await writeFile(file, options.record)
  const record = async (): Promise<unknown> => {
    const raw = await readFile(file, "utf8").catch(() => undefined)
    return raw === undefined ? undefined : JSON.parse(raw)
  }
  let atRead: unknown
  let owed = options.owed ?? owesWork
  const asked: unknown[] = []
  const paths: string[] = []
  // The reminder's owed check asks one route, never the whole project (dispatch://INFRA-2694).
  const fetchStub = (async (input, init) => {
    const path = new URL(input.toString()).pathname
    paths.push(path)
    const body: unknown = init?.body == null ? undefined : JSON.parse(String(init.body))
    const own = options.routes?.(path, body)
    if (own !== undefined) return own
    if (path === "/legion/v1/claims/owed") {
      atRead = await record()
      asked.push(body)
      return owed()
    }
    return Response.json({ error: "no stand-in route" }, { status: 404 })
  }) as typeof fetch
  const daemon = createLegionDaemonClient("http://daemon.test", {
    pluginContract: PLUGIN_CONTRACT,
    fetch: fetchStub,
  })
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
    readOwed: (question, signal) => daemon.owed(question, { signal }),
    reportNews: (report, signal) => daemon.waitNews(report, { signal }),
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
    daemon,
    record,
    recordAtDaemonRead: () => atRead,
    inbox,
    daemonPaths: paths,
    published,
    reports,
    asked,
    answersOwed: (answer) => {
      owed = answer
    },
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

test("the tool is declared under its own name, describes itself with the refusal a model's call gets, and takes the hook events", () => {
  const definition = phaseHookToolDefinition()
  expect(definition.name).toBe(PHASE_HOOK_TOOL_NAME)
  expect(definition.description).toBe(PHASE_HOOK_REFUSAL)
  expect(definition.inputSchema).toMatchObject({
    properties: {
      event: {
        enum: ["Stop", "StopFailure", "PostToolUse", "PostToolUseFailure", "UserPromptSubmit"],
      },
    },
    required: ["event"],
  })
})

test("a turn settling while the daemon says the claim owes work is blocked once, with the reminder naming that work, and the count is recorded before the answer", async () => {
  const { endpoint, record, recordAtDaemonRead, inbox, asked, daemonPaths, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "I pushed the branch.",
        stopHookActive: "false",
      }),
    ).toEqual({ decision: "block", reason: REMINDER })
    expect(REMINDER).toContain(`Your turn ended with ${WORK} still owed`)
    // The claim asked about itself, authenticated by its own secret.
    expect(asked).toEqual([
      { claimToken: implementer.claimToken, sessionId: SESSION, secret: implementer.secret },
    ])
    // The daemon is asked before the reminder is counted, and the count is on disk by the time
    // the hook answers: a reminder the worker has been given is never uncounted. What the worker
    // said rides the record with it, for a relaunch's architect notice to quote.
    const said = { reported: false, lastMessage: "I pushed the branch." }
    expect(recordAtDaemonRead()).toEqual({ state: "active", count: 0, ...said })
    expect(await record()).toEqual({ state: "active", count: 1, ...said })
    // The check asks what this claim owes and never reads the whole project
    // (dispatch://INFRA-2694), which is what the channel server wires it to.
    expect(daemonPaths).toEqual(["/legion/v1/claims/owed"])

    // The second settle of the run lets the turn end and carries its reminder from the timer, so
    // the daemon sees the turn end and can deliver a queued task.
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Still here.",
        stopHookActive: "true",
      }),
    ).toEqual({})
    expect(inbox).toEqual([])
    await waitFor(async () => inbox.length > 0, "the timer's reminder to reach the inbox")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({
      state: "active",
      count: 2,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})

// Both ways on this host: the daemon's answer that the claim owes nothing blocks no `Stop`,
// and its timer, which asks again when it is due, writes nothing into the session.
test("a turn the daemon says owes nothing is blocked by nothing, from the Stop or its timer", async () => {
  const { endpoint, record, inbox, asked, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
    owed: owesNothing,
  })
  try {
    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Completed the phase." }),
    ).toEqual({})
    // The restored count makes this settle's reminder a timer's; it asks the daemon when it is
    // due, and the answer is the same. A case asserting that nothing happens has no condition to
    // wait for, so this is one of the file's fixed waits, three times the reminder interval.
    await waitFor(async () => asked.length === 1, "the timer to ask the daemon")
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({
      state: "active",
      count: 1,
      reported: false,
      lastMessage: "Completed the phase.",
    })
  } finally {
    await close()
  }
})

// A daemon that does not answer — unreachable, refusing, or answering what the contract does
// not describe — still has the worker nudged, and the nudge says it could not read the record:
// it names no phase and no owed work, because it read neither, and the one operation it names is
// `declare_wait`.
test("a daemon that does not say what the claim owes still blocks the Stop, with a reminder that names no phase and no operation but declare_wait", async () => {
  for (const [failure, answer] of [
    ["a refusal", () => Response.json({ error: "the daemon is restarting" }, { status: 503 })],
    ["an answer the contract refuses", () => Response.json({ owes: true, work: "" })],
  ] as const) {
    const { endpoint, close } = await harness({
      record: recorded("active", 0),
      claim: implementer,
      owed: answer,
    })
    try {
      expect(
        await endpoint.handle({ event: "Stop", lastAssistantMessage: "Pushed." }),
        failure,
      ).toEqual({ decision: "block", reason: UNREAD_REMINDER })
    } finally {
      await close()
    }
  }
  expect(UNREAD_REMINDER).toContain("could not read the Legion daemon's record")
  for (const named of ["phase", "handoff_complete", "implementing", WORK]) {
    expect(UNREAD_REMINDER).not.toContain(named)
  }
})

// INFRA-945: every settle tells the daemon what this session is doing, so the idle rule can
// decide whether the pane's pod may be released. A turn ending with a background task of the
// worker's own still running names it; a report the daemon refuses costs the worker nothing.
test("the Stop hook reports what this session is doing to the daemon", async () => {
  const { endpoint, reports, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
  })
  try {
    await endpoint.handle({
      event: "Stop",
      lastAssistantMessage: "Waiting for the build.",
      backgroundTasks: JSON.stringify([{ id: "bg_1", status: "running" }]),
    })
    expect(reports).toHaveLength(1)
    expect(reports[0]).toMatchObject({
      claimToken: "legion-legion-legion-208-implementer",
      sessionId: SESSION,
      secret: implementer.secret,
      activity: {
        backgroundWork: { running: true, kinds: ["claude-background-task"] },
      },
    })
    expect(Date.parse(String(reports[0]?.activity.at))).not.toBeNaN()

    // The task has ended: the next settle says so, and reports again even though the turn
    // earned no reminder — a claim with no report after its last turn is never released.
    await endpoint.handle({ event: "Stop", lastAssistantMessage: "Done.", backgroundTasks: "[]" })
    expect(reports).toHaveLength(2)
    expect(reports[1]?.activity.backgroundWork).toEqual({ running: false, kinds: [] })
    expect(
      Date.parse(String(reports[1]?.activity.phase.nextReminderAt)),
      "the second settle's reminder is armed on a timer, and the report says when",
    ).not.toBeNaN()
  } finally {
    await close()
  }
})

test("a report the daemon refuses still leaves the worker its reminder", async () => {
  const { endpoint, reports, lines, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
    reportActivityFails: true,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Pushed." })).toEqual({
      decision: "block",
      reason: REMINDER,
    })
    expect(reports).toHaveLength(1)
    expect(
      lines.some((line) => line.includes("activity report did not reach the daemon")),
    ).toBeTrue()
  } finally {
    await close()
  }
})

test("a session holding no claim reports nothing", async () => {
  const { endpoint, reports, close } = await harness({ record: recorded("active", 0) })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Pushed." })).toEqual({})
    expect(reports).toEqual([])
  } finally {
    await close()
  }
})

// A WAITING line buys nothing (dispatch://INFRA-2040): a worker that waits on something outside
// its own work declares the wait with the `legion` tool's `declare_wait`. A turn that ends with
// that line and nothing else gets the reminder a turn without it gets. The one quiet wait a turn's
// end buys is the worker's own background work still running (`${background_tasks}`), reminded
// once after the waiting delay.
test("a turn that ends with a WAITING line gets the same reminder as a turn without that line, and only background work buys the quiet wait", async () => {
  for (const said of ["Pushed the branch.", "Pushed the branch.\nWAITING on CI to go green."]) {
    const { endpoint, record, close } = await harness({
      record: recorded("active", 0),
      claim: implementer,
    })
    try {
      expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: said }), said).toEqual({
        decision: "block",
        reason: REMINDER,
      })
      expect(await record(), said).toEqual({
        state: "active",
        count: 1,
        reported: false,
        lastMessage: said,
      })
    } finally {
      await close()
    }
  }

  const { endpoint, record, inbox, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Started the build in the background.",
        backgroundTasks: JSON.stringify([{ id: "bg_1", status: "running" }]),
      }),
    ).toEqual({})
    const waiting = { reported: false, lastMessage: "Started the build in the background." }
    expect(await record()).toEqual({ state: "waiting", count: 0, ...waiting })
    // Nothing is sent into the wait itself.
    await Bun.sleep(30)
    expect(inbox).toEqual([])
    await waitFor(async () => inbox.length > 0, "the reminder the wait earned")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "active", count: 1, ...waiting })
    // One reminder, not a second from the same wait.
    await Bun.sleep(60)
    expect(inbox).toEqual([REMINDER])
  } finally {
    await close()
  }
})

// `StopFailure` runs instead of `Stop` when the turn ended on an API error, and its
// `${last_assistant_message}` is the error text rather than anything the worker said ("Hooks
// reference", code.claude.com/docs/en/hooks). That is the provider refusing the turn, so the
// answer carries no continuation: the host would turn one into another turn at once and the
// provider would refuse that one the same way, as it did for dispatch://INFRA-264's implementer.
// One timer is armed all the same, because the task that refused turn carried ended with it and
// nothing else would wake this worker: a single transient refusal would otherwise leave the issue
// idle. A refusal that can clear fails no claim (dispatch://INFRA-4508), so the reminders keep
// their pace; the daemon fails the claim at once for a refusal whose status cannot clear, a
// reminder's own turn included, which it judges from the bridge's turn-outcome report, and
// answers then that it owes nothing, and the test below this one holds that a failed claim's
// session is reminded of nothing.
test("a turn that ended on an API error carries no continuation, and is woken once by its timer", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("active", 0, { lastMessage: "I pushed the branch." }),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({
        event: "StopFailure",
        lastAssistantMessage: "API Error: Rate limit reached",
      }),
    ).toEqual({})
    // The words the worker last said stand: the provider's error text is not one of them, and a
    // turn the worker never read moves nothing.
    expect(await record()).toEqual({
      state: "active",
      count: 0,
      reported: false,
      lastMessage: "I pushed the branch.",
    })
    // Nothing is sent as the turn ends; the reminder comes from the timer instead.
    expect(inbox).toEqual([])
    await waitFor(async () => inbox.length > 0, "the reminder the refused turn's timer carries")
    expect(inbox).toEqual([REMINDER])
    // One wake, not a run of them: the next refusal arms the next one. An assertion that nothing
    // more arrives has no signal to await, so this is one of the file's fixed waits, three times
    // the reminder interval the harness sets above.
    await Bun.sleep(60)
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({
      state: "active",
      count: 1,
      reported: false,
      lastMessage: "I pushed the branch.",
    })
  } finally {
    await close()
  }
})

// The daemon failing this role's claim is its word that this session is finished, and the daemon
// answers that the failed claim owes nothing.
// It is what ends the reminding here, since this host sees a refused turn only as the
// `StopFailure` of the turn it refused: the timer that turn arms asks the daemon, and reminds
// nothing.
test("a refused turn whose claim the daemon has failed is reminded of nothing", async () => {
  const { endpoint, record, inbox, asked, lines, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
    owed: owesNothing,
  })
  try {
    expect(
      await endpoint.handle({
        event: "StopFailure",
        lastAssistantMessage: "API Error: Refusal (reasoning_extraction)",
      }),
    ).toEqual({})
    await waitFor(async () => asked.length > 0, "the owed check the refused turn's timer runs")
    // A case asserting that nothing happens has no condition to wait for, so this is one of the
    // file's fixed waits (its header says why a fake clock settles nothing here), three times
    // the reminder interval above.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "active", count: 1, reported: false })
    expect(lines.some((line) => line.includes("the daemon says this claim owes nothing"))).toBe(
      true,
    )
  } finally {
    await close()
  }
})

// INFRA-484: the update message reaches Claude Code as an inbound message, and
// `ready_for_update` finishes nothing the daemon records. The `Stop` of the turn the worker
// answers in must still block nothing: a blocked `Stop` continues that same turn, so the work it
// did would go with the pod, and before the round's bound the daemon waits for every turn to end.
test("a ready_for_update answer blocks no Stop: the answering turn ends, and nothing is armed in the wait", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
  })
  try {
    // What the server's CallTool handler does once `ready_for_update` returned without throwing
    // (`./src/envoy-channel-server`): the worker has pushed what it wants kept and waits for its
    // process to be replaced.
    endpoint.updateAnswered()
    expect(await record()).toEqual({ state: "waiting", count: 0, reported: false })
    // Claude Code's own `PostToolUse` for the call that answered: the worker ran a real tool
    // call, which does not take the wait back.
    expect(await endpoint.handle({ event: "PostToolUse" })).toEqual({})
    expect(await record()).toEqual({ state: "waiting", count: 0, reported: false })

    const answered = "Pushed everything and answered; waiting for the new pod."
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: answered })).toEqual({})
    const said = { reported: false, lastMessage: answered }
    expect(await record()).toEqual({ state: "waiting", count: 0, ...said })
    // Nothing is written into the wait the answer created, and nothing ever will be: it arms no
    // timer at all. The daemon's update round is its watchdog, and a reminder fired here would be
    // a prompt, which starts a turn of work the replacement throws away, and before the round's
    // bound a running turn holds that replacement back (INFRA-484, the `awaiting-update` state of
    // `@legion/pi-shared/phase-stall`, which a record holds as the plain `waiting`). A case
    // asserting that nothing happens has no condition to wait for, so this is one of the file's
    // fixed waits, over the quiet wait itself and so over every wait this endpoint arms, and the
    // endpoint's own `setTimeout` is what runs on the real clock. The quiet wait the worker's own
    // background work buys keeps its reminder, which its own case holds.
    await Bun.sleep(200)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "waiting", count: 0, ...said })
  } finally {
    await close()
  }
})

test("the prompt that carries this endpoint's own reminder does not reset its count", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => inbox.length > 0, "the timer's reminder to reach the inbox")
    const said = { reported: false, lastMessage: "Still here." }
    expect(await record()).toEqual({ state: "active", count: 2, ...said })
    expect(await endpoint.handle({ event: "UserPromptSubmit", prompt: inbox[0] ?? "" })).toEqual({})
    expect(await record()).toEqual({ state: "active", count: 2, ...said })
  } finally {
    await close()
  }
})

test("every prompt submitted says a turn of this session has started, before the phase gate", async () => {
  // The claim's first-turn signal, which the channel server answers by taking this session's
  // Envoy name when its claim boot has registered the step by then, and by nothing when it has
  // not (`onFirstTurn`, `src/envoy-channel-server.ts`). This endpoint draws no such line: it
  // fires the signal before the gate below, for the bridge's seeding `/rename` turn — which
  // runs before any claim exists, since it writes the transcript the boot waits for — as for
  // an architect's turn, and the server decides what each comes to.
  const seeding = await harness()
  expect(await seeding.endpoint.handle({ event: "UserPromptSubmit", prompt: "/rename" })).toEqual(
    {},
  )
  expect(seeding.turnsStarted()).toBe(1)
  await seeding.close()

  const architect = await harness({ claim: { ...implementer, role: "architect" } })
  await architect.endpoint.handle({ event: "UserPromptSubmit", prompt: "plan it" })
  expect(architect.turnsStarted()).toBe(1)
  await architect.close()

  const worker = await harness({ record: recorded("active", 0), claim: implementer })
  await worker.endpoint.handle({ event: "PostToolUse", toolName: "Bash" })
  await worker.endpoint.handle({ event: "Stop" })
  expect(worker.turnsStarted()).toBe(0)
  await worker.endpoint.handle({ event: "UserPromptSubmit", prompt: "implement it" })
  await worker.endpoint.handle({ event: "UserPromptSubmit", prompt: "and again" })
  // Once per prompt: the step behind the signal claims once however often it is run
  // (`claimRoleAtFirstTurn`, `@legion/pi-shared/legion-claim-core`).
  expect(worker.turnsStarted()).toBe(2)
  // A call the model made itself is refused before anything moves, this signal included.
  expect(await worker.endpoint.handle({ event: "WhateverIsThis" })).toEqual({
    error: PHASE_HOOK_REFUSAL,
  })
  expect(worker.turnsStarted()).toBe(2)
  await worker.close()
})

// Every prompt but this endpoint's own reminder is an inbound message — the daemon's task, an
// Envoy delivery this server wrote, a person's message — and moves the rhythm's count and
// nothing else.
test("every other prompt is an inbound message, which starts the count again", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("active", 2),
    claim: implementer,
  })
  try {
    for (const prompt of [
      "Envoy delivery from the architect: the architect asks for a status",
      "Implement INFRA-220.",
    ]) {
      expect(await endpoint.handle({ event: "UserPromptSubmit", prompt })).toEqual({})
      expect(await record()).toEqual({ state: "active", count: 0, reported: false })
    }
  } finally {
    await close()
  }
})

// The daemon's own update message reaches a role whose phase has closed as a prompt like any
// other, an Envoy delivery this server wrote. With no model of the phase, nothing a prompt moves
// can decide that a reminder goes out, so the daemon's answer that the claim owes nothing keeps it
// quiet: no guard says so, the construction does.
test("an update message sent to a role whose phase is closed produces no reminder", async () => {
  const { endpoint, inbox, asked, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
    owed: owesNothing,
  })
  try {
    expect(
      await endpoint.handle({ event: "PostToolUse", toolName: "mcp__legion__legion" }),
    ).toEqual({})
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Handed off." })).toEqual(
      {},
    )

    expect(
      await endpoint.handle({
        event: "UserPromptSubmit",
        prompt: "Your pod is being updated: push what you keep.",
      }),
    ).toEqual({})
    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Nothing of mine to push." }),
    ).toEqual({})
    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still nothing." }),
    ).toEqual({})
    // Each settle asked the daemon, and each answer was that nothing is owed.
    expect(asked).toHaveLength(3)
    // A case asserting that nothing happens has no condition to wait for.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
  } finally {
    await close()
  }
})

test("a helper agent's tool call is not the worker working, and does not reset the count", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("active", 2),
    claim: implementer,
  })
  try {
    for (const event of ["PostToolUse", "PostToolUseFailure"]) {
      expect(await endpoint.handle({ event, toolName: "Bash", agentId: "agent-7" })).toEqual({})
      expect(await record()).toEqual({ state: "active", count: 2, reported: false })
    }
    expect(await endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })).toEqual(
      {},
    )
    expect(await record()).toEqual({ state: "active", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a tool call that failed is work too, and resets the count", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("active", 2),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({ event: "PostToolUseFailure", toolName: "Bash", agentId: "" }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "active", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("the architect is told at the threshold, asked again when the listener kept nothing, and never twice", async () => {
  const { endpoint, record, inbox, published, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
    env: { LEGION_STALL_ARCHITECT_AFTER: "2" },
    publish: async (_input, attempt) => {
      if (attempt > 1) return {}
      // The listener's "nothing was kept for that role". The notice is the shared one
      // (`createPhaseReminder`, packages/pi-shared/src/phase-reminder.ts), which sends no `keep`
      // field, so a listener keeps it for an architect role no live session holds and answers
      // 202. It answers this 404 when its keep failed, and a listener of an earlier release
      // answers it for a holder that is not live. Either way the architect was not told, and the
      // next delivered reminder publishes again under the same key.
      throw new EnvoyApiError({
        method: "POST",
        url: "http://listener.test/v1/messages/publish",
        status: 404,
        responseBody: JSON.stringify({ error: "no holder", reason: "unclaimed" }),
      })
    },
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => published.length === 1, "the architect to be told")
    const refused = (await record()) as { reported: boolean; reportKey: string }
    // The refusal leaves the notice to be sent again, under the key it was minted with.
    expect(refused.reported).toBe(false)
    expect(refused.reportKey).toMatch(/^[0-9a-f-]{36}$/)
    expect(published[0]?.topic).toContain("architect")
    // The notice names the work the daemon answered was owed.
    expect(published[0]?.message).toContain(
      `has let 2 reminders go unanswered: its turns keep ending with ${WORK} still owed`,
    )
    expect(JSON.parse(String(published[0]?.payload)).work).toBe(WORK)

    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => published.length === 2, "the architect to be told again")
    expect(published[1]?.idempotencyKey).toBe(refused.reportKey)
    expect(await record()).toEqual({
      state: "active",
      count: 3,
      reported: true,
      reportKey: refused.reportKey,
      lastMessage: "Still here.",
    })

    // Accepted once is told once: the run's later reminders add no second notice.
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => inbox.length === 3, "the third reminder of the run")
    expect(published.length).toBe(2)
  } finally {
    await close()
  }
})

// The notice goes to the architect the daemon names as the claim's owner in its owed answer: a
// child's own architect while that claim holds its role, the tree's once the daemon names it in
// its stead, and the tree's, named from the claim, when the answer names none.
test("the architect notice of a child goes to the architect the daemon names as its owner, else to the tree's", async () => {
  const child: ClaimCapability = {
    ...implementer,
    issue: "LEGION-209",
    claimToken: "legion-legion-legion-209-implementer",
  }
  const work = "the implementing phase of LEGION-209"
  for (const [named, topic] of [
    ["legion-legion-legion-209-architect", "notifications.role.legion-legion-legion-209-architect"],
    ["legion-legion-legion-208-architect", "notifications.role.legion-legion-legion-208-architect"],
    [undefined, "notifications.role.legion-legion-legion-208-architect"],
  ] as const) {
    const { endpoint, published, close } = await harness({
      record: recorded("active", 1),
      claim: child,
      env: { LEGION_STALL_ARCHITECT_AFTER: "2" },
      owed: () =>
        Response.json({ owes: true, work, ...(named === undefined ? {} : { architect: named }) }),
    })
    try {
      expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
        {},
      )
      await waitFor(async () => published.length === 1, "the architect to be told")
      expect(published[0]?.topic, String(named)).toBe(topic)
      expect(published[0]?.message).toContain("The implementer of LEGION-209")
    } finally {
      await close()
    }
  }
})

test("a timer whose turn has started again writes nothing", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    expect(
      await endpoint.handle({ event: "UserPromptSubmit", prompt: "Carry on with INFRA-220." }),
    ).toEqual({})
    // Three times the 20 ms the armed reminder was waiting for.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({
      state: "active",
      count: 0,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})

test("a resumed session the daemon says owes work is reminded once its claim has booted", async () => {
  const { record, inbox, boot, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
  })
  try {
    // Nothing is armed before the claim has booted: the reminder needs the claim it gates on.
    await Bun.sleep(40)
    expect(inbox).toEqual([])
    boot()
    await waitFor(async () => inbox.length > 0, "the resume reminder")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "active", count: 1, reported: false })
  } finally {
    await close()
  }
})

test("a resumed session whose task arrives first is not reminded on top of it", async () => {
  const { endpoint, inbox, boot, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
  })
  try {
    // The daemon re-sending an interrupted task arrives as a prompt, and drops the resume
    // reminder through the same generation guard.
    expect(
      await endpoint.handle({ event: "UserPromptSubmit", prompt: "Carry on with INFRA-220." }),
    ).toEqual({})
    boot()
    await Bun.sleep(60)
    expect(inbox).toEqual([])
  } finally {
    await close()
  }
})

test("a worker whose workspace was recreated is told once, as a turn of its own", async () => {
  const { endpoint, record, inbox, boot, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
    env: { LEGION_WORKSPACE_RECREATED: "true" },
    owed: owesNothing,
  })
  try {
    boot()
    await waitFor(async () => inbox.length > 0, "the recreated-workspace notice")
    expect(inbox[0]).toContain("from what was pushed to legion/LEGION-208")
    // Recorded in the record the rhythm and count live in, so a server Claude Code restarts
    // inside the same process does not repeat it.
    expect(await record()).toEqual({
      state: "active",
      count: 0,
      reported: false,
      recreatedNoticeSent: true,
    })
    // The turn it starts is an inbound message like any other: it says nothing of what is owed.
    await endpoint.handle({ event: "UserPromptSubmit", prompt: inbox[0] ?? "" })
    expect(await record()).toEqual({
      state: "active",
      count: 0,
      reported: false,
      recreatedNoticeSent: true,
    })
    await Bun.sleep(40)
    expect(inbox.length).toBe(1)
  } finally {
    await close()
  }
})

test("a record that already says the recreated-workspace notice was sent earns no second one", async () => {
  const { inbox, boot, close } = await harness({
    record: recorded("active", 0, { recreatedNoticeSent: true }),
    claim: implementer,
    env: { LEGION_WORKSPACE_RECREATED: "true" },
    owed: owesNothing,
  })
  try {
    boot()
    await Bun.sleep(40)
    expect(inbox).toEqual([])
  } finally {
    await close()
  }
})

test("a workspace recreated again, after a launch that recreated nothing, is told again", async () => {
  // Three launches of one session, each a server reading the record the one before it left: the
  // first recreated the workspace, the second did not, and the third recreated it again. The
  // launcher decides `LEGION_WORKSPACE_RECREATED` afresh for every launch, so the third is a new
  // recreation the worker has not been told of.
  const launch = async (record: string, recreated: "true" | "false") => {
    const {
      inbox,
      boot,
      record: written,
      close,
    } = await harness({
      record,
      claim: implementer,
      env: { LEGION_WORKSPACE_RECREATED: recreated, LEGION_STALL_RESUME_MS: "600000" },
    })
    try {
      boot()
      if (recreated === "true") {
        await waitFor(async () => inbox.length > 0, "the recreated-workspace notice")
      } else {
        // Nothing is to happen, so there is no condition to wait for: the file's own small
        // multiple of the 20 ms interval, as its other nothing-happens cases use.
        await Bun.sleep(40)
      }
      return { told: inbox.length, record: JSON.stringify(await written()) }
    } finally {
      await close()
    }
  }
  const first = await launch(recorded("active", 0), "true")
  expect(first.told).toBe(1)
  const second = await launch(first.record, "false")
  expect(second.told).toBe(0)
  const third = await launch(second.record, "true")
  expect(third.told).toBe(1)
  // Three launches, each of which may wait out `waitFor`'s own 5 s deadline.
}, 20_000)

test("the architect notice a resumed worker's reminder earns names what the worker last said before its relaunch", async () => {
  const before = await harness({
    record: recorded("active", 0),
    claim: implementer,
    env: { LEGION_STALL_ARCHITECT_AFTER: "3" },
  })
  let carried: string
  try {
    expect(
      await before.endpoint.handle({ event: "Stop", lastAssistantMessage: "Pushed the change." }),
    ).toEqual({ decision: "block", reason: REMINDER })
    expect(
      await before.endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Still pushing the change.",
      }),
    ).toEqual({})
    await waitFor(async () => before.inbox.length === 1, "the timer's reminder")
    carried = JSON.stringify(await before.record())
  } finally {
    await before.close()
  }

  // The relaunch: the same record, no task re-sent, so the resume reminder is the third
  // unanswered one and earns the architect notice.
  const after = await harness({
    record: carried,
    claim: implementer,
    env: { LEGION_STALL_ARCHITECT_AFTER: "3" },
  })
  try {
    after.boot()
    await waitFor(async () => after.published.length === 1, "the architect notice")
    const notice = after.published[0]
    expect(notice?.message).toContain("Still pushing the change.")
    expect(JSON.parse(String(notice?.payload)).lastMessage).toBe("Still pushing the change.")
  } finally {
    await after.close()
  }
})

test("a launcher that recreated nothing sends no notice and records no key", async () => {
  const { record, inbox, boot, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
    owed: owesNothing,
  })
  try {
    boot()
    await Bun.sleep(40)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "active", count: 0, reported: false })
  } finally {
    await close()
  }
})

// A restarted server reminds no role the daemon says owes nothing (dispatch://INFRA-2746): a
// server made afresh — with no record, or with one an earlier release wrote, its phase
// closed or open — holds nothing a restart could reset. While the daemon answers that the claim
// owes nothing it reports nothing, from its resume timer or from a `Stop`; once it answers that
// the work is owed, the same fresh server reminds.
test("a reminder made afresh reports nothing while the daemon answers that the claim owes nothing", async () => {
  for (const record of [undefined, recorded("closed", 0), recorded("open", 3)]) {
    const label = record ?? "no record"
    const { endpoint, inbox, asked, boot, answersOwed, close } = await harness({
      ...(record === undefined ? {} : { record }),
      claim: implementer,
      owed: owesNothing,
    })
    try {
      boot()
      await waitFor(async () => asked.length === 1, `the resume timer to ask the daemon (${label})`)
      expect(
        await endpoint.handle({ event: "Stop", lastAssistantMessage: "Idle." }),
        label,
      ).toEqual({})
      await Bun.sleep(60)
      expect(inbox, label).toEqual([])

      answersOwed(owesWork)
      expect(
        await endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement it." }),
        label,
      ).toEqual({})
      expect(
        await endpoint.handle({ event: "Stop", lastAssistantMessage: "Started." }),
        label,
      ).toEqual({ decision: "block", reason: REMINDER })
    } finally {
      await close()
    }
  }
})

// `quiet` and `text-call` are what an earlier build recorded for a phase nothing had been
// reminded about, so a session resumed on either stands at the rhythm's start.
for (const deployed of ["quiet", "text-call"]) {
  test(`a record an earlier build wrote as ${deployed} reads as the rhythm's start, with no reminders yet`, async () => {
    const { endpoint, record, close } = await harness({
      record: JSON.stringify({ state: deployed }),
      claim: implementer,
    })
    try {
      expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "" })).toEqual({
        decision: "block",
        reason: REMINDER,
      })
      expect(await record()).toEqual({ state: "active", count: 1, reported: false })
    } finally {
      await close()
    }
  })
}

test("a session holding no claim, as the bridge's seeding /rename turn does, is never reminded", async () => {
  const { endpoint, record, close } = await harness({ record: recorded("active", 0) })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Renamed." })).toEqual({})
    expect(await record()).toEqual({ state: "active", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("an architect's session runs no phase reminder", async () => {
  const { endpoint, record, asked, close } = await harness({
    record: recorded("active", 0),
    claim: { ...implementer, role: "architect" },
  })
  try {
    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Started the tester." }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "active", count: 0, reported: false })
    expect(asked).toEqual([])
  } finally {
    await close()
  }
})

test("a call naming no event, or one this endpoint does not know, is refused and moves nothing", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("active", 2),
    claim: implementer,
  })
  try {
    for (const call of [{}, { event: "SessionEnd" }, "Stop", undefined]) {
      expect(await endpoint.handle(call)).toEqual({ error: PHASE_HOOK_REFUSAL })
    }
    expect(await record()).toEqual({ state: "active", count: 2, reported: false })
  } finally {
    await close()
  }
})

test("a session outside a Legion pane has no inbox, and says so rather than losing the reminder", async () => {
  const { endpoint, lines, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
    inbox: false,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(
      async () => lines.some((line) => line.includes("has no way to reach this worker")),
      "the log line naming the missing inbox",
    )
  } finally {
    await close()
  }
})

// No turn starts on a line the bridge did not queue, so no `Stop` would arm the next reminder: the
// failed one is not counted, and the reminder is written again from the timer.
test("an inbox write the bridge refused is not counted, and the reminder comes again", async () => {
  const { endpoint, record, inbox, lines, close } = await harness({
    record: recorded("active", 1),
    claim: implementer,
    inboxAnswer: async (attempt) => {
      if (attempt === 1) throw new Error("the bridge did not queue the line")
    },
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => inbox.length === 2, "the refused reminder to be written again")
    expect(inbox).toEqual([REMINDER, REMINDER])
    expect(lines.some((line) => line.includes("a phase reminder did not reach this worker"))).toBe(
      true,
    )
    // One reminder reached the worker, so one is counted on top of the restored count.
    expect(await record()).toEqual({
      state: "active",
      count: 2,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})

// ─── A wait the worker declared, and the design gate's hold (dispatch://INFRA-2040) ───────────

/** Another delivery's key behind the listener's role-forward prefix, as `UPDATE_ASK_KEY`'s is:
 * news, which the daemon ends the wait for. */
const DELIVERY_KEY = "envoy.role.forward.publish.6f2a9c1e4b7d40a8b3e5c9d1f0a2b4c6"

/** The merger of the same tree, the claim the closed design gate holds; what the stand-in daemon
 * calls its work; the gate's hold as the owed answer gives it; and the closed note's sentence a
 * reminder built from that answer carries (the closed form of the daemon's
 * `workflow.MergerGateNote`). */
const merger: ClaimCapability = {
  ...implementer,
  role: "merger",
  claimToken: "legion-legion-legion-208-merger",
}
const MERGER_WORK = "the merging phase of LEGION-208"
const HOLD = {
  step: "spec",
  artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
  version: 7,
} as const
const CLOSED_NOTE =
  "The design gate of LEGION-208 closed: version 7 of its spec document " +
  "(d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093) is with its reviewer. Merge nothing, enqueue " +
  "nothing, and take a pull request you queued back out of the merge queue."
/** A tool call written as text, in the shape the Stage 3 model emitted, and the sentence the
 * reminder for it opens with. */
const TEXT_CALL = '<invoke name="legion"><parameter name="op">handoff_complete</parameter>'
const TEXT_CALL_SENTENCE =
  "Your last message holds a tool call written as text, so it did not run: make it a real tool call."

/** A phase worker's pane whose daemon is `standIn` (`waitStandIn`): the implementer unless
 * `claim` names another, on a record with no reminder counted unless `record` gives one. */
function waitingPane(
  standIn: WaitStandIn,
  options: { readonly claim?: ClaimCapability; readonly record?: string } = {},
): Promise<Harness> {
  return harness({
    record: options.record ?? recorded("active", 0),
    claim: options.claim ?? implementer,
    owed: standIn.owed,
    routes: standIn.answer,
  })
}

/** The worker's `declare_wait` through the pane's `legion` tool, as the channel server's CallTool
 * handler runs it: the real tool and daemon client, with the endpoint told the wait's id
 * (`onWaitDeclared`). The worker waits on CI, and the daemon wakes it at the wait's time if
 * nothing has. */
function declares(pane: Harness): Promise<unknown> {
  return executeLegionTool(
    {
      daemon: pane.daemon,
      claim: () => implementer,
      handoffReport: async () => {
        throw new Error("no case here completes")
      },
      branchHead: async () => {
        throw new Error("no case here answers an update")
      },
      onWaitDeclared: pane.endpoint.waitDeclared,
    },
    { op: "declare_wait", waiting_on: "CI on the pull request", wake_at: "2026-11-21T10:00:00Z" },
  )
}

/** The body of the implementer's report of news for `waitId`, with `deliveryKey` when given. */
function reportOf(waitId: string, deliveryKey?: string) {
  return {
    claimToken: implementer.claimToken,
    sessionId: SESSION,
    secret: implementer.secret,
    waitId,
    ...(deliveryKey === undefined ? {} : { deliveryKey }),
  }
}

/** The pane's channel server, as far as news goes (`channelOf`). */
interface Channel {
  /** The listener relays one message to the worker's role under `key`; resolves with the card the
   * channel wrote into the session once it has answered the frame `done`. */
  readonly relays: (key: string, summary: string) => Promise<string>
  readonly shutdown: () => Promise<void>
}

/**
 * The pane's channel server as far as news goes: a channel session on a fake NATS whose relay
 * writes each frame into the session as its card and tells the endpoint of it, as
 * `runEnvoyChannelServer` wires `deliveryWritten` to the phase endpoint's `news`. The key the
 * endpoint is told is the one `deliverRelayed` reads off the frame itself.
 */
async function channelOf(endpoint: PhaseHookEndpoint): Promise<Channel> {
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), scratchState(), {
      connection: nats,
      notifier,
      deliveryWritten: (key) => void endpoint.news(key),
    }),
  )
  let seq = 0
  return {
    relays: async (key, summary) => {
      seq += 1
      const reply = `_INBOX.${seq}`
      nats.emitRelay("ses_claude", {
        subject: directSubject,
        seq,
        reply,
        raw: JSON.stringify({
          event_id: `evt-${seq}`,
          dedupe_key: key,
          source: "agent",
          source_session: "ses_reviewer",
          topic: "notifications.role.legion-legion-legion-208-implementer",
          issued_at: 1_763_715_600_000,
          payload_summary: summary,
        }),
      })
      await waitFor(
        async () => nats.repliesOn(reply).includes("done"),
        `the frame under ${key} to be written`,
      )
      return notifier.notifications.at(-1)?.content ?? ""
    },
    shutdown: () => session.shutdown(),
  }
}

// R1, R3 and R9: a turn's end whose owed answer gives a wait the worker declared, or the design
// gate's hold, continues nothing, arms no timer and reports that no reminder is due, so the daemon
// can release the pod. The wait is the one the worker declared in that very turn, through the
// pane's `legion` tool. A case asserting that nothing happens has no condition to wait for, so
// this one waits out the file's longest delay (the quiet wait's 80 ms) and more.
test("a standing wait and a standing hold send no reminder, arm no timer and report no reminder due", async () => {
  for (const held of ["wait", "hold"] as const) {
    const standIn = waitStandIn(held === "hold" ? MERGER_WORK : WORK)
    if (held === "hold") standIn.state.hold = HOLD
    const pane = await waitingPane(standIn, held === "hold" ? { claim: merger } : {})
    try {
      await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Work LEGION-208." })
      if (held === "wait") {
        expect(await declares(pane)).toEqual({ wait: standIn.state.wait })
      }
      for (const said of ["Pushed; CI runs on the pull request.", "Still waiting."]) {
        expect(
          await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: said }),
          held,
        ).toEqual({})
        expect(pane.reports.at(-1)?.activity.phase.nextReminderAt, held).toBeNull()
      }
      const asked = pane.asked.length
      await Bun.sleep(120)
      // A timer would have asked the daemon again when it was due.
      expect(pane.asked, held).toHaveLength(asked)
      expect(pane.inbox, held).toEqual([])
      expect(await pane.record(), held).toMatchObject({ state: "active", count: 0 })
    } finally {
      await pane.close()
    }
  }
})

// R3 and D8: the channel server reports each delivery it writes after the declaration, under the
// frame's own key, whichever turn Claude Code takes it into: the declaring turn itself, as that
// turn's next step, or a turn of its own once the declaring turn has ended. The daemon ends the
// wait, and the turn's end that follows gets the reminder.
for (const taken of ["into the declaring turn", "into a turn of its own"] as const) {
  test(`a delivery the channel server writes after the declaration ends the wait, taken ${taken}`, async () => {
    const standIn = waitStandIn(WORK)
    const pane = await waitingPane(standIn)
    const channel = await channelOf(pane.endpoint)
    try {
      await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement LEGION-208." })
      await declares(pane)
      if (taken === "into a turn of its own") {
        expect(
          await pane.endpoint.handle({
            event: "Stop",
            lastAssistantMessage: "Declared the wait on CI.",
          }),
        ).toEqual({})
        expect(standIn.reports).toEqual([])
      }

      const card = await channel.relays(DELIVERY_KEY, "The review is in")
      if (taken === "into a turn of its own") {
        await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: card })
      }
      expect(
        await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: "Read the review." }),
      ).toEqual({ decision: "block", reason: REMINDER })

      expect(standIn.reports).toEqual([reportOf("wait-1", DELIVERY_KEY)])
      expect(standIn.state.wait).toBeUndefined()
      expect(await pane.record()).not.toHaveProperty("waitId")
    } finally {
      await channel.shutdown()
      await pane.close()
    }
  })
}

// D9: the update ask is a role message the channel server relays like any other, and it is
// reported under the frame's key exactly as the frame carries it, role-forward prefix included.
// The daemon knows that key for its own ask and answers `ended: false`, which keeps the wait
// standing in the plugin: the turn the ask started earns no reminder and arms nothing.
test("the update ask the channel server writes is reported with the frame's exact key, and the daemon's ended:false keeps the wait standing", async () => {
  const standIn = waitStandIn(WORK)
  const pane = await waitingPane(standIn)
  const channel = await channelOf(pane.endpoint)
  try {
    await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement LEGION-208." })
    await declares(pane)
    expect(
      await pane.endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Declared the wait on CI.",
      }),
    ).toEqual({})

    const card = await channel.relays(UPDATE_ASK_KEY, "Your pod is being updated")
    await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: card })
    expect(
      await pane.endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "CI still runs; nothing to push yet.",
      }),
    ).toEqual({})

    expect(standIn.reports).toEqual([reportOf("wait-1", UPDATE_ASK_KEY)])
    expect(standIn.state.wait).toMatchObject({ id: "wait-1" })
    const kept = await pane.record()
    expect(kept).toMatchObject({ waitId: "wait-1" })
    expect(kept).not.toHaveProperty("unsentNews")
    expect(pane.reports.at(-1)?.activity.phase.nextReminderAt).toBeNull()
    // Nothing is to happen: the file's fixed wait over its longest delay.
    await Bun.sleep(120)
    expect(pane.inbox).toEqual([])
  } finally {
    await channel.shutdown()
    await pane.close()
  }
})

// D8: a delivery the channel server writes before the declaration, in the declaring turn, names
// no wait, so it reports nothing, and the wait the worker then declares stands.
test("a delivery written before the declaration reports nothing and leaves the wait standing", async () => {
  const standIn = waitStandIn(WORK)
  const pane = await waitingPane(standIn)
  const channel = await channelOf(pane.endpoint)
  try {
    await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement LEGION-208." })
    await channel.relays(DELIVERY_KEY, "The review is in")
    await declares(pane)
    expect(
      await pane.endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Declared the wait on CI.",
      }),
    ).toEqual({})

    expect(standIn.reports).toEqual([])
    expect(standIn.state.wait).toMatchObject({ id: "wait-1" })
    expect(await pane.record()).toMatchObject({ waitId: "wait-1" })
    expect(pane.reports.at(-1)?.activity.phase.nextReminderAt).toBeNull()
  } finally {
    await channel.shutdown()
    await pane.close()
  }
})

// R3 and D8: a prompt of the daemon's after the declaration (the wait's own task, the resume task)
// reaches this host only as a `UserPromptSubmit`, which reports nothing and leaves the wait
// standing. A person's message the channel server writes (the router's `inbox`, which calls the
// endpoint's `news` with no key) is news with no key: the daemon ends the wait on it, and that
// turn's end gets the reminder.
test("a daemon prompt after the declaration leaves the wait standing, and a person's message is reported with no key", async () => {
  const standIn = waitStandIn(WORK)
  const pane = await waitingPane(standIn)
  try {
    await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement LEGION-208." })
    await declares(pane)
    expect(
      await pane.endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "Declared the wait on CI.",
      }),
    ).toEqual({})
    for (const prompt of [
      "The wait you declared on CI on the pull request reached its time.",
      "Resume LEGION-208: the daemon relaunched your process.",
    ]) {
      await pane.endpoint.handle({ event: "UserPromptSubmit", prompt })
      expect(
        await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: "CI still runs." }),
        prompt,
      ).toEqual({})
    }
    expect(standIn.reports).toEqual([])
    expect(standIn.state.wait).toMatchObject({ id: "wait-1" })

    await pane.endpoint.news()
    await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Where is the dashboard?" })
    expect(
      await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: "It is at /dash." }),
    ).toEqual({ decision: "block", reason: REMINDER })
    expect(standIn.reports).toEqual([reportOf("wait-1")])
    expect(standIn.state.wait).toBeUndefined()
  } finally {
    await pane.close()
  }
})

// D8: a report that fails, or that has no answer within the bound, ends the wait in the plugin
// and is kept, with its key, in the record file. The turn's end sends it again first; when that
// fails too, the turn gets the reminder although the daemon's answer still gives the wait. The
// process the daemon relaunches reads the record file, sends the report at its boot before it
// reads the owed answer, and the daemon ends the wait: the resume reminder is armed, and comes.
test("a failed report gets the reminder and is sent again, also by a relaunched process", async () => {
  const standIn = waitStandIn(WORK)
  const before = await waitingPane(standIn)
  const channel = await channelOf(before.endpoint)
  let carried: string
  try {
    await before.endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement LEGION-208." })
    await declares(before)
    expect(
      await before.endpoint.handle({ event: "Stop", lastAssistantMessage: "Declared the wait." }),
    ).toEqual({})
    standIn.state.failReports = 2

    const card = await channel.relays(DELIVERY_KEY, "The review is in")
    await waitFor(async () => standIn.reports.length === 1, "the first report of the delivery")
    await before.endpoint.handle({ event: "UserPromptSubmit", prompt: card })
    expect(
      await before.endpoint.handle({ event: "Stop", lastAssistantMessage: "Read the review." }),
    ).toEqual({ decision: "block", reason: REMINDER })
    expect(standIn.reports).toEqual([
      reportOf("wait-1", DELIVERY_KEY),
      reportOf("wait-1", DELIVERY_KEY),
    ])
    expect(standIn.state.wait).toMatchObject({ id: "wait-1" })
    // The record file carries the wait's id and the unsent report with its delivery key.
    expect(await before.record()).toMatchObject({
      waitId: "wait-1",
      unsentNews: [{ waitId: "wait-1", deliveryKey: DELIVERY_KEY }],
    })
    carried = await readFile(before.file, "utf8")
  } finally {
    await channel.shutdown()
    await before.close()
  }

  const relaunched = await waitingPane(standIn, { record: carried })
  try {
    relaunched.boot()
    await waitFor(
      async () => relaunched.inbox.length > 0,
      "the relaunched worker's resume reminder",
    )
    expect(relaunched.inbox).toEqual([REMINDER])
    expect(standIn.reports).toEqual([
      reportOf("wait-1", DELIVERY_KEY),
      reportOf("wait-1", DELIVERY_KEY),
      reportOf("wait-1", DELIVERY_KEY),
    ])
    expect(standIn.state.wait).toBeUndefined()
    const record = await relaunched.record()
    expect(record).not.toHaveProperty("unsentNews")
    expect(record).not.toHaveProperty("waitId")
    // The boot's activity report says when the resume reminder it armed is due.
    expect(Date.parse(String(relaunched.reports[0]?.activity.phase.nextReminderAt))).not.toBeNaN()
  } finally {
    await relaunched.close()
  }
})

// D8 and R6: a process relaunched while the wait stands reads the owed answer at its boot before
// it arms anything. On a record an earlier build wrote, which names no wait, it learns the wait
// from that answer, arms no resume reminder, and sends an activity report that says no reminder
// is due, so the daemon can release the pod. News the channel server writes then is reported
// against that wait.
test("a boot with a standing wait arms nothing and reports no reminder due, and news then ends that wait", async () => {
  const standIn = waitStandIn(WORK)
  standIn.state.wait = {
    id: "wait-7",
    waitingOn: "CI on the pull request",
    wakeAt: "2026-11-21T10:00:00Z",
    declaredAt: "2026-11-21T09:00:00.123456Z",
  }
  const pane = await waitingPane(standIn, { record: recorded("active", 1) })
  const channel = await channelOf(pane.endpoint)
  try {
    pane.boot()
    await waitFor(async () => pane.reports.length === 1, "the boot's activity report")
    expect(pane.asked).toHaveLength(1)
    expect(pane.reports[0]).toMatchObject({
      claimToken: implementer.claimToken,
      sessionId: SESSION,
      secret: implementer.secret,
      activity: {
        backgroundWork: { running: false, kinds: [] },
        phase: { nextReminderAt: null },
      },
    })
    expect(await pane.record()).toEqual({
      state: "active",
      count: 1,
      reported: false,
      waitId: "wait-7",
    })
    // Nothing is to happen: three times the resume reminder's delay.
    await Bun.sleep(60)
    expect(pane.inbox).toEqual([])
    expect(pane.asked).toHaveLength(1)

    const card = await channel.relays(DELIVERY_KEY, "The review is in")
    await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: card })
    expect(
      await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: "Read the review." }),
    ).toEqual({ decision: "block", reason: REMINDER })
    expect(standIn.reports).toEqual([reportOf("wait-7", DELIVERY_KEY)])
  } finally {
    await channel.shutdown()
    await pane.close()
  }
})

// R1, R9 and D15: the one reminder a wait or a hold lets through is the one for a tool call
// written as text, as pi-shared builds it. It says that the call did not run, asks for a real tool
// call and asks for no `handoff_complete`; during a hold it also names the closed gate, its step
// and its document's version, and tells the merger to merge nothing. It keeps every reminder's
// rhythm: the run's continuation first, then the reminder's own timer.
test("a tool call written as text is reminded during a wait and during a hold, and the hold's reminder names the closed gate", async () => {
  for (const held of ["wait", "hold"] as const) {
    const standIn = waitStandIn(held === "hold" ? MERGER_WORK : WORK)
    if (held === "hold") standIn.state.hold = HOLD
    const pane = await waitingPane(standIn, held === "hold" ? { claim: merger } : {})
    try {
      await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Work LEGION-208." })
      if (held === "wait") await declares(pane)

      const continued = await pane.endpoint.handle({
        event: "Stop",
        lastAssistantMessage: TEXT_CALL,
      })
      const text = held === "hold" ? `${TEXT_CALL_SENTENCE} ${CLOSED_NOTE}` : TEXT_CALL_SENTENCE
      expect(continued, held).toEqual({ decision: "block", reason: text })
      expect("reason" in continued ? continued.reason : "", held).not.toContain("handoff_complete")

      expect(
        await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: TEXT_CALL }),
        held,
      ).toEqual({})
      await waitFor(async () => pane.inbox.length > 0, `the timer's reminder during the ${held}`)
      expect(pane.inbox, held).toEqual([text])
      expect(standIn.reports, held).toEqual([])
    } finally {
      await pane.close()
    }
  }
})

// R1 and R9: a held merger's boot arms no resume reminder and reports no reminder due, and its
// turns' ends are reminded of nothing while the gate stays closed. Once the gate opens, the
// daemon's answer gives no hold, and the next turn's end with no completion gets the reminder.
test("a held merger is reminded of nothing until the gate opens, and the next turn's end then gets the reminder", async () => {
  const standIn = waitStandIn(MERGER_WORK)
  standIn.state.hold = HOLD
  const pane = await waitingPane(standIn, { claim: merger })
  try {
    pane.boot()
    await waitFor(async () => pane.reports.length === 1, "the boot's activity report")
    expect(pane.reports[0]?.activity.phase.nextReminderAt).toBeNull()

    await pane.endpoint.handle({ event: "UserPromptSubmit", prompt: "Merge LEGION-208." })
    expect(
      await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: "The gate is closed." }),
    ).toEqual({})
    expect(pane.reports.at(-1)?.activity.phase.nextReminderAt).toBeNull()
    expect(pane.inbox).toEqual([])

    // The gate opens, and its opening message starts a turn of the merger's.
    standIn.state.hold = undefined
    await pane.endpoint.handle({
      event: "UserPromptSubmit",
      prompt: "The design gate of LEGION-208 opened.",
    })
    expect(
      await pane.endpoint.handle({ event: "Stop", lastAssistantMessage: "The gate opened." }),
    ).toEqual({ decision: "block", reason: owedReminder(MERGER_WORK) })
  } finally {
    await pane.close()
  }
})
