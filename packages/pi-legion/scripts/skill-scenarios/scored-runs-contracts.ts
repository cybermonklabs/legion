/**
 * The shapes of the files that the parts of a scored run write and read: the rig, the label's
 * entry (`label-texts.sh` and its three tests), the stand-ins, the seed, the scorer and the
 * acceptance worker. The side that reads a file parses it with the schema here, so a file that one
 * side writes and the other reads in two shapes fails at the read, and the error names the field.
 * Every object is strict: a field this module does not name is refused. The one exception is the
 * recorded plans answer, a Dispatch answer whose fields are Dispatch's: it is read as far as the
 * rig needs it (`RecordedPlans`).
 *
 * A value that can be absent is `null`, never a missing key or an empty string. An instant is UTC
 * with `Z`, as Bun's `Date.toISOString()` and `date -u` write it. A path that a test binary of the
 * label reads is absolute, because that binary runs in its own package directory.
 */
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";

const nonEmpty = z.string().min(1);
const absolutePath = z.string().startsWith("/");
const instant = z.iso.datetime();
const count = z.number().int().nonnegative();
const positive = z.number().int().positive();
const httpStatus = z.number().int().min(100).max(599);
/** A whole git commit id, as `git rev-parse` prints it. */
const commitId = z.string().regex(/^[0-9a-f]{40}$/);
/** A Dispatch issue key, `<PROJECT>-<n>`. */
const issueKey = z.string().regex(/^[A-Z][A-Z0-9]*-[1-9]\d*$/);
/** A GitHub repository, `<owner>/<name>`. */
const repository = z.string().regex(/^[^/\s]+\/[^/\s]+$/);
/** A JSON object whose fields belong to another program: the label's, or a request's. */
const jsonObject = z.record(z.string(), z.unknown());

// C1. The files of a run's directory.

/** A line of `dispatch-proxy.jsonl`: one request to the run's Dispatch. `query` is the raw query
 * without its `?`, and `""` when the request has none. */
export const DispatchProxyLine = z.strictObject({
  at: instant,
  method: z.string().regex(/^[A-Z]+$/),
  path: absolutePath,
  query: z.string(),
  status: httpStatus,
});
export type DispatchProxyLine = z.infer<typeof DispatchProxyLine>;

/** The tools that `pod-standin.ts` stands in for. */
export const POD_TOOLS = ["kubectl", "docker", "podman", "helm", "gcloud", "kind"] as const;

/** A line of `pod-calls.jsonl`: one call of a pod tool. `argv` is the arguments after the tool's
 * name, and `starts` is whether the call would start a pod or a container. */
export const PodCallLine = z.strictObject({
  at: instant,
  tool: z.enum(POD_TOOLS),
  argv: z.array(z.string()),
  starts: z.boolean(),
});
export type PodCallLine = z.infer<typeof PodCallLine>;

/** The verdict of a clause: `"pending"` is a judged clause whose judgment is not written yet. */
export const ClausePass = z.union([z.boolean(), z.literal("pending")]);
export type ClausePass = z.infer<typeof ClausePass>;

/** A clause of a run: its id (the stage clause's id, or the bound's), the check's sentence, its
 * verdict, the evidence for it, and the counts it read, each by name. A reader names a clause by
 * its id alone: the sentence is for a person, and a sentence reworded must not change a count. */
export const ScoreClause = z.strictObject({
  id: nonEmpty,
  clause: nonEmpty,
  pass: ClausePass,
  evidence: nonEmpty,
  counts: z.record(nonEmpty, count),
});
export type ScoreClause = z.infer<typeof ScoreClause>;

/** A run's clauses: at least one, no two of one id. */
const ScoreClauses = z
  .array(ScoreClause)
  .min(1)
  .superRefine((clauses, context) => {
    const ids = clauses.map((clause) => clause.id);
    for (const id of new Set(ids.filter((id, at) => ids.indexOf(id) !== at)))
      context.addIssue({ code: "custom", message: `two clauses have the id ${id}` });
  });

/** A fact reported beside the clauses. It changes no verdict. */
export const Reported = z.strictObject({
  name: nonEmpty,
  value: z.union([z.boolean(), count, z.string()]),
  evidence: nonEmpty,
});
export type Reported = z.infer<typeof Reported>;

