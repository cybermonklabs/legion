import { readFile } from "node:fs/promises";
import { setTimeout as sleep } from "node:timers/promises";
import { listOf, numberOf, recordOf, stringOf } from "./corpus-io";

/**
 * The two endpoints the round's twelve arms are scored on, each answering one typed result.
 *
 * - Claude, through the deployment's gateway: `POST $ANTHROPIC_BASE_URL/v1/messages`, with the
 *   key read from `/var/run/model-gateway-key/key` at the moment of the call, so a key rolled
 *   between two calls is picked up by the second. The answer is parsed for the shape
 *   `recipes.ts` asks for - the label of a chosen option, on a line of its own or at the head of
 *   one, qualified - and an answer naming no label, or naming two of them, is the `unreadable`
 *   fault, carrying the body of what came back so the report can count those answers rather
 *   than guess at them.
 * - Jev, called directly at `https://api.typesafe.ai/v1/systemone` because the gateway serves no
 *   TypeSafe model: one choice question over the case as its state, which answers the chosen
 *   name with its confidence and its probabilities. The key is `TYPESAFE_API_KEY`, which the
 *   broker puts in the environment of the command it starts, so the runner itself must be that
 *   command: `agent-secrets TYPESAFE_API_KEY -- bun scripts/approval-eval/run-round.ts ...`.
 *
 * The credential never leaves this module. It is read at call time, put in one header, and never
 * printed, logged, sliced, put in an error message or written to a row; every fault detail is
 * passed through `scrubSecrets`, which the runner calls over the same details again on every
 * path out of the process, so even an endpoint that echoed a key back could not write one into
 * the round's output. Neither function prints anything.
 *
 * A fault carries the answering body of the attempt that ended the ladder, bounded at
 * `DETAIL_CHARS`, and never its length or a count of it: a fault whose reason is thrown away
 * costs the next reader an hour, and a provider's JSON error carries its `message` well past the
 * first 200 characters.
 *
 * One retry ladder covers both: a transport error, a 429 and a 5xx are tried again, four times,
 * with growing waits and jitter, and a `retry-after` header on the answer sets the wait where it
 * asks for a longer one. A 4xx other than 429 is not retried, since the request is what is
 * wrong. Every outcome is a `ModelAnswer`: an answer, or a fault saying why none came.
 */

/** Where the gateway's key is kept, read at the moment of each call. */
const CLAUDE_KEY_FILE = "/var/run/model-gateway-key/key";

/** The one route of the gateway this round calls. */
const CLAUDE_PATH = "/v1/messages";

/** The version header the Messages API requires. */
const ANTHROPIC_VERSION = "2023-06-01";

/** Where Jev is called, which is not the gateway: the gateway serves no TypeSafe model. */
const JEV_URL = "https://api.typesafe.ai/v1/systemone";

/** The name of the environment variable the broker puts the Jev key in. */
const JEV_KEY_VARIABLE = "TYPESAFE_API_KEY";

/** The name the round asks its one Jev question under, and so the name the answer comes back as. */
export const JEV_QUESTION = "verdict";

/** The growing waits between the tries of one call: four retries after the first try. */
const RETRY_WAITS_MS: readonly number[] = [2_000, 4_000, 8_000, 16_000];

/** How much of a wait is drawn at random, so retried calls of one round do not march in step. */
const JITTER = 0.4;

/** No wait between tries is longer than this, however long a `retry-after` asks for. */
const LONGEST_WAIT_MS = 60_000;

/** How long one call has before it is given up on, where the caller bounds none. */
const DEFAULT_TIME_MS = 180_000;

