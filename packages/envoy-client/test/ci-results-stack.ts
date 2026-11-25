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
import { z } from "zod";
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
// but GitHub itself: two real secrets brokers (./cmd/cipherd) on one database, each with one
// result entry and the airlock's proxy, the real Dispatch server (./cmd/dispatch) reading them,
// and a stand-in of GitHub (fake-github-actions.ts), which issues the job's OIDC token and answers
// Dispatch's reads of the check run and of the spec files. A cluster's token issuer for
// Dispatch's own account at the brokers is dev-oidc-issuer, as packages/envoy/scripts/dev-broker.sh
// starts it.
//
// The two brokers stand where the deployment's broker pods stand, as
// packages/envoy/scripts/dev-airlock.sh runs two: each behind one `dev-proxy-stands front` for the
// API ports and another for the proxy ports, a byte relay that hands each new connection to the
// next broker in turn and carries a CONNECT tunnel as it carries anything else. A job reaches
// Dispatch the way a job in production does: it holds the airlock placeholder of its run's result
// grant, which `agent-secrets result-grant` gives the command it runs, and writes to Dispatch's
// public URL, https://dispatch.ci-results.test, through the proxy front. Whichever broker takes
// the tunnel reads the grant from the database, mints the run's results token bound to its own
// key, signs a DPoP proof for that public URL and sends the write to Dispatch's own loopback port,
// the one private destination of its settings (BROKER_PROXY_PRIVATE_UPSTREAM). No resolver
// answers the public name: a write that goes around the proxy opens no connection at all.
//
// Run as a script, `bun packages/envoy-client/test/ci-results-stack.ts` starts the stack, seeds
// project ACME, whose default repository is local-org/local-repo, one commit of that repository
// with the probe spec and issue ACME-1 whose Verification document holds a check CI proves and an
// ordinary one, prints the variables of one job of a push run and the commands of the try line,
// and runs until Ctrl-C. With `--forge-only` it starts the stand-in's Actions token endpoint alone
// and prints the three variables a job reads it by: each token it mints is signed by a key no real
// issuer holds, so a broker that verifies GitHub's tokens refuses it on its signature.

// Destructured: claude-envoy's typecheck, which compiles this package's test files, forbids
// `process.env.X`.
const { PATH: SHELL_PATH = "" } = process.env;

/** The project the stack's result entry writes, whose default repository is {@link REPOSITORY}. */
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

/** Dispatch's public host, the one host the result entry binds: a name no resolver answers, so
 *  only the airlock's proxy reaches Dispatch by it. */
export const DISPATCH_HOST = "dispatch.ci-results.test";
/** Dispatch's public URL, `DISPATCH_SERVER_URL` and the origin each proof names. */
const DISPATCH_PUBLIC_URL = `https://${DISPATCH_HOST}`;
/** The broker cluster's own ranges the proxies refuse (BROKER_AIRLOCK_CLUSTER_RANGES): the
 *  benchmarking range of RFC 2544, which no other rule of the proxy refuses. */
export const CLUSTER_RANGES = "198.18.0.0/15";
/** The broker processes the stack runs, as two pods of one deployment. */
const BROKER_COUNT = 2;

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

/** The line each broker logs once at start, naming the thumbprint of its pod's DPoP key. */
const DPOP_KEY_LINE = "broker: the proxy's DPoP key";
/** The message of the line Dispatch writes for each request it served (cmd/dispatch's README,
 *  "Access log"). */
const ACCESS_LINE = 'msg="dispatch: request"';

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

/** The check of the seeded issue's Verification document that its ci_check gives to the job
 *  go-checks, and the ordinary check beside it, which no job proves. */
export const CI_CHECK = "c1";
export const ORDINARY_CHECK = "k1";

/** The negative control both checks name: a check CI proves names one (docs.CICheckRule). */
const NEGATIVE_CONTROL = "A build whose server does not build.";

/** The seeded issue's Verification document. */
const VERIFICATION = `:::check{#${CI_CHECK} negative_control="${NEGATIVE_CONTROL}" ci_check="go-checks"}
The server builds at the head.
:::

:::check{#${ORDINARY_CHECK} negative_control="${NEGATIVE_CONTROL}"}
A person reads the issue page.
:::
`;

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
      readonly run: { readonly check_run_id: string };
    };
  };
}

