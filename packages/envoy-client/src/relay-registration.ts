// packages/envoy-client/src/relay-registration.ts
//
// One registration of a session that holds a relay, as every host makes it: the topics the
// listener will store for the session, the relay's delivery request, the outage told once
// (`inboundUnavailable`, `./inbound-outage`), and, where an edge listener refuses topics of no
// project of the session's token, exactly those topics dropped, the session told which, and the
// rest registered again.

import type { ConsumerDelivery } from "./consumer-delivery";
import { envoyNoticeText } from "./delivery-ledger";
import type { InboundUnavailable } from "./inbound-outage";
import { registrationTopics } from "./stream-topics";
import { type DeliveryRequest, EnvoyApiError, expandSubscriptionTopics } from "./transport";

/** What a registration needs of a session's relay; `ConsumerDelivery` is one. */
export type RegisteringRelay = Pick<ConsumerDelivery, "setFollowed" | "unfollow" | "registration">;

/** What `registerWithRelay` is handed; `Result` is the host client's registration answer. */
export interface RelayRegistration<Result> {
  /**
   * The relay this registration asks the listener for a consumer on, or `undefined` when the
   * session holds no relay subject (no NATS connection yet, or inbound off for this session).
   */
  readonly relay: RegisteringRelay | undefined;
  /**
   * Why the session holds no relay subject when `relay` is undefined, in words a notice may hold
   * (`natsFailureReason` of the failure of the NATS call that would have opened it); the refusal and
   * the outage notice name it, and the outage report hands it to the host's `held`
   * (`InboundUnavailable.noRelay`). Undefined when the host has no failure to name.
   */
  readonly relayMissing?: string;
  /** This session's own agent subject, which every registration carries. */
  readonly ownAgentSubject: string;
  /** Every topic the session follows now, filtered by `registrationTopics`: read when the
   *  registration starts and again before each resend, so a topic the session stopped following
   *  while this one waited is not named again. */
  readonly followed: () => Iterable<string>;
  /** The Legion roles this session holds, which decide the controller notice lane it may follow. */
  readonly roles?: readonly string[];
  /** This session's outage report, told once per outage. */
  readonly outage: InboundUnavailable;
  /** Sends the registration carrying these topics and this delivery request. */
  readonly send: (topics: readonly string[], delivery: DeliveryRequest) => Promise<Result>;
  /**
   * Takes the answer's consumer, once the registration was accepted: the host drops it when it
   * has since left the session or closed the relay this asked for.
   */
  readonly answered: (result: Result) => void;
  /**
   * Stops following these topics, which an edge listener refused for the projects of this
   * session's token (`projectRefusedTopics`) for `reason`, and tells the session which, once, where
   * it tells it an outage (`refusedTopicsNoticeText`): the host takes them out of what it follows,
   * so no later registration of its carries them, and the session does not go on believing it
   * follows them. `registerWithRelay` sends the rest at once, whichever path registered, unless
   * the host's stop began (`stopping`).
   */
  readonly drop: (topics: readonly string[], reason: string) => void;
  /** Where the one line naming the topics `drop` was given goes. */
  readonly log: (line: string) => void;
  /**
   * The host's own reading that the session's stop began. Once it answers true, no registration
   * is sent again and no topic is dropped (R24 of dispatch://INFRA-3589): a stopping session
   * closes its relay on purpose, and its record is deleted next. When it is not given, the
   * session never reads as stopping.
   */
  readonly stopping?: () => boolean;
}

/**
 * What a session is told of the topics an edge listener refused for the projects of its token
 * (`reason`, `topic_not_in_project` or `repository_not_in_project`), worded once for every host:
 * it no longer follows them, and every other topic it follows stays.
 */
export function refusedTopicsNoticeText(topics: readonly string[], reason: string): string {
  return envoyNoticeText({
    notice: "Envoy topics refused",
    topics: topics.join(", "),
    reason,
    detail:
      "The listener delivers to this session only the topics of its token's projects, so it refused these, and this session no longer follows them. Every other topic it follows stays followed and goes on being delivered.",
  });
}

/** The reasons an edge listener refuses a registration for a topic of no project of the session's
 *  token, naming each such topic in `refused` (`ErrorWireSchema.reason`). */
const PROJECT_TOPIC_REFUSALS = ["topic_not_in_project", "repository_not_in_project"] as const;

/**
 * The topics of `topics` a project refusal of the listener's names, with its reason: each topic
 * whose wire forms (`expandSubscriptionTopics`, which the client sends) include a topic the 403's
 * `refused` lists, the session's own agent subject never among them. Undefined for any other
 * failure, a 403 for any other reason included, and for a refusal that names none of `topics` but
 * that subject.
 */
