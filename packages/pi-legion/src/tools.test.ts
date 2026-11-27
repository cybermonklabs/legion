import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { type LegionRole, roleToken } from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import {
  LEGION_PHASES,
  LEGION_WORKFLOW_PHASES,
  type LegionPhase,
  LegionProjectP0BoundClearRequest,
  LegionProjectP0BoundRequest,
  type LegionState,
  LegionStateResponse,
} from "@legion/contracts/legion-api";
import { bearerCredential } from "@legion/envoy-client/bearer";
import { DispatchClient, DispatchServiceError } from "@legion/envoy-client/dispatch-http";
import { fakeDispatchFetch } from "@legion/envoy-client/test/fake-dispatch";
import {
  createLegionDaemonClient,
  LegionDaemonApiError,
  type LegionDaemonClient,
} from "@legion/pi-shared/daemon-client";
import {
  findHandoffReport,
  type JjRunner,
  type LatestHandoff,
} from "@legion/pi-shared/handoff-record";
import { phaseOpParameters } from "@legion/pi-shared/legion-phase-ops";
import type { PiApi, RegisteredTool, SessionContext } from "@legion/pi-shared/pi-types";
import { withImageManifest } from "@legion/pi-shared/test/image-manifest";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { z } from "zod";
import pkg from "../package.json";
import { createLegionTool, type LegionToolSession } from "./tools";

/** The contract this build speaks, as `tools.ts` reads it (`daemonContract`): this checkout's. */
const PLUGIN_CONTRACT = daemonContract(pkg, import.meta.dir);

function context(sessionId = "ses_208"): SessionContext {
  return {
    cwd: "/workspace",
    hasUI: true,
    setInterval: () => undefined,
    setTimeout: () => undefined,
    sessionManager: {
      getSessionId: () => sessionId,
      getSessionFile: () => "/sessions/208.jsonl",
      ensureOnDisk: async () => undefined,
      getEntries: () => [],
    },
    ui: { notify: () => undefined },
  };
}

const pi = {
  zod: z,
  sendMessage: () => undefined,
  appendEntry: () => undefined,
  getActiveTools: () => [],
  setActiveTools: async () => undefined,
  on: () => undefined,
  registerTool: () => undefined,
  registerCommand: () => undefined,
  registerMessageRenderer: () => undefined,
} as unknown as PiApi;

/** The Dispatch of a tool that must never read Dispatch. */
const noDispatch = async (): Promise<DispatchClient> => {
  throw new Error("no Dispatch read expected");
};

/** The controller grant of a tool that answers for no controller. */
const noControllerGrant = async (sessionId: string): Promise<never> => {
  throw new Error(`no controller grant expected for ${sessionId}`);
};

/** The handoff report of a tool that must never complete a phase. */
const noHandoffReport = async (phase: LegionPhase, role: LegionRole): Promise<never> => {
  throw new Error(`no handoff report expected for the ${role} at ${phase}`);
};

/** The branch-head lookup of a tool that must never read a branch head. */
const noBranchHead = async (): Promise<string> => {
  throw new Error("branchHead is not expected in this test");
};

/** The claim's registration secret: random, as the daemon's is, since the pane holds it. */
const CLAIM_SECRET = fixtureSecret("claim");
const DISPATCH_TOKEN = fixtureSecret("dispatch");

/** A claim's session on LEGION-208: a root architect's, a sub-architect's (its issue is not its
 * tree), or a phase worker's by role. */
function claimSession(role: LegionRole, issue = "LEGION-208"): LegionToolSession {
  return {
    kind: role === "architect" ? "architect" : "phase-worker",
    role,
    sessionID: "ses_208",
    tree: "LEGION-208",
    issue,
    secret: CLAIM_SECRET,
    claimToken: roleToken("omp", issue, role),
    generation: 1,
  };
}

/** A daemon stub that records every call: `grant` answers a grant, `state` the state given,
 * `record` the issue's record out of that state, `daemonContract` the contract the daemon
 * states, and every other route the answer given for it (an empty object by default). */
function recordingDaemon(options: {
  readonly state?: unknown;
  readonly answers?: Readonly<Record<string, unknown>>;
  readonly daemonContract?: number;
}) {
  const calls: Array<readonly [string, object | undefined]> = [];
  const routes = [
    "gateRegister",
    "waveRelease",
    "phaseBackward",
    "phaseRetry",
    "signOff",
    "rootClose",
    "childRerun",
    "planRegister",
    "taskStop",
    "issueStatus",
    "freshSession",
    "compact",
    "stopAgent",
    "retryAgent",
    "suspendAgent",
    "resumeAgent",
    "issuePodUpdate",
    "intakeReplay",
    "intakeDiscard",
    "handoffComplete",
    "releaseRecords",
  ];
  const daemon = {
    grant: async (input: object) => {
      calls.push(["grant", input]);
      return { grantId: "grant-208", expiresAt: "2099-01-01T00:00:00Z" };
    },
    state: async () => {
      calls.push(["state", undefined]);
      return options.state ?? { issues: {} };
    },
    record: async (issue: string) => {
      calls.push(["record", { issue }]);
      const state = (options.state ?? { issues: {} }) as { issues: Record<string, unknown> };
      return state.issues[issue];
    },
    daemonContract: async () => {
      calls.push(["daemonContract", undefined]);
      return options.daemonContract;
    },
    ...Object.fromEntries(
      routes.map((name) => [
        name,
        async (input: object) => {
          calls.push([name, input]);
          const answer = options.answers?.[name];
          if (answer instanceof Error) throw answer;
          return answer ?? {};
        },
      ])
    ),
  };
  return { calls, daemon: () => daemon as never };
}

/** An answer the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

/** The real daemon client over a daemon that answers each route of `answers` with that golden,
 * or, for a list, each call of the route with the next golden of the list, under `status`, and
 * records what reached it. A tool call over it goes through the contract's request and answer
 * schemas, as it does in a session, and states this build's own contract, as the extension's
 * client does (`extensions/legion.ts`). */
function goldenDaemon(answers: Readonly<Record<string, string | readonly string[]>>, status = 200) {
  const posted: Array<readonly [string, unknown]> = [];
  const calls = new Map<string, number>();
  const stub = (async (input, init) => {
    const route = new URL(input.toString()).pathname;
    posted.push([route, init?.body == null ? undefined : JSON.parse(init.body.toString())]);
    const call = calls.get(route) ?? 0;
    calls.set(route, call + 1);
    const answer = answers[route];
    const golden = typeof answer === "string" ? answer : answer?.[call];
    return golden === undefined
      ? Response.json({ error: `no golden answers call ${call + 1} of ${route}` }, { status: 404 })
      : Response.json(daemonFixture(golden), { status });
  }) as typeof fetch;
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: PLUGIN_CONTRACT,
    fetch: stub,
  });
  return { posted, daemon: () => client };
}

/** Every session kind the tool answers for: a root architect, a sub-architect, each phase worker,
 * and the controller. */
const EVERY_SESSION: readonly LegionToolSession[] = [
  claimSession("architect"),
  claimSession("architect", "LEGION-209"),
  ...(["planner", "implementer", "tester", "reviewer", "merger"] as const).map((role) =>
    claimSession(role)
  ),
  { kind: "controller", sessionID: "ses_controller" },
];

const toolFor = (active: LegionToolSession) =>
  createLegionTool({
    pi,
    daemon: recordingDaemon({}).daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => active,
  });

// The daemon's refusals and notices over children carry its rules for ending a child
// (record.EndChildRule) and its sentence on a status it has not taken yet
// (record.StatusNotTakenYet), and the description gives an architect the same words. The daemon's
// golden test (`packages/daemon/internal/api`, TestTheEndChildRuleGolden) writes both texts to
// `end-child-rule.json`. This case holds the description to the file, so a word changed on either
// side alone fails one test or the other. The first and the last sentence of the rule are written
// out here, so a fixture that holds only a part of the rule fails here too.
test("the legion tool's description carries the daemon's rules for ending a child and its not-taken-yet sentence, word for word", () => {
  const { rule, notTakenYet } = daemonFixture("end-child-rule.json") as {
    readonly rule: string;
    readonly notTakenYet: string;
  };
  expect(rule).toStartWith("End a child with your own status write in Dispatch. ");
  expect(rule).toEndWith(" When the daemon set the move back, write it again.");
  const { description } = toolFor(claimSession("architect"));
  expect(description).toContain(rule);
  expect(description).toContain(notTakenYet);
});

/** The `legion` tool's parameter schema as Oh My Pi agents are given it, serialized whole: every
 * property and every `op` value in the order the tool declares them. */
const OMP_LEGION_TOOL_SCHEMA = JSON.stringify({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  properties: {
    op: {
      type: "string",
      enum: [
        "register_gate",
        "register_plan",
        "stop_task",
        "release_children",
        "request_backward_move",
        "retry_or_escalate",
        "sign_off",
        "close_root",
        "rerun_child",
        "read_record",
        "read_release_records",
        "handoff_complete",
        "ready_for_update",
        "read_build",
        "read_state",
        "set_status",
        "replace_pod",
        "fresh_session",
        "compact",
        "stop",
        "retry",
        "suspend",
        "resume_agent",
        "pause",
        "resume",
        "set_p0_bound",
        "clear_p0_bound",
        "replay_give_up",
        "discard_give_up",
      ],
    },
    issue: { type: "string" },
    since: { type: "string" },
    artifactId: {
      type: "string",
      description:
        "register_gate's document for the step it registers, and register_plan's plan: its artifact id, slug, or filename, as a dispatch command's --artifact takes it",
    },
    version: { type: "number" },
    step: {
      type: "string",
      enum: ["criteria", "spec"],
      description:
        "register_gate's design-gate step: criteria for the issue's Verification document, spec for its primary document. A deployment that gates one step takes the registration without it",
    },
    basis: {
      type: "string",
      description:
        "register_gate's basis: the ancestor's document for the same step, as dispatch://<KEY>/spec, dispatch://<KEY>/artifact/<slug> or its artifact id. basis, basisVersion and reason come together or not at all",
    },
    basisVersion: {
      type: "number",
      description:
        "register_gate's basis version: the version of the basis document a person approved, or the version its open approval request asks a person to approve",
    },
    issues: { type: "array", items: { type: "string" } },
    task: {
      type: "string",
      description:
        "stop_task's task: the id of its plan_task block in the plan Legion runs for the issue, default for the project's default steps",
    },
    to: {
      type: "string",
      enum: [
        "admitted",
        "planning",
        "implementing",
        "testing",
        "reviewing",
        "accepting",
        "retro",
        "merging",
        "done",
        "held",
      ],
    },
    reason: {
      type: "string",
      description:
        "close_root's and request_backward_move's why, retry_or_escalate's optional why, the controller's pause's why, set_p0_bound's and clear_p0_bound's why, register_gate's reason for its basis, and discard_give_up's why, which it requires. No other operation takes it, replay_give_up included. rerun_child takes only issue: say why in a dispatch comment on the child",
    },
    decision: { type: "string", enum: ["retry", "escalate"] },
    summary: { type: "string" },
    verdict: { type: "string", enum: ["pass", "pending", "fail"] },
    wake_at: { type: "string" },
    check_opened_at: { type: "string" },
    plan_rating: { type: "integer", minimum: 1, maximum: 5 },
    plan_reason: { type: "string" },
    // Zod renders a whole number with no stated bound above as the largest safe integer.
    return_to: { type: "integer", minimum: 1, maximum: Number.MAX_SAFE_INTEGER },
    round: { type: "string" },
    status: { type: "string", enum: ["todo", "backlog", "icebox"] },
    phase: { type: "string", enum: [...LEGION_PHASES] },
    settings: { type: "boolean" },
    role: {
      type: "string",
      enum: ["architect", "planner", "implementer", "tester", "reviewer", "merger"],
    },
    giveUp: {
      type: "number",
      description:
        "replay_give_up's and discard_give_up's one give-up, a message the daemon's intake gave up on: its record id, as the intake-gave-up notice names it. Name it, or consumer and throughSeq, never both",
    },
    consumer: {
      type: "string",
      description:
        "replay_give_up's and discard_give_up's intake consumer, as read_state's summary names it on an intake give-ups line: with throughSeq, every open give-up of that consumer at or below that stream sequence",
    },
    throughSeq: {
      type: "number",
      description:
        "With consumer: the highest stream sequence replay_give_up or discard_give_up takes. read_state's summary gives each consumer's newest seq, which takes all of its open give-ups",
    },
    bound: {
      type: "number",
      description:
        "set_p0_bound's bound: how many P0 issues may hold an admission slot at once outside the cap, a whole number from 1 to 2147483647. Name it or unbounded, never both",
    },
    unbounded: {
      type: "boolean",
      description:
        "set_p0_bound's no bound: true sets no P0 bound, whatever legion.yaml's admission_p0_bound says. Name it or bound, never both",
    },
  },
  required: ["op"],
  additionalProperties: false,
});

test("the legion tool's parameter schema serializes to the exact schema Oh My Pi agents are given, for every session", () => {
  for (const active of EVERY_SESSION) {
    expect(JSON.stringify(z.toJSONSchema(toolFor(active).parameters as z.ZodType))).toBe(
      OMP_LEGION_TOOL_SCHEMA
    );
  }
});

// The tool lists its fields by hand, so a field the phase ops take could be left out of it and
// no agent could ever send it. Every field `phaseOpParameters` declares is in the schema.
test("the schema the Oh My Pi tool exposes carries every field the phase ops take, wake_at among them", () => {
  const declared = z.toJSONSchema(toolFor(claimSession("implementer")).parameters as z.ZodType);
  const properties = Object.keys(declared.properties ?? {});
  expect(properties).toContain("wake_at");
  for (const field of Object.keys(phaseOpParameters(z))) expect(properties).toContain(field);
});

test("a root architect's handoff_complete is refused as unavailable before its arguments are read", async () => {
  const root = toolFor(claimSession("architect"));
  for (const parameters of [{}, { summary: "" }, { summary: "x", verdict: "maybe" }]) {
    await expect(
      root.execute("", { op: "handoff_complete", ...parameters }, undefined, undefined, context())
    ).resolves.toEqual({
      content: [
        {
          type: "text",
          text: "handoff_complete is not available to a root architect session, which runs no phase",
        },
      ],
      details: {},
      isError: true,
    });
  }
  // A field handoff_complete never takes is named first, as the operation's field check names it.
  await expect(
    root.execute("", { op: "handoff_complete", phase: "plan" }, undefined, undefined, context())
  ).resolves.toMatchObject({
    content: [
      {
        type: "text",
        text: 'handoff_complete does not accept field "phase"; it takes summary, issue, verdict, wake_at, check_opened_at, plan_rating, plan_reason, return_to; handoff_complete is not available to a root architect session, which runs no phase',
      },
    ],
  });
});

