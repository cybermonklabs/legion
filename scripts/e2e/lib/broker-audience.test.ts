import { expect, test } from "bun:test";
import { join } from "node:path";
import { deploymentBrokerAudience } from "./broker-audience";

// Stage 4a mints its login token for the broker audience the namespace's daemons use: the one their
// legion.yaml names, the daemon's default where it names none, and no audience at all where two
// daemons use different ones. Each row that reads one audience has a neighbour that reads another
// from the same ConfigMaps changed in one line.
const helper = join(import.meta.dir, "broker-audience.ts");
const enrolled =
  "runtime:\n  kubernetes:\n    agent_secrets:\n      url: https://secrets.internal.example\n";
const named = `${enrolled}      audience: agent-secrets\n`;
const unenrolled = "runtime:\n  kubernetes:\n    namespace: legion\n";
const ca = { "ca.crt": "not a daemon's configuration" };

/** `kubectl get configmaps -o json` for ConfigMaps of these names and data. */
function configMaps(data: Record<string, Record<string, string>>): string {
  return JSON.stringify({
    items: Object.entries(data).map(([name, values]) => ({ metadata: { name }, data: values })),
  });
}

const reads: Array<[string, Record<string, Record<string, string>>, string]> = [
  [
    "a daemon that names agent-secrets",
    { "legion-config": { "legion.yaml": named }, "kube-root-ca.crt": ca },
    "agent-secrets",
  ],
  [
    "the same daemon naming no audience takes the default",
    { "legion-config": { "legion.yaml": enrolled }, "kube-root-ca.crt": ca },
    "cipher",
  ],
  [
    "no daemon enrolls its pods",
    { "legion-config": { "legion.yaml": unenrolled }, "kube-root-ca.crt": ca },
    "cipher",
  ],
  [
    "two daemons that agree",
    { "legion-config": { "legion.yaml": named }, "legion-acme-config": { "legion.yaml": named } },
    "agent-secrets",
  ],
];
for (const [name, data, want] of reads) {
  test(name, () => expect(deploymentBrokerAudience(configMaps(data), "cipher")).toBe(want));
}

test("two daemons that disagree are refused, each named", () => {
  const data = {
    "legion-config": { "legion.yaml": named },
    "legion-acme-config": { "legion.yaml": enrolled },
  };
  expect(() => deploymentBrokerAudience(configMaps(data), "cipher")).toThrow(
    "the namespace's daemons enroll their pods for different broker audiences: agent-secrets (legion-config); cipher (legion-acme-config)"
  );
});

test("a legion.yaml that does not parse is refused, naming its ConfigMap", () => {
  const data = { "legion-config": { "legion.yaml": "runtime: [\n" } };
  expect(() => deploymentBrokerAudience(configMaps(data), "cipher")).toThrow(
    "ConfigMap legion-config: legion.yaml"
  );
});

/** The helper run as the script runs it: the ConfigMaps on stdin and the default as its argument. */
function run(stdin: string, fallback: string) {
  const result = Bun.spawnSync(["bun", helper, fallback], {
    stdin: Buffer.from(stdin),
    stdout: "pipe",
    stderr: "pipe",
  });
  return {
    code: result.exitCode,
    stdout: result.stdout.toString(),
    stderr: result.stderr.toString(),
  };
}

test("the command prints the audience the daemons name, and refuses daemons that disagree", () => {
  expect(run(configMaps({ "legion-config": { "legion.yaml": named } }), "cipher")).toEqual({
    code: 0,
    stdout: "agent-secrets\n",
    stderr: "",
  });
  const refused = run(
    configMaps({
      "legion-config": { "legion.yaml": named },
      "legion-acme-config": { "legion.yaml": enrolled },
    }),
    "cipher"
  );
  expect(refused.code).toBe(1);
  expect(refused.stdout).toBe("");
  expect(refused.stderr).toContain("different broker audiences");
});

test("the command refuses a default that is not one audience", () => {
  const refused = run(configMaps({}), "");
  expect(refused.code).toBe(2);
  expect(refused.stderr).toContain("must be one word");
});
