import { describe, expect, test } from "bun:test"
import { mkdirSync } from "node:fs"
import { join } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { createCodexDelivery } from "../src/codex/codex-delivery"
import {
  type CodexInboxItem,
  codexInboxDirectory,
  inboxContext,
  putInbox,
  takeInbox,
  takenBy,
} from "../src/codex/codex-inbox"

const root = temporaryDirectory("codex-delivery")
let count = 0
/** A fresh inbox directory for one test. */
function inbox(): string {
  const directory = codexInboxDirectory(join(root, String(count++)), "session/1")
  mkdirSync(directory, { recursive: true })
  return directory
}

describe("the Codex inbox", () => {
  test("a message is taken once, by whichever taker renames it first, oldest first", async () => {
    const directory = inbox()
    const first = await putInbox(directory, { content: "one" })
    const second = await putInbox(directory, { content: "two", eventId: "ev-2" })
    expect(await takenBy(directory, first)).toBeUndefined()
    const hook = await takeInbox(directory, "hook")
    expect(hook.map(({ item }) => item)).toEqual([
      { content: "one" },
      { content: "two", eventId: "ev-2" },
    ])
    expect(await takeInbox(directory, "server")).toEqual([])
    expect(await takenBy(directory, first)).toBe("hook")
    expect(await takenBy(directory, second)).toBe("hook")
  })

  test("the session id is one path segment, whatever it holds", () => {
    expect(codexInboxDirectory("/state", "a/../b")).toBe("/state/codex-inbox/a%2F..%2Fb")
  })

  test("an empty or missing inbox yields nothing", async () => {
    expect(await takeInbox(join(root, "missing"), "hook")).toEqual([])
  })

  test("the hook's context quotes every message whole, in order", () => {
    expect(inboxContext([{ content: "one" }, { content: "two\nlines" }])).toBe(
      "Messages delivered to this session while it worked (Envoy, through the Envoy plugin):\n\none\n\ntwo\nlines",
    )
  })
})

describe("createCodexDelivery", () => {
  function harness(directory: string, failQueue = false) {
    const queued: string[] = []
    const taken: CodexInboxItem[] = []
    const delivery = createCodexDelivery({
      inboxDirectory: () => directory,
      pollMs: 10,
      queue: async (text) => {
        if (failQueue) throw new Error("codex queue exited 1")
        queued.push(text)
      },
      taken: (item) => taken.push(item),
      warn: () => undefined,
    })
    return { delivery, queued, taken }
  }

  test("an idle session takes a line through codex queue at once", async () => {
    const { delivery, queued } = harness(inbox())
    await delivery.deliver({ content: "hello" })
    expect(queued).toEqual(["hello"])
    delivery.stop()
  })

  test("during a turn the line waits until the hook takes it, and is never queued as well", async () => {
    const directory = inbox()
    const { delivery, queued, taken } = harness(directory)
    delivery.turnStarted()
    let done = false
    const written = delivery.deliver({ content: "mid-turn", dispatchMessageId: "m1" }).then(() => {
      done = true
    })
    await Bun.sleep(30)
    expect(done).toBe(false)
    expect((await takeInbox(directory, "hook")).map(({ item }) => item.content)).toEqual([
      "mid-turn",
    ])
    await written
    expect(taken).toEqual([{ content: "mid-turn", dispatchMessageId: "m1" }])
    await delivery.turnEnded()
    expect(queued).toEqual([])
    delivery.stop()
  })

  test("a line no tool call took goes to codex queue when the turn ends", async () => {
    const { delivery, queued, taken } = harness(inbox())
    delivery.turnStarted()
    const written = delivery.deliver({ content: "late" })
    await Bun.sleep(20)
    expect(queued).toEqual([])
    await delivery.turnEnded()
    await written
    expect(queued).toEqual(["late"])
    expect(taken).toEqual([])
    delivery.stop()
  })

  test("a codex queue failure rejects the write, so the frame is sent again", async () => {
    const idle = harness(inbox(), true)
    await expect(idle.delivery.deliver({ content: "x" })).rejects.toThrow("codex queue exited 1")
    idle.delivery.stop()
    const busy = harness(inbox(), true)
    busy.delivery.turnStarted()
    const written = busy.delivery.deliver({ content: "y" })
    await Bun.sleep(20)
    await busy.delivery.turnEnded()
    await expect(written).rejects.toThrow("codex queue exited 1")
    busy.delivery.stop()
  })

  test("stopping rejects a line still waiting", async () => {
    const { delivery } = harness(inbox())
    delivery.turnStarted()
    const written = delivery.deliver({ content: "never" })
    await Bun.sleep(20)
    delivery.stop()
    await expect(written).rejects.toThrow("stopped before the message was taken")
  })
})
