import { generateKeyPairSync, randomBytes } from "node:crypto";
import {
  closeSync,
  mkdirSync,
  mkdtempSync,
  openSync,
  readdirSync,
  readFileSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import * as path from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { SQL, type Subprocess } from "bun";
import { resultsTLSFiles } from "./ci-results-tls";
import { type DispatchStack, startDispatchStack, testDatabaseServer } from "./dispatch-stack";
import {
  awaitReady,
  buildGoBinaries,
  ENVOY_MODULE,
  type ReadyBudget,
  stopProcess,
} from "./envoy-stack";
import {
  type FakeGitHubActions,
  GITHUB_ACTIONS_ISSUER,
  startFakeGitHubActions,
} from "./fake-github-actions";

// The local stack a CI job writes its result on, with every part that a production job meets
// but GitHub itself: the real secrets broker (./cmd/cipherd) with one result entry, the real
// Dispatch server (./cmd/dispatch) reading that broker, and a stand-in of GitHub
// (fake-github-actions.ts), which issues the job's OIDC token and answers Dispatch's reads of the
// check run and of the spec files. A cluster's token issuer for Dispatch's own account at the
// broker is dev-oidc-issuer, as packages/envoy/scripts/dev-broker.sh starts it.
//
// Run as a script, `bun packages/envoy-client/test/ci-results-stack.ts` starts the stack, seeds
// project ACME mapped to the repository local-org/local-repo and one commit of it with the probe
// spec, prints the variables of one job of a push run and the commands of the try line, and runs
// until Ctrl-C. With `--forge-only` it starts the stand-in's Actions token endpoint alone and
// prints the three variables a job reads it by: each token it mints is signed by a key no real
// issuer holds, so a broker that verifies GitHub's tokens refuses it on its signature.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

/** The project the stack's result entry writes, mapped to {@link REPOSITORY}. */
const PROJECT = "ACME";
/** The repository whose runs the result entry takes. */
const REPOSITORY = "local-org/local-repo";
/** The workflow the result entry lists, as ci-results.yaml is listed in production. */
const WORKFLOW = ".github/workflows/ci-results.yaml";
/** The refs the entry takes: every branch of two segments, such as `refs/heads/topic/one`. */
const REF_PATTERN = "refs/heads/*/*";
/** The person the entry acts for, which a result entry needs (`exchange.checkGrant`) and which no
 *  stored result, no answer of Dispatch and no output of `dispatch result --project` names. */
const PERSON = "ada@example.com";
/** The audience a job asks its runner to mint its token for, which the broker requires. */
const ACTIONS_AUDIENCE = "dispatch-ci-results-local";
/** The App installation of {@link REPOSITORY} at the stand-in. */
const INSTALLATION = 7;
/** The directory of a commit that holds its spec files. */
const SPEC_DIR = ".dispatch/spec";

/** Dispatch's own service account at the broker (BROKER_API_ACCOUNTS), the audience its token
 *  carries, and the broker's sign-in client at the issuer, as run-broker.sh names them. */
const DISPATCH_ACCOUNT = "system:serviceaccount:example-ns:example-dispatch";
const CLUSTER_AUDIENCE = "example-broker-pod";
const SIGNIN_CLIENT = "ci-results-broker";
/** The secrets the broker is started with: none, under the example prefix and key
 *  dev-broker.sh uses, since nothing here asks for a secret. */
const SECRETS_PREFIX = "example/agent-secrets/";
const SECRETS_KMS_KEY_ARN =
  "arn:aws:kms:us-east-1:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab";

/** The session and the principal a results token of the entry writes under. */
const RESULT_SESSION = `ci-results:${REPOSITORY}/${WORKFLOW}`;
const RESULT_PRINCIPAL = `workflow-results:${REPOSITORY}/${WORKFLOW}`;

/** The check of the probe spec that names the job go-checks, and the one that names lint. */
export const PROBE_CHECK = "probe-passes";
export const OTHER_JOBS_CHECK = "second-probe-passes";

/** The probe spec: one statement and its two program checks, each of another job. */
const PROBE_SPEC = `:::requirement{#the-probe-tests-pass kind="behavior"}
The two probe tests pass at each commit of a branch.
:::

:::check{#${PROBE_CHECK} serves="the-probe-tests-pass" subject="code" procedure="program" runner="go" selector="example.com/acme/probe TestProbe" job="go-checks"}
Claim: The probe test passes.
Procedure: The program runs the probe test one time.
Failing example: A probe test that fails.
:::

:::check{#${OTHER_JOBS_CHECK} serves="the-probe-tests-pass" subject="code" procedure="program" runner="go" selector="example.com/acme/probe TestSecondProbe" job="lint"}
Claim: The second probe test passes.
Procedure: The program runs the second probe test one time.
Failing example: A second probe test that fails.
:::
`;

/** Writes the probe spec into `dir` as the one file `probe.md`, for {@link CIResultsStack.setFiles}. */
export function writeProbeSpec(dir: string): void {
  writeFileSync(path.join(dir, "probe.md"), PROBE_SPEC);
}

/** One job of a push run of the workflow, as `jobEnv` makes its variables. */
export interface JobRun {
  /** The commit the run tested, 40 lowercase hex characters. */
  readonly sha: string;
  /** The ref the run was pushed to, as `refs/heads/<branch>`. */
  readonly ref: string;
  /** The job's name, which its check run is named after. */
  readonly job: string;
  readonly runAttempt: number;
  /** The job's check run, which Dispatch reads the job's name from. */
  readonly checkRunId: number;
  /** The matrix values of the job's leg, which its check run's name carries in parentheses. */
  readonly legName?: string;
  /** Whether the run's tokens are signed by a key the stand-in's issuer does not publish. */
  readonly forged?: boolean;
}

/** A row of `GET /api/v1/projects/{key}/results`, as far as a reader of this stack reads it. */
export interface StoredResult {
  readonly id: string;
  readonly seq: number;
  readonly attestation: {
    readonly predicate: {
      readonly check: string;
      readonly verdict: string;
      readonly observed: string;
    };
  };
}

/** The running stack. */
export interface CIResultsStack {
  /** The real producer binary; child commands never trade an OIDC identity themselves. */
  readonly agentSecrets: string;
  /** The Dispatch server, and the agent token it takes, which is a session's own. */
  readonly dispatch: DispatchStack;
  /** The broker's base URL, as `AGENT_SECRETS_URL` takes it. */
  readonly brokerUrl: string;
  /** The stand-in of GitHub. */
  readonly github: FakeGitHubActions;
  readonly project: string;
  readonly repository: string;
  /** The person the result entry acts for: the broker's configuration, which no result names. */
  readonly person: string;
  /** The session and the principal every results token of the entry writes under. */
  readonly resultSession: string;
  readonly resultPrincipal: string;
  /** The variables a job of `run` holds: Dispatch, the broker, and its runner's token endpoint.
   *  Its check run is added at the stand-in, so Dispatch reads the job's name there. */
  jobEnv(run: JobRun): Record<string, string>;
  /** The spec files of the commit `sha`: every `.md` file directly in `dir`. */
  setFiles(sha: string, dir: string): void;
  /** Every result of the project at `commit`, as a person reads them, newest first. */
  results(commit: string): Promise<StoredResult[]>;
  /** Stops Dispatch, the broker, the issuer and the stand-in, and drops the broker's database. */
  stop(): Promise<void>;
}

/**
 * Reserves the directory one stack works in, as dispatch-stack.ts's `useDispatchStackDirectory`
 * does: call it once, at a test file's module scope, and hand the path to
 * {@link startCIResultsStack}.
 */
export function useCIResultsStackDirectory(): string {
  return temporaryDirectory("ci-results-stack");
}

// The broker migrates a database of its own as it boots, after a cold build of two commands.
const BROKER_READY: ReadyBudget = { timeoutMs: 120_000, tailLines: 20 };

/** The first record of the broker's JSON log whose message is `message` and whose fields hold
 *  `fields`, or undefined while there is none. */
function loggedLine(
  log: string,
  message: string,
  fields: Readonly<Record<string, unknown>> = {}
): Record<string, unknown> | undefined {
  let text: string;
  try {
    text = readFileSync(log, "utf8");
  } catch {
    return undefined;
  }
  for (const line of text.split("\n")) {
    let record: Record<string, unknown>;
    try {
      record = JSON.parse(line) as Record<string, unknown>;
    } catch {
      continue;
    }
    if (record.message !== message) continue;
    if (Object.entries(fields).every(([name, value]) => record[name] === value)) return record;
  }
  return undefined;
}

/** A child process writing its output to `log`, started with `env` and nothing else. */
function spawnLogged(
  command: readonly string[],
  env: Record<string, string>,
  log: string
): Subprocess {
  const out = openSync(log, "a");
  try {
    return Bun.spawn([...command], { cwd: ENVOY_MODULE, env, stdout: out, stderr: out });
  } finally {
    closeSync(out);
  }
}

/**
 * Starts the stack in `options.work`, which the caller reserved with
 * {@link useCIResultsStackDirectory} and which is empty, and resolves once each part serves and
 * project ACME maps local-org/local-repo. It throws, naming why, when a part does not come up;
 * whatever started by then is stopped.
 */
export async function startCIResultsStack(options: {
  readonly work: string;
}): Promise<CIResultsStack> {
  const { work } = options;
  // Read first: a run with no DSN fails before anything starts that would need a stop.
  const postgres = testDatabaseServer();
  const tls = await resultsTLSFiles(path.join(work, "tls"));
  const github = startFakeGitHubActions({ installations: { [REPOSITORY]: INSTALLATION } });
  const database = `ci_results_broker_${crypto.randomUUID().replaceAll("-", "")}`;
  const admin = new SQL(postgres);
  let issuer: Subprocess | undefined;
  let broker: Subprocess | undefined;
  let dispatch: DispatchStack | undefined;
  let created = false;
  const stop = async (): Promise<void> => {
    // Dispatch reads the broker, and the broker reads the issuer and the stand-in.
    await tls.stop();
    await dispatch?.stop();
    if (broker !== undefined) await stopProcess(broker, "SIGTERM");
    if (issuer !== undefined) await stopProcess(issuer, "SIGTERM");
    await github.stop();
    if (created) await admin.unsafe(`drop database if exists ${database} with (force)`);
    await admin.close();
  };
  try {
    const bin = path.join(work, "bin");
    await buildGoBinaries({
      bin,
      packages: ["./cmd/cipherd", "./cmd/dev-oidc-issuer", "./cmd/cipher"],
      stack: "CI results",
      binaries: "./cmd/cipherd, ./cmd/dev-oidc-issuer and ./cmd/cipher",
    });

    // The cluster's issuer: Dispatch's own token at the broker, rewritten before it lapses, and
    // the broker's sign-in client, which the broker requires and nothing here signs in at.
    const accountToken = path.join(work, "dispatch-account-token");
    const signInSecret = path.join(work, "signin-client-secret");
    writeFileSync(signInSecret, randomBytes(32).toString("hex"), { mode: 0o600 });
    const issuerLog = path.join(work, "issuer.log");
    issuer = spawnLogged(
      [
        path.join(bin, "dev-oidc-issuer"),
        "--token",
        `${CLUSTER_AUDIENCE}=${DISPATCH_ACCOUNT}=${accountToken}`,
        "--code-flow",
        `${SIGNIN_CLIENT}=${signInSecret}`,
      ],
      { PATH: SHELL_PATH, HOME: work },
      issuerLog
    );
    let issuerUrl = "";
    await awaitReady("dev-oidc-issuer", issuer, issuerLog, BROKER_READY, async () => {
      issuerUrl = /issuer listening url=(\S+)/.exec(readFileSync(issuerLog, "utf8"))?.[1] ?? "";
      return issuerUrl !== "";
    });

    await admin.unsafe(`create database ${database} strategy file_copy`);
    created = true;
    const databaseUrl = new URL(postgres);
    databaseUrl.pathname = `/${database}`;
    const fakeSecrets = path.join(work, "fake-secrets.json");
    writeFileSync(fakeSecrets, JSON.stringify({ secrets: [] }), { mode: 0o600 });
    const rulesFile = path.join(work, "airlock.json");
    writeFileSync(
      rulesFile,
      JSON.stringify({
        secrets: [
          {
            name: "DISPATCH_RESULTS_TOKEN",
            kind: "dpop",
            hosts: [tls.host],
            headers: ["Authorization"],
            allow: [{ method: "POST", pathPrefix: "/api/v1/projects/" }],
          },
        ],
      })
    );
    const brokerLog = path.join(work, "broker.log");
    broker = spawnLogged(
      [path.join(bin, "cipherd")],
      {
        PATH: SHELL_PATH,
        HOME: work,
        BROKER_DATABASE_URL: databaseUrl.toString(),
        BROKER_LISTEN_ADDR: "127.0.0.1:0",
        BROKER_PUBLIC_URL: "http://127.0.0.1:0",
        BROKER_METRICS_LISTEN_ADDR: "127.0.0.1:0",
        BROKER_DRAIN_SECONDS: "0",
        BROKER_SECRETS_PREFIX: SECRETS_PREFIX,
        BROKER_SECRETS_KMS_KEY_ARN: SECRETS_KMS_KEY_ARN,
        BROKER_FAKE_SECRETS_FILE: fakeSecrets,
        BROKER_AIRLOCK_RULES_FILE: rulesFile,
        BROKER_AIRLOCK_LISTEN_ADDR: "127.0.0.1:0",
        BROKER_AIRLOCK_PROXY_URL: "http://127.0.0.1:0",
        BROKER_AIRLOCK_CA_CERT_FILE: tls.ca,
        BROKER_AIRLOCK_CA_KEY_FILE: tls.key,
        BROKER_AIRLOCK_TEST_UPSTREAM_CA_FILE: tls.ca,
        BROKER_AIRLOCK_TEST_RESOLVE: `${tls.host}=${tls.address}`,
        BROKER_AIRLOCK_TEST_DIAL_ALLOW: `${tls.host}:443 ${tls.address}:443`,
        BROKER_K8S_OIDC_ISSUER: issuerUrl,
        BROKER_K8S_OIDC_AUDIENCE: CLUSTER_AUDIENCE,
        BROKER_API_ACCOUNTS: `dispatch=${DISPATCH_ACCOUNT}`,
        BROKER_SIGNIN_ISSUER: issuerUrl,
        BROKER_SIGNIN_CLIENT_ID: SIGNIN_CLIENT,
        BROKER_SIGNIN_CLIENT_SECRET_FILE: signInSecret,
        BROKER_SIGNIN_DOMAIN: PERSON.split("@")[1] ?? "",
        BROKER_EXCHANGE_GITHUB_ISSUER: github.url,
        BROKER_EXCHANGE_GITHUB_AUDIENCE: ACTIONS_AUDIENCE,
        BROKER_EXCHANGE_RESULT_WORKFLOWS: `${REPOSITORY}=${WORKFLOW}=${REF_PATTERN}=${PROJECT}=${PERSON}`,
      },
      brokerLog
    );
    let brokerUrl = "";
    await awaitReady("the secrets broker", broker, brokerLog, BROKER_READY, async () => {
      const listening = loggedLine(brokerLog, "broker listening");
      if (
        listening === undefined ||
        loggedLine(brokerLog, "token exchange", { state: "on" }) === undefined
      ) {
        return false;
      }
      brokerUrl = `http://${String(listening.addr)}`;
      return (await fetch(`${brokerUrl}/healthz`, { signal: AbortSignal.timeout(2_000) })).ok;
    });

    const dispatchWork = path.join(work, "dispatch");
    mkdirSync(dispatchWork);
    // A throwaway App key, as packages/dispatch/e2e/run-server.sh makes one: every App call goes
    // to the stand-in on this machine.
    const appKey = generateKeyPairSync("rsa", { modulusLength: 2048 }).privateKey.export({
      type: "pkcs8",
      format: "pem",
    });
    dispatch = await startDispatchStack({
      work: dispatchWork,
      env: {
        DISPATCH_SERVER_URL: tls.url,
        DISPATCH_AGENT_SECRETS_URL: brokerUrl,
        DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE: accountToken,
        DISPATCH_GITHUB_API_BASE: github.url,
        DISPATCH_APP_CLIENT_ID: "Iv1.cilocalfake",
        DISPATCH_APP_PEM_B64: String(appKey),
      },
    });
    await tls.start(dispatch.url);

    const server = dispatch;
    const asPerson = async (method: string, route: string, body?: unknown): Promise<unknown> => {
      const response = await fetch(`${server.url}${route}`, {
        method,
        headers: { "Content-Type": "application/json", "X-Dispatch-User": PERSON },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      const text = await response.text();
      if (!response.ok) throw new Error(`${method} ${route}: ${response.status} ${text}`);
      return JSON.parse(text);
    };
    await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    await asPerson("PUT", `/api/v1/settings/repo-projects/${REPOSITORY}`, { project: PROJECT });

    // One workflow run for each push of a ref at a commit, as GitHub numbers them: the attempts
    // and the legs of one push share its run.
    const runIds = new Map<string, number>();
    return {
      agentSecrets: path.join(bin, "cipher"),
      dispatch,
      brokerUrl,
      github,
      project: PROJECT,
      repository: REPOSITORY,
      person: PERSON,
      resultSession: RESULT_SESSION,
      resultPrincipal: RESULT_PRINCIPAL,
      jobEnv(run: JobRun): Record<string, string> {
        const push = `${run.ref}@${run.sha}`;
        const runId = runIds.get(push) ?? 1000 + runIds.size;
        runIds.set(push, runId);
        const request = github.addRun({
          repository: REPOSITORY,
          workflow: WORKFLOW,
          ref: run.ref,
          sha: run.sha,
          runId,
          runAttempt: run.runAttempt,
          checkRunId: run.checkRunId,
          checkRunName: run.legName === undefined ? run.job : `${run.job} (${run.legName})`,
          forged: run.forged === true,
        });
        return {
          DISPATCH_URL: tls.url,
          AGENT_SECRETS_URL: brokerUrl,
          AGENT_SECRETS_MACHINE_IDENTITY: "github-actions",
          AGENT_SECRETS_MACHINE_AUDIENCE: ACTIONS_AUDIENCE,
          ACTIONS_ID_TOKEN_REQUEST_URL: request.requestUrl,
          ACTIONS_ID_TOKEN_REQUEST_TOKEN: request.requestToken,
        };
      },
      setFiles(sha: string, dir: string): void {
        const files: Record<string, string> = {};
        for (const entry of readdirSync(dir, { withFileTypes: true })) {
          if (entry.isFile() && entry.name.endsWith(".md")) {
            files[entry.name] = readFileSync(path.join(dir, entry.name), "utf8");
          }
        }
        github.setFiles(REPOSITORY, sha, SPEC_DIR, files);
      },
      async results(commit: string): Promise<StoredResult[]> {
        const read = (await asPerson(
          "GET",
          `/api/v1/projects/${PROJECT}/results?commit=${commit}`
        )) as { results: StoredResult[] };
        return read.results;
      },
      stop,
    };
  } catch (error) {
    await stop();
    throw error;
  }
}

/** The CLI of this checkout, as a shell outside the package runs it. */
const CLI = `bun ${path.resolve(import.meta.dir, "..", "bin", "dispatch.ts")}`;

/** Quotes one value for a POSIX shell without interpreting its contents. */
function shellWord(value: string): string {
  return `'${value.replaceAll("'", "'\"'\"'")}'`;
}

/** Resolves on the first Ctrl-C or SIGTERM. */
function untilInterrupted(): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  process.once("SIGINT", () => resolve());
  process.once("SIGTERM", () => resolve());
  return promise;
}

/** `--forge-only`: the stand-in's token endpoint, minting a forged token of a push run of this
 *  repository's ci-results.yaml on refs/heads/probe/forge, until Ctrl-C. */
async function forgeOnly(): Promise<void> {
  const github = startFakeGitHubActions({ issuer: GITHUB_ACTIONS_ISSUER });
  const request = github.addRun({
    repository: "cybermonklabs/legion",
    workflow: WORKFLOW,
    ref: "refs/heads/probe/forge",
    sha: "f0".repeat(20),
    runId: 1,
    runAttempt: 1,
    checkRunId: 1,
    checkRunName: "go-checks",
    forged: true,
  });
  console.log(`export ACTIONS_ID_TOKEN_REQUEST_URL='${request.requestUrl}'`);
  console.log(`export ACTIONS_ID_TOKEN_REQUEST_TOKEN=${shellWord(request.requestToken)}`);
  console.log("export AGENT_SECRETS_MACHINE_IDENTITY=github-actions");
  console.error(
    "ci-results-stack: each token is signed by a key no issuer publishes; set AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_AUDIENCE to the broker's, then run agent-secrets result-grant -- dispatch result --project. Ctrl-C stops."
  );
  await untilInterrupted();
  await github.stop();
}

/** The whole stack, seeded, with the try line's commands, until Ctrl-C. */
async function wholeStack(): Promise<void> {
  const work = mkdtempSync(path.join(tmpdir(), "ci-results-stack-"));
  const spec = path.join(work, "spec");
  mkdirSync(spec);
  writeProbeSpec(spec);
  const stackWork = path.join(work, "stack");
  mkdirSync(stackWork);
  console.error(`ci-results-stack: starting in ${work}`);
  const stack = await startCIResultsStack({ work: stackWork });
  try {
    const tested = "0123456789abcdef0123456789abcdef01234567";
    const other = "89abcdef0123456789abcdef0123456789abcdef";
    stack.setFiles(tested, spec);
    stack.setFiles(other, spec);
    const run = {
      sha: tested,
      ref: "refs/heads/topic/try",
      job: "go-checks",
      runAttempt: 1,
      checkRunId: 1,
    };
    const job = stack.jobEnv(run);
    const forged = stack.jobEnv({ ...run, checkRunId: 2, forged: true });
    const result = `${shellWord(stack.agentSecrets)} result-grant -- ${CLI} result --project ${stack.project} --verdict pass`;
    console.log("# One job of a push run of local-org/local-repo at the commit below:");
    for (const [name, value] of Object.entries(job)) console.log(`export ${name}='${value}'`);
    console.log("# The try line: one stored result, then a session's read of it:");
    console.log(`${result} --check ${PROBE_CHECK} --commit ${tested}`);
    console.log(
      `env -u AGENT_SECRETS_URL DISPATCH_HOST=omp DISPATCH_SESSION_ID=ci-results-try DISPATCH_TOKEN=${shellWord(stack.dispatch.token)} ${CLI} results --project ${stack.project} --commit ${tested}`
    );
    console.log("# Each control is refused:");
    console.log(`${result} --check ${PROBE_CHECK} --commit ${other}   # another commit`);
    console.log(`${result} --check ${OTHER_JOBS_CHECK} --commit ${tested}   # another job`);
    console.log(`${CLI} issues --project ${stack.project}   # another route`);
    console.log(
      `ACTIONS_ID_TOKEN_REQUEST_TOKEN=${shellWord(forged.ACTIONS_ID_TOKEN_REQUEST_TOKEN ?? "")} ${result} --check ${PROBE_CHECK} --commit ${tested}   # a forged token`
    );
    console.error("ci-results-stack: ready; Ctrl-C stops the stack.");
    await untilInterrupted();
  } finally {
    await stack.stop();
    rmSync(work, { recursive: true, force: true });
  }
}

if (import.meta.main) {
  await (process.argv.includes("--forge-only") ? forgeOnly() : wholeStack());
}
