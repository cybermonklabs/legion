import { afterEach, expect, test } from "bun:test"
import { mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { SESSION_HEARTBEAT_MS, SESSION_LIVE_WINDOW_MS } from "@legion/contracts"
import type { SetRoleInput, SetRoleResult } from "@legion/envoy-client/transport"
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { legion } from "../package.json" with { type: "json" }
import type { RoleRegainHook } from "../src/envoy-channel-server"
import {
  legionPane,
  ROLE_CLAIM_RETRY_MS,
  ROLE_CLAIM_WAIT_MS,
  SAVED_MARKER_SUFFIX,
  startLegionClaim,
  waitForTranscript,
} from "../src/legion-claim"

const SESSION = "0b9c2f4e-2d3a-4c55-9a1e-6f7d8e9a0b1c"
/** The channel server's client instance, which the pane's claim names as its process. */
const INSTANCE = "a1b2c3d4e5f60718293a4b5c6d7e8f90"
const scratch: string[] = []
afterEach(async () => {
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true })
})

async function tempDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), "claude-envoy-legion-claim-"))
  scratch.push(dir)
  return dir
}

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
async function daemonFixture(name: string): Promise<Record<string, unknown>> {
  const file = resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)
  return JSON.parse(await readFile(file, "utf8"))
}

interface Posted {
  readonly path: string
  readonly body: Record<string, unknown>
}

function daemonAnswering(answer: (path: string) => Response): {
  readonly posted: Posted[]
  readonly fetch: typeof fetch
} {
  const posted: Posted[] = []
  const stub = (async (input, init) => {
    const path = new URL(input.toString()).pathname
    posted.push({ path, body: JSON.parse(String(init?.body)) })
    return answer(path)
  }) as typeof fetch
  return { posted, fetch: stub }
}

test("a session with no boot token is no Legion pane", () => {
  expect(legionPane({ LEGION_DAEMON_URL: "http://daemon.test" })).toBeUndefined()
  expect(legionPane({ LEGION_BOOT_TOKEN: " " })).toBeUndefined()
})

test("a Legion pane reads its boot token from the file the daemon named, and needs its address and directory", async () => {
  const dir = await tempDir()
  const tokenFile = join(dir, "boot")
  await writeFile(tokenFile, "boot-token-from-the-file\n")
  expect(
    legionPane({
      LEGION_BOOT_TOKEN_FILE: tokenFile,
      LEGION_BOOT_TOKEN: "ignored",
      LEGION_DAEMON_URL: "http://daemon.test",
      CLAUDE_CONFIG_DIR: dir,
    }),
  ).toEqual({
    daemonUrl: "http://daemon.test",
    bootToken: "boot-token-from-the-file",
    configDir: dir,
    launcherSaves: false,
  })
  expect(() => legionPane({ LEGION_BOOT_TOKEN: "t", CLAUDE_CONFIG_DIR: dir })).toThrow(
    "LEGION_DAEMON_URL is required for Legion",
  )
  expect(() =>
    legionPane({ LEGION_BOOT_TOKEN: "t", LEGION_DAEMON_URL: "http://daemon.test" }),
  ).toThrow("CLAUDE_CONFIG_DIR is required for Legion")
  expect(() =>
    legionPane({
      LEGION_BOOT_TOKEN_FILE: join(dir, "missing"),
      LEGION_DAEMON_URL: "http://daemon.test",
      CLAUDE_CONFIG_DIR: dir,
    }),
  ).toThrow("LEGION_BOOT_TOKEN_FILE")
})

test("the transcript wait answers the file Claude Code wrote under any project, and stops when aborted", async () => {
  const configDir = await tempDir()
  const aborted = new AbortController()
  const waiting = waitForTranscript(configDir, SESSION, aborted.signal, 5)
  aborted.abort()
  await expect(waiting).rejects.toThrow()
  const file = join(configDir, "projects", "-work", `${SESSION}.jsonl`)
  await mkdir(join(configDir, "projects", "-work"), { recursive: true })
  await writeFile(file, "{}\n")
  await mkdir(join(configDir, "projects", "-other"), { recursive: true })
  await expect(waitForTranscript(configDir, SESSION, new AbortController().signal)).resolves.toBe(
    file,
  )
})

