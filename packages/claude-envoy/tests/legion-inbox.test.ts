import { afterEach, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { createServer, type Server, type Socket } from "node:net"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { createInboxNotifier, TAKE_PENDING_TIMEOUT_MS } from "../src/legion-inbox"
import { waitFor } from "./channel-session-harness"

const scratch: string[] = []
const servers: Server[] = []
afterEach(async () => {
  for (const server of servers.splice(0)) server.close()
  for (const dir of scratch.splice(0)) await rm(dir, { recursive: true, force: true })
})

/** A line the stand-in read: a delivery's `{id, content}`, or a task line's `{id, kind, ...}`. */
type InboxLine = { readonly id: string } & Record<string, unknown>

/** A stand-in for the bridge's inbox (`claudebridge/inbox.go`): it records each line, and the
 * connection it came on, and answers it with `answer`: at once, once the promise `answer` gives
 * settles, or never where `answer` gives undefined. */
async function inbox(answer: (line: InboxLine) => object | Promise<object> | undefined): Promise<{
  readonly socketPath: string
  readonly lines: InboxLine[]
  /** For each line of `lines`, the connection it came on, counted from 1 in the order they
   * opened. */
  readonly connections: number[]
}> {
  const dir = await mkdtemp(join(tmpdir(), "legion-inbox-"))
  scratch.push(dir)
  const socketPath = join(dir, "inbox.sock")
  const lines: InboxLine[] = []
  const connections: number[] = []
  let opened = 0
  const server = createServer((socket: Socket) => {
    opened += 1
    const connection = opened
    let buffered = ""
    socket.setEncoding("utf8")
    socket.on("data", (chunk: string) => {
      buffered += chunk
      for (let end = buffered.indexOf("\n"); end >= 0; end = buffered.indexOf("\n")) {
        const line = JSON.parse(buffered.slice(0, end)) as InboxLine
        buffered = buffered.slice(end + 1)
        lines.push(line)
        connections.push(connection)
        const reply = answer(line)
        if (reply instanceof Promise) {
          void reply.then((settled) => socket.write(`${JSON.stringify(settled)}\n`))
        } else if (reply !== undefined) {
          socket.write(`${JSON.stringify(reply)}\n`)
        }
      }
    })
    // A client that hangs up on a line it no longer waits for is no fault of the stand-in's.
    socket.on("error", () => undefined)
  })
  servers.push(server)
  const listening = Promise.withResolvers<void>()
  server.listen(socketPath, listening.resolve)
  await listening.promise
  return { socketPath, lines, connections }
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

// R6 and R1 of dispatch://INFRA-4130 on Claude Code: the phase hook asks the bridge whether the
// claim's take holds a task, and which task the turn running did not take. The bridge answers one
// connection's lines in order, and a delivery's answer waits for Claude Code to take it, so a
// query behind a delivery on that connection would wait as long as the delivery does: the query
// goes on a connection of its own, which the bridge answers at once.
test("the take-pending query goes on a connection of its own and is answered while a delivery waits", async () => {
  const taken = Promise.withResolvers<void>()
  const { socketPath, lines, connections } = await inbox((line) =>
    line["kind"] === "take-pending"
      ? { id: line.id, ok: true, takePending: { pending: true, waiting: 4 } }
      : // A delivery is answered once Claude Code has taken it, which this one waits for.
        taken.promise.then(() => ({ id: line.id, ok: true })),
  )
  const notifier = createInboxNotifier(socketPath)
  let delivered = false
  const delivery = notifier.notification(notification("envoy:\n  id: evt-1", "evt-1")).then(() => {
    delivered = true
  })
  await waitFor(async () => lines.length === 1, "the delivery to reach the bridge")

  expect(await notifier.takePending()).toEqual({ pending: true, waiting: 4 })
  expect(delivered).toBe(false)
  // Each query dials a connection of its own, and neither is the delivery's.
  expect(await notifier.takePending()).toEqual({ pending: true, waiting: 4 })
  expect(lines).toEqual([
    { id: "evt-1", content: "envoy:\n  id: evt-1" },
    { id: expect.any(String), kind: "take-pending" },
    { id: expect.any(String), kind: "take-pending" },
  ])
  expect(new Set(connections).size).toBe(3)
  expect(new Set(lines.map(({ id }) => id)).size).toBe(3)

  taken.resolve()
  await delivery
  expect(delivered).toBe(true)
})

// An older bridge refuses the kind (`inboxEvent`'s default: "an inbox line of kind … is none the
// bridge takes"), and the hook reads every answer that does not say whether a take is pending as
// no answer at all: the Stop then blocks as before, and no note is written.
test("a take-pending query the bridge refuses, as an older bridge does, or answers with no takePending, answers undefined", async () => {
  const refusing = await inbox(({ id }) => ({
    id,
    ok: false,
    error: 'an inbox line of kind "take-pending" is none the bridge takes',
  }))
  expect(await createInboxNotifier(refusing.socketPath).takePending()).toBeUndefined()

  const plain = await inbox(({ id }) => ({ id, ok: true }))
  expect(await createInboxNotifier(plain.socketPath).takePending()).toBeUndefined()

  const otherID = await inbox(() => ({
    id: "another-line",
    ok: true,
    takePending: { pending: true, waiting: 4 },
  }))
  expect(await createInboxNotifier(otherID.socketPath).takePending()).toBeUndefined()

  const malformed = await inbox(({ id }) => ({ id, ok: true, takePending: { pending: "yes" } }))
  expect(await createInboxNotifier(malformed.socketPath).takePending()).toBeUndefined()
})

test("a take-pending query the bridge does not answer within its bound answers undefined", async () => {
  expect(TAKE_PENDING_TIMEOUT_MS).toBe(2_000)
  const { socketPath, lines } = await inbox(() => undefined)
  const started = performance.now()
  expect(
    await createInboxNotifier(socketPath, { takePendingTimeoutMs: 50 }).takePending(),
  ).toBeUndefined()
  expect(performance.now() - started).toBeLessThan(1_000)
  expect(lines).toEqual([{ id: expect.any(String), kind: "take-pending" }])
})

test("a take-pending query answers undefined when no bridge listens at the socket", async () => {
  const dir = await mkdtemp(join(tmpdir(), "legion-inbox-"))
  scratch.push(dir)
  expect(await createInboxNotifier(join(dir, "absent.sock")).takePending()).toBeUndefined()
})
