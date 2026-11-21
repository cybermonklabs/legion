# Notes for the repeated Dispatch unit suite

The derivations and the history of `.github/workflows/dispatch-unit-repeat.yaml`, which points
into this file. Each `# why:` line of that workflow names the section below that holds the
derivation its step, its job or its key rests on. A value's own arithmetic stays beside the value,
in the matrix's `include` rows.
In the text here, "the workflow" is `.github/workflows/dispatch-unit-repeat.yaml`, and "the tool"
is `packages/dispatch/e2e/tools/unit-repeat.ts`, which `bun run test:repeat` runs.

## The budgets measured from CI's own runs

Each lane's budgets are a hang guard and nothing more: a leg's verdict is what its rows print, and
a budget only ends a leg that will not end by itself. So each budget is set from the slowest leg
measured on main's suite, with a margin of twice that, and the matrix row of each lane writes the
measurement beside the budgets:

- `slowest_seconds`: the slowest test step of the lane's measured legs;
- `other_steps_seconds`: the slowest the job's other steps took together in one leg (the job's
  time minus its test step's), the checkout, Bun, the install, and for the harness lane zsh's
  install and PostgreSQL's start;
- `deadline_seconds`: twice `slowest_seconds`, up to the next whole minute, the time at which the
  step's `timeout` stops the tool (the next section);
- `step_minutes`: the deadline, the 60 s `timeout --kill-after` leaves the tool to report, and
  60 s of slack for the step's own start around `timeout`;
- `job_minutes`: `step_minutes`, and twice `other_steps_seconds` up to the next whole minute.

`packages/dispatch/e2e/__tests__/unit-repeat.test.ts`, which the dashboard job of `ci.yaml` runs
on every change to the workflow, holds each lane's three budgets to those rules, and holds the job
and the step to the matrix's values. Its controls plant each fault in a parsed copy of the
workflow, one past the boundary of its rule as worked out from that lane's own measurements, and
name the lane and the rule that fail. They repeat no measured number. So a new reading changes the
matrix and these notes and no line of the test.

### What was measured, on 2026-11-20

The step times of every leg of 19 runs of the workflow, read from
`gh api repos/cybermonklabs/legion/actions/runs/<id>/jobs` and from the legs' artifacts, 57 legs a
lane:

- the ten runs before 11:00Z that day (runs 44681726917 to 44712651976, pull requests and
  hand-started runs on five branches, each a merge of its head with main or that head);
