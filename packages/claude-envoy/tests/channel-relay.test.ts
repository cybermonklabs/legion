/**
 * A Claude Code or Codex session whose inbound is the listener's relay socket
 * (`ENVOY_INBOUND=listener`): the channel server dials the socket rather than NATS
 * (`dialChannelInbound`), and neither live view takes its control subject on it, since the socket
 * carries no agent stream and refuses that subject (`subject_not_granted`). The socket is a
 * stand-in that serves the socket's wire and the broker's token exchange on one origin
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
import type { ModLink } from "../src/mod-link"

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
