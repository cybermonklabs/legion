import { beforeAll, describe, expect, test } from "bun:test";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import {
  holdsMarker,
  marker,
  markerLines,
  type Rule,
  writeRules,
  writeStandInGh,
} from "./gh-words";

// Every gh call of lib/workflow.sh whose stderr or answer could reach what a proof prints, run
// against a stand-in gh first on PATH (gh-words.ts). Whatever answers gh in GitHub's place holds
// gh's token and writes both gh's stderr (`gh: <message> (HTTP 502)`) and its answer, so each row
// puts the marker not-a-secret-token in one of the two, runs the lib's own function (the lib
// sourced as a stage proof sources it, after lib/rig.sh), and holds what the run printed, stdout
// and stderr together with any value a caller prints, to no run of eight of the marker's
// characters. The twin: the file under $evidence/gh the lib keeps gh's words in does hold the
// marker, so the row is not blind, and the run names that file; the control: the stand-in logged
// the call the row is about.
const root = join(import.meta.dir, "..", "..", "..");
const lib = join(import.meta.dir, "workflow.sh");
const dir = temporaryDirectory("workflow-gh-words-test");
const bin = join(dir, "bin");
beforeAll(() => writeStandInGh(bin));

const says = `gh: ${marker} (HTTP 502)\n`;
const head = "0123456789abcdef0123456789abcdef01234567";
const mergeCommit = "fedcba9876543210fedcba9876543210fedcba98";
const branch = "legion/LEGION-1-smoke";

type Row = {
  name: string;
  script: string;
  rules: Rule[];
  // the file under $evidence/gh that keeps the marker, which the output names
  kept: string;
  // a piece of the call the row is about, as the stand-in logs it
  call: string;
  env?: Record<string, string>;
  // the run prints no line naming the kept file (a poll that fails quietly)
  unnamed?: boolean;
};

// The stage scripts' shell options, ERR trap and check helpers, lib/rig.sh and the lib, then the
// stand-ins of what a row's function reads besides gh.
const prelude = `set -Eeuo pipefail
check=gh-words
note() { printf '   %s\\n' "$*"; }
fail() { printf 'FAIL %s: %s\\n' "$check" "$*" >&2; exit 1; }
trap 'printf "FAIL %s: line %s exited %s: %s\\n" "$check" "$LINENO" "$?" "$BASH_COMMAND" >&2' ERR
root=$ROOT work="$RUN/work" evidence="$RUN/evidence" timeout_hook=
repo=sjawhar/legion-smoke project=S3TEST ptoken=s3test ok= pr_number=7 smoke_file=smoke/LEGION-1.md
. "$root/scripts/e2e/lib/rig.sh"
. "$LIB"
implement_app_login='example-implement[bot]' review_app_login='example-review[bot]'
daemon_state() { printf '{"issues":{"LEGION-1":{"pullRequest":{"head":"%s"}}}}\\n' "${head}"; }
`;

let runs = 0;
function run(row: Row) {
  const runDir = join(dir, `run-${++runs}`);
  const ghDir = join(runDir, "gh-stand-in");
  mkdirSync(join(runDir, "work"), { recursive: true });
  mkdirSync(join(runDir, "evidence"));
  writeRules(ghDir, row.rules);
  const result = Bun.spawnSync(["bash", "-c", prelude + row.script], {
    env: {
      PATH: `${bin}:${process.env.PATH}`,
      HOME: runDir,
      ROOT: root,
      RUN: runDir,
      LIB: lib,
      GH_DIR: ghDir,
      MERGE_WHEN_CLEAN_BOUND: "1",
      MERGE_WHEN_CLEAN_POLL: "0.02",
      ...row.env,
    },
  });
  const keptPath = join(runDir, "evidence", "gh", row.kept);
  return {
    code: result.exitCode,
    output: result.stdout.toString() + result.stderr.toString(),
    calls: readFileSync(join(ghDir, "calls"), "utf8").split("\n").filter(Boolean),
    keptPath,
    kept: existsSync(keptPath) ? readFileSync(keptPath, "utf8") : "",
  };
}

// The pull request's merge record as merge_when_clean_read's --jq filter prints it.
const record = (
  fields: Partial<
    Record<
      "state" | "merge" | "head" | "ref" | "commit" | "at" | "method" | "position" | "queue",
      string
    >
  >
) =>
  `${[
    fields.state ?? "OPEN",
    fields.merge ?? "BLOCKED",
    fields.head ?? head,
    fields.ref ?? branch,
    fields.commit ?? "",
    fields.at ?? "",
    fields.method ?? "",
    fields.position ?? "",
    fields.queue ?? "",
  ].join("\n")}\n`;
