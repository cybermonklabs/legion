// The Codex plugin under Codex's shared app server, which runs one session per thread in a single
// `codex app-server` process (plain `codex` on Codex 0.170.0 and 0.171.0, and `codex agents`).
//
// A shell whose arguments name `app-server` stands in for that process: the plugin tells it apart
// by its command line (`sharedAppServer`). The plugin's hook and its MCP servers run as that
// shell's children, as Codex runs them as the app server's, with a stand-in listener, a stand-in
// NATS hub and a stand-in `codex` binary that writes each `codex queue` it is asked for to a file.
// The thread records are written where the hook writes them, `codex-sessions/<app server pid>/
// <thread id>.json`, and the hook's own rows run the hook itself.
import { afterEach, describe, expect, setDefaultTimeout, test } from "bun:test"
import { chmodSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { legacyAgentSubject } from "@legion/contracts"
import { deliverSubject } from "@legion/envoy-client/consumer-delivery"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import type { Subprocess } from "bun"
import { z } from "zod"
import { processStarted, writeCodexSessionRecord } from "../src/codex/codex-session-record"
import { forwardToolCall } from "../src/codex/codex-tool-route"
import { waitFor } from "./channel-session-harness"
import { FakeNatsServer } from "./fake-nats-server"
import { PROCESS_WAIT_MS, ROW_TIMEOUT_MS } from "./reopen-server"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

// Each row runs whole server processes, which a loaded machine starts and runs slowly; every wait
// ends the moment its fact holds, and its bound only names a step that is stuck.
setDefaultTimeout(ROW_TIMEOUT_MS)

const channelEntry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")
const hookEntry = resolve(import.meta.dir, "..", "hooks", "codex-hook.ts")
const makeDirectory = temporaryDirectories("codex-shared")

/** Two Codex thread ids, in the shape Codex gives one, whose short ids differ. */
const A = "019a0f3c-5b1e-7d2a-9c4f-0b1d2e3f4a5b"
const B = "019b7e21-6c2f-7e3b-8d5a-1c2e3f4a5b6c"

/** A plugin server's entry among the app server's servers (`../src/codex/codex-owner`). */
const ServerEntry = z.object({ pid: z.number() })

const stops: (() => Promise<void>)[] = []
afterEach(async () => {
  for (const stop of stops.splice(0).reverse()) await stop()
}, 60_000)

/** One registration the listener was sent. */
interface Registration {
  readonly session: string
  readonly title: string
  readonly dir: string
  readonly plugin: string
  readonly instance: string | undefined
}

/** One deregistration the listener was sent. */
interface Deregistration {
  readonly session: string
  readonly instance: string | null
}

/** The pids of the processes whose parent is `parent`, from /proc. */
function childrenOf(parent: number): number[] {
  return readdirSync("/proc").flatMap((name) => {
    if (!/^\d+$/.test(name)) return []
    try {
      const stat = readFileSync(`/proc/${name}/stat`, "utf8")
      const ppid = Number(stat.slice(stat.lastIndexOf(")") + 2).split(" ")[1])
      return ppid === parent ? [Number(name)] : []
    } catch {
      return []
    }
  })
}

/**
 * The stand-in shared app server, running `servers` plugin MCP servers as its children, each
 * reading the shell's standard input as Codex keeps each server's MCP pipe open, against a stand-in
 * listener and NATS hub.
 */
async function startSharedAppServer(servers: number) {
  const root = makeDirectory()
  const state = join(root, "state")
  mkdirSync(state, { recursive: true })
  const hub = new FakeNatsServer()
  const registrations: Registration[] = []
  const deregistrations: Deregistration[] = []
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as {
          session_id: string
          topics: string[]
          title: string
          dir: string
          plugin: string
          client_instance_id?: string
        }
        registrations.push({
          dir: body.dir,
          instance: body.client_instance_id,
          plugin: body.plugin,
          session: body.session_id,
          title: body.title,
        })
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: body.dir,
          topics: body.topics,
          delivery: consumerDelivery(body.session_id),
        })
      }
      if (request.method === "DELETE" && url.pathname.startsWith("/v1/sessions/")) {
        deregistrations.push({
          instance: url.searchParams.get("client_instance_id"),
          session: decodeURIComponent(url.pathname.slice("/v1/sessions/".length)),
        })
        return new Response(null, { status: 200 })
      }
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  // The stand-in `codex`: each call's arguments, NUL-separated, then an end line.
  const queuedLog = join(root, "queued.log")
  const codex = join(root, "codex")
  writeFileSync(
    codex,
    `#!/bin/sh\n{ for arg in "$@"; do printf '%s\\0' "$arg"; done; printf '\\n--end--\\n'; } >> '${queuedLog}'\n`,
  )
  chmodSync(codex, 0o755)
  // `sh -c <script> app-server …`: the arguments after the script start with `app-server`, which
  // is what names Codex's shared app server. `exec 3<&0` keeps the shell's standard input for the
  // servers, since a background job's own is /dev/null.
  const app = Bun.spawn(
    [
      "sh",
      "-c",
      'exec 3<&0; i=0; while [ "$i" -lt "$1" ]; do "$2" "$3" --host codex <&3 & i=$((i + 1)); done; wait',
      "app-server",
      String(servers),
      process.execPath,
      channelEntry,
    ],
    {
      cwd: state,
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: root,
        ENVOY_NATS_URL: hub.url,
        ENVOY_URL: `http://127.0.0.1:${listener.port}`,
        PLUGIN_DATA: state,
        XDG_RUNTIME_DIR: root,
        ...NO_TRANSPILER_CACHE,
      },
      stdin: "pipe",
      stdout: "ignore",
      stderr: "pipe",
    },
  )
  let stderr = ""
  void (async () => {
    for await (const chunk of app.stderr.pipeThrough(new TextDecoderStream())) stderr += chunk
  })()
  const started = processStarted(app.pid)
  if (started === undefined) throw new Error("the stand-in app server's start cannot be read")
  const threads = join(state, "codex-sessions", String(app.pid))
  const serversDirectory = join(state, "codex-servers", String(app.pid))
  stops.push(async () => {
    app.stdin.end()
    const exited = await Promise.race([app.exited.then(() => true), Bun.sleep(10_000)])
    if (exited !== true) {
      for (const child of childrenOf(app.pid)) process.kill(child, "SIGKILL")
      app.kill("SIGKILL")
      await app.exited
    }
    listener.stop(true)
    await hub.stop()
  })

  /** Records thread `thread`, working in a directory named `directory`, as `SessionStart` does. */
  const record = async (thread: string, directory: string, source = "startup"): Promise<void> => {
    const cwd = join(root, directory)
    mkdirSync(cwd, { recursive: true })
    await writeCodexSessionRecord(join(threads, `${thread}.json`), {
      codex_executable: codex,
      codex_home: join(root, "codex-home"),
      codex_started: started,
      cwd,
      session_id: thread,
      source,
      transcript_path: null,
    })
  }
  /** Leaves thread `thread`'s ended marker, as `SessionEnd` does when Codex unloads it. */
  const end = (thread: string): void => writeFileSync(join(threads, `${thread}.ended`), "")
  /** Loads thread `thread` again, as `SessionStart` with `source: "resume"` does. */
  const load = async (thread: string, directory: string): Promise<void> => {
    await record(thread, directory, "resume")
    rmSync(join(threads, `${thread}.ended`), { force: true })
  }
  /** Each `codex queue` call's arguments. */
  const queued = (): string[][] => {
    let text = ""
    try {
      text = readFileSync(queuedLog, "utf8")
    } catch {
      return []
    }
    return text
      .split("\n--end--\n")
      .filter((call) => call !== "")
      .map((call) => call.split("\0").slice(0, -1))
  }
  /** The pids of the plugin servers in their order of publication: the first is the owner. */
  const serversInOrder = (): number[] => {
    let names: string[] = []
    try {
      names = readdirSync(serversDirectory).filter((name) => /^\d+\.json$/.test(name))
    } catch {
      return []
    }
    return names
      .sort((a, b) => Number.parseInt(a, 10) - Number.parseInt(b, 10))
      .flatMap((name) => {
        try {
          return [
            ServerEntry.parse(JSON.parse(readFileSync(join(serversDirectory, name), "utf8"))).pid,
          ]
        } catch {
          return []
        }
      })
  }
  const describeState = (): string =>
    `registrations ${JSON.stringify(registrations)}; deregistrations ${JSON.stringify(deregistrations)}; queued ${JSON.stringify(queued())}; stderr:\n${stderr}`
  /** Relays one message to thread `thread`'s session, as the listener relays its consumer. */
  const send = (thread: string, seq: number, text: string, reply: string): void => {
    hub.deliverRelay(thread, {
      subject: legacyAgentSubject(thread),
      seq,
      payload: JSON.stringify({
        event_id: `evt-${reply}`,
        source: "agent",
        source_session: "ses_peer",
        topic: legacyAgentSubject(thread),
        dedupe_key: `delivery-${reply}`,
        issued_at: 1_760_000_000_000,
        payload_summary: text,
      }),
      reply,
    })
  }
  const relaySubscribed = (thread: string): boolean =>
    hub.subscribed.includes(deliverSubject(thread))
  return {
    app,
    deregistrations,
    describeState,
    end,
    hub,
    load,
    queued,
    record,
    registrations,
    relaySubscribed,
    root,
    send,
    serversDirectory,
    serversInOrder,
  }
}

