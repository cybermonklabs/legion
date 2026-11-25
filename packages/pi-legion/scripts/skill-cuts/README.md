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
repositories copies map to, the dev box's home and its Claude Code config directory, the clones
made by hand on a dev box, the session pod's deployment repository and image pin, the file where
the dev box's own repository pins its Oh My Pi build, the registries read with a token and the
images a deployment builds) comes from the `--config <skill-cuts.json>` file at run time
(`config.ts` says each key: `devboxHome`, `sessionPod` and `ompPin` among them). No file here
names one. The gateway key comes from `--key-file`, read inside the process that uses it, never
printed, logged or written. The behaviour test and the load test read the skill texts from
directories and the cases or tasks from a file outside the repository. The repositories a context
needs come from
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
`behaviour` and `load` need no GitHub token only with `--repos`. `copies`,
`build-cases` and `build-tasks --profiles-out` read GitHub through `gh`, and only read it: GET
calls, and GraphQL queries that read file contents in batches.

## Commands

### `behaviour screen|confirm|control|report`

```
bun scripts/skill-cuts/cli.ts behaviour <screen|confirm|control|report> \
  --config <skill-cuts.json> --key-file <file> --out <dir> \
  --old <skill dir at the cut's base> --new <skill dir in the cut> --cases <case file> \
  [--planted-line <text>] [--budget-usd <n>] [--concurrency <n, default 8>] \
  [--after-case <command>] [--request-log <file>] \
  [--models <models.yml>] [--omp <binary>] [--repos <dir>] [--hint-model <model>] \
  [--omp-config <file>]... [--cache-dir <dir>] [--scratch <dir>] \
  [--reading-models <file>] [--again <case id>]... [--confirm-flagged] \
  [--allow-runners <digest>[,<digest>...]]
                                 (--again, --confirm-flagged: confirm only)
```

One skill per run: `--old` and `--new` are that skill's own directories, and `--cases` holds that
skill's cases (`cases.ts`), one JSON object per line.

- `screen`: per case, the three judge controls (the real step must pass; the case's
  `violatingStep`, a step that breaks the line, must fail; "I will continue." must fail; a case
  that fails any is dropped, naming the control), then a capture of each arm (old, new, none), 10
  replays of each, each reply judged blind with all arms shuffled, and runs made again for the runs
  with no verdict, at most 5 more per arm. It stops at 6 informative or 15 screened cases. A case
  whose model the gateway does not serve (none of the configuration's `servedModels` has its id,
  the text after its last `/`) is dropped before any call as `model-not-served`; a case on a
  served model runs as that served `provider/model`, so an Anthropic model behind another
  gateway prefix runs as `anthropic/<id>`. A case whose listing measure (its `listing measure`
  evidence item, under `build-cases` below) misses more than 10% of the names its session's
  recorded listing holds is dropped before any call too, as `listing-misses`, with the count, the
  share and the names; exactly 10% is kept (the architect, 2026-11-22). A recorded name the
  profile does not install is answered "unknown skill" when a run reads it, where the session got
  the skill's text, so a miss changes what the runs see. The names the rebuilt profile adds to
  the recorded listing count only for a case whose capture builds its listing from the rebuilt
  profile (a measure of kind `subagent listing`): over 10% of the recorded names it is dropped as
  `listing-adds` (the architect, 2026-11-24), and a case over 10% on both sides is dropped as
  `listing-misses`, its reason giving the adds part too. A case whose capture sends its session's
  recorded system prompt whole (a measure of kind `session listing`: its profile holds the
  session's system prompt, and the capture sends it with only the tested skill's listing line
  changed per arm) is never dropped for added names, however many: the model in every arm sees
  exactly the recorded listing, whatever the rebuilt profile installs, and the profile changes
  there only how a read of a skill is answered, from the profile's installed copy or with the
  unknown-skill reply (the architect, 2026-11-25). `report` counts those reads (below); the count
  drops no case and changes no verdict. The rule reads the capture's own choice from the
  measure: `build.ts` writes the kind `session listing` exactly when the case's profile holds the
  session's recorded system prompt, and `capture.ts` sends the recorded prompt exactly when the
  profile holds one (`build.test.ts` checks it over the cases a build writes), so the rule never
  reads a run's result. `report` applies the rule to saved results as well, so a case with runs
  saved before the listing-adds rule, whose capture builds its listing from the profile, is
  dropped at screen when the report runs, with no model call; a case a runner saved as dropped for
  `listing-adds` before this rule keeps that saved drop (`screen` skips a dropped case and `report`
  prints its saved reason) until its result file is removed. With
  `--reading-models`
  (the skill's `<cut id>/models/<skill>.json` that `attribute` writes: its reading sessions by
  model), a skill with fewer than 6 reading sessions on models the gateway serves is refused
  before any call (exit 2): it gets no verdict, with the reason `too few sessions on served
  models` and its served and total counts, and the walk of the cut's ranking goes on to the next
  skill.
- `confirm`: 40 fresh runs of old and new for each flagged case that is not dropped. Each confirm
  records the runner that made it: a digest of the runner's code (each file of this directory but
  its tests, its test data and this README, and each module of `../skill-scenarios` that its files
  import, with those modules' own imports there). Where a case's last two confirms at the same
  texts on one runner disagree, one a loss and one not, `confirm` runs a third, and the three are
  pooled (the architect's decision "The behaviour test's pass rule"): the case is a loss when the
  cut's text passes at least 30 fewer of the 120 runs than the old text, the same bound of 10 for
  each 40.
  Confirms made on different runners (before and after a fix) or at other texts are not pooled:
  only the last confirm's runner and texts count. `--again <case id>` asks for one more confirm of
  a flagged case whose confirm is finished, on this runner; it is refused (exit 2), naming the
  case, where the case already has three confirms at these texts on this runner (no fourth runs)
  or two that agree (a third runs only where two disagree).
  `--confirm-flagged` confirms each flagged case that is not dropped even where the skill can get
  no verdict (fewer than 6 informative cases, a control that failed, or no case left to be the
  control case), where `confirm` without it makes no run. It is the route for a one-case rerun,
  a case another tree asks to run again: a case file of that case alone, `screen`, then
  `confirm --confirm-flagged`. It runs the same confirm stage (40 fresh runs of old and new; a
  third confirm where the last two at the same texts on one runner disagree, and the three
  pooled), and `--again` works with it as without it. It never confirms a case the screen did not
  flag. Each confirm it runs records it in the case's results (`confirmFlagged`), so `report`
  needs no option to know it: the report of such a run gives the case results as any report does,
  and as its verdict line `verdict: case result only; no skill verdict stands (--confirm-flagged)`,
  never a skill verdict. The other commands refuse the option (exit 2).
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
  leaves the score (the architect, 2026-11-22): a case dropped at screen is not informative; a case
  dropped at confirm or control keeps its screen's passes, and any confirm made before the drop.
  A flagged case that can no longer be confirmed leaves its skill no "no loss detected" verdict:
  the skill gets no verdict, `flagged case <id> could not be confirmed (<reason>)`, unless another
  of its cases is a confirmed loss, which stands as the skill's loss. A dropped control case gives
  its place to the next informative case of the control rule's order (the largest old − none gap,
  a tie to the lower id), chosen by that rule and by no result; with none left, the skill gets no
  verdict, and the reason names each dropped one. A drop at confirm of a case its screen did not
  flag cannot happen, since only flagged cases are confirmed: results that show one are refused,
  naming the case, by `report` and by every stage command (exit 1), not scored. A read that fails
  for another reason, after its retries, fails the command (exit 1), and the next run reads it
  again.
