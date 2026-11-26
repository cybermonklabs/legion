import { messageFor } from "@legion/envoy-client/errors";
import { assistantRanTool, assistantRefusal } from "@legion/pi-shared/phase-stall";
import type {
  AgentEndMessage,
  CompactionPreparation,
  HostModel,
  PiApi,
  SessionBeforeCompactEvent,
  SessionStopEvent,
  SessionStopEventResult,
} from "@legion/pi-shared/pi-types";
import { integerSetting, type StallLimitsLog } from "@legion/pi-shared/stall-limits";

/**
 * What a Legion role does when OpenAI refuses its turn under `cyber_policy`, before anything
 * reaches the daemon. The operator asked for a retry ladder before a hold: a plain retry, a retry
 * with a message appended, a compacted retry, and then a weaker model of the same family from the
 * configured fallback chain, never another family. A content filter is often stochastic, so the
 * same request can pass a second time.
 *
 * One pass of four rungs, one try each, each after its own wait:
 *
 * 1. `retry`: the request sent again as it was.
 * 2. `message`: the request with one message appended that asks the model to describe what it
 *    would otherwise reproduce.
 * 3. `compact`: the history compacted into a text summary, the newest turn group included, and
 *    the request sent from the summary. A session with nothing to compact, as a new one is at
 *    the shipped `compaction.keepRecentTokens`, gets the turn Oh My Pi resumes instead: a retry.
 * 4. `weaker-model`: the request sent again on the first model below the session's own in its
 *    `retry.fallbackChains` whose family is the session model's own. No rung changes family.
 *
 * Only when the last rung is refused too does the turn settle, so the refusal reaches the daemon
 * with its `cyber_policy` code, as the prompt's result and as the turn's outcome, and the daemon
 * holds the agent as before.
 *
 * Why this runs inside the turn. The daemon reads a prompt's outcome from Oh My Pi's
 * `prompt_result`, which the host writes at the first `agent_end` it marks `yielded`
 * (`modes/rpc/rpc-prompt-results.ts:159-170` at v18.18.6-sami.20261125-164517). A `session_stop`
 * handler that answers a continuation makes the host end the run with `willContinue`, which it
 * marks as not yielded (`session/agent-session.ts:6458-6459, 7061-7067`), so the prompt stays open
 * across every rung and its result is the last rung's. The host stops waiting for a
 * `session_stop` handler at 30 s without aborting it (`extensibility/extensions/runner.ts:152,
 * 1983-1988, 2030`), so each wait is at most `LONGEST_RUNG_WAIT_MS`, and past `SETTLE_BUDGET_MS` a
 * rung changes nothing and sends nothing. The compaction itself runs outside every handler
 * (below), so its length is bound by nothing here.
 *
 * The compaction. It is asked for as `mode: "soft"`, a text summary by the session's model: the
 * shipped `compaction.methodOrder` puts provider-native compaction and snapcompact first, and
 * both keep the refused text (`session/compact-modes.ts:40-57`). The summary covers the newest
 * turn group too, through a `session_before_compact` handler registered at the ladder's first
 * compaction. Oh My Pi has no way to remove a handler, and with one registered it runs no
 * background compaction (`session/session-maintenance.ts:2988-2992, 3104, 3359`), so a pane that
 * has climbed to this rung once compacts its sessions in the foreground from then on. A
 * compaction with nothing to compact rejects, and the host resumes the turn it aborted
 * (`session-maintenance.ts:2108-2115, 2428-2437`); that resumed turn is the rung's request. Any
 * other failure resumes nothing, so the ladder starts a turn from inside the session (below).
 *
 * Why each rung's request reaches OpenAI. After a `cyber_policy` refusal Oh My Pi holds every call
 * to an OpenAI route, and every route whose family it cannot tell, until input from outside the
 * session (`session/cyber-policy-closure.ts:9-11, 81-96, 131`); a continuation is inside input, and
 * a call it makes is held with no request sent. So each rung first sends one hidden message marked
 * `origin: "outside"`, which ends the hold before the message is queued
 * (`session/agent-session.ts:12605-12616`), and the compaction is asked for with the same origin
 * (`:9369-9373`). Deciding to retry is this plugin's, on the operator's behalf, with the settings
 * below. That message and the continuation's own text are taken out of every request again
 * (`CyberPolicyLadder.request`), and so is each refused reply, so the plain retry sends what the
 * refused request sent.
 *
 * Two turns end with no `session_stop` and so get no ladder: a refused reply that carries tool
 * calls, and a turn that ends while a background job's result is due (`agent-session.ts:6417-6421,
 * 6454-6457`). Their refusal reaches the daemon as before.
 */

