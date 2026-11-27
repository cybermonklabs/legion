import { messageFor } from "@legion/envoy-client/errors";
import { type EnvoyClient, isBusUnanswered, stopHandBack } from "@legion/envoy-client/transport";

/**
 * The ledger of the role holds this process gave up: what it still owes the listener, and what
 * the listener refused to take back. One hold is one role topic under the session id it was held
 * as, and a hold is in at most one of the two books:
 *
 *  - pending: given up locally while the listener still names this process for it, so the next
 *    pass hands it back ({@link HandBackLedger.settleEnded});
 *  - abandoned: a hand-back the listener refused, left for the listener to reap until this
 *    process registers that hold's session again, which takes it back as a pending hand-back
 *    ({@link HandBackLedger.adoptFor}).
 *
 * The listener refuses a release its own holder's renewal beat under each of its conditional
 * deletes: the route fails rather than reporting a release it did not make. Such a hold is
 * abandoned rather than forgotten, because dropping it would leave the role standing at the
 * listener while this process's registrations went on renewing its claim — the defect a release
 * naming its process fixed, reached by another path. Throwing instead would fail a claim that
 * landed, which the caller's state has already taken.
 */
export interface HandBackLedger {
  /**
   * Hands `topic` back at the listener under `id`, abandoning the hold when the listener refuses
   * it. `logFields` names the hold as the caller's own warnings name it, beside the error.
   */
  readonly handBackOrAbandon: (
    topic: string,
    id: string,
    logFields: Record<string, unknown>
  ) => Promise<void>;
  /**
   * Hands back every pending hold, in turn, and answers whether `adopt` kept any of them.
   *
   * `acting` is the caller's fence, asked before each hold: a caller refused mid-hand-back stops
   * before the delete, since the task that replaces it settles what is still pending. A `Map`
   * iterator visits entries inserted after it was created, and an entry a session event added
   * behind the loop belongs to that replacing task.
   *
   * `adopt` answers whether the hold is one the caller keeps rather than hands back, and takes
   * it over when it does. The holds whose hand-back fails are abandoned once the loop is over,
   * so no hold this loop may still adopt is in both books while it runs.
   */
  readonly settleEnded: (acting: () => boolean, adopt: (hold: Hold) => boolean) => Promise<boolean>;
  /** Takes back every abandoned hold of `sessionID` as a pending hand-back: a registration of
   *  that session renews its claim at the listener, so this process owes the release again. */
  readonly adoptFor: (sessionID: string) => void;
  /** Files a hold this process has at the listener and no longer wants, for the next pass to
   *  hand back. */
  readonly recordEndedHold: (topic: string, id: string) => void;
  /** Forgets every hold of `topic`, pending or abandoned: the listener names a claim of this
   *  process for it now, and a release under the old id would end that new hold. */
  readonly forgetHoldsOf: (topic: string) => void;
  /**
   * Reads the listener's `last_seen` for the session a refused claim of `role` names when that
   * session holds this process's own abandoned hold of `topic`, and answers whether the claim
   * waits on it. A read that fails, or finds another holder, leaves the next pass to decide, and
   * so does one the task's deadline outran: both answer true, the answer that records no
   * refusal.
   */
  readonly observeAbandonedHold: (
    topic: string,
    role: string,
    holder: string | undefined,
    live: () => boolean
  ) => Promise<boolean>;
  /**
   * Hands back, side by side, `held` (the hold the process still keeps) and every hold in both
   * books, each once, for a process whose session is stopping: the stop's own call, which the
   * client's stop gate lets through (`stopHandBack`). Each release names this process, as every
   * release here does, so the listener frees only a claim this process made. A release that
   * fails is warned about on `warn`, the stop's own writer, which no tell gate holds, and let
   * go, and neither book changes: no pass runs after a stop, so nothing is left to send one again.
   */
  readonly handBackAll: (
    held: Hold | undefined,
    warn: (message: string, context: Record<string, unknown>) => void
  ) => Promise<void>;
}

/** One role hold this process gave up: the role topic, and the session id it was held as. */
export interface Hold {
  readonly topic: string;
  readonly id: string;
}

export interface HandBackLedgerDeps {
  /** The listener client, read at each call, as the state module reads it. */
  readonly client: () => EnvoyClient;
  /** The process this ledger's releases name. */
  readonly clientInstanceID: string;
  /** Writes this ledger's warning about a hand-back that failed; the host's logger stays the
   *  caller's. */
  readonly warn: (message: string, context: Record<string, unknown>) => void;
}