- The runner of each run. Each stage and each run records the runner that made it: a digest of the
  runner's code (each file of this directory but its tests, its test data and this README, and each
  module of `../skill-scenarios` that its files import, found from their imports and followed
  through those modules' own; the commit is not knowable in a copy of the directory with no git).
  A run saved before runs recorded it takes its stage's runner, and a stage saved before stages
  recorded theirs is the runner `unrecorded`. `screen`, `confirm` and `control` take a saved stage
  of their own kind (a case's screen; a flagged case's last confirm; the control case's control)
  as done, or go on with one still running, only when each of its runs, or the stage where it has
  none, is this runner's or that of a runner `--allow-runners <digest>[,<digest>...]` names. A
  saved stage of another runner is refused before any model call (exit 2), naming the case, its
  runner and this runner, and the `--allow-runners` value that takes it: the runners the given
  value names and the others, since a new value replaces the old one. It is never skipped as done
  and never made again. Confirms are pooled on one runner at one set of texts as above, whatever
  `--allow-runners` says.
- `report`: for each stage kind (`screen`, `confirm` with earlier confirms, `control`), each
  runner with its case stages and its runs (a stage whose runs two runners made counts as a case
  stage of each); results whose runs more than one runner made are refused (exit 2), naming each
  runner with its counts, unless `--allow-runners` names each, and the runners line then says
  they were allowed. Then each case's passes per arm and stage; for a dropped case, the stage
  that dropped it, why, and the passes of each stage it ran (its screen's old, new and none
  passes where the screen
  ran); for a case with more than one confirm, each confirm numbered (one on another runner or at
  other texts marked not counted) and, for three pooled ones, their pooled passes, and a
  `pooled confirms` line naming each such case with its three results and the pooled numbers; the
  control case's passes of old, `minus` and `twin` and what each control says, and each dropped
  case the control case took the place of; the skill's verdict and its reason (`keeps` prints as
  "no loss detected by the registered rule"; a run with a confirm made by `--confirm-flagged` gets
  the verdict line under `confirm` above in its place), ending with how many of the skill's
  screened cases have a listing that could not be measured (no listing measure, or one that says
  so); the drops per judge control and the drops counted as `model-not-served`, as
  `unreadable-directory`, as `listing-misses`, as `listing-adds`, as `injection-unrebuildable`, the
  last by kind (a case under each kind it holds), as `path-form`, by the arm whose text holds the
  file, as
  `reads-past-last-round` and as `provider-refused`; the verdict line also counts the screened
  cases whose profile took names from their recorded listing (`fromRecorded`, below); an estimate of
  the procedure's false-loss
  chance at the cases run (exact binomial under identical old and new texts, the pass rates taken
  from each case's own screen; its assumptions are printed with it),
  a table of the chance it finds a loss of 10 to 40 points in one informative case, the spend
  at list price (below), the peak memory, how many captures' system-prompt digests differ
  from their session's, and, for the cases whose capture sends their session's recorded system
  prompt whole (their profile holds the prompt), the `outside recorded listing` line: how many of
  their runs read a skill outside the recorded listing, of the runs that record their reads (a run
  saved before runs recorded them, or whose replay failed, records none, and is counted apart),
  and each such skill with its runs by how the read was answered (`copy`: an installed copy
  answered it; `absent`: the unknown-skill reply). A run that reads a skill in several rounds
  counts once for it, and a read answered from the arm's own text (the tested skill) is no
  count. The line is a count only: it drops no case and changes no verdict (the architect,
  2026-11-25). It calls no model, and needs only `--config`, `--out`, `--old` and
  `--cases`; `--request-log` names the log it sets the spend against, `<out>/requests.jsonl` by
  default. With
  `--reading-models` it also prints each model of that file with its sessions and whether the
  gateway serves it, the sessions whose file names no model and those in no models query's rows,
  and the served models the screened cases ran on, each with its cases; a verdict then ends with
  the reading sessions it speaks for, on each served model its screened cases ran on (a model
  counted under the served model of its id, so `gateway/claude-sonnet-5-5` under
  `anthropic/claude-sonnet-5-5`), and those it does not speak for, on models the gateway does not
  serve and on served models no screened case ran on. A skill too few of whose reading sessions
  are on served models gets no verdict. Without `--reading-models`, it says the reading sessions
  by model were not given, and the verdict names no models.

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
not kept with `--out`. So where the cache lacks one of them (on a new machine, say), the case's
context is built again before the replays, as the capture built it, under a scratch directory of
its own that is removed after: that makes each missing copy again from its repository at its
commit. A saved capture whose copies are all in the cache builds nothing and reads no repository.
One whose copies that build does not give again (the case's profile names another commit or
another copy, or the record keeps a copy under another cache key) is refused (exit 2, or that
task's failure in `load`), naming the case, the arm and the first copy that differs: make it again
under a new `--out`. The run stops above 1 GiB of peak resident memory, and no new model call
starts once the replies cost `--budget-usd` at list price.

Each cache entry a context builds (`<cache-dir>/<kind>s/<key>`: a skill, a repository's checkout under `trees/`, a plugin, a compose, a file or a directory) has a file list, `<cache-dir>/lists/<kind>s/<key>.json`, kept apart from the entries: each file and symbolic link the entry was made with, with its size or its target.
A context that finds the entry in the cache checks it against that list, and a listed path that is missing or changed stops the context with `IncompleteCacheEntry`, which names the entry, how many listed paths differ, and up to 5 of them in full with what differs; removing the entry and its list builds it again.
A file the entry holds beyond its list is no change: a compose writes its Go test into a checkout while it runs.
A checkout with no list, made before lists were kept, is made again, since only a checkout's files keep their commit's file times (from its tarball), so only a checkout can be partly kept by a save that keeps only files newer than a cutoff.
Any other entry with no list is taken unchecked: the runner wrote it when it built it, and making each again would cost a GitHub read per file of thousands of entries.

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
skill file is a `skill://` read of any skill, a whole read by path of the cut's own copy, or a
whole read by path of another skill the profile lists (a path that ends `/<name>/SKILL.md` or holds
`skills/<name>/references/`, as `count.sql` places a read). The case's skill is answered from the
arm; another listed skill from the copy its capture installed (`<cache-dir>/skills/<key>`), the
same text in every arm; in `none`, a `skill://` read of the case's skill from the copy of another
source that `none` lists under the skill's name (below), where it lists one, as another listed
skill's; a skill the arm does not install (the case's skill in `none` otherwise, or a skill the
profile does not list) with Oh My Pi's `Unknown skill: <name>` text, and the run goes on. The judged
step is each answered round's read calls, without their results, then the first reply that holds
another call or none, so a case whose line governs a read of a file of the skill is judged on that
read. A reply after round 3 that still only reads skill files gets no verdict and is run again, as
any run with no verdict is. Each report counts such runs by stage and arm, and a stage that an arm
cannot finish for them drops its case as `reads-past-last-round`, listed with its counts. Each run
records each answered round's reads (`skillReads`: the skill, the file, by `skill://` or by path,
and whether the arm, the installed copy or no copy answered it).

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
  --prompt <case|restate|audit|map|attack|same-job|sentence|names|tasks> \
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
its answer names that finding. It exits 0 when the control held, 1 when it did not.

### `count-sql`

```
bun scripts/skill-cuts/cli.ts count-sql --config <skill-cuts.json> --store <db> --day <YYYY-MM-DD> [--hour <0-23>] [--query count|models]
bun scripts/skill-cuts/cli.ts count-sql --config <skill-cuts.json> --store <db> --query roots|hashes
bun scripts/skill-cuts/cli.ts count-sql --config <skill-cuts.json> --query transcript-coverage [--run]
bun scripts/skill-cuts/cli.ts count-sql --config <skill-cuts.json> --query transcript --day <YYYY-MM-DD> [--hour <0-23>] [--run]
```

Prints `count.sql` filled for one store and one UTC day (or hour), the same as
`bun scripts/skill-cuts/countsql.ts`. `--query models` prints `models.sql` instead: each session
file's model, read from the first 256 Ki characters of the file, as one row per file hash.

For the `attribute` context step, print the store-wide query with
`count-sql --config <skill-cuts.json> --store <db> --query roots`. Omit `--day` and `--hour`.
Run the query with the configuration's `psqlCommand <db>` and save its result as
`<count>/<db>/roots.txt`.
The query reads only the file table's paths. It returns each file's count hash and Oh My Pi
profile, not the path. You can add this file to an existing count without running the count again.

Each count row also names, for each file, the keys of the reads that put it in the row (`calls`,
and `unpaired_calls` for calls with no result). A key is md5 of the call's id, a space and the
call line's UTC timestamp to the microsecond, cut to 16 hex characters. Inside one query, a key
that two files hold counts in the file whose file hash (md5 of its path, 16 hex characters) sorts
first in byte order, and `shared_call_ids` counts such keys.

To include the ClickHouse transcript store, add this optional key to the private configuration:

```json
{
  "transcriptStore": {
    "command": "/home/dev/bin/query-transcripts",
    "hosts": [
      { "host": "host-a", "context": "devbox" },
      { "host": "host-b", "context": "legion:ACME" }
    ]
  }
}
```

`command` is an executable at an absolute path. It receives the SQL on stdin with no arguments,
prints results on stdout, puts diagnostics on stderr, and exits nonzero on failure. `--run`
uses it for either transcript query. Without `--run`, `count-sql` prints the SQL as for Postgres.
The config's `hosts` lists context mappings, not a filter: the queries include unlisted hosts.

1. For each configured Postgres store, run `--query hashes` with no day through
   `psqlCommand <db>` and save its output as `<count>/<db>/hashes.txt`. It returns the same file
   hashes as the count, for files whose mtime is at or after the window start, even if the file
   does not contain a skill read.
2. Run `--query transcript-coverage --run` once, saving stdout as
   `<count>/transcripts/coverage.txt`. It returns per-host and per-format coverage and the hashes
   needed to decide which sessions are new.
3. Run `--query transcript --day <YYYY-MM-DD> --run` once per UTC day in the window, saving stdout
   as `<count>/transcripts/<YYYY-MM-DD>.txt`. If you split a day into hours, save the 24 outputs as
   `<count>/transcripts/<YYYY-MM-DD>T<HH>.txt` instead. Use `transcripts` as the store label in
   `run.log`. Save output files only on success.

The daily query aggregates call-time reads in ClickHouse, tagged `transcripts@<host>`. It emits
the same count rows as Postgres, with file hashes and call keys, plus tool counts per format. It
does not return tool calls or messages. An Oh My Pi read uses the answer's skill-file header
when present. A Claude Code `Skill` read uses its base-directory message when the import can
pair it with the call; otherwise it uses the input. The import may omit the source tool-use id
on the base-directory message. In that case the query uses the meta line immediately after the
result in the same file, provided the line identifies only one Skill call. Failed answers are
excluded. Calls with no result in their file are listed apart and are not reads.

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

Every load applies the listing rule (the architect, 2026-11-22; on added names too, 2026-11-24;
narrowed by how the capture shows the listing, 2026-11-25), so no one applies it by hand. Each
task's listing measure comes from the tasks file's build-tasks summary (`<tasks>.summary.json`): a
session task's own profile's `listing measure` evidence item, and a written task's context
session's; a task with no summary beside its file is not measurable.
A task whose rebuilt listing misses more than 10% of the names its session's recorded listing
holds is dropped before any run (`listing-misses: ` with the count, the share and the names;
exactly 10% is kept). A task whose capture builds its listing from the rebuilt profile is dropped
too when its listing adds more names than 10% of the recorded ones (`listing-adds: `, with the
same reason form); that is a task measured as `subagent listing`, and every task on Claude Code,
whose capture lists the skills its plugins hold and sends no recorded system prompt. A task over
10% on both sides is dropped as `listing-misses`, its reason giving the adds part too. A task
whose capture sends its session's recorded system prompt whole (on Oh My Pi, a measure of kind
`session listing`: its profile holds the session's system prompt) is never dropped for added
names: the model in every run sees exactly the recorded listing, whatever the rebuilt profile
installs, and the profile changes there only how a read of a skill is answered, from an installed
copy or with the unknown-skill reply. A recorded name the profile does not install is answered
"unknown skill" when a run reads it, where the session got the text, so a miss still drops the
task. For such tasks the command counts the runs that read a skill outside the recorded listing,
by skill and by how the read was answered (`copy`: an installed copy; `absent`: the unknown-skill
reply), from the reads each run records: the summary's `outsideRecordedListing` (`tasks` counted,
their `runs`, the runs `examined` that record their reads, the runs that read `outside` the
listing, and `bySkill`) and the `outside recorded listing` line it prints. The count drops no task
and changes no verdict. A dropped task's line and the summary's task give the reason, the
summary's `listing` counts the
tasks measured, not measurable and dropped, and the drops by the start of their reason
(`droppedBy`, also on the listing rule's line), and no verdict reads it. Where the rule drops more
than half of a load's tasks, the command stops before any run (exit 3) and says so with the
numbers: the architect decides such a load. The skill's verdict is made twice, with and without
the tasks whose listing
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
keeps and every blob it read), each finished code search, each prepack script's history and each
read of what git did to a directory a prepack script staged from is saved there the moment it ends,
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
to the copy at the directory that version staged it from, as the tree at `--at` holds it. Where the
tree at `--at` holds no `SKILL.md` there, git says what became of it: the newest commit of the
default branch since `windowStart`, up to the commit read, that changed `<dir>/SKILL.md`
(`repos/<r>/commits?sha=<commit read>&path=<dir>/SKILL.md&since=<windowStart>&per_page=1`), and
that commit's files (`repos/<r>/commits/<sha>`). When that commit renamed the file (status
`renamed`, its `previous_filename` the staged one) to a directory the tree at `--at` holds a copy
at, the rows map the path to that copy, under the name the script staged, its former name: the
row's `name` is that former name. Each of those reads goes through the run's gh rules and is kept
in the cache. The rows name the package's unit, so `build-cases` and `build-tasks` resolve that
unit at a session's commit as they resolve any. Under the script's line, `skill-copies.md` names
each such path with the commits that staged it there (`from <commit> (<date>) until <commit>
(<date>)`) and the rename that maps it, and each path that gets no row, with why: the tree at
`--at` holds no `SKILL.md` at the directory it was staged from and git removed it, renamed it to a
directory the tree holds no copy at, or names no commit that changed it; or the package is no
package unit of the script's own repository. `former-stages.tsv` holds each such path at each
place its package is installed: the script, the package, the skill's name there, the commit from
which the script stages it there no longer, the directory git renamed it to, the copy its prefix
row maps it to (empty for none) and why.

