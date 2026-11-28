// The CI job's own `dispatch result --project` (`jobResultCommand`,
// dispatch-commands/job-result.ts), run through the `dispatch` command as a job's step runs it.

import { afterAll, beforeEach, describe, expect, jest, test } from "bun:test";
import { readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import {
  SPEC_CHECK_OBSERVED_CUT_NOTE,
  SPEC_CHECK_OBSERVED_END,
  SPEC_CHECK_OBSERVED_MAX,
  SPEC_CHECK_OBSERVED_START,
} from "@legion/contracts";
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import type { Socket } from "bun";
import { keyedLayer } from "../../test/keyed-layer";
import { routeAnswer } from "../../test/route-answers";
import { SPEC_COMMIT, storedSpecResult } from "../../test/spec-check-result";
import { type StubProxy, startStubProxy } from "../../test/stub-proxy";
import { runDispatchCli } from "../dispatch-cli";
import { DISPATCH_TOKEN_CHECK_WAIT_MS, type ResultBound } from "../dispatch-commands/job-result";
import { DISPATCH_WRITE_RETRY } from "../dispatch-http";

const makeDirectory = temporaryDirectories("dispatch-job-result");

/** The row's folder: the job's HOME and the command's state directory. */
let stateDir = "";
beforeEach(() => {
  stateDir = makeDirectory();
});

// `dispatch result --project` is a CI job's own write, and the job has no session: it runs under
// `agent-secrets result-grant`, which gives it the proxy placeholder of its run as
// DISPATCH_RESULTS_TOKEN, and the airlock proxy puts the run's results token in its place. The job
// trades nothing. The stand-ins below are the runner's token endpoint and the broker's token
// endpoint, which the job must ask nothing, and Dispatch's two project results routes.
describe("dispatch result --project, as a CI job writes it", () => {
  const entry = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
  /** The proxy placeholder of the job's run, the run's OIDC token, the bearer the runner's
   *  endpoint takes, and a results token: none of them may reach the command's output or its
   *  argument list. */
  const PLACEHOLDER = "agent-secrets-placeholder-result-Run4242abcdefghijklmnopqrstu";
  const OIDC_TOKEN = fixtureSecret("oidc");
  const REQUEST_TOKEN = fixtureSecret("runner");
  const RESULTS_TOKEN = fixtureSecret("results");
  const SECRETS = [PLACEHOLDER, OIDC_TOKEN, REQUEST_TOKEN, RESULTS_TOKEN];

  interface Seen {
    readonly method: string;
    readonly path: string;
    readonly authorization: string | null;
    readonly dpop: string | null;
    readonly body: unknown;
  }
  const runnerAsked: { authorization: string | null; audience: string | null }[] = [];
  const trades: { form: URLSearchParams; proof: string }[] = [];
  const written: Seen[] = [];
  let brokerAnswer: () => Response | Promise<Response>;
  let dispatchAnswer: () => Response | Promise<Response>;
  /** Whether the stand-in answers the write naming no key back, as something in Dispatch's place
   *  does: Dispatch's own answer to a keyed write always names it. */
  let unkeyed = false;
  /** The result Dispatch stores for the job's write, as the route answers it. */
  const result = storedSpecResult(43, "pass", "", 1);
  const stored = routeAnswer<typeof result>("POST /api/v1/projects/{key}/results", result, 201);
  /** The answer of the write in Dispatch's own form, `body`, the stored result by default: the
   *  keyed layer the stand-in Dispatch answers behind names the write's key back on it. */
  const storedAnswer = (body: unknown = stored) => Response.json(body, { status: 201 });

  const runner = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      runnerAsked.push({
        authorization: request.headers.get("Authorization"),
        audience: new URL(request.url).searchParams.get("audience"),
      });
      asked();
      return Response.json({ count: 1, value: OIDC_TOKEN });
    },
  });
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      trades.push({
        form: new URLSearchParams(await request.text()),
        proof: request.headers.get("DPoP") ?? "",
      });
      asked();
      return brokerAnswer();
    },
  });
  // Behind Dispatch's keyed layer, which names the write's key back on each 2xx and each 409 (its
  // KEY_COMMITTED, and a handler's refusal), unless the row says it answers as none does.
  const dispatch = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: keyedLayer(
      async (request: Request) => {
        const text = await request.text();
        written.push({
          method: request.method,
          path: new URL(request.url).pathname,
          authorization: request.headers.get("Authorization"),
          dpop: request.headers.get("DPoP"),
          body: text === "" ? undefined : JSON.parse(text),
        });
        asked();
        return dispatchAnswer();
      },
      { judged: (response) => response.ok || response.status === 409, namesNoKey: () => unkeyed }
    ),
  });
  afterAll(() => {
    for (const stub of [runner, broker, dispatch]) stub.stop(true);
  });
  // A row on the fake clock that never ended left its timers faked: the next row starts real.
  const realSetTimeout = globalThis.setTimeout;
  beforeEach(() => {
    globalThis.setTimeout = realSetTimeout;
    jest.useRealTimers();
    runnerAsked.length = 0;
    trades.length = 0;
    written.length = 0;
    brokerAnswer = () =>
      Response.json(
        Object.fromEntries([
          ["access_token", RESULTS_TOKEN],
          ["issued_token_type", "urn:ietf:params:oauth:token-type:access_token"],
          ["token_type", "DPoP"],
          ["expires_in", 600],
        ])
      );
    dispatchAnswer = () => storedAnswer();
    unkeyed = false;
  });

  const dispatchUrl = `http://127.0.0.1:${dispatch.port}`;
  /** A job's environment as `agent-secrets result-grant` gives its command: no DISPATCH_HOST and
   *  no session, Dispatch's URL and the proxy placeholder, and the broker's and the runner's
   *  variables of the step it runs in, of which the command reads none. */
  const jobEnvironment = (): Record<string, string | undefined> => ({
    PATH: process.env.PATH,
    HOME: stateDir,
    DISPATCH_STATE_DIR: stateDir,
    DISPATCH_URL: dispatchUrl,
    DISPATCH_RESULTS_TOKEN: PLACEHOLDER,
    AGENT_SECRETS_URL: `http://127.0.0.1:${broker.port}`,
    AGENT_SECRETS_MACHINE_AUDIENCE: "example-audience",
    ACTIONS_ID_TOKEN_REQUEST_URL: `http://127.0.0.1:${runner.port}/_apis/idtoken?api-version=2.0`,
    ACTIONS_ID_TOKEN_REQUEST_TOKEN: REQUEST_TOKEN,
  });
  const record = [
    "result",
    "--project",
    "ACME",
    "--check",
    "p-1",
    "--verdict",
    "pass",
    "--commit",
    SPEC_COMMIT,
  ];

  /** The command as its own process, as a job's step runs it. */
  async function spawnJob(
    argv: readonly string[],
    env: Record<string, string | undefined>
  ): Promise<{ code: number; stdout: string; stderr: string }> {
    const child = Bun.spawn([process.execPath, entry, ...argv], {
      cwd: stateDir,
      env: { ...env, ...bunCacheOff },
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

  /** The command in this process, under exactly `env`, with its one bound `bound` where a test
   *  shortens it, and the write client's `fetchImpl` where a test watches its answers. */
  async function runJob(
    argv: readonly string[],
    env: Record<string, string | undefined>,
    bound?: ResultBound,
    fetchImpl?: typeof fetch
  ): Promise<{ code: number; stdout: string; stderr: string }> {
    let stdout = "";
    let stderr = "";
    const code = await runDispatchCli(argv, env, {
      stdout: (text) => {
        stdout += text;
      },
      stderr: (text) => {
        stderr += text;
      },
      readText: (path) => readFileSync(path, "utf-8"),
      cwd: stateDir,
      ...(bound === undefined ? {} : { resultBound: bound }),
      ...(fetchImpl === undefined ? {} : { fetchImpl }),
    });
    return { code, stdout, stderr };
  }

  /** Each row's wait on what the stand-ins were asked (`arrived`); every stand-in calls `asked`
   *  once it has recorded a request, and so does the write client's fetch once an answer's
   *  headers came (`answersHeard`). */
  const watches = new Set<{ readonly met: () => boolean; readonly resolve: () => void }>();
  let answersHeard = 0;
  function asked(): void {
    for (const watch of watches) {
      if (!watch.met()) continue;
      watches.delete(watch);
      watch.resolve();
    }
  }
  /** Settles once `met` holds of what the stand-ins were asked. */
  function arrived(met: () => boolean): Promise<void> {
    if (met()) return Promise.resolve();
    const { promise, resolve } = Promise.withResolvers<void>();
    watches.add({ met, resolve });
    return promise;
  }

  /** Lets the event loop run `count` turns, moving no clock. */
  async function turns(count: number): Promise<void> {
    for (let turn = 0; turn < count; turn++) {
      const { promise, resolve } = Promise.withResolvers<void>();
      setImmediate(resolve);
      await promise;
    }
  }

  /** The fake clock a row moves (`onFakeClock`). */
  interface FakeClock {
    /** Moves the clock `ms` on, firing each timer due by then. */
    readonly advance: (ms: number) => void;
    /** The delay of the timer the code sets `index`th (from 0) since the clock started, once it
     *  has set it: each wait the call takes (`sleep`, a write's pause, its 20 s silence cut). */
    readonly timerSet: (index: number) => Promise<number>;
    /** Settles once the call has ended. */
    readonly ended: Promise<void>;
    /** How far the clock has moved since the call started. */
    readonly elapsed: () => number;
  }

  /**
   * `runJob` on the test runner's fake timers, with how far that clock had moved when the call
   * ended (`ms`, undefined where it had not ended). The command's bound and each wait of the call
   * run on a clock that moves only when `drive` moves it, so a row judges when on that clock the
   * call ended, which the machine's load cannot change: on the real clock the 60 s bound once
   * read 246,969 ms on a loaded host. The stand-ins answer on real sockets, and the clock stands
   * still while they do, so `drive` moves it only once the call waits on it: a stand-in holds its
   * request (`arrived`), or the call has set the timer of a wait (`timerSet`). After `drive`, the
   * call has the event loop's turns to end in, and no time. `proxy`, where a row gives one, is the
   * proxy every request of the call goes through, as HTTPS_PROXY sends it in a process of its own.
   */
  async function onFakeClock(
    argv: readonly string[],
    env: Record<string, string | undefined>,
    bound: ResultBound | undefined,
    drive: (clock: FakeClock) => Promise<void> = (clock) => clock.ended,
    proxy?: string
  ): Promise<{ code: number; stdout: string; stderr: string; ms: number | undefined }> {
    jest.useFakeTimers();
    const faked = globalThis.setTimeout;
    const delays: number[] = [];
    const timerWatches = new Set<{ readonly index: number; readonly resolve: () => void }>();
    globalThis.setTimeout = ((handler: TimerHandler, delay?: number, ...rest: unknown[]) => {
      delays.push(delay ?? 0);
      for (const watch of timerWatches) {
        if (watch.index >= delays.length) continue;
        timerWatches.delete(watch);
        watch.resolve();
      }
      return faked(handler, delay, ...rest);
    }) as unknown as typeof setTimeout;
    answersHeard = 0;
    const heard = (async (input: RequestInfo | URL, init?: RequestInit) => {
      const response = await fetch(input, proxy === undefined ? init : { ...init, proxy });
      answersHeard += 1;
      asked();
      return response;
    }) as typeof fetch;
    const started = performance.now();
    const elapsed = () => performance.now() - started;
    let ms: number | undefined;
    const job = runJob(argv, env, bound, heard).finally(() => {
      ms = elapsed();
    });
    try {
      await drive({
        advance: (step) => jest.advanceTimersByTime(step),
        timerSet: async (index) => {
          if (index >= delays.length) {
            const { promise, resolve } = Promise.withResolvers<void>();
            timerWatches.add({ index, resolve });
            await promise;
          }
          return delays[index] as number;
        },
        ended: job.then(() => undefined),
        elapsed,
      });
      const ran = await Promise.race([job, turns(50).then(() => undefined)]);
      return ran === undefined
        ? { code: Number.NaN, stdout: "", stderr: "", ms: undefined }
        : { ...ran, ms };
    } finally {
      globalThis.setTimeout = faked;
      jest.useRealTimers();
    }
  }

  /** Moves the clock over each wait the call sets, as it sets it, until the call ends: a wait is
   *  every timer but the write's 20 s silence cut, which no row here reaches. */
  async function eachWait(clock: FakeClock): Promise<void> {
    for (let index = 0; ; index++) {
      const delay = await Promise.race([clock.ended.then(() => undefined), clock.timerSet(index)]);
      if (delay === undefined) return;
      if (delay !== DISPATCH_WRITE_RETRY.silenceMs) clock.advance(delay);
    }
  }

  /** Nothing went to the runner, the broker or Dispatch. */
  const sentNothing = () => {
    expect(runnerAsked).toEqual([]);
    expect(trades).toEqual([]);
    expect(written).toEqual([]);
  };

  /** No token is in what the command printed, nor in the argument list it ran with. */
  const namesNoToken = (argv: readonly string[], stdout: string, stderr: string) => {
    for (const secret of SECRETS) {
      expect(stdout, secret).not.toContain(secret);
      expect(stderr, secret).not.toContain(secret);
      expect(argv.join(" "), secret).not.toContain(secret);
    }
  };

  // The 30 s limit is the hang guard of the rows below that run the entry as a child process.
  test("a CI job's result goes out with the placeholder as its DPoP token and no proof of its own", async () => {
    // A test's output opens with `---`, so the job gives it in the `=` form, which reads no flag.
    const argv = [...record, "--observed=--- PASS: TestP (0.01s)"];
    const { code, stdout, stderr } = await spawnJob(argv, jobEnvironment());

    expect({ code, stderr }, stdout).toEqual({ code: 0, stderr: "" });
    expect(JSON.parse(stdout)).toEqual(stored);
    expect(written.map((seen) => [seen.method, seen.path, seen.authorization, seen.dpop])).toEqual([
      ["POST", "/api/v1/projects/ACME/results", `DPoP ${PLACEHOLDER}`, null],
    ]);
    expect({ runner: runnerAsked.length, broker: trades.length }).toEqual({ runner: 0, broker: 0 });
    namesNoToken(argv, stdout, stderr);
  }, 30_000);

  // The proxy block of the profile DISPATCH_RESULTS_TOKEN allows the results write to Dispatch's
  // host and nothing else, so the write asks Dispatch nothing first and nothing after: the
  // stand-in counts every request.
  test("the job's write sends one POST and nothing else to Dispatch's host", async () => {
    const { code, stdout } = await runJob(record, jobEnvironment());
    expect(code, stdout).toBe(0);
    expect(written.map((seen) => `${seen.method} ${seen.path}`)).toEqual([
      "POST /api/v1/projects/ACME/results",
    ]);
  });

  test("with no DISPATCH_RESULTS_TOKEN it refuses before any request, naming agent-secrets result-grant", async () => {
    const { DISPATCH_RESULTS_TOKEN: _placeholder, ...none } = jobEnvironment();
    for (const env of [none, { ...none, DISPATCH_RESULTS_TOKEN: "" }]) {
      const { code, stdout } = await runJob(record, env);
      expect({ code, stdout }).toEqual({
        code: 2,
        stdout:
          "dispatch: dispatch result --project runs under `agent-secrets result-grant -- …`, which gives it DISPATCH_RESULTS_TOKEN; this environment holds none, so nothing was sent\n",
      });
    }
    // A value that cannot be a header's is refused by its variable, and quoted nowhere.
    const spaced = await runJob(record, { ...none, DISPATCH_RESULTS_TOKEN: `${PLACEHOLDER} x` }); // fixture-secret: invalid placeholder refused before any client can hold it.
    expect({ code: spaced.code, stdout: spaced.stdout }).toEqual({
      code: 2,
      stdout:
        "dispatch: DISPATCH_RESULTS_TOKEN holds white space or a control character, so it is no placeholder `agent-secrets result-grant` gives, and nothing was sent\n",
    });
    sentNothing();
  });

  // A job's step environment names the broker and the identity agent-secrets traded, and the
  // command it runs inherits them: the write reads none of them, and trades nothing.
  test("AGENT_SECRETS_MACHINE_IDENTITY=github-actions asks the broker nothing", async () => {
    const { code, stdout } = await runJob(record, {
      ...jobEnvironment(),
      AGENT_SECRETS_MACHINE_IDENTITY: "github-actions",
    });
    expect(code, stdout).toBe(0);
    expect({
      broker: trades.length,
      runner: runnerAsked.length,
      sent: written.map((seen) => [seen.authorization, seen.dpop]),
    }).toEqual({ broker: 0, runner: 0, sent: [[`DPoP ${PLACEHOLDER}`, null]] });
  });

  test("a refusal names no credential either", async () => {
    const refusal = `The files of .dispatch/spec at ${SPEC_COMMIT} hold no check p-9.`;
    dispatchAnswer = () =>
      Response.json({ code: "CHECK_NOT_IN_FILES", error: refusal }, { status: 409 });
    const argv = record.map((word) => (word === "p-1" ? "p-9" : word));
    const dispatchRefused = await spawnJob(argv, jobEnvironment());
    expect(dispatchRefused.code, dispatchRefused.stdout).toBe(1);
    expect(dispatchRefused.stdout.trim()).toBe(`CHECK_NOT_IN_FILES: ${refusal}`);
    namesNoToken(argv, dispatchRefused.stdout, dispatchRefused.stderr);
  }, 30_000);

  // A job's step writes `--project "$PROJECT" --check "$CHECK" --commit "$SHA"`, and an empty
  // variable is the honest way to send one empty: it is refused before any write, naming the flag.
  test("an empty --project, --check or --commit is refused before any request, naming the flag", async () => {
    for (const [flag, noun] of [
      ["--project", "project"],
      ["--check", "check"],
      ["--commit", "commit"],
    ] as const) {
      const argv = [...record];
      argv[argv.indexOf(flag) + 1] = "";
      const { code, stdout, stderr } = await runJob(argv, jobEnvironment());
      expect({ code, stderr, lines: stdout.split("\n").slice(0, 2) }, flag).toEqual({
        code: 2,
        stderr: "",
        lines: [
          "dispatch result was not called: 1 problem",
          `- ${flag} is empty, so the call names no ${noun}: a variable that gives it may be empty or unset`,
        ],
      });
    }
    sentNothing();
  });

  /** Dispatch's `409 KEY_COMMITTED` to the write it answers now, which its keyed layer names the
   *  write's key back on, or the key `other` names where a test gives another request's. */
  const keyCommitted = (other?: string) =>
    Response.json(
      { code: "KEY_COMMITTED", error: "the write landed and its answer was lost" },
      { status: 409, headers: other === undefined ? {} : { "Idempotency-Key": other } }
    );

  // A write whose answer was lost after Dispatch stored it (409 KEY_COMMITTED) is done, and the
  // job's log is read by a program: it prints the JSON the session path records for the same
  // case and exits 0. It gives no advice, since a results token reads nothing back.
  test("a result stored with its answer lost prints landed_without_answer as JSON and exits 0", async () => {
    dispatchAnswer = () => keyCommitted();
    const { code, stdout, stderr } = await runJob(record, jobEnvironment());
    expect({ code, stdout, stderr }).toEqual({
      code: 0,
      stdout: `${JSON.stringify({ landed_without_answer: "KEY_COMMITTED" })}\n`,
      stderr: "",
    });
    expect(written.map((seen) => [seen.method, seen.path])).toEqual([
      ["POST", "/api/v1/projects/ACME/results"],
    ]);
  });

  // A landing needs the key Dispatch names back, on a 409 as on a 2xx: a KEY_COMMITTED that names
  // none, or another request's, is no verdict of Dispatch's on this write, so the result may or
  // may not be stored, and a second run is safe.
  test("a 409 KEY_COMMITTED that does not name the write's key back prints write_outcome_unknown and exits 1", async () => {
    for (const named of ["", "another-request-key"]) {
      written.length = 0;
      // One names no key back, as something in Dispatch's place answers; the other names another
      // request's.
      unkeyed = named === "";
      dispatchAnswer = () => keyCommitted(named === "" ? undefined : named);
      const { code, stdout, stderr } = await runJob(record, jobEnvironment());
      expect({ code, stdout, stderr }, named).toEqual({
        code: 1,
        stdout: `${JSON.stringify({ write_outcome_unknown: "HTTP_409" })}\n`,
        stderr: "",
      });
      expect(written.map((seen) => [seen.method, seen.path])).toEqual([
        ["POST", "/api/v1/projects/ACME/results"],
      ]);
    }
  });

  /** A proxy in front of the stand-in Dispatch that sends each request on, and closes the job's
   *  connection in place of passing on the answer of each results write that `drops` names (by
   *  its number, from 1), and of every other request when `dropOthers`: the write lands, and the
   *  job hears nothing back. */
  function lossyProxy(drops: (write: number) => boolean, dropOthers: boolean) {
    let writes = 0;
    interface Link {
      upstream: Promise<Socket<undefined>>;
      drop: boolean;
    }
    const proxy = Bun.listen<Link>({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        open(client) {
          client.data = {
            drop: false,
            upstream: Bun.connect({
              hostname: "127.0.0.1",
              port: Number(dispatch.port),
              socket: {
                data(_upstream, answer) {
                  if (client.data.drop) client.end();
                  else client.write(answer);
                },
                close() {
                  client.end();
                },
              },
            }),
          };
        },
        // A request's first chunk opens with its request line; the rest of it is its body.
        async data(client, request) {
          const line = request.subarray(0, 64).toString("latin1");
          if (/^[A-Z]+ \//.test(line)) {
            const write = line.startsWith("POST /api/v1/projects/");
            if (write) writes += 1;
            client.data.drop = write ? drops(writes) : dropOthers;
          }
          (await client.data.upstream).write(request);
        },
        close(client) {
          void client.data.upstream.then((upstream) => upstream.end());
        },
      },
    });
    return { url: `http://127.0.0.1:${proxy.port}`, stop: () => proxy.stop(true) };
  }

  // A write that went out and heard nothing back may have stored the result, here it did: the
  // proxy drops the answer of the write. The command's bound leaves no time for a pause
  // (`oneAttempt`), so the write is not asked again. The job's step still fails, since the write
  // is not confirmed, and its one line says so to a program.
  test("a write that went out and heard no answer prints write_outcome_unknown and exits 1", async () => {
    const proxy = lossyProxy(() => true, false);
    try {
      const ran = await runJob(
        record,
        { ...jobEnvironment(), DISPATCH_URL: proxy.url },
        oneAttempt
      );
      expect(ran).toEqual({
        code: 1,
        stdout: `${JSON.stringify({ write_outcome_unknown: "ECONNRESET" })}\n`,
        stderr: "",
      });
      expect(written.map((seen) => [seen.method, seen.path])).toEqual([
        ["POST", "/api/v1/projects/ACME/results"],
      ]);
    } finally {
      proxy.stop();
    }
  });

  // The control: a connection refused before any request went out reached nothing, so the write
  // stored nothing, and the command prints the transport error and no write_outcome_unknown.
  test("a write whose connection was refused prints no write_outcome_unknown", async () => {
    const closed = Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response() });
    const url = `http://127.0.0.1:${closed.port}`;
    closed.stop(true);
    const { code, stdout, stderr } = await runJob(
      record,
      { ...jobEnvironment(), DISPATCH_URL: url },
      oneAttempt
    );
    expect({ code, stderr, unknown: stdout.includes("write_outcome_unknown") }).toEqual({
      code: 1,
      stderr: "",
      unknown: false,
    });
    expect(stdout).toContain("is unreachable: ECONNREFUSED.");
    expect(written).toEqual([]);
  });

  // A landed write whose first answer the proxy drops is asked again under its key, and Dispatch
  // answers KEY_COMMITTED: the old line, byte for byte, and exit 0.
  test("a landed write whose answer is lost and asked again still prints landed_without_answer", async () => {
    const proxy = lossyProxy((write) => write === 1, false);
    dispatchAnswer = () =>
      written.filter((seen) => seen.method === "POST").length === 1
        ? storedAnswer()
        : keyCommitted();
    try {
      const ran = await runJob(record, { ...jobEnvironment(), DISPATCH_URL: proxy.url });
      expect(ran).toEqual({
        code: 0,
        stdout: `${JSON.stringify({ landed_without_answer: "KEY_COMMITTED" })}\n`,
        stderr: "",
      });
      expect(written.map((seen) => [seen.method, seen.path])).toEqual([
        ["POST", "/api/v1/projects/ACME/results"],
        ["POST", "/api/v1/projects/ACME/results"],
      ]);
    } finally {
      proxy.stop();
    }
  }, 30_000);

  /** A bound of the command short enough for a test, on the fake clock each of its rows runs on
   *  (`onFakeClock`): no round starts later than `reserveMs` before `totalMs`. */
  const shortBound = (totalMs: number, reserveMs: number): ResultBound => ({ totalMs, reserveMs });
  /** A bound whose window to ask a write again (its total less its reserve, 0.5 s) ends before the
   *  write's first pause of 1 s, so a write that gets no definitive answer makes its one attempt. */
  const oneAttempt = shortBound(60_000, 59_500);

  // Dispatch answers 201 only after it stored the result, and names the request's key back on it
  // (`TestEveryKeyedAnswerNamesItsKey`), but a body that does not arrive whole is no answer the job
  // can read (the write table, write-outcome.ts): a body cut short or broken off is asked again
  // under the key (`body-broken`, `body-malformed`), and one that stalls ends at the command's own
  // bound (B4 of the review of 2f29345b8d, its two rows). The bound here ends before the first
  // pause, so the job prints write_outcome_unknown, which a second run makes safe, and exits 1,
  // within the bound. The stand-in Dispatch is a raw socket, so the status line and the first bytes
  // of the body surely reach the job before the connection closes or goes silent.
  // A body cut short, or one that is not the JSON it says it is, ends the write by itself: no ask
  // fits in the bound's window, so nothing waits on the clock, and its row leaves the fake clock
  // (`onFakeClock`) where it is. The line then names the body's own failure, a plain `Error`. A
  // row that moved the clock once the 201's headers came would race the bound against the body:
  // the bound won on an idle machine and the cut on a loaded one, so that row tested the bound and
  // failed under load. Only the body that stalls waits for the bound, so only its row moves the
  // clock, once the 201's headers came, and its line names the bound, `TimeoutError`.
  const bodyStart = '{"id":"00000000-0000-4000-8000-';
  /** The `Idempotency-Key` a raw request carries, which a stand-in of Dispatch on a raw socket names
   *  back itself: `keyedLayer` wraps a fetch-shaped answer, and a socket gives none. */
  const keyOf = (request: Buffer): string =>
    /\r\nidempotency-key: *([^\r\n]*)/i.exec(request.toString("latin1"))?.[1]?.trim() ?? "";
  for (const [name, length, close, endedBy] of [
    ["is cut", 400, true, "Error"],
    ["stalls past the bound", 400, false, "TimeoutError"],
    ["is not the JSON it says it is", bodyStart.length, false, "Error"],
  ] as const) {
    test(`a 201 whose body ${name} prints write_outcome_unknown and exits 1`, async () => {
      let posts = 0;
      const lost = Bun.listen({
        hostname: "127.0.0.1",
        port: 0,
        socket: {
          data(socket, request) {
            if (!request.subarray(0, 5).toString("latin1").startsWith("POST ")) return;
            posts += 1;
            socket.write(
              "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\n" +
                `Idempotency-Key: ${keyOf(request)}\r\n` +
                `Content-Length: ${length}\r\n\r\n${bodyStart}`
            );
            if (close) socket.end();
          },
        },
      });
      try {
        const ran = await onFakeClock(
          record,
          { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${lost.port}` },
          shortBound(1_000, 200),
          endedBy === "TimeoutError"
            ? async (clock) => {
                await Promise.race([clock.ended, arrived(() => answersHeard >= 1)]);
                clock.advance(1_000);
                await clock.ended;
              }
            : (clock) => clock.ended
        );
        expect({
          code: ran.code,
          stdout: ran.stdout,
          stderr: ran.stderr,
          posts,
          withinTheBound: ran.ms !== undefined && ran.ms <= 1_000,
          clockStill: ran.ms === 0,
        }).toEqual({
          code: 1,
          stdout: `${JSON.stringify({ write_outcome_unknown: endedBy })}\n`,
          stderr: "",
          posts: 1,
          withinTheBound: true,
          clockStill: endedBy === "Error",
        });
      } finally {
        lost.stop(true);
      }
    }, 30_000);
  }

  /** A stand-in Dispatch on a raw socket that answers every POST with `status`, `type` and the
   *  whole of `body`, naming back the request's key itself (`keyOf`, since no fetch-shaped layer
   *  wraps a socket) as Dispatch does (`this`), none, or another request's (`other`), counting the
   *  posts. */
  function answeringWhole(
    status: string,
    type: string,
    body: string,
    echo: "this" | "none" | "other" = "this"
  ) {
    const seen = { posts: 0 };
    const server = Bun.listen({
      hostname: "127.0.0.1",
      port: 0,
      socket: {
        data(socket, request) {
          if (!request.subarray(0, 5).toString("latin1").startsWith("POST ")) return;
          seen.posts += 1;
          const key = keyOf(request);
          const named =
            echo === "none"
              ? ""
              : `Idempotency-Key: ${echo === "this" ? key : `${key}-another`}\r\n`;
          socket.write(
            `HTTP/1.1 ${status}\r\nContent-Type: ${type}\r\n${named}` +
              `Content-Length: ${Buffer.byteLength(body)}\r\n\r\n${body}`
          );
          socket.end();
        },
      },
    });
    return { seen, server };
  }

  // A 201 whose body comes whole and still gives no stored result: JSON of another shape (D-1 of
  // the acceptance of 8ae6f5bcb4) and JSON null are a landing, since the client reads a write's 2xx
  // into the row its callers read (`dispatch-write-rows.ts`), and an answer that is not that row is
  // ANSWER_LOST, which the reporter reads as stored. It printed `Cannot destructure property
  // 'predicate' …` and exited 1. A body that is no JSON under another type, or empty, is no answer
  // Dispatch writes (`foreign-2xx`, write-outcome.ts), so its outcome is unknown.
  for (const [name, type, body, printed, code] of [
    [
      "is no JSON, typed text/html",
      "text/html",
      "<html><body>not json</body></html>",
      { write_outcome_unknown: "Error" },
      1,
    ],
    ["is empty, typed JSON", "application/json", "", { write_outcome_unknown: "Error" }, 1],
    ["is the JSON {}", "application/json", "{}", { landed_without_answer: "ANSWER_LOST" }, 0],
    ["is JSON null", "application/json", "null", { landed_without_answer: "ANSWER_LOST" }, 0],
  ] as const) {
    test(`a 201 whose whole body ${name} prints ${Object.keys(printed)[0]} and exits ${code}`, async () => {
      const { seen, server } = answeringWhole("201 Created", type, body);
      try {
        const ran = await onFakeClock(
          record,
          { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${server.port}` },
          shortBound(1_000, 200)
        );
        expect({
          code: ran.code,
          stdout: ran.stdout,
          stderr: ran.stderr,
          posts: seen.posts,
        }).toEqual({
          code,
          stdout: `${JSON.stringify(printed)}\n`,
          stderr: "",
          posts: 1,
        });
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // The controls of the rows above: the true stored result, on the same raw socket, is printed
  // whole with its id; and a refusal, whatever its body, stays a refusal that stored nothing.
  test("a 201 whose whole body is the stored result prints it with its id and exits 0", async () => {
    const { seen, server } = answeringWhole(
      "201 Created",
      "application/json",
      JSON.stringify(stored)
    );
    try {
      const ran = await onFakeClock(
        record,
        { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${server.port}` },
        shortBound(1_000, 200)
      );
      expect({ code: ran.code, stderr: ran.stderr, posts: seen.posts }).toEqual({
        code: 0,
        stderr: "",
        posts: 1,
      });
      expect(JSON.parse(ran.stdout)).toEqual(stored);
      expect(JSON.parse(ran.stdout).id).toBe(stored.id);
    } finally {
      server.stop(true);
    }
  }, 30_000);

  for (const [name, type, body] of [
    [
      "Dispatch's own refusal",
      "application/json",
      '{"code":"CHECK_NOT_IN_FILES","error":"no such check"}',
    ],
    ["a page that is no JSON", "text/html", "<html><body>refused</body></html>"],
    ["an empty body", "application/json", ""],
    ["the JSON {}", "application/json", "{}"],
  ] as const) {
    test(`a 409 whose body is ${name} stays a refusal, exit 1, and no landing`, async () => {
      const { server } = answeringWhole("409 Conflict", type, body);
      try {
        const ran = await onFakeClock(
          record,
          { ...jobEnvironment(), DISPATCH_URL: `http://127.0.0.1:${server.port}` },
          shortBound(1_000, 200)
        );
        expect({ code: ran.code, landed: ran.stdout.includes("landed_without_answer") }).toEqual({
          code: 1,
          landed: false,
        });
        if (name === "Dispatch's own refusal") {
          expect(ran.stdout).toBe("CHECK_NOT_IN_FILES: no such check\n");
        }
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // A 2xx that is not Dispatch's answer to the write - it names no key back and gives no stored
  // result, or names another request's key back - came from whatever answers in Dispatch's place
  // (a proxy's page, a server that is not Dispatch), which may have passed the write on first:
  // the job prints write_outcome_unknown and exits 1, never landed_without_answer, which its
  // reporter reads as stored (B5 of the review of 355f8ada68). Each runs the command as its own
  // process, as a job's step runs it.
  for (const [name, status, type, body, echo, code] of [
    [
      "a 200 page that names no key back",
      "200 OK",
      "text/html",
      "<html><body>OK</body></html>",
      "none",
      "HTTP_200",
    ],
    [
      "a 200 with the stored result that names another request's key back",
      "200 OK",
      "application/json",
      JSON.stringify(stored),
      "other",
      "HTTP_200",
    ],
    ["a 204 that names no key back", "204 No Content", "application/json", "", "none", "HTTP_204"],
    [
      "a 201 of JSON from a server that is no Dispatch",
      "201 Created",
      "application/json",
      '{"ok":true}',
      "none",
      "HTTP_201",
    ],
  ] as const) {
    test(`${name} prints write_outcome_unknown and exits 1`, async () => {
      const { seen, server } = answeringWhole(status, type, body, echo);
      try {
        const ran = await spawnJob(record, {
          ...jobEnvironment(),
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        });
        expect({ code: ran.code, stdout: ran.stdout, posts: seen.posts }).toEqual({
          code: 1,
          stdout: `${JSON.stringify({ write_outcome_unknown: code })}\n`,
          posts: 1,
        });
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // Dispatch's keyed layer names the key back on every answer of every POST and PATCH route, so a
  // 201 that names no key back is no answer of Dispatch's to this write, whatever result it holds:
  // this call's own, another run's of the same check, verdict and commit (the hardening note H1 of the
  // deep audit of 7735fa52b4), or another call's of another project, check, verdict or commit (a
  // proxy or a cache that answers one write with another's). The write may have reached Dispatch
  // behind what answered: write_outcome_unknown, exit 1. No content of the answer is compared.
  const { predicate, subject } = stored.attestation;
  /** Another call's stored result: the job's own, its attestation changed as `change` says. */
  const another = (change: Partial<typeof stored.attestation>): typeof stored => ({
    ...stored,
    attestation: { ...stored.attestation, ...change },
  });
  for (const [held, body] of [
    ["this call's own stored result", stored],
    [
      "another run's stored result of the same check, verdict and commit",
      routeAnswer(
        "POST /api/v1/projects/{key}/results",
        storedSpecResult(44, "pass", "macos", 2),
        201
      ),
    ],
    [
      "another call's stored result, of another project",
      another({ predicate: { ...predicate, project: "OTHER" } }),
    ],
    [
      "another call's stored result, of another check",
      another({ predicate: { ...predicate, check: "p-2" } }),
    ],
    [
      "another call's stored result, of another verdict",
      another({ predicate: { ...predicate, verdict: "fail" } }),
    ],
    [
      "another call's stored result, of another commit",
      another({
        subject: subject.map((item) => ({ ...item, digest: { gitCommit: "2".repeat(40) } })),
      }),
    ],
  ] as const) {
    test(`a 201 with ${held} that names no key back prints write_outcome_unknown and exits 1`, async () => {
      const { seen, server } = answeringWhole(
        "201 Created",
        "application/json",
        JSON.stringify(body),
        "none"
      );
      try {
        const ran = await spawnJob(record, {
          ...jobEnvironment(),
          DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        });
        expect({ code: ran.code, stdout: ran.stdout, posts: seen.posts }).toEqual({
          code: 1,
          stdout: `${JSON.stringify({ write_outcome_unknown: "HTTP_201" })}\n`,
          posts: 1,
        });
      } finally {
        server.stop(true);
      }
    }, 30_000);
  }

  // Dispatch's own 204 to the write would name the key back: a body that gives no stored result
  // under it is the landing, as for its 201.
  test("a 204 that names the key back prints landed_without_answer and exits 0", async () => {
    const { seen, server } = answeringWhole("204 No Content", "application/json", "");
    try {
      const ran = await spawnJob(record, {
        ...jobEnvironment(),
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      });
      expect({ code: ran.code, stdout: ran.stdout, posts: seen.posts }).toEqual({
        code: 0,
        stdout: `${JSON.stringify({ landed_without_answer: "ANSWER_LOST" })}\n`,
        posts: 1,
      });
    } finally {
      server.stop(true);
    }
  }, 30_000);

  // Dispatch never answers a write with a redirect; a proxy in front of it sends one to its
  // sign-in page. The write follows none: the 302 is the answer, a refusal that names it with
  // exit 1, and the page is never asked for, so its 200 is never read as the write's.
  test("a 302 to a sign-in page is not followed, and is a refusal naming it", async () => {
    const seen = { posts: 0, pages: 0 };
    const proxy = Bun.serve({
      port: 0,
      fetch(request) {
        if (request.method === "POST") {
          seen.posts += 1;
          return new Response(null, { status: 302, headers: { Location: "/login" } });
        }
        seen.pages += 1;
        return new Response("<html><body>Sign in</body></html>", {
          headers: { "Content-Type": "text/html" },
        });
      },
    });
    try {
      const ran = await spawnJob(record, {
        ...jobEnvironment(),
        DISPATCH_URL: `http://127.0.0.1:${proxy.port}`,
      });
      expect({ code: ran.code, posts: seen.posts, pages: seen.pages }).toEqual({
        code: 1,
        posts: 1,
        pages: 0,
      });
      expect(ran.stdout).toStartWith(
        `HTTP_302: POST http://127.0.0.1:${proxy.port}/api/v1/projects/ACME/results answered 302`
      );
    } finally {
      proxy.stop(true);
    }
  }, 30_000);

  /** The results writes Dispatch got so far, the one it answers now included. */
  const posts = (): number => written.filter((seen) => seen.method === "POST").length;
  /** Dispatch's two answers of its check of a broker token that judged nothing, before any
   *  handler ran (`brokerTokenActor`): a spent bucket of checks, and a broker that answered
   *  nothing. */
  const tokenNotChecked = {
    limited: () =>
      Response.json(
        {
          code: "BROKER_TOKEN_RATE_LIMITED",
          error: "too many access tokens checked from this address; try again in 200ms",
        },
        { status: 429, headers: { "Retry-After": "1" } }
      ),
    unavailable: () =>
      Response.json(
        {
          code: "BROKER_UNAVAILABLE",
          error: "the secrets broker cannot be reached; try again in a minute",
        },
        { status: 503 }
      ),
  };
  /** The ways a call is turned away before anything is stored: Dispatch's check of the token, its
   *  429 and its 503 for a broker that answered nothing. Each sets the stand-in up to turn away
   *  `times` calls, then to answer as usual. */
  const turnedAway: readonly (readonly [name: string, setUp: (times: number) => void])[] = [
    [
      "Dispatch's 429 of its token check",
      (times) => {
        dispatchAnswer = () => (posts() <= times ? tokenNotChecked.limited() : storedAnswer());
      },
    ],
    [
      "Dispatch's 503 for a broker that answered nothing",
      (times) => {
        dispatchAnswer = () => (posts() <= times ? tokenNotChecked.unavailable() : storedAnswer());
      },
    ],
  ];

  // Dispatch's check of the job's token answers before any handler runs, so a write it turns away
  // did nothing: it is asked again after the answer's Retry-After, or a second, and one result is
  // stored.
  test("a call Dispatch's token check turns away is asked again, and the result is stored", async () => {
    const defaults = { dispatchAnswer };
    for (const [name, setUp] of turnedAway) {
      ({ dispatchAnswer } = defaults);
      written.length = 0;
      setUp(2);
      const { code, stdout, stderr, ms } = await onFakeClock(
        record,
        jobEnvironment(),
        shortBound(5_000, 1_000),
        eachWait
      );
      expect(
        {
          code,
          stderr,
          waited: ms !== undefined && ms >= 2_000,
          stored: code === 0 ? JSON.parse(stdout) : stdout,
        },
        name
      ).toEqual({ code: 0, stderr: "", waited: true, stored });
      expect(posts(), name).toBe(3);
    }
  }, 30_000);

  /** The one line a call turned away for its whole bound of `seconds` ends with: which turned
   *  it away, then that answer's own words. */
  const turnedAwayLines = (seconds: number): Record<string, string> => ({
    "Dispatch's 429 of its token check": `Dispatch's check of the job's token turned the write away, and the ${seconds} s this command may take leave no time to ask again: BROKER_TOKEN_RATE_LIMITED: too many access tokens checked from this address; try again in 200ms`,
    "Dispatch's 503 for a broker that answered nothing": `Dispatch's check of the job's token turned the write away, and the ${seconds} s this command may take leave no time to ask again: BROKER_UNAVAILABLE: the secrets broker cannot be reached; try again in a minute`,
  });

  // Turned away for ever, the call ends at its one bound with one line that says which turned it
  // away, and no write_outcome_unknown: no write ran.
  test("a call Dispatch's token check turns away for ever ends at the one bound with one line", async () => {
    const defaults = { dispatchAnswer };
    const lines = turnedAwayLines(3);
    for (const [name, setUp] of turnedAway) {
      ({ dispatchAnswer } = defaults);
      written.length = 0;
      setUp(Number.POSITIVE_INFINITY);
      const { code, stdout, stderr, ms } = await onFakeClock(
        record,
        jobEnvironment(),
        shortBound(3_000, 500),
        eachWait
      );
      expect(
        {
          code,
          stderr,
          lines: stdout.split("\n").length,
          withinTheBound: ms !== undefined && ms < 3_000,
        },
        name
      ).toEqual({
        code: 1,
        stderr: "",
        lines: 2,
        withinTheBound: true,
      });
      expect(stdout, name).toStartWith(lines[name] ?? "");
    }
  }, 30_000);

  // The case of the fresh acceptance of f60429ff0d (its D-A): the write client asks again under
  // the key inside each call, while Dispatch's check of the token turns every attempt away. The
  // client's deadline and the command's waits are one bound on one clock: the call ends inside it
  // with the refusal that names the last code, and no write_outcome_unknown, since no handler ever
  // ran. Before the one bound the client's own 60 s deadline, counted apart from the command's
  // waits, ended the call first, and an attempt it ended before it was sent read as one that may
  // have stored.
  for (const [name, answer, code] of [
    ["503 BROKER_UNAVAILABLE", tokenNotChecked.unavailable, "BROKER_UNAVAILABLE"],
    ["429 BROKER_TOKEN_RATE_LIMITED", tokenNotChecked.limited, "BROKER_TOKEN_RATE_LIMITED"],
  ] as const) {
    test(`Dispatch's token check turning every write away with ${name}, against the keyed layer, ends at the one bound naming ${code}`, async () => {
      dispatchAnswer = answer;
      const ran = await onFakeClock(record, jobEnvironment(), shortBound(8_000, 1_000), eachWait);
      expect(
        {
          code: ran.code,
          stderr: ran.stderr,
          lines: ran.stdout.split("\n").length,
          namesTheCause: ran.stdout.startsWith(
            "Dispatch's check of the job's token turned the write away"
          ),
          namesTheCode: ran.stdout.includes(`: ${code}: `),
          outcomeUnknown: ran.stdout.includes("write_outcome_unknown"),
          askedAgainUnderTheKey: posts() > 2,
          withinTheBound: ran.ms !== undefined && ran.ms < 8_000,
        },
        ran.stdout
      ).toEqual({
        code: 1,
        stderr: "",
        lines: 2,
        namesTheCause: true,
        namesTheCode: true,
        outcomeUnknown: false,
        askedAgainUnderTheKey: true,
        withinTheBound: true,
      });
    }, 90_000);
  }

  // The client's deadline is the command's bound: a Dispatch that never answers ends the call at
  // the bound, as a write whose outcome is unknown, since the attempt the bound cut was sent.
  test("a Dispatch that never answers ends the call at the one bound as an outcome unknown", async () => {
    const { promise: never } = Promise.withResolvers<Response>();
    dispatchAnswer = () => never;
    const ran = await onFakeClock(
      record,
      jobEnvironment(),
      shortBound(3_000, 500),
      async (clock) => {
        await arrived(() => written.length >= 1);
        clock.advance(3_000);
        // The bound aborts the write's request, which ends on the event loop's own time.
        await clock.ended;
      }
    );
    expect({
      code: ran.code,
      stdout: ran.stdout,
      stderr: ran.stderr,
      atTheBound: ran.ms === 3_000,
    }).toEqual({
      code: 1,
      stdout: `${JSON.stringify({ write_outcome_unknown: "TimeoutError" })}\n`,
      stderr: "",
      atTheBound: true,
    });
  }, 90_000);

  /** Dispatch's public host as the job's write names it behind the airlock. No resolver here
   *  answers it, so the write reaches nothing but the proxy. */
  const airlocked = "https://dispatch.ci-results.test";
  /** The proxy's URL as `agent-secrets result-grant` writes it in HTTPS_PROXY: the placeholder is
   *  its password, so no line the command prints may quote it. */
  const proxyUrl = (proxy: StubProxy): string =>
    proxy.url.replace("http://", `http://agent-secrets:${PLACEHOLDER}@`);
  /** The headers the airlock proxy writes on an answer of its own. */
  const AIRLOCK_FORWARDED = "Cipher-Proxy-Forwarded";
  const AIRLOCK_CODE = "Cipher-Proxy-Code";

  // The airlock proxy answers a CONNECT whose password names no live grant with 407, and Bun tells
  // the caller its status line alone (ERR_PROXY_TUNNEL). Nothing went on to Dispatch, so the write
  // ends at once and says so, with no write_outcome_unknown: asked again, the proxy answers the
  // same. The command runs as its own process, with the proxy in the HTTPS_PROXY its step has.
  test("a CONNECT the proxy refuses with 407 ends the write at once: nothing was sent, no write_outcome_unknown", async () => {
    const proxy = startStubProxy(() => ({
      status: 407,
      reason: "Proxy Authentication Required",
      code: "GRANT_NOT_LIVE",
    }));
    try {
      const ran = await spawnJob(record, {
        ...jobEnvironment(),
        DISPATCH_URL: airlocked,
        HTTPS_PROXY: proxyUrl(proxy),
      });
      expect({
        code: ran.code,
        stdout: ran.stdout,
        stderr: ran.stderr,
        connects: proxy.connects,
      }).toEqual({
        code: 1,
        stdout:
          "AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (407 Proxy Authentication Required); nothing was sent to Dispatch\n",
        stderr: "",
        connects: ["dispatch.ci-results.test:443"],
      });
      expect(written).toEqual([]);
      namesNoToken(record, ran.stdout, ran.stderr);
    } finally {
      proxy.stop();
    }
  }, 30_000);

  // A 503 at the CONNECT is a connection that never opened, as a refused one is: the write asks
  // again within its bound, on the schedule of every write, and then ends with that refusal, its
  // status named, and no write_outcome_unknown. It runs on the fake clock, through the proxy as
  // fetch's own option, the form HTTPS_PROXY takes in a process of its own.
  test("a CONNECT the proxy refuses with 503 is asked again within the bound, and ends naming 503 and nothing sent", async () => {
    const proxy = startStubProxy(() => ({
      status: 503,
      reason: "Service Unavailable",
      code: "GRANT_UNREADABLE",
    }));
    try {
      const ran = await onFakeClock(
        record,
        { ...jobEnvironment(), DISPATCH_URL: airlocked },
        shortBound(5_000, 1_000),
        eachWait,
        proxyUrl(proxy)
      );
      expect({
        code: ran.code,
        stdout: ran.stdout,
        stderr: ran.stderr,
        askedAgain: proxy.connects.length >= 2,
        withinTheBound: ran.ms !== undefined && ran.ms < 5_000,
      }).toEqual({
        code: 1,
        stdout:
          "AIRLOCK_TUNNEL_REFUSED: the airlock proxy refused the connection (503 Service Unavailable); nothing was sent to Dispatch\n",
        stderr: "",
        askedAgain: true,
        withinTheBound: true,
      });
      expect(written).toEqual([]);
    } finally {
      proxy.stop();
    }
  }, 30_000);

  // The tunnel's refusal says nothing of an earlier attempt that may have reached Dispatch: here
  // the proxy opens the first tunnel and breaks it after the write went into it, and refuses the
  // next CONNECT 407. The write may be stored, so the command prints write_outcome_unknown, never
  // that nothing was sent.
  test("a CONNECT refused after an attempt that may have reached Dispatch prints write_outcome_unknown", async () => {
    const proxy = startStubProxy((connect) =>
      connect === 1
        ? "open-then-close"
        : { status: 407, reason: "Proxy Authentication Required", code: "GRANT_NOT_LIVE" }
    );
    try {
      const ran = await onFakeClock(
        record,
        { ...jobEnvironment(), DISPATCH_URL: airlocked },
        shortBound(5_000, 1_000),
        eachWait,
        proxyUrl(proxy)
      );
      expect({
        code: ran.code,
        stdout: ran.stdout,
        stderr: ran.stderr,
        connects: proxy.connects.length,
      }).toEqual({
        code: 1,
        stdout: `${JSON.stringify({ write_outcome_unknown: "AIRLOCK_TUNNEL_REFUSED" })}\n`,
        stderr: "",
        connects: 2,
      });
    } finally {
      proxy.stop();
    }
  }, 30_000);

  /** The airlock proxy's own refusal inside an open connection, in the proxy's words. */
  const airlockRefusal = (status: number, code: string, words: string) => () =>
    new Response(words, {
      status,
      headers: {
        [AIRLOCK_FORWARDED]: "false",
        [AIRLOCK_CODE]: code,
        "Content-Type": "text/plain; charset=utf-8",
      },
    });

  // Inside a connection that was open, the proxy answers a request it refuses before it dials
  // anything, and says so (`Cipher-Proxy-Forwarded: false`): the answer reached no service and
  // stored nothing. It is final, printed with its code and the proxy's words as the client's
  // excerpt quotes every proxy's own answer: one line, cut to the excerpt's length, each piece of
  // the placeholder redacted, the word "placeholder" among them, which the placeholder spells. It
  // is no write_outcome_unknown.
  test("the airlock's refusal inside an open connection prints its code and its words as the excerpt quotes them, and says nothing was stored", async () => {
    const words =
      "airlock: a request to this host carries the grant's placeholder as its one Authorization header, DPoP <placeholder>; this one does not, so nothing was sent (answer 7f3a)";
    dispatchAnswer = airlockRefusal(403, "NO_PLACEHOLDER", `${words} ${PLACEHOLDER}`);
    const { code, stdout, stderr } = await runJob(record, jobEnvironment());
    expect({ code, stdout, stderr, posts: posts() }).toEqual({
      code: 1,
      stdout:
        "NO_PLACEHOLDER: airlock: a request to this host carries the grant's [redacted] as its one Authorization header, DPoP ; this one does not…; nothing was stored\n",
      stderr: "",
      posts: 1,
    });
    namesNoToken(record, stdout, stderr);
  });

  /** The airlock proxy's own answer written after the write may have left it for Dispatch
   *  (`Cipher-Proxy-Forwarded: unknown`): a connection that broke after the write was sent, a
   *  grant that ended while Dispatch held the call. */
  const airlockAfterSend = (status: number, code: string, words: string) => () =>
    new Response(words, {
      status,
      headers: {
        [AIRLOCK_FORWARDED]: "unknown",
        [AIRLOCK_CODE]: code,
        "Content-Type": "text/plain; charset=utf-8",
      },
    });

  // The proxy's own answer after the write may have reached Dispatch says so
  // (`Cipher-Proxy-Forwarded: unknown`): Dispatch may have stored it, so the write is asked again
  // under its key, which settles it, and ends write_outcome_unknown when nothing settles it within
  // the bound, never "nothing was stored". The same answer said `false`, written before anything
  // left the proxy, is the control: nothing reached Dispatch, so the 5xx is asked again under the
  // key as a connection that never opened is, and ends with nothing stored and no
  // write_outcome_unknown.
  test("the airlock's own answer after the write may have left is asked again under the key, and ends write_outcome_unknown", async () => {
    const broke = airlockAfterSend(
      502,
      "UPSTREAM_UNREACHABLE",
      "airlock: dispatch.example: stream error: stream ID 1; INTERNAL_ERROR; received from peer"
    );
    const cut = airlockAfterSend(
      407,
      "GRANT_NOT_LIVE",
      "airlock: grant is expired, revoked, or its session ended (answer 7f3a)"
    );
    const rows: readonly (readonly [
      name: string,
      answer: () => Response | Promise<Response>,
      code: number,
      stdout: (out: string) => boolean,
      askedAgain: boolean,
    ])[] = [
      [
        "a 502 after the write was sent, then stored",
        () => (posts() <= 1 ? broke() : storedAnswer()),
        0,
        (out) => JSON.stringify(JSON.parse(out)) === JSON.stringify(stored),
        true,
      ],
      [
        "a 502 after the write was sent, for the whole bound",
        broke,
        1,
        (out) => out === `${JSON.stringify({ write_outcome_unknown: "UPSTREAM_UNREACHABLE" })}\n`,
        true,
      ],
      [
        "a 407 of a grant that ended while Dispatch held the write, then stored",
        () => (posts() <= 1 ? cut() : storedAnswer()),
        0,
        (out) => JSON.stringify(JSON.parse(out)) === JSON.stringify(stored),
        true,
      ],
      [
        "a 407 of a grant that ended while Dispatch held the write, for the whole bound",
        cut,
        1,
        (out) => out === `${JSON.stringify({ write_outcome_unknown: "GRANT_NOT_LIVE" })}\n`,
        true,
      ],
      [
        "the same 502 written before anything left the proxy, the control",
        airlockRefusal(
          502,
          "UPSTREAM_UNREACHABLE",
          "airlock: dispatch.example: dial tcp: connection refused"
        ),
        1,
        (out) =>
          out ===
          "UPSTREAM_UNREACHABLE: airlock: dispatch.example: dial tcp: connection refused; nothing was stored\n",
        true,
      ],
    ];
    const seen: unknown[] = [];
    for (const [name, answer, wanted, printed, again] of rows) {
      written.length = 0;
      dispatchAnswer = answer;
      const ran = await onFakeClock(record, jobEnvironment(), shortBound(5_000, 1_000), eachWait);
      let matched = false;
      try {
        matched = printed(ran.stdout);
      } catch {
        // A row that wants the stored result parses what was printed, which a refusal is not.
      }
      seen.push({
        name,
        code: ran.code === wanted ? true : ran.code,
        printed: matched ? true : ran.stdout,
        askedAgain: posts() > 1 === again ? true : posts(),
      });
      namesNoToken(record, ran.stdout, ran.stderr);
    }
    expect(seen).toEqual(
      rows.map(([name]) => ({ name, code: true, printed: true, askedAgain: true }))
    );
  }, 30_000);

  // A grant the proxy could not read this moment (its 503 GRANT_UNREADABLE) may be readable at the
  // next attempt, so the write asks again under its key, and the result is stored. Turned away so
  // for the whole bound, it ends with the proxy's refusal and no write_outcome_unknown, since no
  // attempt reached a service. Dispatch's own 503, the control, may come after it committed, and
  // ends as an outcome unknown.
  test("the airlock's GRANT_UNREADABLE inside an open connection is asked again within the bound", async () => {
    const unreadable = airlockRefusal(
      503,
      "GRANT_UNREADABLE",
      "airlock: the grant could not be read"
    );
    const rows: readonly (readonly [
      name: string,
      answer: () => Response | Promise<Response>,
      code: number,
      stdout: (out: string) => boolean,
    ])[] = [
      [
        "twice, then stored",
        () => (posts() <= 2 ? unreadable() : storedAnswer()),
        0,
        (out) => JSON.stringify(JSON.parse(out)) === JSON.stringify(stored),
      ],
      [
        "for the whole bound",
        unreadable,
        1,
        (out) =>
          out === "GRANT_UNREADABLE: airlock: the grant could not be read; nothing was stored\n",
      ],
      [
        "Dispatch's own 503, the control",
        () => Response.json({ code: "UNAVAILABLE", error: "internal" }, { status: 503 }),
        1,
        (out) => out === `${JSON.stringify({ write_outcome_unknown: "UNAVAILABLE" })}\n`,
      ],
    ];
    for (const [name, answer, wanted, printed] of rows) {
      written.length = 0;
      dispatchAnswer = answer;
      const ran = await onFakeClock(record, jobEnvironment(), shortBound(5_000, 1_000), eachWait);
      expect(
        {
          code: ran.code,
          stderr: ran.stderr,
          printed: printed(ran.stdout),
          askedAgain: posts() >= 3,
        },
        `${name}: ${ran.stdout}`
      ).toEqual({ code: wanted, stderr: "", printed: true, askedAgain: true });
    }
  }, 30_000);

  // The reserve of the one bound is set from the wait of Dispatch's check of a token on the broker,
  // which the command copies: the copy is held to Go's own constant, so a change of that wait
  // changes the reserve with it.
  test("the reserve's copy of Dispatch's token-check wait is Go's introspectTimeout", () => {
    const go = readFileSync(
      join(import.meta.dir, "..", "..", "..", "envoy", "internal", "ciphertoken", "introspect.go"),
      "utf8"
    );
    const seconds = /^const introspectTimeout = (\d+) \* time\.Second$/m.exec(go)?.[1];
    expect(seconds === undefined ? undefined : Number(seconds) * 1_000).toBe(
      DISPATCH_TOKEN_CHECK_WAIT_MS
    );
  });

  // A job's log is read by its runner, which ends a line at \n and at \r, and reads a workflow
  // command from a line that starts with "::" once its leading white space is trimmed and from a
  // "##[" anywhere in a line. The call prints text it did not write: its arguments, its
  // environment, and the answers of the broker and of Dispatch. Each such value is printed on its
  // line, escaped by the rule of Go's runnerlog.Printable (runner-log.ts), so no value starts a
  // line of its own and no line is a workflow command. A row that shows the value also shows it
  // in the form the rule prints it in: as text in a line of words, as JSON in a JSON answer, and
  // as the client's excerpt of it in a line of words that quotes a proxy's own answer.
  test("no value the call prints starts a line of its own or is a workflow command", async () => {
    const long = "y".repeat(100_000);
    /** A value, and how it prints in a line of words, in a JSON answer, and in a line of words
     *  that quotes it through the client's excerpt (a proxy's own answer, `AirlockRefusedError`):
     *  on one line, its white space one space, cut to the excerpt's length. */
    const values: readonly (readonly [
      name: string,
      value: string,
      text: string,
      json: string,
      excerpted: string,
    ])[] = [
      [
        "a line break",
        "p\n::error::line",
        "p\\n::error::line",
        '"p\\n::error::line"',
        "p ::error::line",
      ],
      [
        "a carriage return",
        "p\r::warning::cr",
        "p\\r::warning::cr",
        '"p\\r::warning::cr"',
        "p ::warning::cr",
      ],
      [
        "control characters",
        "p\u001b[2K\u000b\u0085::error::c",
        "p\\x1b[2K\\v\\u0085::error::c",
        '"p\\u001b[2K\\u000b\\u0085::error::c"',
        "p\\x1b[2K \\u0085::error::c",
      ],
      ["an error command", "::error::x", "::error::x", '"::error::x"', "::error::x"],
      [
        "a command of the second form",
        "##[group]x",
        "##\\x5bgroup]x",
        '"##\\u005bgroup]x"',
        "##\\x5bgroup]x",
      ],
      ["a mask command", "::add-mask::x", "::add-mask::x", '"::add-mask::x"', "::add-mask::x"],
      [
        "a stop of command processing",
        "::stop-commands::t",
        "::stop-commands::t",
        '"::stop-commands::t"',
        "::stop-commands::t",
      ],
      [
        "a very long line",
        `${long}\n::error::long`,
        `${long}\\n::error::long`,
        `"${long}\\n::error::long"`,
        `${"y".repeat(120)}…;`,
      ],
    ];
    /** The arguments of the job's call with `value` after `flag`. */
    const withValue = (flag: string, value: string): string[] => {
      const argv = [...record];
      argv[argv.indexOf(flag) + 1] = value;
      return argv;
    };
    /** The body of the last write Dispatch got, as the stand-in reads it to word its refusal. */
    const sent = (): Record<string, string> =>
      (written.at(-1)?.body ?? {}) as Record<string, string>;
    const refused = (code: string, error: string) =>
      Response.json({ code, error }, { status: code === "COMMIT_NOT_THE_RUNS" ? 403 : 409 });
    /** Each place a value reaches a print: whether the output shows the value, and in which
     *  form, and how one call puts `value` there. */
    const places: readonly (readonly [
      place: string,
      shows: "text" | "json" | "excerpted" | "none",
      run: (value: string) => Promise<{ code: number; stdout: string; stderr: string }>,
    ])[] = [
      [
        "the project key",
        "none",
        (value) => runJob(withValue("--project", value), jobEnvironment()),
      ],
      [
        "the check id, in a dry run",
        "json",
        (value) => runJob([...withValue("--check", value), "--dry-run"], jobEnvironment()),
      ],
      [
        "the check id, in Dispatch's refusal",
        "text",
        (value) => {
          dispatchAnswer = () =>
            refused(
              "CHECK_NOT_THIS_JOBS",
              `check ${sent().check} names the job "lint", and this token's job is "go-checks" (check run 7001, "go-checks"): a job writes results only for the checks that name it`
            );
          return runJob(withValue("--check", value), jobEnvironment());
        },
      ],
      [
        "the commit, in Dispatch's refusal",
        "text",
        (value) => {
          dispatchAnswer = () =>
            refused(
              "COMMIT_NOT_THE_RUNS",
              `the result is for commit ${sent().commit}, and this token's run tested commit ${SPEC_COMMIT}: a job writes results only for the commit that it tested`
            );
          return runJob(withValue("--commit", value), jobEnvironment());
        },
      ],
      [
        "the observed text, refused as a flag",
        "text",
        (value) => runJob([...record, "--observed", `--- ${value}`], jobEnvironment()),
      ],
      [
        "the observed file's name, refused as unreadable",
        "text",
        (value) => runJob([...record, "--observed-file", `${stateDir}/${value}`], jobEnvironment()),
      ],
      [
        "Dispatch's error text",
        "text",
        (value) => {
          dispatchAnswer = () => refused("CHECK_NOT_IN_FILES", value);
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "Dispatch's error code",
        "none",
        (value) => {
          dispatchAnswer = () => refused(value, "the result is refused");
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "a body that is not Dispatch's JSON",
        "none",
        (value) => {
          dispatchAnswer = () =>
            new Response(value, { status: 400, headers: { "Content-Type": "text/plain" } });
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "a spec file's name in Dispatch's refusal",
        "text",
        (value) => {
          dispatchAnswer = () =>
            refused(
              "SPEC_FILES_REFUSED",
              `the spec files of example-org/example-repo at ${SPEC_COMMIT} have 1 problem, so they hold no check to write a result for. The first: .dispatch/spec/${value}.md:1: a-lonely-statement: leaf-without-check: no statement is under it and no check serves it`
            );
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "the stored result's observed text",
        "json",
        (value) => {
          const kept = { ...stored.attestation.predicate, observed: value };
          dispatchAnswer = () =>
            storedAnswer({ ...stored, attestation: { ...stored.attestation, predicate: kept } });
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "the airlock proxy's own refusal",
        "excerpted",
        (value) => {
          dispatchAnswer = () =>
            new Response(value, {
              status: 403,
              headers: { [AIRLOCK_FORWARDED]: "false", [AIRLOCK_CODE]: "NO_PLACEHOLDER" },
            });
          return runJob(record, jobEnvironment());
        },
      ],
      [
        "Dispatch's code of a write it may have stored",
        "json",
        (value) => {
          dispatchAnswer = () => Response.json({ code: value, error: "internal" }, { status: 500 });
          return runJob(record, jobEnvironment(), oneAttempt);
        },
      ],
      [
        // `dispatch results --project` runs as a session, so its output may be read in a job's
        // log too: a listed result's leg, in its line and as --json.
        "a listed result's leg, in the listing and as --json",
        "text",
        async (value) => {
          dispatchAnswer = () =>
            Response.json({ results: [storedSpecResult(44, "fail", value, 1)] });
          const session = {
            PATH: process.env.PATH,
            HOME: stateDir,
            DISPATCH_STATE_DIR: stateDir,
            DISPATCH_URL: dispatchUrl,
            DISPATCH_HOST: "omp",
            DISPATCH_SESSION_ID: "ses-results-listing",
            DISPATCH_TOKEN: fixtureSecret("session"),
          };
          const listing = ["results", "--project", "ACME", "--commit", SPEC_COMMIT];
          const text = await runJob(listing, session);
          const json = await runJob([...listing, "--json"], session);
          return {
            code: text.code || json.code,
            stdout: `${text.stdout}${json.stdout}`,
            stderr: `${text.stderr}${json.stderr}`,
          };
        },
      ],
    ];
    const defaults = { dispatchAnswer };
    /** One line per row that failed: its place, its value, and what was wrong with the output. */
    const failed: string[] = [];
    for (const [place, shows, run] of places) {
      for (const [name, value, text, json, excerpted] of values) {
        ({ dispatchAnswer } = defaults);
        const { code, stdout, stderr } = await run(value);
        const out = `${stdout}${stderr}`;
        // Split wherever a runner or a log viewer can end a line, so no line can hide in another.
        const lines = out.split(/\r\n|[\n\r\u0085\u2028\u2029]/);
        const commands = lines.filter(
          (line) => line.trimStart().startsWith("::") || line.includes("##[")
        );
        // The command's own line ends are its one \n each; any other line break, control
        // character, format character or separator came from a value.
        const unescaped = [...out.matchAll(/[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]/gu)]
          .map(([character]) => character)
          .filter((character) => character !== "\n");
        const shown = shows === "none" || out.includes({ text, json, excerpted }[shows]);
        if (commands.length > 0 || unescaped.length > 0 || !shown) {
          const characters = unescaped.map((character) => character.codePointAt(0)?.toString(16));
          failed.push(
            `${place}, ${name}: exit ${code}, ${commands.length} workflow command lines, ` +
              `unescaped [${characters.join(" ")}], value shown ${shown}, printed ` +
              JSON.stringify(out.slice(0, 160))
          );
        }
      }
    }
    expect(failed).toEqual([]);
  }, 120_000);

  test("--issue and --project are exclusive, refused before any request", async () => {
    for (const env of [
      jobEnvironment(),
      { ...jobEnvironment(), DISPATCH_HOST: "omp", DISPATCH_SESSION_ID: "ses-1" },
    ]) {
      const { code, stdout } = await runJob([...record, "--issue", "DSP-1"], env);
      expect(code, stdout).toBe(2);
      expect(stdout).toContain("exactly one of issue and project is required");
    }
    sentNothing();
  });

  test("an argument of an issue's record is refused with --project", async () => {
    for (const [flag, value, field] of [
      ["--role", "implementer", "role"],
      ["--surface", "api", "surface"],
      ["--run", "go test ./...", "run"],
      ["--negative-control", "The test without its check.", "negative_control"],
      ["--failed", "0", "failed"],
      ["--skipped", "0", "skipped"],
      ["--excluded", "0", "excluded"],
      ["--screenshot", "dispatch://DSP-1/artifact/shot-png", "screenshots"],
      ["--retract", "00000000-0000-4000-8000-000000000041", "retract"],
    ] as const) {
      const { code, stdout } = await runJob([...record, flag, value], jobEnvironment());
      expect(code, `${flag}: ${stdout}`).toBe(2);
      expect(stdout, flag).toContain(`This call also carries ${field}.`);
    }
    sentNothing();
  });

  test("posts the body {commit, check, verdict, observed} and nothing else", async () => {
    // A person's bearer in the job's environment is not what the job writes with.
    const env = { ...jobEnvironment(), DISPATCH_TOKEN: fixtureSecret("person") };
    const observed = "panic: test timed out after 10m0s";
    const withObserved = await runJob(
      [...record.map((word) => (word === "pass" ? "could_not_run" : word)), "--observed", observed],
      env
    );
    expect(withObserved.code, withObserved.stdout).toBe(0);
    const bare = await runJob(record, env);
    expect(bare.code, bare.stdout).toBe(0);

    expect(written.map((seen) => seen.body)).toStrictEqual([
      { commit: SPEC_COMMIT, check: "p-1", verdict: "could_not_run", observed },
      { commit: SPEC_COMMIT, check: "p-1", verdict: "pass" },
    ]);
    expect(written.map((seen) => seen.authorization)).toEqual([
      `DPoP ${PLACEHOLDER}`,
      `DPoP ${PLACEHOLDER}`,
    ]);
  });

  test("sends what a result keeps of observed: its start and its end around the note, with what Dispatch cannot store replaced", async () => {
    // A failing test's log over 1 MiB is ordinary, and Dispatch takes no request that large. A
    // result keeps SPEC_CHECK_OBSERVED_MAX characters, so that is what the command sends: the
    // first SPEC_CHECK_OBSERVED_START of them, the note that says the middle was cut, and the last
    // SPEC_CHECK_OBSERVED_END. A text of the bound goes whole, and a character outside the basic
    // plane, two UTF-16 units, is one character of the count. A NUL, which Dispatch cannot store,
    // and each byte that is not UTF-8 are sent as U+FFFD, so the result is still written.
    const start = SPEC_CHECK_OBSERVED_START;
    const end = SPEC_CHECK_OBSERVED_END;
    const note = SPEC_CHECK_OBSERVED_CUT_NOTE;
    const rows: [name: string, content: string | Buffer, sent: string][] = [
      [
        "a log of 1 MiB",
        `=== RUN   TestP\n${"x".repeat(1024 * 1024)}--- FAIL: TestP\n`,
        `=== RUN   TestP\n${"x".repeat(start - 16)}${note}${"x".repeat(end - 16)}--- FAIL: TestP\n`,
      ],
      [
        "a text of the bound",
        "é".repeat(SPEC_CHECK_OBSERVED_MAX),
        "é".repeat(SPEC_CHECK_OBSERVED_MAX),
      ],
      [
        "a text one past the bound",
        `s${"é".repeat(SPEC_CHECK_OBSERVED_MAX - 1)}e`,
        `s${"é".repeat(start - 1)}${note}${"é".repeat(end - 1)}e`,
      ],
      [
        "a text outside the basic plane",
        "🙂".repeat(2500),
        `${"🙂".repeat(start)}${note}${"🙂".repeat(end)}`,
      ],
      [
        "a NUL and bytes that are not UTF-8",
        Buffer.from([0x61, 0x00, 0x62, 0xff, 0x63]),
        "a\uFFFDb\uFFFDc",
      ],
    ];
    for (const [name, content] of rows) {
      const file = join(stateDir, `observed-${name.replaceAll(" ", "-")}.txt`);
      writeFileSync(file, content);
      const ran = await runJob([...record, "--observed-file", file], jobEnvironment());
      expect(ran.code, `${name}: ${ran.stdout}`).toBe(0);
    }

    const sent = written.map(({ body }) =>
      body !== null &&
      typeof body === "object" &&
      "observed" in body &&
      typeof body.observed === "string"
        ? body.observed
        : ""
    );
    expect(sent).toEqual(rows.map(([, , kept]) => kept));
    for (const kept of sent.slice(0, 4)) expect([...kept].length).toBe(SPEC_CHECK_OBSERVED_MAX);
  });

  test("a failing test's log of 10,000 characters sends its first line and its --- FAIL line, with the note between them", async () => {
    // A test's output names the test in its first line and says why it failed at its end.
    const first = "=== RUN   TestX\n";
    const last = "--- FAIL: TestX (0.01s)\n";
    const log = join(stateDir, "observed-failing.txt");
    writeFileSync(log, `${first}${"x\n".repeat((10_000 - first.length - last.length) / 2)}${last}`);
    const ran = await runJob([...record, "--observed-file", log], jobEnvironment());
    expect(ran.code, ran.stdout).toBe(0);
    const body = written[0]?.body as { observed?: string } | undefined;
    const sent = body?.observed ?? "";
    const note = sent.indexOf(SPEC_CHECK_OBSERVED_CUT_NOTE);
    expect({
      characters: [...sent].length,
      first: sent.startsWith(first),
      last: sent.endsWith(last),
      noteBetween:
        note >= first.length &&
        note + SPEC_CHECK_OBSERVED_CUT_NOTE.length <= sent.length - last.length,
    }).toEqual({ characters: SPEC_CHECK_OBSERVED_MAX, first: true, last: true, noteBetween: true });
  });
});
