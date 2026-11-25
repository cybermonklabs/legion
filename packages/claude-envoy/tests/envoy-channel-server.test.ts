import { beforeEach, expect, setDefaultTimeout, spyOn, test } from "bun:test"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { dirname, join, resolve } from "node:path"
import { BROKER_VARIABLES } from "@legion/cipher-client/environment"
import { METADATA_HOST_VAR } from "@legion/cipher-client/metadata-identity"
import { IDEMPOTENCY_KEY_COMMITTED } from "@legion/contracts"
import { forgetBrokerClients } from "@legion/envoy-client/broker-credential"
import { ENVOY_INBOUND_VAR } from "@legion/envoy-client/envoy-settings"
import { NATS_AUTH_VAR } from "@legion/envoy-client/nats-credential"
import { sessionConfigFile } from "@legion/envoy-client/session-settings"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import type { Server, Subprocess } from "bun"
import { nkeys } from "nats"
import codexMarketplace from "../../../.agents/plugins/marketplace.json" with { type: "json" }
import marketplace from "../../../.claude-plugin/marketplace.json" with { type: "json" }
import plugin from "../.claude-plugin/plugin.json" with { type: "json" }
import ompPlugin from "../.omp-plugin/plugin.json" with { type: "json" }
import codexPlugin from "../codex/.codex-plugin/plugin.json" with { type: "json" }
import codexHooks from "../codex/hooks/hooks.json" with { type: "json" }
import codexPkg from "../codex/package.json" with { type: "json" }
import type { ModCallArguments, ModCallName, ModCallResults } from "../hooks/mod-protocol"
import pkg from "../package.json" with { type: "json" }
import {
  type ChannelNotifier,
  type ChannelSession,
  createChannelDelivery,
  MCP_SERVER_INFO,
  sessionCapabilities,
  startChannelSession,
  userTurnDispatchClient,
} from "../src/envoy-channel-server"
import { modSession } from "../src/mod-session"
import { roleStateFile, SessionIdentity } from "../src/session-identity"
import {
  deliveryRaw,
  directSubject,
  type FakeClient,
  FakeNats,
  FakeNotifier,
  noInterest,
  recordingClient,
  sessionOptions,
  useScratchState,
  waitFor,
} from "./channel-session-harness"
import { FakeNatsServer } from "./fake-nats-server"
import { isolatePaneEnvironment } from "./pane-environment"
import { beforeTheGuard, collected, ROW_GUARD_MS, rowStarted } from "./row-deadline"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

// Every row's hang guard (`ROW_GUARD_MS`, 120 s, `./row-deadline.ts`), set above the first row:
// Bun holds `setDefaultTimeout` only for the rows registered after it.
setDefaultTimeout(ROW_GUARD_MS)

isolatePaneEnvironment()

const scratchState = useScratchState()
/** A scratch HOME for a channel server this file runs as a child process of its own. */
const temporaryHome = temporaryDirectories("claude-envoy-home")
/** The plugin's entries in the repository's marketplace, which the two cases below read. */
const marketplaceEntries = marketplace.plugins.filter((entry) => entry.name === plugin.name)
/** The Dispatch token the channel answers a rejected frame with. */
const DISPATCH_TOKEN = fixtureSecret("dsp")

/**
 * The listener's relay of one stored frame to this session's consumer, the one path a message
 * reaches it by; its replies go to an inbox of its own, named after its sequence.
 */
function relayed(nats: FakeNats, seq: number, raw: string): void {
  nats.emitRelay("ses_claude", { subject: directSubject, seq, raw, reply: `_INBOX.${seq}` })
}

/**
 * A role-lane envelope the listener forwarded to the direct subject, under the key the listener
 * minted for the publish behind the arbiter's forward mark.
 */
const roleForwardRaw = JSON.stringify({
  event_id: "evt-role-7",
  dedupe_key: "envoy.role.forward.publish.0123456789abcdef0123456789abcdef",
  source: "agent",
  source_session: "ses_sender",
  topic: "notifications.role.reviewer",
  issued_at: 1_760_000_000_000,
  payload_summary: "Review please",
})

test("writes each Envoy envelope into the session as the rendered card that names its sender", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
    configFile: sessionConfigFile("claude", process.env),
  })

  await delivery.enqueue({ subject: directSubject, raw: deliveryRaw })

  expect(notifier.notifications).toEqual([
    {
      content:
        'envoy:\n  to: you (ses_claude)\n  from: ses_sender\n  at: "2025-10-09T08:53:20Z"\n  id: evt-42\n  urgency: high\n  reply_with:\n    tool: envoy_send\n    args:\n      session_id: ses_sender\n      in_reply_to: evt-42\n      message: ...\n  summary: Use the channel',
      eventId: "evt-42",
    },
  ])
})

/** A comment event on CORE-1's owner topic, as Dispatch's outbox publishes it: `payload` is the
 *  event's own. */
function commentEventRaw(type: string, eventId: string, payload: Record<string, unknown>): string {
  return JSON.stringify({
    event_id: eventId,
    source: "dispatch",
    source_event_id: "7",
    topic: `notifications.dispatch.issue.CORE-1.${type}`,
    dedupe_key: eventId,
    issued_at: 1_760_000_000_000,
    payload_summary: "CORE-1 comment",
    payload: JSON.stringify({
      id: 7,
      seq: 7,
      issue_key: "CORE-1",
      type,
      actor: { kind: "user", id: "alice" },
      notify: true,
      quiet: false,
      created_at: "2026-11-02T00:00:00Z",
      payload: {
        id: "22222222-2222-4222-8222-222222222222",
        issue_key: "CORE-1",
        author: { kind: "user", id: "alice" },
        created_at: "2026-11-02T00:00:00Z",
        ...payload,
      },
    }),
    trace_id: eventId,
  })
}

// This server never reads a cut body whole: its card is the delivery Dispatch sent, which keeps
// what its frame carried. A comment event that carried its body's first part alone keeps that
// part and the note naming the route that answers it whole, and a delete's event is a card of its
// own kind, which carries no body.
test("a cut comment's card keeps the part its frame carried and the note, and a delete is a card of its own kind", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
    configFile: sessionConfigFile("claude", process.env),
  })
  const subject = "notifications.dispatch.issue.CORE-1.comment"

  await delivery.enqueue({
    subject,
    raw: commentEventRaw("comment.created", "dispatch-cut", {
      body: "The first paragraph.",
      body_truncated: true,
    }),
  })
  await delivery.enqueue({
    subject,
    raw: commentEventRaw("comment.deleted", "dispatch-deleted", {
      deleted_at: "2026-11-03T00:00:00Z",
      deleted_by: { kind: "user", id: "alice" },
    }),
  })

  const [cut, deleted] = notifier.notifications.map((notification) => notification.content)
  expect(cut).toContain("type: comment.created")
  expect(cut).toContain("body: The first paragraph.")
  expect(cut).toContain(
    "the body is its first part only; read it whole with GET /api/v1/comments/22222222-2222-4222-8222-222222222222",
  )
  expect(deleted).toContain("type: comment.deleted")
  expect(deleted).toContain('deleted_at: "2026-11-03T00:00:00Z"')
  expect(deleted).not.toContain("body:")
})

