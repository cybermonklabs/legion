// The inbound connection a channel server holds for its session, over the session's life: NATS,
// dialed with the session's credential and redialed until it opens (`dialChannelNats`), or the
// listener's relay socket where the settings say `ENVOY_INBOUND=listener` (`dialChannelInbound`);
// admitted as one session id at a time where it presents the session's broker token
// (`sessionAdmission`, `admittingMoves`), replaced once it closes for good (`channelReopener`),
// and closed with a bound where no session took it (`closeUnstartedConnection`). Both channel
// servers take theirs from here: Claude Code's (`./envoy-channel-server`) and Codex's
// (`./codex/codex-server`).
import type { RequestCredential } from "@legion/envoy-client/bearer"
import { inboundUnavailable } from "@legion/envoy-client/consumer-delivery"
import { LoggableError } from "@legion/envoy-client/errors"
import { connectListenerRelay } from "@legion/envoy-client/listener-relay"
import {
  connectNats,
  NATS_DIAL_PLAN,
  type NatsDialPlan,
  natsConnectionOptions,
  natsDialFailure,
  natsServersForLog,
} from "@legion/envoy-client/nats-connection"
import {
  followNatsCredential,
  keepNatsCredential,
  type NatsCredential,
} from "@legion/envoy-client/nats-credential"
import { type ConnectionOptions, connect, type NatsConnection } from "nats"
import type { ChannelConnection } from "./channel-topics"
import type { SessionIdentity } from "./session-identity"

/** Why a dial or a start a stop ended did not come up: the process is stopping (`dialChannelNats`,
 *  and the start's own flush, `startChannelSession`). */
export const START_STOPPING =
  "the channel server stopped before its NATS subscriptions reached the server"

/** Which session id a connection that presents a broker token is admitted as: one at a time, the
 *  dial's first, then each `/clear`'s once its move lands (`followSession`). */
export interface SessionAdmission {
  /** Records that the connection is admitted as `session` now. */
  readonly admit: (session: string) => void
  /** Settles once the connection is admitted as `session`: at once where it is now, else when a
   *  later `admit` names it. Only the latest id asked about is waited for: a wait for an id asked
   *  about before it is dropped unsettled, since the live view takes only its latest id's subject.
   *  The wait is kept through an `admit` of another id, which an earlier `/clear`'s move can land
   *  as after the live view has heard of a later one. */
  readonly admitted: (session: string) => Promise<void>
}

/** A {@link SessionAdmission} admitted as `first`. */
export function sessionAdmission(first: string): SessionAdmission {
  let current = first
  let pending: { readonly session: string; readonly wait: PromiseWithResolvers<void> } | undefined
  return {
    admit: (session) => {
      current = session
      if (pending?.session !== session) return
      pending.wait.resolve()
      pending = undefined
    },
    admitted: (session) => {
      if (session === current) return Promise.resolve()
      if (pending?.session !== session) {
        pending = { session, wait: Promise.withResolvers<void>() }
      }
      return pending.wait.promise
    },
  }
}

/** A connection's credential move (`followSession`), wrapped so that it records, once it lands,
 *  the id it admitted the connection as (`admittingMoves`). */
export type AdmittingMove = (
  follow: (() => Promise<void>) | undefined,
) => (() => Promise<void>) | undefined

/**
 * Wraps each connection's credential move so that it records, once it lands, the id it admits the
 * connection as: the id the session had as the move began, which a later `/clear` or `/new` does
 * not change. Each server wraps its first connection's move and each one a reopen dials in its
 * place (`channelReopener`). A connection that presents no broker token has no move to wrap, and
 * `admission` is then undefined.
 */
export function admittingMoves(
  admission: SessionAdmission | undefined,
  identity: SessionIdentity,
): AdmittingMove {
  return (follow) =>
    follow === undefined || admission === undefined
      ? undefined
      : async (): Promise<void> => {
          const forSession = identity.id
          await follow()
          admission.admit(forSession)
        }
}

/** The channel server's NATS connection, as `dialChannelNats` opened it. */
export interface ChannelNats {
  readonly connection: NatsConnection
  /** Stops the keeper of the connection's broker token; nothing where it presents none. */
  readonly stop: () => void
  /** The move a `/clear` runs before the new id's relay subject opens (`followNatsCredential`),
   *  where the connection presents the session's broker token. */
  readonly followSession?: () => Promise<void>
}

