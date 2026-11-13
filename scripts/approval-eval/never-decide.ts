import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { CaseRow } from "./build-corpus";
import { artifactsOf } from "./corpus-io";
import { askClaude, type ClaudeAsk, type ModelAnswer } from "./models";

/**
 * What the agent never decides, whatever its confidence.
 *
 * This is requirement Q7 of dispatch://INFRA-2897/spec. The rule itself is prose, in
 * `skills/approval-review/references/never-decide.md`, and that file is the only statement of
 * it: it is a reference file of the shipped skill, and this module applies those same bytes by
 * asking a model. One definition, one reading, so the coverage the eval reports is the coverage
 * the skill can deliver.
 *
 * It was a list of regular-expression signals before. The signals were repaired against the 80
 * committed fixtures and reached ten in ten there, and held 15 of 60 of a held-out set written
 * by an agent that had never seen them: they had learned those fixtures, and a longer list would
 * have learned the next set the same way. They are gone. Nothing here matches a phrase.
 *
 * One kind is settled without a call. A root carrying the `legion` label that stands on the
 * backlog or in icebox is a fact of the issue - its parent, its label, its status, which
 * document is its primary - and not a reading of any text, so it is decided from `IssueFacts`
 * before the model is asked and no model is asked to judge it. The caller hands those facts in,
 * because the rendered case does not carry them and this module reads no Dispatch.
 *
 * The call's answer must be readable: one line naming one of the seven kinds or `none`, and a
 * second line giving the reason in the model's own words. An answer in any other shape is a
 * fault, reported as a fault, and never quietly turned into a decide - the one thing a rule
 * about what must not be decided cannot do is decide by accident.
 *
 * The answers are cached, because one draw puts this question to 985 cases and a round must be
 * reproducible without paying for it twice. The key is the SHA-256 of the prose, the model id,
 * the document's id, the document's version, the issue's parent, its status and its sorted
 * labels, and the whole rendered evidence of the case, which is `cacheKey` below: a case is a
 * document at a version under the facts it was asked about, and a later version, or another
 * set of facts, is a different reading. A change of one byte of the prose changes every key by
 * construction, which is what makes the cache safe: no entry can outlive the definition it was
 * answered under.
 */

/**
 * Every kind the agent never decides, in the order the call offers them as labels. Nothing
 * tries them one after another: the one call is offered all seven and `none`, and names exactly
 * one of them. `waiting-legion-root` is settled from the issue's own facts before any call is
 * made, so a call is asked only about a case those facts did not settle, and an answer naming
 * that kind is read as that kind all the same.
 */
export const NEVER_DECIDE_KINDS = [
  "waiting-legion-root",
  "changes-these-skills",
  "destructive-act",
  "credential",
  "owner-only-setting",
  "outside-message",
  "outside-account",
] as const;

/** The named kinds of request the agent never decides. */
export type NeverDecideKind = (typeof NEVER_DECIDE_KINDS)[number];

/** The model the rule is put to where no caller names another. */
export const DEFAULT_NEVER_DECIDE_MODEL = "anthropic/claude-opus-5-5";

/**
 * The prose that is the rule: the reference file of the skill that ships it, named as the
 * repository holds it. A record carries this path, so it says which file was applied without
 * carrying the path of the machine that ran it.
 */
export const NEVER_DECIDE_DEFINITION_PATH = "skills/approval-review/references/never-decide.md";

/** That same file in this checkout, which is where the prose is read from. */
const DEFINITION_FILE = fileURLToPath(
  new URL(`../../${NEVER_DECIDE_DEFINITION_PATH}`, import.meta.url)
);

/** The statuses at which a root waits for a person rather than running, and how to say each. */
const WAITING_AT: Readonly<Record<string, string>> = {
  backlog: "on the backlog",
  icebox: "in icebox",
};

/** What the answer's first line may name: the seven kinds, and the word for none of them. */
const NONE = "none";
const LABELS: readonly string[] = [...NEVER_DECIDE_KINDS, NONE];

