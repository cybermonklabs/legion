// The channel server's way out (`../src/way-out`): each step waits for its fact unless the server
// bounds it itself, and the bounds it sets itself leave its exit bound room on the longest path.
// The server process's own way out, against a real listener, is envoy-channel-stall.test.ts's.
import { afterEach, expect, jest, test } from "bun:test"
import type { ChannelRecordLine } from "../src/channel-record"
import {
  CLIENT_KILL_MS,
  EXIT_FLUSH_MS,
  SHUTDOWN_BOUNDS,
  SHUTDOWN_BOUNDS_WORST_MS,
  SHUTDOWN_EXIT_MS,
  wayOut,
} from "../src/way-out"

afterEach(() => {
  jest.useRealTimers()
})

test("the server's own bounds, on the way out's longest path, leave the exit bound room", () => {
  // Every other step waits for its fact, so this is what the bounds take before those steps run:
  // a session's drain and settle, or a failed start's drain. Raise a bound past what the exit
  // bound leaves (pi-envoy's own settle, which the session's settle shares, say) and this fails.
  expect(SHUTDOWN_BOUNDS_WORST_MS).toBe(
    Math.max(SHUTDOWN_BOUNDS.drain + SHUTDOWN_BOUNDS.settle, SHUTDOWN_BOUNDS["start-drain"]),
  )
  expect(SHUTDOWN_BOUNDS_WORST_MS).toBeLessThan(SHUTDOWN_EXIT_MS)
  // The exit on the bound flushes stderr inside the MCP client's time from stdin's end to SIGKILL.
  expect(SHUTDOWN_EXIT_MS + EXIT_FLUSH_MS).toBeLessThan(CLIENT_KILL_MS)
})

test("a step the server does not bound waits for its fact however long it takes, and is named while it runs", async () => {
  jest.useFakeTimers()
  const lines: ChannelRecordLine[] = []
  const way = wayOut((line) => lines.push(line))
  const write = Promise.withResolvers<void>()
  let ended: "fact" | "bound" | undefined
  const step = way.step("state", write.promise).then((by) => {
    ended = by
  })
  // Far past any bound this branch ever gave a last write (100 ms) or the DELETE (750 ms more).
  jest.advanceTimersByTime(60_000)
  await Promise.resolve()
  expect(ended).toBeUndefined()
  expect(way.running()).toEqual(["state"])
  write.resolve()
  await step
  expect(ended).toBe("fact")
  expect(way.running()).toEqual([])
  expect(lines).toEqual([{ wait: "state", endedBy: "fact" }])
})

test("a step the server bounds itself goes on at its bound, recording the delay it armed", async () => {
  jest.useFakeTimers()
  const lines: ChannelRecordLine[] = []
  const way = wayOut((line) => lines.push(line))
  const drain = way.step("drain", new Promise(() => {}), SHUTDOWN_BOUNDS.drain)
  jest.advanceTimersByTime(SHUTDOWN_BOUNDS.drain)
  expect(await drain).toBe("bound")
  expect(way.running()).toEqual([])
  expect(lines).toEqual([{ wait: "drain", armedMs: SHUTDOWN_BOUNDS.drain, endedBy: "bound" }])
})

test("a step whose work fails ended on its fact: what it waited for came", async () => {
  const lines: ChannelRecordLine[] = []
  const way = wayOut((line) => lines.push(line))
  expect(await way.step("deregister", Promise.reject(new Error("the listener refused")))).toBe(
    "fact",
  )
  expect(lines).toEqual([{ wait: "deregister", endedBy: "fact" }])
})
