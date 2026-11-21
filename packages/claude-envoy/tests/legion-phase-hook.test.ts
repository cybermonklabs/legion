import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { LegionStateResponse } from "@legion/contracts/legion-api"
import {
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport"
import {
  type ActivityInput,
  claimCaller,
  createLegionDaemonClient,
} from "@legion/pi-shared/daemon-client"
import { createHoldsRead } from "@legion/pi-shared/holds-read"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { REMINDER } from "@legion/pi-shared/phase-stall"
import { heldTakeNote, taskWaitingNote } from "@legion/pi-shared/task-notes"
import { legion } from "../package.json" with { type: "json" }
import { createInboxNotifier, type TakePending } from "../src/legion-inbox"
import {
  createPhaseHookEndpoint,
  PHASE_HOOK_REFUSAL,
  PHASE_HOOK_TOOL_NAME,
  type PhaseHookAnswer,
  type PhaseHookEndpoint,
  paneReminderLog,
  phaseHookToolDefinition,
  phaseStallFile,
} from "../src/legion-phase-hook"
import { waitFor } from "./channel-session-harness"

const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

/** The pane's claim: the implementer of LEGION-208, whose phase in the daemon's state fixture is
 * `implementing`, so the reminder's record check says this role still works this phase. */
const implementer: ClaimCapability = {
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
function recorded(state: string, count: number, extra: Record<string, unknown> = {}): string {
  return JSON.stringify({ state, count, reported: false, ...extra })
}

interface Harness {
  readonly endpoint: PhaseHookEndpoint
  readonly file: string
  /** The record on disk, or undefined when nothing has been written. */
  readonly record: () => Promise<unknown>
  /** The record as it stood when the daemon answered the record check. */
  readonly recordAtDaemonRead: () => unknown
  /** Every route this endpoint asked the daemon for, in order. */
  readonly daemonPaths: readonly string[]
  /** The lines this endpoint wrote into the bridge's inbox, in order. */
  readonly inbox: readonly string[]
  /** Every message published to the Envoy listener, in order. */
  readonly published: readonly PublishInput[]
  /** Every activity report this endpoint sent the daemon, in order. */
  readonly reports: readonly ActivityInput[]
  /** Settles the claim's boot, as `startLegionClaim`'s `booted` does. */
  readonly boot: () => void
  readonly lines: readonly string[]
  /** How many take-pending queries the endpoint has asked the bridge. */
  readonly takePendingQueries: () => number
  /** The body of each `claims/task` read the holds read made, in order. */
  readonly taskReads: readonly unknown[]
  /** How many of the holds read's `claims/task` reads the daemon has answered. Whatever reads a
   * count that includes an answer finds the holds read holding that answer too. */
  readonly answered: () => number
  /** Moves the holds read's clock on by `ms`. */
  readonly advance: (ms: number) => void
  readonly close: () => Promise<void>
}

async function harness(
  options: {
    readonly record?: string
    readonly claim?: ClaimCapability
    /** The state the daemon's record gives the implementer's claim; `working` in the fixture. */
    readonly claimState?: string
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
    /** What the bridge answers the `attempt`th take-pending query (`LegionInbox.takePending`):
     * nothing by default, which is what the inbox gives for a bridge that refused the query or
     * did not answer it. */
    readonly takePending?: (attempt: number) => Promise<TakePending | undefined>
    /** What the daemon answers the `attempt`th `claims/task` read, which only the holds read makes
     * here: a reading that names no held take by default. */
    readonly claimsTask?: (attempt: number) => Promise<Record<string, unknown>>
  } = {},
): Promise<Harness> {
  const directory = await mkdtemp(join(tmpdir(), "claude-envoy-phase-hook-"))
  const file = phaseStallFile(directory, SESSION)
  if (options.record !== undefined) await writeFile(file, options.record)
  const state = LegionStateResponse.parse(
    JSON.parse(
      await readFile(
        resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/state.json"),
        "utf8",
      ),
    ),
  ) as { issues: Record<string, { workers: Record<string, { claim: { state: string } }> }> }
  const implementerClaim = state.issues["LEGION-208"]?.workers["implementer"]?.claim
  if (options.claimState !== undefined && implementerClaim !== undefined) {
    implementerClaim.state = options.claimState
  }
  const record = async (): Promise<unknown> => {
    const raw = await readFile(file, "utf8").catch(() => undefined)
    return raw === undefined ? undefined : JSON.parse(raw)
  }
  let atRead: unknown
  const taskReads: unknown[] = []
  const paths: string[] = []
  // The reminder's record check reads one issue's record, never the whole project
  // (dispatch://INFRA-2694): the stub answers that route from the state golden's own issues.
  const fetchStub = (async (input, init) => {
    const path = new URL(input.toString()).pathname
    if (path === "/legion/v1/claims/task") {
      taskReads.push(JSON.parse(String(init?.body)))
      const reading = options.claimsTask?.(taskReads.length) ?? { task: null, finished: 0 }
      return Response.json(await reading)
    }
    paths.push(path)
    atRead = await record()
    const named = /^\/legion\/v1\/issues\/([^/]+)\/record$/.exec(path)
    if (named === null) return Response.json(state)
    const held = state.issues[decodeURIComponent(named[1] ?? "")]
    return held === undefined
      ? Response.json(
          { code: "RECORD_NOT_FOUND", error: `The daemon has no record for ${named[1]}` },
          { status: 404 },
        )
      : Response.json(held)
  }) as typeof fetch
  const daemon = createLegionDaemonClient("http://daemon.test", {
    pluginContract: legion.daemonApiVersion,
    fetch: fetchStub,
  })
  const lines: string[] = []
  let now = Date.parse("2026-11-19T20:00:00Z")
  let answered = 0
  const holdsRead = createHoldsRead({
    daemon: {
      task: (input, init) => {
        const reading = daemon.task(input, init)
        // Counted by a handler on the answer itself, attached ahead of the holds read's own: that
        // one runs straight after this one, before anything that can read the count.
        void reading.then(
          () => {
            answered += 1
          },
          () => undefined,
        )
        return reading
      },
    },
    caller: () =>
      options.claim === undefined ? undefined : claimCaller(options.claim, options.claim.sessionID),
    now: () => now,
    failed: (error) => {
      lines.push(`holds read failed: ${String(error)}`)
    },
  })
  let takePendingQueries = 0
  const inbox: string[] = []
  const published: PublishInput[] = []
  const reports: ActivityInput[] = []
  const booted = Promise.withResolvers<void>()
  const endpoint = createPhaseHookEndpoint({
    stateFile: file,
    claim: () => options.claim,
    booted: booted.promise,
    readRecord: (issue, signal) => daemon.record(issue, { signal }),
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
    takePending: async () => {
      takePendingQueries += 1
      return options.takePending?.(takePendingQueries)
    },
    holdsRead,
  })
  return {
    endpoint,
    file,
    record,
    recordAtDaemonRead: () => atRead,
    inbox,
    daemonPaths: paths,
    published,
    reports,
    boot: booted.resolve,
    lines,
    takePendingQueries: () => takePendingQueries,
    taskReads,
    answered: () => answered,
    advance: (ms) => {
      now += ms
    },
    close: async () => {
      // As the server's own `stop` does: no armed timer outlives the case that armed it.
      endpoint.stop()
      await rm(directory, { recursive: true, force: true })
    },
  }
}

test("the tool names itself Claude Code's hook endpoint and tells the model not to call it", () => {
  const definition = phaseHookToolDefinition()
  expect(definition.name).toBe(PHASE_HOOK_TOOL_NAME)
  expect(definition.description).toContain("hook endpoint")
  expect(definition.description).toContain("Do not call it")
})

test("a turn settling on an open phase is blocked once, with the reminder, and the count is recorded before the answer", async () => {
  const { endpoint, record, recordAtDaemonRead, inbox, daemonPaths, close } = await harness({
    record: recorded("open", 0),
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
    // The record check reads the daemon before the reminder is counted, and the count is on disk
    // by the time the hook answers: a reminder the worker has been given is never uncounted. What
    // the worker said rides the record with it, for a relaunch's architect notice to quote.
    const said = { reported: false, lastMessage: "I pushed the branch." }
    expect(recordAtDaemonRead()).toEqual({ state: "open", count: 0, ...said })
    expect(await record()).toEqual({ state: "open", count: 1, ...said })
    // The check reads this issue's record and never the whole project
    // (dispatch://INFRA-2694), which is what the channel server wires it to.
    expect(daemonPaths).toEqual(["/legion/v1/issues/LEGION-208/record"])

    // The second settle of the run lets the turn end and carries its reminder from the timer, so
    // the daemon sees the turn end and the idle worker can take a task the daemon wrote for it.
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
      state: "open",
      count: 2,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})

// INFRA-945: every settle tells the daemon what this session is doing, so the idle rule can
// decide whether the pane's pod may be released. A turn ending with a background task of the
// worker's own still running names it; a report the daemon refuses costs the worker nothing.
test("the Stop hook reports what this session is doing to the daemon", async () => {
  const { endpoint, reports, close } = await harness({
    record: recorded("open", 0),
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
    record: recorded("open", 0),
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
  const { endpoint, reports, close } = await harness({ record: recorded("open", 0) })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Pushed." })).toEqual({})
    expect(reports).toEqual([])
  } finally {
    await close()
  }
})

test("a settle that says what it waits on buys the quiet wait, and is reminded once after it", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({
        event: "Stop",
        lastAssistantMessage: "WAITING on CI to go green on the branch.",
      }),
    ).toEqual({})
    const waiting = { reported: false, lastMessage: "WAITING on CI to go green on the branch." }
    expect(await record()).toEqual({ state: "waiting", count: 0, ...waiting })
    // Nothing is sent into the wait itself.
    await Bun.sleep(30)
    expect(inbox).toEqual([])
    await waitFor(async () => inbox.length > 0, "the reminder the wait earned")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "open", count: 1, ...waiting })
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
// idle. What stops the reminding of a session the provider refuses for ever is the daemon failing
// the claim at `launch_failure_limit`, which the test below this one holds.
test("a turn that ended on an API error carries no continuation, and is woken once by its timer", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 0, { lastMessage: "I pushed the branch." }),
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
      state: "open",
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
      state: "open",
      count: 1,
      reported: false,
      lastMessage: "I pushed the branch.",
    })
  } finally {
    await close()
  }
})

