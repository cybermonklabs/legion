#!/usr/bin/env bun
// The one schema of a held-out measurement's locked case files and of the outcome record each run
// writes. Three files hold the cases, each locked before any arm runs: heldout-cases-locked.json and
// devcases-locked.json ({version, set: "heldout" | "devcase", cases: HeldoutCase[]}), and
// skilluse-cases-locked.json ({version, set: "skilluse", cases: SkillUseCase[]}). A run writes
// outcome.json (OutcomeRecord), and a case's known-good and known-bad traces are outcome records
// too, so a trace is scored exactly as a run is. Every parser is strict: a key the schema does not
// name, or a part it requires that is missing, is refused, and the error names the case and the
// part. A case's known-bad trace and planted omission parse when absent: the inventory
// (heldoutGaps) reports them, so a draft parses before it is complete.
//
//   heldout-case.ts gaps <heldout or devcase file> [<skilluse file>]
//       each file's gaps: its number of gaps alone on stdout (`<file>: <n> gaps`), each gap line on
//       stderr (`<file>: <gap>`); exits 1 when a file has a gap, 0 when none has. The roles are
//       HELDOUT_ROLES, the agents packages/pi-legion/agents/*.md, and the partition is the one
//       scripts/pi-plugin-prepack.sh gives @sjawhar/pi-legion, staged as its prepack stages it.
//   heldout-case.ts parse <file>
//       validates one case file, its `set` choosing the shape, and prints its set and case count.
// A file that does not parse exits 2, with its error on stderr.
import { readdirSync, readFileSync, rmSync } from "node:fs";
import path from "node:path";
import { DISPATCH_KEY_PATTERN } from "@legion/contracts";
import { LEGION_WORKFLOW_PHASES } from "@legion/contracts/legion-api";
import { shippedAgents, stageSkills } from "@legion/pi-shared/test/skills-guard";
import { z } from "zod";
import { parseInstant } from "./production-dispatch";

/** The version of the case files' shape this schema reads. */
export const SCHEMA_VERSION = 1;
/** The Legion plugin, whose agents and skill partition the inventory is counted against. */
const PACKAGE_ROOT = path.resolve(import.meta.dir, "../..");
const LEGION_PLUGIN = "@sjawhar/pi-legion";
/** The Envoy skill the skill-use population also runs in a plain Envoy session. */
export const DISPATCH_FIRST = "dispatch-first";

/** The ten roles a held-out case is written for: a Legion pane's prompt each, which is not the
 * daemon's claim role (the retro and the production-check look run under a worker's claim). */
export const HELDOUT_ROLES = [
  "headless-controller",
  "root-architect",
  "child-architect",
  "planner",
  "implementer",
  "tester",
  "reviewer",
  "merger",
  "retro",
  "production-check",
] as const;
export type HeldoutRole = (typeof HELDOUT_ROLES)[number];

/** A case's kind: `S` success, `B` refused or waiting, `E` engagement, `Q1`..`Q4` and `D1` the
 * population's five single cases (a D1 run stages a build Job, which its outcome's `staging`
 * actions record), and `A` a task agent's own run, which `agent` names. */
export const CASE_KINDS = ["S", "B", "E", "Q1", "Q2", "Q3", "Q4", "D1", "A"] as const;
/** The kinds the population holds once each, whatever the role. */
export const POPULATION_KINDS = ["Q1", "Q2", "Q3", "Q4", "D1"] as const;
/** The kinds that carry a known-bad trace and a planted omission. */
const OMISSION_KINDS: readonly string[] = ["S", "B"];
/** A skill-use case's kind: a task that needs the skill, or a near miss that does not. */
export const SKILL_USE_KINDS = ["need", "near-miss"] as const;
/** Where a skill-use case runs: a Legion pane with a role, or a plain Envoy session. */
export const SESSIONS = ["legion", "plain-envoy"] as const;

/** The task agents the Legion plugin ships (packages/pi-legion/agents/*.md), sorted. */
export function legionAgents(): string[] {
  return shippedAgents(PACKAGE_ROOT).sort();
}

/** The skills scripts/pi-plugin-prepack.sh ships in the Legion plugin, sorted: staged into a
 * temporary directory exactly as its prepack stages dist/skills, read, and removed. */
export function legionPartition(): string[] {
  const staged = stageSkills(LEGION_PLUGIN);
  try {
    return readdirSync(staged).sort();
  } finally {
    rmSync(path.dirname(staged), { recursive: true, force: true });
  }
}

const AGENTS = legionAgents();

