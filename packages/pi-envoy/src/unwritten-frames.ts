import type { DedupeIdentity, DeliveryClaim } from "@legion/envoy-client/delivery";
import { ledgerIdentity } from "@legion/envoy-client/delivery-ledger";
import type { ContentBlock } from "@legion/pi-shared/pi-types";
import type { CardMode } from "./card-wake";
import { type HeldReplayNotice, writtenCardIdentity } from "./missed-messages";

/**
 * How a card is handed to the host: `card` and `aside` each go as an aside that starts a turn when
 * the host is idle (`CardWake.deliveryFor`; `aside` is a targeted Aside, which the wake never
 * covers); `inert`, a steer that starts no turn (`triggerTurn: false`); or `stored`, a `low` card
 * (`lowCard`), as `nextTurn` with no turn. An idle host writes an inert card at once and runs
 * nothing for it; one handed while a turn streams is steered into that turn, and the agent loop
 * runs another model turn for it (measured on omp 18.12.2). So an inert card is handed only to a
 * settled host (`UnwrittenFramesHost.settled`): one sent while the host is busy is held until the
 * first settled heartbeat (`handBackWithdrawn`), as is a replay notice (`sendNotice`), which is
 * handed the same way. A card handed again keeps its delivery.
 *
 * An idle host appends a stored card to the context and to the session's store at once, starting no
 * turn and emitting no `message_end`. A host that streams keeps it in memory only, where a turn
 * that a card starts does not take it and a process that ends loses it. So a stored card too is
 * handed only to a settled host, and not while a card handed to start a turn has not started it
 * (`runStarted`). One sent while the host is busy is held, and handed at the first settled check
 * (`storedCheck`, which the extension runs after a run that does not continue, and at each
 * heartbeat that finds the host settled) or right before a card that starts a turn. A held
 * stored card holds back no other card. With no `message_end`, a stored card is written once the
 * session's store holds its entry (`storedCheck`, `UnwrittenFramesHost.storedEntries`), and it is
 * never handed again.
 */
export type CardDelivery = CardMode | "inert";

/**
 * A frame handed to the host that the host has not written to the session's transcript yet. Oh
 * My Pi holds a card or a user turn sent while a turn streams in memory until the turn takes it,
 * and writes it, emitting its `message_end`, only then (measured on omp 18.8.9, idle and mid-turn
 * alike); a process that ends first loses it. Until then the frame's claim stays pending, so the
 * frame is answered no `done`, the stream keeps its copy, and nothing records it shown.
 */
interface Unwritten {
  /** The session it was handed to: written once the host is on another, it records nothing. */
  readonly sessionID: string;
  readonly identity: DedupeIdentity;
  /**
   * The frame's claim, settled once the host writes it. None for a replayed card of a frame the
   * dedupe had already let go of (`showReplayCard` in extensions/envoy.ts): that card still waits
   * to be written.
   */
  readonly claim: DeliveryClaim | undefined;
}

/**
 * A card waiting to be written, under every key `unwrittenCardKeys` gives its frame: one handed to
 * the host, or an inert card held until the host is settled.
 */
interface UnwrittenCard extends Unwritten {
  readonly keys: readonly string[];
  /** What it is handed as: a card the host withdrew unwritten is handed again as it was. */
  readonly content: string | readonly ContentBlock[];
  readonly deliverAs: CardDelivery;
  /**
   * The host moments counted when it was handed (`handBackWithdrawn`); undefined for an inert
   * card held, never handed, which the next settled moment hands.
   */
  readonly handedAt: number | undefined;
}

/** A person's turn waiting to be written; a `pi.sendUserMessage` turn is found by its text. */
interface UnwrittenTurn extends Unwritten {
  readonly via: "text" | "typed";
  /**
   * The host moments counted when the host took it into its queue (`handBackWithdrawn`): a
   * `pi.sendUserMessage` turn's at the send, a typed input's once `pi.sendUserInput` answers that
   * it submitted a message; undefined until then.
   */
  readonly handedAt: number | undefined;
}

