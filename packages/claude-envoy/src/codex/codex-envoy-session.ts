import { join } from "node:path"
import { DELIVERY_CAPABILITIES } from "@legion/contracts"
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { sessionDirectory as dispatchSessionDirectory } from "@legion/envoy-client/dispatch-session-directory"
import { writeSessionTitle } from "@legion/envoy-client/dispatch-session-state"
import { messageFor } from "@legion/envoy-client/errors"
import {
  type SessionConfigFile,
  sessionDispatchSettings,
  sessionSettings,
} from "@legion/envoy-client/session-settings"
import { createEnvoyClient, newClientInstanceID } from "@legion/envoy-client/transport"
import { connect } from "nats"
/** The npm package a Codex session runs, which its registrations name. */
import { name as CODEX_PACKAGE_NAME } from "../../codex/package.json" with { type: "json" }
import {
  admittingMoves,
  channelReopener,
  closeUnstartedConnection,
  dialChannelInbound,
  sessionAdmission,
} from "../channel-connection"
import type { ChannelConnection } from "../channel-topics"
import {
  type ChannelSession,
  type ChannelToolRuntime,
  postDispatchReply,
  startChannelSession,
} from "../envoy-channel-server"
import { inboundOffChannelSession } from "../envoy-off"
import { channelConnectionName, installedPluginVersion } from "../installed-build"
import { SessionIdentity } from "../session-identity"
import { type SessionTitleEnvironment, sessionTitle } from "../session-title"
import { type CodexAgentStream, startCodexAgentStream } from "./codex-agent-stream"
import { askSideQuestion, type CodexCommand, codexProcessEnv, queueMessage } from "./codex-cli"
import { type CodexDelivery, createCodexDelivery } from "./codex-delivery"
import { codexInboxDirectory, discardWaiting } from "./codex-inbox"
import type { CodexSessionRecord } from "./codex-session-record"
import { followTranscript, type TranscriptFollower, transcriptEvents } from "./codex-transcript"
import { createCodexUserTurnRouter } from "./codex-user-turns"

/**
 * One Codex session's Envoy side: the Claude Code plugin's session code (`startChannelSession`)
 * with Codex's parts. A line goes into the session through `codex queue` or the `PostToolUse` hook
 * (`./codex-delivery`), a person's message through `./codex-user-turns`, and the live view comes
 * from the session's transcript (`./codex-agent-stream`).
 *
 * The server that owns a `codex` process of its own runs one, which a `/new` moves to the new
 * session's id through the hand-off file a Claude Code `/clear` writes (`followsNew`). The server
 * that owns Codex's shared app server runs one per recorded thread (`./codex-threads`), each with a
 * NATS connection and a client instance of its own, and none of them moves.
 */
export interface CodexEnvoySessionOptions {
  /** The plugin's state directory. */
  readonly stateDirectory: string
  /** The `codex` process the session runs in, whose environment `codex queue` runs in. */
  readonly codexPid: number
  /** The record the session starts from. */
  readonly bound: CodexSessionRecord
  /** Whether a `/new` moves this session to the next one through the process's hand-off file: a
   *  `codex` process of its own runs one session at a time, while each thread of the shared app
   *  server keeps its own id for as long as Codex keeps it loaded. */
  readonly followsNew: boolean
  /** What the title is worked out from (`sessionTitle`). */
  readonly titleEnvironment: SessionTitleEnvironment
  /** The session's envoy.json. */
  readonly configFile: SessionConfigFile
  /** The bundle this server runs, which names its build and its NATS connection. */
  readonly bundlePath: string
  readonly warn: (text: string) => void
}

