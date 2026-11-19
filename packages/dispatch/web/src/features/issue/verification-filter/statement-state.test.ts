import { expect, test } from "bun:test";

import type { CheckBlock } from "../../../api/types";
import type { TreeNode } from "../issue-tree";
import { NODE_STATE_WORDS } from "../page-words";
import {
  COMMIT_CHARACTERS,
  namedCommit,
  shortCommit,
  statementCounts,
  statementStandings,
  type VerificationReading,
  verificationReading,
} from "./statement-state";
import {
  COMMIT,
  check,
  checkNode,
  node,
  OTHER_COMMIT,
  result,
  statementNode,
  verificationView,
} from "./verification-fixtures";

/**
 * How the Verification filter reads the issue's results (dispatch://INFRA-1196 R17): the one
 * commit the page names, how each statement stands at it, and how many statements carry a
 * check. Each row below is one of the ways a statement can stand, read over the tree the filter
 * leaves and the results read the page already made.
 *
 * What the body draws of this reading is `VerificationFilterBody.drawn.test.tsx`.
 */

/** The reading over the statements given, as the body is handed it. */
function reading(
  statements: readonly TreeNode[],
  checks: readonly CheckBlock[],
  ...loose: readonly TreeNode[]
): VerificationReading {
  return verificationReading({ checks, view: verificationView([...statements, ...loose]) });
}

/** A check that passed at the commit the page names. */
function passing(blockId: string): CheckBlock {
  return check({
    block_id: blockId,
    pass_stands: { answer: "yes" },
    results: [result({ check_block_id: blockId, id: `result-${blockId}` })],
  });
}

// R17: "a statement reads Met where every check of it passed at that commit".
test("a statement whose every check passed at the commit the page names reads Met", () => {
  const blocks = [passing("c-one"), passing("c-two")];
  const statement = statementNode("r1", "The page draws the checks.", blocks.map(checkNode));
  const read = reading([statement], blocks);

  expect(read.commit).toBe(COMMIT);
  expect(read.statements.map((held) => held.state)).toEqual(["met"]);
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["passed", "passed"]);
  expect(read.covered).toBe(1);
  expect(statementCounts(read.statements)).toEqual({ met: 1, not_met: 0, unknown: 0 });
});

// R17: "Not met where one failed there". One failure settles it, whatever the other checks say.
test("a statement one of whose checks failed at that commit reads Not met", () => {
  const failed = check({
    block_id: "c-two",
    pass_stands: { answer: "no" },
    results: [result({ check_block_id: "c-two", failed: 1, id: "result-two", verdict: "fail" })],
  });
  const blocks = [passing("c-one"), failed];
  const statement = statementNode("r1", "The page draws the checks.", blocks.map(checkNode));
  const read = reading([statement], blocks);

  expect(read.statements.map((held) => held.state)).toEqual(["not_met"]);
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["passed", "failed"]);
  expect(statementCounts(read.statements)).toEqual({ met: 0, not_met: 1, unknown: 0 });
});

// R17: "a result recorded against another commit reads Out of date and makes its statement
// Unknown". The server still answers that the pass stands - it compares a result with the basis
// its own check carries - and the page reads every result of the issue against the one commit
// it names.
test("a pass recorded against another commit reads Out of date and leaves its statement Unknown", () => {
  const elsewhere = check({
    block_id: "c-two",
    pass_stands: { answer: "yes" },
    results: [result({ check_block_id: "c-two", commit_sha: OTHER_COMMIT, id: "result-two" })],
  });
  const blocks = [passing("c-one"), elsewhere];
  const statement = statementNode("r1", "The page draws the checks.", blocks.map(checkNode));
  const read = reading([statement], blocks);

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["passed", "older"]);
  expect(read.statements[0]?.checks.map((held) => held.atNamedCommit)).toEqual([true, false]);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
  expect(NODE_STATE_WORDS.check.older).toBe("Out of date");
});

// A statement whose only check passed at another commit has no result at the commit the page
// names at all, so the page names the commit of the result it does hold.
test("a pass at another commit alone names that commit and leaves the statement Unknown", () => {
  const elsewhere = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [result({ commit_sha: OTHER_COMMIT, id: "result-one" })],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(elsewhere)])],
    [elsewhere]
  );

  expect(read.commit).toBe(OTHER_COMMIT);
  // The one result the issue holds is the commit the page names, so the pass is at it.
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["passed"]);
  expect(read.statements.map((held) => held.state)).toEqual(["met"]);
});

