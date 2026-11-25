#!/usr/bin/env bun
/**
 * The owner-walk scenario: the check `v-guidance-judge`, its stages, its cases and the world of
 * one run. A run is one fresh owner (a root's architect, or a child's) that plans the issue G of
 * `worlds/owner-walk/issue.md`, then answers the questions of its form. Each stage is an entry of
 * OWNER_WALK_STAGES: the check its clauses are the sentences of, the forms each clause holds, the
 * runs each form takes and the controls. A later piece that changes what an owner is given adds its
 * stage here, with its clauses and its world facts. scored-runs.ts scores a run with its stage.
 *
 * The world's files, in `worlds/owner-walk/` (each `.md` text opens with `# <title>`, which the seed
 * takes as the issue's title, and the rest is its spec):
 *   issue.md            G's spec, with its two tasks. `{{EARLIER_ISSUE}}` is E1's key, which the
 *                       seed fills.
 *   root.md, child.md   R's spec, and the child C's (for a child owner).
 *   earlier-issue.md    E1's spec.
 *   earlier-implement.json
 *                       E1's `implement` record, but its three stamps (schemaVersion, phase,
 *                       issue), which the seed adds. Its session is EARLIER_SESSION.
 *   fixtures.json       the gh stand-in's routes: E1's pull request EARLIER_PR, merged. rig.sh
 *                       copies it to the run's fixtures.json.
 *   guidance-first.md   form A's and D's guidance: the implementer on the Claude family and the
 *                       reviewer on the GPT family, each with its model.
 *   guidance-second.md  form C's guidance: both on the Claude family, with one model.
 *   questions.txt       the second turn's questions, one a line: `<forms>: <question>`.
 * The guidance files name their models `{{IMPLEMENTER_MODEL}}` and `{{REVIEWER_MODEL}}`, which the
 * world's guidance fills (`guidance`, below).
 *
 * A run's directory is `owner-walk-<kind>-<form>-<family>-<label>-<n>`: the case, the label and the
 * run's number. The case is `<kind>-<form>-<family>`, as SKILL_SCENARIOS_OWNER_WALK_CASE names it.
 *
 *   owner-walk.ts cases [<stage>]
 *     Prints each case of the stage (default guidance) and the scored runs it takes, `<case> <n>`,
 *     one a line: each form's runs for each kind of owner and each family.
 *   owner-walk.ts world <case> <stage> <plans-answer.json> <world.json> [<claude model> <gpt model>]
 *     Writes the run's world.json (OwnerWalkWorld): the case, the stage, its form's guidance and
 *     questions, and the plans read's project and keys as the run's Dispatch holds them: the run
 *     keys of the recorded answer's issues, read with plans-answer.source beside the answer
 *     (scored-runs-contracts.ts readPlansAnswer). Forms A, C and D take the two model ids, which
 *     the guidance names; forms B and E take none and ignore them.
 *   owner-walk.ts guidance <world.json> <out>
 *     Writes the world's guidance file to <out>, its two model placeholders filled with the world's
 *     models: the owner's `planGuidanceFile`. A world with no guidance is refused.
 *   owner-walk.ts questions <world.json>
 *     Prints the second turn's message: the world's questions, a blank line between two.
 * Each command exits 2 on a usage error, and 1 with the reason on a refused input.
 */
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  type DispatchProxyLine,
  FAMILIES,
  OWNER_WALK_FORMS,
  OWNER_WALK_KINDS,
  OWNER_WALK_PROJECT,
  type OwnerWalkForm,
  type OwnerWalkKind,
  OwnerWalkWorld,
  PlansAnswerSource,
  type RecordedPlan,
  readPlansAnswer,
} from "./scored-runs-contracts";

/** The world's files. */
export const OWNER_WALK_DIR = path.join(import.meta.dir, "worlds", "owner-walk");
/** The session that wrote E1's implement record: the earlier agent of G's first task. */
export const EARLIER_SESSION = "rig-implementer-e1";
/** E1's pull request on the gh stand-in (fixtures.json). */
export const EARLIER_PR = 41;

