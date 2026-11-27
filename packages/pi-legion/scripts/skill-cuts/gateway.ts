/**
 * The runner's one way to the model gateway. `createGateway` posts a body
 * through `skill-scenarios/messages-api.ts` (`postMessages`), the route and retry that
 * `label-comments.ts` uses too, to the gateway route of the body's API (`isResponsesBody`): an
 * Anthropic Messages API body to `<baseUrl>/v1/messages` with the key in `x-api-key`, and an OpenAI
 * Responses API body (one of `input` items, as Oh My Pi 18.18.6 sends for `openai/gpt-6.1-sol`,
 * F8) to `<baseUrl>/v1/responses` with the key as `authorization: Bearer <key>`:
 *
 * - The key is the text of `keyFile`, read inside this process for each try and held
 *   (`@legion/contracts/held-secrets`), so it is never printed, logged or written: every error
 *   `send` throws has the key, and each run of 10 or more of its characters, taken out of its
 *   message, its stack and its fields, whatever echoed it (a refusal that quotes the key it got).
 * - `send(body, {headers})` sends `headers` with a Messages body on every try: the `anthropic-*`
 *   headers Oh My Pi sent beside it (its `anthropic-beta` names the betas that the body's fields
 *   need, and the gateway answers 400 to such a body without it), and the routing header
 *   `x-litellm-session-id` (ROUTING_HEADER), whose value must be a UUID. Only those names are
 *   taken, and a name that can carry a key (`x-api-key`, `authorization`, any name holding `key`,
 *   `token`, `secret`, `auth`, `cookie`, `credential` or `password`) is refused by name before any
 *   call. A Responses body goes with no header of the caller's: one given is refused by name, the
 *   routing header among them.
 * - An outage (a 5xx, a 408 or a 429), a failed connection, a try past `timeoutMs` (its body read
 *   included; the try is ended) and a reply that does not read as a reply of the body's API are
 *   tried again, after the longer of the `retry-after` wait and the back-off (1 s, 2 s, 4 s, ... at
 *   most 30 s), up to
 *   GATEWAY_TRIES tries in all, whose waits add up to 91 s at least: more than the 60 s a
 *   gateway's rate guard looks back over, so a refusal that names a 1 s `retry-after` but holds a
 *   team back for a minute is outlasted. Any other status, a 400 among them, throws at once, and
 *   so does a streamed reply whose error's type (a Responses error's `code`) ends in `_policy`
 *   (the provider's content policy, as `cyber_policy`), as a MessagesApiError of the answer's
 *   status whose message is `the stream ended in an error: <type>: <message>` (STREAM_FAILED):
 *   the provider refuses the same content on each try, and each try is paid for.
 * - At most `concurrency` calls are in flight; the others wait their turn, in the order sent. A
 *   reply says when its call got its place and was sent and how many calls were in flight then
 *   (`sentAt`, `inFlight`; `onSent` is told the same as the call goes), and carries the
 *   gateway's headers of GATEWAY_HEADERS: the deployment and model group that answered, the
 *   fallbacks and retries the gateway made, and the provider's request id.
 * - `send` changes no byte of a body. `freshIdentity(body)` is how a caller gives a body a fresh
 *   session id (`metadata.user_id`, or a Responses body's `prompt_cache_key`) in that id's own
 *   form: the runner sends each run whose captured body names such an id with one of its own (a
 *   body that names none goes with none), and each judge call with one of its own. The gateway
 *   does not pin a Messages call's deployment by that id: it pins it by ROUTING_HEADER's value
 *   (the SHA-256 of it, placed on the deployments' weighted shares), and a call that names none
 *   by a hash of the caller, the model, the system prompt and the first messages, so every run of
 *   one body went to one deployment. The runner sends each run of a stage or a load test with a
 *   routing id of its round (replay.ts), and `deploymentOf` reads which deployment answered a
 *   reply, as an opaque prefix.
 * - A body that sets `stream` gets a streamed (SSE) reply, read whole before it counts
 *   (`streamedMessage`; `streamedResponse` for a Responses reply, F9); any other gets one JSON
 *   message. A Responses reply comes back in ReplyMessage's frame (`responseMessage`): its output
 *   items are its `content`, so every reader of a reply takes either API's (`toolCalls`).
 * - `spend()` sums each reply's usage (input not read from the cache, output, cache read, cache
 *   write, and a Responses reply's reasoning tokens) per model, and its dollars at list price
 *   (PRICES, or `prices` for a model the table lacks). A body whose model has no price is refused
 *   before any call, since its dollars could not be counted. Only a reply read whole is counted.
 *   A try that failed is not, even a streamed Messages reply that failed after its
 *   `message_start` had sent its input tokens: the gateway can bill such a try and this count
 *   leaves it out, so the spend can fall short of the bill by the tries that failed mid-stream.
 * - Once the dollars reach `budgetUsd`, no new call starts: `send` rejects with `BudgetSpent`,
 *   which says so. A call already in flight finishes and is counted, so the spend can pass the
 *   budget by at most the calls in flight, and the bill by those and the tries that failed
 *   mid-stream.
 * - Each ok answer of each try records one usage span through `spendRecorder`
 *   (`skill-scenarios/spend-record.ts`, README.md "Usage records"), whether the try's reply is
 *   then taken or tried again, since the provider bills each: postMessages gives it the answer's
 *   text whole, the body's model, the try's start and end, and the session id the gateway logs for
 *   the body (`gatewaySessionId`). A try that got no ok answer records nothing. `spendRecorder`
 *   null records nothing: a gateway whose answers are a test's stand-ins.
 */
import { createHash, randomBytes, randomUUID } from "node:crypto";
import { holdSecret, scrubError } from "@legion/contracts/held-secrets";
import { z } from "zod";
import {
  type AnswerTimes,
  MessagesApiError,
  postMessages,
  readKeyFile,
  sseData,
} from "../skill-scenarios/messages-api";
import type { Fetch, Sleep } from "../skill-scenarios/production-dispatch";
import { conversationId, type SpendRecorder } from "../skill-scenarios/spend-record";

/** How many times one call is tried before `send` fails: the back-off's waits before the last of
 *  them (1 + 2 + 4 + 8 + 16 + 30 + 30 s) pass the 60 s window a rate guard's refusal counts over. */
