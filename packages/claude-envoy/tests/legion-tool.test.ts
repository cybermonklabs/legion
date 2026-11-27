import { expect, test } from "bun:test"
import { readFile } from "node:fs/promises"
import { resolve } from "node:path"
import type { LegionRole } from "@legion/contracts"
import { daemonContract } from "@legion/contracts/daemon-contract"
import type { LegionPhase } from "@legion/contracts/legion-api"
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import {
  type ClaimCapability,
  legionToolDescription,
  PHASE_OP_SENTENCES,
} from "@legion/pi-shared/legion-phase-ops"
import { stateAtPhase } from "@legion/pi-shared/test/daemon-state"
import { withImageManifest } from "@legion/pi-shared/test/image-manifest"
import pkg, { name, version } from "../package.json" with { type: "json" }
import * as legionTool from "../src/legion-tool"
import { executeLegionTool, legionToolDefinition } from "../src/legion-tool"

/** The contract this build speaks, as `legion-tool.ts` reads it (`daemonContract`): this
 * checkout's. */
const PLUGIN_CONTRACT = daemonContract(pkg, import.meta.dir)

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
async function daemonFixture(name: string): Promise<unknown> {
  const file = resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name)
  return JSON.parse(await readFile(file, "utf8"))
}

/** What the daemon answers a defect report it recorded: the defect's id, that it was recorded, and
 * the generation of the next change that carries it. */
const DEFECT_RECORDED = { defect: "41", state: "recorded", nextChange: 4 }

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
    "/legion/v1/phase/backward": {},
    "/legion/v1/defects/report": DEFECT_RECORDED,
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
    if (path === "/legion/v1/claims/ready-for-update") return new Response(null, { status: 204 })
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
      pluginContract: PLUGIN_CONTRACT,
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

test("answerLegionTool closes only a step and answers an update without closing a look", async () => {
  const { client } = await daemon({ state: stateAtPhase("testing") })
  const calls: string[] = []
  const runtime = {
    daemon: client,
    claim: () => implementer,
    handoffReport: async () => ({ commit: "c0ffee" }),
    branchHead: async () => "",
  }
  const hook = {
    phaseClosed: () => {
      calls.push("closed")
    },
    updateAnswered: () => {
      calls.push("update")
    },
  }
  await legionTool.answerLegionTool(
    runtime,
    {
      op: "handoff_complete",
      summary: "looked",
      verdict: "pass",
      check_opened_at: "2026-10-19T06:00:00.123456Z",
    },
    hook,
  )
  expect(calls).toEqual([])
  await legionTool.answerLegionTool(
    runtime,
    { op: "handoff_complete", summary: "tested", verdict: "pass" },
    hook,
  )
  expect(calls).toEqual(["closed"])
  await legionTool.answerLegionTool(runtime, { op: "ready_for_update", round: "update-1" }, hook)
  expect(calls).toEqual(["closed", "update"])
})

/** A report_defect call whose every field holds. */
const DEFECT_REPORT = {
  op: "report_defect",
  issue: "LEGION-208",
  defect: "The merged widget drops its last row: `bun test widget` fails on row 12.",
  head: "3f2a9c41d0b7e6a85c1f4d2e9b0a7c6d5e4f3a2b",
}

// report_defect records a defect of the merged change and moves no phase: the role goes on with
// its step, so a report that succeeded closes no phase and answers no update, for the implementer
// in retro and the merger in merging alike.
test("report_defect does not close the phase", async () => {
  const { client, requests } = await daemon()
  const calls: string[] = []
  const hook = {
    phaseClosed: () => {
      calls.push("closed")
    },
    updateAnswered: () => {
      calls.push("update")
    },
  }
  const posted = {
    method: "POST",
    path: "/legion/v1/defects/report",
    body: {
      grantId: "grant-for-one-command",
      issue: "LEGION-208",
      defect: DEFECT_REPORT.defect,
      head: DEFECT_REPORT.head,
    },
  }
  for (const role of ["implementer", "merger"] as const) {
    const runtime = {
      daemon: client,
      claim: () => ({ ...implementer, role }),
      handoffReport: async (): Promise<never> => {
        throw new Error("a defect report reads no handoff")
      },
      branchHead: noBranchHead,
    }
    await expect(legionTool.answerLegionTool(runtime, DEFECT_REPORT, hook)).resolves.toEqual(
      DEFECT_RECORDED,
    )
  }
  expect(calls).toEqual([])
  expect(requests).toEqual([grantRequest, posted, grantRequest, posted])
})

test("a named look of a running issue reaches the daemon through executeLegionTool", async () => {
  const { client, requests } = await daemon({ state: stateAtPhase("implementing") })
  const runtime = {
    daemon: client,
    claim: () => ({ ...implementer, role: "tester" as const }),
    handoffReport: async (): Promise<never> => {
      throw new Error("look reads no handoff")
    },
    branchHead: noBranchHead,
  }
  await expect(
    executeLegionTool(runtime, {
      op: "handoff_complete",
      summary: "checking",
      verdict: "pending",
      wake_at: "2026-10-19T09:00:00Z",
      check_opened_at: "2026-10-19T06:00:00.123456Z",
    }),
  ).resolves.toEqual({})
  expect(requests.map(({ path }) => path)).toEqual([
    "/legion/v1/grants",
    "/legion/v1/handoff/complete",
  ])
  expect(requests.at(-1)?.body).toMatchObject({ checkOpenedAt: "2026-10-19T06:00:00.123456Z" })
})