const Name = z
  .string()
  .regex(/^[A-Za-z0-9][A-Za-z0-9._-]*$/, "a name of letters, digits, '.', '_' and '-'");
const Text = z.string().regex(/\S/, "a text that is not blank");
const Commit = z.string().regex(/^[0-9a-f]{40}$/, "a commit: 40 lower-case hexadecimal characters");
/** A git tree's hash: SHA-1's 40 hexadecimal characters or SHA-256's 64. */
const TreeHash = z
  .string()
  .regex(
    /^[0-9a-f]{40}(?:[0-9a-f]{24})?$/,
    "a tree hash: 40 or 64 lower-case hexadecimal characters"
  );
const Instant = z
  .string()
  .refine(
    (text) => parseInstant(text) !== undefined,
    "an RFC 3339 time, as 2030-01-08T12:00:00Z is"
  );
const IssueKey = z.string().regex(DISPATCH_KEY_PATTERN, "a Dispatch issue key, as ACME-12 is");
/** A refusal code as the daemon, Dispatch and the tools write one: HANDOFF_NO_PULL_REQUEST. */
const Code = z
  .string()
  .regex(/^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+$/, "a refusal code, as HANDOFF_NO_PULL_REQUEST is");
const Phase = z.enum(LEGION_WORKFLOW_PHASES);
/** Named JSON values: a request's arguments, a write's fields. */
const Fields = z.record(z.string(), z.json());
const Count = z.number().int().positive();
/** An agent or a skill a case names. It is not held to this checkout's agents/ or skills/: a
 * locked case outlives the checkout it was written at, and a candidate that deletes a skill still
 * runs that skill's need task, so the inventory, not the parser, counts what a population covers. */
const Agent = Name;
const Skill = Name;

/** Whether time `a` comes strictly before time `b`, or undefined where either is no RFC 3339
 * time: a refinement still runs over a field whose own format check failed, and that field's
 * Instant already names it. */
function earlier(a: string, b: string): boolean | undefined {
  const from = parseInstant(a);
  const to = parseInstant(b);
  return from === undefined || to === undefined ? undefined : from < to;
}

type Context = z.core.$RefinementCtx;
function refuse(context: Context, where: PropertyKey[], message: string): void {
  context.addIssue({ code: "custom", path: where, message });
}

/** Each repeat in `names`, refused at its place under `where`. */
function refuseRepeats(context: Context, names: string[], where: (index: number) => PropertyKey[]) {
  const seen = new Set<string>();
  names.forEach((name, index) => {
    if (seen.has(name)) refuse(context, where(index), `${name} is named twice`);
    seen.add(name);
  });
}

/** An open window of time: strictly after `after` and strictly before `before`. */
const WINDOW = { after: Instant, before: Instant };
function refuseEmptyWindow(
  context: Context,
  where: PropertyKey[],
  window: { after: string; before: string }
): void {
  if (earlier(window.after, window.before) === false)
    refuse(context, where, `after ${window.after} is not before ${window.before}`);
}
const Window = z
  .strictObject(WINDOW)
  .superRefine((window, context) => refuseEmptyWindow(context, [], window));

/** Where a case was written from: a https URL or a path in the repository, and the commit of the
 * repository its world was taken at. */
const Source = z.strictObject({
  ref: z
    .string()
    .regex(
      /^(?:https:\/\/\S+|(?![/.])(?!.*\.\.)[\w.@+/-]+)$/,
      "a https URL or a relative path in the repository"
    ),
  commit: Commit,
});

// The world: the starting state of each part a run reaches. A part the run has none of is null.

/** The handoff words of the file-backed phases (pi-shared's handoff-record.ts FILE_BACKED). */
const HandoffWord = z.enum(["plan", "implement", "test", "review"]);

/** The scratch Dispatch issue the role works, with everything recorded on it. */
const DispatchWorld = z.strictObject({
  issue: z.strictObject({
    key: IssueKey,
    title: Text,
    status: Name,
    spec: z.string(),
    parent: IssueKey.nullable(),
  }),
  /** The issue's documents besides its spec, by file name (`verification.md`). */
  documents: z.array(z.strictObject({ name: Text, body: z.string() })),
  /** Each handoff record, as `dispatch handoff-write` stores it under its writer's session. */
  handoffs: z.array(
    z.strictObject({
      id: Text,
      phase: HandoffWord,
      version: Count,
      author: Text,
      at: Instant,
      body: Fields,
    })
  ),
  comments: z.array(z.strictObject({ id: Text, author: Text, at: Instant, body: Text })),
  asks: z.array(
    z.strictObject({
      id: Text,
      author: Text,
      at: Instant,
      question: Text,
      options: z.array(Text),
      state: z.enum(["open", "answered", "dismissed"]),
      answer: Text.nullable(),
    })
  ),
  /** Each result recorded against a check of the Verification document (`dispatch result`). */
  results: z.array(
    z.strictObject({
      check: Name,
      role: Name,
      verdict: z.enum(["pass", "fail"]),
      commit: Commit,
      at: Instant,
    })
  ),
});

