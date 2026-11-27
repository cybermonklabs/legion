import type { Client } from "@modelcontextprotocol/sdk/client/index.js"
import type { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { untilTheGuard } from "./row-deadline"

/**
 * Connects `client` to the channel server `transport` starts, waiting for its answer to
 * `initialize` until the row's named deadline (`untilTheGuard`, 5 s inside the row's guard,
 * `guardMs`, `ROW_GUARD_MS`'s 120 s unless the row states another) rather than the MCP SDK's own
 * 60 s, and failing by name, with what the server wrote to stderr, when it never answers. A row
 * that uses it calls `rowStarted` from a `beforeEach` of its own.
 */
export async function connectedToServer(
  client: Client,
  transport: StdioClientTransport,
  stderr: () => string,
  guardMs?: number,
): Promise<void> {
  await client.connect(transport, { timeout: untilTheGuard(guardMs) }).catch((error: unknown) => {
    throw new Error(
      `the channel server did not answer initialize: ${String(error)}; its stderr: ${stderr()}`,
    )
  })
}
