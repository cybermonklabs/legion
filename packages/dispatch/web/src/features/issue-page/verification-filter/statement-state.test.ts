import { expect, test } from "bun:test";

import type { Artifact, CheckBlock, IssueDetails } from "../../../api/types";
import { buildIssueTree, type TreeNode } from "../issue-tree";
import { NODE_STATE_WORDS, RESULT_STANDS_WORDS } from "../page-words";
import { filterSignals, presetView } from "../presets";
import { blockNodeId } from "../tree-drafts";
import { treeRows } from "../tree-fold";
import { treeAnswer, treeNode, treeTuple } from "../tree-read/tree-fixtures";
import type { TreeAnswerNode } from "../tree-read/tree-read";
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
  ISSUE_KEY,
  node,
  OTHER_COMMIT,
  result,
  SPEC_SLUG,
  statementNode,
  VERIFICATION_SLUG,
  verificationView,
} from "./verification-fixtures";

/**
 * How the Verification filter reads the issue's results: the one commit the page names, how
 * each statement stands at it, and how many statements carry a check. Each row below is one of
 * the ways a statement can stand, read over the tree the filter leaves and the results read the
 * page already made.
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

/** A check that passed at the commit the page names, which is the basis it compares with, so
 *  the server reads its pass there (`at_basis`). */
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
// Unknown". The commit is the one the server names for the check - the basis it compares with
// (`compared_with`) - and the server reads this pass at another commit (`other_commit`), so no
// pass of the check stands (`pass_stands` is `no`) and the check reads Out of date, as the
// pass's own fact does.
test("a pass recorded against another commit reads Out of date and leaves its statement Unknown", () => {
  const elsewhere = check({
    block_id: "c-two",
    pass_stands: { answer: "no" },
    results: [
      result({
        check_block_id: "c-two",
        commit_sha: OTHER_COMMIT,
        commit_stands: { answer: "other_commit" },
        id: "result-two",
      }),
    ],
  });
  const blocks = [passing("c-one"), elsewhere];
  const statement = statementNode("r1", "The page draws the checks.", blocks.map(checkNode));
  const read = reading([statement], blocks);

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["passed", "older"]);
  expect(read.statements[0]?.checks.map((held) => held.atBasis)).toEqual([true, false]);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
  expect(NODE_STATE_WORDS.check.older).toBe("Out of date");
});

// A statement whose only check passed at another commit than the one its check compares with:
// the page names the commit of the one result it holds, and the check still reads as the server
// reads that pass (`other_commit`), since the commit the page names decides no check's state.
test("a pass at another commit alone names that commit and leaves the statement Unknown", () => {
  const elsewhere = check({
    block_id: "c-one",
    pass_stands: { answer: "no" },
    results: [
      result({
        commit_sha: OTHER_COMMIT,
        commit_stands: { answer: "other_commit" },
        id: "result-one",
      }),
    ],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(elsewhere)])],
    [elsewhere]
  );

  expect(read.commit).toBe(OTHER_COMMIT);
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["older"]);
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
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

// R17's "Unknown otherwise": a check with no result leaves its statement Unknown, and the
// statement is covered all the same, since a check serves it.
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

// The server writes its times to the microsecond, and `Date.parse` reads them to the
// millisecond: two results recorded inside one millisecond are still read newest first by the
// server's own time, and not left in the order the read happened to send them in.
test("of two results recorded in one millisecond, the later microsecond is the newest", () => {
  const rerun = check({
    block_id: "c-one",
    results: [
      result({
        commit_sha: OTHER_COMMIT,
        created_at: "2026-11-05T10:00:00.000100Z",
        failed: 1,
        id: "fail-first",
        verdict: "fail",
      }),
      result({ commit_sha: COMMIT, created_at: "2026-11-05T10:00:00.000900Z", id: "pass-later" }),
    ],
  });

  expect(namedCommit([rerun])).toBe(COMMIT);
});

