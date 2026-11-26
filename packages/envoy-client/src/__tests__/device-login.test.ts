// packages/envoy-client/src/__tests__/device-login.test.ts
//
// A laptop's credential: the machine logs in once with `dispatch login --device`, and every
// session after that trades the access token the login holds
// (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`, `@legion/cipher-client/device-login`). These rows
// hold what a session and the person at the keyboard see: the command prints the code and where
// to type it and keeps the login; a session's Dispatch credential is the login's trade, naming
// the session; and two processes that trade at once spend one refresh token once, since the
// broker revokes a login whose rotated refresh token is presented again past its grace.

import { expect, setDefaultTimeout, spyOn, test } from "bun:test";
import {
  chmodSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import * as fsp from "node:fs/promises";
import * as path from "node:path";
import { thumbprintOf } from "@legion/cipher-client/test/sign-ins";
import { DEVICE_LOGIN_COMMAND } from "@legion/cipher-client/token-errors";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { HANG_GUARD_MS } from "@legion/test-fixtures/hang-guard";
import { runDispatchCli } from "../dispatch-cli";
import { dispatchSettings } from "../dispatch-config";
import { readEnvoyConfig } from "../envoy-config";
import { deviceLoginStandIns } from "./broker-stand-ins";

// The rows wait on files, processes or local servers, which a loaded machine slows. Their bound
// is a hang guard, not a verdict, for every row of this file that states none of its own.
setDefaultTimeout(HANG_GUARD_MS);

const { broker, laptopEnvironment, loginFile, tempDir } = deviceLoginStandIns("device-login");

const PERSON = "ada@example.com";

/** `dispatch` run with `argv` in `env`, as a person runs it in a terminal: no host, no session.
 *  `sleep` is each wait before a poll of the code; by default none. */
async function dispatch(
  argv: readonly string[],
  env: Record<string, string>,
  sleep: (ms: number) => Promise<void> = async () => {}
) {
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
    sleep,
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

test("a second dispatch login --device names the earlier login's credential, which stays live", async () => {
  const stub = broker();
  const earlier = await loginFile(stub.url, { credential_id: "cred-earlier-1" });
  const env = {
    HOME: tempDir(),
    AGENT_SECRETS_URL: stub.url,
    AGENT_SECRETS_DEVICE_LOGIN_FILE: earlier.path,
  };
  const { code, stdout } = await dispatch(["login", "--device", "--person", PERSON], env);
  expect(code, stdout).toBe(0);
  // The new login replaces the file; the earlier credential lives on at the broker until it is
  // revoked or lapses, so a copy of the old file still works. The command says which to revoke,
  // and where: the machine logins listed on the page the code was typed into.
  expect(stdout).toContain("cred-earlier-1");
  expect(stdout).toContain(`Machine logins on ${stub.url}/credentials/machine`);
  // A first login has nothing earlier to name.
  const first = await dispatch(["login", "--device", "--person", PERSON], {
    ...env,
    AGENT_SECRETS_DEVICE_LOGIN_FILE: path.join(tempDir(), "legion", "device-login.json"),
  });
  expect(first.code, first.stdout).toBe(0);
  expect(first.stdout).not.toContain("Machine logins on");
});

test("of two dispatch login --device at once, the one kept second names the other's credential", async () => {
  const first = broker({ credentialId: "cred-first-1" });
  const second = broker({ credentialId: "cred-second-1" });
  const file = path.join(tempDir(), "legion", "device-login.json");
  const env = (url: string) => ({
    HOME: tempDir(),
    AGENT_SECRETS_URL: url,
    AGENT_SECRETS_DEVICE_LOGIN_FILE: file,
  });
  const argv = ["login", "--device", "--person", PERSON];
  // Both start while the machine holds no login. The first is approved and kept while the second
  // still waits for its approval; the second is approved then, and its file replaces the first's.
  const secondWaits = Promise.withResolvers<void>();
  const one = dispatch(argv, env(first.url), async () => {
    await secondWaits.promise;
  });
  const two = dispatch(argv, env(second.url), async () => {
    secondWaits.resolve();
    await one;
  });
  const [kept1, kept2] = await Promise.all([one, two]);
  expect(kept1.code, kept1.stdout).toBe(0);
  expect(kept2.code, kept2.stdout).toBe(0);
  expect(kept1.stdout).not.toContain("Machine logins on");
  // The second is told the credential its file replaced, which stays live.
  expect(kept2.stdout).toContain(
    "the earlier login of this machine, launcher credential cred-first-1"
  );
  expect(JSON.parse(readFileSync(file, "utf8")).credential_id).toBe("cred-second-1");
});

test("dispatch login refuses a file setting that names no regular file, before any code is asked for", async () => {
  // A directory, which no save can replace; a named pipe and a link to one, which a read waits on
  // until a process writes to it; a link that loops, which no read can follow. Each is refused
  // with exit 2 before any request, so no person approves a login this machine could not keep.
  for (const [name, make, says] of [
    ["directory", (file: string) => mkdirSync(file), "is a directory, not a regular file"],
    [
      "named pipe",
      (file: string) => expect(Bun.spawnSync(["mkfifo", file]).exitCode).toBe(0),
      "is a named pipe, not a regular file",
    ],
    [
      "link to a named pipe",
      (file: string) => {
        expect(Bun.spawnSync(["mkfifo", `${file}.pipe`]).exitCode).toBe(0);
        symlinkSync(`${file}.pipe`, file);
      },
      "is a named pipe, not a regular file",
    ],
    [
      "link that loops",
      (file: string) => symlinkSync(file, file),
      "is a loop of links, not a regular file",
    ],
  ] as const) {
    const stub = broker();
    const file = path.join(tempDir(), "legion", "device-login.json");
    mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
    chmodSync(path.dirname(file), 0o700);
    make(file);
    const { code, stdout } = await dispatch(["login", "--device", "--person", PERSON], {
      HOME: tempDir(),
      AGENT_SECRETS_URL: stub.url,
      AGENT_SECRETS_DEVICE_LOGIN_FILE: file,
    });
    expect(code, `${name}: ${stdout}`).toBe(2);
    expect(stdout, name).toContain("AGENT_SECRETS_DEVICE_LOGIN_FILE");
    expect(stdout, name).toContain(says);
    // Named by its setting, never by its path.
    expect(stdout, name).not.toContain(file);
    expect(stub.authorizations, name).toHaveLength(0);
  }
});

test("dispatch login --device refuses a code no poll could reach before it prints the code", async () => {
  // A broker whose interval is no shorter than its code's life: the code would lapse before its
  // first poll, and an approval of it would mint a launcher credential bound to a key no process
  // holds. The command shows no code to approve, names the broker's interval and lifetime, and
  // does not tell the person to ask again, which would meet the same answer.
  const stub = broker({ interval: 1_800 });
  const file = path.join(tempDir(), "legion", "device-login.json");
  const { code, stdout } = await dispatch(["login", "--device", "--person", PERSON], {
    HOME: tempDir(),
    AGENT_SECRETS_URL: stub.url,
    AGENT_SECRETS_DEVICE_LOGIN_FILE: file,
  });
  expect(code, stdout).toBe(1);
  expect(stdout).not.toContain("WDJB-MJHT");
  expect(stdout).not.toContain("enter the code");
  expect(stdout).toContain(
    "answered a device code that lives 600 s, with a poll interval of 1800 s"
  );
  expect(stdout).not.toContain("ask again");
  expect(stub.authorizations).toHaveLength(1);
  expect(stub.polls).toHaveLength(0);
});

test("a login approved and then not kept names its credential and where to revoke it", async () => {
  const stub = broker({ credentialId: "cred-unkept-1" });
  const file = path.join(tempDir(), "legion", "device-login.json");
  // The file's place passes the check before the code is asked for; a directory is made there
  // while the person approves, so the save after the approval cannot keep the login there.
  const { code, stdout } = await dispatch(
    ["login", "--device", "--person", PERSON],
    { HOME: tempDir(), AGENT_SECRETS_URL: stub.url, AGENT_SECRETS_DEVICE_LOGIN_FILE: file },
    async () => {
      mkdirSync(file, { recursive: true });
    }
  );
  expect(code, stdout).toBe(1);
  // The credential is live at the broker: the person is told which it is and where to revoke it.
  expect(stdout).toContain("cred-unkept-1");
  expect(stdout).toContain("is a directory");
  expect(stdout).toContain(`revoke it in Machine logins on ${stub.url}/credentials/machine`);
  expect(stdout).not.toContain(file);
  // The save stopped before it wrote anything: no copy of the login and no temporary file.
  expect(readdirSync(path.dirname(file))).toEqual(["device-login.json"]);
});

test("a login kept whose lock is not let go says so, and asks for no revoke", async () => {
  const stub = broker({ credentialId: "cred-kept-1" });
  const file = path.join(tempDir(), "legion", "device-login.json");
  // The save keeps the login; then letting go of its lock fails, at the move of the lock aside.
  const real = fsp.rename;
  const renames = spyOn(fsp, "rename").mockImplementation((async (
    from: Parameters<typeof fsp.rename>[0],
    to: Parameters<typeof fsp.rename>[1]
  ) => {
    if (String(from) === `${file}.lock`) {
      throw Object.assign(new Error("refused"), { code: "EIO" });
    }
    return await real(from, to);
  }) as typeof fsp.rename);
  let ran = { code: -1, stdout: "", stderr: "" };
  try {
    ran = await dispatch(["login", "--device", "--person", PERSON], {
      HOME: tempDir(),
      AGENT_SECRETS_URL: stub.url,
      AGENT_SECRETS_DEVICE_LOGIN_FILE: file,
    });
  } finally {
    renames.mockRestore();
  }
  // The machine uses this login: the command says it is kept, and why the lock was left, and
  // tells nobody to revoke it.
  expect(ran.code, ran.stdout).toBe(0);
  expect(ran.stdout).toContain("launcher credential cred-kept-1");
  expect(ran.stdout).toContain("the login is kept, but this process could not let go of the lock");
  expect(ran.stdout).toContain("EIO");
  expect(ran.stdout).not.toContain("revoke it");
  expect(JSON.parse(readFileSync(file, "utf8")).credential_id).toBe("cred-kept-1");
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
  // A broker off this machine's loopback is https: the login's tokens cross that connection.
  const offMachine = await dispatch(["login", "--device", "--person", PERSON], {
    ...env,
    AGENT_SECRETS_URL: "http://broker.test",
  });
  expect(offMachine.code, offMachine.stdout).toBe(2);
  expect(offMachine.stdout).toContain("loopback");
  // A login directory others may write is refused before any code is asked for, so no person
  // approves a login this machine cannot keep.
  const open = path.join(tempDir(), "open", "device-login.json");
  mkdirSync(path.dirname(open));
  chmodSync(path.dirname(open), 0o775);
  const openDirectory = await dispatch(["login", "--device", "--person", PERSON], {
    ...env,
    AGENT_SECRETS_DEVICE_LOGIN_FILE: open,
  });
  expect(openDirectory.code, openDirectory.stdout).toBe(2);
  expect(openDirectory.stdout).toContain("chmod 700");
  expect(stub.authorizations).toHaveLength(0);
  // So is a directory of this user's own that this user cannot enter (0600, as a `chmod -R 600`
  // leaves it) or cannot write to (0555). Root passes `access` on any mode, so under root each
  // case asks for its code and keeps the login there; the row checks that instead, so it always
  // checks something. Each case has a broker of its own that approves at once.
  const asRoot = process.getuid?.() === 0;
  for (const [name, mode] of [
    ["unentered", 0o600],
    ["closed", 0o555],
  ] as const) {
    const approving = broker();
    const closed = path.join(tempDir(), name, "device-login.json");
    mkdirSync(path.dirname(closed));
    chmodSync(path.dirname(closed), mode);
    const closedDirectory = await dispatch(["login", "--device", "--person", PERSON], {
      ...env,
      AGENT_SECRETS_URL: approving.url,
      AGENT_SECRETS_DEVICE_LOGIN_FILE: closed,
    });
    chmodSync(path.dirname(closed), 0o700);
    if (asRoot) {
      expect(closedDirectory.code, `${name}: ${closedDirectory.stdout}`).toBe(0);
      expect(closedDirectory.stdout, name).not.toContain("cannot enter and write");
      expect(approving.authorizations, name).toHaveLength(1);
    } else {
      expect(closedDirectory.code, `${name}: ${closedDirectory.stdout}`).toBe(2);
      expect(closedDirectory.stdout, name).toContain("cannot enter and write");
      expect(closedDirectory.stdout, name).toContain("EACCES; run chmod 700");
      expect(approving.authorizations, name).toHaveLength(0);
    }
  }
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
  const config = await readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, {
    cwd: tempDir(),
  });
  const resolution = await dispatchSettings(env, config, { session: "ses-laptop-1" });
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

/** Where the device login's modules are, resolved through cipher-client's own exports: a child
 *  script in a temporary directory imports them by path, since no package resolves from there. */
const DEVICE_LOGIN_MODULE = Bun.resolveSync("@legion/cipher-client/device-login", import.meta.dir);
const DEVICE_LOGIN_FILE_MODULE = Bun.resolveSync(
  "@legion/cipher-client/device-login-file",
  import.meta.dir
);
const TOKEN_CLIENT_MODULE = Bun.resolveSync("@legion/cipher-client/token-client", import.meta.dir);

/**
 * Two processes on one laptop that each trade the device login at `file` at the same moment, as
 * two sessions starting together do: each says it is ready, waits for the parent's line, trades,
 * and prints the token it got. The lock is the controls' one difference: "file" is the login's
 * own, which covers the whole trade; "none" holds nothing; "refresh-only" covers the read and the
 * refresh of the login and leaves its trade outside. Both controls are built from the module's own
 * read and refresh, with the same one-minute rule.
 */
async function tradeTwiceAtOnce(
  brokerUrl: string,
  file: string,
  lock: "file" | "none" | "refresh-only"
) {
  const child = path.join(tempDir(), "trade.ts");
  writeFileSync(
    child,
    `import { DeviceLoginIdentity, refreshDeviceLogin, withDeviceLoginLock } from ${JSON.stringify(DEVICE_LOGIN_MODULE)};
import { readDeviceLogin } from ${JSON.stringify(DEVICE_LOGIN_FILE_MODULE)};
import { BrokerTokenClient } from ${JSON.stringify(TOKEN_CLIENT_MODULE)};
const [file, brokerUrl, session, lock] = process.argv.slice(2);
const identity = new DeviceLoginIdentity({ path: file, brokerUrl });
const readOrRefresh = async (held) => {
  const kept = await readDeviceLogin(file);
  const left = (kept.accessExpiresAt?.getTime() ?? 0) - Date.now();
  if (kept.accessToken !== "" && left >= 60_000) return kept;
  return await refreshDeviceLogin(file, kept, { describe: "the control's login", held });
};
const control = (describe, read) => ({
  describe: () => describe,
  async read() {
    const login = await read();
    return { token: login.accessToken, tokenType: "urn:ietf:params:oauth:token-type:access_token" };
  },
});
const subjects = {
  file: identity,
  none: control("a device login traded with no lock", () => readOrRefresh(async () => true)),
  "refresh-only": control("a device login whose lock covers its refresh alone", () =>
    withDeviceLoginLock(file, readOrRefresh)
  ),
};
const subject = subjects[lock];
const client = new BrokerTokenClient({ brokerUrl, key: await identity.key(), subject, session, log: () => {} });
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
  // The broker answers no refresh until it has been sent two, with no time limit, so no clock
  // decides whether the second process reads the file before the first one's refresh is kept.
  const stub = broker({ refreshesBeforeAnswer: 2 });
  const written = await loginFile(stub.url);
  await tradeTwiceAtOnce(stub.url, written.path, "none");
  expect(presented(stub.refreshes).get(written.refreshToken)).toBe(2);
}, 30_000);

test("a process refreshes the login only once another's trade of it is answered", async () => {
  // Access tokens of 30 s, under the one-minute margin, so each process refreshes before its
  // trade, and the broker holds each trade's answer for up to 2 s, until a refresh arrives: a
  // refresh while a trade is out revokes that trade's token. Under the login's lock the second
  // process refreshes only after the first one's trade is answered.
  const stub = broker({ accessSeconds: 30, tradeHoldMs: 2_000 });
  const written = await loginFile(stub.url);
  const outcomes = await tradeTwiceAtOnce(stub.url, written.path, "file");
  expect(outcomes.map((outcome) => outcome.error)).toEqual([undefined, undefined]);
  expect(stub.revokedTrades).toHaveLength(0);
  expect(stub.refreshes).toHaveLength(2);
  expect([...presented(stub.refreshes).values()]).toEqual([1, 1]);
}, 30_000);

test("control: with a lock around the refresh alone, the second refresh revokes the first trade's token", async () => {
  // The broker answers no trade until it has been sent two refreshes, with no time limit, so no
  // clock decides whether the second process's refresh lands while the first one's trade is out.
  const stub = broker({ accessSeconds: 30, refreshesBeforeTrade: 2 });
  const written = await loginFile(stub.url);
  const outcomes = await tradeTwiceAtOnce(stub.url, written.path, "refresh-only");
  expect(stub.revokedTrades).toHaveLength(1);
  // The broker refuses a revoked access token `400 invalid_request`.
  expect(outcomes.filter((outcome) => outcome.error?.includes("invalid_request"))).toHaveLength(1);
}, 30_000);
