import { expect, test } from "bun:test"
import { chmodSync, writeFileSync } from "node:fs"
import { delimiter, join } from "node:path"
import { placedRoleName, placeRole } from "@legion/envoy-client/stream-topics"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import {
  createEnvoyClient,
  type EnvoyClient,
  UNRESPONSIVE_RULE,
} from "@legion/envoy-client/transport"
import { captureEnvironment } from "@legion/test-fixtures/environment"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import type { Server } from "bun"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  channelToolDefinitions,
  executeEnvoyTool,
} from "../src/envoy-channel-server"
import { SessionIdentity } from "../src/session-identity"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const makeDirectory = temporaryDirectories("claude-envoy-slack")

/** A session following `followed`, whose access token lists `projects` as the listener answered
 *  its registration (`placeRole`). */
function sessionWith(followed: string[], projects?: readonly string[]): ChannelSession {
  return {
    delivery: {
      enqueue: async () => true,
      inbox: () => [],
    },
    topics: () => ["notifications.agent.ses_claude", ...followed],
    follow: async (topics) => {
      const fresh = topics.filter((topic) => !followed.includes(topic))
      followed.push(...fresh)
      return fresh
    },
    unfollow: async () => [],
    rememberRole: async () => undefined,
    onRoleRegained: () => undefined,
    capabilitiesChanged: () => undefined,
    observe: () => undefined,
    messageOf: () => undefined,
    roleClaimName: (role) => placedRoleName(placeRole(role, projects)),
    shutdown: async () => undefined,
  }
}
const identity = new SessionIdentity("ses_claude", process.cwd())
/** The client instance of the channel-server process the tools act for. */
const INSTANCE = "a1b2c3d4e5f60718293a4b5c6d7e8f90"

test("declares exactly the shared Envoy tools, the bounded inbox included", () => {
  const definitions = channelToolDefinitions()

  expect(definitions.map(({ name }) => name)).toEqual(envoyToolSpecs.map(({ name }) => name))
  expect(definitions.map(({ name }) => name)).toContain("envoy_inbox")
})

test("sends Envoy messages through the shared transport", async () => {
  let body = ""
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (_request, init) => {
      body = String(init?.body)
      return Response.json({
        event_id: "evt-sent",
        source: "agent",
        source_event_id: "agent.ses_claude.evt-sent",
        source_session: "ses_claude",
        topic: "notifications.agent.ses_receiver",
        dedupe_key: "agent.ses_receiver.evt-sent",
        issued_at: 1,
        payload_summary: "ready",
        trace_id: "trace-sent",
        recipient: "ses_receiver",
      })
    },
  })
  const followed: string[] = []
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith(followed),
    clientInstanceID: INSTANCE,
  }

  const result = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
  })

  expect(JSON.parse(body)).toMatchObject({
    source: "agent",
    source_session: "ses_claude",
    target_session: "ses_receiver",
    message: "ready",
  })
  expect(result).toEqual({
    message: "sent evt-sent to ses_receiver (live)",
    event_id: "evt-sent",
    recipient: "ses_receiver",
    recipient_state: "live",
    confirmed: true,
    state: "connected",
  })
})

// The list is what the model reads before it decides a peer is dead: a session past its live
// window is still running, so it must be asked for and must arrive with the state and the
// heartbeat that say so (dispatch://INFRA-392).
test("lists the unresponsive sessions too, each with its state and last_seen", async () => {
  let requested = ""
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (request) => {
      requested = String(request)
      return Response.json([
        {
          session_id: "ses_stalled",
          machine_id: "host-a",
          dir: "/work",
          port: 0,
          title: "Reviewer",
          topics: [],
          self_subscribed: true,
          last_seen: 1_700_000_000_000,
          state: "unresponsive",
        },
      ])
    },
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith([]),
    clientInstanceID: INSTANCE,
  }

  const result = await executeEnvoyTool(runtime, "envoy_sessions", {})

  expect(requested).toBe("http://envoy.test/v1/sessions?include=unresponsive")
  expect(result).toEqual([
    expect.objectContaining({
      session_id: "ses_stalled",
      state: "unresponsive",
      last_seen: 1_700_000_000_000,
    }),
  ])
})