// A result whose time the page cannot read is older than every result whose time it can, wherever
// it arrives, so it never names the commit over a result the page can place; and it throws nothing.
test("a result whose time is not a timestamp is the oldest, wherever it arrives", () => {
  const unread = result({ commit_sha: OTHER_COMMIT, created_at: "not a time", id: "unread" });
  const placed = result({ created_at: "2026-11-01T00:00:00Z", id: "placed" });

  expect(namedCommit([check({ block_id: "c-one", results: [unread, placed] })])).toBe(COMMIT);
  expect(namedCommit([check({ block_id: "c-one", results: [placed, unread] })])).toBe(COMMIT);
  expect(namedCommit([check({ block_id: "c-one", results: [unread] })])).toBe(OTHER_COMMIT);
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

// R17: "Not met where one failed there". The commit is the one the server names for the check:
// it compares with the heads of two pull requests, and the server reads both results at its
// basis (`at_basis`), the older pass at one head and the newer fail at the other, which is the
// commit the page names too. The server's `pass_stands` says yes for any pass at a basis,
// whatever later fails exist, so a check with an older pass at another commit and a fail at the
// commit the page names read "Out of date" and left its statement Unknown. The fail at the
// basis is what the page reads.
const regressed = check({
  block_id: "c-one",
  compared_with: [
    { commit: COMMIT, kind: "pull_request", pull_request: 7 },
    { commit: OTHER_COMMIT, kind: "pull_request", pull_request: 8 },
  ],
  pass_stands: { answer: "yes" },
  results: [
    result({
      commit_stands: { answer: "at_basis" },
      created_at: "2026-11-15T09:00:00Z",
      id: "fail-here",
      verdict: "fail",
    }),
    result({
      commit_sha: OTHER_COMMIT,
      commit_stands: { answer: "at_basis" },
      created_at: "2026-11-13T09:00:00Z",
      id: "pass-elsewhere",
    }),
  ],
});

test("a fail at the commit the page names reads Failed whatever older pass still stands", () => {
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(regressed)])],
    [regressed]
  );

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks[0]).toMatchObject({
    atBasis: true,
    standsOn: "fail-here",
    state: "failed",
  });
  expect(read.statements.map((held) => held.state)).toEqual(["not_met"]);
});

/** The issue's tree as `buildIssueTree` draws it from its reads: one statement of the spec,
 *  which every check of `checks` serves, and the results read answering `checks`. */
function issueTree(checks: readonly CheckBlock[]): TreeNode {
  const document = (id: string, slug: string, name: string, primary: boolean): Artifact => ({
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    id,
    issue_key: ISSUE_KEY,
    kind: "doc",
    name,
    primary,
    project: "CORE",
    ref_key: `artifact:${id}`,
    session_id: null,
    slug,
    versions: [],
  });
  const spec = document("artifact-spec", SPEC_SLUG, "spec.md", true);
  const verification = document(
    "artifact-verification",
    VERIFICATION_SLUG,
    "verification.md",
    false
  );
  const statement = `dispatch://${ISSUE_KEY}/${SPEC_SLUG}#b-r1`;
  // A node of the tree read's answer: a typed block of `document`, at `position` in it.
  const answered = (
    id: string,
    supertag: string,
    parent: string,
    document: Artifact,
    position: number,
    text: string
  ): TreeAnswerNode =>
    treeNode(id, supertag, parent, {
      document: { artifact_id: document.id, position, slug: document.slug },
      text,
    });
  const issue: IssueDetails = {
    advances: [],
    advances_revision: 0,
    artifacts: [spec, verification],
    assignee: null,
    blocked_by: [],
    children: [],
    claim: null,
    closed_at: null,
    components: { ids: [], inherited_from: null, mode: "inherit", reason: null, unknown: [] },
    created_at: "2026-11-05T00:00:00Z",
    created_by: { id: "alice", kind: "user" },
    decisions_artifact_id: "",
    design_gates: [],
    external_links: [],
    holder_state: null,
    key: ISSUE_KEY,
    labels: [],
    last_seen: null,
    last_seq: 1,
    number: 1,
    open_asks: [],
    parent: null,
    plan_artifact_id: "",
    primary_artifact_id: spec.id,
    priority: null,
    progress: { children: null, tasks: null },
    project: "CORE",
    rank: "U",
    referenced_by_count: 0,
    repo: null,
    resolved_repo: null,
    route: null,
    route_holder: null,
    route_status: null,
    route_status_reason: null,
    status: "todo",
    takeable_at: null,
    title: "The issue page draws one ordered body",
    updated_at: "2026-11-05T00:00:00Z",
    verification_artifact_id: verification.id,
  };
  return buildIssueTree({
    coverage: {
      gaps: [],
      has_plan: false,
      issue: ISSUE_KEY,
      requirements: [
        {
          block_id: "r1",
          checks: checks.length,
          gaps: [],
          plan_tasks: 0,
          ref: statement,
          text: "The page draws it.",
        },
      ],
      spec_artifact_id: spec.id,
    },
    // The tree read's answer: the statement under the issue, and each check under it, which it
    // serves.
    tree: treeAnswer(
      [
        answered("r1", "requirement", ISSUE_KEY, spec, 0, "The page draws it."),
        ...checks.map((one, at) =>
          answered(one.block_id, "check", "r1", verification, at, one.text)
        ),
        // Each result under its check, as the read answers it, its observed text read as the
        // results read reads it.
        ...checks.flatMap((one) =>
          (one.results ?? []).map((held) =>
            treeNode(held.id, "result", one.block_id, {
              made_at: held.created_at,
              text: held.observed,
              ...(held.observed_tree === undefined ? {} : { text_tree: held.observed_tree }),
            })
          )
        ),
      ],
      checks.map((one) => treeTuple("serves", one.block_id, "r1"))
    ),
    issue,
    results: { checks: [...checks], issue: ISSUE_KEY, verification_artifact_id: verification.id },
  });
}

