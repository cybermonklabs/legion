// packages/envoy-client/src/dispatch-commands/host-session.ts
//
// The session a `dispatch` command acts as under the host that runs it, and the Dispatch it reads
// there, which `dispatch-cli.ts` takes from here. `dispatch api` (`api.ts`) takes the session
// variables alone, for the AskGate records it reads before a write. Neither file is imported
// here, so neither import is a cycle.

import type { ActiveDispatchConfig, DispatchEnvironment } from "../dispatch-config";
import type { DispatchHost } from "../dispatch-cwd";
import {
  handedConfigFile,
  type SessionConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
} from "../session-settings";

const HOSTS = ["omp", "claude", "codex"] as const satisfies readonly DispatchHost[];

/**
 * The variables a `dispatch` command reads a session's id from, in the order the refusal ledger
 * takes the first one set under any host but Codex (`runDispatchCli`): claude-envoy's QA override
 * and then Claude Code's own id (`claudeSessionId` in claude-envoy), the id an Oh My Pi plugin
 * sets or a person names, and Codex's thread id, which alone is read under Codex. `dispatch api`
 * reads the AskGate record of each one set before a write, so every session the ledger can be
 * written for is one whose gate it reads.
 */
export const SESSION_VARIABLES = [
  "ENVOY_SESSION_ID",
  "CLAUDE_CODE_SESSION_ID",
  "DISPATCH_SESSION_ID",
  "CODEX_THREAD_ID",
] as const;

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
  // biome-ignore-start lint/complexity/useLiteralKeys: claude-envoy type-checks this source under noPropertyAccessFromIndexSignature, which refuses a dot on an index signature
  const [sessionId, sessionVariable] =
    host === "claude"
      ? [
          env["ENVOY_SESSION_ID"] ?? env["CLAUDE_CODE_SESSION_ID"],
          "ENVOY_SESSION_ID or CLAUDE_CODE_SESSION_ID",
        ]
      : host === "codex"
        ? [env["CODEX_THREAD_ID"], "CODEX_THREAD_ID"]
        : [env["DISPATCH_SESSION_ID"], "DISPATCH_SESSION_ID"];
  if (sessionId === undefined) {
    return {
      problem: `dispatch: ${sessionVariable} is not set, so this call has no session to act as. The host plugin sets it in an agent's shell.`,
    };
  }
  const file =
    (env["DISPATCH_HOST"] === undefined ? null : handedConfigFile(env)) ??
    sessionConfigFile(host, env);
  // biome-ignore-end lint/complexity/useLiteralKeys: see above
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
