import { expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import * as legionApi from "@legion/contracts/legion-api";
import { LegionReadyRequest } from "@legion/contracts/legion-api";
import * as ts from "typescript";
import { z } from "zod";
import {
  createLegionDaemonClient,
  daemonUnreachable,
  LegionDaemonApiError,
  type LegionDaemonClient,
  LegionDaemonContractError,
  type ReadyInput,
  retryWhileDaemonRestarts,
} from "./daemon-client";

/** A response the daemon's own golden test wrote (`packages/daemon/internal/api`). */
function daemonFixture(name: string): Record<string, unknown> {
  return JSON.parse(
    readFileSync(path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api", name), "utf8")
  );
}

/** `body` without `key`: what a daemon that dropped a field the contract requires would answer. */
function without(body: Record<string, unknown>, key: string): Record<string, unknown> {
  return Object.fromEntries(Object.entries(body).filter(([name]) => name !== key));
}

interface Recorded {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

function daemon(respond: (url: URL) => Response): {
  readonly fetch: typeof fetch;
  readonly requests: Recorded[];
} {
  const requests: Recorded[] = [];
  const stub = (async (input, init) => {
    const url = new URL(input.toString());
    requests.push({
      method: init?.method ?? "GET",
      url: url.toString(),
      body: init?.body == null ? undefined : JSON.parse(init.body.toString()),
    });
    return respond(url);
  }) as typeof fetch;
  return { fetch: stub, requests };
}

/** The contract the build under test speaks. Every request states it, and the refusal cases
 * below set the contract the daemon states against it. */
const CONTRACT = 60;

const registration = {
  bootToken: "boot-token-from-the-pane",
  sessionId: "ses_architect_208",
  ompSessionFile: "/state/home/.omp/agent/sessions/ses_architect_208.jsonl",
  agentId: "ses_architect_208",
  pluginContract: 1,
};

test("registers with the claim wire's request and returns the claim the daemon issued", async () => {
  const issued = daemonFixture("register.json");
  const { fetch, requests } = daemon(() => Response.json(issued));
  const client = createLegionDaemonClient("http://daemon.test/", {
    pluginContract: CONTRACT,
    fetch,
  });

  await expect(client.register(registration)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: registration },
  ]);
});

test("refuses a registration answer that drops a field the daemon API contract requires, without quoting its secret", async () => {
  const registered = daemonFixture("register.json");
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json(without(registered, "promptAgents"))).fetch,
  });

  const refused = client.register(registration);
  await expect(refused).rejects.toBeInstanceOf(LegionDaemonContractError);
  await expect(refused).rejects.toThrow("POST /legion/v1/claims/register answered 200");
  await refused.catch((error: Error) => {
    expect(error.message).toContain("promptAgents");
    expect(error.message).not.toContain(String(registered.secret));
  });
});

test("a refusal carries its status and the daemon's sentence", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json(daemonFixture("error.json"), { status: 409 })).fetch,
  });

  const refused = await client.register(registration).catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({
    status: 409,
    detail: "Worker respawn must resume the same agent session",
    message:
      "POST /legion/v1/claims/register failed with 409: Worker respawn must resume the same agent session",
  });
});

test("a refusal whose body is not the daemon's refusal shape keeps its status and says so", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => new Response("upstream timed out", { status: 504 })).fetch,
  });

  const refused = await client.register(registration).catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({ status: 504 });
  expect((refused as LegionDaemonApiError).detail).toContain("upstream timed out");
  expect((refused as LegionDaemonApiError).detail).toContain("not the daemon's refusal");
});

test("ready and exit post the claim wire's requests and accept only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch,
  });
  const ready = {
    claimToken: "legion-legion-legion-208-architect",
    sessionId: "ses_architect_208",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
    generation: 3,
  };

  await expect(client.ready(ready)).resolves.toBeUndefined();
  await expect(client.exit({ ...ready, reason: "session ended" })).resolves.toBeUndefined();
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/ready", body: ready },
    {
      method: "POST",
      url: "http://daemon.test/legion/v1/claims/exit",
      body: { ...ready, reason: "session ended" },
    },
  ]);

  const talkative = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json({})).fetch,
  });
  await expect(talkative.ready(ready)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("activity posts the claim wire's report and accepts only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch,
  });
  const report = {
    claimToken: "legion-legion-legion-208-implementer",
    sessionId: "ses_implementer_208",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
    activity: {
      at: "2026-11-01T12:00:00.000Z",
      backgroundWork: { running: true, kinds: ["async-job"] },
      phase: { nextReminderAt: "2026-11-01T12:05:00.000Z" },
    },
  };

  await expect(client.activity(report)).resolves.toBeUndefined();
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/activity", body: report },
  ]);

  const talkative = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json({})).fetch,
  });
  await expect(talkative.activity(report)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("the release records are read by a POST carrying the claim's token and secret, and a list the daemon left out is refused", async () => {
  const records = {
    releases: [
      {
        id: 1,
        issue: "LEGION-208",
        generation: 2,
        roles: ["implementer", "tester"],
        at: "2026-11-01T12:10:00Z",
        bundleBytes: 2048,
      },
    ],
    resumes: [
      {
        id: 1,
        release: 1,
        issue: "LEGION-208",
        role: "implementer",
        kind: "task",
        at: "2026-11-01T12:40:00Z",
        podReadyAt: "2026-11-01T12:41:00Z",
        firstTurnAt: "2026-11-01T12:42:00Z",
        stuck: false,
      },
    ],
    holds: [
      {
        id: 1,
        issue: "LEGION-209",
        reason: "background_work",
        detail: "async-job",
        firstAt: "2026-11-01T12:50:00Z",
        at: "2026-11-01T13:00:00Z",
        passes: 11,
      },
    ],
  };
  const { fetch, requests } = daemon(() => Response.json(records));
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch,
  });
  // The pod holds its claim and no operator token, so the claim's own token and secret
  // authenticate the read, and the secret rides in the body rather than a query string.
  const window = {
    claimToken: "legion-legion-legion-208-implementer",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
    since: "2026-11-01T12:00:00Z",
  };

  await expect(client.releaseRecords(window)).resolves.toEqual(records as never);
  expect(requests).toEqual([
    {
      method: "POST",
      url: "http://daemon.test/legion/v1/claims/release-records",
      body: window,
    },
  ]);

  // A window with nothing in it is three empty lists; a list the daemon left out is no answer.
  const empty = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json({ releases: [], resumes: [], holds: [] })).fetch,
  });
  await expect(empty.releaseRecords(window)).resolves.toEqual({
    releases: [],
    resumes: [],
    holds: [],
  });
  const partial = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json({ releases: [], resumes: [] })).fetch,
  });
  await expect(partial.releaseRecords(window)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("a ready carrying the capability report is the daemon's golden ReadyRequest, field for field", async () => {
  // ready.json is what the daemon's golden test wrote of a `claim.ReadyRequest` with a report, so
  // sending it as the client's input and recording the body it posts pins both sides to it.
  const fixture = daemonFixture("ready.json");
  const golden: ReadyInput = LegionReadyRequest.parse(fixture);
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch,
  });

  await expect(client.ready(golden)).resolves.toBeUndefined();
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/ready", body: fixture },
  ]);
  expect(golden.capabilities?.rows).toHaveLength(3);
});

