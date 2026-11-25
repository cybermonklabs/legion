// packages/envoy/scripts/approve/session.ts
//
// The agent session of scripts/approve-check.sh, on the stack `dev-stack.sh --approve` stands up:
// a session on a box where a person signed in (`agent-secrets login`), whose Dispatch credential is
// the broker credential the TypeScript client builds from this process's environment. Each command
// is one step of the check and prints one JSON line of what it saw, never a token or a placeholder.
//
// Dispatch serves under https://<the approve host> (DISPATCH_URL). On the local stack it listens on
// loopback (APPROVE_CHECK_DISPATCH_LOCAL). A request through Cipher's proxy goes to the public URL,
// which the proxy terminates and sends on to Dispatch's own address; a direct request is signed for
// the public URL, as every request of a box is in production, and sent to the loopback address,
// which stands in for the name resolving there. With APPROVE_CHECK_DISPATCH_LOCAL unset, the run is
// on a box (k31 Part 1), and every request goes to the public URL itself.
//
//   setup                         as the person: a project, an issue and its spec, saved to the state file
//   approve                       the spec's version reviewed approved through the proxy (reviewArtifact)
//   approve-then-revoked          the same review twice on one approve route, the sign-in revoked between
//   box <method> <path> [json]    a request with the box's own Dispatch token, direct
//   proxy <method> <path> [json]  a request with the approve placeholder, through the proxy
//   reviews                       the spec's reviews, read with the box's token
//   comments                      the issue's comments, read with the box's token
//   agent-tokens                  the person's agent tokens, read with the box's token
//
// Step 1e's scan of what the session left on the box is scripts/approve/scan.ts, a file of its own,
// which loads none of the envoy-client modules this file runs the session with.

import { readFileSync, writeFileSync } from "node:fs";
import type { ApproveRoute } from "../../../envoy-client/src/bearer";
import { brokerCredential } from "../../../envoy-client/src/broker-credential";
import { DispatchClient } from "../../../envoy-client/src/dispatch-http";

const env = process.env as Record<string, string | undefined>;

function setting(name: string): string {
  const value = (env[name] ?? "").trim();
  if (value === "") fail(`${name} is unset`);
  return value;
}

function fail(message: string): never {
  process.stderr.write(`approve-session: ${message}\n`);
  process.exit(2);
}

function say(answer: Record<string, unknown>): void {
  process.stdout.write(`${JSON.stringify(answer)}\n`);
}

const PUBLIC = setting("DISPATCH_URL").replace(/\/+$/, "");
const LOCAL = (env["APPROVE_CHECK_DISPATCH_LOCAL"] ?? "").trim().replace(/\/+$/, "");

/** A fetch that sends a request of the public URL to Dispatch's loopback address, and one through
 *  a proxy as it is: the proxy itself reaches Dispatch's own address. */
const localFetch = Object.assign(
  async (input: Parameters<typeof fetch>[0], init?: RequestInit & { proxy?: string }) => {
    const url = String(input);
    if (LOCAL === "" || init?.proxy !== undefined || !url.startsWith(PUBLIC)) {
      return await fetch(input, init);
    }
    return await fetch(LOCAL + url.slice(PUBLIC.length), init);
  },
  { preconnect: () => {} }
) as typeof fetch;

const credential = brokerCredential(env);

/** The state `setup` wrote: the issue, its spec's artifact and the version reviewed. */
interface State {
  readonly issue: string;
  readonly artifact: string;
  readonly version: number;
}

function state(): State {
  return JSON.parse(readFileSync(setting("APPROVE_CHECK_STATE"), "utf8")) as State;
}

/** What one answer was: its status, Dispatch's or the proxy's code, and the proxy's own headers. */
async function described(answer: Response): Promise<Record<string, unknown>> {
  const text = await answer.text();
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    body = undefined;
  }
  const record = (typeof body === "object" && body !== null ? body : {}) as Record<string, unknown>;
  return {
    status: answer.status,
    code: record["code"] ?? null,
    proxy_code: answer.headers.get("Cipher-Proxy-Code"),
    forwarded: answer.headers.get("Cipher-Proxy-Forwarded"),
    // Dispatch's error, or the proxy's own refusal, which is plain text naming its rule.
    error: (typeof record["error"] === "string" ? String(record["error"]) : text.trim()).slice(
      0,
      300
    ),
    body: record,
  };
}

