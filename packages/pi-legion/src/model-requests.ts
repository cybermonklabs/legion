import type { LegionModelRequestBody } from "@legion/contracts/legion-api";
import { LEGION_MAX_MODEL_REQUESTS } from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import {
  LegionDaemonApiError,
  type LegionDaemonClient,
  retryWhileDaemonRestarts,
} from "@legion/pi-shared/daemon-client";
import type { AgentEndMessage } from "@legion/pi-shared/pi-types";

/**
 * The Oh My Pi half of the report of model requests (`POST /legion/v1/claims/model-requests`,
 * dispatch://INFRA-5899): each model request a session of this process makes — the role's own,
 * each subagent's it starts, and the controller's — is reported to the daemon with the moment it
 * ended, its uncached input tokens and, for one a limit refused, who refused it, so the daemon
 * counts the tokens and the refusals the project's limit counts (the provider's headroom and
 * refusals).
 *
 * **Which host event carries a request.** Measured on the pinned host
 * (`v18.18.6-sami.20261125-164517`) in RPC mode against an Anthropic-shaped stand-in, with an
 * extension that logged every event: each request the session's agent loop sends ends in exactly
 * one `message_end` of an assistant message, which carries the request's `usage` (`input`,
 * `cacheRead`, `cacheWrite`), its `model`, the provider's `responseId` and the moment the session
 * stamped it ended (`completedAt`, `session/agent-session.ts:5807-5810`). A request the provider
 * refused with HTTP 429 that the session retries itself ends in its own `message_end` too —
 * `stopReason: "error"`, `errorStatus: 429`, the provider's text in `errorMessage`, every usage
 * member 0 — before the host's `auto_retry_start`, and its retry ends in a `message_end` of its
 * own. A `task` subagent's requests reach this same extension instance with
 * `context.agent.kind === "sub"` and the subagent's own session id, and no second instance saw
 * them. A 429 the transport retries itself (the Anthropic provider's own loop,
 * `packages/ai/src/providers/anthropic.ts:3787-3810`, which no `x-should-retry: false` stops)
 * reaches no extension at all: `message_end` comes once, for the attempt that was answered, and
 * `after_provider_response` fires only for a response whose headers came with a 2xx
 * (`anthropic.ts:2806`). Those refusals, and a side request outside the agent loop (a title, a
 * classifier, a compaction's summary), are what this host cannot report. So each batch says
 * whether its host reports the 429s its transport retries (`retriedRefusalsReported`,
 * `reportsRetriedRefusals`), false on this host, and the daemon reads the provider's refusals as
 * not recorded while a request of such a batch is in the hour they count, rather than a count it
 * knows to be low; the headroom counts this host's tokens as any other's.
 *
 * **What a request is.** A `message_end` of an assistant message the provider answered: one with
 * the provider's response id or tokens, or a refusal (`errorStatus` 429, the status a limit
 * refuses with, whose text says whose limit: spec S1's "The provider's configuration"). A message
 * the host wrote itself — a gate's stop, a request that never reached the provider — carries none
 * of them and is no request; nor is another failure that counted no token. Its uncached input is
 * `input + cacheWrite`, never `cacheRead`. A refusal is classed by its own text: the project's own
 * limit's where `LEGION_SIGNAL_OWN_REFUSAL` matches, else the provider's where
 * `LEGION_SIGNAL_PROVIDER_REFUSAL` does, else unknown. Each attempt is its own message with its own
 * text, so no attempt is classed by another's.
 *
 * **When it is sent.** One pending batch per reporter. The first pending request arms one timer
 * for its end plus `MODEL_REQUEST_REPORT_BOUND_MS`; a request that joins the batch moves no
 * deadline. The turn's end (`agent_end`) sends what is pending at once and drops the timer.
 * Requests that end while a report is on its way wait for a report of their own. A request the
 * daemon's answer does not name as held is sent again under the same id, no sooner than the bound
 * after that answer; an id names one request for ever (`newId`). A report the daemon refuses (a
 * 4xx) will not pass on a retry and is dropped with a warning, as the turn outcome's is. Nothing
 * of the session waits on any of it: the hooks return at once.
 *
 * The turn's outcome report (`./turn-outcome`) is a separate route and is unchanged by this one.
 * Claude Code's half is `packages/daemon/internal/claudebridge/modelrequests.go`.
 */

