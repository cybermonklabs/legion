import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { type Server as ControlBridge, createServer } from "node:http"
import { connect as connectSocket } from "node:net"
import { basename, dirname, join, resolve } from "node:path"
import { EnvelopeSchema, legacyAgentSubject } from "@legion/contracts"
import { createConsumerDelivery } from "@legion/envoy-client/consumer-delivery"
import { createDeliveryLedger } from "@legion/envoy-client/delivery-ledger"
import {
  type EnvoyStack,
  startEnvoyStack,
  useEnvoyStackDirectory,
} from "@legion/envoy-client/test/envoy-stack"
import { createEnvoyClient } from "@legion/envoy-client/transport"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import type { Server, Subprocess } from "bun"
import { connect, type NatsConnection, StringCodec } from "nats"
import {
  type DispatchStack,
  startDispatchStack,
  useDispatchStackDirectory,
} from "../../envoy-client/test/dispatch-stack"
import { codexServersDirectory, currentOwner } from "../src/codex/codex-owner"
import {
  codexSessionRecordFile,
  readCodexSessionRecord,
  sharedCodexRecordFile,
} from "../src/codex/codex-session-record"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

const {
  LEGION_TEST_CODEX_SHARED_CASE: activeCase,
  LEGION_TEST_CODEX_SHARED_BINARY: codex,
  LEGION_TEST_CODEX_SHARED_OLD_BINARY: oldCodex,
  LEGION_TEST_CODEX_SHARED_SOURCE: source,
  LEGION_TEST_CODEX_SHARED_NO_TRUST: noTrust,
  DISPATCH_TEST_DATABASE_URL: database,
  PATH = "",
} = process.env
const packageRoot = source ?? resolve(import.meta.dir, "..")
const work = temporaryDirectory("codex-shared")
const envoyWork = useEnvoyStackDirectory()
const dispatchWork = useDispatchStackDirectory()
const enabled = activeCase !== undefined && codex !== undefined && database !== undefined
const bound = 10_000
const setupBound = 480_000
const peer = `codex-shared-peer-${process.pid}`

type Item = Record<string, unknown>
interface ModelRequest {
  readonly number: number
  readonly at: number
  readonly thread: string | null
  readonly turn: string | null
  readonly input: readonly Item[]
  readonly whole: string
}
interface ModelPlan {
  readonly tool?: string
  readonly args?: Item
  readonly holdMs?: number
  readonly waitForRegistration?: boolean
}
interface ServerStart {
  readonly pid: number
  readonly parent: number
  readonly standby: boolean
}
interface SessionRow {
  readonly session_id: string
  readonly title: string
  readonly plugin?: string
}

const requests: ModelRequest[] = []
const plans = new Map<string, ModelPlan>()
const received: { message: string; source: string | undefined }[] = []
const children = new Set<Subprocess>()
const homes: Home[] = []
const controls = new Map<Home, { server: ControlBridge; url: string }>()
const sockets = new Set<WebSocket>()
let model: Server<undefined> | undefined
let envoy: EnvoyStack | undefined
let dispatch: DispatchStack | undefined
let bus: NatsConnection | undefined
let client: ReturnType<typeof createEnvoyClient>
let market = ""
const launchLog = join(work, "mcp.jsonl")
const serverLog = join(work, "mcp.stderr")
const hookLog = join(work, "hooks.jsonl")
const hookErrors = join(work, "hook.stderr")

async function until<T>(
  what: string,
  deadline: number,
  read: () => T | undefined | Promise<T | undefined>,
): Promise<T> {
  while (true) {
    const found = await read()
    if (found !== undefined) return found
    if (Date.now() >= deadline) throw new Error(`timed out waiting for ${what}`)
    await Bun.sleep(75)
  }
}

const elapsed = (start: number, finish = Date.now()) => ((finish - start) / 1000).toFixed(2)
const marker = (label: string) => `codex-shared-${label}-${crypto.randomUUID()}`

function listTools(items: unknown, namespace?: string): { name: string; namespace?: string }[] {
  if (!Array.isArray(items)) return []
  return (items as Item[]).flatMap((item) => {
    if (typeof item["name"] !== "string") return []
    if (item["type"] === "namespace") return listTools(item["tools"], item["name"])
    const prefix = item["namespace"] ?? namespace
    return [{ name: item["name"], ...(typeof prefix === "string" ? { namespace: prefix } : {}) }]
  })
}

function modelAnswer(input: readonly Item[], tools: unknown, number: number): Item {
  const userInputs = input.filter((item) => item["role"] === "user")
  const latest = JSON.stringify(userInputs.at(-1)?.["content"] ?? "")
  const active = [...plans].find(([key]) => latest.includes(key))
  const plan = active?.[1]
  const step = active?.[0]
  if (plan?.tool !== undefined && step !== undefined) {
    const callID = `call_${step}`
    const searchID = `search_${step}`
    if (!input.some((item) => item["call_id"] === callID)) {
      const tool = [
        ...listTools(tools),
        ...input.flatMap((item) =>
          item["type"] === "tool_search_output" ? listTools(item["tools"]) : [],
        ),
      ].find(({ name }) => name === plan.tool || name.endsWith(`__${plan.tool}`))
      if (tool !== undefined) {
        return {
          type: "function_call",
          ...tool,
          id: `fc_${number}`,
          call_id: callID,
          arguments: JSON.stringify(plan.args ?? {}),
        }
      }
      if (
        Array.isArray(tools) &&
        tools.some((item: Item) => item["type"] === "tool_search") &&
        !input.some((item) => item["call_id"] === searchID)
      ) {
        return {
          type: "tool_search_call",
          id: `ts_${number}`,
          call_id: searchID,
          execution: "client",
          status: "completed",
          arguments: { query: `envoy ${plan.tool}`, limit: 20 },
        }
      }
    }
  }
  return {
    type: "message",
    role: "assistant",
    id: `msg_${number}`,
    status: "completed",
    content: [{ type: "output_text", annotations: [], text: "Ready." }],
  }
}