/** One case of owner-walk: the kind of owner, its form and the family of its model. */
export interface OwnerWalkCase {
  kind: OwnerWalkKind;
  form: OwnerWalkForm;
  family: (typeof FAMILIES)[number];
}
export const caseName = (c: OwnerWalkCase) => `${c.kind}-${c.form}-${c.family}`;
const CASE = new RegExp(
  `^(${OWNER_WALK_KINDS.join("|")})-(${OWNER_WALK_FORMS.join("|")})-(${FAMILIES.join("|")})$`
);
/** The case a name gives, or a refusal that names each value it takes. */
export function parseOwnerWalkCase(name: string): OwnerWalkCase {
  const match = CASE.exec(name);
  if (match === null)
    throw new Error(
      `${JSON.stringify(name)} is no case of owner-walk: a case is <kind>-<form>-<family>, kind ${OWNER_WALK_KINDS.join(" or ")}, form ${OWNER_WALK_FORMS.join(", ")}, family ${FAMILIES.join(" or ")}`
    );
  return {
    kind: match[1] as OwnerWalkKind,
    form: match[2] as OwnerWalkForm,
    family: match[3] as OwnerWalkCase["family"],
  };
}
/** An owner-walk run's directory name: `owner-walk-<case>-<label>-<n>`. A label may hold a hyphen. */
const RUN = new RegExp(
  `^owner-walk-((?:${OWNER_WALK_KINDS.join("|")})-(?:${OWNER_WALK_FORMS.join("|")})-(?:${FAMILIES.join("|")}))-(.+)-(\\d+)$`
);
/** The case, the label and the number an owner-walk run's directory names, or undefined. */
export function ownerWalkRun(
  name: string
): { case: OwnerWalkCase; label: string; n: number } | undefined {
  const match = RUN.exec(name);
  if (match === null) return undefined;
  return {
    case: parseOwnerWalkCase(match[1] ?? ""),
    label: match[2] ?? "",
    n: Number(match[3]),
  };
}

// The plan's task blocks.

/** One `plan_task` block of a plan document: its id, its step words in order, its attributes as
 * its opening line writes them, and its text. */
