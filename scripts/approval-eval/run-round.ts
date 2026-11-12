import { createHash } from "node:crypto";
import { appendFile, mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { parseArgs } from "node:util";
import type { CaseKind, CaseRow } from "./build-corpus";
import {
  type Corpus,
  CorpusError,
  listOf,
  OutputPathError,
  outDirectory,
  pool,
  randomStream,
  readCorpus,
  recordOf,
  stringOf,
  UsageError,
} from "./corpus-io";
import {
  type DispatchConfig,
  DispatchConfigError,
  DispatchRefusedError,
  DispatchUnreachableError,
  dispatchConfig,
} from "./dispatch-read";
import type { SliceName } from "./draw";
import { cutOf as cutOfDraw, drawPath, readDraw } from "./draw-io";
import {
  askClaude,
  askJev,
  assertJevKeyPresent,
  type ClaudeAsk,
  type FaultKind,
  GatewayError,
  type JevAsk,
  type JevChoice,
  JevKeyError,
  type ModelAnswer,
  scrubSecrets,
} from "./models";
import {
  type Prompt,
  RECIPES,
  RecipeError,
  type RecipeName,
  readDistillation,
  renderPrompt,
} from "./recipes";
import { assertNoLeak, issueTitleOf, LeakError, renderCase } from "./render";
import { type ReplayedDocument, replayCase } from "./replay";
import { type NearestAnswer, nearestCases } from "./retrieval";
import {
  CACHE_VERSION,
  type CachedEvidence,
  cachePath,
  type Endpoint,
  evidenceDirectory,
  type JudgementRow,
  readEvidenceCache,
  rowOf,
  rowsPath,
} from "./round-io";

/**
 * Scores the round's twelve arms over one list of cases, in one resumable batch.
 *
 * The arms are the cross of the four recipes of `recipes.ts` and the three models of the plan
 * (dispatch://INFRA-2897/artifact/plan-of-analysis-md@v2, "The arms"), and every arm scores the
 * same cases: the fit, report and anchor slices of a draw. The runner refuses to start when two
 * arms would see different case lists, naming both, because an arm compared with another over
 * another set of cases is not compared at all.
 *
 * What is built once per case is the expensive part: the documents of the approval request, read
 * at the version it named, the issue's title, and the nearest training cases the many-shot
 * recipes carry. All three are a Dispatch read, so they are built once, shared by every arm and
 * every judgement of that case, and written to a cache under `--out`, which a resumed run reads
 * rather than fetching again. `assertNoLeak` runs over every rendered case before any arm is
 * asked, and a case that fails it stops the round naming the case.
 *
 * Each judgement carries its own seed, derived from the case id, the arm's name and the
 * judgement's index, and that seed decides the order of the evidence: the order of the documents
 * where a case has more than one, and the order of the many-shot examples. The seed is written in
 * the row, so one row's prompt can be built again exactly.
 *
 * **The resume invariant.** One row of `rows.jsonl` per (arm, case, judgement), never two. On
 * start the runner reads the rows already in `--out`, keeps the last row of each key, skips every
 * key whose row answered, and asks again every key whose row carries a fault. The row that comes
 * back from that second ask replaces the faulted one in the file rather than sitting beside it,
 * so a count over the file is a count of judgements and never of attempts at them.
 *
 * **A fault says why.** A fault's `faultDetail` is the answering body of the attempt that ended
 * the retry ladder, not its length and not a tally, bounded at the 2,000 characters `models.ts`
 * keeps, and `attempts` says how many tries there were. The summary and `round.json` carry, for
 * each fault kind and each arm, the count and one example detail, and the progress line prints a
 * new fault kind's first detail once, so a round that is failing says why while it runs.
 *
 * **No credential, anywhere.** The Jev arms spend a broker secret, which reaches only the
 * environment of the command the broker starts, so this runner must itself be that command:
 * `agent-secrets TYPESAFE_API_KEY --reason '...' --ttl 15m -- bun
 * scripts/approval-eval/run-round.ts ...`. The grant lives 15 minutes, which is what
 * `--deadline-seconds` is for: the run stops cleanly, flushes, prints what is left and exits 10,
 * and the loop of grants starts it again. Every fault detail passes `scrubSecrets` on all three
 * paths out of this process - the row appended to `rows.jsonl`, the example written into
 * `round.json`, and the line printed on stderr - and no row, line or file of this runner holds a
 * prompt, a document or a word of Dispatch text.
 */

/** How many judgements each case and arm gets where no `--k` says otherwise. */
const DEFAULT_JUDGEMENTS = 5;

/** How many judgements are in flight at once where no `--concurrency` says otherwise. */
const DEFAULT_CONCURRENCY = 8;

/** How often the run says where it has got to, on stderr. */
const PROGRESS_MS = 30_000;

/** The slices every arm scores, which are the three the plan's table reports from. */
export const SCORED_SLICES: readonly SliceName[] = ["fit", "report", "anchor"];

/** Every judgement this invocation planned, over the arms it named, has an answered row. */
const EXIT_FINISHED = 0;

/** The probe failed, so no arm was asked. */
const EXIT_PROBE_FAILED = 1;

/** The round could not start at all. */
const EXIT_CANNOT_RUN = 2;

/** The deadline stopped the run with judgements left: run it again. */
const EXIT_UNFINISHED = 10;

/** How many characters of a fault detail one printed line carries. */
const PROGRESS_DETAIL_CHARS = 400;

/** Where a judgement's second order is drawn from, so the examples do not follow the documents. */
const NEIGHBOUR_SEED_MIX = 0x9e3779b9;

const USAGE = `usage: bun scripts/approval-eval/run-round.ts --corpus <dir> --slices <dir> \\
  --out <dir> [--k 5] [--arms <file>] [--only <arm>]... [--concurrency <n>] \\
  [--deadline-seconds <n>] [--distillation <path>] [--probe-only] [--max-cases <n>]

  --corpus            a directory build-corpus.ts wrote
  --slices            a directory draw.ts wrote: slices.json and draw.json
  --out               where rows.jsonl, round.json and the evidence cache go, which must be
                      outside the repository workspace
  --k                 judgements per case and arm (default ${DEFAULT_JUDGEMENTS})
  --arms              a JSON file of arms, replacing the twelve built in
  --only              score this arm alone; repeat the flag for more than one
  --concurrency       judgements in flight at once (default ${DEFAULT_CONCURRENCY})
  --deadline-seconds  stop cleanly this many seconds after the start, and exit 10
  --distillation      the file the distilled and both recipes read
  --probe-only        make the probe of each endpoint and stop
  --max-cases         score the first n cases only. It exists for a smoke run of the whole path
                      and is never used for a scored round.

Exit status: 0 when every judgement this invocation planned was answered, 1 when the probe
failed, 2 when it could not run, and 10 when judgements it planned are left, which means run it
again.`;

/** One arm: a recipe, a model, the endpoint that serves it, and the slices it scores. */
export interface Arm {
  /** The name `--only` takes and every row carries. */
  name: string;
  recipe: RecipeName;
  model: string;
  endpoint: Endpoint;
  /** The slices this arm scores. Every arm of a round must name the same ones. */
  slices: readonly SliceName[];
}

/** One model of the round: the key its arms are named by, and where it is called. */
interface Model {
  key: string;
  model: string;
  endpoint: Endpoint;
}

/** The three models the plan names, two on the gateway and one on TypeSafe's own endpoint. */
const MODELS: readonly Model[] = [
  { key: "opus-5-5", model: "anthropic/claude-opus-5-5", endpoint: "claude" },
  { key: "fable-5-1", model: "anthropic/claude-fable-5-1", endpoint: "claude" },
  { key: "jev", model: "jev-latest", endpoint: "jev" },
];

/** The twelve arms of the round: the four recipes over the three models. */
export const ARMS: readonly Arm[] = Object.values(RECIPES).flatMap((recipe) =>
  MODELS.map((model) => ({
    name: `${recipe.name}-${model.key}`,
    recipe: recipe.name,
    model: model.model,
    endpoint: model.endpoint,
    slices: SCORED_SLICES,
  }))
);

/** The arms table is unreadable, or two arms would see different case lists. */
export class ArmsError extends Error {
  override readonly name = "ArmsError";
}

/** The round cannot start over these cases. */
export class RoundError extends Error {
  override readonly name = "RoundError";
}

/** The probe of an endpoint came back with a fault, so no arm was asked. */
export class ProbeError extends Error {
  override readonly name = "ProbeError";
}

/** One case of the scored set: the row, and which slice it was drawn into. */
export interface ScoredCase {
  row: CaseRow;
  slice: SliceName;
  kind: CaseKind;
}

/** What is read once per case and shared by every arm and every judgement of it. */
export interface CaseEvidence {
  /** The documents of the request, at the version it named. */
  documents: readonly ReplayedDocument[];
  /** The issue's title, which heads the rendering. */
  issueTitle: string | null;
  /** The nearest training cases, for the recipes that carry examples. Null where none are. */
  nearest: NearestAnswer | null;
}

/** What one probe call measured, which is the shape and the size the round then runs at. */
export interface ProbeRecord {
  endpoint: Endpoint;
  arm: string;
  model: string;
  caseId: string;
  promptBytes: number;
  /** What came back: the labels chosen, and whether the endpoint reported its own numbers. */
  chosen: string[];
  hasProbabilities: boolean;
  hasConfidence: boolean;
  inputTokens: number | null;
  outputTokens: number | null;
  latencyMs: number;
  attempts: number;
  /** Whether Jev cut the state to its token budget. Always false on the gateway. */
  stateCut: boolean;
  fault: FaultKind | null;
  faultDetail: string | null;
}

/** A count of faults with one example of what they said, which is what a reader acts on. */
export interface FaultCount {
  count: number;
  example: string | null;
}

/** What one round did, printed at the end and written as `round.json`. */
export interface RoundSummary {
  startedAt: string;
  finishedAt: string;
  wallMs: number;
  /** False when judgements this invocation planned have no answered row, which a rerun asks. */
  finished: boolean;
  cases: {
    total: number;
    bySlice: Record<string, Record<string, number>>;
    byKind: Record<string, number>;
  };
  arms: string[];
  models: string[];
  judgements: {
    /** This invocation's arms, times their cases, times `--k`. */
    planned: number;
    /** How many calls this run made. */
    asked: number;
    /** How many of those planned judgements hold an answered row, this run's or an earlier. */
    answered: number;
    left: number;
  };
  faults: {
    byKind: Record<string, FaultCount>;
    byArm: Record<string, FaultCount>;
  };
  tokens: Record<string, { calls: number; input: number; output: number }>;
  retrieval: Record<string, { routes: Record<string, number>; excluded: number }>;
  probes: ProbeRecord[];
}

/** What a round is asked to score, and under what bounds. */
export interface RoundPlan {
  /** Where `rows.jsonl`, `round.json` and the evidence cache go. */
  out: string;
  arms: readonly Arm[];
  cases: readonly ScoredCase[];
  judgements: number;
  concurrency: number;
  /** How many seconds the run has before it stops cleanly, and null for no deadline. */
  deadlineSeconds: number | null;
  /** The file the distilled and both recipes read. */
  distillationPath: string | null;
  /** Whether one call per endpoint is made before the arms are asked. */
  probe: boolean;
  /** Whether the run stops after that probe. */
  probeOnly: boolean;
}

/** What the round reads the world through, so a test can hand it hand-written answers. */
export interface RoundDeps {
  /** The documents, the title and the neighbours of one case, built once and cached. */
  evidenceOf(scored: ScoredCase): Promise<CaseEvidence>;
  askClaude(ask: ClaudeAsk): Promise<ModelAnswer>;
  askJev(ask: JevAsk): Promise<ModelAnswer>;
  /** The clock the deadline and the row stamps are read from. */
  now(): number;
  /** Where the progress lines go. */
  log(line: string): void;
}

/** What one round answered: its summary, and the exit status the command takes from it. */
export interface RoundResult {
  summary: RoundSummary;
  exitCode: number;
}

/** The flags, as the command reads them. */
interface Flags {
  corpus: string;
  slices: string;
  out: string;
  judgements: number;
  arms: string | undefined;
  only: string[];
  concurrency: number;
  deadlineSeconds: number | null;
  distillation: string | undefined;
  probeOnly: boolean;
  /** How many cases to score: a smoke run of the whole path, never a scored round. */
  maxCases: number | null;
}

/** The seed of one judgement: the case, the arm and the index, and nothing else. */
export function seedOf(caseId: string, arm: string, judgement: number): number {
  return createHash("sha256")
    .update(`${caseId}\n${arm}\n${judgement}`, "utf8")
    .digest()
    .readUInt32BE(0);
}

/** `items` in the order `seed` decides, by a Fisher-Yates walk of that one stream. */
export function inSeedOrder<T>(items: readonly T[], seed: number): T[] {
  const ordered = [...items];
  const next = randomStream(seed);
  for (let at = ordered.length - 1; at > 0; at -= 1) {
    const other = Math.floor(next() * (at + 1));
    const held = ordered[at] as T;
    ordered[at] = ordered[other] as T;
    ordered[other] = held;
  }
  return ordered;
}

/** The key of one judgement, which is what `rows.jsonl` holds exactly one row for. */
export function rowKey(arm: string, caseId: string, judgement: number): string {
  return `${arm}\u0000${caseId}\u0000${judgement}`;
}

/**
 * The rows written so far, one per (arm, case, judgement). A row for a key the file already
 * holds replaces that key's row, by rewriting the file, so a faulted judgement asked again
 * leaves one row and never two.
 */
export class RowStore {
  private readonly path: string;
  private readonly rows = new Map<string, JudgementRow>();
  private writing: Promise<void> = Promise.resolve();
  /** Whether a rewrite is queued and has not started, so another would write the same bytes. */
  private rewriteQueued = false;

  private constructor(path: string) {
    this.path = path;
  }

  /** The store at `path`, with every row already there read back and the file compacted. */
  static async open(path: string): Promise<RowStore> {
    const store = new RowStore(path);
    let text: string;
    try {
      text = await readFile(path, "utf8");
    } catch {
      return store;
    }
    let again = false;
    for (const line of text.split("\n")) {
      if (line.trim() === "") continue;
      const row = rowOf(line);
      if (row === null) {
        again = true;
        continue;
      }
      const key = rowKey(row.arm, row.caseId, row.judgement);
      if (store.rows.has(key)) again = true;
      store.rows.set(key, row);
    }
    if (again) await store.rewrite();
    return store;
  }

  /** Whether this judgement has a row that answered, which is the one a resume skips. */
  answered(key: string): boolean {
    const row = this.rows.get(key);
    return row !== undefined && row.fault === null;
  }

  /** Every row the file holds, in the order their keys were first written. */
  all(): JudgementRow[] {
    return [...this.rows.values()];
  }

  /** One row, appended where its key is new and written over its key where it is not. */
  async put(row: JudgementRow): Promise<void> {
    const key = rowKey(row.arm, row.caseId, row.judgement);
    const replaces = this.rows.has(key);
    this.rows.set(key, row);
    if (replaces) {
      // A rewrite already queued and not yet started serializes the map as it stands when it
      // runs, and this row is in that map already, so queuing a second one would write the very
      // same bytes twice. The flag is cleared as the queued one starts, before it reads the map.
      if (!this.rewriteQueued) {
        this.rewriteQueued = true;
        this.writing = this.writing.then(async () => {
          this.rewriteQueued = false;
          await this.rewrite();
        });
      }
      await this.writing;
      return;
    }
    this.writing = this.writing.then(async () => {
      await appendFile(this.path, `${JSON.stringify(row)}\n`, "utf8");
    });
    await this.writing;
  }

  /** The whole file written again from the rows held, through a temporary file and a rename. */
  private async rewrite(): Promise<void> {
    const text = this.all()
      .map((row) => `${JSON.stringify(row)}\n`)
      .join("");
    const temporary = `${this.path}.writing`;
    await writeFile(temporary, text, "utf8");
    await rename(temporary, this.path);
  }
}

/** The arms of a `--arms` file, which replaces the twelve built in. */
export function armsOf(parsed: unknown, where: string): Arm[] {
  const entries = Array.isArray(parsed) ? parsed : listOf(recordOf(parsed)?.arms);
  if (entries.length === 0) {
    throw new ArmsError(`${where} names no arm: it takes a JSON list of arms`);
  }
  const arms: Arm[] = [];
  const named = new Set<string>();
  for (const [at, entry] of entries.entries()) {
    const record = recordOf(entry);
    const name = stringOf(record?.name)?.trim();
    const recipe = stringOf(record?.recipe)?.trim();
    const model = stringOf(record?.model)?.trim();
    const endpoint = stringOf(record?.endpoint)?.trim();
    if (!name || !recipe || !model || !endpoint) {
      throw new ArmsError(`${where} arm ${at + 1} names no name, recipe, model or endpoint`);
    }
    if (named.has(name)) throw new ArmsError(`${where} names the arm ${name} twice`);
    named.add(name);
    if (!(recipe in RECIPES)) {
      throw new ArmsError(
        `${where} arm ${name} names the recipe ${recipe}, which is none of ` +
          `${Object.keys(RECIPES).join(", ")}`
      );
    }
    if (endpoint !== "claude" && endpoint !== "jev") {
      throw new ArmsError(
        `${where} arm ${name} names the endpoint ${endpoint}, which is neither claude nor jev`
      );
    }
    const slices = listOf(record?.slices).map((slice) => stringOf(slice)?.trim() ?? "");
    for (const slice of slices) {
      if (!SCORED_SLICES.includes(slice as SliceName)) {
        throw new ArmsError(
          `${where} arm ${name} names the slice ${slice}, which is none of ` +
            `${SCORED_SLICES.join(", ")}`
        );
      }
    }
    arms.push({
      name,
      recipe: recipe as RecipeName,
      model,
      endpoint,
      slices: slices.length === 0 ? SCORED_SLICES : (slices as SliceName[]),
    });
  }
  return arms;
}

/** The arms a `--only` keeps, in the order it names them. */
export function armsOnly(arms: readonly Arm[], only: readonly string[]): Arm[] {
  if (only.length === 0) return [...arms];
  const kept: Arm[] = [];
  for (const name of only) {
    const arm = arms.find((one) => one.name === name);
    if (arm === undefined) {
      throw new UsageError(
        `--only ${name} names no arm. The arms are ${arms.map((one) => one.name).join(", ")}`
      );
    }
    if (!kept.includes(arm)) kept.push(arm);
  }
  return kept;
}

/** The cases one arm scores: the scored set in its own order, kept to the slices the arm names. */
export function casesOfArm(arm: Arm, cases: readonly ScoredCase[]): ScoredCase[] {
  return cases.filter((scored) => arm.slices.includes(scored.slice));
}

/**
 * Every arm must see the same case list, and this throws `ArmsError` naming both arms where two
 * do not. An arm compared with another over another set of cases is not compared at all, and the
 * plan's "Failure-mode checks" makes this refusal the round's own.
 */
export function assertSameCases(arms: readonly Arm[], cases: readonly ScoredCase[]): void {
  const first = arms[0];
  if (first === undefined) {
    throw new ArmsError("a round scores at least one arm, and none was named");
  }
  const wanted = casesOfArm(first, cases);
  const wantedIds = wanted.map((scored) => scored.row.caseId).join("\n");
  for (const arm of arms.slice(1)) {
    const mine = casesOfArm(arm, cases);
    if (mine.map((scored) => scored.row.caseId).join("\n") === wantedIds) continue;
    throw new ArmsError(
      `the arms ${first.name} and ${arm.name} would see different case lists: ` +
        `${first.name} ${wanted.length} cases over the slices ${first.slices.join(", ")}, and ` +
        `${arm.name} ${mine.length} cases over the slices ${arm.slices.join(", ")}. ` +
        "Every arm of a round scores the same cases."
    );
  }
  if (wanted.length === 0) throw new ArmsError("the arms name slices holding no case");
}

/** The option labels one case offered, which is what an answer names. */
function labelsOf(row: CaseRow): string[] {
  const labels: string[] = [];
  for (const option of row.options) {
    const label = option.label?.trim();
    if (label && !labels.includes(label)) labels.push(label);
  }
  return labels;
}

/** The labels as Jev's named choices: a name it answers, the label it stands for, and why. */
function choicesOf(row: CaseRow): JevChoice[] {
  const choices: JevChoice[] = [];
  const taken = new Set<string>();
  for (const option of row.options) {
    const label = option.label?.trim();
    if (!label || choices.some((choice) => choice.label === label)) continue;
    const bare = label
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "_")
      .replace(/^_+|_+$/g, "");
    let name = bare === "" ? `option_${choices.length + 1}` : bare;
    while (taken.has(name)) name = `${name}_${choices.length + 1}`;
    taken.add(name);
    choices.push({ name, label, description: option.description?.trim() || label });
  }
  return choices;
}

