import { envoyNoticeText } from "./delivery-ledger";
import { messageFor } from "./errors";
import { EnvoyApiError, isBusUnanswered } from "./transport";

/**
 * The kind of failure an outage report (`inboundUnavailable`) tells a session of, sorted once,
 * where the failure is first seen: `refused` reads it from the typed error, and `noRelay` is its
 * own kind. Each host looks up the words of its own texts by it (a `Record` of every kind), and so
 * do `inboundUnavailableNoticeText` and `inboundRestoredNoticeText` for the session's notices, so
 * no host reads a message or a reason to tell the kinds apart.
 *
 * - `unrecorded`: the listener answered the registration with the bus-outage 503
 *   (`isBusUnanswered`). Its message bus did not answer in time, so the listener may or may not
 *   have recorded the registration. The client cannot tell which of the registration's calls
 *   failed. A retry that got no answer after that 503 may have landed (`busUnansweredStands`),
 *   and the session's consumer may well exist and deliver. So the session is told that its
 *   registration is not recorded for certain, and not that it has no consumer. The accepted
 *   registration after it is told as one the listener recorded, and not as a session that
 *   receives again. An accepted registration is answered only once the listener wrote its
 *   record, so that is true, except on a listener whose known-session store is not open, which
 *   records nothing and answers 200 all the same (the listener's `knownRecordWriteTimeout`).
 * - `github_withheld`: an edge listener refused the registration 503 `repositories_unknown`
 *   (`EnvoyApiError.reason`). The edge could not read the repositories of the session's projects.
 *   So it follows none of the session's GitHub topics and withholds their messages. Where it
 *   already held a registration of the session, it renews that entry and keeps the session's
 *   consumer, which goes on delivering the session's other messages. Where it held none, it
 *   creates none, and the session receives nothing (the listener's `answerRepositoryOutage` and
 *   `renewThroughOutage`). The client cannot tell which case it is in, so the session is told
 *   what it receives in each case. The accepted registration after it is told as the GitHub
 *   topics followed again, and not as a session that receives again: the edge acknowledged each
 *   GitHub message it withheld without delivering it, so such a message may be missing.
 * - `refused`: the listener refused the registration by any other reason `ErrorWireSchema.reason`
 *   lists (`transport.ts`), or the registration failed. The listener gave the session no consumer,
 *   which is the only path an Envoy message reaches it by, so it receives nothing until a
 *   registration is accepted.
 * - `no_relay`: no registration was sent, because the session holds no relay subject
 *   (`InboundUnavailable.noRelay`): its relay never opened, or one that was open was lost. It
 *   receives nothing until a registration is accepted, as after `refused`.
 *
 * The kinds say how little reaches the session (`OUTAGE_DEPTH`): everything may still reach it
 * after `unrecorded`, its other messages may after `github_withheld`, and nothing does after
 * `refused` or `no_relay`. An outage is told once, and a later failure is told once more only
 * where it lets less reach the session than the told one.
 */
export type InboundOutageKind = "unrecorded" | "github_withheld" | "refused" | "no_relay";

/**
 * One failure an outage report told a session of (`inboundUnavailable`): its kind, and `why`, the
 * error's message (`messageFor`). For a refusal, `why` is the listener's own message, which
 * carries its machine-readable reason.
 */
export interface InboundOutage {
  readonly kind: InboundOutageKind;
  readonly why: string;
}

/**
 * The reason an edge listener refuses a registration 503 with when it could not read the
 * repositories of the session's projects (`ErrorWireSchema.reason`; the listener's
 * `edgeRepositoriesUnknown`).
 */
const REPOSITORIES_UNKNOWN = "repositories_unknown";

/** The kind of a registration the listener refused or that failed (`InboundOutageKind`). */
function refusalKind(error: unknown): InboundOutageKind {
  if (isBusUnanswered(error)) return "unrecorded";
  if (error instanceof EnvoyApiError && error.reason === REPOSITORIES_UNKNOWN) {
    return "github_withheld";
  }
  return "refused";
}

/** How much each kind of outage keeps from the session (`InboundOutageKind`): more at 2 than 0. */
const OUTAGE_DEPTH: Record<InboundOutageKind, number> = {
  unrecorded: 0,
  github_withheld: 1,
  refused: 2,
  no_relay: 2,
};

/** The words of a session that has no consumer: a refusal's, and a missing relay subject's. */
const NO_CONSUMER_NOTICE = {
  notice: "Envoy inbound unavailable",
  detail:
    "The listener gave this session no durable consumer, which is the only path an Envoy message reaches it by, so it receives none. Everything else works, and the session registers again at every heartbeat; what the stream stored meanwhile arrives once a registration is accepted.",
} as const;

/** The `notice:` and `detail:` of what a session is told of each kind of outage. */
const UNAVAILABLE_NOTICES: Record<
  InboundOutageKind,
  { readonly notice: string; readonly detail: string }
