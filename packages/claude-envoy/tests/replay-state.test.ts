// The state file's writer keeps each of its three parts through a write of another: a ledger write
// keeps the consumer snapshot and the told outage, and the other two keep the ledger.
import { expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { readReplayState, replayStateFile, replayStateWriter } from "../src/replay-state"

async function scratchFile(): Promise<{
  readonly file: string
  readonly done: () => Promise<void>
}> {
  const directory = await mkdtemp(join(tmpdir(), "replay-state-"))
  return {
    file: replayStateFile(directory, "ses_replay_state"),
    done: () => rm(directory, { recursive: true, force: true }),
  }
}

test("a told outage survives a ledger write and a consumer write, and its end replaces it", async () => {
  const { file, done } = await scratchFile()
  try {
    const writer = replayStateWriter(file, { ledger: [] })
    await writer.inboundOutage(true)
    expect((await readReplayState(file)).inboundOutageTold).toBe(true)
    await writer.ledger({ ledger: [], state: { topics: ["team.alpha"] } })
    await writer.consumer({
      resume: { stream_id: "stream-1", seq: 7 },
      follows: { "team.alpha": 1 },
    })
    const kept = await readReplayState(file)
    expect(kept.inboundOutageTold).toBe(true)
    expect(kept.state?.topics).toEqual(["team.alpha"])
    expect(kept.consumer?.resume).toEqual({ stream_id: "stream-1", seq: 7 })
    await writer.inboundOutage(false)
    await writer.ledger({ ledger: [], state: { topics: ["team.beta"] } })
    const ended = await readReplayState(file)
    expect(ended.inboundOutageTold).toBe(false)
    expect(ended.state?.topics).toEqual(["team.beta"])
    expect(ended.consumer?.resume).toEqual({ stream_id: "stream-1", seq: 7 })
  } finally {
    await done()
  }
})

test("a writer restored from a file that records a told outage keeps it through a ledger write", async () => {
  const { file, done } = await scratchFile()
  try {
    const writer = replayStateWriter(file, { ledger: [], inboundOutageTold: true })
    await writer.ledger({ ledger: [] })
    expect((await readReplayState(file)).inboundOutageTold).toBe(true)
  } finally {
    await done()
  }
})
