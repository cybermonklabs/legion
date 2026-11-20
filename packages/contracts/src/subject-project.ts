import { PROJECT_KEY_PATTERN } from "./dispatch-tools";
import { DISPATCH_KEY_PATTERN, isLegionProjectToken } from "./legion-roles";
import {
  AGENT_TOPIC_PREFIX,
  DISPATCH_ISSUE_TOPIC_PREFIX,
  NO_PROJECT,
  ROLE_TOPIC_PREFIX,
} from "./subject";

/**
 * The readers of a message name's project, and of the two forms of the names the rename moves:
 * an issue's events, an agent's messages and a role's lane each gained the project as a token of
 * their own (`dispatchIssueSubject`, `agentSubject`, `roleSubject`). Every reader takes both
 * forms until the old one is refused. The Go readers are the hand-written twins of these
 * (packages/envoy/internal/contracts/subject_project.go), and fixtures/subject-project.json is one
 * table both run.
 */

const NOTIFICATIONS = "notifications.";
const EXCEPTION_PREFIX = "notifications.envoy.exceptions.";
const KEPT_PREFIX = "notifications.envoy.kept.";

function isLiteral(token: string | undefined): token is string {
  return token !== undefined && token !== "" && token !== "*" && token !== ">";
}

/** A token in a new-form name's project position read as a project: a project key is itself, and
 * `NO_PROJECT`, a wildcard and anything else name no project. */
function projectOfToken(token: string | undefined): string {
  return token !== undefined && PROJECT_KEY_PATTERN.test(token) ? token : NO_PROJECT;
}

/** The project of an issue key (`INFRA` of `INFRA-12`), or undefined for a token that is no issue
 * key: `DISPATCH_KEY_PATTERN` with a project part that is a project key. */
export function issueProject(key: string): string | undefined {
  const project = DISPATCH_KEY_PATTERN.exec(key)?.[1];
  return project !== undefined && PROJECT_KEY_PATTERN.test(project) ? project : undefined;
}

// The tail is `[\s\S]`, any character a line terminator among them, as the Go twin's
// non-empty rest is (subject_project.go RoleProject): `.` matches no line terminator.
const LEGION_ROLE_TOKEN = /^legion-([a-z0-9]+)-[\s\S]/;

/** The project of a Legion deployment's token: its upper case when the token is one (lower-case
 * letters and digits, as `claim.ProjectToken` writes it) and that is a project key. The token is
 * checked before it is upper-cased, so a name whose upper case only a Unicode table makes a key
 * (`ßk`) reads as it does in the Go twin. */
function legionTokenProject(token: string | undefined): string {
  return token !== undefined && isLegionProjectToken(token)
    ? projectOfToken(token.toUpperCase())
    : NO_PROJECT;
}

/**
 * The project a role works for, read from the role's own name: a Legion role,
 * `legion-<token>-…` (`roleToken`, `controllerToken`), works for the project whose key is its
 * token in upper case, and every other role, a lead role among them, for none (`NO_PROJECT`).
 */
export function roleProject(role: string): string {
  return legionTokenProject(LEGION_ROLE_TOKEN.exec(role)?.[1]);
}

/** A role-lane name's project, read from the tokens after the lane's prefix: the new form's
 * first of two, the old form's role. */
function roleLaneProject(rest: readonly string[]): string {
  return rest.length >= 2 ? projectOfToken(rest[0]) : roleProject(rest[0] ?? "");
}

export interface SubjectProject {
  /** The project, `NO_PROJECT` for a name of none, and "" for a GitHub name, which names a
   * repository and no project. */
  readonly project: string;
  /** A GitHub name's repository, `owner/name` as the name writes both (sanitized), else "". */
  readonly repo: string;
  /** False only for a name that is no notification topic at all. */
  readonly ok: boolean;
}

interface SubjectProjectRow {
  /** The family the row reads, as the pattern that names all of it. */
  readonly family: string;
  readonly read: (rest: readonly string[]) => { project: string; repo?: string };
}

