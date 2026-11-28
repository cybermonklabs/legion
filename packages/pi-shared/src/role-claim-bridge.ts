import { envoyPluginInterface } from "./interface";
import type { SessionContext } from "./pi-types";

/**
 * The process-wide role-claim bridge between the Envoy and Legion plugins. The Envoy entry
 * (`extensions/envoy.ts`) registers each bound instance's claim entry point and fires the regain
 * hook from its heartbeat; the Legion entry (`extensions/legion.ts`, `controller-session.ts`)
 * claims through it and registers that hook. The bridge object itself is the `roleClaim` member of
 * the versioned interface (`interface.ts`), which both plugins reach through `globalThis`.
 *
 * `replaces` is the session this session replaces, as the daemon's registration names it: the
 * claim sends it as its `previous_session_id`, so the listener hands that session's unread direct
 * and role messages to this one once the claim lands. Undefined or empty, the claim names none.
 */
export type LegionRoleClaim = (
  sessionID: string,
  role: string,
  context?: SessionContext,
  replaces?: string
) => Promise<void>;

/**
 * `whileHolding` names a role this session holds: the subscription then lasts as long as the
 * session holds that role and closes when it stops holding it: another live session takes it, a
 * `new` or `resume` switch leaves it behind, a `/fork`, `/branch`, `/handoff` or tree navigation
 * reclaims the role its transcript records when that is another one, or a claim through this
 * bridge moves the session to another role. The model's `envoy_role_set`
 * refuses to move the session off such a role, and its `envoy_unsubscribe` keeps such a
 * subscription open and reports it kept. It is never registered with the listener, so a resumed
 * process gets it back only by claiming the role again.
 */
export type LegionNoticeSubscription = (
  sessionID: string,
  topic: string,
  context?: SessionContext,
  whileHolding?: string
) => Promise<void>;

/**
 * Why the heartbeat decided the listener had lost sight of this session's role: `"reclaimed"` —
 * the listener no longer named this session and a soft claim landed; `"reregistered"` — the
 * claim itself survived, but this session had been unreachable (a registry outage), so the
 * listener may have answered "no holder" for it meanwhile.
 */
export type RoleRegainReason = "reclaimed" | "reregistered";

type LegionRoleRegained = (role: string, reason: RoleRegainReason) => Promise<void>;

/**
 * One bound `envoyExtension(pi)` instance: its claim entry point and a live read of the session
 * id it currently serves. OMP re-binds every extension factory for each in-process `task`
 * subagent, so a process holds one instance per live agent session, and a claim for the pane's
 * session must reach the pane's instance — the one whose heartbeat re-asserts the role — not
 * whichever instance bound last (a subagent's, whose own heartbeat would then see the pane's
 * id as drift and hand the role to the subagent's session). While the pane is live, `sessionID`
 * reads the pane's live `SessionManager`, not this instance's own module state: OMP mutates the
 * manager before it dispatches a `session_switch`, so the key is already current when
 * legion.ts's handler for that event claims — whichever of the two extensions OMP dispatches
 * first. The module variable lags until this instance's own rebind runs, and keying on it would
 * make the routing depend on handler order. Once an instance's stop has ended, the pane's manager
 * is gone, and `sessionID` answers the id the instance last served, so its own session's claims
 * still reach it.
 */
export type LegionRoleClaimInstance = {
  readonly claim: LegionRoleClaim;
  readonly subscribe: LegionNoticeSubscription;
  readonly sessionID: () => string;
};

export type LegionRoleClaimBridge = {
  /**
   * Every bound instance in bind order. An instance whose session's stop has begun moves to the
   * front and stays: it refuses that session's claims and role-bound subscriptions naming the stop
   * (`RoleClaimStoppingError`) for the rest of the process. One that served no session, a `task`
   * subagent's, leaves as its stop ends.
   */
  readonly instances: LegionRoleClaimInstance[];
  /**
   * Sessions Legion drives: every id `claimEnvoyRole` has claimed a role for, in this process.
   * envoy.ts's run-end ask nudge skips them — a phase worker already answers to the design
   * gate and its architect, and a reminder to open an ask is noise in a pane nobody watches.
   */
  readonly managedSessions: Set<string>;
  /**
   * legion.ts's regain hook. One slot — unlike `instances` — because several legion.ts
   * instances share a process (OMP re-binds every extension factory for each in-process `task`
   * subagent) but only an instance that has established a Legion identity registers here, so
   * the slot always holds the identity-bearing instance's listener.
   */
  regained: LegionRoleRegained | undefined;
  /**
   * Whether this process is the daemon-launched controller, whose Slack subscriptions belong to
   * the controller role: the Legion entry's factory sets it from its environment, in that process
   * alone, so it is the same in every instance; false in any other process. While it is true, the
   * Envoy entry restores a resumed session's registered interests after unsubscribing every Slack
   * topic among them (`slackTopicChannel`) from the listener and dropping it: a restored topic
   * would be a plain subscription, which outlives the controller role, and the controller hears
   * its listed channels only through the subscriptions its role owns, which it never registers.
   */
  slackRoleOwned: boolean;
  /**
   * Whether this process is a Legion controller, of either launch form, whose watched workflows'
   * default-branch topics (`isGithubDefaultBranchWorkflowSubject` of `@legion/contracts`) belong
   * to the controller role: the Legion entry's factory sets it from its environment, in that
   * process alone, so it is the same in every instance; false in any other process. While it is
   * true, the Envoy entry restores a resumed session's registered interests after unsubscribing
   * every such topic among them from the listener and dropping it, in the same removal as the
   * Slack topics `slackRoleOwned` drops: an edge listener judges the GitHub topics of the
   * session's stored row at each registration, so a restored one would refuse the role claim and
   * every other topic while the edge cannot read the project's repositories, and the controller
   * hears its watched workflows only through the subscriptions its role owns, which it never
   * registers.
   */
  watchedWorkflowsRoleOwned: boolean;
};