test("reads the daemon's state over a bodyless GET", async () => {
  const state = daemonFixture("state.json");
  const { fetch, requests } = daemon(() => Response.json(state));

  await expect(
    createLegionDaemonClient("http://daemon.test", { pluginContract: CONTRACT, fetch }).state()
  ).resolves.toEqual(state as never);
  expect(requests).toEqual([
    { method: "GET", url: "http://daemon.test/legion/v1/state", body: undefined },
  ]);
});

test("a state read hands fetch its caller's signal, and an abort ends a read the daemon never answers", async () => {
  const handed: (AbortSignal | null | undefined)[] = [];
  const hung = (async (_input, init) => {
    handed.push(init?.signal);
    const { promise, reject } = Promise.withResolvers<Response>();
    init?.signal?.addEventListener("abort", () => reject(init.signal?.reason), { once: true });
    return promise;
  }) as typeof fetch;
  const controller = new AbortController();
  const late = new Error("the reader stopped waiting");

  const read = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: hung,
  }).state({
    signal: controller.signal,
  });
  controller.abort(late);

  await expect(read).rejects.toBe(late);
  expect(handed).toEqual([controller.signal]);
});

test("posts every Stage 3 workflow request through its matching route", async () => {
  type WorkflowClient = {
    readonly grant: (body: object) => Promise<unknown>;
    readonly handoffComplete: (body: object) => Promise<unknown>;
    readonly issueStatus: (body: object) => Promise<unknown>;
    readonly freshSession: (body: object) => Promise<unknown>;
    readonly compact: (body: object) => Promise<unknown>;
    readonly gateRegister: (body: object) => Promise<unknown>;
    readonly waveRelease: (body: object) => Promise<unknown>;
    readonly phaseBackward: (body: object) => Promise<unknown>;
    readonly phaseRetry: (body: object) => Promise<unknown>;
    readonly signOff: (body: object) => Promise<unknown>;
    readonly childPark: (body: object) => Promise<unknown>;
    readonly childRerun: (body: object) => Promise<unknown>;
  };
  const calls: ReadonlyArray<readonly [keyof WorkflowClient, string, object, string]> = [
    [
      "grant",
      "/legion/v1/grants",
      { sessionId: "ses_208", secret: "claim-secret", tree: "LEGION-208", issue: "LEGION-209" },
      "grant.json",
    ],
    // An implementing round's completion names the Dispatch handoff record and the pushed tip,
    // which the legion tool found in the pane; the daemon answers the note READY leaves for a pull
    // request already merged.
    [
      "handoffComplete",
      "/legion/v1/handoff/complete",
      {
        grantId: "grant-208",
        summary: "done",
        verdict: "",
        ready: false,
        commit: "abc123",
        handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
        handoffVersion: 2,
      },
      "handoff-complete.json",
    ],
    [
      "issueStatus",
      "/legion/v1/issues/status",
      { grantId: "grant-208", issue: "LEGION-208", status: "todo" },
      "issue-status.json",
    ],
    [
      "freshSession",
      "/legion/v1/claims/fresh-session",
      { grantId: "grant-208", issue: "LEGION-209", role: "implementer" },
      "fresh-session.json",
    ],
    // The compaction's answer is the one claim recovery that carries a body: whether the
    // session was compacted, and the reason it was not.
    [
      "compact",
      "/legion/v1/claims/compact",
      { grantId: "grant-208", issue: "LEGION-209", role: "implementer" },
      "compact.json",
    ],
    [
      "gateRegister",
      "/legion/v1/gates/register",
      {
        grantId: "grant-208",
        issue: "LEGION-208",
        artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
        version: 7,
      },
      "gate-register.json",
    ],
    [
      "waveRelease",
      "/legion/v1/waves/release",
      { grantId: "grant-208", issues: ["LEGION-209"] },
      "wave-release.json",
    ],
    [
      "phaseBackward",
      "/legion/v1/phase/backward",
      { grantId: "grant-208", to: "implementing", reason: "test failed" },
      "phase-backward.json",
    ],
    [
      "phaseRetry",
      "/legion/v1/phase/retry",
      { grantId: "grant-208", issue: "LEGION-208", decision: "retry" },
      "phase-retry.json",
    ],
    [
      "signOff",
      "/legion/v1/signoff",
      { grantId: "grant-208", issue: "LEGION-208" },
      "signoff.json",
    ],
    [
      "childPark",
      "/legion/v1/children/park",
      { grantId: "grant-208", issue: "LEGION-209" },
      "child-park.json",
    ],
    [
      "childRerun",
      "/legion/v1/children/rerun",
      { grantId: "grant-208", issue: "LEGION-209" },
      "child-rerun.json",
    ],
  ];
  const { fetch, requests } = daemon((url) => {
    const call = calls.find(([, route]) => route === url.pathname);
    return Response.json(call === undefined ? { error: "no route" } : daemonFixture(call[3]));
  });
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch,
  }) as unknown as WorkflowClient;

  for (const [operation, route, body, fixture] of calls) {
    expect(client[operation], `${operation} is available`).toBeDefined();
    await expect(client[operation](body)).resolves.toEqual(daemonFixture(fixture));
    expect(requests.at(-1)).toEqual({
      method: "POST",
      url: `http://daemon.test${route}`,
      body,
    });
  }
});

test("a workflow refusal surfaces its stable code and message", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() =>
      Response.json(
        { code: "GATE_UNAPPROVED", error: "the current spec version needs approval" },
        { status: 409 }
      )
    ).fetch,
  }) as unknown as {
    readonly gateRegister?: (body: object) => Promise<unknown>;
  };

  expect(client.gateRegister, "gateRegister is available").toBeDefined();
  if (client.gateRegister === undefined) return;
  const refused = await client
    .gateRegister({
      grantId: "grant-208",
      issue: "LEGION-208",
      artifactId: "d2f1c6b4-8e07-4a53-9c1d-6b8f2e5a7093",
      version: 7,
    })
    .catch((error: unknown) => error);
  expect(refused).toMatchObject({
    code: "GATE_UNAPPROVED",
    detail: "the current spec version needs approval",
    message:
      "POST /legion/v1/gates/register failed with 409 GATE_UNAPPROVED: the current spec version needs approval",
  });
});