const graphql = "* api graphql *";
const rollup = "* pr view 7 --json statusCheckRollup *";
const noChecks: Rule = { match: rollup, out: "[]\n" };
const branchDelete = `* api -X DELETE repos/sjawhar/legion-smoke/git/refs/heads/* *`;

// clean_smoke_main's calls, each answering as GitHub does when the cleanup goes through.
const cleanup: Record<string, Rule> = {
  tree: {
    match: "* api repos/*/git/trees/main?recursive=1 *",
    out: ".legion/LEGION-1/plan.json\n",
  },
  main: { match: "* api repos/*/git/ref/heads/main *", out: "0123abc\n" },
  refs: { match: "* api repos/*/git/matching-refs/heads/* *", out: "" },
  create: { match: "* api repos/*/git/refs -f *", out: "{}\n" },
  sha: { match: "* api repos/*/contents/*?ref=* *", out: "blob1\n" },
  remove: { match: "* api -X DELETE repos/*/contents/* *", out: "{}\n" },
  pr: { match: "* pr create *", out: "https://github.com/sjawhar/legion-smoke/pull/5\n" },
  state: { match: "* pr view * --json mergeStateStatus *", out: "CLEAN\n" },
  merge: { match: "* pr merge *" },
};
const cleanupRules = (first: Rule) => [first, ...Object.values(cleanup)];
// close_smoke_cleanup's calls: the run's cleanup pull request 5 open on its branch, which remains.
const leftover: Record<string, Rule> = {
  list: {
    match: "* pr list --state open --limit 1000 --json number,url,headRefName *",
    out: "5 https://github.com/sjawhar/legion-smoke/pull/5\n",
  },
  close: { match: "* pr close 5 *" },
  refs: {
    match: "* api repos/*/git/matching-refs/heads/* *",
    out: "refs/heads/proof/clean-main-s3test\n",
  },
  remove: { match: branchDelete },
};
const leftoverRules = (first: Rule) => [first, ...Object.values(leftover)];
const closeCleanup = "smoke_cleanup_branch=proof/clean-main-s3test\nclose_smoke_cleanup || true\n";
const runList = "* pr list --state open --limit 1000 --json number,headRefName *";
const bot: Record<string, Rule> = {
  head: { match: "* api repos/*/pulls/7 --jq .head.sha *", out: `${head}\n` },
  post: {
    match: "* api --method POST repos/*/pulls/7/comments *",
    out: "PRRC_1 sjawhar-agent[bot] Bot\n",
  },
};

