import { expect, test } from "bun:test"
import { mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { modSocketPath } from "../hooks/mod-protocol"
import { waitFor } from "./channel-session-harness"
import { startFakeMod } from "./fake-mod"
import { FakeNatsServer } from "./fake-nats-server"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

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

// What Claude Code sees over MCP, with Dispatch configured: the Envoy tools and nothing else, since
// agents reach Dispatch through the `dispatch` command the plugin's bin/ puts on the Bash tool's
// PATH. A terminal session reaches the model only through its mod (INFRA-1026), so it advertises
// no delivery mode until the mod connects, and Aside, BTW and Send once one has.
test("over MCP the server offers only the Envoy tools, refuses dispatch_ask, points at the dispatch command, and advertises nothing until its mod connects", async () => {
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
    },
    stderr: "pipe",
  })

  try {
    await client.connect(transport)
    // The server answers tool calls only once its channel session has started.
    await waitFor(
      () =>
        client.callTool({ name: "envoy_whoami", arguments: {} }).then(
          () => true,
          (error: unknown) => {
            if (String(error).includes("still starting")) return false
            throw error
          },
        ),
      "the channel session to start",
    )

    const { tools } = await client.listTools()
    expect(tools.map(({ name }) => name)).toEqual(envoyToolSpecs.map(({ name }) => name))
    // Nothing until the mod connects: without one no message reaches the model at all.
    expect(advertised.length).toBeGreaterThan(0)
    for (const modes of advertised) expect(modes).toEqual([])

    const socketPath = modSocketPath("ses_mcp", { XDG_RUNTIME_DIR: stateDirectory })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    const mod = await startFakeMod(socketPath, { sessionId: "ses_mcp" })
    try {
      // The mod's hello makes the session register again, with what it can take now.
      await waitFor(
        async () => (advertised.at(-1) as string[] | undefined)?.length === 3,
        "the session to advertise its delivery modes",
        () => JSON.stringify(advertised),
      )
      expect(advertised.at(-1)).toEqual(["aside", "btw", "steer"])
    } finally {
      await mod.stop()
    }
    const instructions = client.getInstructions() ?? ""
    expect(instructions).toContain("`dispatch` command")
    expect(instructions).not.toContain("Dispatch tools")

    await expect(
      client.callTool({ name: "dispatch_ask", arguments: { issue: "DSP-3", question: "Ship?" } }),
    ).rejects.toThrow("Unsupported Envoy tool: dispatch_ask")
    expect(dispatchRequests).toEqual([])
  } finally {
    await client.close().catch(() => undefined)
    dispatch.stop(true)
    listener.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
}, 20_000)

// A mod says hello once per Claude Code process, so a server Claude Code starts again inside
// that process hears none, and neither does a listener added after the hello landed: the session
// then advertised nothing until the next heartbeat, up to 90 s later, and Dispatch showed it as
// taking no messages (INFRA-1026 review). A poll says a mod is there just as well.
test("the session advertises its delivery modes for a mod that polls and never says hello", async () => {
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
    },
    stderr: "pipe",
  })

  try {
    await client.connect(transport)
    await waitFor(
      () =>
        client.callTool({ name: "envoy_whoami", arguments: {} }).then(
          () => true,
          (error: unknown) => {
            if (String(error).includes("still starting")) return false
            throw error
          },
        ),
      "the channel session to start",
    )
    expect(advertised.at(-1)).toEqual([])

    const socketPath = modSocketPath("ses_quiet", { XDG_RUNTIME_DIR: stateDirectory })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    await waitFor(
      () =>
        stat(socketPath).then(
          () => true,
          () => false,
        ),
      "the server to bind its mod socket",
    )
    const mod = await startFakeMod(socketPath, { sessionId: "ses_quiet", hello: false })
    try {
      await waitFor(
        async () => (advertised.at(-1) as string[] | undefined)?.length === 3,
        "the session to advertise its delivery modes",
        () => JSON.stringify(advertised),
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
}, 20_000)

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
    },
    stderr: "pipe",
  })
  // The start has reached its wait for the token once its stderr says what the first trade came
  // to: the token client writes that line at the wait after the trade, and the start names its
  // wait before it trades. A start that never gets there in 45 s, a minute's row on a loaded
  // machine, is named with what its stderr said.
  let stderr = ""
  const waiting = Promise.withResolvers<void>()
  transport.stderr?.on("data", (chunk: Buffer | string) => {
    stderr += String(chunk)
    if (stderr.includes("trading again")) waiting.resolve()
  })
  const bound = setTimeout(
    () =>
      waiting.reject(
        new Error(
          `the start did not reach its wait for the token within 45 s; its stderr said: ${stderr.slice(-2_000)}`,
        ),
      ),
    45_000,
  )
  // Settled here too, so a bound that fires while the connect is still out is no unhandled
  // rejection; the row reads it where it awaits the wait.
  waiting.promise.catch(() => undefined)
  try {
    await client.connect(transport)
    await waiting.promise
    const said = await client.callTool({ name: "envoy_whoami", arguments: {} }).then(
      () => {
        throw new Error("the session started though its broker answers every trade 503")
      },
      (error: unknown) => String(error),
    )
    expect(said).toContain(
      "Envoy channel server is still starting: it waits for this session's broker token, and its stderr says what each trade came to",
    )
  } finally {
    clearTimeout(bound)
    await client.close().catch(() => undefined)
    broker.stop(true)
    await rm(stateDirectory, { recursive: true, force: true })
  }
}, 60_000)
