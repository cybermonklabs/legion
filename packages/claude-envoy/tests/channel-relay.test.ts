/**
 * A Claude Code or Codex session whose inbound is the listener's relay socket
 * (`ENVOY_INBOUND=listener`): the channel server dials the socket rather than NATS
 * (`dialChannelInbound`), and neither live view takes its control subject on it, since the socket
 * carries no agent stream and refuses that subject (`subject_not_granted`); a session whose own
 * relay subject the listener refuses is told why and takes it again at the heartbeat; and a
 * claim's task frame on the socket, and each reopen of it, reach the session's task values as they
 * do over NATS. The socket is a stand-in that serves the socket's wire and the broker's token
 * exchange on one origin (`@legion/envoy-client/test/relay-socket-stand-in`).
 */
import { afterEach, expect, setDefaultTimeout, spyOn, test } from "bun:test"
import { writeFileSync } from "node:fs"
import * as path from "node:path"
import { readEnvoyConfig } from "@legion/envoy-client/envoy-config"
import { type EnvoyEnvironment, envoySettings } from "@legion/envoy-client/envoy-settings"
import { type RelayTaskValue, TaskHeader } from "@legion/envoy-client/relay-frame"
import {
  type RelaySocketStandIn,
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "@legion/envoy-client/test/relay-socket-stand-in"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import type { ModEvent } from "../hooks/mod-protocol"
import { startAgentStream } from "../src/agent-stream-runner"
import { channelReopener, dialChannelInbound } from "../src/channel-connection"
import { startCodexAgentStream } from "../src/codex/codex-agent-stream"
import { startChannelSession } from "../src/envoy-channel-server"
import type { ModLink } from "../src/mod-link"
import { SessionIdentity } from "../src/session-identity"
import { gatedStderr } from "../src/stop-gates"
import {
  FakeNotifier,
  noInterest,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

// The rows wait on files, processes or local servers, which a loaded machine slows. Their bound
// is a hang guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS)

isolatePaneEnvironment()

const scratchState = useScratchState()

const SESSION = "0c3b6f6e-0000-4000-8000-0000000000aa"
const tempDir = temporaryDirectories("channel-relay")
const started: (() => unknown)[] = []
afterEach(async () => {
  for (const stop of started.splice(0).reverse()) await stop()
})

/** A mod link that never connects: the live view's control subject is what this file reads, and
 *  a mod that says nothing leaves the runner nothing else to do. */
function quietLink(): ModLink {
  const ended = Promise.withResolvers<void>()
  return {
    close: async () => ended.resolve(),
    connected: () => false,
    // No event, ever: the stream ends when the link closes.
    events: (async function* (): AsyncGenerator<ModEvent> {
      await ended.promise
      yield* []
    })(),
    onSessionId: () => undefined,
    onConnected: () => undefined,
    request: () => Promise.reject(new Error("no mod")),
    sessionId: () => SESSION,
    socketPath: "/scratch/claude-envoy-relay-test.sock",
  }
}

/** A session's settings whose listener and broker are both `server`, told `ENVOY_INBOUND`. */
async function relaySettings(server: RelaySocketStandIn) {
  const dir = tempDir()
  const tokenFile = path.join(dir, "token")
  writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`)
  const env: EnvoyEnvironment = {
    HOME: dir,
    ENVOY_URL: server.url,
    ENVOY_INBOUND: "listener",
    AGENT_SECRETS_URL: server.url,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
  }
  const settings = await envoySettings(
    env,
    await readEnvoyConfig(path.join(dir, "envoy.json"), env, { cwd: dir }),
    { session: () => SESSION },
  )
  if (!settings.enabled) throw new Error(`Envoy is off: ${settings.error ?? settings.notice}`)
  return settings
}

test("a relay socket session dials the socket, and neither live view takes a subject on it", async () => {
  const server = startRelaySocketStandIn()
  started.push(() => server.stop())
  const settings = await relaySettings(server)
  const { inbound } = settings
  if ("off" in inbound) throw new Error(`inbound is off: ${inbound.off}`)
  const warnings: string[] = []
  const warn = (text: string): void => void warnings.push(text)
  const dialled = await dialChannelInbound(
    inbound,
    settings.url,
    () => SESSION,
    "claude-envoy-channel-test",
    warn,
    undefined,
    AbortSignal.timeout(10_000),
  )
  started.push(() => dialled.connection.close())
  expect(dialled.liveView).toBe(false)
  expect(server.upgrades.map((upgrade) => [upgrade.session, upgrade.status])).toEqual([
    [SESSION, 101],
  ])

  const claude = startAgentStream({
    connection: dialled.connection,
    followsSessionId: false,
    link: quietLink(),
    liveView: dialled.liveView,
    sessionId: SESSION,
    warn,
  })
  started.push(() => claude.stop())
  const codex = startCodexAgentStream({
    connection: dialled.connection,
    liveView: dialled.liveView,
    sessionId: SESSION,
    warn,
  })
  started.push(() => codex.stop())
  // The relay subject, which the session does take, after both: one round trip shows the
  // listener read every frame the session sent before it.
  dialled.connection.subscribe(standInDeliverSubject(SESSION))
  await dialled.connection.flush()

  if (server.refusedSubs.length > 0) {
    throw new Error(
      `the stand-in refused these subscribes: ${server.refusedSubs.map((sub) => sub.subject).join(", ")}`,
    )
  }
  expect(server.subs).toEqual([{ session: SESSION, subject: standInDeliverSubject(SESSION) }])
})

// Before the session starts the socket's refused trades have no session to tell: they are said on
// stderr once, worded, however many fail before one succeeds (`inboundUnavailable`), rather than a
// line a try for as long as the broker refuses.
test("trades refused before the session starts are said on stderr once, and the socket opens once one succeeds", async () => {
  const server = startRelaySocketStandIn()
  started.push(() => server.stop())
  const settings = await relaySettings(server)
  const { inbound } = settings
  if ("off" in inbound) throw new Error(`inbound is off: ${inbound.off}`)
  if (!("relaySetting" in inbound)) throw new Error("inbound is not the relay socket")
  let refusals = 2
  const credential = inbound.relayCredential
  const refusing = {
    ...inbound,
    relayCredential: {
      ...credential,
      headers: (...args: Parameters<typeof credential.headers>) => {
        if (refusals > 0) {
          refusals -= 1
          return Promise.reject(new Error("the secrets broker refused the trade (fixture)"))
        }
        return credential.headers(...args)
      },
    },
  }
  const warnings: string[] = []
  const dialled = await dialChannelInbound(
    refusing,
    settings.url,
    () => SESSION,
    "claude-envoy-channel-test",
    (text) => void warnings.push(text),
    undefined,
    AbortSignal.timeout(15_000),
  )
  started.push(() => dialled.connection.close())
  expect(refusals).toBe(0)
  expect(warnings.filter((line) => line.includes("has no token"))).toEqual([
    "this session's relay socket has no token: the secrets broker refused the trade (fixture); the session starts once a trade succeeds",
  ])
}, 30_000)

// The relay socket of a session whose stop has begun (R24 of dispatch://INFRA-3589) opens no
// socket again: a close the listener sends after the stop began closes the connection for good,
// rather than opening an upgrade under the stopping session's token.
test("a relay socket dialled with the session's stop reading opens no socket again once the stop has begun", async () => {
  const server = startRelaySocketStandIn()
  started.push(() => server.stop())
  const settings = await relaySettings(server)
  const { inbound } = settings
  if ("off" in inbound) throw new Error(`inbound is off: ${inbound.off}`)
  let stopping = false
  const dialled = await dialChannelInbound(
    inbound,
    settings.url,
    () => SESSION,
    "claude-envoy-channel-test",
    () => undefined,
    undefined,
    AbortSignal.timeout(10_000),
    () => stopping,
  )
  started.push(() => dialled.connection.close())

  stopping = true
  server.close(1012, "listener_stopping")
  await waitFor(async () => dialled.connection.isClosed(), "the relay socket closed for good")

  expect(server.upgrades.map((upgrade) => [upgrade.session, upgrade.status])).toEqual([
    [SESSION, 101],
  ])
})

// A relay socket a reopen opened (`channelReopener`) holds the server's stop reading and writes
// its lines through the server's tell gate, for its whole life: once the stop has begun, a close
// from the listener opens no socket again and its line is dropped. The row's first close comes
// with no stop begun, so its line is written and the socket opens again: the control of the
// absence after the stop.
test("a reopened relay socket opens no socket again and writes nothing on stderr once the server's stop has begun", async () => {
  const server = startRelaySocketStandIn()
  started.push(() => server.stop())
  const settings = await relaySettings(server)
  const { inbound } = settings
  if ("off" in inbound) throw new Error(`inbound is off: ${inbound.off}`)
  let stopping = false
  const serverStderr = gatedStderr(() => stopping)
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  try {
    const reopen = channelReopener({
      inbound,
      listenerUrl: settings.url,
      session: () => SESSION,
      name: "claude-envoy-channel-test",
      signal: new AbortController().signal,
      stopping: () => stopping,
      warn: (text) => serverStderr(`envoy-channel: ${text}\n`),
      swapKeeper: () => undefined,
      admitting: (follow) => follow,
      useConnection: () => undefined,
    })
    const reopened = await reopen(() => undefined)
    started.push(() => reopened.connection.close())

    server.close(1012, "listener_stopping")
    await waitFor(
      async () => server.upgrades.length === 2 && server.sockets(SESSION) === 1,
      "the relay socket opened again",
    )
    const beforeStop = written.filter((line) => line.startsWith("envoy-channel:"))
    expect(beforeStop).toContainEqual(
      expect.stringContaining(`the listener's relay socket for ${SESSION} closed (`),
    )

    stopping = true
    server.close(1012, "listener_stopping")
    await waitFor(async () => reopened.connection.isClosed(), "the relay socket closed for good")

    expect(server.upgrades).toHaveLength(2)
    expect(written.filter((line) => line.startsWith("envoy-channel:"))).toEqual(beforeStop)
  } finally {
    stderr.mockRestore()
  }
})

