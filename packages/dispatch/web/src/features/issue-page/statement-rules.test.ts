import { expect, test } from "bun:test";

import type { CheckBlock } from "../../api/types";
import { NODE_STATE_WORDS, VERIFICATION_WORDS } from "./page-words";
import {
  type CheckStateReads,
  checkStanding,
  namedCommit,
  stateAtCommit,
  statementState,
} from "./statement-rules";
import { COMMIT, check, OTHER_COMMIT, result } from "./verification-filter/verification-fixtures";

/**
 * R6's statement rule, which Dashboard's two tiles, the Verification view and the Spec view read:
 * a statement reads Met only where every check that serves it passed at the one commit the page
 * names (`namedCommit`), the commit of the newest result recorded against any check of the issue;
 * a result recorded at another commit reads Out of date there and makes its statement Unknown. A
 * check's own row keeps its own reading (`checkStanding`, decision 152).
 */

/** How each of `checks` stands at the commit the page names for them all, as a statement reads
 *  its checks. */
function atNamedCommit(checks: readonly CheckStateReads[]) {
  const commit = namedCommit(checks);
  return checks.map((one) => ({ atCommit: stateAtCommit(checkStanding(one), commit) }));
}

/** A `before_merge` check of an issue with two pull requests, whose heads are `COMMIT` and
 *  `OTHER_COMMIT`, so a result at either head stands at the check's basis (`at_basis`). */
function twoHeads(partial: Partial<CheckBlock> & { block_id: string }): CheckBlock {
  return check({
    compared_with: [
      { commit: COMMIT, kind: "pull_request", pull_request: 7 },
      { commit: OTHER_COMMIT, kind: "pull_request", pull_request: 8 },
    ],
    when: "before_merge",
    ...partial,
  });
}

/** A check of `twoHeads` whose one result passed at `commit`, recorded at `at`. */
function passedAt(blockId: string, commit: string, at: string): CheckBlock {
  return twoHeads({
    block_id: blockId,
    results: [
      result({
        check_block_id: blockId,
        commit_sha: commit,
        created_at: at,
        id: `pass-${blockId}`,
      }),
    ],
  });
}

// R6: "A result recorded at another commit reads Out of date and makes its statement Unknown."
// Each pass stands at its own check's basis, a head of one of the issue's two pull requests, so
// each check's own row reads Passed; the newer pass names the commit, and the older one is not at
// it.
test("a statement whose two checks passed at different commits reads Unknown, the older Out of date at the named commit and Passed on its row", () => {
  const older = passedAt("c-older", OTHER_COMMIT, "2026-11-13T09:00:00Z");
  const newer = passedAt("c-newer", COMMIT, "2026-11-14T09:00:00Z");
  const checks = [older, newer];

  expect(namedCommit(checks)).toBe(COMMIT);
  // Each check's own row: Passed, read at the commit the server names for it.
  expect(checks.map((one) => checkStanding(one).state)).toEqual(["passed", "passed"]);
  // At the commit the page names, the older pass is Out of date.
  expect(atNamedCommit(checks)).toEqual([{ atCommit: "older" }, { atCommit: "passed" }]);
  expect(statementState(atNamedCommit(checks))).toBe("unknown");
  expect(NODE_STATE_WORDS.check.older).toBe("Out of date");
  expect(VERIFICATION_WORDS.states.unknown).toBe("Unknown");
  // Whatever order the read sends them in.
  expect(statementState(atNamedCommit([newer, older]))).toBe("unknown");
});

// R6: "A statement reads Met only where every check that serves it passed at the one commit the
// page names."
test("a statement whose checks both passed at the named commit reads Met", () => {
  const checks = [
    passedAt("c-first", COMMIT, "2026-11-13T09:00:00Z"),
    passedAt("c-second", COMMIT, "2026-11-14T09:00:00Z"),
  ];

  expect(namedCommit(checks)).toBe(COMMIT);
  expect(atNamedCommit(checks)).toEqual([{ atCommit: "passed" }, { atCommit: "passed" }]);
  expect(statementState(atNamedCommit(checks))).toBe("met");
});

