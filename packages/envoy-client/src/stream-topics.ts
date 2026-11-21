import {
  AGENT_TOPIC_PREFIX,
  LEGION_NOTICE_TOPIC_PREFIX,
  legacyTwin,
  ROLE_TOPIC_PREFIX,
  uncapturedTopicWarning,
} from "@legion/contracts";
import type { Interest } from "./transport";

/** The topic a failed delivery's report goes to is this prefix followed by the original topic. */
export const EXCEPTION_TOPIC_PREFIX = "notifications.envoy.exceptions.";

/**
 * The stream subjects whose topics carry one agent session's private payload, or the listener's
 * own record: each session's inbox, the Legion daemon's notices for a project's controller, a
 * failed delivery's report on a session's subject (a session's own lane included), a failed
 * delivery on a team topic, which carries the undelivered payload and the session it was for as
 * the agent lane does, the kept lane (the role messages the listener could not deliver and the
 * drain cursors over them, `notifications.envoy.kept.`, which is `KeptTopicPrefix` in
 * packages/envoy/internal/bus/stream.go), the resume lane (where a deleted session consumer
 * stopped, `notifications.envoy.resume.`, which is `SessionResumeSubjectPrefix` in
 * packages/envoy/internal/bus/session_resume.go), and the takeover lane (where the listener records
 * that a session took a role from another, `notifications.envoy.takeover.`, which is
 * `TakeoverSubjectPrefix` in packages/envoy/internal/bus/takeover.go). The listener alone reads the
 * last three, so no session registers a topic under them, its own included. `@legion/contracts`
 * exports none of those prefixes, so each is written here as the Go constants spell it. This is the
 * listener's own list, `privateStreamSubjects` in packages/envoy/internal/bus/stream.go.
 */