test("registers once the transcript exists, says ready, claims the claim token's role soft right after, and reads nothing on a regain", async () => {
  const configDir = await tempDir()
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering((path) =>
    path.endsWith("/claims/register")
      ? Response.json(registered)
      : new Response(null, { status: 204 }),
  )
  const setRoles: SetRoleInput[] = []
  const remembered: string[] = []
  let regain: RoleRegainHook | undefined
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir, launcherSaves: false },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: {
      setRole: async (input): Promise<SetRoleResult> => {
        setRoles.push(input)
        return {
          claimed: true,
          interest: { session_id: SESSION, machine_id: "m", dir: "/w", topics: [] },
        }
      },
    },
    session: {
      rememberRole: async (role) => {
        remembered.push(role)
      },
      onRoleRegained: (hook) => {
        regain = hook
      },
    },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
    pollMs: 5,
  })

  // The claim is polling: the bridge's seed writes the transcript only after the server started.
  expect(claim.capability()).toBeUndefined()
  const transcript = join(configDir, "projects", "-work-legion-208", `${SESSION}.jsonl`)
  await mkdir(join(configDir, "projects", "-work-legion-208"), { recursive: true })
  await writeFile(transcript, "{}\n")
  await claim.booted

  const ready = {
    claimToken: registered["claimToken"],
    sessionId: SESSION,
    secret: registered["secret"],
    generation: registered["generation"],
  }
  expect(posted).toEqual([
    {
      path: "/legion/v1/claims/register",
      body: {
        bootToken: "boot-1",
        sessionId: SESSION,
        ompSessionFile: transcript,
        agentId: SESSION,
        pluginContract: legion.daemonApiVersion,
      },
    },
    { path: "/legion/v1/claims/ready", body: ready },
  ])
  // Soft, and naming this process: the listener holds the claim for this channel server alone,
  // so a second `claude` on the same session id neither takes nor releases it.
  expect(setRoles).toEqual([
    {
      sessionID: SESSION,
      role: String(registered["claimToken"]),
      soft: true,
      clientInstanceID: INSTANCE,
    },
  ])
  expect(remembered).toEqual([String(registered["claimToken"])])
  expect(claim.capability()).toEqual({
    sessionID: SESSION,
    tree: String(registered["tree"]),
    issue: String(registered["issue"]),
    role: registered["role"] as never,
    secret: String(registered["secret"]),
    claimToken: String(registered["claimToken"]),
    generation: Number(registered["generation"]),
    promptAgents: registered["promptAgents"] as never,
  })
  expect(stops).toBe(0)
  expect(lines).toEqual([])

  // A pane that takes no task value reads nothing at a regain, and sends no second ready: a
  // ready makes the daemon send nothing.
  await regain?.("some-other-role", "reclaimed")
  await regain?.(String(registered["claimToken"]), "reregistered")
  expect(posted).toHaveLength(2)
  expect(lines).toEqual([])
})

test("a registration the daemon refuses logs the refusal, stops the pane, and claims no role", async () => {
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const refusal = await daemonFixture("error.json")
  const { fetch } = daemonAnswering(() => Response.json(refusal, { status: 409 }))
  const lines: string[] = []
  let stops = 0
  const setRoles: SetRoleInput[] = []
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "spent", configDir, launcherSaves: false },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: {
      setRole: async (input) => {
        setRoles.push(input)
        return { claimed: false, holder: "x" }
      },
    },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
  })
  await claim.booted
  expect(lines).toEqual([`claims/register registration failed (409): ${refusal["error"]}`])
  expect(stops).toBe(1)
  expect(setRoles).toEqual([])
  expect(claim.capability()).toBeUndefined()
})