- run 44726566359, started by hand at main `1c7c825367`;
- eight runs at the heads of the pull request that set these budgets, all on main `5570d0acd2`:
  44751162563 (started by hand) and 44751677485 (the pull request's own) at its first head,
  44759854373 and 44759866208 at its second, 44770373346 at its third, 44796999569 (started by
  hand) at its fourth, 44808555733 at its fifth, and 44814644776 at its sixth.

A leg stopped before Bun ended has no time of its own, and no report. Its time is estimated from
its `bun.log`, which holds a result line for each row that ended: the step's time, times the
results a whole leg of the same lane of that run wrote, over the results the stopped leg's log
holds.

| Lane | Legs whose Bun ended by itself | Legs stopped before Bun ended | Slowest | Old step budget |
| --- | --- | --- | --- | --- |
| web | 41, 726 to 1,954 s | 16, by the budget at 972 or 973 s, estimated at most 1,133 s | 1,954 s, run 44759854373, round 1 | 16 minutes |
| web-cold | 55, 94 to 158 s; 9 of them past the budget, so their steps failed with no row failed | 2, both by the budget at 132 s | 158 s, run 44751677485, round 1 | 2 minutes |
| harness | 56, 1,071 to 2,301 s | 1, by a cancel of its run at 2,274 s, estimated 2,283 s | 1,565 s, run 44796999569, round 2 | 30 minutes |

The web lane grew within the day: main's `a8067fcd51` (11:41Z) added
`web/src/features/issue/useIssueDrafts.test.ts`, whose rows took 1,168 s together over a leg's 20
runs, and its rows a run went from 6,296 (run 44726566359) to 6,462 (run 44751162563). So the
eight runs on main `5570d0acd2` hold the slowest legs of every lane. The same suite's web legs
took 1,202 to 1,954 s across those eight runs: one runner can take 1.6 times another's time,
which is the spread the margin of 2 is there for.

The harness lane's slowest leg is read over the 48 legs of the branches that change no harness
row. Pull request #4236 adds harness rows whose real-time waits take minutes, and its 9 legs took
1,651 to 2,301 s; it sets its own harness budget, and 2,301 s is inside the deadline set here.

The other steps took at most 37 s in a web leg (run 44751162563, round 3), 44 s in a web-cold leg
(run 44814644776, round 1) and 229 s in a harness leg (run 44709413418, round 1, most of it zsh's
install).

The window grew four times after a head had set budgets from it:

- the runs at the pull request's first head read a web leg of 1,661 s, which its second head took
  in;
- the runs at its second head read a web leg of 1,954 s and a harness leg of 1,496 s, which its
  third head took in;
- the runs at its third and fourth heads read a web-cold leg whose other steps took 32 s and a
  harness leg of 1,565 s, which its sixth head took in;
- the run at its sixth head read a web-cold leg whose other steps took 44 s, which its seventh head
  took in.

The first three times, a lane's budgets no longer kept the rule over every leg read, and the head
that took the legs in set every lane's budgets again by the rule. The fourth time the rule still
held, since 8 minutes and 2 × 44 s come to 568 s, under the web-cold job's 10 minutes: the seventh
head changed the measurement beside the budget, and no budget.

The largest job budget, 70 minutes for a web leg, is far inside GitHub's own limit for a job on a
GitHub-hosted runner. GitHub's "Actions limits" page
(https://docs.github.com/en/actions/reference/limits, read on 2026-11-20) says: "Each job in a
workflow can run for up to 6 hours of execution time." Each step's budget is inside its job's,
and each deadline inside its step's, so `timeout` fires first, then the step budget, then the job
budget, and GitHub's limit never decides.

### Why the old budgets stopped legs that were passing

The old budgets were the slowest leg plus 15 per cent, measured over the workflow's runs of
2026-11-17 to 2026-11-19, when a web run read 4,885 to 5,189 rows; main's suite then grew to
6,462 rows a run. So in the first 11 runs above, the budget stopped 16 web and 2 web-cold legs
before Bun ended, and failed 9 more web-cold legs after Bun had ended. Three of those nine are the
web-cold failures of the hour of 09:00Z on 2026-11-20 (jobs 136196998929, 136166391921 and
136207986486): in each, Bun had ended by itself with 6,214 rows passed and none failed, after
125.0, 131.3 and 125.4 s, and the tool had written a clean report, when the step's two-minute
budget failed the step. No row was running long; the lane was.

### What a run costs now

A run that passes takes what its rows take: the runs read cost 115 to 164 billed minutes, and $0,
since standard runners are free for a public repository. The budgets bound only a leg that hangs:
at most 70 minutes for a web leg, 10 for a web-cold leg and 63 for a harness leg.

### When to measure again

When a leg's report says it was stopped, and the file it names passes in the legs that ended by
themselves, the suite has outgrown the deadline: read the legs again as above, add them to the
window, and set the matrix's measurements and budgets from the new reading by the same rule. A
leg that ends by itself above a lane's `slowest_seconds` is read the same way: it moves the
slowest, and the budgets follow it, so a budget never rests on a margin its own window breaks.

## A leg's deadline, and what a stopped leg names

A step that GitHub ends at its `timeout-minutes` is ended from outside, and says only that the
action "has timed out": the tool never learns of it, so no report names a row, and the 18 legs the
old budgets stopped before Bun ended, in the reading above, wrote no report at all. So the step's
own deadline comes first, inside its budget, and the tool is told of it:

- The step runs `timeout --foreground --signal=TERM --kill-after=60 <deadline_seconds> bun run
  test:repeat ...`. At the deadline `timeout` sends SIGTERM to `bun run` alone, and not to its
  process group (`--foreground`), so Bun's workers are left for the tool to stop. `bun run` passes
  the SIGTERM on to the tool, through the script's `&&` too (measured on Bun 1.4.4: a script
  `true && bun <file>` under `timeout` got the signal in that file's process).
- The tool adds `packages/dispatch/e2e/tools/unit-repeat-progress.ts` to the lane's preloads. That
  preload appends a line to `<out>/progress.jsonl` as each worker loads a test file, and as each
  run of the file and each row of the run begins and ends.
- On the SIGTERM the tool reads that file first, and which of its workers are alive. Then it passes
  the signal on to `bun run`, which passes it on to Bun's test process. Bun runs in a process group
  of its own (`detached`), and the tool kills that group whole if Bun has not ended by the end of
  its grace after the stop, and again once it has ended, so that no worker outlives the tool.
- Once Bun has ended, for each file a live worker was in that had begun a row, the tool runs that
  file alone under the leg's seed with the preload failing every row before its body, which prints
  the file's rows by name in the order the seed runs them (measured against the order the bodies
  ran in, under five seeds). That run still runs the file's own code and hooks, so it runs in a
  process group of its own too, which the tool kills whole at the end of its bound, and again once
  it has ended.
- The report, the step summary and one `::error` for each such file name the file, the run of it
  and the row of that run by its place and its name, and give the command that runs the file again
  in that order. The report also says how Bun and each names run ended against their bounds. The
  tool exits 1, and `<out>/progress.jsonl` stays in the leg's artifact.

A stopped leg's error then reads, for a planted row that spins for ever in a copy of the package:

```text
::error file=packages/dispatch/e2e/__tests__/planted.test.ts,title=Running when the leg was stopped::e2e/__tests__/planted.test.ts: in row 1, "planted spins", or a hook after it, of run 1 of 2, 8.7 s since it began, when SIGTERM stopped the leg after 9.0 s (seed 2563570073). Run it again from packages/dispatch: bun run test:harness e2e/__tests__/planted.test.ts --seed=2563570073
```

That reading is the step's own `run:` text with the harness leg's values, two reruns and a 10 s
deadline, over that copy, under a 40 s `timeout` that stands in for GitHub's step budget: the leg
ended after 12 s and named the row. The seed is drawn afresh for each run, so the row's place
moves with it. The workflow before this deadline, at main `1c7c825367` and at `5570d0acd2`, ran
the same copy to the whole 40 s and printed no error and no report. `unit-repeat.test.ts` drives
the same stop through the tool on every change to it.

What a stop cannot name, and what it names with care:

- A file whose run had not begun is named as being in its own code, which Bun runs again for each
  run of it, and one whose run had begun and no row of it as being in a `beforeAll` hook.
- A row's `end` is written by the preload's `afterEach`, which Bun 1.4.4 runs after the file's own
  `afterEach` hooks and which a throwing one of them skips (both measured on 2026-11-20). So in a
  file that runs one row at a time, a row counts as ended when the next row of its run begins, and
  the last row begun with no `end` is named as "row N, or a hook after it". A file whose rows have
  all ended is named with the row that ended last, as being after it, in a hook or the code around
  its rows.
- A file that runs its rows at once (its source names `.concurrent`) is named with how many rows
  had begun and not ended, and no single row, since a `start` there says nothing of the rows
  beside it.
- Where the run that reads a file's names fails or reaches its bound, the row is named by its
  place alone and the report says why its name was not read.
- A file of a worker that was gone at the stop is not named as running. Bun's junit file gives a
  `(worker crashed)` testcase to each file whose results it had not taken in when the stop came:
  the file in hand, and also, measured on 2026-11-20, a file whose worker had ended its rows and
  exited a moment before. That testcase cannot tell a worker that crashed before the stop from one
  the stop ended, so after a stop the report lists each such file as one whose results Bun did not
  report, in the stop's own failure, and makes no failing row of it.

### The two bounds of a stop

A stop has two bounds of its own, each a hang guard that decides no verdict:

- the grace, how long Bun gets to end after the stop, before the tool kills its group: 10 s;
- the names bound, how long a run that reads a file's names gets, before the tool kills its group:
  20 s.

A caller sets them with `DISPATCH_UNIT_REPEAT_STOP_GRACE_MS` and
`DISPATCH_UNIT_REPEAT_NAMES_BOUND_MS`, as `unit-repeat.test.ts`'s stop rows do, so that neither
decides a row's verdict. Each default is at least three times the largest reading of its kind.

The readings were taken on 2026-11-20, from 19:42Z to 19:58Z, on the dev box, at its own load of
383 to 671. They come from nine legs, three of each lane, each in this tree's
`packages/dispatch`. Each leg ran `timeout --foreground --signal=TERM <deadline> bun run
test:repeat --lane <lane>` with the matrix's reruns, on 4 CPUs (`taskset`) with 8 busy loops on
those CPUs. The deadline was 90 s for web, 60 s for web-cold and 150 s for harness, so each leg was
stopped with 3 or 4 files running. Both bounds were set to 10 minutes, so that no bound cut a
reading short.

| Lane | Bun's end after the stop, ms | Each names run, ms |
| --- | --- | --- |
| web | 423, 636, 421 | 1,461, 1,550, 1,929, 1,629; 1,011, 1,922, 1,842, 1,556; 1,164, 1,324, 1,744, 2,191 |
| web-cold | 147, 86, 180 | 2,031, 2,017, 1,283; 1,278, 2,746, 1,325; 1,436, 3,743, 2,132 |
| harness | 745, 966, 683 | 509, 267, 268; 519, 344, 4,178; 106, 57, 83, 375 |

- **The grace:** Bun ended at most 966 ms after the stop. 3 × 966 ms is 2.9 s, and the grace is
  10 s.
- **The names bound:** a names run took at most 4,178 ms, with 3 or 4 running at once. 3 × 4,178 ms
  is 12.5 s, and the bound is 20 s.
- **Inside the kill-after:** the grace, the names bound and the 2 s the tool gives a pipe to close
  come to 32 s, inside the 60 s that the step's `timeout --kill-after` gives the tool after the
  stop.
