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

// A result that counts for nothing says so in the server's own words (`not_counted`), as the
// Verification tab ends its entry with them: a role's result of a check CI proves, or a reading
// recorded before the current deploy of the program its check reads. It says so right after where
// it stands, which it qualifies. A result that counts draws no such fact (the control).
test("a result that counts for nothing says why, and one that counts says nothing of it", () => {
  const reason =
    "a role recorded it, and only a run of the CI job its ci_check names records the result of a check CI proves";
  const facts = resultFacts(recorded({ not_counted: reason }));
  expect(facts.map((fact) => fact.name).slice(0, 3)).toEqual(["verdict", "stands", "not_counted"]);
  expect(facts.find((fact) => fact.name === "not_counted")?.value).toBe(reason);
  expect(nodeFactLabel("not_counted")).toBe("counts for nothing");
  for (const counted of [recorded(), recorded({ not_counted: "" })]) {
    expect(resultFacts(counted).some((fact) => fact.name === "not_counted")).toBe(false);
  }
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

// A check CI proves whose run proved nothing and that holds no result at all draws, in place of a
// body that says nothing of why, the server's own sentence saying what the run showed.
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

/** The head of the other open pull request, whose run derived a result of its own. */
const OTHER_HEAD = "d".repeat(40);

// The check's `ci` is the read of one head, and `ci.result` names the result that read derived,
// the only result of it. Catches: a body that reads any result the check holds as the read's own,
// so a check whose read derived nothing while it holds a result derived at another head, or one
// its CI job recorded, says nothing of why, where `dispatch results --issue` heads the check with
// "no result: <detail>". The job's result is held in `results` as checkWithNewerFields serves it,
// and in `recorded_results` as the server sends it. The control: a read that names the result it
// derived, which the check holds beside the others, draws no such sentence.
test("a check CI proves whose read derived nothing says why, whatever other result it holds", () => {
  const detail = `CI run contracts is in_progress at ${RESULT_COMMIT}, the head of acme/widgets#8, and has not concluded.`;
  const unproved = ciRead({
    case: "not_completed",
    commit: RESULT_COMMIT,
    detail,
    repo: "acme/widgets",
    state: "no_result",
  });
  const atOtherHead = recorded({
    commit_sha: OTHER_HEAD,
    id: "ci-run:701",
    role: "ci",
    run: "contracts",
  });
  const jobResult = recorded({
    branch: "legion/CORE-1",
    id: "job-1",
    role: "ci",
    run: "contracts",
  });
  for (const held of [
    { results: [atOtherHead] },
    { results: [jobResult] },
    { results: [atOtherHead, jobResult] },
    { recorded_results: [jobResult], results: [atOtherHead] },
    { recorded_results: [jobResult], results: [] },
  ] satisfies Partial<CheckBlock>[]) {
    const facts = factsByName(check({ ci: unproved, ci_check: "contracts", run: "", ...held }));
    expect(facts.ci_unproved).toBe(detail);
  }

  const derived = recorded({ id: "ci-run:802", role: "ci", run: "contracts" });
  const proved = ciRead({ commit: RESULT_COMMIT, repo: "acme/widgets", result: "ci-run:802" });
  const facts = factsByName(
    check({
      ci: proved,
      ci_check: "contracts",
      results: [atOtherHead, derived, jobResult],
      run: "",
    })
  );
  expect(facts.ci_unproved).toBeUndefined();
});

// A block that carries `ci_check` and breaks one of its rules is no check CI proves, so the server
// gives it no `ci`: it reads as the ordinary check it is, with no CI fact at all.
test("a check the server gives no ci draws no CI fact", () => {
  for (const block of [
    check({ ci_check: "contracts", results: [recorded()], run: "" }),
    check({ ci_check: "contracts", run: "" }),
  ]) {
    expect(Object.keys(factsByName(block))).toEqual(["when", "surface", "run", "negative_control"]);
  }
});