const none = () => ({ project: NO_PROJECT });

/**
 * One row for every family the notification stream carries and for the role lanes, which it does
 * not. A family with no row reads as no project, which is never a guess at a project: the bus
 * census (packages/envoy/internal/bus/subject_project_coverage_test.go) holds the Go twin of this
 * table to the stream's own subject list, so a family the stream gains has a row before any
 * reader meets it.
 */
const SUBJECT_PROJECT_ROWS: readonly SubjectProjectRow[] = [
  // Dispatch's issue events: the new form names the project first, and the old form's project is
  // its key's.
  {
    family: "notifications.dispatch.issue.>",
    read: (rest) => ({ project: issueProject(rest[0] ?? "") ?? projectOfToken(rest[0]) }),
  },
  // Dispatch's document and project events, each named under its project already.
  {
    family: "notifications.dispatch.document.>",
    read: (rest) => ({ project: projectOfToken(rest[0]) }),
  },
  {
    family: "notifications.dispatch.project.>",
    read: (rest) => ({ project: projectOfToken(rest[0]) }),
  },
  // Dispatch's own start (startup.Subject), which is no project's.
  { family: "notifications.dispatch.server.>", read: none },
  // One session's messages: the new form names a project, the old form none.
  {
    family: "notifications.agent.>",
    read: (rest) => ({ project: rest.length >= 2 ? projectOfToken(rest[0]) : NO_PROJECT }),
  },
  { family: "notifications.role.>", read: (rest) => ({ project: roleLaneProject(rest) }) },
  // The Legion daemon's lanes: `<token>.controller`, `<token>.<issue>` and `<token>.wake` are the
  // deployment's, and the one `wake` every deployment shares is none's.
  {
    family: "notifications.legion.>",
    read: (rest) => ({ project: rest.length >= 2 ? legionTokenProject(rest[0]) : NO_PROJECT }),
  },
  // A repository's events: the name holds no project, so the reader gets the repository alone.
  {
    family: "notifications.github.>",
    read: (rest) => ({
      project: "",
      repo: rest.length >= 2 ? `${rest[0]}/${rest[1]}` : "",
    }),
  },
  { family: "notifications.slack.>", read: none },
  { family: "notifications.ghostwispr.>", read: none },
  { family: "notifications.whatsapp.>", read: none },
  { family: "notifications.team.>", read: none },
  // A failed delivery's report is the project's of the name it reports on.
  {
    family: "notifications.envoy.exceptions.>",
    read: (rest) => {
      const wrapped = subjectProject(rest.join("."));
      return wrapped.ok
        ? { project: wrapped.project, repo: wrapped.repo }
        : { project: NO_PROJECT };
    },
  },
  // The listener's kept role messages and their drain cursors, each a role lane's.
  {
    family: "notifications.envoy.kept.>",
    read: (rest) =>
      rest[0] === "role" || rest[0] === "cursor"
        ? { project: roleLaneProject(rest.slice(1)) }
        : { project: NO_PROJECT },
  },
  // Where a deleted session consumer stopped, which names a session and no project.
  { family: "notifications.envoy.resume.>", read: none },
  // A role a session took from another: the role's own project, as its kept lane reads it.
  {
    family: "notifications.envoy.takeover.>",
    read: (rest) => ({ project: roleProject(rest[0] ?? "") }),
  },
];

/** The families `subjectProject` has a row for, each as the pattern naming all of it. */
export function subjectProjectFamilies(): string[] {
  return SUBJECT_PROJECT_ROWS.map((row) => row.family);
}

/**
 * The project a message name belongs to. Every family the stream carries, and the role lanes,
 * have a row (`subjectProjectFamilies`); a family with none, and a wildcard where a project
 * would be, answer `NO_PROJECT`. `ok` is false only for a name that does not begin
 * `notifications.`, which no delivery carries.
 */