/** The git remote and the role's jj workspace. `commits` holds every commit either names;
 * `localOnly` is each commit only the workspace holds, never a remote branch's head. */
const GitWorld = z
  .strictObject({
    commits: z
      .array(z.strictObject({ id: Commit, parents: z.array(Commit), description: Text }))
      .min(1),
    /** Each branch the remote holds and the head pushed to it. */
    remote: z.strictObject({
      name: Name,
      branches: z.array(z.strictObject({ name: Text, head: Commit })),
    }),
    /** The commit the workspace stands on (jj's `@-`), and each commit no remote branch holds. */
    workspace: z.strictObject({ standingOn: Commit, localOnly: z.array(Commit) }),
  })
  .superRefine((git, context) => {
    const known = new Set(git.commits.map((commit) => commit.id));
    const heads = new Set(git.remote.branches.map((branch) => branch.head));
    const unknown = (id: string, where: PropertyKey[]) => {
      if (!known.has(id)) refuse(context, where, `${id} is no commit of git.commits`);
    };
    git.remote.branches.forEach((branch, index) => {
      unknown(branch.head, ["remote", "branches", index, "head"]);
    });
    unknown(git.workspace.standingOn, ["workspace", "standingOn"]);
    git.workspace.localOnly.forEach((id, index) => {
      unknown(id, ["workspace", "localOnly", index]);
      if (heads.has(id))
        refuse(context, ["workspace", "localOnly", index], `${id} is a remote branch's head`);
    });
  });

const ReviewThread = z.strictObject({
  id: Text,
  path: Text,
  line: Count.nullable(),
  resolved: z.boolean(),
  outdated: z.boolean(),
  comments: z.array(z.strictObject({ author: Text, at: Instant, body: Text })).min(1),
});

const PullRequest = z
  .strictObject({
    number: Count,
    state: z.enum(["open", "closed", "merged"]),
    draft: z.boolean(),
    head: z.strictObject({ ref: Text, sha: Commit }),
    base: Text,
    title: Text,
    body: z.string(),
    mergeCommit: Commit.nullable(),
    reviews: z.array(
      z.strictObject({
        id: Text,
        author: Text,
        state: z.enum(["APPROVED", "CHANGES_REQUESTED", "COMMENTED", "DISMISSED"]),
        commit: Commit,
        at: Instant,
        body: z.string(),
      })
    ),
    /** The review threads as GitHub's GraphQL pages them: each inner list is one page, in order. */
    reviewThreads: z.array(z.array(ReviewThread)),
    /** The check runs on each commit the pull request's head has stood at. */
    checks: z.array(
      z
        .strictObject({
          name: Text,
          commit: Commit,
          status: z.enum(["queued", "in_progress", "completed"]),
          conclusion: z
            .enum([
              "success",
              "failure",
              "neutral",
              "cancelled",
              "skipped",
              "timed_out",
              "action_required",
            ])
            .nullable(),
        })
        .superRefine((check, context) => {
          if ((check.status === "completed") !== (check.conclusion !== null))
            refuse(context, ["conclusion"], "a check run has a conclusion exactly once completed");
        })
    ),
  })
  .superRefine((pull, context) => {
    if ((pull.state === "merged") !== (pull.mergeCommit !== null))
      refuse(context, ["mergeCommit"], "a pull request has a merge commit exactly once merged");
  });

/** GitHub as the run's `gh` stand-in answers it. */
const GitHubWorld = z.strictObject({
  repository: z.string().regex(/^[\w.-]+\/[\w.-]+$/, "a repository as owner/name"),
  pullRequests: z.array(PullRequest),
  /** What the merge queue answers each request the role may make of it. */
  mergeQueue: z.array(
    z.strictObject({
      pullRequest: Count,
      request: Text,
      answer: z.enum(["queued", "merged", "refused", "removed"]),
      detail: z.string(),
    })
  ),
});