/** The notices the server wrote for itself under `heading`: the one write that names no event. */
function notices(notifier: FakeNotifier, heading: string): string[] {
  return notifier.notifications
    .filter((written) => written.eventId === undefined && written.content.includes(heading))
    .map((written) => written.content)
}

// The wire lets the listener answer any `sub` `subject_not_granted`. #4658 answers so only a
// subject other than the session's own, and closes the socket 1013 when NATS refuses a subscribe;
// a listener that refused the session's own subject would leave a dead subscription unless it
// ends with that error, as a nats.js one the server refused does. So the session is told why it
// receives nothing and takes the subject again at the heartbeat.
test("a relay subject the listener refuses is told as an outage naming why, and the heartbeat takes it again", async () => {
  const server = startRelaySocketStandIn()
  started.push(() => server.stop())
  const settings = await relaySettings(server)
  const { inbound } = settings
  if ("off" in inbound) throw new Error(`inbound is off: ${inbound.off}`)
  const dialled = await dialChannelInbound(
    inbound,
    settings.url,
    () => SESSION,
    "claude-envoy-channel-test",
    () => undefined,
    undefined,
    AbortSignal.timeout(10_000),
  )
  started.push(() => dialled.connection.close())
  const notifier = new FakeNotifier()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), scratchState(), {
      connection: dialled.connection,
      ...(dialled.reportOutagesTo === undefined
        ? {}
        : { reportOutagesTo: dialled.reportOutagesTo }),
      notifier,
      heartbeatMs: 50,
      client: recordingClient([], { subscribe: async (input) => noInterest(input.sessionID) }),
    }),
  )
  try {
    const relaySubject = standInDeliverSubject(SESSION)
    server.refuseOwnSubject = true
    // The socket opens again and takes the subject again, which the listener now refuses.
    server.close(1012, "listener_stopping")
    await waitFor(
      async () => notices(notifier, "Envoy inbound unavailable").length === 1,
      "the refusal told",
      () => JSON.stringify(notifier.notifications),
    )
    expect(notices(notifier, "Envoy inbound unavailable")[0]).toContain(
      `the listener's relay socket refused the subject ${relaySubject} (subject_not_granted)`,
    )
    // The heartbeat asks for the subject again while it is refused.
    const refusedOnce = server.refusedSubs.length
    await waitFor(
      async () => server.refusedSubs.length > refusedOnce,
      "the subject asked for again at a heartbeat",
    )
    server.refuseOwnSubject = false
    await waitFor(
      async () => notices(notifier, "Envoy inbound restored").length === 1,
      "the session told it receives again once the subject is granted",
      () => JSON.stringify(notifier.notifications),
    )
    expect(server.subs.at(-1)).toEqual({ session: SESSION, subject: relaySubject })
    expect(notices(notifier, "Envoy inbound unavailable")).toHaveLength(1)
  } finally {
    await session.shutdown()
  }
}, 30_000)