// The daemon failing this role's claim is its word that this session is finished: the retry it
// starts runs in a session of its own. It is what ends the reminding here, since this host sees
// a refused turn only as the `StopFailure` of the turn it refused.
test("no reminder is sent once the daemon has failed this role's claim", async () => {
  const { endpoint, record, inbox, lines, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
    claimState: "failed",
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    // A case asserting that nothing happens has no condition to wait for, so this is one of the
    // file's fixed waits (its header says why a fake clock settles nothing here), three times
    // the reminder interval above.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({
      state: "open",
      count: 1,
      reported: false,
      lastMessage: "Still here.",
    })
    expect(lines.some((line) => line.includes("the daemon has failed this claim"))).toBe(true)
  } finally {
    await close()
  }
})

test("a successful handoff_complete closes the phase, and the next settle blocks nothing", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
  })
  try {
    // What the server's CallTool handler does once the `legion` tool returned without throwing
    // (`./src/envoy-channel-server`): the worker completed its phase, so nothing is open to
    // remind it about any more.
    endpoint.phaseClosed()
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })

    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Completed the phase." }),
    ).toEqual({})
    // Neither a continuation nor a timer: the record check is not what stands between a finished
    // worker and a reminder telling it to complete again. A case asserting that nothing happens
    // has no condition to wait for, so this is one of the file's fixed waits, three times the
    // reminder interval above.
    await Bun.sleep(60)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })
  } finally {
    await close()
  }
})