/**
 * How many tokens an answer is given. The label itself is one line, but this gateway answers a
 * large case with a thinking block before it, and a budget that runs out inside that block
 * leaves a body with no text block at all, which is the `unreadable` fault. Measured on
 * 2026-11-11 over three 84,000-byte cases: an answer that chose a label spent 170 to 223 output
 * tokens, and 6 of 15 answers at a budget of 256 carried a thinking block and no text. The
 * budget is what an answer may take and not what it costs, so it is set well past that.
 *
 * The same defect one size up, measured on 2026-11-12: at 2,048 all 32 unreadable approval rows
 * of round 1 were `anthropic/claude-fable-5-1` - 18 of `bare-fable-5-1`, 9 of
 * `distilled-fable-5-1`, 7 of `both-fable-5-1` - and every one of them was a body whose only
 * block was `thinking`, not a label the parser missed. The control: that model on a short
 * prompt at 2,048 stops at `end_turn` with blocks `thinking` and `text` and 255 output tokens,
 * and at 8,192 answers the same text with 370, so only the large cases reach the budget.
 *
 * Raising the budget cannot change an answer that already fitted inside the old one - the
 * budget bounds an answer and does not shape it - so the rows already scored stay comparable
 * with the rows scored after this. That is why this is a fix to an arm and not a new arm.
 */
const DEFAULT_MAX_TOKENS = 8_192;

/**
 * How much of a refusal or an unreadable answer is kept: the body, far enough in to reach the
 * `message` a provider's JSON error puts after its code, and never a whole page.
 */
const DETAIL_CHARS = 2_000;

/** How many times a Jev state is halved when the endpoint refuses it for its size. */
const JEV_STATE_CUTS = 3;

/** What is written where a Jev state was cut, so the cut is visible in the state itself. */
const JEV_CUT_MARK = "\n\n[... the state was cut to the endpoint's budget ...]\n\n";

/** Why no answer came, or why the answer that came could not be read. */
export type FaultKind = "transport" | "status" | "unreadable";

/** The limit of one call: how long it has, and how many tokens its answer may take. */
export interface CallBounds {
  timeMs?: number;
  maxTokens?: number;
}

/** One answer of one endpoint, which is what a judgement records. */
export interface ModelAnswer {
  /** The option labels the answer chose, and empty where no answer could be read. */
  chosen: string[];
  /**
   * The answer's whole text, where one came back and could be read as a label. A caller that
   * reads more than the label it chose - the never-decide rule, which wants the reason in the
   * model's own words - takes it from here; a caller that wants only the label leaves it.
   */
  text?: string;
  /** How likely each option was, keyed by label, where the endpoint reports them. */
  probabilities: Record<string, number> | null;
  /** How sure the endpoint says it is, from 0 to 1, where it says. */
  confidence: number | null;
  /** The tokens the endpoint reports it read and wrote, and null where it reports none. */
  inputTokens: number | null;
  outputTokens: number | null;
  /** How long the whole call took, every try of it counted. */
  latencyMs: number;
  /** How many tries were made, the first one counted. */
  attempts: number;
  /** Whether the state was cut to the endpoint's budget before it was accepted. */
  stateCut: boolean;
  /** Why no answer came, or why it could not be read. Null on an answer. */
  fault: FaultKind | null;
  /** What that fault was, in words that carry no credential. */
  faultDetail: string | null;
}

/** One prompt as `recipes.ts` renders it, with the labels its answer must be one of. */
export interface ClaudePrompt {
  system: string;
  user: string;
  /** The option labels the case offered, one of which the answer names. */
  labels: readonly string[];
}

/** One call to Claude: the model it is put to, the prompt, and the limit of the call. */
export interface ClaudeAsk {
  model: string;
  prompt: ClaudePrompt;
  bounds?: CallBounds;
}

/** One named choice of a Jev question: the name it is asked under, and the option it stands for. */
export interface JevChoice {
  /** The name the criteria are keyed by, which is what the answer names. */
  name: string;
  /** The option label that name stands for, which is what the round records. */
  label: string;
  /** What choosing it means, which is the criterion the model reads. */
  description: string;
}

/** What a Jev call is told beside its state: the question, its choices and the call's limit. */
export interface JevOptions {
  /** What the model is asked of the state, which is the recipe's own instruction text. */
  instructions: string;
  /** The named choices, in the order the case offered them. */
  choices: readonly JevChoice[];
  bounds?: CallBounds;
}

/** One call to Jev: the model, the case as its state, and the question put about it. */
export interface JevAsk {
  model: string;
  state: string;
  options: JevOptions;
}

/** The gateway is not named, or its key could not be read, so no Claude call can be made. */
export class GatewayError extends Error {
  override readonly name = "GatewayError";
}

