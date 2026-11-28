/** `bun e2e/job-token-live.ts`: the live proof that a job's token a running secrets broker mints
 *  writes to a running Dispatch under the job's own identity and no person's, through
 *  `dispatch api`: `--path <route>` for a GET, and `--method POST` with `--body-file` for a write,
 *  its body read from a file or from standard input. A job entry (BROKER_EXCHANGE_JOB_ACCOUNTS,
 *  BROKER_EXCHANGE_JOB_WORKFLOWS) names an identity, its projects and a fixed session, and no
 *  person; the broker's exchange answers that identity's own token with a token whose service is
 *  the identity, and Dispatch records each write of it under the session with that service and no
 *  owner. A route that needs an OAuth scope refuses a job's token, which holds none, with
 *  `403 OAUTH_SCOPE_REQUIRED` before Dispatch's project check runs. That check judges a job's
 *  token of one project, where it writes another project, as it judges a person's named token
 *  for the same project: both write a public project's objects as a token that names no project
 *  does. The example entry's token, which names a person and EXAMPLE, is the control: whoami
 *  names that person as its owner, so the read tells a person's token from a job's, and its
 *  answer to the job pod's creation in OPS is the one the job pod's must equal.
 *
 *  Run it from packages/dispatch once two stacks are up on this machine:
 *
 *  1. The broker: `bash ../envoy/scripts/dev-broker.sh`, with
 *     `DEV_BROKER_POSTGRES_URL=postgres://<role>@<host>:<port>/<database>` where there is no
 *     Docker. Its banner prints `AGENT_SECRETS_URL`, `AGENT_SECRETS_ACCOUNT_TOKEN_FILE` and
 *     `DEV_BROKER_DIR`, which holds `job-token` (the job pod entry's pod token),
 *     `actions-request-token` (the request token of the job workflow's run, whose request URL the
 *     issuer's log there, `issuer.log`, names, as the banner's `actions request URL` line does) and
 *     `exchange-token` (the example entry's, a pod the script registered under
 *     `example-worker-session`).
 *  2. Dispatch, reading that broker: `DISPATCH_E2E_AGENT_SECRETS_URL=$AGENT_SECRETS_URL
 *     DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$AGENT_SECRETS_ACCOUNT_TOKEN_FILE
 *     bun run dispatch:local`, with `DATABASE_URL=<a loopback database of its own>` where there is
 *     no Docker. Its "Run a spec against it" line prints the variables a command reaches it by;
 *     this script reads `DISPATCH_E2E_PORT` of them.
 *  3. `<the variables that line prints> AGENT_SECRETS_URL=<the broker> DEV_BROKER_DIR=<its
 *     workdir> bun e2e/job-token-live.ts`.
 *
 *  Each request is this checkout's `dispatch api` (packages/envoy-client/bin/dispatch.ts), run in an
 *  environment that keeps PATH, HOME and TMPDIR alone and sets DISPATCH_URL to this Dispatch,
 *  AGENT_SECRETS_URL to this broker, PERSON_SIGNIN_TRADE=off and the step's identity, so no request
 *  reaches another Dispatch or broker, and no person's sign-in kept under HOME is traded in a job's
 *  place. The steps:
 *
 *  1. The job pod (AGENT_SECRETS_MACHINE_IDENTITY=kubernetes, its token `job-token`): whoami names
 *     no owner, the pod's account as the service, the entry's session and project EXAMPLE; it
 *     creates an issue in EXAMPLE, the body on standard input, whose events record the creation
 *     under that session with that service and no owner; the same creation in OPS, a public
 *     project, gets the answer that the control of step 3, a person's named token for EXAMPLE,
 *     gets for it there: exit 0, an issue in OPS for each.
 *  2. The job workflow's run (AGENT_SECRETS_MACHINE_IDENTITY=github-actions, the request URL and
 *     request token of the issuer that stands in for GitHub, AGENT_SECRETS_MACHINE_AUDIENCE the
 *     broker's BROKER_EXCHANGE_GITHUB_AUDIENCE, and no DISPATCH_HOST and no session id): it comments
 *     on that issue, the body read from a file, and whoami names no owner and the workflow as the
 *     service.
 *  3. The control (the example entry's `exchange-token`, under the session the script registered
 *     its pod for): whoami names that entry's person as the owner. Its creation in OPS is step 1's.
 *
 *  It writes into that Dispatch: project EXAMPLE, signed in as alice, as read-token-live.ts does,
 *  since dispatch:local seeds only CORE and OPS; the job pod's issue in EXAMPLE; the workflow's
 *  comment on it; and the two issues of step 1 in OPS. Project EXAMPLE then exists, so a second
 *  run needs `bun run dispatch:local` restarted, which empties the instance.
 *
 *  It prints one line per request, with `dispatch api`'s exit status and the code Dispatch answered
 *  (the control's creation in OPS on the job pod's line for OPS, as the answer that line wants),
 *  and exits 1 when any answer is not the one it wants, or when a step before them fails. */
