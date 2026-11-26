import type { LegionTurnOutcomeRequestBody } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  type ClaimCaller,
  LegionDaemonApiError,
  type LegionDaemonClient,
  sendUntilAnswered,
} from "@legion/pi-shared/daemon-client";
import { assistantRefusal } from "@legion/pi-shared/phase-stall";
import type { AgentEndEvent, AgentEndMessage } from "@legion/pi-shared/pi-types";

/**
 * The Oh My Pi half of the turn-outcome report (INFRA-714). Every turn this pane ends — one the
 * model answered, and one the provider refused — is reported to the daemon over HTTP, under a
 * sequence this plugin counts itself, so the daemon, not a reminder, answers a session whose
 * provider refuses turn after turn (dispatch://INFRA-264). A refusal this plugin reports carries
 * the provider's failure (`providerError`, below), with the HTTP status the provider answered
 * where the host recorded one. Where the report is of the turn of the task a take of the claim
 * holds, the daemon's provider-error ladder takes it: the task is kept and given to the same
 * session again after a wait, or, for a client error the ladder cannot wait out (a 4xx but 401,
 * 403, 408 and 429), the claim fails and the workflow holds the issue. Every other refused turn —
 * a reminder's, a card's or a background result's, whose report names no task — is judged from
 * its report alone (dispatch://INFRA-4508): a status that cannot clear fails the claim and holds
 * the issue, and any other counts in the claim's run of provider-error turns. No run of refused
 * turns fails a claim. Naming no task is how this plugin says a turn was not the take's, so it
 * sends neither the `unprompted` mark nor an `httpStatus` of its own beside `providerError`, which
 * a plugin from before the task's number sends (below the contract of the daemon API's task-take
 * entry, `contract_20261125171504_task-take.go`). A refusal Oh My
 * Pi goes on from, a step of its own retry or model-fallback walk (`turnGoesOn`, `./turn-end`),
 * ends no turn, so it is not reported: the `agent_end` entry of `extensions/legion.ts` hears it
 * neither here nor in the claim's take, and the end of the attempt the host goes on to is the
 * turn's end, reported in its turn.
 *
 * A refused turn the host classes as OpenAI's cyber-policy refusal carries `code: "cyber_policy"`,
 * the take's turn and every other turn alike. The class is the host's own: its
 * `isCyberPolicyRefusal` (`@oh-my-pi/pi-ai/error`), handed the assistant message that ended the
 * turn, as the host hands it the same message to set a `prompt_result`'s `code`; this plugin reads
 * none of the provider's words itself. The status says nothing the daemon can act on there — a
 * gateway relays the refusal as a 503, and a request the host held after one has none — so the
 * class is what fails the claim, under its own cause, before the next take, input from outside the
 * session, ends the host's hold and sends the refused content to OpenAI again; the report of the
 * take's turn has the daemon take that task back first. The claim's take hears the turn's end
 * before this report goes (`extensions/legion.ts`), so a take it asks at that end, of a task the
 * claim was given while the turn ran, can still start a turn before the report lands; that turn's
 * own report names its number and the class, which fails the claim, so the race costs one take at
 * most (`packages/daemon/internal/supervise/providererror.go`'s codeCyberPolicy).
 *
 * The count is this producer's alone. It is not the daemon's turn ordinal and not
 * `supervise.StreamTurnEnd`'s: two counters would drift the moment one of them missed an end, and
 * only one of them can own the number a report is keyed by. It starts at 1 within a claim's
 * launch generation, which a pane holds for its whole life, and is taken before the report is
 * sent, so reports that overlap keep the order their turns ended in whatever order they arrive —
 * the daemon keys each outcome by `(generation, sequence)` and derives the trailing run of
 * refusals afresh, so a late report lands in its own place and nothing is ever recounted.
 *
 * The report of a task's turn — the first run after an accepted take (`@legion/pi-shared/task-take`)
 * — names the task's number (`task`), which is what finishes the task on the daemon: for a worker
 * on the current contract nothing else does (the plan's decision 8). A turn the provider failed
 * also carries the failure (`providerError`): the HTTP status of the request it failed on, the
 * provider and model it went to, and the provider's message, with which the daemon keeps the task
 * under its number and writes its value again after a wait instead of finishing it
 * (dispatch://INFRA-2811 R2). The daemon-launched controller's turns are reported the same way,
 * its start task's naming that task's number. A turn that ended while the claim's take still held
 * an accepted take's text unrun — the take was answered during that turn, which the daemon gave
 * it as the first to start after its accept — names that take's number as `held`
 * (`TaskTake.held`): the turn was not the task's, which the daemon reads as certain and puts the
 * take back to await its own turn by, even where it has lost its count of this session's turns.
 *
 * Because only the report finishes the task, a report is sent until the daemon answers it
 * (`sendUntilAnswered`, the policy a take is sent by): any failure that carries no HTTP status,
 * whatever the transport names it (a refused, reset or closed connection, an unreachable host or
 * network, a connect or read timeout, a DNS failure among them), no answer within five minutes,
 * and a 502, 503 or 504 that names no Legion error code are sent again, the same request, after a
 * doubling wait with no attempt limit, and so is any 500 the route answers, which is the daemon's
 * own failure to record the report (`turn outcome failed: the daemon could not record it`). No
 * list of the transport's codes decides it: a report a failure stopped would leave its task's turn
 * started on the daemon, which arms no bound over it, while the take holds the number finished
 * from the turn's end, so nothing would finish the task or take it again until a newer task or
 * the process's end. The daemon stores one outcome per `(generation, sequence)` and applies a
 * repeat once, so a report sent again whose first attempt landed changes nothing, and one whose
 * write failed is applied. A 2xx ends the sending, and so does a refusal
 * (a 4xx, or any other answer the daemon names a code for), which is warned about once: nothing
 * this pane can do changes it. The session's end stops what is still being sent for it
 * (`sessionEnded`). The report resolves how the daemon answered it (`TurnReportAnswer`): a 4xx
 * applied nothing, so a task the report named is still owed, and the claim's take, which held
 * that number finished from the turn's end, holds it finished no longer (`TaskTake.reportRefused`
 * in `@legion/pi-shared/task-take`, which `extensions/legion.ts` calls).
 *
 * Claude Code's half of the same report is `packages/daemon/internal/claudebridge`: neither half
 * is a shimwire frame, because a plugin cannot put one on the shim stream and `shimwire.AgentEnd`
 * carries the whole chunked transcript.
 */