export const GATEWAY_TRIES = 8;

/** What a model charges, in USD per million tokens: prompt tokens not read from the cache, reply
 *  tokens (thinking and reasoning included), a cache write of the shorter lifetime and of the
 *  longer one (Anthropic's 5 minutes and 1 hour, OpenAI's 30 minutes and 12 hours), and a cache
 *  read. */
interface Rates {
  readonly input: number;
  readonly output: number;
  readonly cacheWriteShort: number;
  readonly cacheWriteLong: number;
  readonly cacheRead: number;
}

/** A model's list price: its rates, and the higher rates of a prompt longer than `over` tokens
 *  (input, cache read and cache write together) for a model priced by prompt length. */
export interface Price extends Rates {
  readonly longPrompt?: { readonly over: number; readonly rates: Rates };
}

/** Where PRICES come from. */
export const PRICE_SOURCE =
  "https://platform.claude.com/docs/en/about-claude/pricing (standard speed, global inference), read 2026-11-23, and https://developers.openai.com/api/docs/pricing (Standard), read 2026-11-20";

/** The list prices by model id: Anthropic's Claude models from PRICE_SOURCE's "Model pricing"
 *  table, and OpenAI's models from its "Standard" table. A reply of fast mode or of US-only
 *  inference costs more than this counts: the bodies Oh My Pi 18.18.6 sends (F2) set neither
 *  `speed` nor `inference_geo`, and each F5 reply's usage says `"speed": "standard"` and
 *  `"inference_geo": "global"`. So does a reply of another OpenAI tier (Fast, Ultrafast, Flex, a
 *  regional endpoint): the bodies Oh My Pi sends (F8) set no `service_tier`, each F9 reply says
 *  `"service_tier": "default"`, and the gateway's own `usage.cost` of each is these rates' count.
 *  OpenAI's long context is a prompt of more than 272,000 input tokens. */
export const PRICES: Readonly<Record<string, Price>> = {
  "claude-fable-5-2": {
    input: 8,
    output: 40,
    cacheWriteShort: 10,
    cacheWriteLong: 16,
    cacheRead: 0.2,
  },
  "claude-fable-5-1": {
    input: 10,
    output: 50,
    cacheWriteShort: 12.5,
    cacheWriteLong: 20,
    cacheRead: 0.25,
  },
  "claude-fable-5": {
    input: 10,
    output: 50,
    cacheWriteShort: 12.5,
    cacheWriteLong: 20,
    cacheRead: 1,
  },
  "claude-mythos-5-2": {
    input: 8,
    output: 40,
    cacheWriteShort: 10,
    cacheWriteLong: 16,
    cacheRead: 0.2,
  },
  "claude-mythos-5-1": {
    input: 10,
    output: 50,
    cacheWriteShort: 12.5,
    cacheWriteLong: 20,
    cacheRead: 0.25,
  },
  "claude-mythos-5": {
    input: 10,
    output: 50,
    cacheWriteShort: 12.5,
    cacheWriteLong: 20,
    cacheRead: 1,
  },
  "claude-opus-5-5": {
    input: 4,
    output: 20,
    cacheWriteShort: 5,
    cacheWriteLong: 8,
    cacheRead: 0.2,
  },
  "claude-opus-5": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-opus-4-8": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-opus-4-7": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-opus-4-6": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-opus-4-6-20260205": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-opus-4-7-20260416": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-opus-4-5": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-opus-4-5-20251101": {
    input: 5,
    output: 25,
    cacheWriteShort: 6.25,
    cacheWriteLong: 10,
    cacheRead: 0.5,
  },
  "claude-sonnet-5-5": {
    input: 2,
    output: 10,
    cacheWriteShort: 2.5,
    cacheWriteLong: 4,
    cacheRead: 0.1,
  },
  "claude-sonnet-5": {
    input: 2,
    output: 10,
    cacheWriteShort: 2.5,
    cacheWriteLong: 4,
    cacheRead: 0.2,
  },
  "claude-sonnet-4-6": {
    input: 3,
    output: 15,
    cacheWriteShort: 3.75,
    cacheWriteLong: 6,
    cacheRead: 0.3,
  },
  "claude-sonnet-4-5": {
    input: 3,
    output: 15,
    cacheWriteShort: 3.75,
    cacheWriteLong: 6,
    cacheRead: 0.3,
  },
  "claude-sonnet-4-5-20250929": {
    input: 3,
    output: 15,
    cacheWriteShort: 3.75,
    cacheWriteLong: 6,
    cacheRead: 0.3,
  },
  "claude-haiku-5-5": {
    input: 0.1,
    output: 0.5,
    cacheWriteShort: 0.125,
    cacheWriteLong: 0.2,
    cacheRead: 0.01,
    longPrompt: {
      over: 100_000,
      rates: {
        input: 0.5,
        output: 2.5,
        cacheWriteShort: 0.625,
        cacheWriteLong: 1,
        cacheRead: 0.05,
      },
    },
  },
  "claude-haiku-4-5": {
    input: 1,
    output: 5,
    cacheWriteShort: 1.25,
    cacheWriteLong: 2,
    cacheRead: 0.1,
  },
  "claude-haiku-4-5-20251001": {
    input: 1,
    output: 5,
    cacheWriteShort: 1.25,
    cacheWriteLong: 2,
    cacheRead: 0.1,
  },
  "gpt-6.1-sol": {
    input: 2,
    output: 10,
    cacheWriteShort: 2.5,
    cacheWriteLong: 5,
    cacheRead: 0.1,
    longPrompt: {
      over: 272_000,
      rates: { input: 4, output: 15, cacheWriteShort: 5, cacheWriteLong: 10, cacheRead: 0.2 },
    },
  },
};

/** A token count, as the API sends one. */
const Count = z.number().int().nonnegative();

/** A reply's `usage`, as the Messages API reports it. Fields it does not name are kept. */
const Usage = z.looseObject({
  input_tokens: Count,
  output_tokens: Count,
  cache_creation_input_tokens: Count.nullish(),
  cache_read_input_tokens: Count.nullish(),
  /** The cache write split by how long the cache keeps it. */
  cache_creation: z
    .looseObject({
      ephemeral_5m_input_tokens: Count.nullish(),
      ephemeral_1h_input_tokens: Count.nullish(),
    })
    .nullish(),
});
type Usage = z.infer<typeof Usage>;

