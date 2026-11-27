import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { connect } from "node:net";
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
  DISPATCH_HOST,
  type JobRun,
  type LogLines,
  OTHER_JOBS_CHECK,
  PROBE_CHECK,
  startCIResultsStack,
  useCIResultsStackDirectory,
  writeProbeSpec,
} from "../../test/ci-results-stack";
import { dispatchStackEnabled } from "../../test/dispatch-stack";

// The v-recorder path over its whole length: a CI job's own GitHub token, traded by the real
// `agent-secrets result-grant` at the real secrets broker (./cmd/cipherd) under a real result
// entry for the proxy placeholder of a result grant, which the real `dispatch` CLI of this
// checkout, run as its child process, presents through the broker's airlock proxy to the real
// Dispatch server (./cmd/dispatch); the proxy puts the run's results token, which the broker
// minted bound to its own key, in its place. Dispatch reads the job's check run and the spec files
// at the run's commit from a stand-in of GitHub. The stack runs two brokers on one database
// behind one front for their API ports and one for their proxy ports, as two broker pods stand.
// The Go tests of each part prove its own rules; only this file proves that the job holds the
// placeholder alone, that the token the broker mints is the token Dispatch serves, and that the
// command a CI job runs writes the result Dispatch keeps, through the airlock and no other way.

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
// `describe` below has stopped every process writing into it, and hands the two stack directories
// to a removal it does not wait for (`useCIResultsStackDirectory`).
const stackDirectory = useCIResultsStackDirectory();
const cliState = temporaryDirectory("ci-results-cli-state");
const specFiles = temporaryDirectory("ci-results-spec");
/** The probe spec beside a file whose block never closes, and the probe spec with its check's
 *  failing example left out: two trees the graph logic refuses (specgraph.Judge). */
const unclosedFiles = temporaryDirectory("ci-results-unclosed-spec");
const lackingFiles = temporaryDirectory("ci-results-lacking-spec");
/** The observed texts the rows below read from files, as a job's step writes its log. */
const observedFiles = temporaryDirectory("ci-results-observed");
/** The stack of row A4, whose result grants last three seconds, and the directory its gated step
 *  and the test hand each other files in. */
const shortGrantDirectory = useCIResultsStackDirectory();
const gates = temporaryDirectory("ci-results-gates");

/** A job's step that runs the write its arguments give four times at once, each with its own
 *  observed text, all under the one grant of the `agent-secrets result-grant` it runs in, and
 *  fails when any of the four does. */
const FOUR_AT_ONCE = `pids=""
for i in 1 2 3 4; do "$@" --observed "write $i of four at once" & pids="$pids $!"; done
status=0
for pid in $pids; do wait "$pid" || status=1; done
exit "$status"`;
/** A job's step that runs the write its arguments give four times, one after another, each a new
 *  process and so a new connection to the proxy front, under one grant. */
const FOUR_IN_TURN = `for i in 1 2 3 4; do "$@" --observed "write $i of one grant" || exit 1; done`;
/** A job's step that writes once, records that write's exit status in <gate>/first.exit, waits
 *  for <gate>/go, and writes again under the same grant. */
const GATED = `gate="$1"; shift
"$@" --observed "before the grant ended"
echo "$?" > "$gate/first.exit"
while [ ! -e "$gate/go" ]; do sleep 0.05; done
exec "$@" --observed "after the grant ended"`;

