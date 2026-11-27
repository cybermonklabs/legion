// How a client of this workspace prints a field another party wrote: a server's refusal, an
// issuer's error description, a code, a body. Such a field is the reason a person acts on, so it
// prints as written, but a URL it quotes can carry a credential, which must not reach a log or an
// error a session prints. The rule fails closed: a field it cannot read with certainty prints as
// WORDS_NOT_READABLE. Go's half is `redact.Remote` in packages/envoy/internal/redact, and both
// halves are held to the fixture at packages/envoy/internal/redact/testdata/remote.json.

import { errorClass } from "./log-text";
import { isPublicRequestName, type RequestRoute, requestRoute } from "./public-request-names";

/** What a message prints in place of a field the rule cannot read as clean: Go's
 *  `redact.WordsNotReadable`. */
export const WORDS_NOT_READABLE =
  "(not printed: the answer's words hold a URL the rule cannot read as clean)";

/** The characters an encoding of a URL hides from step 1, which steps 0 and 7 name. Go's
 *  `separators`. */
const SEPARATORS = "/:@?#=&%\\";

/** The escapes the rule's step 7 rejects, lower-cased: of each of {@link SEPARATORS}, as `%XX`,
 *  `\u00XX`, `\xXX`, `%u00XX` and `\U000000XX`, and as a three-digit octal escape. */
const ENCODED_SEPARATORS = [...SEPARATORS].flatMap((separator) => {
  const code = separator.charCodeAt(0);
  const hex = code.toString(16);
  return [
    `%${hex}`,
    `\\u00${hex}`,
    `\\x${hex}`,
    `%u00${hex}`,
    `\\u000000${hex}`,
    `\\${code.toString(8).padStart(3, "0")}`,
  ];
});

/** Whether `lower` holds a `\u{...}` escape whose hexadecimal digits, any number of leading zeros
 *  among them, name one of {@link SEPARATORS}. Go's `braceEscape`. */
function braceEscape(lower: string): boolean {
  for (const [, digits] of lower.matchAll(/\\u\{([0-9a-f]+)\}/g)) {
    const code = Number.parseInt(digits as string, 16);
    if (code < 0x80 && SEPARATORS.includes(String.fromCharCode(code))) return true;
  }
  return false;
}

/** An HTML character reference as a page's reader takes it: hexadecimal, decimal or named, its
 *  `;` optional. A name is at most 32 letters: HTML's longest named reference has 31 letters and
 *  its `;`. A longer run of letters is no one reference: the match takes its first 32 letters as
 *  the name and the rest is copied as it came, so the run reads as it would whole: one that
 *  begins with amp loses those three letters ({@link separatorReference}), and any other stays as
 *  it is. The cap keeps each match short, as Go's does. Go's `characterReference`. */
