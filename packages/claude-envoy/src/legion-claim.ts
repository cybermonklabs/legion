import { readdir, stat } from "node:fs/promises"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { SESSION_LIVE_WINDOW_MS } from "@legion/contracts"
import { daemonContract } from "@legion/contracts/daemon-contract"
import type { LegionTask } from "@legion/contracts/legion-api"
import { messageFor } from "@legion/envoy-client/errors"
import type { RelayTaskValue } from "@legion/envoy-client/relay-frame"
import { readSecretFile } from "@legion/envoy-client/secret-file"
import type { EnvoyClient } from "@legion/envoy-client/transport"
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client"
import { bootLegionClaim, type LegionClaimBoot } from "@legion/pi-shared/legion-claim-core"
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops"
import pkg from "../package.json" with { type: "json" }
import { configuredValue } from "./claude-session"
import type { ChannelSession } from "./envoy-channel-server"
import type { InboxTaskValue } from "./legion-inbox"
import { logRoleClaimWarnings } from "./role-claim-warnings"

/**
 * A Claude Code phase worker's claim on the Legion daemon, made by the MCP server Claude Code runs
 * for the pane (`dist/envoy-channel.js`, under the server name `legion`) through the claim boot
 * every harness shares (`@legion/pi-shared/legion-claim-core`). The daemon launches the pane with
 * its boot token (`LEGION_BOOT_TOKEN_FILE`) and its address (`LEGION_DAEMON_URL`).
 */

/** The daemon API contract this build speaks, read once as the module loads (`daemonContract`):
 * the manifest of the module this process loaded names it once a pack or `scripts/build.ts --out`
 * has written it, and a checkout's own fixtures decide it for code run from the checkout. A
 * bundle installed with neither refuses to load here rather than speak a number it guessed. */
const PLUGIN_CONTRACT = daemonContract(pkg, import.meta.dir)

/** What the daemon put on a Legion pane: its address, the boot token the registration spends, the
 * claim's own Claude Code directory (`CLAUDE_CONFIG_DIR`), which holds the transcript, and whether
 * its pod's launcher keeps that transcript in the session table (`LEGION_CLAUDE_SESSION_ID`, which
 * only the Kubernetes runtime sets), so the registration waits for the launcher's first save. */
export interface LegionPane {
  readonly daemonUrl: string
  readonly bootToken: string
  readonly configDir: string
  readonly launcherSaves: boolean
}

/**
 * The Legion pane this server runs in, or undefined when no boot token is on its environment (a
 * Claude Code session outside Legion). A set `LEGION_BOOT_TOKEN_FILE` is authoritative, as in
 * every pane, and read without holding the event loop: a missing or blank file rejects naming it.
 * A pane without `LEGION_DAEMON_URL` or `CLAUDE_CONFIG_DIR` rejects naming the variable.
 */
export async function legionPane(env: NodeJS.ProcessEnv): Promise<LegionPane | undefined> {
  const pointer = env["LEGION_BOOT_TOKEN_FILE"]
  if (pointer === undefined && configuredValue(env["LEGION_BOOT_TOKEN"]) === undefined) {
    return undefined
  }
  const required = (key: string): string => {
    const value = configuredValue(env[key])
    if (value === undefined) {
      throw new Error(`${key} is required for Legion: the pane carries a boot token`)
    }
    return value
  }
  return {
    daemonUrl: required("LEGION_DAEMON_URL"),
    configDir: required("CLAUDE_CONFIG_DIR"),
    bootToken:
      pointer === undefined
        ? required("LEGION_BOOT_TOKEN")
        : await readSecretFile("LEGION_BOOT_TOKEN_FILE", pointer),
    launcherSaves: configuredValue(env["LEGION_CLAUDE_SESSION_ID"]) !== undefined,
  }
}

/**
 * The pane's client for the daemon. It states this build's own contract on every request, as the
 * claim's registration below states it: the daemon serves each answer in the shape the contract
 * the caller states describes, so a pane an older or newer daemon answers still reads its answers.
 */
