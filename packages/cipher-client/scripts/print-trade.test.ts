// packages/cipher-client/scripts/print-trade.test.ts
//
// print-trade.ts as scripts/project-secrets-check.sh runs it: one trade from the environment, then
// the broker's introspection of the token it got, read with the account token
// AGENT_SECRETS_ACCOUNT_TOKEN_FILE names, printed as one JSON line that never holds the token.

import { expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

const makeDirectory = temporaryDirectories("print-trade");
const SCRIPT = join(import.meta.dir, "print-trade.ts");
const ACCESS_TOKEN = fixtureSecret("access");
const ACCOUNT_TOKEN = fixtureSecret("account");

interface Seen {
  readonly path: string;
  readonly authorization: string;
  readonly form: URLSearchParams;
}

/** A broker that mints ACCESS_TOKEN for any trade and answers introspection with `answer`. */
function broker(answer: { status: number; body: Record<string, unknown> }) {
  const seen: Seen[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const path = new URL(request.url).pathname;
      seen.push({
        path,
        authorization: request.headers.get("Authorization") ?? "",
        form: new URLSearchParams(await request.text()),
      });
      if (path === "/v1/oauth/introspect") {
        return Response.json(answer.body, { status: answer.status });
      }
      return Response.json({
        access_token: ACCESS_TOKEN,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  return { url: `http://127.0.0.1:${server.port}`, seen, stop: () => server.stop(true) };
}

/** Runs print-trade.ts as a pod would, with the account token file when `account` is true. */
async function run(url: string, account: boolean) {
  const dir = makeDirectory();
  const tokenFile = join(dir, "exchange-token");
  await writeFile(tokenFile, `${fixtureSecret("pod")}\n`);
  const accountFile = join(dir, "account-token");
  await writeFile(accountFile, `${ACCOUNT_TOKEN}\n`);
  const env: Record<string, string> = {
    PATH: process.env.PATH ?? "",
    HOME: dir,
    AGENT_SECRETS_URL: url,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
    AGENT_SECRETS_SESSION: "legion-priv-worker-session",
    AGENT_SECRETS_SIGNIN_DIR: join(dir, "signins"),
  };
  if (account) env.AGENT_SECRETS_ACCOUNT_TOKEN_FILE = accountFile;
  const child = Bun.spawn(["bun", SCRIPT], { env, stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, exit] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  return { stdout, stderr, exit };
}

test("a trade prints its introspection's scope and projects, and never the token", async () => {
  const stub = broker({
    status: 200,
    body: {
      active: true,
      session: "legion-priv-worker-session",
      person: "carol@example.com",
      project_scope: "named",
      projects: ["PRIV"],
      jkt: "thumbprint",
    },
  });
  try {
    const { stdout, stderr, exit } = await run(stub.url, true);
    expect({ exit, stderr }).toEqual({ exit: 0, stderr: "" });
    expect(stdout.endsWith("\n")).toBe(true);
    expect(JSON.parse(stdout)).toEqual({
      active: true,
      person: "carol@example.com",
      service: "",
      project_scope: "named",
      projects: ["PRIV"],
    });
    expect(stdout).not.toContain(ACCESS_TOKEN);
    const introspection = stub.seen.find((s) => s.path === "/v1/oauth/introspect");
    expect(introspection?.authorization).toBe(`Bearer ${ACCOUNT_TOKEN}`);
    expect(introspection?.form.get("token")).toBe(ACCESS_TOKEN);
  } finally {
    stub.stop();
  }
});

test("an answer naming no project prints an empty list, not a missing one", async () => {
  const stub = broker({
    status: 200,
    body: { active: true, session: "s", service: "example-fleet", project_scope: "person" },
  });
  try {
    const { stdout, exit } = await run(stub.url, true);
    expect(exit).toBe(0);
    expect(JSON.parse(stdout)).toEqual({
      active: true,
      person: "",
      service: "example-fleet",
      project_scope: "person",
      projects: [],
    });
  } finally {
    stub.stop();
  }
});

test("with no account token it trades nothing and exits 2 naming the variable", async () => {
  const stub = broker({ status: 200, body: { active: true } });
  try {
    const { stdout, stderr, exit } = await run(stub.url, false);
    expect(exit).toBe(2);
    expect(stdout).toBe("");
    expect(stderr).toContain("AGENT_SECRETS_ACCOUNT_TOKEN_FILE");
    expect(stub.seen).toEqual([]);
  } finally {
    stub.stop();
  }
});

test("an introspection the broker refuses exits 4 with its status and words", async () => {
  const stub = broker({ status: 403, body: { error: "the caller is not an API account" } });
  try {
    const { stdout, stderr, exit } = await run(stub.url, true);
    expect(exit).toBe(4);
    expect(stdout).toBe("");
    expect(stderr).toContain("403");
    expect(stderr).toContain("the caller is not an API account");
    expect(stderr).not.toContain(ACCESS_TOKEN);
  } finally {
    stub.stop();
  }
});