/**
 * One judgement's prompt: the case rendered with its documents in this seed's order, and the
 * recipe's examples in this seed's own second order. The same seed renders the same prompt.
 */
export async function promptOf(
  arm: Arm,
  scored: ScoredCase,
  evidence: CaseEvidence,
  seed: number,
  distillationPath: string | null
): Promise<Prompt> {
  const documents =
    evidence.documents.length > 1 ? inSeedOrder(evidence.documents, seed) : [...evidence.documents];
  const text = renderCase(scored.row, documents, { issueTitle: evidence.issueTitle });
  const nearest =
    evidence.nearest === null
      ? undefined
      : {
          ...evidence.nearest,
          neighbours: inSeedOrder(evidence.nearest.neighbours, (seed ^ NEIGHBOUR_SEED_MIX) >>> 0),
        };
  return await renderPrompt(arm.recipe, scored.row, text, {
    distillationPath: distillationPath ?? undefined,
    nearest,
  });
}

/** One prompt put to the arm's own endpoint. */
async function askArm(
  arm: Arm,
  row: CaseRow,
  prompt: Prompt,
  deps: RoundDeps
): Promise<ModelAnswer> {
  if (arm.endpoint === "claude") {
    return await deps.askClaude({
      model: arm.model,
      prompt: { system: prompt.system, user: prompt.user, labels: labelsOf(row) },
    });
  }
  return await deps.askJev({
    model: arm.model,
    state: prompt.user,
    options: { instructions: prompt.system, choices: choicesOf(row) },
  });
}