export function paneDaemonClient(daemonUrl: string): LegionDaemonClient {
  return createLegionDaemonClient(daemonUrl, { pluginContract: PLUGIN_CONTRACT })
}

/** Writes one line to the pane's log, the server's stderr. */
export function logLegion(line: string): void {
  process.stderr.write(`[legion] ${line}\n`)
}

/**
 * Ends the pane's Claude Code, this server's parent, so the daemon launches the claim again: what
 * an Oh My Pi pane's own exit does. `legion claude-bridge` exits with Claude Code's status, and the
 * shim reports the death.
 */
export function endPane(): void {
  process.kill(process.ppid, "SIGTERM")
}

/**
 * Logs why the pane cannot run, then ends it (`endPane`). `server` is the channel server's stop
 * and its writer of the pane's log, which passes each line through the server's tell gate
 * (`logLegionLine` in `./envoy-channel-server`). Once the server's stop has begun this does
 * nothing (R24 of dispatch://INFRA-3589): the stop ends the pane already, and `endPane` would
 * SIGTERM the `claude` this server runs under on its way out.
 */
export function stopPane(
  why: string,
  server: { readonly stopping: () => boolean; readonly log: (line: string) => void },
): void {
  if (server.stopping()) return
  server.log(`${why}; stopping the pane so the daemon launches it again`)
  endPane()
}

/**
 * The transcript Claude Code writes for `sessionID`, `<configDir>/projects/<cwd>/<id>.jsonl`, once
 * it exists: the value a relaunch's `--resume` continues. A fresh session writes nothing until its
 * first input, which is the bridge's `/rename` seed (`packages/daemon/internal/claudebridge`); a
 * resumed one already has it. Polls every `pollMs` until `signal` aborts.
 */
export async function waitForTranscript(
  configDir: string,
  sessionID: string,
  signal: AbortSignal,
  pollMs = 100,
): Promise<string> {
  const projects = join(configDir, "projects")
  const name = `${sessionID}.jsonl`
  for (;;) {
    signal.throwIfAborted()
    for (const directory of await readdir(projects).catch(() => [] as string[])) {
      const file = join(projects, directory, name)
      if (await isFile(file)) return file
    }
    await delay(pollMs, undefined, { signal })
  }
}

/** The file a pod's launcher writes beside a transcript once its first save of it has committed
 * to the session table (`SavedMarkerSuffix`, `packages/daemon/internal/launcher/claude.go`). */
export const SAVED_MARKER_SUFFIX = ".legion-saved"

/**
 * Waits until the pod's launcher has saved `transcript` to the session table, which it says by
 * writing `<transcript>.legion-saved` beside it: the pod's disk dies with the pod, so a transcript
 * the daemon recorded before the table held it would be refused at the next pod's resume. Polls
 * every `pollMs` until `signal` aborts.
 */
async function waitForSavedTranscript(
  transcript: string,
  signal: AbortSignal,
  pollMs = 100,
): Promise<void> {
  const marker = `${transcript}${SAVED_MARKER_SUFFIX}`
  for (;;) {
    signal.throwIfAborted()
    if (await isFile(marker)) return
    await delay(pollMs, undefined, { signal })
  }
}

function isFile(file: string): Promise<boolean> {
  return stat(file).then(
    (info) => info.isFile(),
    () => false,
  )
}