// Root R17's Not met is a fail at that commit; a fail at another commit is no fail there, and
// reads Out of date as an older pass does.
test("a fail at the named commit leaves its statement Not met, and a fail at another commit leaves it Unknown", () => {
  const passed = passedAt("c-passed", COMMIT, "2026-11-13T09:00:00Z");
  const failedHere = twoHeads({
    block_id: "c-failed",
    pass_stands: { answer: "no" },
    results: [
      result({
        check_block_id: "c-failed",
        created_at: "2026-11-14T09:00:00Z",
        failed: 1,
        id: "fail-here",
        verdict: "fail",
      }),
    ],
  });
  expect(namedCommit([passed, failedHere])).toBe(COMMIT);
  expect(atNamedCommit([passed, failedHere])).toEqual([
    { atCommit: "passed" },
    { atCommit: "failed" },
  ]);
  expect(statementState(atNamedCommit([passed, failedHere]))).toBe("not_met");

  const failedElsewhere = twoHeads({
    block_id: "c-failed",
    pass_stands: { answer: "no" },
    results: [
      result({
        check_block_id: "c-failed",
        commit_sha: OTHER_COMMIT,
        created_at: "2026-11-12T09:00:00Z",
        failed: 1,
        id: "fail-elsewhere",
        verdict: "fail",
      }),
    ],
  });
  expect(checkStanding(failedElsewhere).state).toBe("failed");
  expect(namedCommit([passed, failedElsewhere])).toBe(COMMIT);
  expect(atNamedCommit([passed, failedElsewhere])).toEqual([
    { atCommit: "passed" },
    { atCommit: "older" },
  ]);
  expect(statementState(atNamedCommit([passed, failedElsewhere]))).toBe("unknown");
});

// A check the server says passed, whose pass the read does not hold, stands on no result, so no
// result read shows it passed at the commit the page names.
test("a check the server says passed with no pass in the read is Out of date at the named commit", () => {
  const unheld = twoHeads({
    block_id: "c-unheld",
    pass_stands: { answer: "yes" },
    results: [
      result({
        check_block_id: "c-unheld",
        commit_stands: { answer: "other_commit" },
        failed: 1,
        id: "fail-unplaced",
        verdict: "fail",
      }),
    ],
  });
  const standing = checkStanding(unheld);

  expect(standing).toEqual({ state: "passed" });
  expect(namedCommit([unheld])).toBe(COMMIT);
  expect(stateAtCommit(standing, COMMIT)).toBe("older");
  expect(statementState(atNamedCommit([unheld]))).toBe("unknown");
});

// Where no result that stands names a commit, nothing is read against one: each check reads as it
// stands on its own (`checkStanding`), which is how every statement read before R6.
test("with no named commit each check reads as it stands on its own", () => {
  const noCommit = twoHeads({
    block_id: "c-no-commit",
    results: [result({ check_block_id: "c-no-commit", commit_sha: "", id: "pass-no-commit" })],
  });
  const unrun = twoHeads({ block_id: "c-unrun", pass_stands: { answer: "no" }, results: [] });

  expect(namedCommit([noCommit, unrun])).toBeUndefined();
  expect(checkStanding(noCommit).state).toBe("passed");
  expect(atNamedCommit([noCommit])).toEqual([{ atCommit: "passed" }]);
  expect(statementState(atNamedCommit([noCommit]))).toBe("met");
  expect(atNamedCommit([noCommit, unrun])).toEqual([
    { atCommit: "passed" },
    { atCommit: "no-result" },
  ]);
  expect(statementState(atNamedCommit([noCommit, unrun]))).toBe("unknown");
  // A statement with no check is Unknown, whatever commit is named.
  expect(statementState([])).toBe("unknown");
});

/** Why a role's result of a check CI proves counts for nothing, as the server says it. */
const ROLE_ON_CI_CHECK =
  "a role recorded it, and only a run of the CI job its ci_check names records the result of a check CI proves";

// A result that counts for nothing is no evidence, as the server's pass_stands reads it
// (results_pass_stands.go skips it as it skips a withdrawn one): a role's result of a check CI
// proves, or a reading recorded before the current deploy of the program its check reads.
// Catches: a role's newer pass at the basis turning a check whose CI job's fail stands there into
// Passed; a check whose only result counts for nothing reading Passed, or Retracted where nobody
// withdrew anything; and such a result naming the page's commit.
test("a result that counts for nothing decides no state of its check and names no commit", () => {
  const rolePass = result({
    check_block_id: "c-ci",
    commit_sha: OTHER_COMMIT,
    created_at: "2026-11-15T09:00:00Z",
    id: "role-pass",
    not_counted: ROLE_ON_CI_CHECK,
  });
  const jobFail = result({
    branch: "legion/CORE-1",
    check_block_id: "c-ci",
    created_at: "2026-11-14T09:00:00Z",
    id: "job-fail",
    role: "ci",
    verdict: "fail",
  });
  const failed = twoHeads({
    block_id: "c-ci",
    pass_stands: { answer: "no", reason: "fail" },
    results: [rolePass, jobFail],
  });
  const roleOnly = twoHeads({
    block_id: "c-ci",
    pass_stands: { answer: "no", reason: "no_result" },
    results: [rolePass],
  });

  expect(checkStanding(failed)).toEqual({ stands: jobFail, state: "failed" });
  expect(checkStanding(roleOnly)).toEqual({ state: "no-result" });
  expect(namedCommit([failed])).toBe(COMMIT);
  expect(namedCommit([roleOnly])).toBeUndefined();
});