> = {
  unrecorded: {
    notice: "Envoy inbound unavailable",
    detail:
      "This session's registration is not recorded for certain: the listener's message bus did not answer in time, so the listener may or may not have recorded it. Envoy messages may still reach the session, but nothing confirms it until a registration is accepted. Everything else works, and the session registers again at every heartbeat.",
  },
  github_withheld: {
    notice: "Envoy GitHub topics withheld",
    detail:
      "The listener could not read the repositories of this session's projects. So it follows none of this session's GitHub topics now, and it withholds their messages. It refused this registration. If the listener already held a registration of this session, it kept that registration and the session's consumer, so the session receives the messages of every other topic the listener follows for it; a topic the session began to follow after its last accepted registration is followed only once a registration is accepted. If the listener held no registration of this session, the session receives nothing until a registration is accepted. The session registers again at every heartbeat.",
  },
  refused: NO_CONSUMER_NOTICE,
  no_relay: NO_CONSUMER_NOTICE,
};

/** The words of a session that receives again after it received nothing. */
const RECEIVES_AGAIN_NOTICE = {
  notice: "Envoy inbound restored",
  detail:
    "The listener accepted a registration of this session, so the session receives Envoy messages again. What the stream stored while it received nothing arrives now.",
} as const;

/** The `notice:` and `detail:` of what a session is told once an outage of each kind ends. */
const RESTORED_NOTICES: Record<
  InboundOutageKind,
  { readonly notice: string; readonly detail: string }
> = {
  unrecorded: {
    notice: "Envoy inbound restored",
    detail:
      "The listener recorded a registration of this session. A message the stream stored that has not reached the session arrives now.",
  },
  github_withheld: {
    notice: "Envoy GitHub topics followed again",
    detail:
      "The listener accepted a registration of this session, so it follows this session's GitHub topics again, and their messages reach the session again. A GitHub message the listener withheld meanwhile may be missing.",
  },
  refused: RECEIVES_AGAIN_NOTICE,
  no_relay: RECEIVES_AGAIN_NOTICE,
};

/**
 * What a session is told of an outage (`inboundUnavailable`'s `tell`), worded once for every host
 * by its kind (`InboundOutageKind`), with `why`, the failure's own message. Each host shows it where
 * it shows a gap notice.
 */
export function inboundUnavailableNoticeText({ kind, why }: InboundOutage): string {
  const { notice, detail } = UNAVAILABLE_NOTICES[kind];
  return envoyNoticeText({ notice, error: why, detail });
}

/**
 * What a session is told when a registration is accepted after an outage it was told of
 * (`inboundUnavailable`'s `restored`), worded once for every host by the kind of that outage
 * (`InboundOutageKind`), and shown where that outage's notice was shown.
 */
export function inboundRestoredNoticeText(kind: InboundOutageKind): string {
  const { notice, detail } = RESTORED_NOTICES[kind];
  return envoyNoticeText({ notice, detail });
}

/**
 * How long a session that has had no accepted registration yet may hold no relay subject before
 * it is told that it receives nothing (`inboundUnavailable`). At a pod's start the first NATS
 * dial can fail, and the relay opens on a later try. That race closes within about a minute
 * (dispatch://INFRA-3589, R24): on 2026-11-16 an Oh My Pi session whose NATS was held closed
 * for its first pass took 67 s from its start to its first accepted registration. The bound
 * starts at the first registration that found no relay subject, not at the start.
 *
 * A pass's dial dials again after a failure, for at most 25 s (`connectNats`, `NATS_DIAL_PLAN`),
 * so a pass whose dials all fail registers up to 25 s after it began. pi-envoy starts the next
 * pass at the first 15 s tick after the last one ended (`NATS_RETRY_INTERVAL_MS`). So a real
 * outage is told within about three minutes of the start, inside the listener's 300 s live
 * window. A session whose NATS connection presents its broker token (`NATS_AUTH=broker`) waits up
 * to 20 s for the token before each dial (`TRADE_DEADLINE_MS`), so each of its passes takes up to
 * 20 s more, and its outage is told within about four minutes of the start, still inside that
 * window.
 */
export const INBOUND_FIRST_OPEN_BOUND_MS = 120_000;

/** One session's outage report (`inboundUnavailable`). */
export interface InboundUnavailable {
  /**
   * No registration was sent, because the session holds no relay subject (`no_relay`). Before
   * the session's first accepted registration this is the start race, held until the bound
   * passes; after it, a relay that was open was lost, which tells the session at once. Either
   * way it is told once per outage, and once more after a told outage that let more reach the
   * session (`InboundOutageKind`). The first call of the start race goes to the host's `held` at
   * once, with `relayMissing`, so the host can log it while the session's notice waits.
   * `relayMissing` is why the relay subject could not be opened, as the host said it
   * (`RelayRegistration.relayMissing`), undefined when the host named no reason.
   */
  noRelay(error: unknown, relayMissing?: string): void;
  /**
   * A registration was refused or failed: its kind is read from `error` (`InboundOutageKind`),
   * and the session is told at once, through the host's `tell`, unless this outage already told
   * it. A registration the listener answered with the bus-outage 503 (`unrecorded`) is held
   * instead, as the start race is: the first such refusal since the last accepted registration
   * starts the bound, and only one at or after its end tells the session. A refusal that lets
   * less reach the session than the told outage is told once more.
   */
  refused(error: unknown): void;
  /**
   * A registration was accepted: an outage the session was told of ends with `restored`, which
   * takes the last outage that was told, and the next outage tells the session again. An outage
   * held and never told ends with nothing said.
   */
  accepted(): void;
}