/** A reply's `usage`, as OpenAI's Responses API reports it (F9): `input_tokens` counts every prompt
 *  token, the cache's reads (`cached_tokens`) and writes (`cache_write_tokens`, split by lifetime)
 *  among them, and `output_tokens` counts the reasoning tokens among it. Fields it does not name
 *  are kept (the gateway's own `cost` among them). */
const ResponsesUsage = z.looseObject({
  input_tokens: Count,
  output_tokens: Count,
  input_tokens_details: z
    .looseObject({
      cached_tokens: Count.nullish(),
      cache_write_tokens: Count.nullish(),
      cache_write_tokens_details: z
        .looseObject({ ttl_30m_tokens: Count.nullish(), ttl_12h_tokens: Count.nullish() })
        .nullish(),
    })
    .nullish(),
  output_tokens_details: z.looseObject({ reasoning_tokens: Count.nullish() }).nullish(),
});
type ResponsesUsage = z.infer<typeof ResponsesUsage>;

/** One block of a reply's content, kept whole so that a reply can be sent back as it came: a
 *  Messages reply's `text`, `thinking` (its signature included), `tool_use` and others, or a
 *  Responses reply's output items, `message`, `reasoning` (its `encrypted_content` included) and
 *  `function_call`. */
export const ContentBlock = z.looseObject({ type: z.string() });
export type ContentBlock = z.infer<typeof ContentBlock>;

/** A reply message: a Messages API message, whose fields it does not name (`id`,
 *  `stop_sequence`, ...) are kept; or a Responses API response in the same frame
 *  (`responseMessage`). */
export const ReplyMessage = z.looseObject({
  model: z.string().min(1),
  content: z.array(ContentBlock),
  stop_reason: z.string().nullable(),
  usage: Usage,
});
export type ReplyMessage = z.infer<typeof ReplyMessage>;

/** A Messages API request body: its model and messages, and every other field as the caller set
 *  it (`system`, `tools`, `thinking`, `max_tokens`, `stream`). */
export interface MessagesBody {
  readonly model: string;
  readonly messages: readonly unknown[];
  readonly stream?: boolean;
  readonly [field: string]: unknown;
}

/** An OpenAI Responses API request body: its model and input items, and every other field as the
 *  caller set it (`instructions`, `tools`, `reasoning`, `include`, `store`, `stream`; F8). */
export interface ResponsesBody {
  readonly model: string;
  readonly input: readonly unknown[];
  readonly stream?: boolean;
  readonly [field: string]: unknown;
}

/** A request body of either API. */
export type RequestBody = MessagesBody | ResponsesBody;

/** True for a Responses API body: one of `input` items, with no `messages`. */
export function isResponsesBody(body: RequestBody): body is ResponsesBody {
  return Array.isArray(body.input) && body.messages === undefined;
}

/** A UUID, any version, as Oh My Pi and Claude Code write a session id. */
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/** Whether `text` is a UUID, of any version. */
export function isUuid(text: string): boolean {
  return UUID.test(text);
}

/** Fewer hex digits than this in an id are too few to draw a fresh id from. */
const MIN_HEX_DIGITS = 16;
/** A word of an id that holds hex digits alone: a run of letters and digits between characters
 *  that are neither, such as a digest or one group of a UUID. */
const HEX_WORD = /(?<![0-9a-z])[0-9a-f]+(?![0-9a-z])/gi;

/** A fresh value in the form of the id text `value`: a new random UUID for a UUID; else each hex
 *  digit of each word of hex digits alone (HEX_WORD) drawn again at random, every other character
 *  kept, so a 64-digit hex digest stays one and the letters of a word such as `user` or `account`
 *  stay as they are; a text of fewer than MIN_HEX_DIGITS such digits becomes a new UUID. An empty
 *  text stays empty. */
function redrawnId(value: string): string {
  if (value === "") return value;
  if (UUID.test(value)) return randomUUID();
  const hex = (value.match(HEX_WORD) ?? []).join("");
  if (hex.length < MIN_HEX_DIGITS) return randomUUID();
  const upper = /[A-F]/.test(hex) && !/[a-f]/.test(hex);
  const drawn = randomBytes(hex.length);
  let at = 0;
  return value.replace(HEX_WORD, (word) =>
    word.replace(/./g, () => {
      const digit = ((drawn[at++] ?? 0) & 15).toString(16);
      return upper ? digit.toUpperCase() : digit;
    })
  );
}

/** `value` with each string inside it drawn again as `redrawnId` draws one, its keys, their
 *  order and every other value kept. */
function redrawnValues(value: unknown): unknown {
  if (typeof value === "string") return redrawnId(value);
  if (Array.isArray(value)) return value.map(redrawnValues);
  if (typeof value === "object" && value !== null) {
    return Object.fromEntries(
      Object.entries(value).map(([key, each]) => [key, redrawnValues(each)])
    );
  }
  return value;
}

/** A fresh id in the form of the id field's text `value`: a JSON object's strings each drawn
 *  again (`redrawnValues`: Oh My Pi's `{"session_id": …}`, Claude Code's `{"device_id": …,
 *  "account_uuid": "", "session_id": …}`), or the text itself drawn again. */
function freshIdText(value: string): string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    parsed = undefined;
  }
  return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed)
    ? JSON.stringify(redrawnValues(parsed))
    : redrawnId(value);
}

/**
 * `body` with a fresh id in the field its host names the session by, and nothing else changed:
 * a Messages body's `metadata.user_id` and a Responses body's `prompt_cache_key` (F8), each drawn
 * again in its own form (`freshIdText`). A gateway that routes or caches by that id makes it a
 * factor of every run that sends it: one body sent with two fixed ids and with a fresh one per
 * send, interleaved on one deployment, read the skill's reference in 8, 26 and 18 of 30 runs. A
 * body that holds no such id is given back as it is, or with `add` given one: `{"session_id":
 * <uuid>}` as Oh My Pi writes it, or a UUID as the cache key. `id` is the id sent, or null.
 */