test("the legion tool exposes only the workflow operations each role owns", async () => {
  const state = {
    issues: { "LEGION-208": { key: "LEGION-208", phase: "held" } },
  };
  const { calls, daemon } = recordingDaemon({ state });
  const run = async (session: LegionToolSession, parameters: Record<string, unknown>) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => session,
    }).execute("", parameters, undefined, undefined, context());
  const architect = claimSession("architect");
  const worker = claimSession("implementer");
  const phaseWorkerTool = createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => worker,
  });

  await expect(
    run(architect, {
      op: "register_gate",
      issue: "LEGION-208",
      artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
      version: 7,
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls).toEqual([
    [
      "grant",
      { sessionId: "ses_208", secret: CLAIM_SECRET, tree: "LEGION-208", issue: "LEGION-208" },
    ],
    [
      "gateRegister",
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
    ],
  ]);

  expect(
    (phaseWorkerTool.parameters as z.ZodType).safeParse({
      op: "request_backward_move",
      to: "integrating",
      reason: "test failed",
    }).success
  ).toBeFalse();

  // A phase worker's move is of its own issue, which its grant names, so the request carries no
  // issue; one naming another issue is refused before any mint. The architect's grant serves its
  // whole tree, so its move names the issue it returns from accepting, and is refused without one.
  await expect(
    run(worker, { op: "request_backward_move", to: "implementing", reason: "test failed" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseBackward",
    { grantId: "grant-208", to: "implementing", reason: "test failed" },
  ]);
  await expect(
    run(worker, {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-208",
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseBackward",
    { grantId: "grant-208", to: "implementing", reason: "test failed" },
  ]);
  const beforeRefusedMove = calls.length;
  await expect(
    run(worker, {
      op: "request_backward_move",
      to: "implementing",
      reason: "test failed",
      issue: "LEGION-209",
    })
  ).resolves.toMatchObject({
    isError: true,
    content: [
      {
        type: "text",
        text: expect.stringContaining(
          "a phase worker moves only its own issue LEGION-208, not LEGION-209"
        ),
      },
    ],
  });
  await expect(
    run(architect, { op: "request_backward_move", to: "testing", reason: "the proof is thin" })
  ).resolves.toMatchObject({
    isError: true,
    content: [
      { type: "text", text: expect.stringContaining("request_backward_move requires issue") },
    ],
  });
  expect(calls).toHaveLength(beforeRefusedMove);
  await expect(
    run(architect, {
      op: "request_backward_move",
      to: "testing",
      reason: "the proof is thin",
      issue: "LEGION-209",
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseBackward",
    { grantId: "grant-208", to: "testing", reason: "the proof is thin", issue: "LEGION-209" },
  ]);
  await expect(
    run(architect, { op: "release_children", issues: ["LEGION-209"] })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual(["waveRelease", { grantId: "grant-208", issues: ["LEGION-209"] }]);
  await expect(
    run(architect, { op: "retry_or_escalate", issue: "LEGION-208", decision: "retry" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseRetry",
    { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
  ]);
  await expect(run(architect, { op: "sign_off", issue: "LEGION-208" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["signOff", { grantId: "grant-208", issue: "LEGION-208" }]);
  await expect(
    run(architect, { op: "close_root", issue: "LEGION-208", reason: "no change" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "rootClose",
    { grantId: "grant-208", issue: "LEGION-208", reason: "no change" },
  ]);
  await expect(run(architect, { op: "close_root", issue: "LEGION-208" })).resolves.toMatchObject({
    isError: true,
  });
  await expect(run(architect, { op: "rerun_child", issue: "LEGION-209" })).resolves.toMatchObject({
    details: {},
  });
  expect(calls.at(-1)).toEqual(["childRerun", { grantId: "grant-208", issue: "LEGION-209" }]);
  await expect(run(architect, { op: "read_record", issue: "LEGION-208" })).resolves.toMatchObject({
    details: { record: state.issues["LEGION-208"] },
  });
  for (const op of ["sign_off", "close_root", "rerun_child"]) {
    await expect(run(worker, { op, issue: "LEGION-208" })).resolves.toMatchObject({
      isError: true,
    });
  }
  // The controller's operations belong to no claim, and nothing schedules a worker.
  for (const session of [architect, worker]) {
    await expect(run(session, { op: "read_state" })).resolves.toMatchObject({
      isError: true,
      content: [
        {
          type: "text",
          text: `read_state is not available to ${session.kind === "architect" ? "an" : "a"} ${session.kind} session`,
        },
      ],
    });
    await expect(
      run(session, { op: "set_status", issue: "LEGION-208", status: "todo" })
    ).resolves.toMatchObject({ isError: true });
    await expect(run(session, { op: "replace_pod", issue: "LEGION-208" })).resolves.toMatchObject({
      isError: true,
      content: [
        {
          type: "text",
          text: `replace_pod is not available to ${session.kind === "architect" ? "an" : "a"} ${session.kind} session`,
        },
      ],
    });
  }
  await expect(run(architect, { op: "spawn_worker", issue: "LEGION-208" })).resolves.toMatchObject({
    isError: true,
  });
});

// The release carries why it was made: the architect's retry_or_escalate takes an optional
// reason, which the daemon writes into the task the restarted role is given and into its own log
// line for the release. The field has to be declared in three places or the call is refused two
// layers above the daemon, so this reads the whole way through: the tool accepts it, the request
// carries it, and a call that names none is the call it was before.
test("retry_or_escalate takes a reason", async () => {
  const { calls, daemon } = recordingDaemon({});
  const architect = claimSession("architect");
  const run = async (parameters: Record<string, unknown>) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => architect,
    }).execute("", parameters, undefined, undefined, context());

  await expect(
    run({
      op: "retry_or_escalate",
      issue: "LEGION-208",
      decision: "retry",
      reason: "the pod ran out of disk; its node is drained",
    })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseRetry",
    {
      grantId: "grant-208",
      issue: "LEGION-208",
      decision: "retry",
      reason: "the pod ran out of disk; its node is drained",
    },
  ]);

  // The reason is the daemon's own where the call names none: the request carries no key at all,
  // so nothing below reads an empty one as a reason given.
  await expect(
    run({ op: "retry_or_escalate", issue: "LEGION-208", decision: "escalate" })
  ).resolves.toMatchObject({ details: {} });
  expect(calls.at(-1)).toEqual([
    "phaseRetry",
    { grantId: "grant-208", issue: "LEGION-208", decision: "escalate" },
  ]);

  // An empty reason is no reason: refused before the grant is minted, as every other empty
  // required field of an operation is.
  const before = calls.length;
  await expect(
    run({ op: "retry_or_escalate", issue: "LEGION-208", decision: "retry", reason: "" })
  ).resolves.toMatchObject({
    isError: true,
    content: [{ type: "text", text: "retry_or_escalate requires reason" }],
  });
  expect(calls).toHaveLength(before);
});

// Architects sent a child operation a `reason`, as close_root and request_backward_move take one,
// and were told only that the field is refused: five refusals over four days of architect
// sessions, each sent again without it and its why lost. The daemon keeps no why for a rerun, so a
// refusal of its `reason` names the fields the operation takes and the dispatch comment on the
// child the why goes in, before any grant is minted. Any other operation's refusal names the
// fields it takes alone: it may name no single issue (release_children, whose field is `issues`).
test("rerun_child refuses a reason before any grant, naming issue and the dispatch comment the why goes in", async () => {
  const { calls, daemon } = recordingDaemon({});
  const tool = createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("architect"),
  });
  const refusals = [
    [
      { op: "rerun_child", issue: "LEGION-209", reason: "The spec changed" },
      'rerun_child does not accept field "reason"; it takes issue. Say why in a dispatch comment on the child',
    ],
    // Every field the operation does not take is named, as a phase op's refusal names them.
    [
      { op: "sign_off", issue: "LEGION-208", reason: "approved", note: "x" },
      'sign_off does not accept field "reason"; sign_off does not accept field "note"; it takes issue',
    ],
    [
      { op: "release_children", issues: ["LEGION-209"], reason: "wave 2" },
      'release_children does not accept field "reason"; it takes issues',
    ],
    // A field other than `reason` is refused naming what the operation takes, and nothing more.
    [
      { op: "register_gate", issue: "LEGION-208", artifactId: "spec", version: 1, steps: "spec" },
      'register_gate does not accept field "steps"; it takes issue, artifactId, version, step, basis, basisVersion, reason',
    ],
    [
      { op: "register_plan", issue: "LEGION-209", artifactId: "plan", version: 1, step: "spec" },
      'register_plan does not accept field "step"; it takes issue, artifactId, version',
    ],
    [
      { op: "stop_task", issue: "LEGION-209", task: "a", reason: "the plan changed" },
      'stop_task does not accept field "reason"; it takes issue, task',
    ],
  ] as const;
  for (const [parameters, text] of refusals) {
    await expect(tool.execute("", parameters, undefined, undefined, context())).resolves.toEqual({
      content: [{ type: "text", text }],
      details: {},
      isError: true,
    });
  }
  expect(calls).toEqual([]);
});
// The records the production check of a deployment's idle release counts, read by the pod's own
// claim: the daemon takes the claim token and the claim's secret, which stay in this process.
test("read_release_records reads the window with this session's claim, and reports a refusal as read_record does", async () => {
  const records = {
    releases: [
      {
        id: 1,
        issue: "LEGION-208",
        generation: 2,
        roles: ["tester"],
        at: "2026-11-01T12:00:00Z",
        bundleBytes: 64,
      },
    ],
    resumes: [
      {
        id: 1,
        release: 1,
        issue: "LEGION-208",
        role: "tester",
        kind: "task",
        at: "2026-11-01T12:30:00Z",
        podReadyAt: "2026-11-01T12:31:00Z",
        firstTurnAt: "2026-11-01T12:32:00Z",
        stuck: false,
      },
    ],
    holds: [],
  };
  const { calls, daemon } = recordingDaemon({ answers: { releaseRecords: records } });
  const toolFor = (session: LegionToolSession, answering = daemon) =>
    createLegionTool({
      pi,
      daemon: answering,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => session,
    });
  const window = { op: "read_release_records", since: "2026-11-01T00:00:00Z" };

  for (const session of [claimSession("tester"), claimSession("architect")]) {
    await expect(
      toolFor(session).execute("", window, undefined, undefined, context())
    ).resolves.toMatchObject({
      details: {
        counts: { releases: 1, resumes: 1, resumesByKind: { task: 1 }, holds: 0 },
        wokenWithinTenMinutes: { releases: 0, share: 0 },
        wakeToFirstTurnSeconds: { measured: 1, median: 120, p90: 120, max: 120 },
      },
    });
    expect(calls.at(-1)).toEqual([
      "releaseRecords",
      {
        claimToken: roleToken(
          "omp",
          "LEGION-208",
          session.kind === "architect" ? "architect" : "tester"
        ),
        secret: CLAIM_SECRET,
        since: "2026-11-01T00:00:00Z",
      },
    ]);
  }

  // A window the route could not read is refused here, before the request, as a field problem of
  // any other op is; a refusal from the daemon reaches the agent as its own sentence.
  const before = calls.length;
  await expect(
    toolFor(claimSession("tester")).execute(
      "",
      { op: "read_release_records", since: "yesterday" },
      undefined,
      undefined,
      context()
    )
  ).resolves.toMatchObject({
    isError: true,
    content: [
      {
        type: "text",
        text: "read_release_records' since is an RFC 3339 time, as 2026-11-02T09:00:00Z is",
      },
    ],
  });
  expect(calls).toHaveLength(before);
  const refusing = recordingDaemon({
    answers: { releaseRecords: new Error("the daemon keeps no release records") },
  });
  await expect(
    toolFor(claimSession("tester"), refusing.daemon).execute(
      "",
      window,
      undefined,
      undefined,
      context()
    )
  ).resolves.toMatchObject({
    isError: true,
    content: [{ type: "text", text: "the daemon keeps no release records" }],
  });
});

// Every role reads one description, and the daemon accepts a verdict from the tester alone
// (VERDICT_ROLE_FORBIDDEN): it may not leave another role to infer from its own instructions that
// it reports one.
test("the legion tool's description gives a verdict to the tester alone", () => {
  const tool = createLegionTool({
    pi,
    daemon: recordingDaemon({}).daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("reviewer"),
  });
  expect(tool.description).toContain("`verdict` pass|fail from the tester alone");
  expect(tool.description).not.toContain("when your role's instructions require one");
});

test("a workflow refusal tells the agent both its code and message", async () => {
  const refusal = Object.assign(
    new Error(
      "POST /legion/v1/signoff failed with 409 SIGNOFF_OUTSIDE_ACCEPTING: LEGION-208 is in phase reviewing, not accepting; this sign-off changed nothing"
    ),
    {
      code: "SIGNOFF_OUTSIDE_ACCEPTING",
      detail: "LEGION-208 is in phase reviewing, not accepting; this sign-off changed nothing",
    }
  );
  const tool = createLegionTool({
    pi,
    daemon: (() => ({
      grant: async () => ({ grantId: "grant-208" }),
      signOff: async () => Promise.reject(refusal),
    })) as never,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("architect"),
  });

  await expect(
    tool.execute("", { op: "sign_off", issue: "LEGION-208" }, undefined, undefined, context())
  ).resolves.toMatchObject({
    content: [{ type: "text", text: expect.stringContaining("SIGNOFF_OUTSIDE_ACCEPTING") }],
    isError: true,
    details: {},
  });
});

/** The root LEGION-208's spec (version 4) and Verification document (version 2), and its
 * ancestor LEGION-100's spec (version 3) and Verification document (version 5). */
const ROOT_SPEC = "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093";
const ROOT_VERIFICATION = "3b9e7f10-5c2d-4e8a-9f61-0a7d2c4b8e15";
const ANCESTOR_SPEC = "6f4a2c8e-1b3d-4f5a-8c7e-9d0b1a2c3e4f";
const ANCESTOR_VERIFICATION = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d";

/** The question the root's spec puts to a person in its one decision block. */
const OPEN_QUESTION = "Which store holds the export?";

/**
 * A Dispatch holding the root LEGION-208 and its ancestor LEGION-100, each with a spec and a
 * Verification document. The root's spec holds one decision block, open at its version 4, where
 * `openBlock`, and none otherwise; its Verification document holds none. `paths` is every request
 * the tool sent it.
 */
function treeDispatch(openBlock = false) {
  const document = (id: string, slug: string, name: string, primary: boolean, latest: number) => ({
    id,
    slug,
    name,
    kind: "doc",
    primary,
    approval: { state: "draft", latest_version: latest },
  });
  const board = fakeDispatchFetch({
    routes: {
      "/api/v1/issues/LEGION-208": {
        key: "LEGION-208",
        primary_artifact_id: ROOT_SPEC,
        artifacts: [
          document(ROOT_SPEC, "spec", "spec.md", true, 4),
          document(ROOT_VERIFICATION, "verification-md", "verification.md", false, 2),
        ],
      },
      "/api/v1/issues/LEGION-100": {
        key: "LEGION-100",
        primary_artifact_id: ANCESTOR_SPEC,
        artifacts: [
          document(ANCESTOR_SPEC, "spec", "spec.md", true, 3),
          document(ANCESTOR_VERIFICATION, "verification-md", "verification.md", false, 5),
        ],
      },
      [`/api/v1/artifacts/${ROOT_SPEC}/blocks`]: openBlock
        ? [{ id: "b-1", type: "ask", from: 0, to: 90 }]
        : [],
      [`/api/v1/artifacts/${ROOT_VERIFICATION}/blocks`]: [],
      "/api/v1/issues/LEGION-208/asks": [
        {
          id: "ask-b-1",
          kind: "question",
          block_id: "b-1",
          block_artifact: { id: ROOT_SPEC },
          state: "open",
          question: OPEN_QUESTION,
        },
      ],
      [`/api/v1/artifacts/${ROOT_SPEC}/versions/4`]: {
        number: 4,
        markdown: `## Export\n\n:::ask{#b-1 urgency="med" multiple="false" state="open"}\n${OPEN_QUESTION}\n:::`,
      },
    },
  });
  return {
    paths: board.paths,
    dispatch: async () =>
      new DispatchClient("http://dispatch.test", bearerCredential(DISPATCH_TOKEN), board.fetchImpl),
  };
}

/** The legion tool of the session on `sessionIssue`, a root architect's on LEGION-208 by default,
 * over `daemon` and the pane's `dispatch`. */
const gateTool = (
  daemon: () => LegionDaemonClient,
  dispatch: () => Promise<DispatchClient | undefined>,
  sessionIssue = "LEGION-208"
) =>
  createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch,
    session: () => claimSession("architect", sessionIssue),
  });

/** The bodies of every registration the daemon was sent, in order. */
const registrations = (calls: ReadonlyArray<readonly [string, object | undefined]>) =>
  calls.filter(([route]) => route === "gateRegister").map(([, body]) => body);

test("register_gate takes the document reference the Dispatch tools take, and registers its id", async () => {
  const { calls, daemon } = recordingDaemon({});
  const { paths, dispatch } = treeDispatch();
  const run = (
    artifactId: string,
    issue = "LEGION-208",
    sessionIssue = "LEGION-208",
    pane: () => Promise<DispatchClient | undefined> = dispatch
  ) =>
    gateTool(daemon, pane, sessionIssue).execute(
      "",
      { op: "register_gate", issue, artifactId, version: 4 },
      undefined,
      undefined,
      context()
    );
  const registration = {
    grantId: "grant-208",
    issue: "LEGION-208",
    artifactId: ROOT_SPEC,
    version: 4,
  };

  // A reference is looked up and its id registered; an id goes to the daemon as it is.
  await expect(run("spec")).resolves.toMatchObject({ details: {} });
  await expect(run(ROOT_SPEC)).resolves.toMatchObject({ details: {} });
  expect(registrations(calls)).toEqual([registration, registration]);
  expect(paths).toEqual(["/api/v1/issues/LEGION-208"]);

  // A reference the lookup refuses registers nothing, and so does one in a pane with no
  // Dispatch. So does a call from anyone but the tree root's own architect, refused before any
  // lookup: a sub-architect, or the root architect naming another issue.
  const sent = calls.length;
  const root = "the design gate belongs to the tree root LEGION-208";
  const refusals = [
    [
      await run("notes"),
      'register_gate could not look up document \\"notes\\" on LEGION-208 in Dispatch: document \\"notes\\" not found by slug',
    ],
    [
      await run("spec", "LEGION-208", "LEGION-208", async () => undefined),
      'register_gate cannot look up document \\"spec\\" on LEGION-208: this pane has no Dispatch configured; pass the document\'s id',
    ],
    [await run("spec", "LEGION-208", "LEGION-209"), `${root}; its root architect registers it`],
    [await run("spec", "LEGION-209"), `${root}; register it there`],
  ] as const;
  for (const [result, message] of refusals) {
    expect(result.isError).toBe(true);
    expect(JSON.stringify(result.content)).toContain(message);
  }
  expect(calls).toHaveLength(sent);
  expect(paths).toEqual(["/api/v1/issues/LEGION-208", "/api/v1/issues/LEGION-208"]);
});

// A basis names the ancestor's document for the same step (dispatch://INFRA-3320, R1). Its
// reference names its own issue, the ancestor's, so the tool looks it up there, never on the root;
// an artifact id goes to the daemon as it is. The root's own document is read for an open
// decision block first, as `dispatch request-approval` reads it.
test("register_gate resolves a basis reference on the issue it names, and sends the basis, its version and the reason", async () => {
  const { calls, daemon } = recordingDaemon({});
  const { paths, dispatch } = treeDispatch();
  const reason = "LEGION-208 is a view on LEGION-100: its checks serve P2 of the approved spec.";
  const register = async (
    artifactId: string,
    version: number,
    step: string,
    basis: string,
    basisVersion: number
  ) => {
    const read = paths.length;
    const result = await gateTool(daemon, dispatch).execute(
      "",
      {
        op: "register_gate",
        issue: "LEGION-208",
        artifactId,
        version,
        step,
        basis,
        basisVersion,
        reason,
      },
      undefined,
      undefined,
      context()
    );
    expect(result).toMatchObject({ details: {} });
    expect(result.isError).toBeUndefined();
    return paths.slice(read);
  };

  expect(await register("spec", 4, "spec", "dispatch://LEGION-100/spec", 3)).toEqual([
    "/api/v1/issues/LEGION-208",
    `/api/v1/artifacts/${ROOT_SPEC}/blocks`,
    "/api/v1/issues/LEGION-100",
  ]);
  expect(
    await register(
      "verification-md",
      2,
      "criteria",
      "dispatch://LEGION-100/artifact/verification-md@v5",
      5
    )
  ).toEqual([
    "/api/v1/issues/LEGION-208",
    `/api/v1/artifacts/${ROOT_VERIFICATION}/blocks`,
    "/api/v1/issues/LEGION-100",
  ]);
  expect(await register(ROOT_SPEC, 4, "spec", ANCESTOR_SPEC, 3)).toEqual([
    "/api/v1/issues/LEGION-208",
    `/api/v1/artifacts/${ROOT_SPEC}/blocks`,
  ]);

  const registered = { grantId: "grant-208", issue: "LEGION-208", reason };
  expect(registrations(calls)).toEqual([
    {
      ...registered,
      artifactId: ROOT_SPEC,
      version: 4,
      step: "spec",
      basisArtifactId: ANCESTOR_SPEC,
      basisVersion: 3,
    },
    {
      ...registered,
      artifactId: ROOT_VERIFICATION,
      version: 2,
      step: "criteria",
      basisArtifactId: ANCESTOR_VERIFICATION,
      basisVersion: 5,
    },
    {
      ...registered,
      artifactId: ROOT_SPEC,
      version: 4,
      step: "spec",
      basisArtifactId: ANCESTOR_SPEC,
      basisVersion: 3,
    },
  ]);
});

// A basis reference names its issue, so an artifact id it carries is looked up on that issue as a
// slug is: the id of another issue's document refuses the call, naming the id and the issue the
// reference names, before a grant is minted or the daemon is asked anything. The same reference
// over that issue's own document registers it.
test("register_gate refuses a basis reference whose artifact id is no document of the issue it names", async () => {
  const { calls, daemon } = recordingDaemon({});
  const { paths, dispatch } = treeDispatch();
  const register = (basis: string) =>
    gateTool(daemon, dispatch).execute(
      "",
      {
        op: "register_gate",
        issue: "LEGION-208",
        artifactId: "spec",
        version: 4,
        step: "spec",
        basis,
        basisVersion: 3,
        reason: "LEGION-208's checks serve P2 of LEGION-100's approved spec.",
      },
      undefined,
      undefined,
      context()
    );

  await expect(register(`dispatch://LEGION-100/artifact/${ROOT_SPEC}`)).resolves.toEqual({
    content: [
      {
        type: "text",
        text: `register_gate could not look up document "${ROOT_SPEC}" on LEGION-100 in Dispatch: document "${ROOT_SPEC}" not found by slug; this issue's documents: spec (spec.md), verification-md (verification.md)`,
      },
    ],
    details: {},
    isError: true,
  });
  expect(calls).toEqual([]);
  expect(paths).toEqual([
    "/api/v1/issues/LEGION-208",
    `/api/v1/artifacts/${ROOT_SPEC}/blocks`,
    "/api/v1/issues/LEGION-100",
  ]);

  await expect(register(`dispatch://LEGION-100/artifact/${ANCESTOR_SPEC}`)).resolves.toMatchObject({
    details: {},
  });
  expect(registrations(calls)).toEqual([
    {
      grantId: "grant-208",
      issue: "LEGION-208",
      artifactId: ROOT_SPEC,
      version: 4,
      step: "spec",
      basisArtifactId: ANCESTOR_SPEC,
      basisVersion: 3,
      reason: "LEGION-208's checks serve P2 of LEGION-100's approved spec.",
    },
  ]);
});

// basis, basisVersion and reason come together or not at all, as the daemon's BASIS_INCOMPLETE
// says: a call that names some of them, or a basis that names no issue's document, is refused
// whole before the tool reads Dispatch or mints a grant.
test("register_gate refuses a basis named in part, or one that names no issue's document, before Dispatch or the daemon is asked anything", async () => {
  const { calls, daemon } = recordingDaemon({});
  const { paths, dispatch } = treeDispatch();
  const tool = gateTool(daemon, dispatch);
  const reason = "LEGION-208's checks serve P2 of LEGION-100's approved spec.";
  const forms =
    "register_gate's basis is the ancestor's document as dispatch://<KEY>/spec, dispatch://<KEY>/artifact/<slug> or its artifact id";
  const together = "register_gate takes basis, basisVersion and reason together or not at all";
  const refusals = [
    [
      { basis: "dispatch://LEGION-100/spec" },
      `${together}; this call names basis without basisVersion and reason`,
    ],
    [
      { basis: "dispatch://LEGION-100/spec", basisVersion: 3 },
      `${together}; this call names basis and basisVersion without reason`,
    ],
    [
      { basisVersion: 3, reason },
      `${together}; this call names basisVersion and reason without basis`,
    ],
    [{ reason }, `${together}; this call names reason without basis and basisVersion`],
    [
      { basis: "dispatch://LEGION-100/spec", basisVersion: 0, reason },
      "register_gate's basisVersion is a positive integer",
    ],
    [{ basis: "spec", basisVersion: 3, reason }, `${forms}, not "spec"`],
    [
      { basis: "dispatch://LEGION-100/spec#b-p2", basisVersion: 3, reason },
      `${forms}, not "dispatch://LEGION-100/spec#b-p2"`,
    ],
    [
      { basis: "dispatch://LEGION-100/artifact/verification-md@v4", basisVersion: 5, reason },
      "register_gate's basis dispatch://LEGION-100/artifact/verification-md@v4 names version 4, not basisVersion 5",
    ],
  ] as const;
  for (const [basis, text] of refusals) {
    await expect(
      tool.execute(
        "",
        { op: "register_gate", issue: "LEGION-208", artifactId: "spec", version: 4, ...basis },
        undefined,
        undefined,
        context()
      )
    ).resolves.toEqual({ content: [{ type: "text", text }], details: {}, isError: true });
  }
  expect(calls).toEqual([]);
  expect(paths).toEqual([]);
});

// A basis opens the step with no person's review of the root's own document, so a question that
// document still puts to a person refuses the call with the test `dispatch request-approval`
// makes, and the daemon is asked nothing: no grant, no registration. A registration with no basis
// opens the step only on a person's approval, which the approval request's own refusal guards.
test("register_gate refuses a basis while the root's document holds an open decision block, before the daemon is asked anything", async () => {
  const { calls, daemon } = recordingDaemon({});
  const { paths, dispatch } = treeDispatch(true);
  const basisCall = {
    op: "register_gate",
    issue: "LEGION-208",
    artifactId: "spec",
    version: 4,
    step: "spec",
    basis: "dispatch://LEGION-100/spec",
    basisVersion: 3,
    reason: "LEGION-208's checks serve P2 of LEGION-100's approved spec.",
  };

  const refused = await gateTool(daemon, dispatch).execute(
    "",
    basisCall,
    undefined,
    undefined,
    context()
  );
  expect(refused.isError).toBe(true);
  const [refusal] = refused.content;
  const lines = refusal?.type === "text" ? refusal.text.split("\n") : [];
  expect(lines.slice(0, 2)).toEqual([
    "legion register_gate was not called: spec.md (version 4) has 1 open decision block. A basis opens the step with no person's review of this document, so its open question would go unanswered, and answering one writes a new version, which this registration does not name.",
    `- ${JSON.stringify(OPEN_QUESTION)} (block b-1, ask ask-b-1)`,
  ]);
  expect(lines[2]).toStartWith("Do not register on a basis over an open block.");
  expect(calls).toEqual([]);
  // The root's document was read as the approval request reads it; the ancestor never was.
  expect([...paths].sort()).toEqual(
    [
      "/api/v1/issues/LEGION-208",
      `/api/v1/artifacts/${ROOT_SPEC}/blocks`,
      "/api/v1/issues/LEGION-208/asks",
      `/api/v1/artifacts/${ROOT_SPEC}/versions/4`,
    ].sort()
  );

  // A pane with no Dispatch cannot make that test, so it registers no basis either.
  await expect(
    gateTool(daemon, async () => undefined).execute("", basisCall, undefined, undefined, context())
  ).resolves.toEqual({
    content: [
      {
        type: "text",
        text: "register_gate cannot register LEGION-208 on a basis: this pane has no Dispatch configured, where the tool reads the root's document for an open decision block",
      },
    ],
    details: {},
    isError: true,
  });
  expect(calls).toEqual([]);

  // The same document registered with no basis goes to the daemon, which opens the step only on
  // a person's approval of it.
  const read = paths.length;
  await expect(
    gateTool(daemon, dispatch).execute(
      "",
      { op: "register_gate", issue: "LEGION-208", artifactId: ROOT_SPEC, version: 4, step: "spec" },
      undefined,
      undefined,
      context()
    )
  ).resolves.toMatchObject({ details: {} });
  expect(registrations(calls)).toEqual([
    { grantId: "grant-208", issue: "LEGION-208", artifactId: ROOT_SPEC, version: 4, step: "spec" },
  ]);
  expect(paths).toHaveLength(read);
});

// A basis registers a specific saved version, and an answer settles into a new version that ends
// the basis: unlike `dispatch request-approval` (whose open request carries forward), a basis waits
// until the saved version itself records each ask block — own ask or copy — settled.
test("register_gate on a basis waits for the saved version to record an answered ask or copy settled", async () => {
  const basisCall = {
    op: "register_gate",
    issue: "LEGION-208",
    artifactId: "spec",
    version: 4,
    step: "spec",
    basis: "dispatch://LEGION-100/spec",
    basisVersion: 3,
    reason: "LEGION-208's checks serve P2 of LEGION-100's approved spec.",
  };
  const document = (id: string, slug: string, name: string, primary: boolean, latest: number) => ({
    id,
    slug,
    name,
    kind: "doc",
    primary,
    approval: { state: "draft", latest_version: latest },
  });
  const runWith = async (scenario: {
    readonly asks: readonly unknown[];
    readonly versionOpener: string;
    readonly liveAsks: readonly unknown[];
  }) => {
    const { calls, daemon } = recordingDaemon({});
    const board = fakeDispatchFetch({
      routes: {
        "/api/v1/issues/LEGION-208": {
          key: "LEGION-208",
          primary_artifact_id: ROOT_SPEC,
          artifacts: [document(ROOT_SPEC, "spec", "spec.md", true, 4)],
        },
        "/api/v1/issues/LEGION-100": {
          key: "LEGION-100",
          primary_artifact_id: ANCESTOR_SPEC,
          artifacts: [document(ANCESTOR_SPEC, "spec", "spec.md", true, 3)],
        },
        [`/api/v1/artifacts/${ROOT_SPEC}/blocks`]: [{ id: "b-1", type: "ask", from: 0, to: 90 }],
        "/api/v1/issues/LEGION-208/asks": scenario.asks,
        [`/api/v1/artifacts/${ROOT_SPEC}/versions/4`]: {
          number: 4,
          markdown: `## Export\n\n${scenario.versionOpener}\n${OPEN_QUESTION}\n:::`,
        },
        [`/api/v1/artifacts/${ROOT_SPEC}/text`]: {
          markdown: `## Export\n\n:::ask{#b-1 urgency="med" multiple="false"}\n${OPEN_QUESTION}\n:::`,
          token: "sha256:live",
          version: 4,
          asks: scenario.liveAsks,
        },
      },
    });
    const dispatch = async () =>
      new DispatchClient("http://dispatch.test", bearerCredential(DISPATCH_TOKEN), board.fetchImpl);
    const result = await gateTool(daemon, dispatch).execute(
      "",
      basisCall,
      undefined,
      undefined,
      context()
    );
    const [first] = result.content;
    return {
      calls,
      isError: result.isError === true,
      text: first?.type === "text" ? first.text : "",
    };
  };
  const ownAsk = (state: "open" | "answered") => ({
    id: "ask-b-1",
    kind: "question",
    block_id: "b-1",
    block_artifact: { id: ROOT_SPEC },
    state,
    question: OPEN_QUESTION,
  });
  const answeredLive = {
    block_id: "b-1",
    state: "answered",
    answered_by: "alice",
    answered_at: "2026-11-20T10:00:00Z",
    selected: [],
  };
  const statelessOpener = ':::ask{#b-1 urgency="med" multiple="false"}';
  const answeredOpener = ':::ask{#b-1 urgency="med" multiple="false" state="answered"}';
  const copySource = {
    copied_from: "ask-source",
    copied_from_document: "dispatch://LEGION-100/spec",
  };
  const copyOpener = (state?: string) =>
    `:::ask{#b-1 urgency="med" multiple="false"${state === undefined ? "" : ` state="${state}"`} copied_from="${copySource.copied_from}" copied_from_document="${copySource.copied_from_document}"}`;

  // Control: an unanswered own ask is refused before calling the daemon.
  const unansweredOwn = await runWith({
    asks: [ownAsk("open")],
    versionOpener: statelessOpener,
    liveAsks: [{ block_id: "b-1", state: "open" }],
  });
  expect(unansweredOwn.isError).toBe(true);
  expect(unansweredOwn.calls).toEqual([]);
  expect(unansweredOwn.text).toContain("legion register_gate was not called");
  expect(unansweredOwn.text).toContain("(block b-1, ask ask-b-1)");
  expect(unansweredOwn.text).not.toContain("still open in version 4");

  // An answered own ask whose saved version holds no state is refused, naming the wait.
  const unsettledOwn = await runWith({
    asks: [ownAsk("answered")],
    versionOpener: statelessOpener,
    liveAsks: [answeredLive],
  });
  expect(unsettledOwn.isError).toBe(true);
  expect(unsettledOwn.calls).toEqual([]);
  expect(unsettledOwn.text).toContain("answered but still open in version 4");
  expect(unsettledOwn.text).toContain("wait for that version");

  // Once the saved version records state="answered", registration proceeds.
  const settledOwn = await runWith({
    asks: [ownAsk("answered")],
    versionOpener: answeredOpener,
    liveAsks: [answeredLive],
  });
  expect(settledOwn.isError).toBe(false);
  expect(registrations(settledOwn.calls)).toHaveLength(1);

  // Control: an open copy is refused with its source's ask.
  const openCopy = await runWith({
    asks: [],
    versionOpener: copyOpener(),
    liveAsks: [{ block_id: "b-1", state: "open", ...copySource }],
  });
  expect(openCopy.isError).toBe(true);
  expect(openCopy.calls).toEqual([]);
  expect(openCopy.text).toContain("a copy of ask dispatch://LEGION-100/ask/ask-source");
  expect(openCopy.text).toContain("answer it there");

  // A settled copy whose saved version holds no state is refused, naming the wait.
  const unsettledCopy = await runWith({
    asks: [],
    versionOpener: copyOpener(),
    liveAsks: [{ ...answeredLive, ...copySource }],
  });
  expect(unsettledCopy.isError).toBe(true);
  expect(unsettledCopy.calls).toEqual([]);
  expect(unsettledCopy.text).toContain("a copy of ask dispatch://LEGION-100/ask/ask-source");
  expect(unsettledCopy.text).toContain("answered but still open in version 4");
  expect(unsettledCopy.text).toContain("wait for that version");

  // Once the saved version records state="answered" on the copy, registration proceeds.
  const settledCopy = await runWith({
    asks: [],
    versionOpener: copyOpener("answered"),
    liveAsks: [{ ...answeredLive, ...copySource }],
  });
  expect(settledCopy.isError).toBe(false);
  expect(registrations(settledCopy.calls)).toHaveLength(1);
});

// The owner's plan (contract 219): register_plan names its document as register_gate does, for any
// issue of the architect's tree, and answers the plan Legion took; each refusal of the call itself
// comes before any lookup, grant or request, and the daemon's refusal reaches the agent with its
// code. stop_task names the task to stop by its block's id.
test("register_plan takes the document reference the Dispatch tools take and answers the plan Legion took", async () => {
  const plan = "8f14e45f-ceea-467f-a0e6-1d8b2f8c9d11";
  const taken = { plan: { version: 3, tasks: [{ id: "a", steps: ["implementer", "reviewer"] }] } };
  // The pane's Dispatch holds LEGION-209 with its plan document, which a reference is looked up in.
  const board = fakeDispatchFetch({
    routes: {
      "/api/v1/issues/LEGION-209": {
        key: "LEGION-209",
        artifacts: [
          {
            id: plan,
            slug: "plan-md",
            name: "plan.md",
            kind: "doc",
            primary: false,
            approval: { state: "draft", latest_version: 3 },
          },
        ],
      },
    },
  });
  const { calls, daemon } = recordingDaemon({ answers: { planRegister: taken } });
  const run = (parameters: Record<string, unknown>, sessionIssue = "LEGION-208") =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: async () =>
        new DispatchClient(
          "http://dispatch.test",
          bearerCredential(DISPATCH_TOKEN),
          board.fetchImpl
        ),
      session: () => claimSession("architect", sessionIssue),
    }).execute("", { op: "register_plan", ...parameters }, undefined, undefined, context());

  // A child of the root architect's tree, by the plan's filename; then the sub-architect's own
  // issue, by the document's id, which goes to the daemon as it is.
  await expect(run({ issue: "LEGION-209", artifactId: "plan.md", version: 3 })).resolves.toEqual({
    content: [{ type: "text", text: JSON.stringify(taken) }],
    details: taken,
  });
  await expect(
    run({ issue: "LEGION-209", artifactId: plan, version: 3 }, "LEGION-209")
  ).resolves.toMatchObject({ details: taken });
  const lookups = board.paths.length;
  expect(lookups).toBeGreaterThan(0);
  expect(board.paths.every((path) => path.startsWith("/api/v1/issues/LEGION-209"))).toBe(true);
  const registration = { grantId: "grant-208", issue: "LEGION-209", artifactId: plan, version: 3 };
  expect(calls.filter(([route]) => route === "planRegister")).toEqual([
    ["planRegister", registration],
    ["planRegister", registration],
  ]);

  const before = calls.length;
  for (const [parameters, text] of [
    [{ artifactId: "plan", version: 1 }, "register_plan requires issue"],
    [{ issue: "LEGION-209", version: 1 }, "register_plan requires artifactId"],
    [
      { issue: "LEGION-209", artifactId: "plan" },
      "register_plan requires a positive integer version",
    ],
    [
      { issue: "LEGION-209", artifactId: "plan", version: 0 },
      "register_plan requires a positive integer version",
    ],
    [
      { issue: "LEGION-209", artifactId: "plan", version: 1.5 },
      "register_plan requires a positive integer version",
    ],
  ] as const) {
    await expect(run(parameters)).resolves.toEqual({
      content: [{ type: "text", text }],
      details: {},
      isError: true,
    });
  }
  expect(calls).toHaveLength(before);
  expect(board.paths).toHaveLength(lookups);
});

