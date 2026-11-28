import { expect, test } from "bun:test";

import type { CheckBlock } from "../../api/types";
import { type CheckState, NODE_STATE_WORDS, VERIFICATION_WORDS } from "./page-words";
import {
  type CheckStateReads,
  checkStanding,
  namedCommit,
  standsAtBasis,
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

// ---------------------------------------------------------------------------------------------
// A check that reads the live deploy: one of production or of the graph, run after the merge or
// on the default branch (the server's `readsTheLiveDeploy`, `results_deploys.go`). The server
// decides it by the current deploy of the program it reads, counting only the results recorded
// at that deploy's commit after it went out (`markNotCounted`), and answers over them alone
// (`liveResultStands`), so its row reads that answer and no pass at its basis. The issue page
// once read Passed where the results route answered unknown, with no deploy and after a deploy
// at another commit (dispatch://INFRA-5610). Each test below is one case: a live check under
// each answer the route gives it, and beside them checks that read no deploy, as they read before.
// ---------------------------------------------------------------------------------------------

/** The block id of each row's one check. */
const LIVE = "c-live";

/** A pass at the basis the check compares with (`at_basis`), at `COMMIT`. */
const PASS_AT_BASIS = result({ check_block_id: LIVE, id: "pass-basis" });

/** A check of production run after the merge, whose one result is a pass at its basis, with
 *  whatever the row changes about it. */
function liveCheck(partial: Partial<CheckBlock>): CheckBlock {
  return check({
    block_id: LIVE,
    results: [PASS_AT_BASIS],
    subject: "production",
    when: "after_merge",
    ...partial,
  });
}

/** The current deploy of the program the check reads, at `commit`, gone out before every result
 *  these rows record. */
function deployAt(commit: string): NonNullable<CheckBlock["pass_stands"]["deploy"]> {
  return { commit, deployed_at: "2026-11-14T00:00:00Z", program: "dispatch" };
}

/** How `block` reads on its own row: the state `checkStanding` reads, by its key and by the
 *  page's word for it. */
function rowReading(block: CheckBlock): { readonly state: CheckState; readonly word: string } {
  const { state } = checkStanding(block);
  return { state, word: NODE_STATE_WORDS.check[state] };
}

// With no deploy of the program the route answers unknown (`no_deploy`), and a pass at the
// check's basis says nothing of a deploy that is not there.
test("a production check after the merge with no deploy of its program reads Cannot tell over a pass at its basis", () => {
  const noDeploy = liveCheck({ pass_stands: { answer: "unknown", reason: "no_deploy" } });

  expect(rowReading(noDeploy)).toEqual({ state: "unmarked", word: "Cannot tell" });
  expect(checkStanding(noDeploy).stands).toBeUndefined();
});

// Once the program is deployed at another commit, the server marks the pass at the basis not
// counted, and it stands at no basis on the page either.
test("a production check after the merge whose program's deploy runs another commit reads Cannot tell over a pass that deploy does not count", () => {
  const uncounted = result({
    check_block_id: LIVE,
    id: "pass-basis",
    not_counted:
      "it names a commit that the current deploy of the program this check reads does not run",
  });
  const elsewhere = liveCheck({
    pass_stands: {
      answer: "unknown",
      deploy: deployAt(OTHER_COMMIT),
      reason: "no_result_from_deploy",
    },
    results: [uncounted],
  });

  expect(rowReading(elsewhere)).toEqual({ state: "unmarked", word: "Cannot tell" });
  expect(checkStanding(elsewhere).stands).toBeUndefined();
  // The one fact that tells the two passes apart is `not_counted`: the same pass counted stands
  // at its basis.
  expect(standsAtBasis(uncounted)).toBe(false);
  expect(standsAtBasis(PASS_AT_BASIS)).toBe(true);
});

test("a production check after the merge whose deploy's pass the server names reads Passed on that pass", () => {
  const passed = liveCheck({
    pass_stands: {
      answer: "yes",
      commit: COMMIT,
      deploy: deployAt(COMMIT),
      results: [PASS_AT_BASIS.id],
      roles: ["tester"],
    },
  });

  expect(rowReading(passed)).toEqual({ state: "passed", word: "Passed" });
  expect(checkStanding(passed)).toEqual({ stands: PASS_AT_BASIS, state: "passed" });
});

test("a production check after the merge whose newest result at its deploy failed reads Failed on that fail", () => {
  const fail = result({
    check_block_id: LIVE,
    created_at: "2026-11-14T01:10:00Z",
    failed: 1,
    id: "fail-deploy",
    verdict: "fail",
  });
  const failed = liveCheck({
    pass_stands: {
      answer: "no",
      commit: COMMIT,
      deploy: deployAt(COMMIT),
      reason: "fail",
      results: [fail.id],
    },
    results: [fail, PASS_AT_BASIS],
  });

  expect(rowReading(failed)).toEqual({ state: "failed", word: "Failed" });
  expect(checkStanding(failed)).toEqual({ stands: fail, state: "failed" });
});

test("a graph check on the default branch whose program is not named reads Cannot tell over a pass at its basis", () => {
  const unnamed = liveCheck({
    pass_stands: { answer: "unknown", reason: "program_not_named" },
    subject: "graph",
    when: "default_branch",
  });

  expect(rowReading(unnamed)).toEqual({ state: "unmarked", word: "Cannot tell" });
});

test("a production check after the merge whose deploy does not hold the merge reads Cannot tell over a pass at its basis", () => {
  const unmerged = liveCheck({ pass_stands: { answer: "unknown", reason: "deploy_not_merged" } });

  expect(rowReading(unmerged)).toEqual({ state: "unmarked", word: "Cannot tell" });
});

test("a production check after the merge whose deploy could not be read reads Cannot tell over a pass at its basis", () => {
  const unread = liveCheck({ pass_stands: { answer: "unknown", reason: "read_failed" } });

  expect(rowReading(unread)).toEqual({ state: "unmarked", word: "Cannot tell" });
});

// A check of production before the merge reads no deploy, so its pass at the pull request's
// head is read at its basis as every other check's is.
test("a production check before the merge reads Passed on a pass at its head", () => {
  const beforeMerge = liveCheck({
    compared_with: [{ commit: COMMIT, kind: "pull_request", pull_request: 7 }],
    pass_stands: { answer: "yes" },
    when: "before_merge",
  });

  expect(rowReading(beforeMerge)).toEqual({ state: "passed", word: "Passed" });
  expect(checkStanding(beforeMerge)).toEqual({ stands: PASS_AT_BASIS, state: "passed" });
});

// A check of the code after the merge reads no deploy: the server answers unknown over a pass
// and a fail at one commit (`pass_and_fail`), and the newest of them at the basis decides.
test("a code check after the merge with a pass and then a fail at its basis reads Failed, as before", () => {
  const fail = result({
    check_block_id: LIVE,
    created_at: "2026-11-14T01:10:00Z",
    failed: 1,
    id: "fail-basis",
    verdict: "fail",
  });
  const code = liveCheck({
    pass_stands: {
      answer: "unknown",
      commit: COMMIT,
      reason: "pass_and_fail",
      results: [PASS_AT_BASIS.id, fail.id],
    },
    results: [fail, PASS_AT_BASIS],
    subject: undefined,
  });

  expect(rowReading(code)).toEqual({ state: "failed", word: "Failed" });
  expect(checkStanding(code)).toEqual({ stands: fail, state: "failed" });
});

test("a production check after the merge with no deploy and no result reads Cannot tell", () => {
  const unrun = liveCheck({ pass_stands: { answer: "unknown", reason: "no_deploy" }, results: [] });

  expect(rowReading(unrun)).toEqual({ state: "unmarked", word: "Cannot tell" });
});

test("a production check after the merge the server says has no result reads No result", () => {
  const unrun = liveCheck({ pass_stands: { answer: "no", reason: "no_result" }, results: [] });

  expect(rowReading(unrun)).toEqual({ state: "no-result", word: "No result" });
});

test("a code check after the merge with no result reads No result", () => {
  const unrun = liveCheck({
    pass_stands: { answer: "no", reason: "no_result" },
    results: [],
    subject: undefined,
  });

  expect(rowReading(unrun)).toEqual({ state: "no-result", word: "No result" });
});

// A payload with no `pass_stands`, from a server that does not send it, says nothing of the
// deploy, so the page says it could not read the check.
test("a production check after the merge whose answer holds no pass_stands reads Could not read", () => {
  const unanswered = liveCheck({ pass_stands: undefined });

  expect(rowReading(unanswered)).toEqual({ state: "unreadable", word: "Could not read" });
});

// A result the server marks `not_counted` stands for nothing: its check's current deploy does
// not run its commit, or it was recorded before that deploy, and the server's `standingCommit`
// gives it no standing, as it gives none to a result over moved words. So, like such a result,
// it names no commit for the page (`namedCommit`): a newer fail at a commit the deploy does not
// run leaves the commit the page names at the deploy's pass, where the check's row reads Passed
// and its statement reads Met.
test("a result the server does not count names no commit for the page", () => {
  const uncountedFail = result({
    check_block_id: LIVE,
    commit_sha: OTHER_COMMIT,
    commit_stands: { answer: "other_commit" },
    created_at: "2026-11-14T02:00:00Z",
    failed: 1,
    id: "fail-elsewhere",
    not_counted:
      "it names a commit that the current deploy of the program this check reads does not run",
    verdict: "fail",
  });
  const live = liveCheck({
    pass_stands: {
      answer: "yes",
      commit: COMMIT,
      deploy: deployAt(COMMIT),
      results: [PASS_AT_BASIS.id],
      roles: ["tester"],
    },
    results: [uncountedFail, PASS_AT_BASIS],
  });

  expect(checkStanding(live)).toEqual({ stands: PASS_AT_BASIS, state: "passed" });
  expect(namedCommit([live])).toBe(COMMIT);
  expect(statementState(atNamedCommit([live]))).toBe("met");
});