`copies` also writes the former cache locations of a marketplace's plugin, for each target
repository whose marketplace manifest changed during the window. It reads the manifest's versions
in the window (one GraphQL read of its history), and for each plugin a version names by a
directory of the repository (its marketplace's name, its own, its directory) it writes the cache
locations Oh My Pi's plugin cache and Claude Code's gave the skills in that directory's
`skills/`: `**/plugins/cache/plugins/<marketplace>___<plugin>___*/skills/<name>/` and
`**/plugins/cache/<marketplace>/<plugin>/*/skills/<name>/`. A skill still in that directory,
whose marketplace or plugin the manifest names no more, maps to the copy now there. A skill git
moved (a commit that changed the manifest lists its `SKILL.md` as renamed; a later move of the
same skill is followed) maps to the copy at the directory it went to, with `renamed_to` set in
`former-stages.tsv`, so `attribute` counts those reads under `former_name_sessions`. A skill git
removed, or moved to a directory with no copy now, has a `former-stages.tsv` row with no copy, and
`attribute` says in the outcome of a read there that the plugin held it then and no longer.
The limit: a move is seen only in a commit that also changed the marketplace manifest, so a skill
moved by a commit that left the manifest alone keeps no former row. Each file list of a commit is
read once and cached by repository and commit. GitHub lists at most 300 of a commit's files: a
commit that lists 300 has its moves read as listed, and a note under the manifest and a failure
say that a move past them may be missed.

### `attribute`

```
bun scripts/skill-cuts/cli.ts attribute --config <skill-cuts.json> --count <count dir> \
  --copies <copies out dir> --spans <spans.jsonl> --out <dir> [--changed <cut id>=<file>]... \
  [--models <models dir>]
```

The read count per skill copy (`attribute.ts`): it maps each read's copy path to a repository
copy by the prefix table, then by the repository-relative skill directory a checkout's path ends
with, then by the copies the session's Oh My Pi profile installs. Only the prefix table places reads under
the worker image's install location (`/opt/legion/`, copies.ts's `IMAGE_ROOT`). Matching rows
must agree on one copy. If no row matches or the rows disagree, the read stays unattributed
with any former-stage note for that path. Neither checkout nor context can substitute a copy.
For the profile step, `attribute` groups the prefix-table reads by store and profile.
It infers which units each group installed from those reads. It places a new read only if
that group supplies one copy of the name. It does not borrow copies from another profile
in the same store. If `roots.txt` does not identify the session's profile, this step cannot
place the read. `attribute` refuses a count output without the corresponding `roots.txt`.
The transcript-store coverage query supplies the Oh My Pi profiles for transcript hosts. A host
with no configured context uses only the two path rules; if neither decides, its read stays
unattributed because the host's context is not known.
The default profile of a `devbox` store also has each unit that the dev box's own installers
install into Oh My Pi, whether or not the session read from it. The dev box's repository is the
one repository of `manifests.tsv` that holds both an installer script (`installers/*.sh`) and
`.claude/settings.json`, and its units are the `installers.tsv` rows of that repository with host
`omp` and the action ships, stages, links or installs (the same rule as the dev-box profile's
in `build-cases`). So `/home/<user>/.agents/skills/gh/`, which cli/cli's `gh` and another
repository's `gh` both end, goes to cli/cli's, which the dev box installs with `gh skill install`.
A session of another profile has none of these units.
A read inside a project skills directory (`.omp/skills`, `.claude/skills`, `.agent/skills`,
`.agents/skills`, `.codex/skills`, `.opencode/skills`, `.github/skills`) of a checkout is a
project skill, whether the path is absolute or relative to the session's working directory. A
home directory's own skills directory (`/home/<user>/.claude/skills`, `~/.agents/skills`) is a
user's and is not a project's. The checkout rule places a project skill only at a copy in such a
directory too, and the profile never places it: a plugin that happens to hold a skill of the
same name is not that project's skill, and a copy at `skills/<name>` of another repository is
not either. A project skill that the copies output holds no copy of is unattributed, and its
outcome says it is a project skill. `build-cases` places each read by the same code, so a read of
a project skill installs no plugin in a case's profile.
A read placed by a Git rename's prefix row (a `former-stages.tsv` row with a `renamed_to`) counts
in the renamed copy's `sessions` and `former_name_sessions`. Outside the worker image, if the
prefix table does not decide a read that matches a former-name row, the context can choose the
renamed copy under that name if it installs the row's unit. A copies output with no `former-stages.tsv`
(made before `copies` wrote it) is refused (exit 1), naming the file, by `attribute` and by
`build-cases` alike: run `copies` again. Copies of several repositories at the
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
(`<out>/<cut id>/sessions/<skill>.jsonl`, file hashes only). With `--models`, it also counts each
copy's reading sessions by model, and writes each ranked skill's
`<out>/<cut id>/models/<skill>.json`: the counts by model of the sessions its sessions file holds
(`sessions`; `byModel`, each `{model, anthropic, served, sessions}`, most first; `noModel`, the
sessions whose file's head names no model; `notQueried`, those no models query held;
`notAnthropic`; `served`, those on a model the gateway serves; `withModel`), which `behaviour
--reading-models` reads. Without `--models` it writes no models file.
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