/** The hosts a reach names: the metadata server, by its name and its address, and the bucket's
 * API. */
export const REACH_HOSTS = [
  "metadata.google.internal",
  "169.254.169.254",
  "storage.googleapis.com",
] as const;

/** A tool argument of the run's agent, or of one of its subagents, that names a host of
 * `REACH_HOSTS`: the session, the message's index in that session, the tool and the host. A host
 * named in an assistant's text or in a tool's result is no reach. A reach changes no verdict and
 * leaves no run out. The record is a measurement, not a guard: it lists a host only where a tool's
 * arguments name it in plain text, so a host reached through a variable, an encoding or a file is
 * not listed. */
export const Reach = z.strictObject({
  session: nonEmpty,
  message: count,
  tool: nonEmpty,
  host: z.enum(REACH_HOSTS),
});
export type Reach = z.infer<typeof Reach>;

/** `score.json`: the run's clauses, what is reported beside them, and its reach. */
export const Score = z.strictObject({
  run: nonEmpty,
  clauses: ScoreClauses,
  reported: z.array(Reported),
  reach: z.array(Reach),
});
export type Score = z.infer<typeof Score>;

/** The verdict of a run over its clauses: `false` when a clause fails, `"pending"` when no clause
 * fails and a clause waits for its judgment, and `true` when every clause passes. */
export function runPass(clauses: readonly ScoreClause[]): ClausePass {
  if (clauses.some((clause) => clause.pass === false)) return false;
  if (clauses.some((clause) => clause.pass === "pending")) return "pending";
  return true;
}

/** `row.json` of a scored run: its clauses, its verdict over them (`runPass`), and its reach. */
export const ScoredRow = z
  .strictObject({
    outcome: z.literal("scored"),
    run: nonEmpty,
    scenario: nonEmpty,
    label: nonEmpty,
    case: nonEmpty,
    pass: ClausePass,
    clauses: ScoreClauses,
    reach: z.array(Reach),
  })
  .superRefine((row, context) => {
    const pass = runPass(row.clauses);
    if (row.pass !== pass) {
      context.addIssue({
        code: "custom",
        path: ["pass"],
        message: `the row says ${row.pass}, and its clauses say ${pass}`,
      });
    }
  });
export type ScoredRow = z.infer<typeof ScoredRow>;

/** `row.json` of a run that the rig could not score, with the reason. */
export const RigErrorRow = z.strictObject({
  outcome: z.literal("rig-error"),
  run: nonEmpty,
  scenario: nonEmpty,
  label: nonEmpty,
  case: nonEmpty,
  error: nonEmpty,
});
export type RigErrorRow = z.infer<typeof RigErrorRow>;

/** `row.json`: the run's row, or its rig error. */
export const RunRow = z.discriminatedUnion("outcome", [ScoredRow, RigErrorRow]);
export type RunRow = z.infer<typeof RunRow>;

// C2. The entry's files.

/** An issue of the world, as the label's engine records it. `parent` is `null` for a root, and
 * `phase` is `null` for an issue in no Legion phase. */
export const WorldIssue = z.strictObject({
  key: nonEmpty,
  title: nonEmpty,
  parent: nonEmpty.nullable(),
  tree: nonEmpty,
  status: nonEmpty,
  phase: nonEmpty.nullable(),
});
export type WorldIssue = z.infer<typeof WorldIssue>;

/** A notice row of the label's outbox: its issue, its notice kind, its row id, and its payload,
 * the label's notice as the row holds it. */
export const QueuedNotice = z.strictObject({
  issue: nonEmpty,
  kind: nonEmpty,
  row: positive,
  payload: jsonObject,
});
export type QueuedNotice = z.infer<typeof QueuedNotice>;

/** `texts/world-request.json`. `plan.task` is the task's id and `plan.text` the text of its
 * block; `plan.steps` are the task's step roles in order. `earlier[].verdict` is `null` for a
 * step that gives none. `gate.artifactId` is the run's real spec document, which the label's
 * catch-up notice names to the owner. */