// INFRA-484: the update message reaches Claude Code while the phase is open, and
// `ready_for_update` closes no phase. The `Stop` of the turn the worker answers in must still
// block nothing: a blocked `Stop` continues that same turn, so the work it did would go with the
// pod, and before the round's bound the daemon waits for every turn to end.
test("a ready_for_update answer blocks no Stop: the answering turn ends, with the phase still open", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 1),
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
    // a prompt, which starts a turn the daemon reads as the answer retracted — the round would
    // restart at every wait and never converge (INFRA-484, the `awaiting-update` state of
    // `@legion/pi-shared/phase-stall`, which a record holds as the plain `waiting`). A case
    // asserting that nothing happens has no condition to wait for, so this is one of the file's
    // fixed waits, over the quiet wait itself and so over every wait this endpoint arms, and the
    // endpoint's own `setTimeout` is what runs on the real clock. The WAITING reply's own wait
    // keeps its reminder, which its own case holds.
    await Bun.sleep(200)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "waiting", count: 0, ...said })
  } finally {
    await close()
  }
})

test("the prompt that carries this endpoint's own reminder does not reset its count", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 1),
    claim: implementer,
  })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => inbox.length > 0, "the timer's reminder to reach the inbox")
    const said = { reported: false, lastMessage: "Still here." }
    expect(await record()).toEqual({ state: "open", count: 2, ...said })
    expect(await endpoint.handle({ event: "UserPromptSubmit", prompt: inbox[0] ?? "" })).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 2, ...said })
  } finally {
    await close()
  }
})