/** A person's turn as `UnwrittenFrames.awaitTurn` takes it. */
export interface HandedTurn {
  /** The session the turn was handed to. */
  readonly sessionID: string;
  /** The frame the turn carries; none for a frame with no identity, which nothing waits on. */
  readonly identity: DedupeIdentity | undefined;
  readonly claim: DeliveryClaim | undefined;
  /** `text`: sent with `pi.sendUserMessage`, in the host's queue from the send. `typed`: sent with
   *  `pi.sendUserInput`, in the queue only once the host answers that it submitted a message. */
  readonly via: "text" | "typed";
}

/** A replay notice not handed yet, for the session it was raised for. `recorded`: the session's
 *  transcript records it held, so its hand-off is marked there too. */
interface HeldNotice extends HeldReplayNotice {
  readonly sessionID: string;
  recorded: boolean;
}

/** What `UnwrittenFrames` asks of the extension around it. */
export interface UnwrittenFramesHost {
  /** The session this instance serves now, which a session event moves: a settled moment hands
   *  again only what this session was handed, a run's end is done with only its text turns, and
   *  a notice is held only for it. */
  sessionID(): string;
  /** Whether the host has `id` open, which is the conversation a card handed now reaches: a card
   *  for any other session is handed to nobody (`sendCard`), since the host writes what it is
   *  handed in the conversation it has. The host's live id alone, not the two halves the
   *  extension's own `sessionOpen` reads: between the host's move and the event that tells the
   *  extension of it, the session the host moved to is the one its cards reach. */
  hostIsOn(id: string): boolean;
  /**
   * Whether the host is settled: idle, holding no message, not shutting down, and on the session
   * this instance serves. Only a settled host is handed an inert card, a stored card or a replay
   * notice, or handed back what an interrupt withdrew.
   */
  settled(): boolean;
  /** Every entry of the session this instance serves, on every branch of its tree, in the order
   *  the host recorded them, once Oh My Pi's store holds that session (`storeHoldsSession`, or
   *  its file on disk under file storage); undefined before that (Oh My Pi stores no session
   *  before its first assistant message), for a session the host stores nowhere, or while the
   *  host is on another session. A stored card is written once an entry here names its frame
   *  (`writtenCardIdentity`), on whatever branch the host stored it. */
  storedEntries(): readonly unknown[] | undefined;
  /** Records a frame the host wrote in the ledger of `sessionID`, the session it was shown in
   *  (`DeliveryLedger.recordDeliveredFor`). The write binds to that session and records nothing
   *  once it is not the one this process writes its transcript to, so the caller makes no
   *  session test of its own. */
  recordDelivered(sessionID: string, identity: DedupeIdentity): Promise<void>;
  /** Hands the host a card (`pi.sendMessage`); a throw means it was not handed. */
  handCard(
    content: string | readonly ContentBlock[],
    deliverAs: CardDelivery,
    identity: DedupeIdentity | undefined
  ): void;
  /** A card for a session the host does not have open was handed to nobody, and its claim went
   *  back with nothing recorded (`sendCard`). */
  cardNotShown(sessionID: string, identity: DedupeIdentity | undefined): void;
  /** Hands the host a replay notice (`pi.sendMessage` of a card naming `LOCAL_ENVOY_NOTICE`, a
   *  steer that starts no turn); a throw means it was not handed. */
  handNotice(content: string): void;
  /** The Dispatch message id of the person's turn `message` is, among the turns noted for this
   *  session (`matchInjectedUserTurn`), if any. */
  matchTurn(message: unknown): string | undefined;
  /** The interrupt withdrew the person's turn `messageID` handed to `sessionID`: no message will
   *  carry it, so whatever was noted to find it, or to go with it, goes. */
  turnWithdrawn(sessionID: string, messageID: string): void;
  /** A card the host withdrew unwritten, or an inert card held until the host settled, could not
   *  be handed; it keeps waiting, and the next settled moment tries again. */
  handBackFailed(identity: DedupeIdentity, error: unknown): void;
  /** A replay notice could not be handed; it stays held, and the next settled moment tries again. */
  handNoticeFailed(error: unknown): void;
  /** Records the replay notice `id` held until the host settles in the transcript of the session
   *  this instance serves (`REPLAY_NOTICE_ENTRY`), so a restart that cuts the hold short holds it
   *  again (`requeueNotices`); a throw means it was not recorded. */
  recordNotice(id: string, content: string): void;
  /** Records in that transcript that the held notice `id` was handed; a throw means it was not. */
  noticeHanded(id: string): void;
  /** A held notice's record could not be written: it is held in this process only. */
  recordNoticeFailed(error: unknown): void;
}