export const WorldRequest = z.strictObject({
  project: nonEmpty,
  repository,
  defaultBranch: nonEmpty,
  workspace: absolutePath,
  issues: z.array(WorldIssue).min(1),
  gate: z
    .strictObject({
      issue: nonEmpty,
      step: nonEmpty,
      version: positive,
      approved: z.boolean(),
      artifactId: nonEmpty,
    })
    .nullable(),
  subject: nonEmpty,
  plan: z
    .strictObject({ task: nonEmpty, steps: z.array(nonEmpty).min(1), text: nonEmpty })
    .nullable(),
  earlier: z.array(
    z.strictObject({
      role: nonEmpty,
      handoffRecord: nonEmpty,
      handoffVersion: positive,
      commit: commitId,
      verdict: nonEmpty.nullable(),
    })
  ),
  pullRequest: z.strictObject({ number: positive, head: commitId }).nullable(),
  moment: z.enum(["step", "owner"]),
  role: nonEmpty,
});
export type WorldRequest = z.infer<typeof WorldRequest>;

/** `texts/world-answer.json`: every start row and every notice row, oldest first. */
export const WorldAnswer = z.strictObject({
  starts: z.array(z.strictObject({ role: nonEmpty, issue: nonEmpty, task: nonEmpty })),
  notices: z.array(QueuedNotice),
});
export type WorldAnswer = z.infer<typeof WorldAnswer>;

/** `texts/launch-request.json`. `ghToken` is the token the gh stand-in takes, and `path` the
 * pane's `PATH`. `planGuidanceFile` is the world's guidance file, absent when it has none. */
export const LaunchRequest = z.strictObject({
  project: nonEmpty,
  stateDir: absolutePath,
  workspace: absolutePath,
  daemonUrl: z.url(),
  envoyUrl: z.url(),
  envoyTokenFile: absolutePath,
  natsUrls: z.array(z.url()).min(1),
  bootTokenFile: absolutePath,
  ghToken: nonEmpty,
  ghPath: absolutePath,
  path: nonEmpty,
  claim: z.strictObject({ tree: nonEmpty, issue: nonEmpty, role: nonEmpty }),
  repository,
  defaultBranch: nonEmpty,
  instructionsFile: absolutePath,
  notices: z.array(QueuedNotice),
  planGuidanceFile: absolutePath.optional(),
});
export type LaunchRequest = z.infer<typeof LaunchRequest>;

/** `texts/launch-answer.json`: the system prompt's file, the pane's environment, and each publish
 * to the run's Envoy listener, with its dedupe key. */
export const LaunchAnswer = z.strictObject({
  systemPromptFile: absolutePath,
  env: z.record(z.string().regex(/^[A-Za-z_][A-Za-z0-9_]*$/), z.string()),
  published: z.array(z.strictObject({ topic: nonEmpty, message: nonEmpty, key: nonEmpty })),
});
export type LaunchAnswer = z.infer<typeof LaunchAnswer>;

/** The request of `label-texts.sh plan-check`: a plan version as the run's Dispatch holds it. */
export const PlanRequest = z.strictObject({
  artifactId: nonEmpty,
  version: positive,
  latestVersion: positive,
  blocks: z.array(
    z.strictObject({
      id: nonEmpty,
      type: nonEmpty,
      attributes: z.record(nonEmpty, z.string()),
      text: z.string(),
    })
  ),
});
export type PlanRequest = z.infer<typeof PlanRequest>;

/** The answer of `label-texts.sh plan-check`: the route's status and its body as text. */
export const PlanAnswer = z.strictObject({ status: httpStatus, body: z.string() });
export type PlanAnswer = z.infer<typeof PlanAnswer>;

// C3. The daemon stand-in's files.

/** A line of the stand-in's log. Each optional field is copied from the request's body when the
 * body holds it, whatever the route answered, so a refused request is logged too. `body` is what
 * the line keeps of the request's body: the whole parsed body of an owner's route (a sign-off, a
 * move back, a task stop, a release, a gate's or a plan's registration), and the whole `activity`
 * of an activity report (`backgroundWork` and `phase`, with its `nextReminderAt`). */
export const StandinLogLine = z.strictObject({
  at: instant,
  path: absolutePath,
  status: httpStatus,
  grantId: z.string().optional(),
  sessionId: z.string().optional(),
  mintedGrantId: z.string().optional(),
  handoffId: z.string().optional(),
  handoffVersion: z.number().optional(),
  body: jsonObject.optional(),
});
export type StandinLogLine = z.infer<typeof StandinLogLine>;