test("declares the seven phase-worker operations under one op, and names the Envoy publish tool as Claude Code shows it", () => {
  const definition = legionToolDefinition()
  expect(definition.name).toBe("legion")
  expect(definition.inputSchema).toMatchObject({
    properties: {
      op: {
        enum: [
          "read_record",
          "read_release_records",
          "handoff_complete",
          "request_backward_move",
          "ready_for_update",
          "read_build",
          "report_defect",
        ],
      },
      issue: { type: "string" },
      since: { type: "string" },
      summary: { type: "string" },
      verdict: { enum: ["pass", "pending", "fail"] },
      wake_at: { type: "string" },
      plan_rating: { type: "integer", minimum: 1, maximum: 5 },
      plan_reason: { type: "string" },
      return_to: { type: "integer", minimum: 1 },
      reason: { type: "string" },
      round: { type: "string" },
      defect: { type: "string" },
      head: { type: "string" },
      check: { type: "string" },
    },
    required: ["op"],
  })
  // The texts this host passes the shared template: the Envoy publish tool by its name as Claude
  // Code shows it, and the phase-op sentences it puts before and after handoff_complete's
  // contract. Where the template places each is the template's own test
  // (packages/pi-shared/src/legion-phase-ops.test.ts).
  expect(definition.description).toBe(
    legionToolDescription({
      envoyPublish: "mcp__legion__envoy_publish",
      before: PHASE_OP_SENTENCES.readRecord + PHASE_OP_SENTENCES.readReleaseRecords,
      after:
        PHASE_OP_SENTENCES.requestBackwardMove +
        PHASE_OP_SENTENCES.reportDefect +
        PHASE_OP_SENTENCES.readBuild,
    }),
  )
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
    { op: "handoff_complete", summary: "Implemented it.", verdict: "pass" },
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
        verdict: "pass",
        commit: "a".repeat(40),
        handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
        handoffVersion: 2,
      },
    },
  ])
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

// A tester's fail names the step its task returns to through this tool as through every host's
// (contract 219): `return_to` reaches the daemon as its own `returnTo`.
test("a tester's fail sends return_to as returnTo", async () => {
  const { client, requests } = await daemon({ state: stateAtPhase("testing") })
  const tested = "d".repeat(40)
  await executeLegionTool(
    {
      daemon: client,
      claim: () => ({ ...implementer, role: "tester" }),
      handoffReport: async () => ({ commit: tested }),
      branchHead: noBranchHead,
    },
    { op: "handoff_complete", summary: "Red.", verdict: "fail", return_to: 1 },
  )
  expect(requests.at(-1)).toEqual({
    method: "POST",
    path: "/legion/v1/handoff/complete",
    body: {
      grantId: "grant-for-one-command",
      summary: "Red.",
      verdict: "fail",
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
          pluginContract: PLUGIN_CONTRACT,
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
    handoffReport: async (): Promise<never> => {
      throw new Error("no handoff report expected")
    },
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
    handoffReport: async (): Promise<never> => {
      throw new Error("no handoff report expected")
    },
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
    pluginContract: PLUGIN_CONTRACT,
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
    "op is one of read_record, read_release_records, handoff_complete, request_backward_move, ready_for_update, read_build, report_defect",
  )
  await expect(
    executeLegionTool(runtime, { op: "read_record", issue: "LEGION-208", summary: "x" }),
  ).rejects.toThrow('read_record does not accept field "summary"')
  await expect(executeLegionTool(runtime, { op: "handoff_complete" })).rejects.toThrow(
    "handoff_complete requires summary",
  )
  await expect(
    executeLegionTool(runtime, { ...DEFECT_REPORT, head: DEFECT_REPORT.head.slice(0, 12) }),
  ).rejects.toThrow("report_defect's head is the 40-character lower-case commit")
  expect(requests).toEqual([])
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
    legion: { daemonApiVersion: PLUGIN_CONTRACT - 1 },
  }
  await withImageManifest("/opt/legion/claude-envoy/package.json", atImagePath, async () => {
    const { executeLegionTool: executeFreshLegionTool } = (await import(
      LEGION_TOOL_LOADED_UNDER_THE_IMAGE_MOCK
    )) as { readonly executeLegionTool: typeof executeLegionTool }
    const { client, requests } = await daemon({ daemonContract: PLUGIN_CONTRACT + 1 })
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
      pluginContract: PLUGIN_CONTRACT,
      daemonContract: PLUGIN_CONTRACT + 1,
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
    pluginContract: PLUGIN_CONTRACT,
    daemonContract: null,
    daemonContractNote: "the daemon stated no contract",
  })
  await expect(
    executeLegionTool(runtime, { op: "read_build", issue: "LEGION-208" }),
  ).rejects.toThrow('read_build does not accept field "issue"')
  expect(requests.map(({ path }) => path)).toEqual(["/healthz"])
})
