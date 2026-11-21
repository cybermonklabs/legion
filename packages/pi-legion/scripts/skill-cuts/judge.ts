/**
 * The blind judge of the behaviour test: one plain Messages API call per
 * reply with `prompts/judge.md`, whose header pins the model, the thinking budget, `max_tokens`,
 * the temperature and the effort of every request (readers.ts, `promptBody`).
 *
 * A request holds four parts and nothing else: the case's criterion (pass and fail), its line, the
 * last messages before the step rendered as text (`beforeText`), and the reply (`stepText`). The
 * first three are the case's, the same for every arm and every run; they go in the user message's
 * first block, which is marked for the prompt cache, and the reply goes in the second. So the
 * request names no arm, and no path of an arm's files, and holds no text of an arm's skill files
 * but the case's line:
 *
 * - `beforeText` renders the case's own messages, never an arm's, and leaves out the text of each
 *   read result of a file of the case's skill (the case's `skillReads`, and any other read of a
 *   `skill://<skill>` URL), with a note in its place; every other read result loses its
 *   `[Skill file: <path>]` line. It shows the last BEFORE_MESSAGES messages, and before them the
 *   last user message when it is older, each message cut to MESSAGE_CHARS characters (its start and
 *   its end kept); a note says how many messages or characters each cut left out.
 * - `stepText` renders a reply's text and tool calls, the same way for an Oh My Pi message (the
 *   real step), a Messages API reply and a Responses API reply (a replayed one): a Responses
 *   `message` item as the text of its parts and a `function_call` item as a Messages `tool_use`
 *   block of the same name and input, so the request has one shape whichever model answered.
 *   Thinking and reasoning are left out: the step is what the agent did, and its thinking can
 *   quote the arm's skill text.
 *
 * A judgement is the JSON object `{"verdict": "pass" | "fail", "reason": ...}`. One that does not
 * parse is asked for again, JUDGE_TRIES times in all, and then gives the verdict null, never fail
 * (score.ts counts judged runs only). A send that rejects rejects (`judgeReply`), or gives null
 * with its reason (`judgeBlind`), never a verdict. Each call, a try asked again among them, goes
 * with a session id of its own (gateway.ts's `freshIdentity`, which adds `metadata.user_id`), so
 * no two judgements share one: a gateway that routes or caches by it would make it a factor.
 *
 * `judgeBlind` shuffles the replies of all arms of a case with a seed (`blindOrder`), names each by
 * an opaque id that depends on the seed and its place alone, and starts their judgements in that
 * order: the first alone, then the rest at once, so that they read the case's part from the cache
 * the first one wrote rather than each write it. The caller records the seed, so the order and the
 * ids can be made again. `judgeControls` runs a case's three controls (CONTROLS), the real step's
 * first and alone in the same way: its real step must pass, and both its violating step (written
 * by the case reader before any run) and the reply NOTHING_REPLY must fail.
 */
import { createHash, randomBytes } from "node:crypto";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { readCalls, type SessionMessage, stripSkillHeader } from "../skill-scenarios/transcript";
import type { ArmName } from "./arms";
import type { Case } from "./cases";
import {
  BudgetSpent,
  freshIdentity,
  type Gateway,
  type MessagesBody,
  type Spent,
  toolCalls,
} from "./gateway";
import { ask, loadPrompt, promptBody } from "./readers";

/** How many times one reply is judged before its verdict is null. */
export const JUDGE_TRIES = 3;
/** The last messages before the step that the judge reads. */
export const BEFORE_MESSAGES = 12;
/** The most characters of one message before the step that the judge reads. */
export const MESSAGE_CHARS = 4_000;
/** The judge control's reply that does nothing. */
export const NOTHING_REPLY = "I will continue.";

/** The names of a case's judge controls. */
export const ControlName = z.enum(["realStep", "violating", "nothing"]);
export type ControlName = z.infer<typeof ControlName>;
/** A case's judge controls: the step each one judges and the verdict it must get. A case that
 *  fails any control is dropped, and the drop is counted by each control it failed. */
export const CONTROLS = {
  realStep: { step: "the real step", wanted: "pass" },
  violating: { step: "the violating step", wanted: "fail" },
  nothing: { step: `the reply "${NOTHING_REPLY}"`, wanted: "fail" },
} as const satisfies Record<ControlName, { step: string; wanted: "pass" | "fail" }>;