/** An issue of the `--issues` file, for the state document. `phase` is as `WorldIssue`'s. */
export const StandinIssue = z.strictObject({
  key: nonEmpty,
  tree: nonEmpty,
  phase: nonEmpty.nullable(),
  status: nonEmpty,
});
export type StandinIssue = z.infer<typeof StandinIssue>;

/** The `--issues` file. */
export const StandinIssues = z.array(StandinIssue);
export type StandinIssues = z.infer<typeof StandinIssues>;

// C4. A label manifest.

/** Where a manifest line is: its number from 1, and its text. */
const manifestPlace = { line: positive, text: nonEmpty };

/** One line of a label manifest, with its arguments by name. */
export const ManifestLine = z.discriminatedUnion("kind", [
  z.strictObject({ kind: z.literal("file"), commit: nonEmpty, path: nonEmpty, ...manifestPlace }),
  z.strictObject({
    kind: z.literal("reverse"),
    commit: nonEmpty,
    path: nonEmpty,
    ...manifestPlace,
  }),
  z.strictObject({ kind: z.literal("patch"), patchFile: nonEmpty, ...manifestPlace }),
  z.strictObject({ kind: z.literal("unpatch"), patchFile: nonEmpty, ...manifestPlace }),
  z.strictObject({
    kind: z.literal("drop"),
    path: nonEmpty,
    textFile: nonEmpty,
    ...manifestPlace,
  }),
  z.strictObject({
    kind: z.literal("replace"),
    path: nonEmpty,
    oldFile: nonEmpty,
    newFile: nonEmpty,
    ...manifestPlace,
  }),
]);
export type ManifestLine = z.infer<typeof ManifestLine>;

/** Each verb's arguments: the field each one goes to, and the word its usage shows. */
const MANIFEST_VERBS: Record<ManifestLine["kind"], readonly (readonly [string, string])[]> = {
  file: [
    ["commit", "<commit>"],
    ["path", "<path>"],
  ],
  reverse: [
    ["commit", "<commit>"],
    ["path", "<path>"],
  ],
  patch: [["patchFile", "<patch file>"]],
  unpatch: [["patchFile", "<patch file>"]],
  drop: [
    ["path", "<path>"],
    ["textFile", "<text file>"],
  ],
  replace: [
    ["path", "<path>"],
    ["oldFile", "<old file>"],
    ["newFile", "<new file>"],
  ],
};

/**
 * The lines of a label manifest, in order. A blank line, and a line whose first non-blank
 * character is `#`, is no line. Arguments are separated by blanks, so no argument holds one. A
 * line with another verb, or with a wrong count of arguments, is refused, and the error names the
 * line's number and text.
 */
export function parseManifest(text: string): ManifestLine[] {
  return text.split("\n").flatMap((raw, index) => {
    const line = raw.trim();
    if (line === "" || line.startsWith("#")) return [];
    const where = `manifest line ${index + 1} ${JSON.stringify(line)}`;
    const [verb = "", ...words] = line.split(/\s+/);
    if (!Object.hasOwn(MANIFEST_VERBS, verb)) {
      throw new Error(
        `${where}: no verb ${JSON.stringify(verb)}; the verbs are ${Object.keys(MANIFEST_VERBS).join(", ")}`
      );
    }
    const fields = MANIFEST_VERBS[verb as ManifestLine["kind"]];
    if (words.length !== fields.length) {
      const usage = fields.map(([, word]) => word).join(" ");
      throw new Error(
        `${where}: ${verb} takes ${fields.length} argument(s), ${usage}, and the line gives ${words.length}`
      );
    }
    return [
      ManifestLine.parse({
        kind: verb,
        ...Object.fromEntries(fields.map(([field], at) => [field, words[at]])),
        line: index + 1,
        text: line,
      }),
    ];
  });
}

// C5. Worlds.

export const OWNER_WALK_FORMS = ["A", "B", "C", "D", "E"] as const;
export type OwnerWalkForm = (typeof OWNER_WALK_FORMS)[number];
/** The kinds of owner: a root's architect, or a child's. */
export const OWNER_WALK_KINDS = ["root", "child"] as const;
export type OwnerWalkKind = (typeof OWNER_WALK_KINDS)[number];
/** The run's project in its own Dispatch, which the recorded plans answer's keys are mapped to. */
export const OWNER_WALK_PROJECT = "LWEVAL";
/** The forms whose owner gets no guidance file. */
const UNGUIDED_FORMS: readonly OwnerWalkForm[] = ["B", "E"];
export const FAMILIES = ["claude", "gpt"] as const;
const family = z.enum(FAMILIES);

