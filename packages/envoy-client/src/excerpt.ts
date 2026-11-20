// packages/envoy-client/src/excerpt.ts
//
// One line of someone else's text, safe to print: a body or a reason phrase that a gateway, a proxy
// or anything else in a server's place wrote. A client that quotes such a body in an error
// quotes it through `bodyForText`: the Dispatch client (`dispatch-http.ts`) a gateway's page, the
// Envoy transport (`transport.ts`) a body that is not the listener's error JSON, and the operator's
// live-events probe (`scripts/e2e/lib/dispatch-live-events.ts`) a whoami answer with no event_head.

import { textHead } from "./ask-answer";

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
 * `EXCERPT_SCAN_LIMIT` characters are read and every pattern runs in linear time: a tag holds no
 * `<`, a script or style block left open runs to the end, and a JWS is matched only from the start
 * of a run of base64url characters.
 */
export function excerpt(text: string, tokens: Tokens): string {
  const trimmed = (typeof tokens === "string" ? [tokens] : tokens).map((token) => token.trim());
  const plain = redactTokens(text, trimmed)
    .replace(/<(script|style)\b[^<>]*>(?:[\s\S]*?<\/\1\s*>|[\s\S]*$)/gi, " ")
    .replace(/<[^<>]*>/g, " ");
  return textHead(
    plain
      .replace(
        /(\b(?:authorization|dpop)["']?\s*[:=]\s*(?:\[\s*)?["']?(?:(?:bearer|basic|digest|token|dpop)\s+)?)[^\s"'<>,;\]]+/gi,
        `$1${REDACTED}`
      )
      .replace(/(\b(?:bearer|dpop)\s+)[^\s"'<>,;]+/gi, `$1${REDACTED}`)
      .replace(/(?<![A-Za-z0-9_-])eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/g, REDACTED)
  );
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
