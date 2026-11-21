/** The names a real-stack row makes its data under, as pure functions of the row they belong to
 *  and of the name its spec writes. `rowKey`, `rowPerson`, `rowSessionId` and `rowNumber` in
 *  e2e/test.ts call these with the running row (`test.info()`), and their doc comments say what
 *  each name is for; what is here is only how each is derived, written once and readable without
 *  a running row, so e2e/__tests__/row-names.test.ts can hold the derivation to its inputs. */
import { createHash } from "node:crypto";

/** What every name of a row derives from. Playwright gives each `--repeat-each` copy of a row the
 *  same `testId` and a `repeatEachIndex` of its own, and a retry the same two with a `retry` of its
 *  own, so all three are read: a name from `testId` alone would be the same name in two repeats of
 *  the row on one worker, and the second would read what the first left. `phase` is the phase the
 *  row has moved on to (`nextRowPhase`, e2e/test.ts), and `nonce` the bytes the process drew once,
 *  so a second run of the suite against the same database never meets a name the first made. */
export interface RowIdentity {
  readonly testId: string;
  readonly repeatEachIndex: number;
  readonly retry: number;
  readonly phase: number;
  readonly nonce: string;
}

/** How many names the five-letter suffix tells apart: 26 ** 5 (A-Z). */
const SUFFIXES = 26 ** 5;

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

/** The five letters A-Z, in base 26, of `rowDigest(row, logical)`: the suffix a row key and a
 *  row's person put after the name a spec writes. */
function rowSuffix(row: RowIdentity, logical: string): string {
  let rest = rowDigest(row, logical).readUIntBE(0, 6) % SUFFIXES;
  let suffix = "";
  for (let place = 0; place < 5; place += 1) {
    suffix = String.fromCharCode(65 + (rest % 26)) + suffix;
    rest = Math.floor(rest / 26);
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
 *  letters. */
export function rowKeyOf(row: RowIdentity, logical: string): string {
  if (!/^[A-Z][A-Z0-9]*$/.test(logical)) {
    throw new Error(
      `rowKey: the logical key ${JSON.stringify(logical)} must start with A-Z and hold only A-Z and 0-9`
    );
  }
  return `${logical.slice(0, 5)}${rowSuffix(row, logical)}`;
}

/** `rowPerson`'s login (e2e/test.ts): `logical` whole and `rowSuffix`'s five letters,
 *  lower-cased. */
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
 *  below 26 ** 5, plus `FIRST_ROW_NUMBER`. */
export function rowNumberOf(row: RowIdentity, logical: string): number {
  checkLogicalLogin("rowNumber", logical);
  return FIRST_ROW_NUMBER + (rowDigest(row, `number:${logical}`).readUIntBE(0, 6) % SUFFIXES);
}
