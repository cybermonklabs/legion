/**
 * The scoring of the scored scenarios (owner-walk): each run's clauses, its reach, the judge's
 * input, and the cells a scenario's runs make. score.ts runs it:
 *
 *   score.ts runs <runs dir> --run <name>   unscored's rules, then ownerWalk, then row.json
 *   score.ts cells <runs dir> <scenario>    cells, one CellLine of JSON a line
 *   score.ts cell <runs dir> <scenario> <stage> <label> <case> <count>
 *                                           caseCell, the line of one case's cell (`rig.sh cell`)
 *
 * What ownerWalk(dir, run, label) reads in the run's directory `dir`:
 *   world.json            the world (OwnerWalkWorld, owner-walk.ts world)
 *   captured.json         the world's Dispatch records after the run (Captured of
 *                         scored-runs-contracts.ts, seed.ts capture-scored), with the seed's fixture
 *   dispatch-proxy.jsonl  each request of the run's agents to its Dispatch (dispatch-proxy.ts)
 *   plans-answer.json, plans-answer.source
 *                         the recorded plans answer the proxy served, and its source; rig.sh puts
 *                         both in the run's directory
 *   sessions/**.jsonl     the transcripts of the run's session and of its subagents
 *   out.txt, held.txt     the run's end: `exit=bound` as out.txt's last line fails the run, with the
 *                         reason rig.sh writes to held.txt
 *   judgment.json         the acceptance worker's verdicts on the judged items, when it wrote them
 * and what it writes there:
 *   score.json            the run's clauses, what is reported beside them, and its reach (Score)
 *   judge-input.md        the texts the run's judged items need, naming no label and no form;
 *                         a run with no judged item gets none
 * cells(runsDir, scenario) reads row.json and world.json of each run directory of the scenario, and
 * caseCell the runs of one case, or for a control form every run of the form, under one label.
 */
import { existsSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import type { z } from "zod";
import {
  caseName,
  filledGuidance,
  type OwnerWalkCase,
  type OwnerWalkClause,
  type OwnerWalkEvidence,
  type OwnerWalkStage,
  ownerWalkRun,
  parseOwnerWalkCase,
  planWrites,
  stageOf,
} from "./owner-walk";
import { parseInstant } from "./production-dispatch";
import {
  Captured,
  CellLine,
  DispatchProxyLine,
  FAMILIES,
  type HeldPlansAnswer,
  Judgment,
  mapRecordedKeys,
  OWNER_WALK_KINDS,
  OwnerWalkFixture,
  OwnerWalkWorld,
  PlansAnswerSource,
  REACH_HOSTS,
  Reach,
  type Reported,
  RunRow,
  readPlansAnswer,
  Score,
  type ScoreClause,
  type ScoredRow,
  wholeKeys,
} from "./scored-runs-contracts";
import { SessionEntry, session, sessionId } from "./transcript";

/** The scenarios whose runs score.ts scores through this module. */
export const SCORED_SCENARIOS: readonly string[] = ["owner-walk"];

function readJson(dir: string, file: string): unknown {
  const full = path.join(dir, file);
  if (!existsSync(full)) throw new Error(`the run holds no ${file}`);
  return JSON.parse(readFileSync(full, "utf8"));
}
/** Each line of a JSONL file of the run, parsed as `schema`; a line that does not parse is the
 * rig's fault, named by its file and line. */
function jsonLines<T>(dir: string, file: string, schema: z.ZodType<T>): T[] {
  const full = path.join(dir, file);
  if (!existsSync(full)) throw new Error(`the run holds no ${file}`);
  return readFileSync(full, "utf8")
    .split("\n")
    .flatMap((line, index) => {
      if (line.trim() === "") return [];
      const parsed = schema.safeParse(JSON.parse(line));
      if (!parsed.success) throw new Error(`${file}:${index + 1}: ${parsed.error.message}`);
      return [parsed.data];
    });
}
/** Every string in a value, depth first. */
function stringsOf(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(stringsOf);
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(stringsOf);
  return [];
}
/** "a", "a and b", "a, b and c". */
function listed(items: readonly string[]): string {
  return items.length <= 1
    ? (items[0] ?? "")
    : `${items.slice(0, -1).join(", ")} and ${items.at(-1)}`;
}

// The transcripts.

/** Every transcript under the run's sessions/, the run's own and each subagent's at any depth, in
 * the order of their paths. */
function transcriptFiles(dir: string): string[] {
  const walk = (at: string): string[] =>
    existsSync(at)
      ? readdirSync(at, { withFileTypes: true }).flatMap((entry) =>
          entry.isDirectory()
            ? walk(path.join(at, entry.name))
            : entry.name.endsWith(".jsonl")
              ? [path.join(at, entry.name)]
              : []
        )
      : [];
  return walk(path.join(dir, "sessions")).sort();
}
function entriesOf(file: string): SessionEntry[] {
  return readFileSync(file, "utf8")
    .split("\n")
    .flatMap((line, index) => {
      if (line.trim() === "") return [];
      const parsed = SessionEntry.safeParse(JSON.parse(line));
      if (!parsed.success) throw new Error(`${file}:${index + 1} is not a record`);
      return [parsed.data];
    });
}

/** A host of REACH_HOSTS as a tool argument names it: the whole name, in any case. A name inside a
 * longer one (`mystorage.googleapis.com`) is another host; a subdomain of the bucket's API
 * (`<bucket>.storage.googleapis.com`) names it. */
const HOST_NAMES: Record<(typeof REACH_HOSTS)[number], RegExp> = {
  "metadata.google.internal": /(?<![A-Za-z0-9-])metadata\.google\.internal(?![A-Za-z0-9-])/i,
  "169.254.169.254": /(?<![0-9.])169\.254\.169\.254(?![0-9])/,
  "storage.googleapis.com": /(?<![A-Za-z0-9-])storage\.googleapis\.com(?![A-Za-z0-9-])/i,
};
/**
 * The run's reach: each tool call of its agent and its subagents (every transcript under sessions/)
 * whose arguments name a host of REACH_HOSTS, once for each host it names. `session` is the
 * transcript's session id (its header's; the transcript's path under sessions/ when it has no
 * header), `message` the call's message's index among that transcript's `message` entries, from 0,
 * and `tool` the tool's name. An assistant's text, its thinking and a tool's result are no tool
 * argument, so a host they name is no reach. A host a call reaches without naming it (through a
 * variable, an encoding or a file) is not seen. The reach changes no verdict and leaves no run
 * out: it is a record for the reader of the scores. `run` names the run in an error.
 */
export function reachOf(dir: string, run: string): Reach[] {
  const reach: Reach[] = [];
  for (const file of transcriptFiles(dir)) {
    let entries: SessionEntry[];
    try {
      entries = entriesOf(file);
    } catch (error) {
      throw new Error(`${run}: ${error instanceof Error ? error.message : String(error)}`);
    }
    const id = sessionId(entries) ?? path.relative(path.join(dir, "sessions"), file);
    let message = -1;
    for (const entry of entries) {
      if (entry.type !== "message") continue;
      message += 1;
      if (entry.message?.role !== "assistant") continue;
      for (const part of entry.message.content ?? []) {
        if (part.type !== "toolCall") continue;
        const named = stringsOf(part.arguments);
        for (const host of REACH_HOSTS)
          if (named.some((text) => HOST_NAMES[host].test(text)))
            reach.push(Reach.parse({ session: id, message, tool: part.name ?? "tool", host }));
      }
    }
  }
  return reach;
}

/** Each model the transcript names, as `<provider>/<model>`, in the order it first names it: each
 * `model_change` entry's model, and each assistant message's provider and model. */
export function transcriptModels(entries: SessionEntry[]): string[] {
  const models = new Set<string>();
  for (const entry of entries) {
    if (entry.type === "model_change" && typeof entry.model === "string" && entry.model !== "")
      models.add(entry.model);
    const message = entry.message;
    if (
      message?.role === "assistant" &&
      typeof message.provider === "string" &&
      typeof message.model === "string" &&
      message.provider !== "" &&
      message.model !== ""
    )
      models.add(`${message.provider}/${message.model}`);
  }
  return [...models];
}

/** Why a scored run's model does not count, or undefined when it does: its label's `model` line in
 * the built-from rig.sh copied into the run (`model <provider>/<model>`) must be the one model its
 * transcript names. A label with no such line is no model the run can be held to. */
export function modelProblem(dir: string, entries: SessionEntry[]): string | undefined {
  const file = path.join(dir, "built-from");
  if (!existsSync(file)) return "it has no built-from, so its label's model line cannot be read";
  const lines = readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.startsWith("model "));
  if (lines.length === 0)
    return "its label's built-from names no model line, so the run's model cannot be checked";
  if (lines.length > 1)
    return `its label's built-from names ${lines.length} model lines, so the run's model cannot be checked: ${listed(lines)}`;
  const want = (lines[0] ?? "").slice("model ".length).trim();
  const models = transcriptModels(entries);
  if (models.length === 0)
    return `its transcript names no model, so it cannot be held to its label's ${want}`;
  if (models.length > 1)
    return `its transcript names ${models.length} models, ${listed(models)}: a scored run runs on its label's model alone`;
  if (models[0] !== want)
    return `its transcript names the model ${models[0]}, and its label's built-from names ${want}`;
  return undefined;
}