/** `TYPESAFE_API_KEY` is not in the environment, so the runner was not started under the broker. */
export class JevKeyError extends Error {
  override readonly name = "JevKeyError";

  constructor() {
    super(
      `${JEV_KEY_VARIABLE} is not in the environment: the Jev arms spend a broker secret, so ` +
        "the runner must itself be the command the broker starts, as in `agent-secrets " +
        `${JEV_KEY_VARIABLE} --reason '...' --ttl 15m -- bun scripts/approval-eval/run-round.ts ...\``
    );
  }
}

/**
 * Whether the Jev key is in the environment at all, which the runner asks before a round rather
 * than after the first call. It answers nothing about the value and throws `JevKeyError` when
 * the variable is unset or empty.
 */
export function assertJevKeyPresent(): void {
  if ((process.env[JEV_KEY_VARIABLE] ?? "").trim() === "") throw new JevKeyError();
}

/** The Jev key as the header carries it: the value with its trailing line ending taken off. */
function jevKey(): string {
  const value = process.env[JEV_KEY_VARIABLE];
  if (value === undefined || value.replace(/[\r\n]+$/, "") === "") throw new JevKeyError();
  const key = value.replace(/[\r\n]+$/, "");
  remember(key);
  return key;
}

/** The gateway's base URL and its key, both read now. */
async function claudeEndpoint(): Promise<{ url: string; key: string }> {
  const base = process.env.ANTHROPIC_BASE_URL?.trim();
  if (!base) {
    throw new GatewayError("ANTHROPIC_BASE_URL names the model gateway, and it is not set");
  }
  let key: string;
  try {
    key = (await readFile(CLAUDE_KEY_FILE, "utf8")).replace(/[\r\n]+$/, "");
  } catch {
    throw new GatewayError(`${CLAUDE_KEY_FILE} could not be read, so no gateway call can be made`);
  }
  if (key === "") throw new GatewayError(`${CLAUDE_KEY_FILE} holds no key`);
  remember(key);
  return { url: `${base.replace(/\/+$/, "")}${CLAUDE_PATH}`, key };
}

/** What a credential is written as once it is taken out of a text. */
const REDACTED = "<redacted>";

/**
 * Every credential this module has read in this process, held so that a detail can be scrubbed
 * of all of them at once. A value in here is one the process already holds in order to make its
 * calls; it is never printed, written or compared with anything but a text being scrubbed.
 */
const secretsRead = new Set<string>();

/** Keeps one credential for the scrub, which a key read or rolled at call time adds to. */
function remember(secret: string): void {
  if (secret !== "") secretsRead.add(secret);
}

/**
 * `text` with every credential this process has read taken out of it. The runner calls it over
 * every fault detail on each path out of the process - the row appended to `rows.jsonl`, the
 * example written into `round.json`, and the line printed on stderr - because a file has more
 * readers than a terminal.
 */
export function scrubSecrets(text: string): string {
  let kept = text;
  const jev = process.env[JEV_KEY_VARIABLE]?.replace(/[\r\n]+$/, "") ?? "";
  if (jev !== "") kept = kept.split(jev).join(REDACTED);
  for (const secret of secretsRead) kept = kept.split(secret).join(REDACTED);
  return kept;
}

/** What one try of one call came back with. */
interface Attempt {
  status: number;
  body: string;
  /** Why this try gave no answer, where it gave none. */
  failure: string | null;
  /** Whether another try could do better: a transport error, a 429 or a 5xx. */
  again: boolean;
  /** What the answer's `retry-after` header asked for, in milliseconds. */
  retryAfterMs: number | null;
}

/** A `retry-after` header, which is either a count of seconds or an HTTP date. */
function retryAfterOf(header: string | null): number | null {
  if (header === null) return null;
  const seconds = Number(header.trim());
  if (Number.isFinite(seconds) && seconds >= 0) return Math.trunc(seconds * 1_000);
  const at = Date.parse(header);
  return Number.isNaN(at) ? null : Math.max(0, at - Date.now());
}

