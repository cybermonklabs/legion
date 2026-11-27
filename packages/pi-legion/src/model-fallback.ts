import { messageFor } from "@legion/envoy-client/errors";
import {
  type FallbackChainContext,
  type FallbackChainLog,
  modelSelector,
} from "@legion/pi-shared/fallback-chain";
import {
  type FallbackCooldownInput,
  type FallbackCooldownLimits,
  type FallbackCooldownState,
  restoreDue,
  stepFallbackCooldown,
} from "@legion/pi-shared/fallback-cooldown";
import type {
  AgentEndEvent,
  HostModel,
  ModelChangeEntry,
  RetryFallbackAppliedEvent,
  SessionContext,
} from "@legion/pi-shared/pi-types";
import { classifyProviderError, classifyTurnEnd } from "@legion/pi-shared/provider-refusal";
import { turnGoesOn } from "./turn-end";

/**
 * The session's model across a provider's refusal (INFRA-956 R1): a content filter that refuses a
 * Legion role's turn sends the next request to the next model of Oh My Pi's fallback chain, in
 * the same session, and the session comes back to its own model after a cooldown that grows each
 * time that model is refused again.
 *
 * Every collaborator is a field of `ModelFallbackDeps`, the shape
 * `packages/pi-shared/src/phase-reminder.ts` has: this module imports nothing of Oh My Pi, so a
 * unit test drives it with no host, and `packages/pi-legion/extensions/legion.ts` is the only
 * file that knows what `pi` is. The waits are decided by the cooldown machine
 * (`@legion/pi-shared/fallback-cooldown`); this runner owns the host's events and the switch.
 *
 * Nothing here throws out of a host event handler: every failure is one `log.warn` and a return.
 */

/** The slice of the host's session context this runner reads; a `SessionContext`
 * (`@legion/pi-shared/pi-types`) is one, and a unit test builds one by hand. */
export interface ModelFallbackContext {
  readonly sessionManager: {
    readonly getBranch?: () => readonly unknown[];
    /** The session the context is on now: read on each side of a switch of the model, so a pane
     * that moved to another session in between is named. */
    readonly getSessionId?: () => string;
  };
  readonly models?: SessionContext["models"];
  readonly modelRegistry?: SessionContext["modelRegistry"];
  /** The host's `ctx.isIdle()`: false while a turn runs. The timer's return waits for it. */
  readonly isIdle?: () => boolean;
  /** The host's `ctx.hasPendingMessages()`: true while the host holds a message to steer or follow
   * the turn, which a continuation then reads its model for. The timer counts a pane that holds
   * one as busy even when it runs no turn, so it makes no return while the message is held. */
  readonly hasPendingMessages?: () => boolean;
}

export interface ModelFallbackDeps {
  /** `pi.setModel`: switches the session's model as `/model` does, and answers false when no
   * credential of this session serves it. */
  readonly setModel: (model: HostModel) => Promise<boolean>;
  /** Persists the state where a relaunch of this worker reads it again (the Oh My Pi transcript
   * entry `LEGION_FALLBACK_ENTRY`), on every change of it. */
  readonly saveState: (state: FallbackCooldownState) => void;
  /** Epoch ms. The cooldown machine reads no clock of its own, so every input carries this. */
  readonly now: () => number;
  readonly limits: FallbackCooldownLimits;
  /** `nextFallbackModel` (`@legion/pi-shared/fallback-chain`): the model below this one in the
   * deployment's own `retry.fallbackChains`. */
  readonly nextModel: (
    ctx: FallbackChainContext,
    model: HostModel
  ) => Promise<HostModel | undefined>;
  /** `fallbackModels` (`@legion/pi-shared/fallback-chain`): every model below this one in the same
   * chain, in its order, which the cyber-policy ladder searches for one of the same family. */
  readonly fallbackModels: (
    ctx: FallbackChainContext,
    model: HostModel
  ) => Promise<readonly HostModel[]>;
  readonly log: FallbackChainLog;
  /** The host's own timer for the pane (`SessionContext.setTimeout`): runs `callback` once,
   * `delayMs` from now, and answers whether it set the timer. This repository's `SessionContext`
   * slice keeps no handle to clear it by, so a callback that is no longer the current arming's
   * finds that out itself. */
  readonly armTimer: (callback: () => Promise<void>, delayMs: number) => boolean;
  /** The context of the pane a timer's return goes into, read when the timer fires: the
   * session the pane is on, or undefined when it is on none, or on one that is no Legion role's.
   * The runner itself checks that it is the session whose fallback state it holds. */
  readonly paneContext: () => Promise<ModelFallbackContext | undefined>;
}