export function subjectProject(subject: string): SubjectProject {
  if (!subject.startsWith(NOTIFICATIONS)) return { project: "", repo: "", ok: false };
  for (const row of SUBJECT_PROJECT_ROWS) {
    const prefix = row.family.slice(0, -1);
    if (subject.length > prefix.length && subject.startsWith(prefix)) {
      const read = row.read(subject.slice(prefix.length).split("."));
      return { project: read.project, repo: read.repo ?? "", ok: true };
    }
  }
  return { project: NO_PROJECT, repo: "", ok: true };
}

/** A kept lane's role or cursor name: its kind and the tokens after it. */
function keptLane(topic: string): { kind: string; rest: string[] } | undefined {
  if (!topic.startsWith(KEPT_PREFIX)) return undefined;
  const [kind, ...rest] = topic.slice(KEPT_PREFIX.length).split(".");
  return kind === "role" || kind === "cursor" ? { kind, rest } : undefined;
}

/**
 * The new form of an old-form topic, and whether `topic` was in the old form: an issue's topic
 * naming its key alone (`notifications.dispatch.issue.INFRA-12.>`) gains its key's project, a
 * session's own agent subject (`notifications.agent.<session>`) gains a wildcard project, and a
 * role lane gains its role's project (`roleProject`); a failed-delivery report of one, and a kept
 * role lane, follow the name they carry. Every other topic, a wildcard where the old form's key,
 * session or role would be among them, is its own new form.
 */
export function canonicalTopic(topic: string): { canonical: string; legacy: boolean } {
  const unchanged = { canonical: topic, legacy: false };
  if (topic.startsWith(DISPATCH_ISSUE_TOPIC_PREFIX)) {
    const rest = topic.slice(DISPATCH_ISSUE_TOPIC_PREFIX.length);
    const project = issueProject(rest.split(".")[0] ?? "");
    return project === undefined
      ? unchanged
      : { canonical: `${DISPATCH_ISSUE_TOPIC_PREFIX}${project}.${rest}`, legacy: true };
  }
  if (topic.startsWith(AGENT_TOPIC_PREFIX)) {
    const rest = topic.slice(AGENT_TOPIC_PREFIX.length).split(".");
    return rest.length === 1 && isLiteral(rest[0])
      ? { canonical: `${AGENT_TOPIC_PREFIX}*.${rest[0]}`, legacy: true }
      : unchanged;
  }
  if (topic.startsWith(ROLE_TOPIC_PREFIX)) {
    const rest = topic.slice(ROLE_TOPIC_PREFIX.length).split(".");
    return rest.length === 1 && isLiteral(rest[0])
      ? { canonical: `${ROLE_TOPIC_PREFIX}${roleProject(rest[0])}.${rest[0]}`, legacy: true }
      : unchanged;
  }
  if (topic.startsWith(EXCEPTION_PREFIX)) {
    const wrapped = canonicalTopic(topic.slice(EXCEPTION_PREFIX.length));
    return wrapped.legacy
      ? { canonical: `${EXCEPTION_PREFIX}${wrapped.canonical}`, legacy: true }
      : unchanged;
  }
  const kept = keptLane(topic);
  if (kept !== undefined && kept.rest.length === 1 && isLiteral(kept.rest[0])) {
    const role = kept.rest[0];
    return { canonical: `${KEPT_PREFIX}${kept.kind}.${roleProject(role)}.${role}`, legacy: true };
  }
  return unchanged;
}

/** Whether `token` may stand in a new-form name's project position for a twin: a project key,
 * `NO_PROJECT`, or a wildcard over every project. */
function isProjectPosition(token: string | undefined): boolean {
  return (
    token !== undefined &&
    (PROJECT_KEY_PATTERN.test(token) || token === NO_PROJECT || token === "*")
  );
}