/** Waits for a fact of the stand-in app server, naming its state when it never holds. */
function until(
  run: { readonly describeState: () => string },
  condition: () => boolean,
  what: string,
): Promise<void> {
  return waitFor(async () => condition(), what, run.describeState, PROCESS_WAIT_MS)
}

/**
 * Waits until each of `threads` has a running Envoy session at the owner, read through a forwarded
 * `envoy_whoami`, and answers what each answered. A session takes its relay subject and registers
 * before its start returns, so neither says the session runs; a call while it still starts, or
 * while no server serves the threads yet, is answered so and tried again.
 */
async function untilRunning(
  run: { readonly describeState: () => string; readonly serversDirectory: string },
  ...threads: string[]
): Promise<unknown[]> {
  const answers: unknown[] = []
  for (const thread of threads) {
    let answer: unknown
    await waitFor(
      async () => {
        try {
          answer = await forwardToolCall(run.serversDirectory, {
            arguments: {},
            name: "envoy_whoami",
            thread,
          })
          return true
        } catch (error) {
          const text = error instanceof Error ? error.message : String(error)
          if (text.includes("is starting") || text.includes("cannot be reached")) return false
          throw error
        }
      },
      `thread ${thread}'s Envoy session to run`,
      run.describeState,
      PROCESS_WAIT_MS,
    )
    answers.push(answer)
  }
  return answers
}

