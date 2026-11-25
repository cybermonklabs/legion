import { expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"
import { LEGION_ROLES, type LegionRole } from "@legion/contracts"
import type { LegionPhase } from "@legion/contracts/legion-api"
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import {
  PHASE_OP_SENTENCES,
  PHASE_OPS,
  phaseOpsOf,
} from "@legion/pi-shared/legion-phase-op-declarations"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import { stateAtPhase } from "@legion/pi-shared/test/daemon-state"
import { withImageManifest } from "@legion/pi-shared/test/image-manifest"
import { legion, name, version } from "../package.json" with { type: "json" }
import { executeLegionTool, legionToolDefinition } from "../src/legion-tool"

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
async function daemonFixture(name: string): Promise<unknown> {
  const file = resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)
  return JSON.parse(await readFile(file, "utf8"))
}

/** A window of release records as the daemon serves it: two releases, the first woken five
 * minutes later and the second two hours later, each with a first turn observed. */
const RELEASE_RECORDS = {
  releases: [
    {
      id: 1,
      issue: "LEGION-208",
      generation: 2,
      roles: ["implementer"],
      at: "2026-11-01T12:00:00Z",
      bundleBytes: 2048,
    },
    {
      id: 2,
      issue: "LEGION-209",
      generation: 1,
      roles: ["tester"],
      at: "2026-11-01T13:00:00Z",
      bundleBytes: 0,
    },
  ],
  resumes: [
    {
      id: 1,
      release: 1,
      issue: "LEGION-208",
      role: "implementer",
      kind: "task",
      at: "2026-11-01T12:05:00Z",
      podReadyAt: "2026-11-01T12:05:30Z",
      firstTurnAt: "2026-11-01T12:06:00Z",
      stuck: false,
    },
    {
      id: 2,
      release: 2,
      issue: "LEGION-209",
      role: "tester",
      kind: "message",
      at: "2026-11-01T15:00:00Z",
      podReadyAt: "2026-11-01T15:01:00Z",
      firstTurnAt: "2026-11-01T15:02:00Z",
      stuck: false,
    },
  ],
  holds: [
    {
      id: 1,
      issue: "LEGION-210",
      reason: "background_work",
      detail: "async-job",
      firstAt: "2026-11-01T12:50:00Z",
      at: "2026-11-01T13:00:00Z",
      passes: 11,
    },
  ],
}

interface Request {
  readonly method: string
  readonly path: string
  readonly body: unknown
}

/** A stub daemon answering the golden fixtures: `state` in place of `state.json` when given, and
 * `daemonContract` as the contract its health route states when given. */
async function daemon(
  options: { readonly daemonContract?: number; readonly state?: unknown } = {},
): Promise<{
  readonly client: LegionDaemonClient
  readonly requests: Request[]
}> {
  const state = (options.state ?? (await daemonFixture("state.json"))) as {
    issues: Record<string, unknown>
  }
  const answers: Record<string, unknown> = {
    "/legion/v1/state": state,
    "/legion/v1/grants": await daemonFixture("grant.json"),
    "/legion/v1/handoff/complete": await daemonFixture("handoff-complete.json"),
    "/legion/v1/review-verdict": await daemonFixture("review-verdict.json"),
    "/legion/v1/phase/backward": {},
    "/legion/v1/claims/release-records": RELEASE_RECORDS,
  }
  const requests: Request[] = []
  const stub = (async (input, init) => {
    const path = new URL(input.toString()).pathname
    requests.push({
      method: init?.method ?? "GET",
      path,
      body: init?.body == null ? undefined : JSON.parse(String(init.body)),
    })
    // The health route states the daemon's contract in its header, which `read_build` reads.
    if (path === "/healthz") {
      return new Response(JSON.stringify({ status: "ok", project: "legion" }), {
        headers: {
          "Content-Type": "application/json",
          ...(options.daemonContract === undefined
            ? {}
            : { "Legion-Daemon-Contract": String(options.daemonContract) }),
        },
      })
    }
    // One issue's record, as the daemon answers it, and its 404 for an issue it records
    // nothing of (dispatch://INFRA-2694).
    const named = /^\/legion\/v1\/issues\/([^/]+)\/record$/.exec(path)
    if (named !== null) {
      const held = state.issues[decodeURIComponent(named[1] ?? "")]
      return held === undefined
        ? Response.json(
            { code: "RECORD_NOT_FOUND", error: `The daemon has no record for ${named[1]}` },
            { status: 404 },
          )
        : Response.json(held)
    }
    return Response.json(answers[path])
  }) as typeof fetch
  return {
    client: createLegionDaemonClient("http://daemon.test", {
      pluginContract: legion.daemonApiVersion,
      fetch: stub,
    }),
    requests,
  }
}

