/**
 * The session store's side of a frame's acknowledgement: what the session manager's `flush()`
 * says it wrote. `undefined` from `flush` is a host whose session manager has none.
 */
export interface StoreFlushHost {
  /** The session the host has open now: the one whose store a flush started now writes. */
  sessionID(): string;
  /** The session manager's `flush()` (`SessionContext.sessionManager.flush`), or undefined. */
  flush(): (() => Promise<void>) | undefined;
  /** A flush rejected after the one before it resolved: the store refuses this session's writes,
   *  and `waiting` frames are held unacknowledged until a flush resolves. */
  refused(error: unknown, waiting: number): void;
  /** A flush resolved after one rejected: the store takes the session's writes again, and
   *  `settled` frames held since were acknowledged. */
  accepted(settled: number): void;
}

interface Waiting {
  readonly sessionID: string;
  readonly acknowledge: () => void;
}

/**
 * Acknowledges a delivered frame only once the session store has written it (INFRA-5660).
 *
 * The host's `message_end` means Oh My Pi appended the message in memory and queued the store's
 * write (`#queueMessageEndPersistence` at the pinned tag), not that the store holds it: a process
 * that ends with its store refusing its writes loses every message it was handed since. So a
 * frame the host wrote, or handled with nothing to write, is acknowledged (`acknowledge`, the
 * relay's `done`) only once the session manager's `flush()`, called after the frame's entries
 * were appended, resolves. A flush that rejects leaves the frame unacknowledged: the consumer
 * keeps answering `progress` for it, so the listener offers it to no other process while this
 * one lives, and the next process of the session is offered it again, where the delivery ledger
 * drops a copy its transcript holds. Every frame held so settles at the next flush that
 * resolves, whichever frame's flush that is, since that flush began after the frame's entries
 * were appended, and only for the session it was delivered in: a flush of another session's
 * store says nothing of this one's. A session manager with no `flush()` acknowledges at once,
 * as nothing else can say the store took the frame.
 */
export class SettleAfterFlush {
  readonly #host: StoreFlushHost;
  readonly #waiting = new Set<Waiting>();
  // Whether the last flush that ended rejected: `refused` and `accepted` are said once a turn.
  #refusing = false;

  constructor(host: StoreFlushHost) {
    this.#host = host;
  }

  /**
   * The frame delivered in `sessionID` is written, or handled with nothing to write: it is
   * acknowledged once a flush of that session's store resolves, at once on a host with no flush.
   */
  settle(sessionID: string, acknowledge: () => void): void {
    const flush = this.#host.flush();
    if (flush === undefined) {
      acknowledge();
      return;
    }
    this.#waiting.add({ sessionID, acknowledge });
    const flushed = this.#host.sessionID();
    const covered = [...this.#waiting].filter((waiting) => waiting.sessionID === flushed);
    let flushing: Promise<void>;
    try {
      flushing = flush();
    } catch (error) {
      this.#rejected(error);
      return;
    }
    flushing.then(
      () => this.#resolved(covered),
      (error: unknown) => this.#rejected(error)
    );
  }

  #resolved(covered: readonly Waiting[]): void {
    let settled = 0;
    for (const waiting of covered) {
      if (!this.#waiting.delete(waiting)) continue;
      settled += 1;
      waiting.acknowledge();
    }
    if (!this.#refusing) return;
    this.#refusing = false;
    this.#host.accepted(settled);
  }

  #rejected(error: unknown): void {
    if (this.#refusing) return;
    this.#refusing = true;
    this.#host.refused(error, this.#waiting.size);
  }
}