/** The guidance an owner of form A, C or D gets: its file, relative to the world's directory, and
 * the families and models it names for the implementer and the reviewer. */
export const OwnerWalkGuidance = z.strictObject({
  file: nonEmpty,
  implementerFamily: family,
  reviewerFamily: family,
  implementerModel: nonEmpty,
  reviewerModel: nonEmpty,
});
export type OwnerWalkGuidance = z.infer<typeof OwnerWalkGuidance>;

/** `world.json` of `owner-walk`. `issue` is the file of the planned issue's spec, relative to the
 * world's directory. `plansProject` and `plansKeys` are the project and the keys of the recorded
 * plans answer. `questions` are the second turn's questions of the world's form. */
export const OwnerWalkWorld = z
  .strictObject({
    scenario: z.literal("owner-walk"),
    stage: nonEmpty,
    kind: z.enum(OWNER_WALK_KINDS),
    form: z.enum(OWNER_WALK_FORMS),
    family,
    issue: nonEmpty,
    plansProject: nonEmpty,
    plansKeys: z.array(nonEmpty).min(1),
    guidance: OwnerWalkGuidance.nullable(),
    questions: z.array(nonEmpty).min(1),
  })
  .superRefine((world, context) => {
    const unguided = UNGUIDED_FORMS.includes(world.form);
    if (unguided !== (world.guidance === null)) {
      context.addIssue({
        code: "custom",
        path: ["guidance"],
        message: `form ${world.form} ${unguided ? "has no guidance" : "has a guidance file"}`,
      });
    }
  });
export type OwnerWalkWorld = z.infer<typeof OwnerWalkWorld>;

/** A recorded issue of the plans answer with the run key the key map gives it, and its title. */
export const RecordedPlan = z.strictObject({ recorded: nonEmpty, key: issueKey, title: nonEmpty });
export type RecordedPlan = z.infer<typeof RecordedPlan>;

/** `seed.ts owner-walk`'s fixture (`fixture.json`): the world's issues as the run's Dispatch
 * holds them, E1's record, and the recorded plans' issues with their run keys. `owner` is the
 * owner's own issue (R for a root owner, C for a child owner) and `planned` is G. An issue's
 * `spec.approvedVersion` is the version of its spec the person approved, null where none was. */
export const OwnerWalkFixture = z.strictObject({
  scenario: z.literal("owner-walk"),
  kind: z.enum(OWNER_WALK_KINDS),
  project: z.literal(OWNER_WALK_PROJECT),
  owner: issueKey,
  planned: issueKey,
  issues: z
    .array(
      z.strictObject({
        key: issueKey,
        part: z.enum(["root", "child", "earlier", "planned"]),
        title: nonEmpty,
        parent: issueKey.nullable(),
        status: nonEmpty,
        spec: z.strictObject({ artifact: nonEmpty, approvedVersion: positive.nullable() }),
      })
    )
    .min(1),
  earlier: z.strictObject({
    key: issueKey,
    record: z.strictObject({ id: nonEmpty, version: positive }),
    session: nonEmpty,
    pr: positive,
    head: commitId,
  }),
  plans: z.array(RecordedPlan),
});
export type OwnerWalkFixture = z.infer<typeof OwnerWalkFixture>;

/** `captured.json`, which `seed.ts capture-scored` writes: the seed's fixture whole, and each issue
 * the fixture names as the run's Dispatch held it after the run: its status, every document with
 * every version (its authors and its markdown), and every handoff record with its author and
 * body. */