const implementer: ClaimCapability = {
  sessionID: "ses_implementer",
  tree: "LEGION-208",
  issue: "LEGION-208",
  role: "implementer",
  secret: "U2VjcmV0",
  claimToken: "legion-legion-legion-208-implementer",
  generation: 1,
}

/** The branch-head lookup of a tool that must never signal readiness for an update. */
const noBranchHead = async (): Promise<never> => {
  throw new Error("no branch-head lookup expected")
}

/** The handoff report of a tool that must never complete a phase. */
const noHandoffReport = async (): Promise<never> => {
  throw new Error("no handoff report expected")
}

const grantRequest = {
  method: "POST",
  path: "/legion/v1/grants",
  body: {
    sessionId: "ses_implementer",
    secret: "U2VjcmV0",
    tree: "LEGION-208",
    issue: "LEGION-208",
  },
}

// The phase ops are defined once, in `PHASE_OPS`, and every host's `legion` tool serves that list:
// this MCP tool and pi-legion's (`packages/pi-legion/src/tools.test.ts` holds the same of it), so
// the two list the same phase ops, review_verdict among them, to a review step's agent.
test("declares the phase ops, PHASE_OPS, under one op, and names the Envoy publish tool as Claude Code shows it", () => {
  const definition = legionToolDefinition({ LEGION_ROLE: "reviewer" })
  expect(definition.name).toBe("legion")
  expect(definition.inputSchema.properties?.["op"]).toMatchObject({ enum: [...PHASE_OPS] })
  expect(definition.inputSchema).toMatchObject({
    properties: {
      op: {
        enum: [
          "read_record",
          "read_release_records",
          "handoff_complete",
          "request_backward_move",
          "ready_for_update",
          "review_verdict",
          "read_build",
        ],
      },
      issue: { type: "string" },
      since: { type: "string" },
      summary: { type: "string" },
      verdict: { enum: ["pass", "pending", "fail", "approve", "changes"] },
      commit: {
        type: "string",
        description:
          "review_verdict's head: the pull request's current head, the commit your verdict is on",
      },
      wake_at: { type: "string" },
      plan_rating: { type: "integer", minimum: 1, maximum: 5 },
      plan_reason: { type: "string" },
      return_to: { type: "integer", minimum: 1 },
      reason: { type: "string" },
      round: { type: "string" },
    },
    required: ["op"],
  })
  // The exact fields, in order (toMatchObject above admits extra keys): no completion and no
  // verdict names the commit of a tester's pass.
  expect(Object.keys(definition.inputSchema.properties ?? {})).toEqual([
    "op",
    "issue",
    "since",
    "summary",
    "verdict",
    "wake_at",
    "plan_rating",
    "plan_reason",
    "return_to",
    "to",
    "reason",
    "commit",
    "round",
  ])
  expect(definition.description).toContain(
    "`verdict` on a phase completion, which is a review step's alone, approve|changes",
  )
  expect(definition.description).toContain(
    `this tool cannot spawn workers. ${PHASE_OP_SENTENCES.readRecord}${PHASE_OP_SENTENCES.readReleaseRecords}handoff_complete`,
  )
  expect(definition.description).toContain(
    "complete again. ready_for_update answers the daemon's update message on this issue's pod: `round` is the round id that message names, and the daemon takes your answer for that round alone. The reviewer creates no commit and pushes nothing, so each commit and push this description names is not its own, and the merger pushes the merge of the base it made. Push every commit you want kept to the issue branch first. Then let each subagent and each background job that you started finish, or stop it. Commit and push what it made before you answer. The daemon replaces the pod and its disk when the round ends. The round ends at the moment the message names. It can end earlier, once every role has answered, no role is in a turn and each role's last report names no running subagent or background job. No answer delays that moment, and the end of a turn is no answer. If a subagent or a background job finishes after you answer, commit and push what it made. Then call ready_for_update again with the same round. The call ends no phase. The daemon holds your next task until your process comes back on the new pod, in this same session. request_backward_move returns an issue to an earlier phase (`to`, `reason`). A phase worker moves its own issue. The daemon refuses the tester's move out of testing (BACKWARD_REFUSED), since a testing step ends by its checks' states: the tester records each check's result with `dispatch result`, a fail with its evidence for a check it cannot verify, and completes. The architect that owns an issue in accepting moves it, named by `issue`, back to the phase of an earlier step of its task, and names in `commit` the head it sends back. That move is its acceptance's changes at that head, which the daemon takes only at the pull request's current head (VERDICT_HEAD_NOT_CURRENT). That move counts no review round: only a review that requests changes counts one. review_verdict answers the daemon's request, when the head of the pull request moves once a review step of the issue's task has passed, for a verdict on the new head from the agent of each review step of that task that has passed: the reviewer, and the owning architect on the issue whose acceptance it ran. Send `verdict` approve or changes, `commit` that head, and `summary`; an architect names in `issue` the issue it accepted, and a reviewer leaves `issue` out. It writes no handoff and ends no phase. The daemon takes it only from such an agent (VERDICT_CALLER_FORBIDDEN), only once its own step has passed, never for the step that runs now (VERDICT_NOT_PAST_REVIEW), whose verdict is the reviewer's completion or the owner's sign_off or move back, and only on the pull request's current head (VERDICT_HEAD_NOT_CURRENT). read_build reports the build this process runs: the plugin package and version the process loaded, and the contract that build speaks. It also reports the contract the daemon states on its answer to the health route. That contract is null where the daemon stated none, where the daemon refused the health route and where no answer reached this process at all, and a note of its own says which. The two numbers can differ, and the answer shows both. Report the version this call answers, never the one at the plugin's path on the image. In a pod, an Oh My Pi role relaunched onto the daemon's contract loads the build that relaunch installed under the role's own state home, and a Claude Code role installs no plugin: it loads its pod image's own claude-envoy. read_build reads no project state. What a later phase needs",
  )
  expect(definition.description).toContain("with mcp__legion__envoy_publish.")
})