function responseEvents(number: number, item: Item): Response {
  const id = `resp_${number}`
  return new Response(
    [
      { type: "response.created", response: { id } },
      { type: "response.output_item.done", item },
      {
        type: "response.completed",
        response: {
          id,
          usage: {
            input_tokens: 1,
            input_tokens_details: { cached_tokens: 0 },
            output_tokens: 1,
            output_tokens_details: { reasoning_tokens: 0 },
            total_tokens: 2,
          },
        },
      },
    ]
      .map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`)
      .join(""),
    { headers: { "content-type": "text/event-stream" } },
  )
}

function spawn(argv: string[], home: Home | undefined, piping: "pipe" | "ignore" = "ignore") {
  const child = Bun.spawn(argv, {
    cwd: home?.cwd ?? work,
    env: home?.env ?? { ...NO_TRANSPILER_CACHE, PATH },
    stdin: piping,
    stdout: "pipe",
    stderr: "pipe",
  })
  children.add(child)
  void child.exited.then(() => children.delete(child))
  return child
}

async function command(argv: string[], home?: Home) {
  const child = spawn(argv, home)
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  return { code, stdout, stderr }
}

async function checked(argv: string[], home?: Home) {
  const result = await command(argv, home)
  if (result.code !== 0)
    throw new Error(`${argv.join(" ")} exited ${result.code}: ${result.stderr}`)
  return result.stdout
}

interface Home {
  readonly codex: string
  readonly cwd: string
  readonly codexHome: string
  readonly pluginData: string
  readonly env: Record<string, string>
  readonly tmux: string
}

async function makeHome(label: string, binary: string, trustProject: boolean): Promise<Home> {
  if (envoy === undefined || dispatch === undefined || model === undefined) {
    throw new Error("verification stacks have not started")
  }
  const base = join(work, label)
  const home = join(base, "home")
  const codexHome = join(base, "c")
  const cwd = join(base, "work")
  const temp = join(base, "tmp")
  const config = join(home, ".config", "legion")
  for (const directory of [codexHome, cwd, temp, config]) mkdirSync(directory, { recursive: true })
  writeFileSync(
    join(codexHome, "config.toml"),
    [
      'model = "standin-model"',
      'model_provider = "standin"',
      'approval_policy = "never"',
      'sandbox_mode = "danger-full-access"',
      "thread_unload_delay_secs = 5",
      "[analytics]",
      "enabled = false",
      "[model_providers.standin]",
      'name = "standin"',
      `base_url = "http://127.0.0.1:${model.port}/v1"`,
      'wire_api = "responses"',
      "requires_openai_auth = false",
      ...(trustProject ? [`[projects.${JSON.stringify(cwd)}]`, 'trust_level = "trusted"'] : []),
      "",
    ].join("\n"),
  )
  writeFileSync(
    join(config, "envoy.json"),
    JSON.stringify({
      envoy: { enabled: true, url: envoy.envoyUrl, natsUrls: [envoy.natsUrl] },
      dispatch: { enabled: true, serverUrl: dispatch.url, token: dispatch.token },
    }),
    { mode: 0o600 },
  )
  const fixtures: Home = {
    codex: binary,
    cwd,
    codexHome,
    pluginData: join(codexHome, "plugins", "data", "codex-envoy-legion-test"),
    tmux: join(base, "t"),
    env: {
      ...NO_TRANSPILER_CACHE,
      PATH: `${dirname(process.execPath)}:${PATH}`,
      HOME: home,
      CODEX_HOME: codexHome,
      XDG_CONFIG_HOME: join(home, ".config"),
      XDG_STATE_HOME: join(home, ".local", "state"),
      TMPDIR: temp,
      TERM: "xterm-256color",
      LANG: "C.UTF-8",
    },
  }
  homes.push(fixtures)
  if (!trustProject) await checked(["git", "init", "-q", "--template=", cwd], fixtures)
  await checked([binary, "plugin", "marketplace", "add", market], fixtures)
  await checked([binary, "plugin", "add", "codex-envoy@legion-test"], fixtures)
  return fixtures
}

interface RpcMessage {
  readonly id?: number
  readonly error?: { readonly message: string }
  readonly result?: unknown
  readonly method?: string
  readonly params?: unknown
}

class ControlClient {
  private nextID = 0
  private readonly replies = new Map<number, RpcMessage>()
  readonly notifications: RpcMessage[] = []
  private readonly socket: WebSocket
  private failure: Error | undefined

  constructor(home: Home) {
    const control = controls.get(home)
    if (control === undefined) throw new Error("the Codex control socket has not started")
    this.socket = new WebSocket(control.url)
    sockets.add(this.socket)
    this.socket.addEventListener("close", () => sockets.delete(this.socket))
    this.socket.addEventListener("error", () => {
      this.failure = new Error("the Codex control WebSocket reported an error")
    })
    this.socket.addEventListener("message", ({ data }) => {
      try {
        const packet = JSON.parse(String(data)) as RpcMessage
        if (packet.id === undefined) this.notifications.push(packet)
        else this.replies.set(packet.id, packet)
      } catch (cause) {
        this.failure = new Error("Codex sent an invalid RPC reply", { cause })
      }
    })
  }

  private write(packet: Item) {
    this.socket.send(JSON.stringify({ jsonrpc: "2.0", ...packet }))
  }

  async open() {
    await until("the Codex control WebSocket to open", Date.now() + bound, () => {
      if (this.failure !== undefined) throw this.failure
      return this.socket.readyState === WebSocket.OPEN || undefined
    })
    await this.call("initialize", {
      clientInfo: { name: "codex-envoy-verification", version: "1" },
      capabilities: { experimentalApi: true },
    })
    this.write({ method: "initialized" })
    return this
  }

  async call<T = unknown>(method: string, params: Item): Promise<T> {
    const id = ++this.nextID
    this.write({ id, method, params })
    const reply = await until(method, Date.now() + bound, () => {
      if (this.failure !== undefined) throw this.failure
      return this.replies.get(id)
    })
    this.replies.delete(id)
    if (reply.error !== undefined) throw new Error(`${method}: ${reply.error.message}`)
    return reply.result as T
  }

  close() {
    this.socket.close()
  }
}

interface Hook {
  readonly key: string
  readonly currentHash: string
  readonly pluginId: string | null
  readonly trustStatus: string
}

async function hooks(control: ControlClient, home: Home) {
  const listed = await control.call<{ data: { hooks: Hook[] }[] }>("hooks/list", {
    cwds: [home.cwd],
  })
  return listed.data
    .flatMap((entry) => entry.hooks)
    .filter((hook) => hook.pluginId?.includes("codex-envoy"))
}

async function daemon(home: Home) {
  await checked([home.codex, "app-server", "daemon", "start"], home)
  // Codex's local daemon speaks WebSocket on a Unix socket. The loopback bridge lets Bun's
  // WebSocket client make one independent Unix connection per test client.
  const path = join(home.codexHome, "app-server-control", "app-server-control.sock")
  const route = `/${crypto.randomUUID()}`
  const server = createServer((_request, response) => response.writeHead(404).end())
  server.on("upgrade", (request, downstream, head) => {
    // Keep access to the Unix socket private just as Codex does.
    if (request.url !== route) {
      downstream.destroy()
      return
    }
    const upstream = connectSocket(path)
    const headers = []
    for (let index = 0; index < request.rawHeaders.length; index += 2) {
      headers.push(`${request.rawHeaders[index]}: ${request.rawHeaders[index + 1]}`)
    }
    upstream.write(`GET / HTTP/1.1\r\n${headers.join("\r\n")}\r\n\r\n`)
    upstream.write(head)
    downstream.pipe(upstream).pipe(downstream)
    downstream.on("error", () => upstream.destroy())
    upstream.on("error", () => downstream.destroy())
  })
  const listening = Promise.withResolvers<void>()
  server.once("error", listening.reject)
  server.listen(0, "127.0.0.1", listening.resolve)
  await listening.promise
  const address = server.address()
  if (address === null || typeof address === "string")
    throw new Error("missing loopback bridge port")
  controls.set(home, { server, url: `ws://127.0.0.1:${address.port}${route}` })
  const control = await new ControlClient(home).open()
  try {
    const pluginHooks = await hooks(control, home)
    expect(pluginHooks.length, "Codex discovered the plugin's hooks").toBeGreaterThanOrEqual(3)
    if (noTrust === "1") {
      expect(pluginHooks.every((hook) => hook.trustStatus !== "trusted")).toBe(true)
    } else {
      await control.call("config/batchWrite", {
        edits: pluginHooks.map((hook) => ({
          keyPath: `hooks.state.${JSON.stringify(hook.key)}.trusted_hash`,
          value: hook.currentHash,
          mergeStrategy: "upsert",
        })),
      })
      expect((await hooks(control, home)).every((hook) => hook.trustStatus === "trusted")).toBe(
        true,
      )
    }
  } finally {
    control.close()
  }
}

function launches(standby = false) {
  if (!existsSync(launchLog)) return []
  return readFileSync(launchLog, "utf8")
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line) as ServerStart)
    .filter((row) => row.standby === standby)
}

