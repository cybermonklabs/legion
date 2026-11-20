// packages/envoy-client/src/__tests__/device-login.test.ts
//
// A laptop's credential: the machine logs in once with `dispatch login --device`, and every
// session after that trades the access token the login holds
// (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`, `@legion/broker-client/device-login`). These rows
// hold what a session and the person at the keyboard see: the command prints the code and where
// to type it and keeps the login; a session's Dispatch credential is the login's trade, naming
// the session; and two processes that trade at once spend one refresh token once, since the
// broker revokes a login whose rotated refresh token is presented again past its grace.

import { expect, test } from "bun:test";
import { statSync, writeFileSync } from "node:fs";
import * as path from "node:path";
import { thumbprintOf } from "@legion/broker-client/test/sign-ins";
import { DEVICE_LOGIN_COMMAND } from "@legion/broker-client/token-errors";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { runDispatchCli } from "../dispatch-cli";
import { dispatchSettings } from "../dispatch-config";
import { readEnvoyConfig } from "../envoy-config";
import { deviceLoginStandIns } from "./broker-stand-ins";

const { broker, laptopEnvironment, loginFile, tempDir } = deviceLoginStandIns("device-login");

const PERSON = "ada@example.com";

/** `dispatch` run with `argv` in `env`, as a person runs it in a terminal: no host, no session. */
async function dispatch(argv: readonly string[], env: Record<string, string>) {
  let stdout = "";
  let stderr = "";
  const code = await runDispatchCli(argv, env, {
    stdout: (text) => {
      stdout += text;
    },
    stderr: (text) => {
      stderr += text;
    },
    readText: () => "",
    cwd: tempDir(),
    sleep: async () => {},
  });
  return { code, stdout, stderr };
}

test("dispatch login --device prints the code and where to type it, and keeps the login", async () => {
  const stub = broker({ polls: ["pending", "approve"] });
  const file = path.join(tempDir(), "legion", "device-login.json");
  const env = {
    HOME: tempDir(),
    AGENT_SECRETS_URL: stub.url,
    AGENT_SECRETS_DEVICE_LOGIN_FILE: file,
  };
  const { code, stdout } = await dispatch(["login", "--device", "--person", PERSON], env);
  expect(code, stdout).toBe(0);
  expect(stdout).toContain(`${stub.url}/credentials/machine`);
  expect(stdout).toContain("WDJB-MJHT");
  expect(stdout).toContain(PERSON);
  // Named by its setting, as every message names the file: a path can hold a pasted secret.
  expect(stdout).toContain("AGENT_SECRETS_DEVICE_LOGIN_FILE");
  // A machine whose sessions would not trade the login yet is told the one setting that does.
  expect(stdout).toContain("AGENT_SECRETS_MACHINE_IDENTITY=device-login");
  expect(stub.authorizations[0]?.form.get("dpop_jkt")).toBeTruthy();
  expect(stub.polls).toHaveLength(2);
  expect(statSync(file).mode & 0o777).toBe(0o600);
});

test("dispatch --help names dispatch login, and dispatch login --help says what it reads", async () => {
  const all = await dispatch(["--help"], { HOME: tempDir() });
  expect(all.code).toBe(0);
  expect(all.stdout).toContain("dispatch login --device");
  const one = await dispatch(["login", "--help"], { HOME: tempDir() });
  expect(one.code).toBe(0);
  expect(one.stdout).toContain("Usage: dispatch login --device [--person <address>]");
  expect(one.stdout).toContain("AGENT_SECRETS_URL");
  expect(one.stdout).toContain("AGENT_SECRETS_MACHINE_IDENTITY=device-login");
});