test("a prompt carrying a line this server wrote for a delivery is that delivery's turn", async () => {
  const delivery = "Envoy delivery from the architect: the architect asks for a status"
  const open = await harness({ record: recorded("open", 2), claim: implementer })
  try {
    open.endpoint.deliveryWritten(delivery)
    expect(await open.endpoint.handle({ event: "UserPromptSubmit", prompt: delivery })).toEqual({})
    expect(await open.record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await open.close()
  }

  // A delivery is something happening, but it is not an assignment: it reopens no closed phase.
  // The line is dropped as its prompt arrives, so the same text next time is an assignment.
  const closed = await harness({ record: recorded("closed", 0), claim: implementer })
  try {
    closed.endpoint.deliveryWritten(delivery)
    await closed.endpoint.handle({ event: "UserPromptSubmit", prompt: delivery })
    expect(await closed.record()).toEqual({ state: "closed", count: 0, reported: false })
    await closed.endpoint.handle({ event: "UserPromptSubmit", prompt: delivery })
    expect(await closed.record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await closed.close()
  }
})

test("a prompt carrying anything else is the daemon's assignment and opens the phase", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("closed", 0),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({ event: "UserPromptSubmit", prompt: "Implement INFRA-220." }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

// A task's turn starts with the text its accepted take answered, which the bridge writes to Claude
// Code itself (`packages/daemon/internal/claudebridge/take.go`): it is no line this server wrote,
// so its prompt is the assignment, a note the daemon put in front of the text included. A card
// this server wrote while the take was pending is still a delivery when its own prompt arrives
// behind the task's: it reopens no phase the worker closed in between.
test("a taken task's prompt is the assignment, with a card waiting behind it and a note in front of it", async () => {
  const card = "Envoy delivery from the architect: the architect asks for a status"
  const taken =
    "Your previous turn on this task stopped because of a provider error (HTTP 529). Your work did not cause it.\n\nImplement INFRA-220."
  const { endpoint, record, close } = await harness({
    record: recorded("closed", 0),
    claim: implementer,
  })
  try {
    endpoint.deliveryWritten(card)
    expect(await endpoint.handle({ event: "UserPromptSubmit", prompt: taken })).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
    endpoint.phaseClosed()
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })
    expect(await endpoint.handle({ event: "UserPromptSubmit", prompt: card })).toEqual({})
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a helper agent's tool call is not the worker working, and does not reset the count", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
  })
  try {
    for (const event of ["PostToolUse", "PostToolUseFailure"]) {
      expect(await endpoint.handle({ event, toolName: "Bash", agentId: "agent-7" })).toEqual({})
      expect(await record()).toEqual({ state: "open", count: 2, reported: false })
    }
    expect(await endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })).toEqual(
      {},
    )
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a tool call that failed is work too, and resets the count", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
  })
  try {
    expect(
      await endpoint.handle({ event: "PostToolUseFailure", toolName: "Bash", agentId: "" }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

/** A `PostToolUse` answer that hands the model `notes`, joined by a blank line, as the hook's
 * `additionalContext`. */
function noted(...notes: string[]): PhaseHookAnswer {
  return {
    hookSpecificOutput: { hookEventName: "PostToolUse", additionalContext: notes.join("\n\n") },
  }
}

// R6 of dispatch://INFRA-4130 on Claude Code: a task the claim's take holds is the worker's next
// turn, and a reminder says nothing that task does not. While the bridge answers that a take is
// pending, the `Stop` continues nothing and counts nothing, so the turn ends and the bridge takes
// the task at its end; the reminder waits on its timer, which sends nothing while the take is
// still pending and reminds once it is not.
test("the Stop hook blocks nothing while the bridge holds a pending take", async () => {
  // S18: a value the bridge has not asked about yet. S19a: a take of it the daemon refused
  // `in-turn` during this turn, which the bridge holds pending until the turn's `result`, and
  // whose number it names as the task this turn did not take.
  for (const waiting of [0, 4]) {
    let pending = true
    const { endpoint, record, inbox, takePendingQueries, close } = await harness({
      record: recorded("open", 0),
      claim: implementer,
      takePending: async () => ({ pending, waiting }),
    })
    try {
      expect(
        await endpoint.handle({ event: "Stop", lastAssistantMessage: "I pushed the branch." }),
      ).toEqual({})
      expect(takePendingQueries()).toBe(1)
      const said = { reported: false, lastMessage: "I pushed the branch." }
      expect(await record()).toEqual({ state: "open", count: 0, ...said })

      // The timer the settle armed asks the bridge again when it fires, and sends nothing.
      await waitFor(async () => takePendingQueries() >= 3, "the armed timer to ask the bridge")
      expect(inbox).toEqual([])
      expect(await record()).toEqual({ state: "open", count: 0, ...said })

      // The daemon refused the take for a hold that stands: nothing is pending, and the reminding
      // goes on as before.
      pending = false
      await waitFor(async () => inbox.length > 0, "the reminder once no take is pending")
      expect(inbox).toEqual([REMINDER])
      expect(await record()).toEqual({ state: "open", count: 1, ...said })
    } finally {
      await close()
    }
  }
})

test("the Stop hook blocks as before when the bridge does not answer whether a take is pending", async () => {
  // The inbox's own query against a socket no bridge listens at, and what that query gives for a
  // bridge that refuses the kind, as an older bridge does, for an answer with no `takePending`,
  // and for no answer within its bound (each held in tests/legion-inbox.test.ts): no answer.
  const unanswered = [
    createInboxNotifier(join(tmpdir(), "claude-envoy-no-bridge", "inbox.sock")).takePending,
    async () => undefined,
  ]
  for (const takePending of unanswered) {
    const { endpoint, record, takePendingQueries, close } = await harness({
      record: recorded("open", 0),
      claim: implementer,
      takePending,
    })
    try {
      expect(
        await endpoint.handle({ event: "Stop", lastAssistantMessage: "I pushed the branch." }),
      ).toEqual({ decision: "block", reason: REMINDER })
      expect(takePendingQueries()).toBe(1)
      expect(await record()).toEqual({
        state: "open",
        count: 1,
        reported: false,
        lastMessage: "I pushed the branch.",
      })
    } finally {
      await close()
    }
  }
})

// R1 of dispatch://INFRA-4130 on Claude Code: a task value that reaches the bridge while a turn
// runs waits for that turn to end. The worker's own `PostToolUse` says so, once for each task,
// naming the task the bridge says the turn running did not take.
test("a PostToolUse while the bridge holds an unasked task value adds one note naming its number", async () => {
  let waiting = 4
  const { endpoint, record, takePendingQueries, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
    takePending: async () => ({ pending: true, waiting }),
  })
  const toolCall = () => endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })
  try {
    expect(await toolCall()).toEqual(noted(taskWaitingNote(4)))
    // The call is the worker working all the same: the count goes back to 0, as at every call.
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
    // Once for each task: the next call carries nothing, and a newer value gets a note of its own.
    expect(await toolCall()).toEqual({})
    waiting = 5
    expect(await toolCall()).toEqual(noted(taskWaitingNote(5)))
    // A number noted before, and a turn the bridge names no waiting task in, add nothing.
    waiting = 4
    expect(await toolCall()).toEqual({})
    waiting = 0
    expect(await toolCall()).toEqual({})
    expect(takePendingQueries()).toBe(5)
  } finally {
    await close()
  }
})