/** The four rungs, in the order the ladder climbs them. */
export const CYBER_POLICY_RUNGS = ["retry", "message", "compact", "weaker-model"] as const;
export type CyberPolicyRung = (typeof CYBER_POLICY_RUNGS)[number];

/** The longest wait before one rung: under the host's 30 s bound on a `session_stop` handler, with
 * room for the rung's own work (a model switch, an outside message) after the wait. */
export const LONGEST_RUNG_WAIT_MS = 25_000;

/** How long one rung's `session_stop` handler may take before it gives up the rung: under the
 * 30 s after which Oh My Pi drops the handler's answer and lets the turn settle, so a handler that
 * runs late moves no model and ends no hold after that. */
export const SETTLE_BUDGET_MS = 28_000;

/** The message the second rung appends, unless `LEGION_CYBER_POLICY_RETRY_MESSAGE` sets another. */
export const DEFAULT_CYBER_POLICY_MESSAGE =
  "The provider's content filter blocked your last response. Continue the task, and do not reproduce exploit code, payloads or credentials verbatim; describe them instead.";

/** The `customType` of the hidden message the ladder sends before each rung, and after a failed
 * compaction. Taken out of every request (`CyberPolicyLadder.request`), so the model never reads
 * it. */
export const CYBER_POLICY_RETRY_ENTRY = "legion-cyber-policy-retry";

/** That message, whose text only the transcript shows. */
const CYBER_POLICY_RETRY_MESSAGE = {
  customType: CYBER_POLICY_RETRY_ENTRY,
  content:
    "Legion sends the request the provider's content filter refused again (cyber_policy ladder).",
  display: false,
} as const;

/** The continuation text of every rung but the second. The host needs a text to continue the turn
 * (`agent-session.ts:7108-7133`); this one is taken out of every request again, and it says what a
 * model would need to read if it ever were sent. */
export const CYBER_POLICY_CONTINUE = "Continue the task from where the refused request left off.";

/** The host's `customType` for a `session_stop` continuation (`agent-session.ts:7123-7131`). */
const SESSION_STOP_CONTINUATION = "session-stop-continuation";

/** The name of the host's rejection of a compaction with nothing to compact, after which it resumes
 * the turn it aborted (`ManualCompactionNoOpError`, `session/session-maintenance.ts:467-472`). */
const NOTHING_TO_COMPACT = "ManualCompactionNoOpError";

/** The name of the host's rejection of a compaction someone stopped, an interrupt among them
 * (`CompactionCancelledError`, `packages/agent/src/compaction/errors.ts:13-19`). */
const COMPACTION_CANCELLED = "CompactionCancelledError";

/** Each rung's wait and the second rung's message. */
export interface CyberPolicyLadderSettings {
  readonly waitsMs: Readonly<Record<CyberPolicyRung, number>>;
  readonly message: string;
}

/** The variable each rung's wait is read from, and its default. */
const WAIT_SETTINGS: Readonly<Record<CyberPolicyRung, { name: string; defaultMs: number }>> = {
  retry: { name: "LEGION_CYBER_POLICY_RETRY_WAIT_MS", defaultMs: 15_000 },
  message: { name: "LEGION_CYBER_POLICY_MESSAGE_WAIT_MS", defaultMs: 25_000 },
  compact: { name: "LEGION_CYBER_POLICY_COMPACT_WAIT_MS", defaultMs: 25_000 },
  "weaker-model": { name: "LEGION_CYBER_POLICY_FALLBACK_WAIT_MS", defaultMs: 25_000 },
};

