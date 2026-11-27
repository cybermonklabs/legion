import { type ChildProcess, spawn } from "node:child_process"
import { closeSync, mkdirSync, openSync, statSync } from "node:fs"
import { dirname, join } from "node:path"
import { mcpServers } from "../../codex/.codex-plugin/plugin.json" with { type: "json" }

/** The most a standby log keeps: a standby that finds it larger starts it again. */
const STANDBY_LOG_LIMIT_BYTES = 1024 * 1024

/** The standby's log, beside the entries of the app server's plugin servers: no process reads a
 *  detached standby's stderr, so a standby that fails before it publishes says why here. */
export function codexStandbyLogFile(serversDirectory: string): string {
  return join(serversDirectory, "standby.log")
}

/** Opens the standby's log for appending, or afresh when it has reached its limit. */
function openStandbyLog(file: string): number {
  mkdirSync(dirname(file), { recursive: true, mode: 0o700 })
  let size = 0
  try {
    size = statSync(file).size
  } catch {
    // No log yet.
  }
  return openSync(file, size >= STANDBY_LOG_LIMIT_BYTES ? "w" : "a", 0o600)
}

/** Start the same owner code outside the process group Codex may stop with its MCP child. Its
 *  stderr goes to `logFile` (`codexStandbyLogFile`). */
export function spawnCodexEnvoyStandby(
  bundlePath: string,
  codexPid: number,
  appServerStart: string,
  logFile: string,
  hookStateDirectory?: string,
): ChildProcess {
  const args = [bundlePath, "--codex-standby", String(codexPid), appServerStart]
  let env: NodeJS.ProcessEnv | undefined
  if (hookStateDirectory !== undefined) {
    args.push("--from-hook")
    // A hook inherits more than an MCP server. Give its child only the plugin's MCP environment,
    // the usual process-launch variables, and the private state directory Codex gave the hook.
    env = { PLUGIN_DATA: hookStateDirectory }
    for (const name of [
      "HOME",
      "PATH",
      "USER",
      "TMPDIR",
      "TEMP",
      "TMP",
      "LANG",
      "LC_ALL",
      ...mcpServers.envoy.env_vars,
    ]) {
      const value = process.env[name]
      if (value !== undefined) env[name] = value
    }
  }
  const log = openStandbyLog(logFile)
  try {
    const child = spawn(process.execPath, args, {
      cwd: dirname(dirname(bundlePath)),
      detached: true,
      stdio: ["ignore", "ignore", log],
      ...(env === undefined ? {} : { env }),
    })
    child.unref()
    return child
  } finally {
    // The child holds its own copy of the descriptor.
    closeSync(log)
  }
}