// A withdrawn result is no evidence (`results_comparison.go`, `tree-drafts.ts`), so it neither
// names the commit the rest of the issue is read against nor leaves its check reading Failed.
test("a check whose every result was withdrawn reads Retracted and names no commit", () => {
  const withdrawn = check({
    block_id: "c-two",
    pass_stands: { answer: "no" },
    results: [
      result({
        check_block_id: "c-two",
        commit_sha: OTHER_COMMIT,
        created_at: "2026-11-15T09:00:00Z",
        id: "result-withdrawn",
        retracted: {
          actor: { id: "tester", kind: "session" },
          created_at: "2026-11-15T10:00:00Z",
          reason: "Run was wrong.",
        },
      }),
    ],
  });
  const blocks = [passing("c-one"), withdrawn];
  const statement = statementNode("r1", "The page draws the checks.", blocks.map(checkNode));
  const read = reading([statement], blocks);

  // The newest result of the issue is the withdrawn one; the commit is the standing pass's.
  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["passed", "retracted"]);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
  expect(NODE_STATE_WORDS.check.retracted).toBe("Retracted");
  // The body still draws the withdrawn result: it is all the check has to show.
  expect(read.statements[0]?.checks[1]?.latest?.id).toBe("result-withdrawn");
});

// R17's "Unknown otherwise", and R10: the statement carries its gap, which the coverage read
// writes onto the node; a statement whose check has no result is covered all the same.
test("a check with no result reads No result, and its statement is covered and Unknown", () => {
  const unrun = check({ block_id: "c-one", pass_stands: { answer: "no" }, results: [] });
  const read = reading([statementNode("r1", "The page draws it.", [checkNode(unrun)])], [unrun]);

  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["no-result"]);
  expect(read.statements[0]?.checks[0]?.latest).toBeUndefined();
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
  expect(read.covered).toBe(1);
  expect(read.commit).toBeUndefined();
  expect(NODE_STATE_WORDS.check["no-result"]).toBe("No result");
});

// R2: "each check under every requirement it serves as context". The tree holds one node per
// placement under the one id, and both statements are covered by it.
test("a check that serves two statements is read under both, and both are covered", () => {
  const shared = passing("c-one");
  const first = statementNode("r1", "The page draws the first.", [checkNode(shared)]);
  const second = statementNode("r2", "The page draws the second.", [
    { ...checkNode(shared), transcluded: true },
  ]);
  const read = reading([first, second], [shared]);

  expect(read.statements.map((held) => held.node.id)).toEqual([first.id, second.id]);
  expect(read.statements.map((held) => held.checks[0]?.node.id)).toEqual([
    checkNode(shared).id,
    checkNode(shared).id,
  ]);
  expect(read.statements.map((held) => held.state)).toEqual(["met", "met"]);
  expect(read.covered).toBe(2);
});

// A statement no check serves is Unknown and is in no covered count: "a statement that has a
// check is not a statement that is true" cuts both ways (R17).
test("a statement no check serves is Unknown and is not covered", () => {
  const served = passing("c-one");
  const read = reading(
    [
      statementNode("r1", "The page draws it.", [checkNode(served)]),
      statementNode("r2", "Nothing proves this.", []),
    ],
    [served]
  );

  expect(read.statements.map((held) => held.state)).toEqual(["met", "unknown"]);
  expect(read.covered).toBe(1);
  expect(statementCounts(read.statements)).toEqual({ met: 1, not_met: 0, unknown: 1 });
});

// The commit is the newest result's, read from the times the results carry and not from the
// order the read answered in.
test("the commit the page names is the newest standing result's, whatever order they arrive in", () => {
  const older = check({
    block_id: "c-one",
    results: [result({ commit_sha: OTHER_COMMIT, created_at: "2026-11-10T00:00:00Z", id: "old" })],
  });
  const newer = check({
    block_id: "c-two",
    results: [
      result({
        check_block_id: "c-two",
        commit_sha: COMMIT,
        created_at: "2026-11-14T00:58:00Z",
        id: "new",
      }),
    ],
  });

  expect(namedCommit([older, newer])).toBe(COMMIT);
  expect(namedCommit([newer, older])).toBe(COMMIT);
});

// A payload with no answer over the check's results at all is the page's own blindness, and no
// verdict of the check's: it reads "Could not read" and proves nothing.
test("a check whose results the page cannot read draws Could not read and no met statement", () => {
  const unreadable = check({
    block_id: "c-one",
    pass_stands: undefined as unknown as never,
    results: [result({ id: "result-one" })],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(unreadable)])],
    [unreadable]
  );

  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["unreadable"]);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
  expect(NODE_STATE_WORDS.check.unreadable).toBe("Could not read");
});

// A commit Dispatch could not place: there is a result, and nothing can be said of how it
// stands, so neither Met nor Not met is true of its statement.
test("a check Dispatch cannot judge draws Cannot tell and leaves its statement Unknown", () => {
  const unmarked = check({
    block_id: "c-one",
    pass_stands: { answer: "unknown" },
    results: [result({ commit_stands: { answer: "unknown" }, id: "result-one" })],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(unmarked)])],
    [unmarked]
  );

  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["unmarked"]);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
  expect(NODE_STATE_WORDS.check.unmarked).toBe("Cannot tell");
});