/** One judgement's row, with its fault detail scrubbed before it is written. */
function rowOfAnswer(
  arm: Arm,
  scored: ScoredCase,
  judgement: number,
  seed: number,
  prompt: Prompt,
  answer: ModelAnswer,
  at: number
): JudgementRow {
  return {
    arm: arm.name,
    recipe: arm.recipe,
    model: arm.model,
    endpoint: arm.endpoint,
    caseId: scored.row.caseId,
    slice: scored.slice,
    kind: scored.kind,
    judgement,
    seed,
    verdict: answer.chosen,
    probabilities: answer.probabilities,
    confidence: answer.confidence,
    inputTokens: answer.inputTokens,
    outputTokens: answer.outputTokens,
    latencyMs: answer.latencyMs,
    attempts: answer.attempts,
    stateCut: answer.stateCut,
    retrievalRoute: prompt.record.route,
    examples: prompt.record.examples,
    excluded: prompt.record.excluded,
    promptBytes: prompt.record.bytes,
    fault: answer.fault,
    faultDetail: answer.faultDetail === null ? null : scrubSecrets(answer.faultDetail),
    at: new Date(at).toISOString(),
  };
}

/** One more fault of this name, keeping the first detail it carried as the example. */
function countFault(counts: Map<string, FaultCount>, name: string, detail: string | null): void {
  const held = counts.get(name);
  if (held === undefined) {
    counts.set(name, { count: 1, example: detail });
    return;
  }
  held.count += 1;
  if (held.example === null) held.example = detail;
}

