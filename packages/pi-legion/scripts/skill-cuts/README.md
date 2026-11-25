# skill-cuts

The runner that tests whether a cut of a skill's text keeps what agents do with it. Run every command
from `packages/pi-legion` as `bun scripts/skill-cuts/cli.ts <command> ...`.

A replayed request is sent as a real session sent it, and nothing of the run that made it may
change what the agent does. Two such things did, and the runner now removes both:

- **The request id.** A body whose `metadata.user_id` (or, on the OpenAI route, `prompt_cache_key`)
  stays the same from run to run moves the pass rate with the batch it is sent in. One body, sent
  interleaved to one gateway deployment, passed 8 of 30 runs with one fixed id, 26 of 30 with
  another, and 18 of 30 with a fresh id for every send. So the runner draws a fresh id for every
  run (the same id in each round of one run) and for every judge call, sends the runs of all the
  arms of a round in one seeded order, never one arm's batch after another's, and checks each
  skill with a false-loss control (`behaviour control`).
- **The harness strings.** A captured body named the capture's own scratch directory, which named
  the skill under test and the cut, in Oh My Pi's date and working-directory reminder and in the
  context files' paths. On one case, with that path the agent read the skill's reference in 12 and
  33 of 40 runs; with a path that named no skill, in 0 and 1 of 40. So every string of a captured
  body that comes from the capture and not from the session is replaced by the session's recorded
  value, or by a neutral one where the store holds none ("The harness strings", below).

Every private value (the owners, the session stores, the gateway's address, the paths left out, the
repositories copies map to, the dev box's Claude Code config directory, the clones made by hand on
a dev box, the skill sets and the repository that pins them) comes from the `--config
<skill-cuts.json>` file at run time (`config.ts` says each key). No file here names one. The
gateway key comes from `--key-file`, read inside the process that uses it, never printed, logged
or written. The behaviour test and the load test read the skill texts from directories and the
cases or tasks from a file outside the repository. The repositories a context needs come from
`--repos <dir>`, a tree of `<owner>/<name>/<commit>/` directories, each the repository at that
commit; without `--repos` they are read from GitHub through `gh` (GET calls and GraphQL
queries; a whole repository's tarball only for a plugin's checkout). A skill's directory is read
from its commit's tree and its blobs; where GitHub cuts that tree short (a repository of too many
entries), the directory alone is read, along its path from the root tree, one level at a time
where GitHub cuts its own tree short too, never from the whole repository's tarball. A skill
directory the repository holds as a symbolic link (a project's `.claude/skills/<name>`, for
example) is read at the directory the link names in the repository, as Oh My Pi's scan follows the
link (discovery/helpers.ts, lines 579-580 and 594-596). A link out of the repository, or to no
directory of it, drops the case or the task as `unreadable-directory`, and the drop names the case,
the link and its target. The one-level tree reads of such a read are kept while they are among the
last 64, so the levels read to find the link are not read again. So
`behaviour` and `load` need no GitHub token only with `--repos`. `copies`, `set-count`,
`craft-lines`, `build-cases` and `build-tasks --profiles-out` read GitHub through `gh`, and only
read it: GET calls, and GraphQL queries that read file contents in batches.

## Commands

### `behaviour screen|confirm|control|report`

```
bun scripts/skill-cuts/cli.ts behaviour <screen|confirm|control|report> \
  --config <skill-cuts.json> --key-file <file> --out <dir> \
  --old <skill dir at the cut's base> [--new <skill dir in the cut> | --ours <skill dir of ours>] \
  --cases <case file> [--case <id>] \
  [--planted-line <text>] [--budget-usd <n>] [--concurrency <n, default 8>] \
  [--after-case <command>] [--request-log <file>] \
  [--models <models.yml>] [--omp <binary>] [--repos <dir>] [--hint-model <model>] \
  [--omp-config <file>]... [--cache-dir <dir>] [--scratch <dir>] \
  [--reading-models <file>] [--limits <file>] [--build-summary <file>]... \
  [--again <case id>]... [--confirm-flagged] \
  [--allow-runners <digest>[,<digest>...]]
                                 (--again, --confirm-flagged: confirm only; --again not with --case)
```

One skill per run: `--old` is that skill's own directory (the old text: the skill at the cut's
base, or a set's skill, their skill), and `--cases` holds that skill's cases (`cases.ts`), one
JSON object per line. The old text is compared with one arm, the
compared arm: `--new` (the skill's directory in the cut), `--ours` (a skill of ours, "The arm of
ours" below), or neither, which compares the old text with no skill. `--new` and `--ours` exclude
each other (exit 2). Each stage records its compared arm (`compared`; a stage that records none
was run with `--new` before it was recorded), and a command whose `--out` holds runs of another
compared arm is refused (exit 2). A case is informative when old passes at least 2 more of its
10 screen runs than no skill; it is flagged when it is informative and old passes at least 2 more
than the compared arm (with no skill compared, that is informative); and a confirmed case is a
loss when the compared arm passes at least 10 fewer of 40 fresh runs than old.

- `screen`: per case, the three judge controls (the real step must pass; the case's
  `violatingStep`, a step that breaks the line, must fail; "I will continue." must fail; a case
  that fails any is dropped, naming the control), then a capture of each arm (old, the compared
  arm unless it is no skill, and none), 10
  replays of each, each reply judged blind with all arms shuffled, and runs made again for the runs
  with no verdict, at most 5 more per arm. The session cases (`source.kind` `session`, or none)
  are screened first, in the file's order, and the written cases (`build-cases --written`) after
  them: at most 15 screened cases of each kind, until the skill has 6 informative cases in all. A
  case whose model the gateway does not serve (none of the configuration's `servedModels` has its
  id, the text after its last `/`) is dropped before any call as `model-not-served`; a case on a
  served model runs as that served `provider/model`, so an Anthropic model behind another
  gateway prefix runs as `anthropic/<id>`. A case whose listing measure (its `listing measure`
  evidence item, under `build-cases` below) misses more than 10% of the names its session's
  recorded listing holds is dropped before any call too, as `listing-misses`, with the count and
  the names. With `--reading-models` (the skill's
  `<cut or set id>/models/<skill>.json` that `attribute` writes: its reading sessions by model),
  a skill with fewer than 6 reading sessions on models the gateway serves gets no session case
  screened: with no written case it is refused before any call (exit 2), it gets no verdict, with
  the reason `too few sessions on served models` and its served and total counts, and the walk of
  the cut's ranking goes on to the next skill; with written cases, the screen says the session
  cases are not screened and screens the written ones, and the skill's verdict, or its reason for
  none, is the written cases', never that refusal. A session case whose screen began before the
  refusal applied is passed over too: its runs count for nothing, and `confirm`, `control` and
  `report` take the screen as finished without it.
- `confirm`: 40 fresh runs of old and the compared arm for each flagged case that is not dropped.
  Each confirm records the runner that made it: a digest of the runner's code (each file of this
  directory but its tests, its test data and this README). Where a case's last two confirms at the
  same texts on one runner disagree, one a loss and one not, `confirm` runs a third, and the three
  are pooled (the architect's decision "The behaviour test's pass rule"): the case is a loss when
  the compared arm passes at least 30 fewer of the 120 runs than the old text, the same bound of 10
  for each 40. Confirms made on different runners (before and after a fix) or at other texts are
  not pooled: only the last confirm's runner and texts count. `--again <case id>` asks for one
  more confirm of a flagged case whose confirm is finished, on this runner; it is refused (exit
  2), naming the case, where the case already has three confirms at these texts on this runner
  (no fourth runs) or two that agree (a third runs only where two disagree). `confirm --case
  <id>` makes 40 fresh runs of old and the compared arm on that one case, with no flag gate and
  no gate of 6 informative cases, and no screen or judge controls: give it its own `--out`. It
  refuses an id the case file does not hold, a case whose model the gateway does not serve, and
  `--again` beside it (exit 2).
  `--confirm-flagged` confirms each flagged case that is not dropped even where the skill can get
  no verdict (fewer than 6 informative cases, a control that failed, or no case left to be the
  control case), where `confirm` without it makes no run. It is the route for a one-case rerun,
  a case another tree asks to run again: a case file of that case alone, `screen`, then
  `confirm --confirm-flagged`. It runs the same confirm stage (40 fresh runs of old and the
  compared arm; a third confirm where the last two at the same texts on one runner disagree, and
  the three pooled), and `--again` works with it as without it. It never confirms a case the
  screen did not flag. Each confirm it runs records it in the case's results (`confirmFlagged`),
  so `report` needs no option to know it: the report of such a run gives the case results as any
  report does, and as its verdict line
  `verdict: case result only; no skill verdict stands (--confirm-flagged)`, never a skill verdict.
  The other commands refuse the option (exit 2).
- `control`: on the skill's control case, 40 runs each of three arms: old; `minus` (old without
  each line of the case's file that is the case's line, trimmed, or without each line that is
  `--planted-line`; a rule written twice is still the rule, so every copy goes; a link in the old
  directory is copied as the file it names); and `twin` (old again, with a capture and runs of its
  own). Two controls are scored on these runs by the confirm rule (a loss when the other arm
  passes at least 10 fewer of 40 runs than old). The planted-loss control, old against `minus`,
  must be a loss, or the test could not see a loss. The false-loss control, old against `twin`,
  must be no loss, or the test calls a loss where the texts are the same. A skill whose planted-loss
  control is no loss, or whose false-loss control is a loss, gets no verdict, and the report says
  which control failed.