describe("under Codex's shared app server", () => {
  test("the owner registers each recorded thread as its own session, titled by its directory and short id, before any tool call", async () => {
    const run = await startSharedAppServer(1)
    await until(run, () => run.serversInOrder().length === 1, "the server's entry")
    await run.record(A, "alpha")
    await run.record(B, "beta")
    const registered = (thread: string) => run.registrations.find((each) => each.session === thread)
    await until(
      run,
      () => registered(A) !== undefined && registered(B) !== undefined,
      "a registration of each thread",
    )
    expect(registered(A)).toMatchObject({
      dir: join(run.root, "alpha"),
      plugin: "@sjawhar/codex-envoy",
      title: "alpha 019a0f3c",
    })
    expect(registered(B)).toMatchObject({
      dir: join(run.root, "beta"),
      plugin: "@sjawhar/codex-envoy",
      title: "beta 019b7e21",
    })
    // Each thread is a session of its own, with a client instance of its own.
    expect(registered(A)?.instance).toBeDefined()
    expect(registered(A)?.instance).not.toBe(registered(B)?.instance)
    expect([...new Set(run.registrations.map((each) => each.session))].sort()).toEqual([A, B])
    await until(
      run,
      () => run.relaySubscribed(A) && run.relaySubscribed(B),
      "each thread's relay subject",
    )
  })

  test("a message to each thread goes into that thread alone, through codex queue for its id", async () => {
    const run = await startSharedAppServer(1)
    await run.record(A, "alpha")
    await run.record(B, "beta")
    await untilRunning(run, A, B)
    run.send(A, 1, "nonce-for-alpha", "_INBOX.alpha")
    run.send(B, 1, "nonce-for-beta", "_INBOX.beta")
    const holding = (nonce: string): string[] =>
      run
        .queued()
        .filter((call) => call.some((arg) => arg.includes(nonce)))
        .map((call) => call[2] ?? "")
    await until(
      run,
      () => holding("nonce-for-alpha").length > 0 && holding("nonce-for-beta").length > 0,
      "each nonce queued",
    )
    expect(holding("nonce-for-alpha")).toEqual([A])
    expect(holding("nonce-for-beta")).toEqual([B])
    for (const call of run.queued()) expect(call.slice(0, 2)).toEqual(["queue", "--thread"])
    const answers = (reply: string): string[] =>
      run.hub.published.filter((frame) => frame.subject === reply).map((frame) => frame.payload)
    await until(
      run,
      () => answers("_INBOX.alpha").includes("done") && answers("_INBOX.beta").includes("done"),
      "both frames answered done",
    )
  })

  test("a thread's end deregisters its session alone, and loading it again registers it under the same id", async () => {
    const run = await startSharedAppServer(1)
    await run.record(A, "alpha")
    await run.record(B, "beta")
    await untilRunning(run, A, B)
    const instanceOfA = run.registrations.find((each) => each.session === A)?.instance
    expect(instanceOfA).toBeDefined()
    run.end(A)
    await until(
      run,
      () => run.deregistrations.some((each) => each.session === A),
      "thread A's deregistration",
    )
    expect(run.deregistrations).toEqual([{ instance: instanceOfA ?? null, session: A }])
    const before = run.registrations.length

    await run.load(A, "alpha")
    await until(
      run,
      () => run.registrations.slice(before).some((each) => each.session === A),
      "thread A registered again",
    )
    const again = run.registrations.slice(before).find((each) => each.session === A)
    expect(again).toMatchObject({ title: "alpha 019a0f3c" })
    expect(again?.instance).not.toBe(instanceOfA)
    // B kept its session throughout.
    expect(run.deregistrations.filter((each) => each.session === B)).toEqual([])
  })

  test("an Envoy tool call another server sends to the owner acts as the thread it names", async () => {
    const run = await startSharedAppServer(1)
    await run.record(A, "alpha")
    await run.record(B, "beta")
    const [b, a] = await untilRunning(run, B, A)
    expect(b).toMatchObject({ session_id: B, dir: join(run.root, "beta") })
    expect(a).toMatchObject({ session_id: A, dir: join(run.root, "alpha") })
    const whoami = (thread: string) =>
      forwardToolCall(run.serversDirectory, { arguments: {}, name: "envoy_whoami", thread })
    await expect(whoami("019c0000-0000-7000-8000-000000000000")).rejects.toThrow(
      "this thread has no Envoy session",
    )
    run.end(A)
    await until(
      run,
      () => run.deregistrations.some((each) => each.session === A),
      "thread A's deregistration",
    )
    await expect(whoami(A)).rejects.toThrow("this thread has no Envoy session")
  })

  test("when Codex stops the owner's server, the next server registers every live thread again, and an ended one stays ended", async () => {
    const run = await startSharedAppServer(2)
    await until(run, () => run.serversInOrder().length === 2, "both servers' entries")
    const [owner, next] = run.serversInOrder()
    await run.record(A, "alpha")
    await run.record(B, "beta")
    await untilRunning(run, A, B)
    const first = run.registrations.find((each) => each.session === B)?.instance
    expect(first).toBeDefined()
    // Codex unloads A, whose server is the owner: its marker, then the owner's server stops.
    run.end(A)
    await until(
      run,
      () => run.deregistrations.some((each) => each.session === A),
      "thread A's deregistration",
    )
    const before = run.registrations.length
    process.kill(owner ?? 0, "SIGTERM")
    // A registration of B by another client instance than the owner's: its heartbeats are the
    // owner's own. The owner's deregistration of B and the next server's registration of it can
    // land in either order (the listener keeps the newer instance's entry), so both are waited for.
    const takenOver = () =>
      run.registrations.slice(before).find((each) => each.session === B && each.instance !== first)
    await until(
      run,
      () =>
        takenOver() !== undefined &&
        run.deregistrations.some((each) => each.session === B && each.instance === first),
      "thread B deregistered by the owner and registered again by the next server",
    )
    expect(run.deregistrations).toEqual([
      { instance: expect.any(String), session: A },
      { instance: first ?? null, session: B },
    ])
    expect(takenOver()?.title).toBe("beta 019b7e21")
    // The next server read A's marker before it started any session: A stays ended.
    expect(run.registrations.slice(before).filter((each) => each.session === A)).toEqual([])
    await until(run, () => run.serversInOrder().length === 1, "the stopped owner's entry removed")
    expect(run.serversInOrder()).toEqual([next ?? 0])
    // And B's tool calls reach it there: the stopped owner served none after its stop began.
    const [taken] = await untilRunning(run, B)
    expect(taken).toMatchObject({ session_id: B })
    expect(run.hub.subscribed.filter((each) => each === deliverSubject(B)).length).toBe(2)
  })
})

