import { messageFor } from "@legion/envoy-client/errors";
import { type LegionDaemonClient, retryWhileDaemonRestarts } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import { assistantRefusal } from "@legion/pi-shared/phase-stall";
import type { AgentEndEvent, InputEvent } from "@legion/pi-shared/pi-types";
import { turnGoesOn } from "./turn-end";

/**
 * The Oh My Pi half of the turn-outcome report (INFRA-714). Every turn this pane ends — one the
 * model answered, and one the provider refused — is reported to the daemon over HTTP, under a
 * sequence this plugin counts itself. The daemon derives the claim's run of refused turns from
 * the reports and answers each report with it, which this plugin logs; the run fails no claim
 * and decides nothing (dispatch://INFRA-4508): a retry of the claim resumes its recorded session
 * whatever the run, unless the retry asks for a new one (`freshSession`,
 * `packages/daemon/internal/supervise/turnoutcome.go`).
 *
 * A refused turn's report carries the HTTP status the provider answered, where the host recorded
 * one (`errorStatus` on the assistant message that ended the turn, the field the host's own
 * `prompt_result` takes its `httpStatus` from), and the report of a turn that ran no prompt of the
 * daemon's says so (`unprompted`). Such a turn — a phase reminder this plugin starts itself with
 * `pi.sendMessage`, an Envoy message, a background job's result — has no prompt result, so the
 * daemon judges it from this report alone, as it judges a prompted turn from that prompt's result
 * (dispatch://INFRA-4508's R4): a refusal whose status cannot clear fails the claim and holds the
 * issue, and any other refusal counts in the claim's run of provider-error turns. No status is
 * ever read out of the refusal's words.
 *
 * A refused turn the host classes as OpenAI's cyber-policy refusal carries `code: "cyber_policy"`.
 * The class is the host's own: its `isCyberPolicyRefusal` (`@oh-my-pi/pi-ai/error`), handed the
 * assistant message that ended the turn, as the host hands it the same message to set a
 * `prompt_result`'s `code`; this plugin reads none of the provider's words itself. The status says
 * nothing the daemon can act on there — a gateway relays the refusal as a 503, and a request the
 * host held after one has none — so the class is what fails the claim, under its own cause, before
 * the daemon's next prompt, input from outside the session, ends the host's hold and sends the
 * refused content to OpenAI again. A report and the stream's end of its turn reach the daemon in no
 * fixed order, so one prompt can still go before this report lands; that prompt's own result then
 * names the class and fails the claim, so the race costs one send at most
 * (`packages/daemon/internal/supervise/providererror.go`'s codeCyberPolicy). A prompt's turn
 * carries the class too, and the daemon reads it there not at all: that prompt's result judges it.
 *
 * Which turns a prompt ran is read the way the host decides which run a `prompt_result` reports
 * (`RpcPromptResults`, `packages/coding-agent/src/modes/rpc/rpc-prompt-results.ts:164-169` at the
 * pinned tag): a prompt the daemon sent arrives as `input` with `source: "rpc"` — the daemon is
 * this pane's one RPC client — and its result reports the first run that starts after it, up to
 * the first end the host does not go on from itself (`turnGoesOn`, the host's own "yielded"),
 * whatever message waits to run next. Every report from that run's start to that end is a
 * prompted one, which the result alone judges, whichever of the two reaches the daemon first; a
 * run a waiting message starts after that end, an Envoy steer among them, is no prompt's, and its
 * report goes as unprompted. This reading can part from the host's in two windows. A prompt the
 * host refused as busy after its `input` leaves the next run taken for that prompt's: that report
 * goes as prompted, so the turn goes uncounted rather than counted twice. A run that starts
 * between Oh My Pi accepting a prompt and emitting its `input` is reported unprompted, and is also
 * judged by the prompt's result; where the prompt's result reports that run, the late `input`
 * leaves the run after it read as prompted, and that run goes unjudged.
 *
 * The count is this producer's alone. It is not the daemon's turn ordinal and not
 * `supervise.StreamTurnEnd`'s: two counters would drift the moment one of them missed an end, and
 * only one of them can own the number a report is keyed by. It starts at 1 within a claim's
 * launch generation, which a pane holds for its whole life, and is taken before the report is
 * sent, so reports that overlap keep the order their turns ended in whatever order they arrive —
 * the daemon keys each outcome by `(generation, sequence)` and derives the trailing run of
 * refusals afresh, so a late report lands in its own place and nothing is ever recounted.
 *
 * Claude Code's half of the same report is `packages/daemon/internal/claudebridge`: neither half
 * is a shimwire frame, because a plugin cannot put one on the shim stream and `shimwire.AgentEnd`
 * carries the whole chunked transcript.
 */

export interface TurnOutcomeLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface TurnOutcomeReporterDeps {
  /** The pane's daemon client, built lazily by the entry that owns it. */
  readonly daemon: () => LegionDaemonClient;
  /** The claim the session with that id registered, or undefined: a session before its
   * registration, the controller, a person's own pane, and a `task` subagent, which loads an
   * instance of its own and registers nothing. */
  readonly claim: (sessionID: string) => ClaimCapability | undefined;
  readonly log: TurnOutcomeLog;
  /** The host's own cyber-policy classifier (`isCyberPolicyRefusal` of `@oh-my-pi/pi-ai/error`),
   * asked about the assistant message that ended a refused turn. The entry reads the host's module
   * lazily, so the answer is a promise, and a failure to read it is this reporter's to absorb. */
  readonly cyberPolicyRefusal: (message: unknown) => Promise<boolean>;
}

