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
 *   seed <run>                  Seeds the staged issue on the run's own Dispatch through
 *                               packages/dispatch/e2e/api.ts, and writes <run>/seed.json.
 *   compose <run> <module>      Composes the implementer's pane prompt from that daemon module
 *                               with workerPane, and writes the whole `--append-system-prompt`
 *                               argument, as shell text, to <run>/arg.txt.
 *   advance <run> <seconds>     The watcher: waits for the session's first read of the plan's
 *                               text in the transcript under <run>/sess, writes the last plan
 *                               version with the newest approval, and writes <run>/advance.json.
 *                               With no such read inside <seconds>, it writes that version at the
 *                               bound and says so.
 *   score <run> <arm> <n> <attempt>
 *                               Reads the transcript and the two files the session wrote, prints
 *                               the run's row, and writes <run>/row.json.
 *   summarize <arm> <run>...    Prints one summary line over those runs' rows.
 *
 * Every plan.md version carries the marker `PLANREV-<version>`, which is how a tool result in the
 * transcript says which version of the plan's text the session read, and how the watcher tells a
 * read of the plan from any other command. A handoff body carries no such marker: it cites its
 * plan version in words, as the planner's record does.
 */
import { existsSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
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

/** One of the three designs the staged plan carries, each differing in the one call the plan
 *  tells the implementer to write into its file, which is what makes the file say which design
 *  the session built from. */
interface Design {
  /** The design's name in a row: which of the three it is. */
  readonly name: string;
  /** The call the implementer writes, and the token the score reads the file for. */
  readonly call: string;
  /** The sentence the plan's design section holds, which an edit replaces whole. */
  readonly sentence: string;
}

/** The design the accepted handoff's plan version holds. */
const early: Design = {
  call: "pulumi.Import",
  name: "early",
  sentence: "The migration takes over the bucket that exists already, with `pulumi.Import`.",
};
/** The design plan.md holds when the session begins. */
const standing: Design = {
  call: "aws.s3.BucketV2",
  name: "standing",
  sentence:
    "The migration leaves that bucket alone and declares a new one, with `aws.s3.BucketV2`.",
};
/** The design the watcher writes while the session runs. */
const mid: Design = {
  call: "pulumi.Alias",
  name: "mid",
  sentence:
    "The migration renames the bucket in place and keeps its old name, with `pulumi.Alias`.",
};

/** The three designs, in the order the plan writes them: this list is what a file the session
 *  wrote is read against and what the summary counts, so a fourth design is never counted as
 *  `wrote-other`. */
const designs: readonly Design[] = [early, standing, mid];

/** The project the staged issue is filed under. The daemon's own token for a claim is this key
 *  in lower case, which is what the pane the shell composes is launched for. */
const projectKey = "DRIVE";
/** How many versions plan.md holds when the session begins. */
const versionsAtStart = 8;
/** The plan.md version each design is written at: `early` at 1, `standing` at 4, `mid` at 9. */
const standingFrom = 4;
/** How many `plan` handoff records the issue holds. */
const handoffRecords = 6;
/** The plan.md version the handoff record the task's line names cites. The newest record cites
 *  `versionsAtStart` instead: the pointer the task carries is the stale one. */
const citedByTaskHandoff = 1;

/** What the shell and the later steps read of the seed. `project` and
 *  `newestHandoffCitesPlanVersion` are the staging's own record for a reader of a past run:
 *  nothing reads them back. */
interface Seed {
  readonly issue: string;
  readonly project: string;
  readonly title: string;
  readonly planArtifact: string;
  readonly versionsAtStart: number;
  readonly taskHandoffVersion: number;
  readonly handoffCitesPlanVersion: number;
  readonly newestHandoffCitesPlanVersion: number;
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
  /** When the watcher read that tool result, or reached its bound. */
  readonly noticedAt: string;
  /** When Dispatch had the new version and its approval. */
  readonly landedAt: string;
  readonly planVersion: number;
  readonly design: string;
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
  readonly taskHandoffVersion: number;
  readonly handoffCitesPlanVersion: number;
  readonly newestAtStart: number;
  readonly approvedAtStart: number;
  readonly reads: readonly PlanRead[];
  readonly trigger: string;
  readonly firedAt: string;
  readonly landedAt: string;
  readonly sessionLastWroteAt: string;
  /** The call the session wrote into the first task's file, `none` when the file holds no
   *  design's call and `several` when it holds more than one. */
  readonly firstWrote: string;
  /** Which design that call is: `early`, `standing`, `mid`, `none` or `several`. */
  readonly firstDesign: string;
  /** The call the session wrote into the second task's file, the one the summary counts, read
   *  the same way. */
  readonly wrote: string;
  /** Which design that call is: `early`, `standing`, `mid`, `none` or `several`. */
  readonly design: string;
  /** Whether the two files name different designs, which is the plan read again between the two
   *  tasks. Two files that name the same design say nothing about a second read. */
  readonly designsDiffer: boolean;
  /** Whether every plan read of the session's is older than the mid-session version's landing. */
  readonly startOnly: boolean;
  /** False when the mid-session version landed after the session stopped writing, which is a run
   *  to make again rather than a pass or a failure. */
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

/** The plan's text at `revision`, carrying `design`. Every version is this text: an edit moves
 *  the marker, and the two design changes replace the one sentence. The plan holds two tasks,
 *  and each one asks for the design's own call in a file of its own: a worker that reads the
 *  plan again before the second task writes the design that stands then, and a worker that reads
 *  it once writes the same design twice. */
function planText(issue: string, revision: number, design: Design): string {
  return `# How ${issue} moves the bucket

Plan revision PLANREV-${revision}.

## The design

${design.sentence}

## Task 1: write the design note

Write one line to the file your task names for this task: \`note: <call>\`.
\`<call>\` is the call the design above names. Write that line and nothing else.

## Task 2: write the adoption line

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

/** The session the seed's handoff records are written under, as a planner's own would be. */
const planner = { id: "plan-pointer-drive-planner", kind: "session" as const };

async function seed(runDir: string): Promise<void> {
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
    content: planText(issue.key, 1, early),
    name: "plan.md",
  });
  const plan = upload.artifact.id;
  if (upload.artifact.slug !== "plan-md") fail(`the plan uploaded as ${upload.artifact.slug}`);
  for (let version = 2; version <= versionsAtStart; version += 1) {
    const ops: EditOp[] = [
      { find: `PLANREV-${version - 1}`, op: "replace", with: `PLANREV-${version}` },
    ];
    if (version === standingFrom) {
      ops.push({ find: early.sentence, op: "replace", with: standing.sentence });
    }
    await editPlan(plan, ops, version, `the edit to plan.md version ${version}`);
  }
  // The newest record cites the newest version, and the record the task's line names cites the
  // first: the two answers the drive is about are both on the issue before the session starts.
  for (let version = 1; version <= handoffRecords; version += 1) {
    const cites = version === handoffRecords ? versionsAtStart : version;
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
  await createPlanReview(plan, citedByTaskHandoff);
  const seeded: Seed = {
    approvedPlanVersionAtStart: citedByTaskHandoff,
    handoffCitesPlanVersion: citedByTaskHandoff,
    issue: issue.key,
    newestHandoffCitesPlanVersion: versionsAtStart,
    planArtifact: plan,
    project: projectKey,
    taskHandoffVersion: citedByTaskHandoff,
    title,
    versionsAtStart,
  };
  writeJson(path.join(runDir, "seed.json"), seeded);
  process.stdout.write(
    `   seeded ${issue.key}: plan.md v${versionsAtStart} (two tasks, ${standing.call} since` +
      ` v${standingFrom}), ${handoffRecords} plan handoffs, the task's v${citedByTaskHandoff}` +
      ` citing plan.md v${citedByTaskHandoff} (${early.call}), approved at v${citedByTaskHandoff}\n`
  );
}

function compose(runDir: string, daemonModule: string): void {
  const pane = workerPane(
    { port: 1, profile: process.env.OMP_PROFILE ?? "default", rig: path.join(runDir, "pane") },
    process.env,
    {
      issue: readJson<Seed>(path.join(runDir, "seed.json")).issue,
      project: projectKey.toLowerCase(),
      role: "implementer",
    },
    { daemonModule, ghPath: process.env.LEGION_GH_PATH, systemPrompt: true }
  );
  const argument = pane.systemPromptArgument ?? "";
  if (argument === "") fail(`the pane composed from ${daemonModule} carries no system prompt`);
  writeFileSync(path.join(runDir, "arg.txt"), argument);
}

/** The session's transcript, the newest `.jsonl` the session directory holds, or undefined while
 *  the session has written none. */
function transcript(runDir: string): string | undefined {
  const dir = path.join(runDir, "sess");
  if (!existsSync(dir)) return undefined;
  const files = readdirSync(dir)
    .filter((name) => name.endsWith(".jsonl"))
    .sort();
  const newest = files.at(-1);
  return newest === undefined ? undefined : path.join(dir, newest);
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

/** Every read of the plan's text in the transcript, oldest first: a tool result whose own text
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
  return reads;
}

/** When the session last wrote to its transcript: the time the run is scorable against, since a
 *  version that lands after it reached no turn of the session's. */
function lastWroteAt(parsed: readonly Entry[]): string {
  let last = "";
  for (const entry of parsed) {
    if (typeof entry.timestamp === "string" && entry.timestamp > last) last = entry.timestamp;
  }
  return last;
}

async function advance(runDir: string, boundSeconds: number): Promise<void> {
  const seeded = readJson<Seed>(path.join(runDir, "seed.json"));
  const version = seeded.versionsAtStart + 1;
  const deadline = Date.now() + boundSeconds * 1000;
  let trigger: Advance["trigger"] = "bound";
  let firedAt = "";
  while (Date.now() < deadline) {
    const file = transcript(runDir);
    const first = file === undefined ? undefined : planReads(entries(file))[0];
    if (first !== undefined) {
      trigger = "plan-read";
      firedAt = first.at;
      break;
    }
    await Bun.sleep(250);
  }
  const noticedAt = new Date().toISOString();
  await editPlan(
    seeded.planArtifact,
    [
      { find: `PLANREV-${seeded.versionsAtStart}`, op: "replace", with: `PLANREV-${version}` },
      { find: standing.sentence, op: "replace", with: mid.sentence },
    ],
    version,
    "the mid-session edit"
  );
  await createPlanReview(seeded.planArtifact, version);
  const landed: Advance = {
    design: mid.name,
    firedAt,
    landedAt: new Date().toISOString(),
    noticedAt,
    planVersion: version,
    trigger,
  };
  writeJson(path.join(runDir, "advance.json"), landed);
}

/** Which design a file the session wrote names, read off the file rather than off what the
 *  session said about it. */
function wroteDesign(file: string): { wrote: string; design: string } {
  if (!existsSync(file)) return { design: "none", wrote: "none" };
  const written = readFileSync(file, "utf8");
  const found = designs.filter((design) => written.includes(design.call));
  if (found.length === 1) return { design: found[0].name, wrote: found[0].call };
  if (found.length === 0) return { design: "none", wrote: "none" };
  return { design: "several", wrote: found.map((design) => design.call).join("+") };
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
    `issue=${row.issue}`,
    `task-handoff=plan-version-${row.taskHandoffVersion}`,
    `handoff-cites=plan.md@v${row.handoffCitesPlanVersion}`,
    `newest-at-start=v${row.newestAtStart}`,
    `approved-at-start=v${row.approvedAtStart}`,
    `plan-reads=${reads}`,
    `first-plan-read=${row.reads[0]?.at ?? "none"}`,
    `last-plan-read=${row.reads.at(-1)?.at ?? "none"}`,
    `trigger=${row.trigger}`,
    `fired=${row.firedAt === "" ? "none" : row.firedAt}`,
    `landed=${row.landedAt}`,
    `session-last-wrote=${row.sessionLastWroteAt}`,
    `first-wrote=${row.firstWrote}`,
    `first-design=${row.firstDesign}`,
    `wrote=${row.wrote}`,
    `design=${row.design}`,
    `designs-differ=${row.designsDiffer ? "yes" : "no"}`,
    `start-only-read=${row.startOnly ? "yes" : "no"}`,
    `scorable=${row.scorable ? "yes" : "no"}`,
  ].join(" ");
}

function score(runDir: string, arm: string, run: string, attempt: number): void {
  const seeded = readJson<Seed>(path.join(runDir, "seed.json"));
  const landed = readJson<Advance>(path.join(runDir, "advance.json"));
  const file = transcript(runDir);
  if (file === undefined) fail(`the session under ${runDir}/sess wrote no transcript`);
  const parsed = entries(file);
  const reads = planReads(parsed);
  const sessionLastWroteAt = lastWroteAt(parsed);
  // The first task's file is the one the row records, the second task's the one it scores: the
  // second is written after a read of the plan between the two tasks would have happened.
  const first = wroteDesign(path.join(runDir, "note.txt"));
  const written = wroteDesign(path.join(runDir, "adoption.txt"));
  const row: Row = {
    approvedAtStart: seeded.approvedPlanVersionAtStart,
    arm,
    attempt,
    design: written.design,
    designsDiffer: first.design !== written.design,
    firedAt: landed.firedAt,
    firstDesign: first.design,
    firstWrote: first.wrote,
    handoffCitesPlanVersion: seeded.handoffCitesPlanVersion,
    issue: seeded.issue,
    landedAt: landed.landedAt,
    newestAtStart: seeded.versionsAtStart,
    reads,
    run,
    scorable: sessionLastWroteAt > landed.landedAt,
    sessionLastWroteAt,
    startOnly: reads.length > 0 && reads.every((read) => read.at < landed.landedAt),
    taskHandoffVersion: seeded.taskHandoffVersion,
    trigger: landed.trigger,
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
    `attempts=${rows.reduce((made, row) => made + row.attempt, 0)}`,
    `wrote-mid=${counted((row) => row.design === mid.name)}`,
    `wrote-standing=${counted((row) => row.design === standing.name)}`,
    `wrote-early=${counted((row) => row.design === early.name)}`,
    `wrote-other=${counted((row) => !designs.some((design) => design.name === row.design))}`,
    `designs-differ=${counted((row) => row.designsDiffer)}`,
    `start-only-read=${counted((row) => row.startOnly)}`,
    `read-after-the-change=${counted((row) => row.reads.some((read) => read.at > row.landedAt))}`,
    `trigger-at-bound=${counted((row) => row.trigger === "bound")}`,
  ].join(" ");
  process.stdout.write(`${line}\n`);
}

const [step, ...rest] = process.argv.slice(2);
switch (step) {
  case "seed":
    if (rest.length !== 1) fail("usage: seed <run directory>");
    await seed(rest[0]);
    break;
  case "compose":
    if (rest.length !== 2) fail("usage: compose <run directory> <daemon module>");
    compose(rest[0], rest[1]);
    break;
  case "advance":
    if (rest.length !== 2) fail("usage: advance <run directory> <bound in seconds>");
    await advance(rest[0], Number(rest[1]));
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