// owner-walk.

/** The clause a run cut at its bound fails: the part's rule beside the check's sentences. */
export const BOUND_CLAUSE = "the run ended within its bound";

/** The text of a message's text parts. */
const textOf = (entry: SessionEntry) =>
  (entry.message?.content ?? [])
    .filter((part) => part.type === "text")
    .map((part) => part.text ?? "")
    .join("\n");
/** The words of a text, lowercased: its runs of letters and digits. */
const wordsOf = (text: string) => text.toLowerCase().match(/[\p{L}\p{N}]+/gu) ?? [];
/** The first run of `n` words in a row that `text` and `source` both hold, or undefined. */
function sharedWords(text: string, source: string, n: number): string | undefined {
  const runs = new Set<string>();
  const words = wordsOf(source);
  for (let at = 0; at + n <= words.length; at += 1) runs.add(words.slice(at, at + n).join(" "));
  const mine = wordsOf(text);
  for (let at = 0; at + n <= mine.length; at += 1) {
    const run = mine.slice(at, at + n).join(" ");
    if (runs.has(run)) return run;
  }
  return undefined;
}
/** A block of text in a fence longer than any run of backticks in it. */
function fenced(text: string, language = ""): string {
  const longest = Math.max(0, ...(text.match(/`+/g) ?? []).map((run) => run.length));
  const fence = "`".repeat(Math.max(3, longest + 1));
  return `${fence}${language}\n${text.replace(/\n$/, "")}\n${fence}`;
}

/** A tool call that reads earlier plans: the `dispatch plans` command, the plans route, or the
 * Dispatch tool of that name. */
const PLANS_READ =
  /\bdispatch\s+plans\b|\/api\/v1\/projects\/[^/\s"'`]+\/plans\b|\bdispatch_plans\b/;
/** Each read of earlier plans the run's agents made, with what it printed: in every transcript,
 * each tool call whose arguments PLANS_READ matches, and the text of its result. */
function plansReads(dir: string): { call: string; printed: string }[] {
  return transcriptFiles(dir).flatMap((file) => {
    const entries = entriesOf(file);
    const results = new Map<string, string>();
    for (const entry of entries)
      if (entry.message?.role === "toolResult" && entry.message.toolCallId !== undefined)
        results.set(entry.message.toolCallId, textOf(entry));
    return entries.flatMap((entry) =>
      entry.message?.role !== "assistant"
        ? []
        : (entry.message.content ?? []).flatMap((part) => {
            if (part.type !== "toolCall") return [];
            const call = stringsOf(part.arguments).find((text) => PLANS_READ.test(text));
            if (call === undefined) return [];
            return [
              {
                call: call.split("\n")[0]?.slice(0, 200) ?? "",
                printed: results.get(part.id ?? "") ?? "(no result)",
              },
            ];
          })
    );
  });
}

/** The texts of a run its judged clauses need, as judge-input.md: what each item needs, and no
 * label, run name or form letter. */
function judgeInput(input: {
  judged: OwnerWalkClause[];
  captured: Captured;
  fixture: OwnerWalkFixture;
  world: OwnerWalkWorld;
  plan: { version: number; markdown: string } | undefined;
  versions: number;
  reads: { call: string; printed: string }[];
  questions: string[];
  answer: string | undefined;
}): string {
  const { captured, fixture, world } = input;
  const needs = new Set(input.judged.flatMap((clause) => clause.needs ?? []));
  const issue = (key: string) => captured.issues.find((each) => each.key === key);
  const parts = [
    "# What one owner wrote",
    "",
    "Judge each item below with `rubrics/owner-walk.md`, from this file alone.",
    "",
    "## The items",
    "",
    ...input.judged.map((clause) => `- \`${clause.id}\`: ${clause.sentence}`),
  ];
  if (needs.has("spec")) {
    const spec = issue(fixture.planned)
      ?.artifacts.find((artifact) => artifact.primary)
      ?.versions.at(-1)?.markdown;
    parts.push(
      "",
      `## The spec of the issue the owner planned, ${fixture.planned}`,
      "",
      spec === undefined ? "The run's Dispatch holds no spec of it." : fenced(spec, "markdown")
    );
  }
  if (needs.has("earlier")) {
    const record = issue(fixture.earlier.key)
      ?.handoffs.filter((handoff) => handoff.phase === "implement")
      .at(-1);
    parts.push(
      "",
      `## The \`implement\` record of the earlier issue, ${fixture.earlier.key}`,
      "",
      record === undefined
        ? "The run's Dispatch holds no such record."
        : `Written by the session \`${stringsOf(record.author.id).join("") || "unknown"}\`:\n\n${fenced(record.body, "json")}`
    );
  }
  if (needs.has("plan"))
    parts.push(
      "",
      `## The owner's plan: plan.md of ${fixture.planned}`,
      "",
      input.plan === undefined
        ? `The owner wrote no plan.md on ${fixture.planned}.`
        : `Version ${input.plan.version}, the newest of ${input.versions}:\n\n${fenced(input.plan.markdown, "markdown")}`
    );
  if (needs.has("read"))
    parts.push(
      "",
      "## What the owner's reads of earlier plans printed",
      "",
      ...(input.reads.length === 0
        ? ["The run made no read of earlier plans."]
        : input.reads.flatMap((read, index) => [
            `### Read ${index + 1}: \`${read.call.replaceAll("`", "'")}\``,
            "",
            fenced(read.printed),
            "",
          ]))
    );
  if (needs.has("guidance"))
    parts.push(
      "",
      "## The guidance the owner got",
      "",
      world.guidance === null
        ? "The owner got no guidance."
        : fenced(filledGuidance(world), "markdown")
    );
  if (needs.has("answer"))
    parts.push(
      "",
      "## The questions, and the owner's answer",
      "",
      fenced(input.questions.join("\n\n")),
      "",
      input.answer === undefined || input.answer.trim() === ""
        ? "The owner gave no answer."
        : fenced(input.answer)
    );
  return `${parts.join("\n").replace(/\n{3,}/g, "\n\n")}\n`;
}

/**
 * owner-walk: one run's clauses, each the sentence of its stage's check (owner-walk.ts,
 * OWNER_WALK_STAGES), for the clauses that hold the run's form. A program decides a clause with a
 * rule; the acceptance worker judges the others from judge-input.md with rubrics/owner-walk.md,
 * and each is `pending` until judgment.json gives its verdict. The plan is the newest version of
 * G's plan.md, written before or after the questions (the count after them is reported). A run
 * whose out.txt ends `exit=bound` fails BOUND_CLAUSE too, with held.txt's reason. Reported beside
 * the clauses, changing none: `cites-key` (the plan names a key of the world's plans keys, or holds
 * eight words in a row of a recorded rating's reason), `quotes-eight` (for a form with guidance:
 * the answer holds eight words in a row of the guidance) and `plan-versions-after-questions`.
 * Writes score.json and judge-input.md, and answers the score. A record missing or malformed is
 * thrown, which score.ts makes the run's rig error.
 */
export function ownerWalk(dir: string, run: string, label: string): Score {
  const named = ownerWalkRun(run);
  if (named === undefined)
    throw new Error(
      `${run} is no owner-walk run's directory: owner-walk-<kind>-<form>-<family>-<label>-<n>`
    );
  if (named.label !== label)
    throw new Error(`the directory ${run} names the label ${named.label}, not ${label}`);
  const world = OwnerWalkWorld.parse(readJson(dir, "world.json"));
  if (caseName(world) !== caseName(named.case))
    throw new Error(
      `world.json is of the case ${caseName(world)}, and the directory names ${caseName(named.case)}`
    );
  const stage = stageOf(world.stage);
  const captured = Captured.parse(readJson(dir, "captured.json"));
  const fixture = OwnerWalkFixture.parse(captured.fixture);
  const planned = captured.issues.find((issue) => issue.key === fixture.planned);
  if (planned === undefined) throw new Error(`captured.json holds no issue ${fixture.planned}`);
  const planDocument = planned.artifacts.find((artifact) => artifact.name === "plan.md");
  const versions = [...(planDocument?.versions ?? [])].sort((a, b) => a.version - b.version);
  const newest = versions.at(-1);
  const evidence: OwnerWalkEvidence = {
    world,
    planned: fixture.planned,
    proxy: jsonLines(dir, "dispatch-proxy.jsonl", DispatchProxyLine),
    planArtifact: planDocument?.id,
    plan: newest === undefined ? undefined : { version: newest.version, markdown: newest.markdown },
  };
  if (newest !== undefined && planWrites(evidence).length === 0)
    throw new Error(
      `captured.json holds ${fixture.planned}'s plan.md, and dispatch-proxy.jsonl holds no write of it: the proxy did not see the run's writes`
    );

  const entries = session(dir);
  const out = existsSync(path.join(dir, "out.txt"))
    ? readFileSync(path.join(dir, "out.txt"), "utf8").trim().split("\n")
    : [];
  const bound = out.at(-1) === "exit=bound";
  const first = world.questions[0] ?? "";
  const asked = entries.findIndex(
    (entry) => entry.message?.role === "user" && textOf(entry).includes(first)
  );
  if (asked < 0 && !bound)
    throw new Error(
      "no user message of the run's transcript holds the world's questions: the rig sent no second turn"
    );
  const answer =
    asked < 0
      ? undefined
      : entries
          .slice(asked + 1)
          .filter((entry) => entry.message?.role === "assistant")
          .map(textOf)
          .filter((text) => text.trim() !== "")
          .join("\n\n");

  const own = stage.clauses.filter((clause) => clause.forms.includes(world.form));
  const judged = own.filter((clause) => clause.rule === undefined);
  const judgmentFile = path.join(dir, "judgment.json");
  const judgment = existsSync(judgmentFile)
    ? Judgment.parse(JSON.parse(readFileSync(judgmentFile, "utf8")))
    : undefined;
  if (judgment !== undefined) {
    if (judgment.run !== run)
      throw new Error(`judgment.json judges the run ${judgment.run}, not ${run}`);
    const seen = new Set<string>();
    for (const item of judgment.items) {
      if (!judged.some((clause) => clause.id === item.item))
        throw new Error(
          `judgment.json judges ${item.item}, which is no judged item of this run (${judged.map((clause) => clause.id).join(", ") || "none"})`
        );
      if (seen.has(item.item)) throw new Error(`judgment.json judges ${item.item} twice`);
      seen.add(item.item);
    }
  }
  const clauses: ScoreClause[] = own.map((clause) => {
    if (clause.rule !== undefined) {
      const result = clause.rule(evidence);
      return {
        clause: clause.sentence,
        pass: result.pass,
        evidence: `${clause.id}: ${result.evidence}`,
        counts: result.counts,
      };
    }
    const item = judgment?.items.find((each) => each.item === clause.id);
    return item === undefined
      ? {
          clause: clause.sentence,
          pass: "pending",
          evidence: `${clause.id}: waiting for judgment.json's item ${clause.id} (rubrics/owner-walk.md)`,
          counts: {},
        }
      : {
          clause: clause.sentence,
          pass: item.verdict === "pass",
          evidence: `${clause.id}: ${item.verdict}, by ${judgment?.by}: ${item.why}`,
          counts: {},
        };
  });
  if (bound) {
    const held = path.join(dir, "held.txt");
    clauses.push({
      clause: BOUND_CLAUSE,
      pass: false,
      evidence: existsSync(held)
        ? readFileSync(held, "utf8").trim() || "held.txt is empty"
        : "out.txt ends exit=bound, and the rig wrote no held.txt naming what held the run",
      counts: {},
    });
  }

  // Reported beside the clauses. The recorded reasons are mapped as the proxy served them
  // (mapRecordedKeys), so they are the text the agent read.
  const source = PlansAnswerSource.parse(readJson(dir, "plans-answer.source"));
  const answerFile = path.join(dir, "plans-answer.json");
  if (!existsSync(answerFile)) throw new Error("the run holds no plans-answer.json");
  let recorded: HeldPlansAnswer;
  try {
    recorded = readPlansAnswer(readFileSync(answerFile), source, world.plansProject);
  } catch (error) {
    throw new Error(
      `plans-answer.json, with plans-answer.source: ${error instanceof Error ? error.message : String(error)}`
    );
  }
  const reasons = recorded.answer.issues
    .flatMap((issue) =>
      (issue.ratings ?? []).map((rating) => mapRecordedKeys(rating.reason ?? "", source.keyMap))
    )
    .filter((reason) => reason !== "");
  const planText = evidence.plan?.markdown ?? "";
  const key = world.plansKeys.find((each) => wholeKeys([each]).test(planText));
  const reason = reasons
    .map((each) => sharedWords(planText, each, 8))
    .find((run) => run !== undefined);
  const reported: Reported[] = [
    {
      name: "cites-key",
      value: key !== undefined || reason !== undefined,
      evidence:
        key !== undefined
          ? `the plan names ${key}`
          : reason !== undefined
            ? `the plan holds "${reason}", eight words in a row of a recorded reason`
            : `the plan names no key of ${world.plansKeys.join(", ")}, and holds no eight words in a row of a recorded reason`,
    },
  ];
  if (world.guidance !== null) {
    const quoted = sharedWords(answer ?? "", filledGuidance(world), 8);
    reported.push({
      name: "quotes-eight",
      value: quoted !== undefined,
      evidence:
        quoted === undefined
          ? "the answer holds no eight words in a row of the guidance"
          : `the answer holds "${quoted}" of the guidance`,
    });
  }
  const askedAt = asked < 0 ? undefined : parseInstant(entries[asked]?.timestamp ?? "");
  const after =
    askedAt === undefined
      ? []
      : versions.filter((version) => (parseInstant(version.created_at) ?? 0n) > askedAt);
  reported.push({
    name: "plan-versions-after-questions",
    value: after.length,
    evidence:
      askedAt === undefined
        ? "the run's transcript gives the questions no time"
        : `${after.length} of ${versions.length} versions of plan.md came after the questions (${entries[asked]?.timestamp})`,
  });

  const score = Score.parse({ run, clauses, reported, reach: reachOf(dir, run) });
  writeFileSync(path.join(dir, "score.json"), `${JSON.stringify(score)}\n`);
  const inputFile = path.join(dir, "judge-input.md");
  if (judged.length === 0) rmSync(inputFile, { force: true });
  else
    writeFileSync(
      inputFile,
      judgeInput({
        judged,
        captured,
        fixture,
        world,
        plan: evidence.plan,
        versions: versions.length,
        reads: judged.some((clause) => clause.needs?.includes("read")) ? plansReads(dir) : [],
        questions: world.questions,
        answer,
      })
    );
  return score;
}

/** One cell of a scenario's runs under a label (C7): its name, its count, the control it is (none
 * for a case's cell), and its runs with their numbers. */
interface CellGroup {
  label: string;
  cell: string;
  count: number;
  control: { id: string; clause: string; fewerThan: number } | undefined;
  rows: { n: number; row: RunRow }[];
}

/** The cell a run of `owner`'s case counts in, in `stage` under `label`, with no run in it yet: a
 * form a control counts is one cell, `<stage>/<form>`, over all its owners, at the stage's runs of
 * the form for each kind and each family; every other form's cell is its case's,
 * `<stage>/<case>`, at the stage's runs of the form. */
function cellOf(
  label: string,
  stageName: string,
  stage: OwnerWalkStage,
  owner: OwnerWalkCase
): CellGroup {
  const control = stage.controls.find((each) => each.form === owner.form);
  return {
    label,
    cell: `${stageName}/${control === undefined ? caseName(owner) : owner.form}`,
    count:
      stage.runs[owner.form] *
      (control === undefined ? 1 : OWNER_WALK_KINDS.length * FAMILIES.length),
    control:
      control === undefined
        ? undefined
        : {
            id: control.clause,
            clause: stage.clauses.find((clause) => clause.id === control.clause)?.sentence ?? "",
            fewerThan: control.fewerThan,
          },
    rows: [],
  };
}

/** A cell's first scored runs by their number, up to its count; a rig error counts none. */
function scoredOf(group: CellGroup): ScoredRow[] {
  return [...group.rows]
    .sort((a, b) => a.n - b.n || (a.row.run < b.row.run ? -1 : 1))
    .flatMap(({ row }) => (row.outcome === "scored" ? [row] : []))
    .slice(0, group.count);
}

/**
 * A cell's line (C7), the one rule of a cell's verdict: a case's cell passes when each of its
 * scored runs passes, and a control's when fewer than its bound pass the clause it counts. A cell
 * with fewer scored runs than its count is UNKNOWN, and one with a pending run (or a pending clause
 * of its control) is PENDING. Neither is ever a pass.
 */
function cellLine(scenario: string, group: CellGroup): CellLine {
  const scored = scoredOf(group);
  const verdicts = scored.map((row) => {
    if (group.control === undefined) return row.pass;
    const counted = row.clauses.find((clause) => clause.clause === group.control?.clause);
    if (counted === undefined)
      throw new Error(
        `${row.run} holds no clause "${group.control.clause}", which its control counts`
      );
    return counted.pass;
  });
  const passes = verdicts.filter((verdict) => verdict === true).length;
  const pending = verdicts.filter((verdict) => verdict === "pending").length;
  const decided =
    group.control === undefined ? passes === group.count : passes < group.control.fewerThan;
  return CellLine.parse({
    scenario,
    label: group.label,
    cell: group.cell,
    count: group.count,
    scored: scored.length,
    passes,
    pending,
    verdict:
      scored.length < group.count ? "UNKNOWN" : pending > 0 ? "PENDING" : decided ? "pass" : "fail",
  });
}

/** Refuses a scenario no run of which this module scores. */
function requireScored(scenario: string): void {
  if (!SCORED_SCENARIOS.includes(scenario))
    throw new Error(
      `${scenario} is no scored scenario; the scored scenarios are ${SCORED_SCENARIOS.join(", ")}`
    );
}

/**
 * The cells of a scenario's runs in `runsDir` (C7), sorted by label and cell, each with its
 * verdict (cellLine). A run counts by its row.json (score.ts runs --run) and its world.json; a run
 * directory without both is in no cell, and stderr names it. owner-walk's cells, per label, are
 * cellOf's: a control form's one cell over all its owners, `passes` counting the owners that pass
 * the control's clause, and every other form's cell one case.
 */
export function cells(runsDir: string, scenario: string): CellLine[] {
  requireScored(scenario);
  const groups = new Map<string, CellGroup>();
  for (const name of existsSync(runsDir) ? readdirSync(runsDir).sort() : []) {
    const named = ownerWalkRun(name);
    if (named === undefined) continue;
    const dir = path.join(runsDir, name);
    const missing = ["row.json", "world.json"].filter((file) => !existsSync(path.join(dir, file)));
    if (missing.length > 0) {
      console.error(`${name}: no ${missing.join(" and no ")}, so it is in no cell`);
      continue;
    }
    const row = RunRow.parse(readJson(dir, "row.json"));
    const world = OwnerWalkWorld.parse(readJson(dir, "world.json"));
    const want = caseName(named.case);
    if (
      row.run !== name ||
      row.label !== named.label ||
      row.case !== want ||
      caseName(world) !== want
    )
      throw new Error(
        `${name}: its row names ${row.run}, label ${row.label} and case ${row.case}, and its world the case ${caseName(world)}`
      );
    const empty = cellOf(named.label, world.stage, stageOf(world.stage), world);
    // A group's label and cell, a tab between them, are its own; a tab sorts before any character
    // of a name, so `ctl-D` comes before `ctl-D-strict`.
    const id = `${named.label}\t${empty.cell}`;
    const group = groups.get(id) ?? empty;
    group.rows.push({ n: named.n, row });
    groups.set(id, group);
  }
  return [...groups.entries()]
    .sort(([a], [b]) => (a < b ? -1 : 1))
    .map(([, group]) => cellLine(scenario, group));
}

/**
 * The line `rig.sh cell` prints for the case `caseText` under `label`, in `stageName`: the cell's
 * runs, its scored runs and its rig errors, counted from the run directories that hold a
 * row.json, and its verdict, cellLine's.
 *   - A case form's cell is the case's runs at `count`, the count its batch ran to:
 *     `cell <scenario> <label> <case>: <n> runs, <s> scored, <e> rig errors, count <count>; each
 *     clause: <id> <passes> of <s>[ (<p> pending)], …; runs passed <passes> of <s>; verdict <v>`.
 *   - A control form's cell is the control's own, as cells() gives it: every owner's runs of the
 *     form under the label, at the control's count, whatever `count` is: `cell <scenario> <label>
 *     <case>: the control <stage>/<form> over every owner: <n> runs, <s> scored, <e> rig errors,
 *     count <c>; <clause id> <passes> of <s>[ (<p> pending)]; it passes when fewer than <bound>
 *     pass <clause id>; verdict <v>`.
 * A case with no run that holds a row.json prints `cell <scenario> <label> <case>: no run holds a
 * row.json, so the cell is UNKNOWN`.
 */
export function caseCell(
  runsDir: string,
  scenario: string,
  stageName: string,
  label: string,
  caseText: string,
  count: number
): string {
  requireScored(scenario);
  const stage = stageOf(stageName);
  const want = parseOwnerWalkCase(caseText);
  const head = `cell ${scenario} ${label} ${caseName(want)}`;
  const group = cellOf(label, stageName, stage, want);
  for (const name of existsSync(runsDir) ? readdirSync(runsDir).sort() : []) {
    const named = ownerWalkRun(name);
    const file = path.join(runsDir, name, "row.json");
    if (named === undefined || named.label !== label || !existsSync(file)) continue;
    const mine =
      group.control === undefined
        ? caseName(named.case) === caseName(want)
        : named.case.form === want.form;
    if (!mine) continue;
    group.rows.push({ n: named.n, row: RunRow.parse(JSON.parse(readFileSync(file, "utf8"))) });
  }
  const errors = group.rows.filter(({ row }) => row.outcome === "rig-error").length;
  if (group.control !== undefined) {
    const { id, fewerThan } = group.control;
    const cell =
      cells(runsDir, scenario).find((line) => line.label === label && line.cell === group.cell) ??
      cellLine(scenario, { ...group, rows: [] });
    const pending = cell.pending > 0 ? ` (${cell.pending} pending)` : "";
    return `${head}: the control ${cell.cell} over every owner: ${group.rows.length} runs, ${cell.scored} scored, ${errors} rig errors, count ${cell.count}; ${id} ${cell.passes} of ${cell.scored}${pending}; it passes when fewer than ${fewerThan} pass ${id}; verdict ${cell.verdict}`;
  }
  if (group.rows.length === 0) return `${head}: no run holds a row.json, so the cell is UNKNOWN`;
  const counted = { ...group, count };
  const scored = scoredOf(counted);
  const line = cellLine(scenario, counted);
  // Each clause by its id, in the order the scored runs first name it; a clause of no stage id (the
  // bound's) by its sentence.
  const tallies = new Map<string, { passes: number; pending: number; of: number }>();
  for (const row of scored)
    for (const clause of row.clauses) {
      const id = stage.clauses.find((each) => each.sentence === clause.clause)?.id ?? clause.clause;
      const tally = tallies.get(id) ?? { passes: 0, pending: 0, of: 0 };
      tally.of += 1;
      if (clause.pass === true) tally.passes += 1;
      if (clause.pass === "pending") tally.pending += 1;
      tallies.set(id, tally);
    }
  const each = [...tallies]
    .map(
      ([id, tally]) =>
        `${id} ${tally.passes} of ${tally.of}${tally.pending > 0 ? ` (${tally.pending} pending)` : ""}`
    )
    .join(", ");
  return `${head}: ${group.rows.length} runs, ${scored.length} scored, ${errors} rig errors, count ${count}; each clause: ${each}; runs passed ${line.passes} of ${scored.length}; verdict ${line.verdict}`;
}
