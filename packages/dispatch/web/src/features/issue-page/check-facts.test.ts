import { expect, test } from "bun:test";

import type { CheckBlock, CheckResult } from "../../api/types";
import { checkFacts, resultFacts } from "./check-facts";
import { nodeFactLabel, RESULT_STANDS_WORDS } from "./page-words";

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

test("a result at the commit its check compares with reads as up to date", () => {
  expect(standsWord(recorded())).toBe(RESULT_STANDS_WORDS.at_basis);
  expect(RESULT_STANDS_WORDS.at_basis).toBe("Up to date");
});

test("a result the work has moved past reads as out of date", () => {
  expect(standsWord(recorded({ commit_stands: { answer: "other_commit" } }))).toBe(
    RESULT_STANDS_WORDS.other_commit
  );
  expect(RESULT_STANDS_WORDS.other_commit).toBe("Out of date");
});

// "Up to date" means both halves: at the commit the work stands at, and against the words the
// result proved. A check reworded since keeps the first half and loses the second, and the
// result once read as current over words it never ran against (`checkState` reads the same
// field). The moved words are read before the commit, so a commit Dispatch could not place
// does not hide them.
test("a result under a check whose words moved since does not read as up to date", () => {
  expect(standsWord(recorded({ check_words_moved: true }))).toBe("Out of date");
  expect(
    standsWord(recorded({ check_words_moved: true, commit_stands: { answer: "unknown" } }))
  ).toBe("Out of date");
});

test("a result whose commit Dispatch could not place draws no word for where it stands", () => {
  expect(standsWord(recorded({ commit_stands: { answer: "unknown" } }))).toBeUndefined();
  expect(standsWord(recorded({ commit_stands: undefined }))).toBeUndefined();
});

/** A check of the Verification document as the results read answers one, with whatever the case
 *  changes about it. */
function check(extra: Partial<CheckBlock> = {}): CheckBlock {
  return {
    artifact_id: "artifact-verification",
    block_id: "c-ci",
    compared_with: [],
    issue_key: "CORE-1",
    negative_control: "Break the list and the run fails.",
    pass_stands: { answer: "no" },
    ref: "dispatch://CORE-1/artifact/verification-md#b-c-ci",
    results: [],
    run: "bun test web/src",
    sha256: "sum",
    surface: "cli",
    text: "Every check of the document is listed.",
    version: 1,
    when: "before_merge",
    ...extra,
  };
}

/** What the server read of a CI run at the issue's pull request head (`CheckBlock.ci`). */
function ciRead(extra: Partial<NonNullable<CheckBlock["ci"]>> = {}): NonNullable<CheckBlock["ci"]> {
  return {
    case: "success",
    detail: `CI run contracts concluded success at ${RESULT_COMMIT}, the head of acme/widgets#7.`,
    line: `- Check c-ci (before_merge): pass at ${RESULT_COMMIT}, from CI run contracts. Check c-ci`,
    run: "contracts",
    state: "pass",
    ...extra,
  };
}

/** The facts of `block`, by name, and the value each draws. */
function factsByName(block: CheckBlock): Record<string, string> {
  return Object.fromEntries(checkFacts(block).map((fact) => [fact.name, fact.value]));
}

// A check CI proves names the CI run that proves it, under the label the document draws its
// `ci_check` under, and its one result is the one the server derived from that run, drawn under
// it as a recorded one is (main's #3972, which the Verification tab drew as "CI check").
test("a check CI proves names the CI run that proves it", () => {
  const derived = recorded({ id: "ci-run:9001", role: "ci", run: "contracts" });
  const facts = factsByName(
    check({ ci: ciRead(), ci_check: "contracts", results: [derived], run: "" })
  );
  expect(facts.ci_check).toBe("contracts");
  expect(nodeFactLabel("ci_check")).toBe("CI check");
  expect(facts.ci_unproved).toBeUndefined();
  expect(Object.keys(facts)).toEqual(["when", "surface", "run", "ci_check", "negative_control"]);
});

// A check CI proves whose run proved nothing holds no result at all, and in place of a body that
// says nothing of why, it draws the server's own sentence saying what the run showed.
test("a check CI proves with no result says why its run proved nothing", () => {
  const detail = `CI run contracts concluded skipped at ${RESULT_COMMIT}, the head of acme/widgets#7, which proves nothing.`;
  const facts = factsByName(
    check({
      ci: ciRead({ case: "skipped", detail, state: "no_result" }),
      ci_check: "contracts",
      run: "",
    })
  );
  expect(facts.ci_check).toBe("contracts");
  expect(facts.ci_unproved).toBe(detail);
  expect(nodeFactLabel("ci_unproved")).toBe("why no result");
});

// A block that carries `ci_check` and holds a recorded result is no check CI proves, so the
// server gives it no `ci`: it reads as the ordinary check it is, with no CI fact at all.
test("a check the server gives no ci draws no CI fact", () => {
  for (const block of [
    check({ ci_check: "contracts", results: [recorded()], run: "" }),
    check({ ci_check: "contracts", run: "" }),
  ]) {
    expect(Object.keys(factsByName(block))).toEqual(["when", "surface", "run", "negative_control"]);
  }
});
