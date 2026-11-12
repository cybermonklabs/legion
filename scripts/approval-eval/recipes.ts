import { Buffer } from "node:buffer";
import { readFile } from "node:fs/promises";
import type { CaseRow } from "./build-corpus";
import type { DispatchConfig } from "./dispatch-read";
import { type NearestAnswer, nearestCases, type RetrievalRoute } from "./retrieval";

/**
 * The four information recipes, which are what this round changes.
 *
 * Every arm is given the same case, rendered by `render.ts`, and differs only in what else it
 * is told (dispatch://INFRA-2897/artifact/plan-of-analysis-md@v2, "The arms"):
 *
 * - `bare`: the documents, the issue key, title and project, and the question with its options.
 * - `distilled`: that, and one written distillation of the person's principles and repeated
 *   judgements, drawn from the training slice alone, which becomes a reference file of the
 *   shipped skill. The caller names the file; a caller naming none gets a refusal rather than a
 *   quiet second copy of `bare`, because an arm that silently became another arm would be
 *   reported as a difference between recipes that is no difference at all.
 * - `many-shot`: that, and the eight nearest training cases by meaning, each as its question,
 *   the verdict and the person's words where they wrote any. Never the neighbour's document: an
 *   example is a decision, not a spec, and eight specs would be most of the context.
 * - `both`: the distillation and the examples together.
 *
 * Each prompt asks for one answer in one fixed shape, so the runner reads a verdict and never a
 * paragraph, and each carries the record of what went into it: how many examples, by which
 * retrieval route, how many neighbours were excluded for sitting on the judged case's own
 * ground, and how many bytes were sent.
 */

/** The name of one arm's information recipe. */
export type RecipeName = "bare" | "distilled" | "many-shot" | "both";

/** One recipe: what it is, and what it needs beyond the rendered case. */
export interface Recipe {
  name: RecipeName;
  /** What this recipe is, in the words of the plan. */
  what: string;
  /** Whether it carries the written distillation. */
  distillation: boolean;
  /** How many neighbouring cases it carries. */
  examples: number;
}

/** How many neighbours a many-shot arm is given, as the plan's "The arms" names. */
export const MANY_SHOT_EXAMPLES = 8;

/** The four recipes, which are the four arms of each model. */
export const RECIPES: Readonly<Record<RecipeName, Recipe>> = {
  bare: {
    name: "bare",
    what: "the documents, the issue key, title and project, and the question with its options",
    distillation: false,
    examples: 0,
  },
  distilled: {
    name: "distilled",
    what: "the bare case and one written distillation of the person's principles",
    distillation: true,
    examples: 0,
  },
  "many-shot": {
    name: "many-shot",
    what: "the bare case and the eight nearest training cases by meaning",
    distillation: false,
    examples: MANY_SHOT_EXAMPLES,
  },
  both: {
    name: "both",
    what: "the bare case, the distillation and the eight nearest training cases",
    distillation: true,
    examples: MANY_SHOT_EXAMPLES,
  },
};

/** A recipe was asked for without what it needs, so it would have been another recipe. */
export class RecipeError extends Error {
  override readonly name = "RecipeError";
}

/** How `renderPrompt` is told where the distillation and the neighbours come from. */
export interface PromptOptions {
  /** The file holding the distillation, which a reference file of the skill will be. */
  distillationPath?: string;
  /** A retrieval already run for this case, which the two many-shot arms share. */
  nearest?: NearestAnswer;
  /** The corpus to retrieve over, where no retrieval was handed in. */
  corpus?: readonly CaseRow[];
  /** The cut, past which no case may be an example. */
  cut?: string | Date;
  /** The server and the bearer, for a retrieval run here. */
  config?: DispatchConfig;
}

/** What went into one prompt, which the round records beside the judgement. */
export interface PromptRecord {
  recipe: RecipeName;
  /** How many neighbouring cases the prompt carries. */
  examples: number;
  /** Which index found them, and null for a recipe that carries none. */
  route: RetrievalRoute | null;
  /** How many neighbours were dropped for the judged case's own document or issue. */
  excluded: number | null;
  /** How many bytes of system and user text were sent. */
  bytes: number;
}

/** One prompt, as the runner sends it, and the record of what went into it. */
export interface Prompt {
  system: string;
  user: string;
  record: PromptRecord;
}

/** The one shape every recipe asks its answer in, so the runner reads a verdict and no prose. */
const ANSWER_SHAPE = [
  "Answer with the label of the option you choose, exactly as it is written above, on one line",
  "by itself, and nothing else: no reasoning, no punctuation around it and no other word. Where",
  "the case says more than one option may be chosen, put each chosen label on its own line.",
].join(" ");

