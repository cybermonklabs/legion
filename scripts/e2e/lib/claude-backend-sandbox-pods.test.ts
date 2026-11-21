import { describe, expect, test } from "bun:test";
import { join } from "node:path";
import { scriptFunctions } from "./script-functions";

// claude-backend-sandbox.sh's pod_endpoint_mismatch, the check each role's pod is held to before
// the run asks anything of it: the agent carries every endpoint of the run's scratch services, and
// no Envoy bearer. The listener takes no shared token and the daemon hands a role none, so a role's
// one credential at the listener is the broker token it trades for its own session; a role that
// carries ENVOY_TOKEN_FILE or ENVOY_TOKEN was handed a shared token from somewhere. The function is
// taken from the script by name and run with pod_env, which reads the agent's environment through
// the cluster, replaced by a stand-in that prints a fixture environment.
const check = scriptFunctions(join(import.meta.dir, "..", "claude-backend-sandbox.sh"))(
  "pod_endpoint_mismatch"
);

/** The environment a correct role's agent carries, against the rig below. */
const handed = [
  "DISPATCH_URL=http://10.0.0.5:7001",
  "ENVOY_URL=http://10.0.0.5:7002",
  "ENVOY_NATS_URL=nats://10.0.0.5:7003",
  "LEGION_DAEMON_URL=http://10.0.0.5:7004",
  "AGENT_SECRETS_MACHINE_IDENTITY=kubernetes",
  "HOME=/home/agent",
];

/** Runs the check against an agent whose environment is env, or whose read fails when env is null. */
function judge(env: string[] | null) {
  const podEnv =
    env === null
      ? `pod_env() { echo "pods \\"pod-a\\" not found" >&2; return 1; }`
      : `pod_env() { printf '%s\\n' ${env.map((line) => JSON.stringify(line)).join(" ")}; }`;
  const run = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
host=10.0.0.5 port_dispatch=7001 port_listener=7002 port_nats=7003 port_daemon=7004
${podEnv}
${check}
status=0
pod_endpoint_mismatch pod-a tester || status=$?
echo "status $status"`,
    ],
    { env: { PATH: process.env.PATH ?? "" } }
  );
  return run.stdout.toString() + run.stderr.toString();
}

describe("pod_endpoint_mismatch", () => {
  test("a role that carries every endpoint and no bearer is clean", () => {
    expect(judge(handed)).toBe("status 1\n");
  });

  test.each([
    ["ENVOY_TOKEN_FILE", "/var/run/legion/providers/ENVOY_TOKEN"],
    ["ENVOY_TOKEN", "shared-bearer-value"],
  ])("a role that carries %s is a mismatch, named by its variable and not its value", (name, value) => {
    const out = judge([...handed, `${name}=${value}`]);
    expect(out).toBe(`${name} in its environment, want none\nstatus 0\n`);
    expect(out).not.toContain(value);
  });

  test("an endpoint the role does not carry is named before a bearer it does", () => {
    const env = handed.filter((line) => !line.startsWith("ENVOY_URL=")).concat("ENVOY_TOKEN=x");
    expect(judge(env)).toBe("ENVOY_URL=<unset>, want http://10.0.0.5:7002\nstatus 0\n");
  });

  test("an environment that cannot be read is named, not passed", () => {
    expect(judge(null)).toBe(
      'no readable agent environment in its tester namespaces: pods "pod-a" not found \nstatus 0\n'
    );
  });
});
