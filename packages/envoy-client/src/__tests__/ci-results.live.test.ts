import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { newDpopKey } from "@legion/cipher-client/dpop";
import {
  SPEC_CHECK_OBSERVED_CUT_NOTE,
  SPEC_CHECK_OBSERVED_END,
  SPEC_CHECK_OBSERVED_MAX,
  SPEC_CHECK_OBSERVED_START,
} from "@legion/contracts";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  type CIResultsStack,
  type JobRun,
  OTHER_JOBS_CHECK,
  PROBE_CHECK,
  startCIResultsStack,
  useCIResultsStackDirectory,
  writeProbeSpec,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";

// The v-recorder path over its whole length: a CI job's own GitHub token, traded at the real
// secrets broker (./cmd/cipherd) under a real result entry for a token that writes results only,
// presented by the real `dispatch` CLI of this checkout, run as a child process, to the real
// Dispatch server (./cmd/dispatch), which reads the job's check run and the spec files at the
// run's commit from a stand-in of GitHub. The Go tests of each part prove its own rules; only
// this file proves that the token the broker mints is the token Dispatch serves, and that the
// command a CI job runs writes the result Dispatch keeps.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

/** The CLI entry a row runs, as a CI job's shell reaches it: this checkout's own `dispatch`. */
const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");

/** The branch every run below is a push of: two segments, which the result entry's pattern
 *  `refs/heads/*` `/*` takes. */
const REF = "refs/heads/topic/probe";
/** The job the probe check names, which the check run of each run below is named after. */
const JOB = "go-checks";
/** The agent session that reads the results back: a session's own command runs with a host and a
 *  session, which a CI job's `dispatch result --project` needs neither of. */
const SESSION = "session-ci-results-live";