/** What the call is told beside the prose: the shape its answer must take to be readable. */
const ANSWER_SHAPE = [
  "## How to answer",
  "",
  "Answer in exactly two lines, and write nothing else: no heading, no preamble, no list.",
  "",
  "The first line is one of these words alone, with nothing before or after it:",
  "",
  ...LABELS.map((label) => `    ${label}`),
  "",
  "The second line is one sentence saying why, in your own words: what the document asks for",
  "that makes it that kind, naming the line it asks it in; or, where the first line is `none`,",
  "why what the document names is only mentioned, described or reported rather than asked for.",
].join("\n");

/**
 * How long one of these calls has, and how many tokens its answer may take. Two lines cost
 * under a hundred tokens, but this gateway writes a thinking block before them on a large case
 * and a budget that runs out inside that block leaves a body with no text at all, which this
 * rule reads as a fault (`DEFAULT_MAX_TOKENS` in `models.ts` measures that). The budget bounds
 * an answer rather than costing it, so it is set well past what an answer takes.
 */
const BOUNDS = { timeMs: 180_000, maxTokens: 8_192 };

/** What the rule was told about the issue, which the rendered case does not carry. */
export interface IssueFacts {
  /** The parent issue's key, and null for a root. */
  parent: string | null;
  /** The issue's labels, among which `legion` makes it a Legion tree. */
  labels: readonly string[];
  /** The issue's status, of which `backlog` and `icebox` wait for a person. */
  status: string;
  /** The id of the issue's primary document, which an approval of admits the root. */
  primaryArtifactId: string | null;
}

/** One case, or one document, put to the rule. */
export interface NeverDecideInput {
  /** The evidence text as `renderCase` renders it. */
  text: string;
  /** The case being judged, where there is one: its documents say which artifact is asked about. */
  row?: CaseRow | null;
  /** The issue's own facts, which only the waiting-root kind reads. */
  issue?: IssueFacts | null;
  /** The id of the document being judged, which the cache key takes. */
  documentId?: string | null;
  /** The version of that document, since a later version is a different reading. */
  documentVersion?: number | null;
}

/** Where one answer came from: the issue's facts, a call, or an answer a call made before. */
export type NeverDecideSource = "issue-facts" | "model" | "cache";

/** What the rule says about one case: whether it is held, under which kind, and why. */
export interface NeverDecideAnswer {
  held: boolean;
  kind: NeverDecideKind | null;
  /** Why, in the model's own words, or in the issue's facts for the kind that takes no call. */
  reason: string;
  /** The model's whole answer, kept so a disagreement is read later rather than asked again. */
  answer: string | null;
  source: NeverDecideSource;
  /** Why the answer could not be read, and null where it was read. A fault is never a decide. */
  fault: string | null;
  /** What this call read and wrote, and null where no call was paid for. */
  inputTokens: number | null;
  outputTokens: number | null;
}

/** How the rule asks a model, which a test hands in so that it makes no call. */
export type AskModel = (ask: ClaudeAsk) => Promise<ModelAnswer>;

/** What a caller may change about how the rule is applied. */
export interface NeverDecideOptions {
  /** The model the question is put to. */
  model?: string;
  /** Where answers are kept between runs, and null or absent to keep none. */
  cacheDirectory?: string | null;
  /** The asking function, which defaults to the gateway. */
  ask?: AskModel;
  /** The prose of the rule, where the caller holds it already rather than reading the file. */
  definition?: string;
}

/** The prose of the rule, read once per process. */
let definitionRead: Promise<string> | null = null;

/** The prose that is the rule, as the shipped skill carries it. */
export function neverDecideDefinition(): Promise<string> {
  definitionRead ??= readFile(DEFINITION_FILE, "utf8");
  return definitionRead;
}

/**
 * Whether this case is an approval of the primary document of a root carrying the `legion` label
 * that waits for a person, which the daemon reads as the person's own instruction to run that
 * tree. The issue's facts come from the caller; a case with none is not held under this kind.
 */
