import {
  LEGION_MAX_MODEL_REQUESTS,
  LegionModelRequest,
  type LegionModelRequestBody,
} from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  daemonUnreachable,
  LegionDaemonApiError,
  type LegionDaemonClient,
} from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndMessage } from "@legion/pi-shared/pi-types";

/**
 * The Oh My Pi half of the report of model requests (`POST /legion/v1/claims/model-requests`,
 * dispatch://INFRA-5899): each model request a session of this process makes — the role's own,
 * each subagent's it starts, and the controller's — is reported to the daemon with the moment it
 * ended and its uncached input tokens, which the provider's headroom counts.
 *
 * **Which host event carries a request.** Measured on the host in RPC mode against an
 * Anthropic-shaped stand-in, with an extension that logged every event: each request the session's
 * agent loop sends ends in exactly one `message_end` of an assistant message, which carries the
 * request's `usage` (`input`, `cacheRead`, `cacheWrite`), its `model`, the provider's `responseId`
 * and the moment the session stamped it ended (`completedAt`). A request a limit
 * answered with HTTP 429 that the session retries itself ends in its own `message_end` too —
 * `stopReason: "error"`, `errorStatus: 429`, no `responseId`, every usage member 0 — before the
 * host's `auto_retry_start`, and its retry ends in a `message_end` of its own. A `task` subagent
 * loads its own extension instance. The `message_end` hook identifies subagent events by their
 * context and reports them under the pane's credential, if available. Oh My Pi's `message_end`
 * does not report a request made outside the agent loop — a title, a classifier, a compaction's
 * summary, a memory extraction — so the daemon's tokens do not count those.
 *
 * **What a request is.** A `message_end` of an assistant message the provider answered: one with
 * the provider's response id or tokens. A message with neither is no request, whatever its status:
 * a message the host wrote itself — a gate's stop, a request that never reached the provider — and
 * a request a limit answered with HTTP 429, which counted no token, are not reported, as the
 * Claude Code bridge reports neither its `api_retry` line nor its synthetic answer. Its uncached
 * input is `input + cacheWrite`, never `cacheRead`. Every request is reported alike, with nothing
 * of its answer.
 *
 * **When it is sent.** One pending batch per reporter. One timer is armed for the earliest
 * deadline pending. A request's deadline is its end plus `MODEL_REQUEST_DUE_AFTER_MS`, which is
 * `MODEL_REQUEST_SEND_MARGIN_MS` short of `MODEL_REQUEST_REPORT_BOUND_MS`: a timer fires late,
 * never early, and the call that carries the report starts only once it has fired, so a timer
 * armed for the bound itself would send the report after it. A request that joins the batch moves
 * no request's deadline: it moves the timer only where its own deadline comes before the one the
 * timer is armed for, as a request read late does, which then goes by its own deadline. The turn's
 * end (`agent_end`, and `session_shutdown`) sends what is pending at once and drops the timer,
 * whatever failed before. A send the timer or the turn's end makes goes at once, whatever report
 * is on its way, and waits for no answer: each report is a call of its own, aborted where the
 * daemon has not answered it within `MODEL_REQUEST_REPORT_TIMEOUT_MS`, 10 s, and carries only
 * requests that no report on its way carries, at most `LEGION_MAX_MODEL_REQUESTS` of them and all
 * of one owner. What a full report leaves, and another owner's requests, go at once too, each in a
 * call of its own, so every request pending when a send falls due goes then. Reports so cross, and
 * their answers arrive in either order: each answer puts back only its own report's requests, and
 * the daemon takes a request again by its id and keeps the later end, so a crossing changes
 * nothing it decides. A request the daemon's answer does not name as held goes again under the
 * same id. A failed call holds the requests it put back — whether the daemon was unreachable or
 * failing, gave no answer within the 10 s, or the call failed before it left: each is due a bound
 * after that call ends, and goes then or with an earlier send, which takes everything pending,
 * while every other request keeps its own deadline. Only a deadline or the turn's end makes a
 * send, and a call's end only arms the timer, so a failing daemon is never called in a loop. A
 * request the contract refuses is dropped at `messageEnded` with a warning, so it never shares a
 * batch. A request older than an hour is dropped with a warning. A report the daemon refuses (a
 * 4xx) is dropped with a warning, as a turn outcome's is.
 * Nothing of the session waits on any of it: the hooks return at once.
 *
 * The turn's outcome report (`./turn-outcome`) is a separate route and is unchanged by this one.
 * Claude Code's half is `packages/daemon/internal/claudebridge/modelrequests.go`.
 */

/** How long after a request's end its report may wait, whatever report is on its way ("When it is
 * sent", above): a quarter of the minute the headroom reads, so the timer sends about four times
 * a minute (spec, "The model requests"). */
export const MODEL_REQUEST_REPORT_BOUND_MS = 15_000;

/** How much sooner than `MODEL_REQUEST_REPORT_BOUND_MS` after a request's end its report falls
 * due, for the reason "When it is sent" (above) gives. The Claude Code bridge leaves the same
 * room, `requestSendMargin` in `packages/daemon/internal/claudebridge/modelrequests.go`, so both
 * hosts state one rule. */
