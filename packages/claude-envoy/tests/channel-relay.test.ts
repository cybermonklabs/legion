/**
 * A Claude Code or Codex session whose inbound is the listener's relay socket
 * (`ENVOY_INBOUND=listener`): the channel server dials the socket rather than NATS
 * (`dialChannelInbound`), and neither live view takes its control subject on it, since the socket
 * carries no agent stream and refuses that subject (`subject_not_granted`); and a session whose own
 * relay subject the listener refuses is told why and takes it again at the heartbeat. The socket
 * is a stand-in that serves the socket's wire and the broker's token exchange on one origin
 * (`@legion/envoy-client/test/relay-socket-stand-in`).
 */
import { afterEach, expect, test } from "bun:test"
import { writeFileSync } from "node:fs"
import * as path from "node:path"
import { readEnvoyConfig } from "@legion/envoy-client/envoy-config"
import { type EnvoyEnvironment, envoySettings } from "@legion/envoy-client/envoy-settings"
import {
  type RelaySocketStandIn,
  standInDeliverSubject,
  startRelaySocketStandIn,
} from "@legion/envoy-client/test/relay-socket-stand-in"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import type { ModEvent } from "../hooks/mod-protocol"
import { startAgentStream } from "../src/agent-stream-runner"
import { dialChannelInbound } from "../src/channel-connection"
import { startCodexAgentStream } from "../src/codex/codex-agent-stream"
import { startChannelSession } from "../src/envoy-channel-server"
import type { ModLink } from "../src/mod-link"
import { SessionIdentity } from "../src/session-identity"
import {
  FakeNotifier,
  noInterest,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

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
function relaySettings(server: RelaySocketStandIn) {
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
  const settings = envoySettings(
    env,
    readEnvoyConfig(path.join(dir, "envoy.json"), env, { cwd: dir }),
    { session: () => SESSION },
  )
  if (!settings.enabled) throw new Error(`Envoy is off: ${settings.error ?? settings.notice}`)
  return settings
}

test("a relay socket session dials the socket, and neither live view takes a subject on it", async () => {
  const server = startRelaySocketStandIn()
  started.push(() => server.stop())
  const settings = relaySettings(server)
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
  const settings = relaySettings(server)
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
  const settings = relaySettings(server)
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