/** How a failed dial names the credential it presented. */
const CREDENTIAL_NAMES: Record<NatsCredential["kind"], string> = {
  broker: "the session's broker token",
  seed: "an nkey seed",
  none: "no credential",
}

/** The channel server's dials: every plugin's waits, for the server's life. */
export const CHANNEL_DIAL_PLAN: NatsDialPlan = {
  ...NATS_DIAL_PLAN,
  budgetMs: Number.POSITIVE_INFINITY,
}

/** What `dialChannelNats` dials: the session's NATS servers, the setting that names them, and the
 *  credential the connection presents (`EnvoyInbound` where inbound messages are on). */
export interface ChannelNatsSettings {
  readonly natsUrls: readonly string[]
  readonly natsServersSetting: string
  readonly natsCredential: NatsCredential
}

/** What `dialChannelInbound` opens the listener's relay socket with (`EnvoyInbound` where
 *  `ENVOY_INBOUND=listener`): the setting that chose it, and the session's broker credential. */
export interface ChannelRelaySettings {
  readonly relaySetting: string
  readonly relayCredential: RequestCredential
}

/** How a server takes its session's inbound messages (`EnvoyInbound` where they are on). */
export type ChannelInboundSettings = ChannelNatsSettings | ChannelRelaySettings

/**
 * Opens the session's NATS connection with the credential `inbound` resolved. A broker token is
 * signed into the CONNECT itself, so a live one is taken before the dial; and the connection is
 * then kept on a token the auth service admits (`keepNatsCredential`, warnings through `warn`,
 * with no dial error, which tells them from a redial's), until `stop` or the connection closing.
 * A dial that fails is dialed again as `plan` says, which by default is for the server's life;
 * each redial is warned of, naming the servers by their setting and the credential it presented
 * beside why it failed, as a log may print both
 * (`natsDialFailure`: no part of a server's text, and the reason nats.js's code or the server's
 * fixed refusal, never text derived from what the servers were set to or what the client sent),
 * and `warn` is handed the failed dial's error with its line. A dial that fails for good (an
 * authorization refusal, or the end of a finite `plan`) throws the same line, as a
 * `LoggableError`; the error keeps no cause: nats.js's own error still carries them. A token that
 * cannot be taken fails with the credential's own line (`BrokerNatsCredential.ready`). `dial` is
 * nats.js's `connect`, or a test's.
 *
 * `signal` aborting (the process is stopping) ends the wait for the token and for the dial at once,
 * with `START_STOPPING`, and starts no further dial; the token trade itself goes on, since it is
 * shared with every caller of this session's token, and a connection the dial still makes is
 * closed.
 */