export interface CodexEnvoySession {
  /**
   * Starts the session's Envoy side and resolves once it runs, once it finds Envoy off for the
   * session (`notReady`), or once a stop ended the start. Rejects when the start failed.
   */
  start(): Promise<void>
  /** What the Envoy tools act through, once the session runs. */
  runtime(): ChannelToolRuntime | undefined
  /** Why the Envoy tools never run for this session: Envoy is off for it. */
  notReady(): string | undefined
  /** The session the record names ended (`SessionEnd`), or runs again (`/resume`). */
  ended(ended: boolean): void
  /** The process moved to the session `next` names (`/new`), which has ended or not. */
  moveTo(next: CodexSessionRecord, ended: boolean): void
  /**
   * Ends any start still under way, stops the live view, the transcript follower and the keeper of
   * the connection's broker token, and withdraws the delivery's waiting lines, so Envoy sends each
   * of their frames again. Resolves once every waiting line is withdrawn or found taken.
   */
  withdraw(): Promise<void>
  /** Withdraws, waits for a start under way to end, and shuts the session down: its listener
   *  route is deregistered and its connection closed. Resolves once that is done. */
  shutdown(): Promise<void>
}

/** The capabilities a Codex session advertises: all three, since every one has a way in. */
const CODEX_CAPABILITIES = DELIVERY_CAPABILITIES

