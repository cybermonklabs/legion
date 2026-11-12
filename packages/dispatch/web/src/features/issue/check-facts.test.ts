import { expect, test } from "bun:test";

import type { CheckResult } from "../../api/types";
import { resultFacts } from "./check-facts";
import { RESULT_CURRENCY_WORDS } from "./page-words";

/** The commit the result ran at, which is also the one its check compares results with. */
const RESULT_COMMIT = "c".repeat(40);

/** One recorded pass at the commit its check compares with, against the words of the check as
 *  they stand, with whatever the case changes about it. */
function recorded(extra: Partial<CheckResult> = {}): CheckResult {
  return {
    actor: { id: "tester", kind: "session" },
    check_artifact_id: "artifact-verification",
    check_block_id: "c1",
    check_issue_key: "CORE-1",
    check_sha256: "sum",
    check_version: 1,
    check_words_moved: false,
    commit_sha: RESULT_COMMIT,
    commit_stands: { answer: "at_basis" },
    created_at: "2026-11-05T00:00:00Z",
    excluded: 0,
    failed: 0,
    id: "result-1",
    issue_key: "CORE-1",
    negative_control: "Break it and the run fails.",
    observed: "41 passed",
    role: "tester",
    run: "bun test web/src",
    screenshots: [],
    session: "session-1",
    skipped: 0,
    surface: "cli",
    verdict: "pass",
    ...extra,
  };
}

/** What the result's body says of where it stands, or `undefined` where it draws no such fact. */
function standsWord(result: CheckResult): string | undefined {
  return resultFacts(result).find((fact) => fact.name === "stands")?.value;
}

test("a result at the commit its check compares with reads as current", () => {
  expect(standsWord(recorded())).toBe(RESULT_CURRENCY_WORDS.current);
});

test("a result the work has moved past reads as older", () => {
  expect(standsWord(recorded({ commit_stands: { answer: "other_commit" } }))).toBe(
    RESULT_CURRENCY_WORDS.older
  );
});

// "Current" means both halves: at the commit the work stands at, and against the words the
// result proved. A check reworded since keeps the first half and loses the second, and the
// result read "Current" over words it never ran against (`checkState` reads the same field).
test("a result under a check whose words moved since does not read as current", () => {
  expect(standsWord(recorded({ check_words_moved: true }))).toBe(RESULT_CURRENCY_WORDS.older);
});

test("a result whose commit Dispatch could not place draws no word for where it stands", () => {
  expect(standsWord(recorded({ commit_stands: { answer: "unknown" } }))).toBeUndefined();
  expect(standsWord(recorded({ commit_stands: undefined }))).toBeUndefined();
});