/** The faults this run saw, by kind and by arm, each with the first detail it carried. */
class Faults {
  readonly byKind = new Map<string, FaultCount>();
  readonly byArm = new Map<string, FaultCount>();

  /** Counts one fault, and answers whether its kind is one this run had not seen before. */
  add(kind: FaultKind, arm: string, detail: string | null): boolean {
    const fresh = !this.byKind.has(kind);
    countFault(this.byKind, kind, detail);
    countFault(this.byArm, arm, detail);
    return fresh;
  }

  get total(): number {
    let total = 0;
    for (const one of this.byKind.values()) total += one.count;
    return total;
  }
}

/** A line every `PROGRESS_MS`, and the call that stops it. */
function everyProgress(deps: RoundDeps, line: () => string): () => void {
  const timer = setInterval(() => {
    deps.log(line());
  }, PROGRESS_MS);
  timer.unref?.();
  return () => {
    clearInterval(timer);
  };
}

/** How long, in the words a reader of a progress line reads at a glance. */
function duration(ms: number): string {
  const seconds = Math.max(0, Math.round(ms / 1_000));
  const hours = Math.floor(seconds / 3_600);
  const minutes = Math.floor((seconds % 3_600) / 60);
  if (hours > 0) return `${hours}h${String(minutes).padStart(2, "0")}m`;
  if (minutes > 0) return `${minutes}m${String(seconds % 60).padStart(2, "0")}s`;
  return `${seconds}s`;
}

/** One fault detail on one line, scrubbed again and bounded, as a printed line carries it. */
function oneLine(detail: string | null): string {
  if (detail === null) return "(no detail)";
  const flat = scrubSecrets(detail).replace(/\s+/g, " ").trim();
  return flat.length > PROGRESS_DETAIL_CHARS ? `${flat.slice(0, PROGRESS_DETAIL_CHARS)}...` : flat;
}

/** One judgement of one arm over one case, as the round walks them. */
interface Task {
  arm: Arm;
  scored: ScoredCase;
  judgement: number;
  key: string;
}

/** How large one case's documents are, which is what "the largest case" is read by. */
function sizeOf(scored: ScoredCase, evidence: ReadonlyMap<string, CaseEvidence>): number {
  const held = evidence.get(scored.row.caseId);
  if (held === undefined) return 0;
  let bytes = 0;
  for (const document of held.documents) bytes += document.bytes ?? 0;
  return bytes;
}