async function tryOnce(url: string, init: RequestInit, timeMs: number): Promise<Attempt> {
  try {
    const answer = await fetch(url, { ...init, signal: AbortSignal.timeout(timeMs) });
    const body = await answer.text();
    if (answer.ok) {
      return { status: answer.status, body, failure: null, again: false, retryAfterMs: null };
    }
    const again = answer.status === 429 || answer.status >= 500;
    return {
      status: answer.status,
      body,
      failure: `${answer.status} ${body.slice(0, DETAIL_CHARS)}`,
      again,
      retryAfterMs: retryAfterOf(answer.headers.get("retry-after")),
    };
  } catch (error) {
    const named = error instanceof Error ? `${error.name}: ${error.message}` : String(error);
    return { status: 0, body: "", failure: named, again: true, retryAfterMs: null };
  }
}

/** What the ladder came back with: the answered body, or the fault that ended it. */
interface Sent {
  body: string | null;
  attempts: number;
  fault: FaultKind | null;
  faultDetail: string | null;
}

/**
 * One call, with the retry ladder around it. The waits grow, each is drawn with jitter, and a
 * `retry-after` the answer carries raises the wait where it asks for a longer one. A 4xx other
 * than 429 ends the ladder at once: another try of the same request would be refused the same
 * way.
 */
async function send(url: string, init: RequestInit, bounds: CallBounds | undefined): Promise<Sent> {
  const timeMs = bounds?.timeMs ?? DEFAULT_TIME_MS;
  for (let attempt = 0; ; attempt += 1) {
    const tried = await tryOnce(url, init, timeMs);
    if (tried.failure === null) {
      return { body: tried.body, attempts: attempt + 1, fault: null, faultDetail: null };
    }
    const wait = tried.again ? RETRY_WAITS_MS[attempt] : undefined;
    if (wait === undefined) {
      return {
        body: null,
        attempts: attempt + 1,
        fault: tried.status === 0 ? "transport" : "status",
        faultDetail: scrubSecrets(tried.failure),
      };
    }
    const jittered = Math.round(wait * (1 + Math.random() * JITTER));
    await sleep(Math.min(LONGEST_WAIT_MS, Math.max(jittered, tried.retryAfterMs ?? 0)));
  }
}

/** An answer that came back in no shape this can read, as a result, with its body kept. */
function unreadable(detail: string, latencyMs: number, attempts: number): ModelAnswer {
  return {
    chosen: [],
    probabilities: null,
    confidence: null,
    inputTokens: null,
    outputTokens: null,
    latencyMs,
    attempts,
    stateCut: false,
    fault: "unreadable",
    faultDetail: scrubSecrets(detail).slice(0, DETAIL_CHARS),
  };
}

/** The text of a Messages answer: every text block of its content, joined. */
function claudeText(body: unknown): string | null {
  const text = listOf(recordOf(body)?.content)
    .map((block) => recordOf(block))
    .filter((block) => stringOf(block?.type) === "text")
    .map((block) => stringOf(block?.text) ?? "")
    .join("\n");
  return text.trim() === "" ? null : text;
}

/**
 * Why an answer that carried no text stopped, and what it did carry: `max_tokens, blocks
 * thinking` for the budget running out inside a thinking block. It heads that answer's fault
 * detail so a reader of `round.json` sees the cause without parsing the body after it.
 */
export function noTextDetail(body: unknown): string {
  const record = recordOf(body);
  const stop = stringOf(record?.stop_reason) ?? "no stop reason";
  const types = [
    ...new Set(
      listOf(record?.content).map((block) => stringOf(recordOf(block)?.type) ?? "unnamed")
    ),
  ];
  return `${stop}, ${types.length === 0 ? "no blocks" : `blocks ${types.join(", ")}`}`;
}

/** What an answer reports it read and wrote, as both endpoints name those counts. */
function usageOf(body: unknown): { inputTokens: number | null; outputTokens: number | null } {
  const usage = recordOf(recordOf(body)?.usage);
  return {
    inputTokens: numberOf(usage?.input_tokens),
    outputTokens: numberOf(usage?.output_tokens),
  };
}

/**
 * What may follow a label at the head of a line for the line to still be naming that label: a
 * separator, or a space and then a separator. A model that chose a label and then said why
 * writes one of these between the two.
 */
