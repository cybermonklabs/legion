// The broker audience Stage 4a mints its login token for, and enrolls its pods for: the one the
// namespace's own daemons use, so the run proves enrollment against the broker they enroll with.
// Each ConfigMap with a `legion.yaml` key is a daemon's configuration. One whose
// runtime.kubernetes.agent_secrets block names no audience uses the daemon's default, as the loader
// does, and with no such block at all the run takes that default too. Daemons that use different
// audiences are refused, each named: no one audience is the deployment's. The harness holds its
// pods to the same rule (deploymentBrokerAudience, packages/daemon/internal/runtime/sandbox/
// live_secrets_pure_test.go).
//
//   kubectl get configmaps -o json | bun scripts/e2e/lib/broker-audience.ts <default>
//     prints the audience and exits 0; exits 1 naming why when the ConfigMaps cannot be read or
//     the daemons use different audiences, and 2 when <default> is not one audience.

type ConfigMapList = {
  items?: Array<{ metadata?: { name?: string }; data?: Record<string, string> }>;
};
type DaemonFile = {
  runtime?: { kubernetes?: { agent_secrets?: { audience?: unknown } | null } };
} | null;

/** The audience the daemons whose ConfigMaps `configMaps` lists use, or `fallback` where none names one. */
export function deploymentBrokerAudience(configMaps: string, fallback: string): string {
  const list = JSON.parse(configMaps) as ConfigMapList;
  const daemons = new Map<string, string[]>();
  for (const item of list.items ?? []) {
    const text = item.data?.["legion.yaml"];
    if (text === undefined) {
      continue;
    }
    const name = item.metadata?.name ?? "";
    let file: DaemonFile;
    try {
      file = Bun.YAML.parse(text) as DaemonFile;
    } catch (error) {
      throw new Error(`ConfigMap ${name}: legion.yaml: ${(error as Error).message}`);
    }
    const block = file?.runtime?.kubernetes?.agent_secrets;
    if (block === undefined || block === null) {
      continue;
    }
    const named =
      typeof block.audience === "string" && block.audience !== "" ? block.audience : fallback;
    daemons.set(named, [...(daemons.get(named) ?? []), name]);
  }
  const audiences = [...daemons.keys()].sort();
  if (audiences.length === 0) {
    return fallback;
  }
  if (audiences.length === 1) {
    return audiences[0];
  }
  const each = audiences.map(
    (audience) => `${audience} (${(daemons.get(audience) ?? []).join(", ")})`
  );
  throw new Error(
    `the namespace's daemons enroll their pods for different broker audiences: ${each.join("; ")}`
  );
}

if (import.meta.main) {
  const [fallback = ""] = process.argv.slice(2);
  if (!/^\S+$/.test(fallback)) {
    process.stderr.write(
      `broker-audience: the daemon's default audience must be one word, not ${JSON.stringify(fallback)}\n`
    );
    process.exit(2);
  }
  try {
    process.stdout.write(`${deploymentBrokerAudience(await Bun.stdin.text(), fallback)}\n`);
  } catch (error) {
    process.stderr.write(`broker-audience: ${(error as Error).message}\n`);
    process.exit(1);
  }
}