/**
 * The keys a card waiting to be written is found under, from the frame it shows: its event id and
 * its dedupe key, as the ledger knows it (`ledgerIdentity`), so the written card names it
 * by either. None for a frame the ledger keeps nothing of.
 */
export function unwrittenCardKeys(identity: {
  readonly event_id?: string;
  readonly dedupe_key?: string;
}): string[] {
  return [
    ...(identity.event_id === undefined ? [] : [`event:${identity.event_id}`]),
    ...(identity.dedupe_key === undefined ? [] : [`key:${identity.dedupe_key}`]),
  ];
}

/**
 * Whether Oh My Pi's store holds every entry of `entries`, all the entries of a session the host
 * stores (one that names a session file), on every branch of its tree, as they tell it: one of
 * them is an assistant message. The host stores no session before its first assistant message,
 * whichever branch holds it. When it records that message it hands the store the whole session,
 * every branch, and from then on it hands the store each entry as it records it, on whatever
 * branch the leaf is (`SessionManager` `#shouldHaveSessionFile`, whose
 * `#historyContainsAssistantMessage` reads every entry, and `#recordEntry`, at the pinned build).
 * So a card stored on a branch with no reply, once another branch holds one, is stored. That is so
 * under file storage, and under `OMP_SESSION_STORAGE=sql`, where the session is rows of a database
 * and the file the host names is never on disk; there the store takes each entry at once and
 * writes it to the database right after, as it does every entry.
 */
export function storeHoldsSession(entries: readonly unknown[]): boolean {
  return entries.some((entry) => {
    if (typeof entry !== "object" || entry === null || !("type" in entry)) return false;
    if (entry.type !== "message" || !("message" in entry)) return false;
    const { message } = entry;
    return (
      typeof message === "object" &&
      message !== null &&
      "role" in message &&
      message.role === "assistant"
    );
  });
}

/**
 * The cards (`unwrittenCardKeys`) and the person's turns (by Dispatch message id) handed to the
 * host and not yet written (`Unwritten`), and the inert cards held until the host is settled,
 * each under the claim of its frame. This owns each claim from the hand-off on: the claim
 * settles, and the replay ledger records the frame, once the host writes it (`written`,
 * `turnHandled`, `textTurnsEnded`), or at once for a frame nothing waits on (`sendCard`,
 * `awaitTurn`); it is released, recording nothing, when the card was for a session the host does
 * not have open, or the host refused the card, or an inert card held had nothing to wait under
 * (`sendCard`), or the host dropped the frame unwritten (a session change,
 * `releaseOtherSessions`; a person's turn an interrupt withdrew,
 * `handBackWithdrawn`), so the frame is answered no `done` and the stream keeps its copy.
 *
 * It also holds the replay notices (`sendNotice`) sent while the host is not settled, in order,
 * until the first settled moment hands them. A notice names no frame and has no claim: nothing
 * waits on its write. A held notice is recorded in the session's transcript, and marked handed
 * there once it is, so a restart that cuts the hold short holds it again (`requeueNotices`).
 */