// No live session holds the role, so the listener kept the message for one hour and answered 202
// with kept_until, the end of that hour (dispatch://INFRA-2846, R3). The tool says until when the
// copy waits and, since this server always names its own session as the source, that a message
// comes if no session takes it by then, in the same line the Oh My Pi plugin shows. A holder that
// is not live is named with its state, and a listener of an earlier release names no time.
test("a kept publish says until when it is kept", async () => {
  const keptUntil = Date.UTC(2026, 10, 15, 10, 30)
  const until = "2026-11-15T10:30:00.000Z"
  const lastSeen = Date.UTC(2026, 10, 15, 9, 20)
  // What the listener answers for each role, read from the topic a publish names.
  const answers: Record<string, Record<string, unknown>> = {
    "notifications.role.legion-architect": { kept_reason: "unclaimed", kept_until: keptUntil },
    "notifications.role.reviewer": {
      kept_reason: "holder_lapsed",
      kept_until: keptUntil,
      holder: "ses_reviewer",
      holder_state: "unresponsive",
      last_seen: lastSeen,
    },
    // A listener of an earlier release: kept, and no time named.
    "notifications.role.controller": { kept_reason: "unclaimed" },
  }
  const published: Record<string, unknown>[] = []
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (_request, init) => {
      const body = JSON.parse(String(init?.body)) as { readonly topic: string }
      published.push(body)
      return new Response(
        JSON.stringify({
          event_id: "evt-kept",
          source: "agent",
          source_event_id: "agent.ses_claude.evt-kept",
          source_session: "ses_claude",
          topic: body.topic,
          dedupe_key: "agent.role.evt-kept",
          issued_at: 1,
          payload_summary: "the worker is stalled",
          trace_id: "trace-kept",
          kept: true,
          ...answers[body.topic],
        }),
        { status: 202, headers: { "Content-Type": "application/json" } },
      )
    },
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith([]),
    clientInstanceID: INSTANCE,
  }
  const publish = (topic: string) =>
    executeEnvoyTool(runtime, "envoy_publish", { topic, message: "the worker is stalled" })

  // The tool's description names the hour, kept_until and the message that follows it.
  const description = channelToolDefinitions().find(
    ({ name }) => name === "envoy_publish",
  )?.description
  expect(description).toContain("The listener then keeps the message for one hour.")
  expect(description).toContain("its kept_until is the time that hour ends")
  expect(description).toContain(
    "If no session takes the message by kept_until, Envoy drops it. If the publish named your session as its source, Envoy then sends your session a message that names the dropped message, unless it dropped it more than an hour late.",
  )

  expect(await publish("notifications.role.legion-architect")).toEqual({
    message: `published evt-kept. Envoy kept it for the role until ${until}: no live session holds the role now. The next session to hold the role before then gets it. If none does, Envoy drops it and sends you a message that names it, unless it drops it more than an hour late.`,
    event_id: "evt-kept",
    topic: "notifications.role.legion-architect",
    kept: true,
    kept_reason: "unclaimed",
    kept_until: keptUntil,
  })
  expect(published[0]).toMatchObject({ source_session: "ses_claude" })

  // A holder that is unresponsive: named with its state, with the warning that such a holder is
  // not dead and that the sender starts no replacement for it.
  const unresponsive = await publish("notifications.role.reviewer")
  expect(unresponsive).toEqual({
    message: `published evt-kept. Envoy kept it for the role until ${until}: its holder ses_reviewer is unresponsive, so no live session holds the role now. The next session to hold the role before then gets it. If none does, Envoy drops it and sends you a message that names it, unless it drops it more than an hour late. ${UNRESPONSIVE_RULE}`,
    event_id: "evt-kept",
    topic: "notifications.role.reviewer",
    holder: "ses_reviewer",
    holder_state: "unresponsive",
    last_seen: lastSeen,
    kept: true,
    kept_reason: "holder_lapsed",
    kept_until: keptUntil,
  })
  expect(unresponsive).toMatchObject({
    message: expect.stringMatching(
      /An unresponsive session is not dead.*Start no replacement for it/,
    ),
  })

  // A listener of an earlier release names no time, and the line says kept with no time.
  expect(await publish("notifications.role.controller")).toEqual({
    message:
      "published evt-kept. Envoy kept it for the role: no live session holds the role now. Envoy did not say until when it keeps it.",
    event_id: "evt-kept",
    topic: "notifications.role.controller",
    kept: true,
    kept_reason: "unclaimed",
  })
})