test("dispatch login refuses a call it cannot make, and says why", async () => {
  const stub = broker({ polls: ["deny"] });
  const file = path.join(tempDir(), "legion", "device-login.json");
  const env = {
    HOME: tempDir(),
    AGENT_SECRETS_URL: stub.url,
    AGENT_SECRETS_DEVICE_LOGIN_FILE: file,
  };
  const noDevice = await dispatch(["login", "--person", PERSON], env);
  expect(noDevice.code).toBe(2);
  expect(noDevice.stdout).toContain("--device");
  const noPerson = await dispatch(["login", "--device"], env);
  expect(noPerson.code).toBe(2);
  expect(noPerson.stdout).toContain("--person");
  expect(noPerson.stdout).toContain("AGENT_SECRETS_PERSON");
  const noBroker = await dispatch(["login", "--device", "--person", PERSON], { HOME: tempDir() });
  expect(noBroker.code).toBe(2);
  expect(noBroker.stdout).toContain("AGENT_SECRETS_URL");
  expect(stub.authorizations).toHaveLength(0);

  const denied = await dispatch(["login", "--device", "--person", PERSON], env);
  expect(denied.code).toBe(1);
  expect(denied.stdout).toContain("denied");
  expect(denied.stdout).toContain(DEVICE_LOGIN_COMMAND);
  expect(() => statSync(file)).toThrow();

  // The person may also be named by AGENT_SECRETS_PERSON, as a session names whom it acts for.
  const named = broker();
  const byVariable = await dispatch(["login", "--device"], {
    ...env,
    AGENT_SECRETS_URL: named.url,
    AGENT_SECRETS_PERSON: PERSON,
  });
  expect(byVariable.code, byVariable.stdout).toBe(0);
});

