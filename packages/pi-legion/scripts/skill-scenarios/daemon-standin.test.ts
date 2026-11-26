import { afterAll, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import {
  type AnswerEntry,
  readAnswers,
  recordedAnswer,
  type StandinOptions,
  startDaemonStandin,
} from "./daemon-standin";
import { World } from "./heldout-case";

// Every row starts the stand-in in this process, as a run starts it as a process of its own, and
// asks it what a role's `legion` tool asks: register, mint a grant, redeem it. The case worlds are
// the committed example's (testdata/heldout-case-example.json): P05, an implementer's, and P10, a
// production check's look.
const work = temporaryDirectory("daemon-standin");
let runs = 0;
const BOOT_TOKEN = fixtureSecret("boot");
const WORDS = { planning: "plan", implementing: "implement", testing: "test", reviewing: "review" };

const example = z
  .object({ cases: z.array(z.looseObject({ id: z.string(), world: z.unknown() })) })
  .parse(
    JSON.parse(
      readFileSync(path.join(import.meta.dir, "testdata", "heldout-case-example.json"), "utf8")
    )
  ).cases;
const worldOf = (id: string) => World.parse(example.find((entry) => entry.id === id)?.world);

type Body = Record<string, unknown>;
const started: { stop(): void }[] = [];
afterAll(() => {
  for (const standin of started) standin.stop();
});

/** A stand-in on a port of its own, with a log of its own, and the calls a role makes of it. */
function boot(options: Partial<StandinOptions> = {}) {
  const dir = path.join(work, `run-${++runs}`);
  mkdirSync(dir);
  const logFile = path.join(dir, "daemon.log");
  const standin = startDaemonStandin({
    port: 0,
    logFile,
    bootToken: BOOT_TOKEN,
    project: "skillsrig",
    issue: "ACME-12",
    tree: "ACME-12",
    role: "tester",
    repository: "cybermonklabs/legion",
    defaultBranch: "main",
    phase: "testing",
    handoffWords: WORDS,
    ...options,
  });
  started.push(standin);
  const call = async (method: "GET" | "POST", route: string, body?: unknown) => {
    const response = await fetch(`http://127.0.0.1:${standin.port}${route}`, {
      method,
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    const text = await response.text();
    return {
      status: response.status,
      headers: response.headers,
      body: (text === "" ? null : JSON.parse(text)) as Body | null,
    };
  };
  /** The claim registers and mints a grant, as the tool does before each write. */
  const grant = async () => {
    const registered = await call("POST", "/legion/v1/claims/register", { bootToken: BOOT_TOKEN });
    const secret = String(registered.body?.secret);
    const minted = await call(
      "POST",
      "/legion/v1/grants",
      options.role === "controller"
        ? { sessionId: "session-1", secret }
        : { sessionId: "session-1", secret, tree: options.tree ?? "ACME-12", issue: "ACME-12" }
    );
    return { grantId: String(minted.body?.grantId), secret };
  };
  const lines = () =>
    readFileSync(logFile, "utf8")
      .split("\n")
      .filter((line) => line !== "")
      .map((line) => JSON.parse(line) as Body);
  return { call, grant, lines, logFile, dir, port: standin.port };
}

const COMPLETE = "/legion/v1/handoff/complete";
const RECORD = (issue: string) => `/legion/v1/issues/${issue}/record`;

test("serves the claim's registration and grants, and logs each request with its body and answer, secrets redacted", async () => {
  const daemon = boot();
  const refused = await daemon.call("POST", "/legion/v1/claims/register", { bootToken: "wrong" });
  expect(refused).toMatchObject({ status: 403, body: { error: "Invalid boot token" } });
  const { grantId, secret } = await daemon.grant();
  const ready = await daemon.call("POST", "/legion/v1/claims/ready", {
    claimToken: "legion-skillsrig-acme-12-tester",
    sessionId: "session-1",
    secret,
    generation: 1,
  });
  expect(ready).toMatchObject({ status: 204, body: null });
  expect(
    await daemon.call("POST", "/legion/v1/claims/ready", {
      claimToken: "legion-skillsrig-acme-12-tester",
      sessionId: "session-1",
      secret: "wrong",
      generation: 1,
    })
  ).toMatchObject({ status: 403 });

  const complete = await daemon.call("POST", COMPLETE, {
    grantId,
    summary: "tested",
    verdict: "pass",
    handoffId: "h-1",
    handoffVersion: 1,
  });
  expect(complete).toMatchObject({ status: 200, body: {} });

  const log = daemon.lines();
  expect(log.map((line) => [line.method, line.path, line.status])).toEqual([
    ["POST", "/legion/v1/claims/register", 403],
    ["POST", "/legion/v1/claims/register", 200],
    ["POST", "/legion/v1/grants", 200],
    ["POST", "/legion/v1/claims/ready", 204],
    ["POST", "/legion/v1/claims/ready", 403],
    ["POST", COMPLETE, 200],
  ]);
  expect(log[0]).toMatchObject({
    body: { bootToken: "<redacted>" },
    answer: { error: "Invalid boot token" },
  });
  expect(log[1]?.answer).toMatchObject({
    claimToken: "legion-skillsrig-acme-12-tester",
    tree: "ACME-12",
    issue: "ACME-12",
    role: "tester",
    secret: "<redacted>",
  });
  expect(log[2]).toMatchObject({ sessionId: "session-1", body: { secret: "<redacted>" } });
  expect(log[3]).toMatchObject({ answer: null });
  expect(log[5]).toMatchObject({
    grantId,
    handoffId: "h-1",
    handoffVersion: 1,
    body: { summary: "tested", verdict: "pass" },
    answer: {},
  });
  for (const line of log) expect(typeof line.at).toBe("string");
  expect(readFileSync(daemon.logFile, "utf8")).not.toContain(secret);
  expect(readFileSync(daemon.logFile, "utf8")).not.toContain(BOOT_TOKEN);
});

test("refuses a request that does not parse, a grant that is not live, and a route it has none of", async () => {
  const daemon = boot();
  expect(await daemon.call("POST", COMPLETE, { summary: "no grant" })).toMatchObject({
    status: 400,
  });
  expect(
    await daemon.call("POST", COMPLETE, { grantId: "unknown", summary: "s", verdict: "pass" })
  ).toMatchObject({ status: 403, body: { error: "Invalid or expired grant" } });
  expect(await daemon.call("POST", "/legion/v1/stop", {})).toMatchObject({ status: 404 });
  expect(await daemon.call("GET", RECORD("ACME-99"))).toMatchObject({
    status: 404,
    body: { code: "RECORD_NOT_FOUND" },
  });
  expect(daemon.lines().map((line) => line.status)).toEqual([400, 403, 404, 404]);
});

// Catches: the pane's turn-boundary reports answered 404, as the first lane proof's stand-in
// answered 33 activity reports and the turn outcome of a P05 run. The bodies are the ones the
// pinned plugin sent in that run, the secret put back.
test("answers the pane's activity with 204 and a turn's outcome with its refused run, under the claim's secret", async () => {
  const daemon = boot();
  const { secret } = await daemon.grant();
  const claim = { claimToken: "legion-skillsrig-acme-12-tester", sessionId: "session-1" };
  const activity = {
    at: "2026-11-26T12:09:56.181Z",
    backgroundWork: { running: false, kinds: [] },
    phase: { nextReminderAt: null },
  };
  const outcome = { generation: 1, sequence: 1, outcome: "clean", unprompted: true };
  expect(
    await daemon.call("POST", "/legion/v1/claims/activity", { ...claim, secret, activity })
  ).toMatchObject({ status: 204, body: null });
  expect(
    await daemon.call("POST", "/legion/v1/claims/turn-outcome", { ...claim, secret, ...outcome })
  ).toMatchObject({ status: 200, body: { refusedRun: 0, failed: false } });
  expect(
    await daemon.call("POST", "/legion/v1/claims/activity", { ...claim, secret: "wrong", activity })
  ).toMatchObject({ status: 403 });
  expect(
    await daemon.call("POST", "/legion/v1/claims/turn-outcome", {
      ...claim,
      secret: "wrong",
      ...outcome,
    })
  ).toMatchObject({ status: 403 });
});

test("a completion is refused as the daemon refuses it: the record a phase ends with, the verdict, the role", async () => {
  const tester = boot();
  const { grantId } = await tester.grant();
  expect(
    await tester.call("POST", COMPLETE, { grantId, summary: "s", verdict: "pass" })
  ).toMatchObject({
    status: 400,
    body: { code: "HANDOFF_RECORD_REQUIRED" },
  });
  expect(
    await tester.call("POST", COMPLETE, {
      grantId,
      summary: "s",
      verdict: "",
      handoffId: "h",
      handoffVersion: 1,
    })
  ).toMatchObject({ status: 400, body: { code: "TESTER_VERDICT_REQUIRED" } });
  expect(
    await tester.call("POST", COMPLETE, {
      grantId,
      summary: "s",
      verdict: "pass",
      wakeAt: "2030-01-09T00:00:00Z",
      handoffId: "h",
      handoffVersion: 1,
    })
  ).toMatchObject({ status: 400, body: { code: "CHECK_WAKE_UNEXPECTED" } });

  const merger = boot({ role: "merger", phase: "merging" });
  const mergerGrant = await merger.grant();
  expect(
    await merger.call("POST", COMPLETE, {
      grantId: mergerGrant.grantId,
      summary: "s",
      verdict: "",
      commit: "abc",
      handoffId: "h",
      handoffVersion: 1,
    })
  ).toMatchObject({ status: 409, body: { code: "HANDOFF_RECORD_MISMATCH" } });
  expect(
    await merger.call("POST", COMPLETE, {
      grantId: mergerGrant.grantId,
      summary: "s",
      verdict: "pass",
      commit: "abc",
    })
  ).toMatchObject({ status: 400, body: { code: "VERDICT_ROLE_FORBIDDEN" } });
  expect(
    await merger.call("POST", COMPLETE, { grantId: mergerGrant.grantId, summary: "s", verdict: "" })
  ).toMatchObject({ status: 400, body: { code: "MISSING_FIELD" } });
  expect(
    await merger.call("POST", COMPLETE, {
      grantId: mergerGrant.grantId,
      summary: "s",
      verdict: "",
      commit: "abc",
    })
  ).toMatchObject({ status: 200, body: {} });

  const reviewer = boot({ role: "reviewer", phase: "implementing" });
  const reviewerGrant = await reviewer.grant();
  expect(
    await reviewer.call("POST", COMPLETE, {
      grantId: reviewerGrant.grantId,
      summary: "s",
      verdict: "",
    })
  ).toMatchObject({ status: 409, body: { code: "HANDOFF_NOT_CURRENT_PHASE" } });

  const architect = boot({ role: "architect", phase: "accepting" });
  const architectGrant = await architect.grant();
  expect(
    await architect.call("POST", COMPLETE, {
      grantId: architectGrant.grantId,
      summary: "s",
      verdict: "",
    })
  ).toMatchObject({ status: 409, body: { code: "ACCEPTANCE_IS_SIGN_OFF" } });
});

test("a tester's fail names a step before the one it stands at, from the world's step", async () => {
  const base = worldOf("P05");
  const world = {
    ...base,
    daemon: { phase: "testing", step: { place: 3, name: "test" }, task: {}, productionCheck: null },
  } as World;
  const daemon = boot({ world, phase: "testing" });
  const { grantId } = await daemon.grant();
  const fail = { grantId, summary: "s", verdict: "fail", handoffId: "h", handoffVersion: 1 };
  expect(await daemon.call("POST", COMPLETE, { ...fail, returnTo: 3 })).toMatchObject({
    status: 409,
    body: { code: "RETURN_REFUSED" },
  });
  expect(await daemon.call("POST", COMPLETE, { ...fail, returnTo: 2 })).toMatchObject({
    status: 200,
  });
});

// The implementer's completion of implementing needs the pull request the issue's branch is open
// on: the world's, or one a successful call in the run's calls.jsonl opened and none has closed.
test("an implementer's completion needs an open pull request on its branch: the world's or one the run opened", async () => {
  const calls = path.join(work, "calls-implementer.jsonl");
  const call = (argv: string[], exit = 0, files: Record<string, string> = {}) =>
    `${JSON.stringify({ at: "2030-01-07T15:00:00Z", as: "gh", argv, files, route: null, exit })}\n`;
  writeFileSync(calls, call(["pr", "view", "1"]));
  const daemon = boot({
    role: "implementer",
    phase: "implementing",
    world: worldOf("P05"),
    ghCalls: calls,
  });
  const { grantId } = await daemon.grant();
  const complete = {
    grantId,
    summary: "s",
    verdict: "",
    commit: "a".repeat(40),
    handoffId: "h",
    handoffVersion: 1,
  };
  expect(await daemon.call("POST", COMPLETE, complete)).toMatchObject({
    status: 409,
    body: { code: "HANDOFF_NO_PULL_REQUEST" },
  });
  const failed = call(["pr", "create", "--head", "legion/ACME-12", "--base", "main"], 1);
  writeFileSync(calls, `${readFileSync(calls, "utf8")}${failed}`);
  expect((await daemon.call("POST", COMPLETE, complete)).status).toBe(409);
  const created = call(["pr", "create", "--head", "legion/ACME-12", "--base", "main"]);
  writeFileSync(calls, `${readFileSync(calls, "utf8")}${created}`);
  expect(await daemon.call("POST", COMPLETE, complete)).toMatchObject({ status: 200, body: {} });
  writeFileSync(calls, `${readFileSync(calls, "utf8")}${call(["pr", "close", "7"])}`);
  expect((await daemon.call("POST", COMPLETE, complete)).status).toBe(409);
  const posted = call([
    "api",
    "-X",
    "POST",
    "repos/cybermonklabs/legion/pulls",
    "-f",
    "head=legion/ACME-12",
  ]);
  writeFileSync(calls, `${readFileSync(calls, "utf8")}${posted}`);
  expect((await daemon.call("POST", COMPLETE, complete)).status).toBe(200);
  // A pull request the world holds open counts without any call.
  const open = worldOf("P05");
  const holds = boot({
    role: "implementer",
    phase: "implementing",
    world: {
      ...open,
      github: {
        repository: "cybermonklabs/legion",
        mergeQueue: [],
        pullRequests: [
          {
            number: 5,
            state: "open",
            draft: false,
            head: { ref: "legion/ACME-12", sha: "b".repeat(40) },
            base: "main",
            title: "t",
            body: "",
            mergeCommit: null,
            reviews: [],
            reviewThreads: [],
            checks: [],
          },
        ],
      },
    },
  });
  const held = await holds.grant();
  expect(await holds.call("POST", COMPLETE, { ...complete, grantId: held.grantId })).toMatchObject({
    status: 200,
  });
  expect((await holds.call("GET", RECORD("ACME-12"))).body).toMatchObject({
    pullRequest: { number: 5, state: "open", head: "b".repeat(40) },
  });
});

// A production check's look is the world's: the tester works look 1 of a check that opened at
// 11:58 and whose last look starts at its bound, a day on.
test("a production check's look: the record shows the world's check, a report is held to it and is recorded once", async () => {
  const world = worldOf("P10");
  const daemon = boot({ world, phase: "done", issue: "ACME-21", tree: "ACME-21", role: "tester" });
  const record = await daemon.call("GET", RECORD("ACME-21"));
  expect(record.body).toMatchObject({
    key: "ACME-21",
    phase: "done",
    status: "done",
    check: {
      repo: "cybermonklabs/legion",
      pullRequest: 77,
      mergeCommit: "5d7f9b1d3f5a7c9e0a2c4e6b8d1f3a5c7e9b0d2f",
      openedAt: "2030-01-08T11:58:00Z",
      role: "tester",
      looks: 1,
      lookOpen: true,
      nextWakeAt: "2030-01-09T12:00:00Z",
      last: false,
    },
  });
  const state = await daemon.call("GET", "/legion/v1/state");
  expect(state.body).toMatchObject({
    admission: { active: [], trees: [] },
    issues: { "ACME-21": { phase: "done", check: { looks: 1, last: false } } },
  });

  const { grantId } = await daemon.grant();
  const report = {
    grantId,
    summary: "not in production yet",
    checkOpenedAt: "2030-01-08T11:58:00Z",
  };
  expect(await daemon.call("POST", COMPLETE, { ...report, verdict: "pending" })).toMatchObject({
    status: 400,
    body: { code: "CHECK_WAKE_REQUIRED" },
  });
  expect(
    await daemon.call("POST", COMPLETE, {
      ...report,
      verdict: "pass",
      wakeAt: "2030-01-08T13:00:00Z",
    })
  ).toMatchObject({ status: 400, body: { code: "CHECK_WAKE_UNEXPECTED" } });
  expect(await daemon.call("POST", COMPLETE, { ...report, verdict: "maybe" })).toMatchObject({
    status: 400,
    body: { code: "CHECK_VERDICT_REQUIRED" },
  });
  expect(
    await daemon.call("POST", COMPLETE, {
      ...report,
      checkOpenedAt: "2030-01-08T11:00:00Z",
      verdict: "pending",
      wakeAt: "2030-01-08T13:00:00Z",
    })
  ).toMatchObject({ status: 409, body: { code: "CHECK_NOT_OPEN" } });
  expect(
    await daemon.call("POST", COMPLETE, {
      ...report,
      verdict: "pending",
      wakeAt: "2030-01-08T13:00:00Z",
      commit: "a".repeat(40),
    })
  ).toMatchObject({ status: 400, body: { code: "CHECK_PHASE_FIELDS_UNEXPECTED" } });
  expect(
    await daemon.call("POST", COMPLETE, {
      ...report,
      verdict: "pending",
      wakeAt: "2030-01-08T13:00:00Z",
      planRating: 3,
      planReason: "r",
    })
  ).toMatchObject({ status: 400, body: { code: "PLAN_RATING_UNEXPECTED" } });
  expect(
    await daemon.call("POST", COMPLETE, { grantId, summary: "s", verdict: "pass" })
  ).toMatchObject({
    status: 409,
    body: { code: "CHECK_LOOK_NOT_SERVED" },
  });
  const pending = { ...report, verdict: "pending", wakeAt: "2030-01-08T13:00:00Z" };
  expect(await daemon.call("POST", COMPLETE, pending)).toMatchObject({ status: 200, body: {} });
  expect(await daemon.call("POST", COMPLETE, pending)).toMatchObject({
    status: 409,
    body: { code: "HANDOFF_ALREADY_RECORDED" },
  });
  // A pass closes the check: the record shows none, and a later report finds none open.
  expect(await daemon.call("POST", COMPLETE, { ...report, verdict: "pass" })).toMatchObject({
    status: 200,
  });
  expect((await daemon.call("GET", RECORD("ACME-21"))).body).not.toHaveProperty("check");
  expect(await daemon.call("POST", COMPLETE, { ...report, verdict: "fail" })).toMatchObject({
    status: 409,
    body: { code: "CHECK_NOT_OPEN" },
  });

  // Only the check's own role, the tester, reports on it.
  const reviewer = boot({
    world,
    phase: "done",
    issue: "ACME-21",
    tree: "ACME-21",
    role: "reviewer",
  });
  const other = await reviewer.grant();
  expect(
    await reviewer.call("POST", COMPLETE, { ...report, grantId: other.grantId, verdict: "pass" })
  ).toMatchObject({ status: 409, body: { code: "CHECK_ROLE_MISMATCH" } });
});

test("a look that is the check's last takes a pending with no time, and refuses one with a time", async () => {
  const world = worldOf("P10");
  const last = {
    ...world,
    daemon: { ...world.daemon, productionCheck: { ...world.daemon?.productionCheck, last: true } },
  } as World;
  const daemon = boot({
    world: last,
    phase: "done",
    issue: "ACME-21",
    tree: "ACME-21",
    role: "tester",
  });
  expect((await daemon.call("GET", RECORD("ACME-21"))).body).toMatchObject({
    check: { last: true, lastWake: { kind: "bound" } },
  });
  const { grantId } = await daemon.grant();
  const report = {
    grantId,
    summary: "still not",
    verdict: "pending",
    checkOpenedAt: "2030-01-08T11:58:00Z",
  };
  expect(
    await daemon.call("POST", COMPLETE, { ...report, wakeAt: "2030-01-08T13:00:00Z" })
  ).toMatchObject({
    status: 400,
    body: { code: "CHECK_WAKE_UNEXPECTED" },
  });
  expect(await daemon.call("POST", COMPLETE, report)).toMatchObject({ status: 200 });
});

test("serves an architect's operations, and refuses a child's design gate and a worker's move of another issue", async () => {
  const root = boot({ role: "architect", issue: "ACME-10", tree: "ACME-10", phase: "admitted" });
  const { grantId } = await root.grant();
  expect(
    await root.call("POST", "/legion/v1/gates/register", {
      grantId,
      issue: "ACME-10",
      artifactId: randomUUID(),
      version: 1,
    })
  ).toMatchObject({ status: 200, body: {} });
  expect(
    await root.call("POST", "/legion/v1/plans/register", {
      grantId,
      issue: "ACME-11",
      artifactId: randomUUID(),
      version: 2,
    })
  ).toMatchObject({ status: 200, body: { plan: { version: 2 } } });
  expect(
    await root.call("POST", "/legion/v1/signoff", { grantId, issue: "ACME-11" })
  ).toMatchObject({ status: 200, body: {} });
  expect(
    await root.call("POST", "/legion/v1/waves/release", {
      grantId,
      issues: ["ACME-11", "ACME-12", "ACME-11"],
    })
  ).toMatchObject({ status: 200, body: { released: ["ACME-11", "ACME-12"] } });
  expect(
    await root.call("POST", "/legion/v1/children/rerun", { grantId, issue: "ACME-11" })
  ).toMatchObject({ status: 200, body: {} });
  expect(
    await root.call("POST", "/legion/v1/phase/backward", {
      grantId,
      to: "planning",
      reason: "again",
      issue: "ACME-10",
    })
  ).toMatchObject({ status: 200, body: {} });
  expect((await root.call("GET", RECORD("ACME-10"))).body).toMatchObject({ phase: "planning" });
  expect(await root.call("POST", "/legion/v1/signoff", { grantId })).toMatchObject({ status: 400 });

  const child = boot({ role: "architect", issue: "ACME-12", tree: "ACME-10", phase: "admitted" });
  const childGrant = await child.grant();
  expect(
    await child.call("POST", "/legion/v1/gates/register", {
      grantId: childGrant.grantId,
      issue: "ACME-12",
      artifactId: randomUUID(),
      version: 1,
    })
  ).toMatchObject({ status: 403, body: { code: "GATE_ROOT_ONLY" } });

  const worker = boot({ role: "implementer", phase: "implementing" });
  const workerGrant = await worker.grant();
  expect(
    await worker.call("POST", "/legion/v1/phase/backward", {
      grantId: workerGrant.grantId,
      to: "planning",
      reason: "r",
      issue: "ACME-99",
    })
  ).toMatchObject({ status: 400, body: { code: "ISSUE_NOT_GRANTS" } });
  expect(
    await worker.call("POST", "/legion/v1/phase/backward", {
      grantId: workerGrant.grantId,
      to: "planning",
      reason: "r",
    })
  ).toMatchObject({ status: 200 });
  expect((await worker.call("GET", RECORD("ACME-12"))).body).toMatchObject({ phase: "planning" });
});

test("serves the controller's registration, its grants, its state and set_status, which a worker's grant is refused", async () => {
  const controller = boot({ role: "controller", phase: "implementing" });
  const registered = await controller.call("POST", "/legion/v1/claims/register", {
    bootToken: BOOT_TOKEN,
  });
  expect(registered.body).toMatchObject({
    claimToken: "legion-skillsrig-controller",
    role: "controller",
    generation: 1,
  });
  expect(registered.body).not.toHaveProperty("issue");
  const { grantId } = await controller.grant();
  expect(
    await controller.call("POST", "/legion/v1/issues/status", {
      grantId,
      issue: "ACME-12",
      status: "backlog",
    })
  ).toMatchObject({ status: 200, body: {} });
  const state = await controller.call("GET", "/legion/v1/state");
  expect(state.body).toMatchObject({
    issues: { "ACME-12": { phase: "implementing", status: "backlog" } },
  });
  expect(await controller.call("GET", "/healthz")).toMatchObject({
    status: 200,
    body: { project: "skillsrig" },
  });
  expect((await controller.call("GET", "/healthz")).headers.get("Legion-Daemon-Contract")).toMatch(
    /^\d+$/
  );
  // A controller's grant is no claim's: a completion redeeming it is refused.
  expect(
    await controller.call("POST", COMPLETE, { grantId, summary: "s", verdict: "" })
  ).toMatchObject({ status: 403, body: { code: "GRANT_INVALID" } });

  const worker = boot({ role: "implementer", phase: "implementing" });
  const workerGrant = await worker.grant();
  expect(
    await worker.call("POST", "/legion/v1/issues/status", {
      grantId: workerGrant.grantId,
      issue: "ACME-12",
      status: "todo",
    })
  ).toMatchObject({ status: 403, body: { code: "CONTROLLER_REQUIRED" } });
});

// The answers files: a request is answered by the first entry whose method, path and every match
// value equal its own, the case's entries before the shared file's, and the built-in answer
// otherwise. The request still has to parse and hold a live grant first.
test("answers from the answers files, the case's entries first, else the built-in answer", async () => {
  const entry = (overrides: Partial<AnswerEntry>): AnswerEntry => ({
    name: "entry",
    method: "POST",
    path: COMPLETE,
    match: {},
    status: 409,
    body: { code: "FROM_FILE", error: "from the file" },
    ...overrides,
  });
  const daemon = boot({
    answers: [
      entry({
        name: "case",
        match: { verdict: "pending", wakeAt: null },
        body: { code: "FROM_CASE", error: "case" },
      }),
      entry({ name: "shared", match: { verdict: "pending" } }),
      entry({ name: "quiet", path: "/legion/v1/signoff", status: 204, body: undefined }),
    ],
  });
  const { grantId } = await daemon.grant();
  const pending = { grantId, summary: "s", verdict: "pending" };
  expect(await daemon.call("POST", COMPLETE, pending)).toMatchObject({
    status: 409,
    body: { code: "FROM_CASE" },
  });
  expect(
    await daemon.call("POST", COMPLETE, { ...pending, wakeAt: "2030-01-08T13:00:00Z" })
  ).toMatchObject({ status: 409, body: { code: "FROM_FILE" } });
  // No entry matches a pass: the built-in answer's.
  expect(
    await daemon.call("POST", COMPLETE, {
      grantId,
      summary: "s",
      verdict: "pass",
      handoffId: "h",
      handoffVersion: 1,
    })
  ).toMatchObject({ status: 200, body: {} });
  expect(
    await daemon.call("POST", "/legion/v1/signoff", { grantId, issue: "ACME-11" })
  ).toMatchObject({ status: 204, body: null });
  // A grant that is not live is refused before any entry is read.
  expect(await daemon.call("POST", COMPLETE, { ...pending, grantId: "unknown" })).toMatchObject({
    status: 403,
  });

  const line = daemon.lines().find((candidate) => candidate.path === COMPLETE);
  expect(line).toMatchObject({
    method: "POST",
    status: 409,
    body: { summary: "s", verdict: "pending" },
    answer: { code: "FROM_CASE", error: "case" },
  });
});

test("a match holds at its dot path, null where the request has no value, and an entry of a member the schema does not name is refused", () => {
  const entry = (match: Record<string, unknown>): AnswerEntry => ({
    name: "entry",
    method: "POST",
    path: "/p",
    match: match as AnswerEntry["match"],
    status: 200,
  });
  const body = {
    args: { wake_at: "2030-01-08T13:00:00Z", list: [{ id: 1 }, { id: 2 }] },
    flag: null,
  };
  const answers = (match: Record<string, unknown>) =>
    recordedAnswer([entry(match)], "POST", "/p", body);
  expect(answers({ "args.wake_at": "2030-01-08T13:00:00Z" })).toBeDefined();
  expect(answers({ "args.list.1.id": 2 })).toBeDefined();
  expect(answers({ "args.list": [{ id: 1 }, { id: 2 }] })).toBeDefined();
  expect(answers({ "args.list": [{ id: 1 }] })).toBeUndefined();
  expect(answers({ "args.missing": null })).toBeDefined();
  expect(answers({ flag: null })).toBeDefined();
  expect(answers({ "args.wake_at": null })).toBeUndefined();
  expect(recordedAnswer([entry({})], "GET", "/p", body)).toBeUndefined();
  expect(recordedAnswer([entry({})], "POST", "/q", body)).toBeUndefined();

  const file = path.join(work, "answers-bad.json");
  const valid = {
    version: 1,
    captured: { command: "c", at: "2030-01-01T00:00:00Z", commit: "a".repeat(40) },
    answers: [{ name: "n", method: "POST", path: "/p", match: {}, status: 409, body: {} }],
  };
  writeFileSync(file, JSON.stringify(valid));
  expect(readAnswers(file)).toHaveLength(1);
  writeFileSync(
    file,
    JSON.stringify({
      ...valid,
      answers: [{ ...valid.answers[0], when: { github: "no pull request" } }],
    })
  );
  expect(() => readAnswers(file)).toThrow("is no daemon-answers.json");
});

// The stand-in as a run starts it: a process of its own, given the world and the answers files by
// flag. The phase and the handoff words are the daemon's own, asked of daemon-pane.go, which
// `go run` compiles.
test.skipIf(skipWithout("go"))(
  "starts as a process with a world, answers files and the run's gh calls, and prints the port it listens on",
  async () => {
    const dir = path.join(work, "process");
    mkdirSync(dir);
    const bootFile = path.join(dir, "boot");
    writeFileSync(bootFile, `${BOOT_TOKEN}\n`);
    const worldFile = path.join(dir, "world.json");
    writeFileSync(worldFile, JSON.stringify(worldOf("P10")));
    const captured = { command: "c", at: "2030-01-01T00:00:00Z", commit: "a".repeat(40) };
    const answers = (name: string, status: number) => ({
      version: 1,
      captured,
      answers: [
        {
          name,
          method: "POST",
          path: COMPLETE,
          match: { verdict: "fail" },
          status,
          body: { code: name.toUpperCase(), error: name },
        },
      ],
    });
    writeFileSync(path.join(dir, "shared.json"), JSON.stringify(answers("shared", 409)));
    writeFileSync(path.join(dir, "case.json"), JSON.stringify(answers("case", 400)));
    const logFile = path.join(dir, "daemon.log");
    const child = Bun.spawn(
      [
        process.execPath,
        path.join(import.meta.dir, "daemon-standin.ts"),
        "0",
        logFile,
        bootFile,
        "skillsrig",
        "ACME-21",
        "tester",
        "cybermonklabs/legion",
        "main",
        "--world",
        worldFile,
        "--answers",
        path.join(dir, "shared.json"),
        "--case-answers",
        path.join(dir, "case.json"),
        "--gh-calls",
        path.join(dir, "calls.jsonl"),
      ],
      { stdout: "pipe", stderr: "pipe", env: { ...process.env } }
    );
    try {
      const reader = child.stdout.getReader();
      let printed = "";
      while (!/listening on http:\/\/127\.0\.0\.1:\d+/.test(printed)) {
        const { value, done } = await reader.read();
        if (done)
          throw new Error(`the stand-in exited: ${await new Response(child.stderr).text()}`);
        printed += new TextDecoder().decode(value);
      }
      const base = /http:\/\/127\.0\.0\.1:\d+/.exec(printed)?.[0] ?? "";
      const record = (await (await fetch(`${base}${RECORD("ACME-21")}`)).json()) as Body;
      expect(record).toMatchObject({ phase: "done", check: { looks: 1, last: false } });
      const registered = (await (
        await fetch(`${base}/legion/v1/claims/register`, {
          method: "POST",
          body: JSON.stringify({ bootToken: BOOT_TOKEN }),
        })
      ).json()) as Body;
      const minted = (await (
        await fetch(`${base}/legion/v1/grants`, {
          method: "POST",
          body: JSON.stringify({
            sessionId: "s",
            secret: registered.secret,
            tree: "ACME-21",
            issue: "ACME-21",
          }),
        })
      ).json()) as Body;
      const answered = await fetch(`${base}${COMPLETE}`, {
        method: "POST",
        body: JSON.stringify({ grantId: minted.grantId, summary: "s", verdict: "fail" }),
      });
      expect(answered.status).toBe(400);
      expect(await answered.json()).toEqual({ code: "CASE", error: "case" });
    } finally {
      child.kill();
    }
  },
  600_000
);