export const MODEL_REQUEST_SEND_MARGIN_MS = 250;

/** How long after a request's end its report falls due ("When it is sent", above), the value the
 * Claude Code bridge names `requestDueAfter`. */
export const MODEL_REQUEST_DUE_AFTER_MS =
  MODEL_REQUEST_REPORT_BOUND_MS - MODEL_REQUEST_SEND_MARGIN_MS;

/** How long one report's call may go unanswered before it is aborted, a failed call ("When it is
 * sent", above). The Claude Code bridge bounds its call alike, `requestReportTimeout` in
 * `packages/daemon/internal/claudebridge/modelrequests.go`. */
export const MODEL_REQUEST_REPORT_TIMEOUT_MS = 10_000;

/** Requests older than the daemon's hour of history are not useful on a retry. */
const MODEL_REQUEST_KEEP_MS = 60 * 60_000;

/** The claim a request is reported under: a role's registration, or the controller's. */
export type ModelRequestCredential = Pick<
  ClaimCapability,
  "claimToken" | "sessionID" | "secret" | "generation"
>;

export interface ModelRequestsLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface ModelRequestsReporterDeps {
  readonly daemon: () => LegionDaemonClient;
  /** The claim a top-level session of this process holds — a role's registration or the
   * controller's — and undefined for any other session. */
  readonly credential: (sessionID: string) => ModelRequestCredential | undefined;
  /** The top-level session the pane is on, whose claim a subagent's requests are reported under. */
  readonly paneSession: () => string | undefined;
  readonly log: ModelRequestsLog;
  readonly now: () => number;
  /** Runs `callback` once `delayMs` has passed and answers what cancels it. */
  readonly schedule: (callback: () => void, delayMs: number) => () => void;
  /** A fresh id, never given out before. */
  readonly newId: () => string;
}

export interface ModelRequestsReporter {
  /** An assistant message ended (`message_end`) in `session`: a subagent's when `subagent`. */
  readonly messageEnded: (
    where: { readonly session: string; readonly subagent: boolean },
    message: unknown
  ) => void;
  /** A run ended (`agent_end`): what is pending is sent now. */
  readonly turnEnded: () => void;
  /** Resolves once no report is on its way. */
  readonly settled: () => Promise<void>;
}

/** One request waiting for the daemon to hold it, and when it is due to be sent at the latest. */
interface Pending {
  readonly owner: ModelRequestCredential;
  readonly request: LegionModelRequestBody;
  due: number;
}

/** Whether `message` is an assistant message, the one kind that ends a request. */
function isAssistantMessage(message: unknown): message is AgentEndMessage {
  return (
    typeof message === "object" &&
    message !== null &&
    "role" in message &&
    message.role === "assistant"
  );
}

/** A count of tokens the host reported, and 0 for one it did not. */
function tokens(value: number | undefined): number {
  return typeof value === "number" && Number.isInteger(value) && value > 0 ? value : 0;
}