/**
 * How long apart the attempts of {@link startLegionClaim}'s wait on a claim the listener refused
 * naming this very session are.
 *
 * The wait runs after `claims/ready`, not before it: the role is claimed right after the ready
 * has answered (`bootLegionClaim`), so the daemon's registration deadline —
 * `worker_boot_timeout_seconds` × `worker_boot_registration_deadline_intervals`, 120 s × 3 = 360
 * s with the shipped defaults (`packages/daemon/internal/config/config.go`, armed by `register`
 * through `armRegistration` in `packages/daemon/internal/supervise`) — is already met when the
 * first attempt goes out, and no part of this wait is spent against it.
 *
 * What bounds the wait now is the listener's own live window: the standing claim of the process
 * this pane replaced is granted to nobody until a whole `SESSION_LIVE_WINDOW_MS` has passed with
 * no answer for it, so no interval can bring the grant forward, and every part of an interval
 * past that point is dead waiting with the session already ready — the daemon may have handed it
 * a task, and the lane the listener keeps for a role no session holds is handed over only when
 * the claim lands. The interval is kept at 10 s for that: the grant falls at most 10 s after the
 * window passes, where the session heartbeat's own cadence (`SESSION_HEARTBEAT_MS`, 90 s) would
 * leave the pane unaddressable for up to 90 s more.
 *
 * What the shorter interval costs: one `POST /v1/roles/set` per interval, a conditional write the
 * listener refuses without touching the claim, at most about 30 of them, and only in a pane the
 * daemon relaunched into its predecessor's standing claim — once per such relaunch, for at most
 * one live window.
 */
export const ROLE_CLAIM_RETRY_MS = 10_000

/**
 * How long that same wait keeps asking before it gives up and the pane is stopped: the live
 * window and one attempt more, so the last attempt falls after the window the process this pane
 * replaced could still have been answering for its claim in, even when it answered for it at the
 * very moment this pane first asked.
 *
 * That window is the whole of what can explain a refusal naming this session, and `claims/ready`
 * is already answered, so nothing outside the pane is counting: past the wait, the claim is
 * stopped on as every other failure after a registration is, and the pane writes why to its log
 * and is launched again rather than sitting nameless for a lane it can never be handed.
 */
export const ROLE_CLAIM_WAIT_MS = SESSION_LIVE_WINDOW_MS + ROLE_CLAIM_RETRY_MS

export interface LegionClaim {
  /** The registered claim, or undefined until `claims/register` has answered. */
  readonly capability: () => ClaimCapability | undefined
  /** Settles once the boot has run — ready sent and the Envoy role claimed, or the failure
   * reported and the pane stopped, or a boot that never reached a registration logged. Never
   * rejects. */
  readonly booted: Promise<void>
  /** A claim's task value a task frame relayed (`onTaskValue`, the channel session's
   * `taskValues`): handed to the bridge when it is this claim's. */
  readonly relayed: (value: RelayTaskValue) => void
  /** Reads the claim's task value from the daemon (`claims/task`) and hands it to the bridge: at
   * a new epoch of the session's relay, which lost whatever the listener relayed while the
   * session was not subscribed. Reads nothing before the boot has claimed the role, and never
   * rejects. */
  readonly readTask: () => Promise<void>
}

/**
 * Boots the pane's claim in the background of the server (`bootLegionClaim`): waits for the
 * transcript, and in a pod whose launcher keeps it (`launcherSaves`) for the launcher's first save
 * of it too, registers with it as the session file (`agentId` is the session id,
 * `pluginContract` this build's own contract), sends `claims/ready` with no
 * capability report, and claims the claim token's Envoy role soft right after it, recording it
 * with the channel session, whose heartbeat re-asserts it. The claim names the session the
 * registration says this one replaces (`replaces`) as its `previous_session_id`, and each warning
 * the listener answers it with is logged. A failure the core stops on ends the pane through
 * `stop`; a failure before a registration is logged and leaves the pane to the daemon's
 * registration deadline.
 *
 * The claim keeps none of its task values: it hands the bridge every one it is handed
 * (`task`, the inbox's task line) — the registration's, the `claims/task` read the shared core
 * makes once the ready has answered, one at each regain of the role and at each new epoch of the
 * relay (`readTask`), and each one a task frame relays (`relayed`) — each with the number the
 * daemon's reading it came with says the claim finished, 0 for a relayed one; a reading that holds
 * no value hands the bridge that number alone (`finished`, the inbox's finished line). Once that
 * read after the ready has answered, it writes the ready line (`ready`): behind the reading's own
 * line, and after a read that failed too (logged, with no reading line), so the bridge never stands
 * ready without the reading it was given. The bridge, which knows whether Claude Code is in a
 * turn, holds the finished number, the ready gate and the newest value, reports each value seen
 * and takes the task (`packages/daemon/internal/claudebridge/take.go`), as Oh My Pi's take does
 * (`@legion/pi-shared/task-take`), and holds every card until the ready line, the session's own
 * direct message included. Both lines are written before the role is claimed, and the inbox
 * carries every line in the order it is written, so every card — one the role's kept lane hands
 * over at the claim, or one sent to the session before it — waits behind the take of each value
 * the ready decision wrote (the plan's decision 16). A value the daemon writes later — the resume
 * its outbox queues after the ready, a task queued later still — reaches the bridge as the
 * listener relays it, and a card that arrives before it may start a turn first, which the take
 * then waits behind.
 *
 * A claim the listener refuses naming this very session is waited out rather than stopped on:
 * the pane the daemon relaunched with `--resume` meets the standing claim of the process it
 * replaced, and that claim is granted the moment nobody has answered for it inside the
 * listener's live window (`roleWaitMs`). Stopping instead would hand the daemon a relaunch that
 * meets the same claim, for a whole window of relaunches.
 *
 * The claim is asked again every {@link ROLE_CLAIM_RETRY_MS} for up to
 * {@link ROLE_CLAIM_WAIT_MS}, the listener's live window and one attempt more, which is the
 * whole of what can explain such a refusal.
 */
