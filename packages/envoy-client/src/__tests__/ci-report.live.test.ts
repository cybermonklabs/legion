import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { cpSync, existsSync, mkdirSync, readFileSync, watch, writeFileSync } from "node:fs";
import { join } from "node:path";
import { useRowGuard } from "@legion/test-fixtures/deadline";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { until } from "@legion/test-fixtures/wait";
import {
  commitSpec,
  DISPATCH_CLI,
  DISPATCH_ENTRY,
  git,
  outcomes,
  type Reported,
  runStep,
} from "../../test/ci-report-step";
import {
  type CIResultsStack,
  DISPATCH_HOST,
  type JobRun,
  type StoredResult,
  startCIResultsStack,
  useCIResultsStackDirectory,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";
import { buildGoBinaries } from "../../test/envoy-stack";
import { routeAnswer } from "../../test/route-answers";

// The fixture CI job of v-recorder: the last step of a job, packages/envoy's ci-report, run as
// the job's own process in a checkout of the job's commit, over the job's go test report, as the
// action .github/actions/dispatch-results runs it (C3): under the real `agent-secrets
// result-grant`, which trades the job's GitHub token at the real secrets broker for the proxy
// placeholder of the run's result grant. ci-report calls this checkout's real `dispatch` CLI once
// for each check whose test ran; the CLI holds that placeholder and no Dispatch credential, and
// writes the result through the broker's airlock proxy, which puts the run's results token in its
// place, to the real Dispatch server of the CI results stack (test/ci-results-stack.ts). Each row
// reads the stored rows back as a person reads them.
//
// The reports in test/fixtures/ci-report are what `go test -json -count=1 -trimpath` wrote for a
// module example.com/acme/probe of four tests, TestProbe, TestSecondProbe, TestThirdProbe and
// TestFourthProbe: go-test-all-pass.jsonl with the first two passing, go-test-first-fails.jsonl
// with TestProbe failing, go-test-second-fails.jsonl with TestSecondProbe failing, and
// go-test-four-pass.jsonl with all four passing. go-test-job-passed.jsonl is
// go-test-first-fails.jsonl with its three package events, and those alone, changed to a passing
// package ("PASS", "ok", "pass"): a job whose own outcome is success while its named test failed,
// which go test itself never writes. test/fixtures/ci-report/make-reports.sh makes each again from
// the package of test/fixtures/ci-report/cases. test/fixtures/ci-report/spec holds the two checks
// of job go-checks that name the first two tests, and spec-without-the-second-check the first
// alone, each as one spec file, probe.yaml, in the form `spec-check print` writes.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

/** The fixtures, outside src/: each job that compiles envoy-client reads src/, and none of those
 *  jobs reads these. */
const FIXTURES = join(import.meta.dir, "..", "..", "test", "fixtures", "ci-report");
/** The approved list of the rows: both checks. */
const SPEC = join(FIXTURES, "spec");
/** The files of a commit that does not hold the second check of the approved list. */
const SPEC_WITHOUT_SECOND = join(FIXTURES, "spec-without-the-second-check");
/** The files of the rows of a grant that ends between two writes: the four checks of go-checks
 *  in FOUR, of the tests TestProbe, TestSecondProbe, TestThirdProbe and TestFourthProbe, which
 *  go-test-four-pass.jsonl, what `go test -json -count=1 -trimpath ./...` wrote for them, holds
 *  passing. */
const SPEC_FOUR = join(FIXTURES, "spec-four-checks");
/** The four checks of SPEC_FOUR, in the order the step calls them: the order of their ids. */
const FOUR = ["probe-a-passes", "probe-b-passes", "probe-c-passes", "probe-d-passes"] as const;
/** The gate in front of each call of the step's CLI (test/gated-cli.ts): the first call runs at
 *  once, and each later one waits for the row's `release`. */
const GATED_CLI = join(import.meta.dir, "..", "..", "test", "gated-cli.ts");
/** The guard of each row that gates its calls: Bun's kill of the row (`test`'s third argument)
 *  and the deadline of its waits (`useRowGuard`), which ends the wait for the stack's log lines. */
const GATED_ROW_GUARD_MS = 180_000;
/** The line every refusal of either port of a broker is logged as (answer.RefusalMessage). */
const REFUSAL_LINE = "broker: answer refused";
/** What the CLI prints when the airlock's proxy refuses its CONNECT with 407: Bun gives it the
 *  tunnel's status line alone, and not the airlock's code. */
const TUNNEL_REFUSED_407 =
  "AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (407 Proxy Authentication Required); nothing was sent to Dispatch";

/** The branch every run below is a push of: two segments, which the result entry's pattern
 *  `refs/heads/*` `/*` takes. */
const REF = "refs/heads/topic/fixture-job";
/** The job the two checks name. */
const JOB = "go-checks";
const PROBE = "probe-passes";
const SECOND = "second-probe-passes";

/** The workflow file of the result entry, which Dispatch reads at a run's commit to know which
 *  job makes a check run (ci-results-stack.ts's WORKFLOW, which the stack does not export). */
const WORKFLOW_PATH = ".github/workflows/ci-results.yaml";
/** That workflow file at a commit whose job go-checks has a matrix of two legs, a and b: GitHub
 *  names each leg's check run `go-checks (<leg>)`, and Dispatch takes that name as a leg of
 *  go-checks only where the file at the commit makes go-checks a matrix job. */
const MATRIX_WORKFLOW = `name: ci-results
on: push
jobs:
  go-checks:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        leg: [a, b]
    steps:
      - run: go test ./...
`;

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped every process writing into it.
const stackDirectory = useCIResultsStackDirectory();
const work = temporaryDirectory("ci-report-live");

/** A checkout of one commit of the repository: its directory and its commit. */
interface Checkout {
  readonly dir: string;
  readonly sha: string;
}

describe.skipIf(!dispatchStackEnabled)(
  "the last step of a CI job writes one result for each check whose test ran",
  () => {
    let stack: CIResultsStack | undefined;
    const ciReport = join(work, "bin", "ci-report");
    /** The graph logic's own command, whose `state` derives the state of each check of a scenario
     *  of The Spec (specgraph.Derive): Dispatch stores results and derives no state. */
    const specCheck = join(work, "bin", "spec-check");
    const cliState = join(work, "cli-state");

    /** The running stack, or the error that says why no row can run. */
    function running(): CIResultsStack {
      if (stack === undefined) throw new Error("the CI results stack did not start");
      return stack;
    }

    /** A new checkout whose .dispatch/spec is a copy of `spec`, committed with the message
     *  `name`, and the stand-in of GitHub serving those files at that commit, with the stack's
     *  workflow file there, or `workflow` in its place. */
    function checkoutOf(name: string, spec: string, workflow?: string): Checkout {
      const dir = join(work, "checkouts", name);
      const sha = commitSpec(work, dir, spec, name);
      running().setFiles(sha, join(dir, ".dispatch", "spec"));
      if (workflow !== undefined) {
        running().github.setFile(running().repository, sha, WORKFLOW_PATH, workflow);
      }
      return { dir, sha };
    }

    /** A job of a push of REF at the checkout's commit, with a check run of its own. */
    function jobOf(
      checkout: Checkout,
      checkRunId: number,
      overrides: Partial<JobRun> = {}
    ): JobRun {
      return { sha: checkout.sha, ref: REF, job: JOB, runAttempt: 1, checkRunId, ...overrides };
    }

    /** The run of the leg `leg` of the matrix job JOB: GitHub names its check run `JOB (<leg>)`,
     *  and its step reports the checks of JOB. */
    function legOf(
      checkout: Checkout,
      checkRunId: number,
      leg: string,
      overrides: Partial<JobRun> = {}
    ): JobRun {
      return jobOf(checkout, checkRunId, { job: `${JOB} (${leg})`, ...overrides });
    }

    /** What a row may change of the job's last step: its `--job`, JOB by default, its
     *  `--dispatch-cli`, DISPATCH_CLI by default, a DISPATCH_URL of its own, and more variables. */
    interface StepOptions {
      readonly job?: string;
      readonly dispatchCli?: string;
      readonly dispatchUrl?: string;
      readonly env?: Readonly<Record<string, string>>;
    }

    /** The job's last step, as the action runs it (runStep), in the checkout, over the report
     *  `fixture`. */
    function lastStep(
      checkout: Checkout,
      run: JobRun,
      fixture: string,
      options: StepOptions = {}
    ): Promise<Reported> {
      return runStep(running(), {
        ciReport,
        dir: checkout.dir,
        run,
        report: join(FIXTURES, fixture),
        job: options.job ?? JOB,
        dispatchCli: options.dispatchCli ?? DISPATCH_CLI,
        cliState,
        ...(options.dispatchUrl === undefined ? {} : { dispatchUrl: options.dispatchUrl }),
        ...(options.env === undefined ? {} : { env: options.env }),
      });
    }

    /** Every stored row at `commit`, as a person reads them, newest first. */
    async function rowsAt(commit: string): Promise<StoredResult[]> {
      return await running().results(commit);
    }

    /** The job `run` writing a pass of `check` at its own commit itself, with the real CLI under
     *  `agent-secrets result-grant` and not through the step: the command's exit status and what
     *  it printed on both streams. */
    async function writeItself(
      run: JobRun,
      check: string
    ): Promise<{ code: number; printed: string }> {
      const written = Bun.spawn(
        running().jobCommand([
          process.execPath,
          DISPATCH_ENTRY,
          "result",
          "--project",
          running().project,
          "--check",
          check,
          "--verdict",
          "pass",
          "--commit",
          run.sha,
        ]),
        {
          env: {
            PATH: SHELL_PATH,
            HOME: cliState,
            DISPATCH_STATE_DIR: cliState,
            ...running().jobEnv(run),
          },
          stdout: "pipe",
          stderr: "pipe",
        }
      );
      const [stdout, stderr, code] = await Promise.all([
        new Response(written.stdout).text(),
        new Response(written.stderr).text(),
        written.exited,
      ]);
      return { code, printed: `${stderr}${stdout}` };
    }

    /** Each row as its check, verdict, job, leg, attempt and commit, oldest first. */
    function described(rows: readonly StoredResult[]): string[] {
      return [...rows]
        .sort((a, b) => a.seq - b.seq)
        .map(({ attestation: { subject, predicate } }) =>
          [
            predicate.check,
            predicate.verdict,
            `job ${predicate.job}`,
            `leg ${predicate.leg === "" ? "-" : predicate.leg}`,
            `attempt ${predicate.run.attempt}`,
            `at ${subject[0]?.digest.gitCommit}`,
          ].join(" ")
        );
    }

    /** Each row as its check, its check run and the member leg of its predicate as the read gives
     *  it, JSON-quoted, or "no leg" where the read leaves the member out, oldest first. A reader of
     *  the results takes a row with no leg as the leg of no matrix. */
    function legsOf(rows: readonly StoredResult[]): string[] {
      return [...rows]
        .sort((a, b) => a.seq - b.seq)
        .map(
          ({ attestation: { predicate } }) =>
            `${predicate.check} check run ${predicate.run.check_run_id}: ${
              Object.hasOwn(predicate, "leg") ? `leg ${JSON.stringify(predicate.leg)}` : "no leg"
            }`
        );
    }

    beforeAll(async () => {
      mkdirSync(cliState, { recursive: true });
      await buildGoBinaries({
        bin: join(work, "bin"),
        packages: ["./cmd/ci-report", "./cmd/spec-check"],
        stack: "fixture CI job",
        binaries: "./cmd/ci-report and ./cmd/spec-check",
      });
      stack = await startCIResultsStack({ work: stackDirectory });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("two checks name two tests of one job: the test that passed is a pass and the test that failed is a fail, at the job's commit", async () => {
      const checkout = checkoutOf("two-checks", SPEC);
      const reported = await lastStep(checkout, jobOf(checkout, 201), "go-test-second-fails.jsonl");

      expect({ code: reported.code, lines: outcomes(reported) }).toEqual({
        code: 0,
        lines: [`${PROBE} pass exit 0`, `${SECOND} fail exit 0`],
      });
      const rows = await rowsAt(checkout.sha);
      expect(described(rows)).toEqual([
        `${PROBE} pass job ${JOB} leg - attempt 1 at ${checkout.sha}`,
        `${SECOND} fail job ${JOB} leg - attempt 1 at ${checkout.sha}`,
      ]);
      expect(reported.lines.map((line) => line.result_id).sort()).toEqual(
        rows.map((row) => row.id).sort()
      );
    }, 120_000);

    test("the job passed and its named test failed: the check's result is a fail", async () => {
      const packageEnd = readFileSync(join(FIXTURES, "go-test-job-passed.jsonl"), "utf8")
        .split("\n")
        .filter((text) => text !== "")
        .map((text) => JSON.parse(text) as { Action: string; Test?: string })
        .filter((event) => event.Test === undefined && ["pass", "fail"].includes(event.Action));
      expect(packageEnd.map((event) => event.Action)).toEqual(["pass"]);

      const checkout = checkoutOf("job-passed", SPEC);
      const reported = await lastStep(checkout, jobOf(checkout, 301), "go-test-job-passed.jsonl");

      expect({ code: reported.code, lines: outcomes(reported) }).toEqual({
        code: 0,
        lines: [`${PROBE} fail exit 0`, `${SECOND} pass exit 0`],
      });
      expect(described(await rowsAt(checkout.sha))).toEqual([
        `${PROBE} fail job ${JOB} leg - attempt 1 at ${checkout.sha}`,
        `${SECOND} pass job ${JOB} leg - attempt 1 at ${checkout.sha}`,
      ]);
    }, 120_000);

    test("a check of the approved list that the files at the commit do not hold gets no call, no line and no result", async () => {
      expect(readFileSync(join(SPEC, "probe.yaml"), "utf8")).toContain(`\nid: ${SECOND}\n`);
      expect(readFileSync(join(FIXTURES, "go-test-all-pass.jsonl"), "utf8")).toContain(
        '"Action":"pass","Package":"example.com/acme/probe","Test":"TestSecondProbe"'
      );

      const checkout = checkoutOf("not-in-files", SPEC_WITHOUT_SECOND);
      const run = jobOf(checkout, 401);
      const reported = await lastStep(checkout, run, "go-test-all-pass.jsonl");

      expect({ code: reported.code, lines: outcomes(reported) }).toEqual({
        code: 0,
        lines: [`${PROBE} pass exit 0`],
      });
      // The same job writing a result for the check anyway is refused by Dispatch, which reads
      // the files at the run's commit itself.
      const written = await writeItself(run, SECOND);
      expect({ code: written.code, named: written.printed.includes("CHECK_NOT_IN_FILES") }).toEqual(
        { code: 1, named: true }
      );
      expect(described(await rowsAt(checkout.sha))).toEqual([
        `${PROBE} pass job ${JOB} leg - attempt 1 at ${checkout.sha}`,
      ]);
    }, 120_000);

    // R12: the step reads the check nodes of the files at its commit. A step before it that
    // changes a spec file and commits nothing, here one that gives the second check to another
    // job, changes no check: the step reads the commit's files through git.
    test("a spec file that an earlier step changed and did not commit: the step reports the checks of the commit", async () => {
      const checkout = checkoutOf("changed-after-commit", SPEC);
      const file = join(checkout.dir, ".dispatch", "spec", "probe.yaml");
      const committed = readFileSync(file, "utf8");
      const changed = committed.replace(
        "selector: example.com/acme/probe TestSecondProbe\n  job: go-checks\n",
        "selector: example.com/acme/probe TestSecondProbe\n  job: lint\n"
      );
      expect(changed).not.toBe(committed);
      writeFileSync(file, changed);
      expect(git(work, checkout.dir, "status", "--porcelain")).toBe(
        " M .dispatch/spec/probe.yaml\n"
      );

      const reported = await lastStep(checkout, jobOf(checkout, 851), "go-test-second-fails.jsonl");

      expect({ code: reported.code, lines: outcomes(reported) }).toEqual({
        code: 0,
        lines: [`${PROBE} pass exit 0`, `${SECOND} fail exit 0`],
      });
      expect(described(await rowsAt(checkout.sha))).toEqual([
        `${PROBE} pass job ${JOB} leg - attempt 1 at ${checkout.sha}`,
        `${SECOND} fail job ${JOB} leg - attempt 1 at ${checkout.sha}`,
      ]);
    }, 120_000);

    // The step reads the files at the commit as spec-check and Dispatch read them
    // (specgraph.Judge), so one tree has one answer in both places: the step makes no call for a
    // tree that the graph logic refuses, and Dispatch refuses a write of it, naming the same first
    // problem. The first assertion holds both answers, so a failure shows which of them moved.
    test("files at the commit that the graph logic refuses get no call and no result, and the step names the first problem that Dispatch names", async () => {
      // Parse reads each node of these files, and Validate refuses the two statements that no
      // check serves.
      const spec = join(work, "specs", "statements-without-a-check");
      cpSync(SPEC, spec, { recursive: true });
      for (const [file, id] of [
        ["lonely.yaml", "a-lonely-statement"],
        ["z-lonely.yaml", "another-lonely-statement"],
      ] as const) {
        writeFileSync(
          join(spec, file),
          `---\ntype: requirement\nid: ${id}\nkind: behavior\ntext:\n  - The probe keeps one claim.\n`
        );
      }
      const leaf = "leaf-without-check: no statement is under it and no check serves it";
      const first = `.dispatch/spec/lonely.yaml:1: a-lonely-statement: ${leaf}`;

      const checkout = checkoutOf("refused-tree", spec);
      const run = jobOf(checkout, 801);
      const reported = await lastStep(checkout, run, "go-test-all-pass.jsonl");
      const problems = reported.stderr
        .split("\n")
        .filter((line) => line.includes("/.dispatch/spec/"))
        .map((line) => line.slice(line.indexOf(".dispatch/spec/")));
      const written = await writeItself(run, PROBE);

      expect({
        step: { code: reported.code, lines: outcomes(reported), first: problems[0] },
        dispatch: {
          code: written.code,
          first: /SPEC_FILES_REFUSED: .* The first: (.*)/.exec(written.printed)?.[1],
        },
      }).toEqual({ step: { code: 2, lines: [], first }, dispatch: { code: 1, first } });
      expect(problems).toEqual([
        first,
        `.dispatch/spec/z-lonely.yaml:1: another-lonely-statement: ${leaf}`,
      ]);
      expect(reported.stderr).toContain("the tree is refused, so no result is written");
      expect(await rowsAt(checkout.sha)).toEqual([]);
    }, 120_000);

    test("a second job runs the named test and it passes there, the named job fails it: the stored rows are the named job's, and the second job's write is refused", async () => {
      const checkout = checkoutOf("second-job", SPEC);

      const named = await lastStep(checkout, jobOf(checkout, 501), "go-test-first-fails.jsonl");
      expect({ code: named.code, lines: outcomes(named) }).toEqual({
        code: 0,
        lines: [`${PROBE} fail exit 0`, `${SECOND} pass exit 0`],
      });
      // envoy-go's own last step: no check names envoy-go, so it writes nothing.
      const own = await lastStep(
        checkout,
        jobOf(checkout, 502, { job: "envoy-go" }),
        "go-test-all-pass.jsonl",
        { job: "envoy-go" }
      );
      expect({ code: own.code, lines: own.lines }).toEqual({ code: 0, lines: [] });
      // envoy-go's step set to report go-checks' checks: Dispatch reads the job from envoy-go's
      // check run and refuses each write.
      const posing = await lastStep(
        checkout,
        jobOf(checkout, 503, { job: "envoy-go" }),
        "go-test-all-pass.jsonl"
      );
      expect({
        code: posing.code,
        lines: outcomes(posing),
        refused: posing.lines.map((line) => line.why.startsWith("CHECK_NOT_THIS_JOBS")),
      }).toEqual({
        code: 1,
        lines: [`${PROBE} pass exit 1`, `${SECOND} pass exit 1`],
        refused: [true, true],
      });

      expect(described(await rowsAt(checkout.sha))).toEqual([
        `${PROBE} fail job ${JOB} leg - attempt 1 at ${checkout.sha}`,
        `${SECOND} pass job ${JOB} leg - attempt 1 at ${checkout.sha}`,
      ]);
    }, 120_000);

    test("each leg of a matrix writes its own result, and a second attempt of one leg leaves the other leg's row as it was", async () => {
      const checkout = checkoutOf("matrix", SPEC, MATRIX_WORKFLOW);
      const probeRows = async (): Promise<StoredResult[]> =>
        (await rowsAt(checkout.sha)).filter((row) => row.attestation.predicate.check === PROBE);

      const legA = await lastStep(checkout, legOf(checkout, 601, "a"), "go-test-all-pass.jsonl");
      const legB = await lastStep(checkout, legOf(checkout, 602, "b"), "go-test-first-fails.jsonl");
      expect([legA.code, legB.code]).toEqual([0, 0]);
      const before = (await probeRows()).find((row) => row.attestation.predicate.leg === "a");

      const again = await lastStep(
        checkout,
        legOf(checkout, 603, "b", { runAttempt: 2 }),
        "go-test-all-pass.jsonl"
      );
      expect(again.code).toBe(0);

      const rows = await probeRows();
      expect(described(rows)).toEqual([
        `${PROBE} pass job ${JOB} leg a attempt 1 at ${checkout.sha}`,
        `${PROBE} fail job ${JOB} leg b attempt 1 at ${checkout.sha}`,
        `${PROBE} pass job ${JOB} leg b attempt 2 at ${checkout.sha}`,
      ]);
      expect(new Set(rows.map((row) => row.attestation.predicate.run.run_id)).size).toBe(1);
      expect(before).toBeDefined();
      expect(rows.find((row) => row.attestation.predicate.leg === "a")).toEqual(
        before as StoredResult
      );
    }, 180_000);

    test("each stored result of a job with no matrix holds the member leg, and it is empty", async () => {
      const checkout = checkoutOf("no-matrix-leg", SPEC);
      const reported = await lastStep(checkout, jobOf(checkout, 611), "go-test-second-fails.jsonl");
      expect(reported.code).toBe(0);

      expect(legsOf(await rowsAt(checkout.sha))).toEqual([
        `${PROBE} check run 611: leg ""`,
        `${SECOND} check run 611: leg ""`,
      ]);
    }, 120_000);

    test("each stored result of each leg of a matrix job holds the member leg, with the values of its own leg", async () => {
      const checkout = checkoutOf("matrix-leg", SPEC, MATRIX_WORKFLOW);
      const legA = await lastStep(checkout, legOf(checkout, 612, "a"), "go-test-all-pass.jsonl");
      const legB = await lastStep(
        checkout,
        legOf(checkout, 613, "b"),
        "go-test-second-fails.jsonl"
      );
      expect([legA.code, legB.code]).toEqual([0, 0]);

      expect(legsOf(await rowsAt(checkout.sha))).toEqual([
        `${PROBE} check run 612: leg "a"`,
        `${SECOND} check run 612: leg "a"`,
        `${PROBE} check run 613: leg "b"`,
        `${SECOND} check run 613: leg "b"`,
      ]);
    }, 180_000);

    /** One commit of the scenario of `probeStates`: its checkout, and the legs of each run of JOB
     *  that ended there, [""] for a job of no matrix and [] for a commit where no run of JOB ran. */
    interface Subject {
      readonly checkout: Checkout;
      readonly ended: readonly string[];
    }

    /** The state of PROBE at each commit of `subjects`, by commit, as the graph logic derives it:
     *  `spec-check state --json` (specgraph.Derive) over a scenario of The Spec whose runs are the
     *  rows that Dispatch stored at those commits, each as Dispatch stored it. Dispatch holds
     *  nothing else that the derivation reads yet, so the rest is written here, each part so that
     *  it allows true: the approved text of the check, the text that the files at each commit
     *  hold, approved before any row; a failing example of the check that caught its fault,
     *  planted on each commit by an agent that did not write the test; a history where each commit
     *  after the first is a child of the first that changes a file of the check's tie, so that no
     *  rule of carry counts a pass of one commit at another; and the runs of JOB that ended. A row
     *  whose words the approved text does not hold throws, since no approved version holds them. */
    async function probeStates(
      name: string,
      subjects: readonly [Subject, ...Subject[]]
    ): Promise<Record<string, unknown>> {
      const dir = join(work, "scenarios", name);
      const approvedAt = "2026-01-01T00:00:00Z";
      const code = "probe/probe.go";
      const spec = readFileSync(join(SPEC_WITHOUT_SECOND, "probe.yaml"), "utf8");
      const listed = Bun.spawnSync([
        specCheck,
        "checks",
        "--dir",
        SPEC_WITHOUT_SECOND,
        "--job",
        JOB,
      ]);
      if (listed.exitCode !== 0) throw new Error(`spec-check checks: ${listed.stderr.toString()}`);
      const digest = (
        JSON.parse(listed.stdout.toString()) as { id: string; procedure_sha256: string }[]
      ).find((check) => check.id === PROBE)?.procedure_sha256;

      const rows = (await Promise.all(subjects.map(({ checkout }) => rowsAt(checkout.sha)))).flat();
      const runs = rows.map(({ seq, attestation: { subject, predicate } }) => {
        if (predicate.procedure_digest !== digest) {
          throw new Error(`row ${seq} holds words the approved text does not hold`);
        }
        return {
          check: predicate.check,
          check_version: 1,
          kind: "run",
          outcome: predicate.verdict,
          commit: subject[0]?.digest.gitCommit,
          job: predicate.job,
          leg: predicate.leg,
          attempt: predicate.run.attempt,
          at: predicate.recorded_at,
          seq,
        };
      });
      const faults = subjects.map(({ checkout }, index) => ({
        check: PROBE,
        check_version: 1,
        kind: "failing_example",
        outcome: "fail",
        commit: `fault-on-${checkout.sha}`,
        planted_on: checkout.sha,
        fault_paths: [code],
        job: JOB,
        leg: "",
        attempt: 1,
        at: "2026-01-02T00:00:00Z",
        seq: index + 1,
        fault_author: "an-agent-of-faults",
        test_authors: ["the-agent-of-the-test"],
      }));
      const first = subjects[0].checkout.sha;
      const json = (file: string, value: unknown) =>
        writeFileSync(join(dir, file), `${JSON.stringify(value, null, 2)}\n`);
      mkdirSync(dir, { recursive: true });
      json("approved.json", {
        nodes: spec.split(/^(?=---\n)/m).map((document) => ({
          id: /^id: (.*)$/m.exec(document)?.[1],
          removed: false,
          partly_checked: false,
          versions: [{ version: 1, at: approvedAt, document }],
        })),
      });
      json("history.json", {
        commits: subjects.map(({ checkout }, index) => ({
          id: checkout.sha,
          ...(index === 0 ? {} : { parent: first }),
          files: { [code]: `${index + 1}`, "probe/probe_test.go": "1" },
        })),
      });
      json(
        "ties.json",
        Object.fromEntries(subjects.map(({ checkout }) => [checkout.sha, { [PROBE]: ["probe/*"] }]))
      );
      json("results.json", [...runs, ...faults]);
      json(
        "jobs.json",
        subjects.flatMap(({ checkout, ended }) =>
          ended.map((leg) => ({ job: JOB, leg, commit: checkout.sha, ended: true }))
        )
      );
      json("uncited.json", {});
      const now = new Date().toISOString();
      json(
        "subjects.json",
        subjects.map(({ checkout }) => ({ commit: checkout.sha, branch: "topic/fixture-job", now }))
      );
      for (const { checkout } of subjects) {
        mkdirSync(join(dir, "files", checkout.sha), { recursive: true });
        cpSync(
          join(SPEC_WITHOUT_SECOND, "probe.yaml"),
          join(dir, "files", checkout.sha, "probe.yaml")
        );
      }

      const derived = Bun.spawnSync([specCheck, "state", "--scenario", dir, "--json"]);
      if (derived.exitCode !== 0) {
        throw new Error(
          `spec-check state exited ${derived.exitCode}: ${derived.stderr.toString()}`
        );
      }
      const states = JSON.parse(derived.stdout.toString()) as {
        subject: { commit: string };
        checks: { id: string }[];
      }[];
      return Object.fromEntries(
        states.map(({ subject, checks }) => [subject.commit, checks.find(({ id }) => id === PROBE)])
      );
    }

    // v-recorder: "A check with no result that counts does not read true" (The Spec's R7 and R9).
    // Dispatch stores the rows and derives no state of a check of the spec files; the graph logic
    // does. So this row reads the state from that logic over the rows that Dispatch stored
    // (probeStates), and never from this file. At A and D the step wrote a pass that counts, and
    // the check reads true: that is the control that each other input of the scenario allows true.
    // At B the job ended before its last step, and Dispatch holds no row there; at C leg b ended
    // before its last step; at E no run of the job ran. A pass of the check stands at A, at C's
    // leg a and at D, so a build that lets a pass of another commit or another leg stand in for the
    // missing one, or that reads a run of the job that ended as a pass, reads B, C or E true, and
    // fails here.
    test("a check with no result that counts reads unknown and never true: the job ended before its last step, one leg did, or no job ran; where the step wrote a pass that counts, it reads true", async () => {
      const reported = checkoutOf("silence-a-reported", SPEC_WITHOUT_SECOND);
      expect(
        outcomes(await lastStep(reported, jobOf(reported, 701), "go-test-all-pass.jsonl"))
      ).toEqual([`${PROBE} pass exit 0`]);
      const cancelled = checkoutOf("silence-b-cancelled", SPEC_WITHOUT_SECOND);
      const silentLeg = checkoutOf("silence-c-silent-leg", SPEC_WITHOUT_SECOND, MATRIX_WORKFLOW);
      expect(
        outcomes(await lastStep(silentLeg, legOf(silentLeg, 703, "a"), "go-test-all-pass.jsonl"))
      ).toEqual([`${PROBE} pass exit 0`]);
      const bothLegs = checkoutOf("silence-d-both-legs", SPEC_WITHOUT_SECOND, MATRIX_WORKFLOW);
      for (const [id, leg] of [
        [704, "a"],
        [705, "b"],
      ] as const) {
        expect(
          outcomes(await lastStep(bothLegs, legOf(bothLegs, id, leg), "go-test-all-pass.jsonl"))
        ).toEqual([`${PROBE} pass exit 0`]);
      }
      const notRun = checkoutOf("silence-e-no-run", SPEC_WITHOUT_SECOND);

      const subjects = [
        { checkout: reported, ended: [""] },
        { checkout: cancelled, ended: [""] },
        { checkout: silentLeg, ended: ["a", "b"] },
        { checkout: bothLegs, ended: ["a", "b"] },
        { checkout: notRun, ended: [] },
      ] as const;
      expect(
        await Promise.all(
          subjects.map(async ({ checkout }) => described(await rowsAt(checkout.sha)))
        )
      ).toEqual([
        [`${PROBE} pass job ${JOB} leg - attempt 1 at ${reported.sha}`],
        [],
        [`${PROBE} pass job ${JOB} leg a attempt 1 at ${silentLeg.sha}`],
        [
          `${PROBE} pass job ${JOB} leg a attempt 1 at ${bothLegs.sha}`,
          `${PROBE} pass job ${JOB} leg b attempt 1 at ${bothLegs.sha}`,
        ],
        [],
      ]);

      const unknown = (reason: string, detail = "") => ({
        id: PROBE,
        state: "unknown",
        reason,
        detail,
        from: "",
        disagree: false,
      });
      const passed = (from: string) => ({
        id: PROBE,
        state: "true",
        reason: "passed",
        detail: "",
        from,
        disagree: false,
      });
      expect(await probeStates("silence", subjects)).toEqual({
        [reported.sha]: passed(reported.sha),
        [cancelled.sha]: unknown("ci_fault"),
        [silentLeg.sha]: unknown("ci_fault", "b"),
        [bothLegs.sha]: passed(bothLegs.sha),
        [notRun.sha]: unknown("subject_moved"),
      });
    }, 240_000);

    // v-recorder: a build that records a result for another commit than the one that the job
    // tested fails. The job's token and GITHUB_SHA name the commit that the job tested; the
    // checkout then moves to a commit of its own, and the step reads that commit with git, so
    // Dispatch refuses each write, and no row is stored at either commit.
    test("a checkout whose HEAD moved after the job's commit writes no result for either commit, and each write is refused", async () => {
      const checkout = checkoutOf("moved-head", SPEC);
      const run = jobOf(checkout, 901);
      git(work, checkout.dir, "commit", "--allow-empty", "-q", "-m", "the checkout moves");
      const moved = git(work, checkout.dir, "rev-parse", "HEAD").trim();
      expect(moved).not.toBe(checkout.sha);

      const reported = await lastStep(checkout, run, "go-test-all-pass.jsonl");

      expect({
        code: reported.code,
        lines: outcomes(reported),
        refused: reported.lines.map((line) => line.why.startsWith("COMMIT_NOT_THE_RUNS:")),
      }).toEqual({
        code: 1,
        lines: [`${PROBE} pass exit 1`, `${SECOND} pass exit 1`],
        refused: [true, true],
      });
      expect({ tested: await rowsAt(checkout.sha), moved: await rowsAt(moved) }).toEqual({
        tested: [],
        moved: [],
      });
    }, 120_000);

    /** A stand-in of Dispatch on a raw socket, so its answer is exactly these bytes: every POST
     *  gets a 201 whose body is `body`, by default `{}`, which is not the route's row, and, with
     *  `echo`, the request's own `Idempotency-Key` back, as Dispatch's keyed layer sends it on each
     *  2xx of a keyed write. It reads each request up to the end of its headers before it answers,
     *  and counts the POSTs and the keys they carried. */
    function answeringDispatch(echo: boolean, body = "{}") {
      const seen = { posts: 0, keys: [] as string[] };
      const heads = new Map<object, string>();
      const server = Bun.listen({
        hostname: "127.0.0.1",
        port: 0,
        socket: {
          data(socket, chunk) {
            const head = (heads.get(socket) ?? "") + chunk.toString("latin1");
            heads.set(socket, head);
            const end = head.indexOf("\r\n\r\n");
            if (end < 0) return;
            heads.set(socket, "\0answered");
            if (!head.startsWith("POST ")) return;
            seen.posts += 1;
            const key = /^idempotency-key:[ \t]*(.*?)[ \t]*$/im.exec(head.slice(0, end))?.[1] ?? "";
            seen.keys.push(key);
            socket.write(
              "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\n" +
                (echo ? `Idempotency-Key: ${key}\r\n` : "") +
                `Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`
            );
            socket.end();
          },
          close(socket) {
            heads.delete(socket);
          },
        },
      });
      return { seen, server };
    }

    /** The step's lines as their check, outcome, exit, result id and why, its count line, and
     *  its exit status. */
    function stepAnswer(reported: Reported) {
      return {
        code: reported.code,
        lines: reported.lines.map(({ check, outcome, exit, result_id, why }) => ({
          check,
          outcome,
          exit,
          result_id,
          why,
        })),
        counted: /; (\d+ results written, \d+ calls failed)$/m.exec(reported.stderr)?.[1],
      };
    }

    // A write whose 201 carries back its own Idempotency-Key and gives no stored result - here
    // the JSON {}, which is not the route's row - landed: the echoed key says Dispatch's keyed
    // layer answered, and Dispatch answers a write's 2xx only after the write committed. The CLI
    // prints {"landed_without_answer":"ANSWER_LOST"} and exits 0. The step counts that one result
    // as written, and its line says that the answer was lost, with no id. The job's token is
    // traded at the stack's real broker by the real `agent-secrets result-grant`; only Dispatch is
    // a stand-in, which the step's own DISPATCH_URL names inside the grant (StepOptions), over
    // plain HTTP, which the grant's HTTPS_PROXY does not take.
    test("a write whose 201 echoes its key and gives no stored result counts as written, and its line says that the answer was lost", async () => {
      const checkout = checkoutOf("answer-lost", SPEC_WITHOUT_SECOND);
      const { seen, server } = answeringDispatch(true);
      try {
        const reported = await lastStep(checkout, jobOf(checkout, 951), "go-test-all-pass.jsonl", {
          dispatchUrl: `http://127.0.0.1:${server.port}`,
        });
        expect({ ...stepAnswer(reported), posts: seen.posts }).toEqual({
          code: 0,
          lines: [
            {
              check: PROBE,
              outcome: "pass",
              exit: 0,
              result_id: "",
              why: "the result was stored, and its answer was lost on the way back (ANSWER_LOST), so its id is unknown",
            },
          ],
          counted: "1 results written, 0 calls failed",
          posts: 1,
        });
        // The echo is the request's own key, which the CLI sent.
        expect(seen.keys[0]).toMatch(/^\S+$/);
      } finally {
        server.stop(true);
      }
      // The write went to the stand-in alone: the stack's Dispatch holds no row at the commit.
      expect(await rowsAt(checkout.sha)).toEqual([]);
    }, 120_000);

    // The same 201 with no key carried back is no answer of Dispatch's keyed layer: a proxy in
    // Dispatch's place can write a 2xx while the write never reached Dispatch. Nothing says the
    // result was stored, so the CLI prints {"write_outcome_unknown":"<code>"} and exits 1, and the
    // step counts no result as written, says that the outcome is unknown, and exits 1.
    test("a write whose 201 echoes no key and gives no stored result is not counted as written, and its line says that the outcome is unknown", async () => {
      const checkout = checkoutOf("no-echo", SPEC_WITHOUT_SECOND);
      const { seen, server } = answeringDispatch(false);
      try {
        const reported = await lastStep(checkout, jobOf(checkout, 952), "go-test-all-pass.jsonl", {
          dispatchUrl: `http://127.0.0.1:${server.port}`,
        });
        const answer = stepAnswer(reported);
        expect({
          code: answer.code,
          lines: answer.lines.map(({ why, ...line }) => ({
            ...line,
            unknown:
              /^the write went out and no answer said whether Dispatch stored it \([^()\s]+\): a second run of the step is safe, since Dispatch keeps each result$/.test(
                why
              ),
            why,
          })),
          counted: answer.counted,
          posted: seen.posts > 0,
        }).toEqual({
          code: 1,
          lines: [
            {
              check: PROBE,
              outcome: "pass",
              exit: 1,
              result_id: "",
              unknown: true,
              why: expect.any(String),
            },
          ],
          counted: "0 results written, 1 calls failed",
          posted: true,
        });
      } finally {
        server.stop(true);
      }
      expect(await rowsAt(checkout.sha)).toEqual([]);
    }, 120_000);

    // The same 201 with no key carried back, now holding a stored result of the job's own write:
    // its project, its check, its verdict, its job and its commit. A 201 that names no key back is
    // no answer of Dispatch's to this write, whatever it holds, since a proxy or a cache can answer
    // one write with another's row. So the CLI prints {"write_outcome_unknown":"HTTP_201"} and
    // exits 1, and the step counts no result as written, says that the outcome is unknown, and
    // exits 1.
    test("a write whose 201 echoes no key and holds a stored result of its own check is not counted as written, and its line says that the outcome is unknown", async () => {
      const checkout = checkoutOf("no-echo-row", SPEC_WITHOUT_SECOND);
      const stored = routeAnswer(
        "POST /api/v1/projects/{key}/results",
        {
          attestation: {
            subject: [
              {
                name: `git+https://github.com/${running().repository}`,
                digest: { gitCommit: checkout.sha },
              },
            ],
            predicate: {
              project: running().project,
              check: PROBE,
              verdict: "pass",
              job: JOB,
              leg: "",
            },
          },
        },
        201
      );
      const { seen, server } = answeringDispatch(false, JSON.stringify(stored));
      try {
        const reported = await lastStep(checkout, jobOf(checkout, 953), "go-test-all-pass.jsonl", {
          dispatchUrl: `http://127.0.0.1:${server.port}`,
        });
        expect({ ...stepAnswer(reported), posts: seen.posts }).toEqual({
          code: 1,
          lines: [
            {
              check: PROBE,
              outcome: "pass",
              exit: 1,
              result_id: "",
              why: "the write went out and no answer said whether Dispatch stored it (HTTP_201): a second run of the step is safe, since Dispatch keeps each result",
            },
          ],
          counted: "0 results written, 1 calls failed",
          posts: 1,
        });
      } finally {
        server.stop(true);
      }
      expect(await rowsAt(checkout.sha)).toEqual([]);
    }, 120_000);

    /** What one gated step did and left, as rows (b) and its control compare it. */
    interface Gated {
      /** The commit the step reported at. */
      readonly sha: string;
      /** The first call's exit status, read before any later call ran, and the rows stored then. */
      readonly firstExit: string;
      readonly storedFirst: string[];
      /** How many result grants the row ended between the first call and the second. */
      readonly grantsEnded: number;
      readonly code: number;
      readonly lines: string[];
      /** For each line, whether its why starts with the code of a grant that is not live. */
      readonly grantEnd: boolean[];
      /** For each line, whether its why holds the CLI's own words for the airlock's 407 at the
       *  CONNECT (dispatch-job-result.test.ts). */
      readonly airlockWords: boolean[];
      readonly counted: string | undefined;
      /** Each call's exit status as its gate wrote it, in call order, "none" for a call that
       *  wrote none. */
      readonly exits: string[];
      /** Each refusal the brokers logged after the first call. */
      readonly refusals: Record<string, unknown>[];
      /** The result writes Dispatch served after the first call. */
      readonly posts: number;
      readonly stored: string[];
    }

    /**
     * The job's last step over the four passing checks of SPEC_FOUR, with test/gated-cli.ts in
     * front of each call of the real CLI, all under the run's one result grant: the first write
     * runs at once. Once it has ended and the second call waits, the row reads what is stored,
     * takes a mark of the stack's logs, ends the run's result grant in the brokers' database when
     * `endGrant` holds, and writes `release`, which lets the second call and every later one run.
     */
    async function gatedStep(name: string, checkRunId: number, endGrant: boolean): Promise<Gated> {
      const checkout = checkoutOf(name, SPEC_FOUR);
      const gates = join(work, "gates", name);
      mkdirSync(gates, { recursive: true });
      const step = lastStep(checkout, jobOf(checkout, checkRunId), "go-test-four-pass.jsonl", {
        dispatchCli: `${process.execPath} ${GATED_CLI} ${DISPATCH_CLI}`,
        env: { GATED_CLI_DIR: gates },
      });
      // The step's end, in words, which ends a wait for a file its gate will never write.
      const stepEnded = step.then(
        (reported) => `the step ended: ${JSON.stringify(reported)}`,
        (error: unknown) => `the step failed: ${String(error)}`
      );
      // The gate's file `file` once the gate has written it, whole: the directory is watched
      // before the one check, so a file written between the two is not missed, and the wait ends
      // with the step.
      const gateFile = async (file: string): Promise<string> => {
        const path = join(gates, file);
        const { promise, resolve, reject } = Promise.withResolvers<void>();
        const watcher = watch(gates, () => {
          if (existsSync(path)) resolve();
        });
        watcher.on("error", reject);
        if (existsSync(path)) resolve();
        void stepEnded.then((why) =>
          existsSync(path) ? resolve() : reject(new Error(`the gate wrote no ${file}: ${why}`))
        );
        try {
          await promise;
        } finally {
          watcher.close();
        }
        return readFileSync(path, "utf8").trim();
      };
      // The reads between the first call and the release run in one promise whose finally writes
      // `release` and waits for the step's end: a read that throws still lets each later call run
      // and end, so ci-report, the CLI and agent-secrets end with the row, and the throw is the
      // row's failure.
      const { firstExit, storedFirst, mark, grantsEnded } = await (async () => {
        const firstExit = await gateFile("1.exit");
        await gateFile("2.waiting");
        const storedFirst = described(await rowsAt(checkout.sha));
        const mark = running().logMark();
        const grantsEnded = endGrant ? await running().endResultGrants(checkRunId) : 0;
        return { firstExit, storedFirst, mark, grantsEnded };
      })().finally(async () => {
        writeFileSync(join(gates, "release"), "");
        await stepEnded;
      });
      const reported = await step;

      // Each later call was refused at the CONNECT, which a broker logs, or written, which
      // Dispatch logs; a log line is written as its call ends, which can be after the command that
      // made the call exits, so the row waits until the logs hold a line for each of the three
      // calls, whichever each was. Both rows reach that; the row's guard ends a wait that does not.
      const resultsPath = `/api/v1/projects/${running().project}/results`;
      const since = () => {
        const lines = running().linesSince(mark);
        return {
          refusals: lines.brokers.flat().filter((record) => record.message === REFUSAL_LINE),
          posts: lines.access.filter(
            (line) => line.includes(" method=POST ") && line.includes(` path=${resultsPath} `)
          ).length,
        };
      };
      const seen = await until(
        "a broker's refusal or Dispatch's write logged for each call after the first",
        () => {
          const logged = since();
          return logged.refusals.length + logged.posts >= FOUR.length - 1 && logged;
        }
      );
      return {
        sha: checkout.sha,
        firstExit,
        storedFirst,
        grantsEnded,
        code: reported.code,
        lines: outcomes(reported),
        grantEnd: reported.lines.map((line) => line.why.startsWith("GRANT_NOT_LIVE: ")),
        airlockWords: reported.lines.map((line) => line.why.includes(TUNNEL_REFUSED_407)),
        counted: stepAnswer(reported).counted,
        exits: FOUR.map((_, i) => {
          const exit = join(gates, `${i + 1}.exit`);
          return existsSync(exit) ? readFileSync(exit, "utf8").trim() : "none";
        }),
        refusals: seen.refusals,
        posts: seen.posts,
        stored: described(await rowsAt(checkout.sha)),
      };
    }

    // Task 2.3, row (b): a write after the run's result grant ended. The step's first write is
    // stored; the row then ends the grant in the brokers' database, as its 15 minutes ending
    // would, and lets the step go on. The broker that takes each later CONNECT refuses it 407
    // GRANT_NOT_LIVE, nothing goes on to Dispatch, and ci-report prints the refusal on each of
    // those checks' lines, naming the grant's end, and exits 1.
    test(
      "a write after the run's result grant ended is refused at the proxy, and the step prints that refusal on its check's line",
      async () => {
        useRowGuard(GATED_ROW_GUARD_MS);
        const gated = await gatedStep("grant-ends", 961, true);
        const at = (check: string): string =>
          `${check} pass job ${JOB} leg - attempt 1 at ${gated.sha}`;
        expect({
          ...gated,
          refusals: gated.refusals.map(({ status, code, method, caller_class, host }) => ({
            status,
            code,
            method,
            caller_class,
            host,
          })),
        }).toEqual({
          sha: gated.sha,
          firstExit: "0",
          storedFirst: [at(FOUR[0])],
          grantsEnded: 1,
          code: 1,
          lines: [
            `${FOUR[0]} pass exit 0`,
            `${FOUR[1]} pass exit 1`,
            `${FOUR[2]} pass exit 1`,
            `${FOUR[3]} pass exit 1`,
          ],
          grantEnd: [false, true, true, true],
          airlockWords: [false, true, true, true],
          counted: "1 results written, 3 calls failed",
          exits: ["0", "1", "1", "1"],
          refusals: [1, 2, 3].map(() => ({
            status: 407,
            code: "GRANT_NOT_LIVE",
            method: "CONNECT",
            caller_class: "airlock",
            host: DISPATCH_HOST,
          })),
          posts: 0,
          stored: [at(FOUR[0])],
        });
      },
      GATED_ROW_GUARD_MS
    );

    // Row (b)'s control: the same gated step, and no grant ended. Each write is stored, no broker
    // refuses a CONNECT, and the step exits 0: what row (b) reads of the refusals and of the
    // stored rows comes from the grant's end, and not from the gate or the release.
    test(
      "the same gated step with its grant live writes each of its four results",
      async () => {
        useRowGuard(GATED_ROW_GUARD_MS);
        const gated = await gatedStep("grant-live", 962, false);
        const at = (check: string): string =>
          `${check} pass job ${JOB} leg - attempt 1 at ${gated.sha}`;
        expect(gated).toEqual({
          sha: gated.sha,
          firstExit: "0",
          storedFirst: [at(FOUR[0])],
          grantsEnded: 0,
          code: 0,
          lines: FOUR.map((check) => `${check} pass exit 0`),
          grantEnd: [false, false, false, false],
          airlockWords: [false, false, false, false],
          counted: "4 results written, 0 calls failed",
          exits: ["0", "0", "0", "0"],
          refusals: [],
          posts: 3,
          stored: FOUR.map(at),
        });
      },
      GATED_ROW_GUARD_MS
    );
  }
);
