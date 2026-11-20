import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  CI_CHECK,
  type CIResultsStack,
  type JobRun,
  ORDINARY_CHECK,
  startCIResultsStack,
  useCIResultsStackDirectory,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";

// The results route of an issue over its whole length, for a check that its ci_check gives to a
// CI job: the job's own GitHub token, traded at the real secrets broker under a real result entry
// for a results token, presented by the real `dispatch` CLI of this checkout, run as a child
// process, to the real Dispatch server, which reads the job's check run and the head of the
// branch the job pushed from a stand-in of GitHub. The Go tests of the route prove its rules;
// this file proves that the command a CI job runs records the result Dispatch keeps, and that
// the command a reader runs prints where that result stands.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

/** The CLI entry a row runs, as a CI job's shell reaches it: this checkout's own `dispatch`. */
const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
/** The job that the check's ci_check names, which each check run below is named after. */
const JOB = "go-checks";
/** The session that reads the results back, as an agent's command reads them. */
const SESSION = "session-ci-check-results-live";
/** The address of no server: a command that sent any request to it would fail to connect. */
const NOWHERE = "http://127.0.0.1:1";

/** A commit of its own for each row, so what a row reads back is that row's alone. */
function commit(digit: string): string {
  return digit.repeat(40);
}

const stackDirectory = useCIResultsStackDirectory();
const cliState = temporaryDirectory("ci-check-results-cli-state");