function waitingLegionRoot(input: NeverDecideInput): string | null {
  const issue = input.issue;
  if (!issue) return null;
  if (issue.parent !== null) return null;
  if (!issue.labels.includes("legion")) return null;
  const waitingAt = WAITING_AT[issue.status];
  if (waitingAt === undefined) return null;
  const primary = issue.primaryArtifactId;
  if (primary === null) return null;
  const row = input.row;
  // With a case in hand, the hold stands only on an approval of that primary document. With a
  // bare document, the issue's facts alone hold it, which is the conservative reading.
  if (row !== null && row !== undefined) {
    if (row.kind !== "approval") return null;
    if (!artifactsOf(row).has(primary)) return null;
  }
  return (
    `the issue is a root carrying the legion label, ${waitingAt}, ` +
    "and this is its primary document"
  );
}

/** What the call is told about the issue, where the caller named any of it. */
function factsOf(input: NeverDecideInput): string[] {
  const issue = input.issue;
  const lines: string[] = [];
  if (issue) {
    lines.push(
      `- The issue is ${issue.parent === null ? "a root" : `a child of ${issue.parent}`}, ` +
        `at status ${issue.status}, with labels ${issue.labels.join(", ") || "none"}.`
    );
  }
  if (input.documentId != null) {
    const version = input.documentVersion == null ? "" : ` at version ${input.documentVersion}`;
    lines.push(`- The document asked about is ${input.documentId}${version}.`);
  }
  return lines;
}

/** One case as the call is given it: what is known about the issue, then the document itself. */
function userOf(input: NeverDecideInput): string {
  const facts = factsOf(input);
  const head = facts.length === 0 ? [] : ["What is known about the issue:", "", ...facts, ""];
  return [...head, "The document, whole:", "", input.text].join("\n");
}

/**
 * The key one answer is kept under, which a change of one byte of the prose changes. The issue's
 * facts are in the key because `factsOf` puts them in the prompt: two askings of one document
 * under different facts are different questions and must not collide. Adding them invalidates
 * every entry written before, by construction, which is correct and cheap: the entries that
 * matter are asked again once.
 */
function cacheKey(definition: string, model: string, input: NeverDecideInput): string {
  const issue = input.issue;
  const parts = [
    definition,
    model,
    input.documentId ?? "",
    input.documentVersion === null || input.documentVersion === undefined
      ? ""
      : String(input.documentVersion),
    issue?.parent ?? "",
    issue?.status ?? "",
    [...(issue?.labels ?? [])].sort().join("\u001f"),
    input.text,
  ];
  return createHash("sha256").update(parts.join("\u0000"), "utf8").digest("hex");
}

/** What is kept of one answer, which is the whole of it and not only its verdict. */
interface CacheEntry {
  model: string;
  documentId: string | null;
  documentVersion: number | null;
  answeredAt: string;
  held: boolean;
  kind: NeverDecideKind | null;
  reason: string;
  /** The model's answer as it came back, so a disagreement is read rather than asked again. */
  answer: string;
}

/** The answer kept under `key`, and null where none is kept or what is kept cannot be read. */
async function readCache(directory: string, key: string): Promise<CacheEntry | null> {
  let text: string;
  try {
    text = await readFile(join(directory, `${key}.json`), "utf8");
  } catch {
    return null;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return null;
  }
  if (typeof parsed !== "object" || parsed === null) return null;
  const entry = parsed as Partial<CacheEntry>;
  if (typeof entry.reason !== "string" || typeof entry.answer !== "string") return null;
  if (typeof entry.held !== "boolean") return null;
  const kind = entry.kind ?? null;
  if (kind !== null && !NEVER_DECIDE_KINDS.includes(kind)) return null;
  return {
    model: entry.model ?? "",
    documentId: entry.documentId ?? null,
    documentVersion: entry.documentVersion ?? null,
    answeredAt: entry.answeredAt ?? "",
    held: entry.held,
    kind,
    reason: entry.reason,
    answer: entry.answer,
  };
}

/** Keeps one readable answer under `key`. A fault is never kept: it is asked again. */
async function writeCache(directory: string, key: string, entry: CacheEntry): Promise<void> {
  await mkdir(directory, { recursive: true });
  await writeFile(join(directory, `${key}.json`), `${JSON.stringify(entry, null, 2)}\n`, "utf8");
}