export function freshIdentity(
  body: RequestBody,
  add = false
): { readonly body: RequestBody; readonly id: string | null } {
  if (isResponsesBody(body)) {
    const key = body.prompt_cache_key;
    if (typeof key !== "string" && !add) return { body, id: null };
    const id = typeof key === "string" ? freshIdText(key) : randomUUID();
    return { body: { ...body, prompt_cache_key: id }, id };
  }
  const metadata = body.metadata;
  const userId =
    typeof metadata === "object" && metadata !== null
      ? Reflect.get(metadata, "user_id")
      : undefined;
  if (typeof userId !== "string" && !add) return { body, id: null };
  const id =
    typeof userId === "string" ? freshIdText(userId) : JSON.stringify({ session_id: randomUUID() });
  const kept = typeof metadata === "object" && metadata !== null ? metadata : {};
  return { body: { ...body, metadata: { ...kept, user_id: id } }, id };
}

/** The id `body` names its session by: its `metadata.user_id` or `prompt_cache_key`, or null. */
export function bodyIdentity(body: RequestBody): string | null {
  const id = isResponsesBody(body)
    ? body.prompt_cache_key
    : typeof body.metadata === "object" && body.metadata !== null
      ? Reflect.get(body.metadata, "user_id")
      : undefined;
  return typeof id === "string" ? id : null;
}

/** The session id the gateway logs (`session_id`) for `body`, as its usage span sends it
 *  (`gen_ai.conversation.id`): the `session_id` of a Messages body's `metadata.user_id`, or a
 *  Responses body's `prompt_cache_key`, else its `user` (spend-record.ts's `conversationId`);
 *  undefined for a body that names none. */
export function gatewaySessionId(body: RequestBody): string | undefined {
  const user = isResponsesBody(body) && typeof body.user === "string" ? body.user : null;
  return conversationId(bodyIdentity(body) ?? user);
}

/** One tool call of a reply: its id, its tool's name and its input, as either API writes one (a
 *  Messages `tool_use` block, its `id` and its `input` object; a Responses `function_call` item,
 *  its `call_id` and its `arguments`, JSON text parsed here). A call whose arguments do not parse
 *  has the input undefined. */
export interface ToolCall {
  readonly id: string | undefined;
  readonly name: string | undefined;
  readonly input: unknown;
}

/** Whether a block of a reply's content is a tool call: a Messages `tool_use` block, or a
 *  Responses `function_call` item. */
export function isToolCallBlock(block: ContentBlock): boolean {
  return block.type === "tool_use" || block.type === "function_call";
}

/** The tool calls of a reply's content, in order. */
export function toolCalls(content: readonly ContentBlock[]): ToolCall[] {
  return content.flatMap((block): ToolCall[] => {
    if (!isToolCallBlock(block)) return [];
    const name = typeof block.name === "string" ? block.name : undefined;
    if (block.type === "tool_use") {
      return [
        { id: typeof block.id === "string" ? block.id : undefined, name, input: block.input },
      ];
    }
    let input: unknown;
    try {
      input = block.arguments === "" ? {} : JSON.parse(String(block.arguments));
    } catch {
      input = undefined;
    }
    return [{ id: typeof block.call_id === "string" ? block.call_id : undefined, name, input }];
  });
}

/** What one reply used: the model that answered, its tokens, and their dollars at list price. A
 *  Responses reply also says how many of its output tokens were reasoning (`reasoning`); a
 *  Messages reply counts its thinking in `output` and says no more. The schema reads one back
 *  from a file the runner wrote. */
export const SpentRecord = z.strictObject({
  model: z.string(),
  input: z.number(),
  output: z.number(),
  cacheRead: z.number(),
  cacheWrite: z.number(),
  reasoning: z.number().optional(),
  usd: z.number(),
});
export type Spent = Readonly<z.infer<typeof SpentRecord>>;

/** The dollars of `spent` (each a Spent, or any record of its `usd`), summed. */
export function usdOf(spent: readonly { readonly usd: number }[]): number {
  return spent.reduce((sum, each) => sum + each.usd, 0);
}

/** The reply headers of the gateway (LiteLLM) and its provider that the request log records:
 *  the deployment and the model group that answered, the fallbacks and retries the gateway made,
 *  and the provider's own request id, as the gateway forwards it. */
export const GATEWAY_HEADERS = [
  "x-litellm-model-id",
  "x-litellm-model-group",
  "x-litellm-attempted-fallbacks",
  "x-litellm-attempted-retries",
  "llm_provider-request-id",
  "llm_provider-x-request-id",
  "request-id",
] as const;

/** One model call's answer: the reply message, the reply's `request-id` header when it came with
 *  one (the provider's request id, as the gateway forwards it), how long the call took from its
 *  start (its retries included), and what it used. `gatewayHeaders` holds each of
 *  GATEWAY_HEADERS the reply came with; `providerRequestId` is the first of the provider's ids
 *  among them (the two `llm_provider-` ones, then `request-id`), or null. `sentAt` is when the
 *  call got its place among the calls in flight and was sent, and `inFlight` how many were in
 *  flight then, this one among them. */
export interface Reply {
  readonly message: ReplyMessage;
  readonly requestId?: string;
  readonly durationMs: number;
  readonly spent: Spent;
  readonly gatewayHeaders: Readonly<Record<string, string>>;
  readonly providerRequestId: string | null;
  readonly sentAt: string;
  readonly inFlight: number;
}

/** A deployment id that begins with 8 hex digits, which a deployment's opaque prefix is. */
const HEX_PREFIX = /^[0-9a-f]{8}/i;

/** The deployment that answered `reply`, as the runner records it: the first 8 hex digits of its
 *  `x-litellm-model-id`, in lower case, an opaque prefix that names no deployment's owner (an id
 *  that does not begin with 8 hex digits gives the first 8 of its SHA-256 instead, so no name is
 *  printed); or "" for a reply that carried none. */
export function deploymentOf(reply: Pick<Reply, "gatewayHeaders">): string {
  const id = reply.gatewayHeaders["x-litellm-model-id"];
  if (id === undefined || id === "") return "";
  const prefix = HEX_PREFIX.exec(id)?.[0] ?? createHash("sha256").update(id).digest("hex");
  return prefix.slice(0, 8).toLowerCase();
}