- In every stage, a case one of whose captures needs a directory no read can give (the commit
  holds no such directory, or GitHub cuts a level of its tree short even read alone) is dropped
  as `unreadable-directory`, its drop naming the repository, the commit and the path; a run
  started again skips it. A stage left unscreened (an arm lacks its judged runs after its 5 extra
  runs) counts for nothing in the score, unless its runs show it can never be judged: an arm's
  runs that read skill files past the last answered round drop its case as `reads-past-last-round`
  (below); else a stage in which the provider refused, on a content policy (a stream error whose
  type ends in `_policy`, such as `cyber_policy`), every replay that got no verdict in each arm
  that lacks its judged runs drops its case as `provider-refused: provider refused every replay of
  <arms> (<policy>): <n> refusals (old <a>, new <b>, none <c>)` (the architect, 2026-11-24): the
  refused arms joined by "and" (`old and new`), none named where every arm of the stage is
  refused, the policies in the order first seen, and the refusals counted for every arm of the
  stage, 0 where it has none. An arm with its judged runs blocks nothing, so a screen whose old
  and new arms the provider refused is dropped though its none arm was judged. A stage saved
  unscreened in that shape with no drop, from before this rule, is dropped when `screen`,
  `confirm` or `control` reaches its case again, from its saved runs and with no model call. A
  stage where an arm that lacks its runs has a run with no verdict for another reason stays
  unscreened. Each drop records the stage that made it (`droppedAt`; a result written
  before that field is read as dropped at screen when its screen runs are not counted, else at
  confirm when its screen flagged it and it has no confirm runs, else at control). No dropped case
  leaves the score (the architect, 2026-11-22): a case dropped at screen is not informative, and
  not screened; a case dropped at confirm or control keeps its screen's passes, and any confirm
  made before the drop. A flagged case that can no longer be confirmed leaves its skill no "no
  loss detected" verdict: the skill gets no verdict, `flagged case <id> could not be confirmed
  (<reason>)`, unless another of its cases is a confirmed loss, which stands as the skill's loss.
  A dropped control case gives its place to the next informative case of the control rule's order
  (the largest old − none gap, a tie to the lower id), chosen by that rule and by no result; with
  none left, the skill gets no verdict, and the reason names each dropped one. A drop at confirm
  of a case its screen did not flag cannot happen, since only flagged cases are confirmed: results
  that show one are refused, naming the case, by `report` and by every stage command (exit 1), not
  scored. A read that fails for another reason, after its retries, fails the command (exit 1), and
  the next run reads it again.
- The runner of each run. Each stage and each run records the runner that made it: a digest of the
  runner's code (each file of this directory but its tests, its test data and this README; the
  commit is not knowable in a copy of the directory with no git). A run saved before runs recorded
  it takes its stage's runner, and a stage saved before stages recorded theirs is the runner
  `unrecorded`. `screen`, `confirm` and `control` take a saved stage of their own kind (a case's
  screen; a flagged case's last confirm, or with `confirm --case` that case's confirm; the control
  case's control) as done, or go on with one still running, only when each of its runs, or the
  stage where it has none, is this runner's or
  that of a runner `--allow-runners <digest>[,<digest>...]` names. A saved stage of another runner
  is refused before any model call (exit 2), naming the case, its runner and this runner: it is
  never skipped as done and never made again. Confirms are pooled on one runner at one set of
  texts as above, whatever `--allow-runners` says.
- `report`: for each stage kind (`screen`, `confirm` with earlier confirms, `control`), each
  runner with its case stages and its runs (a stage whose runs two runners made counts as a case
  stage of each); results whose runs more than one runner made are refused (exit 2), naming each
  runner with its counts, unless `--allow-runners` names each, and the runners line then says
  they were allowed. Then the compared arm; each case's passes and runs per arm and stage; for a
  dropped case, the stage that dropped it, why, and the passes of each stage it ran (its screen's
  passes where the screen ran); for a case with more than one confirm, each confirm numbered (one
  on another runner or at other texts marked not counted) and, for three pooled ones, their pooled
  passes,
  and a `pooled confirms` line naming each such case with its three results and the pooled
  numbers; the cases of each kind (`source.kind` `session` or `written`; a case with none is a
  session case) and the informative cases of each, the control case's passes of old, `minus` and
  `twin` and what each control says, and each dropped case the control case took the place of;
  each model of the cases with its cases, the skill's verdict and its reason (a run with a confirm
  made by `--confirm-flagged` gets the verdict line under `confirm` above in its place), ending
  with how many of the skill's screened cases have a listing that could not be measured (no
  listing measure, or one that says so); the drops per judge control and the drops counted as
  `model-not-served`, as `unreadable-directory`, as `listing-misses`, as
  `injection-unrebuildable`, the last by kind (a case under each kind it holds), as `path-form`,
  by the arm whose text holds the file, as `reads-past-last-round` and as `provider-refused`; the
  verdict line also counts the screened cases whose profile took names from their recorded
  listing (`fromRecorded`, below); an estimate of the procedure's
  false-loss chance at the cases run (exact binomial under identical old and compared texts, the
  pass rates taken from each case's own screen; its assumptions are printed with it),
  a table of the chance it finds a loss of 10 to 40 points in one informative case, each screened
  case's smallest drop ("The smallest drop" below), the spend at list price (below), the peak
  memory, how many captures' system-prompt digests differ from their session's, and the text of
  `--limits <file>` as it is (the limits a result states, so no limit
  is written into the runner), or that none was given. A screened case is one whose screen runs
  are counted and that is not dropped at screen. Beside the verdict, as descriptions that
  change no rule: each kind's screened cases (their ids), how many are informative, and each
  screen arm's passes summed over them; the screened cases whose real step reads a file of the
  skill other than SKILL.md (`cases whose real step reads a file of the skill other than SKILL.md,
  which the arm with no skill does not have, so it fails them by construction: <n> of <screened>
  screened (<id> reads <path>; …)`, or `none of <screened> screened`), a read being a `read` of
  `skill://<skill>/<path>`, a whole read by path of the case's own copy (`pathCopy`; a relative or
  `~` path resolved against its `cwd` and `home` as Oh My Pi's read resolves it), or, for a case
  that names no copy, a read of a path that ends with the skill's installed directory and
  `/<path>`, `<path>` a file of `--old` (each file at any depth, a symbolic link to a file among
  them, as `readers.ts`'s `skillFilePaths` lists them, the listing `--drop-navigation` reads too),
  as the arms tell a read of a file of the skill (`cases.ts`'s `stepSkillFiles`); the arm with no
  skill, in the screen whatever is compared, fails
  such a step whatever the skill's text teaches; a screened case whose arms the runner cannot
  build (`buildArm` throws) is named in it
  as `<id>: its reads cannot be told (<the first line of why>)` and counted in neither, and the
  line gives how many of the other screened cases are informative, flagged and confirmed losses;
  the mean over the screened cases of old's screen pass
  rate less the compared arm's, over all of them and over each kind's, with a 95% interval
  clustered by case (each case's difference is one observation, so the interval is Student's t
  over the cases; one case has none, and a kind with no screened case has no mean); and how many
  screened cases tie (old and the compared arm pass alike), floor (0 of 10 in both), ceil (10 of
  10 in both) and reverse (the compared arm passes more than old), a floor or a ceiling being a
  tie too. Beside them, the inspection floors (`inspection floors (screened cases on which at
  least 8 of the 10 screen runs of each screen arm are judged fail on a step whose own tool calls
  are all read-only calls; …): <n> of <screened> screened (session <a>: <ids>; written <b>:
  <ids>)`): a run's own calls are its judged reply's, less each answered round's calls, and must be
  one or more, each read-only by the rule below ("Read-only calls"); and the mean of old less the
  compared arm in screen pass rate over all screened cases and over the screened cases that are
  not inspection floors (named). No case is dropped for being a floor. With `--build-summary <file>` (report only, repeatable: the summaries of
  `build-cases --drop-navigation`, of the session form and of the `--written` form), it prints
  beside the verdict, as a description that changes no rule, `navigation lines of the skill that
  no case tests (from the build summaries): <file>:<number> "<text>"; …`: each navigation line
  of those summaries' `navigation.lines` (each file and number once) whose file and text, trimmed,
  no case of `--cases` has as its line (`baseFile` and `line.text`), or `none`; and after it
  `navigation drops with a replacement case: <n> by the build summaries, <m> after the judge
  controls (<id> dropped by the judge controls; <id> not screened yet; …)`: n the replacement
  cases the summaries' session records name (each `navigation` entry's `replacement` id, each case
  once), m those whose result a judge control did not drop at the screen (a case with no result
  yet counts, as the build counted it), each case the controls dropped and each with no result
  named (no parentheses when there is none); without the flag it prints neither line. A file that
  is not a build-cases summary naming its navigation lines (one of another command, or one written
  before build-cases named them) is refused (exit 2), as is the flag on a stage command. It names
  the judge: the SHA-256 of the bytes of `prompts/judge.md` the process read (once, the text every
  judgement sends) and the settings its header pins, and how many stages of the results record
  that judge, another one, or none. Each stage records
  the judge it was made with (`judge`) before its first judgement; a results file made before
  that records none and still loads. A skill whose planted-loss control is no loss gets the reason
  "deleting the case's line showed no detectable change (planted-loss control not a loss)" (with
  `--planted-line`, the planted line in place of the case's), and one whose control's runs are
  unscreened "planted-loss control not a loss: its runs are unscreened". It calls no model, and
  needs only `--config`, `--out`, `--old` and `--cases`; `--request-log` names the log it sets
  the spend against, `<out>/requests.jsonl` by default; its compared arm is the one its results
  record (a `--new` or `--ours` it is given must be that one). With the cut compared, `keeps`
  prints as "no loss detected by the registered rule" and a loss names the cases where the cut's
  text passes at least 10 fewer of 40 fresh runs than the old text, and those where it passes at
  least 30 fewer of 120 over three pooled confirms. With no skill compared, the
  report says once
  that the arm with no skill informs and clears nothing; `loss` prints as "no skill loses against
  their skill on <n> cases" and `keeps` as "no loss of no skill detected by the registered rule".
  With ours compared, it calls the arm of ours a bundle where its text replaced a skill of our name
  that a case's profile held ("their skill hidden, ours at the given text (it replaced the
  profile's own <name>)"), names on each case's line the history reads the arm of ours removed,
  and gives ours against old twice: over all screened cases, and over the screened cases with no
  removed read (each arm's screen passes, and the confirm passes and the count of losses where
  confirm runs were made, three pooled confirms counted as their 120 runs). With
  `--reading-models` it also prints each model of that file with its sessions and whether the
  gateway serves it, the sessions whose file names no model and those in no models query's rows,
  and the served models the screened session cases ran on, each with its cases; a verdict then
  ends with the reading sessions it speaks for, on each served model its screened session cases
  ran on (a model counted under the served model of its id, so `gateway/claude-sonnet-5-5` under
  `anthropic/claude-sonnet-5-5`), and those it does not speak for, on models the gateway does not
  serve and on served models no screened session case ran on. A written case is a written task in
  a recorded session's context, never a reading session: the report counts the screened written
  cases on a line of their own by the model each ran on (`written tasks: <n> on <model>, …`), and
  a skill whose screened cases are all written speaks for no reading sessions, which its verdict
  says. A skill too few of whose reading sessions are on served models, and with no written case,
  gets no verdict. Without `--reading-models`, it says the reading sessions by model were not
  given, and the verdict names no models.

The report's spend at list price is the sum of what the case results in `<out>/cases` record
their calls cost, each call once, and the report prints each part of it: the judge controls (the
gateway's spend over each case's three controls), and for each stage kind its stages' dollars
(each stage's `usd`: the gateway's spend while the stage ran, in each process that ran it), split
into the replays and the judgements its runs record (the runs with no verdict, which are run
again, among them) and the calls no run records (its captures' hint calls, the rounds a replay
that then failed had answered, replies whose judgement the budget refused). It is the spend of the
results in `<out>/cases`, not of every command run on `--out`: the calls of a round stopped
before it was saved, of a result taken out of `<out>/cases`, or of a capture that failed are in no
result, and the `done` line of a stage command gives the calls of that command alone. So where
the request log exists, the report prints the log's total by kind of call and its difference from
the results' sum in two parts that sum to it, each with its cause: the judge calls against the
results' judge controls and judgements, and the replay and hint calls against the results'
replays and the calls no run records. With them it counts the replay calls of the log whose
session id no run of the results holds (a run records the id its replay went with), and the runs
of the results whose session id no replay call of the log holds (results made with another
`--request-log`, or brought from another `--out`).

`score.ts` holds the rules. Each case's results are one file, `<out>/cases/<case id>.json`,
written whole after each round of runs, so a run started again goes on from what was saved and
skips each complete case. Each run in it keeps its arm, its verdict, the session id it went with
(`userId`) and its place in its round's send order (`sendPlace`), so both controls can be
recounted from it. A round sends every arm's runs in one order, shuffled by the round's recorded
seed (`replay.ts`'s `sendOrder`); the first run of each arm in that order goes alone among its
arm's, so its reply writes the prefix cache the arm's later runs read, and no place of the order
changes. A results file whose stages were run before this (they record no `sends`) is refused:
use a new `--out`. `--after-case <command>` runs `<command> <that file>` when a case's stage
ends, and is ended after 10 minutes (`AFTER_CASE_TIMEOUT_MS`). Captures are kept under
`<out>/captures/`; a capture that is not there yet is made with Oh My Pi and needs `--models`:
`--omp`, else `$LEGION_OMP_PATH`, else the `omp` on PATH, taken as an
absolute path; a command given `--models` when none of them names one is refused (exit 2), and
`load` reads Oh My Pi the same way. A saved capture is read back and not made again; the
installed copies of the profile's other skills that its record names, which its replays answer a
read of such a skill from, are not in it but in the cache (`<cache-dir>/skills/<key>`), which is
not kept with `--out`. Before replay, the runner makes each missing skill copy again from its
repository at its commit, as the capture did. It does not build plugins, compose a role prompt
or read context files. If all copies are cached, the runner builds nothing and reads no
repository. If refilling the cache gives different copies from the saved record (the case's
profile names another commit or copy, or the record keeps a copy under another cache key), the
runner refuses the capture (exit 2, or that task's failure in `load`), naming the case, the arm
and the first copy that differs: make it again under a new `--out`. The run stops above 1 GiB
of peak resident memory, and no new model call starts once the replies cost `--budget-usd` at
list price.

The gateway serves two routes, and each case runs on its own model's: the Anthropic Messages
route (`/v1/messages`, `anthropic/<id>`) and the OpenAI Responses route (`/v1/responses`,
`openai/<id>`, today `openai/gpt-6.1-sol`). `--models` names both providers as the pod's
`models.yml` does (`openai` is Oh My Pi's built-in provider at the gateway's `/v1`, with its
models under `modelOverrides`); Oh My Pi 18.18.6 sends an OpenAI model's turn as a Responses body
(its system prompt as `instructions`, its history as `input` items; testdata F8), and the capture
keeps it as it does a Messages body. Hints are written by the hint model, which must be on the
Messages route: an OpenAI case needs `--hint-model anthropic/<id>`. A hint is the line of at most
160 characters that Oh My Pi lists a skill under in place of its description. A capture asks the
hint model once for each skill text its cache lacks, and every arm and case of that text lists the
cached hint. A reply Oh My Pi does not keep (empty, more than one line, or over 160 characters)
fails no capture. As in Oh My Pi, the skill is then listed under its cut-down description: the
description with each run of white space made one space and its ends trimmed, whole when it has
at most 100 characters; else its first sentence, when that sentence ends within the first 99
characters and has 40 or more; else those 99 characters cut before their last space when 40 or
more come before it, or else whole, and followed by `…`. The runner asks no more for that text,
because Oh My Pi asks that model no more either. The cache file and the capture record's `hints`
say `kept: false`, give the reply, and say why. The run that writes the missing hints (the hint
run) holds Oh My Pi's turn until Oh My Pi has stored the row of each in its
`skill-descriptions.db`, read as the turn waits, and ends the turn once it has. Oh My Pi asks for
at most 4 hints at a time and gives a request up after 30 s, storing nothing of it and asking no
more for it in that run, so the turn has no bound of its own as a whole: it waits as long as hint
requests and answers keep coming, and ends once 60 s pass with neither. The hint run's Oh My Pi
may take its time limit plus 30 s for each 4 hints owed. A hint Oh My Pi stored no row of fails
the capture, naming why from what the recorder saw (no request came, its answer had not come, or
its answer came after Oh My Pi's 30 s, as a gateway call tried again can), after every hint Oh
My Pi did store is cached, so a run made again asks for the others alone. A profile that
installs one text twice, once as `<namespace>/<name>`, lists it once: Oh My Pi admits the bare
copies first, then each namespace's, and drops a copy whose body and frontmatter are those of a
copy of its name it admitted before, so the capture lists that copy nowhere and owes it no hint.
Oh My Pi names each copy by its SKILL.md's frontmatter `name`, trimmed, else its directory's name,
and the runner reads every SKILL.md's frontmatter as Oh My Pi does (`common.ts`'s
`readFrontmatter`), `copies` among them, so each copy is installed under the name Oh My Pi lists it
under. A capture whose installed copy Oh My Pi would name otherwise (an arm text that renames the
case's skill, say) is refused (exit 2, or that task's failure in `load`) as the copy is installed,
naming the case, the copy's source and both names.
Oh My Pi asks for temperature 0, but sends no temperature to Opus 4.7 on, Sonnet, Fable and Mythos
5 on or Haiku 5.5 on, which take none, so their hints run at the API's default of 1. `gateway.ts`
sends each body to its API's route (the key as `x-api-key`, or as
`authorization: Bearer` on the Responses route), tries a call again on a 429 or a 5xx, a failed
connection, a timeout or a reply it cannot read (8 tries in all, 91 s of back-off), but never a
stream refused on a content policy (the error's type, a Responses error's `code`, ends in `_policy`,
as `cyber_policy`), which fails at once, since the provider refuses the same content on each try. It
reads the stream whole, and prices each reply at its model's list price (`PRICES`, from
Anthropic's and OpenAI's pricing pages; gpt-6.1-sol at $2 input, $10 output, $0.10 a cache read
and $2.50 a 30-minute cache write per million tokens, more above 272,000 input tokens), counting a
Responses reply's cached input and reasoning tokens too. A Messages body gets a cache breakpoint on
its last message; a Responses body gets none, since that route caches the shared prefix by itself.
The judge stays `claude-opus-5-5` on the Messages route and reads a Responses reply's text and tool
calls as it reads a Messages reply's, so its request names no model, route or arm.

A reply whose every tool call reads a skill file is answered and sent again, in the body's API
(`function_call_output` items on the Responses route), at most 3 rounds (`MAX_SKILL_ROUNDS`). A
skill file is a `skill://` read of any skill, a whole read by path of the cut's own copy (for a case
that names no copy, of a file under the directory its `skill://` reads show the skill installed
in: "A case that names no copy" below), or a whole read by path of another
skill the profile lists (a path that ends `/<name>/SKILL.md` or holds `skills/<name>/references/`,
as `count.sql` places a read); a `skill://` read, or a read by path of the cut's own copy or that
directory, with one line range (`:N-M`, `:N..M`, `:N+K`, `:N`, `:N-`; `arms.ts`'s
`readRange`) is one too, in every run, session and written cases alike, answered with the lines of
its range in Oh My Pi's range form: a `skill://` read under its `[Skill file: <path>]` line with
no row numbers, as a whole one, a read by path as the checkout's ranged reads below, each with a
line before the range (one that does not start at line 1) and three after it (one with an end),
then `[<n> more lines in file. Use :<next> to continue]` when lines are left (`skillFileRange`,
`pathRangeAnswer`); where the pin widens a range of a code file to the brackets of the blocks it
cuts, the lines alone. A read with another selector (`:raw`, a list, `:-N`) is no skill read. The
arm's skill is answered from the
arm (in the arm of ours, our skill; "The arm of ours" below); another listed skill from the copy
its capture installed (`<cache-dir>/skills/<key>`), the same text in every arm; in `none`, and in
the arm of ours, a `skill://` read of the case's skill from the copy of another source that the
arm lists under the skill's name (below), where it lists one, as another listed skill's; a skill
the arm does not install (the case's skill in `none` and the arm of ours otherwise, or a skill
the profile does not list) with Oh My Pi's `Unknown skill: <name>` text (a read by path of the
case's skill in `none` with `Path '<path>' not found`), and the run goes on.
The judged step is each answered round's read calls, without
their results, then the first reply that holds another call or none, so a case whose line governs
a read of a file of the skill is judged on that read. A reply after round 3 that still only reads
skill files gets no verdict and is run again, as any run with no verdict is. Each report counts
such runs by stage and arm, and a stage that an arm cannot finish for them drops its case as
`reads-past-last-round`, listed with its counts. Each run records each answered round's reads
(`skillReads`: the skill, the file, by `skill://` or by path, and whether the arm, the installed
copy or no copy answered it).

A written case's run (`source.kind` `written`, with a `checkout`) also answers a reply whose every
tool call is read-only ("Read-only calls" below) from the checkout of its context session's
repository at its commit, at most 5 rounds (`MAX_CHECKOUT_ROUNDS`) beside the 3 rounds of skill
reads; a skill read in such a reply is answered as above. A session case's run gets no such
answers. A written case with no `checkout` (one built before build-cases passed over a context
session in no checkout it knows, `no-checkout` below) answers none: its reply whose every tool call
is read-only, one of them other than a skill read, gets no verdict as `unanswered-read` (below),
its reason naming that call and saying the case has no checkout, and the run is made again; a
session case's run is judged on such a reply, as before. Every written case of one commit, in
every stage and every run that shares the cache, answers from one shared checkout,
`<cache-dir>/checkouts/<owner>/<name>/<commit>`, read-only once made and never removed by a stage
(cli.ts's `sharedCheckout`). It is made once: `git init`, `git
remote add origin https://github.com/<repo>.git` (through the pod's git; `<repos>/<owner>/<name>.git`
with `--repos`), `git fetch --depth=50 origin <commit>`, which brings the blobs of each of those 50
commits (an answer runs with no network, below, so it could fetch no blob the checkout lacked),
`git checkout --detach <commit>` (a whole clone of a large repository's history does not finish in
a stage's time: on a repository of 115,950 files the checkout took 547 s and 2.7 GB), and `git
remote set-url origin https://github.com/<repo>.git`, so `git remote -v` and git's errors name the
remote a session's checkout names, not `--repos`' directory on the runner; then every
symbolic link in it that resolves outside it or not at all is removed (`checkout.ts`'s
`removeOutsideLinks`; a link that resolves inside stays); where the case's context session ran jj,
the git checkout is then made a colocated jj repository (`checkout.ts`'s `colocateJj`: `jj git init
--colocate`, then `jj status`, whose snapshot of the working copy is taken once there; each runs in
the answers' sandbox, below, the copy writable there, with the runner's jj, found on its PATH); and
its marker (`<commit>.ready.json`: whether it is git, its depth, `"blobs": true` for a checkout
that holds its commits' blobs, the links removed, `jj` when made a jj repository, the seconds and
bytes of its making) is written last, so a half-made checkout is never used. A git checkout whose
marker records no blobs (fetched before with `--filter=blob:none`) is made again, and said so. A
written case records whether its context session ran jj (`checkout.jj`: build-cases reads it from
that session's transcript, a `bash` call whose command holds the word `jj`); a case written before
that record has none, and the screen then reads the case's own messages for a jj call, as it did. A
checkout made before a case whose session ran jj asks for one is made a colocated jj repository
then, once, under the same lock, and its marker written again; a `jj git init --colocate` that
fails is recorded (`"jj": {"why"}`), its `.jj` removed, and no jj command of a session that ran jj
is answered. A lock (`<commit>.lock`, the
pid of the run making it) keeps two runs from making it at once: a run that finds the lock of a
live process waits for the marker; one that finds the lock of a process that runs no more removes
it and the half-made checkout and makes it again, and says so. Its making is said once, with its
seconds and size, and its colocation once. Where a git step fails, the checkout is GitHub's tarball
of the commit (context.ts's tree read, kept in `--cache-dir`) copied there, shared the same way, and
its git and jj commands are not answered. Each stage records the checkout it answered from
(`checkout`: `{"git", "depth", "removedLinks"}`, and `"jj"`: `true` or `{"why"}`, once a case whose
session ran jj made it a jj repository or failed to): its history is 50 commits deep, so `git log`
shows at most 50 commits and then the shallow boundary, and `git status` shows a detached HEAD (the
session's branch is not known) and each link removed as deleted; git runs with
`GIT_OPTIONAL_LOCKS=0`, so `git status` writes no index there. A jj command answers as in a jj
workspace at the commit where the copy is a colocated jj repository, each run with
`--ignore-working-copy`, so it reads the snapshot the colocation took, takes no lock of the working
copy and writes nothing (answers given at once agree); and as jj does in a checkout that is no jj
repository (`Error: There is no jj repo in "."`) where the case's session ran no jj and no case made
it one, since the sandbox shows jj no directory above the copy (where such a checkout tracks a
`.jj` at its root, no jj command is answered: jj would answer from that store). The answers are
the pinned Oh My Pi's forms (`checkout.ts`): `read` as a whole read by path, its rows as the
case's reads by path show them, or `Path '<path>' not found`; a file that is no prose too (no
notebook, SVG or document), and one of 100 lines or more, which Oh My Pi may show summarized,
unsummarized in place of the summary, the same in every arm, its round's record saying so
(`"wholeText": true`: unsummarized text in place of a summary); a file of more than 300 lines as
Oh My Pi pages it unsummarized (`read.defaultLimit`): its first 300 lines, then `[<n> more lines
in file. Use :301 to continue]`; a read with one line range with the lines of its range (the hashline line,
a line before and three after, then `[<n> more lines in file. Use :<next> to continue]`; the
lines alone where the pin widens a code file's range to its brackets), and a range that starts
past the file's end with the pin's notice alone, no error (`Line <n> is beyond end of file
(<lines> lines total). Use :1 to read from the start, or :<lines> to read the last line.`; so is
such a range of the cut's own copy read by path, in every arm); a read of a directory with Oh My
Pi's listing (`.`, then each entry to two levels below it, hidden ones too and `.DS_Store` left
out, `- <name>` or `- <name>/` indented two blanks a level; a directory under it of more than 12
entries shows 11 of them, `- … <n> more` and its last; `(empty directory)` for none): Oh My Pi
orders each directory's entries by their time of change and shows each one's age, which a
checkout does not keep, so every entry is taken as changed at one time, by name, each line its name
alone, as Oh My Pi shows an entry with no age; a listing that would show `.git` or `.jj`, or a
ranged read of a directory, is not answered. `grep` and `glob` are answered in their tools'
forms (`glob`'s paths in path order, since a checkout keeps no times of change; `.git` and `.jj`
left out of both); a `grep` and a `glob` are each worked out in a worker thread, never the
runner's own, since grep's JavaScript regular expression may backtrack where Oh My Pi's Rust one
does not (`(a+)+b` over a long line), and glob's matcher of each path may take seconds for a long
pattern in a large checkout; past 30 s the worker is ended and the call is not answered. A `grep`
one test of whose regular expression over a line (or of its exclusions' over a path) takes more
than 100 ms is not answered either: JavaScript's engine stops a backtracking match after a count of
its steps and gives no match (`(a+)+b` over 40 a's and then `xab`, about 0.7 s), where Oh My Pi's
search finds one, so its `No matches found` would be wrong. Each file's whole text is tested first,
with the `m` flag, and its lines only where that test finds a match or takes more than 100 ms: a
match of a line is a match of the whole text (a look-around, which could look past the line, is
not answered), so no match is lost, and the lines of the many files with none are neither split
nor tested. A `grep` pattern may start with one inline flag
group `(?FLAGS)`, FLAGS one or more of `i`, `m` and `s` each once (`(?i)technique[- ]targeted`),
which Oh My Pi's Rust regex takes and JavaScript's does not: the runner takes it off and adds its
letters to JavaScript's flags (`i` as `case: false` does); a flag group anywhere else, a scoped one
(`(?i:…)`), a negated one (`(?-i)`) or one of another flag (`x`, `U`, `u`, `R`) is not answered.
A `grep` path may be a list joined by `;` (outside braces): its parts that
start with `!` and name no path of the checkout are exclusions (one with no `/`, or that starts
with `**`, matches at any depth; another is anchored where it resolves), and the rest are
searched, each on its own in their order, or as the one directory that holds the others, a part
the checkout lacks noted after the matches (`Skipped missing paths: <paths>`). A directory's
files are searched in path order, `node_modules` not entered; each match with one line of context
before it and three after, each line's trailing whitespace taken off, and a line past 512 bytes
cut as Oh My Pi's native search cuts it (a context line to its first 509 bytes and `...`, a match
line to a window around its first match marked `...` on each cut side, then ` [col <n>]`), with
`[Some lines truncated to 512 bytes]` after the matches where a match line was cut. A search of a
directory or a list keeps 20 matches of a file and shows 20 files a page, the next page by `skip`,
and a search of one file keeps 200, each with Oh My Pi's notices (`Showing files <a>-<b> of <n>.
Use skip=<b> for the next page, or narrow paths/pattern.`, `More than 20 matches in <files>; grep
keeps the first 20 per file when searching several. Search one file alone for up to 200.`, `More
than 200 matches; grep keeps the first 200 in a file. …`, `No more results (<n> files total;
skip=<k> is past the end)`). A `glob` path with no glob character is a directory, every file and
directory under which matches, or a file, which answers itself; a path that is a list joined by
`;` (outside braces) answers each of its patterns, a pattern whose directory is not there noted
after the paths (`Skipped missing paths: <patterns>`), and a limit above 200 is noted as clamped
(`Requested limit <n> clamped to the max of 200`). A `grep` or `glob` list joined by commas or
blanks, a `glob` pattern (or every pattern of a list) whose base is not there, a file under a
glob, a `grep` path (or every path of a list) the checkout lacks, and a `grep` exclusion of a
form the runner does not read (a class, an escape, a nested group) are not answered (the pin's
error is not rebuilt). `bash` runs in the checkout's sandbox (below), its working directory and
the checkout's root mapped between the session's paths and the sandbox's (`/mnt/checkout`), its
output as Oh My Pi's bash tool gives it, within 30 s of when its sandbox is made (its wall time
counts from then too; the wait in the queue, below, is no part of either). An output past 50 KB,
one with a line past 768 bytes, and one of 1,000 characters or more
that Oh My Pi's minimizer may rewrite (a command whose top level is simple commands joined by
`&&`, `;` or `||`, one of which, unpiped, runs `ls`, `tree`, `find`, `grep`, `rg`, `wc`, `cat`,
`stat`, `git` or `jj`; not one in a group or a subshell) are not answered: Oh My Pi shows each
with a notice that names the artifact the session saves the whole output to, whose id is the
session's own count of its artifacts, which a checkout cannot know. Every path an answer shows is
the session's: the copy's path in the sandbox, and its own, as given and as its real path, become
the session's checkout root in every output, so `pwd` gives the session's working directory,
`git rev-parse --show-toplevel`, `git worktree list` and `jj root` its checkout root, and an
error message the session's path; a test holds that no answer of a fixture run shows the copy's
path. The calls are the model's, run on the runner's machine, so none may read a file outside the
checkout: with no link out of it left, `cat` of a link, `grep -R`, `rg --follow`, `find -L` and a
`read` follow no link out, and no option that follows links is refused. Beside that, a `read`,
`grep` or `glob` whose target resolves outside the copy is not answered, nor is a `bash` command a
word of which may lead outside it: a word whose expansion the runner cannot place inside the
checkout (it follows a variable the command assigned, `$PWD`, `$(pwd)`, `$(git rev-parse
--show-toplevel)` where no directory between the working directory and the root holds a `HEAD`
(git dies at a directory the checkout lays out as a bare repository, and prints nothing) and, in a
colocated copy, `$(jj root)`; not one assigned only in a branch of `||` or `&&`, or from another
substitution), a word naming `.git`, an absolute path off the checkout's root (one under `/proc`
among them), a relative one that leaves the root from any directory the shell may be in there, an
option's value that does (after a long option's `=`, or any rest of a short one's word after one of
its letters, so `grep -f..` and `git ls-files -X..` too: an option that reads one file, as grep's
`-f` and `--exclude-from`, rg's `-f` and `--ignore-file`, git's `grep -f`, `ls-files -X` and `blame
--contents`, has its value read as a path), or a path that leaves the copy as the kernel walks it
there, through a link that stays inside followed by `..` (`l/../x` through a link `l` to the root:
each part's links are followed, one part at a time, before the `..` after it); nor, of a word whose
unquoted text holds a glob character (`*`, `?`, `[`), a `cd` to it, one that bash may make into a
word starting with `-` (read with no matcher: a word whose first part holds a glob character,
which bash expands in the directory the shell is in, `nullglob` off, where a directory the shell
may be in holds any name starting with `-`, or is none the checkout holds; so `wc *`, `wc
[[:punct:]]*` or `cat *` beside a file named `--files0-from=.list` or `-n` are not answered, and
`wc opts/*` or `cat ./*` are), or a path whose glob part starts with `.` (bash matches a name's
leading `.` only so, `dotglob` off, so it may match `.`, `..` before bash 5.2, or `.git`: `cat
.gi*/config`) or comes before a `..` (`a/l*/../x`: the kernel walks the `..` from what the glob
matches, a link among them); nor a `cd` that names no
directory, which goes to `$HOME`; nor one that expands `$HOME` or `$PATH`, whose value would be the
runner's, or holds, in a word of any command (`echo`'s among them), an unquoted `~` at its start
or after its `=` or a `:`, where bash puts a home directory of the runner's (`~legion`, `~root`);
nor a `jj` command whose repository may be a `.jj` the checkout tracks (one between the directory
jj starts from, its `-R`'s or the working directory, and the root: in the sandbox jj stops there
at its configuration, which it cannot move to the read-only home, and such a store may name a git
repository outside the copy), nor any jj command of a checkout whose root holds a `.jj` that no
colocation of the runner's made (its session ran no jj: jj would answer from the store the
checkout tracks, where the session saw `There is no jj repo`). A quoted word is a literal
(a single-quoted one, or a double-quoted one whose every `$` is escaped and that holds no
backtick), whatever `$`, `?`, `*`, braces or `~` it holds. A `cd` of one word (a literal, no
option) to a directory the checkout holds, inside its root, each part of it before a `..` a
directory too (as bash's own `cd` walks it), succeeds, so where it surely runs (no `&&` or `||`
before it in its list) every later command of the call, until another `cd`, runs from there
alone, and a relative path is read from there (`cd <root>/pkg/src && …; cat ../package.json`
reads `pkg/package.json`); after any other `cd` (with an option, a second operand, a part before
a `..` the checkout lacks: `cd -x a/b`, `cd a/b -x`, `cd x/../a/b`, each of which bash refuses),
the shell may still be where it was, or, where the kernel's walk of the path as written ends
elsewhere (`cd -P`, or bash's own `cd` where its walk finds no directory), there. The kernel walks
a `..` from the directory the shell is in as it is real, and the runner from its path as written:
so where the working directory's real path in the checkout is not its path (the sandbox enters it
as it is real), or a `cd` may go through a symbolic link of the checkout, a later word that holds
`..` is not answered (`cwd` `a/l` through a link `l` to `e/f`: `cd ..; cat ../../x`), nor a command
that prints the working directory's own path (`pwd`, `$PWD`, `env`, `export`, `git rev-parse
--show-prefix`, `--show-cdup` or `--show-toplevel`, `jj root`): bash, git and jj give it from the
real directory (`…/e/f`), where the session showed the path as written (`…/a/l`).

The read-only rule decides which calls are answered; it does not make running them safe. So each
answered `bash` command, and each jj command of a checkout's colocation, runs in a sandbox
(`checkout.ts`'s `runSandboxed`): new user, mount, network, process and host-name namespaces
(`unshare -rmnpfu --mount-proc`); only the checkout copy of the machine's disk, read-only, at
`/mnt/checkout`; an empty, read-only file system over `/run`, `/var/run`, `/home`, `/root`,
`/legion`, `/tmp`, `/var/tmp`, `/dev/shm`, the operator's configuration, `/opt` (the pod's
installed tools, which no read-only program needs, with lock files any user may write; the
runner's jj is bound at `/mnt/jj` first), the runner's home, and
each directory that a variable of the runner's environment ending in `_FILE`, `_DIR` or `_HOME`
names (the model gateway's key, the Dispatch token, Envoy's and gh's configuration, the secrets
broker's keys and the store's address among them; a directory the sandbox's user cannot enter, as
`/root` may be, needs none; a file in a directory the sandbox needs is mounted over alone, and where
it cannot be, no command runs); no network; no capability, so
nothing it runs can unmount what hides a directory; an environment of only what it needs (`PATH`,
`LANG=C.UTF-8`, `HOME` an empty directory, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`,
`GIT_OPTIONAL_LOCKS=0`, git's `core.fsmonitor=false`, `core.hooksPath=/dev/null`,
`safe.bareRepository=explicit`, `protocol.allow=never`, `core.pager=cat`, `core.checkStat=minimal`
and `core.trustctime=false` through `GIT_CONFIG_COUNT` (the last two so git trusts the stat data
the runner's user wrote to the index: the sandbox's user namespace shows each file as owned by
uid 0, so git's default check would hash every file again on each call, and `git status` in a
checkout of 115,965 files ran past the time limit; git still compares a file's content where its
mtime seconds, size or mode differ, so no answer's text changes), and for jj a configuration that
names no external tool: no pager, its default diff formatter, no signature shown by its templates,
no file-system monitor, `signing.backend =
"none"` and each signing backend's program (`signing.backends.gpg.program`, `.gpgsm.program`,
`.ssh.program`) a path the sandbox does not hold, so a `-T` template that verifies a signature runs
no gpg or ssh-keygen); a time limit (30 s from the sandbox's ready marker), past which every
process it started is ended, and an output
cap, past which it is ended and its output not read. A command whose sandbox cannot be made is not
answered, nor one that runs a program (each but bash's builtins) on no directory of the sandbox's
PATH (a pod with no `rg`): bash's `command not found` is no answer the session saw. The sandbox's
fresh `/proc` still lists in `/proc/self/mountinfo` the mount points of the pod it is made from
(the credential mounts under `/run` among them, each hidden); no answered call reads it, since a
path under `/proc` is off the checkout's root. The programs the sandbox runs are the machine's own
(on this pod GNU coreutils 9.7, GNU grep 3.11, GNU findutils 4.10 and GNU sed 4.9), where Oh My
Pi's bash tool may run built-in utilities of its own (the Oh My Pi shell on this pod has `cat`,
`head`, `ls`, `wc`, `grep`, `sed` and `find` as shell builtins, uutils-like: `cat 0.8.0`, `grep
(pi-uu-grep) 0.2.0`, `sed 0.1.1`), so an answer of `ls` and the like (its layout, an error's
wording) may differ from what the session saw. A test runs, through the path every answered
command runs through, `cat` of a key file, `ls` of the runner's home and of `/opt`, `env`, and `git
status` in a tracked directory laid out as a bare repository whose `config` sets `core.fsmonitor`,
as though the rule had let each through: the key is in no answer, the home and `/opt` list empty,
`env` shows no credential, no fsmonitor runs, the copy is read-only and there is no network; and
`curl` is refused and never run.

A runner process starts one checkout sandbox at a time, in request order.
The queue limits sandboxes, not gateway calls, `read`, `grep`, or `glob`.
On a two-CPU pod, eight concurrent `git status` commands all exceeded 30 s.
Two concurrent commands finished in 10.9 s each.

The runner bounds setup at 30 s from spawn and starts a fresh 30 s command deadline when the sandbox reports ready.
Queue wait counts against neither deadline.
The sandbox keeps its place until its process exits.
An expired call remains unanswered, so the replay receives no verdict and retries within its attempt limit.
The same queue holds the commands that make a colocated jj checkout, with a 30-minute setup and command deadline.

A reply one of whose read-only calls the copy answers in no form (a read with another selector, a
path outside the checkout or under `~`, a git or jj command where the copy is the tarball, a jj
command where its colocation failed or whose checkout tracks a `.jj` at its root, a command whose
sandbox cannot be made or one of whose programs the sandbox lacks, an output whose notice would
name an artifact (past 50 KB, a line past 768 bytes, or one the minimizer may rewrite), a `find`
or `ls` tool, a run past its time, a `grep` or `glob` past 30 s, a `grep` one regex test of which
over a line took more than 100 ms; and any call but a skill read where the written case has no
checkout, above) gets no verdict, never judged on that call: its reason begins `unanswered-read:`
and names the call and why, and it is run again as any run with no verdict (at most 5 extra runs
a case, stage and arm; an arm that cannot finish for such runs and those that read past round 5
drops its case as
`reads-past-last-round`, its reason counting each kind). The judged step is each answered round's
calls, then the act; a reply after round 5 still all read-only calls the copy answers gets no
verdict and is run again (`reads-past-last-round`). A session case's read-only calls are its
judged step, as before. Each run records its checkout rounds (`checkoutRounds`) and, in
`skillReads`, each round's answers: a checkout one as `{"from": "checkout", "tool", "input",
"isError"}` (and `"wholeText": true` for a whole-text answer), its input naming the path, the
pattern or the command. Each report counts, by kind of case, the runs run again for each reason
and the whole-text answers: `runs with no verdict, run again, and reads answered whole, by kind of
case: session: <n> reads-past-last-round, <n> unanswered-read, <n> reads answered with the file's
whole text in place of the pin's summary; written: …`.

#### Read-only calls

One rule (`checkout.ts`'s `readOnlyCall`, its lists `READ_ONLY_TOOLS`, `READ_ONLY_PROGRAMS`,
`READ_ONLY_GIT` and `READ_ONLY_JJ`) decides both the checkout rounds and the report's inspection
floors. A read-only call is a call of the `read`, `grep`, `glob`, `find` or `ls` tool (the pinned
Oh My Pi's file finder is `glob`; `find` and `ls` are declared by no capture), or a `bash` call
whose commands, joined by pipes, `&&`, `||`, `;` or line ends, in a subshell `( … )`, a group
`{ …; }` or a command substitution (`$(…)`, or a backtick with no `\` in it; each counted by its
inner commands), are each `cd`, `cat`, `head`, `tail`, `ls`, `find` (without
`-exec`, `-execdir`, `-ok`, `-okdir`, `-delete`, `-fprint`, `-fprint0`, `-fprintf`, `-fls` or
`-files0-from`, which reads its starting points from a file),
`grep`, `rg`, `wc`, `sed -n`, `nl`, `stat`, `file`, `tree`, `pwd`, `true`, `echo`, `printf`, `env`
with no command (`-0`, `-i`, `-u NAME`, then assignments), `unset` (but of `PATH`, `HOME`, `PWD`,
`GIT_OPTIONAL_LOCKS`, `GIT_CONFIG_COUNT`, `GIT_CONFIG_KEY_n`, `GIT_CONFIG_VALUE_n`,
`GIT_CONFIG_SYSTEM`, `GIT_CONFIG_PARAMETERS` or `JJ_CONFIG`, the answers' own), `export`, an
assignment `NAME=value` alone
or before a command, `git` (with at most `-C <dir>`, `--no-pager` or `-P` before it) `log`, `show`,
`diff`, `status`, `grep`, `ls-files`, `blame`, `rev-parse`, `worktree list`, `remote` (alone, `-v`,
`show` or `get-url`) or `branch` with no name (alone, `-a`, `-r`, `-v`, `--list` or
`--show-current`), or `jj` `status`, `st`, `log`, `show`, `diff`, `root`, `file show`, `bookmark
list` or `workspace list`, with no redirection to a file anywhere (`/dev/null`, a copy of one
stream into another such as `2>&1`, and an input `<` are allowed). Every program's options are read
by one rule, as GNU's getopt_long reads them: a short option attached to its value (`-vNAME`) or
in a cluster (`-nO<cmd>`, `-ni`, `-bC`), and a long option by any start of its name that git or
getopt takes (`--in` for `--in-place`, `--open` for `--open-files-in-pager`). By any of those
spellings, no option may run a program, write a file or read one no word names: sed's `-i` and
`--in-place`, and `-f` and `--file` (a script the rule cannot read); git's `--output`, `-O` (`git
grep -O<cmd>` runs it), `--open-files-in-pager`, `--ext-diff`, `--no-index` and `--show-signature`,
and a format's `%G` (both run gpg); jj's `--tool`, `--config`, `--config-toml` and
`--config-file`; rg's `--pre`, `--pre-glob`, `-z` (`--search-zip`) and `--hostname-bin`; tree's
`-o`, `-R`, `--fromfile` and `--fromtabfile` (listings read from a file); wc's `--files0-from`
(`--files0`, `--f`: a list of files to count, whose paths no word names); and file's `-C`
(`--compile`), `-m` (`--magic-file`), `-f` (`--files-from`), `-p`
(`--preserve-date`), `-z`, `-Z` and `--uncompress`. The options are those of the tools the sandbox
runs, each checked against its own `--help` (GNU coreutils 9.7, GNU grep 3.11, GNU findutils 4.10,
GNU sed 4.9, git 2.47): no other option of `cat`, `head`, `tail`, `ls`, `wc`, `nl` or `stat` reads
a path no word names (`echo`, `printf`, `true` and `pwd` are bash's builtins), and none of the
read-only git subcommands takes `--pathspec-from-file`. A sed script (its `-e` scripts, else its
first operand) is read command by command, as GNU sed reads it: an `e`, `r`, `R`, `w` or `W`
command, or
an `s` with an `e` or `w` flag, makes the call not read-only, and a regex, a replacement or an
`a`, `i` or `c` text is no command. An assignment, `export` or `printf -v` (or `-vNAME`) affects
only the call's own shell, and sets no variable that picks a program, a library, a configuration or
a directory (`PATH`, `HOME`, `IFS`, `CDPATH`, `PAGER`, any `GIT_*`, `JJ_*`, `LD_*`, `XDG_*`,
`BASH*`, …). A `$VAR` (or `${VAR}`) expansion is allowed of a variable the command assigns before
it, or of `HOME`, `PATH` or `PWD`; a word of `find`, `rg`, `tree`, `file`, `sed`, `git`, `jj` or
`printf` takes one known value, so no expansion slips in an option that writes, runs a program or
sets a variable (and one of `wc`, but for a glob word whose every text starts with no `-`, a file
operand); a word whose unquoted text holds a glob character (`*`, `?`, `[`) has none, since
bash puts in its place the names of the files it matches, one of which may be an option made of a
file's name (`git grep -e x *` with a file `--open-files-in-pager=sh`, `printf *` with a file
`-vPATH`), and a glob in a word of `printf`, `env` or `export`, or in an assignment's value, makes
the call not read-only (so `git diff src/*.py`, `sed -n 1p *.md` and an unquoted `find . -name
*.py` are not answered; `cat src/*.py`, `grep -n x src/*.py` and `wc -l src/*.py` are, and a glob
of theirs that the copy's names make an option is not, above). So does a
brace expansion (`{a,b}`, `{1..3}`) in the unquoted text of any word of any command, or of a
redirection's target, which bash makes into several words, a quoted or escaped `}` or `,` neither
ending nor splitting one, as bash reads it (`sed -n -e p {x\},-e1e\ id} f` is two scripts, the
second running `id`; `git show HEAD@{1}:f` and a quoted `'a{b,c}'` hold none). So `cd
/work/acme/proj && unset GIT_CONFIG_GLOBAL && pwd && (jj st || git status) && git worktree list`
and `cd "$(git rev-parse --show-toplevel)"` are read-only, and `git
branch new-name`, `git remote add x y`, `jj new`, `jj describe`, `export X=$(rm -rf /)`, `env -u X
git status`, `(cat a > b)`, `printf -vPATH .`, `sed -n 'r /etc/hostname' f`, `sed -n --in p f`,
`git grep -nOid main`, `tree -ao out` and `rg --pr=sh x` are not. A process substitution, an
arithmetic expansion (`$((…))`, or bash's old `$[…]`, which runs the substitutions a variable it
reads holds), any other expansion (`$?`, `$1`, `${X:-y}`, `$'…'`), a background `&`, a heredoc or
herestring, a comment, a program named by its path or an expansion, or any other command makes the
call not read-only.

#### The arm of ours

`--ours <dir>` is a skill of ours, whose frontmatter `name` (and directory name) may differ from
the case's skill (`arms.ts`). Its profile is the case's less every copy of their skill's source
(the repository and path of the copy listed under their name, at any commit, under any
namespace), as the arm with no skill leaves them out, with ours under our own name and
description in place of the copy listed under theirs. Where the profile already lists a skill of
another source under our name, it keeps that entry, replaced by the `--ours` text, and adds no
copy of theirs, so no name is installed twice (the context refuses a name installed twice), and
the stage records that it replaced it (`ours.replacedOwn`). The arm then lists each name as the
arm with no skill does (below, `relisted`): a copy of another source listed as
`<namespace>/<their name>` is listed under their bare name where no copy left holds it bare, and
a namespaced copy of our name keeps its namespace, since ours holds that name bare in the
directory Oh My Pi reads first; every other copy of another source stays as the profile has it.
Each read of a file of their skill
in the history becomes a read of `skill://<ours>/<the same path inside the skill>`, answered from
our directory at our installed directory (theirs, with our name as its last part); where ours
has no such file, the read call and its result are removed as the arm with no skill removes them,
and the stage records the read's path (`ours.removedReads`). A whole read of the cut's copy by its
path (a case's `pathCopy`) is treated the same way: our file at that path, in Oh My Pi's path-read
form, or the call and its result removed (and the path recorded) where ours has no such file. So a
renamed unchanged copy of their skill gives old's texts but for the name. In a reply, a read of
`skill://<ours>/…` is answered from our directory, a read of the copy by its path (or, in a case
that names no copy, of a file under their installed directory, below) from our file at
that path (or as not found where ours has none), and a read of `skill://<theirs>/…` from the copy
of another source the arm lists under their name (recorded as answered by that copy), or, where
it lists none, with Oh My Pi's `Unknown skill: <theirs>` text (recorded as answered by no copy);
each is a skill read, answered in its round as above, not judged as the step. A block of their
skill that reaches
the session beside its listing and its reads ("A skill can reach a session other than by its
listing line" below) carries our text in its place: the Envoy plugin's dispatch-first block is the
plugin's function of our SKILL.md, and an autoloaded block is rebuilt from our SKILL.md under the
recorded path with our name as its skill directory's last part, its details naming our skill and
that path. The capture lists our skill under its own name and description, and a subagent's
recorded listing names ours in their skill's line, or, where the arm lists another source's copy
under their name, keeps their line under that copy's hint and adds our line where Oh My Pi's
listing, sorted by name, puts it. Its captures are kept under
`<out>/captures/ours-<digest of our directory>/`.

#### The judge reads no skill name

In a run with an arm of ours (`--ours`), before the judge reads a case's criterion (pass and
fail), its line, its history and a step, in every arm, the case's skill name and ours become
`the-skill` (`blind.ts`): `skill://<name>` reads `skill://the-skill`, a path part `/<name>/`
reads `/the-skill/`, and the name as a whole word reads `the-skill`. A longer name that holds the
name as part of a word (`alphabet`, `alpha-extra` for `alpha`) stays. So the judge cannot tell
the arm of their skill from the arm of ours by a name, and the criterion uses the step's words. A
run with `--new` (both arms hold the case's skill) or with no skill compared (the judge reads the
case's own history in every arm) blinds nothing: its judge reads the case's text as it is.

#### The smallest drop

For each screened case, the report gives the smallest drop, in 5-point steps below the case's
screened pass rate of old, that the rule (the screen's flag, then the confirm's loss: `score.ts`'s
`lossChance` with the compared arm) catches with a chance of at least 80%, with that chance. With
the cut or ours compared, the compared arm passes the drop below old and no skill at the case's
screened rate; with no skill compared, no skill is the arm that drops, and the screen's gap is
counted once (informative and flagged are one condition). Where no drop down to a rate of 0
reaches 80%, the figure says so and gives the chance at the largest drop. The report gives the
median over the screened cases and over the informative cases ("none" when there are none; a
case no drop reaches counts above every other). At old 0.9 and no skill 0.1 the figure is 40
points at 87.52%, compared with the cut or with no skill.

### The harness strings

Before a capture is saved, every string of its body that comes from the capture's run and not
from the session is replaced (`capture.ts`, "The harness strings"):

- Oh My Pi's reminder at the start of the first user turn (`Today: <date>; current working
  directory: '<dir>'`) becomes the reminder Oh My Pi journaled for that turn in the session, byte
  for byte. Where the session's head holds none, it is written with the session's working
  directory and the UTC date of the turn it stands for: a case's step, or a load task's first user
  turn.
- The working directory is the session header's `cwd`, else the one its journaled reminder names,
  else `/work`. A context file's `<file path="…">` under the capture's working directory names it,
  and every other mention of the capture's working directory does too. When the profile's
  `workingDirectory` is below the checkout's root, a parent level's context file, and every other
  mention of the capture's checkout, names the session's checkout: that working directory less the
  `workingDirectory`.
- A devbox context's user-level `CLAUDE.md` is shown at `<devboxClaudeConfigDir>/CLAUDE.md`, the
  configuration's value of the dev box's CLAUDE_CONFIG_DIR, else at `/work/.claude/CLAUDE.md`.
- A prompt Oh My Pi builds shows `- OS: linux` in its workstation lines, the same in every arm. A
  subagent's recorded prompt is sent as recorded, its workstation lines and `<file path>` blocks
  the session's own.
- The stand-in daemons answer with that working directory, made-up ids, and the session id each
  request to the Envoy listener names.
- A Claude Code capture (`load`) shows the session's working directory, its memory directory
  under the config directory above, the session's date in `Today's date is`, and `OS Version:
  Linux`.

A body that still holds the case's scratch directory after this is refused. The session's head is
read once from its store, through the configuration's `psqlCommand` (`session-head.sql`: the
file of the path hash, its first 256 Ki characters only), and saved as
`<out>/sessions/<store>-<path hash>.json` (a head an earlier run saved as `<path hash>.json` is
read back too); a read that fails stops that capture, with why. Each capture's record says each
value it shows and where the value came from (`harness`: the store, the configuration, or
neutral, and why), how many strings each rule replaced, and keeps the body as the host sent it
beside the saved one (`<stem>.raw.body.json`). A saved capture whose record holds no `harness`
was made before this rule and is refused.

### `read`

```
bun scripts/skill-cuts/cli.ts read --config <skill-cuts.json> --key-file <file> \
  --prompt <case|restate|audit|navigation|map|attack|same-job|sentence|names|tasks|craft|nearest|claim|written-case|proposal-target|proposal> \
  --in <input.json> --out <answer.json> [--request-log <file>] [--budget-usd <n>] \
  [--concurrency <n>]
```

Runs one reader (`readers.ts`, `prompts/<name>.md`) on one input, a JSON value of that reader's
input schema, and writes `{prompt, settings, answer, refusals, spent, usd}`.

### `control map|attack|same-job|sentence`

```
bun scripts/skill-cuts/cli.ts control <kind> --config <skill-cuts.json> --key-file <file> \
  --in <reader input.json> --out <result.json> [--request-log <file>] [--budget-usd <n>] \
  [--concurrency <n>] \
  map:      --delete-line <a line of the cut text that the base text holds too>
  attack:   [--plant-line <line>]          (default: "Run the tests where needed.")
  same-job: --planted <skill entry.json> --copy-of <id>
  sentence: --into <id> --sentence <text>
```

Plants a known finding in a copy of the reader's input, runs the reader once, and writes whether
its answer names that finding. It exits 0 when the control held, 1 when it did not. The craft
reader's control, `control craft`, takes its own flags (below).

### `count-sql`

```
bun scripts/skill-cuts/cli.ts count-sql --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD> [--hour <0-23>] [--query count|models]
```

Prints `count.sql` filled for one store and one UTC day (or hour), the same as
`bun scripts/skill-cuts/countsql.ts`. `--query models` prints `models.sql` instead: each session
file's model, read from the first 256 Ki characters of the file, as one row per file hash.

Each count row also names, for each file, the keys of the reads that put it in the row (`calls`,
and `unpaired_calls` for calls with no result). A key is md5 of the call's id, a space and the
call line's UTC timestamp to the microsecond, cut to 16 hex characters. Inside one query, a key
that two files hold counts in the file whose file hash (md5 of its path, 16 hex characters) sorts
first in byte order, and `shared_call_ids` counts such keys.

### `load`

```
bun scripts/skill-cuts/cli.ts load --config <skill-cuts.json> --key-file <file> \
  --skill <skill dir> [--name <name>] --tasks <tasks.jsonl> --host <omp|claude> \
  [--profile <profile.json>] --out <dir> [--runs <n, default 3>] \
  [--model <model, default anthropic/claude-sonnet-5-5>] \
  [--thinking <level, default off on omp, auto (Claude Code's own) on claude>] \
  [--budget-usd <n>] [--concurrency <n>] [--request-log <file>] \
  [--repos <dir>] [--cache-dir <dir>] [--scratch <dir>] \
  [--models <models.yml>] [--omp <binary>] [--hint-model <model>] [--hint-draw <n, default 1>] \
  [--omp-config <file>]... [--claude <binary>] [--context-session <store>:<path hash>]
```

The load test (`load.ts`): whether the host's agent loads the skill for each task,
given what the host lists for it. `--tasks` holds one task per line, `{id, text, label, source}`
with `label` `need` or `near-miss` and `source` `written`, or `session` with its `store` and
`pathHash`. A profile file is the other skills installed beside the skill (the `Profile` of
`cases.ts`), a `context` (a case's: `devbox`, the default, `legion:<project>:<role>` or
`subagent:<agent>`, whose profile carries the subagent's recorded system prompt and tools), and
for Claude Code a `claude` plugin layout: `{marketplace, plugins: [{name, skills}], skillPlugin}`.
The layout names each skill but the project skills. A project skill is a skill of the profile's
`sessionRepository` (the repository the session's working directory was a checkout of, at the
commit the profile's build read it at) whose path is `<level>/.claude/skills/<name>` or
`<level>/.omp/skills/<name>`, where `<level>` is the working directory's path in the checkout or a
parent of it up to the root. Claude Code lists a `.claude/skills` project skill under its bare
name, not as `<plugin>:<name>`, so `load` writes it at its path in the checkout and in no plugin;
it leaves a `.omp/skills` one out, since Claude Code does not read `.omp/skills`; a layout that
puts a project skill in a plugin is refused. A profile that names no session repository has no
project skill. The capture's record names where each skill went (`placements`).
Each task runs in the context of the session it came from: a task line may name its own `profile`
file (absolute, or relative to the tasks file's directory), and `--profile` is the profile of each
task that names none; a task with neither is refused. `--name` tests the skill under another
name: its frontmatter `name` and its directory's name change in a scratch copy, a subagent's
recorded prompt lists it under that name (its listing line's name changes, its text and every
other line stay), and nothing else that names the skill changes.

Per task, the host's first request is captured once against a recorder, in the context of the
task's own profile (Oh My Pi through `capture.ts`, which lists the skill under the hint Oh My Pi
has a model write, with the plugins' stand-ins, and sends a subagent's recorded prompt with only
the skill's listing line changed; Claude Code with a scratch `CLAUDE_CONFIG_DIR` per profile, into
which a local marketplace installs the profile's skills and the tested skill, each in its plugin,
while a project skill goes in the checkout), then sent until it has `--runs` judged runs (below).
The plugins' stand-ins of an Oh My Pi capture listen on 127.0.0.1. A Legion role's run gets a
stand-in Legion daemon, a stand-in Envoy listener and a sink for the NATS server the Envoy plugin
dials. The run of every other profile that installs the Envoy plugin, whatever its context (a
devbox one included), gets that listener and that sink, since the plugin defines its tools only
when it has a listener to reach; a subagent's run gets the listener alone. A capture follows the
harness strings rule above: a session task stands for its own session, and a written task for the
one `--context-session` names (the session its profile was rebuilt from; `<path hash>` is the
SHA-256 of that session file's path in hex), or for none, with neutral values, when it is not
given. The runs of a profile's tasks are sent together, once each of its tasks has its capture,
in one seeded order, each run with a fresh session id that each of its requests keeps, the first
run of each task in the order alone among its task's. Each body is its capture's but for that id
and a cache breakpoint on its last message (`replay.ts`'s `withCacheBreakpoint`), so that a
task's later runs read the whole body from the cache. An `openai/<id>` model runs on Oh My Pi
only, on the Responses route, with an Anthropic `--hint-model`. A reply loads the skill when one
of its tool calls reads it (Oh My Pi: `read` of `skill://<name>` or its `SKILL.md`, a `tool_use`
block or a Responses `function_call` item; Claude Code: `Skill` naming `<name>` or
`<plugin>:<name>`, or `Read` of its `SKILL.md`).

The hints (the architect, 2026-11-23). Oh My Pi lists each skill as `- <name>: <text>`, where
`<text>` is a hint a model writes from the skill's name and description, at temperature 1, or a
cut-down preview of the description until the hint is written. The hint is made from the name
too, so the two arms of a name test get different hints by design, and that is part of the
name's effect. Where a task's session recorded its listing, the load lists each skill under the
line that listing holds, not a new draw: `build-tasks --profiles-out` keeps in each profile the
listing its listing measure uses (`recordedListing`: `source` `{kind: "session"}` or `{kind:
"subagent", pathHash}`, each line's `name` and `text`, and `tested`, the profile's entry of the
session's copy of the tested skill). A skill the profile installs keeps its recorded line. The
tested skill keeps its line only where the load lists it under that name and with the
description of the session's copy (read from its repository at its commit): a name test's
proposed name, or an end-time text that differs from the session's, gets a drawn hint. The
capture writes each recorded line into the scratch profile's `skill-descriptions.db` under Oh My
Pi's key for that name and description (a recorded preview is listed as the preview), taking the
key from the cached hint of draw 1, else from `<cache-dir>/hint-keys/<key>.json`, else from the
row Oh My Pi stores when the hint run answers its hint request with the line itself; no model is
asked for a recorded line. `behaviour` passes no recorded line.

`--hint-draw <n>` (a whole number, default 1) is the draw of every hint no recorded line gives.
Draw n > 1 asks the hint model again for each skill text and caches the hint apart. A draw's hint
is the file `<cache-dir>/hints/<key>.json`, where `<key>` is the first 32 hex characters of the
SHA-256 of
the Oh My Pi version (`omp --version`'s output), the name and the description, each followed by
a NUL byte, and for draw n > 1 also `draw <n>` and a NUL byte. The file is one JSON object:
`{"omp", "name", "description", "ompKey", "hint", "model", "writtenAt", "kept", "draw"}`
(`draw` only for n > 1; `reply` and `reason` where `kept` is false). `ompKey` is Oh My Pi's key
of the name and description, the same in every draw: to seed a draw's hint by hand, copy it from
that skill text's draw 1 file or a capture record's `hints`. Claude Code lists each skill under
its description, so it has no hint to draw: `--hint-draw` above 1 is refused there (exit 2).

Each capture record's `hints` entry gives each listed skill's line (`hint`) and where it came
from (`source`): `recorded (session)`, `recorded (subagent <path hash>)` or `drawn (draw <n>)`.
Each task of `summary.json` gives the tested skill's line and its source (`listedUnder: {source,
line}`; on Claude Code the source is `description (Claude Code)`, and a Claude Code record says
so in `listedSource`), the summary's `listedUnder` counts the tasks of each source, and the
command prints how many tasks list the skill under a recorded line and how many under a draw. An
Oh My Pi `--out` made before this rule, or under another draw, is refused.

Skill reads follow the behaviour replay's rule (the architect, 2026-11-22). On Oh My Pi, a reply
whose every tool call reads a skill file is answered as `behaviour` answers one: a read of
another skill the profile installs from that skill's installed copy (made again first where the
cache lacks it, as `behaviour` makes it), a read of a skill the profile does not install with Oh
My Pi's unknown-skill answer, a read of another file of the tested skill from its directory. Then
the request goes again with the answers, at most 3 rounds.
Each run comes to one of four outcomes:

- `loaded`: a reply reads the skill, the first message or one after answered rounds, before any
  reply does more than read skill files. The run ends there.
- `sent by` another skill: the answer to a read of that skill holds a text that names the tested
  skill, that is `skill://<name>`, `/skill:<name>`, the name in backticks, or the name next to the
  word "skill" ("the `x` skill", "skill x", "x skill"). The name alone as a word does not count,
  so names such as `analyze` or `sdd` do not match ordinary prose. The run ends once that answer
  is made, before it is sent.
- `reads past last round`: the reply after the third answered round still only reads skill files.
- `not loaded`: any other run.

Claude Code's runs end at their first message, and their run lines say so (`skillReads: "first
message"`): the runner does not rebuild Claude Code's result of a `Skill` or `Read` call, so it
answers no read there.

`loaded` and `not loaded` runs are judged. For each `sent by` or `reads past last round` run the
task runs again, at most 5 extra runs a task, until it has `--runs` judged runs; the extra runs go
in a seeded order of their own. A task still short of them after its 5 extra runs gives no
description evidence, a need task and a near-miss task alike: the skill's verdict leaves it out,
and its line and the summary's `noEvidence` say why, with the counts (`sent by <skill> in <n> of
<m> runs`, `reads past the last round in <n> of <m> runs`). It prints, per task, its loads among
its judged runs, what each run came to, its runs sent by each other skill, its runs that read past
the last round, its extra runs, its verdict and its spend at list price (its runs, its failed runs,
its hints, all); each send order's seed; and the skill's verdict (`score.ts`). It writes
`<out>/runs/<task>.jsonl`, one line per run: its outcome, `loadedBy`, `sentBy`, `skillReads`, and
each reply (its tool calls with their names and targets, the answers made to its reads with where
each came from, `arm` for the tested skill's directory, `copy` for an installed copy and `absent`
for none, and what it used; every reply but the last went back with its answers), with its session
id, its send seed and its place in the order. A run that fails after one or more of its requests
had their replies (a later request refused, the budget's refusal among them) comes to no outcome
and is made again by the next command, but what those replies used is kept in
`<out>/runs/<task>.failed.jsonl` (the error and each reply's usage), and counts in its task's spend,
its task's line and the test's spend from then on. It also writes `<out>/captures/`,
`<out>/sessions/` and `<out>/summary.json`, whose tasks give `judged`, `loads`, `sentBy`,
`readsPastLastRound`, `extraRuns`, `noEvidence`, `fault` and `spend` (`runsUsd`, `failedRuns`,
`failedRunsUsd`, `hintsUsd`, `usd`), and whose `spend` gives the test's `runsUsd`,
`failedRunsUsd`, `hintsUsd` and `usd`; each run line and each task of the summary names its
profile file and the SHA-256 of its bytes. A task whose run file holds its judged runs, or has used
its extra runs, is skipped, so a run started again goes on from what was saved; a task whose
capture or runs were made under a profile of other bytes is refused, and so is an `--out` whose
runs were sent before each had its own session id or before this rule. A task whose context needs
a directory no read can give (as `behaviour` drops a case for) is dropped: its line and the
summary's `dropped` give the reason (`unreadable-directory: ` and the repository, the commit and
the path), a run started again skips it while its profile file keeps the same bytes, and the
verdict rule counts it as a task with no runs.

Every load applies the listing rule (the architect, 2026-11-22), so no one applies it by hand.
Each task's listing measure comes from the tasks file's build-tasks summary
(`<tasks>.summary.json`): a session task's own profile's `listing measure` evidence item, and a
written task's context session's; a task with no summary beside its file is not measurable. A
task whose rebuilt listing misses more than 10% of the names its session's recorded listing holds
is dropped before any run (`listing-misses: ` with the count and the names): its line and the
summary's task give the reason, the summary's `listing` counts the tasks measured, not
measurable and dropped, and no verdict reads it. Where the rule drops more than half of a load's
tasks, the command stops before any run (exit 3) and says so with the numbers: the architect
decides such a load. The skill's verdict is made twice, with and without the tasks whose listing
could not be measured, both in the summary's `verdicts`. Where they agree it stands, its reason
giving how many tasks were not measured; where they differ there is no verdict (`no verdict:
depends on tasks whose listing could not be measured`, with both results).

### `copies`

```
bun scripts/skill-cuts/cli.ts copies --config <skill-cuts.json> --at <ISO instant> --out <dir> \
  [--only <owner/repo>]... [--max-search-calls <n>] [--cache-dir <dir>]
```

The list of copies and loaders (`copies.ts`): every repository of the configured owners at its
default branch's commit at `--at`, each `SKILL.md` outside the configured `excludePaths`, the
names in two or more repositories, the manifests and installers that install each skill, the
files outside a skill that name one (found by code search, paced under its limit), and the
copy-prefix table the read count is attributed by. Its files are listed in `copies.ts`'s header.
With `--cache-dir`, each owner's listing, each repository's read (its commit, the tree entries it
keeps and every blob it read) and each finished code search is saved there the moment it ends,
under a temporary name renamed into place, so a run stopped at any moment and started again with
the same arguments reads nothing twice; the hit files the searches find are read again. A truncated
tree is walked a level at a time in GraphQL batches of up to 50 subtrees, at most 4 reads in flight
for one repository and 8 in all, of which at most 4 run a gh process at once (`ghLimiter`, below).
It prints one line per listed repository read (`copies: tree <n>/<total> <owner/repo>: <entries>
entries, <calls> tree reads, <seconds> s, cached|read`, or `failed (<gh's error>)`), one per
pinned repository read (`copies: pinned <owner/repo> at <ref> (<what pins it>): …`), and one per
25 searches, so the log gives the rate.

Seven install paths give a skill to Oh My Pi from outside the owners' trees, and each is a unit of
the list, read as Oh My Pi 18.18.6 reads it:

- An Oh My Pi plugin package (a `package.json` whose `omp` or `pi` field is truthy; its skills come
  from `omp` when that field is there, else from `pi`): the skill directories its manifest's
  `skills` list declares, each inside the package, else its `skills/`, each scanned as Oh My Pi
  scans a skill directory (an entry holding no `SKILL.md` is read one level deeper, a skill with
  no description is left out, and a declared directory lists no skill through a link that leads
  out of the package), and the skills its own repository's prepack script stages for it. It is
  installed at `node_modules/<name>/`, and, for a package the worker image carries (one its own
  repository's prepack script stages skills for, or whose directory holds that repository's
  Claude Code build script), also at `/opt/legion/<its last name>/`. A prepack script copies its
  own repository's `skills/<name>` into the package directory it runs in, so a fork's script
  that stages the same package name stages into the fork's package, never its parent's, and the
  plugin a build script generates holds its own repository's stages alone.
- A `github:<owner>/<name>#<ref>` dependency of an Oh My Pi plugins `package.json`: the package
  at the root of that repository, at that ref, installed under the dependency's name, whatever
  the repository's owner.
- An `ensure_vendor <url> <name> <ref>` clone of an installer script, which a `skills-sources.json`
  pool under `vendor/<name>/` feeds to Oh My Pi: the cloned repository at that ref, less each
  entry the installer's `find … -name … -exec rm` removes from the pool. A clone made as one entry
  of a pool's skills directory (`ensure_vendor <url> <pool>/skills/<name> <ref>`, a repository that
  is one skill, its `SKILL.md` at its root) gives that repository's root skill under `<name>`, or,
  where its root holds no `SKILL.md`, each entry of its root that holds one.
- A clone made by hand that the configuration's `clones` states (`{repo, dir, clone}`: the
  directory `dir` of the dev box's checkout of `repo` is a clone of `clone`, such as a private
  repository cloned into a dotfiles checkout). No installer the list reads makes it, so the
  operator states it. A `skills-sources.json` pool of `repo` in `dir` is read as a pool in an
  `ensure_vendor` clone is: the cloned repository at its default branch, and the note of each
  `installers.tsv` row of the unit names `clones[<i>] of the configuration`. Both repositories
  must be of an owner in `owners`, or the configuration is refused at load. A `dir` that holds no
  pool of `repo`'s `skills-sources.json` stops the run once the repositories are read (exit 2,
  nothing written), naming the key. `build-cases` and `build-tasks` read the clone at its default
  branch's commit at the session's start, as they read an installer's clone that names no ref.
- A plugin an installer script installs from a marketplace into Oh My Pi's own plugin tree
  (`omp plugin install <plugin>@<marketplace>`, or a helper given the marketplace's repository and
  `<plugin>@<marketplace>`): the marketplace's repository is read at its default branch, and the
  plugin's source at the commit its marketplace entry pins (`sha`, else `ref`). Oh My Pi lists
  such a plugin's skills from its `skills/` and from the `skills` entries of its marketplace entry
  and of its own manifest
  (discovery/claude-plugins.ts, loadSkills, lines 226-275; an install of Oh My Pi's own is not
  Claude Code's registry, so no setting gates it: lines 44-53). A line's host is read from its
  words less its `<plugin>@<marketplace>` and `<owner>/<name>` words, so a marketplace named
  `claude-plugins-official` names no host.
- A `gh skill install <repo> <skill> … --pin <ref>` of an installer script (each `$NAME` the
  script sets read so): the repository's `skills/<skill>` at that ref (else its one copy of that
  name), installed at `~/.agents/skills/<skill>`, which Oh My Pi's agents provider lists
  (discovery/agents.ts, lines 123-127 and 158-170).
- A Legion deployment's files: its role setup script (`legion/*setup.sh`) fetches a catalog
  repository (`repo=<owner>/<name>`, `omp plugin marketplace add`) and installs plugins of it
  (`omp plugin install`); its program (`legion/main.go`) names the plugins every role installs
  (`everyRolePlugins`); a project file's `verificationSkillsLeaveOut` names the catalog plugins
  its roles leave out; and a worker image's Dockerfile (`legion/worker*/Dockerfile`) installs each
  skill of a GitHub release as an Oh My Pi plugin under `/opt/<dir>/` (an `image:<dir>` unit, at
  the release's tag). A marketplace entry whose source is `git-subdir` is the plugin directory
  `path` of that repository at its pin.

Each repository a pin names (the dependency's, the clone's, the marketplace's, a marketplace
plugin's source, a `gh skill install`'s) is read at the pin's ref, in place of its default
branch's commit when an owner's listing holds it too; `repositories.tsv`'s `branch` names the
ref. A repository with no pinned ref is read at its default branch. A marketplace read at its pin
pins its plugins' sources in turn, so the pins are read again until no new one appears. The prefix
rows of these units reach the table only when the configuration's `targets` name their
repositories. `build-cases` and `build-tasks` read a pinned repository at the ref its installer
names at the session's start; where `repositories.tsv` names a ref (a pin's tag) made after the
session's start, they read the repository's default branch then, and leave the repository out,
with that evidence, when that branch has no commit then either.

The prefix table also maps each path a prepack script staged a skill at during the count's window
and stages no longer, where the worker images and package releases of those commits hold the
skill. One GraphQL read of the script's history at the commit read gives each version that held in
the window: the one in effect at `windowStart`, and each one a commit of the default branch made
since. For each skill an older version staged for a package that the script read no longer stages
for that package into that directory, the package's unit gets prefix rows at that path under each
place the unit is installed (`**/node_modules/<package>/dist/skills/<name>/`, and
`/opt/legion/<package>/dist/skills/<name>/` for a package the worker image carries), each mapped
to the copy at the directory that version staged it from, as the tree at `--at` holds it. The rows
name the package's unit, so `build-cases` and `build-tasks` resolve that unit at a session's
commit as they resolve any. Under the script's line, `skill-copies.md` names each such path with
the commits that staged it there (`from <commit> (<date>) until <commit> (<date>)`), and each
path that gets no row, with why: the tree at `--at` holds no `SKILL.md` at the directory it was
staged from (a skill renamed or removed since), or the package is no package unit of the script's
own repository.

### `attribute`

```
bun scripts/skill-cuts/cli.ts attribute --config <skill-cuts.json> --count <count dir> \
  --copies <copies out dir> --spans <spans.jsonl> --out <dir> \
  [--changed <cut or set id>=<file>]... [--models <models dir>]
```

The read count per skill copy (`attribute.ts`): it maps each read's copy path to a repository
copy by the prefix table, then by the repository-relative skill directory a checkout's path ends
with, then by the copies the store's context installs. Copies of several repositories at the
skill directory a checkout's path ends with are told apart by how the path names the repository,
in this order: its owner and its name as two directories in a row; its name as a directory; its
owner followed by a directory that holds its name; a directory that holds its name (`.dotfiles`
holds `dotfiles`). So an exact name wins over a directory that only holds it:
`/legion/workspaces/acme/kiln-extra/` names acme/kiln-extra, not acme/kiln. A checkout the path
leaves tied (a repository and its fork, `skills/<name>` in both, neither named) goes on to the
context, which places the read at the one of the tied copies the context installs, else leaves
it unattributed with both reasons. It counts each session once across the count's queries; marks
ties and loaded skills; states each store's span; ranks each cut's changed skills for the
behaviour test and writes each ranked skill's reading sessions
(`<out>/<cut or set id>/sessions/<skill>.jsonl`, file hashes only). With `--models`, it also
counts each copy's reading sessions by model, and writes each ranked skill's
`<out>/<cut or set id>/models/<skill>.json`: the counts by model of the sessions its sessions file
holds (`sessions`; `byModel`, each `{model, anthropic, served, sessions}`, most first; `noModel`,
the sessions whose file's head names no model; `notQueried`, those no models query held;
`notAnthropic`; `served`, those on a model the gateway serves; `withModel`), which `behaviour
--reading-models` reads. Without `--models` it writes no models file.
`--changed` takes a skill set of the configuration (`sets`) as it takes a cut: `--changed
<set id>=<file>`, the file one skill directory of the set's repository per line (the set's skills
to rank), ranks them by the sessions the rules above place at that repository's copies, and
writes `<out>/<set id>/ranking.json`, `<out>/<set id>/sessions/<skill>.jsonl` and, with
`--models`, `<out>/<set id>/models/<skill>.json`. A read of a set's skill at
`node_modules/<package>/skills/<name>/` goes to the set's copy by the prefix row `copies` gives
the set's package (when `targets` names the set's repository), not to a skill of ours of the same
name.
A path inside a copy of another repository that an installer writes (`<dir>/vendor/<name>/`, a
package under `node_modules/`, a plugin under `plugins/cache/`) is a checkout only of a repository
the copy's own directories name, and a read inside an `ensure_vendor` clone that no prefix row
maps goes to no copy by the context rule either: it is unattributed.
A read that two files of a store hold (the same call id and timestamp: a forked session copies
its parent's lines) counts in one of them, the file whose file hash (md5 of its path, 16 hex
characters) sorts first in byte order, whichever queries hold the two, since a fork written on
another mtime day than its parent is in another query. A file stays a session of a skill when
one of its reads of it counts in it. `build-cases` reads the count by the same rule. A count made
by an earlier `count.sql`, whose rows have no `calls`, fails the command (exit 1). The report's
store table gives the call ids two files of one query hold, the call ids files of two queries
hold, and the pairs of files of two queries that hold one.

### `set-count`

```
bun scripts/skill-cuts/cli.ts set-count --config <skill-cuts.json> --rows <count dir> \
  --spans <spans.jsonl> --out <dir> [--dotfiles-commit <sha>] [--rows-label <text>] \
  [--not-read <name>=<reason>]...
```

The read count of each skill set's skills (`set-count.ts`, with the pins read by `sets.ts`). It
reads no store and calls no model. The configuration must give two keys: `sets`, `[{id, repo}]`,
each skill set measured against our skills (`id` the set's name, which is also its Claude Code
plugin name and the `<id>:` prefix of its skills in a Claude Code `Skill` call, and may not be a
cut's id or another set's; `repo` its upstream repository, `owner/name`), which `attribute
--changed`, `build-cases --set` and `build-cases --reprofile` also take where they take a cut; and
`dotfilesRepo`, the `owner/name` whose `omp/plugins/package.json` and `installers/omp.sh` pin the
sets (required when `sets` is given), which only this command reads.

Inputs:

- `--rows`: the count's raw output, in `attribute --count`'s layout (`<store>/<day>.txt`, or
  `<store>/<day>T<hour>.txt` for each hour of a split day), read by `attribute.ts`'s own reader.
- `--spans`: each store's earliest record, in the form `attribute --spans` takes.
- `--dotfiles-commit`: the commit of `dotfilesRepo` whose pins are read, 40 hex digits; without
  it, the default branch's commit when the command runs.
- `--rows-label`: words that name the rows (an artifact and its version), written as given.
- `--not-read`: a source of sessions the rows cannot hold, with its reason (the Claude Code
  transcripts), once for each.

The pins. A set is pinned by a dependency `"<package>": "github:<owner>/<repo>#<ref>"` of
`omp/plugins/package.json` (its owner and repository the set's, in any case, whatever the package
key) or by a line `ensure_vendor <url> <dir> <commit>` of `installers/omp.sh` whose url is the
set's repository on GitHub, each line read as `copies` reads an installer's clones (`copies.ts`'s
`parseManifest`: quotes taken off, and a line that passes a variable or a command's output pins
nothing). A set that no line pins, or that two lines pin, fails the command. A
40-hex ref is the commit; a tag resolves to its commit (a lightweight tag's ref names it, an
annotated tag's tag object names it); any other ref fails the command. The set's skills are the
`skills/<name>/SKILL.md` of its repository's tree at that commit, so the place its installer reads
skills from must be `skills/`: for a package, its `omp.skills`, else its `pi.skills`, else (none
declared, or no `package.json`) `skills/`, as Oh My Pi reads a plugin package; for a vendor
checkout, the one `skills-sources.json` pool `vendor/<dir>`. Any other place fails the command.
The set's upstreams are its repository's fork parent and source. The count also names each commit
of `dotfilesRepo` in the window at which a set's pin changed, from GitHub's list of the commits of
each manifest's path, each set against the commit's first parent, with the pin before and after.

The rule. A count row counts as the skill S of a set when S is one of the set's skills at its pin.
Its copy is the set's own copy when it starts with `<set>:` (a Claude Code `Skill` call), holds
the pin's install directory (`/node_modules/<package>/` or `/vendor/<dir>/`), or holds
`/plugins/cache/` and after it a path part that is the set's id (Claude Code's plugin cache). A
copy that is a URL is no install directory: a URL that names the set's repository or an upstream
of it is a read of the upstream by URL, any other URL another copy. A row with no path and no
prefix (empty, a `skill://` path, a bare name) names no copy; any other path or prefix is another
copy. A session is its store and file hash, and counts once for S, in the first of these columns
that one of its `read` rows reaches: `own`, `upstream_url`, `other_copy` (it read only another
copy, such as a Legion fork), `no_copy`. `reference_own` counts the sessions with a `reference`
row of S at the own copy, apart. Only a row's paired reads count, never its unpaired calls. A read
that two files of a store hold (a forked session copies its parent's lines) counts in one of them,
the file whose path hash sorts first, whichever queries hold the two, as `attribute` counts it
(`attribute.ts`'s `countOnce`): a file stays a session of S in a row when one of its reads there
counts in it. Rows made by an earlier `count.sql`, with no `calls` and `unpaired_calls`, are
refused.

Before it reads GitHub it refuses (exit 2), naming each gap, rows in which a configured store that
has rows lacks a summary row for a day from the window's start to its end: no output of that day
or of all 24 of its hours, a file that is not one whole count.sql result, or a summary row whose
mtime range is not its query's. A configured store with no rows is named as not read.

Outputs under `--out`:

- `exemplar-count.md`: the window; the rows read (the label as given, and one sha256 over the
  files: of one line `<sha256>  <store>/<file>` per file, sorted by path, each file's sha256 of the
  bytes it counted, read once); each store with its
  earliest record from the spans file; each store not read and each `--not-read` with its reason;
  the dotfiles commit and each pin with the commit it resolves to; the pin changes in the window;
  the rule; the sessions per skill in each column, and per skill and store; the skills with at
  least 20 own-copy sessions; the `count-sql` command line of each store and day the rows hold
  (with its psqlCommand, as it made the file), and the command's own line, with the
  `--dotfiles-commit` it read added when none was given, so it reads the same pins again.
- `exemplar-count.tsv`: one row per skill of each set at its pin (a skill with no read at 0), in
  the sets' order, most own-copy sessions first: `set`, `skill`, `own_sessions`,
  `upstream_url_sessions`, `other_copy_sessions`, `no_copy_sessions`, `reference_own_sessions`,
  then each again per configured store (`<column>:<store>`).
- `sessions/<set>/<skill>.jsonl`: each own-copy session, one line `{"store", "hash"}`, sorted by
  store then hash; an empty file for a skill with none.
- `summary.json`: the numbers of each skill in total and per store, the rows' files with their
  sha256, the stores (each with its queries and its sessions with a read, a read two files hold
  counted once as above), those not read, the pins, the pin changes, the skills at 20 or more
  (`atLeast20`) and the commands.

It exits 0 when the count is written, 1 when a GitHub read fails or a pin cannot be read as
above, and 2 when it refuses its arguments, a configuration without sets, a `--not-read` that is
not `<name>=<reason>`, rows or a spans file it cannot read, an `--out` inside `--rows`, or rows
with a gap.

### The craft list

A JSON file written by hand (`craftlist.ts`): the properties of a skill that the craft guide of
every set of `sets` states. Its shape:

```json
{"schemaVersion": 1,
 "sets": {"<set>": {"repo": "<owner/name>", "commit": "<40 hex>", "guide": ["<path>", "..."]}},
 "properties": [{"id": "C1", "property": "<one line>", "checkedBy": "reader",
   "readerLooksFor": "<text>",
   "lines": [{"set": "<set>", "path": "<path>", "line": 95, "lineEnd": 98}, "..."],
   "example": {"set": "<set>", "path": "<path>", "line": 3}}]}
```

- `sets`: each set by its name, with its repository, the commit its guide is read at, and the
  guide's paths.
- `id`: `C1`, `C2`, … once each.
- `lines`: the line (or the lines `line` to `lineEnd`) of each set's guide that states the
  property, at least one of each set of `sets`. A property that a set's guide does not state is no
  property of the list.
- `checkedBy`: `reader` (the craft reader checks it on a skill's text) or `behaviour` (only a
  behaviour test can check it).
- `example`: for a reader property, one line of a skill of a set that shows it. `record`: for a
  behaviour property, one `https://` link to a record of the sets' testing.

Every command that reads the list refuses one that breaks a rule above.

### `craft-lines`

```
bun scripts/skill-cuts/cli.ts craft-lines --list <craft-list.json>
```

Prints each set with its repository and commit, then for each property each cited line (or
range) and its example line, with the text of those lines in the set's repository at its commit,
and its record link. It reads each file once from GitHub through `gh` (`sets.ts`, `SetReads.text`)
and calls no model. It exits 1 naming each cited line or example past the end of its file and
each path the commit does not hold, or when a GitHub read fails; 2 when it refuses its arguments
or the list; and 0 when it printed every cited line.

### The style text's rule ids

The craft reader judges each rule of a style text (`--style`), by its id (`style.ts`): `S1`, `S2`,
… in the text's order:

1. each body row of the text's first table, the parts of a SKILL.md in order (not its header row
   or its separator row);
2. each top-level list item (`-`, `*`, `+` or `1.`) under a line or a heading named Rules
   (`**Rules:**`, `## Rules`);
3. each top-level list item under `**Check before you commit:**`.

A section runs to the next heading, or to the next line that is only a bold label that ends in a
colon. An item's indented lines (its `- Example:` and `- Source:` sub-items) belong to its text and
get no id; a paragraph of a section gets none. A style text with no rule is refused. So a style
text of 5 table rows and 10 numbered rules gets 15 ids, and one of 7 table rows, 10 items under
Rules and 3 under Check before you commit gets 20, its check item "No paragraph holds more than
one rule" `S19`.

### `craft`

```
bun scripts/skill-cuts/cli.ts craft --config <skill-cuts.json> --key-file <file> \
  --skill <skill dir> [--skill-source <text>] \
  --style <style text> --style-source <text> --craft <craft-list.json> --craft-source <text> \
  [--nearest <skill dir of theirs>]... --out <entry.json> \
  [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]
```

One fresh request of the `craft` reader (`readers.ts`, `prompts/craft.md`), asked again while its
answer is refused, 3 tries in all. It reads every `.md` file of `--skill` (SKILL.md first), the
style text and its rules with their ids, the craft list's properties whose `checkedBy` is `reader`
(never a behaviour property), and every `.md` file of each `--nearest` skill; nothing else. The
`--*-source` flags are words written into the entry as given (for example an artifact and its
version, or a repository at a commit).

The reader judges each style rule and each reader property once, `met`, `gap` or
`not-applicable`, and lists each gap: its file and line (for a missing element, the line after
which it belongs, 0 before line 1), the rule it is against (`S<n>`, `C<n>`, or
`nearest:<skill>:<path>:<line>`, the line of a nearest skill that shows what this skill lacks or
contradicts), its kind (`present` or `missing`) and why. The prompt says that no gap rests on a
word or line count alone: a length the style text gives is a hint to the writer, not a threshold.
An answer is refused when a gap names no file of the skill, a line past its end, or a rule that is
none of these; when it judges a rule twice or not at all; when a rule judged `gap` has no gap; or
when a gap is against a rule not judged `gap`.

It writes the entry to `--out`: `{entry: "craft", skill: {name, dir, source, files: [{path,
sha256}]}, style: {file, source, sha256, rules}, craft: {file, source, sha256, readerProperties},
nearest: [{name, dir, files}], reader: {prompt, settings}, input, answer, refusals, spent, usd}`,
where `answer` is `{judged, gaps}`, or null when every reply was refused. It writes the exact user
message beside it, `<out less .json>.input.md` (`input`). It exits 0 with an answer, 1 when every
reply was refused, and 2 when it refuses its arguments, a style text with no rule, a craft list,
or a skill directory with no SKILL.md.

### `craft-merge`

```
bun scripts/skill-cuts/cli.ts craft-merge --entry <entry.json> --entry <entry.json> \
  [--entry <entry.json>]... --out <merged.json>
```

The merge of fresh `craft` readings of one skill (`craftmerge.ts`). The craft reader's answer
varies between fresh requests of the same inputs (on one skill text it listed 6, 5, 0 and 3 style
gaps), so a skill's findings come from several readings of the same inputs. Each `--entry` is an
entry that `craft` wrote. It calls no model and reads nothing but the entries.

It refuses (exit 2), naming each difference, an entry that is not a reading of the first entry's
inputs: another name of the skill's directory, another set of skill files or another sha256 of
one, another sha256 of the style text or of the craft list, or another set of nearest skills (by
name) or of their files and sha256. It refuses, naming it, an entry whose `answer` is null (every
reply refused), and it refuses fewer than two `--entry` and an entry it cannot read, that is no
JSON or that is no craft entry. It refuses, naming both, one reading given twice: two `--entry`
of one file (the same path once every link and `..` is resolved) or of two files with the same
content (the same sha256), since that reading's gaps would look found by more readings than found
them.

Two gaps are one gap when they have the same `file`, `line` and `rule` (`S<n>`, `C<n>`, or the
same `nearest:<skill>:<path>:<line>`); a different kind does not split them. It writes `--out`:
`{entry: "craft-merged", skill, style, craft, nearest, reader, readings, entries: [{path,
sha256}], gaps: [{file, line, rule, readings, of, kinds, why}], judged: [{rule, gap, met,
notApplicable}], noGap, usd}`, where:

- `skill`, `style`, `craft`, `nearest` and `reader` are the first entry's, as it wrote them;
- `readings` is the number of entries, and `entries` each entry's path and the sha256 of its file,
  in the order given;
- `gaps` holds each gap that any reading lists: `readings` is the count of readings that list it,
  of `of`; `kinds` and `why` are the kind and the why of each listing, in reading order, each why
  with the prefix `reading <i>: `. They are sorted by file, then line, then rule (as text, so
  `S10` comes before `S2`);
- `judged` holds each rule that any reading judged, with the count of readings that judged it
  `gap`, `met` and `not-applicable`, sorted by the rule's letter and then its number;
- `noGap` is true exactly when no reading lists a gap (`gaps` is empty);
- `usd` is the sum of the entries' `usd`.

It prints one line: the skill, the number of merged gaps and how many of them are in each count
of readings (`in 3 of 3: 8; 2 of 3: 6; 1 of 3: 3` for three entries), or `no gap in <n> readings`,
and the spend. It exits 0 when it wrote the merged entry, and 2 when it refuses.

### `nearest`

```
bun scripts/skill-cuts/cli.ts nearest --config <skill-cuts.json> --key-file <file> \
  --skill <skill dir> [--skill-source <text>] --candidates <source>=<root>... --out <row.json> \
  [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]
```

One fresh request of the `nearest` reader (`prompts/nearest.md`). Its input is the skill's name,
its whole description and its SKILL.md less the frontmatter block (the body Oh My Pi reads after
the block, trimmed: `common.ts`'s `splitFrontmatter`), and every skill of each
`--candidates` root (each `<root>/<name>/SKILL.md`), with its name, its whole description and the
source the flag gives it; the frontmatter is read as `common.ts`'s `readFrontmatter` reads it
(past any HTML comments before it), and a name is the frontmatter's `name`, trimmed, else the
directory's name, as `load` names a skill (`common.ts`'s `ompSkillName`). Two candidates of
different sources may have one name, so the answer names each candidate by its name and source:
`{verdict, nearest: [{name, source, verdict, missing, reason}], reason}`, with at most two entries,
each `equivalent` (it does the skill's job, for the same trigger) or `partial` (it does a material
part of it); the answer's `verdict` is `none` exactly when it names none, else the best entry's.
It writes the row to `--out`: `{row: "nearest", skill: {name, description, source, dir},
candidates: [{source, root, skills}], reader, input, answer, nearest, refusals, spent, usd}`, where
`nearest` is each entry with the candidate's whole description, and the exact user message beside
it (`<out less .json>.input.md`). It exits 0 with an answer, 1 when every reply was refused, and 2
when it refuses its arguments, a `--candidates` that is not `<source>=<root>`, a root that holds no
skill, or a skill with no SKILL.md.

### `craft-coverage`

```
bun scripts/skill-cuts/cli.ts craft-coverage --root <dir> [--source <text>] [--skip <name>]... \
  --entries <reader-findings.jsonl>
```

The inventory of the craft entries. It lists each `<name>/SKILL.md` directly under `--root`, less
each `--skip` name, and prints each with its craft entry (the last one, with its gap count, or
`every reply refused`) or `none`. An entry is a line of `--entries` whose `entry` is `craft` (the
other lines, a proposal's entry among them, are passed over), and it matches a skill by its
skill directory's name and, with `--source`, by its `skill.source`. It calls no model and reads
nothing else. It exits 1 naming each skill with no entry, 2 when it refuses its arguments, a root
it cannot read or that holds no skill, or an entries file it cannot read or a line of it that is
no JSON or no craft entry, and 0 otherwise.

### `proposals`

```
bun scripts/skill-cuts/cli.ts proposals --config <skill-cuts.json> --key-file <file> \
  --body <pull request body> --body-source <text> --section <heading> --list <text>... \
  --pr-diff <id prefix>=<gh pr diff output>... --skills-root <repo>=<dir>... \
  --repo-root <repo>=<repository tree> \
  --style <style text> --style-source <text> --craft <craft-list.json> --craft-source <text> \
  --findings <reader-findings.jsonl> --out <dir> \
  [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]
bun scripts/skill-cuts/cli.ts proposals --dry-run --body <pull request body> --section <heading> \
  --list <text>...
```

Reads each open proposal of closed pull requests that a later pull request's body lists, with two
readers (`readers.ts`), and writes one entry per item. `--body` is the body as text (`gh pr view
<n> --json body --jq .body`), and `--body-source` the words the entries give it (the pull request
at the head read).

**The items.** `--section` names a heading of the body, of any level, by its whole text; its section
runs to the next heading of its level or a higher one. Each `--list` names the one line of the
section that starts with its text (`Lines whose target is gone` names `Lines whose target is gone
from this repository (this pull request cannot carry them):`). The list is the `- ` items after
that line, up to the next line after a blank line that is no item, or the next heading; a line
indented under an item goes on with its text. An item is `` - `<id>` (<state>): <text> ``,
whatever the words in the parentheses (`open`, `partly on main`, `open; no ruling found`, `target
gone`, `done in its new home`, or any other). A list no `--list` names is never read. The command
exits 1 naming each line of a named list that is not of that form, gives an id with a character
other than a letter, a digit, `.`, `_` and `-`, or gives an id twice, or that follows an item, is
not indented and is no item. It exits 2 when the section or a list's line is not found once, or no
item follows a list's line. `--dry-run` prints the items with each list's count and reads nothing
else.

**The diffs and the trees.** Each item's id names its pull request by its prefix, the text before
its first `-` (`41-9.12` → `41`): `--pr-diff 41=<file>` gives that pull request's `gh pr diff`
output, which `proposals.ts`'s `splitDiff` splits into files (each path before and after the
change, so a renamed file has both) and hunks (each hunk's `@@` line and its lines, as many as the
line counts). Each `--skills-root <repo>=<dir>` gives a directory of skills (`<dir>/<name>/SKILL.md`)
and the words of its repository at the commit read, which must be the `skill.source` the
repository's `craft` entries give. `--repo-root <repo>=<dir>` gives the whole tree of the repository
the pull requests changed, at the commit read, for a target that is a file of it.

**Two readers per item.** `proposal-target` (`prompts/proposal-target.md`) gets the item's pull
request's whole diff, with each file's hunks by their indexes, first in the request and marked for
the prompt cache (`cache_control: {"type": "ephemeral"}`), so each item of one pull request reads it
from the cache; then the repository of `--repo-root`, the item (id, state and text) and every skill
of the skills roots (repository and name). It answers the hunks that make the item (each a file of
the diff, by either of its paths, and a hunk's index in it, from 0) and its targets: a `dotfiles`
file of the repository by its path, a `skill` of the list by its repository and name, or `none`,
each with why. An answer is refused that names a file the diff does not hold, a hunk past its
file's last, a skill not in the list, or a target twice. A path that names two files of the diff
(a file renamed away from it, and a new file at it) names the one the diff shows at that path, in
the answer's check and in the entry alike.

When every target is `none`, no second call is made: the entry says no target was found and gives
the body's own words of the item. Else `proposal` (`prompts/proposal.md`) gets the item, its hunks,
each target's text with its lines numbered (a file read from `--repo-root`, or that the tree holds
no such file; a skill's `.md` files from its skills root, named `<skill>/<path>`), the gaps of each
target skill's `craft` entry of `--findings` (the last entry whose source is the skill's root's
repository and whose directory's name is the skill's; numbered across the targets), the style
rules with their ids, and the craft list's reader properties, as `craft` gets them. It answers the
gaps the change fixes (each a finding by its number, or a style rule or property with its file and
line), the gaps it makes (each a rule, with its file and line where it gives them), and a one-line
summary. An answer is refused that names a finding, a rule or a file the request does not hold, a
fix with both a finding and a rule or neither, a line with no file, or a line past its file's end.

**The entries.** Each item's entry is `<out>/<id>.json`: `{entry: "proposal", id, state, text,
list, line, body: {file, source, sha256}, diff: {pr, file, sha256}, style, craft, findings: {file,
sha256}, input, target: {reader, answer, refusals, spent, usd}, hunks: [{file, hunk, header}],
targets, noTarget, reader, answer, refusals, spent, usd}`. `targets` gives each target read: a file
with its repository, path, file and SHA-256 (null when the tree holds no such file), or a skill
with its repository, name, directory, each file's SHA-256 and its findings entry (`{line, gaps}`:
the entry's line of `--findings` and its count of gaps, null when every reply of that entry was
refused; null with no entry). `noTarget` is `{bodyWords, why}` for an item with no target found,
and then `targets` is empty and `reader` and `answer` are null; `answer` is the `proposal`
reader's answer, `usd` the dollars of both calls. The user messages sent go to
`<out>/<id>.input.md`. A reader whose every reply was refused leaves its item no entry, and its
refusals go to `<out>/<id>.refused.json`.

The first item of each pull request is read alone, then the rest, as many at once as
`--concurrency` lets. An item whose entry exists is not read again, so a run started again reads
only the items with none; an entry of another state or text of its item than the body's is refused
(exit 2), so a changed item is read again only once its old entry is moved away. It exits 0 when
every item has an entry, 1 when an item has none at the end, 2 when it refuses its arguments, a
diff, a root with no skill, a style text, a craft list or a findings file, and 3 when the budget is
spent.

### `control craft`

```
bun scripts/skill-cuts/cli.ts control craft \
  (--remove-section <heading> | --join-lines <a>-<b>) \
  --copy-line <a>[-<b>] [--copy-kind <present|missing>]... [--copy-rule <id>]... \
  [--copy-claim <text>] \
  --original-line <a>[-<b>|all] [--original-kind <present|missing>]... [--original-rule <id>]... \
  [--original-claim <text>] \
  <craft's flags: --config, --key-file, --skill, --style, --style-source, --craft, \
   --craft-source, --nearest, --skill-source> --out <dir> \
  [--request-log <file>] [--budget-usd <n>] [--concurrency <n>]
```

The craft reader's control. It writes a changed copy of the directory `--skill` names (the one a
link points at, so the skill itself is never written) under `<out>/copy/<skill dir name>/`, with
SKILL.md changed: `--remove-section` removes the heading's line and every line up to the next
heading of the same or a higher level (a line in a fenced code block is no heading);
`--join-lines a-b` joins the list items of lines a to b into one paragraph at line a, their list
markers removed. Then it runs `craft` on the copy
and on the unchanged skill, each with the same `--nearest` (`<out>/copy.json` and
`<out>/original.json`, each with its `.input.md`).

Each side has its own criteria. A gap is a candidate of a side when its line is in the side's
range (a line of SKILL.md: the copy's line numbers for the copy, the unchanged skill's for the
original; `all`: any line of any file), and, where given, its kind is one of the side's kinds and
its rule one of the side's rules (no `--*-kind`: any kind; no `--*-rule`: any rule). A rule is an
`S<n>` of the style text, a reader `C<n>` of the craft list, or `nearest:<skill>:<path>:<line>`.
Where a side gives a claim, the `claim` reader (`prompts/claim.md`, one fresh request a candidate)
answers whether the candidate's gap states the claim, with its reason; each claim request's input
and answer go to `<out>/claims/<side>-<n>.json`, with its `.input.md`.

The control holds only when the copy has a candidate that states its claim (any candidate, where
it gives no claim) and the unchanged skill has no candidate that states its claim (no candidate,
where it gives no claim); a candidate of the unchanged skill whose every claim reply was refused
does not let it hold. It writes `<out>/control.json` (each side's criteria, candidates with their
claim answers, whether it holds and why; whether the control held and why; the spend), prints each
side and its candidates, and exits 0 when the control held, 1 when it did not (or a craft read's
every reply was refused), and 2 when it refuses its arguments, a heading SKILL.md does not hold
once, a line of `--join-lines` that is no list item, or an `--out` that holds a copy already.

A control that a removed section's check is missing, for example: `--remove-section '## Step 4:
Verify' --copy-line 31-43 --copy-claim <the claim> --original-line all --original-claim <the
claim>`. A control of the paragraph rule: `--join-lines 37-39 --copy-line 37 --copy-kind present
--copy-rule <the paragraph rule's id> --original-line 38 --original-claim 'line 38 holds more than
one rule'`.

### `build-cases` and `build-tasks`

```
bun scripts/skill-cuts/cli.ts build-cases --config <skill-cuts.json> --key-file <file> \
  (--cut <id> | --set <id>) --skill <name> --old <skill dir at the cut's base> \
  [--new <skill dir in the cut>] --sessions <sessions.jsonl> --seed <n> \
  --copies <copies out dir> --count <count dir> --out <cases.jsonl> [--max <n>] \
  [--budget-usd <n>] [--concurrency <n>] [--stop-projected-usd <n>] \
  [--stop-after-sessions <n>] [--stop-zero-case-usd <n>] [--drop-navigation]
bun scripts/skill-cuts/cli.ts build-cases --config <skill-cuts.json> --key-file <file> \
  (--cut <id> | --set <id>) --skill <name> --old <skill dir> --copies <copies out dir> \
  --count <count dir> --written <n> \
  (--context-session <store>:<hash> | --sessions <sessions.jsonl> --seed <n>) --out <cases.jsonl> \
  [--session-cases <cases.jsonl>] [--reading-models <file>] [--model-pool <file>]... \
  [--written-answer <file>] [--budget-usd <n>] [--drop-navigation]
bun scripts/skill-cuts/cli.ts build-cases --config <skill-cuts.json> --old <skill dir at the cut's base> \
  --copies <copies out dir> --count <count dir> --reprofile <cases.jsonl> --out <new cases.jsonl>
bun scripts/skill-cuts/cli.ts build-tasks --config <skill-cuts.json> --key-file <file> \
  --skill <skill dir> --store <db>... --out <tasks.jsonl> [--model <model>] \
  [--profiles-out <dir> --copies <copies out dir> --count <count dir> --context-session <store>:<hash>] \
  [--max-candidates <n>] [--max-search-minutes <m>]
bun scripts/skill-cuts/cli.ts build-tasks --config <skill-cuts.json> --copies <copies out dir> \
  --count <count dir> --reprofile <tasks.jsonl> --profiles-out <new dir> \
  [--context-session <store>:<hash>]
bun scripts/skill-cuts/cli.ts build-tasks --first-read --config <skill-cuts.json> --key-file <file> \
  --skill <skill dir> --sessions <sessions.jsonl> --seed <n> [--max <n>] --out <tasks.jsonl> \
  [--model <model>] [--profiles-out <dir> --copies <copies out dir> --count <count dir>]
```

`build.ts`: `build-cases` pulls each candidate session through the config's `psqlCommand`, has
the `case` reader propose the step the skill governs, reading the session from the skill's read
on in windows of at most 40 messages, at most 6 windows a session, and writes the cases
`loadCases` reads, each with the evidence for its rebuilt context; `build-tasks` writes the need
and near-miss tasks of a load test from the first requests of real sessions. `--count` and
`--copies` are the count directory and the copies output that `attribute` read to make the
sessions file: `build-cases` places each session's read of the skill with `attribute.ts`'s own
rules (the prefix table, then a checkout, then the store's context), and keeps a session only
when they place it at the cut repository's copy; a read they place at another copy, or at none, is
`not-cut-copy`, with the rule named. `--set <id>` takes a skill set of the configuration (`sets`)
in place of `--cut <id>`; one of the two is required, and both are refused (exit 2). The set's
`repo` is then the cut repository: a session is kept only when its read is placed at the set's
copy (a read of our skill of the same name, a Legion fork's among them, is `not-cut-copy`), the
profile must install the set's copy, and each case's `cut` holds the set's id; `--reprofile`
finds each case's cut or set by its `cut`. A session's reads of the skill are its `skill://` reads
and its reads of the skill's files by path, by the count's rule (a path or URL that, its read
selector taken off, ends `<skill>/SKILL.md` or holds `skills/<skill>/references/`). A case is built
from a successful `skill://` read, or from a successful whole read by path (no selector) that those
rules place at the cut repository's copy, by the path as read, as the count places it. That copy's
directory is the path up to the skill's directory, resolved as Oh My Pi's read resolves a path: an
absolute path as it is, a relative one against the session's working directory (its header's `cwd`),
and one under `~` against the home directory the session ran under, the part of its transcript's
path before the first `/.omp/` (Oh My Pi keeps its sessions under `<home>/.omp/` and resolves `~`
against that same home). Such a case names the copy (`pathCopy`: that directory, with the working
and home directories), and every whole read by path of a file under it, absolute, relative or under
`~`, is in `skillReads`, so each arm answers it with its own text and `none` answers it as not
found. The case's profile installs the units of that copy, as it does for a `skill://` read's
`[Skill file: …]` path. A session whose only successful reads of the skill are reads by path that no
case is built from is dropped under the first reason that applies: `path-range` (a read selector,
`:1-40` or `:raw`), `other-copy` (another repository's copy, or a URL), `unattributed` (a path the
rules place at no copy), `path-relative` (the cut's copy at a relative path in a session whose
header names no working directory, or under `~` in one whose transcript's path names no home). A
case keeps every other successful read by path of the skill before its step as recorded in every
arm, and its evidence item `excluded reads` names each. A case with a file read by path that the
arms do not answer in Oh My Pi's form (arms.ts's `pathFormFault`, or a directory), in the old text
or in `--new`'s, is dropped as `path-form`, so no `behaviour` run fails on it. `--new` is optional
(the session form only: `--written` and `--reprofile` refuse it, since they check no read by
path): a build before the cut has a text checks the old text alone, and its summary's `inputs`
record `new: null`; a build goes on from a summary only with the same choice (given where it was
given, absent where it was absent), and is refused otherwise, naming both. `behaviour screen`
then drops a case whose new text, or ours (`--ours`), holds such a file before any model call, as
`path-form: <arm>: <fault>`, and runs the rest of the file. A session whose work at the skill's
read is on the skill's own text (writing, editing or rewriting it or a copy of it, reviewing,
testing, grading, scoring or comparing it) is no case of the skill's behaviour, since its next
step works on the text and does not follow it: the case reader answers
`{"step": null, "material": {"why": …}}` for it
(prompts/case.md), and the build drops it as `skill-as-material`, counted with the other drops.
Any other read of the
skill's files before the step that the case neither holds nor names is still `read-by-path`. A
session is a candidate only on a model the gateway serves (`servedModels`: its id after the last
`/`, so a
`claude-*` model behind any gateway's prefix counts); a session on any other model is counted as
`model-not-served` and never run on another model. The `case` reader is offered only the base
text's lines that the session's own read of that file held (a session whose reads hold none is
`no-common-line`, before any reader call). A case's profile lists what each installing unit
shipped at the session's start: each repository of a unit is read at its last commit at or before
that start (its tree, its `SKILL.md` files and its manifests) and resolved by `copies.ts`'s rules,
so every skill of the profile is in its repository at the commit the case names, and the case's
evidence says which skills the later copies output lists that the unit did not ship then. A
repository GitHub holds no commit of at or before the time it is read at (the session's start, or
a subagent's root's last prompt build) did not exist then, so nothing of it was installed then:
the profile leaves it out, and the evidence item `left out: <repo>, which has no commit at or
before the session's start <time>` gives the `gh api` read. Such a repository is often one the dev
box's installers began to install after the session's start, which the later copies output
lists. When it is the session repository, its project skills and context files are not read and
the profile names no session repository; no later commit is read in its place, so the listing
measure counts any of its project skills the session listed as missing. Any other failed GitHub
read still fails the profile. A read
whose skill none of its units ships at that start, because the later copies output names a unit
as its manifest names it later (a marketplace or a plugin renamed since: the read's Oh My Pi
plugin cache directory, `<marketplace>___<plugin>___<version>`, shows the old name), is placed by
the prefix rows those manifests give at that start, at the unit that installed it then. The
session repository is the repository the session's working directory is a checkout of: for a
Legion pane, the repository of its workspace `/legion/workspaces/<owner>/<name>/<workspace>/…`
(the checkout at `<workspace>`); for a dev-box session, the dev box's own repository where the
working directory is in its checkout, the directory whose `.claude/` the configuration's
`devboxClaudeConfigDir` names (the dev box's CLAUDE_CONFIG_DIR); else the repository of the copies
output's list named as the last part of its working directory, or of the nearest parent named as
any listed repository when one repository alone has that name and its tree at its commit holds the
working directory's path below that parent (a parent's name alone decides nothing: otherwise none,
and the evidence says why, or names the repositories that matched); the checkout's root is that
directory. A working directory directly inside such a parent whose path the tree does not hold is a
workspace of that repository (a jj or git worktree, such as `<dir>/<repo>/<workspace>`) and the
checkout's root, when a file the session read by an absolute path under it is a file of the tree at
its path below the working directory (a read of `<dir>/<repo>/<workspace>/CLAUDE.md` where the
tree holds `CLAUDE.md`): the evidence item `session repository <repo>` says it is a workspace of
that repository named `<workspace>` and names the read and the file. Where the session read no
such file, or the working directory is two or more levels below that parent, there is none, and
the evidence says why. The profile names that
repository and its commit at the session's start (`sessionRepository`), and lists the
repository's project skills at that commit, as Oh My Pi 18.18.6 lists them, provider by provider:
each `<level>/.omp/skills/<name>/SKILL.md` (one with a description), then each
`<level>/.claude/skills/<name>/SKILL.md`, then each `<level>/.agent/skills/<name>` and
`<level>/.agents/skills/<name>` (`.agent/` first at each level), then the working directory's
`.codex/skills/<name>`, `.opencode/skills/<name>` and `.github/skills/<name>` (one with a
description), where the levels are the working directory's path in the
checkout and each of its parents up to the root, the nearest first (a working directory
`<root>/agents/agentvm` reads `agents/agentvm/`, `agents/` and the root). A skill directory such a
directory holds as a symbolic link is read at the directory the link names in the repository, and
listed at the link's path, as Oh My Pi's scan follows a link; a link out of the repository, or to
no directory of it, is left out and named in the evidence. The session repository is read only
where Oh My Pi reads a project, one directory a GitHub
call: each level, its `.omp/`, `.claude/`, `.agent/`, `.agents/`, `.gemini/`, `.github/`, `.codex/`
and `.opencode/`, its skills directories, and each skill directory in those. So a repository whose
whole tree GitHub cuts short (`truncated`) is read too. Where another skill has
the same name, the profile holds as the name's copy the one Oh My Pi gives the session (unless the
session read the other): a project's `.omp/skills` comes before `~/.omp/agent/skills` and
extension packages; its `.claude/skills` comes after those and after `~/.claude/skills`, and
before Claude Code plugins; of one kind, the nearer level comes first; a pool an extension's
`resources_discover` names comes before them all; of units of one kind, the first unit's. Oh My
Pi lists a later copy whose `SKILL.md` differs as `<namespace>/<name>` and drops one whose
`SKILL.md` is the same (extensibility/skills.ts, skillNamespace, lines 96-116, and
resolveCollision, lines 164-223), so the profile installs such a copy with its `namespace`, and a
capture lays it out under `namespaced/<namespace>/skills/<name>`, a skills directory it gives Oh
My Pi after its own, where Oh My Pi gives it that name. The namespace is the plugin's name for a
package (its `node_modules` directory's name), a Claude Code plugin or a worker image plugin;
else the directory that holds its `skills/`: a pool's directory, `agent` for `~/.omp/agent/skills`;
for a dotted directory, its provider's id: `native` for a project's `.omp/skills`, `claude` for
`.claude/skills`. A copy of another unit is left out, with why. The case's evidence cites the Oh
My Pi source for each. Every top-level session
in a checkout has its session repository's context files as Oh My Pi reads them: one at each
level from the working directory up to the checkout's root, the one its ranking keeps in that
level's slot (`.omp/AGENTS.md` at the nearest level whose `.omp/` holds anything, priority 100;
the working directory's `.claude/CLAUDE.md`, 80; `.agent/AGENTS.md` and `.agents/AGENTS.md`,
70; the working directory's `.gemini/GEMINI.md`, 60, and `.github/copilot-instructions.md`, 30;
then `AGENTS.md` and `CLAUDE.md`, 10, in that order), listed the farthest level first, each at
its path in the repository, and the profile's `workingDirectory` names the working directory's
path in the checkout when it is below the root. A Legion session also has its project's
instructions file. A top-level devbox session also has the user-level context file Oh My Pi
reads, marked `user: true` in the profile: `CLAUDE.md` in Claude Code's config directory, which
the dev box's `CLAUDE_CONFIG_DIR` names, the `.claude/` of the repository that installs its
units. That is the one repository of the profile's units that holds Claude Code's settings there
(`.claude/settings.json`) at the session's start, and the profile names its `.claude/CLAUDE.md` at
that commit; with none or several such repositories, or no `CLAUDE.md` there, the evidence says it
is left out and why. A capture writes the user-level file as `CLAUDE.md` in a scratch Claude Code
config directory and runs Oh My Pi with `CLAUDE_CONFIG_DIR` there, so Oh My Pi reads it as its
user-level file; it writes the session repository's context files at their paths in a scratch
checkout and runs Oh My Pi in the `workingDirectory` inside it, so each is read at the depth the
session read it, and a file of a parent is shown under the session's checkout (its working
directory less that path). `load` on Claude Code gives the user-level file as user memory,
`CLAUDE.md` in its scratch `CLAUDE_CONFIG_DIR`, and the other files in the same checkout. A
subagent's recorded prompt is sent as recorded.

A dev-box profile installs, beside the units its session's reads come from, every unit the dev
box's own installers install into Oh My Pi at the session's start: the one repository of the
copies output that holds both an installer script (`installers/*.sh`) and Claude Code's settings
(`.claude/settings.json`) is read at its commit of the session's start, with each repository its
installers pin (a repository the copies output no longer names is read at the pinned ref, or its
default branch when the installer names none), and each unit its installers then install into
Oh My Pi (plugin lists, skill pools, plugin installs, linked user skills) is installed. With no
such repository, or several, the evidence says so and only the read units are installed. A skill
that a pattern of that repository's Oh My Pi config `skills.ignoredSkills` matches, as a Bun.Glob,
by its own name or by the `<namespace>/<name>` it would be listed under as a later copy of a name
whose text differs, is left out (Oh My Pi 18.18.6 loads no such skill: extensibility/skills.ts,
lines 386-389, 401-406, 437 and 544). A
skill whose `SKILL.md` frontmatter sets `hide: true` or `disable-model-invocation: true` is
installed, so `skill://` reads it, and the evidence marks it `not listed`: Oh My Pi loads it and
leaves it out of the prompt's listing (extensibility/skills.ts, line 494; system-prompt.ts,
lines 961-983). A Legion profile installs what the deployment gives each role at its start: the
deployment is the repository whose project file (`legion/projects/*.yaml`) names the session's
project, read at its commit of the session's start with the catalog its role setup fetches; the
profile installs each plugin of the catalog its roles install (the every-role plugins, and, when
the project file sets `verificationSkillsLeaveOut`, an empty list too, every other plugin but those
it names) and each skill its worker image installs from a release. It installs the plugin lists
(`omp-plugins` manifests) of the repositories its worker image's packages come from (those of the
prefix rows under `/opt/legion/`) and no other: a dev box's own plugin list is no pod's, and the
evidence names each list left out. A daemon-launched controller installs the every-role plugins
alone, which a profile does not tell apart.

A Legion profile reads the worker image's packages (the units of the repository that the prefix
rows under `/opt/legion/` come from) at the commit that image was built from, not at that
repository's default branch at the session's start. The deployment, read at its commit of the
session's start, names the project's image in the top-level `image:` line of the project file
(`legion/projects/<file>.yaml`), where a deployment keeps each project's values once it moves
them out of its Pulumi config; with no such line, in the `image:` line of the project's entry
`- key: <PROJECT>` of the Pulumi config (`Pulumi.prod.yaml` in the directory above the project
file's: `legion/Pulumi.prod.yaml`). A nested map's `image:` and the `daemonImage:` line are not
that line. The commit is the one that
GitHub's build provenance of the image's digest names (`gh api
repos/<repository>/attestations/sha256:<digest>`, the `gitCommit` of its first resolved
dependency). For an image that names no digest, or whose digest has no provenance there (GitHub
answers 404, as for an image that a deployment builds on the Legion image in another registry),
the comment lines right above its `image:` line decide, by the first of their words, in their
order, that names a commit:
an image digest whose provenance names one (the base the image was built on), a commit of the
repository (a word of 7 to 40 hex digits, letters and digits both), or a tag of the repository (a
word of the form `<name>-v<version>` or `v<version>`, such as `legion-v15.2.0`, at the tag's last
commit at or before the deployment's read). The order matters: a comment that names the image's
base commit first and an older release after it ("before it …") is read at the base commit. Where
none of these gives a commit (no `image:` line in the project file and no config file at that
commit, no entry or `image:` line for the project, or a comment that names none), the
repository is read at its default branch at the session's start. The evidence item `the worker
image's commit of <repository>` says which rule decided, with each read; the image's units are
resolved from the repository's tree at that commit, and their copies and plugin directories name
it. The rest of that repository is still read at the session's start: a Legion workspace of the
image's repository gives its project skills and context files at that commit.

The Legion daemon composes a pod's role prompt, and a project's daemon image can be another build
than its worker image. The deployment names it in a `daemonImage:` line, found as the `image:` line
is (the project file's top-level one, else the one of the project's entry in the Pulumi config),
and its commit of the same repository is decided by the same rule: its digest's provenance, else
the first word of the comment right above it that names a commit. The evidence item `the daemon
image's commit of <repository>` says which rule decided, or why none did. A capture composes the
role prompt (context.ts) from `prompts.go` and its parts at that commit when the item names one,
else at the commit the profile reads that repository's plugins at (the worker image's), and its
record's `rolePrompt.commitOf` says which (`daemon image` or `plugin`). A load task's capture
reads the item from its task's evidence in the tasks file's build-tasks summary. `load` refuses
(exit 2, before any run) an `--out` that holds a task's capture whose record says its role prompt
was composed at another commit than that evidence names now, naming both; a record with no
`commitOf`, made before it was recorded, was composed at the plugin's commit.

The arms and the load test tell the copies of the tested skill apart by their source, not their
name (cases.ts's `testedCopy`: the repository and path of the copy listed under the skill's own
name, at any commit, under any namespace). Each arm installs its own text in every copy of that
source, and `none` and the arm of ours (which installs ours under our name in their place) leave
out every one, so no arm keeps the old text readable at another path; a copy of another source
that shares the name (listed as `<namespace>/<name>`) stays as the profile has it in every arm but
those two. Oh My Pi 18.18.6 lists a copy under its bare name once it finds no other copy of that
name, so for each name that no copy left in `none` or the arm of ours (ours included) holds bare
and some copy holds under a namespace, the arm lists under the bare name the copy whose
namespace's skills directory Oh My Pi reads first (a capture gives Oh My Pi `skills/` first, then
each namespace's directory in the order of its first copy in the profile); every other copy keeps
its namespace. The arm records the name the profile lists each such copy under (`relisted`). A
`skill://<name>` read in `none`, or of their name in the arm of ours, is then answered from that
copy, and a subagent's recorded prompt in that arm sends the `<name>` line under that copy's hint
and leaves out the `<namespace>/<name>` line (the arm of ours adds our line where Oh My Pi's
sorted listing puts it, "The arm of ours" above). A load
task's profile leaves out every copy of the tested source, which the load test installs once under
`--name`, and keeps the other sources' copies; load reads a namespaced skill as
`<namespace>/<name>` in every name check, and refuses one on Claude Code, whose layout places
skills by their bare name. A profile holds each copy once, since it reads a repository at one
commit: where the session's host had the tested source at a second install path too (Oh My Pi
lists that one as `<namespace>/<name>` when its text there differs), the task's evidence says so
(`held once`), and the listing measure counts such a recorded name missing.
In a load run, a read of another source's copy of the name (`skill://<namespace>/<name>`, or a
path under `namespaced/<namespace>/skills/`, where a capture installs it) is no load of the tested
skill; a read of `skill://<name>` or of the tested copy's path is.

A case whose session read the cut's own copy of the skill by its path on disk names that copy
(`pathCopy`: its absolute directory, with the session's working directory and the home directory it
ran under, which resolve a relative or `~` path as Oh My Pi's read resolves one, cases.ts's
`resolvedReadPath`), and each whole read of a file under it, absolute, relative or under `~` (no
line range; cases.ts's `pathReadFile`) is one of the case's `skillReads`. Each arm but `none`
answers such a read from its own file as the pinned Oh My Pi answers a read of a text file by path
at its default settings: no `[Skill file: …]` line; in a session that shows hashline rows, the line
`[<path>#<tag>]`, where the path is the one the session's own result shows and the tag is Oh My Pi's
snapshot tag of the arm's text, then `<n>:<line>` for each line; `<n>|<line>` or the lines alone in
a session whose reads show those; a line longer than 768 characters cut to 768 and `…`, followed by
Oh My Pi's notice of the cut lines. A file the arm lacks gets `Path '<path>' not found`, the path as
the session read it. `none` keeps the read and answers every file that way, where it removes a
`skill://` read. A replay answers a reply's whole read by path of the copy the same way. A read with
a line range, a read of another copy, and a path the case does not name stay as the session had
them. Oh My Pi shows some files in a form the arms do not rebuild (not Markdown or plain text, not
UTF-8, longer than 300 lines or 50 KB, or holding a line that opens a git conflict block): building
an arm of such a case stops with an error naming the case, and a replay takes such a read for the
judged step.

A case that names no copy (its session read the skill by `skill://`, and any read of it by path
stays as recorded) takes for one the directory its first `[Skill file: …]` line shows the skill
installed in, less the file it read, in every arm (arms.ts's `pathCopyOf`; none where no such line
shows one). A replay answers a reply's whole read by path of a file under that directory, or a
read of one with one line range, as it answers a read of the copy: each arm's own file at the path
read, its hashline line showing the file under the directory a recorded read of a file there
shows, else the absolute one; `none` with `Path '<path>' not found`; the arm of ours with our file
at that path. A read of a directory under it is no skill read. Such a copy names no working or home
directory, so a read of it by a relative or `~` path is not answered either.

A skill can reach a session other than by its listing line and a read, and each arm then gives the
session its own text of it there too (`arms.ts`). The Envoy plugin puts dispatch-first into every
request of a session with Dispatch: a user message after the leading compaction summaries, built
from the plugin's own `skills/dispatch-first/SKILL.md`, which it reads as it loads. No transcript
keeps that block, and a capture runs with Dispatch off, so for a case of dispatch-first whose
profile installs the plugin each arm but `none` puts the block, built from its own SKILL.md by the
plugin's own function, where the plugin puts it; `none` puts none, and gets a hidden stand-in of
the file (`hide: true`), so the plugin still loads and the listing names nothing more. A case of
any other skill gets no block. Whether the session had Dispatch is taken from the case where the
case says
(arms.ts's `dispatchOf`): a Legion role's session had it (its context is
`legion:<project>:<role>`), and so did a subagent of one (its evidence item `subagent <agent>`
names a `legion:<project>` store); so did a session one of whose messages holds the dispatch-first
block (the marker `<dispatch-first-skill>`, which opens the Envoy plugin's block and Claude Code's
session-start hook output alike) or calls Dispatch (a `dispatch_<command>` tool, a write of an
`xd://dispatch_<command>` device, or a `dispatch` command in a bash call). A dev box's session
that shows neither is taken to have had it. Each arm carries the answer and why (`arm.dispatch`),
each capture's record keeps it beside `injections` (`dispatch`), and the block goes in on each
answer. On 2026-11-22, of the 180 distinct cases built so far, 101 take it from a Legion role, 53
from the transcript (each a `dispatch` command in a bash call) and 26 rest on the assumption.
`behaviour report` counts, for each skill, the cases whose captures carry the block by where the
answer comes from (its `dispatch:` line), and its verdict line gives how many of the screened
cases rest on the assumption beside how many listings could not be measured; `load` counts its
tasks the same way beside its listing counts (the summary's `dispatch`, from each task profile's
context alone, since a task holds no message of its session). Oh My Pi gives a subagent each
skill its agent definition's `autoloadSkills` names as a `skill-prompt` message before its task
(task/executor.ts, lines 5573-5607), and the transcript
keeps it: each arm but `none` rebuilds that message from its own SKILL.md under the recorded path,
as Oh My Pi renders it (the body, a blank line, `---`, a blank line and `Skill: <path>`, put through
Oh My Pi's prompt format, which takes the padding out of a table's cells), and `none` leaves it
out. A case that holds such a block in a form the runner cannot rebuild is dropped, never run, and
the rest of the cases file runs (the architect, 2026-11-22): `behaviour screen` drops it before any
model call, at screen, as `injection-unrebuildable: <kinds>: <the faults>` (each fault's kind,
then each fault), where a kind is `user-invoked` (a `/skill:` prompt the user typed),
`other-shape` (a block with arguments or in another form) or `plugin-block-misplaced` (a
dispatch-first block away from where the plugin puts
it); a subagent whose agent autoloads the skill but whose messages hold no block of it and open
with no compaction summary is dropped the same way when its capture is made, as
`autoload-no-block`, at the stage of that capture. `report` lists each such case as it lists every
dropped case, and its dropped line counts them by kind. Each capture's record lists the blocks
it carries (`injections`), and a saved capture of such an arm made before these rules is refused.

In the load test such a skill gets the same block under every name it is tested under, so its runs
measure whether the agent reads a skill whose text it already holds. Under another name a
dispatch-first in a profile that installs the Envoy plugin is captured with the plugin's block of
the tested text and the hidden stand-in, and a profile that also installs a copy under its own name
is refused: the plugin would put that copy in. The load test refuses dispatch-first on Claude Code,
whose Envoy plugin puts it into a session from a session-start hook the test does not run, and a
subagent profile whose agent definition (read in Oh My Pi's order: the project's agents, the dev
box's user agents, then the profile's plugins, each at its commit) autoloads the tested skill: the
subagent has its text before its task whatever the listing says.
In any other profile such a skill is listed and tested like any other.

On 2026-11-22 the commits of cybermonklabs/legion the cases use (155 of them) have one skill a
plugin puts into the session, dispatch-first (the Envoy plugin), and two an agent definition
autoloads, `thermonuclear-code-quality` and `thermonuclear-deep-review` (each by the agent of its
name in `packages/pi-legion/agents/`). Both set `disable-model-invocation`, so no host lists them,
and the load test refuses them. The other plugins the profiles install hold skills, and one of them
agents, but inject and autoload none, and no agent of Oh My Pi's own autoloads a skill.

Oh My Pi reads a subagent's agent definition at the subagent's start from the user agents
directory (`~/.omp/agent/agents`, or `~/.omp/profiles/<name>/agent/agents` under a named profile)
before any plugin's `agents/`, and takes the first file of the agent's name (task/discovery.ts,
lines 53-74, 93-120 and 157-162; task/structured-subagent.ts, lines 538-541; utils/src/dirs.ts,
lines 361-363). The dev box's repository links its own agents there: its installers/omp.sh links
`${DOTFILES_DIR}/omp/agents` as `${OMP_AGENT_DIR}/agents`, `OMP_AGENT_DIR` being
`${HOME}/.omp/agent` (lines 9 and 43 on 2026-11-22, lines 9 and 52 on 2026-10-31). So for a
subagent's context the runner reads, after the project's agents (below), the directory that an
installer script of a repository of the profile (`installers/*.sh`: an `ensure_link` or `ln -s`
line whose target is `.omp/agent/agents` under the home) links there (context.ts's
`devboxAgents`); the record's `agent` names that line (`linkedBy`). Oh My Pi reads those agents at
each subagent's start, from the dev box's checkout as it is then, so the runner reads the
installers and the directory they link at the commit of the subagent's start: the commit the
case's `read times of <repository>` evidence item gives for that repository's pool directories
(the commit after "its pool directories at"), else the one commit the profile holds the repository
at. A profile that holds such a repository at two commits with no such item reads none, and the
record's `leftOut` says why. It reads none for a Legion store's session, whose pod runs no dev
box's installers, and none when two repositories of the profile each link one, which the record's
`leftOut` says. On 2026-11-22 the dev box's repository holds 25 agent definitions and none
autoloads a skill; two are named `thermonuclear-code-quality` and `thermonuclear-deep-review`, so a
dev box's subagent of either name runs the box's definition, which autoloads nothing, though the
profile also installs pi-legion's.

Oh My Pi reads a project's agents before the user's: the `.omp/agents` directory nearest the
session's working directory, from that directory up through each parent to the filesystem's root,
and only that one (task/discovery.ts, lines 34 and 100-111; config.ts, lines 218-246; it skips
`.claude/agents` and the like, lines 13-16). So for a subagent's context the runner first reads
the session repository at its commit, from the profile's working directory up to the checkout's
root, and takes the first level that holds a `.omp/agents` (context.ts's `projectAgents`); the
record's `agent` names it with `source: "project"` and its path in the session repository. A
`.omp` or `.omp/agents` the repository holds as a symbolic link is followed to its target in the
repository, as Oh My Pi's `stat` follows it, and `linkedBy` names each link followed
(`<link> -> <target>`). A link to nothing in the repository is no directory, and the walk goes on.
A link out of the repository (an absolute target, or one above its root) stops the walk, and the
record's `leftOut` names the link and its target: Oh My Pi reads that target when it is a
directory, and then no farther level. Above the checkout, Oh My Pi's walk reaches the dev box's
home when the working directory is under it, and `~/.omp/agents` there is a project directory too,
farther than every level of the checkout. Where no level of the checkout holds one, the runner
reads the directory an installer of the profile's repositories links as `~/.omp/agents` (an
`ensure_link` or `ln -s` line, read at the commit the user agents are read at), when the
`session repository <repository>` or `no session repository` evidence item names a working
directory (the path after "working directory ") under `/home/<user>` or `/root`. A working
directory outside the home reads none, and the record says nothing of it, as Oh My Pi's walk
never reaches it. A working directory no evidence item names, or an installer line that names
`~/.omp/agents` other than as such a link (a copy, a `mkdir`), reads none, and the record's
`leftOut` says why. The levels between the checkout's root and the home are no repository's, and
the runner reads none of them. On 2026-11-22 no installer, script or shim of the dev box's
repository names `~/.omp/agents`, at its last commit or at 2026-10-31's (they name
`~/.omp/agent/...`, `~/.omp/plugins`, `~/.omp/browser-relay`, `~/.omp/skill-farms`,
`~/.omp/stats.db` and `~/.omp/profiles`), so it is read for no case, and none of the 15 session
repositories of the cases holds `.omp/agents` at its default branch.

A profile is read at the prompt build its session ran under. Oh My Pi 18.18.6 writes a
`system-prompt-digest` line in a session's file when its prompt changes, just before the first
reply under the new prompt (session/session-tools.ts, lines 1818-1827), and a top-level session
reads every skill directory again, a pool's among them, as it starts and on each reload that
rebuilds its prompt (lines 2321-2337, 2466-2506 and 2530-2547). So a top-level case is read at the
line of the build its step ran under: the last of its session's lines at or before its step's
message (`realStep`'s time), or the next line when that one holds the case's `sessionDigest` and
the last does not (the step was the first reply under its build, whose line Oh My Pi writes as
that reply ends). Every repository, a pool's too, is read at that line's time; a case under the
session's first build, and every task (it runs as its session's first message), is read at the
session's start, when Oh My Pi made that build. `build-cases` takes the lines from the transcript,
`--reprofile` from `digest-times.sql`, which serves a top-level session as its own root (one
query a store). The `read times` evidence item names the step's time, the line used (its place,
time and digest) and the time read.

A subagent's profile (a session that recorded its prompt) is read at two times. Oh My Pi 18.18.6
gives a subagent the skills its parent loaded (task/structured-subagent.ts, lines 806-818 and
895-902), a subagent of a subagent its parent's, and the subagent rescans only the skill
directories an extension names (session/session-tools.ts, lines 2350-2452). So each repository is
read at the root session's last prompt build before the subagent's start, and each repository a
skills-sources pool clones at the subagent's start. A pool's host is read at both: its plugin
lists, installers and settings at the root's build, and its pool directories at the subagent's
start; the `read times of <repository>` evidence item names both commits (`its pool directories
at <sha>`, which context.ts reads). The dev box's own pool units come from its host's tree at the
subagent's start too: Oh My Pi reads `skills-sources.json` again as a subagent starts (its
extension's `resources_discover`) and merges what it gets into the parent's snapshot. A pool that
list names at the subagent's start and not at the root's build is listed only where its clone was
on the host then, which no repository records, so the profile installs it only where the
subagent's recorded listing names one of its skills (the unit's evidence says `pool at the
subagent's start`; one whose skills the listing does not name is left out, with why). This is an
inference from the recorded listing, which is also what the listing measure compares against, so
the measure marks each name installed that way (`fromRecorded`, below). The root is the file the topmost `<…>Z_<id>` directory of the
subagent's path stands for (a file below no such directory is its own root). `digest-times.sql`
reads the time and digest of each of the root's digest lines, one query a store for
`--reprofile` and one a session otherwise, and no other text of the file. With no build before
the start, every repository is read at the subagent's start. The `read times` evidence item names
both times and which was used; a pin whose tag has no commit at the time read leaves its
repository at the commit read, and the evidence says so.

A package a plugin list pins by an exact npm version (`"@scope/name": "1.2.3"`, no range) ships
the skills of that release, not its repository's default branch: `bun install` installs that
version. Its release tag is `<directory>-v<version>` of its repository (`pi-envoy-v15.1.2` names a
release of `packages/pi-envoy`), or `v<version>` for a package at the repository's root; when
GitHub lists that tag (`gh api repos/<repo>/git/matching-refs/tags/<tag>`) and its tree holds the
package, the package's skills are read there, as copies.ts reads a package, and installed at that
commit. Else the registry's tarball of the version (`https://registry.npmjs.org/<name>/-/<name less
its scope>-<version>.tgz`, one small download) names the skills it ships, each installed from the
repository at the commit read; a version of neither ships nothing to the profile. Each read is a
`version of <unit>` evidence item. The dev box's own plugin lists come first when two pin one
package. A try of the tarball's download is given up after 60 s, its body included; a try that
failed for a transient reason (that limit, a connection refused or reset, a name not resolved, an
HTTP 429 or 5xx) is sent again after 5, 15, 45 and 135 s, as a GitHub read is, each retry said on
standard error and counted with the GitHub reads' (`githubRetries`). A 404 is no tarball; any other
failure, and a download that failed on every try (its error naming each try's), fails that profile's
build, and the next build that needs the tarball downloads it again.

A skill at its repository's root (a clone that is one skill, or a package whose manifest declares
`.`) has the path `.` in a profile, which cases.ts's schema and load accept. A task profile is
checked against the schema load reads it with (cases.ts's Profile and a context) before it is
written: one load would refuse is never written, and its task fails (`--profiles-out`) or is left
out (`--reprofile`) with the schema's words.

A dev-box session in a checkout of a repository whose `.claude/settings.json` turns plugins on in
`enabledPlugins` lists those Claude Code plugins' skills: Claude Code installs them for that
project, and Oh My Pi 18.18.6 lists a project-scope install for the active project
(discovery/helpers.ts, `loadClaudePluginRoots`), at the rank of a Claude Code plugin, a second copy
of a name under the plugin's name where its text differs. A user-scope install it lists only where
its `enabledProviders` names `claude-plugins` or `claude`, which no config the runner reads sets,
so the plugins the dev box's own settings enable are not installed elsewhere. The registry that
holds the installs is host state no repository holds.

Each case and each load task's profile carries a `listing measure` evidence item, whose source
is one JSON object (`ListingMeasure` in build.ts): how far the listing its rebuilt profile gives
(its skills less those Oh My Pi hides, the tested skill included) is from the listing its
session saw. A session that recorded its system prompt (a subagent's) is measured against that
prompt's `<skills>` lines (`session listing`). A top-level session records only its prompt's
digest, and the build makes no prompt, so `rebuiltDigest` is always null; it is measured against
the recorded listing of a subagent it started under the build its profile is read at (`subagent
listing`: the earliest one-level child of its directory whose `session_init` holds a listing and
whose header's time falls between that build's line and the next, or before the second line for
the first build; read with `subagents.sql`, one query a session as `build-cases` pulls it and one
a store for `--reprofile` and `build-tasks --profiles-out`, since Oh My Pi gives a subagent its
parent's skills: task/structured-subagent.ts, lines 811-818 and 900; task/executor.ts, lines
662-666 and 5256), else it is `not measurable`, with why: a subagent spawned under another build
is never used. The measure says the store holds no such subagent only after `subagents.sql` was
asked, and names the window it asked for; a session the store was not asked about (it recorded
its own listing, no build of it is known, the store no longer holds it, or the query failed) is
`not measurable` with that reason. A task the reader picked from a session is asked about by the
path hash its profile was rebuilt from, as every other session is. A context session
(`--context-session`) has no step: its measured subagent is the earliest of all whose
`session_init` holds a listing, found first, and its profile is read at the build that subagent
was spawned under (the session's last line before the subagent's start), so the measure compares
one build with itself. The
item names the recorded names the profile does not list (`missing`), those it lists that the
recording lacks (`added`), and `missingShare`. Only names are compared: Oh My Pi lists under each
name a model's hint for the skill or a preview of its description (system-prompt.ts, lines
981-983; extensibility/skill-descriptions.ts, lines 448-476), never the frontmatter's text. The
summaries of `build-cases`, `build-cases --reprofile`, `build-tasks --profiles-out` and
`build-tasks --reprofile` count the measures of each kind and sum the missing and added names
(`listing`); a task that keeps its old profile is `not measurable`, with why. A measure whose
profile installed names only because the recorded listing names one of their pool's skills (a pool
new at a subagent's start, above) lists those names as `fromRecorded`: the measure cannot see a
miss of them, since the same listing decided them. The summaries count such measures
(`listing.fromRecorded`), and so do `behaviour report`'s verdict line and `load`'s summary and
listing line. The measure drops
nothing here: `behaviour` and `load` apply the listing rule to it (above, under each command).

`--reprofile` builds the profiles of an existing case file again that way, from the cases and
their sessions' heads (no transcript pulled, no reader call), and writes a new file in which only
`profile` and the profile's part of `evidence` differ: each case's `source`, its `kind` among it,
stays. A written case's profile is built again from the reads of its context session its evidence
names (`context session read: …`), which its messages do not hold, and its written case's
evidence stays. A case holds no working directory, so each
case's is the header's `cwd` in its session's head, read from its store with `session-head.sql`
(one query per store, through the configuration's `psqlCommand`; each head is kept under
`<out>.sessions/`). For a case whose session the store no longer holds, the evidence says so
(`no session head`), and the session repository is the one its old evidence names (`session
repository <repo>` or `no session repository`); a case built before that evidence was written is
taken to be in the workspace of the repository whose `AGENTS.md` its old profile holds. A Legion
session's
role comes from the daemon's first message to it, its catch-up notice, or the claim it holds
beside its workspace; a subagent's session (`subagent:<agent>`) keeps the system prompt and tools
its `session_init` line recorded, and its capture sends that prompt with only the skill's
listing line changed per arm (in a `none` that lists another source's copy bare, less its
`<namespace>/<name>` line). Once a step is found, two more readers decide whether the case is
kept. The `restate` reader reads every message the case keeps before the step, back to its first
and less the skill's own read results, whole (a long message in overlapping parts), in windows of
at most 100,000 characters and at most 10 windows; a restatement of the line in any window drops
the case (`history-restates-line`), and a case whose messages it could not read in full, for too
many windows or a window it could not read, is dropped (`restatement-check-incomplete`), never kept.
The `audit` reader reads the line, the messages before the step as the judge reads them and the
real step, never the criterion, and answers `follows`, `breaks` or `not-governed`; only `follows`
keeps the case (else `audit-breaks`, `audit-not-governed`), and its answer and reason go in the
case's evidence. The summary records, per session, the check's messages read beside those it must
read, its windows, and the audit. `--budget-usd` stops the build once its readers cost that much at
list price (exit 3). The case file and its spool hold real session text, so `build-cases` (both
forms) refuses an `--out` inside a git or jj checkout (exit 2).

After each session decided, the build reads its peak resident memory (`VmHWM`, or the largest
`VmRSS` read each second on a kernel that keeps no `VmHWM`, as gVisor); above 1 GiB it stops as at
the budget (exit 3), with the cases built so far written: each session still open is stopped and
recorded `stopped before its case was built: the peak memory passed 1 GiB`, so it is not decided,
and a run that goes on reads it again. The summary's `stopped` then says so (`the build's peak
resident memory, <n> MiB, is above 1 GiB, so no new session starts`). Every summary's
`peakRssBytes` holds the largest peak read in the run, in bytes, and the last line printed gives it
(`…, peak memory <n> MiB; written to …`), so one run measures the memory a build takes.

The stop rule stops a skill's build (exit 4) as soon as
`--stop-zero-case-usd` dollars (60 unless given) are spent with no case built, or, once at least
`--stop-after-sessions` sessions (40) are decided, when the cost projected to `--max` cases from
the build's own cases and spend passes `--stop-projected-usd` dollars (200): projected = max ×
spent ÷ cases, unbounded with no case, so a build with no case after 40 sessions stops. It is
checked after each session decided, before the build reads another, and the projection needs
`--max`. The summary's `stopped` then holds `{rule, sessions, cases, spentUsd, projectedUsd}`
(`rule` the flag's name, `projectedUsd` null when unbounded); the sessions still open are
`not-needed`, and each drop keeps its reason. A run that goes on checks the rule on the sessions
the earlier runs decided before it reads the store, so a build the rule stopped stops again at
once with the same bounds.

Each session's transcript is pulled into a file of its own beside the output (`<out>.spool/`,
removed once the session is decided, and the directory once the build ends), one row of the
pull's output at a time, and read from there line by line. Only the entries the builder reads
whole are held whole (the header, a `session_init`, model and thinking changes, context resets, a
system prompt's digest, Envoy role claims); every other entry is held light (a message with its
roles, tool calls and `[Skill file: …]` line) and read whole again only where a check or a reader
needs it (the reader's windows, the reads it offers, the case's messages, the compaction before
the step). So a session's memory follows its number of entries and what its case needs, not the
length of its file.

`--concurrency <n>` (4 unless given) lets `build-cases` read up to n sessions at once; the store is
still read one query at a time, and the gateway takes n calls at once. Each session's outcome is
decided and written in the seeded order, a session that finished first waiting for those before
it, so the cases kept are the first `--max` cases of that order, as one session at a time would
keep them. At most n sessions are open (being read, or read and waiting), so at most n spool files
are there at once. When the build ends, each session still open is stopped before its next
reader call and recorded with what it spent, as `not-needed` (or, at the budget or the memory
stop, as stopped by it).

`--drop-navigation` (both forms above; without it every build is as before, as a build for a test
of a skill's old text against its new text needs) gives no case whose line is a navigation line:
a line whose only instruction that the step can show is to read or open a file of the skill. The
arm with no skill does not have that file, so it fails such a step whatever the skill's text
teaches. The `navigation` reader (`prompts/navigation.md`, the audit's model and settings) reads
each line the `case` reader proposes, its file's path and the path of each file of `--old` (each
file at any depth, a symbolic link to a file among them, as `readers.ts`'s `skillFilePaths` lists
them), and answers `{"navigation": true|false, "reason": "..."}`. It judges each line once in a
build (one process), by its file and number: every session that proposes the line, and every
written task under it, takes that one verdict (a reader at temperature 1 could otherwise call a
line navigation for one session and not for another, and keep a case under a line the summary
lists), and its spend is counted once, in a session's `readerUsd` on the session whose proposal
made the call. The verdicts live in the process: a build that goes on from a stopped run starts
with none, so a line asked before the stop and again after it is judged again. This is the only
rule that drops a step for
a read: a step that reads a file of the skill under a line that is not a navigation line stays a
case (a step under "Read `references/scope.md` and resolve the reviewed diff" is one). In the
session form, a step under a navigation line gives no case, and the files it read are the files of
its replacement: those its `read` calls read by the rule the arms and `behaviour report` use
(`cases.ts`'s `stepSkillFiles`: a read of `skill://<skill>/<path>`; a whole read by path, absolute,
relative or under `~`, of the copy a case at the step would name (`pathCopy`: the cut's copy that
the session's first read by path a case is built from, before the step, read); or, where there is
no such copy, a read of a path that ends with `<installed dir>/<path>`, the installed directory
being the one the session's first read before the step shows). The `case` reader goes on from the
window that starts right after that step, whether the reader saw it in its window or among the
messages shown before it (so no later step of that window is passed over), within the 6 windows,
offered only those files' lines (the lines the session's own reads of them held, as the reader's
offered lines always are).
Its first proposal there is the replacement case, with that file's line as its line: it must be a
later step, and it goes through every check any proposal goes through, the navigation reader
first, then the line's checks, the restatement check and the audit (and, later, the judge
controls). Where the step read no file of the skill, the session's reads of those files hold no
line of the base text, the windows run out, the replacement's own line is a navigation line, or
any check drops the replacement, the session gives no case: it is dropped
`navigation-no-replacement`, its detail naming the navigation line and why. A session gives at
most one case, as without the flag. A session whose navigation reader fails before any step is
dropped for a navigation line is dropped `navigation-failed`. Each
session record names the steps dropped for a navigation line (`navigation: [{messageIndex, file,
number, text, reason, replacement}]`: the line's file, its number there, found as a case's line
number is, its text, the reader's reason, and the id of the case its replacement gave, or `null`).
The summary's `navigation` is `{lines: [{file, number, text, dropped}], dropped, withReplacement,
withoutReplacement}`: each navigation line, by file and then number, with the steps dropped for it,
and how many dropped steps got a replacement case and how many did not; without the flag its
lines are none and its counts 0. With the flag, the last line printed adds `<n> steps dropped for
<n> navigation lines (<n> with a replacement case, <n> without) (--drop-navigation)`. In the
`--written` form, the `written-case` reader is given the navigation lines of the session build
(the `navigation.lines` of the summary beside `--session-cases`, `<session-cases>.summary.json`;
none when there is no such file), so it knows which lines only send the agent to a file of the
skill: a task's history is still the task and the skill's read alone (below), and the agent reads
any such file itself in the run; a file there that is no build-cases summary naming its navigation
lines is refused (exit 2). A task whose line the `navigation` reader calls navigation is dropped
(`written-navigation`), before its restatement check, with no replacement; a task whose following
step reads a file of the skill under another line is not dropped for that read; and its summary's
`navigation` lists those lines in the same form. The
navigation reader's spend is counted in `readerSpend` with the other readers'. Both summaries
record the flag in their inputs (`dropNavigation`), and a run that goes on from one of the other
value is refused (exit 2); a summary written before the flag goes on as one without it.
`behaviour report --build-summary` names the navigation lines of these summaries that no case
tests, and counts their replacement cases before and after the judge controls.

`build-cases --written <n>` (the second form above) writes up to n written cases of one skill, for
a skill whose session cases give fewer than 6 informative cases in the screen.

A written case is a written task in a recorded session's context, not that session run again.
The context session is the one `--context-session` names (`<store>:<file hash>`, the hash the
sessions file names a file by), else the first of `--sessions` in the order `--seed` gives that is
on a model the gateway serves, whose rebuilt profile installs the cut repository's (or the set's)
copy of the skill and whose working directory is in a checkout the build knows (below); when no
such session is on a served model, the first such session on any model.
`--context-session` with `--sessions` or `--seed` is refused (exit 2),
since the build would read neither. A session gives a context as a session gives a case: its first
successful read of the skill is the earlier of its first successful `skill://` read, which
`attribute.ts`'s rules must place at the cut repository's copy, and its first successful whole read
by path (absolute, relative to its working directory, or under `~`, as above) that they place at
the cut repository's copy; a session whose only successful reads of the skill are reads by path of
another kind is passed over under the first of them that applies (`path-range`, `other-copy`,
`unattributed`, `path-relative`), as the session form drops it, and one with none as `no-read`.
The model and thinking level are those in effect at that read, its context (`devbox`, its Legion
role, or a subagent's recorded prompt and tools) is rebuilt as build-cases rebuilds a case's, and
its profile is built from its reads up to and including that read (a read by path of the copy
among them), as a session case's is from the reads before its step, at the time `--reprofile`
reads a written case's (a written case records no time of its step): a top-level session's at its
start, its first prompt build, and its listing measured against the earliest subagent of it
spawned under that build; a subagent's at its root's last prompt build before its start. The
summary's `contextSession` names
it, how it was chosen, and each session passed over before it with why (a drop reason and its
detail, `model-not-served` for a session on a model the gateway does not serve,
`no-user-text: <detail>` for a session whose first user message holds no text, whose kind of work
the written tasks take (the detail: `the context session <store>:<hash> holds no user message with
text, whose kind of work the written tasks take`),
`no-checkout: <detail>` for a session whose working directory is in no checkout the build knows
(its header names no working directory, or its profile no session repository, the detail giving
the profile's evidence of why; or the directory is not at the profile's path in the checkout),
whose runs would get no answer to any read-only call,
`outside-material: <detail>` for a session whose recorded system prompt, role's prompt or first
user message names by an absolute path outside its checkout's root a file the session itself
reads, by a `read` call or with `cat`, `head`, `tail` or `sed -n` in a `bash` call (a skill's
file, read by `skill://`, shown as a skill file or under a skill's installed directory, is none;
the detail names the path, where it is named and the message that reads it), whose runs would ask
for material the checkout copy answers no read of, `listing-misses: <detail>` for a session whose
listing measure, the one its written cases would carry (`listing measure` in a case's evidence,
above), the screen's own rule (`score.ts`'s `listingStanding`) drops: its rebuilt listing misses
more than 10% of the names of the recorded listing it is set against (the detail is the rule's
text, the count and the names; a measure that says not measurable, as one does for a session that
records no prompt and has no subagent with a listing under its build, is no reason, since the
screen keeps such a case), whose every case the screen would drop before any model call,
`skill-as-material: <why>` for a session
whose work, by the written-case reader's answer, is on the skill's own text,
`reader-refused: <refusals>` for a session for which the written-case reader's every reply, on
each of its 3 tries, stopped at the model's refusal (`stop_reason` `refusal`: the model refuses
that session's material), or `reader-failed: <refusals>` for a session for which the reader's
answer was refused on each of its 3 tries in any other way (its schema refuses it, as it refuses
a task whose line is not the line of the skill's file it names, or a reply holds no JSON, some of
the replies perhaps stopped at refusal); the detail of either is the reader's refusal of each
reply, which names each refused field, not the answer's text, and the tries' spend is in the
summary's `readerSpend` as every reader call's is; a reader whose call fails still fails the
build, exit 1, as does an answer of its second ask refused on each try, below): such a session is
passed over, a `no-user-text`, a `no-checkout`, an `outside-material` or a `listing-misses` one
before the reader is
asked (the listing measure is read from the store with subagents.sql for a top-level session, as a
written case's is), as the
session form drops a `skill-as-material` one, and the next candidate taken; a `--context-session`
of one is refused, exit 1). A case's
`model` is the context session's when the gateway serves it; else the served model with the most
reading sessions of the skill in `--reading-models` (the skill's `<cut or set id>/models/<skill>.json`);
else the served model with the most reading sessions summed over the `--model-pool` files (each a
models file of a skill of the set: give the flag once per file); with none, the build fails
(exit 1). The `written-case` reader (`prompts/written-case.md`) is called once for each context
session it is asked of: it reads the base text (`--old`, each line numbered), the navigation lines
of the session build (`<navigation>`: with `--drop-navigation`, those of the summary beside
`--session-cases`, as above; else none), each session case of `--session-cases` (its real step and
its criterion; none when the flag is not given), the context session's checkout (`<checkout>`: its
repository, root and working directory, and every file of the commit's tree; for a tree of more
than 2,000 paths, or one GitHub cuts short, a partial listing (`build.ts`'s `partialListing`): the
tree's directories, shallowest first, at most 5,000, then the files under each directory the
session's first message names (a word of it that is such a directory, from the root or the working
directory, or a path under one) and under the working directory, at most 3,000, its `listing`
saying what it holds and where each part was cut (and that GitHub cut the tree short, where it
did), the prompt telling the reader the list may be partial) and its first user message, and
writes at most n tasks in which a line of the skill
(of any file of it, none of the navigation lines) decides the agent's act once it has read the
skill and looked at what the act needs; or `{"tasks": [], "material": {"why": …}}` when the
session's work at the skill's read is on the skill's own text. Each task comes with the task (the
user's message), `existing` (each path the task treats as already in the checkout, as it names
it; never a path it asks the agent to create), the line (file, number and text; its schema refuses
one that is not that line of the base text, or that is a navigation line given), the criterion, a
following step and a violating step. `--written-answer <file>` gives that answer in a file, read
in place of the reader's call and checked by the same schema (its count against `--written`;
refused, exit 2, when it is none); the summary records its path and SHA-256
(`inputs.writtenAnswer`) and `tasksFrom`, and every check below runs on it as on the reader's. A
task one of whose `existing` paths the checkout does not hold at the commit is dropped as
`written-missing-path`, naming the path; a path it asks to
create is never checked. When every task of the reader's answer is dropped so, the reader is asked
once more, the same input with `<absent>` naming each such path as absent from the checkout at the
commit (one retry, its spend counted with the readers'); its tasks are decided as the first
answer's, numbered on after them. The summary records the retry (`retry: {"absent", "tasksWritten"}`,
or `null`) and each task's answer (`tasks[].answer`: 1 or 2), and the last line printed says the
reader was asked twice. A `--written-answer` gets no retry. Each task gives a case whose `messages` are the task as the user's message,
an assistant call `read skill://<skill>` and its result, and nothing else: the base text's
`SKILL.md`, answered as the arms answer a history read, in the directory the context session's
first read of the skill showed (for a read by path, the directory of the copy it read: the path
read, resolved, less the file inside the skill). The agent reads any other file of the skill
itself in the run, where the skill-read rounds answer it, and looks at the checkout in the
checkout rounds above. Its `realStep` is the following step, its `violatingStep` the violating
one, its `source` the context session's store and path hash with `kind` `written`, its `checkout`
the context session's (`{repo, commit, root, cwd, jj}`: the profile's session repository and
commit, the checkout's root on the session's machine, the working directory, and whether the
context session ran jj, a `bash` call of its transcript whose command holds the word `jj`, which
the case's own history, the task and the skill's read, cannot show; a case built before the build
passed over a context session in no checkout it knows may have none, and its runs answer no
read-only call, above), and its profile, working directory and system context the context
session's; its
evidence names each read of the context session its profile is built from, and so how it read the
skill (`context session read: message <n> reads [Skill file: <path>]` for a `skill://` read,
`context session read: message <n> reads <path> by its path` for a whole read by path of the cut's
copy, its source naming the copy's directory), the task (`written case: task <n> of the
written-case reader`, or `of --written-answer <file>`), each existing path (`written case: treats
<path> as there`), the checkout (`written case: checkout <repo> at <commit>`, saying whether the
context session ran jj), the context
session and the model. Secrets are taken
out as from a session case. The `restate` reader reads the task and the read call (never a
file's text): a restatement of the line drops the task (`written-restates`). The `audit` reader
reads the line, the messages as the judge reads them and the following step: anything but
`follows` drops the task (`written-not-followed`). The screen's three judge controls run on a
written case as on a session case. The summary (`<out>.summary.json`) records the inputs, the
context session, the model, where the tasks came from (`tasksFrom`), each task's line,
`existing` and outcome (its case, or its drop reason and detail, with its restatement check and
audit), the drops by reason, the readers' spend and the largest peak resident memory read
(`peakRssBytes`). The build
does not go on from an earlier run: an `--out` that is there is refused (exit 2). `--budget-usd`
stops it once its readers cost that much (exit 3), with the cases built so far written; so does a
peak resident memory above 1 GiB, read after each task decided (`stopped`: `the build's peak
resident memory, <n> MiB, is above 1 GiB, so no new task starts`). Put the written cases after the
skill's session cases in one file for `behaviour` (`cat cases.jsonl written.jsonl`), which screens
the session cases first.

A build that pulls transcripts (`build-cases`, `build-tasks` with `--model` or `--profiles-out`,
`build-tasks --reprofile`, `build-tasks --first-read`) holds its spool `<out>.spool` by a lock
file in it, `run.lock`, which names its process. A second run of the same `--out` is refused
(exit 2), naming that process, while it runs, so no run empties or removes the spool of another.
A lock whose process runs no more (a run that was killed) is taken over, and the run says so.
`build-cases` takes the lock before it reads or writes anything at `--out`. When a build stops for
any reason, it stops each open session and waits for it before the spool is removed.

The runner starts at most 4 gh processes at once, across all its GitHub reads (copies.ts's
`spawnGh` and context.ts's reads, through common.ts's `ghLimiter`): a pod's gh is a wrapper of
about 145 MiB a call, and one profile can start a read for every repository at once. The
environment variable `SKILL_CUTS_GH_CONCURRENCY` (a whole number of 1 or more) sets another bound.

Every child the runner starts and every download it makes has a limit. Each Oh My Pi and Claude
Code run is killed after 10 minutes (`RUN_TIMEOUT_MS`). The other commands are ended at their limit
and fail naming the command and the limit (common.ts's `timeLimit` and `pastLimit`): `omp
--version` after a minute (`OMP_VERSION_TIMEOUT_MS`); `claude --version` after a minute, and
`claude plugin marketplace add` and `claude plugin install` after 5 minutes each
(`CLAUDE_VERSION_TIMEOUT_MS`, `CLAUDE_PLUGIN_TIMEOUT_MS`); the commands that build a context
(context.ts's `COMMAND_TIMEOUT_MS`): `tar` after 10 minutes, `bun install` after 15, `bun run
build` after 10 and `go test` after 15; and `--after-case` after 10 minutes
(`AFTER_CASE_TIMEOUT_MS`), when it counts as a failed `--after-case` command. Each limit is over
100 times what one run took on the pod (each constant's comment gives the measure). A gh call is
ended after 120 s (`GH_CALL_TIMEOUT_MS`, context.ts's API reads too; its tarball read after 15
minutes, `GH_TARBALL_TIMEOUT_MS`), and an ended call is
sent again as a transient failure (`time limit`).
A store command is ended once it prints nothing for 20 minutes (`STORE_QUIET_MS`): the store's SQL
ends itself after 15 minutes, and psql prints only after the query ends, so a pull that is still
printing rows is never ended. A store read that is ended
fails that read, naming the command and the limit: a pull is the `pull-failed` drop, and any other
read fails the run. Each wait for a GitHub rate limit is said on standard error before it starts:
the call, gh's reason, the wait and when it ends. A refusal for GitHub's secondary rate limit, which
has no reset, waits a minute, doubled at each refusal. A refusal for the primary limit waits until
the reset of the installation that refused the call, at most 15 minutes: the pod's gh sends each
call with the GitHub App installation of the owner the call names, and each installation's limit
resets at its own time. copies.ts, context.ts and the profile's reads here use the same rule
(common.ts's `rateLimitWait` and `untilInstallationReset`), which reads the reset with one call that
names the same owner. For a REST call it is `gh api -i --method GET repos/<owner>/<repo>`
(`users/<owner>/repos?per_page=1` for a call that names the owner alone), whose
`X-RateLimit-Reset` header gh prints with `-i` on an answer GitHub refuses too. For a GraphQL call
(`gh api graphql` with an `owner` field, or `gh repo list <owner>`) it is a `rateLimit { resetAt }`
query with the same `owner` and `name` fields, or that header of its answer when GitHub refuses the
query. A line on standard error names the owner, its reset and the end of the wait. A call that
names no owner (a code search whose answer gives no reset), and a read that gives no reset or a
reset already past, wait the full 15 minutes. `gh api rate_limit` is not read: it names no owner,
so the pod's gh answers it with its default owner's installation, whose reset can come before the
refusing one's. A call is sent again after at most 5 waits; a profile read GitHub still refuses
after them fails as a read that fails for a transient reason on every try does, so no SKILL.md it
held is left out of a profile in silence: `build-cases` drops the session as `profile-failed`, and
`build-cases --reprofile` leaves the case out, names the read in the summary's `leftOut` and
exits 1.

A stopped build goes on where it stopped. The summary records the run's inputs: the cut, the
skill, the seed, whether `--drop-navigation` is given, and the SHA-256 of `--old`'s files, every
file of `--new`, the sessions file, the copies files read and the count's query outputs. Run again
with the same `--out` and the same inputs, `build-cases` keeps every session already decided
(dropped, or a case the cases file holds), with no pull and no reader call, and goes on in the
same order from the first session not decided, until `--max` cases in all; the budget counts the
dollars already spent, and the summary's spend adds the runs, while its `peakRssBytes` is the last
run's own (a summary written before that field goes on as well). A session `not-needed` or
stopped by the budget or the memory stop is not decided, so it is read again when it is needed.
It refuses (exit 2, files left as they were) other inputs, naming each difference; a cases file
with no summary; and a summary that records no inputs, or no `--new` (one an older `build-cases`
wrote). A kill at any moment leaves both files readable: each case is appended as one whole line,
then the summary is written to a temporary name and renamed; a case line the summary does not
record (a kill between the two) is left out when the build goes on, and its session is read
again. `build-tasks` goes on the same way: its queries, one per store and day, take an hour and
more on the real stores, so the summary records the terms and each query as it ends, and
`<out>.found.jsonl` the messages each found (path hashes and texts; removed once the tasks are
written); a run of the same skill text, stores and window makes no terms call and repeats no
recorded query, and a finished build is left as it is.

The `tasks` reader says of each need task whether the skill governs its first step: whether an
agent doing the task would read the skill at once, before any other act. `build-tasks` leaves out
each need task whose first step the skill does not govern, and its summary counts them
(`droppedNeed`) and keeps each with the reader's reason (`dropped`). The reader must give at least
5 need tasks whose first step the skill governs, and 5 near-miss tasks.

`build-tasks --first-read` writes the need tasks of the load test's positive control. It takes the
sessions file `attribute` wrote for the skill (`{"store", "hash", "path"?}` per line) in the order of
`--seed`, pulls each session in turn, and keeps a session only when its first assistant message
reads the skill (load.ts's rule: an Oh My Pi `read` of `skill://<name>` or of its `SKILL.md`). It
takes that session's first request as a need task as it is, with its secrets taken out, and no
reader chooses among them; a request that an earlier session already made is taken once. The
`tasks` reader then says only whether the skill governs each request's first step, and a request
whose first step it does not govern is left out and counted, with the reader's reason, as above.
The build stops once `--max` requests (5 unless given) are kept, or the sessions run out. The
summary records each session it tried and why it was not taken (`not first`, `same request`, or a
pull that failed). A stopped run goes on as `build-tasks` does: the summary records its inputs
(the SHA-256 of `SKILL.md` and of the sessions file, the seed and `--max`), each session tried
and each request judged, and `<out>.found.jsonl` holds the requests found until the tasks are
written.

`--profiles-out <dir>` (either mode) runs each task in the rebuilt context of the session it came
from. For each task from a session, `build-tasks` pulls the session again and rebuilds its profile
as `build-cases` rebuilds a case's at the session's start: the units its host had and what they
shipped at their commits then (from `--copies` and `--count`), the plugins, the context files, a
subagent's recorded system prompt and tools, and the context (`devbox`, `legion:<project>:<role>`
or `subagent:<agent>`), less the tested skill. It writes that to `<dir>/<task id>.json` (load.ts's
`LoadProfile`), and the task line's `profile` is the file's path. A session whose rebuilt context
does not list the skill (a subagent's recorded listing lacks its line, or another context's
profile does not install it) would test nothing, so it is dropped as `skill-not-listed`: in the
reader's mode each offered message's session is rebuilt first, the drop is counted in
`candidateDrops`, and the next candidate is taken in its place; in `--first-read` the session is
dropped when it is tried, and counted (`skillNotListed`). A written task takes the profile of
`--context-session <store>:<hash>`, the session the reader's mode requires for it (the most-read
session of the context that installs the skill). That profile is rebuilt before any reader call;
the build fails when that session gives none, and fails too (exit 1, since it is found after a
store read) when its rebuilt context does not list the skill. The summary's `profiles` records
the context session, each task's profile with its evidence, and why a task has none. A build
that has already finished gets no profiles from `--profiles-out` afterwards; `--reprofile` builds
them again.