export interface TurnOutcomeLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

/** The claim a turn's report speaks for: the agent form the report names it in, and the role and
 * issue its lines name it by (the controller's claim is on no issue). */
export interface ReportedClaim {
  readonly caller: ClaimCaller;
  readonly role: string;
  readonly issue?: string;
}

export interface TurnOutcomeReporterDeps {
  /** The pane's daemon client, built lazily by the entry that owns it. */
  readonly daemon: () => LegionDaemonClient;
  /** The claim the session with that id reports for, or undefined: a session before its
   * registration, an operator's controller, a person's own pane, and a `task` subagent, which
   * loads an instance of its own and registers nothing. */
  readonly claim: (sessionID: string) => ReportedClaim | undefined;
  readonly log: TurnOutcomeLog;
  /** The host's own cyber-policy classifier (`isCyberPolicyRefusal` of `@oh-my-pi/pi-ai/error`),
   * asked about the assistant message that ended a refused turn. The entry reads the host's module
   * lazily, so the answer is a promise, and a failure to read it is this reporter's to absorb. */
  readonly cyberPolicyRefusal: (message: unknown) => Promise<boolean>;
  /** The wait between a report's attempts, and each attempt's bound: a timer by default; a test
   * passes its own clock. */
  readonly wait?: (ms: number, signal: AbortSignal) => Promise<boolean>;
}

/**
 * How the daemon answered a turn's report (`TurnOutcomeReporter.report`): `recorded`, a 2xx;
 * `refused`, a 4xx, coded or not, which is not sent again and applied nothing, so a task the
 * report named is still owed.
 */
export type TurnReportAnswer = "recorded" | "refused";

export interface TurnOutcomeReporter {
  /**
   * Reports the turn that `agent_end` just ended, for the claim the session holds; `task` is the
   * number of the task whose turn it was, undefined for a turn of no task, and `held` the number
   * of the accepted take whose text the session's take held unrun through that turn
   * (`TaskTake.held`), undefined for none, which the report names as `held` so the daemon knows
   * for certain the turn was not that task's. Resolves how the daemon answered
   * (`TurnReportAnswer`) once it answered or refused, and undefined for a session that holds no
   * claim, a session that ended first, and a failure that is neither a 2xx nor a 4xx. Never
   * rejects: Oh My Pi does not await an `agent_end` handler, so a rejection here would be an
   * unhandled one, and nothing of the turn waits on the report either way.
   */
  readonly report: (
    sessionID: string,
    event: AgentEndEvent,
    task?: number,
    held?: number
  ) => Promise<TurnReportAnswer | undefined>;
  /** The session ended (`session_shutdown`): every report of it still being sent stops. */
  readonly sessionEnded: (sessionID: string) => void;
}

/**
 * The provider failure an `error` stop carries, as the turn-outcome report names it: the HTTP
 * status of the failed request (0 where it had none, as a content filter's refusal has none, and
 * for a number that is no HTTP status, 100 to 599, which the host's field can hold), the provider
 * and the model, the provider's message, and whether this plugin reads the status as a failure
 * that may clear: a 408 or a 429, a provider's "not now" its own retries may still get through,
 * or a 5xx. The daemon decides by the status and the report's `code` (`cyberPolicy`, below). No
 * status is ever read out of the words.
 */
