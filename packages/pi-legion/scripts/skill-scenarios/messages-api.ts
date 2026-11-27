/**
 * The Anthropic Messages API route and retry that label-comments.ts and skill-cuts/gateway.ts both
 * use, so one loop decides when a model call is tried again. gateway.ts posts to the gateway's
 * OpenAI Responses route through the same loop (`path` and `auth`).
 *
 * Exports:
 * - `ANTHROPIC_VERSION`: the `anthropic-version` header every call with the `x-api-key` key sends.
 * - `readKeyFile(file)`: the key a file holds, read now. `runKeyCommand(file)`: the key a program
 *   prints, run now. Each throws naming the file or the program, never the key.
 * - `modelError(status, text)`: why a call failed, from the error JSON when the body is one (the
 *   Messages API's and OpenAI's share its shape, `{"error": {"type", "message"}}`).
 * - `retryAfterMs(response)`: the wait a `retry-after` header asks for, at most 60 s, or 0.
 * - `postMessages(call)`: one POST of `call.body` to `<call.url><call.path>` (`/v1/messages` by
 *   default) with the key got again from `call.key()` for each try: in the `x-api-key` header
 *   beside `anthropic-version` (`call.auth` `x-api-key`, the default), or as
 *   `authorization: Bearer <key>` (`bearer`, OpenAI's form); and `call.headers` beside it (names
 *   taken in lower case; one may replace `anthropic-version`, none the key's header or
 *   `content-type`). Each try has `call.timeoutMs`, its body read included; a try past it is
 *   ended and counts as a failed try. A failed connection, a body that fails after its headers, a
 *   timeout, an outage (`outage`: a 5xx, a 408 or a 429), or an ok answer that
 *   `call.accept(text, response)` throws on is tried again, after the longer of the `retry-after`
 *   wait (an outage only) and `backoffMs` (1 s, 2 s, 4 s, ... at most 30 s), up to `call.tries`
 *   tries in all. Any other status (a 400, a 401, a 404) throws at once. It returns what `accept`
 *   returns for the first ok answer it takes. A failure
 *   throws `MessagesApiError`, whose message is the last try's reason (`the model answered
 *   <status> <type>: <message>`, `the model call failed: <why>`, or `accept`'s own message),
 *   followed by `, on each of <n> tries` when every try failed, and whose `status` is the HTTP
 *   status of the last try's answer (its headers came, even when its body then failed), or
 *   undefined when the last try got no answer. `accept` gets the body whole as text: a streamed
 *   (SSE) reply is read to its end before it. Each ok answer, its body read whole, is first given
 *   to `call.onAnswer` with when its try was sent and when its body ended (spend-record.ts
 *   records its usage there), whether `accept` then takes it or not: the provider billed it
 *   either way. A try whose body failed, and an answer that is not ok, give it nothing.
 * - `sseData(text, wanted)`: the `data` of each event of an SSE stream, in order, an event's
 *   `data` lines joined by newlines; an event with no data, the `[DONE]` that closes a Responses
 *   stream, and an event whose text `wanted` (when given) does not match are none.
 */
import { outage } from "@legion/contracts/http-status";
import { messageFor } from "@legion/envoy-client/errors";
import { backoffMs, type Fetch, type Sleep } from "./production-dispatch";

export const ANTHROPIC_VERSION = "2023-06-01";
/** The longest the key command may run. */
const KEY_COMMAND_TIMEOUT_MS = 30_000;

/** The key that `file` holds, read now. Throws naming the file, never the key. */
export async function readKeyFile(file: string): Promise<string> {
  const key = (await Bun.file(file).text()).trim();
  if (key === "") throw new Error(`the key file ${file} is empty`);
  return key;
}

/** The key that the program `file` prints, run now. Throws naming the program, never its output. */
export async function runKeyCommand(file: string): Promise<string> {
  const child = Bun.spawn([file], { stdout: "pipe", stderr: "ignore", stdin: "ignore" });
  const timer = setTimeout(() => child.kill(), KEY_COMMAND_TIMEOUT_MS);
  const [output, code] = await Promise.all([new Response(child.stdout).text(), child.exited]);
  clearTimeout(timer);
  if (code !== 0) throw new Error(`the key command ${file} exited ${code}`);
  const key = output.trim();
  if (key === "") throw new Error(`the key command ${file} printed no key`);
  return key;
}

/** Why the model call failed, from Anthropic's error JSON when the body is one. */
export function modelError(status: number, text: string): string {
  try {
    const shape = JSON.parse(text) as { error?: { type?: unknown; message?: unknown } };
    const { type, message } = shape.error ?? {};
    if (typeof type === "string" && typeof message === "string") {
      return `the model answered ${status} ${type}: ${message}`;
    }
  } catch {
    // The body is not JSON. The line below says so.
  }
  return `the model answered ${status}`;
}

/** The wait that a `retry-after` header asks for, in milliseconds, or 0. */
export function retryAfterMs(response: Response): number {
  const seconds = Number(response.headers.get("retry-after"));
  return Number.isFinite(seconds) && seconds > 0 ? Math.min(60_000, seconds * 1000) : 0;
}