export interface PlanTaskBlock {
  id: string;
  steps: string[];
  attributes: Record<string, string>;
  text: string;
}
const FENCE = /^[ \t]*(`{3,}|~{3,})/;
const TASK_OPEN = /^:::plan_task\{([^}]*)\}[ \t]*$/;
const BLOCK_CLOSE = /^:::[ \t]*$/;
const ATTRIBUTE = /#([^\s"]+)|([A-Za-z_][\w-]*)="([^"]*)"/g;
/** The `plan_task` blocks of a plan's markdown, in its order. A block's opening line is
 * `:::plan_task{#<id> steps="…" …}` and its close a line `:::`, as Dispatch reads them. An opening
 * line inside a fenced code block is a quote of the form, and no block. */
export function planTaskBlocks(markdown: string): PlanTaskBlock[] {
  const blocks: PlanTaskBlock[] = [];
  let fence: string | undefined;
  let open: { attributes: Record<string, string>; id: string; lines: string[] } | undefined;
  for (const line of markdown.split("\n")) {
    const marker = FENCE.exec(line)?.[1];
    if (fence !== undefined) {
      if (marker !== undefined && marker[0] === fence[0] && marker.length >= fence.length)
        fence = undefined;
      open?.lines.push(line);
      continue;
    }
    if (marker !== undefined) {
      fence = marker;
      open?.lines.push(line);
      continue;
    }
    if (open !== undefined) {
      if (BLOCK_CLOSE.test(line)) {
        const steps = (open.attributes.steps ?? "").split(/\s+/).filter((word) => word !== "");
        blocks.push({
          id: open.id,
          steps,
          attributes: open.attributes,
          text: open.lines.join("\n"),
        });
        open = undefined;
      } else open.lines.push(line);
      continue;
    }
    const opening = TASK_OPEN.exec(line);
    if (opening === null) continue;
    const attributes: Record<string, string> = {};
    let id = "";
    for (const hit of (opening[1] ?? "").matchAll(ATTRIBUTE)) {
      if (hit[1] !== undefined) id = hit[1];
      else if (hit[2] !== undefined) attributes[hit[2]] = hit[3] ?? "";
    }
    open = { attributes, id, lines: [] };
  }
  return blocks;
}
/** A step word's role: `implementer:cd` is the implementer, `acceptance:owner` the acceptance. */
const roleOf = (step: string) => step.split(":")[0] ?? step;
/** The blocks whose steps put a reviewer after an implementer. */
function reviewedBlocks(blocks: PlanTaskBlock[]): PlanTaskBlock[] {
  return blocks.filter((block) => {
    const roles = block.steps.map(roleOf);
    const implementer = roles.indexOf("implementer");
    return implementer >= 0 && roles.slice(implementer + 1).includes("reviewer");
  });
}
/** A family's words, as words in any case; a model id holds them (`openai/gpt-6.1-sol`). */
const FAMILY_WORDS: Record<(typeof FAMILIES)[number], RegExp> = {
  claude: /\b(?:claude|anthropic)\b/i,
  gpt: /\b(?:gpt|openai)\b/i,
};

// The clauses.

/** What a program clause reads of one run: its world, G's key, the proxy's log of the run's
 * Dispatch requests in log order, the id of G's plan.md and the newest version of it. */
export interface OwnerWalkEvidence {
  world: OwnerWalkWorld;
  planned: string;
  proxy: DispatchProxyLine[];
  planArtifact: string | undefined;
  plan: { version: number; markdown: string } | undefined;
}
/** A clause's verdict on one run, with what decided it and what it counted. */
export interface ClauseResult {
  pass: boolean;
  evidence: string;
  counts: Record<string, number>;
}
/** The texts of a run a judged item needs (scored-runs.ts writes each into judge-input.md). */
export type JudgeText = "spec" | "earlier" | "plan" | "read" | "guidance" | "answer";
/** One clause of a stage: its id, the check's sentence, the forms whose owners it holds, and a
 * program's rule, or for a clause the acceptance worker judges, the texts its judge needs. */
export interface OwnerWalkClause {
  id: string;
  sentence: string;
  forms: readonly OwnerWalkForm[];
  rule?: (evidence: OwnerWalkEvidence) => ClauseResult;
  needs?: readonly JudgeText[];
}
/** A control counted over all the owners of a form: it passes when fewer than `fewerThan` of them
 * pass `clause`. */
export interface OwnerWalkControl {
  form: OwnerWalkForm;
  clause: string;
  fewerThan: number;
}
export interface OwnerWalkStage {
  check: string;
  clauses: readonly OwnerWalkClause[];
  /** The scored runs of each form, for each kind of owner and each family. */
  runs: Record<OwnerWalkForm, number>;
  controls: readonly OwnerWalkControl[];
}

/** The proxy lines that write G's documents: a create or a new version through the issue's
 * documents route, or any write of the plan's own document, that Dispatch answered 2xx (a refused
 * write wrote nothing). The first is the plan's first write (a document of G other than plan.md,
 * written before it, counts as that write too). */
export function planWrites(evidence: OwnerWalkEvidence): number[] {
  const own = `/api/v1/issues/${evidence.planned}/artifacts`;
  const plan =
    evidence.planArtifact === undefined ? undefined : `/api/v1/artifacts/${evidence.planArtifact}`;
  return evidence.proxy.flatMap((line, index) =>
    line.method !== "GET" &&
    line.method !== "HEAD" &&
    line.status >= 200 &&
    line.status < 300 &&
    (line.path === own ||
      line.path.startsWith(`${own}/`) ||
      (plan !== undefined && (line.path === plan || line.path.startsWith(`${plan}/`))))
      ? [index]
      : []
  );
}

/** read: "runs the read of earlier plans before it writes its plan". A GET of the run's project's
 * plans, answered 2xx, stands in the proxy's log before the first write of G's plan.md. The log's
 * order decides, as each request is one line in the order it was answered. */
function readRule(evidence: OwnerWalkEvidence): ClauseResult {
  const route = `/api/v1/projects/${evidence.world.plansProject}/plans`;
  const reads = evidence.proxy.flatMap((line, index) =>
    line.method === "GET" && line.path === route && line.status >= 200 && line.status < 300
      ? [index]
      : []
  );
  const writes = planWrites(evidence);
  const first = writes[0];
  const before = first === undefined ? [] : reads.filter((index) => index < first);
  const counts = { reads: reads.length, readsBeforeWrite: before.length, writes: writes.length };
  const at = (index: number) => `line ${index + 1} (${evidence.proxy[index]?.at})`;
  if (first === undefined)
    return {
      pass: false,
      evidence: `dispatch-proxy.jsonl holds no write of ${evidence.planned}'s plan.md`,
      counts,
    };
  if (before.length === 0)
    return {
      pass: false,
      evidence: `no GET ${route} stands before the plan's first write, ${at(first)} of dispatch-proxy.jsonl${reads.length === 0 ? "" : `; the read came after it, at ${reads.map(at).join(", ")}`}`,
      counts,
    };
  return {
    pass: true,
    evidence: `GET ${route} at ${at(before[0] ?? 0)}, before the plan's first write at ${at(first)}`,
    counts,
  };
}

/** The plan's task blocks, or a failed result when the run wrote no plan. */
function blocksOf(
  evidence: OwnerWalkEvidence
): { blocks: PlanTaskBlock[] } | { failed: ClauseResult } {
  if (evidence.plan === undefined)
    return {
      failed: {
        pass: false,
        evidence: `${evidence.planned} holds no plan.md`,
        counts: { blocks: 0 },
      },
    };
  return { blocks: planTaskBlocks(evidence.plan.markdown) };
}
const stepsOf = (block: PlanTaskBlock) => `#${block.id} steps="${block.steps.join(" ")}"`;

/** review step: "puts a reviewer step after the implementer step". A `plan_task` block of the
 * newest plan names `reviewer` after `implementer` in its steps. */
function reviewStepRule(evidence: OwnerWalkEvidence): ClauseResult {
  const read = blocksOf(evidence);
  if ("failed" in read) return read.failed;
  const reviewed = reviewedBlocks(read.blocks);
  const counts = { blocks: read.blocks.length, reviewed: reviewed.length };
  const where = `plan.md version ${evidence.plan?.version}`;
  return reviewed.length > 0
    ? { pass: true, evidence: `${where}: ${reviewed.map(stepsOf).join(", ")}`, counts }
    : {
        pass: false,
        evidence: `${where}: no task block names reviewer after implementer (${read.blocks.map(stepsOf).join(", ") || "no task block"})`,
        counts,
      };
}

/** families: "that step's text names the reviewer's and the implementer's model families". The
 * text of a block that puts a reviewer after the implementer names both families of the guidance,
 * each by its words (FAMILY_WORDS). */
function familiesRule(evidence: OwnerWalkEvidence): ClauseResult {
  const read = blocksOf(evidence);
  if ("failed" in read) return read.failed;
  const guidance = evidence.world.guidance;
  if (guidance === null)
    throw new Error(`form ${evidence.world.form} has no guidance, whose families it would read`);
  const families = [...new Set([guidance.implementerFamily, guidance.reviewerFamily])];
  const reviewed = reviewedBlocks(read.blocks);
  const naming = reviewed.filter((block) =>
    families.every((family) => FAMILY_WORDS[family].test(block.text))
  );
  const counts = { reviewed: reviewed.length, naming: naming.length };
  if (naming.length > 0)
    return {
      pass: true,
      evidence: `${naming.map(stepsOf).join(", ")} names ${families.join(" and ")}`,
      counts,
    };
  return {
    pass: false,
    evidence:
      reviewed.length === 0
        ? "no task block puts a reviewer after the implementer"
        : reviewed
            .map(
              (block) =>
                `${stepsOf(block)} names ${families.filter((family) => FAMILY_WORDS[family].test(block.text)).join(" and ") || "no family"} of ${families.join(" and ")}`
            )
            .join("; "),
    counts,
  };
}

/** The stages of owner-walk. `guidance` is `v-guidance-judge` at the piece dispatch://INFRA-4115,
 * version 9 of the Verification document; each clause's sentence is the check's. */
export const OWNER_WALK_STAGES: Record<string, OwnerWalkStage> = {
  guidance: {
    check: "v-guidance-judge",
    clauses: [
      {
        id: "read",
        sentence: "runs the read of earlier plans before it writes its plan",
        forms: ["A", "B", "C", "D"],
        rule: readRule,
      },
      {
        id: "choices",
        sentence:
          "says, for each step, whether a fresh agent or the agent of an earlier step works it, and why",
        forms: ["A", "B", "C", "E"],
        needs: ["plan"],
      },
      {
        id: "earlier-and-fresh",
        sentence: "names the earlier agent for the first task, and a fresh agent for the second",
        forms: ["A", "B", "C"],
        needs: ["spec", "earlier", "plan"],
      },
      {
        id: "cites",
        sentence: "cites what dispatch plans printed",
        forms: ["A"],
        needs: ["plan", "read"],
      },
      {
        id: "review-step",
        sentence: "puts a reviewer step after the implementer step",
        forms: ["A", "C"],
        rule: reviewStepRule,
      },
      {
        id: "families",
        sentence: "that step's text names the reviewer's and the implementer's model families",
        forms: ["A"],
        rule: familiesRule,
      },
      {
        id: "not-yet-quotes",
        sentence:
          "answers that the review is not yet on another family, and it quotes the guidance",
        forms: ["C"],
        needs: ["guidance", "answer"],
      },
      {
        id: "yes",
        sentence: "answers yes and names both models",
        forms: ["A"],
        needs: ["guidance", "answer"],
      },
    ],
    runs: { A: 3, B: 3, C: 1, D: 3, E: 3 },
    controls: [
      { form: "D", clause: "read", fewerThan: 6 },
      { form: "E", clause: "choices", fewerThan: 6 },
    ],
  },
};

/** A stage by its name, or a refusal that names the stages. */
export function stageOf(name: string): OwnerWalkStage {
  const stage = OWNER_WALK_STAGES[name];
  if (stage === undefined)
    throw new Error(
      `owner-walk has no stage ${JSON.stringify(name)}; its stages are ${Object.keys(OWNER_WALK_STAGES).join(", ")}`
    );
  return stage;
}

// The world of one run.

/** The form whose texts a control form's owner gets: D is A with an older label, E is B. */
const QUESTIONS_OF: Record<OwnerWalkForm, OwnerWalkForm> = {
  A: "A",
  B: "B",
  C: "C",
  D: "A",
  E: "B",
};
/** The second turn's questions of a form, from questions.txt, in the file's order. */
export function questionsOf(form: OwnerWalkForm): string[] {
  const asked = QUESTIONS_OF[form];
  const file = path.join(OWNER_WALK_DIR, "questions.txt");
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .flatMap((line, index) => {
      const match = /^([A-E](?: [A-E])*): (\S.*)$/.exec(line);
      if (match === null)
        throw new Error(`${file}:${index + 1} is no line of the form <forms>: <question>`);
      return (match[1] ?? "").split(" ").includes(asked) ? [match[2] ?? ""] : [];
    });
}

/** The run's world: its case, its stage, its form's guidance with the two model ids, its form's
 * questions, and the plans read's project and keys: the run keys of the recorded answer's issues,
 * `plans` as readPlansAnswer answers them. */
export function ownerWalkWorld(input: {
  case: OwnerWalkCase;
  stage: string;
  plans: readonly RecordedPlan[];
  models?: { claude: string; gpt: string };
}): OwnerWalkWorld {
  stageOf(input.stage);
  const { kind, form, family } = input.case;
  const needModels = () => {
    if (input.models === undefined)
      throw new Error(`form ${form} has guidance, which names a Claude model and a GPT model`);
    return input.models;
  };
  const guidance =
    form === "A" || form === "D"
      ? {
          file: "guidance-first.md",
          implementerFamily: "claude" as const,
          reviewerFamily: "gpt" as const,
          implementerModel: needModels().claude,
          reviewerModel: needModels().gpt,
        }
      : form === "C"
        ? {
            file: "guidance-second.md",
            implementerFamily: "claude" as const,
            reviewerFamily: "claude" as const,
            implementerModel: needModels().claude,
            reviewerModel: needModels().claude,
          }
        : null;
  return OwnerWalkWorld.parse({
    scenario: "owner-walk",
    stage: input.stage,
    kind,
    form,
    family,
    issue: "issue.md",
    plansProject: OWNER_WALK_PROJECT,
    plansKeys: input.plans.map((plan) => plan.key),
    guidance,
    questions: questionsOf(form),
  });
}

const PLACEHOLDERS = ["{{IMPLEMENTER_MODEL}}", "{{REVIEWER_MODEL}}"] as const;
/** The world's guidance file, its two placeholders filled with the world's models. A file that
 * lacks one of them, or a world with no guidance, is refused. */
export function filledGuidance(world: OwnerWalkWorld): string {
  if (world.guidance === null) throw new Error(`form ${world.form} has no guidance`);
  const file = path.join(OWNER_WALK_DIR, world.guidance.file);
  const text = readFileSync(file, "utf8");
  for (const placeholder of PLACEHOLDERS)
    if (!text.includes(placeholder)) throw new Error(`${file} holds no ${placeholder}`);
  return text
    .replaceAll(PLACEHOLDERS[0], world.guidance.implementerModel)
    .replaceAll(PLACEHOLDERS[1], world.guidance.reviewerModel);
}

/** A world text of `worlds/owner-walk/`: its title, from its first line `# <title>`, and the rest. */
export function worldText(name: string): { title: string; spec: string } {
  const file = path.join(OWNER_WALK_DIR, name);
  const [first = "", ...rest] = readFileSync(file, "utf8").split("\n");
  if (!first.startsWith("# ")) throw new Error(`${file} opens with no "# <title>" line`);
  return { title: first.slice(2).trim(), spec: rest.join("\n").replace(/^\n+/, "") };
}

const USAGE =
  "usage: owner-walk.ts cases [<stage>] | world <case> <stage> <plans-answer.json> <world.json> [<claude model> <gpt model>] | guidance <world.json> <out> | questions <world.json>";

function main(argv: string[]): void {
  const [command, ...rest] = argv;
  const readWorld = (file: string) => OwnerWalkWorld.parse(JSON.parse(readFileSync(file, "utf8")));
  if (command === "cases" && rest.length <= 1) {
    const stage = stageOf(rest[0] ?? "guidance");
    for (const kind of OWNER_WALK_KINDS)
      for (const form of OWNER_WALK_FORMS)
        for (const family of FAMILIES)
          console.log(`${caseName({ kind, form, family })} ${stage.runs[form]}`);
  } else if (command === "world" && (rest.length === 4 || rest.length === 6)) {
    const [name = "", stage = "", answerFile = "", out = "", claude, gpt] = rest;
    const sourceFile = path.join(path.dirname(answerFile), "plans-answer.source");
    let plans: RecordedPlan[];
    try {
      const source = PlansAnswerSource.parse(JSON.parse(readFileSync(sourceFile, "utf8")));
      plans = readPlansAnswer(readFileSync(answerFile), source, OWNER_WALK_PROJECT).plans;
    } catch (error) {
      throw new Error(
        `the recorded plans answer ${answerFile}, with its source ${sourceFile}: ${error instanceof Error ? error.message : String(error)}`
      );
    }
    const world = ownerWalkWorld({
      case: parseOwnerWalkCase(name),
      stage,
      plans,
      ...(claude === undefined || gpt === undefined ? {} : { models: { claude, gpt } }),
    });
    writeFileSync(out, `${JSON.stringify(world)}\n`);
  } else if (command === "guidance" && rest.length === 2) {
    writeFileSync(rest[1] ?? "", filledGuidance(readWorld(rest[0] ?? "")));
  } else if (command === "questions" && rest.length === 1) {
    console.log(readWorld(rest[0] ?? "").questions.join("\n\n"));
  } else {
    console.error(USAGE);
    process.exit(2);
  }
}

if (import.meta.main) {
  try {
    main(Bun.argv.slice(2));
  } catch (error) {
    console.error(`owner-walk.ts: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(1);
  }
}