test("the role a tool claims names the process claiming it on the wire", async () => {
  // The listener records the claim's process as the role's holder, and frees a claim only for
  // the process that made it, so a role this `claude` takes cannot be released or taken by a
  // second one that resumed its session id (dispatch://INFRA-783).
  let body = ""
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (_request, init) => {
      body = String(init?.body)
      return Response.json({
        session_id: "ses_claude",
        machine_id: "devbox",
        dir: "/work",
        topics: ["notifications.role.reviewer"],
      })
    },
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith([]),
    clientInstanceID: INSTANCE,
  }

  await executeEnvoyTool(runtime, "envoy_role_set", { role: "reviewer" })

  // A token that lists no project claims a free-form role under none.
  expect(JSON.parse(body)).toEqual({
    session_id: "ses_claude",
    role: "_.reviewer",
    client_instance_id: INSTANCE,
  })
})

test("the role tools claim and read a free-form role under the one project the token lists", async () => {
  const requests: { readonly url: string; readonly body: string }[] = []
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (request, init) => {
      requests.push({ url: String(request), body: String(init?.body) })
      return String(request).includes("/v1/roles/set")
        ? Response.json({
            session_id: "ses_claude",
            machine_id: "devbox",
            dir: "/work",
            topics: ["notifications.role.PRIV.reviewer"],
          })
        : Response.json({ role: "PRIV.reviewer", holder: "ses_claude", last_seen: 1 })
    },
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith([], ["PRIV"]),
    clientInstanceID: INSTANCE,
  }

  await executeEnvoyTool(runtime, "envoy_role_set", { role: "reviewer" })
  await executeEnvoyTool(runtime, "envoy_role_get", { role: "reviewer" })

  expect(JSON.parse(requests[0]?.body ?? "")).toMatchObject({ role: "PRIV.reviewer" })
  expect(requests[1]?.url).toBe("http://envoy.test/v1/roles/PRIV.reviewer")
})

test("a send that is not confirmed names its key, and a resend under that key sends it again", async () => {
  const keys: string[] = []
  let answer: "time out" | "repeat" = "time out"
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async (_request, init) => {
      keys.push((JSON.parse(String(init?.body)) as { idempotency_key: string }).idempotency_key)
      if (answer === "time out") throw new DOMException("The operation timed out.", "TimeoutError")
      return Response.json({
        event_id: "evt-resent",
        source: "agent",
        source_event_id: "agent.ses_claude.evt-resent",
        source_session: "ses_claude",
        topic: "notifications.agent.ses_receiver",
        dedupe_key: `agent.ses_receiver.${keys[0]}`,
        issued_at: 1,
        payload_summary: "ready",
        trace_id: "trace-resent",
        recipient: "ses_receiver",
        duplicate: true,
      })
    },
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith([]),
    clientInstanceID: INSTANCE,
  }

  const failed = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
  }).then(
    () => undefined,
    (error: unknown) => (error instanceof Error ? error.message : String(error)),
  )
  const key = keys[0] as string
  expect(failed).toContain("The send to ses_receiver is not confirmed delivered")
  expect(failed).toContain(
    `Sending it again with idempotency_key ${key} within 72 hours is the safe resend`,
  )

  answer = "repeat"
  const resent = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
    idempotency_key: key,
  })

  expect(keys).toEqual([key, key, key])
  expect(resent).toMatchObject({
    message:
      "sent to ses_receiver once: an earlier attempt under the same idempotency_key was already stored, so this one stored no second copy",
  })
})

