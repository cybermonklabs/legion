// The Codex plugin's server runs the same session code as the Claude Code one
// (`startChannelSession`), so a Codex session whose NATS connection closes for good is given a new
// one too: Codex starts no plugin server again for a thread whose server lost its connection. The
// server is a whole process, started with `--host codex` as Codex starts it, beneath this test's
// process, which stands in for the `codex` process: the session record names this process and
// a stand-in `codex` binary that writes each `codex queue` it is asked for to a file, which is how
// every line reaches an idle Codex session (`../src/codex/codex-delivery`).
import { expect, test } from "bun:test"
import { chmod, readFile, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { agentStreamControlSubject, legacyAgentSubject } from "@legion/contracts"
import {
  codexSessionRecordFile,
  processStarted,
  writeCodexSessionRecord,
} from "../src/codex/codex-session-record"
import { waitFor } from "./channel-session-harness"
import { count, PROCESS_WAIT_MS, ROW_TIMEOUT_MS, startReopenServer } from "./reopen-server"

/** A Codex thread id, in the shape Codex gives one. */
const SESSION = "019a0f3c-5b1e-7d2a-9c4f-0b1d2e3f4a5b"
const CONTROL = agentStreamControlSubject(SESSION)
const DIRECT = legacyAgentSubject(SESSION)
/** What every `codex queue` call wrote, one call after another. */
const QUEUED = "queued.log"

/**
 * The record the plugin's `SessionStart` hook writes for this test's process, and the stand-in
 * `codex` binary it names, which appends each message `codex queue` is handed to `QUEUED`.
 */
async function codexSession(stateDirectory: string): Promise<void> {
  const started = processStarted(process.pid)
  if (started === undefined) throw new Error("this process's start cannot be read")
  const codex = join(stateDirectory, "codex")
  await writeFile(
    codex,
    `#!/bin/sh\nfor arg in "$@"; do printf '%s\\n' "$arg"; done >> '${join(stateDirectory, QUEUED)}'\n`,
  )
  await chmod(codex, 0o755)
  await writeCodexSessionRecord(codexSessionRecordFile(stateDirectory, process.pid), {
    codex_executable: codex,
    codex_home: join(stateDirectory, "codex-home"),
    codex_started: started,
    cwd: stateDirectory,
    session_id: SESSION,
    source: "startup",
    transcript_path: null,
  })
}

test(
  "a Codex session's NATS connection closed for good is dialed again: the relay reopens on a new connection at a new epoch, and the end of the outage is told once",
  async () => {
    const run = await startReopenServer({
      sessionId: SESSION,
      args: ["--host", "codex"],
      env: (stateDirectory) => ({ PLUGIN_DATA: stateDirectory }),
      prepare: codexSession,
    })
    const { broker, gate, stderr, state } = run
    const queued = (): Promise<string> =>
      readFile(join(run.stateDirectory, QUEUED), "utf8").catch(() => "")
    const controlSubscriptions = (): number =>
      broker.subscribed.filter((subject) => subject === CONTROL).length
    try {
      // The live view takes its control subject as the session starts.
      await waitFor(
        async () => controlSubscriptions() === 1,
        "the live view's control subject",
        state,
        PROCESS_WAIT_MS,
      )
      const firstEpoch = run.deliveries.at(-1)?.epoch
      await run.closeForGood()

      // The session is told once that it receives nothing, naming why, as it is told everything:
      // through `codex queue`.
      await waitFor(
        async () =>
          count(await queued(), "Envoy inbound unavailable") === 1 &&
          stderr().includes("envoy-channel: this session receives nothing ("),
        "the outage told in the session and on stderr",
        state,
        PROCESS_WAIT_MS,
      )
      expect(await queued()).toContain(
        "because its NATS connection failed: Authorization Violation",
      )

      // The hub admits again: the relay subject is taken on a new connection and a new epoch is
      // registered, so the listener restarts the relay at its ack floor.
      const refusedUntil = run.deliveries.length
      gate.open()
      await waitFor(
        async () =>
          run.relaySubscriptions() === 2 &&
          broker.liveConnections === 1 &&
          run.deliveries.slice(refusedUntil).some((asked) => asked.epoch !== firstEpoch),
        "the relay subscription on a new connection and a registration of a new epoch",
        state,
        PROCESS_WAIT_MS,
      )
      await waitFor(
        async () => controlSubscriptions() === 2,
        "the live view's control subject on the new connection",
        state,
        PROCESS_WAIT_MS,
      )

      broker.deliverRelay(SESSION, {
        subject: DIRECT,
        seq: 3,
        payload: JSON.stringify({
          event_id: "evt-codex-after-reopen",
          source: "agent",
          source_session: "ses_peer",
          topic: DIRECT,
          dedupe_key: "delivery-codex-after-reopen",
          issued_at: 1_760_000_000_000,
          payload_summary: "codex peer message after the reopen",
        }),
        reply: "_INBOX.codex-after-reopen",
      })
      const answers = (): string[] =>
        broker.published
          .filter((frame) => frame.subject === "_INBOX.codex-after-reopen")
          .map((frame) => frame.payload)
      await waitFor(
        async () => answers().includes("done"),
        "the frame relayed on the new connection to be queued and answered done",
        state,
        PROCESS_WAIT_MS,
      )
      expect(answers()).toEqual(["received", "done"])
      expect(count(await queued(), "codex peer message after the reopen")).toBe(1)

      // The accepted registration ends the outage, told once in the session and on stderr.
      await waitFor(
        async () =>
          count(await queued(), "Envoy inbound restored") === 1 &&
          stderr().includes("envoy-channel: this session receives again"),
        "the end of the outage told in the session and on stderr",
        state,
        PROCESS_WAIT_MS,
      )
      expect(count(stderr(), "envoy-channel: this session receives again")).toBe(1)
      expect(count(await queued(), "Envoy inbound unavailable")).toBe(1)
      expect(stderr()).toContain(
        "envoy-channel: the NATS connection closed for good (Authorization Violation); dialing a new one",
      )
    } finally {
      await run.stop()
    }
  },
  ROW_TIMEOUT_MS,
)