test("a controller registers on the claim route with its capability and reads the controller's answer", async () => {
  const issued = daemonFixture("register-controller.json");
  const { fetch, requests } = daemon(() => Response.json(issued));
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch,
  });
  const capability = { ...registration, bootToken: "controller-capability" };

  await expect(client.registerController(capability)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: capability },
  ]);
  // A claim's registration is not a controller's, nor the other way round.
  const claimAnswer = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json(daemonFixture("register.json"))).fetch,
  });
  await expect(claimAnswer.registerController(capability)).rejects.toBeInstanceOf(
    LegionDaemonContractError
  );
  await expect(
    createLegionDaemonClient("http://daemon.test", { pluginContract: CONTRACT, fetch }).register(
      registration
    )
  ).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("the registered controller mints its grant with the session form that names no tree", async () => {
  const { fetch, requests } = daemon(() => Response.json(daemonFixture("grant.json")));
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch,
  });

  await expect(
    client.controllerGrant({ sessionId: "ses_controller", secret: "registration-secret" })
  ).resolves.toEqual(daemonFixture("grant.json") as never);
  expect(requests).toEqual([
    {
      method: "POST",
      url: "http://daemon.test/legion/v1/grants",
      body: { sessionId: "ses_controller", secret: "registration-secret" },
    },
  ]);
});

test("the registration, the grant and the handoff completion are read whole when each carries a key no schema names, and the key is not in the value", async () => {
  const registered = daemonFixture("register.json");
  const granted = daemonFixture("grant.json");
  const completed = daemonFixture("handoff-complete.json");
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon((url) => {
      if (url.pathname === "/legion/v1/claims/register")
        return Response.json({ ...registered, roleTokens: { reviewer: "t" } });
      if (url.pathname === "/legion/v1/grants") return Response.json({ ...granted, scope: "repo" });
      return Response.json({ ...completed, mergeQueuePosition: 2 });
    }).fetch,
  });

  const claim = await client.register(registration);
  expect(claim).toEqual(registered as never);
  expect(claim).not.toHaveProperty("roleTokens");

  const grant = await client.grant({
    sessionId: "ses_208",
    secret: "claim-secret",
    tree: "LEGION-208",
    issue: "LEGION-209",
  });
  expect(grant).toEqual(granted as never);
  expect(grant).not.toHaveProperty("scope");

  const completion = await client.handoffComplete({
    grantId: "grant-208",
    summary: "done",
    verdict: "",
    ready: false,
    commit: "abc123",
    handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
    handoffVersion: 2,
  });
  expect(completion).toEqual(completed as never);
  expect(completion).not.toHaveProperty("mergeQueuePosition");
});

test("a body that drops a field the plugin reads, or carries one of a type the contract does not describe, is refused by the path of the field that is wrong", async () => {
  const state = daemonFixture("state.json");
  const issues = state.issues as Record<string, Record<string, unknown>>;

  const dropped = await createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() => Response.json(without(state, "issues"))).fetch,
  })
    .state()
    .catch((error: unknown) => error);
  expect(dropped).toBeInstanceOf(LegionDaemonContractError);
  expect((dropped as Error).message).toContain("issues");

  const mistyped = await createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() =>
      Response.json({
        ...state,
        issues: { ...issues, "LEGION-208": { ...issues["LEGION-208"], generation: "three" } },
      })
    ).fetch,
  })
    .state()
    .catch((error: unknown) => error);
  expect(mistyped).toBeInstanceOf(LegionDaemonContractError);
  expect((mistyped as Error).message).toContain("issues.LEGION-208.generation");
});

/** A daemon whose every route refuses with that status, that code, and a key no schema names. */
function refusing(status: number, code: string): LegionDaemonClient {
  return createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: daemon(() =>
      Response.json(
        { code, error: "Dispatch refused the write", retryAfterSeconds: 30 },
        { status }
      )
    ).fetch,
  });
}

test("a 409 refusal keeps its stable code through a key no schema names", async () => {
  const refused = await refusing(409, "GATE_UNAPPROVED")
    .state()
    .catch((error: unknown) => error);

  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({
    status: 409,
    code: "GATE_UNAPPROVED",
    detail: "Dispatch refused the write",
  });
});

test("a 502 refusal keeps its stable code through a key no schema names, and is the daemon's own answer, which is handed back rather than retried for ten minutes", async () => {
  const client = refusing(502, "DISPATCH_FAILED");
  const refused = await client.state().catch((error: unknown) => error);

  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({ status: 502, code: "DISPATCH_FAILED" });
  // A 502 the daemon itself coded is not a restarting daemon, so the retry hands it straight
  // back: a worker is told why it was refused instead of waiting out the restart budget.
  expect(daemonUnreachable(refused)).toBeUndefined();
  let clock = 0;
  const waits: number[] = [];
  let attempts = 0;
  const retried = await retryWhileDaemonRestarts(
    "read the state",
    async () => {
      attempts += 1;
      return client.state();
    },
    {
      now: () => clock,
      sleep: async (ms: number) => {
        waits.push(ms);
        clock += ms;
      },
    }
  ).catch((error: unknown) => error);
  expect(retried).toBeInstanceOf(LegionDaemonApiError);
  expect(retried).toMatchObject({ status: 502, code: "DISPATCH_FAILED" });
  expect(attempts).toBe(1);
  expect(waits).toEqual([]);
});

/** A daemon that answers `body` with `contract` in its `Legion-Daemon-Contract` header, and
 * states no contract where `contract` is undefined. */
function stating(body: unknown, contract: number | undefined, status = 200): typeof fetch {
  return (async (_input, _init) =>
    new Response(JSON.stringify(body), {
      status,
      headers: {
        "Content-Type": "application/json",
        ...(contract === undefined ? {} : { "Legion-Daemon-Contract": String(contract) }),
      },
    })) as typeof fetch;
}

/** The state as a daemon serves it to a build that reads only what plugin code reads: every
 * field of `LEGION_ANSWER_READS` and nothing else. */
const PARED_STATE = {
  daemon: { project: "legion" },
  issues: { "LEGION-208": { phase: "implementing" } },
};

test("an answer with every field no plugin code reads left out is read whole", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating(PARED_STATE, CONTRACT),
  });

  await expect(client.state()).resolves.toEqual(PARED_STATE as never);

  // The per-issue record and the health body, pared the same way.
  const record = { phase: "done", check: { role: "tester", looks: 2, last: false } };
  const reading = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating(record, CONTRACT),
  });
  await expect(reading.record("LEGION-208")).resolves.toEqual(record as never);

  const healthy = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating({ project: "legion" }, CONTRACT),
  });
  await expect(healthy.health()).resolves.toEqual({ project: "legion" });
});

test("an answer whose field carries a type the contract does not describe is refused", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating({ ...PARED_STATE, issues: { "LEGION-208": { phase: 7 } } }, CONTRACT),
  });

  const refused = await client.state().catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionDaemonContractError);
  expect((refused as Error).message).toContain("issues.LEGION-208.phase");
});

/** The refusal a build at `CONTRACT` raises for a state body it cannot read, from a daemon
 * stating `daemon`. The body drops `issues`, which plugin code reads, and carries a word no
 * message may quote. */