/** The daemon's record of the issue. `task` holds the text fields of the issue's task, by name.
 * `step` is the task step the phase is on, null where none runs (a done issue). */
const DaemonWorld = z.strictObject({
  phase: Phase,
  step: z.strictObject({ place: Count, name: Text }).nullable(),
  task: z.record(z.string(), z.string()),
  /** The issue's open production check: the look the role works, whether no look follows it,
   * when the check opened, the bound its last look starts at, and the merge it waits for. */
  productionCheck: z
    .strictObject({
      look: Count,
      last: z.boolean(),
      openedAt: Instant,
      boundAt: Instant,
      mergeCommit: Commit,
    })
    .nullable(),
});

/** What production runs: the command or address that reports its revision, and that revision. */
const ProductionWorld = z.strictObject({ probe: Text, revision: Commit });

/** A run's world: its clock and the starting state of each part. */
export const World = z
  .strictObject({
    clock: Instant,
    dispatch: DispatchWorld,
    git: GitWorld.nullable(),
    github: GitHubWorld.nullable(),
    daemon: DaemonWorld.nullable(),
    production: ProductionWorld.nullable(),
  })
  .superRefine((world, context) => {
    const check = world.daemon?.productionCheck;
    if (!check) return;
    if (earlier(world.clock, check.openedAt) === true)
      refuse(context, ["daemon", "productionCheck", "openedAt"], "the check opens after the clock");
    if (earlier(world.clock, check.boundAt) === false)
      refuse(context, ["daemon", "productionCheck", "boundAt"], "the bound is not after the clock");
  });
export type World = z.infer<typeof World>;

/** The parts of a world a refused call can be held to leave unchanged: each part of each world
 * part, and production whole. */
export const WORLD_PARTS: readonly string[] = [
  ...Object.keys(DispatchWorld.shape).map((key) => `dispatch.${key}`),
  ...Object.keys(GitWorld.shape).map((key) => `git.${key}`),
  ...Object.keys(GitHubWorld.shape).map((key) => `github.${key}`),
  ...Object.keys(DaemonWorld.shape).map((key) => `daemon.${key}`),
  "production",
];
const WorldPart = z
  .string()
  .refine((part) => WORLD_PARTS.includes(part), `a part of the world: ${WORLD_PARTS.join(", ")}`);

// The outcome record: every scored action of a run in one ordered list, and what the run read.

/** The fields of each kind of scored action beside `kind`, `seq` and `at`:
 * - daemon: a `legion` tool request, its op and arguments, and the daemon's answer (`code` null
 *   when it took the request);
 * - dispatch: a Dispatch write, its command or event type, the issue or reference it targets and
 *   its key fields;
 * - github: a GitHub call, either `method` and `path` (a `gh api` call) or `command` (a `gh`
 *   subcommand such as `gh pr create`), and its key fields;
 * - push: a ref the run pushed and the commit it pushed there;
 * - staging: a staged build Job, its snapshot's tree hash and its files. */
const DETAILS = {
  daemon: {
    op: Name,
    args: Fields,
    answer: z.strictObject({ code: Code.nullable(), body: z.json() }),
  },
  dispatch: { command: Name, target: Text, fields: Fields },
  github: {
    method: z.enum(["GET", "POST", "PATCH", "PUT", "DELETE"]).optional(),
    path: Text.optional(),
    command: Text.optional(),
    fields: Fields,
  },
  push: { ref: Text, commit: Commit },
  staging: { tree: TreeHash, files: z.array(Text).min(1) },
};
export const ACTION_KINDS = ["daemon", "dispatch", "github", "push", "staging"] as const;
const SCORED = { seq: z.number().int().nonnegative(), at: Instant };

const OutcomeAction = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("daemon"), ...SCORED, ...DETAILS.daemon }),
  z.strictObject({ kind: z.literal("dispatch"), ...SCORED, ...DETAILS.dispatch }),
  z
    .strictObject({ kind: z.literal("github"), ...SCORED, ...DETAILS.github })
    .superRefine((call, context) => {
      const api = call.method !== undefined && call.path !== undefined;
      const command = call.command !== undefined;
      if (api === command || (!api && (call.method !== undefined || call.path !== undefined)))
        refuse(
          context,
          [],
          "a github action names method and path (a gh api call) or command (a gh subcommand), one form alone"
        );
    }),
  z.strictObject({ kind: z.literal("push"), ...SCORED, ...DETAILS.push }),
  z.strictObject({ kind: z.literal("staging"), ...SCORED, ...DETAILS.staging }),
]);

