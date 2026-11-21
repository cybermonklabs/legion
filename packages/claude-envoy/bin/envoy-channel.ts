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
  // wrote has reached the reader, rather than when that handle lets go. This flush has no bound of
  // its own, unlike the exit on the way out's bound (`EXIT_FLUSH_MS`, `../src/way-out`): a way out
  // that ended in time leaves every byte to its reader, the MCP client, which reads stdout and
  // stderr until it kills the process. What bounds it is that kill: Claude Code's MCP SDK client
  // sends SIGKILL 4 s after it ends stdin (`CLIENT_KILL_MS`), and Codex's sends it to the
  // server's process group 2 s after its SIGTERM (`PROCESS_GROUP_TERM_GRACE_PERIOD`,
  // codex-rs/rmcp-client/src/stdio_server_launcher.rs, rust-v0.154.0).
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