describe("the SessionStart and SessionEnd hooks under Codex's shared app server", () => {
  test("record each thread on its own, leave its marker at its end, remove it at its load again, and record no fork", async () => {
    const root = makeDirectory()
    const state = join(root, "state")
    const work = join(root, "work")
    mkdirSync(join(root, ".config", "legion"), { recursive: true })
    mkdirSync(work, { recursive: true })
    writeFileSync(
      join(root, ".config", "legion", "envoy.json"),
      JSON.stringify({ envoy: { enabled: false }, dispatch: { enabled: false } }),
    )
    const transcript = join(root, `rollout-${A}.jsonl`)
    const input = (session: string, source: string, transcriptPath: string | null) =>
      JSON.stringify({ cwd: work, session_id: session, source, transcript_path: transcriptPath })
    writeFileSync(join(root, "start.json"), input(A, "startup", transcript))
    writeFileSync(join(root, "end.json"), JSON.stringify({ session_id: A }))
    writeFileSync(join(root, "resume.json"), input(A, "resume", transcript))
    writeFileSync(join(root, "fork.json"), input(B, "startup", null))
    // Every hook runs as a child of one stand-in app server, as Codex runs them as its own: `$$`
    // is that shell, whose pid names the directory of its thread records.
    const script = [
      'cd "$1"',
      '"$2" "$3" session-start < start.json > start.out',
      'ls "$4/$$" > after-start',
      '"$2" "$3" session-end < end.json',
      'ls "$4/$$" > after-end',
      '"$2" "$3" session-start < resume.json > resume.out',
      'ls "$4/$$" > after-resume',
      '"$2" "$3" session-start < fork.json > fork.out',
      'ls "$4/$$" > after-fork',
      "echo $$ > pid",
    ].join("; ")
    const hooks: Subprocess = Bun.spawn(
      [
        "sh",
        "-c",
        script,
        "app-server",
        root,
        process.execPath,
        hookEntry,
        join(state, "codex-sessions"),
      ],
      {
        env: {
          PATH: process.env["PATH"] ?? "",
          HOME: root,
          PLUGIN_DATA: state,
          ...NO_TRANSPILER_CACHE,
        },
        stdout: "ignore",
        stderr: "pipe",
      },
    )
    expect(await hooks.exited).toBe(0)
    const read = (name: string): string => readFileSync(join(root, name), "utf8")
    const listed = (name: string): string[] => read(name).split("\n").filter(Boolean)
    expect(listed("after-start")).toEqual([`${A}.json`])
    expect(listed("after-end")).toEqual([`${A}.ended`, `${A}.json`])
    expect(listed("after-resume")).toEqual([`${A}.json`])
    // A fork keeps no transcript and is never recorded.
    expect(listed("after-fork")).toEqual([`${A}.json`])
    expect(read("fork.out")).toContain("keeps no transcript")
    expect(read("start.out")).not.toContain("does not register")
    const pid = read("pid").trim()
    const recorded = JSON.parse(
      readFileSync(join(state, "codex-sessions", pid, `${A}.json`), "utf8"),
    ) as Record<string, unknown>
    expect(recorded).toMatchObject({
      cwd: work,
      session_id: A,
      source: "resume",
      transcript_path: transcript,
    })
    // No record of the process as a whole: that is a process of its own's.
    expect(readdirSync(join(state, "codex-sessions"))).toEqual([pid])
  })
})