/** A judgement as the judge answers it. */
const Judgement = z.strictObject({
  verdict: z.enum(["pass", "fail"]),
  reason: z.string().min(1),
});

/** One block of a message: a session message's (`text`, `thinking`, `toolCall` with `arguments`),
 *  a Messages API reply's (`text`, `thinking`, `redacted_thinking`, `tool_use` with `input`) or a
 *  Responses API reply's output item (`message` with its `output_text` and `refusal` parts,
 *  `reasoning`, `function_call` with `arguments` as JSON text). */
type Block = { readonly type: string; readonly [field: string]: unknown };

/** A tool call as the judge reads it: its tool's name, then its input as JSON. */
function callText(name: unknown, input: unknown): string {
  return `[tool call: ${String(name)}] ${JSON.stringify(input ?? {})}`;
}

/** What the judge reads of one part of a Responses `message` item: its text or refusal. */
function partText(part: unknown): string {
  const text = typeof part === "object" && part !== null ? Reflect.get(part, "text") : undefined;
  const refusal =
    typeof part === "object" && part !== null ? Reflect.get(part, "refusal") : undefined;
  if (typeof text === "string") return text;
  if (typeof refusal === "string") return refusal;
  return `[a part of type ${String(Reflect.get(Object(part), "type"))}]`;
}

/** What the judge reads of one block, or undefined for thinking and reasoning, which it never
 *  reads. A Responses `function_call` reads as the `tool_use` block of its name and parsed
 *  arguments would, and arguments that do not parse as they were sent. */
function blockText(block: Block): string | undefined {
  switch (block.type) {
    case "text":
      return typeof block.text === "string" ? block.text : "";
    case "toolCall":
      return callText(block.name, block.arguments);
    case "tool_use":
    case "server_tool_use":
      return callText(block.name, block.input);
    case "message":
      return (Array.isArray(block.content) ? block.content : []).map(partText).join("\n");
    case "function_call": {
      const input = toolCalls([block])[0]?.input;
      return input === undefined
        ? `[tool call: ${String(block.name)}] ${String(block.arguments)}`
        : callText(block.name, input);
    }
    case "thinking":
    case "redacted_thinking":
    case "reasoning":
      return undefined;
    case "image":
      return "[an image]";
    default:
      return `[a block of type ${block.type}]`;
  }
}

/** The text and tool calls of a reply or a message's content, one block per line. Its thinking is
 *  left out; a step with neither text nor a tool call says so. */
export function stepText(content: readonly Block[]): string {
  const text = content.flatMap((block) => blockText(block) ?? []).join("\n");
  return text.trim() === "" ? "[the step holds no text and no tool call]" : text;
}

/** One message as text: its role in brackets (a tool result's tool, and whether it failed), then
 *  its blocks, with a read result's `[Skill file: <path>]` line left out. */
export function renderMessage(message: SessionMessage): string {
  const head =
    message.role === "toolResult"
      ? `[tool result: ${message.toolName ?? "a tool"}${message.isError === true ? ", an error" : ""}]`
      : `[${message.role ?? "message"}]`;
  const blocks = (message.content ?? []).flatMap((block) => {
    const text = blockText(block);
    if (text === undefined) return [];
    return message.role === "toolResult" ? [stripSkillHeader(text)] : [text];
  });
  return [head, ...blocks].join("\n");
}

/** Whether a read's `target` is a file of skill `skill`: `skill://<skill>`, a file under it, or
 *  it with a selector. */
export function readsSkillUrl(target: string, skill: string): boolean {
  const url = `skill://${skill}`;
  return target === url || target.startsWith(`${url}/`) || target.startsWith(`${url}:`);
}

/** The indices in `messages` of the read results of a file of skill `skill` (`readsSkillUrl`). */
export function skillReadResults(messages: readonly SessionMessage[], skill: string): Set<number> {
  const reads = new Set(
    readCalls(messages.map((message) => ({ message })))
      .filter(({ target }) => readsSkillUrl(target, skill))
      .map(({ id }) => id)
  );
  const indices = new Set<number>();
  for (const [index, message] of messages.entries()) {
    if (message.role === "toolResult" && reads.has(message.toolCallId ?? "")) indices.add(index);
  }
  return indices;
}