export interface ModelFallback {
  /** `session_start`, and every session change after it: the state the branch holds
   * (`restoreFallbackCooldown`), or undefined when this session has never fallen back. A
   * relaunched worker comes back with its due time and its doubling levels, and a session already
   * sitting on a fallback the record does not hold is armed from the branch itself. */
  readonly restore: (state: FallbackCooldownState | undefined, ctx: ModelFallbackContext) => void;
  /** The host's `retry_fallback_applied`: the host walked the chain itself, and its own reason
   * says whether a content filter is what it walked for. */
  readonly armFromEvent: (event: RetryFallbackAppliedEvent, ctx: ModelFallbackContext) => void;
  /** The host's `agent_end`: a turn that has settled, which the provider ended cleanly, with a
   * content filter, or with some other failure. That is every end but one the host goes on from
   * itself; an end that waits only on a background job's result has settled. It makes no return:
   * a return the timer found the pane too busy for is made at the timer's first firing on an idle
   * pane (`maybeRestore`). */
  readonly turnSettled: (event: AgentEndEvent, ctx: ModelFallbackContext) => Promise<void>;
  /**
   * The return to the model the session fell back from, which this runner makes in one place:
   * one check of whether the arming is due, with these triggers.
   *
   * - The host's `before_agent_start`, for any arming: it runs before the first request of a run
   *   a prompt starts.
   * - The host's `context`, for any arming: it runs before every request, but after the host has
   *   read that request's model, so a return made there is the model of the next request.
   * - A timer at the due time (`armTimer`), for a refusal's arming only: it is what puts back a
   *   run no prompt starts, an Envoy card's or a background job's result's, which neither hook
   *   reaches before that run's first request. The timer switches the model only on an idle
   *   pane: one that runs no turn and holds no queued message. A switch in the middle of a
   *   stream drops an OpenAI or Codex response, and a queued message is a steered continuation
   *   about to read its model. A timer that finds the pane busy fires again `BUSY_RECHECK_MS`
   *   later, and so on until a firing finds it idle, or until a hook has made the return; a busy
   *   turn's next `context` hook makes it one request on the fallback model later. A run that an
   *   agent-initiated wake starts before the first idle firing begins on the fallback model and
   *   comes back at its second request. A message the host holds on a pane that runs no turn
   *   keeps the timer firing four times a second and making no return until a hook runs: the
   *   run that message starts comes back as on main, at its `before_agent_start` when it is a
   *   prompt and at its second request otherwise.
   *
   * An arming of any other cause waits for the host to stop suppressing its model, which no time
   * says, so it sets no timer: only the two hooks end it, and a run no prompt starts can begin on
   * the fallback model and come back at its second request.
   */
  readonly maybeRestore: (ctx: ModelFallbackContext) => Promise<void>;
  /**
   * The first model below the session's own in its chain whose family is the session model's own
   * (`HostModel.identity.class`), or undefined when the chain holds none, when the session's model
   * names no family the catalog knows (`unknown`, a gateway's alias, whose family nothing here
   * can tell), or when this host exposes no registry to walk with. Every model of another family
   * in the chain is passed over: the cyber-policy ladder's last rung never changes family.
   */
  readonly sameFamilyFallback: (ctx: ModelFallbackContext) => Promise<HostModel | undefined>;
  /**
   * Moves the session to `next` as one step down its chain, and arms the return to the model the
   * walk started from on the refusal cooldown, exactly as a content filter's step does
   * (`walkChain`). Answers whether the session is on `next` with its return armed.
   */
  readonly stepDownTo: (ctx: ModelFallbackContext, next: HostModel) => Promise<boolean>;
}

/** The newest `model_change` on the branch, the one before it, and the one the walk ending there
 * started from. The entry before says which model a fallback left, and whether a new step
 * continues the walk a live arming began. */
interface NewestChange {
  readonly entry: ModelChangeEntry;
  readonly previous: ModelChangeEntry | undefined;
  /** The newest entry before `entry` that is no step of a fallback walk, or undefined when the
   * branch holds none: the model the session was on before this walk down the chain, which is
   * what an arming returns to (`restoreTarget`). */
  readonly walkedFrom: ModelChangeEntry | undefined;
}

/** A `model_change` as `getBranch()` hands it back (`ModelChangeEntry`, `src/pi-types.ts`). The
 * branch is read as `unknown`, field by field, as `provider-refusal.ts` reads its message: a host
 * whose entry shape differs leaves the fallback unclaimed rather than throwing in a handler. */