test("register_plan passes on the daemon's refusal of a plan with its code and every reason", async () => {
  const refusal = Object.assign(
    new Error(
      "POST /legion/v1/plans/register failed with 409 PLAN_REFUSED: Legion refused this plan and recorded nothing: task a names the step prober, and the deployment defines no role prober."
    ),
    { code: "PLAN_REFUSED" }
  );
  const { daemon } = recordingDaemon({ answers: { planRegister: refusal } });
  const result = await createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("architect"),
  }).execute(
    "",
    {
      op: "register_plan",
      issue: "LEGION-208",
      artifactId: "8f14e45f-ceea-467f-a0e6-1d8b2f8c9d11",
      version: 2,
    },
    undefined,
    undefined,
    context()
  );
  expect(result.isError).toBe(true);
  expect(JSON.stringify(result.content)).toContain("409 PLAN_REFUSED");
  expect(JSON.stringify(result.content)).toContain("task a names the step prober");
});

test("stop_task stops the task its block names, and is the architect's alone", async () => {
  const { calls, daemon } = recordingDaemon({});
  const run = (session: LegionToolSession, parameters: Record<string, unknown>) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => session,
    }).execute("", { op: "stop_task", ...parameters }, undefined, undefined, context());
  const architect = claimSession("architect");

  await expect(run(architect, { issue: "LEGION-209", task: "default" })).resolves.toEqual({
    content: [{ type: "text", text: "{}" }],
    details: {},
  });
  expect(calls).toEqual([
    [
      "grant",
      { sessionId: "ses_208", secret: CLAIM_SECRET, tree: "LEGION-208", issue: "LEGION-208" },
    ],
    ["taskStop", { grantId: "grant-208", issue: "LEGION-209", task: "default" }],
  ]);

  for (const [session, parameters, text] of [
    [architect, { issue: "LEGION-209" }, "stop_task requires task"],
    [architect, { issue: "LEGION-209", task: " " }, "stop_task requires task"],
    [architect, { task: "a" }, "stop_task requires issue"],
    [
      claimSession("implementer"),
      { issue: "LEGION-208", task: "a" },
      "stop_task is not available to a phase-worker session",
    ],
  ] as const) {
    await expect(run(session, parameters)).resolves.toEqual({
      content: [{ type: "text", text }],
      details: {},
      isError: true,
    });
  }
  expect(calls).toHaveLength(2);
});