/**
 * The ladder's settings, read from this worker's environment as its other settings are
 * (`stallLimits`, `fallbackCooldownLimits`). A deployment sets them with `pod.env` or `pane_env`
 * in `legion.yaml`. A wait is a whole number of milliseconds from 0 to `LONGEST_RUNG_WAIT_MS`: a
 * value that is no such number is warned about and the default used, and one above the longest is
 * warned about and cut to it. An empty message is warned about and the default used.
 */
export function cyberPolicyLadderSettings(
  env: Record<string, string | undefined>,
  log: StallLimitsLog
): CyberPolicyLadderSettings {
  const wait = (rung: CyberPolicyRung): number => {
    const { name, defaultMs } = WAIT_SETTINGS[rung];
    const value = integerSetting(env, log, name, defaultMs, 0);
    if (value <= LONGEST_RUNG_WAIT_MS) return value;
    log.warn(
      `legion: ${name} is above ${LONGEST_RUNG_WAIT_MS}, the longest wait Oh My Pi's 30 s bound on a session_stop handler leaves room for; using ${LONGEST_RUNG_WAIT_MS}`,
      { value: env[name] }
    );
    return LONGEST_RUNG_WAIT_MS;
  };
  const waitsMs = {
    retry: wait("retry"),
    message: wait("message"),
    compact: wait("compact"),
    "weaker-model": wait("weaker-model"),
  };
  const raw = env.LEGION_CYBER_POLICY_RETRY_MESSAGE;
  if (raw === undefined) return { waitsMs, message: DEFAULT_CYBER_POLICY_MESSAGE };
  if (raw.trim() !== "") return { waitsMs, message: raw };
  log.warn("legion: LEGION_CYBER_POLICY_RETRY_MESSAGE is empty; using the default message");
  return { waitsMs, message: DEFAULT_CYBER_POLICY_MESSAGE };
}

/** The slice of the host's session context the ladder hands its collaborators. */
export interface CyberPolicyLadderContext {
  readonly compact?: (options: {
    readonly origin?: "outside" | "inside";
    readonly mode?: "soft" | "remote" | "snapcompact";
  }) => Promise<void>;
}

export interface CyberPolicyLadderLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface CyberPolicyLadderDeps<Context extends CyberPolicyLadderContext> {
  readonly settings: CyberPolicyLadderSettings;
  /** The host's `isCyberPolicyRefusal` (`@oh-my-pi/pi-ai/error`): OpenAI's refusal, or a call the
   * host's hold held after one. */
  readonly cyberPolicyRefusal: (message: unknown) => Promise<boolean>;
  /** The host's `isRecordableCyberPolicyRefusal`
   * (`@oh-my-pi/pi-coding-agent/session/cyber-policy-closure`): OpenAI's own refusal, never a call
   * the hold held. Only such a refusal starts a pass of the ladder. */
  readonly recordableRefusal: (message: unknown) => Promise<boolean>;
  /** The host's `pi.sendMessage`, which carries the ladder's hidden `CYBER_POLICY_RETRY_ENTRY`
   * message. */
  readonly sendMessage: PiApi["sendMessage"];
  /** The model of the session model's family below it in its chain (`ModelFallback`). */
  readonly sameFamilyFallback: (ctx: Context) => Promise<HostModel | undefined>;
  /** Moves the session to that model and arms its return (`ModelFallback.stepDownTo`). */
  readonly stepDownTo: (ctx: Context, model: HostModel) => Promise<boolean>;
  /** Resolves true after `ms`, or false at once when `signal` aborts first. */
  readonly wait: (ms: number, signal?: AbortSignal) => Promise<boolean>;
  /** Registers `handler` on the host's `session_before_compact`, for this pane's Legion sessions.
   * The ladder calls it once, before its first compaction, since a registered handler turns off
   * the host's background compaction and the host has no way to remove one. */
  readonly registerBeforeCompact: (handler: (event: SessionBeforeCompactEvent) => void) => void;
  readonly log: CyberPolicyLadderLog;
}

