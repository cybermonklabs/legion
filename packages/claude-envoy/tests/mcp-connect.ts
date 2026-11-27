import { LONGEST_TIMER_MS, settled, timeLeft } from "@legion/test-fixtures/wait"
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

/** How far past the row's deadline an MCP request's own timeout lands (`pastTheDeadline`). */
const PAST_THE_DEADLINE_MS = 1_000

/**
 * The timeout to give an MCP request that a shared wait (`until`'s probe, or `settled`) waits on:
 * a second past the row's deadline, so the wait gives up first and names its fact. A request whose
 * timeout is the deadline itself (`timeLeft()`) races it, and when the SDK's timer wins the row
 * fails naming only "MCP error -32001: Request timed out". The SDK's own default, 60 s, would end
 * a request that a loaded host still answers inside the row's deadline. It is capped as
 * `timeLeft()` is, at the longest delay a timer takes, so that it can be handed to a timer.
 */
export function pastTheDeadline(): number {
  return Math.min(LONGEST_TIMER_MS, timeLeft() + PAST_THE_DEADLINE_MS)
}

/**
 * `connectedToServer` for a row that waits through `@legion/test-fixtures/wait`: the wait for the
 * answer to `initialize` ends at the row's deadline (`settled`), naming it, with the request's own
 * timeout just past that deadline (`pastTheDeadline`) in place of the MCP SDK's 60 s.
 */
export async function connectedBeforeTheDeadline(
  client: Client,
  transport: StdioClientTransport,
  stderr: () => string,
): Promise<void> {
  await settled(
    "the channel server's answer to initialize",
    client.connect(transport, { timeout: pastTheDeadline() }),
  ).catch((error: unknown) => {
    throw new Error(
      `the channel server did not answer initialize: ${String(error)}; its stderr: ${stderr()}`,
    )
  })
}

/**
 * The answer to the tool call `params`, waited on through `settled` until the row's deadline, with
 * the request's own timeout a second past it (`pastTheDeadline`), so the wait names `what` first.
 */
export function toolAnswer(
  client: Client,
  what: string,
  params: Parameters<Client["callTool"]>[0],
) {
  return settled(what, client.callTool(params, undefined, { timeout: pastTheDeadline() }))
}
