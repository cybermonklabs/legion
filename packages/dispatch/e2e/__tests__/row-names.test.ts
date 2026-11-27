/** A row's names are its own in every copy of it a run makes, and their suffix holds no vowel.
 *
 *  The database is emptied before the worker's first row and before the review corpus's seed,
 *  not before each row, so a row reads beside what the rows before it on the worker wrote
 *  (e2e/test.ts). Playwright 1.64.1 gives each `--repeat-each` copy of a row a
 *  `testId` and a worker of its own (`RowIdentity`, e2e/row-names.ts), and the derivation reads
 *  `repeatEachIndex` beside `testId` as well. These rows hold that derivation for one `testId`
 *  they give both copies, which Playwright 1.64.1 never gives two copies: two copies that differ
 *  in `repeatEachIndex` alone still get a project, a person, a session and a number each. They
 *  read the derivations of e2e/row-names.ts, which `rowKey`, `rowPerson`, `rowSessionId` and
 *  `rowNumber` call with the running row, over those two copies, and the suffix a key and a
 *  person end in over many rows: five of the twenty consonants other than Y and the ten digits,
 *  so a control's name, which Playwright matches as a substring, finds no word in it but one that
 *  consonants and digits spell alone.
 */

import { expect, test } from "bun:test";
import { PROJECT_KEY_PATTERN } from "@legion/contracts";

import { type RowIdentity, rowKeyOf, rowNumberOf, rowPersonOf, rowSessionIdOf } from "../row-names";

/** The symbols a suffix may hold: B C D F G H J K L M N P Q R S T V W X Z and 0-9. */
const SUFFIX_SYMBOLS = "BCDFGHJKLMNPQRSTVWXZ0123456789";

/** Copy `repeatEachIndex` of one row, in one process, on its first attempt and phase. */
function copy(repeatEachIndex: number): RowIdentity {
  return {
    nonce: "0123456789abcdef0123456789abcdef",
    phase: 0,
    repeatEachIndex,
    retry: 0,
    testId: "3f1c2b9a8e7d6c5b4a39-1d2e3f4a5b6c7d8e9f0a",
  };
}

/** Every kind of name a row asks for, for the logical names the suite writes most. */
function namesOf(row: RowIdentity) {
  return {
    key: rowKeyOf(row, "CORE"),
    number: rowNumberOf(row, "issue"),
    person: rowPersonOf(row, "alice"),
    session: rowSessionIdOf(row, "planner"),
  };
}

test("two copies of one row get a key, a person, a session id and a number each", () => {
  const first = namesOf(copy(0));
  const second = namesOf(copy(1));
  expect(second.key).not.toBe(first.key);
  expect(second.person).not.toBe(first.person);
  expect(second.session).not.toBe(first.session);
  expect(second.number).not.toBe(first.number);
});

test("a copy of a row asking twice gets the same names", () => {
  expect(namesOf(copy(0))).toEqual(namesOf(copy(0)));
  expect(namesOf(copy(1))).toEqual(namesOf(copy(1)));
});

/** The copies of one row at the first `count` values of `repeatEachIndex`. */
function copies(count: number): RowIdentity[] {
  return Array.from({ length: count }, (_, index) => copy(index));
}

test("a suffix is five consonants other than Y and digits, and draws on every one of them", () => {
  const drawn = new Set<string>();
  for (const row of copies(2000)) {
    const key = rowKeyOf(row, "CORE");
    const person = rowPersonOf(row, "alice");
    expect(key).toMatch(new RegExp(`^CORE[${SUFFIX_SYMBOLS}]{5}$`));
    expect(person).toMatch(new RegExp(`^alice[${SUFFIX_SYMBOLS.toLowerCase()}]{5}$`));
    for (const symbol of key.slice("CORE".length)) drawn.add(symbol);
  }
  // No vowel and no Y in 10,000 drawn symbols, and each of the thirty drawn: the digits are as
  // much the suffix's as the consonants are.
  expect([...drawn].sort()).toEqual([...SUFFIX_SYMBOLS].sort());
});

test("a key is a project key the server takes, whatever its logical key's length", () => {
  for (const row of copies(200)) {
    for (const logical of ["X", "E2E", "CORE", "INFRA", "LONGREASON"]) {
      const key = rowKeyOf(row, logical);
      expect(key).toMatch(PROJECT_KEY_PATTERN);
      expect(key).toHaveLength(Math.min(logical.length, 5) + 5);
      expect(key.startsWith(logical.slice(0, 5))).toBe(true);
    }
  }
});
