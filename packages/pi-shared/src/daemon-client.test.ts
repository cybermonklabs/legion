import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LegionReadyRequest } from "@legion/contracts/legion-api";
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
  const client = createLegionDaemonClient("http://daemon.test/", { contract: CONTRACT, fetch });

  await expect(client.register(registration)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: registration },
  ]);
});

test("refuses a registration answer that drops a field the daemon API contract requires, without quoting its secret", async () => {
  const registered = daemonFixture("register.json");
  const client = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
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
    contract: CONTRACT,
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
    contract: CONTRACT,
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
  const client = createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch });
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
    contract: CONTRACT,
    fetch: daemon(() => Response.json({})).fetch,
  });
  await expect(talkative.ready(ready)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("activity posts the claim wire's report and accepts only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch });
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
    contract: CONTRACT,
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
  const client = createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch });
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
    contract: CONTRACT,
    fetch: daemon(() => Response.json({ releases: [], resumes: [], holds: [] })).fetch,
  });
  await expect(empty.releaseRecords(window)).resolves.toEqual({
    releases: [],
    resumes: [],
    holds: [],
  });
  const partial = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
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
  const client = createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch });

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
    createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch }).state()
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
    contract: CONTRACT,
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
    contract: CONTRACT,
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
    contract: CONTRACT,
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
  const client = createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch });
  const capability = { ...registration, bootToken: "controller-capability" };

  await expect(client.registerController(capability)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: capability },
  ]);
  // A claim's registration is not a controller's, nor the other way round.
  const claimAnswer = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
    fetch: daemon(() => Response.json(daemonFixture("register.json"))).fetch,
  });
  await expect(claimAnswer.registerController(capability)).rejects.toBeInstanceOf(
    LegionDaemonContractError
  );
  await expect(
    createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch }).register(
      registration
    )
  ).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("the registered controller mints its grant with the session form that names no tree", async () => {
  const { fetch, requests } = daemon(() => Response.json(daemonFixture("grant.json")));
  const client = createLegionDaemonClient("http://daemon.test", { contract: CONTRACT, fetch });

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
    contract: CONTRACT,
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
    contract: CONTRACT,
    fetch: daemon(() => Response.json(without(state, "issues"))).fetch,
  })
    .state()
    .catch((error: unknown) => error);
  expect(dropped).toBeInstanceOf(LegionDaemonContractError);
  expect((dropped as Error).message).toContain("issues");

  const mistyped = await createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
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
    contract: CONTRACT,
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
    contract: CONTRACT,
    fetch: stating(PARED_STATE, CONTRACT),
  });

  await expect(client.state()).resolves.toEqual(PARED_STATE as never);

  // The per-issue record and the health body, pared the same way.
  const record = { phase: "done", check: { role: "tester", looks: 2, last: false } };
  const reading = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
    fetch: stating(record, CONTRACT),
  });
  await expect(reading.record("LEGION-208")).resolves.toEqual(record as never);

  const healthy = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
    fetch: stating({ project: "legion" }, CONTRACT),
  });
  await expect(healthy.health()).resolves.toEqual({ project: "legion" });
});

test("an answer whose field carries a type the contract does not describe is refused", async () => {
  const client = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
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
    contract: CONTRACT,
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
    contract: CONTRACT,
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
    contract: CONTRACT,
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
    contract: CONTRACT,
    fetch: stating(
      { code: "RECORD_NOT_FOUND", error: "The daemon has no record for LEGION-404" },
      CONTRACT,
      404
    ),
  });
  await expect(absent.record("LEGION-404")).resolves.toBeUndefined();

  // A 404 of another code is a read that failed, not an absent record.
  const otherCode = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
    fetch: stating({ code: "ISSUE_NOT_FOUND", error: "no such issue" }, CONTRACT, 404),
  });
  await expect(otherCode.record("LEGION-404")).rejects.toBeInstanceOf(LegionDaemonApiError);

  const refused = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
    fetch: stating({ error: "the daemon is restarting" }, CONTRACT, 503),
  });
  await expect(refused.record("LEGION-208")).rejects.toBeInstanceOf(LegionDaemonApiError);

  const unreadable = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
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
    contract: CONTRACT,
    fetch: unreadable,
  });

  await expect(client.daemonContract()).resolves.toBe(61);
  expect(requests).toEqual(["/healthz"]);
  // The same answer through `health`, which does parse, is refused: the two differ by the body.
  await expect(client.health()).rejects.toBeInstanceOf(LegionDaemonContractError);

  const silent = createLegionDaemonClient("http://daemon.test", {
    contract: CONTRACT,
    fetch: stating({ project: "legion" }, undefined),
  });
  await expect(silent.daemonContract()).resolves.toBeUndefined();
});