test("a role another live session holds stops the pane after it has said it is ready, logging why", async () => {
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering((path) =>
    path.endsWith("/claims/register")
      ? Response.json(registered)
      : new Response(null, { status: 204 }),
  )
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir, launcherSaves: false },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: { setRole: async () => ({ claimed: false, holder: "ses_other" }) },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
  })
  await claim.booted
  // The role is claimed after `claims/ready`, so the daemon has already been told this pane is
  // ready when the refusal stops it: the relaunch reads the claim's task value at its own
  // registration, the path a death inside a turn already takes.
  expect(posted.map(({ path }) => path)).toEqual([
    "/legion/v1/claims/register",
    "/legion/v1/claims/ready",
  ])
  expect(lines).toEqual([
    `claim boot failed after claims/register registered ${registered["claimToken"]}; stopping the pane so the daemon launches it again: Envoy role ${registered["claimToken"]} is held by live session ses_other`,
  ])
  expect(stops).toBe(1)
})

test("a claim refused by a standing claim under this session's own id is asked again, not thrown on", async () => {
  // The pane the daemon relaunched with `--resume`: the process it replaced renewed the claim at
  // its last heartbeat, so the listener refuses this one 409 naming this very session until
  // nobody has answered for that claim inside its live window. Throwing stops the pane, the
  // daemon launches it again, and it meets the same claim — a window of relaunches for every
  // relaunch (dispatch://INFRA-783).
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering((path) =>
    path.endsWith("/claims/register")
      ? Response.json(registered)
      : new Response(null, { status: 204 }),
  )
  const setRoles: SetRoleInput[] = []
  const remembered: string[] = []
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir, launcherSaves: false },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: {
      setRole: async (input): Promise<SetRoleResult> => {
        setRoles.push(input)
        // Two refusals naming this session, then the grant the live window buys.
        if (setRoles.length < 3) return { claimed: false, holder: SESSION }
        return {
          claimed: true,
          interest: { session_id: SESSION, machine_id: "m", dir: "/w", topics: [] },
        }
      },
    },
    session: {
      rememberRole: async (role) => {
        remembered.push(role)
      },
      onRoleRegained: () => undefined,
    },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
    pollMs: 5,
    roleRetryMs: 5,
    roleWaitMs: 5_000,
  })
  await claim.booted

  const token = String(registered["claimToken"])
  expect(setRoles).toEqual(
    setRoles.map(() => ({
      sessionID: SESSION,
      role: token,
      soft: true,
      clientInstanceID: INSTANCE,
    })),
  )
  expect(setRoles).toHaveLength(3)
  expect(remembered).toEqual([token])
  expect(posted.map(({ path }) => path)).toEqual([
    "/legion/v1/claims/register",
    "/legion/v1/claims/ready",
  ])
  expect(stops).toBe(0)
  // Said once, however many times the claim is pressed: the state is normal and self-resolving.
  expect(lines).toEqual([
    `Envoy role ${token} is claimed under this session by the process this pane replaced; asking again every 5 ms for up to 5000 ms, until nobody has answered for that claim inside the listener's live window`,
  ])
})

