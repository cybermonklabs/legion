import type { LegionRegistration, LegionTask } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  callReadyWithRetry,
  claimCaller,
  LegionDaemonApiError,
  type LegionDaemonClient,
  type RegisterInput,
} from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";

/** What a host gives the claim boot: the registration it sends, and how it claims the Envoy role,
 * hears a regain, takes the claim's task value, records the capability, logs and stops. */
export interface LegionClaimHost {
  readonly daemon: LegionDaemonClient;
  /** `claims/register`'s body: the pane's boot token, the session, its transcript, and the
   * contract this build speaks (`pluginContract`, the package's `legion.daemonApiVersion`). */
  readonly registration: RegisterInput;
  /** Runs once the registration answered, before the capability is recorded, the ready and the
   * role claim. The answer carries the claim's task value (`task`, `finished`), which a host's
   * take holds from here (`src/task-take.ts`), so the mark it keeps while the task's turn is
   * pending stands before the role's kept lane is handed over at the role claim. Oh My Pi's claim
   * also holds that mark from here until `afterReadyRead` runs (`TaskTake.awaitRead`), whatever
   * value the registration carried, since the ready decision can write one. */
  readonly afterRegister?: (claim: LegionRegistration) => Promise<void>;
  /** Records the registered claim, before `claims/ready`: what serves the claim's task reads the
   * capability, and a take is answered only once the ready is in. */
  readonly setCapability: (capability: ClaimCapability) => void;
  /** Claims `role`, the claim token, on Envoy for this session: the topic the listener relays the
   * claim's task value and every notice on. It runs right after `claims/ready` has answered, the
   * claim's value has been read again (`taskValue`) and `afterReadyRead` has run: the task is a
   * value the session takes once its host is idle, so nothing the role's kept lane hands over can
   * take the task's turn, and the session holds its name from its ready on. Throws when the role
   * cannot be held, which is logged and stops the session; a claim the listener answered 503 is
   * claimed again instead (`claimRoleAgainOnBusy`). */
  readonly claimRole: (role: string) => Promise<void>;
  /**
   * Runs once the read of the claim's value after `claims/ready` has answered — its reading handed
   * to `taskValue`, or its failure logged — and before the role claim; never when the ready
   * failed. Until then no card may start a turn, the session's own direct message included, since
   * the ready decision can have written a value the host does not hold yet (the plan's decision
   * 15). Oh My Pi's claim ends there the hold on its take's task-turn mark it took at the
   * registration (`TaskTake.awaitRead`), leaving the mark to the take's own rule; claude-envoy's
   * writes the Claude bridge's ready line there, behind the reading's own line, and the bridge
   * holds every card until that line (decision 16).
   */
  readonly afterReadyRead: () => void;
  /**
   * Takes the claim's task value as the daemon reads it now (`claims/task`): once the ready has
   * answered, before the role claim; at each regain of the role; and at each read the host asks
   * for (`LegionClaimBoot.readTask`) when its relay begins a new epoch — a value the listener
   * relayed while the session did not hold its role, or was not subscribed, is lost, so it is
   * read rather than waited for. claude-envoy's claim asks for that read and hands every reading
   * to the Claude bridge; Oh My Pi's claim hands each reading to its take (`src/task-take.ts`),
   * whose own read (`takeClaimTask`) covers a new epoch of its relay, so it asks for none.
   */
  readonly taskValue: (reading: LegionTask) => void;
  /** The first wait before the role is claimed again after the listener answered the claim 503,
   * doubled at each retry up to `ROLE_CLAIM_RETRY_MAX_DELAY_MS`. Unset, `ROLE_CLAIM_RETRY_FIRST_DELAY_MS`;
   * a test sets it short. */
  readonly roleClaimRetryFirstDelayMs?: number;
  /** Aborts when the host stops: a claim the listener answered 503 is not made again after it. */
  readonly signal?: AbortSignal;
  /** Sets the one hook the Envoy heartbeat runs after it re-establishes this session as a role's
   * holder, with why it did (`reclaimed`, `reregistered`). */
  readonly onRoleRegained: (listener: (role: string, reason: string) => Promise<void>) => void;
  /** Writes one line to the agent's log. */
  readonly log: (line: string) => void;
  /** What `stop` does, as a log line names it: `exiting so the daemon launches it again`. */
  readonly stopping: string;
  /** Ends the claim's agent, so the daemon launches it again. */
  readonly stop: () => void;
}

/** A claim the boot took to its role: what its host asks of it afterwards. */
export interface LegionClaimBoot {
  /** Reads the claim's task value from the daemon and hands it to the host's `taskValue`; a read
   * that fails is logged and asks nothing again, since the next epoch, regain or relayed value
   * carries the value as well. Never rejects. */
  readonly readTask: () => Promise<void>;
}

