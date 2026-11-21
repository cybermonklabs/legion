import {
  AGENT_TOPIC_PREFIX,
  agentSubject,
  canonicalRole,
  claimProject,
  DISPATCH_TOPIC_PREFIX,
  GHOSTWISPR_TOPIC_PREFIX,
  LEGION_NOTICE_TOPIC_PREFIX,
  legacyTwin,
  NO_PROJECT,
  parseRoleClaimKey,
  ROLE_TOPIC_PREFIX,
  roleSubject,
  uncapturedTopicRefusal,
} from "@legion/contracts";

/** A role and the project it is claimed, read and addressed under. */
export interface PlacedRole {
  readonly project: string;
  readonly role: string;
}

/**
 * The project `role` is claimed, read and addressed under. A Legion role token
 * (`legion-<token>-…`) works for the project its own name gives (`roleProject`), whatever the
 * session's token lists. A free-form role name (`reviewer`, `lead-infra`) works for the one project
 * the session's broker token lists, which the listener answers each registration with
 * (`SubscribeResult.projects`), and for `NO_PROJECT` where the token lists none or several, or the
 * listener named none (one that predates the field, or a registration not yet answered). A role
 * already named in the new form (`<project>.<role>`, `canonicalRole`) keeps the project it names,
 * which the listener judges (`contracts.ClaimProject`). The Legion project token
 * (`LEGION_PROJECT`, `infra`) is never a project here: only the listener's list of project keys
 * is read, each held to `claimProject`, which takes nothing but a project key or `NO_PROJECT`.
 */
export function placeRole(role: string, tokenProjects: readonly string[] | undefined): PlacedRole {
  const named = canonicalRole(role);
  if (!named.legacy || named.project !== NO_PROJECT) {
    return { project: named.project, role: named.name };
  }
  const only = tokenProjects?.length === 1 ? tokenProjects[0] : undefined;
  const project = only === undefined ? NO_PROJECT : claimProject(named.name, only);
  return { project: project instanceof Error ? NO_PROJECT : project, role: named.name };
}

/** The name a claim (`POST /v1/roles/set`) and a role read (`GET /v1/roles/<role>`) give a placed
 *  role, its project first: `INFRA.legion-infra-infra-12-architect`, `_.reviewer`. */
export function placedRoleName(placed: PlacedRole): string {
  return `${placed.project}.${placed.role}`;
}

/** A placed role's lane, `notifications.role.<project>.<role>` (`roleSubject`). */
export function placedRoleLane(placed: PlacedRole): string {
  return roleSubject(placed.project, placed.role);
}

/**
 * The role a role lane names, in either form, and undefined for any other topic: the new form
 * `notifications.role.<project>.<role>`, and a lane named by a claim's key, which is the bare role
 * for a claim of the project the role's own name gives (`parseRoleClaimKey`). That is how the
 * listener writes a claim's lane onto its holder's interest record, so a host reads back the role
 * a stored record names here.
 */
export function roleOfLane(topic: string): PlacedRole | undefined {
  if (!topic.startsWith(ROLE_TOPIC_PREFIX)) return undefined;
  const rest = topic.slice(ROLE_TOPIC_PREFIX.length);
  return rest === "" ? undefined : parseRoleClaimKey(rest);
}

/**
 * A session's own agent subject as a host registers, follows and persists it: the session's
 * messages under every project, `notifications.agent.*.<session>`, which is the new form
 * (`canonicalTopic`) of the old `notifications.agent.<session>`. A sender names a message's
 * project (`agentSubject`), so the session's own interest names none of its own: it is the
 * wildcard over every project. The listener filters the session's consumer on every form of it
 * (`bus.SessionConsumerFilters`), so the session receives its messages whichever form a producer
 * still writes; a reader of a delivered subject asks `agentSubjectSession` whose it is.
 */
export function ownAgentSubject(sessionID: string): string {
  return agentSubject("*", sessionID);
}

/**
 * The one session an agent subject names, read in either form through `legacyTwin`: the old
 * `notifications.agent.<session>`, or the new one under a project, `NO_PROJECT` or a wildcard over
 * every project. Undefined for a name of any other family and for one naming more than a session
 * (`notifications.agent.x.y.z`, or a token in the project's place that is none). A wildcard where
 * the session would be is answered as it is written (`*`, `>`), which no session id equals.
 */
export function agentSubjectSession(subject: string): string | undefined {
  const old = legacyTwin(subject) ?? subject;
  if (!old.startsWith(AGENT_TOPIC_PREFIX)) return undefined;
  const session = old.slice(AGENT_TOPIC_PREFIX.length);
  return session === "" || session.includes(".") ? undefined : session;
}