export function createCodexEnvoySession(options: CodexEnvoySessionOptions): CodexEnvoySession {
  const { bound, codexPid, configFile, stateDirectory, warn } = options
  const stopping = new AbortController()
  const isStopping = (): boolean => stopping.signal.aborted
  /** The record of the session the process runs now: `bound`, until a `/new` moves it. */
  let record = bound
  /** Whether that session has ended, as its marker last said. Before the delivery exists, its
   *  creation reads this instead. */
  let recordEnded = false
  /** Whether the session code, the live view and the transcript follower have started; a `/new`
   *  before then is applied once they have. */
  let ready = false
  let offReason: string | undefined
  let runtime: ChannelToolRuntime | undefined
  let session: ChannelSession | undefined
  let connection: ChannelConnection | undefined
  let stream: CodexAgentStream | undefined
  let follower: TranscriptFollower | undefined
  let delivery: CodexDelivery | undefined
  /** Stops the keeper of the connection's broker token, once one is dialled. */
  let stopNatsKeeper: () => void = () => undefined
  let starting: Promise<void> | undefined
  let withdrawn: Promise<void> | undefined
  let stopped: Promise<void> | undefined
  const id = new SessionIdentity(bound.session_id, bound.cwd)

  /** Reads the transcript of the session now bound into the delivery's turn state and the live
   *  view, from its first line; a `/new` starts this again on the new session's transcript. */
  const follow = (path: string | null): void => {
    follower?.stop()
    follower = undefined
    if (path === null || delivery === undefined) return
    const into = delivery
    follower = followTranscript(
      path,
      (line) => {
        for (const event of transcriptEvents(line)) {
          if (event.kind === "turn-started") into.turnStarted()
          else if (event.kind === "turn-ended") {
            void into.turnEnded().catch((error: unknown) => warn(messageFor(error)))
          }
          stream?.event(event)
        }
      },
      { warn },
    )
  }

  /** Moves the Codex parts to the session `next` names (`/new`): the delivery's turn state and
   *  waiting lines, the live view, and the transcript followed. The Envoy session code follows
   *  the hand-off file the hook wrote beside the record on its own. */
  const applyMove = (next: CodexSessionRecord): void => {
    void delivery?.rebind(recordEnded).catch((error: unknown) => warn(messageFor(error)))
    stream?.rebind(next.session_id)
    follow(next.transcript_path)
  }

  const run = async (): Promise<void> => {
    // Lines an earlier owner of this session left waiting: that server is gone, so no write waits
    // on them, and Envoy sends each one's frame again.
    await discardWaiting(codexInboxDirectory(stateDirectory, bound.session_id))
    const { dispatch, envoy, notice } = await sessionSettings(configFile, process.env, {
      cwd: bound.cwd,
      session: () => id.id,
    })
    if (notice !== null) warn(notice)
    const title = await sessionTitle(options.titleEnvironment, bound.cwd)
    const stampTitle = (sessionId: string, value: string): void => {
      writeSessionTitle(dispatchSessionDirectory(process.env, sessionId), value)
    }
    if (!envoy.enabled) {
      offReason = `Envoy is off for this session: ${envoy.error ?? envoy.notice}`
      if (dispatch.enabled) stampTitle(id.id, title(id.id))
      return
    }
    if (isStopping()) return

    const command = (): CodexCommand => ({
      codexHome: record.codex_home,
      // Codex's own environment, not this server's: Codex gave this server only `env_vars`.
      env: codexProcessEnv(codexPid, process.env),
      executable: record.codex_executable ?? "codex",
    })
    const client = createEnvoyClient({
      baseUrl: envoy.url,
      credential: envoy.credential,
      fetch: globalThis.fetch,
    })
    const clientInstanceID = newClientInstanceID()
    /** The session the `codex` process runs now, as its record names it: what `codex queue` and
     *  the inbox are addressed by, which moves at `/new` before the Envoy session code adopts the
     *  new id from the hand-off file. */
    const runningSession = (): string => record.session_id
    const codexDelivery = createCodexDelivery({
      inboxDirectory: (sessionId) => codexInboxDirectory(stateDirectory, sessionId),
      queue: (text) =>
        queueMessage(
          command(),
          runningSession(),
          text,
          join(stateDirectory, "codex-long-messages"),
        ),
      sessionId: runningSession,
      taken: (item) => stream?.delivered(item.content, item.dispatchMessageId),
      warn,
    })
    delivery = codexDelivery
    // An end the record's watcher saw before the delivery existed.
    if (recordEnded) codexDelivery.sessionEnded()

    const { inbound } = envoy
    if ("off" in inbound) {
      session = inboundOffChannelSession({ identity: id, stateDirectory, why: inbound.off })
    } else {
      // Dialled as the Claude Code session's connection is (`dialChannelInbound`): NATS with a
      // seed, no credential, or the session's broker token, which is then kept renewed and, at a
      // `/new`, moved to the new session's (`followSession`) before that session's subjects are
      // taken; or the listener's relay socket, opened again for the new session at a `/new`. A
      // connection that closes for good is replaced as the Claude Code one is (`channelReopener`):
      // Codex starts no plugin server again for a thread whose server lost its connection.
      const natsName = channelConnectionName(
        `codex-envoy-channel-${id.id}`,
        options.bundlePath,
        CODEX_PACKAGE_NAME,
      )
      // A stop asked for while the token or the dial is awaited ends the start, and is no failure:
      // the session ends as an asked-for stop does, as the Claude Code server's does.
      const dialled = await dialChannelInbound(
        inbound,
        envoy.url,
        () => id.id,
        natsName,
        warn,
        connect,
        stopping.signal,
      ).catch((error: unknown) => {
        if (isStopping()) return undefined
        throw error
      })
      if (dialled === undefined) return
      connection = dialled.connection
      stopNatsKeeper = dialled.stop
      // Which id the connection is admitted as, where its broker token admits one session at a
      // time: the dial's, then each `/new`'s once its move lands. The live view takes a new id's
      // control subject only then (`CodexAgentStreamOptions.admitted`).
      const admission = dialled.followSession === undefined ? undefined : sessionAdmission(id.id)
      const admitting = admittingMoves(admission, id)
      const followSession = admitting(dialled.followSession)
      const reopen = channelReopener({
        inbound,
        listenerUrl: envoy.url,
        session: () => id.id,
        name: natsName,
        signal: stopping.signal,
        swapKeeper: (stop) => {
          stopNatsKeeper()
          stopNatsKeeper = stop
        },
        admitting,
        useConnection: (next) => {
          connection = next
          stream?.useConnection(next)
        },
      })
      const started = await startChannelSession({
        capabilities: () => CODEX_CAPABILITIES,
        client,
        clientInstanceID,
        configFile,
        connection,
        ...(options.followsNew ? { handoffPid: codexPid } : {}),
        heartbeatMs: envoy.heartbeatMs,
        identity: id,
        ...(followSession === undefined ? {} : { followSession }),
        ...(dialled.reportOutagesTo === undefined
          ? {}
          : { reportOutagesTo: dialled.reportOutagesTo }),
        notifier: {
          notification: ({ content, eventId }) =>
            codexDelivery.deliver({ content, ...(eventId === undefined ? {} : { eventId }) }),
        },
        pluginVersion: installedPluginVersion(options.bundlePath) ?? "source",
        pluginName: CODEX_PACKAGE_NAME,
        router: (state) =>
          createCodexUserTurnRouter({
            ask: (question) => askSideQuestion(command(), id.id, question),
            deliver: (item) => codexDelivery.deliver(item),
            dispatch: async (ms) => {
              const active = activeDispatchSettings(
                (await sessionDispatchSettings(configFile, process.env, { cwd: bound.cwd }))
                  .dispatch,
              )
              return active === null
                ? undefined
                : new DispatchClient(active.url, active.credential, fetch, {
                    signal: AbortSignal.timeout(ms),
                  })
            },
            expect: (text, messageId) => stream?.expect(text, messageId),
            reply: (target, result) => postDispatchReply(id, configFile, target, result),
            sessionId: () => id.id,
            state,
            warn,
          }),
        reopen,
        // Ends a start still flushing through its connection's redials (`flushThroughRedials`):
        // the session is stopping.
        startSignal: stopping.signal,
        stateDirectory,
        title,
        ...(dispatch.enabled ? { stampTitle } : {}),
      }).catch((error: unknown) => {
        // A stop asked for while the session was still starting ends the start, and is no
        // failure, as above.
        if (isStopping()) return undefined
        throw error
      })
      if (started === undefined) return
      session = started
      stream = startCodexAgentStream({
        connection,
        liveView: dialled.liveView,
        sessionId: id.id,
        warn,
        ...(admission === undefined
          ? {}
          : { admitted: (next: string) => admission.admitted(next) }),
      })
      // A stop that landed while the session started ran before this view existed, and stopped
      // nothing of it: it goes now, before the shutdown closes the relay, so its control subject
      // goes first.
      if (isStopping()) {
        stream.stop()
        return
      }
    }
    runtime = { client, clientInstanceID, identity: id, session }
    if (isStopping()) return
    follow(bound.transcript_path)
    ready = true
    // A `/new` the record's watcher saw while the session was starting.
    if (record.session_id !== bound.session_id) applyMove(record)
  }

  const withdraw = (): Promise<void> => {
    stopping.abort()
    withdrawn ??= (async () => {
      follower?.stop()
      stream?.stop()
      stopNatsKeeper()
      await delivery?.stop()
    })()
    return withdrawn
  }

  return {
    start() {
      if (isStopping()) return Promise.resolve()
      starting ??= run()
      return starting
    },
    runtime: () => runtime,
    notReady: () => offReason,
    ended(ended) {
      if (ended === recordEnded) return
      recordEnded = ended
      if (ended) delivery?.sessionEnded()
      else void delivery?.sessionResumed().catch((error: unknown) => warn(messageFor(error)))
    },
    moveTo(next, ended) {
      record = next
      recordEnded = ended
      if (ready) applyMove(next)
    },
    withdraw,
    shutdown() {
      stopped ??= (async () => {
        const withdrawing = withdraw()
        // The start ends at once on the stop, or finishes a registration already on the wire,
        // whose session is then shut down here.
        await starting?.catch(() => undefined)
        // What the start set up after the withdraw: the token's keeper and the follower.
        stopNatsKeeper()
        follower?.stop()
        if (session !== undefined) await session.shutdown()
        else if (connection !== undefined) await closeUnstartedConnection(connection)
        await withdrawing
      })()
      return stopped
    },
  }
}