import { randomUUID } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { baseUrl, createProject } from "./api";
import { dispatchEntry } from "./dispatch-cli";
import { harnessPorts } from "./harness-ports";

/** The project the job pod entry and the example entry name (dev-broker.sh's EXCHANGE_PROJECT). */
const project = "EXAMPLE";
/** A project dispatch:local seeds that neither the job pod's entry nor the example entry names. */
const otherProject = "OPS";
/** The job pod entry (dev-broker.sh's JOB_ACCOUNT and JOB_SESSION). */
const jobAccount = "system:serviceaccount:example-ns:example-job";
const jobSession = "example-job-session";
/** The job workflow entry's service, <repository>/<workflow path>@<ref> (JOB_WORKFLOW_RUN), and the
 *  audience its run asks for (WORKFLOW_AUDIENCE, the broker's BROKER_EXCHANGE_GITHUB_AUDIENCE). */
const workflowService =
  "example-org/example-repo/.github/workflows/example-job.yaml@refs/heads/main";
const workflowAudience = "legion-broker-github";
/** The example entry's person (APPROVER_LOGIN) and the session its pod is registered under
 *  (EXCHANGE_SESSION), which that entry does not fix. */
const person = "ada@example.com";
const personSession = "example-worker-session";
/** How long one `dispatch api` run may take, its broker trade included. */
const RUN_DEADLINE_MS = 60_000;

/** `name`'s value, trimmed, or a refusal that says where it comes from. */
function required(name: string, from: string): string {
  const value = (process.env[name] ?? "").trim();
  if (value === "") throw new Error(`${name} is required: ${from}`);
  return value;
}

/** The request URL the issuer logged for the run whose request token is `tokenFile`:
 *  `dev-oidc-issuer: <file> holds the request token of <run>'s Actions run (request URL <URL>)`. */
function actionsRequestUrl(issuerLog: string, tokenFile: string): string {
  const prefix = `dev-oidc-issuer: ${tokenFile} holds the request token of `;
  for (const line of readFileSync(issuerLog, "utf-8").split("\n")) {
    const url = line.startsWith(prefix) ? /\(request URL (\S+)\)$/.exec(line)?.[1] : undefined;
    if (url !== undefined) return url;
  }
  throw new Error(`${issuerLog} names no request URL for ${tokenFile}`);
}

/** What one `dispatch api` run did: its exit status, the body it printed, and the code Dispatch
 *  answered (the `<CODE>` of the `<CODE>: <error>` line the command prints for Dispatch's refusal,
 *  "-" for an answer it took, and the command's own last line in parentheses for any other end). */
interface Ran {
  readonly exit: number;
  readonly body: string;
  readonly code: string;
}

/** Runs `dispatch api <args>` with `identity`'s variables, `stdin` on standard input. */
async function dispatchApi(
  brokerUrl: string,
  identity: Readonly<Record<string, string>>,
  args: readonly string[],
  stdin?: string
): Promise<Ran> {
  const kept: Record<string, string> = {};
  for (const name of ["PATH", "HOME", "TMPDIR"]) {
    const value = process.env[name];
    if (value !== undefined) kept[name] = value;
  }
  const child = Bun.spawn([process.execPath, dispatchEntry, "api", ...args], {
    env: {
      ...kept,
      DISPATCH_URL: baseUrl,
      AGENT_SECRETS_URL: brokerUrl,
      PERSON_SIGNIN_TRADE: "off",
      ...identity,
    },
    stdin: stdin === undefined ? "ignore" : Buffer.from(stdin),
    stdout: "pipe",
    stderr: "pipe",
    timeout: RUN_DEADLINE_MS,
  });
  const [body, stderr, exit] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  // The command prints Dispatch's refusal on standard output, as it prints every line of its own.
  const refused = /^([A-Z][A-Z0-9_]*): /.exec(body)?.[1];
  const said = (body.trim() || stderr.trim()).split("\n").at(-1) ?? "";
  return { body, code: exit === 0 ? "-" : (refused ?? `(${said})`), exit };
}