/** The pane a completion runs in: its workspace and issue. */
const paneEnv = { LEGION_WORKSPACE: "/workspaces/LEGION-208", LEGION_ISSUE: "LEGION-208" };
const TIP = "210d53a9d1b109df97fbb5dd5041d659dbab1323";
const STANDING = "c0de0000000000000000000000000000000000ff";
/** The newest handoff record the pane's Dispatch holds for the phase: its body names the head the
 * phase's work was done against, which the completion reports, and the implementer's is the tip
 * it pushed. */
const RECORD = {
  id: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
  version: 2,
  body: JSON.stringify({ schemaVersion: 1, issue: "LEGION-208", head: TIP }),
};

/** A jj whose workspace stands on `standing` and whose `legion/LEGION-208@origin`, whose tip is
 * `TIP`, carries that commit, unless `pushed` is false. An implementer pane that pushed its round
 * stands on `TIP`, the head its record names. */
function paneJj(standing = STANDING, pushed = true): JjRunner {
  return async (args) => {
    const revision = args[2] ?? "";
    if (revision === "@-") return standing;
    if (revision.startsWith("remote_bookmarks(") && revision.endsWith(` & ${standing}::`)) {
      return pushed ? TIP : "";
    }
    throw new Error(`unexpected jj ${args.join(" ")}`);
  };
}

/** A Dispatch answering `answer` for every phase's newest record, recording each read in `reads`. */
function paneDispatch(
  reads: string[] = [],
  answer: () => Promise<typeof RECORD> = async () => RECORD
): LatestHandoff {
  return async (issue, word) => {
    reads.push(`${issue} ${word}`);
    return answer();
  };
}

/** A daemon state with LEGION-208 at `phase` (and LEGION-209, a sub-architect's issue, admitted). */
const stateAt = (phase: LegionPhase) => ({
  issues: {
    "LEGION-208": { key: "LEGION-208", phase },
    "LEGION-209": { key: "LEGION-209", phase: "admitted" },
  },
});

test("a named look's report leaves the phase open and a step's completion closes it", async () => {
  let completed = 0;
  const { calls, daemon } = recordingDaemon({ state: stateAt("testing") });
  const tool = createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => {
      completed++;
    },
    onUpdateAnswered: () => undefined,
    handoffReport: async () => ({ commit: TIP, handoffId: RECORD.id, handoffVersion: 2 }),
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("tester"),
  });
  const report = await tool.execute(
    "",
    {
      op: "handoff_complete",
      summary: "looked",
      verdict: "pass",
      check_opened_at: "2026-10-19T06:00:00.123456Z",
    },
    undefined,
    undefined,
    context()
  );
  expect(report).not.toHaveProperty("isError", true);
  expect(completed).toBe(0);
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "looked",
      verdict: "pass",
      checkOpenedAt: "2026-10-19T06:00:00.123456Z",
    },
  ]);
  await tool.execute(
    "",
    { op: "handoff_complete", summary: "tested", verdict: "pass" },
    undefined,
    undefined,
    context()
  );
  expect(completed).toBe(1);
});

test("handoff_complete finds the phase's handoff record and the pushed tip, posts them with a fresh grant, and closes the phase on success", async () => {
  const completed: string[] = [];
  const reads: string[] = [];
  const { calls, daemon } = recordingDaemon({ state: stateAt("implementing") });
  const run = (
    session: LegionToolSession,
    parameters: Record<string, unknown>,
    jj: JjRunner = paneJj(),
    latestHandoff: LatestHandoff = paneDispatch(reads)
  ) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
      onUpdateAnswered: () => undefined,
      handoffReport: (phase, role) =>
        findHandoffReport({ phase, role, env: paneEnv, jj, latestHandoff }),
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => session,
    }).execute("", parameters, undefined, undefined, context());

  // The implementer at implementing: the issue's phase is read from the daemon's state, the newest
  // implement record from the pane's Dispatch, and the head that record names — which the pane's
  // jj holds to the commit the workspace stands on, pushed — travels as `commit`, with the wire's
  // zero value for the verdict.
  await expect(
    run(claimSession("implementer"), { op: "handoff_complete", summary: "Done." }, paneJj(TIP))
  ).resolves.toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
  expect(calls).toEqual([
    ["record", { issue: "LEGION-208" }],
    [
      "grant",
      { sessionId: "ses_208", secret: CLAIM_SECRET, tree: "LEGION-208", issue: "LEGION-208" },
    ],
    [
      "handoffComplete",
      {
        grantId: "grant-208",
        summary: "Done.",
        verdict: "",
        commit: TIP,
        handoffId: RECORD.id,
        handoffVersion: RECORD.version,
      },
    ],
  ]);
  expect(reads).toEqual(["LEGION-208 implement"]);
  expect(completed).toEqual(["ses_208"]);

  // A role that does not work the issue's phase reports its own newest record of the phase it
  // left: the tester at implementing its test record and the head it names, with the verdict as
  // given. A role whose left phase writes no record reports the commit the workspace stands on and
  // no record: the merger, and a sub-architect, which leaves no phase, each posting @- (the daemon
  // then refuses the sub-architect's, ACCEPTANCE_IS_SIGN_OFF; this stand-in daemon records it).
  await run(claimSession("tester"), { op: "handoff_complete", summary: "Red.", verdict: "fail" });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "Red.",
      verdict: "fail",
      commit: TIP,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    },
  ]);
  await run(claimSession("merger"), { op: "handoff_complete", summary: "Merged." });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "Merged.", verdict: "", commit: STANDING },
  ]);
  await run(claimSession("architect", "LEGION-209"), {
    op: "handoff_complete",
    summary: "Children released.",
  });
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "Children released.",
      verdict: "",
      commit: STANDING,
    },
  ]);
  expect(completed).toEqual(["ses_208", "ses_208", "ses_208", "ses_208"]);
  expect(reads).toEqual(["LEGION-208 implement", "LEGION-208 test"]);

  // Refused before any daemon call: a root architect, which runs no phase; a missing summary; a
  // verdict of the wrong shape; and a phase, which the daemon knows from the grant.
  const before = calls.length;
  const refusals = [
    [claimSession("architect"), { summary: "Done." }, "root architect session"],
    [claimSession("implementer"), {}, "handoff_complete requires summary"],
    [
      claimSession("tester"),
      { summary: "x", verdict: "maybe" },
      "verdict is pass, pending or fail",
    ],
    [claimSession("planner"), { summary: "x", phase: "plan" }, 'does not accept field "phase"'],
  ] as const;
  for (const [session, parameters, message] of refusals) {
    const result = await run(session, { op: "handoff_complete", ...parameters });
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
  expect(calls).toHaveLength(before);
  expect(completed).toHaveLength(4);

  // Without check_opened_at, pending is refused as a phase completion before any daemon call.
  const pendingOnPhase = await run(claimSession("tester"), {
    op: "handoff_complete",
    summary: "Waiting on the deploy.",
    verdict: "pending",
  });
  expect(pendingOnPhase.isError).toBe(true);
  expect(pendingOnPhase.content).toEqual([
    {
      type: "text",
      text: "handoff_complete's pending verdict reports a production check. Send check_opened_at with the time from the look task or check.openedAt from read_record. To complete a step, send pass or fail.",
    },
  ]);
  expect(calls).toHaveLength(before);
  expect(completed).toHaveLength(4);

  // Each refusal of the report is the tool's error, naming the remedy: the state is read, and
  // then nothing is minted or posted, and the phase stays open.
  const reportRefusals = [
    [
      paneJj(),
      paneDispatch([], async () => {
        throw new DispatchServiceError("HANDOFF_NOT_FOUND", 404, "no implement handoff");
      }),
      "LEGION-208 has no implement handoff in Dispatch: write it with `dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>`, then complete again",
    ],
    [
      paneJj(),
      paneDispatch([], async () => {
        throw new DispatchServiceError("UNAVAILABLE", 503, "database is starting");
      }),
      "handoff_complete could not read LEGION-208's newest implement handoff from Dispatch: 503 UNAVAILABLE: database is starting; complete again once Dispatch answers",
    ],
    // A round that forgot its push: the record names the branch's tip at origin, and the commit
    // the workspace stands on is not pushed.
    [
      paneJj(STANDING, false),
      paneDispatch(),
      `the commit this workspace stands on, ${STANDING}, is not on legion/LEGION-208@origin: push it with \`jj bookmark set legion/LEGION-208 -r @- && jj git push --bookmark legion/LEGION-208\`; the handoff's \`head\` must name that pushed commit, and the newest implement handoff of LEGION-208 (version 2) names ${TIP}, so when that is another commit, write the handoff again with \`head\` set to ${STANDING} (\`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>\`); then complete again`,
    ],
    // A pushed `@-` the record does not name: it names the branch's tip at origin instead.
    [
      paneJj(),
      paneDispatch(),
      `the newest implement handoff of LEGION-208 (version 2) names ${TIP} as its head, not the commit this workspace stands on, ${STANDING}, which legion/LEGION-208@origin carries: write the handoff again with \`head\` set to ${STANDING} (\`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>\`), then complete again`,
    ],
    [
      paneJj(),
      paneDispatch([], async () => ({ ...RECORD, body: '{"schemaVersion":1}' })),
      'the newest implement handoff of LEGION-208 (version 2) names no head: write the handoff again with `head` set to the 40-character lower-case commit you pushed as `@-`, as `jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T commit_id` prints it once the push succeeded (`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>`), then complete again',
    ],
  ] as const;
  for (const [jj, latestHandoff, message] of reportRefusals) {
    const result = await run(
      claimSession("implementer"),
      { op: "handoff_complete", summary: "Done." },
      jj,
      latestHandoff
    );
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: message }]);
    expect(calls.at(-1)).toEqual(["record", { issue: "LEGION-208" }]);
  }
  expect(completed).toHaveLength(4);
});

test("the planner, the tester and the reviewer post their phase's handoff record and the head it names", async () => {
  for (const [phase, role, word] of [
    ["planning", "planner", "plan"],
    ["testing", "tester", "test"],
    ["reviewing", "reviewer", "review"],
  ] as const) {
    const reads: string[] = [];
    const { calls, daemon } = recordingDaemon({ state: stateAt(phase) });
    const result = await createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: (issuePhase, claimRole) =>
        findHandoffReport({
          phase: issuePhase,
          role: claimRole,
          env: paneEnv,
          jj: async (args) => {
            throw new Error(`no jj expected for the ${role}: ${args.join(" ")}`);
          },
          latestHandoff: paneDispatch(reads),
        }),
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => claimSession(role),
    }).execute(
      "",
      {
        op: "handoff_complete",
        summary: "Done.",
        ...(role === "tester" ? { verdict: "pass" } : {}),
      },
      undefined,
      undefined,
      context()
    );
    expect(result.isError).toBeUndefined();
    expect(reads).toEqual([`LEGION-208 ${word}`]);
    expect(calls.at(-1)).toEqual([
      "handoffComplete",
      {
        grantId: "grant-208",
        summary: "Done.",
        verdict: role === "tester" ? "pass" : "",
        commit: TIP,
        handoffId: RECORD.id,
        handoffVersion: RECORD.version,
      },
    ]);
  }
});

// The agent that does the retro rates the plan the issue ran in the completion that ends the
// retro. Retro writes no handoff, so the completion reports the commit the workspace stands on,
// and the rating travels beside it under the daemon's names.
test("the retro's handoff_complete posts its rating of the plan as planRating and planReason, with the commit the workspace stands on", async () => {
  const reads: string[] = [];
  const { calls, daemon } = recordingDaemon({ state: stateAt("retro") });
  const reason = "The plan's tester step caught the regression the review would have missed.";
  const result = await createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: (phase, role) =>
      findHandoffReport({
        phase,
        role,
        env: paneEnv,
        jj: paneJj(),
        latestHandoff: paneDispatch(reads),
      }),
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("implementer"),
  }).execute(
    "",
    { op: "handoff_complete", summary: "Retro done.", plan_rating: 5, plan_reason: reason },
    undefined,
    undefined,
    context()
  );
  expect(result.isError).toBeUndefined();
  expect(reads).toEqual([]);
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "Retro done.",
      verdict: "",
      commit: STANDING,
      planRating: 5,
      planReason: reason,
    },
  ]);
});

test("a completion once the issue moved on posts the role's own newest record of the phase it left, and @- where that phase writes none", async () => {
  const recorded = { commit: TIP, handoffId: RECORD.id, handoffVersion: RECORD.version };
  for (const [phase, role, issue, read, report] of [
    // A review ends its round on its own: a request for changes moves the issue to implementing at
    // once, an approval to its task's next step (accepting, in the default steps) once the pull
    // request is mergeable, and the reviewer completes after it.
    ["implementing", "reviewer", "LEGION-208", ["LEGION-208 review"], recorded],
    ["accepting", "reviewer", "LEGION-208", ["LEGION-208 review"], recorded],
    ["reviewing", "tester", "LEGION-208", ["LEGION-208 test"], recorded],
    // At merging the implementer left retro, which writes no record; a sub-architect leaves no
    // phase, so it posts @- in another role's file-backed phase as well.
    ["merging", "implementer", "LEGION-208", [], { commit: STANDING }],
    ["implementing", "architect", "LEGION-209", [], { commit: STANDING }],
  ] as const) {
    const reads: string[] = [];
    const { calls, daemon } = recordingDaemon({
      state: { issues: { [issue]: { key: issue, phase } } },
    });
    const result = await createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: (issuePhase, claimRole) =>
        findHandoffReport({
          phase: issuePhase,
          role: claimRole,
          env: paneEnv,
          jj: paneJj(),
          latestHandoff: paneDispatch(reads),
        }),
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => claimSession(role, issue),
    }).execute(
      "",
      {
        op: "handoff_complete",
        summary: "Done.",
        ...(role === "tester" ? { verdict: "pass" } : {}),
      },
      undefined,
      undefined,
      context()
    );
    expect(result.isError).toBeUndefined();
    expect(reads).toEqual([...read]);
    expect(calls.at(-1)).toEqual([
      "handoffComplete",
      {
        grantId: "grant-208",
        summary: "Done.",
        verdict: role === "tester" ? "pass" : "",
        ...report,
      },
    ]);
  }
});

