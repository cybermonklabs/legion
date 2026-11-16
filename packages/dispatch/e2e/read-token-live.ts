/** `bun e2e/read-token-live.ts`: the live proof that a read-only token a running secrets broker
 *  mints reads a running Dispatch's architecture route and nothing else. A read entry's pod token
 *  (BROKER_EXCHANGE_READ_ACCOUNTS) is exchanged at the broker for a token that holds the scope
 *  `read` alone and names no person and no project; Dispatch serves it on the routes its read list
 *  names (`readScopeRoutes`, packages/envoy/internal/dispatch/api/routes_table.go) and answers it
 *  403 READ_ONLY_TOKEN on every other one. The example entry's token, which writes its project,
 *  then sends the same write and is answered 2xx, so the refusal is the scope's and not the
 *  write's.
 *
 *  Run it from packages/dispatch once two stacks are up on this machine:
 *
 *  1. The broker: `bash ../envoy/scripts/dev-broker.sh`, with
 *     `DEV_BROKER_POSTGRES_URL=postgres://<role>@<host>:<port>/<database>` where there is no
 *     Docker. Its banner prints `AGENT_SECRETS_URL`, `AGENT_SECRETS_ACCOUNT_TOKEN_FILE` and
 *     `DEV_BROKER_DIR`, which holds `read-token` (the read entry's pod token) and `exchange-token`
 *     (the example entry's, a pod the script registered under `example-worker-session`).
 *  2. Dispatch, reading that broker: `DISPATCH_E2E_AGENT_SECRETS_URL=$AGENT_SECRETS_URL
 *     DISPATCH_E2E_AGENT_SECRETS_ACCOUNT_TOKEN_FILE=$AGENT_SECRETS_ACCOUNT_TOKEN_FILE
 *     bun run dispatch:local`, with `DATABASE_URL=<a loopback database of its own>` where there is
 *     no Docker. Its "Run a spec against it" line prints the variables a command reaches it by;
 *     this script reads `DISPATCH_E2E_PORT` and `FAKE_GITHUB_PORT` of them.
 *  3. `<the variables that line prints> AGENT_SECRETS_URL=<the broker> DEV_BROKER_DIR=<its
 *     workdir> bun e2e/read-token-live.ts`.
 *
 *  It writes into that Dispatch: project EXAMPLE, which is the example entry's project so that
 *  entry's token may write there; an architecture source of EXAMPLE on the fake GitHub, whose
 *  repositories it replaces, signed in as alice as `seedArchitecture` in e2e/architecture.e2e.ts
 *  seeds CORE's; an issue of EXAMPLE; and the example entry's comment on it. Project EXAMPLE then
 *  exists, so a second run needs `bun run dispatch:local` restarted, which empties the instance.
 *
 *  It prints one line per request with the status and the code Dispatch answered, and exits 1
 *  when any answer is not the one it wants, or when a step before them fails. */
import { randomUUID } from "node:crypto";
import { join } from "node:path";
import { DISPATCH_READ_ONLY_TOKEN, IDEMPOTENCY_KEY_HEADER } from "@legion/contracts";
import type { RequestCredential } from "@legion/envoy-client/bearer";
import { brokerCredential } from "@legion/envoy-client/broker-credential";

import {
  baseUrl,
  createIssue,
  createProject,
  listComments,
  putArchitectureSource,
  syncArchitectureSource,
} from "./api";
import { harnessPorts } from "./harness-ports";

/** The example entry's project (dev-broker.sh's EXCHANGE_PROJECT): its named token writes it. */
const project = "EXAMPLE";
/** The session the read entry fixes (dev-broker.sh's READ_SESSION). */
const readSession = "example-reader-session";
/** The session dev-broker.sh registered the example entry's pod under (EXCHANGE_SESSION). */
const namedSession = "example-worker-session";
/** The repository of EXAMPLE's architecture source, which the fake GitHub serves. */
const architectureRepo = "example/arch";
/** The component model it serves: one component inside another, enough for a tree. */
const model = {
  "platform.md": "---\ntitle: Platform\n---\nEverything that runs.\n",
  "reader.md": "---\ntitle: Reader\nparent: platform\n---\nWhat a read-only token reads.\n",
};
/** The comment both tokens send, this run's own, so the count alice reads is of this run's. */
const commentBody = `Written by read-token-live.ts, run ${randomUUID()}.`;

/** `name`'s value, trimmed, or a refusal that says where it comes from. */
function required(name: string, from: string): string {
  const value = (process.env[name] ?? "").trim();
  if (value === "") throw new Error(`${name} is required: ${from}`);
  return value;
}

/** The broker credential of a pod whose projected token is `tokenFile`, trading under `session`.
 *  The environment is this script's own and carries nothing of the shell's, and the person
 *  trade is off, so a sign-in kept under this machine's HOME is never traded in the pod's place. */
function podCredential(brokerUrl: string, tokenFile: string, session: string): RequestCredential {
  return brokerCredential(
    {
      AGENT_SECRETS_URL: brokerUrl,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
      PERSON_SIGNIN_TRADE: "off",
    },
    session
  );
}

/** What Dispatch answered one request: its status and its error code, "" for an answer with
 *  none. */
interface Answer {
  readonly status: number;
  readonly code: string;
}

