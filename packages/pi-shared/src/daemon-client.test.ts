import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { LegionReadyRequest } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  callReadyWithRetry,
  createLegionDaemonClient,
  DAEMON_ATTEMPT_LIMIT_MS,
  type DaemonRestartRetry,
  daemonUnreachable,
  LegionDaemonApiError,
  type LegionDaemonClient,
  LegionDaemonContractError,
  LegionDaemonNoAnswerError,
  type ReadyInput,
  retryWhileDaemonRestarts,
} from "./daemon-client";
import { daemonWaitMs, forgetDaemonWaitsForTests } from "./daemon-wait";
import { type ClaimCapability, executePhaseOp } from "./legion-phase-ops";

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
  const client = createLegionDaemonClient("http://daemon.test/", fetch);

  await expect(client.register(registration)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: registration },
  ]);
});

test("refuses a registration answer that drops a field the daemon API contract requires, without quoting its secret", async () => {
  const registered = daemonFixture("register.json");

  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(without(registered, "promptAgents"))).fetch
  );

  const refused = client.register(registration);
  await expect(refused).rejects.toBeInstanceOf(LegionDaemonContractError);
  await expect(refused).rejects.toThrow("POST /legion/v1/claims/register answered 200");
  await refused.catch((error: Error) => {
    expect(error.message).toContain("promptAgents");
    expect(error.message).not.toContain(String(registered.secret));
  });
});

test("a refusal carries its status and the daemon's sentence", async () => {
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(daemonFixture("error.json"), { status: 409 })).fetch
  );

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
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => new Response("upstream timed out", { status: 504 })).fetch
  );

  const refused = await client.register(registration).catch((error: unknown) => error);
  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({ status: 504 });
  expect((refused as LegionDaemonApiError).detail).toContain("upstream timed out");
  expect((refused as LegionDaemonApiError).detail).toContain("not the daemon's refusal");
});

test("ready and exit post the claim wire's requests and accept only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", fetch);
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

  const talkative = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({})).fetch
  );
  await expect(talkative.ready(ready)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("activity posts the claim wire's report and accepts only the empty 204", async () => {
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", fetch);
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

  const talkative = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({})).fetch
  );
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
  const client = createLegionDaemonClient("http://daemon.test", fetch);
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
  const empty = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({ releases: [], resumes: [], holds: [] })).fetch
  );
  await expect(empty.releaseRecords(window)).resolves.toEqual({
    releases: [],
    resumes: [],
    holds: [],
  });
  const partial = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json({ releases: [], resumes: [] })).fetch
  );
  await expect(partial.releaseRecords(window)).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("a ready carrying the capability report is the daemon's golden ReadyRequest, field for field", async () => {
  // ready.json is what the daemon's golden test wrote of a `claim.ReadyRequest` with a report, so
  // sending it as the client's input and recording the body it posts pins both sides to it.
  const fixture = daemonFixture("ready.json");
  const golden: ReadyInput = LegionReadyRequest.parse(fixture);
  const { fetch, requests } = daemon(() => new Response(null, { status: 204 }));
  const client = createLegionDaemonClient("http://daemon.test", fetch);

  await expect(client.ready(golden)).resolves.toBeUndefined();
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/ready", body: fixture },
  ]);
  expect(golden.capabilities?.rows).toHaveLength(3);
});

test("reads the daemon's state over a bodyless GET", async () => {
  const state = daemonFixture("state.json");
  const { fetch, requests } = daemon(() => Response.json(state));

  await expect(createLegionDaemonClient("http://daemon.test", fetch).state()).resolves.toEqual(
    state as never
  );
  expect(requests).toEqual([
    { method: "GET", url: "http://daemon.test/legion/v1/state", body: undefined },
  ]);
});