test("delivers each dedupe key once and answers each relayed frame, never a receipt", async () => {
  // Nothing answers a sender: the listener's role forward counts as delivered once the stream
  // stored it, and the only bodies this process publishes are its frames' own relay replies.
  const nats = new FakeNats()
  const notifier = new FakeNotifier(nats.order)
  const stateDirectory = scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      notifier,
    }),
  )

  try {
    relayed(nats, 1, roleForwardRaw)
    relayed(nats, 2, roleForwardRaw)
    await waitFor(
      async () =>
        nats.repliesOn("_INBOX.1").includes("done") && nats.repliesOn("_INBOX.2").includes("done"),
      "both frames answered",
    )

    expect(notifier.notifications).toHaveLength(1)
    expect(nats.repliesOn("_INBOX.1")).toEqual(["received", "done"])
    expect(nats.repliesOn("_INBOX.2")).toEqual(["received", "done"])
    // The only bodies on the bus are those four answers: no frame is answered a receipt.
    expect(nats.published).toHaveLength(4)
  } finally {
    await session.shutdown()
  }
})

/**
 * One Dispatch message as the listener publishes it for one send: `/v1/messages/send` mints a
 * fresh `event_id`, `source_event_id` and `trace_id` for every request and derives `dedupe_key`
 * from the idempotency key Dispatch passes, which is the same for an attempt and its same-mode
 * Retry (`packages/envoy/internal/contracts/message_envelope.go` AgentSendEnvelope, which
 * `packages/envoy/cmd/listener/api.go` sendHandler calls).
 */
function listenerSend(send: string): string {
  return JSON.stringify({
    event_id: `evt-${send}`,
    source: "dispatch",
    source_event_id: `src-${send}`,
    topic: directSubject,
    dedupe_key: "agent.ses_claude.33333333-3333-4333-8333-333333333333:aside",
    issued_at: 1_760_000_000_000,
    payload_summary: "Run the migration",
    payload: JSON.stringify({
      event: {
        actor: { id: "alice", kind: "user" },
        issue_key: "CORE-1",
        payload: {
          author: { id: "alice", kind: "user" },
          body: "Run the migration",
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id: "33333333-3333-4333-8333-333333333333",
          in_reply_to: null,
          issue_key: "CORE-1",
          target: "session:ses_claude",
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode: "aside" },
    }),
    trace_id: `trace-${send}`,
  })
}

test("a Dispatch Retry the listener re-sends under the same dedupe key reaches Claude once", async () => {
  const nats = new FakeNats()
  const notifier = new FakeNotifier()
  const stateDirectory = scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      notifier,
    }),
  )

  try {
    relayed(nats, 1, listenerSend("attempt"))
    relayed(nats, 2, listenerSend("retry"))
    await waitFor(async () => nats.repliesOn("_INBOX.2").includes("done"), "the retry's done")

    expect(notifier.notifications).toHaveLength(1)
    expect(notifier.notifications[0]?.eventId).toBe("evt-attempt")
  } finally {
    await session.shutdown()
  }
})

test("a send whose notification failed is delivered when it is sent again", async () => {
  const nats = new FakeNats()
  const delivered: string[] = []
  let failNext = true
  const stateDirectory = scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      notifier: {
        notification: async (notification) => {
          if (failNext) {
            failNext = false
            throw new Error("the MCP transport refused the write")
          }
          delivered.push(String(notification.eventId))
        },
      },
    }),
  )

  try {
    relayed(nats, 1, listenerSend("attempt"))
    await waitFor(async () => nats.repliesOn("_INBOX.1").includes("nak"), "the failed frame's nak")
    relayed(nats, 2, listenerSend("retry"))
    await waitFor(async () => nats.repliesOn("_INBOX.2").includes("done"), "the retry's done")
    relayed(nats, 3, listenerSend("third"))
    await waitFor(async () => nats.repliesOn("_INBOX.3").includes("done"), "the third's done")

    // Claude never saw the first send, so its key must not turn the second away as a repeat; it
    // did see the second, so the third is one.
    expect(delivered).toEqual(["evt-retry"])
  } finally {
    await session.shutdown()
  }
})

// What replaces the server's record of the cards it has (INFRA-1026 review, round eleven): the
// server is at-least-once and re-hands the event, and the mod's write is idempotent by the
// Envoy event id, so the engine sees the one message once. Both copies are therefore handed
// over, each carrying `evt-42`, and the inbox names the message once. The mod's own half of
// this is driven against the real `$` in `./mod.test.ts`.
test("a card Envoy sends again is handed over again, under the one event id", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
    configFile: sessionConfigFile("claude", process.env),
  })

  expect(await delivery.enqueue({ subject: directSubject, raw: deliveryRaw })).toBe(true)
  expect(await delivery.enqueue({ subject: directSubject, raw: deliveryRaw })).toBe(true)

  expect(notifier.notifications.map(({ eventId }) => eventId)).toEqual(["evt-42", "evt-42"])
  // One message, named once: the inbox lists the card, not every frame carrying it.
  expect(delivery.inbox().map(({ event_id }) => event_id)).toEqual(["evt-42"])
})

// Repro `R-A` (INFRA-1026 review, round eleven) at the server: a mod that never answers — a
// SIGSTOPped Claude Code, one that polls for nothing, one that is gone — leaves the frame
// unanswered, so Envoy sends the card again and this server writes it to the session again. The
// message is never lost to keep it from arriving twice, and what keeps it from arriving twice is
// the mod's record of its Envoy event id (`../hooks/mod-writer.ts`, `writeOnce`), not a record
// here.
test("a card the mod never answered is written again when Envoy resends it", async () => {
  const nats = new FakeNats()
  const written: string[] = []
  const holding: Array<() => void> = []
  const claude = modSession({
    link: {
      connected: () => true,
      request: <N extends ModCallName>(
        call: N,
        args: ModCallArguments[N],
        options?: { readonly timeoutMs?: number },
      ): Promise<ModCallResults[N]> => {
        if (call !== "session.message") {
          return Promise.reject(new Error(`the card path made an unexpected mod call: ${call}`))
        }
        written.push((args as ModCallArguments["session.message"]).message.text)
        // A mod that answers nothing: the caller's own bound ends the wait, as the link's does.
        const held = Promise.withResolvers<ModCallResults[N]>()
        const timer = setTimeout(
          () =>
            held.reject(
              new Error(`the mod did not answer ${call} within ${options?.timeoutMs ?? 0} ms`),
            ),
          options?.timeoutMs ?? 0,
        )
        timer.unref()
        holding.push(() => {
          clearTimeout(timer)
          held.reject(new Error("the session ended"))
        })
        return held.promise
      },
    },
    showMs: 40,
    warn: () => undefined,
  })
  const notifier: ChannelNotifier = {
    notification: async ({ content, blocks, eventId }) => {
      await claude.show({
        content: blocks ?? [{ text: content, type: "text" }],
        text: content,
        ...(eventId === undefined ? {} : { eventId }),
      })
    },
  }
  const stateDirectory = scratchState()
  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      connection: nats,
      notifier,
    }),
  )

  try {
    relayed(nats, 1, deliveryRaw)
    await waitFor(async () => written.length === 1, "the card's first write")
    // The mod answers nothing, so the write rejects at its bound and the frame is naked: that
    // nak is the signal the first delivery is over, which no duration guesses at.
    await waitFor(async () => nats.repliesOn("_INBOX.1").includes("nak"), "the frame's nak")
    // Envoy sends it again: the same event on the same stream sequence, a second delivery.
    nats.emitRelay("ses_claude", {
      subject: directSubject,
      seq: 1,
      raw: deliveryRaw,
      reply: "_INBOX.1-again",
      delivered: 2,
    })
    await waitFor(async () => written.length === 2, "the resent card's own write")

    expect(written).toHaveLength(2)
  } finally {
    for (const stop of holding.splice(0)) stop()
    await session.shutdown()
  }
})