// A claim's task value rides the session's relay subject as a task frame, which the socket carries
// as it carries every message on that subject (a `msg` whose headers are the frame's): the frame
// reaches the session's task values, which a Legion pane's claim hands the bridge, and is answered
// nothing, since it is no message of the session's consumer. A reopen of the socket takes the
// subject again on the new socket and begins a new epoch, whose first registration answer tells
// the task values to read the value again, as a NATS reconnect does: a task frame relayed while
// the socket was closed is lost.
test("a task frame on the relay socket reaches the session's task values unanswered, and a reopen of the socket tells them a new epoch", async () => {
  const server = startRelaySocketStandIn()
  started.push(() => server.stop())
  const settings = await relaySettings(server)
  const { inbound } = settings
  if ("off" in inbound) throw new Error(`inbound is off: ${inbound.off}`)
  const dialled = await dialChannelInbound(
    inbound,
    settings.url,
    () => SESSION,
    "claude-envoy-channel-test",
    () => undefined,
    undefined,
    AbortSignal.timeout(10_000),
  )
  started.push(() => dialled.connection.close())
  const relayed: RelayTaskValue[] = []
  let epochs = 0
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity(SESSION, "/tmp"), scratchState(), {
      connection: dialled.connection,
      ...(dialled.reportOutagesTo === undefined
        ? {}
        : { reportOutagesTo: dialled.reportOutagesTo }),
      client: recordingClient([], { subscribe: async (input) => noInterest(input.sessionID) }),
      taskValues: {
        relayed: (value) => relayed.push(value),
        epoch: () => {
          epochs += 1
        },
      },
    }),
  )
  try {
    const relaySubject = standInDeliverSubject(SESSION)
    await waitFor(async () => epochs === 1, "the first epoch's answer")
    expect(server.subs).toEqual([{ session: SESSION, subject: relaySubject }])

    const claim = "legion-legion-legion-208-implementer"
    const bucketEpoch = "2026-11-13T16:21:44.123456789Z"
    const reply = server.deliver(SESSION, {
      headers: {
        [TaskHeader.claim]: [claim],
        [TaskHeader.epoch]: [bucketEpoch],
        [TaskHeader.revision]: ["12"],
        [TaskHeader.op]: ["put"],
      },
      data: JSON.stringify({ task: 4 }),
    })
    if (reply === undefined) {
      throw new Error("the stand-in holds no relay subscription of the session")
    }
    await waitFor(async () => relayed.length === 1, "the task frame")
    expect(relayed).toEqual([{ claim, epoch: bucketEpoch, revision: 12, op: "put", task: 4 }])
    // A round trip after the frame: the listener has read every frame the session sent before it,
    // and none of them answered the task frame.
    await dialled.connection.flush()
    expect(server.receipts.filter((receipt) => receipt.reply === reply)).toEqual([])
    expect(server.refusedPublishes).toEqual([])
    expect(epochs).toBe(1)

    // The listener is stopping: the socket opens again at once and takes the subject again. The
    // new epoch can be told from the registration's answer before the stand-in has read the
    // socket's second sub, so the test waits for both.
    server.close(1012, "listener_stopping")
    await waitFor(
      async () => epochs === 2 && server.subs.length === 2,
      "the reopen's epoch and subscription",
    )
    expect(server.upgrades.filter((upgrade) => upgrade.status === 101)).toHaveLength(2)
    expect(server.subs).toEqual([
      { session: SESSION, subject: relaySubject },
      { session: SESSION, subject: relaySubject },
    ])
    expect(relayed).toHaveLength(1)
  } finally {
    await session.shutdown()
  }
}, 30_000)