/** One `skill://` read: the skill, the file the read named (`SKILL.md` for `skill://<name>`),
 * whether it resolved, the bytes it returned, and whether it came before the first scored action. */
const SkillRead = z.strictObject({
  skill: Name,
  path: Text,
  resolved: z.boolean(),
  bytes: z.number().int().nonnegative(),
  beforeFirstAction: z.boolean(),
});

/** What a run did, as outcome.json records it, and the shape of every trace. `id` is a fresh UUID
 * that names neither the case nor the arm; no field holds prompt text, an arm, a route or a token
 * count. `seq` rises strictly along `actions`. `agentReport` is an A case's returned report. */
export const OutcomeRecord = z
  .strictObject({
    id: z.uuid(),
    actions: z.array(OutcomeAction),
    agentReport: z.string().optional(),
    skillReads: z.array(SkillRead),
    dispatchFirstInjected: z.boolean(),
    lastText: z.string(),
  })
  .superRefine((outcome, context) => {
    outcome.actions.forEach((action, index) => {
      const before = outcome.actions[index - 1];
      if (before !== undefined && action.seq <= before.seq)
        refuse(
          context,
          ["actions", index, "seq"],
          `seq ${action.seq} does not rise above seq ${before.seq} of the action before it`
        );
    });
  });
export type OutcomeRecord = z.infer<typeof OutcomeRecord>;

// A held-out case.

/** One way to take an action: an outcome action of `kind` whose fields hold `match` (each named
 * field equal; an object holds a match's members, recursively) and whose time at each `between`
 * field (a dot path, as `args.wake_at`) falls strictly inside its window. */
const Route = z
  .strictObject({
    kind: z.enum(ACTION_KINDS),
    match: Fields,
    between: z.array(z.strictObject({ field: Text, ...WINDOW })).optional(),
  })
  .superRefine((route, context) => {
    const fields: readonly string[] = Object.keys(DETAILS[route.kind]);
    const unknown = (field: string, where: PropertyKey[]) => {
      if (!fields.includes(field))
        refuse(
          context,
          where,
          `a ${route.kind} action has no field ${field}; its fields are ${fields.join(", ")}`
        );
    };
    for (const field of Object.keys(route.match)) unknown(field, ["match", field]);
    route.between?.forEach((window, index) => {
      unknown(window.field.split(".")[0] ?? "", ["between", index, "field"]);
      refuseEmptyWindow(context, ["between", index], window);
    });
  });

/** What an action does to the daemon: nothing (`unchanged`); an answer, `ok` where it took the
 * request, and the phase the issue then stands in (`answered`); or a production check's next
 * look named, its `production_checks.wake_at` inside the window with the `check_wake` outbox row
 * due then (`check-wake`). */
const Effect = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("unchanged") }),
  z.strictObject({
    kind: z.literal("answered"),
    answer: z.union([z.literal("ok"), Code]),
    phaseAfter: Phase,
  }),
  z.strictObject({
    kind: z.literal("check-wake"),
    wakeAt: Window,
    outbox: z.literal("check_wake"),
  }),
]);

/** A correct next action: its routes, the first the usual one and each other an alternate of the
 * same effect, and the effect it drives. Only a daemon request drives a daemon answer. */
const CaseAction = z
  .strictObject({ name: Name, routes: z.array(Route).min(1), effect: Effect })
  .superRefine((action, context) => {
    if (action.effect.kind === "unchanged") return;
    action.routes.forEach((route, index) => {
      if (route.kind !== "daemon")
        refuse(
          context,
          ["routes", index, "kind"],
          `an ${action.effect.kind} effect is the daemon's answer, so each route is a daemon request`
        );
    });
  });

/** A refusal of `action` after which a corrected retry may still pass: its code, and the world
 * parts the refused call must leave as they were. */
const AllowedRefusal = z.strictObject({
  code: Code,
  action: Name,
  unchanged: z.array(WorldPart).min(1),
});

/** An action that fails the run when taken, even if later undone: at any time, or, with `until`,
 * while any of the named actions has not yet been taken (an out-of-order write, which a later
 * action does not cure). */
const Forbidden = z.strictObject({
  name: Name,
  routes: z.array(Route).min(1),
  until: z.array(Name),
  why: Text,
});

/** How a refusal or wait case must end: after the named action the role stops (no scored action
 * follows, and it ends its turn) or waits (no write until the event `detail` names). */
const WaitRequired = z.strictObject({
  after: Name,
  mode: z.enum(["stop", "wait"]),
  detail: Text,
});

