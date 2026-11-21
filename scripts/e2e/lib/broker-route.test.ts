import { expect, test } from "bun:test";
import { chmodSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// The gate the two cluster proofs that run a Kubernetes daemon read their pods' broker route
// through (claude-backend-sandbox.sh and stage4b-sandbox-tree.sh): a daemon under the Kubernetes
// runtime enrols every pod with the deployment's secrets broker, and each role trades the pod's
// exchange token for a broker token of its own, which is a session's one credential at the Envoy
// listener now that the listener takes no shared token. Stdout is the run's
// `runtime.kubernetes.agent_secrets` block, indented to sit under `kubernetes:`; a refusal names
// every variable that is missing or wrong on stderr, never a value, and prints nothing on stdout.
const script = join(import.meta.dir, "broker-route.sh");
const dirs = temporaryDirectories("broker-route-test");

/** A token file of mode `mode` holding `body`, in a directory of its own. */
function tokenFile(body = "eyJ.token.body\n", mode = 0o600): string {
  const path = join(dirs(), "token");
  writeFileSync(path, body);
  chmodSync(path, mode);
  return path;
}

function run(vars: Record<string, string>, ...extra: string[]) {
  const env: Record<string, string> = { PATH: process.env.PATH ?? "", ...vars };
  const result = Bun.spawnSync(["bash", script, ...extra], { env });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

/** Every required input, set to a value the gate takes. */
function inputs(): Record<string, string> {
  return {
    LEGION_E2E_AGENT_SECRETS_URL: "https://secrets.example.test",
    LEGION_E2E_AGENT_SECRETS_TOKEN_FILE: tokenFile(),
    LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE: "legion-broker-exchange",
  };
}

test("the daemon's three inputs print the agent_secrets block, nothing on stderr", () => {
  const vars = inputs();
  const { code, stdout, stderr } = run(vars);
  expect(stderr).toBe("");
  expect(code).toBe(0);
  expect(stdout).toBe(
    "    agent_secrets:\n" +
      "      url: https://secrets.example.test\n" +
      `      token_file: ${vars.LEGION_E2E_AGENT_SECRETS_TOKEN_FILE}\n` +
      "      exchange_audience: legion-broker-exchange\n"
  );
});

test("LEGION_E2E_AGENT_SECRETS_AUDIENCE, when set, is the block's audience", () => {
  const { code, stdout } = run({
    ...inputs(),
    LEGION_E2E_AGENT_SECRETS_AUDIENCE: "legion-broker-pod",
  });
  expect(code).toBe(0);
  expect(stdout).toContain("      audience: legion-broker-pod\n");
});

test("every input unset is refused at once, each named, with nothing on stdout", () => {
  const { code, stdout, stderr } = run({}, "LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE");
  expect(code).toBe(1);
  expect(stdout).toBe("");
  for (const name of [
    "LEGION_E2E_AGENT_SECRETS_URL is unset",
    "LEGION_E2E_AGENT_SECRETS_TOKEN_FILE is unset",
    "LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE is unset",
    "LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE is unset",
  ]) {
    expect(stderr).toContain(name);
  }
});

// Each wrong value is refused naming the variable, and never printing the value, which names the
// operator's own deployment or is a credential.
test.each([
  [
    "a broker URL that is not https",
    "LEGION_E2E_AGENT_SECRETS_URL",
    "http://secrets.example.test",
    "is not an https:// URL",
  ],
  [
    "a broker URL with a path",
    "LEGION_E2E_AGENT_SECRETS_URL",
    "https://secrets.example.test/v1",
    "is not an https:// URL",
  ],
  [
    "a relative token file",
    "LEGION_E2E_AGENT_SECRETS_TOKEN_FILE",
    "token",
    "is not an absolute path",
  ],
  [
    "a token file that is not there",
    "LEGION_E2E_AGENT_SECRETS_TOKEN_FILE",
    "/nonexistent/broker-token",
    "is not a readable file",
  ],
  [
    "an exchange audience with a space",
    "LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE",
    "legion broker",
    "is not one audience",
  ],
])("%s is refused, naming the variable and not the value", (_what, name, value, refusal) => {
  const { code, stdout, stderr } = run({ ...inputs(), [name]: value });
  expect(code).toBe(1);
  expect(stdout).toBe("");
  expect(stderr).toContain(`${name} ${refusal}`);
  expect(stderr).not.toContain(value);
});

test("a token file its group or others can read is refused, naming its mode", () => {
  const { code, stdout, stderr } = run({
    ...inputs(),
    LEGION_E2E_AGENT_SECRETS_TOKEN_FILE: tokenFile(undefined, 0o640),
  });
  expect(code).toBe(1);
  expect(stdout).toBe("");
  expect(stderr).toContain(
    "LEGION_E2E_AGENT_SECRETS_TOKEN_FILE is readable by its group or others (mode 640): make it 0600"
  );
});

test("a token file that holds only white space is refused, and what it holds is never printed", () => {
  const { code, stderr } = run({
    ...inputs(),
    LEGION_E2E_AGENT_SECRETS_TOKEN_FILE: tokenFile(" \n"),
  });
  expect(code).toBe(1);
  expect(stderr).toContain("LEGION_E2E_AGENT_SECRETS_TOKEN_FILE holds no token");
});

test("an exchange audience that is the enrolment's own audience is refused, naming both", () => {
  const { code, stderr } = run({
    ...inputs(),
    LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE: "agent-secrets",
  });
  expect(code).toBe(1);
  expect(stderr).toContain(
    "LEGION_E2E_AGENT_SECRETS_EXCHANGE_AUDIENCE must differ from the enrolment's audience (LEGION_E2E_AGENT_SECRETS_AUDIENCE, default agent-secrets)"
  );
});

test("a token-file variable the caller names is checked as the daemon's is, and printed in no block", () => {
  const listener = tokenFile(undefined, 0o644);
  const refused = run(
    { ...inputs(), LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE: listener },
    "LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE"
  );
  expect(refused.code).toBe(1);
  expect(refused.stderr).toContain(
    "LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE is readable by its group or others (mode 644)"
  );
  chmodSync(listener, 0o600);
  const taken = run(
    { ...inputs(), LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE: listener },
    "LEGION_E2E_LISTENER_AGENT_SECRETS_TOKEN_FILE"
  );
  expect(taken.stderr).toBe("");
  expect(taken.code).toBe(0);
  expect(taken.stdout).not.toContain(listener);
});