test("the wait asks on its own interval until the live window passes, and the grant lands one interval after it", async () => {
  // The worst relaunch: the process this pane replaced renewed its claim at the moment this pane
  // first asked, so the listener holds the claim for a whole live window. `claims/ready` is
  // answered before the first attempt goes out — the role is claimed right after it — so the
  // daemon's registration deadline is already met and nothing outside this
  // pane is counting the wait. What the interval buys is how soon a session that is ready, and
  // may already have been given a task, becomes addressable once the window passes.
  //
  // The attempts run here 5 ms apart so the test is instant; each is answered as the listener
  // would answer the attempt at that point of the shipped schedule — `ROLE_CLAIM_RETRY_MS`
  // apart, the first at the claim — and the times asserted below are that schedule's.
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering((path) =>
    path.endsWith("/claims/register")
      ? Response.json(registered)
      : new Response(null, { status: 204 }),
  )
  const setRoles: SetRoleInput[] = []
  let grantedAt: number | undefined
  /** What the pane had posted to the daemon when it first asked for its role. */
  let postedWhenAsked: string[] | undefined
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir, launcherSaves: false },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: {
      setRole: async (input): Promise<SetRoleResult> => {
        setRoles.push(input)
        postedWhenAsked ??= posted.map(({ path }) => path)
        const at = (setRoles.length - 1) * ROLE_CLAIM_RETRY_MS
        // The listener refuses while the claim stands, naming this very session — the id the
        // predecessor holds it under — and grants it to the first attempt that falls after the
        // live window its last renewal bought it.
        if (at < SESSION_LIVE_WINDOW_MS) return { claimed: false, holder: SESSION }
        grantedAt ??= at
        return {
          claimed: true,
          interest: { session_id: SESSION, machine_id: "m", dir: "/w", topics: [] },
        }
      },
    },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: () => undefined,
    stop: () => {
      stops += 1
    },
    pollMs: 5,
    roleRetryMs: 5,
  })
  await claim.booted

  // Ready was said before the asking began, and the schedule says when the grant came: the first
  // attempt past the live window, 31 attempts in, 300,000 ms after the first.
  expect(postedWhenAsked).toEqual(["/legion/v1/claims/register", "/legion/v1/claims/ready"])
  expect(stops).toBe(0)
  expect(setRoles).toHaveLength(SESSION_LIVE_WINDOW_MS / ROLE_CLAIM_RETRY_MS + 1)
  expect(grantedAt).toBe(SESSION_LIVE_WINDOW_MS)
  // Inside the wait, so the pane never gives up on a claim that is about to be granted.
  expect(grantedAt).toBeLessThan(ROLE_CLAIM_WAIT_MS)
  // And that is what the interval buys: at the session heartbeat's cadence, the cadence this
  // wait ran at before, the first attempt past the window falls a minute later, a minute of a
  // ready session no task of the daemon's can reach.
  const atHeartbeatCadence =
    Math.ceil(SESSION_LIVE_WINDOW_MS / SESSION_HEARTBEAT_MS) * SESSION_HEARTBEAT_MS
  expect(atHeartbeatCadence - (grantedAt ?? Number.NaN)).toBe(60_000)
  // One attempt more than the window is the whole wait: the window is all that can explain a
  // refusal naming this session, so the pane gives up a single interval past it rather than
  // asking into a deadline nothing is counting.
  expect(ROLE_CLAIM_WAIT_MS).toBe(SESSION_LIVE_WINDOW_MS + ROLE_CLAIM_RETRY_MS)
})

test("a claim under this session's own id that is never granted stops the pane once the wait is out", async () => {
  // The wait is `ROLE_CLAIM_WAIT_MS` (`src/legion-claim.ts`), the listener's live window and one
  // retry more: past it, nothing explains a claim that still stands under this id, and the pane
  // is stopped as any other failed boot is.
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering((path) =>
    path.endsWith("/claims/register")
      ? Response.json(registered)
      : new Response(null, { status: 204 }),
  )
  const setRoles: SetRoleInput[] = []
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir, launcherSaves: false },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: {
      setRole: async (input) => {
        setRoles.push(input)
        return { claimed: false, holder: SESSION }
      },
    },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
    pollMs: 5,
    roleRetryMs: 5,
    roleWaitMs: 40,
  })
  await claim.booted

  const token = String(registered["claimToken"])
  expect(posted.map(({ path }) => path)).toEqual([
    "/legion/v1/claims/register",
    "/legion/v1/claims/ready",
  ])
  expect(setRoles.length).toBeGreaterThan(1)
  expect(lines.at(-1)).toBe(
    `claim boot failed after claims/register registered ${token}; stopping the pane so the daemon launches it again: Envoy role ${token} is still claimed under this session by another process after 40 ms of asking`,
  )
  expect(stops).toBe(1)
})

