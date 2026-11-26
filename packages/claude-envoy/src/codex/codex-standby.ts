import { type ChildProcess, spawn } from "node:child_process"
import { dirname } from "node:path"
import { mcpServers } from "../../codex/.codex-plugin/plugin.json" with { type: "json" }

/** Start the same owner code outside the process group Codex may stop with its MCP child. */
export function spawnCodexEnvoyStandby(
  bundlePath: string,
  codexPid: number,
  appServerStart: string,
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
  const child = spawn(process.execPath, args, {
    cwd: dirname(dirname(bundlePath)),
    detached: true,
    stdio: "ignore",
    ...(env === undefined ? {} : { env }),
  })
  child.unref()
  return child
}
