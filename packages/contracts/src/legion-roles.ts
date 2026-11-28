import { ROLE_TOPIC_PREFIX } from "./subject";

/** A Legion issue key: a Dispatch key (`^[A-Z][A-Z0-9]*-[0-9]+$`, e.g. `LEGION-7`). Owner/repo
 * comes from `DaemonConfig`, never from this key — GitHub issues are never part of Legion's
 * lifecycle (see the T20 design). Not a template-literal union so it stays ergonomic as a plain
 * string through roles/state; every value at runtime matches `ISSUE_KEY_PATTERN`
 * (`legion-state.ts`). */
export type IssueKey = string;

export const LEGION_ROLES = [
  "architect",
  "planner",
  "implementer",
  "tester",
  "reviewer",
  "merger",
] as const;

export type LegionRole = (typeof LEGION_ROLES)[number];

/** A phase worker's claim role. This is a string; `isStepRole` checks the grammar at runtime. */
export type LegionStepRole = string;

/** The design-gate steps a Legion deployment can gate, each naming one document of a root issue
 * a person must approve before the tree's work runs: `decisions` names the issue's decisions
 * document, `criteria` its Verification document, `spec` its primary document.
 *
 * One tuple serves both wire contracts that carry these words — the daemon's own API
 * (`legion-api.ts`) and the list it publishes onto its Dispatch project row
 * (`dispatch-api.ts`) — so a step added here reaches both schemas and every message built from
 * them rather than two lists kept in step by hand. */
export const GATED_STEPS = ["decisions", "criteria", "spec"] as const;

/** The scale of a plan's rating, `planRating`: the whole numbers from 1 (the plan cost the work)
 * to 5 (the plan fit the work). This is its one definition. The plugin's `plan_rating`, the
 * `dispatch_plans` description and `dispatch plans`' printer read these, and `scripts/gen-go.ts`
 * generates them into Go for Dispatch's record of the retro (`envoy/internal/contracts`) and for the
 * daemon's completion route (`github.com/sjawhar/envoy/steprecord`). They are written in this
 * module, which the dispatch command's bundle and the dashboard already load, so both read them
 * without the daemon's API module; `legion-api.ts` re-exports them. */
export const LEGION_PLAN_RATING_LEAST = 1;
export const LEGION_PLAN_RATING_MOST = 5;

/** The most characters the reason of a plan's rating holds, counted as Unicode code points as the
 * daemon and Dispatch count them, generated into Go with the scale: Dispatch's record of the retro
 * takes one to 2,000. */
export const LEGION_PLAN_REASON_MAX = 2000;

export interface ParsedRoleToken {
  project: string;
  issue: IssueKey;
  role: LegionStepRole;
}

const ENVOY_ROLE_TOKEN = /^[a-z0-9][a-z0-9_-]*$/;
const PROJECT_TOKEN = /^[a-z0-9]+$/;
/** A Dispatch issue key: uppercase project, then a numeric suffix — `IssueKey`'s sole shape. */
export const DISPATCH_KEY_PATTERN = /^([A-Z][A-Z0-9]*)-([0-9]+)$/;
/** The issue part of a claim token, `<issue project>-<number>`, and the agent's name after it: the
 * role of the agent's first step, or a numbered name such as `implementer-2` for a second agent of
 * one role. Neither part of the issue holds a `-`, so the name is everything after the second one,
 * and may hold a `-` of its own (`code-checker`); the Go daemon's
 * `claim.Token.Cut` reads it by the same fields. */
const DISPATCH_ROLE_PART = /^([a-z0-9]+)-([0-9]+)-(.+)$/;
/** The role grammar of Dispatch's step record, which each of Legion's six matches; the Go daemon's
 * `claim.IsRole` reads the same. */
const STEP_ROLE = /^[a-z][a-z0-9-]{0,31}$/;

export function isLegionRole(role: string): role is LegionRole {
  return LEGION_ROLES.some((candidate) => candidate === role);
}

/** Whether a claim of an issue can be on `role`: each of Legion's six, and each role a deployment
 * defines, a name of `^[a-z][a-z0-9-]{0,31}$`. The controller is none: its claim holds no issue.
 * The one definition of a role of the step grammar; the Go daemon's `claim.IsRole` is its twin. */
export function isStepRole(role: string): boolean {
  return role !== "controller" && STEP_ROLE.test(role);
}