export interface CyberPolicyLadder<Context extends CyberPolicyLadderContext> {
  /** `session_stop`: the turn is about to settle. A `cyber_policy` refusal climbs one rung and
   * answers the host's continuation; anything else answers undefined and lets the turn settle. */
  readonly settle: (
    event: SessionStopEvent,
    ctx: Context
  ) => Promise<SessionStopEventResult | undefined>;
  /** `context`: one provider request is about to be built. Starts the compacted rung's compaction
   * when it is due, and answers the request's messages without the ladder's own messages and
   * without the refused replies, or undefined when it carries none of them. */
  readonly request: (
    messages: readonly unknown[],
    ctx: Context
  ) => Promise<readonly unknown[] | undefined>;
  /** `turn_end`: a reply the provider gave ends the pass, so the next refusal starts a new one,
   * and so does an abort that is not the ladder's own compaction's. */
  readonly turnEnded: (message: AgentEndMessage | undefined) => void;
  /** The pane moved to another session: whatever pass ran was the old session's, and a compaction
   * it started answers nothing of the new one's. */
  readonly reset: () => void;
}

/** Whether `message` is one of the ladder's own: the hidden message, or a continuation carrying
 * `CYBER_POLICY_CONTINUE`. The second rung's continuation is the model's to read. */
function isLadderMessage(message: unknown): boolean {
  if (typeof message !== "object" || message === null) return false;
  if (!("role" in message) || message.role !== "custom" || !("customType" in message)) {
    return false;
  }
  if (message.customType === CYBER_POLICY_RETRY_ENTRY) return true;
  return (
    message.customType === SESSION_STOP_CONTINUATION &&
    "content" in message &&
    message.content === CYBER_POLICY_CONTINUE
  );
}

