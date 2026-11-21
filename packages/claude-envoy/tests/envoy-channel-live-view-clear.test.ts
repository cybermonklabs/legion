// The Agents page's live view follows a `/clear` the mod reported before the view began listening.
// The view starts once the mod's link has bound and the session has started, so a `/clear` early in
// a session can be reported by the mod (a `session-id` event) before the view listens for one, and
// the link reports an id once (`ModLink.onSessionId` replays nothing). The view started on the id
// the server had then, and nothing moved it: it served the old id, and under a broker token, whose
// connection is admitted as one id at a time, it took no subject at all once the old id's was
// refused. The server's own adoption of the `/clear` (`adoptHandoff`) moves it. Whole processes:
// the mod is `./fake-mod`, and the registration is held until it has reported the `/clear`.
import { expect, test } from "bun:test"
import { agentStreamControlSubject } from "@legion/contracts"
import { modSocketPath } from "../hooks/mod-protocol"
import { sessionHandoffFile, writeSessionHandoff } from "../src/session-identity"
import { waitFor } from "./channel-session-harness"
import { startFakeMod } from "./fake-mod"
import { type SessionBroker, sessionBroker, startReopenServer } from "./reopen-server"

const BEFORE = "ses_view_before_clear"
const AFTER = "ses_view_after_clear"

/**
 * A Claude Code channel server for session `BEFORE` whose mod reports a `/clear` to `AFTER` while
 * the session's first registration is held, so before the live view starts: the view starts on
 * `BEFORE`, and the server adopts `AFTER` only once the hook's handoff file names it.
 */
async function startWithEarlyClear(broker?: SessionBroker) {
  const held = Promise.withResolvers<void>()
  const run = await startReopenServer({
    sessionId: BEFORE,
    ...(broker === undefined ? {} : { nonce: "fake-nonce" }),
    env: (stateDirectory) => ({
      CLAUDE_CODE_SESSION_ID: BEFORE,
      CLAUDE_PLUGIN_DATA: stateDirectory,
      ...broker?.env,
    }),
    awaitRegistration: false,
    onRequest: (method, path) =>
      method === "POST" && path === "/v1/interests/subscribe" ? held.promise : undefined,
  })
  try {
    const socketPath = modSocketPath(BEFORE, { XDG_RUNTIME_DIR: run.stateDirectory })
    if (socketPath === undefined) throw new Error("the session names no mod socket")
    const mod = await startFakeMod(socketPath, { sessionId: BEFORE })
    // The link has the `/clear` before the session starts, so before the live view listens.
    await mod.send({ kind: "session-id", sessionId: AFTER })
    held.resolve()
    await waitFor(
      async () => run.broker.subscribed.includes(agentStreamControlSubject(BEFORE)),
      "the live view started on the id the server had",
      run.state,
    )
    return { run, mod }
  } catch (error) {
    held.resolve()
    await run.stop()
    throw error
  }
}

test("a /clear the mod reported before the live view listened moves the view once the server adopts it", async () => {
  const { run, mod } = await startWithEarlyClear()
  try {
    await writeSessionHandoff(sessionHandoffFile(run.stateDirectory, process.pid), AFTER)
    await waitFor(
      async () => run.sessions.includes(AFTER),
      "the server to adopt the /clear and register the new id",
      run.state,
    )
    await waitFor(
      async () => run.broker.subscribed.includes(agentStreamControlSubject(AFTER)),
      "the live view's control subject under the new id",
      () => `${run.state()}\nsubscribed: ${JSON.stringify(run.broker.subscribed)}`,
    )
    // It left the old id's subject, and took the new one's once.
    expect(run.broker.unsubscribed).toContain(agentStreamControlSubject(BEFORE))
    expect(
      run.broker.subscribed.filter((subject) => subject === agentStreamControlSubject(AFTER)),
    ).toHaveLength(1)
  } finally {
    await mod.stop()
    await run.stop()
  }
})

test("under the session's broker token, that view takes the new id's control subject once the connection presents the new id's token", async () => {
  const tokens = await sessionBroker()
  const started = await startWithEarlyClear(tokens).catch(async (error: unknown) => {
    await tokens.stop()
    throw error
  })
  const { run, mod } = started
  const control = agentStreamControlSubject(AFTER)
  try {
    const tradedAt = tokens.minted.length
    await writeSessionHandoff(sessionHandoffFile(run.stateDirectory, process.pid), AFTER)
    await waitFor(
      async () => run.broker.subscribed.includes(control),
      "the live view's control subject under the new id",
      () => `${run.state()}\nsubscribed: ${JSON.stringify(run.broker.subscribed)}`,
    )
    // Taken once, on a connection whose CONNECT presented a token traded for the new id: not on
    // the old id's credential, which the auth service would refuse it.
    const taken = run.broker.subscribed.flatMap((subject, index) =>
      subject === control ? [index] : [],
    )
    expect(taken).toHaveLength(1)
    const presented = run.broker.connects[(run.broker.subscribedAfter[taken[0] ?? 0] ?? 0) - 1]
    await run.checkConnects(
      tokens.minted.slice(tradedAt).includes(String(presented?.["auth_token"])),
      "the new id's control subject was taken on a connection presenting no token traded for it",
    )
  } finally {
    await mod.stop()
    await run.stop()
    await tokens.stop()
  }
})