/** A request with the box's own Dispatch token, signed for the public URL, sent direct. */
async function box(method: string, path: string, body?: unknown): Promise<Response> {
  const url = PUBLIC + path;
  const headers = await credential.headers(method, url);
  return await localFetch(url, {
    method,
    headers: {
      ...headers,
      Accept: "application/json",
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
}

async function approveRoute(): Promise<ApproveRoute> {
  const route = await credential.approveRoute?.();
  if (route === undefined)
    fail("the credential has no approve route: no person is signed in on this box");
  return route;
}

/** A request with the approve placeholder, through the proxy the route names. */
async function proxied(
  route: ApproveRoute,
  method: string,
  path: string,
  body?: unknown
): Promise<Response> {
  const proxy = new URL(route.proxyUrl);
  proxy.username = "agent";
  proxy.password = route.placeholder;
  const init: RequestInit & { proxy: string; tls: { ca: string } } = {
    method,
    headers: {
      Accept: "application/json",
      Authorization: `DPoP ${route.placeholder}`,
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    proxy: proxy.toString(),
    tls: { ca: route.caCertificate },
  };
  return await fetch(PUBLIC + path, init);
}

async function setup(): Promise<void> {
  const project = await described(
    await box("POST", "/api/v1/projects", { key: "APV", name: "Approve check" })
  );
  if (project.status !== 201 && project.status !== 200)
    fail(`the project: ${JSON.stringify(project)}`);
  const issue = await described(
    await box("POST", "/api/v1/issues", {
      project: "APV",
      title: "A document to approve",
      spec: "# Approve check\n\nThe document a session approves through Cipher's proxy.\n",
    })
  );
  const key = (issue.body as Record<string, unknown>)["key"];
  if (typeof key !== "string") fail(`the issue: ${JSON.stringify(issue)}`);
  const read = await described(await box("GET", `/api/v1/issues/${key}`));
  const issueBody = read.body as Record<string, unknown>;
  const artifact = issueBody["primary_artifact_id"];
  if (typeof artifact !== "string") fail(`the issue read: ${JSON.stringify(read)}`);
  const summary = await described(await box("GET", `/api/v1/issues/${key}/summary`));
  const documents = ((summary.body as Record<string, unknown>)["documents"] ?? []) as Record<
    string,
    unknown
  >[];
  const version = Number(
    documents.find((document) => document["id"] === artifact)?.["latest_version"] ?? 0
  );
  if (!(version > 0)) fail(`the spec's version: ${JSON.stringify(summary)}`);
  writeFileSync(setting("APPROVE_CHECK_STATE"), JSON.stringify({ issue: key, artifact, version }));
  say({ step: "setup", issue: key, artifact, version });
}

async function approve(): Promise<void> {
  const { artifact, version } = state();
  const client = new DispatchClient(PUBLIC, credential, localFetch);
  try {
    const review = (await client.reviewArtifact(artifact, version, {
      state: "approved",
    })) as unknown as Record<string, unknown>;
    say({
      step: "approve",
      recorded: true,
      state: review["state"],
      version: review["version"] ?? review["version_number"],
      actor: review["actor"],
    });
  } catch (error) {
    say({
      step: "approve",
      recorded: false,
      error: error instanceof Error ? error.message : String(error),
    });
  }
}

async function approveThenRevoked(): Promise<void> {
  const { artifact, version } = state();
  const route = await approveRoute();
  const path = `/api/v1/artifacts/${artifact}/versions/${version}/reviews`;
  const first = await described(await proxied(route, "POST", path, { state: "approved" }));
  const revoke = setting("APPROVE_CHECK_REVOKE_COMMAND");
  const ran = Bun.spawnSync(["bash", "-c", revoke], { stdout: "pipe", stderr: "pipe" });
  const second = await described(await proxied(route, "POST", path, { state: "approved" }));
  say({
    step: "approve-then-revoked",
    first: { status: first.status, code: first.code },
    revoke_exit: ran.exitCode,
    second,
  });
}

async function main(): Promise<void> {
  const [command] = process.argv.slice(2);
  switch (command) {
    case "setup":
      return await setup();
    case "approve":
      return await approve();
    case "approve-then-revoked":
      return await approveThenRevoked();
    case "box":
    case "proxy": {
      // The method, the path, and a JSON body where one is given.
      const [method = "GET", path = "/", text] = process.argv.slice(3);
      const body: unknown = text === undefined ? undefined : JSON.parse(text);
      const sent =
        command === "box"
          ? await box(method, path, body)
          : await proxied(await approveRoute(), method, path, body);
      return say({ step: command, ...(await described(sent)) });
    }
    case "reviews": {
      const { artifact } = state();
      return say({
        step: "reviews",
        ...(await described(await box("GET", `/api/v1/artifacts/${artifact}/reviews`))),
      });
    }
    case "comments": {
      const { issue } = state();
      return say({
        step: "comments",
        ...(await described(await box("GET", `/api/v1/issues/${issue}/comments`))),
      });
    }
    case "agent-tokens":
      return say({
        step: "agent-tokens",
        ...(await described(await box("GET", "/api/v1/me/agent-tokens"))),
      });
    default:
      fail(`unknown command ${String(command)}`);
  }
}

await main();
process.exit(0);