/** Where each log of the stack ended when {@link CIResultsStack.logMark} was taken: the byte
 *  offset after the last whole line of each broker's log, in broker order, and of Dispatch's. */
export interface LogMark {
  readonly brokers: readonly number[];
  readonly dispatch: number;
}

/** The lines the stack's logs gained since a {@link LogMark}. */
export interface LogLines {
  /** Each broker's new lines, each the JSON record it logged, in broker order. */
  readonly brokers: readonly (readonly Record<string, unknown>[])[];
  /** Dispatch's new access-log lines, one per request it served, as it wrote them. */
  readonly access: readonly string[];
}

/** The broker's introspection of a token, as far as a reader of this stack reads it: whether it
 *  is live, and for a live results token its scope, the key thumbprint it is bound to and its
 *  run's check run. */
const IntrospectionSchema = z.object({
  active: z.boolean(),
  scope: z.string().optional(),
  jkt: z.string().optional(),
  ci: z.object({ check_run_id: z.string().optional() }).optional(),
});
export type Introspection = z.infer<typeof IntrospectionSchema>;

/** What dev-proxy-stands reports of the airlock's certificate authority it wrote, and what one of
 *  its fronts reports of the address it bound. */
const StandsReportSchema = z.object({ ca_cert_file: z.string(), ca_key_file: z.string() });
const FrontReportSchema = z.object({ address: z.string().min(1) });

/** The running stack. */
export interface CIResultsStack {
  /** The real producer binary, `./cmd/cipher` built as `agent-secrets`: child commands never
   *  trade an OIDC identity themselves. */
  readonly agentSecrets: string;
  /** The Dispatch server, and the agent token it takes, which is a session's own. Its `url` is its
   *  own loopback port, which a job never names. */
  readonly dispatch: DispatchStack;
  /** The API front's base URL, as `AGENT_SECRETS_URL` takes it: either broker answers. */
  readonly brokerUrl: string;
  /** The proxy front's address, `host:port`, which every grant's proxy URL names. */
  readonly proxyAddress: string;
  /** The thumbprint of each broker's pod key, as each logged it at start, in broker order. */
  readonly thumbprints: readonly string[];
  /** The stand-in of GitHub. */
  readonly github: FakeGitHubActions;
  readonly project: string;
  readonly repository: string;
  /** The seeded issue of the project, ACME-1, whose Verification document holds CI_CHECK, which
   *  its ci_check gives to the job go-checks, and ORDINARY_CHECK. */
  readonly issue: string;
  /** The person the result entry acts for: the broker's configuration, which no result names. */
  readonly person: string;
  /** The session and the principal every results token of the entry writes under. */
  readonly resultSession: string;
  readonly resultPrincipal: string;
  /** The environment of `agent-secrets result-grant` in a job of `run`: Dispatch's public URL, the
   *  broker, and its runner's token endpoint. Its check run is added at the stand-in, so Dispatch
   *  reads the job's name there. */
  jobEnv(run: JobRun): Record<string, string>;
  /** The argv that runs `argv` under `agent-secrets result-grant`, as a job's step runs it: as the
   *  child of this stack's real `agent-secrets result-grant --` (agentSecrets). Run with
   *  `jobEnv(run)`, the producer trades the run's token at a broker for the airlock's placeholder
   *  of the run, and starts `argv` with that placeholder alone in DISPATCH_RESULTS_TOKEN and the
   *  proxy front as its proxy, so the placeholder a row's command presents is one a broker minted. */
  jobCommand(argv: readonly string[]): string[];
  /** The spec files of the commit `sha`: every `.md` file directly in `dir`. */
  setFiles(sha: string, dir: string): void;
  /** The head of `branch` of the repository is `sha` from now on, as a push moves it. */
  setBranch(branch: string, sha: string): void;
  /** Every result of the project at `commit`, as a person reads them, newest first. */
  results(commit: string): Promise<StoredResult[]>;
  /** Where each log ends now, for {@link linesSince}. */
  logMark(): LogMark;
  /** The lines each log gained since `mark`. */
  linesSince(mark: LogMark): LogLines;
  /** Ends every result grant of the check run `checkRunId` in the brokers' database at once, in
   *  one committed update, and answers how many it ended. */
  endResultGrants(checkRunId: number): Promise<number>;
  /** The key thumbprint each results token the brokers' store holds for the check run
   *  `checkRunId` is bound to: one entry per access-token session whose run is that one. */
  resultTokenBindings(checkRunId: number): Promise<string[]>;
  /** The brokers' introspection of `token`, asked as Dispatch's own account asks it. */
  introspect(token: string): Promise<Introspection>;
  /** Stops Dispatch, the brokers, their fronts, the issuer and the stand-in, and drops the
   *  brokers' database. */
  stop(): Promise<void>;
}

