import { runCodexEnvoyServer } from "../src/codex/codex-server"
import { runEnvoyChannelServer } from "../src/envoy-channel-server"

// `.codex-plugin/plugin.json` starts this bundle with `--host codex`; Claude Code's `.mcp.json`
// starts it with no argument.
const codex = process.argv.slice(2).join(" ") === "--host codex"

try {
  await (codex ? runCodexEnvoyServer() : runEnvoyChannelServer())
} catch (error) {
  const message = error instanceof Error ? error.message : "unknown channel server failure"
  process.stderr.write(`${codex ? "Codex" : "Claude"} Envoy channel server failed: ${message}\n`)
  process.exitCode = 1
}