function providerError(
  message: AgentEndMessage,
  refusal: string
): NonNullable<LegionTurnOutcomeRequestBody["providerError"]> {
  const recorded = message.errorStatus;
  const status =
    typeof recorded === "number" && Number.isInteger(recorded) && recorded >= 100 && recorded <= 599
      ? recorded
      : 0;
  return {
    status,
    provider: message.provider ?? "",
    model: message.model ?? "",
    message: refusal,
    retryable: status === 408 || status === 429 || status >= 500,
  };
}

/** The run's last assistant message, and the provider's refusal it ended in, if any. */
function endOf(event: AgentEndEvent): {
  readonly last: AgentEndMessage | undefined;
  readonly refusal: string | undefined;
} {
  const last = event.messages?.findLast((message) => message.role === "assistant");
  return { last, refusal: assistantRefusal(last) };
}

/**
 * Whether the report of the run `event` ended carries the provider's failure (`providerError`).
 * The daemon finishes a current claim's task on a report naming its number only without one, so
 * the claim's take reads this at the run's end (`TaskTake.runEnded`) to know whether that number
 * is finished.
 */
export function reportsProviderError(event: AgentEndEvent): boolean {
  const { last, refusal } = endOf(event);
  return last !== undefined && refusal !== undefined;
}

/** The turn-outcome route's own failure to record a report (`claimFailure` in
 * `packages/daemon/internal/api/claims.go`): a 500, sent again as decision 8 asks. */
const recordFailed = (error: unknown): boolean =>
  error instanceof LegionDaemonApiError && error.status === 500;

export function createTurnOutcomeReporter(deps: TurnOutcomeReporterDeps): TurnOutcomeReporter {
  /** The generation the sequence below counts within, and the last number it gave out. */
  let counting: number | undefined;
  let sequence = 0;
  /** What stops the reports each session is still sending, until that session ends. */
  const stops = new Map<string, AbortController>();
  /** Whether the host classes `message`, the last of a refused turn, as OpenAI's cyber-policy
   * refusal. A host that cannot answer — its module unread, a pin without the classifier — classes
   * it as none: the turn is still reported, with no code, and the pane says why. */
  const classify = async (message: unknown): Promise<boolean> => {
    try {
      return await deps.cyberPolicyRefusal(message);
    } catch (error) {
      deps.log.warn(
        "legion: the host could not class a refused turn; it is reported with no code",
        {
          error: messageFor(error),
        }
      );
      return false;
    }
  };

  return {
    report: async (sessionID, event, task, held) => {
      const claim = deps.claim(sessionID);
      if (claim === undefined) return undefined;
      const { caller } = claim;
      // Taken before the first await, so two turns can never take the same number nor take them
      // out of the order they ended in. A generation this producer has not counted within starts
      // the count again, as a fresh launch's does.
      if (caller.generation !== counting) {
        counting = caller.generation;
        sequence = 0;
      }
      sequence += 1;
      // The number this report owns: `sequence` moves on as the next turn ends, and the lines
      // below are written after an await.
      const mine = sequence;
      const { last, refusal } = endOf(event);
      // The host's class of the refusal, read after the number is taken: the classifier may wait
      // on the host's module, and the turns' order is fixed above.
      const cyberPolicy = refusal !== undefined && (await classify(last));
      const request = {
        ...caller,
        sequence: mine,
        outcome: refusal === undefined ? ("clean" as const) : ("refused" as const),
        ...(refusal === undefined ? {} : { message: refusal }),
        ...(task === undefined ? {} : { task }),
        ...(held === undefined ? {} : { held }),
        ...(refusal === undefined || last === undefined
          ? {}
          : { providerError: providerError(last, refusal) }),
        ...(cyberPolicy ? { code: "cyber_policy" as const } : {}),
      };
      let stop = stops.get(sessionID);
      if (stop === undefined) {
        stop = new AbortController();
        stops.set(sessionID, stop);
      }
      const lines = {
        ...(claim.issue === undefined ? {} : { issue: claim.issue }),
        role: claim.role,
        sequence: mine,
        outcome: request.outcome,
        ...(task === undefined ? {} : { task }),
        ...(held === undefined ? {} : { held }),
        ...(cyberPolicy ? { code: "cyber_policy" as const } : {}),
      };
      try {
        const outcome = await sendUntilAnswered(
          "claims/turn-outcome",
          (signal) => deps.daemon().turnOutcome(request, { signal }),
          { stop: stop.signal, alsoSendAgain: recordFailed, warn: deps.log.warn, wait: deps.wait }
        );
        deps.log.debug("legion: reported a turn's outcome", {
          ...lines,
          refusedRun: outcome.refusedRun,
          failed: outcome.failed,
        });
        return "recorded";
      } catch (error) {
        deps.log.warn("legion: a turn's outcome did not reach the daemon", {
          ...lines,
          error: messageFor(error),
        });
        // A 4xx, coded or not, is not sent again, and the report it refused applied nothing.
        return error instanceof LegionDaemonApiError && error.status >= 400 && error.status < 500
          ? "refused"
          : undefined;
      }
    },
    sessionEnded: (sessionID) => {
      stops.get(sessionID)?.abort();
      stops.delete(sessionID);
    },
  };
}