/** What `inboundUnavailable` takes besides `tell`. */
export interface InboundUnavailableOptions {
  /** The clock the bound is read on; `Date.now` unless a test passes its own. */
  readonly now?: () => number;
  /**
   * How long the start race, and a run of registrations the bus-outage 503 answers, is held
   * (`INBOUND_FIRST_OPEN_BOUND_MS` unless a test passes one).
   */
  readonly firstOpenBoundMs?: number;
  /**
   * Tells the session once that an outage it was told of has ended, because a registration was
   * accepted. `told` is the last outage `tell` took.
   */
  readonly restored?: (told: InboundOutage) => void;
  /**
   * Called once, at once, when the start race's hold begins: the first registration before the
   * session's first accepted one that found no relay subject. `relayMissing` is why the relay
   * subject could not be opened (`noRelay`'s `relayMissing`), undefined when the host
   * named no reason. The session is told nothing yet, so a host logs it here: its log then shows
   * at once that a registration found no relay subject, while the notice waits for the bound.
   * It is not called for a call the bound does not hold, or once `stopping` answers true.
   */
  readonly held?: (relayMissing: string | undefined) => void;
  /**
   * Whether the session has begun to stop; once it answers true, the report tells nothing, neither
   * `tell` nor `restored`, and calls no `held`. A stopping session closes its own relay on
   * purpose, and a registration refused, answered or skipped during the stop says nothing about
   * what the session receives, since its record is deleted next.
   */
  readonly stopping?: () => boolean;
}

/**
 * Tells the session once per outage that it has no accepted registration, and once that a
 * registration was accepted after that. The listener refuses a registration it can give no
 * consumer, and the host registers again at every heartbeat, so a host that told the session each
 * time would tell it every tick for as long as the outage lasts; one that never told it again
 * would leave a second outage silent. `tell` takes the outage, its kind and the failure's own
 * message (`InboundOutage`). Each host words it with `inboundUnavailableNoticeText` and shows that
 * where it shows a gap notice. `restored` takes that same outage, and the host words its kind
 * with `inboundRestoredNoticeText` in the same place.
 *
 * A session whose first relay open is still being tried holds no relay subject for a while at
 * its start, and it receives once the relay opens. So until the session's first accepted
 * registration, an outage with no relay subject is held: the first such call starts the bound,
 * and only a call at or after its end tells the session. That first call goes to `held` at once,
 * so the host's log says that a registration found no relay subject while the notice waits. A
 * registration the listener answered with the bus-outage 503 is held the same way, at any time:
 * it is not recorded for certain, the session's consumer may still deliver, and the next
 * heartbeat's registration is usually accepted, so the first such refusal since the last
 * accepted registration starts the bound and an accepted registration inside it ends the hold
 * with nothing said. Any other refusal is told at once, as is an outage with no relay subject
 * after the first accepted registration.
 *
 * Each outage is told once, with one exception: a failure that lets less reach the session than
 * the told one is told once more (`InboundOutageKind`), and `restored` takes that later outage. A
 * session that has begun to stop (`stopping`) is told nothing at all.
 */
export function inboundUnavailable(
  tell: (outage: InboundOutage) => void,
  {
    now = () => Date.now(),
    firstOpenBoundMs = INBOUND_FIRST_OPEN_BOUND_MS,
    restored,
    held,
    stopping = () => false,
  }: InboundUnavailableOptions = {}
): InboundUnavailable {
  // The outage this report told the session last, until a registration is accepted.
  let told: InboundOutage | undefined;
  let everAccepted = false;
  let heldSince: number | undefined;
  let unansweredSince: number | undefined;
  // A failure that lets less reach the session than the told one is told once more.
  const say = (kind: InboundOutageKind, error: unknown) => {
    if (stopping()) return;
    if (told !== undefined && OUTAGE_DEPTH[kind] <= OUTAGE_DEPTH[told.kind]) return;
    told = { kind, why: messageFor(error) };
    tell(told);
  };
  return {
    noRelay(error, relayMissing) {
      if (!everAccepted) {
        const at = now();
        const begins = heldSince === undefined;
        heldSince ??= at;
        if (at < heldSince + firstOpenBoundMs) {
          if (begins && !stopping()) held?.(relayMissing);
          return;
        }
      }
      say("no_relay", error);
    },
    refused(error) {
      const kind = refusalKind(error);
      if (kind === "unrecorded") {
        const at = now();
        unansweredSince ??= at;
        if (at < unansweredSince + firstOpenBoundMs) return;
      }
      say(kind, error);
    },
    accepted() {
      everAccepted = true;
      unansweredSince = undefined;
      const last = told;
      if (last === undefined || stopping()) return;
      told = undefined;
      restored?.(last);
    },
  };
}