`--reprofile <tasks.jsonl> --profiles-out <new dir>` builds the profile of each task of a tasks
file again, as `--profiles-out` builds one now, with no candidate search and no reader call, so the
tasks a bounded search chose stay as they are. A task from a session takes its session's profile:
the session is found in its store by its path hash (`session-paths.sql`, one query per store) and
pulled as the search pulls it. A written task takes the profile of `--context-session`, else of
the context session the tasks file's summary (`<tasks.jsonl>.summary.json`) names. The command
writes each profile to `<new dir>/<task id>.json`, the tasks again to `<new dir>/tasks.jsonl` with
each `profile` naming its new file, and `<new dir>/tasks.jsonl.summary.json`. The summary says of
each task whether the profile's bytes changed (`unchanged` or `changed`) and lists the evidence
items added and removed against the old summary's. A task whose session its store no longer holds
keeps its old profile (`kept`), and its evidence item `no session in the store` says so. A
session task whose rebuilt context no longer lists the skill is left out and named, and the
command exits 1. A store read, a pull or a rebuild that fails stops the command (exit 1) before it
writes anything to `<new dir>`; `<new dir>` must be new or empty.

`--model <model>` (either mode) keeps only the sessions whose first request ran on that model, as
the session's file names it and `attribute --models` writes it: its last `model_change` before its
first assistant message, a thinking suffix left out (so `<gateway>/openai/gpt-6.1-sol` and
`openai/gpt-6.1-sol` are two models). Every other session is dropped and counted as
`other-model`: in the reader's mode each offered message's session is pulled and the next
candidate taken in its place (`candidateDrops`), and in `--first-read` when the session is tried,
where the summary's `model` (`{name, firstRead, otherModel}`) and the printed line say how many
sessions tried on that model read the skill in their first reply, and say so when they are fewer
than 5. Written tasks are unchanged. The summary names the model, and the inputs hold it, so a run
that goes on with another model is refused.