/** A commit of its own for each row, so what a row reads back is that row's alone. */
function commit(digit: string): string {
  return digit.repeat(40);
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped every process writing into it.
const stackDirectory = useCIResultsStackDirectory();
const cliState = temporaryDirectory("ci-results-cli-state");
const specFiles = temporaryDirectory("ci-results-spec");
/** The probe spec beside a file whose block never closes, and the probe spec with its check's
 *  failing example left out: two trees the graph logic refuses (specgraph.Judge). */
const unclosedFiles = temporaryDirectory("ci-results-unclosed-spec");
const lackingFiles = temporaryDirectory("ci-results-lacking-spec");
/** The observed texts the rows below read from files, as a job's step writes its log. */
const observedFiles = temporaryDirectory("ci-results-observed");

/** What one run of the CLI printed and how it ended. */
interface Ran {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

describe.skipIf(!dispatchStackEnabled)(
  "a CI job writes its result with its own identity, and Dispatch keeps it",
  () => {
    let stack: CIResultsStack | undefined;

    /** The running stack, or the error that says why no row can run. */
    function running(): CIResultsStack {
      if (stack === undefined) throw new Error("the CI results stack did not start");
      return stack;
    }

    /** The real CLI entry run with `env` and nothing of this process's own but PATH. */
    async function cli(
      args: readonly string[],
      env: Readonly<Record<string, string>>
    ): Promise<Ran> {
      const producer =
        env["AGENT_SECRETS_MACHINE_IDENTITY"] === "github-actions"
          ? [running().agentSecrets, "result-grant", "--"]
          : [];
      const child = Bun.spawn([...producer, process.execPath, ENTRY, ...args], {
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

    /** `dispatch result --project` for `check`, at `at`, as the job `run` describes, with the
     *  observed text `observed` gives. */
    function writeResult(
      run: JobRun,
      at: string,
      check: string = PROBE_CHECK,
      observed: readonly string[] = ["--observed", "ok  \texample.com/acme/probe\t0.01s"]
    ): Promise<Ran> {
      return cli(
        [
          "result",
          "--project",
          running().project,
          "--check",
          check,
          "--verdict",
          "pass",
          "--commit",
          at,
          ...observed,
        ],
        running().jobEnv(run)
      );
    }

    /** A run of the job on REF at `sha`, with a check run of its own, whose commit holds the
     *  spec files of `files`. */
    function runAt(
      sha: string,
      checkRunId: number,
      overrides: Partial<JobRun> = {},
      files: string = specFiles
    ): JobRun {
      running().setFiles(sha, files);
      return { sha, ref: REF, job: JOB, runAttempt: 1, checkRunId, ...overrides };
    }

    /** A refusal: the CLI exited 1 and named `code` in what it printed. */
    function refused(ran: Ran, code: string): void {
      expect({ code: ran.code, named: `${ran.stderr}${ran.stdout}`.includes(code) }).toEqual({
        code: 1,
        named: true,
      });
    }

    /** A session's own command: a host, a session and the session's own token, the one the
     *  stack's Dispatch takes for an agent. */
    function asSession(): Record<string, string> {
      return {
        DISPATCH_URL: running().dispatch.url,
        DISPATCH_TOKEN: running().dispatch.token,
        DISPATCH_HOST: "omp",
        DISPATCH_SESSION_ID: SESSION,
      };
    }

    beforeAll(async () => {
      writeProbeSpec(specFiles);
      writeProbeSpec(unclosedFiles);
      writeFileSync(
        join(unclosedFiles, "broken.md"),
        ':::requirement{#broken kind="behavior"}\nThis block never closes.\n'
      );
      writeProbeSpec(lackingFiles);
      const probe = await Bun.file(join(lackingFiles, "probe.md")).text();
      writeFileSync(
        join(lackingFiles, "probe.md"),
        probe.replace("Failing example: A probe test that fails.\n", "")
      );
      stack = await startCIResultsStack({ work: stackDirectory });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("a CI job's pass is stored at the commit it tested, under the job its check run names", async () => {
      const at = commit("1");
      const ran = await writeResult(runAt(at, 101), at);

      if (ran.code !== 0)
        throw new Error(
          `the real result command failed: ${JSON.stringify(ran)}; TLS front: ${await Bun.file(join(stackDirectory, "tls", "tls-front.log")).text()}`
        );
      expect({ code: ran.code, stderr: ran.stderr }).toEqual({ code: 0, stderr: "" });
      const stored = JSON.parse(ran.stdout) as {
        id: string;
        seq: number;
        kind: string;
        attestation: {
          subject: { digest: { gitCommit: string } }[];
          predicate: {
            project: string;
            check: string;
            verdict: string;
            job: string;
            leg: string;
            run: {
              run_id: string;
              check_run_id: string;
              attempt: number;
              ref: string;
              event_name: string;
            };
            recorder: { session: string; principal: string };
          };
        };
      };
      const { predicate } = stored.attestation;
      expect({
        kind: stored.kind,
        commit: stored.attestation.subject[0]?.digest.gitCommit,
        project: predicate.project,
        check: predicate.check,
        verdict: predicate.verdict,
        job: predicate.job,
        leg: predicate.leg,
        checkRun: predicate.run.check_run_id,
        attempt: predicate.run.attempt,
        ref: predicate.run.ref,
        event: predicate.run.event_name,
        recorder: predicate.recorder,
      }).toEqual({
        kind: "run",
        commit: at,
        project: running().project,
        check: PROBE_CHECK,
        verdict: "pass",
        job: JOB,
        leg: "",
        checkRun: "101",
        attempt: 1,
        ref: REF,
        event: "push",
        recorder: {
          session: running().resultSession,
          principal: running().resultPrincipal,
        },
      });
      expect(predicate.recorder).not.toHaveProperty("person");

      // A person reads the one result back, and so does `dispatch results --project` under a
      // session's own token, which reads results and writes none.
      expect((await running().results(at)).map((row) => row.id)).toEqual([stored.id]);
      const listed = await cli(
        ["results", "--project", running().project, "--commit", at],
        asSession()
      );
      expect({ code: listed.code, stderr: listed.stderr }).toEqual({ code: 0, stderr: "" });
      expect(listed.stdout).toContain(
        `- #${stored.seq} ${PROBE_CHECK}: pass. Job ${JOB}, no leg, run ${predicate.run.run_id} attempt 1, commit ${at}.`
      );
    }, 120_000);

    test("a result for another commit than the one the job tested is refused, and nothing is stored", async () => {
      const tested = commit("2");
      const other = commit("3");
      running().setFiles(other, specFiles);

      refused(await writeResult(runAt(tested, 102), other), "COMMIT_NOT_THE_RUNS");
      expect(await running().results(other)).toEqual([]);
      expect(await running().results(tested)).toEqual([]);
    }, 120_000);

    test("a result for a check that names another job is refused, and nothing is stored", async () => {
      const at = commit("4");

      refused(await writeResult(runAt(at, 103), at, OTHER_JOBS_CHECK), "CHECK_NOT_THIS_JOBS");
      expect(await running().results(at)).toEqual([]);
    }, 120_000);

    test("a run token signed by a key the issuer does not publish gets no token at the broker, and nothing is stored", async () => {
      const at = commit("5");

      refused(await writeResult(runAt(at, 104, { forged: true }), at), "invalid_request");
      expect(await running().results(at)).toEqual([]);
    }, 120_000);

    test("a run on a ref that no result entry lists gets no token, and nothing is stored", async () => {
      const at = commit("6");
      const ref = "refs/heads/a/b/c";

      const ran = await writeResult(runAt(at, 105, { ref }), at);
      refused(ran, ref);
      expect(await running().results(at)).toEqual([]);
    }, 120_000);

    test("a random access token with a valid DPoP proof is refused as not active, and nothing is stored", async () => {
      const at = commit("7");
      running().setFiles(at, specFiles);
      const url = `${running().dispatch.url}/api/v1/projects/${running().project}/results`;
      const accessToken = crypto.randomUUID();
      const key = await newDpopKey();

      const response = await fetch(url, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `DPoP ${accessToken}`,
          DPoP: await key.proof({ method: "POST", url, accessToken }),
        },
        body: JSON.stringify({ commit: at, check: PROBE_CHECK, verdict: "pass", observed: "" }),
      });
      const body = (await response.json()) as { code?: string };

      expect({ status: response.status, code: body.code }).toEqual({
        status: 401,
        code: "BROKER_TOKEN_NOT_ACTIVE",
      });
      expect(await running().results(at)).toEqual([]);
    }, 120_000);

    test("a session's own token writes no result: the command sends none, and Dispatch refuses it", async () => {
      const at = commit("8");
      running().setFiles(at, specFiles);

      // The project form writes with the machine's own identity alone and reads no bearer, so with
      // no secrets broker named the command refuses before any request.
      const ran = await cli(
        [
          "result",
          "--project",
          running().project,
          "--check",
          PROBE_CHECK,
          "--verdict",
          "pass",
          "--commit",
          at,
        ],
        asSession()
      );
      expect({
        code: ran.code,
        named: `${ran.stderr}${ran.stdout}`.includes("DISPATCH_RESULTS_TOKEN"),
      }).toEqual({
        code: 2,
        named: true,
      });
      // The same write under the session's token, which another client can send, is refused by
      // Dispatch itself.
      const response = await fetch(
        `${running().dispatch.url}/api/v1/projects/${running().project}/results`,
        {
          method: "POST",
          headers: {
            "Content-Type": "application/json",
            Authorization: `Bearer ${running().dispatch.token}`,
          },
          body: JSON.stringify({ commit: at, check: PROBE_CHECK, verdict: "pass", observed: "" }),
        }
      );
      const body = (await response.json()) as { code?: string };
      expect({ status: response.status, code: body.code }).toEqual({
        status: 403,
        code: "RESULTS_SCOPE_REQUIRED",
      });
      expect(await running().results(at)).toEqual([]);
    }, 120_000);

    test("the results token reaches no other route: the airlock refuses what its rule does not allow, and Dispatch the rest", async () => {
      const at = commit("9");
      const project = running().project;

      // A separate real child receives only the airlock placeholder and uses it on four other
      // routes. The rule allows POST under /api/v1/projects/ alone, so the proxy refuses the
      // first three itself, before it dials, and says so (Airlock-Forwarded: false and its
      // Airlock-Code); the fourth is under the prefix, so the proxy carries it with the run's
      // token and a proof of the broker's key, and Dispatch's own gate refuses the scope.
      const env = running().jobEnv(runAt(at, 106));
      const routes = [
        ["GET", `/api/v1/issues?project=${project}`, null],
        ["POST", "/api/v1/issues", { project, title: "An issue a CI job writes" }],
        ["GET", `/api/v1/projects/${project}/results?commit=${at}`, null],
        ["POST", `/api/v1/projects/${project}/artifacts`, { name: "an artifact a CI job writes" }],
      ];
      const probe = `import { ownIdentityDispatch } from ${JSON.stringify(join(import.meta.dir, "..", "dispatch-config.ts"))};
const resolution=ownIdentityDispatch(process.env);
if(!resolution.enabled) throw new Error(resolution.error);
const answers=[];
for(const [method,route,body] of ${JSON.stringify(routes)}) {
 const url=resolution.url+route;
 const response=await fetch(url,{method,headers:{...(body===null?{}:{"Content-Type":"application/json"}),...await resolution.credential.headers(method,url)},...(body===null?{}:{body:JSON.stringify(body)})});
 const text=await response.text();
 const forwarded=response.headers.get("Airlock-Forwarded");
 let code=response.headers.get("Airlock-Code");
 if (forwarded!=="false") { if (!text.startsWith("{")) throw new Error("scope response: "+response.status+" "+text); code=JSON.parse(text).code; }
 answers.push(method+" "+route+": "+response.status+" "+(forwarded==="false"?"airlock":"dispatch")+" "+code);
}
console.log(JSON.stringify(answers));
`;
      const child = Bun.spawn(
        [running().agentSecrets, "result-grant", "--", process.execPath, "-e", probe],
        { env: { PATH: SHELL_PATH, HOME: cliState, ...env }, stdout: "pipe", stderr: "pipe" }
      );
      const [output, errors, code] = await Promise.all([
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
        child.exited,
      ]);
      expect({ code, errors }).toEqual({ code: 0, errors: "" });
      const answers = JSON.parse(output);

      expect(answers).toEqual([
        `GET /api/v1/issues?project=${project}: 403 airlock REQUEST_NOT_ALLOWED`,
        "POST /api/v1/issues: 403 airlock REQUEST_NOT_ALLOWED",
        `GET /api/v1/projects/${project}/results?commit=${at}: 403 airlock REQUEST_NOT_ALLOWED`,
        `POST /api/v1/projects/${project}/artifacts: 403 dispatch RESULTS_ONLY_TOKEN`,
      ]);
    }, 120_000);

    // The airlock authenticated the run, and the result entry's person is the broker's
    // configuration: nobody authenticated that person for this write, and that person ran no
    // test (R12). The command prints the stored row into the job's log, which is public for a
    // public repository.
    test("the stored result and the command's output name no person", async () => {
      const at = commit("a");
      const ran = await writeResult(runAt(at, 107), at);

      expect(ran.code).toBe(0);
      expect(`${ran.stdout}${ran.stderr}`).not.toContain(running().person);
      expect(JSON.stringify(await running().results(at))).not.toContain(running().person);
    }, 120_000);

    // Dispatch reads the files at the run's commit as spec-check and the reporter read them
    // (specgraph.Judge), so a tree they refuse holds no check to write a result for.
    test("a result for a check of files the graph logic refuses is refused, and nothing is stored", async () => {
      for (const [at, files, checkRunId, problem] of [
        [commit("b"), unclosedFiles, 108, "broken.md:1: broken: leaf-without-check"],
        [commit("c"), lackingFiles, 109, `probe.md:5: ${PROBE_CHECK}: no-failing-example`],
      ] as const) {
        const ran = await writeResult(runAt(at, checkRunId, {}, files), at);

        expect({ code: ran.code, stored: (await running().results(at)).length }).toEqual({
          code: 1,
          stored: 0,
        });
        refused(ran, "SPEC_FILES_REFUSED");
        expect(ran.stdout).toContain(problem);
      }
    }, 120_000);

    // A failing test's log over 1 MiB is ordinary. Dispatch keeps SPEC_CHECK_OBSERVED_MAX
    // characters of it, so the command sends those: the start, which names the test, the note
    // that says the middle was cut, and the end, which says why the test failed.
    test("an observed text of 1 MiB is stored as its start and its end, cut to 2000 runes", async () => {
      const at = commit("d");
      const big = join(observedFiles, "observed-1mib.txt");
      const first = "=== RUN   TestProbe\n";
      const last = "--- FAIL: TestProbe (0.01s)\n";
      writeFileSync(big, `${first}${"x".repeat(1024 * 1024)}${last}`);
      const ran = await writeResult(runAt(at, 110), at, PROBE_CHECK, ["--observed-file", big]);

      expect(ran.code).toBe(0);
      const [row] = await running().results(at);
      const observed = row?.attestation.predicate.observed ?? "";
      expect(observed).toBe(
        `${first}${"x".repeat(SPEC_CHECK_OBSERVED_START - first.length)}${SPEC_CHECK_OBSERVED_CUT_NOTE}` +
          `${"x".repeat(SPEC_CHECK_OBSERVED_END - last.length)}${last}`
      );
      expect(observed.length).toBe(SPEC_CHECK_OBSERVED_MAX);
    }, 120_000);
  }
);