const PRIVATE_STREAM_SUBJECTS = [
  `${AGENT_TOPIC_PREFIX}>`,
  `${LEGION_NOTICE_TOPIC_PREFIX}>`,
  `${EXCEPTION_TOPIC_PREFIX}${AGENT_TOPIC_PREFIX}>`,
  `${EXCEPTION_TOPIC_PREFIX}notifications.team.>`,
  "notifications.envoy.kept.>",
  "notifications.envoy.resume.>",
  "notifications.envoy.takeover.>",
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
 * team lane beside it), another project's controller lane, the kept lane
 * (`notifications.envoy.kept.role.<role>`, `notifications.envoy.kept.cursor.<role>`), the resume
 * lane (`notifications.envoy.resume.<id>`, the session's own included), the takeover lane
 * (`notifications.envoy.takeover.<role>.<previous holder>.<attempt>`), and every wildcard
 * reaching any of them (`notifications.agent.*`, `notifications.>`, `notifications.*.<x>`,
 * `notifications.envoy.>`) count. Two literal subjects are this session's own: `ownAgentSubject`,
 * in either form (its new form under a project, `NO_PROJECT` or a wildcard over every project,
 * whose old form `legacyTwin` reads back), and the controller notice lane of a project whose
 * controller role `roles` names (`controllerNoticeRole`), which its holder registers because a
 * session receives what its own consumer delivers and nothing else. This mirrors the listener's
 * rule, `ReachesPrivateStreamSubject` in packages/envoy/internal/bus/stream.go: the listener stores
 * no such interest, so a host registers none.
 */
export function reachesPrivateStreamSubject(
  ownAgentSubject: string,
  topic: string,
  roles: readonly string[] = []
): boolean {
  if (!PRIVATE_STREAM_SUBJECTS.some((subject) => subjectsOverlap(topic, subject))) return false;
  const role = controllerNoticeRole(topic);
  if (role !== undefined) return !roles.includes(role);
  // A session id holding `*` or `>` makes a wildcard of its "own" agent subject, no session's alone.
  const own = topic === ownAgentSubject || legacyTwin(topic) === ownAgentSubject;
  return !own || /[*>]/.test(ownAgentSubject);
}

/**
 * The topics a registration carries, for every host that builds one: the session's own agent
 * subject, which its consumer is always filtered on, and every topic it follows except one
 * reaching a private stream subject that is not this session's (`reachesPrivateStreamSubject`).
 * The listener leaves such a topic out of the stored row itself, answering a warning, so a host
 * that followed one still follows it: it just never goes to the listener.
 *
 * No topic is left out because the stream subjects this build was compiled with capture nothing
 * of it. The listener judges that on a fresh read of the deployed stream's subjects
 * (`uncapturedTopics` in packages/envoy/cmd/listener/api.go), which during a roll hold the
 * subjects a newer build added. A topic only those capture, left out here, would stay out of the
 * session's consumer filter, and the consumer would move past every message stored on it. The
 * listener keeps a topic no stream subject captures in the row (`uncapturedTopicWarning` in
 * `@legion/contracts`). No older listener this plugin works with
 * refuses a registration for such a topic. A listener built from #2603 until it kept them leaves
 * the topic out with a warning of its own and stores the rest of the registration. One built from
 * #2446 (the oldest this plugin needs) until #2603 stores the topic and filters the consumer only
 * on what its own compiled subjects capture.
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
        (topic) => !reachesPrivateStreamSubject(ownAgentSubject, topic, roles)
      ),
    ]),
  ];
}

/**
 * The listener's warnings a host's subscribe tool shows its caller, out of all those the listener
 * answered the registration with (`answer`): every warning but the uncaptured-topic warning for a
 * topic the caller did not name in this call. A registration carries topics the caller did not
 * name there: the base `X` that `expandSubscriptionTopics` adds for a named `X.>`, and every other
 * topic the session follows. Each stays registered, so that nothing stored on it is lost once a
 * stream subject captures it, and the listener still logs its warning. The caller is shown only
 * the warnings for what it named. A host registers on every subscribe call, also when the call
 * adds no topic, so a caller that names a followed topic again is shown its warning again.
 *
 * A warning names its topic first, and a NATS subject holds no space, so the text before the first
 * space is the topic. The warning is that topic's uncaptured-topic warning only when it is the
 * exact text `uncapturedTopicWarning` builds for that topic and the answer's `session_id`, which is
 * the session the listener builds it for. The listener's answer carries no structured field to
 * match on. Every other warning (a private stream subject, a GitHub topic's spelling or wiring) is
 * shown whatever it names. A listener built before `uncapturedTopicWarning` words its warning
 * otherwise, and that warning is shown too.
 */
export function warningsForNamedTopics(
  answer: Pick<Interest, "session_id" | "warnings">,
  named: readonly string[]
): string[] {
  return (answer.warnings ?? []).filter((warning) => {
    const space = warning.indexOf(" ");
    if (space <= 0) return true;
    const topic = warning.slice(0, space);
    return named.includes(topic) || warning !== uncapturedTopicWarning(topic, answer.session_id);
  });
}

/**
 * The subjects a restore follows again, which is what the ledger persists: the session's own
 * agent subject, then every topic it follows but a role's and one reaching a private stream
 * subject. A role's topic — a role lane, or a subject the host holds only while it holds a role
 * (`heldForRole`) — is the role claim's to open, so a process that restores this state without
 * the role follows none of it; a topic reaching a private stream subject
 * (`PRIVATE_STREAM_SUBJECTS`, `reachesPrivateStreamSubject`) is not this session's to follow, and
 * the listener leaves it out of the registration.
 */
export function replayFollowedSubjects(
  ownAgentSubject: string,
  topics: Iterable<string>,
  heldForRole: (topic: string) => boolean = () => false
): string[] {
  const followed = [...topics].filter(
    (topic) =>
      !topic.startsWith(ROLE_TOPIC_PREFIX) &&
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
 * topic is absent from the newest state, so it does not come back from here; the listener's entry
 * can still hold it, and whether a resume releases it or follows it from there is
 * `releasedOnResume`'s rule (`./delivery-ledger`).
 */
export function rememberedTopics(topics: readonly string[] | undefined): string[] {
  return (topics ?? []).filter(
    (topic) => !topic.startsWith(AGENT_TOPIC_PREFIX) && !topic.startsWith(ROLE_TOPIC_PREFIX)
  );
}
