// packages/envoy-client/src/excerpt.ts
//
// One line of someone else's text, safe to print: a body or a reason phrase that a gateway, a proxy
// or anything else in a server's place wrote. A client that quotes such a body in an error
// quotes it through `bodyForText`: the Dispatch client (`dispatch-http.ts`) a gateway's page, the
// Envoy transport (`transport.ts`) a body that is not the listener's error JSON, and the operator's
// live-events probe (`scripts/e2e/lib/dispatch-live-events.ts`) a whoami answer with no event_head.

import { textHead, WHITESPACE_RUN } from "./ask-answer";

const REDACTED = "[redacted]";

/**
 * The most of a body an excerpt scans. A gateway page's inline styles and scripts can run to tens
 * of kilobytes before its first words, so this reaches well past them, and it bounds the work a
 * body of any size can cost.
 */
export const EXCERPT_SCAN_LIMIT = 64 * 1024;

/**
 * The shortest run of a token's characters an excerpt redacts wherever it stands. A gateway that
 * echoes the header can cut it short, split it with markup, or write it twice with the copies
 * overlapping (a token can end in its own first characters), so redacting only whole copies leaves
 * most of one behind; a token shorter than this is redacted only whole.
 */
const BEARER_PIECE = 8;

/** A token, or the tokens, an excerpt redacts: a client's credential's secret, and each token it
 *  presented lately (`SentTokens`). */
export type Tokens = string | readonly string[];

/**
 * The work excerpts have done in this process, counted where it is spent, so a test holds it to
 * the size of what an excerpt reads rather than to the time it took: each match the token
 * redaction's scans visit (`redactTokens`), and each character a pattern pass of `excerpt` is
 * handed. A step a regular expression takes inside one pass is not counted, because
 * JavaScriptCore counts none: recheck's reading of each pattern (`EXCERPT_PATTERNS`) is what holds
 * a pass linear.
 */
let bearerMatchesVisited = 0;
let patternCharactersHanded = 0;

/** How many matches the token redaction's scans have visited in this process. */
export function bearerRedactionMatches(): number {
  return bearerMatchesVisited;
}

/** How many characters the pattern passes of excerpts have been handed in this process. */
export function excerptPatternCharacters(): number {
  return patternCharactersHanded;
}

/**
 * The first `EXCERPT_SCAN_LIMIT` characters of `text`, each run of any of `tokens`' characters at
 * least `BEARER_PIECE` long (a token's whole length where it is shorter) replaced with one
 * `REDACTED`. Every piece of that length starting inside the limit is found (one that runs past it
 * reads up to `BEARER_PIECE - 1` characters beyond), so a run is redacted whole however the limit
 * or another copy cuts it. Redaction changes the length, so the text is cut to the limit only
 * afterwards, and nothing from past the limit moves into it. Each distinct piece is one native
 * scan of that slice, and no two distinct pieces of one length match at the same position, so the
 * marking is bounded by the slice and the scans by the limit and the tokens' lengths, whatever the
 * size of the body. Searching again for a repeated piece would mark the same matches again: a
 * token of one character repeated would mark every position of the slice once per piece.
 */
function redactTokens(text: string, tokens: readonly string[]): string {
  const lengths = tokens.map((token) => Math.min(BEARER_PIECE, token.length));
  const longest = Math.max(0, ...lengths);
  if (longest === 0) return text.slice(0, EXCERPT_SCAN_LIMIT);
  const scanned = text.slice(0, EXCERPT_SCAN_LIMIT + longest - 1);
  const covered = new Uint8Array(scanned.length);
  tokens.forEach((token, index) => {
    const length = lengths[index] ?? 0;
    if (length === 0) return;
    const pieces = new Set<string>();
    for (let at = 0; at + length <= token.length; at++) pieces.add(token.slice(at, at + length));
    for (const piece of pieces) {
      let found = scanned.indexOf(piece);
      while (found !== -1 && found < EXCERPT_SCAN_LIMIT) {
        bearerMatchesVisited++;
        covered.fill(1, found, found + length);
        found = scanned.indexOf(piece, found + 1);
      }
    }
  });
  let redacted = "";
  let kept = 0;
  for (let start = covered.indexOf(1); start !== -1; start = covered.indexOf(1, kept)) {
    redacted += `${scanned.slice(kept, start)}${REDACTED}`;
    const end = covered.indexOf(0, start);
    kept = end === -1 ? scanned.length : end;
  }
  return `${redacted}${scanned.slice(kept, EXCERPT_SCAN_LIMIT)}`.slice(0, EXCERPT_SCAN_LIMIT);
}