export interface TurnOutcomeReporter {
  /** Input is about to run in the session (`input`): one with `source: "rpc"` is a prompt the
   * daemon sent, whose result reports the next run to start. */
  readonly inputArrived: (sessionID: string, event: InputEvent) => void;
  /** A run started in the session (`agent_start`). */
  readonly runStarted: (sessionID: string) => void;
  /**
   * Reports the turn that `agent_end` just ended, for the claim the session holds. Never rejects:
   * Oh My Pi does not await an `agent_end` handler, so a rejection here would be an unhandled one,
   * and nothing of the turn waits on the report either way.
   */
  readonly report: (sessionID: string, event: AgentEndEvent) => Promise<void>;
}

export function createTurnOutcomeReporter(deps: TurnOutcomeReporterDeps): TurnOutcomeReporter {
  /** The generation the sequence below counts within, and the last number it gave out. */
  let counting: number | undefined;
  let sequence = 0;
  /** A prompt the daemon sent has arrived and no run has started since: the next run is its. */
  let promptOwed = false;
  /** The run going now is a prompt's, up to the end that prompt's result reports. */
  let prompted = false;
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
    inputArrived: (sessionID, event) => {
      if (deps.claim(sessionID) === undefined) return;
      if (event.source === "rpc") promptOwed = true;
    },
    runStarted: (sessionID) => {
      if (deps.claim(sessionID) === undefined) return;
      if (promptOwed) {
        prompted = true;
        promptOwed = false;
      }
    },
    report: async (sessionID, event) => {
      const claim = deps.claim(sessionID);
      if (claim === undefined) return;
      const last = event.messages?.findLast((message) => message.role === "assistant");
      const refusal = assistantRefusal(last);
      // A refusal Oh My Pi goes on from ends no turn (`turnGoesOn`): the host has already scheduled
      // a retry or the next model of its fallback chain, and the end of that attempt is the turn's
      // end, reported in its turn. Counted, each fallback step was one more refused turn of the run
      // (dispatch://INFRA-4508). An end that waits only on a background job's result has settled,
      // so it is reported; so is an answered end the host goes on from, which ends the run of
      // refusals before it.
      if (refusal !== undefined && turnGoesOn(event)) return;
      const unprompted = !prompted;
      // The end a prompt's result reports is the first the host does not go on from, whatever
      // waits to run next (rpc-prompt-results.ts:164-169 at the pinned tag); a run after it is no
      // prompt's until another prompt arrives.
      if (!turnGoesOn(event)) prompted = false;
      // Taken before the first await, so two turns can never take the same number nor take them
      // out of the order they ended in. A generation this producer has not counted within starts
      // the count again, as a fresh launch's does.
      if (claim.generation !== counting) {
        counting = claim.generation;
        sequence = 0;
      }
      sequence += 1;
      // The number this report owns: `sequence` moves on as the next turn ends, and the lines
      // below are written after an await.
      const mine = sequence;
      // The status the host recorded on the refused message, and none for a number that is no
      // HTTP status: the contract admits 100 to 599 alone (`LegionTurnOutcomeRequest`), and the
      // daemon takes any other number as no status.
      const recorded = refusal === undefined ? undefined : last?.errorStatus;
      const status =
        typeof recorded === "number" &&
        Number.isInteger(recorded) &&
        recorded >= 100 &&
        recorded <= 599
          ? recorded
          : undefined;
      // The host's class of the refusal, read after the number is taken: the classifier may wait
      // on the host's module, and the turns' order is fixed above.
      const cyberPolicy = refusal !== undefined && (await classify(last));
      const request = {
        claimToken: claim.claimToken,
        sessionId: claim.sessionID,
        secret: claim.secret,
        generation: claim.generation,
        sequence: mine,
        outcome: refusal === undefined ? ("clean" as const) : ("refused" as const),
        ...(refusal === undefined ? {} : { message: refusal }),
        ...(status === undefined ? {} : { httpStatus: status }),
        ...(unprompted ? { unprompted: true } : {}),
        ...(cyberPolicy ? { code: "cyber_policy" as const } : {}),
      };
      try {
        // A send that fails on nothing but a restarting daemon is retried until it lands: the
        // daemon stores one outcome per `(generation, sequence)`, so a repeat of this report
        // writes the same cell and changes no count, and a turn that goes unreported is a turn
        // the daemon counts neither way — a refusal silently dropped is a run of refusals that
        // comes up short. A refusal the daemon itself gives (any answer in its refusal shape: a
        // 4xx, or a 5xx such as a turn outcome it could not record) is not retried: it is warned
        // about once and the turn goes uncounted, since nothing this pane can do fixes it.
        const outcome = await retryWhileDaemonRestarts("claims/turn-outcome", () =>
          deps.daemon().turnOutcome(request)
        );
        deps.log.debug("legion: reported a turn's outcome", {
          issue: claim.issue,
          role: claim.role,
          sequence: mine,
          outcome: request.outcome,
          httpStatus: status,
          code: request.code,
          unprompted,
          refusedRun: outcome.refusedRun,
          failed: outcome.failed,
        });
      } catch (error) {
        deps.log.warn("legion: a turn's outcome did not reach the daemon", {
          issue: claim.issue,
          role: claim.role,
          sequence: mine,
          outcome: request.outcome,
          error: messageFor(error),
        });
      }
    },
  };
}
