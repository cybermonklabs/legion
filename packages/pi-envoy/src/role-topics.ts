import { reachesPrivateStreamSubject, streamStores } from "@legion/envoy-client/stream-topics";
import { expandSubscriptionTopics } from "@legion/envoy-client/transport";
import { listenerRoleName } from "./listener-role-name";

/**
 * The variable a deployment names, for each Envoy role, the topics that role's holder follows
 * while it holds the role: a JSON object from role name to a list of topics, such as
 * `{"ops-owner": ["notifications.slack.*.C0ALERTS.message"]}`. A pod's launch sets it, as it sets
 * `ENVOY_SESSION_ROLE`, so a session that takes the role after a relaunch follows them again.
 */
export const ROLE_TOPICS_VAR = "ENVOY_ROLE_TOPICS";

/** The topics `ENVOY_ROLE_TOPICS` names for each role, or why the variable was refused. */
export interface RoleTopics {
  /** The topics the holder of `role` follows: none for a role the variable does not name. */
  readonly topicsFor: (role: string) => readonly string[];
  /** Whether `subject` is one the topics of any role expand to. */
  readonly names: (subject: string) => boolean;
  /** Why the variable was refused, naming it; undefined when it was taken or is unset. */
  readonly error?: string;
}

const NONE: readonly string[] = [];
const NOTHING: RoleTopics = { topicsFor: () => NONE, names: () => false };

/** The longest subject the listener takes (`internal/bus/stored.go`, `checkSubject`), in bytes. */
const MAX_SUBJECT_BYTES = 4_032;

/** A topic a role's holder may follow: one the notification stream stores, and no subject private
 *  to a session (an agent subject, a failed-delivery lane, a controller notice lane), which the
 *  listener stores for nobody. Each token must be a NATS token: none empty, no white space, and
 *  the whole subject no longer than `MAX_SUBJECT_BYTES`, which is what the listener's consumer
 *  filter refuses too (`bus.CheckFilterSubject`). This rule is stricter than that filter on
 *  wildcards: it takes `*` and `>` only as whole tokens and `>` only as the last one, where the
 *  filter reads `*x` or `a>` as a literal token. A deployment that writes either almost surely
 *  meant a wildcard, so the variable is refused rather than left to follow a subject that never
 *  carries a post. */
function topicRefusal(topic: string): string | undefined {
  const tokens = topic.split(".");
  const wildcardMisplaced = tokens.some(
    (token, index) =>
      (/[*>]/.test(token) && token.length > 1) || (token === ">" && index < tokens.length - 1)
  );
  if (
    tokens.some((token) => token === "" || /\s/.test(token)) ||
    wildcardMisplaced ||
    new TextEncoder().encode(topic).length > MAX_SUBJECT_BYTES
  ) {
    return `${JSON.stringify(topic)} is not a NATS subject`;
  }
  if (!streamStores(topic)) {
    return `the notification stream stores nothing on ${JSON.stringify(topic)}, so it would deliver nothing`;
  }
  if (reachesPrivateStreamSubject("", topic)) {
    return `${JSON.stringify(topic)} reaches a subject private to one session, which the listener stores for nobody else`;
  }
  return undefined;
}

/**
 * The keys of the JSON object `text` holds, as written, in order, duplicates kept: `JSON.parse`
 * keeps only the last of two keys of the same spelling, so a duplicate is read here. `text` is
 * already known to parse as an object, so a string token at depth one followed by `:` is a key.
 */
function topLevelKeys(text: string): string[] {
  const keys: string[] = [];
  let depth = 0;
  for (let index = 0; index < text.length; index += 1) {
    const char = text[index];
    if (char === "{" || char === "[") depth += 1;
    else if (char === "}" || char === "]") depth -= 1;
    else if (char === '"') {
      let end = index + 1;
      while (text[end] !== '"') end += text[end] === "\\" ? 2 : 1;
      if (depth === 1 && /^\s*:/.test(text.slice(end + 1))) {
        keys.push(JSON.parse(text.slice(index, end + 1)) as string);
      }
      index = end;
    }
  }
  return keys;
}

/**
 * Reads `ENVOY_ROLE_TOPICS` (`ROLE_TOPICS_VAR`). Unset or blank, no role follows a topic of it.
 * A role name is read as the listener reads a claim's (`listenerRoleName`), so a padded name is
 * the same role. A value that is not a JSON object of role names to lists of topics, a role name
 * that is empty, two keys the listener reads as one role (the same key twice included), and a
 * topic `topicRefusal` refuses, refuse the whole variable: no role follows any topic of it, and
 * `error` says why, so a deployment never runs on part of what it wrote.
 */