/** The fact an S or B case's planted omission takes out, and the whole world without it. */
const PlantedOmission = z.strictObject({ fact: Text, world: World });

/** The rubric lines a production-check case is judged on, in this order. */
const CHECK_RUBRIC = ["diagnosis", "scheduling"];

/** One held-out or development case. `actions` lists every correct next action in the order the
 * role must take them. `rubric` is one or more named lines. `traces.bad` and `plantedOmission`
 * may be absent while a case is drafted; the inventory names each missing one. An A case is a task
 * agent's own run in a plain session: what it returns is its report, so it may name no action and
 * run under no daemon, and each of its traces carries the report it is judged on. */
export const HeldoutCase = z
  .strictObject({
    id: Name,
    role: z.enum(HELDOUT_ROLES),
    kind: z.enum(CASE_KINDS),
    agent: Agent.optional(),
    source: Source,
    world: World,
    task: Text,
    tools: z.array(Name).min(1),
    actions: z.array(CaseAction),
    allowedRefusals: z.array(AllowedRefusal),
    forbidden: z.array(Forbidden),
    waitRequired: WaitRequired.nullable(),
    rubric: z.array(z.strictObject({ name: Name, text: Text })).min(1),
    traces: z.strictObject({ good: OutcomeRecord, bad: OutcomeRecord.optional() }),
    plantedOmission: PlantedOmission.optional(),
  })
  .superRefine((each, context) => {
    if ((each.kind === "A") !== (each.agent !== undefined))
      refuse(context, ["agent"], "an A case names its agent, and no other case names one");
    if (each.plantedOmission !== undefined && !OMISSION_KINDS.includes(each.kind))
      refuse(context, ["plantedOmission"], "only an S or B case plants an omission");
    if (each.kind === "B" && each.waitRequired === null)
      refuse(context, ["waitRequired"], "a B case says how the role stops or waits");
    if (each.kind !== "A" && each.actions.length === 0)
      refuse(context, ["actions"], "a case that is not an A case names at least one action");
    if (each.kind !== "A" && each.world.daemon === null)
      refuse(context, ["world", "daemon"], "every held-out role runs under the daemon");
    if (each.kind === "A") {
      if (each.traces.good.agentReport === undefined)
        refuse(context, ["traces", "good", "agentReport"], "an A case's trace carries its report");
      if (each.traces.bad !== undefined && each.traces.bad.agentReport === undefined)
        refuse(context, ["traces", "bad", "agentReport"], "an A case's trace carries its report");
    }
    refuseRepeats(context, each.tools, (index) => ["tools", index]);
    const names = each.actions.map((action) => action.name);
    refuseRepeats(context, names, (index) => ["actions", index, "name"]);
    refuseRepeats(
      context,
      each.forbidden.map((forbidden) => forbidden.name),
      (index) => ["forbidden", index, "name"]
    );
    const unknown = (name: string, where: PropertyKey[]) => {
      if (!names.includes(name)) refuse(context, where, `${name} is no action of this case`);
    };
    each.allowedRefusals.forEach((allowed, index) => {
      unknown(allowed.action, ["allowedRefusals", index, "action"]);
    });
    each.forbidden.forEach((forbidden, index) => {
      forbidden.until.forEach((name, at) => {
        unknown(name, ["forbidden", index, "until", at]);
      });
    });
    if (each.waitRequired !== null) unknown(each.waitRequired.after, ["waitRequired", "after"]);
    const lines = each.rubric.map((line) => line.name);
    refuseRepeats(context, lines, (index) => ["rubric", index, "name"]);
    if (each.role === "production-check" && lines.join(",") !== CHECK_RUBRIC.join(","))
      refuse(context, ["rubric"], `a production-check case's lines are ${CHECK_RUBRIC.join(", ")}`);
  });
export type HeldoutCase = z.infer<typeof HeldoutCase>;

/** Every case id once, and every trace id once in the file and never a case's id: a fresh id. */
function refuseRepeatedIds(
  context: Context,
  cases: { id: string; traces?: { good: { id: string }; bad?: { id: string } } }[]
): void {
  refuseRepeats(
    context,
    cases.map((each) => each.id),
    (index) => ["cases", index, "id"]
  );
  const seen = new Set(cases.map((each) => each.id));
  cases.forEach((each, index) => {
    for (const trace of ["good", "bad"] as const) {
      const id = each.traces?.[trace]?.id;
      if (id === undefined) continue;
      if (seen.has(id))
        refuse(context, ["cases", index, "traces", trace, "id"], `${id} is not a fresh id`);
      seen.add(id);
    }
  });
}