test("retains only the latest fifty inbound envelope summaries", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
    configFile: sessionConfigFile("claude", process.env),
  })

  for (let index = 0; index < 51; index += 1) {
    await delivery.enqueue({
      subject: directSubject,
      raw: JSON.stringify({
        event_id: `evt-${index}`,
        source: "agent",
        source_session: "ses_sender",
        issued_at: index,
        payload_summary: `message ${index}`,
      }),
    })
  }

  expect(delivery.inbox()).toHaveLength(50)
  expect(delivery.inbox()[0]).toEqual({
    event_id: "evt-50",
    at: "1970-01-01T00:00:00Z",
    from: "ses_sender",
    summary: "message 50",
  })
  expect(delivery.inbox()[49]).toEqual({
    event_id: "evt-1",
    at: "1970-01-01T00:00:00Z",
    from: "ses_sender",
    summary: "message 1",
  })
})

// A frame with no `event_id` names no event, so the inbox cannot read one as a copy of another:
// two of them are two messages, and both are written to the session (INFRA-1026 review). The
// rule before this one keyed every such frame by the word the entry shows — `unknown` — so the
// inbox named the first and silently dropped the rest while the session read all of them.
test("every frame with no event id is listed, since none of them names a copy", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
    configFile: sessionConfigFile("claude", process.env),
  })
  const unnamed = (summary: string): string =>
    JSON.stringify({
      source: "agent",
      source_session: "ses_sender",
      issued_at: 0,
      payload_summary: summary,
    })

  expect(await delivery.enqueue({ subject: directSubject, raw: unnamed("the first") })).toBe(true)
  expect(await delivery.enqueue({ subject: directSubject, raw: unnamed("the second") })).toBe(true)

  // Written twice, so listed twice: the inbox says what the session was handed.
  expect(notifier.notifications).toHaveLength(2)
  expect(delivery.inbox().map(({ summary }) => summary)).toEqual(["the second", "the first"])
})

test("registers no delivery mode for a session whose channel Claude Code did not load", async () => {
  const stateDirectory = scratchState()
  const subscribes: unknown[] = []
  const client: FakeClient = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input)
      return noInterest()
    },
  })

  const session = await startChannelSession(
    sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
      client,
      capabilities: () => [],
    }),
  )

  try {
    expect(subscribes).toEqual([
      expect.objectContaining({ sessionID: "ses_claude", capabilities: [] }),
    ])
  } finally {
    await session.shutdown()
    await Bun.sleep(5)
  }
})

test("registers aside capability and soft-reclaims the role persisted for a resumed session id", async () => {
  const stateDirectory = scratchState()
  const roleFile = roleStateFile(stateDirectory, "ses_claude")
  await mkdir(dirname(roleFile), { recursive: true })
  await writeFile(roleFile, JSON.stringify({ session_id: "ses_claude", role: "reviewer" }))
  const subscribes: unknown[] = []
  const setRoles: unknown[] = []
  const client: FakeClient = recordingClient([], {
    subscribe: async (input) => {
      subscribes.push(input)
      return noInterest()
    },
    setRole: async (input) => {
      setRoles.push(input)
      return { claimed: true, interest: noInterest() }
    },
  })

  const options = sessionOptions(new SessionIdentity("ses_claude", "/tmp"), stateDirectory, {
    client,
    pluginVersion: "afe10b2f3e1b",
  })
  const session = await startChannelSession(options)

  try {
    expect(subscribes).toEqual([
      expect.objectContaining({
        sessionID: "ses_claude",
        topics: [directSubject],
        capabilities: ["aside", "steer"],
        selfSubscribed: true,
        plugin: { name: "@sjawhar/claude-envoy", version: "afe10b2f3e1b" },
      }),
    ])
    expect(setRoles).toEqual([
      {
        sessionID: "ses_claude",
        role: "reviewer",
        soft: true,
        previousSessionID: "ses_claude",
        clientInstanceID: options.clientInstanceID,
      },
    ])
    expect(JSON.parse(await readFile(roleFile, "utf8"))).toEqual({
      session_id: "ses_claude",
      role: "reviewer",
      client_instance_id: options.clientInstanceID,
    })
  } finally {
    await session.shutdown()
  }
})

test("the committed manifests declare no version the release would have to bump; omp and the MCP server report package.json's", () => {
  // Claude Code names an installed plugin by the `version` of its .claude-plugin/plugin.json,
  // never by the version the registry served: with none it calls the install `unknown`, caches
  // it under that name and sees no later release as an update (measured on 2.1.311). So
  // release.yaml's claude_envoy_build writes the release's version into the manifest it packs,
  // and the committed one declares none — a string committed here would hold every install at it
  // until someone changed it, and the release writes it anyway. The entry in the marketplace
  // declares no `version` of its own either, which is what leaves the packed manifest to name
  // an install; the range on the entry's npm source is a field of its own, below.
  expect(plugin).not.toHaveProperty("version")
  expect(marketplaceEntries).toHaveLength(1)
  expect(marketplaceEntries[0]).not.toHaveProperty("version")
  expect(ompPlugin.version).toBe(pkg.version)
  // The Codex CLI names its plugin cache directory by this version and reinstalls when it changes
  // (measured on 0.154.0), so release.yaml writes the release's version into it as well, and into
  // the Codex package's own package.json, which publishes it at the same version as this one.
  expect(codexPlugin.version).toBe(pkg.version)
  expect(codexPkg.version).toBe(pkg.version)
  expect(MCP_SERVER_INFO).toEqual({ name: "envoy", version: pkg.version })
})