// The runtime sets `LEGION_ROLE` on every pane before Claude Code starts this server, so the tool
// list Claude Code reads, which the server builds before the pane's claim registers, offers the
// phase ops of the pane's role (`phaseOpsOf`): review_verdict to a review step's agent alone. A
// pane whose environment names no Legion role is one this server cannot tell, so it lists every
// phase op, and the shared executor holds review_verdict by the role the claim registered.
test("lists the phase ops of the pane's role, review_verdict to a review step's agent alone", () => {
  const listed = (env: Record<string, string | undefined>) =>
    (legionToolDefinition(env).inputSchema.properties?.["op"] as { enum?: unknown } | undefined)
      ?.enum
  for (const role of LEGION_ROLES)
    expect(listed({ LEGION_ROLE: role }), role).toEqual([...phaseOpsOf(role)])
  expect(listed({ LEGION_ROLE: "tester" })).not.toContain("review_verdict")
  expect(listed({ LEGION_ROLE: "reviewer" })).toContain("review_verdict")
  for (const env of [{}, { LEGION_ROLE: "" }, { LEGION_ROLE: "deployer" }]) {
    expect(listed(env), JSON.stringify(env)).toEqual([...PHASE_OPS])
  }
})

test("handoff_complete finds the record and commit for the issue's phase and the claim's role, then mints its grant and posts them", async () => {
  const { client, requests } = await daemon()
  const lookups: string[] = []
  const answer = await executeLegionTool(
    {
      daemon: client,
      claim: () => implementer,
      handoffReport: async (phase: LegionPhase, role: LegionRole) => {
        lookups.push(`${phase} ${role}`)
        return {
          commit: "a".repeat(40),
          handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
          handoffVersion: 2,
        }
      },
      branchHead: noBranchHead,
    },
    { op: "handoff_complete", summary: "Implemented it." },
  )
  expect(lookups).toEqual(["implementing implementer"])
  expect(answer).toEqual(await daemonFixture("handoff-complete.json"))
  expect(requests).toEqual([
    { method: "GET", path: "/legion/v1/issues/LEGION-208/record", body: undefined },
    grantRequest,
    {
      method: "POST",
      path: "/legion/v1/handoff/complete",
      body: {
        grantId: "grant-for-one-command",
        summary: "Implemented it.",
        verdict: "",
        commit: "a".repeat(40),
        handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
        handoffVersion: 2,
      },
    },
  ])
})