export class UnwrittenFrames {
  readonly #host: UnwrittenFramesHost;
  readonly #cards = new Map<string, UnwrittenCard>();
  readonly #turns = new Map<string, UnwrittenTurn>();
  // The replay notices not handed yet, in the order they were sent, each with the session it was
  // raised for, and whether the transcript records it held.
  #notices: HeldNotice[] = [];
  // The notices recorded held that this process handed, each by its session and id
  // (`JSON.stringify([sessionID, id])`), as a fork's transcript copies the entries, ids included,
  // of the session it forked from: a restore that reads a transcript of their session whose mark
  // of that failed to be written does not hold them again.
  readonly #handedNotices = new Set<string>();
  // The moments the host had to queue or write a card handed before it: each heartbeat, and each
  // run that ended aborted (`handBackWithdrawn`, `runAborted`).
  #moments = 0;
  // The moment a card that starts a turn was handed to a settled host, until that turn starts
  // (`runStarted`) or the host writes a card that starts one (a fold writes it with no run), or
  // a whole moment has passed: the host reads idle until it starts that turn, after its own
  // awaits, and a stored card handed in between would find it streaming and stay in its memory.
  #turnStartingAt: number | undefined;

  constructor(host: UnwrittenFramesHost) {
    this.#host = host;
  }

  /**
   * Hands the host a card for `session`, for the frame `identity` under `claim`: one that shows
   * it, or a failure notice naming it. `queued`: the card is shown only once the host writes it
   * (`written`), and the claim, when there is one, waits until then. `shown`: a card of a frame
   * the ledger keeps nothing of, which nothing waits on, so its claim settles at once. A send
   * that throws leaves nothing waiting and releases the claim, so the listener sends the frame
   * again.
   *
   * `closed`: the host is not on `session` (`UnwrittenFramesHost.hostIsOn`), so nothing is handed
   * and nothing is marked shown - the host writes what it is handed in the conversation it has,
   * under the name of the session the card is for. The claim goes back with nothing recorded
   * (`cardNotShown`), and the frame is shown again only by whatever sends it again: a caller
   * names a session other than the host's own for a Dispatch delivery and for a replayed frame
   * alone (`cardSession` and `showReplayCard` in `../extensions/envoy.ts`), and each of those is
   * sent again to the session it names. Every caller is answered this, so none has to
   * read the session for itself.
   *
   * An inert or a stored card sent while the host is not settled is not handed: it waits,
   * `queued`, and the first settled moment hands it (`handBackWithdrawn`, `storedCheck`, and for a
   * stored card a card that starts a turn, which hands it first). One whose frame the ledger keeps
   * nothing of has nothing to wait under, so its claim is released and the listener sends the
   * frame again. A stored card is held as well while a card that starts a turn is on its way to the
   * host (`#turnStarting`).
   */
  sendCard(
    session: string,
    content: string | readonly ContentBlock[],
    deliverAs: CardDelivery,
    identity: DedupeIdentity | undefined,
    claim: DeliveryClaim | undefined
  ): "queued" | "shown" | "closed" {
    if (!this.#host.hostIsOn(session)) {
      this.#host.cardNotShown(session, identity);
      claim?.release();
      return "closed";
    }
    const settled = this.#host.settled();
    const startsTurn = (deliverAs === "card" || deliverAs === "aside") && settled;
    const hand =
      deliverAs === "stored" ? settled && !this.#turnStarting() : deliverAs !== "inert" || settled;
    // The stored cards held while the host was busy go first, in the order they came, so the
    // turn this card starts, or the next turn after this stored card, reads them before it.
    if ((startsTurn || (deliverAs === "stored" && hand)) && !this.#turnStarting()) {
      this.#handHeldStored();
    }
    const keys = identity === undefined ? [] : unwrittenCardKeys(ledgerIdentity(identity));
    const card: UnwrittenCard | undefined =
      identity === undefined || keys.length === 0
        ? undefined
        : {
            sessionID: session,
            identity,
            claim,
            keys,
            content,
            deliverAs,
            handedAt: hand ? this.#moments : undefined,
          };
    if (card !== undefined) this.#awaitWrite(card);
    if (!hand) {
      if (card === undefined) claim?.release();
      return "queued";
    }
    try {
      this.#host.handCard(content, deliverAs, identity);
    } catch (error) {
      if (card !== undefined) this.#stopAwaitingWrite(card);
      claim?.release();
      throw error;
    }
    if (startsTurn) this.#turnStartingAt = this.#moments;
    if (card !== undefined) return "queued";
    // The ledger keeps nothing of the frame, so there is nothing to record.
    claim?.settle();
    return "shown";
  }