/** How long after a request's end its report may wait: a quarter of the minute the headroom reads,
 * so a session sends at most four reports a minute (spec, "The model requests"). */
export const MODEL_REQUEST_REPORT_BOUND_MS = 15_000;

/** The launch environment's names of the project's two refusal patterns
 * (`runtime.OwnRefusalVariable`, `runtime.ProviderRefusalVariable` in the daemon). */
const OWN_REFUSAL_VARIABLE = "LEGION_SIGNAL_OWN_REFUSAL";
const PROVIDER_REFUSAL_VARIABLE = "LEGION_SIGNAL_PROVIDER_REFUSAL";

/** The HTTP status a limit refuses a request with; a failure of any other status is no refusal. */
const REFUSED_STATUS = 429;

/** The claim a request is reported under: a role's registration, or the controller's. */
export interface ModelRequestCredential {
  readonly claimToken: string;
  readonly sessionID: string;
  readonly secret: string;
  readonly generation: number;
}

/** The project's two refusal patterns, each absent where the launch set none. */
export interface RefusalPatterns {
  readonly own?: RegExp;
  readonly provider?: RegExp;
}

export interface ModelRequestsLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

type RefusalClass = NonNullable<LegionModelRequestBody["refusal"]>;

/**
 * The two patterns the launch's environment sets, each compiled as `new RegExp(pattern, "su")`,
 * under which the daemon's loader admits only what Go's `regexp.Compile("(?s)" + pattern)` reads
 * alike (`config.SharedPattern`). An unset or empty variable is no pattern. A pattern this engine
 * refuses — which the loader never lets through — is warned about once and classes nothing.
 */
export function refusalPatterns(
  env: Readonly<Record<string, string | undefined>>,
  log: ModelRequestsLog
): RefusalPatterns {
  const compile = (variable: string): RegExp | undefined => {
    const pattern = env[variable];
    if (pattern === undefined || pattern === "") return undefined;
    try {
      return new RegExp(pattern, "su");
    } catch (error) {
      log.warn("legion: a refusal pattern of the launch does not compile; it classes nothing", {
        variable,
        error: messageFor(error),
      });
      return undefined;
    }
  };
  return { own: compile(OWN_REFUSAL_VARIABLE), provider: compile(PROVIDER_REFUSAL_VARIABLE) };
}

/** Who refused a request whose refusal said `text`: the own pattern first, the provider's second. */
export function refusalClass(text: string, patterns: RefusalPatterns): RefusalClass {
  if (patterns.own?.test(text) === true) return "own";
  if (patterns.provider?.test(text) === true) return "provider";
  return "unknown";
}

/**
 * The events a host fires for each attempt its HTTP transport retries by itself, any one of which
 * carries the 429 of such an attempt to an extension. None is named: the pinned host offers none
 * (the capture above), and the event this waits for is the change to the Oh My Pi fork that
 * dispatch://INFRA-5899/comment/d25642c3-df88-4141-8ff0-020f9f8be3f8 asks for, whose name and
 * shape that thread decides. Until it is named here, with a handler that reports each such 429,
 * every batch says its host does not report them.
 */
const RETRIED_ATTEMPT_EVENTS: readonly string[] = [];

/** A host as `reportsRetriedRefusals` reads it: whether it offers an extension the event named. */
export interface HostEvents {
  readonly offers: (event: string) => boolean;
}

/**
 * The pinned host as `reportsRetriedRefusals` reads it. Its extension API lists no event an
 * extension can ask after — `pi.on` takes a handler under any name, and the host calls those of
 * the events it fires — and it fires none for an attempt its transport retries, so it offers none.
 */
export const PINNED_HOST_EVENTS: HostEvents = { offers: () => false };

/**
 * Whether `host` reports each HTTP 429 its transport retries by itself, which each batch says
 * (`retriedRefusalsReported`): true only where it offers one of `events`, an event for each such
 * attempt, read from the host and never from a version text. The daemon reads the provider's
 * refusals as not recorded while a request of a batch that says false is in the hour they count,
 * since that host's count is known to be low.
 */