export function readRoleTopics(value: string | undefined): RoleTopics {
  if (value === undefined || value.trim() === "") return NOTHING;
  const refused = (why: string): RoleTopics => ({
    ...NOTHING,
    error: `${ROLE_TOPICS_VAR} is refused, so no role follows a topic it names: ${why}`,
  });
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    return refused("it is not JSON");
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    return refused("it is not a JSON object of role names to lists of topics");
  }
  const seen = new Set<string>();
  for (const named of topLevelKeys(value)) {
    const role = listenerRoleName(named);
    if (role === "") return refused(`${JSON.stringify(named)} names no role`);
    if (seen.has(role)) return refused(`role ${role} is named twice`);
    seen.add(role);
  }
  const byRole = new Map<string, readonly string[]>();
  for (const [named, topics] of Object.entries(parsed)) {
    const role = listenerRoleName(named);
    if (!Array.isArray(topics) || !topics.every((topic) => typeof topic === "string")) {
      return refused(`role ${role} names no list of topics`);
    }
    for (const topic of topics as readonly string[]) {
      const why = topicRefusal(topic);
      if (why !== undefined) return refused(`role ${role}: ${why}`);
    }
    byRole.set(role, [...new Set(topics as readonly string[])]);
  }
  const named = new Set(expandSubscriptionTopics([...byRole.values()].flat()));
  return {
    topicsFor: (role) => byRole.get(listenerRoleName(role)) ?? NONE,
    names: (subject) => named.has(subject),
  };
}

/**
 * Who wants each subject the session follows for a role, of the topics the deployment names for
 * it. A subject can be wanted twice: by the role, and by the session itself (a topic it followed
 * with `envoy_subscribe`, or one its transcript remembers). It stays followed while either wants
 * it, so the role's end leaves the session's own, and the session's unsubscribe leaves the
 * role's while the role is held.
 *
 * - `open(role)` claims every subject the role's topics expand to, following each the session did
 *   not follow yet; one it followed already is its own as well as the role's.
 * - `close()` drops every claim and answers the subjects only the role wanted, for the caller to
 *   stop following, each exactly, and to take out of the listener's registry.
 * - `claimOwn` and `releaseOwn` move a subject between the two when the session subscribes to,
 *   or unsubscribes from, a subject the role wants.
 * - A subject an edge listener refused (`refuse`) leaves the role's claims and is not asked for
 *   again until the role ends.
 */
export interface RoleTopicFollows {
  readonly open: (role: string) => void;
  readonly close: () => readonly string[];
  readonly refuse: (subjects: readonly string[]) => void;
  /** The session wants `subject` itself too; answers whether only the role wanted it before. */
  readonly claimOwn: (subject: string) => boolean;
  /** The session no longer wants `subject` itself, though the role still does; answers whether it
   *  was the session's own as well, so what the ledger records changes. */
  readonly releaseOwn: (subject: string) => boolean;
  /** The role `open` last claimed subjects for, until `close`. */
  readonly role: () => string | undefined;
  /** Whether the role wants `subject`. */
  readonly has: (subject: string) => boolean;
  /** Whether only the role wants `subject`, so the session follows it for no reason of its own. */
  readonly roleOnly: (subject: string) => boolean;
  /** Every subject the role wants, in the order `open` claimed them. */
  readonly subjects: () => readonly string[];
  /** Whether a subject `open` newly followed is one no registration has carried since. */
  readonly owed: () => boolean;
  /** A registration carrying `topics` went out. */
  readonly registered: (topics: readonly string[]) => void;
}

export interface RoleTopicFollowsDeps {
  readonly roleTopics: RoleTopics;
  /** Follows `topic`, the live subscriptions staying with the extension, and answers each subject
   *  it newly followed. */
  readonly follow: (topic: string) => readonly string[];
}

export function createRoleTopicFollows(deps: RoleTopicFollowsDeps): RoleTopicFollows {
  // Every subject the role wants, and the part of them the session does not want itself.
  const wanted = new Set<string>();
  const onlyRole = new Set<string>();
  const refused = new Set<string>();
  let openedFor: string | undefined;
  let owed = false;
  return {
    open: (role) => {
      openedFor = role;
      for (const topic of deps.roleTopics.topicsFor(role)) {
        const subjects = expandSubscriptionTopics([topic]);
        if (subjects.some((subject) => refused.has(subject))) continue;
        const added = new Set(deps.follow(topic));
        for (const subject of subjects) {
          if (added.has(subject)) {
            onlyRole.add(subject);
            owed = true;
          }
          wanted.add(subject);
        }
      }
    },
    close: () => {
      const closed = [...onlyRole];
      wanted.clear();
      onlyRole.clear();
      refused.clear();
      openedFor = undefined;
      owed = false;
      return closed;
    },
    refuse: (subjects) => {
      for (const subject of subjects) {
        if (!wanted.delete(subject)) continue;
        onlyRole.delete(subject);
        refused.add(subject);
      }
    },
    claimOwn: (subject) => onlyRole.delete(subject),
    releaseOwn: (subject) => {
      if (!wanted.has(subject) || onlyRole.has(subject)) return false;
      onlyRole.add(subject);
      return true;
    },
    role: () => openedFor,
    has: (subject) => wanted.has(subject),
    roleOnly: (subject) => onlyRole.has(subject),
    subjects: () => [...wanted],
    owed: () => owed,
    registered: (topics) => {
      if ([...wanted].every((subject) => topics.includes(subject))) owed = false;
    },
  };
}