/** What one run of the CLI printed and how it ended. */
interface Ran {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** The prefix of every result grant's proxy placeholder (placeholder.MintedPrefix("result")). */
const RESULT_PLACEHOLDER_PREFIX = "agent-secrets-placeholder-result-";
/** The line the proxy logs for each call to a bound host it carried (proxy's callMessage). */
const CALL_LINE = "airlock proxy: a call to a bound host";
/** The line every refusal of either port of a broker is logged as (answer.RefusalMessage). */
const REFUSAL_LINE = "broker: answer refused";
/** The proxy password's user name, which the proxy does not read (cmd/cipher's airlockProxyUser). */
const PROXY_USER = "agent-secrets";

/** `argv` run with `env` and nothing of this process's own but PATH, with the CLI's state in a
 *  directory of the file's own. */
async function exec(argv: readonly string[], env: Readonly<Record<string, string>>): Promise<Ran> {
  const child = Bun.spawn([...argv], {
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

/** The argv of the real CLI's `dispatch result --project` for `check` at `at`, with the observed
 *  text `observed` gives, as a job's step runs it. */
function resultArgv(
  project: string,
  at: string,
  check: string = PROBE_CHECK,
  observed: readonly string[] = ["--observed", "ok  \texample.com/acme/probe\t0.01s"]
): string[] {
  return [
    process.execPath,
    ENTRY,
    "result",
    "--project",
    project,
    "--check",
    check,
    "--verdict",
    "pass",
    "--commit",
    at,
    ...observed,
  ];
}

/** `read`'s value once `done` holds of it, or its last value when `withinMs` has passed: a log
 *  line is written as its call ends, which can be after the command that made the call exits.
 *  The brokers and Dispatch are real processes writing their own logs, so there is no event of
 *  theirs to await and no clock of theirs to drive: the poll reads the log every 100 ms. */
async function eventually<T>(
  read: () => T | Promise<T>,
  done: (value: T) => boolean,
  withinMs = 10_000
): Promise<T> {
  const until = Date.now() + withinMs;
  for (;;) {
    const value = await read();
    if (done(value) || Date.now() >= until) return value;
    await Bun.sleep(100);
  }
}

/** Each broker's proxy call lines among `lines` for the check run `checkRunId`, in broker order. */
function callLines(lines: LogLines, checkRunId: number): Record<string, unknown>[][] {
  return lines.brokers.map((records) =>
    records.filter(
      (record) => record.message === CALL_LINE && record.ci_check_run_id === String(checkRunId)
    )
  );
}

/** The proxy's answer to one CONNECT to `target` through `address`, the proxy front, with
 *  `password` as the proxy password: its status line and its body. It waits up to `withinMs`, as
 *  long as a proxy that dials an address nothing answers takes to give up. */
function rawConnect(
  address: string,
  target: string,
  password: string,
  withinMs = 60_000
): Promise<{ readonly status: string; readonly text: string }> {
  const [host = "", port = ""] = address.split(":");
  const { promise, resolve, reject } = Promise.withResolvers<{ status: string; text: string }>();
  const socket = connect({ host, port: Number(port) });
  const chunks: Buffer[] = [];
  const answer = (): { status: string; text: string } => {
    const text = Buffer.concat(chunks).toString("utf8");
    return { status: text.split("\r\n", 1)[0] ?? "", text };
  };
  const timer = setTimeout(() => {
    socket.destroy();
    reject(new Error(`no answer to CONNECT ${target} within ${withinMs} ms: ${answer().text}`));
  }, withinMs);
  socket.on("connect", () => {
    const credential = Buffer.from(`${PROXY_USER}:${password}`).toString("base64");
    socket.write(
      `CONNECT ${target} HTTP/1.1\r\nHost: ${target}\r\nProxy-Authorization: Basic ${credential}\r\n\r\n`
    );
  });
  socket.on("data", (chunk: Buffer) => {
    chunks.push(chunk);
    const text = Buffer.concat(chunks).toString("utf8");
    const head = text.indexOf("\r\n\r\n");
    if (head < 0) return;
    const length = Number(/\r\ncontent-length:\s*(\d+)/i.exec(text.slice(0, head))?.[1] ?? "0");
    if (Buffer.byteLength(text.slice(head + 4)) >= length) {
      clearTimeout(timer);
      socket.destroy();
      resolve(answer());
    }
  });
  socket.on("error", (error) => {
    clearTimeout(timer);
    reject(error);
  });
  socket.on("close", () => {
    clearTimeout(timer);
    resolve(answer());
  });
  return promise;
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

    /** The real CLI entry as a session runs it, with `env` and nothing of this process's own but
     *  PATH. */
    function cli(args: readonly string[], env: Readonly<Record<string, string>>): Promise<Ran> {
      return exec([process.execPath, ENTRY, ...args], env);
    }

    /** `dispatch result --project` for `check`, at `at`, as the job `run` describes, under
     *  `agent-secrets result-grant`, with the observed text `observed` gives. */
    function writeResult(
      run: JobRun,
      at: string,
      check: string = PROBE_CHECK,
      observed?: readonly string[]
    ): Promise<Ran> {
      return exec(
        running().jobCommand(resultArgv(running().project, at, check, observed)),
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

      if (ran.code !== 0) throw new Error(`the real result command failed: ${JSON.stringify(ran)}`);
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

      // A separate real child receives only the proxy placeholder and uses it on four other
      // routes. The rule allows POST under /api/v1/projects/ and under /api/v1/issues/ alone, so
      // the proxy refuses the first three itself, before it dials, and says so
      // (Cipher-Proxy-Forwarded: false and its Cipher-Proxy-Code): a POST to /api/v1/issues is
      // under neither prefix. The fourth is under the first prefix, so the proxy carries it with
      // the run's token and a proof of the broker's key, and Dispatch's own gate refuses the scope.
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
 const forwarded=response.headers.get("Cipher-Proxy-Forwarded");
 let code=response.headers.get("Cipher-Proxy-Code");
 if (forwarded!=="false") { if (!text.startsWith("{")) throw new Error("scope response: "+response.status+" "+text); code=JSON.parse(text).code; }
 answers.push(method+" "+route+": "+response.status+" "+(forwarded==="false"?"airlock":"dispatch")+" "+code);
}
console.log(JSON.stringify(answers));
`;
      const {
        code,
        stdout: output,
        stderr: errors,
      } = await exec(running().jobCommand([process.execPath, "-e", probe]), env);
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

    /** The placeholder of a new grant of the job whose environment `env` is, as the command it
     *  runs reads it: also the grant's proxy password. */
    async function placeholderOf(env: Readonly<Record<string, string>>): Promise<string> {
      const printed = await exec(running().jobCommand(["printenv", "DISPATCH_RESULTS_TOKEN"]), env);
      if (printed.code !== 0) throw new Error(`no placeholder: ${JSON.stringify(printed)}`);
      return printed.stdout.trim();
    }

    // A1. The job's write is stored through the airlock while the job holds the placeholder
    // alone: no value of its environment is a token the brokers serve, every token the brokers
    // minted for its run is bound to one of their own keys, and one broker's proxy logged the
    // write.
    test("A1: a CI job's pass is stored through the airlock while the job holds the placeholder alone", async () => {
      const at = "a1".repeat(20);
      const checkRunId = 201;
      const env = running().jobEnv(runAt(at, checkRunId));

      const printed = await exec(running().jobCommand(["env"]), env);
      expect({ code: printed.code, stderr: printed.stderr }).toEqual({ code: 0, stderr: "" });
      const environment = new Map<string, string>();
      for (const line of printed.stdout.split("\n")) {
        const split = line.indexOf("=");
        if (split > 0) environment.set(line.slice(0, split), line.slice(split + 1));
      }
      const placeholder = environment.get("DISPATCH_RESULTS_TOKEN") ?? "";
      const live: string[] = [];
      for (const [name, value] of environment) {
        if (value !== "" && (await running().introspect(value)).active) live.push(name);
      }
      expect({
        placeholder: placeholder.startsWith(RESULT_PLACEHOLDER_PREFIX),
        values: environment.size > 0,
        live,
      }).toEqual({ placeholder: true, values: true, live: [] });

      const mark = running().logMark();
      const ran = await exec(running().jobCommand(resultArgv(running().project, at)), env);
      const calls = await eventually(
        () => callLines(running().linesSince(mark), checkRunId),
        (lines) => lines.flat().length > 0
      );
      const bindings = await running().resultTokenBindings(checkRunId);
      const rows = await running().results(at);
      expect({
        code: ran.code,
        said: ran.code === 0 ? "" : `${ran.stdout}${ran.stderr}`,
        stored: rows.map((row) => row.attestation.predicate.run.check_run_id),
        tokens: {
          minted: bindings.length > 0,
          outsideTheBrokers: bindings.filter((jkt) => !running().thumbprints.includes(jkt)),
        },
        brokersThatLogged: calls.filter((lines) => lines.length > 0).length,
        calls: calls.flat().map((line) => ({
          method: line.method,
          path: line.path,
          status: line.status,
          grant: /^result:[0-9a-f-]{36}$/.test(String(line.grant)),
          run: line.ci_check_run_id,
        })),
      }).toEqual({
        code: 0,
        said: "",
        stored: [String(checkRunId)],
        tokens: { minted: true, outsideTheBrokers: [] },
        brokersThatLogged: 1,
        calls: [
          {
            method: "POST",
            path: `/api/v1/projects/${running().project}/results`,
            status: 201,
            grant: true,
            run: String(checkRunId),
          },
        ],
      });
    }, 180_000);

    // A2. No resolver answers Dispatch's public name, so a write that goes around the proxy opens
    // no connection; and the placeholder itself, sent to Dispatch's own port, is no credential.
    test("A2: a write around the proxy cannot authenticate", async () => {
      const at = "a2".repeat(20);
      const checkRunId = 202;
      const env = running().jobEnv(runAt(at, checkRunId));

      const mark = running().logMark();
      const around = await exec(
        running().jobCommand([
          "env",
          "NO_PROXY=*",
          "no_proxy=*",
          ...resultArgv(running().project, at),
        ]),
        env
      );
      const said = `${around.stdout}${around.stderr}`;
      const calls = await eventually(
        () => callLines(running().linesSince(mark), checkRunId),
        (lines) => lines.flat().length > 0,
        2_000
      );
      expect({
        code: around.code,
        // The command's words for a connection that never opened: Dispatch's public URL, which
        // only the proxy can reach, and the resolver's answer to its name.
        neverOpened: said.includes(
          `Dispatch at https://${DISPATCH_HOST} is unreachable: getaddrinfo ENOTFOUND`
        )
          ? ""
          : said,
        unknown: said.includes("write_outcome_unknown"),
        stored: (await running().results(at)).length,
        calls: calls.flat().length,
      }).toEqual({ code: 1, neverOpened: "", unknown: false, stored: 0, calls: 0 });

      const placeholder = await placeholderOf(env);
      const url = `${running().dispatch.url}/api/v1/projects/${running().project}/results`;
      const key = await newDpopKey();
      const response = await fetch(url, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          Authorization: `DPoP ${placeholder}`,
          DPoP: await key.proof({ method: "POST", url, accessToken: placeholder }),
        },
        body: JSON.stringify({ commit: at, check: PROBE_CHECK, verdict: "pass", observed: "" }),
      });
      const body = (await response.json()) as { code?: string };
      expect({
        placeholder: placeholder.startsWith(RESULT_PLACEHOLDER_PREFIX),
        status: response.status,
        code: body.code,
        stored: (await running().results(at)).length,
      }).toEqual({ placeholder: true, status: 401, code: "BROKER_TOKEN_NOT_ACTIVE", stored: 0 });
    }, 120_000);

    // A3. Two runs of the workflow share its session, and each grant is still its own run's alone.
    test("A3: two runs cannot use each other's capability", async () => {
      const x = "a3".repeat(20);
      const y = "b3".repeat(20);
      const first = running().jobEnv(runAt(x, 11));
      const second = running().jobEnv(runAt(y, 12));

      refused(
        await exec(running().jobCommand(resultArgv(running().project, y)), first),
        "COMMIT_NOT_THE_RUNS"
      );
      expect(await running().results(y)).toEqual([]);

      const fourAtOnce = (env: Record<string, string>, at: string): Promise<Ran> =>
        exec(
          running().jobCommand([
            "sh",
            "-c",
            FOUR_AT_ONCE,
            "sh",
            ...resultArgv(running().project, at, PROBE_CHECK, []),
          ]),
          env
        );
      const [one, two] = await Promise.all([fourAtOnce(first, x), fourAtOnce(second, y)]);
      const runsAt = async (at: string): Promise<string[]> =>
        (await running().results(at)).map((row) => row.attestation.predicate.run.check_run_id);
      expect({
        one: one.code === 0 ? "" : `${one.stdout}${one.stderr}`,
        two: two.code === 0 ? "" : `${two.stdout}${two.stderr}`,
        x: await runsAt(x),
        y: await runsAt(y),
      }).toEqual({ one: "", two: "", x: ["11", "11", "11", "11"], y: ["12", "12", "12", "12"] });
    }, 180_000);

    // A5. The private destination is an exception for the one bound host: with the grant's proxy
    // password, a tunnel to any other address is refused before anything is dialled. A minted
    // grant's own rule answers first (MINTED_GRANT_HOST_ONLY): it carries the write to the host
    // it binds and nowhere else, so it never reaches the address rules, the cluster ranges among
    // them.
    test("A5: another private address stays refused", async () => {
      const env = running().jobEnv(runAt("a5".repeat(20), 205));
      const password = await placeholderOf(env);
      const answers: Record<string, string | boolean>[] = [];
      for (const target of ["10.11.12.13:443", "198.18.0.10:443"]) {
        const { status, text } = await rawConnect(running().proxyAddress, target, password);
        answers.push({
          target,
          status,
          forwarded: /\r\ncipher-proxy-forwarded:\s*(\S+)/i.exec(text)?.[1] ?? "",
          code: /\r\ncipher-proxy-code:\s*(\S+)/i.exec(text)?.[1] ?? "",
          // The address rule's refusal names the setting; the one-host rule answers before it.
          namesTheRanges: text.includes("BROKER_AIRLOCK_CLUSTER_RANGES"),
        });
      }
      expect({ password: password.startsWith(RESULT_PLACEHOLDER_PREFIX), answers }).toEqual({
        password: true,
        answers: ["10.11.12.13:443", "198.18.0.10:443"].map((target) => ({
          target,
          status: "HTTP/1.1 403 Forbidden",
          forwarded: "false",
          code: "MINTED_GRANT_HOST_ONLY",
          namesTheRanges: false,
        })),
      });
    }, 180_000);

    // A6. One grant's writes reach both brokers, as a deployment's pods take a job's connections
    // in turn, and each pod serves the grant from the database.
    test("A6: two brokers serve one grant", async () => {
      const at = "a6".repeat(20);
      const checkRunId = 206;
      const env = running().jobEnv(runAt(at, checkRunId));

      const mark = running().logMark();
      const ran = await exec(
        running().jobCommand([
          "sh",
          "-c",
          FOUR_IN_TURN,
          "sh",
          ...resultArgv(running().project, at, PROBE_CHECK, []),
        ]),
        env
      );
      const calls = await eventually(
        () => callLines(running().linesSince(mark), checkRunId),
        (lines) => lines.flat().length >= 4
      );
      const rows = await running().results(at);
      expect({
        said: ran.code === 0 ? "" : `${ran.stdout}${ran.stderr}`,
        brokersThatLogged: calls.map((lines) => lines.length > 0),
        grants: new Set(calls.flat().map((line) => String(line.grant))).size,
        observed: rows.map((row) => row.attestation.predicate.observed).sort(),
      }).toEqual({
        said: "",
        brokersThatLogged: [true, true],
        grants: 1,
        observed: [1, 2, 3, 4].map((i) => `write ${i} of one grant`),
      });
    }, 180_000);

    // A7. The proxy drops the command's forwarding headers, so Dispatch's access log names the
    // broker's address; and a request to Dispatch's host without the placeholder never leaves.
    test("A7: Dispatch sees the broker's address and not the command's", async () => {
      const at = "a7".repeat(20);
      const project = running().project;
      const env = running().jobEnv(runAt(at, 207));
      const resultsPath = `/api/v1/projects/${project}/results`;
      // A Bun fetch of the job's own, through the proxy its environment names, with the
      // placeholder as its Authorization or with none.
      const probe = (
        withPlaceholder: boolean
      ): string => `const response = await fetch(process.env.DISPATCH_URL + ${JSON.stringify(resultsPath)}, {
 method: "POST",
 headers: {
  "Content-Type": "application/json",
  "Idempotency-Key": crypto.randomUUID(),
  "X-Forwarded-For": "203.0.113.9",
  ${withPlaceholder ? 'Authorization: "DPoP " + process.env.DISPATCH_RESULTS_TOKEN,' : ""}
 },
 body: ${JSON.stringify(JSON.stringify({ commit: at, check: PROBE_CHECK, verdict: "pass", observed: "A7" }))},
});
await response.text();
console.log(JSON.stringify({ status: response.status, forwarded: response.headers.get("Cipher-Proxy-Forwarded"), code: response.headers.get("Cipher-Proxy-Code") }));
`;
      const send = async (
        withPlaceholder: boolean
      ): Promise<{ answer: unknown; access: string[] }> => {
        const mark = running().logMark();
        const ran = await exec(
          running().jobCommand([process.execPath, "-e", probe(withPlaceholder)]),
          env
        );
        if (ran.code !== 0) throw new Error(`the job's fetch failed: ${JSON.stringify(ran)}`);
        const access = await eventually(
          () =>
            running()
              .linesSince(mark)
              .access.filter(
                (line) => line.includes(" method=POST ") && line.includes(` path=${resultsPath} `)
              ),
          (lines) => lines.length > 0,
          withPlaceholder ? 10_000 : 2_000
        );
        return { answer: JSON.parse(ran.stdout), access };
      };

      const stored = await send(true);
      expect({
        answer: stored.answer,
        stored: (await running().results(at)).length,
        addresses: stored.access.map((line) => / address=(\S+)/.exec(line)?.[1] ?? line),
      }).toEqual({
        answer: { status: 201, forwarded: "true", code: null },
        stored: 1,
        addresses: ["127.0.0.1"],
      });

      const refusedHere = await send(false);
      expect(refusedHere).toEqual({
        answer: { status: 403, forwarded: "false", code: "NO_PLACEHOLDER" },
        access: [],
      });
      expect((await running().results(at)).length).toBe(1);
    }, 120_000);
  }
);

