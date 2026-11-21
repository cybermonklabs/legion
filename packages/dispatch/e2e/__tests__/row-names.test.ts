/** A row's names are its own in every copy of it a run makes.
 *
 *  The database is emptied once per worker, so a row reads beside what every row before it on
 *  the worker wrote (e2e/test.ts), and `--repeat-each` runs the copies of one row one after the
 *  other on one worker. Playwright gives those copies the same `testId` and a `repeatEachIndex`
 *  each, so a name derived from `testId` alone would be the same project, person, session and
 *  number in both copies, and the second would read what the first left. These read the
 *  derivations of e2e/row-names.ts, which `rowKey`, `rowPerson`, `rowSessionId` and `rowNumber`
 *  call with the running row, over two copies of one row.
 */

import { expect, test } from "bun:test";

import { type RowIdentity, rowKeyOf, rowNumberOf, rowPersonOf, rowSessionIdOf } from "../row-names";

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