export function createCyberPolicyLadder<Context extends CyberPolicyLadderContext>(
  deps: CyberPolicyLadderDeps<Context>
): CyberPolicyLadder<Context> {
  /** The index of the next rung to climb; 0 while no pass runs. */
  let next = 0;
  /** A pass ran out of rungs and the provider has not answered since: no refusal starts another
   * pass until it does, so one refusal gets one pass and no more. */
  let spent = false;
  /** The compacted rung's continuation is due: its first request starts the compaction. */
  let compactDue = false;
  /** The ladder's own compaction is running, which `widen` widens. */
  let compacting = false;
  /** The ladder's compaction aborted the turn it was asked from, and that turn's `turn_end` has
   * not come yet: the one abort that ends no pass. */
  let ownAbortDue = false;
  /** `widen` is registered on the host's `session_before_compact`. */
  let widening = false;
  /** Which pass this is: ending a pass, spending it and `reset` move it on, so a compaction or a
   * `settle` that began under an earlier one changes nothing when it ends. */
  let pass = 0;

  const classify = async (
    check: (message: unknown) => Promise<boolean>,
    message: unknown
  ): Promise<boolean> => {
    try {
      return await check(message);
    } catch (error) {
      deps.log.warn(
        "legion: the host could not class a refused reply for the cyber_policy ladder",
        {
          error: messageFor(error),
        }
      );
      return false;
    }
  };

  const endPass = (why: string): void => {
    if (next > 0) deps.log.debug(`legion: the cyber_policy ladder ended: ${why}`, { rungs: next });
    next = 0;
    compactDue = false;
    pass += 1;
  };

  const exhaust = (): void => {
    deps.log.warn(
      "legion: every rung of the cyber_policy ladder was refused; the refusal goes to the daemon",
      { rungs: next }
    );
    next = 0;
    compactDue = false;
    spent = true;
    pass += 1;
  };

  /** Input from outside the session, sent before each rung: it ends the host's hold for that
   * rung's request (`session/agent-session.ts:12605-12616`). */
  const endHold = (): void =>
    deps.sendMessage(CYBER_POLICY_RETRY_MESSAGE, {
      deliverAs: "nextTurn",
      triggerTurn: false,
      origin: "outside",
    });

  /** A turn from inside the session, started after a failed compaction: the turn that compaction
   * aborted is not resumed, and the prompt waits on a run nothing else starts. A refused summary
   * started the host's hold again, which holds this turn's call, and that held call's `settle`
   * climbs the next rung, or, with none left, settles the prompt with the `cyber_policy` code.
   * After any other failure the hold stays ended, and this turn's request is the rung's retry. */
  const startHeldTurn = (): void =>
    deps.sendMessage(CYBER_POLICY_RETRY_MESSAGE, { deliverAs: "nextTurn", triggerTurn: true });

  /** `session_before_compact`: the ladder's own compaction summarizes the newest turn group too;
   * every other compaction is left as the host prepared it. */
  const widen = (event: SessionBeforeCompactEvent): void => {
    if (!compacting) return;
    const leaf = event.branchEntries.at(-1);
    const leafId =
      typeof leaf === "object" && leaf !== null && "id" in leaf && typeof leaf.id === "string"
        ? leaf.id
        : undefined;
    if (leafId === undefined) return;
    summarizeThrough(event.preparation, leafId);
  };

  const startCompaction = (ctx: Context): void => {
    const compact = ctx.compact;
    // `settle` passes over the compacted rung where the host has no compaction.
    if (compact === undefined) return;
    if (!widening) {
      widening = true;
      deps.registerBeforeCompact(widen);
    }
    compacting = true;
    ownAbortDue = true;
    const started = pass;
    // The synchronous part of the host's compaction aborts the agent, so the request this hook was
    // called for is never sent; the host resumes the turn once the summary is committed, or at
    // once when there is nothing to compact (`session/session-maintenance.ts:1999-2006, 2428-2437`).
    compact({ origin: "outside", mode: "soft" }).then(
      () => {
        if (started !== pass) return;
        compacting = false;
        deps.log.debug("legion: the cyber_policy ladder compacted the session; the turn resumes");
      },
      (error: unknown) => {
        if (started !== pass) return;
        compacting = false;
        const name = error instanceof Error ? error.name : undefined;
        if (name === NOTHING_TO_COMPACT) {
          deps.log.debug(
            "legion: the cyber_policy ladder had nothing to compact; the turn Oh My Pi resumes is the rung's request"
          );
          return;
        }
        if (name === COMPACTION_CANCELLED || name === "AbortError") {
          endPass("its compaction was stopped");
          return;
        }
        deps.log.warn(
          "legion: the cyber_policy ladder's compaction failed; a turn from inside the session goes on",
          { error: messageFor(error) }
        );
        startHeldTurn();
      }
    );
  };

  return {
    settle: async (event, ctx) => {
      const started = pass;
      const last = event.last_assistant_message;
      if (assistantRefusal(last) === undefined) {
        endPass("the turn ended with no refusal");
        return undefined;
      }
      if (!(await classify(deps.cyberPolicyRefusal, last))) {
        endPass("the turn ended in another provider error");
        return undefined;
      }
      // A pass starts on OpenAI's own refusal only. A call the hold held is no new refusal: after a
      // spent pass it is the hold doing its work, and the turn settles.
      if (next === 0 && (spent || !(await classify(deps.recordableRefusal, last)))) {
        return undefined;
      }
      // A host with no compaction for extensions has no compacted rung: it is passed over.
      if (CYBER_POLICY_RUNGS[next] === "compact" && ctx.compact === undefined) {
        deps.log.warn(
          "legion: this host has no compaction for extensions; the ladder passes over it"
        );
        next += 1;
      }
      const rung = CYBER_POLICY_RUNGS[next];
      if (rung === undefined) {
        exhaust();
        return undefined;
      }
      next += 1;
      let target: HostModel | undefined;
      if (rung === "weaker-model") {
        target = await deps.sameFamilyFallback(ctx);
        if (target === undefined) {
          exhaust();
          return undefined;
        }
      }
      // Oh My Pi drops this handler's answer at 30 s without aborting it (`SETTLE_BUDGET_MS`).
      const signal = AbortSignal.any([event.signal, AbortSignal.timeout(SETTLE_BUDGET_MS)]);
      /** The rung is given up: the settle pass was aborted or ran past its budget, or the pane
       * moved to another session, whose pass this one must leave alone. */
      const giveUp = (): undefined => {
        if (started === pass) endPass("the settle pass was aborted or ran past its budget");
        return undefined;
      };
      if (!(await deps.wait(deps.settings.waitsMs[rung], signal))) return giveUp();
      if (signal.aborted || started !== pass) return giveUp();
      if (target !== undefined) {
        if (!(await deps.stepDownTo(ctx, target))) {
          exhaust();
          return undefined;
        }
        if (signal.aborted || started !== pass) return giveUp();
      }
      endHold();
      deps.log.debug("legion: the cyber_policy ladder climbs a rung", { rung });
      if (rung === "compact") compactDue = true;
      return {
        continue: true,
        additionalContext: rung === "message" ? deps.settings.message : CYBER_POLICY_CONTINUE,
      };
    },
    request: async (messages, ctx) => {
      if (compactDue) {
        compactDue = false;
        startCompaction(ctx);
      }
      const kept: unknown[] = [];
      for (const message of messages) {
        if (isLadderMessage(message)) continue;
        if (
          assistantRefusal(message) !== undefined &&
          !assistantRanTool(message) &&
          (await classify(deps.cyberPolicyRefusal, message))
        ) {
          continue;
        }
        kept.push(message);
      }
      return kept.length === messages.length ? undefined : kept;
    },
    turnEnded: (message) => {
      const reason = message?.stopReason;
      if (reason === "aborted") {
        // The ladder's compaction aborts the turn it was asked from: that abort is the rung's own.
        if (ownAbortDue) {
          ownAbortDue = false;
          return;
        }
        endPass("the turn was aborted");
        return;
      }
      if (reason === "stop" || reason === "toolUse" || reason === "length") {
        spent = false;
        ownAbortDue = false;
        endPass("the provider answered");
      }
    },
    reset: () => {
      next = 0;
      spent = false;
      compactDue = false;
      compacting = false;
      ownAbortDue = false;
      pass += 1;
    },
  };
}

/**
 * Widens a compaction to the branch's leaf: every message the host would keep verbatim — the
 * newest turn group, which holds the request that was refused — is summarized with the rest, and
 * the first entry kept is the leaf itself, the refused reply the next request leaves out
 * (`CyberPolicyLadder.request`). The host keeps the newest group whatever its keep budget says
 * (`packages/agent/src/compaction/compaction.ts:516-519`), so this is the one way to have it
 * summarized.
 */
export function summarizeThrough(preparation: CompactionPreparation, leafId: string): void {
  const keepsLeaf = preparation.recentEntryIds?.at(-1) === leafId;
  const summarized = keepsLeaf
    ? preparation.recentMessages.slice(0, -1)
    : preparation.recentMessages;
  preparation.messagesToSummarize = [
    ...preparation.messagesToSummarize,
    ...preparation.turnPrefixMessages,
    ...summarized,
  ];
  preparation.turnPrefixMessages = [];
  preparation.recentMessages = keepsLeaf ? preparation.recentMessages.slice(-1) : [];
  preparation.recentEntryIds = keepsLeaf ? [leafId] : [];
  preparation.isSplitTurn = false;
  preparation.firstKeptEntryId = leafId;
}