test("the Codex manifest starts the server in the plugin's own directory, as the Codex host", () => {
  // Codex runs a plugin's MCP server with `cwd: "./"` in the plugin's directory and expands no
  // `${PLUGIN_ROOT}` in its arguments; it passes the server only the variables `env_vars` names;
  // and it asks before every call of a plugin's tool unless the tool is pre-approved (all measured
  // on 0.154.0).
  const envoy = codexPlugin.mcpServers.envoy
  expect(envoy.cwd).toBe("./")
  expect(envoy.args).toEqual(["./dist/envoy-channel.js", "--host", "codex"])
  // Every variable that names where Envoy, NATS, Dispatch or the secrets broker is, or the
  // credential for it: a Legion session pod sets several of these (its NATS nkey seed and its
  // Dispatch token as files), and one Codex does not pass leaves the server without it. The read
  // settings narrow the server's Dispatch trade, and the token files turn its Dispatch off, so
  // the server must see those too.
  for (const name of [
    "AGENTBOX_HOST",
    "AGENT_SECRETS_MACHINE_AUDIENCE",
    "AGENT_SECRETS_MACHINE_IDENTITY",
    "AGENT_SECRETS_MACHINE_TOKEN_FILE",
    "AGENT_SECRETS_URL",
    "DISPATCH_BROKER_DPOP_KEY_FILE",
    "DISPATCH_BROKER_TOKEN_FILE",
    "DISPATCH_PUBLIC_URL",
    "DISPATCH_STATE_DIR",
    "DISPATCH_TOKEN",
    "DISPATCH_TOKEN_BROKER",
    "DISPATCH_TOKEN_BROKER_AS_OF",
    "DISPATCH_TOKEN_BROKER_READ",
    "DISPATCH_TOKEN_FILE",
    "DISPATCH_URL",
    "ENVOY_CONFIG_FILE",
    "ENVOY_MACHINE_ID",
    "ENVOY_NATS_URL",
    "ENVOY_TOKEN",
    "ENVOY_TOKEN_FILE",
    "ENVOY_URL",
    "NATS_NKEY_SEED",
    "NATS_NKEY_SEED_FILE",
    "XDG_CONFIG_HOME",
  ]) {
    expect(envoy.env_vars).toContain(name)
  }
  expect(envoy.tools).toEqual({ dispatch: { approval_mode: "approve" } })
  // Codex's validator refuses a manifest `hooks` field; with none, Codex reads hooks/hooks.json.
  expect(codexPlugin).not.toHaveProperty("hooks")
  const commands = Object.values(codexHooks.hooks).flatMap((groups) =>
    groups.flatMap((group) => group.hooks.map((hook) => hook.command)),
  )
  for (const command of commands) expect(command).toMatch(/^bun "\$\{PLUGIN_ROOT\}\/dist\//)
})

// Codex hands a plugin's MCP server only the variables its manifest's `env_vars` names, beside
// `HOME`, `PATH` and a few more it passes every server (README.md, "Codex hands a plugin's server
// only `HOME`, `PATH`…"). A Codex session given `NATS_AUTH=broker` in its environment, with
// NATS_AUTH missing from the list, dialled NATS with no credential, and a pool pod's trade threw
// for want of AGENT_SECRETS_POOL_*; one given `ENVOY_INBOUND=listener` with it missing would dial
// NATS rather than the listener's relay socket. The list is taken from the code's own names, so a
// variable the broker path comes to read fails this row until the manifest names it.
test("the Codex manifest names every variable the broker-token path reads", () => {
  const passedToEveryServer = new Set(["HOME"])
  const read = [NATS_AUTH_VAR, ENVOY_INBOUND_VAR, ...BROKER_VARIABLES, METADATA_HOST_VAR].filter(
    (name) => !passedToEveryServer.has(name),
  )
  const listed = new Set<string>(codexPlugin.mcpServers.envoy.env_vars)
  expect(read.filter((name) => !listed.has(name))).toEqual([])
})

test("the marketplace entry installs the plugin from npm, by name, above the placeholder", () => {
  // release.yaml's claude_envoy_build packs this package and its publish job uploads that tarball
  // to npm, so the registry name is package.json's own, read here rather than written twice. The
  // source's `version` is a range, not the version of an install: it leaves out 0.0.0, the
  // placeholder a person published so that trusted publishing could attach to the package at all,
  // and takes `latest`, which the resolver prefers while it satisfies the range. npm's `latest`
  // is that placeholder until the first release, and it holds no plugin, so an install before
  // then must fail by name rather than install nothing. 1.0.0 is what the version step computes
  // for the first release (release-bump.sh over CLAUDE_ENVOY_VERSION_ARGS from the placeholder's
  // 0.0.0). 99.0.0 is every release after it: a range that pinned the major, `^1.0.0` or `1.x`,
  // would take the placeholder out and the first release in just as well, and then hold every
  // install at the last 1.x while the releases carried on. Those three are what the range is
  // for, so they are what is asserted, rather than the string that happens to hold them.
  expect(marketplaceEntries).toHaveLength(1)
  const source = marketplaceEntries[0]?.source
  expect(source?.source).toBe("npm")
  expect(source?.package).toBe(pkg.name)
  const range = source?.version ?? ""
  expect(Bun.semver.satisfies("0.0.0", range)).toBe(false)
  expect(Bun.semver.satisfies("1.0.0", range)).toBe(true)
  expect(Bun.semver.satisfies("99.0.0", range)).toBe(true)
})

test("the Codex marketplace entry installs the Codex package from npm, above the placeholder", () => {
  // Codex reads .agents/plugins/marketplace.json in place of .claude-plugin/marketplace.json when
  // a repository has both (measured on 0.154.0). The range is the Claude Code entry's, for the
  // reasons the test above gives: the Codex package's first publish is a placeholder too.
  expect(codexMarketplace.plugins).toHaveLength(1)
  const entry = codexMarketplace.plugins[0]
  expect(entry?.name).toBe(codexPlugin.name)
  expect(entry?.source.source).toBe("npm")
  expect(entry?.source.package).toBe(codexPkg.name)
  const range = entry?.source.version ?? ""
  expect(Bun.semver.satisfies("0.0.0", range)).toBe(false)
  expect(Bun.semver.satisfies("1.0.0", range)).toBe(true)
  expect(Bun.semver.satisfies("99.0.0", range)).toBe(true)
})

test("the Oh My Pi manifest declares no MCP server", () => {
  // Oh My Pi reads .omp-plugin/plugin.json before .claude-plugin/plugin.json and a manifest
  // mcpServers replaces .mcp.json outright, so an empty map is what keeps omp from launching a
  // channel server that exits without Claude Code's session identity. Dropping the key falls
  // through to .mcp.json and starts it again.
  expect(ompPlugin.mcpServers).toEqual({})
})

const rejectedFrameRaw = JSON.stringify({
  event_id: "dispatch-malformed",
  source: "dispatch",
  source_event_id: "1",
  topic: directSubject,
  dedupe_key: "dispatch-malformed",
  issued_at: 1,
  payload_summary: "Can this ship?",
  payload: JSON.stringify({
    event: {
      actor: { id: "alice", kind: "user" },
      issue_key: "CORE-1",
      payload: { body: "Can this ship?", id: "44444444-4444-4444-8444-444444444444" },
      type: "message.created",
    },
    delivery: { attempt: 1, mode: "aside" },
  }),
  trace_id: "dispatch-malformed",
})

const rejectedCommentFrameRaw = JSON.stringify({
  event_id: "dispatch-malformed-comment",
  source: "dispatch",
  source_event_id: "2",
  topic: directSubject,
  dedupe_key: "dispatch-malformed-comment",
  issued_at: 1,
  payload_summary: "Please review this.",
  payload: JSON.stringify({
    event: {
      actor: { id: "alice", kind: "user" },
      issue_key: "CORE-1",
      payload: { body: "Please review this.", id: "comment-payload-1" },
      type: "comment.created",
    },
    delivery: {
      attempt: 1,
      mode: "aside",
      comment_id: "33333333-3333-4333-8333-333333333333",
      target: "session:ses_claude",
    },
  }),
  trace_id: "dispatch-malformed-comment",
})

test("answers a rejected targeted frame on Dispatch instead of notifying the model", async () => {
  let server: Server<undefined> | undefined
  try {
    const replies: Array<{
      readonly path: string
      readonly auth: string | null
      readonly body: unknown
    }> = []
    server = Bun.serve({
      port: 0,
      fetch: async (request) => {
        replies.push({
          path: new URL(request.url).pathname,
          auth: request.headers.get("authorization"),
          body: await request.json(),
        })
        return Response.json({})
      },
    })
    process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
    process.env["DISPATCH_TOKEN"] = DISPATCH_TOKEN
    const notifier = new FakeNotifier()
    const delivery = createChannelDelivery({
      identity: new SessionIdentity("ses_claude", process.cwd()),
      notifier,
      configFile: sessionConfigFile("claude", process.env),
    })

    // Not handed to Claude Code, so the forwarder releases its claim: Dispatch records the attempt
    // failed, and a same-mode Retry is answered again rather than dropped as a repeat.
    expect(await delivery.enqueue({ subject: directSubject, raw: rejectedFrameRaw })).toBe(false)

    expect(notifier.notifications).toEqual([])
    expect(replies).toEqual([
      {
        path: "/api/v1/messages/44444444-4444-4444-8444-444444444444/reply",
        auth: `Bearer ${DISPATCH_TOKEN}`,
        body: {
          actor: { kind: "session", id: "ses_claude" },
          attempt: 1,
          error: "Invalid Dispatch targeted delivery frame",
        },
      },
    ])
  } finally {
    server?.stop(true)
  }
})

// A report Dispatch made and gave no copy of its answer for (`409 KEY_COMMITTED`) answered the
// attempt: its line says it landed, never that it could not be reported.
test("a rejected frame's report that landed with no answer back is not said to be unreported", async () => {
  let server: Server<undefined> | undefined
  const written = spyOn(process.stderr, "write").mockImplementation(() => true)
  try {
    const replies: string[] = []
    server = Bun.serve({
      port: 0,
      fetch: (request) => {
        replies.push(new URL(request.url).pathname)
        return Response.json(
          { code: IDEMPOTENCY_KEY_COMMITTED, error: "this request already ran" },
          { status: 409 },
        )
      },
    })
    process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
    process.env["DISPATCH_TOKEN"] = DISPATCH_TOKEN
    const delivery = createChannelDelivery({
      identity: new SessionIdentity("ses_claude", process.cwd()),
      notifier: new FakeNotifier(),
      configFile: sessionConfigFile("claude", process.env),
    })

    expect(await delivery.enqueue({ subject: directSubject, raw: rejectedFrameRaw })).toBe(false)

    const lines = written.mock.calls.map((call) => String(call[0]))
    expect(replies).toEqual(["/api/v1/messages/44444444-4444-4444-8444-444444444444/reply"])
    expect(lines.filter((line) => line.includes("could not report"))).toEqual([])
    expect(
      lines.filter((line) =>
        line.startsWith(
          "envoy-channel: the report of the rejected delivery landed on Dispatch, which gave no copy of its answer — POST ",
        ),
      ),
    ).toHaveLength(1)
  } finally {
    written.mockRestore()
    server?.stop(true)
  }
})

// The forwarder releases the claim of a frame Claude Code was not handed, so the re-send of a
// Dispatch frame the channel answered with an error is answered again rather than dropped as a
// repeat of its dedupe key.
test("a Dispatch frame the channel answered with an error is answered again when re-sent", async () => {
  let server: Server<undefined> | undefined
  let session: ChannelSession | undefined
  try {
    const control = JSON.parse(
      rejectedFrameRaw.replaceAll(
        "44444444-4444-4444-8444-444444444444",
        "55555555-5555-4555-8555-555555555555",
      ),
    )
    const replied: string[] = []
    server = Bun.serve({
      port: 0,
      fetch: (request) => {
        replied.push(new URL(request.url).pathname)
        return Response.json({})
      },
    })
    const answersTo = (id: string) => replied.filter((path) => path.includes(id)).length
    process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
    process.env["DISPATCH_TOKEN"] = DISPATCH_TOKEN
    const nats = new FakeNats()
    const notifier = new FakeNotifier()
    const stateDirectory = scratchState()
    session = await startChannelSession(
      sessionOptions(new SessionIdentity("ses_claude", process.cwd()), stateDirectory, {
        connection: nats,
        notifier,
      }),
    )

    relayed(nats, 1, rejectedFrameRaw)
    // Answered on Dispatch, and the frame is done: the key it was claimed under is released
    // again, since Claude Code was never handed the message. The channel answers a frame `done`
    // only once it is through with it, its reply on Dispatch posted, so each verdict below is
    // read at that frame's own `done`.
    expect(await nats.settled("_INBOX.1")).toBe("done")
    expect(answersTo("44444444")).toBe(1)

    // The listener mints a fresh event id for the re-send; its dedupe key is the first send's.
    relayed(nats, 2, JSON.stringify({ ...JSON.parse(rejectedFrameRaw), event_id: "resend" }))
    // A frame of another key after it. The two answers can reach Dispatch in either order, since
    // each reads the session's settings first, asynchronously, so neither frame's answer stands
    // for the other's: each is read at its own `done`.
    relayed(
      nats,
      3,
      JSON.stringify({ ...control, event_id: "control", dedupe_key: "dispatch-control" }),
    )
    expect(await Promise.all([nats.settled("_INBOX.2"), nats.settled("_INBOX.3")])).toEqual([
      "done",
      "done",
    ])

    expect(notifier.notifications).toEqual([])
    expect(answersTo("44444444")).toBe(2)
    expect(answersTo("55555555")).toBe(1)
  } finally {
    await session?.shutdown()
    server?.stop(true)
  }
})

test("answers a malformed targeted comment through its supplied comment reply address", async () => {
  let server: Server<undefined> | undefined
  try {
    const replies: Array<{
      readonly path: string
      readonly auth: string | null
      readonly body: unknown
    }> = []
    server = Bun.serve({
      port: 0,
      fetch: async (request) => {
        replies.push({
          path: new URL(request.url).pathname,
          auth: request.headers.get("authorization"),
          body: await request.json(),
        })
        return Response.json({})
      },
    })
    process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
    process.env["DISPATCH_TOKEN"] = DISPATCH_TOKEN
    const notifier = new FakeNotifier()
    const delivery = createChannelDelivery({
      identity: new SessionIdentity("ses_claude", process.cwd()),
      notifier,
      configFile: sessionConfigFile("claude", process.env),
    })

    await delivery.enqueue({ subject: directSubject, raw: rejectedCommentFrameRaw })

    expect(notifier.notifications).toEqual([])
    expect(replies).toEqual([
      {
        path: "/api/v1/comments/33333333-3333-4333-8333-333333333333/reply",
        auth: `Bearer ${DISPATCH_TOKEN}`,
        body: {
          actor: { kind: "session", id: "ses_claude" },
          attempt: 1,
          target: "session:ses_claude",
          error: "Invalid Dispatch targeted delivery frame",
        },
      },
    ])
  } finally {
    server?.stop(true)
  }
})

test("drops a malformed targeted frame that has no reply address", async () => {
  const notifier = new FakeNotifier()
  const delivery = createChannelDelivery({
    identity: new SessionIdentity("ses_claude", "/tmp"),
    notifier,
    configFile: sessionConfigFile("claude", process.env),
  })

  await delivery.enqueue({
    subject: directSubject,
    raw: JSON.stringify({
      event_id: "dispatch-junk",
      source: "dispatch",
      payload: JSON.stringify({ delivery: { attempt: "one" } }),
    }),
  })

  expect(notifier.notifications).toEqual([])
  expect(delivery.inbox()).toEqual([])
})

/** The variables a pod's broker trade reads beside DISPATCH_URL, which `isolatePaneEnvironment`
 *  leaves alone: each test that sets one restores the process's own value after it. They are
 *  cipher-client's own list, less the home directories every process keeps. */
const brokerVariables = [
  "DISPATCH_TOKEN_BROKER",
  ...BROKER_VARIABLES.filter((name) => name !== "HOME" && name !== "XDG_STATE_HOME"),
]

test("the user-turn router's Dispatch client trades under the session's id", async () => {
  const original = brokerVariables.map((name) => [name, process.env[name]] as const)
  const servers: Server<undefined>[] = []
  try {
    // The session each trade asked the broker to write under, as its `session` form field.
    const trades: string[] = []
    servers.push(
      Bun.serve({
        port: 0,
        hostname: "127.0.0.1",
        fetch: async (request) => {
          const form = new URLSearchParams(await request.text())
          trades.push(form.get("session") ?? "no session")
          return Response.json({
            access_token: fixtureSecret("access"),
            issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
            token_type: "DPoP",
            expires_in: 600,
            credential_id: "cred-1",
          })
        },
      }),
    )
    const schemes: string[] = []
    servers.push(
      Bun.serve({
        port: 0,
        hostname: "127.0.0.1",
        fetch: (request) => {
          schemes.push((request.headers.get("authorization") ?? "").split(" ")[0] ?? "")
          return Response.json({ kind: "agent", owner: null, service: null })
        },
      }),
    )
    const [broker, dispatch] = servers
    // A pod launched under the broker setting: a projected token to trade, no bearer, and no
    // AGENT_SECRETS_SESSION, which no pod sets.
    const home = temporaryHome("router-dispatch")
    const projected = join(home, "token")
    await writeFile(projected, "projected.service.account.token\n")
    for (const name of brokerVariables) delete process.env[name]
    process.env["DISPATCH_URL"] = `http://127.0.0.1:${dispatch?.port}`
    process.env["DISPATCH_TOKEN_BROKER"] = "1"
    process.env["AGENT_SECRETS_URL"] = `http://127.0.0.1:${broker?.port}`
    process.env["AGENT_SECRETS_MACHINE_IDENTITY"] = "kubernetes"
    process.env["AGENT_SECRETS_MACHINE_TOKEN_FILE"] = projected
    // Nobody signed in, so the trade is the pod's own.
    process.env["AGENT_SECRETS_SIGNIN_DIR"] = join(home, "signins")

    const client = await userTurnDispatchClient(
      new SessionIdentity("ses_claude", home),
      sessionConfigFile("claude", process.env),
      5_000,
    )
    if (client === undefined) throw new Error("Dispatch is off for the router")
    await client.whoami()

    expect(trades).toEqual(["ses_claude"])
    expect(schemes).toEqual(["DPoP"])
  } finally {
    forgetBrokerClients()
    for (const server of servers) server.stop(true)
    for (const [name, value] of original) {
      if (value === undefined) delete process.env[name]
      else process.env[name] = value
    }
  }
})

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

// The rows below run the channel server as a child process of its own, with Bun's transpiler cache
// off (`./transpiler-cache.ts`), and wait for what it answers or does: one that ends first is
// named with its exit, and an answer or an exit that never comes fails the row by name before the
// hang guard this file sets as its rows' default (`ROW_GUARD_MS`, 120 s, `./row-deadline.ts`). A
// fixed 20 s decided these rows before: at a load near 1,000 a server that wrote that cache into
// its empty HOME first had not answered its `initialize` in 20 s.
beforeEach(rowStarted)

/**
 * Runs the channel server as Claude Code does, in `env.HOME` with `env` as its whole environment:
 * it initializes over MCP, lists the tools, calls `envoy_whoami`, then each of `calls` (request ids
 * 4 and on), then closes stdin. Returns the exit code, each response by its request id, and stderr.
 */
async function runChannelServer(
  env: Readonly<Record<string, string>> & { readonly HOME: string },
  calls: readonly { readonly name: string; readonly arguments: object }[] = [],
): Promise<{
  readonly exitCode: number
  readonly responses: ReadonlyMap<number, Record<string, unknown>>
  readonly stderr: string
}> {
  const server = Bun.spawn(["bun", channelEntry], {
    cwd: env.HOME,
    env: { PATH: process.env["PATH"] ?? "", ...NO_TRANSPILER_CACHE, ...env },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })
  const stderr = collected(server.stderr)
  const responses = new Map<number, Record<string, unknown>>()
  const state = () =>
    `it answered requests [${[...responses.keys()].join(", ")}] and wrote to stderr: ${stderr.text()}`
  const waiters = new Map<number, () => void>()
  // A response, or the server's exit naming the request it never answered.
  const answered = (id: number): Promise<void> => {
    if (responses.has(id)) return Promise.resolve()
    const waiting = Promise.withResolvers<void>()
    waiters.set(id, waiting.resolve)
    return Promise.race([
      waiting.promise,
      server.exited.then((code) => {
        throw new Error(`the channel server exited ${code} before answering request ${id}`)
      }),
    ])
  }
  const reading = (async () => {
    let buffered = ""
    for await (const chunk of server.stdout.pipeThrough(new TextDecoderStream())) {
      buffered += chunk
      for (let newline = buffered.indexOf("\n"); newline !== -1; newline = buffered.indexOf("\n")) {
        const message = JSON.parse(buffered.slice(0, newline)) as Record<string, unknown>
        buffered = buffered.slice(newline + 1)
        const id = message["id"]
        if (typeof id !== "number") continue
        responses.set(id, message)
        waiters.get(id)?.()
      }
    }
  })()
  const send = (message: object) => server.stdin.write(`${JSON.stringify(message)}\n`)
  try {
    send({
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: {
        protocolVersion: "2025-06-18",
        capabilities: {},
        clientInfo: { name: "test", version: "0" },
      },
    })
    await beforeTheGuard(answered(1), "the channel server's answer to initialize", state)
    send({ jsonrpc: "2.0", method: "notifications/initialized" })
    send({ jsonrpc: "2.0", id: 2, method: "tools/list", params: {} })
    send({
      jsonrpc: "2.0",
      id: 3,
      method: "tools/call",
      params: { name: "envoy_whoami", arguments: {} },
    })
    for (const [index, call] of calls.entries()) {
      send({ jsonrpc: "2.0", id: 4 + index, method: "tools/call", params: call })
    }
    await beforeTheGuard(
      Promise.all([answered(2), answered(3), ...calls.map((_, index) => answered(4 + index))]),
      "the channel server's answers to requests 2 and on",
      state,
    )
    await server.stdin.end()
    const [exitCode] = await beforeTheGuard(
      Promise.all([server.exited, stderr.ended, reading]),
      "the channel server's exit once stdin ended",
      state,
    )
    return { exitCode, responses, stderr: stderr.text() }
  } finally {
    server.kill("SIGKILL")
  }
}

test("with CLAUDE_CONFIG_DIR holding no envoy.json the server names the unread old file, lists no tool, and exits 0 when stdin ends", async () => {
  const home = temporaryHome("off")
  const configDirectory = join(home, "claude-config")
  const old = join(home, ".config", "opencode", "envoy.json")
  await mkdir(configDirectory)
  await mkdir(dirname(old), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: "old-token",
      },
    }),
  )

  const { exitCode, responses, stderr } = await runChannelServer({
    HOME: home,
    CLAUDE_CONFIG_DIR: configDirectory,
    CLAUDE_CODE_SESSION_ID: "ses_off",
    CLAUDE_PLUGIN_DATA: home,
  })
  const file = join(configDirectory, "envoy.json")
  const notice = `${old} is not read for this profile; copy or link it to ${file}, or write that file`

  expect(responses.get(2)?.["result"]).toEqual({ tools: [] })
  expect(responses.get(3)?.["error"]).toMatchObject({
    message: expect.stringContaining(
      `Envoy is off for this session: no envoy.json at ${file}: Envoy is off; ${notice}`,
    ),
  })
  expect(stderr).toContain(
    `envoy-channel: no envoy.json at ${file}: Dispatch and Envoy are off; ${notice}`,
  )
  expect(exitCode).toBe(0)
})