test("replies in a Slack thread with slack_post, with the token the broker grants", async () => {
  // The broker's client on PATH, standing in for the exec form once LEGION_SLACK_BOT_TOKEN is
  // granted, and a fake Slack Web API at SLACK_POST_API_URL.
  const restore = captureEnvironment(["PATH", "SLACK_POST_API_URL"])
  let slack: Server<undefined> | undefined
  try {
    const dir = makeDirectory()
    writeFileSync(
      join(dir, "agent-secrets"),
      '#!/bin/sh\nwhile [ "$1" != "--" ]; do shift; done\nshift\nLEGION_SLACK_BOT_TOKEN=xoxb-granted exec "$@"\n',
    )
    chmodSync(join(dir, "agent-secrets"), 0o755)
    const posts: unknown[] = []
    slack = Bun.serve({
      port: 0,
      hostname: "127.0.0.1",
      async fetch(request) {
        posts.push({
          authorization: request.headers.get("authorization"),
          body: await request.json(),
        })
        return Response.json({ ok: true, channel: "C00000000", ts: "1700000000.000200" })
      },
    })
    process.env["PATH"] = `${dir}${delimiter}${process.env["PATH"] ?? ""}`
    process.env["SLACK_POST_API_URL"] = `http://127.0.0.1:${slack.port}/api`
    const runtime: ChannelToolRuntime = {
      identity,
      client: createEnvoyClient({
        baseUrl: "http://envoy.test",
        fetch: async () => Response.json({}),
      }),
      session: sessionWith([]),
      clientInstanceID: INSTANCE,
    }

    const result = await executeEnvoyTool(runtime, "slack_post", {
      channel: "C00000000",
      thread_ts: "1700000000.000100",
      text: "Filed as ACME-12.",
    })

    expect(posts).toEqual([
      {
        authorization: "Bearer xoxb-granted",
        body: { channel: "C00000000", thread_ts: "1700000000.000100", text: "Filed as ACME-12." },
      },
    ])
    expect(result).toEqual({
      channel: "C00000000",
      thread_ts: "1700000000.000100",
      ts: "1700000000.000200",
    })
  } finally {
    slack?.stop(true)
    restore()
  }
})

test("a dispatch_* call is an unknown tool even with Dispatch configured, and reaches no Dispatch", async () => {
  let server: Server<undefined> | undefined
  try {
    const requests: string[] = []
    server = Bun.serve({
      port: 0,
      fetch: (request) => {
        requests.push(`${request.method} ${new URL(request.url).pathname}`)
        return Response.json({ error: "unexpected" }, { status: 500 })
      },
    })
    process.env["DISPATCH_URL"] = `http://127.0.0.1:${server.port}`
    process.env["DISPATCH_TOKEN"] = fixtureSecret("dsp")
    const runtime: ChannelToolRuntime = {
      identity,
      client: {} as EnvoyClient,
      session: sessionWith([]),
      clientInstanceID: INSTANCE,
    }

    await expect(
      executeEnvoyTool(runtime, "dispatch_ask", { issue: "DSP-3", question: "Approve?" }),
    ).rejects.toThrow("Unsupported Envoy tool: dispatch_ask")
    expect(requests).toEqual([])
  } finally {
    server?.stop(true)
  }
})

test("envoy_list reports the union of live NATS subscriptions and registry interests", async () => {
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async () =>
      Response.json({
        session_id: "ses_claude",
        machine_id: "devbox",
        dir: "/tmp",
        topics: ["notifications.agent.ses_claude", "notifications.dispatch.issue.DSP-9.>"],
      }),
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith(["notifications.dispatch.issue.DSP-3.>"]),
    clientInstanceID: INSTANCE,
  }

  const result = await executeEnvoyTool(runtime, "envoy_list", {})

  expect(result).toEqual({
    session_id: "ses_claude",
    machine_id: "devbox",
    dir: "/tmp",
    topics: [
      "notifications.agent.ses_claude",
      "notifications.dispatch.issue.DSP-9.>",
      "notifications.dispatch.issue.DSP-3.>",
    ],
    interests: [
      { topic: "notifications.agent.ses_claude", source: "both" },
      { topic: "notifications.dispatch.issue.DSP-9.>", source: "registry" },
      { topic: "notifications.dispatch.issue.DSP-3.>", source: "live" },
    ],
  })
})