/** Runs counted by the deployment that answered them, each run given by its replies' deployments
 *  in order (`deploymentOf`, as a run records them): a run is counted once, under the deployment
 *  that answered each of its replies, under those deployments joined by `+` when more than one
 *  did, under `unnamed` when no reply named one, and under `no reply` when it has none (a replay
 *  that failed). The counts go in the order of their deployments' text. */
export function deploymentCounts(runs: readonly (readonly string[])[]): Map<string, number> {
  const counts = new Map<string, number>();
  for (const run of runs) {
    const named = [...new Set(run.filter((each) => each !== ""))];
    const key = run.length === 0 ? "no reply" : named.length === 0 ? "unnamed" : named.join("+");
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  return new Map([...counts].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)));
}

/** `deploymentCounts` of `runs` in words: `0a1b2c3d 5, 4e5f6a7b 5`, or `no runs`. */
export function deploymentWords(runs: readonly (readonly string[])[]): string {
  const counts = [...deploymentCounts(runs)];
  return counts.length === 0 ? "no runs" : counts.map(([key, n]) => `${key} ${n}`).join(", ");
}

/** One model's total: the replies counted, their tokens (the reasoning tokens among their output
 *  as the Responses API counts them, 0 for the Messages API) and their dollars at list price. */
export interface ModelSpend {
  readonly calls: number;
  readonly input: number;
  readonly output: number;
  readonly cacheRead: number;
  readonly cacheWrite: number;
  readonly reasoning: number;
  readonly usd: number;
}

/** Two spends of one model added field by field, `a`'s first; `b` itself when there is no `a`. */
export function addModelSpend(a: ModelSpend | undefined, b: ModelSpend): ModelSpend {
  if (a === undefined) return b;
  return {
    calls: a.calls + b.calls,
    input: a.input + b.input,
    output: a.output + b.output,
    cacheRead: a.cacheRead + b.cacheRead,
    cacheWrite: a.cacheWrite + b.cacheWrite,
    reasoning: a.reasoning + b.reasoning,
    usd: a.usd + b.usd,
  };
}

/** Every reply counted so far: per model, and the dollars of all of them. */
export interface Spend {
  readonly models: Record<string, ModelSpend>;
  readonly usd: number;
}

export interface GatewayOptions {
  /** The gateway's base URL: calls go to `<baseUrl>/v1/messages` and `<baseUrl>/v1/responses`. */
  readonly baseUrl: string;
  /** The file that holds the gateway key. */
  readonly keyFile: string;
  /** The most calls in flight at once. */
  readonly concurrency: number;
  /** The longest one try may take, its reply read whole included. */
  readonly timeoutMs: number;
  /** No new call starts once the dollars at list price reach this. */
  readonly budgetUsd?: number;
  /** Prices for models PRICES lacks, or in place of its own. */
  readonly prices?: Readonly<Record<string, Price>>;
  readonly fetch?: Fetch;
  readonly sleep?: Sleep;
  /** Where each ok answer records its usage span (spend-record.ts's recorder; the runner's is
   *  `processSpendRecorder()`), or null for none: a gateway whose answers are a test's
   *  stand-ins. */
  readonly spendRecorder: SpendRecorder | null;
}

/** When a call got its place and was sent, and how many calls were in flight then, this one
 *  among them. */
export interface Sent {
  readonly at: string;
  readonly inFlight: number;
}

/** What one `send` takes beside its body. */
export interface SendOptions {
  /** Headers sent with a Messages body, such as the `anthropic-beta` Oh My Pi sent beside it,
   *  which the body's fields need (adaptive thinking's `block_binding`, `context_management`,
   *  `output_config.effort`), and ROUTING_HEADER with a UUID. Only `anthropic-*` names and that
   *  one; a name that can carry a key is refused, and so is any header beside a Responses body. */
  readonly headers?: Readonly<Record<string, string>>;
  /** Told when the call gets its place and is sent, before its reply (or its failure) comes. */
  readonly onSent?: (sent: Sent) => void;
}

export interface Gateway {
  /** The reply to `body`. Rejects with `BudgetSpent` once the budget is reached. */
  send(body: RequestBody, options?: SendOptions): Promise<Reply>;
  /** Every reply counted so far, as a copy. */
  spend(): Spend;
}

/** A header name that can carry a credential: the gateway signs each call with the key file's key
 *  itself, and no other credential goes with a call. */
const KEY_BEARING = /key|token|secret|auth|cookie|credential|password/i;
/** The header names a caller may send: Anthropic's own. */
const ANTHROPIC_HEADER = /^anthropic-[a-z0-9-]+$/i;

/** The request header the gateway pins a Messages call's deployment by (see the file's header):
 *  a UUID, one for each round of a stage's or a load test's runs (replay.ts). The one header
 *  beside the `anthropic-*` ones that a caller may send, and only beside a Messages body. */
export const ROUTING_HEADER = "x-litellm-session-id";

/** Throws naming the first header of `headers` that is not sent with a call (beside a Responses
 *  body when `responses`): one whose name can carry a key; the routing header beside a Responses
 *  body, or with a value that is not a UUID; or one that is neither the routing header nor an
 *  `anthropic-*` header. Never names a value. */
function checkHeaders(headers: Readonly<Record<string, string>>, responses: boolean): void {
  for (const [name, value] of Object.entries(headers)) {
    if (name.toLowerCase() === ROUTING_HEADER) {
      if (responses) {
        throw new Error(
          `the header ${name} goes beside a Messages body only: a Responses API body goes with no header beside it`
        );
      }
      if (!isUuid(value)) {
        throw new Error(
          `the header ${name} takes a UUID, and its value is none: the gateway pins a call's deployment by that id`
        );
      }
      continue;
    }
    if (KEY_BEARING.test(name)) {
      throw new Error(
        `the header ${name} can carry a key, so it is not sent: the gateway signs each call with the key file's key itself`
      );
    }
    if (!ANTHROPIC_HEADER.test(name)) {
      throw new Error(
        `only anthropic-* headers and ${ROUTING_HEADER} are sent beside a body, not ${name}`
      );
    }
  }
}