// The issue's workspace is one checkout every role of the issue shares, so once the issue has
// moved on, the commit it stands on is often a sibling role's: here the tester's, which the
// branch at origin does not carry. The implementer's late completion is of implementing, the phase
// it left, and posts its implement record and the head it names, reading nothing of the workspace,
// for the daemon to answer.
test("an implementer whose issue is in testing posts its implement record, though @- is the tester's unpushed commit", async () => {
  const completed: string[] = [];
  const reads: string[] = [];
  const asked: string[] = [];
  const workspace = paneJj(STANDING, false);
  const { calls, daemon } = recordingDaemon({ state: stateAt("testing") });
  const result = await createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
    onUpdateAnswered: () => undefined,
    handoffReport: (phase, role) =>
      findHandoffReport({
        phase,
        role,
        env: paneEnv,
        jj: (args, cwd) => {
          asked.push(args.join(" "));
          return workspace(args, cwd);
        },
        latestHandoff: paneDispatch(reads),
      }),
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("implementer"),
  }).execute("", { op: "handoff_complete", summary: "Done." }, undefined, undefined, context());
  expect(result.isError).toBeUndefined();
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "Done.",
      verdict: "",
      commit: TIP,
      handoffId: RECORD.id,
      handoffVersion: RECORD.version,
    },
  ]);
  expect(reads).toEqual(["LEGION-208 implement"]);
  expect(asked).toEqual([]);
  expect(completed).toEqual(["ses_208"]);
});

// A tester whose issue was held before it wrote its test record completes testing, the phase it
// left, with no record to name: the tool posts the commit the workspace stands on rather than
// tell it to write a record, and the daemon's answer, refusing a completion while no role works
// the issue's phase, reaches the agent.
test("a tester with no record on a held issue posts @-, and the daemon's answer reaches the agent", async () => {
  const reads: string[] = [];
  const refusal = Object.assign(
    new Error(
      "POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_CURRENT_PHASE: the tester does not run phase held of LEGION-208; this completion changed nothing"
    ),
    { code: "HANDOFF_NOT_CURRENT_PHASE" }
  );
  const { calls, daemon } = recordingDaemon({
    state: stateAt("held"),
    answers: { handoffComplete: refusal },
  });
  const result = await createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: (phase, role) =>
      findHandoffReport({
        phase,
        role,
        env: paneEnv,
        jj: paneJj(),
        latestHandoff: paneDispatch(reads, async () => {
          throw new DispatchServiceError("HANDOFF_NOT_FOUND", 404, "no test handoff");
        }),
      }),
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("tester"),
  }).execute(
    "",
    { op: "handoff_complete", summary: "Tests pass.", verdict: "pass" },
    undefined,
    undefined,
    context()
  );
  expect(reads).toEqual(["LEGION-208 test"]);
  expect(calls.at(-1)).toEqual([
    "handoffComplete",
    { grantId: "grant-208", summary: "Tests pass.", verdict: "pass", commit: STANDING },
  ]);
  expect(result).toMatchObject({
    isError: true,
    content: [{ type: "text", text: expect.stringContaining("409 HANDOFF_NOT_CURRENT_PHASE") }],
  });
});

/** LEGION-208 closed by its merge, with its production check open on its second look: `role` is
 * the role the check records as the one that works it, and `last` false says a look follows. */
const openCheck = {
  repo: "acme/widgets",
  pullRequest: 1239,
  mergeCommit: "3e9c0a7b5d2f4e8a1c6b9d0f2a4e7c5b8d1f3a60",
  openedAt: "2026-09-22T08:40:12Z",
  role: "implementer",
  looks: 2,
  lookOpen: true,
  lastWake: {
    kind: "issue.deployed",
    detail: "run 38230245571 completed 2026-09-22T09:10:44Z",
    at: "2026-09-22T09:10:51Z",
  },
  nextWakeAt: "2026-10-06T08:40:12Z",
  last: false,
};

/** The time a pending report names for its next look. */
const NEXT_LOOK = "2026-09-29T09:00:00Z";

test("a report on its closed issue's production check carries a verdict and no commit, and a pending its next look", async () => {
  const completed: string[] = [];
  const { calls, daemon } = recordingDaemon({
    state: {
      issues: {
        "LEGION-208": { key: "LEGION-208", phase: "done", check: openCheck },
      },
    },
  });
  const run = (session: LegionToolSession, parameters: Record<string, unknown>) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
      onUpdateAnswered: () => undefined,
      // A check report resolves no record and no commit: the report is never asked.
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => session,
    }).execute("", parameters, undefined, undefined, context());

  // A named check report posts a grant and report only, and leaves phase input open.
  for (const verdict of ["pending", "pass", "fail"]) {
    const wake = verdict === "pending" ? { wake_at: NEXT_LOOK } : {};
    await expect(
      run(claimSession("implementer"), {
        op: "handoff_complete",
        summary: `Look 2: ${verdict}.`,
        verdict,
        check_opened_at: openCheck.openedAt,
        ...wake,
      })
    ).resolves.toEqual({ content: [{ type: "text", text: "{}" }], details: {} });
    expect(calls.slice(-2)).toEqual([
      [
        "grant",
        { sessionId: "ses_208", secret: CLAIM_SECRET, tree: "LEGION-208", issue: "LEGION-208" },
      ],
      [
        "handoffComplete",
        {
          grantId: "grant-208",
          summary: `Look 2: ${verdict}.`,
          verdict,
          checkOpenedAt: openCheck.openedAt,
          ...(verdict === "pending" ? { wakeAt: NEXT_LOOK } : {}),
        },
      ],
    ]);
  }
  expect(completed).toEqual([]);

  // A missing verdict is refused before any daemon call.
  const noVerdict = await run(claimSession("implementer"), {
    op: "handoff_complete",
    summary: "Looked.",
    check_opened_at: openCheck.openedAt,
  });
  expect(noVerdict.isError).toBe(true);
  expect(noVerdict.content).toEqual([
    {
      type: "text",
      text: "handoff_complete with check_opened_at names a verdict of pass, pending or fail",
    },
  ]);
  expect(calls).toHaveLength(6);
  expect(completed).toHaveLength(0);

  // A tester's named report posts the wake its pending supplied.
  const { calls: testerCalls, daemon: testerDaemon } = recordingDaemon({
    state: {
      issues: {
        "LEGION-208": {
          key: "LEGION-208",
          phase: "done",
          check: { ...openCheck, role: "tester" },
        },
      },
    },
  });
  const testerAtDone = await createLegionTool({
    pi,
    daemon: testerDaemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("tester"),
  }).execute(
    "",
    {
      op: "handoff_complete",
      summary: "Looked.",
      verdict: "pending",
      wake_at: NEXT_LOOK,
      check_opened_at: openCheck.openedAt,
    },
    undefined,
    undefined,
    context()
  );
  expect(testerAtDone.isError).toBeUndefined();
  expect(testerCalls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "Looked.",
      verdict: "pending",
      wakeAt: NEXT_LOOK,
      checkOpenedAt: openCheck.openedAt,
    },
  ]);

  // A named report on an absent check is posted for the daemon to refuse.
  const { calls: closedCalls, daemon: closedDaemon } = recordingDaemon({
    state: { issues: { "LEGION-208": { key: "LEGION-208", phase: "done" } } },
  });
  const implementerAtDone = await createLegionTool({
    pi,
    daemon: closedDaemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: () => undefined,
    onUpdateAnswered: () => undefined,
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("implementer"),
  }).execute(
    "",
    {
      op: "handoff_complete",
      summary: "Looked.",
      verdict: "pass",
      check_opened_at: openCheck.openedAt,
    },
    undefined,
    undefined,
    context()
  );
  expect(implementerAtDone.isError).toBeUndefined();
  expect(closedCalls.at(-1)).toEqual([
    "handoffComplete",
    {
      grantId: "grant-208",
      summary: "Looked.",
      verdict: "pass",
      checkOpenedAt: openCheck.openedAt,
    },
  ]);
});

test("a refused completion surfaces the daemon's code and leaves the phase open", async () => {
  const refusal = Object.assign(
    new Error(
      `POST /legion/v1/handoff/complete failed with 409 HANDOFF_NOT_NEW: the implementer named handoff record ${RECORD.id} for its previous phase of LEGION-208; write this phase's handoff with \`dispatch handoff-write --issue LEGION-208 --phase implement --body-file <file>\` before completing`
    ),
    { code: "HANDOFF_NOT_NEW" }
  );
  const completed: string[] = [];
  const { daemon } = recordingDaemon({
    state: stateAt("implementing"),
    answers: { handoffComplete: refusal },
  });
  const tool = createLegionTool({
    pi,
    daemon,
    controllerGrant: noControllerGrant,
    onPhaseCompleted: (ctx) => completed.push(ctx.sessionManager.getSessionId()),
    onUpdateAnswered: () => undefined,
    handoffReport: (phase, role) =>
      findHandoffReport({
        phase,
        role,
        env: paneEnv,
        jj: paneJj(TIP),
        latestHandoff: paneDispatch(),
      }),
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => claimSession("implementer"),
  });

  await expect(
    tool.execute("", { op: "handoff_complete", summary: "Done." }, undefined, undefined, context())
  ).resolves.toMatchObject({
    isError: true,
    content: [{ type: "text", text: expect.stringContaining("HANDOFF_NOT_NEW") }],
  });
  expect(completed).toEqual([]);
});

/** A state the contracts schema accepts, with what a test varies laid over it: `read_state`
 * summarizes the daemon's own answer, so the tests build it as the daemon serves it. */
function legionState(overrides: Record<string, unknown> = {}): LegionState {
  return LegionStateResponse.parse({
    daemon: {
      project: "INFRA",
      schemaVersion: 71,
      boots: 9,
      firstBootAt: "2026-10-01T00:00:00Z",
      startedAt: "2026-10-30T01:02:03Z",
    },
    admission: { cap: 8, active: [], waiting: [], blocked: [], free: 0 },
    issues: {},
    pendingStatusWrites: [],
    heldOutboxRows: [],
    strandedClaims: [],
    capabilities: [],
    repositories: { live: [], withoutWebhookApp: [], caches: [] },
    consumers: [],
    settings: { review_round_cap: 3, worker_boot_timeout_seconds: 900 },
    ...overrides,
  });
}

/** The phase workers of an issue, in the order the summary lists them under the architect. */
const WORKER_ROLES = ["planner", "implementer", "tester", "reviewer", "merger"] as const;

/** The state at the size that cut the controller's read (dispatch://INFRA-329): the 92 issues
 * the INFRA deployment recorded, `running` of them still in the workflow, each carrying a slot, a
 * design gate, a pull request, an architect and a claim for every role with both turn times. Half
 * the issues run at once, nearly six times the deployment's admission cap, so the summary is
 * measured well above the load it meets. */
function stateOf92Issues(running = 46): LegionState {
  const at = (minutes: number) =>
    `2026-10-30T${String(Math.floor(minutes / 60)).padStart(2, "0")}:${String(minutes % 60).padStart(2, "0")}:00Z`;
  const claimView = (index: number, state: string) => ({
    session: `ses_${index}`,
    state,
    locator: {
      runtime: "sandbox",
      claim: `claim-${index}`,
      incarnation: `uid-${index}/1`,
      sandbox: {
        namespace: "legion",
        name: `infra-${index}`,
        podUid: `uid-${index}`,
        container: "roles",
        generation: 1,
      },
    },
    pluginContract: 42,
    lastTurnStart: at(index % 1440),
    lastTurnEnd: at((index + 7) % 1440),
  });
  const phases = LEGION_WORKFLOW_PHASES.filter((phase) => phase !== "done");
  const issues: Record<string, unknown> = {};
  for (let index = 0; index < 92; index += 1) {
    const key = `INFRA-${100 + index}`;
    const phase = phases[index % phases.length];
    issues[key] =
      index < running
        ? {
            key,
            generation: 2,
            phase,
            status: "in_progress",
            repo: "acme/widgets",
            defaultBranch: "main",
            ...(phase === "held" ? { holdReason: "escalated" } : {}),
            architect: claimView(index, "idle"),
            workers: Object.fromEntries(
              WORKER_ROLES.map((role, offset) => [
                role,
                {
                  claim: claimView(index * 6 + offset, offset === 0 ? "working" : "retired"),
                  pass: 1,
                  rounds: 0,
                },
              ])
            ),
            slot: { index: index % 8, admittedAt: at(index) },
            designGate: {
              artifactId: `artifact-${index}`,
              currentVersion: 3,
              approvedVersion: index % 2 === 0 ? 2 : null,
            },
            pullRequest: {
              repo: "acme/widgets",
              number: 1000 + index,
              head: `${index}`.padStart(40, "0"),
              state: "open",
              mergeState: "blocked",
              reviewDecision: "CHANGES_REQUESTED",
            },
          }
        : {
            key,
            generation: 2,
            phase: "done",
            status: "done",
            repo: "acme/widgets",
            defaultBranch: "main",
            workers: {},
          };
  }
  return legionState({
    issues,
    admission: {
      cap: 8,
      free: 0,
      active: ["INFRA-100", "INFRA-101"],
      waiting: ["INFRA-102"],
      blocked: [{ issue: "INFRA-102", blockedBy: ["INFRA-103", "INFRA-104"] }],
    },
    capabilities: [
      { name: "subagents", status: "present", detail: "6 agents discovered" },
      {
        name: "model-fallback",
        status: "open",
        detail: "no fallback configured",
        configLine: "capabilities.decided.model-fallback",
      },
    ],
  });
}

/** The controller's tool over `daemon`, recording each session its grant is minted for. */
function controllerTool(daemon: () => LegionDaemonClient, minted: string[]): RegisteredTool {
  return createLegionTool({
    pi,
    daemon,
    controllerGrant: async (sessionId) => {
      minted.push(sessionId);
      return { grantId: "controller-grant-1", expiresAt: "2099-01-01T00:00:00Z" };
    },
    onPhaseCompleted: () => {
      throw new Error("a controller completes no phase");
    },
    onUpdateAnswered: () => {
      throw new Error("a controller answers no update message");
    },
    handoffReport: noHandoffReport,
    branchHead: noBranchHead,
    dispatch: noDispatch,
    session: () => ({ kind: "controller", sessionID: "ses_controller" }),
  });
}

/** The lines of `read_state`'s summary, read as the controller reads them from `daemon`. */
async function summaryLinesOf(daemon: () => LegionDaemonClient): Promise<string[]> {
  const result = await controllerTool(daemon, []).execute(
    "",
    { op: "read_state" },
    undefined,
    undefined,
    context("ses_controller")
  );
  const [block] = result.content;
  return (block !== undefined && block.type === "text" ? block.text : "").split("\n");
}

test("the controller reads the state without a grant and sets an issue's status with its own", async () => {
  const state = legionState({
    admission: { cap: 2, free: 0, active: ["LEGION-208"], waiting: [], blocked: [] },
    issues: {
      "LEGION-208": {
        key: "LEGION-208",
        generation: 1,
        phase: "held",
        status: "in_progress",
        holdReason: "escalated",
        workers: {},
      },
    },
  });
  const minted: string[] = [];
  // What the daemon answers a fresh session whose phase restart the workflow refused: the claim
  // came back, and this is the one thing about it the controller can read nowhere else.
  const notRestarted = {
    phaseNotRestarted:
      "legion-omp-legion-209-implementer is back in a new session owing no task, and nothing restarted its phase: " +
      "LEGION-209 is still held after the retry of its phase implementing",
  };
  const { calls, daemon } = recordingDaemon({ state, answers: { freshSession: notRestarted } });
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));

  // read_state with no field is the summary: the skill reads the issue's phase and hold, the
  // admission lists and the capability rows from it, and reads a full record with a filter.
  const summary = await run({ op: "read_state" });
  expect(summary.details).toEqual({});
  expect(summary.content).toEqual([
    {
      type: "text",
      text: expect.stringContaining("\nissues 1\nLEGION-208 held in_progress\n  hold escalated"),
    },
  ]);
  expect(calls).toEqual([["state", undefined]]);
  expect(minted).toEqual([]);

  await expect(run({ op: "set_status", issue: "LEGION-208", status: "backlog" })).resolves.toEqual({
    content: [{ type: "text", text: "{}" }],
    details: {},
  });
  expect(minted).toEqual(["ses_controller"]);
  expect(calls.at(-1)).toEqual([
    "issueStatus",
    { grantId: "controller-grant-1", issue: "LEGION-208", status: "backlog" },
  ]);

  // fresh_session names the claim by its issue and role, and mints the controller's grant as
  // set_status does: the daemon decides which claims it reaches. Its answer is the daemon's own,
  // so the controller reads why nothing restarted the claim's phase; nothing else records it.
  await expect(
    run({ op: "fresh_session", issue: "LEGION-209", role: "implementer" })
  ).resolves.toEqual({
    content: [{ type: "text", text: JSON.stringify(notRestarted) }],
    details: notRestarted,
  });
  expect(minted).toEqual(["ses_controller", "ses_controller"]);
  expect(calls.at(-1)).toEqual([
    "freshSession",
    { grantId: "controller-grant-1", issue: "LEGION-209", role: "implementer" },
  ]);

  // compact names the claim the same way and returns the daemon's own answer: a compaction Oh
  // My Pi refused or skipped says so and names why, and the claim is back either way.
  await expect(run({ op: "compact", issue: "LEGION-209", role: "implementer" })).resolves.toEqual({
    content: [{ type: "text", text: "{}" }],
    details: {},
  });
  expect(calls.at(-1)).toEqual([
    "compact",
    { grantId: "controller-grant-1", issue: "LEGION-209", role: "implementer" },
  ]);
  // Refused before any mint: a status the board does not have, a missing issue, a role no claim
  // is on — the controller's own among them — and every operation of a claim.
  const before = calls.length;
  const refusals = [
    [{ op: "set_status", issue: "LEGION-208", status: "done" }, "todo, backlog, icebox"],
    [{ op: "set_status", status: "todo" }, "set_status requires issue"],
    [{ op: "fresh_session", issue: "LEGION-209" }, "fresh_session requires role architect"],
    [
      { op: "fresh_session", issue: "LEGION-209", role: "controller" },
      "fresh_session requires role architect",
    ],
    [{ op: "fresh_session", role: "implementer" }, "fresh_session requires issue"],
    [
      { op: "fresh_session", issue: "LEGION-209", role: "implementer", status: "todo" },
      'fresh_session does not accept field "status"; it takes issue, role',
    ],
    [
      { op: "read_state", to: "planning" },
      'read_state does not accept field "to"; it takes issue, issues, phase, settings',
    ],
    [{ op: "read_record", issue: "LEGION-208" }, "not available to a controller session"],
    [
      { op: "read_release_records", since: "2026-11-01T00:00:00Z" },
      "not available to a controller session",
    ],
    [{ op: "handoff_complete", summary: "Done." }, "not available to a controller session"],
    [{ op: "sign_off", issue: "LEGION-208" }, "not available to a controller session"],
  ] as const;
  for (const [parameters, message] of refusals) {
    const result = await run(parameters);
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
  // read_state takes its four filters and nothing else, so its refusal of a `reason` is that
  // field list alone.
  await expect(run({ op: "read_state", reason: "why" })).resolves.toEqual({
    content: [
      {
        type: "text",
        text: 'read_state does not accept field "reason"; it takes issue, issues, phase, settings',
      },
    ],
    details: {},
    isError: true,
  });
  expect(calls).toHaveLength(before);
  expect(minted).toEqual(["ses_controller", "ses_controller", "ses_controller"]);
});