let mismatches = 0;

/** Prints one line, what was asked, what came back and what was wanted, and counts it when it
 *  is not what was wanted. */
function report(what: string, ok: boolean, got: string, wanted: string): void {
  if (!ok) mismatches++;
  console.log(`${ok ? "ok  " : "FAIL"} ${what}: ${got} (want ${wanted})`);
}

/** The JSON object `text` holds, or an empty one. */
function parsed(text: string): Record<string, unknown> {
  try {
    const value: unknown = JSON.parse(text);
    return typeof value === "object" && value !== null && !Array.isArray(value)
      ? (value as Record<string, unknown>)
      : {};
  } catch {
    return {};
  }
}

/** Reports one whoami run against the owner, service, session and project it must name; a field
 *  `want` leaves out is printed and not compared. */
function whoami(
  what: string,
  ran: Ran,
  want: { owner: string | null; service?: string | null; session?: string; project?: string }
): void {
  const answer = parsed(ran.body);
  const fields = ["owner", "service", "session", "project"] as const;
  const ok =
    ran.exit === 0 &&
    answer.kind === "agent" &&
    fields.every((field) => want[field] === undefined || answer[field] === want[field]);
  report(
    `${what} GET /api/v1/whoami`,
    ok,
    `exit ${ran.exit} ${ran.code}, ${fields.map((field) => `${field} ${String(answer[field])}`).join(", ")}`,
    `exit 0 -, ${fields
      .filter((field) => want[field] !== undefined)
      .map((field) => `${field} ${String(want[field])}`)
      .join(", ")}`
  );
}