/** The one probe call of one endpoint, at the largest case of the scored set. */
async function probeEndpoint(
  arm: Arm,
  scored: ScoredCase,
  evidence: CaseEvidence,
  plan: RoundPlan,
  deps: RoundDeps
): Promise<ProbeRecord> {
  const seed = seedOf(scored.row.caseId, arm.name, 0);
  const prompt = await promptOf(arm, scored, evidence, seed, plan.distillationPath);
  const answer = await askArm(arm, scored.row, prompt, deps);
  return {
    endpoint: arm.endpoint,
    arm: arm.name,
    model: arm.model,
    caseId: scored.row.caseId,
    promptBytes: prompt.record.bytes,
    chosen: answer.chosen,
    hasProbabilities: answer.probabilities !== null,
    hasConfidence: answer.confidence !== null,
    inputTokens: answer.inputTokens,
    outputTokens: answer.outputTokens,
    latencyMs: answer.latencyMs,
    attempts: answer.attempts,
    stateCut: answer.stateCut,
    fault: answer.fault,
    faultDetail: answer.faultDetail === null ? null : scrubSecrets(answer.faultDetail),
  };
}

/**
 * One real call per endpoint, at the largest case of the scored set and on that endpoint's
 * heaviest arm, so the round learns the answer shape, the usage, the latency and whether Jev cut
 * the state before thousands of calls are made on those assumptions. A probe that faults stops
 * the round.
 */
async function probeEndpoints(
  plan: RoundPlan,
  evidence: ReadonlyMap<string, CaseEvidence>,
  deps: RoundDeps
): Promise<ProbeRecord[]> {
  // Among the cases whose evidence this run holds, not among every case of the plan: a resume
  // builds evidence only for the cases it still has work for, so the plan's own largest case is
  // usually not one of them, and looking the probe up there refuses a resume that has work to do.
  // Each case's size is read once and the first of any tie is kept, which is what the sort this
  // replaced did: it compared every pair and so sized each case many times over.
  let largest: ScoredCase | undefined;
  let largestBytes = -1;
  for (const scored of plan.cases) {
    if (!evidence.has(scored.row.caseId)) continue;
    const bytes = sizeOf(scored, evidence);
    if (bytes > largestBytes) {
      largestBytes = bytes;
      largest = scored;
    }
  }
  const held = largest === undefined ? undefined : evidence.get(largest.row.caseId);
  if (largest === undefined || held === undefined) {
    throw new ProbeError("no case of the scored set had its evidence built, so none can be probed");
  }
  const records: ProbeRecord[] = [];
  for (const endpoint of ["claude", "jev"] as const) {
    const arms = plan.arms.filter((arm) => arm.endpoint === endpoint);
    if (arms.length === 0) continue;
    let heaviest = arms[0] as Arm;
    let heaviestBytes = -1;
    for (const arm of arms) {
      const prompt = await promptOf(
        arm,
        largest,
        held,
        seedOf(largest.row.caseId, arm.name, 0),
        plan.distillationPath
      );
      if (prompt.record.bytes > heaviestBytes) {
        heaviestBytes = prompt.record.bytes;
        heaviest = arm;
      }
    }
    const record = await probeEndpoint(heaviest, largest, held, plan, deps);
    deps.log(
      `probe ${record.endpoint.padEnd(7)}${record.arm} on ${record.caseId}: ` +
        `${record.promptBytes} prompt bytes, ${record.latencyMs}ms, ${record.attempts} attempt(s), ` +
        `in ${record.inputTokens ?? "?"} out ${record.outputTokens ?? "?"}, chose ` +
        `${record.chosen.join(" | ") || "nothing"}` +
        `${record.hasProbabilities ? ", with probabilities" : ""}` +
        `${record.hasConfidence ? ", with confidence" : ""}` +
        `${record.endpoint === "jev" ? `, state cut ${record.stateCut}` : ""}`
    );
    if (record.fault !== null) {
      throw new ProbeError(
        `the ${record.endpoint} probe on ${record.arm} failed with the ${record.fault} fault ` +
          `after ${record.attempts} attempt(s): ${oneLine(record.faultDetail)}`
      );
    }
    records.push(record);
  }
  return records;
}

/** How many judgements of each arm have no answered row, which is what a resume would ask. */
function leftByArm(plan: RoundPlan, store: RowStore): Map<string, number> {
  const left = new Map<string, number>();
  for (const arm of plan.arms) {
    let count = 0;
    for (const scored of casesOfArm(arm, plan.cases)) {
      for (let judgement = 0; judgement < plan.judgements; judgement += 1) {
        if (!store.answered(rowKey(arm.name, scored.row.caseId, judgement))) count += 1;
      }
    }
    left.set(arm.name, count);
  }
  return left;
}

/** One progress line: what is done, what is left, how fast, and when it would finish. */
function progressLine(
  asked: number,
  tasks: number,
  faults: Faults,
  startedAt: number,
  now: number
): string {
  const minutes = Math.max((now - startedAt) / 60_000, 1 / 60);
  const rate = asked / minutes;
  const left = Math.max(0, tasks - asked);
  const finish =
    rate > 0 ? new Date(now + (left / rate) * 60_000).toISOString() : "not yet measurable";
  return (
    `progress  asked ${asked}, left ${left}, faults ${faults.total}, ` +
    `${rate.toFixed(1)}/min, finish ~${finish}`
  );
}

/**
 * The summary of one round: what this invocation planned, and what the store answers of it.
 *
 * Planned and answered are both read over the judgements this invocation planned, which is its
 * own arms over their own cases at its own `--k`, and each of those counts as answered when
 * the store holds an answered row for it, whether this run wrote it or an earlier one did. A
 * round is run one arms file at a time, so the store a later invocation opens holds every
 * earlier invocation's rows: counting answered over all of them against a planned read over
 * this invocation's arms alone reports more answered than planned and calls a complete round
 * unfinished, which is what the exit status and `finished` below follow from. The per-arm
 * records beside them, the tokens, the retrieval routes and the faults, stay over every row
 * the store holds, so the last summary of a round still says what the whole round spent.
 */