export const Captured = z.strictObject({
  fixture: OwnerWalkFixture,
  issues: z.array(
    z.strictObject({
      key: nonEmpty,
      title: nonEmpty,
      status: nonEmpty,
      parent: nonEmpty.nullable(),
      artifacts: z.array(
        z.strictObject({
          id: nonEmpty,
          name: nonEmpty,
          slug: nonEmpty,
          primary: z.boolean(),
          versions: z.array(
            z.strictObject({
              version: positive,
              created_at: nonEmpty,
              authors: z.array(jsonObject),
              markdown: z.string(),
            })
          ),
        })
      ),
      handoffs: z.array(
        z.strictObject({
          id: nonEmpty,
          phase: nonEmpty,
          version: positive,
          author: jsonObject,
          created_at: nonEmpty,
          body: z.string(),
        })
      ),
    })
  ),
});
export type Captured = z.infer<typeof Captured>;

// C6. A judgment.

/** `judgment.json`: the acceptance worker's verdict on each judged item of a run. */
export const Judgment = z.strictObject({
  run: nonEmpty,
  items: z
    .array(z.strictObject({ item: nonEmpty, verdict: z.enum(["pass", "fail"]), why: nonEmpty }))
    .min(1),
  by: nonEmpty,
  at: instant,
});
export type Judgment = z.infer<typeof Judgment>;

// C7. A scored cell.

/** A line of `score.ts cells`: a cell's scored runs, passes and pending runs against its count.
 * A cell with fewer scored runs than its count is `UNKNOWN`; else a cell with a pending run is
 * `PENDING`; else it is `pass` or `fail`. Neither of the first two is ever a pass. */
export const CellLine = z
  .strictObject({
    scenario: nonEmpty,
    label: nonEmpty,
    cell: nonEmpty,
    count: positive,
    scored: count,
    passes: count,
    pending: count,
    verdict: z.enum(["pass", "fail", "UNKNOWN", "PENDING"]),
  })
  .superRefine((cell, context) => {
    if (cell.passes + cell.pending > cell.scored) {
      context.addIssue({
        code: "custom",
        path: ["passes"],
        message: `${cell.passes} passes and ${cell.pending} pending runs are more than ${cell.scored} scored runs`,
      });
    }
    let due: "UNKNOWN" | "PENDING" | undefined;
    if (cell.scored < cell.count) due = "UNKNOWN";
    else if (cell.pending > 0) due = "PENDING";
    const decided = cell.verdict === "pass" || cell.verdict === "fail";
    if (due === undefined ? !decided : cell.verdict !== due) {
      context.addIssue({
        code: "custom",
        path: ["verdict"],
        message: `a cell with ${cell.scored} of ${cell.count} scored runs and ${cell.pending} pending is ${due ?? "pass or fail"}, not ${cell.verdict}`,
      });
    }
  });
export type CellLine = z.infer<typeof CellLine>;

// C8. The label's whole daemon in the rig, and the readings of a forward-only run.

/** The request of `label-texts.sh daemon`. `issue` is the world's issue key, `steps` the task's
 * Steps line as `plan.Step` strings, `world` the world's directory, `ghLog` the gh stand-in's log
 * (read for a merge it answered as merged), `omp` the pinned Oh My Pi, `home` the run's own HOME
 * copied from the label's profile, `profile` the label's Oh My Pi profile with its model route,
 * and `paneLogs` the directory each pane's own output is written under. `paneDaemon` is `label`
 * for every scored run; `intercept-report` serves the path check's one control. */
export const RigDaemonRequest = z.strictObject({
  moment: z.enum(["retro", "merging", "notice"]),
  role: z.enum(["implementer", "merger", "architect"]),
  issue: z.string(),
  steps: z.array(z.string()),
  world: z.string(),
  dispatchUrl: z.string(),
  dispatchTokenFile: z.string(),
  envoyUrl: z.string(),
  natsUrl: z.string(),
  postgresUrl: z.string(),
  ghLog: z.string(),
  omp: z.string(),
  home: z.string(),
  profile: z.string(),
  paneLogs: z.string(),
  paneDaemon: z.enum(["label", "intercept-report"]),
});
export type RigDaemonRequest = z.infer<typeof RigDaemonRequest>;

/** The `<ready>` file of `label-texts.sh daemon`, written once the daemon took the real pane's
 * `claims/ready`. `session` is the real pane's session, `replaced` the fake claimant's session it
 * replaced (null at a first launch), `systemPrompt` the prompt file the local runtime wrote for the
 * pane, and `sessionDir` where Oh My Pi writes the pane's session under the run's HOME. */
