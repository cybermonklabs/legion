import { expect, test } from "bun:test";
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  anchors,
  headingText,
  REPO_ROOT,
  retiredRerunProhibitions,
  retiredTesterCiReads,
  TESTER_ONLY_PARTS,
} from "./skills-guard";

// The heading parse behind `anchors`, which decides whether a `skill://<name>/<path>#anchor` link
// resolves. It stands in for `/^#{1,6}\s+(.+?)\s*#*$/`, an expression quadratic in a line's
// trailing whitespace (dispatch://INFRA-1639), and each row below is that expression's own answer,
// oddities included: it is the meaning every skill's anchors were written against.
const headings: readonly (readonly [line: string, text: string | undefined])[] = [
  ["# One", "One"],
  ["###### six", "six"],
  ["##\ttab", "tab"],
  // Seven hashes is no heading, and neither is a hash with no whitespace after it.
  ["####### seven", undefined],
  ["#no space", undefined],
  ["   ## indented", undefined],
  // The text must be a character, so a line of hashes and whitespace alone is no heading until
  // there is a character for the group to take: `##   ` gives it a space, whose anchor is `-`.
  ["##", undefined],
  ["######", undefined],
  ["## ", undefined],
  ["##   ", " "],
  ["## ###", "#"],
  // Trailing hashes are dropped with the whitespace before them, but only where they end the
  // line: after `##  ` stands whitespace, so the hashes are part of the text.
  ["## a##", "a"],
  ["## a ##", "a"],
  ["## a  ##  ", "a  ##"],
  ["## a#b", "a#b"],
  // A file read with CRLF endings and split on "\n" leaves the carriage return on the line, where
  // it is whitespace to drop; a carriage return inside the text is no heading, since `.` matches
  // no line terminator.
  ["## a\r", "a"],
  ["## a\rb", undefined],
];

test.each(headings)("the heading text of %j is %j", (line, text) => {
  expect(headingText(line)).toBe(text);
});

test("an anchor is the heading lower-cased, its punctuation dropped and its spaces hyphens", () => {
  expect([...anchors(["# One", "## a  ##  ", "## 💡 Tip ##", "#no space"].join("\n"))]).toEqual([
    "one",
    "a--",
    "-tip",
  ]);
});

// A heading line long enough to matter: the old expression took two minutes over this one, and a
// parse that reads it a bounded number of times takes about a millisecond. The bound is Bun's own
// per-test timeout rather than a wall-clock assertion of our own, which a loaded runner would
// make flaky: a parse quadratic in the trailing whitespace cannot finish inside it.
test("a heading line of 200,000 trailing spaces is parsed, not scanned once per character", () => {
  const spaces = " ".repeat(200_000);
  expect(headingText(`## a${spaces}x`)).toBe(`a${spaces}x`);
  expect(headingText(`## a${spaces}###`)).toBe("a");
  expect(headingText(`##${spaces}`)).toBe(" ");
  // The control the measurement used: one hash, so `\s+` takes the whole run in one pass.
  expect(headingText(`#${spaces}x`)).toBe("x");
});

// The rules INFRA-1303 retired (`retiredRerunProhibitions`), each run over a page of its own. The
// rows that run them over the trees (pi-legion's and pi-envoy's skills-guard.test.ts) expect
// nothing found, which a pattern that matches nothing passes too, so these rows show each pattern
// finds the sentence it was written for and lets through each sentence the trees keep beside it.
// Every page is wrapped as the files wrap, with a break inside the words a pattern reads, and the
// retired wording starts below the page's first line, so a hit names the line that wording starts
// on rather than the first line of the file.

/**
 * What `rule` — `retiredRerunProhibitions` unless named — reports over a root holding `page` alone,
 * as `file`, a path below that root (`page.md` unless named).
 */
