// packages/envoy-client/src/dispatch-commands/host-session.ts
//
// The session a `dispatch` command acts as under the host that runs it, and the Dispatch it reads
// there (`hostSession`, `hostedDispatch`), or, where no host runs `dispatch api`, the Dispatch it
// reads from the environment alone (`unhostedDispatch`); `dispatch-cli.ts` takes each from here.
// Neither `dispatch-cli.ts` nor `api.ts` is imported here, so neither import is a cycle.

import { brokerRouteConfigured } from "../broker-credential";
import {
  type ActiveDispatchConfig,
  type DispatchEnvironment,
  dispatchSettings,
  tokenFilesSet,
} from "../dispatch-config";
import type { DispatchHost } from "../dispatch-cwd";
import type { EnvoyConfig } from "../envoy-config";
import {
  handedConfigFile,
  type SessionConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
} from "../session-settings";

const HOSTS = ["omp", "claude", "codex"] as const satisfies readonly DispatchHost[];

/** What `dispatch api` refuses with, where it has no host, when the environment names no
 *  Dispatch or no credential for it. */
const NO_DISPATCH =
  "no Dispatch is set; set DISPATCH_URL, and a credential (DISPATCH_TOKEN, DISPATCH_TOKEN_FILE, DISPATCH_BROKER_TOKEN_FILE and DISPATCH_BROKER_DPOP_KEY_FILE, or the broker's AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY)";

/** The configuration a command with no host reads: no envoy.json at all, so no file under HOME
 *  speaks for a job that no session runs, and Dispatch is read from the environment alone. */
const NO_FILE: EnvoyConfig = {
  kind: "read",
  path: "",
  dispatch: {},
  envoy: {},
  checkout: null,
  missing: null,
  warnings: [],
};

/** The host a command runs under and the session it acts as there, with the envoy.json that
 *  host's plugin chose, or the usage error a command refuses the absence of either with. */
export type HostSession =
  | {
      readonly host: DispatchHost;
      readonly sessionId: string;
      readonly file: SessionConfigFile;
    }
  | { readonly problem: string };

/**
 * The session a command acts as under the host `hostName` names (`commandHost`): Claude Code's
 * session, Codex's `CODEX_THREAD_ID`, else `DISPATCH_SESSION_ID`; and the file the session's
 * plugin handed its shell (each plugin sets `DISPATCH_HOST` and `ENVOY_CONFIG_FILE` together),
 * else the host's own rule. A host that is none of `HOSTS`, or a session id that is not set, is
 * the usage error every command refuses with.
 */
export function hostSession(
  hostName: string | undefined,
  env: Readonly<Record<string, string>>
): HostSession {
  const host = HOSTS.find((name) => name === hostName);
  if (host === undefined) {
    return {
      problem:
        hostName === undefined
          ? "dispatch: DISPATCH_HOST is not set. The host plugin sets it in an agent's shell. From an agent's session, send the command as a bash call. To run dispatch by hand, set DISPATCH_HOST, and set DISPATCH_SESSION_ID to the id of your own session."
          : `dispatch: DISPATCH_HOST is ${JSON.stringify(hostName)}, not omp, claude or codex.`,
    };
  }
  // Under Claude Code the session is the one claude-envoy registers and claims under: its QA
  // override ENVOY_SESSION_ID, else CLAUDE_CODE_SESSION_ID (`claudeSessionId` in claude-envoy).
  const [sessionId, sessionVariable] =
    host === "claude"
      ? [
          env.ENVOY_SESSION_ID ?? env.CLAUDE_CODE_SESSION_ID,
          "ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID",
        ]
      : host === "codex"
        ? [env.CODEX_THREAD_ID, "CODEX_THREAD_ID"]
        : [env.DISPATCH_SESSION_ID, "DISPATCH_SESSION_ID"];
  if (sessionId === undefined) {
    return {
      problem: `dispatch: ${sessionVariable} is not set, so this call has no session to act as. The host plugin sets it in an agent's shell.`,
    };
  }
  const file =
    (env.DISPATCH_HOST === undefined ? null : handedConfigFile(env)) ??
    sessionConfigFile(host, env);
  return { host, sessionId, file };
}

/** Dispatch as a command under a host reads it, or the usage error the command refuses with;
 *  either way with the lines the read prints first, each `dispatch: ` and a file the read found
 *  and did not read (a checkout's OpenCode-era file). */
export type HostedDispatch = { readonly warnings: readonly string[] } & (
  | { readonly config: ActiveDispatchConfig }
  | { readonly problem: string }
);

/**
 * Dispatch from `file`, the envoy.json `hostSession` found, read now under `options.session`, the
 * session the command acts as: a broker trade writes under it, so the token the command presents
 * names it rather than naming none or another of this machine's. The command is the one caller
 * that takes the token files (`tokenFilesTarget`, dispatch-config.ts). A file whose location could
 * not be worked out, and a Dispatch that is off, are the usage error the command refuses with.
 * Each caller prints the lines to its own stream.
 */
export async function hostedDispatch(
  file: SessionConfigFile,
  env: DispatchEnvironment,
  options: { readonly cwd: string; readonly session: string }
): Promise<HostedDispatch> {
  if (file.path === null) return { problem: `dispatch: ${file.error}`, warnings: [] };
  const { dispatch: resolution, warnings } = await sessionDispatchSettings(file, env, {
    ...options,
    tokenFiles: true,
  });
  const lines = warnings.map((warning) => `dispatch: ${warning}`);
  if (!resolution.enabled) {
    return {
      problem:
        resolution.error === null
          ? `dispatch: ${resolution.notice}`
          : `dispatch: dispatch config: ${resolution.error}`,
      warnings: lines,
    };
  }
  return { config: resolution, warnings: lines };
}

/** Whether the environment names no Dispatch credential: no bearer variable, and no broker and
 *  identity to trade. A broker setting that cannot be read names one, whose problem is said. */
function namesNoCredential(env: DispatchEnvironment): boolean {
  if (env.DISPATCH_TOKEN !== undefined || env.DISPATCH_TOKEN_FILE !== undefined) return false;
  try {
    return !brokerRouteConfigured(env).traded;
  } catch {
    return false;
  }
}

/**
 * Dispatch as `dispatch api` reads it where no host is found (`commandHost`), as in a GitHub
 * Actions job: from the environment alone, reading no envoy.json, its broker trade naming the
 * session of DISPATCH_SESSION_ID, else of AGENT_SECRETS_SESSION, else the session the job entry
 * fixes; or the usage error the command refuses with, each word of it the command's own.
 */
export async function unhostedDispatch(
  env: DispatchEnvironment
): Promise<{ readonly config: ActiveDispatchConfig } | { readonly problem: string }> {
  const resolution = await dispatchSettings(env, NO_FILE, {
    session: env.DISPATCH_SESSION_ID,
    tokenFiles: true,
  });
  if (resolution.enabled) return { config: resolution };
  // The off texts of an unset Dispatch or credential name an envoy.json, which a command with no
  // host does not read; any other says what was wrong with what is set, the token files' refusal
  // among them, which names DISPATCH_URL where it is unset beside them.
  return {
    problem:
      tokenFilesSet(env) === undefined && (env.DISPATCH_URL === undefined || namesNoCredential(env))
        ? `dispatch api was not called: ${NO_DISPATCH}`
        : `dispatch api was not called: dispatch config: ${resolution.error ?? resolution.notice}`,
  };
}
