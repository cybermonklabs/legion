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
  60 s of slack for the step's own start around `timeout`, up to the next whole minute. The slack
  rests on no reading: it sits behind the deadline and the kill-after, so it decides nothing;
- `job_minutes`: `step_minutes`, twice `other_steps_seconds` and `retry_seconds`, up to the next
  whole minute.

The job's rule is this file's own. `.github/ci-notes/dispatch-browser.md`'s rule for
`dispatch-browser.yaml` adds the measured maximum of the steps without a budget once; this one
adds it twice, the same margin of 2 as the deadline, because the other steps vary from runner to
runner as the test step does. Over the 31 runs below, a leg's other steps took 15 to 37 s in the
web lane, 13 to 44 s in the web-cold lane and 28 to 229 s in the harness lane: 2.5, 3.4 and 8.2
times the fastest. Taken once, those maxima would give 69, 9 and 136 minutes in place of 70, 10
and 140, before the retry term below. The job budget stays a backstop behind the step's, and
decides no verdict.

`retry_seconds` is the retry arithmetic of the job's single slowest retried step, the term
.github/ci-budgets.md#the-budgets-note adds for a job that retries a download
(dispatch://INFRA-1545): the setup-bun step's two `ci_tools_fetch` calls at 3416 s
(`ci-budgets.md`'s Budgets note), above the workspace install's 1050 s and the harness leg's zsh
install's 1400 s. With it the three jobs take 127, 67 and 197 minutes. The download gate reads no
number out of `job_minutes`, an expression's value, so the test below holds it. It also holds each
lane's `retry_seconds` to the setup-bun step's own figure, which it reads from the comment above
that step's key (`2 x 1708 = 3416 s`), so a change of that figure the matrix does not take fails it.

`packages/dispatch/e2e/__tests__/unit-repeat-budgets.test.ts`, which the dashboard job of
`ci.yaml` runs on every change to the workflow, holds each lane's three budgets to exactly those
rules, and holds the job and the step to the matrix's values. So a budget raised or cut with no
new measurement fails it, and so does a measurement written with no budget set again from it.
Its controls plant each fault in a parsed copy of the workflow, a step above and a step below
each budget's rule as worked out from that lane's own measurements, and name the lane and the
rules that fail. They repeat no measured number. So a new reading changes the matrix and these
notes and no line of the test.

### What was measured, on 2026-11-20 to 2026-11-22

The step times of every leg of 31 runs of the workflow, read from
`gh api repos/cybermonklabs/legion/actions/runs/<id>/jobs` and from the legs' artifacts, 93 legs a
lane:

- the ten runs before 11:00Z on 2026-11-20 (runs 44681726917 to 44712651976, pull requests and
  hand-started runs on five branches, each a merge of its head with main or that head);
- run 44726566359, started by hand at main `1c7c825367`;
- eleven runs at the heads of the pull request that set these budgets: 44751162563 (started by
  hand) and 44751677485 (the pull request's own) at its first head, 44759854373 and 44759866208
  at its second, 44770373346 at its third, 44796999569 (started by hand) and 44796938761 (the
  pull request's own) at its fourth, 44808555733 at its fifth, 44814644776 at its sixth and
  44821135577 at its seventh, all on main `5570d0acd2`; and 44836898568 at its eighth, a merge of
  main `f1d27d3a82`;
- five pull-request runs of 2026-11-21 under main's old budgets: #4615's run 44839359908, and
  44913926295 (#4437), 44918296869, 44921906214 and 44926138389 (#4790), each a merge of its
  branch with main after #4615 had merged;
- run 44940719055, the pull request's own at its head `3989f41d6a`, a merge of main `e2ce417d59`;
  runs 44948990810 and 44956324879, its own at `3ff830bb16` and `b23e61c369`; and run
  44963607247, its own at `0682934fca`, whose merge ref (`73a40e355f`) put the head on main
  `dc7e668784`, which holds #4659.

A leg stopped before Bun ended has no time of its own, and no report. Its time is estimated from
its `bun.log`, which holds a result line for each row that ended: the step's time, times the
results a whole leg of the same lane of that run wrote, over the results the stopped leg's log
holds. The two web-cold legs stopped at 132 s, rounds 2 and 3 of run 44696645178, hold 6,166 and
6,208 results against round 1's 6,225, so they are estimated at 133.3 s and 132.4 s. In the runs
of 2026-11-21, a whole leg of run 44913926295 (#4437's branch) wrote 6,892 rows a run, so a whole
web leg there writes 137,840 results; run 44918296869's own whole leg wrote 6,909 a run, and
#4615's 6,464. Runs 44921906214 and 44926138389 have no whole leg of their own, and take 6,892,
more than the 6,861 a run of this pull request's merge of main: where their branches run main's
rows, that errs toward a longer estimate and a larger budget. The estimate takes the results to
come at an even rate. A leg whose last minutes are one slow file running alone ends later than
its estimate says, so an estimate is a floor.

Round 3 of run 44963607247 is such a leg. This pull request's own deadline stopped it at 3,300 s
while one file ran alone, `e2e/__tests__/outside-agent-acceptance.test.ts`, in row 7 of its run
17 of 20, after 294 of its 360 rows. The results it wrote, 17,874 of a whole leg's 17,940, give
3,312 s, the floor above. That file's own remaining 66 rows, at its 7.06 s a row, give 3,766 s.
The leg's time over the file's runs, 3,300 × 20 ÷ 17, gives 3,882 s. The larger, 3,882 s, is
taken: a budget is a hang guard, and errs long.

| Lane | Legs whose Bun ended by itself | Legs stopped before Bun ended | Slowest | Old step budget |
| --- | --- | --- | --- | --- |
| web | 62, 726 to 1,954 s | 31, by the budget at 972 or 973 s, estimated at most 1,614 s | 1,954 s, run 44759854373, round 1 | 16 minutes |
| web-cold | 82, 94 to 175 s; 14 of them past the old budget, so their steps failed with no row failed | 11, by the budget at 132 or 133 s, estimated at 132.4 to 173.9 s | 175 s, run 44956324879, round 3 | 2 minutes |
| harness | 91, 1,070 to 3,241 s | 2: a cancel of its run at 2,274 s, estimated 2,283 s; and this pull request's 3,300 s deadline, estimated 3,882 s | 3,882 s, estimated, run 44963607247, round 3 | 30 minutes |

The web lane grew within the day: main's `a8067fcd51` (11:41Z) added
`web/src/features/issue/useIssueDrafts.test.ts`, whose rows took 1,168 s together over a leg's 20
runs, and its rows a run went from 6,296 (run 44726566359) to 6,462 (run 44751162563). So the
ten runs on main `5570d0acd2` hold the slowest web leg. The same suite's web legs took 1,062 to
1,954 s across those ten runs: one runner can take 1.8 times another's time, which is the spread
the margin of 2 is there for. By 2026-11-21 main's suite had grown again, to 6,861 rows a run in
this pull request's merge of main, and no web leg of that day's five runs ended by itself: all 15
stopped at main's 16 minutes, estimated at 1,016 to 1,614 s. The pull request's own run at its
head `3989f41d6a`, 44940719055, read the first whole ones: 1,815, 1,313 and 1,881 s, under the
1,954 s the web budgets rest on.

The harness lane's slowest leg is read over the 81 legs of the branches that add no harness row of
their own beside main's: #4236's 9 legs in the window, and the 3 of #4790's run 44926138389, whose
head adds a harness row, are left out. Pull request #4615 merged on 2026-11-21 with harness rows of
its own, so its run 44839359908 counts, and its round 1 took 1,642 s; round 2 of the pull
request's own run 44940719055 took 1,649 s. Pull request #4659 then merged with
`outside-agent-acceptance.test.ts` and `outside-agent-public.test.ts`, 18 and 2 rows a run, which
take about 7 s and 31 s a row. In run 44963607247, the first whose merge holds them, the three harness
legs took 2,571 s and 3,241 s and the third was stopped at 3,300 s, against 1,107 to 1,649 s at
the heads before. The legs' row times summed to 31,553 to 31,989 s, against 28,975 s in a leg of
run 44956324879, and no row failed. The acceptance file's 20 runs, about 2,500 s of rows, run one
after another in one worker, and it was the last file to end in each of the three legs. So a
harness leg now ends about 2,500 s after that file starts, and when it starts depends on the
seed's order of files: that is the spread from 2,571 s to an estimated 3,882 s. Pull request
#4236 adds harness rows whose real-time waits take minutes: its 9 legs in the window took 1,651 to
2,301 s, and round 3 of its later run 44740717331 took 2,375 s. It sets its own harness budget,
and 2,375 s is inside the deadline set here. If it merges, its harness legs come into the window
and set the harness budgets again by the rule.

Pull request #4523 sets its own web and web-cold budgets and changes the lanes' tests, so its runs
are not in the window either. Its run 44922135913's web-cold legs took 169, 169 and 185 s, and
185 s is above the 175 s the web-cold deadline rests on: 360 s still leaves 1.95 times it. If
#4523 merges, its web-cold legs come into the window, and by the rule 2 × 185 s sets a 420 s
deadline.

The other steps took at most 37 s in a web leg (run 44751162563, round 3), 44 s in a web-cold leg
(run 44814644776, round 1) and 229 s in a harness leg (run 44709413418, round 1, most of it zsh's
install), and at least 15, 13 and 28 s.

The window grew nine times after a commit had set budgets from it:

- the runs at the pull request's first head read a web leg of 1,661 s, which its second head took
  in;
- the runs at its second head read a web leg of 1,954 s and a harness leg of 1,496 s, which its
  third head took in;
- the runs at its third and fourth heads read a web-cold leg whose other steps took 32 s and a
  harness leg of 1,565 s, which its sixth head took in;
- the run at its sixth head read a web-cold leg whose other steps took 44 s, which its seventh head
  took in;
- the pull request's own run at its fourth head, 44796938761, read a web-cold leg of 169 s, which
  `b6160faaae` took in, with the runs at its seventh and eighth heads;
- #4615's merge and the five runs of 2026-11-21 read a harness leg of 1,642 s and a web-cold leg
  estimated at 173.9 s, which `62898d3adc` took in;
- the run at `3989f41d6a` read a harness leg of 1,649 s, which the next commit took in;
- the run at `b23e61c369` read a web-cold leg of 175 s, above the 173.9 s estimate, which the next
  commit took in, with the run at `3ff830bb16`;
- the run at `0682934fca`, the first whose merge holds #4659, read harness legs of 2,571 and
  3,241 s and one stopped at 3,300 s and estimated at 3,882 s, which the commit after the merge of
  main took in.

The first three times, a lane's budgets no longer kept the rule over every leg read, and the head
that took the legs in set every lane's budgets again by the rule. The next two times the rule
still gave the same budgets: 8 minutes and 2 × 44 s come to 568 s, under the web-cold job's 10
minutes, and 2 × 169 s is 338 s, which rounds up to the same 360 s deadline. The sixth time it
set the harness budgets again, to 3,300 s, 57 minutes and 65 minutes from 2 × 1,642 s, and kept
the web-cold ones, since 2 × 174 s is 348 s, which rounds up to the same 360 s. The seventh time
it kept them all: 2 × 1,649 s is 3,298 s, which rounds up to the same 3,300 s. So did the eighth:
2 × 175 s is 350 s, which rounds up to the same 360 s. The ninth set the harness budgets again, to
7,800 s, 132 minutes and 140 minutes, from 2 × 3,882 s.

The largest job budget, 140 minutes for a harness leg, is far inside GitHub's own limit for a job
on a GitHub-hosted runner. GitHub's "Actions limits" page
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

A run that passes takes what its rows take: the 31 runs read cost 115 to 242 billed minutes each
(each leg's job rounded up to a whole minute; run 44963607247 cost 242), and $0,
since standard runners are free for a public repository. The budgets bound only a leg that hangs:
at most 70 minutes for a web leg, 10 for a web-cold leg and 140 for a harness leg.

### When to measure again

When a leg's report says it was stopped, and the file it names passes in the legs that ended by
themselves, the suite has outgrown the deadline: read the legs again as above, add them to the
window, and set the matrix's measurements and budgets from the new reading by the same rule. A
leg that ends by itself above a lane's `slowest_seconds` is read the same way: it moves the
slowest, and the budgets follow it, so a budget never rests on a margin its own window breaks.

## A leg's deadline, and what a stopped leg names

GitHub's runner ends a step that reaches its `timeout-minutes`, or that a cancel of the run
reaches, with signals to the step's top process alone: SIGINT, SIGTERM 7.5 s later, and SIGKILL
2.5 s after that (actions/runner, `src/Runner.Sdk/ProcessInvoker.cs`, `SendSignal`; a `run:` step
takes that path, since `ScriptHandler.cs` starts it with `killProcessOnCancel: false`). The top
process of a `run:` step is the bash that runs the step's script file, which passes no signal on
to a child it waits for. So before this deadline the tool never learned of a budget's end: no
report named a row, and the 42 legs the old budgets stopped before Bun ended, in the reading
above, wrote no report at all. So the step's own deadline comes first, inside its budget, the
tool is told of it, and the runner's own signals reach the tool too:

- The step runs `exec timeout --foreground --signal=TERM --kill-after=60 <deadline_seconds> bun
  run test:repeat ...`. At the deadline `timeout` sends SIGTERM to `bun run` alone, and not to its
  process group (`--foreground`), so Bun's workers are left for the tool to stop. `bun run` passes
  the SIGTERM on to the tool, through the script's `&&` too (measured on Bun 1.4.4: a script
  `true && bun <file>` under `timeout` got the signal in that file's process). `exec` makes
  `timeout` the step's top process, so a signal of the runner's reaches `timeout`, which passes it
  on to `bun run` in the same way. Measured on 2026-11-21 with the step's text in a script file
  under `bash -e`, as the runner runs it, and SIGINT to that bash alone once a spinning row had
  begun: with `exec` the tool named the row after 0.4 s and the step ended 0.9 s after it began;
  without it nothing reached the tool until its 30 s deadline.
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
  ran in, under five seeds). The tool ends that run once it has read the names of the rows the
  workers had begun, so the rest of the file, a hook that hangs after the row included, decides
  nothing. That run still runs the file's own code and hooks, so it runs in a process group of its
  own too, which the tool kills whole once it has read those names, at the end of its bound where
  the run never prints them, and again once it has ended.
- The report, the step summary and one `::error` for each such file name the file, the run of it
  and the row of that run by its place and its name, and give the command that runs the file again
  in that order. The report also says how Bun and each names run ended against their bounds. The
  tool exits 1, and `<out>/progress.jsonl` stays in the leg's artifact.
- The tool listens for SIGTERM and SIGINT until its report is written, not only while Bun runs.
  A signal with no listener ends a process at once, so a deadline that came after Bun had ended,
  while the tool read Bun's results and wrote its report, would end the tool with no report; a
  review of the pull request that set these budgets found that window. Such a stop has nothing
  left to stop: the tool keeps every result Bun wrote, and writes the report again with one more
  failure, which says the leg was stopped after Bun had ended, while the report was written. The
  listener of a signal that lands during that synchronous work runs on the next turn of the event
  loop, which the tool takes once the report is written (on Bun 1.4.4, a SIGTERM another process
  sent during synchronous work reached its listener before the first `setImmediate` after it, in
  30 of 30 runs). A stop after that turn leaves the report as written, whole.
- A cancel, or a step budget's end, reaches the tool as the runner's SIGINT: a stop like the
  deadline's SIGTERM. The runner's SIGTERM 7.5 s later is a second signal, which the tool passes
  over. In the readings below Bun ended at most 966 ms after a stop and a names run took at most
  4,178 ms, so such a report comes about 5 s after the SIGINT, inside the 7.5 s. Where Bun or a
  names run runs to its bound (10 s, 20 s), the runner kills `timeout` 10 s after its SIGINT, and
  the step ends while the tool, its grandchild, goes on and writes its report: the step's log then
  need not hold the tool's errors, and the "Upload the report" step, which runs after a cancel too,
  holds the report only where the tool wrote it first. `unit-repeat.test.ts` sends the SIGINT to
  the workflow's own step text under `bash -e`, as the runner does.

### When Bun ends early, with no stop

A worker, or Bun's test process itself, killed from outside Bun, as the out-of-memory killer
kills the largest process, ends Bun early with no stop. Bun 1.4.4 then gives the worker's file a
`(worker crashed)` testcase and goes on, or, when its test process is killed, writes no junit
file at all; neither names the row that was running. Bun runs a lane of two files or more in
workers (`bun test --test-worker`) and a lane of one file in its test process (measured on
2026-11-21). So where Bun ended by a signal, wrote no junit file the tool can read, or wrote a
`(worker crashed)` testcase, the tool reads the progress file once Bun has ended, and names each
file whose worker's last load had a run of it not ended, by its run and its row, with a names run
as after a stop: in the report, the step summary and one `::error` each, titled "Running when its
worker died". The file's `(worker crashed)` testcase makes no failing row of its own. The progress
file stays in the leg's artifact whenever the leg fails; only a leg that passed with no stop,
whose junit file holds every row's result, removes it.

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
- A file of a worker that was gone at the stop is not named as running. Where the progress file
  shows that worker in a run of the file it had not ended, the worker died before the stop, a
  crash of its own or a kill from outside Bun, and the report names the file as one its worker died
  in, by its run and its row, as Bun's early end does above. Bun's junit file gives a
  `(worker crashed)` testcase to each file whose results it had not taken in when the stop came:
  the file in hand, and also, measured on 2026-11-20, a file whose worker had ended its rows and
  exited a moment before. Except for a file its worker died in, the report lists each such file as
  one whose results Bun did not report, in the stop's own failure, and makes no failing row of it.

### The two bounds of a stop

A stop has two bounds of its own, each a hang guard that decides no verdict:

- the grace, how long Bun gets to end after the stop, before the tool kills its group: 10 s;
- the names bound, how long a run that reads a file's names gets to print the names the tool
  needs, before the tool kills its group: 20 s. The tool ends the run as soon as it has read
  them, so the bound ends only a run that never prints them.

A caller sets them with `DISPATCH_UNIT_REPEAT_STOP_GRACE_MS` and
`DISPATCH_UNIT_REPEAT_NAMES_BOUND_MS`, as `unit-repeat.test.ts`'s stop rows do, so that neither
decides a row's verdict; the one row that holds a names run which can never print to its bound
sets that bound to 2 s. Each default is at least three times the largest reading of its kind.

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
- **Inside the kill-after:** the grace, the 2 s the tool then gives a pipe to close, the names
  bound and the 2 s it gives that run's pipe come to 34 s, inside the 60 s that the step's
  `timeout --kill-after` gives the tool after the stop.