test("the controller stops, retries, suspends and resumes an agent it names by issue and role", async () => {
  const minted: string[] = [];
  // What the daemon answers each action (agent-*.json): the claim's state after it, and who
  // holds a suspension over the agent.
  const answers = {
    stopAgent: { state: "retired" },
    retryAgent: { state: "launching" },
    suspendAgent: { state: "suspended", suspendedBy: "controller" },
    resumeAgent: { state: "launching" },
  };
  const { calls, daemon } = recordingDaemon({ answers });
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));

  // Each names the claim as fresh_session and compact do, mints the controller's grant, and
  // returns the daemon's answer whole.
  for (const [op, route] of [
    ["stop", "stopAgent"],
    ["retry", "retryAgent"],
    ["suspend", "suspendAgent"],
    ["resume_agent", "resumeAgent"],
  ] as const) {
    const answer = answers[route];
    await expect(run({ op, issue: "LEGION-209", role: "tester" })).resolves.toEqual({
      content: [{ type: "text", text: JSON.stringify(answer) }],
      details: answer,
    });
    expect(calls.at(-1)).toEqual([
      route,
      { grantId: "controller-grant-1", issue: "LEGION-209", role: "tester" },
    ]);
  }
  expect(minted).toEqual(["ses_controller", "ses_controller", "ses_controller", "ses_controller"]);

  // Refused before any mint: a missing role, the controller's own, a missing issue and a field
  // none of the four takes.
  const before = calls.length;
  for (const [parameters, message] of [
    [{ op: "stop", issue: "LEGION-209" }, "stop requires role architect"],
    [{ op: "retry", issue: "LEGION-209", role: "controller" }, "retry requires role architect"],
    [{ op: "suspend", role: "tester" }, "suspend requires issue"],
    [
      { op: "resume_agent", issue: "LEGION-209", role: "tester", reason: "why" },
      'resume_agent does not accept field "reason"; it takes issue, role',
    ],
  ] as const) {
    const result = await run(parameters);
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
  expect(calls).toHaveLength(before);
  expect(minted).toHaveLength(4);

  // A refusal of the daemon's is the tool's failure, its code and words as the daemon gave them:
  // the operator's own suspension, which only the operator lifts.
  const refusing = recordingDaemon({
    answers: {
      resumeAgent: new LegionDaemonApiError(
        "POST",
        "/legion/v1/agents/resume",
        409,
        "SUSPENDED_BY_OPERATOR",
        "legion-omp-legion-209-tester is held by the operator's own suspension, which only the operator lifts"
      ),
    },
  });
  const refused = await controllerTool(refusing.daemon, []).execute(
    "",
    { op: "resume_agent", issue: "LEGION-209", role: "tester" },
    undefined,
    undefined,
    context("ses_controller")
  );
  expect(refused.isError).toBe(true);
  expect(refused.content).toEqual([
    { type: "text", text: expect.stringContaining("SUSPENDED_BY_OPERATOR") },
  ]);
});

// The controller's replace_pod names the issue alone, mints the controller's grant as set_status
// does, and returns the daemon's answer whole: the round over the issue's pod, its reasons, and
// whether this request opened it. Refused before any mint for a missing issue and for a field it
// does not take; a refusal of the daemon's is the tool's failure, its code as the daemon gave it.
test("the controller asks for the replacement of an issue's pod with its own grant", async () => {
  const minted: string[] = [];
  // What the daemon answers (issue-pod-update.json): the round the request opened.
  const opened = {
    round: "pod:9d3f0c7a-2b41-4a39-9f6b-6c1d0a7e5b22",
    reasons: "the project's controller asked for the replacement of this pod",
    opened: true,
  };
  const { calls, daemon } = recordingDaemon({ answers: { issuePodUpdate: opened } });
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));

  await expect(run({ op: "replace_pod", issue: "LEGION-209" })).resolves.toEqual({
    content: [{ type: "text", text: JSON.stringify(opened) }],
    details: opened,
  });
  expect(calls.at(-1)).toEqual([
    "issuePodUpdate",
    { grantId: "controller-grant-1", issue: "LEGION-209" },
  ]);
  expect(minted).toEqual(["ses_controller"]);

  const before = calls.length;
  for (const [parameters, message] of [
    [{ op: "replace_pod" }, "replace_pod requires issue"],
    [
      { op: "replace_pod", issue: "LEGION-209", role: "tester" },
      'replace_pod does not accept field "role"; it takes issue',
    ],
  ] as const) {
    const result = await run(parameters);
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
  expect(calls).toHaveLength(before);
  expect(minted).toHaveLength(1);

  const refusing = recordingDaemon({
    answers: {
      issuePodUpdate: new LegionDaemonApiError(
        "POST",
        "/legion/v1/issues/pod-update",
        409,
        "POD_UPDATE_LIMITED",
        "the project's pod_update_max_open_rounds (2) holds back the update round of LEGION-209"
      ),
    },
  });
  const refused = await controllerTool(refusing.daemon, []).execute(
    "",
    { op: "replace_pod", issue: "LEGION-209" },
    undefined,
    undefined,
    context("ses_controller")
  );
  expect(refused.isError).toBe(true);
  expect(refused.content).toEqual([
    { type: "text", text: expect.stringContaining("POD_UPDATE_LIMITED") },
  ]);
});

// The architect's operation set, pinned whole: every `op` the tool's schema lists that an
// architect's session is not refused as unavailable. It is the tree's workflow operations, the
// phase ops and read_build, which every session holds; the controller's read_state, set_status,
// replace_pod, the six agent actions, pause, resume, set_p0_bound, clear_p0_bound and the
// intake's replay_give_up and discard_give_up stay the controller's alone.
test("an architect's operation set is the tree's operations and none of the agent actions", async () => {
  const opSchema = z.toJSONSchema(toolFor(claimSession("architect")).parameters as z.ZodType)
    .properties?.op;
  const declared = typeof opSchema === "object" ? (opSchema.enum ?? []).map(String) : [];
  expect(declared).toHaveLength(29);
  for (const architect of [claimSession("architect"), claimSession("architect", "LEGION-209")]) {
    const tool = toolFor(architect);
    const available: string[] = [];
    for (const op of declared) {
      const result = await tool.execute("", { op }, undefined, undefined, context());
      const refused = result.content.some(
        (part) =>
          part.type === "text" && part.text === `${op} is not available to an architect session`
      );
      if (!refused) available.push(op);
    }
    expect(available).toEqual([
      "register_gate",
      "register_plan",
      "stop_task",
      "release_children",
      "request_backward_move",
      "retry_or_escalate",
      "sign_off",
      "close_root",
      "rerun_child",
      "read_record",
      "read_release_records",
      "handoff_complete",
      "ready_for_update",
      "read_build",
    ]);
  }
});

// An agent action from an architect's session, named whole with its issue and role, is refused by
// the operation's name as every operation outside its kind's list is, before a grant is minted or
// a request sent.
test("an architect's agent action is refused as unavailable before any grant", async () => {
  for (const architect of [claimSession("architect"), claimSession("architect", "LEGION-209")]) {
    const { calls, daemon } = recordingDaemon({});
    const tool = createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => architect,
    });
    for (const op of ["fresh_session", "compact", "stop", "retry", "suspend", "resume_agent"]) {
      await expect(
        tool.execute(
          "",
          { op, issue: "LEGION-209", role: "tester" },
          undefined,
          undefined,
          context()
        )
      ).resolves.toEqual({
        content: [{ type: "text", text: `${op} is not available to an architect session` }],
        details: {},
        isError: true,
      });
    }
    expect(calls).toEqual([]);
  }
});

// R6: a role reads which plugin build its own process runs. The build is the manifest of the
// module this process loaded: `tools.ts` imports `../package.json` and takes `pluginBuildOf` of
// it, never the image's copy at `/opt/legion/pi-legion`, which a relaunch never updates. For this
// case alone a read of that copy answers a manifest with a version and a contract of its own
// (withImageManifest), so the answer below fails wherever the build is read from that path,
// whatever the image holds. The case imports `tools.ts` again under the mock and asks that copy.
const TOOLS_LOADED_UNDER_THE_IMAGE_MOCK = "./tools?read_build";
test("read_build answers the package and version this process loaded, both contract numbers, and reads no state", async () => {
  const atImagePath = {
    ...pkg,
    version: "0.0.0-image",
    legion: { daemonApiVersion: PLUGIN_CONTRACT - 1 },
  };
  await withImageManifest("/opt/legion/pi-legion/package.json", atImagePath, async () => {
    const { createLegionTool: createFreshLegionTool } = (await import(
      TOOLS_LOADED_UNDER_THE_IMAGE_MOCK
    )) as { readonly createLegionTool: typeof createLegionTool };
    const { calls, daemon } = recordingDaemon({ daemonContract: PLUGIN_CONTRACT + 1 });
    const tool = createFreshLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => claimSession("implementer"),
    });

    await expect(
      tool.execute("", { op: "read_build" }, undefined, undefined, context())
    ).resolves.toMatchObject({
      details: {
        plugin: pkg.name,
        version: pkg.version,
        pluginContract: PLUGIN_CONTRACT,
        daemonContract: PLUGIN_CONTRACT + 1,
        daemonContractNote: null,
      },
    });
    // The health route's header is the one read, and no project state is asked for.
    expect(calls).toEqual([["daemonContract", undefined]]);
  });
});

test("read_build says the daemon stated no contract, and belongs to the controller session too", async () => {
  const { daemon } = recordingDaemon({});
  const toolFor = (active: LegionToolSession) =>
    createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => active,
    });

  for (const active of [claimSession("architect"), claimSession("implementer")]) {
    await expect(
      toolFor(active).execute("", { op: "read_build" }, undefined, undefined, context())
    ).resolves.toMatchObject({
      details: { daemonContract: null, daemonContractNote: "the daemon stated no contract" },
    });
  }
  // The controller holds a claim like any other role, and it is the session that compares two
  // contract numbers when a roll goes wrong (dispatch://INFRA-2694). It reaches no claim grant,
  // so its answer comes from the controller's own branch of the tool.
  const controller = toolFor({ kind: "controller", sessionID: "ses_controller" });
  await expect(
    controller.execute("", { op: "read_build" }, undefined, undefined, context("ses_controller"))
  ).resolves.toMatchObject({
    details: {
      plugin: pkg.name,
      version: pkg.version,
      pluginContract: PLUGIN_CONTRACT,
      daemonContract: null,
      daemonContractNote: "the daemon stated no contract",
    },
  });
  // It takes no field there either.
  await expect(
    controller.execute(
      "",
      { op: "read_build", issue: "LEGION-208" },
      undefined,
      undefined,
      context("ses_controller")
    )
  ).resolves.toMatchObject({
    isError: true,
    content: [{ type: "text", text: 'read_build does not accept field "issue"; it takes none' }],
  });
});

// The project's pause (dispatch://INFRA-3759, check k6). The controller pauses its project with
// one operation, which sends its reason, and resumes it with one more, which names nothing: the
// daemon acts on its own project. Each goes through the real daemon client, so the answer the
// controller reads is the golden the daemon's own test wrote, parsed by the contract's schema.
// A call the tool can tell is wrong is refused before a grant is minted or a request sent.
test("the controller pauses the project with its reason and resumes it, and each answer is the daemon's golden", async () => {
  const { posted, daemon } = goldenDaemon({
    "/legion/v1/project/pause": "project-pause.json",
    "/legion/v1/project/resume": "project-resume.json",
  });
  const minted: string[] = [];
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));
  const reason = "the model provider is refusing every turn";

  const paused = await run({ op: "pause", reason });
  expect(paused.isError).toBeUndefined();
  expect(paused.details).toEqual(daemonFixture("project-pause.json"));
  expect(paused.content).toEqual([{ type: "text", text: JSON.stringify(paused.details) }]);
  expect(posted).toEqual([["/legion/v1/project/pause", { grantId: "controller-grant-1", reason }]]);
  expect(minted).toEqual(["ses_controller"]);

  const resumed = await run({ op: "resume" });
  expect(resumed.isError).toBeUndefined();
  expect(resumed.details).toEqual(daemonFixture("project-resume.json"));
  expect(resumed.content).toEqual([{ type: "text", text: JSON.stringify(resumed.details) }]);
  expect(posted.at(-1)).toEqual(["/legion/v1/project/resume", { grantId: "controller-grant-1" }]);
  expect(minted).toEqual(["ses_controller", "ses_controller"]);

  const refusals = [
    [{ op: "pause" }, "pause requires reason"],
    [{ op: "pause", reason: " " }, "pause requires reason"],
    [
      { op: "pause", reason, issue: "LEGION-208" },
      'pause does not accept field "issue"; it takes reason',
    ],
    [{ op: "resume", reason }, 'resume does not accept field "reason"; it takes none'],
    [{ op: "resume", issue: "LEGION-208" }, 'resume does not accept field "issue"; it takes none'],
  ] as const;
  for (const [parameters, text] of refusals) {
    await expect(run(parameters)).resolves.toEqual({
      content: [{ type: "text", text }],
      details: {},
      isError: true,
    });
  }
  expect(posted).toHaveLength(2);
  expect(minted).toHaveLength(2);
});

// The P0 bound (C3 of dispatch://INFRA-5636, check c-bound-tool). The controller sets the bound
// with one operation, which sends exactly the request the daemon API contract describes, a bound or
// none with its reason, and clears it with another. Each goes through the real daemon client, so
// the answer the controller reads is the golden the daemon's own route test wrote, parsed by the
// contract's schema: the set of 2, then the set of none, whose bound in force and live bound are
// null, then the clear. A call the tool can tell is wrong is refused before a grant is minted.
test("the controller sets and clears the P0 bound, each request the contract's and each answer the daemon's golden", async () => {
  const { posted, daemon } = goldenDaemon({
    "/legion/v1/project/p0-bound": ["p0-bound.json", "p0-bound-none.json"],
    "/legion/v1/project/p0-bound/clear": "p0-bound-clear.json",
  });
  const minted: string[] = [];
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));
  const reason = "the P0 issues outran the gateway";

  const set = await run({ op: "set_p0_bound", bound: 2, reason });
  expect(set.isError).toBeUndefined();
  expect(set.details).toEqual(daemonFixture("p0-bound.json"));
  const none = await run({ op: "set_p0_bound", unbounded: true, reason });
  expect(none.isError).toBeUndefined();
  expect(daemonFixture("p0-bound-none.json")).toMatchObject({ bound: null, live: { bound: null } });
  expect(none.details).toEqual(daemonFixture("p0-bound-none.json"));
  const cleared = await run({ op: "clear_p0_bound", reason: "back to the file" });
  expect(cleared.details).toEqual(daemonFixture("p0-bound-clear.json"));
  expect(posted).toEqual([
    ["/legion/v1/project/p0-bound", { grantId: "controller-grant-1", bound: 2, reason }],
    ["/legion/v1/project/p0-bound", { grantId: "controller-grant-1", unbounded: true, reason }],
    [
      "/legion/v1/project/p0-bound/clear",
      { grantId: "controller-grant-1", reason: "back to the file" },
    ],
  ]);
  // Each request is one the contract describes, as the daemon's request-shape records do.
  expect(LegionProjectP0BoundRequest.safeParse(posted[0]?.[1]).success).toBeTrue();
  expect(LegionProjectP0BoundRequest.safeParse(posted[1]?.[1]).success).toBeTrue();
  expect(LegionProjectP0BoundClearRequest.safeParse(posted[2]?.[1]).success).toBeTrue();

  const refusals = [
    [
      { op: "set_p0_bound", reason },
      "set_p0_bound requires bound, a whole number from 1 to 2147483647, or unbounded true, not both",
    ],
    [
      { op: "set_p0_bound", bound: 2, unbounded: true, reason },
      "set_p0_bound requires bound, a whole number from 1 to 2147483647, or unbounded true, not both",
    ],
    [
      { op: "set_p0_bound", bound: 0, reason },
      "set_p0_bound's bound is a whole number from 1 to 2147483647",
    ],
    [
      { op: "set_p0_bound", bound: 2147483648, reason },
      "set_p0_bound's bound is a whole number from 1 to 2147483647",
    ],
    [{ op: "set_p0_bound", bound: 2 }, "set_p0_bound requires reason"],
    [{ op: "clear_p0_bound" }, "clear_p0_bound requires reason"],
  ] as const;
  for (const [parameters, text] of refusals) {
    await expect(run(parameters)).resolves.toEqual({
      content: [{ type: "text", text }],
      details: {},
      isError: true,
    });
  }
  expect(posted).toHaveLength(3);
  expect(minted).toHaveLength(3);
});