/** What the rule read out of one call: the kind it named, its reason, or why neither could be. */
interface Read {
  held: boolean;
  kind: NeverDecideKind | null;
  reason: string;
  answer: string;
  fault: string | null;
}

/** The answer in no readable shape, with what came back kept so a reader sees it. */
function faulted(detail: string, answer: string): Read {
  return {
    held: false,
    kind: null,
    reason: `the rule could not be applied: ${detail}`,
    answer,
    fault: detail,
  };
}

/**
 * One call's answer read as the rule: the first line names the kind, which `askClaude` matched
 * against the labels, and the lines after it are the reason. An answer naming no kind, naming
 * two, or giving no reason is a fault and not a decide.
 */
function readNeverDecideAnswer(answer: ModelAnswer): Read {
  const text = answer.text ?? answer.faultDetail ?? "";
  if (answer.fault !== null) return faulted(`${answer.fault}: ${text}`.trim(), text);
  if (answer.chosen.length !== 1) {
    return faulted(
      `the answer named ${answer.chosen.length} of the kinds, and must name one`,
      text
    );
  }
  const chosen = answer.chosen[0] ?? "";
  const lines = text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
  const reason = lines.slice(1).join(" ").trim();
  if (reason === "") return faulted("the answer named a kind and gave no reason for it", text);
  if (chosen === NONE) return { held: false, kind: null, reason, answer: text, fault: null };
  const kind = NEVER_DECIDE_KINDS.find((one) => one === chosen) ?? null;
  if (kind === null) return faulted(`the answer named ${chosen}, which is no kind`, text);
  return { held: true, kind, reason, answer: text, fault: null };
}

/** The gateway, which is what the rule asks where the caller hands in no asking function. */
const askTheGateway: AskModel = (ask) => askClaude(ask);

/**
 * What the rule says about one case or one document: the issue's facts first, for the one kind
 * they settle, and then one call putting the prose and this document to the model.
 */
export async function neverDecide(
  input: NeverDecideInput,
  options: NeverDecideOptions = {}
): Promise<NeverDecideAnswer> {
  const waiting = waitingLegionRoot(input);
  if (waiting !== null) {
    return {
      held: true,
      kind: "waiting-legion-root",
      reason: waiting,
      answer: null,
      source: "issue-facts",
      fault: null,
      inputTokens: null,
      outputTokens: null,
    };
  }
  const definition = options.definition ?? (await neverDecideDefinition());
  const model = options.model ?? DEFAULT_NEVER_DECIDE_MODEL;
  const directory = options.cacheDirectory ?? null;
  // The key is a hash over the whole evidence, so it is worth nothing where nothing is kept.
  const key = directory === null ? "" : cacheKey(definition, model, input);
  if (directory !== null) {
    const kept = await readCache(directory, key);
    if (kept !== null) {
      return {
        held: kept.held,
        kind: kept.kind,
        reason: kept.reason,
        answer: kept.answer,
        source: "cache",
        fault: null,
        // Nothing was read or written for this one: the call it came from was paid for before.
        inputTokens: null,
        outputTokens: null,
      };
    }
  }
  const ask = options.ask ?? askTheGateway;
  const answer = await ask({
    model,
    prompt: { system: `${definition}\n\n${ANSWER_SHAPE}\n`, user: userOf(input), labels: LABELS },
    bounds: BOUNDS,
  });
  const read = readNeverDecideAnswer(answer);
  if (read.fault === null && directory !== null) {
    await writeCache(directory, key, {
      model,
      documentId: input.documentId ?? null,
      documentVersion: input.documentVersion ?? null,
      answeredAt: new Date().toISOString(),
      held: read.held,
      kind: read.kind,
      reason: read.reason,
      answer: read.answer,
    });
  }
  return {
    held: read.held,
    kind: read.kind,
    reason: read.reason,
    answer: read.answer,
    source: "model",
    fault: read.fault,
    inputTokens: answer.inputTokens,
    outputTokens: answer.outputTokens,
  };
}