export async function dialChannelNats(
  inbound: ChannelNatsSettings,
  name: string,
  warn: (text: string, dialError?: unknown) => void,
  dial: (options: ConnectionOptions) => Promise<NatsConnection> = connect,
  signal?: AbortSignal,
  plan: NatsDialPlan = CHANNEL_DIAL_PLAN,
): Promise<ChannelNats> {
  const stopped = Promise.withResolvers<"stopping">()
  const onAbort = (): void => stopped.resolve("stopping")
  if (signal?.aborted === true) onAbort()
  signal?.addEventListener("abort", onAbort)
  /** `waited`, or `START_STOPPING` the moment `signal` aborts. A rejection that lands after the
   *  stop won is settled here, never left unhandled. */
  const unlessStopped = async <T>(waited: Promise<T>): Promise<T> => {
    const outcome = await Promise.race([
      waited.then(
        (value) => ({ value }),
        (error: unknown) => ({ error }),
      ),
      stopped.promise,
    ])
    if (outcome === "stopping") throw new Error(START_STOPPING)
    if ("error" in outcome) throw outcome.error
    return outcome.value
  }
  try {
    const { natsCredential } = inbound
    const failed = (error: unknown): string =>
      natsDialFailure(
        natsServersForLog(inbound.natsServersSetting, inbound.natsUrls.length),
        error,
        CREDENTIAL_NAMES[natsCredential.kind],
      )
    const broker = natsCredential.kind === "broker" ? natsCredential.broker : undefined
    // Waits on the token client's retry loop for the server's life: a pod session not registered
    // yet (#4153), a broker that does not answer, a 5xx. Claude Code does not restart a channel
    // server that ended, so a start that failed on any of them would leave the session with no
    // delivery at all; the loop says what it waits on at each wait, and a refusal no trade cures
    // still ends the start. `unlessStopped` ends the wait at a stop.
    await unlessStopped(
      broker?.ready({ life: signal ?? new AbortController().signal }) ?? Promise.resolve(),
    )
    // A dial that fails is dialed again for the server's life, as the token wait above is:
    // Claude Code starts no channel server again once one ended, and a hub closes a dial whose
    // CONNECT came after its 2 s authentication timeout, then answers the next one. Each dial has
    // its own bound (`connectNats`); a stop ends the redials.
    const dialing = connectNats(
      dial,
      natsConnectionOptions({ servers: inbound.natsUrls, name, credential: natsCredential }),
      plan,
      {
        stopped: () => signal?.aborted === true,
        onRetry: (error, delayMs) =>
          warn(`${failed(error)}; dialing again in ${delayMs} ms`, error),
      },
    )
    let connection: NatsConnection
    try {
      connection = await unlessStopped(dialing)
    } catch (error) {
      if (signal?.aborted === true) {
        // A dial that completes after the stop opened a connection nobody will use.
        dialing.then((late) => late.close()).catch(() => undefined)
        throw new Error(START_STOPPING)
      }
      throw new LoggableError(failed(error))
    }
    if (broker === undefined) return { connection, stop: () => {} }
    return {
      connection,
      stop: keepNatsCredential(connection, broker, warn),
      followSession: () => followNatsCredential(connection, broker),
    }
  } finally {
    signal?.removeEventListener("abort", onAbort)
  }
}

/** The channel server's inbound connection, over NATS or the listener's relay socket
 *  (`dialChannelInbound`). */
export interface ChannelInbound {
  readonly connection: ChannelConnection
  /** Stops the keeper of a NATS connection's broker token; nothing for the others. */
  readonly stop: () => void
  /** The move a `/clear` or a `/new` runs before the new id's relay subject opens, where the
   *  connection is admitted as one session at a time: a NATS connection presenting the session's
   *  broker token (`followNatsCredential`), and the relay socket, which is opened for one session
   *  with that session's own token (`ListenerRelay.follow`). */
  readonly followSession?: () => Promise<void>
  /** Whether the session's live view takes its control subject on the connection: never over the
   *  relay socket, which carries no agent stream (`ControlSubscriptionOptions.liveView`). */
  readonly liveView: boolean
}

/**
 * Opens the session's inbound as its settings say: NATS (`dialChannelNats`, `dial` its `connect`),
 * or the relay socket of the listener at `listenerUrl` for the session `session` names now, with
 * the session's broker credential (`connectListenerRelay`), answered once its first socket is
 * open. The socket opens again on its own after a close for the server's life, warning on `warn`
 * why an open failed; a token the broker refused is told on `warn` until the session holds the
 * socket and tells its own outage (`ListenerRelay.reportOutagesTo`). `signal` aborting (the
 * process is stopping) ends either wait at once.
 */
export async function dialChannelInbound(
  inbound: ChannelInboundSettings,
  listenerUrl: string,
  session: () => string,
  name: string,
  warn: (text: string, dialError?: unknown) => void,
  dial: (options: ConnectionOptions) => Promise<NatsConnection> = connect,
  signal?: AbortSignal,
): Promise<ChannelInbound> {
  if ("natsUrls" in inbound) {
    return { ...(await dialChannelNats(inbound, name, warn, dial, signal)), liveView: true }
  }
  const relay = await connectListenerRelay(
    {
      url: listenerUrl,
      session,
      credential: inbound.relayCredential,
      outage: inboundUnavailable(
        (why) => warn(`this session's relay socket has no token: ${why}`),
        () => warn("this session's relay socket has its token again"),
      ),
      log: (line) => warn(line),
    },
    signal,
  )
  return { connection: relay, stop: () => {}, followSession: () => relay.follow(), liveView: false }
}

