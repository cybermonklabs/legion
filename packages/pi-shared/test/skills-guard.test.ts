import { expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  anchors,
  headingText,
  retiredRerunProhibitions,
  retiredTesterCiReads,
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

/** What `rule` — `retiredRerunProhibitions` unless named — reports over a root holding `page` alone, as `page.md`. */
function reported(
  page: readonly string[],
  rule: (roots: readonly string[]) => string[] = retiredRerunProhibitions
): string[] {
  const root = mkdtempSync(path.join(os.tmpdir(), "retired-rerun-"));
  try {
    writeFileSync(path.join(root, "page.md"), `${page.join("\n")}\n`);
    return rule([root]);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

// Each retired sentence in the words of the file that carried it, the text around it shortened,
// and the line its pattern's first word stands on. The last six rows are the shapes no file
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
];

test.each(retired)("%s is reported at the line it starts on", (_source, page, line) => {
  expect(reported(page)).toEqual([`page.md:${line}`]);
});

// Each sentence the trees keep near the words of a retired one, which a pattern too wide would
// refuse, in the words of the file that carries it and the text around it shortened. The sentences
// no tree carries are the send-back limited by `only`, which the merge-queue pattern's lookahead
// lets through, the implementer's re-run of a check the base does not require, which holds no
// approval, and the last three, which a docs page, a runbook and the merger's prompt could
// plausibly say: the lane and reviewer patterns refused each until INFRA-1303's review round 3.
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
    'red-at-the-head.md\'s "Where no run at the head holds the failing check, the role re-runs nothing"',
    [
      "- Red again: the reviewer requests changes and names the check. The implementer records the red,",
      "  tells the architect, and completes its phase, naming the check; the reviewer judges the red in",
      "  its round.",
      "- Where no run at the head holds the failing check, the role re-runs",
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
    'red-at-the-head.md\'s "The merger, the architect in `accepting` and an implementer that runs retro re-run nothing."',
    [
      "- The merger, the architect in `accepting` and an implementer that runs retro re-run",
      "  nothing.",
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
      "gives you: the run that holds a red, until it ends; your one re-run; and a red the counted set",
      "does not yet report `completed`, however you learned of it.",
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
    'worker-common.md\'s "only the reviewer in `reviewing` or the implementer while it holds `implementing` re-runs one"',
    [
      "A red check is information, not a verdict: only the reviewer in `reviewing` or the implementer",
      "while it holds `implementing` re-runs one the change did not make, once at that head and after",
      "it records the failure in a handoff record of its own phase with `dispatch handoff-write`. The",
      "tester presses nothing, and the merger and an implementer that runs retro re-run",
      "nothing (`skill://legion-worker/references/red-at-the-head.md`).",
    ],
  ],
  [
    'troubleshooting.md\'s "re-runs a check that failed on itself once at that head"',
    [
      "  the issue's current phase and the implementer, and judges none of them. The reviewer in",
      "  `reviewing` or the implementer while it holds `implementing` re-runs a check that failed on",
      "  itself once at that head, after it records the failure in a handoff record of its phase",
      "  (`dispatch handoff-write`), and no other role re-runs one: the tester reads no CI. The",
      "  implementer fixes code that is at fault, and GitHub holds the merge.",
    ],
  ],
  [
    'concepts.md\'s "A red is for the reviewer in `reviewing` or the implementer while it holds `implementing` to act on"',
    [
      "  names no pull request. A red is for the reviewer in `reviewing` or the implementer while it holds",
      "  `implementing` to act on — either re-runs a check that failed on itself once at that head, the",
      "  tester, the merger and an implementer that runs retro re-run nothing, the implementer fixes code",
      "  that is at fault, and a check still running is waited for — and for GitHub to hold the merge",
      "  over.",
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
    'the merger\'s prompt, "You re-run nothing: …"',
    [
      "A red check at the head is not yours to act on. You re-run",
      "nothing: the reviewer in `reviewing` or the implementer while it holds `implementing` does.",
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
];

test.each(retiredTesterReads)("%s is reported at the line it starts on", (_source, page, line) => {
  expect(reported(page, retiredTesterCiReads)).toEqual([`page.md:${line}`]);
});

// Each sentence the trees keep beside the tester's retired ones, which a pattern too wide would
// refuse: the sentences that replaced them, the reviewer's own read and wait, and the sentences that
// keep a `ci_check` check CI's. The last fourteen are sentences a docs page, a runbook or a tester's
// page could plausibly say, which no tree carries: the tester patterns refused the first seven until
// the class search at INFRA-1303's merge head, and the last seven until the one at 5c710625.
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
    'red-at-the-head.md\'s "The holders of the re-run are the reviewer in `reviewing`"',
    [
      "- The holders of the re-run are the reviewer in `reviewing` and the implementer while it holds",
      "  `implementing`.",
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
      "gives you: the run that holds a red, until it ends; your one re-run; and a red the counted set",
      "does not yet report `completed`, however you learned of it.",
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
      "its result is the one derived from that job's run. A check carrying `ci_check` is CI's: that",
      "read shows its one derived result.",
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
    'pr-body.md\'s "After a forward merge, the tester reruns each check the merge can reach"',
    [
      "- After a forward merge, the tester reruns each check the merge can reach and records each other",
      "  check with no `ci_check` at the new head unrun.",
    ],
  ],
  [
    "red-at-the-head.md's record before the press, `--phase <implement|review>`",
    [
      "The record is a handoff record of the role's own phase, written with",
      "`dispatch handoff-write --issue <issue> --phase <implement|review> --body-file <file>` before the",
      "`gh run rerun` call, never after it. Its body carries the field `redAtTheHead`, a list with one",
      "entry per red, each entry naming all eight facts above.",
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
];

test.each(keptBesideTesterReads)("%s is not reported", (_source, page) => {
  expect(reported(page, retiredTesterCiReads)).toEqual([]);
});