function isModelChange(entry: unknown): entry is ModelChangeEntry {
  return (
    typeof entry === "object" &&
    entry !== null &&
    "type" in entry &&
    entry.type === "model_change" &&
    "id" in entry &&
    typeof entry.id === "string" &&
    entry.id !== ""
  );
}

/** A step of a walk down the chain, as against every other `model_change`. Before a walk of the
 * session's own the host appends an entry naming the new model with `role: "fallback"` and
 * `resolvedModelIsFallback: true`; its own restore appends `role: "fallback"` with
 * `resolvedModelIsFallback: false`, and the operator's `/model`, the plugin's own `setModel` and
 * an advisor's failover are none of the three. */
function isLiveFallback(entry: ModelChangeEntry): boolean {
  return entry.role === "fallback" && entry.resolvedModelIsFallback === true;
}

/** The index of the newest `model_change` before `end`, or -1 when the branch holds none. */
function modelChangeBefore(branch: readonly unknown[], end: number): number {
  for (let index = Math.min(end, branch.length) - 1; index >= 0; index -= 1) {
    if (isModelChange(branch[index])) return index;
  }
  return -1;
}

function sameModel(a: HostModel | undefined, b: HostModel | undefined): boolean {
  return a !== undefined && b !== undefined && a.provider === b.provider && a.id === b.id;
}

/** The newest `model_change` the branch holds with the entries around it, or undefined when it
 * holds none this build reads. */
function newestModelChange(ctx: ModelFallbackContext): NewestChange | undefined {
  const branch = ctx.sessionManager.getBranch?.() ?? [];
  const at = modelChangeBefore(branch, branch.length);
  if (at < 0) return undefined;
  const entry = branch[at];
  if (!isModelChange(entry)) return undefined;
  const beforeAt = modelChangeBefore(branch, at);
  const before = beforeAt < 0 ? undefined : branch[beforeAt];
  return {
    entry,
    previous: isModelChange(before) ? before : undefined,
    walkedFrom: walkedFrom(branch, at),
  };
}

/** The model the walk ending at `at` started from, as the branch records it: the steps of one
 * walk are the run of fallback entries before `at`, so the entry this answers is the newest one
 * before them that is no step of a walk — the operator's `/model`, the host's own restore, the
 * model the session was given to begin with. */
function walkedFrom(branch: readonly unknown[], at: number): ModelChangeEntry | undefined {
  let index = modelChangeBefore(branch, at);
  while (index >= 0) {
    const entry = branch[index];
    if (!isModelChange(entry)) return undefined;
    if (!isLiveFallback(entry)) return entry;
    index = modelChangeBefore(branch, index);
  }
  return undefined;
}

/** When the branch says an entry was written, or undefined when this build cannot read it: the
 * host stamps every entry with an ISO time (`SessionEntryBase`,
 * `session/session-entries.ts:99-117` at the pinned tag). */
function changedAt(entry: ModelChangeEntry): number | undefined {
  const at = entry.timestamp === undefined ? Number.NaN : Date.parse(entry.timestamp);
  return Number.isFinite(at) ? at : undefined;
}

/** The longest delay a timer of the host's runtime holds: Bun, like Node, runs a timer whose delay
 * is longer than 2^31 - 1 ms after 1 ms. A wait past it is set in pieces (`armReturn`), and the
 * cooldown settings put no upper bound on a wait. */
const LONGEST_TIMER_MS = 2_147_483_647;

/**
 * How long a timer that found the pane busy waits before it looks again. A run that a wake starts
 * before the first firing on the idle pane begins on the fallback model (and comes back at its
 * second request), so the interval is the longest idle gap after a turn's end that the return can
 * still miss. Measured on 2026-11-20 over the 2,844 Oh My Pi transcripts this dev box wrote in a
 * day, as the time from a run's last assistant message to the next run's first message:
 *
 * - a background job's result: 9.4% of 6,227 come within 5 ms (a job that ended during the turn,
 *   whose result continues it with no idle firing possible at any interval), 11.1% within 250 ms,
 *   11.9% within 1 s, 15.5% within 5 s;
 * - an Envoy card: 0.1% of 9,161 within 250 ms, 0.8% within 1 s, 6.1% within 5 s;
 * - an IRC message: 0.2% of 6,451 within 250 ms, 0.8% within 1 s, 5.6% within 5 s.
 *
 * At 250 ms the return misses 0.1-0.2% of cards and IRC wakes and 1.7% of job results beyond the
 * 9.4% no interval catches; at 5 s it would miss about 6% of cards and IRC wakes. A firing reads
 * the pane's context, the session's claim and two host flags, and the timer fires four times a
 * second for as long as the pane is busy: while a turn runs, and while the host holds a message on
 * a pane that runs no turn, until a hook runs. That is a small cost against a turn's own requests.
 */