/**
 * The role that works a phase, the TypeScript mirror of the daemon's own table
 * (`packages/daemon/internal/phase/phase.go`, `RoleFor`). A phase no role works —
 * `awaiting_person`, `awaiting_children`, `done`, `held`, `unrecorded` — has none, and so has any
 * value that is not a phase; `admitted` and `accepting` are the architect's, and `retro` is the
 * implementer's turn on the issue it developed after the task's last step, however many steps it
 * worked. `step` has none either, since the role at work there is the issue's `stepRole`, the role
 * of its running step, which no phase names. The one reader here, the phase reminder, runs for a
 * phase worker alone, so the architect's two phases never remind anyone.
 */
export function roleForPhase(phase: string): LegionRole | undefined {
  switch (phase) {
    case "planning":
      return "planner";
    case "implementing":
    case "retro":
      return "implementer";
    case "testing":
      return "tester";
    case "reviewing":
      return "reviewer";
    case "admitted":
    case "accepting":
      return "architect";
    case "merging":
      return "merger";
    default:
      return undefined;
  }
}

export function isLegionProjectToken(project: string): boolean {
  return PROJECT_TOKEN.test(project);
}

/** The one rule that turns the operator-written `project` (`legion.yaml`'s value, `LEGION_ID`,
 * `cybermonklabs/legion`) into the Legion project token every role token, secret file, and
 * `LEGION_PROJECT` carries (`cybermonklabslegion`): lowercased, every non-alphanumeric dropped — the Go
 * daemon's `claim.ProjectToken` too. The TypeScript daemon, `legion controller start`'s
 * operator-side loader and the Go controller's plugin all apply it, so an operator's copied value
 * lands on the daemon's own controller token; a value that sanitizes to nothing is refused naming
 * `field`. */
export function legionProjectToken(value: string, field: string): string {
  const project = value.toLowerCase().replace(/[^a-z0-9]/g, "");
  if (!project) throw new Error(`${field} must include at least one alphanumeric character`);
  return project;
}

export function assertLegionProjectToken(project: string): void {
  if (!isLegionProjectToken(project)) {
    throw new Error(`Invalid Legion project token: ${project}`);
  }
}

/** The claim token of the agent named for `role` on `issue`, byte for byte the Go daemon's
 * `claim.NewToken`: the role's own claim, which a step of no key runs in where no other agent of
 * the plan holds the role's name. Any other agent's token ends in its own name, whatever role its
 * step is on now: the role of its first step, or a numbered name such as `implementer-2` for a
 * second agent of one role. A role outside the step grammar (`isStepRole`), the controller's among
 * them, is refused as Go refuses it. */
export function roleToken(project: string, issue: IssueKey, role: LegionStepRole): string {
  assertLegionProjectToken(project);

  const dispatchMatch = DISPATCH_KEY_PATTERN.exec(issue);
  if (!dispatchMatch) throw new Error(`Invalid IssueKey: ${issue}`);
  if (!isStepRole(role)) throw new Error(`Invalid Legion role: ${role}`);
  const [, dispatchProject, number] = dispatchMatch;
  return `legion-${project}-${dispatchProject?.toLowerCase()}-${number}-${role}`;
}

export function sanitizeToken(part: string): string {
  const token = part
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");

  return token || "x";
}

export function controllerToken(project: string): string {
  assertLegionProjectToken(project);
  return `legion-${project}-controller`;
}

export function roleTopic(token: string): string {
  return `${ROLE_TOPIC_PREFIX}${token}`;
}

/** A claim token of an issue ends `<issue project>-<number>-<agent>`; its project component is
 * already `[A-Z][A-Z0-9]*`, so lowercasing it needs no further escaping. The agent's name, which
 * the answer's `role` holds, is read by its fields (`DISPATCH_ROLE_PART`), and a name outside the
 * step grammar parses as no claim's. */
export function parseRoleToken(
  project: string,
  token: string
): ParsedRoleToken | { controller: true } | undefined {
  if (!isLegionProjectToken(project)) return undefined;

  const prefix = `legion-${project}-`;
  if (!ENVOY_ROLE_TOKEN.test(token) || !token.startsWith(prefix)) return undefined;

  const rest = token.slice(prefix.length);
  if (rest === "controller") return { controller: true };

  const dispatchMatch = DISPATCH_ROLE_PART.exec(rest);
  if (!dispatchMatch) return undefined;
  const [, dispatchProject, numberPart, role] = dispatchMatch;
  if (!dispatchProject || !numberPart || !role || !isStepRole(role)) return undefined;
  return {
    project,
    issue: `${dispatchProject.toUpperCase()}-${numberPart}`,
    role,
  };
}
