/**
 * The one Messages API call of the skill-scenario tools that ask a model directly:
 * label-comments.ts and heldout-reader.ts. A tool names its route with --key-file <file> (a 0600
 * file that holds an Anthropic key, read again for each try, the call going to
 * https://api.anthropic.com) or --gateway <url> with --key-command <file> (a program that prints the
 * gateway key, run again for each try, the call going to <gateway>/v1/messages). The key goes in
 * the `x-api-key` header and never into an error. An outage (`outage`: a 5xx, a 408 or a 429), and
 * a call that fails before its body has arrived, is tried again after a wait; any other answer that
 * is not 200 fails at once.
 */
import { statSync } from "node:fs";
import { outage } from "@legion/contracts/http-status";
import { messageFor } from "@legion/envoy-client/errors";
import { backoffMs, type Fetch, type Sleep } from "./production-dispatch";

/** Anthropic's own endpoint, for the --key-file route. */
const ANTHROPIC_URL = "https://api.anthropic.com";
const ANTHROPIC_VERSION = "2023-06-01";
/** The longest the key command, every process it starts included, may run. */
const KEY_COMMAND_TIMEOUT_MS = 30_000;

/** Where the model calls go, and how each call gets its key. */
export interface ModelRoute {
  readonly url: string;
  readonly key: () => Promise<string>;
}

/** The route flags a tool takes, as given. */
export interface RouteFlags {
  readonly gateway?: string;
  readonly keyCommand?: string;
  readonly keyFile?: string;
}

/** How one call is tried: how many times, and how long one try may take. */
export interface CallOptions {
  readonly fetch: Fetch;
  readonly sleep: Sleep;
  readonly tries: number;
  readonly timeoutMs: number;
}

/** The key that `file` holds, read now. Throws naming the file, never the key. */
async function readKeyFile(file: string): Promise<string> {
  const key = (await Bun.file(file).text()).trim();
  if (key === "") throw new Error(`the key file ${file} is empty`);
  return key;
}

/** The key that the program `file` prints, run now. Throws naming the program, never its output.
 * The program runs in a process group of its own, and at `limitMs` (KEY_COMMAND_TIMEOUT_MS unless
 * a test names a shorter limit) the whole group is killed: a wrapper's own child that hangs (a
 * `curl` to a metadata server) holds the output pipe open after the wrapper itself is gone, and the
 * key is read before a try's own timeout starts. */
export async function runKeyCommand(
  file: string,
  limitMs = KEY_COMMAND_TIMEOUT_MS
): Promise<string> {
  const child = Bun.spawn([file], {
    stdout: "pipe",
    stderr: "ignore",
    stdin: "ignore",
    detached: true,
  });
  let stopped = false;
  const timer = setTimeout(() => {
    stopped = true;
    try {
      process.kill(-child.pid, "SIGKILL");
    } catch {
      // Every process of the group has exited already.
    }
  }, limitMs);
  try {
    const [output, code] = await Promise.all([new Response(child.stdout).text(), child.exited]);
    if (stopped)
      throw new Error(
        `the key command ${file} ran past its limit of ${limitMs} ms, so every process it started was stopped`
      );
    if (code !== 0) throw new Error(`the key command ${file} exited ${code}`);
    const key = output.trim();
    if (key === "") throw new Error(`the key command ${file} printed no key`);
    return key;
  } finally {
    clearTimeout(timer);
  }
}

/** The model route that the flags name, or the refusal. */
export function modelRoute(flags: RouteFlags): ModelRoute | string {
  const { gateway, keyCommand, keyFile } = flags;
  const refusal =
    "name one model route: --key-file <file>, or --gateway <url> with --key-command <file>";
  if (keyFile !== undefined) {
    if (gateway !== undefined || keyCommand !== undefined) return refusal;
    let mode: number;
    try {
      mode = statSync(keyFile).mode;
    } catch (error) {
      return `--key-file ${keyFile} cannot be read: ${messageFor(error)}`;
    }
    if ((mode & 0o077) !== 0) {
      return `--key-file ${keyFile} can be read by its group or by others. Make it 0600.`;
    }
    return { url: ANTHROPIC_URL, key: () => readKeyFile(keyFile) };
  }
  if (gateway === undefined || keyCommand === undefined) return refusal;
  let url: URL;
  try {
    url = new URL(gateway);
  } catch {
    return `--gateway ${gateway} is not a URL`;
  }
  if (url.protocol !== "https:" && url.protocol !== "http:") {
    return `--gateway ${gateway} is not an http or https URL`;
  }
  return { url: gateway.replace(/\/+$/, ""), key: () => runKeyCommand(keyCommand) };
}

/** Why the model call failed, from Anthropic's error JSON when the body is one. */
function modelError(status: number, text: string): string {
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
function retryAfterMs(response: Response): number {
  const seconds = Number(response.headers.get("retry-after"));
  return Number.isFinite(seconds) && seconds > 0 ? Math.min(60_000, seconds * 1000) : 0;
}

/**
 * Posts `body` to the route's `/v1/messages` and answers `read` of the 200 answer's text. A `read`
 * that throws asks again, as an outage or a failed connection does, up to `tries` tries in
 * all. Throws naming why the last try failed.
 */
export async function askModel<T>(
  route: ModelRoute,
  body: string,
  read: (text: string) => T,
  options: CallOptions
): Promise<T> {
  let last = "";
  let wait = 0;
  for (let attempt = 1; attempt <= options.tries; attempt++) {
    if (attempt > 1) await options.sleep(Math.max(wait, backoffMs(attempt - 1)));
    wait = 0;
    let response: Response;
    let text: string;
    try {
      response = await options.fetch(`${route.url}/v1/messages`, {
        method: "POST",
        headers: {
          "x-api-key": await route.key(),
          "anthropic-version": ANTHROPIC_VERSION,
          "content-type": "application/json",
        },
        body,
        signal: AbortSignal.timeout(options.timeoutMs),
      });
      // A connection that fails while the body arrives is a failed call, as one before the headers.
      text = await response.text();
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
      throw new Error(last);
    }
    try {
      return read(text);
    } catch (error) {
      last = messageFor(error);
    }
  }
  throw new Error(`${last}, on each of ${options.tries} tries`);
}