test("a laptop session's Dispatch credential is its device login's trade, naming the session", async () => {
  const stub = broker();
  const cached = fixtureSecret("access");
  const written = await loginFile(stub.url, {
    access_token: cached,
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const env = laptopEnvironment(stub.url, "http://dispatch.test", written.path);
  const config = readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, { cwd: tempDir() });
  const resolution = dispatchSettings(env, config, { session: "ses-laptop-1" });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  expect(resolution.credential.scheme).toBe("DPoP");
  const headers = await resolution.credential.headers("GET", "http://dispatch.test/api/v1/whoami");
  expect(headers.Authorization).toBe(`DPoP ${stub.tradeTokens[0]}`);
  const proof = JSON.parse(
    Buffer.from((headers.DPoP ?? "").split(".")[0] ?? "", "base64url").toString("utf8")
  ) as { jwk: JsonWebKey };
  // Dispatch checks the proof against the key the token is bound to: the login's own.
  expect(await thumbprintOf(proof.jwk)).toBe(written.thumbprint);
  const trade = stub.trades[0];
  expect(trade?.form.get("subject_token")).toBe(cached);
  expect(trade?.form.get("subject_token_type")).toBe(
    "urn:ietf:params:oauth:token-type:access_token"
  );
  expect(trade?.form.get("session")).toBe("ses-laptop-1");
  expect(trade?.form.has("actor_token")).toBe(false);
  expect(trade?.proofThumbprint).toBe(written.thumbprint);
});

/** Where the device login's module is: a child script in a temporary directory imports it by
 *  path, since no package resolves from there. */
const DEVICE_LOGIN_MODULE = path.resolve(
  import.meta.dir,
  "../../../broker-client/src/device-login.ts"
);
const TOKEN_CLIENT_MODULE = path.resolve(
  import.meta.dir,
  "../../../broker-client/src/token-client.ts"
);

/**
 * Two processes on one laptop that each trade the device login at `file` at the same moment, as
 * two sessions starting together do: each says it is ready, waits for the parent's line, trades,
 * and prints the token it got. With `lock` "none" they trade through a lock that holds nothing,
 * which is the control: the code is otherwise the same.
 */
async function tradeTwiceAtOnce(brokerUrl: string, file: string, lock: "file" | "none") {
  const child = path.join(tempDir(), "trade.ts");
  writeFileSync(
    child,
    `import { DeviceLoginIdentity } from ${JSON.stringify(DEVICE_LOGIN_MODULE)};
import { BrokerTokenClient } from ${JSON.stringify(TOKEN_CLIENT_MODULE)};
const [file, brokerUrl, session, lock] = process.argv.slice(2);
const identity = new DeviceLoginIdentity({
  path: file,
  brokerUrl,
  ...(lock === "none" ? { lock: (_path, work) => work() } : {}),
});
const client = new BrokerTokenClient({ brokerUrl, key: await identity.key(), subject: identity, session, log: () => {} });
const stdin = Bun.stdin.stream().getReader();
console.log("ready");
await stdin.read();
try {
  const token = await client.token();
  console.log(JSON.stringify({ traded: token.accessToken }));
} catch (error) {
  console.log(JSON.stringify({ error: String(error) }));
}
client.close();
`
  );
  const children = ["ses-one", "ses-two"].map((session) =>
    Bun.spawn([process.execPath, child, file, brokerUrl, session, lock], {
      stdin: "pipe",
      stdout: "pipe",
      stderr: "inherit",
    })
  );
  const readers = children.map((spawned) => spawned.stdout.getReader());
  let buffers = children.map(() => "");
  const nextLine = async (at: number): Promise<string> => {
    const reader = readers[at] as (typeof readers)[number];
    while (!(buffers[at] ?? "").includes("\n")) {
      const { value, done } = await reader.read();
      if (done) throw new Error(`child ${at} exited before its line: ${buffers[at]}`);
      buffers = buffers.map((text, index) =>
        index === at ? text + new TextDecoder().decode(value) : text
      );
    }
    const text = buffers[at] as string;
    const end = text.indexOf("\n");
    buffers[at] = text.slice(end + 1);
    return text.slice(0, end);
  };
  expect(await Promise.all([nextLine(0), nextLine(1)])).toEqual(["ready", "ready"]);
  for (const spawned of children) {
    spawned.stdin.write("go\n");
    spawned.stdin.flush();
  }
  const lines = await Promise.all([nextLine(0), nextLine(1)]);
  for (const spawned of children) spawned.stdin.end();
  await Promise.all(children.map((spawned) => spawned.exited));
  return lines.map((line) => JSON.parse(line) as { traded?: string; error?: string });
}

/** Each refresh token the broker was presented, by how often. */
function presented(refreshes: readonly { form: URLSearchParams }[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const { form } of refreshes) {
    const token = form.get("refresh_token") ?? "";
    counts.set(token, (counts.get(token) ?? 0) + 1);
  }
  return counts;
}

test("two processes trading at once spend the device login's refresh token once", async () => {
  // The broker holds each refresh's answer for up to 2 s, until another arrives, so two refreshes
  // made at once overlap there: under the lock the second process never refreshes at all.
  const stub = broker({ refreshOverlapMs: 2_000 });
  const written = await loginFile(stub.url);
  const outcomes = await tradeTwiceAtOnce(stub.url, written.path, "file");
  expect(outcomes.map((outcome) => outcome.error)).toEqual([undefined, undefined]);
  expect(new Set(outcomes.map((outcome) => outcome.traded)).size).toBe(2);
  expect([...presented(stub.refreshes)]).toEqual([[written.refreshToken, 1]]);
  // The second process spent the access token the first one's refresh kept in the file.
  expect(stub.trades.map((trade) => trade.form.get("subject_token"))).toEqual([
    stub.loginTokens[0],
    stub.loginTokens[0],
  ]);
}, 30_000);

test("control: with no lock, the two processes spend one refresh token twice", async () => {
  const stub = broker({ refreshOverlapMs: 2_000 });
  const written = await loginFile(stub.url);
  await tradeTwiceAtOnce(stub.url, written.path, "none");
  expect(presented(stub.refreshes).get(written.refreshToken)).toBe(2);
}, 30_000);