const BUSY_RECHECK_MS = 250;

export function createModelFallback(deps: ModelFallbackDeps): ModelFallback {
  let state: FallbackCooldownState = { levels: {} };
  /** Every `model_change` entry an event has already claimed, so a second event — an advisor's
   * that follows the session's own onto the same model — does not claim it again. */
  const claimed = new Set<string>();
  /** The fallback the host applied since the last settled turn: the `model_change` entry its walk
   * wrote, and whether the event said what it walked for. An event that named a reason has
   * already answered the refusal it walked, so a filter that settles after it is another one. */
  let appliedThisTurn: { readonly entryId: string; readonly reasoned: boolean } | undefined;

  /** Keeps the state and persists it, and only when the step changed it: every unchanged step of
   * the machine answers the state it was given. */
  const save = (next: FallbackCooldownState): void => {
    if (next === state) return;
    state = next;
    deps.saveState(next);
    armReturn();
  };

  /** The due time the host's timer is set for, so a save that leaves the arming's due time
   * where it was sets no second timer. */
  let timerFor: number | undefined;

  /** The session whose fallback state this runner holds: the one `restore` last read. A session
   * change moves the pane before `restore` reads the new session's state (a controller's re-claim
   * runs in between), and a timer that fires then has no session to make its return into. */
  let session: string | undefined;

  /**
   * The timer trigger of the return (`ModelFallback.maybeRestore` states the rule): the host's
   * timer for the due time of a refusal's arming, in pieces no longer than a timer can hold, and
   * `BUSY_RECHECK_MS` apart while it finds the pane busy. It reads the arming from the state each
   * time it sets a timer, so a re-arm stops by itself once the arming is gone (put back by a hook,
   * superseded, or another session's state now): it sets no timer for an arming the state does not
   * hold.
   */
  const armReturn = (delayMs?: number): void => {
    const arming = state.arming;
    if (arming === undefined || arming.cause !== "refusal" || arming.dueAt === timerFor) return;
    const set = deps.armTimer(
      async () => {
        // A newer arming set a timer of its own; this one belongs to none.
        if (timerFor !== arming.dueAt) return;
        timerFor = undefined;
        // A timer that ran before the due time by this runner's clock (the host's clock ahead of
        // it, or a wait longer than one timer can hold) is set again for what is left.
        if (deps.now() < arming.dueAt) {
          armReturn();
          return;
        }
        let busy = false;
        try {
          const ctx = await deps.paneContext();
          if (ctx === undefined || ctx.sessionManager.getSessionId?.() !== session) return;
          busy = ctx.isIdle?.() === false || ctx.hasPendingMessages?.() === true;
          if (!busy) await restoreIfDue(ctx);
        } catch (error) {
          contained(
            "put the session back on the model it fell back from when its wait ended",
            error
          );
        }
        // A busy pane is looked at again. A timer that found it idle and could not set the model
        // leaves the return to the next request's hook, rather than trying again at once.
        if (busy) armReturn(BUSY_RECHECK_MS);
      },
      delayMs ?? Math.min(LONGEST_TIMER_MS, Math.max(0, arming.dueAt - deps.now()))
    );
    if (set) timerFor = arming.dueAt;
  };

  /** One step of the cooldown machine against the state this runner holds. */
  const step = (input: FallbackCooldownInput): FallbackCooldownState =>
    stepFallbackCooldown(state, input, deps.limits);

  /**
   * The event's own fallback, or undefined when it is not this session's.
   *
   * Ported from the reference extension (sjawhar/dotfiles `omp/extensions/fallback-event.ts`),
   * whose reasoning holds here. Oh My Pi emits `retry_fallback_applied` from two places: its turn
   * recovery, when it moves the session's own model down `retry.fallbackChains`, and its advisor
   * runtime, when an advisor's own model fails over. The payloads look the same, and an advisor's
   * step leaves the session's model alone. Before the first kind the host appends a
   * `model_change` naming the new model with `role: "fallback"` and `resolvedModelIsFallback:
   * true`; the advisor path appends nothing, and the host's own restore appends `role: "fallback"`
   * with `resolvedModelIsFallback: false`. So an event is the session's own when the newest
   * `model_change` on the branch is such an entry, resolves to the event's `to`, has not been
   * claimed by an earlier event, and follows a `model_change` naming the event's `from`. The last
   * two close two holes: an advisor event that follows a session fallback onto the same model
   * finds the entry claimed, and one that lands between the host appending the entry and emitting
   * the session's event names the wrong `from`.
   */
  const ownFallback = (
    event: RetryFallbackAppliedEvent,
    ctx: ModelFallbackContext
  ): NewestChange | undefined => {
    const resolve = ctx.models?.resolve;
    if (resolve === undefined) return undefined;
    const change = newestModelChange(ctx);
    if (change === undefined) return undefined;
    const { entry, previous } = change;
    if (entry.model === undefined) return undefined;
    if (!isLiveFallback(entry)) return undefined;
    if (claimed.has(entry.id)) return undefined;
    if (!sameModel(resolve(event.to), resolve(entry.model))) return undefined;
    const beforeModel = previous?.model === undefined ? undefined : resolve(previous.model);
    const fromModel = resolve(event.from);
    // Lenient when either side resolves to nothing: a missed fallback costs more than this race.
    if (beforeModel !== undefined && fromModel !== undefined && !sameModel(beforeModel, fromModel))
      return undefined;
    claimed.add(entry.id);
    return change;
  };

  /**
   * The chain walk's context, or undefined when this host exposes no registry to walk with. The
   * host's own `ModelRegistry` methods read its private state, so each is called on the registry
   * rather than handed over detached.
   */
  const chainContext = (ctx: ModelFallbackContext): FallbackChainContext | undefined => {
    const registry = ctx.modelRegistry;
    const models = ctx.models;
    if (models === undefined) return undefined;
    if (registry?.find === undefined || registry.hasProvider === undefined) return undefined;
    return {
      modelRegistry: {
        find: (provider, id) => registry.find?.(provider, id),
        hasProvider: (provider) => registry.hasProvider?.(provider) === true,
      },
      models,
      log: deps.log,
    };
  };

  /**
   * The selector a new arming returns to: the model the session was on before the first step of
   * the walk this step belongs to, never one the chain handed it in the middle.
   *
   * Two records say what that model is, and the live arming is read first. A step continues a
   * walk when the `model_change` it arms on follows the arming's own entry: nothing moved the
   * session's model in between, so this is the same chain walked one step further down and the
   * model to come back to is the one that arming already names — the rule the reference
   * extension states (sjawhar/dotfiles `omp/extensions/fallback-event.ts`). This is also the
   * only record left when the walk began on a model no `model_change` names, a session's first.
   *
   * The branch says it for every walk this process holds no arming for: a relaunch into a walk
   * an older build left, a host walk `ownFallback` did not claim. Its steps are the run of
   * fallback entries before this one (`walkedFrom`), and the entry before them names the model
   * the walk started on. A walk after any other model change — the operator's `/model`, the
   * host's own restore — finds that change there and arms on the model it is leaving, which is
   * the same one.
   *
   * The cooldown keys its doubling level by this selector, so a continued walk raises the level
   * of the model the session will return to rather than one in the middle of the chain.
   */
  const restoreTarget = (change: NewestChange, leaving: string): string => {
    const arming = state.arming;
    if (arming !== undefined && change.previous?.id === arming.entryId) return arming.restoreTo;
    return change.walkedFrom?.model ?? leaving;
  };

  /**
   * The plugin's own step down the chain, for a content filter the host did not walk: the host
   * leaves a refusal that lands after the model has already sent text where it is
   * (`turn-recovery.ts:2177-2211` and `:3605-3615` at tag
   * `v18.18.6-sami.20261126-222037`).
   *
   * The arming names the `model_change` entry **this** call wrote, read back from `getBranch()`
   * after it. The host's own fallback claim would not do: `pi.setModel` clears the active retry
   * fallback and appends an ordinary `model_change` (`session/model-controls.ts:327-331` at the
   * pinned tag), so nothing on the branch would carry the host's claim at all.
   */
  const walkChain = async (ctx: ModelFallbackContext, current: HostModel): Promise<void> => {
    const selector = modelSelector(current);
    const chain = chainContext(ctx);
    if (chain === undefined) {
      deps.log.warn(
        "legion: a content filter refused this turn and this host exposes no model registry",
        {
          model: selector,
        }
      );
      return;
    }
    const next = await deps.nextModel(chain, current);
    if (next === undefined) {
      deps.log.warn("legion: a content filter refused this turn and the chain has no next model", {
        model: selector,
      });
      return;
    }
    await applyStep(ctx, current, next);
  };

  /** Sets `next` as one step down the chain from `current` and arms the return on the
   * `model_change` entry the switch wrote; answers whether the arming was saved. */
  const applyStep = async (
    ctx: ModelFallbackContext,
    current: HostModel,
    next: HostModel
  ): Promise<boolean> => {
    const selector = modelSelector(current);
    if (!(await deps.setModel(next))) {
      deps.log.warn("legion: the next model of the chain could not be set; staying on this one", {
        model: selector,
        next: modelSelector(next),
      });
      return false;
    }
    const change = newestModelChange(ctx);
    if (change === undefined) {
      deps.log.warn("legion: the model switch left no model_change entry to arm the return on", {
        model: selector,
        next: modelSelector(next),
      });
      return false;
    }
    save(
      step({
        kind: "fallback-applied",
        restoreTo: restoreTarget(change, selector),
        entryId: change.entry.id,
        cause: "refusal",
        now: deps.now(),
      })
    );
    return true;
  };

  /** The family the host's catalog gives `model`, or undefined when it gives none it knows. */
  const familyOf = (model: HostModel | undefined): string | undefined => {
    const family = model?.identity?.class;
    return family === undefined || family === "" || family === "unknown" ? undefined : family;
  };

  const sameFamilyFallback = async (ctx: ModelFallbackContext): Promise<HostModel | undefined> => {
    const current = ctx.models?.current?.();
    const family = familyOf(current);
    if (current === undefined || family === undefined) {
      deps.log.warn(
        "legion: the session's model names no family the catalog knows, so no weaker model of its family is chosen",
        { model: current === undefined ? undefined : modelSelector(current) }
      );
      return undefined;
    }
    const chain = chainContext(ctx);
    if (chain === undefined) {
      deps.log.warn("legion: this host exposes no model registry to find a weaker model with", {
        model: modelSelector(current),
      });
      return undefined;
    }
    const candidates = await deps.fallbackModels(chain, current);
    return candidates.find(
      (candidate) => !sameModel(candidate, current) && familyOf(candidate) === family
    );
  };

  const settled = async (event: AgentEndEvent, ctx: ModelFallbackContext): Promise<void> => {
    // The host is about to go on with this turn itself (`turnGoesOn`), so it has not settled;
    // acting on it would step the session down twice (`session/agent-session.ts:6371-6379` at the
    // pinned tag). An end that waits only on a background job's result has settled, and the job's
    // result starts the next turn on the model this end leaves the session on.
    //
    // One window stays open there. The host does not wait for this handler (it fires the
    // extensions' `agent_end` without awaiting them, `session/agent-session.ts:7103` at the pinned
    // tag), and it picks each request's model before that request's `context` hook runs
    // (`agent/src/agent-loop.ts:2090-2093`). What a request does wait for is a switch already under
    // way: `pi.setModel` runs the switch inside `trackModelSwitch`
    // (`session/model-controls.ts:329-351`), and each main request waits for it before the host
    // reads its model (`settleModelSwitch`, `session/session-tools.ts:2406-2417`, a hook that
    // `agent-session.ts:3948` registers and `agent-loop.ts:1469` runs before each model call). So a
    // request of a run a job's result starts goes to the model that refused only when the host
    // selects it before the switch has started: while the walk below still awaits the chain's next
    // model, the model's key or its metadata (`refreshSelectedModelMetadata`,
    // `model-controls.ts:325`, which for a local backend can wait on the network), every request
    // selected meanwhile takes the old model, so there is no bound of one request; a request
    // selected once the tracked switch has started waits for it and takes the new one. Measured on
    // 2026-11-21 on Oh My Pi 18.18.6-sami.20261119-162325, a build whose requests did not wait for
    // a switch, with a job that had already ended when the refused turn ended, the result's first
    // request went to the next model in 5 of 5 runs. What would close the window is in the host:
    // awaiting the extensions' `agent_end` handlers before it starts the run a background job's
    // result asks for.
    if (turnGoesOn(event)) return;
    const applied = appliedThisTurn;
    appliedThisTurn = undefined;
    const outcome = classifyTurnEnd(event);
    const current = ctx.models?.current?.();
    if (outcome.kind === "content-filter") {
      // A host build whose `retry_fallback_applied` carried no reason: this settled turn is the
      // one thing that says a filter is what the host walked for, so that same arming becomes a
      // refusal's and waits out the growing cooldown rather than a suppression the host never
      // applies for one. The walk already happened for it, so the session steps down no further.
      if (
        applied?.reasoned === false &&
        state.arming?.entryId === applied.entryId &&
        state.arming.cause === "other"
      ) {
        save(
          step({
            kind: "fallback-applied",
            restoreTo: state.arming.restoreTo,
            entryId: state.arming.entryId,
            cause: "refusal",
            now: deps.now(),
          })
        );
        return;
      }
      // Every other filter that ended a settling turn is one no fallback was applied **for**,
      // and the plugin steps the session down itself. A refusal of the fallback model after the
      // host walked an earlier one in the same turn is one of those: the host's own event said
      // what that walk was for, so this message is the session's second refusal, on the model it
      // just moved to, and leaving it alone would send the next request to the model that had
      // refused it.
      if (current === undefined) {
        deps.log.warn("legion: a content filter refused this turn and this host names no model");
        return;
      }
      await walkChain(ctx, current);
      return;
    }
    // A provider error that is no content filter leaves every wait where it is: a rate limit is
    // not the model refusing. Only a clean turn puts its model's wait back to the base.
    if (outcome.kind === "provider-error" || current === undefined) return;
    save(step({ kind: "clean-turn", selector: modelSelector(current) }));
  };

  const restoreIfDue = async (ctx: ModelFallbackContext): Promise<void> => {
    const arming = state.arming;
    if (arming === undefined) return;
    // Whether the arming is due at all is decided in one place, the cooldown machine's
    // `restoreDue`, and asked before anything else: the host runs this before every provider
    // request, and a refusal's arming waits an hour to eight hours. The reads below — the whole
    // session branch, the whole model catalogue — would answer nothing for every one of those
    // requests, and the host is asked whether it suppresses the model only for an arming whose
    // wait that is. The `superseded` and `restored` bookkeeping skipped while an arming waits
    // moves no model: it is read again at the first request after the wait is over.
    const suppressed = () => ctx.modelRegistry?.isSelectorSuppressed?.(arming.restoreTo) === true;
    if (restoreDue(state, deps.now(), suppressed) === undefined) return;
    const newest = newestModelChange(ctx);
    // Any model change since the fallback — the operator's `/model`, the host's own restore,
    // another session resumed in this process — is not this arming's to undo.
    if (newest === undefined || newest.entry.id !== arming.entryId) {
      save(step({ kind: "superseded" }));
      return;
    }
    const resolve = ctx.models?.resolve;
    // The return undoes the fallback's own move and nothing after it. A launch that names its
    // model (`--model @default`) moves the session without a `model_change` entry, so the
    // arming's entry can still be the newest while the session runs a model its deployment chose
    // since: the session goes back only while it still runs the model that entry names. A host
    // that names no current model cannot show that, so the arming ends, as it does below for a
    // model gone from the roster, rather than reading the branch again at every request.
    const current = ctx.models?.current?.();
    const fallback = newest.entry.model === undefined ? undefined : resolve?.(newest.entry.model);
    if (!sameModel(current, fallback)) {
      save(step({ kind: "superseded" }));
      return;
    }
    const target = resolve?.(arming.restoreTo);
    if (target === undefined) {
      // Gone from this session's roster (a credential lost, a catalog change): nothing to go back
      // to, so the arming ends rather than being tried at every request for the rest of the run.
      save(step({ kind: "superseded" }));
      return;
    }
    if (sameModel(current, target)) {
      save(step({ kind: "restored" }));
      return;
    }
    const sessionBefore = ctx.sessionManager.getSessionId?.();
    const switched = await deps.setModel(target);
    const sessionAfter = ctx.sessionManager.getSessionId?.();
    if (sessionAfter !== sessionBefore) {
      // The record goes into the session the pane is on now, which is not this arming's: the
      // arming stays recorded in its own session, whose newest model change is still the
      // fallback, so its next return switches it.
      deps.log.warn(
        "legion: the pane moved to another session while the model it fell back from was set again",
        { model: arming.restoreTo, from: sessionBefore, to: sessionAfter }
      );
      return;
    }
    if (!switched) {
      deps.log.warn("legion: the model this session fell back from could not be set again", {
        model: arming.restoreTo,
      });
      return;
    }
    // `restore` can replace the state while the switch is awaited: this arming's return is not
    // that state's to record.
    if (state.arming !== arming) return;
    save(step({ kind: "restored" }));
  };

  /** What a handler does with a failure of its own: one warning, and the host's event is over. */
  const contained = (what: string, error: unknown): void => {
    deps.log.warn(`legion: the model fallback runner failed to ${what}`, {
      error: messageFor(error),
    });
  };

  return {
    restore: (restored, ctx) => {
      try {
        state = restored ?? { levels: {} };
        session = ctx.sessionManager.getSessionId?.();
        // A fallback applied in a turn of the session this pane has just left is no fallback of
        // this one.
        appliedThisTurn = undefined;
        if (state.arming !== undefined) {
          // A relaunched worker's arming: its return is due when the record says, whatever the
          // relaunch's boot cost, and the timer is set for that moment, or now when it is past.
          armReturn();
          return;
        }
        // No record of an arming, and the session is sitting on a fallback: one written before
        // this build ran, or a host walk `ownFallback` did not claim. Nothing else will ever arm
        // it, so it is armed here from the entry's own time (dispatch://INFRA-956 R1, answer 5)
        // and the session comes back when the wait from that moment is over.
        const change = newestModelChange(ctx);
        if (change === undefined) return;
        const { entry, walkedFrom } = change;
        if (!isLiveFallback(entry)) return;
        // Where that walk started, which `restoreTarget` reads for a live one: the branch is the
        // whole record here, since a session with no arming of its own holds no other. A walk of
        // two steps or more is why this is not the change before the entry: that one names a
        // model the chain handed the session in the middle of the same walk, and the role's own
        // model is the one before every step of it.
        if (walkedFrom?.model === undefined) {
          deps.log.warn(
            "legion: this session runs on a fallback model and its branch does not say which model it came from",
            { model: entry.model }
          );
          return;
        }
        // The entry says nothing about why the host stepped down, so it is armed as a refusal's:
        // a cooldown the session comes back after, rather than a suppression that nothing here
        // will ever see end. An entry whose own time this build cannot read is taken as written
        // now, so the session waits the whole base cooldown from this moment: coming back late
        // costs one wait, coming back early hands the session to a model that may refuse again.
        claimed.add(entry.id);
        save(
          step({
            kind: "fallback-applied",
            restoreTo: walkedFrom.model,
            entryId: entry.id,
            cause: "refusal",
            now: changedAt(entry) ?? deps.now(),
          })
        );
      } catch (error) {
        contained("read the fallback this session is already on", error);
      }
    },
    armFromEvent: (event, ctx) => {
      try {
        // The Fireworks Fast degrade keeps the model and drops only the tier that refused the
        // session, so restoring it would fail again every cooldown.
        if (event.role === "fireworks-fast") return;
        const own = ownFallback(event, ctx);
        if (own === undefined) return;
        appliedThisTurn = { entryId: own.entry.id, reasoned: event.reason !== undefined };
        // What the host walked for, in the provider's own words: it names the reason as
        // `Request failed: ` and the error text of the message that failed
        // (`session/turn-recovery.ts` at tag `v18.18.6-sami.20261126-222037`), so
        // `Request failed: Refusal (cyber): Declined` reads as Anthropic's `cyber` filter and
        // arms the same refusal the plugin's own walk arms, category and all.
        //
        // Reading it here is what covers a refusal that came before the model sent any text:
        // the host walks the chain itself and then replays the turn, which succeeds, so the
        // settled turn's last assistant message is clean and `settled` below never sees that
        // refusal. An `"other"` arming would then wait on a suppression the host never applies
        // — for a classifier refusal it pins the fallback instead
        // (`session/turn-recovery.ts:4318` at the pinned tag) — and hand the session straight
        // back to the model that had just refused it, with no cooldown at all.
        //
        // A host build that sends no reason arms `"other"`, as every arming did before, and
        // `settled` keeps the path that re-arms such a fallback as a refusal once the turn ends
        // on the filter's own message. That is why both paths exist, and why `appliedThisTurn`
        // carries which build this is: after an event that named its reason, a filter that ends
        // the turn is a second refusal and `settled` steps the session down for it.
        const refusal =
          event.reason === undefined ? undefined : classifyProviderError(event.reason);
        save(
          step({
            kind: "fallback-applied",
            cause: refusal?.kind === "content-filter" ? "refusal" : "other",
            restoreTo: restoreTarget(own, event.from),
            entryId: own.entry.id,
            now: deps.now(),
          })
        );
      } catch (error) {
        contained("arm the cooldown of a fallback the host applied", error);
      }
    },
    turnSettled: async (event, ctx) => {
      try {
        await settled(event, ctx);
      } catch (error) {
        contained("read how the provider ended this turn", error);
      }
    },
    maybeRestore: async (ctx) => {
      try {
        await restoreIfDue(ctx);
      } catch (error) {
        contained("put the session back on the model it fell back from", error);
      }
    },
    sameFamilyFallback: async (ctx) => {
      try {
        return await sameFamilyFallback(ctx);
      } catch (error) {
        contained("find a weaker model of the session model's family", error);
        return undefined;
      }
    },
    stepDownTo: async (ctx, next) => {
      try {
        const current = ctx.models?.current?.();
        if (current === undefined) {
          deps.log.warn("legion: this host names no model to step down from");
          return false;
        }
        return await applyStep(ctx, current, next);
      } catch (error) {
        contained("step down to a weaker model of the session model's family", error);
        return false;
      }
    },
  };
}