async function refusalFrom(daemonContract: number | undefined): Promise<Error> {
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating({ daemon: { project: "legion" }, secret: BODY_SECRET }, daemonContract),
  });
  return (await client.state().catch((error: unknown) => error)) as Error;
}

/** A value only the body carries: no refusal message may quote it. */
const BODY_SECRET = "U3VwZXJ2aXNlZEJ5TGVnaW9u";

test("a refusal from a build below the daemon says the pod waits for its update round", async () => {
  const refused = await refusalFrom(CONTRACT + 1);

  expect(refused).toBeInstanceOf(LegionDaemonContractError);
  expect(refused.message).toContain("issues");
  expect(refused.message).toContain(`This build speaks daemon API contract ${CONTRACT}.`);
  expect(refused.message).toContain(`The daemon speaks ${CONTRACT + 1}.`);
  expect(refused.message).toContain(
    "This pod is behind the daemon, and it waits for its update round."
  );
  expect(refused.message).not.toContain(BODY_SECRET);
});

test("a refusal from a build above the daemon says the daemon is the older of the two", async () => {
  const refused = await refusalFrom(CONTRACT - 1);

  expect(refused).toBeInstanceOf(LegionDaemonContractError);
  expect(refused.message).toContain(`This build speaks daemon API contract ${CONTRACT}.`);
  expect(refused.message).toContain(`The daemon speaks ${CONTRACT - 1}.`);
  expect(refused.message).toContain(
    "The daemon is the older of the two, and the update round will not cure it."
  );
  expect(refused.message).not.toContain(BODY_SECRET);
});

test("a refusal of an answer carrying no contract says the daemon stated none", async () => {
  const refused = await refusalFrom(undefined);

  expect(refused).toBeInstanceOf(LegionDaemonContractError);
  expect(refused.message).toContain(`This build speaks daemon API contract ${CONTRACT}.`);
  expect(refused.message).toContain(
    "The daemon stated no contract, so this build cannot say which side is behind."
  );
  expect(refused.message).not.toContain(BODY_SECRET);
});

test("a refusal raised on the retry of a dropped attempt says the earlier attempt may have been applied", async () => {
  // The first attempt loses its connection before the daemon answers, as a restart makes it;
  // the second reaches a daemon whose answer this build cannot read.
  let attempts = 0;
  const restarting = (async (_input, _init) => {
    attempts += 1;
    if (attempts === 1) throw new TypeError("fetch failed");
    return new Response(JSON.stringify({ daemon: { project: "legion" }, secret: BODY_SECRET }), {
      status: 200,
      headers: {
        "Content-Type": "application/json",
        "Legion-Daemon-Contract": String(CONTRACT + 1),
      },
    });
  }) as typeof fetch;
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: restarting,
  });

  let clock = 0;
  const refused = (await retryWhileDaemonRestarts("handoff_complete", () => client.state(), {
    now: () => clock,
    sleep: async (ms: number) => {
      clock += ms;
    },
  }).catch((error: unknown) => error)) as Error;

  expect(refused).toBeInstanceOf(LegionDaemonContractError);
  expect(attempts).toBe(2);
  expect(refused.message).toContain(
    "an earlier attempt of handoff_complete lost its connection before the daemon answered, and may have been applied: read_record of your own issue says whether it landed"
  );
  expect(refused.message).toContain(
    "This pod is behind the daemon, and it waits for its update round."
  );
  expect(refused.message).not.toContain(BODY_SECRET);
});

test("every request states this build's contract, a GET included", async () => {
  const stated: { readonly method: string; readonly contract: string | null }[] = [];
  const recording = (async (input, init) => {
    stated.push({
      method: init?.method ?? "GET",
      contract: new Headers(init?.headers).get("Legion-Plugin-Contract"),
    });
    const url = new URL(input.toString());
    if (url.pathname === "/legion/v1/state") return Response.json(PARED_STATE);
    if (url.pathname === "/healthz") return Response.json({ project: "legion" });
    if (url.pathname.endsWith("/record")) return Response.json({ phase: "implementing" });
    return new Response(null, { status: 204 });
  }) as typeof fetch;
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: recording,
  });

  await client.state();
  await client.record("LEGION-208");
  await client.health();
  await client.daemonContract();
  await client.exit({
    claimToken: "legion-legion-legion-208-architect",
    sessionId: "ses_architect_208",
    secret: "U3VwZXJ2aXNlZEJ5TGVnaW9u",
    generation: 3,
    reason: "session ended",
  });

  expect(stated).toEqual([
    { method: "GET", contract: String(CONTRACT) },
    { method: "GET", contract: String(CONTRACT) },
    { method: "GET", contract: String(CONTRACT) },
    { method: "GET", contract: String(CONTRACT) },
    { method: "POST", contract: String(CONTRACT) },
  ]);
});

test("a record read answers undefined for RECORD_NOT_FOUND and throws for every other refusal", async () => {
  const absent = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating(
      { code: "RECORD_NOT_FOUND", error: "The daemon has no record for LEGION-404" },
      CONTRACT,
      404
    ),
  });
  await expect(absent.record("LEGION-404")).resolves.toBeUndefined();

  // A 404 of another code is a read that failed, not an absent record.
  const otherCode = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating({ code: "ISSUE_NOT_FOUND", error: "no such issue" }, CONTRACT, 404),
  });
  await expect(otherCode.record("LEGION-404")).rejects.toBeInstanceOf(LegionDaemonApiError);

  const refused = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating({ error: "the daemon is restarting" }, CONTRACT, 503),
  });
  await expect(refused.record("LEGION-208")).rejects.toBeInstanceOf(LegionDaemonApiError);

  const unreadable = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating({ status: "ok" }, CONTRACT),
  });
  await expect(unreadable.record("LEGION-208")).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("the daemon's contract is read from the header, and no body is parsed", async () => {
  const requests: string[] = [];
  const unreadable = (async (input) => {
    requests.push(new URL(input.toString()).pathname);
    return new Response("this is no JSON body", {
      status: 200,
      headers: { "Legion-Daemon-Contract": "61" },
    });
  }) as typeof fetch;
  const client = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: unreadable,
  });

  await expect(client.daemonContract()).resolves.toBe(61);
  expect(requests).toEqual(["/healthz"]);
  // The same answer through `health`, which does parse, is refused: the two differ by the body.
  await expect(client.health()).rejects.toBeInstanceOf(LegionDaemonContractError);

  const silent = createLegionDaemonClient("http://daemon.test", {
    pluginContract: CONTRACT,
    fetch: stating({ project: "legion" }, undefined),
  });
  await expect(silent.daemonContract()).resolves.toBeUndefined();
});

/** `compute`, run at the first call and held after it. Every derived value of the two censuses
 * below reads only files and schemas no test changes, so computing one a second time would read
 * the same bytes and build the same answer. */