/**
 * The old form of a new-form topic, while readers take the old form too, and undefined for a topic
 * with none: an issue's topic under its key's own project names its key alone; one session's agent
 * subject and one role's lane under any project name the session and the role alone; a
 * failed-delivery report and a kept lane follow the name they carry. A topic over every issue of a
 * project (`notifications.dispatch.issue.INFRA.>`) has no old form, since no subject pattern names
 * the issues of one project in the old form. Nor has an issue's topic under a wildcard project
 * (`notifications.dispatch.issue.*.INFRA-12.>`): its old form would overlap it, and a session
 * consumer handed both would be reduced to a filter over every project's issues; the new form alone
 * already reaches the key's own project.
 */
export function legacyTwin(canonical: string): string | undefined {
  if (canonical.startsWith(DISPATCH_ISSUE_TOPIC_PREFIX)) {
    const [project, key, ...rest] = canonical.slice(DISPATCH_ISSUE_TOPIC_PREFIX.length).split(".");
    if (key === undefined || project === undefined) return undefined;
    const keyProject = issueProject(key);
    if (keyProject === undefined || project !== keyProject) return undefined;
    return [`${DISPATCH_ISSUE_TOPIC_PREFIX}${key}`, ...rest].join(".");
  }
  for (const prefix of [AGENT_TOPIC_PREFIX, ROLE_TOPIC_PREFIX]) {
    if (canonical.startsWith(prefix)) {
      const rest = canonical.slice(prefix.length).split(".");
      return rest.length === 2 && isProjectPosition(rest[0]) && isLiteral(rest[1])
        ? `${prefix}${rest[1]}`
        : undefined;
    }
  }
  if (canonical.startsWith(EXCEPTION_PREFIX)) {
    const twin = legacyTwin(canonical.slice(EXCEPTION_PREFIX.length));
    return twin === undefined ? undefined : `${EXCEPTION_PREFIX}${twin}`;
  }
  const kept = keptLane(canonical);
  if (kept !== undefined && kept.rest.length === 2 && isProjectPosition(kept.rest[0])) {
    const role = kept.rest[1];
    return isLiteral(role) ? `${KEPT_PREFIX}${kept.kind}.${role}` : undefined;
  }
  return undefined;
}

/**
 * Every form a reader takes `topic` in while the old form is read: `topic`, then its new form
 * when it is in the old one (`canonicalTopic`), then its old form when it is in the new one
 * (`legacyTwin`). A topic of a family the rename does not move is its one form.
 */
export function topicForms(topic: string): string[] {
  const { canonical, legacy } = canonicalTopic(topic);
  if (legacy) return [topic, canonical];
  const twin = legacyTwin(topic);
  return twin === undefined ? [topic] : [topic, twin];
}

/**
 * A claimed role read in either form: the new form names its project and then the role
 * (`INFRA.legion-infra-infra-12-architect`, `_.lead-reviewer`), and the old form, the role alone,
 * works for the project its name says (`roleProject`). A role's name holds no dot
 * (`roles.PatternString`), so a name that is not the new form is the old one, and is left to the
 * role check to refuse.
 */
export function canonicalRole(role: string): { project: string; name: string; legacy: boolean } {
  const dot = role.indexOf(".");
  if (dot >= 0) {
    const project = role.slice(0, dot);
    const name = role.slice(dot + 1);
    if (
      (PROJECT_KEY_PATTERN.test(project) || project === NO_PROJECT) &&
      name !== "" &&
      !name.includes(".")
    ) {
      return { project, name, legacy: false };
    }
  }
  return { project: roleProject(role), name: role, legacy: true };
}

/** The shape of an issue key in an issue event's name as the Legion daemon has always read it
 * (`claim.IsIssueKey`): no length is held to its project part, since a daemon's own project is
 * held to none. Dispatch's keys are held to `PROJECT_KEY_PATTERN` by its database, so every name
 * Dispatch writes is read here too. */
const ISSUE_EVENT_KEY = /^([A-Z][A-Z0-9]*)-[0-9]+$/;