/** The refusal of a call once the budget is reached. */
export class BudgetSpent extends Error {
  constructor(
    readonly budgetUsd: number,
    readonly spentUsd: number
  ) {
    super(
      `the budget of $${budgetUsd} is spent: the replies so far cost $${spentUsd.toFixed(4)} at list price, so no new model call starts`
    );
    this.name = "BudgetSpent";
  }
}

/** A Messages reply's tokens: input not read from the cache, output, cache reads and cache
 *  writes. */
function tokensOf(usage: Usage): Omit<Spent, "model" | "usd"> {
  return {
    input: usage.input_tokens,
    output: usage.output_tokens,
    cacheRead: usage.cache_read_input_tokens ?? 0,
    cacheWrite: usage.cache_creation_input_tokens ?? 0,
  };
}

/** A Responses reply's tokens, as tokensOf gives a Messages reply's: its input less the cache's
 *  reads and writes, its output (reasoning included), the reads and the writes; and the reasoning
 *  tokens among the output. */
export function responsesTokensOf(usage: ResponsesUsage): Omit<Spent, "model" | "usd"> {
  const cacheRead = usage.input_tokens_details?.cached_tokens ?? 0;
  const cacheWrite = usage.input_tokens_details?.cache_write_tokens ?? 0;
  return {
    input: usage.input_tokens - cacheRead - cacheWrite,
    output: usage.output_tokens,
    cacheRead,
    cacheWrite,
    reasoning: usage.output_tokens_details?.reasoning_tokens ?? 0,
  };
}

/** The dollars of `tokens` at `price`, `writeShort` of its cache write at the shorter lifetime's
 *  rate and the rest at the longer one's. */
function priced(tokens: Omit<Spent, "model" | "usd">, writeShort: number, price: Price): number {
  const prompt = tokens.input + tokens.cacheRead + tokens.cacheWrite;
  const rates =
    price.longPrompt !== undefined && prompt > price.longPrompt.over
      ? price.longPrompt.rates
      : price;
  const short = Math.min(tokens.cacheWrite, writeShort);
  return (
    (tokens.input * rates.input +
      tokens.output * rates.output +
      tokens.cacheRead * rates.cacheRead +
      short * rates.cacheWriteShort +
      (tokens.cacheWrite - short) * rates.cacheWriteLong) /
    1e6
  );
}

/** A Messages reply's dollars at `price`. A cache write the usage does not say is of 5 minutes is
 *  priced as one of 1 hour, the dearer, so that the count never falls short of the bill. */
export function costUsd(usage: Usage, price: Price): number {
  return priced(tokensOf(usage), usage.cache_creation?.ephemeral_5m_input_tokens ?? 0, price);
}

/** A Responses reply's dollars at `price`. A cache write the usage does not say is of 30 minutes
 *  is priced as one of 12 hours, the dearer, for the same reason. */
export function responsesCostUsd(usage: ResponsesUsage, price: Price): number {
  const short = usage.input_tokens_details?.cache_write_tokens_details?.ttl_30m_tokens ?? 0;
  return priced(responsesTokensOf(usage), short, price);
}

/** Each event of an SSE stream, in order: the JSON of its `data` lines. The `[DONE]` that closes
 *  a Responses stream (F9) is no event. */
function sseEvents(text: string): unknown[] {
  return sseData(text).map((data) => JSON.parse(data));
}

const Event = z.looseObject({ type: z.string() });
const Index = z.number().int().nonnegative();
const MessageStart = z.looseObject({
  message: z.looseObject({ usage: z.record(z.string(), z.unknown()) }),
});
const BlockStart = z.looseObject({ index: Index, content_block: ContentBlock });
const BlockDelta = z.looseObject({ index: Index, delta: z.looseObject({ type: z.string() }) });
const BlockStop = z.looseObject({ index: Index });
const MessageDelta = z.looseObject({
  delta: z.looseObject({}),
  usage: z.record(z.string(), z.unknown()).nullish(),
});
const StreamError = z.looseObject({
  error: z.looseObject({ type: z.string().optional(), message: z.string().optional() }),
});

/** How the error a streamed reply's reader throws on the stream's error begins (StreamFailed). */
export const STREAM_FAILED = "the stream ended in an error";

/** What a streamed reply's reader throws on the stream's `error` event (a Responses stream's
 *  `response.failed` too): `<STREAM_FAILED>: <type>: <message>`, its error's type (a Responses
 *  error's `code`; `error` where the event names none) kept as `type`. */
class StreamFailed extends Error {
  constructor(
    readonly type: string,
    said: string
  ) {
    super(`${STREAM_FAILED}: ${type}: ${said}`);
  }
}

/** The block at `index` of `blocks`, or a throw naming the event that names a block not started. */
function blockAt(blocks: Record<string, unknown>[], index: number, event: string) {
  const block = blocks[index];
  if (block === undefined)
    throw new Error(`the stream's ${event} names block ${index}, never started`);
  return block;
}

/** `text` plus `more`, where `text` is a string or nothing yet. */
function appended(text: unknown, more: unknown): string {
  return `${typeof text === "string" ? text : ""}${typeof more === "string" ? more : ""}`;
}

/**
 * The message a streamed (SSE) reply holds, put together whole as the Messages API's own clients
 * do: `message_start` gives the message, each block is started, grown by its deltas (text,
 * thinking and its signature, citations, and a tool call's input as JSON in pieces, parsed when
 * the block stops) and stopped, and `message_delta` sets the stop reason and the final usage
 * counts. A `ping` and an event of a kind not named here are passed over. Throws on an `error`
 * event, on a delta of a kind it does not know, and on a stream that ends before `message_stop`,
 * so that a reply cut short is never taken for a whole one.
 */