function once<T>(compute: () => T): () => T {
  let held: { readonly value: T } | undefined;
  return () => {
    held ??= { value: compute() };
    return held.value;
  };
}

// ─────────────────────────────────────────────────────────────────────────────
// The table census: the answers this client parses, held to the daemon's table.
//
// `packages/contracts/fixtures/daemon-api/answers.json` is the daemon's own table of the
// answers a plugin parses (`packages/daemon/internal/api/answers.go`), one row per response
// schema. This census reads the client's source and fails when either side names an answer the
// other does not. The two sides are joined by route and by nothing else: no mapping of schema
// to row id is written here, so neither side can be taught to agree with the other.
// ─────────────────────────────────────────────────────────────────────────────

/** The four helpers of `daemon-client.ts` that read an answer: `read` and `post` parse a success
 * body, `acknowledge` parses no schema at all and accepts only the empty 204, and `parseBody` is
 * what `call` runs over an answer that is not a success. A call of one of them is a parse site.
 * `call` is not among them: `daemonContract()` reaches the daemon through `call` and reads the
 * stated contract off the header, parsing no schema, so it is no parse site and the table owes
 * it no row. */
const PARSE_HELPERS = new Set(["read", "post", "acknowledge", "parseBody"]);

/** One call of one of those helpers: the schema identifier it names (`undefined` at an
 * `acknowledge`, which names none), the route the answer is parsed on (`undefined` at the
 * `parseBody` of a refusal, which carries no path of its own) and where the call stands. */
interface ParseSite {
  readonly schema: string | undefined;
  readonly route: string | undefined;
  readonly where: string;
}

/** The route a path argument names, each substitution of a template literal written `{}` as
 * `answers.json` writes a route's parameter (`GET /legion/v1/issues/{key}/record`, whose `{key}`
 * this census writes `{}` as well). Undefined where this pass cannot read the argument as a
 * literal, which fails the census rather than passing the call over. */
function literalPath(node: ts.Expression | undefined): string | undefined {
  if (node === undefined) return undefined;
  if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node)) return node.text;
  if (ts.isTemplateExpression(node)) {
    return node.templateSpans.reduce(
      (text, span) => `${text}{}${span.literal.text}`,
      node.head.text
    );
  }
  return undefined;
}

/** The method a `read` call names, which is a literal of the two the helper takes. */
function literalMethod(node: ts.Expression | undefined): string | undefined {
  if (node === undefined || !ts.isStringLiteral(node)) return undefined;
  return node.text === "GET" || node.text === "POST" ? node.text : undefined;
}

/** The schema a call names, which is a plain identifier or nothing this pass reads. */
function schemaIdentifier(node: ts.Expression | undefined): string | undefined {
  return node !== undefined && ts.isIdentifier(node) ? node.text : undefined;
}

/** What one call of one helper is: a parse site, or the sentence saying why this pass refuses to
 * read it. The arguments are the helpers' own: `read(method, path, schema, …)`,
 * `post(path, request, response, body)` — which answers a POST, since `post` hands its path to
 * `read` with `"POST"` — `acknowledge(path, body, …)` and `parseBody(schema, body)`. */
function siteOfCall(
  helper: string,
  args: ts.NodeArray<ts.Expression>,
  where: string
): ParseSite | string {
  if (helper === "parseBody") {
    const schema = schemaIdentifier(args.at(0));
    return schema === undefined
      ? `${where}: the schema this parseBody call names is no plain identifier`
      : { schema, route: undefined, where };
  }
  if (helper === "acknowledge") {
    const route = literalPath(args.at(0));
    return route === undefined
      ? `${where}: this acknowledge call's path is no literal this census can read`
      : { schema: undefined, route: `POST ${route}`, where };
  }
  const method = helper === "post" ? "POST" : literalMethod(args.at(0));
  const route = literalPath(args.at(helper === "post" ? 0 : 1));
  const schema = schemaIdentifier(args.at(2));
  if (method === undefined) {
    return `${where}: this read call's method is no "GET" or "POST" literal`;
  }
  if (route === undefined) {
    return `${where}: this ${helper} call's path is no literal this census can read`;
  }
  if (schema === undefined) {
    return `${where}: the schema this ${helper} call names is no plain identifier`;
  }
  return { schema, route: `${method} ${route}`, where };
}

/** Every parse site of the client's source, and the calls this pass refuses to read.
 *
 * A call standing inside the declaration of one of the four helpers is no site: there the
 * schema and the path are that helper's own parameters, named by the call that reached it, and
 * the site is that call. Every other call of one of the four is read or refused — the census
 * fails closed, naming a call whose schema is no plain identifier or whose path it cannot read
 * as a literal rather than passing it over. */
function parseSites(source: ts.SourceFile): {
  readonly sites: readonly ParseSite[];
  readonly problems: readonly string[];
} {
  const sites: ParseSite[] = [];
  const problems: string[] = [];
  const visit = (node: ts.Node, insideHelper: boolean): void => {
    const declaresHelper =
      (ts.isVariableDeclaration(node) || ts.isFunctionDeclaration(node)) &&
      node.name !== undefined &&
      ts.isIdentifier(node.name) &&
      PARSE_HELPERS.has(node.name.text);
    const inside = insideHelper || declaresHelper;
    if (
      !inside &&
      ts.isCallExpression(node) &&
      ts.isIdentifier(node.expression) &&
      PARSE_HELPERS.has(node.expression.text)
    ) {
      const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
      const read = siteOfCall(node.expression.text, node.arguments, `daemon-client.ts:${line}`);
      if (typeof read === "string") problems.push(read);
      else sites.push(read);
    }
    ts.forEachChild(node, (child) => visit(child, inside));
  };
  visit(source, false);
  return { sites, problems };
}

/** One answer the client parses: how a failure names it, where it is first parsed, the routes it
 * is parsed on, and whether it parses no body at all. */
interface ClientAnswer {
  readonly label: string;
  readonly where: string;
  readonly noBody: boolean;
  readonly routes: readonly string[];
}

/** The answers the parse sites make, grouped by the schema each names.
 *
 * Two groups are not a schema's. The sites that name no schema are the `acknowledge` calls,
 * which parse nothing and accept only the empty 204: they are one answer, the table's row with
 * `noBody` true, which names the four routes that answer it. A site that names no route of its
 * own is the `parseBody` of a refusal, which `call` runs over every answer that is not a
 * success, whatever the route: it is the table's row with no route at all. */