function summaryOf(
  plan: RoundPlan,
  store: RowStore,
  probes: readonly ProbeRecord[],
  asked: number,
  startedAt: number,
  finishedAt: number
): RoundSummary {
  const bySlice: Record<string, Record<string, number>> = {};
  const byKind: Record<string, number> = {};
  for (const scored of plan.cases) {
    bySlice[scored.slice] ??= {};
    const slice = bySlice[scored.slice] as Record<string, number>;
    slice[scored.kind] = (slice[scored.kind] ?? 0) + 1;
    byKind[scored.kind] = (byKind[scored.kind] ?? 0) + 1;
  }
  const planned = plan.arms.reduce(
    (total, arm) => total + casesOfArm(arm, plan.cases).length * plan.judgements,
    0
  );
  const tokens: Record<string, { calls: number; input: number; output: number }> = {};
  const retrieval: Record<string, { routes: Record<string, number>; excluded: number }> = {};
  const byKindFaults = new Map<string, FaultCount>();
  const byArmFaults = new Map<string, FaultCount>();
  // Every judgement this invocation planned that the store does not answer, by arm.
  const left = leftByArm(plan, store);
  const answered = planned - [...left.values()].reduce((total, count) => total + count, 0);
  for (const row of store.all()) {
    tokens[row.arm] ??= { calls: 0, input: 0, output: 0 };
    const held = tokens[row.arm] as { calls: number; input: number; output: number };
    held.calls += 1;
    held.input += row.inputTokens ?? 0;
    held.output += row.outputTokens ?? 0;
    retrieval[row.arm] ??= { routes: {}, excluded: 0 };
    const route = retrieval[row.arm] as { routes: Record<string, number>; excluded: number };
    const name = row.retrievalRoute ?? "none";
    route.routes[name] = (route.routes[name] ?? 0) + 1;
    route.excluded += row.excluded ?? 0;
    if (row.fault === null) continue;
    const detail = row.faultDetail === null ? null : scrubSecrets(row.faultDetail);
    countFault(byKindFaults, row.fault, detail);
    countFault(byArmFaults, row.arm, detail);
  }
  return {
    startedAt: new Date(startedAt).toISOString(),
    finishedAt: new Date(finishedAt).toISOString(),
    wallMs: finishedAt - startedAt,
    finished: planned - answered === 0,
    cases: { total: plan.cases.length, bySlice, byKind },
    arms: plan.arms.map((arm) => arm.name),
    models: [...new Set(plan.arms.map((arm) => arm.model))],
    judgements: { planned, asked, answered, left: planned - answered },
    faults: {
      byKind: Object.fromEntries(byKindFaults),
      byArm: Object.fromEntries(byArmFaults),
    },
    tokens,
    retrieval,
    probes: [...probes],
  };
}

/**
 * The round itself: the evidence built once per case and held to `assertNoLeak`, the probe, and
 * then every judgement of every arm that has no answered row yet.
 */
export async function runRound(plan: RoundPlan, deps: RoundDeps): Promise<RoundResult> {
  assertSameCases(plan.arms, plan.cases);
  const startedAt = deps.now();
  await mkdir(plan.out, { recursive: true });
  const store = await RowStore.open(rowsPath(plan.out));
  const tasks: Task[] = [];
  for (const scored of plan.cases) {
    for (const arm of plan.arms) {
      if (!arm.slices.includes(scored.slice)) continue;
      for (let judgement = 0; judgement < plan.judgements; judgement += 1) {
        const key = rowKey(arm.name, scored.row.caseId, judgement);
        if (store.answered(key)) continue;
        tasks.push({ arm, scored, judgement, key });
      }
    }
  }
  // A probe measures what the round is about to spend, so a run with nothing left to ask makes
  // none: a resume that asks nothing calls no endpoint at all.
  const probing = plan.probeOnly || (plan.probe && tasks.length > 0);
  const wanted = new Set(tasks.map((task) => task.scored.row.caseId));
  const needed = plan.probeOnly
    ? [...plan.cases]
    : plan.cases.filter((scored) => wanted.has(scored.row.caseId));
  const evidence = new Map<string, CaseEvidence>();
  let built = 0;
  const stopEvidence = everyProgress(
    deps,
    () => `evidence  built ${built} of ${needed.length} cases`
  );
  try {
    await pool(needed, plan.concurrency, async (scored) => {
      const one = await deps.evidenceOf(scored);
      assertNoLeak(
        scored.row,
        renderCase(scored.row, one.documents, { issueTitle: one.issueTitle })
      );
      evidence.set(scored.row.caseId, one);
      built += 1;
    });
  } finally {
    stopEvidence();
  }
  deps.log(`evidence  built ${built} of ${needed.length} cases`);
  const probes = probing ? await probeEndpoints(plan, evidence, deps) : [];
  if (plan.probeOnly) {
    const summary = summaryOf(plan, store, probes, 0, startedAt, deps.now());
    await writeFile(join(plan.out, "round.json"), `${JSON.stringify(summary, null, 2)}\n`, "utf8");
    return { summary, exitCode: EXIT_FINISHED };
  }
  const faults = new Faults();
  const deadlineAt =
    plan.deadlineSeconds === null ? null : startedAt + plan.deadlineSeconds * 1_000;
  let asked = 0;
  const stopJudgements = everyProgress(deps, () =>
    progressLine(asked, tasks.length, faults, startedAt, deps.now())
  );
  try {
    await pool(
      tasks,
      plan.concurrency,
      async (task) => {
        const held = evidence.get(task.scored.row.caseId);
        if (held === undefined) {
          throw new RoundError(`no evidence was built for the case ${task.scored.row.caseId}`);
        }
        const seed = seedOf(task.scored.row.caseId, task.arm.name, task.judgement);
        const prompt = await promptOf(task.arm, task.scored, held, seed, plan.distillationPath);
        const answer = await askArm(task.arm, task.scored.row, prompt, deps);
        const row = rowOfAnswer(
          task.arm,
          task.scored,
          task.judgement,
          seed,
          prompt,
          answer,
          deps.now()
        );
        await store.put(row);
        asked += 1;
        if (row.fault !== null && faults.add(row.fault, row.arm, row.faultDetail)) {
          deps.log(
            `fault ${row.fault} on ${row.arm}, the first of its kind: ${oneLine(row.faultDetail)}`
          );
        }
      },
      () => deadlineAt !== null && deps.now() >= deadlineAt
    );
  } finally {
    stopJudgements();
  }
  const finishedAt = deps.now();
  const summary = summaryOf(plan, store, probes, asked, startedAt, finishedAt);
  await writeFile(join(plan.out, "round.json"), `${JSON.stringify(summary, null, 2)}\n`, "utf8");
  if (!summary.finished) {
    // What is left is the deadline's doing where it stopped the walk before its end, and the
    // faults' doing where every task was asked, and a reader of the line must not have to guess.
    const stopped = deadlineAt !== null && asked < tasks.length;
    deps.log(
      stopped
        ? `deadline  stopped after ${duration(finishedAt - startedAt)} with ` +
            `${summary.judgements.left} judgement(s) left; run it again`
        : `faults    ${summary.judgements.left} judgement(s) of ${tasks.length} asked carry a ` +
            "fault and have no answer; run it again to ask them again"
    );
    for (const [arm, left] of leftByArm(plan, store)) {
      if (left > 0) deps.log(`          ${arm} ${left} left`);
    }
  }
  return { summary, exitCode: summary.finished ? EXIT_FINISHED : EXIT_UNFINISHED };
}