// read_state's summary gives the P0 holders, the bound in force and the live value that stands,
// on one line after the admission cap's, and no such line for a body served below the P0 start's
// contract entry, which carries neither key, nor for a body that carries one key and not the
// other, which no daemon serves: it writes the two together.
test("the read_state summary gives the P0 holders and the P0 bound in force", async () => {
  const base = daemonFixture("state.json") as LegionState;
  const summaryOf = async (state: LegionState) => {
    const { daemon } = recordingDaemon({ state });
    return (
      await controllerTool(daemon, []).execute(
        "",
        { op: "read_state" },
        undefined,
        undefined,
        context("ses_controller")
      )
    ).content[0];
  };
  const admission = base.admission;
  const live = admission?.p0Bound?.live;
  if (
    admission?.p0Holders === undefined ||
    live === undefined ||
    live.setBy.kind !== "controller"
  ) {
    throw new Error(
      "state.json carries no P0 holders or no live P0 bound set by the controller: this test proves nothing"
    );
  }
  expect(await summaryOf(base)).toMatchObject({
    text: expect.stringContaining(
      `\np0 holders ${admission.p0Holders} bound ${admission.p0Bound?.bound} set ${live.bound} by the controller (session ${live.setBy.session}) at ${live.setAt}: ${live.reason}\n`
    ),
  });

  const { p0Holders: _held, p0Bound: _bound, ...older } = admission;
  for (const body of [
    older,
    { ...older, p0Holders: admission.p0Holders },
    { ...older, p0Bound: admission.p0Bound },
  ]) {
    const summary = await summaryOf({ ...base, admission: body });
    expect(summary).toMatchObject({ text: expect.stringContaining("\nadmission cap ") });
    expect(summary).toMatchObject({ text: expect.not.stringContaining("\np0 holders") });
  }
});

// The daemon's refusals reach the controller as every other write's do: the code, then the
// daemon's sentence, which names the pause that stands, the last pause, or the setting that
// turns the pause off. The bodies are the daemon's goldens, which carry that pause beside the
// sentence; the client reads the code and the sentence.
test("a refused pause and a refused resume tell the controller the daemon's code and its sentence", async () => {
  const refusals = [
    [
      { op: "pause", reason: "a second pause" },
      "/legion/v1/project/pause",
      "project-pause-refused.json",
      "PROJECT_ALREADY_PAUSED",
    ],
    [
      { op: "pause", reason: "a pause under tmux" },
      "/legion/v1/project/pause",
      "project-pause-unavailable.json",
      "PAUSE_UNAVAILABLE",
    ],
    [
      { op: "resume" },
      "/legion/v1/project/resume",
      "project-resume-refused.json",
      "PROJECT_NOT_PAUSED",
    ],
  ] as const;
  for (const [parameters, route, golden, code] of refusals) {
    const { daemon } = goldenDaemon({ [route]: golden }, 409);
    await expect(
      controllerTool(daemon, []).execute(
        "",
        parameters,
        undefined,
        undefined,
        context("ses_controller")
      )
    ).resolves.toEqual({
      content: [
        {
          type: "text",
          text: `POST ${route} failed with 409 ${code}: ${daemonFixture(golden).error}`,
        },
      ],
      details: {},
      isError: true,
    });
  }
});

// Neither operation is a claim's: a root architect, a sub-architect and every phase worker are
// refused by the operation's name, before a grant is minted or a request sent.
test("pause and resume are offered to no architect and no phase worker", async () => {
  const { posted, daemon } = goldenDaemon({
    "/legion/v1/project/pause": "project-pause.json",
    "/legion/v1/project/resume": "project-resume.json",
  });
  const claims = EVERY_SESSION.filter((session) => session.kind !== "controller");
  expect(claims).toHaveLength(7);
  for (const session of claims) {
    const tool = createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => session,
    });
    for (const parameters of [{ op: "pause", reason: "the provider is down" }, { op: "resume" }]) {
      await expect(tool.execute("", parameters, undefined, undefined, context())).resolves.toEqual({
        content: [
          {
            type: "text",
            text: `${parameters.op} is not available to ${session.kind === "architect" ? "an" : "a"} ${session.kind} session`,
          },
        ],
        details: {},
        isError: true,
      });
    }
  }
  expect(posted).toEqual([]);
});

// The controller's resume_agent and the project's resume are two operations: resume_agent names
// an agent by issue and role and posts that agent's resume, and resume names no issue and is
// refused the agent's two fields. A resume_agent the daemon refuses because the project is paused
// reaches the controller in the daemon's own words, which name the resume operation that lifts
// the pause, so a controller that meant the project's resume reads which operation that is.
test("resume_agent resumes an agent, resume names no issue, and a paused project's refusal names resume", async () => {
  const { calls, daemon } = recordingDaemon({ answers: { resumeAgent: { state: "launching" } } });
  const tool = controllerTool(daemon, []);
  await tool.execute(
    "",
    { op: "resume_agent", issue: "LEGION-209", role: "tester" },
    undefined,
    undefined,
    context("ses_controller")
  );
  expect(calls.at(-1)).toEqual([
    "resumeAgent",
    { grantId: "controller-grant-1", issue: "LEGION-209", role: "tester" },
  ]);
  const asProject = await tool.execute(
    "",
    { op: "resume", issue: "LEGION-209", role: "tester" },
    undefined,
    undefined,
    context("ses_controller")
  );
  expect(asProject.content).toEqual([
    {
      type: "text",
      text: 'resume does not accept field "issue"; resume does not accept field "role"; it takes none',
    },
  ]);

  const sentence =
    "resume of legion-legion-legion-209-tester refused: the project is paused (pause 7), and a paused project " +
    "starts no turn of a role but the controller's; nothing of legion-legion-legion-209-tester changed. The " +
    "pause lifts only at the project's resume: the controller's resume operation of its legion tool, which " +
    "names no issue, or the operator's legion project resume. Ask again once the project runs";
  const refusing = recordingDaemon({
    answers: {
      resumeAgent: new LegionDaemonApiError(
        "POST",
        "/legion/v1/agents/resume",
        409,
        "PROJECT_PAUSED",
        sentence
      ),
    },
  });
  const refused = await controllerTool(refusing.daemon, []).execute(
    "",
    { op: "resume_agent", issue: "LEGION-209", role: "tester" },
    undefined,
    undefined,
    context("ses_controller")
  );
  expect(refused.isError).toBe(true);
  expect(refused.content).toEqual([
    { type: "text", text: expect.stringContaining("PROJECT_PAUSED") },
  ]);
  expect(refused.content).toEqual([
    { type: "text", text: expect.stringContaining("the controller's resume operation") },
  ]);
});

// The intake's give-ups (dispatch://INFRA-1575, R3). The controller replays a give-up, a message
// the daemon's intake gave up on, by its record id, or every open one of a consumer at or below a
// stream sequence, and discards them the same way with a reason. Each call goes through the real
// daemon client to the controller's route, so the answer the controller reads is the golden the
// daemon's own test wrote, parsed by the contract's schema.
test("the controller's replay_give_up sends one record id to the daemon's controller route and returns its answer", async () => {
  const { posted, daemon } = goldenDaemon({ "/legion/v1/intake/replay": "intake-replay.json" });
  const minted: string[] = [];
  const replayed = await controllerTool(daemon, minted).execute(
    "",
    { op: "replay_give_up", giveUp: 41 },
    undefined,
    undefined,
    context("ses_controller")
  );
  expect(replayed.isError).toBeUndefined();
  expect(replayed.details).toEqual(daemonFixture("intake-replay.json"));
  expect(replayed.content).toEqual([{ type: "text", text: JSON.stringify(replayed.details) }]);
  expect(posted).toEqual([
    ["/legion/v1/intake/replay", { grantId: "controller-grant-1", giveUp: 41 }],
  ]);
  expect(minted).toEqual(["ses_controller"]);
});

// The bulk form names the consumer and the stream sequence the summary gives as its newest. A
// selection the daemon would refuse (GIVE_UP_REQUEST_INVALID) is refused before a grant is minted
// or a request sent: both forms, neither, one half of the bulk form, a number that is no record id
// or stream sequence, and a field the replay does not take.
test("the controller's replay_give_up sends a consumer and every open record at or below a stream sequence (throughSeq)", async () => {
  const { posted, daemon } = goldenDaemon({ "/legion/v1/intake/replay": "intake-replay.json" });
  const minted: string[] = [];
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));
  const consumer = "legion-go-LEGION-dispatch";

  const replayed = await run({ op: "replay_give_up", consumer, throughSeq: 6044896 });
  expect(replayed.details).toEqual(daemonFixture("intake-replay.json"));
  expect(posted).toEqual([
    ["/legion/v1/intake/replay", { grantId: "controller-grant-1", consumer, throughSeq: 6044896 }],
  ]);
  expect(minted).toEqual(["ses_controller"]);

  const refusals = [
    [
      { op: "replay_give_up", giveUp: 41, consumer, throughSeq: 6044896 },
      "replay_give_up takes giveUp, or consumer and throughSeq, not both",
    ],
    [{ op: "replay_give_up" }, "replay_give_up requires giveUp, or consumer and throughSeq"],
    [
      { op: "replay_give_up", consumer },
      "replay_give_up requires throughSeq with consumer, a stream sequence from 1",
    ],
    [
      { op: "replay_give_up", consumer, throughSeq: 0 },
      "replay_give_up requires throughSeq with consumer, a stream sequence from 1",
    ],
    [{ op: "replay_give_up", throughSeq: 6044896 }, "replay_give_up requires consumer"],
    [
      { op: "replay_give_up", giveUp: 41.5 },
      "replay_give_up's giveUp is a give-up's record id, a whole number from 1",
    ],
    [
      { op: "replay_give_up", giveUp: 41, reason: "the fix runs" },
      'replay_give_up does not accept field "reason"; it takes giveUp, consumer, throughSeq',
    ],
  ] as const;
  for (const [parameters, text] of refusals) {
    await expect(run(parameters)).resolves.toEqual({
      content: [{ type: "text", text }],
      details: {},
      isError: true,
    });
  }
  expect(posted).toHaveLength(1);
  expect(minted).toHaveLength(1);
});

test("the controller's discard_give_up sends its reason and returns the daemon's answer", async () => {
  const { posted, daemon } = goldenDaemon({ "/legion/v1/intake/discard": "intake-discard.json" });
  const minted: string[] = [];
  const tool = controllerTool(daemon, minted);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));
  const consumer = "legion-go-LEGION-github";
  const reason = "the listener published these bodies as text, and no fix can apply them";

  const bulk = await run({ op: "discard_give_up", consumer, throughSeq: 812004, reason });
  expect(bulk.isError).toBeUndefined();
  expect(bulk.details).toEqual(daemonFixture("intake-discard.json"));
  expect(bulk.content).toEqual([{ type: "text", text: JSON.stringify(bulk.details) }]);
  const one = await run({ op: "discard_give_up", giveUp: 42, reason });
  expect(one.details).toEqual(daemonFixture("intake-discard.json"));
  expect(posted).toEqual([
    [
      "/legion/v1/intake/discard",
      { grantId: "controller-grant-1", consumer, throughSeq: 812004, reason },
    ],
    ["/legion/v1/intake/discard", { grantId: "controller-grant-1", giveUp: 42, reason }],
  ]);
  expect(minted).toEqual(["ses_controller", "ses_controller"]);
});

// A discard is the one act that ends a fact with no apply, so it says why: one with no reason, or
// a blank one, is refused before a grant is minted or a request sent, and so is one that names no
// give-up.
test("discard_give_up without a reason is refused before any call", async () => {
  const { posted, daemon } = goldenDaemon({ "/legion/v1/intake/discard": "intake-discard.json" });
  const minted: string[] = [];
  const tool = controllerTool(daemon, minted);
  const refusals = [
    [{ op: "discard_give_up", giveUp: 41 }, "discard_give_up requires reason"],
    [{ op: "discard_give_up", giveUp: 41, reason: " " }, "discard_give_up requires reason"],
    [
      { op: "discard_give_up", consumer: "legion-go-LEGION-dispatch", throughSeq: 6044896 },
      "discard_give_up requires reason",
    ],
    [
      { op: "discard_give_up", reason: "it can never apply" },
      "discard_give_up requires giveUp, or consumer and throughSeq",
    ],
  ] as const;
  for (const [parameters, text] of refusals) {
    await expect(
      tool.execute("", parameters, undefined, undefined, context("ses_controller"))
    ).resolves.toEqual({ content: [{ type: "text", text }], details: {}, isError: true });
  }
  expect(posted).toEqual([]);
  expect(minted).toEqual([]);
});

// Neither operation is a claim's: a root architect, a sub-architect and every phase worker are
// refused by the operation's name, before a grant is minted or a request sent.
test("a phase worker and an architect have neither replay_give_up nor discard_give_up", async () => {
  const { posted, daemon } = goldenDaemon({
    "/legion/v1/intake/replay": "intake-replay.json",
    "/legion/v1/intake/discard": "intake-discard.json",
  });
  const claims = EVERY_SESSION.filter((session) => session.kind !== "controller");
  expect(claims).toHaveLength(7);
  for (const session of claims) {
    const tool = createLegionTool({
      pi,
      daemon,
      controllerGrant: noControllerGrant,
      onPhaseCompleted: () => undefined,
      onUpdateAnswered: () => undefined,
      handoffReport: noHandoffReport,
      branchHead: noBranchHead,
      dispatch: noDispatch,
      session: () => session,
    });
    for (const parameters of [
      { op: "replay_give_up", giveUp: 41 },
      { op: "discard_give_up", giveUp: 41, reason: "it can never apply" },
    ]) {
      await expect(tool.execute("", parameters, undefined, undefined, context())).resolves.toEqual({
        content: [
          {
            type: "text",
            text: `${parameters.op} is not available to ${session.kind === "architect" ? "an" : "a"} ${session.kind} session`,
          },
        ],
        details: {},
        isError: true,
      });
    }
  }
  expect(posted).toEqual([]);
});

test("read_state with no field summarizes 92 issues under the tool's limit, naming every one", async () => {
  const state = stateOf92Issues();
  const { daemon } = recordingDaemon({ state });
  const tool = controllerTool(daemon, []);
  const result = await tool.execute(
    "",
    { op: "read_state" },
    undefined,
    undefined,
    context("ses_controller")
  );
  const [block] = result.content;
  const summary = block !== undefined && block.type === "text" ? block.text : "";

  // The INFRA deployment's own state at this size is about 160 KB of JSON, and the tool's output
  // reaches the controller cut at about 41 KB (dispatch://INFRA-329). This one is of that order,
  // and the summary of it is a fifth of the cut.
  expect(Buffer.byteLength(JSON.stringify(state), "utf8")).toBeGreaterThan(100 * 1024);
  expect(Buffer.byteLength(summary, "utf8")).toBeLessThan(30720);
  // Every recorded key is named, on its own line with its phase and status: a summary that drops
  // issues to fit is the bug this replaces.
  expect(summary).toContain("\nissues 92\n");
  const lines = summary.split("\n");
  for (const [key, issue] of Object.entries(state.issues)) {
    expect(lines).toContain(`${key} ${issue.phase} ${issue.status}`);
  }

  expect(summary).toContain("project INFRA boots 9 started 2026-10-30T01:02:03Z");
  expect(summary).toContain(
    "\nadmission cap 8 free 0\nactive INFRA-100 INFRA-101\nwaiting INFRA-102\nblocked INFRA-102 on INFRA-103 INFRA-104\n"
  );
  expect(summary).toContain("\ncapability subagents present\n");
  expect(summary).toContain(
    "\ncapability model-fallback open no fallback configured config capabilities.decided.model-fallback\n"
  );
  // An issue Legion still runs carries its slot, its gate and every claim's state with that
  // claim's last turn times; a hold carries its reason.
  expect(summary).toContain(
    [
      "\nINFRA-100 admitted in_progress",
      "  slot 0 since 2026-10-30T00:00:00Z",
      "  gate 3 approved 2",
      "  architect idle 2026-10-30T00:00:00Z 2026-10-30T00:07:00Z",
      "  planner working 2026-10-30T00:00:00Z 2026-10-30T00:07:00Z",
      "  implementer retired 2026-10-30T00:01:00Z 2026-10-30T00:08:00Z",
      "  tester retired 2026-10-30T00:02:00Z 2026-10-30T00:09:00Z",
      "  reviewer retired 2026-10-30T00:03:00Z 2026-10-30T00:10:00Z",
      "  merger retired 2026-10-30T00:04:00Z 2026-10-30T00:11:00Z\n",
    ].join("\n")
  );
  expect(summary).toContain("\n  hold escalated\n");
  expect(summary).toContain("  gate 3 approved none\n");
  // A done issue's claims are retired, so its line stands alone: that is what pays for naming
  // every key.
  expect(summary).toContain("\nINFRA-146 done done\nINFRA-147 done done\n");
});