function clientAnswers(sites: readonly ParseSite[]): ClientAnswer[] {
  const bySchema = new Map<string, { routes: Set<string>; where: string }>();
  const acknowledged = new Set<string>();
  let acknowledgedAt = "";
  for (const site of sites) {
    if (site.schema === undefined) {
      if (site.route !== undefined) acknowledged.add(site.route);
      if (acknowledgedAt === "") acknowledgedAt = site.where;
      continue;
    }
    const group = bySchema.get(site.schema) ?? { routes: new Set<string>(), where: site.where };
    if (site.route !== undefined) group.routes.add(site.route);
    bySchema.set(site.schema, group);
  }
  const answers = [...bySchema].map(([schema, group]) => ({
    label: schema,
    where: group.where,
    noBody: false,
    routes: [...group.routes].sort(),
  }));
  if (acknowledged.size > 0) {
    answers.push({
      label: "the acknowledge sites, which parse no schema",
      where: acknowledgedAt,
      noBody: true,
      routes: [...acknowledged].sort(),
    });
  }
  return answers;
}

/** What the two sides are joined on: whether the answer carries a body at all, and the routes it
 * is answered on, each route's parameter written `{}` whichever name the table gives it. */
function answerKey(noBody: boolean, routes: readonly string[]): string {
  const named = routes.map((route) => route.replace(/\{[^}]*\}/g, "{}")).sort();
  return `${noBody ? "no body" : "a body"} on ${named.length === 0 ? "no route" : named.join(", ")}`;
}

/** One row of `answers.json`. */
interface AnswerRow {
  readonly id: string;
  readonly routes: readonly string[];
  readonly noBody: boolean;
}

/** The client's own source, parsed once. The table census reads its parse sites, and the
 * read-set census takes the schemas those sites name. */
const clientSource = once(() => {
  const file = path.resolve(import.meta.dir, "daemon-client.ts");
  return ts.createSourceFile(file, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true);
});

/** The parse sites of the client's source, read once. */
const clientParseSites = once(() => parseSites(clientSource()));

test("every answer the client parses is a row of the daemon's table, and every row is parsed", () => {
  const { sites, problems } = clientParseSites();
  // Fails closed: a call this pass could not read is named here, never passed over.
  expect(problems).toEqual([]);
  // A pass that read no call at all would leave every comparison below holding vacuously.
  expect(sites.length).toBeGreaterThan(0);

  const rows: AnswerRow[] = JSON.parse(
    readFileSync(
      path.resolve(import.meta.dir, "../../contracts/fixtures/daemon-api/answers.json"),
      "utf8"
    )
  );
  const unmatched = new Map<string, AnswerRow[]>();
  for (const row of rows) {
    const key = answerKey(row.noBody, row.routes);
    unmatched.set(key, [...(unmatched.get(key) ?? []), row]);
  }

  const wrong: string[] = [];
  for (const answer of clientAnswers(sites)) {
    const key = answerKey(answer.noBody, answer.routes);
    const rowsHere = unmatched.get(key);
    if (rowsHere !== undefined && rowsHere.length > 0) {
      rowsHere.shift();
      continue;
    }
    wrong.push(
      `${answer.where} parses ${answer.label} on ${answer.routes.length === 0 ? "no route of its own" : answer.routes.join(", ")} and answers.json has no row for it`
    );
  }
  for (const rowsHere of unmatched.values()) {
    for (const row of rowsHere) {
      wrong.push(
        `answers.json names ${row.id} on ${row.routes.length === 0 ? "no route" : row.routes.join(", ")} and no parse site of the client parses it`
      );
    }
  }
  expect(wrong).toEqual([]);
});

// ─────────────────────────────────────────────────────────────────────────────
// The read-set census: the answer schemas, held to the set of fields plugin code reads.
//
// `LEGION_ANSWER_READS` (`packages/contracts/src/legion-api.ts`) is the declared read set: one
// entry per field plugin code takes the value of, by the schema's exported name and the path of
// the field within it. This census holds the schemas and that set to one another in three
// directions: a field the set does not name is optional, a name of the set is read somewhere in
// the consumer sources, and a field those sources read is named by the set.
//
// It reaches the answer schemas this client parses and no others: the roster is the schemas the
// parse sites above name, so an answer no plugin code parses — the operator and controller
// answers the Go CLI reads — is outside the census by that rule rather than by a list of names,
// and keeps every field the daemon always writes required.
//
// Both directions that read the sources match by name, and the limit of that is known: a
// property of some other object with the same name is read as a read of the field, which keeps
// the field required (the set may name it) instead of making it optional. That errs toward
// refusing a body the plugin could have read, not toward a plugin reading a field that is not
// there, which is the side this issue cares about.
// ─────────────────────────────────────────────────────────────────────────────

/** The directories whose TypeScript consumes a daemon answer, their test files left out: a test
 * reads a field to prove the schema, which is no plugin read of it. */
const CONSUMER_SOURCES = [
  "packages/pi-shared/src",
  "packages/pi-legion/src",
  "packages/pi-legion/extensions",
  "packages/claude-envoy/src",
];

const REPOSITORY_ROOT = path.resolve(import.meta.dir, "../../..");

/** Whether `value` is a zod schema this census can walk. */
function isSchema(value: unknown): value is z.ZodType {
  return (
    typeof value === "object" &&
    value !== null &&
    "def" in value &&
    "safeParse" in value &&
    typeof value.safeParse === "function"
  );
}

/** One field of one answer schema: the path from that schema's root, written as the read set
 * writes it — a record's value `{}`, a list's element `[]`, a union's option `|0` — the field's
 * own name, the name of the field it sits under (nothing at the schema's root), the object node
 * that declares it, whether the schema accepts a body without it, and whether it is the
 * discriminator of a union. The node is what tells one field reached at two paths from two
 * fields of the same name: the issue record is the state's `issues{}`, so `phase` at both paths
 * is one field, and the claim view stands at an issue's `architect` and under `workers{}.claim`
 * alike. */
interface AnswerField {
  readonly schema: string;
  readonly path: string;
  readonly name: string;
  readonly parent: string | undefined;
  readonly holder: object;
  readonly optional: boolean;
  readonly discriminator: boolean;
}

/** The node kinds the answer schemas are built from that hold no field below them. A kind
 * outside this list and the walk's own branches throws rather than taking its subtree out of
 * the census in silence, as the object census in `packages/contracts/src/legion-api.test.ts`
 * does. */
const READ_CENSUS_LEAF_KINDS: Record<string, true> = {
  boolean: true,
  enum: true,
  literal: true,
  number: true,
  string: true,
  unknown: true,
};

/** The field a path ends at, as a name alone: the last step with its `{}`, `[]` and `|0` marks
 * taken off. */
function endOfPath(path: string): string {
  const steps = path.split(".");
  return steps[steps.length - 1].replace(/(\{\}|\[\]|\|\d+)+$/, "");
}

/** The answer schemas `daemon-client.ts` parses, by the exported name each parse site names.
 * An answer outside this set is one no plugin code parses, and the census below leaves it and
 * its fields alone. */
const parsedSchemas = once(() => {
  const { sites, problems } = clientParseSites();
  // Fails closed, as the table census does: a call that pass could not read would otherwise
  // take its schema out of the census in silence.
  if (problems.length > 0) throw new Error(problems.join("; "));
  const named = new Set(sites.flatMap((site) => (site.schema === undefined ? [] : [site.schema])));
  if (named.size === 0) throw new Error("the census read no parse site of daemon-client.ts");
  return named;
});