/** What one run of the CLI printed and how it ended. */
interface Ran {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** The in-toto statement of a job's result, as far as a reader of it reads it. */
interface ReadStatement {
  readonly _type: string;
  readonly subject: readonly { readonly name: string; readonly digest: Record<string, string> }[];
  readonly predicateType: string;
  readonly predicate: {
    readonly check: string;
    readonly procedure_digest: string;
    readonly run: { readonly check_run_id: string; readonly attempt: number };
    readonly recorder: { readonly session: string };
  };
}

/** One result of `dispatch results --issue --json`, as far as these rows read it. */
interface ReadResult {
  readonly id: string;
  readonly role: string;
  readonly verdict: string;
  readonly commit_sha: string;
  readonly branch?: string;
  readonly check_sha256: string;
  readonly session: string;
  readonly attestation?: ReadStatement;
}

/** One check of `dispatch results --issue --json`: the route's answer whole, which keeps the
 *  branch heads in branch_heads and a check CI proves's recorded results in recorded_results,
 *  out of the fields a command released before them reads. */
interface ReadCheck {
  readonly block_id: string;
  readonly ref: string;
  readonly compared_with?: readonly Record<string, unknown>[];
  readonly branch_heads?: readonly Record<string, unknown>[];
  readonly pass_stands?: {
    readonly answer: string;
    readonly roles?: readonly string[];
    readonly subjects?: readonly Record<string, unknown>[];
  };
  readonly ci?: { readonly case: string };
  readonly results?: readonly ReadResult[];
  readonly recorded_results?: readonly ReadResult[];
}

describe.skipIf(!dispatchStackEnabled)(
  "a CI job records the check its ci_check names with its own identity, and each reader names the branch head",
  () => {
    let stack: CIResultsStack | undefined;

    /** The running stack, or the error that says why no row can run. */
    function running(): CIResultsStack {
      if (stack === undefined) throw new Error("the CI results stack did not start");
      return stack;
    }

    /** The branch the job pushes: the issue's own, of two segments, which the entry takes. */
    function branch(): string {
      return `legion/${running().issue}`;
    }

    /** The real CLI entry run with `env` and nothing of this process's own but PATH. */
    async function cli(
      args: readonly string[],
      env: Readonly<Record<string, string>>
    ): Promise<Ran> {
      const child = Bun.spawn([process.execPath, ENTRY, ...args], {
        env: { PATH: SHELL_PATH, HOME: cliState, DISPATCH_STATE_DIR: cliState, ...env },
        stdout: "pipe",
        stderr: "pipe",
      });
      const [stdout, stderr, code] = await Promise.all([
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
        child.exited,
      ]);
      return { code, stdout, stderr };
    }

    /** A session's own command: a host, a session and the session's own token. */
    function asSession(): Record<string, string> {
      return {
        DISPATCH_URL: running().dispatch.url,
        DISPATCH_TOKEN: running().dispatch.token,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: SESSION,
      };
    }

    /** A run of the job on a push of the issue's branch at `sha`, with a check run of its own. */
    function pushAt(sha: string, checkRunId: number, overrides: Partial<JobRun> = {}): JobRun {
      return {
        sha,
        ref: `refs/heads/${branch()}`,
        job: JOB,
        runAttempt: 1,
        checkRunId,
        ...overrides,
      };
    }

    /** `dispatch result --issue` for `check` at `at`, as the job `run` describes. */
    function recordOnIssue(
      run: JobRun,
      at: string,
      check: string = CI_CHECK,
      extra: readonly string[] = [],
      env: Readonly<Record<string, string>> = running().jobEnv(run)
    ): Promise<Ran> {
      return cli(
        [
          "result",
          "--issue",
          running().issue,
          "--check",
          check,
          "--verdict",
          "pass",
          "--commit",
          at,
          "--observed",
          "ok",
          ...extra,
        ],
        env
      );
    }

    /** A refusal from Dispatch: the CLI exited 1 and named `code` in what it printed. */
    function refused(ran: Ran, code: string): void {
      expect({ code: ran.code, named: `${ran.stderr}${ran.stdout}`.includes(code) }).toEqual({
        code: 1,
        named: true,
      });
    }

    /** The issue's checks as `dispatch results --issue --json` prints them. */
    async function readChecks(): Promise<readonly ReadCheck[]> {
      const ran = await cli(["results", "--issue", running().issue, "--json"], asSession());
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const read: { readonly checks: readonly ReadCheck[] } = JSON.parse(ran.stdout);
      return read.checks;
    }

    /** Check `id` of `checks`. */
    function checkOf(checks: readonly ReadCheck[], id: string): ReadCheck {
      const found = checks.find((check) => check.block_id === id);
      if (found === undefined) throw new Error(`the read holds no check ${id}`);
      return found;
    }

    /** Every result of `check`, in results and in recorded_results. */
    function allResults(check: ReadCheck): readonly ReadResult[] {
      return [...(check.results ?? []), ...(check.recorded_results ?? [])];
    }

    /** The results of `check` that a CI job recorded at `at`. */
    function jobResultsAt(check: ReadCheck, at: string): readonly ReadResult[] {
      return allResults(check).filter(
        (result) => result.branch !== undefined && result.commit_sha === at
      );
    }

    beforeAll(async () => {
      stack = await startCIResultsStack({ work: stackDirectory });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("a job's pass on a push of the issue's branch stands at that branch's head with no pull request, and carries the statement of its run", async () => {
      const at = commit("1");
      running().setBranch(branch(), at);
      const ran = await recordOnIssue(pushAt(at, 201), at);

      expect({ code: ran.code, stderr: ran.stderr }, ran.stdout).toEqual({ code: 0, stderr: "" });
      const c1 = checkOf(await readChecks(), CI_CHECK);
      // The branch head is in branch_heads, and compared_with names none, as a command released
      // before the branch heads reads it.
      expect({ compared: c1.compared_with, heads: c1.branch_heads }).toEqual({
        compared: [],
        heads: [{ kind: "branch_head", branch: branch(), commit: at }],
      });
      const [job, ...others] = jobResultsAt(c1, at);
      expect({ job: job?.role, others: others.length }).toEqual({ job: "ci", others: 0 });
      // The branch head is the issue's one subject, and a pass stands there: the subject names the
      // role whose pass counts at it and the result that is that pass.
      expect(c1.pass_stands).toEqual({
        answer: "yes",
        roles: ["ci"],
        subjects: [
          {
            basis: { kind: "branch_head", branch: branch(), commit: at },
            answer: "yes",
            roles: ["ci"],
            results: [job?.id],
          },
        ],
      });
      // Dispatch holds no pull request of the issue: the check run is read at no head.
      expect(c1.ci?.case).toBe("no_head");

      const statement = job?.attestation;
      expect(statement?._type).toBe("https://in-toto.io/Statement/v1");
      expect(statement?.predicateType).toBe(
        "https://github.com/cybermonklabs/legion/attestations/check-block-result/v1"
      );
      expect(statement?.subject).toEqual([
        { name: `git+https://github.com/${running().repository}`, digest: { gitCommit: at } },
      ]);
      expect({
        check: statement?.predicate.check,
        procedureDigest: statement?.predicate.procedure_digest,
        checkRunId: statement?.predicate.run.check_run_id,
        attempt: statement?.predicate.run.attempt,
        session: statement?.predicate.recorder.session,
      }).toEqual({
        check: c1.ref,
        procedureDigest: job?.check_sha256,
        checkRunId: "201",
        attempt: 1,
        session: job?.session,
      });
      // The result entry's person ran no test, and the job's log is public for a public
      // repository: neither the statement nor the command's output names that person.
      expect(JSON.stringify(statement)).not.toContain(running().person);
      expect(`${ran.stdout}${ran.stderr}`).not.toContain(running().person);
    }, 120_000);

    test("dispatch results prints the job's result on its branch and no pull request line, and each attempt is a line of its own", async () => {
      const at = commit("2");
      running().setBranch(branch(), at);
      const line = `- pass by ci on branch ${branch()} at ${at}. That commit is at the head of branch ${branch()}.`;
      const lines = (text: string) => text.split("\n").filter((row) => row.includes(line)).length;

      expect((await recordOnIssue(pushAt(at, 202), at)).code).toBe(0);
      const first = await cli(["results", "--issue", running().issue], asSession());
      expect({ code: first.code, stderr: first.stderr }).toEqual({ code: 0, stderr: "" });
      expect(lines(first.stdout), first.stdout).toBe(1);
      expect(first.stdout).toContain(
        `- ${CI_CHECK} (before_merge): its CI job's results below, and CI run go-checks derives none: Dispatch holds no open or merged pull request head of ${running().issue}.`
      );
      expect(first.stdout).not.toContain("Pull request line:");

      expect((await recordOnIssue(pushAt(at, 203, { runAttempt: 2 }), at)).code).toBe(0);
      const second = await cli(["results", "--issue", running().issue], asSession());
      expect(lines(second.stdout), second.stdout).toBe(2);
    }, 120_000);

    test("the job's pass stands no more once its branch moves on", async () => {
      const at = commit("3");
      const moved = commit("4");
      running().setBranch(branch(), at);
      expect((await recordOnIssue(pushAt(at, 204), at)).code).toBe(0);
      expect(checkOf(await readChecks(), CI_CHECK).pass_stands?.answer).toBe("yes");

      running().setBranch(branch(), moved);
      const c1 = checkOf(await readChecks(), CI_CHECK);
      expect(c1.branch_heads).toEqual([{ kind: "branch_head", branch: branch(), commit: moved }]);
      expect(c1.pass_stands?.answer).toBe("no");
    }, 120_000);

    test("a result for a check that its ci_check does not give to the job is refused, and nothing is stored", async () => {
      const at = commit("5");
      refused(await recordOnIssue(pushAt(at, 205), at, ORDINARY_CHECK), "CHECK_NOT_THIS_JOBS");
      expect(checkOf(await readChecks(), ORDINARY_CHECK).results ?? []).toEqual([]);
    }, 120_000);

    test("a result for another commit than the one the job tested is refused, and nothing is stored", async () => {
      const tested = commit("6");
      const other = commit("7");
      refused(await recordOnIssue(pushAt(tested, 206), other), "COMMIT_NOT_THE_RUNS");
      const c1 = checkOf(await readChecks(), CI_CHECK);
      expect([...jobResultsAt(c1, tested), ...jobResultsAt(c1, other)]).toEqual([]);
    }, 120_000);

    test("a role's argument with the job's identity is refused before any request, naming its flag", async () => {
      const at = commit("8");
      // Dispatch and the broker are at no server: a command that sent anything would fail to
      // connect, and exit 1 saying so.
      const env = {
        ...running().jobEnv(pushAt(at, 207)),
        DISPATCH_URL: NOWHERE,
        AGENT_SECRETS_URL: NOWHERE,
      };
      const ran = await recordOnIssue(pushAt(at, 207), at, CI_CHECK, ["--role", "tester"], env);

      expect({ code: ran.code, named: ran.stdout.includes("--role") }, ran.stdout).toEqual({
        code: 2,
        named: true,
      });
      expect(jobResultsAt(checkOf(await readChecks(), CI_CHECK), at)).toEqual([]);
    }, 120_000);

    test("a session's own token still records no result of a check CI proves", async () => {
      const at = commit("9");
      const ran = await cli(
        [
          "result",
          "--issue",
          running().issue,
          "--check",
          CI_CHECK,
          "--role",
          "tester",
          "--verdict",
          "pass",
          "--commit",
          at,
        ],
        asSession()
      );
      refused(ran, "CHECK_PROVED_BY_CI");
      expect(
        allResults(checkOf(await readChecks(), CI_CHECK)).filter((r) => r.commit_sha === at)
      ).toEqual([]);
    }, 120_000);
  }
);
