import type { LegionRegistration } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  BoundedStepFailure,
  boundedStep,
  CLAIM_BOOT_ATTEMPT_TIMEOUT_MS,
  CLAIM_BOOT_BUDGET_MS,
  type ClaimBootBounds,
  callReadyWithRetry,
  callWithinClaimBootBudget,
  claimBootBudget,
  type LegionDaemonClient,
  type ReadyInput,
  type RegisterInput,
  type StepBound,
} from "./daemon-client";
import type { ClaimCapability } from "./legion-phase-ops";

/** Who the process booting a claim is, for the one line a boot that registers no claim writes.
 * None of it is on `RegisterInput`, which names a boot token and a session and no claim, so each
 * host fills it from its own environment (`LEGION_ROLE`, `LEGION_ISSUE`, `LEGION_TREE`, and the
 * daemon's address, `LEGION_DAEMON_URL`). A value its environment does not carry, or carries
 * blank, is written `(unset)` in the line rather than left out, so an operator reads which of
 * the four the launch was missing. */
export interface LegionClaimIdentity {
  /** Each is `undefined` where the host's environment did not carry it: a host hands the value
   * it read on, undefined and all, rather than deciding what an absent one reads as. */
  readonly role?: string | undefined;
  readonly issue?: string | undefined;
  readonly tree?: string | undefined;
  readonly daemonUrl: string;
}

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
   * first — at once where the turn started while ready was still being retried. Throws when the
   * role cannot be held, which is logged and stops the session. */
  readonly claimRole: (role: string) => Promise<void>;
  /** Hands the host `started`, the signal to call once the session's first turn has started —
   * whatever started it: the daemon's task, a card, a reminder. It is called as soon as
   * `claims/register` has answered, before `claims/ready`, so a turn that starts while that
   * ready is still being retried is heard rather than lost: the boot remembers it and takes the
   * name the moment ready answers. Calling `started` more than once takes the name once, and a
   * signal the host swallows leaves the name to the bound.
   *
   * Which turn is its session's first is the host's to decide, and the two hosts decide it
   * differently because their startups differ. A host whose own startup can begin no turn
   * before the boot has registered may call `started` at once, inside this very call, for a turn
   * that has already started: pi-legion awaits the boot inside `session_start`, and Oh My Pi
   * reads no input until every such handler has returned, so a turn it heard of is this
   * session's own. A host whose startup seeds a turn of its own counts only a turn that arrives
   * after this call: in claude-envoy the bridge's seeding `/rename` is what writes the
   * transcript the boot waits for, so it is over before `claims/register` is even sent, and a
   * signal kept from before this call would take the name ahead of the daemon's task — the very
   * ordering the deferred claim exists to end.
   *
   * Both hosts have such a signal — Oh My Pi's `agent_start` and the Claude Code hook's
   * `UserPromptSubmit` — so it is required: a boot with no way of hearing its first turn is not
   * a state this interface can be in. */
  readonly onFirstTurn: (started: () => void) => void;
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
  /** Who this process is and which daemon it calls, for the one line every ending that is not a
   * registered claim writes ({@link LegionClaimIdentity}). */
  readonly identity: LegionClaimIdentity;
  /** The boot's two bounds: the bound of one daemon attempt and the boot's whole wall-clock
   * budget. Production passes none and takes `CLAIM_BOOT_ATTEMPT_TIMEOUT_MS` and
   * `CLAIM_BOOT_BUDGET_MS` (`src/daemon-client.ts`); a test passes its own, since no test can
   * wait out the production budget. */
  readonly bounds?: ClaimBootBounds;
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
 * Boots a claim on `/legion/v1/claims/*`, the same on every harness: `claims/register`; the
 * host's first-turn signal registered (`onFirstTurn`), which is why a turn that starts while the
 * ready below is still being retried is not lost; `afterRegister`; the capability recorded;
 * `claims/ready`; the regain listener, which re-runs that same ready each time the heartbeat
 * regains the role; and then, after this function has settled, the Envoy role claimed at the
 * first turn the signal reported or at the bound, whichever comes first.
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
 * **Every ending that is not a registered claim writes one line and ends the agent**, and
 * nothing throws out of this function: a refusal of any status, a request that never reached the
 * daemon, a call that did not answer inside its bound, a spent budget, and a failure of any step
 * after the registration. A process that runs on without a registered claim is a role with no
 * `legion` tool, which can finish no phase and which nothing else notices
 * (dispatch://INFRA-2883); a process that ends is one the daemon launches again.
 *
 * Every daemon call runs through {@link callWithinClaimBootBudget}: each attempt under its own
 * bound, a retry while the daemon cannot answer (a transport failure, a timed-out attempt, a
 * 5xx), and every attempt out of one wall-clock budget that starts at the first call and covers
 * the whole of this function, `afterRegister` included — that hook spawns `jj config path
 * --user`, whose own 10 second bound (`JJ_CONFIG_PATH_TIMEOUT_MS`, `src/jj-attribution.ts`) sits
 * above what the boot can spare. A step that outlives the remaining budget ends the agent on the
 * same line. Only the claim step, which runs after this function has settled, is outside the
 * budget: it waits for the session's own first turn, which no bound of the boot's measures.
 */
export async function bootLegionClaim(host: LegionClaimHost): Promise<LegionClaimBoot> {
  const { daemon } = host;
  const budget = claimBootBudget(
    host.bounds ?? { attemptMs: CLAIM_BOOT_ATTEMPT_TIMEOUT_MS, budgetMs: CLAIM_BOOT_BUDGET_MS }
  );
  // The budget as the steps between the daemon calls run under it, and as a spent one names
  // itself in the boot's one line. The two daemon calls take no bound from the runner: each
  // spends this same budget inside `callWithinClaimBootBudget`, which says so in words of its
  // own, so the runner only names the step that failed.
  const steps: StepBound = {
    remainingMs: budget.remainingMs,
    named: `the claim boot's budget of ${budget.budgetMs} ms`,
  };
  let claim: LegionRegistration;
  try {
    claim = await boundedStep(undefined, "claims/register", () =>
      callWithinClaimBootBudget(
        "claims/register",
        (signal) => daemon.register(host.registration, { signal }),
        budget
      )
    );
  } catch (error) {
    endBoot(host, error, undefined);
    return { claimed: Promise.resolve() };
  }
  // The signal is heard from here, the moment the registration answered and before the ready
  // below, so a turn that starts while that ready is still being retried takes the name as soon
  // as it answers rather than waiting out the bound. The claim step itself still runs after
  // ready (claimRoleAtFirstTurn).
  const firstTurn = listenForFirstTurn(host);
  try {
    await boundedStep(steps, "afterRegister", async () => {
      await host.afterRegister?.(claim);
    });
    await boundedStep(steps, "setCapability", () => {
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
    });
    const ready: ReadyInput = {
      claimToken: claim.claimToken,
      sessionId: host.registration.sessionId,
      secret: claim.secret,
      generation: claim.generation,
    };
    await boundedStep(undefined, "claims/ready", () =>
      callWithinClaimBootBudget("claims/ready", (signal) => daemon.ready(ready, { signal }), budget)
    );
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
    endBoot(host, error, claim.claimToken);
    return { claimed: Promise.resolve() };
  }
  return { claimed: claimRoleAtFirstTurn(host, claim.claimToken, firstTurn) };
}

/** The one line every ending that is not a registered claim writes: who this process is (the
 * role, the issue and the tree its host read from the environment), the daemon it called, the
 * call that failed and why, the claim where one was registered and that there is none where none
 * was, and what the process does now. It carries no boot token, no secret and no part of the
 * registration body: the line goes to a log an operator and a controller read.
 *
 * It is exported because a host has endings of its own before the boot is called — the Claude
 * Code host waits for its transcript first — and one sentence read in one place is what keeps
 * every ending of a claim that never registered readable as the same event. */
export function claimBootFailureLine(ending: {
  readonly identity: LegionClaimIdentity;
  /** What the host's `stop` does, as {@link LegionClaimHost.stopping} words it. */
  readonly stopping: string;
  /** The call or step that failed: a daemon route by its path, a host's step by its name. */
  readonly call: string;
  readonly cause: unknown;
  /** The claim the registration issued, and undefined where none had answered yet. */
  readonly claimToken?: string | undefined;
}): string {
  const claimed =
    ending.claimToken === undefined
      ? "no claim was registered"
      : `the claim ${ending.claimToken} was registered`;
  return `claim boot failed: ${claimIdentityWords(ending.identity)}; ${ending.call} failed: ${messageFor(ending.cause)}; ${claimed}; ${ending.stopping}`;
}

/** Who a Legion process is, as every line ending it names it: its role, issue and tree, and the
 * daemon it registers with, each `(unset)` where the environment did not carry it. One wording
 * for the claim boot's line and for the Oh My Pi entry's cover over the rest of its
 * `session_start` (`packages/pi-legion/extensions/legion.ts`), so an operator reading the pod's
 * log reads either ending of one role the same way (dispatch://INFRA-2883 R1). */
export function claimIdentityWords(identity: LegionClaimIdentity): string {
  const { role, issue, tree, daemonUrl } = identity;
  return `role ${told(role)}, issue ${told(issue)}, tree ${told(tree)}, daemon ${told(daemonUrl)}`;
}

/** A Legion process's identity as its own environment names it: `LEGION_ROLE`, `LEGION_ISSUE`,
 * `LEGION_TREE` and `LEGION_DAEMON_URL`, each handed on as read. */
export function claimIdentityOf(env: NodeJS.ProcessEnv): LegionClaimIdentity {
  // biome-ignore-start lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access
  return {
    role: env["LEGION_ROLE"],
    issue: env["LEGION_ISSUE"],
    tree: env["LEGION_TREE"],
    daemonUrl: env["LEGION_DAEMON_URL"] ?? "",
  };
  // biome-ignore-end lint/complexity/useLiteralKeys: see above
}

/** An identity value the host's environment did not carry, as the line shows it. */
function told(value: string | undefined): string {
  return value === undefined || value.trim() === "" ? "(unset)" : value;
}

/** The one ending every failure of the boot takes, wherever it happens: one line, then the
 * agent. `claimToken` is the claim where the registration had answered, and undefined where it
 * had not. The deferred role claim, which runs after the boot has settled, ends the same way,
 * since a name this session cannot hold changes on no retry either. */
function endBoot(host: LegionClaimHost, error: unknown, claimToken: string | undefined): void {
  const failed = error instanceof BoundedStepFailure;
  host.log(
    claimBootFailureLine({
      identity: host.identity,
      stopping: host.stopping,
      call: failed ? error.step : "the claim boot",
      cause: failed ? error.failure : error,
      claimToken,
    })
  );
  host.stop();
}

/**
 * Hears the host's first turn from the moment the registration answered, and answers the way the
 * claim step hands itself over: the step runs at once for a turn that has already started, and
 * at the signal otherwise.
 *
 * It exists because the claim step comes after `claims/ready`, and that ready is retried (a 5xx,
 * a transport failure). The daemon's task can reach the session while those retries run — the
 * daemon took the first ready and its answer was lost — so the turn it starts would otherwise
 * find no step to run and the name would be taken only at the bound (INFRA-1573). A host may
 * report the same turn more than once — pi-legion reports one it kept from before this call and
 * reports every later `agent_start` too — and each report after the first runs a step that has
 * already run, which does nothing.
 */
function listenForFirstTurn(host: LegionClaimHost): (claim: () => void) => void {
  let started = false;
  let claim: (() => void) | undefined;
  host.onFirstTurn(() => {
    started = true;
    claim?.();
  });
  return (step) => {
    claim = step;
    if (started) step();
  };
}

/**
 * The boot's last step, run after the boot itself has settled: `role` claimed on Envoy at the
 * first turn `firstTurn` reports or at the bound, whichever comes first, and once however many
 * of the two arrive. The timer is armed and the step handed over in that order and both before
 * this returns, so a turn that started while the boot was still readying claims inside that
 * hand-over and loses nothing, and the timer is unreferenced where the runtime offers it: the
 * bound is a backstop and keeps no process alive for its own sake.
 *
 * A refusal is logged and stops the session, the one ending every failure of the boot takes
 * (`endBoot`, with the claim it registered). It runs outside the boot's budget: this step waits
 * for the session's own first turn, which no bound of the boot's measures. The promise answers
 * when the step has run, either way, and never rejects.
 */
function claimRoleAtFirstTurn(
  host: LegionClaimHost,
  role: string,
  firstTurn: (claim: () => void) => void
): Promise<void> {
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
        endBoot(host, new BoundedStepFailure("claimRole", error), role);
      }
      done.resolve();
    })();
  };
  const bound = setTimeout(claim, host.firstTurnBoundMs);
  bound.unref?.();
  firstTurn(claim);
  return done.promise;
}