// A phase completion carries no verdict but the reviewer's on this host as on every other: the
// tool refuses any other role's once the record says the completion is a phase's, and mints and
// posts nothing.
test("a phase completion naming a verdict is refused after the record read, with nothing minted or posted", async () => {
  const { client, requests } = await daemon()
  await expect(
    executeLegionTool(
      {
        daemon: client,
        claim: () => implementer,
        handoffReport: async () => {
          throw new Error("no handoff report expected")
        },
        branchHead: noBranchHead,
      },
      { op: "handoff_complete", summary: "Implemented it.", verdict: "pass" },
    ),
  ).rejects.toThrow(
    new Error(
      "handoff_complete carries no verdict on a phase completion but the reviewer's: Dispatch's check states decide the testing phase",
    ),
  )
  expect(requests.map(({ path }) => path)).toEqual(["/legion/v1/issues/LEGION-208/record"])
})

// The agent that does the retro rates the plan in the completion that ends it, through this tool
// as through every host's: the two fields reach the daemon under its own names.
test("the retro's handoff_complete sends its rating of the plan as planRating and planReason", async () => {
  const { client, requests } = await daemon({ state: stateAtPhase("retro") })
  const lookups: string[] = []
  const standing = "c".repeat(40)
  const reason = "The reviewer's second round found what the plan had left out."
  await executeLegionTool(
    {
      daemon: client,
      claim: () => implementer,
      handoffReport: async (phase: LegionPhase, role: LegionRole) => {
        lookups.push(`${phase} ${role}`)
        return { commit: standing }
      },
      branchHead: noBranchHead,
    },
    { op: "handoff_complete", summary: "Retro done.", plan_rating: 3, plan_reason: reason },
  )
  expect(lookups).toEqual(["retro implementer"])
  expect(requests.at(-1)).toEqual({
    method: "POST",
    path: "/legion/v1/handoff/complete",
    body: {
      grantId: "grant-for-one-command",
      summary: "Retro done.",
      verdict: "",
      commit: standing,
      planRating: 3,
      planReason: reason,
    },
  })
})

// A tester's completion names the step its task returns to, should the check states at the head
// it tested fail its step, through this tool as through every host's (contract 219): `return_to`
// reaches the daemon as its own `returnTo`, beside no verdict.
test("a tester's completion sends return_to as returnTo", async () => {
  const { client, requests } = await daemon({ state: stateAtPhase("testing") })
  const tested = "d".repeat(40)
  await executeLegionTool(
    {
      daemon: client,
      claim: () => ({ ...implementer, role: "tester" }),
      handoffReport: async () => ({ commit: tested }),
      branchHead: noBranchHead,
    },
    { op: "handoff_complete", summary: "Red.", return_to: 1 },
  )
  expect(requests.at(-1)).toEqual({
    method: "POST",
    path: "/legion/v1/handoff/complete",
    body: {
      grantId: "grant-for-one-command",
      summary: "Red.",
      verdict: "",
      commit: tested,
      returnTo: 1,
    },
  })
})

