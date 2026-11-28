import { expect, spyOn, test } from "bun:test"
import { gatedStderr } from "../src/stop-gates"

test("the tell gate writes a stderr line of the server's own before the stop has begun, and none once it has", () => {
  let stopping = false
  const serverStderr = gatedStderr(() => stopping)
  const written: string[] = []
  const stderr = spyOn(process.stderr, "write").mockImplementation((chunk) => {
    written.push(String(chunk))
    return true
  })
  try {
    serverStderr("envoy-channel: before the stop\n")
    stopping = true
    serverStderr("envoy-channel: registry heartbeat failed (x); retrying at the next heartbeat\n")

    expect(written).toEqual(["envoy-channel: before the stop\n"])
  } finally {
    stderr.mockRestore()
  }
})
