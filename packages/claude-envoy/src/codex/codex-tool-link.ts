import { mkdtemp, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { messageFor } from "@legion/envoy-client/errors"
import { z } from "zod"
import { writeAtomicStateFile } from "../session-state"
import { type CodexServerEntry, currentOwner } from "./codex-owner"
import { threadIdShape } from "./codex-thread-id"

const ToolCall = z.object({
  thread: z.string().refine(threadIdShape),
  name: z.string(),
  args: z.unknown(),
})

type ToolCall = z.infer<typeof ToolCall>
type ToolAnswer = { result: unknown } | { error: string }

/** The pointer belongs to this publication of the owner. A new owner never reuses its socket. */
function endpoint(directory: string, owner: CodexServerEntry): string {
  return join(directory, `${owner.seq}.${owner.pid}.socket`)
}

/** Codex sends a thread's tools to its own MCP process. The owner alone holds its subscribed
 *  Envoy runtime, so the other MCP processes forward the request to this private Unix socket. */
export async function serveSharedCodexTools(
  directory: string,
  owner: CodexServerEntry,
  call: (request: ToolCall) => Promise<unknown>,
): Promise<() => Promise<void>> {
  // A mode-0700 temporary directory also keeps the address short when CODEX_HOME is deep.
  const socketDirectory = await mkdtemp(join(tmpdir(), "codex-envoy-"))
  const unix = join(socketDirectory, "tools.sock")
  const pointer = endpoint(directory, owner)
  let server: ReturnType<typeof Bun.serve> | undefined
  const close = async (): Promise<void> => {
    await server?.stop(true)
    await rm(pointer, { force: true })
    await rm(socketDirectory, { recursive: true, force: true })
  }
  try {
    server = Bun.serve({
      unix,
      async fetch(request, peer) {
        // An accepted tool call (for example, a send that waits for delivery) has its own bound.
        peer.timeout(request, 0)
        if (request.method !== "POST") return new Response(null, { status: 405 })
        try {
          const tool = ToolCall.parse(await request.json())
          return Response.json({ result: await call(tool) } satisfies ToolAnswer)
        } catch (error) {
          return Response.json({ error: messageFor(error) } satisfies ToolAnswer)
        }
      },
    })
    await writeAtomicStateFile(pointer, unix)
    return close
  } catch (error) {
    await close()
    throw error
  }
}

export async function callSharedCodexTool(
  directory: string,
  request: ToolCall,
  signal: AbortSignal,
): Promise<unknown> {
  const owner = await currentOwner(directory)
  const unix =
    owner === undefined
      ? undefined
      : await readFile(endpoint(directory, owner), "utf8").catch(() => undefined)
  if (unix === undefined) throw new Error("Envoy is not ready: the Codex plugin owner is starting")
  // Do not retry across an owner change. The previous owner may have executed the call already.
  const response = await fetch("http://localhost/", {
    unix,
    method: "POST",
    body: JSON.stringify(request),
    signal,
  })
  const answer = (await response.json()) as ToolAnswer
  if ("error" in answer) throw new Error(answer.error)
  return answer.result
}
