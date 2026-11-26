import { afterEach, beforeEach, expect, test } from "bun:test"
import { mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { timeLeft, until } from "@legion/test-fixtures/wait"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { modSocketPath } from "../hooks/mod-protocol"
import { waitFor } from "./channel-session-harness"
import { startFakeMod } from "./fake-mod"
import { FakeNatsServer } from "./fake-nats-server"
import { connectedBeforeTheDeadline, connectedToServer } from "./mcp-connect"
import { natsGate } from "./nats-gate"
import { ROW_TIMEOUT_MS } from "./reopen-server"
import { rowStarted, untilTheGuard } from "./row-deadline"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

// Marks each row's start for the dial row's `untilTheGuard` waits (`./row-deadline.ts`).
beforeEach(rowStarted)

/** Where each session's home is made. Its mod socket is `<home>/claude-envoy/ce-<hash>.sock`, and
 *  that whole path has 100 bytes to fit in (`MOD_SOCKET_PATH_MAX_BYTES`): under `tmpdir()` a deep
 *  `TMPDIR` (a dev box's `/mnt/scratch/...`, a Legion pod's) leaves none, and the session then names
 *  no socket. So the home goes under `/tmp`, whatever `TMPDIR` is, as channel-fatal-exit.test.ts's
 *  does. */
const SOCKET_ROOT = "/tmp"

/** A session home of its own, under {@link SOCKET_ROOT}. */
function sessionHome(prefix: string): Promise<string> {
  return mkdtemp(join(SOCKET_ROOT, prefix))
}

/**
 * Aborted by the file's `afterEach` once the row running now has ended, at its guard included,
 * and then replaced for the next row. A row hands the signal it began with to its fake mod
 * (`startFakeMod`'s `signal`) together with the signal for the MCP transport's close.
 */
let rowEnd = new AbortController()
afterEach(() => {
  rowEnd.abort(new Error("the row ended"))
  rowEnd = new AbortController()
})

/**
 * The SDK owns the child; its close callback reports when the transport is gone. `untilServed` is
 * `until` for a wait on this server: a failure of it also says what the server wrote to stderr.
 * The close's own reason already says so. `untilStarted` waits for the channel session to start:
 * the server answers tool calls only once it has.
 */
function observeServer(client: Client, transport: StdioClientTransport) {
  let stderr = ""
  transport.stderr?.on("data", (chunk: Buffer | string) => {
    stderr += String(chunk)
  })
  const closed = new AbortController()
  client.onclose = () => {
    closed.abort(new Error(`the channel server's MCP transport closed; its stderr: ${stderr}`))
  }
  const untilServed = (what: string, probe: () => boolean | Promise<boolean>): Promise<void> =>
    until(what, () => {
      closed.signal.throwIfAborted()
      return probe()
    }).then(
      () => undefined,
      (error: unknown) => {
        if (error === closed.signal.reason) throw error
        throw new Error(`${String(error)}; its stderr: ${stderr}`, { cause: error })
      },
    )
  const untilStarted = (): Promise<void> =>
    untilServed("the channel session to start", () =>
      client
        .callTool({ name: "envoy_whoami", arguments: {} }, undefined, { timeout: timeLeft() })
        .then(
          () => true,
          (error: unknown) => {
            // A call the close rejected fails with the close's own reason.
            closed.signal.throwIfAborted()
            if (String(error).includes("still starting")) return false
            throw error
          },
        ),
    )
  return { stderr: () => stderr, closed: closed.signal, untilServed, untilStarted }
}

// The rows below start each session in a home of its own and reach it at its mod socket; a home
// under a deep TMPDIR named no socket, and two of them failed (the review of #4140's forward merge).
test("a session home's mod socket fits however deep TMPDIR is", async () => {
  const was = process.env["TMPDIR"]
  const deep = await mkdtemp(join("/tmp", `claude-envoy-depth-${"d".repeat(64)}-`))
  await mkdir(deep, { recursive: true })
  process.env["TMPDIR"] = deep
  let home: string | undefined
  try {
    expect(tmpdir()).toBe(deep)
    home = await sessionHome("claude-envoy-mcp-")
    expect(modSocketPath("ses_depth", { XDG_RUNTIME_DIR: home })).toBeDefined()
  } finally {
    if (was === undefined) delete process.env["TMPDIR"]
    else process.env["TMPDIR"] = was
    if (home !== undefined) await rm(home, { recursive: true, force: true })
    await rm(deep, { recursive: true, force: true })
  }
})

// Each row below starts a channel server of its own, with Bun's transpiler cache off
// (`./transpiler-cache.ts`), and waits for what that server does: its answer to `initialize`
// (`connectedBeforeTheDeadline`, or `connectedToServer` for the dial row), each later step and each
// MCP request, until the row's deadline (`@legion/test-fixtures/wait`), or 5 s inside the dial
// row's own 600 s (`untilTheGuard`, `./row-deadline.ts`), so a step that never comes fails naming
// itself before Bun's line would. The session-home row starts no server. The first two rows had 5 s
// waits and a 20 s limit, and at loads of 720 to 1,150 the first row's server, writing that cache
// into its empty HOME, had not started within them; the dial row's `initialize` had the MCP SDK's
// 60 s inside its 600 s, and at a load of 1,200 to 1,400 it failed there at 99 s
// (dispatch://INFRA-3904).

// What Claude Code sees over MCP, with Dispatch configured: the Envoy tools and nothing else, since
// agents reach Dispatch through the `dispatch` command the plugin's bin/ puts on the Bash tool's
// PATH. A terminal session reaches the model only through its mod (INFRA-1026), so it advertises
// no delivery mode until the mod connects, and Aside, BTW and Send once one has.
test("over MCP the server offers only the Envoy tools, refuses dispatch_ask, points at the dispatch command, and advertises nothing until its mod connects", async () => {
  const ended = rowEnd.signal
  const broker = new FakeNatsServer()
  const dispatchRequests: string[] = []
  const dispatch = Bun.serve({
    port: 0,
    fetch: (request) => {
      dispatchRequests.push(`${request.method} ${new URL(request.url).pathname}`)
      return Response.json({ error: "unexpected" }, { status: 500 })
    },
  })
  const advertised: unknown[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as {
          session_id: string
          topics: string[]
          capabilities?: unknown
        }
        advertised.push(body.capabilities)
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
          // Every stand-in listener answers the session's own consumer
          // (`@legion/envoy-client/test/subscribe-answer`).
          delivery: consumerDelivery(body.session_id),
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  const stateDirectory = await sessionHome("claude-envoy-mcp-")
  const dispatchToken = fixtureSecret("dsp")
  const client = new Client({ name: "test", version: "0" })
  const transport = new StdioClientTransport({
    command: "bun",
    args: [entry],
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: "ses_mcp",
      CLAUDE_PLUGIN_DATA: stateDirectory,
      // The socket the server binds for its mod, and the one the fake mod below connects to,
      // are both resolved from this base (`resolveModSocketPath`).
      XDG_RUNTIME_DIR: stateDirectory,
      DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
      DISPATCH_TOKEN: dispatchToken,
      ...NO_TRANSPILER_CACHE,
    },
    stderr: "pipe",
  })
  const { stderr, closed, untilServed, untilStarted } = observeServer(client, transport)

  try {
    await connectedBeforeTheDeadline(client, transport, stderr)
    await untilStarted()

    const { tools } = await client.listTools(undefined, { timeout: timeLeft() })
    expect(tools.map(({ name }) => name)).toEqual(envoyToolSpecs.map(({ name }) => name))
    // Nothing until the mod connects: without one no message reaches the model at all.
    expect(advertised.length).toBeGreaterThan(0)
    for (const modes of advertised) expect(modes).toEqual([])

    const socketPath = modSocketPath("ses_mcp", { XDG_RUNTIME_DIR: stateDirectory })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    const mod = await startFakeMod(socketPath, {
      sessionId: "ses_mcp",
      signal: AbortSignal.any([ended, closed]),
    })
    try {
      // The mod's hello makes the session register again, with what it can take now.
      await untilServed(
        "the session to advertise its delivery modes",
        () => (advertised.at(-1) as string[] | undefined)?.length === 3,
      )
      expect(advertised.at(-1)).toEqual(["aside", "btw", "steer"])
    } finally {
      await mod.stop()
    }
    const instructions = client.getInstructions() ?? ""
    expect(instructions).toContain("`dispatch` command")
    expect(instructions).not.toContain("Dispatch tools")

    await expect(
      client.callTool(
        { name: "dispatch_ask", arguments: { issue: "DSP-3", question: "Ship?" } },
        undefined,
        { timeout: timeLeft() },
      ),
    ).rejects.toThrow("Unsupported Envoy tool: dispatch_ask")
    expect(dispatchRequests).toEqual([])
  } finally {
    await client.close().catch(() => undefined)
    dispatch.stop(true)
    listener.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

// A mod says hello once per Claude Code process, so a server Claude Code starts again inside
// that process hears none, and neither does a listener added after the hello landed: the session
// then advertised nothing until the next heartbeat, up to 90 s later, and Dispatch showed it as
// taking no messages (INFRA-1026 review). A poll says a mod is there just as well.
test("the session advertises its delivery modes for a mod that polls and never says hello", async () => {
  const ended = rowEnd.signal
  const broker = new FakeNatsServer()
  const advertised: unknown[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as {
          session_id: string
          topics: string[]
          capabilities?: unknown
        }
        advertised.push(body.capabilities)
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
          delivery: consumerDelivery(body.session_id),
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  const stateDirectory = await sessionHome("claude-envoy-quiet-")
  const client = new Client({ name: "test", version: "0" })
  const transport = new StdioClientTransport({
    command: "bun",
    args: [entry],
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: "ses_quiet",
      CLAUDE_PLUGIN_DATA: stateDirectory,
      XDG_RUNTIME_DIR: stateDirectory,
      ...NO_TRANSPILER_CACHE,
    },
    stderr: "pipe",
  })
  const { stderr, closed, untilServed, untilStarted } = observeServer(client, transport)

  try {
    await connectedBeforeTheDeadline(client, transport, stderr)
    await untilStarted()
    expect(advertised.at(-1)).toEqual([])

    const socketPath = modSocketPath("ses_quiet", { XDG_RUNTIME_DIR: stateDirectory })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    await untilServed("the server to bind its mod socket", () =>
      stat(socketPath).then(
        () => true,
        () => false,
      ),
    )
    const mod = await startFakeMod(socketPath, {
      sessionId: "ses_quiet",
      hello: false,
      signal: AbortSignal.any([ended, closed]),
    })
    try {
      await untilServed(
        "the session to advertise its delivery modes",
        () => (advertised.at(-1) as string[] | undefined)?.length === 3,
      )
      expect(advertised.at(-1)).toEqual(["aside", "btw", "steer"])
    } finally {
      await mod.stop()
    }
  } finally {
    await client.close().catch(() => undefined)
    listener.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

// Under NATS_AUTH=broker the start waits for the session's token for as long as the server lives,
// since Claude Code does not start a channel server again: a tool call meanwhile says what the start
// waits on, where it said only that the server was starting (the review of 2fa6fbf7cf, m1).
test("a tool call while the start waits for its broker token says what the start waits on", async () => {
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      await request.text()
      return new Response("the broker is away", { status: 503 })
    },
  })
  const stateDirectory = await sessionHome("claude-envoy-broker-wait-")
  const tokenFile = join(stateDirectory, "projected-token")
  await writeFile(tokenFile, `${fixtureSecret("proj")}\n`)
  const client = new Client({ name: "test", version: "0" })
  const transport = new StdioClientTransport({
    command: "bun",
    args: [entry],
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: "nats://127.0.0.1:1",
      ENVOY_URL: "http://127.0.0.1:1",
      CLAUDE_CODE_SESSION_ID: "ses_broker_wait",
      CLAUDE_PLUGIN_DATA: stateDirectory,
      XDG_RUNTIME_DIR: stateDirectory,
      NATS_AUTH: "broker",
      AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
      ...NO_TRANSPILER_CACHE,
    },
    stderr: "pipe",
  })
  // The start has reached its wait for the token once its stderr says what the first trade came
  // to: the token client writes that line at the wait after the trade, and the start names its
  // wait before it trades. A start that never gets there fails at the row's deadline (`until`),
  // naming the wait.
  const { stderr, untilServed } = observeServer(client, transport)
  try {
    await connectedBeforeTheDeadline(client, transport, stderr)
    await untilServed("the start's wait for the token", () => stderr().includes("trading again"))
    const said = await client
      .callTool({ name: "envoy_whoami", arguments: {} }, undefined, { timeout: timeLeft() })
      .then(
        () => {
          throw new Error("the session started though its broker answers every trade 503")
        },
        (error: unknown) => String(error),
      )
    expect(said).toContain(
      "Envoy channel server is still starting: it waits for this session's broker token, and its stderr says what each trade came to",
    )
  } finally {
    await client.close().catch(() => undefined)
    broker.stop(true)
    await rm(stateDirectory, { recursive: true, force: true })
  }
})

// The channel server dials NATS again for its life, and an Envoy tool called meanwhile is told
// why the start is still waiting, which until then only reached stderr. Once the connection is up
// that reason is gone: what the start waits on next (here, the listener) is not the dial.
test(
  "an Envoy tool called while the start dials NATS again is told the dial's last failure, and not once the connection is up",
  async () => {
    const broker = new FakeNatsServer()
    const gate = natsGate(broker.url)
    const held = Promise.withResolvers<void>()
    const listener = Bun.serve({
      port: 0,
      fetch: async (request) => {
        const url = new URL(request.url)
        if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
          const body = (await request.json()) as { session_id: string; topics: string[] }
          await held.promise
          return Response.json({
            session_id: body.session_id,
            machine_id: "test",
            dir: "/tmp",
            topics: body.topics,
            delivery: consumerDelivery(body.session_id),
          })
        }
        if (request.method === "DELETE") return new Response(null, { status: 200 })
        return Response.json({ error: "not found" }, { status: 404 })
      },
    })
    const stateDirectory = await sessionHome("claude-envoy-dial-")
    const client = new Client({ name: "test", version: "0" })
    const transport = new StdioClientTransport({
      command: "bun",
      args: [entry],
      cwd: stateDirectory,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: stateDirectory,
        ENVOY_NATS_URL: gate.url,
        ENVOY_URL: `http://127.0.0.1:${listener.port}`,
        CLAUDE_CODE_SESSION_ID: "ses_dialing",
        CLAUDE_PLUGIN_DATA: stateDirectory,
        XDG_RUNTIME_DIR: stateDirectory,
        ...NO_TRANSPILER_CACHE,
      },
      stderr: "pipe",
    })
    let stderr = ""
    transport.stderr?.on("data", (chunk: Buffer | string) => {
      stderr += String(chunk)
    })
    // Each request waits for its answer until the row's named deadline, not the MCP SDK's 60 s.
    const whoami = () =>
      client
        .callTool({ name: "envoy_whoami", arguments: {} }, undefined, {
          timeout: untilTheGuard(ROW_TIMEOUT_MS),
        })
        .then(
          () => "answered",
          (error: unknown) => String(error),
        )
    let answer = ""
    const state = () => `the tool last said: ${answer}; the server's stderr: ${stderr}`

    try {
      await connectedToServer(client, transport, () => stderr, ROW_TIMEOUT_MS)
      await waitFor(
        async () => {
          answer = await whoami()
          return answer.includes("dialing again in")
        },
        "the tool to name the dial's last failure",
        state,
        untilTheGuard(ROW_TIMEOUT_MS),
      )
      expect(answer).toContain("Envoy channel server is still starting: could not connect to NATS")

      gate.open()
      await waitFor(
        async () => broker.subscribed.length > 0,
        "the session's subscriptions to reach NATS",
        state,
        untilTheGuard(ROW_TIMEOUT_MS),
      )
      // Connected, and still starting: the listener holds the registration.
      expect(await whoami()).toContain("Envoy channel server is still starting")
      expect(await whoami()).not.toContain("could not connect")

      held.resolve()
      await waitFor(
        async () => {
          answer = await whoami()
          return answer === "answered"
        },
        "the channel session to start",
        state,
        untilTheGuard(ROW_TIMEOUT_MS),
      )
    } finally {
      held.resolve()
      await client.close().catch(() => undefined)
      listener.stop(true)
      gate.stop()
      await broker.stop()
      await rm(stateDirectory, { recursive: true, force: true })
    }
  },
  ROW_TIMEOUT_MS,
)

// The dial's warnings name what the start waits on while it dials; the broker token's keeper warns
// through the same channel once the connection is up, and a keeper warning is no reason the start
// is waiting. Here the first token lives two seconds and every later trade is refused, so the
// keeper warns about a second after the dial, while the listener holds the start's registration:
// a tool call then is told the start registers, not the keeper's warning.
test("a broker token keeper's warning while the start registers leaves the tool told what the start waits on", async () => {
  let trades = 0
  const tokens = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      await request.text()
      trades += 1
      if (trades > 1) return new Response("the broker is away", { status: 503 })
      return Response.json({
        access_token: fixtureSecret("channel"),
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 2,
      })
    },
  })
  const broker = new FakeNatsServer({ nonce: "fake-nonce" })
  const held = Promise.withResolvers<void>()
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as { session_id: string; topics: string[] }
        await held.promise
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
          delivery: consumerDelivery(body.session_id),
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  const stateDirectory = await sessionHome("claude-envoy-keeper-")
  const tokenFile = join(stateDirectory, "projected-token")
  await writeFile(tokenFile, `${fixtureSecret("proj")}\n`)
  const client = new Client({ name: "test", version: "0" })
  const transport = new StdioClientTransport({
    command: "bun",
    args: [entry],
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: "ses_keeper_warns",
      CLAUDE_PLUGIN_DATA: stateDirectory,
      XDG_RUNTIME_DIR: stateDirectory,
      NATS_AUTH: "broker",
      AGENT_SECRETS_URL: `http://127.0.0.1:${tokens.port}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
      ...NO_TRANSPILER_CACHE,
    },
    stderr: "pipe",
  })
  const { stderr, untilServed } = observeServer(client, transport)

  try {
    await connectedBeforeTheDeadline(client, transport, stderr)
    // Connected, and the start's registration held: the keeper warns about the lapsing token.
    await untilServed(
      "the session's subscriptions to reach NATS",
      () => broker.subscribed.length > 0,
    )
    await untilServed("the keeper's warning", () =>
      stderr().includes("envoy-channel: the NATS connection's broker token"),
    )
    const answer = await client
      .callTool({ name: "envoy_whoami", arguments: {} }, undefined, { timeout: timeLeft() })
      .then(
        () => "answered",
        (error: unknown) => String(error),
      )
    expect(answer).toContain(
      "Envoy channel server is still starting: it registers this session with Envoy",
    )
    expect(answer).not.toContain("broker token")
  } finally {
    held.resolve()
    await client.close().catch(() => undefined)
    listener.stop(true)
    tokens.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