function processCommand(pid: number) {
  return readFileSync(`/proc/${pid}/cmdline`, "utf8").replaceAll("\0", " ")
}

async function jsonRows(url: string, token?: string): Promise<SessionRow[]> {
  const response = await fetch(url, {
    ...(token === undefined ? {} : { headers: { Authorization: `Bearer ${token}` } }),
    signal: AbortSignal.timeout(2_000),
  })
  if (!response.ok) throw new Error(`${new URL(url).pathname} returned ${response.status}`)
  return (await response.json()) as SessionRow[]
}

async function roster() {
  if (envoy === undefined || dispatch === undefined)
    throw new Error("verification stacks have not started")
  const [listener, agents] = await Promise.all([
    jsonRows(`${envoy.envoyUrl}/v1/sessions`),
    jsonRows(`${dispatch.url}/api/v1/agents`, dispatch.token),
  ])
  return { listener, agents }
}

async function listed(id: string, promptAt: number, sharedDirectory?: string) {
  const rows = await until(
    `${id} on the listener and Agents page within 10 s`,
    promptAt + bound,
    async () => {
      const { listener, agents } = await roster()
      const envoyRow = listener.find((row) => row.session_id === id)
      const dispatchRow = agents.find((row) => row.session_id === id)
      return envoyRow === undefined || dispatchRow === undefined
        ? undefined
        : { envoyRow, dispatchRow }
    },
  )
  const expected = {
    session_id: id,
    ...(sharedDirectory === undefined
      ? {}
      : { title: `${basename(sharedDirectory)} ${id.slice(0, 8)}` }),
  }
  expect(rows.envoyRow).toMatchObject({ ...expected, plugin: "@sjawhar/codex-envoy" })
  expect(rows.dispatchRow).toMatchObject(expected)
  console.log(`${id} listed after ${elapsed(promptAt)} s ${JSON.stringify(rows)}`)
}

async function absent(id: string, endedAt: number, other?: string) {
  await until(`${id} to leave both rosters within 10 s`, endedAt + bound, async () => {
    const { listener, agents } = await roster()
    return (
      [listener, agents].every(
        (rows) =>
          !rows.some((row) => row.session_id === id) &&
          (other === undefined || rows.some((row) => row.session_id === other)),
      ) || undefined
    )
  })
  console.log(`${id} left both rosters after ${elapsed(endedAt)} s`)
}

interface Thread {
  readonly id: string
  readonly control: ControlClient
  readonly launched: ServerStart
}

async function startThread(home: Home, existingServer?: ServerStart): Promise<Thread> {
  const control = await new ControlClient(home).open()
  const before = launches().length
  const { thread } = await control.call<{ thread: { id: string } }>("thread/start", {
    cwd: home.cwd,
  })
  const launched = await until(
    `MCP parent for ${thread.id}`,
    Date.now() + bound,
    () => launches()[before] ?? existingServer,
  )
  expect(processCommand(launched.parent)).toContain("app-server")
  expect(processCommand(launched.parent)).toContain("--managed-daemon")
  return { id: thread.id, control, launched }
}

async function turn(thread: Thread, prompt: string) {
  const at = Date.now()
  const { turn } = await thread.control.call<{ turn: { id: string } }>("turn/start", {
    threadId: thread.id,
    input: [{ type: "text", text: prompt, text_elements: [] }],
  })
  return { at, id: turn.id }
}