test("an envoy.json that turns Envoy off opens no NATS connection and calls no listener, whatever the variables name", async () => {
  let stopBroker: (() => Promise<void>) | undefined
  let listener: Server<undefined> | undefined
  try {
    const home = temporaryHome("off-file")
    const configDirectory = join(home, "claude-config")
    await mkdir(configDirectory)
    await writeFile(
      join(configDirectory, "envoy.json"),
      JSON.stringify({ envoy: { enabled: false } }),
    )
    // An address of its own (fakeHost): the count below is the one a stray client inflates.
    const broker = new FakeNatsServer({ host: "127.86.0.2" })
    stopBroker = () => broker.stop()
    const listenerCalls: string[] = []
    listener = Bun.serve({
      port: 0,
      fetch: (request) => {
        listenerCalls.push(`${request.method} ${new URL(request.url).pathname}`)
        return Response.json({ error: "no call belongs here" }, { status: 500 })
      },
    })
    const { exitCode, responses } = await runChannelServer({
      HOME: home,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_CODE_SESSION_ID: "ses_off_file",
      CLAUDE_PLUGIN_DATA: home,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      ENVOY_NATS_URL: broker.url,
    })

    expect(responses.get(2)?.["result"]).toEqual({ tools: [] })
    expect(exitCode).toBe(0)
    expect(broker.connections).toBe(0)
    expect(listenerCalls).toEqual([])
  } finally {
    listener?.stop(true)
    await stopBroker?.()
  }
})