/** What the judge is for, which every arm is told in the same words. */
const TASK = [
  "You decide an approval request as one person decides it. You are shown the request as that",
  "person saw it: the issue it belongs to, the document at the version the request named, and",
  "the question with the options they were offered. Choose the option that person chose.",
].join(" ");

/** One neighbour as an example: the question, the verdict and the words, never the document. */
function exampleOf(row: CaseRow, at: number): string {
  const lines = [`### Example ${at}`, "", `Question: ${row.question ?? "none"}`];
  if (row.kind === "approval") {
    lines.push(`Their verdict: ${row.verdict ?? "none"}`);
  }
  if (row.selected.length > 0) {
    lines.push(`They chose: ${row.selected.join(", ")}`);
  }
  if (row.hasWords && row.words !== null) {
    lines.push("", "Their words:", row.words.trim());
  }
  return lines.join("\n");
}

/** The neighbours a recipe needs: the retrieval handed in, or one run over the corpus named. */
async function neighboursFor(
  recipe: Recipe,
  caseRow: CaseRow,
  options: PromptOptions
): Promise<NearestAnswer | null> {
  if (recipe.examples === 0) return null;
  if (options.nearest !== undefined) return options.nearest;
  if (options.corpus === undefined || options.cut === undefined) {
    throw new RecipeError(
      `the ${recipe.name} recipe carries ${recipe.examples} examples, so it needs either a ` +
        "retrieval in `nearest` or a corpus and a cut to run one over"
    );
  }
  return await nearestCases(options.corpus, caseRow, {
    count: recipe.examples,
    cut: options.cut,
    config: options.config,
  });
}

/**
 * The distillations read from disk, by path. One round renders a prompt once per judgement, and
 * every one of them carries the same file: without this the file is read about fourteen thousand
 * times for one round. Only a read that returned is kept, so a path that has moved raises on
 * every call and an empty file raises its `RecipeError` on every call, as before.
 */
const distillations = new Map<string, string>();

/** The distillation at `path`, read once per process. */
async function distillationAt(path: string): Promise<string> {
  const held = distillations.get(path);
  if (held !== undefined) return held;
  const text = (await readFile(path, "utf8")).trim();
  if (text === "") throw new RecipeError(`the distillation at ${path} is empty`);
  distillations.set(path, text);
  return text;
}

/**
 * The read a command makes before it drives anything, so a path that has moved is refused
 * before the work starts. The text it read is kept, so the first prompt costs no second read
 * and nothing can move the file between the check and that prompt. An empty file is not kept:
 * it is `renderPrompt` that refuses one, and it still does.
 */
export async function readDistillation(path: string): Promise<string> {
  const text = (await readFile(path, "utf8")).trim();
  if (text !== "") distillations.set(path, text);
  return text;
}

/**
 * One arm's prompt over one case: the system text that says what the judge does and in what
 * shape it answers, and the user text carrying the distillation, the examples and the evidence.
 */
export async function renderPrompt(
  recipe: RecipeName,
  caseRow: CaseRow,
  evidence: string,
  options: PromptOptions = {}
): Promise<Prompt> {
  const named = RECIPES[recipe];
  let distillation: string | null = null;
  if (named.distillation) {
    const path = options.distillationPath?.trim();
    if (!path) {
      throw new RecipeError(
        `the ${named.name} recipe is the bare case and a distillation, and no ` +
          "`distillationPath` was named: a recipe never quietly becomes `bare`"
      );
    }
    distillation = await distillationAt(path);
  }
  const nearest = await neighboursFor(named, caseRow, options);
  const system = `${TASK}\n\n${ANSWER_SHAPE}`;
  const parts: string[] = [];
  if (distillation !== null) {
    parts.push("## How this person judges", "", distillation, "");
  }
  if (nearest !== null && nearest.neighbours.length > 0) {
    parts.push(
      `## ${nearest.neighbours.length} decisions this person made before this one`,
      "",
      "Each is a past case with what they decided. None is the case you are judging.",
      ""
    );
    nearest.neighbours.forEach((neighbour, index) => {
      parts.push(exampleOf(neighbour.row, index + 1), "");
    });
  }
  parts.push("## The request to decide", "", evidence.trimEnd(), "", ANSWER_SHAPE);
  const user = `${parts.join("\n")}\n`;
  return {
    system,
    user,
    record: {
      recipe: named.name,
      examples: nearest?.neighbours.length ?? 0,
      route: nearest?.route ?? null,
      excluded: nearest?.excluded ?? null,
      bytes: Buffer.byteLength(system, "utf8") + Buffer.byteLength(user, "utf8"),
    },
  };
}