function completed(thread: Thread, turnID: string) {
  return thread.control.notifications.some(({ method, params }) => {
    if (method !== "turn/completed") return false
    const data = params as { threadId: string; turn: { id: string; status: string } }
    if (data.threadId !== thread.id || data.turn.id !== turnID) return false
    expect(data.turn.status).toBe("completed")
    return true
  })
}

async function idle(thread: Thread, prompt = marker("ready")) {
  const started = await turn(thread, prompt)
  await until(
    `turn ${started.id} to complete`,
    started.at + bound,
    () => completed(thread, started.id) || undefined,
  )
  return started
}

async function send(id: string, message: string) {
  let result: Awaited<ReturnType<typeof client.send>>
  try {
    result = await client.send({
      source: "agent",
      sourceSessionID: peer,
      targetSessionID: id,
      message,
    })
  } catch (cause) {
    throw new Error(`the listener did not accept ${message} for ${id}`, { cause })
  }
  if (result.keyTaken !== undefined) throw new Error(`the listener did not store ${message}`)
  const at = result.envelope.issued_at
  console.log(`${message} accepted for ${id} at ${at}`)
  return at
}

async function carrying(id: string, message: string, sentAt: number, timeout = bound) {
  const row = await until(
    `${id} to read ${message} since acceptance after ${elapsed(sentAt)} s`,
    sentAt + timeout,
    () => requests.find((request) => request.thread === id && request.whole.includes(message)),
  )
  expect(row.at - sentAt).toBeLessThanOrEqual(timeout)
  expect(row.turn).not.toBeNull()
  console.log(
    `${message} in request ${row.number}, thread ${id}, turn ${row.turn}, ${elapsed(sentAt, row.at)} s`,
  )
  return row
}

async function listenerRegistration(id: string, deadline: number, what: string) {
  if (envoy === undefined) throw new Error("Envoy verification stack has not started")
  const url = `${envoy.envoyUrl}/v1/sessions`
  return until(`listener registration of ${id} for ${what}`, deadline, async () =>
    (await jsonRows(url)).find((row) => row.session_id === id),
  )
}

async function delivery(id: string, label: string, registrationDeadline?: number) {
  const message = marker(label)
  if (registrationDeadline !== undefined)
    await listenerRegistration(id, registrationDeadline, message)
  const at = await send(id, message)
  return carrying(id, message, at)
}

function toolOutput(request: ModelRequest, prompt: string) {
  return request.input.find(
    (item) => item["type"] === "function_call_output" && item["call_id"] === `call_${prompt}`,
  )
}

function toolStarted(
  home: Home,
  prompt: string,
  sleepSeconds: number,
  waitForRegistration = false,
) {
  const flag = join(home.cwd, prompt)
  plans.set(prompt, {
    tool: "exec_command",
    waitForRegistration,
    // Leave the transcript follower time to observe the running turn before the check sends.
    args: { cmd: `sleep 1; touch '${flag}'; sleep ${sleepSeconds - 1}; echo finished-${prompt}` },
  })
  return () =>
    until(`shell for ${prompt} to start`, Date.now() + bound, () =>
      existsSync(flag) ? true : undefined,
    )
}

async function ended(home: Home, thread: Thread) {
  const record = sharedCodexRecordFile(home.pluginData, thread.launched.parent, thread.id)
  const generation = (await readCodexSessionRecord(record))?.generation
  if (generation === undefined) throw new Error(`no shared load record for ${thread.id}`)
  const before = hookCalls().length
  thread.control.close()
  const file = `${record}.ended`
  return until(`SessionEnd marker for ${thread.id}`, Date.now() + 15_000, () =>
    existsSync(file) && readFileSync(file, "utf8") === generation
      ? hookCalls()
          .slice(before)
          .find((hook) => hook.event === "session-end")?.at
      : undefined,
  )
}

async function viaDaemon<T>(body: (home: Home) => Promise<T>) {
  if (codex === undefined) throw new Error("set LEGION_TEST_CODEX_SHARED_BINARY")
  const home = await makeHome("daemon", codex, true)
  await daemon(home)
  return body(home)
}