`attribute` keeps a file already owned by Postgres out of the transcript-store count. If more
than one transcript host imports another file, the first host label in byte order owns it. The
same shared-call-key rule applies across the remaining transcript files, including across hosts.
The Stores section gives first and last records and gap days per host and format. It reports
the number of sessions in the window, the number added after dedupe, and how many added sessions
read a skill or reference. Formats with no `read`, `Read` or `Skill` calls in the query results
give no reads. A copy's `transcript_store_sessions` is its number of added read sessions.
`partial_coverage_hosts` lists the contributing partial hosts when more than half of those
sessions come from host/format pairs with a gap or an incomplete start or end. A gap is a UTC
day with no active session between days that have sessions.

With Claude Code coverage, a Claude-only install has a read count like any other copy. Without
`transcriptStore`, the count reports that it did not query ClickHouse. Rankings include added
sessions; the ranked skills' sessions and corresponding model files for `build-cases` and
`behaviour` still use Postgres sessions, which the case builder can pull.

### `build-cases` and `build-tasks`

```
bun scripts/skill-cuts/cli.ts build-cases --config <skill-cuts.json> --key-file <file> --cut <id> \
  --skill <name> --old <skill dir at the cut's base> [--new <skill dir in the cut>] \
  --sessions <sessions.jsonl> --seed <n> --copies <copies out dir> --count <count dir> \
  --out <cases.jsonl> [--max <n>] [--budget-usd <n>] [--concurrency <n>] \
  [--stop-projected-usd <n>] [--stop-after-sessions <n>] [--stop-zero-case-usd <n>]
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

`build-cases` and `build-cases --reprofile` read the Oh My Pi profile from the transcript path.
`<home>/.omp/agent/sessions/` is the default profile. `<home>/.omp/profiles/<name>/agent/sessions/`
is a named profile. A subagent's transcript is below its root's directory, so it uses the same
profile.
`--reprofile` of a case whose store holds its session no longer takes the profile the case saved
(its `Oh My Pi profile` evidence item), and, for a case built before cases saved one, the profile
its store gave such a case then: `legion` in a Legion store, `default` in a `devbox` store.
A session's context follows its store, its profile and where its skills came from.
It is one of five kinds:

- A default-profile session in a `devbox` store ran on the dev box, and gets the dev-box installers and their pins at the session's start.
- A named-profile session in a `devbox` store with a `[Skill file: …]` header under the dev box's home ran in a pod that mounts that home.
  That pod's Oh My Pi loaded the dev box's skills, so the session gets the dev-box context too: the installers, the pins, the checkout rule and the pool rules.
- Any other named-profile session in a `devbox` store ran in a session pod, and gets what the pod's image links into its profile (below).
- A `legion`-profile session in a Legion store ran in a Legion pod, and gets its worker image's packages and its deployment's role setup.
- Any other named-profile session in a Legion store gets its session repository's project skills, not Legion role plugins.

Oh My Pi prints a `[Skill file: <path>]` header only for a skill it loaded.
So in every context each such header places its unit at the copy its path resolves to (the prefix table, else `attribute.ts`'s rules), as direct proof.
A unit the context installs already is not added again.
A whole read of a file by its path places a unit only in a dev-box context, in a Legion pod's `legion` profile, or for a session whose profile is not known, since in a pod its links decide which units it ships.

Set `devboxHome` in the private configuration to the dev box's home directory, an absolute path such as `/home/dev`.
A header is under that home by whole path segments: `/home/dev2/…` is not under `/home/dev`.
A read by path decides no context.
Without `devboxHome`, every named-profile session in a `devbox` store gets the session-pod context.
The evidence item `a pod that mounts the dev box's home, profile <name>` says how many headers are under that home and names the first with its message index.
A session with headers both under that home and elsewhere gets the dev-box context, and the units its other headers place, and the item `skill files from the dev box's home and the pod's own paths, profile <name>` names one header of each kind with its message index.