/** Sends `method path` to Dispatch under `credential`, with a DPoP proof for that very URL. */
async function present(
  credential: RequestCredential,
  method: string,
  path: string,
  body?: object,
  headers: Readonly<Record<string, string>> = {}
): Promise<Answer> {
  const url = new URL(path, baseUrl).toString();
  const response = await fetch(url, {
    body: body === undefined ? undefined : JSON.stringify(body),
    headers: {
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      ...headers,
      ...(await credential.headers(method, url)),
    },
    method,
  });
  const text = await response.text();
  let code = "";
  try {
    const parsed = JSON.parse(text) as { code?: unknown };
    if (typeof parsed?.code === "string") code = parsed.code;
  } catch {
    // An answer that is not JSON carries no code.
  }
  return { code, status: response.status };
}

/** The answer a request must get: a status, or any 2xx, and the code where one is wanted. */
interface Want {
  readonly status: number | "2xx";
  readonly code?: string;
}

let mismatches = 0;

/** Prints one line, what was asked, what came back and what was wanted, and counts it when it
 *  is not what was wanted. */
function report(what: string, ok: boolean, got: string, wanted: string): void {
  if (!ok) mismatches++;
  console.log(`${ok ? "ok  " : "FAIL"} ${what}: ${got} (want ${wanted})`);
}

/** Reports one request's answer against `want`. */
function answered(what: string, answer: Answer, want: Want): void {
  const statusOk =
    want.status === "2xx"
      ? answer.status >= 200 && answer.status < 300
      : answer.status === want.status;
  report(
    what,
    statusOk && (want.code === undefined || answer.code === want.code),
    `${answer.status} ${answer.code || "-"}`,
    `${want.status}${want.code === undefined ? "" : ` ${want.code}`}`
  );
}

async function main(): Promise<void> {
  if (process.env.PLAYWRIGHT_BASE_URL) {
    throw new Error(
      "unset PLAYWRIGHT_BASE_URL: this script drives the harness `bun run dispatch:local` started"
    );
  }
  const ports = harnessPorts();
  for (const port of [ports.dispatch, ports.fakeGithub]) {
    if (port.port === 0) {
      throw new Error(
        `${port.variable} is required: the port \`bun run dispatch:local\` prints for it`
      );
    }
  }
  const brokerUrl = required("AGENT_SECRETS_URL", "the broker dev-broker.sh's banner exports");
  const brokerDir = required("DEV_BROKER_DIR", "the workdir dev-broker.sh's banner exports");

  // The architecture source, as seedArchitecture seeds CORE's: the project, the fake GitHub's
  // repository, the source and its sync, all signed in as alice. Then the issue both tokens write
  // a comment on.
  await createProject({ key: project, name: "Example" });
  const seeded = await fetch(`http://127.0.0.1:${ports.fakeGithub.port}/__fixture/repos`, {
    body: JSON.stringify({
      [architectureRepo]: { contents: "read", files: model, installation_id: 301 },
    }),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
  if (!seeded.ok) throw new Error(`seed fake github: ${seeded.status} ${await seeded.text()}`);
  await putArchitectureSource(project, { branch: "main", repo: architectureRepo });
  const source = await syncArchitectureSource(project, architectureRepo);
  if (source.last_error !== null) {
    throw new Error(`the sync of ${architectureRepo} failed: ${source.last_error}`);
  }
  const issue = await createIssue({ project, title: "Read by a read-only token" });
  console.log(`seeded ${project}: architecture source ${architectureRepo}, issue ${issue.key}`);

  const readOnly = podCredential(brokerUrl, join(brokerDir, "read-token"), readSession);
  const named = podCredential(brokerUrl, join(brokerDir, "exchange-token"), namedSession);
  const architecture = `/api/v1/projects/${project}/architecture`;
  const comments = `/api/v1/issues/${issue.key}/comments`;
  // One key for both POSTs, as one client sending the same write twice would. A key belongs to its
  // caller (requestKeyLogin, internal/dispatch/api), so the named token's write runs under it
  // whatever the refused request did.
  const comment = { body: commentBody };
  const keyed = { [IDEMPOTENCY_KEY_HEADER]: `read-token-live-${randomUUID()}` };
  const refused: Want = { code: DISPATCH_READ_ONLY_TOKEN, status: 403 };

  answered(`GET ${architecture} [read-only token]`, await present(readOnly, "GET", architecture), {
    status: 200,
  });
  answered(
    `POST ${comments} [read-only token]`,
    await present(readOnly, "POST", comments, comment, keyed),
    refused
  );
  // The refused write wrote nothing: alice reads no comment with this run's body on the issue.
  const written = (await listComments(issue.key)).filter((row) => row.body === commentBody);
  report(
    `GET ${comments} [alice]`,
    written.length === 0,
    `${written.length} comment(s) with this run's body`,
    "0"
  );
  answered(
    `GET /api/v1/issues/${issue.key} [read-only token]`,
    await present(readOnly, "GET", `/api/v1/issues/${issue.key}`),
    refused
  );
  answered(
    `POST ${comments} [named-project token]`,
    await present(named, "POST", comments, comment, keyed),
    { status: "2xx" }
  );
}

let exitCode = 0;
try {
  await main();
  if (mismatches > 0) {
    console.error(`read-token-live: ${mismatches} answer(s) were not the ones wanted`);
    exitCode = 1;
  }
} catch (error) {
  console.error(
    `read-token-live failed: ${error instanceof Error ? error.message : String(error)}`
  );
  exitCode = 1;
}
process.exit(exitCode);