// R2 of dispatch://INFRA-4130 on Claude Code: the daemon holds another role's take of this issue
// behind this turn, and names it in the `holds` of this claim's own `claims/task`. A `PostToolUse`
// of the worker's own starts that read, at most once a minute, and is answered without waiting
// for it; the first `PostToolUse` after the answer notes each held take once. A read the turn's
// end overtakes notes nothing: the daemon accepts each take it names at that end.
test("a PostToolUse while the daemon names a take held behind this turn adds one note naming the role and its number", async () => {
  const readings: PromiseWithResolvers<Record<string, unknown>>[] = []
  let waiting = 0
  const { endpoint, taskReads, answered, advance, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
    takePending: async () => ({ pending: waiting > 0, waiting }),
    claimsTask: () => {
      const reading = Promise.withResolvers<Record<string, unknown>>()
      readings.push(reading)
      return reading.promise
    },
  })
  const toolCall = () => endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })
  const holding = (...holds: { role: string; task: number }[]) => ({
    task: null,
    finished: 3,
    holds,
  })
  try {
    // The first call starts the read with the claim's caller, and is answered before the read is.
    expect(await toolCall()).toEqual({})
    expect(taskReads).toEqual([
      {
        claimToken: implementer.claimToken,
        sessionId: SESSION,
        secret: implementer.secret,
        generation: implementer.generation,
      },
    ])
    readings[0]?.resolve(holding({ role: "tester", task: 7 }))
    await waitFor(async () => answered() === 1, "the holds read's answer")
    expect(await toolCall()).toEqual(noted(heldTakeNote("tester", 7)))
    // Once for each held take, and no second read inside the minute.
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(1)

    // A minute on, the next call reads again. A take noted before gets no second note, a newly
    // named one gets its own, and the claim's own task, waiting too, goes first.
    advance(60_000)
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(2)
    readings[1]?.resolve(holding({ role: "tester", task: 7 }, { role: "reviewer", task: 9 }))
    await waitFor(async () => answered() === 2, "the second holds read's answer")
    waiting = 6
    expect(await toolCall()).toEqual(noted(taskWaitingNote(6), heldTakeNote("reviewer", 9)))

    // A read still unanswered when the turn ends is dropped, and so is what the reads of the
    // ended turn named: the next turn's calls note nothing of it.
    advance(60_000)
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(3)
    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "WAITING on the tester." }),
    ).toEqual({})
    readings[2]?.resolve(holding({ role: "planner", task: 11 }))
    await endpoint.handle({ event: "UserPromptSubmit", prompt: "Carry on with LEGION-208." })
    await waitFor(async () => answered() === 3, "the ended turn's holds read's answer")
    expect(await toolCall()).toEqual({})
    // The planner's take, still held a minute into the new turn, is noted from that turn's read.
    advance(60_000)
    expect(await toolCall()).toEqual({})
    readings[3]?.resolve(holding({ role: "planner", task: 11 }))
    await waitFor(async () => answered() === 4, "the new turn's holds read's answer")
    expect(await toolCall()).toEqual(noted(heldTakeNote("planner", 11)))
  } finally {
    await close()
  }
})