/** How long a start that failed waits for its connection to drain before closing it. */
const START_DRAIN_BOUND_MS = 2_000

/**
 * Ends the connection of a session that never started: its start failed, or a stop ended it.
 * Both servers' teardowns call it (`runEnvoyChannelServer`, `./codex/codex-server`). The drain is
 * bounded: a connection still redialing, or one whose path stopped carrying the hub's answers, as
 * one whose start a stop ended can be, may never finish a drain, and the close after it is what
 * ends the connection for good.
 */
export async function closeUnstartedConnection(connection: ChannelConnection): Promise<void> {
  await Promise.race([connection.drain().catch(() => undefined), Bun.sleep(START_DRAIN_BOUND_MS)])
  await connection.close().catch(() => undefined)
}

/** A connection `ChannelSessionOptions.reopen` opened in place of one closed for good. */
export interface ReopenedConnection {
  readonly connection: ChannelConnection
  /** `ChannelSessionOptions.followSession`, for this connection. */
  readonly followSession?: () => Promise<void>
}

/** Opens a new inbound connection in place of one closed for good
 *  (`ChannelSessionOptions.reopen`). */
export type ReopenConnection = (failed: (dialError: unknown) => void) => Promise<ReopenedConnection>

/** What a server hands `channelReopener`. */
export interface ChannelReopenerOptions {
  /** What the server's first connection was opened with. */
  readonly inbound: ChannelInboundSettings
  /** The listener a relay socket is opened at (`EnabledEnvoySettings.url`). */
  readonly listenerUrl: string
  /** The session id the server holds now, which a relay socket is opened for. */
  readonly session: () => string
  /** The connection name every connection of the server presents. */
  readonly name: string
  /** Aborts when the server stops: it ends a dial's waits and its redials, and closes a connection
   *  a dial opened after it. */
  readonly signal: AbortSignal
  /** Stops the token keeper the server holds now and holds `stop` in its place: the one the
   *  server's teardown stops. */
  readonly swapKeeper: (stop: () => void) => void
  /** The server's wrapper of a connection's credential move (`admittingMoves`). */
  readonly admitting: AdmittingMove
  /** Moves what else the server holds on its connection, its live view, to `connection`. */
  readonly useConnection: (connection: ChannelConnection) => void
}

/**
 * A server's `ChannelSessionOptions.reopen`: a new connection once the session's closed for good,
 * opened as the server's first one was (`dialChannelInbound`), warning of each redial on stderr,
 * until one opens or the server stops. A relay socket opens again on its own after every close, so
 * it closes for good only as the server closes it; a NATS connection is what this replaces in a
 * running session. The closed connection's token keeper stops before the dial, and the new one's is
 * the one the teardown stops; the live view moves to the new connection. Under a broker token the
 * new connection presents the token the session's credential holds as it dials, the one traded
 * last (a lapsed one is traded again for the id the session has now): through a `/clear`, the old
 * id's until the `/clear`'s move has traded the new id's, and the new id's from then on, even
 * where the connection closed before the move reconnected with it. The admission is left as it
 * is either way: a `/clear` whose move had not ended runs it again on the new connection
 * (`register`), and that admits the new id once the connection presents its token, at once where
 * it already does. A connection a dial opens after the server began stopping is closed. Both
 * servers take theirs from here: Claude Code's (`runEnvoyChannelServer`) and Codex's
 * (`./codex/codex-server`).
 */
export function channelReopener(options: ChannelReopenerOptions): ReopenConnection {
  return async (failed) => {
    options.swapKeeper(() => undefined)
    const next = await dialChannelInbound(
      options.inbound,
      options.listenerUrl,
      options.session,
      options.name,
      (text, dialError) => {
        process.stderr.write(`envoy-channel: ${text}\n`)
        if (dialError !== undefined) failed(dialError)
      },
      connect,
      options.signal,
    )
    if (options.signal.aborted) {
      next.stop()
      void next.connection.close().catch(() => undefined)
      throw new Error(START_STOPPING)
    }
    options.swapKeeper(next.stop)
    options.useConnection(next.connection)
    const followSession = options.admitting(next.followSession)
    return {
      connection: next.connection,
      ...(followSession === undefined ? {} : { followSession }),
    }
  }
}
