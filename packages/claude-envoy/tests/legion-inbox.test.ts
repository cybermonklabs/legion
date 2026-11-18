import { afterEach, expect, test } from "bun:test"
import { createServer, type Server, type Socket } from "node:net"
import { join } from "node:path"
import { socketPathRefusal, socketRoot } from "@legion/test-fixtures/socket-root"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import { createInboxNotifier } from "../src/legion-inbox"

// Each inbox's unix socket stands in for the bridge's, which Go binds
// (packages/daemon/internal/claudebridge/inbox.go) and so holds to the 107 bytes a socket path holds
// on Linux (`socketPathRefusal`); Bun's own server binds past that. So it goes in a directory of its
// own, made under TMPDIR when its path fits there and under /tmp otherwise, which says so
// (`socketRoot`). The maker names a directory <prefix>-XXXXXX.
const prefix = "legion-inbox"
const directory = temporaryDirectories(prefix, {
  root: socketRoot("legion-inbox.test.ts's inbox", (root) =>
    socketPathRefusal(join(root, `${prefix}-XXXXXX`, "inbox.sock")),
  ),
})
const servers: Server[] = []
afterEach(() => {
  for (const server of servers.splice(0)) server.close()
})

/** A stand-in for the bridge's inbox (`claudebridge/inbox.go`): it records each line and answers
 * it with `answer`. */
async function inbox(answer: (delivery: { id: string; content: string }) => object): Promise<{
  readonly socketPath: string
  readonly lines: { id: string; content: string }[]
}> {
  const socketPath = join(directory(), "inbox.sock")
  const lines: { id: string; content: string }[] = []
  const server = createServer((socket: Socket) => {
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const delivery = JSON.parse(buffered.slice(0, end))
        buffered = buffered.slice(end + 1)
        lines.push(delivery)
        socket.write(`${JSON.stringify(answer(delivery))}\n`)
      }
    })
  })
  servers.push(server)
  await new Promise<void>((listening) => server.listen(socketPath, listening))
  return { socketPath, lines }
}

function notification(content: string, eventID?: string) {
  return { content, ...(eventID === undefined ? {} : { eventId: eventID }) }
}

test("writes each delivery as one line keyed by its event id and resolves on the bridge's ok", async () => {
  const { socketPath, lines } = await inbox(({ id }) => ({ id, ok: true }))
  const notifier = createInboxNotifier(socketPath)
  await notifier.notification(notification("envoy:\n  id: evt-1", "evt-1"))
  await notifier.notification(notification("envoy:\n  id: evt-2", "evt-2"))
  expect(lines).toEqual([
    { id: "evt-1", content: "envoy:\n  id: evt-1" },
    { id: "evt-2", content: "envoy:\n  id: evt-2" },
  ])
})

test("a delivery without an event id still gets an id of its own", async () => {
  const { socketPath, lines } = await inbox(({ id }) => ({ id, ok: true }))
  await createInboxNotifier(socketPath).notification(notification("plain"))
  expect(lines).toHaveLength(1)
  expect(lines[0]?.id).not.toBe("")
})

test("rejects a delivery the bridge refuses, naming its error", async () => {
  const { socketPath } = await inbox(({ id }) => ({ id, error: "the bridge is stopping" }))
  await expect(
    createInboxNotifier(socketPath).notification(notification("x", "evt-3")),
  ).rejects.toThrow("the Legion inbox refused delivery evt-3: the bridge is stopping")
})

test("rejects when no bridge listens at the socket", async () => {
  const dir = directory()
  await expect(
    createInboxNotifier(join(dir, "absent.sock")).notification(notification("x", "evt-4")),
  ).rejects.toThrow("the Legion inbox")
})