`--max-candidates <n>` and `--max-search-minutes <m>` bound the reader's mode's candidate search,
which runs with `--model` or `--profiles-out` and pulls each candidate's whole transcript, one at a
time: on the real stores the terms can match tens of thousands of messages, and the search can run
for days. The search ends before its next candidate once the candidates decided (accepted or
dropped) reach n, or its own time reaches m minutes, both summed over the runs of one `--out`; the
time is checked between candidates, so the last one decided can run past it. The `tasks` reader
then gets the messages accepted so far and writes the rest of the tasks (in the
`--context-session` context with `--profiles-out`), as when the candidates run out. Each decision
is appended to `<out>.candidates.jsonl` as it is made, after a first line holding the bounds, and
so is the bound that ends the search. A run that goes on takes each candidate decided there as it
was decided, with no pull, and refuses (exit 2) bounds other than those, naming both values of
each; a file that holds only the bounds, or no file (a run killed during its queries), takes any
bounds. The summary's `search` holds the bounds, what ended the search (`--max-candidates`,
`--max-search-minutes`, `candidates ran out` or `enough offered`), the candidates decided and
accepted, and the search's seconds; `candidateDrops` holds the drops by reason. The file is
removed once the summary is written. With neither bound the search runs to 60 messages offered or
the last candidate, and writes no file. A bound is refused without `--model` or `--profiles-out`
(no search runs) and with `--first-read` (it stops at `--max`). Build a load test's tasks with
`--max-candidates 60 --max-search-minutes 90`.