export function startLegionClaim(deps: {
  readonly pane: LegionPane
  readonly sessionID: string
  readonly daemon: LegionDaemonClient
  readonly client: Pick<EnvoyClient, "setRole">
  /** The channel server's own client instance: the id its registrations carry, which this
   *  claim names as the process holding the role. */
  readonly clientInstanceID: string
  readonly session: Pick<ChannelSession, "rememberRole" | "onRoleRegained">
  /** Hands the bridge the claim's task value (`LegionInbox.task`). */
  readonly task: (value: InboxTaskValue) => void
  /** Tells the bridge the claim's ready has answered, and so has the read of the claim's value
   * after it (`LegionInbox.ready`). */
  readonly ready: () => void
  /** Hands the bridge the finished number of a reading that holds no value
   * (`LegionInbox.finished`). */
  readonly finished: (finished: number) => void
  readonly signal: AbortSignal
  /** Writes one line to the pane's log, the server's stderr. */
  readonly log: (line: string) => void
  /** Ends the pane's Claude Code, this server's parent, so the daemon launches the claim again. */
  readonly stop: () => void
  /**
   * How long a claim refused by a standing claim under this very session id keeps asking before
   * it gives up and the pane is stopped: {@link ROLE_CLAIM_WAIT_MS}.
   */
  readonly roleWaitMs?: number
  /** How long between those attempts: {@link ROLE_CLAIM_RETRY_MS}. */
  readonly roleRetryMs?: number
  readonly pollMs?: number
}): LegionClaim {
  let capability: ClaimCapability | undefined
  const { sessionID } = deps
  const roleWaitMs = deps.roleWaitMs ?? ROLE_CLAIM_WAIT_MS
  const roleRetryMs = deps.roleRetryMs ?? ROLE_CLAIM_RETRY_MS
  let claimBoot: LegionClaimBoot | undefined

  /** A reading of the claim's value (the registration's answer, a `claims/task` read), handed to
   * the bridge: its value with the number the reading says the claim finished, or that number
   * alone where it holds no value. */
  const read = (reading: LegionTask): void => {
    if (reading.task === null) deps.finished(reading.finished)
    else deps.task({ ...reading.task, finished: reading.finished })
  }

  const boot = async (): Promise<void> => {
    const transcript = await waitForTranscript(
      deps.pane.configDir,
      sessionID,
      deps.signal,
      deps.pollMs,
    )
    if (deps.pane.launcherSaves) {
      await waitForSavedTranscript(transcript, deps.signal, deps.pollMs)
    }
    claimBoot = await bootLegionClaim({
      daemon: deps.daemon,
      registration: {
        bootToken: deps.pane.bootToken,
        sessionId: sessionID,
        ompSessionFile: transcript,
        agentId: sessionID,
        pluginContract: PLUGIN_CONTRACT,
      },
      afterRegister: async (registered) => read(registered),
      taskValue: read,
      // The read after the ready has answered, and its value or finished number is in the inbox
      // already, or its failure is logged: the ready line goes behind it and ahead of the role
      // claim, at which the listener hands the role's kept lane over, and the bridge holds every
      // card until this line.
      afterReadyRead: deps.ready,
      setCapability: (registered) => {
        capability = registered
      },
      claimRole: async (role, replaces) => {
        // Soft, so a role another live session holds is left with it, and naming this process,
        // so the listener records the claim as this process's: a second `claude` that resumed
        // this session id can then neither be taken for the holder nor release what it holds.
        // `replaces`, the session the registration says this one replaces, is named as the
        // claim's predecessor: the listener hands that session's unread direct and role messages
        // to this one once the claim lands.
        //
        // A refusal naming this very session is that same claim seen from the other side: the
        // process this pane was relaunched from renewed it at its last heartbeat, so it stands
        // inside the listener's live window and the listener grants it to nobody else until
        // that window passes with no answer for it. So the claim is asked again on the
        // channel server's own heartbeat cadence until it is granted. Only a refusal naming a
        // different session throws, which stops the pane: that role is another session's, and
        // no amount of asking changes it.
        const deadline = Date.now() + roleWaitMs
        let waited = false
        for (;;) {
          const claimed = await deps.client.setRole({
            sessionID,
            role,
            soft: true,
            clientInstanceID: deps.clientInstanceID,
            ...(replaces ? { previousSessionID: replaces } : {}),
          })
          if (claimed.claimed) {
            logRoleClaimWarnings(deps.log, role, sessionID, claimed.warnings)
            break
          }
          if (claimed.holder !== sessionID) {
            throw new Error(`Envoy role ${role} is held by live session ${claimed.holder}`)
          }
          const left = deadline - Date.now()
          if (left <= 0) {
            throw new Error(
              `Envoy role ${role} is still claimed under this session by another process after ${roleWaitMs} ms of asking`,
            )
          }
          if (!waited) {
            waited = true
            // Said once: the state is normal and self-resolving, and a line per attempt would
            // fill the pane's log with the same sentence for a whole live window.
            deps.log(
              `Envoy role ${role} is claimed under this session by the process this pane replaced; asking again every ${roleRetryMs} ms for up to ${roleWaitMs} ms, until nobody has answered for that claim inside the listener's live window`,
            )
          }
          await delay(Math.min(roleRetryMs, left), undefined, { signal: deps.signal })
        }
        await deps.session.rememberRole(role)
      },
      onRoleRegained: (listener) => deps.session.onRoleRegained(listener),
      log: deps.log,
      // The server's shutdown: a claim answered 503 is not made again once it aborts.
      signal: deps.signal,
      stopping: "stopping the pane so the daemon launches it again",
      // A boot the shutdown aborted stops nothing: this server is already going, and `stop`
      // would SIGTERM the `claude` it runs under on its way out.
      stop: () => {
        if (!deps.signal.aborted) deps.stop()
      },
    })
  }

  // A boot that failed before a registration claims nothing, and says so with a `booted` that
  // settles: a caller waiting on the boot is not left waiting for a step that is never coming.
  const booted = boot().catch((error: unknown): void => {
    if (!deps.signal.aborted) {
      deps.log(
        `claim boot failed before a registration; the daemon's registration deadline relaunches the pane: ${messageFor(error)}`,
      )
    }
  })
  return {
    capability: () => capability,
    booted,
    relayed: (value) => {
      if (value.claim !== capability?.claimToken) return
      // A relay frame carries no reading, so it says nothing of what the claim finished.
      const at = { epoch: value.epoch, revision: value.revision, finished: 0 }
      deps.task({ number: value.op === "delete" ? 0 : value.task, ...at })
    },
    readTask: async () => {
      await claimBoot?.readTask()
    },
  }
}