/** The summary as the command prints it. */
function summaryLines(summary: RoundSummary): string[] {
  const lines: string[] = [];
  const slices = Object.entries(summary.cases.bySlice)
    .map(
      ([slice, kinds]) => `${slice} ${Object.values(kinds).reduce((one, other) => one + other, 0)}`
    )
    .join(", ");
  const kinds = Object.entries(summary.cases.byKind)
    .map(([kind, count]) => `${kind} ${count}`)
    .join(", ");
  lines.push(`cases             ${summary.cases.total} (${slices}; ${kinds})`);
  lines.push(
    `arms              ${summary.arms.length} over ${summary.models.length} model(s): ` +
      `${summary.arms.join(", ")}`
  );
  lines.push(
    `judgements        planned ${summary.judgements.planned}, asked here ` +
      `${summary.judgements.asked}, answered ${summary.judgements.answered}, left ` +
      `${summary.judgements.left}`
  );
  lines.push(`wall              ${duration(summary.wallMs)}`);
  const kindFaults = Object.entries(summary.faults.byKind);
  lines.push(`faults            ${kindFaults.length === 0 ? "none" : ""}`);
  for (const [kind, one] of kindFaults) {
    lines.push(`  by kind ${kind.padEnd(14)}${one.count}  e.g. ${oneLine(one.example)}`);
  }
  for (const [arm, one] of Object.entries(summary.faults.byArm)) {
    lines.push(`  by arm  ${arm.padEnd(22)}${one.count}  e.g. ${oneLine(one.example)}`);
  }
  lines.push("tokens by arm");
  for (const [arm, one] of Object.entries(summary.tokens)) {
    lines.push(
      `  ${arm.padEnd(22)}${String(one.calls).padStart(6)} rows  in ${one.input}  out ${one.output}`
    );
  }
  lines.push("retrieval by arm");
  for (const [arm, one] of Object.entries(summary.retrieval)) {
    const routes = Object.entries(one.routes)
      .map(([route, count]) => `${route} ${count}`)
      .join(", ");
    lines.push(`  ${arm.padEnd(22)}${routes}, excluded ${one.excluded}`);
  }
  for (const probe of summary.probes) {
    lines.push(
      `probe ${probe.endpoint.padEnd(7)}${probe.arm}, ${probe.promptBytes} prompt bytes, ` +
        `${probe.latencyMs}ms, in ${probe.inputTokens ?? "?"} out ${probe.outputTokens ?? "?"}, ` +
        `chose ${probe.chosen.join(" | ") || "nothing"}` +
        `${probe.endpoint === "jev" ? `, state cut ${probe.stateCut}` : ""}`
    );
  }
  return lines;
}

/**
 * The cache of one case, or null where there is none of this case at this version, or where
 * the one there holds too few examples. A miss is no failure here: the runner reads the
 * evidence again and writes it over whatever was there.
 */
async function readCache(
  out: string,
  caseId: string,
  examples: number,
  corpus: Corpus
): Promise<CaseEvidence | null> {
  const { cache } = await readEvidenceCache(out, caseId);
  if (cache === null || !(cache.examples >= examples)) return null;
  if (cache.nearest === null) {
    return examples === 0
      ? { documents: cache.documents, issueTitle: cache.issueTitle, nearest: null }
      : null;
  }
  const neighbours = [];
  for (const one of cache.nearest.neighbours) {
    const row = corpus.byCaseId.get(one.caseId);
    if (row === undefined) return null;
    neighbours.push({ row, place: one.place, score: one.score });
  }
  return {
    documents: cache.documents,
    issueTitle: cache.issueTitle,
    nearest: {
      neighbours,
      route: cache.nearest.route,
      excluded: cache.nearest.excluded,
      degraded: cache.nearest.degraded,
      allowed: cache.nearest.allowed,
      pages: cache.nearest.pages,
    },
  };
}

/** The evidence of one case: the cache where it holds, and a Dispatch read where it does not. */
async function cachedEvidenceOf(
  scored: ScoredCase,
  out: string,
  corpus: Corpus,
  cut: string,
  examples: number,
  config: DispatchConfig
): Promise<CaseEvidence> {
  const path = cachePath(out, scored.row.caseId);
  const held = await readCache(out, scored.row.caseId, examples, corpus);
  if (held !== null) return held;
  const replayed = await replayCase(scored.row, config);
  const issueTitle =
    scored.row.issueKey === null ? null : await issueTitleOf(scored.row.issueKey, config);
  const nearest =
    examples === 0
      ? null
      : await nearestCases(corpus.rows, scored.row, { count: examples, cut, config });
  const cache: CachedEvidence = {
    version: CACHE_VERSION,
    caseId: scored.row.caseId,
    builtAt: new Date().toISOString(),
    issueTitle,
    documents: replayed.documents,
    examples,
    nearest:
      nearest === null
        ? null
        : {
            route: nearest.route,
            excluded: nearest.excluded,
            degraded: nearest.degraded,
            allowed: nearest.allowed,
            pages: nearest.pages,
            neighbours: nearest.neighbours.map((one) => ({
              caseId: one.row.caseId,
              place: one.place,
              score: one.score,
            })),
          },
  };
  await mkdir(evidenceDirectory(out), { recursive: true });
  await writeFile(path, `${JSON.stringify(cache)}\n`, "utf8");
  return { documents: replayed.documents, issueTitle, nearest };
}

/** The scored cases of a draw: the fit, report and anchor slices, in the order they were drawn. */
async function scoredCasesOf(slices: string, corpus: Corpus): Promise<ScoredCase[]> {
  const path = join(slices, "slices.json");
  let parsed: unknown;
  try {
    parsed = JSON.parse(await readFile(path, "utf8"));
  } catch {
    throw new RoundError(`${path} could not be read: --slices takes a directory draw.ts wrote`);
  }
  const record = recordOf(parsed);
  const cases: ScoredCase[] = [];
  for (const slice of SCORED_SLICES) {
    const ids = listOf(record?.[slice]).map((id) => stringOf(id) ?? "");
    if (ids.length === 0) throw new RoundError(`${path} names no case in the ${slice} slice`);
    for (const caseId of ids) {
      const row = corpus.byCaseId.get(caseId);
      if (row === undefined) {
        throw new RoundError(`${path} names the case ${caseId}, which this corpus does not hold`);
      }
      if (labelsOf(row).length < 2) {
        throw new RoundError(
          `the case ${caseId} offers fewer than two labelled options, so no arm can judge it`
        );
      }
      cases.push({ row, slice, kind: row.kind });
    }
  }
  return cases;
}