// The latest result the body draws is the newest that stands, so a check whose newest result
// was withdrawn still shows the evidence it stands on.
test("the latest result a check draws is the newest that stands", () => {
  const taken = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [
      result({
        created_at: "2026-11-15T09:00:00Z",
        id: "result-withdrawn",
        retracted: {
          actor: { id: "tester", kind: "session" },
          created_at: "2026-11-15T10:00:00Z",
          reason: "Run was wrong.",
        },
        verdict: "fail",
      }),
      result({ created_at: "2026-11-14T00:58:00Z", id: "result-standing" }),
    ],
  });
  const read = reading([statementNode("r1", "The page draws it.", [checkNode(taken)])], [taken]);

  expect(read.statements[0]?.checks[0]?.latest?.id).toBe("result-standing");
  expect(read.statements.map((held) => held.state)).toEqual(["met"]);
});

// R17: "Not met where one failed there". The server's `pass_stands` says yes for any pass at a
// basis, whatever later fails exist, so a check with an older pass at another commit and a fail
// at the commit the page names read "Out of date" and left its statement Unknown. The fail at
// the named commit is what the page reads.
test("a fail at the commit the page names reads Failed whatever older pass still stands", () => {
  const regressed = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [
      result({ created_at: "2026-11-15T09:00:00Z", id: "fail-here", verdict: "fail" }),
      result({
        commit_sha: OTHER_COMMIT,
        created_at: "2026-11-13T09:00:00Z",
        id: "pass-elsewhere",
      }),
    ],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(regressed)])],
    [regressed]
  );

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks[0]).toMatchObject({
    atNamedCommit: true,
    standsOn: "fail-here",
    state: "failed",
  });
  expect(read.statements.map((held) => held.state)).toEqual(["not_met"]);
});

// Ruling 6: the body draws the newest result of each check and folds the rest, so the model
// hands it the rest apart, newest first, and names the result the state stands on.
test("a check's other results are handed apart from its latest, newest first", () => {
  const busy = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [
      result({ created_at: "2026-11-12T09:00:00Z", id: "oldest" }),
      result({ created_at: "2026-11-14T09:00:00Z", id: "newest" }),
      result({ created_at: "2026-11-13T09:00:00Z", id: "middle" }),
    ],
  });
  const read = reading([statementNode("r1", "The page draws it.", [checkNode(busy)])], [busy]);
  const drawn = read.statements[0]?.checks[0];

  expect(drawn?.latest?.id).toBe("newest");
  expect(drawn?.earlier.map((held) => held.id)).toEqual(["middle", "oldest"]);
  expect(drawn?.standsOn).toBe("newest");
});

// R10: a check of a descendant issue counts. The coverage read counts it for the statement and
// this issue's tree holds no node for it, so the statement is covered and draws no check.
test("a statement a child issue's check serves is covered", () => {
  const served = {
    ...statementNode("r1", "The page draws it.", []),
    counts: { checks: 2, planTasks: 0 },
  };
  const unserved = {
    ...statementNode("r2", "The page draws that.", []),
    counts: { checks: 0, planTasks: 0 },
  };
  const read = reading([served, unserved], []);

  expect(read.covered).toBe(1);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown", "unknown"]);
});

// The Spec filter's mark is this reading, over the whole tree, so one statement reads the same
// in both views.
test("every statement of the whole tree is read as the Verification filter reads it", () => {
  const passed = passing("c-one");
  const tree = node({
    children: [
      statementNode("r1", "The page draws it.", [checkNode(passed)]),
      statementNode("r2", "The page draws that.", []),
    ],
    id: "CORE-1",
    type: "issue",
  });
  const standings = statementStandings(tree, [passed]);

  expect([...standings]).toEqual([
    ["spec#b-r1", { covered: true, state: "met" }],
    ["spec#b-r2", { covered: false, state: "unknown" }],
  ]);
});

// While the results read is still out the page has read nothing of how the check stands, so it
// draws no state for it and no statement of it is Met.
test("a check the results read has not answered for draws no state", () => {
  const unread = passing("c-one");
  const read = reading([statementNode("r1", "The page draws it.", [checkNode(unread)])], []);

  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual([undefined]);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
  expect(read.covered).toBe(1);
});

// A check whose `serves` names no statement is a row of the filter all the same, and so is a
// node an address named: the body draws both rather than dropping what the filter kept.
test("the body is given the checks that serve no statement and every other row the filter kept", () => {
  const loose = passing("c-loose");
  const named = node({ id: "comment-1", text: "A comment an address named.", type: "comment" });
  const read = verificationReading({
    checks: [loose],
    view: verificationView(
      [statementNode("r1", "The page draws it.", []), checkNode(loose), named],
      [named.id]
    ),
  });

  expect(read.loose.map((held) => held.node.id)).toEqual([checkNode(loose).id]);
  expect(read.loose.map((held) => held.state)).toEqual(["passed"]);
  expect(read.others.map((held) => held.id)).toEqual(["comment-1"]);
  expect(read.covered).toBe(0);
});

// The page draws a commit as the picture draws it, in the short form git itself writes.
test("a commit is drawn in the short form the picture draws", () => {
  expect(shortCommit(COMMIT)).toBe(COMMIT.slice(0, COMMIT_CHARACTERS));
  expect(shortCommit(COMMIT)).toHaveLength(10);
  expect(shortCommit("abc")).toBe("abc");
});