/** Whether `subject` is `sessionID`'s own agent subject, in either form (`agentSubjectSession`). */
export function isOwnAgentSubject(subject: string, sessionID: string): boolean {
  return agentSubjectSession(subject) === sessionID;
}

/** The topic a failed delivery's report goes to is this prefix followed by the original topic. */
export const EXCEPTION_TOPIC_PREFIX = "notifications.envoy.exceptions.";

/**
 * The literal prefixes of the subjects the notification stream (`ENVOY_NOTIFICATIONS`) stores, as
 * the listener's binary compiles them (`streamSubjects` in packages/envoy/internal/bus/stream.go,
 * each a prefix followed by `>`). A session's consumer relays its agent subject and every followed
 * topic under one of them; a topic under none is one the listener's publish route refuses by
 * name and its registration route stores for nobody, since nothing would store it.
 */
const STREAM_SUBJECT_PREFIXES = [
  AGENT_TOPIC_PREFIX,
  DISPATCH_TOPIC_PREFIX,
  "notifications.github.",
  "notifications.slack.",
  LEGION_NOTICE_TOPIC_PREFIX,
  GHOSTWISPR_TOPIC_PREFIX,
  "notifications.whatsapp.",
  "notifications.team.",
  `${EXCEPTION_TOPIC_PREFIX}${AGENT_TOPIC_PREFIX}`,
  `${EXCEPTION_TOPIC_PREFIX}notifications.team.`,
] as const;

/**
 * Whether the stream stores a publish to `topic`, read as the listener's `bus.StreamCaptures`
 * reads it: the topic extends one of the stream's prefixes by at least one token. A `*` or `>` in
 * the topic is read literally, so `notifications.dispatch.issue.X.>` is stored.
 */
export function streamStores(topic: string): boolean {
  return STREAM_SUBJECT_PREFIXES.some(
    (prefix) => topic.length > prefix.length && topic.startsWith(prefix)
  );
}

/**
 * Whether nothing would ever be delivered to a session on `topic`: a `notifications.` topic no
 * stream subject captures and that is no role lane (`bus.ReachesNoSession` in packages/envoy/
 * internal/bus). A session takes its messages from its own consumer, which holds what the stream
 * stored, so such an interest would be stored and deliver nothing. A role lane still travels over
 * core NATS and is registered by its holder (POST /v1/roles/set).
 *
 * POST /v1/interests/subscribe leaves such a topic out of the registration and names it in a
 * warning, the rest of the registration stored; POST /v1/messages/publish refuses such a publish
 * by name.
 */
export function streamDeliversNothing(topic: string): boolean {
  return (
    topic.startsWith("notifications.") &&
    !topic.startsWith(ROLE_TOPIC_PREFIX) &&
    !streamStores(topic)
  );
}

/**
 * Whether a host's subscribe tool refuses `topic` outright: the caller named a topic nothing
 * would ever be delivered on (`streamDeliversNothing`) and the listener would not have left it
 * out as a private stream subject first (`reachesPrivateStreamSubject`, which covers a wildcard
 * such as `notifications.>` or `notifications.*.x`). The listener judges a registration's topics
 * in that order too, warning about a private one as a private one (`subscribeHandler` in
 * packages/envoy/cmd/listener/api.go). A private subject stays followed and merely unregistered,
 * as it always was: a host opens one itself, a fork parent's agent subject among them, where
 * nothing but a caller ever names a topic the stream captures nothing of.
 */
function subscribeRefusesTopic(ownAgentSubject: string, topic: string): boolean {
  return streamDeliversNothing(topic) && !reachesPrivateStreamSubject(ownAgentSubject, topic);
}

/**
 * Throws when a host's subscribe tool was handed a topic it refuses (`subscribeRefusesTopic`),
 * naming every such topic in the listener's own words. Both hosts' tools call it on what their
 * caller named — `envoy_subscribe` in pi-envoy, `follow` in claude-envoy — so a caller reads the
 * same sentence from either, and neither follows a subject nothing is ever delivered on. The
 * sentence is written once, as `uncapturedTopicRefusal` in `@legion/contracts`, which
 * `packages/contracts/scripts/gen-go.ts` emits as `contracts.UncapturedTopicRefusal` for the
 * listener's own warning (`splitUncapturedTopics`), so the tool and the registration read alike:
 * a registration carries topics nobody typed — a stored row, a restored transcript — and is
 * answered with a warning, where a tool call is a caller asking for this one topic by name. A
 * topic a stored row or a restored transcript carries never comes here: those are followed and
 * left out of the registration instead (`registrationTopics`).
 */
