/**
 * The runner's readers (the `read` command's prompts), and the one way each prompt of `prompts/`
 * reaches the model, which judge.ts takes too.
 *
 * A prompt file, `prompts/<name>.md`, is a header and a text. The header, between two `---` lines,
 * pins every setting of the model call, one `<key>: <value>` per line: `model`, `thinking_budget`
 * (the thinking budget in tokens), `max_tokens`, `temperature` (1, the only value the Messages API
 * takes with thinking on) and `effort` (`output_config.effort`). Each request made with the prompt
 * sends all five (`promptBody`), so no setting is left to the provider's default, and a request
 * log of the bodies shows each one. The text is the system prompt.
 *
 * A reader is one prompt with a zod schema of its input and one of its answer: one plain Messages
 * API call through the gateway per input batch (`runReader`), whose answer is the JSON object the
 * reply's text holds (whole, or in one fenced block). A reply whose answer does not parse, or that
 * the schema refuses, is asked again, READER_TRIES times in all; each refusal is kept and returned,
 * so a run can count them (a names answer refused for a reason that is only that the job is not
 * clear from the name alone says NAME_ONLY_REFUSAL). An answer's schema is built for its input
 * (`Reader.answer`), so it also refuses an answer that names an id, a file, a line or a message the
 * input does not hold.
 *
 * The readers, each with the input it reads and the answer it gives:
 * - `case`: the lines of a skill's base text a case may name (those the session's own read
 *   of each file held, each with its number in the file; a file the session did not read is not
 *   shown) and the session around the skill's read; the first step the skill governs, its message
 *   index, the line it exercises, a criterion stating pass and fail, a violating step (a plausible
 *   next step that breaks the line and otherwise looks valid, which the judge must fail), and each
 *   message before the step that states the line's instruction in any wording, with words quoted
 *   from it; or no step. The case builder drops a proposal whose line is not a whole line of the
 *   base text, or of the session's read of its file, or that names a restatement.
 * - `restate`: a case's line and one window of the messages before its step (a long message
 *   in parts); each message that states the line's instruction in any wording, with words quoted
 *   from it. The case builder reads every message before the step this way, window by window, and
 *   drops a case on any.
 * - `audit`: a case's line, the messages before its step as the judge reads them, and its
 *   real step, never its criterion; whether the step follows the line, breaks it, or is not a
 *   step the line governs, with a reason. The case builder keeps a case only when it follows.
 * - `map`: a skill's base and cut texts and the rule-by-rule audit of the cut, if any; each rule
 *   of the base text with its line in the cut text or why it is gone, called kept, narrowed,
 *   widened or lost, with the audit's words where the audit answers it.
 * - `attack`: a skill's cut text; each vague phrase, undefined term, rule nobody can check and
 *   line that reads two ways, by file and line.
 * - `same-job`: every skill's id, repository, name and description; each pair whose
 *   descriptions give the same task to load them for.
 * - `sentence`: skills' SKILL.md texts; each place one holds the sentence "disagrees with the
 *   code" in any wording.
 * - `names`: every skill's name and description, its load test's fault if any, and the names
 *   in use; each skill nominated (with its risk, the task, the reason and one to three new names)
 *   or not. Each skill with a fault is nominated.
 * - `tasks`: a skill's name and description and first user messages of real sessions; search
 *   terms, and at least TASKS_PER_LABEL need and near-miss tasks, each labelled, each taken from a
 *   message by its id or written and marked written. Each need task says whether the skill governs
 *   its first step (`firstStep`: an agent doing it would read the skill at once, before any other
 *   act), and at least TASKS_PER_LABEL need tasks are ones it governs; the schema refuses a need
 *   task that does not say. In `control` mode each message is a need task of the positive control
 *   (a session whose first assistant message read the skill): the answer takes every message, once,
 *   as a need task, and says only whether the skill governs its first step.
 *
 * The positive controls of the map, attack, same-job and sentence readers (`runControl`) each
 * plant a known finding in a copy of real input, run the reader once, and hold only when its
 * answer names that finding.
 */
import { readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { textLines } from "./cases";
import type { Gateway, MessagesBody, ReplyMessage, Spent } from "./gateway";
import { TaskLabel } from "./score";

/** The directory of the prompt files. */
export const PROMPTS_DIR = path.join(import.meta.dir, "prompts");
/** The most times a reader asks for one batch before it fails. */
export const READER_TRIES = 3;
/** The fewest tasks of each label a load test takes: at least 5 need and 5 near-miss. */
export const TASKS_PER_LABEL = 5;
/** What a names answer is refused for when a nomination's reason or task says only that the job is
 *  not clear from the name alone; a run counts the refusals that hold it. */
export const NAME_ONLY_REFUSAL = "says only that the job is not clear from the name alone";

/** The settings a prompt file's header pins. With thinking on, the Messages API takes temperature
 *  1 only, and a thinking budget of 1024 tokens or more, below `max_tokens`. */
const PromptSettings = z
  .strictObject({
    model: z.string().min(1),
    thinking_budget: z.int().min(1024),
    max_tokens: z.int().positive(),
    temperature: z.literal(1),
    effort: z.enum(["low", "medium", "high", "max"]),
  })
  .refine((settings) => settings.thinking_budget < settings.max_tokens, {
    message: "is not below max_tokens",
    path: ["thinking_budget"],
  });
type PromptSettings = z.infer<typeof PromptSettings>;

/** A prompt file: its name (the file's, less `.md`), the settings its header pins, and its text. */
export interface Prompt {
  readonly name: string;
  readonly settings: PromptSettings;
  readonly text: string;
}

const HEADER = /^---\n([\s\S]*?)\n---\n/;
const HEADER_LINE = /^([a-z_]+): *(\S(?:.*\S)?) *$/;
const NUMBER = /^-?\d+(?:\.\d+)?$/;

/** The issues of `list` under the path `at`. An answer no option of a union takes is read as the
 *  option it came closest to, the one with the fewest issues, so the refusal names what is
 *  wrong. */
function issueLines(list: readonly z.core.$ZodIssue[], at: readonly PropertyKey[]): string[] {
  return list.flatMap((issue) => {
    const where = [...at, ...issue.path];
    if (issue.code === "invalid_union" && issue.errors.length > 0) {
      const closest = issue.errors.reduce((best, each) =>
        each.length < best.length ? each : best
      );
      return issueLines(closest, where);
    }
    return [`${where.map(String).join(".") || "the value"}: ${issue.message}`];
  });
}

/** Each issue of `error`, as `<path>: <message>`, joined. */
function issues(error: z.ZodError): string {
  return issueLines(error.issues, []).join("; ");
}

/** The prompt file `file` (the text of `prompts/<name>.md`). Throws naming the file when its header
 *  is missing, holds a line that is not `<key>: <value>`, gives a key twice, or does not pin each
 *  setting of PromptSettings and nothing else, or when no text follows it. */
export function parsePrompt(name: string, file: string): Prompt {
  const where = `prompts/${name}.md`;
  const header = HEADER.exec(file);
  if (header === null) {
    throw new Error(
      `${where} has no header: a line "---", one "<key>: <value>" per setting, and a line "---"`
    );
  }
  const fields: Record<string, string | number> = {};
  for (const line of (header[1] ?? "").split("\n")) {
    const field = HEADER_LINE.exec(line);
    if (field === null) throw new Error(`${where}: a header line is not "<key>: <value>": ${line}`);
    const [, key = "", value = ""] = field;
    if (key in fields) throw new Error(`${where}: the header gives ${key} twice`);
    fields[key] = NUMBER.test(value) ? Number(value) : value;
  }
  const settings = PromptSettings.safeParse(fields);
  if (!settings.success)
    throw new Error(`${where}: the header is refused: ${issues(settings.error)}`);
  const text = file.slice(header[0].length).trim();
  if (text === "") throw new Error(`${where} holds no prompt text after its header`);
  return { name, settings: settings.data, text };
}

const prompts = new Map<string, Prompt>();

/** The prompt `prompts/<name>.md`, read once per process. */
export function loadPrompt(name: string): Prompt {
  let prompt = prompts.get(name);
  if (prompt === undefined) {
    prompt = parsePrompt(name, readFileSync(path.join(PROMPTS_DIR, `${name}.md`), "utf8"));
    prompts.set(name, prompt);
  }
  return prompt;
}

/** One text block of a request's user message. `cache` marks it for the prompt cache: a part that
 *  every request of a batch shares, sent before the part that differs. */
interface UserPart {
  readonly text: string;
  readonly cache?: boolean;
}

/** The request body of one call with `prompt`: its pinned settings, the prompt as the system
 *  prompt, and `parts` as the one user message. The reply streams, and the gateway reads it
 *  whole. */
export function promptBody(prompt: Prompt, parts: readonly UserPart[]): MessagesBody {
  const { model, thinking_budget, max_tokens, temperature, effort } = prompt.settings;
  return {
    model,
    max_tokens,
    temperature,
    thinking: { type: "enabled", budget_tokens: thinking_budget },
    output_config: { effort },
    stream: true,
    system: [{ type: "text", text: prompt.text }],
    messages: [
      {
        role: "user",
        content: parts.map(({ text, cache }) =>
          cache === true
            ? { type: "text", text, cache_control: { type: "ephemeral" } }
            : { type: "text", text }
        ),
      },
    ],
  };
}

/** The text a reply's text blocks hold, joined; its thinking is no part of it. */
function answerText(message: ReplyMessage): string {
  return message.content
    .flatMap((block) =>
      block.type === "text" && typeof block.text === "string" ? [block.text] : []
    )
    .join("\n");
}

const FENCED = /^```(?:json)?[ \t]*\n([\s\S]*?)\n```$/;

/** The JSON value an answer's text holds: the text whole, or the one fenced block it is. Throws
 *  when it holds none. */
function answerJson(text: string): unknown {
  const trimmed = text.trim();
  const json = FENCED.exec(trimmed)?.[1] ?? trimmed;
  try {
    return JSON.parse(json);
  } catch {
    const shown = trimmed.length > 200 ? `${trimmed.slice(0, 200)}…` : trimmed;
    throw new Error(`the answer is not one JSON value: ${JSON.stringify(shown)}`);
  }
}

/** `value` as `schema` parses it. Throws naming each issue. */
export function parsedAnswer<T>(schema: z.ZodType<T>, value: unknown): T {
  const parsed = schema.safeParse(value);
  if (!parsed.success) throw new Error(`the answer is refused: ${issues(parsed.error)}`);
  return parsed.data;
}

/** What asking got: the first answer that parsed, or undefined when none did; why each reply before
 *  it was refused; and what each reply used, in the order sent. */
interface Asked<T> {
  readonly answer: T | undefined;
  readonly refusals: readonly string[];
  readonly spent: readonly Spent[];
}

/** Sends `body` and reads the reply's answer as `schema` parses its JSON (`answerJson`,
 *  `parsedAnswer`), sending it again while that throws, `tries` times in all. A send that rejects
 *  (the gateway's BudgetSpent among them) rejects. */
export async function ask<T>(
  body: MessagesBody,
  schema: z.ZodType<T>,
  gateway: Gateway,
  tries: number
): Promise<Asked<T>> {
  const refusals: string[] = [];
  const spent: Spent[] = [];
  for (let n = 0; n < tries; n += 1) {
    const reply = await gateway.send(body);
    spent.push(reply.spent);
    try {
      return {
        answer: parsedAnswer(schema, answerJson(answerText(reply.message))),
        refusals,
        spent,
      };
    } catch (error) {
      const why = messageFor(error);
      const stop = reply.message.stop_reason;
      refusals.push(stop === "end_turn" ? why : `${why} (the reply stopped at ${stop})`);
    }
  }
  return { answer: undefined, refusals, spent };
}

/** A reader: the schema of its input, the schema of its answer to an input, and its input as the
 *  user message. */
export interface Reader<I, A> {
  readonly input: z.ZodType<I>;
  answer(input: I): z.ZodType<A>;
  render(input: I): string;
}

/** A file of a skill: its path inside the skill's directory, and its text. */
const File = z.strictObject({ path: z.string().min(1), text: z.string() });
type File = z.infer<typeof File>;
/** An id the caller gives an item of a batch, which the answer names it by. */
const Id = z.string().min(1);
/** A file and a line of it, from 1. */
const Place = z.strictObject({ file: z.string().min(1), line: z.int().positive() });
type Place = z.infer<typeof Place>;
/** One line of text: not blank, no line break. */
const OneLine = z
  .string()
  .refine((text) => text.trim() !== "" && !text.includes("\n"), "is not one non-blank line");

/** Adds the issue `message` at `at` to `ctx`. */
function refuse(ctx: z.RefinementCtx, at: PropertyKey[], message: string): void {
  ctx.addIssue({ code: "custom", path: at, message });
}

/** Refuses each id of `items` given twice. */
function uniqueIds(items: readonly { id: string }[], ctx: z.RefinementCtx, at: PropertyKey): void {
  const seen = new Set<string>();
  for (const [index, { id }] of items.entries()) {
    if (seen.has(id)) refuse(ctx, [at, index, "id"], `gives the id ${id} twice`);
    seen.add(id);
  }
}

/** The number of lines of each file, by its path, as `numbered` numbers them. */
function lineCounts(files: readonly File[]): Map<string, number> {
  return new Map(files.map((file) => [file.path, textLines(file.text).length]));
}

/** Refuses `place` when its file is no file of `files` or its line is past the file's last. */
function checkPlace(
  ctx: z.RefinementCtx,
  at: PropertyKey[],
  place: Place,
  counts: ReadonlyMap<string, number>,
  text: string
): void {
  const count = counts.get(place.file);
  if (count === undefined) refuse(ctx, [...at, "file"], `is no file of the ${text}`);
  else if (place.line > count) {
    refuse(
      ctx,
      [...at, "line"],
      `is past the last line, ${count}, of ${place.file} in the ${text}`
    );
  }
}

/** A text with each line numbered from 1, `<number>: <line>`. */
function numbered(text: string): string {
  return textLines(text)
    .map((line, index) => `${index + 1}: ${line}`)
    .join("\n");
}

/** Each file as `<file path="...">`, its lines numbered. */
function fileBlocks(files: readonly File[]): string {
  return files
    .map((file) => `<file path="${file.path}">\n${numbered(file.text)}\n</file>`)
    .join("\n\n");
}

// The case reader.

/** A message of a transcript as the case reader reads it: its index in the session, its role, and
 *  its text (judge.ts's `renderMessage`, with the skill's read results left out). */
const TranscriptMessage = z.strictObject({
  index: z.int().nonnegative(),
  role: z.string().min(1),
  text: z.string(),
});
/** A file of the skill as the case reader is offered it: its path, and only the lines of it a case
 *  may name (the base text's lines the session's own read of the file held), each with its number
 *  in the file. */
const OfferedFile = z.strictObject({
  path: z.string().min(1),
  lines: z.array(z.strictObject({ number: z.int().positive(), text: OneLine })).min(1),
});
export const CaseInput = z.strictObject({
  skill: z.string().min(1),
  files: z.array(OfferedFile).min(1),
  messages: z.array(TranscriptMessage).min(1),
});
export type CaseInput = z.infer<typeof CaseInput>;
/** A step the case reader writes: its text (empty when it holds only tool calls) and its tool
 *  calls, each a tool's name and its arguments, as the transcript shows a step. */
const WrittenStep = z
  .strictObject({
    text: z.string(),
    toolCalls: z.array(
      z.strictObject({
        name: z.string().min(1),
        arguments: z.record(z.string(), z.unknown()),
      })
    ),
  })
  .refine((step) => step.text.trim() !== "" || step.toolCalls.length > 0, {
    message: "holds no text and no tool call",
  });
/** The messages before the step that state the line's instruction in other words, each with the
 *  words quoted from it: the case reader's and the restatement reader's answer. */
const Restatements = z.array(
  z.strictObject({ messageIndex: z.int().nonnegative(), quote: z.string().min(1) })
);
const CaseProposal = z.strictObject({
  step: z.strictObject({ messageIndex: z.int().nonnegative(), what: z.string().min(1) }),
  line: z.strictObject({ file: z.string().min(1), text: OneLine }),
  criterion: z.strictObject({ pass: z.string().min(1), fail: z.string().min(1) }),
  violatingStep: WrittenStep,
  restatements: Restatements,
});
const NoCase = z.strictObject({ step: z.null(), reason: z.string().min(1) });
export const CaseAnswer = z.union([CaseProposal, NoCase]);
export type CaseAnswer = z.infer<typeof CaseAnswer>;

/** `text` as a restatement's quote is looked for in it: a JSON string's escapes of a quote mark,
 *  a backslash and a line break read as those characters, each run of white space as one space,
 *  in lower case. A message's tool calls are rendered with their arguments as JSON. */
function looseText(text: string): string {
  return text
    .replaceAll("\\n", " ")
    .replaceAll('\\"', '"')
    .replaceAll("\\\\", "\\")
    .replace(/\s+/g, " ")
    .trim()
    .toLowerCase();
}

/** What the case reader's `<skill>` says of the lines it shows. */
const OFFERED_NOTE =
  "Only the lines a case may name are shown: the lines of each file that the agent's own read of that file held, each with its number in the file. A line not shown cannot be a case's line, and a file not shown was not read.";

const caseReader: Reader<CaseInput, CaseAnswer> = {
  input: CaseInput,
  answer: (input) =>
    CaseAnswer.superRefine((answer, ctx) => {
      if (answer.step === null) return;
      const step = answer.step.messageIndex;
      const message = input.messages.find((each) => each.index === step);
      if (message === undefined) {
        refuse(ctx, ["step", "messageIndex"], "is no message index of the transcript");
      } else if (message.role !== "assistant") {
        refuse(
          ctx,
          ["step", "messageIndex"],
          `is a ${message.role} message, not an assistant message`
        );
      }
      if (!input.files.some((file) => file.path === answer.line.file)) {
        refuse(ctx, ["line", "file"], "is no file of the skill shown");
      }
      // A restatement is a message before the step that holds the words quoted from it.
      for (const [index, { messageIndex, quote }] of answer.restatements.entries()) {
        const at = ["restatements", index];
        const restating = input.messages.find((each) => each.index === messageIndex);
        if (restating === undefined) {
          refuse(ctx, [...at, "messageIndex"], "is no message index of the transcript");
        } else if (messageIndex >= step) {
          refuse(ctx, [...at, "messageIndex"], `is not before the step, message ${step}`);
        } else if (!looseText(restating.text).includes(looseText(quote))) {
          refuse(ctx, [...at, "quote"], `is not words of message ${messageIndex}`);
        }
      }
    }),
  render: (input) =>
    [
      `<skill name="${input.skill}">\n${OFFERED_NOTE}\n\n${input.files
        .map(
          (file) =>
            `<file path="${file.path}">\n${file.lines.map((line) => `${line.number}: ${line.text}`).join("\n")}\n</file>`
        )
        .join("\n\n")}\n</skill>`,
      `<transcript>\n${input.messages
        .map(
          (message) =>
            `<message index="${message.index}" role="${message.role}">\n${message.text}\n</message>`
        )
        .join("\n\n")}\n</transcript>`,
    ].join("\n\n"),
};

// The restatement reader: the whole history before a case's step, window by window.

/** One window of the messages before a case's step. A message longer than a window comes in
 *  parts, each under the message's index, its text opening with a note of which part it is. */
const RestateInput = z.strictObject({
  line: OneLine,
  messages: z.array(TranscriptMessage).min(1),
});
type RestateInput = z.infer<typeof RestateInput>;
export const RestateAnswer = z.strictObject({ restatements: Restatements });
export type RestateAnswer = z.infer<typeof RestateAnswer>;

const restateReader: Reader<RestateInput, RestateAnswer> = {
  input: RestateInput,
  answer: (input) =>
    RestateAnswer.superRefine((answer, ctx) => {
      // A restatement names a message of the window, and its quote is words of that message
      // (of one of its parts).
      for (const [index, { messageIndex, quote }] of answer.restatements.entries()) {
        const at = ["restatements", index];
        const parts = input.messages.filter((each) => each.index === messageIndex);
        if (parts.length === 0) {
          refuse(ctx, [...at, "messageIndex"], "is no message index of the window");
        } else if (!parts.some((part) => looseText(part.text).includes(looseText(quote)))) {
          refuse(ctx, [...at, "quote"], `is not words of message ${messageIndex}`);
        }
      }
    }),
  render: (input) =>
    [
      `<line>\n${input.line}\n</line>`,
      `<messages>\n${input.messages
        .map(
          (message) =>
            `<message index="${message.index}" role="${message.role}">\n${message.text}\n</message>`
        )
        .join("\n\n")}\n</messages>`,
    ].join("\n\n"),
};

// The audit reader: the real step against the line, without the criterion.

/** A case's line, the messages before its step as the judge reads them (judge.ts, `beforeText`),
 *  and its real step (judge.ts, `stepText`). Never the case's criterion. */
const AuditInput = z.strictObject({
  line: OneLine,
  before: z.string(),
  step: z.string().min(1),
});
type AuditInput = z.infer<typeof AuditInput>;
const AuditVerdict = z.enum(["follows", "breaks", "not-governed"]);
export const AuditAnswer = z.strictObject({ verdict: AuditVerdict, reason: z.string().min(1) });
export type AuditAnswer = z.infer<typeof AuditAnswer>;

const auditReader: Reader<AuditInput, AuditAnswer> = {
  input: AuditInput,
  answer: () => AuditAnswer,
  render: (input) =>
    [
      `<line>\n${input.line}\n</line>`,
      `<before>\n${input.before}\n</before>`,
      `<step>\n${input.step}\n</step>`,
    ].join("\n\n"),
};

// The map reader.

export const MapInput = z.strictObject({
  skill: z.string().min(1),
  base: z.array(File).min(1),
  /** Empty when the cut deleted the skill. */
  cut: z.array(File),
  /** The rule-by-rule audit's text for this skill, or null when it has none. */
  audit: z.string().min(1).nullable(),
});
export type MapInput = z.infer<typeof MapInput>;
const RuleStatus = z.enum(["kept", "narrowed", "widened", "lost"]);
const MapRule = z
  .strictObject({
    file: z.string().min(1),
    line: z.int().positive(),
    rule: z.string().min(1),
    status: RuleStatus,
    cut: Place.nullable(),
    reason: z.string().min(1),
    audit: z.string().min(1).nullable(),
  })
  .refine((rule) => (rule.status === "lost") === (rule.cut === null), {
    message: "a lost rule has no place in the cut text, and every other rule has one",
    path: ["cut"],
  });
const MapAnswer = z.strictObject({ rules: z.array(MapRule).min(1) });
type MapAnswer = z.infer<typeof MapAnswer>;

const mapReader: Reader<MapInput, MapAnswer> = {
  input: MapInput,
  answer: (input) =>
    MapAnswer.superRefine((answer, ctx) => {
      const base = lineCounts(input.base);
      const cut = lineCounts(input.cut);
      for (const [index, rule] of answer.rules.entries()) {
        checkPlace(ctx, ["rules", index], rule, base, "base text");
        if (rule.cut !== null) checkPlace(ctx, ["rules", index, "cut"], rule.cut, cut, "cut text");
        if (input.audit === null && rule.audit !== null) {
          refuse(ctx, ["rules", index, "audit"], "cites an audit, and none was given");
        }
      }
    }),
  render: (input) =>
    [
      `<base>\n${fileBlocks(input.base)}\n</base>`,
      `<cut>\n${input.cut.length === 0 ? "The cut deleted the skill: it has no file." : fileBlocks(input.cut)}\n</cut>`,
      `<audit>\n${input.audit ?? "None."}\n</audit>`,
    ].join("\n\n"),
};

// The attack reader.

export const AttackInput = z.strictObject({
  skill: z.string().min(1),
  files: z.array(File).min(1),
});
export type AttackInput = z.infer<typeof AttackInput>;
const FindingKind = z.enum(["vague", "undefined-term", "uncheckable", "two-readings"]);
const AttackAnswer = z.strictObject({
  findings: z.array(
    z.strictObject({
      kind: FindingKind,
      file: z.string().min(1),
      line: z.int().positive(),
      quote: z.string().min(1),
      why: z.string().min(1),
    })
  ),
});
type AttackAnswer = z.infer<typeof AttackAnswer>;

const attackReader: Reader<AttackInput, AttackAnswer> = {
  input: AttackInput,
  answer: (input) =>
    AttackAnswer.superRefine((answer, ctx) => {
      const counts = lineCounts(input.files);
      for (const [index, finding] of answer.findings.entries()) {
        checkPlace(ctx, ["findings", index], finding, counts, "skill");
      }
    }),
  render: (input) => `<skill name="${input.skill}">\n${fileBlocks(input.files)}\n</skill>`,
};

// The same-job reader.

export const SameJobInput = z
  .strictObject({
    skills: z
      .array(
        z.strictObject({
          id: Id,
          repo: z.string().min(1),
          name: z.string().min(1),
          description: z.string(),
        })
      )
      .min(2),
  })
  .superRefine((input, ctx) => uniqueIds(input.skills, ctx, "skills"));
export type SameJobInput = z.infer<typeof SameJobInput>;
const SameJobAnswer = z.strictObject({
  pairs: z.array(z.strictObject({ a: Id, b: Id, task: z.string().min(1), why: z.string().min(1) })),
});
type SameJobAnswer = z.infer<typeof SameJobAnswer>;

const sameJobReader: Reader<SameJobInput, SameJobAnswer> = {
  input: SameJobInput,
  answer: (input) =>
    SameJobAnswer.superRefine((answer, ctx) => {
      const ids = new Set(input.skills.map((skill) => skill.id));
      for (const [index, pair] of answer.pairs.entries()) {
        for (const side of ["a", "b"] as const) {
          if (!ids.has(pair[side])) refuse(ctx, ["pairs", index, side], "is no id of the skills");
        }
        if (pair.a === pair.b) refuse(ctx, ["pairs", index, "b"], "pairs a skill with itself");
      }
    }),
  render: (input) =>
    `<skills>\n${input.skills
      .map(
        (skill) =>
          `<skill id="${skill.id}" repo="${skill.repo}" name="${skill.name}">\n${skill.description}\n</skill>`
      )
      .join("\n")}\n</skills>`,
};

// The sentence reader.

export const SentenceInput = z
  .strictObject({
    skills: z
      .array(
        z.strictObject({
          id: Id,
          repo: z.string().min(1),
          path: z.string().min(1),
          text: z.string(),
        })
      )
      .min(1),
  })
  .superRefine((input, ctx) => uniqueIds(input.skills, ctx, "skills"));
export type SentenceInput = z.infer<typeof SentenceInput>;
const SentenceAnswer = z.strictObject({
  found: z.array(z.strictObject({ id: Id, line: z.int().positive(), quote: z.string().min(1) })),
});
type SentenceAnswer = z.infer<typeof SentenceAnswer>;

const sentenceReader: Reader<SentenceInput, SentenceAnswer> = {
  input: SentenceInput,
  answer: (input) =>
    SentenceAnswer.superRefine((answer, ctx) => {
      const counts = new Map(input.skills.map((skill) => [skill.id, textLines(skill.text).length]));
      for (const [index, found] of answer.found.entries()) {
        const count = counts.get(found.id);
        if (count === undefined) refuse(ctx, ["found", index, "id"], "is no id of the skills");
        else if (found.line > count) {
          refuse(
            ctx,
            ["found", index, "line"],
            `is past the last line, ${count}, of skill ${found.id}`
          );
        }
      }
    }),
  render: (input) =>
    input.skills
      .map(
        (skill) =>
          `<skill id="${skill.id}" repo="${skill.repo}" path="${skill.path}">\n${numbered(skill.text)}\n</skill>`
      )
      .join("\n\n"),
};

// The names reader.

/** The words a reason can be made of and still say only that the job is not clear from the name
 *  alone: none of them names a task the name suggests or hides. */
const NAME_ONLY_WORDS = new Set(
  [
    "a an the this that these those its it it's they their skill skills skill's job jobs task tasks",
    "work purpose purposes use uses usage role what which who how does do did doing done is isn't",
    "are aren't was wasn't be being been has have not no nothing clear unclear obvious unobvious",
    "evident apparent explicit self-explanatory from by in on of to for at about as with alone",
    "itself only just solely name names named naming its say says tell tells show shows make makes",
    "explain explains convey conveys describe describes indicate indicates reveal reveals suggest",
    "suggests give gives hint hints any idea reader readers agent agents user users someone one",
    "can can't cannot could couldn't would wouldn't will won't doesn't don't know known guess",
    "infer understand without reading description very really fully entirely quite enough exactly",
    "immediately clearly obviously vague generic ambiguous unspecific nonspecific opaque cryptic",
    "hard difficult too so much or and but",
  ]
    .join(" ")
    .split(" ")
);

/** Whether `text` says only that the job is not clear from the skill's name `name` alone: every
 *  word of it is one of NAME_ONLY_WORDS, the name and its parts left aside. */
export function nameOnlyReason(text: string, name: string): boolean {
  const own = new Set([name, ...name.split("-")].map((word) => word.toLowerCase()));
  const words = text
    .toLowerCase()
    .replaceAll("’", "'")
    .split(/[^a-z0-9'-]+/)
    .map((word) => word.replace(/^['-]+|['-]+$/g, ""))
    .filter((word) => word !== "" && !own.has(word));
  return words.every((word) => NAME_ONLY_WORDS.has(word));
}

const NamesInput = z
  .strictObject({
    skills: z
      .array(
        z.strictObject({
          id: Id,
          repo: z.string().min(1),
          name: z.string().min(1),
          description: z.string(),
          /** What the skill's load test found wrong with the description, or null. */
          descriptionFault: z.string().min(1).nullable(),
        })
      )
      .min(1),
    /** Every skill name in the copies output's list: no proposed name may be one. */
    namesTaken: z.array(z.string().min(1)),
  })
  .superRefine((input, ctx) => uniqueIds(input.skills, ctx, "skills"));
type NamesInput = z.infer<typeof NamesInput>;
const SkillName = z
  .string()
  .regex(/^[a-z0-9]+(?:-[a-z0-9]+)*$/, "is not lowercase words joined by -");
const NamesAnswer = z.strictObject({
  skills: z.array(
    z.discriminatedUnion("nominated", [
      z.strictObject({ id: Id, nominated: z.literal(false) }),
      z.strictObject({
        id: Id,
        nominated: z.literal(true),
        risk: z.enum(["wrong-task", "missed"]),
        task: z.string().min(1),
        reason: z.string().min(1),
        proposed: z.array(SkillName).min(1).max(3),
      }),
    ])
  ),
});
type NamesAnswer = z.infer<typeof NamesAnswer>;

const namesReader: Reader<NamesInput, NamesAnswer> = {
  input: NamesInput,
  answer: (input) =>
    NamesAnswer.superRefine((answer, ctx) => {
      const skills = new Map(input.skills.map((skill) => [skill.id, skill]));
      const taken = new Set(input.namesTaken);
      const answered = new Set<string>();
      for (const [index, entry] of answer.skills.entries()) {
        const skill = skills.get(entry.id);
        if (skill === undefined) {
          refuse(ctx, ["skills", index, "id"], "is no id of the skills");
          continue;
        }
        if (answered.has(entry.id))
          refuse(ctx, ["skills", index, "id"], `answers ${entry.id} twice`);
        answered.add(entry.id);
        if (!entry.nominated) {
          if (skill.descriptionFault !== null) {
            refuse(
              ctx,
              ["skills", index, "nominated"],
              "is false for a skill whose description has a fault"
            );
          }
          continue;
        }
        for (const field of ["reason", "task"] as const) {
          if (nameOnlyReason(entry[field], skill.name)) {
            refuse(ctx, ["skills", index, field], NAME_ONLY_REFUSAL);
          }
        }
        for (const [at, name] of entry.proposed.entries()) {
          if (name === skill.name)
            refuse(ctx, ["skills", index, "proposed", at], "is the skill's own name");
          else if (taken.has(name))
            refuse(ctx, ["skills", index, "proposed", at], "is a name in use");
          else if (entry.proposed.indexOf(name) !== at) {
            refuse(ctx, ["skills", index, "proposed", at], "is proposed twice");
          }
        }
      }
      for (const skill of input.skills) {
        if (!answered.has(skill.id)) refuse(ctx, ["skills"], `gives no entry for ${skill.id}`);
      }
    }),
  render: (input) =>
    [
      `<skills>\n${input.skills
        .map((skill) => {
          const fault =
            skill.descriptionFault === null ? "" : `\n<fault>${skill.descriptionFault}</fault>`;
          return `<skill id="${skill.id}" repo="${skill.repo}" name="${skill.name}">\n${skill.description}${fault}\n</skill>`;
        })
        .join("\n")}\n</skills>`,
      `<names-taken>\n${input.namesTaken.join("\n")}\n</names-taken>`,
    ].join("\n\n"),
};

// The tasks reader.

export const TasksInput = z
  .strictObject({
    skill: z.strictObject({ name: z.string().min(1), description: z.string() }),
    /** First user messages of real sessions, each with an id: those a query returned (`pick`),
     *  or the first requests of sessions whose first assistant message read the skill
     *  (`control`). */
    messages: z.array(z.strictObject({ id: Id, text: z.string().min(1) })),
    /** `pick` (when absent): propose terms, and pick and write the tasks. `control`: each message
     *  is a need task of the positive control, unchosen; the reader only says whether the skill
     *  governs its first step. */
    mode: z.enum(["pick", "control"]).optional(),
  })
  .superRefine((input, ctx) => uniqueIds(input.messages, ctx, "messages"));
export type TasksInput = z.infer<typeof TasksInput>;
/** Whether a task's first step is one the skill governs (an agent doing the task would read the
 *  skill at once, before any other act), and why. */
const FirstStep = z.strictObject({ governed: z.boolean(), reason: z.string().min(1) });
const TaskAnswerFields = {
  label: TaskLabel,
  why: z.string().min(1),
  firstStep: FirstStep.optional(),
};
export const TasksAnswer = z.strictObject({
  terms: z.array(z.string().min(1)).max(6),
  tasks: z.array(
    z.discriminatedUnion("from", [
      z.strictObject({ ...TaskAnswerFields, from: z.literal("session"), messageId: Id }),
      z.strictObject({ ...TaskAnswerFields, from: z.literal("written"), text: z.string().min(1) }),
    ])
  ),
});
export type TasksAnswer = z.infer<typeof TasksAnswer>;

/** What the tasks reader's `<control>` part says. */
const CONTROL_NOTE =
  "Each message is the first request of a session whose agent read the skill in its first reply.";

const tasksReader: Reader<TasksInput, TasksAnswer> = {
  input: TasksInput,
  answer: (input) =>
    TasksAnswer.superRefine((answer, ctx) => {
      const ids = new Set(input.messages.map((message) => message.id));
      const used = new Set<string>();
      for (const [index, task] of answer.tasks.entries()) {
        if (task.label === "need" && task.firstStep === undefined) {
          refuse(
            ctx,
            ["tasks", index, "firstStep"],
            "is missing: a need task says whether the skill governs its first step"
          );
        }
        if (task.from !== "session") continue;
        if (!ids.has(task.messageId))
          refuse(ctx, ["tasks", index, "messageId"], "is no id of the messages");
        else if (used.has(task.messageId)) {
          refuse(ctx, ["tasks", index, "messageId"], "takes a message a task already took");
        }
        used.add(task.messageId);
      }
      if (input.mode === "control") {
        for (const [index, task] of answer.tasks.entries()) {
          if (task.label !== "need" || task.from !== "session") {
            refuse(ctx, ["tasks", index], "is not a need task of a message: a control task is one");
          }
        }
        for (const id of ids) {
          if (!used.has(id)) refuse(ctx, ["tasks"], `leaves out the message ${id}`);
        }
        return;
      }
      if (answer.terms.length === 0) refuse(ctx, ["terms"], "holds no term");
      const governed = answer.tasks.filter(
        (task) => task.label === "need" && task.firstStep?.governed === true
      ).length;
      if (governed < TASKS_PER_LABEL) {
        refuse(
          ctx,
          ["tasks"],
          `holds ${governed} need tasks whose first step the skill governs, fewer than ${TASKS_PER_LABEL}`
        );
      }
      const nearMiss = answer.tasks.filter((task) => task.label === "near-miss").length;
      if (nearMiss < TASKS_PER_LABEL) {
        refuse(ctx, ["tasks"], `holds ${nearMiss} near-miss tasks, fewer than ${TASKS_PER_LABEL}`);
      }
    }),
  render: (input) =>
    [
      ...(input.mode === "control" ? [`<control>\n${CONTROL_NOTE}\n</control>`] : []),
      `<skill name="${input.skill.name}">\n<description>\n${input.skill.description}\n</description>\n</skill>`,
      `<first-messages>\n${
        input.messages.length === 0
          ? "None."
          : input.messages
              .map((message) => `<message id="${message.id}">\n${message.text}\n</message>`)
              .join("\n\n")
      }\n</first-messages>`,
    ].join("\n\n"),
};

/** Every reader, by the name of its prompt file. */
export const READERS = {
  case: caseReader,
  restate: restateReader,
  audit: auditReader,
  map: mapReader,
  attack: attackReader,
  "same-job": sameJobReader,
  sentence: sentenceReader,
  names: namesReader,
  tasks: tasksReader,
} as const;
export type ReaderName = keyof typeof READERS;
export const READER_NAMES = Object.keys(READERS) as ReaderName[];

/** A reader's answer, the refusals of the replies before it, and what each reply used. */
interface ReaderResult {
  readonly answer: unknown;
  readonly refusals: readonly string[];
  readonly spent: readonly Spent[];
}

/** A reader whose every reply was refused. */
export class ReaderFailed extends Error {
  constructor(
    readonly reader: ReaderName,
    readonly refusals: readonly string[],
    readonly spent: readonly Spent[]
  ) {
    super(
      `the ${reader} reader's answer was refused on each of ${refusals.length} tries: ${refusals.join(" | ")}`
    );
    this.name = "ReaderFailed";
  }
}

/** The request body of reader `name` for `input`, already parsed by its input schema. */
export function readerBody(name: ReaderName, input: unknown): MessagesBody {
  const reader: Reader<unknown, unknown> = READERS[name];
  return promptBody(loadPrompt(name), [{ text: reader.render(input) }]);
}

/** Runs reader `name` on one input batch: one call, asked again while the answer is refused,
 *  READER_TRIES times in all. Throws when the input is refused, and ReaderFailed when every reply
 *  is. A send that rejects (the gateway's BudgetSpent among them) rejects. */
export async function runReader(
  name: ReaderName,
  input: unknown,
  gateway: Gateway
): Promise<ReaderResult> {
  const reader: Reader<unknown, unknown> = READERS[name];
  const given = reader.input.safeParse(input);
  if (!given.success)
    throw new Error(`the ${name} reader's input is refused: ${issues(given.error)}`);
  const schema = reader.answer(given.data);
  const asked = await ask(readerBody(name, given.data), schema, gateway, READER_TRIES);
  if (asked.answer === undefined) throw new ReaderFailed(name, asked.refusals, asked.spent);
  return { answer: asked.answer, refusals: asked.refusals, spent: asked.spent };
}

// The readers' positive controls. Each plants a known finding in a copy of real
// input, runs the reader once on it, and holds only when the answer names that finding: the map
// reader calls a deleted rule lost, the attack reader lists an added vague line, the same-job
// reader pairs a planted rewording of a description with its original, and the sentence reader
// finds a planted rewording of the sentence.

/** The line the attack control adds. */
export const ATTACK_PLANTED_LINE = "Run the tests where needed.";

/** The `.md` files of the skill directory `dir` (SKILL.md and its references), SKILL.md first and
 *  the rest by path, as the map, attack and case readers take them. */
export function skillFiles(dir: string): File[] {
  return readdirSync(dir, { recursive: true, withFileTypes: true })
    .filter((entry) => entry.isFile() && entry.name.endsWith(".md"))
    .map((entry) => path.relative(dir, path.join(entry.parentPath, entry.name)))
    .sort(
      (a, b) => Number(b === "SKILL.md") - Number(a === "SKILL.md") || (a < b ? -1 : a > b ? 1 : 0)
    )
    .map((file) => ({ path: file, text: readFileSync(path.join(dir, file), "utf8") }));
}

/** A control: the reader's real input, and what to plant in it. `map`: the line of the cut text to
 *  delete, which the base text must hold word for word (a rule the cut kept), so the rule at that
 *  base line is the one that must be lost. `attack`: the line to add at the end of SKILL.md.
 *  `same-job`: a skill entry to add, whose description rewords that of the skill `copyOf`.
 *  `sentence`: a wording of the sentence to add at the end of the skill `into`. */
export type Control =
  | { readonly reader: "map"; readonly input: MapInput; readonly deleteLine: string }
  | { readonly reader: "attack"; readonly input: AttackInput; readonly plantLine: string }
  | {
      readonly reader: "same-job";
      readonly input: SameJobInput;
      readonly planted: SameJobInput["skills"][number];
      readonly copyOf: string;
    }
  | {
      readonly reader: "sentence";
      readonly input: SentenceInput;
      readonly into: string;
      readonly sentence: string;
    };

/** Whether a control held, and why. */
export interface Held {
  readonly held: boolean;
  readonly why: string;
}

/** A control's planted input, and its check of the reader's answer. */
interface Planted {
  readonly input: unknown;
  held(answer: unknown): Held;
}

/** The first line of `files` that is `line` (trimmed), or undefined. */
function placeOf(files: readonly File[], line: string): Place | undefined {
  for (const file of files) {
    const index = textLines(file.text).findIndex((each) => each.trim() === line.trim());
    if (index >= 0) return { file: file.path, line: index + 1 };
  }
  return undefined;
}

/** `text` with `line` added at its end as a paragraph of its own, and the line's number. */
function withLineAdded(text: string, line: string): { text: string; line: number } {
  const lines = [...textLines(text), "", line];
  return { text: `${lines.join("\n")}\n`, line: lines.length };
}

/** The planted input of `control` and its check. Throws when the control cannot be planted: a
 *  line to delete that the cut or the base text lacks, an id given twice, or an id the input
 *  lacks. */
export function plantControl(control: Control): Planted {
  switch (control.reader) {
    case "map": {
      const { input, deleteLine } = control;
      const cut = placeOf(input.cut, deleteLine);
      const base = placeOf(input.base, deleteLine);
      if (cut === undefined)
        throw new Error(`the map control's line is no line of the cut text: ${deleteLine}`);
      if (base === undefined) {
        throw new Error(
          `the map control's line is no line of the base text, so its rule cannot be found: ${deleteLine}`
        );
      }
      const files = input.cut.map((file) => {
        if (file.path !== cut.file) return file;
        const lines = textLines(file.text).filter((_, index) => index !== cut.line - 1);
        return { ...file, text: `${lines.join("\n")}\n` };
      });
      return {
        input: { ...input, cut: files },
        held: (answer) => {
          const rule = MapAnswer.parse(answer).rules.find(
            (each) => each.file === base.file && each.line === base.line
          );
          const at = `the rule at ${base.file}:${base.line} of the base text`;
          if (rule === undefined)
            return { held: false, why: `the map gives no rule at ${base.file}:${base.line}` };
          return { held: rule.status === "lost", why: `${at} is ${rule.status}: ${rule.reason}` };
        },
      };
    }
    case "attack": {
      const { input, plantLine } = control;
      const skill = input.files.find((file) => file.path === "SKILL.md") ?? input.files[0];
      if (skill === undefined) throw new Error("the attack control's skill has no file");
      const added = withLineAdded(skill.text, plantLine);
      return {
        input: {
          ...input,
          files: input.files.map((file) => (file === skill ? { ...file, text: added.text } : file)),
        },
        held: (answer) => {
          const found = AttackAnswer.parse(answer).findings.find(
            (each) => each.file === skill.path && each.line === added.line
          );
          return found === undefined
            ? { held: false, why: `no finding at ${skill.path}:${added.line}, the added line` }
            : { held: true, why: `${found.kind} at ${skill.path}:${added.line}: ${found.why}` };
        },
      };
    }
    case "same-job": {
      const { input, planted, copyOf } = control;
      if (!input.skills.some((skill) => skill.id === copyOf)) {
        throw new Error(`the same-job control's original ${copyOf} is no id of the skills`);
      }
      if (input.skills.some((skill) => skill.id === planted.id)) {
        throw new Error(
          `the same-job control's planted id ${planted.id} is an id of the skills already`
        );
      }
      return {
        input: { skills: [...input.skills, planted] },
        held: (answer) => {
          const pair = SameJobAnswer.parse(answer).pairs.find(
            ({ a, b }) => (a === planted.id && b === copyOf) || (a === copyOf && b === planted.id)
          );
          return pair === undefined
            ? { held: false, why: `no pair of ${planted.id} and ${copyOf}` }
            : { held: true, why: `${planted.id} and ${copyOf} are paired: ${pair.task}` };
        },
      };
    }
    case "sentence": {
      const { input, into, sentence } = control;
      const skill = input.skills.find((each) => each.id === into);
      if (skill === undefined)
        throw new Error(`the sentence control's skill ${into} is no id of the skills`);
      const added = withLineAdded(skill.text, sentence);
      return {
        input: {
          skills: input.skills.map((each) =>
            each === skill ? { ...each, text: added.text } : each
          ),
        },
        held: (answer) => {
          const found = SentenceAnswer.parse(answer).found.find(
            (each) => each.id === into && each.line === added.line
          );
          return found === undefined
            ? {
                held: false,
                why: `no sentence found in ${into} at line ${added.line}, the added line`,
              }
            : { held: true, why: `${into} line ${added.line}: ${found.quote}` };
        },
      };
    }
  }
}

/** Runs `control`: its reader on its planted input, once, and its check of the answer. */
export async function runControl(
  control: Control,
  gateway: Gateway
): Promise<Held & { result: ReaderResult }> {
  const planted = plantControl(control);
  const result = await runReader(control.reader, planted.input, gateway);
  return { ...planted.held(result.answer), result };
}