/** What a caller may change about the stack it starts. */
export interface CIResultsStackOptions {
  /** The directory the stack works in, which the caller reserved with
   *  {@link useCIResultsStackDirectory} and which is empty. */
  readonly work: string;
  /** How long a result grant lasts, in seconds (BROKER_RESULT_GRANT_TEST_SECONDS), in place of
   *  the production lifetime, so a row can watch a grant end. */
  readonly grantSeconds?: number;
}

/**
 * Reserves the directory one stack works in, as dispatch-stack.ts's `useDispatchStackDirectory`
 * does: call it once, at a test file's module scope, and hand the path to
 * {@link startCIResultsStack}. The directory holds the four binaries the stack builds, and its
 * `afterAll` hands it to a removal it does not wait for, for the reason `useEnvoyStackDirectory`
 * states.
 */
export function useCIResultsStackDirectory(): string {
  return temporaryDirectory("ci-results-stack", { removeInBackground: true });
}

// The broker migrates a database of its own as it boots, after a cold build of four commands.
const BROKER_READY: ReadyBudget = { timeoutMs: 120_000, tailLines: 20 };
// dev-proxy-stands binds its sockets and writes its report before anything else.
const FRONT_READY: ReadyBudget = { timeoutMs: 30_000, tailLines: 20 };

/** Every JSON record of a log's text, in order; a line that is no JSON object is left out. */
function records(text: string): Record<string, unknown>[] {
  const out: Record<string, unknown>[] = [];
  for (const line of text.split("\n")) {
    try {
      const record: unknown = JSON.parse(line);
      if (typeof record === "object" && record !== null && !Array.isArray(record)) {
        out.push(record as Record<string, unknown>);
      }
    } catch {
      // A line of the log that is no record, such as the last one while it is being written.
    }
  }
  return out;
}

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
  return records(text).find(
    (record) =>
      record.message === message &&
      Object.entries(fields).every(([name, value]) => record[name] === value)
  );
}

/** A child process writing its output to `log`, or its standard output to `out` and its errors to
 *  `log`, started with `env` and nothing else. */
function spawnLogged(
  command: readonly string[],
  env: Record<string, string>,
  log: string,
  out: string = log
): Subprocess {
  const errors = openSync(log, "a");
  const output = out === log ? errors : openSync(out, "a");
  try {
    return Bun.spawn([...command], { cwd: ENVOY_MODULE, env, stdout: output, stderr: errors });
  } finally {
    closeSync(errors);
    if (output !== errors) closeSync(output);
  }
}

/** Builds `./cmd/cipher` as `<bin>/agent-secrets`, the name a job's step runs it by, and answers
 *  its path. */
async function buildAgentSecrets(bin: string): Promise<string> {
  const binary = path.join(bin, "agent-secrets");
  const spawned = Bun.spawn(["go", "build", "-o", binary, "./cmd/cipher"], {
    cwd: ENVOY_MODULE,
    stdout: "pipe",
    stderr: "pipe",
  });
  const [stderr, code] = await Promise.all([new Response(spawned.stderr).text(), spawned.exited]);
  if (code !== 0) {
    throw new Error(
      `go build -o ${binary} ./cmd/cipher in ${ENVOY_MODULE} exited ${code}: ${stderr}`
    );
  }
  return binary;
}

/** The byte offset after the last whole line of `file`, 0 for a file not there yet. */
function wholeLinesEnd(file: string): number {
  let text: Buffer;
  try {
    text = readFileSync(file);
  } catch {
    return 0;
  }
  return text.lastIndexOf(0x0a) + 1;
}

/** The whole lines `file` gained after the byte offset `from`. */
function linesAfter(file: string, from: number): string[] {
  let text: Buffer;
  try {
    text = readFileSync(file);
  } catch {
    return [];
  }
  const end = text.lastIndexOf(0x0a) + 1;
  if (end <= from) return [];
  return text.subarray(from, end).toString("utf8").split("\n").slice(0, -1);
}