/**
 * Why a subscription that lasts while the session holds a role (`LegionNoticeSubscription`'s
 * `whileHolding`) is refused when the session does not hold that role: the Envoy entry throws it,
 * and the Legion controller reads it (`refusedForRoleNotHeld`) to stop subscribing its role's
 * topics with one log line rather than giving each of them up.
 */
export class RoleNotHeldError extends Error {
  override readonly name = "RoleNotHeldError";

  constructor(sessionID: string, role: string) {
    super(`Envoy session ${sessionID} does not hold role ${role} for a notice subscription`);
  }
}

/**
 * Whether `error` is a subscription refused because the session does not hold the role it names
 * (`RoleNotHeldError`). It is read by its name, not by class, because it crosses plugin bundles:
 * the Envoy entry throws it from its own copy of this module, and the Legion entry reads it from
 * another (`listenerAskedToClaimAgain` in `./legion-claim-core`).
 */
export function refusedForRoleNotHeld(error: unknown): boolean {
  return error instanceof Error && error.name === "RoleNotHeldError";
}

/** The bridge, read from the interface at each call: a consumer never keeps the object, so the
 * publish order of the two plugins is irrelevant. */
export function legionRoleClaimBridge(): LegionRoleClaimBridge {
  return envoyPluginInterface().roleClaim;
}

/**
 * The envoy instance currently serving `sessionID`: the instance whose live `SessionManager`
 * reports `sessionID`. While the pane is live the key is the manager's id, not the instance's own
 * module state, so a call made from legion.ts's `session_switch` handler routes correctly whether
 * OMP dispatched envoy.ts's rebind for the same event before or after it. With several instances
 * on the same id (a fixture that binds one per test) the one later in the list wins. A target no
 * instance serves yet falls back to the last instance in the list, which establishes the session
 * itself; a stopped instance moves to the front, so the fallback is a live one where there is
 * one. A session whose stop has begun keeps its instance on the bridge, which refuses the call
 * naming the stop, so no other instance gets it. With no instance bound, it throws naming `what`.
 */
function boundInstance(sessionID: string, what: string): LegionRoleClaimInstance {
  const { instances } = legionRoleClaimBridge();
  const instance =
    instances.findLast((candidate) => candidate.sessionID() === sessionID) ?? instances.at(-1);
  if (instance === undefined) throw new Error(`Envoy has no bound instance for ${what}`);
  return instance;
}

/**
 * Claims `role` for `sessionID` through the envoy instance currently serving that session
 * (`boundInstance`), so its heartbeat is the one that re-asserts the claim afterwards. `replaces`
 * is the session this one replaces, which the claim names as its `previous_session_id`
 * (`LegionRoleClaim`).
 */
export async function claimEnvoyRole(
  sessionID: string,
  role: string,
  context?: SessionContext,
  replaces?: string
): Promise<void> {
  legionRoleClaimBridge().managedSessions.add(sessionID);
  await boundInstance(sessionID, "a role claim").claim(sessionID, role, context, replaces);
}

/**
 * Adds a persisted direct subscription through the Envoy adapter that owns delivery and recovery,
 * the instance serving that session (`boundInstance`); with `whileHolding`, only for as long as
 * this session holds that role.
 */
export async function subscribeLegionNotice(
  sessionID: string,
  topic: string,
  context?: SessionContext,
  whileHolding?: string
): Promise<void> {
  await boundInstance(sessionID, "a notice subscription").subscribe(
    sessionID,
    topic,
    context,
    whileHolding
  );
}

/**
 * Registers the hook the heartbeat fires after it re-establishes this session as `role`'s live
 * holder (see `reassertRole`). legion.ts re-runs the role's daemon ready call from it. Last
 * registration wins — the bridge holds one `regained` slot, unlike its per-instance claim list —
 * so legion.ts calls this only from the paths that establish a Legion identity, never at
 * extension setup, or a `task` subagent's identity-less instance would replace the holder's
 * listener.
 */
export function onEnvoyRoleRegained(
  listener: (role: string, reason: RoleRegainReason) => Promise<void>
): void {
  legionRoleClaimBridge().regained = listener;
}
