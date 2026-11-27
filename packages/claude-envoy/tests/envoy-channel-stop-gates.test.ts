import { expect, spyOn, test } from "bun:test"
import * as fsPromises from "node:fs/promises"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { dirname, join, resolve } from "node:path"
import { agentStreamControlSubject } from "@legion/contracts"
import { daemonContract } from "@legion/contracts/daemon-contract"
import { createEnvoyClient, EnvoyApiError } from "@legion/envoy-client/transport"
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { modSocketPath } from "../hooks/mod-protocol"
import pkg from "../package.json" with { type: "json" }
import { LIVE_VIEW_READ_FAILED } from "../src/agent-stream-runner"
import {
  type ChannelNotifier,
  DELIVERY_FAILED_LINE,
  executeEnvoyTool,
  startChannelSession,
} from "../src/envoy-channel-server"
import { inboundOffChannelSession } from "../src/envoy-off"
import { startLegionClaim } from "../src/legion-claim"
import {
  readPersistedRole,
  roleStateFile,
  SessionIdentity,
  sessionHandoffFile,
  writePersistedRole,
  writeSessionHandoff,
} from "../src/session-identity"
import {
  deliveryRaw,
  directSubject,
  FakeNats,
  noInterest,
  recordingClient,
  sessionOptions,
  settled,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { type FakeMod, startFakeMod } from "./fake-mod"
import { isolatePaneEnvironment } from "./pane-environment"
import { PROCESS_WAIT_MS, ROW_TIMEOUT_MS, startReopenServer } from "./reopen-server"

// A session whose stop has begun registers, unsubscribes and claims nothing, and writes no role
// record (R24 of dispatch://INFRA-3589), whichever path is still running when the stop begins:
// the Legion claim's boot, `unfollow`, a role restore whose removal of the old record the stop
// overtakes, and the rest of a start the stop overtakes.

isolatePaneEnvironment()

const scratchState = useScratchState()

/** Holds a call on the wire until the row resolves what this answers. */
function held(): { readonly promise: Promise<void>; readonly resolve: () => void } {
  return Promise.withResolvers<void>()
}

/**
 * A Legion pane's claim boot, which registers, says ready, reads the claim's value and claims its
 * role right after that read (`bootLegionClaim`). Where `stopAtReady` is set, the server's stop
 * begins while the boot's `claims/ready` is on the wire: it aborts the boot's signal, `booting`,
 * which is the server's stop reading, and starts the shutdown (`stopping`). The session's client
 * records each call on `calls`, and holds the shutdown's DELETE on the wire until `deleting`
 * resolves. `claimToken` is the role the daemon registered.
 */
async function readyClaim(stopAtReady: boolean) {
  const session = "0b9c2f4e-2d3a-4c55-9a1e-6f7d8e9a0b1c"
  const stateDirectory = scratchState()
  const configDir = scratchState()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${session}.jsonl`), "{}\n")
  const fixture = async (name: string): Promise<Record<string, unknown>> =>
    JSON.parse(
      await readFile(resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8"),
    )
  const registered = await fixture("register.json")
  const current = await fixture("task.json")
  const booting = new AbortController()
  const deleting = held()
  const calls: string[] = []
  const client = recordingClient(calls, {
    unregisterSession: async ({ sessionID }) => {
      calls.push(`unregister ${sessionID}`)
      await deleting.promise
    },
  })
  const channel = await startChannelSession(
    sessionOptions(new SessionIdentity(session, "/tmp"), stateDirectory, {
      client,
      startSignal: booting.signal,
    }),
  )
  let stopping: Promise<void> | undefined
  const daemonFetch = (async (input: string | URL | Request) => {
    const route = new URL(input.toString()).pathname
    if (route.endsWith("/claims/register")) return Response.json(registered)
    if (route.endsWith("/claims/task")) return Response.json(current)
    if (route.endsWith("/claims/ready") && stopAtReady) {
      // The server's stop: the abort first, then the shutdown, whose DELETE is held.
      booting.abort()
      stopping = channel.shutdown()
    }
    return new Response(null, { status: 204 })
  }) as typeof fetch
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir, launcherSaves: false },
    sessionID: session,
    daemon: createLegionDaemonClient("http://daemon.test", {
      pluginContract: daemonContract(pkg, import.meta.dir),
      fetch: daemonFetch,
    }),
    clientInstanceID: "a1b2c3d4e5f60718293a4b5c6d7e8f90",
    client,
    session: channel,
    task: () => undefined,
    ready: () => undefined,
    finished: () => undefined,
    signal: booting.signal,
    log: () => undefined,
    stop: () => undefined,
    pollMs: 5,
  })
  return {
    session,
    stateDirectory,
    booting,
    deleting,
    calls,
    channel,
    claim,
    stopping: () => stopping,
    claimToken: String(registered["claimToken"]),
  }
}

// A Legion pane's claim boot registers and says ready, reads the claim's value, and claims its
// role right after that read. The server's stop begins while the boot's ready is on the wire: it
// aborts the boot's signal, and its shutdown's DELETE is still on the wire when the boot comes to
// the claim. The boot sends no role claim and writes no role record. The control row after it is
// the same claim with no stop begun.
test("the Legion claim's boot that the server's stop overtakes at claims/ready sends no claim and writes no role record", async () => {
  const run = await readyClaim(true)
  try {
    await run.claim.booted
    await waitFor(async () => run.calls.includes(`unregister ${run.session}`), "the stop's DELETE")
    await settled()

    expect(run.calls.filter((call) => call.startsWith("setRole"))).toEqual([])
    expect(await readPersistedRole(roleStateFile(run.stateDirectory, run.session))).toBeUndefined()
  } finally {
    run.booting.abort()
    run.deleting.resolve()
    await run.stopping()
    await run.channel.shutdown()
  }
})

test("control: the Legion claim's boot with no stop begun sends the claim and writes the role record", async () => {
  const run = await readyClaim(false)
  try {
    await run.claim.booted

    expect(run.calls.filter((call) => call.startsWith("setRole"))).toEqual([
      `setRole ${run.session} ${run.claimToken} soft`,
    ])
    expect(await readPersistedRole(roleStateFile(run.stateDirectory, run.session))).toMatchObject({
      session_id: run.session,
      role: run.claimToken,
    })
  } finally {
    run.booting.abort()
    run.deleting.resolve()
    await run.channel.shutdown()
  }
})

/** A session that follows `topic`, whose client records each call on `calls` and holds the
 *  shutdown's DELETE on the wire until `deleting` resolves. */
async function followingSession(session: string, topic: string) {
  const deleting = held()
  const calls: string[] = []
  const client = recordingClient(calls, {
    unregisterSession: async ({ sessionID }) => {
      calls.push(`unregister ${sessionID}`)
      await deleting.promise
    },
  })
  const channel = await startChannelSession(
    sessionOptions(new SessionIdentity(session, "/tmp"), scratchState(), { client }),
  )
  expect(await channel.follow([topic])).not.toEqual([])
  return { deleting, calls, channel }
}

// A session follows a topic, and its stop begins: the shutdown's DELETE is still on the wire when
// the topic is unfollowed. The session drops nothing and sends no removal, as `follow` adds
// nothing once the stop has begun. The control row after it unfollows with no stop begun.
test("unfollow once the stop has begun drops nothing and sends no removal", async () => {
  const session = "ses_unfollow_while_stopping"
  const topic = "notifications.dispatch.issue.DSP-7.>"
  const { deleting, calls, channel } = await followingSession(session, topic)
  let stopping: Promise<void> | undefined
  try {
    stopping = channel.shutdown()
    await waitFor(async () => calls.includes(`unregister ${session}`), "the stop's DELETE")
    const before = calls.length

    const removed = await channel.unfollow([topic])
    await settled()

    expect(removed).toEqual([])
    expect(calls.slice(before)).toEqual([])
  } finally {
    deleting.resolve()
    await stopping
    await channel.shutdown()
  }
})

test("control: unfollow with no stop begun drops the topic and sends its removal", async () => {
  const session = "ses_unfollow_no_stop"
  const topic = "notifications.dispatch.issue.DSP-7.>"
  const { deleting, calls, channel } = await followingSession(session, topic)
  try {
    const before = calls.length

    const removed = await channel.unfollow([topic])
    await settled()

    // A wildcard follow covers its base subject too, and the removal drops both.
    expect(removed).toContain(topic)
    expect(calls.slice(before)).toEqual([`unsubscribe ${session} ${removed.join(",")}`])
  } finally {
    deleting.resolve()
    await channel.shutdown()
  }
})

/**
 * A session `ses_old` whose role record names `reviewer`, and which a `/clear` moves to
 * `ses_new` (`clear`): the heartbeat takes the role back from the old id's record, the claim
 * lands, the old record is removed, and the new one is written. `removing` settles when the
 * removal of the old record starts. Where `stopAtRemoval` is set, the server's stop begins then:
 * the server's stop aborts the start signal, which is the session's stop reading.
 */
async function roleRestore(stopAtRemoval: boolean) {
  const stateDirectory = scratchState()
  const booting = new AbortController()
  const options = sessionOptions(new SessionIdentity("ses_old", "/tmp"), stateDirectory, {
    client: recordingClient([], {
      // The listener names this session the holder once a claim landed, so no heartbeat claims
      // again on its own.
      getRole: async (role) => ({ role, holder: "ses_new", last_seen: 1, state: "live" }),
      subscribe: async (input) => noInterest(input.sessionID),
    }),
    heartbeatMs: 25,
    handoffPid: 881,
    startSignal: booting.signal,
  })
  const oldRecord = roleStateFile(stateDirectory, "ses_old")
  await mkdir(dirname(oldRecord), { recursive: true })
  await writePersistedRole(oldRecord, {
    session_id: "ses_old",
    role: "reviewer",
    client_instance_id: options.clientInstanceID,
  })
  const handoff = sessionHandoffFile(stateDirectory, 881)
  await writeSessionHandoff(handoff, "ses_old")
  const realRm = fsPromises.rm
  const removing = held()
  const remove = spyOn(fsPromises, "rm").mockImplementation(async (path, rmOptions) => {
    if (String(path) === oldRecord) {
      if (stopAtRemoval) booting.abort()
      removing.resolve()
    }
    return realRm(path, rmOptions)
  })
  const channel = await startChannelSession(options)
  return {
    booting,
    channel,
    removing: removing.promise,
    newRecord: roleStateFile(stateDirectory, "ses_new"),
    clear: () => writeSessionHandoff(handoff, "ses_new"),
    restore: () => remove.mockRestore(),
  }
}

// A `/clear` moves the session to a new id, and the heartbeat takes the role back from the old
// id's record: the claim lands, the old record is removed, and the new one is written. The stop
// begins while that removal runs. The new record is not written. The control row after it is the
// same restore with no stop begun.
test("a role restore whose removal of the old record the stop overtakes writes no new record", async () => {
  const run = await roleRestore(true)
  try {
    await run.clear()
    await run.removing
    await run.channel.shutdown()
    await settled()

    expect(await readPersistedRole(run.newRecord)).toBeUndefined()
  } finally {
    run.restore()
    run.booting.abort()
    await run.channel.shutdown()
  }
})

test("control: a role restore with no stop begun writes the new id's record", async () => {
  const run = await roleRestore(false)
  try {
    await run.clear()
    await run.removing
    await waitFor(
      async () => (await readPersistedRole(run.newRecord)) !== undefined,
      "the new id's role record",
    )

    expect(await readPersistedRole(run.newRecord)).toMatchObject({
      session_id: "ses_new",
      role: "reviewer",
    })
  } finally {
    run.restore()
    run.booting.abort()
    await run.channel.shutdown()
  }
})

/**
 * The start of session `session`, whose role record names `reviewer` under another process: a
 * process holding no role takes such a record back at its start. The client records each call on
 * `calls`. Where `stopAtTitle` is set, the server's stop lands as the start's first registration is
 * built, when the title is read. `written` is the record as the row wrote it, and `ours` is that
 * record once this process holds it.
 */
async function startWithForeignRecord(session: string, stopAtTitle: boolean) {
  const stateDirectory = scratchState()
  const record = roleStateFile(stateDirectory, session)
  await mkdir(dirname(record), { recursive: true })
  const written = { session_id: session, role: "reviewer", client_instance_id: "another-process" }
  await writePersistedRole(record, written)
  const booting = new AbortController()
  const calls: string[] = []
  const options = sessionOptions(new SessionIdentity(session, "/tmp"), stateDirectory, {
    client: recordingClient(calls),
    startSignal: booting.signal,
    title: () => {
      if (stopAtTitle) booting.abort()
      return ""
    },
  })
  const channel = await startChannelSession(options)
  return {
    record,
    written,
    ours: { ...written, client_instance_id: options.clientInstanceID },
    calls,
    channel,
  }
}

// The server's stop begins while the session still starts, after the start's connection
// confirmed its subscriptions: the start's first registration is being built. Nothing of the
// start goes on after it: no registration is sent, no role is claimed back from the session's
// record, and the record is left as it was. The control row after it is the same start with no
// stop begun.
test("a stop that begins after the start's flush registers nothing, claims nothing and writes no role record", async () => {
  const { record, written, calls, channel } = await startWithForeignRecord(
    "ses_stopped_while_starting",
    true,
  )
  try {
    expect(
      calls.filter((call) => call.startsWith("subscribe") || call.startsWith("setRole")),
    ).toEqual([])
    expect(await readPersistedRole(record)).toEqual(written)
  } finally {
    await channel.shutdown()
  }
})

test("control: a start with no stop begun registers, claims the record's role back and writes the record", async () => {
  const session = "ses_started_no_stop"
  const { record, ours, calls, channel } = await startWithForeignRecord(session, false)
  try {
    await waitFor(
      async () => (await readPersistedRole(record))?.client_instance_id === ours.client_instance_id,
      "the record this process holds",
    )

    expect(calls.some((call) => call.startsWith(`subscribe ${session} `))).toBe(true)
    expect(calls.some((call) => call.startsWith(`setRole ${session} reviewer`))).toBe(true)
    expect(await readPersistedRole(record)).toEqual(ours)
  } finally {
    await channel.shutdown()
  }
})

/** A session whose one queued delivery's write fails once the row lets it (`failing`), after the
 *  notifier was handed it (`writing`); `booting` is the server's stop reading. */
async function failingDelivery() {
  const booting = new AbortController()
  const writing = held()
  const failing = held()
  const notifier: ChannelNotifier = {
    notification: async () => {
      writing.resolve()
      await failing.promise
      throw new Error("the bridge did not take the message")
    },
  }
  const channel = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), scratchState(), {
      notifier,
      startSignal: booting.signal,
    }),
  )
  return { booting, writing, failing, channel }
}

/** Every byte written to stderr from now until `restore`, which nothing else then sees. */
function stderrWrites(): { readonly written: string[]; readonly restore: () => void } {
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  return { written, restore: () => stderr.mockRestore() }
}

// A frame queued before the stop whose write fails after the stop began: the queue's failure line
// is a stderr line of the session's own, so its tell gate drops it.
test("a delivery queued before the stop that fails after the stop began says nothing on stderr", async () => {
  const { booting, writing, failing, channel } = await failingDelivery()
  const stderr = stderrWrites()
  try {
    const delivered = channel.delivery
      .enqueue({ subject: directSubject, raw: deliveryRaw })
      .catch(() => false)
    await writing.promise
    // The server's stop: the abort is the session's stop reading.
    booting.abort()
    failing.resolve()
    expect(await delivered).toBe(false)
    await settled()

    expect(stderr.written.filter((line) => line.startsWith(DELIVERY_FAILED_LINE))).toEqual([])
  } finally {
    stderr.restore()
    await channel.shutdown()
  }
})

// The control of the row above: the same failed write with no stop begun says so on stderr.
test("control: a delivery that fails with no stop begun says so on stderr", async () => {
  const { writing, failing, channel } = await failingDelivery()
  const stderr = stderrWrites()
  try {
    const delivered = channel.delivery
      .enqueue({ subject: directSubject, raw: deliveryRaw })
      .catch(() => false)
    await writing.promise
    failing.resolve()
    expect(await delivered).toBe(false)
    await settled()

    expect(stderr.written.filter((line) => line.startsWith(DELIVERY_FAILED_LINE))).toEqual([
      `${DELIVERY_FAILED_LINE}the bridge did not take the message\n`,
    ])
  } finally {
    stderr.restore()
    await channel.shutdown()
  }
})

/** A session whose notifier holds the first frame it is handed until the row lets it go
 *  (`release`), and takes every other write at once, recording each on `written`. The listener
 *  accepts each registration until the row queues its notice (`noticeBehindHeldFrame`), and
 *  refuses each from then on, as a listener that can give the session no consumer does.
 *  `booting` is the server's stop reading. */
async function heldFrameSession() {
  const booting = new AbortController()
  const writing = held()
  const releasing = held()
  const written: { readonly content: string; readonly eventId?: string }[] = []
  const notifier: ChannelNotifier = {
    notification: async (notification) => {
      written.push(notification)
      if (notification.eventId === undefined || written.length > 1) return
      writing.resolve()
      await releasing.promise
    },
  }
  let refusing = false
  const client = recordingClient([], {
    subscribe: async (input) => {
      if (!refusing) return noInterest(input.sessionID)
      throw new EnvoyApiError({
        method: "POST",
        url: "http://127.0.0.1:4000/v1/interests/subscribe",
        status: 409,
        responseBody: JSON.stringify({
          error: "the listener could not give this session a consumer",
          reason: "not_requested",
        }),
      })
    },
  })
  const channel = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), scratchState(), {
      notifier,
      client,
      startSignal: booting.signal,
    }),
  )
  return {
    booting,
    release: releasing.resolve,
    written,
    channel,
    /** Queues a frame the notifier holds, then, behind it, the outage notice of a follow whose
     *  registration the listener refuses: the refusal tells the session as it is answered, so
     *  the notice is queued once the follow settles. Answers the frame's delivery. */
    noticeBehindHeldFrame: async (): Promise<{ readonly delivered: Promise<boolean> }> => {
      const delivered = channel.delivery.enqueue({ subject: directSubject, raw: deliveryRaw })
      await writing.promise
      refusing = true
      await channel.follow(["notifications.team.r24-queued-notice"]).catch(() => [])
      return { delivered }
    },
  }
}

// A notice queued before the stop waits its turn behind a frame the notifier still holds as the
// stop begins. The queue reads the stop when the notice's turn comes, and shows it to nobody.
test("a notice queued before the stop behind a frame still being written is shown to nobody once the stop has begun", async () => {
  const session = await heldFrameSession()
  const stderr = stderrWrites()
  try {
    const { delivered } = await session.noticeBehindHeldFrame()
    // The server's stop: the abort is the session's stop reading.
    session.booting.abort()
    session.release()
    expect(await delivered).toBe(true)
    await settled()

    // A notice this server writes for itself is the one write that names no Envoy event.
    expect(session.written.map((write) => write.eventId ?? "notice")).toEqual(["evt-42"])
  } finally {
    stderr.restore()
    session.release()
    await session.channel.shutdown()
  }
})

// The control of the row above: the same notice behind the same frame, with no stop begun, is
// shown once the frame is written.
test("control: a notice queued behind a frame still being written with no stop begun is shown after it", async () => {
  const session = await heldFrameSession()
  const stderr = stderrWrites()
  try {
    const { delivered } = await session.noticeBehindHeldFrame()
    session.release()
    expect(await delivered).toBe(true)
    await settled()

    expect(session.written.map((write) => write.eventId ?? "notice")).toEqual(["evt-42", "notice"])
  } finally {
    stderr.restore()
    session.release()
    await session.channel.shutdown()
  }
})

/** A connection whose drain and close fail, and stay open, as a stalled link's can. */
class UnclosableNats extends FakeNats {
  override drain(): Promise<void> {
    return Promise.reject(new Error("the drain failed"))
  }

  override close(): Promise<void> {
    return Promise.reject(new Error("the close failed"))
  }
}

// The stop's own lines are written while the stop runs (R24 of dispatch://INFRA-3589): the
// shutdown writes its failed DELETE, and the drain and the close of its connection that failed
// (`ChannelTopics.close`), to stderr with no tell gate, which drops every other line of the
// session's own from the moment the stop begins.
test("a shutdown whose DELETE, drain and close fail writes those three lines on stderr, and no other line", async () => {
  const connection = new UnclosableNats()
  const channel = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), scratchState(), {
      connection,
      client: recordingClient([], {
        unregisterSession: async () => {
          throw new Error("the listener did not answer")
        },
      }),
    }),
  )
  const stderr = stderrWrites()
  try {
    await channel.shutdown()

    // `natsFailureReason` names an error nats.js did not raise by its class alone.
    expect(stderr.written).toEqual([
      "envoy-channel: draining the broker connection failed — Error (message withheld)\n",
      "envoy-channel: closing the broker connection failed — Error (message withheld)\n",
      "envoy-channel: session deregistration failed — the listener did not answer\n",
    ])
  } finally {
    stderr.restore()
    await connection.closeForGood()
  }
})

/** An inbound-off session (`./envoy-off`) and a client whose listener grants a claim of
 *  `reviewer`. Where `stopsOnClaim` is set, the server's stop begins while the claim is on the
 *  wire. */
function inboundOffClaim(stopsOnClaim: boolean) {
  const stateDirectory = scratchState()
  const identity = new SessionIdentity("ses_inbound_off", "/tmp")
  let stopping = false
  const session = inboundOffChannelSession({
    identity,
    stateDirectory,
    why: "no NATS server is configured",
    stopping: () => stopping,
  })
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async () => {
      if (stopsOnClaim) stopping = true
      return Response.json({
        session_id: identity.id,
        machine_id: "devbox",
        dir: "/work",
        topics: ["notifications.role.reviewer"],
      })
    },
  })
  return {
    session,
    record: roleStateFile(stateDirectory, identity.id),
    claim: () =>
      executeEnvoyTool(
        { identity, client, session, clientInstanceID: "a1b2c3d4e5f60718293a4b5c6d7e8f90" },
        "envoy_role_set",
        { role: "reviewer" },
      ),
  }
}

// An inbound-off session (`./envoy-off`) reads the server's stop too: a role claim that was on the
// wire when the stop began is refused when its answer comes, and leaves no record. The control row
// after it is the same claim with no stop begun.
test("inbound-off: an envoy_role_set claim the listener answers after the stop began keeps no record", async () => {
  const { session, record, claim } = inboundOffClaim(true)

  await expect(claim()).rejects.toThrow(
    "this session is stopping, so it claims no role: role reviewer is not held here",
  )
  await session.rememberRole("reviewer")
  expect(await readPersistedRole(record)).toBeUndefined()
})

test("control: inbound-off: an envoy_role_set claim the listener answers with no stop begun keeps its record", async () => {
  const { session, record, claim } = inboundOffClaim(false)

  await claim()
  await session.rememberRole("reviewer")
  expect(await readPersistedRole(record)).toMatchObject({ role: "reviewer" })
})

// The live view's reads of the mod are on the wire when the server's stop begins; the stop closes
// the mod's link, which rejects them. The view's warnings go through the server's tell gate, which
// drops them. Whole processes: the mod is `./fake-mod`, which never answers those reads.
test(
  "the live view's reads the stop's close of the mod link ends say nothing on stderr",
  async () => {
    const sessionId = "ses_view_at_stop"
    const run = await startReopenServer({
      sessionId,
      env: (stateDirectory) => ({
        CLAUDE_CODE_SESSION_ID: sessionId,
        CLAUDE_PLUGIN_DATA: stateDirectory,
      }),
    })
    let mod: FakeMod | undefined
    try {
      const socketPath = modSocketPath(sessionId, { XDG_RUNTIME_DIR: run.stateDirectory })
      if (socketPath === undefined) throw new Error("the session names no mod socket")
      const never = new Promise<never>(() => undefined)
      mod = await startFakeMod(socketPath, {
        sessionId,
        answers: { "session.model": () => never, "command.list": () => never },
      })
      await waitFor(
        async () => run.broker.subscribed.includes(agentStreamControlSubject(sessionId)),
        "the live view started",
        run.state,
        PROCESS_WAIT_MS,
      )
      // An event of the conversation: the view reads the session's model and commands.
      await mod.send({ kind: "turn-start", turnId: "turn-at-stop" })
      const asked = mod
      await waitFor(
        async () => asked.madeOf("session.model").length > 0,
        "the live view's read of the model on the wire",
        run.state,
        PROCESS_WAIT_MS,
      )

      run.server.kill("SIGTERM")
      await run.server.exited
      await run.stderrEnded

      expect(run.stderr()).not.toContain(LIVE_VIEW_READ_FAILED)
    } finally {
      await mod?.stop()
      await run.stop()
    }
  },
  ROW_TIMEOUT_MS,
)

// The control of the row above: no stop begins, and the mod answers the view's read of the model
// with an error. The view's warning is on stderr. The same whole processes.
test(
  "control: a live view read that fails with no stop begun says so on stderr",
  async () => {
    const sessionId = "ses_view_no_stop"
    const run = await startReopenServer({
      sessionId,
      env: (stateDirectory) => ({
        CLAUDE_CODE_SESSION_ID: sessionId,
        CLAUDE_PLUGIN_DATA: stateDirectory,
      }),
    })
    let mod: FakeMod | undefined
    try {
      const socketPath = modSocketPath(sessionId, { XDG_RUNTIME_DIR: run.stateDirectory })
      if (socketPath === undefined) throw new Error("the session names no mod socket")
      mod = await startFakeMod(socketPath, {
        sessionId,
        answers: {
          "session.model": () => Promise.reject(new Error("the session's model is unreadable")),
        },
      })
      await waitFor(
        async () => run.broker.subscribed.includes(agentStreamControlSubject(sessionId)),
        "the live view started",
        run.state,
        PROCESS_WAIT_MS,
      )
      // An event of the conversation: the view reads the session's model, and the read fails.
      await mod.send({ kind: "turn-start", turnId: "turn-no-stop" })
      await waitFor(
        async () => run.stderr().includes(LIVE_VIEW_READ_FAILED),
        "the live view's warning on stderr",
        run.state,
        PROCESS_WAIT_MS,
      )

      expect(run.stderr()).toContain(`envoy-channel: ${LIVE_VIEW_READ_FAILED}model — `)
    } finally {
      await mod?.stop()
      await run.stop()
    }
  },
  ROW_TIMEOUT_MS,
)
