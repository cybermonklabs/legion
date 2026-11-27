/**
 * What the skill-cuts commands share: a whole-file write, a read of a file's bytes at an offset,
 * JSON text and a JSON file's value as a refusal, the lines of a JSON-lines text and file
 * (`jsonTextLines`, `jsonLines`), a count in words, a SHA-256 in hex, a zod error's faults on one
 * line, one line of text as a schema, strict option parsing, an option's directory as a refusal
 * (`mustBeDirectory`), the configuration as a refusal, the two errors a command's `main` turns
 * into its exit status (`Refused` 2, `Stopped` 3), a SKILL.md's text as Oh My Pi reads, splits and
 * names it (`ompText`, `splitFrontmatter`, `readFrontmatter`, `ompSkillName`: the runner's one
 * frontmatter reader), the one rule by which a gh call or a registry's request that failed is
 * sent again (`GhRetries`), with the words a registry request's failure is read in
 * (`httpFailure`, `fetchFailure`), and a read started beside others (`started`). It imports no
 * other command's file, so every one of them can import it.
 */
import { createHash } from "node:crypto";
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  readFileSync,
  readSync,
  renameSync,
  statSync,
  writeFileSync,
} from "node:fs";
import path from "node:path";
import { parseArgs } from "node:util";
import { messageFor } from "@legion/envoy-client/errors";
import { z } from "zod";
import { loadConfig, type SkillCutsConfig } from "./config";

/** A command's refusal of its arguments or inputs, before any store read or model call: its
 *  `main` exits 2. */
export class Refused extends Error {}

/** A run that stops on its own rule (the budget is spent, or the memory passed its limit): its
 *  `main` exits 3. */
export class Stopped extends Error {}

/** Writes `text` to `file` whole: into a fresh file beside it that is then renamed over it. */
export function writeWhole(file: string, text: string): void {
  mkdirSync(path.dirname(file), { recursive: true });
  const fresh = `${file}.${process.pid}.tmp`;
  writeFileSync(fresh, text);
  renameSync(fresh, file);
}

/** The bytes `length` of the file `file` from `offset`. */
export function bytesAt(file: string, offset: number, length: number): Buffer {
  const bytes = Buffer.alloc(length);
  const fd = openSync(file, "r");
  try {
    for (let at = 0; at < length; ) {
      const read = readSync(fd, bytes, at, length - at, offset + at);
      if (read === 0) throw new Error(`${file} ends before byte ${offset + length}`);
      at += read;
    }
  } finally {
    closeSync(fd);
  }
  return bytes;
}

/** `value` as JSON, two spaces deep, with a final newline. */
export function json(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

/** The SHA-256 of `bytes`, in hex. */
export function sha256(bytes: string | Uint8Array): string {
  return createHash("sha256").update(bytes).digest("hex");
}

/** Each file of `files`: its path and the SHA-256 of its text. */
export function fileDigests(
  files: readonly { readonly path: string; readonly text: string }[]
): { path: string; sha256: string }[] {
  return files.map((file) => ({ path: file.path, sha256: sha256(file.text) }));
}

/** One line of text: not blank, no line break. */
export const OneLine = z
  .string()
  .refine((text) => text.trim() !== "" && !text.includes("\n"), "is not one non-blank line");

/** `text` as JSON, or undefined when it is none. */
export function parseJson(text: string): unknown {
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return undefined;
  }
}

/** The JSON value the file `file` holds, read as `what` (`--store-only`, `the input`). Throws a
 *  refusal naming the file when it cannot be read or is not JSON. */
export function readJson(file: string, what: string): unknown {
  let text: string;
  try {
    text = readFileSync(file, "utf8");
  } catch (error) {
    throw new Refused(`cannot read ${what} ${file}: ${messageFor(error)}`);
  }
  try {
    return JSON.parse(text);
  } catch (error) {
    throw new Refused(`${what} ${file} is not JSON: ${messageFor(error)}`);
  }
}

/** Each line of the JSON-lines text `text` that is not blank, with its line number (from 1) and
 *  its value (`parseJson`: undefined for a line that is no JSON). */
export function* jsonTextLines(
  text: string
): Generator<{ readonly at: number; readonly value: unknown }> {
  for (const [index, line] of text.split("\n").entries()) {
    if (line.trim() === "") continue;
    yield { at: index + 1, value: parseJson(line) };
  }
}

/** The lines of the JSON-lines file `file`, read as UTF-8 (`jsonTextLines`); none when the file is
 *  missing. load.ts reads its run files by it, and cli.ts the request log. */
export function* jsonLines(
  file: string
): Generator<{ readonly at: number; readonly value: unknown }> {
  if (!existsSync(file)) return;
  yield* jsonTextLines(readFileSync(file, "utf8"));
}

/** `count` with `word`, or with `plural` unless the count is 1: `1 session`, `2 sessions`. */
export function counted(count: number, word: string, plural = `${word}s`): string {
  return `${count} ${count === 1 ? word : plural}`;
}

/** Each issue of a zod error as `<path>: <message>`, joined by `; `; an issue of the whole value
 *  is named `root`. */
export function schemaFaults(error: z.ZodError, root = "the value"): string {
  return error.issues
    .map((issue) => `${issue.path.join(".") || root}: ${issue.message}`)
    .join("; ");
}

/** The options `parse` takes, as `parseArgs` takes them. */
type ParseOptions = NonNullable<Parameters<typeof parseArgs>[0]>["options"];

/** The values `parse` gives for the options `O`: each option's value, or undefined. */
export type OptionValues<O extends ParseOptions> = ReturnType<
  typeof parseArgs<{ args: string[]; options: O; strict: true; allowPositionals: false }>
>["values"];

/** Parses `argv` with `options`, strictly, with no positional argument. Throws a refusal that
 *  ends with `usage`. */