test("a handoff report the pane refuses posts nothing and mints nothing", async () => {
  const { client, requests } = await daemon()
  await expect(
    executeLegionTool(
      {
        daemon: client,
        claim: () => implementer,
        handoffReport: async () => {
          throw new Error(
            "LEGION-208 has no implement handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>`, then complete again",
          )
        },
        branchHead: noBranchHead,
      },
      { op: "handoff_complete", summary: "Done." },
    ),
  ).rejects.toThrow("LEGION-208 has no implement handoff in Dispatch")
  expect(requests.map(({ path }) => path)).toEqual(["/legion/v1/issues/LEGION-208/record"])
})

test("the MCP request's cancellation ends a handoff_complete waiting out a daemon restart", async () => {
  const requests: string[] = []
  const cancelled = new AbortController()
  // The daemon refuses the connection, and the client cancels the request at that moment: no
  // wait starts and nothing more is sent.
  const refusing = (async (input): Promise<Response> => {
    requests.push(new URL(input.toString()).pathname)
    cancelled.abort()
    throw Object.assign(
      new TypeError("Unable to connect. Is the computer able to access the url?"),
      {
        code: "ConnectionRefused",
      },
    )
  }) as typeof fetch
  await expect(
    executeLegionTool(
      {
        daemon: createLegionDaemonClient("http://daemon.test", {
          pluginContract: legion.daemonApiVersion,
          fetch: refusing,
        }),
        claim: () => implementer,
        handoffReport: async () => ({ commit: "a".repeat(40) }),
        branchHead: noBranchHead,
      },
      { op: "handoff_complete", summary: "Done." },
      cancelled.signal,
    ),
  ).rejects.toThrow(
    "handoff_complete: the Legion daemon was unreachable (Unable to connect. Is the computer able to access the url?) and the call was aborted; call it again once the daemon is back",
  )
  expect(requests).toEqual(["/legion/v1/issues/LEGION-208/record"])
})

test("read_record answers the issue's record, and request_backward_move posts with its grant", async () => {
  const { client, requests } = await daemon()
  const runtime = {
    daemon: client,
    claim: () => implementer,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
  }
  const state = (await daemonFixture("state.json")) as { issues: Record<string, unknown> }
  await expect(
    executeLegionTool(runtime, { op: "read_record", issue: "LEGION-208" }),
  ).resolves.toEqual({
    record: state.issues["LEGION-208"],
  })
  await expect(
    executeLegionTool(runtime, { op: "read_record", issue: "LEGION-999" }),
  ).rejects.toThrow("The daemon has no record for LEGION-999")
  await expect(
    executeLegionTool(runtime, {
      op: "request_backward_move",
      to: "planning",
      reason: "The plan misses X.",
    }),
  ).resolves.toEqual({})
  expect(requests.slice(2)).toEqual([
    grantRequest,
    {
      method: "POST",
      path: "/legion/v1/phase/backward",
      body: { grantId: "grant-for-one-command", to: "planning", reason: "The plan misses X." },
    },
  ])
})