// Risk (c) of dispatch://INFRA-4130: a turn can end with neither `Stop` nor `StopFailure` (one the
// user interrupts), so the holds answer it kept is still kept when the next turn starts. The
// daemon accepts each take that answer names at that end, so it waits no more: the next turn's
// `UserPromptSubmit` drops it where its read started a minute or more ago, as Oh My Pi's run start
// does, and that turn notes it only where a read of its own names it again.
test("a holds answer of a turn that ended with no Stop is not noted in the next turn", async () => {
  const { endpoint, taskReads, answered, advance, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
    claimsTask: async () => ({ task: null, finished: 3, holds: [{ role: "tester", task: 7 }] }),
  })
  const toolCall = () => endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })
  try {
    // The turn's first call starts the read, which names the tester's take; the turn is
    // interrupted before a call notes it, and no `Stop` or `StopFailure` comes. The next turn
    // starts a minute after that read started.
    expect(await toolCall()).toEqual({})
    await waitFor(async () => answered() === 1, "the holds read's answer")
    advance(60_000)
    await endpoint.handle({ event: "UserPromptSubmit", prompt: "Carry on with LEGION-208." })

    // The next turn's first call notes nothing of the ended turn's answer, and starts a read of
    // its own, a minute having passed.
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(2)

    // That read names the take again, and the next call notes it once.
    await waitFor(async () => answered() === 2, "the new turn's holds read's answer")
    expect(await toolCall()).toEqual(noted(heldTakeNote("tester", 7)))
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(2)
  } finally {
    await close()
  }
})

// Claude Code may fire `UserPromptSubmit` for a message the bridge injects while a turn runs, so
// the reset there keeps a holds read still in flight: its answer is at most
// `HOLDS_READ_BOUND_MS` old, and dropping it would put R2's note a minute past its bound.
test("a UserPromptSubmit while a holds read is in flight still gives one note from that read's answer", async () => {
  const reading = Promise.withResolvers<Record<string, unknown>>()
  const { endpoint, taskReads, answered, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
    claimsTask: () => reading.promise,
  })
  const toolCall = () => endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })
  try {
    // The worker's call starts the read, whose answer the daemon holds back; the prompt comes
    // while it is in flight, and the answer after it.
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(1)
    await endpoint.handle({ event: "UserPromptSubmit", prompt: "A message for LEGION-208." })
    reading.resolve({ task: null, finished: 3, holds: [{ role: "tester", task: 7 }] })
    await waitFor(async () => answered() === 1, "the holds read's answer")

    // The next call notes the take that answer names, and the one after notes nothing.
    expect(await toolCall()).toEqual(noted(heldTakeNote("tester", 7)))
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(1)
  } finally {
    await close()
  }
})

