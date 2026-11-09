/**
 * The driver of plan-pointer-drive.sh: the parts of one run that need the Dispatch API, the
 * daemon's own prompt composer, or the session's transcript. The shell runs it once per step,
 * and every step reads and writes the run directory the shell gives it.
 *
 *   PLAYWRIGHT_BASE_URL=http://127.0.0.1:<port> E2E_AGENT_TOKEN=e2e-token \
 *     bun scripts/e2e/plan-pointer-drive.ts <step> <run directory> [...]
 *
 * The steps, in the order one attempt of a run makes them:
 *
 *   seed <run> <tasks>          Seeds the staged issue on the run's own Dispatch through
 *                               packages/dispatch/e2e/api.ts, with a plan of one task or two,
 *                               and writes <run>/seed.json and <run>/task.txt.
 *   compose <run> <module> <issue>
 *                               Composes the implementer's pane prompt for that issue from that
 *                               daemon module with workerPane, and writes the whole
 *                               `--append-system-prompt` argument, as shell text, to
 *                               <run>/arg.txt.
 *   advance <run> <seconds> <approved|unapproved>
 *                               The watcher: waits for the session's first read of the plan's
 *                               text in the transcripts under <run>/sess, writes the mid-session
 *                               plan version, approves it with a plan review when the third
 *                               argument is `approved`, and writes <run>/advance.json. With no
 *                               such read inside <seconds>, it writes that version at the bound
 *                               and says so.
 *   score <run> <arm> <n> <attempt>
 *                               Reads the transcripts and the file of each task the plan holds,
 *                               prints the run's row, and writes <run>/row.json.
 *   summarize <arm> <run>...    Prints one summary line over those runs' rows.
 *
 * The staged plan carries four designs, as INFRA-1992's own history carried them. `early` is the
 * design of versions 1 to 3, which the task's handoff pointer cites and which the plan review
 * approved at version 1. `standing` is the design of versions 4 to 6, approved at version 6, so
 * it is the newest approved design when the session begins. `unapproved` is the design of
 * versions 7 and 8, which no plan review approves: version 8 is the newest version at the start
 * and a worker must not build from it. `mid` is the design of version 9, which the watcher writes
 * while the session runs. The watcher has two shapes: with `approved` it records a plan review of
 * version 9, so the correct build becomes `mid`; with `unapproved` it records none, so the
 * correct build stays `standing`.
 *
 * The staged plan holds one task or two, and the row and the summary line both name which. With
 * two tasks the plan asks for `note: <call>` in the first task's file and `adopt: <call>` in the
 * second, and the second is the file the row scores. With one task the plan holds that scored
 * task alone: the session writes `adopt: <call>` and no first file, so the row's first-wrote and
 * first-design read `none` and designs-differ is `no`.
 *
 * Every plan.md version carries the marker `PLANREV-<version>`, which is how a tool result in the
 * transcript says which version of the plan's text the session read, and how the watcher tells a
 * read of the plan from any other command. A handoff body carries no such marker: it cites its
 * plan version in words, as the planner's record does.
 */
