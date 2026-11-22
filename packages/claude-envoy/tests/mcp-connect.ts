import type { Client } from "@modelcontextprotocol/sdk/client/index.js"
import type { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { PROCESS_WAIT_MS } from "./reopen-server"

/**
 * Connects `client` to the channel server `transport` starts, waiting for its answer to
 * `initialize` up to `PROCESS_WAIT_MS` (the last resort of every wait on a whole server process),
 * not the MCP SDK's own 60 s, and failing by name, with what the server wrote to stderr, when it
 * never answers.
 */
export async function connectedToServer(
  client: Client,
  transport: StdioClientTransport,
  stderr: () => string,
): Promise<void> {
  await client.connect(transport, { timeout: PROCESS_WAIT_MS }).catch((error: unknown) => {
    throw new Error(
      `the channel server did not answer initialize: ${String(error)}; its stderr: ${stderr()}`,
    )
  })
}
