#!/usr/bin/env bun
/**
 * codex-probe.ts — natsprobe in the environment a Codex plugin's MCP server was started with, for
 * the Codex rows of `scripts/e2e/nats-callout-sessions.sh`'s probe check.
 *
 * It reads that environment from `--environ`, a copy of the server's `/proc/<pid>/environ` the
 * script took while the server waited for its session record (`codex_keep_server_env`,
 * scripts/e2e/lib/codex-host.sh): what Codex (or the stand-in `codex`,
 * role-takeover/codex-client.ts) handed it, which the plugin manifest's `env_vars` filtered. It
 * resolves which NATS credential the plugin's own settings give that environment, by the calls the
 * server makes (`sessionSettings` over `sessionConfigFile("codex", …)`,
 * packages/claude-envoy/src/codex/codex-server.ts), so the session's envoy.json and `NATS_AUTH`
 * decide as they decide for the server. Then it runs natsprobe in that environment, naming
 * `--session` as the session it trades under (`AGENT_SECRETS_SESSION`, natsprobe's name for what the
 * server reads from its session record), with `--no-credential` where the settings resolve none.
 * Its first line says which credential; it prints no value.
 *
 * Usage:
 *   bun codex-probe.ts --environ <file> --session <id> --cwd <dir> -- <natsprobe> <args...>
 *
 * It exits with natsprobe's status, or 2 where the environment cannot be read or the settings
 * resolve no NATS connection there.
 */
import { readFileSync } from "node:fs";
import { sessionConfigFile, sessionSettings } from "@legion/envoy-client/session-settings";

function usage(why: string): never {
  process.stderr.write(`codex-probe: ${why}\n`);
  process.exit(2);
}

const separator = process.argv.indexOf("--");
if (separator === -1) usage("expected -- <natsprobe> <args...>");
const options = new Map<string, string>();
const named = process.argv.slice(2, separator);
for (let index = 0; index < named.length; index += 2) {
  const name = named[index];
  const value = named[index + 1];
  if (name === undefined || !name.startsWith("--") || value === undefined) {
    usage(`expected --name value pairs, got ${named.join(" ")}`);
  }
  options.set(name.slice(2), value);
}
const environFile = options.get("environ");
const session = options.get("session");
const cwd = options.get("cwd");
const command = process.argv.slice(separator + 1);
if (
  environFile === undefined ||
  session === undefined ||
  cwd === undefined ||
  command.length === 0
) {
  usage("--environ, --session, --cwd and a natsprobe command are required");
}

let environ: string;
try {
  environ = readFileSync(environFile, "utf8");
} catch (error) {
  usage(`the plugin server's environment cannot be read from ${environFile}: ${String(error)}`);
}
const env: Record<string, string> = {};
for (const entry of environ.split("\0")) {
  const equals = entry.indexOf("=");
  if (equals > 0) env[entry.slice(0, equals)] = entry.slice(equals + 1);
}
if (Object.keys(env).length === 0) usage(`${environFile} holds no environment`);

const { envoy } = sessionSettings(sessionConfigFile("codex", env), env, {
  cwd,
  session: () => session,
});
if (!envoy.enabled)
  usage(`the plugin's settings turn Envoy off there: ${envoy.error ?? envoy.notice}`);
if ("off" in envoy.inbound) {
  usage(`the plugin's settings take no inbound messages there: ${envoy.inbound.off}`);
}
const kind = envoy.inbound.natsCredential.kind;
const said =
  kind === "broker"
    ? "the session's broker token"
    : kind === "seed"
      ? "an nkey seed"
      : "no credential (neither NATS_AUTH in its environment nor natsAuth in its envoy.json names one)";
process.stdout.write(
  `codex-probe: the plugin's settings present ${said} in its server's environment\n`
);
if (kind === "seed") usage("an nkey seed is no credential natsprobe presents");

const probe = Bun.spawnSync(kind === "broker" ? command : [...command, "--no-credential"], {
  env: { ...env, AGENT_SECRETS_SESSION: session },
  stdout: "inherit",
  stderr: "inherit",
});
process.exit(probe.exitCode ?? 2);
