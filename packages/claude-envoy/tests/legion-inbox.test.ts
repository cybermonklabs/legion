import { afterEach, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { createServer, type Server, type Socket } from "node:net"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { createInboxNotifier } from "../src/legion-inbox"

const scratch: string[] = []
const servers: Server[] = []
afterEach(async () => {
  for (const server of servers.splice(0)) server.close()
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true })
})

/** A line the stand-in read: a delivery's `{id, content}`, or a task line's `{id, kind, ...}`. */
type InboxLine = { readonly id: string } & Record<string, unknown>

/** A stand-in for the bridge's inbox (`claudebridge/inbox.go`): it records each line and answers
 * it with `answer`. */
async function inbox(answer: (line: InboxLine) => object): Promise<{
  readonly socketPath: string
  readonly lines: InboxLine[]
}> {
  const dir = await mkdtemp(join(tmpdir(), "legion-inbox-"))
  scratch.push(dir)
  const socketPath = join(dir, "inbox.sock")
  const lines: InboxLine[] = []
  const server = createServer((socket: Socket) => {
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = JSON.parse(buffered.slice(0, end)) as InboxLine
        buffered = buffered.slice(end + 1)
        lines.push(line)
        socket.write(`${JSON.stringify(answer(line))}\n`)
      }
    })
  })
  servers.push(server)
  const listening = Promise.withResolvers<void>()
  server.listen(socketPath, listening.resolve)
  await listening.promise
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
  const dir = await mkdtemp(join(tmpdir(), "legion-inbox-"))
  scratch.push(dir)
  await expect(
    createInboxNotifier(join(dir, "absent.sock")).notification(notification("x", "evt-4")),
  ).rejects.toThrow("the Legion inbox")
})

const EPOCH = "2026-11-13T16:21:44.123456789Z"

test("writes the claim's task value as one task line with an id of its own and the finished number it came with, a deleted value as task 0, and resolves on the bridge's ok", async () => {
  const { socketPath, lines } = await inbox(({ id }) => ({ id, ok: true }))
  const notifier = createInboxNotifier(socketPath)
  await notifier.notification(notification("envoy:\n  id: evt-1", "evt-1"))
  await notifier.task({ number: 4, epoch: EPOCH, revision: 12, finished: 3 })
  await notifier.task({ number: 0, epoch: EPOCH, revision: 13, finished: 0 })
  expect(lines).toEqual([
    { id: "evt-1", content: "envoy:\n  id: evt-1" },
    { id: expect.any(String), kind: "task", number: 4, epoch: EPOCH, revision: 12, finished: 3 },
    { id: expect.any(String), kind: "task", number: 0, epoch: EPOCH, revision: 13, finished: 0 },
  ])
  expect(new Set(lines.map(({ id }) => id)).size).toBe(3)
})

test("writes the ready line with an id of its own, behind the lines written before it, and resolves on the bridge's ok", async () => {
  const { socketPath, lines } = await inbox(({ id }) => ({ id, ok: true }))
  const notifier = createInboxNotifier(socketPath)
  const task = notifier.task({ number: 4, epoch: EPOCH, revision: 12, finished: 3 })
  const ready = notifier.ready()
  await Promise.all([task, ready])
  expect(lines).toEqual([
    { id: expect.any(String), kind: "task", number: 4, epoch: EPOCH, revision: 12, finished: 3 },
    { id: expect.any(String), kind: "ready" },
  ])
  expect(new Set(lines.map(({ id }) => id)).size).toBe(2)
})

test("writes a reading's finished number, where the reading holds no value, as one finished line with an id of its own, and resolves on the bridge's ok", async () => {
  const { socketPath, lines } = await inbox(({ id }) => ({ id, ok: true }))
  const notifier = createInboxNotifier(socketPath)
  await notifier.finished(8)
  await notifier.finished(0)
  expect(lines).toEqual([
    { id: expect.any(String), kind: "finished", finished: 8 },
    { id: expect.any(String), kind: "finished", finished: 0 },
  ])
  expect(new Set(lines.map(({ id }) => id)).size).toBe(2)
})

test("a task line the bridge refuses rejects naming its error, and the delivery behind it keeps its own answer", async () => {
  // A bridge that knows no task line answers it as the malformed delivery it reads it as.
  const { socketPath } = await inbox((line) =>
    line["kind"] === "task"
      ? { id: line.id, error: "an inbox delivery is one JSON object with an id and content" }
      : { id: line.id, ok: true },
  )
  const notifier = createInboxNotifier(socketPath)
  const task = notifier.task({ number: 4, epoch: EPOCH, revision: 12, finished: 3 })
  const delivery = notifier.notification(notification("x", "evt-5"))
  await expect(task).rejects.toThrow(
    /^the Legion inbox refused task line [0-9a-f-]{36}: an inbox delivery is one JSON object with an id and content$/,
  )
  await expect(delivery).resolves.toBeUndefined()
})