export function streamedMessage(text: string): unknown {
  let message: { usage: Record<string, unknown>; [field: string]: unknown } | undefined;
  const blocks: Record<string, unknown>[] = [];
  const json = new Map<number, string>();
  let stopped = false;
  for (const event of sseEvents(text)) {
    switch (Event.parse(event).type) {
      case "message_start":
        message = { ...MessageStart.parse(event).message };
        break;
      case "content_block_start": {
        const start = BlockStart.parse(event);
        blocks[start.index] = { ...start.content_block };
        break;
      }
      case "content_block_delta": {
        const { index, delta } = BlockDelta.parse(event);
        const block = blockAt(blocks, index, "content_block_delta");
        if (delta.type === "text_delta") block.text = appended(block.text, delta.text);
        else if (delta.type === "thinking_delta") {
          block.thinking = appended(block.thinking, delta.thinking);
        } else if (delta.type === "signature_delta") block.signature = delta.signature;
        else if (delta.type === "input_json_delta") {
          json.set(index, appended(json.get(index), delta.partial_json));
        } else if (delta.type === "citations_delta") {
          const citations = Array.isArray(block.citations) ? block.citations : [];
          block.citations = [...citations, delta.citation];
        } else throw new Error(`the stream holds a delta of a kind not known here: ${delta.type}`);
        break;
      }
      case "content_block_stop": {
        const { index } = BlockStop.parse(event);
        const block = blockAt(blocks, index, "content_block_stop");
        const input = json.get(index);
        if (input !== undefined) block.input = input === "" ? {} : JSON.parse(input);
        break;
      }
      case "message_delta": {
        if (message === undefined) {
          throw new Error("the stream's message_delta came before message_start");
        }
        const { delta, usage } = MessageDelta.parse(event);
        const counts = { ...message.usage };
        // The counts it sends are the totals so far; one it leaves out or sends null stays.
        for (const [field, value] of Object.entries(usage ?? {})) {
          if (value !== null && value !== undefined) counts[field] = value;
        }
        message = { ...message, ...delta, usage: counts };
        break;
      }
      case "message_stop":
        stopped = true;
        break;
      case "error": {
        const { error } = StreamError.parse(event);
        throw new StreamFailed(error.type ?? "error", error.message ?? "");
      }
    }
  }
  if (message === undefined) throw new Error("the stream holds no message_start");
  if (!stopped) throw new Error("the stream ended before message_stop");
  // A block index the stream never started leaves a hole, which `Object.keys` does not count.
  if (Object.keys(blocks).length !== blocks.length) {
    throw new Error("the stream left a content block out");
  }
  return { ...message, content: blocks };
}

/** The reply message `text` holds, streamed or not. Throws when it is none. */
function replyMessage(text: string, streamed: boolean): ReplyMessage {
  const parsed = ReplyMessage.safeParse(streamed ? streamedMessage(text) : JSON.parse(text));
  if (parsed.success) return parsed.data;
  const issue = parsed.error.issues[0];
  throw new Error(
    `the reply is not a Messages API message: ${issue?.path.join(".") || "the message"}: ${issue?.message}`
  );
}

const ResponseEvent = z.looseObject({ type: z.string(), response: z.unknown() });
const ErrorFields = z.looseObject({ code: z.string().nullish(), message: z.string().nullish() });
const StreamFailure = ErrorFields.extend({
  error: ErrorFields.nullish(),
  response: z.looseObject({ error: ErrorFields.nullish() }).nullish(),
});

/** What an `error` event (its own `code` and `message`, or its `error`'s) or a `response.failed`
 *  event (its response's `error`) of a Responses stream says, as StreamFailed. */
function streamError(event: unknown): StreamFailed {
  const failure: z.infer<typeof StreamFailure> = StreamFailure.safeParse(event).data ?? {};
  const said = failure.response?.error ?? failure.error ?? failure;
  return new StreamFailed(said.code ?? "error", said.message ?? "");
}

/**
 * The response a streamed (SSE) Responses reply holds: the one its last event, `response.completed`
 * or `response.incomplete`, carries whole, its output items and usage included (F9), as OpenAI's
 * own clients take it; the events before it hold nothing that one lacks. Throws on an `error` or
 * `response.failed` event, and on a stream that ends before either last event, so that a reply cut
 * short is never taken for a whole one.
 */
export function streamedResponse(text: string): unknown {
  let done: unknown;
  for (const event of sseEvents(text)) {
    const { type, response } = ResponseEvent.parse(event);
    if (type === "response.completed" || type === "response.incomplete") done = response;
    else if (type === "error" || type === "response.failed") {
      throw streamError(event);
    }
  }
  if (done === undefined) throw new Error("the stream ended before response.completed");
  return done;
}

/** A Responses API response, as far as the runner reads one. */
const ResponsesReply = z.looseObject({
  id: z.string().optional(),
  model: z.string().min(1),
  status: z.string(),
  output: z.array(ContentBlock),
  incomplete_details: z.looseObject({ reason: z.string().nullish() }).nullish(),
  usage: ResponsesUsage,
});

/** A Responses API response as a ReplyMessage: its id and model, its output items as `content`,
 *  its stop reason the reason it is incomplete, else its status (`completed`), and its usage as
 *  sent. Its echo of the request (`instructions`, `tools`) is left out. Beside it, its usage as
 *  ResponsesUsage reads it. Throws when it is no response, or one whose status is neither
 *  `completed` nor `incomplete`. */
function responseReply(response: unknown): { message: ReplyMessage; usage: ResponsesUsage } {
  const parsed = ResponsesReply.safeParse(response);
  if (!parsed.success) {
    const issue = parsed.error.issues[0];
    throw new Error(
      `the reply is not a Responses API response: ${issue?.path.join(".") || "the response"}: ${issue?.message}`
    );
  }
  const { id, model, status, output, incomplete_details, usage } = parsed.data;
  if (status !== "completed" && status !== "incomplete") {
    throw new Error(`the response's status is ${status}, neither completed nor incomplete`);
  }
  const message = ReplyMessage.parse({
    ...(id === undefined ? {} : { id }),
    model,
    content: output,
    stop_reason: incomplete_details?.reason ?? status,
    usage,
  });
  return { message, usage };
}

/** A Responses API response as a ReplyMessage (`responseReply`). */
export function responseMessage(response: unknown): ReplyMessage {
  return responseReply(response).message;
}