// A session runs the release before its daemon's through every roll, so the state it reads can
// carry fields this build has no schema line for. read_record is what every phase worker reads its
// own record with, so it is what proves such a response is not merely parsed but used.
test("a state from a daemon a release ahead parses without its unknown top-level and nested fields, and read_record reads the record out of it", async () => {
  const state = daemonFixture("state.json") as { issues: Record<string, unknown> };
  const record = state.issues["LEGION-208"] as Record<string, unknown>;
  const ahead = {
    ...state,
    runnerPools: [{ name: "gvisor", size: 4 }],
    issues: { ...state.issues, "LEGION-208": { ...record, reparentedAt: "2026-10-30T09:00:00Z" } },
  };
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(ahead)).fetch
  );
  const claim: ClaimCapability = {
    sessionID: "ses_implementer_208",
    tree: "LEGION-208",
    issue: "LEGION-208",
    role: "implementer",
    secret: "U2VjcmV0",
    claimToken: "legion-legion-legion-208-implementer",
    generation: 1,
  };

  await expect(client.state()).resolves.toEqual(state as never);
  await expect(
    executePhaseOp(
      {
        daemon: client,
        claim,
        handoffReport: async () => ({ commit: "a".repeat(40) }),
        branchHead: async () => {
          throw new Error("no branch-head lookup expected");
        },
      },
      { op: "read_record", issue: "LEGION-208" }
    )
  ).resolves.toEqual({ record });
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

  const read = createLegionDaemonClient("http://daemon.test", hung).state({
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
    readonly relaunch: (body: object) => Promise<unknown>;
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
    // which the legion tool found in the pane; the daemon answers an empty object.
    [
      "handoffComplete",
      "/legion/v1/handoff/complete",
      {
        grantId: "grant-208",
        summary: "done",
        verdict: "",
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
    // The relaunch names its claim by token, not by issue and role, and answers an empty object.
    [
      "relaunch",
      "/legion/v1/claims/relaunch",
      { grantId: "grant-208", claim: "legion-legion-legion-209-implementer" },
      "relaunch.json",
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
  const client = createLegionDaemonClient("http://daemon.test", fetch) as unknown as WorkflowClient;

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
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() =>
      Response.json(
        { code: "GATE_UNAPPROVED", error: "the current spec version needs approval" },
        { status: 409 }
      )
    ).fetch
  ) as unknown as {
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
  const client = createLegionDaemonClient("http://daemon.test", fetch);
  const capability = { ...registration, bootToken: "controller-capability" };

  await expect(client.registerController(capability)).resolves.toEqual(issued as never);
  expect(requests).toEqual([
    { method: "POST", url: "http://daemon.test/legion/v1/claims/register", body: capability },
  ]);
  // A claim's registration is not a controller's, nor the other way round.
  const claimAnswer = createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(daemonFixture("register.json"))).fetch
  );
  await expect(claimAnswer.registerController(capability)).rejects.toBeInstanceOf(
    LegionDaemonContractError
  );
  await expect(
    createLegionDaemonClient("http://daemon.test", fetch).register(registration)
  ).rejects.toBeInstanceOf(LegionDaemonContractError);
});

test("the registered controller mints its grant with the session form that names no tree", async () => {
  const { fetch, requests } = daemon(() => Response.json(daemonFixture("grant.json")));
  const client = createLegionDaemonClient("http://daemon.test", fetch);

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
  const client = createLegionDaemonClient(
    "http://daemon.test",
    daemon((url) => {
      if (url.pathname === "/legion/v1/claims/register")
        return Response.json({ ...registered, roleTokens: { reviewer: "t" } });
      if (url.pathname === "/legion/v1/grants") return Response.json({ ...granted, scope: "repo" });
      return Response.json({ ...completed, mergeQueuePosition: 2 });
    }).fetch
  );

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
    commit: "abc123",
    handoffId: "5b1f0c2e-9d7a-4c3b-8e61-2f4a9d0c7b13",
    handoffVersion: 2,
  });
  expect(completion).toEqual(completed as never);
  expect(completion).not.toHaveProperty("mergeQueuePosition");
});

test("a body that drops a required field, or carries one of a type the contract does not describe, is refused by the path of the field that is wrong", async () => {
  const state = daemonFixture("state.json");
  const issues = state.issues as Record<string, Record<string, unknown>>;

  const dropped = await createLegionDaemonClient(
    "http://daemon.test",
    daemon(() => Response.json(without(state, "capabilities"))).fetch
  )
    .state()
    .catch((error: unknown) => error);
  expect(dropped).toBeInstanceOf(LegionDaemonContractError);
  expect((dropped as Error).message).toContain("capabilities");

  const mistyped = await createLegionDaemonClient(
    "http://daemon.test",
    daemon(() =>
      Response.json({
        ...state,
        issues: { ...issues, "LEGION-208": { ...issues["LEGION-208"], generation: "three" } },
      })
    ).fetch
  )
    .state()
    .catch((error: unknown) => error);
  expect(mistyped).toBeInstanceOf(LegionDaemonContractError);
  expect((mistyped as Error).message).toContain("issues.LEGION-208.generation");
});