### `results-index`

```
bun scripts/skill-cuts/cli.ts results-index --count <set-count out dir> \
  --nearest <dir of nearest rows> --cases <dir of case files> --results <dir of results>
```

The inventory of the results of the skills of each set that `set-count` counted
(`results-index.ts`). It lists each skill that `set-count`'s `summary.json` (in `--count`) gives
at 20 or more own-copy sessions (`atLeast20`), in its order, beside its nearest row (a `nearest`
row JSON file at any depth under `--nearest` of the skill's name; a row whose every reply was
refused names no nearest skill, so it is none), its cases file (`cases-<skill>.jsonl` or
`cases-<skill>.jsonl.gz` in `--cases`) and its result (`results-<skill>/` or
`results-<skill>.tar.gz` in `--results`, holding the output of `behaviour report` for the skill as
`report.txt`, whose `verdict:` line it prints; a result with no report, or a report with no
verdict line, is none). It calls no model and reads nothing else. It exits 1 naming each skill
that lacks a nearest row, a cases file or a result, and 2 when it refuses its arguments, a
`summary.json` that is not set-count's, a directory it cannot read, or a `.json` file under
`--nearest` that is no JSON or a nearest row of another shape.

## The request log

Each model request of `behaviour`, `read`, `control`, `craft`, `nearest`, `proposals` and `load`
adds one JSON line to the request log (`<out>/requests.jsonl` for `behaviour`, `load`,
`control craft` and `proposals`, `<out file>.requests.jsonl` otherwise):
what the call was for; when it was asked for (`at`), when it got its place and was sent
(`sentAt`) and how many calls were in flight then, it among them (`inFlight`); the session id it
went with (`userId`: its `metadata.user_id` or `prompt_cache_key`); the model, the thinking type
and budget, `max_tokens` (a Responses body's `max_output_tokens`), the temperature, the effort (a
Responses body's `reasoning.effort`); and the reply's answering model (`answeredBy`), its message
id, its request id and the provider's request id, the gateway's headers of it (`gateway`:
`x-litellm-model-id`, `x-litellm-model-group`, `x-litellm-attempted-fallbacks`,
`x-litellm-attempted-retries` and the provider's request-id headers, each it came with), its
cache read and write tokens, usage and dollars at list price. It never holds the key or a
message's text.

## Usage records

Each model answer the runner gets records one usage span, as a program with no agent harness of
its own must (`skill-scenarios/spend-record.ts`, the spend client's `spend_record.py` in
TypeScript): one OTLP/HTTP JSON span, shaped like Oh My Pi's model-response span, posted to the
spend collector.

- **The one place.** `skill-scenarios/messages-api.ts`'s `postMessages` gives each ok answer of
  each try, its body read whole, to `gateway.ts`'s recorder (`createGateway`'s `spendRecorder`,
  which every gateway must name). Every command that calls a model opens its gateway with the
  process's recorder (`processSpendRecorder()`): `read`, `control` (`control craft` among them),
  `craft`, `nearest`, `proposals`, `behaviour` and `load` through `cli.ts`'s `openGateway`,
  `build-cases` (its `--written` form among them) and `build-tasks` through `build.ts`'s
  `readerGateway`. So every replay, judge call, reader call, hint call and load run records its
  answer there, and nowhere else.
- **Which answers.** Every ok answer, the provider having billed each: one the runner then asks
  again for (a reply it cannot read), and a stream that ends in a content-policy refusal after
  its answer started, among them. A streamed answer is read as the whole answer it assembles: a
  Messages stream's `message_start` message with its `message_delta` counts, or a Responses
  stream's `response.completed` response. A stream that ends before its answer starts holds no
  usage, and records nothing; so do a 429, a 5xx, any other refusal and a body that fails while
  it arrives.
- **What a span says.** The provider (from the model: anthropic for a Claude model, openai for a
  GPT one), the body's model and the answer's, the answer's id (`msg_…`, `resp_…`), when the try
  was sent and when its answer ended, the session id the gateway logs for the body (the
  `session_id` of a Messages body's `metadata.user_id`, else a Responses body's
  `prompt_cache_key`, else its `user`), and the token counts: all the prompt's tokens (a Messages
  answer's uncached input with its cache reads and writes), the cache reads, the cache writes
  with their 5-minute and 1-hour parts, the output, and a Responses answer's reasoning. It sends
  no cost: the runner knows only its own list-price count, so the collector prices the row.
- **Where it goes.** `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, else `OTEL_EXPORTER_OTLP_ENDPOINT` and
  `/v1/traces`, with `Content-Type: application/json` and no other header, never through a proxy
  (Bun's fetch reads `http_proxy` and the rest; each post sets `proxy: false`). Its resource
  attributes are every pair of `OTEL_RESOURCE_ATTRIBUTES`; `service.name`, from
  `OTEL_SERVICE_NAME`, else `spend-record`, where no pair names one; and `theorem.harness=script`,
  in place of any pair of that key. An environment with neither endpoint variable records
  nothing, and stderr says so once.
- **Never in the way.** A span is queued (1000 at most; one more is dropped) and one sender posts
  them in order, each post ended after 2 s. The first failure is written once to stderr.
  `cli.ts`'s `main` waits up to 2 s for the queue on every way out, once for every command
  (`build.ts`'s `main` leaves the wait to its caller); a run ended by a signal loses what is
  still queued.
- **The child programs.** Oh My Pi (`behaviour`'s captures, `load`'s captures and their hint runs)
  and Claude Code (`load`) run with an environment the runner builds from nothing, which holds no
  `OTEL_*` variable (and, for Claude Code, `DISABLE_TELEMETRY`), so neither records a span. Their
  model requests go to the recorder (`recorder.ts`), never to the gateway: it answers each turn
  itself, which is no model call, and sends each hint request it is owed through the gateway,
  which records that answer once. Given the OpenTelemetry variables, they would record the
  recorder's stand-in replies as model calls, and each hint call twice.
- **A backfill.** `recordLogged(line)` queues the span of one line of a request log (above) with
  the line's own start (`sentAt`), end (`durationMs` later), ids and usage; a line of a failed call
  is passed over. A backfill queues at most 1000 lines at a time, then awaits `flush()` until it
  returns true, the queue emptied, before it queues more: `flush` returns false at its time limit
  with spans still queued, and the queue drops what it has no room for.

## Exit status

0 done (a control held); 1 failed (every reply of a reader refused, a control that did not hold, a
call, a GitHub read, a load test's task or an `--after-case` command that failed, an item line of
`proposals` that does not parse, an item it leaves with no entry, a skill `results-index` finds
with no nearest row, cases file or result, a skill `craft-coverage` finds with no craft entry, a
line `craft-lines` cites that cannot be read at its commit, or `behaviour` results that show a
drop no stage makes); 2 refused before any model call or store read (among the refusals, an
`--out` inside a git or jj checkout, from `behaviour`'s stage commands, `load` and
`build-cases`), or as a case's capture installs a copy Oh My Pi would name otherwise than its
profile does, or as a saved capture's installed copies, made again, are not the ones its record
names; 3 stopped (the budget is spent, the peak memory passed 1 GiB in `behaviour` or
`build-cases`, or 1.5 GiB in `copies`, or the listing rule dropped more than half of a load's
tasks); 4 stopped by build-cases' stop rule.