// Claude Code may fire `UserPromptSubmit` for a message the bridge injects while a turn runs,
// which ends no turn, so the reset there keeps an answer whose read started less than a minute
// ago too: the next read starts only a minute after that one did, and waiting for its answer
// would put R2's note past its bound.
test("a UserPromptSubmit keeps a holds answer whose read started less than a minute ago", async () => {
  const { endpoint, taskReads, answered, advance, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
    claimsTask: async () => ({ task: null, finished: 3, holds: [{ role: "tester", task: 7 }] }),
  })
  const toolCall = () => endpoint.handle({ event: "PostToolUse", toolName: "Bash", agentId: "" })
  try {
    // The worker's call starts the read, whose answer names the tester's take; the prompt comes
    // half a minute after that read started.
    expect(await toolCall()).toEqual({})
    await waitFor(async () => answered() === 1, "the holds read's answer")
    advance(30_000)
    await endpoint.handle({ event: "UserPromptSubmit", prompt: "A message for LEGION-208." })

    // The next call notes the take that answer names, the one after notes nothing, and no second
    // read started.
    expect(await toolCall()).toEqual(noted(heldTakeNote("tester", 7)))
    expect(await toolCall()).toEqual({})
    expect(taskReads).toHaveLength(1)
  } finally {
    await close()
  }
})

// Rows S5 and S17 of dispatch://INFRA-4130: a helper agent's tool call is not the worker, which
// alone can end the turn a note asks it to end. It asks the bridge nothing, starts no holds read,
// and uses up no note: the worker's own next call carries them.
test("a subagent's PostToolUse carries no task note", async () => {
  const { endpoint, record, takePendingQueries, taskReads, answered, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
    takePending: async () => ({ pending: true, waiting: 4 }),
    claimsTask: async () => ({ task: null, finished: 3, holds: [{ role: "tester", task: 7 }] }),
  })
  const subagent = { toolName: "Bash", agentId: "agent-7" }
  const worker = { event: "PostToolUse", toolName: "Bash", agentId: "" }
  try {
    for (const event of ["PostToolUse", "PostToolUseFailure"]) {
      expect(await endpoint.handle({ event, ...subagent })).toEqual({})
    }
    expect(takePendingQueries()).toBe(0)
    expect(taskReads).toEqual([])
    expect(await record()).toEqual({ state: "open", count: 2, reported: false })

    expect(await endpoint.handle(worker)).toEqual(noted(taskWaitingNote(4)))
    expect(taskReads).toHaveLength(1)
    await waitFor(async () => answered() === 1, "the holds read's answer")
    expect(await endpoint.handle({ event: "PostToolUse", ...subagent })).toEqual({})
    expect(await endpoint.handle(worker)).toEqual(noted(heldTakeNote("tester", 7)))
  } finally {
    await close()
  }
})