async function setup() {
  try {
    envoy = await startEnvoyStack({ work: envoyWork })
    dispatch = await startDispatchStack({ work: dispatchWork, env: { ENVOY_URL: envoy.envoyUrl } })
  } catch (error) {
    console.error("CODEX_SHARED_SETUP_FAILED")
    throw error
  }
  client = createEnvoyClient({ baseUrl: envoy.envoyUrl, fetch })
  const nats = await connect({ servers: [envoy.natsUrl], name: peer })
  bus = nats
  const subject = legacyAgentSubject(peer)
  const ledger = createDeliveryLedger({
    host: {
      sessionID: () => peer,
      followedSubjects: () => [subject],
      showFailureNotice: async () => undefined,
      failureNoticeFailed: (_notice, error) => console.error(error),
      log: (message, error) => console.error(message, error),
      write: () => undefined,
    },
  })
  const codec = StringCodec()
  const delivery = createConsumerDelivery({
    sessionID: peer,
    ledger,
    host: {
      deliver: (frame) => {
        const message = EnvelopeSchema.parse(JSON.parse(frame.data))
        received.push({ message: message.payload_summary, source: message.source_session })
        frame.done()
      },
      gap: (notice) => {
        throw new Error(notice.message)
      },
      log: (message, error) => console.error(message, error),
    },
    publish: (reply, body) => nats.publish(reply, codec.encode(body)),
    persist: () => true,
  })
  nats.subscribe(delivery.subject, {
    callback: (error, message) => {
      if (error) throw error
      delivery.receive(message)
    },
  })
  await nats.flush()
  const answer = await client.subscribe({
    sessionID: peer,
    directory: work,
    topics: [subject],
    port: 0,
    title: "Codex verification peer",
    driving: false,
    selfSubscribed: true,
    plugin: { name: "@legion/envoy-client", version: "codex-shared-verification" },
    delivery: delivery.registration(),
  })
  delivery.answered(answer.delivery)

  model = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      if (!request.url.endsWith("/responses") || request.method !== "POST")
        return Response.json({ data: [] })
      const body = (await request.json()) as { tools?: unknown; input?: Item[] }
      const input = body.input ?? []
      const whole = JSON.stringify(input)
      const number = requests.length + 1
      const latestUser = JSON.stringify(
        input.filter((item) => item["role"] === "user").at(-1)?.["content"] ?? "",
      )
      const at = Date.now()
      const thread = request.headers.get("thread-id")
      const metadata = JSON.parse(request.headers.get("x-codex-turn-metadata") ?? "{}") as {
        turn_id?: string
      }
      requests.push({
        number,
        at,
        thread,
        turn: metadata.turn_id ?? request.headers.get("turn-id"),
        input,
        whole,
      })
      const plan = [...plans].find(([key]) => latestUser.includes(key))?.[1]
      if (plan?.holdMs !== undefined) await Bun.sleep(plan.holdMs)
      if (plan?.waitForRegistration === true && thread !== null) {
        await listenerRegistration(thread, at + bound, "the standalone tool turn")
      }
      return responseEvents(number, modelAnswer(input, body.tools, number))
    },
  })
  market = join(work, "market")
  const plugin = join(market, "plugins", "codex-envoy")
  mkdirSync(join(market, ".agents", "plugins"), { recursive: true })
  const build = Bun.spawnSync([process.execPath, "scripts/build.ts", "--codex-out", plugin], {
    cwd: packageRoot,
    env: { ...NO_TRANSPILER_CACHE, PATH },
    stderr: "pipe",
    stdout: "pipe",
  })
  if (build.exitCode !== 0) throw new Error(`plugin build failed: ${build.stderr.toString()}`)
  writeFileSync(
    join(market, ".agents", "plugins", "marketplace.json"),
    JSON.stringify({
      name: "legion-test",
      plugins: [
        {
          name: "codex-envoy",
          policy: { authentication: "ON_INSTALL", installation: "AVAILABLE" },
          source: { source: "local", path: "./plugins/codex-envoy" },
        },
      ],
    }),
  )
  wrapBundle(
    plugin,
    "envoy-channel",
    `
appendFileSync(${JSON.stringify(launchLog)}, JSON.stringify({ pid: process.pid, parent: process.ppid, standby: process.argv[2] === "--codex-standby" }) + "\\n");
const stderr = process.stderr.write.bind(process.stderr);
process.stderr.write = (data, ...args) => {
  appendFileSync(${JSON.stringify(serverLog)}, data);
  return stderr(data, ...args);
};
`,
  )
  wrapBundle(
    plugin,
    "codex-hook",
    `
appendFileSync(${JSON.stringify(hookLog)}, JSON.stringify({ event: process.argv[2], at: Date.now() }) + "\\n");
const stderr = process.stderr.write.bind(process.stderr);
process.stderr.write = (data, ...args) => {
  appendFileSync(${JSON.stringify(hookErrors)}, data);
  return stderr(data, ...args);
};
`,
  )
}

function wrapBundle(plugin: string, name: string, observe: string) {
  const entry = join(plugin, "dist", `${name}.js`)
  const bundle = readFileSync(entry, "utf8")
  // Keep the script as Bun's entry point so the hook's import.meta.main test remains true.
  writeFileSync(
    entry,
    `import { appendFileSync as fixtureAppendFileSync } from "node:fs";
{
const appendFileSync = fixtureAppendFileSync;
${observe}
}
${bundle}`,
  )
}

function hookCalls(): { event: string; at: number }[] {
  if (!existsSync(hookLog)) return []
  return readFileSync(hookLog, "utf8")
    .trimEnd()
    .split("\n")
    .map((line) => JSON.parse(line))
}

const scenarios: { name: string; run: () => Promise<void> }[] = []

function scenario(name: string, run: () => Promise<void>) {
  scenarios.push({ name, run })
}

scenario("listed", () =>
  viaDaemon(async (home) => {
    const a = await startThread(home)
    const b = await startThread(home)
    expect(a.id).not.toBe(b.id)
    expect(a.launched.parent).toBe(b.launched.parent)
    console.log(`threads ${a.id} and ${b.id} share app server PID ${a.launched.parent}`)
    const turns = await Promise.all([idle(a), idle(b)])
    const outcomes = await Promise.allSettled([
      listed(a.id, turns[0].at, home.cwd),
      listed(b.id, turns[1].at, home.cwd),
    ])
    const failures = outcomes.flatMap((outcome) =>
      outcome.status === "rejected" ? [String(outcome.reason)] : [],
    )
    if (failures.length > 0) throw new Error(failures.join("\n"))
  }),
)

scenario("delivered", () =>
  viaDaemon(async (home) => {
    const a = await startThread(home)
    const ready = await idle(a)
    // The original plugin must fail this case at the send, naming the message that did not arrive.
    const first = await delivery(a.id, "idle", ready.at + bound)
    expect(first.turn).not.toBe(ready.id)
    const idleTurn = first.turn
    if (idleTurn === null) throw new Error("idle delivery had no turn ID")
    await until(
      `delivery turn ${idleTurn} to finish`,
      Date.now() + bound,
      () => completed(a, idleTurn) || undefined,
    )

    const shell = marker("sleep")
    const started = toolStarted(home, shell, 3)
    const running = await turn(a, shell)
    await started()
    const duringTool = await delivery(a.id, "running-tool")
    expect(duringTool.turn).toBe(running.id)
    expect(JSON.stringify(toolOutput(duringTool, shell))).toContain(`finished-${shell}`)
    await until(
      `tool turn ${running.id} to finish`,
      Date.now() + bound,
      () => completed(a, running.id) || undefined,
    )

    const hold = marker("no-tool")
    plans.set(hold, { holdMs: 8_000 })
    const waiting = await turn(a, hold)
    await until(`the held request for ${waiting.id}`, Date.now() + bound, () =>
      requests.find(
        (row) => row.thread === a.id && row.turn === waiting.id && row.whole.includes(hold),
      ),
    )
    const message = marker("running-model")
    const sent = await send(a.id, message)
    const last = await carrying(a.id, message, sent, 8_000 + bound)
    const next = requests.find(
      (row) => row.thread === a.id && row.at >= sent && row.turn !== waiting.id,
    )
    expect(next?.number).toBe(last.number)
  }),
)

