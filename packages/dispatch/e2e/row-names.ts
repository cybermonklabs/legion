/** The names a real-stack row makes its data under, as pure functions of the row they belong to
 *  and of the name its spec writes. `rowKey`, `rowPerson`, `rowSessionId` and `rowNumber` in
 *  e2e/test.ts call these with the running row (`test.info()`), and their doc comments say what
 *  each name is for; what is here is only how each is derived, written once and readable without
 *  a running row, so e2e/__tests__/row-names.test.ts can hold the derivation to its inputs. */
import { createHash } from "node:crypto";

/** What every name of a row derives from. Playwright 1.64.1 gives each `--repeat-each` copy of a
 *  row a `testId` and a worker of its own: `applyRepeatEachIndex` (playwright/lib/common/index.js)
 *  puts ` (repeat:N)` into what the id hashes and `-N` at the end of the worker hash. A retry
 *  keeps the `testId` and the `repeatEachIndex` of the attempt before it and has a `retry` of its
 *  own. All three are read, so two copies of a row get names of their own even under one
 *  `testId`, which Playwright 1.64.1 never gives two copies; reading `repeatEachIndex` beside an
 *  id that already differs does no harm. `phase` is the phase the row has moved on to
 *  (`nextRowPhase`, e2e/test.ts), and `nonce` the bytes the process drew once, so a second run of
 *  the suite against the same database never meets a name the first made. */
export interface RowIdentity {
  readonly testId: string;
  readonly repeatEachIndex: number;
  readonly retry: number;
  readonly phase: number;
  readonly nonce: string;
}

/** The symbols a suffix is spelled in: the twenty consonants other than Y, then the ten digits.
 *  Playwright matches a role's `name` string as a case-insensitive substring of an accessible
 *  name, so a key whose suffix spelled a word matched a control named by that word:
 *  `rowKey("CORE")` once drew COREPLYQU, and `{ name: "Reply" }` on its comment card also
 *  matched the "Copy reference dispatch://COREPLYQU-1/…" button (conversation.e2e.ts, nightly
 *  44915867310). With no vowel and no Y, a suffix spells only what consonants and digits spell
 *  alone, as "DNS" or "P0" do. A word can still run on from the logical key's own last letters
 *  into the suffix's first - CLOSE and ND hold "Send" - and `exact: true` keeps such a name to
 *  its own control. */
const SUFFIX_SYMBOLS = "BCDFGHJKLMNPQRSTVWXZ0123456789";

/** How many names the five-character suffix tells apart: 30 ** 5, 24,300,000. */
const SUFFIXES = SUFFIX_SYMBOLS.length ** 5;

/** How many numbers `rowNumberOf` tells apart: 26 ** 5, 11,881,376. 10,000 more still fits a
 *  Postgres `integer`, which a pull request's number is. */
const ROW_NUMBERS = 26 ** 5;

/** The first number `rowNumberOf` answers. Every number the suite writes as a fixed one - the
 *  review corpus's pull requests and runs (e2e/delivery-seed.ts), a pull request a spec names -
 *  is below it, so a row's number is never one of them. */
const FIRST_ROW_NUMBER = 10_000;

/** A SHA-256 of every field of `row` and of `logical`: what every name below derives from. */
function rowDigest(row: RowIdentity, logical: string): Buffer {
  return createHash("sha256")
    .update([row.testId, row.repeatEachIndex, row.retry, row.phase, row.nonce, logical].join("\n"))
    .digest();
}

/** The five characters of `SUFFIX_SYMBOLS`, in base 30, of `rowDigest(row, logical)`: the suffix
 *  a row key and a row's person put after the name a spec writes. */
function rowSuffix(row: RowIdentity, logical: string): string {
  let rest = rowDigest(row, logical).readUIntBE(0, 6) % SUFFIXES;
  let suffix = "";
  for (let place = 0; place < 5; place += 1) {
    suffix = SUFFIX_SYMBOLS.charAt(rest % SUFFIX_SYMBOLS.length) + suffix;
    rest = Math.floor(rest / SUFFIX_SYMBOLS.length);
  }
  return suffix;
}

/** Refuses a `logical` that is no name `rowPersonOf`, `rowSessionIdOf`, `rowNumberOf` and `own`
 *  (e2e/test.ts) take: a-z, then a-z and 0-9. `caller` names the function in the error. */
export function checkLogicalLogin(caller: string, logical: string): void {
  if (!/^[a-z][a-z0-9]*$/.test(logical)) {
    throw new Error(
      `${caller}: the logical name ${JSON.stringify(logical)} must start with a-z and hold only a-z and 0-9`
    );
  }
}

/** `rowKey`'s key (e2e/test.ts): the first five characters of `logical` and `rowSuffix`'s five
 *  consonants and digits. It starts with `logical`'s first letter and is at most ten characters,
 *  so it is a key `PROJECT_KEY_PATTERN` (@legion/contracts) takes. */
export function rowKeyOf(row: RowIdentity, logical: string): string {
  if (!/^[A-Z][A-Z0-9]*$/.test(logical)) {
    throw new Error(
      `rowKey: the logical key ${JSON.stringify(logical)} must start with A-Z and hold only A-Z and 0-9`
    );
  }
  return `${logical.slice(0, 5)}${rowSuffix(row, logical)}`;
}

/** `rowPerson`'s login (e2e/test.ts): `logical` whole and `rowSuffix`'s five consonants and
 *  digits, lower-cased. */
export function rowPersonOf(row: RowIdentity, logical: string): string {
  checkLogicalLogin("rowPerson", logical);
  return `${logical}${rowSuffix(row, logical).toLowerCase()}`;
}

/** `rowSessionId`'s UUID (e2e/test.ts): the first sixteen bytes of the digest of `session:` and
 *  `logical`, with RFC 9562's version 8 and its variant written into them. */
export function rowSessionIdOf(row: RowIdentity, logical: string): string {
  checkLogicalLogin("rowSessionId", logical);
  const bytes = rowDigest(row, `session:${logical}`).subarray(0, 16);
  bytes.writeUInt8((bytes.readUInt8(6) & 0x0f) | 0x80, 6);
  bytes.writeUInt8((bytes.readUInt8(8) & 0x3f) | 0x80, 8);
  const hex = bytes.toString("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

/** `rowNumber`'s number (e2e/test.ts): the digest of `number:` and `logical` read as a number
 *  below `ROW_NUMBERS`, plus `FIRST_ROW_NUMBER`. */
export function rowNumberOf(row: RowIdentity, logical: string): number {
  checkLogicalLogin("rowNumber", logical);
  return FIRST_ROW_NUMBER + (rowDigest(row, `number:${logical}`).readUIntBE(0, 6) % ROW_NUMBERS);
}
