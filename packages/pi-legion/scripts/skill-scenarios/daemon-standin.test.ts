import { afterAll, expect, setDefaultTimeout, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { roleForPhase } from "@legion/contracts";
import {
  LegionErrorResponse,
  LegionIssueRecordResponse,
  LegionStateResponse,
} from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import {
  temporaryDirectories,
  temporaryDirectory,
} from "@legion/test-fixtures/temporary-directories";
import { z } from "zod";
import {
  type AnswerEntry,
  readAnswers,
  recordedAnswer,
  type StandinOptions,
  startDaemonStandin,
} from "./daemon-standin";
import {
  DEFAULT_BRANCH,
  grant,
  lineOf,
  logLines,
  OWN_CONTRACT,
  PROJECT,
  plantLabel,
  post,
  REPO,
  REPOSITORY,
  refused,
  register,
  SESSION,
  withStandin,
} from "./daemon-standin-harness";
import { World } from "./heldout-case";

// The rows that run daemon-pane.go, which `go run` compiles, skip on a host without go.
const skipGo = skipWithout("go");

// Each row of a case's stand-in, up to the rows of a process below, starts it in this process, as
// a run starts it as a process of its own, and asks it what a role's `legion` tool asks: register,
// mint a grant, redeem it. The case worlds are the committed example's
// (testdata/heldout-case-example.json): P05, an implementer's, and P10, a production check's look.
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
// on: the world's, or one gh-standin recorded opening on the branch (a line's `pull`, which it
// writes only for a call it took) and recorded no close or merge of since. What a call did is read
// from that record alone, never from its arguments: a POST gh makes for its fields alone opens one,
// and a line that kept nothing opens none, whatever its arguments say.
test("an implementer's completion needs an open pull request on its branch: the world's or one the run opened", async () => {
  const calls = path.join(work, "calls-implementer.jsonl");
  const call = (argv: string[], exit: number, pull?: Record<string, unknown>) =>
    `${JSON.stringify({ at: "2030-01-07T15:00:00Z", as: "gh", argv, files: {}, route: null, exit, pull })}\n`;
  const append = (line: string) => writeFileSync(calls, `${readFileSync(calls, "utf8")}${line}`);
  const opened = (number: number, head = "legion/ACME-12") => ({
    action: "open",
    number,
    head,
    base: "main",
    sha: "a".repeat(40),
    title: "t",
    body: "",
    draft: false,
  });
  writeFileSync(calls, call(["pr", "view", "1"], 0));
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
  const status = async () => (await daemon.call("POST", COMPLETE, complete)).status;
  expect(await daemon.call("POST", COMPLETE, complete)).toMatchObject({
    status: 409,
    body: { code: "HANDOFF_NO_PULL_REQUEST" },
  });
  // A creation gh-standin refused, and one it answered from a route and kept nothing of.
  append(call(["pr", "create", "--head", "legion/ACME-12", "--base", "main"], 1));
  append(call(["pr", "create", "--head", "legion/ACME-12", "--base", "main"], 0));
  expect(await status()).toBe(409);
  // Another branch's.
  append(call(["pr", "create", "--head", "legion/ACME-13"], 0, opened(1, "legion/ACME-13")));
  expect(await status()).toBe(409);
  append(
    call(["api", "repos/cybermonklabs/legion/pulls", "-f", "head=legion/ACME-12"], 0, opened(2))
  );
  expect(await daemon.call("POST", COMPLETE, complete)).toMatchObject({ status: 200, body: {} });
  // A close of another pull request leaves it open; its own close or merge closes it, a reopen
  // opens it again, and a merge that waits for the requirements (`auto`) leaves it open.
  append(call(["pr", "close", "1"], 0, { action: "close", number: 1 }));
  expect(await status()).toBe(200);
  append(call(["pr", "close", "2"], 0, { action: "close", number: 2 }));
  expect(await status()).toBe(409);
  append(call(["pr", "reopen", "2"], 0, { action: "reopen", number: 2 }));
  expect(await status()).toBe(200);
  append(call(["pr", "merge", "2", "--auto"], 0, { action: "merge", number: 2, auto: true }));
  expect(await status()).toBe(200);
  append(
    call(["api", "-X", "PUT", "repos/cybermonklabs/legion/pulls/2/merge"], 0, {
      action: "merge",
      number: 2,
      auto: false,
    })
  );
  expect(await status()).toBe(409);
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

// The last look's pending carries no time and closes the check as not run (workflow's
// endLookPending): the record shows no check, and each report after it finds none open. A
// recorded answer captured on a look that was not the last does not hold here.
test("a look that is the check's last takes a pending with no time, which closes the check, and refuses one with a time", async () => {
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
    answers: readAnswers(path.join(import.meta.dir, "testdata", "daemon-answers.json")),
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
  expect(await daemon.call("POST", COMPLETE, report)).toEqual({
    status: 200,
    headers: expect.anything(),
    body: {},
  });
  expect((await daemon.call("GET", RECORD("ACME-21"))).body).not.toHaveProperty("check");
  expect((await daemon.call("GET", "/legion/v1/state")).body).not.toHaveProperty(
    "issues.ACME-21.check"
  );
  for (const verdict of ["pending", "pass", "fail"])
    expect(await daemon.call("POST", COMPLETE, { ...report, verdict })).toMatchObject({
      status: 409,
      body: { code: "CHECK_NOT_OPEN" },
    });
});

// A pending on a look before the last ends that look and names the next: the record shows the look
// closed and the next look due at the time the report named, moved as the daemon's clampWake moves
// it (no sooner than a minute after the case's now; the bound where it falls at or past the bound).
test("a pending before the last look ends the look, and the next look is due when the report says, clamped as the daemon clamps it", async () => {
  const lookAfter = async (wakeAt: string) => {
    const daemon = boot({
      world: worldOf("P10"),
      phase: "done",
      issue: "ACME-21",
      tree: "ACME-21",
      role: "tester",
    });
    const { grantId } = await daemon.grant();
    const pending = {
      grantId,
      summary: "not in production yet",
      verdict: "pending",
      checkOpenedAt: "2030-01-08T11:58:00Z",
      wakeAt,
    };
    expect(await daemon.call("POST", COMPLETE, pending)).toMatchObject({ status: 200, body: {} });
    const record = await daemon.call("GET", RECORD("ACME-21"));
    expect(record.body).toMatchObject({ check: { looks: 1, lookOpen: false, last: false } });
    const check = (record.body as { check?: { nextWakeAt?: unknown } } | null)?.check;
    return String(check?.nextWakeAt);
  };
  expect(await lookAfter("2030-01-08T12:30:00.123456789Z")).toBe("2030-01-08T12:30:00.123456Z");
  expect(await lookAfter("2030-01-09T12:00:00Z")).toBe("2030-01-09T12:00:00Z");
  expect(await lookAfter("2030-01-10T00:00:00+02:00")).toBe("2030-01-09T12:00:00Z");
  // The P10 world's clock is 12:00:00, so the floor is 12:01:00 and a few seconds of the test.
  const floored = Date.parse(await lookAfter("2030-01-08T12:00:30Z"));
  expect(floored).toBeGreaterThanOrEqual(Date.parse("2030-01-08T12:01:00Z"));
  expect(floored).toBeLessThan(Date.parse("2030-01-08T12:02:00Z"));
});

// The two production-check answers of testdata/daemon-answers.json (F3) are the real daemon's to
// the P10 look before any report: the stand-in, given no answers file, answers the same requests
// with the same status and body. Given that file, it still answers each look report from the check
// it holds, so a report the file holds as accepted is recorded once, and its repeat refused.
test("a look report is answered from the check the stand-in holds: as the daemon answered F3, and once", async () => {
  const captured = readAnswers(path.join(import.meta.dir, "testdata", "daemon-answers.json"));
  const looks = captured.filter((entry) => "checkOpenedAt" in entry.match);
  expect(looks.map((entry) => entry.name)).toEqual(["check-wake-required", "pending-accepted"]);
  const run = (answers: AnswerEntry[]) =>
    boot({ world: worldOf("P10"), phase: "done", issue: "ACME-21", tree: "ACME-21", answers });
  const bare = run([]);
  const { grantId } = await bare.grant();
  for (const entry of looks) {
    const request = Object.fromEntries(
      Object.entries(entry.match).filter(([, value]) => value !== null)
    );
    expect(
      await bare.call("POST", COMPLETE, { grantId, summary: "not yet", ...request })
    ).toMatchObject({ status: entry.status, body: entry.body ?? {} });
  }
  const filed = run(captured);
  const filedGrant = await filed.grant();
  const accepted = looks.find((entry) => entry.status === 200)?.match ?? {};
  const report = { grantId: filedGrant.grantId, summary: "not yet", ...accepted };
  expect(await filed.call("POST", COMPLETE, report)).toMatchObject({ status: 200 });
  expect((await filed.call("GET", RECORD("ACME-21"))).body).toMatchObject({
    check: { lookOpen: false, nextWakeAt: "2030-01-08T12:30:00Z" },
  });
  expect(await filed.call("POST", COMPLETE, report)).toMatchObject({
    status: 409,
    body: { code: "HANDOFF_ALREADY_RECORDED" },
  });
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
test.skipIf(skipGo)(
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

// daemon-standin.ts as the rig runs it: a real process on a port of its own. Each row starts its
// own stand-in and reads what it answered and what it wrote to its log. register_plan's rows, with
// a stub Dispatch and a planted label entry, are daemon-standin-plan.test.ts's; what both files
// share is daemon-standin-harness.ts.

// Each row starts a Bun process, and the rows of a phase worker ask daemon-pane.go for the role's
// phase, which `go run` compiles: those get a bound of their own, as daemon-pane.test.ts's do.
setDefaultTimeout(60_000);
const GO_BOUND = 600_000;

const freshDir = temporaryDirectories("daemon-standin");

// Catches: a sign-off or a move back answered 404, logged without the body the scorer reads
// (score.ts's acceptance clause names W from it), or a body that does not parse answered 200 or
// left out of the log.
test("a sign_off and a move back are logged with their bodies", async () => {
  const dir = freshDir();
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "accepting"],
    async (standin) => {
      const claim = await register(standin);
      const grantId = await grant(standin, claim);
      const signOff = { grantId, issue: "LWEVAL-7" };
      const signed = await post(standin, "/legion/v1/signoff", signOff);
      expect(signed.status).toBe(200);
      expect(await signed.json()).toEqual({});
      const backward = {
        grantId,
        to: "implementing",
        reason: `planted ${randomUUID()}`,
        issue: "LWEVAL-7",
      };
      const moved = await post(standin, "/legion/v1/phase/backward", backward);
      expect(moved.status).toBe(200);
      expect(await moved.json()).toEqual({});
      expect(lineOf(standin, "/legion/v1/signoff")).toMatchObject({
        status: 200,
        grantId,
        body: signOff,
      });
      expect(lineOf(standin, "/legion/v1/phase/backward")).toMatchObject({
        status: 200,
        grantId,
        body: backward,
      });

      // A body the schema refuses: a field LegionSignOffRequest does not name.
      const odd = await post(standin, "/legion/v1/signoff", { ...signOff, extra: "x" });
      expect(odd.status).toBe(400);
      expect(LegionErrorResponse.parse(await odd.json()).error).toContain("LegionSignOffRequest");
      const signOffLines = logLines(standin).filter((line) => line.path === "/legion/v1/signoff");
      expect(signOffLines.map((line) => line.status)).toEqual([200, 400]);
    }
  );
});

// Catches: an activity report answered 404 (the rig's end of a run waits on it), its log line
// without the report's backgroundWork and phase.nextReminderAt, or a line that keeps the claim
// token, the session id or the secret in its body.
test("an activity report is answered and logged with its backgroundWork", async () => {
  const dir = freshDir();
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted"],
    async (standin) => {
      const claim = await register(standin);
      const activity = {
        at: new Date().toISOString(),
        backgroundWork: { running: true, kinds: ["async-job"] },
        phase: { nextReminderAt: "2026-11-24T12:05:00Z" },
      };
      const report = {
        claimToken: claim.claimToken,
        sessionId: SESSION,
        secret: claim.secret,
        activity,
      };
      const answered = await post(standin, "/legion/v1/claims/activity", report);
      expect(answered.status).toBe(204);
      expect(await answered.text()).toBe("");

      const line = lineOf(standin, "/legion/v1/claims/activity");
      expect(line.status).toBe(204);
      expect(line.body).toEqual(activity);
      expect(JSON.stringify(line.body)).not.toContain(claim.secret);
      expect(JSON.stringify(line.body)).not.toContain(claim.claimToken);
      expect(JSON.stringify(line.body)).not.toContain(SESSION);

      // Another session's secret is refused, and a report without its activity does not parse.
      const foreign = await post(standin, "/legion/v1/claims/activity", {
        ...report,
        secret: fixtureSecret("other"),
      });
      expect(foreign.status).toBe(403);
      const { activity: _, ...bare } = report;
      const empty = await post(standin, "/legion/v1/claims/activity", bare);
      expect(empty.status).toBe(400);
      expect(LegionErrorResponse.parse(await empty.json()).error).toContain(
        "LegionActivityRequest"
      );
    }
  );
});

// Catches: a header that states the rig's own plugin's contract when the run's label is another
// checkout, which is the contract the label's plugin speaks and `read_build` reads.
test("the health header states the label's contract, not the rig's", async () => {
  const dir = freshDir();
  const contract = OWN_CONTRACT + 7;
  const { label } = plantLabel(dir, contract);
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", "--label", label],
    async (standin) => {
      const health = await fetch(`${standin.url}/healthz`);
      expect(health.status).toBe(200);
      expect(health.headers.get("Legion-Daemon-Contract")).toBe(String(contract));
      expect(health.headers.get("Legion-Daemon-Contract")).not.toBe(String(OWN_CONTRACT));
      expect(await health.json()).toEqual({ status: "ok", project: PROJECT });
    }
  );
});

// Catches: a header that reads a label's contract from its manifest alone, which a checkout's
// manifest leaves out from the daemon API contract cutover on, where the label's plugin speaks the
// frozen list's greatest number plus its checkout's count of entry fixtures.
test("the health header states the contract a label's checkout counts when its manifest states none", async () => {
  const dir = freshDir();
  const last = OWN_CONTRACT + 20;
  const { label } = plantLabel(dir, { released: [last - 3, last], entries: 4 });
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted", "--label", label],
    async (standin) => {
      const health = await fetch(`${standin.url}/healthz`);
      expect(health.status).toBe(200);
      expect(health.headers.get("Legion-Daemon-Contract")).toBe(String(last + 4));
    }
  );
});

// Catches: a state that holds the claim's issue alone, gives an issue its own key as its tree when
// the world names another, records an issue the world holds in no phase, which a daemon never
// records, or puts an issue on another repository or default branch than the command line names.
test("the state lists the world's issues with their tree", async () => {
  const dir = freshDir();
  const issuesFile = path.join(dir, "issues.json");
  writeFileSync(
    issuesFile,
    JSON.stringify([
      { key: "LWEVAL-1", tree: "LWEVAL-1", phase: "implementing", status: "in_progress" },
      { key: "LWEVAL-2", tree: "LWEVAL-1", phase: "implementing", status: "in_progress" },
      { key: "LWEVAL-3", tree: "LWEVAL-1", phase: null, status: "backlog" },
    ])
  );
  await withStandin(
    dir,
    [
      PROJECT,
      "LWEVAL-2",
      "architect",
      ...REPO,
      "implementing",
      "--tree",
      "LWEVAL-1",
      "--issues",
      issuesFile,
    ],
    async (standin) => {
      const claim = await register(standin);
      expect(claim.tree).toBe("LWEVAL-1");
      const answered = await fetch(`${standin.url}/legion/v1/state`);
      expect(answered.status).toBe(200);
      const state = LegionStateResponse.parse(await answered.json());
      const issues = state.issues ?? {};
      expect(Object.keys(issues).sort()).toEqual(["LWEVAL-1", "LWEVAL-2"]);
      for (const key of ["LWEVAL-1", "LWEVAL-2"]) {
        expect(issues[key]).toMatchObject({
          key,
          tree: "LWEVAL-1",
          phase: "implementing",
          repo: REPOSITORY,
          defaultBranch: DEFAULT_BRANCH,
        });
      }
      expect(state.admission?.trees).toEqual(["LWEVAL-1"]);
      const unrecorded = await fetch(`${standin.url}/legion/v1/issues/LWEVAL-3/record`);
      expect(unrecorded.status).toBe(404);
    }
  );
});

// Catches: an --issues file that would make the claim's own issue stand in another phase or tree
// than the stand-in's command line gives it, which the run's reminder check and owner read.
test("an --issues file that gives the claim's issue another phase or tree is refused, naming both", () => {
  const dir = freshDir();
  const issuesFile = path.join(dir, "issues.json");
  writeFileSync(
    issuesFile,
    JSON.stringify([
      { key: "LWEVAL-7", tree: "LWEVAL-7", phase: "implementing", status: "in_progress" },
    ])
  );
  const phase = refused(dir, [
    PROJECT,
    "LWEVAL-7",
    "architect",
    ...REPO,
    "accepting",
    "--issues",
    issuesFile,
  ]);
  expect(phase.code).toBe(2);
  expect(phase.stderr).toContain("implementing");
  expect(phase.stderr).toContain("accepting");
  const tree = refused(dir, [
    PROJECT,
    "LWEVAL-7",
    "architect",
    ...REPO,
    "implementing",
    "--tree",
    "LWEVAL-1",
    "--issues",
    issuesFile,
  ]);
  expect(tree.code).toBe(2);
  expect(tree.stderr).toContain("LWEVAL-1");
  expect(tree.stderr).toContain("LWEVAL-7");
});

// Catches: tasks/stop, release_children (its route is POST /legion/v1/waves/release) or
// gates/register answered 404, answered with something other than {}, or logged without its body,
// a refused one included.
test("tasks/stop, release_children's waves/release and gates/register answer {} with a 200 and log the body", async () => {
  const dir = freshDir();
  await withStandin(
    dir,
    [PROJECT, "LWEVAL-7", "architect", ...REPO, "admitted"],
    async (standin) => {
      const claim = await register(standin);
      const grantId = await grant(standin, claim);
      const bodies = {
        "/legion/v1/tasks/stop": { grantId, issue: "LWEVAL-8", task: "rig-task" },
        "/legion/v1/waves/release": { grantId, issues: ["LWEVAL-8"] },
        "/legion/v1/gates/register": {
          grantId,
          issue: "LWEVAL-7",
          artifactId: randomUUID(),
          version: 2,
          step: "spec",
        },
      };
      for (const [route, body] of Object.entries(bodies)) {
        const response = await post(standin, route, body);
        expect(response.status).toBe(200);
        expect(await response.json()).toEqual({});
        expect(lineOf(standin, route)).toMatchObject({ status: 200, grantId, body });
      }

      // A grant the stand-in never minted is refused, and its line still keeps the body.
      const unknown = { grantId: randomUUID(), issue: "LWEVAL-8", task: "rig-task" };
      expect((await post(standin, "/legion/v1/tasks/stop", unknown)).status).toBe(403);
      expect(
        logLines(standin)
          .filter((line) => line.path === "/legion/v1/tasks/stop")
          .at(-1)
      ).toMatchObject({ status: 403, body: unknown });
    }
  );
});

// Catches: a record route that answers the claim's issue in another phase than the one its role
// works, or moves it, so the phase reminder's record check (pi-shared/src/phase-reminder.ts) drops
// each reminder a deployment's worker would get while its phase is open.
test.skipIf(skipGo)(
  "issues/<key>/record keeps the worker's phase while it is open",
  async () => {
    const dir = freshDir();
    const issuesFile = path.join(dir, "issues.json");
    writeFileSync(
      issuesFile,
      JSON.stringify([
        { key: "LWEVAL-4", tree: "LWEVAL-4", phase: "testing", status: "in_progress" },
        { key: "LWEVAL-9", tree: "LWEVAL-9", phase: "implementing", status: "in_progress" },
      ])
    );
    await withStandin(
      dir,
      [PROJECT, "LWEVAL-4", "tester", ...REPO, "--issues", issuesFile],
      async (standin) => {
        const phaseNow = async (): Promise<string> => {
          const answered = await fetch(`${standin.url}/legion/v1/issues/LWEVAL-4/record`);
          expect(answered.status).toBe(200);
          return LegionIssueRecordResponse.parse(await answered.json()).phase;
        };
        expect(await phaseNow()).toBe("testing");
        expect(roleForPhase(await phaseNow())).toBe("tester");

        const claim = await register(standin);
        const report = {
          claimToken: claim.claimToken,
          sessionId: SESSION,
          secret: claim.secret,
          activity: {
            at: new Date().toISOString(),
            backgroundWork: { running: false, kinds: [] },
            phase: { nextReminderAt: "2026-11-24T12:05:00Z" },
          },
        };
        expect((await post(standin, "/legion/v1/claims/activity", report)).status).toBe(204);
        // A completion that names no test record is refused, so the phase stays open.
        const completion = await post(standin, "/legion/v1/handoff/complete", {
          grantId: await grant(standin, claim),
          summary: "the tests pass",
          verdict: "pass",
        });
        expect(completion.status).toBe(400);
        expect(LegionErrorResponse.parse(await completion.json()).code).toBe(
          "HANDOFF_RECORD_REQUIRED"
        );
        expect(await phaseNow()).toBe("testing");
      }
    );
  },
  GO_BOUND
);

// Catches: a change that breaks rig.sh's tester call (run_tester_proof: eight arguments, the
// world's repository and default branch last, and no option), which serves a tester's one issue in
// its own tree, on that repository and branch, with the rig's own contract; and a call that leaves
// out the repository and branch served, where the stand-in must refuse it.
test.skipIf(skipGo)(
  "rig.sh's eight-argument tester call serves a tester's claim on its repository, with the rig's own contract",
  async () => {
    const dir = freshDir();
    const short = refused(freshDir(), [PROJECT, "LWEVAL-5", "tester"]);
    expect(short.code).toBe(2);
    expect(short.stderr).toContain("<repository> <default branch>");
    await withStandin(dir, [PROJECT, "LWEVAL-5", "tester", ...REPO], async (standin) => {
      const claim = await register(standin);
      expect(claim).toMatchObject({ tree: "LWEVAL-5", issue: "LWEVAL-5" });
      const state = LegionStateResponse.parse(
        await (await fetch(`${standin.url}/legion/v1/state`)).json()
      );
      expect(Object.keys(state.issues ?? {})).toEqual(["LWEVAL-5"]);
      expect(state.issues?.["LWEVAL-5"]).toMatchObject({
        tree: "LWEVAL-5",
        phase: "testing",
        repo: REPOSITORY,
        defaultBranch: DEFAULT_BRANCH,
      });
      const health = await fetch(`${standin.url}/healthz`);
      expect(health.headers.get("Legion-Daemon-Contract")).toBe(String(OWN_CONTRACT));
      expect(existsSync(standin.log)).toBe(true);
    });
  },
  GO_BOUND
);
