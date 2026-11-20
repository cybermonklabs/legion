import { scrubProcessStreams } from "@legion/contracts/held-secrets"
import { exitProcess } from "@legion/envoy-client/process-exit"
import { runCodexEnvoyServer } from "../src/codex/codex-server"
import { runEnvoyChannelServer } from "../src/envoy-channel-server"

// `codex/.codex-plugin/plugin.json` starts this bundle with `--host codex`; Claude Code's
// `.mcp.json` starts it with no argument.
const codex = process.argv.slice(2).join(" ") === "--host codex"

// Every line this process writes, its MCP stream on stdout and its failures on stderr, passes the
// scrub of the secrets it holds (`@legion/contracts/held-secrets`).
scrubProcessStreams()

try {
  await (codex ? runCodexEnvoyServer() : runEnvoyChannelServer())
  // The server's teardown has run. A handle nothing will use can outlive it (a NATS dial that a
  // stop ended waits out nats.js's own dial timeout), so the process ends here, once what it
  // wrote has reached the reader, rather than when that handle lets go.
  await exitProcess(0)
} catch (error) {
  const message = error instanceof Error ? error.message : "unknown channel server failure"
  // The server's own teardown has run (its `finally`): the mod link is closed and the session, if
  // one started, deregistered. A failed start ends the process, rather than leaving it alive on
  // whatever handle is left (its stdin, a timer) with no session behind it, once the message and
  // everything before it has been written.
  process.stderr.write(`${codex ? "Codex" : "Claude"} Envoy channel server failed: ${message}\n`)
  await exitProcess(1)
}
