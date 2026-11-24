import { expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { createServer, type Socket } from "node:net"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import { envoyToolSpecs } from "@legion/envoy-client/tool-contract"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { loopbackHost } from "@legion/test-fixtures/loopback-host"
import { readStandinBody } from "@legion/test-fixtures/standin-body"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { legion } from "../package.json" with { type: "json" }
import { PHASE_HOOK_TOOL_NAME } from "../src/legion-phase-hook"
import { waitFor } from "./channel-session-harness"
import { FakeNatsServer } from "./fake-nats-server"
import { PROCESS_WAIT_MS, ROW_TIMEOUT_MS } from "./reopen-server"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")
const SESSION = "6f1c2d3e-4a5b-4c6d-8e7f-9a0b1c2d3e4f"
/** What the row keeps of its timeout for its `finally`: closing the client and the stand-ins. */
const ROW_CLEANUP_MS = 10_000

async function daemonFixture(name: string): Promise<Record<string, unknown>> {
  const file = resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)
  return JSON.parse(await readFile(file, "utf8"))
}

// A Legion pane's MCP server as Claude Code runs it under `legion claude-bridge`: a boot token,
// the daemon's address, its own CLAUDE_CONFIG_DIR and the bridge's inbox socket on its
// environment. It registers once the transcript exists, claims the claim token's role, says
// ready, offers the `legion` tool beside the Envoy tools, advertises the Send (steer) the bridge
// serves, and hands an Envoy delivery to the inbox.
test(
  "a Legion pane's server registers on its transcript, claims its role, serves the legion tool and delivers through the inbox",
  async () => {
    const root = await mkdtemp(join(tmpdir(), "claude-envoy-legion-pane-"))
    const configDir = join(root, "claude")
    const registered = await daemonFixture("register.json")
    const claimToken = String(registered["claimToken"])
    const daemonCalls: { path: string; body: unknown }[] = []
    // Each stand-in listens at an address of its own, out of reach on Linux of a client of another
    // test that still dials its port at 127.0.0.1, and refuses by name a POST body that is no JSON
    // object before it records the request (`loopbackHost`, `readStandinBody`).
    const daemon = Bun.serve({
      port: 0,
      hostname: loopbackHost(),
      fetch: async (request) => {
        const path = new URL(request.url).pathname
        const { body, refusal } = await readStandinBody(request)
        if (refusal !== undefined) return refusal
        daemonCalls.push({ path, body: request.method === "GET" ? undefined : body })
        if (path === "/legion/v1/claims/register") return Response.json(registered)
        if (path === "/legion/v1/claims/ready") return new Response(null, { status: 204 })
        if (path === "/legion/v1/state") return Response.json(await daemonFixture("state.json"))
        // `read_record` and the reminder's record check read one issue's record
        // (dispatch://INFRA-2694).
        if (path === "/legion/v1/issues/LEGION-208/record") {
          const state = (await daemonFixture("state.json")) as { issues: Record<string, unknown> }
          return Response.json(state.issues["LEGION-208"])
        }
        return Response.json({ error: "unexpected" }, { status: 500 })
      },
    })
    const roles: unknown[] = []
    const advertised: unknown[] = []
    /** The client instance each registration carried: the process the pane's claim must name. */
    const instances: Array<string | undefined> = []
    const holders = new Map<string, string>()
    const listener = Bun.serve({
      port: 0,
      hostname: loopbackHost(),
      fetch: async (request) => {
        const url = new URL(request.url)
        const interest = { session_id: SESSION, machine_id: "test", dir: root, topics: [] }
        if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
          const read = await readStandinBody(request)
          if (read.refusal !== undefined) return read.refusal
          const registration = read.body as {
            readonly capabilities?: unknown
            readonly client_instance_id?: string
          }
          advertised.push(registration.capabilities)
          instances.push(registration.client_instance_id)
          // Every accepted registration is answered with the session's own consumer.
          return Response.json({
            ...interest,
            delivery: consumerDelivery(SESSION),
          })
        }
        if (request.method === "POST" && url.pathname === "/v1/roles/set") {
          const read = await readStandinBody(request)
          if (read.refusal !== undefined) return read.refusal
          const claim = read.body as { session_id: string; role: string }
          roles.push(claim)
          holders.set(claim.role, claim.session_id)
          return Response.json(interest)
        }
        const role = /^\/v1\/roles\/(.+)$/.exec(url.pathname)?.[1]
        const holder = role === undefined ? undefined : holders.get(decodeURIComponent(role))
        if (request.method === "GET" && role !== undefined && holder !== undefined) {
          return Response.json({ role: decodeURIComponent(role), holder, last_seen: 1 })
        }
        if (request.method === "DELETE") return new Response(null, { status: 200 })
        return Response.json({ error: "not found" }, { status: 404 })
      },
    })
    const broker = new FakeNatsServer()
    const inboxLines: { id: string; content: string }[] = []
    const inboxPath = join(root, "inbox.sock")
    const inbox = createServer((socket: Socket) => {
      let buffered = ""
      socket.setEncoding("utf8")
      socket.on("data", (chunk: string) => {
        buffered += chunk
        for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
          const line = JSON.parse(buffered.slice(0, end)) as { id: string; content: string }
          buffered = buffered.slice(end + 1)
          inboxLines.push(line)
          socket.write(`${JSON.stringify({ id: line.id, ok: true })}\n`)
        }
      })
    })
    await new Promise<void>((listening) => inbox.listen(inboxPath, listening))
    await mkdir(configDir, { recursive: true })
    const tokenFile = join(root, "boot-token")
    const bootToken = fixtureSecret("boot")
    await writeFile(tokenFile, `${bootToken}\n`)

    const client = new Client({ name: "test", version: "0" })
    const transport = new StdioClientTransport({
      command: "bun",
      args: [entry],
      cwd: root,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: root,
        ENVOY_NATS_URL: broker.url,
        ENVOY_URL: listener.url.origin,
        CLAUDE_CODE_SESSION_ID: SESSION,
        CLAUDE_CONFIG_DIR: configDir,
        CLAUDE_PLUGIN_DATA: join(root, "plugin-data"),
        LEGION_BOOT_TOKEN_FILE: tokenFile,
        LEGION_DAEMON_URL: daemon.url.origin,
        LEGION_CLAUDE_INBOX: inboxPath,
        // Ten minutes, so the role below can only have been claimed by the turn this case
        // submits and never by the claim's own bound (INFRA-1573).
        LEGION_CLAIM_FIRST_TURN_BOUND_MS: "600000",
      },
      stderr: "pipe",
    })
    // A whole server process, which a loaded machine starts and runs slowly: each wait below ends
    // on its fact, or on the server's close, which no later fact can follow, and names what it waited
    // for with what the stand-ins saw; each request to the server takes the same guard (`request`)
    // in place of the SDK's 60 s. A wait's guard is `PROCESS_WAIT_MS`, cut to what is left of the
    // row's own deadline (`ROW_TIMEOUT_MS`, less `ROW_CLEANUP_MS` for the `finally`), so the row's
    // ten waits, at 120 s each, can never run past the runner's timeout: a stuck one names its step
    // first, however long the waits before it took. A fixed 20 s for the whole row decided it at a
    // load of about 2,400, with every fact still to come.
    const rowDeadline = performance.now() + ROW_TIMEOUT_MS - ROW_CLEANUP_MS
    const guard = (): number =>
      Math.max(1, Math.min(PROCESS_WAIT_MS, Math.floor(rowDeadline - performance.now())))
    const request = (): { timeout: number } => ({ timeout: guard() })
    let closed = false
    client.onclose = () => {
      closed = true
    }
    let stderr = ""
    transport.stderr?.on("data", (chunk: Buffer) => {
      stderr += chunk.toString()
    })
    const observed = (): string =>
      `server closed ${closed}; daemon calls ${daemonCalls.map(({ path }) => path).join(", ") || "none"}; roles ${roles.length}; inbox lines ${inboxLines.length}; stderr:\n${stderr}`
    const until = async (fact: () => boolean, what: string): Promise<void> => {
      await waitFor(async () => fact() || closed, what, observed, guard())
      if (!fact()) throw new Error(`the server closed before ${what} (${observed()})`)
    }

    try {
      await client.connect(transport, request())
      const { tools } = await client.listTools(undefined, request())
      expect(tools.map(({ name }) => name)).toEqual([
        ...envoyToolSpecs.map(({ name }) => name),
        "legion",
        "legion_phase_hook",
      ])
      await expect(
        client.callTool(
          { name: "legion", arguments: { op: "read_record", issue: "LEGION-208" } },
          undefined,
          request(),
        ),
      ).rejects.toThrow("this session has no Legion claim yet")

      // The bridge's /rename seed, as a fresh pane's Claude Code submits it: a prompt before any
      // claim exists, since the seed is what writes the transcript the pane registers with. It
      // says a turn started, and the server has no step to run for it yet (INFRA-1573).
      const seed = await client.callTool(
        {
          name: PHASE_HOOK_TOOL_NAME,
          arguments: { event: "UserPromptSubmit", prompt: "/rename legion" },
        },
        undefined,
        request(),
      )
      expect(seed.content).toEqual([{ type: "text", text: "{}" }])
      const transcript = join(configDir, "projects", "-work", `${SESSION}.jsonl`)
      await mkdir(join(configDir, "projects", "-work"), { recursive: true })
      await writeFile(transcript, "{}\n")
      await until(
        () => daemonCalls.some(({ path }) => path === "/legion/v1/claims/ready"),
        "the pane to say it is ready",
      )
      expect(daemonCalls).toEqual([
        {
          path: "/legion/v1/claims/register",
          body: {
            bootToken,
            sessionId: SESSION,
            ompSessionFile: transcript,
            agentId: SESSION,
            pluginContract: legion.daemonApiVersion,
          },
        },
        {
          path: "/legion/v1/claims/ready",
          body: {
            claimToken,
            sessionId: SESSION,
            secret: registered["secret"],
            generation: registered["generation"],
          },
        },
      ])
      // The seed is behind the pane and it holds no name: the registration and its ready have
      // run, and only a turn after them takes the Envoy name, so a message the listener kept for
      // the role is handed over inside a running turn rather than ahead of the daemon's task
      // (INFRA-1573).
      expect(roles).toEqual([])
      const turn = await client.callTool(
        {
          name: PHASE_HOOK_TOOL_NAME,
          arguments: { event: "UserPromptSubmit", prompt: "Start implementing" },
        },
        undefined,
        request(),
      )
      expect(turn.content).toEqual([{ type: "text", text: "{}" }])
      await until(() => roles.length > 0, "the pane to claim its role at its first turn")
      // The claim names the process that made it — the one id this server minted and registers
      // with — so the listener holds the role for this `claude` alone (dispatch://INFRA-783).
      const instance = instances[0]
      expect(instance).toMatch(/^[0-9a-f]{32}$/)
      expect([...new Set(instances)]).toEqual([instance])
      expect(roles).toEqual([
        { session_id: SESSION, role: claimToken, soft: true, client_instance_id: instance },
      ])
      // Dispatch refuses a mode the session does not advertise, and sends a Send by default: every
      // registration of the session carries both modes the bridge serves.
      expect(advertised.length).toBeGreaterThan(0)
      for (const modes of advertised) expect(modes).toEqual(["aside", "steer"])

      const record = await client.callTool(
        { name: "legion", arguments: { op: "read_record", issue: "LEGION-208" } },
        undefined,
        request(),
      )
      const state = (await daemonFixture("state.json")) as { issues: Record<string, unknown> }
      expect(record.content).toEqual([
        { type: "text", text: JSON.stringify({ record: state.issues["LEGION-208"] }) },
      ])
      // A read that leaves `issue` out answers the claim's own issue, LEGION-208 here.
      const own = await client.callTool(
        { name: "legion", arguments: { op: "read_record" } },
        undefined,
        request(),
      )
      expect(own.content).toEqual(record.content)

      // Nothing this pane's server asks the daemon is the whole project body: `read_record` and
      // the phase reminder's own check both read one issue's record (dispatch://INFRA-2694).
      expect(daemonCalls.map(({ path }) => path)).toContain("/legion/v1/issues/LEGION-208/record")
      expect(daemonCalls.map(({ path }) => path)).not.toContain("/legion/v1/state")

      // The session receives only what its own consumer holds, which the listener relays to it.
      broker.deliverRelay(SESSION, {
        subject: `notifications.agent.${SESSION}`,
        seq: 1,
        reply: "_INBOX.legion-1",
        payload: JSON.stringify({
          event_id: "evt-legion-1",
          dedupe_key: "legion-1",
          source: "agent",
          source_session: "ses_architect",
          topic: `notifications.agent.${SESSION}`,
          issued_at: 1_760_000_000_000,
          payload_summary: "Start implementing",
        }),
      })
      await until(() => inboxLines.length > 0, "the delivery to reach the inbox")
      expect(inboxLines[0]?.id).toBe("evt-legion-1")
      expect(inboxLines[0]?.content).toContain("Start implementing")
    } finally {
      await client.close().catch(() => undefined)
      inbox.close()
      daemon.stop(true)
      listener.stop(true)
      await broker.stop()
      await rm(root, { recursive: true, force: true })
    }
  },
  ROW_TIMEOUT_MS,
)