/**
 * The patterns of `excerpt`'s passes, in the order it runs them: scripts and styles, tags, the
 * value of an `Authorization` or a `DPoP` header or member, the value after `Bearer` or `DPoP`, a
 * JWS, and the whitespace runs `textHead` folds to one space (`WHITESPACE_RUN`). A tag holds no
 * `<`, and a script or style block left open runs to the end. The block pattern spells its two
 * names out, where a back-reference to one name would close a block with the name that opened it
 * the same way, and the JWS pattern starts a match only at the start of a run of base64url
 * characters by taking the character before it (or the text's start) into its first group, where
 * a look-behind would do the same: recheck's automaton checker reads neither a back-reference nor
 * a look-behind, and reads each of these linear (`excerpt-patterns.test.ts`).
 */
const SCRIPT_OR_STYLE_BLOCK =
  /<script\b[^<>]*>(?:[\s\S]*?<\/script\s*>|[\s\S]*)|<style\b[^<>]*>(?:[\s\S]*?<\/style\s*>|[\s\S]*)/gi;
const TAG = /<[^<>]*>/g;
const AUTHORIZATION_VALUE =
  /(\b(?:authorization|dpop)["']?\s*[:=]\s*(?:\[\s*)?["']?(?:(?:bearer|basic|digest|token|dpop)\s+)?)[^\s"'<>,;\]]+/gi;