/** A model call that failed: its reason, and the last answer's HTTP status when one came. */
export class MessagesApiError extends Error {
  constructor(
    message: string,
    readonly status: number | undefined
  ) {
    super(message);
    this.name = "MessagesApiError";
  }
}

/** One model call, as `postMessages` makes it. */
export interface MessagesCall<T> {
  /** The route's base URL with no trailing slash: the call goes to `<url><path>`. */
  readonly url: string;
  /** The route's path, `/v1/messages` by default. */
  readonly path?: string;
  /** The key, got again for each try. */
  readonly key: () => Promise<string>;
  /** How the key goes: in the `x-api-key` header, beside `anthropic-version` (the default), or as
   *  `authorization: Bearer <key>`, with no `anthropic-version` (OpenAI's form). */
  readonly auth?: "x-api-key" | "bearer";
  /** The request body, as JSON text. */
  readonly body: string;
  /** Headers sent beside the call's own, such as the `anthropic-beta` a body's fields need. Each
   *  name is taken in lower case; one may replace `anthropic-version`, but never the key's header
   *  or `content-type`, which the call sets after them. */
  readonly headers?: Readonly<Record<string, string>>;
  readonly fetch: Fetch;
  readonly sleep: Sleep;
  /** How many times the call is tried before it fails. */
  readonly tries: number;
  /** The longest one try may take, its body read included. */
  readonly timeoutMs: number;
  /** Takes an ok answer, its body read whole. A throw fails the try, and the call goes again. */
  readonly accept: (text: string, response: Response) => T | Promise<T>;
  /** Told of each ok answer, its body read whole, before `accept` takes it: its text, when its
   *  try was sent and when its body ended (epoch milliseconds). spend-record.ts's recorder records
   *  the answer's usage here; it never throws and never waits. None when absent. */
  readonly onAnswer?: (answer: string, times: AnswerTimes) => void;
}

/** When one try was sent and when its answer's body ended, in epoch milliseconds. */
export interface AnswerTimes {
  readonly startedAt: number;
  readonly endedAt: number;
}

/** The `data` of each event of the SSE stream `text`, in order: an event's `data` lines joined
 *  by newlines. An event with no data, and the `[DONE]` that closes the gateway's Responses
 *  stream, are none; so is each event whose text `wanted` does not match, when it is given. */
export function sseData(text: string, wanted?: RegExp): string[] {
  const data: string[] = [];
  for (const chunk of text.split(/\r?\n\r?\n/)) {
    if (wanted !== undefined && !wanted.test(chunk)) continue;
    const joined = chunk
      .split(/\r?\n/)
      .filter((line) => line.startsWith("data:"))
      .map((line) => line.slice("data:".length).replace(/^ /, ""))
      .join("\n");
    const trimmed = joined.trim();
    if (trimmed !== "" && trimmed !== "[DONE]") data.push(joined);
  }
  return data;
}

/** What `accept` makes of the first ok answer it takes. Throws `MessagesApiError`. */
export async function postMessages<T>(call: MessagesCall<T>): Promise<T> {
  let last = "";
  let status: number | undefined;
  let wait = 0;
  for (let attempt = 1; attempt <= call.tries; attempt++) {
    if (attempt > 1) await call.sleep(Math.max(wait, backoffMs(attempt - 1)));
    wait = 0;
    status = undefined;
    let response: Response;
    let text: string;
    let startedAt: number;
    let endedAt: number;
    try {
      const key = await call.key();
      startedAt = Date.now();
      response = await call.fetch(`${call.url}${call.path ?? "/v1/messages"}`, {
        method: "POST",
        headers: {
          ...(call.auth === "bearer" ? {} : { "anthropic-version": ANTHROPIC_VERSION }),
          ...Object.fromEntries(
            Object.entries(call.headers ?? {}).map(([name, value]) => [name.toLowerCase(), value])
          ),
          ...(call.auth === "bearer" ? { authorization: `Bearer ${key}` } : { "x-api-key": key }),
          "content-type": "application/json",
        },
        body: call.body,
        signal: AbortSignal.timeout(call.timeoutMs),
      });
      // The answer came: its status stands even when its body fails below.
      status = response.status;
      // A connection that fails while the body arrives is a failed call, as one before the headers.
      text = await response.text();
      endedAt = Date.now();
    } catch (error) {
      last = `the model call failed: ${messageFor(error)}`;
      continue;
    }
    if (!response.ok) {
      last = modelError(response.status, text);
      if (outage(response.status)) {
        wait = retryAfterMs(response);
        continue;
      }
      throw new MessagesApiError(last, status);
    }
    // Outside the tries' catch, so nothing `onAnswer` does can send the call again.
    call.onAnswer?.(text, { startedAt, endedAt });
    try {
      return await call.accept(text, response);
    } catch (error) {
      // A sampled reply can be one the caller cannot use. Ask again.
      last = messageFor(error);
    }
  }
  throw new MessagesApiError(`${last}, on each of ${call.tries} tries`, status);
}