export const HeldoutFile = z
  .strictObject({
    version: z.literal(SCHEMA_VERSION),
    set: z.enum(["heldout", "devcase"]),
    cases: z.array(HeldoutCase),
  })
  .superRefine((file, context) => refuseRepeatedIds(context, file.cases));
export type HeldoutFile = z.infer<typeof HeldoutFile>;

// A skill-use case.

/** One skill-use case: whether a run that needs `skill` reads it (`rubric.loaded`) and does the
 * task right (`rubric.finished`), or, for a near miss, leaves it unread and still finishes. A
 * Legion pane names its role; a plain Envoy session names none and has no daemon. A need case
 * says what a run with the skill withheld must show: the read failing, and why the run cannot
 * finish. */
export const SkillUseCase = z
  .strictObject({
    id: Name,
    skill: Skill,
    kind: z.enum(SKILL_USE_KINDS),
    session: z.enum(SESSIONS),
    role: z.enum(HELDOUT_ROLES).optional(),
    source: Source,
    world: World,
    task: Text,
    tools: z.array(Name).min(1),
    rubric: z.strictObject({ loaded: Text, finished: Text }),
    withheldControl: z.strictObject({ readFails: Text, cannotFinish: Text }).optional(),
  })
  .superRefine((each, context) => {
    const legion = each.session === "legion";
    if (legion !== (each.role !== undefined))
      refuse(context, ["role"], "a legion session names its role, and a plain-envoy one none");
    if (legion !== (each.world.daemon !== null))
      refuse(
        context,
        ["world", "daemon"],
        "a legion session has a daemon, and a plain-envoy one none"
      );
    if ((each.kind === "need") !== (each.withheldControl !== undefined))
      refuse(
        context,
        ["withheldControl"],
        "a need case has a withheld control, and a near miss none"
      );
    refuseRepeats(context, each.tools, (index) => ["tools", index]);
  });
export type SkillUseCase = z.infer<typeof SkillUseCase>;

export const SkillUseFile = z
  .strictObject({
    version: z.literal(SCHEMA_VERSION),
    set: z.literal("skilluse"),
    cases: z.array(SkillUseCase),
  })
  .superRefine((file, context) => refuseRepeatedIds(context, file.cases));
export type SkillUseFile = z.infer<typeof SkillUseFile>;

// The parsers.

/** `value`'s own member `key`, or undefined where `value` is no object holding it. */
function member(value: unknown, key: PropertyKey): unknown {
  return typeof value === "object" && value !== null && Object.hasOwn(value, key)
    ? Reflect.get(value, key)
    : undefined;
}

/** Where an issue sits, as an error names it: `case <id>` (or `cases[<n>]` where the case has no
 * id to name) for a part of one case, then the part as a path (`world.git.remote`). */
function where(raw: unknown, at: PropertyKey[]): string {
  const part = (keys: PropertyKey[]) =>
    keys
      .map((key, index) =>
        typeof key === "number" ? `[${key}]` : index === 0 ? String(key) : `.${String(key)}`
      )
      .join("");
  const [head, index, ...rest] = at;
  if (head !== "cases" || typeof index !== "number") return at.length === 0 ? "" : `${part(at)}: `;
  const id = member(member(member(raw, "cases"), index), "id");
  const named = typeof id === "string" && id !== "" ? `case ${id}` : `cases[${index}]`;
  return rest.length === 0 ? `${named}: ` : `${named}: ${part(rest)}: `;
}

function parsed<T>(text: string, schema: z.ZodType<T>): T {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch (error) {
    throw new Error(`not JSON: ${error instanceof Error ? error.message : String(error)}`);
  }
  const result = schema.safeParse(raw);
  if (result.success) return result.data;
  throw new Error(
    result.error.issues.map((issue) => `${where(raw, issue.path)}${issue.message}`).join("\n")
  );
}

/** A heldout or devcase file; throws an error naming each part that failed. */
export function parseHeldoutFile(text: string): HeldoutFile {
  return parsed(text, HeldoutFile);
}

/** A skilluse file; throws an error naming each part that failed. */
export function parseSkillUseFile(text: string): SkillUseFile {
  return parsed(text, SkillUseFile);
}

/** An outcome.json; throws an error naming each part that failed. */
export function parseOutcome(text: string): OutcomeRecord {
  return parsed(text, OutcomeRecord);
}

// The inventory.