/** A daemon whose every route refuses with that status, that code, and a key no schema names. */
function refusing(status: number, code: string): LegionDaemonClient {
  return createLegionDaemonClient(
    "http://daemon.test",
    daemon(() =>
      Response.json(
        { code, error: "Dispatch refused the write", retryAfterSeconds: 30 },
        { status }
      )
    ).fetch
  );
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

test("a 502 refusal keeps its stable code through a key no schema names, and is the daemon's own answer, which is handed back rather than waited out", async () => {
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

/** One `LegionDaemonApiError` as a route of the daemon's would raise it. */
function api(status: number, code?: string): LegionDaemonApiError {
  return new LegionDaemonApiError(
    "POST",
    "/legion/v1/handoff/complete",
    status,
    code,
    "the gateway could not reach the daemon"
  );
}

/** One of the ends Bun's fetch reports by a code on an `Error`. */
function network(code: string): Error {
  return Object.assign(new Error(`connect ${code} 10.0.0.1:8437`), { code });
}

/** A fake clock for the wait: `now` moves only when the retry sleeps, every sleep is recorded,
 * and no attempt of the case that uses it hangs, so the attempt's own limit never fires. */
function waitClock(): {
  readonly options: DaemonRestartRetry;
  readonly waits: number[];
  readonly elapsed: () => number;
} {
  let clock = 0;
  const waits: number[] = [];
  return {
    options: {
      now: () => clock,
      sleep: async (ms: number) => {
        waits.push(ms);
        clock += ms;
      },
      attemptLimit: () => new Promise<void>(() => {}),
    },
    waits,
    elapsed: () => clock,
  };
}

// The w1 error table, read as the one question the wait asks of an answer: is the daemon
// serving? A refused, reset or closed connection, a host that does not resolve or cannot be
// reached, an attempt with no answer, and an uncoded 502, 503 or 504 all say it is not. Every
// other answer is the daemon's own and is the caller's to act on, the two 500s among them.
test("the answers that say the daemon is not serving are the w1 table's, and every other answer is the caller's", () => {
  for (const code of ["ConnectionRefused", "ECONNREFUSED"]) {
    expect(daemonUnreachable(network(code)), code).toBe("refused");
  }
  for (const code of ["ENOTFOUND", "EAI_AGAIN", "EHOSTUNREACH", "ENETUNREACH"]) {
    expect(daemonUnreachable(network(code)), code).toBe("refused");
  }
  for (const code of [
    "ConnectionClosed",
    "ECONNRESET",
    "EPIPE",
    "ETIMEDOUT",
    "ConnectionTimeout",
  ]) {
    expect(daemonUnreachable(network(code)), code).toBe("dropped");
  }
  for (const message of ["fetch failed", "Failed to fetch"]) {
    expect(daemonUnreachable(new TypeError(message)), message).toBe("dropped");
  }
  for (const status of [502, 503, 504]) {
    expect(daemonUnreachable(api(status)), `${status}`).toBe("dropped");
  }
  expect(daemonUnreachable(new LegionDaemonNoAnswerError("handoff_complete", 300_000))).toBe(
    "dropped"
  );

  expect(daemonUnreachable(api(500))).toBeUndefined();
  for (const code of ["DISPATCH_UNAVAILABLE", "FACTS_UNAVAILABLE"]) {
    expect(daemonUnreachable(api(500, code)), code).toBeUndefined();
  }
  for (const status of [502, 503, 504]) {
    expect(daemonUnreachable(api(status, "DISPATCH_FAILED")), `${status}`).toBeUndefined();
  }
  expect(daemonUnreachable(api(409, "HANDOFF_ALREADY_RECORDED"))).toBeUndefined();
  expect(daemonUnreachable(api(403))).toBeUndefined();
});

test("a call that finds the daemon not serving is made again once it serves, for every answer of that table", async () => {
  for (const failure of [
    network("ECONNREFUSED"),
    network("ECONNRESET"),
    network("ConnectionClosed"),
    network("ENOTFOUND"),
    network("EAI_AGAIN"),
    network("EHOSTUNREACH"),
    network("ENETUNREACH"),
    new TypeError("fetch failed"),
    api(502),
    api(503),
    api(504),
  ]) {
    const clock = waitClock();
    let attempts = 0;
    const told: string[] = [];
    const answered = await retryWhileDaemonRestarts(
      "handoff_complete",
      async () => {
        attempts += 1;
        if (attempts === 1) throw failure;
        return "recorded";
      },
      { ...clock.options, onRetrying: (message) => told.push(message) }
    );

    expect(answered, messageFor(failure)).toBe("recorded");
    expect(attempts, messageFor(failure)).toBe(2);
    expect(clock.waits, messageFor(failure)).toEqual([500]);
    expect(told, messageFor(failure)).toHaveLength(1);
    expect(told[0], messageFor(failure)).toContain("for up to 60 minutes");
  }
});

test("a call waits up to 60 minutes for a daemon that is not serving, and past that ends as today", async () => {
  const clock = waitClock();
  let attempts = 0;

  const ended = await retryWhileDaemonRestarts(
    "handoff_complete",
    async () => {
      attempts += 1;
      throw network("ECONNREFUSED");
    },
    clock.options
  ).catch((error: unknown) => error);

  expect(clock.elapsed()).toBe(60 * 60_000);
  expect((ended as Error).message).toContain("stayed so for 60 minutes");
  expect((ended as Error).message).toContain("call it again once the daemon is back");
  // 500 ms doubling to 30 s and then 30 s each: an hour is well over a hundred attempts, where
  // the ten minutes this replaces was twenty-six.
  expect(attempts).toBeGreaterThan(120);
});

test("an attempt the daemon takes and does not answer in five minutes is ended and made again", async () => {
  const limits: number[] = [];
  const signals: (AbortSignal | undefined)[] = [];
  const waits: number[] = [];
  let clock = 0;
  let attempts = 0;

  const answered = await retryWhileDaemonRestarts(
    "handoff_complete",
    async (attempt) => {
      attempts += 1;
      signals.push(attempt);
      if (attempts === 1) return new Promise<string>(() => {});
      return "recorded";
    },
    {
      now: () => clock,
      sleep: async (ms: number) => {
        waits.push(ms);
        clock += ms;
      },
      // The first attempt's limit runs out; the second answers, so its limit never does.
      attemptLimit: (ms: number) => {
        limits.push(ms);
        return attempts === 1 ? Promise.resolve() : new Promise<void>(() => {});
      },
    }
  );

  expect(answered).toBe("recorded");
  expect(limits).toEqual([DAEMON_ATTEMPT_LIMIT_MS, DAEMON_ATTEMPT_LIMIT_MS]);
  expect(waits).toEqual([500]);
  // The attempt the client gave up on is ended before the next one is sent, so no second
  // request of the same call is ever in flight beside it.
  expect(signals[0]?.aborted).toBe(true);
  expect(signals[1]?.aborted).toBe(false);
});

test("a repeat after an attempt with no answer is told the earlier attempt may have been applied", async () => {
  const clock = waitClock();
  let attempts = 0;

  const refused = await retryWhileDaemonRestarts(
    "handoff_complete",
    async () => {
      attempts += 1;
      if (attempts === 1) return new Promise<string>(() => {});
      throw api(409, "HANDOFF_ALREADY_RECORDED");
    },
    {
      ...clock.options,
      attemptLimit: () => (attempts === 1 ? Promise.resolve() : new Promise<void>(() => {})),
    }
  ).catch((error: unknown) => error);

  expect(refused).toBeInstanceOf(LegionDaemonApiError);
  expect(refused).toMatchObject({ status: 409, code: "HANDOFF_ALREADY_RECORDED" });
  expect((refused as Error).message).toContain(
    "an earlier attempt of handoff_complete lost its connection before the daemon answered, and may have been applied: read the record before calling again"
  );
});

test("an uncoded 500 and a coded one are the caller's at once, with no wait", async () => {
  for (const failure of [
    api(500),
    api(500, "DISPATCH_UNAVAILABLE"),
    api(500, "FACTS_UNAVAILABLE"),
  ]) {
    const clock = waitClock();
    let attempts = 0;
    const refused = await retryWhileDaemonRestarts(
      "handoff_complete",
      async () => {
        attempts += 1;
        throw failure;
      },
      clock.options
    ).catch((error: unknown) => error);

    expect(refused, messageFor(failure)).toBe(failure);
    expect(attempts, messageFor(failure)).toBe(1);
    expect(clock.waits, messageFor(failure)).toEqual([]);
  }
});

// `claims/ready` keeps the three tries it has always had of a 5xx the daemon itself answered,
// and hands an answer that says the daemon is not serving to the wait around it, which is where
// the sixty minutes are. Without that, the three tries would spend the outage in three seconds.
test("claims/ready tries another 5xx three times and leaves an answer of the not-serving table to the wait", async () => {
  const waits: number[] = [];
  const options: DaemonRestartRetry = { sleep: async (ms: number) => void waits.push(ms) };

  let fives = 0;
  const failed = await callReadyWithRetry(
    "claims/ready",
    async () => {
      fives += 1;
      throw api(500);
    },
    options
  ).catch((error: unknown) => error);
  expect(failed).toBeInstanceOf(LegionDaemonApiError);
  expect(fives).toBe(3);
  expect(waits).toEqual([1_000, 1_000]);

  for (const failure of [api(503), network("ECONNREFUSED"), new TypeError("fetch failed")]) {
    let attempts = 0;
    const handed = await callReadyWithRetry(
      "claims/ready",
      async () => {
        attempts += 1;
        throw failure;
      },
      options
    ).catch((error: unknown) => error);
    expect(handed, messageFor(failure)).toBe(failure);
    expect(attempts, messageFor(failure)).toBe(1);
  }
});

// The worker's own stall watch judges a call that has been open with no step, and a call
// waiting out a daemon restart is exactly that shape. So the wait is recorded under the tool
// call that waits, and the watch takes it off that call's clock alone
// (`src/stall-watchdog.ts`, callOverdue).
test("the time a tool call spends waiting for a daemon that is not serving is recorded under that call", async () => {
  forgetDaemonWaitsForTests();
  let clock = 1_000;
  let attempts = 0;
  const flaky = async () => {
    attempts += 1;
    if (attempts % 4 !== 0) throw network("ECONNREFUSED");
    return "recorded";
  };
  const clockOptions = {
    now: () => clock,
    sleep: async (ms: number) => {
      clock += ms;
    },
    attemptLimit: () => new Promise<void>(() => {}),
  };

  await retryWhileDaemonRestarts("handoff_complete", flaky, {
    ...clockOptions,
    toolCallId: "toolu_legion",
  });

  // 500 + 1,000 + 2,000 ms of waiting, from the call's first request to the answer that came,
  // and none of it any other call's.
  expect(clock).toBe(4_500);
  expect(daemonWaitMs("toolu_legion", 0, clock)).toBe(3_500);
  expect(daemonWaitMs("toolu_bash", 0, clock)).toBe(0);

  // A wait no tool call owns — the boot's, a role regain's `claims/ready`, a turn's outcome —
  // pauses nothing, so it records nothing.
  forgetDaemonWaitsForTests();
  const from = clock;
  await retryWhileDaemonRestarts("claims/ready after role regain", flaky, clockOptions);
  expect(clock).toBe(from + 3_500);
  expect(daemonWaitMs("claims/ready after role regain", from, clock)).toBe(0);

  // The wait begins at the call's first request, not at its first failure: a daemon that takes
  // the connection and answers four minutes later, with no failure at all, charged the call's
  // stall clock none of those four minutes.
  forgetDaemonWaitsForTests();
  const asked = clock;
  await retryWhileDaemonRestarts(
    "handoff_complete",
    async () => {
      clock += 4 * 60_000;
      return "recorded";
    },
    {
      now: () => clock,
      attemptLimit: () => Promise.withResolvers<void>().promise,
      toolCallId: "toolu_legion",
    }
  );
  expect(daemonWaitMs("toolu_legion", asked, clock)).toBe(4 * 60_000);
});

// The agent's abort ends the call wherever it is. A call aborted before its first attempt sends
// nothing at all; one aborted while an attempt waits on the daemon ends that attempt, whose
// signal is aborted, at once rather than at the attempt's limit, and says that attempt may have
// been applied.
test("an aborted call sends nothing more, and an abort during an attempt ends that attempt at once", async () => {
  const before = new AbortController();
  before.abort();
  let sent = 0;
  const refused = await retryWhileDaemonRestarts(
    "sign_off",
    async () => {
      sent += 1;
      return "signed off";
    },
    { signal: before.signal, attemptLimit: () => Promise.withResolvers<void>().promise }
  ).catch((error: unknown) => error);
  expect(sent).toBe(0);
  expect((refused as Error).message).toBe(
    "sign_off: the call was aborted before it was sent; nothing was sent"
  );

  const during = new AbortController();
  const signals: AbortSignal[] = [];
  const pending = retryWhileDaemonRestarts(
    "sign_off",
    async (attempt) => {
      signals.push(attempt);
      during.abort();
      return Promise.withResolvers<string>().promise;
    },
    { signal: during.signal, attemptLimit: () => Promise.withResolvers<void>().promise }
  ).catch((error: unknown) => error);
  expect(((await pending) as Error).message).toBe(
    "sign_off: the call was aborted while an attempt waited for the daemon's answer; that attempt may have been applied, so read the record before calling again"
  );
  expect(signals).toHaveLength(1);
  expect(signals[0]?.aborted).toBe(true);
});