function reported(
  page: readonly string[],
  rule: (roots: readonly string[]) => string[] = retiredRerunProhibitions,
  file = "page.md"
): string[] {
  const root = mkdtempSync(path.join(os.tmpdir(), "retired-rerun-"));
  try {
    mkdirSync(path.dirname(path.join(root, file)), { recursive: true });
    writeFileSync(path.join(root, file), `${page.join("\n")}\n`);
    return rule([root]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

// Each retired sentence in the words of the file that carried it, the text around it shortened,
// and the line its pattern's first word stands on. The last twenty-one rows are the shapes no file
// carried, in the words a reword would bring them back in. Every row is found by its own pattern
// alone, so a row whose pattern is taken out of the guard reports nothing and fails.
const retired: readonly (readonly [source: string, page: readonly string[], line: number])[] = [
  [
    'pr-body.md\'s "no CI lane is rerun as evidence"',
    [
      "- **The tester** reads CI on GitHub at the head it is judging for everything static: lint,",
      "  types and the unit and integration suites are CI's, read there and recorded for no later",
      "  step, except a `ci_check` job, and no CI lane is",
      "  rerun as evidence.",
    ],
    3,
  ],
  [
    'tester.md\'s "never re-run a CI lane as evidence"',
    [
      "Read CI for everything static, on GitHub at the head you tested and written down nowhere for a",
      "later step to trust, except a `ci_check` job, and never re-run a CI",
      "lane as evidence.",
    ],
    2,
  ],
  [
    'pr-body.md\'s "rerun no lane as evidence", as cybermonklabs/legion#4447 wrote it',
    [
      "- The tester tries to break the changed surface and writes the red test for a way no test would catch.",
      "  Lint, types and the unit and integration suites are CI's: read them on GitHub at the head, rerun",
      "  no lane as evidence, and wait for none.",
    ],
    2,
  ],
  [
    'tester.md\'s "rerun none of them yourself"',
    [
      "Read CI for everything static, on GitHub at the head you tested and written down nowhere for a",
      "later step to trust, except a `ci_check` job; rerun none of",
      "them yourself.",
    ],
    2,
  ],
  [
    'review-threads.md\'s "you re-run nothing"',
    [
      "## The reviewer, on a re-review",
      "",
      "A review bot that is red while a thread it opened still stands is CI like any other, and you",
      "re-run nothing: that red moves the issue nowhere, but the base branch holds the merge for a",
      "check it requires.",
    ],
    3,
  ],
  [
    'envoy/SKILL.md\'s "A Legion implementer waits this way only"',
    [
      "## Waiting on a re-run",
      "",
      "- A Legion implementer waits this way",
      "  only on a re-run of a red check its task names. Otherwise it pushes and completes its phase,",
      "  and the daemon reads CI for it (`skill://legion-worker`).",
    ],
    3,
  ],
  [
    'merge-gate.md\'s "is work to fix, so the issue goes back"',
    [
      "  - *The watch.* On a merge-queue base it reads the pull request until GitHub merges it or takes",
      "    it out of the queue, and acts on a removal: a head the queue took out over its own `merge_group`",
      "    checks is work to fix, so the issue goes",
      "    back; a person's dequeue is that person's decision, reported to the architect. A removal is",
      "    never a merge.",
    ],
    2,
  ],
  [
    'merger.md\'s "That is work to fix. Send the issue back"',
    [
      "5. Act on a removal by its `removalReason`:",
      "   - `failed_checks`: the queue's own `merge_group` checks failed. That is work to",
      "     fix. Send the issue back with `request_backward_move` (step 6).",
    ],
    2,
  ],
  [
    'merger.md\'s "is work to fix, so send the issue back"',
    [
      "5. On a merge-queue base, watch the pull request until GitHub merges it or takes it out of the",
      "queue, and act on a removal: a queue that took the head out because its own `merge_group`",
      "checks failed is work to fix, so send the issue back; a person's dequeue is that person's",
      "decision, which you report.",
    ],
    2,
  ],
  [
    'tester.md\'s "A lane still running is read as running; never wait for it."',
    [
      "Read CI for everything static, on GitHub at the head you tested and written down nowhere for a",
      "later step to trust. A lane still",
      "running is read as running; never wait",
      "for it.",
    ],
    2,
  ],
  [
    'pr-body.md\'s "the tester never waits for it"',
    [
      "  read CI on GitHub at the head you are judging for everything static: lint, types and the unit",
      "  and integration suites are CI's, read there and recorded for no later step. A lane GitHub shows",
      "  `queued` or `in progress` is read as such: the tester",
      "  never waits for it, since a required red that settles sends the work back from `testing` and",
      "  the reviewer reads the head's checks before it approves.",
    ],
    2,
  ],
  [
    'architect-common.md\'s "re-run it, wait for it, or have the implementer fix the code"',
    [
      "A `checks-red` notice is one thing only: the head of an issue in `testing` conflicts with its",
      "base. A red is yours to decide on — re-run",
      "it, wait for it, or have the implementer fix the code — and GitHub's to hold the merge for.",
    ],
    2,
  ],
  // worker-common.md's one sentence carries both grant shapes, so it is two rows, each shortened
  // to the half its own pattern reads.
  [
    'worker-common.md\'s "whether it is re-run, waited for or fixed"',
    [
      "When CI settles on the pull request's head, the daemon names every check GitHub reported. A red",
      "check is information, not a verdict: whether it is re-run,",
      "waited for or fixed, and whether the pull request merges, is GitHub's under its own rules.",
    ],
    2,
  ],
  [
    'worker-common.md\'s "is a decision for the roles it reached"',
    [
      "When CI settles on the pull request's head, the daemon names every check GitHub reported. What",
      "becomes of a red is a decision",
      "for the roles it reached, and whether the pull request merges is GitHub's.",
    ],
    2,
  ],
  [
    'troubleshooting.md\'s "Re-run a check that failed on itself, have the implementer fix code"',
    [
      "- **A red check.** Nothing moves for it. The daemon names every check GitHub reported, with the",
      "  conclusion GitHub gave it, and judges none of them. Re-run a check that failed",
      "  on itself, have the implementer fix code that is at fault, or leave it to GitHub, which holds",
      "  the merge under the base branch's own required checks.",
    ],
    2,
  ],
  [
    'review.go\'s message, "decide whether to re-run the checks, wait for them, or fix the code"',
    [
      "The daemon's message to the three roles told of a settlement read: decide whether to",
      "re-run the checks, wait for them, or fix the code.",
    ],
    2,
  ],
  [
    'merge-gate.md\'s "leaves the decision to the roles it told"',
    [
      "  CI red at the head in `accepting` moves nothing, as it moves nothing anywhere: the daemon names",
      "  the checks GitHub reported and leaves the decision",
      "  to the roles it told. Read the head's checks on GitHub yourself before you accept.",
    ],
    2,
  ],
  [
    'concepts.md\'s "A red is for those roles to decide on"',
    [
      "  them: it sends one message each to the tree's architect, the worker of the issue's current",
      "  phase and the implementer, and moves the issue nowhere. A red is for those",
      "  roles to decide on, and for GitHub to hold the merge over.",
    ],
    2,
  ],
  // The wait the architect retired after round 20 (`AN_UNREPORTED_RED_WAITS`), each in the words
  // of the file that carried it at b6cb4b99.
  [
    'red-at-the-head.md\'s "is unreported, not absent: it gets no re-run and is waited for"',
    [
      "The reviewer's existing re-run of a cancelled or absent required check, which its role prompt",
      "gives, neither spends the head's re-run nor is spent by it. A check the counted set does not",
      "list while a run at the head holds its failed job is unreported, not absent: it gets no re-run and",
      "is waited for, as *The act and the wait* says.",
    ],
    2,
  ],
  [
    'reviewer.md\'s "is unreported, not absent, and gets no re-run: wait for it"',
    [
      "Second the head's checks as GitHub counts them for the pull request, the counted set.",
      "One `queued` or `in_progress`: wait. One the counted set does not list yet while a run at the",
      "head holds its failed job is unreported, not absent, and gets no re-run: wait for it.",
    ],
    2,
  ],
  [
    "red-at-the-head.md's \"The run's own `completed` is no leave to press while the counted set does not report that check\"",
    [
      "`gh run view <run-id> --json status --jq .status`, and reads the log and presses the re-run only on",
      "`completed`. The run's own `completed` is no leave to press while the counted set does not report",
      "that check `completed`.",
    ],
    2,
  ],
  [
    'red-at-the-head.md\'s "A red the counted set does not yet report `completed` is not yet a red to act on"',
    [
      "## The act and the wait",
      "",
      "**A red the counted set does not yet report `completed` is not yet a red to act on, however the role",
      "learned of it.**",
    ],
    3,
  ],
  [
    'reviewer.md\'s third wait, "a red the counted set does not yet report `completed`, however you learned of it"',
    [
      "You wait for none of them but the three `skill://legion-worker/references/red-at-the-head.md`",
      "gives you: the run that holds a red, until it ends; your one re-run; and a red the counted set",
      "does not yet report `completed`, however you learned of it.",
    ],
    2,
  ],
  // The trigger of the reviewer's spend-free re-run (`ABSENCE_FROM_THE_SET_IS_RERUN`), in the words
  // of go/reviewer.md at 5525e6c7: the architect's gap review of 2026-11-22 found that its absence
  // from the counted set also took a failed job of a completed `pull_request` run that the set did
  // not list yet, so such a red got the re-run that spends nothing, with no record.
  [
    "reviewer.md's \"One `completed` as `cancelled`, or absent from that set: re-run that run's failed and cancelled jobs\"",
    [
      "One `queued` or `in_progress`: wait. One `completed` as `cancelled`, or",
      "absent from that",
      "set: re-run that run's failed and cancelled jobs, once, then wait — that re-run neither spends",
      "the head's one re-run of a red nor is spent by it.",
    ],
    2,
  ],
  [
    'a reviewer\'s re-run of a check the base does not require, "is yours to re-run once before you approve"',
    [
      "The reviewer reads the head's checks on GitHub before it approves the head it reviews.",
      "A red on a check the base does not",
      "require is yours to re-run once before you",
      "approve.",
    ],
    2,
  ],
  [
    'main\'s rule turned on a required check, "You neither wait for a required check nor re-run it"',
    [
      "  `gh pr view <n> --json baseRefName,headRefOid`, two reads. The required set is the",
      "  base branch's rulesets' required checks and its branch protection's, when it has any.",
      "  You neither wait for a required",
      "  check nor re-run it for the approval, and the daemon reads no verdict from it either.",
    ],
    3,
  ],
  // A ban written to the reviewer by name. Written to a bare `you`, as "You re-run nothing, a
  // required check red at the head included.", it reads as the merger's own rule does and passes,
  // which is the cost of `REVIEWER_RERUNS_NOTHING`'s subject.
  [
    'a ban of the reviewer\'s re-run of a required red, "The reviewer re-runs nothing, a required check red at the head included."',
    [
      "The reviewer reads the head's checks on GitHub before it approves.",
      "The reviewer re-runs",
      "nothing, a required check red at the head included.",
    ],
    2,
  ],
  [
    'a ban of the re-run of a check that failed, "Never re-run a check that failed."',
    [
      "The reviewer reads the head's checks on GitHub before it approves.",
      "Never re-run a",
      "check that failed.",
    ],
    2,
  ],
  [
    'a ban of the re-run of a lane, "Never re-run a lane that failed on itself."',
    [
      "The reviewer reads the head's checks on GitHub before it approves.",
      "Never re-run a lane",
      "that failed on itself.",
    ],
    2,
  ],
  // The merger's send-back with a colon, which `FIRST_EJECTION_GOES_BACK`'s `[,.]` and the two
  // literal pins of roles_test.go let through until the class search at INFRA-1303's merge head.
  [
    'merger.md\'s send-back with a colon, "That is work to fix: send the issue back"',
    [
      "5. Act on a removal by its `removalReason`:",
      "   - `failed_checks`: the queue's own `merge_group` checks failed. That is work to",
      "     fix: send the issue back with `request_backward_move` (step 6).",
    ],
    2,
  ],
  // One step from a retired form, each let through until the class search at b6cb4b99: the
  // evidence ban with `do not` or `don't`, the merger's send-back with `and` or of `it`, the thread
  // rule as an imperative, the decision as the told roles' own, and the architect's grant with one
  // choice fewer.
  [
    'tester.md\'s evidence ban with "do not", "Do not re-run a CI lane as evidence."',
    ["Read the app's own output for what you ran.", "Do not re-run a CI", "lane as evidence."],
    2,
  ],
  [
    'tester.md\'s evidence ban with "don\'t", "Don\'t rerun failed lanes as evidence."',
    ["Read the app's own output for what you ran.", "Don't rerun failed", "lanes as evidence."],
    2,
  ],
  [
    'merger.md\'s send-back with "and", "That is work to fix, and the issue goes back"',
    [
      "5. Act on a removal by its `removalReason`:",
      "   - `failed_checks`: the queue's own `merge_group` checks failed. That is work to",
      "     fix, and the issue goes back.",
    ],
    2,
  ],
  [
    'merger.md\'s send-back of "it", "That is work to fix. Send it back"',
    [
      "5. Act on a removal by its `removalReason`:",
      "   - `failed_checks`: the queue's own `merge_group` checks failed. That is work to",
      "     fix. Send it back with `request_backward_move`.",
    ],
    2,
  ],
  [
    "review-threads.md's rule as an imperative, \"While the review bot's thread stands, re-run nothing\"",
    [
      "## The reviewer, on a re-review",
      "",
      "While the review bot's thread stands,",
      "re-run nothing: that red moves the issue nowhere.",
    ],
    4,
  ],
  [
    'the decision as the told roles\' own, "The roles the daemon told decide whether to re-run it"',
    [
      "When CI settles on the pull request's head, the daemon names every check GitHub reported.",
      "The roles the daemon told",
      "decide whether to re-run it.",
    ],
    2,
  ],
  [
    'architect-common.md\'s grant with one choice fewer, "re-run it or wait for it"',
    [
      "A `checks-red` notice is one thing only: the head of an issue in `testing` conflicts with its",
      "base. A red is yours to decide on: re-run it or",
      "wait for it.",
    ],
    2,
  ],
  // A ban of the reviewer's re-run with the review bot named after it, each let through until the
  // class search at 5525e6c7 by `REVIEWER_RERUNS_NOTHING`'s trailing limit, which took a condition
  // after the `nothing`, or a `for` whatever its sentence named after it.
  [
    'a review bot named after the ban, "The reviewer re-runs nothing when a review bot is red."',
    [
      "The reviewer reads the head's checks on GitHub before it approves.",
      "The reviewer re-runs",
      "nothing when a review bot is red.",
    ],
    2,
  ],
  [
    'a review bot named after the ban, "The reviewer re-runs nothing until the review bot\'s thread is resolved."',
    [
      "The reviewer reads the head's checks on GitHub before it approves.",
      "The reviewer re-runs",
      "nothing until the review bot's thread is resolved.",
    ],
    2,
  ],
  [
    'a review bot named after the ban, "The reviewer re-runs nothing for a review bot\'s red."',
    [
      "The reviewer reads the head's checks on GitHub before it approves.",
      "The reviewer re-runs",
      "nothing for a review bot's red.",
    ],
    2,
  ],
  // Two shapes one step from a retired one, the first refused all along and the second let through
  // until the class search at 59442ee2: the reviewer's old trigger with `rerun`, and the review-bot
  // ban with the bot written `review-bot`.
  [
    'reviewer.md\'s old trigger with "rerun", "absent from that set: rerun that run\'s failed and cancelled jobs"',
    [
      "One `queued` or `in_progress`: wait. One `completed` as `cancelled`, or",
      "absent from that",
      "set: rerun that run's failed and cancelled jobs, once, then wait.",
    ],
    2,
  ],
  [
    'a review-bot named after the ban, "Your review re-runs nothing for a review-bot red."',
    [
      "The reviewer reads the head's checks on GitHub before it approves.",
      "Your review re-runs",
      "nothing for a review-bot red.",
    ],
    2,
  ],
  // The reviewer's old trigger with a negation that limits no re-run between the absence and the
  // re-run, each let through until the class search at e7052779 by the exception that took any
  // negation in that gap.
  [
    'the old trigger with "not merely late"',
    [
      "One `queued` or `in_progress`: wait. One `completed` as `cancelled`, or",
      "absent from that set, not merely late: re-run that run's failed and cancelled jobs, once, then",
      "wait.",
    ],
    2,
  ],
  [
    'the old trigger with "(it isn\'t listed)"',
    [
      "One `queued` or `in_progress`: wait. One `completed` as `cancelled`, or",
      "absent from that set (it isn't listed): re-run that run's failed and cancelled jobs, once, then",
      "wait.",
    ],
    2,
  ],
  [
    'the old trigger with "no matter why"',
    [
      "One `queued` or `in_progress`: wait. One `completed` as `cancelled`, or",
      "absent from that set, no matter why: re-run that run's failed and cancelled jobs, once, then",
      "wait.",
    ],
    2,
  ],
];

test.each(retired)("%s is reported at the line it starts on", (_source, page, line) => {
  expect(reported(page)).toEqual([`page.md:${line}`]);
});

// Each sentence the trees keep near the words of a retired one, which a pattern too wide would
// refuse, in the words of the file that carries it and the text around it shortened. The sentences
// no tree carries are the send-back limited by `only`, which the merge-queue pattern's lookahead
// lets through, the implementer's re-run of a check the base does not require, which holds no
// approval, the three after them, which a docs page, a runbook and the merger's prompt could
// plausibly say, which the lane and reviewer patterns refused until INFRA-1303's review round 3,
// and the last thirteen, the rules of this change's own page and prompts said of a role, the
// changelog fragment's own sentence and a measurement's note, which the lane, reviewer, wait,
// choice and approval patterns refused until the class search at b6cb4b99, and six after them, a
// check absent from the counted set whose re-run is withheld, which the absence pattern refused
// until the class search at 59442ee2 (the first three) and at f25ce50d (the last three, whose
// negation stands a word or more before the re-run).
const kept: readonly (readonly [source: string, page: readonly string[]])[] = [
  [
    'merge-gate.md\'s "A merge-group run is never re-run"',
    [
      "  failed and cancelled jobs, once, then wait — never the whole run, which runs again every job",
      "  that succeeded too. A merge-group run is never",
      "  re-run: the implementer finishes its round instead, and the pull request is tested again when",
      "  it is queued again.",
    ],
  ],
  [
    'merge-gate.md\'s "never re-run a job for a `cancelled` row that a newer run of the same name has superseded"',
    [
      "  When the list holds more than one run of a required name, the newest run decides. An older run",
      "  of that name, whatever its conclusion, is no result: never re-run a",
      "  job for a `cancelled` row that a newer run of the same name has superseded.",
    ],
  ],
  [
    'merge-gate.md\'s "No role re-runs a group build."',
    [
      "  as that page's *What becomes of the record* says. No role",
      "  re-runs a group build. GitHub does re-run one, but the re-run puts nothing back in the queue.",
    ],
  ],
  [
    'red-at-the-head.md\'s "If no run at the head holds the failing check, the role re-runs nothing"',
    [
      "- Red again: the reviewer requests changes and names the check. The implementer records the red,",
      "  tells the architect, and completes its phase, naming the check; the reviewer judges the red in",
      "  its round.",
      "- If no run at the head holds the failing check, the role re-runs",
      "  nothing and tells the architect.",
    ],
  ],
  [
    'red-at-the-head.md\'s "When the re-run is spent, the role re-runs nothing."',
    [
      "When the re-run is spent, the role",
      "re-runs nothing. The reviewer requests changes, names the",
      "check, and says the head's one re-run is spent.",
    ],
  ],
  [
    'red-at-the-head.md\'s "The architect in `accepting` and an implementer that runs retro re-run nothing."',
    ["- The architect in `accepting` and an implementer that runs retro re-run", "  nothing."],
  ],
  [
    "red-at-the-head.md's bound of the merger's re-run, \"At any other head it re-runs nothing.\"",
    [
      "- The holders of the re-run are the reviewer in `reviewing`, the implementer while it holds",
      "  `implementing`, and the merger in `merging` at a head its own forward merge made.",
      "- The merger re-runs only a failed `pull_request` check at a head its own forward merge",
      "  made: once at that head, only after it records the failure, and in the order, the waits and",
      "  the spend this page gives. At any other head it re-runs",
      "  nothing. A `merge_group` check goes only through the one requeue of a `failed_checks`",
      "  removal, which its role prompt gives.",
    ],
  ],
  [
    'red-at-the-head.md\'s "A role that is told of a red whose phase it does not hold re-runs nothing."',
    ["- A role that is told of a red whose phase it does not hold", "  re-runs nothing."],
  ],
  [
    'red-at-the-head.md\'s "what the counted set carries, re-runs nothing"',
    [
      "there spends the head's one re-run on a run nobody counted. The wait ends three ways: the",
      "counted set reports that check, and the role acts on it; the counted set settles with every",
      "check it holds `completed` and that one absent, or the head moves, and the role then records",
      "where it learned of it and what the counted set carries, re-runs",
      "nothing, goes on with its round and tells the architect.",
    ],
  ],
  [
    'red-at-the-head.md\'s "it gets no re-run"',
    [
      "1. A check that judges the pull request itself rather than its code, such as the title check, is",
      "   the change's, and it gets no",
      "   re-run.",
    ],
  ],
  [
    'review-threads.md\'s "a re-run does not clear that red"',
    [
      "App's. A review bot that is red while a thread it opened still stands is CI like any other, and a",
      "re-run does not clear that red: that red moves the issue nowhere, but the base branch holds the",
      "merge for a check it requires.",
    ],
  ],
  [
    'tester.md\'s "you do not run in your own pod what CI runs for you"',
    [
      "Let CI do its job: your job is to boot and run the app's real workflows like a user would. You",
      "read no check run, re-run no CI and wait on no CI: lint, types, the unit and integration suites",
      "and every other lane are CI's work, and you do not run in your own",
      "pod what CI runs for you.",
    ],
  ],
  [
    'a holder\'s wait in the Envoy skill, "waits this way on the re-run"',
    [
      "- The reviewer in `reviewing` or the implementer while it holds `implementing` waits this way on",
      "  the re-run of a red at the head, which `skill://legion-worker/references/red-at-the-head.md`",
      "  decides. Otherwise a Legion worker pushes and completes its phase, and the daemon reads CI for it.",
    ],
  ],
  [
    'merge-gate.md\'s "is work to fix, so the issue goes back on the second such removal"',
    [
      "    it out of the queue, and acts on a removal: a head the queue took out over its own",
      "    `merge_group` checks is work to fix, so the issue goes back",
      "    on the second such removal of that head, the first being queued once more (the bullet",
      "    below); a person's dequeue is that person's decision, reported to the architect.",
    ],
  ],
  [
    'merger.md\'s "the issue goes back only on the second such removal of that head"',
    [
      "   - `failed_checks`: the queue's own `merge_group` checks failed. At a head that has not changed,",
      "     the removal gets that same head queued once more (step 4), and the issue goes back only on",
      "     the second such removal of that head, with `request_backward_move` (step 6).",
    ],
  ],
  [
    'a merge-queue send-back limited by "only on the second"',
    [
      "    it out of the queue, and acts on a removal: a head the queue took out over its own",
      "    `merge_group` checks is work to fix, so the issue goes back only",
      "    on the second such removal of that head.",
    ],
  ],
  [
    'reviewer.md\'s "You wait for none of them but the three"',
    [
      "You wait for none of them but the three `skill://legion-worker/references/red-at-the-head.md`",
      "gives you: the run that holds a red, until it ends; your one re-run; and a check you were told",
      "of that no `pull_request` run of the head holds yet, waited for once.",
    ],
  ],
  [
    "red-at-the-head.md's \"A failed job of the head's own `pull_request` run is acted on once that run has completed\"",
    [
      "`failure` over a lane GitHub cancelled is that cancelled lane. A failed job of the head's own",
      "`pull_request` run is acted on once that run has completed, whether or not the counted set lists its",
      "check yet, and no run that is not a `pull_request` run of the head is acted on at all.",
    ],
  ],
  [
    "red-at-the-head.md's \"The run's own `completed` is the leave to press\"",
    [
      "`completed`. The run's own `completed` is the leave to press where the run is a `pull_request` run",
      "of the head, whether or not the counted set lists the check yet; until then the role waits for",
      "the run, as *The act and the wait* says.",
    ],
  ],
  [
    "red-at-the-head.md's wait's end, \"the counted set settles with every check it holds `completed` and that one absent\"",
    [
      "- After that one wait no `pull_request` run of the head holds the check, and the counted set settles",
      "  with every check it holds `completed` and that one absent, or the head moves. Then the role",
      "  records the red, where it learned of it and what the counted set carries, re-runs nothing, goes",
      "  on with its round, and tells the architect.",
    ],
  ],
  // The sentences beside `ABSENCE_FROM_THE_SET_IS_RERUN`: the reviewer's spend-free re-run and the
  // page's that replaced its old trigger, the page's absence of a check the counted set does not
  // list yet, and the architect's absence from the daemon's notice.
  [
    'reviewer.md\'s spend-free re-run, "A required check absent from the counted set, or `completed` as `cancelled` there"',
    [
      "One `queued` or `in_progress`: wait. A required check absent from the counted set, or",
      "`completed` as `cancelled` there, whose job in a `pull_request` run of the head was cancelled",
      "or never ran: re-run that run's failed and cancelled jobs, once, then wait — that re-run",
      "neither spends the head's one re-run of a red nor is spent by it. A failed job of a completed",
      "`pull_request` run of the head is never that case: it is a red, which that page decides.",
    ],
  ],
  [
    "red-at-the-head.md's \"The reviewer's role prompt gives it one more re-run\"",
    [
      "spend is judged. The reviewer's role prompt gives it one more re-run, of a required check absent",
      "from the counted set or `completed` as `cancelled` there whose job in a `pull_request` run of the",
      "head was cancelled or never ran. That re-run neither spends the head's re-run nor is spent by it.",
    ],
  ],
  [
    'red-at-the-head.md\'s "may find the failing check pending, queued, in progress or absent"',
    [
      "**An unread counted set is not an empty one.** After a push the counted set can lag, so a role",
      "that reads it at once may find the failing check pending, queued, in progress or absent. A read",
      "made seconds after a push never shows that the head has no red.",
    ],
  ],
  [
    'architect-common.md\'s "is absent from that list rather than named as missing"',
    [
      "This notice names no required check: one the base requires that never ran has no check run at",
      "all and is absent from that list rather than named as missing, which makes the missing check",
      "the thing to look for — read the base's required checks and the head's check runs on GitHub.",
    ],
  ],
  [
    'pr-body.md\'s "the tester lets CI do its job"',
    [
      "  catch. Lint, types and the unit and integration suites are CI's: the tester lets CI do its",
      "  job, boots and runs the app's real workflows like a user would, and reads no check run,",
      "  re-runs no CI and waits on no CI, since the reviewer reads the head's checks before it approves.",
    ],
  ],
  [
    'worker-common.md\'s "or the merger in `merging` at a head its own forward merge made re-runs one"',
    [
      "A red check is information, not a verdict: only the reviewer in `reviewing`, the",
      "implementer while it holds `implementing`, or the merger in `merging` at a head its own",
      "forward merge made re-runs one the change did not make. It re-runs it once at that head,",
      "only after it records the failure, and follows the order, the waits and the read of the",
      "spend in `skill://legion-worker/references/red-at-the-head.md`. The reviewer or the",
      "implementer records the failure in a handoff record of its own phase with",
      "`dispatch handoff-write`, and the merger, which writes no handoff, in a `dispatch comment`",
      "on the issue. The merger re-runs only a failed `pull_request` check: a `merge_group` check",
      "goes only through the one requeue of a `failed_checks` removal, and at any other head the",
      "merger re-runs",
      "nothing. The tester presses nothing, and an implementer that runs retro re-runs nothing.",
    ],
  ],
  [
    'troubleshooting.md\'s "or the merger in `merging` at a head its own forward merge made re-runs a check that failed on itself"',
    [
      "  the issue's current phase and the implementer, and judges none of them. The reviewer in",
      "  `reviewing`, the implementer while it holds `implementing`, or the merger in `merging`",
      "  at a head its own forward merge made re-runs a check that failed on itself once at that",
      "  head, after it records the failure, and follows the order, the waits and the read of the",
      "  spend in `skills/legion-worker/references/red-at-the-head.md`. The reviewer or the",
      "  implementer records the failure in a handoff record of its phase",
      "  (`dispatch handoff-write`), and the merger, which writes no handoff, in a",
      "  `dispatch comment` on the issue. The merger re-runs only a failed `pull_request` check,",
      "  re-runs no merge queue group build, and re-runs nothing at any other head. No other role",
      "  re-runs one: the tester reads no CI. The implementer fixes code that is at fault, and",
      "  GitHub holds the merge.",
    ],
  ],
  [
    'concepts.md\'s "Three roles hold the re-run of a check that failed on itself"',
    [
      "  names no pull request. A red is for the reviewer in `reviewing` or the implementer while",
      "  it holds `implementing` to act on, and in `merging` for the merger. Three roles hold the",
      "  re-run of a check that failed on itself: the reviewer in `reviewing`, the implementer while",
      "  it holds `implementing`, and the merger in `merging` at a head its own forward merge made.",
      "  Each re-runs it once at that head, only after it records the failure, and follows the",
      "  order, the waits and the read of the spend in",
      "  `skills/legion-worker/references/red-at-the-head.md`. The merger re-runs only a failed",
      "  `pull_request` check, records the failure in a `dispatch comment` on the issue, since it",
      "  writes no handoff, and re-runs nothing at any other head. It re-runs no merge queue group",
      "  build: the first `failed_checks` removal of a head gets that head queued once more. The",
      "  tester and an implementer that runs retro re-run nothing. The implementer fixes code that is",
      "  at fault, and a check still running is waited for.",
    ],
  ],
  [
    'merge-gate.md\'s "the daemon names the checks GitHub reported and judges none of them"',
    [
      "  CI red at the head in `accepting` moves nothing, as it moves nothing anywhere: the daemon names",
      "  the checks GitHub reported and judges none of them. Read the head's checks",
    ],
  ],
  [
    "architect-common.md's \"where the head's one re-run is already spent it is the implementer's too\"",
    [
      "a red required check the change made is the implementer's, `request_backward_move` to",
      "`implementing`; one it did not make is the reviewer's to re-run once, so tell the reviewer on",
      "its topic, and where the head's one re-run is already spent it is the implementer's too",
      "(`skill://legion-worker/references/red-at-the-head.md` decides which);",
    ],
  ],
  [
    'implementer.md\'s "never wait for CI, except for the waits"',
    [
      "Push, then complete: never wait for CI, except for the waits",
      "`skill://legion-worker/references/red-at-the-head.md` gives for a red you act on. Outside those",
      "waits, each of which a wake ends as that page says, never wait for, watch or poll the pull",
      "request's remote CI — no `gh run watch`, no `gh pr checks --watch`.",
    ],
  ],
  [
    'merge-gate.md\'s "still in progress: wait for it"',
    [
      "  cancelled results again and stays red. GitHub refuses a re-run while an attempt of that run is",
      "  still in progress: wait for it.",
    ],
  ],
  [
    // Brought in from main (cybermonklabs/legion#4211) by the forward merge at 32d06c1c. The
    // architect's decision kept main's rule for a check the base does not require: the reviewer
    // neither waits for nor re-runs such a check for the approval, and keeps its one re-run for a
    // red on a check the base requires. So the sentence is the rule, and passes.
    'merge-gate.md\'s "You neither wait for it nor re-run it for the approval"',
    [
      "  `gh pr view <n> --json baseRefName,headRefOid`, two reads. The required set is the",
      "  base branch's rulesets' required checks and its branch protection's, when it has any.",
      "  A check the base does not require holds no approval. You neither wait for it nor",
      "  re-run it for the approval, and the daemon reads no verdict from it either.",
    ],
  ],
  [
    'red-at-the-head.md\'s "The reviewer neither waits for a check the base does not require nor re-runs it"',
    [
      "- The reviewer neither waits for a check the base does not",
      "  require nor re-runs it for the approval, as `skill://legion-worker/references/merge-gate.md` says.",
      "  It approves over such a red the change did not make and names the red in its `redAtTheHead`",
      "  record and its review body, and nowhere else: with no re-run, nothing shows the test fails at",
      "  random, so the red is no occurrence for the census. The implementer still re-runs such a red",
      "  once.",
    ],
  ],
  [
    "an implementer's re-run of a check the base does not require, which holds no approval",
    [
      "The reviewer approves over a red the change did not make on a check the base does not require.",
      "The implementer re-runs a red on a check the base",
      "does not require once.",
    ],
  ],
  [
    'a docs page\'s "If no checks appear after a minute, re-run the workflow from the Actions tab."',
    [
      "After a push, GitHub starts the pull request's workflows on its own. If no",
      "checks appear after a minute, re-run the workflow from the Actions tab.",
    ],
  ],
  [
    'a runbook\'s "When no job starts, rerun `apply.sh`"',
    [
      "The deploy runs one job for each region. When no job",
      "starts, rerun `apply.sh` and read what it prints.",
    ],
  ],
  [
    'the merger\'s prompt, "at any other head you re-run nothing"',
    [
      "You re-run only a failed `pull_request` check at a head your own forward merge made, once at",
      "that head, and only after you record the failure as that page says; a `merge_group` check goes",
      "only through the one requeue of a `failed_checks` removal above, and at any other head you re-run",
      "nothing.",
    ],
  ],
  [
    'the merger\'s prompt, "The merger never re-runs a check: …"',
    [
      "A removal over `failed_checks` gets the same head queued once more. The merger never",
      "re-runs a check: a re-run of a group build puts nothing back in the queue.",
    ],
  ],
  [
    'a tester\'s page, "The tester re-runs no check and reads no CI."',
    [
      "The tester boots the app and drives it as a user would. The tester re-runs no",
      "check and reads no CI.",
    ],
  ],
  [
    'a measurement\'s note, "No job was re-run in that measurement"',
    [
      "The run's jobs were read at its end. No job was",
      "re-run in that measurement: attempt 1 is the run's only attempt.",
    ],
  ],
  [
    'the merger\'s prompt, "The merger re-runs none of them: …"',
    [
      "The group build's checks failed. The merger re-runs none",
      "of them: it records the removal and queues the head once more.",
    ],
  ],
  [
    "red-at-the-head.md's spent re-run, \"When the head's one re-run is spent, the reviewer re-runs nothing\"",
    ["When the head's one re-run is spent, the reviewer re-runs", "nothing and requests changes."],
  ],
  [
    'a reviewer past review, "Past review the reviewer re-runs nothing"',
    [
      "Past review the reviewer re-runs",
      "nothing: it reads the merge and approves the new head or requests changes.",
    ],
  ],
  [
    'red-at-the-head.md\'s limit, "The reviewer re-runs nothing for a check the base does not require."',
    ["The reviewer re-runs nothing for a", "check the base does not require."],
  ],
  [
    "the implementer's prompt, \"A job still running is CI's work: never wait for it\"",
    [
      "Push, then complete. A job still running is CI's work: never wait",
      "for it, and push when your round is done.",
    ],
  ],
  [
    'the merger\'s prompt, "Do not re-run the group build, wait for its settlement, or leave the queue"',
    [
      "A group build that failed is no red of yours. Do not re-run the group build, wait for its",
      "settlement, or leave the queue: record the removal first.",
    ],
  ],
  [
    'the pi-legion changelog fragment\'s "while the implementer still re-runs such a red once"',
    [
      "The reviewer neither waits for nor re-runs a check the base does not require for the approval,",
      "while the implementer still re-runs such a red once.",
    ],
  ],
  [
    'an approval over a red of a check the base does not require, "which only the implementer re-runs"',
    [
      "The reviewer approves over a red of a check the base does not require, which only the",
      "implementer re-runs.",
    ],
  ],
  [
    'a tester\'s page, "The tester neither waits for a required check nor re-runs it"',
    [
      "The tester drives the app. The tester neither waits for a required",
      "check nor re-runs it: it lets CI do its job.",
    ],
  ],
  [
    'the daemon\'s page, "The daemon never re-runs a job on its own."',
    ["The daemon names every check GitHub reported. The daemon never re-runs a", "job on its own."],
  ],
  [
    'a check absent from the counted set, "is never re-run on that absence alone"',
    [
      "The counted set can lag behind a push. A check absent from the counted set is never",
      "re-run on that absence alone.",
    ],
  ],
  [
    'a check absent from the counted set, "is not re-run while its run is in progress"',
    [
      "The counted set can lag behind a push. A required check absent from the counted set is not",
      "re-run while its run is in progress.",
    ],
  ],
  [
    'a check absent from that set, "gets no re-run until a `pull_request` run of the head holds its job"',
    [
      "The counted set can lag behind a push. A check absent from that set gets no",
      "re-run until a `pull_request` run of the head holds its job.",
    ],
  ],
  [
    'a check absent from the counted set, "is no reason to re-run anything"',
    [
      "The counted set can lag behind a push. A check absent from the counted set is no reason to",
      "re-run anything.",
    ],
  ],
  [
    'a check absent from the counted set, "gets no spend-free re-run while a failed job holds it"',
    [
      "The counted set can lag behind a push. A required check absent from the counted set gets no",
      "spend-free re-run while a failed job holds it.",
    ],
  ],
  [
    'a check absent from the counted set, "is not to be re-run on that absence alone"',
    [
      "The counted set can lag behind a push. A check absent from the counted set is not to be",
      "re-run on that absence alone.",
    ],
  ],
];

test.each(kept)("%s is not reported", (_source, page) => {
  expect(reported(page)).toEqual([]);
});

// The tester's sentences INFRA-1303 retired when the tester came to read no CI
// (`retiredTesterCiReads`, Sami's decision on
// dispatch://INFRA-1303/ask/dd2229bf-075c-4524-b045-41f39978dbb3), each in the words of the file
// that carried it, the text around it shortened, and the line its pattern's first matched word
// stands on. As above, each row is found by one pattern alone, so a row whose pattern is taken out
// of the guard reports nothing and fails.
const retiredTesterReads: readonly (readonly [
  source: string,
  page: readonly string[],
  line: number,
])[] = [
  [
    'tester.md\'s "read CI for everything static", as main has it',
    [
      "Judge whether the implementer's tests would catch the ways it breaks. Record what you ran and",
      "what you observed, and read CI for everything",
      "static — lint, types, the unit and integration suites and every other lane are CI's work, read",
      "on GitHub at the head you tested and written down nowhere for a later step to trust.",
    ],
    2,
  ],
  [
    'concepts.md\'s "reads CI for everything static"',
    [
      "| `reviewing` | reviewer | Reviews the pull request. | review App |",
      "| `testing` | tester | Runs the changed surface for real, tries to break it, and reads CI for everything static; reports `pass` or `fail`. | review App |",
    ],
    2,
  ],
  [
    "pr-body.md's \"are CI's, read on GitHub at the head\"",
    [
      "- The tester tries to break the changed surface and writes the red test for a way no test would",
      "  catch. Lint, types and the unit and integration suites are CI's,",
      "  read on GitHub at the head.",
    ],
    2,
  ],
  [
    "pr-body.md's \"are CI's: read them on GitHub at the head\", as main has it",
    [
      "- The tester tries to break the changed surface and writes the red test for a way no test would",
      "  catch. Lint, types and the unit and integration suites are CI's: read",
      "  them on GitHub at the head, and wait for none.",
    ],
    2,
  ],
  [
    "tester.md's \"are CI's work, read on GitHub at the head you tested\", its read of everything static shortened away",
    [
      "Judge whether the implementer's tests would catch the ways it breaks. Lint, types, the unit and",
      "integration suites and every other lane are CI's work, read",
      "on GitHub at the head you tested and written down nowhere for a later step to trust.",
    ],
    2,
  ],
  [
    "pr-body.md's \"are CI's, read there and recorded for no later step\", as it stood before",
    [
      "- **The tester** reads CI on GitHub at the head it is judging for everything static: lint,",
      "  types and the unit and integration suites are CI's, read",
      "  there and recorded for no later step.",
    ],
    2,
  ],
  [
    'a reword of pr-body.md\'s "are CI\'s: read them on GitHub at the head" with no place, "read them at the head you tested"',
    [
      "- The tester tries to break the changed surface and writes the red test for a way no test would",
      "  catch. Lint, types and the unit and integration suites are CI's: read",
      "  them at the head you tested.",
    ],
    2,
  ],
  [
    "tester.md's \"Read the head's checks on GitHub before any verdict\"",
    [
      "Record what you ran and what you observed.",
      "Read the head's checks on GitHub before any verdict, whether or not a check of the issue",
      "carries `ci_check`.",
    ],
    2,
  ],
  [
    "pr-body.md's \"The tester reads the head's checks on GitHub before any verdict\"",
    [
      "- The tester tries to break the changed surface and writes the red test for a way no test would",
      "  catch. The tester reads the",
      "  head's checks on GitHub before any verdict.",
    ],
    2,
  ],
  [
    'a reword of "The tester reads the head\'s checks on GitHub before any verdict" as "the head\'s CI checks"',
    [
      "- The tester tries to break the changed surface and writes the red test for a way no test would",
      "  catch. The tester reads the head's",
      "  CI checks before any verdict.",
    ],
    2,
  ],
  [
    "tester.md's \"The role that holds the issue's phase re-runs a red at the head\"",
    [
      "Record what you ran and what you observed. The role that holds the issue's",
      "phase re-runs a red at the head of the pull request that the change did not cause.",
    ],
    1,
  ],
  [
    "red-at-the-head.md's \"The holder of the issue's phase presses the re-run: the tester in `testing`\"",
    [
      "## Who may re-run",
      "",
      "- The holder of the issue's phase presses the re-run: the tester in `testing`, the reviewer in",
      "  `reviewing`, the implementer in `implementing`.",
    ],
    3,
  ],
  [
    'worker-common.md\'s "That role is the tester in `testing`, the reviewer in `reviewing`"',
    [
      "A red check is information, not a verdict. That role is the tester",
      "in `testing`, the reviewer in `reviewing` or the implementer while it holds `implementing`.",
    ],
    1,
  ],
  [
    "pr-body.md's \"is the tester's to re-run once\"",
    [
      "  read on GitHub at the head of the pull request. A red at the head that the change did not make is",
      "  the tester's to re-run once.",
    ],
    2,
  ],
  [
    'red-at-the-head.md\'s "The tester and the implementer still re-run such a red once."',
    [
      "  with no re-run, nothing shows the test fails at random, so the red is no occurrence for the",
      "  census. The tester and the implementer still re-run such a red once.",
    ],
    2,
  ],
  [
    "pr-body.md's \"by the role that holds the issue's phase\"",
    [
      "- A red check that failed on its own is re-run once with `gh run rerun <run-id> --failed`, by the",
      "  role that holds the issue's phase and after it records the failure, and its settlement waited",
      "  for as `skill://envoy` says (it waits again on a re-run still queued or in progress).",
    ],
    2,
  ],
  [
    "red-at-the-head.md's \"The holder of the issue's phase presses the re-run\", its list shortened away",
    ["## Who may re-run", "", "- The holder of the issue's", "  phase presses the re-run."],
    3,
  ],
  [
    "architect.md's \"the role that holds the issue's phase re-runs a red the change did not make once\"",
    [
      "When CI settles on the head, the daemon names every check GitHub reported. A red is",
      "information, not a verdict: the role that holds the issue's",
      "phase re-runs a red the change did not make once.",
    ],
    2,
  ],
  [
    'a reword of the holder as "The role holding the issue\'s phase re-runs a red at the head"',
    [
      "Record what you ran and what you observed. The role holding the issue's",
      "phase re-runs a red at the head.",
    ],
    1,
  ],
  [
    "a reword of pr-body.md's passive re-run, \"is re-run once by the holder of the issue's phase\"",
    [
      "A red at the head that the change did not make is information. It is re-run once by the",
      "holder of the issue's phase.",
    ],
    2,
  ],
  [
    "a reword of the page's press, \"The holder of the issue's phase presses `gh run rerun <run-id> --failed` once\"",
    [
      "## Who may re-run",
      "",
      "- The holder of the issue's phase presses",
      "  `gh run rerun <run-id> --failed` once.",
    ],
    3,
  ],
  [
    'a reword of the holder\'s re-run, "re-runs the failed jobs once"',
    [
      "Record what you ran and what you observed. The role that holds the issue's phase re-runs the",
      "failed jobs once.",
    ],
    1,
  ],
  [
    'tester.md\'s "you wait for none of them but three"',
    [
      "The round goes on when the re-run is green. A lane GitHub shows `queued` or `in progress` is read",
      "as such, and you wait for none of them but three: the run that holds a red, until it ends; that",
      "re-run; and a red the counted set does not yet report `completed`.",
    ],
    2,
  ],
  [
    'tester.md\'s "you wait for none of them but that re-run"',
    [
      "The round goes on when the re-run is green. A lane GitHub shows `queued` or `in progress` is",
      "read as such, and you wait for none of them but that re-run.",
    ],
    2,
  ],
  [
    'pr-body.md\'s "The tester waits on CI for nothing but what that page gives it"',
    [
      "  holds the rule. The tester",
      "  waits on CI for nothing but what that page gives it: the run that holds a red, until it ends.",
    ],
    1,
  ],
  [
    'pr-body.md\'s "the tester waits for none of them but the re-run"',
    [
      "  read on GitHub at the head. A lane GitHub shows `queued` or `in progress` is read as such, and",
      "  the tester waits for none",
      "  of them but the re-run.",
    ],
    2,
  ],
  [
    'a reword of pr-body.md\'s wait, "The tester waits on CI only for the re-run that page gives it"',
    ["  holds the rule. The tester waits on", "  CI only for the re-run that page gives it."],
    1,
  ],
  [
    'a reword of pr-body.md\'s wait, "The tester waits for nothing on CI but the re-run"',
    ["  holds the rule. The tester waits for nothing", "  on CI but the re-run."],
    1,
  ],
  [
    'pr-body.md\'s "however the tester learned of it"',
    [
      "  the re-run; and a red the counted set does not yet report `completed`, however the tester",
      "  learned of it.",
    ],
    1,
  ],
  [
    'ci-checks.md\'s "A `ci_check` check goes in `failures`"',
    [
      "- The tester runs and records only the checks with no `ci_check`. A `ci_check` check goes in",
      "  `failures` only when its derived result is a fail, or its run concluded `skipped`, `neutral` or",
      "  `cancelled`.",
    ],
    1,
  ],
  [
    'red-at-the-head.md\'s "--phase <implement|test|review>" before the `gh run rerun` call, as 0d572aa7 had it',
    [
      "The record is a handoff record of the role's own phase, written with",
      "`dispatch handoff-write --issue <issue> --phase <implement|test|review> --body-file <file>` before the",
      "`gh run rerun` call, never after it.",
    ],
    2,
  ],
  [
    "a `test` handoff carrying `redAtTheHead`",
    [
      "A red at the head that the change did not make is written down before anything else. The",
      "tester writes the failure down in a `test`",
      "handoff whose `redAtTheHead` names all eight facts.",
    ],
    2,
  ],
  [
    "`redAtTheHead` in a `--phase test` record",
    [
      "Record the red before anything else.",
      "Its `redAtTheHead` list goes in the record you write with",
      "`dispatch handoff-write --issue <issue> --phase test`.",
    ],
    2,
  ],
  [
    '`redAtTheHead` in a record of `phase: "test"`',
    [
      "Every testing round records a `test` handoff on the Dispatch issue.",
      'Its JSON object carries `phase: "test"` and a `redAtTheHead` list of',
      "the reds at the head.",
    ],
    2,
  ],
  [
    "`redAtTheHead` before the `test` handoff that carries it",
    [
      "Record the red before anything else.",
      "A `redAtTheHead` list naming all eight facts goes in the",
      "`test` handoff of the round.",
    ],
    2,
  ],
  [
    '"the tester records the red"',
    [
      "A red at the head that the change did not make is the reviewer's to re-run once.",
      "The tester",
      "records the red in its handoff and goes on with its round.",
    ],
    2,
  ],
  [
    "the tester's `redAtTheHead`",
    [
      "The reviewer names the red in its review body.",
      "The tester's `redAtTheHead` entry names",
      "the same eight facts.",
    ],
    2,
  ],
  // The tester beside the reviewer on a case only CI shows, which no pattern refused until the class
  // search at b6cb4b99.
  [
    'ci-checks.md\'s "The tester and the reviewer each send it to the architect"',
    [
      "2. A held-back job reads `skipped`, not absent: the label cures it.",
      "3. Otherwise the check's words are wrong. The tester and the",
      "   reviewer each send it to the architect as a finding with `envoy_publish`; neither fails the",
      "   round nor requests changes for it.",
    ],
    2,
  ],
];

test.each(retiredTesterReads)("%s is reported at the line it starts on", (_source, page, line) => {
  expect(reported(page, retiredTesterCiReads)).toEqual([`page.md:${line}`]);
});

// Every path `TESTER_ONLY_PARTS` names is a file of the repository: a path a rename left behind
// would leave the rule scoped to it reading nothing.
test.each([...TESTER_ONLY_PARTS])("the tester's own part %s exists", (part) => {
  expect(existsSync(path.join(REPO_ROOT, part))).toBe(true);
});

// The tester's read of a check CI proves (`THE_TESTERS_READ_SHOWS_A_DERIVED_RESULT`), which is
// retired only where the tester is the reader, so it is read only in the parts composed for the
// tester alone (`TESTER_ONLY_PARTS`). The first row is the tester's role part at 5525e6c7, which
// the architect's gap review of 2026-11-22 found after the tester came to read no CI; the next
// three are rewords, the last two of which name another role after the read and were let through
// until the class search at e7052779. Each row is run in each of those parts, where it is refused,
// and in a page outside them, where the same words can be another role's read, and pass.
const readsInTesterParts: readonly (readonly [
  source: string,
  page: readonly string[],
  line: number,
])[] = [
  [
    'tester.md\'s "that read shows its one derived result"',
    [
      "Read the issue's checks with `dispatch results --issue <issue>`. A check carrying `ci_check` is",
      "CI's: that read shows its one",
      "derived result. It needs no pass from the implementer, and you do not run or record it.",
    ],
    2,
  ],
  [
    'a reword of the tester\'s read said to it, "that read shows its derived result, which you record in `failures`"',
    [
      "Read the issue's checks with `dispatch results --issue <issue>`.",
      "A check carrying `ci_check` is CI's: that read shows its",
      "derived result, which you record in `failures`.",
    ],
    2,
  ],
  [
    'the tester\'s read with the reviewer after it, "which the reviewer reads before it approves"',
    [
      "Read the issue's checks with `dispatch results --issue <issue>`.",
      "A check carrying `ci_check` is CI's: that read shows its one",
      "derived result, which the reviewer reads before it approves.",
    ],
    2,
  ],
  [
    'the tester\'s read with the implementer after it, "and it needs no pass from the implementer"',
    [
      "Read the issue's checks with `dispatch results --issue <issue>`.",
      "A check carrying `ci_check` is CI's: that read shows its one",
      "derived result, and it needs no pass from the implementer.",
    ],
    2,
  ],
];

test.each(
  TESTER_ONLY_PARTS.flatMap((part) => readsInTesterParts.map((row) => [part, ...row] as const))
)("in %s, %s is reported at the line it starts on", (part, _source, page, line) => {
  expect(reported(page, retiredTesterCiReads, part)).toEqual([`${part}:${line}`]);
});

test.each(
  readsInTesterParts
)("outside the tester's own parts, %s is not reported", (_source, page) => {
  expect(reported(page, retiredTesterCiReads)).toEqual([]);
});

// The tester's own sentences beside that read, which the rule lets through in the parts composed
// for it: the role part's "you do not read its derived result", and a read that shows no derived
// result.
const keptInTesterParts: readonly (readonly [source: string, page: readonly string[]])[] = [
  [
    'tester.md\'s "you do not read its derived result"',
    [
      "Read the issue's checks with `dispatch results --issue <issue>`. A check carrying `ci_check` is",
      "CI's (`skill://legion-worker/references/ci-checks.md`): you do not read its derived result, run it or",
      "record it, and it is never an entry of your `failures`.",
    ],
  ],
  [
    'a read that shows no derived result, "that read shows the implementer\'s results"',
    [
      "Read the issue's checks with `dispatch results --issue <issue>`: that read shows the",
      "implementer's results and the commit each one was recorded at.",
    ],
  ],
];

test.each(
  TESTER_ONLY_PARTS.flatMap((part) => keptInTesterParts.map((row) => [part, ...row] as const))
)("in %s, %s is not reported", (part, _source, page) => {
  expect(reported(page, retiredTesterCiReads, part)).toEqual([]);
});

// Each sentence the trees keep beside the tester's retired ones, which a pattern too wide would
// refuse: the sentences that replaced them, the reviewer's own read and wait, and the sentences that
// keep a `ci_check` check CI's. The last twenty-one are sentences a docs page, a runbook or a
// tester's page could plausibly say, which no tree carries: the tester patterns refused the first
// seven until the class search at INFRA-1303's merge head, the next seven until the one at
// 5c710625, the next three until the one at b6cb4b99, the next one, the reviewer's read of a
// derived result, until the one at 59442ee2, and the last three, the reviewer's own reads said to
// it, until the one at e7052779.
const keptBesideTesterReads: readonly (readonly [source: string, page: readonly string[]])[] = [
  [
    'tester.md\'s "Let CI do its job"',
    [
      "Record what you ran and what you observed. Let CI do its job: your job is to boot and run the",
      "app's real workflows like a user would. You read no check run, re-run no CI and wait on no CI:",
      "lint, types, the unit and integration suites and every other lane are CI's work, and you do not",
      "run in your own pod what CI runs for you. A red check at the head of the pull request ends no",
      "round of yours, since your verdict is on the issue's checks and the surface you drove.",
    ],
  ],
  [
    "pr-body.md's \"are CI's: the tester lets CI do its job\"",
    [
      "  catch. Lint, types and the unit and integration suites are CI's: the tester lets CI do its",
      "  job, boots and runs the app's real workflows like a user would, and reads no check run,",
      "  re-runs no CI and waits on no CI, since the reviewer reads the head's checks before it approves.",
    ],
  ],
  [
    'red-at-the-head.md\'s "The holders of the re-run are the reviewer in `reviewing`, the implementer while it holds `implementing`, and the merger in `merging`"',
    [
      "- The holders of the re-run are the reviewer in `reviewing`, the implementer while it holds",
      "  `implementing`, and the merger in `merging` at a head its own forward merge made.",
      "- The tester presses nothing: it reads no CI, and a red at the head does not end its round.",
      "- A role that is told of a red whose phase it does not hold re-runs nothing.",
    ],
  ],
  [
    "worker-common.md's \"One role holds the issue's phase at a time\"",
    [
      "One role holds the issue's phase at a time, and every role that has a session may work beside",
      "it. A reviewer reads each push as it lands, and a tester re-runs nothing.",
    ],
  ],
  [
    'reviewer.md\'s "You wait for none of them but the three"',
    [
      "You wait for none of them but the three `skill://legion-worker/references/red-at-the-head.md`",
      "gives you: the run that holds a red, until it ends; your one re-run; and a check you were told",
      "of that no `pull_request` run of the head holds yet, waited for once.",
    ],
  ],
  [
    "concepts.md's \"The reviewer reads the head's checks on GitHub itself before it approves\"",
    [
      "  needs its merge forward now (below), and never re-implements. The reviewer reads the",
      "  head's checks on GitHub itself before it approves, and the architect before it accepts.",
    ],
  ],
  [
    "tester.md's and the tester's role part's \"is CI's\"",
    [
      "A check CI runs is recorded from CI's job at the head: a check carrying `ci_check` is CI's, and",
      "its result is the one derived from that job's run. A check carrying `ci_check` is CI's",
      "(`skill://legion-worker/references/ci-checks.md`): you do not read its derived result, run it or",
      "record it, and it is never an entry of your `failures`.",
    ],
  ],
  [
    'architect-root.md\'s "whenever that read shows no claim at all"',
    [
      "Claim the root with `dispatch claim --issue <root>` whenever that read shows no claim at all, or",
      "shows your own session.",
    ],
  ],
  [
    "ci-checks.md's \"It neither reads nor records a `ci_check` check's derived result\"",
    [
      "- The tester runs and records only the checks with no `ci_check`. It neither reads nor records a",
      "  `ci_check` check's derived result, which is CI's: the reviewer reads it before it approves.",
    ],
  ],
  [
    'ci-checks.md\'s "The reviewer sends it to the architect as a finding"',
    [
      "3. Otherwise the check's words are wrong. The reviewer sends it to the architect as a finding",
      "   with `envoy_publish`, and requests no changes for it; the tester reads no CI run, so the case",
      "   never reaches it.",
    ],
  ],
  [
    'concepts.md\'s "the tester and the reviewer go on at that head"',
    [
      "  records the conflict, and the tester and the reviewer go on at that head; an approval of the",
      "  head ends the review round.",
    ],
  ],
  [
    'pr-body.md\'s "After a forward merge, the tester reruns each check the merge can reach"',
    [
      "- After a forward merge, the tester reruns each check the merge can reach and records each other",
      "  check with no `ci_check` at the new head unrun.",
    ],
  ],
  [
    "red-at-the-head.md's record before the press, `--phase <implement|review>`, and the merger's `dispatch comment`",
    [
      "The implementer's or the reviewer's record is a handoff record of the role's own phase, written",
      "with `dispatch handoff-write --issue <issue> --phase <implement|review> --body-file <file>` before",
      "the `gh run rerun` call, never after it. Its body carries the field `redAtTheHead`, a list with one",
      "entry per red, each entry naming all eight facts above. The merger writes no handoff record: its",
      "record is a `dispatch comment` on the issue that carries the eight facts, posted before the",
      "`gh run rerun` call, as its `failed_checks` requeue record is.",
    ],
  ],
  [
    'red-at-the-head.md\'s "names the red in its `redAtTheHead` record and its review body"',
    [
      "  as `skill://legion-worker/references/merge-gate.md` says. It approves over such a red the change",
      "  did not make and names the red in its `redAtTheHead` record and its review body, and nowhere else:",
      "  with no re-run, nothing shows the test fails at random, so the red is no occurrence for the",
      "  census.",
    ],
  ],
  [
    "the worker skill's handoff read, `--phase <plan|implement|test|review>`",
    [
      "1. Read the issue. Then read each earlier phase's handoff (`dispatch handoff-read --issue <issue>",
      "   --phase <plan|implement|test|review>`). A record beats your memory.",
    ],
  ],
  [
    "the implementer's role part's `--phase test --version <n>`",
    [
      "Read the earlier handoffs too, and, for the round that sent the work back, the `test` or `review`",
      "handoff your task's `Handoff:` line names, at its version, with the command that line quotes",
      "(`--phase test --version <n>` or `--phase review --version <n>`).",
    ],
  ],
  [
    'tester.md\'s `test` handoff, its `phase: "test"` and its `failures`',
    [
      "Every testing round records a `test` handoff on the Dispatch issue: a JSON object with",
      '`schemaVersion: 1`, `phase: "test"`, `implementerProof`, and `failures` (each `{check, evidence}`).',
    ],
  ],
  [
    "a `test` handoff that carries no `redAtTheHead`",
    ["The tester reads no CI, so its `test` handoff carries no", "`redAtTheHead`."],
  ],
  [
    'the tester\'s red test, "The tester writes the red test that pins each defect"',
    [
      "The tester tries to break the changed surface. The tester writes the red",
      "test that pins each defect it finds, and the resumed implementer makes it pass.",
    ],
  ],
  [
    "a docs page's \"Formatting is CI's: read the `fmt` job's log\"",
    [
      "The repository checks its own format in CI. Formatting is CI's:",
      "read the `fmt` job's log to see what it changed.",
    ],
  ],
  [
    "a docs page's \"Lint is CI's work, read the job log on GitHub when it fails.\"",
    [
      "Nothing lints on your machine. Lint is CI's work,",
      "read the job log on GitHub when it fails.",
    ],
  ],
  [
    "a docs page's \"The reviewer reads the head's checks on GitHub before any verdict it gives.\"",
    [
      "The reviewer works from the pull request alone. The reviewer reads the",
      "head's checks on GitHub before any verdict it gives.",
    ],
  ],
  [
    "a runbook's \"starts the role that holds the issue's phase again, and that role reruns its last command\"",
    [
      "After a restart the daemon starts the role that holds the issue's",
      "phase again, and that role reruns its last command.",
    ],
  ],
  [
    'a tester\'s page, "The tester waits for the app to boot, but reads no CI."',
    [
      "The tester boots the app and drives it as a user would. The tester waits",
      "for the app to boot, but reads no CI.",
    ],
  ],
  [
    'a tester\'s page, "The tester waits until the deploy finishes, but never on CI."',
    [
      "The tester drives the deployed app. The tester waits until the",
      "deploy finishes, but never on CI.",
    ],
  ],
  [
    'a tester\'s page, "The tester notes the red banner the app shows when the login fails."',
    [
      "The tester records what it saw. The tester notes the red",
      "banner the app shows when the login fails.",
    ],
  ],
  [
    "a tester's page, \"The tester waits for nothing but the app's own boot.\"",
    [
      "The tester boots the app and drives it as a user would. The tester waits for",
      "nothing but the app's own boot.",
    ],
  ],
  [
    'a tester\'s page, "The tester waits on CI for nothing, but it waits for the app to boot."',
    [
      "The tester boots the app and drives it as a user would. The tester waits on CI for",
      "nothing, but it waits for the app to boot.",
    ],
  ],
  [
    "a runbook's \"re-runs its setup, and is then taken by the role that holds the issue's phase\"",
    [
      "A pod restarted by the daemon re-runs its setup, and is then taken by the role that",
      "holds the issue's phase.",
    ],
  ],
  [
    "a runbook's \"The daemon reruns the reminder, which is read by the role that holds the issue's phase.\"",
    ["The daemon reruns the reminder, which is read by the role that holds", "the issue's phase."],
  ],
  [
    "a runbook's \"A job is re-run by the daemon, and its log is read by the role that holds the issue's phase.\"",
    [
      "A job is re-run by the daemon, and its log is read by the role that holds",
      "the issue's phase.",
    ],
  ],
  [
    "a runbook's \"The role that holds the issue's phase presses Enter in its pane\"",
    ["The role that holds the issue's phase presses", "Enter in its pane to accept the prompt."],
  ],
  [
    "a runbook's \"The role that holds the issue's phase reruns its last command after a restart.\"",
    ["The role that holds the issue's phase reruns", "its last command after a restart."],
  ],
  [
    'a tester\'s page, "The tester records each red check of the issue in `failures`"',
    [
      "The tester runs the issue's checks. The tester records each red",
      "check of the issue in `failures`, with the command it ran and what it observed.",
    ],
  ],
  [
    'a tester\'s page, "The tester names the red check of the issue that failed"',
    [
      "The tester drives the changed surface. The tester names the red",
      "check of the issue that failed on the surface it drove.",
    ],
  ],
  [
    'a tester\'s page, "The tester notes the red check the dashboard shows"',
    [
      "The tester opens the app's CI page. The tester notes the red",
      "check the dashboard shows when the app's CI page loads.",
    ],
  ],
  [
    'the reviewer\'s read, "the read shows its one derived result"',
    [
      "Before it approves, the reviewer reads each `ci_check` check: the read",
      "shows its one derived result.",
    ],
  ],
  [
    'the reviewer\'s imperative read, "Before you approve, read each `ci_check` check in `dispatch results`"',
    [
      "Before you approve, read each `ci_check` check in `dispatch results`: the read shows its one",
      "derived result.",
    ],
  ],
  [
    'the reviewer\'s read said to it, "your read shows its one derived result"',
    [
      "A check carrying `ci_check` is CI's: your read shows its one derived result, and you approve",
      "only on a pass.",
    ],
  ],
  [
    'the reviewer\'s read of each, "the read shows each derived result"',
    ["Read `dispatch results` at the head: the read shows each", "derived result."],
  ],
];

test.each(keptBesideTesterReads)("%s is not reported", (_source, page) => {
  expect(reported(page, retiredTesterCiReads)).toEqual([]);
});