scenario("isolated", () =>
  viaDaemon(async (home) => {
    const a = await startThread(home)
    const b = await startThread(home)
    const owner = await currentOwner(codexServersDirectory(home.pluginData, a.launched.parent))
    expect(owner?.pid).toBe(a.launched.pid)
    expect(owner?.pid).not.toBe(b.launched.pid)
    const ready = await Promise.all([idle(a), idle(b)])
    await Promise.all(
      [a, b].map((thread, index) =>
        listenerRegistration(
          thread.id,
          (ready[index]?.at ?? Date.now()) + bound,
          "the isolation checks",
        ),
      ),
    )

    const sent: { id: string; message: string }[] = []
    async function sendPair(label: string) {
      const pair = [a, b].map(({ id }) => ({ id, message: marker(label) }))
      sent.push(...pair)
      return Promise.all(
        pair.map(async ({ id, message }) => carrying(id, message, await send(id, message))),
      )
    }
    const idleRows = await sendPair("isolation-idle")
    await Promise.all(
      [a, b].map((thread, index) => {
        const turnID = idleRows[index]?.turn
        if (turnID === undefined || turnID === null)
          throw new Error(`no idle delivery turn for ${thread.id}`)
        return until(
          `${thread.id} to finish delivery`,
          Date.now() + bound,
          () => completed(thread, turnID) || undefined,
        )
      }),
    )

    const tools = await Promise.all(
      [a, b].map(async (thread, index) => {
        const prompt = marker(`isolation-tool-${index}`)
        const started = toolStarted(home, prompt, 3)
        const running = await turn(thread, prompt)
        await started()
        return running.id
      }),
    )
    const runningRows = await sendPair("isolation-running")
    expect(runningRows.map((row) => row.turn)).toEqual(tools)
    await Promise.all(
      [a, b].map((thread, index) => {
        const turnID = tools[index]
        if (turnID === undefined) throw new Error(`missing tool turn of ${thread.id}`)
        return until(
          `${thread.id} to finish its tool turn`,
          Date.now() + bound,
          () => completed(thread, turnID) || undefined,
        )
      }),
    )

    const call = marker("send")
    const outbound = marker("from-standby")
    plans.set(call, { tool: "envoy_send", args: { session_id: peer, message: outbound } })
    await idle(b, call)
    const envelope = await until(`the peer to receive ${outbound}`, Date.now() + bound, () =>
      received.find((row) => row.message.includes(outbound)),
    )
    expect(envelope.source).toBe(b.id)
    console.log(`peer got ${outbound} from standby thread ${envelope.source}`)

    const who = marker("whoami")
    plans.set(who, { tool: "dispatch", args: { args: ["whoami"] } })
    const response = await idle(a, who)
    const whoami = requests
      .filter((row) => row.turn === response.id)
      .map((row) => toolOutput(row, who))
      .find(Boolean)
    expect(JSON.stringify(whoami)).toContain(a.id)
    expect(JSON.stringify(whoami)).toContain("[exit 0]")

    for (const { id, message } of sent) {
      const observing = [
        ...new Set(requests.filter((row) => row.whole.includes(message)).map((row) => row.thread)),
      ]
      console.log(`${message} in threads ${observing.join(", ")}`)
      expect(observing, message).toEqual([id])
    }
  }),
)

scenario("ended", () =>
  viaDaemon(async (home) => {
    const a = await startThread(home)
    const b = await startThread(home)
    const aTurn = await idle(a)
    const bTurn = await idle(b)
    await Promise.all([listed(a.id, aTurn.at), listed(b.id, bTurn.at)])
    const directory = codexServersDirectory(home.pluginData, a.launched.parent)
    expect((await currentOwner(directory))?.pid).toBe(a.launched.pid)
    const endedAt = await ended(home, a)
    console.log(`SessionEnd observed for owner thread ${a.id} at ${endedAt}`)
    await absent(a.id, endedAt, b.id)
    await until(
      `plugin server ${b.launched.pid} to take over`,
      endedAt + bound,
      async () => (await currentOwner(directory))?.pid === b.launched.pid || undefined,
    )
    await delivery(b.id, "surviving-owner-takeover")

    const late = marker("unloaded")
    try {
      await send(a.id, late)
    } catch (error) {
      // A deregistered session can be refused by the listener. Either way, the thread must stay unloaded.
      console.log(String(error))
    }
    const observationEnd = Date.now() + 30_000
    while (Date.now() < observationEnd) {
      expect(
        requests.some((row) => row.whole.includes(late)),
        late,
      ).toBe(false)
      await Bun.sleep(Math.min(100, observationEnd - Date.now()))
    }
    const positive = marker("resume-positive-control")
    const at = Date.now()
    await checked([home.codex, "queue", "--resume", "--thread", a.id, "--message", positive], home)
    await carrying(a.id, positive, at)
  }),
)

async function checkResume(home: Home, previous: Thread, startedAt: number, other: string) {
  const file = sharedCodexRecordFile(home.pluginData, previous.launched.parent, previous.id)
  const record = await until(
    `${previous.id} to write a resume record`,
    startedAt + bound,
    async () => {
      const found = await readCodexSessionRecord(file)
      return found?.source === "resume" && Bun.file(file).lastModified >= startedAt
        ? found
        : undefined
    },
  )
  expect(record.session_id).toBe(previous.id)
  console.log(`SessionStart resumed ${record.session_id}, source ${record.source}`)
  await listed(previous.id, startedAt)
  const [a, b] = await Promise.all([
    delivery(previous.id, "resumed-thread"),
    delivery(other, "other-thread"),
  ])
  expect(a.thread).toBe(previous.id)
  expect(b.thread).toBe(other)
  const fromB = [...b.input].reverse().find((item) => item["role"] === "user")
  expect(fromB).toBeDefined()
  const bMessage = /codex-shared-other-thread-[a-f0-9-]+/.exec(JSON.stringify(fromB))?.[0]
  expect(bMessage).toBeDefined()
  expect(
    requests.some((row) => row.thread === previous.id && row.whole.includes(bMessage ?? "")),
  ).toBe(false)
}

