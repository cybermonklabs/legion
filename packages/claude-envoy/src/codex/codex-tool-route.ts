import { chmod, mkdtemp, readFile, rename, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { messageFor } from "@legion/envoy-client/errors"
import type { Server } from "bun"
import { z } from "zod"

/**
 * How an Envoy tool call reaches the thread it acts as, under Codex's shared app server.
 *
 * Codex sends a thread's tool calls to the plugin server it started for that thread, and names the
 * thread on each call (`_meta.threadId`). The thread's Envoy session, though, runs in the server
 * that owns the process (`./codex-threads`), which may be another thread's. So the owner serves
 * the Envoy tools of every thread on a Unix socket of its own, and names that socket in the servers
 * directory it was elected in (`toolRouteFile`); a server that stands by sends each Envoy tool call
 * there, with the thread Codex named, and answers with what the owner answered. The socket is in a
 * directory only this user can enter (`mkdtemp`), and the file naming it in the plugin's own state
 * directory, so only this user's processes reach it.
 */
export interface ToolCall {
  /** The Codex thread whose turn made the call. */
  readonly thread: string
  /** The Envoy tool. */
  readonly name: string
  /** Its arguments, as the model gave them. */
  readonly arguments: unknown
}

const ToolCallBody = z.object({
  thread: z.string().min(1),
  name: z.string().min(1),
  arguments: z.unknown(),
})

// The error first: an object with no `value` still parses as `{ value: undefined }`.
const ToolAnswer = z.union([z.object({ error: z.string() }), z.object({ value: z.unknown() })])

const ToolRoute = z.object({ socket: z.string().min(1) })

/** The file in a shared app server's servers directory (`codexServersDirectory`) that names the
 *  socket its owner serves the threads' Envoy tools on. */
export function toolRouteFile(serversDirectory: string): string {
  return join(serversDirectory, "owner-route.json")
}

/** The path every call is posted to: over a Unix socket the host names nothing. */
const TOOL_URL = "http://codex-envoy/tool"

/** How long a forwarded call may take: the bound the `dispatch` tool has, since an Envoy tool waits
 *  on the listener as that waits on Dispatch. */
const FORWARD_TIMEOUT_MS = 120_000

export interface ToolRouteServer {
  /** Stops serving, and removes the route file while it still names this server. */
  stop(): Promise<void>
}

/**
 * Serves `run` for every thread's Envoy tool calls on a socket of its own, and names it in
 * `serversDirectory`'s route file, replacing whatever an earlier owner left there. `run` answers
 * the tool's value, or throws with the reason the call failed, which the caller is answered.
 */
export async function serveToolRoute(
  serversDirectory: string,
  run: (call: ToolCall) => Promise<unknown>,
): Promise<ToolRouteServer> {
  const directory = await mkdtemp(join(tmpdir(), "codex-envoy-"))
  const socket = join(directory, "route.sock")
  let server: Server<undefined>
  try {
    server = Bun.serve({
      unix: socket,
      fetch: async (request) => {
        const call = ToolCallBody.safeParse(await request.json().catch(() => undefined))
        if (!call.success) return Response.json({ error: "not an Envoy tool call" })
        try {
          return Response.json({ value: (await run(call.data)) ?? null })
        } catch (error) {
          return Response.json({ error: messageFor(error) })
        }
      },
    })
  } catch (error) {
    await rm(directory, { force: true, recursive: true })
    throw error
  }
  try {
    await chmod(socket, 0o600)
    const temporary = join(serversDirectory, `.owner-route.${process.pid}.tmp`)
    await writeFile(temporary, JSON.stringify({ socket }), { mode: 0o600 })
    await rename(temporary, toolRouteFile(serversDirectory))
  } catch (error) {
    await server.stop(true)
    await rm(directory, { force: true, recursive: true })
    throw error
  }
  return {
    async stop() {
      if ((await routeSocket(serversDirectory)) === socket) {
        await rm(toolRouteFile(serversDirectory), { force: true })
      }
      await server.stop(true)
      await rm(directory, { force: true, recursive: true })
    },
  }
}

/** The socket the route file names, or undefined when there is none or it does not parse. */
async function routeSocket(serversDirectory: string): Promise<string | undefined> {
  try {
    const parsed = ToolRoute.safeParse(
      JSON.parse(await readFile(toolRouteFile(serversDirectory), "utf8")),
    )
    return parsed.success ? parsed.data.socket : undefined
  } catch {
    return undefined
  }
}

/**
 * Sends `call` to the server that owns the shared app server whose servers directory is
 * `serversDirectory`, and answers the tool's value. Throws with the owner's reason when the call
 * failed there, and says so when no owner can be reached: the owner may be changing, since Codex
 * stops a server when it unloads that server's thread, and the next one takes over within seconds.
 */
export async function forwardToolCall(serversDirectory: string, call: ToolCall): Promise<unknown> {
  const socket = await routeSocket(serversDirectory)
  const unreachable = (why: string): Error =>
    new Error(
      `Envoy is not ready: the plugin server that runs the Envoy sessions of this shared app server's threads cannot be reached (${why}); when Codex unloads that server's thread the next server takes over within seconds, so try again`,
    )
  if (socket === undefined) throw unreachable("no server has taken them over yet")
  let answer: unknown
  try {
    const response = await fetch(TOOL_URL, {
      body: JSON.stringify(call),
      headers: { "content-type": "application/json" },
      method: "POST",
      signal: AbortSignal.timeout(FORWARD_TIMEOUT_MS),
      unix: socket,
    })
    answer = await response.json()
  } catch (error) {
    throw unreachable(messageFor(error))
  }
  const parsed = ToolAnswer.safeParse(answer)
  if (!parsed.success) throw unreachable("its answer is not one this server reads")
  if ("error" in parsed.data) throw new Error(parsed.data.error)
  return parsed.data.value
}