// The controller files a clear defect only on a repository both role GitHub Apps are installed on
// (finding-asks.md, step 1 of "File a clear defect no tree owns"), and its role prompt tells it
// that the summary prints the repository lines an operator's `legion state` prints
// (packages/daemon/internal/prompts/roles/controller-root.md). No filter answers for
// repositories, so the summary prints, after the capabilities, cmd/legion's repositoryLines for
// `repositories.live`: one line for each entry GitHub's last read did not name or GitHub answered
// a role App is not installed on, with both reasons where both hold and the name it is served
// under, and none for a repository both Apps are installed on whose last read named it, or one
// GitHub has not answered yet.
test("read_state with no field prints legion state's line for each repository whose read failed or that lacks an App", async () => {
  const state = legionState({
    capabilities: [{ name: "subagents", status: "present", detail: "6 agents discovered" }],
    repositories: {
      live: [
        { repo: "acme/gadgets", apps: { implement: true, review: false } },
        { repo: "acme/widgets", apps: { implement: true, review: true } },
        { repo: "other/tools", apps: { implement: false, review: true } },
        {
          repo: "sjawhar/legion",
          servedAs: "cybermonklabs/legion",
          apps: { implement: false, review: false },
        },
        { repo: "zeta/archive", readError: "GitHub answered 404 for zeta/archive" },
        {
          repo: "zeta/both",
          readError: "GitHub did not answer for zeta/both",
          apps: { implement: true, review: false },
        },
        { repo: "zeta/new" },
      ],
      withoutWebhookApp: [],
      caches: [],
    },
  });
  const { daemon } = recordingDaemon({ state });
  const result = await controllerTool(daemon, []).execute(
    "",
    { op: "read_state" },
    undefined,
    undefined,
    context("ses_controller")
  );
  const [block] = result.content;
  const summary = block !== undefined && block.type === "text" ? block.text : "";

  expect(summary).toContain(
    [
      "\ncapability subagents present",
      "repository acme/gadgets: the review App is not installed on acme/gadgets",
      "repository other/tools: the implement App is not installed on other/tools",
      "repository sjawhar/legion (served as cybermonklabs/legion): the implement and review Apps are not installed on cybermonklabs/legion",
      "repository zeta/archive: GitHub answered 404 for zeta/archive",
      "repository zeta/both: GitHub did not answer for zeta/both; the review App is not installed on zeta/both",
      "issues 0",
    ].join("\n")
  );
  // A served repository, and one GitHub has not answered both reads of yet, have no line, as in
  // `legion state`.
  expect(summary).not.toContain("acme/widgets");
  expect(summary).not.toContain("zeta/new");
});

// A spec step open on an ancestor's approval holds in `designGate.approvedVersion` the version its
// root registered, which no person approved (entry 20261125161152 (gate-basis)): the summary names it as its entry in
// `gatedSteps` does, by its status word and the ancestor's version, and so the step that waits on
// the ancestor's request; a step on no basis keeps its approved line.
test("read_state's summary names a step on an ancestor's basis by its gatedSteps entry, not as approved", async () => {
  const root = (key: string, designGate: unknown, gatedSteps: unknown[]) => ({
    key,
    generation: 1,
    phase: "planning",
    status: "in_progress",
    workers: {},
    designGate,
    gatedSteps,
  });
  const state = legionState({
    issues: {
      "LEGION-207": root(
        "LEGION-207",
        { artifactId: "spec-207", currentVersion: 4, approvedVersion: 4 },
        [
          {
            step: "spec",
            artifactId: "spec-207",
            currentVersion: 4,
            approvedVersion: 4,
            status: "open_on_basis",
            basis: {
              issue: "LEGION-200",
              artifactId: "spec-200",
              version: 5,
              approver: "user:alice",
              approvedAt: "2026-09-20T15:02:41Z",
            },
          },
        ]
      ),
      "LEGION-214": root(
        "LEGION-214",
        { artifactId: "spec-214", currentVersion: 2, approvedVersion: 1 },
        [
          {
            step: "spec",
            artifactId: "spec-214",
            currentVersion: 2,
            approvedVersion: 1,
            status: "pending_on_basis",
            basis: { issue: "LEGION-200", artifactId: "spec-200", version: 6, askId: "ask-200" },
          },
        ]
      ),
      "LEGION-218": root(
        "LEGION-218",
        { artifactId: "spec-218", currentVersion: 3, approvedVersion: 3 },
        [
          {
            step: "spec",
            artifactId: "spec-218",
            currentVersion: 3,
            approvedVersion: 3,
            status: "approved",
          },
        ]
      ),
    },
  });
  const { daemon } = recordingDaemon({ state });
  const result = await controllerTool(daemon, []).execute(
    "",
    { op: "read_state" },
    undefined,
    undefined,
    context("ses_controller")
  );
  const [block] = result.content;
  const summary = block !== undefined && block.type === "text" ? block.text : "";

  expect(summary).toContain(
    "\nLEGION-207 planning in_progress\n  gate 4 open_on_basis LEGION-200 5\n"
  );
  expect(summary).toContain(
    "\nLEGION-214 planning in_progress\n  gate 2 pending_on_basis LEGION-200 6\n"
  );
  expect(summary).toContain("\nLEGION-218 planning in_progress\n  gate 3 approved 3");
  expect(summary).not.toContain("approved 4");
  expect(summary).not.toContain("approved 1");
});

// The controller reads the live trees and the held issues off the summary: one `trees` line
// naming the root of every live tree, `(none)` when no tree runs and no line for a body the daemon
// stripped of `trees`, and one `held` line per held issue with its reason and the issues that hold
// it. The first state is the daemon's golden, read through the client.
test("read_state's summary names the live trees and each held issue with its reason", async () => {
  const admissionLines = (lines: readonly string[]) =>
    lines.filter((line) => /^(active|waiting|trees|blocked|held) /.test(line));

  const golden = await summaryLinesOf(goldenDaemon({ "/legion/v1/state": "state.json" }).daemon);
  expect(admissionLines(golden)).toEqual([
    "active LEGION-208 LEGION-210 LEGION-212 LEGION-218 LEGION-219",
    "waiting LEGION-212 LEGION-211",
    "trees LEGION-208 LEGION-210 LEGION-211 LEGION-212 LEGION-218 LEGION-219",
    "blocked LEGION-209 on LEGION-210 LEGION-213",
    "blocked LEGION-211 on LEGION-210",
    "held LEGION-209 dispatch_blocker by LEGION-210 LEGION-213",
    "held LEGION-214 label_off",
  ]);

  const idle = legionState({
    admission: { cap: 8, active: [], waiting: [], trees: [], blocked: [], held: [], free: 8 },
  });
  expect(admissionLines(await summaryLinesOf(recordingDaemon({ state: idle }).daemon))).toEqual([
    "active (none)",
    "waiting (none)",
    "trees (none)",
  ]);

  const stripped = legionState();
  expect(stripped.admission?.trees).toBeUndefined();
  expect(admissionLines(await summaryLinesOf(recordingDaemon({ state: stripped }).daemon))).toEqual(
    ["active (none)", "waiting (none)"]
  );
});

// What the controller reads of the pause in `read_state` with no field (dispatch://INFRA-3759,
// R6): one line while the project is paused, naming the pause, since when, by whom and why, the
// roles whose stop it still waits on and how many rows it holds; one line while the project
// runs after a pause, naming that pause and when and by whom it was ended; and neither before
// the project's first pause. The two states are the daemon's goldens, read through the client.
test("read_state's summary prints the pause's line while the project is paused and the last pause's line while it runs", async () => {
  const pauseLines = (lines: readonly string[]) =>
    lines.filter((line) => line.startsWith("paused: ") || line.startsWith("last pause: "));

  const paused = await summaryLinesOf(goldenDaemon({ "/legion/v1/state": "state.json" }).daemon);
  const pausedLine =
    "paused: pause 7 since 2026-09-22T09:09:00Z by the controller (session ses_controller): " +
    "the model provider is refusing every turn; waits on 1 role — LEGION-208 implementer; " +
    "holds 2 rows";
  expect(pauseLines(paused)).toEqual([pausedLine]);

  const running = await summaryLinesOf(
    goldenDaemon({ "/legion/v1/state": "state-last-pause.json" }).daemon
  );
  expect(pauseLines(running)).toEqual([
    "last pause: pause 7 since 2026-11-15T09:00:00Z by the operator: " +
      "the model provider is refusing every turn; " +
      "ended 2026-11-15T09:41:30Z by the controller (session ses_controller)",
  ]);

  const unpaused = await summaryLinesOf(recordingDaemon({ state: legionState() }).daemon);
  expect(pauseLines(unpaused)).toEqual([]);

  // A pause every role has stopped for, with nothing held yet, and one that waits on two roles
  // and holds one row.
  const pause = {
    id: 9,
    since: "2026-11-16T08:00:00Z",
    by: { kind: "operator" },
    reason: "a provider outage",
  };
  const settled = legionState({ paused: { ...pause, waitsOn: [], held: [] } });
  expect(pauseLines(await summaryLinesOf(recordingDaemon({ state: settled }).daemon))).toEqual([
    "paused: pause 9 since 2026-11-16T08:00:00Z by the operator: a provider outage; " +
      "waits on no role; holds no row",
  ]);
  const waiting = legionState({
    paused: {
      ...pause,
      waitsOn: [
        {
          claim: "legion-infra-infra-100-architect",
          issue: "INFRA-100",
          role: "architect",
          askedAt: "2026-11-16T08:00:00Z",
        },
        {
          claim: "legion-infra-infra-101-tester",
          issue: "INFRA-101",
          role: "tester",
          askedAt: "2026-11-16T08:00:00Z",
        },
      ],
      held: [{ id: 41, kind: "notice", issue: "INFRA-100", waitedSeconds: 30 }],
    },
  });
  expect(pauseLines(await summaryLinesOf(recordingDaemon({ state: waiting }).daemon))).toEqual([
    "paused: pause 9 since 2026-11-16T08:00:00Z by the operator: a provider outage; " +
      "waits on 2 roles — INFRA-100 architect, INFRA-101 tester; holds 1 row",
  ]);
});

// What the controller reads of the wake pacing in `read_state` with no field: one line with how
// many wakes the pacing holds until their turn and when the earliest of them is due. The
// controller's skill has it read that count before it asks for a start again
// (`skills/legion-controller/references/operations.md`, step 3 of "Actions on an agent"), so a
// summary without the line leaves that step with nothing to read.
test("read_state's summary counts the wakes the pacing holds and says when the earliest is due", async () => {
  const wakesLines = (lines: readonly string[]) =>
    lines.filter((line) => line.startsWith("wakes waiting"));

  // A state without the key says nothing of what waits, so the summary has no such line: the
  // daemon leaves the key off the state it serves a caller it reads as below the contract that
  // added the key (`LegionStateResponse`).
  const unsaid = await summaryLinesOf(recordingDaemon({ state: legionState() }).daemon);
  expect(wakesLines(unsaid)).toEqual([]);

  // Outside a wave the list is empty, and the line says so and names no moment.
  const none = legionState({ wakesWaiting: [] });
  expect(wakesLines(await summaryLinesOf(recordingDaemon({ state: none }).daemon))).toEqual([
    "wakes waiting 0",
  ]);

  // The daemon lists the wakes in claim order, so the earliest is not the first. It is the
  // earliest as a time: read as text, the half second after it would sort before it.
  const wave = legionState({
    admission: {
      cap: 8,
      free: 0,
      active: ["INFRA-100", "INFRA-101"],
      waiting: ["INFRA-102"],
      blocked: [{ issue: "INFRA-102", blockedBy: ["INFRA-103"] }],
    },
    capabilities: [{ name: "subagents", status: "present", detail: "6 agents discovered" }],
    wakesWaiting: [
      {
        claim: "legion-infra-infra-100-architect",
        issue: "INFRA-100",
        role: "architect",
        kind: "catch-up",
        dueAt: "2026-11-16T08:00:23Z",
      },
      {
        claim: "legion-infra-infra-100-implementer",
        issue: "INFRA-100",
        role: "implementer",
        kind: "launch",
        dueAt: "2026-11-16T08:00:08.5Z",
      },
      {
        claim: "legion-infra-infra-101-tester",
        issue: "INFRA-101",
        role: "tester",
        kind: "plugin-refresh",
        dueAt: "2026-11-16T08:00:08Z",
      },
    ],
  });
  const waiting = await summaryLinesOf(recordingDaemon({ state: wave }).daemon);
  const waitingLine = "wakes waiting 3 next 2026-11-16T08:00:08Z";
  expect(wakesLines(waiting)).toEqual([waitingLine]);

  // The daemon's own golden, read through the client: three wakes, the first of them the earliest.
  const golden = await summaryLinesOf(goldenDaemon({ "/legion/v1/state": "state.json" }).daemon);
  expect(wakesLines(golden)).toEqual(["wakes waiting 3 next 2026-09-22T09:17:56Z"]);
});

// What the controller reads of the intake's give-ups in `read_state` with no field: one line for
// each consumer with open give-ups, with how many and the newest stream sequence among them, which
// is the throughSeq of a replay or a discard of all of them, and one line saying none is open.
// A state without the key, served to a caller below the contract that added it, says nothing.
test("the read_state summary prints each consumer's open give-ups and their newest sequence", async () => {
  const giveUpLines = (lines: readonly string[]) =>
    lines.filter((line) => line.startsWith("intake give-ups"));

  const golden = await summaryLinesOf(goldenDaemon({ "/legion/v1/state": "state.json" }).daemon);
  expect(giveUpLines(golden)).toEqual([
    "intake give-ups legion-go-LEGION-dispatch open 2 newest seq 6044896",
    "intake give-ups legion-go-LEGION-github open 1 newest seq 812004",
  ]);
  expect(golden[0]).toContain(
    "intake give-ups <consumer> open <count> newest seq <stream sequence>"
  );

  const none = await summaryLinesOf(
    goldenDaemon({ "/legion/v1/state": "state-stage3.json" }).daemon
  );
  expect(giveUpLines(none)).toEqual(["intake give-ups (none)"]);

  const unsaid = await summaryLinesOf(recordingDaemon({ state: legionState() }).daemon);
  expect(giveUpLines(unsaid)).toEqual([]);
});

test("read_state's filters answer full records, and name what they cannot select", async () => {
  const state = stateOf92Issues();
  const { daemon } = recordingDaemon({ state });
  const tool = controllerTool(daemon, []);
  const run = (parameters: Record<string, unknown>) =>
    tool.execute("", parameters, undefined, undefined, context("ses_controller"));

  // The record a filter answers is the state's own, every claim and count in it.
  await expect(run({ op: "read_state", issue: "INFRA-101" })).resolves.toEqual({
    content: [
      {
        type: "text",
        text: JSON.stringify({ issues: { "INFRA-101": state.issues["INFRA-101"] } }),
      },
    ],
    details: { issues: { "INFRA-101": state.issues["INFRA-101"] } },
  });
  await expect(
    run({ op: "read_state", issues: ["INFRA-101", "INFRA-130"] })
  ).resolves.toMatchObject({
    details: {
      issues: { "INFRA-101": state.issues["INFRA-101"], "INFRA-130": state.issues["INFRA-130"] },
    },
  });
  const reviewing = Object.fromEntries(
    Object.entries(state.issues).filter(([, issue]) => issue.phase === "reviewing")
  );
  expect(Object.keys(reviewing).length).toBeGreaterThan(1);
  await expect(run({ op: "read_state", phase: "reviewing" })).resolves.toMatchObject({
    details: { issues: reviewing },
  });
  await expect(run({ op: "read_state", settings: true })).resolves.toMatchObject({
    details: { settings: state.settings },
  });

  const refusals = [
    [{ op: "read_state", issue: "INFRA-999" }, "The daemon has no record for INFRA-999"],
    [
      { op: "read_state", issues: ["INFRA-998", "INFRA-999"] },
      "The daemon has no record for INFRA-998, INFRA-999",
    ],
    [
      { op: "read_state", issue: "INFRA-101", settings: true },
      "read_state takes one of issue, issues, phase, settings, not issue and settings",
    ],
    [{ op: "read_state", phase: "nowhere" }, `read_state's phase is one of ${LEGION_PHASES[0]}`],
    [{ op: "read_state", settings: false }, "read_state's settings is true"],
    [{ op: "read_state", issues: "INFRA-101" }, "read_state's issues is a list of issue keys"],
  ] as const;
  for (const [parameters, message] of refusals) {
    const result = await run(parameters);
    expect(result.isError).toBe(true);
    expect(result.content).toEqual([{ type: "text", text: expect.stringContaining(message) }]);
  }
});