test("the architect is told at the threshold, asked again when the listener kept nothing, and never twice", async () => {
  const { endpoint, record, inbox, published, close } = await harness({
    record: recorded("open", 1),
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
    expect(published[0]?.message).toContain("2 phase reminders")

    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Still here." })).toEqual(
      {},
    )
    await waitFor(async () => published.length === 2, "the architect to be told again")
    expect(published[1]?.idempotencyKey).toBe(refused.reportKey)
    expect(await record()).toEqual({
      state: "open",
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

test("a timer whose turn has started again writes nothing", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("open", 1),
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
      state: "open",
      count: 0,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})

test("a session resumed on an open phase is reminded once its claim has booted", async () => {
  const { record, inbox, boot, close } = await harness({
    record: recorded("open", 0),
    claim: implementer,
  })
  try {
    // Nothing is armed before the claim has booted: the reminder needs the claim it gates on.
    await Bun.sleep(40)
    expect(inbox).toEqual([])
    boot()
    await waitFor(async () => inbox.length > 0, "the resume reminder")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "open", count: 1, reported: false })
  } finally {
    await close()
  }
})

test("a resumed session whose task arrives first is not reminded on top of it", async () => {
  const { endpoint, inbox, boot, close } = await harness({
    record: recorded("open", 0),
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
    record: recorded("closed", 0),
    claim: implementer,
    env: { LEGION_WORKSPACE_RECREATED: "true" },
  })
  try {
    boot()
    await waitFor(async () => inbox.length > 0, "the recreated-workspace notice")
    expect(inbox[0]).toContain("it holds what was pushed to legion/LEGION-208")
    // Recorded in the record the phase and count live in, so a server Claude Code restarts inside
    // the same process does not repeat it.
    expect(await record()).toEqual({
      state: "closed",
      count: 0,
      reported: false,
      recreatedNoticeSent: true,
    })
    // The turn it starts is this server's own line, not the daemon's assignment: a phase that was
    // over stays over.
    await endpoint.handle({ event: "UserPromptSubmit", prompt: inbox[0] ?? "" })
    expect(await record()).toEqual({
      state: "closed",
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
    record: recorded("closed", 0, { recreatedNoticeSent: true }),
    claim: implementer,
    env: { LEGION_WORKSPACE_RECREATED: "true" },
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
  const first = await launch(recorded("closed", 0), "true")
  expect(first.told).toBe(1)
  const second = await launch(first.record, "false")
  expect(second.told).toBe(0)
  const third = await launch(second.record, "true")
  expect(third.told).toBe(1)
  // Three launches, each of which may wait out `waitFor`'s own 5 s deadline.
}, 20_000)

test("the architect notice a resumed worker's reminder earns names what the worker last said before its relaunch", async () => {
  const before = await harness({
    record: recorded("open", 0),
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
    record: recorded("closed", 0),
    claim: implementer,
  })
  try {
    boot()
    await Bun.sleep(40)
    expect(inbox).toEqual([])
    expect(await record()).toEqual({ state: "closed", count: 0, reported: false })
  } finally {
    await close()
  }
})

// `quiet` and `text-call` are what the build now in the pods records for an open phase nothing has
// been reminded about, so a session resumed on either is reminded rather than left alone.
for (const deployed of ["quiet", "text-call"]) {
  test(`a record the deployed build wrote as ${deployed} reads as an open phase with no reminders yet`, async () => {
    const { endpoint, record, close } = await harness({
      record: JSON.stringify({ state: deployed }),
      claim: implementer,
    })
    try {
      expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "" })).toEqual({
        decision: "block",
        reason: REMINDER,
      })
      expect(await record()).toEqual({ state: "open", count: 1, reported: false })
    } finally {
      await close()
    }
  })
}

test("a session holding no claim, as the bridge's seeding /rename turn does, is never reminded", async () => {
  const { endpoint, record, close } = await harness({ record: recorded("open", 0) })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Renamed." })).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("an architect's session runs no phase reminder", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 0),
    claim: { ...implementer, role: "architect" },
  })
  try {
    expect(
      await endpoint.handle({ event: "Stop", lastAssistantMessage: "Started the tester." }),
    ).toEqual({})
    expect(await record()).toEqual({ state: "open", count: 0, reported: false })
  } finally {
    await close()
  }
})

test("a call naming no event, or one this endpoint does not know, is refused and moves nothing", async () => {
  const { endpoint, record, close } = await harness({
    record: recorded("open", 2),
    claim: implementer,
  })
  try {
    for (const call of [{}, { event: "SessionEnd" }, "Stop", undefined]) {
      expect(await endpoint.handle(call)).toEqual({ error: PHASE_HOOK_REFUSAL })
    }
    expect(await record()).toEqual({ state: "open", count: 2, reported: false })
  } finally {
    await close()
  }
})

test("a session with no record at all is no record, not a fresh open phase", async () => {
  const { endpoint, record, close } = await harness({ claim: implementer })
  try {
    expect(await endpoint.handle({ event: "Stop", lastAssistantMessage: "Done." })).toEqual({})
    expect(await record()).toBeUndefined()
  } finally {
    await close()
  }
})

test("a session outside a Legion pane has no inbox, and says so rather than losing the reminder", async () => {
  const { endpoint, lines, close } = await harness({
    record: recorded("open", 1),
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
    record: recorded("open", 1),
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
      state: "open",
      count: 2,
      reported: false,
      lastMessage: "Still here.",
    })
  } finally {
    await close()
  }
})