/** The cut the draw was made at, which the retrieval's dating rule reads. */
async function cutOf(slices: string): Promise<string> {
  const path = drawPath(slices);
  const draw = await readDraw(path);
  if (draw === null) {
    throw new RoundError(`${path} could not be read: --slices takes a directory draw.ts wrote`);
  }
  const cut = cutOfDraw(draw);
  if (cut === null) {
    throw new RoundError(`${path} names no cut, so no dating rule can be applied`);
  }
  return cut;
}

/** One whole-number flag, at or above `least`. */
function whole(value: string | undefined, flag: string, fallback: number, least: number): number {
  if (value === undefined) return fallback;
  const number = Number(value.trim());
  if (!Number.isSafeInteger(number) || number < least) {
    throw new UsageError(`${flag} takes a whole number of ${least} or above\n\n${USAGE}`);
  }
  return number;
}

function readFlags(argv: readonly string[]): Flags {
  let values: {
    corpus?: string;
    slices?: string;
    out?: string;
    k?: string;
    arms?: string;
    only?: string[];
    concurrency?: string;
    "deadline-seconds"?: string;
    distillation?: string;
    "probe-only"?: boolean;
    "max-cases"?: string;
  };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        corpus: { type: "string" },
        slices: { type: "string" },
        out: { type: "string" },
        k: { type: "string" },
        arms: { type: "string" },
        only: { type: "string", multiple: true },
        concurrency: { type: "string" },
        "deadline-seconds": { type: "string" },
        distillation: { type: "string" },
        "probe-only": { type: "boolean" },
        "max-cases": { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const corpus = values.corpus?.trim();
  const slices = values.slices?.trim();
  const out = values.out?.trim();
  if (!corpus || !slices || !out) throw new UsageError(USAGE);
  return {
    corpus,
    slices,
    out,
    judgements: whole(values.k, "--k", DEFAULT_JUDGEMENTS, 1),
    arms: values.arms?.trim() || undefined,
    only: values.only ?? [],
    concurrency: whole(values.concurrency, "--concurrency", DEFAULT_CONCURRENCY, 1),
    deadlineSeconds:
      values["deadline-seconds"] === undefined
        ? null
        : whole(values["deadline-seconds"], "--deadline-seconds", 0, 1),
    distillation: values.distillation?.trim() || undefined,
    probeOnly: values["probe-only"] === true,
    maxCases:
      values["max-cases"] === undefined ? null : whole(values["max-cases"], "--max-cases", 0, 1),
  };
}

/** The round as the command runs it: the real corpus, the real draw and the real endpoints. */
async function round(flags: Flags): Promise<number> {
  const corpus = await readCorpus(flags.corpus);
  const out = await outDirectory(flags.out);
  const table =
    flags.arms === undefined
      ? ARMS
      : armsOf(JSON.parse(await readFile(flags.arms, "utf8")), flags.arms);
  const arms = armsOnly(table, flags.only);
  if (arms.some((arm) => arm.endpoint === "jev")) assertJevKeyPresent();
  if (arms.some((arm) => RECIPES[arm.recipe].distillation) && flags.distillation === undefined) {
    throw new UsageError(
      "--distillation names the file the distilled and both recipes read, and none was named"
    );
  }
  if (flags.distillation !== undefined) {
    // Read now rather than at the first judgement: a path that has moved is worth a refusal
    // before hours of evidence are built over it. The text read here is the text the prompts
    // use, so the check costs nothing and the file cannot move between it and the first use.
    try {
      await readDistillation(flags.distillation);
    } catch {
      throw new UsageError(`--distillation ${flags.distillation} could not be read`);
    }
  }
  const examples = arms.reduce((most, arm) => Math.max(most, RECIPES[arm.recipe].examples), 0);
  const cut = await cutOf(flags.slices);
  const drawn = await scoredCasesOf(flags.slices, corpus);
  const cases = flags.maxCases === null ? drawn : drawn.slice(0, flags.maxCases);
  const config = await dispatchConfig();
  const plan: RoundPlan = {
    out,
    arms,
    cases,
    judgements: flags.judgements,
    concurrency: flags.concurrency,
    deadlineSeconds: flags.deadlineSeconds,
    distillationPath: flags.distillation ?? null,
    probe: true,
    probeOnly: flags.probeOnly,
  };
  const deps: RoundDeps = {
    evidenceOf: (scored) => cachedEvidenceOf(scored, out, corpus, cut, examples, config),
    askClaude,
    askJev,
    now: () => Date.now(),
    log: (line) => {
      console.error(line);
    },
  };
  console.log(`corpus            ${corpus.directory}`);
  console.log(`slices            ${flags.slices}`);
  console.log(`out               ${out}`);
  console.log(`arms              ${arms.map((arm) => arm.name).join(", ")}`);
  console.log(
    `cases             ${cases.length}${flags.maxCases === null ? "" : " (--max-cases: a smoke run)"}`
  );
  console.log(`judgements        ${flags.judgements} per case and arm`);
  const result = await runRound(plan, deps);
  console.log("");
  for (const line of summaryLines(result.summary)) console.log(line);
  console.log("");
  console.log(`written           ${rowsPath(out)}`);
  console.log(`                  ${join(out, "round.json")}`);
  return result.exitCode;
}

async function main(argv: readonly string[]): Promise<number> {
  let flags: Flags;
  try {
    flags = readFlags(argv);
  } catch (error) {
    if (!(error instanceof UsageError)) throw error;
    console.error(error.message);
    return EXIT_CANNOT_RUN;
  }
  try {
    return await round(flags);
  } catch (error) {
    if (error instanceof ProbeError) {
      console.error(`${error.name}: ${error.message}`);
      return EXIT_PROBE_FAILED;
    }
    if (
      error instanceof UsageError ||
      error instanceof OutputPathError ||
      error instanceof CorpusError ||
      error instanceof ArmsError ||
      error instanceof RoundError ||
      error instanceof LeakError ||
      error instanceof RecipeError ||
      error instanceof GatewayError ||
      error instanceof JevKeyError ||
      error instanceof DispatchConfigError ||
      error instanceof DispatchRefusedError ||
      error instanceof DispatchUnreachableError
    ) {
      console.error(`${error.name}: ${error.message}`);
      return EXIT_CANNOT_RUN;
    }
    throw error;
  }
}

// Only when run: a test that imports `runRound` must not run a command.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