async function main(work: string): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error(
      "unset PLAYWRIGHT_BASE_URL: this script drives the harness `bun run dispatch:local` started"
    );
  }
  const { dispatch } = harnessPorts();
  if (dispatch.port === 0) {
    throw new Error(
      `${dispatch.variable} is required: the port \`bun run dispatch:local\` prints for it`
    );
  }
  const brokerUrl = required("AGENT_SECRETS_URL", "the broker dev-broker.sh's banner exports");
  const brokerDir = required("DEV_BROKER_DIR", "the workdir dev-broker.sh's banner exports");
  const requestTokenFile = join(brokerDir, "actions-request-token");
  const requestUrl = actionsRequestUrl(join(brokerDir, "issuer.log"), requestTokenFile);
  const requestToken = readFileSync(requestTokenFile, "utf-8").trim();

  await createProject({ key: project, name: "Example" });
  console.log(`seeded ${project}`);

  // The control: the example entry's token, which names a person and EXAMPLE. Step 1 compares the
  // job pod's creation in OPS with its own, and step 3 reads its whoami.
  const control = {
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: join(brokerDir, "exchange-token"),
    DISPATCH_SESSION_ID: personSession,
  };

  // 1. The job pod.
  const pod = {
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: join(brokerDir, "job-token"),
  };
  whoami("[job pod]", await dispatchApi(brokerUrl, pod, ["--path", "/api/v1/whoami"]), {
    owner: null,
    service: jobAccount,
    session: jobSession,
    project,
  });
  const spec = "# Spec\n\nFiled by job-token-live.ts.";
  const created = await dispatchApi(
    brokerUrl,
    pod,
    ["--method", "POST", "--path", "/api/v1/issues", "--body-file", "-"],
    JSON.stringify({ project, spec, title: `Filed by a job pod in ${project}` })
  );
  const key = parsed(created.body).key;
  report(
    `[job pod] POST /api/v1/issues (${project}, body on standard input)`,
    created.exit === 0 && typeof key === "string",
    `exit ${created.exit} ${created.code}, key ${String(key)}`,
    "exit 0 -, a key"
  );
  if (typeof key !== "string") throw new Error(`the job pod created no issue: ${created.body}`);

  const listed = await dispatchApi(brokerUrl, pod, ["--path", `/api/v1/issues/${key}/events`]);
  let events: unknown;
  try {
    events = JSON.parse(listed.body);
  } catch {
    events = undefined;
  }
  const creation = (Array.isArray(events) ? events : [])
    .map((event) => event as { type?: unknown; actor?: Record<string, unknown> })
    .find((event) => event.type === "issue.created");
  const actor = creation?.actor ?? {};
  report(
    `[job pod] GET /api/v1/issues/${key}/events`,
    listed.exit === 0 &&
      actor.kind === "session" &&
      actor.id === jobSession &&
      actor.service === jobAccount &&
      actor.owner === undefined,
    `exit ${listed.exit} ${listed.code}, issue.created by ${String(actor.kind)}:${String(actor.id)}, service ${String(actor.service)}, owner ${actor.owner === undefined ? "none" : String(actor.owner)}`,
    `exit 0 -, issue.created by session:${jobSession}, service ${jobAccount}, owner none`
  );

  // The same creation in OPS, under the job pod's token and under the control's: the job's token
  // gets the answer a person's named token for EXAMPLE gets there.
  const elsewhere = await dispatchApi(
    brokerUrl,
    pod,
    ["--method", "POST", "--path", "/api/v1/issues", "--body-file", "-"],
    JSON.stringify({ project: otherProject, spec, title: `Filed by a job pod in ${otherProject}` })
  );
  const personElsewhere = await dispatchApi(
    brokerUrl,
    control,
    ["--method", "POST", "--path", "/api/v1/issues", "--body-file", "-"],
    JSON.stringify({
      project: otherProject,
      spec,
      title: `Filed by the example entry in ${otherProject}`,
    })
  );
  report(
    `[job pod] POST /api/v1/issues (${otherProject}, body on standard input)`,
    elsewhere.exit === personElsewhere.exit && elsewhere.code === personElsewhere.code,
    `exit ${elsewhere.exit} ${elsewhere.code}`,
    `the person entry's: exit ${personElsewhere.exit} ${personElsewhere.code}`
  );

  // 2. The job workflow's run, with no DISPATCH_HOST and no session id.
  const workflow = {
    AGENT_SECRETS_MACHINE_IDENTITY: "github-actions",
    ACTIONS_ID_TOKEN_REQUEST_URL: requestUrl,
    ACTIONS_ID_TOKEN_REQUEST_TOKEN: requestToken,
    AGENT_SECRETS_MACHINE_AUDIENCE: workflowAudience,
  };
  const commentFile = join(work, "comment.json");
  writeFileSync(
    commentFile,
    JSON.stringify({ body: `Written by job-token-live.ts, run ${randomUUID()}.` })
  );
  const commented = await dispatchApi(brokerUrl, workflow, [
    "--method",
    "POST",
    "--path",
    `/api/v1/issues/${key}/comments`,
    "--body-file",
    commentFile,
  ]);
  report(
    `[job workflow] POST /api/v1/issues/${key}/comments (body from a file)`,
    commented.exit === 0,
    `exit ${commented.exit} ${commented.code}`,
    "exit 0 -"
  );
  whoami("[job workflow]", await dispatchApi(brokerUrl, workflow, ["--path", "/api/v1/whoami"]), {
    owner: null,
    service: workflowService,
  });

  // 3. The control.
  whoami("[person entry]", await dispatchApi(brokerUrl, control, ["--path", "/api/v1/whoami"]), {
    owner: person,
  });
}

let exitCode = 0;
const work = mkdtempSync(join(tmpdir(), "job-token-live-"));
try {
  await main(work);
  if (mismatches > 0) {
    console.error(`job-token-live: ${mismatches} answer(s) were not the ones wanted`);
    exitCode = 1;
  }
} catch (error) {
  console.error(`job-token-live failed: ${error instanceof Error ? error.message : String(error)}`);
  exitCode = 1;
} finally {
  rmSync(work, { force: true, recursive: true });
}
process.exit(exitCode);
