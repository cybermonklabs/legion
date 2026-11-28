import { afterAll, beforeAll, describe, expect, setDefaultTimeout, test } from "bun:test";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { fileLimit, ROW_MARGIN_MS } from "@legion/test-fixtures/row-clock";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  CI_CHECK,
  type CIResultsStack,
  type JobRun,
  startCIResultsStack,
  useCIResultsStackDirectory,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";

// Each row starts the real entry once, as results-reading-one-line.test.ts's rows do, whose start
// took 9.97 s at a load of 568 on 288 CPUs: 60 s for that start, and the row clock's margin.
setDefaultTimeout(fileLimit(60_000 + ROW_MARGIN_MS));

// What a CI job's `dispatch result --issue` prints goes into its runner's log, which reads a line
// that starts with `::`, and a `##[` anywhere, as a workflow command. Each line it prints stays one
// line, by runnerlog.Printable's rule (runner-log.ts), when Dispatch's refusal holds a line break
// or `::error`. Those rows run the real entry as a CI job's step runs it, through the stack's
// `jobCommand` (ci-results-stack.ts): the child of the real `agent-secrets result-grant`, which
// trades the run's token at the real broker and gives the command the airlock's placeholder of
// the run alone, on the way through the broker's airlock proxy to the real Dispatch server.
//
// A job's log that `dispatch results --issue` prints follows the same rule: the whole log is one
// line, its line breaks written as `\n`, and no line is a workflow command. That read is a
// session's, no CI job's, and runs the real entry against a stub of Dispatch.

const SHA = "0123456789abcdef0123456789abcdef01234567";
const entry = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
let answer: () => Response = () => Response.json({}, { status: 500 });

const dispatch = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => answer() });
afterAll(() => {
  dispatch.stop(true);
});

/** A fresh home for each run, which the fixture removes after the test that made it. */
const makeHome = temporaryDirectories("ci-job-one-line");