// The listener's bearer and the NATS seed are resolved once and handed to the client and the
// connection: a server that dropped either would leave the session unregistered or signed out.
test("the server calls the listener with the file's envoy.token and signs in to NATS with its seed", async () => {
  let stopBroker: (() => Promise<void>) | undefined
  let listener: Server<undefined> | undefined
  let server: Subprocess<"pipe", "pipe", "pipe"> | undefined
  try {
    const home = temporaryHome("credentials")
    const configDirectory = join(home, "claude-config")
    await mkdir(configDirectory)
    const broker = new FakeNatsServer({ nonce: "fake-nonce" })
    stopBroker = () => broker.stop()
    const user = nkeys.createUser()
    const auth: Array<string | null> = []
    const registered = Promise.withResolvers<void>()
    const plugins: unknown[] = []
    listener = Bun.serve({
      port: 0,
      fetch: async (request) => {
        auth.push(request.headers.get("Authorization"))
        const url = new URL(request.url)
        if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
          const body = (await request.json()) as {
            session_id: string
            topics: string[]
            plugin: string
            plugin_version: string
          }
          plugins.push({ plugin: body.plugin, plugin_version: body.plugin_version })
          registered.resolve()
          return Response.json({
            session_id: body.session_id,
            machine_id: "test",
            dir: "/tmp",
            topics: body.topics,
            // Every stand-in listener answers the session's own consumer
            // (`@legion/envoy-client/test/subscribe-answer`): one that answers no delivery leaves
            // its session refused inbound, which is a state of its own and not this case's. What
            // this case reads is what an accepted registration carries: its bearer and its plugin.
            delivery: consumerDelivery(body.session_id),
          })
        }
        if (request.method === "DELETE") return new Response(null, { status: 200 })
        return Response.json({ error: "not found" }, { status: 404 })
      },
    })
    const listenerToken = fixtureSecret("listener")
    await writeFile(
      join(configDirectory, "envoy.json"),
      JSON.stringify({
        envoy: {
          enabled: true,
          url: `http://127.0.0.1:${listener.port}`,
          natsUrls: [broker.url],
          token: listenerToken,
          natsSeed: new TextDecoder().decode(user.getSeed()),
        },
      }),
    )
    const running = Bun.spawn(["bun", channelEntry], {
      cwd: home,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: "ses_credentials",
        CLAUDE_PLUGIN_DATA: home,
        ...NO_TRANSPILER_CACHE,
      },
      stdin: "pipe",
      stdout: "pipe",
      stderr: "pipe",
    })
    server = running
    const stderr = collected(running.stderr)
    const state = () => `stderr: ${stderr.text()}`

    await beforeTheGuard(
      Promise.race([
        Promise.all([registered.promise, broker.until(() => broker.connects.length > 0)]),
        running.exited.then(async (code) => {
          throw new Error(
            `the channel server exited ${code} before it registered: ${await stderr.ended}`,
          )
        }),
      ]),
      "the channel server's registration and its NATS sign-in",
      state,
    )
    expect(broker.connects[0]).toMatchObject({ nkey: user.getPublicKey() })
    expect(new Set(auth)).toEqual(new Set([`Bearer ${listenerToken}`]))
    // The bundle runs from this checkout, outside Claude Code's plugin cache.
    expect(plugins[0]).toEqual({ plugin: "@sjawhar/claude-envoy", plugin_version: "source" })
    server.kill("SIGTERM")
    expect(
      await beforeTheGuard(running.exited, "the channel server's exit on SIGTERM", state),
    ).toBe(0)
  } finally {
    server?.kill("SIGKILL")
    listener?.stop(true)
    await stopBroker?.()
  }
})