const rows: Row[] = [
  {
    name: "require_proof_human: gh's stderr",
    script: "require_proof_human",
    rules: [{ match: graphql, code: 4, err: says }],
    kept: "proof-human.err",
    call: "api graphql -f query={viewer{login}}",
  },
  {
    name: "require_proof_human: the login gh answered",
    script: "require_proof_human",
    rules: [{ match: graphql, out: `${marker}\n` }],
    kept: "proof-human.answer",
    call: "api graphql -f query={viewer{login}}",
  },
  {
    name: "pull_request_product_files: gh's stderr",
    script: "pull_request_product_files || true",
    rules: [{ match: "* api --paginate repos/*/pulls/7/files *", code: 1, err: says }],
    kept: "pull-request-files.err",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/files",
  },
  {
    name: "pull_request_legion_only_commits: the listing's stderr",
    script: "pull_request_legion_only_commits || true",
    rules: [{ match: "* api --paginate repos/*/pulls/7/commits *", code: 1, err: says }],
    kept: "pull-request-commits.err",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/commits",
  },
  {
    name: "pull_request_legion_only_commits: a commit read's stderr",
    script: "pull_request_legion_only_commits || true",
    rules: [
      { match: "* api --paginate repos/*/pulls/7/commits *", out: `${head}\n` },
      { match: `* api repos/*/commits/${head} *`, code: 1, err: says },
    ],
    kept: "pull-request-commit-files.err",
    call: `api repos/sjawhar/legion-smoke/commits/${head}`,
  },
  {
    name: "pull_request_legion_only_commits: a commit gh listed, printed as a handoff-only push",
    script: "pull_request_legion_only_commits",
    rules: [
      { match: "* api --paginate repos/*/pulls/7/commits *", out: `${marker}\n` },
      { match: "* api repos/*/commits/* *", out: ".legion/LEGION-1/plan.json\n" },
    ],
    kept: "pull-request-commits.answer",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/commits",
  },
  {
    name: "reviewer_requested_changes_from: gh's stderr",
    script: "reviewer_requested_changes_from 1 session || true",
    rules: [{ match: "* api --paginate repos/*/pulls/7/reviews *", code: 1, err: says }],
    kept: "reviews.err",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/reviews --jq .[]",
  },
  {
    name: "round_correction_pushed: gh's stderr",
    script: "round_correction_pushed 1 || true",
    rules: [{ match: "* api --paginate repos/*/pulls/7/files *", code: 1, err: says }],
    kept: "pull-request-patches.err",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/files",
  },
  {
    name: "review_app_reviews: gh's stderr",
    script: "reviewer_commented || true",
    rules: [{ match: "* api --paginate repos/*/pulls/7/reviews *", code: 1, err: says }],
    kept: "review-app-reviews.err",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/reviews",
  },
  {
    name: "reviewer_approved_head: the head read's stderr",
    script: "reviewer_approved_head || true",
    rules: [{ match: "* api repos/*/pulls/7 --jq .head.sha *", code: 1, err: says }],
    kept: "pull-request.err",
    call: "api repos/sjawhar/legion-smoke/pulls/7 --jq .head.sha",
  },
  {
    name: "smoke_main_leftovers: gh's stderr",
    script: "smoke_main_leftovers || true",
    rules: [{ ...cleanup.tree, code: 1, out: undefined, err: says }],
    kept: "smoke-main-tree.err",
    call: "git/trees/main?recursive=1",
  },
  {
    name: "merge_when_clean: a merge record read's stderr",
    script: 'merge_when_clean "$repo" 7 --squash',
    rules: [{ match: graphql, code: 1, err: says }, noChecks],
    kept: "merge-record.err",
    call: "api graphql -F owner=sjawhar",
  },
  {
    name: "merge_when_clean: a state gh answered",
    script: 'merge_when_clean "$repo" 7 --squash',
    rules: [{ match: graphql, out: record({ state: marker }) }, noChecks],
    kept: "merge-record.answer",
    call: "api graphql -F owner=sjawhar",
  },
  {
    name: "merge_when_clean: a merge commit gh answered, which the caller prints",
    script:
      'merge_when_clean "$repo" 7 --squash\nprintf \'commit=%s\\n\' "$merge_when_clean_commit"',
    rules: [{ match: graphql, out: record({ state: "MERGED", merge: "UNKNOWN", commit: marker }) }],
    kept: "merge-record.answer",
    call: "api graphql -F owner=sjawhar",
    unnamed: true,
  },
  {
    name: "merge_when_clean: a head gh answered, which the hand merge names",
    script: 'merge_when_clean "$repo" 7 --squash',
    rules: [
      {
        match: graphql,
        out: [
          record({ merge: "CLEAN", head: marker }),
          record({ state: "MERGED", merge: "UNKNOWN", head: marker, commit: mergeCommit }),
        ],
      },
      { match: "* pr merge 7 *" },
    ],
    kept: "merge-record.answer",
    call: "pr merge 7 --match-head-commit",
    unnamed: true,
  },
  {
    name: "merge_when_clean: an auto-merge time gh answered",
    script: 'merge_when_clean "$repo" 7 --squash',
    rules: [{ match: graphql, out: record({ at: marker, method: "SQUASH" }) }, noChecks],
    kept: "merge-record.answer",
    call: "api graphql -F owner=sjawhar",
  },
  {
    name: "merge_when_clean: the head branch gh answered, deleted after the merger's merge",
    script: 'merge_when_clean "$repo" 7 --squash --delete-branch',
    rules: [
      {
        match: graphql,
        out: record({ state: "MERGED", merge: "UNKNOWN", ref: marker, commit: mergeCommit }),
      },
      { match: branchDelete },
    ],
    kept: "merge-record.answer",
    call: "api -X DELETE repos/sjawhar/legion-smoke/git/refs/heads/",
    unnamed: true,
  },
  {
    name: "merge_when_clean: the hand merge's refusal",
    script: 'merge_when_clean "$repo" 7 --squash',
    rules: [
      {
        match: graphql,
        out: [
          record({ merge: "CLEAN" }),
          record({ state: "MERGED", merge: "UNKNOWN", commit: mergeCommit }),
        ],
      },
      { match: "* pr merge 7 *", code: 1, err: says },
    ],
    kept: "hand-merge.log",
    call: `pr merge 7 --match-head-commit ${head} --squash`,
  },
  {
    name: "merge_when_clean_timeout: the unsettled checks gh answered",
    script: 'merge_when_clean "$repo" 7 --squash',
    rules: [
      { match: graphql, out: record({}) },
      {
        match: rollup,
        out: `[{"name":"${marker}","workflow":"gate","status":"QUEUED","conclusion":null}]\n`,
      },
    ],
    kept: "unsettled-checks.answer",
    call: "pr view 7 --json statusCheckRollup",
  },
  {
    name: "merge_when_clean_timeout: the checks read's stderr",
    script: 'merge_when_clean "$repo" 7 --squash',
    rules: [
      { match: graphql, out: record({}) },
      { match: rollup, code: 1, err: says },
    ],
    kept: "unsettled-checks.err",
    call: "pr view 7 --json statusCheckRollup",
  },
  {
    name: "merge_when_clean_record: a branch delete GitHub refused",
    script: 'await_merger_merge "$repo" 7 --delete-branch',
    rules: [
      { match: graphql, out: record({ state: "MERGED", merge: "UNKNOWN", commit: mergeCommit }) },
      { match: branchDelete, code: 1, err: says },
    ],
    kept: "branch-delete.log",
    call: `api -X DELETE repos/sjawhar/legion-smoke/git/refs/heads/${branch}`,
  },
  {
    name: "smoke_pr_mergeable: a merge state gh answered, which the timeout hook prints",
    script: "smoke_pr_mergeable 5 || report_smoke_pr_merge_state",
    rules: [{ ...cleanup.state, out: `${marker}\n` }],
    kept: "cleanup-merge-state.answer",
    call: "pr view 5 --json mergeStateStatus",
  },
  {
    name: "smoke_pr_mergeable: gh's stderr",
    script: "smoke_pr_mergeable 5 || report_smoke_pr_merge_state",
    rules: [{ ...cleanup.state, code: 1, out: undefined, err: says }],
    kept: "cleanup-merge-state.err",
    call: "pr view 5 --json mergeStateStatus",
  },
  {
    name: "smoke_branch_state: gh's stderr",
    script: "smoke_branch_state proof/clean-main-s3test || true",
    rules: [{ ...cleanup.refs, code: 1, err: says }],
    kept: "branch-state.err",
    call: "git/matching-refs/heads/proof/clean-main-s3test",
  },
  {
    name: "smoke_commit_files: gh's stderr",
    script: `smoke_commit_files ${head}`,
    rules: [{ match: `* api repos/*/commits/${head} *`, code: 1, err: says }],
    kept: "commit-files.err",
    call: `api repos/sjawhar/legion-smoke/commits/${head}`,
  },
  {
    name: "clean_smoke_main: the main read's stderr",
    script: "clean_smoke_main",
    rules: cleanupRules({ ...cleanup.main, code: 1, out: undefined, err: says }),
    kept: "main-ref.err",
    call: "git/ref/heads/main",
  },
  {
    name: "clean_smoke_main: the branch create's stderr",
    script: "clean_smoke_main",
    rules: cleanupRules({ ...cleanup.create, code: 1, out: undefined, err: says }),
    kept: "cleanup-branch.err",
    call: "git/refs -f ref=refs/heads/proof/clean-main-s3test",
  },
  {
    name: "clean_smoke_main: a leftover's read's stderr",
    script: "clean_smoke_main",
    rules: cleanupRules({ ...cleanup.sha, code: 1, out: undefined, err: says }),
    kept: "leftover.err",
    call: "contents/.legion/LEGION-1/plan.json?ref=proof/clean-main-s3test",
  },
  {
    name: "clean_smoke_main: a leftover's removal's stderr",
    script: "clean_smoke_main",
    rules: cleanupRules({ ...cleanup.remove, code: 1, out: undefined, err: says }),
    kept: "leftover.err",
    call: "api -X DELETE repos/sjawhar/legion-smoke/contents/.legion/LEGION-1/plan.json",
  },
  {
    name: "clean_smoke_main: the pull request create's stderr",
    script: "clean_smoke_main",
    rules: cleanupRules({ ...cleanup.pr, code: 1, out: undefined, err: says }),
    kept: "cleanup-pull-request.err",
    call: "pr create --base main --head proof/clean-main-s3test",
  },
  {
    name: "clean_smoke_main: the pull request URL gh answered",
    script: "clean_smoke_main",
    rules: cleanupRules({
      ...cleanup.pr,
      out: `https://github.com/sjawhar/legion-smoke/pull/${marker}\n`,
    }),
    kept: "cleanup-pull-request.answer",
    call: "pr create --base main --head proof/clean-main-s3test",
    unnamed: true,
  },
  {
    name: "clean_smoke_main: the merge's stderr",
    script: "clean_smoke_main",
    rules: cleanupRules({ ...cleanup.merge, code: 1, err: says }),
    kept: "cleanup-merge.err",
    call: "pr merge 5 --squash --delete-branch",
  },
  {
    name: "close_smoke_cleanup: the listing's stderr",
    script: closeCleanup,
    rules: leftoverRules({ ...leftover.list, code: 1, out: undefined, err: says }),
    kept: "cleanup-open-pull-requests.err",
    call: "pr list --state open --limit 1000 --json number,url,headRefName",
  },
  {
    name: "close_smoke_cleanup: a pull request URL gh listed",
    script: closeCleanup,
    rules: leftoverRules({
      ...leftover.list,
      out: `5 https://github.com/${marker}/legion-smoke/pull/5\n`,
    }),
    kept: "cleanup-open-pull-requests.answer",
    call: "pr list --state open --limit 1000 --json number,url,headRefName",
  },
  {
    name: "close_smoke_cleanup: a close GitHub refused",
    script: closeCleanup,
    rules: leftoverRules({ ...leftover.close, code: 1, err: says }),
    kept: "cleanup-close-5.log",
    call: "pr close 5 --comment",
  },
  {
    name: "close_smoke_cleanup: a branch delete GitHub refused",
    script: closeCleanup,
    rules: leftoverRules({ ...leftover.remove, code: 1, err: says }),
    kept: "cleanup-branch-delete.log",
    call: "api -X DELETE repos/sjawhar/legion-smoke/git/refs/heads/proof/clean-main-s3test",
  },
  {
    name: "close_run_pull_requests: the listing's stderr",
    script: "close_run_pull_requests || true",
    rules: [{ match: runList, code: 1, err: says }],
    kept: "run-pull-requests.err",
    call: "pr list --state open --limit 1000 --json number,headRefName",
  },
  {
    name: "close_run_pull_requests: a number gh listed",
    script: "close_run_pull_requests || true",
    rules: [{ match: runList, out: `${marker}\n` }, { match: "* pr close * --delete-branch *" }],
    kept: "run-pull-requests.answer",
    call: `pr close ${marker} --delete-branch`,
  },
  {
    name: "close_run_pull_requests: a branch delete gh refused after the close",
    script: "close_run_pull_requests || true",
    rules: [
      { match: runList, out: "2\n" },
      { match: "* pr close 2 --delete-branch *", code: 1, err: says },
      { match: "* pr view 2 --json state *", out: "CLOSED\n" },
    ],
    kept: "run-pull-request-close-2.log",
    call: "pr close 2 --delete-branch",
  },
  {
    name: "close_run_pull_requests: a close gh refused",
    script: "close_run_pull_requests || true",
    rules: [
      { match: runList, out: "2\n" },
      { match: "* pr close 2 --delete-branch *", code: 1, err: says },
      { match: "* pr view 2 --json state *", out: "OPEN\n" },
    ],
    kept: "run-pull-request-close-2.log",
    call: "pr close 2 --delete-branch",
  },
  {
    name: "post_bot_thread: the head read's stderr",
    script: "post_bot_thread || true",
    rules: [{ ...bot.head, code: 1, out: undefined, err: says }, bot.post],
    kept: "pull-request.err",
    call: "api repos/sjawhar/legion-smoke/pulls/7 --jq .head.sha",
  },
  {
    name: "post_bot_thread: the comment's stderr",
    script: "post_bot_thread || true",
    rules: [bot.head, { ...bot.post, code: 1, out: undefined, err: says }],
    kept: "bot-thread.err",
    call: "api --method POST repos/sjawhar/legion-smoke/pulls/7/comments",
  },
  {
    name: "post_bot_thread: the author gh answered",
    script: "post_bot_thread || true",
    rules: [bot.head, { ...bot.post, out: `PRRC_1 ${marker} User\n` }],
    kept: "bot-thread.answer",
    call: "api --method POST repos/sjawhar/legion-smoke/pulls/7/comments",
  },
  {
    name: "bot_thread_replies: gh's stderr",
    script: "bot_thread_replies PRRC_1 || true",
    rules: [{ match: graphql, code: 1, err: says }],
    kept: "bot-thread-replies.err",
    call: "reviewThreads(first: 100) { nodes { isResolved",
  },
  {
    name: "bot_thread_node_id: gh's stderr",
    script: "bot_thread_node_id PRRC_1 || true",
    rules: [{ match: graphql, code: 1, err: says }],
    kept: "bot-thread-node.err",
    call: "reviewThreads(first: 100) { nodes { id comments(first: 1)",
  },
  {
    name: "review_threads: gh's stderr",
    script: "review_threads || true",
    rules: [{ match: graphql, code: 1, err: says }],
    kept: "review-threads.err",
    call: "reviewThreads(first: 100) { pageInfo { hasNextPage }",
  },
  {
    name: "review_threads: jq's error over the page gh answered",
    script: "review_threads || true",
    rules: [
      {
        match: graphql,
        out: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":"${marker}"}}}}}\n`,
      },
    ],
    kept: "review-threads.jq.err",
    call: "reviewThreads(first: 100) { pageInfo { hasNextPage }",
  },
  {
    name: "assert_review_of_head: gh's stderr",
    script: "assert_review_of_head LEGION-1 1 CHANGES_REQUESTED",
    rules: [{ match: "* api --paginate repos/*/pulls/7/reviews *", code: 1, err: says }],
    kept: "reviewed-head.err",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/reviews",
  },
  {
    name: "assert_review_of_head: a reviewed commit gh answered",
    script: "assert_review_of_head LEGION-1 1 CHANGES_REQUESTED",
    rules: [{ match: "* api --paginate repos/*/pulls/7/reviews *", out: `${marker}\n` }],
    kept: "reviewed-head.answer",
    call: "api --paginate repos/sjawhar/legion-smoke/pulls/7/reviews",
  },
];

