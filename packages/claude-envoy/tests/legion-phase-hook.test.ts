import { expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import {
  EnvoyApiError,
  type PublishInput,
  type PublishResult,
} from "@legion/envoy-client/transport"
import { type ActivityInput, createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { owedReminder, UNREAD_REMINDER } from "@legion/pi-shared/phase-stall"
import {
  createPhaseHookEndpoint,
  PHASE_HOOK_REFUSAL,
  PHASE_HOOK_TOOL_NAME,
  type PhaseHookEndpoint,
  paneReminderLog,
  phaseHookToolDefinition,
  phaseStallFile,
} from "../src/legion-phase-hook"
import { waitFor } from "./channel-session-harness"

const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"

/** The pane's claim: the implementer of LEGION-208. */
const implementer: ClaimCapability = {
  sessionID: SESSION,
  claimToken: "legion-legion-legion-208-implementer",
  generation: 5,
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
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
  /** The record on disk, or undefined when nothing has been written. */
  readonly record: () => Promise<unknown>
  /** The record as it stood when the daemon was asked what the claim owes. */
  readonly recordAtDaemonRead: () => unknown
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
  )
  const record = async (): Promise<unknown> => {
    const raw = await readFile(file, "utf8").catch(() => undefined)
    return raw === undefined ? undefined : JSON.parse(raw)
  }
  let atRead: unknown
  let owed = options.owed ?? owesWork
  const asked: unknown[] = []
  const fetchStub = (async (input, init) => {
    if (new URL(input.toString()).pathname === "/legion/v1/claims/owed") {
      atRead = await record()
      asked.push(JSON.parse(String(init?.body)))
      return owed()
    }
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
    readOwed: (question, signal) => daemon.owed(question, { signal }),
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

test("the tool names itself Claude Code's hook endpoint and tells the model not to call it", () => {
  const definition = phaseHookToolDefinition()
  expect(definition.name).toBe(PHASE_HOOK_TOOL_NAME)
  expect(definition.description).toContain("hook endpoint")
  expect(definition.description).toContain("Do not call it")
})

test("a turn settling while the daemon says the claim owes work is blocked once, with the reminder naming that work, and the count is recorded before the answer", async () => {
  const { endpoint, record, recordAtDaemonRead, inbox, asked, close } = await harness({
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

// R5, both ways on this host: the daemon's answer that the claim owes nothing blocks no `Stop`,
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

// R5: a daemon that does not answer — unreachable, refusing, or answering what the contract does
// not describe — still has the worker nudged, and the nudge says it could not read the record:
// it names no phase and no operation, because it read neither.
test("a daemon that does not say what the claim owes still blocks the Stop, with a reminder that names no phase and no operation", async () => {
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

test("a settle that says what it waits on buys the quiet wait, and is reminded once after it", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("active", 0),
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
    expect(await record()).toEqual({ state: "active", count: 1, ...waiting })
    // One reminder, not a second from the same wait.
    await Bun.sleep(60)
    expect(inbox).toEqual([REMINDER])
  } finally {
    await close()
  }
})

test("a turn that ended on an API error is settled, answered {} and reminded from the timer", async () => {
  const { endpoint, record, inbox, close } = await harness({
    record: recorded("active", 0),
    claim: implementer,
  })
  try {
    // Claude Code ignores what a StopFailure hook returns, so the machine is told this settle can
    // carry no continuation: it arms the reminder itself, under the guards every armed reminder
    // has, and the count moves when that reminder is written into the session.
    expect(
      await endpoint.handle({ event: "StopFailure", lastAssistantMessage: "I pushed the branch." }),
    ).toEqual({})
    const said = { reported: false, lastMessage: "I pushed the branch." }
    expect(await record()).toEqual({ state: "active", count: 0, ...said })
    await waitFor(async () => inbox.length > 0, "the reminder the failed turn earned")
    expect(inbox).toEqual([REMINDER])
    expect(await record()).toEqual({ state: "active", count: 1, ...said })
  } finally {
    await close()
  }
})

// INFRA-484: the update task reaches Claude Code as a prompt, an inbound message, and
// `ready_for_update` finishes nothing the daemon records. The `Stop` of the turn the worker
// answers in must still block nothing: a blocked `Stop` continues that same turn, so the work it
// did would go with the pod, and the daemon would see no turn end.
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

// R5: the daemon's own update task reaches a role whose phase has closed as a prompt like any
// other. With no model of the phase, nothing a prompt moves can decide that a reminder goes out,
// so the daemon's answer that the claim owes nothing keeps it quiet: no guard says so, the
// construction does.
test("an update task sent to a role whose phase is closed produces no reminder", async () => {
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
      // The listener's "nothing was kept for that role". The notice is the shared one, which
      // sends `keep: false` (`createPhaseReminder`, packages/pi-shared/src/phase-reminder.ts),
      // so the listener keeps nothing for an architect role no session holds: this 404 is the
      // ordinary answer to it. The architect was not told, and the next delivered reminder
      // publishes again under the same key.
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
    expect(inbox[0]).toContain("it holds what was pushed to legion/LEGION-208")
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

// R5's restart row on this host, the defect's own signature (dispatch://INFRA-2746): a server
// made afresh — with no record, or with the one the release before this one left, its phase
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