/** A listener that answers `GET /v1/sessions` with no session and records every call. */
function recordingListener(calls: string[]) {
  return Bun.serve({
    port: 0,
    fetch: (request) => {
      const path = new URL(request.url).pathname
      calls.push(`${request.method} ${path}`)
      if (request.method === "GET" && path === "/v1/sessions") return Response.json([])
      return Response.json({ error: "no call belongs here" }, { status: 500 })
    },
  })
}

// Inbound messages arrive over NATS; the tools call the listener. An Envoy with no NATS server
// keeps its tools, as pi-envoy's does, and registers nothing a delivery would reach deaf.
test("with Envoy on and no NATS server the server serves its listener tools with inbound off, and says so", async () => {
  let listener: Server<undefined> | undefined
  try {
    const home = temporaryHome("no-nats")
    const configDirectory = join(home, "claude-config")
    await mkdir(configDirectory)
    const calls: string[] = []
    listener = recordingListener(calls)
    await writeFile(
      join(configDirectory, "envoy.json"),
      JSON.stringify({ envoy: { url: `http://127.0.0.1:${listener.port}` } }),
    )
    const { exitCode, responses, stderr } = await runChannelServer(
      {
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: "ses_no_nats",
        CLAUDE_PLUGIN_DATA: home,
      },
      [
        { name: "envoy_sessions", arguments: {} },
        { name: "envoy_subscribe", arguments: { topics: ["notifications.role.reviewer"] } },
      ],
    )

    const listed = responses.get(2)?.["result"] as { tools: { name: string }[] } | undefined
    expect(listed?.tools.map((tool) => tool.name)).toContain("envoy_send")
    expect(JSON.stringify(responses.get(3)?.["result"])).toContain("ses_no_nats")
    expect(responses.get(4)?.["result"]).toEqual({ content: [{ type: "text", text: "[]" }] })
    // A tool's error is the tool's result, which the model reads, and no JSON-RPC error.
    expect(responses.get(5)?.["error"]).toBeUndefined()
    expect(responses.get(5)?.["result"]).toEqual({
      content: [
        {
          type: "text",
          text: expect.stringContaining(
            "inbound messages are off for this session: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)",
          ),
        },
      ],
      isError: true,
    })
    expect(stderr).toContain(
      "envoy-channel: Envoy's inbound messages are off: Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL); the Envoy tools still work",
    )
    expect(calls).toEqual(["GET /v1/sessions"])
    expect(exitCode).toBe(0)
  } finally {
    listener?.stop(true)
  }
})