/**
 * The issue key an issue event's name carries, in either form: the new form names the key's own
 * project and then the key (`notifications.dispatch.issue.INFRA.INFRA-12.<type>`), and the old form
 * the key alone (`notifications.dispatch.issue.INFRA-12.<type>`), with `legacy` true. Undefined for
 * a name of the family whose key is in neither form, a project that is not the key's own among
 * them, and for a name of any other family. A key is read by `ISSUE_EVENT_KEY`, the shape every
 * daemon has accepted. The old form is read until it is refused only.
 */
export function issueEventKey(subject: string): { key: string; legacy: boolean } | undefined {
  if (!subject.startsWith(DISPATCH_ISSUE_TOPIC_PREFIX)) return undefined;
  const [first = "", second = ""] = subject.slice(DISPATCH_ISSUE_TOPIC_PREFIX.length).split(".");
  if (ISSUE_EVENT_KEY.test(first)) return { key: first, legacy: true };
  if (ISSUE_EVENT_KEY.exec(second)?.[1] === first) return { key: second, legacy: false };
  return undefined;
}

/**
 * The key of a role claim in the `envoy_roles` bucket: the bare role name when `project` is
 * `roleProject(role)`, which every claim made before a producer addresses a role under another
 * project is, and `<project>.<role>` only when it is not, so no claim that exists ever changes key.
 * `project` is a project key or `NO_PROJECT`. A role's name holds no dot, so the two kinds of key
 * never collide.
 */
export function roleClaimKey(project: string, role: string): string {
  return project === roleProject(role) ? role : `${project}.${role}`;
}

/**
 * The project a claim of `role` works for, given the project the claim requested (`undefined` or
 * `""` for none), or an `Error` for a request the rule refuses, once a claim keys on its project.
 * Nothing calls it in this slice: while every claim keys on its bare role name, the listener's
 * `POST /v1/roles/set` refuses any new-form role whose project is not `roleProject(role)`, so for a
 * free-form role under a project (`PRIV.lead-reviewer`) the route answers 400 where this answers
 * `PRIV`. Plan C's t-c-8 wires it into the route, and t-c-6 reads Dispatch's claims through it.
 *
 * A Legion role token (`legion-<token>-…`) works for the project its own name gives
 * (`roleProject`), whatever the claim asks for, and a request naming another project is refused
 * rather than quietly ignored: a caller that asked for one project and got another would believe
 * a claim it does not hold. Only a free-form role name takes a requested project, which must be a
 * project key or `NO_PROJECT`. A claim that requests none works for `roleProject`.
 */
export function claimProject(role: string, requested?: string): string | Error {
  const own = roleProject(role);
  if (requested === undefined || requested === "") return own;
  if (own !== NO_PROJECT) {
    return requested === own
      ? own
      : new Error(
          `role ${role} works for project ${own}, and a claim of it cannot ask for project ${requested}`
        );
  }
  if (!PROJECT_KEY_PATTERN.test(requested) && requested !== NO_PROJECT) {
    return new Error(
      `a claim of role ${role} asks for project ${requested}, which is no project key`
    );
  }
  return requested;
}

/**
 * A role claim's key read back to the project and role it was written from (`roleClaimKey`): a key
 * naming a project and a role is that pair, and a bare key is the role, working for the project
 * its name says (`roleProject`). Unlike `canonicalRole`, which goes with the old form of a claimed
 * role, it stays: a bare key is how every claim of a role's own project is keyed for good.
 */
export function parseRoleClaimKey(key: string): { project: string; role: string } {
  const dot = key.indexOf(".");
  if (dot >= 0) {
    const project = key.slice(0, dot);
    const role = key.slice(dot + 1);
    if (
      (PROJECT_KEY_PATTERN.test(project) || project === NO_PROJECT) &&
      role !== "" &&
      !role.includes(".")
    ) {
      return { project, role };
    }
  }
  return { project: roleProject(key), role: key };
}
