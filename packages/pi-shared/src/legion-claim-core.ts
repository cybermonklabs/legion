import type { LegionRegistration } from "@legion/contracts/legion-api";
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
 * hears a regain, records the capability, logs and stops. */
export interface LegionClaimHost {
  readonly daemon: LegionDaemonClient;
  /** `claims/register`'s body: the pane's boot token, the session, its transcript, and the
   * contract this build speaks (`pluginContract`, the package's `legion.daemonApiVersion`). */
  readonly registration: RegisterInput;
  /** Runs once the registration answered, before the capability is recorded. */
  readonly afterRegister?: (claim: LegionRegistration) => Promise<void>;
  /** Records the registered claim. It is recorded before `claims/ready`, the call the deferred
   * role claim now waits behind: the daemon's task can arrive the moment ready answers, before
   * this session holds its Envoy name, and what serves it reads the capability then. */
  readonly setCapability: (capability: ClaimCapability) => void;
  /** Claims `role`, the claim token, on Envoy for this session: the topic the daemon sends the
   * claim's task and every notice on. It is the boot's last step and runs after `claims/ready`
   * has answered, at the session's first turn (`onFirstTurn`) or at the bound, whichever comes
   * first. Throws when the role cannot be held, which is logged and stops the session. */
  readonly claimRole: (role: string) => Promise<void>;
  /** Registers `claim`, the step that takes this session's Envoy name, to be run once the
   * session's first turn has started — whatever started it: the daemon's task, a card, a
   * reminder. Calling it more than once claims once, and a signal the host swallows leaves the
   * name to the bound.
   *
   * Which turn is its session's first is the host's to decide, and the two hosts decide it
   * differently because their startups differ. A host whose own startup can begin no turn
   * before the boot has registered may call `claim` at once, inside this very call, for a turn
   * that has already started: pi-legion awaits the boot inside `session_start`, and Oh My Pi
   * reads no input until every such handler has returned, so a turn it heard of is this
   * session's own. A host whose startup seeds a turn of its own before the claim exists counts
   * only a turn that arrives after this registration: in claude-envoy the bridge's seeding
   * `/rename` is what writes the transcript the boot waits for, so a signal kept from before
   * the registration would take the name ahead of the daemon's task — the very ordering the
   * deferred claim exists to end.
   *
   * Both hosts have such a signal — Oh My Pi's `agent_start` and the Claude Code hook's
   * `UserPromptSubmit` — so it is required: a boot with no way of hearing its first turn is not
   * a state this interface can be in. */
  readonly onFirstTurn: (claim: () => void) => void;
  /** How long the deferred claim waits for `onFirstTurn` before it claims the name itself: the
   * session's own `claimFirstTurnBoundMs` (`src/claim-first-turn-bound.ts`), which every host
   * reads from its own environment and which holds the default. */
  readonly firstTurnBoundMs: number;
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

/** What the boot answers: the claim step it left to run after it, for a caller that waits on it
 * and for a test that reads when the name was taken. */
export interface LegionClaimBoot {
  /** Settles once that step has run — the role claimed, or the refusal logged and the session
   * stopped — and at once for a boot that stopped before it ever reached the step. It never
   * rejects: a claim the listener refuses is the host's `stop`, not this promise's failure. */
  readonly claimed: Promise<void>;
}

/**
 * Boots a claim on `/legion/v1/claims/*`, the same on every harness: `claims/register`;
 * `afterRegister`; the capability recorded; `claims/ready`, retried on a 5xx or transport failure
 * (`callReadyWithRetry`); the regain listener, which re-runs that same ready each time the
 * heartbeat regains the role; and then, after this function has settled, the Envoy role claimed
 * at the session's first turn (`onFirstTurn`) or at the bound, whichever comes first.
 *
 * The name is taken last because the Envoy listener keeps a message published to a role no
 * session holds and hands the whole kept lane over the moment the role is claimed: claimed before
 * the daemon's task, that lane takes the session's first turn and the task waits for the turn to
 * end; claimed at the first turn, it arrives inside a turn that is already running. Nothing of a
 * message is held, delayed, reordered or read here — only when this session answers to its name.
 *
 * The claim step is out of this function, and out of its one `try`, because a host awaits the
 * whole boot where no turn of its session can begin: pi-legion awaits it inside its
 * `session_start` handler, and Oh My Pi reads no input until every such handler has returned, so
 * a boot that waited there for a first turn would deadlock. `bootLegionClaim` settles once ready
 * has answered, with the claim still to come, and what it answers carries that step's own promise
 * ({@link LegionClaimBoot.claimed}).
 *
 * The ready carries no capability report. The session measures its capability rows later, on the
 * interval it runs while it is idle (`packages/pi-legion/src/capability-schedule.ts`).
 *
 * A registration the daemon refuses (a 4xx: a spent boot token, another contract) and any failure
 * after a registration are logged and `stop` the agent, since neither changes on retry. Any other
 * refusal of the registration (a 5xx) is logged and rejects, as a request that never reached the
 * daemon does unlogged: both leave the agent to the daemon's registration deadline. A claim the
 * listener refuses is logged and stops the agent in exactly the same words, now after the daemon
 * may have delivered a task: the relaunch re-sends a delivery that is pending and unconfirmed,
 * the path a death inside a turn already takes.
 */
export async function bootLegionClaim(host: LegionClaimHost): Promise<LegionClaimBoot> {
  const { daemon } = host;
  let claim: LegionRegistration;
  try {
    claim = await daemon.register(host.registration);
  } catch (error) {
    if (!(error instanceof LegionDaemonApiError)) throw error;
    host.log(`claims/register registration failed (${error.status}): ${error.detail}`);
    if (error.status < 400 || error.status >= 500) throw error;
    host.stop();
    return { claimed: Promise.resolve() };
  }
  try {
    await host.afterRegister?.(claim);
    host.setCapability({
      sessionID: host.registration.sessionId,
      claimToken: claim.claimToken,
      generation: claim.generation,
      tree: claim.tree,
      issue: claim.issue,
      role: claim.role,
      secret: claim.secret,
      promptAgents: claim.promptAgents,
    });
    const ready: ReadyInput = {
      claimToken: claim.claimToken,
      sessionId: host.registration.sessionId,
      secret: claim.secret,
      generation: claim.generation,
    };
    await callReadyWithRetry("claims/ready", () => daemon.ready(ready));
    host.onRoleRegained(async (role, reason) => {
      if (role !== claim.claimToken) return;
      try {
        await callReadyWithRetry("claims/ready after role regain", () => daemon.ready(ready));
        host.log(`re-ran claims/ready after role ${role} was ${reason}`);
      } catch (error) {
        host.log(
          `claims/ready after role ${role} was ${reason} failed; the daemon's queued task stays undelivered until the next regain or boot: ${messageFor(error)}`
        );
      }
    });
  } catch (error) {
    stopAfterRegistration(host, claim.claimToken, error);
    return { claimed: Promise.resolve() };
  }
  return { claimed: claimRoleAtFirstTurn(host, claim.claimToken) };
}

/** The one path every failure after a registration takes, wherever it happens: the boot's own
 * steps and the deferred role claim, which runs after the boot has settled, report it in the
 * same sentence and stop the session, since neither changes on retry. */
function stopAfterRegistration(host: LegionClaimHost, role: string, error: unknown): void {
  host.log(
    `claim boot failed after claims/register registered ${role}; ${host.stopping}: ${messageFor(error)}`
  );
  host.stop();
}

/**
 * The boot's last step, run after the boot itself has settled: `role` claimed on Envoy at the
 * host's first-turn signal or at the bound, whichever comes first, and once however many of the
 * two arrive. The timer is armed and the signal registered in that order and both before this
 * returns, so a host whose turn has already started claims inside its own `onFirstTurn` call and
 * loses nothing, and the timer is unreferenced where the runtime offers it: the bound is a
 * backstop and keeps no process alive for its own sake.
 *
 * A refusal is logged and stops the session, the one failure path every other failure after a
 * registration takes. The promise answers when the step has run, either way, and never rejects.
 */
function claimRoleAtFirstTurn(host: LegionClaimHost, role: string): Promise<void> {
  const done = Promise.withResolvers<void>();
  let ran = false;
  const claim = (): void => {
    if (ran) return;
    ran = true;
    clearTimeout(bound);
    void (async () => {
      try {
        await host.claimRole(role);
      } catch (error) {
        stopAfterRegistration(host, role, error);
      }
      done.resolve();
    })();
  };
  const bound = setTimeout(claim, host.firstTurnBoundMs);
  bound.unref?.();
  host.onFirstTurn(claim);
  return done.promise;
}