scenario("resumed", () =>
  viaDaemon(async (home) => {
    const a = await startThread(home)
    const b = await startThread(home)
    const [firstA, firstB] = await Promise.all([idle(a), idle(b)])
    await Promise.all([listed(a.id, firstA.at), listed(b.id, firstB.at)])
    await absent(a.id, await ended(home, a), b.id)

    const control = await new ControlClient(home).open()
    await until(`thread/resume of ${a.id}`, Date.now() + bound, async () => {
      try {
        const { thread } = await control.call<{ thread: { id: string } }>("thread/resume", {
          threadId: a.id,
        })
        expect(thread.id).toBe(a.id)
        return true
      } catch (error) {
        if (!String(error).includes("is closing")) throw error
        return undefined
      }
    })
    const resumed = { ...a, control }
    const turnA = await idle(resumed)
    await checkResume(home, resumed, turnA.at, b.id)

    await absent(a.id, await ended(home, resumed), b.id)
    const positive = marker("queue-resume")
    const at = Date.now()
    await checked([home.codex, "queue", "--resume", "--thread", a.id, "--message", positive], home)
    await carrying(a.id, positive, at)
    await checkResume(home, a, at, b.id)
  }),
)

scenario("single-mcp", () =>
  viaDaemon(async (home) => {
    const a = await startThread(home)
    const b = await startThread(home, a.launched)
    const [firstA, firstB] = await Promise.all([idle(a), idle(b)])
    await Promise.all([listed(a.id, firstA.at), listed(b.id, firstB.at)])
    const appServer = a.launched.parent
    expect(launches().filter(({ parent }) => parent === appServer)).toHaveLength(1)

    const endedAt = await ended(home, a)
    await absent(a.id, endedAt, b.id)
    await delivery(b.id, "sibling-without-mcp")
    await absent(b.id, await ended(home, b))
    await until(
      "the standby process to exit",
      Date.now() + bound,
      () =>
        launches(true).every(({ pid }) => {
          try {
            return !processCommand(pid).includes("--codex-standby")
          } catch {
            return true
          }
        }) || undefined,
    )
  }),
)

scenario("first-prompt", () =>
  viaDaemon(async (home) => {
    const a = await startThread(home)
    const b = await startThread(home, a.launched)
    const firstA = await idle(a)
    await listed(a.id, firstA.at)
    await absent(a.id, await ended(home, a))
    expect((await roster()).listener.some(({ session_id }) => session_id === b.id)).toBe(false)

    const firstB = await idle(b)
    await listed(b.id, firstB.at)
    await delivery(b.id, "first-prompt-after-mcp-exit")
    await absent(b.id, await ended(home, b))
  }),
)

async function screen(home: Home, target: string) {
  const result = await command(
    ["tmux", "-S", home.tmux, "capture-pane", "-p", "-J", "-t", target],
    home,
  )
  if (result.code !== 0) throw new Error(`Codex terminal ${target} closed: ${result.stderr}`)
  return result.stdout
}

async function keys(home: Home, target: string, ...key: string[]) {
  await checked(["tmux", "-S", home.tmux, "send-keys", "-t", target, ...key], home)
}

async function choose(
  home: Home,
  target: string,
  caption: string,
  choice: string,
  deadline: number,
) {
  let sent: { current: string; at: number } | undefined
  await until(`${caption} to accept "${choice}"`, deadline, async () => {
    const pane = await screen(home, target)
    if (!pane.includes(caption)) return true
    const entries = pane.split("\n").map((line) => /([› ]?)\s+(\d+)\. (.+)/.exec(line))
    const current = entries.find((entry) => entry?.[1] === "›")?.[2]
    const goal = entries.find((entry) => entry?.[3]?.includes(choice))?.[2]
    if (current === undefined || goal === undefined) return undefined
    if (sent?.current === current && Date.now() - sent.at < 750) return undefined
    await keys(
      home,
      target,
      current === goal ? "Enter" : Number(current) < Number(goal) ? "Down" : "Up",
    )
    sent = { current, at: Date.now() }
    return undefined
  })
}

async function terminal(home: Home, label: string, args: string[], prompt: string, agents = false) {
  const before = launches().length
  const beforeHooks = hookCalls().length
  const target = `codex:${label}`
  const newServer =
    (await command(["tmux", "-S", home.tmux, "has-session", "-t", "codex"], home)).code !== 0
  await checked(
    [
      "tmux",
      "-S",
      home.tmux,
      "-f",
      "/dev/null",
      ...(newServer
        ? ["new-session", "-d", "-s", "codex", "-x", "200", "-y", "50"]
        : ["new-window", "-d", "-t", "codex"]),
      "-n",
      label,
      "-c",
      home.cwd,
      home.codex,
      ...args,
      ...(agents ? [] : [prompt]),
    ],
    home,
  )

  const deadline = Date.now() + 40_000
  let screenText = ""
  let newAgentAt = 0
  let typedAt: number | undefined
  const row = await until(`first model request of ${label}`, deadline, async () => {
    const observed = requests.find(
      (request) => request.whole.includes(prompt) && request.thread !== null,
    )
    if (observed !== undefined) return observed
    screenText = await screen(home, target)
    const menu = [
      ["Trust this folder?", "Trust and continue"],
      ["Hooks need review", "Trust all and continue"],
      ["Meet GPT-6 Sol", "Use existing model"],
    ].find(([caption]) => caption !== undefined && screenText.includes(caption))
    if (menu?.[0] && menu[1]) {
      await choose(home, target, menu[0], menu[1], deadline)
      if (agents) typedAt = undefined
    } else if (agents && /\bn\s+new\b/i.test(screenText)) {
      if (Date.now() - newAgentAt > 750) {
        await keys(home, target, "n")
        newAgentAt = Date.now()
      }
    } else if (
      newAgentAt > 0 &&
      typedAt === undefined &&
      !/model:\s+loading/.test(screenText) &&
      /Ask Codex|›[ \t]*$/m.test(screenText)
    ) {
      await keys(home, target, "-l", prompt)
      // Codex's paste detection consumes a newline sent in the same input frame.
      await Bun.sleep(300)
      typedAt = Date.now()
      await keys(home, target, "Enter")
    }
    return undefined
  }).catch((cause: unknown) => {
    const visible = screenText
      .split("\n")
      .map((line) => line.trimEnd())
      .filter(Boolean)
    throw new Error(`Codex ${label} did not start a turn. Screen:\n${visible.join("\n")}`, {
      cause,
    })
  })
  const at =
    typedAt ??
    hookCalls()
      .slice(beforeHooks)
      .find((hook) => hook.event === "session-start")?.at
  if (at === undefined) throw new Error(`Codex did not run SessionStart for ${label}`)
  const launched = await until(`MCP process for ${label}`, at + bound, () => launches()[before])
  if (row.thread === null) throw new Error(`Codex did not name the thread for ${label}`)
  return { target, launched, at, id: row.thread }
}