  /** Whether a card of the frame `identity` waits to be written: that card shows it. */
  waitsOnCard(identity: DedupeIdentity): boolean {
    return unwrittenCardKeys(ledgerIdentity(identity)).some((key) => this.#cards.has(key));
  }

  /** Whether a stored card of the session this instance serves waits, held for a settled host or
   *  handed and not yet in the session's store (`storedCheck`). */
  waitsOnStored(): boolean {
    const sessionID = this.#host.sessionID();
    return [...this.#cards.values()].some(
      (card) => card.sessionID === sessionID && card.deliverAs === "stored"
    );
  }

  /**
   * Hands the host a replay notice (`stream_recreated`, `retention_gap`) for the session this
   * instance serves now, at once when the host is settled; otherwise it is held, after any held
   * before it, until the first settled moment (`handBackWithdrawn`). A notice the host cannot be
   * handed is held too (`handNoticeFailed`). Nothing waits on a notice's write. A notice held is
   * recorded in the session's transcript (`UnwrittenFramesHost.recordNotice`); one that cannot be
   * is held in this process only (`recordNoticeFailed`).
   */
  sendNotice(content: string): void {
    const notice: HeldNotice = {
      id: crypto.randomUUID(),
      sessionID: this.#host.sessionID(),
      content,
      recorded: false,
    };
    this.#notices.push(notice);
    if (this.#host.settled()) this.#handNotices();
    if (!this.#notices.includes(notice)) return;
    try {
      this.#host.recordNotice(notice.id, content);
      notice.recorded = true;
    } catch (error) {
      this.#host.recordNoticeFailed(error);
    }
  }

  /**
   * Holds again, for the session this instance serves now, the notices its transcript records as
   * held and not handed (`heldReplayNotices`), after those held already, until the first settled
   * moment hands them. They are recorded already; one held for this session already, or handed
   * for it by this process, is passed over, so a restore that reads the same transcript twice
   * holds each once. One held for another session is held for this one too: a fork's transcript
   * copies the entries of the session it forked from, whose hold a session change drops
   * (`releaseOtherSessions`).
   */
  requeueNotices(notices: readonly HeldReplayNotice[]): void {
    const sessionID = this.#host.sessionID();
    for (const { id, content } of notices) {
      if (
        this.#handedNotices.has(JSON.stringify([sessionID, id])) ||
        this.#notices.some((held) => held.sessionID === sessionID && held.id === id)
      ) {
        continue;
      }
      this.#notices.push({ id, sessionID, content, recorded: true });
    }
  }

  /**
   * A person's turn `messageID` was handed to the host, which writes it as a user message. A turn
   * of a frame with no identity gives the ledger nothing to record, so nothing waits on its write:
   * its claim settles at once.
   */
  awaitTurn(messageID: string, turn: HandedTurn): void {
    const { identity } = turn;
    if (identity === undefined) {
      turn.claim?.settle();
      return;
    }
    this.#turns.set(messageID, {
      ...turn,
      identity,
      handedAt: turn.via === "text" ? this.#moments : undefined,
    });
  }

  /** The host answered that typed input `messageID` submitted a message: it is in the host's
   *  queue from now on, unless its message was written already. */
  turnSubmitted(messageID: string): void {
    const waiting = this.#turns.get(messageID);
    if (waiting !== undefined) this.#turns.set(messageID, { ...waiting, handedAt: this.#moments });
  }

  /** A person's turn the host wrote, or answered that it submitted no message for: done waiting. */
  async turnHandled(messageID: string): Promise<void> {
    const turn = this.#turns.get(messageID);
    if (turn === undefined) return;
    this.#turns.delete(messageID);
    await this.#recordWritten(turn);
  }

  /**
   * The host wrote `message` to the transcript as it ended it (`message_end`): a card or a
   * person's turn handed to it, idle or taken by a turn after it streamed, is shown from here on.
   * A card is found by the frame its `details` or text name (`writtenCardIdentity`), a turn by
   * the host's `matchTurn`.
   */
  async written(message: unknown): Promise<void> {
    const card = writtenCardIdentity(message);
    const waiting = unwrittenCardKeys(card ?? {})
      .map((key) => this.#cards.get(key))
      .find((found) => found !== undefined);
    if (waiting !== undefined) {
      // A card that starts a turn is written once that turn takes it, or once the host folds it
      // with no turn: either way no turn is on its way any more.
      if (waiting.deliverAs === "card" || waiting.deliverAs === "aside") {
        this.#turnStartingAt = undefined;
      }
      this.#stopAwaitingWrite(waiting);
      await this.#recordWritten(waiting);
    }
    if (this.#turns.size === 0) return;
    const turn = this.#host.matchTurn(message);
    if (turn !== undefined) await this.turnHandled(turn);
  }

  /** A run ended aborted: it may have taken back the cards it held, and those handed before its
   *  end are looked at by the next `handBackWithdrawn`. */
  runAborted(): void {
    this.#moments += 1;
  }

  /** A run started (`agent_start`): a card handed to start a turn has started it, or another run
   *  took its place, and a stored card held for it waits for that run's end. */
  runStarted(): void {
    this.#turnStartingAt = undefined;
  }

  /**
   * Hands the stored cards held while the host was busy once the host is settled, in the order
   * they came, and records as written each stored card of this session whose entry the session's
   * store holds (`UnwrittenFramesHost.storedEntries`): the host emits no `message_end` for a
   * stored card, so that store is its one witness. The store holds a session from its first
   * assistant message on, so a stored card handed before that waits for it. A card is the newest
   * entry the host records when it stores it, so the entries are read from their end, and only
   * until each handed card is found. Answers whether a stored card of this session still waits,
   * held or handed, so the caller checks again later.
   */
  async storedCheck(): Promise<boolean> {
    if (this.#host.settled() && !this.#turnStarting()) this.#handHeldStored();
    const sessionID = this.#host.sessionID();
    const handed = [...new Set(this.#cards.values())].filter(
      (card) =>
        card.sessionID === sessionID && card.deliverAs === "stored" && card.handedAt !== undefined
    );
    const entries = handed.length === 0 ? undefined : this.#host.storedEntries();
    if (entries !== undefined) {
      const unfound = new Set(handed);
      for (let at = entries.length - 1; at >= 0 && unfound.size > 0; at -= 1) {
        const keys = unwrittenCardKeys(writtenCardIdentity(entries[at]) ?? {});
        for (const card of unfound) {
          if (card.keys.some((key) => keys.includes(key))) unfound.delete(card);
        }
      }
      const written = handed.filter((card) => !unfound.has(card));
      for (const card of written) this.#stopAwaitingWrite(card);
      for (const card of written) await this.#recordWritten(card);
    }
    return this.waitsOnStored();
  }

  /**
   * A run's end stopped the search for this session's `pi.sendUserMessage` turns: each still
   * waiting is done with now, found or not. The host keeps no mark of such a turn, so its text was
   * the only way to tell it was written.
   */
  async textTurnsEnded(): Promise<void> {
    for (const [messageID, turn] of this.#turns) {
      if (turn.via === "text" && turn.sessionID === this.#host.sessionID()) {
        await this.turnHandled(messageID);
      }
    }
  }

  /**
   * Counts a host moment (each heartbeat tick) and, when the host is settled
   * (`UnwrittenFramesHost.settled`), hands the inert and stored cards and the replay notices held
   * while the host was busy (`sendCard`, `sendNotice`) and hands back what an interrupt withdrew.
   *
   * Oh My Pi withdraws every card a streaming turn holds, writing none and restoring none, when
   * the person interrupts the turn (Escape; RPC `abort_and_restore_queue`), and is idle with an
   * empty queue from then on; after a plain abort it keeps them queued and writes them in the run
   * it starts at once (measured on omp 18.8.9: `hasPendingMessages()` false, then true, at the
   * aborted run's `agent_end`). So a card still unwritten while the host is idle and holds no
   * message was withdrawn: it is handed again, as it was, its claim kept. One handed since the
   * host's last moment is left alone, as it may still be on its way into the queue:
   * `pi.sendMessage` returns before the host queues the card or starts its run. A held inert or
   * stored card was never handed, so any settled moment hands it (a stored card not while a card
   * that starts a turn is on its way), and so is each held notice, once. A stored card handed is
   * left alone: no `message_end` comes for it, and the session's store is what says it was written
   * (`storedCheck`). A card or a notice that cannot be handed keeps waiting as it was
   * (`handBackFailed`, `handNoticeFailed`), and the next settled moment tries again.
   *
   * The interrupt withdraws a person's turn the same way: a typed input (`pi.sendUserInput`,
   * answered `handled: "prompt"`) or a `pi.sendUserMessage` turn the streaming turn held goes back
   * to whoever interrupted (measured on omp 18.8.9: RPC `abort_and_restore_queue` answers it in
   * its `steering` list, the terminal puts it in the editor) and is never written. Such a turn is
   * not handed again, since run late it could act on an instruction the person no longer means:
   * its claim is released and nothing records it, so the listener sends the frame again and it
   * is shown as a card that runs nothing. That holds for a typed input only. A `pi.sendUserMessage` turn (sent
   * only on a host without `pi.sendUserInput`) never gets here: the aborted run's `agent_end` calls
   * `textTurnsEnded` right after `runAborted` (extensions/envoy.ts), which records the turn as
   * shown, since the host keeps no mark that would tell a withdrawn one from a written one: the
   * stream's copy of such a turn is never shown again, and only the person who interrupted got
   * it back.
   */
  handBackWithdrawn(): void {
    const since = this.#moments;
    this.#moments += 1;
    if (!this.#host.settled()) return;
    this.#handNotices();
    const sessionID = this.#host.sessionID();
    const stores = !this.#turnStarting();
    for (const card of new Set(this.#cards.values())) {
      if (card.sessionID !== sessionID) continue;
      if (card.deliverAs === "stored") {
        if (card.handedAt !== undefined || !stores) continue;
      } else if (card.handedAt !== undefined && card.handedAt >= since) {
        continue;
      }
      this.#handAgain(card);
    }
    for (const [messageID, turn] of this.#turns) {
      if (turn.sessionID !== sessionID || turn.handedAt === undefined || turn.handedAt >= since) {
        continue;
      }
      this.#turns.delete(messageID);
      this.#host.turnWithdrawn(turn.sessionID, messageID);
      turn.claim?.release();
    }
  }

  /**
   * The host moved to `sessionID`: what the session it left was handed and has not written is no
   * longer waited on, since the host dropped it with the session (measured on omp 18.8.9: a card a
   * turn held when RPC `new_session` ran got no `message_end`). Its claim is released and nothing
   * records it, so the stream keeps its copy and the session's own consumer relays it again once
   * the host is back on that session or resumes it, to be waited on anew. A notice held for the session it left is dropped:
   * it concerns only the session it was raised for, whose transcript records it, so the restore
   * once the host is back on that session holds it again (`requeueNotices`).
   */
  releaseOtherSessions(sessionID: string): void {
    for (const waiting of [this.#cards, this.#turns]) {
      for (const [key, frame] of waiting) {
        if (frame.sessionID === sessionID) continue;
        waiting.delete(key);
        frame.claim?.release();
      }
    }
    this.#notices = this.#notices.filter((notice) => notice.sessionID === sessionID);
  }

  // Hands the held notices of the session this instance serves, in order, and marks each the
  // transcript records held as handed. One the host cannot be handed stays held, with every notice
  // after it, so the next settled moment keeps their order.
  #handNotices(): void {
    const sessionID = this.#host.sessionID();
    for (let index = 0; index < this.#notices.length; ) {
      const notice = this.#notices[index];
      if (notice === undefined || notice.sessionID !== sessionID) {
        index += 1;
        continue;
      }
      try {
        this.#host.handNotice(notice.content);
      } catch (error) {
        this.#host.handNoticeFailed(error);
        return;
      }
      this.#notices.splice(index, 1);
      if (!notice.recorded) continue;
      this.#handedNotices.add(JSON.stringify([notice.sessionID, notice.id]));
      try {
        this.#host.noticeHanded(notice.id);
      } catch (error) {
        this.#host.recordNoticeFailed(error);
      }
    }
  }

  // Whether a card handed to start a turn may still be on its way to that turn (`#turnStartingAt`):
  // until its run starts, the host writes it, or the next moment after the one it was handed in
  // has passed, which bounds a hand-off the host dropped with no sign.
  #turnStarting(): boolean {
    return this.#turnStartingAt !== undefined && this.#moments - this.#turnStartingAt < 2;
  }

  // Hands the stored cards of this session held while the host was busy, in the order they came.
  #handHeldStored(): void {
    const sessionID = this.#host.sessionID();
    for (const card of new Set(this.#cards.values())) {
      if (
        card.sessionID === sessionID &&
        card.deliverAs === "stored" &&
        card.handedAt === undefined
      ) {
        this.#handAgain(card);
      }
    }
  }

  // Hands a waiting card to the host as it was, from this moment on. One the host cannot be
  // handed keeps waiting as it was, and the next settled moment tries again.
  #handAgain(card: UnwrittenCard): void {
    const again: UnwrittenCard = { ...card, handedAt: this.#moments };
    this.#awaitWrite(again);
    try {
      this.#host.handCard(card.content, card.deliverAs, card.identity);
    } catch (error) {
      this.#stopAwaitingWrite(again);
      this.#awaitWrite(card);
      this.#host.handBackFailed(card.identity, error);
    }
  }

  // The host wrote what it was handed: the record of the frame is asked for under the session
  // the frame names, the one it was shown in, so the stream's copy is never shown again, here or
  // after a restart, and its claim settles. No test of this instance's own id belongs here:
  // `DeliveryLedger.recordDeliveredFor` makes the test itself, and records nothing, once the
  // session it is handed is not the one this process writes its transcript to.
  // A card the host wrote in the window `sendCard` admits one for - the host has moved, the
  // switch event has not reached this instance - is in the file of the conversation it was shown
  // in, and that card is the record that conversation's own restore reads
  // (`writtenCardIdentity`, in `./missed-messages`).
  async #recordWritten(frame: Unwritten): Promise<void> {
    await this.#host.recordDelivered(frame.sessionID, frame.identity);
    frame.claim?.settle();
  }

  // A card waits under every key of its frame, and stops waiting only as the entry it is.
  #awaitWrite(card: UnwrittenCard): void {
    for (const key of card.keys) this.#cards.set(key, card);
  }

  #stopAwaitingWrite(card: UnwrittenCard): void {
    for (const key of card.keys) {
      if (this.#cards.get(key) === card) this.#cards.delete(key);
    }
  }
}