test("a registration the daemon failed (5xx) is logged and leaves the pane to the registration deadline", async () => {
  const configDir = await tempDir()
  await mkdir(join(configDir, "projects", "p"), { recursive: true })
  await writeFile(join(configDir, "projects", "p", `${SESSION}.jsonl`), "{}\n")
  const { fetch } = daemonAnswering(() =>
    Response.json({ error: "register failed" }, { status: 503 }),
  )
  const lines: string[] = []
  let stops = 0
  const claim = startLegionClaim({
    pane: { daemonUrl: "http://daemon.test", bootToken: "boot-1", configDir, launcherSaves: false },
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: { setRole: async () => ({ claimed: false, holder: "x" }) },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: (line) => lines.push(line),
    stop: () => {
      stops += 1
    },
  })
  // A boot that failed before a registration claims nothing, and says so at once rather than
  // leaving a caller waiting for a step that is never coming.
  await claim.booted
  expect(lines).toEqual([
    "claims/register registration failed (503): register failed",
    "claim boot failed before a registration; the daemon's registration deadline relaunches the pane: POST /legion/v1/claims/register failed with 503: register failed",
  ])
  expect(stops).toBe(0)
})

/** Boots a claim on the pane `env` describes, with its transcript already written, and returns
 * what it posted to the daemon and the transcript's path. */
async function bootOn(env: Record<string, string>): Promise<{
  readonly posted: Posted[]
  readonly transcript: string
  readonly booted: Promise<void>
}> {
  const configDir = await tempDir()
  const pane = legionPane({
    LEGION_BOOT_TOKEN: "boot-1",
    LEGION_DAEMON_URL: "http://daemon.test",
    CLAUDE_CONFIG_DIR: configDir,
    ...env,
  })
  if (pane === undefined) throw new Error("no Legion pane")
  const transcript = join(configDir, "projects", "-work", `${SESSION}.jsonl`)
  await mkdir(join(configDir, "projects", "-work"), { recursive: true })
  await writeFile(transcript, "{}\n")
  const registered = await daemonFixture("register.json")
  const { posted, fetch } = daemonAnswering((path) =>
    path.endsWith("/claims/register")
      ? Response.json(registered)
      : new Response(null, { status: 204 }),
  )
  const claim = startLegionClaim({
    pane,
    sessionID: SESSION,
    daemon: createLegionDaemonClient("http://daemon.test", fetch),
    clientInstanceID: INSTANCE,
    client: {
      setRole: async (): Promise<SetRoleResult> => ({
        claimed: true,
        interest: { session_id: SESSION, machine_id: "m", dir: "/w", topics: [] },
      }),
    },
    session: { rememberRole: async () => undefined, onRoleRegained: () => undefined },
    signal: new AbortController().signal,
    log: () => undefined,
    stop: () => undefined,
    pollMs: 5,
  })
  return { posted, transcript, booted: claim.booted }
}

test("in a pod whose launcher keeps the transcript, the registration waits for the launcher's first save", async () => {
  const { posted, transcript, booted } = await bootOn({ LEGION_CLAUDE_SESSION_ID: SESSION })
  // A real delay, deliberately: the boot polls the real filesystem every 5 ms, and the only sign
  // that it waits is a registration that does not come, which no fake clock can stand in for.
  // Twelve polls with the transcript there and no marker register nothing.
  await Bun.sleep(60)
  expect(posted).toEqual([])
  await writeFile(`${transcript}${SAVED_MARKER_SUFFIX}`, "")
  await booted
  expect(posted.map(({ path }) => path)).toEqual([
    "/legion/v1/claims/register",
    "/legion/v1/claims/ready",
  ])
  expect(posted[0]?.body["ompSessionFile"]).toBe(transcript)
})

test("a pane no launcher keeps registers once the transcript exists, waiting for no save", async () => {
  const { posted, transcript, booted } = await bootOn({})
  await booted
  expect(posted.map(({ path }) => path)).toEqual([
    "/legion/v1/claims/register",
    "/legion/v1/claims/ready",
  ])
  expect(posted[0]?.body["ompSessionFile"]).toBe(transcript)
})
