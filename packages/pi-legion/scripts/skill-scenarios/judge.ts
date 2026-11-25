#!/usr/bin/env bun
// Runs the judges of the spec-writing and spec-rewrite scenarios, and of the worker scenarios
// reviewer-text-findings and threads-closed (judge-prompts.ts). Each judge is a fresh `omp -p` that
// rig.sh judge-omp starts in a directory of its own, <work>/judging/<run>/<prompt>[-<n>]/, which
// holds only that prompt's inputs and prompt.txt. The judge writes its answer as JSON into that
// directory. judge.ts validates it and copies it into the run directory. A judge that writes no
// answer, or one that does not parse, fails the command, naming the file.
//
//   judge.ts answer <run dir> <turn>
//     The person stand-in: one `answer` judge for each open ask in state-<turn>.json, given the
//     source's messages.md and asks.json. Writes answers-<turn>.jsonl, one line per ask
//     ({"ask","selected","text"}), and an empty file when no ask is open.
//   judge.ts run <run dir>...
//     For a spec run, the nine judges of each run (faults, decisions, findings, checkable,
//     questions, numbers, preamble, proposal, and shared-runs once for each shared run that
//     score.ts's sharedRuns lists). Writes faults.json, decisions.json, findings.json,
//     checkable.json, questions.json, numbers.json, preamble.json, proposal.json and
//     shared-runs.json into each run directory. For a run of a worker scenario (score.ts's
//     JUDGED_SCENARIOS), the two judges of Verification v47's rule 8 (`judgeWorker`): accepts and
//     names. Writes accepts.json and names.json into each run directory.
//
// <work> is the directory that holds the run's `runs/` directory, or the `archive/runs/` rig.sh
// moves a run to once it has ended, as score.ts reads it (`workOfRuns`). Seeded asks
// (seeded.json) never reach a judge's inputs.
//
// judge.ts removes and replaces nothing an earlier judging left. Before any judge starts, it
// refuses, naming each, a judge directory under <work>/judging/<run>/ or an answer file in the run
// directory (answers-<turn>.jsonl, or the nine files of `run`) that is already there: a run is
// judged again once those are moved aside.
import { existsSync, lstatSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { z } from "zod";
import { scenarioOf } from "./completion-answers";
import {
  AcceptsOutput,
  acceptsPrompt,
  answerPrompt,
  CheckableFile,
  checkablePrompt,
  decisionsPrompt,
  FAULTS_MD,
  FindingsOutput,
  faultsPrompt,
  findingsPrompt,
  type JudgeProfile,
  type JudgePrompt,
  NamesOutput,
  namesPrompt,
  numbersPrompt,
  preamblePrompt,
  proposalPrompt,
  questionsPrompt,
  SHARED_RUNS_RULE,
  SharedRunsFile,
  sharedRunPrompt,
} from "./judge-prompts";
import { Refused } from "./run-records";
import {
  approval,
  checkouts,
  coverage,
  JUDGED_SCENARIOS,
  judgeInputs,
  runsOf,
  SpecFixture,
  State,
  seededIds,
  sessionVersions,
  sharedRuns,
  specWork,
  workerCommit,
  workerCoverage,
  workerGivens,
  workerTexts,
  workOfRuns,
} from "./score";
import { session, sessionId } from "./transcript";

/** Starts one judge in `dir` under `profile` and resolves to its exit status. `output` is the name
 * of the one file in `dir` the judge writes its answer to, the only file of `dir` it may write. */
export type JudgeRunner = (profile: JudgeProfile, dir: string, output: string) => Promise<number>;
export interface JudgeOptions {
  runner?: JudgeRunner;
  /** How many judges of a run run at once. */
  parallel?: number;
}

const here = import.meta.dir;
/** The production runner: `bash rig.sh judge-omp <profile> <dir> <output>`, with this process's
 * environment, so the rig's work directory and the operator's model route reach it. */
const rigRunner: JudgeRunner = async (profile, dir, output) => {
  const child = Bun.spawn(["bash", path.join(here, "rig.sh"), "judge-omp", profile, dir, output], {
    stdout: "inherit",
    stderr: "inherit",
  });
  return await child.exited;
};

/** The work directory of a run under <work>/runs/<run> or <work>/archive/runs/<run>. */
function workOf(runDir: string): string {
  const runs = path.dirname(path.resolve(runDir));
  if (path.basename(runs) !== "runs")
    throw new Error(`${runDir} is not under a runs/ directory of the rig's work directory`);
  return workOfRuns(runs);
}
function read<T>(file: string, schema: z.ZodType<T>): T {
  return schema.parse(JSON.parse(readFileSync(file, "utf8")));
}

/** One judge to run: its directory's name, its prompt, its input files (relative path to content),
 * and what its answer must cover besides its shape (score.ts `coverage`: the problems, empty when
 * it covers its input). */
interface Job<T> {
  dir: string;
  prompt: JudgePrompt<T>;
  inputs: Record<string, string>;
  covers?: (answer: T) => string[];
}
/** Refuses, naming each, the paths of `paths` that are there: what an earlier judging left, which
 * judge.ts never removes or replaces. Called with every path a judging makes before any judge
 * starts, so a refused judging leaves nothing behind. */
function refuseTaken(paths: readonly string[]): void {
  const taken = paths.filter((one) => lstatSync(one, { throwIfNoEntry: false }) !== undefined);
  if (taken.length > 0)
    throw new Error(
      `judge.ts: ${taken.join(", ")} from an earlier judging; judge.ts removes and replaces none of it: move it aside to judge again`
    );
}
/** Makes the job's directory, which refuseTaken found absent, with only its inputs and
 * prompt.txt, runs the judge in it, and returns its validated answer. An answer that does not
 * parse, or does not cover what the judge was given, fails the run, naming the answer's file. */
async function runJob<T>(job: Job<T>, runner: JudgeRunner): Promise<T> {
  mkdirSync(path.dirname(job.dir), { recursive: true });
  mkdirSync(job.dir);
  for (const [file, text] of Object.entries(job.inputs)) {
    mkdirSync(path.dirname(path.join(job.dir, file)), { recursive: true });
    writeFileSync(path.join(job.dir, file), text);
  }
  writeFileSync(path.join(job.dir, "prompt.txt"), job.prompt.text(job.dir));
  const status = await runner(job.prompt.profile, job.dir, job.prompt.file);
  const answer = path.join(job.dir, job.prompt.file);
  if (!existsSync(answer))
    throw new Error(`${answer} is missing: the judge wrote no answer (exit ${status})`);
  let parsed: unknown;
  try {
    parsed = JSON.parse(readFileSync(answer, "utf8"));
  } catch (error) {
    throw new Error(`${answer} does not parse: ${error instanceof Error ? error.message : error}`);
  }
  const result = job.prompt.schema.safeParse(parsed);
  if (!result.success)
    throw new Error(`${answer} does not parse: ${z.prettifyError(result.error)}`);
  const problems = job.covers?.(result.data) ?? [];
  if (problems.length > 0)
    throw new Error(
      `${answer} does not cover what judge.ts gave its judge: ${problems.join("; ")}`
    );
  return result.data;
}
/** Runs every job, at most `parallel` at once, and resolves to their answers in job order. */
async function runAll<T>(jobs: Job<T>[], runner: JudgeRunner, parallel: number): Promise<T[]> {
  const answers: T[] = new Array(jobs.length);
  let next = 0;
  const lanes = Array.from({ length: Math.min(parallel, jobs.length) }, async () => {
    while (next < jobs.length) {
      const index = next++;
      const job = jobs[index];
      if (job !== undefined) answers[index] = await runJob(job, runner);
    }
  });
  await Promise.all(lanes);
  return answers;
}

/** The source's input files a judge reads: messages.md and asks.json. */
function sourceInputs(runDir: string): Record<string, string> {
  return {
    "messages.md": readFileSync(path.join(runDir, "source", "messages.md"), "utf8"),
    "asks.json": readFileSync(path.join(runDir, "source", "asks.json"), "utf8"),
  };
}
const pretty = (value: unknown) => `${JSON.stringify(value, null, 2)}\n`;

/** `judge.ts answer`: the stand-in answers each open ask of the turn, one judge per ask. */
export async function answerTurn(runDir: string, turn: number, options: JudgeOptions = {}) {
  const runner = options.runner ?? rigRunner;
  const run = path.basename(path.resolve(runDir));
  const judging = path.join(workOf(runDir), "judging", run);
  const seeded = seededIds(runDir);
  const open = read(path.join(runDir, `state-${turn}.json`), State).open_asks.filter(
    (ask) => !seeded.has(ask.id)
  );
  const jobs = open.map((ask, index) => {
    const labels = (ask.options ?? []).map((option) => option.label);
    // The stand-in's label must be one the ask offers, and one label unless the ask takes several.
    const schema = answerPrompt.schema.superRefine((value, ctx) => {
      for (const label of value.selected)
        if (!labels.includes(label))
          ctx.addIssue({ code: "custom", message: `"${label}" is not a label of ask ${ask.id}` });
      if (labels.length > 0 && value.selected.length === 0)
        ctx.addIssue({ code: "custom", message: `no label of ask ${ask.id} is selected` });
      if (ask.multiple !== true && value.selected.length > 1)
        ctx.addIssue({ code: "custom", message: `ask ${ask.id} takes one label` });
    });
    return {
      dir: path.join(judging, `answer-${turn}-${index + 1}`),
      prompt: { ...answerPrompt, schema },
      inputs: {
        ...sourceInputs(runDir),
        "ask.json": pretty({
          question: ask.question,
          options: ask.options ?? [],
          multiple: ask.multiple ?? false,
        }),
      },
    };
  });
  const out = path.join(runDir, `answers-${turn}.jsonl`);
  refuseTaken([out, ...jobs.map((job) => job.dir)]);
  const answers = await runAll(jobs, runner, options.parallel ?? 5);
  writeFileSync(
    out,
    answers
      .map((answer, index) =>
        JSON.stringify({ ask: open[index]?.id, selected: answer.selected, text: answer.text })
      )
      .map((line) => `${line}\n`)
      .join(""),
    { flag: "wx" }
  );
}

/** The spec version each spec-only check led to: the first version the run's session wrote at or
 * after the write that followed the check (an unseen version's write is at its own time, so it
 * leads to itself). */
function specAfter(runDir: string, writeAt: string, own: string): string {
  const version = sessionVersions(runDir, own).find(
    (one) => Date.parse(one.created_at) >= Date.parse(writeAt)
  );
  if (version === undefined)
    throw new Error(`spec-versions.json holds no version the session wrote at or after ${writeAt}`);
  return readFileSync(path.join(runDir, "spec-versions", `${version.version}.md`), "utf8");
}

/** `judge.ts run`: the nine judges of one spec run, or, for a run of a worker scenario (score.ts's
 * JUDGED_SCENARIOS, by the scenario its record or its directory names), the two of `judgeWorker`. */
export async function judgeRun(runDir: string, options: JudgeOptions = {}) {
  const scenario = scenarioOf(runDir);
  if (scenario !== undefined && JUDGED_SCENARIOS.includes(scenario))
    return await judgeWorker(runDir, scenario, options);
  const runner = options.runner ?? rigRunner;
  const parallel = options.parallel ?? 5;
  const work = workOf(runDir);
  const run = path.basename(path.resolve(runDir));
  const judging = path.join(work, "judging", run);
  const file = (name: string) => readFileSync(path.join(runDir, name), "utf8");
  const spec = file("spec.md");
  const entries = session(runDir);
  const own = sessionId(entries);
  if (own === undefined) throw new Error(`${runDir}: the transcript has no session header`);
  const fixture = read(path.join(runDir, "fixture.json"), SpecFixture);
  // Every checkout a spec run's agent can reach, as the runs beside it recorded them (score.ts's
  // `checkouts`), never the work directory's profiles as they stand now.
  const reach = checkouts([...runsOf(path.dirname(path.resolve(runDir))).values()]);
  const specWorkDone = specWork(runDir, reach, fixture);
  const given = judgeInputs(runDir, specWorkDone);
  const documentFiles = Object.fromEntries(given.documents.map((name) => [name, file(name)]));
  const { checks, writes } = specWorkDone;
  const findingsInputs = Object.fromEntries(
    checks.flatMap((check, index) => [
      [`check-${index + 1}/assignment.md`, `${check.assignment}\n`],
      [`check-${index + 1}/report.md`, `${check.report}\n`],
      [`check-${index + 1}/spec-after.md`, specAfter(runDir, writes[check.write]?.at ?? "", own)],
    ])
  );
  const { approved, version } = approval(runDir);
  const approvedSpec =
    approved && version !== null ? file(path.join("spec-versions", `${version}.md`)) : undefined;

  // A judge with nothing to judge runs no session: no check, no findings judge; no approval, no
  // checkable judge. Its file is written with no row.
  const single: Job<unknown>[] = [
    { dir: "faults", prompt: faultsPrompt, inputs: { "spec.md": spec, "faults.md": FAULTS_MD } },
    { dir: "decisions", prompt: decisionsPrompt, inputs: { "spec.md": spec } },
    ...(checks.length === 0
      ? []
      : [{ dir: "findings", prompt: findingsPrompt, inputs: findingsInputs }]),
    ...(approvedSpec === undefined
      ? []
      : [{ dir: "checkable", prompt: checkablePrompt, inputs: { "spec.md": approvedSpec } }]),
    {
      dir: "questions",
      prompt: questionsPrompt,
      inputs: { "agent-asks.json": pretty(given.asks), ...sourceInputs(runDir) },
    },
    { dir: "numbers", prompt: numbersPrompt, inputs: { "agent-texts.json": pretty(given.texts) } },
    {
      dir: "preamble",
      prompt: preamblePrompt,
      inputs: {
        "spec.md": spec,
        "agent-texts.json": pretty(given.texts),
        ...documentFiles,
        "messages.md": file(path.join("source", "messages.md")),
      },
    },
    {
      dir: "proposal",
      prompt: proposalPrompt,
      inputs: { "spec.md": spec, "agent-asks.json": pretty(given.asks) },
    },
  ].map(
    (job) =>
      ({
        ...job,
        dir: path.join(judging, job.dir),
        covers: (answer: unknown) => coverage(job.prompt.file, answer, given),
      }) as Job<unknown>
  );
  const runs = sharedRuns(runDir);
  const shared = runs.map((found, index) => ({
    dir: path.join(judging, `shared-runs-${index + 1}`),
    prompt: sharedRunPrompt,
    inputs: {
      "run.txt": `${found.run}\n`,
      "context.md": `# The agent's text (${found.source})\n\n${found.text}\n\n# The person's input\n\n${found.input}\n`,
      "rule.md": SHARED_RUNS_RULE,
    },
  }));
  // Every answer file this judging writes into the run directory, and every judge directory.
  const outputs = [
    ...new Set([
      ...single.map((job) => job.prompt.file),
      findingsPrompt.file,
      checkablePrompt.file,
      "shared-runs.json",
    ]),
  ].map((name) => path.join(runDir, name));
  refuseTaken([...outputs, ...single.map((job) => job.dir), ...shared.map((job) => job.dir)]);
  const [singleAnswers, sharedAnswers] = await Promise.all([
    runAll(single, runner, parallel),
    runAll(shared, runner, parallel),
  ]);
  const write = (name: string, text: string) =>
    writeFileSync(path.join(runDir, name), text, { flag: "wx" });
  for (const [index, job] of single.entries()) {
    const answer = singleAnswers[index];
    if (job.prompt === checkablePrompt)
      write(job.prompt.file, pretty(CheckableFile.parse({ ...(answer as object), version })));
    else write(job.prompt.file, readFileSync(path.join(job.dir, job.prompt.file), "utf8"));
  }
  if (checks.length === 0) write(findingsPrompt.file, pretty(FindingsOutput.parse({ rows: [] })));
  if (approvedSpec === undefined)
    write(checkablePrompt.file, pretty(CheckableFile.parse({ rows: [], version: null })));
  write(
    "shared-runs.json",
    pretty(
      SharedRunsFile.parse({
        rows: runs.map((found, index) => ({
          source: found.source,
          run: found.run,
          verdict: sharedAnswers[index]?.verdict,
          reason: sharedAnswers[index]?.reason,
        })),
      })
    )
  );
}

/** `judge.ts run` for a run of a worker scenario (`scenario`, one of JUDGED_SCENARIOS): the
 * judges Verification v47's rule 8 has decide from the text what the agents of the run wrote
 * (score.ts's `workerTexts`): the accepts judge, whether each text accepts the commit
 * `workerCommit` names, and the names judge, whether each text names each thing `workerGivens`
 * lists. A commit named by a SHA prefix of seven characters or more is the scorer's to decide,
 * never a judge's. Writes accepts.json and names.json into the run directory, the answer of each
 * judge as it wrote it, and a file with no row for a judge that had nothing to read, which runs no
 * session: a run whose agents wrote no text, or (accepts) a scenario with no commit to accept. */
export async function judgeWorker(runDir: string, scenario: string, options: JudgeOptions = {}) {
  const runner = options.runner ?? rigRunner;
  const judging = path.join(workOf(runDir), "judging", path.basename(path.resolve(runDir)));
  const texts = workerTexts(runDir);
  const givens = workerGivens(runDir);
  const commit = workerCommit(runDir, scenario);
  const covers = (file: string) => (answer: unknown) =>
    workerCoverage(file, answer, texts, givens, commit);
  const jobs = [
    ...(texts.length === 0 || commit === undefined
      ? []
      : [
          {
            dir: path.join(judging, acceptsPrompt.name),
            prompt: acceptsPrompt,
            inputs: { "texts.json": pretty(texts), "commit.txt": `${commit}\n` },
            covers: covers(acceptsPrompt.file),
          },
        ]),
    ...(texts.length === 0
      ? []
      : [
          {
            dir: path.join(judging, namesPrompt.name),
            prompt: namesPrompt,
            inputs: { "texts.json": pretty(texts), "given.json": pretty(givens) },
            covers: covers(namesPrompt.file),
          },
        ]),
  ] as Job<unknown>[];
  const files = [acceptsPrompt.file, namesPrompt.file];
  refuseTaken([...files.map((name) => path.join(runDir, name)), ...jobs.map((job) => job.dir)]);
  await runAll(jobs, runner, options.parallel ?? 5);
  const empty = { [acceptsPrompt.file]: AcceptsOutput, [namesPrompt.file]: NamesOutput };
  for (const name of files) {
    const job = jobs.find((one) => one.prompt.file === name);
    const text =
      job === undefined
        ? pretty(empty[name]?.parse({ rows: [] }))
        : readFileSync(path.join(job.dir, name), "utf8");
    writeFileSync(path.join(runDir, name), text, { flag: "wx" });
  }
}

if (import.meta.main) {
  const [command, ...rest] = Bun.argv.slice(2);
  const parallel = Number(process.env.SKILL_SCENARIOS_PARALLEL ?? 5) || 5;
  if (command === "answer" && rest.length === 2 && /^\d+$/.test(rest[1] ?? "")) {
    await answerTurn(rest[0] ?? "", Number(rest[1]), { parallel });
  } else if (command === "run" && rest.length >= 1) {
    for (const runDir of rest) {
      try {
        await judgeRun(runDir, { parallel });
      } catch (error) {
        // A refusal (an entry beside the run that is no run, an input the run did not record) is
        // no judgment: it names the cause and exits 2, as `rig.sh health` and `rig.sh score` do.
        if (!(error instanceof Refused)) throw error;
        console.error(`judge: ${error.message}`);
        process.exit(2);
      }
      console.log(`${path.basename(path.resolve(runDir))}: judged`);
    }
  } else {
    console.error("usage: judge.ts answer <run dir> <turn> | run <run dir>...");
    process.exit(2);
  }
}