/**
 * Boots a claim on `/legion/v1/claims/*`, the same on every harness: `claims/register`;
 * `afterRegister`, where the host takes the registration's task value; the capability recorded;
 * `claims/ready`, retried on a 5xx or transport failure (`callReadyWithRetry`); the claim's task
 * value read again (`claims/task`) into the host's `taskValue`; `afterReadyRead` once that read
 * has answered; the Envoy role claimed right after that, and claimed again while the listener
 * answers it 503; and the regain listener, which reads the
 * claim's task value again, once, each time the heartbeat regains the role, logging a read that
 * fails. It answers the claim it booted, through which the host reads that value again at a new
 * epoch of its relay, or undefined for a boot that stopped.
 *
 * The daemon writes the number of the task a claim is owed as the claim's value, which the
 * listener relays to the session holding the role, and the session takes it once its host is idle
 * (`src/task-take.ts`). The registration's answer carries the value the daemon held then, but the
 * ready decision can write one of its own (the relaunch record's task), and the listener relays a
 * value only when its watch of the role's holder fires, after the role claim has handed the
 * role's kept lane over. So the value is read once the ready has answered and before the role is
 * claimed, and the host holds it, with its take's mark, when the lane the listener kept for the
 * role arrives (`packages/pi-envoy/src/card-wake.ts`): every value the ready decision wrote is
 * ahead of every card. The session's agent subject is open from its start, though, so a card
 * addressed to the session itself can arrive while the ready or that read is still on its way:
 * the host holds every card until that read has answered — Oh My Pi's take from the registration
 * on, until `afterReadyRead` ends its hold; the Claude bridge until the ready line
 * `afterReadyRead` writes — so none starts a turn ahead of a task the ready decision wrote. A
 * value written later — the resume the daemon's outbox queues after the ready, a task queued
 * later still — reaches the session as the listener relays it, and a card that arrives before it
 * may take a turn first, which the take then waits behind. A read that fails is logged and the
 * boot goes on, its hold ended all the same: the daemon's next write of the value reaches the
 * session as well. Nothing of a message is held, delayed, reordered or read here; the host does
 * that. A regain reads the value rather than sending the ready again, since a ready makes the
 * daemon send nothing.
 *
 * The ready carries no capability report. The session measures its capability rows later, on the
 * interval it runs while it is idle (`packages/pi-legion/src/capability-schedule.ts`).
 *
 * A registration the daemon refuses (a 4xx: a spent boot token, another contract) and any failure
 * after a registration but the read after the ready — the host's `afterRegister`, the ready, the
 * role claim, the role claim's refusal among them — are logged and `stop` the agent, since none of
 * them changes on retry; a relaunched process reads its claim's value at its own
 * registration. Any other refusal of the registration (a 5xx) is logged and rejects, as a request
 * that never reached the daemon does unlogged: both leave the agent to the daemon's registration
 * deadline.
 *
 * A role claim the listener answered 503 is no refusal (`listenerAskedToClaimAgain`): the listener
 * wrote no claim - every try of its write met a conflict, or the takeover record a claim writes
 * first, for the holder it takes the role from, did not store - so the claim is made again after a
 * wait that starts at `roleClaimRetryFirstDelayMs` and doubles (`claimRoleAgainOnBusy`), with one
 * log line per wait, until it lands or is refused, and no more once the host's `signal` aborts,
 * which stops nothing either: the host is already going. The session stays up meanwhile, holding
 * the value it read and its take's mark: the role's messages wait for it with the holder the claim
 * would have replaced or on the role's kept lane, and the ones that holder did not read are handed
 * over with that lane when the claim lands, so they wait behind the task's turn as every card
 * does. The task value is no role message: the listener relays it on the session's own subject
 * from the task bucket, so that handover neither replays it nor drops it.
 */
export async function bootLegionClaim(host: LegionClaimHost): Promise<LegionClaimBoot | undefined> {
  const { daemon } = host;
  let claim: LegionRegistration;
  try {
    claim = await daemon.register(host.registration);
  } catch (error) {
    if (!(error instanceof LegionDaemonApiError)) throw error;
    host.log(`claims/register registration failed (${error.status}): ${error.detail}`);
    if (error.status < 400 || error.status >= 500) throw error;
    host.stop();
    return undefined;
  }
  const { taskValue } = host;
  // The claim in the agent form its ready and its task reads speak for.
  const caller = claimCaller(claim, host.registration.sessionId);
  // The claim's value read now and handed to the host; whether the read answered.
  const readTask = async (why: string): Promise<boolean> => {
    try {
      taskValue(await daemon.task(caller));
      return true;
    } catch (error) {
      host.log(
        `claims/task ${why} failed; the claim's value reaches this session with the daemon's next write of it: ${messageFor(error)}`
      );
      return false;
    }
  };
  try {
    await host.afterRegister?.(claim);
    host.setCapability({
      sessionID: host.registration.sessionId,
      tree: claim.tree,
      issue: claim.issue,
      role: claim.role,
      secret: claim.secret,
      claimToken: claim.claimToken,
      generation: claim.generation,
      promptAgents: claim.promptAgents,
    });
    await callReadyWithRetry("claims/ready", () => daemon.ready(caller));
    await readTask("after claims/ready");
    host.afterReadyRead();
  } catch (error) {
    stopAfterRegistration(host, claim.claimToken, error);
    return undefined;
  }
  try {
    await claimRoleAgainOnBusy(() => host.claimRole(claim.claimToken), claim.claimToken, {
      log: host.log,
      firstDelayMs: host.roleClaimRetryFirstDelayMs,
      signal: host.signal,
    });
  } catch (error) {
    if (!host.signal?.aborted) stopAfterRegistration(host, claim.claimToken, error);
    return undefined;
  }
  host.onRoleRegained(async (role, reason) => {
    if (role !== claim.claimToken) return;
    if (await readTask(`after role ${role} was ${reason}`)) {
      host.log(`read claims/task after role ${role} was ${reason}`);
    }
  });
  return {
    readTask: async () => {
      await readTask("at the relay's new epoch");
    },
  };
}