test("envoy_sessions filters by the machine a session runs on and by the listener it registered through", async () => {
  const sessions = [
    {
      session_id: "ses_on_hub",
      machine_id: "envoy-hub",
      host: "gke-node-7",
      dir: "/work",
      port: 0,
      title: "",
      topics: [],
      self_subscribed: true,
    },
    {
      session_id: "ses_on_devbox",
      machine_id: "devbox",
      host: "devbox",
      dir: "/work",
      port: 0,
      title: "",
      topics: [],
      self_subscribed: true,
    },
  ]
  const client = createEnvoyClient({
    baseUrl: "http://envoy.test",
    fetch: async () => Response.json(sessions),
  })
  const runtime: ChannelToolRuntime = {
    identity,
    client,
    session: sessionWith([]),
    clientInstanceID: INSTANCE,
  }
  const listed = async (machine: string) =>
    (
      (await executeEnvoyTool(runtime, "envoy_sessions", { machine })) as { session_id: string }[]
    ).map((session) => session.session_id)

  expect(await listed("envoy-hub")).toEqual(["ses_on_hub"])
  expect(await listed("gke-node-7")).toEqual(["ses_on_hub"])
  expect(await listed("devbox")).toEqual(["ses_on_devbox"])
  expect(await listed("other")).toEqual([])
})

/** One envelope, as the listener echoes it back to a send or a publish. */
function envelope(topic: string): Record<string, unknown> {
  return {
    event_id: "evt-text",
    source: "agent",
    source_event_id: "agent.ses_claude.evt-text",
    source_session: "ses_claude",
    topic,
    dedupe_key: "dedupe-text",
    issued_at: 1,
    payload_summary: "message",
    trace_id: "trace-text",
  }
}

/** A runtime whose listener answers every send and publish with `answer`. */
function runtimeAnswering(answer: Record<string, unknown>): ChannelToolRuntime {
  return {
    identity,
    client: createEnvoyClient({
      baseUrl: "http://envoy.test",
      fetch: async () => Response.json(answer),
    }),
    session: sessionWith([]),
    clientInstanceID: INSTANCE,
  }
}

test("tells the model its message was queued, never sent, when the target is stopped", async () => {
  const runtime = runtimeAnswering({
    ...envelope("notifications.agent.ses_receiver"),
    recipient: "ses_receiver",
    // A session with no entry but a record: the registry's state stays one of its three.
    recipient_state: "gone",
    state: "stopped",
    last_seen: Date.UTC(2026, 10, 2, 9, 15, 0),
  })

  const result = await executeEnvoyTool(runtime, "envoy_send", {
    session_id: "ses_receiver",
    message: "ready",
  })

  expect(result).toEqual({
    message:
      "queued evt-text for ses_receiver: the session is stopped (last seen 2026-11-02T09:15:00Z); if it runs again within 72 hours, it gets the message",
    event_id: "evt-text",
    recipient: "ses_receiver",
    recipient_state: "gone",
    confirmed: true,
    state: "stopped",
    last_seen: Date.UTC(2026, 10, 2, 9, 15, 0),
  })
})

for (const published of [
  {
    counts: { recipients: 0, recipients_connected: 0 },
    label: "no other session subscribes",
    text: "published evt-text; no other session subscribes to notifications.team.test, so no other session gets it",
  },
  {
    counts: { recipients: 3, recipients_connected: 2 },
    label: "three other sessions get it",
    text: "published evt-text to 3 other sessions (2 running now)",
  },
  {
    counts: {},
    label: "a listener that counts nobody",
    text: "published evt-text.",
  },
]) {
  test(`renders a publish the listener answered with ${published.label}`, async () => {
    const runtime = runtimeAnswering({
      ...envelope("notifications.team.test"),
      ...published.counts,
    })

    const result = await executeEnvoyTool(runtime, "envoy_publish", {
      topic: "notifications.team.test",
      message: "broadcast",
    })

    // Strict, so a count the listener did not send is no key of the result at all.
    expect(result).toStrictEqual({
      message: published.text,
      event_id: "evt-text",
      topic: "notifications.team.test",
      ...published.counts,
    })
  })
}
