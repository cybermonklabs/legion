// daemon-standin.ts as the rig runs it: a real process on a port of its own. Each row starts its
// own stand-in and reads what it answered and what it wrote to its log. register_plan's rows, with
// a stub Dispatch and a planted label entry, are daemon-standin-plan.test.ts's; what both files
// share is daemon-standin-harness.ts.
import { expect, setDefaultTimeout, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { existsSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  LegionErrorResponse,
  LegionIssueRecordResponse,
  LegionOwedResponse,
  LegionStateResponse,
} from "@legion/contracts/legion-api";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { skipWithout } from "@legion/test-fixtures/host-tools";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
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
  type Standin,
  withStandin,
} from "./daemon-standin-harness";

// Each row starts a Bun process, and the rows of a phase worker ask daemon-pane.go for the role's
// phase, which `go run` compiles: those get a bound of their own, as daemon-pane.test.ts's do.
setDefaultTimeout(60_000);
const GO_BOUND = 600_000;
const skipGo = skipWithout("go");

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
// than the stand-in's command line gives it, which the run's worker and owner read.
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
// works, or moves it, so a worker's `read_record` reads its phase as another while it is open, and
// `claims/owed`, which the phase reminder asks and the stand-in answers from that phase, says its
// work is done.
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

// Catches: what the claim owes answered 404, or answered from anything but the phase the record
// holds its issue in. The phase reminder (pi-shared/src/phase-reminder.ts) asks `claims/owed`
// after each turn, and the stall watch asks it for its notice's owner: unanswered, the reminder
// names no work and the notice goes to the tree's architect, so the rig's workers would be told
// what no deployment's worker is. The control is a tester whose issue the record holds in
// reviewing, a phase its role does not work, which owes nothing.
test.skipIf(skipGo)(
  "claims/owed answers that a worker owes its phase while the record holds its issue there",
  async () => {
    const owedOf = async (standin: Standin) => {
      const claim = await register(standin);
      const ask = { claimToken: claim.claimToken, sessionId: SESSION, secret: claim.secret };
      const answered = await post(standin, "/legion/v1/claims/owed", ask);
      expect(answered.status).toBe(200);
      const foreign = { ...ask, secret: fixtureSecret("other") };
      expect((await post(standin, "/legion/v1/claims/owed", foreign)).status).toBe(403);
      return LegionOwedResponse.parse(await answered.json());
    };
    await withStandin(freshDir(), [PROJECT, "LWEVAL-4", "tester", ...REPO], async (standin) => {
      expect(await owedOf(standin)).toEqual({
        owes: true,
        work: "the testing phase of LWEVAL-4",
        architect: "legion-skillsstandin-lweval-4-architect",
      });
    });
    await withStandin(
      freshDir(),
      [PROJECT, "LWEVAL-4", "tester", ...REPO, "reviewing"],
      async (standin) => {
        expect(await owedOf(standin)).toEqual({
          owes: false,
          work: "",
          architect: "legion-skillsstandin-lweval-4-architect",
        });
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