test("read_release_records posts the claim's own credential and counts the window; a refusal is the caller's", async () => {
  const { client, requests } = await daemon()
  const runtime = {
    daemon: client,
    claim: () => implementer,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
  }

  await expect(
    executeLegionTool(runtime, { op: "read_release_records", since: "2026-11-01T00:00:00Z" }),
  ).resolves.toMatchObject({
    since: "2026-11-01T00:00:00Z",
    releases: RELEASE_RECORDS.releases,
    counts: {
      releases: 2,
      resumes: 2,
      resumesByKind: { task: 1, message: 1 },
      holds: 1,
      holdsByReason: { background_work: 1 },
    },
    // Five minutes and two hours released; the first wake came within ten minutes of its
    // release, and the two first turns were 60 and 120 seconds after their wakes.
    podHoursReleased: 2.08,
    wokenWithinTenMinutes: { releases: 1, share: 0.5 },
    wakeToFirstTurnSeconds: { measured: 2, withoutFirstTurn: 0, median: 60, p90: 120, max: 120 },
  })
  expect(requests).toEqual([
    {
      method: "POST",
      path: "/legion/v1/claims/release-records",
      body: {
        claimToken: "legion-legion-legion-208-implementer",
        secret: "U2VjcmV0",
        since: "2026-11-01T00:00:00Z",
      },
    },
  ])

  // A window the route could not read is refused before the post, as read_record's missing
  // issue is; the daemon's own refusal reaches the caller with its sentence.
  await expect(
    executeLegionTool(runtime, { op: "read_release_records", since: "yesterday" }),
  ).rejects.toThrow("read_release_records' since is an RFC 3339 time, as 2026-11-02T09:00:00Z is")
  expect(requests).toHaveLength(1)
  const refusing = createLegionDaemonClient("http://daemon.test", {
    pluginContract: legion.daemonApiVersion,
    fetch: (async (_input, _init) =>
      Response.json(
        { error: "the daemon keeps no release records" },
        { status: 500 },
      )) as typeof fetch,
  })
  await expect(
    executeLegionTool(
      { ...runtime, daemon: refusing },
      { op: "read_release_records", since: "2026-11-01T00:00:00Z" },
    ),
  ).rejects.toThrow(
    "POST /legion/v1/claims/release-records failed with 500: the daemon keeps no release records",
  )
})

test("refuses before the claim registered, an operation a phase worker has not, and a field its operation does not take", async () => {
  const { client, requests } = await daemon()
  const handoffReport = async () => ({ commit: "c" })
  await expect(
    executeLegionTool(
      { daemon: client, claim: () => undefined, handoffReport, branchHead: noBranchHead },
      { op: "read_record", issue: "LEGION-208" },
    ),
  ).rejects.toThrow("this session has no Legion claim yet")
  const runtime = {
    daemon: client,
    claim: () => implementer,
    handoffReport,
    branchHead: noBranchHead,
  }
  await expect(
    executeLegionTool(runtime, { op: "set_status", issue: "LEGION-208" }),
  ).rejects.toThrow(
    "op is one of read_record, read_release_records, handoff_complete, request_backward_move, ready_for_update, review_verdict, read_build",
  )
  await expect(
    executeLegionTool(runtime, { op: "read_record", issue: "LEGION-208", summary: "x" }),
  ).rejects.toThrow('read_record does not accept field "summary"')
  await expect(executeLegionTool(runtime, { op: "handoff_complete" })).rejects.toThrow(
    "handoff_complete requires summary",
  )
  expect(requests).toEqual([])
})

// The agent of each review step sends its verdict on a head that moved past review through this
// tool's review_verdict, the shared phase op: a fresh grant with the verdict, the head and the
// summary, posted to `POST /legion/v1/review-verdict`. That agent is the reviewer, which runs on
// Claude Code where `worker_backends` puts it there, and the architect that owns the issue and runs
// its acceptance, whose call names the issue it accepted (the daemon runs every architect on Oh My
// Pi, and this host's executor is the same). Every other claim, the planner's, the implementer's,
// the tester's and the merger's, is refused it by its role before any request.
test("review_verdict posts a review step's agent's verdict and commit, and is refused to every other session before any request", async () => {
  const head = "e".repeat(40)
  const call = {
    op: "review_verdict",
    verdict: "approve",
    commit: head,
    summary: "The retro commit changes no code that runs.",
  }
  const reviewer: ClaimCapability = {
    ...implementer,
    sessionID: "ses_reviewer",
    role: "reviewer",
    claimToken: "legion-legion-legion-208-reviewer",
  }
  const owner: ClaimCapability = {
    ...implementer,
    sessionID: "ses_architect",
    role: "architect",
    claimToken: "legion-legion-legion-208-architect",
  }
  const { client, requests } = await daemon()
  const runtimeOf = (daemon: LegionDaemonClient, claim: ClaimCapability) => ({
    daemon,
    claim: () => claim,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
  })
  await expect(executeLegionTool(runtimeOf(client, reviewer), call)).resolves.toEqual({})
  await expect(
    executeLegionTool(runtimeOf(client, owner), { ...call, issue: "LEGION-210" }),
  ).resolves.toEqual({})
  expect(requests).toEqual([
    { ...grantRequest, body: { ...grantRequest.body, sessionId: "ses_reviewer" } },
    {
      method: "POST",
      path: "/legion/v1/review-verdict",
      body: {
        grantId: "grant-for-one-command",
        verdict: "approve",
        commit: head,
        summary: "The retro commit changes no code that runs.",
      },
    },
    { ...grantRequest, body: { ...grantRequest.body, sessionId: "ses_architect" } },
    {
      method: "POST",
      path: "/legion/v1/review-verdict",
      body: {
        grantId: "grant-for-one-command",
        issue: "LEGION-210",
        verdict: "approve",
        commit: head,
        summary: "The retro commit changes no code that runs.",
      },
    },
  ])

  // Every other claim is refused before any request: the stub records none of them.
  const others = await daemon()
  const roles = LEGION_ROLES.filter((each) => each !== "reviewer" && each !== "architect")
  expect(roles).toEqual(["planner", "implementer", "tester", "merger"])
  for (const role of roles) {
    await expect(
      executeLegionTool(runtimeOf(others.client, { ...implementer, role }), call),
    ).rejects.toThrow(
      new Error(
        `review_verdict is the verdict of a review step's agent, the reviewer or the architect that owns the issue, and this session is the ${role}'s`,
      ),
    )
  }
  expect(others.requests).toEqual([])
})

