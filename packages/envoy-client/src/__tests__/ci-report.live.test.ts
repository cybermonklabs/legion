import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { cpSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  type CIResultsStack,
  type JobRun,
  startCIResultsStack,
  useCIResultsStackDirectory,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";
import { buildGoBinaries } from "../../test/envoy-stack";
import { routeAnswer } from "../../test/route-answers";

// The fixture CI job of v-recorder: the last step of a job, packages/envoy's ci-report, run as
// the job's own process in a checkout of the job's commit, over the job's go test report. It
// calls this checkout's real `dispatch` CLI once for each check whose test ran, and the CLI writes
// the result through the real secrets broker to the real Dispatch server of the CI results stack
// (test/ci-results-stack.ts). Each row reads the stored rows back as a person reads them.
//
// The reports in test/fixtures/ci-report are what `go test -json -count=1 -trimpath ./...` wrote
// for a module example.com/acme/probe of two tests, TestProbe and TestSecondProbe:
// go-test-all-pass.jsonl with both passing, go-test-first-fails.jsonl with TestProbe failing,
// go-test-second-fails.jsonl with TestSecondProbe failing. go-test-job-passed.jsonl is
// go-test-first-fails.jsonl with its three package events, and those alone, changed to a passing
// package ("PASS", "ok", "pass"): a job whose own outcome is success while its named test failed,
// which go test itself never writes. test/fixtures/ci-report/spec holds the two checks of job
// go-checks that name those tests, and spec-without-the-second-check the first alone.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

/** The CLI entry ci-report runs, as a CI job's shell reaches it: this checkout's own `dispatch`. */
const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
/** The fixtures, outside src/: each job that compiles envoy-client reads src/, and none of those
 *  jobs reads these. */
const FIXTURES = join(import.meta.dir, "..", "..", "test", "fixtures", "ci-report");
/** The approved list of the rows: both checks. */
const SPEC = join(FIXTURES, "spec");
/** The files of a commit that does not hold the second check of the approved list. */
const SPEC_WITHOUT_SECOND = join(FIXTURES, "spec-without-the-second-check");

/** The branch every run below is a push of: two segments, which the result entry's pattern
 *  `refs/heads/*` `/*` takes. */
const REF = "refs/heads/topic/fixture-job";
/** The job the two checks name. */
const JOB = "go-checks";
const PROBE = "probe-passes";
const SECOND = "second-probe-passes";

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped every process writing into it.
const stackDirectory = useCIResultsStackDirectory();
const work = temporaryDirectory("ci-report-live");

/** One JSON line that ci-report prints. */
interface ReportLine {
  readonly check: string;
  readonly test: string;
  readonly outcome: string;
  readonly exit: number | null;
  readonly result_id: string;
  readonly why: string;
}

/** What one run of ci-report did. */
interface Reported {
  readonly code: number;
  readonly lines: ReportLine[];
  readonly stderr: string;
}

/** A row of `GET /api/v1/projects/{key}/results`, as these rows read it. */
interface Row {
  readonly id: string;
  readonly seq: number;
  readonly attestation: {
    readonly subject: { readonly digest: { readonly gitCommit: string } }[];
    readonly predicate: {
      readonly check: string;
      readonly verdict: string;
      readonly job: string;
      readonly leg: string;
      readonly run: {
        readonly run_id: string;
        readonly attempt: number;
        readonly check_run_id: string;
      };
    };
  };
}

/** A checkout of one commit of the repository: its directory and its commit. */
interface Checkout {
  readonly dir: string;
  readonly sha: string;
}

/** Runs git with `args` in `cwd`, with no configuration of this machine's, and answers stdout. */
function git(cwd: string, ...args: string[]): string {
  const ran = Bun.spawnSync(
    [
      "git",
      "-c",
      "user.name=ACME CI",
      "-c",
      "user.email=ci@example.com",
      "-c",
      "commit.gpgsign=false",
      ...args,
    ],
    {
      cwd,
      env: {
        PATH: SHELL_PATH,
        HOME: work,
        GIT_CONFIG_GLOBAL: "/dev/null",
        GIT_CONFIG_NOSYSTEM: "1",
      },
    }
  );
  if (ran.exitCode !== 0) throw new Error(`git ${args.join(" ")}: ${ran.stderr.toString()}`);
  return ran.stdout.toString();
}

/** What a reader of the results makes of `check` at a commit whose rows are `rows`, for the run
 *  `runId` that tested it: the newest result's verdict, or UNKNOWN when Dispatch holds none. A
 *  job that left no result is never a pass. */
function readCheck(rows: readonly Row[], check: string, runId: string): string {
  const newest = rows.find((row) => row.attestation.predicate.check === check);
  return newest === undefined
    ? `UNKNOWN: Dispatch recorded nothing for run ${runId}`
    : newest.attestation.predicate.verdict;
}

/** The run id GitHub's token for the job `env` names, read from a token its runner mints. */
async function runIdOf(env: Readonly<Record<string, string>>): Promise<string> {
  const url = new URL(env.ACTIONS_ID_TOKEN_REQUEST_URL ?? "");
  url.searchParams.set("audience", env.AGENT_SECRETS_MACHINE_AUDIENCE ?? "");
  const response = await fetch(url, {
    headers: { Authorization: `Bearer ${env.ACTIONS_ID_TOKEN_REQUEST_TOKEN ?? ""}` },
  });
  const { value } = (await response.json()) as { value: string };
  const payload = JSON.parse(Buffer.from(value.split(".")[1] ?? "", "base64url").toString()) as {
    run_id: string;
  };
  return payload.run_id;
}

describe.skipIf(!dispatchStackEnabled)(
  "the last step of a CI job writes one result for each check whose test ran",
  () => {
    let stack: CIResultsStack | undefined;
    const ciReport = join(work, "bin", "ci-report");
    const cliState = join(work, "cli-state");

    /** The running stack, or the error that says why no row can run. */
    function running(): CIResultsStack {
      if (stack === undefined) throw new Error("the CI results stack did not start");
      return stack;
    }

    /** A new checkout whose .dispatch/spec is a copy of `spec`, committed with the message
     *  `name`, and the stand-in of GitHub serving those files at that commit. */
    function checkoutOf(name: string, spec: string): Checkout {
      const dir = join(work, "checkouts", name);
      cpSync(spec, join(dir, ".dispatch", "spec"), { recursive: true });
      git(dir, "init", "-q");
      git(dir, "add", "-A");
      git(dir, "commit", "-q", "-m", name);
      const sha = git(dir, "rev-parse", "HEAD").trim();
      running().setFiles(sha, join(dir, ".dispatch", "spec"));
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

    /** The job's last step: ci-report in the checkout, over the report `fixture`, for the checks
     *  of `job`, with the job's variables, `overrides` over them, and nothing of this process's
     *  own but PATH. */
    async function lastStep(
      checkout: Checkout,
      run: JobRun,
      fixture: string,
      job: string = JOB,
      overrides: Readonly<Record<string, string>> = {}
    ): Promise<Reported> {
      const child = Bun.spawn(
        [
          ciReport,
          "--dir",
          ".dispatch/spec",
          "--job",
          job,
          "--report",
          join(FIXTURES, fixture),
          "--project",
          running().project,
          "--dispatch-cli",
          `${process.execPath} ${ENTRY}`,
        ],
        {
          cwd: checkout.dir,
          env: {
            PATH: SHELL_PATH,
            HOME: cliState,
            DISPATCH_STATE_DIR: cliState,
            GITHUB_SHA: run.sha,
            ...running().jobEnv(run),
            ...overrides,
          },
          stdout: "pipe",
          stderr: "pipe",
        }
      );
      const [stdout, stderr, code] = await Promise.all([
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
        child.exited,
      ]);
      const lines = stdout
        .split("\n")
        .filter((text) => text !== "")
        .map((text) => JSON.parse(text) as ReportLine);
      return { code, lines, stderr };
    }

    /** Every stored row at `commit`, as a person reads them, newest first. */
    async function rowsAt(commit: string): Promise<Row[]> {
      return (await running().results(commit)) as unknown as Row[];
    }

    /** The job `run` writing a pass of `check` at its own commit itself, with the real CLI and
     *  not through the step: the command's exit status and what it printed on both streams. */
    async function writeItself(
      run: JobRun,
      check: string
    ): Promise<{ code: number; printed: string }> {
      const written = Bun.spawn(
        [
          process.execPath,
          ENTRY,
          "result",
          "--project",
          running().project,
          "--check",
          check,
          "--verdict",
          "pass",
          "--commit",
          run.sha,
        ],
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

    /** Each line as its check, outcome and exit. */
    function outcomes(reported: Reported): string[] {
      return reported.lines.map((line) => `${line.check} ${line.outcome} exit ${line.exit}`);
    }

    /** Each row as its check, verdict, job, leg, attempt and commit, oldest first. */
    function described(rows: readonly Row[]): string[] {
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
    function legsOf(rows: readonly Row[]): string[] {
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
        packages: ["./cmd/ci-report"],
        stack: "fixture CI job",
        binaries: "./cmd/ci-report",
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
      expect(readFileSync(join(SPEC, "probe.md"), "utf8")).toContain(`#${SECOND} `);
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
      const file = join(checkout.dir, ".dispatch", "spec", "probe.md");
      const committed = readFileSync(file, "utf8");
      const changed = committed.replace(
        'selector="example.com/acme/probe TestSecondProbe" job="go-checks"',
        'selector="example.com/acme/probe TestSecondProbe" job="lint"'
      );
      expect(changed).not.toBe(committed);
      writeFileSync(file, changed);
      expect(git(checkout.dir, "status", "--porcelain")).toBe(" M .dispatch/spec/probe.md\n");

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
      // Parse reads each block of these files, and Validate refuses the two statements that no
      // check serves.
      const spec = join(work, "specs", "statements-without-a-check");
      cpSync(SPEC, spec, { recursive: true });
      for (const [file, id] of [
        ["lonely.md", "a-lonely-statement"],
        ["z-lonely.md", "another-lonely-statement"],
      ] as const) {
        writeFileSync(
          join(spec, file),
          `:::requirement{#${id} kind="behavior"}\nThe probe keeps one claim.\n:::\n`
        );
      }
      const leaf = "leaf-without-check: no statement is under it and no check serves it";
      const first = `.dispatch/spec/lonely.md:1: a-lonely-statement: ${leaf}`;

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
        `.dispatch/spec/z-lonely.md:1: another-lonely-statement: ${leaf}`,
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
        "envoy-go"
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
      const checkout = checkoutOf("matrix", SPEC);
      const probeRows = async (): Promise<Row[]> =>
        (await rowsAt(checkout.sha)).filter((row) => row.attestation.predicate.check === PROBE);

      const legA = await lastStep(
        checkout,
        jobOf(checkout, 601, { legName: "a" }),
        "go-test-all-pass.jsonl"
      );
      const legB = await lastStep(
        checkout,
        jobOf(checkout, 602, { legName: "b" }),
        "go-test-first-fails.jsonl"
      );
      expect([legA.code, legB.code]).toEqual([0, 0]);
      const before = (await probeRows()).find((row) => row.attestation.predicate.leg === "a");

      const again = await lastStep(
        checkout,
        jobOf(checkout, 603, { legName: "b", runAttempt: 2 }),
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
      expect(rows.find((row) => row.attestation.predicate.leg === "a")).toEqual(before as Row);
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
      const checkout = checkoutOf("matrix-leg", SPEC);
      const legA = await lastStep(
        checkout,
        jobOf(checkout, 612, { legName: "a" }),
        "go-test-all-pass.jsonl"
      );
      const legB = await lastStep(
        checkout,
        jobOf(checkout, 613, { legName: "b" }),
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

    // This row cannot fail for a fault of the reporter: it reads no rows at a commit where no step
    // ran, and the UNKNOWN text comes from this file's own readCheck. No product code decides
    // "unknown" yet; the graph logic owns that. So nobody may count this row as evidence that "a
    // step that left no result reads as unknown".
    test("a job cancelled before its last step leaves no row at its commit, which reads as unknown and never as a pass", async () => {
      // Another push, whose job did report, holds a pass of the same check at its own commit.
      const reported = checkoutOf("cancelled-sibling", SPEC);
      const sibling = await lastStep(reported, jobOf(reported, 701), "go-test-all-pass.jsonl");
      expect(sibling.code).toBe(0);

      const cancelled = checkoutOf("cancelled", SPEC);
      const runId = await runIdOf(running().jobEnv(jobOf(cancelled, 702)));

      const rows = await rowsAt(cancelled.sha);
      const read = readCheck(rows, PROBE, runId);
      console.log(read);
      expect({ rows: rows.length, read }).toEqual({
        rows: 0,
        read: `UNKNOWN: Dispatch recorded nothing for run ${runId}`,
      });
    }, 120_000);

    // v-recorder: a build that records a result for another commit than the one that the job
    // tested fails. The job's token and GITHUB_SHA name the commit that the job tested; the
    // checkout then moves to a commit of its own, and the step reads that commit with git, so
    // Dispatch refuses each write, and no row is stored at either commit.
    test("a checkout whose HEAD moved after the job's commit writes no result for either commit, and each write is refused", async () => {
      const checkout = checkoutOf("moved-head", SPEC);
      const run = jobOf(checkout, 901);
      git(checkout.dir, "commit", "--allow-empty", "-q", "-m", "the checkout moves");
      const moved = git(checkout.dir, "rev-parse", "HEAD").trim();
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
    // traded at the stack's real broker; only Dispatch is a stand-in.
    test("a write whose 201 echoes its key and gives no stored result counts as written, and its line says that the answer was lost", async () => {
      const checkout = checkoutOf("answer-lost", SPEC_WITHOUT_SECOND);
      const { seen, server } = answeringDispatch(true);
      try {
        const reported = await lastStep(
          checkout,
          jobOf(checkout, 951),
          "go-test-all-pass.jsonl",
          JOB,
          { DISPATCH_URL: `http://127.0.0.1:${server.port}` }
        );
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
        const reported = await lastStep(
          checkout,
          jobOf(checkout, 952),
          "go-test-all-pass.jsonl",
          JOB,
          { DISPATCH_URL: `http://127.0.0.1:${server.port}` }
        );
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
        const reported = await lastStep(
          checkout,
          jobOf(checkout, 953),
          "go-test-all-pass.jsonl",
          JOB,
          { DISPATCH_URL: `http://127.0.0.1:${server.port}` }
        );
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
  }
);