scenario("terminals", async () => {
  if (codex === undefined) throw new Error("set LEGION_TEST_CODEX_SHARED_BINARY")
  const shared = await makeHome("terminal", codex, false)
  const embedded = await makeHome("embedded", codex, false)
  const rows = [
    { home: shared, label: "plain-a", args: [], agents: false, parent: "--managed-daemon" },
    { home: shared, label: "plain-b", args: [], agents: false, parent: "--managed-daemon" },
    { home: shared, label: "agents", args: ["agents"], agents: true, parent: "--managed-daemon" },
    {
      home: embedded,
      label: "agents-embedded",
      args: ["agents", "--no-daemon"],
      agents: true,
      parent: "agents --no-daemon",
    },
  ]
  const ids = new Set()
  const sharedParents = new Set()
  for (const { home, label, args, agents, parent } of rows) {
    const opened = await terminal(home, label, args, marker(label), agents)
    const commandLine = processCommand(opened.launched.parent)
    expect(commandLine).toContain(parent)
    await listed(opened.id, opened.at)
    await delivery(opened.id, label)
    ids.add(opened.id)
    if (home === shared) sharedParents.add(opened.launched.parent)
    console.log(`${label} ran under "${commandLine}" as thread ${opened.id}`)
  }
  expect(ids.size).toBe(rows.length)
  expect(sharedParents.size).toBe(1)
})

scenario("own-process", async () => {
  if (codex === undefined || oldCodex === undefined) throw new Error("set both Codex binaries")
  const rows = [
    {
      label: "exec",
      args: ["exec", "--skip-git-repo-check", "--dangerously-bypass-hook-trust"],
      binary: codex,
    },
    { label: "own-daemon", args: ["--no-daemon"], binary: codex },
    { label: "own-bypass", args: ["--dangerously-bypass-hook-trust"], binary: codex },
    { label: "own-old", args: [], binary: oldCodex },
  ]
  for (const { label, args, binary } of rows) {
    console.log(`${label} runs ${(await checked([binary, "--version"])).trim()}`)
    const exec = label === "exec"
    const home = await makeHome(label, binary, true)
    const prompt = marker(label)
    const started = toolStarted(home, prompt, 4, true)
    const before = launches().length
    const at = Date.now()
    const process = exec ? spawn([binary, ...args, prompt], home) : undefined
    const opened = process === undefined ? await terminal(home, label, args, prompt) : undefined
    const launched = await until(`MCP process for ${label}`, at + bound, () => launches()[before])
    const commandLine = processCommand(launched.parent)
    expect(commandLine).not.toContain("app-server")
    const record = await until(`SessionStart record for ${label}`, at + bound, () =>
      readCodexSessionRecord(codexSessionRecordFile(home.pluginData, launched.parent)),
    )
    if (opened !== undefined) expect(opened.id).toBe(record.session_id)
    await started()
    const listing = listed(record.session_id, opened?.at ?? at)
    const row = await delivery(record.session_id, label)
    await listing
    expect(JSON.stringify(toolOutput(row, prompt))).toContain(`finished-${prompt}`)
    if (process !== undefined) {
      expect(await process.exited).toBe(0)
    } else if (opened !== undefined) {
      await checked(["tmux", "-S", home.tmux, "kill-window", "-t", opened.target], home)
    }
    await absent(record.session_id, Date.now())
    console.log(`${label} ran under "${commandLine}" as thread ${record.session_id}`)
  }
})

describe("the Envoy plugin on the real Codex binary", () => {
  if (enabled) {
    beforeAll(setup, setupBound)
    afterAll(async () => {
      for (const socket of sockets) socket.close()
      for (const { server } of controls.values()) server.close()
      for (const child of children) child.kill("SIGTERM")
      for (const home of homes) {
        if (existsSync(home.tmux)) await command(["tmux", "-S", home.tmux, "kill-server"], home)
        if (existsSync(join(home.codexHome, "app-server-control"))) {
          await command([home.codex, "app-server", "daemon", "stop"], home)
        }
      }
      model?.stop(true)
      await bus?.close()
      try {
        await dispatch?.stop()
      } finally {
        await envoy?.stop()
      }
    }, 60_000)
  }
  for (const { name, run } of scenarios) {
    test.skipIf(!enabled || activeCase !== name)(
      name,
      async () => {
        try {
          await run()
        } catch (error) {
          const rows = await roster().catch((cause: unknown) => ({ error: String(cause) }))
          console.error(`rosters: ${JSON.stringify(rows)}`)
          for (const log of [serverLog, hookErrors]) {
            if (existsSync(log)) {
              console.error(readFileSync(log, "utf8").split("\n").slice(-15).join("\n"))
            }
          }
          for (const home of homes) {
            if (existsSync(home.pluginData)) {
              const state = [...new Bun.Glob("codex-sessions/**/*").scanSync(home.pluginData)]
              console.error(`Codex hook records in ${home.codexHome}: ${state.join(", ")}`)
            }
            const log = join(home.codexHome, "app-server-daemon", "daemon.stderr.log")
            if (!existsSync(log)) continue
            const lines = readFileSync(log, "utf8").trim().split("\n")
            for (const line of lines
              .filter((text) => /envoy-channel|hook|ERROR/.test(text))
              .slice(-12)) {
              console.error(line)
            }
          }
          const observed = requests.map(({ number, thread, turn }) => ({ number, thread, turn }))
          console.error(`model requests: ${JSON.stringify(observed)}`)
          throw error
        }
      },
      180_000,
    )
  }
})