// The tree read a check's state from `checkState` alone while this body turned it over at the
// commit the page names, so one check read two states. The regressed check above read Passed on
// its row, and the filter row counted nothing failing, while this body read it Failed; a fail
// that stands at another commit read Failed on the row and in the count, and Out of date here.
// The tree's node holds the one reading this body draws (`checkStanding`), which is the server's.
test("a check reads one state on its row, in the fold, in the filter row and in the body", () => {
  const statement = blockNodeId(SPEC_SLUG, "r1");
  const checkId = (blockId: string) => blockNodeId(VERIFICATION_SLUG, blockId);
  const placed = (root: TreeNode, blockId: string) =>
    root.children
      .find((child) => child.id === statement)
      ?.children.find((child) => child.id === checkId(blockId));
  const bodyStates = (root: TreeNode, checks: readonly CheckBlock[]) =>
    verificationReading({
      checks,
      view: presetView(root, "verification", "").root,
    }).statements[0]?.checks.map((held) => held.state);

  const tree = issueTree([regressed]);
  expect(placed(tree, "c-one")?.state).toBe("failed");
  expect(bodyStates(tree, [regressed])).toEqual(["failed"]);
  // Every branch is drawn open (R11), the statement's down to its check.
  const rows = treeRows(tree);
  expect(rows.find((row) => row.kind === "node" && row.node.id === statement)).toMatchObject({
    expanded: true,
  });
  expect(rows.some((row) => row.kind === "node" && row.node.id === checkId("c-one"))).toBe(true);
  expect(filterSignals(tree, "").verification).toEqual({
    count: 1,
    flag: { count: 1, word: "failing" },
  });

  // The reverse: a fail that stands at the commit its own check compares with, which is not the
  // commit the page names, since another check's pass is newer. The server reads it at its
  // check's basis (`at_basis`), so it is a fail on its row, in the body and in the count alike,
  // as its own fact reads it.
  const passedHere = check({ block_id: "c-one", results: [result({ id: "pass-here" })] });
  const failedElsewhere = check({
    block_id: "c-two",
    compared_with: [{ commit: OTHER_COMMIT, kind: "named" }],
    pass_stands: { answer: "no" },
    results: [
      result({
        check_block_id: "c-two",
        commit_sha: OTHER_COMMIT,
        created_at: "2026-11-13T09:00:00Z",
        failed: 1,
        id: "fail-elsewhere",
        verdict: "fail",
      }),
    ],
  });
  const reverse = issueTree([passedHere, failedElsewhere]);
  expect(placed(reverse, "c-two")?.state).toBe("failed");
  expect(bodyStates(reverse, [passedHere, failedElsewhere])).toEqual(["passed", "failed"]);
  expect(filterSignals(reverse, "").verification).toEqual({
    count: 2,
    flag: { count: 1, word: "failing" },
  });
});