function projectRefusedTopics(
  error: unknown,
  topics: readonly string[],
  ownAgentSubject: string
): { readonly reason: string; readonly topics: string[] } | undefined {
  if (!(error instanceof EnvoyApiError) || error.details.status !== 403) return undefined;
  const reason = PROJECT_TOPIC_REFUSALS.find((refusal) => refusal === error.reason);
  if (reason === undefined) return undefined;
  const refused = new Set(error.refused ?? []);
  const named = topics.filter(
    (topic) =>
      topic !== ownAgentSubject &&
      expandSubscriptionTopics([topic]).some((form) => refused.has(form))
  );
  return named.length === 0 ? undefined : { reason, topics: named };
}

/**
 * `registerWithRelay`'s refusal of a session that holds no relay subject, so a host can tell it
 * from a refusal the listener sent. Its message names why the relay is missing when the host said
 * (`relayMissing`).
 */
export class NoRelaySubjectError extends Error {}

/**
 * Why a registration is refused once its session's stop has begun (R24 of
 * dispatch://INFRA-3589): the stop closes the relay on purpose and deletes the session's record
 * next, so the registration has nothing to ask for. `registerWithRelay` throws it, and so does a
 * host that refuses a registration before it builds one.
 */
export class RegistrationStoppingError extends Error {
  override readonly name = "RegistrationStoppingError";

  constructor() {
    super("this session is stopping, so it registers nothing");
  }
}

/**
 * One registration, as every host that holds a relay makes it: the topics the listener will
 * store (`registrationTopics`), stamped on the relay before the registration carries them, then
 * sent, with the outage told once whichever way it ends.
 *
 * A registration whose host began stopping (`stopping`) before it is sent is refused first, with
 * `RegistrationStoppingError`, whether or not the session holds a relay subject: it sends nothing
 * and tells the outage report nothing.
 *
 * A session with no relay subject open is refused here rather than registered: every
 * registration asks for consumer delivery, a session receives only what its own consumer holds,
 * and one carrying no relay would be refused `409 not_requested` and leave nothing registered.
 * The outage report holds this at the session's start (`InboundUnavailable.noRelay`), and the
 * host's next try opens the relay and registers. The first such refusal of the hold goes to the
 * host's `held` at once, with why the relay is missing (`relayMissing`).
 *
 * An edge listener refuses a whole registration that names one topic of no project of the
 * session's token (`PROJECT_TOPIC_REFUSALS`), and the host sends its topics again at every
 * heartbeat: kept, that one topic would refuse every registration until the entry lapsed and the
 * session received nothing. So the topics the refusal names are dropped, the session told which
 * (`drop`, with the refusal's reason), the plugin's log told once (`log`), and the rest
 * registered again at once; each round drops at least one topic, so it
 * ends. Any other refusal, and a project refusal naming nothing but the session's own agent
 * subject, fails the registration as before.
 *
 * The resend stops at the host's stop (`stopping`). A project refusal answered once the stop
 * began fails the registration as any other refusal does: no topic is dropped, nothing is
 * logged, nothing is sent again, and the outage report gets the refusal, which it tells to no
 * stopping session (`InboundUnavailableOptions.stopping`).
 */
export async function registerWithRelay<Result>({
  relay,
  relayMissing,
  ownAgentSubject,
  followed,
  roles,
  outage,
  send,
  answered,
  drop,
  log,
  stopping = () => false,
}: RelayRegistration<Result>): Promise<Result> {
  if (stopping()) throw new RegistrationStoppingError();
  if (relay === undefined) {
    const refused = new NoRelaySubjectError(
      `this session holds no relay subject, so the listener can give it no consumer and it can receive nothing${relayMissing === undefined ? "" : `, because its relay subject could not be opened: ${relayMissing}`}`
    );
    outage.noRelay(refused, relayMissing);
    throw refused;
  }
  let topics = registrationTopics(ownAgentSubject, followed(), roles);
  relay.setFollowed(topics);
  for (;;) {
    let result: Result;
    try {
      result = await send(topics, relay.registration());
    } catch (error) {
      const refused = projectRefusedTopics(error, topics, ownAgentSubject);
      if (refused === undefined || stopping()) {
        outage.refused(error);
        throw error;
      }
      drop(refused.topics, refused.reason);
      log(
        `the listener refused ${refused.topics.join(", ")} for this session's projects (${refused.reason}); the session no longer follows them and registers the rest`
      );
      // Only the refused topics leave the relay's follows: another registration may have added a
      // topic while this one waited, and the listener, which merges what each registration
      // names into the session's row, goes on relaying it. Taking this call's own list again
      // would drop that topic here, and its frames would be answered done unhandled.
      relay.unfollow(refused.topics);
      // The resend names only this call's topics the session still follows: the listener merges
      // a registration's topics into the row, so one unsubscribed while this call waited would
      // be put back. Leaving a topic out removes nothing there.
      const still = new Set(registrationTopics(ownAgentSubject, followed(), roles));
      topics = topics.filter((topic) => !refused.topics.includes(topic) && still.has(topic));
      continue;
    }
    outage.accepted();
    answered(result);
    return result;
  }
}