export function parse<O extends ParseOptions>(
  argv: readonly string[],
  options: O,
  usage: string
): OptionValues<O> {
  try {
    return parseArgs({ args: [...argv], options, strict: true, allowPositionals: false }).values;
  } catch (error) {
    throw new Refused(`${messageFor(error)}. ${usage}`);
  }
}

/** The value of each option `names` names, or a refusal naming the ones missing. */
export function required<K extends string>(
  values: Partial<Record<K, unknown>>,
  names: readonly K[],
  usage: string
): Record<K, string> {
  const missing = names.filter((name) => typeof values[name] !== "string");
  if (missing.length > 0) {
    throw new Refused(`${missing.map((name) => `--${name}`).join(", ")} required. ${usage}`);
  }
  return values as Record<K, string>;
}

/** A refusal unless `dir`, the value of the option `flag` (`--new`, `--skill`), is a directory. */
export function mustBeDirectory(flag: string, dir: string): void {
  if (!statSync(dir, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Refused(`${flag} ${dir} is not a directory`);
  }
}

/** The configuration in `file`. Throws a refusal naming why it is none. */
export function configFrom(file: string): SkillCutsConfig {
  try {
    return loadConfig(file);
  } catch (error) {
    throw new Refused(messageFor(error));
  }
}

/** The first line of `text` once the whole text is trimmed: its leading white space goes, and so
 *  does its trailing white space when it is the last line; "" when it has none. */
export function firstLine(text: string): string {
  return text.trim().split("\n")[0] ?? "";
}

/** A file's text as Oh My Pi reads a file (capability/fs.ts's `readFile`, `Bun.file(…).text()`):
 *  decoded as UTF-8 with a leading byte order mark dropped, so a SKILL.md that starts with one
 *  still opens with its frontmatter's `---`. Takes the file's bytes, or a text already decoded (a
 *  GraphQL blob's `text`, a file read as UTF-8), whose leading mark it drops the same way. Every
 *  SKILL.md the runner parses is read through it. */
export function ompText(raw: Uint8Array | string): string {
  return typeof raw === "string" ? raw.replace(/^\uFEFF/, "") : new TextDecoder().decode(raw);
}

/**
 * A SKILL.md's text split as Oh My Pi's `parseFrontmatter` splits it (packages/utils/src/
 * frontmatter.ts of cybermonklabs/oh-my-pi at the tag v18.18.6-sami.20261119-162325, the build
 * the runner pins, arms.ts; the file is the same at rig.sh's v18.18.6-sami.20261117-013345):
 * CRLF read as LF; with `repair` (its default, which the skill loader's parse takes), the HTML
 * comments before an opening `---` dropped, with the white space after them
 * (`stripLeadingHtmlComments`); then the frontmatter block, from after that `---` to the next line
 * that starts `---`, and the body, the text after that `\n---`, trimmed. A text that opens with no
 * `---`, or has no such line, has no block (`undefined`), and its body is the whole text,
 * untrimmed. `frontmatterFields` reads the block; the body is what Oh My Pi's `resolveCollision`
 * compares (`repair`, capture.ts's `listedSkills`) and what an autoloaded block holds (no
 * `repair`, arms.ts's `autoloadedText`).
 */
export function splitFrontmatter(
  content: string,
  repair: boolean
): { readonly block: string | undefined; readonly body: string } {
  let text = content.replace(/\r\n?/g, "\n");
  const comments = repair ? /^(?:(?:\s*<!--[\s\S]*?-->)+)/.exec(text) : null;
  if (comments && /^\s*---/.test(text.slice(comments[0].length))) {
    text = text.slice(comments[0].length).replace(/^\s+/, "");
  }
  const end = text.startsWith("---") ? text.indexOf("\n---", 3) : -1;
  if (end === -1) return { block: undefined, body: text };
  return { block: text.slice(4, end), body: text.slice(end + 4).trim() };
}

/** An ambiguous plain scalar of a frontmatter line, as Oh My Pi finds one. */
const PLAIN_SCALAR_KEY_VALUE = /^(\s*[A-Za-z_][\w-]*:\s+)(\S.*?)(\s*)$/;
/** The first characters of a value that is no plain scalar. */
const FLOW_OR_EXPLICIT_VALUE_START = new Set(['"', "'", "[", "{", "|", ">", "!", "&", "*", "#"]);

/** `text`'s frontmatter block parsed as one YAML mapping, tabs read as two spaces, or null. */
function yamlRecord(text: string): Record<string, unknown> | null {
  const loaded: unknown = Bun.YAML.parse(text.replaceAll("\t", "  "));
  if (typeof loaded !== "object" || loaded === null || Array.isArray(loaded)) return null;
  return { ...loaded };
}

/**
 * `text` with each `<!--` … `-->` span dropped, as one pass of Oh My Pi's
 * `replace(/<!--[\s\S]*?-->/g, "")` drops them in packages/utils/src/frontmatter.ts at the pin,
 * so the runner reads the description Oh My Pi reads: a span runs from a `<!--` to the first
 * `-->` that starts after it, and the scan goes on after that `-->`, so the text a drop joins
 * together is not read again (`<!<!---->--` gives `<!--`, as Oh My Pi's pass does). A `<!--` with
 * no `-->` after it stays. This is no sanitizer: its output is frontmatter text for a YAML parse
 * and is never rendered as HTML.
 */
function dropComments(text: string): string {
  let kept = "";
  let at = 0;
  for (;;) {
    const open = text.indexOf("<!--", at);
    if (open === -1) break;
    const close = text.indexOf("-->", open + 4);
    if (close === -1) break;
    kept += text.slice(at, open);
    at = close + 3;
  }
  return kept + text.slice(at);
}

/** `value` with each kebab-case key of each mapping in it, at every depth, in camelCase (`foo-bar`
 *  as `fooBar`; a key it then shares takes the later value), as Oh My Pi's
 *  `normalizeFrontmatterKeys` gives a skill's frontmatter (packages/utils/src/frontmatter.ts at
 *  the pin). A YAML parse gives plain mappings, lists and scalars alone. */
function camelKeys(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(camelKeys);
  if (typeof value !== "object" || value === null) return value;
  return Object.fromEntries(
    Object.entries(value).map(([key, inner]) => [
      key.replace(/-([a-z])/g, (_, letter: string) => letter.toUpperCase()),
      camelKeys(inner),
    ])
  );
}

/**
 * The fields of a frontmatter block (`splitFrontmatter`'s, with `repair`), read as Oh My Pi's
 * `parseFrontmatter` reads a skill's with its defaults (packages/utils/src/frontmatter.ts of
 * cybermonklabs/oh-my-pi at the tag v18.18.6-sami.20261119-162325, the build the runner pins, the
 * same file as at v18.18.6-sami.20261117-013345): the comments inside it dropped; parsed as YAML
 * with each tab replaced by two spaces, a block that is no mapping giving none (`{}`); when that
 * fails, parsed again with each plain scalar that holds `: ` quoted
 * (`quoteAmbiguousPlainScalars`); when that fails too, each `key: value` line read on its own, a
 * value that does not parse as YAML kept as its text. Kebab-case keys become camelCase, at every
 * depth (`camelKeys`).
 */
export function frontmatterFields(raw: string): Record<string, unknown> {
  const block = dropComments(raw);
  let read: Record<string, unknown> | null | undefined;
  try {
    read = yamlRecord(block);
  } catch {
    let quoted = false;
    const repaired = block
      .split("\n")
      .map((line) => {
        const match = PLAIN_SCALAR_KEY_VALUE.exec(line);
        const value = match?.[2]?.trimEnd() ?? "";
        if (!value.includes(": ") || FLOW_OR_EXPLICIT_VALUE_START.has(value[0] ?? "")) return line;
        quoted = true;
        return `${match?.[1]}${JSON.stringify(value)}${match?.[3]}`;
      })
      .join("\n");
    try {
      read = quoted ? yamlRecord(repaired) : undefined;
    } catch {
      read = undefined;
    }
    if (read === undefined) {
      read = {};
      for (const line of block.split("\n")) {
        const match = /^([\w-]+):\s*(.*)$/.exec(line);
        if (match?.[1] === undefined) continue;
        const text = (match[2] ?? "").trim();
        let value: unknown = text;
        if (text !== "") {
          try {
            const parsed: unknown = Bun.YAML.parse(text);
            if ((parsed !== null && typeof parsed !== "object") || Array.isArray(parsed)) {
              value = parsed;
            }
          } catch {
            // The value's own text stands.
          }
        }
        read[match[1]] = value;
      }
    }
  }
  return camelKeys(read ?? {}) as Record<string, unknown>;
}

/** The frontmatter of a SKILL.md's (or an agent definition's) text as Oh My Pi reads it: the
 *  block `splitFrontmatter` finds with `repair` (CRLF as LF, leading HTML comments before the
 *  opening `---` dropped), read by `frontmatterFields`. A text with no block has none (`{}`). */
export function readFrontmatter(content: string): Record<string, unknown> {
  const { block } = splitFrontmatter(content, true);
  return block === undefined ? {} : frontmatterFields(block);
}

/** The name Oh My Pi gives a skill whose frontmatter is `frontmatter` and whose directory is
 *  named `dirName` (discovery/helpers.ts:546-548 at the pin), which it lists the skill under and
 *  finds a collision by (extensibility/skills.ts:172, :459-462): the frontmatter's `name`,
 *  trimmed, when it is a string that trims to something; else `dirName`. */
export function ompSkillName(
  frontmatter: Readonly<Record<string, unknown>>,
  dirName: string
): string {
  const name = frontmatter.name;
  return typeof name === "string" && name.trim() !== "" ? name.trim() : dirName;
}

/** A rate limit, primary or secondary, as gh reports it. */
export const RATE_LIMITED = /rate limit|HTTP 429/i;
/** The most times one call GitHub refused for its rate limit is sent again. */
export const MAX_RATE_RETRIES = 5;
/** gh's error text for a failure the same call may not meet again, with its reason: the union of
 *  the two lists the runner had before it shared one rule, copies.ts's and context.ts's. The
 *  connection timed out, was reset or refused, or ended early; gh's answer ended early, so gh
 *  could not parse it (`unexpected end of JSON input`: gh read a large body, such as a recursive
 *  tree of 17 MB, cut short, and the same call gives it whole again); GitHub's end reset the
 *  HTTP/2 stream of gh's answer (`stream error: stream ID <n>; <code>; received from peer`, gh's
 *  words: the same tree's read ended so on 2026-11-24); GitHub's front end answered
 *  a 5xx, or a registry a 429 (`HTTP 429 (too many requests)`, the words of the registry reads'
 *  `httpFailure` alone: GitHub's 429 is a rate limit, RATE_LIMITED, waited out at most
 *  MAX_RATE_RETRIES times and then not sent again); the gh process ran past the time limit its
 *  caller gave it and was ended (context.ts's reads have one); a name did not resolve, or did not
 *  resolve yet; a pipe broke; or the text names a timeout in any other words. The first pattern
 *  that matches gives the reason, so a more particular reason comes before a more general one. A
 *  call that fails so is sent again (`GhRetries.waitToRetry`); no other failure is. */
const TRANSIENT_FAILURES: readonly (readonly [reason: string, pattern: RegExp])[] = [
  ["TLS handshake timeout", /TLS handshake timeout/i],
  ["connection reset", /connection reset|reset by peer|ECONNRESET/i],
  ["connection refused", /connection refused|ECONNREFUSED/i],
  ["unexpected EOF", /unexpected EOF/i],
  ["body cut short", /unexpected end of JSON input/i],
  ["stream reset", /stream error: stream ID \d+; [A-Z_]+; received from peer/],
  ["i/o timeout", /i\/o timeout/i],
  ["HTTP 502", /HTTP 502\b/],
  ["HTTP 503", /HTTP 503\b/],
  ["HTTP 504", /HTTP 504\b/],
  ["other HTTP 5xx", /HTTP 5\d\d/i],
  ["HTTP 429", /HTTP 429 \(too many requests\)/],
  ["time limit", /timed out after \d+ ms/],
  ["timed out", /timed out/i],
  ["TLS handshake", /TLS handshake/i],
  ["no such host", /no such host/i],
  ["temporary failure", /temporary failure/i],
  ["broken pipe", /broken pipe/i],
  ["timeout", /timeout/i],
];
/** The reasons of TRANSIENT_FAILURES that are GitHub's answer to a GraphQL batch too large: a batch
 *  of more than one item that fails so is split by its caller at once, not sent again. Beside a
 *  502 and a 504 that is `timeout`, which GitHub's refusal of a query too costly matches ("This
 *  may be the result of a timeout"); a TLS handshake or i/o timeout matches its own reason first,
 *  so a batch that meets one is sent again. */
const BATCH_TOO_LARGE: Readonly<Record<string, true>> = {
  "HTTP 502": true,
  "HTTP 504": true,
  timeout: true,
};
/** The waits before each try again of a call that failed for a transient reason: at most 4 tries
 *  more. */
const TRANSIENT_WAITS_MS: readonly number[] = [5_000, 15_000, 45_000, 135_000];

/** The reason of TRANSIENT_FAILURES that the error text `stderr` names, or undefined when it names
 *  none. */
export function transientReason(stderr: string): string | undefined {
  return TRANSIENT_FAILURES.find(([, pattern]) => pattern.test(stderr))?.[0];
}

/** The error of a call whose tries failed with `errors`, in their order: the one error, or each
 *  try's. */
export function triesError(errors: readonly string[]): string {
  if (errors.length === 1) return errors[0] ?? "";
  return `${errors.length} tries: ${errors.map((error, index) => `try ${index + 1}: ${error}`).join("; ")}`;
}

/** The reason, in the words of TRANSIENT_FAILURES, of each error code with which Bun's fetch fails
 *  a connection that a later try may make (Bun 1.4.4: a connection refused, or reset or closed
 *  before the answer ended; a name getaddrinfo did not resolve, or did not resolve yet), so a
 *  registry read that fails so is sent again as a gh read that fails so is. */
const CONNECTION_FAILURES: Readonly<Record<string, string>> = {
  ECONNREFUSED: "connection refused",
  ECONNRESET: "connection reset",
  ENOTFOUND: "no such host",
  EAI_AGAIN: "temporary failure",
};

/** The failure of a registry's answer of `status`, one that is not OK, in the words of
 *  TRANSIENT_FAILURES: a 429, a registry's "too many requests", is sent again as a 5xx is
 *  (`HTTP 429`), and no other 4xx is. The registry reads' one wording: build.ts's npm tarball
 *  read's and pod-image.ts's image config read's. */
export function httpFailure(status: number): string {
  return status === 429 ? "HTTP 429 (too many requests)" : `HTTP ${status}`;
}

/** The failure, in the words of TRANSIENT_FAILURES, of a registry read's fetch that threw `error`
 *  under `signal`, the read's time limit of `timeoutMs`: that limit, once `signal` has ended the
 *  fetch, or a connection that failed (CONNECTION_FAILURES, with fetch's own words); undefined for
 *  any other error, which its caller throws as fetch threw it. The failure is read from these words
 *  alone, never from the URL, so a package named connect-timeout makes no failure transient. */
export function fetchFailure(
  error: unknown,
  signal: AbortSignal,
  timeoutMs: number
): string | undefined {
  if (signal.aborted) return `timed out after ${timeoutMs} ms`;
  const code = error instanceof Error && "code" in error ? error.code : undefined;
  if (typeof code !== "string" || !Object.hasOwn(CONNECTION_FAILURES, code)) return undefined;
  return `${CONNECTION_FAILURES[code]} (${messageFor(error)})`;
}

/** What one try of a gh call gave, as `GhRetries` reads it. */
export interface GhTry {
  readonly code: number;
  readonly stderr: string;
}

/** What `GhRetries.send` gives: the try it ended on, whose `stderr` is each try's error
 *  (`triesError`) when there were several, and then `lastStderr` is that last try's own error. */
export type Sent<T extends GhTry> = T & { readonly lastStderr?: string };

/** The error text of the try a call `GhRetries.send` made ended on. A call is sent again only
 *  after a transient failure or a rate limit, so a last try that failed so is a call that failed
 *  so on every try, and one that failed for good (a 404) after a 502 failed for good. */
export function lastTryError(result: Sent<GhTry>): string {
  return result.lastStderr ?? result.stderr;
}

/** Where `GhRetries` waits, and says that it does. */
export interface RetryIo {
  sleep(ms: number): Promise<void>;
  /** Writes one line to standard error. */
  err(line: string): void;
}

/** Where a command says each retry of its gh calls, and how it waits. */
export interface CommandRetryIo {
  /** Writes one line to standard error. */
  err(line: string): void;
  /** The wait before a gh call is sent again; Bun.sleep unless given. */
  sleep?(ms: number): Promise<void>;
}

/** The GhRetries of the command `who`: each retry said on `io.err`, each wait `io.sleep`'s, else
 *  Bun.sleep's. */
export function commandRetries(io: CommandRetryIo, who: string): GhRetries {
  return new GhRetries(
    { sleep: (ms) => io.sleep?.(ms) ?? Bun.sleep(ms), err: (line) => io.err(line) },
    who
  );
}

/** `promise`, a read started now beside others and awaited later in its turn: its rejection is
 *  marked handled, so a read not yet awaited when an earlier one throws is no unhandled
 *  rejection. Awaiting it still throws. */
export function started<T>(promise: Promise<T>): Promise<T> {
  promise.catch(() => undefined);
  return promise;
}

/** How a retry's line names a gh call whose caller gives it no name of its own. */
const A_GH_CALL = "a gh call";

/**
 * The one rule by which the runner sends a gh call again (copies.ts's reads, build.ts's
 * `GithubReads`, context.ts's `ghSource`), and a registry's request (build.ts's npm tarball read,
 * pod-image.ts's image config read), with its count of the calls it sent again. A call whose
 * error names a transient failure (TRANSIENT_FAILURES) is sent again after 5, 15, 45 and 135 s, at
 * most 4 times more (TRANSIENT_WAITS_MS), each retry counted by its reason and said on standard
 * error; but a GraphQL batch of more than one item that GitHub answers as a batch too large
 * (BATCH_TOO_LARGE: a 502, a 504, or its refusal of a query too costly) is not, so that its caller
 * splits it at once. A call GitHub refused for its rate limit is sent again after the wait its
 * caller gives (`send`'s `rateWait`), at most MAX_RATE_RETRIES times. No other failure is sent
 * again: a 404, a 401 or 403 that is no rate limit, and an answer not of its shape fail at once. A
 * call that fails on every try fails as it would on one, its error naming each try's.
 */
export class GhRetries {
  /** The calls sent again after a transient failure, by reason, in TRANSIENT_FAILURES' order. */
  readonly counts = new Map(TRANSIENT_FAILURES.map(([reason]): [string, number] => [reason, 0]));

  /** `who` names the command whose calls they are on each line said. */
  constructor(
    private readonly io: RetryIo,
    private readonly who: string
  ) {}

  /** Writes `line` to standard error as a line of `who`'s. */
  say(line: string): void {
    this.io.err(`${this.who}: ${line}`);
  }

  /** Whether a call that failed with `stderr`, sent again `retried` times so far, is sent again:
   *  when `stderr` names a transient failure, and not one of BATCH_TOO_LARGE for a batch its caller
   *  `splits`, and fewer than 4 retries were made, this counts the retry by its reason, says so on
   *  standard error (naming the call as `read` does, A_GH_CALL when absent) and sleeps the next
   *  wait of TRANSIENT_WAITS_MS. A caller that holds a read slot calls it outside the slot, so the
   *  wait holds none. */
  async waitToRetry(
    stderr: string,
    retried: number,
    options: { readonly splits?: boolean; readonly read?: string } = {}
  ): Promise<boolean> {
    const reason = transientReason(stderr);
    const wait = TRANSIENT_WAITS_MS[retried];
    if (
      reason === undefined ||
      wait === undefined ||
      (options.splits === true && Object.hasOwn(BATCH_TOO_LARGE, reason))
    )
      return false;
    this.counts.set(reason, (this.counts.get(reason) ?? 0) + 1);
    this.say(
      `${options.read ?? A_GH_CALL} failed (${firstLine(stderr)}); sending it again in ${wait / 1000} s, retry ${retried + 1} of ${TRANSIENT_WAITS_MS.length}`
    );
    await this.io.sleep(wait);
    return true;
  }

  /** The first try of the call `send` makes that exits 0, else the last try, which no rule sends
   *  again, its `stderr` each try's error (`triesError`) when there were several, and its own
   *  error then kept as `lastStderr` (`lastTryError`). `splits`: the call is a GraphQL batch of
   *  more than one item, which its caller splits. `rateWait`: the milliseconds to wait before a
   *  call GitHub refused for its rate limit is sent again, at its `refused`-th refusal (from 0);
   *  without it a rate limit is not waited out. Each such wait is said on standard error first:
   *  the call, gh's first error line, the wait and when it ends. `send` takes any read slot
   *  itself, so no wait holds one. */
  async send<T extends GhTry>(
    send: () => Promise<T>,
    options: {
      readonly splits?: boolean;
      readonly rateWait?: (stderr: string, refused: number) => Promise<number>;
      /** What the call is, in the line said on each retry (A_GH_CALL when absent). */
      readonly read?: string;
    } = {}
  ): Promise<Sent<T>> {
    const errors: string[] = [];
    let limited = 0;
    let transient = 0;
    for (;;) {
      const result = await send();
      if (result.code === 0) return result;
      errors.push(firstLine(result.stderr) || `gh exited ${result.code}`);
      if (
        options.rateWait !== undefined &&
        limited < MAX_RATE_RETRIES &&
        RATE_LIMITED.test(result.stderr)
      ) {
        const wait = await options.rateWait(result.stderr, limited);
        // A rate wait can be 15 minutes: it says itself, so it is never taken for a hang.
        this.say(
          `${options.read ?? A_GH_CALL} was refused for its rate limit (${firstLine(result.stderr)}); waiting ${Math.round(wait / 1000)} s, until ${new Date(Date.now() + wait).toISOString()}, before sending it again, refusal ${limited + 1} of ${MAX_RATE_RETRIES}`
        );
        await this.io.sleep(wait);
        limited++;
      } else if (await this.waitToRetry(result.stderr, transient, options)) {
        transient++;
      } else if (errors.length === 1) return result;
      else return { ...result, stderr: `${triesError(errors)}\n`, lastStderr: result.stderr };
    }
  }

  /** The retries counted, with those of `before` (an earlier run's record) added: each reason with
   *  at least one, in TRANSIENT_FAILURES' order, as a summary records them. */
  record(before: Readonly<Record<string, number>> = {}): Record<string, number> {
    const record: Record<string, number> = {};
    for (const [reason, count] of this.counts) {
      const all = count + (before[reason] ?? 0);
      if (all > 0) record[reason] = all;
    }
    return record;
  }
}

/** Runs at most `limit` tasks at once: each task past the limit waits until one ends, the first to
 *  wait starting first. */
export class Limiter {
  #running = 0;
  readonly #waiting: (() => void)[] = [];

  constructor(readonly limit: number) {
    if (!Number.isInteger(limit) || limit < 1) {
      throw new Error(`a limit of ${limit} tasks at once is not a whole number of 1 or more`);
    }
  }

  /** `task`'s result, once a place is free and the task has run. */
  async run<T>(task: () => Promise<T>): Promise<T> {
    if (this.#running < this.limit) this.#running += 1;
    else await new Promise<void>((resolve) => this.#waiting.push(resolve));
    try {
      return await task();
    } finally {
      // The place passes to the first task that waits, or is freed.
      const next = this.#waiting.shift();
      if (next === undefined) this.#running -= 1;
      else next();
    }
  }
}

/** The environment variable that bounds the gh processes the runner has at once. */
const GH_CONCURRENCY_ENV = "SKILL_CUTS_GH_CONCURRENCY";
/** The gh processes the runner has at once when GH_CONCURRENCY_ENV is unset: a pod's gh is a
 *  wrapper of about 145 MiB a call (a binary and a leak check), so the bound keeps the runner's
 *  processes near 600 MiB however many reads one profile starts. */
export const GH_CONCURRENCY = 4;
let ghLimit: Limiter | undefined;

/** The one bound on concurrent gh processes of the whole runner: every gh spawn runs inside
 *  `ghLimiter().run(...)`. Its limit is GH_CONCURRENCY_ENV's value (a whole number of 1 or more),
 *  else GH_CONCURRENCY; throws on another value. */
export function ghLimiter(): Limiter {
  if (ghLimit === undefined) {
    const given = process.env[GH_CONCURRENCY_ENV];
    if (given !== undefined && !/^[1-9]\d*$/.test(given)) {
      throw new Error(`${GH_CONCURRENCY_ENV}=${given} is not a whole number of 1 or more`);
    }
    ghLimit = new Limiter(given === undefined ? GH_CONCURRENCY : Number(given));
  }
  return ghLimit;
}

/** A child's time limit (`timeLimit`): `touch()` starts its time again (a limit on time without
 *  output), `timedOut()` says whether the limit ended the child, `clear()` stops it, and `ended`
 *  settles when it fires. */
export interface TimeLimit {
  touch(): void;
  timedOut(): boolean;
  clear(): void;
  readonly ended: Promise<void>;
}

/** Ends `child` (SIGTERM) once `ms` pass, if `ms` is given; each `touch()` starts the `ms` again.
 *  `timedOut()` says whether the limit ended it, so a child a signal from elsewhere ended is not
 *  reported as out of time. Every child the runner starts has a limit, and every one but three
 *  gets it here: copies.ts's and context.ts's gh calls (`gh <call> timed out after <ms> ms`, a
 *  transient failure), the store reads (psql-rows.ts's `eachStoreRow`), and, ended with
 *  `pastLimit`'s reason, context.ts's commands (`tar`, `bun install`, `bun run build`, `go
 *  test`), `omp --version` (capture.ts), Claude Code's `--version` and `claude plugin` commands
 *  (load.ts), a registry's token command (pod-image.ts) and `--after-case` (cli.ts). The Oh My Pi
 *  run (capture.ts) and the Claude Code run (load.ts), which write their output to files, keep
 *  their own timer, which ends them with SIGKILL; `count-sql --run`'s transcript command
 *  (countsql.ts), which runs alone and synchronously, is ended by `Bun.spawnSync`'s own
 *  `timeout`, with `pastLimit`'s reason. */
export function timeLimit(child: { kill(): void }, ms: number | undefined): TimeLimit {
  let timedOut = false;
  let timer: ReturnType<typeof setTimeout> | undefined;
  let fire = () => {};
  const ended = new Promise<void>((resolve) => {
    fire = resolve;
  });
  const start = () => {
    if (ms === undefined) return;
    clearTimeout(timer);
    timer = setTimeout(() => {
      timedOut = true;
      child.kill();
      fire();
    }, ms);
  };
  start();
  return { touch: start, timedOut: () => timedOut, clear: () => clearTimeout(timer), ended };
}

/** Why a child that ran past its limit (`timeLimit`) was ended: the command and the limit, in one
 *  wording for every command the runner starts. */
export function pastLimit(command: string, ms: number): string {
  return `\`${command}\` ran past its limit of ${ms} ms, so it was ended`;
}

/** Gives each chunk of `stream` to `take` until the stream ends, or until `limit` ends its child:
 *  the reader is then cancelled, which ends a pending read at once, since a process the child
 *  started (psql under the store's wrapper script, the binary under gh's) can hold the stream open
 *  after the child is gone. A chunk is let go once `take` returns. */
export async function eachChunk(
  stream: ReadableStream<Uint8Array>,
  limit: TimeLimit,
  take: (chunk: Uint8Array) => void
): Promise<void> {
  const reader = stream.getReader();
  void limit.ended.then(() => reader.cancel().catch(() => {}));
  for (;;) {
    const next = await reader.read();
    if (next.done) return;
    take(next.value);
  }
}

/** The text of `stream` (`eachChunk`), as far as it came before `limit` ended its child. */
export async function textOf(
  stream: ReadableStream<Uint8Array>,
  limit: TimeLimit
): Promise<string> {
  const chunks: Uint8Array[] = [];
  await eachChunk(stream, limit, (chunk) => chunks.push(chunk));
  return Buffer.concat(chunks).toString("utf8");
}

/** How long a store command (the configuration's `psqlCommand`) may print nothing before it is
 *  ended: every SQL of the runner ends itself after 15 minutes (`statement_timeout`), the longest
 *  real query (count.sql of one day of the busiest store, 2026-11-22) took 860 s, and psql prints
 *  a query's rows only once it ends, so the silence before the first row is the query's time. 20
 *  minutes is that 15 and 5 for connecting (the store's sign-in) and the server's cancel; a long
 *  answer then streams, each chunk starting the time again, so its size never ends it. */
export const STORE_QUIET_MS = 1_200_000;

/** GitHub's refusal for its secondary rate limit, which has no reset to wait for (`rateLimitWait`). */
const SECONDARY_LIMITED = /secondary rate limit/i;
/** The first wait after a refusal for the secondary rate limit, doubled at each refusal after. */
export const SECONDARY_WAIT_MS = 60_000;

/** The milliseconds to wait before a call GitHub refused for its rate limit (its error `stderr`)
 *  is sent again, at its `refused`-th refusal (from 0): the one rule of copies.ts's reads,
 *  context.ts's and build.ts's. GitHub's secondary rate limit has no reset (the core limit may
 *  have most of its calls left): GitHub asks a client to wait its `retry-after` (which gh shows
 *  only with `-i`, so never for these calls), else at least a minute, and to back off; so the wait
 *  is SECONDARY_WAIT_MS, doubled at each refusal, and `untilReset` is not asked. A primary limit
 *  is waited out to its reset (`untilReset()`, the caller's `untilInstallationReset`). */
export async function rateLimitWait(
  stderr: string,
  refused: number,
  untilReset: () => Promise<number>
): Promise<number> {
  if (SECONDARY_LIMITED.test(stderr)) return SECONDARY_WAIT_MS * 2 ** refused;
  return untilReset();
}

/** The longest wait for a primary rate limit's reset, and the wait when the reset is not read. */
export const MAX_RATE_WAIT_MS = 15 * 60_000;

/** HTTP status, headers (by lowercase name) and body of a `gh api -i` answer, which gh prints
 *  for an answer GitHub refused too. */
export function splitIncluded(text: string): {
  status: number;
  headers: Map<string, string>;
  body: string;
} {
  const gap = /\r?\n\r?\n/.exec(text);
  if (!text.startsWith("HTTP/") || gap === null)
    return { status: 0, headers: new Map(), body: text };
  const head = text.slice(0, gap.index).split(/\r?\n/);
  const headers = new Map<string, string>();
  for (const line of head.slice(1)) {
    const colon = line.indexOf(":");
    if (colon > 0)
      headers.set(line.slice(0, colon).trim().toLowerCase(), line.slice(colon + 1).trim());
  }
  return {
    status: Number(/^HTTP\/\S+\s+(\d+)/.exec(head[0] ?? "")?.[1] ?? 0),
    headers,
    body: text.slice(gap.index + gap[0].length),
  };
}

/** The options of `gh api` that take a value, so that `installationOf` takes no option's value for
 *  the endpoint: `true` for a field (`<key>=<value>`), whose key and value it reads. */
const GH_API_VALUED: Readonly<Record<string, boolean>> = {
  "-X": false,
  "--method": false,
  "-H": false,
  "--header": false,
  "-q": false,
  "--jq": false,
  "-t": false,
  "--template": false,
  "--input": false,
  "-p": false,
  "--preview": false,
  "--hostname": false,
  "--cache": false,
  "-f": true,
  "--raw-field": true,
  "-F": true,
  "--field": true,
};

/** The GitHub App installation whose rate limit a gh call spends: its owner's, with the name of
 *  the repository when the call names one, and whether the call reads the GraphQL API. */
interface Installation {
  readonly owner: string;
  readonly name: string | undefined;
  readonly graphql: boolean;
}

/** The installation the pod's gh sends the gh call `args` (without `gh`) with, as it routes the
 *  call by the owner the call names: for `gh api`, the owner and the repository of a
 *  `repos/<owner>/<repo>/...` endpoint, or the owner alone of `repos/<owner>`, `orgs/<owner>/...`
 *  or `users/<owner>/...`; for `gh api graphql`, its `owner` field and its `name` field; for
 *  `gh repo list <owner>`, which reads the GraphQL API, that owner. Undefined for a call that
 *  names no owner, such as `search/code` or `rate_limit`. */
function installationOf(args: readonly string[]): Installation | undefined {
  if (args[0] === "repo" && args[1] === "list") {
    const owner = args[2] ?? "";
    return owner === "" || owner.startsWith("-")
      ? undefined
      : { owner, name: undefined, graphql: true };
  }
  if (args[0] !== "api") return undefined;
  let endpoint = "";
  const fields = new Map<string, string>();
  for (let at = 1; at < args.length; at++) {
    const arg = args[at] ?? "";
    if (Object.hasOwn(GH_API_VALUED, arg)) {
      at++;
      const value = args[at] ?? "";
      const eq = value.indexOf("=");
      if (GH_API_VALUED[arg] === true && eq > 0)
        fields.set(value.slice(0, eq), value.slice(eq + 1));
    } else if (endpoint === "" && !arg.startsWith("-")) endpoint = arg;
  }
  if (endpoint === "graphql") {
    const owner = fields.get("owner") ?? "";
    const name = fields.get("name") ?? "";
    return owner === "" ? undefined : { owner, name: name || undefined, graphql: true };
  }
  const [first, owner = "", name = ""] = (endpoint.split("?")[0] ?? "").split("/");
  if (owner === "") return undefined;
  if (first === "repos") return { owner, name: name || undefined, graphql: false };
  if (first === "orgs" || first === "users") return { owner, name: undefined, graphql: false };
  return undefined;
}

/** The one gh call that reads the reset of `at`'s rate limit, sent with `at`'s installation as
 *  the pod's gh routes it, with `-i` so that its answer's headers come too: for a GraphQL limit,
 *  a query of `rateLimit { resetAt }` with the owner (and the repository's name) as its `owner`
 *  (and `name`) fields; for the REST limit, a GET of the repository, or of the owner's first
 *  repository when the call named the owner alone. */
function resetRead(at: Installation): string[] {
  if (at.graphql) {
    const [fields, query] =
      at.name === undefined
        ? [
            ["-f", `owner=${at.owner}`],
            "query($owner: String!) { rateLimit { resetAt } repositoryOwner(login: $owner) { login } }",
          ]
        : [
            ["-f", `owner=${at.owner}`, "-f", `name=${at.name}`],
            "query($owner: String!, $name: String!) { rateLimit { resetAt } repository(owner: $owner, name: $name) { id } }",
          ];
    return ["api", "-i", "graphql", ...fields, "-f", `query=${query}`];
  }
  const endpoint =
    at.name === undefined ? `users/${at.owner}/repos?per_page=1` : `repos/${at.owner}/${at.name}`;
  return ["api", "-i", "--method", "GET", endpoint];
}

/** The `resetAt` of a GraphQL answer's `data.rateLimit` (the text `body`), in milliseconds since
 *  the epoch; undefined when it has none. */
function resetAtOf(body: string): number | undefined {
  try {
    const resetAt: unknown = JSON.parse(body)?.data?.rateLimit?.resetAt;
    const ms = typeof resetAt === "string" ? Date.parse(resetAt) : Number.NaN;
    return Number.isFinite(ms) ? ms : undefined;
  } catch {
    return undefined;
  }
}

/** The reset a `gh api -i` answer `stdout` gives, in milliseconds since the epoch: a GraphQL
 *  answer's `rateLimit.resetAt` when `graphql`, else the answer's X-RateLimit-Reset header (in
 *  seconds), which GitHub sends on an answer it refuses for the limit too; undefined when it gives
 *  neither. */
function resetOf(stdout: string, graphql: boolean): number | undefined {
  const answer = splitIncluded(stdout);
  const resetAt = graphql ? resetAtOf(answer.body) : undefined;
  if (resetAt !== undefined) return resetAt;
  const header = Number(answer.headers.get("x-ratelimit-reset"));
  return Number.isFinite(header) && header > 0 ? header * 1000 : undefined;
}

/** What `untilInstallationReset` reads the reset with, and says its wait on. */
export interface ResetIo {
  /** One gh call, its arguments without `gh`. */
  gh(args: readonly string[]): Promise<{ readonly code: number; readonly stdout: string }>;
  /** Writes one line to standard error, as the caller's command's (`GhRetries.say`). */
  say(line: string): void;
  /** Milliseconds since the epoch; `Date.now()` when absent. */
  now?(): number;
}

/** The milliseconds to wait before the gh call `args` (without `gh`), which GitHub refused for its
 *  primary rate limit, is sent again: until the reset of the limit of the installation that
 *  refused it, and a second more, at most MAX_RATE_WAIT_MS. That installation is the one of the
 *  owner the call names, by which the pod's gh routes it (`installationOf`); its reset is read
 *  with one call of its own (`resetRead`, `resetOf`). `gh api rate_limit` names no owner, so the
 *  pod's gh answers it from another installation, whose reset can come before the refusing one's.
 *  A call that names no owner, a read that gives no reset and a reset already past wait
 *  MAX_RATE_WAIT_MS. The wait is said on `io.say`: whose reset it waits for, and until when. */
export async function untilInstallationReset(
  args: readonly string[],
  io: ResetIo
): Promise<number> {
  const now = () => io.now?.() ?? Date.now();
  const longest = (why: string) => {
    io.say(
      `${why}; waiting ${MAX_RATE_WAIT_MS / 1000} s, the longest rate wait, until ${new Date(now() + MAX_RATE_WAIT_MS).toISOString()}`
    );
    return MAX_RATE_WAIT_MS;
  };
  const at = installationOf(args);
  if (at === undefined) {
    return longest(
      "the refused call names no owner in its endpoint or its owner field, so no installation's reset is read"
    );
  }
  const limit = `${at.owner}'s ${at.graphql ? "GraphQL" : "REST"} rate limit`;
  const read = await io.gh(resetRead(at));
  const reset = resetOf(read.stdout, at.graphql);
  if (reset === undefined) {
    const why = read.code === 0 ? "its answer gave none" : `gh exited ${read.code}, giving none`;
    return longest(`the reset of ${limit} could not be read (${why})`);
  }
  const clock = now();
  const wait = Math.min(reset + 1_000 - clock, MAX_RATE_WAIT_MS);
  const resets = `${limit} resets at ${new Date(reset).toISOString()}`;
  if (wait <= 0) return longest(`${resets}, a time already past`);
  io.say(
    `${resets}; waiting ${Math.round(wait / 1000)} s, until ${new Date(clock + wait).toISOString()}`
  );
  return wait;
}

/** The `rateWait` of `GhRetries.send` for the gh call `args` (without `gh`): `rateLimitWait`,
 *  whose primary limit is waited out to the reset of the installation that refused the call,
 *  read with `io` (`untilInstallationReset`). The one rule of copies.ts's, context.ts's and
 *  build.ts's reads. */
export function installationRateWait(
  args: readonly string[],
  io: ResetIo
): (stderr: string, refused: number) => Promise<number> {
  return (stderr, refused) =>
    rateLimitWait(stderr, refused, () => untilInstallationReset(args, io));
}