export function refuseUncapturedTopics(
  sessionID: string,
  ownAgentSubject: string,
  topics: readonly string[]
): void {
  const uncaptured = topics.filter((topic) => subscribeRefusesTopic(ownAgentSubject, topic));
  if (uncaptured.length === 0) return;
  throw new Error(uncaptured.map((topic) => uncapturedTopicRefusal(topic, sessionID)).join("; "));
}

/**
 * The stream subjects whose topics carry one agent session's private payload: each session's
 * inbox, the Legion daemon's notices for a project's controller, a failed delivery's report on a
 * session's subject (a session's own lane included), and a failed delivery on a team topic, which
 * carries the undelivered payload and the session it was for as the agent lane does. This is the
 * listener's own list, `privateStreamSubjects` in packages/envoy/internal/bus/stream.go.
 */
const PRIVATE_STREAM_SUBJECTS = [
  `${AGENT_TOPIC_PREFIX}>`,
  `${LEGION_NOTICE_TOPIC_PREFIX}>`,
  `${EXCEPTION_TOPIC_PREFIX}${AGENT_TOPIC_PREFIX}>`,
  `${EXCEPTION_TOPIC_PREFIX}notifications.team.>`,
];

/** What a controller notice lane ends with; every other subject of that family names an issue. */
const CONTROLLER_NOTICE_SUFFIX = ".controller";

/**
 * The Legion role token whose holder `topic`'s notices are for, or undefined when `topic` is no
 * controller notice lane: project `<p>`'s lane is `notifications.legion.<p>.controller` and its
 * role token is `legion-<p>-controller`, which is `controllerToken(p)` for every project token
 * the daemon mints. Both are read here from the one project token the lane names, as the
 * listener's `bus.ControllerNoticeRole` reads them, so only a literal lane names a role: a
 * wildcard token, an empty project, a longer subject, and the issue notices of the same family
 * (`notifications.legion.<p>.<issue key>`) name none.
 */
export function controllerNoticeRole(topic: string): string | undefined {
  if (!topic.startsWith(LEGION_NOTICE_TOPIC_PREFIX) || !topic.endsWith(CONTROLLER_NOTICE_SUFFIX)) {
    return undefined;
  }
  const project = topic.slice(
    LEGION_NOTICE_TOPIC_PREFIX.length,
    topic.length - CONTROLLER_NOTICE_SUFFIX.length
  );
  if (project === "" || /[.*>]/.test(project)) return undefined;
  return `legion-${project}-controller`;
}

/** Whether some subject matches both patterns, under NATS wildcard rules. */
export function subjectsOverlap(a: string, b: string): boolean {
  const aTokens = a.split(".");
  const bTokens = b.split(".");
  for (let index = 0; index < aTokens.length && index < bTokens.length; index += 1) {
    const aToken = aTokens[index];
    const bToken = bTokens[index];
    if (aToken === ">" || bToken === ">") return true;
    if (aToken !== "*" && bToken !== "*" && aToken !== bToken) return false;
  }
  return aTokens.length === bTokens.length;
}

/**
 * Whether a topic reaches a private stream subject that is not this session's: it overlaps one
 * under NATS wildcard rules, so another session's agent subject, a failed-delivery lane
 * (`notifications.envoy.exceptions.notifications.agent.<id>`, the session's own included, and the
 * team lane beside it), another project's controller lane, and every wildcard reaching any of
 * them (`notifications.agent.*`, `notifications.>`, `notifications.*.<x>`,
 * `notifications.envoy.>`) count. Two literal subjects are this session's own: `ownAgentSubject`,
 * in either form (the old `notifications.agent.<session>`, or the new one under a project,
 * `NO_PROJECT` or a wildcard over every project, which `agentSubjectSession` reads back), and the
 * controller notice lane of a project whose controller role `roles` names
 * (`controllerNoticeRole`), which its holder registers because a session receives what its own
 * consumer delivers and nothing else. This mirrors the listener's rule,
 * `ReachesPrivateStreamSubject` in packages/envoy/internal/bus/stream.go: the listener stores no
 * such interest, so a host registers none.
 */