// The server reads each result at the commit it names for that result's own
// check. A `before_merge` check compares with the pull request's head, and an `after_merge` one
// with the branch the pull request merged into, so a newer result of the second at another
// commit says nothing of the first. The page read every check against the one commit it names,
// the newest result's, so a pass at the head read Out of date on its row while its own fact read
// "Up to date". The commit each check is read at is the one the server names for it, so the row
// reads what the fact reads.
test("a before_merge pass at the head reads Passed beside another check's newer result elsewhere", () => {
  const atHead = check({
    block_id: "c-one",
    compared_with: [{ commit: COMMIT, kind: "pull_request", pull_request: 7 }],
    pass_stands: { answer: "yes" },
    results: [result({ commit_stands: { answer: "at_basis" }, id: "pass-at-head" })],
    when: "before_merge",
  });
  const merged = check({
    block_id: "c-two",
    compared_with: [{ branch: "main", kind: "branch" }],
    pass_stands: { answer: "yes" },
    results: [
      result({
        check_block_id: "c-two",
        commit_sha: OTHER_COMMIT,
        commit_stands: { answer: "at_basis" },
        created_at: "2026-11-15T09:00:00Z",
        id: "pass-on-main",
      }),
    ],
    when: "after_merge",
  });
  const checks = [atHead, merged];
  const tree = issueTree(checks);
  const row = tree.children
    .find((child) => child.id === blockNodeId(SPEC_SLUG, "r1"))
    ?.children.find((child) => child.id === blockNodeId(VERIFICATION_SLUG, "c-one"));
  const read = verificationReading({ checks, view: presetView(tree, "verification", "").root });

  // The page names the newest result's commit, and reads no check against it.
  expect(read.commit).toBe(OTHER_COMMIT);
  expect(row?.state).toBe("passed");
  expect(read.statements[0]?.checks.map((held) => held.state)).toEqual(["passed", "passed"]);
  expect(read.statements.map((held) => held.state)).toEqual(["met"]);
  expect(filterSignals(tree, "").verification).toEqual({ count: 2 });
  // The pass's own fact reads the server's answer, which is the row's.
  const stands = row?.children
    .find((child) => child.id === "pass-at-head")
    ?.facts.find((fact) => fact.name === "stands");
  expect(stands?.value).toBe(RESULT_STANDS_WORDS.at_basis);
  expect(RESULT_STANDS_WORDS.at_basis).toBe("Up to date");
});

// A check fails, its author fixes the check's words, and nobody has run it again. A fail
// recorded against words the check no longer has stands for nothing, as `checkState` and the
// server's `standingCommit` read it, so it is no fail at the commit the page names either: the
// check is Out of date and its statement Unknown, not Not met on evidence about words the check
// no longer has. Such a result names no commit either, so another check's result names it here.
test("a fail at the commit the page names whose check's words moved reads Out of date", () => {
  const moved = check({
    block_id: "c-one",
    pass_stands: { answer: "no" },
    results: [result({ check_words_moved: true, failed: 1, id: "fail-moved", verdict: "fail" })],
  });
  const blocks = [moved, passing("c-two")];
  const statement = statementNode("r1", "The page draws it.", blocks.map(checkNode));
  const read = reading([statement], blocks);

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks[0]?.state).toBe("older");
  expect(NODE_STATE_WORDS.check.older).toBe("Out of date");
  expect(read.statements.map((held) => held.state)).toEqual(["unknown"]);
});

// The server's `standingCommit` gives a result recorded against words its check no longer has
// no standing, and the page's own reading says it stands for nothing. The page still named its
// commit where it was the newest result, and read every result over the check's words now
// against a commit that only evidence about other words had run at.
test("a result over words its check no longer has names no commit, however new it is", () => {
  const reworded = check({
    block_id: "c-one",
    results: [
      result({
        check_words_moved: true,
        commit_sha: OTHER_COMMIT,
        created_at: "2026-11-15T09:00:00Z",
        id: "pass-moved",
      }),
      result({ created_at: "2026-11-14T09:00:00Z", id: "pass-current" }),
    ],
  });
  const movedOnly = check({
    block_id: "c-one",
    results: [result({ check_words_moved: true, id: "pass-moved" })],
  });

  expect(namedCommit([reworded])).toBe(COMMIT);
  expect(namedCommit([movedOnly])).toBeUndefined();
});

