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

/** A topic a role's holder may follow: one the notification stream stores, and no subject private
 *  to a session (an agent subject, a failed-delivery lane, a controller notice lane), which the
 *  listener stores for nobody. Each token is a NATS token: none empty, no white space, and a
 *  wildcard only as a whole token, `>` only as the last one, which is what the listener's consumer
 *  filter takes (`bus.CheckFilterSubject`). */
function topicRefusal(topic: string): string | undefined {
  const tokens = topic.split(".");
  const wildcardMisplaced = tokens.some(
    (token, index) =>
      (/[*>]/.test(token) && token.length > 1) || (token === ">" && index < tokens.length - 1)
  );
  if (tokens.some((token) => token === "" || /\s/.test(token)) || wildcardMisplaced) {
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
 * What the session follows of the topics the deployment names for the role it holds. Each time a
 * hold of a role lands, `open` follows that role's topics; only the subjects it newly followed
 * are the role's, so a topic the session followed itself before stays its own, and the role's end
 * (`close`) answers only the role's for the caller to close and unregister. A subject an edge
 * listener refused (`refuse`) leaves the role's and is not followed again for it until it ends.
 */
export interface RoleTopicFollows {
  readonly open: (role: string) => void;
  readonly close: () => readonly string[];
  readonly refuse: (subjects: readonly string[]) => void;
  /** Whether `subject` is one this module followed for the held role. */
  readonly has: (subject: string) => boolean;
  /** Every subject this module followed for the held role, in the order it followed them. */
  readonly subjects: () => readonly string[];
  /** Whether a subject followed for the role is one no registration has carried since. */
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
  const followedForRole = new Set<string>();
  const refused = new Set<string>();
  let owed = false;
  return {
    open: (role) => {
      for (const topic of deps.roleTopics.topicsFor(role)) {
        if (expandSubscriptionTopics([topic]).some((subject) => refused.has(subject))) continue;
        for (const subject of deps.follow(topic)) {
          followedForRole.add(subject);
          owed = true;
        }
      }
    },
    close: () => {
      const closed = [...followedForRole];
      followedForRole.clear();
      refused.clear();
      owed = false;
      return closed;
    },
    refuse: (subjects) => {
      for (const subject of subjects) {
        if (followedForRole.delete(subject)) refused.add(subject);
      }
    },
    has: (subject) => followedForRole.has(subject),
    subjects: () => [...followedForRole],
    owed: () => owed,
    registered: (topics) => {
      if ([...followedForRole].every((subject) => topics.includes(subject))) owed = false;
    },
  };
}