export function reportsRetriedRefusals(
  host: HostEvents,
  events: readonly string[] = RETRIED_ATTEMPT_EVENTS
): boolean {
  return events.some((event) => host.offers(event));
}

export interface ModelRequestsReporterDeps {
  readonly daemon: () => LegionDaemonClient;
  /** The claim a top-level session of this process holds — a role's registration or the
   * controller's — and undefined for any other session. */
  readonly credential: (sessionID: string) => ModelRequestCredential | undefined;
  /** The top-level session the pane is on, whose claim a subagent's requests are reported under. */
  readonly paneSession: () => string | undefined;
  readonly patterns: RefusalPatterns;
  readonly log: ModelRequestsLog;
  readonly now: () => number;
  /** Runs `callback` once `delayMs` has passed and answers what cancels it. */
  readonly schedule: (callback: () => void, delayMs: number) => () => void;
  /** A fresh id, never given out before. */
  readonly newId: () => string;
  /** Whether this host reports each HTTP 429 its transport retries by itself
   * (`reportsRetriedRefusals`), which every batch says. */
  readonly retriedRefusalsReported: boolean;
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
  let sending: Promise<void> | undefined;
  let cancelTimer: (() => void) | undefined;
  /** A send fell due while another was on its way: it goes as soon as that one is answered. */
  let due = false;

  const arm = (): void => {
    if (cancelTimer !== undefined || pending.length === 0) return;
    const at = Math.min(...pending.map((entry) => entry.due));
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

  const send = (): void => {
    if (sending !== undefined) {
      due = true;
      return;
    }
    const first = pending[0];
    if (first === undefined) return;
    disarm();
    const batch: Pending[] = [];
    const rest: Pending[] = [];
    for (const entry of pending) {
      if (batch.length < LEGION_MAX_MODEL_REQUESTS && sameOwner(entry.owner, first.owner)) {
        batch.push(entry);
      } else {
        rest.push(entry);
      }
    }
    pending = rest;
    sending = post(first.owner, batch).finally(() => {
      sending = undefined;
      if (due || pending.some((entry) => !sameOwner(entry.owner, first.owner))) {
        due = false;
        send();
        return;
      }
      arm();
    });
  };

  const post = async (owner: ModelRequestCredential, batch: readonly Pending[]): Promise<void> => {
    let kept: ReadonlySet<string> = new Set();
    try {
      const answer = await retryWhileDaemonRestarts("claims/model-requests", () =>
        deps.daemon().modelRequests({
          claimToken: owner.claimToken,
          sessionId: owner.sessionID,
          secret: owner.secret,
          generation: owner.generation,
          requests: batch.map((entry) => entry.request),
          retriedRefusalsReported: deps.retriedRefusalsReported,
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
        return;
      }
      deps.log.warn("legion: a report of model requests did not reach the daemon; it goes again", {
        claimToken: owner.claimToken,
        requests: batch.length,
        error: messageFor(error),
      });
    }
    const again = deps.now() + MODEL_REQUEST_REPORT_BOUND_MS;
    const unheld = batch.filter((entry) => !kept.has(entry.request.id));
    for (const entry of unheld) entry.due = again;
    pending = [...unheld, ...pending];
  };

  return {
    messageEnded: (where, message) => {
      if (!isAssistantMessage(message)) return;
      const ownerSession = where.subagent ? deps.paneSession() : where.session;
      const owner = ownerSession === undefined ? undefined : deps.credential(ownerSession);
      if (owner === undefined) return;
      const refused = message.stopReason === "error" && message.errorStatus === REFUSED_STATUS;
      const usage = message.usage;
      const counted =
        tokens(usage?.input) +
        tokens(usage?.cacheWrite) +
        tokens(usage?.cacheRead) +
        tokens(usage?.output);
      if (!refused && message.responseId === undefined && counted === 0) return;
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
        ...(refused ? { refusal: refusalClass(message.errorMessage ?? "", deps.patterns) } : {}),
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
