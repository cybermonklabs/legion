import { beforeAll, expect, test } from "bun:test";
import { chmodSync, mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// stage4a-sandbox-runtime.sh mints its broker login token, and enrolls its pods, for the token
// audience the broker verifies, LEGION_E2E_AGENT_SECRETS_AUDIENCE, and guesses none: a run that names
// the broker (its URL or its login account) and no audience is refused in prerequisites, naming the
// input, before it reads a cluster or builds anything, and a run that names no broker needs none.
// Every cluster and build tool is a stand-in that does nothing, and the operator context and every
// kubeconfig name nothing real, so the teardown each run makes at its exit reaches no cluster. A run
// the audience check lets through stops at the next prerequisite, LEGION_E2E_POD_SERVICE_ACCOUNT,
// which no row sets. Besides this directory, a run writes only the work directory the script makes
// for itself and removes at its exit.
const root = join(import.meta.dir, "..", "..", "..");
const script = join(root, "scripts", "e2e", "stage4a-sandbox-runtime.sh");
const dir = temporaryDirectory("stage4a-broker-audience-test");
const bin = join(dir, "bin");
const kubeconfig = join(dir, "kubeconfig");

beforeAll(() => {
  mkdirSync(bin);
  // The tools prerequisites requires by name; none of them runs before the audience check.
  for (const name of [
    "go",
    "kubectl",
    "curl",
    "ss",
    "agent-secrets",
    "diff",
    "docker",
    "jq",
    "bun",
  ]) {
    writeFileSync(join(bin, name), "#!/bin/sh\nexit 0\n");
    chmodSync(join(bin, name), 0o755);
  }
  writeFileSync(kubeconfig, "");
});

/** The first CHECK … FAIL line of a run given every earlier prerequisite and these broker inputs. */
function firstFailure(evidence: string, broker: Record<string, string>): string {
  const run = Bun.spawnSync(["bash", script], {
    env: {
      HOME: dir,
      PATH: `${bin}:/usr/local/bin:/usr/bin:/bin`,
      KUBECONFIG: kubeconfig,
      LEGION_E2E_OPERATOR_CONTEXT: "stand-in-operator",
      LEGION_E2E_RUNTIME_CONTEXT: "stand-in",
      LEGION_E2E_RUNTIME_KUBECONFIG: kubeconfig,
      LEGION_E2E_RUNTIME_IDENTITY_KIND: "kubernetes-service-account",
      LEGION_E2E_PLATFORM: "eks",
      LEGION_E2E_IMAGE: `registry.internal.example/legion-worker@sha256:${"0".repeat(64)}`,
      LEGION_E2E_STORAGE_CLASS: "stand-in",
      STAGE4A_EVIDENCE_DIR: join(dir, evidence),
      ...broker,
    },
    stdout: "pipe",
    stderr: "pipe",
    timeout: 60_000,
  });
  const output = run.stdout.toString() + run.stderr.toString();
  return output.split("\n").find((line) => /^CHECK \S+: FAIL: /.test(line)) ?? "";
}

const url = { LEGION_E2E_AGENT_SECRETS_URL: "https://broker.internal.example" };
const account = { LEGION_E2E_AGENT_SECRETS_ACCOUNT: "proof/stand-in" };
const audience = { LEGION_E2E_AGENT_SECRETS_AUDIENCE: "stand-in-audience" };
const refused = /^CHECK prerequisites: FAIL: LEGION_E2E_AGENT_SECRETS_AUDIENCE is unset/;
const passed = /^CHECK prerequisites: FAIL: LEGION_E2E_POD_SERVICE_ACCOUNT /;

const rows: Array<[string, Record<string, string>, RegExp]> = [
  ["the broker's URL with no audience is refused", url, refused],
  ["the login account with no audience is refused", account, refused],
  ["the URL and the account with no audience are refused", { ...url, ...account }, refused],
  ["a run that names no broker needs no audience", {}, passed],
  ["the URL and the account with an audience pass", { ...url, ...account, ...audience }, passed],
];
for (const [index, [name, broker, want]] of rows.entries()) {
  test(name, () => expect(firstFailure(`run-${index}`, broker)).toMatch(want), 90_000);
}