// A4. A result grant ends with the time the broker gave it, and the proxy carries none of its
// writes after that: the next write is refused at the CONNECT, by the broker that took it, and
// Dispatch hears nothing of it. A stack of its own, whose grants last three seconds.
describe.skipIf(!dispatchStackEnabled)("a result grant that ended ends the proxy's use", () => {
  let stack: CIResultsStack | undefined;

  beforeAll(async () => {
    writeProbeSpec(specFiles);
    stack = await startCIResultsStack({ work: shortGrantDirectory, grantSeconds: 3 });
  }, 600_000);
  afterAll(async () => {
    await stack?.stop();
  }, 60_000);

  test("A4: an ended grant ends the proxy's use", async () => {
    if (stack === undefined) throw new Error("the CI results stack did not start");
    const live = stack;
    const at = "a4".repeat(20);
    live.setFiles(at, specFiles);
    const env = live.jobEnv({ sha: at, ref: REF, job: JOB, runAttempt: 1, checkRunId: 204 });
    const gate = join(gates, "a4");
    mkdirSync(gate);
    const firstExit = join(gate, "first.exit");

    const step = exec(
      live.jobCommand([
        "sh",
        "-c",
        GATED,
        "sh",
        gate,
        ...resultArgv(live.project, at, PROBE_CHECK, []),
      ]),
      env
    );
    const first = await eventually(
      () => (existsSync(firstExit) ? readFileSync(firstExit, "utf8").trim() : ""),
      (status) => status !== "",
      60_000
    );
    const storedFirst = (await live.results(at)).length;
    // The grant's three seconds run from the exchange, before the first write: four seconds after
    // that write ended, the grant has ended on the brokers' clock. Real time is the thing under
    // test here, so the wait is real.
    await Bun.sleep(4_000);
    const mark = live.logMark();
    writeFileSync(join(gate, "go"), "");
    const ran = await step;
    const said = `${ran.stdout}${ran.stderr}`;
    const refusals = await eventually(
      () =>
        live
          .linesSince(mark)
          .brokers.flat()
          .filter((record) => record.message === REFUSAL_LINE),
      (lines) => lines.length > 0,
      2_000
    );
    // Read before the row's own read of the results below, which is a request Dispatch logs.
    const access = live.linesSince(mark).access;
    expect({
      first,
      storedFirst,
      code: ran.code,
      tunnelRefused: said.includes("AIRLOCK_TUNNEL_REFUSED"),
      status: said.includes("407 Proxy Authentication Required"),
      nothingSent: said.includes("nothing was sent to Dispatch"),
      stored: (await live.results(at)).length,
      refusals: refusals.map((record) => ({
        status: record.status,
        code: record.code,
        method: record.method,
        caller_class: record.caller_class,
        host: record.host,
        caller: record.caller,
      })),
      access,
    }).toEqual({
      first: "0",
      storedFirst: 1,
      code: 1,
      tunnelRefused: true,
      status: true,
      nothingSent: true,
      stored: 1,
      refusals: [
        {
          status: 407,
          code: "GRANT_NOT_LIVE",
          method: "CONNECT",
          caller_class: "airlock",
          host: DISPATCH_HOST,
          caller: "",
        },
      ],
      access: [],
    });
  }, 180_000);
});
