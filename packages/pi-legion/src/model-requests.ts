import {
  LEGION_MAX_MODEL_REQUESTS,
  LegionModelRequest,
  type LegionModelRequestBody,
} from "@legion/contracts/legion-api";
import { messageFor } from "@legion/envoy-client/errors";
import { LegionDaemonApiError, type LegionDaemonClient } from "@legion/pi-shared/daemon-client";
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
 * **When it is sent.** The rules of both hosts' reporters
 * (dispatch://INFRA-5899/artifact/reporter-rules-md@v7), as this reporter keeps them, stated here
 * once; the Claude Code bridge's header states them as the bridge keeps them. Every other comment
 * of this reporter's code that names when a request is sent, put back, dropped or due points here
 * and restates none of it, or defines a constant: it names what the constant is and points here.
 * A test's comment describes its own row's scenario.
 *
 * - *Words.* A fresh request is one no call put back. A put-back request is one a call put back
 *   (`post`): every request of a call that failed, and each request an answer did not keep. A call
 *   fails on every ending but an answer and a 4xx other than 408 or 429, whoever sent the 4xx: the
 *   client throws before the call leaves (`deps.daemon()` among it), the connection is refused or
 *   drops, the call reaches its limit, `MODEL_REQUEST_REPORT_TIMEOUT_MS` (10 s), and is aborted,
 *   the answer cannot be read, or the answer is a 5xx, a 408 or a 429, from the daemon or a proxy,
 *   whatever its body. A report is one call, with at most `LEGION_MAX_MODEL_REQUESTS` (200)
 *   requests, all of one owner, and either all put-back requests or none; a report on its way is a
 *   call that has not ended yet.
 * - *A fresh request* is due at its end plus `MODEL_REQUEST_DUE_AFTER_MS`, which is
 *   `MODEL_REQUEST_SEND_MARGIN_MS` (250 ms) short of `MODEL_REQUEST_REPORT_BOUND_MS` (15 s), on the
 *   reporter's clock: a timer fires late, never early, and the call starts only once it has fired,
 *   so a timer armed for the bound itself would send the report after it. One timer is armed for
 *   the earliest moment due: a request that joins moves no other request's moment, and moves the
 *   timer only where its own comes first, as a request read late does; one read after its moment
 *   leaves at once. Its report leaves at its moment whatever report is on its way, in a call of
 *   its own that waits for no answer, and more than 200 of one owner due at once leave in as many
 *   reports as they need, all at once.
 * - *A put-back request.* S1 (dispatch://INFRA-5899/spec) does not bound it: the retry rule that
 *   follows does. It is due `MODEL_REQUEST_REPORT_BOUND_MS` after the end of the call that put it
 *   back, and only a due put-back request starts a send. Before it is due, it may ride in its
 *   owner's one report of put-back requests in a send that something else started: a fresh
 *   request's moment, a run's end, or another put-back request that is due. So a daemon that
 *   fails, or that answers and keeps nothing, is never called in a loop: a call's end only arms
 *   the timer. Each send carries at most one report of an owner's put-back requests, the oldest by
 *   end first, and the put-back requests it leaves past that report are due
 *   `MODEL_REQUEST_LEFT_DUE_AFTER_MS` (25 s) after that send, which keeps the open calls flat
 *   through a long stall. No report of an owner's put-back requests leaves while another is on its
 *   way: until that report's call ends, the owner's put-back requests wait for it (`waits`), so no
 *   send but a shutdown takes them and the timer is armed for none of them; its end, by its answer
 *   or its limit, arms the timer for them again, at once where one is due.
 * - *Answers.* Reports cross, and their answers arrive in either order: each answer puts back only
 *   its own report's requests, and the daemon takes a request again by its id and keeps the later
 *   end, so a crossing changes nothing it decides.
 * - *Dropped.* A report answered with a 4xx other than 408 or 429, by the daemon or a proxy, is
 *   dropped with a warning, and its requests are not put back. A request the contract's
 *   per-request schema refuses (`LegionModelRequest`) is dropped where it is read, at
 *   `messageEnded`, with a warning naming its id and the schema's reason, so it never shares a
 *   report, and its owner's other requests still leave. A request older than the hour the host
 *   keeps (`MODEL_REQUEST_KEEP_MS`) is dropped with a warning at a send.
 * - *A run's end* (`turnEnded`, at `agent_end`) sends every pending fresh request at once, whatever
 *   failed before, and drops the timer; put-back requests follow their rule.
 * - *A shutdown* (`shutdown`, at `session_shutdown`) sends every pending request at once, put-back
 *   requests included, in reports of at most 200, each a call of its own with its limit, and waits
 *   for no report on its way. No later send comes, so the stall's bound on open calls (next) does
 *   not apply at a shutdown.
 * - *A stall or an outage of the daemon.* Fresh requests still leave by their moments and each call
 *   ends by its limit, so while the host runs, until a shutdown, the calls open at once are no more
 *   than the fresh reports of the last 10 s and one report of put-back requests per owner, whatever
 *   the length of the stall.
 *
 * Nothing of the session waits on any of it: the hooks return at once.
 *
 * The turn's outcome report (`./turn-outcome`) is a separate route and is unchanged by this one.
 * Claude Code's half is `packages/daemon/internal/claudebridge/modelrequests.go`.
 */

/** How long after a request's end its report may wait, and after the end of the call that put a
 * request back it is due again ("When it is sent", above): a quarter of the minute the headroom
 * reads, so the timer sends about four times a minute (spec, "The model requests"). */
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

/** How long after a send the put-back requests it leaves past its one report of them are due
 * ("When it is sent", above): the call's limit, `MODEL_REQUEST_REPORT_TIMEOUT_MS`, and then a
 * bound, when the report the send took in their place is due again had its call failed at its
 * limit. So they start no send of their own before that report could go again, and the open calls
 * stay flat through a long stall. The Claude Code bridge waits as long, `requestLeftDueAfter` in
 * `packages/daemon/internal/claudebridge/modelrequests.go`. */
export const MODEL_REQUEST_LEFT_DUE_AFTER_MS =
  MODEL_REQUEST_REPORT_TIMEOUT_MS + MODEL_REQUEST_REPORT_BOUND_MS;

/** How long after its end the reporter keeps a request ("When it is sent", above): the daemon's
 * hour of history, past which a request is of no use on a retry. */
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
  /** A run ended (`agent_end`); "When it is sent" (above) says what it sends. */
  readonly turnEnded: () => void;
  /** The session shuts down (`session_shutdown`); "When it is sent" (above) says what it sends. */
  readonly shutdown: () => void;
  /** Resolves once no report is on its way. */
  readonly settled: () => Promise<void>;
}

/** One request waiting for the daemon to hold it, when it is due, and whether it is a put-back
 * request ("When it is sent", above). */
interface Pending {
  readonly owner: ModelRequestCredential;
  readonly request: LegionModelRequestBody;
  due: number;
  putBack: boolean;
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
  /** The reports on their way, each until its answer is handled ("When it is sent", above). */
  const onTheWay = new Set<Promise<void>>();
  /** The owner of each report of put-back requests on its way, until its call's end is handled. */
  const retrying: ModelRequestCredential[] = [];
  let cancelTimer: (() => void) | undefined;
  /** When the armed timer fires, while `cancelTimer` is set. */
  let armedAt = 0;

  /** Arms the timer for the earliest due of the requests pending but those that wait (`waits`):
   * again, where a timer is armed for later. */
  const arm = (): void => {
    const first = earliest(pending.filter((entry) => !waits(entry)));
    if (first === undefined) return;
    if (cancelTimer !== undefined && armedAt <= first.due) return;
    disarm();
    armedAt = first.due;
    cancelTimer = deps.schedule(
      () => {
        cancelTimer = undefined;
        send(false);
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

  /** Whether `entry` is a put-back request that waits for a report of its owner's put-back
   * requests on its way ("When it is sent", above). */
  const waits = (entry: Pending): boolean =>
    entry.putBack && retrying.some((owner) => sameOwner(owner, entry.owner));

  const earliest = (entries: readonly Pending[]): Pending | undefined => {
    let first: Pending | undefined;
    for (const entry of entries) {
      if (first === undefined || entry.due < first.due) first = entry;
    }
    return first;
  };

  /** Makes a send, as "When it is sent" (above) says: drops each request past
   * `MODEL_REQUEST_KEEP_MS`, takes every request pending at a shutdown (`everything`) and else what
   * `take` takes, and puts each report of them on its way (`report`), the earliest due's first. It
   * takes every batch out of `pending` before it reports any: a call that fails before it leaves
   * puts its batch back at once, and this send must not take that batch again. */
  const send = (everything: boolean): void => {
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
    let going = pending;
    pending = [];
    if (!everything) going = take(going, now);
    const batches: {
      readonly owner: ModelRequestCredential;
      readonly retry: boolean;
      readonly batch: Pending[];
    }[] = [];
    for (let first = earliest(going); first !== undefined; first = earliest(going)) {
      const batch: Pending[] = [first];
      const rest: Pending[] = [];
      for (const entry of going) {
        if (entry === first) continue;
        if (
          batch.length < LEGION_MAX_MODEL_REQUESTS &&
          entry.putBack === first.putBack &&
          sameOwner(entry.owner, first.owner)
        ) {
          batch.push(entry);
        } else {
          rest.push(entry);
        }
      }
      going = rest;
      batches.push({ owner: first.owner, retry: first.putBack, batch });
    }
    for (const { owner, retry, batch } of batches) report(owner, retry, batch);
  };

  /** What a send at a moment due or a run's end takes of `entries`, by the put-back rule of "When
   * it is sent" (above): it answers those, and puts the rest back in `pending`, due as that rule
   * says. */
  const take = (entries: readonly Pending[], now: number): Pending[] => {
    const retries: Pending[][] = [];
    const leftDue = now + MODEL_REQUEST_LEFT_DUE_AFTER_MS;
    const putBack = entries
      .filter((entry) => entry.putBack)
      .sort((a, b) => Date.parse(a.request.endedAt) - Date.parse(b.request.endedAt));
    for (const entry of putBack) {
      if (waits(entry)) {
        pending.push(entry);
        continue;
      }
      const retry = retries.find(
        ([first]) => first !== undefined && sameOwner(first.owner, entry.owner)
      );
      if (retry === undefined) {
        retries.push([entry]);
      } else if (retry.length < LEGION_MAX_MODEL_REQUESTS) {
        retry.push(entry);
      } else {
        entry.due = Math.max(entry.due, leftDue);
        pending.push(entry);
      }
    }
    return [...entries.filter((entry) => !entry.putBack), ...retries.flat()];
  };

  /** Puts `batch`, which no other report carries, on its way (`post`). Where it is a report of
   * put-back requests (`retry`), its owner stays in `retrying` until the call's end is handled
   * (`waits`); that end arms the timer (`arm`), as "When it is sent" (above) says. */
  const report = (
    owner: ModelRequestCredential,
    retry: boolean,
    batch: readonly Pending[]
  ): void => {
    if (retry) retrying.push(owner);
    const onItsWay: Promise<void> = post(owner, batch).then(() => {
      onTheWay.delete(onItsWay);
      if (retry) retrying.splice(retrying.indexOf(owner), 1);
      arm();
    });
    onTheWay.add(onItsWay);
  };

  /** Makes one report of `batch`, in a call aborted at `MODEL_REQUEST_REPORT_TIMEOUT_MS` on the
   * reporter's clock, and puts back under the same id each of its requests that "When it is sent"
   * (above) makes a put-back request, due as that section says. */
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
        error.status !== 408 &&
        error.status !== 429
      ) {
        // The 4xx that drops a report ("When it is sent", above): it will not pass on a retry,
        // the daemon's own refusal (a stale generation, a secret it no longer holds, a body it
        // cannot read) or a proxy's, and nothing this pane can do fixes it.
        deps.log.warn(
          `legion: a report of model requests was refused with ${error.status}; they go unreported`,
          {
            claimToken: owner.claimToken,
            requests: batch.length,
            error: messageFor(error),
          }
        );
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
    for (const entry of unheld) {
      entry.due = again;
      entry.putBack = true;
    }
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
      pending.push({
        owner,
        request: parsed.data,
        due: endedAt + MODEL_REQUEST_DUE_AFTER_MS,
        putBack: false,
      });
      arm();
    },
    turnEnded: () => {
      send(false);
    },
    shutdown: () => {
      send(true);
    },
    settled: async () => {
      while (onTheWay.size > 0) await Promise.all(onTheWay);
    },
  };
}
