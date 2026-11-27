import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

import { baseUrl, e2eAgentToken } from "./api";
import { cliTranspilerCacheEnv } from "./cli-transpiler-cache";

// The command is the one agents run - `packages/envoy-client/bin/dispatch.ts`, its own process -
// so a row spawns that entry rather than calling the library behind it.
export const dispatchEntry = fileURLToPath(
  new URL("../../envoy-client/bin/dispatch.ts", import.meta.url)
);

/** The session the command acts as: the server records it as each write's actor, and a personal
 *  token's act names it (`actingPerson` in `packages/envoy/internal/dispatch/api/server.go`). */
export const cliSession = "e2e-dispatch-cli";

export interface CliRun {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/**
 * Runs the real `dispatch` entry against the harness's Dispatch, as a Legion pane's agent runs it.
 *
 * The child's environment is written here rather than inherited: `DISPATCH_TOKEN_FILE` wins over
 * `DISPATCH_TOKEN` in this command (`packages/envoy-client/src/dispatch-config.ts`), and a Legion
 * pane exports one, so an inherited environment would send another Dispatch's token - or point the
 * command at another Dispatch entirely through its own `DISPATCH_URL` or `ENVOY_CONFIG_FILE`.
 * `HOME` and `DISPATCH_STATE_DIR` are the row's own temporary directory, so the command reads no
 * developer's `envoy.json` and writes its session state nowhere but there. The one path outside
 * it is Bun's transpiler cache, the run's own directory where `globalSetup` made one
 * (`e2e/cli-transpiler-cache.ts`): it holds compiled modules and nothing the command reads as
 * configuration or state, and each call is still a fresh `bun` process of its own.
 *
 * `options.token` is the bearer the command presents: the harness's shared agent token unless a
 * row hands it one of its own, which is how a row runs the command under a person's own personal
 * token, as an agent acting for its person runs it.
 *
 * `options.stdin` is the bytes the command reads on its standard input, as a pipe hands them to a
 * `--content-file -`, and then the end of that input; without it the command's standard input is
 * ignored.
 */
export function runDispatch(
  args: readonly string[],
  home: string,
  { token = e2eAgentToken, stdin }: { readonly token?: string; readonly stdin?: Uint8Array } = {}
): Promise<CliRun> {
  const argv = [dispatchEntry, ...args];
  const options = {
    cwd: home,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: home,
      DISPATCH_STATE_DIR: home,
      DISPATCH_HOST: "omp",
      DISPATCH_SESSION_ID: cliSession,
      DISPATCH_URL: baseUrl,
      DISPATCH_TOKEN: token,
      ...cliTranspilerCacheEnv(),
    },
  };
  const child =
    stdin === undefined
      ? spawn("bun", argv, { ...options, stdio: ["ignore", "pipe", "pipe"] })
      : spawn("bun", argv, { ...options, stdio: ["pipe", "pipe", "pipe"] });
  const finished = Promise.withResolvers<CliRun>();
  if (child.stdin !== null) {
    // A command that exits before it has read the whole input fails the row here rather than
    // leaving the write's EPIPE unhandled.
    child.stdin.once("error", finished.reject);
    child.stdin.end(stdin);
  }
  let stdout = "";
  let stderr = "";
  child.stdout.setEncoding("utf8");
  child.stdout.on("data", (chunk: string) => {
    stdout += chunk;
  });
  child.stderr.setEncoding("utf8");
  child.stderr.on("data", (chunk: string) => {
    stderr += chunk;
  });
  child.once("error", finished.reject);
  child.once("close", (code) => finished.resolve({ code: code ?? -1, stdout, stderr }));
  return finished.promise;
}