// R6: a role reads which plugin build its own process runs. The build is this module's own
// manifest, never the image's copy at `/opt/legion/claude-envoy`, which a relaunch never updates.
// For this case alone a read of that copy answers a manifest with a version and a contract of
// its own (withImageManifest), so the answer below fails wherever the build is read from that
// path, whatever the image holds. The case imports `legion-tool.ts` again under the mock and asks
// that copy.
const LEGION_TOOL_LOADED_UNDER_THE_IMAGE_MOCK = "../src/legion-tool?read_build"
test("read_build answers the package and version this process loaded, both contract numbers, and reads no project state", async () => {
  const atImagePath = {
    name,
    version: "0.0.0-image",
    legion: { daemonApiVersion: legion.daemonApiVersion - 1 },
  }
  await withImageManifest("/opt/legion/claude-envoy/package.json", atImagePath, async () => {
    const { executeLegionTool: executeFreshLegionTool } = (await import(
      LEGION_TOOL_LOADED_UNDER_THE_IMAGE_MOCK
    )) as { readonly executeLegionTool: typeof executeLegionTool }
    const { client, requests } = await daemon({ daemonContract: legion.daemonApiVersion + 1 })
    const runtime = {
      daemon: client,
      claim: () => implementer,
      handoffReport: async (): Promise<never> => {
        throw new Error("no handoff report expected")
      },
      branchHead: noBranchHead,
    }

    await expect(executeFreshLegionTool(runtime, { op: "read_build" })).resolves.toEqual({
      plugin: name,
      version,
      pluginContract: legion.daemonApiVersion,
      daemonContract: legion.daemonApiVersion + 1,
      daemonContractNote: null,
    })
    // The health route alone, read for its header: no project state is asked for.
    expect(requests.map(({ path }) => path)).toEqual(["/healthz"])
  })
})

test("read_build says so where the daemon states no contract, and takes no field", async () => {
  const { client, requests } = await daemon()
  const runtime = {
    daemon: client,
    claim: () => implementer,
    handoffReport: async (): Promise<never> => {
      throw new Error("no handoff report expected")
    },
    branchHead: noBranchHead,
  }

  await expect(executeLegionTool(runtime, { op: "read_build" })).resolves.toMatchObject({
    pluginContract: legion.daemonApiVersion,
    daemonContract: null,
    daemonContractNote: "the daemon stated no contract",
  })
  await expect(
    executeLegionTool(runtime, { op: "read_build", issue: "LEGION-208" }),
  ).rejects.toThrow('read_build does not accept field "issue"')
  expect(requests.map(({ path }) => path)).toEqual(["/healthz"])
})