export function reachesPrivateStreamSubject(
  ownAgentSubject: string,
  topic: string,
  roles: readonly string[] = []
): boolean {
  if (!PRIVATE_STREAM_SUBJECTS.some((subject) => subjectsOverlap(topic, subject))) return false;
  const role = controllerNoticeRole(topic);
  if (role !== undefined) return !roles.includes(role);
  // The session's own agent subject in any form, and only while it names one session: a session
  // id holding `*` or `>` makes a wildcard of its "own" agent subject, no session's alone.
  const session = agentSubjectSession(ownAgentSubject);
  return session === undefined || /[*>]/.test(session) || agentSubjectSession(topic) !== session;
}

/** Whether `topic` is anything but `ownAgentSubject` in another form, which a registration and a
 *  restore carry once, as `ownAgentSubject` itself: a stored row read back holds the old form the
 *  listener adds to every registration, and a state an earlier build persisted holds it too. */
function notOwnAgentSubject(ownAgentSubject: string, topic: string): boolean {
  const session = agentSubjectSession(ownAgentSubject);
  return session === undefined || agentSubjectSession(topic) !== session;
}

/**
 * The topics a registration carries, for every host that builds one: the session's own agent
 * subject, which its consumer is always filtered on, once and in the form `ownAgentSubject`
 * names, and every topic it follows that the listener will store for it. Two are left out, and
 * a host that followed one still follows it: it just never goes to the listener.
 *
 * A topic reaching a private stream subject that is not this session's
 * (`reachesPrivateStreamSubject`) is left out because the listener leaves it out of the stored
 * row itself, answering a warning.
 *
 * A topic nothing would ever be delivered on (`streamDeliversNothing`) is left out because the
 * listener stores none: it leaves such a topic out of the row and names it in a warning, so a
 * registration carrying one would draw that warning at every heartbeat, for as long as anything
 * still names it. A host cannot refuse it where a topic is first followed and be done: a topic
 * also comes back from the listener's own stored row and from the transcript the host restores
 * (INFRA-771 review), and a row written by an earlier build holds `notifications.team`, the bare
 * base that build expanded `notifications.team.>` to, which this build's stream captures
 * nothing of. Filtering here is the one place every registration passes through. A caller that
 * names such a topic itself is refused by name instead, at the host's own subscribe tool
 * (`uncapturedTopicRefusal`).
 */
export function registrationTopics(
  ownAgentSubject: string,
  followed: Iterable<string>,
  roles: readonly string[] = []
): string[] {
  return [
    ...new Set([
      ownAgentSubject,
      ...[...followed].filter(
        (topic) =>
          notOwnAgentSubject(ownAgentSubject, topic) &&
          !reachesPrivateStreamSubject(ownAgentSubject, topic, roles) &&
          !streamDeliversNothing(topic)
      ),
    ]),
  ];
}

/**
 * The subjects a restore follows again, which is what the ledger persists: the session's own
 * agent subject, once and in the form `ownAgentSubject` names, then every topic it follows but a
 * role's and one reaching a private stream subject. A role's topic — a role lane, or a subject
 * the host holds only while it holds a role (`heldForRole`) — is the role claim's to open, so a
 * process that restores this state without the role follows none of it; another session's agent
 * subject (a fork's parent, until the host
 * closes it), a failed-delivery lane, a controller notice lane, or a wildcard reaching one of
 * them, is not this session's to follow, and the listener would refuse it
 * (`reachesPrivateStreamSubject`).
 */
export function replayFollowedSubjects(
  ownAgentSubject: string,
  topics: Iterable<string>,
  heldForRole: (topic: string) => boolean = () => false
): string[] {
  const followed = [...topics].filter(
    (topic) =>
      !topic.startsWith(ROLE_TOPIC_PREFIX) &&
      notOwnAgentSubject(ownAgentSubject, topic) &&
      !reachesPrivateStreamSubject(ownAgentSubject, topic) &&
      !heldForRole(topic)
  );
  return [...new Set([ownAgentSubject, ...followed])];
}

/**
 * The topics a persisted state says the session followed that a restore follows again, so a
 * session back after the listener reaped its interests still follows them and takes what its
 * consumer kept for them: every subject of the state but agent subjects (the host opens its own,
 * and another's is not its to follow) and role lanes (held through a role's claim). An unfollowed
 * topic is absent from the newest state, so it stays unfollowed.
 */
export function rememberedTopics(topics: readonly string[] | undefined): string[] {
  return (topics ?? []).filter(
    (topic) => !topic.startsWith(AGENT_TOPIC_PREFIX) && !topic.startsWith(ROLE_TOPIC_PREFIX)
  );
}