import {
  existsSync,
  readdirSync,
  readFileSync,
  renameSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import type { EditArtifactResponse, EditOp } from "@legion/contracts";
import {
  createHandoff,
  createIssue,
  createIssueArtifact,
  createPlanReview,
  createProject,
  editArtifact,
} from "../../packages/dispatch/e2e/api";
import { workerPane } from "../../packages/pi-legion/scripts/skill-scenarios/daemon-pane";

/** The name of one of the four designs the staged plan carries. */
type DesignName = "early" | "standing" | "unapproved" | "mid";

/** What a file the session wrote names: one design, `none` when the file holds no design's
 *  call, and `several` when it holds more than one. */
type WroteName = DesignName | "none" | "several";

/** The row's answer on the scored file, read against the approvals as they stood when the
 *  session wrote that file. The row takes the first of these words that holds, and the summary
 *  counts one field for each of them:
 *
 *    approved    the file names the newest approved design, which is what the rule asks for;
 *    pointer     it names the design of the plan.md version the task line's handoff record
 *                cites, which is the build the rule forbids;
 *    stale       it names an approved design that is neither the newest approved one nor the
 *                pointer's, which is lag;
 *    unapproved  it names a design no plan review approved;
 *    other       it names no design's call, or more than one.
 */
const verdicts = ["approved", "pointer", "stale", "unapproved", "other"] as const;
type Verdict = (typeof verdicts)[number];

/** One of the four designs the staged plan carries, each differing in the one call the plan
 *  tells the implementer to write into its file, which is what makes the file say which design
 *  the session built from. */
interface Design {
  /** The design's name in a row: which of the four it is. */
  readonly name: DesignName;
  /** The call the implementer writes, and the token the score reads the file for. */
  readonly call: string;
  /** The sentence the plan's design section holds, which an edit replaces whole. */
  readonly sentence: string;
}

/** The design the version the task's handoff pointer cites holds, approved at that version. */
const early: Design = {
  call: "pulumi.Import",
  name: "early",
  sentence: "The migration takes over the bucket that exists already, with `pulumi.Import`.",
};
/** The design of the newest version the plan review approved before the session begins. */
const standing: Design = {
  call: "aws.s3.BucketV2",
  name: "standing",
  sentence:
    "The migration leaves that bucket alone and declares a new one, with `aws.s3.BucketV2`.",
};
/** The design of the newest versions at the session's start, which no plan review approves. */
const unapproved: Design = {
  call: "aws.s3.BucketReplicationConfig",
  name: "unapproved",
  sentence:
    "The migration copies the objects to a bucket in the new account, with" +
    " `aws.s3.BucketReplicationConfig`.",
};
/** The design the watcher writes while the session runs. */
const mid: Design = {
  call: "pulumi.Alias",
  name: "mid",
  sentence:
    "The migration renames the bucket in place and keeps its old name, with `pulumi.Alias`.",
};

/** The four designs, in the order the plan writes them: this list is what a file the session
 *  wrote is read against and what the summary counts, so a fifth design is never counted as
 *  `wrote-other`. */
const designs: readonly Design[] = [early, standing, unapproved, mid];

/** The project the staged issue is filed under. The daemon's own token for a claim is this key
 *  in lower case, which is what the pane the shell composes is launched for. */
const projectKey = "DRIVE";
/** How many versions plan.md holds when the session begins. */
const versionsAtStart = 8;
/** The first plan.md version `standing` is written at. */
const standingFrom = 4;
/** The first plan.md version `unapproved` is written at. */
const unapprovedFrom = 7;
/** The plan.md version the watcher writes while the session runs, which carries `mid`. */
const midVersion = versionsAtStart + 1;
/** The plan.md versions the plan review approved before the session begins, oldest first. The
 *  newest of them is the approved plan at the session's start, and the one the task's pointer
 *  cites is an older approved version. */
const approvedAtStart: readonly number[] = [1, 6];
/** The newest of those versions: the approved plan when the session begins. */
const approvedVersionAtStart = approvedAtStart[approvedAtStart.length - 1];
/** How many `plan` handoff records the issue holds. */
const handoffRecords = 6;
/** The `plan` handoff record the task's line names, which is the first of them. The newest
 *  record is `handoffRecords`, and the task's line names an older one: the pointer the task
 *  carries is the stale one. */
const taskHandoffVersion = 1;
/** The plan.md version that record cites. The newest record cites `versionsAtStart` instead. */
const taskHandoffCites = 1;

/** The design plan.md carries at `version`. */
function designAt(version: number): Design {
  if (version >= midVersion) return mid;
  if (version >= unapprovedFrom) return unapproved;
  if (version >= standingFrom) return standing;
  return early;
}

/** The file the first task of a two-task plan writes, the file the scored task writes, and the
 *  file the round's `implement` handoff body goes to. The task line the seed writes names them,
 *  and the score reads the same two task files. */
const firstFile = "note.txt";
const scoredFile = "adoption.txt";
const handoffFile = "implement.json";

/** What the shell and the later steps read of the seed: what this run alone has, and the two
 *  numbers the shell reads the staged plan back against. Everything else a step needs is a
 *  constant of this file, so no step reads a staged number from a file it could read here. */
interface Seed {
  readonly issue: string;
  readonly title: string;
  readonly planArtifact: string;
  /** How many tasks the staged plan holds: which of the two stagings this run is. */
  readonly tasks: 1 | 2;
  /** How many versions plan.md holds when the session begins: the shell reads this number back
   *  from the `Versions:` line of `dispatch read`. */
  readonly versionsAtStart: number;
  /** The newest plan.md version the plan review approved before the session begins: the shell
   *  reads this number back from the `Approval:` line of the same read. */
  readonly approvedPlanVersionAtStart: number;
}

/** What the watcher leaves for the score. */
interface Advance {
  /** `plan-read` when the session's own first read of the plan's text fired it, `bound` when
   *  no such read appeared inside the bound the shell set. */
  readonly trigger: "plan-read" | "bound";
  /** The transcript's own time of the tool result that fired the trigger, or the empty string
   *  at the bound. */
  readonly firedAt: string;
  /** When Dispatch had the new version, and its approval where the watcher records one. */
  readonly landedAt: string;
  readonly planVersion: number;
  /** Whether the watcher recorded a plan review of that version: the watcher's shape. */
  readonly approved: boolean;
}

/** One read of the plan's text in the transcript: when the tool result arrived, and every
 *  plan.md version its text carried a marker of. */
interface PlanRead {
  readonly at: string;
  readonly versions: readonly number[];
}

/** One run's measurement, as the row prints it and the summary counts it. */
interface Row {
  readonly arm: string;
  readonly run: string;
  /** Which attempt of that run wrote this row: an unscorable attempt is made again. */
  readonly attempt: number;
  readonly issue: string;
  /** How many tasks the staged plan held: which of the two stagings made this row. */
  readonly tasks: 1 | 2;
  /** The watcher's shape: `approved` when it recorded a plan review of the mid-session version,
   *  `unapproved` when it recorded none. */
  readonly midApproval: "approved" | "unapproved";
  /** The `plan` handoff record the task line names. */
  readonly taskHandoffVersion: number;
  /** The plan.md version that record cites. */
  readonly handoffCitesPlanVersion: number;
  readonly newestAtStart: number;
  /** The newest plan.md version the plan review approved before the session began. */
  readonly approvedAtStart: number;
  /** The plan.md version the watcher wrote while the session ran. */
  readonly midVersion: number;
  readonly reads: readonly PlanRead[];
  readonly trigger: Advance["trigger"];
  readonly firedAt: string;
  readonly landedAt: string;
  /** The transcript's own time of the write tool result for the scored task's file, or `none`
   *  when no transcript holds one and the file does not exist. */
  readonly scoredWroteAt: string;
  /** Where that time came from: `tool-result` the write call's own result, `file` the file's
   *  modification time where the session wrote it by another route, `none` neither. */
  readonly scoredWroteFrom: "tool-result" | "file" | "none";
  /** The call the session wrote into the first task's file, `none` when the file holds no
   *  design's call and `several` when it holds more than one. A one-task staging writes no such
   *  file, so it reads `none` there. */
  readonly firstWrote: string;
  /** Which design that call is. */
  readonly firstDesign: WroteName;
  /** The call the session wrote into the scored task's file, the one the summary counts, read
   *  the same way. The scored task is the second of two, or the only one of a one-task plan. */
  readonly wrote: string;
  /** Which design that call is. */
  readonly design: WroteName;
  /** The design of the newest approved plan.md version the session could have read when it
   *  wrote the scored file: what a worker that follows the rule writes. */
  readonly approvedDesign: DesignName;
  /** Whether that newest approved version is the watcher's own. The watcher's plan review
   *  counts for a scorable attempt alone, and only where the session read the plan after that
   *  version landed, or read a text that carried it, and in both cases before it wrote the
   *  scored file. A session that read the approved plan, and wrote that design a second after
   *  the watcher moved the plan, never saw the watcher's version. */
  readonly countedMidApproval: boolean;
  /** What the scored file's design was at that moment. The five words are `verdicts`' own, and
   *  that list, the script header and scripts/e2e/README.md describe each one. */
  readonly verdict: Verdict;
  /** Whether the two files name different designs, which is the plan read again between the two
   *  tasks. Two files that name the same design say nothing about a second read, and a file
   *  naming no design or several says nothing either. A one-task staging has one file, so this
   *  is always false there. */
  readonly designsDiffer: boolean;
  /** Whether a plan read of the session's carried the mid-session version: the session read the
   *  plan after the watcher moved it. A session that read the plan at no time reads `false`
   *  here, and so does one that read it before the watcher moved it. */
  readonly readAfterChange: boolean;
  /** False when the scored file was written before the mid-session version landed, which is a
   *  run to make again rather than a pass or a failure. */
  readonly scorable: boolean;
}

function fail(why: string): never {
  process.stderr.write(`plan-pointer-drive.ts: ${why}\n`);
  process.exit(2);
}

function readJson<T>(file: string): T {
  return JSON.parse(readFileSync(file, "utf8")) as T;
}

function writeJson(file: string, value: unknown): void {
  writeFileSync(file, `${JSON.stringify(value, null, 2)}\n`);
}

/** The same JSON, written where no reader can read it half written: `writeFileSync` leaves a
 *  part of the file readable while it runs, and the shell waits for the watcher's own file to
 *  appear. The writer writes `<file>.part` first and renames it, which is one step for a
 *  reader, so the file appears whole. Both names are in the run's own directory. */
function writeJsonWhole(file: string, value: unknown): void {
  const part = `${file}.part`;
  writeJson(part, value);
  renameSync(part, file);
}

/** One edit of plan.md, refused when it did not apply every op or did not mint `version`, whose
 *  summary is that version's own. The e2e client types the answer as `{ applied }` alone, so the
 *  contract's own type is what reads the minted version. `whose` names the edit in the refusal,
 *  which a failed run keeps as its evidence. */
async function editPlan(
  artifact: string,
  ops: EditOp[],
  version: number,
  whose: string
): Promise<void> {
  const edited = (await editArtifact(
    artifact,
    { ops, summary: `revision ${version}` },
    { as: "agent" }
  )) as EditArtifactResponse;
  if (edited.applied !== ops.length || edited.version?.number !== version) {
    fail(`${whose} applied ${JSON.stringify(edited)}`);
  }
}

/** The plan's text at `revision`, carrying `design`, with `tasks` tasks. Every version is this
 *  text: an edit moves the marker, and each design change replaces the one sentence. Each
 *  task asks for the design's own call in a file of its own. With two tasks, a worker that reads
 *  the plan again before the second task writes the design that stands then, and a worker that
 *  reads it once writes the same design twice. With one task, the plan holds the scored task
 *  alone, which begins after the mid-session version lands. */
function planText(issue: string, revision: number, design: Design, tasks: 1 | 2): string {
  const designNote =
    tasks === 1
      ? ""
      : `## Task 1: write the design note

Write one line to the file your task names for this task: \`note: <call>\`.
\`<call>\` is the call the design above names. Write that line and nothing else.

`;
  return `# How ${issue} moves the bucket

Plan revision PLANREV-${revision}.

## The design

${design.sentence}

${designNote}## Task ${tasks}: write the adoption line

Write one line to the file your task names for this task: \`adopt: <call>\`.
\`<call>\` is the call the design above names. Write that line and nothing else.
Do no other task of this issue.
`;
}

/** The plan handoff record at `version`, citing the plan.md version it was written beside. The
 *  body names no call of any design: the record is a pointer, and the design is the document's. */
function handoffBody(issue: string, version: number, citesPlanVersion: number): object {
  return {
    issue,
    phase: "plan",
    planDocument: `dispatch://${issue}/artifact/plan-md@v${citesPlanVersion}`,
    planVersion: citesPlanVersion,
    requiredSkills: { implementer: ["skill://legion-worker"] },
    schemaVersion: 1,
    summary: `The plan for ${issue}, round ${version}.`,
  };
}

/** The task line the session is given, written from this file's own constants and its own list of
 *  the staged tasks, so one change cannot make the row print a version the session was never
 *  told. The `Handoff:` sentence is the shape the daemon renders (task,
 *  packages/daemon/internal/workflow/effects.go): the phase's word, the version the daemon
 *  accepted, and the command that reads that version. Nothing else in the task names a plan
 *  version, and nothing in it says the plan can have moved. */
function taskText(runDir: string, issue: string, title: string, tasks: 1 | 2): string {
  const steps =
    tasks === 1
      ? [`write task 1's line to ${path.join(runDir, scoredFile)} with the write tool`]
      : [
          `write task 1's line to ${path.join(runDir, firstFile)} with the write tool`,
          `write task 2's line to ${path.join(runDir, scoredFile)} with the write tool`,
        ];
  steps.push(
    `write your \`implement\` handoff body to ${path.join(runDir, handoffFile)} with the write tool`
  );
  const round =
    tasks === 1 ? "one task and your own record of it" : "two tasks and your own record of them";
  const files = steps.length === 2 ? "two" : "three";
  return (
    `Continue ${title}. Issue: ${issue}. Phase: implementing.` +
    ` Handoff: plan version ${taskHandoffVersion}` +
    ` (dispatch handoff-read --issue ${issue} --phase plan --version ${taskHandoffVersion}).` +
    ` This box carries no workspace, no jj and no GitHub for this issue, so your round is the` +
    ` plan's ${round}: ${steps.join(", then ")}.` +
    ` Record nothing with dispatch and open no pull request: the round ends when the ${files}` +
    ` files are written.\n`
  );
}

/** The session the seed's handoff records are written under, as a planner's own would be. */
const planner = { id: "plan-pointer-drive-planner", kind: "session" as const };

async function seed(runDir: string, tasks: 1 | 2): Promise<void> {
  const title = "Move the bucket to the new account";
  await createProject({ key: projectKey, name: "Plan pointer drive" });
  const issue = await createIssue({
    project: projectKey,
    spec: `The account move needs the bucket in the new account, with its objects and its name.

:::requirement{#r1}
The bucket serves the same objects from the new account.
:::
`,
    title,
  });
  const upload = await createIssueArtifact(issue.key, {
    content: planText(issue.key, 1, early, tasks),
    name: "plan.md",
  });
  const plan = upload.artifact.id;
  if (upload.artifact.slug !== "plan-md") fail(`the plan uploaded as ${upload.artifact.slug}`);
  // Each approval is recorded beside the version it approves, as a plan review records one, so
  // the newest of them is the approval every later read of the document shows.
  if (approvedAtStart.includes(1)) await createPlanReview(plan, 1);
  for (let version = 2; version <= versionsAtStart; version += 1) {
    const ops: EditOp[] = [
      { find: `PLANREV-${version - 1}`, op: "replace", with: `PLANREV-${version}` },
    ];
    const before = designAt(version - 1);
    const now = designAt(version);
    if (now !== before) ops.push({ find: before.sentence, op: "replace", with: now.sentence });
    await editPlan(plan, ops, version, `the edit to plan.md version ${version}`);
    if (approvedAtStart.includes(version)) await createPlanReview(plan, version);
  }
  // The record the task's line names cites `taskHandoffCites`, the newest record cites the
  // newest version, and every other record cites the version of its own round. The first two
  // are the answers the drive is about, and the issue carries both before the session starts.
  for (let version = 1; version <= handoffRecords; version += 1) {
    const cites =
      version === taskHandoffVersion
        ? taskHandoffCites
        : version === handoffRecords
          ? versionsAtStart
          : version;
    const recorded = await createHandoff(
      issue.key,
      "plan",
      handoffBody(issue.key, version, cites),
      planner
    );
    if (recorded.version !== version) {
      fail(`the plan handoff recorded as version ${recorded.version}, not ${version}`);
    }
  }
  const seeded: Seed = {
    approvedPlanVersionAtStart: approvedVersionAtStart,
    issue: issue.key,
    planArtifact: plan,
    tasks,
    title,
    versionsAtStart,
  };
  writeJson(path.join(runDir, "seed.json"), seeded);
  writeFileSync(path.join(runDir, "task.txt"), taskText(runDir, issue.key, title, tasks));
  process.stdout.write(
    `   seeded ${issue.key}: plan.md v${versionsAtStart} (${tasks} task(s), ${standing.call}` +
      ` from v${standingFrom}, ${unapproved.call} from v${unapprovedFrom}), ${handoffRecords} plan` +
      ` handoffs, the task's v${taskHandoffVersion} citing plan.md v${taskHandoffCites}` +
      ` (${early.call}), approved at v${approvedAtStart.join(" and v")}\n`
  );
}

function compose(runDir: string, daemonModule: string, issue: string): void {
  const pane = workerPane(
    { port: 1, profile: process.env.OMP_PROFILE ?? "default", rig: path.join(runDir, "pane") },
    process.env,
    { issue, project: projectKey.toLowerCase(), role: "implementer" },
    { daemonModule, ghPath: process.env.LEGION_GH_PATH, systemPrompt: true }
  );
  const argument = pane.systemPromptArgument ?? "";
  if (argument === "") fail(`the pane composed from ${daemonModule} carries no system prompt`);
  writeFileSync(path.join(runDir, "arg.txt"), argument);
}

/** Every transcript the session has written, oldest name first: each `.jsonl` under the session
 *  directory at any depth, since a subagent's transcript is a file in a directory of its own
 *  beside the session's. */
function transcripts(runDir: string): string[] {
  const dir = path.join(runDir, "sess");
  if (!existsSync(dir)) return [];
  return readdirSync(dir, { recursive: true })
    .map((name) => String(name))
    .filter((name) => name.endsWith(".jsonl"))
    .sort()
    .map((name) => path.join(dir, name));
}

/** One entry of the session's transcript, as the watcher and the score read it. */
interface Entry {
  readonly type?: string;
  readonly timestamp?: string;
  readonly message?: unknown;
}

/** Every entry of the transcript, skipping a last line a live session has half written. */
function entries(file: string): Entry[] {
  const read: Entry[] = [];
  for (const line of readFileSync(file, "utf8").split("\n")) {
    if (line === "") continue;
    try {
      read.push(JSON.parse(line));
    } catch {
      break;
    }
  }
  return read;
}

/** Every entry of every transcript the session has written. */
function allEntries(runDir: string): Entry[] {
  return transcripts(runDir).flatMap((file) => entries(file));
}

/** Every read of the plan's text in the transcripts, oldest first: a tool result whose own text
 *  carries a `PLANREV-<n>` marker, which only a version of plan.md holds. */
function planReads(parsed: readonly Entry[]): PlanRead[] {
  const reads: PlanRead[] = [];
  for (const entry of parsed) {
    const message = entry.message as { role?: string } | undefined;
    if (entry.type !== "message" || message?.role !== "toolResult") continue;
    const versions = [...JSON.stringify(message).matchAll(/PLANREV-(\d+)/g)].map((match) =>
      Number(match[1])
    );
    if (versions.length === 0) continue;
    reads.push({
      at: entry.timestamp ?? "",
      versions: [...new Set(versions)].sort((a, b) => a - b),
    });
  }
  return reads.sort((one, other) => (one.at < other.at ? -1 : one.at > other.at ? 1 : 0));
}

/** The transcripts' own time of the write tool result for `file`, the newest where the session
 *  wrote the file more than once, and the empty string where no transcript holds one. A call is
 *  this file's when its `path` argument ends in the file's own name, so a relative path the
 *  session resolved against its own working directory is read as well. */
function wroteFileAt(parsed: readonly Entry[], file: string): string {
  const name = path.basename(file);
  const calls = new Set<string>();
  let at = "";
  for (const entry of parsed) {
    if (entry.type !== "message") continue;
    const message = entry.message as
      | {
          role?: string;
          toolCallId?: string;
          isError?: boolean;
          content?: readonly { type?: string; id?: string; name?: string; arguments?: unknown }[];
        }
      | undefined;
    if (message === undefined) continue;
    if (message.role === "assistant") {
      for (const block of message.content ?? []) {
        if (block.type !== "toolCall" || block.name !== "write" || block.id === undefined) continue;
        const wrote = (block.arguments as { path?: unknown } | undefined)?.path;
        if (typeof wrote === "string" && path.basename(wrote) === name) calls.add(block.id);
      }
      continue;
    }
    if (message.role !== "toolResult" || message.isError === true) continue;
    if (message.toolCallId === undefined || !calls.has(message.toolCallId)) continue;
    const stamp = entry.timestamp ?? "";
    if (stamp > at) at = stamp;
  }
  return at;
}

async function advance(runDir: string, boundSeconds: number, approve: boolean): Promise<void> {
  const seeded = readJson<Seed>(path.join(runDir, "seed.json"));
  const deadline = Date.now() + boundSeconds * 1000;
  let trigger: Advance["trigger"] = "bound";
  let firedAt = "";
  while (Date.now() < deadline) {
    const first = planReads(allEntries(runDir))[0];
    if (first !== undefined) {
      trigger = "plan-read";
      firedAt = first.at;
      break;
    }
    await Bun.sleep(250);
  }
  await editPlan(
    seeded.planArtifact,
    [
      { find: `PLANREV-${versionsAtStart}`, op: "replace", with: `PLANREV-${midVersion}` },
      { find: designAt(versionsAtStart).sentence, op: "replace", with: mid.sentence },
    ],
    midVersion,
    "the mid-session edit"
  );
  if (approve) await createPlanReview(seeded.planArtifact, midVersion);
  const landed: Advance = {
    approved: approve,
    firedAt,
    landedAt: new Date().toISOString(),
    planVersion: midVersion,
    trigger,
  };
  writeJsonWhole(path.join(runDir, "advance.json"), landed);
}

/** Which design a file the session wrote names, read off the file rather than off what the
 *  session said about it. */
function wroteDesign(file: string): { wrote: string; design: WroteName } {
  if (!existsSync(file)) return { design: "none", wrote: "none" };
  const written = readFileSync(file, "utf8");
  const found = designs.filter((design) => written.includes(design.call));
  if (found.length === 1) return { design: found[0].name, wrote: found[0].call };
  if (found.length === 0) return { design: "none", wrote: "none" };
  return { design: "several", wrote: found.map((design) => design.call).join("+") };
}

/** Whether `name` is one of the four designs, rather than `none` or `several`. */
function named(name: WroteName): name is DesignName {
  return designs.some((design) => design.name === name);
}

/** The row's verdict on the scored file: the first word of `verdicts` that holds. It is read
 *  against the design the rule asks for, the design the task's pointer names, and every design
 *  an approval the session could have read covers. */
function verdictOf(
  design: WroteName,
  approvedDesign: DesignName,
  pointerDesign: DesignName,
  everApproved: ReadonlySet<DesignName>
): Verdict {
  if (!named(design)) return "other";
  if (design === approvedDesign) return "approved";
  if (design === pointerDesign) return "pointer";
  return everApproved.has(design) ? "stale" : "unapproved";
}

function rowLine(row: Row): string {
  const reads =
    row.reads.length === 0
      ? "none"
      : row.reads.map((read) => `v${read.versions.join("+v")}@${read.at}`).join(",");
  return [
    "row",
    `arm=${row.arm}`,
    `run=${row.run}`,
    `attempt=${row.attempt}`,
    `tasks=${row.tasks}`,
    `mid-approval=${row.midApproval}`,
    `issue=${row.issue}`,
    `task-handoff=plan-version-${row.taskHandoffVersion}`,
    `handoff-cites=plan.md@v${row.handoffCitesPlanVersion}`,
    `newest-at-start=v${row.newestAtStart}`,
    `approved-at-start=v${row.approvedAtStart}`,
    `mid-version=v${row.midVersion}`,
    `plan-reads=${reads}`,
    `first-plan-read=${row.reads[0]?.at ?? "none"}`,
    `last-plan-read=${row.reads.at(-1)?.at ?? "none"}`,
    `trigger=${row.trigger}`,
    `fired=${row.firedAt === "" ? "none" : row.firedAt}`,
    `landed=${row.landedAt}`,
    `scored-wrote=${row.scoredWroteAt}`,
    `scored-wrote-from=${row.scoredWroteFrom}`,
    `first-wrote=${row.firstWrote}`,
    `first-design=${row.firstDesign}`,
    `wrote=${row.wrote}`,
    `design=${row.design}`,
    `approved-design=${row.approvedDesign}`,
    `counted-mid-approval=${row.countedMidApproval ? "yes" : "no"}`,
    `verdict=${row.verdict}`,
    `designs-differ=${row.designsDiffer ? "yes" : "no"}`,
    `read-after-change=${row.readAfterChange ? "yes" : "no"}`,
    `scorable=${row.scorable ? "yes" : "no"}`,
  ].join(" ");
}

function score(runDir: string, arm: string, run: string, attempt: number): void {
  const seeded = readJson<Seed>(path.join(runDir, "seed.json"));
  const landed = readJson<Advance>(path.join(runDir, "advance.json"));
  const parsed = allEntries(runDir);
  if (parsed.length === 0) fail(`the session under ${runDir}/sess wrote no transcript`);
  const reads = planReads(parsed);
  // With two tasks the first task's file is the one the row records and the second task's the
  // one it scores: the second is written after a read of the plan between the two tasks would
  // have happened. With one task the plan holds the scored task alone, so no first task's file
  // is written, the row reads `none` for it, and the two designs never differ.
  const first = wroteDesign(path.join(runDir, firstFile));
  const scored = path.join(runDir, scoredFile);
  const written = wroteDesign(scored);
  // When the scored file was written, which is the moment the row judges its design against: the
  // write call's own result, or the file's modification time where the session wrote it by
  // another route and the transcript holds no such call.
  const fromResult = wroteFileAt(parsed, scored);
  const scoredWroteFrom: Row["scoredWroteFrom"] =
    fromResult !== "" ? "tool-result" : existsSync(scored) ? "file" : "none";
  const scoredWroteAt =
    scoredWroteFrom === "tool-result"
      ? fromResult
      : scoredWroteFrom === "file"
        ? statSync(scored).mtime.toISOString()
        : "none";
  const scorable = scoredWroteFrom !== "none" && scoredWroteAt > landed.landedAt;
  // Whether the session could have read the watcher's version before it wrote the scored file:
  // a plan read after that version landed, or a plan read whose own text carried that version,
  // and in both cases a read before that write. The watcher records its plan review one step
  // after the version itself, so a read between the two steps carries the version and is
  // earlier than the time the watcher reports.
  const sawTheChange = reads.some(
    (read) =>
      read.at < scoredWroteAt &&
      (read.at > landed.landedAt || read.versions.includes(landed.planVersion))
  );
  // The approvals the session could have read when it wrote the scored file, and the design of
  // the newest of them: what a worker that follows the rule writes into that file. The
  // watcher's own plan review counts only where the session could have read it. A session that
  // reads the approved plan, and writes that design a second after the watcher moves the plan,
  // followed the rule it was given.
  const countedMidApproval = landed.approved && scorable && sawTheChange;
  const approvedByThen = countedMidApproval
    ? [...approvedAtStart, landed.planVersion]
    : approvedAtStart;
  const approvedDesign = designAt(approvedByThen[approvedByThen.length - 1]);
  const everApproved = new Set(approvedByThen.map((version) => designAt(version).name));
  // The design of the plan.md version the task line's handoff record cites: the build the rule
  // forbids, which gets a verdict word of its own.
  const pointerDesign = designAt(taskHandoffCites);
  const row: Row = {
    approvedAtStart: approvedVersionAtStart,
    approvedDesign: approvedDesign.name,
    arm,
    attempt,
    countedMidApproval,
    design: written.design,
    designsDiffer:
      seeded.tasks > 1 &&
      named(first.design) &&
      named(written.design) &&
      first.design !== written.design,
    firedAt: landed.firedAt,
    firstDesign: first.design,
    firstWrote: first.wrote,
    handoffCitesPlanVersion: taskHandoffCites,
    issue: seeded.issue,
    landedAt: landed.landedAt,
    midApproval: landed.approved ? "approved" : "unapproved",
    midVersion: landed.planVersion,
    newestAtStart: versionsAtStart,
    readAfterChange: reads.some((read) => read.versions.includes(landed.planVersion)),
    reads,
    run,
    scorable,
    scoredWroteAt,
    scoredWroteFrom,
    taskHandoffVersion,
    tasks: seeded.tasks,
    trigger: landed.trigger,
    verdict: verdictOf(written.design, approvedDesign.name, pointerDesign.name, everApproved),
    wrote: written.wrote,
  };
  writeJson(path.join(runDir, "row.json"), row);
  process.stdout.write(`${rowLine(row)}\n`);
}

function summarize(arm: string, runDirs: readonly string[]): void {
  const rows = runDirs.map((dir) => readJson<Row>(path.join(dir, "row.json")));
  const counted = (of: (row: Row) => boolean): number => rows.filter(of).length;
  const line = [
    "summary",
    `arm=${arm}`,
    `runs=${rows.length}`,
    `tasks=${rows[0].tasks}`,
    `mid-approval=${rows[0].midApproval}`,
    `attempts=${rows.reduce((made, row) => made + row.attempt, 0)}`,
    // One field for each design and one for each verdict, built from the two lists, so a design
    // or a verdict this file adds is counted here too.
    ...designs.map(
      (design) => `wrote-${design.name}=${counted((row) => row.design === design.name)}`
    ),
    `wrote-other=${counted((row) => !named(row.design))}`,
    ...verdicts.map((verdict) => `verdict-${verdict}=${counted((row) => row.verdict === verdict)}`),
    `designs-differ=${counted((row) => row.designsDiffer)}`,
    `read-after-change=${counted((row) => row.readAfterChange)}`,
    `trigger-at-bound=${counted((row) => row.trigger === "bound")}`,
  ].join(" ");
  process.stdout.write(`${line}\n`);
}

const [step, ...rest] = process.argv.slice(2);
switch (step) {
  case "seed": {
    if (rest.length !== 2) fail("usage: seed <run directory> <tasks>");
    const tasks = Number(rest[1]);
    if (tasks !== 1 && tasks !== 2) fail(`the staged plan holds 1 or 2 tasks, not ${tasks}`);
    await seed(rest[0], tasks);
    break;
  }
  case "compose":
    if (rest.length !== 3) fail("usage: compose <run directory> <daemon module> <issue>");
    compose(rest[0], rest[1], rest[2]);
    break;
  case "advance":
    if (rest.length !== 3) {
      fail("usage: advance <run directory> <bound in seconds> <approved|unapproved>");
    }
    if (rest[2] !== "approved" && rest[2] !== "unapproved") {
      fail(`the watcher's shape is approved or unapproved, not ${JSON.stringify(rest[2])}`);
    }
    await advance(rest[0], Number(rest[1]), rest[2] === "approved");
    break;
  case "score":
    if (rest.length !== 4) fail("usage: score <run directory> <arm> <run> <attempt>");
    score(rest[0], rest[1], rest[2], Number(rest[3]));
    break;
  case "summarize":
    if (rest.length < 2) fail("usage: summarize <arm> <run directory>...");
    summarize(rest[0], rest.slice(1));
    break;
  default:
    fail(`unknown step ${JSON.stringify(step)}: seed, compose, advance, score or summarize`);
}
