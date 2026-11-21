// scripts/e2e/lib/operator-nats-seed.ts: the nkey seed a command run from the operator's shell
// sends to a NATS it names (envoy-bridge.ts's upstream, nats-stream.ts's server). It is what
// `sessionEnvoySettings` resolves over the envoy.json `envoyConfigPath("command")` chooses with
// ENVOY_NATS_URL set to those servers (packages/envoy-client/src/session-settings.ts), so the
// operator's `envoy.natsSeed` serves these scripts as it serves the operator's sessions, and only
// against the servers written beside it.
import type { EnvoyEnvironment, NatsSeed } from "../../../packages/envoy-client/src/envoy-settings";
import { LoggableError } from "../../../packages/envoy-client/src/errors";
import {
  sessionConfigFile,
  sessionEnvoySettings,
} from "../../../packages/envoy-client/src/session-settings";

/**
 * The seed for `servers` (comma-separated, as ENVOY_NATS_URL takes them): NATS_NKEY_SEED_FILE or
 * NATS_NKEY_SEED, else the chosen file's `envoy.natsSeed` when its `envoy.natsUrls` are exactly
 * these servers; undefined when neither names one, for a server that asks for no credential.
 * Rejects naming `label` when Envoy resolves none: a file that turns Envoy off or cannot be read, a
 * seed variable that cannot be read, or a file's seed for other servers (which leave Envoy on with
 * inbound messages off, `inbound.off` naming the refusal, and are a refusal here all the same).
 * Rejects too where the operator's settings name a session's broker token (`NATS_AUTH=broker`): a
 * hub's auth service grants that token one session's own channels, and these scripts read the
 * hub's other subjects and its streams, so they connect with a seed or with none. These scripts
 * name no session, so the settings turn inbound off for it (`EnvoyInbound.noSessionNamed`), and
 * the refusal names the seed to set. Each refusal is composed of the settings' own words, never a
 * value they hold, so a failure line prints it (`LoggableError`).
 */
export async function operatorNatsSeed(
  servers: string,
  label: string,
  env: EnvoyEnvironment,
  cwd: string
): Promise<NatsSeed | undefined> {
  const named = { ...env, ENVOY_NATS_URL: servers };
  const settings = await sessionEnvoySettings(sessionConfigFile("command", named), named, { cwd });
  const refused = `the NATS seed for ${label} (resolved as ENVOY_NATS_URL)`;
  const broker = new LoggableError(
    `${refused}: NATS_AUTH (or envoy.natsAuth) names a session's broker token, which a hub grants only that session's own channels; set NATS_NKEY_SEED_FILE for ${label}`
  );
  if (!settings.enabled)
    throw new LoggableError(`${refused}: ${settings.error ?? settings.notice}`);
  const { inbound } = settings;
  if ("off" in inbound) {
    if (inbound.noSessionNamed === true) throw broker;
    throw new LoggableError(`${refused}: ${inbound.off}`);
  }
  const credential = inbound.natsCredential;
  switch (credential.kind) {
    case "seed":
      return credential.seed;
    case "none":
      return undefined;
    case "broker":
      throw broker;
    default: {
      // No tsconfig covers scripts/e2e, so a credential kind added later fails here by name
      // rather than connecting these scripts with no credential.
      const unknown: never = credential;
      throw new LoggableError(`${refused}: an unknown NATS credential ${JSON.stringify(unknown)}`);
    }
  }
}