export function createModelRequestsReporter(
  deps: ModelRequestsReporterDeps
): ModelRequestsReporter {
  let pending: Pending[] = [];
  /** The reports on their way, each until its answer is handled. A send waits for none of them. */
  const onTheWay = new Set<Promise<void>>();
  let cancelTimer: (() => void) | undefined;
  /** When the armed timer fires, while `cancelTimer` is set. */
  let armedAt = 0;

  /** Arms the timer for the earliest deadline pending: again, where a timer is armed for later. */
  const arm = (): void => {
    const first = earliestPending();
    if (first === undefined) return;
    if (cancelTimer !== undefined && armedAt <= first.due) return;
    disarm();
    armedAt = first.due;
    cancelTimer = deps.schedule(
      () => {
        cancelTimer = undefined;
        send();
      },
      Math.max(0, first.due - deps.now())
    );
  };

  const disarm = (): void => {
    cancelTimer?.();
    cancelTimer = undefined;
  };

  const sameOwner = (a: ModelRequestCredential, b: ModelRequestCredential): boolean =>
    a.claimToken === b.claimToken &&
    a.sessionID === b.sessionID &&
    a.secret === b.secret &&
    a.generation === b.generation;

  const earliestPending = (): Pending | undefined => {
    let first: Pending | undefined;
    for (const entry of pending) {
      if (first === undefined || entry.due < first.due) first = entry;
    }
    return first;
  };

  /** Sends every pending request now, as "When it is sent" (above) says, the earliest deadline's
   * report first, each in a call of its own (`report`). It takes every batch out of `pending`
   * before it reports any: a call that fails before it leaves puts its batch back at once, and this
   * send must not take that batch again. */
  const send = (): void => {
    const now = deps.now();
    disarm();
    const cutoff = now - MODEL_REQUEST_KEEP_MS;
    pending = pending.filter(({ owner, request }) => {
      if (Date.parse(request.endedAt) >= cutoff) return true;
      deps.log.warn("legion: dropped a model request past the daemon's keep window", {
        claimToken: owner.claimToken,
        id: request.id,
        endedAt: request.endedAt,
      });
      return false;
    });
    const batches: { readonly owner: ModelRequestCredential; readonly batch: Pending[] }[] = [];
    for (let first = earliestPending(); first !== undefined; first = earliestPending()) {
      const batch: Pending[] = [first];
      const rest: Pending[] = [];
      for (const entry of pending) {
        if (entry === first) continue;
        if (batch.length < LEGION_MAX_MODEL_REQUESTS && sameOwner(entry.owner, first.owner)) {
          batch.push(entry);
        } else {
          rest.push(entry);
        }
      }
      pending = rest;
      batches.push({ owner: first.owner, batch });
    }
    for (const { owner, batch } of batches) report(owner, batch);
  };

  /** Puts `batch`, which no other report carries, on its way in a call of its own. The call's end
   * sends nothing: it arms the timer for what is pending. */
  const report = (owner: ModelRequestCredential, batch: readonly Pending[]): void => {
    const onItsWay: Promise<void> = post(owner, batch).then(() => {
      onTheWay.delete(onItsWay);
      arm();
    });
    onTheWay.add(onItsWay);
  };

  /** Makes one report of `batch`, in a call aborted at `MODEL_REQUEST_REPORT_TIMEOUT_MS` on the
   * reporter's clock, and puts back under the same id each request of it the daemon did not say it
   * holds, due a bound after the call ends, but none of a report the daemon refused (a 4xx). Its
   * answer can cross another report's ("When it is sent", above). */
  const post = async (owner: ModelRequestCredential, batch: readonly Pending[]): Promise<void> => {
    const call = new AbortController();
    const cancelTimeout = deps.schedule(() => {
      call.abort(
        new Error(`the daemon gave no answer within ${MODEL_REQUEST_REPORT_TIMEOUT_MS / 1_000} s`)
      );
    }, MODEL_REQUEST_REPORT_TIMEOUT_MS);
    let kept: ReadonlySet<string> | undefined;
    try {
      const answer = await deps.daemon().modelRequests(
        {
          claimToken: owner.claimToken,
          sessionId: owner.sessionID,
          secret: owner.secret,
          generation: owner.generation,
          requests: batch.map((entry) => entry.request),
        },
        { signal: call.signal }
      );
      kept = new Set(answer.kept);
      deps.log.debug("legion: reported model requests", {
        claimToken: owner.claimToken,
        reported: batch.length,
        kept: kept.size,
      });
    } catch (error) {
      if (
        error instanceof LegionDaemonApiError &&
        error.status >= 400 &&
        error.status < 500 &&
        daemonUnreachable(error) === undefined
      ) {
        // The daemon's own refusal will not pass on a retry: a stale generation, a secret it no
        // longer holds, a body it cannot read. Nothing this pane can do fixes it. A proxy's 408 or
        // 429 in front of the daemon is an outage (`daemonUnreachable`), and goes again.
        deps.log.warn("legion: the daemon refused a report of model requests; they go unreported", {
          claimToken: owner.claimToken,
          requests: batch.length,
          error: messageFor(error),
        });
        return;
      }
      deps.log.warn("legion: a report of model requests did not reach the daemon; it goes again", {
        claimToken: owner.claimToken,
        requests: batch.length,
        error: messageFor(error),
      });
    } finally {
      cancelTimeout();
    }
    const again = deps.now() + MODEL_REQUEST_REPORT_BOUND_MS;
    const unheld = batch.filter((entry) => kept?.has(entry.request.id) !== true);
    for (const entry of unheld) entry.due = again;
    pending = [...unheld, ...pending];
  };

  return {
    messageEnded: (where, message) => {
      if (!isAssistantMessage(message)) return;
      const ownerSession = where.subagent ? deps.paneSession() : where.session;
      const owner = ownerSession === undefined ? undefined : deps.credential(ownerSession);
      if (owner === undefined) return;
      const usage = message.usage;
      const counted =
        tokens(usage?.input) +
        tokens(usage?.cacheWrite) +
        tokens(usage?.cacheRead) +
        tokens(usage?.output);
      if (message.responseId === undefined && counted === 0) return;
      if (message.model === undefined || message.model === "") return;
      const endedAt = message.completedAt ?? deps.now();
      const id = deps.newId();
      const endedDate = new Date(endedAt);
      const parsed = LegionModelRequest.safeParse({
        id,
        session: where.session,
        model: message.model,
        endedAt: Number.isNaN(endedDate.getTime()) ? undefined : endedDate.toISOString(),
        ...(usage === undefined
          ? {}
          : { uncachedInputTokens: tokens(usage.input) + tokens(usage.cacheWrite) }),
      });
      if (!parsed.success) {
        deps.log.warn("legion: dropped a model request the contract refuses", {
          claimToken: owner.claimToken,
          id,
          reason: parsed.error.issues
            .map((issue) => `${issue.path.join(".") || "(root)"}: ${issue.message}`)
            .join("; "),
        });
        return;
      }
      pending.push({ owner, request: parsed.data, due: endedAt + MODEL_REQUEST_DUE_AFTER_MS });
      arm();
    },
    turnEnded: () => {
      send();
    },
    settled: async () => {
      while (onTheWay.size > 0) await Promise.all(onTheWay);
    },
  };
}