For a session-pod session, the builder reads the session-pod image pin at the
root's prompt build. It reads the Dockerfile of the pinned image's build, even if the pin
moved back to an older image.
It confirms the base that Dockerfile's final stage names from the images: the base's `rootfs.diff_ids` must be the first entries of the pinned image's, since a build can pass another base than the Dockerfile's `ARG` default, and diff ids, unlike the manifests' layer digests, do not change where the build compressed a base layer again.
A pinned image the runner cannot read, or one whose diff ids do not start with the base's, leaves the case out as not measurable, naming the pinned image's digest and why.
It uses the base worker's OCI revision or the commit recorded
with the pin. It reads the staged skills at that commit for packages the image links into
the profile. It leaves out a case if the evidence cannot identify the image or its commit.
The evidence states where the builder inferred what the profile loaded.

Set `sessionPod` in the private configuration to enable the image lookup. `repository` names
the deployment repository. `pinFile` and `pinKey` locate its image pin, and `dockerfile` names
the image recipe in that repository. `workerRepository` and `workerDockerfile` locate the base's
source. Use paths relative to their repositories.

Set `registries` in the private configuration to read images in a registry other than public GHCR.
Each entry is `{"host": "registry.example.test", "tokenCommand": ["<program>", "<argument>"]}`.
The runner runs `tokenCommand` once per process and sends its standard output, trimmed, as the Bearer token on that host's `/v2/` reads of an image's manifest and config blob.
A `tokenCommand` that runs past a minute is ended, and the read fails naming the host and the limit.
It keeps the token in memory alone, and never prints it or puts it in an error message.
A host that no entry names, other than GHCR, is not read.
Each request of an image's read (the token GHCR gives, a manifest, the config blob) is given up after 30 seconds, its body included; the token command's own limit is not inside it.
A request that timed out, met a connection failure, or was answered 429 or 5xx is sent again by the rule GitHub reads follow (after 5, 15, 45 and 135 seconds, each retry said on standard error and counted with the GitHub reads'), and after its last try the read fails naming the image and that try's failure.
A configured registry that answers 401 or 403 has refused its token: the read fails at once, naming the host, the image and the status.
A 404 is no such image, and on GHCR's anonymous read a 401 or 403 is an image that is not public: either gives no OCI config.
Set `deploymentImages` to name the images that the deployment repository builds on a worker image.
Each entry is `{"repository": "registry.example.test/acme/deploy/worker", "dockerfile": "images/worker.Dockerfile"}`: the image's reference without tag or digest, and the path of its Dockerfile in the deployment repository.
A Legion pod's image of such a repository takes the commit of the stage its final stage copies the worker's packages from, or, where it copies none of them, of the worker image it is built on (below).
An entry may list `readSteps`: each `{"sha256": "<hash>", "reason": "<what a person read in the step>"}`, a `RUN` step of the Dockerfile's final stage that a person read and found to write no copied package.
`sha256` is the SHA-256, as 64 lower-case hex digits, of the step's text in UTF-8 as `pod-image.ts`'s `dockerfileStages` gives it in `steps`, at the Dockerfile revision the image's label names: the instruction from its `RUN` on, its comment lines left out, a line continued with `\` joined to the next, a heredoc's lines read with it, each run of white space one space, none at its ends.
`reason` is not empty, and a hash is listed once in an entry.
For example, the final stage's lines `RUN tar -xzf sdk.tar.gz \` and `    -C /opt` are the step `RUN tar -xzf sdk.tar.gz -C /opt`, and `printf '%s' 'RUN tar -xzf sdk.tar.gz -C /opt' | sha256sum` prints its hash; its entry is `{"sha256": "<that hash>", "reason": "unpacks sdk.tar.gz into /opt/sdk, a directory of its own; it writes no path under /opt/legion"}`.
The evidence of a case such a step leaves out prints the step's hash, so a person who reads the step there can list it.

`build-tasks --profiles-out` and `build-tasks --reprofile` use the same rule. A task whose pod
image cannot be identified is not measurable. Case and task `context` labels keep their existing
forms. The evidence records the pod profile separately.

`build.ts`: `build-cases` pulls each candidate session through the config's `psqlCommand`, has
the `case` reader propose the step the skill governs, reading the session from the skill's read
on in windows of at most 40 messages, at most 6 windows a session, and writes the cases
`loadCases` reads, each with the evidence for its rebuilt context; `build-tasks` writes the need
and near-miss tasks of a load test from the first requests of real sessions. `--count` and
`--copies` are the count directory and the copies output that `attribute` read to make the
sessions file: `build-cases` places each session's read of the skill with `attribute.ts`'s own
rules (the prefix table, then a checkout, then the session's profile), and keeps a session only
when they place it at the cut repository's copy; a read they place at another copy, or at none, is
`not-cut-copy`, with the rule named. A session's reads of the skill are its `skill://` reads and its
reads of the skill's files by path, by the count's rule (a path or URL that, its read selector taken
off, ends `<skill>/SKILL.md` or holds `skills/<skill>/references/`). A case is built from a
successful `skill://` read, or from a successful whole read by path (no selector) that those rules
place at the cut repository's copy, by the path as read, as the count places it. That copy's
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
or in `--new`'s, is dropped as `path-form`, so no `behaviour` run fails on it. `--new` is optional:
a build before the cut has a text checks the old text alone, and its summary's `inputs` record
`new: null`; a build goes on from a summary only with the same choice (given where it was given,
absent where it was absent), and is refused otherwise, naming both. `behaviour screen` then drops a
case whose new text holds such a file before any model call, as `path-form: <arm>: <fault>`, and
runs the rest of the file. A session whose work at the skill's read is on the skill's own text
(writing, editing or rewriting it or a copy of it, reviewing, testing, grading, scoring or
comparing it) is no case of the skill's behaviour, since its next step works on the text and does
not follow it: the case reader answers `{"step": null, "material": {"why": …}}` for it
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

A pool that an extension gives through `resources_discover` (a pool of the dev box's
`skills-sources.json`, its unit named `pool:<root>`) is decided per session, whether the session's
read or the dev box's installers brought it in. Dev-box sessions that ran the same Oh My Pi build
list these pools in some sessions and not in others, and no file records why. The builder uses
the listing in the session's recorded prompt, or the measured subagent's listing if the session
records only its prompt's digest.

With a recorded listing, the profile installs a pool if the listing names a skill that the pool
ships at the session's start and no installed unit outside the pools ships. The evidence records
how many of the pool's skills the listing names and how many it does not (`named: …; unnamed: …`).
It also names any shared skills: those names do not tell the builder which unit supplied them.
The builder installs the whole pool and marks its listed names `fromRecorded`. The listing
measure counts the names that the profile lists but the recording does not as added names.

A `[Skill file: …]` read in the session's own messages also keeps the pool. If only the read
justifies keeping it, the evidence quotes `message N reads [Skill file: …]` as direct proof that
Oh My Pi loaded the pool. The measure does not mark its names `fromRecorded` in that case.

A name the listing gives as `<namespace>/<name>` is direct proof too, where the pool holds
`<name>`. Oh My Pi lists a copy under its namespace only when it admitted another copy of the name
first, and it admits a pool's copy before any copy from a unit outside the pools. So the builder
installs the pool when an installed unit outside the pools ships `<name>` under that namespace,
and, of the copies of `<name>` that installed units outside the pools ship, the first (the lowest
rank, then the earliest unit) is that unit's copy. The evidence names the listed name and the
unit. The measure does not mark the pool's names `fromRecorded`, since the listing names them bare
or not at all and the measure compares them.

If neither the listing nor a read supports installing the pool, the builder leaves it out. Its
`left out: the pool …` evidence names any shared skills and says why those names do not count.
It also names each `<namespace>/<name>` of the pool's names that the listing gives, with the
reason it proves nothing: no installed unit outside the pools ships the name under that
namespace, or a unit that Oh My Pi admits earlier ships another copy of it.

Without a recorded listing from the session or a subagent, the builder uses the Oh My Pi build
that the dev box pinned at the session's start. The optional `ompPin` configuration gives
that pin's location: `file`, a TOML file of the repository (such as `mise.toml`); `key`, the keys that
lead to the version in it (such as `["tools", "<tool>"]`); and `firstPoolVersion`, the first build
that loads such pools on a fresh start. The file is read at the repository's last commit at or
before the session's start. A pin before `firstPoolVersion` installs no pool, except one that a
`[Skill file: …]` read of the session places, since that read shows Oh My Pi loaded it; a pin at or
after it installs every pool. The evidence item `Oh My Pi pin of <repository>` names the pin, and
sets beside it the Oh My Pi version the session's file records (`runtime.version` of its first
`model_change` line; a fork's build gives only its release, such as `18.16.0`), as a cross-check
that decides nothing. Without `ompPin`, or where the file holds no version at those keys, such
pools are installed as the dev box's installers install them. `--reprofile` reads that version
from the session's head (`session-head.sql`'s `runtime`); a head saved before it was read gives
none.

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
dependency). For an image whose digest has no provenance there (GitHub answers 404, as for an
image that a deployment builds on the Legion image in another registry), the image itself decides:
the build reads its OCI config from the registry by digest (the manifest and the config blob of a
public GHCR image, with the anonymous pull token the registry gives, or of an image in a registry
the configuration's `registries` names, with its token; no layer is pulled). The
`org.opencontainers.image.revision` label decides when it is a commit of the repository. Else the
`org.opencontainers.image.version` label decides, as the release tag `v<version>` or
`<name>-v<version>` (the first words of the image's name, the longest first) that the repository
has, at the tag's last commit at or before the read. A label that names no commit or tag of the
repository decides nothing.
Failing that, an image of a repository that the configuration's `deploymentImages` names, which the deployment builds on a worker image, takes the commit of that worker image.
Its `org.opencontainers.image.source` label must name the deployment, and its `org.opencontainers.image.revision` label a commit the deployment holds at or before its read.
The Dockerfile the entry names, read at that commit, decides.
A `COPY` writes the copied files over whatever the final stage's base holds at those paths.
So where the final stage copies any `/opt/legion/<package>` the profile uses (a `COPY` or `ADD` line whose destination is the package, a path in it, or a directory above it), the stage it copies them from decides, and the base is not read.
The profile uses the package of a prefix row under `/opt/legion/` where at least one unit of the row's `via` is an Oh My Pi unit, one that an `installers.tsv` row of host `omp` installs.
A row that only Claude Code units place, such as one of a Claude Code plugin's package, installs nothing in the pod's Oh My Pi profile, so it adds no package.
Each such package must be copied in the final stage from that one stage alone: every `COPY --from=<stage> <src>... <dest>` line (`--chown` and other flags allowed) that writes into the package must name that stage.
An `ADD` that writes into the package adds it from the build context or a URL, not from the stage.
A package copied from elsewhere, added by `ADD`, or not copied, leaves the image's commit not known, and the evidence names the package and where the final stage copies it from.
No later step of the final stage may write into a copied package: a `COPY` or `ADD` into it is one of its copies above, and a `RUN` after the package's last `COPY` may not name a path that is the package, a path in it, or a directory above it other than `/`.
Paths are compared name by name, and a name with a glob, a brace or a `$` stands for any name, so `rm -rf /opt/legion/*` names `/opt/legion`, and `/opt/legion/pi-acme.bak` is no path in `/opt/legion/pi-acme`.
Such a step leaves the image's commit not known, and the evidence names the step (its first 120 characters), the package and the SHA-256 of the step's text.
A step whose hash the entry's `readSteps` lists (above) is one a person read, and does not leave it so; the evidence of the case that is kept names the step, its hash and the reason listed.
Any other such step still leaves the image's commit not known, and a step whose text changed since it was read has another hash, so it does again.
The stage must be `FROM` a digest-pinned image (a literal reference, or the default of the `ARG` it names), and its commit is that image's: the one its digest's build provenance in the repository names, else the one its labels name.
A build can pass the stage another image than its `ARG` default, so the image's own `org.opencontainers.image.version` label must name that commit: the COPY is confirmed by that label, not by layer content.
The label names the commit as `<version>`, whose release tag is at that commit: `<name>-v<version>` of the repository's own name (`legion-v20.25.1` for the label `20.25.1` of a repository named `legion`), or a tag the labels step above reads the label as.
It names it as `sha-<hex>` (7 to 40 hex digits) where the hex starts the commit.
A label that is absent, names no commit or names another commit leaves the image's commit not known, and the evidence names the label and the stage's commit; neither is preferred.
For example, a final stage `FROM ${TOOLCHAIN}` with `COPY --from=legion /opt/legion/pi-legion /opt/legion/pi-legion`, where `FROM ${LEGION_WORKER} AS legion` and `ARG LEGION_WORKER=ghcr.io/acme/worker@sha256:<digest>`, takes the commit of `ghcr.io/acme/worker` at that digest once the image's version label names it, and the toolchain image is not read.
Where the final stage copies none of those packages, by `COPY` or `ADD`, the base it is built on decides.
The Dockerfile must build its final stage on a digest-pinned base (a literal `FROM`, or the default of the `ARG` it names).
A Dockerfile's `ARG` default is no proof of the base, since a build can pass another, so the images confirm it: the base's `rootfs.diff_ids` must be the first entries of the image's.
Diff ids are the digests of the layers uncompressed, and they stay the same in an image built on the base; the manifests' layer digests are of the compressed layers, and differ where the build compressed a base layer again, so they are not compared.
The commit is then the base's: the one its digest's build provenance in the repository names, else the one its labels name.
Where a step fails, the image's commit is not known, and the evidence names the step and why.
For any other image, failing the labels, the comment lines right above its `image:` line decide,
by the first of their words, in their order, that names a commit:
an image digest whose provenance names one (the base the image was built on), a commit of the
repository (a word of 7 to 40 hex digits, letters and digits both), or a tag of the repository (a
word of the form `<name>-v<version>` or `v<version>`, such as `legion-v15.2.0`, at the tag's last
commit at or before the deployment's read). The order matters: a comment that names the image's
base commit first and an older release after it ("before it …") is read at the base commit. Where
none of these gives a commit (no `image:` line in the project file and no config file at that
commit, no entry or `image:` line for the project, an image no source dates, or a comment that
names none), the image's commit is not known. The repository's default branch is a guess, so no
profile is built on it: the case or task is not measurable, and is left out with that reason
(`build-cases` exits 1 naming the case; `build-tasks` lists the task as left out). A version file
inside an image layer is not read. The evidence item `the worker
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
source, and `none` leaves out every one, so no arm keeps the old text readable at another path; a
copy of another source that shares the name (listed as `<namespace>/<name>`) stays as the profile
has it in every arm but `none`. Oh My Pi 18.18.6 lists a copy under its bare name once it finds no
other copy of that name, so for each name that no copy left in `none` holds bare and some copy
holds under a namespace, `none` lists under the bare name the copy whose namespace's skills
directory Oh My Pi reads first (a capture gives Oh My Pi `skills/` first, then each namespace's
directory in the order of its first copy in the profile); every other copy keeps its namespace.
The arm records the name the profile lists each such copy under (`relisted`). A `skill://<name>`
read in `none` is then answered from that copy, and a subagent's recorded prompt in `none` sends
the `<name>` line under that copy's hint and leaves out the `<namespace>/<name>` line. A load
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
extension's `resources_discover`) and merges what it gets into the parent's snapshot, so a pool
that list names at the subagent's start and not at the root's build is a pool of the subagent too
(the unit's evidence says `pool at the subagent's start`); like every pool, its recorded listing
decides it (above). The root is the file the topmost `<…>Z_<id>` directory of the
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
prompt's `<skills>` lines (`session listing`), and its capture sends that prompt whole, with only
the tested skill's listing line changed per arm, so the model sees the recorded listing whatever
the rebuilt profile installs. `build.ts` writes the kind `session listing` exactly when the
case's or task's profile holds the recorded system prompt, which is exactly when `capture.ts`
sends that prompt, so the kind says which listing the capture shows. A top-level session records only its prompt's
digest, and the build makes no prompt, so `rebuiltDigest` is always null; it is measured against
the recorded listing of a subagent it started under the build its profile is read at (`subagent
listing`: the earliest one-level child of its directory whose `session_init` holds a listing and
whose header's time falls between that build's line and the next, or before the second line for
the first build; read with `subagents.sql`, one query a session as `build-cases` and `build-tasks
--profiles-out` and `build-tasks --reprofile` rebuild it, before its profile, and one a store for
`build-cases --reprofile`, since Oh My Pi gives a subagent its
parent's skills: task/structured-subagent.ts, lines 811-818 and 900; task/executor.ts, lines
662-666 and 5256), else it is `not measurable`, with why: a subagent spawned under another build
is never used. The measure says the store holds no such subagent only after `subagents.sql` was
asked, and names the window it asked for; a session the store was not asked about (it recorded
its own listing, no build of it is known, or the store no longer holds it) is `not measurable`
with that reason. A failed query fails that session's profile (`build-cases` drops it as
`profile-failed`; `build-tasks` gives the task no profile), and fails a `--reprofile` run, since
the listing decides the profile's pools. A task the reader picked from a session is asked about by the
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
profile installed a pool because the recorded listing names a skill of it that no installed unit
outside the pools ships (above) lists that pool's names as `fromRecorded`. The measure cannot see a
miss of those names because the same listing decided which pool to install. The summaries count
such measures
(`listing.fromRecorded`), and so do `behaviour report`'s verdict line and `load`'s summary and
listing line. The measure drops nothing here: `behaviour` and `load` apply the listing rule to
its `missing` names for every case and task, and to its `added` names only for a measure of kind
`subagent listing`, whose capture builds its listing from the rebuilt profile. The `added` names
of a `session listing` measure change only how a run's read of that skill is answered, which
`behaviour report` and `load` count (above, under each command).

`--reprofile` rebuilds each profile from the case and its session's head and path.
It pulls no transcript and calls no reader. Only `profile` and its evidence can change.
A case holds no working directory. The builder reads `cwd` from its session's head with
`session-head.sql` and the transcript path with `session-paths.sql`, one query of each per store.
It saves the head under `<out>.sessions/` but does not save the transcript path.
For a case whose session the store no longer holds, the evidence says so
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
reader call and recorded with what it spent, as `not-needed` (or, at the budget, as stopped by
it).

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
skill, the seed, and the SHA-256 of `--old`'s files, every file of `--new`, the sessions file, the
copies files read and the count's query outputs. Run again with the same `--out` and the same
inputs, `build-cases` keeps every session already decided (dropped, or a case the cases file
holds), with no pull and no reader call, and goes on in the same order from the first session not
decided, until `--max` cases in all; the budget counts the dollars already spent, and the
summary's spend adds the runs. A session `not-needed` or stopped by the budget is not decided, so
it is read again when it is needed. It refuses (exit 2, files left as they were) other inputs,
naming each difference; a cases file with no summary; and a summary that records no inputs, or no
`--new` (one an older `build-cases` wrote). A kill at any moment leaves both files readable: each
case is appended as one whole line, then the summary is written to a temporary name and renamed;
a case line the summary does not
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

## The request log

Each model request of `behaviour`, `read`, `control` and `load` adds one JSON line to the request
log (`<out>/requests.jsonl` for `behaviour` and `load`, `<out file>.requests.jsonl` otherwise):
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
  process's recorder (`processSpendRecorder()`): `read`, `control`, `behaviour` and `load`
  through `cli.ts`'s `openGateway`, `build-cases` and `build-tasks` through `build.ts`'s
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
call, a load test's task or an `--after-case` command that failed, or `behaviour` results that show
a drop no stage makes); 2 refused before any model call or store read (among the refusals, an
`--out` inside a git or jj checkout, from `behaviour`'s stage commands, `load` and `build-cases`),
or as a case's capture installs a copy Oh My Pi would name otherwise than its profile does, or
as a saved capture's installed copies, made again, are not the ones its record names;
3 stopped (the budget is spent, the peak memory passed 1 GiB, 1.5 GiB for `copies`, or the listing
rule dropped more than half of a load's tasks); 4 stopped by build-cases' stop rule.