/** Every field of every answer schema the client parses. The roster is derived from those parse
 * sites, so a schema a later change parses is walked with nobody adding a row here.
 *
 * A node two paths reach is walked at each of them, since the read set names a field at each
 * path that reaches it: the claim view is read at an issue's `architect` and sits under
 * `workers{}.claim` as well. The schemas hold no cycle — a cycle needs a `z.lazy`, which the
 * leaf-kind guard above would throw on — so the walk ends. */
const answerFields = once((): AnswerField[] => {
  const fields: AnswerField[] = [];
  const walk = (
    node: z.core.$ZodType,
    schema: string,
    here: string,
    discriminator: string | undefined
  ): void => {
    if (node instanceof z.ZodObject) {
      for (const [key, value] of Object.entries(node.def.shape)) {
        const below = here === "" ? key : `${here}.${key}`;
        fields.push({
          schema,
          path: below,
          name: key,
          parent: here === "" ? undefined : endOfPath(here),
          holder: node,
          optional: value instanceof z.ZodOptional,
          discriminator: key === discriminator,
        });
        walk(value, schema, below, undefined);
      }
      return;
    }
    if (node instanceof z.ZodArray) {
      walk(node.def.element, schema, `${here}[]`, undefined);
      return;
    }
    if (node instanceof z.ZodRecord) {
      walk(node.def.valueType, schema, `${here}{}`, undefined);
      return;
    }
    if (node instanceof z.ZodDiscriminatedUnion) {
      // The discriminator is read off the union itself, never from a name written here: a body
      // without it matches no member, so `.optional()` on it would state what the parse does
      // not do.
      for (const [index, option] of node.def.options.entries()) {
        walk(option, schema, `${here}|${index}`, node.def.discriminator);
      }
      return;
    }
    if (node instanceof z.ZodUnion) {
      for (const [index, option] of node.def.options.entries()) {
        walk(option, schema, `${here}|${index}`, undefined);
      }
      return;
    }
    if (node instanceof z.ZodOptional || node instanceof z.ZodNullable) {
      walk(node.def.innerType, schema, here, discriminator);
      return;
    }
    const kind = node._zod.def.type;
    if (READ_CENSUS_LEAF_KINDS[kind] !== true) {
      throw new Error(`the census reached a ${kind} node it does not walk, at ${schema}.${here}`);
    }
  };
  const parsed = parsedSchemas();
  for (const [name, value] of Object.entries(legionApi)) {
    if (isSchema(value) && parsed.has(name)) walk(value, name, "", undefined);
  }
  return fields;
});

/** The declared read set, one string per entry: `<schema>.<path>`. */
const declaredReads = once((): Set<string> => {
  const declared = new Set<string>();
  for (const [schema, paths] of Object.entries(legionApi.LEGION_ANSWER_READS)) {
    for (const field of paths) declared.add(`${schema}.${field}`);
  }
  return declared;
});

/** Every `.ts` file of the consumer sources, its test files left out. */
const consumerFiles = once((): string[] => {
  const found: string[] = [];
  const walk = (directory: string): void => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const full = path.join(directory, entry.name);
      if (entry.isDirectory()) walk(full);
      else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) found.push(full);
    }
  };
  for (const directory of CONSUMER_SOURCES) walk(path.join(REPOSITORY_ROOT, directory));
  return found.sort();
});

/** No local name stands for a field: what `readFrom` takes where a file's own names are not
 * read, which is how `localNames` reads the file's bindings themselves. */
const NO_LOCAL_NAMES: ReadonlyMap<string, string> = new Map();

/** What a property access reads from, where the source names it: the property below the access,
 * with an index, a parenthesis, a `!` and an `as` on the way down passed over, so
 * `state.issues[key].phase` reads `phase` of `issues`. A local name the file took from a field
 * reads from that field (`localNames`), so `check.role` of `const check = record.check` reads
 * `role` of `check`. Undefined where what the access reads from is no property of something
 * else — a name bound to a call's answer, `this`. */
function readFrom(node: ts.Expression, locals: ReadonlyMap<string, string>): string | undefined {
  let base: ts.Node = node;
  while (
    ts.isParenthesizedExpression(base) ||
    ts.isNonNullExpression(base) ||
    ts.isAsExpression(base) ||
    ts.isElementAccessExpression(base)
  ) {
    base = base.expression;
  }
  if (ts.isPropertyAccessExpression(base) && ts.isIdentifier(base.name)) return base.name.text;
  return ts.isIdentifier(base) ? locals.get(base.text) : undefined;
}

/**
 * The local names one file binds to a field of a value it read, each to that field's own name.
 * The sources read a record's check, an issue's architect and every row of the release records
 * through such a name, so without this the pair of a read would be lost at the binding.
 *
 * Three bindings take a field: a `const` of it (`const check = record.check`), a loop over a
 * list (`for (const resume of records.resumes)`) and a callback's element, which is the argument
 * before the callback (`countedBy(records.holds, (hold) => …)`) or what the method was called on
 * (`records.resumes.flatMap((resume) => …)`). A name is read one binding deep: the field a
 * binding takes is read from the source itself, never through another local name. A name the
 * file binds twice to different fields stands for neither, so a second binding of one name
 * widens no read.
 */