export const RigDaemonReady = z.strictObject({
  daemonUrl: z.string(),
  claim: z.string(),
  session: z.string(),
  replaced: z.string().nullable(),
  generation: z.number(),
  head: commitId,
  systemPrompt: z.string(),
  sessionDir: z.string(),
});
export type RigDaemonReady = z.infer<typeof RigDaemonReady>;

/** The `<status>` file, written every 10 seconds. `activity` is the newest `claims/activity` body
 * after the last turn's end, null before a turn ended. */
export const RigDaemonStatus = z.strictObject({
  at: z.string(),
  paneAlive: z.boolean(),
  turn: z.enum(["running", "idle"]),
  activity: z.unknown().nullable(),
});
export type RigDaemonStatus = z.infer<typeof RigDaemonStatus>;

/** The `<record>` file, written at SIGTERM. `calls` is every request to the daemon's HTTP API in
 * the run, whatever its status. `defects` is each open, carried and withdrawn defect of the issue.
 * `steps` is the issue's step records as the run's Dispatch serves them. */
export const RigDaemonRecord = z.strictObject({
  issue: z.string(),
  phase: z.string(),
  generation: z.number(),
  calls: z.array(
    z.strictObject({
      at: z.string(),
      method: z.string(),
      path: z.string(),
      status: z.number(),
      body: z.unknown(),
      answer: z.unknown(),
    })
  ),
  defects: z.array(
    z.strictObject({
      id: z.string(),
      text: z.string(),
      role: z.string(),
      head: z.string(),
      generation: z.number(),
      carriedBy: z.number().nullable(),
      withdrawn: z.string().nullable(),
    })
  ),
  steps: z.array(
    z.strictObject({
      task: z.string(),
      step: z.number(),
      kind: z.string(),
      role: z.string(),
      attempt: z.number(),
      result: z.string(),
    })
  ),
});
export type RigDaemonRecord = z.infer<typeof RigDaemonRecord>;

/** `readings.json` of a forward-only or forward-owner run: each flagged message's reading, by its
 * index, the rubric's item, the verdict, the reader and its model. */
export const ForwardReadings = z.strictObject({
  messages: z.array(
    z.strictObject({
      index: z.number(),
      item: z.union([z.literal(1), z.literal(2)]),
      verdict: z.enum(["yes", "no"]),
      reader: z.string(),
      model: z.string(),
    })
  ),
});
export type ForwardReadings = z.infer<typeof ForwardReadings>;

// The recorded plans answer.

/** `plans-answer.source`, beside a recorded plans answer: the label commit the capture ran at,
 * the sha256 of the answer's raw body, and the map from the answer's project key and issue keys
 * to the run's. */
export const PlansAnswerSource = z.strictObject({
  commit: commitId,
  sha256: z.string().regex(/^[0-9a-f]{64}$/),
  keyMap: z.record(nonEmpty, nonEmpty),
});
export type PlansAnswerSource = z.infer<typeof PlansAnswerSource>;

/** The recorded plans answer (`GET /api/v1/projects/{key}/plans`) as far as the rig reads it: its
 * project, and each issue with its title and each rating's reason. The rest is Dispatch's. */
export const RecordedPlans = z.looseObject({
  project: nonEmpty,
  issues: z.array(
    z.looseObject({
      issue: nonEmpty,
      title: nonEmpty,
      ratings: z.array(z.looseObject({ reason: z.string().nullish() })).nullish(),
    })
  ),
});
export type RecordedPlans = z.infer<typeof RecordedPlans>;

/** A recorded plans answer held to its source (readPlansAnswer): the answer, and each of its
 * issues with its run key, in the order of those keys' numbers. */
export interface HeldPlansAnswer {
  answer: RecordedPlans;
  plans: RecordedPlan[];
}

/** The source of a recorded plans answer: `plans-answer.source`, beside the answer. */
export const PLANS_SOURCE = "plans-answer.source";

/** A recorded plans answer as readRecordedPlans reads it: its raw bytes, its source, and the
 * answer held to that source. */
export interface RecordedPlansFiles {
  raw: Buffer;
  source: PlansAnswerSource;
  held: HeldPlansAnswer;
}