/** The lines `command` prints, run with `env`, PATH and a fresh home. */
async function printedBy(
  command: readonly string[],
  env: Readonly<Record<string, string>>
): Promise<string[]> {
  const home = makeHome();
  const child = Bun.spawn([...command], {
    cwd: home,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      DISPATCH_STATE_DIR: home,
      ...env,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stdout, stderr] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return `${stdout}${stderr}`.replace(/\n$/, "").split("\n");
}

/** The lines the entry prints, run with `env` beside Dispatch's stub. */
function run(argv: readonly string[], env: Readonly<Record<string, string>>): Promise<string[]> {
  return printedBy([process.execPath, entry, ...argv], {
    DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
    ...env,
  });
}

/** No printed line is a workflow command. */
function noWorkflowCommand(printed: readonly string[]): void {
  expect(
    printed.filter((line) => line.trimStart().startsWith("::") || line.includes("##["))
  ).toEqual([]);
}

const stackDirectory = useCIResultsStackDirectory();

describe.skipIf(!dispatchStackEnabled)(
  "dispatch result --issue as a CI job: each printed line stays one line",
  () => {
    let stack: CIResultsStack | undefined;

    /** The running stack, or the error that says why no row can run. */
    function running(): CIResultsStack {
      if (stack === undefined) throw new Error("the CI results stack did not start");
      return stack;
    }

    /** A run of the job go-checks on a push of the issue's branch at `sha`, with a check run of
     *  its own: the branch's caller calls the job, so GitHub names its check run `ci / <job>`. */
    function pushAt(sha: string, checkRunId: number, overrides: Partial<JobRun> = {}): JobRun {
      return {
        sha,
        ref: `refs/heads/legion/${running().issue}`,
        job: "go-checks",
        called: true,
        runAttempt: 1,
        checkRunId,
        ...overrides,
      };
    }

    /** The lines the job `run` prints for its pass of `check` at the commit it tested, run as a CI
     *  job's step runs it: through the stack's `jobCommand`, with the job's variables alone. */
    function recordAs(run: JobRun, check: string): Promise<string[]> {
      const argv = [
        "result",
        "--issue",
        running().issue,
        "--check",
        check,
        "--verdict",
        "pass",
        "--commit",
        run.sha,
      ];
      return printedBy(
        running().jobCommand([process.execPath, entry, ...argv]),
        running().jobEnv(run)
      );
    }

    beforeAll(async () => {
      stack = await startCIResultsStack({ work: stackDirectory });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("a check id that holds a line break and ::error, which Dispatch's refusal names", async () => {
      const check = `${CI_CHECK}\n::error title=x::injected`;
      const printed = await recordAs(pushAt("a".repeat(40), 301), check);
      noWorkflowCommand(printed);
      expect(printed).toEqual([
        `UNKNOWN_CHECK: ${CI_CHECK}\\n::error title=x::injected is no check of this issue's Verification document or of one above it`,
      ]);
    }, 120_000);

    // Dispatch's refusals of a job's result name the check as the caller wrote it (UNKNOWN_CHECK,
    // the row above), and quote each other text a job sets, as Go quotes a string: here the name
    // of the job's check run, which a matrix value sets, holding a line break and ::warning.
    test("a refusal of Dispatch that quotes a check run name holding a line break and ::warning", async () => {
      const printed = await recordAs(
        pushAt("b".repeat(40), 302, { job: "go-checks (x\n::warning::y)" }),
        CI_CHECK
      );
      noWorkflowCommand(printed);
      expect(printed).toEqual([
        `CHECK_NOT_THIS_JOBS: check ${CI_CHECK} names CI job "ci / go-checks", and this token's check run is "ci / go-checks (x\\n::warning::y)": a job records results only for the checks whose ci_check names its check run`,
      ]);
    }, 120_000);

    test("the control: a refusal with no line break prints exactly its one line", async () => {
      const printed = await recordAs(
        pushAt("c".repeat(40), 303, { job: "go-checks (x)" }),
        CI_CHECK
      );
      expect(printed).toEqual([
        `CHECK_NOT_THIS_JOBS: check ${CI_CHECK} names CI job "ci / go-checks", and this token's check run is "ci / go-checks (x)": a job records results only for the checks whose ci_check names its check run`,
      ]);
    }, 120_000);
  }
);

describe("dispatch results --issue: a job's log prints on one line, and no line is a workflow command", () => {
  /** The read of ACME-1, whose check c1 CI proves holds one result its job recorded. */
  const read = (observed: string) => ({
    issue: "ACME-1",
    verification_artifact_id: "artifact-1",
    checks: [
      {
        block_id: "c1",
        ref: "dispatch://ACME-1/artifact/verification-md#b-c1",
        issue_key: "ACME-1",
        artifact_id: "artifact-1",
        text: "The server builds at the head.",
        when: "before_merge",
        surface: "",
        run: "",
        negative_control: "A build whose server does not build.",
        ci_check: "go-checks",
        ci: {
          run: "go-checks",
          state: "no_result",
          case: "no_head",
          detail: "Dispatch holds no open or merged pull request head of ACME-1.",
          line: "- Check c1 (before_merge): CI run go-checks has no result (no_head)",
        },
        version: 1,
        sha256: "digest",
        results: [],
        recorded_results: [
          {
            id: "result-1",
            issue_key: "ACME-1",
            check_issue_key: "ACME-1",
            check_artifact_id: "artifact-1",
            check_block_id: "c1",
            check_version: 1,
            check_sha256: "digest",
            check_words_moved: false,
            role: "ci",
            verdict: "fail",
            commit_sha: SHA,
            commit_stands: { answer: "other_commit" },
            branch: "legion/ACME-1",
            surface: "",
            run: "go-checks",
            observed,
            negative_control: "A build whose server does not build.",
            failed: 0,
            skipped: 0,
            excluded: 0,
            screenshots: [],
            session: "ci-results:example",
            actor: { kind: "session", id: "ci-results:example" },
            created_at: "2026-11-16T10:00:00Z",
          },
        ],
        compared_with: [],
        pass_stands: { answer: "no" },
      },
    ],
  });
  const asSession = {
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: "one-line-reader",
    DISPATCH_TOKEN: fixtureSecret("session"),
  };

  test("a job's log holding ::error and ##[ prints neither at the start of a line", async () => {
    answer = () =>
      Response.json(read("--- FAIL: TestX\n::error file=a.go,line=1::boom\n##[group]x"));
    const printed = await run(["results", "--issue", "ACME-1"], asSession);
    noWorkflowCommand(printed);
    expect(printed).toContain(
      "    Reading: --- FAIL: TestX\\n::error file=a.go,line=1::boom\\n##\\x5bgroup]x"
    );
  });

  test("the control: a job's log of two plain lines prints both on the one line of its reading", async () => {
    answer = () => Response.json(read("--- FAIL: TestX\nsecond line"));
    const printed = await run(["results", "--issue", "ACME-1"], asSession);
    expect(printed).toContain("    Reading: --- FAIL: TestX\\nsecond line");
    expect(printed).not.toContain("second line");
  });
});