const BEARER_VALUE = /(\b(?:bearer|dpop)\s+)[^\s"'<>,;]+/gi;
const JWS_VALUE = /(^|[^A-Za-z0-9_-])eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/g;

/** `excerpt`'s patterns by name, for the analyzer rows of `excerpt-patterns.test.ts`. */
export const EXCERPT_PATTERNS: Readonly<Record<string, RegExp>> = {
  "script-or-style-block": SCRIPT_OR_STYLE_BLOCK,
  tag: TAG,
  "authorization-value": AUTHORIZATION_VALUE,
  "bearer-value": BEARER_VALUE,
  "jws-value": JWS_VALUE,
  "whitespace-run": WHITESPACE_RUN,
};

/**
 * Text a gateway chose (a body, or the reason phrase) as one line of plain text safe to quote. A
 * misconfigured gateway echoes the request back, so two kinds of credential are redacted: first
 * every piece of each of the client's own `tokens`, trimmed as fetch sends them (`redactTokens`);
 * then, after scripts, styles and tags are dropped, the value of an `Authorization` or a `DPoP`
 * header or JSON member, in any case, a value written in a list as Go's header JSON writes one
 * (`"Dpop":["<proof>"]`) included (an `Authorization` value's scheme kept: the `DPoP` header is
 * the proof the credential signed for that one request, which is none of `tokens`), the value
 * after `Bearer` or `DPoP`, and every value shaped as a JWS (`eyJ…` `.` `…` `.` `…`, a proof or a
 * JWT) wherever it stands, so an echo the gateway quoted inside a JSON string (`\"`), as HTML
 * (`&quot;`) or as the second value of a list is redacted too. All of it runs before the cut to
 * one line, so a cut never leaves a credential's first characters behind. The text is the
 * answerer's, and the excerpt is built on the host's event loop, so only the first
 * `EXCERPT_SCAN_LIMIT` characters are read and every pattern runs in linear time
 * (`EXCERPT_PATTERNS`).
 */
export function excerpt(text: string, tokens: Tokens): string {
  const trimmed = (typeof tokens === "string" ? [tokens] : tokens).map((token) => token.trim());
  const withoutBlocks = patternPass(redactTokens(text, trimmed), SCRIPT_OR_STYLE_BLOCK, " ");
  const withoutTags = patternPass(withoutBlocks, TAG, " ");
  const withoutAuthorization = patternPass(withoutTags, AUTHORIZATION_VALUE, `$1${REDACTED}`);
  const withoutBearer = patternPass(withoutAuthorization, BEARER_VALUE, `$1${REDACTED}`);
  return headPass(patternPass(withoutBearer, JWS_VALUE, `$1${REDACTED}`));
}

/** `text` with `pattern` replaced, counting the characters the pass is handed. */
function patternPass(text: string, pattern: RegExp, replacement: string): string {
  patternCharactersHanded += text.length;
  return text.replace(pattern, replacement);
}

/** `textHead` of `text`, counting the characters its pass (`WHITESPACE_RUN`) is handed. */
function headPass(text: string): string {
  patternCharactersHanded += text.length;
  return textHead(text);
}

/**
 * `text`, an answer's body that is not what its server writes (`notWhat`, such as "Dispatch's
 * error JSON"), as an error names it: `a body that is not <notWhat> ("<excerpt>")`, one line with
 * `tokens` and any `Authorization`, `DPoP` or `Bearer` value redacted (`excerpt`); `an empty
 * body`; or, where the excerpt reads no text, the body's size and that it holds none (within the
 * first `EXCERPT_SCAN_LIMIT` characters where it runs past them). Never the body whole.
 */
export function bodyForText(text: string, tokens: Tokens, notWhat: string): string {
  const quoted = excerpt(text, tokens);
  if (quoted !== "") return `a body that is not ${notWhat} (${JSON.stringify(quoted)})`;
  if (text === "") return "an empty body";
  const within =
    text.length > EXCERPT_SCAN_LIMIT ? ` in its first ${EXCERPT_SCAN_LIMIT / 1024} KiB` : "";
  const size = Buffer.byteLength(text).toLocaleString("en-US");
  return `a body of ${size} bytes and no readable text${within}`;
}

/**
 * The origin a 3xx answer's `Location` names, read against `url`, the request it answers: scheme,
 * host and port, never a path, a query or user info, through `excerpt` so nothing of `tokens` is
 * printed. Undefined for any other status, and for a `Location` that is absent or does not parse.
 */
export function redirectOrigin(
  response: Response,
  url: string,
  tokens: Tokens
): string | undefined {
  if (response.status < 300 || response.status > 399) return undefined;
  const location = response.headers.get("Location");
  if (location === null || location.trim() === "") return undefined;
  let origin: string;
  try {
    origin = new URL(location, url).origin;
  } catch {
    return undefined;
  }
  if (origin === "null") return undefined;
  const shown = excerpt(origin, tokens);
  return shown === "" ? undefined : shown;
}

/** How many of the tokens a client presented `SentTokens` keeps. A token renews long after it is
 *  minted, so a request's answer meets at most one renewal; four leaves room for several. */
const SENT_TOKENS_KEPT = 4;

/**
 * The tokens a client presented lately, newest first, so an excerpt of an answer redacts the token
 * its request was sent with even where the credential has renewed since and its `secret()` names
 * the next one (the review of 99b1e438db, M4). Each is the `Authorization` header's value after
 * its scheme. Only this client's own tokens are kept, and only the last `SENT_TOKENS_KEPT`, so
 * redacting all of them redacts nothing of anyone else's.
 */
export class SentTokens {
  readonly #tokens: string[] = [];

  /** Keeps the token `presented`'s `Authorization` header carries, if any. */
  note(presented: Readonly<Record<string, string>> | undefined): void {
    if (presented === undefined) return;
    const header = Object.entries(presented).find(
      ([name]) => name.toLowerCase() === "authorization"
    )?.[1];
    const token =
      header
        ?.trim()
        .replace(/^\S+\s+/, "")
        .trim() ?? "";
    if (token === "" || this.#tokens[0] === token) return;
    const known = this.#tokens.indexOf(token);
    if (known !== -1) this.#tokens.splice(known, 1);
    this.#tokens.unshift(token);
    this.#tokens.length = Math.min(this.#tokens.length, SENT_TOKENS_KEPT);
  }

  /** `current` (the credential's secret now) and the tokens kept, each once, none empty. */
  with(current: string): readonly string[] {
    const held = current.trim();
    if (held === "") return [...this.#tokens];
    return [held, ...this.#tokens.filter((token) => token !== held)];
  }
}