test("a tool call the listener refused because its sender is rate limited answers a result with isError that holds the listener's sentence, and no JSON-RPC error", async () => {
  const sentence =
    "Your sending is rate limited. This message was not sent. This listener lets one session send " +
    "4 messages a minute and 2 at once. You can send it again after 15 seconds."
  let listener: Server<undefined> | undefined
  try {
    const home = temporaryHome("rate-limited")
    const configDirectory = join(home, "claude-config")
    await mkdir(configDirectory)
    const calls: string[] = []
    // The listener's 429 for a session past its sending limit (`listenerwire.SenderRateLimited`,
    // built in packages/envoy/cmd/listener/sender_limit.go), to every call.
    listener = Bun.serve({
      port: 0,
      fetch: (request) => {
        calls.push(`${request.method} ${new URL(request.url).pathname}`)
        return Response.json(
          {
            error: sentence,
            reason: "sender_rate_limited",
            retry_after_seconds: 15,
            messages_per_minute: 4,
            burst: 2,
          },
          { status: 429, headers: { "Retry-After": "15" } },
        )
      },
    })
    await writeFile(
      join(configDirectory, "envoy.json"),
      JSON.stringify({ envoy: { url: `http://127.0.0.1:${listener.port}` } }),
    )
    // No NATS server, so the server registers nothing and the tools' calls are the only ones.
    const { exitCode, responses } = await runChannelServer(
      {
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: "ses_rate_limited",
        CLAUDE_PLUGIN_DATA: home,
      },
      [
        { name: "envoy_send", arguments: { session_id: "ses_target", message: "hello" } },
        { name: "envoy_publish", arguments: { topic: "notifications.team.x", message: "hello" } },
      ],
    )

    for (const id of [4, 5]) {
      expect(responses.get(id)?.["error"]).toBeUndefined()
      expect(responses.get(id)?.["result"]).toEqual({
        content: [{ type: "text", text: `${sentence} (reason: sender_rate_limited)` }],
        isError: true,
      })
    }
    // One call to each route, in whichever order the two concurrent calls reached the listener:
    // a refusal is not sent again.
    expect([...calls].sort()).toEqual(["POST /v1/messages/publish", "POST /v1/messages/send"])
    expect(exitCode).toBe(0)
  } finally {
    listener?.stop(true)
  }
})

test("a refused NATS seed leaves the listener tools on, opens no NATS connection, and says why", async () => {
  let listener: Server<undefined> | undefined
  let stopBroker: (() => Promise<void>) | undefined
  try {
    const home = temporaryHome("seed-refused")
    const configDirectory = join(home, "claude-config")
    await mkdir(configDirectory)
    const calls: string[] = []
    listener = recordingListener(calls)
    // An address of its own (fakeHost): the count below is the one a stray client inflates.
    const broker = new FakeNatsServer({ host: "127.86.0.3" })
    stopBroker = () => broker.stop()
    const file = join(configDirectory, "envoy.json")
    await writeFile(
      file,
      JSON.stringify({
        envoy: {
          url: `http://127.0.0.1:${listener.port}`,
          natsUrls: ["nats://file.internal.example:4222"],
          natsSeed: "SUFILESEED",
        },
      }),
    )
    const { exitCode, responses, stderr } = await runChannelServer(
      {
        HOME: home,
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_CODE_SESSION_ID: "ses_seed_refused",
        CLAUDE_PLUGIN_DATA: home,
        ENVOY_NATS_URL: broker.url,
      },
      [{ name: "envoy_sessions", arguments: {} }],
    )

    expect(responses.get(4)?.["result"]).toEqual({ content: [{ type: "text", text: "[]" }] })
    expect(stderr).toContain(
      `envoy-channel: Envoy's inbound messages are off: ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them; the Envoy tools still work`,
    )
    expect(broker.connections).toBe(0)
    expect(calls).toEqual(["GET /v1/sessions"])
    expect(exitCode).toBe(0)
  } finally {
    listener?.stop(true)
    await stopBroker?.()
  }
})

test("a Legion pane takes Aside and Send from the start and gains BTW when its mod connects", () => {
  // The pane's messages go through the bridge's inbox, which needs no mod; the side turn a BTW
  // is does. A terminal session's own rule is driven end to end in `envoy-channel-mcp.test.ts`.
  expect(sessionCapabilities({ pane: true, mod: false })).toEqual(["aside", "steer"])
  expect(sessionCapabilities({ pane: true, mod: true })).toEqual(["aside", "steer", "btw"])
})