/** The one path every failure after a registration takes — the host's `afterRegister`, the ready
 * and the role claim, a refused claim of a pane relaunched with `--resume` among them: it reports
 * the failure in one sentence and stops the session, since none of them changes on retry. The
 * read after the ready never takes it: a failed read is logged and the boot goes on. */
function stopAfterRegistration(host: LegionClaimHost, role: string, error: unknown): void {
  host.log(
    `claim boot failed after claims/register registered ${role}; ${host.stopping}: ${messageFor(error)}`
  );
  host.stop();
}

/** The first wait before a claim the listener answered 503 is made again, and the longest. */
const ROLE_CLAIM_RETRY_FIRST_DELAY_MS = 1_000;
const ROLE_CLAIM_RETRY_MAX_DELAY_MS = 30_000;

/**
 * Makes `claim`, a claim of Envoy role `role`, and makes it again while the listener answers it
 * 503 (`listenerAskedToClaimAgain`): the listener wrote no claim - every try of its write met a
 * conflict, or the takeover record a claim writes first did not store - and asks for the claim
 * again. The wait starts at `firstDelayMs` (`ROLE_CLAIM_RETRY_FIRST_DELAY_MS` unset) and doubles up
 * to `ROLE_CLAIM_RETRY_MAX_DELAY_MS`, with one `log` line per wait naming the attempt the listener
 * answered and its answer; the timer is unref'd, so it holds no process open. Any other failure is
 * thrown at once, for the caller to stop on. An aborted `signal` - the host stopping - ends the
 * waiting with the last 503 thrown and claims no more, so no claim after a stop makes a session the
 * host ended live again. With `maxAttempts`, the 503 of the last attempt is thrown, for a claim a
 * person is waiting on to fail where they see it.
 */
export async function claimRoleAgainOnBusy(
  claim: () => Promise<void>,
  role: string,
  options: {
    readonly log: (line: string) => void;
    readonly firstDelayMs?: number | undefined;
    readonly signal?: AbortSignal | undefined;
    /** The most claims made, the first included; the 503 of the last is thrown. Unset, no bound. */
    readonly maxAttempts?: number | undefined;
  }
): Promise<void> {
  let wait = options.firstDelayMs ?? ROLE_CLAIM_RETRY_FIRST_DELAY_MS;
  for (let attempt = 1; ; attempt++) {
    try {
      await claim();
      return;
    } catch (error) {
      if (!listenerAskedToClaimAgain(error) || options.signal?.aborted) throw error;
      if (options.maxAttempts !== undefined && attempt >= options.maxAttempts) throw error;
      options.log(
        `claim of Envoy role ${role} was answered 503 at attempt ${attempt}, so nothing was claimed; claiming it again in ${wait} ms: ${messageFor(error)}`
      );
      const waited = Promise.withResolvers<void>();
      const timer = setTimeout(waited.resolve, wait);
      timer.unref?.();
      const abort = (): void => {
        clearTimeout(timer);
        waited.resolve();
      };
      options.signal?.addEventListener("abort", abort, { once: true });
      await waited.promise;
      options.signal?.removeEventListener("abort", abort);
      if (options.signal?.aborted) throw error;
      wait = Math.min(wait * 2, ROLE_CLAIM_RETRY_MAX_DELAY_MS);
    }
  }
}

/**
 * Whether `error` is the listener's 503 to a role claim: an `EnvoyApiError` whose status is 503.
 * It is read by its name and status, not by class, because it crosses plugin bundles: pi-envoy
 * throws it from its own copy of envoy-client, behind the role-claim bridge, and pi-legion runs
 * this boot from another copy.
 */
export function listenerAskedToClaimAgain(error: unknown): boolean {
  if (!(error instanceof Error) || error.name !== "EnvoyApiError" || !("details" in error)) {
    return false;
  }
  const { details } = error;
  return (
    typeof details === "object" && details !== null && "status" in details && details.status === 503
  );
}