/** The gateway at `options.baseUrl`. Throws when an option is out of range. */
export function createGateway(options: GatewayOptions): Gateway {
  const { keyFile, concurrency, timeoutMs, budgetUsd, spendRecorder } = options;
  let url: URL;
  try {
    url = new URL(options.baseUrl);
  } catch {
    throw new Error(`the gateway's base URL is not a URL: ${options.baseUrl}`);
  }
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error(`the gateway's base URL is not http or https: ${options.baseUrl}`);
  }
  const baseUrl = options.baseUrl.replace(/\/+$/, "");
  if (!Number.isInteger(concurrency) || concurrency < 1) {
    throw new Error(`the gateway's concurrency is a whole number of 1 or more, not ${concurrency}`);
  }
  if (!Number.isFinite(timeoutMs) || timeoutMs <= 0) {
    throw new Error(`the gateway's timeout is a number of milliseconds above 0, not ${timeoutMs}`);
  }
  if (budgetUsd !== undefined && !(Number.isFinite(budgetUsd) && budgetUsd >= 0)) {
    throw new Error(`the budget is a number of dollars of 0 or more, not ${budgetUsd}`);
  }
  const prices: Readonly<Record<string, Price>> = { ...PRICES, ...options.prices };
  const fetch = options.fetch ?? globalThis.fetch;
  const sleep = options.sleep ?? Bun.sleep;
  // The key, read for each try and held, so that the scrub takes it out of every error.
  const key = async () => holdSecret(await readKeyFile(keyFile));

  const models: Record<string, ModelSpend> = {};
  let totalUsd = 0;
  let running = 0;
  const waiting: (() => void)[] = [];

  /** Counts `message`, the reply to a body whose model `bodyPrice` prices: a Responses reply when
   *  `responsesUsage` gives its usage, else a Messages reply. */
  function count(
    message: ReplyMessage,
    bodyPrice: Price,
    responsesUsage: ResponsesUsage | undefined
  ): Spent {
    // The model that answered is the one charged; the body's price stands where the table lacks it.
    const price = prices[message.model] ?? bodyPrice;
    const tokens =
      responsesUsage === undefined ? tokensOf(message.usage) : responsesTokensOf(responsesUsage);
    const usd =
      responsesUsage === undefined
        ? costUsd(message.usage, price)
        : responsesCostUsd(responsesUsage, price);
    const spent: Spent = { model: message.model, ...tokens, usd };
    models[spent.model] = addModelSpend(models[spent.model], {
      calls: 1,
      input: spent.input,
      output: spent.output,
      cacheRead: spent.cacheRead,
      cacheWrite: spent.cacheWrite,
      reasoning: spent.reasoning ?? 0,
      usd,
    });
    totalUsd += usd;
    return spent;
  }

  async function send(
    body: RequestBody,
    { headers = {}, onSent }: SendOptions = {}
  ): Promise<Reply> {
    const responses = isResponsesBody(body);
    checkHeaders(headers, responses);
    if (responses && Object.keys(headers).length > 0) {
      throw new Error(
        `a Responses API body goes with no header beside it, not ${Object.keys(headers).join(", ")}: the anthropic-* headers are the Messages API's`
      );
    }
    const bodyPrice = prices[body.model];
    if (bodyPrice === undefined) {
      throw new Error(
        `no list price for the model ${body.model}, so its dollars cannot be counted: add it to PRICES in gateway.ts from ${PRICE_SOURCE}, or pass its price`
      );
    }
    const streamed = body.stream === true;
    // A call's turn: one of `concurrency` places, in the order asked. A call that ends hands its
    // place to the first one waiting, so `running` stays the count of calls in flight.
    if (running < concurrency) running += 1;
    else {
      const { promise, resolve } = Promise.withResolvers<void>();
      waiting.push(resolve);
      await promise;
    }
    try {
      if (budgetUsd !== undefined && totalUsd >= budgetUsd) {
        throw new BudgetSpent(budgetUsd, totalUsd);
      }
      // The body's text is made once its call has a place, so at most `concurrency` texts are
      // held, and a call the budget refuses makes none.
      const text = JSON.stringify(body);
      const sentAt = new Date().toISOString();
      const inFlight = running;
      onSent?.({ at: sentAt, inFlight });
      const started = performance.now();
      const recorded = { model: body.model, sessionId: gatewaySessionId(body) };
      const answer = await postMessages({
        url: baseUrl,
        ...(responses ? { path: "/v1/responses", auth: "bearer" as const } : {}),
        key,
        body: text,
        headers,
        fetch,
        sleep,
        tries: GATEWAY_TRIES,
        timeoutMs,
        ...(spendRecorder === null
          ? {}
          : {
              onAnswer: (reply: string, times: AnswerTimes) =>
                spendRecorder.record(reply, { ...recorded, ...times }),
            }),
        accept: (reply, response) => {
          try {
            return {
              ...(responses
                ? responseReply(streamed ? streamedResponse(reply) : JSON.parse(reply))
                : { message: replyMessage(reply, streamed), usage: undefined }),
              requestId: response.headers.get("request-id") ?? undefined,
              gatewayHeaders: Object.fromEntries(
                GATEWAY_HEADERS.flatMap((name): [string, string][] => {
                  const value = response.headers.get(name);
                  return value === null ? [] : [[name, value]];
                })
              ),
            };
          } catch (error) {
            // A stream error whose type ends in `_policy` is the provider's content policy, which
            // refuses the same content on each try: it is given back, not thrown, since
            // postMessages tries again on a throw. It leaves `send` as postMessages' own errors
            // do, a MessagesApiError with the answer's status, which the request log records.
            if (error instanceof StreamFailed && error.type.endsWith("_policy")) {
              return { refused: new MessagesApiError(error.message, response.status) };
            }
            throw error;
          }
        },
      });
      if ("refused" in answer) throw answer.refused;
      const { message, usage, requestId, gatewayHeaders } = answer;
      const durationMs = Math.round(performance.now() - started);
      const spent = count(message, bodyPrice, usage);
      const providerRequestId =
        gatewayHeaders["llm_provider-request-id"] ??
        gatewayHeaders["llm_provider-x-request-id"] ??
        gatewayHeaders["request-id"] ??
        null;
      const sent = { gatewayHeaders, providerRequestId, sentAt, inFlight };
      return requestId === undefined
        ? { message, durationMs, spent, ...sent }
        : { message, requestId, durationMs, spent, ...sent };
    } catch (error) {
      // A refusal can quote the key it got: no part of the key leaves in an error.
      throw scrubError(error);
    } finally {
      const next = waiting.shift();
      if (next === undefined) running -= 1;
      else next();
    }
  }

  return {
    send,
    spend: () => ({ models: structuredClone(models), usd: totalUsd }),
  };
}
