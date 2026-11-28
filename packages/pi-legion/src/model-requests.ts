import type { LegionModelRequestBody } from "@legion/contracts/legion-api";
import { LEGION_MAX_MODEL_REQUESTS } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  LegionDaemonApiError,
  type LegionDaemonClient,
  retryWhileDaemonRestarts,
} from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { AgentEndMessage } from "@legion/pi-shared/pi-types";

/**
 * The Oh My Pi half of the report of model requests (`POST /legion/v1/claims/model-requests`,
 * dispatch://INFRA-5899): each model request a session of this process makes — the role's own,
 * each subagent's it starts, and the controller's — is reported to the daemon with the moment it
 * ended and its uncached input tokens, which the provider's headroom counts.
 *
 * **Which host event carries a request.** Measured on the pinned host
 * (`v18.18.6-sami.20261125-164517`) in RPC mode against an Anthropic-shaped stand-in, with an
 * extension that logged every event: each request the session's agent loop sends ends in exactly
 * one `message_end` of an assistant message, which carries the request's `usage` (`input`,
 * `cacheRead`, `cacheWrite`), its `model`, the provider's `responseId` and the moment the session
 * stamped it ended (`completedAt`, `session/agent-session.ts:5807-5810`). A request a limit
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
 * deadline pending, and never for before a failed call's hold ends. A request's deadline is its
 * end plus `MODEL_REQUEST_REPORT_BOUND_MS` less `MODEL_REQUEST_SEND_MARGIN_MS`, since a timer
 * fires late, never early, and the call that carries the report starts only once it has
 * fired. A request that joins the batch moves no request's deadline: it moves
 * the timer only where its own deadline comes before the one the timer is armed for, as a request
 * read late does, which then goes by its own deadline. The turn's end (`agent_end`) sends what is
 * pending at once and drops the timer. A send the timer or the turn's end makes goes at once,
 * whatever report is on its way, and waits for no answer: each report is a call of its own, with
 * the daemon client's own timeout, and carries only requests that no report on its way carries, at
 * most `LEGION_MAX_MODEL_REQUESTS` of them and all of one owner. What a full report leaves, and
 * another owner's requests, go at once too, each in a call of its own, so every request pending
 * when a send falls due goes then. Reports so cross, and their answers arrive in either order: each
 * answer puts back only its own report's requests, and the daemon takes a request again by its id
 * and keeps the later end, so a crossing changes nothing it decides. A request the daemon's answer
 * does not name as held goes again under the same id. A failed call holds every send until a bound
 * after its answer, and an answer only arms the timer, so a failing daemon is never called in a
 * loop. A request older than an hour is dropped with a warning.
 * A report the daemon refuses (a 4xx) is dropped with a warning, as a turn outcome's is.
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
 * due: a timer fires late, never early, and the call that carries the report starts only once it
 * has fired, so a timer armed for the bound itself sends the report after it. The Claude Code
 * bridge leaves the same room, `requestSendMargin` in
 * `packages/daemon/internal/claudebridge/modelrequests.go`, so both hosts state one rule. */
export const MODEL_REQUEST_SEND_MARGIN_MS = 250;

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
  let retryAt = 0;

  /** Arms the timer for the earliest deadline pending, and never for before a failed call's hold
   * ends (`retryAt`): again, where a timer is armed for later. */
  const arm = (): void => {
    const first = earliestPending();
    if (first === undefined) return;
    const at = Math.max(first.due, retryAt);
    if (cancelTimer !== undefined && armedAt <= at) return;
    disarm();
    armedAt = at;
    cancelTimer = deps.schedule(
      () => {
        cancelTimer = undefined;
        send();
      },
      Math.max(0, at - deps.now())
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

  /** Sends every pending request now, the earliest deadline's report first, each report in a call
   * of its own (`report`) whatever report is on its way, unless a failed call holds sends
   * (`retryAt`): then it arms the timer for when that hold ends. */
  const send = (): void => {
    const now = deps.now();
    if (now < retryAt) {
      arm();
      return;
    }
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
      report(first.owner, batch);
    }
  };

  /** Puts `batch`, which no other report carries, on its way in a call of its own. Its answer
   * sends nothing: it arms the timer for what is pending, and after a failed call holds every
   * send until a bound after that answer (`retryAt`). */
  const report = (owner: ModelRequestCredential, batch: readonly Pending[]): void => {
    const onItsWay: Promise<void> = post(owner, batch).then((answered) => {
      onTheWay.delete(onItsWay);
      if (!answered) {
        retryAt = deps.now() + MODEL_REQUEST_REPORT_BOUND_MS;
        disarm();
      }
      arm();
    });
    onTheWay.add(onItsWay);
  };

  /** Makes one report of `batch`, and puts back under the same id each request of it the daemon
   * did not say it holds, due a bound after this answer, but none of a report the daemon refused
   * (a 4xx). It answers false for a call that failed. Reports cross, and their answers arrive in
   * either order: each puts back its own batch alone, which no other report carries, and the
   * daemon takes a request again by its id and keeps the later end. */
  const post = async (
    owner: ModelRequestCredential,
    batch: readonly Pending[]
  ): Promise<boolean> => {
    let kept: ReadonlySet<string> | undefined;
    try {
      const answer = await retryWhileDaemonRestarts("claims/model-requests", () =>
        deps.daemon().modelRequests({
          claimToken: owner.claimToken,
          sessionId: owner.sessionID,
          secret: owner.secret,
          generation: owner.generation,
          requests: batch.map((entry) => entry.request),
        })
      );
      kept = new Set(answer.kept);
      deps.log.debug("legion: reported model requests", {
        claimToken: owner.claimToken,
        reported: batch.length,
        kept: kept.size,
      });
    } catch (error) {
      if (error instanceof LegionDaemonApiError && error.status >= 400 && error.status < 500) {
        // The daemon's own refusal will not pass on a retry: a stale generation, a secret it no
        // longer holds, a body it cannot read. Nothing this pane can do fixes it.
        deps.log.warn("legion: the daemon refused a report of model requests; they go unreported", {
          claimToken: owner.claimToken,
          requests: batch.length,
          error: messageFor(error),
        });
        return true;
      }
      deps.log.warn("legion: a report of model requests did not reach the daemon; it goes again", {
        claimToken: owner.claimToken,
        requests: batch.length,
        error: messageFor(error),
      });
    }
    const again = deps.now() + MODEL_REQUEST_REPORT_BOUND_MS;
    const unheld = batch.filter((entry) => kept?.has(entry.request.id) !== true);
    for (const entry of unheld) entry.due = again;
    pending = [...unheld, ...pending];
    return kept !== undefined;
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
      const request: LegionModelRequestBody = {
        id: deps.newId(),
        session: where.session,
        model: message.model,
        endedAt: new Date(endedAt).toISOString(),
        ...(usage === undefined
          ? {}
          : { uncachedInputTokens: tokens(usage.input) + tokens(usage.cacheWrite) }),
      };
      pending.push({
        owner,
        request,
        due: endedAt + MODEL_REQUEST_REPORT_BOUND_MS - MODEL_REQUEST_SEND_MARGIN_MS,
      });
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
