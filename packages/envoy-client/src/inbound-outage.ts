import { envoyNoticeText } from "./delivery-ledger";
import { messageFor } from "./errors";

/**
 * What a session is told when the listener can give it no consumer, worded once for every host:
 * its registration was refused, by one of the reasons `ErrorWireSchema.reason` lists
 * (`transport.ts`), or it failed, so nothing reaches it until a later registration is accepted.
 * `why` is the listener's own message, which carries its machine-readable reason. Each host
 * shows it where it shows a gap notice and says once per outage.
 */
export function inboundUnavailableNoticeText(why: string): string {
  return envoyNoticeText({
    notice: "Envoy inbound unavailable",
    error: why,
    detail:
      "The listener gave this session no durable consumer, which is the only path an Envoy message reaches it by, so it receives none. Everything else works, and the session registers again at every heartbeat; what the stream stored meanwhile arrives once a registration is accepted.",
  });
}

/**
 * What a session that was told its inbound is unavailable is told once a registration is
 * accepted again, worded once for every host: without it the outage notice is the last word the
 * session has, and a session that recovered a few seconds later reads itself as deaf for its life.
 */
export function inboundRestoredNoticeText(): string {
  return envoyNoticeText({
    notice: "Envoy inbound restored",
    detail:
      "The listener accepted this session's registration and gave it its durable consumer, so Envoy messages reach it again, the ones the stream stored during the outage first.",
  });
}

/** One session's outage report (`inboundUnavailable`). */
export interface InboundUnavailable {
  /**
   * A registration was refused or failed: tells the session, through the host's `tell`, unless
   * this outage already told it. A notice the host could not show leaves the session untold, so
   * the next refusal of the same outage tells it again.
   */
  refused(error: unknown): void;
  /** A registration was accepted: ends the outage, saying so at once through the host's
   *  `outageEnded` when the host's log said it began, whichever session is bound by now, and
   *  telling the session so through `restored` when it was shown the outage notice (once that
   *  notice is shown, where the host is still showing it); the next outage tells the session
   *  again. */
  accepted(): void;
  /**
   * The host bound a session and read that session's own record of what it was last told of its
   * inbound: `told` says the record ends with the outage notice and no end after it. From
   * here what the session is told is the bound session's. A told record is an outage an earlier
   * process, or this one before it moved away, told the session and did not end; it is taken up,
   * so a refusal tells nothing new and the next accepted registration tells the session it
   * receives again. A record that is not told leaves the session untold: its next refusal tells it
   * that it receives nothing, and the end of an outage another session was told is never told to
   * it. The session the host left keeps its own record, which tells its end when a process binds
   * it again. The host's log is the process's, not the session's: a bind neither ends an outage it
   * said began nor starts one.
   */
  recordRead(told: boolean): void;
}

/**
 * Says once per outage that the session receives nothing, and that it receives again once a
 * registration is accepted. The listener refuses a registration it can give no consumer, and the
 * host registers again at every heartbeat, so a host that told the session each time would tell
 * it every tick for as long as the outage lasts; one that never told it again would leave a second
 * outage silent; and one that never said the outage ended would leave the session believing it
 * deaf after it recovered. `tell` takes the listener's own message (`messageFor`), which each host
 * words with `inboundUnavailableNoticeText`, and `restored` is worded with
 * `inboundRestoredNoticeText`; each host shows both where it shows a gap notice.
 *
 * A host whose notice is written later than it is asked for (claude-envoy's waits its turn behind
 * every frame queued before it) returns from `tell` a promise that settles once the notice was
 * shown, or rejects when it could not be. An outage notice that was not shown leaves the session
 * untold, as the record a host keeps of it does: the next refusal of the same outage tells it
 * again (`again`), and no accepted registration tells it the end of an outage it never heard of;
 * an end accepted while the notice is still being shown is told only once the notice was. A host
 * whose `tell` returns nothing showed its notice as it returned.
 *
 * What the session was shown is the session's; the host's own log is the process's, and is told
 * of each outage once. `again` is true when the log already said the outage began: an earlier
 * notice of it was not shown, or was shown to the session the host bound before. `outageEnded`,
 * where the host gives one, is called at once at the first accepted registration after the log
 * said an outage began, whichever session is bound by then, and after a told record was taken up,
 * whether or not the session was shown either notice, so a log that said an outage began says
 * that it ended.
 */
export function inboundUnavailable(
  tell: (why: string, again: boolean) => void | Promise<void>,
  restored: () => void,
  outageEnded?: () => void
): InboundUnavailable {
  // Whether the session bound now was told an outage and not its end, by this process or, as its
  // record read at the bind says (`recordRead`), by an earlier one. Set as the outage notice is
  // asked for, and taken back if the host could not show it.
  let told = false;
  // Whether the host's log said an outage began (`tell` was asked for it) and not yet that it
  // ended. The process's, so it outlives an outage notice that was not shown and a bind.
  let logged = false;
  // The outage notice the host is still showing, which settles true once it was shown; undefined
  // when there is none.
  let telling: Promise<boolean> | undefined;
  // Counts every change of `told`, so a notice that settles late acts only on the state it was
  // asked for in, never on one a later bind, end or outage made since.
  let change = 0;
  return {
    refused(error) {
      if (told) return;
      told = true;
      const again = logged;
      logged = true;
      const asked = ++change;
      const showing = tell(messageFor(error), again);
      if (showing === undefined) return;
      telling = showing.then(
        () => {
          if (change === asked) telling = undefined;
          return true;
        },
        () => {
          if (change === asked) {
            told = false;
            telling = undefined;
          }
          return false;
        }
      );
    },
    accepted() {
      if (!logged && !told) return;
      logged = false;
      outageEnded?.();
      if (!told) return;
      told = false;
      const ended = ++change;
      const showing = telling;
      if (showing === undefined) {
        restored();
        return;
      }
      void showing.then((shown) => {
        if (shown && change === ended) restored();
      });
    },
    recordRead(recordTold) {
      told = recordTold;
      change += 1;
      telling = undefined;
    },
  };
}