const AFTER_LABEL = /^ ?[-:,;.([{\u2013\u2014]/;

/** One line of an answer, or one label, without the punctuation a model writes around a label. */
function bare(line: string): string {
  return line
    .trim()
    .replace(/^[-*\s]+/, "")
    .replace(/^[`"']+|[`"'.,:;!]+$/g, "")
    .trim()
    .toLowerCase();
}

/**
 * The labels a Claude answer chose, matched without case and without that punctuation.
 *
 * A line that is one of the labels whole is the answer the prompt asked for, and every such line
 * is taken. Where no line is a label whole, a line that begins with a label and then qualifies
 * it - `Dismiss the review on #3170: one click, and the merge goes` - names that label, and is
 * taken only when exactly one label is named that way: a line naming two has chosen nothing.
 *
 * The second rule is a prefix with a separator rather than a search for the label anywhere in
 * the answer because an option's label is a sentence, and a sentence of one option appears
 * inside the text of another option - its label, its description, the reasoning quoting it -
 * often enough that a substring search would pick the wrong label. Requiring the label at the
 * head of a line, and a separator after it, also keeps a label apart from the longer label it is
 * a prefix of: `Dismiss` does not match the line `Dismiss the review on #3170: one click`,
 * because what follows it there is a word and not a separator. The label is stripped of that
 * punctuation the same way the line is, because a label of this corpus ends in a full stop
 * often enough to matter: with the stop left on, the line `Done, but with a different name or
 * size (say which in a comment).` would miss the label it is and match the label `Done`.
 *
 * An answer naming no label this way chose nothing, which is the `unreadable` fault.
 */
export function labelsOf(text: string, labels: readonly string[]): string[] {
  const wanted = new Map<string, string>();
  for (const label of labels) {
    const key = bare(label);
    if (key !== "" && !wanted.has(key)) wanted.set(key, label);
  }
  const lines = text.split("\n").map(bare);
  const chosen: string[] = [];
  for (const line of lines) {
    const label = wanted.get(line);
    if (label !== undefined && !chosen.includes(label)) chosen.push(label);
  }
  if (chosen.length > 0) return chosen;
  // A line that is a label and nothing else was taken above, so what is left to read here is a
  // label with something after it.
  const qualified = new Set<string>();
  for (const line of lines) {
    for (const [key, label] of wanted) {
      if (line.startsWith(key) && AFTER_LABEL.test(line.slice(key.length))) qualified.add(label);
    }
  }
  return qualified.size === 1 ? [...qualified] : [];
}

/**
 * One judgement put to a Claude model through the gateway. The answer is one of the labels the
 * prompt offered, on a line of its own or at the head of one, and anything else is the
 * `unreadable` fault carrying what came back, bounded at `DETAIL_CHARS`.
 */
export async function askClaude(ask: ClaudeAsk): Promise<ModelAnswer> {
  const endpoint = await claudeEndpoint();
  const started = Date.now();
  const sent = await send(
    endpoint.url,
    {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-api-key": endpoint.key,
        "anthropic-version": ANTHROPIC_VERSION,
      },
      body: JSON.stringify({
        model: ask.model,
        max_tokens: ask.bounds?.maxTokens ?? DEFAULT_MAX_TOKENS,
        system: ask.prompt.system,
        messages: [{ role: "user", content: ask.prompt.user }],
      }),
    },
    ask.bounds
  );
  const latencyMs = Date.now() - started;
  if (sent.body === null) {
    return {
      chosen: [],
      probabilities: null,
      confidence: null,
      inputTokens: null,
      outputTokens: null,
      latencyMs,
      attempts: sent.attempts,
      stateCut: false,
      fault: sent.fault,
      faultDetail: sent.faultDetail,
    };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(sent.body);
  } catch {
    return unreadable(sent.body, latencyMs, sent.attempts);
  }
  const text = claudeText(parsed);
  if (text === null) {
    return unreadable(`${noTextDetail(parsed)}\n${sent.body}`, latencyMs, sent.attempts);
  }
  const chosen = labelsOf(text, ask.prompt.labels);
  const usage = usageOf(parsed);
  if (chosen.length === 0) {
    const answer = unreadable(text, latencyMs, sent.attempts);
    return { ...answer, ...usage };
  }
  return {
    chosen,
    text,
    probabilities: null,
    confidence: null,
    ...usage,
    latencyMs,
    attempts: sent.attempts,
    stateCut: false,
    fault: null,
    faultDetail: null,
  };
}

/** Whether a refusal is the endpoint saying the state is larger than its budget. */
function refusedForSize(detail: string | null): boolean {
  if (detail === null) return false;
  return /too (?:large|long|big)|exceed|max(?:imum)?[ _-]?(?:tokens|length|size)|context|payload/i.test(
    detail
  );
}

/** `state` cut to `chars`, head and tail kept with the cut marked between them. */
function cutState(state: string, chars: number): string {
  if (state.length <= chars) return state;
  const keep = Math.max(1, Math.trunc((chars - JEV_CUT_MARK.length) / 2));
  return `${state.slice(0, keep)}${JEV_CUT_MARK}${state.slice(-keep)}`;
}

/**
 * One judgement put to Jev: the case as the state, one choice question over the options the case
 * offered. Where the endpoint refuses the state for its size the state is halved and sent again,
 * up to three times, and the answer is marked as cut, because a Jev reading over a cut case is
 * never compared with a Claude reading over a whole one without the reader seeing it.
 */
export async function askJev(ask: JevAsk): Promise<ModelAnswer> {
  const key = jevKey();
  const byName = new Map(ask.options.choices.map((choice) => [choice.name, choice.label]));
  const criteria: Record<string, string> = {};
  for (const choice of ask.options.choices) criteria[choice.name] = choice.description;
  const started = Date.now();
  let state = ask.state;
  let stateCut = false;
  let attempts = 0;
  let sent: Sent = { body: null, attempts: 0, fault: "transport", faultDetail: "no try was made" };
  for (let cut = 0; cut <= JEV_STATE_CUTS; cut += 1) {
    sent = await send(
      JEV_URL,
      {
        method: "POST",
        headers: { "content-type": "application/json", authorization: `Bearer ${key}` },
        body: JSON.stringify({
          model: ask.model,
          state,
          questions: {
            [JEV_QUESTION]: {
              type: "choice",
              instructions: ask.options.instructions,
              criteria,
            },
          },
        }),
      },
      ask.options.bounds
    );
    attempts += sent.attempts;
    if (sent.body !== null || !refusedForSize(sent.faultDetail) || state.length <= 1) break;
    state = cutState(state, Math.trunc(state.length / 2));
    stateCut = true;
  }
  const latencyMs = Date.now() - started;
  if (sent.body === null) {
    return {
      chosen: [],
      probabilities: null,
      confidence: null,
      inputTokens: null,
      outputTokens: null,
      latencyMs,
      attempts,
      stateCut,
      fault: sent.fault,
      faultDetail: sent.faultDetail,
    };
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(sent.body);
  } catch {
    return { ...unreadable(sent.body, latencyMs, attempts), stateCut };
  }
  // The answer's shape is the System One protocol's own, as `packages/envoy/agentloop/decide.go`
  // reads it: the answers keyed by the names the questions were asked under, each a decision
  // whose `choice`, `confidence` and `probabilities` a choice question fills.
  const decision = recordOf(recordOf(recordOf(parsed)?.answers)?.[JEV_QUESTION]);
  const choice = stringOf(decision?.choice);
  const label = choice === null ? undefined : byName.get(choice);
  if (label === undefined) {
    return { ...unreadable(sent.body, latencyMs, attempts), stateCut };
  }
  const probabilities: Record<string, number> = {};
  for (const [name, value] of Object.entries(recordOf(decision?.probabilities) ?? {})) {
    const probability = numberOf(value);
    if (probability !== null) probabilities[byName.get(name) ?? name] = probability;
  }
  return {
    chosen: [label],
    probabilities: Object.keys(probabilities).length === 0 ? null : probabilities,
    confidence: numberOf(decision?.confidence),
    ...usageOf(parsed),
    latencyMs,
    attempts,
    stateCut,
    fault: null,
    faultDetail: null,
  };
}
