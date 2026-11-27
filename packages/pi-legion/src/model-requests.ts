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
 * **When it is sent.** One pending batch per reporter. The first pending request arms one timer
 * for its end plus `MODEL_REQUEST_REPORT_BOUND_MS`; a request that joins the batch moves no
 * deadline. The turn's end (`agent_end`) sends what is pending at once and drops the timer.
 * Requests that end while a report is on its way wait for a report of their own. A request the
 * daemon's answer does not name as held goes again under the same id. A failed call waits a
 * window before the next send. A request older than an hour is dropped with a warning.
 * A report the daemon refuses (a 4xx) is dropped with a warning, as a turn outcome's is.
 * Nothing of the session waits on any of it: the hooks return at once.
 *
 * The turn's outcome report (`./turn-outcome`) is a separate route and is unchanged by this one.
 * Claude Code's half is `packages/daemon/internal/claudebridge/modelrequests.go`.
 */

/** How long after a request's end its report may wait: a quarter of the minute the headroom reads,
 * so a session sends at most four reports a minute (spec, "The model requests"). */
export const MODEL_REQUEST_REPORT_BOUND_MS = 15_000;

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
  retried?: boolean;
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
  let sending: Promise<void> | undefined;
  let cancelTimer: (() => void) | undefined;
  let retryAt = 0;
  /** A send fell due while another was on its way. It goes at once if the daemon answered. */
  let due = false;

  const arm = (): void => {
    if (cancelTimer !== undefined || pending.length === 0) return;
    const first = earliestPending();
    if (first === undefined) return;
    const at = Math.max(first.due, retryAt);
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

  const send = (): void => {
    if (sending !== undefined) {
      due = true;
      return;
    }
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
    const first = earliestPending();
    if (first === undefined) return;
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
    sending = post(first.owner, batch).then((answered) => {
      sending = undefined;
      const wasDue = due;
      due = false;
      if (!answered) {
        retryAt = deps.now() + MODEL_REQUEST_REPORT_BOUND_MS;
        disarm();
      } else {
        const next = earliestPending();
        if (
          wasDue ||
          (next !== undefined && !next.retried && !sameOwner(next.owner, first.owner))
        ) {
          send();
          return;
        }
      }
      arm();
    });
  };

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
    for (const entry of unheld) {
      entry.due = again;
      entry.retried = true;
    }
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
      pending.push({ owner, request, due: endedAt + MODEL_REQUEST_REPORT_BOUND_MS });
      arm();
    },
    turnEnded: () => {
      send();
    },
    settled: async () => {
      while (sending !== undefined) await sending;
    },
  };
}