/** `text` cut to `max` characters, its start and end kept, with a note of what the cut left out. */
export function cut(text: string, max: number): string {
  if (text.length <= max) return text;
  const half = Math.floor(max / 2);
  return `${text.slice(0, half)}\n[… ${text.length - 2 * half} characters left out …]\n${text.slice(-half)}`;
}

/** The note in place of a read result of the skill's files. */
const SKILL_READ_LEFT_OUT = "[tool result: read]\n[the text of a file of the skill, left out]";

/** The messages before the step as the judge reads them (the module's comment says which). */
export function beforeText(c: Pick<Case, "messages" | "skill" | "skillReads">): string {
  const left = new Set([...c.skillReads, ...skillReadResults(c.messages, c.skill)]);
  const start = Math.max(0, c.messages.length - BEFORE_MESSAGES);
  const lastUser = c.messages.findLastIndex((message) => message.role === "user");
  const shown = lastUser >= 0 && lastUser < start ? [lastUser] : [];
  for (let index = start; index < c.messages.length; index += 1) shown.push(index);
  const parts: string[] = [];
  let next = 0;
  for (const index of shown) {
    const gap = index - next;
    if (gap > 0) parts.push(`[… ${gap} ${gap === 1 ? "message" : "messages"} left out …]`);
    const message = c.messages[index];
    if (message !== undefined) {
      parts.push(
        left.has(index) ? SKILL_READ_LEFT_OUT : cut(renderMessage(message), MESSAGE_CHARS)
      );
    }
    next = index + 1;
  }
  return parts.join("\n\n");
}

/** What one judgement reads: the case's criterion and line, the messages before the step
 *  (`beforeText`) and the reply (`stepText`). */
interface JudgeInput {
  readonly criterion: { readonly pass: string; readonly fail: string };
  readonly line: string;
  readonly before: string;
  readonly reply: string;
}

/** The case's part of a judgement's request: its criterion, its line and the messages before the
 *  step, the same text for every reply of the case. */
function sharedText(input: Omit<JudgeInput, "reply">): string {
  return [
    `<criterion>\nPass: ${input.criterion.pass}\nFail: ${input.criterion.fail}\n</criterion>`,
    `<line>\n${input.line}\n</line>`,
    `<before>\n${input.before}\n</before>`,
  ].join("\n\n");
}

/** The request body of the judgement of `reply` whose case's part is `shared` (`sharedText`):
 *  that part first, marked for the prompt cache, then the step. */
function bodyOf(shared: string, reply: string): MessagesBody {
  return promptBody(loadPrompt("judge"), [
    { text: shared, cache: true },
    { text: `<step>\n${reply}\n</step>` },
  ]);
}

/** The request body of one judgement: the case's part first, marked for the prompt cache, then the
 *  step. */
export function judgeBody(input: JudgeInput): MessagesBody {
  return bodyOf(sharedText(input), input.reply);
}

/** A judgement: its verdict, null when no judgement parsed or the call failed; the judge's reason
 *  or why there is no verdict; and what each call used. */
export interface Judged {
  readonly verdict: "pass" | "fail" | null;
  readonly reason: string;
  readonly spent: readonly Spent[];
}

/** The judgement that `body` asks for. A judgement that does not parse is asked for again,
 *  JUDGE_TRIES times in all, then gives the verdict null. Each call goes with a session id of its
 *  own. Rejects when a send does. */
async function askJudge(body: MessagesBody, gateway: Gateway): Promise<Judged> {
  const freshEach: Gateway = {
    send: (sent, options) => gateway.send(freshIdentity(sent, true).body, options),
    spend: () => gateway.spend(),
  };
  const asked = await ask(body, Judgement, freshEach, JUDGE_TRIES);
  if (asked.answer === undefined) {
    return {
      verdict: null,
      reason: `the judgement did not parse on ${JUDGE_TRIES} tries: ${asked.refusals.join(" | ")}`,
      spent: asked.spent,
    };
  }
  return { verdict: asked.answer.verdict, reason: asked.answer.reason, spent: asked.spent };
}

/** The judgement of one reply. A judgement that does not parse is asked for again, JUDGE_TRIES
 *  times in all, then gives the verdict null. Rejects when a send does. */
export function judgeReply(input: JudgeInput, gateway: Gateway): Promise<Judged> {
  return askJudge(judgeBody(input), gateway);
}