/** One `dev-proxy-stands front` over the addresses its backends file will name. */
interface Front {
  readonly child: Subprocess;
  /** The front's own `host:port`. */
  readonly address: string;
  /** The file it reads its backends from, again whenever the file changes. */
  readonly backends: string;
  /** Its own log, which names each connection it could not hand on. */
  readonly log: string;
}

/**
 * Starts the stack in `options.work`, which the caller reserved with
 * {@link useCIResultsStackDirectory} and which is empty, and resolves once each part serves and
 * project ACME's default repository is local-org/local-repo. It throws, naming why, when a part
 * does not come up; whatever started by then is stopped.
 */
export async function startCIResultsStack(options: CIResultsStackOptions): Promise<CIResultsStack> {
  const { work } = options;
  // Read first: a run with no DSN fails before anything starts that would need a stop.
  const postgres = testDatabaseServer();
  const github = startFakeGitHubActions({ installations: { [REPOSITORY]: INSTALLATION } });
  const database = `ci_results_broker_${crypto.randomUUID().replaceAll("-", "")}`;
  const admin = new SQL(postgres);
  const databaseUrl = new URL(postgres);
  databaseUrl.pathname = `/${database}`;
  let brokerDatabase: SQL | undefined;
  let issuer: Subprocess | undefined;
  const fronts: Subprocess[] = [];
  const brokers: Subprocess[] = [];
  let dispatch: DispatchStack | undefined;
  let created = false;
  const stop = async (): Promise<void> => {
    // Dispatch reads the brokers through the API front, and the brokers read the issuer and the
    // stand-in.
    await dispatch?.stop();
    for (const broker of brokers) await stopProcess(broker, "SIGTERM");
    for (const front of fronts) await stopProcess(front, "SIGTERM");
    if (issuer !== undefined) await stopProcess(issuer, "SIGTERM");
    await github.stop();
    await brokerDatabase?.close();
    if (created) await admin.unsafe(`drop database if exists ${database} with (force)`);
    await admin.close();
  };
  try {
    const bin = path.join(work, "bin");
    await buildGoBinaries({
      bin,
      packages: ["./cmd/cipherd", "./cmd/dev-oidc-issuer", "./cmd/dev-proxy-stands"],
      stack: "CI results",
      binaries: "./cmd/cipherd, ./cmd/dev-oidc-issuer, ./cmd/dev-proxy-stands and ./cmd/cipher",
    });
    const agentSecrets = await buildAgentSecrets(bin);
    const stands = path.join(bin, "dev-proxy-stands");

    // The cluster's issuer: Dispatch's own token at the brokers, rewritten before it lapses, and
    // the brokers' sign-in client, which a broker requires and nothing here signs in at. Its one
    // registered callback is the broker's approval page on loopback, at any port, as
    // scripts/dev-broker.sh registers it (dev-oidc-issuer's --code-flow takes the callback since
    // main's #5185).
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
        `${SIGNIN_CLIENT}=${signInSecret}=http://127.0.0.1/signin/callback`,
      ],
      { PATH: SHELL_PATH, HOME: work },
      issuerLog
    );
    let issuerUrl = "";
    await awaitReady("dev-oidc-issuer", issuer, issuerLog, BROKER_READY, async () => {
      issuerUrl = /issuer listening url=(\S+)/.exec(readFileSync(issuerLog, "utf8"))?.[1] ?? "";
      return issuerUrl !== "";
    });

    // The airlock's certificate authority, as dev-airlock.sh takes it: the pair dev-proxy-stands
    // writes as it starts its stands. The stands themselves are of no use here, so the process
    // stops once its report names the pair, and the files stay.
    const standsDir = path.join(work, "stands");
    const standsJson = path.join(work, "stands.json");
    const standsLog = path.join(work, "stands.log");
    const standsChild = spawnLogged(
      [stands, "--dir", standsDir],
      { PATH: SHELL_PATH, HOME: work },
      standsLog,
      standsJson
    );
    try {
      await awaitReady("dev-proxy-stands", standsChild, standsLog, FRONT_READY, async () =>
        readFileSync(standsLog, "utf8").includes("stands listening records=")
      );
    } finally {
      await stopProcess(standsChild, "SIGTERM");
    }
    const authority = StandsReportSchema.parse(JSON.parse(readFileSync(standsJson, "utf8")));

    // The two fronts, bound before the brokers exist: each broker's BROKER_PUBLIC_URL and
    // BROKER_AIRLOCK_PROXY_URL name them at boot, and Dispatch reads the brokers through the API
    // front. Each reads its backends from a file written once the brokers report their ports.
    const startFront = async (name: string): Promise<Front> => {
      const backends = path.join(work, `${name}-backends`);
      const report = path.join(work, `${name}-front.json`);
      const log = path.join(work, `${name}-front.log`);
      const child = spawnLogged(
        [stands, "front", "--listen", "127.0.0.1:0", "--backends-file", backends],
        { PATH: SHELL_PATH, HOME: work },
        log,
        report
      );
      fronts.push(child);
      await awaitReady(`the ${name} front`, child, log, FRONT_READY, async () =>
        readFileSync(log, "utf8").includes("front listening addr=")
      );
      const { address } = FrontReportSchema.parse(JSON.parse(readFileSync(report, "utf8")));
      return { child, address, backends, log };
    };
    const apiFront = await startFront("api");
    const proxyFront = await startFront("proxy");
    const brokerUrl = `http://${apiFront.address}`;

    await admin.unsafe(`create database ${database} strategy file_copy`);
    created = true;

    // Dispatch first, on a port of its own: each broker's private destination is that port.
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
        DISPATCH_SERVER_URL: DISPATCH_PUBLIC_URL,
        DISPATCH_AGENT_SECRETS_URL: brokerUrl,
        DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE: accountToken,
        DISPATCH_GITHUB_API_BASE: github.url,
        DISPATCH_APP_CLIENT_ID: "Iv1.cilocalfake",
        DISPATCH_APP_PEM_B64: String(appKey),
      },
    });
    const dispatchLog = path.join(dispatchWork, "dispatch.log");
    const dispatchAddress = new URL(dispatch.url).host;

    const fakeSecrets = path.join(work, "fake-secrets.json");
    writeFileSync(fakeSecrets, JSON.stringify({ secrets: [] }), { mode: 0o600 });
    // C8's profile: a name and a proxy block alone, Dispatch's public host alone, kind dpop, the
    // placeholder in Authorization alone, and a POST under the projects' and the issues' routes
    // alone.
    const profilesFile = path.join(work, "profiles.json");
    writeFileSync(
      profilesFile,
      JSON.stringify({
        profiles: [
          {
            name: "DISPATCH_RESULTS_TOKEN",
            proxy: {
              kind: "dpop",
              hosts: [DISPATCH_HOST],
              headers: ["Authorization"],
              // The two results routes a results token writes: a check of the spec files, and the
              // check of an issue's Verification document whose ci_check names the job. Dispatch's
              // own gate refuses the token on every other route under the second prefix.
              allow: [
                { method: "POST", pathPrefix: "/api/v1/projects/" },
                { method: "POST", pathPrefix: "/api/v1/issues/" },
              ],
            },
          },
        ],
      })
    );
    const brokerEnv: Record<string, string> = {
      PATH: SHELL_PATH,
      HOME: work,
      BROKER_DATABASE_URL: databaseUrl.toString(),
      BROKER_LISTEN_ADDR: "127.0.0.1:0",
      BROKER_PUBLIC_URL: brokerUrl,
      BROKER_METRICS_LISTEN_ADDR: "127.0.0.1:0",
      BROKER_DRAIN_SECONDS: "0",
      BROKER_SECRETS_PREFIX: SECRETS_PREFIX,
      BROKER_SECRETS_KMS_KEY_ARN: SECRETS_KMS_KEY_ARN,
      BROKER_FAKE_SECRETS_FILE: fakeSecrets,
      BROKER_PROFILES_FILE: profilesFile,
      BROKER_AIRLOCK_LISTEN_ADDR: "127.0.0.1:0",
      BROKER_AIRLOCK_PROXY_URL: `http://${proxyFront.address}`,
      BROKER_AIRLOCK_CA_CERT_FILE: authority.ca_cert_file,
      BROKER_AIRLOCK_CA_KEY_FILE: authority.ca_key_file,
      // Dispatch's own loopback port is the destination of the bound host's allowed writes, and
      // the one loopback address and port the proxies may dial.
      BROKER_PROXY_PRIVATE_UPSTREAM: `${DISPATCH_HOST}=http://${dispatchAddress}`,
      BROKER_AIRLOCK_TEST_DIAL_ALLOW: dispatchAddress,
      BROKER_AIRLOCK_CLUSTER_RANGES: CLUSTER_RANGES,
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
      ...(options.grantSeconds === undefined
        ? {}
        : { BROKER_RESULT_GRANT_TEST_SECONDS: String(options.grantSeconds) }),
    };
    const brokerLogs: string[] = [];
    for (let n = 1; n <= BROKER_COUNT; n++) {
      const log = path.join(work, `broker${n}.log`);
      brokerLogs.push(log);
      brokers.push(spawnLogged([path.join(bin, "cipherd")], brokerEnv, log));
    }
    const apiAddresses: string[] = [];
    const proxyAddresses: string[] = [];
    const thumbprints: string[] = [];
    for (const [i, broker] of brokers.entries()) {
      const log = brokerLogs[i] ?? "";
      await awaitReady(`secrets broker ${i + 1}`, broker, log, BROKER_READY, async (signal) => {
        const api = loggedLine(log, "broker listening");
        const proxy = loggedLine(log, "airlock proxy listening");
        const key = loggedLine(log, DPOP_KEY_LINE);
        if (
          api === undefined ||
          proxy === undefined ||
          key === undefined ||
          loggedLine(log, "token exchange", { state: "on" }) === undefined
        ) {
          return false;
        }
        const healthy = await fetch(`http://${String(api.addr)}/healthz`, {
          signal: AbortSignal.any([signal, AbortSignal.timeout(2_000)]),
        });
        if (!healthy.ok) return false;
        apiAddresses[i] = String(api.addr);
        proxyAddresses[i] = String(proxy.addr);
        thumbprints[i] = String(key.thumbprint);
        return true;
      });
    }
    // The fronts learn their backends: each rereads its file when it changes.
    writeFileSync(apiFront.backends, `${apiAddresses.join("\n")}\n`);
    writeFileSync(proxyFront.backends, `${proxyAddresses.join("\n")}\n`);
    await awaitReady("the API front", apiFront.child, apiFront.log, BROKER_READY, async () => {
      const response = await fetch(`${brokerUrl}/healthz`, { signal: AbortSignal.timeout(2_000) });
      return response.ok;
    });
    brokerDatabase = new SQL(databaseUrl.toString());
    const store = brokerDatabase;

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
    // The project's default repository, so its issues resolve to the repository a job's run is
    // of: the issue's results route takes a job's result only for such an issue.
    await asPerson("PUT", `/api/v1/projects/${PROJECT}/default-repository`, { repo: REPOSITORY });
    const filed = (await asPerson("POST", "/api/v1/issues", {
      project: PROJECT,
      title: "A change a CI job proves",
      spec: "The server builds at the head of the change's branch.\n",
    })) as { key: string };
    await asPerson("POST", `/api/v1/issues/${filed.key}/artifacts`, {
      name: "verification.md",
      content: VERIFICATION,
    });

    // One workflow run for each push of a ref at a commit, as GitHub numbers them: the attempts
    // and the legs of one push share its run.
    const runIds = new Map<string, number>();
    return {
      agentSecrets,
      dispatch,
      brokerUrl,
      proxyAddress: proxyFront.address,
      thumbprints,
      github,
      project: PROJECT,
      repository: REPOSITORY,
      person: PERSON,
      issue: filed.key,
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
          DISPATCH_URL: DISPATCH_PUBLIC_URL,
          AGENT_SECRETS_URL: brokerUrl,
          AGENT_SECRETS_MACHINE_IDENTITY: "github-actions",
          AGENT_SECRETS_MACHINE_AUDIENCE: ACTIONS_AUDIENCE,
          ACTIONS_ID_TOKEN_REQUEST_URL: request.requestUrl,
          ACTIONS_ID_TOKEN_REQUEST_TOKEN: request.requestToken,
        };
      },
      jobCommand(argv: readonly string[]): string[] {
        return [agentSecrets, "result-grant", "--", ...argv];
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
      setBranch(branch: string, sha: string): void {
        github.setBranch(REPOSITORY, branch, sha);
      },
      async results(commit: string): Promise<StoredResult[]> {
        const read = (await asPerson(
          "GET",
          `/api/v1/projects/${PROJECT}/results?commit=${commit}`
        )) as { results: StoredResult[] };
        return read.results;
      },
      logMark(): LogMark {
        return { brokers: brokerLogs.map(wholeLinesEnd), dispatch: wholeLinesEnd(dispatchLog) };
      },
      linesSince(mark: LogMark): LogLines {
        return {
          brokers: brokerLogs.map((log, i) =>
            records(linesAfter(log, mark.brokers[i] ?? 0).join("\n"))
          ),
          access: linesAfter(dispatchLog, mark.dispatch).filter((line) =>
            line.includes(ACCESS_LINE)
          ),
        };
      },
      async endResultGrants(checkRunId: number): Promise<number> {
        const ended =
          await store`update result_grants set expires_at = now() - interval '1 second' where extra->'ci'->>'check_run_id' = ${String(checkRunId)}`;
        return ended.count;
      },
      async resultTokenBindings(checkRunId: number): Promise<string[]> {
        const rows: { jkt: string | null }[] =
          await store`select request->'session'->>'jwk_thumbprint' as jkt from oauth_access_tokens where request->'session'->'extra'->'ci'->>'check_run_id' = ${String(checkRunId)}`;
        return rows.map((row) => row.jkt ?? "");
      },
      async introspect(token: string): Promise<Introspection> {
        const response = await fetch(`${brokerUrl}/v1/oauth/introspect`, {
          method: "POST",
          headers: {
            Authorization: `Bearer ${readFileSync(accountToken, "utf8").trim()}`,
            "Content-Type": "application/x-www-form-urlencoded",
          },
          body: new URLSearchParams({ token }).toString(),
        });
        const text = await response.text();
        if (!response.ok) throw new Error(`introspection: ${response.status} ${text}`);
        return IntrospectionSchema.parse(JSON.parse(text));
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
    console.log(
      `# Two brokers behind the API front ${stack.brokerUrl} and the proxy front ${stack.proxyAddress}; Dispatch at ${stack.dispatch.url}, reached by the job as ${DISPATCH_PUBLIC_URL} through the proxy alone.`
    );
    console.log("# The try line: one stored result, then a session's read of it:");
    console.log(`${result} --check ${PROBE_CHECK} --commit ${tested}`);
    console.log(
      `env -u AGENT_SECRETS_URL DISPATCH_URL=${stack.dispatch.url} DISPATCH_HOST=omp DISPATCH_SESSION_ID=ci-results-try DISPATCH_TOKEN=${shellWord(stack.dispatch.token)} ${CLI} results --project ${stack.project} --commit ${tested}`
    );
    // A run of the same job on a push of the issue's own branch, whose head is the tested commit.
    const branch = `legion/${stack.issue}`;
    stack.setBranch(branch, tested);
    const onBranch = stack.jobEnv({ ...run, ref: `refs/heads/${branch}`, checkRunId: 3 });
    console.log(
      `# The issue's try line: the job's result of ${CI_CHECK}, which its ci_check names, on a push of ${branch}, then a session's read of it:`
    );
    console.log(
      `ACTIONS_ID_TOKEN_REQUEST_TOKEN=${shellWord(onBranch.ACTIONS_ID_TOKEN_REQUEST_TOKEN ?? "")} ${shellWord(stack.agentSecrets)} result-grant -- ${CLI} result --issue ${stack.issue} --check ${CI_CHECK} --verdict pass --commit ${tested}`
    );
    console.log(
      `env -u AGENT_SECRETS_URL DISPATCH_URL=${stack.dispatch.url} DISPATCH_HOST=omp DISPATCH_SESSION_ID=ci-results-try DISPATCH_TOKEN=${shellWord(stack.dispatch.token)} ${CLI} results --issue ${stack.issue}`
    );
    console.log("# Each control is refused:");
    console.log(`${result} --check ${PROBE_CHECK} --commit ${other}   # another commit`);
    console.log(`${result} --check ${OTHER_JOBS_CHECK} --commit ${tested}   # another job`);
    console.log(
      `${shellWord(stack.agentSecrets)} result-grant -- env NO_PROXY='*' no_proxy='*' ${CLI} result --project ${stack.project} --verdict pass --check ${PROBE_CHECK} --commit ${tested}   # around the proxy`
    );
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
