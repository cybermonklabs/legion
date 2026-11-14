import type { LegionRegistration, LegionTask } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  callReadyWithRetry,
  LegionDaemonApiError,
  type LegionDaemonClient,
  type ReadyInput,
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
   * pending stands before the role's kept lane is handed over at the role claim. */
  readonly afterRegister?: (claim: LegionRegistration) => Promise<void>;
  /** Records the registered claim, before `claims/ready`: what serves the claim's task reads the
   * capability, and a take is answered only once the ready is in. */
  readonly setCapability: (capability: ClaimCapability) => void;
  /** Claims `role`, the claim token, on Envoy for this session: the topic the listener relays the
   * claim's task value and every notice on. It runs right after `claims/ready` has answered
   * (decision 15 of INFRA-2811's plan): the task is a value the session takes once its host is
   * idle, so nothing the role's kept lane hands over can take the task's turn, and the session
   * holds its name from its ready on. Throws when the role cannot be held, which is logged and
   * stops the session. */
  readonly claimRole: (role: string) => Promise<void>;
  /**
   * Takes the claim's task value as the daemon reads it now (`claims/task`): at each regain of
   * the role, and at each read the host asks for (`LegionClaimBoot.readTask`) when its relay
   * begins a new epoch — a value the listener relayed while the session did not hold its role,
   * or was not subscribed, is lost, so it is read rather than waited for. A host that takes no
   * task value leaves it out, and nothing is read.
   */
  readonly taskValue?: (reading: LegionTask) => void;
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
 * `claims/ready`, retried on a 5xx or transport failure (`callReadyWithRetry`); the Envoy role
 * claimed right after it; and the regain listener, which reads the claim's task value again each
 * time the heartbeat regains the role. It answers the claim it booted, through which the host
 * reads that value again at a new epoch of its relay, or undefined for a boot that stopped.
 *
 * The daemon pushes no task: it writes the number of the task a claim is owed as the claim's
 * value, which the listener relays to the session holding the role, and the session takes it once
 * its host is idle (`src/task-take.ts`). So the role is claimed as soon as the ready has answered,
 * and the lane the listener kept for the role arrives while the take's mark holds its cards back
 * (`packages/pi-envoy/src/card-wake.ts`); nothing of a message is held, delayed, reordered or read
 * here. A regain reads the value rather than sending the ready again, since a ready makes the
 * daemon send nothing.
 *
 * The ready carries no capability report. The session measures its capability rows later, on the
 * interval it runs while it is idle (`packages/pi-legion/src/capability-schedule.ts`).
 *
 * A registration the daemon refuses (a 4xx: a spent boot token, another contract) and any failure
 * after a registration, the role claim's refusal among them, are logged and `stop` the agent,
 * since neither changes on retry; a relaunched process reads its claim's value at its own
 * registration. Any other refusal of the registration (a 5xx) is logged and rejects, as a request
 * that never reached the daemon does unlogged: both leave the agent to the daemon's registration
 * deadline.
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
  // The claim's value read now and handed to the host; whether the read answered.
  const readTask = async (why: string): Promise<boolean> => {
    if (taskValue === undefined) return false;
    try {
      taskValue(
        await daemon.task({
          claimToken: claim.claimToken,
          sessionId: host.registration.sessionId,
          secret: claim.secret,
          generation: claim.generation,
        })
      );
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
    const ready: ReadyInput = {
      claimToken: claim.claimToken,
      sessionId: host.registration.sessionId,
      secret: claim.secret,
      generation: claim.generation,
    };
    await callReadyWithRetry("claims/ready", () => daemon.ready(ready));
    await host.claimRole(claim.claimToken);
  } catch (error) {
    host.log(
      `claim boot failed after claims/register registered ${claim.claimToken}; ${host.stopping}: ${messageFor(error)}`
    );
    host.stop();
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