/**
 * The recorded plans answer in `answerFile`, held to its source beside it (PLANS_SOURCE) by
 * readPlansAnswer for `project`: its raw bytes, its source and the held answer. A file it cannot
 * read, a source that does not parse and an answer its source does not hold all throw one error
 * that names both files. The proxy, the seed, the world builder and the scorer read a recording
 * only through this.
 */
export function readRecordedPlans(answerFile: string, project: string): RecordedPlansFiles {
  const sourceFile = path.join(path.dirname(answerFile), PLANS_SOURCE);
  try {
    const raw = readFileSync(answerFile);
    const source = PlansAnswerSource.parse(JSON.parse(readFileSync(sourceFile, "utf8")));
    return { raw, source, held: readPlansAnswer(raw, source, project) };
  } catch (error) {
    throw new Error(
      `${answerFile}, with its source ${sourceFile}: ${error instanceof Error ? error.message : String(error)}`
    );
  }
}

/**
 * The recorded plans answer `raw`, held to its source: its sha256 is the source's, it is a plans
 * answer, and the source's key map takes its project to `project`, each of its issues to a key of
 * `project`, no two of them to one key, and nothing the answer does not hold. Anything else
 * throws, naming each fault. readRecordedPlans reads a recording from its files through this.
 */
export function readPlansAnswer(
  raw: Uint8Array,
  source: PlansAnswerSource,
  project: string
): HeldPlansAnswer {
  const sum = createHash("sha256").update(raw).digest("hex");
  if (sum !== source.sha256)
    throw new Error(
      `its sha256 is ${sum}, and its source records ${source.sha256}: it is not the answer the capture recorded`
    );
  let parsed: unknown;
  try {
    parsed = JSON.parse(Buffer.from(raw).toString("utf8"));
  } catch (error) {
    throw new Error(`it is no JSON: ${error}`);
  }
  const read = RecordedPlans.safeParse(parsed);
  if (!read.success) throw new Error(`it is no plans answer: ${read.error.message}`);
  const answer = read.data;
  const { keyMap } = source;
  const problems: string[] = [];
  if (keyMap[answer.project] !== project)
    problems.push(
      `it maps the recorded project ${answer.project} to ${keyMap[answer.project] ?? "nothing"}, not ${project}`
    );
  const runKey = new RegExp(`^${project}-[1-9]\\d*$`);
  const plans = answer.issues.flatMap((issue) => {
    const key = keyMap[issue.issue];
    if (key !== undefined && runKey.test(key))
      return [{ recorded: issue.issue, key, title: issue.title }];
    problems.push(
      `it maps the recorded issue ${issue.issue} to ${key ?? "nothing"}, which is no key of ${project}`
    );
    return [];
  });
  const held = new Set([answer.project, ...answer.issues.map((issue) => issue.issue)]);
  for (const from of Object.keys(keyMap))
    if (!held.has(from)) problems.push(`it maps ${from}, which the answer does not hold`);
  const keys = plans.map((plan) => plan.key);
  for (const key of new Set(keys.filter((key, at) => keys.indexOf(key) !== at)))
    problems.push(`it maps two recorded issues to ${key}`);
  if (problems.length > 0)
    throw new Error(`the key map of its source does not fit it: ${problems.join("; ")}`);
  const number = (key: string) => Number(key.slice(key.lastIndexOf("-") + 1));
  return { answer, plans: plans.sort((a, b) => number(a.key) - number(b.key)) };
}

/** Every whole key of `keys` in a text, the longest tried first: a key no letter, digit, `-` or `_`
 * stands beside, so a key that begins another (`P-1` and `P-12`) is only itself. Global. */
export function wholeKeys(keys: Iterable<string>): RegExp {
  const alternatives = [...keys]
    .sort((a, b) => b.length - a.length)
    .map((key) => key.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  return alternatives.length === 0
    ? /(?!)/g
    : new RegExp(`(?<![A-Za-z0-9_-])(?:${alternatives.join("|")})(?![A-Za-z0-9_-])`, "g");
}

/** `text` with each whole key of `keyMap` (wholeKeys) replaced by its value, in one pass, so no
 * value is read again as a key: the text of the recorded plans answer as the proxy serves it, and
 * so as the run's agents read it. */
export function mapRecordedKeys(text: string, keyMap: Readonly<Record<string, string>>): string {
  return text.replace(wholeKeys(Object.keys(keyMap)), (key) => keyMap[key] ?? key);
}