function localNames(source: ts.SourceFile): ReadonlyMap<string, string> {
  /** Undefined where the file binds the name to two fields of different names. */
  const bound = new Map<string, string | undefined>();
  const bind = (name: ts.BindingName | undefined, from: ts.Expression | undefined): void => {
    if (name === undefined || from === undefined || !ts.isIdentifier(name)) return;
    const field = readFrom(from, NO_LOCAL_NAMES);
    if (field === undefined) return;
    bound.set(
      name.text,
      bound.has(name.text) && bound.get(name.text) !== field ? undefined : field
    );
  };
  const visit = (node: ts.Node): void => {
    if (ts.isVariableDeclaration(node)) bind(node.name, node.initializer);
    if (ts.isForOfStatement(node) && ts.isVariableDeclarationList(node.initializer)) {
      for (const declaration of node.initializer.declarations) {
        bind(declaration.name, node.expression);
      }
    }
    if (ts.isCallExpression(node)) {
      for (const [index, argument] of node.arguments.entries()) {
        if (!ts.isArrowFunction(argument) && !ts.isFunctionExpression(argument)) continue;
        const list =
          index > 0
            ? node.arguments[index - 1]
            : ts.isPropertyAccessExpression(node.expression)
              ? node.expression.expression
              : undefined;
        bind(argument.parameters[0]?.name, list);
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(source);
  const named = new Map<string, string>();
  for (const [name, field] of bound) if (field !== undefined) named.set(name, field);
  return named;
}

/** What the consumer sources take the value of: every property name they read, and every
 * `<parent>.<field>` pair among those reads, by the first place the pair stands. A name is read
 * by a property access (`claim.state`), by an index written as a string (`row["state"]`) and by
 * an object pattern that binds it (`const { grantId } = grant`), which all take the value. A
 * pair stands where the source reads the field through a local name as well, since `readFrom`
 * takes the file's own bindings (`localNames`). */
interface PropertyReads {
  readonly names: ReadonlySet<string>;
  readonly pairs: ReadonlyMap<string, string>;
  readonly files: number;
}

const propertyReads = once((): PropertyReads => {
  const names = new Set<string>();
  const pairs = new Map<string, string>();
  const files = consumerFiles();
  for (const file of files) {
    const source = ts.createSourceFile(
      file,
      readFileSync(file, "utf8"),
      ts.ScriptTarget.Latest,
      true
    );
    const where = path.relative(REPOSITORY_ROOT, file);
    const locals = localNames(source);
    const visit = (node: ts.Node): void => {
      if (ts.isPropertyAccessExpression(node) && ts.isIdentifier(node.name)) {
        names.add(node.name.text);
        const parent = readFrom(node.expression, locals);
        const pair = parent === undefined ? undefined : `${parent}.${node.name.text}`;
        if (pair !== undefined && !pairs.has(pair)) {
          const line = source.getLineAndCharacterOfPosition(node.getStart(source)).line + 1;
          pairs.set(pair, `${where}:${line}`);
        }
      }
      if (ts.isElementAccessExpression(node) && ts.isStringLiteralLike(node.argumentExpression)) {
        names.add(node.argumentExpression.text);
      }
      if (ts.isBindingElement(node) && ts.isObjectBindingPattern(node.parent)) {
        const key = node.propertyName ?? node.name;
        if (ts.isIdentifier(key)) names.add(key.text);
      }
      ts.forEachChild(node, visit);
    };
    visit(source);
  }
  return { names, pairs, files: files.length };
});

test("every path of the declared read set names a field of its answer schema", () => {
  const fields = answerFields();
  const known = new Set(fields.map((field) => `${field.schema}.${field.path}`));
  const stale = [...declaredReads()]
    .filter((entry) => !known.has(entry))
    .map((entry) => `LEGION_ANSWER_READS names ${entry}, which is no field of an answer schema`);
  // Without this the two cases below would read a mistyped path as a field nothing requires.
  expect(stale).toEqual([]);
  expect(fields.length).toBeGreaterThan(0);
});

test("a field of an answer schema the declared read set does not name is optional", () => {
  const declared = declaredReads();
  const fields = answerFields();
  const required = fields
    .filter(
      (field) =>
        !field.optional && !field.discriminator && !declared.has(`${field.schema}.${field.path}`)
    )
    .map(
      (field) =>
        `${field.schema}.${field.path} is required and LEGION_ANSWER_READS does not name it, so a daemon that stops serving it is refused rather than read`
    );
  expect(required).toEqual([]);

  // The two fields that stay required although the read set names neither are the
  // discriminators, exempted above by what the union itself declares. Were that reading to stop
  // working the list would empty, and the case would hold nothing.
  const discriminators = fields
    .filter((field) => field.discriminator)
    .map((field) => `${field.schema}.${field.path}`);
  expect(discriminators.length).toBeGreaterThan(0);
  expect([...new Set(discriminators.map(endOfPath))].sort()).toEqual(["kind", "runtime"]);
});

/** How a consumer source names a read of one field: `<parent>.<field>` where the field sits
 * under another, and the field's own name at an answer's root, where the value holding the
 * answer carries no name of the contract's. */
function nameOfRead(field: AnswerField): string {
  return field.parent === undefined || field.parent === ""
    ? field.name
    : `${field.parent}.${field.name}`;
}

test("every name the declared read set holds is read by the consumer sources", () => {
  const reads = propertyReads();
  expect(reads.files).toBeGreaterThan(0);
  const fields = answerFields();
  const byPath = new Map(fields.map((field) => [`${field.schema}.${field.path}`, field]));
  // Every row of one field, by the node it sits in and its own name: one field two paths reach
  // stands in one list, since `LegionIssueRecordResponse` is the state's `issues{}` and the claim
  // view stands at an issue's `architect` and under `workers{}.claim` alike. The node is told
  // from another by its place in this map, which no path of a schema could do.
  const numbers = new Map<object, number>();
  const rows = new Map<string, AnswerField[]>();
  const keyOf = (field: AnswerField): string => {
    let number = numbers.get(field.holder);
    if (number === undefined) {
      number = numbers.size;
      numbers.set(field.holder, number);
    }
    return `${number}.${field.name}`;
  };
  for (const field of fields) {
    const key = keyOf(field);
    rows.set(key, [...(rows.get(key) ?? []), field]);
  }
  const taken = (field: AnswerField): boolean =>
    field.parent === undefined || field.parent === ""
      ? reads.names.has(field.name)
      : reads.pairs.has(nameOfRead(field));
  // The pair the source reads the field by, not the name alone: a bare name passes on any
  // `.state` or `.at` the consumer sources write, whatever it is a field of, so `architect.state`
  // would pass on a `.state` of something else entirely. The case below holds the other direction
  // by the same pair. A field two paths reach is read where either path's own name for it is
  // read: the issue record's `phase` is the state's `issues{}.phase`, one field of one node, and
  // this set names it at both.
  const unread = [...declaredReads()].flatMap((entry) => {
    const field = byPath.get(entry);
    // An entry that names no field at all is the first case's to report, not this one's.
    if (field === undefined) return [];
    const ways = rows.get(keyOf(field)) ?? [field];
    if (ways.some(taken)) return [];
    const tried = [...new Set(ways.map((way) => `\`${nameOfRead(way)}\``))].join(" or ");
    return [
      `LEGION_ANSWER_READS names ${entry} and no consumer source takes the value of ${tried}`,
    ];
  });
  expect(unread).toEqual([]);
});

test("a field the consumer sources read of its own parent is in the declared read set", () => {
  const reads = propertyReads();
  const declared = declaredReads();
  expect(reads.pairs.size).toBeGreaterThan(0);
  // This direction reaches the fields below an answer's root, where the source names the field
  // under the field it sits in and the pair tells it from a property of something else. A field
  // at the root is named under the value holding the answer, which carries no name of the
  // contract's, so no pair can be read for it and this direction leaves it to the case above.
  const undeclared = answerFields()
    .filter((field) => field.parent !== undefined)
    .flatMap((field) => {
      const site = reads.pairs.get(`${field.parent}.${field.name}`);
      return site === undefined || declared.has(`${field.schema}.${field.path}`)
        ? []
        : [
            `${field.schema}.${field.path} is read at ${site} and LEGION_ANSWER_READS does not name it`,
          ];
    });
  expect(undeclared).toEqual([]);
});