export function createHandBackLedger(deps: HandBackLedgerDeps): HandBackLedger {
  const pending = new Map<string, Hold>();
  const abandoned = new Map<string, Hold & { readonly lastSeen?: number }>();

  /** Both books are keyed by the hold itself: one role topic under one session id. */
  const keyOf = (topic: string, id: string): string => `${topic}@${id}`;

  // The release names the process making it, so the listener frees the role claim of this
  // process alone: a second process that resumed this session id keeps the role it holds, and
  // its claim outlives the hand-back a copy of its transcript makes.
  const handBack = async (topic: string, id: string): Promise<void> => {
    await deps
      .client()
      .unsubscribe({ sessionID: id, topics: [topic], clientInstanceID: deps.clientInstanceID });
  };

  // Before it answers, the listener frees the claim, takes the role's topic out of the registry
  // and sets the filter of the session's consumer. Its message bus can fail to answer at any of
  // those steps, after the steps before it landed, so the warning for that outage says only what
  // can be so. Any other failure is warned as a role that was not handed back.
  const warnRefused = (error: unknown, logFields: Record<string, unknown>): void => {
    deps.warn(
      isBusUnanswered(error)
        ? "envoy: the listener's message bus did not answer the hand-back of a role this process gave up. The listener may already have freed the claim. The registry may already be without the role's topic."
        : "envoy: a role this process gave up was not handed back",
      { ...logFields, error: messageFor(error) }
    );
  };

  return {
    handBackOrAbandon: (topic, id, logFields) =>
      handBack(topic, id).catch((error: unknown) => {
        abandoned.set(keyOf(topic, id), { topic, id });
        warnRefused(error, logFields);
      }),

    settleEnded: async (acting, adopt) => {
      let restored = false;
      const failed: Hold[] = [];
      for (const [key, hold] of pending) {
        if (!acting()) break;
        pending.delete(key);
        if (adopt(hold)) {
          restored = true;
          continue;
        }
        await handBack(hold.topic, hold.id).catch((error: unknown) => {
          failed.push(hold);
          warnRefused(error, { topic: hold.topic, sessionID: hold.id });
        });
      }
      for (const hold of failed) abandoned.set(keyOf(hold.topic, hold.id), hold);
      return restored;
    },

    adoptFor: (sessionID) => {
      for (const [key, hold] of abandoned) {
        if (hold.id !== sessionID) continue;
        abandoned.delete(key);
        pending.set(key, { topic: hold.topic, id: sessionID });
      }
    },

    recordEndedHold: (topic, id) => {
      pending.set(keyOf(topic, id), { topic, id });
    },

    forgetHoldsOf: (topic) => {
      for (const [key, hold] of abandoned) {
        if (hold.topic === topic) abandoned.delete(key);
      }
      for (const [key, hold] of pending) {
        if (hold.topic === topic) pending.delete(key);
      }
    },

    observeAbandonedHold: async (topic, role, holder, live) => {
      // A refusal that names no holder names no hold of this process's either.
      if (holder === undefined) return false;
      const key = keyOf(topic, holder);
      const hold = abandoned.get(key);
      if (hold === undefined) return false;
      const seen = await deps
        .client()
        .getRole(role)
        .then(
          (info) => (info.holder === holder ? info.last_seen : undefined),
          () => undefined
        );
      if (!live() || seen === undefined) return true;
      // The bound on reading another process's refusal as this process's own stale hold: a later
      // read that differs drops the hold, so the misreading costs at most one extra soft claim.
      if (hold.lastSeen !== undefined && seen !== hold.lastSeen) {
        abandoned.delete(key);
        return false;
      }
      abandoned.set(key, { topic, id: hold.id, lastSeen: seen });
      return true;
    },

    handBackAll: async (held, warn) => {
      const owed = new Map<string, Hold>();
      if (held !== undefined) owed.set(keyOf(held.topic, held.id), held);
      for (const [key, hold] of pending) owed.set(key, hold);
      for (const [key, hold] of abandoned) owed.set(key, { topic: hold.topic, id: hold.id });
      const handBackForStop = stopHandBack(deps.client());
      await Promise.all(
        [...owed.values()].map((hold) =>
          handBackForStop({
            sessionID: hold.id,
            topics: [hold.topic],
            clientInstanceID: deps.clientInstanceID,
          }).catch((error: unknown) =>
            warn(
              isBusUnanswered(error)
                ? "envoy: the listener's message bus did not answer the stop's hand-back of a role this process held. The listener may already have freed the claim. The registry may already be without the role's topic."
                : "envoy: the stop did not hand back a role this process held",
              { topic: hold.topic, sessionID: hold.id, error: messageFor(error) }
            )
          )
        )
      );
    },
  };
}