/** A case's judge controls (CONTROLS): each one's judgement, the controls whose verdict is not the
 *  one wanted (a null verdict is not), in CONTROLS' order, and whether none is. The real step is
 *  judged alone first and the other two after it, so they read the case's part from the cache it
 *  wrote. Rejects when a send does. */
export async function judgeControls(
  c: Case,
  gateway: Gateway
): Promise<Record<ControlName, Judged> & { failed: ControlName[]; hold: boolean }> {
  const shared = sharedText({ criterion: c.criterion, line: c.line.text, before: beforeText(c) });
  const judge = (content: readonly Block[]) => askJudge(bodyOf(shared, stepText(content)), gateway);
  const realStep = await judge(c.realStep.content ?? []);
  const [violating, nothing] = await Promise.all([
    judge(c.violatingStep.content ?? []),
    judge([{ type: "text", text: NOTHING_REPLY }]),
  ]);
  const judged: Record<ControlName, Judged> = { realStep, violating, nothing };
  const failed = ControlName.options.filter(
    (name) => judged[name].verdict !== CONTROLS[name].wanted
  );
  return { ...judged, failed, hold: failed.length === 0 };
}

/** A fresh seed for `blindOrder`, to be recorded with the judgements. */
export function newSeed(): string {
  return randomBytes(16).toString("hex");
}

/** `items` in the order the seed gives (a Fisher-Yates shuffle whose numbers are SHA-256 of the
 *  seed and a counter), each named by an opaque id: 12 hex digits of SHA-256 of the seed and its
 *  place, so an id says nothing of the item it names. The same seed gives the same order and
 *  ids. */
export function blindOrder<T>(items: readonly T[], seed: string): { id: string; item: T }[] {
  let counter = 0;
  const random = () =>
    createHash("sha256").update(`${seed}:shuffle:${counter++}`).digest().readUIntBE(0, 6) / 2 ** 48;
  const order = [...items];
  for (let index = order.length - 1; index > 0; index -= 1) {
    const other = Math.floor(random() * (index + 1));
    [order[index], order[other]] = [order[other] as T, order[index] as T];
  }
  return order.map((item, place) => ({
    id: createHash("sha256").update(`${seed}:id:${place}`).digest("hex").slice(0, 12),
    item,
  }));
}

/** One replayed reply of one arm of a case: its arm, its run, and its content. */
export interface ArmReply {
  readonly arm: ArmName;
  readonly run: number;
  readonly content: readonly Block[];
}

/** One reply's judgement under its opaque id. `budgetRefused` is true when the gateway refused the
 *  call for the budget: the reply was not judged, and it is no run of the case. */
export interface BlindJudged extends Judged {
  readonly id: string;
  readonly arm: ArmName;
  readonly run: number;
  readonly budgetRefused: boolean;
}

/** The judgements of `replies`, the replies of all arms of case `c`, in the order `blindOrder`
 *  gives them under `seed`: the first alone, then the rest at once in that order, so they read the
 *  case's part from the cache the first wrote. A call that fails gives the verdict null with its
 *  reason. */
export async function judgeBlind(
  c: Case,
  replies: readonly ArmReply[],
  gateway: Gateway,
  seed: string
): Promise<{ seed: string; judged: BlindJudged[] }> {
  const shared = sharedText({ criterion: c.criterion, line: c.line.text, before: beforeText(c) });
  const order = blindOrder(replies, seed);
  const judge = ({ item }: { item: ArmReply }) =>
    askJudge(bodyOf(shared, stepText(item.content)), gateway);
  const [head, ...rest] = order;
  const first = head === undefined ? [] : await Promise.allSettled([judge(head)]);
  const settled = [...first, ...(await Promise.allSettled(rest.map(judge)))];
  const judged = order.map(({ id, item }, place): BlindJudged => {
    const outcome = settled[place];
    const named = { id, arm: item.arm, run: item.run };
    if (outcome?.status === "fulfilled")
      return { ...named, ...outcome.value, budgetRefused: false };
    const error: unknown = outcome?.reason;
    return {
      ...named,
      verdict: null,
      reason: `the judge's call failed: ${messageFor(error)}`,
      spent: [],
      budgetRefused: error instanceof BudgetSpent,
    };
  });
  return { seed, judged };
}