describe("lib/workflow.sh prints no word of gh's", () => {
  test.each(rows.map((row) => [row.name, row] as const))("%s", (_name, row) => {
    const r = run(row);
    // The row: no line the run printed holds a run of the marker.
    expect(markerLines(r.output)).toEqual([]);
    // The twin: gh's words, kept in the file, do hold the marker; and the run names that file.
    expect(holdsMarker(r.kept)).toBe(true);
    if (!row.unnamed) expect(r.output).toContain(r.keptPath);
    // The control: the stand-in gh answered the call the row is about.
    expect(r.calls.some((c) => c.includes(row.call))).toBe(true);
  });

  // The controls of the closed values: what the lib holds to a form still prints as gh answered it.
  test("a merge record of GitHub's own forms prints as GitHub answered it", () => {
    const r = run({
      name: "control",
      script:
        'merge_when_clean "$repo" 7 --squash\nprintf \'commit=%s\\n\' "$merge_when_clean_commit"',
      rules: [
        {
          match: graphql,
          out: [
            record({ at: "2026-10-09T12:00:00Z", method: "SQUASH" }),
            record({ merge: "CLEAN" }),
            record({ state: "MERGED", merge: "UNKNOWN", commit: mergeCommit }),
          ],
        },
        { match: "* pr merge 7 *" },
      ],
      kept: "merge-record.answer",
      call: "api graphql",
    });
    expect(r.code).toBe(0);
    expect(r.output).toContain(
      "is armed (auto-merge enabled at 2026-10-09T12:00:00Z (SQUASH); mergeStateStatus BLOCKED)"
    );
    expect(r.output).toContain(`the proof human merges it by hand at ${head}`);
    expect(r.output).toContain(`commit=${mergeCommit}`);
  });

  test("a URL and a number of GitHub's own form print as GitHub answered them", () => {
    const cleaned = run({
      name: "control",
      script: closeCleanup,
      rules: Object.values(leftover),
      kept: "cleanup-open-pull-requests.answer",
      call: "pr list",
    });
    expect(cleaned.output).toContain(
      "closed the run's cleanup pull request https://github.com/sjawhar/legion-smoke/pull/5\n"
    );
    const closed = run({
      name: "control",
      script: "close_run_pull_requests",
      rules: [{ match: runList, out: "2\n" }, { match: "* pr close 2 --delete-branch *" }],
      kept: "run-pull-requests.answer",
      call: "pr list",
    });
    expect(closed.output).toContain("closed sjawhar/legion-smoke#2 and deleted its branch\n");
  });
});