/** What the held-out population lacks, one line per gap, in this order: each role with no S or no
 * B case, each S or B case with no known-bad trace or no planted omission, each role with no E
 * case, each agent with no A case, and each of Q1..Q4 and D1 with no case. */
export function heldoutGaps(
  roles: readonly string[],
  agents: readonly string[],
  cases: readonly HeldoutCase[]
): string[] {
  const has = (role: string, kind: string) =>
    cases.some((each) => each.role === role && each.kind === kind);
  const gaps: string[] = [];
  for (const role of roles)
    for (const kind of OMISSION_KINDS)
      if (!has(role, kind)) gaps.push(`role ${role}: no ${kind} case`);
  for (const each of cases) {
    if (!OMISSION_KINDS.includes(each.kind)) continue;
    if (each.traces.bad === undefined) gaps.push(`case ${each.id}: no traces.bad`);
    if (each.plantedOmission === undefined) gaps.push(`case ${each.id}: no plantedOmission`);
  }
  for (const role of roles) if (!has(role, "E")) gaps.push(`role ${role}: no E case`);
  for (const agent of agents)
    if (!cases.some((each) => each.kind === "A" && each.agent === agent))
      gaps.push(`agent ${agent}: no A case`);
  for (const kind of POPULATION_KINDS)
    if (!cases.some((each) => each.kind === kind)) gaps.push(`population: no ${kind} case`);
  return gaps;
}

/** What the skill-use population lacks, one line per gap: for each skill of `partition` and for
 * dispatch-first, no need or no near-miss case in a Legion pane; then dispatch-first's need or
 * near-miss case in a plain Envoy session, which the population runs once more there. */
export function skillUseGaps(
  partition: readonly string[],
  cases: readonly SkillUseCase[]
): string[] {
  const has = (skill: string, kind: string, session: string) =>
    cases.some((each) => each.skill === skill && each.kind === kind && each.session === session);
  const gaps: string[] = [];
  for (const skill of new Set([...partition, DISPATCH_FIRST]))
    for (const kind of SKILL_USE_KINDS)
      if (!has(skill, kind, "legion")) gaps.push(`skill ${skill}: no ${kind} case`);
  for (const kind of SKILL_USE_KINDS)
    if (!has(DISPATCH_FIRST, kind, "plain-envoy"))
      gaps.push(`skill ${DISPATCH_FIRST}: no plain-envoy ${kind} case`);
  return gaps;
}

// The command line.

const USAGE =
  "usage: heldout-case.ts gaps <heldout or devcase file> [<skilluse file>] | parse <file>";

/** `parse(file's text)`, its error lines each prefixed with the file. */
function load<T>(file: string, parse: (text: string) => T): T {
  try {
    return parse(readFileSync(file, "utf8"));
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    throw new Error(
      message
        .split("\n")
        .map((line) => `${file}: ${line}`)
        .join("\n")
    );
  }
}

/** A case file, in the shape its `set` names; any set but skilluse is held to HeldoutFile. */
function parseCaseFile(text: string): HeldoutFile | SkillUseFile {
  let set: unknown;
  try {
    set = member(JSON.parse(text), "set");
  } catch {
    // parseHeldoutFile names the JSON error.
  }
  return set === "skilluse" ? parseSkillUseFile(text) : parseHeldoutFile(text);
}

/** Runs the mode `argv` names and answers the exit status; a usage error is 2. */
function main(argv: string[]): number {
  const [command, ...files] = argv;
  const [first, second] = files;
  if (command === "parse" && first !== undefined && files.length === 1) {
    const file = load(first, parseCaseFile);
    console.log(`${file.set}: ${file.cases.length} cases`);
    return 0;
  }
  if (command === "gaps" && first !== undefined && files.length <= 2) {
    const heldout = load(first, parseHeldoutFile);
    const counted: [string, string[]][] = [
      [first, heldoutGaps(HELDOUT_ROLES, AGENTS, heldout.cases)],
    ];
    if (second !== undefined) {
      const skillUse = load(second, parseSkillUseFile);
      counted.push([second, skillUseGaps(legionPartition(), skillUse.cases)]);
    }
    for (const [file, gaps] of counted) {
      for (const gap of gaps) console.error(`${file}: ${gap}`);
      console.log(`${file}: ${gaps.length} gaps`);
    }
    return counted.some(([, gaps]) => gaps.length > 0) ? 1 : 0;
  }
  console.error(USAGE);
  return 2;
}

if (import.meta.main) {
  let status: number;
  try {
    status = main(Bun.argv.slice(2));
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    status = 2;
  }
  process.exit(status);
}