// A fail recorded against words the check no longer has is no result at the check's basis, the
// commit the server names for it (here the commit the page names too), so an older pass over the
// check's words now at that commit is what the check reads.
test("a fail over moved words at the named commit does not read Failed over a pass there", () => {
  const reworded = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [
      result({
        check_words_moved: true,
        created_at: "2026-11-15T09:00:00Z",
        failed: 1,
        id: "fail-moved",
        verdict: "fail",
      }),
      result({ created_at: "2026-11-14T09:00:00Z", id: "pass-current" }),
    ],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(reworded)])],
    [reworded]
  );

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks[0]).toMatchObject({
    atBasis: true,
    standsOn: "pass-current",
    state: "passed",
  });
  expect(read.statements.map((held) => held.state)).toEqual(["met"]);
});

// The other side of the same rule: the result at the named commit was the newest that stands
// there, whatever words it was recorded against, so a newer pass over words the check no longer
// has hid a fail over its words now at that commit, and the check read Out of date where it
// failed. The newest result over the check's words now is the fail, at the check's basis.
test("a newer result over moved words at the named commit hides no fail over the words now", () => {
  const reworded = check({
    block_id: "c-one",
    pass_stands: { answer: "no" },
    results: [
      result({ check_words_moved: true, created_at: "2026-11-15T09:00:00Z", id: "pass-moved" }),
      result({
        created_at: "2026-11-14T09:00:00Z",
        failed: 1,
        id: "fail-current",
        verdict: "fail",
      }),
    ],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(reworded)])],
    [reworded]
  );

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks[0]).toMatchObject({
    atBasis: true,
    standsOn: "fail-current",
    state: "failed",
  });
  expect(read.statements.map((held) => held.state)).toEqual(["not_met"]);
});

// A passed check stands on its newest pass, which the body draws as the evidence, and the page
// took a newer pass over words the check no longer has for it: one at another commit read the
// check Out of date, although a pass over its words now stands at the check's basis.
test("a passed check stands on its newest pass over the words it has now", () => {
  const reworded = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [
      result({
        check_words_moved: true,
        commit_sha: OTHER_COMMIT,
        created_at: "2026-11-15T09:00:00Z",
        id: "pass-moved",
      }),
      result({ created_at: "2026-11-14T09:00:00Z", id: "pass-current" }),
    ],
  });
  const read = reading(
    [statementNode("r1", "The page draws it.", [checkNode(reworded)])],
    [reworded]
  );

  expect(read.commit).toBe(COMMIT);
  expect(read.statements[0]?.checks[0]).toMatchObject({
    atBasis: true,
    standsOn: "pass-current",
    state: "passed",
  });
  expect(read.statements.map((held) => held.state)).toEqual(["met"]);
});

// The body draws the newest result of each check and folds the rest, so the model
// hands it the rest apart, newest first, and names the result the state stands on. Each result
// ran at the commit the server names for the check, its basis (`at_basis`), so the newest pass
// is what the passed check stands on.
test("a check's other results are handed apart from its latest, newest first", () => {
  const atBasis = { answer: "at_basis" } as const;
  const busy = check({
    block_id: "c-one",
    pass_stands: { answer: "yes" },
    results: [
      result({ commit_stands: atBasis, created_at: "2026-11-12T09:00:00Z", id: "oldest" }),
      result({ commit_stands: atBasis, created_at: "2026-11-14T09:00:00Z", id: "newest" }),
      result({ commit_stands: atBasis, created_at: "2026-11-13T09:00:00Z", id: "middle" }),
    ],
  });
  const read = reading([statementNode("r1", "The page draws it.", [checkNode(busy)])], [busy]);
  const drawn = read.statements[0]?.checks[0];

  expect(drawn?.latest?.id).toBe("newest");
  expect(drawn?.earlier.map((held) => held.id)).toEqual(["middle", "oldest"]);
  expect(drawn?.standsOn).toBe("newest");
});

// R10: a check of a descendant issue counts. The coverage read counts it for the statement and
// this issue's tree holds no node for it, so the statement is covered and draws no check. Its
// state is read from its checks' own states, each at the commit the server names for that
// check, and it holds none here, so it is Unknown.
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

// The page draws a commit as the picture draws it, in the short form git itself writes. The
// commit is the one the page names for its results (R17), which decides no check's state: each
// check is read at the commit the server names for it, its basis.
test("a commit is drawn in the short form the picture draws", () => {
  expect(shortCommit(COMMIT)).toBe(COMMIT.slice(0, COMMIT_CHARACTERS));
  expect(shortCommit(COMMIT)).toHaveLength(10);
  expect(shortCommit("abc")).toBe("abc");
});