const CHARACTER_REFERENCE = /&(?:#[xX]([0-9A-Fa-f]+)|#([0-9]+)|([A-Za-z]{1,32}));?/g;

/** The named character references of {@link SEPARATORS}. Go's `namedSeparators`. */
const NAMED_SEPARATORS: Readonly<Record<string, string>> = {
  sol: "/",
  colon: ":",
  commat: "@",
  quest: "?",
  num: "#",
  equals: "=",
  amp: "&",
  AMP: "&",
  percnt: "%",
  bsol: "\\",
};

/** The first and the last full-width form of printable ASCII, U+FF01 and U+FF5E: the forms of `!`
 *  to `~`. Go's `isFullWidth`. */
const FULL_WIDTH_FIRST = 0xff01;
const FULL_WIDTH_LAST = 0xff5e;

/** Each full-width form of printable ASCII in a field, {@link FULL_WIDTH_FIRST} to
 *  {@link FULL_WIDTH_LAST}, which {@link decodeSeparators} reads as that character. */
const FULL_WIDTH = new RegExp(
  `[${String.fromCharCode(FULL_WIDTH_FIRST)}-${String.fromCharCode(FULL_WIDTH_LAST)}]`,
  "g"
);

/** The printable ASCII character whose full-width form is `code`, which sits 0xFEE0 above it, or
 *  `undefined` where `code` is no such form. Go's `fromFullWidth`. */
function fromFullWidth(code: number): string | undefined {
  return code >= FULL_WIDTH_FIRST && code <= FULL_WIDTH_LAST
    ? String.fromCharCode(code - 0xfee0)
    : undefined;
}

/** One {@link CHARACTER_REFERENCE} as {@link decodeSeparators} reads it: the separator it names,
 *  `&` and the rest of a name that begins with amp, or the reference as it is. Go's
 *  `separatorReference`. */
function separatorReference(
  reference: string,
  hex: string | undefined,
  decimal: string | undefined,
  name: string | undefined
): string {
  const semicolon = reference.endsWith(";");
  if (name === undefined) {
    const digits = hex ?? decimal ?? "";
    const code = Number.parseInt(digits, hex === undefined ? 10 : 16);
    if (code < 0x80 && SEPARATORS.includes(String.fromCharCode(code))) {
      return String.fromCharCode(code);
    }
    // A reference of a full-width separator (`&#xFF0F;`, `&#65295;`) is that separator to a
    // reader of the page too.
    const ascii = fromFullWidth(code);
    return ascii !== undefined && SEPARATORS.includes(ascii) ? ascii : reference;
  }
  const separator = Object.hasOwn(NAMED_SEPARATORS, name) ? NAMED_SEPARATORS[name] : undefined;
  if (separator !== undefined && (semicolon || separator === "&")) return separator;
  if (name.startsWith("amp") || name.startsWith("AMP")) {
    return `&${name.slice(3)}${semicolon ? ";" : ""}`;
  }
  return reference;
}

/** The most passes that change a field {@link decodeSeparators} makes, each pass decoding one
 *  level of nesting. A page writes a URL's separator as a reference (`&quest;`, one pass), and a
 *  page whose text was escaped once more writes the reference's `&` as a reference too
 *  (`&amp;quest;`, two passes): the deepest any page, answer or test the rule is held to writes.
 *  Four leaves room for two more escapes. Each pass is O(n) in the field, so the decoding costs
 *  at most `REFERENCE_PASSES + 1` passes, O(n), where decoding until no reference was left cost a
 *  pass for each level a hostile field nests (`&amp;amp;amp;…` or `&ampampamp…;`), quadratic in
 *  its length. Go's `redact.ReferencePasses`. */
const REFERENCE_PASSES = 4;

/**
 * `field` as a reader sees it (step 0): each full-width form of printable ASCII as that character
 * ({@link fromFullWidth}), and each character reference of one of {@link SEPARATORS} as that
 * separator, decoded in at most {@link REFERENCE_PASSES} passes that change it; `undefined` where
 * the field still changes on the pass after them. A named reference needs its `;`, as a page's
 * reader takes it, but for `&amp`, which a reader also takes as the start of a longer name
 * (`&ampquest;` is `&quest;`). Go's `decodeSeparators`.
 */
function decodeSeparators(field: string): string | undefined {
  for (let pass = 0; ; pass++) {
    const decoded = field
      .replace(FULL_WIDTH, (c) => fromFullWidth(c.charCodeAt(0)) ?? c)
      .replace(CHARACTER_REFERENCE, separatorReference);
    if (decoded === field) return field;
    if (pass === REFERENCE_PASSES) return undefined;
    field = decoded;
  }
}

/** The longest field, in bytes of UTF-8, the rule reads: a longer field is not readable. Go's
 *  `redact.FieldLimit`, whose comment says why it is 1 MiB. */
const FIELD_LIMIT = 1024 * 1024;

/** UTF-8, which the rule measures a field in ({@link FIELD_LIMIT}), as Go's `len` does. */
const UTF8 = new TextEncoder();

/**
 * The rule for one field another party wrote. It answers the field with each URL's user
 * information removed, each query and fragment value removed (the names stay), each bare query
 * or fragment segment removed and each path parameter's value removed (`;jsessionid=`), only when
 * it can read every URL in the field with certainty, and `undefined` otherwise. A field longer
 * than {@link FIELD_LIMIT} bytes is not readable, before any step reads it. The grammar is Go's
 * `redact.Remote`, step for step:
 *  0. The field is first read with each full-width form of printable ASCII as that character and
 *     each HTML character reference of a URL's separator, or of its full-width form, as that
 *     separator, in at most {@link REFERENCE_PASSES} passes that change it
 *     ({@link decodeSeparators}); a field that still changes on the pass after them, its
 *     references nested deeper than a page writes them, is not readable. Where that reading
 *     differs from the field, steps 1 to 8 read it: a field they cannot read is not readable, and
 *     one whose reading removes anything prints as that reading. Otherwise steps 1 to 8 read the
 *     field as it came.
 *  1. A URL starts at a scheme (`[A-Za-z][A-Za-z0-9+.-]*`), `:` and two slashes, or at two
 *     slashes not preceded by a scheme character, `:` or a slash. Either slash may be a
 *     backslash; step 3 rejects such a start. A `:` and two slashes that no scheme comes before
 *     (`x;://host`, `9://host`, `://host`) also start a URL, at the first character of their
 *     token, and step 3 reads that run as not readable.
 *  2. A run is the text from a URL start to the next ASCII whitespace or the end of the field.
 *     When it does not end inside a query or a fragment, the characters `.,;:!?)]}'"` at its end
 *     are not part of it; when it does, they are part of the last value.
 *  3. Not readable when a run does not begin with two slashes or with a scheme, `:` and two
 *     slashes; when it holds `%`, `\`, a quote, `<`, `>`, a backtick, a control character or any
 *     non-ASCII character; when its authority holds more than one `@`; when it holds an `@` after
 *     its authority; or when a port is not all digits.
 *  4. Not readable when a query or fragment segment (split on `&` and `;`), or a path parameter
 *     (a segment's `;name=value`), has a value that holds `=` (and, in a query or a fragment,
 *     `?`).
 *  5. Not readable when a token, trimmed as in 2, holds an `@` with a `:` before it; in a token
 *     that holds a URL start this reads the text before the start.
 *  6. Not readable when a token after the token that holds the first URL start holds an `@`;
 *     when the token right after a run holds `?`, `#` or `=`; when a run ends inside a query or a
 *     fragment and more text follows it; or when a run is a scheme, two slashes, one word of
 *     unreserved characters and a `:` that ends its token, and more text follows
 *     (`https://user: password/x`, {@link splitUserInfo}).
 *  7. Not readable when the field holds an encoded `/`, `:`, `@`, `?`, `#`, `=`, `&`, `%` or `\`
 *     (`%2F`, `\u002F`, `\x2F`, `%u002F`, `\U0000002F`, `\u{2f}`, the octal `\057`, and their
 *     kin, in any case).
 *  8. The field is read again with its ASCII whitespace removed ({@link markersAccounted}), and
 *     fails closed: from the earliest URL start step 1 finds in that text or in a token of the
 *     field, every `?`, `#`, `@` and key-led `=` must lie inside a run the first reading read as
 *     a clean URL, whatever character sits between; before that start, a key's split
 *     `user:secret@` ({@link userInfoPiece}) is not readable.
 *
 * The known residual prints as it came: a label, a `:` and an address with no key before them
 * (`sign-in: ada@example.com`), which is what a split `user:secret@host` looks like without its
 * scheme while refusals print identities so; and the same in a JSON member
 * (`{"proxy":"user: pw@host"}`), whose key is followed by `":`, not `=`. No syntax tells either
 * apart from an identity. Go's `redact.Remote` names the same.
 *
 * Its answer is not meant to be read by the rule again.
 */
export function readRemoteText(field: string): string | undefined {
  // A field of more UTF-16 code units than FIELD_LIMIT is longer than it in UTF-8 too, each code
  // unit being at least one byte of UTF-8, so only a field of at most that many is encoded.
  if (field.length > FIELD_LIMIT || UTF8.encode(field).length > FIELD_LIMIT) return undefined;
  const decoded = decodeSeparators(field);
  if (decoded === undefined) return undefined;
  if (decoded !== field) {
    const read = readField(decoded);
    if (read !== decoded) return read;
  }
  return readField(field);
}

/** Steps 1 to 8 of {@link readRemoteText}, read on `field` as it is. Go's `readField`. */
function readField(field: string): string | undefined {
  const lower = field.toLowerCase();
  if (ENCODED_SEPARATORS.some((encoded) => lower.includes(encoded)) || braceEscape(lower)) {
    return undefined;
  }
  // What the reading of field's tokens did with each code unit (unset outside every run), which
  // step 8 compares with; a field with no whitespace has no second reading.
  const read: number[] | undefined = /[ \t\n\v\f\r]/.test(field) ? [] : undefined;
  let out = "";
  let seenUrl = false;
  let afterRun = false;
  for (let i = 0; i < field.length; ) {
    if (isAsciiSpace(field.charCodeAt(i))) {
      out += field[i];
      i++;
      continue;
    }
    let end = i;
    while (end < field.length && !isAsciiSpace(field.charCodeAt(end))) end++;
    const token = field.slice(i, end);
    if ((seenUrl && token.includes("@")) || (afterRun && /[?#=]/.test(token))) return undefined;
    const start = urlStart(token);
    const outside = start >= 0 ? token.slice(0, start) : token;
    const bare = trimRunEnd(outside);
    const colon = bare.indexOf(":");
    if (colon >= 0 && bare.lastIndexOf("@") > colon) return undefined;
    out += outside;
    afterRun = false;
    if (start >= 0) {
      const runAt = i + start;
      const run = readRun(token.slice(start), (from, to) => {
        for (let k = runAt + from; read !== undefined && k < runAt + to; k++)
          read[k] = READ_REMOVED;
      });
      if (read !== undefined && run !== undefined) {
        for (let k = runAt; k < runAt + run.length; k++)
          read[k] = Math.max(read[k] ?? 0, READ_KEPT);
      }
      if (run === undefined) return undefined;
      const more = /[^ \t\n\v\f\r]/.test(field.slice(end));
      if ((run.inQuery && more) || (more && splitUserInfo(token.slice(start)))) return undefined;
      out += run.shown + token.slice(start + run.length);
      seenUrl = true;
      afterRun = true;
    }
    i = end;
  }
  if (read !== undefined && !markersAccounted(field, read)) return undefined;
  return out;
}

/** What the reading of a field's tokens did with one code unit: read it outside every run (an
 *  unset entry), keep it in a run, or remove it from one. Go's `byteRead`. */
const READ_KEPT = 1;
const READ_REMOVED = 2;

/**
 * Step 8: whether `field`, read with its ASCII whitespace removed, holds no `?`, `#`, `@` or
 * key-led `=` after the earliest URL start (found in that text or in a token of the field) that
 * `read`, the reading of its tokens, did not read inside a clean run, and before that start no
 * key's split user information ({@link userInfoPiece}). Go's `markersAccounted`.
 */
function markersAccounted(field: string, read: number[]): boolean {
  let text = "";
  const at: number[] = [];
  let start = -1;
  for (let i = 0; i < field.length; ) {
    if (isAsciiSpace(field.charCodeAt(i))) {
      i++;
      continue;
    }
    let end = i;
    while (end < field.length && !isAsciiSpace(field.charCodeAt(end))) end++;
    const u = urlStart(field.slice(i, end));
    if (u >= 0 && start < 0) start = text.length + u;
    for (let k = i; k < end; k++) at.push(k);
    text += field.slice(i, end);
    i = end;
  }
  const u = urlStart(text);
  if (u >= 0 && (start < 0 || u < start)) start = u;
  if (userInfoPiece(start >= 0 ? text.slice(0, start) : text)) return false;
  for (let k = Math.max(start, 0); start >= 0 && k < text.length; k++) {
    if ((read[at[k] as number] ?? 0) === 0 && marker(text, k)) return false;
  }
  return true;
}

/** Whether `text[k]` is one of the characters step 8 requires a clean run to account for: `?`,
 *  `#`, `@`, or a key-led `=` ({@link keyLedEquals}). Go's `marker`. */
function marker(text: string, k: number): boolean {
  const c = text[k];
  return c === "?" || c === "#" || c === "@" || keyLedEquals(text, k);
}

/** Whether `text[k]` is an `=` right after a letter, a digit, `_`, `-` or `.`: a key's. Go's
 *  `keyLedEquals`. */
function keyLedEquals(text: string, k: number): boolean {
  return k > 0 && text[k] === "=" && /^[A-Za-z0-9_.-]$/.test(text[k - 1] as string);
}

/** Whether `run`, a token's text from a URL start to the token's end, is a scheme, two slashes,
 *  one word of unreserved characters and a `:` that ends the token: a user information split after
 *  its `:`. Go's `splitUserInfo`. */
function splitUserInfo(run: string): boolean {
  const head = isSlash(run[0]) ? 2 : run.indexOf(":") + 3;
  return /^[A-Za-z0-9\-._~]+:$/.test(run.slice(head));
}

/** Whether a run that holds the UTF-16 code unit `code` is not readable (step 3): `%`, `\`, a
 *  quote, `<`, `>`, a backtick, a control character, or a non-ASCII character. Go's
 *  `runRefuses`. */
function runRefuses(code: number): boolean {
  return code < 0x20 || code >= 0x7f || "%\\\"'<>`".includes(String.fromCharCode(code));
}

/** The characters RFC 3986 lets a user information hold: unreserved, a sub-delimiter, `:` and the
 *  `%` of an escape. */
const USER_INFO = /^[A-Za-z0-9\-._~!$&'()*+,;=:%]$/;

/**
 * Whether `text` holds a key's split user information as step 8 reads one: an `@` before a
 * character a host can start with (a user-information character other than `:`, or `[`), a `:`
 * before it with only user-information characters other than `:` between, and a key-led `=`
 * anywhere before that `:` ({@link keyLedEquals}), whatever lies between the key and the user.
 * Go's `userInfoPiece`.
 */
function userInfoPiece(text: string): boolean {
  let key = -1;
  for (let k = 1; k < text.length && key < 0; k++) if (keyLedEquals(text, k)) key = k;
  for (let a = 0; key >= 0 && a + 1 < text.length; a++) {
    const next = text[a + 1] as string;
    if (text[a] !== "@" || !((next !== ":" && USER_INFO.test(next)) || next === "[")) continue;
    let colon = a;
    while (colon > 0 && text[colon - 1] !== ":" && USER_INFO.test(text[colon - 1] as string)) {
      colon--;
    }
    if (colon > 0 && text[colon - 1] === ":" && key < colon - 1) return true;
  }
  return false;
}

/** A field another party wrote as a message prints it: what {@link readRemoteText} answers, on
 *  one line ({@link oneLine}), or {@link WORDS_NOT_READABLE} where it cannot read the field. Each
 *  of `carried`, the credentials the request it answers carried, is redacted from it first
 *  ({@link tokens}). Go's `redact.Field`, and `fieldWithout` with credentials. */
export function redactRemoteText(field: string, carried: readonly string[] = []): string {
  const read = readRemoteText(carried.length === 0 ? field : tokens(field, -1, ...carried));
  return read === undefined ? WORDS_NOT_READABLE : oneLine(read);
}

/** The credentials each box was given (`carried`), which its printers redact: kept beside the box,
 *  so the box's one storage is the field. */
const carriedOf = new WeakMap<RemoteText, readonly string[]>();

/** The boxes {@link withheldRemoteText} made: each compares as it came and prints nothing. */
const withheldBoxes = new WeakSet<RemoteText>();

/** A box for a field whose words must not print at all, a library error's that no request record
 *  covers: {@link RemoteText.equals} and its kin still read it as it came, so a branch decided by a
 *  refusal's code is decided as before, and every way it prints prints the empty string. */
function withheldRemoteText(text: string): RemoteText {
  const box = new RemoteText(text);
  withheldBoxes.add(box);
  return box;
}

/**
 * One field another party wrote, held so that it redacts itself: every way JavaScript prints it
 * prints {@link redactRemoteText} of it, never the field, with each of the credentials it was given
 * (`carried`, those the request it answered carried) redacted first. The text sits in a private
 * field, which `JSON.stringify`, an object spread and an inspection do not reach, and its own
 * methods answer the rest: `toString`, `toJSON`, `Symbol.toPrimitive` (a template string,
 * `String()`, `+`) and Node's and Bun's inspection hook. Only this class reads the text, and each of
 * its methods reads it in one of three ways alone: as the argument of the rule, as an operand of a
 * comparison or a parse whose answer is a boolean or a parsed value, or in the one assignment the
 * constructor makes. Code that tells fields apart compares through {@link equals},
 * {@link equalsIgnoringCase}, {@link startsWith}, {@link endsWith} and {@link isEmpty}, which read
 * the field as it came, and reads a time through {@link date}; none answers the text. Go's
 * `redact.RemoteText`.
 */
export class RemoteText {
  readonly #text: string;

  constructor(text: string, carried: readonly string[] = []) {
    this.#text = text;
    if (carried.length > 0) carriedOf.set(this, carried);
  }

  /** The field as a message may print it: {@link redactRemoteText} of it, with each credential it
   *  was given redacted. */
  toString(): string {
    if (withheldBoxes.has(this)) return "";
    return redactRemoteText(this.#text, carriedOf.get(this));
  }

  /** {@link toString}, so `JSON.stringify` writes the field as a message may print it. */
  toJSON(): string {
    return this.toString();
  }

  [Symbol.toPrimitive](): string {
    return this.toString();
  }

  [Symbol.for("nodejs.util.inspect.custom")](): string {
    return this.toString();
  }

  /** Whether the field is empty. It answers no text. */
  get isEmpty(): boolean {
    return this.#text === "";
  }

  /** Whether the field is `text`, compared as it came. It answers no text. */
  equals(text: string): boolean {
    return this.#text === text;
  }

  /** Whether the field begins with `prefix`, compared as it came. It answers no text. */
  startsWith(prefix: string): boolean {
    return this.#text.startsWith(prefix);
  }

  /** Whether the field ends with `suffix`, compared as it came. It answers no text. */
  endsWith(suffix: string): boolean {
    return this.#text.endsWith(suffix);
  }

  /** Whether the field is `text` under Unicode case folding, compared as it came, as Go's
   *  `strings.EqualFold` compares. It answers no text. */
  equalsIgnoringCase(text: string): boolean {
    const literal = text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    return new RegExp(`^${literal}$`, "iu").test(this.#text);
  }

  /** The field parsed as a time (`new Date`), or `undefined` where it names none. A field that
   *  does not parse answers no text. */
  date(): Date | undefined {
    const at = new Date(this.#text);
    return Number.isNaN(at.getTime()) ? undefined : at;
  }

  /** The rule's reading of the field ({@link redactRemoteText}) where `isKnown` accepts it, after
   *  `scrub`, with each credential the box was given redacted ({@link tokens}); `undefined` where
   *  `isKnown` refuses it. For a caller that prints a field only where it is one of a closed set of
   *  shapes, a known refusal, with the credentials it holds scrubbed its own way first. It answers
   *  no more of the field than {@link toString} prints. */
  knownReading(
    isKnown: (reading: string) => boolean,
    scrub: (reading: string) => string
  ): string | undefined {
    if (withheldBoxes.has(this)) return undefined;
    const reading = redactRemoteText(this.#text);
    if (!isKnown(reading)) return undefined;
    const carried = carriedOf.get(this) ?? [];
    return carried.length === 0 ? scrub(reading) : tokens(scrub(reading), -1, ...carried);
  }
}

/**
 * An error a library threw — openid-client and the oauth4webapi it is built on — as
 * {@link callLibrary} holds it once it has crossed that boundary. Such an error can carry an
 * issuer's or the broker's words in `error`, `error_description` and `error_uri`, the answer's
 * body (a `ResponseBodyError`'s `cause`; an `UnsupportedOperationError`'s `cause.body`, which
 * holds a token the answer minted), the answer itself (`response`), and an error of its own
 * under `cause`. This one keeps none of them: the status, the library's name for the error, its
 * class and its cause's ({@link errorClass}, which names no word of a message), and each of its
 * words as a {@link RemoteText}, the code and the description, so every way JavaScript prints it
 * prints only the rule's reading of them. Go's `redact.OAuthAnswer`.
 */
export class LibraryError extends Error {
  /** The HTTP status of the answer behind the error, 0 where it names none. */
  readonly status: number;
  /** The answer's `error`, empty where it names none. */
  readonly code: RemoteText;
  /** The answer's `error_description`, empty where it names none. */
  readonly description: RemoteText;
  /** What a text says of the error the library threw: its class ({@link errorClass}). */
  readonly thrownClass: string;
  /** The class of the error the library's own error was thrown over, "" where none. */
  readonly causeClass: string;
  /** Whether the library threw oauth4webapi's `ResponseBodyError`: the issuer or the broker
   *  answered a 4xx whose body is an OAuth error, so {@link status}, {@link code} and
   *  {@link description} are that answer's. It reads the code that library gives the class
   *  (`RESPONSE_BODY_ERROR`, "OAUTH_RESPONSE_BODY_ERROR"), never {@link name}: the library names an
   *  error by its `constructor.name`, which a subclass or a bundle that renames the class changes,
   *  and any error can be named so. Every client reads a refusal by this, in one place. */
  readonly answeredOAuthError: boolean;
  /** The answer's `Retry-After` (RFC 9110 Section 10.2.3), rebuilt from what it parses as: a count
   *  of seconds as its digits with no leading zero, or a date (any form `Date.parse` reads) as the
   *  ISO 8601 time it names; "" where it is neither or there was no answer. No other header of the
   *  answer is kept, and no character of this one beyond the number or the time it names. A
   *  device login's poll waits it after an outage (cipher-client's `device-login.ts`). Go's
   *  `redact.OAuth2` keeps the header so (`retryAfter`). */
  readonly retryAfter: string;

  constructor(thrown: unknown) {
    const error = thrown instanceof Error ? thrown : undefined;
    const message = error === undefined ? String(thrown) : error.message;
    const code = stringMember(thrown, "error");
    const description = stringMember(thrown, "error_description");
    // A refusal's record travels with the Response openid-client keeps on its error. It is never
    // selected by words an unrelated refusal happens to share with this one.
    const refusal = responseOfThrown(thrown);
    const carried = refusal === undefined ? undefined : recordedOf.get(refusal)?.credentials;
    const covered = carried !== undefined;
    const status = statusOfThrown(thrown);
    super(covered ? redactRemoteText(message, carried) : withheldClass(thrown, status));
    this.name = error !== undefined && /^\w+$/.test(error.name) ? error.name : "Error";
    this.status = status;
    // With no record, the words print nothing (a withheld box) and the code still compares as it
    // came, so a branch on the refusal's code takes the path it took before, as Go's RefusalCode.
    this.code = covered ? new RemoteText(code, carried) : withheldRemoteText(code);
    this.description = covered
      ? new RemoteText(description, carried)
      : withheldRemoteText(description);
    this.thrownClass = errorClass(thrown);
    const cause = error?.cause;
    this.causeClass = cause instanceof Error ? errorClass(cause) : "";
    this.answeredOAuthError = stringMember(thrown, "code") === "OAUTH_RESPONSE_BODY_ERROR";
    this.retryAfter = retryAfterOf(refusal);
  }
}

/**
 * The one way this workspace calls openid-client: every call of its, any of which can throw, is
 * written `callLibrary(() => call(...))`, and the census in
 * packages/envoy/internal/redact/testdata/wholeuse/boundary.ts, which reads the library's own
 * exported API, fails on any that is not. What the call throws, or what the promise it answers
 * rejects with, is a {@link LibraryError} before the caller sees it; what it answers comes back
 * as it is. Go's `redact.OAuth2`.
 */
export function callLibrary<T>(call: () => Promise<T>): Promise<T>;
export function callLibrary<T>(call: () => T): T;
export function callLibrary(call: () => unknown): unknown {
  let result: unknown;
  try {
    result = call();
  } catch (error) {
    throw error instanceof LibraryError ? error : new LibraryError(error);
  }
  if (result instanceof Promise) {
    return result.catch((error: unknown) => {
      throw error instanceof LibraryError ? error : new LibraryError(error);
    });
  }
  return result;
}

/** The status of the first value down `thrown`'s `cause` chain, itself first, that names a
 *  numeric one — a `ResponseBodyError`, or the `Response` an error hangs off its cause — or 0. */
function statusOfThrown(thrown: unknown): number {
  let value = thrown;
  for (let depth = 0; depth < 4; depth += 1) {
    if (typeof value !== "object" || value === null) return 0;
    if ("status" in value && typeof value.status === "number") return value.status;
    if (!("cause" in value)) return 0;
    value = value.cause;
  }
  return 0;
}

/** `answer`'s `Retry-After` as {@link LibraryError.retryAfter} keeps it: digits as the number of
 *  seconds they name, a date `Date.parse` reads as the ISO 8601 time it names, and "" for any
 *  other value or no answer. */
function retryAfterOf(answer: Response | undefined): string {
  const value = answer?.headers.get("retry-after")?.trim() ?? "";
  if (/^\d+$/.test(value)) return BigInt(value).toString();
  const at = Date.parse(value);
  return Number.isFinite(at) ? new Date(at).toISOString() : "";
}

/** `value[name]` where it is a string, and "" otherwise. */
function stringMember(value: unknown, name: string): string {
  if (typeof value !== "object" || value === null || !(name in value)) return "";
  const member: unknown = Reflect.get(value, name);
  return typeof member === "string" ? member : "";
}

/** `text` as one line of a message may print it: as it is where it holds no control character
 *  (C0, DEL or C1) and no lone surrogate, and otherwise quoted as Go's `strconv.Quote` quotes it
 *  (goQuote), every character that is not printable escaped, so a line break, an escape sequence
 *  or a line or paragraph separator cannot start a line of its own. Go's `redact.OneLine`; both
 *  read packages/envoy/internal/redact/testdata/oneline.json. */
export function oneLine(text: string): string {
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i);
    const next = i + 1 < text.length ? text.charCodeAt(i + 1) : 0;
    const control = code <= 0x1f || (code >= 0x7f && code <= 0x9f);
    const high = highSurrogate(code);
    if (control || lowSurrogate(code) || (high && !lowSurrogate(next))) return goQuote(text);
    if (high) i++;
  }
  return text;
}

/** Whether `code`, one code unit of a string, is a high surrogate: the first of a pair. */
function highSurrogate(code: number): boolean {
  return code >= 0xd800 && code <= 0xdbff;
}

/** Whether `code`, one code unit of a string, is a low surrogate: the second of a pair. */
function lowSurrogate(code: number): boolean {
  return code >= 0xdc00 && code <= 0xdfff;
}

/** A character Go's `strconv.IsPrint` prints as it is: a letter, mark, number, punctuation or
 *  symbol, or the ASCII space. */
const printable = /^[\p{L}\p{M}\p{N}\p{P}\p{S} ]$/u;

/** The escapes `strconv.Quote` writes for the C0 controls that have one. */
const shortEscapes: Record<number, string> = {
  7: "\\a",
  8: "\\b",
  9: "\\t",
  10: "\\n",
  11: "\\v",
  12: "\\f",
  13: "\\r",
};

/** `text` quoted as Go's `strconv.Quote` quotes it: `"` and `\` escaped, a printable character as
 *  it is, a control with a short escape by it, any other below U+0080 as `\xNN`, and every other
 *  character as `\uNNNN` or `\UNNNNNNNN`, in lower-case hexadecimal. A lone surrogate, which Go's
 *  JSON decoding reads as U+FFFD, is U+FFFD. */
function goQuote(text: string): string {
  let out = '"';
  for (const ch of text) {
    let code = ch.codePointAt(0) as number;
    if (highSurrogate(code) || lowSurrogate(code)) code = 0xfffd;
    const char = String.fromCodePoint(code);
    if (code === 0x22 || code === 0x5c) out += `\\${char}`;
    else if (printable.test(char)) out += char;
    else if (shortEscapes[code] !== undefined) out += shortEscapes[code];
    else if (code < 0x20 || code === 0x7f) out += `\\x${code.toString(16).padStart(2, "0")}`;
    else if (code < 0x10000) out += `\\u${code.toString(16).padStart(4, "0")}`;
    else out += `\\U${code.toString(16).padStart(8, "0")}`;
  }
  return `${out}"`;
}

/** Where the first URL in `token` starts (step 1), or -1. A `:` and two slashes that no scheme
 *  comes before start a URL at the token's first character, which {@link readRun} refuses. */
function urlStart(token: string): number {
  for (let i = 0; i + 1 < token.length; i++) {
    if (
      token[i] === ":" &&
      i + 2 < token.length &&
      isSlash(token[i + 1]) &&
      isSlash(token[i + 2])
    ) {
      const start = schemeStart(token, i);
      return start >= 0 ? start : 0;
    }
    if (
      isSlash(token[i]) &&
      isSlash(token[i + 1]) &&
      (i === 0 || !/[A-Za-z0-9+.\-:/\\]/.test(token[i - 1] as string))
    ) {
      return i;
    }
  }
  return -1;
}

/** The run at the start of `text`, which runs to its end, read (steps 2, 3 and 4): as it prints,
 *  its length in `text`, and whether it ends inside a query or a fragment; `undefined` where it is
 *  not readable. It calls `cut` with each span of `text`, `[from, to)`, that it removes: the user
 *  information with its `@`, each value, each bare segment and each path parameter's value. */
function readRun(
  text: string,
  cut: (from: number, to: number) => void
): { shown: string; length: number; inQuery: boolean } | undefined {
  let head = 2;
  if (!isSlash(text[0])) {
    const colon = text.indexOf(":");
    if (
      !/^[A-Za-z][A-Za-z0-9+.-]*$/.test(text.slice(0, Math.max(colon, 0))) ||
      colon + 2 >= text.length ||
      !isSlash(text[colon + 1]) ||
      !isSlash(text[colon + 2])
    ) {
      return undefined;
    }
    head = colon + 3;
  }
  const inQuery = /[?#]/.test(text.slice(head));
  const run = inQuery ? text : trimRunEnd(text);
  for (let i = 0; i < run.length; i++) if (runRefuses(run.charCodeAt(i))) return undefined;
  const rest = run.slice(head);
  const authEnd = rest.search(/[/?#]/);
  const authority = authEnd < 0 ? rest : rest.slice(0, authEnd);
  const after = authEnd < 0 ? "" : rest.slice(authEnd);
  if (authority.split("@").length > 2 || after.includes("@")) return undefined;
  const at = authority.indexOf("@");
  const host = authority.slice(at + 1);
  if (!digitPort(host)) return undefined;
  if (at >= 0) cut(head, head + at + 1);
  const afterAt = head + (authEnd < 0 ? rest.length : authEnd);
  let path = after;
  let query = "";
  let fragment = "";
  let queryAt = afterAt;
  let fragmentAt = afterAt;
  const hash = after.indexOf("#");
  if (hash >= 0) {
    path = after.slice(0, hash);
    fragment = after.slice(hash);
    fragmentAt += hash;
  }
  const question = path.indexOf("?");
  if (question >= 0) {
    query = path.slice(question);
    path = path.slice(0, question);
    queryAt += question;
  }
  const shownPath = pathParameters(path, afterAt, cut);
  if (shownPath === undefined) return undefined;
  let shown = run.slice(0, head) + host + shownPath;
  for (const [part, partAt] of [
    [query, queryAt],
    [fragment, fragmentAt],
  ] as const) {
    if (part === "") continue;
    shown += part[0];
    let segments = part.slice(1);
    let segmentAt = partAt + 1;
    for (;;) {
      const end = segments.search(/[&;]/);
      const segment = end < 0 ? segments : segments.slice(0, end);
      const equals = segment.indexOf("=");
      if (equals >= 0) {
        if (/[=?]/.test(segment.slice(equals + 1))) return undefined;
        shown += `${segment.slice(0, equals)}=`;
        if (equals + 1 < segment.length) cut(segmentAt + equals + 1, segmentAt + segment.length);
      } else if (segment !== "") {
        cut(segmentAt, segmentAt + segment.length);
      }
      if (end < 0) break;
      shown += segments[end];
      segments = segments.slice(end + 1);
      segmentAt += end + 1;
    }
  }
  return { shown, length: run.length, inQuery };
}

/** `path`, a run's path, as it prints, with each segment's `;name=value` parameter's value removed
 *  (`cut`, `path` starting at offset `at`), the name and `=` kept; a parameter with no `=` stays.
 *  `undefined` where a value holds `=`. Go's `pathParameters`. */
function pathParameters(
  path: string,
  at: number,
  cut: (from: number, to: number) => void
): string | undefined {
  if (!path.includes(";")) return path;
  let shown = "";
  for (let k = 0; ; ) {
    const semi = path.indexOf(";", k);
    if (semi < 0) return shown + path.slice(k);
    shown += path.slice(k, semi + 1);
    k = semi + 1;
    const stop = path.slice(k).search(/[/;]/);
    const end = stop < 0 ? path.length - k : stop;
    const parameter = path.slice(k, k + end);
    const equals = parameter.indexOf("=");
    if (equals >= 0) {
      if (parameter.slice(equals + 1).includes("=")) return undefined;
      shown += parameter.slice(0, equals + 1);
      if (equals + 1 < parameter.length) cut(at + k + equals + 1, at + k + end);
    } else {
      shown += parameter;
    }
    k += end;
  }
}

/** Whether `host`'s port, if it names one, is all digits: after `]:` for a bracketed IPv6 host,
 *  and after the first `:` otherwise. A `[` with no `]` is not read. */
function digitPort(host: string): boolean {
  let port = "";
  if (host.startsWith("[")) {
    const end = host.indexOf("]");
    if (end < 0) return false;
    const rest = host.slice(end + 1);
    if (rest !== "") {
      if (rest[0] !== ":") return false;
      port = rest.slice(1);
    }
  } else {
    const colon = host.indexOf(":");
    if (colon >= 0) port = host.slice(colon + 1);
  }
  return /^[0-9]*$/.test(port);
}

/** `s` without the characters `.,;:!?)]}'"` it ends in (steps 2 and 5). */
function trimRunEnd(s: string): string {
  let end = s.length;
  while (end > 0 && `.,;:!?)]}'"`.includes(s[end - 1] as string)) end--;
  return s.slice(0, end);
}

/** Where the scheme ending at `colon` starts in `s`: the first letter of the longest run of
 *  scheme characters before `colon`, or -1 when that run holds no letter. */
function schemeStart(s: string, colon: number): number {
  let start = colon;
  while (start > 0 && /[A-Za-z0-9+.-]/.test(s[start - 1] as string)) start--;
  while (start < colon && !/[A-Za-z]/.test(s[start] as string)) start++;
  return start === colon ? -1 : start;
}

function isSlash(character: string | undefined): boolean {
  return character === "/" || character === "\\";
}

/** Whether the UTF-16 code `code` is ASCII whitespace, which ends a run and a token. */
function isAsciiSpace(code: number): boolean {
  return code === 0x20 || (code >= 0x09 && code <= 0x0d);
}

/** The shortest run of a credential's characters {@link tokens} redacts wherever it stands. Go's
 *  `redact.TokenPiece`. */
const TOKEN_PIECE = 8;

/** What {@link tokens} writes in place of a run of a credential's characters. Go's
 *  `redact.Redacted`. */
const REDACTED = "[redacted]";

/** A letter, a digit or `_`: what may not stand right beside a short credential {@link tokens}
 *  redacts. */
const WORD_CHARACTER = /^[\p{L}\p{N}_]$/u;

/**
 * `text` with each run of at least {@link TOKEN_PIECE} characters of any of `credentials` (each
 * trimmed, as a header sends it) replaced with one {@link REDACTED}; a credential shorter than that
 * only whole, and only where no letter, digit or `_` stands right before or after it. A character
 * is a code point. `limit` bounds the text read, in characters, a negative one reading it whole:
 * a run that starts inside it is redacted whole, read up to {@link TOKEN_PIECE} - 1 characters
 * past it, and the answer is cut to `limit` characters afterwards. Go's `redact.Tokens`; both read
 * packages/envoy/internal/redact/testdata/tokens.json.
 */
export function tokens(text: string, limit: number, ...credentials: string[]): string {
  const pieces = new Set<string>();
  const lengths = new Set<number>();
  for (const credential of credentials) {
    const characters = Array.from(credential.trim());
    const length = Math.min(TOKEN_PIECE, characters.length);
    if (length === 0) continue;
    lengths.add(length);
    for (let at = 0; at + length <= characters.length; at++) {
      pieces.add(characters.slice(at, at + length).join(""));
    }
  }
  const characters = Array.from(text);
  const read = limit < 0 ? characters.length : Math.min(characters.length, limit);
  if (pieces.size === 0) return characters.slice(0, read).join("");
  const scanned =
    limit < 0 ? characters.length : Math.min(characters.length, limit + TOKEN_PIECE - 1);
  const covered = new Array<boolean>(scanned).fill(false);
  const word = (at: number): boolean => WORD_CHARACTER.test(characters[at] ?? "");
  for (let at = 0; at < scanned; at++) {
    for (const length of lengths) {
      if (at + length > scanned) continue;
      const piece = characters.slice(at, at + length).join("");
      if (!pieces.has(piece)) continue;
      if (length < TOKEN_PIECE && (word(at - 1) || word(at + length))) continue;
      covered.fill(true, at, at + length);
    }
  }
  let out = "";
  for (let at = 0; at < read; at++) {
    if (!covered[at]) {
      out += characters[at];
      continue;
    }
    while (at + 1 < scanned && covered[at + 1]) at++;
    out += REDACTED;
  }
  return limit < 0 ? out : Array.from(out).slice(0, read).join("");
}

/** What {@link refusalFetch} records of one answer: the credentials its request carried, as each
 * refusalFetch it passed through saw them. The record is keyed by the Response itself, so it
 * travels with an openid-client error's `response`; nothing matches it by a body or a message. */
type Refusal = { readonly credentials: string[] };

/** The record of each answer refusalFetch saw, including a successful or empty one: parsing a
 * successful answer can still fail with the request in its error, and a status line can echo it. */
const recordedOf = new WeakMap<Response, Refusal>();

/** The Response an openid-client or oauth4webapi error carries, itself or in its cause chain, or
 * undefined where it carries none. ResponseBodyError has `response`; an error can itself be a
 * Response in a test or a wrapper. */
function responseOfThrown(thrown: unknown): Response | undefined {
  let value = thrown;
  for (let depth = 0; depth < 8; depth += 1) {
    if (value instanceof Response) return value;
    if (typeof value !== "object" || value === null) return undefined;
    const response = Reflect.get(value, "response");
    if (response instanceof Response) return response;
    if (!("cause" in value)) return undefined;
    value = Reflect.get(value, "cause");
  }
  return undefined;
}

/** The fail-closed reading of an error whose response carries no refusalFetch record: its class
 * and status alone, never a word of the message, code or description. Go's `redact.withheld`. */
function withheldClass(thrown: unknown, status: number): string {
  const suffix = status === 0 ? "" : `, status ${status}`;
  return `${errorClass(thrown)}${suffix}`;
}

/** The request a fetch sends: the options it is sent with, its method (GET where it names none), a
 *  header record or a `Headers`, and its body. */
type FetchOptions = {
  body?: unknown;
  headers?: ConstructorParameters<typeof Headers>[0];
  method?: string;
};

/**
 * `base`, a fetch openid-client is given (its `customFetch`) or one beneath it, which records the
 * credentials each request carried ({@link requestCredentials}) against the Response it returns,
 * and changes nothing of any answer. A fetch underneath one can set another credential (a DPoP
 * proof); the wrapper above joins it into the same response-keyed record. {@link LibraryError}
 * reads an error from that response with those credentials redacted. A Response without a record
 * fails closed, as does a fetch failure with no Response. It is the one wrapper on every fetch
 * this workspace hands openid-client; the census in `cmd/redact-gate` fails on one without it.
 * Go's `redact.RefusalClient`.
 */
export function refusalFetch<Options extends FetchOptions>(
  base: (url: string, options: Options) => Promise<Response>
): (url: string, options: Options) => Promise<Response> {
  return async (url, options) => {
    const credentials = requestCredentials(url, options);
    const response = await base(url, options);
    // An answer need not be a refusal: parsing a 2xx answer can still return an error whose text
    // quotes it, and an empty answer's status can quote the request.
    const recorded = recordedOf.get(response);
    if (recorded !== undefined) {
      recorded.credentials.push(...credentials);
    } else {
      recordedOf.set(response, { credentials });
    }
    return response;
  };
}

/**
 * Every value a request carries under a name a wire contract does not declare public on the
 * request's route (`PUBLIC_REQUEST_NAMES` in public-request-names.ts, each name public only on the
 * routes its contract names, where it names any; the route is the method, GET where `options`
 * names none, and the URL's path): each such header's value whole and after its scheme, but for an
 * `Authorization` or a `Proxy-Authorization` that names a scheme, whose scheme (Bearer, token,
 * Basic, any word before its first space) is the header's grammar and no credential, only the part
 * after it; a Basic pair's user and password as sent and decoded; its URL's user and password and
 * each such value of its query; and each such value of its form or string of its JSON body. A name
 * nobody listed counts, so a new parameter is a credential until a contract declares it public,
 * and so does a listed name on a route its contract does not name. Go's `requestTokens`.
 */
export function requestCredentials(url: string, options: FetchOptions): string[] {
  const credentials: string[] = [];
  const route = requestRoute(url, options.method);
  let contentType = "";
  for (const [name, value] of new Headers(options.headers ?? {})) {
    if (name.toLowerCase() === "content-type") contentType = value.toLowerCase();
    if (isPublicRequestName("header", name, route)) continue;
    const authorization = ["authorization", "proxy-authorization"].includes(name.toLowerCase());
    const space = value.trim().indexOf(" ");
    if (!authorization || space < 0) credentials.push(value);
    if (space < 0) continue;
    const scheme = value.trim().slice(0, space);
    const credential = value
      .trim()
      .slice(space + 1)
      .trim();
    credentials.push(credential);
    if (scheme.toLowerCase() !== "basic") continue;
    const pair = decodedBase64(credential);
    const colon = pair?.indexOf(":") ?? -1;
    if (pair === undefined || colon < 0) continue;
    for (const part of [pair.slice(0, colon), pair.slice(colon + 1)]) {
      credentials.push(part, ...unescaped(part));
    }
  }
  const parsed = URL.canParse(url) ? new URL(url) : undefined;
  if (parsed !== undefined) {
    for (const part of [parsed.username, parsed.password]) {
      if (part !== "") credentials.push(...unescaped(part), part);
    }
    for (const [name, value] of parsed.searchParams) {
      if (!isPublicRequestName("field", name, route)) credentials.push(value);
    }
  }
  return credentials.concat(bodyCredentials(options.body, contentType, route));
}

/** Each value of a form body, or string of a JSON body, under a name no contract declares public on
 *  `route`, the request's; none for any other body. A JSON string's name is the key of the object
 *  it is a member of, through any arrays between; an object under a public key is read by its own
 *  keys. */
function bodyCredentials(body: unknown, contentType: string, route: RequestRoute): string[] {
  const formValues = (form: URLSearchParams): string[] =>
    [...form]
      .filter(([name]) => !isPublicRequestName("field", name, route))
      .map(([, value]) => value);
  if (body instanceof URLSearchParams) return formValues(body);
  const text =
    typeof body === "string"
      ? body
      : body instanceof Uint8Array || body instanceof ArrayBuffer
        ? new TextDecoder().decode(body)
        : undefined;
  if (text === undefined) return [];
  if (contentType.startsWith("application/x-www-form-urlencoded")) {
    return formValues(new URLSearchParams(text));
  }
  if (!/^application\/([\w.+-]*\+)?json\b/.test(contentType)) return [];
  const strings: string[] = [];
  const walk = (value: unknown, isPublic: boolean): void => {
    if (typeof value === "string") {
      if (!isPublic) strings.push(value);
    } else if (Array.isArray(value)) {
      for (const element of value) walk(element, isPublic);
    } else if (value !== null && typeof value === "object") {
      for (const [key, member] of Object.entries(value)) {
        walk(member, isPublicRequestName("field", key, route));
      }
    }
  };
  try {
    walk(JSON.parse(text), false);
  } catch {
    return [];
  }
  return strings;
}

/** `text` decoded from base64, or `undefined` where it is not base64. */
function decodedBase64(text: string): string | undefined {
  if (!/^[A-Za-z0-9+/]*={0,2}$/.test(text)) return undefined;
  try {
    return new TextDecoder().decode(Uint8Array.from(atob(text), (c) => c.charCodeAt(0)));
  } catch {
    return undefined;
  }
}

/** `text` with its percent and `+` escapes decoded, where that differs from it and is valid. */
function unescaped(text: string): string[] {
  try {
    const decoded = decodeURIComponent(text.replace(/\+/g, " "));
    return decoded === text ? [] : [decoded];
  } catch {
    return [];
  }
}
